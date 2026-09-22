// Package gcpfake is an in-process GCP: an httptest.Server speaking GCP's
// real JSON REST wire format, not a stubbed client interface.
//
// Fake the cloud, not the code (spec §8). Every test that uses this package
// goes through the real URL building, JSON encoding, error decoding, await
// loop and backoff of internal/gcprov, exactly as it would against Google.
// The real GCP API is the oracle for its wire format: an error body is
// nested under "error", and a mutation's response shape depends on which of
// three strategies the type uses (AwaitNone, AwaitLongRunning,
// AwaitComputeOperation — see operations.go).
//
// The fake is deterministic and never sleeps for real time: an operation
// completes on its first poll unless a test explicitly asks it not to
// (NeverCompleteOperations), so the await loop is exercised without making
// the suite slow.
package gcpfake

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// pageSize is fixed rather than configurable: paginating at 2 items means
// every list-shaped test exercises pageToken/nextPageToken, rather than
// accidentally fitting in one page and never testing the follow-up call.
const pageSize = 2

// apiErrorSpec is an injected or configured failure, shaped like the wire
// error it will become (see writeError).
type apiErrorSpec struct {
	Status  int
	Code    string
	Message string
}

// Request is one HTTP request the fake actually received, recorded
// regardless of how it was answered (including an injected failure), so a
// test can assert on what the client under test really sent.
type Request struct {
	Method string
	Path   string
	Query  url.Values
	Body   []byte
}

// Server is the fake. Zero value is not usable; construct with New.
type Server struct {
	t   testing.TB
	srv *httptest.Server

	mu sync.Mutex

	// resources holds every stored resource, keyed by the exact request path
	// it lives at (e.g. "/v1/projects/p/locations/r/widgets/one"), matching
	// exactly what Seed and Get take. Keying by the literal path rather than
	// trying to parse GCP's URL templates is what lets one fake serve every
	// API's shape without knowing any of them in advance.
	resources map[string]map[string]any
	// notFoundRemaining forces the next N gets of a path to 404 regardless of
	// what is stored there, for NotFoundTimes.
	notFoundRemaining map[string]int

	opStyle   OperationStyle
	opCounter int

	lroOps         map[string]*lroOp
	computeOps     map[string]*computeOp
	neverComplete  bool
	createThenFail map[string]apiErrorSpec

	failNext *apiErrorSpec
	requests []Request

	caiAssets map[string][]Asset
	caiFail   *apiErrorSpec

	tagBindings map[string][]map[string]any

	// listFields overrides the array-valued field name a GET on a specific
	// collection path uses in its list response, keyed by that exact
	// collection path. See SetListField.
	listFields map[string]string
}

// New starts the fake. The caller must Close it.
func New(t *testing.T) *Server {
	t.Helper()
	s := &Server{
		t:                 t,
		resources:         map[string]map[string]any{},
		notFoundRemaining: map[string]int{},
		lroOps:            map[string]*lroOp{},
		computeOps:        map[string]*computeOp{},
		createThenFail:    map[string]apiErrorSpec{},
		caiAssets:         map[string][]Asset{},
		tagBindings:       map[string][]map[string]any{},
		listFields:        map[string]string{},
	}
	s.srv = httptest.NewServer(s)
	return s
}

// URL is the fake's base URL, e.g. "http://127.0.0.1:PORT".
func (s *Server) URL() string { return s.srv.URL }

// Close shuts the fake down.
func (s *Server) Close() { s.srv.Close() }

// Seed puts a resource directly at path, bypassing create. path is the full
// request path a GET for it would use, e.g.
// "/v1/projects/p/locations/r/widgets/one".
func (s *Server) Seed(path string, body map[string]any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.resources[path] = cloneMap(body)
}

// Get reads a resource back by the same path Seed or a client's create would
// have stored it at.
func (s *Server) Get(path string) (map[string]any, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.resources[path]
	if !ok {
		return nil, false
	}
	return cloneMap(v), true
}

