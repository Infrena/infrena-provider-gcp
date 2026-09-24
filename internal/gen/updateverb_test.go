package gen

import (
	"os"
	"strings"
	"testing"
)

// updateVerbDoc is one Discovery document carrying six collections that differ
// only in how they publish an update. None of them has a magic-modules resource,
// which is the point: these are the types whose update verb has to come from
// Discovery or from nowhere.
//
//   - patchables   PATCH with an updateMask query parameter -> updatable, masked
//   - unmaskeds    PATCH with no updateMask parameter       -> updatable, unmasked
//   - putonlies    PUT only                                 -> NOT updatable
//   - elsewheres   PATCH at a path the get does not use     -> NOT updatable
//   - restricteds  PATCH Google documents as partial        -> NOT updatable, unless
//     an overlay allowlist says
//     which fields it takes
//   - wrappeds     PATCH whose request is an AIP-134 envelope -> updatable, wrapped
func updateVerbDoc() string {
	return `{
  "name": "acme",
  "version": "v1",
  "rootUrl": "https://acme.googleapis.com/",
  "servicePath": "",
  "schemas": {
    "Thing": {"id": "Thing", "type": "object", "properties": {
      "name": {"type": "string", "description": "The name."},
      "size": {"type": "integer", "format": "int64", "description": "How big."}
    }},
    "UpdateThingRequest": {"id": "UpdateThingRequest", "type": "object", "properties": {
      "thing": {"$ref": "Thing", "description": "The updated thing."},
      "updateMask": {"type": "string", "format": "google-fieldmask", "description": "Fields to update."}
    }},
    "Operation": {"id": "Operation", "type": "object", "properties": {"status": {"type": "string"}}}
  },
  "resources": {
    "projects": {"resources": {
      "patchables": {"methods": {
        "get": {"id": "a.p.get", "path": "projects/{project}/patchables/{id}", "httpMethod": "GET", "response": {"$ref": "Thing"}},
        "insert": {"id": "a.p.insert", "path": "projects/{project}/patchables", "httpMethod": "POST", "request": {"$ref": "Thing"}, "response": {"$ref": "Operation"}},
        "delete": {"id": "a.p.delete", "path": "projects/{project}/patchables/{id}", "httpMethod": "DELETE", "response": {"$ref": "Operation"}},
        "patch": {"id": "a.p.patch", "path": "projects/{project}/patchables/{id}", "httpMethod": "PATCH", "request": {"$ref": "Thing"}, "response": {"$ref": "Operation"},
          "parameters": {"updateMask": {"type": "string", "location": "query"}}}
      }},
      "unmaskeds": {"methods": {
        "get": {"id": "a.u.get", "path": "projects/{project}/unmaskeds/{id}", "httpMethod": "GET", "response": {"$ref": "Thing"}},
        "insert": {"id": "a.u.insert", "path": "projects/{project}/unmaskeds", "httpMethod": "POST", "request": {"$ref": "Thing"}, "response": {"$ref": "Operation"}},
        "delete": {"id": "a.u.delete", "path": "projects/{project}/unmaskeds/{id}", "httpMethod": "DELETE", "response": {"$ref": "Operation"}},
        "patch": {"id": "a.u.patch", "path": "projects/{project}/unmaskeds/{id}", "httpMethod": "PATCH", "request": {"$ref": "Thing"}, "response": {"$ref": "Operation"}}
      }},
      "putonlies": {"methods": {
        "get": {"id": "a.o.get", "path": "projects/{project}/putonlies/{id}", "httpMethod": "GET", "response": {"$ref": "Thing"}},
        "insert": {"id": "a.o.insert", "path": "projects/{project}/putonlies", "httpMethod": "POST", "request": {"$ref": "Thing"}, "response": {"$ref": "Operation"}},
        "delete": {"id": "a.o.delete", "path": "projects/{project}/putonlies/{id}", "httpMethod": "DELETE", "response": {"$ref": "Operation"}},
        "update": {"id": "a.o.update", "path": "projects/{project}/putonlies/{id}", "httpMethod": "PUT", "request": {"$ref": "Thing"}, "response": {"$ref": "Operation"}}
      }},
      "elsewheres": {"methods": {
        "get": {"id": "a.e.get", "path": "projects/{project}/elsewheres/{id}", "httpMethod": "GET", "response": {"$ref": "Thing"}},
        "insert": {"id": "a.e.insert", "path": "projects/{project}/elsewheres", "httpMethod": "POST", "request": {"$ref": "Thing"}, "response": {"$ref": "Operation"}},
        "delete": {"id": "a.e.delete", "path": "projects/{project}/elsewheres/{id}", "httpMethod": "DELETE", "response": {"$ref": "Operation"}},
        "patch": {"id": "a.e.patch", "path": "projects/{project}/elsewheres/{id}:updateConfig", "httpMethod": "PATCH", "request": {"$ref": "Thing"}, "response": {"$ref": "Operation"}}
      }},
      "restricteds": {"methods": {
        "get": {"id": "a.r.get", "path": "projects/{project}/restricteds/{id}", "httpMethod": "GET", "response": {"$ref": "Thing"}},
        "insert": {"id": "a.r.insert", "path": "projects/{project}/restricteds", "httpMethod": "POST", "request": {"$ref": "Thing"}, "response": {"$ref": "Operation"}},
        "delete": {"id": "a.r.delete", "path": "projects/{project}/restricteds/{id}", "httpMethod": "DELETE", "response": {"$ref": "Operation"}},
        "patch": {"id": "a.r.patch", "path": "projects/{project}/restricteds/{id}", "httpMethod": "PATCH", "request": {"$ref": "Thing"}, "response": {"$ref": "Operation"},
          "description": "Patches the specified thing with the data included in the request. Only size can be modified."}
      }},
      "wrappeds": {"methods": {
        "get": {"id": "a.w.get", "path": "projects/{project}/wrappeds/{id}", "httpMethod": "GET", "response": {"$ref": "Thing"}},
        "insert": {"id": "a.w.insert", "path": "projects/{project}/wrappeds", "httpMethod": "POST", "request": {"$ref": "Thing"}, "response": {"$ref": "Operation"}},
        "delete": {"id": "a.w.delete", "path": "projects/{project}/wrappeds/{id}", "httpMethod": "DELETE", "response": {"$ref": "Operation"}},
        "patch": {"id": "a.w.patch", "path": "projects/{project}/wrappeds/{id}", "httpMethod": "PATCH", "request": {"$ref": "UpdateThingRequest"}, "response": {"$ref": "Operation"}}
      }}
    }}
  }
}`
}

