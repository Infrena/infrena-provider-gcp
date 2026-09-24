package gcprov

import (
	"context"
	"strings"
	"testing"

	"github.com/infrena/infrena-provider-gcp/internal/catalog"
	"github.com/infrena/infrena-provider-gcp/internal/gcpfake"
	"github.com/infrena/infrena-provider-gcp/internal/gcptest"
	"github.com/infrena/infrena/pkg/resource"
	"github.com/infrena/infrena/pkg/value"
)

func TestAServerAddedNestedKeyIsDropped(t *testing.T) {
	attr := &catalog.Attr{Canonical: "config", Kind: value.KindMap, Fields: map[string]*catalog.Attr{
		"mode": {Canonical: "mode", Kind: value.KindString},
	}}
	ref := mapValue(map[string]any{"mode": "A"})
	in := mapValue(map[string]any{"mode": "A", "serverFingerprint": "xyz"})

	got := Reconcile(attr, ref, in)
	m := got.Raw.(map[string]value.Value)
	if _, present := m["serverFingerprint"]; present {
		t.Error("a server-added key survived; every plan would propose a change forever")
	}
	if m["mode"].Raw != "A" {
		t.Errorf("the declared key was lost: %v", m)
	}
}

func TestAnOpaqueValueIsCopiedExactly(t *testing.T) {
	attr := &catalog.Attr{Canonical: "labels", Kind: value.KindMap, Opaque: true}
	ref := mapValue(map[string]any{"env": "prod"})
	in := mapValue(map[string]any{"env": "prod", "goog-managed-by": "x", "team": "infra"})

	got := Reconcile(attr, ref, in)
	m := got.Raw.(map[string]value.Value)
	// Opaque means exactly that. Dropping "team" because the reference did not
	// mention it would delete something the user wrote.
	if len(m) != 3 {
		t.Errorf("opaque value was pruned to %d keys: %v", len(m), m)
	}
}

// TestAnUnorderedListIsReorderedToMatchTheReference. GCP reorders lists it does
// not consider ordered, and a diff on order alone plans a change forever.
func TestAnUnorderedListIsReorderedToMatchTheReference(t *testing.T) {
	attr := &catalog.Attr{Canonical: "tags", Kind: value.KindList, Unordered: true,
		Elem: &catalog.Attr{Kind: value.KindString}}
	ref := listValue("web", "ssh", "db")
	in := listValue("db", "web", "ssh")

	got := Reconcile(attr, ref, in)
	items := got.Raw.([]value.Value)
	for i, want := range []string{"web", "ssh", "db"} {
		if items[i].Raw != want {
			t.Errorf("item %d = %v, want %v (list not reordered to the reference)", i, items[i].Raw, want)
		}
	}
}

// And an ORDERED list must not be touched, or reconciliation would silently
// rewrite something order-significant like a firewall rule priority list.
func TestAnOrderedListIsLeftAlone(t *testing.T) {
	attr := &catalog.Attr{Canonical: "rules", Kind: value.KindList, Unordered: false,
		Elem: &catalog.Attr{Kind: value.KindString}}
	ref := listValue("a", "b")
	in := listValue("b", "a")
	got := Reconcile(attr, ref, in)
	if got.Raw.([]value.Value)[0].Raw != "b" {
		t.Error("an ordered list was reordered")
	}
}

// TestAnUnorderedListStillShowsAnAddition. Reordering must not become
// "ignore the difference": an element GCP added that the reference never held
// is real drift, and it has to survive at the end of the list.
func TestAnUnorderedListStillShowsAnAddition(t *testing.T) {
	attr := &catalog.Attr{Canonical: "tags", Kind: value.KindList, Unordered: true,
		Elem: &catalog.Attr{Kind: value.KindString}}
	got := Reconcile(attr, listValue("web", "ssh"), listValue("db", "ssh", "web"))
	items := got.Raw.([]value.Value)
	if len(items) != 3 {
		t.Fatalf("reordering dropped an element: %v", items)
	}
	if items[0].Raw != "web" || items[1].Raw != "ssh" || items[2].Raw != "db" {
		t.Errorf("got %v, want the reference's order followed by what GCP added", items)
	}
}

func TestLabelsAreAMapAndReservedOnesAreNeverReported(t *testing.T) {
	in := mapValue(map[string]any{
		"env": "prod", "team": "infra",
		"goog-dm": "deployment", "goog-gke-node": "true",
	})
	got := LabelsIn(in)
	if got["env"] != "prod" || got["team"] != "infra" {
		t.Errorf("user labels lost: %v", got)
	}
	// GCP reserves the goog- prefix, the same way AWS reserves aws-. Reporting one
	// would make every plan propose removing it.
	for k := range got {
		if strings.HasPrefix(k, "goog-") {
			t.Errorf("reserved label %q reported", k)
		}
	}
}

