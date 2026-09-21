// Package mmv1 reads GoogleCloudPlatform/magic-modules resource definitions:
// the lifecycle overlay that supplies what Discovery documents do not.
//
// Only mmv1/products/**/*.yaml is read, and those files are Apache 2.0 by
// per-file header. Nothing under mmv1/third_party/ or tools/ is touched; both
// are MPL 2.0.
package mmv1

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// Field is one parameter or property, at any depth.
type Field struct {
	Name        string   `yaml:"name"`
	Type        string   `yaml:"type"`
	Description string   `yaml:"description"`
	Required    bool     `yaml:"required"`
	Immutable   bool     `yaml:"immutable"`
	Output      bool     `yaml:"output"`
	Resource    string   `yaml:"resource"`   // ResourceRef target
	Imports     string   `yaml:"imports"`    // which of the target's fields the ref carries
	ItemType    any      `yaml:"item_type"`  // string, or a nested mapping for Array of objects
	Properties  []*Field `yaml:"properties"` // NestedObject and Array-of-NestedObject
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

// LoadError names one file that failed to parse, and why.
type LoadError struct {
	Path string
	Err  error
}

func (e LoadError) Error() string { return fmt.Sprintf("%s: %v", e.Path, e.Err) }

func (e LoadError) Unwrap() error { return e.Err }

// LoadDir reads every resource file under root, keyed by product directory.
//
// product.yaml is skipped: it describes the product, not a resource. A file
// that fails to read OR to parse is named in the returned []LoadError rather
// than silently skipped or allowed to abort the rest of the load. Collecting
// rather than aborting matters even beyond not hiding the failure: WalkDir
// visits files in a fixed (alphabetical) order, so aborting on the first bad
// file would silently skip every product that sorts after it — a far bigger,
// and much less visible, loss than naming the one bad file and moving on. The
// returned error is reserved for failures that mean the walk itself didn't
// happen at all, such as an unreadable root directory.
func LoadDir(root string) (map[string][]*Resource, []LoadError, error) {
	out := map[string][]*Resource{}
	var loadErrs []LoadError
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".yaml") {
			return nil
		}
		if filepath.Base(path) == "product.yaml" {
			return nil
		}
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			loadErrs = append(loadErrs, LoadError{Path: path, Err: readErr})
			return nil
		}
		r, parseErr := ParseResource(data)
		if parseErr != nil {
			loadErrs = append(loadErrs, LoadError{Path: path, Err: parseErr})
			return nil
		}
		r.Product = filepath.Base(filepath.Dir(path))
		out[r.Product] = append(out[r.Product], r)
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	return out, loadErrs, nil
}
