package catalog

import (
	_ "embed"
	"sync"
)

//go:embed catalog.json.gz
var blob []byte

var (
	once    sync.Once
	loaded  *Catalog
	loadErr error
)

// Load decodes the embedded catalog, once per process.
//
// Once, because every instance of the plugin serves the same types and the
// decompression is the plugin's whole startup cost — measured against the
// load-cost gate in Task 9.
func Load() (*Catalog, error) {
	once.Do(func() { loaded, loadErr = Decode(blob) })
	return loaded, loadErr
}