// TestAResourceWithNoLabelsOmitsTheKeyEntirely, rather than sending an empty map,
// which some APIs treat as "remove all labels".
func TestNoLabelsMeansNoKey(t *testing.T) {
	if got := LabelsOut(nil); got.Known && len(got.Raw.(map[string]value.Value)) != 0 {
		t.Errorf("an empty label set produced %v", got)
	}
}

// TestLabelsSurviveTheRoundTrip. The pair is only useful if what comes back
// out is what went in, minus what GCP owns.
func TestLabelsSurviveTheRoundTrip(t *testing.T) {
	out := LabelsOut(map[string]string{"env": "prod", "team": "infra"})
	if !out.Known {
		t.Fatal("a non-empty label set was omitted")
	}
	got := LabelsIn(out)
	if len(got) != 2 || got["env"] != "prod" || got["team"] != "infra" {
		t.Errorf("round trip produced %v", got)
	}
}

// TestAWholeNumberFloatKeepsTheKindTheCatalogDeclares is the first of the two
// comparison bugs Task 14's review carried here. JSON has one number type, so
// GCP answering 1 for a float-declared field is indistinguishable from 1.0,
// and fromRaw types a whole number as KindInt. Value.Equal compares Kind
// before Raw, so that attribute would never equal the configured float and
// would patch on every update forever.
//
// Against a REAL catalog entry, not a synthetic one: gcp.wasmplugin's
// logConfig.sampleRate is one of the 134 KindFloat attributes in the shipped
// catalog (28 types, 60 of them inside a list, measured following Fields AND
// Elem), and a fixture declaring its own float would not prove the catalog
// still has any.
func TestAWholeNumberFloatKeepsTheKindTheCatalogDeclares(t *testing.T) {
	ty, _ := syncCatalogFor(t, "gcp.wasmplugin")
	logConfig := ty.Attributes["logConfig"]
	if logConfig == nil || logConfig.Fields["sampleRate"] == nil ||
		logConfig.Fields["sampleRate"].Kind != value.KindFloat {
		t.Fatalf("%s no longer declares logConfig.sampleRate as a float; this test is not exercising one", ty.Name)
	}

	// What GCP answers: a whole number, which fromRaw reads as KindInt.
	incoming := value.Map(map[string]value.Value{
		"sampleRate": value.Int(1, value.SourceProvider),
	}, value.SourceProvider)
	want := mapValue(map[string]any{"sampleRate": 1.0})

	got := Reconcile(logConfig, want, incoming)
	rate := got.Raw.(map[string]value.Value)["sampleRate"]
	if rate.Kind != value.KindFloat {
		t.Errorf("sampleRate came back as %v, so it can never equal the configured float", rate.Kind)
	}
	if !got.Equal(want) {
		t.Errorf("logConfig = %#v, want something equal to the configured %#v", got, want)
	}
}

// TestALossyNumberIsLeftAlone. Retyping must not become rounding: 1.5 into an
// int-declared attribute is a real difference, and hiding it would report a
// resource as matching configuration it does not match.
func TestALossyNumberIsLeftAlone(t *testing.T) {
	attr := &catalog.Attr{Canonical: "size", Kind: value.KindInt}
	got := Reconcile(attr, value.Int(2, value.SourceExplicit), value.Float(1.5, value.SourceProvider))
	if got.Kind != value.KindFloat || got.Raw != 1.5 {
		t.Errorf("got %#v, want the value exactly as it arrived", got)
	}
}

