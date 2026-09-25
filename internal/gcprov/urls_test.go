package gcprov

import (
	"github.com/infrena/infrena-provider-gcp/internal/catalog"
	"regexp"
	"strings"
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

// TestSingleBracePlaceholdersExpand. magic-modules writes "{{project}}";
// paths that come from a service's own Discovery document write "{project}"
// instead (e.g. the real catalog's gcp.dataproc.cluster:
// "v1/projects/{projectId}/regions/{region}/clusters"). Both must expand.
func TestSingleBracePlaceholdersExpand(t *testing.T) {
	got, err := ExpandURL("v1/projects/{projectId}/regions/{region}/clusters",
		attrs(map[string]string{"projectId": "p", "region": "us-central1"}))
	if err != nil {
		t.Fatal(err)
	}
	if want := "v1/projects/p/regions/us-central1/clusters"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestSingleBracePlaceholdersAreEscapedLikeDoubleBraceOnes(t *testing.T) {
	got, err := ExpandURL("v1/instances/{name}", attrs(map[string]string{"name": "a/b"}))
	if err != nil {
		t.Fatal(err)
	}
	if want := "v1/instances/a%2Fb"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// TestAMissingSingleBracePlaceholderIsRefused, same rule as the double-brace
// form: a missing "{region}" is an error naming it, not an empty segment.
func TestAMissingSingleBracePlaceholderIsRefused(t *testing.T) {
	_, err := ExpandURL("v1/projects/{projectId}/regions/{region}/clusters",
		attrs(map[string]string{"projectId": "p"}))
	if err == nil {
		t.Fatal("a missing single-brace placeholder expanded silently")
	}
	if !contains(err.Error(), "region") {
		t.Errorf("the error does not name the missing attribute: %v", err)
	}
}

// TestReservedExpansionIsNotEscaped. "{+parent}" is RFC 6570 reserved
// expansion (the real catalog uses it 100+ times, e.g.
// "v1/{+parent}/clusters"): the value is already a valid multi-segment path
// like "projects/p/locations/l", computed elsewhere, and must be substituted
// exactly -- escaping its "/" would turn one valid path into a broken one.
func TestReservedExpansionIsNotEscaped(t *testing.T) {
	got, err := ExpandURL("v1/{+parent}/clusters",
		attrs(map[string]string{"parent": "projects/p/locations/l"}))
	if err != nil {
		t.Fatal(err)
	}
	if want := "v1/projects/p/locations/l/clusters"; got != want {
		t.Errorf("got %q, want %q: reserved expansion must not escape \"/\"", got, want)
	}
}

func TestAMissingReservedExpansionPlaceholderIsRefused(t *testing.T) {
	_, err := ExpandURL("v1/{+parent}/clusters", nil)
	if err == nil {
		t.Fatal("a missing {+parent} expanded silently")
	}
	if !contains(err.Error(), "parent") {
		t.Errorf("the error does not name the missing attribute: %v", err)
	}
}

var placeholderRE = regexp.MustCompile(`\{\{[^{}]+\}\}|\{[^{}]+\}`)

// TestExpandURLHandlesEveryRealCatalogTemplate is the regression test for the
// claim that motivated handling both brace styles: with only "{{name}}"
// supported, a real fraction of the 233-type embedded catalog could not
// expand at all. Every url template field of every real type is fed every
// placeholder it names (each given a dummy value), and every one must expand
// without error.
func TestExpandURLHandlesEveryRealCatalogTemplate(t *testing.T) {
	c := mustCatalog(t)
	var failed int
	for _, ty := range c.Types {
		for _, tmpl := range []string{ty.BaseURL, ty.CreateURL, ty.UpdateURL, ty.DeleteURL, ty.SelfLink} {
			if tmpl == "" {
				continue
			}
			a := map[string]value.Value{}
			for _, ph := range placeholderRE.FindAllString(tmpl, -1) {
				name := strings.Trim(ph, "{}")
				name = strings.TrimPrefix(strings.TrimSpace(name), "+")
				// {{%name}} is magic-modules' escaped form of name.
				name = strings.TrimPrefix(name, "%")
				a[name] = value.String("x", value.SourceExplicit)
			}
			if _, err := ExpandURL(tmpl, a); err != nil {
				t.Errorf("%s: %q: %v", ty.Name, tmpl, err)
				failed++
				if failed > 20 {
					t.Fatal("too many failures; stopping early")
				}
			}
		}
	}
}

// TestAnEscapedPlaceholderRoundTripsThroughAnID. magic-modules' {{%name}}
// asks for the value escaped: a logging metric named "team/errors" is
// .../metrics/team%2Ferrors, and parsing that id must give the name back.
// Read as a placeholder called "%name", the self_link could not be built.
func TestAnEscapedPlaceholderRoundTripsThroughAnID(t *testing.T) {
	ty := &catalog.Type{Name: "gcp.logging.metric", SelfLink: "projects/{{project}}/metrics/{{%name}}"}
	rel, err := ExpandURL(ty.SelfLink, map[string]value.Value{
		"project": value.String("p", value.SourceExplicit),
		"name":    value.String("team/errors", value.SourceExplicit),
	})
	if err != nil {
		t.Fatal(err)
	}
	if rel != "projects/p/metrics/team%2Ferrors" {
		t.Errorf("expanded %q, want the name escaped", rel)
	}
	got, err := ParseProviderID(ty, rel)
	if err != nil {
		t.Fatal(err)
	}
	if got["name"].Raw != "team/errors" {
		t.Errorf("parsed name %v, want team/errors", got["name"].Raw)
	}
}
