package gen

import (
	"testing"

	"github.com/infrena/infrena-provider-gcp/internal/catalog"
	"github.com/infrena/infrena-provider-gcp/internal/disco"
)

// createIDDoc publishes two collections that are identical except for one
// thing: what the resource's own `name` property is.
//
//	occupieds  name is readOnly -- Discovery means the FULL resource path, so
//	           the create url's "?occupiedId={{name}}" has nothing to fill it
//	alreadies  name is settable -- one of the 61 types that already work, and
//	           must be left untouched
func createIDDoc() string {
	return `{
  "name": "acme",
  "version": "v1",
  "rootUrl": "https://acme.googleapis.com/",
  "servicePath": "",
  "schemas": {
    "Occupied": {"id": "Occupied", "type": "object", "properties": {
      "name": {"type": "string", "readOnly": true, "description": "Output only. The full resource name."},
      "size": {"type": "integer", "format": "int64", "description": "How big."}
    }},
    "Already": {"id": "Already", "type": "object", "properties": {
      "name": {"type": "string", "description": "The short name you choose."},
      "size": {"type": "integer", "format": "int64", "description": "How big."}
    }},
    "Operation": {"id": "Operation", "type": "object", "properties": {"status": {"type": "string"}}}
  },
  "resources": {
    "projects": {"resources": {
      "occupieds": {"methods": {
        "get": {"id": "a.o.get", "path": "projects/{project}/occupieds/{id}", "httpMethod": "GET", "response": {"$ref": "Occupied"}},
        "create": {"id": "a.o.create", "path": "projects/{project}/occupieds", "httpMethod": "POST", "request": {"$ref": "Occupied"}, "response": {"$ref": "Operation"},
          "parameters": {"occupiedId": {"type": "string", "location": "query", "description": "The id to give the new occupied."}}},
        "delete": {"id": "a.o.delete", "path": "projects/{project}/occupieds/{id}", "httpMethod": "DELETE", "response": {"$ref": "Operation"}}
      }},
      "alreadies": {"methods": {
        "get": {"id": "a.a.get", "path": "projects/{project}/alreadies/{id}", "httpMethod": "GET", "response": {"$ref": "Already"}},
        "create": {"id": "a.a.create", "path": "projects/{project}/alreadies", "httpMethod": "POST", "request": {"$ref": "Already"}, "response": {"$ref": "Operation"},
          "parameters": {"alreadyId": {"type": "string", "location": "query", "description": "The id to give the new already."}}},
        "delete": {"id": "a.a.delete", "path": "projects/{project}/alreadies/{id}", "httpMethod": "DELETE", "response": {"$ref": "Operation"}}
      }}
    }}
  }
}`
}

func createIDFixture(t *testing.T) Inputs {
	t.Helper()
	return writeRefFixture(t, map[string]string{
		"schemas/acme.json": createIDDoc(),
		"mmv1/products/acme/Occupied.yaml": `name: Occupied
description: its name is the server's, not yours.
base_url: projects/{{project}}/occupieds
create_url: projects/{{project}}/occupieds?occupiedId={{name}}
self_link: projects/{{project}}/occupieds/{{name}}
properties:
  - name: name
    type: String
  - name: size
    type: Integer
`,
		"mmv1/products/acme/Already.yaml": `name: Already
description: its name is yours to choose.
base_url: projects/{{project}}/alreadies
create_url: projects/{{project}}/alreadies?alreadyId={{name}}
self_link: projects/{{project}}/alreadies/{{name}}
properties:
  - name: name
    type: String
  - name: size
    type: Integer
`,
	})
}

