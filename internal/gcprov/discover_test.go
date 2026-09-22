package gcprov

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/infrena/infrena-provider-gcp/internal/gcpfake"
	"github.com/infrena/infrena-provider-gcp/internal/gcptest"
	"github.com/infrena/infrena/pkg/provider"
)

// TestDiscoverPrefersCloudAssetInventory. One call for a whole project,
// instead of one list call per type -- the reason GCP discovery is complete
// by default where AWS's must be curated.
func TestDiscoverPrefersCloudAssetInventory(t *testing.T) {
	gcptest.Isolate(t)
	s := gcpfake.New(t)
	defer s.Close()
	s.SeedCAI("projects/p", []gcpfake.Asset{
		{AssetType: "tiny.googleapis.com/Widget", Name: "//tiny.googleapis.com/projects/p/locations/r/widgets/one"},
	})
	p := testProviderWithCatalog(t, s, widgetCatalog())

	got, err := p.Discover(context.Background(), provider.DiscoverRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Type != "gcp.widget" {
		t.Fatalf("discovered %+v", got)
	}
	var listed bool
	for _, r := range s.Requests() {
		if r.Method == "GET" && strings.HasSuffix(r.Path, "/widgets") {
			listed = true
		}
	}
	if listed {
		t.Error("per-type list was called even though CAI answered")
	}
}

// TestTheAssetSearchIsSentTheWayCloudAssetPublishesIt. cloudasset's own
// Discovery document says "v1/{+scope}:searchAllResources", httpMethod GET,
// with the scope in the path and everything else in the query. The brief for
// this task and the fake's first router both had it as a POST; against the
// real endpoint that is a 404, and because Discover falls back on ANY CAI
// failure it would have looked exactly like a project where the asset API is
// simply not enabled -- silently degrading every discovery run to the
// six-type fallback list.
func TestTheAssetSearchIsAGetAtTheScopePath(t *testing.T) {
	gcptest.Isolate(t)
	s := gcpfake.New(t)
	defer s.Close()
	s.SeedCAI("projects/p", []gcpfake.Asset{
		{AssetType: "tiny.googleapis.com/Widget", Name: "//tiny.googleapis.com/projects/p/locations/r/widgets/one"},
	})
	p := testProviderWithCatalog(t, s, widgetCatalog())

	if _, err := p.Discover(context.Background(), provider.DiscoverRequest{}); err != nil {
		t.Fatal(err)
	}
	var search *gcpfake.Request
	for i, r := range s.Requests() {
		if strings.HasSuffix(r.Path, ":searchAllResources") {
			search = &s.Requests()[i]
		}
	}
	if search == nil {
		t.Fatal("no asset search was sent at all")
	}
	if search.Method != http.MethodGet {
		t.Errorf("the asset search was sent as %s, but cloudasset publishes searchAllResources as GET", search.Method)
	}
	if search.Path != "/v1/projects/p:searchAllResources" {
		t.Errorf("search path = %q, want the scope expanded unescaped into v1/{+scope}:searchAllResources", search.Path)
	}
	if len(search.Body) != 0 {
		t.Errorf("the asset search carried a %d-byte body; the method takes none", len(search.Body))
	}
}

// TestDiscoverFallsBackWhenCAIIsUnavailable, and says so, rather than
// reporting an empty project as if it were genuinely empty.
func TestDiscoverFallsBackToPerTypeList(t *testing.T) {
	gcptest.Isolate(t)
	s := gcpfake.New(t)
	defer s.Close()
	s.FailCAI(403, "PERMISSION_DENIED", "cloudasset.assets.searchAllResources denied")
	s.Seed("/v1/projects/p/locations/r/widgets/one", map[string]any{"name": "one"})
	p := testProviderWithCatalog(t, s, widgetCatalog())
	p.settings.DiscoverTypes = []string{"gcp.widget"}

	got, err := p.Discover(context.Background(), provider.DiscoverRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("fallback found %d resources, want 1", len(got))
	}
	if got[0].ProviderID != "projects/p/locations/r/widgets/one" {
		t.Errorf("provider id = %q", got[0].ProviderID)
	}
}

// TestADiscoveredResourceIsNamedFromItsOwnName. GCP resources carry a real
// name, so unlike AWS there is no Name-tag dependence and no sanitised-id
// fallback.
func TestADiscoveredResourceIsNamedFromItsOwnName(t *testing.T) {
	gcptest.Isolate(t)
	s := gcpfake.New(t)
	defer s.Close()
	s.SeedCAI("projects/p", []gcpfake.Asset{
		{AssetType: "tiny.googleapis.com/Widget",
			Name: "//tiny.googleapis.com/projects/p/locations/r/widgets/web-frontend"},
	})
	p := testProviderWithCatalog(t, s, widgetCatalog())
	got, err := p.Discover(context.Background(), provider.DiscoverRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("discovered %d resources, want 1", len(got))
	}
	if got[0].ProviderID != "projects/p/locations/r/widgets/web-frontend" {
		t.Errorf("provider id = %q", got[0].ProviderID)
	}
	// The id's own hierarchy is decomposed, so an adopted resource does not
	// show project and region unset and plan them forever.
	for name, want := range map[string]string{"project": "p", "region": "r", "name": "web-frontend"} {
		v, ok := got[0].Attributes[name]
		if !ok {
			t.Errorf("attribute %q is missing from %v", name, got[0].Attributes)
			continue
		}
		if s, _ := v.Raw.(string); s != want {
			t.Errorf("attribute %q = %v, want %q", name, v.Raw, want)
		}
	}
}

// TestCAIPagesThroughItsResults. The fake paginates at two, so a third
// result exists only if the nextPageToken was followed. A discovery that
// stops at the first page reports a project as smaller than it is, which
// reads as a complete answer.
func TestDiscoverFollowsTheAssetSearchsPageToken(t *testing.T) {
	gcptest.Isolate(t)
	s := gcpfake.New(t)
	defer s.Close()
	var assets []gcpfake.Asset
	for _, n := range []string{"one", "two", "three", "four", "five"} {
		assets = append(assets, gcpfake.Asset{
			AssetType: "tiny.googleapis.com/Widget",
			Name:      "//tiny.googleapis.com/projects/p/locations/r/widgets/" + n,
		})
	}
	s.SeedCAI("projects/p", assets)
	p := testProviderWithCatalog(t, s, widgetCatalog())

	got, err := p.Discover(context.Background(), provider.DiscoverRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 5 {
		t.Fatalf("discovered %d of 5 assets; the page token was not followed", len(got))
	}
}

// TestASharedAssetTypeIsResolvedByTheResourcesOwnName. 27 of the 191 asset
// types in the real catalog are served by more than one catalog type. A flat
// assetType -> type map picks whichever was indexed last and mislabels the
// rest: a zonal autoscaler reported as gcp.regionautoscaler generates
// configuration that looks right and cannot apply.
func TestASharedAssetTypeIsResolvedByTheResourcesOwnName(t *testing.T) {
	gcptest.Isolate(t)
	s := gcpfake.New(t)
	defer s.Close()
	s.SeedCAI("projects/p", []gcpfake.Asset{
		{AssetType: "compute.googleapis.com/Autoscaler",
			Name: "//compute.googleapis.com/projects/p/zones/us-central1-a/autoscalers/zonal-one"},
		{AssetType: "compute.googleapis.com/Autoscaler",
			Name: "//compute.googleapis.com/projects/p/regions/us-central1/autoscalers/regional-one"},
	})
	p := testProvider(t, s)

	// Both types must really be in the catalog, or this test would pass by
	// testing nothing.
	for _, name := range []string{"gcp.autoscaler", "gcp.regionautoscaler"} {
		if _, ok := p.catalog.Type(name); !ok {
			t.Fatalf("the catalog no longer serves %s, so this test proves nothing", name)
		}
	}

	got, err := p.Discover(context.Background(), provider.DiscoverRequest{})
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]string{}
	for _, r := range got {
		byID[r.ProviderID] = r.Type
	}
	if len(got) != 2 {
		t.Fatalf("discovered %d autoscalers, want 2: %v", len(got), byID)
	}
	if ty := byID["projects/p/zones/us-central1-a/autoscalers/zonal-one"]; ty != "gcp.autoscaler" {
		t.Errorf("the zonal autoscaler was reported as %q", ty)
	}
	if ty := byID["projects/p/regions/us-central1/autoscalers/regional-one"]; ty != "gcp.regionautoscaler" {
		t.Errorf("the regional autoscaler was reported as %q", ty)
	}
}

// TestAParentScopedAssetTypeIsResolvedByItsHierarchyRoot. The harder half of
// the same problem, and the one the templates cannot answer: logging's four
// LogBucket variants all have base_url "{+parent}/buckets" and self_link
// "{+name}", character for character, so matching the name against the
// template leaves all four matching and every log bucket in the project is
// skipped as ambiguous. catalog.Type.ParentRoot is what tells them apart.
func TestAParentScopedAssetTypeIsResolvedByItsHierarchyRoot(t *testing.T) {
	gcptest.Isolate(t)
	s := gcpfake.New(t)
	defer s.Close()
	s.SeedCAI("projects/p", []gcpfake.Asset{
		{AssetType: "logging.googleapis.com/LogBucket",
			Name: "//logging.googleapis.com/projects/p/locations/global/buckets/_Default"},
		{AssetType: "logging.googleapis.com/LogBucket",
			Name: "//logging.googleapis.com/folders/123/locations/global/buckets/audit"},
	})
	p := testProvider(t, s)

	got, err := p.Discover(context.Background(), provider.DiscoverRequest{})
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]string{}
	for _, r := range got {
		byID[r.ProviderID] = r.Type
	}
	if ty := byID["projects/p/locations/global/buckets/_Default"]; ty != "gcp.logging.bucket" {
		t.Errorf("the project's log bucket was reported as %q, want gcp.logging.bucket", ty)
	}
	if ty := byID["folders/123/locations/global/buckets/audit"]; ty != "gcp.logging.folder.bucket" {
		t.Errorf("the folder's log bucket was reported as %q, want gcp.logging.folder.bucket", ty)
	}
}

