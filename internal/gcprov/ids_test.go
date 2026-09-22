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
	// compute's real shape: the version is in APIBaseURL's own path
	// ("compute/v1/") and PathPrefix is empty, and GCP answers the self link
	// from www.googleapis.com rather than the host the catalog names.
	ty := &catalog.Type{Name: "gcp.instance", Scope: catalog.ScopeZonal,
		APIBaseURL: "https://compute.googleapis.com/compute/v1/",
		SelfLink:   "projects/{{project}}/zones/{{zone}}/instances/{{name}}"}
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

// TestReduceSelfLinkOnlyMatchesAtASegmentBoundary. An unanchored search for
// the api prefix ("v1/" here) would happily match it inside "apiv1/", a
// host-side path segment that merely ends with it, and reduce from the wrong
// offset -- leaving the version in the id. The real, anchored match (preceded
// by "/") comes later in raw and must be the one used.
func TestReduceSelfLinkOnlyMatchesAtASegmentBoundary(t *testing.T) {
	ty := &catalog.Type{Name: "gcp.instance", Scope: catalog.ScopeZonal,
		APIBaseURL: "https://compute.googleapis.com/",
		PathPrefix: "v1/",
		SelfLink:   "projects/{{project}}/zones/{{zone}}/instances/{{name}}"}
	got, err := ProviderID(ty, map[string]any{
		"selfLink": "https://www.googleapis.com/apiv1/v1/projects/p/zones/us-central1-a/instances/web1",
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

// TestReduceSelfLinkTakesTheFirstAnchoredMatch. The counterexample, verbatim:
// a project whose id is literally "projects" -- eight lowercase letters, a
// syntactically valid GCP project id. Taking the LAST anchored match instead
// of the first reduces to "projects/zones/us-central1-a/instances/web1",
// silently deleting the project segment from the id, which then imports and
// reads as some other resource.
//
// The argument that once justified "last" -- that a resource's own trailing
// name might repeat a hierarchy keyword -- does not hold: every prefix
// searched for ends in "/", and a trailing name has nothing after it, so it
// can never produce an anchored match at all. An earlier hierarchy VALUE can,
// and does here.
//
// Driven through ProviderID rather than the helper, so it also fails if
// reduceSelfLink goes back to searching for self_link's literal head.
func TestReduceSelfLinkTakesTheFirstAnchoredMatch(t *testing.T) {
	ty := &catalog.Type{Name: "gcp.instance", Scope: catalog.ScopeZonal,
		APIBaseURL: "https://compute.googleapis.com/compute/v1/",
		SelfLink:   "projects/{{project}}/zones/{{zone}}/instances/{{name}}"}
	got, err := ProviderID(ty, map[string]any{
		"selfLink": "https://www.googleapis.com/compute/v1/projects/projects/zones/us-central1-a/instances/web1",
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if want := "projects/projects/zones/us-central1-a/instances/web1"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// TestReduceSelfLinkTakesTheFirstAnchoredPrefix is the same choice asked of
// the prefix itself rather than of a hierarchy keyword: "v1" is a legal
// cloudkms key ring id, so the prefix "v1/" occurs twice at a segment
// boundary. The first one is the api prefix; the second is the user's own key
// ring, and reducing past it would return "cryptoKeys/k" as the id of a
// resource that is nothing of the sort.
func TestReduceSelfLinkTakesTheFirstAnchoredPrefix(t *testing.T) {
	ty := &catalog.Type{Name: "gcp.cryptokey", Scope: catalog.ScopeRegional,
		APIBaseURL: "https://cloudkms.googleapis.com/",
		PathPrefix: "v1/",
		SelfLink:   "projects/{{project}}/locations/{{location}}/keyRings/{{key_ring}}/cryptoKeys/{{name}}"}
	got, err := ProviderID(ty, map[string]any{
		"selfLink": "https://cloudkms.googleapis.com/v1/projects/p/locations/us-central1/keyRings/v1/cryptoKeys/k",
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if want := "projects/p/locations/us-central1/keyRings/v1/cryptoKeys/k"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}
