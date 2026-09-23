package gcprov

import (
	"context"
	"net/url"
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

// realTagBindingID is a tag binding name captured from real Google on
// 2026-09-22 (task-18a's live run), not an invented one. FOUR segments:
// Google percent-escapes the bound resource's own full name into one
// segment and appends "tagValues/<id>" as two more.
const realTagBindingID = "tagBindings/%2F%2Fcloudresourcemanager.googleapis.com%2Fprojects%2F123456789012/tagValues/281479230039359"

// TestARealTagBindingIdRoundTrips is part A. Against the old two-segment id
// shape ("tagBindings/{{name}}") ParseProviderID refused this outright --
// "has more segments than gcp.tagbinding's id shape" -- so import failed and
// every create stored an id addressing nothing, which made destroy FORGET
// the binding and the tag value's own destroy then fail with "still attached
// to resources".
//
// The round trip is asserted, not the string: parse it, rebuild the request
// url from what came back, and require the url to address the same resource.
// A shape that merely parses is not enough; it has to come back out.
func TestARealTagBindingIdRoundTrips(t *testing.T) {
	c := mustCatalog(t)
	ty, ok := c.Type("gcp.tagbinding")
	if !ok {
		t.Fatal("the catalog no longer ships gcp.tagbinding")
	}
	attrs, err := ParseProviderID(ty, realTagBindingID)
	if err != nil {
		t.Fatalf("a real tag binding id is refused: %v", err)
	}
	p := &Provider{}
	reqURL, err := p.itemURL(ty, ty.DeleteURL, realTagBindingID, attrs)
	if err != nil {
		t.Fatalf("no request url could be built for it: %v", err)
	}
	want := ty.APIBaseURL + ty.PathPrefix + realTagBindingID
	if reqURL != want {
		t.Errorf("delete would go to\n  %q\nwant\n  %q\n(the id must survive being parsed and rebuilt, "+
			"or the resource is deleted nowhere)", reqURL, want)
	}
}

// TestATagBindingsOwnNameIsItsID. A long-running create answers with the
// TagBinding itself, name and all. That name IS the id; expanding it into a
// template that captures one segment escaped its own separators and stored
// "tagBindings/tagBindings%2F%252F%252F..." -- doubly escaped, addressing
// nothing.
func TestATagBindingsOwnNameIsItsID(t *testing.T) {
	c := mustCatalog(t)
	ty, _ := c.Type("gcp.tagbinding")
	id, err := ProviderID(ty, map[string]any{"name": realTagBindingID}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if id != realTagBindingID {
		t.Errorf("provider id = %q, want the name verbatim %q", id, realTagBindingID)
	}
}

// TestABindingIsListedUnderTheParentItsOwnIdNames. The parent used to come
// from Settings.Project, so a tag bound to a BUCKET -- which is most of what
// tag bindings are for -- was created and then never found: the project's
// listing does not contain it, and "not in the listing" is "believed
// absent", so the next plan proposed creating it again while destroy forgot
// it. The binding's id carries its own parent, percent-escaped.
func TestABindingIsListedUnderTheParentItsOwnIdNames(t *testing.T) {
	gcptest.Isolate(t)
	s := gcpfake.New(t)
	defer s.Close()
	const bucketParent = "//storage.googleapis.com/projects/_/buckets/some-bucket"
	bucketID := "tagBindings/" + url.PathEscape(bucketParent) + "/tagValues/999"
	// Seeded under the BUCKET, not under the project the instance configures.
	s.SeedTagBindings(bucketParent, []map[string]any{
		{"name": bucketID, "parent": bucketParent, "tagValue": "tagValues/999"},
	})
	p := testProvider(t, s)

	st, err := p.Read(context.Background(), &resource.ResourceState{
		Type: "gcp.tagbinding", ProviderID: bucketID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if st == nil {
		t.Fatalf("a binding on a bucket read as absent; the listing went to %v, and the "+
			"parent has to come from the id rather than from the instance's project",
			listedParents(s))
	}
	if st.ProviderID != bucketID {
		t.Errorf("provider id = %q, want %q", st.ProviderID, bucketID)
	}
}

// listedParents is every `parent` the fake was asked to list under, for a
// failure message that names where the read actually looked.
func listedParents(s *gcpfake.Server) []string {
	var out []string
	for _, r := range s.Requests() {
		if v := r.Query.Get("parent"); v != "" {
			out = append(out, v)
		}
	}
	return out
}

// TestAnIdWithNoFullResourceNameStillUsesTheConfiguredProject is the other
// side: the id-derived parent is an addition, not a replacement, and a type
// whose id carries no "//service/..." segment must still be listed under the
// instance's own project.
func TestAnIdWithNoFullResourceNameStillUsesTheConfiguredProject(t *testing.T) {
	if got := fullResourceNameIn("tagBindings/abc"); got != "" {
		t.Errorf("an ordinary id yielded a parent %q; only a segment that decodes to "+
			"//<host>/... is a full resource name", got)
	}
	if got := fullResourceNameIn("b/my%2Fbucket/o/x"); got != "" {
		t.Errorf("a segment with an escaped slash but no //host head yielded %q", got)
	}
	want := "//cloudresourcemanager.googleapis.com/projects/123456789012"
	if got := fullResourceNameIn(realTagBindingID); got != want {
		t.Errorf("parent = %q, want %q", got, want)
	}
}
