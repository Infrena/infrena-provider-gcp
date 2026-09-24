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

	// fingerprints counts compute-style fingerprints handed out, so each
	// modification of a locked resource gets a new one, deterministically.
	fingerprints int

	// dropOnCreate lists fields an insert ignores, for DropOnCreate.
	dropOnCreate map[string]bool

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
	// opLinkVersion replaces the leading api version segment of a compute
	// operation's own selfLink and targetLink, for AnswerComputeOperationLinksUnder.
	opLinkVersion string
	// opTargetPath replaces the path a compute operation reports as its
	// targetLink, whatever was really created, for
	// AnswerComputeOperationTargetsAt.
	opTargetPath string

	failNext *apiErrorSpec
	requests []Request

	caiAssets map[string][]Asset
	caiFail   *apiErrorSpec

	tagBindings map[string][]map[string]any

	// onRequest holds a one-shot callback for the next request at a given
	// path, consumed on use -- see OnRequest.
	onRequest map[string]func()

	// listFields overrides the array-valued field name a GET on a specific
	// collection path uses in its list response, keyed by that exact
	// collection path. See SetListField.
	listFields map[string]string

	// declaredCollections is the set of paths a GET at them means "list", as
	// opposed to "get one resource" (404 if absent). Path shape alone cannot
	// tell them apart: a bare scope literal like "global" and a resource id
	// are both just opaque path segments, and GCP's own URL templates are not
	// uniform about how many literal segments come before the first
	// variable one — "projects/{project}/global/firewalls" has an unpaired
	// literal, and a handful of types (Cloud Resource Manager's v3 folders
	// among them) embed their own version segment inside their own template
	// on top of the one every request already carries. An earlier version of
	// this fake tried to infer list-vs-get from segment parity and was wrong
	// for about a fifth of the real catalog, silently: a GET of a real,
	// existing resource landed on the list branch and came back 200 with
	// nothing in it. So this is declared, never inferred from the URL:
	// Seed and a create both declare their own collection automatically (see
	// those methods), and DeclareCollection is there for a collection a test
	// wants to exist with nothing seeded in it at all.
	declaredCollections map[string]bool
}

