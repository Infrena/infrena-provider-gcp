package gen

import (
	"testing"

	"github.com/infrena/infrena-provider-gcp/internal/catalog"
	"github.com/infrena/infrena-provider-gcp/internal/disco"
	"github.com/infrena/infrena-provider-gcp/internal/mmv1"
	"github.com/infrena/infrena/pkg/value"
)

func TestKindsComeFromDiscoveryTypeAndFormat(t *testing.T) {
	cases := []struct {
		s    *disco.Schema
		want value.Kind
	}{
		{&disco.Schema{Type: "string"}, value.KindString},
		{&disco.Schema{Type: "integer", Format: "int32"}, value.KindInt},
		{&disco.Schema{Type: "string", Format: "int64"}, value.KindString}, // GCP writes int64 as a STRING
		{&disco.Schema{Type: "number", Format: "double"}, value.KindFloat},
		{&disco.Schema{Type: "boolean"}, value.KindBool},
		{&disco.Schema{Type: "array", Items: &disco.Schema{Type: "string"}}, value.KindList},
		{&disco.Schema{Type: "object"}, value.KindMap},
	}
	for _, c := range cases {
		if got := KindOf(c.s); got != c.want {
			t.Errorf("KindOf(%+v) = %v, want %v", c.s, got, c.want)
		}
	}
}

// The int64-as-string case above is not a curiosity. GCP serialises 64-bit
// integers as JSON strings throughout (compute disk sizes, quotas). Typing them
// as KindInt would make every such value fail to round-trip.

func TestFlagsComeFromTheRightSource(t *testing.T) {
	body := &disco.Schema{Type: "object", Properties: map[string]*disco.Schema{
		"name":       {Type: "string"},
		"sizeGb":     {Type: "integer", Format: "int32"},
		"selfLink":   {Type: "string", Description: "[Output Only] The URL."},
		"createTime": {Type: "string", ReadOnly: true},
	}}
	mm := &mmv1.Resource{Name: "Widget", Properties: []*mmv1.Field{
		{Name: "name", Type: "String", Required: true, Immutable: true},
		{Name: "sizeGb", Type: "Integer"},
	}}
	d := &disco.Document{Name: "tiny"}

	attrs, err := BuildAttributes(d, body, mm, nil)
	if err != nil {
		t.Fatal(err)
	}
	if a := attrs["name"]; !a.Required || !a.ForceNew || a.Output {
		t.Errorf("name: %+v — want required, forceNew, not output", a)
	}
	if a := attrs["sizeGb"]; a.Required || a.ForceNew || a.Output {
		t.Errorf("sizeGb: %+v — want none of the flags", a)
	}
	// Output-only must be detected from Discovery ALONE. magic-modules says nothing
	// about these two, which is the normal case for compute.
	for _, n := range []string{"selfLink", "createTime"} {
		if !attrs[n].Output {
			t.Errorf("%s not marked output-only", n)
		}
	}
}

func TestAliasesArePreferredThenSnakeCase(t *testing.T) {
	body := &disco.Schema{Type: "object", Properties: map[string]*disco.Schema{
		"authorizedNetwork": {Type: "string"},
		"sizeGb":            {Type: "integer", Format: "int32"},
	}}
	attrs, err := BuildAttributes(&disco.Document{Name: "tiny"}, body, &mmv1.Resource{Name: "W"},
		map[string]string{"authorizedNetwork": "network"})
	if err != nil {
		t.Fatal(err)
	}
	// Curated alias first, then the generated snake_case form.
	if got := attrs["authorizedNetwork"].Aliases; len(got) != 2 || got[0] != "network" || got[1] != "authorized_network" {
		t.Errorf("aliases = %v, want [network authorized_network]", got)
	}
	// No curated alias: snake_case only.
	if got := attrs["sizeGb"].Aliases; len(got) != 1 || got[0] != "size_gb" {
		t.Errorf("aliases = %v, want [size_gb]", got)
	}
}

