package gcprov

import (
	"fmt"
	"strings"

	"github.com/infrena/infrena-provider-gcp/internal/catalog"
	"github.com/infrena/infrena/pkg/value"
)

// ProviderID is the relative resource name itself (spec §5.5), e.g.
// "projects/p/zones/us-central1-a/instances/web1". NONE of the AWS
// <region>/<identifier>, "global/", split-at-first-slash or "|"-joining
// machinery applies here: GCP names are already hierarchical, unique and
// stable, which is exactly what that machinery exists to fake for Cloud
// Control identifiers that carry none of it.
//
// body is whatever GCP just answered (a create's response, a get's body) --
// consulted first, so a create's actual result wins over what the caller
// asked for. attrs is the caller's own resolved attributes, the fallback for
// whatever body does not carry (or for the whole id, when body is nil, e.g.
// ParseProviderID has nothing to hand it).
//
// If body carries its own "selfLink" (every mutation and get response
// does), that is authoritative -- and, per TestAnAbsoluteSelfLinkBecomesARelativeName,
// GCP returns it as an ABSOLUTE url, which is reduced to its relative form
// so the id does not depend on a hostname and matches what Import takes.
// Otherwise the id is ty.SelfLink expanded against body's own fields
// overlaid on attrs.
func ProviderID(ty *catalog.Type, body map[string]any, attrs map[string]value.Value) (string, error) {
	if raw, ok := body["selfLink"].(string); ok && raw != "" {
		return reduceSelfLink(ty, raw)
	}
	merged := make(map[string]value.Value, len(attrs)+len(body))
	for k, v := range attrs {
		merged[k] = v
	}
	for k, raw := range body {
		if k == "selfLink" {
			continue
		}
		merged[k] = fromRaw(raw)
	}
	// Offer every renamed attribute under its wire name too. self_link's
	// placeholders are written in whichever namespace their source used, and
	// gcp.resourcerecordset's ("...rrsets/{name}/{type}") came from the API's
	// own Discovery path, so it asks for "type" while attrs holds
	// "type_value". Without this, the fallback createdID leans on when an
	// operation's target cannot be reduced fails for that type, turning a
	// successful create into an error the host drops. See wireAliases.
	rel, err := ExpandURL(ty.SelfLink, wireAliases(ty.Attributes, merged))
	if err != nil {
		return "", fmt.Errorf("gcprov: %s: computing the provider id: %w", ty.Name, err)
	}
	return rel, nil
}

// reduceSelfLink turns an ABSOLUTE selfLink GCP answered with (scheme, host
// and whatever service/version prefix that host's own convention puts in
// front) into the relative resource name ty.SelfLink's template describes --
// which is the provider id.
//
// It reduces by the API's OWN prefix -- APIBaseURL's path component plus
// PathPrefix, exactly what absURL puts in front of every relative name for
// this type -- and not by the literal text at the head of ty.SelfLink. 16 of
// the 233 types have a self_link that begins with a placeholder
// ("{{parent}}/locations/..."), so there is no literal text to search for at
// all; two of them (gcp.networksecurity.addressgroup and
// gcp.networksecurity.organization.addressgroup) also return a selfLink in
// their bodies, so the literal search failed on every create -- and per the
// orphan rule, an error after a successful create orphans the resource.
//
// The host is ignored on purpose: GCP answers compute self links from
// www.googleapis.com while the catalog names compute.googleapis.com, and a
// hostname is exactly the part of a self link that must not reach an id.
// Only the path after the host is matched.
//
// The match must fall at a path-segment boundary (the path's own start, or
// right after a "/") -- an unanchored search would happily match the prefix
// in the middle of some other segment that merely ends with it ("apiv1/"
// containing "v1/") and reduce from the wrong offset.
//
// The FIRST anchored occurrence is the one taken, never the last. The prefix
// sits at the head of the path, immediately after the host; any later
// anchored occurrence is a value INSIDE the hierarchy that happens to be
// spelled the same way -- a key ring named "v1", a project whose id is
// literally "projects" -- and reducing past it silently deletes real
// segments from the id. The argument for taking the last one does not hold:
// every prefix searched for here ends in "/", and a trailing resource name
// has nothing after it, so a resource name can never produce a spurious
// anchored match in the first place.
func reduceSelfLink(ty *catalog.Type, raw string) (string, error) {
	prefix := urlPath(ty.APIBaseURL) + ty.PathPrefix
	if prefix == "" {
		return "", fmt.Errorf("gcprov: %s: no api prefix to reduce %q by", ty.Name, raw)
	}
	path := urlPath(raw)
	if i := firstAnchoredIndex(path, prefix); i >= 0 {
		return path[i+len(prefix):], nil
	}
	return "", fmt.Errorf("gcprov: %s: %q does not look like one of this type's self links (no %q)", ty.Name, raw, prefix)
}

