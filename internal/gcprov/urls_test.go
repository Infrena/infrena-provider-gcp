package gcprov

import (
	"testing"

	"github.com/infrena/infrena/pkg/value"
)

func TestURLTemplatesExpandFromAttributes(t *testing.T) {
	got, err := ExpandURL("projects/{{project}}/zones/{{zone}}/instances/{{name}}",
		attrs(map[string]string{"project": "p", "zone": "us-central1-a", "name": "web1"}))
	if err != nil {
		t.Fatal(err)
	}
	if want := "projects/p/zones/us-central1-a/instances/web1"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// TestAMissingPlaceholderIsRefused. Silently expanding {{zone}} to ""
// produces projects/p/zones//instances/web1, which GCP answers with a
// confusing 404 rather than a message naming the missing attribute.
func TestAMissingPlaceholderIsRefused(t *testing.T) {
	_, err := ExpandURL("projects/{{project}}/zones/{{zone}}/instances/{{name}}",
		attrs(map[string]string{"project": "p", "name": "web1"}))
	if err == nil {
		t.Fatal("a missing placeholder expanded silently")
	}
	if !contains(err.Error(), "zone") {
		t.Errorf("the error does not name the missing attribute: %v", err)
	}
}

// TestAnUnknownValueIsAlsoRefused. A placeholder resolved to an unknown value
// (computed, not yet available) is exactly as unusable in a url as a missing
// one, and must fail the same way rather than expanding to "<nil>" or "".
func TestAnUnknownValueIsRefused(t *testing.T) {
	a := attrs(map[string]string{"project": "p", "zone": "us-central1-a"})
	a["name"] = value.Unknown(value.KindString, value.SourceComputed)
	_, err := ExpandURL("projects/{{project}}/zones/{{zone}}/instances/{{name}}", a)
	if err == nil {
		t.Fatal("an unknown placeholder expanded silently")
	}
	if !contains(err.Error(), "name") {
		t.Errorf("the error does not name the unknown attribute: %v", err)
	}
}

// TestAValueContainingASlashCannotInjectAnExtraSegment. An attribute is one
// path segment, whatever characters it happens to contain -- a resource named
// "a/b" must not turn one template placeholder into two path segments.
func TestASlashInAValueIsEscapedNotInjected(t *testing.T) {
	got, err := ExpandURL("projects/{{project}}/instances/{{name}}",
		attrs(map[string]string{"project": "p", "name": "a/b"}))
	if err != nil {
		t.Fatal(err)
	}
	if want := "projects/p/instances/a%2Fb"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestAnUnterminatedPlaceholderIsAnError(t *testing.T) {
	_, err := ExpandURL("projects/{{project", attrs(map[string]string{"project": "p"}))
	if err == nil {
		t.Fatal("an unterminated {{ expanded without error")
	}
}

func TestATemplateWithNoPlaceholdersPassesThrough(t *testing.T) {
	got, err := ExpandURL("accessPolicies", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got != "accessPolicies" {
		t.Errorf("got %q", got)
	}
}