// TestNoTwoTypesSharingAnAssetTypeAreIndistinguishable is the invariant the
// whole CAI path rests on. An asset type does NOT name a catalog type: 27 of
// the 191 distinct asset types in the shipped catalog are served by two or
// more of the 233 types. Discovery tells them apart by the resource's own
// name, and it can only do that if no two of them present the same
// (hierarchy root, id template shape) pair -- otherwise a resource of either
// one matches both candidates and is skipped, or, worse, a flat lookup
// labels it as whichever was indexed last.
//
// Measured against the shipped catalog on 2026-09-22: 27 shared asset types,
// and 13 of them are shared by types whose self_link templates are
// CHARACTER-IDENTICAL, so the template alone resolves only 14 of the 27.
// catalog.Type.ParentRoot is what resolves the other 13, and this is what
// fails if a regeneration ever stops recording it.
func TestNoTwoTypesSharingAnAssetTypeAreIndistinguishable(t *testing.T) {
	c := mustCatalog(t)
	p := &Provider{catalog: c}
	byAsset := p.assetTypeIndex()

	shared := 0
	templateOnly := 0
	for assetType, types := range byAsset {
		if len(types) < 2 {
			continue
		}
		shared++
		seen := map[string]string{}
		shapes := map[string]bool{}
		for _, ty := range types {
			shapes[normalizeTemplateShape(ty.SelfLink)] = true
			key := ty.ParentRoot + " " + normalizeTemplateShape(ty.SelfLink)
			if other, clash := seen[key]; clash {
				t.Errorf("%s: %s and %s are indistinguishable (root %q, shape %q), so a resource of either is mislabelled or skipped",
					assetType, other, ty.Name, ty.ParentRoot, normalizeTemplateShape(ty.SelfLink))
			}
			seen[key] = ty.Name
		}
		if len(shapes) == len(types) {
			templateOnly++
		}
	}
	if shared == 0 {
		t.Fatal("no asset type is shared by two types at all, so this test proves nothing")
	}
	t.Logf("%d shared asset types; %d resolvable by template alone, %d needing the parent root",
		shared, templateOnly, shared-templateOnly)
}