// TestAReadDropsWhatGCPAddedAndKeepsTheOrderItWasAsked is the whole task
// through the real read path, against a real catalog type.
//
// gcp.firewall is the one that matters most: five of its attributes are sets
// GCP is free to reorder, it has a declared nested object to hide a
// server-added key inside, and its allowed[].ports is an ordered list one
// level further in, so a single read exercises reordering, pruning and
// leaving-alone at once.
func TestAReadDropsWhatGCPAddedAndKeepsTheOrderItWasAsked(t *testing.T) {
	gcptest.Isolate(t)
	s := gcpfake.New(t)
	defer s.Close()
	ty, c := syncCatalogFor(t, "gcp.firewall")
	if a := ty.Attributes["sourceRanges"]; a == nil || !a.Unordered {
		t.Fatalf("%s no longer declares sourceRanges as an unordered list; this test is not exercising one", ty.Name)
	}
	if a := ty.Attributes["logConfig"]; a == nil || len(a.Fields) == 0 {
		t.Fatalf("%s no longer declares logConfig as an object; this test is not exercising nested pruning", ty.Name)
	}

	id, err := ProviderID(ty, nil, attrs(map[string]string{"project": "p", "firewall": "fw1"}))
	if err != nil {
		t.Fatalf("building the id to seed at: %v", err)
	}
	// What GCP answers: the set in its own order, a key inside logConfig we
	// never modelled, and the ordered ports list untouched.
	s.Seed("/"+ty.PathPrefix+id, map[string]any{
		"name":         "fw1",
		"sourceRanges": []any{"192.168.0.0/16", "10.0.0.0/8", "172.16.0.0/12"},
		"allowed":      []any{map[string]any{"IPProtocol": "tcp", "ports": []any{"80", "443"}}},
		"logConfig":    map[string]any{"enable": true, "serverFingerprint": "xyz"},
	})

	configured := map[string]value.Value{
		"project":      value.String("p", value.SourceExplicit),
		"name":         value.String("fw1", value.SourceExplicit),
		"sourceRanges": listValue("10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16"),
		"allowed": value.List([]value.Value{mapValue(map[string]any{
			"IPProtocol": "tcp", "ports": []string{"80", "443"},
		})}, value.SourceExplicit),
		"logConfig": mapValue(map[string]any{"enable": true}),
	}

	p := testProviderWithCatalog(t, s, c)
	st, err := p.Read(context.Background(), &resource.ResourceState{
		Type: ty.Name, ProviderID: id, Attributes: configured,
	})
	if err != nil {
		t.Fatal(err)
	}
	if st == nil {
		t.Fatal("Read reported absence for a resource the fake is holding")
	}

	for _, name := range []string{"sourceRanges", "logConfig", "allowed"} {
		if !st.Attributes[name].Equal(configured[name]) {
			t.Errorf("state[%s] = %#v, want something equal to the configuration that produced it (%#v); "+
				"a plan would propose this change forever", name, st.Attributes[name], configured[name])
		}
	}
	// And the ordered list one level in is still in GCP's order, which here is
	// also the configured one -- proved by the Equal above -- so assert the
	// positive: nothing reordered "80","443" into anything else.
	ports := st.Attributes["allowed"].Raw.([]value.Value)[0].
		Raw.(map[string]value.Value)["ports"].Raw.([]value.Value)
	if len(ports) != 2 || ports[0].Raw != "80" || ports[1].Raw != "443" {
		t.Errorf("allowed[0].ports = %v, want the order it was sent in", ports)
	}
}

// TestACreateReadsBackInTheOrderItAsked. The readback after a create is the
// one read with no previous state to reconcile against, so unless the
// create's own attributes travel with it, the first state a resource ever has
// records whatever order GCP chose -- and the plan immediately after the
// apply proposes reordering it.
//
// gcp.packetmirroring, because its unordered list is one GCP fills fields
// into: mirroredResources.subnetworks is sent as {url} and comes back as
// {url, canonicalUrl}. That is the shape a strict equality match cannot pair
// with its own reference (see answersTo), and it sits inside a declared
// object, so the element match is reached through Fields and then through
// Elem.
func TestACreateReadsBackInTheOrderItAsked(t *testing.T) {
	gcptest.Isolate(t)
	s := gcpfake.New(t)
	defer s.Close()
	ty, c := syncCatalogFor(t, "gcp.packetmirroring")
	subnets := ty.Attributes["mirroredResources"].Fields["subnetworks"]
	if subnets == nil || !subnets.Unordered || subnets.Elem == nil {
		t.Fatalf("%s no longer declares mirroredResources.subnetworks as an unordered list", ty.Name)
	}
	if a := subnets.Elem.Fields["canonicalUrl"]; a == nil || !a.Output {
		t.Fatalf("%s no longer declares subnetworks[].canonicalUrl as output-only; "+
			"this test is not exercising an element GCP adds to", ty.Name)
	}

	id, err := ProviderID(ty, nil, attrs(map[string]string{"project": "p", "region": "r", "name": "pm1"}))
	if err != nil {
		t.Fatalf("building the id: %v", err)
	}
	path := "/" + ty.PathPrefix + id
	// The fake stores exactly what a create sent it, so GCP's own answer is
	// injected between the POST and the readback GET: the first request to the
	// item's path is that GET, and this runs just before it is answered.
	s.OnRequest(path, func() {
		s.Seed(path, map[string]any{
			"name": "pm1",
			"mirroredResources": map[string]any{"subnetworks": []any{
				map[string]any{"url": "regions/r/subnetworks/b", "canonicalUrl": "https://example.invalid/b"},
				map[string]any{"url": "regions/r/subnetworks/a", "canonicalUrl": "https://example.invalid/a"},
			}},
		})
	})

	desired := map[string]value.Value{
		"project": value.String("p", value.SourceExplicit),
		"region":  value.String("r", value.SourceExplicit),
		"name":    value.String("pm1", value.SourceExplicit),
		"mirroredResources": mapValue(map[string]any{"subnetworks": []any{
			map[string]any{"url": "regions/r/subnetworks/a"},
			map[string]any{"url": "regions/r/subnetworks/b"},
		}}),
	}
	p := testProviderWithCatalog(t, s, c)
	st, err := p.Create(context.Background(), &resource.DesiredResource{Type: ty.Name, Attrs: desired})
	if err != nil {
		t.Fatal(err)
	}
	if st == nil {
		t.Fatal("Create returned (nil, nil), which orphans the resource it just made")
	}
	got := st.Attributes["mirroredResources"].Raw.(map[string]value.Value)["subnetworks"].Raw.([]value.Value)
	if len(got) != 2 {
		t.Fatalf("subnetworks = %#v, want two elements", got)
	}
	for i, want := range []string{"regions/r/subnetworks/a", "regions/r/subnetworks/b"} {
		if u, _ := got[i].Raw.(map[string]value.Value)["url"].AsString(); u != want {
			t.Errorf("subnetworks[%d].url = %q, want %q (the order the create asked for)", i, u, want)
		}
	}
}