// TestTheUpdateVerbComesFromDiscoveryWhenMagicModulesIsSilent is the whole of
// the defect this file exists for. UpdateVerb used to be read from
// magic-modules and from nowhere else, and magic-modules leaves `update_verb:`
// off most resources because it has its own default. The type then shipped with
// no update path at all, which patch.go turns into "every change replaces the
// resource" -- measured on 2026-09-23 as 154 shipping types, including
// gcp.storage.bucket, gcp.network, gcp.subnetwork and gcp.compute.instance,
// every one of which Google publishes a PATCH for. Replacing a bucket takes its
// objects with it.
func TestTheUpdateVerbComesFromDiscoveryWhenMagicModulesIsSilent(t *testing.T) {
	res, err := Build(writeRefFixture(t, map[string]string{"schemas/acme.json": updateVerbDoc(), "mmv1/products/.keep": ""}))
	if err != nil {
		t.Fatal(err)
	}
	ty, ok := res.Catalog.Type("gcp.patchable")
	if !ok {
		t.Fatalf("gcp.patchable missing; catalog has %d types", len(res.Catalog.Types))
	}
	if ty.UpdateVerb != "PATCH" {
		t.Errorf("UpdateVerb = %q, want PATCH: the collection publishes one and nothing else supplies it", ty.UpdateVerb)
	}
	if !ty.UpdateMask {
		t.Error("UpdateMask = false, but the patch method declares an updateMask query parameter")
	}
}

