package gcprov

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/infrena/infrena-provider-gcp/internal/catalog"
	"github.com/infrena/infrena/pkg/resource"
	"github.com/infrena/infrena/pkg/value"
)

// BuildMask computes the patch body and the updateMask from a diff.
//
// The mask is what gives this provider AWS's "only adds or replaces, never
// removes" rule for free: an attribute dropped from configuration
// contributes no mask entry, so GCP keeps whatever it currently holds. That
// is enforced by the API rather than by anything we write, which is a better
// place for it.
//
// BOTH MAPS ARE KEYED BY SCHEMA NAME, and the mask and body it emits are
// keyed by wire name. That asymmetry is the whole of Task 14a: desired comes
// from configuration and current comes from stateFrom, which (since Task
// 14a) translates a response body back into schema names, so looking current
// up by the same key desired uses is correct. It was not before: state held
// "type" while desire held "type_value", the lookup returned a zero
// value.Value whose Known is false, Value.Equal compares Kind before Raw so
// it equalled nothing, and the field was masked on every single update --
// including on the ForceNew ones, where an updateMask naming an immutable
// field is likely rejected outright.
//
// current is the REFRESHED observation the host handed Update, not the last
// state persisted to disk. There is deliberately no extra read here --
// infrena >= 0.7.1 passes the observation from immediately before planning,
// and the AWS provider carried a re-read workaround until that was fixed. Do
// not add one.
func BuildMask(ty *catalog.Type, current, desired map[string]value.Value) (map[string]any, []string) {
	body := map[string]any{}
	var mask []string
	inURL := urlIdentifying(ty)

	// Sorted, because the mask is a query parameter a human reads in a plan
	// and a test asserts on. Map iteration order would make both unstable.
	for _, name := range sortedAttrNames(ty.Attributes) {
		a := ty.Attributes[name]
		switch {
		case a.Output:
			// GCP owns it. A mask naming a read-only field fails the whole
			// request, not just that field.
			continue
		case ty.SetterFor(a.Canonical) != nil:
			// Changed through its own method (setLabels, setUrlMap), never
			// through the resource's update. See Update.
			continue
		case inURL[name] || inURL[a.Canonical]:
			// The url already says it: project, region/zone/location, name,
			// and every type-specific id segment. See urlIdentifying. Either
			// spelling counts, because a url template names a placeholder in
			// whichever namespace its own source wrote it in.
			continue
		}
		want, inDesired := desired[name]
		if !inDesired {
			// Absent from configuration is NOT "set it to empty". infrena
			// cannot tell "never set" from "no longer configured", so both
			// keep GCP's value. This single branch is the whole
			// never-removes guarantee.
			continue
		}
		if !want.Known {
			// The host refuses an unknown before it ever builds a
			// DesiredResource (resource.ResolvedResource.Desired), so this
			// is unreachable through the plugin. Skipping rather than
			// sending is still the right answer if it ever is reached: a
			// value nobody has resolved would go out as JSON null and
			// overwrite a real one.
			continue
		}
		have := current[name]
		if have.Equal(want) {
			continue
		}
		if len(a.Fields) > 0 && want.Kind == value.KindMap && have.Kind == value.KindMap && have.Known {
			// Mask the CHANGED LEAVES, not the parent. Naming the parent
			// replaces the whole object and drops anything GCP put inside it
			// that we never modelled.
			sub, subMask := buildNested(a, a.Canonical, have, want)
			if len(subMask) > 0 {
				body[a.Canonical] = sub
				mask = append(mask, subMask...)
			}
			continue
		}
		// wireValue, not toRaw. A value masked WHOLE -- a list, or a map the
		// catalog declares no Fields for -- still has names inside it, and
		// toRaw copies the schema spelling straight onto the wire. That is
		// how gcp.router's "nats" ends up carrying {"type_value": ...} in a
		// patch body: the mask entry is right and the object under it is not.
		body[a.Canonical] = wireValue(a, want)
		mask = append(mask, a.Canonical)
	}
	if len(mask) > 0 {
		addRequestOptions(ty.Attributes, desired, body)
	}
	return body, mask
}

