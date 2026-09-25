package gen

import (
	"os"
	"path/filepath"
	"testing"
)

func TestARulingWithoutAReasonIsRefused(t *testing.T) {
	path := filepath.Join(t.TempDir(), "overlay.yaml")
	os.WriteFile(path, []byte("rulings:\n  a/B:\n    hooks: [encoder]\n"), 0o644)
	if _, err := LoadOverlay(path, t.TempDir()); err == nil {
		t.Error("a ruling with no note was accepted")
	}
}

func TestARulingThatRulesOnNothingIsRefused(t *testing.T) {
	path := filepath.Join(t.TempDir(), "overlay.yaml")
	os.WriteFile(path, []byte("rulings:\n  a/B:\n    note: looks fine to me\n"), 0o644)
	if _, err := LoadOverlay(path, t.TempDir()); err == nil {
		t.Error("a ruling naming no hooks and no read_via was accepted")
	}
}

// TestAProductAliasNamingAMissingDirectoryIsRefused. A typo'd or stale
// directory name here would silently reproduce the exact bug ProductAliases
// exists to fix: matchResource would just never find anything through it,
// with nothing telling anyone that happened.
// TestARulingThatOnlyCorrectsAFactIsAccepted. BigQuery's Table has no hooks
// and one wrong flag (requirePartitionFilter marked output); a ruling that
// only says settable rules on something.
func TestARulingThatOnlyCorrectsAFactIsAccepted(t *testing.T) {
	path := filepath.Join(t.TempDir(), "overlay.yaml")
	os.WriteFile(path, []byte("rulings:\n  bigquery/Table:\n    hooks: []\n    settable: [requirePartitionFilter]\n    note: a flag magic-modules has wrong\n"), 0o644)
	if _, err := LoadOverlay(path, t.TempDir()); err != nil {
		t.Fatalf("a ruling that corrects a fact was refused: %v", err)
	}
}

func TestAProductAliasNamingAMissingDirectoryIsRefused(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "overlay.yaml")
	os.WriteFile(path, []byte("product_aliases:\n  cloudresourcemanager: [nonexistent]\n"), 0o644)
	if _, err := LoadOverlay(path, filepath.Join(dir, "mmv1", "products")); err == nil {
		t.Error("an alias naming a directory that does not exist was accepted")
	}
}

func TestTheRepositoryOverlayIsValid(t *testing.T) {
	o, err := LoadOverlay("../../gen/overlay.yaml", "../../gen/mmv1/products")
	if err != nil {
		t.Fatalf("gen/overlay.yaml: %v", err)
	}
	if _, ok := o.Rulings["cloudresourcemanager/TagBinding"]; !ok {
		t.Error("the tagBindings ruling required by G6 is missing")
	}
	if len(o.DiscoverDefault) == 0 {
		t.Error("discover_default is empty, so the fallback path would scan nothing")
	}
	if _, ok := o.ProductAliases["cloudresourcemanager"]; !ok {
		t.Error("the cloudresourcemanager product alias is missing; TagBinding lives under products/tags, " +
			"not products/cloudresourcemanager, and matchResource cannot find it without this")
	}
}