// TestAPatchWithNoUpdateMaskParameterIsStillUpdatable guards the half of the
// derivation that is easy to collapse into the other: the mask is a separate
// fact from the verb. Fifteen of the catalog's already-updatable types PATCH
// without one, so deriving "PATCH implies a mask" would send a query parameter
// those APIs never asked for.
func TestAPatchWithNoUpdateMaskParameterIsStillUpdatable(t *testing.T) {
	res, err := Build(writeRefFixture(t, map[string]string{"schemas/acme.json": updateVerbDoc(), "mmv1/products/.keep": ""}))
	if err != nil {
		t.Fatal(err)
	}
	ty, ok := res.Catalog.Type("gcp.unmasked")
	if !ok {
		t.Fatalf("gcp.unmasked missing; catalog has %d types", len(res.Catalog.Types))
	}
	if ty.UpdateVerb != "PATCH" {
		t.Errorf("UpdateVerb = %q, want PATCH", ty.UpdateVerb)
	}
	if ty.UpdateMask {
		t.Error("UpdateMask = true, but the patch method declares no updateMask parameter")
	}
}

// TestAPutOnlyCollectionStaysNonUpdatable is the guard that makes the whole
// change safe, and it is not a detail. BuildMask emits a PARTIAL body -- only
// the attributes that changed. PUT replaces the resource with the body it is
// given, so sending a partial body to a PUT endpoint clears every field the
// diff left out. compute's instances collection publishes `update` (PUT) and no
// patch at all, so a derivation that accepted PUT would quietly start wiping
// virtual machines.
func TestAPutOnlyCollectionStaysNonUpdatable(t *testing.T) {
	res, err := Build(writeRefFixture(t, map[string]string{"schemas/acme.json": updateVerbDoc(), "mmv1/products/.keep": ""}))
	if err != nil {
		t.Fatal(err)
	}
	ty, ok := res.Catalog.Type("gcp.putonly")
	if !ok {
		t.Fatalf("gcp.putonly missing; catalog has %d types", len(res.Catalog.Types))
	}
	if ty.UpdateVerb != "" {
		t.Errorf("UpdateVerb = %q, want empty: a PUT takes a whole resource and BuildMask sends a diff", ty.UpdateVerb)
	}
}

// TestAWrappedPatchRequestIsSentAsAnEnvelope is AIP-134's UpdateXRequest
// shape. pubsub's topics.patch takes UpdateTopicRequest{topic, updateMask}, so
// the resource is wrapped and the mask is a REQUIRED field of the body rather
// than a query parameter; a bare resource is rejected.
//
// This was refused outright until the runtime could send that shape, which is
// what made Pub/Sub a P0 blocker. The mask field is found by its schema
// FORMAT -- google-fieldmask, the protobuf FieldMask type surviving into
// Discovery -- and not by its name, because six of the eight collections that
// use this shape say "updateMask" and spanner's two say "fieldMask".
func TestAWrappedPatchRequestIsSentAsAnEnvelope(t *testing.T) {
	res, err := Build(writeRefFixture(t, map[string]string{"schemas/acme.json": updateVerbDoc(), "mmv1/products/.keep": ""}))
	if err != nil {
		t.Fatal(err)
	}
	ty, ok := res.Catalog.Type("gcp.wrapped")
	if !ok {
		t.Fatalf("gcp.wrapped missing; catalog has %d types", len(res.Catalog.Types))
	}
	if ty.UpdateVerb != "PATCH" {
		t.Errorf("UpdateVerb = %q, want PATCH: the envelope is understood now", ty.UpdateVerb)
	}
	if ty.UpdateWrapper != "thing" {
		t.Errorf("UpdateWrapper = %q, want \"thing\"", ty.UpdateWrapper)
	}
	if ty.UpdateMaskField != "updateMask" {
		t.Errorf("UpdateMaskField = %q, want \"updateMask\"", ty.UpdateMaskField)
	}
	if ty.UpdateMask {
		t.Error("UpdateMask is set, so the mask would ALSO go on the query string")
	}
}

