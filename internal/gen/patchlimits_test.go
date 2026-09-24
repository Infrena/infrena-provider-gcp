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
		"An up-to-date fingerprint must be provided in order to update the Subnetwork, otherwise the request will fail with error 412 conditionNotMet.":                                                                    "fingerprint",
		"You must always provide an up-to-date fingerprint hash in order to update the instance.":                                                                                                                          "fingerprint",
		"This field will be ignored when\ninserting a BackendService. An up-to-date fingerprint must be provided in\norder to update the BackendService, otherwise the request will fail with\nerror 412 conditionNotMet.": "fingerprint",
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

// TestARestrictionIsReadInEveryShapeGoogleWritesIt. Each sentence is copied from
// a pinned Discovery document. The forwarding rules' shape was missed until
// 2026-09-24, which would have let a patch the API ignores stand in for a
// replacement. The unrestricted sentences are there so a pattern that matches
// every JSON merge patch description fails here, not in the catalog.
func TestARestrictionIsReadInEveryShapeGoogleWritesIt(t *testing.T) {
	for desc, want := range map[string]bool{
		"Updates the specified forwarding rule with the data included in the request. This method supportsPATCH semantics and uses theJSON merge patch format and processing rules. Currently, you can only patch the network_tier field.": true,
		"Patches the specified network with the data included in the request. Only routingConfig can be modified.":                                                                                                                         true,
		"Patches the specified TargetHttpsProxy resource with the data included in the request. This method supports PATCH semantics and usesJSON merge patch format and processing rules.":                                                false,
		"Patches the specified SSL policy with the data included in the request.":                                                                                                                                                          false,
	} {
		col := disco.Collection{Methods: map[string]*disco.Method{"patch": {Description: desc}}}
		if got, _ := restrictedPatch(col); got != want {
			t.Errorf("restrictedPatch = %v, want %v, for %q", got, want, desc)
		}
	}
}
