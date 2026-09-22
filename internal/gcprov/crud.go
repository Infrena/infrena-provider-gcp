package gcprov

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/infrena/infrena-provider-gcp/internal/catalog"
	"github.com/infrena/infrena/pkg/resource"
	"github.com/infrena/infrena/pkg/value"
)

// notFoundPatience bounds how long Read retries a 404 before believing a
// resource is genuinely gone. GCP is eventually consistent, so a resource
// created moments ago can still answer 404 -- reporting that as absence
// makes the next plan propose creating it a second time. ~4s, several
// backoff attempts' worth (see sleepBackoff), named so it is something a
// later task can tune with a number attached to a reason, not a bare
// literal buried in a retry loop.
const notFoundPatience = 4 * time.Second

// Create makes a resource and reports what now exists.
//
// THE RULE THAT SHAPES THIS WHOLE FUNCTION: once GCP has created something,
// no path below returns an error. The host DROPS the result of a failed
// create, so an error return after a successful POST leaves a real resource
// tracked nowhere, unfindable by any later plan or destroy. A failure after
// that point goes to stderr and the truthful state is returned instead; the
// next plan converges it.
func (p *Provider) Create(ctx context.Context, desired *resource.DesiredResource) (*resource.ResourceState, error) {
	ty, ok := p.catalog.Type(desired.Type)
	if !ok {
		return nil, fmt.Errorf("gcp: unknown type %q", desired.Type)
	}
	collTmpl := createTemplate(ty)
	reqURL, err := p.expandedURL(ty, collTmpl, desired.Attrs)
	if err != nil {
		return nil, err // nothing sent yet: an ordinary error is safe here
	}
	body := requestBody(ty, collTmpl, desired.Attrs)

	// The LAST point cancellation is allowed to stop us. After the POST there
	// is something real in the world and abandoning it orphans the resource.
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	resp, err := p.client.Do(ctx, http.MethodPost, reqURL, body)
	if err != nil {
		// The POST itself failed, so nothing was created. Safe to error.
		return nil, err
	}

	// From here on, errors are REPORTED, not returned.
	awaited, awaitErr := p.await(ctx, ty, resp)
	if awaitErr != nil {
		fmt.Fprintf(os.Stderr, "gcp: %s: create reported a failure, reading back what exists: %v\n",
			desired.Type, awaitErr)
	}
	st, readErr := p.readAfterCreate(ctx, ty, desired.Attrs, awaited)
	switch {
	case st != nil:
		return st, nil
	case awaitErr != nil:
		// Nothing exists AND the operation failed: the create genuinely did
		// not happen, so the error is the truth and returning it orphans
		// nothing.
		return nil, awaitErr
	case readErr == nil:
		// The await succeeded (awaitErr == nil, ruled out above) and the
		// readback simply found nothing (Read's own "believed absent" is
		// (nil, nil), never a non-nil readErr) -- the readback lost the race
		// with eventual consistency, not proof the create failed. Erroring
		// here would orphan a resource GCP already confirmed exists, so
		// trust the await instead and let the next refresh reconcile the
		// rest.
		fmt.Fprintf(os.Stderr, "gcp: %s: created, but not yet readable; reporting the awaited identity\n",
			desired.Type)
		return p.bestEffortState(ty, desired, awaited)
	default:
		return nil, fmt.Errorf("gcp: %s: created, but the resource cannot be read back: %w",
			desired.Type, readErr)
	}
}