// TestNestedKeysGetTheSameSpellings — spec §4.3 says nested keys accept the same
// spellings as top level. A recursion that built nested attributes without
// aliases would satisfy every assertion above and still make `config: {replica_count: 2}`
// fail for no reason the user can see.
func TestNestedKeysGetTheSameSpellings(t *testing.T) {
	body := &disco.Schema{Type: "object", Properties: map[string]*disco.Schema{
		"config": {Type: "object", Properties: map[string]*disco.Schema{
			"replicaCount": {Type: "integer", Format: "int32"},
		}},
	}}
	attrs, err := BuildAttributes(&disco.Document{Name: "tiny"}, body, &mmv1.Resource{Name: "W"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	nested := attrs["config"].Fields["replicaCount"]
	if nested == nil {
		t.Fatal("nested attribute missing")
	}
	if got := nested.Aliases; len(got) != 1 || got[0] != "replica_count" {
		t.Errorf("nested aliases = %v, want [replica_count]", got)
	}
}

func TestAKeywordCollisionIsRenamed(t *testing.T) {
	body := &disco.Schema{Type: "object", Properties: map[string]*disco.Schema{
		"type": {Type: "string"},
	}}
	attrs, err := BuildAttributes(&disco.Document{Name: "tiny"}, body, &mmv1.Resource{Name: "W"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, clash := attrs["type"]; clash {
		t.Error("an attribute named `type` would collide with the resource keyword")
	}
	if _, ok := attrs["type_value"]; !ok {
		t.Errorf("want type_value, got %v", keys(attrs))
	}
}

func TestAFreeFormMapIsOpaque(t *testing.T) {
	body := &disco.Schema{Type: "object", Properties: map[string]*disco.Schema{
		"labels": {Type: "object", AdditionalProperties: &disco.Schema{Type: "string"}},
		"config": {Type: "object", Properties: map[string]*disco.Schema{"mode": {Type: "string"}}},
	}}
	attrs, err := BuildAttributes(&disco.Document{Name: "tiny"}, body, &mmv1.Resource{Name: "W"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !attrs["labels"].Opaque {
		t.Error("a free-form map must be opaque; translating its keys would corrupt user data")
	}
	if attrs["config"].Opaque {
		t.Error("an object with declared properties must not be opaque")
	}
}

func TestNestedImmutabilityReachesTheNestedAttribute(t *testing.T) {
	body := &disco.Schema{Type: "object", Properties: map[string]*disco.Schema{
		"config": {Type: "object", Properties: map[string]*disco.Schema{
			"replicaCount": {Type: "integer", Format: "int32"},
			"mode":         {Type: "string"},
		}},
	}}
	mm := &mmv1.Resource{Name: "W", Properties: []*mmv1.Field{
		{Name: "config", Type: "NestedObject", Properties: []*mmv1.Field{
			{Name: "replicaCount", Type: "Integer", Immutable: true},
			{Name: "mode", Type: "Enum"},
		}},
	}}
	attrs, err := BuildAttributes(&disco.Document{Name: "tiny"}, body, mm, nil)
	if err != nil {
		t.Fatal(err)
	}
	cfg := attrs["config"]
	if cfg.Fields["replicaCount"] == nil || !cfg.Fields["replicaCount"].ForceNew {
		t.Errorf("nested replicaCount lost ForceNew: %+v", cfg.Fields)
	}
	if cfg.Fields["mode"].ForceNew {
		t.Error("nested mode gained a ForceNew it never had")
	}
}

func TestAwaitIsChosenFromTheOperationShape(t *testing.T) {
	longrunning := &disco.Document{Name: "redis", Schemas: map[string]*disco.Schema{
		"Operation": {Properties: map[string]*disco.Schema{"done": {Type: "boolean"}}},
	}}
	compute := &disco.Document{Name: "compute", Schemas: map[string]*disco.Schema{
		"Operation": {Properties: map[string]*disco.Schema{
			"status": {Type: "string"}, "targetLink": {Type: "string"}}},
	}}
	widget := &disco.Document{Name: "tiny", Schemas: map[string]*disco.Schema{
		"Widget": {Properties: map[string]*disco.Schema{"name": {Type: "string"}}},
	}}
	op := &disco.Method{Response: &disco.Ref{Ref: "Operation"}}
	if k, _ := AwaitOf(longrunning, op); k != catalog.AwaitLongRunning {
		t.Errorf("longrunning doc gave %v", k)
	}
	if k, _ := AwaitOf(compute, op); k != catalog.AwaitComputeOperation {
		t.Errorf("compute doc gave %v", k)
	}
	if k, _ := AwaitOf(widget, &disco.Method{Response: &disco.Ref{Ref: "Widget"}}); k != catalog.AwaitNone {
		t.Errorf("a method returning the resource gave %v, want none", k)
	}
}

func TestScopeComesFromTheURLTemplate(t *testing.T) {
	cases := map[string]catalog.Scope{
		"projects/{{project}}/global/networks":                catalog.ScopeGlobal,
		"projects/{{project}}/regions/{{region}}/subnetworks": catalog.ScopeRegional,
		"projects/{{project}}/zones/{{zone}}/instances":       catalog.ScopeZonal,
		"projects/{{project}}/locations/{{region}}/widgets":   catalog.ScopeRegional,
	}
	for url, want := range cases {
		if got := ScopeOf(url); got != want {
			t.Errorf("ScopeOf(%s) = %v, want %v", url, got, want)
		}
	}
}

func keys(m map[string]*catalog.Attr) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
