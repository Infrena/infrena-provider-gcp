package gen

import (
	"fmt"
	"os"
	"sort"
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

// listFieldBlocklist names array-valued list-response properties that are
// never the collection's own results: pagination/diagnostic scaffolding a
// Discovery document commonly carries alongside the real result array — e.g.
// alloydb's operations.list response has both "operations" and
// "unreachable" (the locations a partial-failure list call could not reach).
// A naive "first array property" picks "unreachable" there, which is wrong.
var listFieldBlocklist = map[string]bool{
	"unreachable": true, "unreachables": true, "warnings": true, "warning": true,
}

// ListFieldOf finds the array-valued property col's List method's response
// carries its results under.
//
// There is no universal name across the corpus to assume: a sample of 532
// List methods across 25 APIs found 209 distinct field names, and "items"
// (compute's convention) covers only 24% of them — logging's buckets.list
// uses "buckets", cloudasset's savedQueries.list uses "savedQueries". So
// this inspects col's own List response schema instead of guessing.
//
// Preference order: the array property whose name matches the collection's
// own leaf (col.Path's last segment, singular or plural, case-insensitively)
// — e.g. "widgets" for a collection at .../widgets — because that is what
// names the actual result in the overwhelming majority of the corpus;
// failing that, the first array property (in sorted order, for determinism)
// that isn't a known non-result field (listFieldBlocklist); failing that,
// "", meaning nothing here can name it and the caller must decide what to do
// without one.
func ListFieldOf(d *disco.Document, col disco.Collection) string {
	m := col.Methods["list"]
	if m == nil || m.Response == nil || m.Response.Ref == "" {
		return ""
	}
	raw, ok := d.Schemas[m.Response.Ref]
	if !ok {
		return ""
	}
	resolved, err := d.Resolve(raw)
	if err != nil || resolved == nil {
		return ""
	}

	var names []string
	for name, p := range resolved.Properties {
		if p != nil && p.Type == "array" {
			names = append(names, name)
		}
	}
	sort.Strings(names)

	leaf := ""
	if len(col.Path) > 0 {
		leaf = col.Path[len(col.Path)-1]
	}
	wantPlural := strings.ToLower(leaf)
	wantSingular := strings.ToLower(singular(leaf))
	for _, name := range names {
		lower := strings.ToLower(name)
		if lower == wantPlural || lower == wantSingular {
			return name
		}
	}
	for _, name := range names {
		if !listFieldBlocklist[strings.ToLower(name)] {
			return name
		}
	}
	return ""
}

// snake converts GCP's lowerCamelCase to snake_case, KEEPING ACRONYMS WHOLE.
//
// The first version put an underscore before every capital, which shattered
// every acronym GCP uses: IPProtocol became "i_p_protocol", natIP "nat_i_p",
// IPv4Range "i_pv4_range". Twelve aliases in the catalog were mangled that way,
// on attributes people actually write -- IPProtocol is how a firewall rule
// names its protocol.
//
// Caught before v0.1.0 was tagged, which matters: an alias is a compatibility
// commitment. Adding one later is additive, but correcting a published one
// breaks whoever wrote it.
//
// Two rules beyond the obvious lower-to-upper boundary:
//
//   A run of capitals ends one character early when a lowercase follows, so
//   "IPProtocol" splits IP|Protocol rather than IPP|rotocol.
//
//   ...except when that lowercase is a plural "s" or a version suffix ("v4",
//   "v6"), which belong to the acronym: "internalIPs" is internal|IPs, and
//   "IPv4Range" is IPv4|Range. Without this the first rule reintroduces the
//   bug for exactly the names it was written to fix.
func snake(s string) string {
	r := []rune(s)
	var b strings.Builder
	for i := 0; i < len(r); i++ {
		if i > 0 && startsWord(r, i) {
			b.WriteByte('_')
		}
		b.WriteRune(unicode.ToLower(r[i]))
	}
	return b.String()
}

// startsWord reports whether r[i] begins a new snake_case word.
func startsWord(r []rune, i int) bool {
	if !unicode.IsUpper(r[i]) {
		return false
	}
	prev := r[i-1]
	// gatewayIPv4, natIP: an upper after a lower or a digit always starts one.
	if !unicode.IsUpper(prev) {
		return true
	}
	// Inside a run of capitals. It only ends here if a lowercase follows, and
	// then only if that lowercase is not part of this same acronym.
	if i+1 >= len(r) || !unicode.IsLower(r[i+1]) {
		return false
	}
	return !acronymTail(r, i+1)
}

// acronymTail reports whether the lowercase run at r[i] belongs to the capitals
// before it rather than to the next word: the "s" of IPs, the "v4" of IPv4.
func acronymTail(r []rune, i int) bool {
	if r[i] == 's' && (i+1 == len(r) || unicode.IsUpper(r[i+1])) {
		return true // IPs, externalIPs
	}
	if (r[i] == 'v' || r[i] == 'V') && i+1 < len(r) && unicode.IsDigit(r[i+1]) {
		return true // IPv4, IPv6
	}
	return false
}

// mmIndex flattens one magic-modules resource's fields by name, at every depth,
// so a Discovery property can find its lifecycle flags wherever they live.
//
// First-seen wins on a name collision. BuildAttributes puts Parameters ahead of
// Properties in the slice this walks, so a name declared in both (e.g. "name"
// appearing as both a method parameter and a body property, with different
// required/immutable flags) resolves to the Parameters entry. That is a real
// precedence decision, not an accident of append order — see
// TestParametersWinOverPropertiesOnNameCollision.
//
// TWO PASSES, names before api_names. The lookups against this index are by
// DISCOVERY's property name, and magic-modules does not always use it: 438
// fields in the corpus record the API's own spelling in api_name instead
// (compute's Firewall "allow"/api_name allowed, eventarc's Trigger
// "matchingCriteria"/api_name eventFilters). Indexing only by Name loses
// every lifecycle flag on those — measured on 2026-09-22, 5 of the 35 is_set
// fields on types this plugin ships were reachable ONLY through api_name, so
// gcp.firewall's allowed and denied shipped as ordered lists and
// reconciliation would have proposed reordering them forever.
//
// The order is what makes it safe. 14 of the corpus's 942 resources declare a
// field whose api_name is also some OTHER field's own name (compute's
// InstanceGroupManager has both "id" and an "instanceGroupManagerId" whose
// api_name is "id"), and a single pass would resolve those by whichever the
// walk happened to reach first. A real field name always wins; an api_name
// only fills a key nothing else claimed. See
// TestARealFieldNameBeatsAnotherFieldsApiName.
func mmIndex(fields []*mmv1.Field) map[string]*mmv1.Field {
	out := map[string]*mmv1.Field{}
	var walk func(fs []*mmv1.Field, key func(*mmv1.Field) string)
	walk = func(fs []*mmv1.Field, key func(*mmv1.Field) string) {
		for _, f := range fs {
			if k := key(f); k != "" {
				if _, seen := out[k]; !seen {
					out[k] = f
				}
			}
			walk(f.Properties, key)
		}
	}
	walk(fields, func(f *mmv1.Field) string { return f.Name })
	walk(fields, func(f *mmv1.Field) string { return f.ApiName })
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
		// Parameters precede Properties: on a name collision (a field magic-modules
		// declares as both a method parameter and a body property, such as "name"),
		// the Parameters entry wins. See mmIndex's doc comment.
		idx = mmIndex(append(append([]*mmv1.Field{}, mm.Parameters...), mm.Properties...))
	}
	var conflicts []string
	out, err := buildLevel(d, body, idx, aliases, true, &conflicts)
	if err != nil {
		return nil, err
	}
	// Sorted before being written: the walk that fills conflicts ranges over a Go
	// map, so the order conflicts arrive in is randomized per run. Regeneration
	// output is something a human diffs between runs to spot a real change; an
	// unstable order would bury that change in reordering noise instead.
	sort.Strings(conflicts)
	for _, c := range conflicts {
		fmt.Fprint(os.Stderr, c)
	}
	return out, nil
}

