package catalog

import (
	"os"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"
	"unicode"

	"gopkg.in/yaml.v3"

	"github.com/infrena/infrena/pkg/schema"
	"github.com/infrena/infrena/pkg/value"
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

	// And it must actually REACH the runtime. The overlay is a
	// generator-time input; discovery's fallback reads the list off the
	// catalog it already loads, so a generator that stopped copying it would
	// leave the fallback with nothing to scan and report every project as
	// empty, while this test's loop above went on passing.
	if len(c.DiscoverDefault) != len(overlay.DiscoverDefault) {
		t.Fatalf("the catalog carries %d discover_default entries, the overlay names %d",
			len(c.DiscoverDefault), len(overlay.DiscoverDefault))
	}
	for i, name := range overlay.DiscoverDefault {
		if c.DiscoverDefault[i] != name {
			t.Errorf("discover_default[%d] is %q in the catalog and %q in the overlay",
				i, c.DiscoverDefault[i], name)
		}
	}
}

// TestEveryTypeRecordsWhereItsNamesBegin. ParentRoot is what tells two
// catalog types sharing one CAI asset type apart when their url templates
// are identical, which 13 of the 27 shared asset types' are. It must be the
// literal first segment of the type's own resource names, never the singular
// lowercased form scopeSegment produces for type NAMES -- comparing
// "folders/123/..." against "folder" matches nothing, silently, and every
// resource of those types is dropped from discovery.
func TestParentRootIsARealHierarchySegment(t *testing.T) {
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	allowed := map[string]bool{
		"": true, "projects": true, "organizations": true, "folders": true, "billingAccounts": true,
	}
	counts := map[string]int{}
	for _, ty := range c.Types {
		if !allowed[ty.ParentRoot] {
			t.Errorf("%s: parent_root %q is not a hierarchy segment", ty.Name, ty.ParentRoot)
		}
		counts[ty.ParentRoot]++
	}
	if counts["projects"] == 0 || counts["folders"] == 0 || counts["organizations"] == 0 {
		t.Errorf("parent_root is not being recorded: %v", counts)
	}
	// A type whose assigned NAME says it is folder- or organization-scoped
	// must have the matching root: those are exactly the parent variants that
	// share an asset type with their project-scoped sibling, and a mismatch
	// between the two is what would put a folder's log bucket under
	// gcp.logging.bucket.
	for _, ty := range c.Types {
		for segment, root := range map[string]string{
			".folder.": "folders", ".organization.": "organizations", ".billingaccount.": "billingAccounts",
		} {
			if strings.Contains(ty.Name, segment) && ty.ParentRoot != root {
				t.Errorf("%s is named as %s-scoped but its parent_root is %q", ty.Name, root, ty.ParentRoot)
			}
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

// eachAttributeLevel calls fn once for every map of declared attributes in
// the catalog -- a Type's own Attributes, every Attr's Fields, and every
// LIST ELEMENT's Fields, reached through Elem.
//
// Through Elem deliberately. A walk that follows only Fields sees 43 of the
// corpus's 306 renamed attributes and reports gcp.grpcroute,
// gcp.responsepolicyrule and gcp.router -- three updatable types -- as
// having none at all.
func eachAttributeLevel(c *Catalog, fn func(where string, level map[string]*Attr)) {
	var walk func(where string, level map[string]*Attr)
	walk = func(where string, level map[string]*Attr) {
		fn(where, level)
		for key, a := range level {
			if len(a.Fields) > 0 {
				walk(where+"."+key, a.Fields)
			}
			if a.Elem != nil && len(a.Elem.Fields) > 0 {
				walk(where+"."+key+"[]", a.Elem.Fields)
			}
		}
	}
	for _, ty := range c.Types {
		walk(ty.Name, ty.Attributes)
	}
}

// TestTheCatalogStillRenamesAttributes. The generator renames a property
// whose name collides with something reserved -- "type" becomes
// "type_value", "provider" becomes "provider_value", "lifecycle" becomes
// "lifecycle_value" -- and internal/gcprov/names.go exists solely to
// translate between that schema name and the wire name GCP actually uses.
//
// If a regeneration ever changes that strategy, every one of those
// translation tests would still pass while testing nothing, because each of
// them iterates a set the generator controls. This is the one place that
// asserts the set is not empty, and it reports its size so a change of
// strategy is visible in the failure rather than inferred from a later bug.
//
// Measured 2026-09-25: 26 renamed attributes, 8 of them Required, every one
// at the top level. Until that day every nested keyword was renamed too (306
// on 2026-09-22, 820 by 2026-09-25), which the host never needed: it
// reserves `type`, `provider` and `lifecycle` only among a resource's own
// keys. A rename anywhere below the top level is now a regression.
func TestTheCatalogStillRenamesAttributes(t *testing.T) {
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	topLevel := map[string]bool{}
	for _, ty := range c.Types {
		topLevel[ty.Name] = true
	}
	total, required := 0, 0
	var nested []string
	eachAttributeLevel(c, func(where string, level map[string]*Attr) {
		for key, a := range level {
			if a.Canonical == key {
				continue
			}
			total++
			if a.Required {
				required++
			}
			if !topLevel[where] {
				nested = append(nested, where+"."+key)
			}
		}
	})
	t.Logf("renamed attributes: %d total, %d required, %d nested", total, required, len(nested))
	if total < 15 {
		t.Errorf("only %d attributes are renamed; 26 were measured on 2026-09-25, so the "+
			"generator's renaming strategy has changed and internal/gcprov/names.go and its "+
			"tests need revisiting", total)
	}
	if required == 0 {
		t.Errorf("no RENAMED attribute is Required any more (8 were); a create that sends the "+
			"wrong name for one of these is the difference between a rejected request and a "+
			"cosmetic diff, and %d renames remain", total)
	}
	if len(nested) != 0 {
		sort.Strings(nested)
		t.Errorf("%d renamed attributes are nested, first %s; only a top-level keyword clashes "+
			"with a resource key, and a nested rename makes users write type_value where "+
			"every API document says type", len(nested), nested[0])
	}
}

// TestNoLevelRenamesTwoAttributesOntoOneWireName is the invariant
// internal/gcprov's toSchema depends on to be a function at all: within one
// level of declared attributes, the wire name identifies the schema name
// uniquely.
//
// Two ways it could stop doing so, and both are checked. Two schema keys
// could carry the same Canonical, or a renamed key's Canonical could also be
// a sibling's own key -- "type_value" meaning "type" alongside a real
// attribute called "type". Either makes a response field ambiguous.
//
// It is pinned here rather than resolved with a tiebreak in toSchema,
// because a tiebreak would let a regenerated catalog silently pick one of
// two meanings for a field a user set; a failure at generation time is the
// cheaper place to find out. Both measured as zero on 2026-09-22.
func TestNoLevelRenamesTwoAttributesOntoOneWireName(t *testing.T) {
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	eachAttributeLevel(c, func(where string, level map[string]*Attr) {
		byWire := map[string][]string{}
		for key, a := range level {
			byWire[a.Canonical] = append(byWire[a.Canonical], key)
		}
		for wire, keys := range byWire {
			if len(keys) > 1 {
				sort.Strings(keys)
				t.Errorf("%s: %v all have the wire name %q, so a response field named %q "+
					"cannot be keyed back to one attribute", where, keys, wire, wire)
			}
		}
		for key, a := range level {
			if a.Canonical == key {
				continue
			}
			if _, clash := level[a.Canonical]; clash {
				t.Errorf("%s: %q has the wire name %q, which is also a sibling's own key; "+
					"a response field named %q is ambiguous", where, key, a.Canonical, a.Canonical)
			}
		}
	})
}

// TestTheCatalogStillMarksUnorderedLists. GCP returns a set-typed list in
// whatever order it likes, and internal/gcprov/reconcile.go reorders exactly
// the lists this flag marks -- so if a regeneration stopped setting it, every
// reconciliation test would still pass while reordering nothing, and the
// provider would go back to proposing a reordering forever for resources
// nobody touched.
//
// Measured 2026-09-22, following Fields AND Elem: 42 attributes across 22 of
// the 233 types, one of them inside a list
// (gcp.router's bgpPeers[].advertisedIpRanges). A walk that followed Fields
// alone would report 41 and miss that one.
//
// gcp.firewall's allowed and denied are named explicitly because they are the
// two that only arrived once mmIndex started indexing magic-modules' api_name
// as well as its name: magic-modules calls them "allow"/"deny" with
// api_name allowed/denied, so a lookup by Discovery's own property name found
// nothing and the is_set flag behind them was dropped on the floor.
func TestTheCatalogStillMarksUnorderedLists(t *testing.T) {
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	total, inList := 0, 0
	eachAttributeLevel(c, func(where string, level map[string]*Attr) {
		for _, a := range level {
			if !a.Unordered {
				continue
			}
			total++
			if strings.Contains(where, "[]") {
				inList++
			}
		}
	})
	t.Logf("unordered lists: %d attributes, %d inside a list", total, inList)
	if total < 30 {
		t.Errorf("only %d attributes are marked Unordered; 42 were measured on 2026-09-22 "+
			"across 22 types, so the is_set propagation in internal/gen/attrs.go has "+
			"regressed and reconciliation is reordering nothing", total)
	}
	if inList == 0 {
		t.Error("no unordered list is itself inside a list any more (one was: " +
			"gcp.router's bgpPeers[].advertisedIpRanges); reconcile.go recurses through " +
			"Elem for exactly that, and the corpus no longer covers it")
	}
	fw, ok := c.Type("gcp.firewall")
	if !ok {
		t.Fatal("the catalog no longer ships gcp.firewall")
	}
	for _, name := range []string{"allowed", "denied", "sourceRanges"} {
		a, ok := fw.Attributes[name]
		if !ok {
			t.Errorf("gcp.firewall no longer declares %q", name)
			continue
		}
		if !a.Unordered {
			t.Errorf("gcp.firewall.%s is not marked Unordered; magic-modules says is_set, and "+
				"for allowed/denied that only reaches the attribute through its api_name", name)
		}
	}
}

// TestNoTypeAdvertisesACreateItCannotPerform is the invariant that stops a
// type nobody can create from claiming it can be.
//
// The tier gate's principle is that a capability the generator cannot vouch
// for does not ship. A create url whose placeholders nothing can fill is
// exactly that: the create fails on string substitution, before a byte
// reaches Google. 38 types shipped in that state and the live suite found
// exactly ONE of them (gcp.serviceaccount), because a live suite finds what
// it exercises.
//
// The rule is the runtime's own. A create-url placeholder must be an
// instance scope setting (project, region, zone, location -- what
// gcprov.Provider.withScope actually fills in, and `parent` is deliberately
// NOT among them because nothing in the plugin's configuration supplies
// one), a stored CreateBinding, or one of the type's own SETTABLE
// attributes.
//
// The fixed points below are named rather than counted, because a count
// alone passes if the set changes while its size does not. Each is a type
// whose create was broken by a DIFFERENT cause and is fixed by a different
// part of task 18b; between them they cover every mechanism, so a
// regeneration that undoes any one of them fails here by name.
func TestNoTypeAdvertisesACreateItCannotPerform(t *testing.T) {
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	// Every type that must be creatable, and the cause that used to stop it.
	for _, tc := range []struct{ name, why string }{
		{"gcp.serviceaccount", "iam's create says {+name} and MEANS the parent project; " +
			"the binding comes from Discovery's own pattern"},
		{"gcp.bigquery.table", "{{dataset_id}} and {{table_id}} are snake_case names for " +
			"attributes NESTED at tableReference.datasetId and tableReference.tableId"},
		{"gcp.routine", "{{dataset_id}} nested at routineReference.datasetId"},
		{"gcp.objectaccesscontrol", "{{%object}}'s leading %% is an escaping instruction, " +
			"not part of the attribute's name"},
		{"gcp.sslcert", "{instance} is a url PATH PARAMETER, never a body field, so it was " +
			"in no schema until the generator declared it"},
		{"gcp.artifactregistry.rule", "{{repository_id}} and {{rule_id}}, same cause"},
		{"gcp.interceptdeployment", "{{intercept_deployment_id}} is the new resource's own " +
			"id, carried in the create url's query string and in no body"},
		{"gcp.logging.bucket", "{+parent} expands from Discovery's pattern to " +
			"projects/{project}/locations/{location}, both of them instance settings"},
		{"gcp.tagbinding", "part A's type: it must still be creatable after the id fix"},
		{"gcp.storage.bucket", "the control: it was never broken and must not become so"},
	} {
		ty, ok := c.Type(tc.name)
		if !ok {
			t.Errorf("the catalog no longer ships %s", tc.name)
			continue
		}
		if missing := ty.UnresolvedCreatePlaceholders(); len(missing) > 0 {
			t.Errorf("%s cannot build a create url %q: %v is unresolved -- %s",
				tc.name, ty.CreateTemplate(), missing, tc.why)
		}
	}

	// And the capability the host is told about must agree with the fact.
	byName := map[string]bool{}
	for _, d := range c.Definitions() {
		byName[d.Type] = d.Capabilities.Create
	}
	for _, ty := range c.Types {
		can := len(ty.UnresolvedCreatePlaceholders()) == 0
		if byName[ty.Name] != can {
			t.Errorf("%s: `infrena explain` says create=%v, but the create url %q %s",
				ty.Name, byName[ty.Name], ty.CreateTemplate(),
				map[bool]string{true: "can be built", false: "cannot be built"}[can])
		}
	}
}

// TestTheTypesThatCannotBeCreatedAreExactlyTheseOnes pins the set, by name.
//
// It is a LIST, not a count, because a count passes while the membership
// churns underneath it -- and this project has already found eight tests
// that passed while testing nothing. A regeneration that breaks a create
// that used to work fails here naming the type; a regeneration that FIXES
// one also fails, and the list is meant to be shortened deliberately, in a
// diff someone reads.
//
// Measured 2026-09-23 against the catalog this commit generates: 65 of 233
// types ship without create. They divide into three causes, all recorded in
// gen/warnings.txt:
//
//   - 56 whose create url names a PARENT the provider cannot express: an
//     organization, folder, billing account, Bigtable instance, Spanner
//     instance, Cloud Tasks queue and so on. The Discovery pattern now says
//     exactly which segment is missing (it used to say only "parent"), and
//     the value cannot be declared as an attribute because the type's
//     self_link is a bare "{+name}" -- so nothing a later Read recovers
//     would carry it back, and an attribute that does not round-trip makes
//     every plan after a successful apply propose a change.
//
//   - 6 whose create url binds the new resource's id to a `name` that
//     Discovery marks output-only while magic-modules declares it as a url
//     PARAMETER. The two sources are describing different things -- the id
//     the user chooses and the full resource name Google answers with -- and
//     resolving that disagreement is a change to what `name` MEANS for those
//     types, not a line of code here.
//
//   - 3 whose create url is "{+parent}" with a pattern that does not say
//     which hierarchy root it means ("^[^/]+/[^/]+/..."), so there is
//     nothing to expand it to.
//
// Every one of them still reads, imports, discovers and deletes; only the
// create is refused, in gcprov.Create, before any request is sent.
func TestTheTypesThatCannotBeCreatedAreExactlyTheseOnes(t *testing.T) {
	want := map[string]bool{}
	for _, n := range []string{
		"gcp.appprofile",
		"gcp.attachment",
		"gcp.authorizedview",
		"gcp.bigtableadmin.backup",
		"gcp.bigtableadmin.cluster",
		"gcp.bigtableadmin.table",
		"gcp.cloudasset.savedquery",
		"gcp.cloudresourcemanager.folder.capabilityconfig",
		"gcp.cloudresourcemanager.organization.capabilityconfig",
		"gcp.config",
		"gcp.credential",
		"gcp.feed",
		"gcp.file.snapshot",
		"gcp.iam.organization.role",
		"gcp.iam.serviceaccount.key",
		"gcp.iam.workforcepool.provider",
		"gcp.iam.workforcepool.provider.key",
		"gcp.iam.workloadidentitypool.provider",
		"gcp.iam.workloadidentitypool.provider.key",
		"gcp.logging.billingaccount.bucket",
		"gcp.logging.billingaccount.exclusion",
		"gcp.logging.billingaccount.link",
		"gcp.logging.billingaccount.savedquery",
		"gcp.logging.billingaccount.sink",
		"gcp.logging.billingaccount.view",
		"gcp.logging.folder.bucket",
		"gcp.logging.folder.exclusion",
		"gcp.logging.folder.link",
		"gcp.logging.folder.savedquery",
		"gcp.logging.folder.sink",
		"gcp.logging.folder.view",
		"gcp.logging.link",
		"gcp.logging.organization.bucket",
		"gcp.logging.organization.exclusion",
		"gcp.logging.organization.link",
		"gcp.logging.organization.savedquery",
		"gcp.logging.organization.sink",
		"gcp.logging.organization.view",
		"gcp.logging.view",
		"gcp.logicalview",
		"gcp.managedidentity",
		"gcp.materializedview",
		"gcp.namespace",
		"gcp.networksecurity.organization.addressgroup",
		"gcp.networksecurity.organization.firewallendpoint",
		"gcp.networksecurity.organization.securityprofile",
		"gcp.networksecurity.organization.securityprofilegroup",
		"gcp.networksecurity.rule",
		"gcp.osconfig.folder.policyorchestrator",
		"gcp.osconfig.organization.policyorchestrator",
		"gcp.proposal",
		"gcp.schemabundle",
		"gcp.scimtenant",
		"gcp.servicelevelobjective",
		"gcp.spanner.backup",
		"gcp.spanner.session",
		"gcp.tag",
		"gcp.task",
		"gcp.token",
		"gcp.usercred",
		"gcp.userworkloadssecret",
		"gcp.version",
	} {
		want[n] = true
	}
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, ty := range c.Types {
		if len(ty.UnresolvedCreatePlaceholders()) > 0 {
			got[ty.Name] = true
		}
	}
	for n := range got {
		if !want[n] {
			ty, _ := c.Type(n)
			t.Errorf("%s can no longer be created and used to be able to: %q needs %v",
				n, ty.CreateTemplate(), ty.UnresolvedCreatePlaceholders())
		}
	}
	for n := range want {
		if !got[n] {
			t.Errorf("%s can now be created; that is progress, so take it off this list", n)
		}
	}
	t.Logf("%d of %d shipped types cannot build a create url", len(got), len(c.Types))
}

// TestNoSiblingGroupFoldsTogether. infrena resolves an attribute spelling by
// folding case (strings.ToLower, and nothing else -- it does NOT fold
// underscores) across declared names and aliases. Two spellings in one sibling
// group that fold to the same key are an ambiguity: the host answers whichever
// the map was walked to first.
//
// infrena refuses that at load -- top-level groups since forever, nested ones
// from v0.14.2. This test exists anyway, and deliberately duplicates the host's
// rule, because the host's refusal arrives in a USER'S hands at plugin load
// while this one arrives here, when the generator changes. A collision would be
// introduced by regenerating the catalog, never by a user, so this is the right
// place to find it.
//
// It follows BOTH nesting edges. Fields is an object's attributes and Elem is a
// list's element, and a walk following only Fields misses most of the corpus:
// when Task 14a measured renamed attributes through Fields alone it found 43,
// and through both it found 306, 263 of them inside a list. Two people made
// that same omission independently.
//
// Measured 2026-09-23: 4321 sibling groups to depth 9, 0 collisions.
func TestNoSiblingGroupFoldsTogether(t *testing.T) {
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	groups := 0
	var check func(tyName, path string, as map[string]*Attr)
	check = func(tyName, path string, as map[string]*Attr) {
		if len(as) == 0 {
			return
		}
		groups++
		claimed := map[string]string{} // folded -> the spelling that claimed it
		for name, a := range as {
			for _, spelling := range append([]string{name}, a.Aliases...) {
				folded := strings.ToLower(spelling)
				if first, taken := claimed[folded]; taken && first != spelling {
					t.Errorf("%s: %s%q and %s%q fold to the same name, so configuration naming it "+
						"reaches whichever was walked to first", tyName, path, first, path, spelling)
				}
				claimed[folded] = spelling
			}
		}
		for name, a := range as {
			check(tyName, path+name+".", a.Fields)
			if a.Elem != nil {
				check(tyName, path+name+"[].", a.Elem.Fields)
			}
		}
	}
	for _, ty := range c.Types {
		check(ty.Name, "", ty.Attributes)
	}
	// Non-vacuity: this walk must actually reach the nested corpus. If a future
	// change stopped it descending, every assertion above would pass by never
	// running. 4321 groups were measured; 2000 is a floor with room for drift.
	if groups < 2000 {
		t.Errorf("walked only %d sibling groups; the catalog had 4321 on 2026-09-23, so this "+
			"test is no longer reaching the nested attributes it exists to check", groups)
	}
}

// TestDefinitionsCarryListElements. Protocol 6 added `elem` to a schema
// attribute, and it is the only way to describe what a list's items look like:
// Fields means "this map's known keys", which a list has none of.
//
// Emitting it is OUR half of the contract and nothing else asserts it. Before
// this existed the host saw a bare KindList for 1901 attributes, so every key
// inside a repeated block was unreachable: 5841 of this catalog's aliases did
// not resolve, an unknown key was not reported, and both failures were silent.
//
// Asserted against the real catalog rather than a fixture, because the point is
// that the GENERATOR produces elements the host accepts, and asserted with a
// count floor so it cannot pass by finding none.
func TestDefinitionsCarryListElements(t *testing.T) {
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	withElem, withElemFields, elemAliases := 0, 0, 0
	var walk func(map[string]schema.Attribute)
	walk = func(as map[string]schema.Attribute) {
		for _, a := range as {
			if a.Elem != nil {
				withElem++
				if a.Elem.Kind == 0 {
					t.Errorf("an element carries no Kind, which the host refuses outright")
				}
				if len(a.Elem.Fields) > 0 {
					withElemFields++
				}
				for _, f := range a.Elem.Fields {
					elemAliases += len(f.Aliases)
				}
				walk(a.Elem.Fields)
			}
			walk(a.Fields)
		}
	}
	for _, d := range c.Definitions() {
		walk(d.Attributes)
	}
	t.Logf("elements reaching the host: %d (%d carrying their own fields), element-field aliases: %d",
		withElem, withElemFields, elemAliases)
	// Measured 2026-09-23: 1901 elements, 1096 of them maps with fields.
	if withElem < 1500 {
		t.Errorf("only %d list elements reached the host; 1901 were measured, so this is no longer "+
			"exercising the conversion it exists to check", withElem)
	}
	if elemAliases == 0 {
		t.Error("no element field carries an alias, so the spellings inside a repeated block " +
			"resolve to nothing and protocol 6 bought us nothing")
	}
}

// TestNoAliasIsAMangledAcronym. snake() used to put an underscore before every
// capital, which shattered every acronym GCP uses: IPProtocol became
// "i_p_protocol", natIP "nat_i_p", IPv4Range "i_pv4_range". Twelve aliases were
// wrong that way, on attributes people write -- IPProtocol is how a firewall
// rule names its protocol.
//
// A one-letter word in a snake_case name is the signature. It is not a perfect
// rule, and the corpus DOES hold legitimate instances -- dataproc's
// sparkRJob and mainRFileUri, where the R is the R language, and BigQuery
// ML's rSquared and calculatePValues -- each a real one-letter word. They are allowed by name below rather than by loosening the
// rule, because loosening it to accept any single letter would accept every
// shattered acronym it exists to catch.
//
// (An earlier version of this comment claimed the corpus had no legitimate
// instance. It has two, and the test failed on them the first time it ran.)
//
// Caught before v0.1.0 was tagged. An alias is a compatibility commitment:
// adding one later is additive, correcting a published one is not.
func TestNoAliasIsAMangledAcronym(t *testing.T) {
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	oneLetter := regexp.MustCompile(`(^|_)[a-z](_|$)`)
	// Real one-letter words, not shattered acronyms. See the doc comment.
	// r_squared and calculate_p_values arrived with gcp.bigquery.job on
	// 2026-09-24: R-squared and p-values, statistics' own one-letter words.
	allowed := map[string]bool{"spark_r_job": true, "main_r_file_uri": true,
		"r_squared": true, "calculate_p_values": true}
	checked := 0
	var walk func(tyName, path string, as map[string]*Attr)
	walk = func(tyName, path string, as map[string]*Attr) {
		for name, a := range as {
			for _, al := range a.Aliases {
				checked++
				if oneLetter.MatchString(al) && !allowed[al] {
					t.Errorf("%s %s%s has alias %q, which looks like a shattered acronym",
						tyName, path, name, al)
				}
			}
			walk(tyName, path+name+".", a.Fields)
			if a.Elem != nil {
				walk(tyName, path+name+"[].", a.Elem.Fields)
			}
		}
	}
	for _, ty := range c.Types {
		walk(ty.Name, "", ty.Attributes)
	}
	// Non-vacuity: 11,680 aliases were measured on 2026-09-23, and a walk that
	// stopped descending would pass by checking almost nothing.
	if checked < 8000 {
		t.Errorf("only %d aliases were checked; the catalog had 11,680, so this walk is no "+
			"longer reaching the attributes it exists to check", checked)
	}
}

// TestTheCuratedAliasesAreStillThere. These are chosen by hand, in
// gen/overlay.yaml, for attributes people write constantly where the mechanical
// snake_case is not the word anyone says: nobody asks for an ip_cidr_range.
//
// They are pinned because a regeneration that silently stopped applying the
// overlay would leave the catalog looking fine and quietly drop spellings users
// had been told to write.
func TestTheCuratedAliasesAreStillThere(t *testing.T) {
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range []struct{ ty, attr, alias string }{
		{"gcp.subnetwork", "ipCidrRange", "cidr"},
		{"gcp.network", "autoCreateSubnetworks", "auto_subnets"},
		{"gcp.firewall", "sourceRanges", "sources"},
		{"gcp.firewall", "destinationRanges", "destinations"},
		{"gcp.firewall", "targetTags", "targets"},
		{"gcp.compute.instance", "machineType", "machine"},
		{"gcp.storage.bucket", "storageClass", "class"},
		{"gcp.container.cluster", "initialNodeCount", "node_count"},
	} {
		ty, ok := c.Type(w.ty)
		if !ok {
			t.Errorf("%s is no longer in the catalog, so its curated aliases are gone with it", w.ty)
			continue
		}
		a := ty.Attributes[w.attr]
		if a == nil {
			t.Errorf("%s has no attribute %q; the overlay names one that does not exist, "+
				"which applies silently and gives the user nothing", w.ty, w.attr)
			continue
		}
		if !slices.Contains(a.Aliases, w.alias) {
			t.Errorf("%s.%s aliases = %v, missing the curated %q", w.ty, w.attr, a.Aliases, w.alias)
		}
		// The mechanical spelling must survive alongside it: a curated alias
		// ADDS a word, it does not replace one.
		if auto := snakeOf(w.attr); auto != w.alias && !slices.Contains(a.Aliases, auto) {
			t.Errorf("%s.%s lost its generated alias %q when the curated one was added",
				w.ty, w.attr, auto)
		}
	}
}

// snakeOf mirrors the generator's own conversion closely enough to spot a
// dropped alias. It is deliberately not imported from internal/gen: this test
// exists to check the catalog, and sharing the function would make it agree
// with the generator by construction rather than by observation.
func snakeOf(s string) string {
	var b strings.Builder
	rs := []rune(s)
	for i, r := range rs {
		if unicode.IsUpper(r) {
			if i > 0 && !unicode.IsUpper(rs[i-1]) {
				b.WriteByte('_')
			}
			b.WriteRune(unicode.ToLower(r))
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// TestTheShippedSecretsReachTheHostAsSensitive checks the definitions the host
// receives, not the catalog, so a conversion that drops the flag fails too.
// One secret at each depth: top level, inside an object, and inside a list
// element. Until 2026-09-24 the generator set Sensitive nowhere, and all three
// were printed in clear.
func TestTheShippedSecretsReachTheHostAsSensitive(t *testing.T) {
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	defs := map[string]*schema.ResourceDefinition{}
	for _, d := range c.Definitions() {
		defs[d.Type] = d
	}
	for _, path := range []string{
		"gcp.sslcertificate/privateKey",
		"gcp.backendservice/iap/oauth2ClientSecret",
		"gcp.compute.instance/disks/[]/diskEncryptionKey/rawKey",
	} {
		segs := strings.Split(path, "/")
		d := defs[segs[0]]
		if d == nil {
			t.Errorf("%s: type %s does not ship", path, segs[0])
			continue
		}
		a, ok := d.Attributes[segs[1]]
		for _, s := range segs[2:] {
			if !ok {
				break
			}
			if s == "[]" {
				ok = a.Elem != nil
				if ok {
					a = *a.Elem
				}
				continue
			}
			a, ok = a.Fields[s]
		}
		if !ok {
			t.Errorf("%s: no such attribute in the definition", path)
			continue
		}
		if !a.Sensitive {
			t.Errorf("%s is a secret and reaches the host without Sensitive, so plans and state show it", path)
		}
	}
}

// TestNodeGroupShipsWithItsNodeCount. magic-modules leaves NodeGroup's
// initialNodeCount as PRE_CREATE_REPLACE_ME for Terraform's pre_create; the
// generator binds it to Discovery's query parameter. Without that binding the
// type is refused, and this is the check that the binding runs.
func TestNodeGroupShipsWithItsNodeCount(t *testing.T) {
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	ty, ok := c.Type("gcp.nodegroup")
	if !ok {
		t.Fatal("gcp.nodegroup does not ship")
	}
	if !strings.Contains(ty.CreateURL, "initialNodeCount={{initialNodeCount}}") {
		t.Errorf("create url %q does not bind initialNodeCount", ty.CreateURL)
	}
	if a := ty.Attributes["initialNodeCount"]; a == nil || !a.Required || !a.CreateOnly {
		t.Errorf("initialNodeCount = %+v, want a required create-only attribute", a)
	}
}

// TestEverySecretLookingFieldIsSensitive. magic-modules' `sensitive` is the
// only source of secrets, and a search of the catalog on 2026-09-24 found 13
// it misses (a Cloud SQL user's password, a GKE cluster's basic-auth
// password, a router's MD5 key). They are marked in the overlay; this is the
// check that the next type shipping one fails here, not in a plan printed to
// someone's terminal.
//
// Settable string fields named as passwords, private or client keys, API
// keys, tokens and shared secrets. A reference to a secret (a Secret Manager
// version, a password URI) is not one. The allowed names are tokens that are
// identifiers, not credentials.
func TestEverySecretLookingFieldIsSensitive(t *testing.T) {
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	secret := regexp.MustCompile(`(?i)(password|passwd|privatekey|private_key|clientkey|apikey|api_key|activationtoken|sharedsecret|passphrase)`)
	// Not a secret: a reference to one, or a field describing one (its type,
	// its expiry).
	reference := regexp.MustCompile(`(?i)(SecretVersion|SecretUri|PasswordUri|Uri$|Name$|Id$|Version$|Interval$|Policy$|Config$|Type$|Duration$|Time$)`)
	notCredentials := map[string]bool{
		// Identifiers that happen to be called tokens: a Firebase source id,
		// a Cloud Run execution suffix, an object restore handle.
		"sourceToken": true, "runExecutionToken": true, "startExecutionToken": true, "restoreToken": true,
	}
	checked := 0
	var walk func(ty, path string, as map[string]*Attr)
	walk = func(ty, path string, as map[string]*Attr) {
		for name, a := range as {
			leaf := a.Canonical
			if leaf == "" {
				leaf = name
			}
			if a.Kind == value.KindString && !a.Output && secret.MatchString(leaf) && !reference.MatchString(leaf) && !notCredentials[leaf] {
				checked++
				if !a.Sensitive {
					t.Errorf("%s %s%s looks like a secret and is not sensitive; mark it in gen/overlay.yaml's sensitive list", ty, path, name)
				}
			}
			walk(ty, path+name+".", a.Fields)
			if a.Elem != nil {
				walk(ty, path+name+"[].", a.Elem.Fields)
			}
		}
	}
	for _, ty := range c.Types {
		walk(ty.Name, "", ty.Attributes)
	}
	// 14 were checked on 2026-09-24, every one sensitive.
	if checked < 12 {
		t.Errorf("only %d secret-looking fields were checked; the walk is not reaching them", checked)
	}
}

// TestTheCatalogKeepsItsRules checks, over every shipped type, the rules the
// generator enforces one type at a time: each was found broken on at least
// one type before it was a rule (a POST update from magic-modules, a create
// url carrying magic-modules' PRE_CREATE_REPLACE_ME, a setter on a path the
// id cannot fill). A rule enforced where it was written and nowhere else is
// how those shipped.
func TestTheCatalogKeepsItsRules(t *testing.T) {
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	ph := regexp.MustCompile(`\{\{?\+?%?([A-Za-z0-9_]+)\}?\}`)
	for _, ty := range c.Types {
		if ty.UpdateVerb != "" && ty.UpdateVerb != "PATCH" {
			t.Errorf("%s: update verb %q; the update is PATCH or nothing", ty.Name, ty.UpdateVerb)
		}
		for _, tmpl := range []string{ty.CreateURL, ty.BaseURL, ty.UpdateURL, ty.DeleteURL, ty.SelfLink} {
			if strings.Contains(tmpl, "PRE_CREATE_REPLACE_ME") {
				t.Errorf("%s: %q carries magic-modules' pre_create token", ty.Name, tmpl)
			}
		}
		if ty.EndpointTemplate != "" && (!strings.Contains(ty.EndpointTemplate, "{location}") || !strings.Contains(ty.SelfLink, "locations/")) {
			t.Errorf("%s: endpoint template %q cannot be filled from its ids", ty.Name, ty.EndpointTemplate)
		}
		idNames := map[string]bool{"project": true, "region": true, "zone": true, "location": true}
		for _, m := range ph.FindAllStringSubmatch(ty.SelfLink, -1) {
			idNames[m[1]] = true
		}
		for n := range ty.CreateBindings {
			idNames[n] = true
		}
		byCanonical := map[string]*Attr{}
		for _, a := range ty.Attributes {
			byCanonical[a.Canonical] = a
		}
		if ty.LockField != "" && byCanonical[ty.LockField] == nil {
			t.Errorf("%s: lock field %q is not an attribute", ty.Name, ty.LockField)
		}
		for _, s := range ty.Setters {
			for _, m := range ph.FindAllStringSubmatch(s.Path, -1) {
				if !idNames[m[1]] {
					t.Errorf("%s: setter %s's path names {%s}, which its id does not", ty.Name, s.Method, m[1])
				}
			}
			for _, f := range s.Fields {
				if a := byCanonical[f]; a == nil || a.Output || a.ForceNew {
					t.Errorf("%s: setter %s carries %q, which is not a field that changes in place", ty.Name, s.Method, f)
				}
			}
		}
		var walk func(path string, as map[string]*Attr)
		walk = func(path string, as map[string]*Attr) {
			for name, a := range as {
				if a.Required && a.Output {
					t.Errorf("%s %s%s is both required and output", ty.Name, path, name)
				}
				if a.InputOnly && a.Output {
					t.Errorf("%s %s%s is both input only and output", ty.Name, path, name)
				}
				walk(path+name+".", a.Fields)
				if a.Elem != nil {
					walk(path+name+"[].", a.Elem.Fields)
				}
			}
		}
		walk("", ty.Attributes)
	}
}
