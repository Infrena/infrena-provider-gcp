package gcpfake

import (
	"net/http"
	"strings"
)

// Asset is one Cloud Asset Inventory search result: enough for Discover
// (Task 16) to map it through the catalog's AssetType and turn it into a
// provider id.
type Asset struct {
	AssetType string
	Name      string // e.g. "//tiny.googleapis.com/projects/p/locations/r/widgets/one"
	// Labels are the user labels CAI indexes for the resource. A search
	// result carries them without any readMask being asked for
	// (schemas/cloudasset.json, ResourceSearchResult.labels), which is what
	// lets discovery decide system ownership from a search result alone
	// rather than reading every resource it finds.
	Labels map[string]string
}

// SeedCAI makes searchAllResources under parent (e.g. "projects/p", no
// version prefix — the same form Discover resolves from the instance)
// return assets.
func (s *Server) SeedCAI(parent string, assets []Asset) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.caiAssets[parent] = assets
}

// FailCAI makes every searchAllResources call fail with this error, for the
// rest of the server's life — modelling cloudasset.googleapis.com being
// unreachable or unauthorized for the whole discovery run, not just one
// call, which is the case Discover's CAI-then-fall-back path exists for.
func (s *Server) FailCAI(status int, code, message string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.caiFail = &apiErrorSpec{Status: status, Code: code, Message: message}
}

// handleSearchAllResources answers GET <scope>:searchAllResources.
//
// A GET. The custom-verb suffix reads like an RPC, but cloudasset's own
// Discovery document (schemas/cloudasset.json) publishes
// "v1/{+scope}:searchAllResources" with httpMethod GET and seven query
// parameters, and no request body: this fake serves GCP's real wire format,
// so it answers the method GCP answers.
func (s *Server) handleSearchAllResources(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	fail := s.caiFail
	s.mu.Unlock()
	if fail != nil {
		writeError(w, fail.Status, fail.Code, fail.Message)
		return
	}

	parent := trimVersionPrefix(strings.TrimSuffix(r.URL.Path, ":searchAllResources"))
	s.mu.Lock()
	assets := append([]Asset(nil), s.caiAssets[parent]...)
	s.mu.Unlock()

	page, next := paginate(assets, r.URL.Query())
	results := make([]map[string]any, len(page))
	for i, a := range page {
		res := map[string]any{"assetType": a.AssetType, "name": a.Name}
		if len(a.Labels) > 0 {
			labels := make(map[string]any, len(a.Labels))
			for k, v := range a.Labels {
				labels[k] = v
			}
			res["labels"] = labels
		}
		results[i] = res
	}
	out := map[string]any{"results": results}
	if next != "" {
		out["nextPageToken"] = next
	}
	writeJSON(w, http.StatusOK, out)
}

// SeedTagBindings makes tagBindings.list under parent (e.g.
// "//cloudresourcemanager.googleapis.com/projects/p") return bindings —
// spec G6's worked ruling: gcp.tagbinding has no get method, so Task 16
// reads one by listing its parent and filtering.
func (s *Server) SeedTagBindings(parent string, bindings []map[string]any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tagBindings[parent] = append(s.tagBindings[parent], bindings...)
}

// handleListTagBindings answers GET .../tagBindings?parent=<parent>.
func (s *Server) handleListTagBindings(w http.ResponseWriter, r *http.Request) {
	parent := r.URL.Query().Get("parent")
	s.mu.Lock()
	bindings := append([]map[string]any(nil), s.tagBindings[parent]...)
	s.mu.Unlock()

	page, next := paginate(bindings, r.URL.Query())
	out := map[string]any{"tagBindings": page}
	if next != "" {
		out["nextPageToken"] = next
	}
	writeJSON(w, http.StatusOK, out)
}
