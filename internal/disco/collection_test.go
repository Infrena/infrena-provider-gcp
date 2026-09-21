package disco

import "testing"

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
