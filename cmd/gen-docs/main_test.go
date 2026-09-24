package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/infrena/infrena-provider-gcp/internal/catalog"
	"github.com/infrena/infrena/pkg/value"
)

// repo paths, from this package's directory.
const (
	refDir       = "../../docs/reference"
	warningsFile = "../../gen/warnings.txt"
)

func load(t *testing.T) (*catalog.Catalog, []warning) {
	t.Helper()
	c, err := catalog.Load()
	if err != nil {
		t.Fatalf("load catalog: %v", err)
	}
	raw, err := os.ReadFile(warningsFile)
	if err != nil {
		t.Fatalf("%v", err)
	}
	ws, err := parseWarnings(string(raw))
	if err != nil {
		t.Fatalf("parse %s: %v", warningsFile, err)
	}
	return c, ws
}

// TestTheCommittedReferenceMatchesTheCatalog. The pages under docs/reference are
// generated and committed, so there are two copies of the same facts and they
// can disagree: change the catalog, forget to regenerate, and the published
// reference quietly describes a plugin that no longer exists. Nobody notices,
// because docs have no runtime.
//
// This is the thing that notices.
func TestTheCommittedReferenceMatchesTheCatalog(t *testing.T) {
	c, ws := load(t)
	diff, err := compare(refDir, Pages(c, ws))
	if err != nil {
		t.Fatalf("compare: %v", err)
	}
	if len(diff) > 0 {
		show := diff
		if len(show) > 20 {
			show = show[:20]
		}
		t.Fatalf("docs/reference is %d page(s) out of step with the catalog; run `go run ./cmd/gen-docs` and commit:\n  %s",
			len(diff), strings.Join(show, "\n  "))
	}
}

// TestRenderingIsDeterministic. Every page is built by ranging over maps -- the
// catalog's types, a type's attributes, an object's fields -- and Go randomises
// map iteration order on purpose. An unsorted range would still produce correct
// pages; it would just produce DIFFERENT correct pages each run, and every
// regeneration would arrive as a diff nobody can review.
//
// Two renders in one process is enough to catch it: the randomisation is
// per-range, not per-process.
func TestRenderingIsDeterministic(t *testing.T) {
	c, ws := load(t)
	first, second := Pages(c, ws), Pages(c, ws)
	if len(first) != len(second) {
		t.Fatalf("two renders produced %d and %d pages", len(first), len(second))
	}
	for _, name := range sortedPages(first) {
		if first[name] != second[name] {
			t.Errorf("%s differs between two renders of the same catalog", name)
		}
	}
}

// TestEveryTypeAndServiceHasAPage. A missing page is not a broken link, it is a
// user concluding the plugin does not serve a type it serves.
func TestEveryTypeAndServiceHasAPage(t *testing.T) {
	c, ws := load(t)
	pages := Pages(c, ws)
	services := map[string]bool{}
	for _, ty := range c.Types {
		services[ty.Service] = true
		if _, ok := pages[ty.Service+"/"+ty.Name+".md"]; !ok {
			t.Errorf("no page for %s", ty.Name)
		}
	}
	for s := range services {
		if _, ok := pages[s+"/README.md"]; !ok {
			t.Errorf("no guide for service %s", s)
		}
	}
	if _, ok := pages["README.md"]; !ok {
		t.Error("no index")
	}
	if _, ok := pages["not-shipped.md"]; !ok {
		t.Error("no not-shipped page")
	}
	if want := len(c.Types) + len(services) + 2; len(pages) != want {
		t.Errorf("rendered %d pages, want %d", len(pages), want)
	}
}