// buildLevel builds one level of attributes. topLevel is not cosmetic: infrena
// REFUSES a nested References (pkg/schema/definition.go: only a top-level
// attribute's is ever projected into a dependency), and the corpus carries 11
// nested ResourceRef fields, so emitting them would make ValidateAll reject the
// whole catalog rather than just those types.
//
// conflicts collects Output-vs-Required diagnostic lines as they're found;
// BuildAttributes sorts and emits them after the whole walk finishes, rather
// than each level printing as it goes.
func buildLevel(d *disco.Document, s *disco.Schema, idx map[string]*mmv1.Field, aliases map[string]string, topLevel bool, conflicts *[]string) (map[string]*catalog.Attr, error) {
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
		alias, hasAlias := aliases[name]
		if hasAlias && alias != "" {
			a.Aliases = append(a.Aliases, alias)
		}
		// Skip the generated form when it's identical to a curated alias already
		// appended above: sizeGb curated as "size_gb" must not end up with
		// "size_gb" listed twice.
		if sn := snake(name); sn != name && sn != alias {
			a.Aliases = append(a.Aliases, sn)
		}
		if f := idx[name]; f != nil {
			a.Required = f.Required
			a.ForceNew = f.Immutable
			if f.Output {
				a.Output = true
			}
			// A set-typed list may come back reordered. Without this the
			// reconciler treats every list as ordered, so a pure reordering
			// reads as drift and the plan never converges — the exact failure
			// reconciliation exists to prevent.
			a.Unordered = f.IsSet
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
				// build-time tool, so this ends up as a line a human reads in the
				// regeneration output, next to the warnings file. Collected here
				// rather than printed immediately — see BuildAttributes for why.
				*conflicts = append(*conflicts, fmt.Sprintf(
					"gen: %s.%s is required per magic-modules but output-only per Discovery; treating it as output-only\n",
					d.Name, name))
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
			// aliases is nil below, not merely omitted: Overlay.Aliases is
			// map[type]map[attribute]alias, with no path notation, so a curated
			// alias can only ever name a TOP-LEVEL attribute — the overlay has no
			// way to address anything nested. Curated aliases are top-level only
			// because of that, not by oversight. snake_case and the original
			// spelling still apply at every depth (see TestNestedKeysGetTheSameSpellings).
			fields, err := buildLevel(d, prop, idx, nil, false, conflicts)
			if err != nil {
				return nil, err
			}
			a.Fields = fields
		case prop.Type == "array" && prop.Items != nil:
			elem := &catalog.Attr{Canonical: name, Kind: KindOf(prop.Items)}
			if prop.Items.Type == "object" && len(prop.Items.Properties) > 0 {
				// Same nil-aliases reasoning as the object branch above: no path
				// notation exists to curate an alias for an array element's field.
				fields, err := buildLevel(d, prop.Items, idx, nil, false, conflicts)
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