// TestAnEnvelopeWithNoFieldMaskIsRefused. A request schema that is not the
// resource and carries no google-fieldmask is a shape this provider does not
// understand, and a request sent in a shape we only half recognise is one whose
// effect we cannot predict.
func TestAnEnvelopeWithNoFieldMaskIsRefused(t *testing.T) {
	doc := strings.Replace(updateVerbDoc(),
		`"updateMask": {"type": "string", "format": "google-fieldmask", "description": "Fields to update."}`,
		`"updateMask": {"type": "string", "description": "Fields to update."}`, 1)
	res, err := Build(writeRefFixture(t, map[string]string{"schemas/acme.json": doc, "mmv1/products/.keep": ""}))
	if err != nil {
		t.Fatal(err)
	}
	ty, ok := res.Catalog.Type("gcp.wrapped")
	if !ok {
		t.Fatalf("gcp.wrapped missing; catalog has %d types", len(res.Catalog.Types))
	}
	if ty.UpdateVerb != "" {
		t.Errorf("UpdateVerb = %q, want empty: nothing in the envelope says where the mask goes", ty.UpdateVerb)
	}
}

// TestMagicModulesStillWinsWhenItDeclaresAnUpdateVerb keeps the derivation
// additive. The 86 types that were updatable before this change got their verb
// and their mask from magic-modules, and those are human-curated decisions that
// a Discovery guess must not overwrite -- several of them deliberately PATCH
// without a mask, and one names an update_url the collection's own patch path
// does not.
// The fixture is built so the two sources DISAGREE, which is the only way this
// test can fail: magic-modules declares a mask, and the `unmaskeds` collection
// it matches publishes a patch with no updateMask parameter. If the derivation
// ever runs in front of magic-modules, the mask silently flips to false. An
// earlier version of this test used a collection with no patch method at all
// and passed under exactly the sabotage it existed to catch.
func TestMagicModulesStillWinsWhenItDeclaresAnUpdateVerb(t *testing.T) {
	res, err := Build(writeRefFixture(t, map[string]string{
		"schemas/acme.json": updateVerbDoc(),
		"mmv1/products/acme/Unmasked.yaml": `name: Unmasked
description: magic-modules says masked; Discovery says otherwise.
base_url: projects/{{project}}/unmaskeds
self_link: projects/{{project}}/unmaskeds/{{name}}
update_verb: PATCH
update_mask: true
properties:
  - name: name
    type: String
    required: true
`,
	}))
	if err != nil {
		t.Fatal(err)
	}
	ty, ok := res.Catalog.Type("gcp.unmasked")
	if !ok {
		t.Fatalf("gcp.unmasked missing; catalog has %d types", len(res.Catalog.Types))
	}
	if ty.UpdateVerb != "PATCH" {
		t.Errorf("UpdateVerb = %q, want PATCH from magic-modules", ty.UpdateVerb)
	}
	if !ty.UpdateMask {
		t.Error("UpdateMask = false: the Discovery derivation overwrote a human-curated magic-modules decision")
	}
}

// TestAPatchAtADifferentPathStaysNonUpdatable covers the guard that had no
// coverage at all until a deliberate sabotage of it passed. Update addresses
// the resource through SelfLink, and SelfLink is the collection's `get` path
// (build.go fills it from there whenever magic-modules supplies none). A patch
// published somewhere else — Google's ":verb" custom-method shape, for instance
// — is a different endpoint, and deriving a verb from it would send every
// update to a url the method does not live at.
func TestAPatchAtADifferentPathStaysNonUpdatable(t *testing.T) {
	res, err := Build(writeRefFixture(t, map[string]string{"schemas/acme.json": updateVerbDoc(), "mmv1/products/.keep": ""}))
	if err != nil {
		t.Fatal(err)
	}
	ty, ok := res.Catalog.Type("gcp.elsewhere")
	if !ok {
		t.Fatalf("gcp.elsewhere missing; catalog has %d types", len(res.Catalog.Types))
	}
	if ty.UpdateVerb != "" {
		t.Errorf("UpdateVerb = %q, want empty: the patch lives at %q, not at the resource's own path",
			ty.UpdateVerb, "projects/{project}/elsewheres/{id}:updateConfig")
	}
}

