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
