package catalog

import (
	"gopkg.in/yaml.v3"
	"os"
	"testing"

	"github.com/infrena/infrena/pkg/schema"
)

// TestTheRealCatalogIsOneInfrenaAccepts runs infrena's own validator over every
// generated definition. A catalog that fails here is one the host refuses at load,
// and the failure would otherwise surface as an unexplained plugin error.
//
// The floor is 200, not a round guess: the real catalog measured 233 types on
// 2026-09-22, against the 41-API want-list in scripts/fetch-schemas and the
// rulings in gen/overlay.yaml as they stood that day (see the Task 9 report
// for the full breakdown). 200 is comfortably below that — enough to catch
// "a whole API stopped fetching" or "the naming pass broke", not so close to
// 233 that ordinary week-to-week drift in Google's own Discovery documents
// trips it. It is NOT the ~500 an earlier draft of the spec guessed before
// anything had been generated against the real corpus; that guess is wrong
// and is being corrected separately.
func TestTheRealCatalogIsOneInfrenaAccepts(t *testing.T) {
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Types) < 200 {
		t.Fatalf("catalog has %d types; measured 233 on 2026-09-22, so anything below 200 means something broke", len(c.Types))
	}
	if err := schema.ValidateAll(c.Definitions()); err != nil {
		t.Fatalf("infrena refuses the generated catalog: %v", err)
	}
}

// TestEveryTypeCarriesWhatTheRuntimeNeeds. A type with no base URL, no await
// decision or no scope is one the runtime cannot serve, and shipping it means
// an error at apply rather than at generation.
func TestEveryTypeCarriesWhatTheRuntimeNeeds(t *testing.T) {
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	for _, ty := range c.Types {
		if ty.APIBaseURL == "" || ty.BaseURL == "" {
			t.Errorf("%s has no URL to call: api=%q base=%q", ty.Name, ty.APIBaseURL, ty.BaseURL)
		}
		if ty.TimeoutSeconds <= 0 {
			t.Errorf("%s has no timeout", ty.Name)
		}
		// A type that awaits an operation must know HOW to poll it. Compute-style
		// types need either a wait path or a poll path — 58 of the 65 publish a
		// wait, and the other 7 (sqladmin, container) publish no wait method at
		// all and must be polled with get. Longrunning types always need a poll
		// path. An earlier version of this check asserted on OperationScope, a
		// bare word that no longer exists precisely because every attempt to
		// build a URL from it was wrong.
		if ty.Await == AwaitComputeOperation && ty.OperationWaitPath == "" && ty.OperationPollPath == "" {
			t.Errorf("%s awaits a compute operation but has neither a wait nor a poll path", ty.Name)
		}
		if ty.Await == AwaitLongRunning && ty.OperationPollPath == "" {
			t.Errorf("%s awaits a long-running operation but has no poll path", ty.Name)
		}
	}
}

// TestAKnownMagicModulesTypeCarriesAnImportFormat. Nothing in
// TestEveryTypeCarriesWhatTheRuntimeNeeds above checks ImportFormat: it is
// legitimately empty for most types (only magic-modules resources that
// declare import_format have one at all, and Capabilities.Import is derived
// FROM it, so a build.go regression that always leaves it empty is
// internally consistent and passes schema.ValidateAll too — Task 9's own
// sabotage step confirmed this the hard way). gcp.storage.bucket is a fixed
// point: it is magic-modules-backed and its import_format is '{{name}}', so
// this fails if buildType ever again stops copying it over.
func TestAKnownMagicModulesTypeCarriesAnImportFormat(t *testing.T) {
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	ty, ok := c.Type("gcp.storage.bucket")
	if !ok {
		t.Fatal("gcp.storage.bucket missing; pick a different fixed point if this type is ever renamed")
	}
	if ty.ImportFormat == "" {
		t.Error("gcp.storage.bucket has no import_format, but magic-modules gives it one")
	}
}

// TestEveryReferencePointsAtATypeWeServe. A dangling edge would make
// `import --generate` write a ${ref} to a type the catalog has never heard of,
// producing a compile error in a file the user never wrote.
func TestEveryReferencePointsAtATypeWeServe(t *testing.T) {
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	var walk func(tyName string, attrs map[string]*Attr)
	walk = func(tyName string, attrs map[string]*Attr) {
		for name, a := range attrs {
			if a.Ref != nil {
				if _, ok := c.Type(a.Ref.Type); !ok {
					t.Errorf("%s.%s references %q, which is not in the catalog", tyName, name, a.Ref.Type)
				}
			}
			walk(tyName, a.Fields)
		}
	}
	for _, ty := range c.Types {
		walk(ty.Name, ty.Attributes)
	}
}

// TestTheTagBindingRulingTookEffect — G6's worked example, end to end.
func TestTheTagBindingRulingTookEffect(t *testing.T) {
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"gcp.tagkey", "gcp.tagvalue", "gcp.tagbinding"} {
		if _, ok := c.Type(n); !ok {
			t.Errorf("%s missing; decision G6 requires all three at v1.0", n)
		}
	}
	tb, _ := c.Type("gcp.tagbinding")
	if tb == nil {
		return
	}
	for name, a := range tb.Attributes {
		if a.Output {
			continue
		}
		if !a.ForceNew {
			t.Errorf("gcp.tagbinding.%s is not ForceNew, but the type has no patch method", name)
		}
	}
}

// TestDiscoverDefaultNamesOnlyTypesWeServe stops the overlay's fallback list
// rotting against the catalog.
//
// It listed gcp.network and gcp.subnetwork, which do not exist at all, and
// gcp.instance and gcp.bucket, which ship under qualified names. Nothing
// noticed for nine tasks because nothing read the list until Task 16's fallback
// discovery — at which point it would have scanned names no type has and
// reported an empty project rather than a broken list.
func TestDiscoverDefaultNamesOnlyTypesWeServe(t *testing.T) {
	data, err := os.ReadFile("../../gen/overlay.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var overlay struct {
		DiscoverDefault []string `yaml:"discover_default"`
	}
	if err := yaml.Unmarshal(data, &overlay); err != nil {
		t.Fatal(err)
	}
	if len(overlay.DiscoverDefault) == 0 {
		t.Fatal("discover_default is empty, so the fallback path would scan nothing")
	}
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range overlay.DiscoverDefault {
		if _, ok := c.Type(name); !ok {
			t.Errorf("discover_default names %q, which the catalog does not serve", name)
		}
	}
}