// overlayWith writes a fixture whose overlay carries an extra block, so the
// `patchable:` map can be exercised through the real LoadOverlay rather than by
// constructing an Overlay in memory -- the yaml spelling is half of what this
// feature is.
func overlayWith(t *testing.T, extra string) Inputs {
	t.Helper()
	in := writeRefFixture(t, map[string]string{"schemas/acme.json": updateVerbDoc(), "mmv1/products/.keep": ""})
	if err := os.WriteFile(in.OverlayPath, []byte("rulings: {}\naliases: {}\ndiscover_default: []\n"+extra), 0o644); err != nil {
		t.Fatal(err)
	}
	return in
}

// TestARestrictedPatchIsRefusedWithoutAnAllowlist is the guard that keeps this
// provider from trading a convergent failure for a non-convergent one. Google
// publishes the whole resource as the patch request schema and then limits it
// in prose: compute.networks.patch accepts only routingConfig, while
// gcp.network declares five settable non-ForceNew attributes. Deriving a verb
// from the schema alone gives four fields a patch the API drops on the floor,
// so the plan proposes the same change for ever. Replacement is destructive and
// loud; a patch that does nothing is neither.
func TestARestrictedPatchIsRefusedWithoutAnAllowlist(t *testing.T) {
	res, err := Build(writeRefFixture(t, map[string]string{"schemas/acme.json": updateVerbDoc(), "mmv1/products/.keep": ""}))
	if err != nil {
		t.Fatal(err)
	}
	ty, ok := res.Catalog.Type("gcp.restricted")
	if !ok {
		t.Fatalf("gcp.restricted missing; catalog has %d types", len(res.Catalog.Types))
	}
	if ty.UpdateVerb != "" {
		t.Errorf("UpdateVerb = %q, want empty: the API says only some fields are patchable", ty.UpdateVerb)
	}
	var found bool
	for _, u := range res.Unpatchable {
		if u.Type == "gcp.restricted" {
			found = true
			if !strings.Contains(u.Says, "Only size can be modified") {
				t.Errorf("the record does not quote the API: %q", u.Says)
			}
		}
	}
	if !found {
		t.Error("gcp.restricted was refused an update and recorded nowhere; a silent refusal is indistinguishable from an API with no patch")
	}
}

// TestAnAllowlistAdmitsTheTypeAndForceNewsTheRest is the other half: a human
// read what the API says and wrote the fields down, so the listed field is
// patched and every other settable one replaces the resource -- which is what
// the API does with it anyway.
func TestAnAllowlistAdmitsTheTypeAndForceNewsTheRest(t *testing.T) {
	res, err := Build(overlayWith(t, `
patchable:
  gcp.restricted:
    fields: [size]
    note: the fixture's patch description says only size can be modified
`))
	if err != nil {
		t.Fatal(err)
	}
	ty, ok := res.Catalog.Type("gcp.restricted")
	if !ok {
		t.Fatalf("gcp.restricted missing; catalog has %d types", len(res.Catalog.Types))
	}
	if ty.UpdateVerb != "PATCH" {
		t.Fatalf("UpdateVerb = %q, want PATCH once a human listed the fields", ty.UpdateVerb)
	}
	size, name := ty.Attributes["size"], ty.Attributes["name"]
	if size == nil || name == nil {
		t.Fatalf("fixture attributes missing: size=%v name=%v", size, name)
	}
	if size.ForceNew {
		t.Error("size is ForceNew, but it is the one field the API does patch")
	}
	if !name.ForceNew {
		t.Error("name is not ForceNew, so a change to it would be sent as a patch the API drops")
	}
	for _, u := range res.Unpatchable {
		if u.Type == "gcp.restricted" {
			t.Error("gcp.restricted is still recorded as replaced-not-patched after an allowlist admitted it")
		}
	}
}