// SetOperationStyle picks which of the two asynchronous wire shapes
// mutations respond with. The zero value, OpSync, returns the resource (or,
// for a delete, an empty object) directly — the 187-of-676-method
// synchronous case (spec §5.3) needs no operation at all.
func (s *Server) SetOperationStyle(style OperationStyle) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.opStyle = style
}

// SetListField overrides the array-valued field name a GET on
// collectionPath's list response carries its results under. Defaults to
// "items" (compute's own convention) when never set for that path.
//
// There is no field name universal across GCP's own List responses to
// assume instead — measured 209 distinct names across 532 sampled List
// methods, and "items" covers only 24% of them (see catalog.Type.ListField,
// which the real generator derives per type from that type's own Discovery
// schema). A test exercising a type whose real API answers with a different
// name (e.g. "buckets", "savedQueries") must set it here to match.
func (s *Server) SetListField(collectionPath, field string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.listFields[collectionPath] = field
}

func (s *Server) listFieldFor(path string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if f, ok := s.listFields[path]; ok && f != "" {
		return f
	}
	return "items"
}

// FailNext makes the very next request, of any method or path, fail with
// this GCP-shaped error. Consumed after one use.
func (s *Server) FailNext(status int, code, message string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failNext = &apiErrorSpec{Status: status, Code: code, Message: message}
}

// NotFoundTimes makes the next n GETs of path answer 404 NOT_FOUND, even
// though a resource may already be stored there — modelling the eventual
// consistency window a just-created resource can 404 through.
func (s *Server) NotFoundTimes(path string, n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.notFoundRemaining[path] = n
}

// Requests is every request the fake has received, in order, including ones
// answered with an injected failure.
func (s *Server) Requests() []Request {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Request, len(s.requests))
	copy(out, s.requests)
	return out
}

// ServeHTTP routes a request to one of: an injected failure, Cloud Asset
// Inventory search, a compute-style operation wait, a tag-bindings list, a
// longrunning-operation poll, or ordinary CRUD — in that order, because each
// of the special shapes is a POST or GET that would otherwise be
// indistinguishable from ordinary CRUD on a path the fake knows nothing
// about.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	bodyBytes, _ := io.ReadAll(r.Body)
	r.Body.Close()

	s.mu.Lock()
	s.requests = append(s.requests, Request{
		Method: r.Method, Path: r.URL.Path, Query: r.URL.Query(), Body: bodyBytes,
	})
	failed := s.failNext
	s.failNext = nil
	s.mu.Unlock()
	if failed != nil {
		writeError(w, failed.Status, failed.Code, failed.Message)
		return
	}

	switch {
	case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, ":searchAllResources"):
		s.handleSearchAllResources(w, r)
	case r.Method == http.MethodPost && waitPathRE.MatchString(r.URL.Path):
		s.handleComputeWait(w, r)
	case r.Method == http.MethodGet && strings.HasSuffix(trimVersionPrefix(r.URL.Path), "tagBindings"):
		s.handleListTagBindings(w, r)
	case r.Method == http.MethodGet:
		if name, ok := s.operationName(r.URL.Path); ok {
			s.handleGetOperation(w, name)
			return
		}
		s.handleGet(w, r)
	case r.Method == http.MethodPost:
		s.handleCreate(w, r, bodyBytes)
	case r.Method == http.MethodPatch:
		s.handlePatch(w, r, bodyBytes)
	case r.Method == http.MethodDelete:
		s.handleDelete(w, r)
	default:
		writeError(w, http.StatusMethodNotAllowed, "UNIMPLEMENTED", fmt.Sprintf("the fake does not support %s", r.Method))
	}
}

