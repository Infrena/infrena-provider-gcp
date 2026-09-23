// Package catalog is the generated description of every GCP type this plugin
// serves: what infrena is told about it, and what the runtime needs to call it.
//
// catalog.json.gz is GENERATED. Never hand-edit it. Change gen/overlay.yaml or
// the generator, regenerate, and commit the diff like any other reviewed change.
package catalog

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/infrena/infrena/pkg/schema"
	"github.com/infrena/infrena/pkg/value"
)

// AwaitKind is how a mutation on this type completes. Three, because a 25-API
// sample on 2026-09-21 found exactly three and no long tail.
type AwaitKind int

const (
	// AwaitNone: the mutation returns the resource itself (187 methods sampled).
	AwaitNone AwaitKind = iota
	// AwaitComputeOperation: status/targetLink polling via the global, region or
	// zone operations collection's `wait` (300 sampled).
	AwaitComputeOperation
	// AwaitLongRunning: google.longrunning.Operation done/error/response (189).
	AwaitLongRunning
)

// Scope is which location axis the type's URL template carries.
type Scope int

const (
	ScopeGlobal Scope = iota
	ScopeRegional
	ScopeZonal
)

// RefTarget is a declared reference edge, read from magic-modules' ResourceRef,
// never inferred.
type RefTarget struct {
	Type      string `json:"type"`
	Attribute string `json:"attribute"`
}

// Attr is one attribute, at any depth.
type Attr struct {
	Canonical   string           `json:"canonical"`
	Aliases     []string         `json:"aliases,omitempty"`
	Kind        value.Kind       `json:"kind"`
	Required    bool             `json:"required,omitempty"`
	ForceNew    bool             `json:"force_new,omitempty"`
	Output      bool             `json:"output,omitempty"`
	Sensitive   bool             `json:"sensitive,omitempty"`
	Description string           `json:"description,omitempty"`
	Ref         *RefTarget       `json:"ref,omitempty"`
	Fields      map[string]*Attr `json:"fields,omitempty"`
	Elem        *Attr            `json:"elem,omitempty"`
	// Opaque marks a value copied exactly: no key translation, nothing dropped,
	// no reordering. Free-form maps and truncated $ref tails are opaque, and
	// translating their keys would corrupt user data.
	Opaque bool `json:"opaque,omitempty"`
	// Unordered marks a list GCP may return in a different order than it was
	// sent. Task 15 reorders those to match the reference; an ordered list is
	// left alone, because there order carries meaning.
	Unordered bool `json:"unordered,omitempty"`
}