// TestAMultiSegmentParentStillMatchesItsOwnTemplate. magic-modules writes
// "{{parent}}" as one template segment for a value that is really two
// ("projects/123"), so a strict segment-by-segment match fails for every
// resource of the 17 types shaped that way -- all of which are parent
// variants sharing an asset type, so every one of their resources would be
// skipped as unattributable rather than mislabelled. Quiet, and complete
// absence from `discover`.
func TestAMultiSegmentParentStillResolvesToOneType(t *testing.T) {
	gcptest.Isolate(t)
	s := gcpfake.New(t)
	defer s.Close()
	s.SeedCAI("projects/p", []gcpfake.Asset{
		{AssetType: "networksecurity.googleapis.com/AddressGroup",
			Name: "//networksecurity.googleapis.com/projects/p/locations/us-central1/addressGroups/allowlist"},
		{AssetType: "networksecurity.googleapis.com/AddressGroup",
			Name: "//networksecurity.googleapis.com/organizations/42/locations/us-central1/addressGroups/orglist"},
	})
	p := testProvider(t, s)

	got, err := p.Discover(context.Background(), provider.DiscoverRequest{})
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]string{}
	for _, r := range got {
		byID[r.ProviderID] = r.Type
	}
	if len(got) != 2 {
		t.Fatalf("discovered %d address groups, want 2: %v", len(got), byID)
	}
	if ty := byID["projects/p/locations/us-central1/addressGroups/allowlist"]; ty != "gcp.networksecurity.addressgroup" {
		t.Errorf("the project address group was reported as %q", ty)
	}
	if ty := byID["organizations/42/locations/us-central1/addressGroups/orglist"]; ty != "gcp.networksecurity.organization.addressgroup" {
		t.Errorf("the organization address group was reported as %q", ty)
	}
}

