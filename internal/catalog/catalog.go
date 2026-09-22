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
	BaseURL    string `json:"base_url"`
	CreateURL  string `json:"create_url,omitempty"`
	UpdateURL  string `json:"update_url,omitempty"`
	DeleteURL  string `json:"delete_url,omitempty"`
	SelfLink   string `json:"self_link,omitempty"`

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

	// ListField is the array-valued property this type's List response
	// carries its results under. There is no universal name across GCP's own
	// APIs to assume instead: a sample of 532 List methods across 25 APIs
	// found 209 distinct field names, and "items" (compute's convention)
	// covers only 24% of them. Empty when the generator found no List method
	// or no usable array property to name.
	ListField string `json:"list_field,omitempty"`

	Attributes map[string]*Attr `json:"attributes"`
}

// Catalog is the whole generated set.
type Catalog struct {
	Generated  string  `json:"generated"`
	MMV1Commit string  `json:"mmv1_commit"`
	Types      []*Type `json:"types"`

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
				Create: t.CreateURL != "" || t.BaseURL != "",
				Read:   true,
				Update: t.UpdateVerb != "",
				Delete: true,
				Import: t.ImportFormat != "",
			},
			ImportID: schema.ImportSpec{Description: t.ImportFormat},
		}
		for name, a := range t.Attributes {
			d.Attributes[name] = a.toSchema(true)
		}
		defs = append(defs, d)
	}
	sort.Slice(defs, func(i, j int) bool { return defs[i].Type < defs[j].Type })
	return defs
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
	return out
}
