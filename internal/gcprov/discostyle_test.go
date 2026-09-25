package gcprov

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/infrena/infrena-provider-gcp/internal/catalog"
	"github.com/infrena/infrena-provider-gcp/internal/gcpfake"
)

// discoMethod is one Discovery method as the fake needs it: the verb, the
// address it answers at, and how it answers.
type discoMethod struct {
	id      string
	verb    string
	pattern *regexp.Regexp
	style   gcpfake.OperationStyle
	// literals is how much of the address is fixed text, so the most
	// specific of several matching methods wins.
	literals int
	// requiredQuery are the query parameters the method requires, by
	// structure or by its "Required." prose (trustConfigs.patch's updateMask
	// is required only in prose).
	requiredQuery []string
}

var (
	discoOnce    sync.Once
	discoMethods map[string][]discoMethod // by Discovery document name
	discoErr     error
)

// discoPlaceholderRE is one Discovery path placeholder: {name} for one segment,
// {+name} for a run of them.
var discoPlaceholderRE = regexp.MustCompile(`\{\+?[^}]+\}`)

// loadDiscoMethods reads gen/methods.tsv once: every Discovery method, with
// its answer style taken from its OWN response schema, which is the point:
// the catalog records one await per type (and two exceptions), and a fake
// taking its style from the catalog agreed with the catalog when it was
// wrong. The committed digest, not schemas/, which is gitignored: a test
// reading schemas/ passed on one laptop and failed in CI (2026-09-25).
func loadDiscoMethods() (map[string][]discoMethod, error) {
	discoOnce.Do(func() {
		data, err := os.ReadFile(filepath.Join("..", "..", "gen", "methods.tsv"))
		if err != nil {
			discoErr = err
			return
		}
		styles := map[string]gcpfake.OperationStyle{
			"none": gcpfake.OpSync, "longrunning": gcpfake.OpLongRunning, "compute_operation": gcpfake.OpCompute,
		}
		discoMethods = map[string][]discoMethod{}
		for _, line := range strings.Split(string(data), "\n") {
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			f := strings.Split(line, "\t")
			if len(f) != 6 {
				discoErr = fmt.Errorf("gen/methods.tsv: %q has %d fields, want 6", line, len(f))
				return
			}
			style, ok := styles[f[3]]
			if !ok {
				discoErr = fmt.Errorf("gen/methods.tsv: unknown answer style %q", f[3])
				return
			}
			var required []string
			if f[4] != "" {
				required = strings.Split(f[4], ",")
			}
			discoMethods[f[0]] = append(discoMethods[f[0]], discoMethod{
				id:            f[5],
				verb:          f[1],
				pattern:       discoPathPattern(f[2]),
				style:         style,
				literals:      len(discoPlaceholderRE.ReplaceAllString(f[2], "")),
				requiredQuery: required,
			})
		}
	})
	if discoErr == nil && len(discoMethods) < 20 {
		return nil, fmt.Errorf("gen/methods.tsv covers only %d services", len(discoMethods))
	}
	return discoMethods, discoErr
}

// discoPathPattern matches a request path ending in the method's address,
// whatever version prefix the provider put in front of it.
func discoPathPattern(path string) *regexp.Regexp {
	var b strings.Builder
	b.WriteString(`(^|/)`)
	last := 0
	for _, loc := range discoPlaceholderRE.FindAllStringIndex(path, -1) {
		b.WriteString(regexp.QuoteMeta(path[last:loc[0]]))
		if strings.HasPrefix(path[loc[0]:], "{+") {
			b.WriteString(`.+`)
		} else {
			b.WriteString(`[^/]+`)
		}
		last = loc[1]
	}
	b.WriteString(regexp.QuoteMeta(path[last:]))
	b.WriteString(`$`)
	return regexp.MustCompile(b.String())
}

// discoStyleCounts records, per test run, how often the Discovery answer
// was found and how often the fake fell back to the catalog's.
type discoStyleCounts struct {
	mu                 sync.Mutex
	matched, unmatched int
	unmatchedByType    map[string][]string
}

