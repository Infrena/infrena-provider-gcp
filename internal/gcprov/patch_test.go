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

func TestTheMaskNamesOnlyWhatChanged(t *testing.T) {
	ty := widgetType()
	current := attrsMixed(map[string]any{"name": "one", "sizeGb": int64(10), "tier": "BASIC"})
	desired := attrsMixed(map[string]any{"name": "one", "sizeGb": int64(20), "tier": "BASIC"})

	body, mask := BuildMask(ty, current, desired)
	if len(mask) != 1 || mask[0] != "sizeGb" {
		t.Errorf("mask = %v, want [sizeGb]", mask)
	}
	if body["sizeGb"] != int64(20) {
		t.Errorf("body = %v, want the new size", body)
	}
	if _, present := body["tier"]; present {
		t.Error("an unchanged field is in the patch body")
	}
}

// TestAnAttributeDroppedFromConfigurationProducesNoMaskEntry -- spec §5.4 and
// PLAN §14.1. infrena cannot tell "never set" from "no longer configured", so
// both keep GCP's current value.
func TestAnAttributeDroppedFromConfigurationProducesNoMaskEntry(t *testing.T) {
	ty := widgetType()
	current := attrsMixed(map[string]any{"name": "one", "sizeGb": int64(10), "tier": "STANDARD"})
	desired := attrsMixed(map[string]any{"name": "one", "sizeGb": int64(10)}) // tier gone

	body, mask := BuildMask(ty, current, desired)
	if len(mask) != 0 {
		t.Errorf("mask = %v, want empty: dropping an attribute must not remove it", mask)
	}
	if _, present := body["tier"]; present {
		t.Error("the dropped attribute is in the patch body, which would reset it")
	}
}

// TestAnOutputOnlyAttributeIsNeverInTheMask. GCP rejects a patch touching
// one, and a mask naming a read-only field fails the whole request.
func TestAnOutputOnlyAttributeIsNeverInTheMask(t *testing.T) {
	ty := widgetType()
	current := attrsMixed(map[string]any{"name": "one", "createTime": "2026-01-01T00:00:00Z"})
	desired := attrsMixed(map[string]any{"name": "one", "createTime": "2026-09-21T00:00:00Z"})
	body, mask := BuildMask(ty, current, desired)
	for _, m := range mask {
		if m == "createTime" {
			t.Error("an output-only field reached the mask; GCP would reject the whole patch")
		}
	}
	if _, present := body["createTime"]; present {
		t.Error("an output-only field reached the patch body")
	}
}

func TestANestedChangeIsMaskedAtItsPath(t *testing.T) {
	ty := widgetType()
	current := map[string]value.Value{"config": mapValue(map[string]any{"mode": "A", "size": int64(1)})}
	desired := map[string]value.Value{"config": mapValue(map[string]any{"mode": "B", "size": int64(1)})}
	body, mask := BuildMask(ty, current, desired)
	// A mask of "config" would replace the whole object and drop anything GCP
	// added inside it; the path is what makes the patch surgical.
	if len(mask) != 1 || mask[0] != "config.mode" {
		t.Errorf("mask = %v, want [config.mode]", mask)
	}
	sub, ok := body["config"].(map[string]any)
	if !ok {
		t.Fatalf("body[config] = %#v, want a nested object", body["config"])
	}
	if sub["mode"] != "B" {
		t.Errorf("body = %v, want the new mode", body)
	}
	if _, present := sub["size"]; present {
		t.Error("an unchanged nested field is in the patch body")
	}
}

