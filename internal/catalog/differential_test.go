package catalog

import (
	"strings"
	"testing"

	"github.com/infrena/infrena/pkg/schema"
)

// group is one sibling set: the map whose keys compete for a folded name.
type group struct {
	ty   *Type
	path string
	as   map[string]*Attr
}

func collectGroups(c *Catalog) []group {
	var out []group
	var walk func(ty *Type, path string, as map[string]*Attr)
	walk = func(ty *Type, path string, as map[string]*Attr) {
		if len(as) > 0 {
			out = append(out, group{ty, path, as})
		}
		for n, a := range as {
			walk(ty, path+n+".", a.Fields)
			if a.Elem != nil {
				walk(ty, path+n+"[].", a.Elem.Fields)
			}
		}
	}
	for _, ty := range c.Types {
		walk(ty, "", ty.Attributes)
	}
	return out
}

// myRule is the independent implementation: fold every name and alias in each
// sibling group and look for two spellings claiming one folded key.
func myRule(ty *Type) bool {
	bad := false
	var walk func(as map[string]*Attr)
	walk = func(as map[string]*Attr) {
		claimed := map[string]string{}
		for name, a := range as {
			for _, sp := range append([]string{name}, a.Aliases...) {
				f := strings.ToLower(sp)
				if first, taken := claimed[f]; taken && first != sp {
					bad = true
				}
				claimed[f] = sp
			}
		}
		for _, a := range as {
			walk(a.Fields)
			if a.Elem != nil {
				walk(a.Elem.Fields)
			}
		}
	}
	walk(ty.Attributes)
	return bad
}

// theirRule is infrena's own verdict, via the exported validator.
func theirRule(ty *Type) bool {
	attrs := make(map[string]schema.Attribute, len(ty.Attributes))
	for n, a := range ty.Attributes {
		attrs[n] = a.toSchema(true)
	}
	d := &schema.ResourceDefinition{Type: ty.Name, Attributes: attrs}
	return d.Validate() != nil
}

// TestDifferentialAgainstInfrenasOwnValidator checks two independent
// implementations of one rule against each other, on a corpus where they
// should both object.
//
// The rule: infrena folds an attribute spelling with strings.ToLower across
// declared names and aliases, and two spellings in one sibling group folding
// to the same key are ambiguous -- the host answers whichever the map was
// walked to first. myRule below is our own walk. theirRule asks infrena's
// exported validator.
//
// WHY INJECT RATHER THAN JUST COMPARE. Both implementations say the untouched
// catalog is clean, and that agreement is worth almost nothing on its own:
// two implementations that inspect nothing also agree. infrena's own corpus
// check made exactly this mistake -- "0 refused across 1,584 AWS types" was
// true and proved nothing, because that catalog declares no nested fields, so
// the nested check never ran once. So every comparison here is made on a group
// that has had a collision put into it deliberately, and the shape breakdown
// is reported so a future reader can see WHERE it ran, not only how often.
//
// Measured 2026-09-23 against infrena v0.15.0: 4321 groups, 3330 testable,
// 3330 agreements, 0 disagreements -- 2052 of them behind an Elem, which is
// the protocol 6 path and was unreachable before that release.
//
// theirRule builds a minimal ResourceDefinition (type and attributes). The
// baseline pass is what makes that safe: if the minimal shape were itself
// objectionable, the untouched catalog would be refused too, and it is not.
func TestDifferentialAgainstInfrenasOwnValidator(t *testing.T) {
	// Decode(blob) rather than Load(): Load returns a shared *Catalog behind a
	// sync.Once, and this test INJECTS collisions into it. Reverting after each
	// probe would usually be enough, but "usually" is not a property worth
	// relying on when every other test in this package reads the same pointer.
	// A private copy removes the question.
	c, err := Decode(blob)
	if err != nil {
		t.Fatal(err)
	}
	groups := collectGroups(c)

	// Baseline: both must agree the untouched catalog is clean.
	cleanMine, cleanTheirs := 0, 0
	for _, ty := range c.Types {
		if myRule(ty) {
			cleanMine++
		}
		if theirRule(ty) {
			cleanTheirs++
			t.Errorf("BASELINE: infrena refuses %s untouched", ty.Name)
		}
	}
	t.Logf("baseline on the untouched catalog: mine flags %d types, infrena refuses %d",
		cleanMine, cleanTheirs)

	// The differential proper: inject one collision per sibling group and ask
	// both implementations. Agreement on a clean corpus proves nothing -- both
	// could be inspecting nothing -- so every comparison below is made on a
	// corpus that SHOULD be refused.
	var tested, agreeRefuse, disagree, skipped int
	byShape := map[string]int{}
	byMembers := map[string]int{}
	for _, g := range groups {
		if len(g.as) == 0 {
			skipped++
			continue
		}
		var names []string
		for n := range g.as {
			names = append(names, n)
		}
		// A group collides when two SPELLINGS fold together, not when two
		// MEMBERS do. A one-member group collides perfectly well if that
		// member's own alias folds to its own name, and this test used to
		// skip all 991 of them as "cannot collide" -- 315 of which carry a
		// real alias, so 315 name/alias pairs went uncompared. The generator
		// emits one alias per camelCase attribute, so a single member holding
		// exactly one alias is among the commonest shapes here, not an exotic
		// one. Found by infrena reading this test rather than by the test.
		victim := names[0]
		collidesWith := victim
		if len(names) > 1 {
			collidesWith = names[1]
		}
		probe := strings.ToUpper(collidesWith)
		g.as[victim].Aliases = append(g.as[victim].Aliases, probe)

		members := "multi-member"
		if len(g.as) == 1 {
			members = "single-member"
		}
		byMembers[members]++
		shape := "top-level"
		switch {
		case strings.Contains(g.path, "[]."):
			shape = "behind an Elem"
		case g.path != "":
			shape = "nested under Fields"
		}
		mine, theirs := myRule(g.ty), theirRule(g.ty)
		tested++
		byShape[shape]++
		switch {
		case mine && theirs:
			agreeRefuse++
		default:
			disagree++
			if disagree <= 12 {
				t.Errorf("DISAGREEMENT %s %s: mine=%v infrena=%v (injected %q onto %q, sibling %q)",
					g.ty.Name, g.path, mine, theirs, probe, victim, collidesWith)
			}
		}
		// revert
		a := g.as[victim]
		a.Aliases = a.Aliases[:len(a.Aliases)-1]
	}
	t.Logf("groups: %d total, %d empty, %d tested", len(groups), skipped, tested)
	t.Logf("   multi-member %d, single-member %d (a lone member collides with its own alias)",
		byMembers["multi-member"], byMembers["single-member"])
	t.Logf("both refuse: %d    disagree: %d", agreeRefuse, disagree)
	for _, k := range []string{"top-level", "nested under Fields", "behind an Elem"} {
		t.Logf("   tested %-22s %d", k, byShape[k])
	}
	if byMembers["single-member"] == 0 {
		t.Error("no single-member group was tested; those collide via their own alias and were " +
			"wrongly excluded once already")
	}
	if byShape["behind an Elem"] == 0 {
		t.Error("no group behind an Elem was tested, so this says nothing about the protocol 6 path")
	}
}
