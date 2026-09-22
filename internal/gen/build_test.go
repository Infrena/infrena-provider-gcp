package gen

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/infrena/infrena-provider-gcp/internal/catalog"
	"github.com/infrena/infrena-provider-gcp/internal/disco"
)

// buildFixture assembles a minimal input tree: one Discovery document, one
// magic-modules product, one overlay.
func buildFixture(t *testing.T) Inputs {
	t.Helper()
	dir := t.TempDir()
	schemas := filepath.Join(dir, "schemas")
	products := filepath.Join(dir, "mmv1", "products", "tiny")
	for _, d := range []string{schemas, products} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	copyFile(t, "testdata/tiny.json", filepath.Join(schemas, "tiny.json"))
	copyFile(t, "testdata/Widget.yaml", filepath.Join(products, "Widget.yaml"))
	copyFile(t, "testdata/Hooked.yaml", filepath.Join(products, "Hooked.yaml"))

	overlay := filepath.Join(dir, "overlay.yaml")
	os.WriteFile(overlay, []byte("rulings: {}\naliases: {}\ndiscover_default: [gcp.widget]\n"), 0o644)

	return Inputs{
		SchemaDir:   schemas,
		MMV1Dir:     filepath.Join(dir, "mmv1", "products"),
		OverlayPath: overlay,
		LockPath:    filepath.Join(dir, "names.lock.json"),
	}
}

