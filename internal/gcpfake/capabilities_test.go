package gcpfake

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// TestDefaultOperationStyleIsSynchronous covers the 187-of-676-method case:
// no SetOperationStyle call at all means a mutation's response IS the
// resource, with no operation to poll.
func TestDefaultOperationStyleIsSynchronous(t *testing.T) {
	s := New(t)
	defer s.Close()

	resp, err := http.Post(s.URL()+"/v1/projects/p/locations/r/widgets?widgetId=one",
		"application/json", strings.NewReader(`{"sizeGb":10}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var got map[string]any
	json.NewDecoder(resp.Body).Decode(&got)
	if _, hasDone := got["done"]; hasDone {
		t.Fatalf("a synchronous create returned an operation envelope: %v", got)
	}
	if got["sizeGb"] != float64(10) {
		t.Fatalf("the resource was not returned directly: %v", got)
	}
	if stored, ok := s.Get("/v1/projects/p/locations/r/widgets/one"); !ok || stored["sizeGb"] != float64(10) {
		t.Errorf("nothing was stored: %v", stored)
	}
}

func TestComputeOperationStyleServesStatusPollingOnItsScope(t *testing.T) {
	s := New(t)
	defer s.Close()
	s.SetOperationStyle(OpCompute)

	resp, err := http.Post(s.URL()+"/v1/projects/p/zones/us-central1-a/instances?instanceId=web1",
		"application/json", strings.NewReader(`{"machineType":"e2-medium"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var op map[string]any
	json.NewDecoder(resp.Body).Decode(&op)
	if op["status"] != "RUNNING" {
		t.Fatalf("initial compute op = %v, want status RUNNING", op)
	}
	name, _ := op["name"].(string)
	if name == "" {
		t.Fatal("compute operation has no name to poll")
	}

	// The literal wire segment is "operations", never "zoneOperations" --
	// that is only Discovery's collection name for the scope and never
	// appears in a url (confirmed against schemas/compute.json).
	waitURL := s.URL() + "/v1/projects/p/zones/us-central1-a/operations/" + name + "/wait"
	r2, err := http.Post(waitURL, "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer r2.Body.Close()
	var op2 map[string]any
	json.NewDecoder(r2.Body).Decode(&op2)
	if op2["status"] != "DONE" {
		t.Fatalf("operation never reached DONE: %v", op2)
	}
	if stored, ok := s.Get("/v1/projects/p/zones/us-central1-a/instances/web1"); !ok || stored["machineType"] != "e2-medium" {
		t.Errorf("the instance was not stored: %v", stored)
	}
}

func TestNeverCompleteOperationsKeepsPollingUnfinished(t *testing.T) {
	s := New(t)
	defer s.Close()
	s.SetOperationStyle(OpLongRunning)
	s.NeverCompleteOperations()

	resp, _ := http.Post(s.URL()+"/v1/projects/p/locations/r/widgets?widgetId=one",
		"application/json", strings.NewReader(`{}`))
	var op map[string]any
	json.NewDecoder(resp.Body).Decode(&op)
	resp.Body.Close()
	name := op["name"].(string)

	for i := 0; i < 3; i++ {
		r, err := http.Get(s.URL() + "/v1/" + name)
		if err != nil {
			t.Fatal(err)
		}
		var polled map[string]any
		json.NewDecoder(r.Body).Decode(&polled)
		r.Body.Close()
		if polled["done"] != false {
			t.Fatalf("poll %d: done = %v, want false forever under NeverCompleteOperations", i, polled["done"])
		}
	}
}

// TestCreateThenFailOperationStillStoresTheResource is the realistic bad
// case await.go's tests depend on: the resource really exists, but the
// operation reports failure.
func TestCreateThenFailOperationStillStoresTheResource(t *testing.T) {
	s := New(t)
	defer s.Close()
	s.SetOperationStyle(OpLongRunning)
	s.CreateThenFailOperation("/v1/projects/p/locations/r/widgets/one", "INTERNAL", "post-create step failed")

	resp, _ := http.Post(s.URL()+"/v1/projects/p/locations/r/widgets?widgetId=one",
		"application/json", strings.NewReader(`{"sizeGb":5}`))
	var op map[string]any
	json.NewDecoder(resp.Body).Decode(&op)
	resp.Body.Close()
	name := op["name"].(string)

	if _, ok := s.Get("/v1/projects/p/locations/r/widgets/one"); !ok {
		t.Fatal("the resource was not created even though only the operation should fail")
	}

	r2, _ := http.Get(s.URL() + "/v1/" + name)
	var polled map[string]any
	json.NewDecoder(r2.Body).Decode(&polled)
	r2.Body.Close()
	if polled["done"] != true {
		t.Fatalf("operation never completed: %v", polled)
	}
	errBody, ok := polled["error"].(map[string]any)
	if !ok {
		t.Fatalf("operation completed without an error: %v", polled)
	}
	if errBody["message"] != "post-create step failed" {
		t.Errorf("error message = %v", errBody["message"])
	}
}

func TestNotFoundTimesThenSucceeds(t *testing.T) {
	s := New(t)
	defer s.Close()
	path := "/v1/projects/p/locations/r/widgets/one"
	s.NotFoundTimes(path, 2)
	s.Seed(path, map[string]any{"name": "one", "sizeGb": float64(10)})

	for i := 0; i < 2; i++ {
		resp, err := http.Get(s.URL() + path)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != 404 {
			t.Fatalf("attempt %d: status = %d, want 404", i, resp.StatusCode)
		}
	}
	resp, err := http.Get(s.URL() + path)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("third attempt: status = %d, want 200", resp.StatusCode)
	}
	var got map[string]any
	json.NewDecoder(resp.Body).Decode(&got)
	if got["sizeGb"] != float64(10) {
		t.Errorf("got %v", got)
	}
}

func TestPatchMergesOnlyFieldsNamedInTheMask(t *testing.T) {
	s := New(t)
	defer s.Close()
	path := "/v1/projects/p/locations/r/widgets/one"
	s.Seed(path, map[string]any{"name": "one", "sizeGb": float64(10), "tier": "STANDARD"})

	req, _ := http.NewRequest(http.MethodPatch, s.URL()+path+"?updateMask=sizeGb",
		bytes.NewReader([]byte(`{"sizeGb":20}`)))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	got, ok := s.Get(path)
	if !ok {
		t.Fatal("resource vanished")
	}
	if got["sizeGb"] != float64(20) {
		t.Errorf("sizeGb = %v, want 20", got["sizeGb"])
	}
	if got["tier"] != "STANDARD" {
		t.Errorf("tier = %v, a field outside the mask must not change", got["tier"])
	}
}

// TestPatchRefusesAMaskNamingAFieldAbsentFromTheBody is the check spec calls
// out explicitly: it is what catches a mask built from the wrong side of a
// diff (Task 14).
func TestPatchRefusesAMaskNamingAFieldAbsentFromTheBody(t *testing.T) {
	s := New(t)
	defer s.Close()
	path := "/v1/projects/p/locations/r/widgets/one"
	s.Seed(path, map[string]any{"name": "one", "sizeGb": float64(10)})

	req, _ := http.NewRequest(http.MethodPatch, s.URL()+path+"?updateMask=tier",
		bytes.NewReader([]byte(`{"sizeGb":20}`)))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 400 {
		t.Fatalf("status = %d, want 400: a mask naming an absent field must be refused", resp.StatusCode)
	}
	got, _ := s.Get(path)
	if got["sizeGb"] != float64(10) {
		t.Errorf("a refused patch still applied: %v", got)
	}
}

func TestPatchMasksANestedPath(t *testing.T) {
	s := New(t)
	defer s.Close()
	path := "/v1/projects/p/locations/r/widgets/one"
	s.Seed(path, map[string]any{
		"name":   "one",
		"config": map[string]any{"mode": "A", "size": float64(1)},
	})

	req, _ := http.NewRequest(http.MethodPatch, s.URL()+path+"?updateMask=config.mode",
		bytes.NewReader([]byte(`{"config":{"mode":"B"}}`)))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	got, _ := s.Get(path)
	cfg := got["config"].(map[string]any)
	if cfg["mode"] != "B" {
		t.Errorf("config.mode = %v, want B", cfg["mode"])
	}
	if cfg["size"] != float64(1) {
		t.Errorf("config.size = %v, a sibling outside the mask must be untouched", cfg["size"])
	}
}

// TestAnEmptyCollectionListsEmpty and TestAnAbsentIndividualResourceStill404s
// are the pair a discover fallback needs to tell apart: scanning every type
// across a project genuinely finds nothing for most of them, and that must
// come back as an empty list, not a 404 — while getting one specific,
// never-created resource must still 404.
//
// The collection here is declared explicitly (DeclareCollection), not
// inferred from its URL: list-vs-get is no longer guessed from path shape
// (an earlier version tried that and was silently wrong for about a fifth of
// the real catalog — see declaredCollections's doc comment), so a
// genuinely-empty collection a test cares about has to say so.
func TestAnEmptyCollectionListsEmpty(t *testing.T) {
	s := New(t)
	defer s.Close()
	s.DeclareCollection("/v1/projects/p/locations/r/widgets")

	resp, err := http.Get(s.URL() + "/v1/projects/p/locations/r/widgets")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, want 200: an empty collection is a valid, empty list, not an absence", resp.StatusCode)
	}
	var got struct {
		Items []map[string]any `json:"items"`
	}
	json.NewDecoder(resp.Body).Decode(&got)
	if len(got.Items) != 0 {
		t.Errorf("items = %v, want empty", got.Items)
	}
}

func TestAnAbsentIndividualResourceStill404s(t *testing.T) {
	s := New(t)
	defer s.Close()

	resp, err := http.Get(s.URL() + "/v1/projects/p/locations/r/widgets/never-created")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Fatalf("status = %d, want 404: a specific absent resource is not the same request as its collection", resp.StatusCode)
	}
}

// TestAGlobalScopeCollectionIsNotConfusedWithItsResource uses the real shape
// that broke the old segment-parity heuristic: compute's firewalls sit at
// "projects/{project}/global/firewalls", where "global" is a bare literal
// with no paired variable segment, unlike "zones/{zone}" or
// "locations/{region}". Declaring rather than inferring means this needs no
// special case at all — Seed just declares its own parent, whatever shape it
// is.
func TestAGlobalScopeCollectionIsNotConfusedWithItsResource(t *testing.T) {
	s := New(t)
	defer s.Close()
	s.Seed("/v1/projects/p/global/firewalls/fw-1", map[string]any{"name": "fw-1", "direction": "INGRESS"})

	listResp, err := http.Get(s.URL() + "/v1/projects/p/global/firewalls")
	if err != nil {
		t.Fatal(err)
	}
	defer listResp.Body.Close()
	if listResp.StatusCode != 200 {
		t.Fatalf("list status = %d, want 200", listResp.StatusCode)
	}
	var list struct {
		Items []map[string]any `json:"items"`
	}
	json.NewDecoder(listResp.Body).Decode(&list)
	if len(list.Items) != 1 || list.Items[0]["name"] != "fw-1" {
		t.Errorf("list = %+v, want one item named fw-1", list.Items)
	}

	getResp, err := http.Get(s.URL() + "/v1/projects/p/global/firewalls/fw-1")
	if err != nil {
		t.Fatal(err)
	}
	defer getResp.Body.Close()
	if getResp.StatusCode != 200 {
		t.Fatalf("get status = %d, want 200 (not the list branch, and not 404)", getResp.StatusCode)
	}
	var got map[string]any
	json.NewDecoder(getResp.Body).Decode(&got)
	if got["direction"] != "INGRESS" {
		t.Errorf("get = %v, want the seeded resource, not an empty list envelope", got)
	}
}

// TestALeadingVersionSegmentEmbeddedInTheTypesOwnTemplateIsNotConfused is the
// other real shape the old heuristic broke: Cloud Resource Manager's v3
// folders collection is just "v3/folders" — its own template embeds a
// version segment on top of the "/v1/" every request in this fake already
// carries, so the real, full path is "/v1/v3/folders" (list) and
// "/v1/v3/folders/123" (get). Under segment parity this used to invert BOTH
// ways at once: the list path (even segment count) was misread as a get, and
// the get path (odd) was misread as a list — so a read of an existing
// resource silently came back as an empty list instead of the resource.
func TestALeadingVersionSegmentEmbeddedInTheTypesOwnTemplateIsNotConfused(t *testing.T) {
	s := New(t)
	defer s.Close()
	s.Seed("/v1/v3/folders/123", map[string]any{"name": "folders/123", "displayName": "Engineering"})

	listResp, err := http.Get(s.URL() + "/v1/v3/folders")
	if err != nil {
		t.Fatal(err)
	}
	defer listResp.Body.Close()
	if listResp.StatusCode != 200 {
		t.Fatalf("list status = %d, want 200", listResp.StatusCode)
	}
	var list struct {
		Items []map[string]any `json:"items"`
	}
	json.NewDecoder(listResp.Body).Decode(&list)
	if len(list.Items) != 1 {
		t.Fatalf("list = %+v, want one item", list.Items)
	}

	getResp, err := http.Get(s.URL() + "/v1/v3/folders/123")
	if err != nil {
		t.Fatal(err)
	}
	defer getResp.Body.Close()
	if getResp.StatusCode != 200 {
		t.Fatalf("get status = %d, want 200", getResp.StatusCode)
	}
	var got map[string]any
	json.NewDecoder(getResp.Body).Decode(&got)
	if got["displayName"] != "Engineering" {
		t.Errorf("get = %v, want the seeded resource, not an empty list envelope", got)
	}
}

func TestSetListFieldOverridesTheDefaultItemsKey(t *testing.T) {
	s := New(t)
	defer s.Close()
	s.Seed("/v1/projects/p/locations/global/buckets/b1", map[string]any{"name": "b1"})
	s.SetListField("/v1/projects/p/locations/global/buckets", "buckets")

	resp, err := http.Get(s.URL() + "/v1/projects/p/locations/global/buckets")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var got map[string]any
	json.NewDecoder(resp.Body).Decode(&got)
	if _, hasItems := got["items"]; hasItems {
		t.Errorf("response still used the default \"items\" key: %v", got)
	}
	buckets, ok := got["buckets"].([]any)
	if !ok || len(buckets) != 1 {
		t.Errorf("buckets = %v, want one entry under the overridden field name", got["buckets"])
	}
}

func TestDeletingSomethingAlreadyGoneIs404(t *testing.T) {
	s := New(t)
	defer s.Close()
	req, _ := http.NewRequest(http.MethodDelete, s.URL()+"/v1/projects/p/locations/r/widgets/gone", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	// The fake reports the real wire truth (404); making destroy idempotent
	// in spite of that belongs to internal/gcprov (Task 13), not here.
	if resp.StatusCode != 404 {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
}

func TestListsPaginateAtTwoItems(t *testing.T) {
	s := New(t)
	defer s.Close()
	for _, id := range []string{"one", "two", "three"} {
		s.Seed("/v1/projects/p/locations/r/widgets/"+id, map[string]any{"name": id})
	}

	resp, err := http.Get(s.URL() + "/v1/projects/p/locations/r/widgets")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var page1 struct {
		Items         []map[string]any `json:"items"`
		NextPageToken string           `json:"nextPageToken"`
	}
	json.NewDecoder(resp.Body).Decode(&page1)
	if len(page1.Items) != 2 {
		t.Fatalf("first page = %d items, want 2", len(page1.Items))
	}
	if page1.NextPageToken == "" {
		t.Fatal("no nextPageToken even though a third item remains")
	}

	resp2, err := http.Get(s.URL() + "/v1/projects/p/locations/r/widgets?pageToken=" + page1.NextPageToken)
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	var page2 struct {
		Items         []map[string]any `json:"items"`
		NextPageToken string           `json:"nextPageToken"`
	}
	json.NewDecoder(resp2.Body).Decode(&page2)
	if len(page2.Items) != 1 {
		t.Fatalf("second page = %d items, want 1", len(page2.Items))
	}
	if page2.NextPageToken != "" {
		t.Errorf("nextPageToken = %q on the final page, want empty", page2.NextPageToken)
	}
}

func TestSeedCAIAndSearchAllResources(t *testing.T) {
	s := New(t)
	defer s.Close()
	s.SeedCAI("projects/p", []Asset{
		{AssetType: "tiny.googleapis.com/Widget", Name: "//tiny.googleapis.com/projects/p/locations/r/widgets/one"},
	})

	resp, err := http.Post(s.URL()+"/v1/projects/p:searchAllResources", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var got struct {
		Results []map[string]any `json:"results"`
	}
	json.NewDecoder(resp.Body).Decode(&got)
	if len(got.Results) != 1 || got.Results[0]["assetType"] != "tiny.googleapis.com/Widget" {
		t.Errorf("results = %+v", got.Results)
	}
}

func TestFailCAIFailsSearchAllResourcesWithTheInjectedError(t *testing.T) {
	s := New(t)
	defer s.Close()
	s.FailCAI(403, "PERMISSION_DENIED", "cloudasset.assets.searchAllResources denied")

	resp, err := http.Post(s.URL()+"/v1/projects/p:searchAllResources", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 403 {
		t.Fatalf("status = %d, want 403", resp.StatusCode)
	}
	var e struct {
		Error struct {
			Status string `json:"status"`
		} `json:"error"`
	}
	json.NewDecoder(resp.Body).Decode(&e)
	if e.Error.Status != "PERMISSION_DENIED" {
		t.Errorf("error status = %q", e.Error.Status)
	}
}

func TestSeedTagBindingsIsListedByParent(t *testing.T) {
	s := New(t)
	defer s.Close()
	parent := "//cloudresourcemanager.googleapis.com/projects/p"
	s.SeedTagBindings(parent, []map[string]any{
		{"name": "tagBindings/abc", "parent": parent, "tagValue": "tagValues/123"},
	})

	resp, err := http.Get(s.URL() + "/v3/tagBindings?parent=" + url.QueryEscape(parent))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var got struct {
		TagBindings []map[string]any `json:"tagBindings"`
	}
	json.NewDecoder(resp.Body).Decode(&got)
	if len(got.TagBindings) != 1 || got.TagBindings[0]["tagValue"] != "tagValues/123" {
		t.Errorf("tagBindings = %+v", got.TagBindings)
	}
}

// TestALockedResourceRefusesAStaleFingerprint is compute's optimistic
// locking. Without it here, 19 patchable compute types never sent a
// fingerprint and every test passed, while real Google would have answered
// every one of those patches with a 412.
func TestALockedResourceRefusesAStaleFingerprint(t *testing.T) {
	s := New(t)
	defer s.Close()
	path := "/compute/v1/projects/p/global/urlMaps/m"
	s.Seed(path, map[string]any{"name": "m", "fingerprint": "fp-a"})
	patch := func(body string) int {
		req, _ := http.NewRequest(http.MethodPatch, s.URL()+path, strings.NewReader(body))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if code := patch(`{"description": "x"}`); code != http.StatusPreconditionFailed {
		t.Errorf("patch with no fingerprint = %d, want 412", code)
	}
	if code := patch(`{"description": "x", "fingerprint": "fp-a"}`); code != http.StatusOK {
		t.Fatalf("patch with the current fingerprint = %d, want 200", code)
	}
	stored, _ := s.Get(path)
	if stored["fingerprint"] == "fp-a" {
		t.Error("the fingerprint did not change after a modification")
	}
	if code := patch(`{"description": "y", "fingerprint": "fp-a"}`); code != http.StatusPreconditionFailed {
		t.Errorf("patch quoting the superseded fingerprint = %d, want 412", code)
	}
}

// TestAnAIP133CreateAnswersWithTheFullName. A resource created with its id as a
// query parameter comes back with `name` set to its full relative name, as
// Google's AIP-133 APIs answer. The fake used to answer with no name at all,
// so a schema calling the short name `name` looked as though it round-tripped.
// Compute's insert, which names the resource in the body, keeps the name it
// was sent.
func TestAnAIP133CreateAnswersWithTheFullName(t *testing.T) {
	s := New(t)
	defer s.Close()
	post := func(path, body string) {
		resp, err := http.Post(s.URL()+path, "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
	}
	post("/v1/projects/p/locations/l/triggers?triggerId=t1", `{"labels": {}}`)
	if got, _ := s.Get("/v1/projects/p/locations/l/triggers/t1"); got["name"] != "projects/p/locations/l/triggers/t1" {
		t.Errorf("AIP-133 create stored name %v, want the full relative name", got["name"])
	}
	post("/compute/v1/projects/p/global/networks", `{"name": "net1"}`)
	if got, _ := s.Get("/compute/v1/projects/p/global/networks/net1"); got["name"] != "net1" {
		t.Errorf("compute insert stored name %v, want the short name it was sent", got["name"])
	}
}
