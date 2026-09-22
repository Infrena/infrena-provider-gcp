package gcpfake

import (
	"fmt"
	"net/http"
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
	// OpCompute: compute-style {"name", "status": "RUNNING"|"DONE"}, polled
	// via the long-poll wait method on the type's operation scope
	// (globalOperations, regionOperations or zoneOperations).
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

// CompleteOperationWithError registers a longrunning operation by name,
// without requiring a preceding create, so a test can poll it directly. It
// completes with GCP's error shape on its first poll (or never, if
// NeverCompleteOperations was called first).
func (s *Server) CompleteOperationWithError(name, code, message string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lroOps[name] = &lroOp{name: name, errCode: code, errMsg: message}
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
// operation. An operation the fake never created via a mutation (a test may
// hand-build one, the way await_test.go's compute test does) is
// auto-vivified as one that completes successfully on this call, because the
// fake's job is to answer the wait, not to insist the operation was born
// through its own POST handler.
func (s *Server) handleComputeWait(w http.ResponseWriter, r *http.Request) {
	m := waitPathRE.FindStringSubmatch(r.URL.Path)
	if m == nil {
		writeNotFound(w, r.URL.Path)
		return
	}
	opName := m[2]

	s.mu.Lock()
	op, ok := s.computeOps[opName]
	if !ok {
		op = &computeOp{name: opName}
		s.computeOps[opName] = op
	}
	never := s.neverComplete
	s.mu.Unlock()

	if never {
		writeJSON(w, http.StatusOK, map[string]any{"name": opName, "status": "RUNNING"})
		return
	}

	body := map[string]any{"name": opName, "status": "DONE"}
	if op.errCode != "" {
		body["error"] = map[string]any{"errors": []map[string]any{
			{"code": op.errCode, "message": op.errMsg},
		}}
	}
	writeJSON(w, http.StatusOK, body)
}