// New starts the fake. The caller must Close it.
func New(t *testing.T) *Server {
	t.Helper()
	s := &Server{
		t:                   t,
		resources:           map[string]map[string]any{},
		notFoundRemaining:   map[string]int{},
		lroOps:              map[string]*lroOp{},
		computeOps:          map[string]*computeOp{},
		createThenFail:      map[string]apiErrorSpec{},
		onRequest:           map[string]func(){},
		caiAssets:           map[string][]Asset{},
		tagBindings:         map[string][]map[string]any{},
		listFields:          map[string]string{},
		declaredCollections: map[string]bool{},
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
// "/v1/projects/p/locations/r/widgets/one". This also declares path's
// parent (".../widgets") a collection, so a GET of it lists — a test never
// has to declare a collection by hand just because it seeded something in
// it.
func (s *Server) Seed(path string, body map[string]any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.resources[path] = cloneMap(body)
	if parent := parentOf(path); parent != "" {
		s.declaredCollections[parent] = true
	}
}

// DeclareCollection marks path as a collection whose GET lists (200,
// possibly with nothing in it), without seeding anything there. Seed and a
// create both declare their own collection automatically; this is for a
// collection a test wants to exist with nothing in it at all — exactly the
// case a discover fallback hits for most types when it scans a project:
// genuinely nothing there, which must come back as an empty list, not a 404.
func (s *Server) DeclareCollection(path string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.declaredCollections[path] = true
}

// parentOf returns path with its last "/"-separated segment removed, or ""
// if path has no parent to speak of.
func parentOf(path string) string {
	i := strings.LastIndex(path, "/")
	if i <= 0 {
		return ""
	}
	return path[:i]
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

// OnRequest arranges for fn to run once, synchronously, the next time a
// request for the exact path arrives -- after the fake has recorded it (so
// Requests() reflects it) but before it is answered. It exists to let a
// test act at a precise moment in an in-flight call, such as cancelling the
// context a client used to reach that request, from the goroutine handling
// it, before the fake's own response is written. Consumed after one use,
// the same shape as FailNext.
func (s *Server) OnRequest(path string, fn func()) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.onRequest[path] = fn
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
	fn := s.onRequest[r.URL.Path]
	if fn != nil {
		delete(s.onRequest, r.URL.Path)
	}
	s.mu.Unlock()
	if failed != nil {
		writeError(w, failed.Status, failed.Code, failed.Message)
		return
	}
	if fn != nil {
		fn()
	}

	switch {
	// Matched on the path alone, whatever the method: cloudasset publishes
	// searchAllResources as a GET (see handleSearchAllResources), and a
	// client that sends it as anything else should get the fake's own answer
	// and be caught by whatever asserts on Requests(), not fall through to
	// ordinary CRUD and come back with an unrelated 404.
	case strings.HasSuffix(r.URL.Path, ":searchAllResources"):
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
		if name, ok := s.computeOperationName(r.URL.Path); ok {
			s.handleGetComputeOperation(w, name)
			return
		}
		s.handleGet(w, r)
	case r.Method == http.MethodPost && s.setterTarget(r.URL.Path) != "":
		s.handleSetter(w, r, bodyBytes)
	case r.Method == http.MethodPost:
		s.handleCreate(w, r, bodyBytes)
	case r.Method == http.MethodPut:
		// Pub/Sub creates with a PUT to the new resource's own path. The
		// resource is stored at exactly that path, and a second PUT to it is
		// the ALREADY_EXISTS the real API answers.
		s.handleCreateAt(w, r, bodyBytes)
	case r.Method == http.MethodPatch:
		s.handlePatch(w, r, bodyBytes)
	case r.Method == http.MethodDelete:
		s.handleDelete(w, r)
	default:
		writeError(w, http.StatusMethodNotAllowed, "UNIMPLEMENTED", fmt.Sprintf("the fake does not support %s", r.Method))
	}
}

// handleGet answers a GET, first checking whether path was DECLARED a
// collection (Seed, a create, or DeclareCollection — never inferred from the
// URL's own shape). A declared path lists, however many — zero included —
// resources it currently holds; anything else is a get of one specific
// resource, 404 if absent.
//
// This used to be inferred from path shape (odd vs even segment count,
// assuming GCP's URL templates always alternate a literal collection segment
// with a variable id segment). That assumption is false for about a fifth of
// the real catalog: "projects/{project}/global/firewalls" has an unpaired
// literal ("global"), and a handful of types (Cloud Resource Manager's v3
// folders among them) embed their own version segment inside their own
// template on top of the one every request already carries. Both failures
// were silent, and the second is the dangerous one: a get of a real,
// existing resource landing on the list branch comes back 200 with nothing
// in it — a successful empty read for something that was seeded moments
// earlier. Declaring rather than inferring is what a fake honestly can know:
// it requires the test to say what exists, rather than guessing from a URL
// whose real templates it does not have access to.
func (s *Server) handleGet(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path

	s.mu.Lock()
	declared := s.declaredCollections[path]
	s.mu.Unlock()

	if declared {
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
	id, fromQuery := resourceID(r, body)
	if id == "" {
		writeError(w, http.StatusBadRequest, "INVALID_ARGUMENT", "no id in the request's query parameters or body")
		return
	}
	collection := strings.TrimSuffix(r.URL.Path, "/")
	path := collection + "/" + id
	stored := cloneMap(body)
	s.mu.Lock()
	for f := range s.dropOnCreate {
		delete(stored, f)
	}
	s.mu.Unlock()
	if fromQuery {
		// AIP-133: a resource created with its id as a query parameter comes
		// back with `name` set to its FULL relative resource name, whatever
		// the request's body said. The fake used to store the body as sent,
		// which carries no name at all for these creates -- so every test
		// saw the short name the id implied, and a type whose schema calls
		// the short name `name` looked as though it round-tripped. Compute's
		// insert, which names the resource in the body and answers with the
		// short name, is untouched.
		stored["name"] = strings.TrimPrefix(trimVersionPrefix(path), "/")
	}

	s.mu.Lock()
	s.resources[path] = stored
	// The URL a create POSTed to is, by definition, a collection — declaring
	// it means a subsequent list of it (even one that finds only what this
	// call just made) never has to be told that by hand.
	s.declaredCollections[collection] = true
	s.mu.Unlock()

	s.respondMutation(w, path, stored, false)
}

// resourceID takes the id GCP's insert methods take it from: a query
// parameter (e.g. "widgetId", "instanceId" — the name varies by API, so any
// non-reserved parameter is accepted) or, failing that, the last segment of
// the body's own "name".
func resourceID(r *http.Request, body map[string]any) (string, bool) {
	reserved := map[string]bool{"updateMask": true, "pageToken": true, "pageSize": true, "parent": true, "requestId": true}
	for k, v := range r.URL.Query() {
		if reserved[k] || len(v) == 0 || v[0] == "" {
			continue
		}
		return v[0], true
	}
	if name, ok := body["name"].(string); ok && name != "" {
		if i := strings.LastIndex(name, "/"); i >= 0 {
			return name[i+1:], false
		}
		return name, false
	}
	return "", false
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
	} else if inner, m, ok := unwrapUpdateEnvelope(patchBody); ok {
		// AIP-134's UpdateXRequest: {"topic": {...}, "updateMask": "a,b"}.
		// The resource is patched from the inner object, under the mask the
		// envelope carries, exactly as a query-string mask would be.
		patchBody, mask = inner, strings.Split(m, ",")
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

	// Compute's optimistic locking. A resource that carries a fingerprint
	// refuses a modification that does not quote the CURRENT one, and gets a
	// new one after every change. The fake did not do this, and 19 patchable
	// compute types went without ever sending one -- every patch to them
	// would have been a 412 from real Google while every test here passed.
	if fp, locked := existing["fingerprint"].(string); locked {
		if sent, _ := patchBody["fingerprint"].(string); sent != fp {
			writeError(w, http.StatusPreconditionFailed, "FAILED_PRECONDITION",
				fmt.Sprintf("conditionNotMet: supplied fingerprint %q does not match current fingerprint %q", sent, fp))
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

	if _, locked := existing["fingerprint"].(string); locked {
		s.mu.Lock()
		s.fingerprints++
		merged["fingerprint"] = fmt.Sprintf("fp-%d", s.fingerprints)
		s.mu.Unlock()
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
// ".../zones/us-central1-a/operations/op-1/wait". The literal wire segment is
// always "operations" -- never "globalOperations"/"regionOperations"/
// "zoneOperations", which are only Discovery's COLLECTION names for the
// three scopes and never appear in a url (confirmed against
// schemas/compute.json: globalOperations' own wait path is
// "projects/{project}/global/operations/{operation}/wait", zoneOperations'
// is "projects/{project}/zones/{zone}/operations/{operation}/wait", etc. --
// "operations" both times). An earlier version of this matched
// "(globalOperations|regionOperations|zoneOperations)" instead, which never
// matched anything a real client would actually send. Only the operation id
// is pulled out; the rest of the path (project, region/zone) is never
// inspected because the fake does not need it to answer.
var waitPathRE = regexp.MustCompile(`/operations/([^/]+)/wait$`)

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

// versionSegmentRE matches a leading path segment that is actually an API
// version -- v1, v2, v3, v1beta1, v2beta, ... -- every form this catalog
// uses. Anything else (e.g. "operations", the first segment of a bare
// "operations/op-1" poll path) is a real path segment and must be kept, not
// discarded on the unconditional assumption that whatever comes first is a
// version. Task 12's await_test.go caught the unconditional version of this:
// every fixture up to Task 10 happened to poll a "/v1/..." path, so the bug
// was invisible to the shapes the suite exercised.
var versionSegmentRE = regexp.MustCompile(`^v[0-9][0-9a-z]*$`)

// trimVersionPrefix drops the leading "/<version>/" segment (whatever the
// version is — v1, v3, v1beta1, ...), leaving the API-relative path, but
// ONLY when that first segment actually looks like a version. A path with no
// version prefix at all (e.g. "/operations/op-1", which the fake's own
// bare-name longrunning operations poll at) is returned unchanged rather
// than having its real first segment silently discarded. Used wherever the
// fake compares a request path against a bare name or parent it was given
// without a version prefix (operation names, CAI parents, tag-binding
// collections).
func trimVersionPrefix(path string) string {
	trimmed := strings.TrimPrefix(path, "/")
	if trimmed == "" {
		return ""
	}
	parts := strings.SplitN(trimmed, "/", 2)
	if len(parts) != 2 {
		// A single segment, no version to trim: e.g. "operations" alone,
		// with nothing after it. Nothing here names an operation or parent by
		// itself, so this deliberately does not fall into the "keep it
		// unchanged" case below -- there IS no remainder to return.
		return ""
	}
	if !versionSegmentRE.MatchString(parts[0]) {
		return trimmed
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

// handleCreateAt is a create addressed to the item's own path rather than to
// its collection -- Pub/Sub's topics, subscriptions and snapshots.
func (s *Server) handleCreateAt(w http.ResponseWriter, r *http.Request, bodyBytes []byte) {
	path := r.URL.Path
	body := map[string]any{}
	if len(bodyBytes) > 0 {
		if err := json.Unmarshal(bodyBytes, &body); err != nil {
			writeError(w, http.StatusBadRequest, "INVALID_ARGUMENT", "malformed JSON body")
			return
		}
	}
	s.mu.Lock()
	_, exists := s.resources[path]
	if !exists {
		body["name"] = strings.TrimPrefix(trimVersionPrefix(path), "/")
		s.resources[path] = body
	}
	s.mu.Unlock()
	if exists {
		writeError(w, http.StatusConflict, "ALREADY_EXISTS", "Resource already exists in the project (resource="+path+").")
		return
	}
	s.respondMutation(w, path, body, false)
}

// unwrapUpdateEnvelope recognises AIP-134's update request: exactly one
// object-valued field (the resource) beside a non-empty field mask named
// updateMask or fieldMask -- the two spellings the pinned documents use. A
// body that is anything else is a bare resource and is left alone.
func unwrapUpdateEnvelope(body map[string]any) (map[string]any, string, bool) {
	var mask string
	var inner map[string]any
	for k, v := range body {
		switch vv := v.(type) {
		case string:
			if k == "updateMask" || k == "fieldMask" {
				mask = vv
				continue
			}
			return nil, "", false
		case map[string]any:
			if inner != nil {
				return nil, "", false
			}
			inner = vv
		default:
			return nil, "", false
		}
	}
	if inner == nil || mask == "" {
		return nil, "", false
	}
	return inner, mask, true
}

// DropOnCreate makes creates ignore the named fields, as compute's inserts
// ignore what only a setter changes: a backend service's securityPolicy, an
// address's labels. The body is stored without them, so the first read after
// the create does not have them.
func (s *Server) DropOnCreate(fields ...string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.dropOnCreate == nil {
		s.dropOnCreate = map[string]bool{}
	}
	for _, f := range fields {
		s.dropOnCreate[f] = true
	}
}

// setterTarget is the stored resource a POST to path calls a method of, or
// "" when path is not "<resource>/<method>". Only method names compute uses
// for setters count ("set..." and "expand..."), so a create that posts to a
// child collection under a stored resource is still a create. Compute's
// global target proxies
// publish setUrlMap and setSslCertificates without the "global" segment
// their own address has, so that spelling is tried as well, as the real API
// serves both.
var setterNameRE = regexp.MustCompile(`^(?:set|expand)[A-Z][A-Za-z]*$`)

func (s *Server) setterTarget(path string) string {
	i := strings.LastIndex(path, "/")
	if i <= 0 || !setterNameRE.MatchString(path[i+1:]) {
		return ""
	}
	parent := path[:i]
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.resources[parent]; ok {
		return parent
	}
	if j := strings.Index(parent, "/projects/"); j >= 0 {
		rest := parent[j+len("/projects/"):]
		if k := strings.Index(rest, "/"); k >= 0 {
			global := parent[:j] + "/projects/" + rest[:k] + "/global" + rest[k:]
			if _, ok := s.resources[global]; ok {
				return global
			}
		}
	}
	return ""
}

// handleSetter answers a compute setter (setLabels, setUrlMap,
// setSecurityPolicy): the body's fields are written onto the resource. A
// resource that carries a labelFingerprint refuses a body that quotes a stale
// one and gets a new one after, which is how compute guards setLabels.
func (s *Server) handleSetter(w http.ResponseWriter, r *http.Request, bodyBytes []byte) {
	path := s.setterTarget(r.URL.Path)
	body := map[string]any{}
	if len(bodyBytes) > 0 {
		if err := json.Unmarshal(bodyBytes, &body); err != nil {
			writeError(w, http.StatusBadRequest, "INVALID_ARGUMENT", "malformed JSON body")
			return
		}
	}
	s.mu.Lock()
	existing := s.resources[path]
	s.mu.Unlock()
	if fp, locked := existing["labelFingerprint"].(string); locked {
		if sent, _ := body["labelFingerprint"].(string); sent != fp {
			writeError(w, http.StatusPreconditionFailed, "FAILED_PRECONDITION",
				fmt.Sprintf("conditionNotMet: supplied labelFingerprint %q does not match current %q", sent, fp))
			return
		}
	}
	merged := cloneMap(existing)
	for k, v := range body {
		merged[k] = v
	}
	s.mu.Lock()
	if _, locked := existing["labelFingerprint"].(string); locked {
		s.fingerprints++
		merged["labelFingerprint"] = fmt.Sprintf("lfp-%d", s.fingerprints)
	}
	s.resources[path] = merged
	s.mu.Unlock()
	s.respondMutation(w, path, merged, false)
}
