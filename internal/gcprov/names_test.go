package gcprov

import (
	"testing"

	"github.com/infrena/infrena-provider-gcp/internal/catalog"
	"github.com/infrena/infrena/pkg/value"
)

// renamedAttr is one attribute whose schema key differs from its wire name,
// together with the LEVEL it was found at -- the map toWire and toSchema
// have to be given to answer for it.
type renamedAttr struct {
	ty      string
	path    string
	level   map[string]*catalog.Attr
	key     string
	canon   string
	depth   int
	viaList bool
}

// everyRenamedAttribute walks every declared attribute of every type in the
// real catalog, through Fields AND through Elem, and returns the ones the
// generator renamed.
//
// Through Elem is the whole point. A walk that follows only Fields finds 43
// of the 306, and reports gcp.grpcroute, gcp.responsepolicyrule and
// gcp.router -- all three updatable -- as having no renamed attributes at
// all.
func everyRenamedAttribute(t *testing.T) []renamedAttr {
	t.Helper()
	var out []renamedAttr
	var walk func(tyName, path string, depth int, viaList bool, level map[string]*catalog.Attr)
	walk = func(tyName, path string, depth int, viaList bool, level map[string]*catalog.Attr) {
		for key, a := range level {
			here := path + "." + key
			if a.Canonical != key {
				out = append(out, renamedAttr{tyName, here, level, key, a.Canonical, depth, viaList})
			}
			if len(a.Fields) > 0 {
				walk(tyName, here, depth+1, viaList, a.Fields)
			}
			if a.Elem != nil && len(a.Elem.Fields) > 0 {
				walk(tyName, here+"[]", depth+1, true, a.Elem.Fields)
			}
		}
	}
	for _, ty := range mustCatalog(t).Types {
		walk(ty.Name, "", 0, false, ty.Attributes)
	}
	return out
}

// TestEveryRenamedAttributeSurvivesARoundTrip. The generator renames a field
// that collides with a reserved word -- "type" becomes "type_value",
// "provider" becomes "provider_value", "lifecycle" becomes
// "lifecycle_value" -- so a value written in configuration under the schema
// name must reach GCP under the wire name, and a body field arriving under
// the wire name must land in state under the schema name. Neither held
// before Task 14a: Canonical was consulted only in patch.go, and crud.go had
// never looked at it at all.
//
// Against the REAL catalog, deliberately. widgetType() has no renamed
// attribute, which is exactly why this defect survived fourteen tasks.
func TestEveryRenamedAttributeSurvivesARoundTrip(t *testing.T) {
	renamed := everyRenamedAttribute(t)

	// THE SET THIS ITERATES MUST NOT BE ABLE TO BECOME EMPTY SILENTLY. A test
	// that loops over a set the generator controls and asserts nothing about
	// its size passes perfectly for a catalog that stopped renaming anything,
	// which is the moment this test's subject disappears and its green tick
	// becomes a lie. 306 measured on 2026-09-22; the floor is well below that
	// so ordinary drift in Google's Discovery documents does not trip it.
	if len(renamed) < 200 {
		t.Fatalf("the corpus has %d renamed attributes; 306 were measured on 2026-09-22, so "+
			"anything under 200 means the generator changed its renaming strategy and this "+
			"test is no longer exercising what it claims to", len(renamed))
	}

	for _, r := range renamed {
		if got := toWire(r.level, r.key); got != r.canon {
			t.Errorf("%s%s: toWire(%q) = %q, want the wire name %q", r.ty, r.path, r.key, got, r.canon)
		}
		if got := toSchema(r.level, r.canon); got != r.key {
			t.Errorf("%s%s: toSchema(%q) = %q, want the schema name %q", r.ty, r.path, r.canon, got, r.key)
		}
	}
}

