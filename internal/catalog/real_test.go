package catalog

import (
	"os"
	"regexp"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

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

// TestNoStoredTemplateCarriesAnAPIVersion. The absolute URL is APIBaseURL +
// PathPrefix + expand(template), for every type, with no special cases. That
// holds only if no stored template carries an API version segment of its own
// -- otherwise the version is either doubled or (for the 72 types measured on
// 2026-09-22) missing entirely, and every call 404s.
//
// OperationPollPath and OperationWaitPath are deliberately exempt: both are
// taken verbatim from the API's own operations methods and composed directly
// against APIBaseURL, which is correct for every type that has one.
func TestNoStoredTemplateCarriesAnAPIVersion(t *testing.T) {
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	version := regexp.MustCompile(`^v[0-9][0-9a-zA-Z]*$`)
	leadsWithVersion := func(tmpl string) bool {
		if tmpl == "" {
			return false
		}
		return version.MatchString(strings.SplitN(strings.TrimPrefix(tmpl, "/"), "/", 2)[0])
	}
	for _, ty := range c.Types {
		for label, tmpl := range map[string]string{
			"base_url": ty.BaseURL, "self_link": ty.SelfLink,
			"create_url": ty.CreateURL, "update_url": ty.UpdateURL,
			"delete_url": ty.DeleteURL, "import_format": ty.ImportFormat,
		} {
			if leadsWithVersion(tmpl) {
				t.Errorf("%s: %s starts with an API version (%q); the version belongs in PathPrefix",
					ty.Name, label, tmpl)
			}
		}
	}
}

// TestEveryTypeComposesAVersionedURL. A type whose composed URL carries no
// version at all is one whose every call 404s. 72 of 233 were in this state
// when the field was introduced.
func TestEveryTypeComposesAVersionedURL(t *testing.T) {
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	version := regexp.MustCompile(`(^|/)v[0-9][0-9a-zA-Z]*(/|$)`)
	for _, ty := range c.Types {
		composed := ty.APIBaseURL + ty.PathPrefix
		if !version.MatchString(composed) {
			t.Errorf("%s composes %q, which names no API version", ty.Name, composed)
		}
	}
}

// TestNoShippedTypeReadsAnotherCollectionThanItCreatesIn. self_link is the
// provider-id template and base_url (or create_url) is where a create POSTs.
// If the id is not inside the collection the create posted to, the type
// creates one resource and addresses another: the resource exists at the url
// the POST went to, while the id that gets stored -- and that every later
// Read, Update and Delete uses -- names something else. Every create of such
// a type orphans.
//
// gcp.vpngateway was exactly that, posting to compute's targetVpnGateways
// (classic VPN) while its id named vpnGateways (HA VPN), and it was the only
// one of the 233: the generator had paired a magic-modules base_url with a
// Discovery get path from a different collection. internal/gen refuses to
// build such a type now; this is the assertion that the shipped catalog has
// none, wherever a future one might come from.
//
// A template carrying a reserved "{+x}" capture is exempt, and that is not a
// loophole: such a capture swallows any number of segments, so two of them
// cannot disagree about a path neither one spells. 88 of the 233 are in that
// position, and the runtime is where they get checked -- against the id GCP
// actually answers with, at create time (outsideCreatedCollection,
// internal/gcprov/crud.go).
func TestNoShippedTypeReadsAnotherCollectionThanItCreatesIn(t *testing.T) {
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	placeholder := regexp.MustCompile(`\{\{[^{}]+\}\}|\{[^{}]+\}`)
	shape := func(tmpl string) string { return placeholder.ReplaceAllString(tmpl, "\x00") }
	var compared int
	for _, ty := range c.Types {
		coll := ty.CreateURL
		if coll == "" {
			coll = ty.BaseURL
		}
		if i := strings.IndexByte(coll, '?'); i >= 0 {
			coll = coll[:i]
		}
		coll = strings.TrimSuffix(coll, "/")
		if coll == "" || ty.SelfLink == "" ||
			strings.Contains(coll, "{+") || strings.Contains(ty.SelfLink, "{+") {
			continue
		}
		compared++
		self, want := shape(ty.SelfLink), shape(coll)
		if self != want && !strings.HasPrefix(self, want+"/") {
			t.Errorf("%s creates in %q but its id names %q, so every create orphans",
				ty.Name, coll, ty.SelfLink)
		}
	}
	// 144 of the 233 types were comparable on 2026-09-22 (the rest carry a
	// whole-path capture on one side or the other). A run that compared
	// nothing would pass this test while asserting nothing at all.
	if compared < 100 {
		t.Errorf("only %d types had two comparable templates; 144 did on 2026-09-22, so this test is no longer asserting what it says", compared)
	}
}
