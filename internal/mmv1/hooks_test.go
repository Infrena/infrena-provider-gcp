package mmv1

import (
	"slices"
	"testing"
)

func TestOnlyWireAffectingHooksAreReported(t *testing.T) {
	clean := loadRes(t, "Widget.yaml")
	if got := clean.WireHooks(); len(got) != 0 {
		t.Errorf("Widget reports hooks %v, want none", got)
	}
	hooked := loadRes(t, "Hooked.yaml")
	got := hooked.WireHooks()
	// `constants` is present in the fixture and must NOT be reported: it emits Go
	// constants and never touches the wire. Reporting it would push ~145 resources
	// into tier 2 for no reason.
	if !slices.Equal(got, []string{"encoder"}) {
		t.Errorf("Hooked reports %v, want [encoder]", got)
	}
}
