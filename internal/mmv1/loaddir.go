// Package mmv1: loading a whole product tree from disk.
//
// Split from parse.go so the directory walk and its error collection sit beside
// loaddir_test.go, which is where they are tested.
package mmv1

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// LoadError names one file that failed to parse, and why.
type LoadError struct {
	Path string
	Err  error
}

func (e LoadError) Error() string { return fmt.Sprintf("%s: %v", e.Path, e.Err) }

func (e LoadError) Unwrap() error { return e.Err }

// LoadDir reads every resource file under root, keyed by product directory.
//
// product.yaml is skipped: it describes the product, not a resource. A file
// that fails to read OR to parse is named in the returned []LoadError rather
// than silently skipped or allowed to abort the rest of the load. Collecting
// rather than aborting matters even beyond not hiding the failure: WalkDir
// visits files in a fixed (alphabetical) order, so aborting on the first bad
// file would silently skip every product that sorts after it — a far bigger,
// and much less visible, loss than naming the one bad file and moving on. The
// returned error is reserved for failures that mean the walk itself didn't
// happen at all, such as an unreadable root directory.
func LoadDir(root string) (map[string][]*Resource, []LoadError, error) {
	out := map[string][]*Resource{}
	var loadErrs []LoadError
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".yaml") {
			return nil
		}
		if filepath.Base(path) == "product.yaml" {
			return nil
		}
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			loadErrs = append(loadErrs, LoadError{Path: path, Err: readErr})
			return nil
		}
		r, parseErr := ParseResource(data)
		if parseErr != nil {
			loadErrs = append(loadErrs, LoadError{Path: path, Err: parseErr})
			return nil
		}
		r.Product = filepath.Base(filepath.Dir(path))
		out[r.Product] = append(out[r.Product], r)
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	return out, loadErrs, nil
}