// Type is one resource type.
type Type struct {
	Name        string `json:"name"`
	Service     string `json:"service"`
	Description string `json:"description,omitempty"`

	Tier       int    `json:"tier"`
	TierReason string `json:"tier_reason,omitempty"`

	APIBaseURL string `json:"api_base_url"`

	// PathPrefix is whatever sits between APIBaseURL and the relative
	// resource name -- "v1/" for networksecurity, "dns/v1/" for dns's
	// responsePolicies, "" for compute, storage and bigquery (whose
	// Discovery servicePath already carries the version).
	//
	// It exists because SelfLink cannot carry it. SelfLink is also the
	// provider-id template, and a provider id must name the resource, not
	// the API version that happened to serve it. Before this field existed
	// the two jobs shared one string and 94 of 233 types leaked "v1/" into
	// the ids users see, while the other 139 did not.
	//
	// The absolute URL for a resource is therefore always, with no special
	// cases: APIBaseURL + PathPrefix + expand(template).
	PathPrefix string `json:"path_prefix,omitempty"`

	BaseURL   string `json:"base_url"`
	CreateURL string `json:"create_url,omitempty"`
	UpdateURL string `json:"update_url,omitempty"`
	DeleteURL string `json:"delete_url,omitempty"`
	SelfLink  string `json:"self_link,omitempty"`

	UpdateVerb string `json:"update_verb,omitempty"`
	UpdateMask bool   `json:"update_mask,omitempty"`

	// OperationPollPath is the template for polling a long-running operation,
	// taken VERBATIM from the API's own operations.get method path — e.g.
	// "v1/{+name}". It is stored rather than reconstructed because every
	// Discovery document publishes it and a reconstruction could drift from
	// what the API actually accepts: all 97 longrunning types in the corpus
	// have one (v1/{+name} 72, v2/{+name} 17, v3/{+name} 8), so there is
	// nothing to guess. The {+name} form is reserved expansion — the operation
	// name is a path containing "/" and must not be escaped.
	OperationPollPath string `json:"operation_poll_path,omitempty"`

	// OperationParamPatterns is the regular expression the API publishes for
	// each path parameter of the operation method this type actually polls --
	// keyed by placeholder name, e.g. {"operation": "[a-z](?:[-a-z0-9]{0,61}...)?"}.
	//
	// It is stored so the assertion that an expanded operation url ADDRESSES
	// something can run off committed data. That check previously read
	// schemas/, which is gitignored and fetched by a script, so it SKIPPED in
	// every clean checkout -- and it is the check written to stop a compute
	// operation being polled at a url that expands cleanly and matches nothing
	// (every GKE cluster mutation, fixed in Task 13d). A guard that is written
	// but not armed reads exactly like a guard.
	//
	// Taken from the method whose path is stored above, not from whichever
	// operations method a second search happens to find, so the patterns
	// cannot describe a different method from the template they are checked
	// against.
	//
	// Empty for a type that awaits nothing, and for one whose API publishes no
	// pattern for a parameter -- Discovery omits them more often than not.
	OperationParamPatterns map[string]string `json:"operation_param_patterns,omitempty"`

	Await AwaitKind `json:"await"`
	// OperationWaitPath is the API's own operations wait path for this type's
	// scope, e.g. "projects/{project}/zones/{zone}/operations/{operation}/wait".
	// EMPTY means the API publishes no wait method — container and sqladmin do
	// not — and the operation must be polled with OperationPollPath instead.
	OperationWaitPath string `json:"operation_wait_path,omitempty"`
	TimeoutSeconds    int    `json:"timeout_seconds"`

	// ReadVia names how to read a type with no get method, e.g.
	// "list_by_parent" — set only when a ruling's ReadVia says so (spec G6's
	// tagBindings worked example). Task 16's readByListingParent consults it.
	ReadVia string `json:"read_via,omitempty"`

	ImportFormat string `json:"import_format,omitempty"`
	AssetType    string `json:"asset_type,omitempty"`
	Scope        Scope  `json:"scope"`

	// ParentRoot is the resource-hierarchy root this type's collection hangs
	// off -- the FIRST segment of its Discovery collection path, and so the
	// first segment of every one of its resource names: "projects",
	// "organizations", "folders", "billingAccounts", or "" for a collection
	// rooted at something else.
	//
	// It exists because AssetType is NOT unique. Measured on this catalog,
	// 2026-09-22: 27 of the 191 distinct asset types are shared by two or
	// more of the 233 catalog types, and for 13 of those 27 the sharing
	// types' BaseURL and SelfLink templates are CHARACTER-IDENTICAL --
	// logging's four LogBucket variants are all base_url "{+parent}/buckets",
	// self_link "{+name}", and the three CapabilityConfig variants are all
	// "{{parent}}/capabilityConfigs/{{capability_config_id}}". The parent
	// root is the only thing that tells them apart, and before this field it
	// was recorded nowhere but inside the assigned name's own spelling
	// ("gcp.logging.folder.bucket"), which is not something a runtime should
	// have to parse.
	//
	// Discovery (internal/gcprov/discover.go) uses it to decide which catalog
	// type a Cloud Asset Inventory result belongs to. Without it a
	// folder-scoped log bucket is reported as a project-scoped one, which
	// generates configuration that looks right and cannot apply.
	ParentRoot string `json:"parent_root,omitempty"`

	// ListField is the array-valued property this type's List response
	// carries its results under. There is no universal name across GCP's own
	// APIs to assume instead: a sample of 532 List methods across 25 APIs
	// found 209 distinct field names, and "items" (compute's convention)
	// covers only 24% of them. Empty when the generator found no List method
	// or no usable array property to name.
	ListField string `json:"list_field,omitempty"`

	// CreateBindings resolves the create template's placeholders that name
	// neither an instance scope setting nor one of this type's own top-level
	// attributes. Keyed by the placeholder's bare name (no "+", no "%").
	// Empty for the great majority of types, whose placeholders need nothing
	// resolved.
	CreateBindings map[string]*CreateBinding `json:"create_bindings,omitempty"`

	Attributes map[string]*Attr `json:"attributes"`
}

