package mmv1

import (
	"os"
	"testing"
)

func loadRes(t *testing.T, name string) *Resource {
	t.Helper()
	data, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	r, err := ParseResource(data)
	if err != nil {
		t.Fatalf("ParseResource(%s): %v", name, err)
	}
	return r
}

func field(t *testing.T, r *Resource, name string) *Field {
	t.Helper()
	for _, f := range append(append([]*Field{}, r.Parameters...), r.Properties...) {
		if f.Name == name {
			return f
		}
	}
	t.Fatalf("no field %q", name)
	return nil
}

func TestFlagsAreReadPerFieldNotDefaulted(t *testing.T) {
	r := loadRes(t, "Widget.yaml")
	cases := []struct {
		name      string
		required  bool
		immutable bool
		output    bool
	}{
		{"region", true, true, false},
		{"name", true, true, false},
		{"sizeGb", false, false, false},
		{"createTime", false, false, true},
		{"network", false, false, false},
	}
	for _, c := range cases {
		f := field(t, r, c.name)
		if f.Required != c.required || f.Immutable != c.immutable || f.Output != c.output {
			t.Errorf("%s: required=%v immutable=%v output=%v, want %v/%v/%v",
				c.name, f.Required, f.Immutable, f.Output, c.required, c.immutable, c.output)
		}
	}
}

// TestImmutabilityIsReadAtEveryDepth. 3,185 immutable fields across the corpus, and
// nested ones are not rare. A parser that only read top-level flags would satisfy the
// test above and still lose every nested ForceNew.
func TestNestedFieldsKeepTheirFlags(t *testing.T) {
	r := loadRes(t, "Widget.yaml")
	cfg := field(t, r, "config")
	if len(cfg.Properties) != 2 {
		t.Fatalf("config has %d nested fields, want 2", len(cfg.Properties))
	}
	var replica *Field
	for _, f := range cfg.Properties {
		if f.Name == "replicaCount" {
			replica = f
		}
	}
	if replica == nil {
		t.Fatal("no nested replicaCount")
	}
	if !replica.Immutable {
		t.Error("nested replicaCount lost its immutable flag")
	}
}

func TestResourceRefCarriesItsTarget(t *testing.T) {
	r := loadRes(t, "Widget.yaml")
	n := field(t, r, "network")
	if n.Type != "ResourceRef" {
		t.Fatalf("network type = %q, want ResourceRef", n.Type)
	}
	if n.Resource != "Network" {
		t.Errorf("network target = %q, want Network", n.Resource)
	}
}

func TestURLsAndUpdateMechanicsAreRead(t *testing.T) {
	r := loadRes(t, "Widget.yaml")
	if r.BaseURL != "projects/{{project}}/locations/{{region}}/widgets" {
		t.Errorf("base_url = %q", r.BaseURL)
	}
	if !r.UpdateMask {
		t.Error("update_mask not read")
	}
	if r.UpdateVerb != "PATCH" {
		t.Errorf("update_verb = %q, want PATCH", r.UpdateVerb)
	}
	if r.Async == nil || r.Async.Type != "OpAsync" {
		t.Errorf("async = %+v, want OpAsync", r.Async)
	}
}
