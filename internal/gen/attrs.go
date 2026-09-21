package gen

import (
	"fmt"
	"os"
	"strings"
	"unicode"

	"github.com/infrena/infrena-provider-gcp/internal/catalog"
	"github.com/infrena/infrena-provider-gcp/internal/disco"
	"github.com/infrena/infrena-provider-gcp/internal/mmv1"
	"github.com/infrena/infrena/pkg/value"
)

// keywords are infrena resource keys an attribute may not shadow. A property
// with one of these names is exposed with "_value" appended.
var keywords = map[string]string{
	"type":      "type_value",
	"provider":  "provider_value",
	"lifecycle": "lifecycle_value",
}

// KindOf maps a Discovery type to an infrena kind.
//
// The int64 case is the one that matters: GCP serialises 64-bit integers as JSON
// STRINGS (compute disk sizes, quotas), so a schema saying type string, format
// int64 is a string as far as the wire is concerned. Typing it as KindInt would
// break every round trip.
func KindOf(s *disco.Schema) value.Kind {
	if s == nil {
		return value.KindString
	}
	switch s.Type {
	case "integer":
		return value.KindInt
	case "number":
		return value.KindFloat
	case "boolean":
		return value.KindBool
	case "array":
		return value.KindList
	case "object":
		return value.KindMap
	default:
		return value.KindString
	}
}

// ScopeOf reads the location axis out of a URL template.
func ScopeOf(baseURL string) catalog.Scope {
	switch {
	case strings.Contains(baseURL, "/zones/"):
		return catalog.ScopeZonal
	case strings.Contains(baseURL, "/regions/"), strings.Contains(baseURL, "/locations/"):
		return catalog.ScopeRegional
	default:
		return catalog.ScopeGlobal
	}
}

// AwaitOf picks the await strategy from the shape of the method's response.
//
// Deciding from the Operation SCHEMA rather than from the API's name is what
// makes this generic: container, dns and sqladmin all use compute-style
// operations without being compute.
func AwaitOf(d *disco.Document, m *disco.Method) (catalog.AwaitKind, string) {
	if m == nil || m.Response == nil || m.Response.Ref != "Operation" {
		return catalog.AwaitNone, ""
	}
	op, ok := d.Schemas["Operation"]
	if !ok {
		return catalog.AwaitNone, ""
	}
	if _, isLRO := op.Properties["done"]; isLRO {
		return catalog.AwaitLongRunning, ""
	}
	if _, isCompute := op.Properties["status"]; isCompute {
		return catalog.AwaitComputeOperation, ""
	}
	return catalog.AwaitNone, ""
}