// addRequestOptions puts every configured SendWithUpdate option into the
// body, at its path, without masking it: Google reads a request option from
// the body and would refuse a mask naming a field it never returns. Only
// Fields are walked. A ruling cannot name a path through a list (the
// generator refuses one), and a list is sent whole anyway.
func addRequestOptions(attrs map[string]*catalog.Attr, desired map[string]value.Value, body map[string]any) {
	for _, name := range sortedAttrNames(attrs) {
		a := attrs[name]
		want, ok := desired[name]
		if !ok || !want.Known {
			continue
		}
		if a.SendWithUpdate {
			body[a.Canonical] = wireValue(a, want)
			continue
		}
		fields, isMap := want.Raw.(map[string]value.Value)
		if len(a.Fields) == 0 || !isMap {
			continue
		}
		sub, _ := body[a.Canonical].(map[string]any)
		if sub == nil {
			sub = map[string]any{}
		}
		addRequestOptions(a.Fields, fields, sub)
		if len(sub) > 0 {
			body[a.Canonical] = sub
		}
	}
}

// buildNested recurses one level of Fields at a time, producing dotted mask
// paths ("config.mode") and the matching sub-body. A nested attribute absent
// from want is skipped by the same never-removes rule as a top-level one,
// and a nested Output field by the same rule as a top-level one.
//
// Only DECLARED fields are walked. An attribute with no Fields at all -- a
// free-form map the generator marked Opaque, or a list -- never reaches here
// (BuildMask's own guard), and is masked whole, which is what those shapes
// mean on the wire: GCP replaces a labels map or a list outright.
//
// path is the parent's own dotted path so far, so a field two levels down
// comes out as "a.b.c" rather than losing its ancestry.
func buildNested(a *catalog.Attr, path string, have, want value.Value) (map[string]any, []string) {
	haveFields, _ := have.Raw.(map[string]value.Value)
	wantFields, _ := want.Raw.(map[string]value.Value)

	sub := map[string]any{}
	var mask []string
	for _, name := range sortedAttrNames(a.Fields) {
		f := a.Fields[name]
		if f.Output {
			continue
		}
		w, ok := wantFields[name]
		if !ok || !w.Known {
			continue
		}
		h := haveFields[name]
		if h.Equal(w) {
			continue
		}
		child := path + "." + f.Canonical
		if len(f.Fields) > 0 && w.Kind == value.KindMap && h.Kind == value.KindMap && h.Known {
			s, m := buildNested(f, child, h, w)
			if len(m) > 0 {
				sub[f.Canonical] = s
				mask = append(mask, m...)
			}
			continue
		}
		// wireValue for the same reason BuildMask uses it: a nested list or
		// an undeclared-shape map carries its own names, and this is the
		// only place they would otherwise be written in the schema spelling.
		sub[f.Canonical] = wireValue(f, w)
		mask = append(mask, child)
	}
	return sub, mask
}

