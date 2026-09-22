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

	// Every other URL and lifecycle field the generator reads. These were
	// unchecked until now, so a parser that silently dropped any of them would
	// have passed: Task 8 builds a type's create, read and delete calls out of
	// exactly these, and a missing delete_url or import_format is a type that
	// generates but cannot be destroyed or imported.
	if want := "projects/{{project}}/locations/{{region}}/widgets?widgetId={{name}}"; r.CreateURL != want {
		t.Errorf("create_url = %q, want %q", r.CreateURL, want)
	}
	if want := "projects/{{project}}/locations/{{region}}/widgets/{{name}}"; r.SelfLink != want {
		t.Errorf("self_link = %q, want %q", r.SelfLink, want)
	}
	if want := "projects/{{project}}/locations/{{region}}/widgets/{{name}}?force=true"; r.DeleteURL != want {
		t.Errorf("delete_url = %q, want %q", r.DeleteURL, want)
	}
	// Deliberately differs from base_url and self_link, so a parser that mapped
	// the wrong key here would be caught rather than coincidentally right.
	if len(r.ImportFormat) != 1 ||
		r.ImportFormat[0] != "projects/{{project}}/locations/{{region}}/widgets/{{name}}" {
		t.Errorf("import_format = %v, want one entry", r.ImportFormat)
	}
	if r.MinVersion != "ga" {
		t.Errorf("min_version = %q, want ga", r.MinVersion)
	}
	// Absent in this fixture, and absence must read as false rather than as
	// "unset and therefore skip the check" — Classify excludes on it.
	if r.Exclude {
		t.Error("exclude is true for a fixture that does not set it")
	}
	if r.UpdateURL != "" {
		t.Errorf("update_url = %q, want empty for a fixture that does not set it", r.UpdateURL)
	}
}