// bestEffortState builds a *resource.ResourceState from what Create already
// knows -- desired's own attributes plus the awaited response body -- for
// the one case where a full readback isn't available even though GCP
// confirmed the create: the eventual-consistency window in which the
// readback GET still 404s. It exists so that case has state to return
// instead of an error (which would orphan a resource that exists): the
// provider id is the same one readAfterCreate already computed from awaited,
// recomputed here rather than threaded through because bestEffortState has
// no other way to know it went through that exact path. Attributes are best
// effort, not authoritative -- the next ordinary Read fills in whatever
// awaited did not carry.
func (p *Provider) bestEffortState(ty *catalog.Type, desired *resource.DesiredResource, awaited map[string]any) (*resource.ResourceState, error) {
	id, err := p.createdID(ty, awaited, desired.Attrs)
	if err != nil {
		return nil, fmt.Errorf("gcp: %s: created, but the resource cannot be read back: %w",
			desired.Type, err)
	}
	attrs := make(map[string]value.Value, len(desired.Attrs)+len(awaited))
	for k, v := range desired.Attrs {
		attrs[k] = v
	}
	// awaited is a GCP response body, so it is keyed by wire name while
	// desired.Attrs is keyed by schema name. Merging the two untranslated
	// would produce a state holding both spellings of the same attribute --
	// the same defect stateFrom exists to avoid, on the one path that
	// bypasses stateFrom entirely.
	//
	// Reconciled against what this create ASKED for, for the same reason
	// stateFrom reconciles against the state it was read at: an operation
	// body carries the same server-added nested keys, reordered sets and
	// whole-number floats a get does, and this state is what the next plan
	// diffs configuration against.
	for k, v := range ReconcileAttrs(ty.Attributes, desired.Attrs, schemaAttrs(ty.Attributes, awaited)) {
		attrs[k] = v
	}
	return &resource.ResourceState{
		Type:       ty.Name,
		ProviderID: id,
		Attributes: attrs,
	}, nil
}

// createdID is the provider id for something GCP has just made: the awaited
// body's own identity when it has one AND that identity names something this
// create could have made, and the caller's own attributes otherwise.
//
// Both halves of that matter. An id was previously accepted whenever
// ProviderID returned no error -- that is, whenever it PARSED -- so a
// targetLink naming a different resource entirely became the stored id
// silently. outsideCreatedCollection is the second question, and this is the
// only place either is asked: readAfterCreate and bestEffortState both come
// through here.
//
// THE FALLBACK EXISTS BECAUSE OF THE ORPHAN RULE. ProviderID reduces a
// selfLink by the type's own api prefix, and reduceSelfLink errors when the
// url does not carry it at a segment boundary -- a target answered under a
// different api version ("v1beta1/projects/..." against a type whose
// path_prefix is "v1/") is the realistic case, since a compute-style
// operation's targetLink is whatever url its API chose to publish, not one
// this provider built. Propagating that error makes Create return an error
// for a resource that already exists, which the host drops, leaving it
// tracked nowhere. 7 of the 65 compute-style types (sqladmin, container)
// reduce a targetLink whose version segment this provider cannot verify from
// their Discovery documents, so this is a live path, not a hypothetical one.
//
// The fallback is not a guess: ProviderID(ty, nil, attrs) expands the type's
// self_link against the attributes this very create was given, which is the
// same id Create would have used had the operation named no target at all.
// If THAT fails too there is genuinely no id to report, and the error is the
// truth.
//
// A body that was already nil got the fallback on the first call, so there
// is nothing to retry and the original error stands.
func (p *Provider) createdID(ty *catalog.Type, awaited map[string]any, desiredAttrs map[string]value.Value) (string, error) {
	id, err := ProviderID(ty, awaited, desiredAttrs)
	if awaited == nil {
		return id, err
	}
	reason := ""
	switch {
	case err != nil:
		reason = fmt.Sprintf("could not be reduced to a provider id (%v)", err)
	default:
		reason = outsideCreatedCollection(ty, id, desiredAttrs)
	}
	if reason == "" {
		return id, nil
	}
	// Loud, because a fallback id that is silently wrong is worse than the
	// error it replaced: this names the type and the reason, so a wrong
	// version segment in some API's targetLink is something a user can
	// report rather than something they discover from a later plan.
	fmt.Fprintf(os.Stderr, "gcp: %s: the operation's target %s; "+
		"using the attributes this create was given instead\n", ty.Name, reason)
	return ProviderID(ty, nil, desiredAttrs)
}

