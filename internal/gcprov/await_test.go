package gcprov

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/infrena/infrena-provider-gcp/internal/catalog"
	"github.com/infrena/infrena-provider-gcp/internal/gcpfake"
	"github.com/infrena/infrena-provider-gcp/internal/gcptest"
)

func TestASynchronousMutationIsNotPolled(t *testing.T) {
	gcptest.Isolate(t)
	s := gcpfake.New(t)
	defer s.Close()
	p := testProvider(t, s)

	ty := &catalog.Type{Name: "gcp.widget", Await: catalog.AwaitNone}
	body := map[string]any{"name": "widgets/one", "sizeGb": float64(10)}
	got, err := p.await(context.Background(), ty, body)
	if err != nil {
		t.Fatal(err)
	}
	if got["sizeGb"] != float64(10) {
		t.Errorf("the response was not passed through: %v", got)
	}
	if n := len(s.Requests()); n != 0 {
		t.Errorf("%d requests made awaiting a synchronous mutation, want 0", n)
	}
}

// TestALongRunningOperationIsPolledUntilDone seeds the operation with
// SeedOperation rather than handing await a hand-built map the fake has
// never heard of -- gcpfake's handleGetOperation 404s an unknown name, same
// as it always has for longrunning (it never auto-vivified the way
// handleComputeWait used to), and rightly so: a test asserting against an
// operation that exists nowhere is a bug in the test. The name is a
// realistic full relative resource name, not the bare "operations/op-N"
// shape the fake's own newLongRunningOp mints, since SeedOperation lets the
// caller choose it.
func TestALongRunningOperationIsPolledUntilDone(t *testing.T) {
	gcptest.Isolate(t)
	s := gcpfake.New(t)
	defer s.Close()
	s.SetOperationStyle(gcpfake.OpLongRunning)
	s.SeedOperation("projects/p/locations/r/operations/op-1", map[string]any{"name": "widgets/one", "sizeGb": float64(10)})
	p := testProvider(t, s)

	ty := &catalog.Type{
		Name: "gcp.widget", Await: catalog.AwaitLongRunning, TimeoutSeconds: 30,
		OperationPollPath: "v1/{+name}",
	}
	op := map[string]any{"name": "projects/p/locations/r/operations/op-1", "done": false}
	got, err := p.await(context.Background(), ty, op)
	if err != nil {
		t.Fatal(err)
	}
	if got == nil {
		t.Fatal("await returned nothing")
	}
	if len(s.Requests()) == 0 {
		t.Error("the operation was never polled")
	}
	// Specifically that OperationPollPath's "v1/" was actually expanded in --
	// not just that some request landed on the seeded name, which
	// gcpfake's own leniency about a missing version prefix (trimVersionPrefix)
	// would let a bare, unexpanded name pass too.
	var polledExpanded bool
	for _, r := range s.Requests() {
		if contains(r.Path, "/v1/projects/p/locations/r/operations/op-1") {
			polledExpanded = true
		}
	}
	if !polledExpanded {
		t.Errorf("OperationPollPath was not expanded into the poll url: %+v", s.Requests())
	}
}

// TestAFailedOperationReportsGCPsMessage. An operation that completes with
// an error is not an await failure, it is a GCP failure, and the message is
// the only thing the user can act on.
func TestAFailedOperationReportsGCPsMessage(t *testing.T) {
	gcptest.Isolate(t)
	s := gcpfake.New(t)
	defer s.Close()
	s.SetOperationStyle(gcpfake.OpLongRunning)
	s.CompleteOperationWithError("operations/bad", "INVALID_ARGUMENT", "Disk size must be at least 10 GB.")
	p := testProvider(t, s)

	ty := &catalog.Type{
		Name: "gcp.widget", Await: catalog.AwaitLongRunning, TimeoutSeconds: 30,
		OperationPollPath: "v1/{+name}",
	}
	_, err := p.await(context.Background(), ty, map[string]any{"name": "operations/bad", "done": false})
	if err == nil {
		t.Fatal("a failed operation was reported as success")
	}
	if !contains(err.Error(), "Disk size must be at least 10 GB.") {
		t.Errorf("GCP's own message was dropped: %v", err)
	}
}

