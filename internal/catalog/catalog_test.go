package catalog

import (
	"testing"

	"github.com/infrena/infrena/pkg/schema"
	"github.com/infrena/infrena/pkg/value"
)

func sample() *Catalog {
	return &Catalog{
		Generated:  "2026-09-21",
		MMV1Commit: "deadbeef",
		Types: []*Type{{
			Name: "gcp.widget", Service: "tiny", Description: "A widget.",
			Tier: 1, TierReason: "generic-safe",
			APIBaseURL: "https://tiny.googleapis.com/v1/",
			BaseURL:    "projects/{{project}}/locations/{{region}}/widgets",
			SelfLink:   "projects/{{project}}/locations/{{region}}/widgets/{{name}}",
			UpdateVerb: "PATCH", UpdateMask: true,
			Await: AwaitLongRunning, Scope: ScopeRegional, TimeoutSeconds: 1200,
			ImportFormat: "projects/{{project}}/locations/{{region}}/widgets/{{name}}",
			AssetType:    "tiny.googleapis.com/Widget",
			Attributes: map[string]*Attr{
				"project":    {Canonical: "project", Kind: value.KindString, Required: true, ForceNew: true},
				"region":     {Canonical: "region", Kind: value.KindString, Required: true, ForceNew: true},
				"sizeGb":     {Canonical: "sizeGb", Aliases: []string{"size", "size_gb"}, Kind: value.KindInt},
				"createTime": {Canonical: "createTime", Aliases: []string{"create_time"}, Kind: value.KindString, Output: true},
				"config": {Canonical: "config", Kind: value.KindMap, Fields: map[string]*Attr{
					"replicaCount": {Canonical: "replicaCount", Kind: value.KindInt, ForceNew: true},
				}},
				"network": {Canonical: "network", Kind: value.KindString,
					Ref: &RefTarget{Type: "gcp.network", Attribute: "selfLink"}},
			},
		}, {
			// A minimal target for "network"'s reference above. schema.ValidateAll
			// checks that a reference names a type actually present in the set, so
			// a fixture with a dangling reference is not a fixture the real
			// validator would ever accept from the generator either.
			Name: "gcp.network", Service: "tiny", Description: "A network.",
			Tier: 1, TierReason: "generic-safe",
			APIBaseURL: "https://tiny.googleapis.com/v1/",
			BaseURL:    "projects/{{project}}/global/networks",
			SelfLink:   "projects/{{project}}/global/networks/{{name}}",
			Await:      AwaitNone, Scope: ScopeGlobal,
			Attributes: map[string]*Attr{
				"project":  {Canonical: "project", Kind: value.KindString, Required: true, ForceNew: true},
				"selfLink": {Canonical: "selfLink", Kind: value.KindString, Output: true},
			},
		}},
	}
}

// TestTheCatalogSurvivesItsOwnEncoding. Everything the runtime needs travels through
// a gzipped JSON file; a field that does not round-trip is a field the plugin silently
// loses at load. Nested ForceNew is asserted specifically because it is the one most
// easily dropped by an encoder that only walks the top level.
func TestTheCatalogSurvivesItsOwnEncoding(t *testing.T) {
	in := sample()
	blob, err := Encode(in)
	if err != nil {
		t.Fatal(err)
	}
	out, err := Decode(blob)
	if err != nil {
		t.Fatal(err)
	}
	ty, ok := out.Type("gcp.widget")
	if !ok {
		t.Fatal("gcp.widget missing after round trip")
	}
	if ty.Await != AwaitLongRunning || ty.Scope != ScopeRegional {
		t.Errorf("await=%v scope=%v, want longrunning/regional", ty.Await, ty.Scope)
	}
	if ty.BaseURL == "" || ty.SelfLink == "" || !ty.UpdateMask {
		t.Errorf("url mechanics lost: %+v", ty)
	}
	if got := ty.Attributes["config"].Fields["replicaCount"]; got == nil || !got.ForceNew {
		t.Errorf("nested ForceNew lost: %+v", got)
	}
	if r := ty.Attributes["network"].Ref; r == nil || r.Type != "gcp.network" {
		t.Errorf("reference lost: %+v", r)
	}
	if got := ty.Attributes["sizeGb"].Aliases; len(got) != 2 {
		t.Errorf("aliases = %v, want two", got)
	}
}

