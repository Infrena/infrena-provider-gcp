package gcprov

import (
	"context"
	"testing"

	"github.com/infrena/infrena-provider-gcp/internal/gcpfake"
	"github.com/infrena/infrena-provider-gcp/internal/gcptest"
	"github.com/infrena/infrena/pkg/resource"
)

// tagBindingParent is the full resource name cloudresourcemanager v3's
// tagBindings.list takes as its parent for project "p" -- "//<service>/<relative
// name>", the Cloud Asset Inventory full-resource-name form, with the service
// coming from the type's own asset type.
const tagBindingParent = "//cloudresourcemanager.googleapis.com/projects/p"

// TestATypeWithNoGetIsReadByListingItsParent. gcp.tagbinding has no get
// method, so its read is a list filtered by parent -- spec G6's worked
// ruling, working end to end through the ordinary Read entry point.
func TestATypeWithNoGetIsReadByListingItsParent(t *testing.T) {
	gcptest.Isolate(t)
	s := gcpfake.New(t)
	defer s.Close()
	s.SeedTagBindings(tagBindingParent, []map[string]any{
		{"name": "tagBindings/abc", "parent": tagBindingParent,
			"tagValue": "tagValues/123"},
	})
	p := testProvider(t, s)

	st, err := p.Read(context.Background(), &resource.ResourceState{
		Type: "gcp.tagbinding", ProviderID: "tagBindings/abc",
	})
	if err != nil {
		t.Fatal(err)
	}
	if st == nil {
		t.Fatal("the binding was not found by listing its parent")
	}
	if st.Attributes["tagValue"].Raw != "tagValues/123" {
		t.Errorf("attributes = %v", st.Attributes)
	}
}

// TestTheReadKeepsTheIdItMatchedBy. The entry was found by matching this
// exact id against the parent's listing, so there is nothing for a
// recomputation to discover -- and recomputing actively breaks this type.
// gcp.tagbinding's self_link is "tagBindings/{{name}}", a single-segment
// {{name}} placeholder, while the field the API answers with is
// name = "tagBindings/abc", the whole relative name. ProviderID overlays the
// body on the id's own attributes, so the placeholder receives
// "tagBindings/abc" and escapes it: "tagBindings/tagBindings%2Fabc", an id
// that addresses nothing and that a later Delete would send verbatim.
func TestAParentListingReadKeepsTheIdItMatchedBy(t *testing.T) {
	gcptest.Isolate(t)
	s := gcpfake.New(t)
	defer s.Close()
	s.SeedTagBindings(tagBindingParent, []map[string]any{
		{"name": "tagBindings/abc", "parent": tagBindingParent, "tagValue": "tagValues/123"},
	})
	p := testProvider(t, s)

	st, err := p.Read(context.Background(), &resource.ResourceState{
		Type: "gcp.tagbinding", ProviderID: "tagBindings/abc",
	})
	if err != nil {
		t.Fatal(err)
	}
	if st == nil {
		t.Fatal("the binding was not found")
	}
	if st.ProviderID != "tagBindings/abc" {
		t.Errorf("provider id = %q, want the id it was read at", st.ProviderID)
	}
}

// TestTheParentListingIsFilteredByTheParentGCPTakes. tagBindings.list
// answers nothing without a parent, and the parent it takes is a FULL
// resource name, not the relative one this provider uses for ids. The
// service comes from the type's asset type rather than from APIBaseURL,
// which a test (and one day a private endpoint) rewrites.
func TestTheParentListingSendsAFullResourceName(t *testing.T) {
	gcptest.Isolate(t)
	s := gcpfake.New(t)
	defer s.Close()
	s.SeedTagBindings(tagBindingParent, []map[string]any{
		{"name": "tagBindings/abc", "parent": tagBindingParent, "tagValue": "tagValues/123"},
	})
	p := testProvider(t, s)

	if _, err := p.Read(context.Background(), &resource.ResourceState{
		Type: "gcp.tagbinding", ProviderID: "tagBindings/abc",
	}); err != nil {
		t.Fatal(err)
	}
	var sent []string
	for _, r := range s.Requests() {
		if r.Path == "/v3/tagBindings" {
			sent = append(sent, r.Query.Get("parent"))
		}
	}
	if len(sent) == 0 {
		t.Fatal("the listing was not sent at the type's own collection path")
	}
	if sent[0] != tagBindingParent {
		t.Errorf("parent = %q, want %q", sent[0], tagBindingParent)
	}
}

