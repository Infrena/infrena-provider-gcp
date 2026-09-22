package gcprov

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/infrena/infrena-provider-gcp/internal/catalog"
	"github.com/infrena/infrena-provider-gcp/internal/gcpfake"
	"github.com/infrena/infrena-provider-gcp/internal/gcptest"
	"github.com/infrena/infrena/pkg/value"
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
// zonal type whose self_link ENDS in "{{name}}" (the shape 12 of the 65
// compute-style types have; 13 CONTAIN a name placeholder somewhere, but
// ending in one is what the merge below turns on) and whose path_prefix is
// the one the fake's own urls carry, so reduceSelfLink has a real prefix to
// reduce a selfLink by.
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
// way the old return was wrong, for the 12 compute-style types whose
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

// operationResourceName is one operation's own relative resource name, the
// shape a modern GCP API's operations.get method takes and the shape its own
// published `pattern` demands (container's is
// "^projects/[^/]+/locations/[^/]+/operations/[^/]+$"). Both await strategies
// have to end up polling something of this shape; where they DIFFER is where
// they get it from, which is the whole of the defect the test below exists
// to catch.
const operationResourceName = "projects/p/locations/l/operations/op-1"

// TestEveryOperationURLTheRuntimeBuildsAddressesAnOperation replaces
// TestEveryOperationTemplateExpandsFromWhatTheRuntimeSupplies, which asserted
// that an operation template EXPANDS against a map of placeholder values the
// test itself supplied. Expanding and ADDRESSING something are different
// properties, and that difference is a whole class of defect this test was
// meant to catch and did not: gcp.container.cluster's "v1/{+name}" expands
// perfectly from the operation's own name, into
// "v1/operation-1740000000000", which matches nothing container serves. The
// old test passed on exactly that url, so it is not enough to assert that a
// template can be filled in.
//
// Two changes make it an addressing test. It drives the RUNTIME's own code --
// operationRequestURL for a compute-style await, awaitLongRunning's own
// expansion for a longrunning one -- rather than a parallel copy of the attrs
// map, so what is under test is what actually runs. And it asserts on the url
// that comes out: a poll target must name an operations collection, and must
// not be one bare segment.
//
// The operation handed in is shaped the way the API itself documents one.
// compute, container and sqladmin all describe Operation.name as the
// server-assigned ID ("Output only. The server-assigned ID for the
// operation." -- schemas/container.json), a BARE id, and all three publish an
// Operation.selfLink alongside it. A longrunning operation's name is the
// opposite: the operation's full relative resource name. Handing both
// strategies the same placeholder name meaning two different things is
// exactly how the defect arose.
func TestEveryOperationURLTheRuntimeBuildsAddressesAnOperation(t *testing.T) {
	c := mustCatalog(t)
	p := &Provider{settings: Settings{Project: "p", Zone: "z", Region: "r"}}
	var checked int
	for _, ty := range c.Types {
		if ty.OperationWaitPath == "" && ty.OperationPollPath == "" {
			continue
		}
		rel, err := pollPathTheRuntimeWouldRequest(p, ty)
		if err != nil {
			t.Errorf("%s: the runtime cannot build an operation url at all: %v", ty.Name, err)
			continue
		}
		checked++
		segs := strings.Split(strings.Trim(rel, "/"), "/")
		if len(segs) < 2 {
			t.Errorf("%s: polls %q, one bare segment, which addresses nothing", ty.Name, rel)
			continue
		}
		if !slices.Contains(segs, "operations") {
			t.Errorf("%s: polls %q, which names no operations collection", ty.Name, rel)
		}
	}
	if checked == 0 {
		t.Fatal("no type in the catalog has an operation path, so this test asserts nothing")
	}
}

// pollPathTheRuntimeWouldRequest returns ty's operation template as the
// runtime would actually fill it in -- the request url with ty.APIBaseURL
// taken back off, so what comes back is exactly the expanded template --
// built by the runtime's own code from an operation body shaped the way ty's
// own API answers one.
func pollPathTheRuntimeWouldRequest(p *Provider, ty *catalog.Type) (string, error) {
	switch ty.Await {
	case catalog.AwaitComputeOperation:
		_, url, err := p.operationRequestURL(ty, computeOperationAsItsAPIAnswersIt(ty))
		if err != nil {
			return "", err
		}
		return strings.TrimPrefix(url, ty.APIBaseURL), nil
	case catalog.AwaitLongRunning:
		// awaitLongRunning's own expansion, with its own value for "name":
		// for a google.longrunning.Operation the name IS the operation's full
		// relative resource name, which is what makes op["name"] the right
		// thing to fill "{+name}" with there and the wrong thing here.
		rel, err := ExpandURL(ty.OperationPollPath, map[string]value.Value{
			"name": value.String(operationResourceName, value.SourceProvider),
		})
		if err != nil {
			return "", err
		}
		return rel, nil
	default:
		return "", fmt.Errorf("has an operation path but await strategy %d never polls one", ty.Await)
	}
}

