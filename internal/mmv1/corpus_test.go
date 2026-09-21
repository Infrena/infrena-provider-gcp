package mmv1

import (
	"os"
	"testing"
)

// TestTheVendoredCorpusMatchesWhatThePlanMeasured. These numbers are load-bearing:
// the tier mechanism (spec §4.1) exists because of the hook count, and G5's scope
// comes from it. If a vendor bump moves them a lot, that is a thing to look at, not
// a thing to re-baseline silently.
func TestTheVendoredCorpusMatchesWhatThePlanMeasured(t *testing.T) {
	if _, err := os.Stat("../../gen/mmv1/products"); os.IsNotExist(err) {
		t.Skip("gen/mmv1 not vendored")
	}
	byProduct, err := LoadDir("../../gen/mmv1/products")
	if err != nil {
		t.Fatalf("LoadDir: %v", err)
	}
	var total, hooked int
	for _, rs := range byProduct {
		for _, r := range rs {
			total++
			if len(r.WireHooks()) > 0 {
				hooked++
			}
		}
	}
	t.Logf("products=%d resources=%d hooked=%d (%.0f%%)",
		len(byProduct), total, hooked, 100*float64(hooked)/float64(total))
	if total < 900 {
		t.Errorf("%d resources, expected about 942; did the vendor copy fail?", total)
	}
	if pct := 100 * float64(hooked) / float64(total); pct < 35 || pct > 55 {
		t.Errorf("hooked share is %.0f%%, expected about 45%%; the tier split assumes this", pct)
	}
}