// TestDefinitionsAreWhatTheHostAccepts runs infrena's own validator, so a catalog
// that passes here is one the host will load rather than one we merely agreed with.
func TestDefinitionsAreWhatTheHostAccepts(t *testing.T) {
	defs := sample().Definitions()
	if len(defs) != 2 {
		t.Fatalf("got %d definitions, want 2", len(defs))
	}
	if err := schema.ValidateAll(defs); err != nil {
		t.Fatalf("infrena refuses these definitions: %v", err)
	}
	// Definitions sorts by Type, so "gcp.network" sorts before "gcp.widget" —
	// find the one under test by name rather than assuming an index.
	var d *schema.ResourceDefinition
	for _, cand := range defs {
		if cand.Type == "gcp.widget" {
			d = cand
		}
	}
	if d == nil {
		t.Fatal("gcp.widget missing from definitions")
	}
	// Output-only attributes must be Computed and NOT Required, or every plan asks
	// the user for a value GCP chooses.
	ct, ok := d.Attribute("createTime")
	if !ok {
		t.Fatal("createTime missing")
	}
	if !ct.Computed || ct.Required {
		t.Errorf("createTime computed=%v required=%v, want true/false", ct.Computed, ct.Required)
	}
	// Settable ones are Optional+Computed (spec §4.3), so dropping one from config
	// is not a diff.
	sz, _ := d.Attribute("sizeGb")
	if !sz.Optional || !sz.Computed {
		t.Errorf("sizeGb optional=%v computed=%v, want true/true", sz.Optional, sz.Computed)
	}
	// project and region are the GCP analogue of AWS's region: required and ForceNew.
	for _, name := range []string{"project", "region"} {
		a, _ := d.Attribute(name)
		if !a.Required || !a.ForceNew {
			t.Errorf("%s required=%v forceNew=%v, want true/true", name, a.Required, a.ForceNew)
		}
	}
	if !d.Capabilities.Import || d.ImportID.Description == "" {
		t.Error("import capability or its description missing")
	}
	// Nested ForceNew, asserted here too (not just in the round-trip test): a
	// toSchema that drops the Fields branch entirely still passes the round
	// trip (it never touches schema.Attribute), so this is the only assertion
	// that would catch it.
	cfg, ok := d.Attribute("config")
	if !ok {
		t.Fatal("config missing")
	}
	rc, ok := cfg.Fields["replicaCount"]
	if !ok || !rc.ForceNew {
		t.Errorf("config.replicaCount forceNew=%v (present=%v), want true/true", rc.ForceNew, ok)
	}
}

// TestNestedReferenceIsNotProjected. schema.ValidateAll refuses a definition
// whose nested attribute carries References outright, because the host only
// ever consults a top-level attribute's — so a Ref set inside Fields (magic-
// modules does have these; e.g. compute/InterconnectAttachment's nested
// `network`) must not survive the conversion, or the whole catalog fails to
// load over an edge nothing was ever going to read.
func TestNestedReferenceIsNotProjected(t *testing.T) {
	cat := &Catalog{
		Types: []*Type{{
			Name: "gcp.thing", Service: "tiny",
			APIBaseURL: "https://tiny.googleapis.com/v1/",
			BaseURL:    "projects/{{project}}/things",
			Attributes: map[string]*Attr{
				"project": {Canonical: "project", Kind: value.KindString, Required: true, ForceNew: true},
				"settings": {Canonical: "settings", Kind: value.KindMap, Fields: map[string]*Attr{
					"backend": {Canonical: "backend", Kind: value.KindString,
						Ref: &RefTarget{Type: "gcp.backend", Attribute: "selfLink"}},
				}},
			},
		}},
	}
	defs := cat.Definitions()
	if err := schema.ValidateAll(defs); err != nil {
		t.Fatalf("infrena refuses these definitions: %v", err)
	}
	nested := defs[0].Attributes["settings"].Fields["backend"]
	if nested.References != nil {
		t.Errorf("nested References = %+v, want nil: only a top-level attribute's References is ever projected", nested.References)
	}
}
