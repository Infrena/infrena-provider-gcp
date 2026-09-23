package gcpplugin

import (
	"os"
	"regexp"
	"strconv"
	"testing"

	"github.com/infrena/infrena-provider-gcp/internal/catalog"
)

// TestTheReadmeTypeCountIsNotStale. The README tells a reader how much of GCP
// this plugin covers, and that number is the first thing they use to decide
// whether it is worth their time.
//
// Prose about a generated artefact rots silently, and this repository has
// already proved it twice in a day: the design spec carried "roughly 500 types"
// from before anything had been generated, and plugin.yaml carried a measured
// element count that was wrong within hours of being written. Neither had
// anything that would fail when the catalog moved. This does.
//
// Ten percent, not exact: the catalog moves when Google edits a Discovery
// document or someone writes a ruling, and a test that failed on every such
// change would be edited to shut it up rather than read. A drift large enough
// to mislead trips it; ordinary week-to-week movement does not.
func TestTheReadmeTypeCountIsNotStale(t *testing.T) {
	data, err := os.ReadFile("../../README.md")
	if err != nil {
		t.Fatal(err)
	}
	c, err := catalog.Load()
	if err != nil {
		t.Fatal(err)
	}
	actual := len(c.Types)

	// Every number the README presents as a type count, wherever it says it.
	claims := regexp.MustCompile(`\*\*(\d+)\s+resource types\*\*|\b(\d+) types ship\b`).
		FindAllStringSubmatch(string(data), -1)
	if len(claims) == 0 {
		t.Fatal("the README makes no claim about how many types are served, so a reader cannot " +
			"tell what this plugin covers and this test is guarding nothing")
	}
	for _, m := range claims {
		raw := m[1]
		if raw == "" {
			raw = m[2]
		}
		claimed, err := strconv.Atoi(raw)
		if err != nil {
			t.Fatalf("unparsable type count %q in the README", raw)
		}
		if drift := float64(claimed-actual) / float64(actual); drift > 0.1 || drift < -0.1 {
			t.Errorf("the README says %d types; the catalog has %d, which is more than 10%% out. "+
				"Regenerate the count rather than widening this test", claimed, actual)
		}
	}
}
