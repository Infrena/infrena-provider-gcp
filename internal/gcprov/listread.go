package gcprov

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/infrena/infrena-provider-gcp/internal/catalog"
	"github.com/infrena/infrena/pkg/resource"
	"github.com/infrena/infrena/pkg/value"
)

// ReadViaListByParent is the catalog.Type.ReadVia value meaning "this type
// has no get method: read one by listing its parent and picking the match".
// Spec G6's worked ruling, and the only ReadVia value the generator emits --
// gcp.tagbinding is the one type that carries it, because cloudresourcemanager
// v3's tagBindings collection publishes create, delete and list and no get at
// all.
const ReadViaListByParent = "list_by_parent"

// readByListingParent reads one resource of a type that has no get method,
// by listing the collection its parent scopes and picking out the entry with
// this id.
//
// It returns (nil, nil) when the parent's listing does not contain it --
// "believed absent", the same contract Read's own 404 path answers with, so
// a caller cannot tell the two reads apart and destroy still converges.
//
// THE PARENT IS A FULL RESOURCE NAME, not a relative one:
// "//cloudresourcemanager.googleapis.com/projects/my-project". That is what
// tagBindings.list's own parent parameter takes, and the "//<service>/" head
// is the Cloud Asset Inventory full-resource-name form, not something
// invented here. The service comes from ty.AssetType, whose own first
// segment is that host ("cloudresourcemanager.googleapis.com/TagBinding"),
// and DELIBERATELY NOT from ty.APIBaseURL: a resource's full name names the
// service that owns the resource, which is a generation-time fact, while
// APIBaseURL is the host requests are sent to and is legitimately rewritten
// (a test's fake, some future private endpoint). Deriving the parent from
// the endpoint would make the parent change whenever the endpoint did.
func (p *Provider) readByListingParent(ctx context.Context, ty *catalog.Type, id string) (map[string]any, error) {
	if ty.ListField == "" {
		// Nothing to read from. Refused rather than guessed: "items" is right
		// for compute and wrong for three quarters of GCP (see
		// catalog.Type.ListField), and a guess that reads no array reports a
		// resource that exists as gone, which destroys nothing and recreates
		// everything.
		return nil, fmt.Errorf("gcp: %s: read_via is %q but the catalog names no list field for it", ty.Name, ty.ReadVia)
	}
	parent, err := p.listParent(ty, id)
	if err != nil {
		return nil, err
	}
	rel, err := ExpandURL(ty.BaseURL, p.scopeAttrs(p.settings.Project))
	if err != nil {
		return nil, fmt.Errorf("gcp: %s: building the parent listing url: %w", ty.Name, err)
	}

	pageToken := ""
	for {
		reqURL := absURL(ty, rel) + "?parent=" + url.QueryEscape(parent)
		if pageToken != "" {
			reqURL += "&pageToken=" + url.QueryEscape(pageToken)
		}
		body, err := p.client.Do(ctx, http.MethodGet, reqURL, nil)
		if err != nil {
			return nil, err
		}
		for _, item := range listItems(body, ty.ListField) {
			if namesSameResource(item, id) {
				return item, nil
			}
		}
		pageToken, _ = body["nextPageToken"].(string)
		if pageToken == "" {
			return nil, nil // listed the whole parent; it is not there
		}
	}
}

// listParent is the full resource name to list under when looking for id.
//
// THE ID ITSELF IS ASKED FIRST, and for gcp.tagbinding the id always
// answers. A binding's name embeds the bound resource's own full name,
// percent-escaped into a single segment:
//
//	tagBindings/%2F%2Fcloudresourcemanager.googleapis.com%2Fprojects%2F123456789012/tagValues/281479230039359
//
// so unescaping that segment gives back exactly the value
// tagBindings.list's `parent` parameter takes.
//
// This changes what Read MEANS for the type, and deliberately. The parent
// used to come from Settings.Project, so a tag bound to a BUCKET or an
// INSTANCE -- which is most of what tag bindings are for -- was created
// successfully and then never found again: the project's own listing does
// not contain it, "not in the listing" is "believed absent", and the next
// plan proposed creating it a second time while destroy forgot it. Reading
// a resource under the parent ITS OWN ID NAMES is the only way that
// converges, and it needs no new configuration.
//
// The settings-derived parent remains the fallback for an id that carries
// no full resource name, which is every type that might later take this
// path and does not spell its id the way CRM does.
func (p *Provider) listParent(ty *catalog.Type, id string) (string, error) {
	if parent := fullResourceNameIn(id); parent != "" {
		return parent, nil
	}
	return p.hierarchyParent(ty)
}