// TestAComputeOperationIsPolledOnItsScope. The wait url is built from
// ty.OperationWaitPath, the API's own published wait path, taken verbatim --
// not reconstructed from a scope word. Three attempts at reconstructing it
// were all wrong, this task's own included: the earlier "OperationScope is a
// bare word, append Operations" fix (still visible in this task's git
// history) built ".../zoneOperations/op-1/wait", but a reviewer who checked
// schemas/compute.json directly found the real wire segment is always
// "operations" -- "zoneOperations" is only Discovery's COLLECTION name for
// the scope and never appears in a url at all. Both the earlier and the
// current fixture happened to pass their own test, which is exactly why this
// one asserts the real literal path rather than a substring that could match
// either shape.
//
// The operation is seeded with SeedComputeOperation rather than handed to
// await as a hand-built map the fake has never heard of: the fake used to
// auto-vivify an unknown compute operation as one that completes
// successfully, but that is inference from the shape of a request rather
// than the test saying what exists -- exactly what this package's declared
// collections (server.go) already rejected for ordinary resources. A test
// asserting against an operation that does not exist anywhere is a bug in
// the test, not a reason to make the fake more lenient.
func TestAComputeOperationIsPolledOnItsScope(t *testing.T) {
	gcptest.Isolate(t)
	s := gcpfake.New(t)
	defer s.Close()
	s.SetOperationStyle(gcpfake.OpCompute)
	s.SeedComputeOperation("op-1", "/projects/p/zones/us-central1-a/instances/web1")
	p := testProvider(t, s)

	ty := &catalog.Type{
		Name: "gcp.instance", Await: catalog.AwaitComputeOperation, Scope: catalog.ScopeZonal, TimeoutSeconds: 30,
		OperationWaitPath: "projects/{project}/zones/{zone}/operations/{operation}/wait",
	}
	op := map[string]any{"name": "op-1", "status": "RUNNING", "zone": "us-central1-a"}
	if _, err := p.await(context.Background(), ty, op); err != nil {
		t.Fatal(err)
	}
	var polled bool
	for _, r := range s.Requests() {
		if contains(r.Path, "/zones/us-central1-a/operations/op-1/wait") {
			polled = true
		}
	}
	// `wait` rather than `get`: it long-polls to a 2-minute deadline instead of
	// burning one request per second against the project's quota.
	if !polled {
		t.Errorf("the operations wait endpoint was not used with the right scope: %+v", s.Requests())
	}
}

// TestANoWaitComputeOperationIsPolledWithGet covers the 7 of 65 compute-style
// types (container, sqladmin) whose API publishes no wait method at all --
// OperationWaitPath is empty for them, and the operation must be polled with
// an ordinary GET on OperationPollPath instead, same status/error body shape
// as the wait case. A wait call against one of these 404s rather than
// long-polling, confirmed against their own Discovery documents (no `wait`
// method anywhere in either schema).
func TestANoWaitComputeOperationIsPolledWithGet(t *testing.T) {
	gcptest.Isolate(t)
	s := gcpfake.New(t)
	defer s.Close()
	s.SetOperationStyle(gcpfake.OpCompute)
	s.SeedComputeOperation("op-1", "/v1/projects/p/instances/db1")
	p := testProvider(t, s)

	ty := &catalog.Type{
		Name: "gcp.sqladmin.instance", Await: catalog.AwaitComputeOperation, TimeoutSeconds: 30,
		OperationPollPath: "v1/projects/{project}/operations/{operation}",
	}
	op := map[string]any{"name": "op-1", "status": "RUNNING"}
	got, err := p.await(context.Background(), ty, op)
	if err != nil {
		t.Fatal(err)
	}
	if got == nil {
		t.Fatal("await returned nothing")
	}
	var polled bool
	for _, r := range s.Requests() {
		if r.Method == "GET" && contains(r.Path, "/projects/p/operations/op-1") {
			polled = true
		}
		if r.Method == "POST" && contains(r.Path, "/wait") {
			t.Errorf("a wait call was made against a type with no wait method: %+v", r)
		}
	}
	if !polled {
		t.Errorf("the operation poll path was not used: %+v", s.Requests())
	}
}

