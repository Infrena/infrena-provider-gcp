package gcprov

import (
	"context"
	"strings"
	"testing"

	"github.com/infrena/infrena-provider-gcp/internal/catalog"
	"github.com/infrena/infrena-provider-gcp/internal/gcpfake"
	"github.com/infrena/infrena-provider-gcp/internal/gcptest"
	"github.com/infrena/infrena/pkg/resource"
	"github.com/infrena/infrena/pkg/value"
)

const deletePath = "/v1/projects/p/locations/r/widgets/one"

// deletingWidget is a widget seeded in the fake, with a create that awaits
// createAwait and a delete that awaits deleteAwait. The two differ for nine
// shipped types, and a delete that reused the create's answered wrongly.
func deletingWidget(t *testing.T, style gcpfake.OperationStyle, createAwait, deleteAwait catalog.AwaitKind) (*gcpfake.Server, *Provider) {
	t.Helper()
	s := gcpfake.New(t)
	s.SetOperationStyle(style)
	s.Seed(deletePath, map[string]any{"name": "one", "sizeGb": float64(10)})
	ty := widgetType()
	ty.Await = createAwait
	if deleteAwait != createAwait {
		ty.DeleteAwait = &deleteAwait
	}
	ty.OperationPollPath = "v1/{+name}"
	return s, testProviderWithCatalog(t, s, &catalog.Catalog{Types: []*catalog.Type{ty}})
}

// TestADeleteWaitsForItsOwnOperation. sqladmin's sslCerts insert answers with
// the resource and its delete with an operation. Reusing the create's "no
// wait", Delete reported success the moment Google accepted the request, so
// a delete that then FAILED was a success to infrena, which drops a replaced
// object's Deposed record on exactly that.
func TestADeleteWaitsForItsOwnOperation(t *testing.T) {
	gcptest.Isolate(t)
	s, p := deletingWidget(t, gcpfake.OpLongRunning, catalog.AwaitNone, catalog.AwaitLongRunning)
	defer s.Close()
	s.DeleteThenFailOperation(deletePath, "FAILED_PRECONDITION", "the certificate is in use")

	err := p.Delete(context.Background(), widgetState())
	if err == nil || !strings.Contains(err.Error(), "in use") {
		t.Fatalf("Delete = %v, want the operation's own failure: the delete was accepted and then failed", err)
	}
	if _, ok := s.Get(deletePath); !ok {
		t.Fatal("the fixture is wrong: the resource is gone, so this proves nothing about waiting")
	}
}

// TestADeleteThatAnswersNothingIsASuccess. Bigtable's and Spanner's creates
// are long-running and their deletes answer Empty. Waiting on the create's
// kind, Delete looked for an operation that was never there and reported a
// delete that had worked as a failure.
func TestADeleteThatAnswersNothingIsASuccess(t *testing.T) {
	gcptest.Isolate(t)
	s, p := deletingWidget(t, gcpfake.OpSync, catalog.AwaitLongRunning, catalog.AwaitNone)
	defer s.Close()

	if err := p.Delete(context.Background(), widgetState()); err != nil {
		t.Fatalf("Delete = %v, for a delete Google answered with nothing and carried out", err)
	}
	if _, ok := s.Get(deletePath); ok {
		t.Error("the resource is still there")
	}
}

// TestALostOperationIsNotADeletion. A 404 while following a delete's
// operation used to count as "already gone". A 404 on the POLL says nothing
// about the resource: here the operation cannot be found and the resource is
// still there, so the delete must be reported as a failure, and infrena
// keeps the handle it would otherwise drop.
func TestALostOperationIsNotADeletion(t *testing.T) {
	gcptest.Isolate(t)
	s, p := deletingWidget(t, gcpfake.OpLongRunning, catalog.AwaitLongRunning, catalog.AwaitLongRunning)
	defer s.Close()
	s.DeleteThenFailOperation(deletePath, "FAILED_PRECONDITION", "never reached")
	ty, _ := p.catalog.Type("gcp.widget")
	ty.OperationPollPath = "v1/{+name}/nowhere"

	err := p.Delete(context.Background(), widgetState())
	if err == nil {
		t.Fatal("Delete succeeded, but its operation could not be found and the resource is still there")
	}
}

// TestAnOperationThatFindsNothingBecauseItIsGoneIsASuccess is the other side
// of the same 404: the resource really has been deleted, so the lost
// operation does not matter. A fix that made every such 404 an error would
// fail here, and destroy would stop being idempotent.
func TestAnOperationThatFindsNothingBecauseItIsGoneIsASuccess(t *testing.T) {
	gcptest.Isolate(t)
	s, p := deletingWidget(t, gcpfake.OpLongRunning, catalog.AwaitLongRunning, catalog.AwaitLongRunning)
	defer s.Close()
	ty, _ := p.catalog.Type("gcp.widget")
	ty.OperationPollPath = "v1/{+name}/nowhere"

	if err := p.Delete(context.Background(), widgetState()); err != nil {
		t.Fatalf("Delete = %v, but the resource is gone", err)
	}
}