// TestTheParentAbsorptionDoesNotLetAnyNameMatch. The allowance above loosens
// which names a type accepts; it must not loosen which type a name picks
// out. A name at the wrong root, or one whose tail does not line up with the
// template, still matches nothing.
func TestTheMultiSegmentParentDoesNotMatchAnythingElse(t *testing.T) {
	c := mustCatalog(t)
	ty, ok := c.Type("gcp.networksecurity.addressgroup")
	if !ok {
		t.Fatal("the catalog no longer serves gcp.networksecurity.addressgroup")
	}
	for _, rel := range []string{
		"folders/1/locations/us/addressGroups/g",     // wrong hierarchy root
		"projects/p/locations/us/securityProfiles/g", // right root, wrong collection
		"projects/p/addressGroups/g",                 // no location segment
		"projects/addressGroups",                     // no room for a parent
	} {
		if matchesIDShape(ty, rel) {
			t.Errorf("%q was accepted as a %s", rel, ty.Name)
		}
	}
}

// TestAnUnservedAssetTypeIsSkippedQuietly. A project is full of resources
// this provider does not model; reporting each as an error would bury the
// ones it does.
func TestAnAssetTypeTheCatalogDoesNotServeIsSkipped(t *testing.T) {
	gcptest.Isolate(t)
	s := gcpfake.New(t)
	defer s.Close()
	s.SeedCAI("projects/p", []gcpfake.Asset{
		{AssetType: "nowhere.googleapis.com/Thing", Name: "//nowhere.googleapis.com/projects/p/things/t"},
		{AssetType: "tiny.googleapis.com/Widget", Name: "//tiny.googleapis.com/projects/p/locations/r/widgets/one"},
	})
	p := testProviderWithCatalog(t, s, widgetCatalog())

	got, err := p.Discover(context.Background(), provider.DiscoverRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Type != "gcp.widget" {
		t.Fatalf("discovered %+v", got)
	}
}

// TestDiscoverMarksWhatGoogleOwns. The flag and its reason travel with the
// resource, on the CAI path as well as the fallback -- a search result
// carries labels without any readMask, which is the whole evidence the
// default discovery path has.
func TestADiscoveredResourceCarriesItsSystemOwnedReason(t *testing.T) {
	gcptest.Isolate(t)
	s := gcpfake.New(t)
	defer s.Close()
	s.SeedCAI("projects/p", []gcpfake.Asset{
		{AssetType: "tiny.googleapis.com/Widget",
			Name:   "//tiny.googleapis.com/projects/p/locations/r/widgets/gke-one",
			Labels: map[string]string{"goog-gke-node": "true"}},
		{AssetType: "tiny.googleapis.com/Widget",
			Name: "//tiny.googleapis.com/projects/p/locations/r/widgets/mine"},
	})
	p := testProviderWithCatalog(t, s, widgetCatalog())

	got, err := p.Discover(context.Background(), provider.DiscoverRequest{})
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]provider.DiscoveredResource{}
	for _, r := range got {
		byID[r.ProviderID] = r
	}
	gke := byID["projects/p/locations/r/widgets/gke-one"]
	if !gke.SystemOwned {
		t.Error("the GKE-labelled resource was not flagged")
	}
	if !strings.Contains(gke.SystemOwnedReason, "goog-gke-node") {
		t.Errorf("reason %q does not name the label", gke.SystemOwnedReason)
	}
	mine := byID["projects/p/locations/r/widgets/mine"]
	if mine.SystemOwned || mine.SystemOwnedReason != "" {
		t.Errorf("an ordinary resource was flagged: %v %q", mine.SystemOwned, mine.SystemOwnedReason)
	}
}