// snake converts GCP's lowerCamelCase to snake_case.
func snake(s string) string {
	var b strings.Builder
	for i, r := range s {
		if unicode.IsUpper(r) {
			if i > 0 {
				b.WriteByte('_')
			}
			b.WriteRune(unicode.ToLower(r))
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// mmIndex flattens one magic-modules resource's fields by name, at every depth,
// so a Discovery property can find its lifecycle flags wherever they live.
func mmIndex(fields []*mmv1.Field) map[string]*mmv1.Field {
	out := map[string]*mmv1.Field{}
	var walk func(fs []*mmv1.Field)
	walk = func(fs []*mmv1.Field) {
		for _, f := range fs {
			if _, seen := out[f.Name]; !seen {
				out[f.Name] = f
			}
			walk(f.Properties)
		}
	}
	walk(fields)
	return out
}

// BuildAttributes turns one request-body schema plus its magic-modules
// definition into catalog attributes.
//
// Discovery is authoritative for SHAPE (what exists, what type, what is
// output-only); magic-modules is authoritative for LIFECYCLE (required,
// immutable, references). Neither source overrides the other on its own ground,
// which is why a property magic-modules has never heard of still ships, just
// without a ForceNew.
func BuildAttributes(d *disco.Document, body *disco.Schema, mm *mmv1.Resource, aliases map[string]string) (map[string]*catalog.Attr, error) {
	if body == nil {
		return nil, fmt.Errorf("no request body schema")
	}
	var idx map[string]*mmv1.Field
	if mm != nil {
		idx = mmIndex(append(append([]*mmv1.Field{}, mm.Parameters...), mm.Properties...))
	}
	return buildLevel(d, body, idx, aliases, true)
}

// buildLevel builds one level of attributes. topLevel is not cosmetic: infrena
// REFUSES a nested References (pkg/schema/definition.go: only a top-level
// attribute's is ever projected into a dependency), and the corpus carries 11
// nested ResourceRef fields, so emitting them would make ValidateAll reject the
// whole catalog rather than just those types.
func buildLevel(d *disco.Document, s *disco.Schema, idx map[string]*mmv1.Field, aliases map[string]string, topLevel bool) (map[string]*catalog.Attr, error) {
	out := map[string]*catalog.Attr{}
	for name, prop := range s.Properties {
		key := name
		if renamed, clash := keywords[name]; clash {
			key = renamed
		}
		a := &catalog.Attr{
			Canonical:   name,
			Kind:        KindOf(prop),
			Output:      d.OutputOnly(prop),
			Description: strings.TrimSpace(prop.Description),
		}
		if alias, ok := aliases[name]; ok && alias != "" {
			a.Aliases = append(a.Aliases, alias)
		}
		if sn := snake(name); sn != name {
			a.Aliases = append(a.Aliases, sn)
		}
		if f := idx[name]; f != nil {
			a.Required = f.Required
			a.ForceNew = f.Immutable
			if f.Output {
				a.Output = true
			}
			// Output WINS over Required, and the disagreement is reported.
			//
			// The two come from independent sources that do not cross-validate:
			// Output is Discovery's readOnly/prose union, Required is
			// magic-modules' `required`, which sometimes means "must appear in
			// the request shape" for a field the server itself populates. If GCP
			// sets a value, a user cannot be required to supply it.
			//
			// Left alone this produces Required+Computed, which schema.Validate
			// refuses — so it would fail, but at catalog-generation time, as a
			// generic error against some deep attribute with nothing pointing back
			// to the source conflict. Clearing it here and naming the field turns
			// an opaque future failure into an attributable one.
			if a.Output && a.Required {
				a.Required = false
				// The generator's own stderr, not the plugin's: gen-gcp is a
				// build-time tool, so this is a line a human reads in the
				// regeneration output, next to the warnings file.
				fmt.Fprintf(os.Stderr,
					"gen: %s.%s is required per magic-modules but output-only per Discovery; treating it as output-only\n",
					d.Name, name)
			}
			if topLevel && f.Type == "ResourceRef" && f.Resource != "" {
				attr := f.Imports
				if attr == "" {
					attr = "selfLink"
				}
				// The target's infrena name is filled in by build.go, which is the
				// only place that knows the whole name map.
				a.Ref = &catalog.RefTarget{Type: f.Resource, Attribute: attr}
			}
		}

		switch {
		case prop.Type == "object" && len(prop.Properties) == 0:
			// Free-form: additionalProperties with no declared properties, or an
			// object the resolver truncated. Copied exactly; translating or pruning
			// its keys would corrupt user data.
			a.Opaque = true
		case prop.Type == "object":
			fields, err := buildLevel(d, prop, idx, nil, false)
			if err != nil {
				return nil, err
			}
			a.Fields = fields
		case prop.Type == "array" && prop.Items != nil:
			elem := &catalog.Attr{Canonical: name, Kind: KindOf(prop.Items)}
			if prop.Items.Type == "object" && len(prop.Items.Properties) > 0 {
				fields, err := buildLevel(d, prop.Items, idx, nil, false)
				if err != nil {
					return nil, err
				}
				elem.Fields = fields
			} else if prop.Items.Type == "object" {
				elem.Opaque = true
			}
			a.Elem = elem
		}
		out[key] = a
	}
	return out, nil
}