// networkedWidget is a widget whose API refuses deletion while networks are
// attached, the way a Cloud DNS policy does.
func networkedWidget(t *testing.T, networks []any) (*gcpfake.Server, *Provider) {
	t.Helper()
	s := gcpfake.New(t)
	s.Seed(deletePath, map[string]any{"name": "one", "networks": networks})
	ty := widgetType()
	ty.UpdateMask = false
	ty.Attributes["networks"] = &catalog.Attr{Canonical: "networks", Kind: value.KindList,
		Elem: &catalog.Attr{Canonical: "networks", Kind: value.KindString}}
	ty.ClearBeforeDelete = []string{"networks"}
	return s, testProviderWithCatalog(t, s, &catalog.Catalog{Types: []*catalog.Type{ty}})
}

func networkedState(networks ...string) *resource.ResourceState {
	st := widgetState()
	var items []any
	for _, n := range networks {
		items = append(items, n)
	}
	st.Attributes["networks"] = attrsMixed(map[string]any{"networks": items})["networks"]
	return st
}

// TestAFieldTheAPIWillNotDeleteAroundIsClearedFirst. Google refuses to delete
// a Cloud DNS policy with networks attached, and configuration cannot detach
// them (a field removed from configuration keeps its value here), so the
// policy could never be destroyed. Delete clears them with a patch first.
func TestAFieldTheAPIWillNotDeleteAroundIsClearedFirst(t *testing.T) {
	gcptest.Isolate(t)
	s, p := networkedWidget(t, []any{"projects/p/global/networks/n"})
	defer s.Close()

	if err := p.Delete(context.Background(), networkedState("projects/p/global/networks/n")); err != nil {
		t.Fatal(err)
	}
	var order []string
	for _, r := range s.Requests() {
		if r.Method == "PATCH" || r.Method == "DELETE" {
			order = append(order, r.Method)
		}
		if r.Method == "PATCH" && !strings.Contains(string(r.Body), `"networks":null`) {
			t.Errorf("the clearing patch does not empty networks: %s", r.Body)
		}
	}
	if strings.Join(order, ",") != "PATCH,DELETE" {
		t.Errorf("requests were %v, want the clearing PATCH and then the DELETE", order)
	}
}

// TestNothingToClearSendsNoPatch. A policy with no networks attached is
// deleted directly: a patch that changes nothing is a request for nothing.
func TestNothingToClearSendsNoPatch(t *testing.T) {
	gcptest.Isolate(t)
	s, p := networkedWidget(t, nil)
	defer s.Close()

	if err := p.Delete(context.Background(), networkedState()); err != nil {
		t.Fatal(err)
	}
	for _, r := range s.Requests() {
		if r.Method == "PATCH" {
			t.Errorf("sent a clearing patch with nothing to clear: %s", r.Body)
		}
	}
}

// TestAnUpdateWaitsOnlyForItsOwnAnswer. Artifact Registry's create answers
// with an operation and its patch with the repository itself. Awaited as an
// operation, an update polled the repository's own name for a `done` it
// never has, until the type's twenty-minute timeout (live, 2026-09-25).
func TestAnUpdateWaitsOnlyForItsOwnAnswer(t *testing.T) {
	gcptest.Isolate(t)
	s := gcpfake.New(t)
	defer s.Close()
	s.SetOperationStyle(gcpfake.OpSync)
	s.Seed(deletePath, map[string]any{"name": "one", "sizeGb": float64(10)})
	ty := widgetType()
	ty.Await = catalog.AwaitLongRunning
	none := catalog.AwaitNone
	ty.UpdateAwait = &none
	ty.OperationPollPath = "v1/{+name}"
	ty.TimeoutSeconds = 3
	p := testProviderWithCatalog(t, s, &catalog.Catalog{Types: []*catalog.Type{ty}})

	st, err := p.Update(context.Background(), widgetState(), widgetDesired(map[string]any{
		"project": "p", "region": "r", "name": "one", "sizeGb": int64(20),
	}))
	if err != nil {
		t.Fatalf("Update = %v, want the patch's own answer taken as the result", err)
	}
	if got, _ := st.Attributes["sizeGb"].AsInt(); got != 20 {
		t.Errorf("state after the update has sizeGb %v, want 20", st.Attributes["sizeGb"])
	}
	for _, r := range s.Requests() {
		if r.Method == "GET" && !strings.HasSuffix(r.Path, "/widgets/one") {
			t.Errorf("the update polled %s, which is not an operation", r.Path)
		}
	}
}