// TestAPatchableEntryWithoutASourceIsRefused. A list of field names with
// nothing behind it cannot be reviewed, and this one governs whether a change
// replaces a resource -- the same reason a ruling's note is mandatory.
func TestAPatchableEntryWithoutASourceIsRefused(t *testing.T) {
	for name, extra := range map[string]string{
		"no note":   "\npatchable:\n  gcp.restricted:\n    fields: [size]\n",
		"no fields": "\npatchable:\n  gcp.restricted:\n    note: says nothing about which fields\n",
	} {
		if _, err := Build(overlayWith(t, extra)); err == nil {
			t.Errorf("%s: accepted, want a refusal", name)
		}
	}
}

// collectionPatchDoc publishes compute's autoscaler shape: the patch lives on
// the COLLECTION and names the resource in a query parameter, so its path is
// not the get's and never can be.
func collectionPatchDoc() string {
	return `{
  "name": "acme", "version": "v1", "rootUrl": "https://acme.googleapis.com/", "servicePath": "",
  "schemas": {
    "Scaler": {"id": "Scaler", "type": "object", "properties": {
      "name": {"type": "string", "description": "The name."},
      "size": {"type": "integer", "format": "int64", "description": "How big."}
    }},
    "Operation": {"id": "Operation", "type": "object", "properties": {"status": {"type": "string"}}}
  },
  "resources": {"projects": {"resources": {"scalers": {"methods": {
    "get": {"id": "a.s.get", "path": "projects/{project}/scalers/{scaler}", "httpMethod": "GET", "response": {"$ref": "Scaler"}},
    "insert": {"id": "a.s.insert", "path": "projects/{project}/scalers", "httpMethod": "POST", "request": {"$ref": "Scaler"}, "response": {"$ref": "Operation"}},
    "delete": {"id": "a.s.delete", "path": "projects/{project}/scalers/{scaler}", "httpMethod": "DELETE", "response": {"$ref": "Operation"}},
    "patch": {"id": "a.s.patch", "path": "projects/{project}/scalers", "httpMethod": "PATCH", "request": {"$ref": "Scaler"}, "response": {"$ref": "Operation"},
      "parameters": {"scaler": {"type": "string", "location": "query", "description": "Name of the scaler to patch."}}}
  }}}}}
}`
}

func scalerFixture(t *testing.T, updateURL string) Inputs {
	t.Helper()
	return writeRefFixture(t, map[string]string{
		"schemas/acme.json": collectionPatchDoc(),
		"mmv1/products/acme/Scaler.yaml": `name: Scaler
description: patched on the collection, named by a query parameter.
base_url: projects/{{project}}/scalers
self_link: projects/{{project}}/scalers/{{name}}
update_url: '` + updateURL + `'
properties:
  - name: name
    type: String
  - name: size
    type: Integer
`,
	})
}

// TestADeclaredUpdateUrlIsAcceptedWhenItMatchesThePatch. magic-modules defaults
// update_verb to PUT, so a resource declaring an update_url and no verb wrote
// that url for a PUT, and pairing it with an inferred PATCH would be a guess.
// Checking it against the method turns the guess into a question the documents
// answer -- and for compute's two autoscalers the answer matters, because their
// patch is published on the collection and that url is the only address a PATCH
// can go to.
func TestADeclaredUpdateUrlIsAcceptedWhenItMatchesThePatch(t *testing.T) {
	res, err := Build(scalerFixture(t, "projects/{{project}}/scalers?scaler={{name}}"))
	if err != nil {
		t.Fatal(err)
	}
	ty, ok := res.Catalog.Type("gcp.scaler")
	if !ok {
		t.Fatalf("gcp.scaler missing; catalog has %d types", len(res.Catalog.Types))
	}
	if ty.UpdateVerb != "PATCH" {
		t.Errorf("UpdateVerb = %q, want PATCH: the declared url is exactly where the patch lives", ty.UpdateVerb)
	}
}

