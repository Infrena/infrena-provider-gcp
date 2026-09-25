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
	"sync"

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
	// Sources says which inputs decided each of this attribute's facts
	// (gen/facts.go, docs/FACTS.md). The generator's alone: never
	// serialised, so neither the runtime nor the embedded catalog sees it.
	Sources map[string][]string `json:"-"`

	Canonical string     `json:"canonical"`
	Aliases   []string   `json:"aliases,omitempty"`
	Kind      value.Kind `json:"kind"`
	Required  bool       `json:"required,omitempty"`
	ForceNew  bool       `json:"force_new,omitempty"`
	Output    bool       `json:"output,omitempty"`
	Sensitive bool       `json:"sensitive,omitempty"`
	// Equivalence names a rule by which two different spellings of this
	// field's value are the same value, so that Google's canonical answer is
	// not drift against what configuration wrote: "port_range" says "80" and
	// "80-80" are one range. gcprov's reconciler applies it.
	Equivalence string           `json:"equivalence,omitempty"`
	Description string           `json:"description,omitempty"`
	Ref         *RefTarget       `json:"ref,omitempty"`
	Fields      map[string]*Attr `json:"fields,omitempty"`
	Elem        *Attr            `json:"elem,omitempty"`
	// Opaque marks a value copied exactly: no key translation, nothing dropped,
	// no reordering. Free-form maps and truncated $ref tails are opaque, and
	// translating their keys would corrupt user data.
	Opaque bool `json:"opaque,omitempty"`
	// CreateOnly marks an attribute that exists only in the create request and
	// is not part of the resource: accountId on a service account, roleId on a
	// role. A read never returns one, so the runtime carries it forward from
	// prior state rather than expecting GCP to echo it back, and it is
	// ForceNew because nothing can change it afterwards.
	CreateOnly bool `json:"create_only,omitempty"`
	// InputOnly marks a field of the RESOURCE that the API accepts and never
	// returns: compute's disks[].initializeParams, a certificate's private
	// key, the tag bindings a resource is created with. Discovery says so in
	// prose ("Input only." / "[Input Only]"), never as structure.
	//
	// It is not CreateOnly. A create-only attribute is not part of the
	// resource at all and travels beside a create wrapper; an input-only one
	// is a resource field and travels inside it, and may be patchable.
	// What they share is the read side: nothing comes back to compare, so the
	// reconciler carries the value forward from the reference rather than
	// reporting it missing -- which the next plan would read as drift, for
	// ever, on a resource that is exactly as configured.
	InputOnly bool `json:"input_only,omitempty"`
	// SendWithUpdate marks an input-only REQUEST OPTION that goes in the body
	// of every update, changed or not, and never in the mask: an Artifact
	// Registry repository's disableUpstreamValidation. A patch carries only
	// what changed, so without this the option was left out of every update
	// after the create, and Google validated credentials the user had asked
	// it not to (live, 2026-09-25). Only a ruling sets it.
	SendWithUpdate bool `json:"send_with_update,omitempty"`

	// Unordered marks a list GCP may return in a different order than it was
	// sent. Task 15 reorders those to match the reference; an ordered list is
	// left alone, because there order carries meaning.
	Unordered bool `json:"unordered,omitempty"`
}

