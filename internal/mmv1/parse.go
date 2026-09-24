// Package mmv1 reads GoogleCloudPlatform/magic-modules resource definitions:
// the lifecycle overlay that supplies what Discovery documents do not.
//
// Only mmv1/products/**/*.yaml is read, and those files are Apache 2.0 by
// per-file header. Nothing under mmv1/third_party/ or tools/ is touched; both
// are MPL 2.0.
package mmv1

import (
	"fmt"

	"gopkg.in/yaml.v3"
)

// Field is one parameter or property, at any depth.
type Field struct {
	Name        string `yaml:"name"`
	Type        string `yaml:"type"`
	Description string `yaml:"description"`
	Required    bool   `yaml:"required"`
	Immutable   bool   `yaml:"immutable"`
	Output      bool   `yaml:"output"`
	Resource    string `yaml:"resource"` // ResourceRef target
	Imports     string `yaml:"imports"`  // which of the target's fields the ref carries
	// IsSet marks a list GCP treats as a SET: it may return the elements in a
	// different order than they were sent. Task 15 reorders those to match the
	// reference; reordering an ordered list would silently rewrite user intent,
	// so this flag is what keeps the two apart. 216 fields in the corpus carry it.
	IsSet bool `yaml:"is_set"`
	// ApiName is the name the API itself uses, when magic-modules calls the
	// field something else: compute's Firewall declares "allow" with
	// api_name "allowed", eventarc's Trigger declares "matchingCriteria"
	// with api_name "eventFilters". Discovery only ever uses the API's name,
	// so without this every flag on such a field -- required, immutable,
	// is_set -- is dropped on the floor when the two sources are joined
	// (internal/gen's mmIndex). 438 fields in the corpus carry one.
	ApiName string `yaml:"api_name"`
	// ItemType is an Array's element: a bare type name ("String") or a whole
	// field, whose properties are the element's fields. It was once decoded
	// as `any` and never read, which dropped every flag on every field
	// inside a list of objects (1,409 of them in the corpus).
	ItemType   *ItemType `yaml:"item_type"`
	Properties []*Field  `yaml:"properties"` // NestedObject and Array-of-NestedObject

	// MinVersion is "beta" on a field only the beta API has. 659 fields.
	MinVersion string `yaml:"min_version"`

	// Sensitive and WriteOnly mark secrets: passwords, keys, tokens.
	// Discovery has no way to say so. 210 and 23 fields.
	Sensitive bool `yaml:"sensitive"`
	WriteOnly bool `yaml:"write_only"`

	// DefaultFromAPI: Google fills the field in when it is not sent, so a
	// value appearing that nobody configured is not drift. 1,499 fields.
	DefaultFromAPI bool `yaml:"default_from_api"`
	// IgnoreRead: "the provider sets the field's value in the resource
	// state based only on the user's configuration" -- the read is not
	// trusted for it. 494 fields.
	IgnoreRead bool `yaml:"ignore_read"`
	// URLParamOnly: the field goes in the URL, never the body. 1,448 fields,
	// nearly all of them parameters.
	URLParamOnly bool `yaml:"url_param_only"`
	// ClientSide: undocumented; by use, Terraform's own setting, never sent
	// to Google. 16 fields.
	ClientSide bool `yaml:"client_side"`
	// SendEmptyValue: "the provider sends 'empty' values to the API if set
	// explicitly in the user's configuration". 621 fields.
	SendEmptyValue bool `yaml:"send_empty_value"`

	// UpdateURL and UpdateVerb, on a field, name the method that changes
	// THAT field when it is not the resource's own update: compute's labels
	// go to setLabels by POST, a forwarding rule's target to setTarget.
	// UpdateID and FingerprintName are undocumented; by use, UpdateID groups
	// fields sent in one call and FingerprintName is the lock that call
	// needs, which is not always the resource's fingerprint.
	// 121 fields carry an UpdateURL.
	UpdateURL       string `yaml:"update_url"`
	UpdateVerb      string `yaml:"update_verb"`
	UpdateID        string `yaml:"update_id"`
	FingerprintName string `yaml:"fingerprint_name"`
	// UpdateMaskFields are the mask paths a change to this field sends, when
	// they are not simply its own name ("streamingConfig.filter"). 53 fields.
	UpdateMaskFields []string `yaml:"update_mask_fields"`

	// CustomExpand and CustomFlatten are per-field templates that reshape
	// the field on the way out and on the way back. Unlike custom_code they
	// are not counted by the tier gate. 446 and 610 fields.
	CustomExpand  string `yaml:"custom_expand"`
	CustomFlatten string `yaml:"custom_flatten"`

	// DiffSuppressFunc names the Go function Terraform uses to call two
	// spellings of the field equal: "80" and "80-80" for a port range. The
	// function is in magic-modules' MPL code and is never copied; its NAME is
	// data, and internal/gen maps the names it knows to rules written here.
	DiffSuppressFunc string `yaml:"diff_suppress_func"`
}

