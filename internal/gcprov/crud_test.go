package gcprov

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/infrena/infrena-provider-gcp/internal/catalog"
	"github.com/infrena/infrena-provider-gcp/internal/gcpfake"
	"github.com/infrena/infrena-provider-gcp/internal/gcptest"
	"github.com/infrena/infrena/pkg/resource"
	"github.com/infrena/infrena/pkg/value"
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

// TestReadSurvivesAResponseItCannotReduce. stateFrom recomputes the provider
// id from a get's own response body so a rename GCP made underneath this
// resource is picked up -- but we asked GCP for THIS resource BY ID and it
// answered 200, so the resource exists at current's id whether or not the
// body's own selfLink happens to reduce against this type's api prefix. A
// selfLink under a version segment the prefix cannot reduce (the realistic
// case: "v1beta1/..." against a type whose path_prefix is "v1/", the same
// shape TestCreateSurvivesAnOperationTargetItCannotReduce covers for an
// operation's targetLink) must not turn a successful get into an error --
// and this must hold for a plain Read, not only one reached through Create,
// since the recompute is unconditional in stateFrom.
func TestReadSurvivesAResponseItCannotReduce(t *testing.T) {
	gcptest.Isolate(t)
	s := gcpfake.New(t)
	defer s.Close()
	const unreducible = "https://widgets.googleapis.com/v1beta1/projects/p/locations/r/widgets/one"
	s.Seed("/v1/projects/p/locations/r/widgets/one", map[string]any{
		"name":     "one",
		"selfLink": unreducible,
	})
	p := testProviderWithCatalog(t, s, widgetCatalog())

	st, err := p.Read(context.Background(), &resource.ResourceState{
		Type: "gcp.widget", ProviderID: "projects/p/locations/r/widgets/one",
	})
	if err != nil {
		t.Fatalf("Read errored for a resource that answered 200: %v", err)
	}
	if st == nil {
		t.Fatal("Read reported absence for a resource that answered 200")
	}
	if st.ProviderID != "projects/p/locations/r/widgets/one" {
		t.Errorf("provider id = %q, want the one this resource was read at", st.ProviderID)
	}

	// Non-vacuity: if the body's own selfLink ever reduces after all,
	// nothing above is being tested.
	if _, err := ProviderID(widgetType(), map[string]any{"selfLink": unreducible}, nil); err == nil {
		t.Fatal("the selfLink is reducible after all, so this test proves nothing")
	}
}

// TestCreateSurvivesAReadbackItCannotReduce is the orphan path stateFrom's
// fix closes: unlike TestCreateSurvivesAnOperationTargetItCannotReduce (an
// unreducible target inside the AWAITED operation, before any GET happens),
// here createdID succeeds cleanly and readAfterCreate's own GET -- the
// ordinary readback every Create does -- comes back with a selfLink this
// type's api prefix cannot reduce. That GET already answered 200 for a
// resource GCP just created; erroring out of stateFrom at that point is
// exactly the orphan the fix rules out.
func TestCreateSurvivesAReadbackItCannotReduce(t *testing.T) {
	gcptest.Isolate(t)
	s := gcpfake.New(t)
	defer s.Close()
	if widgetType().PathPrefix != "v1/" {
		t.Fatalf("this test needs a type whose path_prefix cannot reduce a v1beta1 url; got %q", widgetType().PathPrefix)
	}
	p := testProviderWithCatalog(t, s, widgetCatalog())

	// The create's own POST lands on the collection path; only the readback
	// GET that follows hits the item path, so seeding an unreducible
	// selfLink here the moment that GET arrives -- overwriting what the
	// create itself just stored -- puts it on the readback alone.
	itemPath := "/v1/projects/p/locations/r/widgets/one"
	const unreducible = "https://widgets.googleapis.com/v1beta1/projects/p/locations/r/widgets/one"
	s.OnRequest(itemPath, func() {
		s.Seed(itemPath, map[string]any{"name": "one", "selfLink": unreducible})
	})

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
		t.Errorf("provider id = %q, want the one this create was given", st.ProviderID)
	}

	// Non-vacuity: confirm the readback the provider actually saw is the
	// unreducible one seeded above, not whatever the create itself stored.
	got, ok := s.Get(itemPath)
	if !ok || got["selfLink"] != unreducible {
		t.Fatalf("the readback never saw the unreducible selfLink, so this test proves nothing: %v", got)
	}
}

// widgetCatalogWithABareCaptureID is the shape 83 of the 233 shipped types
// have: a self_link that is nothing but a whole-path capture. ParseProviderID
// accepts ANY string against one of those, so a wrong id is not caught
// anywhere downstream -- it is simply stored, and the next Delete addresses
// whatever it named. Its "name" attribute is the full relative resource name,
// which is what such a type's id template means by name (gcp.container.cluster
// works exactly this way).
func widgetCatalogWithABareCaptureID() *catalog.Catalog {
	ty := widgetType()
	ty.Await = catalog.AwaitComputeOperation
	ty.OperationWaitPath = "projects/{project}/regions/{region}/operations/{operation}/wait"
	ty.SelfLink = "{+name}"
	ty.ImportFormat = "{+name}"
	return &catalog.Catalog{Types: []*catalog.Type{ty}}
}

