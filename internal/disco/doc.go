// Package disco reads Google API Discovery documents: the authoritative,
// always-current schema source for every GCP API.
//
// It knows nothing about infrena, magic-modules or the catalog. Its only job is
// to turn one Discovery document into types the generator can walk.
package disco

import (
	"encoding/json"
	"fmt"
	"regexp"
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
	// Endpoints are the API's other hosts: Secret Manager publishes one
	// regional endpoint per location, secretmanager.<location>.rep.
	// googleapis.com, which its regional secrets must be reached through.
	Endpoints []Endpoint `json:"endpoints"`
}

// Endpoint is one of a Discovery document's location-specific hosts.
type Endpoint struct {
	EndpointURL string `json:"endpointUrl"`
	Location    string `json:"location"`
}

// ResolvedBaseURL is where a method's path is joined onto.
func (d *Document) ResolvedBaseURL() string { return d.RootURL + d.ServicePath }

// Schema is one type, or one property of one type. Discovery reuses the same
// shape for both, and so does this.
type Schema struct {
	ID          string `json:"id"`
	Type        string `json:"type"`
	Format      string `json:"format"`
	Description string `json:"description"`
	Ref         string `json:"$ref"`
	ReadOnly    bool   `json:"readOnly"`
	// Default is the value the API assumes when the field is absent. Read
	// for one purpose: a `kind` whose default is its "service#type" constant.
	Default              any                `json:"default"`
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

// Behavior is one of the AIP-203 field behaviours Google writes into a
// property's description when Discovery has no field for it.
//
// Proto-first APIs annotate fields with google.api.field_behavior, and
// Discovery keeps only one of those annotations as structure (OUTPUT_ONLY
// becomes readOnly). The rest survive as a run of tags at the START of the
// description -- "Optional. Input only. Immutable. Tag keys bound to this
// resource." -- and compute writes its own in brackets ("[Output Only]",
// "[Input Only]"). Measured against googleapis on 2026-09-23: every one of the
// 19 fields a proto marked IMMUTABLE that the catalog did not treat as
// ForceNew already said "Immutable." in this run.
type Behavior string

const (
	BehaviorOutputOnly Behavior = "output only"
	BehaviorInputOnly  Behavior = "input only"
	BehaviorImmutable  Behavior = "immutable"
	BehaviorIdentifier Behavior = "identifier"
	BehaviorRequired   Behavior = "required"
	BehaviorOptional   Behavior = "optional"
)

// behaviorTag matches ONE leading tag. Only the leading run counts: the tags
// are a convention for the first words of a field comment, and the same words
// appear later in ordinary prose ("the deadline for changing ... is immutable
// after") that declares nothing. Measured: 5 settable properties in the
// catalog mention immutability mid-sentence, and none of them is a
// declaration.
var behaviorTag = regexp.MustCompile(`(?i)^\s*(?:\[(output only|input only)\]|(output only|input only|immutable|identifier|required|optional)\.)\s*`)

// Behaviors reads the leading run of field-behaviour tags from a property's
// description. It stops at the first word that is not a tag, so the order the
// tags come in does not matter and prose after them is never read.
func Behaviors(s *Schema) map[Behavior]bool {
	out := map[Behavior]bool{}
	if s == nil {
		return out
	}
	d := s.Description
	for {
		m := behaviorTag.FindStringSubmatch(d)
		if m == nil {
			return out
		}
		tag := m[1]
		if tag == "" {
			tag = m[2]
		}
		out[Behavior(strings.ToLower(tag))] = true
		d = d[len(m[0]):]
	}
}

// OutputOnly reports whether GCP, not the user, sets this property.
//
// It unions two signals because neither is sufficient on its own: compute
// carries 1,520 `readOnly` flags but 2,202 `[Output Only]` prose markers
// (2,202 of 5,177 top-level schema properties match the marker as a
// description prefix, against 1,520 carrying the flag, at compute revision
// 20260910), and the modern APIs use the flag. Trusting either alone marks
// settable properties read-only, or read-only ones settable.
//
// The prose side reads the whole leading tag run rather than just the first
// tag, so "Optional. Output only." counts too. Measured on 2026-09-23 this
// changes nothing today -- every such field also carries readOnly -- but a
// prefix test would have missed one the day the flag did not come with it.
func (d *Document) OutputOnly(s *Schema) bool {
	if s == nil {
		return false
	}
	return s.ReadOnly || Behaviors(s)[BehaviorOutputOnly]
}
