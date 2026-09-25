package gen

import (
	"net/http"
	"sort"
	"strings"

	"github.com/infrena/infrena-provider-gcp/internal/catalog"
	"github.com/infrena/infrena-provider-gcp/internal/disco"
	"github.com/infrena/infrena-provider-gcp/internal/mmv1"
)

// discoveredSetters reads magic-modules' per-field update_url -- the fields a
// resource changes through a method of its own rather than its update -- and
// keeps each method only where Discovery agrees with it:
//
//   - the collection publishes a method of that name, with that verb, on the
//     resource's own address (selfLink plus "/setLabels");
//   - its request schema holds exactly the fields magic-modules groups under
//     that url, plus at most one fingerprint, which is the lock.
//
// Anything else is dropped and its fields keep whatever the rest of the
// generator decided, which for a field no update reaches is a replacement.
// That is the safe direction: a setter admitted wrongly sends a request the
// API refuses or misreads, a setter refused costs a replacement, which is
// what those fields did before this existed. Measured on 2026-09-24 this
// drops compute instance's setDeletionProtection (its value is a query
// parameter, not a body) and updateShieldedInstanceConfig (its request is
// the config itself, not a wrapper naming the field).
func discoveredSetters(d *disco.Document, col disco.Collection, mm *mmv1.Resource, t *catalog.Type) []catalog.Setter {
	if mm == nil {
		return nil
	}
	selfLink, attrs := t.SelfLink, t.Attributes
	byCanonical := map[string]*catalog.Attr{}
	for _, a := range attrs {
		byCanonical[a.Canonical] = a
	}

	type group struct {
		verb    string
		members []string
	}
	groups := map[string]*group{}
	var order []string
	for _, f := range mm.Properties {
		verb := strings.ToUpper(f.UpdateVerb)
		if f.UpdateURL == "" || addressesTheResource(f.UpdateURL) || (verb != http.MethodPost && verb != http.MethodPatch) {
			continue
		}
		name := f.Name
		if f.ApiName != "" {
			name = f.ApiName
		}
		key := verb + " " + f.UpdateURL
		if groups[key] == nil {
			groups[key] = &group{verb: verb}
			order = append(order, key)
		}
		groups[key].members = append(groups[key].members, name)
	}

	var out []catalog.Setter
	for _, key := range order {
		g := groups[key]
		tmpl := key[len(g.verb)+1:]
		if i := strings.IndexByte(tmpl, '?'); i >= 0 {
			tmpl = tmpl[:i]
		}
		method := tmpl[strings.LastIndexByte(tmpl, '/')+1:]
		if method == "" || strings.ContainsAny(method, "{}:") {
			continue
		}
		m := setterMethod(col, method, g.verb)
		if m == nil || m.Request == nil {
			continue
		}
		path := setterPath(strings.TrimPrefix(m.Path, t.PathPrefix), method, selfLink)
		if path == "" {
			continue
		}
		req, err := d.Resolve(d.Schemas[m.Request.Ref])
		if err != nil || req == nil {
			continue
		}

		s := catalog.Setter{Method: method, Path: path, Verb: g.verb}
		for _, member := range g.members {
			a := byCanonical[member]
			switch {
			case a == nil:
				// A Terraform-only name (a target HTTPS proxy's
				// certificateManagerCertificates is its sslCertificates).
			case isFingerprint(member):
				s.Lock = member
			default:
				s.Fields = append(s.Fields, member)
			}
		}
		if len(s.Fields) == 0 || !requestIsExactly(req, s.Fields, s.Lock) {
			continue
		}
		sort.Strings(s.Fields)
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Method < out[j].Method })
	return out
}

// setterMethod finds the collection's method called method, with the given
// verb, whose path ends in that name.
func setterMethod(col disco.Collection, method, verb string) *disco.Method {
	for name, m := range col.Methods {
		if name == method && strings.EqualFold(m.HTTPMethod, verb) && strings.HasSuffix(m.Path, "/"+method) {
			return m
		}
	}
	return nil
}

// setterPath spells the method's path with self_link's placeholders, so the
// resource's own id fills it, or returns "" when it cannot be.
//
// When the method sits on the resource's own address its placeholders are
// renamed by position: compute's setLabels says {resource} where the address
// says {address}. When it sits elsewhere -- compute's global proxies drop
// "/global/" for setUrlMap and setSslCertificates -- every placeholder must
// already be one self_link names, or nothing this provider holds could fill
// it.
func setterPath(path, method, selfLink string) string {
	base := strings.TrimSuffix(path, "/"+method)
	own := placeholderShapeRE.FindAllString(selfLink, -1)
	if normalizeTemplateShape(base) == normalizeTemplateShape(selfLink) {
		i := 0
		return placeholderShapeRE.ReplaceAllStringFunc(base, func(string) string {
			i++
			return own[i-1]
		}) + "/" + method
	}
	known := map[string]bool{}
	for _, p := range own {
		known[strings.Trim(p, "{}+")] = true
	}
	for _, p := range placeholderShapeRE.FindAllString(base, -1) {
		if !known[strings.Trim(p, "{}+")] {
			return ""
		}
	}
	return path
}

// requestIsExactly reports whether req's properties are fields plus lock and
// nothing else. A property left over is something this provider would not
// send, and a field the request does not have is something it would send
// that the method has never heard of.
func requestIsExactly(req *disco.Schema, fields []string, lock string) bool {
	want := map[string]bool{}
	for _, f := range fields {
		want[f] = true
	}
	if lock != "" {
		want[lock] = true
	}
	if len(req.Properties) != len(want) {
		return false
	}
	for name := range req.Properties {
		if !want[name] {
			return false
		}
	}
	return true
}

func isFingerprint(name string) bool {
	return strings.HasSuffix(strings.ToLower(name), "fingerprint")
}

// applySetters makes every field a setter carries settable and not ForceNew:
// magic-modules says it changes in place, and Discovery has just confirmed
// the method exists. Discovery's own "[Output Only]" on such a field (a
// backend service's securityPolicy) means only that the resource's insert and
// update ignore it, which is the reason the setter exists. A field
// magic-modules itself calls output stays output.
//
// With no update verb, every other settable field is then ForceNew, because
// the host will now call Update for this type and Update has nothing else to
// send.
func applySetters(t *catalog.Type, mm *mmv1.Resource) {
	if len(t.Setters) == 0 {
		return
	}
	mmOutput := map[string]bool{}
	for _, f := range mm.Properties {
		name := f.Name
		if f.ApiName != "" {
			name = f.ApiName
		}
		mmOutput[name] = f.Output
	}
	var carried []string
	for _, s := range t.Setters {
		carried = append(carried, s.Fields...)
	}
	if t.UpdateVerb == "" {
		applyPatchAllowlist(t.Attributes, carried, SourceDiscovery)
	}
	for _, a := range t.Attributes {
		if t.SetterFor(a.Canonical) == nil {
			continue
		}
		if a.ForceNew {
			overrule(a, "immutable", SourceMM)
			addSource(a, "immutable", SourceDiscovery)
		}
		a.ForceNew = false
		if !mmOutput[a.Canonical] && a.Output {
			overrule(a, "output", SourceMM)
			a.Output = false
		}
	}
}