// TestAQueryCarriedIdGetsAKeyOfItsOwn is blocker 4 of the P0 backlog, and the
// reason it needs a NEW key rather than the resource's `name`.
//
// magic-modules and Discovery both say "name" and mean different things:
// magic-modules means the short id you choose, Discovery means what the server
// returns, which for these APIs is the full resource path, marked readOnly.
// Output wins over Required in attrs.go, so the create url is left with a
// placeholder nothing can fill and the type ships unable to create.
//
// declareCreateURLParameters will not declare over the Output-only attribute,
// and is right not to: one key cannot hold both values. Giving the id its own
// key -- named after the query parameter that carries it -- is the way out.
func TestAQueryCarriedIdGetsAKeyOfItsOwn(t *testing.T) {
	res, err := Build(createIDFixture(t))
	if err != nil {
		t.Fatal(err)
	}
	ty, ok := res.Catalog.Type("gcp.occupied")
	if !ok {
		t.Fatalf("gcp.occupied missing; catalog has %d types", len(res.Catalog.Types))
	}
	id := ty.Attributes["occupiedId"]
	if id == nil {
		t.Fatal("no occupiedId attribute: the create url's id has nothing to come from")
	}
	if id.Output {
		t.Error("occupiedId is output-only, so nothing can supply it")
	}
	if !id.CreateOnly {
		t.Error("occupiedId is not CreateOnly, so the runtime will expect GCP to echo back a value it never returns")
	}
	if !id.ForceNew {
		t.Error("occupiedId is not ForceNew, but changing it does not modify the resource, it names a different one")
	}
	if got, want := ty.CreateTemplate(), "projects/{{project}}/occupieds?occupiedId={{occupiedId}}"; got != want {
		t.Errorf("create template = %q, want %q", got, want)
	}
	if missing := ty.UnresolvedCreatePlaceholders(); len(missing) > 0 {
		t.Errorf("still uncreatable, needs %v", missing)
	}
	// The resource's own name must be left exactly as Discovery describes it.
	if n := ty.Attributes["name"]; n == nil || !n.Output {
		t.Errorf("name = %+v, want the untouched output-only full resource path", n)
	}
}

// TestATypeWhoseNameAlreadyWorksIsLeftAlone is the regression this cost me
// once. 22 types reached the same code with a placeholder that ALREADY
// resolved, because magic-modules models the id as a url parameter, and an
// earlier version rewrote all of their create templates for no reason. Nothing
// about such a type may change.
func TestATypeWhoseNameAlreadyWorksIsLeftAlone(t *testing.T) {
	res, err := Build(createIDFixture(t))
	if err != nil {
		t.Fatal(err)
	}
	ty, ok := res.Catalog.Type("gcp.already")
	if !ok {
		t.Fatalf("gcp.already missing; catalog has %d types", len(res.Catalog.Types))
	}
	if got, want := ty.CreateTemplate(), "projects/{{project}}/alreadies?alreadyId={{name}}"; got != want {
		t.Errorf("create template = %q, want it untouched as %q", got, want)
	}
	if ty.Attributes["alreadyId"] != nil {
		t.Error("an alreadyId attribute was synthesized for a type whose own name already fills the url")
	}
}

// TestOnlyTheQueryStringIsEverRewritten. A path segment is part of the
// resource's address; renaming one would change what the url addresses rather
// than where its id comes from.
func TestOnlyTheQueryStringIsEverRewritten(t *testing.T) {
	ty := &catalog.Type{
		Name:      "gcp.probe",
		CreateURL: "projects/{{project}}/things/{{name}}/subthings?subthingId={{name}}",
	}
	attrs := map[string]*catalog.Attr{"name": {Canonical: "name", Output: true}}
	create := &disco.Method{Parameters: map[string]*disco.Parameter{
		"subthingId": {Location: "query"},
	}}
	bindCreateQueryID(ty, attrs, create)
	if got, want := ty.CreateURL, "projects/{{project}}/things/{{name}}/subthings?subthingId={{subthingId}}"; got != want {
		t.Errorf("create url = %q, want the path untouched: %q", got, want)
	}
}

// TestAnUndeclaredQueryParameterIsRefused. Without this the generator would
// invent an input the API never offered, which is worse than the type being
// uncreatable: a create would go out carrying a parameter Google ignores, and
// the resource would come back under a name nobody asked for.
func TestAnUndeclaredQueryParameterIsRefused(t *testing.T) {
	ty := &catalog.Type{Name: "gcp.probe", CreateURL: "projects/{{project}}/things?thingId={{name}}"}
	attrs := map[string]*catalog.Attr{"name": {Canonical: "name", Output: true}}
	if bound := bindCreateQueryID(ty, attrs, &disco.Method{}); bound != nil {
		t.Errorf("bound %v from a create method that declares no such parameter", bound)
	}
	if attrs["thingId"] != nil {
		t.Error("an attribute was synthesized for a query parameter the API never published")
	}
}