// TestAUrlIdentifyingAttributeIsNeverInTheMask pins the exclusion that the
// 10 updatable types declaring a required, non-ForceNew `name` depend on
// (gcp.mesh, gcp.authzpolicy, gcp.networksecurity.addressgroup and seven
// others). Their APIs echo `name` back as the FULL resource name while
// configuration holds the short one, so current and desired disagree on
// every update -- and patching that difference asks GCP to rename the
// resource the url just addressed.
func TestAUrlIdentifyingAttributeIsNeverInTheMask(t *testing.T) {
	ty := widgetType()
	current := attrsMixed(map[string]any{
		"project": "p", "region": "r",
		"name":   "projects/p/locations/r/widgets/one", // as GCP echoes it
		"sizeGb": int64(10),
	})
	desired := attrsMixed(map[string]any{
		"project": "p", "region": "r",
		"name":   "one", // as configuration spells it
		"sizeGb": int64(20),
	})
	body, mask := BuildMask(ty, current, desired)
	for _, m := range mask {
		if m == "name" || m == "project" || m == "region" {
			t.Errorf("mask = %v: %q is an id segment the url already carries", mask, m)
		}
	}
	if _, present := body["name"]; present {
		t.Error("the patch body repeats the name the url already said")
	}
	if len(mask) != 1 || mask[0] != "sizeGb" {
		t.Errorf("mask = %v, want [sizeGb]", mask)
	}
}

// TestUpdateDiffsAgainstTheCurrentItWasGiven is the differential test. The
// AWS repo found this bug by running its e2e suite: with a stale `current`,
// drift corrected in configuration never gets patched, because the diff runs
// against the wrong side.
func TestUpdateDiffsAgainstTheCurrentItWasGiven(t *testing.T) {
	gcptest.Isolate(t)
	s := gcpfake.New(t)
	defer s.Close()
	// What is REALLY there: someone changed the size outside infrena.
	s.Seed("/v1/projects/p/locations/r/widgets/one",
		map[string]any{"name": "one", "sizeGb": float64(99)})
	p := testProviderWithCatalog(t, s, widgetCatalog())

	// The host hands Update the REFRESHED observation, which says 99.
	current := &resource.ResourceState{
		Type: "gcp.widget", ProviderID: "projects/p/locations/r/widgets/one",
		Attributes: attrsMixed(map[string]any{"project": "p", "region": "r", "name": "one", "sizeGb": int64(99)}),
	}
	desired := &resource.DesiredResource{
		Type:  "gcp.widget",
		Attrs: attrsMixed(map[string]any{"project": "p", "region": "r", "name": "one", "sizeGb": int64(10)}),
	}
	st, err := p.Update(context.Background(), current, desired)
	if err != nil {
		t.Fatal(err)
	}
	if st == nil {
		t.Fatal("Update returned (nil, nil), which the host treats as a contract violation")
	}
	got, _ := s.Get("/v1/projects/p/locations/r/widgets/one")
	if got["sizeGb"] != float64(10) {
		t.Errorf("sizeGb = %v, want 10: the drift was not corrected", got["sizeGb"])
	}
	// And no extra read was made before patching.
	var gets int
	for _, r := range s.Requests() {
		if r.Method == "GET" && contains(r.Path, "/widgets/one") {
			gets++
		}
	}
	if gets > 1 {
		t.Errorf("%d reads of the resource; Update must diff against what it was handed, not re-read", gets)
	}
}

// TestUpdateSendsTheMaskAsAQueryArgument pins the whole wire shape of a
// patch: the verb, the versioned url absURL builds, and the mask.
func TestUpdateSendsTheMaskAsAQueryArgument(t *testing.T) {
	gcptest.Isolate(t)
	s := gcpfake.New(t)
	defer s.Close()
	path := "/v1/projects/p/locations/r/widgets/one"
	s.Seed(path, map[string]any{"name": "one", "sizeGb": float64(10), "tier": "STANDARD"})
	p := testProviderWithCatalog(t, s, widgetCatalog())

	if _, err := p.Update(context.Background(), widgetState(), widgetDesired(map[string]any{
		"project": "p", "region": "r", "name": "one", "sizeGb": int64(20),
	})); err != nil {
		t.Fatal(err)
	}

	patch := requestOf(t, s, "PATCH", path)
	if got := patch.Query.Get("updateMask"); got != "sizeGb" {
		t.Errorf("updateMask = %q, want %q", got, "sizeGb")
	}
	stored, _ := s.Get(path)
	if stored["sizeGb"] != float64(20) {
		t.Errorf("sizeGb = %v, want 20", stored["sizeGb"])
	}
	if stored["tier"] != "STANDARD" {
		t.Errorf("tier = %v: a field outside the mask must not change", stored["tier"])
	}
}

