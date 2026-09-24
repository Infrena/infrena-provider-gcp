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
	"net/http"
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

// addCreateIDParameter sends the new resource's id the way the API asks for
// it, when the create url does not carry it at all.
//
// Google's convention (AIP-133) is POST <parent>/<collection>?<resource>Id=x,
// with the parameter named after the resource: jobs takes jobId, aclPolicies
// takes aclPolicyId. Measured on 2026-09-24, 21 shipping types the catalog
// said could create sent no id at all -- mostly types magic-modules does not
// cover, so there was no create_url to carry one. Most mark it Required, so
// every create was refused; Cloud Run's is optional, and there the name was
// sent in the body instead, which Cloud Run refuses outright: "job.name must
// be empty on CreateJobRequest". The live suite found it, on a Cloud Run job.
//
// The id gets its own create-only attribute and the url names it, the same
// shape bindCreateQueryID gives a key ring. And the resource's own `name`
// becomes output-only: for every one of these types it is the full resource
// name, which Google assigns from the parent and this id, so it is not the
// user's to send.
//
// Required follows the parameter's own "Required." tag, so a configuration
// that leaves out an id Google insists on is refused at plan time rather than
// by Google after the request is sent.
func addCreateIDParameter(t *catalog.Type, attrs map[string]*catalog.Attr, create *disco.Method) string {
	if create == nil || t.CreateVerb == http.MethodPut {
		return "" // a PUT create names the resource in its own path
	}
	tmpl := t.CreateTemplate()
	path, query, _ := strings.Cut(tmpl, "?")
	var leaf string
	for _, seg := range strings.Split(strings.Trim(path, "/"), "/") {
		if seg != "" && !strings.HasPrefix(seg, "{") {
			leaf = seg
		}
	}
	if leaf == "" {
		return ""
	}
	param := singularSegment(leaf) + "Id"
	p := create.Parameters[param]
	if p == nil || p.Location != "query" {
		return ""
	}
	for _, kv := range strings.Split(query, "&") {
		if k, _, _ := strings.Cut(kv, "="); k == param || k == snake(param) {
			return "" // already sent, under the API's spelling or magic-modules'
		}
	}
	sep := "?"
	if query != "" {
		sep = "&"
	}
	// Where the id template ends in "/{{name}}", this schema's `name` already
	// IS the short id -- magic-modules' model -- so it fills the parameter
	// itself, the way magic-modules' own create_url for these types does:
	// "?customTargetTypeId={{name}}". No second attribute, and so no
	// collision with a resource field of the parameter's name: Cloud Deploy's
	// Target has an output-only `targetId` of its own, and a second input
	// under that key is exactly what the guard below refuses. The full name
	// Google answers with is reported back as the short one by the runtime
	// (gcprov.shortNameFromOwnID), so configuration and state agree.
	if n := attrs["name"]; n != nil && !n.Output && selfLinkEndsInName(t.SelfLink) {
		t.CreateURL = tmpl + sep + param + "={{name}}"
		return param
	}
	if _, taken := attrs[param]; taken {
		return ""
	}
	attrs[param] = &catalog.Attr{
		Canonical:   param,
		Kind:        value.KindString,
		ForceNew:    true,
		CreateOnly:  true,
		Required:    disco.Behaviors(&disco.Schema{Description: p.Description})[disco.BehaviorRequired],
		Description: strings.TrimSpace(p.Description),
	}
	t.CreateURL = tmpl + sep + param + "={{" + param + "}}"
	if n := attrs["name"]; n != nil && !n.Output {
		n.Output, n.Required, n.ForceNew = true, false, false
	}
	return param
}

// selfLinkEndsInName is gcprov's rule, restated: an id template whose last
// segment is the bare `name` placeholder means `name` is the short id.
func selfLinkEndsInName(tmpl string) bool {
	return strings.HasSuffix(tmpl, "/{{name}}") || strings.HasSuffix(tmpl, "/{name}")
}

// fillPreCreateTokens binds a create-url query value magic-modules leaves as
// PRE_CREATE_REPLACE_ME, for its pre_create hook to fill, to an attribute of
// the same name as the query parameter, which the user writes. compute's
// NodeGroup: "?initialNodeCount=PRE_CREATE_REPLACE_ME", filled by Terraform
// from initial_size. The parameter is Discovery's (initialNodeCount, a
// required integer), so the attribute is typed and required from it, and
// create-only: it is a query parameter of the insert and nothing else.
//
// Only a token Discovery publishes as a query parameter of the create is
// bound. Anything else is left in place, and the type is refused with the
// token named (buildType).
func fillPreCreateTokens(t *catalog.Type, attrs map[string]*catalog.Attr, create *disco.Method) {
	path, query, ok := strings.Cut(t.CreateURL, "?")
	if !ok || create == nil || !strings.Contains(query, "PRE_CREATE_REPLACE_ME") {
		return
	}
	kvs := strings.Split(query, "&")
	for i, kv := range kvs {
		k, v, _ := strings.Cut(kv, "=")
		p := create.Parameters[k]
		if v != "PRE_CREATE_REPLACE_ME" || p == nil || p.Location != "query" {
			continue
		}
		if a := attrs[k]; a == nil {
			attrs[k] = &catalog.Attr{
				Canonical:   k,
				Kind:        KindOf(&disco.Schema{Type: p.Type, Format: p.Format}),
				ForceNew:    true,
				CreateOnly:  true,
				Required:    p.Required,
				Description: strings.TrimSpace(p.Description),
			}
		} else if a.Output {
			continue
		}
		kvs[i] = k + "={{" + k + "}}"
	}
	t.CreateURL = path + "?" + strings.Join(kvs, "&")
}
