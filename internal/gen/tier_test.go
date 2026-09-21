package gen

import (
	"testing"

	"github.com/infrena/infrena-provider-gcp/internal/disco"
	"github.com/infrena/infrena-provider-gcp/internal/mmv1"
	"gopkg.in/yaml.v3"
)

func col(methods ...string) disco.Collection {
	c := disco.Collection{Path: []string{"projects", "widgets"}, Methods: map[string]*disco.Method{}}
	for _, m := range methods {
		c.Methods[m] = &disco.Method{HTTPMethod: "POST"}
	}
	return c
}

// customCode builds a mmv1.Resource.CustomCode map from hook keys. WireHooks
// only tests key presence, never the value (see mmv1/parse.go), so the value
// itself is an arbitrary scalar node standing in for a template path.
func customCode(keys ...string) map[string]yaml.Node {
	m := map[string]yaml.Node{}
	for _, k := range keys {
		m[k] = yaml.Node{Kind: yaml.ScalarNode, Value: "x.tmpl"}
	}
	return m
}

func TestAPlainCrudCollectionIsTierOne(t *testing.T) {
	d := Classify(col("get", "list", "insert", "patch", "delete"), &mmv1.Resource{Name: "Widget"}, nil)
	if d.Tier != TierGeneric {
		t.Errorf("tier = %d (%s), want 1", d.Tier, d.Reason)
	}
}

func TestNoCreateMeansExcluded(t *testing.T) {
	d := Classify(col("get", "list"), &mmv1.Resource{Name: "Widget"}, nil)
	if d.Tier != TierExcluded {
		t.Errorf("tier = %d, want 3", d.Tier)
	}
	if d.Reason == "" {
		t.Error("an excluded type with no reason is one nobody can act on")
	}
}

func TestAHookedTypeIsRefusedWithoutARuling(t *testing.T) {
	mm := &mmv1.Resource{Name: "Widget", CustomCode: customCode("encoder")}
	d := Classify(col("get", "insert", "patch", "delete"), mm, nil)
	if d.Tier != TierHooked {
		t.Errorf("tier = %d, want 2", d.Tier)
	}
	if !contains(d.Reason, "encoder") {
		t.Errorf("reason %q does not name the hook", d.Reason)
	}
}

func TestARulingThatNamesEveryHookAdmitsTheType(t *testing.T) {
	mm := &mmv1.Resource{Name: "Widget", CustomCode: customCode("encoder")}
	r := &Ruling{Hooks: []string{"encoder"}, Note: "inspected: sets a default the REST API also defaults"}
	d := Classify(col("get", "insert", "patch", "delete"), mm, r)
	if d.Tier != TierGeneric {
		t.Errorf("tier = %d (%s), want 1 once ruled", d.Tier, d.Reason)
	}
}

// TestAStaleRulingIsRefused is the case that matters. A ruling written when the
// resource had one hook must not keep admitting it after upstream adds a second:
// that is exactly how a vendor bump would silently ship an unreviewed type.
func TestAStaleRulingIsRefused(t *testing.T) {
	mm := &mmv1.Resource{Name: "Widget", CustomCode: customCode("encoder", "custom_import")}
	r := &Ruling{Hooks: []string{"encoder"}, Note: "written before custom_import existed"}
	d := Classify(col("get", "insert", "patch", "delete"), mm, r)
	if d.Tier != TierHooked {
		t.Fatalf("tier = %d, want 2: a ruling covering only some hooks is stale", d.Tier)
	}
	if !contains(d.Reason, "custom_import") {
		t.Errorf("reason %q does not name the unruled hook", d.Reason)
	}
}

func TestExcludeAndBetaAreExcluded(t *testing.T) {
	for _, mm := range []*mmv1.Resource{
		{Name: "W", Exclude: true},
		{Name: "W", MinVersion: "beta"},
	} {
		d := Classify(col("get", "insert", "patch", "delete"), mm, nil)
		if d.Tier != TierExcluded {
			t.Errorf("%+v: tier = %d, want 3", mm, d.Tier)
		}
	}
}

// TestExcludeWinsEvenWhenHookedAndUnreadable guards the order Classify checks
// things in: exclusion must win regardless of what the CRUD/hook switch below
// it would otherwise decide. A full-CRUD fixture can't expose an ordering bug
// here, because it never reaches the switch's TierHooked branch either way —
// this fixture is missing "get" specifically so the switch would return
// TierHooked if the exclude check ran after it.
func TestExcludeWinsEvenWhenHookedAndUnreadable(t *testing.T) {
	mm := &mmv1.Resource{Name: "W", Exclude: true, CustomCode: customCode("encoder")}
	d := Classify(col("insert", "patch", "delete"), mm, nil)
	if d.Tier != TierExcluded {
		t.Errorf("tier = %d, want 3: exclusion must win regardless of hook/CRUD ordering", d.Tier)
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (func() bool {
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
		return false
	})()
}
