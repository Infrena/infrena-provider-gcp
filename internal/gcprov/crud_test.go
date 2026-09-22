package gcprov

import (
	"context"
	"strings"
	"testing"
	"time"

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

// TestCreateStillReturnsStateWhenTheCallerCancelsDuringReadback. Once the
// POST has succeeded, a resource is real and tracked nowhere if Create
// errors out -- the same rule await.go already enforces for the operation
// wait must also hold for readAfterCreate's own GET. OnRequest fires while
// that GET is in flight (the fake has received it but not yet answered) and
// cancels the CALLER's context there; a short sleep afterward gives the
// client's transport a moment to notice and abort a still-cancelable ctx
// before the fake's response is written, so a pre-fix regression (the
// readback using the caller's own cancelable ctx) reproduces reliably
// instead of racing the response across the loopback connection.
func TestCreateStillReturnsStateWhenTheCallerCancelsDuringReadback(t *testing.T) {
	gcptest.Isolate(t)
	s := gcpfake.New(t)
	defer s.Close()
	p := testProviderWithCatalog(t, s, widgetCatalog())

	ctx, cancel := context.WithCancel(context.Background())
	s.OnRequest("/v1/projects/p/locations/r/widgets/one", func() {
		cancel()
		time.Sleep(20 * time.Millisecond)
	})

	st, err := p.Create(ctx, &resource.DesiredResource{
		Type:  "gcp.widget",
		Attrs: attrsMixed(map[string]any{"project": "p", "region": "r", "name": "one"}),
	})
	if err != nil {
		t.Fatalf("Create errored because the CALLER cancelled after the create already "+
			"succeeded, which orphans a resource that exists: %v", err)
	}
	if st == nil {
		t.Fatal("Create returned no state for a resource that was created")
	}
}

// TestCreateTrustsTheAwaitWhenTheReadbackNeverSeesIt. GCP confirmed the
// create (the POST succeeded and the await, trivially for AwaitNone,
// reports the mutation's own response), but the readback GET keeps 404ing
// -- eventual consistency, not proof the create failed. Read gives up after
// notFoundPatience and reports "believed absent" as (nil, nil); Create must
// not turn that into an error, and must not produce the literal "%!w(<nil>)"
// artifact fmt.Errorf's "%w" leaves behind when wrapping a nil error.
//
// Zero jitter (withJitterForTest) removes backoff between retries so this
// test's own overhead stays minimal; notFoundPatience itself is a package
// constant this test cannot shorten, so its real wall-clock cost is close to
// notFoundPatience's own ~4s.
func TestCreateTrustsTheAwaitWhenTheReadbackNeverSeesIt(t *testing.T) {
	gcptest.Isolate(t)
	withJitterForTest(t, func(int64) int64 { return 0 })
	s := gcpfake.New(t)
	defer s.Close()
	// Large enough that the count never runs out before notFoundPatience's
	// deadline does, however many zero-backoff retries fit in that window.
	s.NotFoundTimes("/v1/projects/p/locations/r/widgets/one", 1<<30)
	p := testProviderWithCatalog(t, s, widgetCatalog())

	st, err := p.Create(context.Background(), &resource.DesiredResource{
		Type:  "gcp.widget",
		Attrs: attrsMixed(map[string]any{"project": "p", "region": "r", "name": "one"}),
	})
	if err != nil {
		if contains(err.Error(), "%!w") {
			t.Fatalf("error carries the nil-%%w artifact from wrapping a nil readErr: %v", err)
		}
		t.Fatalf("Create errored for a resource GCP confirmed creating: %v", err)
	}
	if st == nil {
		t.Fatal("Create returned no state for a resource that exists")
	}
	if st.ProviderID != "projects/p/locations/r/widgets/one" {
		t.Errorf("provider id = %q, want the one the awaited body implies", st.ProviderID)
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

// TestAProviderIDForATypeWhoseSelfLinkStartsWithAPlaceholder. 16 types have a
// self_link beginning with {{parent}}, leaving reduceSelfLink no literal text
// to search for. Two of them also return a selfLink in their bodies, so before
// PathPrefix existed ProviderID failed on every create -- and per the orphan
// rule, an error after a successful create orphans the resource.
//
// Against the REAL catalog entry, not a hand-built type, so this fails if the
// generator ever stops emitting the prefix.
func TestAProviderIDForATypeWhoseSelfLinkStartsWithAPlaceholder(t *testing.T) {
	ty, ok := mustCatalog(t).Type("gcp.networksecurity.addressgroup")
	if !ok {
		t.Fatal("the catalog no longer ships gcp.networksecurity.addressgroup")
	}
	if !strings.HasPrefix(ty.SelfLink, "{{") {
		t.Fatalf("this test exists for a self_link that starts with a placeholder; got %q", ty.SelfLink)
	}
	if ty.PathPrefix == "" {
		t.Fatalf("%s has no path_prefix, so its urls carry no api version", ty.Name)
	}
	got, err := ProviderID(ty, map[string]any{
		"selfLink": "https://networksecurity.googleapis.com/" + ty.PathPrefix +
			"projects/p/locations/us-central1/addressGroups/ag",
	}, nil)
	if err != nil {
		t.Fatalf("ProviderID on a body selfLink: %v", err)
	}
	if want := "projects/p/locations/us-central1/addressGroups/ag"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	if strings.Contains(got, "googleapis.com") || strings.Contains(got, ty.PathPrefix) {
		t.Errorf("the provider id %q still carries the host or the api version", got)
	}
}

// widgetCatalogAwaitingComputeOperation is the compute-style counterpart of
// widgetCatalogAwaitingLongRunning: the mutation answers with a compute
// operation ({"name","status","selfLink","targetLink"}) that must be waited
// on, and only then does the created resource have an identity. widgetType's
// own self_link ends in "{{name}}", the shape 12 of the 65 real
// compute-style types have, so this fixture exercises both ways the old
// return was wrong at once.
func widgetCatalogAwaitingComputeOperation() *catalog.Catalog {
	ty := widgetType()
	ty.Await = catalog.AwaitComputeOperation
	ty.OperationWaitPath = "projects/{project}/regions/{region}/operations/{operation}/wait"
	return &catalog.Catalog{Types: []*catalog.Type{ty}}
}

// TestCreatingThroughAComputeOperationIdentifiesTheResourceNotTheOperation.
// Create derives the provider id from what the await handed back, twice
// (readAfterCreate and bestEffortState). When that was the operation itself,
// the id named ".../operations/op-1" and every later Read, Delete and Import
// addressed the operation instead of the resource -- and because the fake
// answers a GET of an operation it knows, the readback even succeeded,
// storing a state that points at the wrong thing forever.
func TestCreatingThroughAComputeOperationIdentifiesTheResourceNotTheOperation(t *testing.T) {
	gcptest.Isolate(t)
	s := gcpfake.New(t)
	defer s.Close()
	s.SetOperationStyle(gcpfake.OpCompute)
	p := testProviderWithCatalog(t, s, widgetCatalogAwaitingComputeOperation())

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
		t.Errorf("provider id = %q, want the widget's own name", st.ProviderID)
	}
	if strings.Contains(st.ProviderID, "operations") {
		t.Errorf("the provider id names the operation rather than what it created: %q", st.ProviderID)
	}
	// The id is only useful if it addresses something: a Read of it must find
	// the widget, not the operation the fake would also happily answer for.
	back, err := p.Read(context.Background(), st)
	if err != nil {
		t.Fatal(err)
	}
	if back == nil {
		t.Fatal("the created resource cannot be read back by the id Create reported")
	}
	if got := back.Attributes["sizeGb"]; got.Raw != int64(10) {
		t.Errorf("the id addressed something other than the widget: read back %v", back.Attributes)
	}
}

// TestCreateSurvivesAnOperationTargetItCannotReduce. An operation's
// targetLink is whatever url its own API publishes, and nothing makes that
// agree with the version the catalog pinned for the type: a "v1beta1/..."
// target against a type whose path_prefix is "v1/" leaves reduceSelfLink
// nothing to reduce by, and ProviderID fails. Failing there would make
// Create return an error for a resource GCP has already made, and the host
// drops a failed create's result, so the resource would exist and be tracked
// nowhere. The caller's own attributes describe the same resource, so they
// are used instead -- the same id Create would have used had the operation
// named no target at all.
func TestCreateSurvivesAnOperationTargetItCannotReduce(t *testing.T) {
	gcptest.Isolate(t)
	s := gcpfake.New(t)
	defer s.Close()
	s.SetOperationStyle(gcpfake.OpCompute)
	s.AnswerComputeOperationLinksUnder("v1beta1")
	p := testProviderWithCatalog(t, s, widgetCatalogAwaitingComputeOperation())

	st, err := p.Create(context.Background(), &resource.DesiredResource{
		Type:  "gcp.widget",
		Attrs: attrsMixed(map[string]any{"project": "p", "region": "r", "name": "one", "sizeGb": int64(10)}),
	})
	if err != nil {
		t.Fatalf("Create errored for a resource GCP already made, which orphans it: %v", err)
	}
	if st == nil {
		t.Fatal("Create returned no state for a resource that exists")
	}
	if st.ProviderID != "projects/p/locations/r/widgets/one" {
		t.Errorf("provider id = %q, want the one the caller's attributes imply", st.ProviderID)
	}

	// Non-vacuity: if the fake ever stops answering with a target under
	// another version, nothing above is being tested at all.
	op := getJSON(t, s.URL()+"/v1/projects/p/locations/r/operations/op-1")
	if target, _ := op["targetLink"].(string); !strings.Contains(target, "/v1beta1/") {
		t.Fatalf("the operation's target is reducible after all, so this test proves nothing: %q", target)
	}
	if widgetType().PathPrefix != "v1/" {
		t.Fatalf("this test needs a type whose path_prefix cannot reduce a v1beta1 url; got %q", widgetType().PathPrefix)
	}
}

// TestCreateSurvivesAnUnreducibleTargetWhenTheReadbackNeverSeesIt covers the
// same orphan path one step over: bestEffortState, which Create falls to
// when the readback GET keeps 404ing inside GCP's eventual-consistency
// window, computes the id from the awaited body a second time and used to
// wrap a failure there into an error. Both of Create's consumers of the
// awaited body have to survive an unreducible target, not just the first.
//
// Zero jitter for the same reason TestCreateTrustsTheAwaitWhenTheReadbackNeverSeesIt
// uses it: the wall-clock cost is notFoundPatience, which no test can shorten.
func TestCreateSurvivesAnUnreducibleTargetWhenTheReadbackNeverSeesIt(t *testing.T) {
	gcptest.Isolate(t)
	withJitterForTest(t, func(int64) int64 { return 0 })
	s := gcpfake.New(t)
	defer s.Close()
	s.SetOperationStyle(gcpfake.OpCompute)
	s.AnswerComputeOperationLinksUnder("v1beta1")
	s.NotFoundTimes("/v1/projects/p/locations/r/widgets/one", 1<<30)
	p := testProviderWithCatalog(t, s, widgetCatalogAwaitingComputeOperation())

	st, err := p.Create(context.Background(), &resource.DesiredResource{
		Type:  "gcp.widget",
		Attrs: attrsMixed(map[string]any{"project": "p", "region": "r", "name": "one"}),
	})
	if err != nil {
		t.Fatalf("Create errored for a resource GCP already made, which orphans it: %v", err)
	}
	if st == nil {
		t.Fatal("Create returned no state for a resource that exists")
	}
	if st.ProviderID != "projects/p/locations/r/widgets/one" {
		t.Errorf("provider id = %q, want the one the caller's attributes imply", st.ProviderID)
	}
}
