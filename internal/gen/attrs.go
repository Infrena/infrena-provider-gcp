package gen

import (
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"
	"unicode"

	"github.com/infrena/infrena-provider-gcp/internal/catalog"
	"github.com/infrena/infrena-provider-gcp/internal/disco"
	"github.com/infrena/infrena-provider-gcp/internal/mmv1"
	"github.com/infrena/infrena/pkg/value"
)

// keywords are infrena resource keys a top-level attribute may not shadow. A
// top-level property with one of these names is exposed with "_value"
// appended; a nested one keeps its name.
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
// Deciding from the operation schema's SHAPE rather than from the API's name
// is what makes this generic: container and sqladmin use compute-style
// operations without being compute.
//
// And from its shape rather than its NAME, too. This used to demand a response
// schema called exactly "Operation", which is what most APIs publish -- but not
// all. Eventarc, Cloud Run v2 and Firestore publish google.longrunning.Operation
// as "GoogleLongrunningOperation", and API Gateway as "ApigatewayOperation". 13
// shipping types in those APIs -- gcp.service, gcp.job, every Eventarc type,
// gcp.firestore.databas among them -- were classified as returning the finished
// resource, so a create handed back an operation still running and Create took
// it as done.
//
// The shape tests are strict on purpose, because a resource can look like an
// operation from one field. dataproc's Job has `done` and `status` and is a
// resource, not an operation, so "has done" is not enough; DNS publishes its own
// "Operation" with only a `status`, which is not compute's.
func AwaitOf(d *disco.Document, m *disco.Method) (catalog.AwaitKind, string) {
	if m == nil || m.Response == nil || m.Response.Ref == "" {
		return catalog.AwaitNone, ""
	}
	op, ok := d.Schemas[m.Response.Ref]
	if !ok || op == nil {
		return catalog.AwaitNone, ""
	}
	switch {
	case isLongRunningOperation(op):
		return catalog.AwaitLongRunning, ""
	case isComputeOperation(op):
		return catalog.AwaitComputeOperation, ""
	}
	return catalog.AwaitNone, ""
}

// isLongRunningOperation is google.longrunning.Operation, whatever the API
// called it: a name to poll, a done flag, and a response or an error to finish
// with. A resource that merely has a `done` field (dataproc's Job) has no name
// of this kind and no response.
func isLongRunningOperation(s *disco.Schema) bool {
	p := s.Properties
	return p["done"] != nil && p["name"] != nil && (p["response"] != nil || p["error"] != nil)
}

