package gcpfake

import (
	"fmt"
	"net/http"
	"strings"
)

// OperationStyle picks which of GCP's two asynchronous mutation shapes a
// fake serves. A 25-API sample (spec §5.3) found exactly these two plus a
// synchronous case that needs no operation at all — OpSync, the zero value.
type OperationStyle int

const (
	// OpSync: a mutation's response IS the resulting resource (or, for a
	// delete, an empty object). No operation is ever created.
	OpSync OperationStyle = iota
	// OpLongRunning: google.longrunning.Operation — {"name", "done": false}
	// polled at GET <api>/v1/<name> until {"done": true, "response": {...}}
	// or {"done": true, "error": {...}}.
	OpLongRunning
	// OpCompute: compute-style {"name", "status": "RUNNING"|"DONE"}. Polled
	// via the long-poll wait method (POST .../operations/<op>/wait) for the
	// 58 of 65 compute-style types whose API publishes one; the other 7
	// (container, sqladmin) publish none and are polled with an ordinary GET
	// instead, same body shape.
	OpCompute
)

// lroOp is one google.longrunning.Operation the fake is tracking.
type lroOp struct {
	name     string
	response map[string]any // nil for a delete, or when it hasn't finished
	errCode  string         // empty means "completes successfully"
	errMsg   string
}

// computeOp is one compute-style operation the fake is tracking.
type computeOp struct {
	name     string
	response map[string]any
	errCode  string
	errMsg   string
}

// grpcCodes maps google.rpc.Code's string names to their wire integers, so
// a longrunning operation's error carries the same numeric code the real
// API would send rather than a fake-only string. See
// https://github.com/googleapis/googleapis/blob/master/google/rpc/code.proto.
var grpcCodes = map[string]int{
	"OK": 0, "CANCELLED": 1, "UNKNOWN": 2, "INVALID_ARGUMENT": 3, "DEADLINE_EXCEEDED": 4,
	"NOT_FOUND": 5, "ALREADY_EXISTS": 6, "PERMISSION_DENIED": 7, "UNAUTHENTICATED": 16,
	"RESOURCE_EXHAUSTED": 8, "FAILED_PRECONDITION": 9, "ABORTED": 10, "OUT_OF_RANGE": 11,
	"UNIMPLEMENTED": 12, "INTERNAL": 13, "UNAVAILABLE": 14, "DATA_LOSS": 15,
}

func grpcCode(status string) int {
	if c, ok := grpcCodes[status]; ok {
		return c
	}
	return grpcCodes["UNKNOWN"]
}

// SeedOperation registers a longrunning operation by name with the response
// it will complete with, without requiring a preceding create -- the
// longrunning mirror of Seed. A test may use a realistic full relative name
// (e.g. "projects/p/locations/l/operations/op-1"), not only the bare
// "operations/op-N" shape the fake's own newLongRunningOp mints: a fake
// requiring the test to say what exists, rather than a routing heuristic
// guessing it from the polled url's shape, is honest about what a fake can
// know (the same principle server.go's declared-collections doc comment
// states for ordinary resources). It completes on its first poll unless
// NeverCompleteOperations was called first, in which case it stays
// unfinished forever -- combine the two to exercise a genuine timeout rather
// than a poll that merely 404s.
func (s *Server) SeedOperation(name string, response map[string]any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lroOps[name] = &lroOp{name: name, response: cloneMap(response)}
}

// CompleteOperationWithError registers a longrunning operation by name,
// without requiring a preceding create, so a test can poll it directly. It
// completes with GCP's error shape on its first poll (or never, if
// NeverCompleteOperations was called first).
func (s *Server) CompleteOperationWithError(name, code, message string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lroOps[name] = &lroOp{name: name, errCode: code, errMsg: message}
}

// SeedComputeOperation registers a compute-style operation by name, without
// requiring a preceding create -- the compute mirror of SeedOperation. A
// completed compute operation's own body never carries a "response" (unlike
// longrunning, compute reports completion by status alone; the caller
// re-reads the resource), so unlike SeedOperation this takes no response
// parameter. It completes on its first wait unless NeverCompleteOperations
// was called first.
func (s *Server) SeedComputeOperation(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.computeOps[name] = &computeOp{name: name}
}

// NeverCompleteOperations makes every operation — longrunning or compute
// style, already created or created from now on — stay unfinished on every
// poll for the rest of the server's life. It exists for exactly one thing:
// proving an await loop stops at its type's timeout instead of polling
// forever.
func (s *Server) NeverCompleteOperations() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.neverComplete = true
}

// CreateThenFailOperation is the realistic bad case: the resource really
// gets created (a subsequent Get finds it), but the operation that reports
// on the mutation completes with an error — e.g. a post-create configuration
// step failing. It applies to every create/update/delete the fake serves at
// path for the rest of the server's life.
func (s *Server) CreateThenFailOperation(path, code, message string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.createThenFail[path] = apiErrorSpec{Code: code, Message: message}
}

// newLongRunningOp records a fresh operation for a just-applied mutation and
// returns its initial "done": false body.
func (s *Server) newLongRunningOp(path string, result map[string]any, isDelete bool) map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.opCounter++
	name := fmt.Sprintf("operations/op-%d", s.opCounter)
	op := &lroOp{name: name}
	if !isDelete {
		op.response = cloneMap(result)
	}
	if spec, ok := s.createThenFail[path]; ok {
		op.errCode, op.errMsg = spec.Code, spec.Message
	}
	s.lroOps[name] = op
	return map[string]any{"name": name, "done": false}
}

