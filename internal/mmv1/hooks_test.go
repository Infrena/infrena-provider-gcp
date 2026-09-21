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

// TestPostCreateFailureIsReportedAsWireAffecting. post_create_failure is the
// riskiest addition to wireHooks: it can run delete_on_failure.go.tmpl, which
// issues a DELETE when create fails, colliding with this provider's rule that
// Create never errors once GCP has made something. Its correctness must not
// rest solely on the corpus band matching by coincidence.
func TestPostCreateFailureIsReportedAsWireAffecting(t *testing.T) {
	r := loadRes(t, "PostCreateFailure.yaml")
	got := r.WireHooks()
	if !slices.Equal(got, []string{"post_create_failure"}) {
		t.Errorf("PostCreateFailure reports %v, want [post_create_failure]", got)
	}
}

// TestNonScalarCustomCodeValuesDontBreakParsingOrWireHooks is the regression
// test for CustomCode being map[string]yaml.Node rather than map[string]string.
// A boolean value (tgc_ignore_terraform_decoder) and a list value
// (custom_identity) must not fail ParseResource, and must not be reported by
// WireHooks — not because their value happens to be non-scalar, but because
// neither key is wire-affecting; only a real wire hook alongside them should
// be reported.
func TestNonScalarCustomCodeValuesDontBreakParsingOrWireHooks(t *testing.T) {
	r := loadRes(t, "NonScalarCustomCode.yaml")
	got := r.WireHooks()
	if !slices.Equal(got, []string{"encoder"}) {
		t.Errorf("NonScalarCustomCode reports %v, want [encoder]", got)
	}
}
