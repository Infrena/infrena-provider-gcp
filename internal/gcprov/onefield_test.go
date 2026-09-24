package gcprov

import (
	"context"
	"testing"

	"github.com/infrena/infrena-provider-gcp/internal/catalog"
	"github.com/infrena/infrena-provider-gcp/internal/gcpfake"
	"github.com/infrena/infrena-provider-gcp/internal/gcptest"
	"github.com/infrena/infrena/pkg/resource"
	"github.com/infrena/infrena/pkg/value"
)

// oneFieldWidget is a fingerprint-locked widget whose API refuses a patch
// changing more than one field, the way compute's subnetworks do. The fake
// enforces both the lock and the one-field rule.
func oneFieldWidget(t *testing.T, oneField bool) (*gcpfake.Server, *Provider) {
	t.Helper()
	s, _, ty, path := lockedWidget(t)
	s.OneFieldPerPatch(path)
	ty.PatchOneField = oneField
	ty.Attributes["description"] = &catalog.Attr{Canonical: "description", Kind: value.KindString}
	return s, testProviderWithCatalog(t, s, &catalog.Catalog{Types: []*catalog.Type{ty}})
}

func twoFieldsChanged() *resource.DesiredResource {
	return widgetDesired(map[string]any{
		"project": "p", "region": "r", "name": "one", "sizeGb": int64(20), "description": "resized",
	})
}

// TestAOneFieldAPIGetsOnePatchPerField. compute's subnetworks answered a
// patch of secondaryIpRanges and logConfig together with "Only one field at
// a time can be modified in the request" (live run, 2026-09-24). Each field
// goes in its own patch, and the second carries the fingerprint the first
// left behind, or it is a 412.
func TestAOneFieldAPIGetsOnePatchPerField(t *testing.T) {
	gcptest.Isolate(t)
	s, p := oneFieldWidget(t, true)
	defer s.Close()

	st, err := p.Update(context.Background(), lockedState(), twoFieldsChanged())
	if err != nil {
		t.Fatalf("a two-field change to a one-field-per-patch type: %v", err)
	}
	var patches int
	for _, r := range s.Requests() {
		if r.Method == "PATCH" {
			patches++
		}
	}
	if patches != 2 {
		t.Errorf("sent %d patches, want one per changed field", patches)
	}
	got, _ := s.Get("/v1/projects/p/locations/r/widgets/one")
	if got["sizeGb"] != float64(20) || got["description"] != "resized" {
		t.Errorf("google holds %v, want both changes applied", got)
	}
	if st == nil {
		t.Fatal("no state after the update")
	}
}

// TestTheFakeRefusesATwoFieldPatch is the fixture's own check: without the
// flag the same change is one patch, and the fake refuses it as compute
// does. If this passed, the test above would prove nothing.
func TestTheFakeRefusesATwoFieldPatch(t *testing.T) {
	gcptest.Isolate(t)
	s, p := oneFieldWidget(t, false)
	defer s.Close()

	if _, err := p.Update(context.Background(), lockedState(), twoFieldsChanged()); err == nil {
		t.Fatal("a two-field patch was accepted; the fake no longer models the refusal")
	}
}
