package gcprov

// Shared test fixtures for this package, Tasks 11-16 (pinned here per the
// task-11 brief so later tasks do not each invent their own): testProvider,
// testProviderWithCatalog, widgetCatalog, widgetType, attrs, attrsMixed,
// mapValue, listValue, contains, staticToken.

import (
	"fmt"
	"strings"
	"testing"

	"golang.org/x/oauth2"

	"github.com/infrena/infrena-provider-gcp/internal/catalog"
	"github.com/infrena/infrena-provider-gcp/internal/gcpfake"
	"github.com/infrena/infrena/pkg/value"
)

// staticToken returns a TokenSource that never touches the real oauth2/google
// ADC chain -- every test still calls gcptest.Isolate too (belt and braces:
// some later task's code path may resolve credentials some other way), but
// nothing that uses staticToken needs Isolate to avoid reaching real GCP.
func staticToken() oauth2.TokenSource {
	return oauth2.StaticTokenSource(&oauth2.Token{AccessToken: "test-token", TokenType: "Bearer"})
}

// attrs builds string-only resolved attributes, for url-template tests.
func attrs(kv map[string]string) map[string]value.Value {
	out := make(map[string]value.Value, len(kv))
	for k, v := range kv {
		out[k] = value.String(v, value.SourceExplicit)
	}
	return out
}

// attrsMixed builds resolved attributes from ordinary Go values: string,
// int, int64, float64, bool, map[string]any (nested, via mapValue) and
// []string/[]any.
func attrsMixed(kv map[string]any) map[string]value.Value {
	out := make(map[string]value.Value, len(kv))
	for k, v := range kv {
		out[k] = anyToValue(v)
	}
	return out
}

// mapValue builds a known KindMap value.Value from plain Go values -- the
// nested-attribute fixtures Task 14 (patch masks) and Task 15 (reconcile)
// compare against.
func mapValue(kv map[string]any) value.Value {
	entries := make(map[string]value.Value, len(kv))
	for k, v := range kv {
		entries[k] = anyToValue(v)
	}
	return value.Map(entries, value.SourceExplicit)
}

// listValue builds a known KindList value.Value of strings.
func listValue(items ...string) value.Value {
	vals := make([]value.Value, len(items))
	for i, s := range items {
		vals[i] = value.String(s, value.SourceExplicit)
	}
	return value.List(vals, value.SourceExplicit)
}

// anyToValue is attrsMixed and mapValue's shared conversion. It panics on an
// unsupported type rather than silently dropping a fixture value wrong,
// since it is only ever reachable from test code building its own input.
func anyToValue(v any) value.Value {
	switch x := v.(type) {
	case string:
		return value.String(x, value.SourceExplicit)
	case int:
		return value.Int(int64(x), value.SourceExplicit)
	case int64:
		return value.Int(x, value.SourceExplicit)
	case float64:
		return value.Float(x, value.SourceExplicit)
	case bool:
		return value.Bool(x, value.SourceExplicit)
	case map[string]any:
		return mapValue(x)
	case []string:
		items := make([]any, len(x))
		for i, s := range x {
			items[i] = s
		}
		return anyToValue(items)
	case []any:
		vals := make([]value.Value, len(x))
		for i, e := range x {
			vals[i] = anyToValue(e)
		}
		return value.List(vals, value.SourceExplicit)
	default:
		panic(fmt.Sprintf("gcprov test helpers: anyToValue: unsupported type %T", v))
	}
}

// contains is strings.Contains under a short local name, so an assertion
// like contains(err.Error(), "zone") reads as prose without importing
// strings into every test file that wants one check.
func contains(s, substr string) bool { return strings.Contains(s, substr) }