// outsideCreatedCollection says why id cannot name something this create
// made, or "" when it can. THE TEST IS NOT WHETHER THE ID PARSES. It is
// whether the id names a resource in the collection the POST actually went
// to -- the difference between an identity that is well-formed and one that
// is this resource's.
//
// Without it, a targetLink that reduces cleanly but names a DIFFERENT
// resource becomes the stored provider id, silently, and a later Delete
// destroys whatever that id named. For the 83 types whose self_link is a
// bare "{+name}" or "{{name}}" capture, ParseProviderID accepts literally any
// string, so nothing downstream would ever notice; for the ones with literal
// segments (gcp.sqladmin.databas, gcp.user, gcp.backuprun) it surfaces
// instead as "created, but the resource cannot be read back" -- an error
// after a successful POST, which is the orphan e3476ac exists to prevent.
// This check covers both, because it compares the id against the create's
// own url rather than against the id template's literal text.
//
// Two things stop it misfiring. It compares against the template the create
// POSTED to (create_url when the type has one, base_url otherwise -- the
// same choice Create itself makes, via createTemplate), with any query string
// dropped: 72 types carry their id in a query parameter
// ("...?attestorId={{name}}"), which names no path segment. And the tighter
// "exactly one segment deeper" rule is applied only where the type's own two
// templates agree that that is the shape (collectionMatchesSelfLink).
// Measured over the 233 shipped types: 142 are collection-plus-one, 1
// (gcp.bigquery.table) has a base_url that IS the item, 1
// (gcp.resourcerecordset) is two segments deeper, and 88 cannot be compared
// at the template level at all because one side is a whole-path capture --
// and every one of those 88 is still checked here, on the expanded values.
//
// A collection template that cannot be expanded is not a failure: Create
// expands the same template to build the POST url before anything is sent,
// so an unexpandable one never reaches this. Nothing to compare means no
// complaint.
func outsideCreatedCollection(ty *catalog.Type, id string, attrs map[string]value.Value) string {
	tmpl := pathPart(createTemplate(ty))
	coll, err := ExpandURL(tmpl, attrs)
	if err != nil {
		return ""
	}
	coll = strings.TrimSuffix(coll, "/")
	if id == coll {
		// gcp.bigquery.table's collection template is the item's own path.
		return ""
	}
	rest, under := strings.CutPrefix(id, coll+"/")
	if !under {
		return fmt.Sprintf("names %q, which is not in %q, the collection this create posted to", id, coll)
	}
	if strings.Contains(rest, "/") && collectionMatchesSelfLink(tmpl, ty.SelfLink) {
		return fmt.Sprintf("names %q, which is deeper than one resource inside %q, "+
			"the collection this create posted to", id, coll)
	}
	return ""
}

// createTemplate is the url template a create POSTs to: create_url when the
// type has one (72 of 233), base_url otherwise. One place, because Create
// builds its request url from it and outsideCreatedCollection has to ask
// about the same collection the request actually went to -- the same reason
// requestBody takes the template it was called with rather than re-deriving
// one.
func createTemplate(ty *catalog.Type) string {
	if ty.CreateURL != "" {
		return ty.CreateURL
	}
	return ty.BaseURL
}

// pathPart is tmpl without its query string. A create_url's query carries
// the new resource's id ("...?attestorId={{name}}") and GCP needs it, so
// Create sends the template whole; only the PATH names a collection, so only
// the path takes part in a comparison against a resource name.
func pathPart(tmpl string) string {
	if i := strings.IndexByte(tmpl, '?'); i >= 0 {
		return tmpl[:i]
	}
	return tmpl
}