// TestABindingThatIsNotInItsParentsListingIsGone. (nil, nil) is Read's own
// "believed absent" contract, and destroy converging depends on this path
// answering it the same way the 404 path does.
func TestABindingMissingFromTheListingReadsAsAbsent(t *testing.T) {
	gcptest.Isolate(t)
	s := gcpfake.New(t)
	defer s.Close()
	s.SeedTagBindings(tagBindingParent, []map[string]any{
		{"name": "tagBindings/other", "parent": tagBindingParent, "tagValue": "tagValues/999"},
	})
	p := testProvider(t, s)

	st, err := p.Read(context.Background(), &resource.ResourceState{
		Type: "gcp.tagbinding", ProviderID: "tagBindings/abc",
	})
	if err != nil {
		t.Fatalf("a binding that is not there is not an error: %v", err)
	}
	if st != nil {
		t.Errorf("read a binding that is not in its parent's listing: %+v", st)
	}
}

// TestTheParentListingPagesThroughItsResults. The fake paginates at two, so
// a binding at position three is only found if nextPageToken was followed.
// A read that stops at the first page reports an existing resource as gone,
// which makes the next plan create a duplicate.
func TestTheParentListingFollowsItsPageToken(t *testing.T) {
	gcptest.Isolate(t)
	s := gcpfake.New(t)
	defer s.Close()
	var bindings []map[string]any
	for _, n := range []string{"one", "two", "three", "four", "five"} {
		bindings = append(bindings, map[string]any{
			"name": "tagBindings/" + n, "parent": tagBindingParent, "tagValue": "tagValues/" + n,
		})
	}
	s.SeedTagBindings(tagBindingParent, bindings)
	p := testProvider(t, s)

	st, err := p.Read(context.Background(), &resource.ResourceState{
		Type: "gcp.tagbinding", ProviderID: "tagBindings/five",
	})
	if err != nil {
		t.Fatal(err)
	}
	if st == nil {
		t.Fatal("a binding on the third page was reported as gone")
	}
	if st.Attributes["tagValue"].Raw != "tagValues/five" {
		t.Errorf("attributes = %v", st.Attributes)
	}
}

// TestImportOfATypeWithNoGetGoesThroughTheSamePath. Import is Read with a
// parsed id, so adopting a tag binding works for exactly the same reason
// refreshing one does -- and if the read_via check had been put anywhere but
// in Read, one of the two would silently 404 instead.
func TestImportingATagBindingUsesTheParentListing(t *testing.T) {
	gcptest.Isolate(t)
	s := gcpfake.New(t)
	defer s.Close()
	s.SeedTagBindings(tagBindingParent, []map[string]any{
		{"name": "tagBindings/abc", "parent": tagBindingParent, "tagValue": "tagValues/123"},
	})
	p := testProvider(t, s)

	st, err := p.Import(context.Background(), "gcp.tagbinding", "tagBindings/abc")
	if err != nil {
		t.Fatal(err)
	}
	if st == nil {
		t.Fatal("the binding could not be imported")
	}
	if st.ProviderID != "tagBindings/abc" {
		t.Errorf("provider id = %q", st.ProviderID)
	}
}

// TestTheCatalogStillRulesTagBindingsReadThatWay. The whole path above is
// reached only because the generated catalog records read_via and a list
// field for this type. If a regeneration drops either, every test here would
// still pass against a Read that quietly went back to a GET, so the ruling
// itself is pinned.
func TestTheCatalogStillRulesTagBindingReadByListing(t *testing.T) {
	c := mustCatalog(t)
	ty, ok := c.Type("gcp.tagbinding")
	if !ok {
		t.Fatal("the catalog no longer serves gcp.tagbinding")
	}
	if ty.ReadVia != ReadViaListByParent {
		t.Errorf("read_via = %q, want %q", ty.ReadVia, ReadViaListByParent)
	}
	if ty.ListField != "tagBindings" {
		t.Errorf("list_field = %q, want tagBindings", ty.ListField)
	}
	if ty.AssetType != "cloudresourcemanager.googleapis.com/TagBinding" {
		t.Errorf("asset_type = %q, which is where the parent's full resource name comes from", ty.AssetType)
	}
}