// TestTheWalkFollowsBothNestingEdges. Attr nests two ways: Fields for an
// object's attributes and Elem for a list's element. They are not alternatives,
// and a walk that follows only Fields reaches 8017 of this catalog's 19730
// attribute paths -- it silently drops every field inside every repeated block,
// which is where most of GCP's configuration lives.
//
// The fixture nests a list inside a list inside an object so that a walk which
// stops at the first Elem fails too, not only one that never enters an Elem.
func TestTheWalkFollowsBothNestingEdges(t *testing.T) {
	ty := &catalog.Type{
		Name: "gcp.widget", Service: "tiny",
		Attributes: map[string]*catalog.Attr{
			"rules": {Canonical: "rules", Kind: value.KindList, Elem: &catalog.Attr{
				Canonical: "rules", Kind: value.KindMap,
				Fields: map[string]*catalog.Attr{
					"matches": {Canonical: "matches", Kind: value.KindList, Elem: &catalog.Attr{
						Canonical: "matches", Kind: value.KindMap,
						Fields: map[string]*catalog.Attr{
							"prefix": {Canonical: "prefix", Kind: value.KindString, ForceNew: true},
						},
					}},
				},
			}},
		},
	}
	got := map[string]bool{}
	for _, r := range attrRows(ty) {
		got[r.Path] = true
	}
	for _, want := range []string{"rules", "rules[]", "rules[].matches", "rules[].matches[]", "rules[].matches[].prefix"} {
		if !got[want] {
			t.Errorf("attrRows did not reach %q; it found %v", want, sortedPaths(attrRows(ty)))
		}
	}
}

// TestEveryAttributeSpellingIsDocumented. The generator emits a snake_case alias
// for every camelCase attribute, and the schema name is not always the wire
// name (307 attributes in this catalog spell them differently). A user writing
// the alias is writing a supported spelling; if the page does not show it, the
// alias may as well not exist.
func TestEveryAttributeSpellingIsDocumented(t *testing.T) {
	c, _ := load(t)
	ty, ok := c.Type("gcp.storage.bucket")
	if !ok {
		t.Skip("gcp.storage.bucket is not in the catalog")
	}
	page := renderType(ty)
	checked := 0
	for _, r := range attrRows(ty) {
		for _, alias := range r.Attr.Aliases {
			if !strings.Contains(page, "`"+alias+"`") {
				t.Errorf("%s: alias %q appears nowhere on the page", r.Path, alias)
			}
			checked++
		}
	}
	if checked == 0 {
		t.Fatal("no aliases checked; the fixture stopped exercising what this test is for")
	}
}

// TestTheDedupedSubtreeStillNamesWhereItsFieldsAre. Several GCP schemas recurse,
// and expanding every occurrence in full costs thousands of identical rows:
// gcp.routine takes 3708 of them where 190 say the same thing. The repeat is
// collapsed, but the attribute is still listed, and its row must say where its
// fields are written down -- otherwise the collapse is just a hole.
func TestTheDedupedSubtreeStillNamesWhereItsFieldsAre(t *testing.T) {
	c, _ := load(t)
	ty, ok := c.Type("gcp.routine")
	if !ok {
		t.Skip("gcp.routine is not in the catalog")
	}
	rows := attrRows(ty)
	byPath := map[string]attrRow{}
	deduped := 0
	for _, r := range rows {
		byPath[r.Path] = r
	}
	for _, r := range rows {
		if r.SameAs == "" {
			continue
		}
		deduped++
		if _, ok := byPath[r.SameAs]; !ok {
			t.Errorf("%s points at %q, which is not on the page", r.Path, r.SameAs)
		}
		if !strings.Contains(r.notes(), "same fields as") {
			t.Errorf("%s was collapsed but its notes do not say so: %q", r.Path, r.notes())
		}
	}
	if deduped == 0 {
		t.Fatal("nothing was collapsed on a type chosen because it recurses")
	}
}

// TestNotShippedAccountsForEveryWarning. gen/warnings.txt is the generator's own
// record of what it refused. This page is the only public form of it, and a row
// that falls out of the grouping is a type whose absence has no explanation
// anywhere a user can read.
func TestNotShippedAccountsForEveryWarning(t *testing.T) {
	_, ws := load(t)
	if len(ws) == 0 {
		t.Fatal("gen/warnings.txt parsed to nothing")
	}
	page := renderNotShipped(ws)
	for _, w := range ws {
		if !strings.Contains(page, "`"+w.Subject+"`") {
			t.Errorf("not-shipped.md never mentions %q (%s)", w.Subject, w.Reason)
		}
	}
	// And the tier gate itself, because "write a ruling naming every hook" is
	// the whole answer to "why can I not have this type".
	for _, phrase := range []string{"gen/overlay.yaml", "unruled wire hooks", "every"} {
		if !strings.Contains(page, phrase) {
			t.Errorf("not-shipped.md does not explain the tier gate: missing %q", phrase)
		}
	}
}

