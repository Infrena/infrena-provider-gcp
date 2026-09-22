package gcpfake

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func TestACreateReturnsAnOperationThatEventuallyCompletes(t *testing.T) {
	s := New(t)
	defer s.Close()
	s.SetOperationStyle(OpLongRunning)

	body := strings.NewReader(`{"name":"widgets/one","sizeGb":10}`)
	resp, err := http.Post(s.URL()+"/v1/projects/p/locations/r/widgets?widgetId=one",
		"application/json", body)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var op map[string]any
	json.NewDecoder(resp.Body).Decode(&op)
	if op["done"] != false {
		t.Fatalf("first response should be an unfinished operation, got %v", op)
	}
	name, _ := op["name"].(string)
	if name == "" {
		t.Fatal("operation has no name to poll")
	}

	// Poll. The fake completes after one poll, which is enough to exercise the
	// await loop without making tests slow.
	r2, err := http.Get(s.URL() + "/v1/" + name)
	if err != nil {
		t.Fatal(err)
	}
	defer r2.Body.Close()
	var op2 map[string]any
	json.NewDecoder(r2.Body).Decode(&op2)
	if op2["done"] != true {
		t.Fatalf("operation never completed: %v", op2)
	}

	if got, ok := s.Get("/v1/projects/p/locations/r/widgets/one"); !ok || got["sizeGb"] != float64(10) {
		t.Errorf("the resource was not actually stored: %v", got)
	}
}

func TestAnInjectedFailureUsesGCPsErrorShape(t *testing.T) {
	s := New(t)
	defer s.Close()
	s.FailNext(429, "RESOURCE_EXHAUSTED", "Quota exceeded.")

	resp, err := http.Get(s.URL() + "/v1/projects/p/locations/r/widgets/one")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 429 {
		t.Fatalf("status = %d, want 429", resp.StatusCode)
	}
	var e struct {
		Error struct {
			Code    int    `json:"code"`
			Status  string `json:"status"`
			Message string `json:"message"`
		} `json:"error"`
	}
	json.NewDecoder(resp.Body).Decode(&e)
	// The nesting under "error" is not decoration: it is what the real API sends,
	// and a fake that flattened it would let a broken decoder pass.
	if e.Error.Status != "RESOURCE_EXHAUSTED" || e.Error.Code != 429 {
		t.Errorf("error body is not GCP-shaped: %+v", e)
	}
}

func TestTheFakeRecordsWhatWasSent(t *testing.T) {
	s := New(t)
	defer s.Close()
	http.Get(s.URL() + "/v1/projects/p/locations/r/widgets/one")
	reqs := s.Requests()
	if len(reqs) != 1 || reqs[0].Method != "GET" {
		t.Fatalf("requests = %+v", reqs)
	}
	if !strings.Contains(reqs[0].Path, "/widgets/one") {
		t.Errorf("path not recorded: %q", reqs[0].Path)
	}
}

// TestTrimVersionPrefixOnlyStripsAnActualVersion. trimVersionPrefix used to
// discard whatever segment came first, version or not: for a bare
// "/operations/op-1" poll path (no version prefix at all -- the shape a
// longrunning operation's own bare name produces, task-12), it silently
// discarded "operations" and returned "op-1", which is not a registered
// operation name, and the poll 404s. Every fixture through Task 10 happened
// to poll a "/v1/..." path, so the bug was invisible to the shapes the suite
// exercised until Task 12's await tests hit a bare-name poll directly.
func TestTrimVersionPrefixOnlyStripsAnActualVersion(t *testing.T) {
	cases := []struct {
		path string
		want string
	}{
		{"/v1/operations/abc", "operations/abc"},
		{"/operations/abc", "operations/abc"},
		{"/v1beta1/projects/p/widgets", "projects/p/widgets"},
		{"/v2/projects/p/locations/l/operations/op-1", "projects/p/locations/l/operations/op-1"},
	}
	for _, c := range cases {
		if got := trimVersionPrefix(c.path); got != c.want {
			t.Errorf("trimVersionPrefix(%q) = %q, want %q", c.path, got, c.want)
		}
	}
}
