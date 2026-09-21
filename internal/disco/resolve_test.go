package disco

import "testing"

// TestResolveTerminatesOnACycle. Discovery documents contain genuine $ref cycles
// (compute's Expr/Policy is one). An unbounded resolver hangs the generator, which
// is why this test carries a timeout: a hang must be a failure, not a stuck suite.
func TestResolveTerminatesOnACycle(t *testing.T) {
	d := load(t, "cyclic.json")
	got, err := d.Resolve(d.Schemas["Node"])
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	// It must have expanded SOMETHING, or the test would pass against a resolver
	// that gave up immediately.
	if got.Properties["child"] == nil || got.Properties["child"].Properties["node"] == nil {
		t.Fatalf("resolver expanded nothing: %+v", got)
	}
	// And it must have stopped. Walk to the bottom and confirm the deepest node is
	// marked opaque rather than continuing.
	depth := 0
	cur := got
	for cur != nil && depth < 100 {
		child := cur.Properties["child"]
		if child == nil {
			break
		}
		cur = child.Properties["node"]
		depth++
	}
	if depth >= 100 {
		t.Fatalf("resolver did not stop: reached depth %d", depth)
	}
	if depth > maxRefDepth {
		t.Errorf("expanded to depth %d, want at most %d", depth, maxRefDepth)
	}
}