// TestRenamedAttributesAreNotOnlyATopLevelProblem pins the two facts that
// decide how far the translation has to recurse, so a later simplification
// down to "translate the top level" or "translate Fields" fails here with
// the reason attached rather than passing and hiding two thirds of the
// corpus.
func TestRenamedAttributesAreNotOnlyATopLevelProblem(t *testing.T) {
	var nested, viaList, deepest int
	for _, r := range everyRenamedAttribute(t) {
		if r.depth > 0 {
			nested++
		}
		if r.viaList {
			viaList++
		}
		if r.depth > deepest {
			deepest = r.depth
		}
	}
	// 286 nested, 263 of them inside a list, deepest 9, measured 2026-09-22.
	if nested == 0 {
		t.Error("no renamed attribute is nested; a top-level-only translation would be enough " +
			"and this test should be deleted rather than loosened")
	}
	if viaList == 0 {
		t.Error("no renamed attribute is reached through a list; recursing into Fields alone " +
			"would be enough and names.go's Elem handling should be deleted rather than kept " +
			"untested")
	}
	if deepest < 5 {
		t.Errorf("the deepest renamed attribute is at depth %d; 9 was measured, and a shallow "+
			"corpus would make this suite stop exercising real recursion", deepest)
	}
}

// listElementType is the smallest fixture that tells a Fields-only
// translation from a correct one: a LIST whose elements carry a renamed
// field, which is the shape 263 of the corpus's 306 renamed attributes have
// and the only shape gcp.grpcroute, gcp.responsepolicyrule and gcp.router
// have at all.
func listElementType() *catalog.Type {
	return &catalog.Type{
		Name: "gcp.listy",
		Attributes: map[string]*catalog.Attr{
			"nats": {
				Canonical: "nats", Kind: value.KindList,
				Elem: &catalog.Attr{
					Canonical: "nats", Kind: value.KindMap,
					Fields: map[string]*catalog.Attr{
						"name":       {Canonical: "name", Kind: value.KindString},
						"type_value": {Canonical: "type", Kind: value.KindString},
					},
				},
			},
			"labels": {Canonical: "labels", Kind: value.KindMap, Opaque: true},
		},
	}
}

// TestTranslationReachesInsideAList. gcp.router, gcp.grpcroute and
// gcp.responsepolicyrule are updatable types whose ONLY renamed attributes
// live inside a list element, so a translation that recurses through Fields
// but not Elem reports all three as unaffected and sends "type_value" inside
// every list object it writes.
func TestTranslationReachesInsideAList(t *testing.T) {
	ty := listElementType()
	vals := map[string]value.Value{
		"nats": value.List([]value.Value{
			mapValue(map[string]any{"name": "nat-1", "type_value": "AUTO_ONLY"}),
		}, value.SourceExplicit),
	}

	body := wireBody(ty.Attributes, vals)
	items, ok := body["nats"].([]any)
	if !ok || len(items) != 1 {
		t.Fatalf("body[nats] = %#v, want a one-element list", body["nats"])
	}
	elem, ok := items[0].(map[string]any)
	if !ok {
		t.Fatalf("body[nats][0] = %#v, want an object", items[0])
	}
	if _, leaked := elem["type_value"]; leaked {
		t.Errorf("the schema name reached the wire inside a list element: %#v", elem)
	}
	if elem["type"] != "AUTO_ONLY" {
		t.Errorf("body[nats][0] = %#v, want the wire name %q carrying the value", elem, "type")
	}

	// And back: a response body's list element must land in state under the
	// schema name, or every plan reports a change to a field nobody touched.
	back := schemaAttrs(ty.Attributes, map[string]any{
		"nats": []any{map[string]any{"name": "nat-1", "type": "AUTO_ONLY"}},
	})
	list, ok := back["nats"].Raw.([]value.Value)
	if !ok || len(list) != 1 {
		t.Fatalf("state[nats] = %#v, want a one-element list", back["nats"])
	}
	fields, ok := list[0].Raw.(map[string]value.Value)
	if !ok {
		t.Fatalf("state[nats][0] = %#v, want an object", list[0])
	}
	if _, leaked := fields["type"]; leaked {
		t.Errorf("the wire name reached state inside a list element: %#v", fields)
	}
	if got, ok := fields["type_value"].AsString(); !ok || got != "AUTO_ONLY" {
		t.Errorf("state[nats][0] = %#v, want the schema name %q carrying the value", fields, "type_value")
	}
}

