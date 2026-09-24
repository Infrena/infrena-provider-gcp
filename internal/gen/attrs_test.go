package gen

import (
	"io"
	"os"
	"sort"
	"strings"
	"testing"

	"github.com/infrena/infrena-provider-gcp/internal/catalog"
	"github.com/infrena/infrena-provider-gcp/internal/disco"
	"github.com/infrena/infrena-provider-gcp/internal/mmv1"
	"github.com/infrena/infrena/pkg/value"
)

func TestKindsComeFromDiscoveryTypeAndFormat(t *testing.T) {
	cases := []struct {
		s    *disco.Schema
		want value.Kind
	}{
		{&disco.Schema{Type: "string"}, value.KindString},
		{&disco.Schema{Type: "integer", Format: "int32"}, value.KindInt},
		{&disco.Schema{Type: "string", Format: "int64"}, value.KindString}, // GCP writes int64 as a STRING
		{&disco.Schema{Type: "number", Format: "double"}, value.KindFloat},
		{&disco.Schema{Type: "boolean"}, value.KindBool},
		{&disco.Schema{Type: "array", Items: &disco.Schema{Type: "string"}}, value.KindList},
		{&disco.Schema{Type: "object"}, value.KindMap},
	}
	for _, c := range cases {
		if got := KindOf(c.s); got != c.want {
			t.Errorf("KindOf(%+v) = %v, want %v", c.s, got, c.want)
		}
	}
}

// The int64-as-string case above is not a curiosity. GCP serialises 64-bit
// integers as JSON strings throughout (compute disk sizes, quotas). Typing them
// as KindInt would make every such value fail to round-trip.

func TestFlagsComeFromTheRightSource(t *testing.T) {
	body := &disco.Schema{Type: "object", Properties: map[string]*disco.Schema{
		"name":       {Type: "string"},
		"sizeGb":     {Type: "integer", Format: "int32"},
		"selfLink":   {Type: "string", Description: "[Output Only] The URL."},
		"createTime": {Type: "string", ReadOnly: true},
	}}
	mm := &mmv1.Resource{Name: "Widget", Properties: []*mmv1.Field{
		{Name: "name", Type: "String", Required: true, Immutable: true},
		{Name: "sizeGb", Type: "Integer"},
	}}
	d := &disco.Document{Name: "tiny"}

	attrs, err := BuildAttributes(d, body, mm, nil)
	if err != nil {
		t.Fatal(err)
	}
	if a := attrs["name"]; !a.Required || !a.ForceNew || a.Output {
		t.Errorf("name: %+v — want required, forceNew, not output", a)
	}
	if a := attrs["sizeGb"]; a.Required || a.ForceNew || a.Output {
		t.Errorf("sizeGb: %+v — want none of the flags", a)
	}
	// Output-only must be detected from Discovery ALONE. magic-modules says nothing
	// about these two, which is the normal case for compute.
	for _, n := range []string{"selfLink", "createTime"} {
		if !attrs[n].Output {
			t.Errorf("%s not marked output-only", n)
		}
	}
}

func TestAliasesArePreferredThenSnakeCase(t *testing.T) {
	body := &disco.Schema{Type: "object", Properties: map[string]*disco.Schema{
		"authorizedNetwork": {Type: "string"},
		"sizeGb":            {Type: "integer", Format: "int32"},
	}}
	attrs, err := BuildAttributes(&disco.Document{Name: "tiny"}, body, &mmv1.Resource{Name: "W"},
		map[string]string{"authorizedNetwork": "network"})
	if err != nil {
		t.Fatal(err)
	}
	// Curated alias first, then the generated snake_case form.
	if got := attrs["authorizedNetwork"].Aliases; len(got) != 2 || got[0] != "network" || got[1] != "authorized_network" {
		t.Errorf("aliases = %v, want [network authorized_network]", got)
	}
	// No curated alias: snake_case only.
	if got := attrs["sizeGb"].Aliases; len(got) != 1 || got[0] != "size_gb" {
		t.Errorf("aliases = %v, want [size_gb]", got)
	}
}