// TestATypeThatTakesNoUpdateMaskIsNotSentOne. 15 of the 86 updatable types
// PATCH without a mask (gcp.firewall, gcp.connection, gcp.router and 12
// others). Appending the parameter anyway sends a query argument those APIs
// never asked for.
func TestATypeThatTakesNoUpdateMaskIsNotSentOne(t *testing.T) {
	gcptest.Isolate(t)
	s := gcpfake.New(t)
	defer s.Close()
	path := "/v1/projects/p/locations/r/widgets/one"
	s.Seed(path, map[string]any{"name": "one", "sizeGb": float64(10)})

	ty := widgetType()
	ty.UpdateMask = false
	p := testProviderWithCatalog(t, s, &catalog.Catalog{Types: []*catalog.Type{ty}})

	if _, err := p.Update(context.Background(), widgetState(), widgetDesired(map[string]any{
		"project": "p", "region": "r", "name": "one", "sizeGb": int64(20),
	})); err != nil {
		t.Fatal(err)
	}
	patch := requestOf(t, s, "PATCH", path)
	if _, present := patch.Query["updateMask"]; present {
		t.Errorf("a type with no update_mask was sent updateMask=%q", patch.Query.Get("updateMask"))
	}
	stored, _ := s.Get(path)
	if stored["sizeGb"] != float64(20) {
		t.Errorf("sizeGb = %v, want 20", stored["sizeGb"])
	}
}

// TestAnUnchangedResourceIsNotPatchedAtAll. An empty mask means nothing
// changed, and an empty patch is a request against a project-wide quota for
// no effect -- some APIs reject it outright.
func TestAnUnchangedResourceIsNotPatchedAtAll(t *testing.T) {
	gcptest.Isolate(t)
	s := gcpfake.New(t)
	defer s.Close()
	s.Seed("/v1/projects/p/locations/r/widgets/one", map[string]any{"name": "one", "sizeGb": float64(10)})
	p := testProviderWithCatalog(t, s, widgetCatalog())

	same := map[string]any{"project": "p", "region": "r", "name": "one", "sizeGb": int64(10)}
	st, err := p.Update(context.Background(),
		&resource.ResourceState{
			Type: "gcp.widget", ProviderID: "projects/p/locations/r/widgets/one",
			Attributes: attrsMixed(same),
		},
		widgetDesired(same))
	if err != nil {
		t.Fatal(err)
	}
	if st == nil {
		t.Fatal("Update returned (nil, nil) for a no-op, which the host treats as a contract violation")
	}
	for _, r := range s.Requests() {
		t.Errorf("a no-op update sent %s %s", r.Method, r.Path)
	}
}

// TestUpdateRefusesATypeWithNoUpdateMethod. 147 of the 233 types publish no
// update method, and Capabilities.Update is derived from exactly that, so
// the host replaces them instead. Defaulting to PATCH would quietly patch an
// API that has no update method if that derivation ever broke.
func TestUpdateRefusesATypeWithNoUpdateMethod(t *testing.T) {
	gcptest.Isolate(t)
	s := gcpfake.New(t)
	defer s.Close()
	s.Seed("/v1/projects/p/locations/r/widgets/one", map[string]any{"name": "one", "sizeGb": float64(10)})

	ty := widgetType()
	ty.UpdateVerb = ""
	p := testProviderWithCatalog(t, s, &catalog.Catalog{Types: []*catalog.Type{ty}})

	_, err := p.Update(context.Background(), widgetState(), widgetDesired(map[string]any{
		"project": "p", "region": "r", "name": "one", "sizeGb": int64(20),
	}))
	if err == nil {
		t.Fatal("a type with no update method was patched anyway")
	}
	if !contains(err.Error(), "no update method") {
		t.Errorf("error = %q, which does not say why", err)
	}
	for _, r := range s.Requests() {
		t.Errorf("a refused update still sent %s %s", r.Method, r.Path)
	}
}