// Catalog is the whole generated set.
type Catalog struct {
	Generated  string  `json:"generated"`
	MMV1Commit string  `json:"mmv1_commit"`
	Types      []*Type `json:"types"`

	// DiscoverDefault is the type list discovery's per-type fallback scans
	// when neither the request nor the instance names any -- gen/overlay.yaml's
	// own discover_default, carried here because the overlay is a
	// generator-time input and the runtime has no other way to read it.
	//
	// It matters ONLY on the fallback path. Cloud Asset Inventory answers for
	// a whole project in a handful of calls and needs no list at all; this is
	// what discovery falls back to when CAI is unavailable, which is why it
	// is short rather than exhaustive.
	DiscoverDefault []string `json:"discover_default,omitempty"`

	byName map[string]*Type
}

// Encode writes a catalog as gzipped JSON.
func Encode(c *Catalog) ([]byte, error) {
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	enc := json.NewEncoder(zw)
	enc.SetIndent("", " ")
	if err := enc.Encode(c); err != nil {
		return nil, fmt.Errorf("catalog: %w", err)
	}
	if err := zw.Close(); err != nil {
		return nil, fmt.Errorf("catalog: %w", err)
	}
	return buf.Bytes(), nil
}

// Decode reads one back.
func Decode(blob []byte) (*Catalog, error) {
	zr, err := gzip.NewReader(bytes.NewReader(blob))
	if err != nil {
		return nil, fmt.Errorf("catalog: %w", err)
	}
	defer zr.Close()
	data, err := io.ReadAll(zr)
	if err != nil {
		return nil, fmt.Errorf("catalog: %w", err)
	}
	var c Catalog
	if err := json.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("catalog: %w", err)
	}
	c.index()
	return &c, nil
}

func (c *Catalog) index() {
	c.byName = make(map[string]*Type, len(c.Types))
	for _, t := range c.Types {
		c.byName[t.Name] = t
	}
}

// Type looks one up by infrena type name.
func (c *Catalog) Type(name string) (*Type, bool) {
	if c.byName == nil {
		c.index()
	}
	t, ok := c.byName[name]
	return t, ok
}

// Definitions converts the catalog into what the host is told.
//
// Only tier 1 and ruled tier-2 types are in the catalog at all (the generator
// refuses the rest), so there is no filtering here: a type that reached the
// catalog is one this plugin serves.
func (c *Catalog) Definitions() []*schema.ResourceDefinition {
	defs := make([]*schema.ResourceDefinition, 0, len(c.Types))
	for _, t := range c.Types {
		d := &schema.ResourceDefinition{
			Type:        t.Name,
			Description: t.Description,
			Attributes:  make(map[string]schema.Attribute, len(t.Attributes)),
			Capabilities: schema.Capabilities{
				// A url is not enough. A create url whose placeholders
				// nothing can fill is a create that fails on string
				// substitution, before a byte reaches Google -- 38 types
				// shipped claiming a create they could not perform, and the
				// live suite found exactly one of them because a live suite
				// finds what it exercises. `infrena explain` is what a user
				// reads to find out what a type can do, so this is where the
				// answer has to be true.
				Create: len(t.UnresolvedCreatePlaceholders()) == 0,
				Read:   true,
				Update: t.UpdateVerb != "",
				Delete: true,
				Import: t.ImportFormat != "",
			},
			ImportID: schema.ImportSpec{Description: importDescription(t.ImportFormat)},
		}
		for name, a := range t.Attributes {
			d.Attributes[name] = a.toSchema(true)
		}
		defs = append(defs, d)
	}
	sort.Slice(defs, func(i, j int) bool { return defs[i].Type < defs[j].Type })
	return defs
}

// importDescription renders an import_format for `infrena explain`.
// magic-modules gives 11 of the 233 types more than one accepted id shape,
// newline-joined (e.g. the full relative name plus a short "{{name}}" form) --
// ParseProviderID (internal/gcprov/ids.go) tries each in order, and a user
// reading `infrena explain` needs to see every one of them too, not a blob
// with raw newlines in it.
func importDescription(format string) string {
	lines := strings.Split(format, "\n")
	for i, l := range lines {
		lines[i] = strings.TrimSpace(l)
	}
	return strings.Join(lines, " or ")
}

