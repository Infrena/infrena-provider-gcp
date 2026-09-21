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
	byProduct, loadErrs, err := LoadDir("../../gen/mmv1/products")
	if err != nil {
		t.Fatalf("LoadDir: %v", err)
	}
	// Every file in the real corpus is expected to parse: the whole point of
	// this package is not to lose resources. A pass here that only logs
	// loadErrs instead of asserting on them would let files start failing to
	// parse — up to the total<900 floor below, roughly 42 of them — without
	// ever failing the test. That is the exact defect class this package
	// exists to avoid, just moved into the test itself.
	if len(loadErrs) != 0 {
		for _, le := range loadErrs {
			t.Logf("unparseable: %v", le)
		}
		t.Errorf("%d files failed to load; see log for paths and errors", len(loadErrs))
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
	pct := 100 * float64(hooked) / float64(total)
	t.Logf("products=%d resources=%d unparseable=%d hooked=%d (%.1f%%)",
		len(byProduct), total, len(loadErrs), hooked, pct)
	if total < 900 {
		t.Errorf("%d resources, expected about 942; did the vendor copy fail?", total)
	}
	// This band is a DRIFT DETECTOR for vendor bumps, not a regression guard
	// for the wireHooks list: sabotage-1 evidence (adding "constants" to
	// wireHooks) moved the aggregate from 45.3% to 48%, comfortably inside a
	// 35-55% band, and only TestOnlyWireAffectingHooksAreReported caught it.
	// Tightened to bracket the measured 427/942 = 45.3% instead. If a real
	// vendor bump moves the true figure outside 43-47%, that's a thing to
	// look at — do not widen the band to make it pass.
	if pct < 43 || pct > 47 {
		t.Errorf("hooked share is %.1f%%, expected 43-47%% around the measured 45.3%%; the tier split assumes this", pct)
	}
}