// Type is one resource type.
type Type struct {
	// Sources is Attr.Sources for the type's own facts.
	Sources map[string][]string `json:"-"`

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

	// CreateWrapper names the create request field that carries the resource,
	// for the eleven types whose API uses the AIP CreateXRequest shape --
	// "serviceAccount" on gcp.serviceaccount, "cluster" on
	// gcp.container.cluster. Empty for the other 216, whose create body IS the
	// resource.
	//
	// Attributes always describe the RESOURCE. This field is how the runtime
	// puts them back in the shape the create call wants, without the schema
	// having to lie about what a read returns. See gen.resourceSchema.
	CreateWrapper string `json:"create_wrapper,omitempty"`

	// UpdateWrapper is the same idea on the update side, and it is NOT a
	// variant spelling of the same body: pubsub's topics.patch takes
	// UpdateTopicRequest{topic, updateMask}, so the resource is wrapped AND
	// the field mask moves off the query string into the body. A type with an
	// UpdateWrapper therefore never carries an updateMask query parameter --
	// UpdateMask is false for all of them -- and the mask goes into
	// UpdateMaskField instead.
	//
	// Eight collections in the pinned documents have this shape: the three
	// pubsub types, three spanner types, cloudasset feeds and iam
	// serviceAccounts. Sending the bare resource to any of them is rejected.
	UpdateWrapper string `json:"update_wrapper,omitempty"`
	// UpdateMaskField is the name of the mask field INSIDE an update wrapper.
	// It is read from the request schema rather than assumed, because it is
	// not always the same word: six of the eight say "updateMask" and
	// spanner's instances and instancePartitions say "fieldMask".
	UpdateMaskField string `json:"update_mask_field,omitempty"`
	// LockField names the top-level field the API requires, up to date, in
	// every update: compute's optimistic-locking fingerprint. "An up-to-date
	// fingerprint must be provided in order to update the Subnetwork,
	// otherwise the request will fail with error 412 conditionNotMet." A user
	// never writes one, so it is never in the diff; Update copies the current
	// value from the observation it was handed instead. Measured on
	// 2026-09-23: 21 types carry one, 19 of them updatable.
	LockField string `json:"lock_field,omitempty"`

	// Setters change some fields in place through a method of their own
	// rather than the resource's update: compute's setLabels, setUrlMap,
	// setSecurityPolicy. magic-modules names them per field (update_url on a
	// property); the generator admits one only where Discovery publishes the
	// same method on the resource's own address and its request carries
	// exactly those fields, plus a lock. A type with setters is updatable
	// even with no UpdateVerb, and the fields they carry never go into a
	// patch.
	Setters []Setter `json:"setters,omitempty"`

	// EndpointTemplate is the host this type is reached through when it is
	// not APIBaseURL's, with "{location}" to be filled from the resource's
	// own path: "https://secretmanager.{location}.rep.googleapis.com/".
	// Regional secrets exist only at their region's endpoint. From
	// magic-modules' product base_url, checked against every endpoint the
	// Discovery document lists.
	EndpointTemplate string `json:"endpoint_template,omitempty"`

	// ClearBeforeDelete are fields Delete patches to empty before deleting,
	// because the API refuses to delete the resource while they are set.
	// From a ruling, which cites the hook that does the same.
	ClearBeforeDelete []string `json:"clear_before_delete,omitempty"`

	// PatchOneField says the API refuses a patch changing more than one
	// top-level field, so Update sends one patch per changed field, reading
	// the lock afresh between them. From the overlay, on evidence.
	PatchOneField bool `json:"patch_one_field,omitempty"`

	// CreateVerb is the HTTP method a create is sent with. Empty means POST,
	// which is 355 of the 358 create methods in the pinned documents. The other
	// three are Pub/Sub's topics, subscriptions and snapshots, which create
	// with a PUT to the new resource's OWN path: a POST there is refused, and
	// gcp.pubsub.snapshot shipped doing exactly that until 2026-09-23.
	CreateVerb string `json:"create_verb,omitempty"`

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
	// DeleteAwait is how a delete completes, when that differs from Await.
	// Await is read from the CREATE method's response and a delete's can be
	// different: sqladmin's sslCerts insert answers with the resource and
	// its delete with an operation, so a delete that reused Await returned
	// as soon as Google accepted it -- and infrena drops a replaced object's
	// Deposed record on that success, the only handle on it. Eight
	// Bigtable, Spanner and KMS types go the other way and reported a
	// failure for a delete that worked. Nil means "the same as Await";
	// DeleteAwaitKind is how to read it.
	DeleteAwait *AwaitKind `json:"delete_await,omitempty"`
	// UpdateAwait is how an update completes, when that differs from Await,
	// for the same reason as DeleteAwait. Artifact Registry's create answers
	// with an operation and its patch with the repository itself; awaited as
	// an operation, the repository's own name was polled for a `done` it
	// never has until the type's twenty-minute timeout (live, 2026-09-25).
	// Nil means "the same as Await"; UpdateAwaitKind is how to read it.
	UpdateAwait *AwaitKind `json:"update_await,omitempty"`
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

	byName    map[string]*Type
	indexOnce sync.Once
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
	c.indexOnce.Do(c.index)
	return &c, nil
}

