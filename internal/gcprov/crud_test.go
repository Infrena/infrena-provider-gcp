package gcprov

import (
	"context"
	"testing"

	"github.com/infrena/infrena-provider-gcp/internal/catalog"
	"github.com/infrena/infrena-provider-gcp/internal/gcpfake"
	"github.com/infrena/infrena-provider-gcp/internal/gcptest"
	"github.com/infrena/infrena/pkg/resource"
)

// widgetCatalogAwaitingLongRunning is
// TestCreateReportsStateWhenTheResourceExistsDespiteAFailure's own catalog.
// widgetType() itself is AwaitNone, which every OTHER CRUD test in this file
// relies on -- it matches the fake's default OpSync response shape (the
// mutation's response IS the resource, no operation envelope). This one test
// sets OpLongRunning, so it needs a type that actually polls the operation it
// gets back instead of mistaking the operation envelope ({"name","done"}) for
// the created resource's own body -- which is what AwaitNone would do,
// producing a provider id built from the OPERATION's name rather than the
// widget's.
func widgetCatalogAwaitingLongRunning() *catalog.Catalog {
	ty := widgetType()
	ty.Await = catalog.AwaitLongRunning
	ty.OperationPollPath = "{+name}"
	return &catalog.Catalog{Types: []*catalog.Type{ty}}
}

func TestCreateStoresAndReportsWhatExists(t *testing.T) {
	gcptest.Isolate(t)
	s := gcpfake.New(t)
	defer s.Close()
	p := testProviderWithCatalog(t, s, widgetCatalog())

	st, err := p.Create(context.Background(), &resource.DesiredResource{
		Type:  "gcp.widget",
		Attrs: attrsMixed(map[string]any{"project": "p", "region": "r", "name": "one", "sizeGb": int64(10)}),
	})
	if err != nil {
		t.Fatal(err)
	}
	if st == nil {
		t.Fatal("Create returned (nil, nil), which orphans the resource it just made")
	}
	if st.ProviderID != "projects/p/locations/r/widgets/one" {
		t.Errorf("provider id = %q", st.ProviderID)
	}
	if got, ok := s.Get("/v1/projects/p/locations/r/widgets/one"); !ok {
		t.Errorf("nothing was actually created: %v", got)
	}
}

// TestCreateReportsStateWhenTheResourceExistsDespiteAFailure. The host drops
// a failed create's result, so returning an error after GCP made something
// leaves a resource that is real, tracked nowhere, and unfindable by a later
// plan.
func TestCreateReportsStateWhenTheResourceExistsDespiteAFailure(t *testing.T) {
	gcptest.Isolate(t)
	s := gcpfake.New(t)
	defer s.Close()
	s.SetOperationStyle(gcpfake.OpLongRunning)
	// The resource is created, then the operation reports failure -- the real
	// and nasty case, e.g. a post-create configuration step failing. The path
	// is the exact request path a create at these attributes lands on
	// (gcpfake keys createThenFail by the literal request path, server.go's
	// handleCreate) -- "/v1/..." because widgetType's own base_url carries
	// that version segment.
	s.CreateThenFailOperation("/v1/projects/p/locations/r/widgets/one", "INTERNAL", "post-create step failed")
	p := testProviderWithCatalog(t, s, widgetCatalogAwaitingLongRunning())

	st, err := p.Create(context.Background(), &resource.DesiredResource{
		Type:  "gcp.widget",
		Attrs: attrsMixed(map[string]any{"project": "p", "region": "r", "name": "one"}),
	})
	if err != nil {
		t.Fatalf("Create errored after GCP created something, which orphans it: %v", err)
	}
	if st == nil || st.ProviderID == "" {
		t.Fatal("Create returned no state for a resource that exists")
	}
}

func TestReadReportsAbsenceAsNilNil(t *testing.T) {
	gcptest.Isolate(t)
	s := gcpfake.New(t)
	defer s.Close()
	p := testProviderWithCatalog(t, s, widgetCatalog())

	st, err := p.Read(context.Background(), &resource.ResourceState{
		Type: "gcp.widget", ProviderID: "projects/p/locations/r/widgets/missing",
	})
	if err != nil {
		t.Fatal(err)
	}
	if st != nil {
		t.Errorf("a missing resource read back as %v", st)
	}
}

// TestReadRetriesBeforeReportingAbsence. GCP is eventually consistent: a
// resource created seconds ago can 404. Reporting that as absence makes the
// next plan propose creating it again.
func TestReadRetriesBeforeReportingAbsence(t *testing.T) {
	gcptest.Isolate(t)
	s := gcpfake.New(t)
	defer s.Close()
	s.NotFoundTimes("/v1/projects/p/locations/r/widgets/one", 2)
	s.Seed("/v1/projects/p/locations/r/widgets/one", map[string]any{"name": "one", "sizeGb": float64(10)})
	p := testProviderWithCatalog(t, s, widgetCatalog())

	st, err := p.Read(context.Background(), &resource.ResourceState{
		Type: "gcp.widget", ProviderID: "projects/p/locations/r/widgets/one",
	})
	if err != nil {
		t.Fatal(err)
	}
	if st == nil {
		t.Fatal("a resource that exists was reported absent after two transient 404s")
	}
}

func TestDeleteRemovesTheResource(t *testing.T) {
	gcptest.Isolate(t)
	s := gcpfake.New(t)
	defer s.Close()
	s.Seed("/v1/projects/p/locations/r/widgets/one", map[string]any{"name": "one"})
	p := testProviderWithCatalog(t, s, widgetCatalog())

	if err := p.Delete(context.Background(), &resource.ResourceState{
		Type: "gcp.widget", ProviderID: "projects/p/locations/r/widgets/one",
	}); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.Get("/v1/projects/p/locations/r/widgets/one"); ok {
		t.Error("the resource is still there")
	}
}

// TestDeletingSomethingAlreadyGoneIsNotAnError. Destroy must converge; a 404
// on delete means the goal is met.
func TestDeletingSomethingAlreadyGoneIsNotAnError(t *testing.T) {
	gcptest.Isolate(t)
	s := gcpfake.New(t)
	defer s.Close()
	p := testProviderWithCatalog(t, s, widgetCatalog())
	if err := p.Delete(context.Background(), &resource.ResourceState{
		Type: "gcp.widget", ProviderID: "projects/p/locations/r/widgets/gone",
	}); err != nil {
		t.Errorf("deleting an absent resource errored: %v", err)
	}
}

func TestImportReadsAnExistingResource(t *testing.T) {
	gcptest.Isolate(t)
	s := gcpfake.New(t)
	defer s.Close()
	s.Seed("/v1/projects/p/locations/r/widgets/one", map[string]any{"name": "one", "sizeGb": float64(10)})
	p := testProviderWithCatalog(t, s, widgetCatalog())

	st, err := p.Import(context.Background(), "gcp.widget", "projects/p/locations/r/widgets/one")
	if err != nil {
		t.Fatal(err)
	}
	// project and region must be recovered FROM THE ID, or the imported
	// resource plans a change on the very next run for attributes nobody set.
	if st.Attributes["project"].Raw != "p" || st.Attributes["region"].Raw != "r" {
		t.Errorf("project/region not recovered from the id: %v", st.Attributes)
	}
}