// TestUpdateReportsStateWhenTheOperationFails. The orphan rule binds Update
// as well as Create: the patch may well have landed, so an error return that
// tells the host nothing happened leaves state describing a resource that no
// longer matches. Report the truthful state instead and let the next plan
// converge.
func TestUpdateReportsStateWhenTheOperationFails(t *testing.T) {
	gcptest.Isolate(t)
	s := gcpfake.New(t)
	defer s.Close()
	path := "/v1/projects/p/locations/r/widgets/one"
	s.Seed(path, map[string]any{"name": "one", "sizeGb": float64(10)})
	s.SetOperationStyle(gcpfake.OpLongRunning)
	s.CreateThenFailOperation(path, "INTERNAL", "post-update step failed")

	ty := widgetType()
	ty.Await = catalog.AwaitLongRunning
	ty.OperationPollPath = "{+name}"
	p := testProviderWithCatalog(t, s, &catalog.Catalog{Types: []*catalog.Type{ty}})

	st, err := p.Update(context.Background(), widgetState(), widgetDesired(map[string]any{
		"project": "p", "region": "r", "name": "one", "sizeGb": int64(20),
	}))
	if err != nil {
		t.Fatalf("Update returned an error for a patch that took effect: %v", err)
	}
	if st == nil {
		t.Fatal("Update returned (nil, nil) after a failed operation")
	}
	if got, _ := st.Attributes["sizeGb"].AsInt(); got != 20 {
		t.Errorf("reported sizeGb = %v, want the patched 20", st.Attributes["sizeGb"])
	}
}

// TestUpdateErrorsRatherThanReportingNothingWhenTheReadbackFails. Read's own
// "believed absent" answer is (nil, nil), and returning that from Update is
// a contract violation the host stops the whole run over: it cannot tell it
// apart from a call that took effect and recorded nothing.
func TestUpdateErrorsRatherThanReportingNothingWhenTheReadbackFails(t *testing.T) {
	gcptest.Isolate(t)
	s := gcpfake.New(t)
	defer s.Close()
	path := "/v1/projects/p/locations/r/widgets/one"
	s.Seed(path, map[string]any{"name": "one", "sizeGb": float64(10)})
	p := testProviderWithCatalog(t, s, widgetCatalog())

	// The patch lands; the readback that follows it does not.
	s.OnRequest(path, func() { s.FailNext(403, "PERMISSION_DENIED", "no read access") })

	st, err := p.Update(context.Background(), widgetState(), widgetDesired(map[string]any{
		"project": "p", "region": "r", "name": "one", "sizeGb": int64(20),
	}))
	if st == nil && err == nil {
		t.Fatal("Update returned (nil, nil), which stops the host's whole run")
	}
	if err == nil {
		t.Fatalf("no error for a readback that failed: %v", st)
	}
	if !contains(err.Error(), "cannot be read back") {
		t.Errorf("error = %q, which does not say what went wrong", err)
	}
}

// TestUpdateErrorsRatherThanReportingNothingWhenTheResourceIsGone is the
// other half of the same contract: Read answers a resource it believes
// absent with (nil, nil), and passing that straight through would be the
// same contract violation -- this time for something that really did vanish
// out from under the patch.
func TestUpdateErrorsRatherThanReportingNothingWhenTheResourceIsGone(t *testing.T) {
	gcptest.Isolate(t)
	s := gcpfake.New(t)
	defer s.Close()
	path := "/v1/projects/p/locations/r/widgets/one"
	s.Seed(path, map[string]any{"name": "one", "sizeGb": float64(10)})
	// The patch lands (handlePatch never consults notFoundRemaining); every
	// readback after it 404s, for longer than Read's notFoundPatience, so
	// Read genuinely believes the widget gone.
	s.NotFoundTimes(path, 1000)
	p := testProviderWithCatalog(t, s, widgetCatalog())

	st, err := p.Update(context.Background(), widgetState(), widgetDesired(map[string]any{
		"project": "p", "region": "r", "name": "one", "sizeGb": int64(20),
	}))
	if st == nil && err == nil {
		t.Fatal("Update returned (nil, nil), which stops the host's whole run")
	}
	if err == nil {
		t.Fatalf("no error for a resource that vanished: %v", st)
	}
	if !contains(err.Error(), "no longer there") {
		t.Errorf("error = %q, which does not say what went wrong", err)
	}
}