// readAfterCreate resolves the just-created resource's identity -- preferring
// the awaited response's own name/selfLink, falling back to the attributes
// the caller supplied -- and reads it back through the ordinary Read path
// (which itself tolerates GCP's eventual consistency). Returning (nil, err)
// only when the resource genuinely is not there is what lets Create tell
// "created, then the operation failed" apart from "never created at all".
func (p *Provider) readAfterCreate(ctx context.Context, ty *catalog.Type, desiredAttrs map[string]value.Value, awaited map[string]any) (*resource.ResourceState, error) {
	id, err := p.createdID(ty, awaited, desiredAttrs)
	if err != nil {
		return nil, err
	}

	// The create already succeeded; the same rule that makes await
	// uncancelable (await.go) applies to reading it back. Abandoning here
	// leaves a resource that exists and is tracked nowhere.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx),
		time.Duration(ty.TimeoutSeconds)*time.Second)
	defer cancel()

	// The attributes this create asked for travel with the readback, as the
	// reference reconciliation expresses GCP's answer in. Without them a
	// created resource's first state records whatever order GCP returned a
	// set in, the plan right after the apply proposes reordering it, and the
	// patch that follows changes nothing -- a plan that never converges, from
	// the one read that has no previous state to reconcile against.
	return p.Read(ctx, &resource.ResourceState{Type: ty.Name, ProviderID: id, Attributes: desiredAttrs})
}

// Read returns the current state, or (nil, nil) when the resource is gone.
//
// A NotFound is NOT proof of absence for a resource created seconds ago: GCP
// is eventually consistent, and reporting "gone" makes the next plan create
// it a second time. So a 404 is retried for notFoundPatience before it is
// believed.
func (p *Provider) Read(ctx context.Context, current *resource.ResourceState) (*resource.ResourceState, error) {
	ty, ok := p.catalog.Type(current.Type)
	if !ok {
		return nil, fmt.Errorf("gcp: unknown type %q", current.Type)
	}
	attrs, err := ParseProviderID(ty, current.ProviderID)
	if err != nil {
		return nil, err
	}
	reqURL, err := p.itemURL(ty, "", current.ProviderID, attrs)
	if err != nil {
		return nil, err
	}

	deadline := time.Now().Add(notFoundPatience)
	for attempt := 0; ; attempt++ {
		body, err := p.client.Do(ctx, http.MethodGet, reqURL, nil)
		switch {
		case err == nil:
			return p.stateFrom(ty, current, attrs, body)
		case !isNotFound(err):
			return nil, err
		case time.Now().After(deadline):
			return nil, nil // believed absent
		}
		if err := p.sleepBackoff(ctx, attempt); err != nil {
			return nil, err
		}
	}
}