// TestGCPsOwnLabelsNeverReachState, at the top level AND inside a list.
//
// A goog- label is applied by GCP and cannot be removed by a user, so keeping
// one in state makes every plan propose deleting it and every apply watch GCP
// put it straight back. gcp.automation carries both shapes: its own labels
// map, and selector.targets[].labels, which is reachable only through
// Attr.Elem -- a walk that followed Fields alone would leave the nested one
// exactly as broken as it was while passing on the top-level one.
func TestGCPsOwnLabelsNeverReachState(t *testing.T) {
	gcptest.Isolate(t)
	s := gcpfake.New(t)
	defer s.Close()
	ty, c := syncCatalogFor(t, "gcp.automation")
	if a := ty.Attributes["labels"]; a == nil || !a.Opaque {
		t.Fatalf("%s no longer declares labels as a free-form map", ty.Name)
	}
	targets := ty.Attributes["selector"].Fields["targets"]
	if targets == nil || targets.Elem == nil || targets.Elem.Fields["labels"] == nil {
		t.Fatalf("%s no longer declares selector.targets[].labels; this test is not exercising the Elem edge", ty.Name)
	}

	id, err := ProviderID(ty, nil, attrs(map[string]string{
		"project": "p", "location": "us-central1", "delivery_pipeline": "dp1", "name": "auto1",
	}))
	if err != nil {
		t.Fatalf("building the id to seed at: %v", err)
	}
	s.Seed("/"+ty.PathPrefix+id, map[string]any{
		"name":   "auto1",
		"labels": map[string]any{"env": "prod", "goog-dm": "deployment"},
		"selector": map[string]any{"targets": []any{
			map[string]any{"id": "t1", "labels": map[string]any{"tier": "web", "goog-gke-node": "true"}},
		}},
	})

	configured := map[string]value.Value{
		"project": value.String("p", value.SourceExplicit),
		"labels":  mapValue(map[string]any{"env": "prod"}),
		"selector": mapValue(map[string]any{"targets": []any{
			map[string]any{"id": "t1", "labels": map[string]any{"tier": "web"}},
		}}),
	}
	p := testProviderWithCatalog(t, s, c)
	st, err := p.Read(context.Background(), &resource.ResourceState{
		Type: ty.Name, ProviderID: id, Attributes: configured,
	})
	if err != nil {
		t.Fatal(err)
	}
	if st == nil {
		t.Fatal("Read reported absence for a resource the fake is holding")
	}

	top := st.Attributes["labels"].Raw.(map[string]value.Value)
	if _, kept := top["goog-dm"]; kept {
		t.Errorf("state[labels] = %v: a label GCP owns and a user cannot remove", top)
	}
	if got, ok := top["env"].AsString(); !ok || got != "prod" {
		t.Errorf("state[labels] = %v: the user's own label was dropped with GCP's", top)
	}
	nested := st.Attributes["selector"].Raw.(map[string]value.Value)["targets"].
		Raw.([]value.Value)[0].Raw.(map[string]value.Value)["labels"].Raw.(map[string]value.Value)
	if _, kept := nested["goog-gke-node"]; kept {
		t.Errorf("selector.targets[0].labels = %v: a reserved label inside a list survived", nested)
	}
	if got, ok := nested["tier"].AsString(); !ok || got != "web" {
		t.Errorf("selector.targets[0].labels = %v: the user's own label was dropped", nested)
	}
	if !st.Attributes["labels"].Equal(configured["labels"]) ||
		!st.Attributes["selector"].Equal(configured["selector"]) {
		t.Errorf("state does not equal the configuration that produced it: labels=%#v selector=%#v",
			st.Attributes["labels"], st.Attributes["selector"])
	}
}