// computeOperationAsItsAPIAnswersIt is one compute-style operation body as
// the three APIs that answer them actually do: a BARE server-assigned id in
// "name", the operation's own absolute url in "selfLink" (all three of
// compute, container and sqladmin publish one), and the scope fields compute
// carries.
func computeOperationAsItsAPIAnswersIt(ty *catalog.Type) map[string]any {
	return map[string]any{
		"name":     "op-1",
		"status":   "RUNNING",
		"selfLink": ty.APIBaseURL + ty.PathPrefix + operationResourceName,
		"zone":     "z",
		"region":   "r",
	}
}

// containerStyleType is the fixture for the two tests below: the shape the 2
// of 65 compute-style types that poll with a captured path have
// (gcp.container.cluster and gcp.container.nodepool), taken from the real
// catalog -- no wait method, a poll path of "v1/{+name}", and an api prefix
// of "v1/" to reduce the operation's own selfLink by.
func containerStyleType() *catalog.Type {
	return &catalog.Type{
		Name: "gcp.container.cluster", Await: catalog.AwaitComputeOperation,
		TimeoutSeconds:    600,
		APIBaseURL:        "https://container.googleapis.com/",
		PathPrefix:        "v1/",
		OperationPollPath: "v1/{+name}",
	}
}

// TestAContainerStyleOperationIsPolledAtItsOwnResourceName. container's
// Operation.name is the server-assigned ID alone, while the method that polls
// it takes a full resource path ("^projects/[^/]+/locations/[^/]+/operations/
// [^/]+$"). The operation's own selfLink is the only thing in the body that
// carries that path, so it -- not the name -- is what fills "{+name}".
//
// The two links deliberately differ in more than punctuation: an
// implementation that took op["name"] would produce "v1/operation-1740000000000",
// which is a url container answers 404 to, so asserting the whole url rather
// than a substring is what makes this test fail on the defect.
func TestAContainerStyleOperationIsPolledAtItsOwnResourceName(t *testing.T) {
	p := &Provider{settings: Settings{Project: "p"}}
	ty := containerStyleType()
	op := map[string]any{
		"name":     "operation-1740000000000",
		"status":   "RUNNING",
		"selfLink": "https://container.googleapis.com/v1/projects/123/locations/us-central1/operations/operation-1740000000000",
	}
	method, url, err := p.operationRequestURL(ty, op)
	if err != nil {
		t.Fatal(err)
	}
	if method != http.MethodGet {
		t.Errorf("method = %s, want GET: container publishes no wait method", method)
	}
	want := "https://container.googleapis.com/v1/projects/123/locations/us-central1/operations/operation-1740000000000"
	if url != want {
		t.Errorf("poll url = %q, want %q", url, want)
	}
}

// TestAnOperationThatNamesOnlyAnIDIsRefusedRatherThanPolledSomewhereWrong.
// The bare id is all there is when the operation publishes no selfLink, or
// one under an api version this type's prefix cannot reduce. Expanding it
// into the template anyway produces "v1/operation-1740000000000" -- a url
// that parses, that GCP answers 404 to, and that says nothing about why. The
// named error is the better answer, and it is the one 13c's own fix replaced
// with a 404.
func TestAnOperationThatNamesOnlyAnIDIsRefusedRatherThanPolledSomewhereWrong(t *testing.T) {
	p := &Provider{settings: Settings{Project: "p"}}
	ty := containerStyleType()
	for _, op := range []map[string]any{
		{"name": "operation-1740000000000", "status": "RUNNING"},
		{"name": "operation-1740000000000", "status": "RUNNING",
			"selfLink": "https://container.googleapis.com/v1alpha1/projects/123/locations/us-central1/operations/operation-1740000000000"},
	} {
		_, url, err := p.operationRequestURL(ty, op)
		if err == nil {
			t.Fatalf("polling %q was accepted for an operation that names only an id: %v", url, op)
		}
		if !contains(err.Error(), "operation-1740000000000") || !contains(err.Error(), ty.Name) {
			t.Errorf("the error names neither the type nor the id it was given: %v", err)
		}
	}
}