// urlIdentifying is the set of attribute names this type's own item url
// consumes -- self_link's placeholders, plus update_url's and base_url's.
// They are excluded from the patch entirely, for the same reason requestBody
// excludes them from a create's body: the url already said it.
//
// base_url is in that union because the url Update actually builds is
// itemURL(ty, ty.UpdateURL, ...), and itemURL may expand update_url, or
// self_link, or BASE_URL plus the id's own last segment, depending on
// collectionMatchesSelfLink -- which compares normalised SHAPE, so two
// templates may legally spell the same segment with different placeholder
// names and the union of two of the three would miss one. Measured over all
// 86 updatable types on 2026-09-22, base_url names ZERO declared attributes
// that self_link and update_url do not already name, so this changes nothing
// today; it is here so that a regenerated catalog in which one diverges
// cannot silently start asking GCP to patch an id segment.
//
// It is derived from the type's own templates rather than from a fixed
// project/region/zone/location list, because the id segments are not
// interchangeable across APIs and most of them are not scope words at all.
// Measured over the regenerated catalog's 86 updatable types, self_link and
// update_url between them name project (65), name (61), location (52),
// parent (16), region (11) and 29 further type-specific segments
// ("capability_config_id", "cross_site_network", "sslPolicy", ...).
//
// This matters for more than tidiness. 10 of those 86 types declare `name`
// as a required, NOT-ForceNew attribute while also naming it in their url
// (gcp.mesh, gcp.authzpolicy, gcp.networksecurity.addressgroup,
// gcp.packetmirroring, gcp.resourcepolicy, gcp.tlsinspectionpolicy,
// gcp.agentgateway, gcp.authzextension, gcp.wiregroup and
// gcp.networksecurity.organization.addressgroup). Those APIs echo `name`
// back as the FULL resource name while configuration holds the short one, so
// state and desire disagree on every single update -- without this exclusion
// all 10 would send "updateMask=name" with the short name in the body on
// every patch, asking GCP to rename the resource the url just addressed. The
// inequality there is a representation difference, not a change.
func urlIdentifying(ty *catalog.Type) map[string]bool {
	names := placeholderNames(ty.SelfLink)
	for _, tmpl := range []string{ty.UpdateURL, ty.BaseURL} {
		for n := range placeholderNames(tmpl) {
			names[n] = true
		}
	}
	return names
}