// TestAnOpaqueMapKeepsEveryKeyTheUserWrote, through the read path rather than
// against a hand-built Attr. Only GCP's own label namespace is dropped;
// everything else in a free-form map is the user's, and pruning it to what
// the reference happened to mention would silently delete configuration.
func TestAnOpaqueMapKeepsEveryKeyTheUserWrote(t *testing.T) {
	gcptest.Isolate(t)
	s := gcpfake.New(t)
	defer s.Close()
	ty, c := syncCatalogFor(t, "gcp.automation")
	if a := ty.Attributes["annotations"]; a == nil || !a.Opaque {
		t.Fatalf("%s no longer declares annotations as a free-form map", ty.Name)
	}
	id, err := ProviderID(ty, nil, attrs(map[string]string{
		"project": "p", "location": "us-central1", "delivery_pipeline": "dp1", "name": "auto1",
	}))
	if err != nil {
		t.Fatalf("building the id to seed at: %v", err)
	}
	s.Seed("/"+ty.PathPrefix+id, map[string]any{
		"name":        "auto1",
		"annotations": map[string]any{"owner": "infra", "runbook": "https://example.invalid"},
	})

	p := testProviderWithCatalog(t, s, c)
	st, err := p.Read(context.Background(), &resource.ResourceState{
		Type: ty.Name, ProviderID: id,
		// The reference mentions ONE of the two keys on purpose.
		Attributes: map[string]value.Value{"annotations": mapValue(map[string]any{"owner": "infra"})},
	})
	if err != nil {
		t.Fatal(err)
	}
	got := st.Attributes["annotations"].Raw.(map[string]value.Value)
	if len(got) != 2 {
		t.Errorf("annotations = %v, want both keys; a free-form map has no schema to prune it by", got)
	}
}

// TestAnUndeclaredTopLevelKeyIsKept. The pruning rule stops at the top level
// deliberately: infrena's planner ignores an attribute that is in state but
// in neither configuration nor the schema, while Import and Discover read a
// resource's identity out of exactly those keys.
func TestAnUndeclaredTopLevelKeyIsKept(t *testing.T) {
	attrs := map[string]*catalog.Attr{
		"config": {Canonical: "config", Kind: value.KindMap, Fields: map[string]*catalog.Attr{
			"mode": {Canonical: "mode", Kind: value.KindString},
		}},
	}
	in := map[string]value.Value{
		"config":      mapValue(map[string]any{"mode": "A", "serverFingerprint": "xyz"}),
		"selfLink":    value.String("https://example.invalid/x", value.SourceProvider),
		"fingerprint": value.String("abc", value.SourceProvider),
	}
	got := ReconcileAttrs(attrs, map[string]value.Value{"config": mapValue(map[string]any{"mode": "A"})}, in)
	if _, kept := got["selfLink"]; !kept {
		t.Error("an undeclared top-level key was dropped; Import reads a resource's identity out of one")
	}
	if _, kept := got["fingerprint"]; !kept {
		t.Error("an undeclared top-level key was dropped")
	}
	nested := got["config"].Raw.(map[string]value.Value)
	if _, kept := nested["serverFingerprint"]; kept {
		t.Error("an undeclared NESTED key survived, which is the one that costs a diff forever")
	}
}

// TestAPatchReadsBackInTheOrderItAsked. A patch has just made DESIRED true,
// so desired -- not the observation from before it -- is the shape GCP's
// answer has to be expressed in. Reconciling the readback against the
// pre-patch state would order a set the way it was before the user's own
// change, and the very next plan would propose putting it back.
//
// Current, desired and GCP's answer are three different orders here on
// purpose: reconciling against the wrong one produces a different, visible
// result rather than the same one by luck.
func TestAPatchReadsBackInTheOrderItAsked(t *testing.T) {
	gcptest.Isolate(t)
	s := gcpfake.New(t)
	defer s.Close()
	ty, c := syncCatalogFor(t, "gcp.packetmirroring")
	if ty.UpdateVerb == "" {
		t.Fatalf("%s is no longer updatable; this test needs a type Update accepts", ty.Name)
	}
	id, err := ProviderID(ty, nil, attrs(map[string]string{"project": "p", "region": "r", "name": "pm1"}))
	if err != nil {
		t.Fatalf("building the id: %v", err)
	}
	path := "/" + ty.PathPrefix + id
	subnet := func(name string) map[string]any {
		return map[string]any{"url": "regions/r/subnetworks/" + name}
	}
	s.Seed(path, map[string]any{
		"name":              "pm1",
		"mirroredResources": map[string]any{"subnetworks": []any{subnet("b"), subnet("a")}},
	})
	// The first request to the item's path is the PATCH; it re-arms the hook
	// so the second -- the readback GET -- gets GCP's own order.
	s.OnRequest(path, func() {
		s.OnRequest(path, func() {
			s.Seed(path, map[string]any{
				"name": "pm1",
				"mirroredResources": map[string]any{"subnetworks": []any{
					subnet("c"), subnet("b"), subnet("a"),
				}},
			})
		})
	})

	subnetVal := func(names ...string) value.Value {
		items := make([]any, len(names))
		for i, n := range names {
			items[i] = map[string]any{"url": "regions/r/subnetworks/" + n}
		}
		return mapValue(map[string]any{"subnetworks": items})
	}
	current := &resource.ResourceState{Type: ty.Name, ProviderID: id, Attributes: map[string]value.Value{
		"project": value.String("p", value.SourceProvider),
		"region":  value.String("r", value.SourceProvider),
		"name":    value.String("pm1", value.SourceProvider),
		// The order GCP last reported, which is not the configured one.
		"mirroredResources": subnetVal("b", "a"),
	}}
	desired := &resource.DesiredResource{Type: ty.Name, Attrs: map[string]value.Value{
		"project":           value.String("p", value.SourceExplicit),
		"region":            value.String("r", value.SourceExplicit),
		"name":              value.String("pm1", value.SourceExplicit),
		"mirroredResources": subnetVal("a", "b", "c"),
	}}

	p := testProviderWithCatalog(t, s, c)
	st, err := p.Update(context.Background(), current, desired)
	if err != nil {
		t.Fatal(err)
	}
	if st == nil {
		t.Fatal("Update returned (nil, nil), which the host treats as a contract violation")
	}
	got := st.Attributes["mirroredResources"].Raw.(map[string]value.Value)["subnetworks"].Raw.([]value.Value)
	var order []string
	for _, item := range got {
		u, _ := item.Raw.(map[string]value.Value)["url"].AsString()
		order = append(order, strings.TrimPrefix(u, "regions/r/subnetworks/"))
	}
	want := []string{"a", "b", "c"}
	if len(order) != len(want) {
		t.Fatalf("subnetworks = %v, want %v", order, want)
	}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("subnetworks = %v, want %v (the order the patch asked for, not the one it replaced)", order, want)
		}
	}
}

