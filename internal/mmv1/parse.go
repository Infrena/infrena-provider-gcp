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
	ApiName    string   `yaml:"api_name"`
	ItemType   any      `yaml:"item_type"`  // string, or a nested mapping for Array of objects
	Properties []*Field `yaml:"properties"` // NestedObject and Array-of-NestedObject
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
	UpdateVerb   string               `yaml:"update_verb"`
	UpdateMask   bool                 `yaml:"update_mask"`
	Exclude      bool                 `yaml:"exclude"`
	MinVersion   string               `yaml:"min_version"`
	ImportFormat []string             `yaml:"import_format"`
	Async        *Async               `yaml:"async"`
	Parameters   []*Field             `yaml:"parameters"`
	Properties   []*Field             `yaml:"properties"`
	CustomCode   map[string]yaml.Node `yaml:"custom_code"`

	// Product is the directory the file was found in, filled by LoadDir.
	Product string `yaml:"-"`
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