// index builds the name lookup. Call it through indexOnce, never directly.
func (c *Catalog) index() {
	byName := make(map[string]*Type, len(c.Types))
	for _, t := range c.Types {
		byName[t.Name] = t
	}
	// Assigned whole, after it is fully built. A reader that sees the field
	// non-nil sees a complete map, never one still filling.
	c.byName = byName
}

// Type looks one up by infrena type name.
// THE INDEX IS BUILT ONCE, UNDER A LOCK, BECAUSE READS ARE CONCURRENT.
//
// This used to be `if c.byName == nil { c.index() }`, which is safe only for a
// Catalog that arrived already indexed. Decode does index eagerly -- but
// gcpplugin's redirect (the endpoint override) builds a Catalog STRUCT LITERAL
// from the shared one, and a literal has a nil byName. The provider then serves
// a plan's refreshes concurrently, two goroutines both saw nil, both rebuilt the
// map, and one replaced it while another was reading: a lookup missed a type
// that exists.
//
// It failed about one run in six and presented as `unknown type "gcp.urlmap"`
// for a type plainly in the catalog. CI caught it on its second day; six local
// runs under four different configurations had not.
func (c *Catalog) Type(name string) (*Type, bool) {
	c.indexOnce.Do(c.index)
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
				Update: t.UpdateVerb != "" || len(t.Setters) > 0,
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

// Setter is one method that changes some of a resource's fields in place.
type Setter struct {
	// Method is the method's name, for messages: "setLabels".
	Method string `json:"method"`
	// Path is the method's own url template, spelled with the resource's
	// self_link placeholders so the resource's id fills it. Not self_link
	// plus the method: compute's global target proxies are read at
	// projects/{project}/global/targetHttpsProxies/{x} and their setUrlMap
	// lives at projects/{project}/targetHttpsProxies/{x}/setUrlMap.
	Path string `json:"path"`
	Verb string `json:"verb"`
	// Fields are the wire names of the attributes the request carries. A
	// change to any of them calls the method, with all of them in the body.
	Fields []string `json:"fields"`
	// Lock is a request property copied from the current state rather than
	// from configuration: compute's labelFingerprint.
	Lock string `json:"lock,omitempty"`
}

// SetterFor is the setter that carries the attribute with wire name
// canonical, or nil when its update, if any, is the resource's own.
func (t *Type) SetterFor(canonical string) *Setter {
	for i := range t.Setters {
		for _, f := range t.Setters[i].Fields {
			if f == canonical {
				return &t.Setters[i]
			}
		}
	}
	return nil
}

// UpdateAwaitKind is how this type's update completes.
func (t *Type) UpdateAwaitKind() AwaitKind {
	if t.UpdateAwait != nil {
		return *t.UpdateAwait
	}
	return t.Await
}

// DeleteAwaitKind is how this type's delete completes.
func (t *Type) DeleteAwaitKind() AwaitKind {
	if t.DeleteAwait != nil {
		return *t.DeleteAwait
	}
	return t.Await
}

// The equivalence rules gcprov knows. Each is written here from what its
// magic-modules name says it does; none is copied.
const (
	// EquivalencePortRange: a single port and the one-port range it names
	// are the same value, "80" and "80-80".
	EquivalencePortRange = "port_range"
	// EquivalenceSelfLink: two references to the same resource, however much
	// of the url each carries -- "https://.../compute/v1/projects/p/global/
	// networks/n", "projects/p/global/networks/n" -- or a bare name and a
	// path ending in it.
	EquivalenceSelfLink = "self_link"
	// EquivalenceResourceName: the same last path segment.
	EquivalenceResourceName = "resource_name"
	// EquivalenceCase: equal but for case ("TCP" and "tcp").
	EquivalenceCase = "case"
	// EquivalenceDuration: the same length of time ("10s" and "10.000s").
	EquivalenceDuration = "duration"
)