// sortedAttrNames returns m's keys in sorted order, so a walk over an
// attribute map reaches the same attribute in the same order every run.
func sortedAttrNames(m map[string]*catalog.Attr) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Update patches a resource and returns its new state.
//
// THE ORPHAN RULE BINDS THIS AS WELL AS CREATE. Once the PATCH is sent
// something in the world has changed, so no path below abandons it: the
// readback is uncancellable (same reasoning as await.go and
// readAfterCreate), and an operation that reports failure goes to stderr and
// is followed by a truthful read rather than returning an error that tells
// the host nothing happened.
//
// It also must never return (nil, nil): the host treats a nil state with a
// nil error as a contract violation and stops the whole run, precisely
// because it cannot tell that apart from "the call took effect and nothing
// recorded it".
func (p *Provider) Update(ctx context.Context, current *resource.ResourceState, desired *resource.DesiredResource) (*resource.ResourceState, error) {
	ty, ok := p.catalog.Type(desired.Type)
	if !ok {
		return nil, fmt.Errorf("gcp: unknown type %q", desired.Type)
	}
	// 64 of the 236 types publish no update method this provider can use,
	// and Capabilities.Update is derived from exactly this field, so the host
	// plans a replacement for them and never calls Update. Refusing here
	// rather than defaulting to PATCH keeps a catalog or host regression
	// from quietly patching a type whose API has no update method: nothing
	// has been sent yet, so an error costs nothing.
	//
	// It used to be 154 of 235, because the verb came only from
	// magic-modules and magic-modules leaves it off most resources. The
	// generator now asks Discovery too (gen.discoveredUpdate), which is why
	// buckets, networks and subnetworks are patched rather than replaced.
	if ty.UpdateVerb == "" && len(ty.Setters) == 0 {
		return nil, fmt.Errorf("gcp: %s: publishes no update method; every change to it replaces the resource", ty.Name)
	}

	body, mask := BuildMask(ty, current.Attributes, desired.Attrs)
	setters := changedSetters(ty, current.Attributes, desired.Attrs)
	if len(mask) == 0 && len(setters) == 0 {
		// Nothing changed. Sending an empty patch would be a request against
		// a project-wide quota for no effect, and some APIs reject it
		// outright.
		return current, nil
	}
	if len(mask) > 0 && ty.UpdateVerb == "" {
		// The generator makes every field no setter carries ForceNew on a
		// type with no update verb, so the host plans a replacement for
		// them and this is unreachable. Refused before anything is sent if
		// it ever is reached.
		return nil, fmt.Errorf("gcp: %s: %s can only change by replacing the resource", ty.Name, strings.Join(mask, ", "))
	}
	if len(mask) == 0 {
		return p.updateThroughSetters(ctx, ty, current, desired, setters, false, nil)
	}

	attrs, err := ParseProviderID(ty, current.ProviderID)
	if err != nil {
		return nil, err
	}
	// itemURL, because Update addresses the same one resource Read and
	// Delete do -- including the absURL join that puts ty.PathPrefix's api
	// version back in front, and the "name" fallback for an update_url that
	// spells the id placeholder differently from self_link. Only 9 of the
	// 233 types name an update_url at all (7 of them updatable), so the
	// other 79 fall through to self_link, which is what "" asks for.
	reqURL, err := p.itemURL(ty, ty.UpdateURL, current.ProviderID, attrs)
	if err != nil {
		return nil, err
	}
	// NOT every updatable type takes an updateMask. Measured across the 236
	// generated types on 2026-09-23: 172 are updatable at all (they have an
	// UpdateVerb, and it is PATCH for every one of them -- a PUT would
	// replace the resource with BuildMask's partial body, so the generator
	// refuses to derive one), and 135 of those use a mask. The other 37 --
	// gcp.firewall, gcp.storage.bucket, gcp.network and gcp.router among
	// them -- PATCH without one. Appending the parameter unconditionally
	// sends a query argument those APIs never asked for.
	// Compute's optimistic lock, AFTER the empty-mask return above: a
	// fingerprint alone is never a reason to patch. Copied from the
	// observation Update was handed, which the host takes immediately before
	// planning, so it is as current as a value can be without the extra read
	// this function deliberately does not make. Without it every patch to the
	// 19 locked compute types is a 412.
	if ty.PatchOneField {
		if fields := topLevelFields(mask); len(fields) > 1 {
			return p.patchOneFieldAtATime(ctx, ty, current, desired, attrs, reqURL, body, mask, fields, setters)
		}
	}
	if ty.LockField != "" {
		if a, v := lockValue(ty, current.Attributes); v.Known {
			body[ty.LockField] = wireValue(a, v)
		}
	}

	if err := ctx.Err(); err != nil {
		return nil, err // last point before the resource is modified
	}
	resp, err := p.sendPatch(ctx, ty, reqURL, body, mask)
	if err != nil {
		// The PATCH itself failed, so nothing was changed, and state still
		// records the resource at the id it already had. Safe to error.
		return nil, err
	}

	// From here on the patch is real, and errors are REPORTED, not returned,
	// for as long as there is a truthful state to return instead.
	_, awaitErr := p.awaitAs(ctx, ty, ty.UpdateAwaitKind(), resp)
	if awaitErr != nil {
		fmt.Fprintf(os.Stderr, "gcp: %s: update reported a failure, reading back what exists: %v\n",
			ty.Name, awaitErr)
	}
	return p.updateThroughSetters(ctx, ty, current, desired, setters, true, awaitErr)
}

// sendPatch sends one patch of body and mask in the shape the type's API
// takes it.
func (p *Provider) sendPatch(ctx context.Context, ty *catalog.Type, reqURL string, body map[string]any, mask []string) (map[string]any, error) {
	switch {
	case ty.UpdateWrapper != "":
		// AIP-134's UpdateXRequest shape: the resource is wrapped and the field
		// mask travels IN THE BODY, not on the query string. pubsub's
		// topics.patch takes UpdateTopicRequest{topic, updateMask} and rejects a
		// bare Topic outright, so this is not a nicety.
		//
		// The mask is required and non-empty for every API that uses this shape,
		// which costs nothing to honour: Update has already returned early above
		// when nothing changed, so mask is never empty by the time it gets here.
		body = map[string]any{
			ty.UpdateWrapper:   body,
			ty.UpdateMaskField: strings.Join(mask, ","),
		}
	case ty.UpdateMask:
		reqURL += maskQuery(reqURL, mask)
	}

	return p.client.Do(ctx, ty.UpdateVerb, reqURL, body)
}

