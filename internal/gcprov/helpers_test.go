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
// name for provider-id purposes and never carries a "v1"-style version
// segment, also matching the real catalog. The per-item HTTP path CRUD code
// needs for Read/Update/Delete is therefore expected to come from expanding
// base_url and appending the id's last segment (exactly how gcpfake itself
// resolves a create's storage path: collection + "/" + id) -- not from
// expanding self_link directly, which would produce an url missing the
// version segment base_url carries. See task-11-report.md.
func widgetType() *catalog.Type {
	return &catalog.Type{
		Name:           "gcp.widget",
		Service:        "widgets",
		BaseURL:        "v1/projects/{{project}}/locations/{{region}}/widgets",
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
			"name":    {Canonical: "name", Kind: value.KindString, Required: true, ForceNew: true},
			"sizeGb":  {Canonical: "sizeGb", Kind: value.KindInt},
			"tier":    {Canonical: "tier", Kind: value.KindString},
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
	return &catalog.Catalog{Generated: c.Generated, MMV1Commit: c.MMV1Commit, Types: types}
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
		client:           NewClient(staticToken(), base, ClientOptions{}),
		catalog:          pointCatalogAt(c, base),
		project:          "p",
		region:           "r",
		discoverProjects: []string{"p"},
	}
}
