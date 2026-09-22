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
	rel, err := ExpandURL(ty.SelfLink, merged)
	if err != nil {
		return "", fmt.Errorf("gcprov: %s: computing the provider id: %w", ty.Name, err)
	}
	return rel, nil
}

// reduceSelfLink turns an ABSOLUTE selfLink GCP answered with (scheme, host
// and whatever service/version prefix that host's own convention puts in
// front) into the relative resource name ty.SelfLink's template describes.
//
// The template's own literal text before its first placeholder (e.g.
// "projects/" for "projects/{{project}}/zones/{{zone}}/instances/{{name}}")
// is found inside raw, and everything from there on is the relative name --
// whatever precedes it (host, api name, version segment) is exactly the
// part a hostname change would alter, which is why it is never stored.
func reduceSelfLink(ty *catalog.Type, raw string) (string, error) {
	prefix := ty.SelfLink
	if i := strings.IndexByte(prefix, '{'); i >= 0 {
		prefix = prefix[:i]
	}
	if prefix == "" {
		return "", fmt.Errorf("gcprov: %s: self_link has no literal prefix to find %q by", ty.Name, raw)
	}
	i := strings.Index(raw, prefix)
	if i < 0 {
		return "", fmt.Errorf("gcprov: %s: %q does not look like one of this type's self links (no %q)", ty.Name, raw, prefix)
	}
	return raw[i:], nil
}

// ParseProviderID recovers the attributes a provider id's own hierarchy
// encodes -- project, zone/region/location, and the resource's own local
// name -- by matching id against ty.SelfLink segment by segment. It refuses
// BEFORE any API call: an id for the wrong type, or one with no hierarchy at
// all, costs a message naming the problem rather than a confusing 404 from a
// url built out of nonsense.
func ParseProviderID(ty *catalog.Type, id string) (map[string]value.Value, error) {
	tmplSegs := strings.Split(ty.SelfLink, "/")
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
				return nil, fmt.Errorf("gcprov: %s: self_link's reserved placeholder %q must be its last segment",
					ty.Name, seg)
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