// --- Task 18b C2: a project id and a project number are the same project.

// seedProjectNumber makes the fake answer cloudresourcemanager's
// projects.get for project "p" with the number GCP canonicalises it to.
func seedProjectNumber(s *gcpfake.Server, number string) {
	s.Seed("/v3/projects/p", map[string]any{
		"name": "projects/" + number, "projectId": "p",
	})
}

// readTagKey reads gcp.tagkey at id, with what the configuration asked for
// as the state it is reconciled against.
func readTagKey(t *testing.T, s *gcpfake.Server, id string, want map[string]value.Value) *resource.ResourceState {
	t.Helper()
	p := testProvider(t, s)
	st, err := p.Read(context.Background(), &resource.ResourceState{
		Type: "gcp.tagkey", ProviderID: id, Attributes: want,
	})
	if err != nil {
		t.Fatal(err)
	}
	if st == nil {
		t.Fatal("the tag key read as absent")
	}
	return st
}

// TestAProjectNumberAnswersToTheIdTheConfigurationWrote. Configuration says
// `parent: projects/example-project-1234`; Cloud Resource Manager answers
// `parent: "projects/123456789012"`. The same project, in canonical form, as
// a different string -- and `parent` is ForceNew, so every plan after a
// successful apply proposed destroying and recreating the tag key. Task 18a's
// live re-run found it, and only because its own defect C was fixed first:
// until then the create failed and there was no second plan to look at.
func TestAProjectNumberAnswersToTheIdTheConfigurationWrote(t *testing.T) {
	gcptest.Isolate(t)
	s := gcpfake.New(t)
	defer s.Close()
	seedProjectNumber(s, "123456789012")
	s.Seed("/v3/tagKeys/281480152414347", map[string]any{
		"name": "tagKeys/281480152414347", "parent": "projects/123456789012", "shortName": "env",
	})

	st := readTagKey(t, s, "tagKeys/281480152414347", attrs(map[string]string{
		"parent": "projects/p", "shortName": "env",
	}))
	if got := st.Attributes["parent"].Raw; got != "projects/p" {
		t.Errorf("parent = %q, want %q -- the same project, and parent is ForceNew, so the "+
			"difference is a plan that proposes replacing the tag key forever", got, "projects/p")
	}
}

// TestADifferentProjectIsStillADifferentProject. The equivalence is between
// ONE id and ONE number, the pair this instance actually resolved. A number
// standing in for an arbitrary id would silently accept a state naming a
// project nobody configured.
func TestADifferentProjectIsStillADifferentProject(t *testing.T) {
	gcptest.Isolate(t)
	s := gcpfake.New(t)
	defer s.Close()
	seedProjectNumber(s, "123456789012")
	s.Seed("/v3/tagKeys/1", map[string]any{
		"name": "tagKeys/1", "parent": "projects/999999999999", "shortName": "env",
	})

	st := readTagKey(t, s, "tagKeys/1", attrs(map[string]string{"parent": "projects/p"}))
	if got := st.Attributes["parent"].Raw; got != "projects/999999999999" {
		t.Errorf("parent = %q; GCP answered with a project this instance never resolved, "+
			"and reporting it as the configured one would hide a real difference", got)
	}
}