// TestTheFallbackReadsTheTypesOwnListField, never a hardcoded "items".
// Measured over the fetched corpus: 209 distinct array-field names across
// 532 list methods, "items" covering 127. Hardcoding it works for compute
// and silently returns nothing for everything else -- which reads as "this
// project has none of that type" rather than as a bug.
func TestTheFallbackReadsTheTypesOwnListField(t *testing.T) {
	gcptest.Isolate(t)
	s := gcpfake.New(t)
	defer s.Close()
	s.FailCAI(403, "PERMISSION_DENIED", "no")
	// A type whose list response calls its array "widgets", as most of GCP
	// does, rather than compute's "items".
	c := widgetCatalog()
	c.Types[0].ListField = "widgets"
	s.SetListField("/v1/projects/p/locations/r/widgets", "widgets")
	s.Seed("/v1/projects/p/locations/r/widgets/one", map[string]any{"name": "one"})
	p := testProviderWithCatalog(t, s, c)
	p.settings.DiscoverTypes = []string{"gcp.widget"}

	got, err := p.Discover(context.Background(), provider.DiscoverRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("found %d resources; the type's own list field was not read", len(got))
	}
}

// TestATypeWithNoListFieldIsSkippedAndSaidSo. Empty means the generator
// found no list method or no usable array property; guessing one would read
// nothing and report the type as empty.
func TestATypeWithNoListFieldIsSkipped(t *testing.T) {
	gcptest.Isolate(t)
	s := gcpfake.New(t)
	defer s.Close()
	s.FailCAI(403, "PERMISSION_DENIED", "no")
	c := widgetCatalog()
	c.Types[0].ListField = ""
	s.Seed("/v1/projects/p/locations/r/widgets/one", map[string]any{"name": "one"})
	p := testProviderWithCatalog(t, s, c)
	p.settings.DiscoverTypes = []string{"gcp.widget"}

	got, err := p.Discover(context.Background(), provider.DiscoverRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("discovered %+v from a type with no list field", got)
	}
	for _, r := range s.Requests() {
		if r.Method == http.MethodGet && strings.HasSuffix(r.Path, "/widgets") {
			t.Error("the collection was listed even though nothing could be read from the response")
		}
	}
}

// TestAnEmptyCollectionIsNotAnError. The fallback scans many types and finds
// nothing for most of them, so empty is the common case: a 200 with no
// results, never a 404.
func TestAnEmptyCollectionDiscoversNothingAndSucceeds(t *testing.T) {
	gcptest.Isolate(t)
	s := gcpfake.New(t)
	defer s.Close()
	s.FailCAI(403, "PERMISSION_DENIED", "no")
	s.DeclareCollection("/v1/projects/p/locations/r/widgets")
	p := testProviderWithCatalog(t, s, widgetCatalog())
	p.settings.DiscoverTypes = []string{"gcp.widget"}

	got, err := p.Discover(context.Background(), provider.DiscoverRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("discovered %+v from an empty collection", got)
	}
}