// urlPath returns u's path with its scheme, host and leading "/" removed --
// the form a relative resource name is measured against. A u that is already
// relative comes back unchanged apart from a leading "/".
func urlPath(u string) string {
	if i := strings.Index(u, "://"); i >= 0 {
		u = u[i+3:]
		j := strings.IndexByte(u, '/')
		if j < 0 {
			return ""
		}
		u = u[j:]
	}
	return strings.TrimPrefix(u, "/")
}

// firstAnchoredIndex returns the index of the first occurrence of prefix in
// path that starts at path's own beginning or is immediately preceded by "/"
// -- i.e. at a path-segment boundary -- or -1 if none does. See
// reduceSelfLink for why the match must be anchored and why the FIRST one is
// the one that matters.
func firstAnchoredIndex(path, prefix string) int {
	for off := 0; off < len(path); {
		i := strings.Index(path[off:], prefix)
		if i < 0 {
			return -1
		}
		i += off
		if i == 0 || path[i-1] == '/' {
			return i
		}
		off = i + 1
	}
	return -1
}

// ParseProviderID recovers the attributes a provider id's own hierarchy
// encodes -- project, zone/region/location, and the resource's own local
// name -- by matching id against ty.SelfLink segment by segment. It refuses
// BEFORE any API call: an id for the wrong type, or one with no hierarchy at
// all, costs a message naming the problem rather than a confusing 404 from a
// url built out of nonsense.
//
// self_link is tried first -- the canonical shape ProviderID itself always
// produces, and so what a stored ResourceState's ProviderID always is for
// Read/Delete/Update. Import, though, takes whatever a user typed, and
// magic-modules supplies more than one accepted shape for 11 of the 233 real
// types, newline-joined in ImportFormat (e.g. gcp.bigquery.table accepts
// both its full relative name and the short "{{table_id}}" alone) --
// legitimate shorthand a user may reasonably type, not a typo. Each
// additional ImportFormat line is tried in turn, in the order magic-modules
// listed them, so a valid shorthand id parses instead of being refused for
// not matching the one canonical form.
func ParseProviderID(ty *catalog.Type, id string) (map[string]value.Value, error) {
	var firstErr error
	for _, tmpl := range importTemplates(ty) {
		attrs, err := parseAgainstTemplate(ty, tmpl, id)
		if err == nil {
			return attrs, nil
		}
		if firstErr == nil {
			firstErr = err
		}
	}
	return nil, firstErr
}

// importTemplates lists every id shape ParseProviderID accepts for ty:
// self_link first, then each of ImportFormat's own newline-joined lines that
// is not identical to self_link (magic-modules typically repeats it as the
// first line; skipped here so it is tried only once).
func importTemplates(ty *catalog.Type) []string {
	templates := []string{ty.SelfLink}
	for _, line := range strings.Split(ty.ImportFormat, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || line == ty.SelfLink {
			continue
		}
		templates = append(templates, line)
	}
	return templates
}