// stateFrom builds a ResourceState from a successful get's response body.
//
// EVERY KEY IT STORES IS A SCHEMA NAME. The body arrives keyed the way GCP
// writes it -- wire names -- and state is what the host diffs configuration
// against, so storing a response key verbatim puts "type" in state while
// configuration holds "type_value" and every plan thereafter reports a change
// to a field nobody touched. That is the invariant BuildMask then relies on
// to look current up by the same schema name it reads desire by. See
// names.go.
//
// The provider id is recomputed (body's own name/selfLink wins, same as
// readAfterCreate) rather than copied from current, so a rename GCP made
// underneath this resource is picked up rather than silently ignored. The
// scoping attributes recovered from the id (project, region/zone/location)
// are merged in underneath the response body's own fields, because most GCP
// APIs never echo them back in the body at all -- without this an imported
// or refreshed resource would show those attributes as unset and a plan
// would propose "setting" them forever.
//
// THE ORPHAN RULE REACHES HERE TOO, even on a plain Read. We asked GCP for
// this exact resource BY ID and it answered 200, so the resource exists at
// current's id whether or not the body's own selfLink happens to reduce
// against this type's api prefix -- reducing it exists only to notice a
// rename GCP made underneath us, and failing to reduce it means "I could not
// tell whether it was renamed", not "the read failed". Reached from Create
// by way of readAfterCreate, returning an error here for that case would
// land in Create's default branch and fail a create for a resource GCP
// already made -- the host drops that result, so the resource would exist
// and be tracked nowhere. So on a reduction failure this keeps current's own
// id rather than erroring; the body's other fields are still reported, and
// the ordinary "id didn't reduce" case (the id genuinely doesn't belong to
// this type) still surfaces from Import and Delete, which parse it directly
// instead of going through here. A silently wrong id would be worse than the
// error it replaces, so the failure still goes to stderr, naming the type
// and the reason.
func (p *Provider) stateFrom(ty *catalog.Type, current *resource.ResourceState, idAttrs map[string]value.Value, body map[string]any) (*resource.ResourceState, error) {
	id, err := ProviderID(ty, body, idAttrs)
	if err != nil {
		fmt.Fprintf(os.Stderr, "gcp: %s: the response's identity could not be reduced to a provider id (%v); "+
			"keeping the id this resource was read at\n", ty.Name, err)
		id = current.ProviderID
	}
	attrs := make(map[string]value.Value, len(idAttrs)+len(body))
	// idAttrs is keyed by self_link's own PLACEHOLDER names, which for
	// gcp.resourcerecordset ("...rrsets/{name}/{type}") is the wire spelling.
	// Left untranslated it would put "type" into the same state map the body
	// puts "type_value" into, and the host would diff configuration against
	// both.
	for k, v := range idAttrs {
		attrs[toSchema(ty.Attributes, k)] = v
	}
	// RECONCILED AGAINST WHAT THIS RESOURCE IS GOING TO BE COMPARED WITH.
	// GCP answers with more than it was sent -- server-set keys inside nested
	// objects, a set in a different order, a whole number where a float was
	// declared -- and every one of those reads as drift against configuration
	// that has not changed. current.Attributes is what the host holds for
	// this resource (the desired attributes, on the readback after a create
	// or a patch; the last observation, on a refresh), so it is the order and
	// the shape the answer has to be expressed in for value.Equal to mean "no
	// drift". See reconcile.go.
	for k, v := range ReconcileAttrs(ty.Attributes, current.Attributes, schemaAttrs(ty.Attributes, body)) {
		attrs[k] = v
	}
	return &resource.ResourceState{
		Type:       current.Type,
		ProviderID: id,
		Attributes: attrs,
	}, nil
}

// Delete removes the resource. A 404 -- before or after the delete call --
// means the goal is already met: destroy must converge.
func (p *Provider) Delete(ctx context.Context, current *resource.ResourceState) error {
	ty, ok := p.catalog.Type(current.Type)
	if !ok {
		return fmt.Errorf("gcp: unknown type %q", current.Type)
	}
	attrs, err := ParseProviderID(ty, current.ProviderID)
	if err != nil {
		return err
	}
	reqURL, err := p.itemURL(ty, ty.DeleteURL, current.ProviderID, attrs)
	if err != nil {
		return err
	}

	if err := ctx.Err(); err != nil {
		return err // last chance to stop before anything is destroyed
	}
	resp, err := p.client.Do(ctx, http.MethodDelete, reqURL, nil)
	if isNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	_, err = p.await(ctx, ty, resp)
	if isNotFound(err) {
		return nil
	}
	return err
}

// Import adopts an existing resource by its provider id.
func (p *Provider) Import(ctx context.Context, resourceType, id string) (*resource.ResourceState, error) {
	ty, ok := p.catalog.Type(resourceType)
	if !ok {
		return nil, fmt.Errorf("gcp: unknown type %q", resourceType)
	}
	// BEFORE any API call. A malformed id, or one for the wrong type, costs a
	// message naming the problem rather than a confusing 404 from a url built
	// out of nonsense.
	attrs, err := ParseProviderID(ty, id)
	if err != nil {
		return nil, err
	}
	return p.Read(ctx, &resource.ResourceState{Type: resourceType, ProviderID: id, Attributes: attrs})
}

// isNotFound unwraps err (which may be wrapped, e.g. by readAfterCreate's
// caller or a %w chain) to an *APIError and reports whether GCP answered
// "not found". Both Status 404 and Code "NOT_FOUND" occur in practice --
// matching only one of them misses the other half of the real cases.
func isNotFound(err error) bool {
	var ae *APIError
	if !errors.As(err, &ae) {
		return false
	}
	return ae.Status == http.StatusNotFound || ae.Code == "NOT_FOUND"
}

