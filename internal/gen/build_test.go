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
