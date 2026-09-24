// createid.go gives a type an input for the id the CALLER chooses, when that
// id travels as a query parameter and the resource's own `name` cannot carry
// it.
//
// Google's dominant create convention is POST <parent>/<collection>?<thing>Id=x
// -- 271 of the 358 create methods in the pinned documents use it. magic-modules
// writes that as `create_url: .../keyRings?keyRingId={{name}}`, so the
// placeholder is the resource's `name`, and for 61 of the 67 types that fill a
// query id this way it just works, because Discovery also publishes `name` as a
// settable short id.
//
// For the rest it does not, and the reason is that magic-modules and Discovery
// use `name` for two different things. magic-modules means the short id you
// choose. Discovery means what the server returns, which for these APIs is the
// FULL resource path -- and it marks it readOnly. attrs.go then makes Output win
// over Required (correctly: if GCP sets a value, a user cannot be made to supply
// it), and the create url is left with a placeholder nothing can fill. Those
// types ship, read, import and delete, and refuse to create.
//
// Reusing `name` cannot fix it. Marking it CreateOnly looks right -- the runtime
// carries a CreateOnly attribute forward from state rather than expecting GCP to
// echo it -- but that carry-forward yields to the API when the API does answer
// ("the API does return it after all; believe the API"), and these APIs DO
// return `name`, as the full path. State would take the long form and disagree
// with the short one the user wrote, for ever.
//
// So the id gets its own attribute, named after the query parameter that
// carries it, and the create template is pointed at that instead. GCP never
// returns `keyRingId`, so the CreateOnly carry-forward behaves exactly as it was
// built to. This is the same shape as the wrapper case in build.go, which turns
// CreateServiceAccountRequest's `accountId` into an attribute; it is that
// mechanism reaching the commoner half of the convention.
package gen

import (
	"strings"

	"github.com/infrena/infrena-provider-gcp/internal/catalog"
	"github.com/infrena/infrena-provider-gcp/internal/disco"
	"github.com/infrena/infrena/pkg/value"
)

// bindCreateQueryID rewrites t's create template so each query-carried id
// placeholder that nothing can fill names a synthesized create-only attribute
// instead, and adds that attribute. It reports the parameters it bound.
//
// It changes NOTHING unless all four hold, because every one of them is a way
// to be wrong:
//
//   - the placeholder sits in the template's QUERY string, not its path. A path
//     segment is part of the resource's address and is not ours to rename.
//   - nothing else can already fill it. A type whose `name` is settable is one
//     of the 61 that work, and must be left exactly as it is.
//   - the create method really declares that query parameter. Without this the
//     generator would be inventing an input the API never offered.
//   - the parameter name is free as an attribute key, so a synthesized input can
//     never shadow a field the resource actually has.
func bindCreateQueryID(t *catalog.Type, attrs map[string]*catalog.Attr, create *disco.Method) []string {
	tmpl := t.CreateTemplate()
	q := strings.IndexByte(tmpl, '?')
	if q < 0 || create == nil {
		return nil
	}
	path, query := tmpl[:q], tmpl[q:]

	var bound []string
	for _, ph := range urlPlaceholderNames(query) {
		if a := topLevelAttr(attrs, ph); a != nil && !a.Output {
			continue // one of the 61: the resource's own field already carries it
		}
		param := queryParamFor(query, ph)
		if param == "" || param == ph {
			continue
		}
		p := create.Parameters[param]
		if p == nil || p.Location != "query" {
			continue
		}
		if _, taken := attrs[param]; taken {
			continue
		}
		attrs[param] = &catalog.Attr{
			Canonical:   param,
			Kind:        value.KindString,
			ForceNew:    true,
			CreateOnly:  true,
			Description: strings.TrimSpace(p.Description),
		}
		query = strings.ReplaceAll(query, "{{"+ph+"}}", "{{"+param+"}}")
		bound = append(bound, param)
	}
	if len(bound) == 0 {
		return nil
	}
	// Written to CreateURL specifically, never to BaseURL: BaseURL is the
	// collection every other caller reads (outsideCreatedCollection compares
	// against it, discovery lists from it), and a query string has no business
	// there. CreateTemplate prefers CreateURL when it is set, so a type that
	// had none now has one saying exactly what its POST does.
	t.CreateURL = path + query
	return bound
}

// queryParamFor returns the query parameter whose value is "{{ph}}", or "" when
// the placeholder is not a whole parameter value. Only an exact match counts:
// "?filter=name eq {{name}}" is not an id being passed, and rewriting it would
// corrupt a filter expression.
func queryParamFor(query, ph string) string {
	want := "{{" + ph + "}}"
	for _, kv := range strings.Split(strings.TrimPrefix(query, "?"), "&") {
		if k, v, ok := strings.Cut(kv, "="); ok && v == want {
			return k
		}
	}
	return ""
}

// urlPlaceholderNames lists the {{x}} and {x} names in a template, in order and
// without repeats. catalog has its own unexported copy for the same job; this
// one exists so the generator does not depend on a runtime internal.
func urlPlaceholderNames(tmpl string) []string {
	var out []string
	seen := map[string]bool{}
	for i := 0; i < len(tmpl); {
		if tmpl[i] != '{' {
			i++
			continue
		}
		openLen, closeSeq := 1, "}"
		if i+1 < len(tmpl) && tmpl[i+1] == '{' {
			openLen, closeSeq = 2, "}}"
		}
		rel := strings.Index(tmpl[i+openLen:], closeSeq)
		if rel < 0 {
			break
		}
		name := strings.TrimPrefix(tmpl[i+openLen:i+openLen+rel], "+")
		if name != "" && !seen[name] {
			seen[name] = true
			out = append(out, name)
		}
		i += openLen + rel + len(closeSeq)
	}
	return out
}

// topLevelAttr finds the attribute a url placeholder names: keyed by that name,
// or whose wire name is that name. The same rule catalog.Type.TopLevelAttr
// applies at runtime, applied here to attributes that are not on a Type yet.
func topLevelAttr(attrs map[string]*catalog.Attr, name string) *catalog.Attr {
	if a, ok := attrs[name]; ok {
		return a
	}
	for _, a := range attrs {
		if a.Canonical == name {
			return a
		}
	}
	return nil
}