// expandedURL is createURL's helper: the collection's own address (POST
// target for create), tmpl expanded against attrs and prefixed with the
// type's api base.
func (p *Provider) expandedURL(ty *catalog.Type, tmpl string, attrs map[string]value.Value) (string, error) {
	rel, err := ExpandURL(tmpl, attrs)
	if err != nil {
		return "", err
	}
	return absURL(ty, rel), nil
}

// absURL joins a relative, already-expanded resource path onto the API base.
// The version lives in PathPrefix, never in the template (see
// catalog.Type.PathPrefix).
func absURL(ty *catalog.Type, rel string) string {
	return ty.APIBaseURL + ty.PathPrefix + rel
}

// itemURL returns the request url addressing ONE existing item: Read's GET
// target, and Delete's (and, from Task 14, Update's) when the type names no
// delete_url/update_url of its own (228 and 224 of the 233 real types,
// respectively -- see the task brief's measured table).
//
// tmpl, when non-empty, is ty.DeleteURL or ty.UpdateURL: expanded directly,
// with the item's own last path segment additionally offered under "name"
// (see withNameFallback) -- some of those templates rename the id
// placeholder self_link itself uses (e.g. self_link's own "{rolloutPlan}"
// vs. delete_url's "{{name}}" for gcp.rolloutplan).
//
// Otherwise the item's address is reconstructed from base_url (the
// collection: create/list's own target) plus the id's own last path
// segment -- exactly how gcpfake's own create stores a resource (collection
// + "/" + id, server.go's handleCreate) -- whenever that reconstruction is
// structurally safe (collectionMatchesSelfLink). self_link is derived, for
// 143 of 233 real types, from the collection's own Discovery `get` path
// (internal/gen/build.go), so reconstructing from base_url keeps read and
// delete pointed at wherever create actually put the resource.
// Structurally unsafe cases -- self_link ending in a reserved "{+name}"
// capture (which does not decompose into base_url's own placeholders, e.g.
// a "{+parent}"), or one of the small number of real types whose self_link
// is not literally base_url plus one more segment -- fall back to expanding
// self_link directly, which is always independently correct: it is the
// API's own published get path, verbatim.
func (p *Provider) itemURL(ty *catalog.Type, tmpl, id string, attrs map[string]value.Value) (string, error) {
	if tmpl != "" {
		rel, err := ExpandURL(tmpl, withNameFallback(attrs, id))
		if err != nil {
			return "", err
		}
		return absURL(ty, rel), nil
	}

	if collectionMatchesSelfLink(ty.BaseURL, ty.SelfLink) {
		if rel, err := ExpandURL(ty.BaseURL, attrs); err == nil {
			return absURL(ty, strings.TrimSuffix(rel, "/")+"/"+url.PathEscape(lastPathSegment(id))), nil
		}
	}

	rel, err := ExpandURL(ty.SelfLink, attrs)
	if err != nil {
		return "", err
	}
	return absURL(ty, rel), nil
}

// withNameFallback returns attrs with an additional "name" entry -- id's own
// last path segment -- UNLESS attrs already has one. A delete_url or
// update_url the catalog names explicitly sometimes calls the id placeholder
// "name" regardless of what self_link itself calls it (magic-modules'
// convention, not this generator's choice -- e.g. gcp.rolloutplan's
// self_link captures "{rolloutPlan}" but its delete_url asks for
// "{{name}}"), so attrs -- which ParseProviderID keyed by self_link's own
// placeholder names -- would otherwise be missing it.
func withNameFallback(attrs map[string]value.Value, id string) map[string]value.Value {
	if _, ok := attrs["name"]; ok {
		return attrs
	}
	out := make(map[string]value.Value, len(attrs)+1)
	for k, v := range attrs {
		out[k] = v
	}
	out["name"] = value.String(lastPathSegment(id), value.SourceProvider)
	return out
}