// discoStyleResolver answers a request the way the Discovery method at its
// address answers. Where several methods of the same verb match and they
// disagree, the most specific address wins; where nothing matches, the
// fake keeps the catalog's style and the miss is counted.
func discoStyleResolver(t *testing.T, ty *catalog.Type, counts *discoStyleCounts) func(method, path string) (gcpfake.OperationStyle, bool) {
	t.Helper()
	all, err := loadDiscoMethods()
	if err != nil {
		t.Fatalf("loading the Discovery documents: %v", err)
	}
	methods := all[ty.Service]
	return func(verb, path string) (gcpfake.OperationStyle, bool) {
		var best *discoMethod
		for i := range methods {
			m := &methods[i]
			if m.verb != verb || !m.pattern.MatchString(path) {
				continue
			}
			if best == nil || m.literals > best.literals {
				best = m
			}
		}
		counts.mu.Lock()
		defer counts.mu.Unlock()
		if best == nil {
			counts.unmatched++
			counts.unmatchedByType[ty.Name] = append(counts.unmatchedByType[ty.Name], verb+" "+path)
			return 0, false
		}
		counts.matched++
		return best.style, true
	}
}

// missingRequiredQuery names, for each mutation the fake received, a query
// parameter its Discovery method requires and the request left out. A
// PATCH with no updateMask where the API requires one is a 400 from Google
// and was a success here.
func missingRequiredQuery(t *testing.T, ty *catalog.Type, reqs []gcpfake.Request) []string {
	t.Helper()
	all, err := loadDiscoMethods()
	if err != nil {
		t.Fatalf("loading the Discovery documents: %v", err)
	}
	var out []string
	for _, r := range reqs {
		if r.Method == "GET" {
			continue
		}
		var best *discoMethod
		for i := range all[ty.Service] {
			m := &all[ty.Service][i]
			if m.verb == r.Method && m.pattern.MatchString(r.SentPath) && (best == nil || m.literals > best.literals) {
				best = m
			}
		}
		if best == nil {
			continue
		}
		for _, q := range best.requiredQuery {
			// Google takes a query parameter in either spelling: the live
			// Artifact Registry create sent repository_id for repositoryId.
			if r.Query.Get(q) == "" && r.Query.Get(snakeCase(q)) == "" {
				out = append(out, r.Method+" "+r.Path+" without "+q+" ("+best.id+")")
			}
		}
	}
	return out
}

// TestTheDiscoveryResolverTellsSharedPathsApart. Every proto-first delete is
// "v1/{+name}"; only flatPath says whose it is. Artifact Registry's
// repository patch answers with the repository, and its create with an
// operation: the resolver must say so from the documents alone.
func TestTheDiscoveryResolverTellsSharedPathsApart(t *testing.T) {
	counts := &discoStyleCounts{unmatchedByType: map[string][]string{}}
	ty := &catalog.Type{Name: "probe", Service: "artifactregistry"}
	resolve := discoStyleResolver(t, ty, counts)
	repo := "/v1/projects/p/locations/l/repositories/r"
	for _, c := range []struct {
		verb, path string
		want       gcpfake.OperationStyle
	}{
		{"POST", "/v1/projects/p/locations/l/repositories", gcpfake.OpLongRunning},
		{"PATCH", repo, gcpfake.OpSync},
		{"DELETE", repo, gcpfake.OpLongRunning},
	} {
		got, ok := resolve(c.verb, c.path)
		if !ok || got != c.want {
			t.Errorf("%s %s answers as %v (found %v), want %v", c.verb, c.path, got, ok, c.want)
		}
	}
}

func snakeCase(s string) string {
	var b strings.Builder
	for i, r := range s {
		if r >= 'A' && r <= 'Z' {
			if i > 0 {
				b.WriteByte('_')
			}
			r += 'a' - 'A'
		}
		b.WriteRune(r)
	}
	return b.String()
}

// discoPutIsUpdate says whether a PUT to path is, per Discovery, an update
// (which replaces the resource) rather than a create: Pub/Sub creates with a
// PUT to the new resource's own path.
func discoPutIsUpdate(t *testing.T, ty *catalog.Type) func(path string) bool {
	t.Helper()
	all, err := loadDiscoMethods()
	if err != nil {
		t.Fatalf("loading the Discovery methods: %v", err)
	}
	methods := all[ty.Service]
	return func(path string) bool {
		var best *discoMethod
		for i := range methods {
			m := &methods[i]
			if m.verb == "PUT" && m.pattern.MatchString(path) && (best == nil || m.literals > best.literals) {
				best = m
			}
		}
		return best != nil && !strings.HasSuffix(best.id, ".create") && !strings.HasSuffix(best.id, ".insert")
	}
}