// TestOneUnscannableProjectDoesNotHideTheOthers. Failing the whole call for
// one project nobody has access to would make every resource in every other
// project invisible.
func TestDiscoveryFailsOpenPerProject(t *testing.T) {
	gcptest.Isolate(t)
	s := gcpfake.New(t)
	defer s.Close()
	s.SeedCAI("projects/good", []gcpfake.Asset{
		{AssetType: "tiny.googleapis.com/Widget", Name: "//tiny.googleapis.com/projects/good/locations/r/widgets/one"},
	})
	p := testProviderWithCatalog(t, s, widgetCatalog())
	// "bad" has no CAI assets and no collection declared, so its fallback
	// list 404s: the whole project fails to scan.
	p.settings.DiscoverProjects = []string{"bad", "good"}
	p.settings.DiscoverTypes = []string{"gcp.widget"}

	got, err := p.Discover(context.Background(), provider.DiscoverRequest{})
	if err != nil {
		t.Fatalf("one unscannable project failed the whole discovery: %v", err)
	}
	if len(got) != 1 || got[0].ProviderID != "projects/good/locations/r/widgets/one" {
		t.Fatalf("discovered %+v", got)
	}
}

// TestTheRequestedTypeFilterIsApplied. DiscoverRequest.Types narrows the
// scan; the provider applies it itself because answering only for the types
// asked for is fewer calls than fetching everything and discarding most.
func TestDiscoverAppliesTheRequestedTypeFilter(t *testing.T) {
	gcptest.Isolate(t)
	s := gcpfake.New(t)
	defer s.Close()
	s.SeedCAI("projects/p", []gcpfake.Asset{
		{AssetType: "compute.googleapis.com/Autoscaler",
			Name: "//compute.googleapis.com/projects/p/zones/us-central1-a/autoscalers/zonal-one"},
		{AssetType: "compute.googleapis.com/Autoscaler",
			Name: "//compute.googleapis.com/projects/p/regions/us-central1/autoscalers/regional-one"},
	})
	p := testProvider(t, s)

	got, err := p.Discover(context.Background(), provider.DiscoverRequest{Types: []string{"gcp.regionautoscaler"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Type != "gcp.regionautoscaler" {
		t.Fatalf("the filter let through %+v", got)
	}
	// The filter also goes to the API, as CAI's own assetTypes parameter.
	var sawFilter bool
	for _, r := range s.Requests() {
		if strings.HasSuffix(r.Path, ":searchAllResources") &&
			len(r.Query["assetTypes"]) == 1 && r.Query["assetTypes"][0] == "compute.googleapis.com/Autoscaler" {
			sawFilter = true
		}
	}
	if !sawFilter {
		t.Error("the search did not carry an assetTypes filter, so CAI returned the whole project")
	}
}

// TestTheFallbackListDefaultsToTheOverlaysList. discover_default is the only
// thing the fallback has to scan when nothing else names a type, and it
// reaches the runtime through the catalog because the overlay is a
// generator-time input.
func TestTheFallbackDefaultsToTheCatalogsDiscoverDefault(t *testing.T) {
	c := mustCatalog(t)
	if len(c.DiscoverDefault) == 0 {
		t.Fatal("the catalog carries no discover_default, so the fallback would scan nothing at all")
	}
	for _, name := range c.DiscoverDefault {
		ty, ok := c.Type(name)
		if !ok {
			t.Errorf("discover_default names %q, which the catalog does not serve", name)
			continue
		}
		if ty.ListField == "" {
			t.Errorf("discover_default names %q, which has no list field and so can never be scanned", name)
		}
	}
}

// TestTheFallbackScansTheCatalogsDefaultListWhenNothingNamesTypes. With no
// request filter and no configured types, discover_default is the only thing
// the fallback has to go on -- and it reaches the runtime only because the
// generator copies it onto the catalog. Without this, a generator that
// stopped copying it would leave every fallback scan reporting an empty
// project, and nothing in this package would notice.
func TestTheFallbackScansTheCatalogsDefaultList(t *testing.T) {
	gcptest.Isolate(t)
	s := gcpfake.New(t)
	defer s.Close()
	s.FailCAI(403, "PERMISSION_DENIED", "no")
	s.Seed("/v1/projects/p/locations/r/widgets/one", map[string]any{"name": "one"})
	c := widgetCatalog()
	c.DiscoverDefault = []string{"gcp.widget"}
	p := testProviderWithCatalog(t, s, c)
	// Deliberately neither: this is the path that has nothing but the
	// catalog's own list.
	p.settings.DiscoverTypes = nil

	got, err := p.Discover(context.Background(), provider.DiscoverRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Type != "gcp.widget" {
		t.Fatalf("the fallback scanned %+v; discover_default was not consulted", got)
	}
}