// toSchema converts one attribute, at any depth. topLevel is true only for an
// attribute directly on a Type, never for one reached through Fields.
func (a *Attr) toSchema(topLevel bool) schema.Attribute {
	out := schema.Attribute{
		Kind:        a.Kind,
		Required:    a.Required,
		ForceNew:    a.ForceNew,
		Sensitive:   a.Sensitive,
		Description: a.Description,
		Aliases:     a.Aliases,
	}
	switch {
	case a.Output:
		// GCP chooses it. Computed and not Optional: configuration may not set it.
		out.Computed = true
		// And NOT Required, whatever the source said. Required+Computed is a
		// combination schema.Validate refuses outright, so leaving it set would
		// fail the whole catalog at load with an error naming only the attribute,
		// not the fact that two independent sources disagreed about it. Task 7's
		// generator already resolves that disagreement in favour of Output and
		// names the field on stderr; this is the belt to its braces, because a
		// conversion should not be able to emit what the host refuses.
		out.Required = false
	case a.Required:
		// project, region, name and friends. Required wins; Computed would let a
		// plan proceed without one.
	default:
		// Spec §4.3: every settable property is Optional+Computed, so an attribute
		// dropped from configuration keeps GCP's value rather than planning a change.
		out.Optional = true
		out.Computed = true
	}
	// schema.ValidateAll refuses a nested References outright ("only a top-level
	// attribute's References is ever projected into a dependency") because the
	// host never consults one at any other depth. Emitting it anyway would not
	// lose a usable reference edge — it was never usable — it would just make
	// the whole catalog fail to load. Keep this guard even if a future source
	// stops populating nested Refs: the conversion should not be able to emit
	// what the host refuses.
	if a.Ref != nil && topLevel {
		out.References = &schema.Reference{Type: a.Ref.Type, Attribute: a.Ref.Attribute}
	}
	if len(a.Fields) > 0 {
		out.Fields = make(map[string]schema.Attribute, len(a.Fields))
		for n, f := range a.Fields {
			out.Fields[n] = f.toSchema(false)
		}
	}
	// Elem describes what each element of a list looks like, and it is the only
	// way to say it: Fields is "this map's known keys", which a list is not.
	//
	// Needs protocol 6. An older host ignores the key, and what it ignores is
	// 1901 elements carrying 5841 of this catalog's aliases -- so on protocol 5
	// a user writing the snake_case spelling this generator advertises inside a
	// repeated block had it reach GCP verbatim, with no canonicalisation, no
	// collision check and no "has no key". That is why the floor moves in the
	// same commit as this.
	//
	// The ordinary conversion is reused deliberately. An element is not
	// configured independently of the list holding it, so Required, Optional and
	// Default mean nothing on one -- but they are IGNORED rather than refused,
	// exactly as they already are on a map's nested keys, so there is no special
	// branch here to drift out of step with the one above.
	if a.Elem != nil {
		elem := a.Elem.toSchema(false)
		out.Elem = &elem
	}
	return out
}

// CreateBinding says where one create-url placeholder's value comes from when
// the placeholder does not simply name one of the type's own top-level
// attributes.
//
// It is RESOLVED AT GENERATION TIME and stored, rather than searched for on
// every request, for the same reason PathPrefix is (see Type.PathPrefix): a
// runtime search re-answers the same question on every call and could answer
// it differently as the attribute tree changes. Exactly one field is set.
type CreateBinding struct {
	// Attr is a dotted path into the resource's own attributes, e.g.
	// "tableReference.datasetId" for gcp.bigquery.table's "{{dataset_id}}".
	// A url template placeholder is not always a top-level attribute: a
	// magic-modules template is written in snake_case against fields that
	// Discovery spells in camelCase and sometimes NESTS one level down, so
	// the value a create url needs can sit anywhere in the tree.
	Attr string `json:"attr,omitempty"`

	// Template is a url sub-template the placeholder expands to, e.g.
	// "projects/{project}" for iam's "{+name}" in
	// "v1/{+name}/serviceAccounts". Discovery publishes a `pattern` for each
	// path parameter, and one placeholder spelling can mean two different
	// things in one collection -- iam's serviceAccounts.create says
	// `^projects/[^/]+$` for "name" while the same collection's get says
	// `^projects/[^/]+/serviceAccounts/[^/]+$`. The pattern is the only
	// thing that tells them apart, so the binding comes from the pattern
	// rather than from an attribute that happens to share the placeholder's
	// name.
	Template string `json:"template,omitempty"`
}