func copyFile(t *testing.T, from, to string) {
	t.Helper()
	data, err := os.ReadFile(from)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(to, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestTheGeneratorShipsTierOneAndRefusesTheRest is the central assertion of G5:
// the catalog contains what the generator can vouch for, and everything else is
// reported rather than dropped.
func TestTheGeneratorShipsTierOneAndRefusesTheRest(t *testing.T) {
	res, err := Build(buildFixture(t))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := res.Catalog.Type("gcp.widget"); !ok {
		t.Errorf("tier-1 gcp.widget missing; catalog has %d types", len(res.Catalog.Types))
	}
	if _, ok := res.Catalog.Type("gcp.hooked"); ok {
		t.Error("gcp.hooked shipped despite an unruled encoder hook")
	}
	var found bool
	for _, w := range res.Warnings {
		if w.Resource == "Hooked" {
			found = true
			if !strings.Contains(w.Reason, "encoder") {
				t.Errorf("warning does not name the hook: %q", w.Reason)
			}
		}
	}
	if !found {
		t.Error("the refused type was dropped silently rather than warned about")
	}
}

// TestGenerationIsDeterministic. The catalog is a committed artifact reviewed as
// a diff, so two runs over the same inputs must produce identical bytes or every
// regeneration is unreviewable noise.
func TestGenerationIsDeterministic(t *testing.T) {
	in := buildFixture(t)
	a, err := Build(in)
	if err != nil {
		t.Fatal(err)
	}
	os.Remove(in.LockPath)
	b, err := Build(in)
	if err != nil {
		t.Fatal(err)
	}
	ab, err := catalog.Encode(a.Catalog)
	if err != nil {
		t.Fatal(err)
	}
	bb, err := catalog.Encode(b.Catalog)
	if err != nil {
		t.Fatal(err)
	}
	if string(ab) != string(bb) {
		t.Error("two runs over identical inputs produced different catalogs")
	}
}

// TestTheLockIsExtendedNotRewritten.
func TestTheGeneratorExtendsTheLock(t *testing.T) {
	in := buildFixture(t)
	os.WriteFile(in.LockPath, []byte(`{"names":{"gone/Widget":"gcp.widget"}}`), 0o644)
	if _, err := Build(in); err != nil {
		t.Fatal(err)
	}
	lock, err := LoadLock(in.LockPath)
	if err != nil {
		t.Fatal(err)
	}
	if lock.Names["gone/Widget"] != "gcp.widget" {
		t.Errorf("the generator dropped a withdrawn type's reservation: %v", lock.Names)
	}
	// And the live one could not have taken the short name.
	if lock.Names["tiny/Widget"] == "gcp.widget" {
		t.Error("the live Widget took a name already reserved by another type")
	}
}

// TestADanglingReferenceIsDropped. gcp.widget.network's magic-modules
// definition points at "Network", which never ships in this fixture — an edge
// to a type the catalog does not serve would make `import --generate` write a
// ${ref} nobody can compile.
func TestADanglingReferenceIsDropped(t *testing.T) {
	res, err := Build(buildFixture(t))
	if err != nil {
		t.Fatal(err)
	}
	ty, ok := res.Catalog.Type("gcp.widget")
	if !ok {
		t.Fatal("gcp.widget missing")
	}
	a, ok := ty.Attributes["network"]
	if !ok {
		t.Fatal("network attribute missing from gcp.widget")
	}
	if a.Ref != nil {
		t.Errorf("network.Ref = %+v, want nil: Network never ships in this fixture", a.Ref)
	}
}

// TestALoadDirFailureIsWarnedNotSwallowed. mmv1.LoadDir collects per-file parse
// failures rather than aborting the walk; Build must not swallow them, or a
// type that failed to parse and left no trace is one nobody can fix.
func TestALoadDirFailureIsWarnedNotSwallowed(t *testing.T) {
	dir := t.TempDir()
	schemas := filepath.Join(dir, "schemas")
	products := filepath.Join(dir, "mmv1", "products", "tiny")
	for _, d := range []string{schemas, products} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// Invalid YAML (an unterminated flow sequence), not merely a resource with
	// no name: this must fail ParseResource's yaml.Unmarshal itself.
	if err := os.WriteFile(filepath.Join(products, "Broken.yaml"), []byte("name: [broken\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	overlay := filepath.Join(dir, "overlay.yaml")
	if err := os.WriteFile(overlay, []byte("rulings: {}\naliases: {}\ndiscover_default: []\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	res, err := Build(Inputs{
		SchemaDir:   schemas,
		MMV1Dir:     filepath.Join(dir, "mmv1", "products"),
		OverlayPath: overlay,
		LockPath:    filepath.Join(dir, "names.lock.json"),
	})
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, w := range res.Warnings {
		if w.Resource == "Broken.yaml" {
			found = true
			if w.Reason == "" {
				t.Error("the parse failure was warned about with no reason")
			}
		}
	}
	if !found {
		t.Error("a magic-modules file that failed to parse was dropped without a trace")
	}
}

// TestARulingsAllForceNewAndReadViaBothTakeEffect. G6's tagBindings ruling has
// no get and no patch: every settable attribute must come out ForceNew, and
// the type must record how to read it, or the ruling is a comment with extra
// steps.
func TestARulingsAllForceNewAndReadViaBothTakeEffect(t *testing.T) {
	dir := t.TempDir()
	schemas := filepath.Join(dir, "schemas")
	products := filepath.Join(dir, "mmv1", "products", "tiny")
	for _, d := range []string{schemas, products} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	copyFile(t, "testdata/frozen-schema.json", filepath.Join(schemas, "tiny.json"))
	copyFile(t, "testdata/Frozen.yaml", filepath.Join(products, "Frozen.yaml"))

	overlay := filepath.Join(dir, "overlay.yaml")
	overlayYAML := "rulings:\n" +
		"  tiny/Frozen:\n" +
		"    hooks: []\n" +
		"    read_via: list_by_parent\n" +
		"    all_force_new: true\n" +
		"    note: mirrors the real TagBinding ruling in a small fixture.\n" +
		"aliases: {}\n" +
		"discover_default: []\n"
	if err := os.WriteFile(overlay, []byte(overlayYAML), 0o644); err != nil {
		t.Fatal(err)
	}

	res, err := Build(Inputs{
		SchemaDir:   schemas,
		MMV1Dir:     filepath.Join(dir, "mmv1", "products"),
		OverlayPath: overlay,
		LockPath:    filepath.Join(dir, "names.lock.json"),
	})
	if err != nil {
		t.Fatal(err)
	}
	ty, ok := res.Catalog.Type("gcp.frozen")
	if !ok {
		t.Fatalf("gcp.frozen missing; catalog has %d types", len(res.Catalog.Types))
	}
	if ty.ReadVia != "list_by_parent" {
		t.Errorf("ReadVia = %q, want list_by_parent", ty.ReadVia)
	}
	if len(ty.Attributes) == 0 {
		t.Fatal("gcp.frozen has no attributes to check")
	}
	// config (Fields) and tags (Elem.Fields) exist specifically so this check
	// walks below the top level: a flat fixture can't tell a correct recursive
	// forceNewAttr from one that only ever touched the top-level map.
	var checked int
	assertAllForceNew(t, "gcp.frozen", ty.Attributes, &checked)
	if checked < 5 {
		t.Fatalf("only checked %d attributes; the nested config/tags fixture should have produced at least 5 (name, value, config, config.mode, config.computed, tags, tags[].key)", checked)
	}
}

// assertAllForceNew walks attrs (and every Fields/Elem below it) asserting
// every non-Output attribute is ForceNew, and counts how many it checked —
// used to catch a walker that silently visits nothing.
func assertAllForceNew(t *testing.T, path string, attrs map[string]*catalog.Attr, checked *int) {
	t.Helper()
	for name, a := range attrs {
		p := path + "." + name
		*checked++
		if a.Output {
			if a.ForceNew {
				t.Errorf("%s is Output AND ForceNew; Output should have exempted it", p)
			}
		} else if !a.ForceNew {
			t.Errorf("%s is not ForceNew, but the ruling says all_force_new and the type has no patch method", p)
		}
		if a.Fields != nil {
			assertAllForceNew(t, p, a.Fields, checked)
		}
		if a.Elem != nil {
			assertAllForceNew(t, p+"[]", map[string]*catalog.Attr{"": a.Elem}, checked)
		}
	}
}

// writeRefFixture builds a temp input tree from an explicit file listing. The
// three tests below each need a specific, small product/service layout to
// exercise one branch of resolveOneRef, and that layout IS the point of the
// test, so it's written out inline rather than hidden in a testdata file.
func writeRefFixture(t *testing.T, files map[string]string) Inputs {
	t.Helper()
	dir := t.TempDir()
	for rel, content := range files {
		full := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	overlay := filepath.Join(dir, "overlay.yaml")
	if err := os.WriteFile(overlay, []byte("rulings: {}\naliases: {}\ndiscover_default: []\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return Inputs{
		SchemaDir:   filepath.Join(dir, "schemas"),
		MMV1Dir:     filepath.Join(dir, "mmv1", "products"),
		OverlayPath: overlay,
		LockPath:    filepath.Join(dir, "names.lock.json"),
	}
}

// simpleDoc is a minimal Discovery document: one resource schema (name plus
// one string property) and one matching get/insert/delete collection.
func simpleDoc(service, collection, schema, extraProp string) string {
	return `{
  "name": "` + service + `",
  "version": "v1",
  "rootUrl": "https://tiny.googleapis.com/",
  "servicePath": "",
  "schemas": {
    "` + schema + `": {
      "id": "` + schema + `",
      "type": "object",
      "properties": {
        "name": {"type": "string"}` + extraProp + `
      }
    },
    "Operation": {"id": "Operation", "type": "object", "properties": {"status": {"type": "string"}}}
  },
  "resources": {
    "projects": {"resources": {"` + collection + `": {"methods": {
      "get": {"id": "x.get", "path": "projects/{project}/` + collection + `/{id}", "httpMethod": "GET", "response": {"$ref": "` + schema + `"}},
      "insert": {"id": "x.insert", "path": "projects/{project}/` + collection + `", "httpMethod": "POST", "request": {"$ref": "` + schema + `"}, "response": {"$ref": "Operation"}},
      "delete": {"id": "x.delete", "path": "projects/{project}/` + collection + `/{id}", "httpMethod": "DELETE", "response": {"$ref": "Operation"}}
    }}}}
  }
}`
}

// TestASameProductReferenceResolves. Step 1 of resolveOneRef: the referring
// resource's own product is tried FIRST, before the cross-product/ambiguity
// steps even look at the candidate set. A third, unrelated product ships a
// second resource also named "Target" — if step 1 were skipped, "Target"
// would have two candidates (samesvc, othersvc) and steps 2/3 would treat it
// as ambiguous and drop it instead of resolving it locally. Only the
// same-product check distinguishes "my own product's Target" from "some
// other product's Target that happens to share the name".
func TestASameProductReferenceResolves(t *testing.T) {
	in := writeRefFixture(t, map[string]string{
		"schemas/samesvc.json": simpleDoc("samesvc", "referrers", "Referrer",
			`, "target": {"type": "string", "description": "The target."}`),
		"schemas/samesvc-targets.json": simpleDoc("samesvc", "targets", "Target", ""),
		"schemas/othersvc.json":        simpleDoc("othersvc", "targets", "Target", ""),
		"mmv1/products/samesvc/Referrer.yaml": `name: Referrer
description: references a target in the same product.
base_url: projects/{{project}}/referrers
properties:
  - name: name
    type: String
    required: true
  - name: target
    type: ResourceRef
    resource: Target
    imports: selfLink
`,
		"mmv1/products/samesvc/Target.yaml": `name: Target
description: the same-product reference target.
base_url: projects/{{project}}/targets
properties:
  - name: name
    type: String
    required: true
`,
		"mmv1/products/othersvc/Target.yaml": `name: Target
description: an unrelated product's own Target, sharing the bare name only.
base_url: projects/{{project}}/targets
properties:
  - name: name
    type: String
    required: true
`,
	})
	res, err := Build(in)
	if err != nil {
		t.Fatal(err)
	}
	ty, ok := res.Catalog.Type("gcp.referrer")
	if !ok {
		t.Fatalf("gcp.referrer missing; catalog has %d types", len(res.Catalog.Types))
	}
	a, ok := ty.Attributes["target"]
	if !ok || a.Ref == nil {
		t.Fatalf("target attribute or its Ref missing: %+v", a)
	}
	// "target" is wanted by both samesvc's and othersvc's Target, so Assign
	// qualifies both names — samesvc's own is gcp.samesvc.target, not the
	// short gcp.target.
	if a.Ref.Type != "gcp.samesvc.target" {
		t.Errorf("target.Ref.Type = %q, want gcp.samesvc.target (samesvc's OWN Target, not othersvc's)", a.Ref.Type)
	}
	for _, w := range res.Warnings {
		if strings.Contains(w.Reason, "ambiguous") {
			t.Errorf("unexpected ambiguity warning: same-product resolution must short-circuit before the ambiguity check: %+v", w)
		}
	}
}

// Two Discovery documents share one schemas dir and one mmv1/products dir
// deliberately: separate services/products loaded in a single Build() call,
// which is exactly what happens for the real ~40-API corpus.

// TestAUniqueCrossProductReferenceResolves. Step 2 of resolveOneRef: the
// referring resource's own product misses, but exactly one other product
// shipped the target name, so it's a real (not ambiguous) cross-product edge
// — e.g. compute/Subnetwork -> networkconnectivity/InternalRange.
func TestAUniqueCrossProductReferenceResolves(t *testing.T) {
	in := writeRefFixture(t, map[string]string{
		"schemas/crossa.json": simpleDoc("crossa", "referrer2s", "Referrer2",
			`, "shared": {"type": "string", "description": "The shared target."}`),
		"schemas/crossb.json": simpleDoc("crossb", "shareds", "Shared", ""),
		"mmv1/products/crossa/Referrer2.yaml": `name: Referrer2
description: references a target that ships in a different product.
base_url: projects/{{project}}/referrer2s
properties:
  - name: name
    type: String
    required: true
  - name: shared
    type: ResourceRef
    resource: Shared
    imports: selfLink
`,
		"mmv1/products/crossb/Shared.yaml": `name: Shared
description: ships in a different product than its referrer.
base_url: projects/{{project}}/shareds
properties:
  - name: name
    type: String
    required: true
`,
	})
	res, err := Build(in)
	if err != nil {
		t.Fatal(err)
	}
	ty, ok := res.Catalog.Type("gcp.referrer2")
	if !ok {
		t.Fatalf("gcp.referrer2 missing; catalog has %d types", len(res.Catalog.Types))
	}
	a, ok := ty.Attributes["shared"]
	if !ok || a.Ref == nil {
		t.Fatalf("shared attribute or its Ref missing: %+v", a)
	}
	if a.Ref.Type != "gcp.shared" {
		t.Errorf("shared.Ref.Type = %q, want gcp.shared", a.Ref.Type)
	}
	for _, w := range res.Warnings {
		if strings.Contains(w.Reason, "ambiguous") {
			t.Errorf("unexpected ambiguity warning for a unique cross-product reference: %+v", w)
		}
	}
}

// TestAnAmbiguousReferenceIsDroppedAndWarned. Step 3 of resolveOneRef: the
// referring resource's own product misses, AND more than one other product
// shipped the target name (942 magic-modules resources share only 802
// distinct names in the real corpus — "Instance" alone spans 16 products).
// Guessing would write a wrong edge into a user's generated configuration, so
// this must drop the reference and warn naming every candidate, not pick one.
func TestAnAmbiguousReferenceIsDroppedAndWarned(t *testing.T) {
	in := writeRefFixture(t, map[string]string{
		"schemas/ambref.json": simpleDoc("ambref", "referrer3s", "Referrer3",
			`, "thing": {"type": "string", "description": "The ambiguous target."}`),
		"schemas/thinga.json": simpleDoc("thinga", "things", "Thing", ""),
		"schemas/thingb.json": simpleDoc("thingb", "things", "Thing", ""),
		"mmv1/products/ambref/Referrer3.yaml": `name: Referrer3
description: references a target name that is ambiguous across products.
base_url: projects/{{project}}/referrer3s
properties:
  - name: name
    type: String
    required: true
  - name: thing
    type: ResourceRef
    resource: Thing
    imports: selfLink
`,
		"mmv1/products/thinga/Thing.yaml": `name: Thing
description: a Thing shipped by product thinga.
base_url: projects/{{project}}/things
properties:
  - name: name
    type: String
    required: true
`,
		"mmv1/products/thingb/Thing.yaml": `name: Thing
description: a Thing shipped by product thingb, colliding by name with thinga's.
base_url: projects/{{project}}/things
properties:
  - name: name
    type: String
    required: true
`,
	})
	res, err := Build(in)
	if err != nil {
		t.Fatal(err)
	}
	ty, ok := res.Catalog.Type("gcp.referrer3")
	if !ok {
		t.Fatalf("gcp.referrer3 missing; catalog has %d types", len(res.Catalog.Types))
	}
	a, ok := ty.Attributes["thing"]
	if !ok {
		t.Fatal("thing attribute missing")
	}
	if a.Ref != nil {
		t.Errorf("thing.Ref = %+v, want nil: Thing is ambiguous across thinga and thingb", a.Ref)
	}
	var found bool
	for _, w := range res.Warnings {
		if w.Resource == "gcp.referrer3.thing" {
			found = true
			if !strings.Contains(w.Reason, "ambiguous") {
				t.Errorf("warning does not say the reference is ambiguous: %q", w.Reason)
			}
			if !strings.Contains(w.Reason, "thinga") || !strings.Contains(w.Reason, "thingb") {
				t.Errorf("warning does not name both candidates: %q", w.Reason)
			}
		}
	}
	if !found {
		t.Error("the ambiguous reference was dropped without a warning naming the candidates")
	}
}

// TestAReferenceToAFailedBuildIsDropped covers the path F1 review found:
// Classify only checks that a create method exists, so a candidate can pass
// tiering and still fail inside buildType (its create method's request
// schema does not exist, here). Ghost has an mm resource and passes Classify,
// but its own build fails; Referrer4, in the same product, references it.
// Building refByProduct/refCandidates from `shipping` (everything that merely
// passed tiering) rather than from `succeeded` (everything that actually
// reached c.Types) would let Referrer4's reference resolve to "gcp.ghost" — a
// name Assign reserved but that no catalog.Type actually carries. It must
// resolve to nothing instead, and Ghost's own failure must still be warned
// about on its own account.
func TestAReferenceToAFailedBuildIsDropped(t *testing.T) {
	in := writeRefFixture(t, map[string]string{
		"schemas/ghostsvc.json": `{
  "name": "ghostsvc",
  "version": "v1",
  "rootUrl": "https://tiny.googleapis.com/",
  "servicePath": "",
  "schemas": {
    "Referrer4": {
      "id": "Referrer4",
      "type": "object",
      "properties": {
        "name": {"type": "string"},
        "ghost": {"type": "string", "description": "The (broken) target."}
      }
    },
    "Operation": {"id": "Operation", "type": "object", "properties": {"status": {"type": "string"}}}
  },
  "resources": {
    "projects": {"resources": {
      "ghosts": {"methods": {
        "get": {"id": "x.ghosts.get", "path": "projects/{project}/ghosts/{id}", "httpMethod": "GET", "response": {"$ref": "MissingSchema"}},
        "insert": {"id": "x.ghosts.insert", "path": "projects/{project}/ghosts", "httpMethod": "POST", "request": {"$ref": "MissingSchema"}, "response": {"$ref": "Operation"}},
        "delete": {"id": "x.ghosts.delete", "path": "projects/{project}/ghosts/{id}", "httpMethod": "DELETE", "response": {"$ref": "Operation"}}
      }},
      "referrer4s": {"methods": {
        "get": {"id": "x.referrer4s.get", "path": "projects/{project}/referrer4s/{id}", "httpMethod": "GET", "response": {"$ref": "Referrer4"}},
        "insert": {"id": "x.referrer4s.insert", "path": "projects/{project}/referrer4s", "httpMethod": "POST", "request": {"$ref": "Referrer4"}, "response": {"$ref": "Operation"}},
        "delete": {"id": "x.referrer4s.delete", "path": "projects/{project}/referrer4s/{id}", "httpMethod": "DELETE", "response": {"$ref": "Operation"}}
      }}
    }}
  }
}`,
		"mmv1/products/ghostsvc/Ghost.yaml": `name: Ghost
description: a resource whose Discovery create method references a schema that does not exist.
base_url: projects/{{project}}/ghosts
properties:
  - name: name
    type: String
    required: true
`,
		"mmv1/products/ghostsvc/Referrer4.yaml": `name: Referrer4
description: references Ghost, whose own build fails despite passing Classify.
base_url: projects/{{project}}/referrer4s
properties:
  - name: name
    type: String
    required: true
  - name: ghost
    type: ResourceRef
    resource: Ghost
    imports: selfLink
`,
	})
	res, err := Build(in)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := res.Catalog.Type("gcp.ghost"); ok {
		t.Error("gcp.ghost shipped despite its create method referencing a schema that does not exist")
	}
	var buildFailureWarned bool
	for _, w := range res.Warnings {
		if w.Resource == "Ghost" && w.Tier == TierExcluded {
			buildFailureWarned = true
		}
	}
	if !buildFailureWarned {
		t.Error("Ghost's own build failure was not warned about")
	}
	ty, ok := res.Catalog.Type("gcp.referrer4")
	if !ok {
		t.Fatalf("gcp.referrer4 missing; catalog has %d types", len(res.Catalog.Types))
	}
	a, ok := ty.Attributes["ghost"]
	if !ok {
		t.Fatal("ghost attribute missing")
	}
	if a.Ref != nil {
		t.Errorf("ghost.Ref = %+v, want nil: Ghost's name was reserved but it never actually shipped", a.Ref)
	}
}

// scopedDoc is a minimal Discovery document with the SAME leaf collection
// name repeated under four different resource hierarchy roots
// (organizations, folders, billingAccounts, projects) plus one bare
// top-level occurrence with no root at all -- the exact shape a real GCP API
// uses for org/folder/project/billing-account scoped resources (logging's
// buckets, cloudresourcemanager's capabilityConfigs), used to test that
// scopeSegment gives each root a distinct name while a project-scoped (or
// unrecognised) collection keeps today's naming.
func scopedDoc(service string) string {
	return `{
  "name": "` + service + `",
  "version": "v1",
  "rootUrl": "https://tiny.googleapis.com/",
  "servicePath": "",
  "schemas": {
    "Widget": {"id": "Widget", "type": "object", "properties": {"name": {"type": "string"}}},
    "Operation": {"id": "Operation", "type": "object", "properties": {"status": {"type": "string"}}}
  },
  "resources": {
    "organizations": {"resources": {"widgets": {"methods": {
      "get": {"id": "x.org.get", "path": "organizations/{org}/widgets/{id}", "httpMethod": "GET", "response": {"$ref": "Widget"}},
      "insert": {"id": "x.org.insert", "path": "organizations/{org}/widgets", "httpMethod": "POST", "request": {"$ref": "Widget"}, "response": {"$ref": "Operation"}},
      "delete": {"id": "x.org.delete", "path": "organizations/{org}/widgets/{id}", "httpMethod": "DELETE", "response": {"$ref": "Operation"}}
    }}}},
    "folders": {"resources": {"widgets": {"methods": {
      "get": {"id": "x.folder.get", "path": "folders/{folder}/widgets/{id}", "httpMethod": "GET", "response": {"$ref": "Widget"}},
      "insert": {"id": "x.folder.insert", "path": "folders/{folder}/widgets", "httpMethod": "POST", "request": {"$ref": "Widget"}, "response": {"$ref": "Operation"}},
      "delete": {"id": "x.folder.delete", "path": "folders/{folder}/widgets/{id}", "httpMethod": "DELETE", "response": {"$ref": "Operation"}}
    }}}},
    "billingAccounts": {"resources": {"widgets": {"methods": {
      "get": {"id": "x.billing.get", "path": "billingAccounts/{account}/widgets/{id}", "httpMethod": "GET", "response": {"$ref": "Widget"}},
      "insert": {"id": "x.billing.insert", "path": "billingAccounts/{account}/widgets", "httpMethod": "POST", "request": {"$ref": "Widget"}, "response": {"$ref": "Operation"}},
      "delete": {"id": "x.billing.delete", "path": "billingAccounts/{account}/widgets/{id}", "httpMethod": "DELETE", "response": {"$ref": "Operation"}}
    }}}},
    "projects": {"resources": {"widgets": {"methods": {
      "get": {"id": "x.proj.get", "path": "projects/{project}/widgets/{id}", "httpMethod": "GET", "response": {"$ref": "Widget"}},
      "insert": {"id": "x.proj.insert", "path": "projects/{project}/widgets", "httpMethod": "POST", "request": {"$ref": "Widget"}, "response": {"$ref": "Operation"}},
      "delete": {"id": "x.proj.delete", "path": "projects/{project}/widgets/{id}", "httpMethod": "DELETE", "response": {"$ref": "Operation"}}
    }}}}
  }
}`
}

// TestScopeVariantsOfTheSameLeafGetDistinctNames. The real catalog shipped
// four DIFFERENT logging bucket types (an org's, a folder's, a billing
// account's and a project's own) under the SAME name, gcp.logging.bucket,
// because Candidate only ever looked at the collection's leaf. Each of these
// has its own base URL and its own IAM; collapsing them into one name meant
// three of the four were silently unreachable behind whichever one the
// naming pass happened to keep.
func TestScopeVariantsOfTheSameLeafGetDistinctNames(t *testing.T) {
	in := writeRefFixture(t, map[string]string{
		"schemas/acme.json": scopedDoc("acme"),
		// base_url is a parent capture, the way the real four-scope case in
		// the corpus spells it (logging's buckets are "{+parent}/buckets").
		// A bare "widgets" would say the create posts to <api>/widgets while
		// every one of these collections addresses its resources under an
		// organization, folder, billing account or project -- which is the
		// create-one-thing-read-another shape buildType now refuses outright,
		// so the fixture would be asserting names for types that cannot ship.
		"mmv1/products/acme/Widget.yaml": `name: Widget
description: a scoped test widget.
base_url: '{+parent}/widgets'
properties:
  - name: name
    type: String
    required: true
`,
	})
	res, err := Build(in)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"gcp.widget", "gcp.acme.organization.widget", "gcp.acme.folder.widget", "gcp.acme.billingaccount.widget"}
	for _, name := range want {
		if _, ok := res.Catalog.Type(name); !ok {
			var got []string
			for _, ty := range res.Catalog.Types {
				got = append(got, ty.Name)
			}
			t.Errorf("%s missing; catalog has %v", name, got)
		}
	}
	if len(res.Catalog.Types) != 4 {
		t.Fatalf("got %d types, want 4 distinct scope variants: %+v", len(res.Catalog.Types), res.Catalog.Types)
	}
}

// TestCheckNamesAreUniqueCatchesADuplicate is a direct unit test of the
// generator-level invariant, independent of whatever Build's own collision
// resolution manages to disambiguate on its own. It exists so the invariant
// is tested as a real safety net rather than something only ever exercised
// indirectly -- in a moment, that indirection turned out to matter: the
// build-level fixture this test used to be no longer collides at all, now
// that disambiguateByPath resolves it (see
// TestUnrelatedResourcesShareALeafAndAreDisambiguatedByPath below). That is
// the correct outcome for THAT input, but it means checkNamesAreUnique itself
// still needs its own direct coverage so a future change to Build's
// resolution logic that reintroduces a real collision is still caught here.
func TestCheckNamesAreUniqueCatchesADuplicate(t *testing.T) {
	types := []*catalog.Type{{Name: "gcp.widget"}, {Name: "gcp.gadget"}, {Name: "gcp.widget"}}
	err := checkNamesAreUnique(types)
	if err == nil {
		t.Fatal("expected an error: gcp.widget was assigned to two types")
	}
	if !strings.Contains(err.Error(), "gcp.widget") || !strings.Contains(err.Error(), "x2") {
		t.Errorf("error does not name the offending type and how many types share it: %v", err)
	}
}

func TestCheckNamesAreUniqueAcceptsDistinctNames(t *testing.T) {
	types := []*catalog.Type{{Name: "gcp.widget"}, {Name: "gcp.gadget"}}
	if err := checkNamesAreUnique(types); err != nil {
		t.Errorf("distinct names were refused: %v", err)
	}
}

// dupLeafDoc is a Discovery document with two top-level collections at
// different, UNRELATED single-segment parents ("alphaShared0" and
// "betaShared0") that both end in "widgets" -- genuinely different resources
// sharing a bare leaf, the shape gcp.iam.provider turned out to have
// (workloadIdentityPools.providers and workforcePools.providers).
func dupLeafDoc(service string) string {
	leaf := func(id, parent string) string {
		path := parent + "/widgets"
		return `"widgets": {"methods": {
      "get": {"id": "` + id + `.get", "path": "` + path + `/{id}", "httpMethod": "GET", "response": {"$ref": "Widget"}},
      "insert": {"id": "` + id + `.insert", "path": "` + path + `", "httpMethod": "POST", "request": {"$ref": "Widget"}, "response": {"$ref": "Operation"}},
      "delete": {"id": "` + id + `.delete", "path": "` + path + `/{id}", "httpMethod": "DELETE", "response": {"$ref": "Operation"}}
    }}`
	}
	return `{
  "name": "` + service + `",
  "version": "v1",
  "rootUrl": "https://tiny.googleapis.com/",
  "servicePath": "",
  "schemas": {
    "Widget": {"id": "Widget", "type": "object", "properties": {"name": {"type": "string"}}},
    "Operation": {"id": "Operation", "type": "object", "properties": {"status": {"type": "string"}}}
  },
  "resources": {
    "alphaShared0": {"resources": {` + leaf("x.a", "alphaShared0") + `}},
    "betaShared0": {"resources": {` + leaf("x.b", "betaShared0") + `}}
  }
}`
}

// TestUnrelatedResourcesShareALeafAndAreDisambiguatedByPath. Two collections
// with no organization/folder/billing-account root and no legacy-alias
// relationship (their canonical paths genuinely differ) must BOTH ship, each
// under a name extended just as far up its own path as it needs to become
// unique.
func TestUnrelatedResourcesShareALeafAndAreDisambiguatedByPath(t *testing.T) {
	in := writeRefFixture(t, map[string]string{
		"mmv1/products/dup/product.yaml": "name: Dup\n",
		"schemas/dup.json":               dupLeafDoc("dup"),
	})
	res, err := Build(in)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"gcp.dup.alphashared0.widget", "gcp.dup.betashared0.widget"} {
		if _, ok := res.Catalog.Type(name); !ok {
			var got []string
			for _, ty := range res.Catalog.Types {
				got = append(got, ty.Name)
			}
			t.Errorf("%s missing; catalog has %v", name, got)
		}
	}
	if len(res.Catalog.Types) != 2 {
		t.Fatalf("got %d types, want 2 disambiguated survivors: %+v", len(res.Catalog.Types), res.Catalog.Types)
	}
}

// TestDisambiguationWalksAsManyLevelsAsItTakes forces a collision that a
// one-level-only implementation cannot resolve: both collections share their
// IMMEDIATE parent segment too ("alphashared1"/"betashared1" both sit under a
// segment literally named "shared1" -- wait, no: depth 2 gives each branch
// its own two-level chain, alphashared0/alphashared1 vs
// betashared0/betashared1, so the one-level walk (alphashared1 vs
// betashared1) already disambiguates them by itself here). The real
// regression this guards is walking each survivor exactly as far as IT
// needs, independently -- proven by TestUnrelatedResourcesShareALeafAndAreDisambiguatedByPath
// (one level) and the iam.key evidence in the real corpus (mixed one and two
// levels within the SAME collision group, recorded in the task report), so
// this test instead pins that a deeper shared prefix still resolves rather
// than exhausting the walk: both branches share "shared0" one level up AND
// diverge only at "shared1" two levels up.
func TestDisambiguationWalksAsManyLevelsAsItTakes(t *testing.T) {
	in := writeRefFixture(t, map[string]string{
		"mmv1/products/dup/product.yaml": "name: Dup\n",
		"schemas/dup.json": `{
  "name": "dup",
  "version": "v1",
  "rootUrl": "https://tiny.googleapis.com/",
  "servicePath": "",
  "schemas": {
    "Widget": {"id": "Widget", "type": "object", "properties": {"name": {"type": "string"}}},
    "Operation": {"id": "Operation", "type": "object", "properties": {"status": {"type": "string"}}}
  },
  "resources": {
    "shared": {"resources": {
      "alphaKind": {"resources": {"widgets": {"methods": {
        "get": {"id": "x.a.get", "path": "shared/alphaKind/widgets/{id}", "httpMethod": "GET", "response": {"$ref": "Widget"}},
        "insert": {"id": "x.a.insert", "path": "shared/alphaKind/widgets", "httpMethod": "POST", "request": {"$ref": "Widget"}, "response": {"$ref": "Operation"}},
        "delete": {"id": "x.a.delete", "path": "shared/alphaKind/widgets/{id}", "httpMethod": "DELETE", "response": {"$ref": "Operation"}}
      }}}},
      "betaKind": {"resources": {"widgets": {"methods": {
        "get": {"id": "x.b.get", "path": "shared/betaKind/widgets/{id}", "httpMethod": "GET", "response": {"$ref": "Widget"}},
        "insert": {"id": "x.b.insert", "path": "shared/betaKind/widgets", "httpMethod": "POST", "request": {"$ref": "Widget"}, "response": {"$ref": "Operation"}},
        "delete": {"id": "x.b.delete", "path": "shared/betaKind/widgets/{id}", "httpMethod": "DELETE", "response": {"$ref": "Operation"}}
      }}}}
    }}
  }
}`,
	})
	res, err := Build(in)
	if err != nil {
		t.Fatal(err)
	}
	// A one-level-only implementation would extend both to "gcp.dup.widgets.widget"
	// (singular("widgets") applied to their shared immediate ancestor "widgets" the
	// nested resource key, if it even used the right segment) or otherwise fail to
	// tell them apart; walking two levels reaches their genuinely distinguishing
	// alphaKind/betaKind segment.
	for _, name := range []string{"gcp.dup.alphakind.widget", "gcp.dup.betakind.widget"} {
		if _, ok := res.Catalog.Type(name); !ok {
			var got []string
			for _, ty := range res.Catalog.Types {
				got = append(got, ty.Name)
			}
			t.Errorf("%s missing; catalog has %v", name, got)
		}
	}
	if len(res.Catalog.Types) != 2 {
		t.Fatalf("got %d types, want 2 disambiguated survivors: %+v", len(res.Catalog.Types), res.Catalog.Types)
	}
}

// TestLegacyAliasesDeduplicateWithAWarningAndALoneOneSurvivesUntouched
// covers categories 1 and 2 together, deliberately in one fixture: a
// competing pair (projects.zones.clusters legacy alongside
// projects.locations.clusters current, mirroring the real container product)
// that must collapse to ONE type with a warning naming the winner, and a
// LONE zonal-only resource with no locations.-rooted competitor (mirroring
// compute's many zone-only resources) that must ship completely unaffected
// -- proving the rule is a de-duplication rule triggered only by an actual
// competitor, never a blanket "prefer locations" exclusion.
func TestLegacyAliasesDeduplicateWithAWarningAndALoneOneSurvivesUntouched(t *testing.T) {
	in := writeRefFixture(t, map[string]string{
		"mmv1/products/dup/Cluster.yaml": `name: Cluster
description: mirrors container's Cluster, whose base_url targets the locations path.
base_url: projects/{{project}}/locations/{{location}}/clusters
properties:
  - name: name
    type: String
    required: true
`,
		"schemas/dup.json": `{
  "name": "dup",
  "version": "v1",
  "rootUrl": "https://tiny.googleapis.com/",
  "servicePath": "",
  "schemas": {
    "Cluster": {"id": "Cluster", "type": "object", "properties": {"name": {"type": "string"}}},
    "Widget": {"id": "Widget", "type": "object", "properties": {"name": {"type": "string"}}},
    "Operation": {"id": "Operation", "type": "object", "properties": {"status": {"type": "string"}}}
  },
  "resources": {
    "projects": {"resources": {
      "zones": {"resources": {"clusters": {"methods": {
        "get": {"id": "x.z.get", "path": "projects/{p}/zones/{z}/clusters/{id}", "httpMethod": "GET", "response": {"$ref": "Cluster"}},
        "insert": {"id": "x.z.insert", "path": "projects/{p}/zones/{z}/clusters", "httpMethod": "POST", "request": {"$ref": "Cluster"}, "response": {"$ref": "Operation"}},
        "delete": {"id": "x.z.delete", "path": "projects/{p}/zones/{z}/clusters/{id}", "httpMethod": "DELETE", "response": {"$ref": "Operation"}}
      }}}},
      "locations": {"resources": {"clusters": {"methods": {
        "get": {"id": "x.l.get", "path": "projects/{p}/locations/{l}/clusters/{id}", "httpMethod": "GET", "response": {"$ref": "Cluster"}},
        "insert": {"id": "x.l.insert", "path": "projects/{p}/locations/{l}/clusters", "httpMethod": "POST", "request": {"$ref": "Cluster"}, "response": {"$ref": "Operation"}},
        "delete": {"id": "x.l.delete", "path": "projects/{p}/locations/{l}/clusters/{id}", "httpMethod": "DELETE", "response": {"$ref": "Operation"}}
      }}}},
      "widgets": {"methods": {
        "get": {"id": "x.w.get", "path": "projects/{p}/widgets/{id}", "httpMethod": "GET", "response": {"$ref": "Widget"}},
        "insert": {"id": "x.w.insert", "path": "projects/{p}/widgets", "httpMethod": "POST", "request": {"$ref": "Widget"}, "response": {"$ref": "Operation"}},
        "delete": {"id": "x.w.delete", "path": "projects/{p}/widgets/{id}", "httpMethod": "DELETE", "response": {"$ref": "Operation"}}
      }}
    }}
  }
}`,
	})
	res, err := Build(in)
	if err != nil {
		t.Fatal(err)
	}
	var clusterCount int
	for _, ty := range res.Catalog.Types {
		if strings.Contains(ty.Name, "cluster") {
			clusterCount++
			if ty.BaseURL != "projects/{{project}}/locations/{{location}}/clusters" {
				t.Errorf("survivor has base URL %q, want the locations-rooted one mm actually targets", ty.BaseURL)
			}
		}
	}
	if clusterCount != 1 {
		t.Fatalf("got %d cluster types, want exactly 1 (the pair should have deduplicated): %+v", clusterCount, res.Catalog.Types)
	}
	if _, ok := res.Catalog.Type("gcp.widget"); !ok {
		t.Error("the lone zonal-only widget (no locations.-rooted competitor) did not ship")
	}
	var warned bool
	for _, w := range res.Warnings {
		if strings.Contains(w.Reason, "legacy alias of") && strings.Contains(w.Reason, "cluster") {
			warned = true
			if !strings.Contains(w.Reason, "projects.locations.clusters") {
				t.Errorf("alias warning does not name the winning collection's path: %q", w.Reason)
			}
		}
	}
	if !warned {
		t.Error("the dropped zonal alias was not warned about")
	}
}

// TestAliasWarningNamesARealWinnerEvenWhenTheWinnerIsAlsoDisambiguated is F2:
// aliasLoser used to capture its winner's Candidate BY VALUE before
// disambiguateByPath ran on the same group, so a winner that was ALSO a
// survivor needing disambiguation (this fixture's shape: a legacy alias pair
// PLUS a third, genuinely distinct resource sharing the same bare leaf) had
// its Candidate mutated out from under the already-recorded warning. The
// warning's lookup then missed and silently produced "legacy alias of  (...)"
// -- a blank name in the one file the brief says to read, not skim.
//
// The fixture: projects.locations.somethingReal.widgets (current) and
// projects.zones.somethingReal.widgets (legacy alias of it) both singularize
// to "widget", as does otherthing.widgets -- an unrelated third resource.
// otherthing forces disambiguation, which extends BOTH the alias winner's and
// otherthing's Resource ("somethingreal.widget" / "otherthing.widget"), so
// the alias winner's Candidate is a different value after resolution than
// before it -- exactly the case a stale copy would miss.
func TestAliasWarningNamesARealWinnerEvenWhenTheWinnerIsAlsoDisambiguated(t *testing.T) {
	in := writeRefFixture(t, map[string]string{
		"mmv1/products/dup/product.yaml": "name: Dup\n",
		"schemas/dup.json": `{
  "name": "dup",
  "version": "v1",
  "rootUrl": "https://tiny.googleapis.com/",
  "servicePath": "",
  "schemas": {
    "Widget": {"id": "Widget", "type": "object", "properties": {"name": {"type": "string"}}},
    "Operation": {"id": "Operation", "type": "object", "properties": {"status": {"type": "string"}}}
  },
  "resources": {
    "projects": {"resources": {
      "locations": {"resources": {"somethingReal": {"resources": {"widgets": {"methods": {
        "get": {"id": "x.l.get", "path": "projects/{p}/locations/{l}/somethingReal/{s}/widgets/{id}", "httpMethod": "GET", "response": {"$ref": "Widget"}},
        "insert": {"id": "x.l.insert", "path": "projects/{p}/locations/{l}/somethingReal/{s}/widgets", "httpMethod": "POST", "request": {"$ref": "Widget"}, "response": {"$ref": "Operation"}},
        "delete": {"id": "x.l.delete", "path": "projects/{p}/locations/{l}/somethingReal/{s}/widgets/{id}", "httpMethod": "DELETE", "response": {"$ref": "Operation"}}
      }}}}}},
      "zones": {"resources": {"somethingReal": {"resources": {"widgets": {"methods": {
        "get": {"id": "x.z.get", "path": "projects/{p}/zones/{z}/somethingReal/{s}/widgets/{id}", "httpMethod": "GET", "response": {"$ref": "Widget"}},
        "insert": {"id": "x.z.insert", "path": "projects/{p}/zones/{z}/somethingReal/{s}/widgets", "httpMethod": "POST", "request": {"$ref": "Widget"}, "response": {"$ref": "Operation"}},
        "delete": {"id": "x.z.delete", "path": "projects/{p}/zones/{z}/somethingReal/{s}/widgets/{id}", "httpMethod": "DELETE", "response": {"$ref": "Operation"}}
      }}}}}}
    }},
    "otherthing": {"resources": {"widgets": {"methods": {
      "get": {"id": "x.o.get", "path": "otherthing/{o}/widgets/{id}", "httpMethod": "GET", "response": {"$ref": "Widget"}},
      "insert": {"id": "x.o.insert", "path": "otherthing/{o}/widgets", "httpMethod": "POST", "request": {"$ref": "Widget"}, "response": {"$ref": "Operation"}},
      "delete": {"id": "x.o.delete", "path": "otherthing/{o}/widgets/{id}", "httpMethod": "DELETE", "response": {"$ref": "Operation"}}
    }}}}
  }
}`,
	})
	res, err := Build(in)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"gcp.dup.somethingreal.widget", "gcp.dup.otherthing.widget"} {
		if _, ok := res.Catalog.Type(name); !ok {
			var got []string
			for _, ty := range res.Catalog.Types {
				got = append(got, ty.Name)
			}
			t.Errorf("%s missing; catalog has %v", name, got)
		}
	}
	var found bool
	for _, w := range res.Warnings {
		if !strings.Contains(w.Reason, "legacy alias of") {
			continue
		}
		found = true
		if strings.Contains(w.Reason, "legacy alias of  (") || strings.Contains(w.Reason, "legacy alias of (") {
			t.Errorf("warning names a blank winner: %q", w.Reason)
		}
		if !strings.Contains(w.Reason, "gcp.dup.somethingreal.widget") {
			t.Errorf("warning does not name the real (post-disambiguation) winner: %q", w.Reason)
		}
	}
	if !found {
		t.Error("the zonal alias was not warned about at all")
	}
}

// TestPathPrefixOfIsDerivedPerType. The prefix cannot be synthesized from the
// API's version: measured across the 39 fetched APIs with an empty
// servicePath, 41 of 2318 method paths do not begin with the bare version.
// dns is the proof inside a single API -- "dns/v1/" for responsePolicies,
// "v1/" for everything else -- so the prefix is read off that type's own
// method path or it is wrong.
func TestPathPrefixOfIsDerivedPerType(t *testing.T) {
	for _, c := range []struct{ path, want string }{
		{"v1/{+parent}/addressGroups", "v1/"},
		{"dns/v1/projects/{project}/responsePolicies", "dns/v1/"},
		{"v1beta1/{+name}", "v1beta1/"},
		{"v3/{+name}", "v3/"},
		// compute, storage and bigquery: the version is already in
		// servicePath, so the method path has none and the prefix is empty.
		{"projects/{project}/zones/{zone}/instances", ""},
		{"b/{bucket}/o/{object}", ""},
		// A custom verb on a bare version segment: "v3:fetchResourceSemantics"
		// is one segment and is not a version, so nothing is taken.
		{"v3:fetchResourceSemantics", ""},
		{"", ""},
	} {
		if got := pathPrefixOf(c.path); got != c.want {
			t.Errorf("pathPrefixOf(%q) = %q, want %q", c.path, got, c.want)
		}
	}
}

// containerShapedDoc is the two-operations-collections shape that made this
// generator nondeterministic: container publishes both a modern
// projects.locations.operations ("v1/{+name}") and a legacy
// projects.zones.operations, and a map walk reached whichever it liked.
func containerShapedDoc() *disco.Document {
	return &disco.Document{
		Name: "container",
		Resources: map[string]*disco.Resource{
			"projects": {Resources: map[string]*disco.Resource{
				"locations": {Resources: map[string]*disco.Resource{
					"operations": {Methods: map[string]*disco.Method{
						"get": {Path: "v1/{+name}"},
					}},
				}},
				"zones": {Resources: map[string]*disco.Resource{
					"operations": {Methods: map[string]*disco.Method{
						"get": {Path: "v1/projects/{projectId}/zones/{zone}/operations/{operationId}"},
					}},
				}},
			}},
		},
	}
}

func TestOperationPollPathPrefersAPathTheRuntimeCanExpand(t *testing.T) {
	// 200 walks of the same document: map order varies per run and per walk,
	// so a first-match implementation fails this with overwhelming odds.
	for i := 0; i < 200; i++ {
		if got := operationPollPath(containerShapedDoc()); got != "v1/{+name}" {
			t.Fatalf("walk %d chose %q, not the expandable v1/{+name}", i, got)
		}
	}
}

func TestOperationPollPathIsStableWhenEveryCandidateIsUnexpandable(t *testing.T) {
	// Nothing here is fillable, so there is no right answer -- but there is
	// still only one answer, the same one every run. await.go reports the
	// missing placeholder by name at the point it matters.
	doc := &disco.Document{
		Name: "odd",
		Resources: map[string]*disco.Resource{
			"a": {Resources: map[string]*disco.Resource{
				"operations": {Methods: map[string]*disco.Method{"get": {Path: "v1/b/{operationId}"}}},
			}},
			"b": {Resources: map[string]*disco.Resource{
				"operations": {Methods: map[string]*disco.Method{"get": {Path: "v1/a/{projectId}"}}},
			}},
		},
	}
	want := operationPollPath(doc)
	if want == "" {
		t.Fatal("no candidate was returned at all")
	}
	for i := 0; i < 200; i++ {
		if got := operationPollPath(doc); got != want {
			t.Fatalf("walk %d returned %q, previously %q", i, got, want)
		}
	}
}

func TestOperationPollPathFindsNothingWhenThereIsNothing(t *testing.T) {
	doc := &disco.Document{Name: "bare", Resources: map[string]*disco.Resource{
		"widgets": {Methods: map[string]*disco.Method{"get": {Path: "v1/widgets/{widget}"}}},
	}}
	if got := operationPollPath(doc); got != "" {
		t.Errorf("an API with no operations collection returned %q", got)
	}
}

// TestRuntimeOperationPlaceholdersMatchesTheRuntime. runtimeOperationPlaceholders
// is a copy of the attrs map operationRequestURL builds, and the two live in
// different packages, so nothing but this test stops them drifting apart.
//
// It reads the source with go/ast rather than as text. The text version sliced
// await.go from "func (p *Provider) operationRequestURL(" to the END OF THE
// FILE and then asked strings.Contains -- so every function BELOW
// operationRequestURL, and every comment anywhere in that region, counted as
// evidence that operationRequestURL still sets a placeholder. A prose mention
// of attrs["region"] in a comment satisfied it just as well as the assignment
// did. Parsing bounds the search to the function's own body, and to code:
// comments are not part of the syntax tree walked here.
func TestRuntimeOperationPlaceholdersMatchesTheRuntime(t *testing.T) {
	path := filepath.Join("..", "gcprov", "await.go")
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var body *ast.BlockStmt
	for _, decl := range f.Decls {
		if fn, ok := decl.(*ast.FuncDecl); ok && fn.Name.Name == "operationRequestURL" {
			body = fn.Body
		}
	}
	if body == nil {
		t.Fatal("operationRequestURL is no longer in internal/gcprov/await.go; this test needs rewriting")
	}
	set := placeholderKeysSetIn(body)
	for _, name := range sortedKeys(runtimeOperationPlaceholders) {
		if !set[name] {
			t.Errorf("the generator believes the runtime supplies %q, but operationRequestURL never sets it", name)
		}
	}
}

// placeholderKeysSetIn returns every string key the function body assigns a
// url placeholder under, in either of the two forms operationRequestURL uses:
// a "name": value entry in the attrs composite literal, and an
// attrs["name"] = value assignment.
func placeholderKeysSetIn(body *ast.BlockStmt) map[string]bool {
	out := map[string]bool{}
	add := func(e ast.Expr) {
		lit, ok := e.(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			return
		}
		if s, err := strconv.Unquote(lit.Value); err == nil {
			out[s] = true
		}
	}
	ast.Inspect(body, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.KeyValueExpr:
			add(x.Key)
		case *ast.IndexExpr:
			if ident, ok := x.X.(*ast.Ident); ok && ident.Name == "attrs" {
				add(x.Index)
			}
		}
		return true
	})
	return out
}

func TestPathPlaceholdersReadsBothSpellings(t *testing.T) {
	got := pathPlaceholders("v1/projects/{{project}}/zones/{zone}/x/{+name}/y")
	want := []string{"project", "zone", "name"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}

// TestAMagicModulesResourceIsPairedByCollectionNotJustByName. The real case,
// reduced: compute's Discovery collection "vpnGateways" is HA VPN, while
// magic-modules' resource NAMED "VpnGateway" is the classic gateway, whose
// base_url is a different collection entirely (targetVpnGateways); HA VPN is
// described by "HaVpnGateway", which matches no Discovery leaf. Pairing on
// the name alone produced a type whose base_url and self_link named two
// different resources, so every create orphaned.
func TestAMagicModulesResourceIsPairedByCollectionNotJustByName(t *testing.T) {
	in := writeRefFixture(t, map[string]string{
		"schemas/tinycompute.json": simpleDoc("tinycompute", "vpnGateways", "VpnGateway", ""),
		"mmv1/products/tinycompute/VpnGateway.yaml": `name: VpnGateway
description: the CLASSIC gateway, which lives in another collection entirely.
base_url: projects/{{project}}/targetVpnGateways
properties:
  - name: name
    type: String
    required: true
`,
		"mmv1/products/tinycompute/HaVpnGateway.yaml": `name: HaVpnGateway
description: the gateway this collection actually serves.
base_url: projects/{{project}}/vpnGateways
properties:
  - name: name
    type: String
    required: true
`,
	})
	res, err := Build(in)
	if err != nil {
		t.Fatal(err)
	}
	ty, ok := res.Catalog.Type("gcp.vpngateway")
	if !ok {
		t.Fatalf("gcp.vpngateway missing; catalog has %d types", len(res.Catalog.Types))
	}
	if !strings.Contains(ty.BaseURL, "/vpnGateways") || strings.Contains(ty.BaseURL, "targetVpnGateways") {
		t.Errorf("base_url = %q, want the collection the Discovery document's create posts to", ty.BaseURL)
	}
	if !strings.Contains(ty.Description, "actually serves") {
		t.Errorf("description = %q, so the type was still paired with the classic gateway", ty.Description)
	}
}

// TestATypeWhoseIDNamesAnotherCollectionDoesNotShip is the gate behind that
// pairing: when no magic-modules resource describes the right collection, the
// name match stands (it is what says whether the type has wire hooks) and the
// type is refused instead, with both templates named in gen/warnings.txt. A
// type that creates one resource and reads another cannot be vouched for, and
// the generator ships nothing it cannot vouch for.
func TestATypeWhoseIDNamesAnotherCollectionDoesNotShip(t *testing.T) {
	in := writeRefFixture(t, map[string]string{
		"schemas/tinycompute.json": simpleDoc("tinycompute", "vpnGateways", "VpnGateway", ""),
		"mmv1/products/tinycompute/VpnGateway.yaml": `name: VpnGateway
description: the CLASSIC gateway, which lives in another collection entirely.
base_url: projects/{{project}}/targetVpnGateways
properties:
  - name: name
    type: String
    required: true
`,
	})
	res, err := Build(in)
	if err != nil {
		t.Fatal(err)
	}
	if ty, ok := res.Catalog.Type("gcp.vpngateway"); ok {
		t.Errorf("a type that posts to %q and names %q shipped anyway", ty.BaseURL, ty.SelfLink)
	}
	var said string
	for _, w := range res.Warnings {
		if w.Resource == "VpnGateway" {
			said = w.Reason
		}
	}
	if said == "" {
		t.Fatal("the refusal was not recorded in the warnings at all, which is the one thing worse than shipping it")
	}
	if !strings.Contains(said, "targetVpnGateways") || !strings.Contains(said, "vpnGateways/") {
		t.Errorf("the warning names neither template it refused over: %q", said)
	}
}

// TestHierarchyRootIsTheLiteralPathSegment. ParentRoot is compared at
// runtime against the head of a Cloud Asset Inventory resource name
// ("folders/123/locations/..."), so it must be the segment verbatim.
// scopeSegment, which feeds the assigned type NAME, answers the same
// question in the singular and lowercased ("folder"), and folding the two
// together would make every comparison fail silently -- discovery would drop
// every folder-, organization- and billing-account-scoped resource rather
// than mislabel it, which is quieter and just as wrong.
func TestHierarchyRootIsTheLiteralPathSegment(t *testing.T) {
	cases := map[string]string{
		"projects":        "projects",
		"organizations":   "organizations",
		"folders":         "folders",
		"billingAccounts": "billingAccounts",
		"buckets":         "",
		"":                "",
	}
	for first, want := range cases {
		var path []string
		if first != "" {
			path = []string{first, "things"}
		}
		if got := hierarchyRoot(path); got != want {
			t.Errorf("hierarchyRoot(%v) = %q, want %q", path, got, want)
		}
	}
	if got := hierarchyRoot(nil); got != "" {
		t.Errorf("hierarchyRoot(nil) = %q", got)
	}
	// The two functions answer for the same input and must not be confused
	// for one another.
	if hierarchyRoot([]string{"folders", "x"}) == scopeSegment([]string{"folders", "x"}) {
		t.Error("hierarchyRoot and scopeSegment now agree, so one of them is answering the wrong question")
	}
}