// TestNestedKeysGetTheSameSpellings — spec §4.3 says nested keys accept the same
// spellings as top level. A recursion that built nested attributes without
// aliases would satisfy every assertion above and still make `config: {replica_count: 2}`
// fail for no reason the user can see.
func TestNestedKeysGetTheSameSpellings(t *testing.T) {
	body := &disco.Schema{Type: "object", Properties: map[string]*disco.Schema{
		"config": {Type: "object", Properties: map[string]*disco.Schema{
			"replicaCount": {Type: "integer", Format: "int32"},
		}},
	}}
	attrs, err := BuildAttributes(&disco.Document{Name: "tiny"}, body, &mmv1.Resource{Name: "W"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	nested := attrs["config"].Fields["replicaCount"]
	if nested == nil {
		t.Fatal("nested attribute missing")
	}
	if got := nested.Aliases; len(got) != 1 || got[0] != "replica_count" {
		t.Errorf("nested aliases = %v, want [replica_count]", got)
	}
}

func TestAKeywordCollisionIsRenamed(t *testing.T) {
	body := &disco.Schema{Type: "object", Properties: map[string]*disco.Schema{
		"type": {Type: "string"},
	}}
	attrs, err := BuildAttributes(&disco.Document{Name: "tiny"}, body, &mmv1.Resource{Name: "W"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, clash := attrs["type"]; clash {
		t.Error("an attribute named `type` would collide with the resource keyword")
	}
	if _, ok := attrs["type_value"]; !ok {
		t.Errorf("want type_value, got %v", keys(attrs))
	}
}

func TestAFreeFormMapIsOpaque(t *testing.T) {
	body := &disco.Schema{Type: "object", Properties: map[string]*disco.Schema{
		"labels": {Type: "object", AdditionalProperties: &disco.Schema{Type: "string"}},
		"config": {Type: "object", Properties: map[string]*disco.Schema{"mode": {Type: "string"}}},
	}}
	attrs, err := BuildAttributes(&disco.Document{Name: "tiny"}, body, &mmv1.Resource{Name: "W"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !attrs["labels"].Opaque {
		t.Error("a free-form map must be opaque; translating its keys would corrupt user data")
	}
	if attrs["config"].Opaque {
		t.Error("an object with declared properties must not be opaque")
	}
}

func TestNestedImmutabilityReachesTheNestedAttribute(t *testing.T) {
	body := &disco.Schema{Type: "object", Properties: map[string]*disco.Schema{
		"config": {Type: "object", Properties: map[string]*disco.Schema{
			"replicaCount": {Type: "integer", Format: "int32"},
			"mode":         {Type: "string"},
		}},
	}}
	mm := &mmv1.Resource{Name: "W", Properties: []*mmv1.Field{
		{Name: "config", Type: "NestedObject", Properties: []*mmv1.Field{
			{Name: "replicaCount", Type: "Integer", Immutable: true},
			{Name: "mode", Type: "Enum"},
		}},
	}}
	attrs, err := BuildAttributes(&disco.Document{Name: "tiny"}, body, mm, nil)
	if err != nil {
		t.Fatal(err)
	}
	cfg := attrs["config"]
	if cfg.Fields["replicaCount"] == nil || !cfg.Fields["replicaCount"].ForceNew {
		t.Errorf("nested replicaCount lost ForceNew: %+v", cfg.Fields)
	}
	if cfg.Fields["mode"].ForceNew {
		t.Error("nested mode gained a ForceNew it never had")
	}
}

// props builds a schema with the named properties, which is all AwaitOf reads.
func props(names ...string) *disco.Schema {
	s := &disco.Schema{Properties: map[string]*disco.Schema{}}
	for _, n := range names {
		s.Properties[n] = &disco.Schema{Type: "string"}
	}
	return s
}

// The shapes below are the real ones, measured across every mutation response
// in the pinned documents on 2026-09-23: every long-running operation carries
// done, error, metadata, name and response, and every compute-style one
// carries error, name, operationType, selfLink, status and targetLink.
var (
	lroShape     = []string{"done", "error", "metadata", "name", "response"}
	computeShape = []string{"error", "name", "operationType", "selfLink", "status", "targetLink"}
)

func TestAwaitIsChosenFromTheOperationShape(t *testing.T) {
	longrunning := &disco.Document{Name: "redis", Schemas: map[string]*disco.Schema{"Operation": props(lroShape...)}}
	compute := &disco.Document{Name: "compute", Schemas: map[string]*disco.Schema{"Operation": props(computeShape...)}}
	widget := &disco.Document{Name: "tiny", Schemas: map[string]*disco.Schema{"Widget": props("name")}}
	op := &disco.Method{Response: &disco.Ref{Ref: "Operation"}}
	if k, _ := AwaitOf(longrunning, op); k != catalog.AwaitLongRunning {
		t.Errorf("longrunning doc gave %v", k)
	}
	if k, _ := AwaitOf(compute, op); k != catalog.AwaitComputeOperation {
		t.Errorf("compute doc gave %v", k)
	}
	if k, _ := AwaitOf(widget, &disco.Method{Response: &disco.Ref{Ref: "Widget"}}); k != catalog.AwaitNone {
		t.Errorf("a method returning the resource gave %v, want none", k)
	}
}

// TestAwaitDoesNotDependOnWhatTheOperationIsCalled is the defect: AwaitOf used
// to require a schema named exactly "Operation". Eventarc, Cloud Run v2 and
// Firestore call google.longrunning.Operation "GoogleLongrunningOperation", and
// API Gateway calls it "ApigatewayOperation", so 13 shipping types -- Cloud Run
// services among them -- treated an operation still running as the finished
// resource.
func TestAwaitDoesNotDependOnWhatTheOperationIsCalled(t *testing.T) {
	for _, name := range []string{"GoogleLongrunningOperation", "ApigatewayOperation"} {
		d := &disco.Document{Name: "run", Schemas: map[string]*disco.Schema{name: props(lroShape...)}}
		if k, _ := AwaitOf(d, &disco.Method{Response: &disco.Ref{Ref: name}}); k != catalog.AwaitLongRunning {
			t.Errorf("%s gave %v, want long-running", name, k)
		}
	}
}

// TestAResourceThatLooksLikeAnOperationIsNotOne. Deciding by shape rather than
// name means a resource can now reach these checks, so they must not be fooled
// by one field. dataproc's Job has `done` and `status` and is a resource; most
// resources have `status` and `name`; DNS's own "Operation" has only `status`
// and is not compute's.
func TestAResourceThatLooksLikeAnOperationIsNotOne(t *testing.T) {
	for name, s := range map[string]*disco.Schema{
		"dataproc Job":       props("done", "status", "reference", "placement"),
		"resource w/ status": props("name", "status", "selfLink", "description"),
		"dns Operation":      props("id", "status", "type", "startTime"),
	} {
		d := &disco.Document{Name: "x", Schemas: map[string]*disco.Schema{"Thing": s}}
		if k, _ := AwaitOf(d, &disco.Method{Response: &disco.Ref{Ref: "Thing"}}); k != catalog.AwaitNone {
			t.Errorf("%s gave %v, want none", name, k)
		}
	}
}

// TestListFieldPrefersTheRealArrayOverUnreachable is the case that broke a
// naive "first array property" heuristic during review: alloydb's
// operations.list response schema carries both "operations" (the real
// results) and "unreachable" (locations a partial-failure list call could
// not reach). This exercises the LEAF-MATCH branch, not the blocklist: the
// collection's own leaf is "operations", which is also the real property's
// name, so that's what wins here — "unreachable" (and the decoy
// "auditConfigs" below) never reach the blocklist check at all. The
// blocklist-skip branch has its own test,
// TestListFieldFallsBackToTheFirstNonBlocklistedArrayWhenNoNameMatches,
// where no property name matches the leaf. "auditConfigs" is a decoy third
// array that sorts alphabetically before both real candidates, so a naive
// "first array in sorted order" implementation cannot pass this by
// coincidentally landing on "operations" the way it would if "unreachable"
// were the only decoy (sorting after "operations" on its own).
func TestListFieldPrefersTheRealArrayOverUnreachable(t *testing.T) {
	doc := &disco.Document{Name: "alloydb", Schemas: map[string]*disco.Schema{
		"ListOperationsResponse": {Type: "object", Properties: map[string]*disco.Schema{
			"auditConfigs":  {Type: "array", Items: &disco.Schema{Type: "object"}},
			"unreachable":   {Type: "array", Items: &disco.Schema{Type: "string"}},
			"operations":    {Type: "array", Items: &disco.Schema{Type: "object"}},
			"nextPageToken": {Type: "string"},
		}},
	}}
	col := disco.Collection{
		Path: []string{"projects", "locations", "operations"},
		Methods: map[string]*disco.Method{
			"list": {Response: &disco.Ref{Ref: "ListOperationsResponse"}},
		},
	}
	if got := ListFieldOf(doc, col); got != "operations" {
		t.Errorf("ListFieldOf = %q, want %q (unreachable must lose to the real collection array)", got, "operations")
	}
}

// TestListFieldMatchesTheCollectionLeafOverAnyOtherArray. logging's
// buckets.list response names its result "buckets", not "items" — matching
// the collection's own leaf must win even when another, earlier-sorted array
// property exists.
func TestListFieldMatchesTheCollectionLeafOverAnyOtherArray(t *testing.T) {
	doc := &disco.Document{Name: "logging", Schemas: map[string]*disco.Schema{
		"ListBucketsResponse": {Type: "object", Properties: map[string]*disco.Schema{
			"buckets":       {Type: "array", Items: &disco.Schema{Type: "object"}},
			"auditConfigs":  {Type: "array", Items: &disco.Schema{Type: "object"}},
			"nextPageToken": {Type: "string"},
		}},
	}}
	col := disco.Collection{
		Path: []string{"projects", "locations", "buckets"},
		Methods: map[string]*disco.Method{
			"list": {Response: &disco.Ref{Ref: "ListBucketsResponse"}},
		},
	}
	if got := ListFieldOf(doc, col); got != "buckets" {
		t.Errorf("ListFieldOf = %q, want %q", got, "buckets")
	}
}

// TestListFieldFallsBackToTheFirstNonBlocklistedArrayWhenNoNameMatches
// covers a collection whose result array's name has nothing to do with the
// collection's own leaf (e.g. a custom method's response), so the fallback —
// not the leaf match — is what has to find it.
func TestListFieldFallsBackToTheFirstNonBlocklistedArrayWhenNoNameMatches(t *testing.T) {
	doc := &disco.Document{Name: "tiny", Schemas: map[string]*disco.Schema{
		"ListWidgetsResponse": {Type: "object", Properties: map[string]*disco.Schema{
			"warnings":      {Type: "array", Items: &disco.Schema{Type: "object"}},
			"results":       {Type: "array", Items: &disco.Schema{Type: "object"}},
			"nextPageToken": {Type: "string"},
		}},
	}}
	col := disco.Collection{
		Path: []string{"projects", "widgets"},
		Methods: map[string]*disco.Method{
			"list": {Response: &disco.Ref{Ref: "ListWidgetsResponse"}},
		},
	}
	if got := ListFieldOf(doc, col); got != "results" {
		t.Errorf("ListFieldOf = %q, want %q (warnings must be skipped)", got, "results")
	}
}

func TestListFieldIsEmptyWithoutAListMethod(t *testing.T) {
	doc := &disco.Document{Name: "tiny", Schemas: map[string]*disco.Schema{}}
	col := disco.Collection{Path: []string{"projects", "widgets"}, Methods: map[string]*disco.Method{}}
	if got := ListFieldOf(doc, col); got != "" {
		t.Errorf("ListFieldOf = %q, want empty when there is no list method", got)
	}
}

func TestScopeComesFromTheURLTemplate(t *testing.T) {
	cases := map[string]catalog.Scope{
		"projects/{{project}}/global/networks":                catalog.ScopeGlobal,
		"projects/{{project}}/regions/{{region}}/subnetworks": catalog.ScopeRegional,
		"projects/{{project}}/zones/{{zone}}/instances":       catalog.ScopeZonal,
		"projects/{{project}}/locations/{{region}}/widgets":   catalog.ScopeRegional,
	}
	for url, want := range cases {
		if got := ScopeOf(url); got != want {
			t.Errorf("ScopeOf(%s) = %v, want %v", url, got, want)
		}
	}
}

// TestConflictDiagnosticsAreEmittedInSortedOrder. The walk that finds
// Output-vs-Required conflicts ranges over a Go map, so without sorting, the
// order these lines print in is randomized per run — a human diffing
// regeneration output between runs would see the diagnostics themselves
// reorder for no reason, burying any real change. Ten conflicting properties
// are used (not two or three) so an implementation that merely got lucky on
// map order has a 1-in-3,628,800 chance of passing by accident.
func TestConflictDiagnosticsAreEmittedInSortedOrder(t *testing.T) {
	names := []string{"zeta", "yankee", "xray", "whiskey", "victor", "uniform", "tango", "sierra", "romeo", "quebec"}
	props := map[string]*disco.Schema{}
	var fields []*mmv1.Field
	for _, n := range names {
		props[n] = &disco.Schema{Type: "string", ReadOnly: true}
		fields = append(fields, &mmv1.Field{Name: n, Required: true})
	}
	body := &disco.Schema{Type: "object", Properties: props}
	mm := &mmv1.Resource{Name: "W", Properties: fields}
	d := &disco.Document{Name: "tiny"}

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	orig := os.Stderr
	os.Stderr = w
	_, buildErr := BuildAttributes(d, body, mm, nil)
	w.Close()
	os.Stderr = orig
	if buildErr != nil {
		t.Fatal(buildErr)
	}
	captured, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimRight(string(captured), "\n"), "\n")
	if len(lines) != len(names) {
		t.Fatalf("got %d diagnostic lines, want %d: %q", len(lines), len(names), captured)
	}
	if !sort.StringsAreSorted(lines) {
		t.Errorf("diagnostics not sorted:\n%s", captured)
	}
}

// TestParametersWinOverPropertiesOnNameCollision. mmIndex flattens Parameters
// ahead of Properties and keeps the first name it sees, so a field declared in
// both (with different flags) must resolve to the Parameters entry. This is a
// real precedence rule, not incidental: a fixture with matching flags in both
// places would pass under either resolution order.
func TestParametersWinOverPropertiesOnNameCollision(t *testing.T) {
	body := &disco.Schema{Type: "object", Properties: map[string]*disco.Schema{
		"name": {Type: "string"},
	}}
	mm := &mmv1.Resource{
		Name: "W",
		Parameters: []*mmv1.Field{
			{Name: "name", Required: true, Immutable: true},
		},
		Properties: []*mmv1.Field{
			{Name: "name", Required: false, Immutable: false},
		},
	}
	attrs, err := BuildAttributes(&disco.Document{Name: "tiny"}, body, mm, nil)
	if err != nil {
		t.Fatal(err)
	}
	if a := attrs["name"]; !a.Required || !a.ForceNew {
		t.Errorf("name: %+v — want the Parameters entry (required, forceNew) to win over Properties", a)
	}
}

// TestACuratedAliasEqualToSnakeCaseIsNotDuplicated. When the curated alias and
// the generated snake_case form are the same string, it must appear once, not
// twice — a consumer treating len(Aliases) as "how many distinct spellings"
// would otherwise be lied to.
func TestACuratedAliasEqualToSnakeCaseIsNotDuplicated(t *testing.T) {
	body := &disco.Schema{Type: "object", Properties: map[string]*disco.Schema{
		"sizeGb": {Type: "integer", Format: "int32"},
	}}
	attrs, err := BuildAttributes(&disco.Document{Name: "tiny"}, body, &mmv1.Resource{Name: "W"},
		map[string]string{"sizeGb": "size_gb"})
	if err != nil {
		t.Fatal(err)
	}
	if got := attrs["sizeGb"].Aliases; len(got) != 1 || got[0] != "size_gb" {
		t.Errorf("aliases = %v, want exactly [size_gb] with no duplicate", got)
	}
}

// TestArrayOfPrimitivesGetsAScalarElem covers the Elem branch for an array
// whose items are not objects: Elem must carry the item Kind and nothing
// else — no Fields, no Opaque.
func TestArrayOfPrimitivesGetsAScalarElem(t *testing.T) {
	body := &disco.Schema{Type: "object", Properties: map[string]*disco.Schema{
		"tags": {Type: "array", Items: &disco.Schema{Type: "string"}},
	}}
	attrs, err := BuildAttributes(&disco.Document{Name: "tiny"}, body, &mmv1.Resource{Name: "W"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	tags := attrs["tags"]
	if tags.Kind != value.KindList {
		t.Fatalf("tags.Kind = %v, want KindList", tags.Kind)
	}
	if tags.Elem == nil || tags.Elem.Kind != value.KindString {
		t.Fatalf("tags.Elem = %+v, want a scalar KindString elem", tags.Elem)
	}
	if tags.Elem.Fields != nil || tags.Elem.Opaque {
		t.Errorf("a scalar element must not carry Fields or Opaque: %+v", tags.Elem)
	}
}

// TestArrayOfObjectsKeepsItsNestedFieldsAndLifecycleFlags covers the other half
// of the Elem branch: an array of declared objects must recurse into its
// item schema exactly like a plain nested object does, including picking up
// ForceNew from magic-modules for a field nested inside the array element. A
// generator that special-cased array elements to skip the mm lookup would
// still pass every other test in this file and still produce a type with a
// silently mutable field GCP actually forbids changing.
func TestArrayOfObjectsKeepsItsNestedFieldsAndLifecycleFlags(t *testing.T) {
	body := &disco.Schema{Type: "object", Properties: map[string]*disco.Schema{
		"rules": {Type: "array", Items: &disco.Schema{Type: "object", Properties: map[string]*disco.Schema{
			"port": {Type: "integer", Format: "int32"},
			"mode": {Type: "string"},
		}}},
	}}
	mm := &mmv1.Resource{Name: "W", Properties: []*mmv1.Field{
		{Name: "rules", Type: "Array", Properties: []*mmv1.Field{
			{Name: "port", Type: "Integer", Immutable: true},
			{Name: "mode", Type: "Enum"},
		}},
	}}
	attrs, err := BuildAttributes(&disco.Document{Name: "tiny"}, body, mm, nil)
	if err != nil {
		t.Fatal(err)
	}
	rules := attrs["rules"]
	if rules.Elem == nil || rules.Elem.Fields == nil {
		t.Fatal("rules.Elem is missing its nested Fields")
	}
	port, ok := rules.Elem.Fields["port"]
	if !ok {
		t.Fatal("nested port attribute missing from the array element")
	}
	if !port.ForceNew {
		t.Errorf("nested port lost ForceNew reached through the array element: %+v", port)
	}
	if rules.Elem.Fields["mode"].ForceNew {
		t.Error("nested mode gained a ForceNew it never had")
	}
}

func keys(m map[string]*catalog.Attr) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// TestAFieldIsFoundByItsApiNameToo. magic-modules names a field whatever
// Terraform calls it and records the API's own name separately, in api_name:
// compute's Firewall declares "allow" with api_name "allowed", eventarc's
// Trigger declares "matchingCriteria" with api_name "eventFilters". Discovery
// only ever uses the API's name, so an index keyed on magic-modules' name
// alone finds nothing for those fields and every lifecycle flag behind them
// -- required, immutable, and is_set, which is what tells reconciliation a
// list may come back reordered -- is dropped silently.
//
// Measured over the vendored corpus on 2026-09-22: 438 api_name lines, and 5
// of the 35 is_set fields on types this plugin ships were reached only this
// way (compute Firewall allow and deny, UrlMap and RegionUrlMap host_rule,
// eventarc Trigger matchingCriteria).
func TestAFieldIsFoundByItsApiNameToo(t *testing.T) {
	body := &disco.Schema{Type: "object", Properties: map[string]*disco.Schema{
		"allowed": {Type: "array", Items: &disco.Schema{Type: "string"}},
	}}
	mm := &mmv1.Resource{Name: "W", Properties: []*mmv1.Field{
		{Name: "allow", ApiName: "allowed", IsSet: true, Required: true},
	}}
	attrs, err := BuildAttributes(&disco.Document{Name: "tiny"}, body, mm, nil)
	if err != nil {
		t.Fatal(err)
	}
	a := attrs["allowed"]
	if a == nil {
		t.Fatal("the Discovery property vanished")
	}
	if !a.Unordered {
		t.Error("allowed is not marked Unordered, so reconciliation will treat a set as " +
			"an ordered list and propose a reordering forever")
	}
	if !a.Required {
		t.Error("allowed is not Required; the magic-modules entry behind it was not found at all")
	}
}

// TestARealFieldNameBeatsAnotherFieldsApiName. 14 of the corpus's 942
// resources declare a field whose api_name is also some OTHER field's own
// name (compute's InstanceGroupManager has an "id" and an
// "instanceGroupManagerId" with api_name "id"). The real field has to win, or
// a Discovery property would take its flags from an unrelated field --
// whichever the walk happened to reach first.
func TestARealFieldNameBeatsAnotherFieldsApiName(t *testing.T) {
	body := &disco.Schema{Type: "object", Properties: map[string]*disco.Schema{
		"id": {Type: "string"},
	}}
	mm := &mmv1.Resource{Name: "W", Properties: []*mmv1.Field{
		// The aliased one first, so first-seen-wins alone would pick it.
		{Name: "instanceGroupManagerId", ApiName: "id", Required: true, Immutable: true},
		{Name: "id"},
	}}
	attrs, err := BuildAttributes(&disco.Document{Name: "tiny"}, body, mm, nil)
	if err != nil {
		t.Fatal(err)
	}
	if a := attrs["id"]; a.Required || a.ForceNew {
		t.Errorf("id: %+v -- want the field actually called \"id\", not the one that merely "+
			"claims the name through api_name", a)
	}
}

// TestImmutabilityComesFromEitherSource. magic-modules' `immutable:` and
// Discovery's "Immutable." tag are ORed: on 2026-09-23 the tag caught 55 fields
// magic-modules had not marked, and magic-modules marks many the API never
// tags. A field the API will not change must replace the resource rather than
// be sent as a patch the API refuses.
func TestImmutabilityComesFromEitherSource(t *testing.T) {
	body := &disco.Schema{Properties: map[string]*disco.Schema{
		"tagged":  {Type: "string", Description: "Optional. Input only. Immutable. Tags bound at creation."},
		"curated": {Type: "string", Description: "The format. Nothing here says so."},
		"neither": {Type: "string", Description: "A description."},
		"prose":   {Type: "string", Description: "The deadline. Immutable. After that it is fixed."},
	}}
	mm := &mmv1.Resource{Name: "Widget", Properties: []*mmv1.Field{{Name: "curated", Type: "String", Immutable: true}}}
	attrs, err := BuildAttributes(&disco.Document{Name: "tiny"}, body, mm, nil)
	if err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]bool{"tagged": true, "curated": true, "neither": false, "prose": false} {
		if got := attrs[name].ForceNew; got != want {
			t.Errorf("%s: ForceNew = %v, want %v", name, got, want)
		}
	}
}

// TestInputOnlyComesFromTheTagInEitherSpelling. The modern APIs write "Input
// only." and compute writes "[Input Only]" -- disks[].initializeParams, the
// field behind an instance that planned its own replacement on every run,
// uses the bracketed form. An output-only field is never input-only: GCP
// returns what it sets, and there is nothing of the user's to carry.
func TestInputOnlyComesFromTheTagInEitherSpelling(t *testing.T) {
	body := &disco.Schema{Properties: map[string]*disco.Schema{
		"modern":  {Type: "string", Description: "Optional. Input only. Immutable. Tags bound at creation."},
		"compute": {Type: "string", Description: "[Input Only] Specifies the parameters for a new disk."},
		"plain":   {Type: "string", Description: "A description that mentions input only in passing."},
		"output":  {Type: "string", ReadOnly: true, Description: "Input only. A field that contradicts itself."},
	}}
	attrs, err := BuildAttributes(&disco.Document{Name: "tiny"}, body, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]bool{"modern": true, "compute": true, "plain": false, "output": false} {
		if got := attrs[name].InputOnly; got != want {
			t.Errorf("%s: InputOnly = %v, want %v", name, got, want)
		}
	}
}

// TestASecretIsSensitiveWhereverItSits. Discovery cannot say a field is a
// secret, so magic-modules is the only source, and until 2026-09-24 the
// generator never read it: an SSL certificate's private key and a disk's raw
// encryption key went into plans and state in clear. The list element is
// reached through item_type, which is where magic-modules keeps an array's
// fields.
func TestASecretIsSensitiveWhereverItSits(t *testing.T) {
	body := &disco.Schema{Type: "object", Properties: map[string]*disco.Schema{
		"privateKey":  {Type: "string"},
		"description": {Type: "string"},
		"disks": {Type: "array", Items: &disco.Schema{Type: "object", Properties: map[string]*disco.Schema{
			"rawKey":   {Type: "string"},
			"diskName": {Type: "string"},
		}}},
	}}
	mm := &mmv1.Resource{Name: "W", Properties: []*mmv1.Field{
		{Name: "privateKey", Sensitive: true},
		{Name: "description"},
		{Name: "disks", Type: "Array", ItemType: &mmv1.ItemType{Type: "NestedObject", Properties: []*mmv1.Field{
			{Name: "rawKey", WriteOnly: true},
			{Name: "diskName"},
		}}},
	}}
	attrs, err := BuildAttributes(&disco.Document{Name: "tiny"}, body, mm, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !attrs["privateKey"].Sensitive {
		t.Error("privateKey is marked sensitive in magic-modules and is not sensitive here")
	}
	if attrs["description"].Sensitive {
		t.Error("description became sensitive, so the flag is not coming from the field")
	}
	elem := attrs["disks"].Elem
	if !elem.Fields["rawKey"].Sensitive {
		t.Error("disks[].rawKey is write_only in magic-modules and is not sensitive here")
	}
	if elem.Fields["diskName"].Sensitive {
		t.Error("disks[].diskName became sensitive")
	}
}

// TestAFlagStaysOnTheFieldItWasWrittenFor. The index was once one flat map
// across every depth, keyed by bare name, so authz policy's required
// ipBlocks[].prefix made every other field called prefix required too, and a
// nested "name" could shadow the resource's own.
func TestAFlagStaysOnTheFieldItWasWrittenFor(t *testing.T) {
	str := &disco.Schema{Type: "string"}
	body := &disco.Schema{Type: "object", Properties: map[string]*disco.Schema{
		"rules": {Type: "array", Items: &disco.Schema{Type: "object", Properties: map[string]*disco.Schema{
			"name": str,
			"ipBlocks": {Type: "array", Items: &disco.Schema{Type: "object", Properties: map[string]*disco.Schema{
				"prefix": str}}},
			"paths": {Type: "array", Items: &disco.Schema{Type: "object", Properties: map[string]*disco.Schema{
				"prefix": str}}},
		}}},
		"name": str,
	}}
	mm := &mmv1.Resource{Name: "W", Properties: []*mmv1.Field{
		{Name: "rules", Type: "Array", ItemType: &mmv1.ItemType{Type: "NestedObject", Properties: []*mmv1.Field{
			{Name: "name"},
			{Name: "ipBlocks", Type: "Array", ItemType: &mmv1.ItemType{Type: "NestedObject", Properties: []*mmv1.Field{
				{Name: "prefix", Required: true}}}},
			{Name: "paths", Type: "Array", ItemType: &mmv1.ItemType{Type: "NestedObject", Properties: []*mmv1.Field{
				{Name: "prefix"}}}},
		}}},
		{Name: "name", Required: true, Immutable: true},
	}}
	attrs, err := BuildAttributes(&disco.Document{Name: "tiny"}, body, mm, nil)
	if err != nil {
		t.Fatal(err)
	}
	rule := attrs["rules"].Elem.Fields
	if !rule["ipBlocks"].Elem.Fields["prefix"].Required {
		t.Error("ipBlocks[].prefix lost the required flag magic-modules gives it")
	}
	if rule["paths"].Elem.Fields["prefix"].Required {
		t.Error("paths[].prefix is required, a flag that belongs to ipBlocks[].prefix")
	}
	if a := attrs["name"]; !a.Required || !a.ForceNew {
		t.Errorf("the top-level name lost its own flags: %+v", a)
	}
	if rule["name"].Required || rule["name"].ForceNew {
		t.Errorf("rules[].name took the top-level name's flags: %+v", rule["name"])
	}
}

// TestAHandWrittenResourcesNestedRequiredIsNotTrusted. Terraform never runs
// the YAML of an exclude_resource resource, so its nested `required` is
// unenforced and wrong in places (compute Instance's access config name). The
// same field on an ordinary resource keeps the flag, so a pass that clears
// Required everywhere fails here too.
func TestAHandWrittenResourcesNestedRequiredIsNotTrusted(t *testing.T) {
	str := &disco.Schema{Type: "string"}
	body := &disco.Schema{Type: "object", Properties: map[string]*disco.Schema{
		"name": str,
		"accessConfigs": {Type: "array", Items: &disco.Schema{Type: "object", Properties: map[string]*disco.Schema{
			"name": str}}},
	}}
	build := func(exclude bool) map[string]*catalog.Attr {
		mm := &mmv1.Resource{Name: "W", ExcludeResource: exclude, Properties: []*mmv1.Field{
			{Name: "name", Required: true},
			{Name: "accessConfigs", Type: "Array", ItemType: &mmv1.ItemType{Type: "NestedObject", Properties: []*mmv1.Field{
				{Name: "name", Required: true}}}},
		}}
		attrs, err := BuildAttributes(&disco.Document{Name: "tiny"}, body, mm, nil)
		if err != nil {
			t.Fatal(err)
		}
		return attrs
	}
	hand := build(true)
	if !hand["name"].Required {
		t.Error("the hand-written resource lost its top-level required name")
	}
	if hand["accessConfigs"].Elem.Fields["name"].Required {
		t.Error("accessConfigs[].name is required on a resource whose YAML Terraform never runs")
	}
	if !build(false)["accessConfigs"].Elem.Fields["name"].Required {
		t.Error("an ordinary resource lost a nested required flag")
	}
}

// TestAFieldGoogleNeverReturnsIsCarried. magic-modules' ignore_read is the
// second source for input only, and the one that covers compute: an SSL
// certificate's privateKey is never returned, and read as removed it replaced
// the certificate on every plan. Nested too, where most of these live.
func TestAFieldGoogleNeverReturnsIsCarried(t *testing.T) {
	str := &disco.Schema{Type: "string"}
	body := &disco.Schema{Type: "object", Properties: map[string]*disco.Schema{
		"privateKey":  str,
		"certificate": str,
		"iap":         {Type: "object", Properties: map[string]*disco.Schema{"oauth2ClientSecret": str}},
		"selfLink":    str,
	}}
	mm := &mmv1.Resource{Name: "W", Properties: []*mmv1.Field{
		{Name: "privateKey", IgnoreRead: true, Required: true, Immutable: true},
		{Name: "certificate"},
		{Name: "iap", Type: "NestedObject", Properties: []*mmv1.Field{{Name: "oauth2ClientSecret", IgnoreRead: true}}},
		{Name: "selfLink", IgnoreRead: true, Output: true},
	}}
	attrs, err := BuildAttributes(&disco.Document{Name: "tiny"}, body, mm, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !attrs["privateKey"].InputOnly {
		t.Error("privateKey is ignore_read in magic-modules and is not carried forward")
	}
	if attrs["certificate"].InputOnly {
		t.Error("certificate became input only, so the flag is not coming from the field")
	}
	if !attrs["iap"].Fields["oauth2ClientSecret"].InputOnly {
		t.Error("iap.oauth2ClientSecret is ignore_read and is not carried forward")
	}
	if attrs["selfLink"].InputOnly {
		t.Error("an output field became input only; there is nothing of the user's to carry")
	}
}

// TestAReferenceThatTakesSeveralTypesIsNotTyped. A url map's defaultService
// is a BackendService reference in magic-modules and takes a backend bucket
// too. Typed, infrena refused a valid configuration before sending anything.
// The ordinary reference beside it keeps its type, so a change that drops
// every reference fails here too.
func TestAReferenceThatTakesSeveralTypesIsNotTyped(t *testing.T) {
	str := &disco.Schema{Type: "string"}
	body := &disco.Schema{Type: "object", Properties: map[string]*disco.Schema{"defaultService": str, "network": str}}
	mm := &mmv1.Resource{Name: "UrlMap", Properties: []*mmv1.Field{
		{Name: "defaultService", Type: "ResourceRef", Resource: "BackendService",
			CustomExpand: "templates/terraform/custom_expand/reference_to_backend.tmpl"},
		{Name: "network", Type: "ResourceRef", Resource: "Network"},
	}}
	attrs, err := BuildAttributes(&disco.Document{Name: "tiny"}, body, mm, nil)
	if err != nil {
		t.Fatal(err)
	}
	if r := attrs["defaultService"].Ref; r != nil {
		t.Errorf("defaultService refers only to %s, but it takes a backend bucket as well", r.Type)
	}
	if attrs["network"].Ref == nil {
		t.Error("network lost its reference")
	}
}