// fullResourceNameIn returns the Cloud Asset Inventory full resource name a
// provider id carries in one of its segments, or "".
//
// The test is the FORM, not the type: a segment that percent-decodes to
// "//<host>/<something>" is a full resource name, and nothing else in a GCP
// relative resource name looks like that -- a leading "//" cannot occur in a
// relative name at all, since an empty path segment is not a thing GCP
// names. So this recognises the shape wherever it appears rather than
// knowing about tag bindings, and a segment that merely contains an escaped
// slash is left alone.
func fullResourceNameIn(id string) string {
	for _, seg := range strings.Split(id, "/") {
		if !strings.Contains(seg, "%2F") && !strings.Contains(seg, "%2f") {
			continue
		}
		decoded, err := url.PathUnescape(seg)
		if err != nil {
			continue
		}
		if !strings.HasPrefix(decoded, "//") || strings.Count(decoded, "/") < 3 {
			continue
		}
		return decoded
	}
	return ""
}

// hierarchyParent is the full resource name of the hierarchy node this
// provider instance is configured against -- the scope a parent-filtered
// list is taken under.
//
// Only a project is built today, because Settings names only a project;
// ty.ParentRoot is still checked rather than assumed, so a type rooted at a
// folder or an organization gets an error naming what is missing instead of
// a list taken under the wrong node, which would answer "absent" for a
// resource that exists.
func (p *Provider) hierarchyParent(ty *catalog.Type) (string, error) {
	service, _, ok := strings.Cut(ty.AssetType, "/")
	if !ok || service == "" {
		return "", fmt.Errorf("gcp: %s: no asset type, so its parent's full resource name cannot be built", ty.Name)
	}
	if root := ty.ParentRoot; root != "" && root != "projects" {
		return "", fmt.Errorf("gcp: %s: is rooted at %s, which this instance's settings do not name", ty.Name, root)
	}
	if p.settings.Project == "" {
		return "", fmt.Errorf("gcp: %s: no project is configured to list its bindings under", ty.Name)
	}
	return "//" + service + "/projects/" + p.settings.Project, nil
}

// namesSameResource reports whether a listed entry is the one id names.
//
// Both spellings are accepted because both occur: GCP answers a tag binding
// with its full relative name ("tagBindings/abc") while a type whose
// self_link captures only the last segment produces a bare id, and a list
// elsewhere may answer with an absolute selfLink. Comparing the whole string
// first and the last segment second matches the exact case without letting
// the loose one hide a mismatch -- two resources in ONE collection cannot
// share a last segment, since that segment is what makes them distinct
// inside it.
func namesSameResource(item map[string]any, id string) bool {
	for _, field := range []string{"name", "selfLink"} {
		v, ok := item[field].(string)
		if !ok || v == "" {
			continue
		}
		if v == id || lastPathSegment(v) == lastPathSegment(id) {
			return true
		}
	}
	return false
}

// listItems pulls the array a list response carries its results under.
//
// field is ty.ListField and is never defaulted to "items": measured across
// the fetched corpus on 2026-09-22 there are 209 distinct array-field names
// across 532 list methods and "items" covers 127 of them, so a hardcoded
// "items" works for compute and silently returns nothing everywhere else --
// which reads as "this project has none of those" rather than as a bug.
//
// A response with no such field, or one whose value is not an array of
// objects, yields nothing rather than an error: an empty collection is a
// legitimate 200 and GCP omits an empty array entirely rather than sending
// [].
func listItems(body map[string]any, field string) []map[string]any {
	raw, ok := body[field].([]any)
	if !ok {
		return nil
	}
	out := make([]map[string]any, 0, len(raw))
	for _, e := range raw {
		if item, ok := e.(map[string]any); ok {
			out = append(out, item)
		}
	}
	return out
}

// stateFromListing builds the ResourceState for a read that went through a
// parent listing, which is stateFrom's work plus one correction: THE ID IS
// THE ONE WE MATCHED BY, never one recomputed from the entry.
//
// stateFrom recomputes the id so that a rename GCP made underneath a
// resource is noticed. There is nothing to notice here: the entry was found
// by matching this very id against the parent's listing, so it is current by
// construction.
//
// It is no longer also a REPAIR. gcp.tagbinding's self_link used to be
// "tagBindings/{{name}}" -- two segments, taken from magic-modules' first
// import_format line, because its own self_link is a LIST url -- while a
// real binding's name has four, and expanding one into the other produced
// "tagBindings/tagBindings%2F...", an id addressing nothing. The id shape
// now comes from the API's own delete path ("{+name}", pattern
// "^tagBindings/.*$"), so ProviderID answers correctly for this type too and
// this line is back to being the small optimisation its first sentence
// describes.
func (p *Provider) stateFromListing(ty *catalog.Type, current *resource.ResourceState, idAttrs map[string]value.Value, body map[string]any) (*resource.ResourceState, error) {
	st, err := p.stateFrom(ty, current, idAttrs, body)
	if err != nil {
		return nil, err
	}
	st.ProviderID = current.ProviderID
	return st, nil
}