// TestASqladminStyleOperationStillPollsOnItsBareID is the other side of the
// refusal above: for the 63 of 65 compute-style types whose template captures
// single segments ({operation}, {project}, {zone}, {region}) the bare id is
// exactly right, and nothing about D1's fix may change that. sqladmin's own
// poll path is the fixture; its operations carry no selfLink this type could
// reduce, and they do not need one.
func TestASqladminStyleOperationStillPollsOnItsBareID(t *testing.T) {
	p := &Provider{settings: Settings{Project: "p"}}
	ty := &catalog.Type{
		Name: "gcp.sqladmin.instance", Await: catalog.AwaitComputeOperation,
		APIBaseURL:        "https://sqladmin.googleapis.com/",
		PathPrefix:        "v1/",
		OperationPollPath: "v1/projects/{project}/operations/{operation}",
	}
	_, url, err := p.operationRequestURL(ty, map[string]any{"name": "op-1", "status": "RUNNING"})
	if err != nil {
		t.Fatal(err)
	}
	if want := "https://sqladmin.googleapis.com/v1/projects/p/operations/op-1"; url != want {
		t.Errorf("poll url = %q, want %q", url, want)
	}
}

// TestEveryOperationURLSatisfiesTheAPIsOwnParameterPattern is the strongest
// form of the arm above: rather than this test's own idea of what addresses
// an operation, it asserts against the `pattern` the Discovery document
// publishes for the operations method's own parameters -- the API's own
// statement of what it accepts. container's
// projects.locations.operations.get declares
// "^projects/[^/]+/locations/[^/]+/operations/[^/]+$" for "name", which no
// bare operation id can ever satisfy; compute declares one for each of
// project, zone, region and operation.
//
// COMPUTE-STYLE TYPES ONLY, and that is not a convenience. What fills a
// compute-style poll url is the provider's own decision (operationRequestURL
// picks between the operation's id and its reduced selfLink), so a pattern is
// a real check on a real choice. A longrunning poll url is filled with
// op["name"] exactly as GCP sent it, so the only value a test could put there
// is one it invented, and the pattern would be testing the fixture. Measured
// while writing this: the invented name fails 40-odd longrunning types whose
// APIs name operations "operations/...", "organizations/.../operations/..."
// or "locations/.../workforcePools/.../operations/..." -- every one of them a
// statement about the fixture and none about the runtime.
//
// schemas/ is fetched by scripts/fetch-schemas and deliberately not
// committed, so this skips when it is absent rather than asserting nothing
// quietly -- the same treatment internal/disco/smoke_test.go gives it. The
// structural arm above needs no schemas and always runs.
func TestEveryOperationURLSatisfiesTheAPIsOwnParameterPattern(t *testing.T) {
	const schemaDir = "../../schemas"
	if _, err := os.Stat(schemaDir); err != nil {
		t.Skipf("%s absent (run scripts/fetch-schemas); the APIs' own parameter patterns cannot be read", schemaDir)
	}
	c := mustCatalog(t)
	p := &Provider{settings: Settings{Project: "p", Zone: "z", Region: "r"}}
	docs := map[string]map[string]*discoveryResource{}
	var checked, checkedAPathCapture int
	for _, ty := range c.Types {
		if ty.Await != catalog.AwaitComputeOperation {
			continue
		}
		tmpl := ty.OperationWaitPath
		if tmpl == "" {
			tmpl = ty.OperationPollPath
		}
		if tmpl == "" {
			continue
		}
		rel, err := pollPathTheRuntimeWouldRequest(p, ty)
		if err != nil {
			continue // already reported by the structural arm
		}
		values, err := valuesSubstitutedInto(tmpl, rel)
		if err != nil {
			t.Errorf("%s: %v", ty.Name, err)
			continue
		}
		res, ok := docs[ty.Service]
		if !ok {
			res = loadDiscoveryResources(t, filepath.Join(schemaDir, ty.Service+".json"))
			docs[ty.Service] = res
		}
		for name, pattern := range operationParameterPatterns(res, tmpl) {
			value, ok := values[name]
			if !ok || pattern == "" {
				continue
			}
			// Discovery patterns are full-match rules; several of compute's
			// are written without anchors, so anchoring here is what makes
			// "op-1" tested against the whole pattern rather than found
			// somewhere inside it.
			re, err := regexp.Compile("^(?:" + pattern + ")$")
			if err != nil {
				t.Errorf("%s: %s publishes a pattern Go cannot compile (%q): %v", ty.Name, tmpl, pattern, err)
				continue
			}
			checked++
			if strings.Contains(tmpl, "{+") {
				checkedAPathCapture++
			}
			if !re.MatchString(value) {
				t.Errorf("%s: polls %q, but %s's own %q parameter accepts only %q, which %q is not",
					ty.Name, rel, tmpl, name, pattern, value)
			}
		}
	}
	if checked == 0 {
		t.Error("not one operation parameter pattern was checked, so this test asserts nothing")
	}
	// The whole-path captures are the ones that can address the wrong thing
	// while still expanding, so a run that checked only compute's single
	// segments has not covered the case this test exists for.
	if checkedAPathCapture == 0 {
		t.Error("no type polling a whole-path \"{+name}\" capture was checked, which is the shape " +
			"container's two types have and the shape the defect lived in")
	}
}

