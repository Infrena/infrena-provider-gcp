package gcprov

import (
	"testing"

	"github.com/infrena/infrena-provider-gcp/internal/catalog"
)

func TestTheProviderIDIsTheRelativeResourceName(t *testing.T) {
	ty := &catalog.Type{
		Name:     "gcp.instance",
		SelfLink: "projects/{{project}}/zones/{{zone}}/instances/{{name}}",
		Scope:    catalog.ScopeZonal,
	}
	got, err := ProviderID(ty, map[string]any{"name": "web1"},
		attrs(map[string]string{"project": "p", "zone": "us-central1-a", "name": "web1"}))
	if err != nil {
		t.Fatal(err)
	}
	if want := "projects/p/zones/us-central1-a/instances/web1"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// TestAnAbsoluteSelfLinkBecomesARelativeName. GCP returns selfLink as an
// absolute URL. Storing that as the provider ID would make the ID change if
// Google ever changed the host, and would not match what import takes.
func TestAnAbsoluteSelfLinkBecomesARelativeName(t *testing.T) {
	ty := &catalog.Type{Name: "gcp.instance", Scope: catalog.ScopeZonal,
		SelfLink: "projects/{{project}}/zones/{{zone}}/instances/{{name}}"}
	got, err := ProviderID(ty, map[string]any{
		"selfLink": "https://www.googleapis.com/compute/v1/projects/p/zones/us-central1-a/instances/web1",
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if want := "projects/p/zones/us-central1-a/instances/web1"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// TestImportRefusesAnIDThatDoesNotMatchTheType, before any API call, so a
// typo costs a message rather than a confusing 404.
func TestImportRefusesAnIDThatDoesNotMatchTheType(t *testing.T) {
	ty := &catalog.Type{Name: "gcp.instance", Scope: catalog.ScopeZonal,
		SelfLink: "projects/{{project}}/zones/{{zone}}/instances/{{name}}"}
	for _, bad := range []string{
		"projects/p/global/networks/default",         // a network, not an instance
		"projects/p/regions/us-central1/instances/x", // regional path for a zonal type
		"web1", // bare name, no hierarchy
	} {
		if _, err := ParseProviderID(ty, bad); err == nil {
			t.Errorf("accepted %q for %s", bad, ty.Name)
		}
	}
	if _, err := ParseProviderID(ty, "projects/p/zones/us-central1-a/instances/web1"); err != nil {
		t.Errorf("refused a valid id: %v", err)
	}
}
