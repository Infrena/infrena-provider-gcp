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
	// becomes a lie. 26 measured on 2026-09-25, all top-level; the floor is
	// below that so ordinary drift in Google's Discovery documents does not
	// trip it.
	if len(renamed) < 15 {
		t.Fatalf("the corpus has %d renamed attributes; 26 were measured on 2026-09-25, so "+
			"anything under 15 means the generator changed its renaming strategy and this "+
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

// TestRenamesStayAtTheTopLevel. The host reserves `type`, `provider` and
// `lifecycle` only among a resource's own keys, so only a top-level
// attribute is renamed. Until 2026-09-25 every nested one was renamed too
// (a BigQuery schema field's `type` had to be written type_value), and the
// live suite found it. The nested translation in names.go stays, tested by
// the fixtures below, but the real corpus must not need it.
func TestRenamesStayAtTheTopLevel(t *testing.T) {
	for _, r := range everyRenamedAttribute(t) {
		if r.depth > 0 || r.viaList {
			t.Errorf("%s%s is renamed below the top level", r.ty, r.path)
		}
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

// TestAListOfScalarsIsCarriedThroughUnchanged. 800 of the catalog's 1901 list
// elements are plain strings and 5 are integers -- the second most common
// element shape after a map. Both directions reach them: wireValue recurses
// into a list whenever Elem is set, regardless of whether the element has any
// Fields, and schemaValue does the same.
//
// For a scalar element the correct behaviour is to do NOTHING: there are no
// keys to rename, and the value is the user's data. That is exactly the shape
// that goes untested, because code which correctly does nothing is
// indistinguishable from code that never runs -- until someone changes the
// walk. Asserted here so a future edit that starts folding element VALUES the
// way it folds element KEYS fails rather than silently mangling data.
//
// Written after infrena found the identical hole on its own side of this
// boundary, from this catalog's Kind distribution.
func TestAListOfScalarsIsCarriedThroughUnchanged(t *testing.T) {
	ty := &catalog.Type{
		Name: "gcp.widget",
		Attributes: map[string]*catalog.Attr{
			// A renamed sibling, so the test proves translation is RUNNING and
			// merely leaving the scalar list alone, rather than passing because
			// nothing was translated at all.
			"type_value": {Canonical: "type", Kind: value.KindString},
			"tags": {
				Canonical: "tags", Kind: value.KindList,
				Elem: &catalog.Attr{Kind: value.KindString},
			},
		},
	}
	original := []string{"b", "a", "type_value"} // one item spelled like a schema key
	items := make([]value.Value, len(original))
	for i, s := range original {
		items[i] = value.String(s, value.SourceExplicit)
	}

	body := wireBody(ty.Attributes, map[string]value.Value{
		"type_value": value.String("IPV4", value.SourceExplicit),
		"tags":       value.List(items, value.SourceExplicit),
	})

	if _, renamed := body["type"]; !renamed {
		t.Fatal("the renamed sibling did not translate, so this test proves nothing about lists")
	}
	got, ok := body["tags"].([]any)
	if !ok {
		t.Fatalf("tags came back as %T, not a list", body["tags"])
	}
	if len(got) != len(original) {
		t.Fatalf("tags has %d items, sent %d", len(got), len(original))
	}
	for i, want := range original {
		if got[i] != any(want) {
			t.Errorf("tags[%d] = %v, want %q -- an element's value is data, not a name to fold",
				i, got[i], want)
		}
	}

	// And the inverse direction, on the same shape.
	back := schemaAttrs(ty.Attributes, map[string]any{
		"type": "IPV4",
		"tags": []any{"b", "a", "type_value"},
	})
	list, ok := back["tags"].Raw.([]value.Value)
	if !ok {
		t.Fatalf("tags came back as %T, not a list", back["tags"].Raw)
	}
	for i, want := range original {
		if s, _ := list[i].Raw.(string); s != want {
			t.Errorf("schemaAttrs tags[%d] = %q, want %q", i, s, want)
		}
	}
}

// TestANestedComputedFieldNeverReachesACreateBody. GCP rejects a body that
// sets a server-computed field at ANY depth, and 922 attributes below the top
// level are Output across 101 types.
//
// The host does not stop one arriving: infrena refuses a computed attribute
// only in its top-level loop, and checkNestedKeys, the only thing that walks
// deeper, checks that a key exists and nothing else. So this is reachable from
// ordinary configuration rather than theoretical.
//
// The patch path always withheld these (buildNested skips f.Output). The
// create path did not, so the same field was correctly kept out of a PATCH and
// sent on a POST.
func TestANestedComputedFieldNeverReachesACreateBody(t *testing.T) {
	ty := &catalog.Type{
		Name: "gcp.widget",
		Attributes: map[string]*catalog.Attr{
			"config": {
				Canonical: "config", Kind: value.KindMap,
				Fields: map[string]*catalog.Attr{
					"size":      {Canonical: "size", Kind: value.KindString},
					"createdAt": {Canonical: "createdAt", Kind: value.KindString, Output: true},
				},
			},
		},
	}
	body := wireBody(ty.Attributes, map[string]value.Value{
		"config": value.Map(map[string]value.Value{
			"size":      value.String("large", value.SourceExplicit),
			"createdAt": value.String("2026-09-23T00:00:00Z", value.SourceExplicit),
		}, value.SourceExplicit),
	})
	cfg, ok := body["config"].(map[string]any)
	if !ok {
		t.Fatalf("config came back as %T", body["config"])
	}
	if _, sent := cfg["createdAt"]; sent {
		t.Error("a nested Output field reached the create body; GCP rejects a body that sets one")
	}
	if got := cfg["size"]; got != any("large") {
		t.Errorf("the settable sibling did not survive: %v — this test would pass vacuously if "+
			"the whole object were dropped", got)
	}
}
