// collisions.go resolves what's left after Scope (names.go, build.go's
// scopeSegment): two collections in the SAME service that still singularize
// to the same bare resource name. The real corpus turned up two genuinely
// different shapes once the org/folder/billing-account case was handled:
//
//   - a legacy alias: the SAME resource reached through an older URL.
//     container's projects.zones.clusters is a deprecated stand-in for
//     projects.locations.clusters; logging's bare sinks/exclusions and
//     locations.buckets are pre-project-scoping endpoints alongside
//     projects.sinks/projects.exclusions/projects.locations.buckets. Shipping
//     both as distinct types would claim two resources where GCP has one;
//     shipping both under one name is the silent-merge bug this whole area
//     of the generator exists to prevent. The answer is neither: keep the
//     richer path, and warn about the other by name so gen/warnings.txt still
//     answers "why doesn't this show up".
//   - genuinely different resources that happen to share a bare leaf. iam's
//     projects.serviceAccounts.keys and
//     projects.locations.workloadIdentityPools.providers.keys are unrelated
//     concepts that both end in "keys". Both ship, under names extended by
//     walking their path leftward one segment at a time until they no longer
//     collide.
package gen

import (
	"strings"

	"github.com/infrena/infrena-provider-gcp/internal/disco"
)

// aliasLoser is a collection dropped as a legacy alias, recorded so its
// warning can be written once its winner has an assigned name (Build only
// knows that after Assign runs, which happens after this resolution pass).
type aliasLoser struct {
	doc     *disco.Document
	rawName string
	// winner points directly at the winner's entry in shipping's backing
	// array, rather than copying its Candidate and path out by value here.
	// It must: this group's winner can ALSO be a survivor that
	// disambiguateByPath (below, still to run when this loser is recorded)
	// mutates, and a value copied before that mutation would go stale --
	// build.go would then look up a Candidate nothing was ever named,
	// finding nothing and silently writing a blank name into the warning. A
	// pointer instead always reads whatever the winner's FINAL Candidate and
	// path turned out to be, however this group resolved.
	winner *pending
}

// resolveWithinServiceCollisions groups shipping's unscoped candidates by
// service and bare resource segment, resolves every group of more than one
// (see the package doc above), and returns the surviving pending entries
// (mutated in place where disambiguation extended a Resource) plus every
// collection dropped as a legacy alias.
//
// Scoped candidates (Scope != "", from build.go's scopeSegment) are never
// touched: they already carry a distinct identity and never collide with
// each other or with the unscoped candidate for the same leaf.
func resolveWithinServiceCollisions(shipping []pending) ([]pending, []aliasLoser) {
	groups := map[string][]int{}
	var order []string
	for i, p := range shipping {
		if p.cand.Scope != "" {
			continue
		}
		key := p.doc.Name + "/" + p.cand.Resource
		if _, ok := groups[key]; !ok {
			order = append(order, key)
		}
		groups[key] = append(groups[key], i)
	}

	drop := map[int]bool{}
	var losers []aliasLoser
	for _, key := range order {
		idxs := groups[key]
		if len(idxs) < 2 {
			continue
		}

		// Cluster by canonical path: members of the same cluster are the same
		// resource reached through a different root or axis. clusterOrder
		// keeps first-seen order so processing (and so which alias warning
		// text is produced) is deterministic across runs.
		clusters := map[string][]int{}
		var clusterOrder []string
		for _, i := range idxs {
			c := strings.Join(canonicalPath(shipping[i].col.Path), ".")
			if _, ok := clusters[c]; !ok {
				clusterOrder = append(clusterOrder, c)
			}
			clusters[c] = append(clusters[c], i)
		}

		var survivors []int
		for _, c := range clusterOrder {
			members := clusters[c]
			winner := members[0]
			for _, i := range members[1:] {
				var mmBaseURL string
				if shipping[winner].mm != nil {
					mmBaseURL = shipping[winner].mm.BaseURL
				}
				if preferred(shipping[i].col.Path, shipping[winner].col.Path, mmBaseURL) {
					winner = i
				}
			}
			for _, i := range members {
				if i == winner {
					continue
				}
				drop[i] = true
				losers = append(losers, aliasLoser{
					doc:     shipping[i].doc,
					rawName: shipping[i].rawName,
					winner:  &shipping[winner],
				})
			}
			survivors = append(survivors, winner)
		}

		// Everything left in the group is a genuinely different resource that
		// still shares its bare leaf: disambiguate by walking each one's path.
		if len(survivors) > 1 {
			disambiguateByPath(shipping, survivors)
		}
	}

	if len(drop) == 0 {
		return shipping, nil
	}
	out := make([]pending, 0, len(shipping)-len(drop))
	for i, p := range shipping {
		if !drop[i] {
			out = append(out, p)
		}
	}
	return out, losers
}

