package gen

import (
	"testing"

	"github.com/infrena/infrena-provider-gcp/internal/catalog"
	"github.com/infrena/infrena-provider-gcp/internal/disco"
	"github.com/infrena/infrena/pkg/value"
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

// structuredIDDoc publishes two collections whose get is the bare "{+name}"
// capture, which is how most proto-first APIs address one resource. One has a
// clean pattern and one has a generic parent that no template can name.
func structuredIDDoc() string {
	return `{
  "name": "acme", "version": "v1", "rootUrl": "https://acme.googleapis.com/", "servicePath": "",
  "schemas": {
    "Db": {"id": "Db", "type": "object", "properties": {"name": {"type": "string", "readOnly": true}}},
    "Feed": {"id": "Feed", "type": "object", "properties": {"name": {"type": "string", "readOnly": true}}},
    "Operation": {"id": "Operation", "type": "object", "properties": {"status": {"type": "string"}}}
  },
  "resources": {"projects": {"resources": {"instances": {"resources": {
    "databases": {"methods": {
      "get": {"id": "a.d.get", "path": "v1/{+name}", "httpMethod": "GET", "response": {"$ref": "Db"},
        "parameters": {"name": {"type": "string", "location": "path", "pattern": "^projects/[^/]+/instances/[^/]+/databases/[^/]+$"}}},
      "create": {"id": "a.d.create", "path": "v1/{+parent}/databases", "httpMethod": "POST", "request": {"$ref": "Db"}, "response": {"$ref": "Db"},
        "parameters": {"parent": {"type": "string", "location": "path", "pattern": "^projects/[^/]+/instances/[^/]+$"}}},
      "delete": {"id": "a.d.delete", "path": "v1/{+name}", "httpMethod": "DELETE",
        "parameters": {"name": {"type": "string", "location": "path", "pattern": "^projects/[^/]+/instances/[^/]+/databases/[^/]+$"}}}
    }}}},
    "feeds": {"methods": {
      "get": {"id": "a.f.get", "path": "v1/{+name}", "httpMethod": "GET", "response": {"$ref": "Feed"},
        "parameters": {"name": {"type": "string", "location": "path", "pattern": "^[^/]+/[^/]+/feeds/[^/]+$"}}},
      "create": {"id": "a.f.create", "path": "v1/{+parent}/feeds", "httpMethod": "POST", "request": {"$ref": "Feed"}, "response": {"$ref": "Feed"},
        "parameters": {"parent": {"type": "string", "location": "path", "pattern": "^[^/]+/[^/]+$"}}},
      "delete": {"id": "a.f.delete", "path": "v1/{+name}", "httpMethod": "DELETE",
        "parameters": {"name": {"type": "string", "location": "path", "pattern": "^[^/]+/[^/]+/feeds/[^/]+$"}}}
    }}
  }}}
}`
}

// TestABareCaptureDocumentsTheRealIdShape. "{+name}" is what 88 types showed
// as their import format, which tells a user nothing about what to type. The
// API's own pattern says, and the placeholders are named after the
// collections in front of them -- "databases" as {database}, which the naming
// pass's singularizer would have spelled "databas".
//
// SelfLink is asserted untouched because the change is documentation only:
// a bare capture parses any id, and that must not become a refusal.
func TestABareCaptureDocumentsTheRealIdShape(t *testing.T) {
	res, err := Build(writeRefFixture(t, map[string]string{"schemas/acme.json": structuredIDDoc(), "mmv1/products/.keep": ""}))
	if err != nil {
		t.Fatal(err)
	}
	// The TYPE is named gcp.databas: the naming pass's singularizer is pinned
	// by gen/names.lock.json and gets "databases" wrong, which is why the real
	// catalog has gcp.firestore.databas. The placeholder below is spelled
	// correctly because it uses singularSegment instead, and the contrast is
	// the point of asserting both.
	db, ok := res.Catalog.Type("gcp.databas")
	if !ok {
		var got []string
		for _, ty := range res.Catalog.Types {
			got = append(got, ty.Name)
		}
		t.Fatalf("gcp.databas missing; catalog has %v", got)
	}
	if got, want := db.ImportFormat, "projects/{project}/instances/{instance}/databases/{database}"; got != want {
		t.Errorf("import format = %q, want %q", got, want)
	}
	if db.SelfLink != "{+name}" {
		t.Errorf("self_link = %q, want the capture left exactly as it was", db.SelfLink)
	}
	feed, ok := res.Catalog.Type("gcp.feed")
	if !ok {
		t.Fatal("gcp.feed missing")
	}
	if feed.ImportFormat != "{+name}" {
		t.Errorf("a generic-parent pattern was given a shape: %q; a template cannot name a parent that may be any collection", feed.ImportFormat)
	}
}

// TestARepeatedPlaceholderIsNotAShape. A template that names two segments the
// same cannot be filled by anyone, so it is refused rather than documented.
func TestARepeatedPlaceholderIsNotAShape(t *testing.T) {
	col := disco.Collection{Methods: map[string]*disco.Method{"get": {Path: "v1/{+name}",
		Parameters: map[string]*disco.Parameter{"name": {Location: "path", Pattern: "^widgets/[^/]+/widgets/[^/]+$"}}}}}
	if got := structuredIDTemplate(col, "{+name}"); got != "" {
		t.Errorf("got %q, want no shape", got)
	}
}

// createParamDoc publishes four collections created by POST to the collection
// with the new resource's id as a query parameter -- AIP-133's shape -- where
// nothing in magic-modules supplies a create url to carry it.
//
//	gadgets  the ordinary case: gadgetId, Required, and a full-path name
//	optis    Cloud Run's case: the id is optional
//	clashes  the resource has an output field of its own called clashId
//	reqs     the only query parameter is requestId, which names no resource
func createParamDoc() string {
	coll := func(plural, schema, param, desc string) string {
		return `"` + plural + `": {"methods": {
        "get": {"id": "a.` + plural + `.get", "path": "v1/{+name}", "httpMethod": "GET", "response": {"$ref": "` + schema + `"},
          "parameters": {"name": {"type": "string", "location": "path", "pattern": "^projects/[^/]+/` + plural + `/[^/]+$"}}},
        "create": {"id": "a.` + plural + `.create", "path": "v1/projects/{project}/` + plural + `", "httpMethod": "POST",
          "request": {"$ref": "` + schema + `"}, "response": {"$ref": "` + schema + `"},
          "parameters": {"project": {"type": "string", "location": "path"}, "` + param + `": {"type": "string", "location": "query", "description": "` + desc + `"}}},
        "delete": {"id": "a.` + plural + `.delete", "path": "v1/{+name}", "httpMethod": "DELETE",
          "parameters": {"name": {"type": "string", "location": "path", "pattern": "^projects/[^/]+/` + plural + `/[^/]+$"}}}
      }}`
	}
	schema := func(n string, extra string) string {
		return `"` + n + `": {"id": "` + n + `", "type": "object", "properties": {
      "name": {"type": "string", "description": "Identifier. The full resource name."},
      "size": {"type": "integer", "format": "int64"}` + extra + `}}`
	}
	return `{"name": "acme", "version": "v1", "rootUrl": "https://acme.googleapis.com/", "servicePath": "",
  "schemas": {` + schema("Gadget", "") + `, ` + schema("Opti", "") + `,
    ` + schema("Clash", `, "clashId": {"type": "string", "readOnly": true, "description": "Output only. Server id."}`) + `,
    ` + schema("Req", "") + `},
  "resources": {"projects": {"resources": {
    ` + coll("gadgets", "Gadget", "gadgetId", "Required. The ID to use for the gadget, which will become the final component of its name.") + `,
    ` + coll("optis", "Opti", "optiId", "Optional. The unique identifier. If not provided, the server will generate one.") + `,
    ` + coll("clashes", "Clash", "clashId", "Required. The ID to use.") + `,
    ` + coll("reqs", "Req", "requestId", "An optional request id for idempotency.") + `
  }}}}`
}

func createParamBuild(t *testing.T) *Result {
	t.Helper()
	res, err := Build(writeRefFixture(t, map[string]string{"schemas/acme.json": createParamDoc(), "mmv1/products/.keep": ""}))
	if err != nil {
		t.Fatal(err)
	}
	return res
}

// TestACreateSendsTheIdTheAPIAsksFor is 21 shipping types, found by a live
// Cloud Run job: the create url carried no id, so a Required id was never
// sent, and where it was optional the name went in the body, which Cloud Run
// refuses ("job.name must be empty on CreateJobRequest"). The id now gets its
// own create-only attribute, the url names it, and `name` -- Google's to
// assign from the parent and the id -- is no longer sent.
func TestACreateSendsTheIdTheAPIAsksFor(t *testing.T) {
	ty, ok := createParamBuild(t).Catalog.Type("gcp.gadget")
	if !ok {
		t.Fatal("gcp.gadget missing")
	}
	if got, want := ty.CreateTemplate(), "projects/{project}/gadgets?gadgetId={{gadgetId}}"; got != want {
		t.Errorf("create template = %q, want %q", got, want)
	}
	id := ty.Attributes["gadgetId"]
	if id == nil || !id.CreateOnly || !id.ForceNew || id.Output {
		t.Fatalf("gadgetId = %+v, want a settable create-only ForceNew attribute", id)
	}
	if !id.Required {
		t.Error("gadgetId is not Required, though the API's own description says so")
	}
	if n := ty.Attributes["name"]; n == nil || !n.Output {
		t.Errorf("name = %+v, want output-only: Google assigns it from the parent and the id", n)
	}
	if missing := ty.UnresolvedCreatePlaceholders(); len(missing) > 0 {
		t.Errorf("still uncreatable, needs %v", missing)
	}
}

// TestAnOptionalIdIsNotRequired. Cloud Run generates an id when none is
// given, so leaving it out is the user's choice, not an error.
func TestAnOptionalIdIsNotRequired(t *testing.T) {
	ty, ok := createParamBuild(t).Catalog.Type("gcp.opti")
	if !ok || ty.Attributes["optiId"] == nil {
		t.Fatalf("gcp.opti has no optiId: %v", ty)
	}
	if ty.Attributes["optiId"].Required {
		t.Error("an optional id was made Required")
	}
}

// TestNeitherACollidingFieldNorAnUnrelatedParameterIsTouched. A resource
// field of the same name is never shadowed -- gcp.target's own output-only
// targetId is why -- and requestId names no resource, so it is left alone
// because the rule is the parameter named after the resource, not any
// parameter ending in Id.
func TestNeitherACollidingFieldNorAnUnrelatedParameterIsTouched(t *testing.T) {
	res := createParamBuild(t)
	clash, _ := res.Catalog.Type("gcp.clash")
	if clash == nil || !clash.Attributes["clashId"].Output || clash.Attributes["clashId"].CreateOnly {
		t.Errorf("the resource's own output field was shadowed: %+v", clash.Attributes["clashId"])
	}
	req, _ := res.Catalog.Type("gcp.req")
	if req == nil || req.Attributes["requestId"] != nil || req.CreateTemplate() != "projects/{project}/reqs" {
		t.Errorf("requestId was taken for a resource id: template %q", req.CreateTemplate())
	}
}

// TestAShortNamedTypeSendsItsIdFromName is gcp.target. Its create needs
// "?targetId=", and the resource has an output-only targetId of its own, so a
// second input under that key is refused. But its id template ends in
// "/{{name}}", where `name` already IS the short id, so name fills the
// parameter -- magic-modules' own shape for these types -- and there is
// nothing to collide.
func TestAShortNamedTypeSendsItsIdFromName(t *testing.T) {
	res, err := Build(writeRefFixture(t, map[string]string{
		"schemas/acme.json": createParamDoc(),
		"mmv1/products/acme/Clash.yaml": `name: Clash
description: its create id collides with a field of its own.
base_url: projects/{{project}}/clashes
self_link: projects/{{project}}/clashes/{{name}}
properties:
  - name: name
    type: String
    required: true
`,
	}))
	if err != nil {
		t.Fatal(err)
	}
	ty, ok := res.Catalog.Type("gcp.clash")
	if !ok {
		t.Fatal("gcp.clash missing")
	}
	if got, want := ty.CreateTemplate(), "projects/{{project}}/clashes?clashId={{name}}"; got != want {
		t.Errorf("create template = %q, want %q", got, want)
	}
	if n := ty.Attributes["name"]; n == nil || n.Output {
		t.Errorf("name = %+v, want the settable short id", n)
	}
	if id := ty.Attributes["clashId"]; id == nil || !id.Output || id.CreateOnly {
		t.Errorf("the resource's own clashId was disturbed: %+v", id)
	}
}

// TestAPreCreateTokenIsBoundToTheQueryParameterItStandsFor. compute's
// NodeGroup create url carries "?initialNodeCount=PRE_CREATE_REPLACE_ME" for
// Terraform's pre_create to fill; Discovery publishes initialNodeCount as a
// required integer query parameter, so it becomes a create-only attribute
// the user writes. A token Discovery does not publish is left, and the type
// is refused (TestACreateURLCarryingAPreCreateTokenIsRefused).
func TestAPreCreateTokenIsBoundToTheQueryParameterItStandsFor(t *testing.T) {
	ty := &catalog.Type{CreateURL: "projects/{{project}}/zones/{{zone}}/nodeGroups?initialNodeCount=PRE_CREATE_REPLACE_ME"}
	attrs := map[string]*catalog.Attr{"name": {Canonical: "name", Kind: value.KindString}}
	create := &disco.Method{Parameters: map[string]*disco.Parameter{
		"initialNodeCount": {Type: "integer", Format: "int32", Location: "query", Required: true, Description: "Initial count of nodes."}}}
	fillPreCreateTokens(ty, attrs, create)
	if want := "projects/{{project}}/zones/{{zone}}/nodeGroups?initialNodeCount={{initialNodeCount}}"; ty.CreateURL != want {
		t.Errorf("create url = %q, want %q", ty.CreateURL, want)
	}
	a := attrs["initialNodeCount"]
	if a == nil || !a.Required || !a.CreateOnly || !a.ForceNew || a.Kind != value.KindInt {
		t.Errorf("initialNodeCount = %+v, want a required create-only integer", a)
	}
}