// handleGet answers a GET, first deciding whether the path names a
// collection (a list call, however many — zero included — resources it
// currently holds) or a specific resource (a get call, which 404s if
// nothing is there). See isCollectionPath for how that's decided: GCP's own
// URL templates alternate a literal collection segment with a variable id
// segment starting from a literal, so the two cases are never actually
// ambiguous from the path alone, and the fake does not need history (what
// was previously created or seeded) to tell them apart. This matters
// specifically because Task 16's discovery fallback scans every type across
// a project and will genuinely find nothing for most of them — those must
// list empty (200), not 404, or the fallback cannot tell "nothing here" from
// "this API doesn't exist."
func (s *Server) handleGet(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path

	if isCollectionPath(path) {
		items := s.collectionItems(path)
		field := s.listFieldFor(path)
		page, next := paginate(items, r.URL.Query())
		out := map[string]any{field: page}
		if next != "" {
			out["nextPageToken"] = next
		}
		writeJSON(w, http.StatusOK, out)
		return
	}

	s.mu.Lock()
	if n := s.notFoundRemaining[path]; n > 0 {
		s.notFoundRemaining[path] = n - 1
		s.mu.Unlock()
		writeNotFound(w, path)
		return
	}
	body, ok := s.resources[path]
	if ok {
		body = cloneMap(body)
	}
	s.mu.Unlock()
	if !ok {
		writeNotFound(w, path)
		return
	}
	writeJSON(w, http.StatusOK, body)
}

// isCollectionPath decides list-vs-get from path shape alone, no history
// needed: every GCP REST URL template this catalog uses alternates a
// literal collection segment with a variable id segment, always starting
// with a literal ("projects/{project}/locations/{location}/widgets" or
// "projects/{project}/locations/{location}/widgets/{id}"). Stripping the
// version prefix, a list call therefore always lands on an ODD number of
// remaining segments (ending on a literal) and a get call on an EVEN number
// (ending on the id). This holds regardless of how many resources are
// currently stored at or under the path, which is exactly the property
// needed to answer "list of an empty collection" and "get of a specific
// absent resource" differently.
func isCollectionPath(path string) bool {
	rel := trimVersionPrefix(path)
	if rel == "" {
		return false
	}
	return len(strings.Split(rel, "/"))%2 == 1
}