// TestAwaitDoesNotAbandonAMutationInFlight. Once a mutation is sent,
// abandoning it leaves a resource that exists and is tracked nowhere. The
// await runs under context.WithoutCancel for exactly that reason.
func TestAwaitDoesNotAbandonAMutationInFlight(t *testing.T) {
	gcptest.Isolate(t)
	s := gcpfake.New(t)
	defer s.Close()
	s.SetOperationStyle(gcpfake.OpLongRunning)
	s.SeedOperation("projects/p/locations/r/operations/op-1", map[string]any{"name": "widgets/one", "sizeGb": float64(10)})
	p := testProvider(t, s)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled before await is called

	ty := &catalog.Type{
		Name: "gcp.widget", Await: catalog.AwaitLongRunning, TimeoutSeconds: 30,
		OperationPollPath: "v1/{+name}",
	}
	got, err := p.await(ctx, ty, map[string]any{"name": "projects/p/locations/r/operations/op-1", "done": false})
	if errors.Is(err, context.Canceled) {
		t.Fatal("await abandoned an operation in flight on a cancelled context")
	}
	if err != nil {
		t.Fatal(err)
	}
	if got == nil {
		t.Error("await returned nothing")
	}
}

// TestAwaitStopsAtTheTypesTimeout. Earlier versions of this test (before the
// operation was seeded) "passed" vacuously: the unregistered operation
// 404'd on the very first poll, so err != nil and the elapsed check were
// both trivially satisfied without the timeout loop ever running. Seeding
// the operation first, so the poll actually succeeds and reports
// unfinished, combined with NeverCompleteOperations, is what makes the
// context deadline genuinely the thing under test -- confirmed by sabotage:
// dropping NeverCompleteOperations (or the timeout wrapping in await
// itself) makes this test fail rather than pass for the wrong reason.
func TestAwaitStopsAtTheTypesTimeout(t *testing.T) {
	gcptest.Isolate(t)
	s := gcpfake.New(t)
	defer s.Close()
	s.SetOperationStyle(gcpfake.OpLongRunning)
	s.SeedOperation("projects/p/locations/r/operations/op-1", nil)
	s.NeverCompleteOperations()
	p := testProvider(t, s)

	ty := &catalog.Type{
		Name: "gcp.widget", Await: catalog.AwaitLongRunning, TimeoutSeconds: 1,
		OperationPollPath: "v1/{+name}",
	}
	start := time.Now()
	_, err := p.await(context.Background(), ty, map[string]any{"name": "projects/p/locations/r/operations/op-1", "done": false})
	if err == nil {
		t.Fatal("an operation that never completes was reported as success")
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Errorf("await ran for %v against a 1s timeout", elapsed)
	}
}

// TestAwaitDoesNotAbandonAComputeOperationInFlight covers, against the
// compute path, the same property TestAwaitDoesNotAbandonAMutationInFlight
// covers against the longrunning path: once a mutation is sent, abandoning
// it leaves a resource that exists and is tracked nowhere. The
// context.WithoutCancel wrapping this guards is done once in await() itself,
// before either strategy is dispatched to, so this exercises the exact same
// code as the longrunning version of this test -- it is not a weaker
// substitute, just a different strategy's operation shape. Kept alongside
// the longrunning version rather than removed now that both run, since it
// costs nothing and pins the property to both operation shapes independently.
func TestAwaitDoesNotAbandonAComputeOperationInFlight(t *testing.T) {
	gcptest.Isolate(t)
	s := gcpfake.New(t)
	defer s.Close()
	s.SetOperationStyle(gcpfake.OpCompute)
	s.SeedComputeOperation("op-1", "/projects/p/zones/us-central1-a/instances/web1")
	p := testProvider(t, s)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled before await is called

	ty := &catalog.Type{
		Name: "gcp.instance", Await: catalog.AwaitComputeOperation, Scope: catalog.ScopeZonal, TimeoutSeconds: 30,
		OperationWaitPath: "projects/{project}/zones/{zone}/operations/{operation}/wait",
	}
	op := map[string]any{"name": "op-1", "status": "RUNNING", "zone": "us-central1-a"}
	got, err := p.await(ctx, ty, op)
	if errors.Is(err, context.Canceled) {
		t.Fatal("await abandoned an operation in flight on a cancelled context")
	}
	if err != nil {
		t.Fatal(err)
	}
	if got == nil {
		t.Error("await returned nothing")
	}
}

