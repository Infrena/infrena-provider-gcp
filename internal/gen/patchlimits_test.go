package gen

import (
	"testing"

	"github.com/infrena/infrena-provider-gcp/internal/catalog"
)

// TestTheLockFieldIsReadFromWhatTheAPISays. Only a top-level fingerprint that
// says it must be provided in order to update is a lock. labelFingerprint
// guards setLabels, a method this provider never calls, and a fingerprint
// that says nothing about updates is just data.
func TestTheLockFieldIsReadFromWhatTheAPISays(t *testing.T) {
	fp := func(desc string) map[string]*catalog.Attr {
		return map[string]*catalog.Attr{"fingerprint": {Canonical: "fingerprint", Description: desc}}
	}
	for desc, want := range map[string]string{
		"An up-to-date fingerprint must be provided in order to update the Subnetwork, otherwise the request will fail with error 412 conditionNotMet.": "fingerprint",
		"You must always provide an up-to-date fingerprint hash in order to update the instance.":                                                       "fingerprint",
		"A hash of the contents. Output only.": "",
	} {
		if got := lockFieldOf(fp(desc)); got != want {
			t.Errorf("%.40q...: got %q, want %q", desc, got, want)
		}
	}
	labels := map[string]*catalog.Attr{"labelFingerprint": {Canonical: "labelFingerprint",
		Description: "An up-to-date fingerprint must be provided in order to update labels."}}
	if got := lockFieldOf(labels); got != "" {
		t.Errorf("labelFingerprint taken as the update lock: %q", got)
	}
}