// ItemType is an Array's element type.
type ItemType Field

// UnmarshalYAML accepts both spellings magic-modules uses: a scalar type name,
// or a mapping that is a field in its own right.
func (it *ItemType) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind == yaml.ScalarNode {
		it.Type = node.Value
		return nil
	}
	return node.Decode((*Field)(it))
}

// Async describes how a mutation completes.
type Async struct {
	Type    string   `yaml:"type"`
	Actions []string `yaml:"actions"`
}

// Resource is one magic-modules resource definition.
type Resource struct {
	Name         string               `yaml:"name"`
	Description  string               `yaml:"description"`
	BaseURL      string               `yaml:"base_url"`
	CreateURL    string               `yaml:"create_url"`
	UpdateURL    string               `yaml:"update_url"`
	DeleteURL    string               `yaml:"delete_url"`
	SelfLink     string               `yaml:"self_link"`
	CreateVerb   string               `yaml:"create_verb"`
	UpdateVerb   string               `yaml:"update_verb"`
	UpdateMask   bool                 `yaml:"update_mask"`
	Exclude      bool                 `yaml:"exclude"`
	MinVersion   string               `yaml:"min_version"`
	ImportFormat []string             `yaml:"import_format"`
	Async        *Async               `yaml:"async"`
	Parameters   []*Field             `yaml:"parameters"`
	Properties   []*Field             `yaml:"properties"`
	CustomCode   map[string]yaml.Node `yaml:"custom_code"`

	// Immutable on a resource: "the resource and all its fields are
	// considered immutable", except fields that name their own update_url.
	// compute's TargetHttpsProxy is immutable and changes its certificates
	// through setSslCertificates.
	// 178 resources say true.
	Immutable bool `yaml:"immutable"`
	// ReadVerb (undocumented) is the verb of a read that is not a GET.
	// 9 resources.
	ReadVerb string `yaml:"read_verb"`
	// DeleteVerb is the verb of a delete that is not a DELETE. 71 resources.
	DeleteVerb string `yaml:"delete_verb"`
	// ExcludeDelete: deleting the resource sends nothing; it is only
	// forgotten. ExcludeRead: there is no read. 84 and 7 resources.
	ExcludeDelete bool `yaml:"exclude_delete"`
	ExcludeRead   bool `yaml:"exclude_read"`
	// NestedQuery (undocumented): by use, the resource is not addressable
	// on its own and is read out of a list inside its parent. 31 resources.
	NestedQuery *NestedQuery `yaml:"nested_query"`
	// ReadQueryParams is a query string every read must carry. 5 resources.
	ReadQueryParams string `yaml:"read_query_params"`
	// Mutex names a lock that serialises mutations sharing it, because the
	// API refuses concurrent ones. 87 resources.
	Mutex string `yaml:"mutex"`
	// IDFormat is the id Terraform stores, when it differs from self_link.
	IDFormat string `yaml:"id_format"`
	// ExcludeResource (undocumented): no Terraform resource is generated
	// from this file. The 35 that say so include compute Instance, storage
	// Bucket and container Cluster, which Terraform writes by hand, so the
	// file may be incomplete and a missing hook is not evidence of none.
	ExcludeResource bool `yaml:"exclude_resource"`

	// Product is the directory the file was found in, filled by LoadDir.
	Product string `yaml:"-"`
	// ProductBaseURL is the product's GA base_url from its product.yaml,
	// filled by LoadDir: "https://secretmanager.{{location}}.rep.googleapis.
	// com/v1/" for regional secrets, whose host depends on the location.
	ProductBaseURL string `yaml:"-"`
}

// NestedQuery says where inside the parent's read the resource lives.
type NestedQuery struct {
	Keys          []string `yaml:"keys"`
	IsListOfIDs   bool     `yaml:"is_list_of_ids"`
	ModifyByPatch bool     `yaml:"modify_by_patch"`
}

// ParseResource decodes one resource YAML file.
//
// CustomCode is decoded as map[string]yaml.Node rather than map[string]string:
// most custom_code values are template paths, but some (tgc_ignore_terraform_decoder,
// tgc_ignore_terraform_encoder) are booleans and others (custom_identity) are lists.
// WireHooks only tests key presence, never a value, so the value's shape doesn't
// matter to this package — but decoding it as a fixed scalar type would mean a
// future list- or bool-valued key either breaks parsing or gets silently dropped.
// A resource that IS wire-affecting must never disappear from the map because its
// custom_code value doesn't fit an assumed shape.
func ParseResource(data []byte) (*Resource, error) {
	var r Resource
	if err := yaml.Unmarshal(data, &r); err != nil {
		return nil, fmt.Errorf("magic-modules resource: %w", err)
	}
	if r.Name == "" {
		return nil, fmt.Errorf("magic-modules resource has no name")
	}
	return &r, nil
}