// topLevelFields are the distinct top-level fields a mask names, in mask
// order ("logConfig.enable" is logConfig).
func topLevelFields(mask []string) []string {
	var out []string
	seen := map[string]bool{}
	for _, m := range mask {
		f, _, _ := strings.Cut(m, ".")
		if !seen[f] {
			seen[f] = true
			out = append(out, f)
		}
	}
	return out
}

// patchOneFieldAtATime is Update for an API that refuses a patch changing
// more than one field (see catalog.Type.PatchOneField): one patch per
// changed top-level field, each awaited before the next.
//
// THE ONE PLACE UPDATE READS. Each patch changes the resource's fingerprint,
// and the next patch must quote the new one or it is refused with a 412, so
// the lock is read afresh before every patch after the first. The diff is
// still the one taken against the observation Update was handed; only the
// lock is re-read.
//
// The orphan rule as elsewhere: the first failure before anything is sent is
// returned, and one after is reported and followed by a truthful read.
func (p *Provider) patchOneFieldAtATime(ctx context.Context, ty *catalog.Type, current *resource.ResourceState, desired *resource.DesiredResource,
	attrs map[string]value.Value, reqURL string, body map[string]any, mask, fields []string, setters []*catalog.Setter) (*resource.ResourceState, error) {
	var lock any
	if ty.LockField != "" {
		if a, v := lockValue(ty, current.Attributes); v.Known {
			lock = wireValue(a, v)
		}
	}
	var failure error
	for i, f := range fields {
		part := map[string]any{f: body[f]}
		var partMask []string
		for _, m := range mask {
			if m == f || strings.HasPrefix(m, f+".") {
				partMask = append(partMask, m)
			}
		}
		if i == 0 {
			if err := ctx.Err(); err != nil {
				return nil, err // last point before the resource is modified
			}
		} else if ty.LockField != "" {
			fresh, err := p.freshLock(ctx, ty, current.ProviderID, attrs)
			if err != nil {
				failure = err
				break
			}
			lock = fresh
		}
		if lock != nil {
			part[ty.LockField] = lock
		}
		resp, err := p.sendPatch(context.WithoutCancel(ctx), ty, reqURL, part, partMask)
		if err != nil && i == 0 {
			return nil, err
		}
		if err == nil {
			_, err = p.awaitAs(ctx, ty, ty.UpdateAwaitKind(), resp)
		}
		if err != nil {
			failure = err
			break
		}
	}
	if failure != nil {
		fmt.Fprintf(os.Stderr, "gcp: %s: a one-field patch failed after an earlier one applied, reading back what exists: %v\n",
			ty.Name, failure)
	}
	return p.updateThroughSetters(ctx, ty, current, desired, setters, true, failure)
}

// freshLock reads the resource for the current value of its lock field.
func (p *Provider) freshLock(ctx context.Context, ty *catalog.Type, id string, attrs map[string]value.Value) (any, error) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Duration(ty.TimeoutSeconds)*time.Second)
	defer cancel()
	getURL, err := p.itemURL(ty, "", id, attrs)
	if err != nil {
		return nil, err
	}
	got, err := p.client.Do(ctx, http.MethodGet, getURL, nil)
	if err != nil {
		return nil, err
	}
	return got[ty.LockField], nil
}

