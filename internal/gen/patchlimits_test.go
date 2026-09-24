package gen

import (
	"testing"

	"github.com/infrena/infrena-provider-gcp/internal/catalog"
	"github.com/infrena/infrena-provider-gcp/internal/disco"
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

func m(path, param, pattern string) *disco.Method {
	return &disco.Method{Path: path, Parameters: map[string]*disco.Parameter{param: {Location: "path", Pattern: pattern}}}
}

// TestTwoSpellingsOfOneAddressAreOneAddress. Pub/Sub's topics.get is
// "v1/{+topic}" and topics.patch is "v1/{+name}", both with the pattern
// "^projects/[^/]+/topics/[^/]+$". A string comparison of the paths refused the
// update envelope for both Pub/Sub types on the placeholder's name alone.
func TestTwoSpellingsOfOneAddressAreOneAddress(t *testing.T) {
	const topic = "^projects/[^/]+/topics/[^/]+$"
	if !sameAddress(m("v1/{+name}", "name", topic), m("v1/{+topic}", "topic", topic)) {
		t.Error("the same address under two placeholder names compared unequal")
	}
	if sameAddress(m("v1/{+name}", "name", topic), m("v1/{+name}:detach", "name", topic)) {
		t.Error("a custom method at a different address compared equal")
	}
	if sameAddress(m("v1/{+name}", "name", topic), m("v1/{+name}", "name", "^projects/[^/]+/subscriptions/[^/]+$")) {
		t.Error("two different collections compared equal")
	}
}

// TestAnUpdateUrlNamingTheAPIsAddressIsRecognised. magic-modules writes
// "projects/{{project}}/topics/{{name}}" meaning the SHORT name; the API's
// patch is "v1/{+name}" over the full one. They are the same address, and
// recognising that is what lets the update go to the API's spelling.
func TestAnUpdateUrlNamingTheAPIsAddressIsRecognised(t *testing.T) {
	patch := m("v1/{+name}", "name", "^projects/[^/]+/topics/[^/]+$")
	if !templateAddressesMethod("projects/{{project}}/topics/{{name}}", patch) {
		t.Error("magic-modules' topic update_url was not recognised as the patch's address")
	}
	if templateAddressesMethod("projects/{{project}}/subscriptions/{{name}}", patch) {
		t.Error("a url for a different collection was taken as the patch's address")
	}
}