// canonicalPath reduces a Discovery collection's path to the form used to
// decide whether it is the SAME resource as another candidate reached
// through a different root or axis: the "projects" root is dropped (a bare
// path and a projects-rooted path for the same resource are the same
// resource), and a zones/regions axis segment right where the root was is
// folded into "locations" (the modern unified axis). Two collections whose
// canonical paths match differ only in root-presence or axis spelling -- the
// legacy-alias shape. Any other difference -- a different resource name
// anywhere in the path -- means they are different resources, not an alias
// pair, however similar the leaf.
func canonicalPath(path []string) []string {
	out := append([]string(nil), path...)
	if len(out) > 0 && out[0] == "projects" {
		out = out[1:]
	}
	if len(out) > 0 && (out[0] == "zones" || out[0] == "regions") {
		out[0] = "locations"
	}
	return out
}

// axisWord is the location-axis segment right after a path's effective root
// -- path[1] when the path is rooted at "projects", or path[0] when it is
// bare -- "locations", "zones" or "regions" in the corpus today. Empty when
// the path has no such segment (e.g. it's just "projects"+leaf, or a bare
// single-segment path).
func axisWord(path []string) string {
	if len(path) > 2 && path[0] == "projects" {
		return path[1]
	}
	if len(path) > 1 && path[0] != "projects" {
		return path[0]
	}
	return ""
}

// preferred reports whether path a should be kept over path b when both are
// the same resource (canonicalPath(a) == canonicalPath(b)), reached through a
// legacy alias. A projects.-rooted path beats a bare one (GCP's bare/implicit
// endpoints predate explicit project scoping and are the ones being phased
// out); a locations.-rooted axis beats zones./regions. (the unified axis
// GCP has been migrating resources like GKE clusters to). mmBaseURL --
// magic-modules' own base_url for the resource, when it has one -- breaks a
// tie neither rule resolves: mm's own opinion of where this resource lives is
// better evidence than either ordering rule above it.
func preferred(a, b []string, mmBaseURL string) bool {
	aRoot := len(a) > 0 && a[0] == "projects"
	bRoot := len(b) > 0 && b[0] == "projects"
	if aRoot != bRoot {
		return aRoot
	}
	aAxis, bAxis := axisWord(a), axisWord(b)
	if aAxis != bAxis {
		if aAxis == "locations" {
			return true
		}
		if bAxis == "locations" {
			return false
		}
	}
	if mmBaseURL != "" {
		aIn := aAxis != "" && strings.Contains(mmBaseURL, "/"+aAxis+"/")
		bIn := bAxis != "" && strings.Contains(mmBaseURL, "/"+bAxis+"/")
		if aIn != bIn {
			return aIn
		}
	}
	// Genuinely undecidable by any rule above: keep whichever path sorts
	// first, so the choice is at least deterministic across runs rather than
	// depending on slice order that happened to fall out of map iteration
	// upstream.
	return strings.Join(a, ".") <= strings.Join(b, ".")
}

// pathWalker yields a collection's path segments leftward from just before
// its leaf, skipping resource-hierarchy roots and location-axis words: none
// of them say what makes two resources different, only where they live.
type pathWalker struct {
	path   []string
	cursor int
}

func newPathWalker(path []string) *pathWalker {
	return &pathWalker{path: path, cursor: len(path) - 2}
}

// next returns the next disambiguating segment, singularized and lowercased
// like any other Candidate.Resource. ok is false once the path is exhausted.
func (w *pathWalker) next() (seg string, ok bool) {
	for w.cursor >= 0 {
		s := w.path[w.cursor]
		w.cursor--
		switch s {
		case "projects", "organizations", "folders", "billingAccounts", "locations", "zones", "regions":
			continue
		}
		return strings.ToLower(singular(s)), true
	}
	return "", false
}

// disambiguateByPath extends each of survivors' Candidate.Resource leftward
// one path segment at a time -- round by round, only the entries STILL
// colliding after the previous round advance -- until every survivor's
// Resource is unique, or every survivor's path is exhausted (in which case
// whatever collides is left for Build's uniqueness invariant to catch and
// report loudly, rather than resolved by a guess here).
//
// Two candidates that diverge one segment before the leaf (iam's
// ServiceAccountKey against the rest) get a one-segment extension; two that
// only diverge further up (WorkloadIdentityPoolProviderKey and
// WorkforcePoolProviderKey, both under a "providers" segment) get extended
// until that segment too -- each survivor walks exactly as far as it needs
// to and no further.
func disambiguateByPath(shipping []pending, survivors []int) {
	walkers := make(map[int]*pathWalker, len(survivors))
	for _, i := range survivors {
		walkers[i] = newPathWalker(shipping[i].col.Path)
	}
	for {
		groups := map[string][]int{}
		for _, i := range survivors {
			groups[shipping[i].cand.Resource] = append(groups[shipping[i].cand.Resource], i)
		}
		var colliding []int
		for _, is := range groups {
			if len(is) > 1 {
				colliding = append(colliding, is...)
			}
		}
		if len(colliding) == 0 {
			return
		}
		var progressed bool
		for _, i := range colliding {
			seg, ok := walkers[i].next()
			if !ok {
				continue
			}
			shipping[i].cand.Resource = seg + "." + shipping[i].cand.Resource
			shipping[i].cand.Qualified = true
			progressed = true
		}
		if !progressed {
			return
		}
	}
}
