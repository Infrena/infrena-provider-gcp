package disco

import (
	"fmt"
	"slices"
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

// TestResolveClonesEnumSoTheSourceDocumentCannotBeMutated. Resolve's own doc
// comment says the copy exists so expanding in place can't corrupt the
// document for the next caller — but a bare struct copy only copies the Enum
// slice header, leaving the resolved copy's backing array aliased to the
// schema still sitting in d.Schemas. A caller that mutates an element (not
// appends) of the "immutable" copy would silently corrupt every later
// caller's view of that schema. This is latent today because nothing else in
// this package mutates Enum, which is exactly why it needs a test rather
// than waiting to be found as a live bug later.
func TestResolveClonesEnumSoTheSourceDocumentCannotBeMutated(t *testing.T) {
	d := load(t, "tiny.json")
	original := slices.Clone(d.Schemas["Status"].Enum)

	resolved, err := d.Resolve(d.Schemas["Status"])
	if err != nil {
		t.Fatalf("Resolve(Status): %v", err)
	}
	if len(resolved.Enum) == 0 {
		t.Fatalf("resolved.Enum is empty; fixture is supposed to carry a non-empty enum")
	}

	resolved.Enum[0] = "MUTATED"

	if got, want := d.Schemas["Status"].Enum, original; !slices.Equal(got, want) {
		t.Errorf("mutating the resolved copy's Enum corrupted d.Schemas: got %v, want %v (shallow copy shared the backing array)", got, want)
	}
}

// TestResolveRefSiteDescriptionWinsOverTarget. The $ref branch's own comment
// claims the referring site's description overrides the target schema's, but
// only the readOnly half of that inheritance was ever asserted. WidgetSpec
// carries its own non-empty description ("Generic spec container."), which a
// resolver that only overrides an EMPTY description (rather than always
// preferring the call site) would wrongly surface instead of "The spec.".
func TestResolveRefSiteDescriptionWinsOverTarget(t *testing.T) {
	d := load(t, "tiny.json")
	w := d.Schemas["Widget"]
	resolved, err := d.Resolve(w.Properties["spec"])
	if err != nil {
		t.Fatalf("Resolve(spec): %v", err)
	}
	if want := "The spec."; resolved.Description != want {
		t.Errorf("Description = %q, want %q (the $ref site's own description must win over the target schema's %q)",
			resolved.Description, want, d.Schemas["WidgetSpec"].Description)
	}
}
