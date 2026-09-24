package gcprov

import (
	"context"
	"strings"
	"testing"

	"github.com/infrena/infrena-provider-gcp/internal/catalog"
	"github.com/infrena/infrena-provider-gcp/internal/gcpfake"
	"github.com/infrena/infrena-provider-gcp/internal/gcptest"
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
