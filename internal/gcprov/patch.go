package gcprov

import (
	"context"
	"fmt"
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
		case inURL[name]:
			// The url already says it: project, region/zone/location, name,
			// and every type-specific id segment. See urlIdentifying.
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
		body[a.Canonical] = toRaw(want)
		mask = append(mask, a.Canonical)
	}
	return body, mask
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
		sub[f.Canonical] = toRaw(w)
		mask = append(mask, child)
	}
	return sub, mask
}

// urlIdentifying is the set of attribute names this type's own item url
// consumes -- self_link's placeholders, plus update_url's when the catalog
// names one. They are excluded from the patch entirely, for the same reason
// requestBody excludes them from a create's body: the url already said it.
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
	for n := range placeholderNames(ty.UpdateURL) {
		names[n] = true
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
	// 147 of the 233 types publish no update method at all, and
	// Capabilities.Update is derived from exactly this field, so the host
	// plans a replacement for them and never calls Update. Refusing here
	// rather than defaulting to PATCH keeps a catalog or host regression
	// from quietly patching a type whose API has no update method: nothing
	// has been sent yet, so an error costs nothing.
	if ty.UpdateVerb == "" {
		return nil, fmt.Errorf("gcp: %s: publishes no update method; every change to it replaces the resource", ty.Name)
	}

	body, mask := BuildMask(ty, current.Attributes, desired.Attrs)
	if len(mask) == 0 {
		// Nothing changed. Sending an empty patch would be a request against
		// a project-wide quota for no effect, and some APIs reject it
		// outright.
		return current, nil
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
	// NOT every updatable type takes an updateMask. Measured across the 233
	// generated types: 86 are updatable at all (they have an UpdateVerb, all
	// of them PATCH), and only 71 of those use a mask -- gcp.firewall,
	// gcp.connection, gcp.router and 12 others PATCH without one. Appending
	// the parameter unconditionally sends a query argument those APIs never
	// asked for.
	if ty.UpdateMask {
		reqURL += maskQuery(reqURL, mask)
	}

	if err := ctx.Err(); err != nil {
		return nil, err // last point before the resource is modified
	}
	resp, err := p.client.Do(ctx, ty.UpdateVerb, reqURL, body)
	if err != nil {
		// The PATCH itself failed, so nothing was changed, and state still
		// records the resource at the id it already had. Safe to error.
		return nil, err
	}

	// From here on the patch is real, and errors are REPORTED, not returned,
	// for as long as there is a truthful state to return instead.
	_, awaitErr := p.await(ctx, ty, resp)
	if awaitErr != nil {
		fmt.Fprintf(os.Stderr, "gcp: %s: update reported a failure, reading back what exists: %v\n",
			ty.Name, awaitErr)
	}

	st, readErr := p.readAfterPatch(ctx, ty, current)
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
func (p *Provider) readAfterPatch(ctx context.Context, ty *catalog.Type, current *resource.ResourceState) (*resource.ResourceState, error) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx),
		time.Duration(ty.TimeoutSeconds)*time.Second)
	defer cancel()
	return p.Read(ctx, current)
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