// discoveryResource is the sliver of a Discovery document this test needs:
// which methods a resource publishes, what path each takes, and the pattern
// each of its parameters declares. internal/disco does not read patterns
// (nothing in the generator needs them), and teaching it to for a test's
// sake would put a field in the production parser that only a test reads.
type discoveryResource struct {
	Methods map[string]struct {
		Path       string `json:"path"`
		Parameters map[string]struct {
			Pattern string `json:"pattern"`
		} `json:"parameters"`
	} `json:"methods"`
	Resources map[string]*discoveryResource `json:"resources"`
}

func loadDiscoveryResources(t *testing.T, path string) map[string]*discoveryResource {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		// A service whose document is not on disk simply cannot be checked;
		// every one that is still is.
		return nil
	}
	var doc struct {
		Resources map[string]*discoveryResource `json:"resources"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	return doc.Resources
}

// operationParameterPatterns returns the patterns declared for the
// parameters of the operations method whose path is tmpl.
//
// It looks only inside a resource that IS an operations collection -- named
// "operations", or compute's scope-specific "globalOperations" /
// "regionOperations" / "zoneOperations" -- which is how the generator picks
// these paths in the first place (internal/gen/build.go). That restriction
// is not cosmetic: container publishes "v1/{+name}" as the path of seven
// different methods, and clusters.get's "name" pattern demands
// ".../clusters/..." where operations.get's demands ".../operations/...".
// Matching on the path alone would test the poll url against the wrong
// resource's rule.
func operationParameterPatterns(res map[string]*discoveryResource, tmpl string) map[string]string {
	found := map[string]map[string]bool{}
	var walk func(map[string]*discoveryResource)
	walk = func(res map[string]*discoveryResource) {
		for name, r := range res {
			if name == "operations" || strings.HasSuffix(name, "Operations") {
				for _, m := range r.Methods {
					if m.Path != tmpl {
						continue
					}
					for param, spec := range m.Parameters {
						if spec.Pattern == "" {
							continue
						}
						if found[param] == nil {
							found[param] = map[string]bool{}
						}
						found[param][spec.Pattern] = true
					}
				}
			}
			walk(r.Resources)
		}
	}
	walk(res)
	out := map[string]string{}
	for param, patterns := range found {
		// One API can publish several operations collections at the same
		// path -- iam has four, all "v1/{+name}", each with its own pattern.
		// Which one a given type's operations live in is not decidable from
		// the path, and asserting against whichever was walked last would be
		// a coin toss, so an ambiguous parameter is left unchecked.
		if len(patterns) != 1 {
			continue
		}
		for pattern := range patterns {
			out[param] = pattern
		}
	}
	return out
}

// valuesSubstitutedInto recovers what each of tmpl's placeholders was filled
// with, by matching the expanded path back against the template. Reading the
// values out of the result is what keeps this test honest: it asserts on
// what the runtime actually substituted, not on a second copy of the
// runtime's own choices.
func valuesSubstitutedInto(tmpl, expanded string) (map[string]string, error) {
	var pattern strings.Builder
	var names []string
	pattern.WriteString("^")
	i := 0
	for i < len(tmpl) {
		if tmpl[i] != '{' {
			pattern.WriteString(regexp.QuoteMeta(string(tmpl[i])))
			i++
			continue
		}
		openLen, closeSeq := 1, "}"
		if i+1 < len(tmpl) && tmpl[i+1] == '{' {
			openLen, closeSeq = 2, "}}"
		}
		rel := strings.Index(tmpl[i+openLen:], closeSeq)
		if rel < 0 {
			return nil, fmt.Errorf("url template %q has an unterminated placeholder", tmpl)
		}
		content := strings.TrimSpace(tmpl[i+openLen : i+openLen+rel])
		names = append(names, strings.TrimPrefix(content, "+"))
		if strings.HasPrefix(content, "+") {
			pattern.WriteString("(.+)") // reserved expansion: a whole path
		} else {
			pattern.WriteString("([^/]+)")
		}
		i += openLen + rel + len(closeSeq)
	}
	pattern.WriteString("$")
	m := regexp.MustCompile(pattern.String()).FindStringSubmatch(expanded)
	if m == nil {
		return nil, fmt.Errorf("%q is not %q filled in, so nothing can be read back out of it", expanded, tmpl)
	}
	out := make(map[string]string, len(names))
	for i, name := range names {
		out[name] = m[i+1]
	}
	return out, nil
}