// placeholderTemplateRE matches one whole-segment placeholder in EITHER
// spelling url templates use in this catalog -- "{{x}}" or "{x}" -- but not
// the reserved "{+x}" form, which a caller must recognise on its own (see
// collectionMatchesSelfLink and ExpandURL).
var placeholderTemplateRE = regexp.MustCompile(`\{\{[^{}]+\}\}|\{[^{}+][^{}]*\}`)

// normalizeTemplateShape collapses every placeholder in tmpl to one token,
// so two templates that name the same shape but spell a placeholder
// differently -- magic-modules' "{{project}}" vs. a Discovery path's
// "{project}" -- compare equal on shape alone.
func normalizeTemplateShape(tmpl string) string {
	return placeholderTemplateRE.ReplaceAllString(tmpl, "\x00")
}

// collectionMatchesSelfLink reports whether self_link is, structurally,
// exactly base's own collection path plus one more (non-reserved) segment.
// When it is not -- a reserved "{+name}" tail, or a real structural
// difference -- reconstructing an item's url from base plus its own id would
// not reliably reach the same resource self_link addresses, so itemURL falls
// back to expanding self_link directly instead.
//
// Neither side can carry an api-version segment any more: the generator
// moves it to PathPrefix, and TestNoStoredTemplateCarriesAnAPIVersion
// (internal/catalog) is what keeps that true, so there is nothing to strip
// before comparing.
func collectionMatchesSelfLink(base, selfLink string) bool {
	i := strings.LastIndex(selfLink, "/")
	if i < 0 {
		return false
	}
	parent, last := selfLink[:i], selfLink[i+1:]
	if strings.HasPrefix(last, "{+") {
		return false
	}
	return normalizeTemplateShape(base) == normalizeTemplateShape(parent)
}

// requestBody builds a mutation's JSON body from attrs: every configured
// attribute except the ones tmpl's own placeholders already consume (GCP
// rejects a body that repeats what the url already said) and every Output
// attribute (server-computed; GCP rejects a body that sets one).
//
// The keys it emits are WIRE names, at every depth (see names.go). attrs is
// keyed the way configuration is, by schema name, and for the 306 attributes
// the generator renamed those are not the same string: before this
// translation existed a create sent {"type_value": "IPV4"} where GCP asked
// for {"type": "IPV4"}, which for the 7 Required ones means asking an API to
// create a resource without a field it demands.
//
// A placeholder is matched in EITHER spelling. Only gcp.resourcerecordset's
// self_link names a renamed attribute today and it has no create template of
// its own, so this is latent rather than live -- but a body that repeats
// what the url already said is rejected by the whole API, not by that field,
// and the cost of checking both is one map lookup.
func requestBody(ty *catalog.Type, tmpl string, attrs map[string]value.Value) map[string]any {
	inURL := placeholderNames(tmpl)
	body := make(map[string]any, len(attrs))
	for name, v := range attrs {
		a := ty.Attributes[name]
		if inURL[name] || (a != nil && inURL[a.Canonical]) {
			continue
		}
		if a != nil && a.Output {
			continue
		}
		body[toWire(ty.Attributes, name)] = wireValue(a, v)
	}
	return body
}

// placeholderNamesRE matches one placeholder in any of the three forms a url
// template uses -- "{{x}}", "{x}" or reserved "{+x}" -- capturing just the
// name.
var placeholderNamesRE = regexp.MustCompile(`\{\{([^{}]+)\}\}|\{\+?([^{}]+)\}`)

// placeholderNames returns the set of attribute names tmpl's own
// placeholders reference.
func placeholderNames(tmpl string) map[string]bool {
	out := map[string]bool{}
	for _, m := range placeholderNamesRE.FindAllStringSubmatch(tmpl, -1) {
		name := m[1]
		if name == "" {
			name = m[2]
		}
		out[strings.TrimSpace(name)] = true
	}
	return out
}