// isComputeOperation is the compute-style operation compute, container and
// sqladmin share: a status to watch, the resource it acts on, and what kind of
// operation it is. All three are required, so a resource with a `status` field
// -- which is most of them -- is never mistaken for one.
func isComputeOperation(s *disco.Schema) bool {
	p := s.Properties
	return p["status"] != nil && p["targetLink"] != nil && p["operationType"] != nil
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
//	A run of capitals ends one character early when a lowercase follows, so
//	"IPProtocol" splits IP|Protocol rather than IPP|rotocol.
//
//	...except when that lowercase is a plural "s" or a version suffix ("v4",
//	"v6"), which belong to the acronym: "internalIPs" is internal|IPs, and
//	"IPv4Range" is IPv4|Range. Without this the first rule reintroduces the
//	bug for exactly the names it was written to fix.
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

// mmIndex indexes ONE level of a magic-modules resource's fields by name, so a
// Discovery property can find its lifecycle flags at the same place in the
// tree. buildLevel descends in step: an object's fields are looked up in the
// matched field's own properties, a list's in its item_type.
//
// It was once a single flat map over every depth, keyed by bare name, and
// first-seen won. That let one field's flags land on every other field of the
// same name anywhere in the resource: the moment list elements were walked,
// authz policy's required ipBlocks[].prefix made every string-match prefix
// required too, and gcp.backendservice's top-level name only came out
// required because a nested "name" stopped shadowing it. magic-modules keeps
// the API's nesting even where it flattens for Terraform (flatten_object is
// Terraform-schema only), so looking up by path loses nothing the flat map
// found honestly.
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
// every lifecycle flag on those. A real field name always wins; an api_name
// only fills a key nothing else claimed (compute's InstanceGroupManager has
// both "id" and an "instanceGroupManagerId" whose api_name is "id"). See
// TestARealFieldNameBeatsAnotherFieldsApiName.
func mmIndex(fields []*mmv1.Field) map[string]*mmv1.Field {
	out := map[string]*mmv1.Field{}
	for _, key := range []func(*mmv1.Field) string{
		func(f *mmv1.Field) string { return f.Name },
		func(f *mmv1.Field) string { return f.ApiName },
	} {
		for _, f := range fields {
			if k := key(f); k != "" {
				if _, seen := out[k]; !seen {
					out[k] = f
				}
			}
		}
	}
	return out
}

// clearNestedRequired clears Required on every attribute below a, not on a.
func clearNestedRequired(a *catalog.Attr) {
	for _, f := range a.Fields {
		f.Required = false
		clearNestedRequired(f)
	}
	if a.Elem != nil {
		a.Elem.Required = false
		clearNestedRequired(a.Elem)
	}
}

// mmChildren is the index for the level below f: an object's own properties,
// or a list's element fields, which magic-modules keeps under item_type. nil
// when magic-modules has no field here, so nothing below inherits a flag from
// a field it does not describe.
func mmChildren(f *mmv1.Field) map[string]*mmv1.Field {
	if f == nil {
		return nil
	}
	if f.ItemType != nil && len(f.ItemType.Properties) > 0 {
		return mmIndex(f.ItemType.Properties)
	}
	return mmIndex(f.Properties)
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
	if mm != nil && mm.ExcludeResource {
		// Terraform implements this resource by hand and never runs its YAML,
		// so nothing has ever enforced the YAML's `required` and it is wrong
		// in places: compute's Instance marks an access config's name
		// required, where the API documents a default ("External NAT"), and
		// storage's Bucket requires `bucket` inside every ACL entry, a field
		// the server fills. Top level is kept: on every such type that ships
		// it is only `name`, which the create url needs.
		for _, a := range out {
			clearNestedRequired(a)
		}
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
		// Only a top-level attribute shares a map with the resource's own
		// keys. Below that, `type` is just a field: renaming it there sent
		// every BigQuery schema field's type to `type_value`.
		if renamed, clash := keywords[name]; clash && topLevel {
			key = renamed
		}
		a := &catalog.Attr{
			Canonical:   name,
			Kind:        KindOf(prop),
			Output:      d.OutputOnly(prop) || isKindConstant(name, prop),
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
		// Immutability has two sources and either is enough. magic-modules'
		// `immutable:` is a human transcription; Discovery's "Immutable." tag is
		// the API's own declaration, carried in prose because Discovery has no
		// field for it. Neither is complete: on 2026-09-23 the prose caught 48
		// settable fields magic-modules had not marked, and magic-modules
		// marks many the API never tags (an artifact registry repository's
		// `format`). So they are ORed, never one in place of the other. A field
		// changed that the API will not change must REPLACE the resource; sent
		// as a patch it is refused, and the plan was wrong before it ran.
		behaviors := disco.Behaviors(prop)
		a.ForceNew = behaviors[disco.BehaviorImmutable]
		// Never on an output-only field: a field GCP sets is returned by
		// definition, and there is nothing of the user's to carry.
		a.InputOnly = behaviors[disco.BehaviorInputOnly] && !a.Output
		if a.Output {
			addSource(a, "output", SourceDiscovery)
		}
		if a.ForceNew {
			addSource(a, "immutable", SourceDiscovery)
		}
		if a.InputOnly {
			addSource(a, "input_only", SourceDiscovery)
		}
		if f := idx[name]; f != nil {
			a.Required = f.Required
			a.ForceNew = a.ForceNew || f.Immutable
			if f.Required {
				addSource(a, "required", SourceMM)
			}
			if f.Immutable {
				addSource(a, "immutable", SourceMM)
			}
			if f.Output {
				addSource(a, "output", SourceMM)
			}
			if f.Sensitive || f.WriteOnly {
				addSource(a, "sensitive", SourceMM)
			}
			if f.IsSet {
				addSource(a, "unordered", SourceMM)
			}
			// Secrets. Discovery has no way to say a field is one, so
			// magic-modules is the only source: `sensitive` on passwords,
			// keys and tokens, `write_only` on values Terraform never keeps.
			// Without this the host prints them in plans and state, in clear.
			a.Sensitive = f.Sensitive || f.WriteOnly
			a.Equivalence = equivalences[f.DiffSuppressFunc]
			// A reference compares as one whatever form it is written in.
			// Terraform's generator gives every ResourceRef field
			// CompareSelfLinkOrResourceName itself, so the YAML never spells it
			// out: a subnetwork's network written as a relative path came back
			// as a full url and planned a replacement (live run, 2026-09-24).
			if a.Equivalence == "" && isResourceRef(f) {
				a.Equivalence = catalog.EquivalenceSelfLink
			}
			if a.Equivalence != "" {
				addSource(a, "equivalence", SourceMM)
			}
			if f.Output {
				a.Output = true
			}
			// Input only has two sources, like immutability. magic-modules'
			// ignore_read marks a field Google never returns, which Discovery
			// often does not say: an SSL certificate's privateKey is required
			// and ForceNew, and without this every plan after a create read
			// it as removed and proposed replacing the certificate. Carrying
			// is safe even where magic-modules is wrong, because a value
			// Google does return is believed (gcprov.carryInputOnly).
			a.InputOnly = (a.InputOnly || f.IgnoreRead) && !a.Output
			if f.IgnoreRead && a.InputOnly {
				addSource(a, "input_only", SourceMM)
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
				// Whoever said output is who overruled it.
				for _, src := range a.Sources["output"] {
					overrule(a, "required", src)
				}
				// The generator's own stderr, not the plugin's: gen-gcp is a
				// build-time tool, so this ends up as a line a human reads in the
				// regeneration output, next to the warnings file. Collected here
				// rather than printed immediately — see BuildAttributes for why.
				*conflicts = append(*conflicts, fmt.Sprintf(
					"gen: %s.%s is required per magic-modules but output-only per Discovery; treating it as output-only\n",
					d.Name, name))
			}
			// Not when the field takes more than one type. magic-modules types
			// a url map's defaultService as a BackendService reference, and
			// its custom_expand (reference_to_backend) is what lets a backend
			// BUCKET through as well, which Google accepts. As a typed
			// reference, infrena refused a url map pointing at a backend
			// bucket before anything was sent (live run, 2026-09-24).
			if topLevel && f.Type == "ResourceRef" && f.Resource != "" && !acceptsSeveralTypes(f) {
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
			fields, err := buildLevel(d, prop, mmChildren(idx[name]), nil, false, conflicts)
			if err != nil {
				return nil, err
			}
			a.Fields = fields
		case prop.Type == "array" && prop.Items != nil:
			// A list of scalars compares element by element, so the list's
			// equivalence is its elements' (a backend service's healthChecks,
			// a list of references).
			elem := &catalog.Attr{Canonical: name, Kind: KindOf(prop.Items), Equivalence: a.Equivalence}
			for _, src := range a.Sources["equivalence"] {
				addSource(elem, "equivalence", src)
			}
			if prop.Items.Type == "object" && len(prop.Items.Properties) > 0 {
				// Same nil-aliases reasoning as the object branch above: no path
				// notation exists to curate an alias for an array element's field.
				fields, err := buildLevel(d, prop.Items, mmChildren(idx[name]), nil, false, conflicts)
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

// acceptsSeveralTypes reports whether a magic-modules ResourceRef takes more
// than the one type it names, which magic-modules says through the expand
// that builds the url: reference_to_backend accepts a backend service or a
// backend bucket.
func acceptsSeveralTypes(f *mmv1.Field) bool {
	return strings.HasSuffix(f.CustomExpand, "/reference_to_backend.tmpl")
}

// equivalences maps a magic-modules diff_suppress_func NAME to the rule
// gcprov applies for it. Only names whose rule is written and tested there
// belong here; an unknown name maps to nothing, which is today's behaviour.
// Measured 2026-09-24: 33 distinct names on 110 fields of shipped types, the
// commonest CompareSelfLinkOrResourceName (31). Each needs its own rule, so
// each is added when one is written, not all at once.
var equivalences = map[string]string{
	// compute's forwarding rules: configuration writes "80" and Google
	// answers "80-80", and portRange is ForceNew, so the rule was replaced
	// on every plan (live run, 2026-09-24).
	"PortRangeDiffSuppress": catalog.EquivalencePortRange,

	// References. Configuration may hold a full url (what ${x.selfLink}
	// resolves to), a relative path or a bare name, and Google answers with
	// its own form; the three names differ only in how much of the path they
	// compare, and the rule here compares the path from "projects/" on.
	"tpgresource.CompareSelfLinkOrResourceName": catalog.EquivalenceSelfLink,
	"tpgresource.CompareSelfLinkRelativePaths":  catalog.EquivalenceSelfLink,
	"tpgresource.CompareSelfLinkCanonicalPaths": catalog.EquivalenceSelfLink,
	"tpgresource.CompareResourceNames":          catalog.EquivalenceResourceName,
	"tpgresource.CaseDiffSuppress":              catalog.EquivalenceCase,
	"tpgresource.DurationDiffSuppress":          catalog.EquivalenceDuration,
}

// isResourceRef reports whether f holds references: a ResourceRef, or an
// Array of them.
func isResourceRef(f *mmv1.Field) bool {
	return f.Type == "ResourceRef" || (f.ItemType != nil && f.ItemType.Type == "ResourceRef")
}

// kindConstantRE is the "service#type" constant an older Google API puts in
// every object's `kind`: dns#policyNetwork, storage#bucket.
var kindConstantRE = regexp.MustCompile(`^[a-z0-9]+#[A-Za-z0-9]+$`)

// isKindConstant reports whether a property is the `kind` constant: the
// type's own name, which Google fills in and nobody configures. 46 schemas
// leave it settable. Settable, it is drift wherever reconciliation cannot
// prune it: Cloud DNS answers every entry of a policy's networks list with
// kind "dns#policyNetwork", and inside an unordered list nothing is pruned,
// so the plan after creating a policy proposed an update (live run,
// 2026-09-24).
func isKindConstant(name string, prop *disco.Schema) bool {
	if name != "kind" || prop == nil {
		return false
	}
	if s, ok := prop.Default.(string); ok && kindConstantRE.MatchString(s) {
		return true
	}
	return regexp.MustCompile(`(?i)identifies what kind|the kind of item this is|^type of (the )?resource`).
		MatchString(strings.TrimSpace(prop.Description))
}