// parseAgainstTemplate matches id against tmpl segment by segment, the way
// url templates are built (ExpandURL's own "{{x}}"/"{x}"/reserved "{+x}"
// forms): a literal segment must match exactly, a plain placeholder captures
// one segment, and a reserved one captures the rest of the path (and must be
// tmpl's own last segment).
func parseAgainstTemplate(ty *catalog.Type, tmpl, id string) (map[string]value.Value, error) {
	tmplSegs := strings.Split(tmpl, "/")
	idSegs := strings.Split(id, "/")

	out := make(map[string]value.Value, len(tmplSegs))
	i, j := 0, 0
	for i < len(tmplSegs) {
		seg := tmplSegs[i]
		name, reserved, isPlaceholder := parsePlaceholderSegment(seg)

		if !isPlaceholder {
			if j >= len(idSegs) || idSegs[j] != seg {
				return nil, fmt.Errorf("gcprov: %q is not a valid id for %s: expected %q at segment %d",
					id, ty.Name, seg, j)
			}
			i, j = i+1, j+1
			continue
		}

		if reserved {
			if i != len(tmplSegs)-1 {
				return nil, fmt.Errorf("gcprov: %s: id template %q's reserved placeholder %q must be its last segment",
					ty.Name, tmpl, seg)
			}
			if j >= len(idSegs) {
				return nil, fmt.Errorf("gcprov: %q is too short for %s", id, ty.Name)
			}
			out[name] = value.String(strings.Join(idSegs[j:], "/"), value.SourceProvider)
			i, j = i+1, len(idSegs)
			continue
		}

		if j >= len(idSegs) {
			return nil, fmt.Errorf("gcprov: %q is too short for %s", id, ty.Name)
		}
		out[name] = value.String(idSegs[j], value.SourceProvider)
		i, j = i+1, j+1
	}
	if j != len(idSegs) {
		return nil, fmt.Errorf("gcprov: %q has more segments than %s's id shape", id, ty.Name)
	}
	return out, nil
}

// parsePlaceholderSegment reports whether seg is a WHOLE-segment placeholder
// -- "{{project}}", "{zone}" or the reserved "{+name}" -- every shape
// self_link uses one of (spec §5.2, urls.go's ExpandURL). name is the
// placeholder's own name with any braces/"+' stripped; reserved marks the
// "{+x}" form, which captures the rest of the path rather than one segment.
func parsePlaceholderSegment(seg string) (name string, reserved, ok bool) {
	if strings.HasPrefix(seg, "{{") && strings.HasSuffix(seg, "}}") && len(seg) > 4 {
		return seg[2 : len(seg)-2], false, true
	}
	if strings.HasPrefix(seg, "{") && strings.HasSuffix(seg, "}") && len(seg) > 2 {
		inner := seg[1 : len(seg)-1]
		if strings.HasPrefix(inner, "+") {
			return inner[1:], true, true
		}
		return inner, false, true
	}
	return "", false, false
}

// toRaw converts a typed Value into the plain JSON datum a GCP request body
// or a stored provider id attribute holds -- the inverse of fromRaw.
// Writing v.Raw directly would work for scalars but serialise a composite's
// []value.Value or map[string]value.Value through Value's own MarshalJSON
// (the state file's wire format, kind/known/raw wrapper and all), which is
// not what GCP's REST API accepts.
func toRaw(v value.Value) any {
	switch v.Kind {
	case value.KindList:
		items, _ := v.Raw.([]value.Value)
		out := make([]any, 0, len(items))
		for _, item := range items {
			out = append(out, toRaw(item))
		}
		return out
	case value.KindMap:
		items, _ := v.Raw.(map[string]value.Value)
		out := make(map[string]any, len(items))
		for k, item := range items {
			out[k] = toRaw(item)
		}
		return out
	default:
		return v.Raw
	}
}

// fromRaw converts a JSON-decoded datum (a GCP response body, or one of its
// nested fields) into a typed Value -- the inverse of toRaw. JSON numbers
// arrive as float64; whole numbers become KindInt so they compare equal to a
// configured integer attribute (encoding/json's only alternative, decoding
// into json.Number, would push that distinction into every caller instead).
func fromRaw(raw any) value.Value {
	switch v := raw.(type) {
	case string:
		return value.String(v, value.SourceProvider)
	case bool:
		return value.Bool(v, value.SourceProvider)
	case float64:
		if v == float64(int64(v)) {
			return value.Int(int64(v), value.SourceProvider)
		}
		return value.Float(v, value.SourceProvider)
	case int64:
		return value.Int(v, value.SourceProvider)
	case []any:
		items := make([]value.Value, len(v))
		for i, e := range v {
			items[i] = fromRaw(e)
		}
		return value.List(items, value.SourceProvider)
	case map[string]any:
		items := make(map[string]value.Value, len(v))
		for k, e := range v {
			items[k] = fromRaw(e)
		}
		return value.Map(items, value.SourceProvider)
	case nil:
		// Unreachable for a top-level attribute (callers skip a nil field
		// entirely), but a null nested inside a list or map lands here. The
		// zero Value (KindInvalid) fails loudly downstream rather than
		// masquerading as some other kind's zero value.
		return value.Value{}
	default:
		return value.String(fmt.Sprint(v), value.SourceProvider)
	}
}