// TestParseWarningsRefusesAnUnknownShape. Skipping a row it does not recognise
// would drop a type from the page silently, which is the one failure this page
// exists to prevent.
func TestParseWarningsRefusesAnUnknownShape(t *testing.T) {
	if _, err := parseWarnings("tier2\tonly-two-fields\n"); err == nil {
		t.Error("a two-field row parsed cleanly")
	}
	ws, err := parseWarnings("# comment\n\ntier3\tsvc/Thing\tno get method; needs a ruling\nnocreate\tgcp.t\t{+parent}/x\tneeds parent\n")
	if err != nil {
		t.Fatalf("a well-formed file failed to parse: %v", err)
	}
	if len(ws) != 2 {
		t.Fatalf("parsed %d rows, want 2", len(ws))
	}
	if got := category(ws[0].Reason); got != "no get method" {
		t.Errorf("category = %q, want %q", got, "no get method")
	}
	if got := ws[1].Template; got != "{+parent}/x" {
		t.Errorf("template = %q", got)
	}
}

// TestCapabilitiesMatchWhatTheHostIsTold. The page and `infrena explain` answer
// the same question, and a page that says a type can be created when the host
// refuses to plan a create is worse than no page.
func TestCapabilitiesMatchWhatTheHostIsTold(t *testing.T) {
	c, _ := load(t)
	defs := c.Definitions()
	byType := map[string]bool{}
	for _, d := range defs {
		byType[d.Type] = d.Capabilities.Create
	}
	uncreatable := 0
	for _, ty := range c.Types {
		if canCreate(ty) != byType[ty.Name] {
			t.Errorf("%s: page says create=%v, the host is told %v", ty.Name, canCreate(ty), byType[ty.Name])
		}
		if !canCreate(ty) {
			uncreatable++
		}
	}
	if uncreatable == 0 {
		t.Fatal("no uncreatable type in the catalog; this test no longer proves anything")
	}
}

// TestEveryLinkOnAPageResolves. The reference is a tree of relative links, and a
// broken one is indistinguishable from a missing page to the person following it.
func TestEveryLinkOnAPageResolves(t *testing.T) {
	c, ws := load(t)
	pages := Pages(c, ws)
	for _, from := range sortedPages(pages) {
		for _, target := range mdLinks(pages[from]) {
			if i := strings.Index(target, "#"); i >= 0 {
				target = target[:i]
			}
			// Only the links this generator writes, which all name a page.
			// Anything else came out of an upstream description.
			if !strings.HasSuffix(target, ".md") {
				continue
			}
			resolved := filepath.ToSlash(filepath.Join(filepath.Dir(from), target))
			if _, ok := pages[resolved]; !ok {
				t.Errorf("%s links to %s, which no page provides", from, target)
			}
		}
	}
}

// mdLinks pulls the targets out of [text](target). Enough for generated pages,
// whose links this file also generates.
func mdLinks(page string) []string {
	var out []string
	for i := 0; i < len(page); i++ {
		if page[i] != ']' || i+1 >= len(page) || page[i+1] != '(' {
			continue
		}
		// An escaped "\]" is not the end of a link label; flatten escapes
		// every bracket that came out of an upstream description.
		if i > 0 && page[i-1] == '\\' {
			continue
		}
		end := strings.IndexByte(page[i+2:], ')')
		if end < 0 {
			continue
		}
		out = append(out, page[i+2:i+2+end])
		i += 2 + end
	}
	return out
}

func sortedPaths(rows []attrRow) []string {
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.Path)
	}
	return out
}