// TestANestedPatchLeavesUnmodelledSiblingsAlone is the same rule as
// TestANestedChangeIsMaskedAtItsPath, measured on the wire rather than on
// the mask: GCP merges only what the mask names, so a field inside the
// object that this provider never modelled survives a nested change.
func TestANestedPatchLeavesUnmodelledSiblingsAlone(t *testing.T) {
	gcptest.Isolate(t)
	s := gcpfake.New(t)
	defer s.Close()
	path := "/v1/projects/p/locations/r/widgets/one"
	s.Seed(path, map[string]any{
		"name": "one",
		"config": map[string]any{
			"mode":       "A",
			"size":       float64(1),
			"addedByGCP": "something we never modelled",
		},
	})
	p := testProviderWithCatalog(t, s, widgetCatalog())

	current := &resource.ResourceState{
		Type: "gcp.widget", ProviderID: "projects/p/locations/r/widgets/one",
		Attributes: map[string]value.Value{
			"project": value.String("p", value.SourceProvider),
			"region":  value.String("r", value.SourceProvider),
			"name":    value.String("one", value.SourceProvider),
			"config":  mapValue(map[string]any{"mode": "A", "size": int64(1)}),
		},
	}
	desired := &resource.DesiredResource{
		Type: "gcp.widget",
		Attrs: map[string]value.Value{
			"project": value.String("p", value.SourceExplicit),
			"region":  value.String("r", value.SourceExplicit),
			"name":    value.String("one", value.SourceExplicit),
			"config":  mapValue(map[string]any{"mode": "B", "size": int64(1)}),
		},
	}
	if _, err := p.Update(context.Background(), current, desired); err != nil {
		t.Fatal(err)
	}

	patch := requestOf(t, s, "PATCH", path)
	if got := patch.Query.Get("updateMask"); got != "config.mode" {
		t.Errorf("updateMask = %q, want %q", got, "config.mode")
	}
	stored, _ := s.Get(path)
	cfg, ok := stored["config"].(map[string]any)
	if !ok {
		t.Fatalf("config = %#v", stored["config"])
	}
	if cfg["mode"] != "B" {
		t.Errorf("mode = %v, want B", cfg["mode"])
	}
	if cfg["addedByGCP"] != "something we never modelled" {
		t.Errorf("config = %v: masking the parent dropped what GCP put inside it", cfg)
	}
	if cfg["size"] != float64(1) {
		t.Errorf("size = %v, want the untouched 1", cfg["size"])
	}
}

// widgetState is the current state every wire-level patch test starts from:
// the widget at "one", sized 10.
func widgetState() *resource.ResourceState {
	return &resource.ResourceState{
		Type:       "gcp.widget",
		ProviderID: "projects/p/locations/r/widgets/one",
		Attributes: attrsMixed(map[string]any{
			"project": "p", "region": "r", "name": "one", "sizeGb": int64(10),
		}),
	}
}

// widgetDesired wraps plain Go values as the desired resource for gcp.widget.
func widgetDesired(kv map[string]any) *resource.DesiredResource {
	return &resource.DesiredResource{Type: "gcp.widget", Attrs: attrsMixed(kv)}
}

// requestOf returns the one request the fake received with this method and
// path, failing the test when there is not exactly one -- a patch sent twice
// is as wrong as a patch never sent.
func requestOf(t *testing.T, s *gcpfake.Server, method, path string) gcpfake.Request {
	t.Helper()
	var found []gcpfake.Request
	for _, r := range s.Requests() {
		if r.Method == method && r.Path == path {
			found = append(found, r)
		}
	}
	if len(found) != 1 {
		t.Fatalf("%d %s requests to %s, want exactly 1 (all: %v)", len(found), method, path, s.Requests())
	}
	return found[0]
}