// handleGetOperation answers a poll of a longrunning operation: unfinished
// forever under NeverCompleteOperations, otherwise done on this very call.
func (s *Server) handleGetOperation(w http.ResponseWriter, name string) {
	s.mu.Lock()
	op, ok := s.lroOps[name]
	never := s.neverComplete
	s.mu.Unlock()
	if !ok {
		writeNotFound(w, name)
		return
	}
	if never {
		writeJSON(w, http.StatusOK, map[string]any{"name": name, "done": false})
		return
	}

	body := map[string]any{"name": name, "done": true}
	if op.errCode != "" {
		// google.rpc.Status: a numeric code and a message, not GCP's HTTP-level
		// {code,status,message} shape — this is a different envelope, nested one
		// level deeper inside a completed Operation.
		body["error"] = map[string]any{"code": grpcCode(op.errCode), "message": op.errMsg}
	} else if op.response != nil {
		body["response"] = op.response
	}
	writeJSON(w, http.StatusOK, body)
}

// newComputeOp records a fresh compute-style operation and returns its
// initial "status": "RUNNING" body.
func (s *Server) newComputeOp(path string, result map[string]any, isDelete bool) map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.opCounter++
	name := fmt.Sprintf("op-%d", s.opCounter)
	op := &computeOp{name: name}
	if !isDelete {
		op.response = cloneMap(result)
	}
	if spec, ok := s.createThenFail[path]; ok {
		op.errCode, op.errMsg = spec.Code, spec.Message
	}
	s.computeOps[name] = op
	return map[string]any{"name": name, "status": "RUNNING", "targetLink": path}
}

// handleComputeWait answers the long-poll wait method for a compute-style
// operation. An operation name the fake has never seen -- not created via a
// mutation, not registered with SeedComputeOperation -- 404s, the same as
// handleGetOperation does for an unknown longrunning name. An earlier
// version of this auto-vivified any unknown name as one that completes
// successfully, on the reasoning that the fake's job is to answer the wait,
// not to insist the operation was born through its own POST handler. That
// reasoning does not survive contact with this package's own
// declared-collections principle (server.go): inferring "this operation
// exists" from the shape of a request the fake happens to receive is exactly
// the kind of URL-shape guessing that principle rules out, and it meant a
// client polling a wrong or stale operation name got a false success here
// instead of the real API's 404.
func (s *Server) handleComputeWait(w http.ResponseWriter, r *http.Request) {
	m := waitPathRE.FindStringSubmatch(r.URL.Path)
	if m == nil {
		writeNotFound(w, r.URL.Path)
		return
	}
	opName := m[1]

	s.mu.Lock()
	op, ok := s.computeOps[opName]
	s.mu.Unlock()
	if !ok {
		writeNotFound(w, opName)
		return
	}
	writeJSON(w, http.StatusOK, s.computeOperationBody(opName, op))
}

// computeOperationName reports whether path's last segment names a
// registered compute-style operation -- the GET-poll counterpart of
// operationName, for the 7 of 65 compute-style types (container, sqladmin)
// whose API publishes no wait method at all and must be polled with an
// ordinary GET instead. The operation id is the last path segment because
// that is what both real shapes end in (sqladmin's
// "v1/projects/{project}/operations/{operation}" and container's "v1/{+name}"
// -- container's operation name is itself a path ending in "operations/<id>")
// and what SeedComputeOperation and a mutation's own newComputeOp key
// s.computeOps by.
func (s *Server) computeOperationName(path string) (string, bool) {
	trimmed := strings.TrimSuffix(path, "/")
	i := strings.LastIndex(trimmed, "/")
	if i < 0 || i == len(trimmed)-1 {
		return "", false
	}
	name := trimmed[i+1:]
	s.mu.Lock()
	_, ok := s.computeOps[name]
	s.mu.Unlock()
	return name, ok
}

// handleGetComputeOperation answers a GET poll of a compute-style operation
// whose API has no wait method -- the counterpart of handleGetOperation, for
// the compute case. Same unknown-name handling as handleComputeWait: 404,
// never auto-vivified.
func (s *Server) handleGetComputeOperation(w http.ResponseWriter, name string) {
	s.mu.Lock()
	op, ok := s.computeOps[name]
	s.mu.Unlock()
	if !ok {
		writeNotFound(w, name)
		return
	}
	writeJSON(w, http.StatusOK, s.computeOperationBody(name, op))
}

// computeOperationBody builds the body a compute-style operation reports
// once found, shared by the wait (POST) and poll (GET) paths -- their
// completed and unfinished bodies are identical; only how the caller reaches
// them (a blocking long-poll vs. an ordinary GET) differs, and that
// difference belongs to the type's OperationWaitPath/OperationPollPath
// choice, not to the fake.
func (s *Server) computeOperationBody(name string, op *computeOp) map[string]any {
	s.mu.Lock()
	never := s.neverComplete
	s.mu.Unlock()
	if never {
		return map[string]any{"name": name, "status": "RUNNING"}
	}
	body := map[string]any{"name": name, "status": "DONE"}
	if op.errCode != "" {
		body["error"] = map[string]any{"errors": []map[string]any{
			{"code": op.errCode, "message": op.errMsg},
		}}
	}
	return body
}