// TestOperationErrorRendersBothFailureShapes covers operationError directly
// rather than through a round trip, because gcpfake currently has no
// equivalent of CompleteOperationWithError for a compute-style operation
// (only for longrunning) -- a compute error can only be injected today via a
// real create through CreateThenFailOperation, which is Task 13's territory.
// operationError itself is a pure function of the error map, so testing it
// directly is not a weaker substitute for a round trip; it is the more
// direct test. Both shapes GCP actually sends are covered: longrunning's
// {"code":<int>,"message":<string>} and compute's
// {"errors":[{"code":<string>,"message":<string>}, ...]}.
func TestOperationErrorRendersBothFailureShapes(t *testing.T) {
	ty := &catalog.Type{Name: "gcp.widget"}

	lro := operationError(ty, map[string]any{
		"code": float64(3), "message": "Disk size must be at least 10 GB.",
	})
	if !contains(lro.Error(), "Disk size must be at least 10 GB.") {
		t.Errorf("longrunning shape: GCP's message was dropped: %v", lro)
	}

	compute := operationError(ty, map[string]any{
		"errors": []any{
			map[string]any{"code": "RESOURCE_ERROR", "message": "Quota 'CPUS' exceeded."},
		},
	})
	if !contains(compute.Error(), "Quota 'CPUS' exceeded.") {
		t.Errorf("compute shape: GCP's message was dropped: %v", compute)
	}
}

// computeTypeForID is the fixture the three tests below share: a compute-style
// zonal type whose self_link ends in "{{name}}" (the shape 13 of the 65
// compute-style types have) and whose path_prefix is the one the fake's own
// urls carry, so reduceSelfLink has a real prefix to reduce a selfLink by.
func computeTypeForID() *catalog.Type {
	return &catalog.Type{
		Name: "gcp.instance", Await: catalog.AwaitComputeOperation,
		Scope: catalog.ScopeZonal, TimeoutSeconds: 30,
		PathPrefix:        "compute/v1/",
		SelfLink:          "projects/{{project}}/zones/{{zone}}/instances/{{name}}",
		OperationWaitPath: "projects/{project}/zones/{zone}/operations/{operation}/wait",
	}
}

