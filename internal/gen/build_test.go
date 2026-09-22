package gen

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/infrena/infrena-provider-gcp/internal/catalog"
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
	for name, a := range ty.Attributes {
		if a.Output {
			continue
		}
		if !a.ForceNew {
			t.Errorf("gcp.frozen.%s is not ForceNew, but the ruling says all_force_new and the type has no patch method", name)
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
