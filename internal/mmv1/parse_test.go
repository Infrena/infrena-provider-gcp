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

// TestTheLifecycleKeysAreRead covers every key added once the coverage guard
// showed they were being dropped. Each is set in the fixture to something its
// zero value is not.
func TestTheLifecycleKeysAreRead(t *testing.T) {
	r := loadRes(t, "Gadget.yaml")
	if !r.Immutable || r.ReadVerb != "POST" || r.DeleteVerb != "PATCH" || !r.ExcludeDelete || !r.ExcludeRead ||
		r.ReadQueryParams != "?view=FULL" || r.Mutex != "gadgets/{{project}}" ||
		r.IDFormat != "projects/{{project}}/gadgets/{{name}}" || !r.ExcludeResource {
		t.Errorf("resource keys not read: %+v", r)
	}
	if nq := r.NestedQuery; nq == nil || len(nq.Keys) != 2 || nq.Keys[1] != "gadgets" || !nq.IsListOfIDs || !nq.ModifyByPatch {
		t.Errorf("nested_query = %+v", r.NestedQuery)
	}
	if !field(t, r, "zone").URLParamOnly {
		t.Error("url_param_only not read")
	}
	if l := field(t, r, "labels"); l.UpdateURL != "projects/{{project}}/gadgets/{{name}}/setLabels" ||
		l.UpdateVerb != "POST" || l.UpdateID != "labels" || l.FingerprintName != "labelFingerprint" {
		t.Errorf("per-field update not read: %+v", l)
	}
	if p := field(t, r, "password"); !p.Sensitive || !p.WriteOnly || !p.ClientSide {
		t.Errorf("secret flags not read: %+v", p)
	}
	s := field(t, r, "size")
	if s.MinVersion != "beta" || !s.DefaultFromAPI || !s.IgnoreRead || !s.SendEmptyValue ||
		s.CustomExpand == "" || s.CustomFlatten == "" || len(s.UpdateMaskFields) != 2 || s.UpdateMaskFields[1] != "config.unit" {
		t.Errorf("field keys not read: %+v", s)
	}
	if it := field(t, r, "tags").ItemType; it == nil || it.Type != "String" {
		t.Errorf("a mapping item_type lost its type: %+v", it)
	}
	it := field(t, r, "rules").ItemType
	if it == nil || it.Type != "NestedObject" || len(it.Properties) != 1 {
		t.Fatalf("a nested item_type lost its fields: %+v", it)
	}
	if p := it.Properties[0]; !p.Immutable || p.ApiName != "rank" {
		t.Errorf("a field inside a list element lost its flags: %+v", p)
	}
}

// TestAScalarItemTypeIsItsTypeName. Most arrays spell the element as a bare
// name, and that spelling must not fail to decode now that a mapping is
// decoded as a field.
func TestAScalarItemTypeIsItsTypeName(t *testing.T) {
	r, err := ParseResource([]byte("name: X\nproperties:\n  - name: ids\n    type: Array\n    item_type: String\n"))
	if err != nil {
		t.Fatal(err)
	}
	if it := r.Properties[0].ItemType; it == nil || it.Type != "String" {
		t.Errorf("item_type = %+v, want Type String", it)
	}
}
