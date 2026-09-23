// Package disco reads Google API Discovery documents: the authoritative,
// always-current schema source for every GCP API.
//
// It knows nothing about infrena, magic-modules or the catalog. Its only job is
// to turn one Discovery document into types the generator can walk.
package disco

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Document is one API's Discovery document.
type Document struct {
	Name        string               `json:"name"`
	Version     string               `json:"version"`
	RootURL     string               `json:"rootUrl"`
	ServicePath string               `json:"servicePath"`
	Schemas     map[string]*Schema   `json:"schemas"`
	Resources   map[string]*Resource `json:"resources"`
}

// ResolvedBaseURL is where a method's path is joined onto.
func (d *Document) ResolvedBaseURL() string { return d.RootURL + d.ServicePath }

// Schema is one type, or one property of one type. Discovery reuses the same
// shape for both, and so does this.
type Schema struct {
	ID                   string             `json:"id"`
	Type                 string             `json:"type"`
	Format               string             `json:"format"`
	Description          string             `json:"description"`
	Ref                  string             `json:"$ref"`
	ReadOnly             bool               `json:"readOnly"`
	Enum                 []string           `json:"enum"`
	Required             []string           `json:"required"`
	Properties           map[string]*Schema `json:"properties"`
	Items                *Schema            `json:"items"`
	AdditionalProperties *Schema            `json:"additionalProperties"`
}

// Ref is a `{"$ref": "Name"}` pointer.
type Ref struct {
	Ref string `json:"$ref"`
}

// Parameter is one method parameter.
type Parameter struct {
	Type     string `json:"type"`
	Location string `json:"location"`
	Required bool   `json:"required"`
	// Pattern is the regular expression Discovery publishes for a path
	// parameter's value, e.g. "^projects/[^/]+$". It is the ONLY thing that
	// tells two identically spelled placeholders apart when one collection
	// uses one spelling for two meanings: iam's serviceAccounts.create says
	// `^projects/[^/]+$` for "name" while the same collection's get says
	// `^projects/[^/]+/serviceAccounts/[^/]+$`. Empty for most parameters,
	// and for every query parameter.
	Pattern string `json:"pattern"`
	// Description is the API's own prose for the parameter, which is the
	// only description a url parameter has: it is not a body property, so
	// nothing in the request schema documents it.
	Description string `json:"description"`
}

// Method is one API method.
type Method struct {
	ID          string                `json:"id"`
	Path        string                `json:"path"`
	HTTPMethod  string                `json:"httpMethod"`
	Description string                `json:"description"`
	Request     *Ref                  `json:"request"`
	Response    *Ref                  `json:"response"`
	Parameters  map[string]*Parameter `json:"parameters"`
}

// Resource is one node of the Discovery `resources` tree.
type Resource struct {
	Methods   map[string]*Method   `json:"methods"`
	Resources map[string]*Resource `json:"resources"`
}

// Parse decodes one Discovery document.
func Parse(data []byte) (*Document, error) {
	var d Document
	if err := json.Unmarshal(data, &d); err != nil {
		return nil, fmt.Errorf("discovery document: %w", err)
	}
	if d.Name == "" {
		return nil, fmt.Errorf("discovery document has no name")
	}
	return &d, nil
}

// OutputOnly reports whether GCP, not the user, sets this property.
//
// It unions two signals because neither is sufficient on its own: compute
// carries 1,520 `readOnly` flags but 2,202 `[Output Only]` prose markers
// (2,202 of 5,177 top-level schema properties match the marker as a
// description prefix, against 1,520 carrying the flag, at compute revision
// 20260910), and the modern APIs use the flag. Trusting either alone marks
// settable properties read-only, or read-only ones settable.
func (d *Document) OutputOnly(s *Schema) bool {
	if s == nil {
		return false
	}
	if s.ReadOnly {
		return true
	}
	desc := strings.TrimSpace(s.Description)
	lower := strings.ToLower(desc)
	return strings.HasPrefix(lower, "[output only]") || strings.HasPrefix(lower, "output only.")
}
