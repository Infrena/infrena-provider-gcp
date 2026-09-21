package disco

import (
	"os"
	"testing"
)

// TestRealComputeDocument is a smoke test against the largest document GCP
// publishes (6.4 MB). It is skipped when the file is absent so the suite stays
// offline by default.
func TestRealComputeDocument(t *testing.T) {
	data, err := os.ReadFile("../../schemas/compute.json")
	if os.IsNotExist(err) {
		t.Skip("schemas/compute.json absent; run scripts/fetch-schemas")
	}
	if err != nil {
		t.Fatal(err)
	}
	d, err := Parse(data)
	if err != nil {
		t.Fatal(err)
	}
	if got := len(d.Collections()); got < 100 {
		t.Errorf("compute has %d collections, expected well over 100", got)
	}
	for name, s := range d.Schemas {
		if _, err := d.Resolve(s); err != nil {
			t.Fatalf("Resolve(%s): %v", name, err)
		}
	}
}
