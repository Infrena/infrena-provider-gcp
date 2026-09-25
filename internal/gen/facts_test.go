package gen

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/infrena/infrena-provider-gcp/internal/catalog"
	"github.com/infrena/infrena/pkg/value"
)

func observedWidget() *catalog.Type {
	return &catalog.Type{
		Name:     "gcp.widget",
		SelfLink: "projects/{{project}}/widgets/{{name}}",
		Setters:  []catalog.Setter{{Method: "setLabels", Fields: []string{"labels"}}, {Method: "setTarget", Fields: []string{"target"}}},
		Attributes: map[string]*catalog.Attr{
			"description": {Canonical: "description", Kind: value.KindString, Required: true},
			"config": {Canonical: "config", Kind: value.KindMap, Fields: map[string]*catalog.Attr{
				"mode": {Canonical: "mode", Kind: value.KindString, ForceNew: true},
			}},
		},
	}
}

// TestAnObservationTheCatalogContradictsIsRefused. A fact seen on Google
// cannot be regenerated away: a ruling or a generator change that disagrees
// with evidence is stale, and must fail loudly rather than win on rank.
func TestAnObservationTheCatalogContradictsIsRefused(t *testing.T) {
	seen := "2026-09-25 TestLiveSomething"
	for _, c := range []struct {
		what string
		obs  Observation
		want string // "" when it must pass
	}{
		{"a matching attribute fact", Observation{Path: "description", Fact: "required", Value: "true", Seen: seen}, ""},
		{"a matching nested fact", Observation{Path: "config.mode", Fact: "immutable", Value: "true", Seen: seen}, ""},
		{"one setter of several", Observation{Fact: "setters", Value: "labels=setLabels", Seen: seen}, ""},
		{"a contradicted fact", Observation{Path: "description", Fact: "required", Value: "false", Seen: seen}, "contradicts evidence"},
		{"a contradicted type fact", Observation{Fact: "update_verb", Value: "PATCH", Seen: seen}, "contradicts evidence"},
		{"a fact outside the vocabulary", Observation{Fact: "insert_keeps_labels", Value: "false", Seen: seen}, "not a fact of the vocabulary"},
		{"an attribute fact with no path", Observation{Fact: "required", Value: "true", Seen: seen}, "not a fact of the vocabulary"},
		{"no such attribute", Observation{Path: "nope", Fact: "required", Value: "true", Seen: seen}, "has no attribute"},
		{"a seen with no test", Observation{Path: "description", Fact: "required", Value: "true", Seen: "2026-09-25"}, "is not"},
	} {
		ty := observedWidget()
		err := applyObserved([]*catalog.Type{ty}, map[string][]Observation{"gcp.widget": {c.obs}})
		switch {
		case c.want == "" && err != nil:
			t.Errorf("%s: refused: %v", c.what, err)
		case c.want != "" && (err == nil || !strings.Contains(err.Error(), c.want)):
			t.Errorf("%s: applyObserved = %v, want an error saying %q", c.what, err, c.want)
		}
		if c.want == "" && c.obs.Path != "" && !containsString(attrAtPath(ty.Attributes, c.obs.Path).Sources[c.obs.Fact], SourceObserved) {
			t.Errorf("%s: passed but was not recorded as observed", c.what)
		}
	}
	if err := applyObserved(nil, map[string][]Observation{"gcp.gone": nil}); err == nil {
		t.Error("an observation of a type that does not ship was accepted")
	}
}

// TestEveryObservationNamesALiveTestThatExists. An observation nothing can
// reproduce is a claim, not evidence: deleting the probe must fail here.
func TestEveryObservationNamesALiveTestThatExists(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "gen", "overlay.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var o Overlay
	if err := yaml.Unmarshal(data, &o); err != nil {
		t.Fatal(err)
	}
	files, err := filepath.Glob(filepath.Join("..", "..", "live", "*_test.go"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no live tests found: %v", err)
	}
	var live strings.Builder
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		live.Write(b)
	}
	n := 0
	for ty, list := range o.Observed {
		for _, obs := range list {
			n++
			_, name, _ := strings.Cut(obs.Seen, " ")
			if !regexp.MustCompile(`func ` + regexp.QuoteMeta(name) + `\(t \*testing\.T\)`).MatchString(live.String()) {
				t.Errorf("%s %s %s: seen by %q, which is not a test in live/", ty, obs.Path, obs.Fact, name)
			}
		}
	}
	if n < 10 {
		t.Errorf("only %d observations in the overlay; 19 were recorded on 2026-09-25", n)
	}
}

// TestUnknownsSplitIntoTwoTiers. terraform-only is magic-modules or a
// default alone; unverified is a type fact only Discovery backs. Anything a
// ruling or an observation backs is known, and an attribute fact that is
// not set says nothing.
func TestUnknownsSplitIntoTwoTiers(t *testing.T) {
	facts := []Fact{
		{"gcp.a", "", "update_verb", "PATCH", []string{SourceMM}},
		{"gcp.a", "", "patch_one_field", "false", []string{SourceDefault}},
		{"gcp.a", "", "create_verb", "POST", []string{SourceDiscovery}},
		{"gcp.a", "", "delete_url", "x", []string{SourceObserved, SourceDiscovery}},
		{"gcp.a", "size", "required", "true", []string{SourceMM}},
		{"gcp.a", "size", "immutable", "true", []string{SourceRuling, SourceMM}},
		{"gcp.a", "size", "output", "false", []string{"!" + SourceMM, SourceRuling}},
		{"gcp.a", "name", "output", "true", []string{SourceDiscovery}},
		{"gcp.a", "size", "sensitive", "true", []string{SourceMM}},
	}
	to, un := Unknowns(facts)
	got := func(fs []Fact) (out []string) {
		for _, f := range fs {
			out = append(out, f.Path+":"+f.Fact)
		}
		return out
	}
	if g := strings.Join(got(to), ","); g != ":update_verb,:patch_one_field,size:required" {
		t.Errorf("terraform-only = %s", g)
	}
	if g := strings.Join(got(un), ","); g != ":create_verb" {
		t.Errorf("unverified = %s", g)
	}
}

// TestOverruledSourcesAreKept. A health check's type: magic-modules said
// output, a ruling said not. Both stay on the record.
func TestOverruledSourcesAreKept(t *testing.T) {
	a := &catalog.Attr{}
	addSource(a, "output", SourceMM)
	overrule(a, "output", SourceRuling)
	if g := strings.Join(a.Sources["output"], "+"); g != "!mm+ruling" {
		t.Errorf("sources = %s, want !mm+ruling", g)
	}
}