// TestAComputeOperationYieldsTheResourceItCreatedNotItself. A compute
// Operation carries BOTH selfLink (its own url) and targetLink (the created
// resource's). ProviderID prefers selfLink, so returning the operation
// unchanged gave all 65 compute-style types a provider id naming an
// operation -- and every later Read, Delete and Import addressed that.
//
// The fake deliberately answers with a selfLink that differs from
// targetLink; if it ever stops doing so this test passes vacuously, which is
// how the original defect survived, so the two links are read off the fake's
// own answer and asserted to differ before anything else is checked.
func TestAComputeOperationYieldsTheResourceItCreatedNotItself(t *testing.T) {
	gcptest.Isolate(t)
	s := gcpfake.New(t)
	defer s.Close()
	s.SetOperationStyle(gcpfake.OpCompute)
	const target = "/compute/v1/projects/p/zones/us-central1-a/instances/web1"
	s.SeedComputeOperation("op-1", target)
	p := testProvider(t, s)

	// The fake's own answer, first: an operation whose selfLink equals its
	// targetLink (or is missing) makes everything below vacuous.
	opBody := getJSON(t, s.URL()+"/compute/v1/projects/p/zones/us-central1-a/operations/op-1")
	self, _ := opBody["selfLink"].(string)
	targetLink, _ := opBody["targetLink"].(string)
	if self == "" || targetLink == "" || self == targetLink {
		t.Fatalf("the fake no longer distinguishes an operation from its target, so this test proves nothing: selfLink=%q targetLink=%q", self, targetLink)
	}

	ty := computeTypeForID()
	awaited, err := p.await(context.Background(), ty,
		map[string]any{"name": "op-1", "status": "RUNNING", "zone": "us-central1-a"})
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := awaited["selfLink"].(string); got != targetLink {
		t.Errorf("await yielded selfLink %q, want the operation's targetLink %q", got, targetLink)
	}

	id, err := ProviderID(ty, awaited, attrs(map[string]string{
		"project": "p", "zone": "us-central1-a", "name": "web1",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if want := "projects/p/zones/us-central1-a/instances/web1"; id != want {
		t.Errorf("provider id = %q, want %q", id, want)
	}
	if contains(id, "/operations/") {
		t.Errorf("the provider id names an operation, not the resource: %q", id)
	}
}

// TestAComputeOperationsOwnNameNeverBecomesTheResources covers the second
// way the old return was wrong, for the 13 compute-style types whose
// self_link ends in "{{name}}": with no selfLink in the body, ProviderID
// merges the body's own fields over the caller's attributes, so the
// OPERATION's name ("operation-1740...") won and the id named a resource
// that never existed. The operation here is already DONE, so await returns
// on its first look and nothing is polled -- the merge is what is under
// test, not the polling.
func TestAComputeOperationsOwnNameNeverBecomesTheResources(t *testing.T) {
	gcptest.Isolate(t)
	s := gcpfake.New(t)
	defer s.Close()
	p := testProvider(t, s)

	ty := computeTypeForID()
	const target = "https://compute.googleapis.com/compute/v1/projects/p/zones/us-central1-a/instances/web1"
	awaited, err := p.await(context.Background(), ty, map[string]any{
		"name": "operation-1740000000000", "status": "DONE", "targetLink": target,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := awaited["name"]; ok {
		t.Errorf("the operation's own name is still in what await returned, where it merges over the caller's: %v", awaited)
	}
	id, err := ProviderID(ty, awaited, attrs(map[string]string{
		"project": "p", "zone": "us-central1-a", "name": "web1",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if want := "projects/p/zones/us-central1-a/instances/web1"; id != want {
		t.Errorf("provider id = %q, want %q -- the operation's name became the resource's", id, want)
	}
}

// TestAComputeOperationWithNoTargetSaysNothingRatherThanSomethingWrong. An
// operation carrying no targetLink names the created resource nowhere, so
// await returns (nil, nil) -- the same shape awaitLongRunning already
// returns for a done-with-no-response operation, and the one ProviderID
// answers by expanding self_link against the caller's own attributes.
func TestAComputeOperationWithNoTargetSaysNothingRatherThanSomethingWrong(t *testing.T) {
	gcptest.Isolate(t)
	s := gcpfake.New(t)
	defer s.Close()
	p := testProvider(t, s)

	ty := computeTypeForID()
	awaited, err := p.await(context.Background(), ty, map[string]any{
		"name": "operation-1740000000000", "status": "DONE",
	})
	if err != nil {
		t.Fatal(err)
	}
	if awaited != nil {
		t.Fatalf("await invented an identity from an operation that names no resource: %v", awaited)
	}
	id, err := ProviderID(ty, awaited, attrs(map[string]string{
		"project": "p", "zone": "us-central1-a", "name": "web1",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if want := "projects/p/zones/us-central1-a/instances/web1"; id != want {
		t.Errorf("provider id = %q, want %q", id, want)
	}
}

// getJSON reads one JSON body straight off the fake, for the one assertion a
// test cannot make through the provider: what the fake itself answers.
func getJSON(t *testing.T, url string) map[string]any {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s: %s", url, resp.Status)
	}
	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	return body
}
