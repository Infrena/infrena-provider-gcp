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
	// Resolving every real schema must truncate nothing. compute has no $ref
	// cycle at this revision (verified separately), so any truncation here
	// means the depth bound is firing on ordinary structural nesting instead
	// of $ref hops, silently dropping real fields from the catalog.
	total := 0
	for name, s := range d.Schemas {
		_, truncated, err := d.resolveCounted(s)
		if err != nil {
			t.Fatalf("Resolve(%s): %v", name, err)
		}
		total += truncated
	}
	if total != 0 {
		t.Errorf("resolving compute's schemas truncated %d subtrees; want 0 (no reachable $ref cycle exists in this document)", total)
	}
}