// widgetType is the one synthetic resource type Tasks 12-16 share: simple
// enough to hand-verify, but exercising the same url/await/patch shapes a
// real catalog type does.
//
// base_url is the COLLECTION path (POST target for create, GET target for
// list), never including {{name}} -- verified against the real embedded
// catalog while building this fixture: of its 233 types, only one
// (gcp.attestor) puts {{name}} in base_url at all, and every other type's
// base_url is collection-shaped. self_link is the ITEM's relative resource
// name for provider-id purposes. The per-item HTTP path CRUD code needs for
// Read/Update/Delete is expected to come from expanding base_url and
// appending the id's last segment (exactly how gcpfake itself resolves a
// create's storage path: collection + "/" + id). See task-11-report.md.
//
// NEITHER template carries the "v1" segment: it lives in path_prefix, the
// one place the version is allowed to be, and absURL puts it back in front
// of every request url. This fixture used to spell base_url "v1/projects/..."
// and self_link "projects/...", which is precisely the split Task 13a
// removed -- the fake serves whatever path it is handed, so the fixture
// agreed with the defect instead of catching it. Every wire path the
// task-11-through-16 fixtures pin ("/v1/projects/p/locations/r/widgets/one")
// is unchanged by the move.
func widgetType() *catalog.Type {
	return &catalog.Type{
		Name:           "gcp.widget",
		Service:        "widgets",
		PathPrefix:     "v1/",
		BaseURL:        "projects/{{project}}/locations/{{region}}/widgets",
		SelfLink:       "projects/{{project}}/locations/{{region}}/widgets/{{name}}",
		ImportFormat:   "projects/{project}/locations/{region}/widgets/{name}",
		AssetType:      "tiny.googleapis.com/Widget",
		Scope:          catalog.ScopeRegional,
		Await:          catalog.AwaitNone,
		TimeoutSeconds: 30,
		UpdateVerb:     "PATCH",
		UpdateMask:     true,
		ListField:      "items",
		Attributes: map[string]*catalog.Attr{
			"project": {Canonical: "project", Kind: value.KindString, Required: true, ForceNew: true},
			"region":  {Canonical: "region", Kind: value.KindString, Required: true, ForceNew: true},
			// NOT ForceNew, deliberately, and that is what the real corpus
			// looks like: 10 of the 86 updatable types declare `name` as
			// required, mutable AND consumed by their url (gcp.mesh,
			// gcp.authzpolicy, gcp.packetmirroring and seven others). While
			// all three of this fixture's url-named attributes were also
			// ForceNew, TestAUrlIdentifyingAttributeIsNeverInTheMask could not
			// tell "excluded because the url says it" from "excluded because
			// it is immutable" -- and BuildMask implements the first rule,
			// not the second.
			"name":   {Canonical: "name", Kind: value.KindString, Required: true},
			"sizeGb": {Canonical: "sizeGb", Kind: value.KindInt},
			"tier":   {Canonical: "tier", Kind: value.KindString},
			"createTime": {
				Canonical: "createTime", Kind: value.KindString, Output: true,
			},
			"config": {
				Canonical: "config", Kind: value.KindMap,
				Fields: map[string]*catalog.Attr{
					"mode": {Canonical: "mode", Kind: value.KindString},
					"size": {Canonical: "size", Kind: value.KindInt},
				},
			},
		},
	}
}

// widgetCatalog wraps widgetType in a one-type Catalog.
func widgetCatalog() *catalog.Catalog {
	return &catalog.Catalog{Types: []*catalog.Type{widgetType()}}
}

// mustCatalog loads the real embedded catalog, failing the test rather than
// silently testing against nothing.
func mustCatalog(t *testing.T) *catalog.Catalog {
	t.Helper()
	c, err := catalog.Load()
	if err != nil {
		t.Fatalf("the embedded catalog will not load: %v", err)
	}
	return c
}

// pointCatalogAt returns a shallow copy of c with every type's ApiBaseURL
// rewritten to base, so a Client built against it reaches the fake server
// rather than the real googleapis.com host baked in at generation time.
//
// A copy, never a mutation of c in place: catalog.Load() caches one instance
// for the whole process ("Once, because every instance of the plugin serves
// the same types" -- internal/catalog/embed.go), so rewriting it directly
// would leak one test's fake server url into every other test that calls
// catalog.Load() afterwards, including ones running concurrently.
func pointCatalogAt(c *catalog.Catalog, base string) *catalog.Catalog {
	types := make([]*catalog.Type, len(c.Types))
	for i, t := range c.Types {
		cp := *t
		cp.APIBaseURL = base
		types[i] = &cp
	}
	// DiscoverDefault travels too: discovery's fallback reads its type list
	// off the catalog, so a copy that dropped it would make every fallback
	// test scan nothing while looking like it had scanned everything.
	return &catalog.Catalog{
		Generated:       c.Generated,
		MMV1Commit:      c.MMV1Commit,
		Types:           types,
		DiscoverDefault: c.DiscoverDefault,
	}
}

// testProvider builds a Provider against the real embedded catalog, wired to
// the fake server s -- for tests that need a type the real catalog ships
// (e.g. gcp.tagbinding) rather than the synthetic widget.
func testProvider(t *testing.T, s *gcpfake.Server) *Provider {
	t.Helper()
	return testProviderWithCatalog(t, s, mustCatalog(t))
}

// testProviderWithCatalog builds a Provider against an explicit catalog
// (typically widgetCatalog()), wired to the fake server s.
//
// project "p" and region "r" are fixed: every task-12-through-16 example
// fixture pins its own paths to them (e.g.
// "/v1/projects/p/locations/r/widgets/one"), so a helper picking different
// defaults would make every one of those examples wrong.
func testProviderWithCatalog(t *testing.T, s *gcpfake.Server, c *catalog.Catalog) *Provider {
	t.Helper()
	base := s.URL() + "/"
	return &Provider{
		client:  NewClient(staticToken(), base, ClientOptions{}),
		catalog: pointCatalogAt(c, base),
		settings: Settings{
			Project:          "p",
			Region:           "r",
			DiscoverProjects: []string{"p"},
			// Cloud Asset Inventory is the one API a Provider calls that has
			// no catalog type to take a host from, so pointCatalogAt cannot
			// redirect it and a discovery test would otherwise search the
			// real cloudasset.googleapis.com. See Settings.AssetInventoryBaseURL.
			AssetInventoryBaseURL: base,
		},
	}
}
