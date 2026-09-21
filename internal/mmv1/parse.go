// Package mmv1 reads GoogleCloudPlatform/magic-modules resource definitions:
// the lifecycle overlay that supplies what Discovery documents do not.
//
// Only mmv1/products/**/*.yaml is read, and those files are Apache 2.0 by
// per-file header. Nothing under mmv1/third_party/ or tools/ is touched; both
// are MPL 2.0.
package mmv1

import (
	"errors"
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
	Name         string            `yaml:"name"`
	Description  string            `yaml:"description"`
	BaseURL      string            `yaml:"base_url"`
	CreateURL    string            `yaml:"create_url"`
	UpdateURL    string            `yaml:"update_url"`
	DeleteURL    string            `yaml:"delete_url"`
	SelfLink     string            `yaml:"self_link"`
	UpdateVerb   string            `yaml:"update_verb"`
	UpdateMask   bool              `yaml:"update_mask"`
	Exclude      bool              `yaml:"exclude"`
	MinVersion   string            `yaml:"min_version"`
	ImportFormat []string          `yaml:"import_format"`
	Async        *Async            `yaml:"async"`
	Parameters   []*Field          `yaml:"parameters"`
	Properties   []*Field          `yaml:"properties"`
	CustomCode   map[string]string `yaml:"custom_code"`

	// Product is the directory the file was found in, filled by LoadDir.
	Product string `yaml:"-"`
}

// ParseResource decodes one resource YAML file.
func ParseResource(data []byte) (*Resource, error) {
	var r Resource
	if err := yaml.Unmarshal(data, &r); err != nil {
		var typeErr *yaml.TypeError
		if !errors.As(err, &typeErr) {
			return nil, fmt.Errorf("magic-modules resource: %w", err)
		}
		// custom_code occasionally carries a non-string value (e.g.
		// custom_identity, a list of field names used for composite ids, on 4
		// of 942 resources at time of writing). It is not a template path and
		// not a wire hook, so map[string]string can't hold it and doesn't need
		// to. yaml.v3 still resolves every other field correctly around a
		// *yaml.TypeError; recover custom_code by re-decoding it leniently
		// and keeping only the scalar entries, rather than discarding a
		// resource that is otherwise perfectly readable.
		var raw struct {
			CustomCode map[string]yaml.Node `yaml:"custom_code"`
		}
		if rawErr := yaml.Unmarshal(data, &raw); rawErr != nil {
			return nil, fmt.Errorf("magic-modules resource: %w", err)
		}
		cc := map[string]string{}
		for k, v := range raw.CustomCode {
			if v.Kind == yaml.ScalarNode {
				cc[k] = v.Value
			}
		}
		r.CustomCode = cc
	}
	if r.Name == "" {
		return nil, fmt.Errorf("magic-modules resource has no name")
	}
	return &r, nil
}

// LoadDir reads every resource file under root, keyed by product directory.
//
// product.yaml is skipped: it describes the product, not a resource. A file
// that still fails to parse after ParseResource's custom_code tolerance is
// reported (as a named-path error, aborting the walk) rather than silently
// skipped: a naive loader that swallows a parse error and moves on drops a
// type from the catalog without saying so.

func LoadDir(root string) (map[string][]*Resource, error) {
	out := map[string][]*Resource{}
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
			return readErr
		}
		r, parseErr := ParseResource(data)
		if parseErr != nil {
			return fmt.Errorf("%s: %w", path, parseErr)
		}
		r.Product = filepath.Base(filepath.Dir(path))
		out[r.Product] = append(out[r.Product], r)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}