// TestAnOpaqueValueKeepsItsOwnKeys. catalog.Attr.Opaque says a value is
// "copied exactly: no key translation, nothing dropped, no reordering" --
// free-form maps hold user data whose keys mean whatever the user meant, and
// a label literally called "type" is a label called "type", not an
// attribute this catalog renamed.
func TestAnOpaqueValueKeepsItsOwnKeys(t *testing.T) {
	ty := listElementType()
	vals := map[string]value.Value{
		"labels": mapValue(map[string]any{"type": "prod", "provider": "us"}),
	}
	labels, ok := wireBody(ty.Attributes, vals)["labels"].(map[string]any)
	if !ok {
		t.Fatalf("body[labels] = %#v, want an object", wireBody(ty.Attributes, vals)["labels"])
	}
	if labels["type"] != "prod" || labels["provider"] != "us" {
		t.Errorf("an opaque value's own keys were rewritten: %#v", labels)
	}

	back, ok := schemaAttrs(ty.Attributes, map[string]any{
		"labels": map[string]any{"type": "prod"},
	})["labels"].Raw.(map[string]value.Value)
	if !ok {
		t.Fatal("state[labels] is not an object")
	}
	if _, present := back["type"]; !present {
		t.Errorf("an opaque value's own keys were rewritten on the way back: %#v", back)
	}
}

// TestAnUndeclaredFieldIsCarriedThroughUnchanged. GCP answers with fields
// the catalog never modelled, and a translation that only knows declared
// names must leave those alone rather than dropping or renaming them.
func TestAnUndeclaredFieldIsCarriedThroughUnchanged(t *testing.T) {
	ty := listElementType()
	if got := toWire(ty.Attributes, "somethingNew"); got != "somethingNew" {
		t.Errorf("toWire of an undeclared name = %q, want it unchanged", got)
	}
	if got := toSchema(ty.Attributes, "somethingNew"); got != "somethingNew" {
		t.Errorf("toSchema of an undeclared name = %q, want it unchanged", got)
	}
	back := schemaAttrs(ty.Attributes, map[string]any{"somethingNew": "x"})
	if got, ok := back["somethingNew"].AsString(); !ok || got != "x" {
		t.Errorf("an undeclared response field was lost: %#v", back)
	}
}

// TestWireAliasesOffersBothSpellingsToAUrlTemplate. gcp.resourcerecordset's
// self_link is "projects/{project}/managedZones/{managedZone}/rrsets/{name}/{type}",
// taken from Cloud DNS's own Discovery path, so it asks for the WIRE name
// while configuration supplies "type_value". It is the only type in the
// corpus whose url templates name a renamed attribute.
func TestWireAliasesOffersBothSpellingsToAUrlTemplate(t *testing.T) {
	ty, ok := mustCatalog(t).Type("gcp.resourcerecordset")
	if !ok {
		t.Fatal("the catalog no longer ships gcp.resourcerecordset")
	}
	if !contains(ty.SelfLink, "{type}") {
		t.Fatalf("this test exists for a self_link naming a renamed attribute; got %q", ty.SelfLink)
	}
	configured := attrs(map[string]string{
		"project": "p", "managedZone": "z", "name": "www.example.com.", "type_value": "A",
	})

	// The pre-fix behaviour, kept here so the fix is visible rather than
	// asserted: the template cannot be expanded from configuration alone.
	if _, err := ExpandURL(ty.SelfLink, configured); err == nil {
		t.Fatal("self_link expanded without the wire alias, so this test no longer proves anything")
	}

	id, err := ProviderID(ty, nil, configured)
	if err != nil {
		t.Fatalf("ProviderID from configuration alone: %v", err)
	}
	if want := "projects/p/managedZones/z/rrsets/www.example.com./A"; id != want {
		t.Errorf("provider id = %q, want %q", id, want)
	}
}