// TestTheProjectNumberIsResolvedOnceAndOnlyWhenSomethingDisagrees. It costs a
// Cloud Resource Manager request, so it must not happen per resource and
// must not happen at all for the great majority of reads, which carry no
// project resource name that disagrees.
func TestTheProjectNumberIsResolvedOnceAndOnlyWhenSomethingDisagrees(t *testing.T) {
	gcptest.Isolate(t)
	s := gcpfake.New(t)
	defer s.Close()
	seedProjectNumber(s, "123456789012")
	for _, id := range []string{"1", "2", "3"} {
		s.Seed("/v3/tagKeys/"+id, map[string]any{
			"name": "tagKeys/" + id, "parent": "projects/123456789012", "shortName": "env",
		})
	}
	p := testProvider(t, s)
	for _, id := range []string{"1", "2", "3"} {
		if _, err := p.Read(context.Background(), &resource.ResourceState{
			Type: "gcp.tagkey", ProviderID: "tagKeys/" + id,
			Attributes: attrs(map[string]string{"parent": "projects/p"}),
		}); err != nil {
			t.Fatal(err)
		}
	}
	if n := countRequests(s, "/v3/projects/p"); n != 1 {
		t.Errorf("the project number was looked up %d times across three reads, want 1", n)
	}

	// And a read whose strings disagree about something that is NOT a
	// project resource name asks for nothing. This is the half that matters:
	// a pair that merely AGREES never reaches the gate at all, so a test
	// built on one would pass with the gate deleted -- which is exactly what
	// happened when this was sabotaged, and is why the case below exists.
	// Every reconciliation compares strings; without the gate essentially
	// every resource this provider reads would cost a Cloud Resource Manager
	// request.
	s2 := gcpfake.New(t)
	defer s2.Close()
	seedProjectNumber(s2, "123456789012")
	s2.Seed("/v3/tagKeys/9", map[string]any{
		"name": "tagKeys/9", "parent": "projects/p",
		"shortName": "environment", "description": "set by google",
	})
	p2 := testProvider(t, s2)
	if _, err := p2.Read(context.Background(), &resource.ResourceState{
		Type: "gcp.tagkey", ProviderID: "tagKeys/9",
		Attributes: attrs(map[string]string{
			"parent": "projects/p", "shortName": "env", "description": "set by me",
		}),
	}); err != nil {
		t.Fatal(err)
	}
	if n := countRequests(s2, "/v3/projects/p"); n != 0 {
		t.Errorf("a read whose only disagreements were ordinary strings still cost %d "+
			"project-number lookups", n)
	}
}

// TestAnUnresolvableProjectNumberLeavesTheTwoDifferent. A spurious
// replacement plan the user can see beats a silent wrong equality: if the
// lookup fails, the provider must not decide the two spellings are the same
// because it could not check.
func TestAnUnresolvableProjectNumberLeavesTheTwoDifferent(t *testing.T) {
	gcptest.Isolate(t)
	s := gcpfake.New(t)
	defer s.Close()
	// No /v3/projects/p seeded: the lookup 404s.
	s.Seed("/v3/tagKeys/1", map[string]any{
		"name": "tagKeys/1", "parent": "projects/123456789012", "shortName": "env",
	})
	st := readTagKey(t, s, "tagKeys/1", attrs(map[string]string{"parent": "projects/p"}))
	if got := st.Attributes["parent"].Raw; got != "projects/123456789012" {
		t.Errorf("parent = %q; with no number resolved the provider cannot know the two "+
			"spellings are one project, and must not pretend it does", got)
	}
}

// TestSameProjectIsSymmetricAndNarrow pins the equivalence itself.
func TestSameProjectIsSymmetricAndNarrow(t *testing.T) {
	a := ProjectAliases{ID: "example-project-1234", Number: "123456789012"}
	for _, tc := range []struct {
		want, got string
		same      bool
	}{
		{"projects/example-project-1234", "projects/123456789012", true},
		{"projects/123456789012", "projects/example-project-1234", true},
		{"projects/example-project-1234", "projects/999999999999", false},
		{"projects/somewhere-else", "projects/123456789012", false},
	} {
		if got := a.sameProject(tc.want, tc.got); got != tc.same {
			t.Errorf("sameProject(%q, %q) = %v, want %v", tc.want, tc.got, got, tc.same)
		}
	}
	if (ProjectAliases{}).sameProject("projects/a", "projects/b") {
		t.Error("an unresolved instance claimed two projects were the same")
	}
}

// TestOnlyAProjectResourceNameTriggersTheLookup. Every reconciliation
// compares strings; if any differing pair could be a project, the provider
// would send a Cloud Resource Manager request for essentially every resource
// it reads.
func TestOnlyAProjectResourceNameTriggersTheLookup(t *testing.T) {
	for _, tc := range []struct {
		s  string
		ok bool
	}{
		{"projects/example-project-1234", true},
		{"projects/123456789012", true},
		{"example-project-1234", false},
		{"projects/p/locations/us-central1", false},
		{"projects/", false},
		{"web1", false},
	} {
		if got := looksLikeAProjectName(tc.s); got != tc.ok {
			t.Errorf("looksLikeAProjectName(%q) = %v, want %v", tc.s, got, tc.ok)
		}
	}
}

// countRequests is how many times the fake was asked for path.
func countRequests(s *gcpfake.Server, path string) int {
	n := 0
	for _, r := range s.Requests() {
		if r.Path == path {
			n++
		}
	}
	return n
}

