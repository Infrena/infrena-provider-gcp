package mmv1

import (
	"os"
	"path/filepath"
	"testing"
)

// TestLoadDirNamesBadFilesWithoutDroppingGoodOnes. A file that fails to decode
// (a genuine type mismatch, not just an odd-but-representable custom_code shape)
// must not abort the whole walk: alphabetical WalkDir order means aborting on
// the first bad file would silently skip every product after it, which is a
// bigger, less visible version of "silently drops a type from the catalog"
// than naming the one bad file and moving on.
func TestLoadDirNamesBadFilesWithoutDroppingGoodOnes(t *testing.T) {
	dir := t.TempDir()
	good := "name: Good\nbase_url: projects/{{project}}/goods\n"
	bad := "name: Bad\nupdate_mask: not-a-boolean\n"
	if err := os.WriteFile(filepath.Join(dir, "Good.yaml"), []byte(good), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "Bad.yaml"), []byte(bad), 0o644); err != nil {
		t.Fatal(err)
	}

	byProduct, loadErrs, err := LoadDir(dir)
	if err != nil {
		t.Fatalf("LoadDir: %v", err)
	}

	product := filepath.Base(dir)
	found := false
	for _, r := range byProduct[product] {
		if r.Name == "Good" {
			found = true
		}
	}
	if !found {
		t.Errorf("Good.yaml was not loaded; byProduct = %+v", byProduct)
	}

	if len(loadErrs) != 1 {
		t.Fatalf("loadErrs = %+v, want exactly one", loadErrs)
	}
	if filepath.Base(loadErrs[0].Path) != "Bad.yaml" {
		t.Errorf("loadErrs[0].Path = %q, want Bad.yaml", loadErrs[0].Path)
	}
}

// TestLoadDirNamesUnreadableFilesWithoutAborting. A file that fails to READ,
// not just to parse, must not abort the walk either: os.ReadFile erroring
// used to propagate straight out of the WalkFunc, discarding everything
// collected so far, which contradicted LoadDir's own doc comment.
func TestLoadDirNamesUnreadableFilesWithoutAborting(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: permission bits don't block root's own reads")
	}
	dir := t.TempDir()
	good := "name: Good\nbase_url: projects/{{project}}/goods\n"
	if err := os.WriteFile(filepath.Join(dir, "Good.yaml"), []byte(good), 0o644); err != nil {
		t.Fatal(err)
	}
	unreadable := filepath.Join(dir, "Unreadable.yaml")
	if err := os.WriteFile(unreadable, []byte("name: Unreadable\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(unreadable, 0o000); err != nil {
		t.Fatal(err)
	}
	// t.TempDir's own cleanup needs to read the directory it removes; restore
	// permissions so that cleanup doesn't itself fail.
	t.Cleanup(func() { _ = os.Chmod(unreadable, 0o644) })

	byProduct, loadErrs, err := LoadDir(dir)
	if err != nil {
		t.Fatalf("LoadDir: %v", err)
	}

	product := filepath.Base(dir)
	found := false
	for _, r := range byProduct[product] {
		if r.Name == "Good" {
			found = true
		}
	}
	if !found {
		t.Errorf("Good.yaml was not loaded; byProduct = %+v", byProduct)
	}

	if len(loadErrs) != 1 {
		t.Fatalf("loadErrs = %+v, want exactly one", loadErrs)
	}
	if filepath.Base(loadErrs[0].Path) != "Unreadable.yaml" {
		t.Errorf("loadErrs[0].Path = %q, want Unreadable.yaml", loadErrs[0].Path)
	}
}