// createScopeSettings are the placeholder names a provider instance's own
// settings fill in for every type, whether or not the type declares them as
// attributes -- see gcprov.Provider.withScope, which is the code that
// actually supplies them. Only these four: nothing in the plugin's
// configuration supplies a `parent`, which is why a type whose create url
// needs one must bind it (from a Discovery pattern) or not ship.
var createScopeSettings = map[string]bool{
	"project": true, "region": true, "zone": true, "location": true,
}

// IsCreateScopeSetting reports whether a url placeholder is one a provider
// instance's own settings fill in. Exported so the generator gate and this
// package's own invariant ask the SAME question the runtime answers -- one
// rule, three readers, rather than three copies that can drift.
func IsCreateScopeSetting(name string) bool { return createScopeSettings[name] }

// CreateTemplate is the url template a create POSTs to: create_url when the
// type has one, base_url otherwise. The runtime and the generator must agree
// about which collection a create goes to -- the provider id it stores is
// checked against it -- so there is one answer, here.
func (t *Type) CreateTemplate() string {
	if t.CreateURL != "" {
		return t.CreateURL
	}
	return t.BaseURL
}

// UnresolvedCreatePlaceholders names every placeholder in this type's create
// template that nothing can fill: not an instance scope setting, not a
// stored CreateBinding, and not one of the type's own SETTABLE attributes.
// It returns them sorted, and nil for a type that can be created.
//
// "Settable" is the part that is easy to leave out and cannot be. An
// attribute the generator marked Output becomes Computed-without-Optional in
// the schema, and infrena refuses configuration that sets one
// ("configuration may not set a computed attribute"). A placeholder bound to
// one is bound to a value no user can ever supply, so the create url can
// never be built -- measured on 2026-09-23: 6 of the 233 types bind their
// new resource's own id to an Output `name` and would otherwise pass a check
// that only asked whether the attribute EXISTS.
func (t *Type) UnresolvedCreatePlaceholders() []string {
	seen := map[string]bool{}
	var out []string
	// expanding holds the placeholders whose own binding template is
	// currently being walked. A pattern-derived template can reintroduce the
	// very name it resolves -- spanner's "{+database}" binds to
	// "projects/{project}/instances/{instance}/databases/{database}", whose
	// last segment is named after the "databases" in front of it -- and
	// without this the inner "{database}" would resolve through the same
	// binding again and the type would look as though it needed nothing.
	// A placeholder may not be its own answer.
	expanding := map[string]bool{}
	var check func(tmpl string)
	check = func(tmpl string) {
		for _, name := range urlPlaceholders(tmpl) {
			if createScopeSettings[name] {
				continue
			}
			if b := t.CreateBindings[name]; b != nil && !expanding[name] {
				if b.Template != "" {
					expanding[name] = true
					check(b.Template)
					delete(expanding, name)
				}
				continue
			}
			if a := t.TopLevelAttr(name); a != nil && !a.Output {
				continue
			}
			if !seen[name] {
				seen[name] = true
				out = append(out, name)
			}
		}
	}
	check(t.CreateTemplate())
	sort.Strings(out)
	return out
}

// TopLevelAttr finds the type's own attribute for a url placeholder: the one
// keyed by that name, or the one whose wire name is that name (a url
// template taken from a Discovery document spells a placeholder the way the
// API does, not the way the schema key does -- see gcprov.wireAliases).
func (t *Type) TopLevelAttr(name string) *Attr {
	if a, ok := t.Attributes[name]; ok {
		return a
	}
	for _, a := range t.Attributes {
		if a.Canonical == name {
			return a
		}
	}
	return nil
}

// urlPlaceholders returns the placeholder names in a url template, in order,
// in either spelling the catalog uses -- magic-modules' "{{x}}" or a
// Discovery path's "{x}" -- with RFC 6570's reserved-expansion "+" and
// magic-modules' "escape me" "%" stripped, exactly as gcprov.ExpandURL
// strips them.
func urlPlaceholders(tmpl string) []string {
	var names []string
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
		if rel == -1 {
			break
		}
		content := strings.TrimSpace(tmpl[i+openLen : i+openLen+rel])
		content = strings.TrimPrefix(content, "+")
		content = strings.TrimPrefix(content, "%")
		names = append(names, content)
		i += openLen + rel + len(closeSeq)
	}
	return names
}
