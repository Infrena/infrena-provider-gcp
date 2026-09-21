package disco

import (
	"os"
	"testing"
)

func load(t *testing.T, name string) *Document {
	t.Helper()
	data, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	d, err := Parse(data)
	if err != nil {
		t.Fatalf("Parse(%s): %v", name, err)
	}
	return d
}

func TestCollectionsAreFlattenedWithTheirFullPath(t *testing.T) {
	d := load(t, "tiny.json")
	cols := d.Collections()
	if len(cols) != 1 {
		t.Fatalf("got %d collections, want 1: %+v", len(cols), cols)
	}
	c := cols[0]
	// The path must carry BOTH segments. A flattener that kept only the leaf would
	// report "widgets" and pass a weaker assertion.
	if got, want := len(c.Path), 2; got != want {
		t.Fatalf("path %v has %d segments, want %d", c.Path, got, want)
	}
	if c.Path[0] != "projects" || c.Path[1] != "widgets" {
		t.Errorf("path = %v, want [projects widgets]", c.Path)
	}
	for _, m := range []string{"get", "insert", "delete"} {
		if _, ok := c.Methods[m]; !ok {
			t.Errorf("method %q missing", m)
		}
	}
}

func TestOutputOnlyUnionsTheFlagAndTheProse(t *testing.T) {
	d := load(t, "tiny.json")
	w := d.Schemas["Widget"]
	cases := map[string]bool{
		"name":              false,
		"selfLink":          true, // prose only, no flag — compute's whole convention
		"creationTimestamp": true, // flag only, no prose
		"spec":              false,
	}
	for prop, want := range cases {
		if got := d.OutputOnly(w.Properties[prop]); got != want {
			t.Errorf("OutputOnly(%s) = %v, want %v", prop, got, want)
		}
	}

	// A $ref property can be marked readOnly at the referring site even though
	// the target schema (WidgetSpec) carries neither the flag nor the prose.
	// OutputOnly must see that only after Resolve inlines the ref and carries
	// the call site's readOnly onto the result — a resolver that drops that
	// inheritance would make this false.
	resolved, err := d.Resolve(w.Properties["activeConfig"])
	if err != nil {
		t.Fatalf("Resolve(activeConfig): %v", err)
	}
	if !d.OutputOnly(resolved) {
		t.Errorf("OutputOnly(resolved activeConfig) = false, want true (readOnly at the $ref site)")
	}
}
