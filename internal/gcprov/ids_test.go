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

// TestParseProviderIDAcceptsAnImportFormatShorthand. magic-modules supplies
// more than one accepted id shape for 11 of the 233 real types, newline-
// joined in import_format -- e.g. gcp.bigquery.table's own import_format is
// its full relative name, then "{{table_id}}" alone. Both are legitimate: a
// user importing by the short form is not making a typo, and refusing it
// because it does not match self_link would be wrong.
func TestParseProviderIDAcceptsAnImportFormatShorthand(t *testing.T) {
	ty := &catalog.Type{
		Name:         "gcp.bigquery.table",
		Scope:        catalog.ScopeGlobal,
		SelfLink:     "projects/{{project}}/datasets/{{dataset_id}}/tables/{{table_id}}",
		ImportFormat: "projects/{{project}}/datasets/{{dataset_id}}/tables/{{table_id}}\n{{table_id}}",
	}

	long, err := ParseProviderID(ty, "projects/p/datasets/d/tables/t")
	if err != nil {
		t.Fatalf("the full relative name did not parse: %v", err)
	}
	if long["project"].Raw != "p" || long["dataset_id"].Raw != "d" || long["table_id"].Raw != "t" {
		t.Errorf("the full form parsed wrong: %v", long)
	}

	short, err := ParseProviderID(ty, "t")
	if err != nil {
		t.Fatalf("the short {{table_id}} form did not parse: %v", err)
	}
	if short["table_id"].Raw != "t" {
		t.Errorf("the short form parsed wrong: %v", short)
	}
}