// inputOnlyAttrs is a small schema with an input-only field at each of the
// places the catalog has them today: top level, inside a nested object, and
// inside the elements of an ordered list -- the last being compute's
// disks[].initializeParams, the field that made an instance plan its own
// replacement on every run. Plus one inside an UNORDERED list, where the
// reference element is chosen by position and cannot be trusted to copy from.
func inputOnlyAttrs() map[string]*catalog.Attr {
	str := func(inputOnly bool) *catalog.Attr {
		return &catalog.Attr{Kind: value.KindString, InputOnly: inputOnly}
	}
	disk := &catalog.Attr{Kind: value.KindMap, Fields: map[string]*catalog.Attr{
		"source":           str(false),
		"initializeParams": str(true),
	}}
	return map[string]*catalog.Attr{
		"secret":   str(true),
		"plain":    str(false),
		"settings": {Kind: value.KindMap, Fields: map[string]*catalog.Attr{"key": str(true), "mode": str(false)}},
		"disks":    {Kind: value.KindList, Elem: disk},
		"rules":    {Kind: value.KindList, Unordered: true, Elem: disk},
	}
}

func s(v string) value.Value { return value.String(v, value.SourceExplicit) }
func obj(m map[string]value.Value) value.Value {
	return value.Map(m, value.SourceExplicit)
}
func lst(vs ...value.Value) value.Value { return value.List(vs, value.SourceExplicit) }

// TestAnInputOnlyFieldIsCarriedForwardWhereverItSits. GCP never returns these
// fields, so reporting them missing is drift on every plan against a resource
// that is exactly as configured. Carried at the top level, inside an object,
// and inside an ordered list's elements.
func TestAnInputOnlyFieldIsCarriedForwardWhereverItSits(t *testing.T) {
	reference := map[string]value.Value{
		"secret":   s("hunter2"),
		"plain":    s("configured"),
		"settings": obj(map[string]value.Value{"key": s("k1"), "mode": s("fast")}),
		"disks":    lst(obj(map[string]value.Value{"source": s("d1"), "initializeParams": s("image-a")})),
	}
	// What GCP answers: every input-only field missing, and so is `plain`,
	// which is NOT input-only and must stay missing.
	incoming := map[string]value.Value{
		"settings": obj(map[string]value.Value{"mode": s("fast")}),
		"disks":    lst(obj(map[string]value.Value{"source": s("d1")})),
	}
	got := ReconcileAttrs(inputOnlyAttrs(), reference, incoming)

	if !got["secret"].Equal(s("hunter2")) {
		t.Errorf("top-level input-only not carried: %v", got["secret"])
	}
	if _, present := got["plain"]; present {
		t.Error("a field that is NOT input-only was carried forward; GCP not returning it is real")
	}
	if settings, _ := got["settings"].Raw.(map[string]value.Value); !settings["key"].Equal(s("k1")) {
		t.Errorf("nested input-only not carried: %v", got["settings"])
	}
	disks, _ := got["disks"].Raw.([]value.Value)
	if len(disks) != 1 {
		t.Fatalf("disks = %v", got["disks"])
	}
	if d, _ := disks[0].Raw.(map[string]value.Value); !d["initializeParams"].Equal(s("image-a")) {
		t.Errorf("input-only inside an ordered list's element not carried: %v", disks[0])
	}
	if !got["disks"].Equal(reference["disks"]) {
		t.Errorf("the replan-forever case still diffs: got %v, configured %v", got["disks"], reference["disks"])
	}
}

// TestAnInputOnlyAnswerIsBelieved. If the API does return the field, its
// answer is the truth, the same rule CreateOnly follows.
func TestAnInputOnlyAnswerIsBelieved(t *testing.T) {
	got := ReconcileAttrs(inputOnlyAttrs(),
		map[string]value.Value{"secret": s("old")},
		map[string]value.Value{"secret": s("rotated-by-someone")})
	if !got["secret"].Equal(s("rotated-by-someone")) {
		t.Errorf("secret = %v, want the API's answer", got["secret"])
	}
}

// TestNothingIsCopiedFromAnUnmatchedListElement. In an unordered list the
// reference element is chosen by POSITION before the list is matched, so it
// may be a sibling; copying its input-only value would attribute one element's
// configuration to another. None of the catalog's input-only fields sit in an
// unordered list today, and this is what keeps one from being mishandled.
func TestNothingIsCopiedFromAnUnmatchedListElement(t *testing.T) {
	got := ReconcileAttrs(inputOnlyAttrs(),
		map[string]value.Value{"rules": lst(obj(map[string]value.Value{"source": s("a"), "initializeParams": s("for-a")}))},
		map[string]value.Value{"rules": lst(obj(map[string]value.Value{"source": s("b")}))})
	rules, _ := got["rules"].Raw.([]value.Value)
	for _, r := range rules {
		if m, _ := r.Raw.(map[string]value.Value); m["initializeParams"].Known {
			t.Errorf("copied %v into an element of an unordered list", m["initializeParams"])
		}
	}
}