// TestADeclaredUpdateUrlIsRefusedWhenThePatchDoesNotAgree covers both halves of
// the check. A url naming a query parameter the patch never declares, and a url
// whose path is not the patch's, are each a different request from the one the
// API publishes.
func TestADeclaredUpdateUrlIsRefusedWhenThePatchDoesNotAgree(t *testing.T) {
	for name, url := range map[string]string{
		"undeclared query parameter": "projects/{{project}}/scalers?autoscaler={{name}}",
		"a path the patch is not at": "projects/{{project}}/otherthings?scaler={{name}}",
	} {
		res, err := Build(scalerFixture(t, url))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		ty, ok := res.Catalog.Type("gcp.scaler")
		if !ok {
			t.Fatalf("%s: gcp.scaler missing", name)
		}
		if ty.UpdateVerb != "" {
			t.Errorf("%s: UpdateVerb = %q, want empty", name, ty.UpdateVerb)
		}
	}
}

// TestAnImmutableResourcePatchesOnlyWhatItNames. magic-modules' resource-level
// `immutable:` says nothing changes in place except the fields that name their
// own update. Both collections here publish a PATCH, as compute's proxies and
// prefixes do. One resource names a field it patches, so that field is
// patched and the rest replace. The other names only methods of its own, a
// PATCH to updateSize (compute's instance does this with
// updateShieldedInstanceConfig) and a POST to setLabels. Neither is the update
// this provider sends, so it is replaced on any change and the warnings file
// says why.
func TestAnImmutableResourcePatchesOnlyWhatItNames(t *testing.T) {
	res, err := Build(writeRefFixture(t, map[string]string{
		"schemas/acme.json": updateVerbDoc(),
		"mmv1/products/acme/Patchable.yaml": `name: Patchable
base_url: projects/{{project}}/patchables
self_link: projects/{{project}}/patchables/{{name}}
immutable: true
properties:
  - name: name
    type: String
    required: true
  - name: size
    type: Integer
    update_url: projects/{{project}}/patchables/{{name}}
    update_verb: PATCH
`,
		"mmv1/products/acme/Unmasked.yaml": `name: Unmasked
base_url: projects/{{project}}/unmaskeds
self_link: projects/{{project}}/unmaskeds/{{name}}
immutable: true
properties:
  - name: name
    type: String
    required: true
  - name: size
    type: Integer
    update_url: projects/{{project}}/unmaskeds/{{name}}/updateSize
    update_verb: PATCH
  - name: labels
    type: KeyValueLabels
    update_url: projects/{{project}}/unmaskeds/{{name}}/setLabels
    update_verb: POST
`,
	}))
	if err != nil {
		t.Fatal(err)
	}
	p, ok := res.Catalog.Type("gcp.patchable")
	if !ok {
		t.Fatalf("gcp.patchable missing; catalog has %d types", len(res.Catalog.Types))
	}
	if p.UpdateVerb != "PATCH" {
		t.Errorf("gcp.patchable UpdateVerb = %q, want PATCH for the one field it names", p.UpdateVerb)
	}
	if p.Attributes["size"].ForceNew {
		t.Error("size is ForceNew, but magic-modules patches it")
	}
	if !p.Attributes["name"].ForceNew {
		t.Error("name is not ForceNew on an immutable resource that does not patch it")
	}
	u, ok := res.Catalog.Type("gcp.unmasked")
	if !ok {
		t.Fatalf("gcp.unmasked missing; catalog has %d types", len(res.Catalog.Types))
	}
	if u.UpdateVerb != "" {
		t.Errorf("gcp.unmasked UpdateVerb = %q, want none: its only updates are its own methods", u.UpdateVerb)
	}
	var recorded bool
	for _, r := range res.Unpatchable {
		recorded = recorded || r.Type == "gcp.unmasked"
		if r.Type == "gcp.patchable" {
			t.Error("gcp.patchable is recorded as replaced, but it patches size")
		}
	}
	if !recorded {
		t.Error("gcp.unmasked lost its update and nothing records why")
	}
}