// collectionItems finds every resource stored directly under path (one path
// segment deeper, not further nested). Always returns a non-nil, possibly
// empty slice: a collection with nothing stored under it is a real, valid
// state (an empty list), not an absent one.
func (s *Server) collectionItems(path string) []map[string]any {
	prefix := path + "/"
	s.mu.Lock()
	defer s.mu.Unlock()
	var keys []string
	for k := range s.resources {
		if strings.HasPrefix(k, prefix) && !strings.Contains(strings.TrimPrefix(k, prefix), "/") {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	items := make([]map[string]any, len(keys))
	for i, k := range keys {
		items[i] = cloneMap(s.resources[k])
	}
	return items
}

func (s *Server) handleCreate(w http.ResponseWriter, r *http.Request, bodyBytes []byte) {
	body := map[string]any{}
	if len(bodyBytes) > 0 {
		if err := json.Unmarshal(bodyBytes, &body); err != nil {
			writeError(w, http.StatusBadRequest, "INVALID_ARGUMENT", "malformed JSON body")
			return
		}
	}
	id := resourceID(r, body)
	if id == "" {
		writeError(w, http.StatusBadRequest, "INVALID_ARGUMENT", "no id in the request's query parameters or body")
		return
	}
	path := strings.TrimSuffix(r.URL.Path, "/") + "/" + id
	stored := cloneMap(body)

	s.mu.Lock()
	s.resources[path] = stored
	s.mu.Unlock()

	s.respondMutation(w, path, stored, false)
}

// resourceID takes the id GCP's insert methods take it from: a query
// parameter (e.g. "widgetId", "instanceId" — the name varies by API, so any
// non-reserved parameter is accepted) or, failing that, the last segment of
// the body's own "name".
func resourceID(r *http.Request, body map[string]any) string {
	reserved := map[string]bool{"updateMask": true, "pageToken": true, "pageSize": true, "parent": true, "requestId": true}
	for k, v := range r.URL.Query() {
		if reserved[k] || len(v) == 0 || v[0] == "" {
			continue
		}
		return v[0]
	}
	if name, ok := body["name"].(string); ok && name != "" {
		if i := strings.LastIndex(name, "/"); i >= 0 {
			return name[i+1:]
		}
		return name
	}
	return ""
}

func (s *Server) handlePatch(w http.ResponseWriter, r *http.Request, bodyBytes []byte) {
	path := r.URL.Path
	s.mu.Lock()
	existing, ok := s.resources[path]
	s.mu.Unlock()
	if !ok {
		writeNotFound(w, path)
		return
	}

	patchBody := map[string]any{}
	if len(bodyBytes) > 0 {
		if err := json.Unmarshal(bodyBytes, &patchBody); err != nil {
			writeError(w, http.StatusBadRequest, "INVALID_ARGUMENT", "malformed JSON body")
			return
		}
	}

	var mask []string
	if m := r.URL.Query().Get("updateMask"); m != "" {
		mask = strings.Split(m, ",")
	}
	// A mask naming a field the body does not carry is exactly the bug a mask
	// built from the wrong side of a diff produces (Task 14 depends on this
	// failing rather than silently succeeding).
	for _, m := range mask {
		if _, present := lookupDotted(patchBody, m); !present {
			writeError(w, http.StatusBadRequest, "INVALID_ARGUMENT",
				fmt.Sprintf("updateMask names %q, which is not present in the request body", m))
			return
		}
	}

	merged := cloneMap(existing)
	if len(mask) == 0 {
		for k, v := range patchBody {
			merged[k] = v
		}
	} else {
		for _, m := range mask {
			v, _ := lookupDotted(patchBody, m)
			setDotted(merged, m, v)
		}
	}

	s.mu.Lock()
	s.resources[path] = merged
	s.mu.Unlock()

	s.respondMutation(w, path, merged, false)
}

func (s *Server) handleDelete(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	s.mu.Lock()
	_, existed := s.resources[path]
	delete(s.resources, path)
	s.mu.Unlock()
	if !existed {
		// A real delete of something already gone is a 404, same as a get.
		// Making destroy idempotent in spite of that is internal/gcprov's job
		// (Task 13), not something the fake should hide by pretending to
		// succeed.
		writeNotFound(w, path)
		return
	}
	s.respondMutation(w, path, nil, true)
}

// respondMutation writes a create/patch/delete's response in whatever shape
// the configured OperationStyle calls for.
func (s *Server) respondMutation(w http.ResponseWriter, path string, result map[string]any, isDelete bool) {
	s.mu.Lock()
	style := s.opStyle
	s.mu.Unlock()

	switch style {
	case OpLongRunning:
		writeJSON(w, http.StatusOK, s.newLongRunningOp(path, result, isDelete))
	case OpCompute:
		writeJSON(w, http.StatusOK, s.newComputeOp(path, result, isDelete))
	default:
		if isDelete {
			writeJSON(w, http.StatusOK, map[string]any{})
			return
		}
		writeJSON(w, http.StatusOK, result)
	}
}

// waitPathRE matches a compute-style operations-wait call, e.g.
// ".../zones/us-central1-a/zoneOperations/op-1/wait". Only the scope
// collection and the operation id are pulled out; the rest of the path
// (project, region/zone) is never inspected because the fake does not need
// it to answer.
var waitPathRE = regexp.MustCompile(`/(globalOperations|regionOperations|zoneOperations)/([^/]+)/wait$`)

// operationName reports whether path names a registered longrunning
// operation, and if so its bare name (e.g. "operations/abc") — the same
// string CompleteOperationWithError and an operation's own "name" field use.
// This is checked by membership, not by pattern-matching the path, so it
// works regardless of which API version segment ("v1", "v3", ...) prefixes
// it.
func (s *Server) operationName(path string) (string, bool) {
	candidate := trimVersionPrefix(path)
	if candidate == "" {
		return "", false
	}
	s.mu.Lock()
	_, ok := s.lroOps[candidate]
	s.mu.Unlock()
	return candidate, ok
}

// trimVersionPrefix drops the leading "/<version>/" segment (whatever the
// version is — v1, v3, ...), leaving the API-relative path. Used wherever
// the fake compares a request path against a bare name or parent it was
// given without a version prefix (operation names, CAI parents, tag-binding
// collections).
func trimVersionPrefix(path string) string {
	trimmed := strings.TrimPrefix(path, "/")
	parts := strings.SplitN(trimmed, "/", 2)
	if len(parts) != 2 {
		return ""
	}
	return parts[1]
}

// paginate slices items at pageSize, honouring an incoming pageToken (the
// string offset a previous call's nextPageToken carried) and reporting the
// next one. Shared by resource lists, CAI search and tag-binding lists so
// all three paginate identically.
func paginate[T any](items []T, q url.Values) (page []T, nextToken string) {
	offset := 0
	if tok := q.Get("pageToken"); tok != "" {
		if n, err := strconv.Atoi(tok); err == nil && n >= 0 {
			offset = n
		}
	}
	if offset > len(items) {
		offset = len(items)
	}
	end := offset + pageSize
	if end > len(items) {
		end = len(items)
	}
	if end < len(items) {
		nextToken = strconv.Itoa(end)
	}
	return items[offset:end], nextToken
}

// lookupDotted walks a dotted path ("config.mode") through nested
// JSON-decoded maps, reporting whether the leaf is present.
func lookupDotted(m map[string]any, dotted string) (any, bool) {
	cur := any(m)
	for _, p := range strings.Split(dotted, ".") {
		cm, ok := cur.(map[string]any)
		if !ok {
			return nil, false
		}
		v, ok := cm[p]
		if !ok {
			return nil, false
		}
		cur = v
	}
	return cur, true
}

// setDotted writes v at a dotted path inside m, creating intermediate maps
// as needed.
func setDotted(m map[string]any, dotted string, v any) {
	parts := strings.Split(dotted, ".")
	cur := m
	for i, p := range parts {
		if i == len(parts)-1 {
			cur[p] = v
			return
		}
		next, ok := cur[p].(map[string]any)
		if !ok {
			next = map[string]any{}
			cur[p] = next
		}
		cur = next
	}
}

// cloneMap deep-copies a JSON-shaped map via a marshal/unmarshal round trip,
// so a caller mutating a body it got from Get (or that the fake stored from
// a request) can never alias state the fake itself still holds.
func cloneMap(m map[string]any) map[string]any {
	if m == nil {
		return map[string]any{}
	}
	b, err := json.Marshal(m)
	if err != nil {
		// Every value that reaches this function came from decoding JSON in
		// the first place, so re-marshaling it cannot fail; if it ever does,
		// the fake itself is broken and should say so loudly rather than
		// silently aliasing state between requests.
		panic(fmt.Sprintf("gcpfake: cloning a resource body: %v", err))
	}
	var out map[string]any
	if err := json.Unmarshal(b, &out); err != nil {
		panic(fmt.Sprintf("gcpfake: cloning a resource body: %v", err))
	}
	return out
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(body)
}

// writeError writes GCP's real error shape: the code, status enum and
// message nested under "error", never flattened.
func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, map[string]any{
		"error": map[string]any{"code": status, "status": code, "message": message},
	})
}

func writeNotFound(w http.ResponseWriter, path string) {
	writeError(w, http.StatusNotFound, "NOT_FOUND", fmt.Sprintf("%s not found", path))
}
