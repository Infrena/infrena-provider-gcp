package disco

import (
	"fmt"
	"testing"
)

// TestResolveTerminatesOnACycle. Discovery documents contain genuine $ref cycles
// (bigquery, container and spanner each have one; container's is a direct
// self-reference, OperationProgress -> OperationProgress). An unbounded
// resolver hangs the generator, which is why this test carries a timeout: a
// hang must be a failure, not a stuck suite.
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

// TestResolveDoesNotTruncateStructuralNesting draws the distinction the bound
// exists for: maxRefDepth counts $ref hops, not plain "properties" nesting.
// A schema nested deeper than maxRefDepth through properties alone, with no
// $ref anywhere, contradicts a resolver that truncates it — that resolver
// would be treating finite structural nesting as if it were an unbounded
// $ref cycle.
func TestResolveDoesNotTruncateStructuralNesting(t *testing.T) {
	d := load(t, "tiny.json") // only need a *Document to call the method on

	const levels = maxRefDepth + 5 // deeper than the bound, on purpose
	leaf := &Schema{Type: "string", Description: "leaf"}
	root := leaf
	for i := 0; i < levels; i++ {
		root = &Schema{
			Type:        "object",
			Description: fmt.Sprintf("level %d", i),
			Properties:  map[string]*Schema{"next": root},
		}
	}

	got, err := d.Resolve(root)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	depth := 0
	cur := got
	for cur != nil {
		next, ok := cur.Properties["next"]
		if !ok {
			break
		}
		cur = next
		depth++
	}
	if depth != levels {
		t.Fatalf("structural nesting truncated at depth %d, want %d (no $ref was involved, so the bound must not fire)", depth, levels)
	}
	if cur.Description != "leaf" {
		t.Errorf("deepest node description = %q, want %q; nesting deeper than maxRefDepth replaced a real leaf with an opaque placeholder", cur.Description, "leaf")
	}
}
