package gen

import (
	"os"
	"path/filepath"
	"testing"
)

func TestARulingWithoutAReasonIsRefused(t *testing.T) {
	path := filepath.Join(t.TempDir(), "overlay.yaml")
	os.WriteFile(path, []byte("rulings:\n  a/B:\n    hooks: [encoder]\n"), 0o644)
	if _, err := LoadOverlay(path); err == nil {
		t.Error("a ruling with no note was accepted")
	}
}

func TestARulingThatRulesOnNothingIsRefused(t *testing.T) {
	path := filepath.Join(t.TempDir(), "overlay.yaml")
	os.WriteFile(path, []byte("rulings:\n  a/B:\n    note: looks fine to me\n"), 0o644)
	if _, err := LoadOverlay(path); err == nil {
		t.Error("a ruling naming no hooks and no read_via was accepted")
	}
}

func TestTheRepositoryOverlayIsValid(t *testing.T) {
	o, err := LoadOverlay("../../gen/overlay.yaml")
	if err != nil {
		t.Fatalf("gen/overlay.yaml: %v", err)
	}
	if _, ok := o.Rulings["cloudresourcemanager/TagBinding"]; !ok {
		t.Error("the tagBindings ruling required by G6 is missing")
	}
	if len(o.DiscoverDefault) == 0 {
		t.Error("discover_default is empty, so the fallback path would scan nothing")
	}
}