// TestACreateRefusesAnIDNamingAResourceItDidNotCreate. An operation's
// targetLink is whatever url its own API published, and reducing it cleanly
// says only that it is well-formed, not that it names the thing that was just
// created. This is the literal-segment half: the id parses out of the
// response fine, and the failure lands later, in ParseProviderID, as
// "created, but the resource cannot be read back" -- an error after a
// successful POST, which is exactly what the orphan rule forbids.
func TestACreateRefusesAnIDNamingAResourceItDidNotCreate(t *testing.T) {
	gcptest.Isolate(t)
	s := gcpfake.New(t)
	defer s.Close()
	s.SetOperationStyle(gcpfake.OpCompute)
	s.AnswerComputeOperationTargetsAt("/v1/projects/p/locations/r/gadgets/one")
	p := testProviderWithCatalog(t, s, widgetCatalogAwaitingComputeOperation())

	var st *resource.ResourceState
	var err error
	stderr := captureStderr(t, func() {
		st, err = p.Create(context.Background(), &resource.DesiredResource{
			Type:  "gcp.widget",
			Attrs: attrsMixed(map[string]any{"project": "p", "region": "r", "name": "one", "sizeGb": int64(10)}),
		})
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
	if !strings.Contains(stderr, "gadgets") {
		t.Errorf("nothing was said about the target that named another resource; stderr was %q", stderr)
	}
}

// TestACreateRefusesABareCaptureIDNamingAnotherCollection is the same defect
// where nothing downstream can catch it. With a self_link of "{+name}",
// ParseProviderID accepts the wrong id, itemURL expands it straight into a
// url, and the create succeeds reporting an id that names someone else's
// resource -- silently, with no stderr line and no failed read to give it
// away. The id is checked against the collection the POST went to, which is
// the only thing here that knows the difference.
func TestACreateRefusesABareCaptureIDNamingAnotherCollection(t *testing.T) {
	gcptest.Isolate(t)
	s := gcpfake.New(t)
	defer s.Close()
	s.SetOperationStyle(gcpfake.OpCompute)
	s.AnswerComputeOperationTargetsAt("/v1/projects/p/locations/r/gadgets/one")
	c := widgetCatalogWithABareCaptureID()
	p := testProviderWithCatalog(t, s, c)

	// Non-vacuity: if this type's own id template rejected the wrong id, the
	// collection check would not be what saves it and this test would prove
	// nothing.
	if _, err := ParseProviderID(c.Types[0], "projects/p/locations/r/gadgets/one"); err != nil {
		t.Fatalf("this type's id template refuses the wrong id by itself: %v", err)
	}

	var st *resource.ResourceState
	var err error
	stderr := captureStderr(t, func() {
		st, err = p.Create(context.Background(), &resource.DesiredResource{
			Type: "gcp.widget",
			Attrs: attrsMixed(map[string]any{
				"project": "p", "region": "r",
				"name": "projects/p/locations/r/widgets/one", "sizeGb": int64(10),
			}),
		})
	})
	if err != nil {
		t.Fatalf("Create errored for a resource GCP already made, which orphans it: %v", err)
	}
	if st == nil {
		t.Fatal("Create returned no state for a resource that exists")
	}
	if strings.Contains(st.ProviderID, "gadgets") {
		t.Errorf("the create adopted an id naming a resource it did not make: %q", st.ProviderID)
	}
	if st.ProviderID != "projects/p/locations/r/widgets/one" {
		t.Errorf("provider id = %q, want the one the caller's attributes imply", st.ProviderID)
	}
	if !strings.Contains(stderr, "gadgets") {
		t.Errorf("a wrong id was replaced without a word about it; stderr was %q", stderr)
	}
}

// TestAnIDTheCreateReallyMadeIsKeptAsIs is the other side of the two tests
// above: the check must not fire on the ordinary case, where the operation's
// target names exactly what was created. If it did, every create would quietly
// fall back to the attributes and the stderr line would mean nothing.
func TestAnIDTheCreateReallyMadeIsKeptAsIs(t *testing.T) {
	gcptest.Isolate(t)
	s := gcpfake.New(t)
	defer s.Close()
	s.SetOperationStyle(gcpfake.OpCompute)
	p := testProviderWithCatalog(t, s, widgetCatalogAwaitingComputeOperation())

	var st *resource.ResourceState
	var err error
	stderr := captureStderr(t, func() {
		st, err = p.Create(context.Background(), &resource.DesiredResource{
			Type:  "gcp.widget",
			Attrs: attrsMixed(map[string]any{"project": "p", "region": "r", "name": "one"}),
		})
	})
	if err != nil {
		t.Fatal(err)
	}
	if st.ProviderID != "projects/p/locations/r/widgets/one" {
		t.Errorf("provider id = %q", st.ProviderID)
	}
	if stderr != "" {
		t.Errorf("a create whose target names exactly what it made complained anyway: %q", stderr)
	}
}

// captureStderr runs f with os.Stderr redirected, and returns what it wrote.
// The fallback paths in Create report on stderr rather than returning an
// error (an error after a successful POST orphans the resource), so the
// stderr line IS the observable behaviour and a test that cannot read it
// cannot tell a silent wrong answer from a reported one.
func captureStderr(t *testing.T, f func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	orig := os.Stderr
	os.Stderr = w
	f()
	os.Stderr = orig
	w.Close()
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}

// syncCatalogFor returns the REAL catalog's entry for name, plus a one-type
// catalog holding it with Await forced to AwaitNone.
//
// The real entry, because the whole subject of Task 14a is an attribute the
// GENERATOR renamed, and widgetType() has none -- inventing a fixture with a
// renamed attribute would test the fixture, not the corpus. Await forced,
// because gcpfake answers a mutation with the resource itself by default
// (OpSync) and these tests are about what is IN the body, not about how the
// operation completes; the same trick widgetCatalogAwaitingLongRunning plays
// in the other direction.
func syncCatalogFor(t *testing.T, name string) (*catalog.Type, *catalog.Catalog) {
	t.Helper()
	real, ok := mustCatalog(t).Type(name)
	if !ok {
		t.Fatalf("the catalog no longer ships %s", name)
	}
	cp := *real
	cp.Await = catalog.AwaitNone
	return &cp, &catalog.Catalog{Types: []*catalog.Type{&cp}}
}

// requireRenamedAt fails unless the type still declares schemaKey as a
// rename of wireName, so a regeneration that stops renaming turns these
// tests into a loud failure rather than a quiet pass over nothing.
func requireRenamedAt(t *testing.T, ty *catalog.Type, schemaKey, wireName string) {
	t.Helper()
	a, ok := ty.Attributes[schemaKey]
	if !ok {
		t.Fatalf("%s no longer declares %q, so this test is not exercising a rename", ty.Name, schemaKey)
	}
	if a.Canonical != wireName {
		t.Fatalf("%s.%s is now canonical %q, not %q; this test is not exercising a rename",
			ty.Name, schemaKey, a.Canonical, wireName)
	}
}

// TestACreateSendsTheWireNameNotTheSchemaName. requestBody used to do
// body[name] = toRaw(v) with name the SCHEMA key, so a create sent
// {"type_value": "SIDECAR_PROXY"} where GCP asks for {"type":
// "SIDECAR_PROXY"}. gcp.endpointpolicy is the sharp end of that: its
// type_value is REQUIRED, one of the 7 required renames in the corpus, so
// the API was being asked to create a resource without a field it demands.
func TestACreateSendsTheWireNameNotTheSchemaName(t *testing.T) {
	gcptest.Isolate(t)
	s := gcpfake.New(t)
	defer s.Close()
	ty, c := syncCatalogFor(t, "gcp.endpointpolicy")
	requireRenamedAt(t, ty, "type_value", "type")
	if !ty.Attributes["type_value"].Required {
		t.Fatalf("%s.type_value is no longer Required; this test exists for a REQUIRED rename", ty.Name)
	}
	p := testProviderWithCatalog(t, s, c)

	st, err := p.Create(context.Background(), &resource.DesiredResource{
		Type: ty.Name,
		Attrs: attrsMixed(map[string]any{
			"project":         "p",
			"name":            "ep1",
			"type_value":      "SIDECAR_PROXY",
			"endpointMatcher": map[string]any{"metadataLabelMatcher": map[string]any{}},
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	if st == nil {
		t.Fatal("Create returned (nil, nil), which orphans the resource it just made")
	}

	var posts int
	for _, r := range s.Requests() {
		if r.Method != http.MethodPost {
			continue
		}
		posts++
		var body map[string]any
		if err := json.Unmarshal(r.Body, &body); err != nil {
			t.Fatalf("the create body is not JSON: %v", err)
		}
		if _, leaked := body["type_value"]; leaked {
			t.Errorf("the create sent the SCHEMA name; GCP has no such field: %v", body)
		}
		if body["type"] != "SIDECAR_PROXY" {
			t.Errorf("the create body = %v, want the wire name %q carrying the value", body, "type")
		}
	}
	if posts != 1 {
		t.Fatalf("expected exactly one POST, saw %d", posts)
	}
}

// TestAReadKeysStateBySchemaName. stateFrom used to do attrs[k] =
// fromRaw(raw) with k straight off the response body, so state held "type"
// while configuration held "type_value". The host diffs those two maps, so
// every plan reported a change to a field nobody had touched, forever.
func TestAReadKeysStateBySchemaName(t *testing.T) {
	gcptest.Isolate(t)
	s := gcpfake.New(t)
	defer s.Close()
	ty, c := syncCatalogFor(t, "gcp.endpointpolicy")
	requireRenamedAt(t, ty, "type_value", "type")
	// GCP's own spelling, which is the only spelling a response ever carries.
	s.Seed("/v1/projects/p/locations/global/endpointPolicies/ep1", map[string]any{
		"type":        "SIDECAR_PROXY",
		"description": "the one being read",
	})
	p := testProviderWithCatalog(t, s, c)

	st, err := p.Read(context.Background(), &resource.ResourceState{
		Type: ty.Name, ProviderID: "projects/p/locations/global/endpointPolicies/ep1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if st == nil {
		t.Fatal("Read reported absence for a resource the fake is holding")
	}
	if _, leaked := st.Attributes["type"]; leaked {
		t.Errorf("state is keyed by the WIRE name, which configuration can never match: %v", st.Attributes)
	}
	got, ok := st.Attributes["type_value"].AsString()
	if !ok || got != "SIDECAR_PROXY" {
		t.Errorf("state[type_value] = %#v, want the value the body carried under %q",
			st.Attributes["type_value"], "type")
	}
}

// TestAReadKeysAListElementBySchemaNameToo is the same claim one level in,
// on the shape two thirds of the corpus's renames actually have. gcp.router
// is one of the three updatable types (with gcp.grpcroute and
// gcp.responsepolicyrule) whose ONLY renamed attribute lives inside a list,
// so a translation that recursed into Fields but not Elem would leave this
// exactly as broken as it was while passing every top-level test above.
func TestAReadKeysAListElementBySchemaNameToo(t *testing.T) {
	gcptest.Isolate(t)
	s := gcpfake.New(t)
	defer s.Close()
	ty, c := syncCatalogFor(t, "gcp.router")
	nats, ok := ty.Attributes["nats"]
	if !ok || nats.Elem == nil {
		t.Fatalf("%s no longer declares nats as a list", ty.Name)
	}
	requireRenamedAt(t, &catalog.Type{Name: ty.Name + ".nats[]", Attributes: nats.Elem.Fields},
		"type_value", "type")

	// "router", not "name": gcp.router's self_link captures its own id under
	// the API's own placeholder name ("projects/{project}/regions/{region}/routers/{router}").
	id, err := ProviderID(ty, nil, attrs(map[string]string{"project": "p", "region": "r", "router": "rt1"}))
	if err != nil {
		t.Fatalf("building the id to seed at: %v", err)
	}
	s.Seed("/"+ty.PathPrefix+id, map[string]any{
		"name": "rt1",
		"nats": []any{map[string]any{"name": "nat-1", "type": "PUBLIC"}},
	})
	p := testProviderWithCatalog(t, s, c)

	st, err := p.Read(context.Background(), &resource.ResourceState{Type: ty.Name, ProviderID: id})
	if err != nil {
		t.Fatal(err)
	}
	if st == nil {
		t.Fatal("Read reported absence for a resource the fake is holding")
	}
	list, ok := st.Attributes["nats"].Raw.([]value.Value)
	if !ok || len(list) != 1 {
		t.Fatalf("state[nats] = %#v, want a one-element list", st.Attributes["nats"])
	}
	fields, ok := list[0].Raw.(map[string]value.Value)
	if !ok {
		t.Fatalf("state[nats][0] = %#v, want an object", list[0])
	}
	if _, leaked := fields["type"]; leaked {
		t.Errorf("a list element in state is keyed by the WIRE name: %#v", fields)
	}
	if got, ok := fields["type_value"].AsString(); !ok || got != "PUBLIC" {
		t.Errorf("state[nats][0] = %#v, want %q carrying the value", fields, "type_value")
	}
}

// TestACreateSendsWireNamesBelowTheTopLevelToo. requestBody translating only
// its own top-level keys passes every other create test in this file and
// still sends "type_value" inside every nested object and list element it
// writes -- which for 263 of the corpus's 306 renamed attributes is the only
// place they ever appear.
//
// gcp.regionsecuritypolicy carries both halves at once: a renamed attribute
// at the top level, and another two levels down inside a list
// (rules[].redirectOptions.type_value), so one create exercises the whole
// descent.
func TestACreateSendsWireNamesBelowTheTopLevelToo(t *testing.T) {
	gcptest.Isolate(t)
	s := gcpfake.New(t)
	defer s.Close()
	ty, c := syncCatalogFor(t, "gcp.regionsecuritypolicy")
	requireRenamedAt(t, ty, "type_value", "type")
	redirect := ty.Attributes["rules"].Elem.Fields["redirectOptions"]
	if a, ok := redirect.Fields["type_value"]; !ok || a.Canonical != "type" {
		t.Fatalf("%s no longer renames rules[].redirectOptions.type; this test is not "+
			"exercising a nested rename", ty.Name)
	}
	p := testProviderWithCatalog(t, s, c)

	st, err := p.Create(context.Background(), &resource.DesiredResource{
		Type: ty.Name,
		Attrs: attrsMixed(map[string]any{
			"project":    "p",
			"region":     "r",
			"name":       "sp1",
			"type_value": "CLOUD_ARMOR",
			"rules": []any{map[string]any{
				"priority":        int64(1000),
				"redirectOptions": map[string]any{"type_value": "GOOGLE_RECAPTCHA"},
			}},
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	if st == nil {
		t.Fatal("Create returned (nil, nil), which orphans the resource it just made")
	}

	var posted map[string]any
	for _, r := range s.Requests() {
		if r.Method != http.MethodPost {
			continue
		}
		if err := json.Unmarshal(r.Body, &posted); err != nil {
			t.Fatalf("the create body is not JSON: %v", err)
		}
	}
	if posted == nil {
		t.Fatal("no create was sent")
	}
	if _, leaked := posted["type_value"]; leaked {
		t.Errorf("the create sent the schema name at the top level: %v", posted)
	}
	rules, ok := posted["rules"].([]any)
	if !ok || len(rules) != 1 {
		t.Fatalf("the create body's rules = %#v, want a one-element list", posted["rules"])
	}
	rule, ok := rules[0].(map[string]any)
	if !ok {
		t.Fatalf("the create body's rules[0] = %#v, want an object", rules[0])
	}
	opts, ok := rule["redirectOptions"].(map[string]any)
	if !ok {
		t.Fatalf("the create body's rules[0].redirectOptions = %#v, want an object", rule["redirectOptions"])
	}
	if _, leaked := opts["type_value"]; leaked {
		t.Errorf("the create sent the schema name inside a list element; GCP has no such "+
			"field: %#v", opts)
	}
	if opts["type"] != "GOOGLE_RECAPTCHA" {
		t.Errorf("the create body's rules[0].redirectOptions = %#v, want the wire name %q",
			opts, "type")
	}
}

// TestAnIDsOwnSegmentsAreKeyedBySchemaNameToo. stateFrom merges TWO sources
// into one state map: the response body, and the attributes ParseProviderID
// recovered from the provider id. The second is keyed by self_link's own
// PLACEHOLDER names, and a placeholder is spelled in whichever namespace the
// template's source wrote it in.
//
// gcp.resourcerecordset is the one type in the corpus where those differ:
// its self_link came from Cloud DNS's own Discovery path, so it captures
// "{type}" while configuration supplies "type_value". Left untranslated, the
// id's own segment puts "type" into the very same state map the body puts
// "type_value" into, and the host diffs configuration against both.
func TestAnIDsOwnSegmentsAreKeyedBySchemaNameToo(t *testing.T) {
	gcptest.Isolate(t)
	s := gcpfake.New(t)
	defer s.Close()
	ty, c := syncCatalogFor(t, "gcp.resourcerecordset")
	requireRenamedAt(t, ty, "type_value", "type")
	if !contains(ty.SelfLink, "{type}") {
		t.Fatalf("this test exists for a self_link capturing a renamed attribute; got %q", ty.SelfLink)
	}
	const id = "projects/p/managedZones/z/rrsets/www.example.com./A"
	// Deliberately WITHOUT "type" in the body: Cloud DNS does echo it, but
	// this test is about the half that comes from the id, and a body carrying
	// it too would mask a failure to translate the id's own segment.
	s.Seed("/dns/v1/"+id, map[string]any{"ttl": float64(300), "rrdatas": []any{"10.0.0.1"}})
	p := testProviderWithCatalog(t, s, c)

	st, err := p.Read(context.Background(), &resource.ResourceState{Type: ty.Name, ProviderID: id})
	if err != nil {
		t.Fatal(err)
	}
	if st == nil {
		t.Fatal("Read reported absence for a resource the fake is holding")
	}
	if _, leaked := st.Attributes["type"]; leaked {
		t.Errorf("the id's own segment reached state under the WIRE name: %v", st.Attributes)
	}
	if got, ok := st.Attributes["type_value"].AsString(); !ok || got != "A" {
		t.Errorf("state[type_value] = %#v, want the segment the id carries under %q",
			st.Attributes["type_value"], "type")
	}
}

// TestTheBestEffortStateIsKeyedBySchemaNameToo. bestEffortState is the one
// path that builds state WITHOUT going through stateFrom: the
// eventual-consistency window where GCP has confirmed the create but the
// readback still 404s, and the orphan rule says report what we know rather
// than error. It merges desired's own attributes (schema-keyed) with the
// awaited response body (wire-keyed), so the same translation has to happen
// here or that window produces a state holding both spellings of the same
// attribute.
func TestTheBestEffortStateIsKeyedBySchemaNameToo(t *testing.T) {
	gcptest.Isolate(t)
	withJitterForTest(t, func(int64) int64 { return 0 })
	s := gcpfake.New(t)
	defer s.Close()
	ty, c := syncCatalogFor(t, "gcp.endpointpolicy")
	requireRenamedAt(t, ty, "type_value", "type")
	// Never readable, so Create falls through to bestEffortState rather than
	// to stateFrom -- the same mechanism
	// TestCreateTrustsTheAwaitWhenTheReadbackNeverSeesIt uses.
	s.NotFoundTimes("/v1/projects/p/locations/global/endpointPolicies/ep1", 1<<30)
	p := testProviderWithCatalog(t, s, c)

	st, err := p.Create(context.Background(), &resource.DesiredResource{
		Type: ty.Name,
		Attrs: attrsMixed(map[string]any{
			"project": "p", "name": "ep1", "type_value": "SIDECAR_PROXY",
		}),
	})
	if err != nil {
		t.Fatalf("Create errored for a resource GCP confirmed creating: %v", err)
	}
	if st == nil {
		t.Fatal("Create returned no state for a resource that exists")
	}
	if _, leaked := st.Attributes["type"]; leaked {
		t.Errorf("the awaited body reached state under the WIRE name, alongside the schema "+
			"name desire supplied: %v", st.Attributes)
	}
	if got, ok := st.Attributes["type_value"].AsString(); !ok || got != "SIDECAR_PROXY" {
		t.Errorf("state[type_value] = %#v, want the value the create was given",
			st.Attributes["type_value"])
	}
}

// --- Task 18b: the create url is built from the bindings the generator stored.

// nestedBindingCatalog is gcp.bigquery.table's shape, reduced: a create url
// whose two id segments live NESTED under tableReference, with the binding
// the generator stores for them.
func nestedBindingCatalog() *catalog.Catalog {
	return &catalog.Catalog{Types: []*catalog.Type{{
		Name:           "gcp.table",
		BaseURL:        "projects/{{project}}/datasets/{{dataset_id}}/tables/{{table_id}}",
		SelfLink:       "projects/{{project}}/datasets/{{dataset_id}}/tables/{{table_id}}",
		Scope:          catalog.ScopeGlobal,
		TimeoutSeconds: 30,
		CreateBindings: map[string]*catalog.CreateBinding{
			"dataset_id": {Attr: "tableReference.datasetId"},
			"table_id":   {Attr: "tableReference.tableId"},
		},
		Attributes: map[string]*catalog.Attr{
			"tableReference": {Canonical: "tableReference", Kind: value.KindMap, Fields: map[string]*catalog.Attr{
				"datasetId": {Canonical: "datasetId", Kind: value.KindString},
				"tableId":   {Canonical: "tableId", Kind: value.KindString},
			}},
			"description": {Canonical: "description", Kind: value.KindString},
		},
	}}}
}

// TestACreateURLIsBuiltFromANestedBinding. The values a magic-modules
// template asks for by snake_case name are two levels down the attribute
// tree, and nothing at the top level has either name. Without the stored
// binding the create fails on `url template ... needs "dataset_id"` before a
// byte is sent.
func TestACreateURLIsBuiltFromANestedBinding(t *testing.T) {
	gcptest.Isolate(t)
	s := gcpfake.New(t)
	defer s.Close()
	p := testProviderWithCatalog(t, s, nestedBindingCatalog())

	// The fake answers this particular shape with a 400 (bigquery's create
	// url addresses the ITEM, not a collection, so the fake finds no id to
	// assign). That is beside the point: what is being asserted is the URL
	// the POST went to, which is decided before anything is sent.
	_, _ = p.Create(context.Background(), &resource.DesiredResource{
		Type: "gcp.table",
		Attrs: attrsMixed(map[string]any{
			"tableReference": map[string]any{"datasetId": "ds", "tableId": "tb"},
			"description":    "hello",
		}),
	})
	req, ok := firstPost(s)
	if !ok {
		t.Fatal("nothing was POSTed")
	}
	if req.Path != "/projects/p/datasets/ds/tables/tb" {
		t.Errorf("POSTed to %q, want /projects/p/datasets/ds/tables/tb -- the nested "+
			"tableReference.datasetId and .tableId are what the url's snake_case "+
			"placeholders name", req.Path)
	}
	// The body still carries tableReference: only the PLACEHOLDER names are
	// kept out of the body, and "tableReference" is not one of them.
	if !contains(string(req.Body), "tableReference") {
		t.Errorf("tableReference was stripped from the request body: %s; GCP needs it there, "+
			"and only a url placeholder's own name is excluded", req.Body)
	}
}

// TestACreateURLIsBuiltFromAPatternTemplate is part B at the runtime end.
// iam's "{+name}/serviceAccounts" means the PARENT PROJECT, and the binding
// the generator took from Discovery's pattern expands to projects/{project},
// whose own placeholder the instance supplies.
func TestACreateURLIsBuiltFromAPatternTemplate(t *testing.T) {
	gcptest.Isolate(t)
	s := gcpfake.New(t)
	defer s.Close()
	c := &catalog.Catalog{Types: []*catalog.Type{{
		Name:           "gcp.sa",
		PathPrefix:     "v1/",
		BaseURL:        "{+name}/serviceAccounts",
		SelfLink:       "{+name}",
		Scope:          catalog.ScopeGlobal,
		TimeoutSeconds: 30,
		CreateBindings: map[string]*catalog.CreateBinding{
			"name": {Template: "projects/{project}"},
		},
		Attributes: map[string]*catalog.Attr{
			"accountId": {Canonical: "accountId", Kind: value.KindString},
		},
	}}}
	p := testProviderWithCatalog(t, s, c)

	_, _ = p.Create(context.Background(), &resource.DesiredResource{
		Type:  "gcp.sa",
		Attrs: attrsMixed(map[string]any{"accountId": "bot"}),
	})
	req, ok := firstPost(s)
	if !ok {
		t.Fatal("nothing was POSTed")
	}
	if req.Path != "/v1/projects/p/serviceAccounts" {
		t.Errorf("POSTed to %q, want /v1/projects/p/serviceAccounts -- {+name} here is the "+
			"parent project, which is what Discovery's own pattern for the parameter says", req.Path)
	}
}

// TestATypeThatCannotBuildItsCreateURLIsRefusedBeforeAnyRequest. 38 types
// shipped claiming a create they could not perform. The refusal names the
// fact, not the symptom, and above all it happens before anything is sent:
// a half-expanded url ("projects/p/instances//sslCerts") is answered by GCP
// with a 404 that names nothing.
func TestATypeThatCannotBuildItsCreateURLIsRefusedBeforeAnyRequest(t *testing.T) {
	gcptest.Isolate(t)
	s := gcpfake.New(t)
	defer s.Close()
	c := &catalog.Catalog{Types: []*catalog.Type{{
		Name:           "gcp.orphan",
		BaseURL:        "folders/{folder}/locations/{location}/buckets",
		SelfLink:       "{+name}",
		TimeoutSeconds: 30,
		Attributes:     map[string]*catalog.Attr{"description": {Canonical: "description"}},
	}}}
	p := testProviderWithCatalog(t, s, c)

	_, err := p.Create(context.Background(), &resource.DesiredResource{
		Type:  "gcp.orphan",
		Attrs: attrsMixed(map[string]any{"description": "x"}),
	})
	if err == nil {
		t.Fatal("a type whose create url cannot be built reported success")
	}
	if !contains(err.Error(), "folder") || !contains(err.Error(), "cannot be created") {
		t.Errorf("the refusal does not name the type's problem: %v", err)
	}
	if len(s.Requests()) != 0 {
		t.Errorf("%d requests were sent for a type that cannot build its create url", len(s.Requests()))
	}
}

// firstPost is the fake's first POST, which for a create is the create
// itself -- the readback that follows is a GET.
func firstPost(s *gcpfake.Server) (gcpfake.Request, bool) {
	for _, r := range s.Requests() {
		if r.Method == "POST" {
			return r, true
		}
	}
	return gcpfake.Request{}, false
}

// TestARefreshKeepsAnInputOnlyFieldGCPNeverReturns goes through Read and the
// reconciler the runtime ACTUALLY builds (projects.go), not the package-level
// ReconcileAttrs. That distinction nearly shipped wrong: the first version of
// the carry-forward was gated on a flag only ReconcileAttrs set, so the unit
// tests passed and production would have dropped the field.
//
// The fake is SEEDED with a body that lacks the field rather than created
// through, because the fake stores whatever it is sent and would hand the
// field straight back -- the one behaviour Google does not have, and the one
// that would make this test pass with the feature deleted.
func TestARefreshKeepsAnInputOnlyFieldGCPNeverReturns(t *testing.T) {
	gcptest.Isolate(t)
	s := gcpfake.New(t)
	defer s.Close()
	ty := widgetType()
	ty.Attributes["bootImage"] = &catalog.Attr{Canonical: "bootImage", Kind: value.KindString, InputOnly: true}
	p := testProviderWithCatalog(t, s, &catalog.Catalog{Types: []*catalog.Type{ty}})
	const id = "projects/p/locations/r/widgets/one"
	s.Seed("/v1/"+id, map[string]any{"name": "one", "sizeGb": float64(10)})

	st, err := p.Read(context.Background(), &resource.ResourceState{
		Type: "gcp.widget", ProviderID: id,
		Attributes: map[string]value.Value{
			"name":      value.String("one", value.SourceExplicit),
			"bootImage": value.String("debian-12", value.SourceExplicit),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if st == nil {
		t.Fatal("the seeded resource read as absent")
	}
	if got := st.Attributes["bootImage"]; !got.Equal(value.String("debian-12", value.SourceExplicit)) {
		t.Errorf("bootImage after a refresh = %v, want it carried forward; missing, every plan would propose changing it", got)
	}
}

// shortNamed is a type whose schema means the SHORT name by `name` -- its id
// template ends in "/{{name}}" -- created by AIP-133's "?widgetId={{name}}".
func shortNamed() *catalog.Type {
	ty := widgetType()
	ty.CreateURL = "projects/{{project}}/locations/{{region}}/widgets?widgetId={{name}}"
	return ty
}

// TestAFullNameForThisResourceIsReportedAsTheShortOne. AIP-133 APIs answer a
// create with `name` set to the FULL relative resource name. For a type whose
// schema calls the short name `name`, reporting that as it came back makes
// state and configuration disagree on a field in the url, and every plan after
// a create proposes a replacement -- 37 shipping types, Eventarc triggers
// among them. The fake now answers the way Google does, which is why this can
// be tested at all.
func TestAFullNameForThisResourceIsReportedAsTheShortOne(t *testing.T) {
	gcptest.Isolate(t)
	s := gcpfake.New(t)
	defer s.Close()
	p := testProviderWithCatalog(t, s, &catalog.Catalog{Types: []*catalog.Type{shortNamed()}})
	st, err := p.Create(context.Background(), widgetDesired(map[string]any{
		"project": "p", "region": "r", "name": "one", "sizeGb": int64(10),
	}))
	if err != nil {
		t.Fatal(err)
	}
	stored, _ := s.Get("/v1/projects/p/locations/r/widgets/one")
	if stored["name"] != "projects/p/locations/r/widgets/one" {
		t.Fatalf("the fake answered name %v; this test needs the full name Google sends", stored["name"])
	}
	if got := st.Attributes["name"]; !got.Equal(value.String("one", got.Source)) {
		t.Errorf("state name after create = %v, want the short name configuration wrote", got)
	}
	read, err := p.Read(context.Background(), st)
	if err != nil || read == nil {
		t.Fatalf("read: %v", err)
	}
	if got := read.Attributes["name"]; !got.Equal(value.String("one", got.Source)) {
		t.Errorf("state name after a refresh = %v, want the short name", got)
	}
}

// TestOnlyThisResourcesOwnNameIsShortened. The rule fires on the same
// collection and the same last segment as the resource's own id, and nowhere
// else -- including where the answer spells the project by NUMBER, which is
// still this resource.
func TestOnlyThisResourcesOwnNameIsShortened(t *testing.T) {
	ty := shortNamed()
	for full, want := range map[string]string{
		"projects/p/locations/r/widgets/one":         "one",
		"projects/123456789/locations/r/widgets/one": "one",
		"projects/p/locations/r/widgets/other":       "projects/p/locations/r/widgets/other",
		"projects/p/locations/r/gadgets/one":         "projects/p/locations/r/gadgets/one",
	} {
		attrs := map[string]value.Value{"name": value.String(full, value.SourceProvider)}
		shortNameFromOwnID(ty, "projects/p/locations/r/widgets/one", attrs, value.Value{})
		if got, _ := attrs["name"].Raw.(string); got != want {
			t.Errorf("%s: got %q, want %q", full, got, want)
		}
	}
}

// TestAFullPathNameIsLeftAlone. Where the schema's `name` IS the full path --
// a Pub/Sub topic, id template "{+topic}" -- or is Google's to set, there is no
// short name to report, and the full one is the value configuration holds.
func TestAFullPathNameIsLeftAlone(t *testing.T) {
	topic := &catalog.Type{SelfLink: "{+topic}", Attributes: map[string]*catalog.Attr{"name": {Kind: value.KindString}}}
	job := &catalog.Type{SelfLink: "projects/{{project}}/jobs/{{name}}", Attributes: map[string]*catalog.Attr{"name": {Kind: value.KindString, Output: true}}}
	for _, ty := range []*catalog.Type{topic, job} {
		attrs := map[string]value.Value{"name": value.String("projects/p/x/one", value.SourceProvider)}
		shortNameFromOwnID(ty, "projects/p/x/one", attrs, value.Value{})
		if got, _ := attrs["name"].Raw.(string); got != "projects/p/x/one" {
			t.Errorf("self_link %q: name shortened to %q", ty.SelfLink, got)
		}
	}
}

// TestARegionalTypeIsReachedAtItsRegionsEndpoint. A regional secret exists
// only at secretmanager.<location>.rep.googleapis.com, and every url for one
// names its location. A url that names none, and a type with no template,
// keep the API's own host.
func TestARegionalTypeIsReachedAtItsRegionsEndpoint(t *testing.T) {
	regional := &catalog.Type{APIBaseURL: "https://secretmanager.googleapis.com/", PathPrefix: "v1/",
		EndpointTemplate: "https://secretmanager.{location}.rep.googleapis.com/"}
	global := &catalog.Type{APIBaseURL: "https://secretmanager.googleapis.com/", PathPrefix: "v1/"}
	for _, c := range []struct {
		ty        *catalog.Type
		rel, want string
	}{
		{regional, "projects/p/locations/europe-west1/secrets/s", "https://secretmanager.europe-west1.rep.googleapis.com/v1/projects/p/locations/europe-west1/secrets/s"},
		{regional, "projects/p/locations/us-central1/secrets?secretId=s", "https://secretmanager.us-central1.rep.googleapis.com/v1/projects/p/locations/us-central1/secrets?secretId=s"},
		{regional, "projects/p/secrets/s", "https://secretmanager.googleapis.com/v1/projects/p/secrets/s"},
		{global, "projects/p/locations/europe-west1/secrets/s", "https://secretmanager.googleapis.com/v1/projects/p/locations/europe-west1/secrets/s"},
	} {
		if got := absURL(c.ty, c.rel); got != c.want {
			t.Errorf("absURL(%q) = %q, want %q", c.rel, got, c.want)
		}
	}
}

// TestACreateAnsweredWithoutASelfLinkStillHasAnID. Cloud DNS answers a create
// with the resource and no selfLink, and a DNS policy's id template, from
// Discovery, ends in {policy} while the resource calls it name. The policy
// was created on real Google and Create failed computing its id, which is a
// resource nothing tracks. The last placeholder takes the resource's name.
func TestACreateAnsweredWithoutASelfLinkStillHasAnID(t *testing.T) {
	gcptest.Isolate(t)
	s := gcpfake.New(t)
	defer s.Close()
	ty := widgetType()
	ty.SelfLink = "projects/{project}/locations/{region}/widgets/{widget}"
	ty.ImportFormat = ty.SelfLink
	p := testProviderWithCatalog(t, s, &catalog.Catalog{Types: []*catalog.Type{ty}})

	st, err := p.Create(context.Background(), widgetDesired(map[string]any{
		"project": "p", "region": "r", "name": "two", "sizeGb": int64(10),
	}))
	if err != nil {
		t.Fatalf("Create failed for a resource that was created: %v", err)
	}
	if st.ProviderID != "projects/p/locations/r/widgets/two" {
		t.Errorf("provider id = %q, want projects/p/locations/r/widgets/two", st.ProviderID)
	}
}

// TestTheNameFillsOnlyAPlaceholderThatStandsForIt. A record set's id ends
// {name}/{type}; with type unset the id is unknown, and filling {type} with
// the name read the record set at .../rrsets/www/www.
func TestTheNameFillsOnlyAPlaceholderThatStandsForIt(t *testing.T) {
	merged := map[string]value.Value{"name": value.String("www", value.SourceExplicit)}
	fillLastFromName("projects/{project}/managedZones/{managedZone}/rrsets/{name}/{type}", merged)
	if _, filled := merged["type"]; filled {
		t.Errorf("{type} was filled with the name: %v", merged["type"].Raw)
	}
	fillLastFromName("projects/{project}/policies/{policy}", merged)
	if merged["policy"].Raw != "www" {
		t.Errorf("{policy} = %v, want the name", merged["policy"].Raw)
	}
}

// TestABareCaptureIDIsBuiltFromAShortName. A logging sink's id template is
// "{+sinkName}", its full name, and it is configured and answered as the
// short "my-sink": read at "my-sink" it was never found. A full name is used
// as it is.
func TestABareCaptureIDIsBuiltFromAShortName(t *testing.T) {
	ty := &catalog.Type{Name: "gcp.logging.sink", SelfLink: "{+sinkName}", CreateURL: "projects/{{project}}/sinks",
		Attributes: map[string]*catalog.Attr{"name": {Canonical: "name", Kind: value.KindString}}}
	s := func(v string) value.Value { return value.String(v, value.SourceExplicit) }
	id, err := ProviderID(ty, map[string]any{"name": "my-sink"}, map[string]value.Value{"project": s("p"), "name": s("my-sink")})
	if err != nil || id != "projects/p/sinks/my-sink" {
		t.Errorf("id = %q, %v; want projects/p/sinks/my-sink", id, err)
	}
	id, err = ProviderID(ty, map[string]any{"name": "projects/p/sinks/other"}, map[string]value.Value{"project": s("p")})
	if err != nil || id != "projects/p/sinks/other" {
		t.Errorf("a full name as the id: %q, %v", id, err)
	}
}

// TestABareCaptureNameIsShortenedOnlyWhenConfiguredShort. A certificate
// issuance config is configured short and answered in full; a Cloud Tasks
// queue must be configured in full. Shortening the queue's name would be the
// drift the rule exists to prevent.
func TestABareCaptureNameIsShortenedOnlyWhenConfiguredShort(t *testing.T) {
	ty := &catalog.Type{SelfLink: "{+name}", Attributes: map[string]*catalog.Attr{"name": {Canonical: "name", Kind: value.KindString}}}
	s := func(v string) value.Value { return value.String(v, value.SourceExplicit) }
	full := "projects/p/locations/l/configs/c"
	for _, c := range []struct {
		ref  value.Value
		want string
	}{
		{s("c"), "c"},
		{s(full), full},
		{value.Value{}, full},
	} {
		attrs := map[string]value.Value{"name": s(full)}
		shortNameFromOwnID(ty, full, attrs, c.ref)
		if attrs["name"].Raw != c.want {
			t.Errorf("configured %v: name reported %v, want %q", c.ref.Raw, attrs["name"].Raw, c.want)
		}
	}
}

// TestAnIDWhoseTemplateStartsWithABoundParentParses. An address group's id
// template starts {{parent}}, bound at create to projects/{project}, and its
// ids are projects/p/locations/r/addressGroups/g: parent is two segments,
// a placeholder captures one, and no such id ever parsed, so the type could
// be created and never read, updated or deleted.
func TestAnIDWhoseTemplateStartsWithABoundParentParses(t *testing.T) {
	ty := &catalog.Type{Name: "gcp.networksecurity.addressgroup",
		SelfLink:       "{{parent}}/locations/{{location}}/addressGroups/{{name}}",
		CreateBindings: map[string]*catalog.CreateBinding{"parent": {Template: "projects/{project}"}}}
	got, err := ParseProviderID(ty, "projects/p/locations/r/addressGroups/g")
	if err != nil {
		t.Fatal(err)
	}
	for k, want := range map[string]string{"parent": "projects/p", "location": "r", "name": "g", "project": "p"} {
		if got[k].Raw != want {
			t.Errorf("%s = %v, want %q", k, got[k].Raw, want)
		}
	}
}