// updateThroughSetters sends the setters Update found changed, then reads
// the resource back. patched says whether the resource's own update already
// went out: until something has been sent, a failure is still "nothing
// happened" and may be returned; after, it is reported and the truthful read
// is returned instead (see Update's orphan rule). The first failure stops the
// rest, so a lock copied from the observation is never sent stale twice.
func (p *Provider) updateThroughSetters(ctx context.Context, ty *catalog.Type, current *resource.ResourceState, desired *resource.DesiredResource, setters []*catalog.Setter, patched bool, awaitErr error) (*resource.ResourceState, error) {
	for _, s := range setters {
		if err := ctx.Err(); err != nil {
			if !patched {
				return nil, err
			}
			break
		}
		sent, err := p.callSetter(ctx, ty, s, current.ProviderID, current.Attributes, desired.Attrs)
		if err != nil && !sent && !patched {
			return nil, err
		}
		patched = patched || sent
		if err != nil {
			fmt.Fprintf(os.Stderr, "gcp: %s: %s failed, reading back what exists: %v\n", ty.Name, s.Method, err)
			if awaitErr == nil {
				awaitErr = err
			}
			break
		}
	}

	st, readErr := p.readAfterPatch(ctx, ty, current, desired.Attrs)
	switch {
	case st != nil:
		return st, nil
	case awaitErr != nil:
		// The operation failed AND the resource is not there: the failure is
		// the whole truth, and its message is the only thing the user can
		// act on.
		return nil, awaitErr
	case readErr != nil:
		return nil, fmt.Errorf("gcp: %s: patched, but the resource cannot be read back: %w", ty.Name, readErr)
	default:
		// Read's own "believed absent" is (nil, nil): something removed the
		// resource out from under the patch. An error, never (nil, nil),
		// which the host treats as a contract violation that stops the run.
		// State keeps the record it already had, so the next plan reads the
		// absence for itself and proposes a create.
		return nil, fmt.Errorf("gcp: %s: patched, but %s is no longer there", ty.Name, current.ProviderID)
	}
}

// readAfterPatch reads the patched resource back through the ordinary Read
// path.
//
// The patch already took effect, so the same rule that makes await
// uncancellable applies to reading it back: abandoning here loses the record
// of a change GCP has already made. The bound is the type's own timeout, not
// the caller's.
//
// The state it reads at carries DESIRED's attributes, not current's: the
// patch has just made desired true, so desired is the shape and the order
// GCP's answer should be expressed in (see ReconcileAttrs). Reconciling
// against the pre-patch observation instead would order a set the way it was
// before the change the user just made.
func (p *Provider) readAfterPatch(ctx context.Context, ty *catalog.Type, current *resource.ResourceState, desiredAttrs map[string]value.Value) (*resource.ResourceState, error) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx),
		time.Duration(ty.TimeoutSeconds)*time.Second)
	defer cancel()
	return p.Read(ctx, &resource.ResourceState{
		Address:    current.Address,
		Type:       current.Type,
		ProviderID: current.ProviderID,
		Attributes: desiredAttrs,
	})
}

// maskQuery renders the updateMask query argument, including its leading
// separator: "?" normally, "&" when the type's own template already carries
// a query string. Two of the catalog's nine update_urls do
// (gcp.autoscaler's "...autoscalers?autoscaler={{name}}"); neither has an
// UpdateVerb today, so neither is reachable, but a url with two "?" in it is
// not a failure worth discovering later.
func maskQuery(reqURL string, mask []string) string {
	sep := "?"
	if strings.Contains(reqURL, "?") {
		sep = "&"
	}
	return sep + "updateMask=" + url.QueryEscape(strings.Join(mask, ","))
}

// lockValue finds the lock field's current value in state, which is keyed by
// schema name while LockField is the wire name.
func lockValue(ty *catalog.Type, state map[string]value.Value) (*catalog.Attr, value.Value) {
	for name, a := range ty.Attributes {
		if a.Canonical == ty.LockField || name == ty.LockField {
			return a, state[name]
		}
	}
	return nil, value.Value{}
}
