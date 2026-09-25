// build.go assembles Tasks 2-7 into the generator: read the inputs, decide
// tiers, name everything, build each type, and hand back the catalog plus
// everything left out of it.
package gen

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/infrena/infrena-provider-gcp/internal/catalog"
	"github.com/infrena/infrena-provider-gcp/internal/disco"
	"github.com/infrena/infrena-provider-gcp/internal/mmv1"
	"github.com/infrena/infrena/pkg/value"
)

// Inputs are the four things the generator reads.
type Inputs struct {
	SchemaDir   string
	MMV1Dir     string
	OverlayPath string
	LockPath    string
}

// Warning is one type that did not ship, and why.
type Warning struct {
	Service  string
	Resource string
	Tier     Tier
	Reason   string
}

// Uncreatable is one type that ships but cannot be created: every
// placeholder in its create url that nothing can fill.
//
// It is NOT a Warning. A Warning names a type this catalog does not serve at
// all; these ARE served -- they read, import, discover and delete, and
// earlier tasks built machinery specifically for some of them (ParentRoot
// exists to tell a folder's log bucket from a project's, and discovery has
// tests for exactly those types). What they cannot do is CREATE, so that is
// the capability that does not ship, not the type.
type Uncreatable struct {
	Type         string
	Template     string
	Placeholders []string
}

// Result is the catalog plus everything left out of it.
type Result struct {
	Catalog  *catalog.Catalog
	Warnings []Warning
	// Uncreatable is every shipped type whose create url cannot be built,
	// recorded so gen/warnings.txt can name each one with the placeholder
	// that stops it -- the point of task 18b's invariant.
	Uncreatable []Uncreatable
	// Unpatchable is every shipped type left non-updatable because its API
	// documents a restricted patch and no overlay entry says which fields it
	// really takes. Like Uncreatable these types DO ship; only update is
	// refused, and the refusal quotes the API's own sentence.
	Unpatchable []Unpatchable
}

// Unpatchable is one type that ships and is replaced rather than patched,
// because Google's patch description says it accepts only some fields and
// nobody has written a gen/overlay.yaml `patchable:` entry for it.
type Unpatchable struct {
	Type string
	// Says is Google's own sentence, so the warnings file argues from the
	// source rather than from our reading of it.
	Says string
}

// pending is one type that passed tiering and is on its way to being built,
// carried between Build's passes. Package level (not local to Build) so the
// within-service collision resolution below can operate on it directly.
type pending struct {
	doc  *disco.Document
	col  disco.Collection
	mm   *mmv1.Resource
	cand Candidate
	dec  Decision
	// rawName is the mm resource name, or (when there is no mm resource) the
	// Discovery collection leaf — the spelling a human reading warnings.txt
	// would recognize, as opposed to cand.Resource, which is lowercased and
	// singularized purely for name assignment. Computed once here so a build
	// failure in pass three names the SAME thing a tier refusal in this same
	// loop would have (see F3: they used to disagree — "Hooked" from a tier
	// refusal, "widget" from a build failure — over a file whose whole job is
	// answering "why is this type unsupported").
	rawName string
}

// Build runs the whole generation pass: tier first, over the whole corpus;
// then name every surviving type at once, so uniqueness is judged over the
// whole corpus rather than per API; only then build types, because a
// reference edge cannot be written until its target has a name.
func Build(in Inputs) (*Result, error) {
	byProduct, loadErrs, err := mmv1.LoadDir(in.MMV1Dir)
	if err != nil {
		return nil, err
	}
	overlay, err := LoadOverlay(in.OverlayPath, in.MMV1Dir)
	if err != nil {
		return nil, err
	}
	lock, err := LoadLock(in.LockPath)
	if err != nil {
		return nil, err
	}

	docs, err := loadDocs(in.SchemaDir)
	if err != nil {
		return nil, err
	}

	// Pass one: decide what ships, and collect naming candidates.
	var shipping []pending
	var warnings []Warning

	// A magic-modules file that failed to read or parse is refused just like
	// any other type that didn't ship: named here, not left as something
	// LoadDir collected and nothing downstream ever looked at.
	for _, le := range loadErrs {
		warnings = append(warnings, Warning{
			Service:  filepath.Base(filepath.Dir(le.Path)),
			Resource: filepath.Base(le.Path),
			Tier:     TierExcluded,
			Reason:   le.Err.Error(),
		})
	}

	for _, d := range docs {
		mms := byProduct[d.Name]
		// mmv1 product directory names routinely diverge from the Discovery
		// API name (TagBinding lives under products/tags, not
		// products/cloudresourcemanager) — see Overlay.ProductAliases. Every
		// aliased directory's resources are searched too, so matchResource can
		// find them by name exactly as if they lived under d.Name itself.
		for _, alias := range overlay.ProductAliases[d.Name] {
			mms = append(mms, byProduct[alias]...)
		}
		for _, col := range d.Collections() {
			leaf := col.Path[len(col.Path)-1]
			mm := preferExactCollection(mms, col.Path, matchResource(mms, leaf, createPathOf(col)))
			rawName := leaf
			if mm != nil {
				rawName = mm.Name
			}
			var ruling *Ruling
			if mm != nil {
				ruling = overlay.Rulings[d.Name+"/"+mm.Name]
			}
			dec := Classify(col, mm, ruling)
			if dec.Tier != TierGeneric {
				warnings = append(warnings, Warning{d.Name, rawName, dec.Tier, dec.Reason})
				continue
			}
			cand := Candidate{Service: d.Name, Resource: strings.ToLower(singular(leaf)), Scope: scopeSegment(col.Path)}
			shipping = append(shipping, pending{d, col, mm, cand, dec, rawName})
		}
	}

	// Pass one and a half: within each service, resolve candidates that still
	// share a resource segment. Two shapes, resolved differently:
	//
	//   - a legacy alias: the SAME resource reached through an older URL
	//     (container's projects.zones.clusters alongside the modern
	//     projects.locations.clusters; logging's bare sinks alongside
	//     projects.sinks). Keep the richer path, drop the other -- warned,
	//     never silently.
	//   - genuinely different resources that merely share a bare leaf after
	//     singularizing it (iam's projects.serviceAccounts.keys and
	//     projects.locations.workloadIdentityPools.providers.keys are
	//     unrelated). Both ship, disambiguated by walking their path leftward
	//     one segment at a time until their names no longer collide.
	//
	// Scoped candidates (organization/folder/billingaccount, from Scope above)
	// never enter this: they already have distinct identities.
	shipping, aliasLosers := resolveWithinServiceCollisions(shipping)

	// Pass two: name everything at once, so uniqueness is decided over the whole
	// corpus rather than per API.
	cands := make([]Candidate, 0, len(shipping))
	for _, p := range shipping {
		cands = append(cands, p.cand)
	}
	names, err := Assign(cands, lock)
	if err != nil {
		return nil, err
	}
	if err := lock.Save(in.LockPath); err != nil {
		return nil, err
	}

	// The alias losers' warning can only be written now that the winner it
	// names actually has a name. al.winner is read here, not captured by
	// value back in resolveWithinServiceCollisions, specifically so this
	// sees the winner's Candidate and path as they ended up AFTER
	// disambiguateByPath ran -- see aliasLoser's doc comment.
	for _, al := range aliasLosers {
		warnings = append(warnings, Warning{
			Service:  al.doc.Name,
			Resource: al.rawName,
			Tier:     TierGeneric,
			Reason: fmt.Sprintf("legacy alias of %s (%s)", names[al.winner.cand],
				strings.Join(al.winner.col.Path, ".")),
		})
	}

	// Pass three: build each type, now that every candidate has a name.
	//
	// References are deliberately NOT resolved here — see pass four below.
	// Classify only checks that a create METHOD exists (a candidate reaching
	// `shipping` says nothing about whether its request body schema exists or
	// its $refs resolve), so a candidate can pass tiering and still fail
	// inside buildType. This pass collects exactly what survives that: c.Types
	// and the build-failure warnings, and — in `succeeded` — enough of each
	// pending to resolve references against in pass four.
	c := &catalog.Catalog{
		Generated:  time.Now().UTC().Format("2006-01-02"),
		MMV1Commit: readPin(filepath.Dir(in.MMV1Dir)),
		// The overlay is a generator-time input, so the runtime's discovery
		// fallback has no way to read discover_default except through the
		// catalog it already loads. Copied verbatim; whether every name in it
		// is a type this catalog actually serves is checked by
		// TestDiscoverDefaultNamesOnlyTypesWeServe (internal/catalog), which
		// reads the overlay and the built catalog together.
		DiscoverDefault: overlay.DiscoverDefault,
	}
	type built struct {
		p pending
		t *catalog.Type
	}
	var succeeded []built
	for _, p := range shipping {
		t, err := buildType(p.doc, p.col, p.mm, names[p.cand], overlay)
		if err != nil {
			warnings = append(warnings, Warning{p.doc.Name, p.rawName, TierExcluded, err.Error()})
			continue
		}
		t.Tier = int(p.dec.Tier)
		t.TierReason = p.dec.Reason
		succeeded = append(succeeded, built{p, t})
		c.Types = append(c.Types, t)
	}

	// Generator-level invariant: the naming pass (Assign, above) is supposed to
	// make every type's name unique, but nothing had ever checked that it
	// actually did -- Candidate collapsing four genuinely different Discovery
	// collections (an org's, a folder's, a billing account's and a project's
	// own logging buckets, say) into one map key went undetected for exactly
	// that reason until it was caught by hand against the real corpus. Checked
	// HERE, not left for schema.ValidateAll downstream: that would report it as
	// "the catalog infrena refuses," which is true but sends whoever sees it
	// looking in the wrong package for why.
	if err := checkNamesAreUnique(c.Types); err != nil {
		return nil, err
	}

	// Pass four: resolve references, now that c.Types names exactly what
	// actually shipped.
	//
	// A raw ResourceRef target name is not enough on its own to find that
	// name: 942 magic-modules resources share only 802 distinct names
	// (Instance alone spans 16 products), so resolving by bare name would
	// silently point a compute Instance reference at gcp.alloydb.instance or
	// whichever product happened to build last. refByProduct disambiguates by
	// keying on "<product>/<name>", matching the key already used for tier
	// rulings and the name lock; refCandidates records every product that
	// shipped each bare name, so a same-product miss can still resolve a
	// genuine cross-product reference (e.g. compute/Subnetwork ->
	// networkconnectivity/InternalRange) when exactly one product has it, and
	// can tell an ambiguous one from a dangling one when more than one does.
	//
	// Both maps are built from `succeeded`, not from `shipping`: building them
	// from everything that merely passed tiering would let a reference resolve
	// to a name that was assigned but never actually reached c.Types (its own
	// build having failed in pass three) — the exact hazard this task exists
	// to prevent, just one step removed. Building from `succeeded` instead
	// makes the invariant structural: the map cannot name something that
	// isn't in it, because it was built FROM what's in it.
	refByProduct := map[string]string{}           // "<product>/<mm resource name>" -> infrena type
	refCandidates := map[string]map[string]bool{} // mm resource name -> products that shipped it
	for _, b := range succeeded {
		if b.p.mm != nil {
			refByProduct[b.p.mm.Product+"/"+b.p.mm.Name] = b.t.Name
			if refCandidates[b.p.mm.Name] == nil {
				refCandidates[b.p.mm.Name] = map[string]bool{}
			}
			refCandidates[b.p.mm.Name][b.p.mm.Product] = true
		}
	}
	for _, b := range succeeded {
		var selfProduct string
		if b.p.mm != nil {
			selfProduct = b.p.mm.Product
		}
		resolveRefs(b.t.Attributes, selfProduct, b.t.Service, b.t.Name, refByProduct, refCandidates, &warnings)
	}
	dropRefsToMissingAttributes(c.Types, &warnings)
	if err := applySensitive(c.Types, overlay.Sensitive); err != nil {
		return nil, err
	}
	// Every shipped type whose create url cannot be built, recorded once,
	// here, over the catalog as it finally stands. The capability itself is
	// derived from the same function at load (catalog.Definitions), so this
	// is the RECORD, not the enforcement: it is what a person reads in
	// gen/warnings.txt and what a regeneration's diff shows moving.
	var uncreatable []Uncreatable
	for _, t := range c.Types {
		if missing := t.UnresolvedCreatePlaceholders(); len(missing) > 0 {
			uncreatable = append(uncreatable, Uncreatable{t.Name, t.CreateTemplate(), missing})
		}
	}
	sort.Slice(uncreatable, func(i, j int) bool { return uncreatable[i].Type < uncreatable[j].Type })

	// The same record, for the same reason, on the update side: a type that
	// ships and is REPLACED rather than patched because its API documents a
	// restricted patch and nobody has written down which fields it takes. Read
	// from succeeded rather than c.Types because the decision needs the
	// collection, which only the pending entry still holds.
	var unpatchable []Unpatchable
	for _, b := range succeeded {
		if b.t.UpdateVerb != "" || overlay.Patchable[b.t.Name] != nil {
			continue
		}
		if restricted, says := restrictedPatch(b.p.col); restricted {
			unpatchable = append(unpatchable, Unpatchable{b.t.Name, says})
		} else if b.p.mm != nil && b.p.mm.Immutable && b.p.col.Methods["patch"] != nil && len(b.t.Setters) == 0 {
			unpatchable = append(unpatchable, Unpatchable{b.t.Name,
				"magic-modules marks the resource immutable and names no field it patches"})
		}
	}
	sort.Slice(unpatchable, func(i, j int) bool { return unpatchable[i].Type < unpatchable[j].Type })

	sort.Slice(c.Types, func(i, j int) bool { return c.Types[i].Name < c.Types[j].Name })
	// Stable, so two warnings with the same service and resource keep the
	// order they were found in (the collection walk, which is sorted) rather
	// than swapping between runs and showing up in a regeneration's diff.
	sort.SliceStable(warnings, func(i, j int) bool {
		if warnings[i].Service != warnings[j].Service {
			return warnings[i].Service < warnings[j].Service
		}
		return warnings[i].Resource < warnings[j].Resource
	})
	return &Result{Catalog: c, Warnings: warnings, Uncreatable: uncreatable, Unpatchable: unpatchable}, nil
}

// checkNamesAreUnique fails loudly, naming every offender, if the naming pass
// let two types through with the same name. Every duplicate is reported in
// one error rather than the first one found, because a naming bug that
// produces one collision usually produces several, and a person fixing this
// wants the whole list before touching anything.
func checkNamesAreUnique(types []*catalog.Type) error {
	count := map[string]int{}
	for _, t := range types {
		count[t.Name]++
	}
	var dups []string
	for name, n := range count {
		if n > 1 {
			dups = append(dups, fmt.Sprintf("%s (x%d)", name, n))
		}
	}
	if len(dups) == 0 {
		return nil
	}
	sort.Strings(dups)
	return fmt.Errorf("gen: the naming pass assigned one name to more than one type, which should be "+
		"impossible: %s", strings.Join(dups, ", "))
}

// WriteWarnings records every type that did not ship, plus every dropped
// reference on a type that DID ship. Never silently dropped (spec §4.1).
func WriteWarnings(path string, ws []Warning, uncreatable []Uncreatable, unpatchable []Unpatchable) error {
	var b strings.Builder
	fmt.Fprintf(&b, "# Types this catalog does not serve, and why.\n")
	fmt.Fprintf(&b, "# GENERATED by cmd/gen-gcp. Do not hand-edit.\n#\n")
	fmt.Fprintf(&b, "# tier 1 here means something ELSE shipped fine, not this exact entry: either\n")
	fmt.Fprintf(&b, "# a reference on some other type was dropped because its target name was\n")
	fmt.Fprintf(&b, "# ambiguous across products, or this collection was a legacy alias -- an\n")
	fmt.Fprintf(&b, "# older URL for the SAME resource another (named) collection already serves.\n")
	fmt.Fprintf(&b, "# tier 2 needs a ruling in gen/overlay.yaml naming every hook.\n")
	fmt.Fprintf(&b, "# tier 3 is not representable at all.\n\n")
	for _, w := range ws {
		fmt.Fprintf(&b, "tier%d\t%s/%s\t%s\n", w.Tier, w.Service, w.Resource, w.Reason)
	}
	if len(uncreatable) > 0 {
		fmt.Fprintf(&b, "\n# Types that SHIP but cannot be created, and the create-url placeholder\n")
		fmt.Fprintf(&b, "# that stops them. Each of these reads, imports, discovers and deletes;\n")
		fmt.Fprintf(&b, "# only create is refused, and it is refused before any request is sent\n")
		fmt.Fprintf(&b, "# rather than by Google answering a url with an empty segment in it.\n")
		fmt.Fprintf(&b, "# A placeholder here names a value nothing supplies: not an instance\n")
		fmt.Fprintf(&b, "# setting (project, region, zone, location), not a stored create\n")
		fmt.Fprintf(&b, "# binding, and not a settable attribute of the type's own.\n\n")
		for _, u := range uncreatable {
			fmt.Fprintf(&b, "nocreate\t%s\t%s\tneeds %s\n", u.Type, u.Template, strings.Join(u.Placeholders, ", "))
		}
	}
	if len(unpatchable) > 0 {
		fmt.Fprintf(&b, "\n# Types that SHIP and are REPLACED rather than patched, with the API's own\n")
		fmt.Fprintf(&b, "# sentence saying why. Google publishes the whole resource as the patch\n")
		fmt.Fprintf(&b, "# request schema and then limits it in prose, which Discovery cannot carry.\n")
		fmt.Fprintf(&b, "# A patch that silently drops the field you changed never converges, so\n")
		fmt.Fprintf(&b, "# these are refused until a `patchable:` entry in gen/overlay.yaml names\n")
		fmt.Fprintf(&b, "# the fields the API really takes. Replacement is destructive, but it is\n")
		fmt.Fprintf(&b, "# loud and it settles.\n\n")
		for _, u := range unpatchable {
			fmt.Fprintf(&b, "noupdate\t%s\t%s\n", u.Type, u.Says)
		}
	}
	return os.WriteFile(path, []byte(b.String()), 0o644)
}

// loadDocs reads every Discovery document under dir, in a deterministic
// (filename) order. schemas/_directory.json — the API directory listing
// fetch-schemas also writes there — is not a Discovery document itself and is
// skipped by name, rather than left to fail disco.Parse's "has no name" check
// and abort the whole build over a file nobody meant to parse.
func loadDocs(dir string) ([]*disco.Document, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") || e.Name() == "_directory.json" {
			continue
		}
		names = append(names, e.Name())
	}
	sort.Strings(names)

	docs := make([]*disco.Document, 0, len(names))
	for _, n := range names {
		data, err := os.ReadFile(filepath.Join(dir, n))
		if err != nil {
			return nil, fmt.Errorf("%s: %w", n, err)
		}
		d, err := disco.Parse(data)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", n, err)
		}
		docs = append(docs, d)
	}
	return docs, nil
}

// matchResource finds the magic-modules resource behind one Discovery
// collection, by case-insensitive singular comparison of the collection's
// leaf name against the resource's own name (widgets <-> Widget). It returns
// nil when nothing matches: not every Discovery collection has a
// magic-modules counterpart, and that alone is not a reason to exclude it.
// matchResource finds the magic-modules resource that describes one Discovery
// collection: the one whose NAME matches the collection's singularized leaf,
// but only when its own base_url names the SAME collection the Discovery
// document's create method posts to.
//
// A name match alone is not enough, and the one type in the corpus where it
// disagrees is the reason this check exists. compute's Discovery collection
// "vpnGateways" is HA VPN; magic-modules' resource named "VpnGateway" is the
// CLASSIC gateway ("kind: compute#targetVpnGateway", base_url
// ".../targetVpnGateways"), and HA VPN lives under the name "HaVpnGateway",
// which matches no Discovery leaf. Pairing on the name alone gave
// gcp.vpngateway magic-modules' base_url (targetVpnGateways) and Discovery's
// get path (vpnGateways): a type that created one resource and read another,
// so every create orphaned.
//
// So: take the name match when its collection agrees, or when there is
// nothing to compare (no base_url, or one whose last segment is a
// placeholder). When it disagrees, look for the resource in this product that
// names the right collection -- which is how HaVpnGateway is found -- and if
// exactly one does, that is the match.
//
// When nothing better is found the name match is still returned, unchanged
// from what this always did, and the type is left to the gate in buildType,
// which refuses to ship a type whose id template is not inside the collection
// its create posts to. Dropping the mm instead would be worse than the defect:
// a magic-modules resource is also what says a type has wire hooks, so
// unpairing one ships as generic-safe something the tier gate had refused
// (measured: binaryauthorization/Policy, refused for an unruled pre_delete
// hook, shipped the moment its pairing was discarded).
//
// The collection search runs ONLY to replace a rejected name match, never to
// find a match where there was none. Letting it run for every unmatched
// collection pairs types the generator has always built from Discovery alone
// with a magic-modules resource that merely posts to the same collection:
// measured over the corpus, that moved 21 types out of the catalog and pulled
// a new one in. None of that is this defect.
//
// The comparison is between the two WIRE paths, magic-modules' base_url and
// the Discovery create method's path, never between base_url and the
// collection's NAME: storage's bucket collection is named "buckets" but posts
// to "b", and a name comparison would unpair it.
func matchResource(mms []*mmv1.Resource, leaf, createPath string) *mmv1.Resource {
	want := strings.ToLower(singular(leaf))
	var byName *mmv1.Resource
	for _, mm := range mms {
		if strings.ToLower(mm.Name) == want {
			byName = mm
			break
		}
	}
	if byName == nil {
		return nil
	}
	wantColl := collectionLeaf(createPath)
	mmColl := collectionLeaf(byName.CreateURL)
	if mmColl == "" {
		mmColl = collectionLeaf(byName.BaseURL)
	}
	if mmColl == "" || wantColl == "" || mmColl == wantColl {
		return byName
	}
	var byCollection []*mmv1.Resource
	for _, mm := range mms {
		if collectionLeaf(mm.BaseURL) == wantColl {
			byCollection = append(byCollection, mm)
		}
	}
	if len(byCollection) == 1 {
		return byCollection[0]
	}
	return byName
}

// createPathOf is the path the Discovery document's own create method posts
// to, or "" when the collection publishes none (such a collection cannot
// ship anyway -- Classify refuses it).
func createPathOf(col disco.Collection) string {
	m := col.Methods["insert"]
	if m == nil {
		m = col.Methods["create"]
	}
	if m == nil {
		return ""
	}
	return m.Path
}

// collectionLeaf is the last literal path segment of a url template, with any
// query string dropped -- the collection a POST to it creates in. It returns
// "" when there is nothing to compare: an empty template, or one whose last
// segment is a placeholder rather than a literal (magic-modules writes
// "{{parent}}/things", and a few templates end in a capture outright).
func collectionLeaf(tmpl string) string {
	if i := strings.IndexByte(tmpl, '?'); i >= 0 {
		tmpl = tmpl[:i]
	}
	tmpl = strings.TrimSuffix(tmpl, "/")
	if tmpl == "" {
		return ""
	}
	last := tmpl[strings.LastIndex(tmpl, "/")+1:]
	if strings.ContainsAny(last, "{}") {
		return ""
	}
	return last
}

// singular strips the plural off a Discovery collection leaf. It only ever
// feeds a case-insensitive comparison against a short, known list of
// magic-modules resource names (matchResource) or a lowercased name segment
// (the Candidate built in Build) — never output a user reads — so covering
// the corpus's common REST plurals is enough; it does not need to be a real
// English inflector.
func singular(s string) string {
	switch {
	case strings.HasSuffix(s, "ies"):
		return s[:len(s)-3] + "y"
	case strings.HasSuffix(s, "ses"), strings.HasSuffix(s, "xes"), strings.HasSuffix(s, "zes"),
		strings.HasSuffix(s, "ches"), strings.HasSuffix(s, "shes"):
		return s[:len(s)-2]
	case strings.HasSuffix(s, "s"):
		return s[:len(s)-1]
	default:
		return s
	}
}

// scopeSegment identifies a Candidate.Scope from a Discovery collection's
// full path: "organization", "folder" or "billingaccount" when the
// collection is reached through that resource hierarchy root, "" for a
// project-scoped collection (col.Path[0] == "projects") or one reached
// through anything else.
//
// It is deliberately narrow. GCP has other axes along which the same leaf
// name recurs under one product WITHOUT being a different resource -- e.g.
// container's projects.zones.clusters is a legacy alias for
// projects.locations.clusters, not a second Cluster type -- and folding
// those into Scope too would rename every ordinary projects.zones.* and
// projects.regions.* type across the whole corpus (compute alone has dozens)
// on the strength of a resemblance to the three cases this actually needs to
// solve. The three below are it; the generator-level uniqueness check in
// Build catches anything scopeSegment doesn't, loudly, rather than silently
// merging it the way the pre-fix Candidate did.
// hierarchyRoot is catalog.Type.ParentRoot: the resource-hierarchy root a
// Discovery collection hangs off, which is also the first segment of every
// resource name that collection serves.
//
// It reports the segment VERBATIM ("projects", "organizations", "folders",
// "billingAccounts") because that is what it is compared against at runtime:
// the head of a Cloud Asset Inventory full resource name. scopeSegment, just
// below, answers a different question for a different consumer -- it names
// the NON-project roots in the singular, lowercased, because its answer
// becomes a segment of the assigned type name -- so the two deliberately do
// not share an implementation. Folding them together would mean either
// naming types "gcp.logging.folders.bucket" or comparing a resource name
// against "folder", and each is wrong in its own direction.
//
// "" for a collection rooted at anything else, which is honest: discovery
// only uses this to tell two types with the same asset type apart, and a
// root this does not recognise is one it cannot help with.
func hierarchyRoot(path []string) string {
	if len(path) == 0 {
		return ""
	}
	switch path[0] {
	case "projects", "organizations", "folders", "billingAccounts":
		return path[0]
	default:
		return ""
	}
}

func scopeSegment(path []string) string {
	if len(path) == 0 {
		return ""
	}
	switch path[0] {
	case "organizations":
		return "organization"
	case "folders":
		return "folder"
	case "billingAccounts":
		return "billingaccount"
	default:
		return ""
	}
}

// readPin reads gen/mmv1.lock, the pinned magic-modules commit fetch-schemas
// vendored, given the directory the vendored product tree lives in (its
// sibling). Absence is not an error: a test fixture has no lock file to read,
// and an empty MMV1Commit is a fine answer for one.
func readPin(mmv1Dir string) string {
	data, err := os.ReadFile(filepath.Join(filepath.Dir(mmv1Dir), "mmv1.lock"))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

// parentShapes maps a Discovery parameter pattern to what that placeholder
// MEANS in a create path.
//
// An API that takes its parent as one capture writes "{+name}/serviceAccounts"
// or "{+parent}/instances", and the placeholder is NOT the resource's own name
// -- iam publishes `pattern: ^projects/[^/]+$` for that very parameter, which
// says so exactly. Nothing filled it, so every create of such a type failed on
// string substitution before a byte reached Google.
//
// The pattern is the API's own statement of the shape, so it is what the
// binding is read from rather than a guess keyed on the placeholder's name.
// Only patterns that pin a parent COMPLETELY are listed: a pattern naming a
// resource ("^projects/[^/]+/topics/[^/]+$") is a different thing and is left
// alone.
var parentShapes = map[string]string{
	`^projects/[^/]+$`:                  "projects/{{project}}",
	`^projects/[^/]+/locations/[^/]+$`:  "projects/{{project}}/locations/{{location}}",
	`^projects/[^/]+/regions/[^/]+$`:    "projects/{{project}}/regions/{{region}}",
	`^projects/[^/]+/locations/global$`: "projects/{{project}}/locations/global",
}

// bindParentPlaceholders rewrites a collection template's placeholders into the
// parent they stand for, where the create method's own parameter pattern says
// what that is. A template with no such placeholder comes back unchanged, which
// is every magic-modules-sourced one -- those already spell the parent out.
func bindParentPlaceholders(tmpl string, create *disco.Method) string {
	if tmpl == "" || create == nil || len(create.Parameters) == 0 {
		return tmpl
	}
	for _, name := range pathPlaceholders(tmpl) {
		p := create.Parameters[name]
		if p == nil {
			continue
		}
		shape, ok := parentShapes[p.Pattern]
		if !ok {
			continue
		}
		// ONLY the reserved-expansion form. "{+parent}" is how Discovery
		// writes a capture that swallows a whole path, which is exactly the
		// shape a parent is. A single "{parent}" is one ordinary segment and
		// needs no binding, and "{{parent}}" is magic-modules' spelling in a
		// template that already spells the rest of the path its own way --
		// binding that one both corrupted the braces (the inner "{parent}"
		// matched, leaving "{projects/...}") and doubled a segment, which
		// dropped 7 types through the create-collection gate.
		tmpl = strings.ReplaceAll(tmpl, "{+"+name+"}", shape)
	}
	return tmpl
}

// resourceSchema resolves the schema describing THE RESOURCE -- what GCP
// stores and answers a read with -- and, separately, how the create request
// wraps it.
//
// For 216 of the 233 types the create request body IS the resource and there
// is no wrapper. Eleven use the AIP CreateXRequest shape instead: the request
// is {accountId, serviceAccount} while a read answers with ServiceAccount
// {displayName, email, name, ...}, and the two share not one field name.
//
// Declaring the request there describes what a user may SEND and not what GCP
// answers with. The host refuses any attribute a provider returns that the
// schema does not declare, so all eleven could neither create nor read --
// gcp.container.cluster among them, with 86 undeclared fields. Nothing caught
// it in 16 tasks because internal/gcpfake echoes back what it is sent, so a
// create response always had exactly the request's shape and the two schemas
// could never disagree in a test. The first live call this project ever made
// failed on it in three seconds.
//
// The resource is the GET method's response, because that is by definition
// what a read answers with, which is the thing the schema has to describe.
func resourceSchema(d *disco.Document, col disco.Collection, create *disco.Method) (
	body *disco.Schema, wrapper string, createOnly []string, err error) {

	get := col.Methods["get"]
	resourceRef := ""
	if get != nil && get.Response != nil {
		resourceRef = get.Response.Ref
	}
	requestRef := ""
	if create.Request != nil {
		requestRef = create.Request.Ref
	}

	// No get to learn the resource from (tagBindings has none), or the request
	// already IS the resource: the long-standing path, unchanged.
	if resourceRef == "" || resourceRef == requestRef {
		b, err := requestBodySchema(d, create)
		return b, "", nil, err
	}

	// A wrapper is a request property that refs the resource itself. Anything
	// else in the request is a create-time parameter that is not part of the
	// resource and will never come back from a read -- accountId, roleId.
	raw, ok := d.Schemas[requestRef]
	if ok && raw != nil {
		for _, name := range sortedKeys(raw.Properties) {
			p := raw.Properties[name]
			if p != nil && p.Ref == resourceRef {
				wrapper = name
				continue
			}
			createOnly = append(createOnly, name)
		}
	}
	if wrapper == "" {
		// The request names the resource nowhere, so this is not the wrapper
		// shape and guessing would be worse than the status quo.
		b, err := requestBodySchema(d, create)
		return b, "", nil, err
	}

	resolved, err := d.Resolve(d.Schemas[resourceRef])
	if err != nil {
		return nil, "", nil, err
	}
	return resolved, wrapper, createOnly, nil
}

// requestBodySchema resolves the schema that carries a method's settable
// shape: its request body, or (for a method with none, such as some list-only
// bodies) its response — some Discovery methods reuse a single schema for
// both directions.
func requestBodySchema(d *disco.Document, m *disco.Method) (*disco.Schema, error) {
	ref := ""
	switch {
	case m.Request != nil:
		ref = m.Request.Ref
	case m.Response != nil:
		ref = m.Response.Ref
	}
	if ref == "" {
		return nil, fmt.Errorf("%s has no request or response schema", m.ID)
	}
	raw, ok := d.Schemas[ref]
	if !ok {
		return nil, fmt.Errorf("%s references unknown schema %q", m.ID, ref)
	}
	return d.Resolve(raw)
}

// versionSegment matches an API version path segment: v1, v2, v1beta1, v3.
var versionSegment = regexp.MustCompile(`^v[0-9][0-9a-zA-Z]*$`)

// pathPrefixOf returns the leading segments of a Discovery method path that
// precede the resource hierarchy -- everything up to and including the first
// version segment, with a trailing "/".
//
// It is derived per TYPE, from that type's own method path, rather than once
// per API. Nothing in the pinned corpus REQUIRES that -- every method in every
// one of the 505 method-bearing collections yields the same prefix as its
// siblings, verified across all 43 Discovery documents -- so a per-API prefix
// would give the same answer today. Per-type is kept because it cannot go
// wrong if that ever stops being true, and because the prefix genuinely does
// vary BETWEEN APIs in a way no rule predicts: dns puts "dns/v1/" in front of
// every one of its 40 methods while networksecurity uses a bare "v1/".
//
// (An earlier version of this comment claimed dns mixed "dns/v1/" and "v1/"
// within the one API. It does not; all 40 dns methods use "dns/v1/". The
// claim came from the plan and was wrong there too.)
//
// Returns "" when the path has no version segment, which is the correct answer
// for compute, storage and bigquery: their Discovery servicePath already
// carries the version, so APIBaseURL is complete on its own.
func pathPrefixOf(methodPath string) string {
	segs := strings.Split(strings.TrimPrefix(methodPath, "/"), "/")
	for i, s := range segs {
		if versionSegment.MatchString(s) {
			return strings.Join(segs[:i+1], "/") + "/"
		}
	}
	return ""
}

// prefixMethodPath returns the method path PathPrefix is derived from: the
// collection's own get, or -- for the types that publish none -- its list,
// then its insert or create. Every one of those is a path Discovery joins
// onto ResolvedBaseURL, so all four answer the same question, and the first
// that exists is as good as any other.
//
// Taken from the type's OWN collection rather than from the service. In the
// pinned corpus every collection in an API agrees, so this is belt-and-braces
// rather than a fix for a known conflict -- see pathPrefixOf.
func prefixMethodPath(col disco.Collection) string {
	for _, name := range []string{"get", "list", "insert", "create"} {
		if m := col.Methods[name]; m != nil && m.Path != "" {
			return m.Path
		}
	}
	return ""
}

// buildType assembles one catalog.Type: BuildAttributes for the shape,
// ScopeOf and AwaitOf for how it's called, and the magic-modules URL fields
// for where.
//
// Every Attr.Ref is left holding the RAW magic-modules resource name it
// points at (BuildAttributes has no way to know the whole name map, and
// buildType itself only knows what's shipped so far in this one call —
// resolving here would let a reference resolve against a type that hasn't
// been decided yet, or that fails to build later in the same pass). Build's
// pass four resolves every Ref once every type in this run has either
// succeeded or been refused.
//
// It also CONSUMES the ruling, not merely validates it: AllForceNew marks
// every settable (non-Output) attribute ForceNew at every depth — this is
// what makes gcp.tagbinding honest, since it has create/delete/list and no
// patch — and ReadVia is recorded on the type so the runtime knows to read it
// by listing the parent instead.
func buildType(doc *disco.Document, col disco.Collection, mm *mmv1.Resource, name string, overlay *Overlay) (*catalog.Type, error) {
	create := col.Methods["insert"]
	if create == nil {
		create = col.Methods["create"]
	}
	if create == nil {
		return nil, fmt.Errorf("no insert or create method")
	}
	body, wrapper, createOnly, err := resourceSchema(doc, col, create)
	if err != nil {
		return nil, err
	}

	var ruling *Ruling
	if mm != nil {
		ruling = overlay.Rulings[doc.Name+"/"+mm.Name]
	}
	var aliases map[string]string
	if overlay.Aliases != nil {
		aliases = overlay.Aliases[name]
	}

	attrs, err := BuildAttributes(doc, body, mm, aliases)
	if err != nil {
		return nil, err
	}
	// A create-time parameter is settable and ForceNew, and a read never
	// returns it, so it has to be declared alongside the resource's own
	// attributes rather than instead of them. Added after BuildAttributes so
	// the resource can never be overwritten by a wrapper field of the same
	// name.
	if wrapper != "" && len(createOnly) > 0 {
		// Built through BuildAttributes on a schema holding just these
		// properties, rather than a second construction path. A parallel
		// builder would be one more place for the two to drift, which is the
		// failure this whole fix exists to undo.
		reqRaw := doc.Schemas[create.Request.Ref]
		only := &disco.Schema{Type: "object", Properties: map[string]*disco.Schema{}}
		for _, n := range createOnly {
			if reqRaw != nil && reqRaw.Properties[n] != nil {
				only.Properties[n] = reqRaw.Properties[n]
			}
		}
		extra, err := BuildAttributes(doc, only, nil, nil)
		if err != nil {
			return nil, fmt.Errorf("create-only parameters of %s: %w", create.ID, err)
		}
		for n, a := range extra {
			if _, taken := attrs[n]; taken {
				continue // the resource's own field wins; it is what a read returns
			}
			a.ForceNew = true
			a.CreateOnly = true
			a.Output = false
			attrs[n] = a
		}
	}
	if ruling != nil && ruling.AllForceNew {
		forceNewAll(attrs)
	}

	t := &catalog.Type{
		Name:       name,
		Service:    doc.Name,
		APIBaseURL: doc.ResolvedBaseURL(),
		ParentRoot: hierarchyRoot(col.Path),
		Attributes: attrs,
		// How the create call wants the resource wrapped. Empty for 216 of
		// the 233 types, whose create body is the resource itself.
		CreateWrapper: wrapper,
	}
	if mm != nil {
		t.Description = mm.Description
		t.BaseURL = mm.BaseURL
		t.CreateURL = mm.CreateURL
		t.DeleteURL = mm.DeleteURL
		t.SelfLink = mm.SelfLink
		// PATCH or nothing, from magic-modules too. Its update_verb is
		// Terraform's whole-object update, and BuildMask sends only what
		// the diff changed: POSTed to monitoring's metricDescriptors (a
		// create that overwrites) it would wipe every field it left out,
		// and pubsub's schemas :commit takes a CommitSchemaRequest wrapper
		// this provider does not build. Refused, the type is replaced on
		// change, which is the safe direction.
		if mm.UpdateVerb == "" || strings.EqualFold(mm.UpdateVerb, http.MethodPatch) {
			t.UpdateVerb = mm.UpdateVerb
			t.UpdateURL = mm.UpdateURL
		} else {
			t.UpdateURL = ""
		}
		t.UpdateMask = mm.UpdateMask
		if len(mm.ImportFormat) > 0 {
			t.ImportFormat = strings.Join(mm.ImportFormat, "\n")
		}
	} else {
		t.Description = strings.TrimSpace(body.Description)
	}

	// SelfLink is how Read, Update, Delete and Import address ONE resource, so a
	// type without it cannot be managed at all. magic-modules supplies it for
	// only 90 of the 233 types; for the other 143 the Discovery document
	// publishes the same thing as the collection's own `get` method path, and
	// every one of those 143 has one (a type with no `get` cannot reach tier 1
	// without a read_via ruling, so this is true by construction rather than by
	// luck).
	//
	// Taken from the type's OWN collection, not from the service: two
	// collections in one API have different get paths, and picking any of them
	// would address the wrong resource.
	if t.SelfLink == "" {
		if get := col.Methods["get"]; get != nil {
			t.SelfLink = get.Path
		}
	}
	// magic-modules writes some self_links as one bare placeholder,
	// "v3/{{name}}", where name is the FULL resource name
	// (projects/p/alertPolicies/123). As a {{...}} placeholder its slashes
	// would be escaped, and the collection check refused all five such types.
	// Discovery's get path says the same address correctly -- "v3/{+name}",
	// reserved expansion, slashes kept -- so it is used instead.
	if bareMMPlaceholderRE.MatchString(t.SelfLink) {
		if get := col.Methods["get"]; get != nil && isBareCapture(strings.TrimPrefix(get.Path, versionPrefixOf(get.Path))) {
			t.SelfLink = get.Path
		}
	}

	t.LockField = lockFieldOf(attrs)

	// UpdateVerb used to come from magic-modules and from nowhere else, which
	// meant it usually came from nowhere: magic-modules relies on its own
	// default when a resource's yaml omits `update_verb:`, and most omit it. A
	// type with no verb is not merely unpatchable — patch.go turns it into
	// "every change REPLACES the resource", and the generated reference then
	// tells the user the API publishes no update method. Measured 2026-09-23:
	// 154 shipping types were in that state and Google publishes a PATCH for at
	// least 33 of them, gcp.storage.bucket, gcp.network, gcp.subnetwork,
	// gcp.compute.instance and gcp.urlmap among them. Replacing a bucket takes
	// its objects with it.
	//
	// So ask the collection. See discoveredUpdate for what it refuses, which is
	// the more important half.
	// The envelope is a fact about the REQUEST, not about which verb was
	// chosen, so it applies whether magic-modules declared the verb or not.
	// It used to run only when magic-modules said nothing, and Pub/Sub's Topic
	// and Subscription both declare update_verb: PATCH -- so they would have
	// shipped sending a bare Topic with the mask on the query string, the one
	// request Pub/Sub rejects. Checked first because discoveredUpdate refuses
	// the envelope shape by design.
	//
	// A declared update_url is no obstacle when it names the same address as
	// the API's own patch method: it is then dropped, and the update goes to
	// the API's spelling of that address through self_link. See
	// templateAddressesMethod for why magic-modules' spelling cannot be kept.
	if (t.UpdateVerb == "" || t.UpdateVerb == http.MethodPatch) &&
		(t.UpdateURL == "" || templateAddressesMethod(t.UpdateURL, col.Methods["patch"])) {
		if wrapper, maskField := discoveredUpdateWrapper(doc, col); wrapper != "" {
			if restricted, _ := restrictedPatch(col); !restricted || overlay.Patchable[name] != nil {
				t.UpdateVerb, t.UpdateWrapper, t.UpdateMaskField = http.MethodPatch, wrapper, maskField
				t.UpdateURL = ""
				// The mask travels inside the envelope; a query-string one
				// as well would be a second mask the API never asked for.
				t.UpdateMask = false
				if allow := overlay.Patchable[name]; allow != nil {
					applyPatchAllowlist(attrs, allow.Fields)
					t.PatchOneField = allow.OneFieldPerPatch
				}
			}
		}
	}
	if t.UpdateVerb == "" {
		if verb, masked := discoveredUpdate(col, t.UpdateURL); verb != "" {
			allow := overlay.Patchable[name]
			restricted, _ := restrictedPatch(col)
			switch {
			case !restricted:
				t.UpdateVerb, t.UpdateMask = verb, masked
			case allow != nil:
				// A human read what the API says and wrote down which fields it
				// really patches. Everything else replaces, which is what the
				// API does with it anyway.
				t.UpdateVerb, t.UpdateMask = verb, masked
				applyPatchAllowlist(attrs, allow.Fields)
				t.PatchOneField = allow.OneFieldPerPatch
			default:
				// Left non-updatable on purpose: see patchlimits.go. Recorded
				// as a noupdate row in gen/warnings.txt by Build.
			}
		}
	}
	// magic-modules' resource-level `immutable:` says the same thing a
	// restricted patch says, as data rather than prose: nothing changes in
	// place except the fields that name their own update. Discovery still
	// publishes a PATCH for every one of these, so without this the proxies
	// and prefixes shipped patching fields the API does not change there
	// (a target HTTPS proxy's urlMap goes through setUrlMap). A human
	// `patchable:` list, where one exists, has already decided and wins.
	if mm != nil && mm.Immutable && t.UpdateVerb != "" && overlay.Patchable[name] == nil {
		if fields := immutableResourcePatchFields(mm); len(fields) > 0 {
			applyPatchAllowlist(attrs, fields)
		} else {
			t.UpdateVerb, t.UpdateMask, t.UpdateURL = "", false, ""
			t.UpdateWrapper, t.UpdateMaskField = "", ""
		}
	}

	// ImportFormat is the ID shape `infrena import` accepts, and Capabilities.Import
	// is derived from it — so a type without one cannot be adopted at all.
	// magic-modules supplies it for only 75 of the 233 types; the rest fall back
	// to SelfLink, which is the same thing by construction: a provider ID here IS
	// the relative resource name (spec §5.5), and SelfLink is its template.
	// Without this, 158 types generate fine and then silently refuse import.
	if t.ImportFormat == "" {
		t.ImportFormat = t.SelfLink
	}

	// SelfLink must address ONE resource. Two of the 233 types get a
	// magic-modules value that does not: gcp.storage.bucket's carries a query
	// string ("b/{{name}}?projection=full") and gcp.tagbinding's is a LIST url
	// ("tagBindings/?parent={{parent}}&pageSize=300"). Read, Delete and Import
	// all build from SelfLink, so both would address the wrong thing.
	//
	// Strip the query string; if what remains is not item-shaped (it ends in
	// "/", meaning nothing named the resource), fall back to the first
	// ImportFormat line, which by definition IS an item ID shape. That turns
	// tagbinding's into "tagBindings/{{name}}".
	if i := strings.IndexByte(t.SelfLink, '?'); i >= 0 {
		t.SelfLink = t.SelfLink[:i]
	}
	if strings.HasSuffix(t.SelfLink, "/") || t.SelfLink == "" {
		// The API's OWN single-item path first, and magic-modules' import
		// format only if there is none. A collection with no `get` still has
		// a `delete`, and a delete addresses exactly one resource -- that is
		// what it is for -- so its path is the id shape by definition.
		//
		// It matters for gcp.tagbinding, the one type in the corpus that
		// reaches here. magic-modules says the id is "tagBindings/{{name}}",
		// two segments. A real tag binding's name has FOUR:
		//
		//	tagBindings/%2F%2Fcloudresourcemanager.googleapis.com%2Fprojects%2F123456789012/tagValues/281479230039359
		//
		// Google percent-escapes the bound resource's own full name into one
		// segment and appends "tagValues/<id>" as two more. Against a
		// two-segment template ParseProviderID refuses the real id outright
		// ("has more segments than gcp.tagbinding's id shape"), so import
		// failed and every create stored an id addressing nothing. The API
		// says the shape itself: tagBindings.delete is "v3/{+name}" with
		// pattern "^tagBindings/.*$" -- reserved expansion, any number of
		// segments, anchored on the collection.
		if del := idTemplateFromDelete(col); del != "" {
			t.SelfLink = del
			// And it is what `infrena explain` must offer too. The
			// import_format magic-modules supplied describes a shape the API
			// does not accept, and a user reading it would type an id that
			// is refused. self_link goes in front of it rather than
			// replacing it: ParseProviderID still tries every line, and one
			// of magic-modules' shorthands may well work.
			t.ImportFormat = t.SelfLink + "\n" + t.ImportFormat
			// And the delete template too, because it is the SAME path: this
			// value came from the delete method. magic-modules says
			// "tagBindings/{{name}}", a plain placeholder, so itemURL would
			// percent-escape a name that is already escaped and DELETE
			// "tagBindings/%252F%252F..." -- a url addressing nothing, on the
			// one call whose failure leaves the resource behind. A delete_url
			// that disagrees with the API's own delete path is not an
			// override worth keeping.
			t.DeleteURL = t.SelfLink
		} else if first, _, _ := strings.Cut(t.ImportFormat, "\n"); first != "" {
			t.SelfLink = first
		}
	}
	if t.BaseURL == "" {
		// No magic-modules definition, or one with no base_url: fall back to
		// the create method's own path, which for a REST-style insert is the
		// collection path itself.
		t.BaseURL = create.Path
	}
	t.Scope = ScopeOf(t.BaseURL)

	// The verb a create is sent with, from the API itself. magic-modules says
	// so too where it matters (Pub/Sub's Topic declares create_verb: PUT), but
	// Discovery is the one that always does, and it is the method we call.
	//
	// A PUT create is sent to the new resource's OWN path, so the create url is
	// that method's path and not the collection magic-modules calls base_url.
	// The PathPrefix pass below strips the version from it with the rest.
	if create.HTTPMethod != "" && create.HTTPMethod != http.MethodPost {
		t.CreateVerb = create.HTTPMethod
		if t.CreateVerb == http.MethodPut {
			t.CreateURL = create.Path
		}
	}

	await, _ := AwaitOf(doc, create)
	t.Await = await
	// The delete's own answer, which is not always the create's: see
	// catalog.Type.DeleteAwait. Recorded only where it differs.
	if deleteAwait, _ := AwaitOf(doc, col.Methods["delete"]); deleteAwait != await {
		t.DeleteAwait = &deleteAwait
	}
	// The paths and the timeout below serve whichever await is not none. The
	// measured mismatches pair none with one kind, never two different
	// kinds, so the create's kind wins only where both exist.
	if await == catalog.AwaitNone && t.DeleteAwait != nil {
		await = *t.DeleteAwait
	}
	// TimeoutSeconds is a GUESS, not derived from any input source: nothing in
	// Discovery, magic-modules or the overlay says how long a mutation may
	// take. The three buckets below are keyed only off how the mutation is
	// awaited, on the assumption that a polled operation (compute-style or
	// long-running) is doing more work server-side than one that returns the
	// resource directly: 60s when nothing is awaited, 600s for a polled
	// compute-style operation, 1200s for a long-running one. Revisit against
	// real GCP operation timings once the real corpus is generated (Task 9).
	switch await {
	case catalog.AwaitComputeOperation:
		// Store the paths the API PUBLISHES rather than building them from a
		// scope word. Three earlier attempts at reconstruction were all wrong:
		// the wire path's literal segment is "operations", never
		// "zoneOperations" — that is only the Discovery COLLECTION name — and
		// the scope segment is "global", "regions/{region}" or "zones/{zone}".
		//
		// Measured 2026-09-22 across the 65 compute-style types: compute (58)
		// publishes a scope-specific wait; container (2) and sqladmin (5)
		// publish NO wait method anywhere, so a wait call against them 404s and
		// they must be polled with get instead. Both paths are recorded and the
		// runtime picks whichever exists.
		t.OperationWaitPath = operationWaitPath(doc, t.Scope)
		t.OperationPollPath = operationPollPath(doc)
		t.TimeoutSeconds = 600
	case catalog.AwaitLongRunning:
		t.TimeoutSeconds = 1200
	default:
		t.TimeoutSeconds = 60
	}

	// AssetType is also a GUESS: "<Discovery rootUrl's host>/<request body
	// schema's own id>" (e.g. "compute.googleapis.com/Instance") follows the
	// pattern Cloud Asset Inventory documents for its own asset types, but
	// nothing here actually queries CAI to confirm it matches for any given
	// resource. Nothing currently reads this field; it exists for whatever in
	// a later task wants to correlate a type with a CAI asset type.
	if body.ID != "" {
		host := strings.TrimSuffix(strings.TrimPrefix(doc.RootURL, "https://"), "/")
		t.AssetType = host + "/" + body.ID
	}

	// A longrunning type must know how to poll its own operations. The API
	// publishes that path itself, as operations.get — store it verbatim rather
	// than rebuilding it from a version segment, because a reconstruction can
	// drift from what the API accepts and this cannot.
	if await == catalog.AwaitLongRunning {
		t.OperationPollPath = operationPollPath(doc)
	}

	// The patterns describe the template the RUNTIME will actually request:
	// operationRequestURL uses the wait path when one exists and the poll path
	// otherwise, so that is the rule here too. Taking them from the other
	// template would produce a check on a url nobody builds.
	if tmpl := t.OperationWaitPath; tmpl != "" {
		t.OperationParamPatterns = operationParamPatterns(doc, tmpl)
	} else if t.OperationPollPath != "" {
		t.OperationParamPatterns = operationParamPatterns(doc, t.OperationPollPath)
	}

	if ruling != nil {
		for _, f := range ruling.Required {
			a := attrs[f]
			if a == nil || a.Output {
				return nil, fmt.Errorf("required names %q, which is not a settable attribute of this type", f)
			}
			a.Required = true
		}
	}
	if ruling != nil && len(ruling.ClearBeforeDelete) > 0 {
		for _, f := range ruling.ClearBeforeDelete {
			if a := attrs[f]; a == nil || a.Output {
				return nil, fmt.Errorf("clear_before_delete names %q, which is not a settable attribute of this type", f)
			}
		}
		t.ClearBeforeDelete = ruling.ClearBeforeDelete
	}
	if ruling != nil && ruling.ReadVia != "" {
		t.ReadVia = ruling.ReadVia
	}

	t.ListField = ListFieldOf(doc, col)

	// Bind any parent placeholder BEFORE the version prefix is stripped, so
	// both operate on the template as Discovery wrote it. The collection
	// template is the create path, so BaseURL and CreateURL both take it.
	t.BaseURL = bindParentPlaceholders(t.BaseURL, create)
	t.CreateURL = bindParentPlaceholders(t.CreateURL, create)

	// The API version lives in exactly one field, and this is where it is put
	// there. It has to run LAST, after every fallback above has settled:
	// SelfLink and ImportFormat are each the end of a chain (magic-modules,
	// then the Discovery `get` path, then SelfLink itself, then a query-string
	// strip, then a list-shape fallback), and only the value that chain
	// finally lands on is the one that has to be stripped.
	//
	// Stripping is conditional because the two sources disagree about the
	// version: a Discovery method path carries it ("v1/{+parent}/addressGroups")
	// while a magic-modules template is written relative to an already-
	// versioned base and never does. TrimPrefix leaves the latter alone by
	// itself, so one rule covers both.
	//
	// OperationPollPath and OperationWaitPath are deliberately left alone:
	// both are taken verbatim from the API's own operations methods and are
	// composed directly against APIBaseURL, never against PathPrefix.
	t.PathPrefix = pathPrefixOf(prefixMethodPath(col))
	if t.PathPrefix != "" {
		for _, p := range []*string{&t.BaseURL, &t.CreateURL, &t.UpdateURL,
			&t.DeleteURL, &t.SelfLink, &t.ImportFormat} {
			*p = strings.TrimPrefix(*p, t.PathPrefix)
		}
	}

	// After PathPrefix has stripped the version: before it, a Discovery-derived
	// self_link still reads "v1/{+name}" and is not recognisably bare.
	//
	// A bare-capture self_link ("{+name}") tells a user nothing about what an
	// id looks like, and it is what `infrena explain` and the generated
	// reference show as the import format for 88 types. The API publishes the
	// real shape as the path parameter's pattern, so show that instead.
	//
	// DOCUMENTATION ONLY, deliberately. ParseProviderID tries every template
	// and self_link is always among them, and a bare capture fits every id, so
	// nothing that parsed before stops parsing: identity is exactly as it was. Moving the structure into self_link
	// itself would make identity REFUSE a name the capture accepts today, and a
	// name refused after a create is a resource nothing tracks. Measured on
	// 2026-09-23 that trade buys nothing else: no bare-capture type declares an
	// attribute a structured id would recover.
	if isBareCapture(t.SelfLink) && (t.ImportFormat == "" || t.ImportFormat == t.SelfLink) {
		if shape := structuredIDTemplate(col, t.SelfLink); shape != "" {
			t.ImportFormat = shape
		}
	}

	// LAST, after PathPrefix has finished rewriting the templates: the
	// bindings are keyed by the placeholders of the template as it is
	// FINALLY stored, and a create url that still carried its version prefix
	// would be a different string.
	t.CreateBindings = createBindings(t, create)
	declareCreateURLParameters(t, create)
	// Its complement, and only for the case it deliberately leaves refused: a
	// placeholder that stays unresolved because an OUTPUT-ONLY attribute sits on
	// its key. That function will not declare over one, and is right not to --
	// one key cannot hold both the id the user chose and the full resource name
	// Google answers with. Giving the id a key of its own is the way out, so
	// this runs after, sees everything that function declared, and touches only
	// what it left behind.
	bindCreateQueryID(t, t.Attributes, create)
	// And when the url carries no id at all: see addCreateIDParameter.
	addCreateIDParameter(t, t.Attributes, create)
	// And a query value magic-modules leaves for its pre_create hook to fill.
	fillPreCreateTokens(t, t.Attributes, create)
	dropUnpublishedCreateQuery(t, create)

	if err := checkSelfLinkIsInsideTheCreateCollection(t); err != nil {
		return nil, err
	}
	// magic-modules marks a create url its pre_create hook rewrites with this
	// token (compute's NodeGroup: "?initialNodeCount=PRE_CREATE_REPLACE_ME").
	// Shipped, every create would send the token itself. Refused until the
	// generator can fill that parameter.
	if strings.Contains(t.CreateURL, "PRE_CREATE_REPLACE_ME") {
		return nil, fmt.Errorf("create url %q carries a token magic-modules' pre_create hook replaces, "+
			"which this provider cannot fill", t.CreateURL)
	}

	t.EndpointTemplate = endpointTemplate(doc, mm, t)
	t.UpdateURL = dropUnfillableQuery(t.UpdateURL, t)
	t.DeleteURL = dropUnfillableQuery(t.DeleteURL, t)

	// After self_link is final: a setter is admitted only on the address the
	// resource is read at. See discoveredSetters.
	t.Setters = discoveredSetters(doc, col, mm, t)
	applySetters(t, mm)

	return t, nil
}

// checkSelfLinkIsInsideTheCreateCollection refuses a type whose id template
// names a resource that its own create could not have made.
//
// self_link IS the provider-id template, and base_url (or create_url) is
// where a create POSTs. If the first is not the second plus the rest of a
// resource name, then the type creates one thing and reads another, and every
// create orphans: the resource exists under the url the POST went to, and the
// id that gets stored -- and that a later Read, Update and Delete address --
// names something else entirely. gcp.vpngateway was exactly this, posting to
// compute's targetVpnGateways (classic VPN) while its id named vpnGateways
// (HA VPN), because a magic-modules base_url was paired with a Discovery get
// path from a different collection. That pairing is now fixed in
// matchResource; this is the gate that means no future one can ship silently.
//
// It is the tier gate's own principle applied to the templates: a type the
// generator cannot vouch for does not ship. The type is refused with both
// templates named, which Build records in gen/warnings.txt like any other
// refusal.
//
// Only comparable templates are judged. A template with a reserved "{+x}"
// capture swallows any number of segments, so nothing can be concluded from
// it either way -- 88 of the 233 shipped types are in that position, and the
// runtime checks those the only way they can be checked, against the expanded
// values at create time (outsideCreatedCollection, internal/gcprov/crud.go).
// Everything else must be the collection itself (gcp.bigquery.table's
// base_url is the item's own path) or something under it (142 types are the
// collection plus one segment; gcp.resourcerecordset is two deeper, its own
// API's shape).
func checkSelfLinkIsInsideTheCreateCollection(t *catalog.Type) error {
	coll := t.CreateURL
	if coll == "" {
		coll = t.BaseURL
	}
	if i := strings.IndexByte(coll, '?'); i >= 0 {
		coll = coll[:i]
	}
	coll = strings.TrimSuffix(coll, "/")
	if coll == "" || t.SelfLink == "" {
		return nil
	}
	if strings.Contains(coll, "{+") || strings.Contains(t.SelfLink, "{+") {
		return nil
	}
	self, want := normalizeTemplateShape(t.SelfLink), normalizeTemplateShape(coll)
	if self == want || strings.HasPrefix(self, want+"/") {
		return nil
	}
	return fmt.Errorf("self_link %q is not inside %q, the collection its create posts to, "+
		"so it would name a resource this type never creates", t.SelfLink, coll)
}

// placeholderShapeRE matches one whole placeholder in either spelling a url
// template uses here, "{{x}}" or "{x}" (the reserved "{+x}" form never
// reaches it -- see checkSelfLinkIsInsideTheCreateCollection).
var placeholderShapeRE = regexp.MustCompile(`\{\{[^{}]+\}\}|\{[^{}]+\}`)

// normalizeTemplateShape collapses every placeholder to one token, so two
// templates that name the same path but spell a placeholder differently --
// magic-modules' "{{project}}" against a Discovery path's "{project}", which
// is the usual case when one template comes from each source -- compare on
// shape alone. (The runtime has the same function for the same reason, in
// internal/gcprov/crud.go; neither package imports the other.)
func normalizeTemplateShape(tmpl string) string {
	return placeholderShapeRE.ReplaceAllString(tmpl, "\x00")
}

// resolveRefs substitutes each Attr.Ref's magic-modules target for its
// infrena type name, at every depth a Ref could appear (in practice only ever
// the top level — attrs.go never sets Ref below it — but this walks the whole
// tree defensively rather than assuming that invariant holds forever).
//
// selfProduct is the product the resource being built itself belongs to
// (mm.Product; empty when mm is nil, in which case no Attr here can carry a
// Ref at all — see BuildAttributes). typeName and serviceName identify this
// resource in any warning resolution produces.
func resolveRefs(attrs map[string]*catalog.Attr, selfProduct, serviceName, typeName string, refByProduct map[string]string, refCandidates map[string]map[string]bool, warnings *[]Warning) {
	for _, a := range attrs {
		resolveAttrRef(a, selfProduct, serviceName, typeName, refByProduct, refCandidates, warnings)
	}
}

func resolveAttrRef(a *catalog.Attr, selfProduct, serviceName, typeName string, refByProduct map[string]string, refCandidates map[string]map[string]bool, warnings *[]Warning) {
	if a.Ref != nil {
		resolveOneRef(a, selfProduct, serviceName, typeName, refByProduct, refCandidates, warnings)
	}
	for _, f := range a.Fields {
		resolveAttrRef(f, selfProduct, serviceName, typeName, refByProduct, refCandidates, warnings)
	}
	if a.Elem != nil {
		resolveAttrRef(a.Elem, selfProduct, serviceName, typeName, refByProduct, refCandidates, warnings)
	}
}

// resolveOneRef resolves a single Ref's raw magic-modules target name to the
// infrena type that ships for it, in three steps:
//
//  1. <selfProduct>/<target> — magic-modules' resource: field names a
//     resource in the SAME product in the common case, and this is the only
//     step that's unambiguous by construction.
//  2. If that misses and exactly one OTHER product shipped something by that
//     bare name, use it — real cross-product references exist (e.g.
//     compute/Subnetwork -> networkconnectivity/InternalRange).
//  3. If more than one product shipped that name and step 1 missed, the
//     target is genuinely ambiguous: DROP the reference and warn naming the
//     candidates, rather than guess and write a wrong edge into a user's
//     generated configuration. (Zero candidates — nobody shipped that name
//     anywhere — also drops, silently: that's the ordinary dangling case.)
func resolveOneRef(a *catalog.Attr, selfProduct, serviceName, typeName string, refByProduct map[string]string, refCandidates map[string]map[string]bool, warnings *[]Warning) {
	target := a.Ref.Type
	if name, ok := refByProduct[selfProduct+"/"+target]; ok {
		a.Ref.Type = name
		return
	}
	cands := refCandidates[target]
	switch len(cands) {
	case 0:
		a.Ref = nil
	case 1:
		for product := range cands {
			a.Ref.Type = refByProduct[product+"/"+target]
		}
	default:
		products := make([]string, 0, len(cands))
		for product := range cands {
			products = append(products, product)
		}
		sort.Strings(products)
		*warnings = append(*warnings, Warning{
			Service:  serviceName,
			Resource: typeName + "." + a.Canonical,
			Tier:     TierGeneric,
			Reason: fmt.Sprintf("reference to %q is ambiguous across products %s; dropped rather than guessed",
				target, strings.Join(products, ", ")),
		})
		a.Ref = nil
	}
}

// dropRefsToMissingAttributes drops a resolved reference whose target type
// has no attribute by the referenced name, and records it. magic-modules'
// `imports:` defaults to selfLink, which compute resources have and the
// proto-first APIs do not: compute/TargetHttpsProxy's serverTlsPolicy
// imports selfLink from networksecurity's ServerTlsPolicy, whose identity is
// its name. infrena validates every reference against the target's schema
// and refuses the WHOLE plugin over one that dangles, so this is not
// cosmetic.
//
// Dropped rather than repointed at the target's name: the short-name rule
// reports a proto-first name as the short one, and these fields want a path.
// The attribute stays a settable string the user writes; only the edge goes.
func dropRefsToMissingAttributes(types []*catalog.Type, warnings *[]Warning) {
	byName := make(map[string]*catalog.Type, len(types))
	for _, t := range types {
		byName[t.Name] = t
	}
	var walk func(t *catalog.Type, a *catalog.Attr)
	walk = func(t *catalog.Type, a *catalog.Attr) {
		if a.Ref != nil {
			if target := byName[a.Ref.Type]; target != nil && target.Attributes[a.Ref.Attribute] == nil {
				*warnings = append(*warnings, Warning{
					Service:  t.Service,
					Resource: t.Name + "." + a.Canonical,
					Tier:     TierGeneric,
					Reason: fmt.Sprintf("reference to %s.%s dropped: %s has no attribute %q",
						a.Ref.Type, a.Ref.Attribute, a.Ref.Type, a.Ref.Attribute),
				})
				a.Ref = nil
			}
		}
		for _, f := range a.Fields {
			walk(t, f)
		}
		if a.Elem != nil {
			walk(t, a.Elem)
		}
	}
	for _, t := range types {
		for _, a := range t.Attributes {
			walk(t, a)
		}
	}
}

// forceNewAll marks every settable (non-Output) attribute ForceNew, at every
// depth. Consumed only when a ruling sets AllForceNew.
func forceNewAll(attrs map[string]*catalog.Attr) {
	for _, a := range attrs {
		forceNewAttr(a)
	}
}

func forceNewAttr(a *catalog.Attr) {
	if !a.Output {
		a.ForceNew = true
	}
	for _, f := range a.Fields {
		forceNewAttr(f)
	}
	if a.Elem != nil {
		forceNewAttr(a.Elem)
	}
}

// runtimeOperationPlaceholders is the set of placeholder names the runtime
// can fill when it expands an operation template. It must stay in step with
// the attrs map operationRequestURL builds in internal/gcprov/await.go: that
// function is the only caller that expands these paths, so a placeholder it
// does not supply cannot be filled by anything, and a template naming one is
// dead on arrival.
var runtimeOperationPlaceholders = map[string]bool{
	"operation": true,
	"name":      true,
	"project":   true,
	"zone":      true,
	"region":    true,
}

// operationPollPath returns the API's own operations.get path.
//
// It collects every candidate and chooses, rather than taking the first one a
// map walk happens to reach. Map iteration is unordered, and container
// publishes two operations collections -- projects.locations.operations
// ("v1/{+name}") and projects.zones.operations
// ("v1/projects/{projectId}/zones/{zone}/operations/{operationId}"). Taking
// whichever came first made the generator produce four different catalogs
// across twelve runs, and half the time it chose a path the runtime cannot
// expand at all: operationRequestURL supplies operation/name/project/zone/
// region and nothing supplies projectId or operationId, so every GKE cluster
// mutation failed at the polling step.
//
// The rule: prefer a template whose placeholders the runtime can actually
// fill, and among those prefer the reserved-expansion "{+name}" form, which
// is the shape every modern GCP API publishes. Ties break on the sorted path
// string so the result never depends on map order.
//
// Measured across the corpus on 2026-09-22: every one of the 97 longrunning
// types' APIs publishes one, in three shapes — v1/{+name} (72), v2/{+name}
// (17), v3/{+name} (8). An empty return therefore means something changed
// upstream, not that this API never had one, and the caller should treat it as
// a type that cannot be awaited rather than guessing a path.
func operationPollPath(doc *disco.Document) string {
	var found []string
	var walk func(res map[string]*disco.Resource)
	walk = func(res map[string]*disco.Resource) {
		for _, name := range sortedKeys(res) {
			r := res[name]
			if name == "operations" {
				if m := r.Methods["get"]; m != nil && m.Path != "" {
					found = append(found, m.Path)
				}
			}
			walk(r.Resources)
		}
	}
	walk(doc.Resources)
	if len(found) == 0 {
		return ""
	}
	sort.Strings(found)

	// Rank: an expandable "{+name}" path beats any other expandable one, and
	// an expandable path of any shape beats one the runtime would choke on.
	// Only if every candidate is unexpandable do we return one anyway -- the
	// generator has nothing better to offer, and await.go reports the missing
	// placeholder by name at the point it matters.
	best, bestRank := "", -1
	for _, path := range found {
		rank := 0
		if runtimeCanExpand(path) {
			rank = 1
			if strings.Contains(path, "{+name}") {
				rank = 2
			}
		}
		if rank > bestRank {
			best, bestRank = path, rank
		}
	}
	return best
}

// operationParamPatterns returns the regular expressions the API publishes for
// the parameters of the operation method whose path is exactly `path`, keyed by
// placeholder name.
//
// It finds the method BY ITS PATH rather than repeating the search and ranking
// that chose it. A second search could settle on a different operations method
// -- container publishes two -- and patterns describing a method other than the
// one whose template we stored would be a check that agrees with itself and
// nothing else. Task 14 learned the same rule about request bodies: derive from
// the template actually used, never from one that should match.
//
// Only placeholders the path actually names are returned, and only where
// Discovery publishes a pattern at all, which for most parameters it does not.
func operationParamPatterns(doc *disco.Document, path string) map[string]string {
	if path == "" {
		return nil
	}
	wanted := map[string]bool{}
	for _, n := range pathPlaceholders(path) {
		wanted[n] = true
	}
	// Collect every pattern published for each parameter, not the last one
	// seen. ONE API CAN PUBLISH SEVERAL OPERATIONS COLLECTIONS AT THE SAME
	// PATH -- iam has four, all "v1/{+name}", each with its own pattern -- and
	// which one a given type's operations live in is not decidable from the
	// path. Storing whichever was walked last would be a coin toss, and a
	// stored coin toss is worse than a gap: the check would look thorough and
	// assert something arbitrary.
	seen := map[string]map[string]bool{}
	var walk func(res map[string]*disco.Resource)
	walk = func(res map[string]*disco.Resource) {
		for _, name := range sortedKeys(res) {
			r := res[name]
			// OPERATIONS COLLECTIONS ONLY. "v1/{+name}" is the commonest path
			// shape in these documents and ordinary getters use it constantly,
			// so walking every method that matches would gather patterns from
			// resources that are not operations at all, make almost every
			// parameter look ambiguous, and drop it -- which is how the first
			// version of this silently collected nothing for container, the one
			// type the check exists for.
			if name == "operations" || strings.HasSuffix(name, "Operations") {
				for _, mn := range sortedKeys(r.Methods) {
					m := r.Methods[mn]
					if m == nil || m.Path != path {
						continue
					}
					for _, pn := range sortedKeys(m.Parameters) {
						if !wanted[pn] || m.Parameters[pn].Pattern == "" {
							continue
						}
						if seen[pn] == nil {
							seen[pn] = map[string]bool{}
						}
						seen[pn][m.Parameters[pn].Pattern] = true
					}
				}
			}
			walk(r.Resources)
		}
	}
	walk(doc.Resources)

	var out map[string]string
	for _, pn := range sortedKeys(seen) {
		if len(seen[pn]) != 1 {
			continue // ambiguous: left unchecked rather than guessed
		}
		for pattern := range seen[pn] {
			if out == nil {
				out = map[string]string{}
			}
			out[pn] = pattern
		}
	}
	return out
}

// runtimeCanExpand reports whether every placeholder in a Discovery method
// path is one runtimeOperationPlaceholders names.
func runtimeCanExpand(path string) bool {
	for _, name := range pathPlaceholders(path) {
		if !runtimeOperationPlaceholders[name] {
			return false
		}
	}
	return true
}

// pathPlaceholders returns the placeholder names in a url template, in the
// order they appear, stripping RFC 6570's reserved-expansion "+" the same way
// gcprov.ExpandURL does. Discovery writes single braces; the double-brace
// magic-modules spelling is accepted too so the two can never disagree about
// what a template needs.
func pathPlaceholders(tmpl string) []string {
	var names []string
	for i := 0; i < len(tmpl); {
		if tmpl[i] != '{' {
			i++
			continue
		}
		openLen, closeSeq := 1, "}"
		if i+1 < len(tmpl) && tmpl[i+1] == '{' {
			openLen, closeSeq = 2, "}}"
		}
		rel := strings.Index(tmpl[i+openLen:], closeSeq)
		if rel == -1 {
			break
		}
		content := strings.TrimSpace(tmpl[i+openLen : i+openLen+rel])
		content = strings.TrimPrefix(content, "+")
		// magic-modules marks a placeholder whose value must be url-escaped
		// with a leading "%" (storage's "{{%object}}", whose value is an
		// object name containing "/"). It is an instruction to the expander,
		// not part of the name -- gcprov.ExpandURL strips it the same way --
		// so a check that kept it would report "%object" as a placeholder no
		// attribute matches while "object" sits right there in the schema.
		content = strings.TrimPrefix(content, "%")
		names = append(names, content)
		i += openLen + rel + len(closeSeq)
	}
	return names
}

// sortedKeys returns m's keys in sorted order, so a walk over it reaches the
// same entries in the same order on every run.
func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// operationWaitPath returns the API's own operations wait path for a scope, or
// "" when the API publishes no wait method at all.
//
// Only compute does, among the services in this catalog. An empty return is a
// real answer meaning "poll with get", not a failure.
func operationWaitPath(doc *disco.Document, scope catalog.Scope) string {
	coll := map[catalog.Scope]string{
		catalog.ScopeGlobal:   "globalOperations",
		catalog.ScopeRegional: "regionOperations",
		catalog.ScopeZonal:    "zoneOperations",
	}[scope]
	if coll == "" {
		return ""
	}
	r := doc.Resources[coll]
	if r == nil {
		return ""
	}
	if m := r.Methods["wait"]; m != nil {
		return m.Path
	}
	return ""
}

// patternWildcardRE matches the one thing a Discovery parameter pattern uses
// to mean "exactly one path segment, anything": "[^/]+", optionally wrapped
// in a capture group. It is matched and replaced BEFORE the pattern is split
// on "/", because the character class contains a slash of its own -- a naive
// split turns "projects/[^/]+" into three pieces, two of them regex
// fragments, and every pattern in the corpus then reads as unparseable.
var patternWildcardRE = regexp.MustCompile(`\(\[\^/\]\+\)|\[\^/\]\+`)

// patternMetaChars are the regex characters a pattern segment may not carry
// once the wildcards are out of it. A segment holding one is a pattern this
// cannot read, and reading it wrongly would build a url out of a regular
// expression nobody verified.
const patternMetaChars = `[](){}^$+*?.|\\`

// wildcardMark stands in for one "[^/]+" while the pattern is split into
// segments. A byte no url or regular expression contains.
const wildcardMark = "\x00"

// templateFromPattern turns a Discovery path parameter's pattern into a url
// sub-template, or "" when the pattern is not a plain sequence of literal
// and single-wildcard segments.
//
//	^projects/[^/]+$                          -> projects/{project}
//	^projects/[^/]+/serviceAccounts/[^/]+$    -> projects/{project}/serviceAccounts/{serviceAccount}
//	^projects/[^/]+/locations/[^/]+$          -> projects/{project}/locations/{location}
//
// A wildcard is named after the LITERAL SEGMENT IN FRONT OF IT, singular --
// not a convention this generator invents, but the one Discovery itself uses
// for the same segments elsewhere ("{project}", "{instance}",
// "{managedZone}"). A wildcard with nothing but another wildcard in front of
// it cannot be named and the whole pattern is refused: logging's
// locations.buckets says `^[^/]+/[^/]+/locations/[^/]+$`, which does not even
// say which hierarchy root it means, and inventing a name for it would be
// inventing the url.
//
// The result is a TEMPLATE, not a value. Whether its own placeholders can be
// filled is a separate question, answered by
// catalog.Type.UnresolvedCreatePlaceholders: iam's serviceAccounts.create
// yields "projects/{project}", every placeholder of which an instance
// supplies, while its keys.create yields
// "projects/{project}/serviceAccounts/{serviceAccount}", whose second
// placeholder nothing supplies -- so the first ships and the second does not.
func templateFromPattern(pattern string) string {
	p := strings.TrimSuffix(strings.TrimPrefix(pattern, "^"), "$")
	if p == "" {
		return ""
	}
	segs := strings.Split(patternWildcardRE.ReplaceAllString(p, wildcardMark), "/")
	out := make([]string, 0, len(segs))
	for i, seg := range segs {
		if seg != wildcardMark {
			if seg == "" || strings.Contains(seg, wildcardMark) || strings.ContainsAny(seg, patternMetaChars) {
				return ""
			}
			out = append(out, seg)
			continue
		}
		if i == 0 || segs[i-1] == wildcardMark {
			return "" // nothing in front of it to name it after
		}
		out = append(out, "{"+lowerFirst(singularSegment(segs[i-1]))+"}")
	}
	return strings.Join(out, "/")
}

// singularSegment singularizes a url COLLECTION segment, so the wildcard
// after it can be named the way Discovery names the same segment elsewhere.
//
// Deliberately NOT `singular`, which singularizes a magic-modules resource
// name for the naming pass and is pinned by gen/names.lock.json -- changing
// it would rename shipped types. It is also wrong for this job: its "ses"
// rule turns "databases" into "databas", so spanner's
// "^projects/[^/]+/instances/[^/]+/databases/[^/]+$" would ask for a
// placeholder nobody could recognise. Here the "es" is only dropped after a
// sibilant, which is the rule English actually follows.
func singularSegment(s string) string {
	switch {
	case strings.HasSuffix(s, "ies"):
		return s[:len(s)-3] + "y"
	case strings.HasSuffix(s, "sses"), strings.HasSuffix(s, "xes"), strings.HasSuffix(s, "zes"),
		strings.HasSuffix(s, "ches"), strings.HasSuffix(s, "shes"):
		return s[:len(s)-2]
	case strings.HasSuffix(s, "s"):
		return s[:len(s)-1]
	default:
		return s
	}
}

// lowerFirst lowercases a name's first rune, so a collection segment
// ("ServiceAccounts" never occurs, but "Instances" style input would) always
// yields a lowerCamel placeholder of the shape Discovery itself writes.
func lowerFirst(s string) string {
	if s == "" {
		return s
	}
	return strings.ToLower(s[:1]) + s[1:]
}

// snakeToCamel turns a magic-modules placeholder spelling into the Discovery
// one: "dataset_id" -> "datasetId". magic-modules writes url templates in
// snake_case whatever the API calls the field, so a placeholder and the
// attribute it names routinely differ by nothing but this.
func snakeToCamel(s string) string {
	parts := strings.Split(s, "_")
	for i := 1; i < len(parts); i++ {
		if parts[i] != "" {
			parts[i] = strings.ToUpper(parts[i][:1]) + parts[i][1:]
		}
	}
	return strings.Join(parts, "")
}

// attrCandidate is one place in the attribute tree a placeholder could bind to.
type attrCandidate struct {
	path  string
	depth int
}

// findAttrPath searches the WHOLE attribute tree for the settable attribute a
// create-url placeholder names, and returns its dotted path, or "" when
// there is no unambiguous answer.
//
// Through Fields AND Elem, because catalog.Attr has two recursion edges and
// a walk that follows only Fields under-reports this corpus by a factor of
// seven (see catalog/real_test.go's eachAttributeLevel). Elem is walked so
// that a candidate INSIDE a list is seen -- and then rejected: a list has
// many elements and no url segment can name which one, so binding to one
// would be a coin flip. Seeing them is what makes the rejection deliberate
// rather than accidental.
//
// Three spellings are accepted, in one pass, because a template and an
// attribute can differ in any of them: the placeholder itself, its
// camelCase form (magic-modules writes snake_case), and the attribute's own
// generated snake_case alias. gcp.bigquery.table's "{{dataset_id}}" reaches
// tableReference.datasetId by the second.
//
// SHALLOWEST WINS, and only when the shallowest is unique. gcp.bigquery.table
// has SIX attributes whose camelCase name is datasetId --
// tableReference.datasetId, cloneDefinition.baseTableReference.datasetId,
// snapshotDefinition.baseTableReference.datasetId,
// tableReplicationInfo.sourceTable.datasetId and two inside lists. Only one
// of them is the table's own dataset, and it is the shallowest; the others
// name some OTHER table's. A rule that took "the first match" would bind the
// create url to whichever the map walk reached first, differently on
// different runs.
func findAttrPath(attrs map[string]*catalog.Attr, placeholder string) string {
	want := map[string]bool{placeholder: true, snakeToCamel(placeholder): true}
	var found []attrCandidate
	var walk func(prefix string, depth int, inList bool, level map[string]*catalog.Attr)
	walk = func(prefix string, depth int, inList bool, level map[string]*catalog.Attr) {
		for key, a := range level {
			path := key
			if prefix != "" {
				path = prefix + "." + key
			}
			if !inList && !a.Output && (want[key] || want[a.Canonical] || hasAlias(a, placeholder)) {
				found = append(found, attrCandidate{path, depth})
			}
			if len(a.Fields) > 0 {
				walk(path, depth+1, inList, a.Fields)
			}
			if a.Elem != nil && len(a.Elem.Fields) > 0 {
				walk(path+"[]", depth+1, true, a.Elem.Fields)
			}
		}
	}
	walk("", 1, false, attrs)
	if len(found) == 0 {
		return ""
	}
	sort.Slice(found, func(i, j int) bool {
		if found[i].depth != found[j].depth {
			return found[i].depth < found[j].depth
		}
		return found[i].path < found[j].path
	})
	if len(found) > 1 && found[1].depth == found[0].depth {
		return "" // ambiguous at the shallowest depth: nothing here can choose
	}
	return found[0].path
}

func hasAlias(a *catalog.Attr, name string) bool {
	for _, al := range a.Aliases {
		if al == name {
			return true
		}
	}
	return false
}

// urlPlaceholder is one placeholder of a url template, with the one thing
// about its spelling that changes what may fill it.
type urlPlaceholder struct {
	Name string
	// Reserved marks RFC 6570's "{+x}" form. It is not decoration: reserved
	// expansion exists precisely because the value contains "/" and must not
	// be escaped, so a reserved placeholder always stands for a MULTI-SEGMENT
	// PATH. That is why no attribute is ever bound to one -- an attribute
	// holding a leaf value named "parent" somewhere down the tree is not the
	// hierarchical parent of this resource, and gcp.extensionbinding proves
	// it: its "{+parent}" would otherwise bind to target.scope.parent, which
	// is a field of the thing the binding points AT.
	Reserved bool
}

// templatePlaceholders parses a url template into its placeholders, in order,
// keeping the reserved "+" marker that pathPlaceholders discards.
func templatePlaceholders(tmpl string) []urlPlaceholder {
	var out []urlPlaceholder
	for i := 0; i < len(tmpl); {
		if tmpl[i] != '{' {
			i++
			continue
		}
		openLen, closeSeq := 1, "}"
		if i+1 < len(tmpl) && tmpl[i+1] == '{' {
			openLen, closeSeq = 2, "}}"
		}
		rel := strings.Index(tmpl[i+openLen:], closeSeq)
		if rel == -1 {
			break
		}
		content := strings.TrimSpace(tmpl[i+openLen : i+openLen+rel])
		ph := urlPlaceholder{Reserved: strings.HasPrefix(content, "+")}
		content = strings.TrimPrefix(content, "+")
		ph.Name = strings.TrimPrefix(content, "%")
		out = append(out, ph)
		i += openLen + rel + len(closeSeq)
	}
	return out
}

// createBindings resolves the create template's placeholders that name
// neither an instance scope setting nor one of the type's own settable
// top-level attributes, and returns what it could resolve.
//
// RESOLVED HERE, AT GENERATION TIME, and stored on the type -- the same
// decision PathPrefix embodies. A runtime search would re-answer the same
// question on every request, and could answer it differently as the
// attribute tree changes; storing the concrete path means the answer is
// reviewed once, in a diff, like any other generated fact.
//
// create is the Discovery method the POST goes to. Its path parameters carry
// the patterns that say what a reserved "{+name}" or "{+parent}" actually
// means, which is the only source for it: one placeholder spelling means two
// different things in one collection (iam's serviceAccounts.create "name" is
// the PARENT project, the same collection's get "name" is the service
// account), and nothing but the pattern tells them apart.
func createBindings(t *catalog.Type, create *disco.Method) map[string]*catalog.CreateBinding {
	out := map[string]*catalog.CreateBinding{}
	for _, ph := range templatePlaceholders(t.CreateTemplate()) {
		if _, done := out[ph.Name]; done {
			continue
		}
		if catalog.IsCreateScopeSetting(ph.Name) {
			continue
		}
		if a := t.TopLevelAttr(ph.Name); a != nil && !a.Output {
			continue
		}
		if create != nil {
			if p := create.Parameters[ph.Name]; p != nil && p.Location == "path" && p.Pattern != "" {
				if tmpl := templateFromPattern(p.Pattern); tmpl != "" {
					out[ph.Name] = &catalog.CreateBinding{Template: withoutRepeatedTail(tmpl, t.CreateTemplate(), ph.Name)}
					continue
				}
			}
		}
		if ph.Reserved {
			continue
		}
		if path := findAttrPath(t.Attributes, ph.Name); path != "" {
			out[ph.Name] = &catalog.CreateBinding{Attr: path}
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// declareCreateURLParameters declares, as attributes of the type, the create
// url placeholders that name a URL PARAMETER rather than a body field --
// gcp.sslcert's "{instance}", gcp.artifactregistry.rule's "{{repository_id}}"
// and "{{rule_id}}", gcp.interceptdeployment's
// "{{intercept_deployment_id}}".
//
// WHY THEY WERE MISSING. A type's attributes come from its create method's
// REQUEST BODY schema, and a path or query parameter is not in the body:
// Google takes the repository from the url and the new resource's id from
// "?ruleId=", so neither appears among the properties the generator walks.
// The result was a type whose own url named something its schema never
// declared, which no configuration could then supply -- 32 of the 233
// shipped types, the single largest cause behind the 38 that could not build
// a create url at all.
//
// ONLY WHEN THE VALUE ROUND-TRIPS. A declared attribute that a later Read
// cannot recover is worse than a missing one: it would be set in
// configuration, absent from the state read back, and every plan after a
// successful apply would propose a change. The value is recoverable exactly
// when the placeholder also appears in the type's self_link, because
// self_link IS the provider-id template and ParseProviderID keys what it
// recovers by self_link's own placeholder names -- which stateFrom then puts
// into state through declaredIDAttrs. A placeholder that is NOT in self_link
// (logging's "{+parent}" under an organization, whose self_link is a bare
// "{+name}") is left unresolved and the type does not ship, rather than
// shipping a type that would never converge.
//
// Required and ForceNew, both for the same reason: the value is part of the
// resource's own NAME. A create cannot proceed without it, and changing it
// does not modify the resource, it names a different one.
func declareCreateURLParameters(t *catalog.Type, create *disco.Method) {
	inSelfLink := map[string]bool{}
	for _, ph := range pathPlaceholders(t.SelfLink) {
		inSelfLink[ph] = true
	}
	for _, ph := range t.UnresolvedCreatePlaceholders() {
		if !inSelfLink[ph] {
			continue
		}
		// An attribute is already there and the only reason the placeholder
		// is unresolved is that it is Output-only. Declaring over it would
		// mean one key holding two different values -- the id the user chose
		// and the full resource name Google answers with -- so the type is
		// left refused and the disagreement stays visible. See the task 18b
		// report: 6 types are in this position.
		if t.TopLevelAttr(ph) != nil {
			continue
		}
		// A name the generator would have had to rename around an infrena
		// keyword cannot be declared under the placeholder's own spelling,
		// and a renamed key is not what ExpandURL would look up. None of the
		// corpus's url parameters hit this; the guard is here so a future
		// one is refused rather than silently mis-keyed.
		if keywords[ph] != "" {
			continue
		}
		desc := fmt.Sprintf("The %s this %s belongs to. It is part of the resource's name, "+
			"supplied in the create url rather than in the request body.", ph, t.Name)
		if create != nil {
			if p := create.Parameters[ph]; p != nil && p.Description != "" {
				desc = strings.TrimSpace(p.Description)
			}
		}
		t.Attributes[ph] = &catalog.Attr{
			Canonical:   ph,
			Kind:        value.KindString,
			Required:    true,
			ForceNew:    true,
			Description: desc,
		}
	}
}

// patternLiteralPrefixRE reads the literal path prefix out of a Discovery
// parameter pattern whose tail is an unconstrained ".*" -- "^tagBindings/.*$"
// gives "tagBindings". Only this exact shape, because it is the only one
// that says "a fixed collection, then a name of any depth"; anything richer
// is a constraint this cannot summarise into a template.
var patternLiteralPrefixRE = regexp.MustCompile(`^\^([A-Za-z0-9]+)/\.\*\$$`)

// idTemplateFromDelete is the id template the API's own delete method
// implies, or "" when it implies none.
//
// A delete addresses exactly one resource, so its path IS an id shape. The
// path is taken verbatim except for one addition: when it is a bare reserved
// placeholder AND the parameter's pattern anchors the value on a literal
// collection segment, that segment goes back in front. The anchor is not
// decoration -- without it the template is "{+name}", which accepts any
// string a user types and turns a typo into a 404 from Google instead of a
// refusal naming the type. A wrong id that parses is worse than one that
// errors.
func idTemplateFromDelete(col disco.Collection) string {
	del := col.Methods["delete"]
	if del == nil || del.Path == "" {
		return ""
	}
	path := strings.TrimPrefix(del.Path, pathPrefixOf(del.Path))
	phs := templatePlaceholders(path)
	if len(phs) != 1 || !phs[0].Reserved || path != "{+"+phs[0].Name+"}" {
		return path
	}
	p := del.Parameters[phs[0].Name]
	if p == nil || p.Location != "path" {
		return path
	}
	m := patternLiteralPrefixRE.FindStringSubmatch(p.Pattern)
	if m == nil {
		return path
	}
	return m[1] + "/" + path
}

// discoveredUpdate derives an update method from a collection's own Discovery
// entry, for the types magic-modules says nothing about. It returns the verb
// and whether the method takes an updateMask query parameter, or "" to leave
// the type non-updatable.
//
// What it REFUSES matters more than what it accepts, because the failure modes
// are silent and destructive rather than loud:
//
//   - PUT is never accepted, only PATCH. BuildMask emits a PARTIAL body — just
//     the attributes that changed — and PUT replaces the resource with the body
//     it is handed, so a partial body against a PUT endpoint clears every field
//     the diff left out. compute's instances collection publishes `update`
//     (PUT) and no patch at all: accepting PUT would start silently wiping
//     virtual machines. A PUT-only type keeps replacing instead, which is
//     wasteful and honest rather than lossy and quiet.
//
//   - A patch whose request schema is not the resource's own is refused. That
//     is the Pub/Sub shape: topics.patch takes UpdateTopicRequest{topic,
//     updateMask}, so the resource is WRAPPED and the mask lives in the body,
//     while Provider.Update sends the bare resource with the mask on the query
//     string. Nothing in the catalog has this shape today and this is what
//     keeps it that way. A patch that publishes no request schema at all is
//     refused for the same reason: nothing said the body is the resource.
//
//   - A patch whose path differs from the collection's own `get` is refused,
//     because Update addresses the resource through SelfLink and SelfLink is
//     the get path. Deriving a verb for a patch that lives somewhere else would
//     send it to the wrong url.
//
// The mask is read separately from the verb rather than implied by it: 15 of
// the types that were already updatable PATCH without a mask, and sending a
// query parameter an API never asked for is its own bug.
//
// WHAT IT DOES NOT CHECK, and should. A method can publish a PATCH that only
// accepts SOME of the resource's fields, and Discovery says so only in prose.
// Read from the pinned documents on 2026-09-23, 3 of the 153 updatable types
// that could be matched to their patch method carry such a restriction:
//
//	compute.networks.patch     "Only routingConfig can be modified."
//	compute.subnetworks.patch  "Only certain fields can be updated ... You must
//	                            specify the current fingerprint"
//	compute.images.patch       "Only the following fields can be modified:
//	                            family, description, deprecation status."
//
// gcp.network declares five settable non-ForceNew attributes and only one of
// them is patchable; gcp.subnetwork declares eighteen and we send no
// fingerprint. For those three, deriving a verb trades a REPLACEMENT — which is
// destructive but converges — for a patch the API may ignore, which does not
// converge and shows as drift nobody caused. The right answer is per-type: mark
// the fields Google will not patch as ForceNew in gen/overlay.yaml, so each
// field gets the behaviour the API actually gives it. Not done yet.
func discoveredUpdate(col disco.Collection, updateURL string) (verb string, masked bool) {
	patch := col.Methods["patch"]
	get := col.Methods["get"]
	if patch == nil || get == nil || patch.HTTPMethod != "PATCH" {
		return "", false
	}
	// Where the update will actually be SENT has to be where the patch method
	// lives. With no update_url that is self_link, which is the get path, so
	// the two paths must agree. With one, magic-modules has named the address
	// itself and it is checked against the method directly -- which is the only
	// way compute's autoscalers can ever be updatable, since their patch is
	// published on the COLLECTION and names the resource in a query parameter,
	// so its path never equals the get's.
	if updateURL == "" {
		if !sameAddress(patch, get) {
			return "", false
		}
	} else if !updateURLMatchesPatch(updateURL, patch) {
		return "", false
	}
	if patch.Request == nil || get.Response == nil {
		return "", false
	}
	// A request that is not the resource is the envelope shape, handled by
	// discoveredUpdateWrapper and admitted only when it resolves cleanly. This
	// function answers for the bare-body case only.
	if patch.Request.Ref != get.Response.Ref {
		return "", false
	}
	if p := patch.Parameters["updateMask"]; p != nil && p.Location == "query" {
		masked = true
	}
	return "PATCH", masked
}

// isBareCapture reports whether a url template names no literal segment at all
// -- "{+name}", "{+sinkName}" -- and so says nothing about the id's shape.
func isBareCapture(tmpl string) bool {
	phs := templatePlaceholders(tmpl)
	return len(phs) == 1 && phs[0].Reserved && tmpl == "{+"+phs[0].Name+"}"
}

// cleanIDPatternRE matches a path parameter pattern that is nothing but
// literal/[^/]+ pairs: "^projects/[^/]+/instances/[^/]+/appProfiles/[^/]+$".
// Anything else -- a generic parent ("^[^/]+/[^/]+/feeds/[^/]+$"), a
// multi-segment tail ("^tagBindings/.*$") -- is not a shape a template can
// name, and is left as the capture.
var cleanIDPatternRE = regexp.MustCompile(`^\^(?:[A-Za-z][A-Za-z0-9-]*/\[\^/\]\+/?)+\$$`)

// structuredIDTemplate is the id shape a bare-capture self_link stands for,
// read from the collection's own get (or, failing that, delete) method: the
// pattern Discovery publishes for that capture's path parameter, with each
// [^/]+ named after the collection in front of it. "" when there is no such
// pattern or it is not a clean chain, and when two segments would share a name
// -- a template with a repeated placeholder is not one anyone could fill.
func structuredIDTemplate(col disco.Collection, selfLink string) string {
	phs := templatePlaceholders(selfLink)
	if len(phs) != 1 {
		return ""
	}
	name := phs[0].Name
	for _, mn := range []string{"get", "delete"} {
		m := col.Methods[mn]
		if m == nil || !strings.HasSuffix(m.Path, "{+"+name+"}") {
			continue
		}
		p := m.Parameters[name]
		if p == nil || p.Location != "path" || !cleanIDPatternRE.MatchString(p.Pattern) {
			return ""
		}
		segs := strings.Split(strings.ReplaceAll(strings.Trim(p.Pattern, "^$"), "[^/]+", "*"), "/")
		seen := map[string]bool{}
		var out []string
		for i := 0; i+1 < len(segs); i += 2 {
			n := singularSegment(segs[i])
			if seen[n] {
				return ""
			}
			seen[n] = true
			out = append(out, segs[i], "{"+n+"}")
		}
		return strings.Join(out, "/")
	}
	return ""
}

// immutableResourcePatchFields names the top-level fields of an immutable
// magic-modules resource that it updates with a PATCH to the resource itself:
// the only fields such a resource changes in place through the update this
// provider sends. A field updated through its own method (setUrlMap,
// setLabels) is not one of them, and neither is NOOP, which magic-modules
// uses for a field another field's update carries.
func immutableResourcePatchFields(mm *mmv1.Resource) []string {
	var out []string
	for _, f := range mm.Properties {
		if !strings.EqualFold(f.UpdateVerb, http.MethodPatch) || !addressesTheResource(f.UpdateURL) {
			continue
		}
		name := f.Name
		if f.ApiName != "" {
			name = f.ApiName
		}
		out = append(out, name)
	}
	return out
}

// addressesTheResource reports whether an update_url names the resource
// itself rather than one of its methods: its last segment is a placeholder
// ("{{name}}"), not a literal verb ("setUrlMap", "updateShieldedInstanceConfig").
func addressesTheResource(tmpl string) bool {
	if i := strings.IndexByte(tmpl, '?'); i >= 0 {
		tmpl = tmpl[:i]
	}
	last := tmpl[strings.LastIndexByte(tmpl, '/')+1:]
	return strings.HasPrefix(last, "{{") && strings.HasSuffix(last, "}}") && !strings.Contains(last, ":")
}

// preferExactCollection replaces a name match with the resource whose
// base_url walks exactly this collection's path, when the name match's does
// not. Secret Manager's two collections, projects.secrets and
// projects.locations.secrets, share a create path ("v1/{+parent}/secrets")
// and the name Secret, so matchResource paired both with the global Secret;
// the regional one is magic-modules' RegionalSecret, in another product,
// whose base_url is projects/{{project}}/locations/{{location}}/secrets.
//
// Only a resource whose name ENDS in the collection's singular is eligible
// ("RegionalSecret" for "secrets"), so this chooses between candidates for
// the same thing and never pairs a collection with something unrelated that
// happens to live at the same depth.
func preferExactCollection(mms []*mmv1.Resource, colPath []string, matched *mmv1.Resource) *mmv1.Resource {
	// A base_url that starts with a placeholder ("{{parent}}/locations/...")
	// can stand for any path, this collection's included, so a match on one
	// is not displaced: measured, the only such displacement swapped
	// networksecurity's AddressGroup for ProjectAddressGroup, a stub that
	// "Only used to generate IAM resources".
	if matched == nil || len(colPath) == 0 || sameLiterals(matched.BaseURL, colPath) ||
		strings.HasPrefix(strings.TrimPrefix(matched.BaseURL, "/"), "{{") {
		return matched
	}
	want := strings.ToLower(singular(colPath[len(colPath)-1]))
	var exact *mmv1.Resource
	for _, mm := range mms {
		if strings.HasSuffix(strings.ToLower(mm.Name), want) && sameLiterals(mm.BaseURL, colPath) {
			if exact != nil {
				return matched // two candidates: not a choice to make by guessing
			}
			exact = mm
		}
	}
	if exact == nil {
		return matched
	}
	return exact
}

// sameLiterals reports whether a url template's literal segments are exactly
// the collection path's words, in order: "projects/{{project}}/locations/
// {{location}}/secrets" against [projects locations secrets].
func sameLiterals(tmpl string, colPath []string) bool {
	if i := strings.IndexByte(tmpl, '?'); i >= 0 {
		tmpl = tmpl[:i]
	}
	var lits []string
	for _, seg := range strings.Split(strings.Trim(tmpl, "/"), "/") {
		if seg != "" && !strings.Contains(seg, "{") {
			lits = append(lits, seg)
		}
	}
	if len(lits) != len(colPath) {
		return false
	}
	for i := range lits {
		if lits[i] != colPath[i] {
			return false
		}
	}
	return true
}

// endpointTemplate is the host a type must be reached through when magic-
// modules puts it on a location-specific one, as regional secrets are, or
// "". Taken from the product's base_url with the api version (PathPrefix)
// removed, and admitted only when it reproduces EVERY endpoint the Discovery
// document lists: a template that got one location wrong would send that
// location's requests to a host that does not serve them.
//
// Only for a type whose self_link names a location, since the runtime fills
// the template from the resource's own path.
func endpointTemplate(doc *disco.Document, mm *mmv1.Resource, t *catalog.Type) string {
	if mm == nil || !strings.Contains(mm.ProductBaseURL, "{{location}}") || len(doc.Endpoints) == 0 ||
		!strings.Contains(t.SelfLink, "locations/") {
		return ""
	}
	tmpl := strings.ReplaceAll(strings.TrimSuffix(mm.ProductBaseURL, t.PathPrefix), "{{location}}", "{location}")
	if !strings.HasSuffix(tmpl, "/") || strings.Contains(tmpl, "{{") {
		return ""
	}
	for _, e := range doc.Endpoints {
		if strings.ReplaceAll(tmpl, "{location}", e.Location) != e.EndpointURL {
			return ""
		}
	}
	return tmpl
}

// bareMMPlaceholderRE is a magic-modules self_link that is one placeholder
// and at most a version segment: "v3/{{name}}", "{{name}}".
var bareMMPlaceholderRE = regexp.MustCompile(`^(?:v[0-9][0-9a-z]*/)?\{\{[a-z_]+\}\}$`)

// versionPrefixOf is a path's leading version segment with its slash
// ("v3/"), or "".
func versionPrefixOf(path string) string {
	if m := regexp.MustCompile(`^v[0-9][0-9a-z]*/`).FindString(path); m != "" {
		return m
	}
	return ""
}

// dropUnfillableQuery removes a query parameter whose value is a placeholder
// nothing this provider holds can fill: not an attribute, in any spelling,
// and not a placeholder of the resource's own id. magic-modules' dataset
// delete_url ends "?deleteContents={{delete_contents_on_destroy}}", a
// Terraform-only field, and every delete of every dataset failed building
// its url. Without the parameter the API's default applies.
func dropUnfillableQuery(tmpl string, t *catalog.Type) string {
	path, query, ok := strings.Cut(tmpl, "?")
	if !ok {
		return tmpl
	}
	known := map[string]bool{"project": true, "region": true, "zone": true, "location": true}
	for p := range templatePlaceholderNames(t.SelfLink) {
		known[p] = true
	}
	for name, a := range t.Attributes {
		known[name], known[a.Canonical], known[snake(name)] = true, true, true
		for _, al := range a.Aliases {
			known[al] = true
		}
	}
	var kept []string
	for _, kv := range strings.Split(query, "&") {
		_, v, _ := strings.Cut(kv, "=")
		if m := placeholderShapeRE.FindString(v); m != "" && m == v {
			if !known[strings.Trim(m, "{}+%")] {
				continue
			}
		}
		kept = append(kept, kv)
	}
	if len(kept) == 0 {
		return path
	}
	return path + "?" + strings.Join(kept, "&")
}

// templatePlaceholderNames are a template's placeholder names, bare.
func templatePlaceholderNames(tmpl string) map[string]bool {
	out := map[string]bool{}
	for _, m := range placeholderShapeRE.FindAllString(tmpl, -1) {
		out[strings.Trim(m, "{}+%")] = true
	}
	return out
}

// withoutRepeatedTail removes from a binding the trailing segments the url
// itself repeats right after the placeholder. Discovery's pattern for a
// networksecurity address group's parent is projects/*/locations/*, and
// magic-modules' url is "{{parent}}/locations/{{location}}/addressGroups",
// so the bound url read projects/p/locations/r/locations/r/addressGroups:
// every create of six types went to an address that does not exist.
// Segments compare by shape, so {location} matches {{location}}.
func withoutRepeatedTail(binding, url, placeholder string) string {
	url, _, _ = strings.Cut(url, "?")
	at := -1
	for _, spelling := range []string{"{{" + placeholder + "}}", "{+" + placeholder + "}", "{" + placeholder + "}"} {
		if i := strings.Index(url, spelling); i >= 0 {
			at = i + len(spelling)
			break
		}
	}
	if at < 0 {
		return binding
	}
	after := strings.Split(strings.Trim(url[at:], "/"), "/")
	bind := strings.Split(binding, "/")
	for k := len(bind) - 1; k > 0; k-- {
		if k > len(after) {
			continue
		}
		same := true
		for i := 0; i < k; i++ {
			if normalizeTemplateShape(bind[len(bind)-k+i]) != normalizeTemplateShape(after[i]) {
				same = false
				break
			}
		}
		if same {
			return strings.Join(bind[:len(bind)-k], "/")
		}
	}
	return binding
}

// dropUnpublishedCreateQuery removes create-url query parameters the create
// method does not publish. compute's network edge security service was
// created at "?networkEdgeSecurityService={{name}}", a parameter compute's
// insert does not take; the runtime keeps a url-named attribute out of the
// body, so the insert went out with no name at all. Dropped, the name is in
// the body where compute reads it. Compared ignoring case and underscores,
// because magic-modules writes repository_id for Discovery's repositoryId.
func dropUnpublishedCreateQuery(t *catalog.Type, create *disco.Method) {
	path, query, ok := strings.Cut(t.CreateURL, "?")
	if !ok || create == nil {
		return
	}
	published := map[string]bool{}
	for p := range create.Parameters {
		published[strings.ToLower(strings.ReplaceAll(p, "_", ""))] = true
	}
	var kept []string
	for _, kv := range strings.Split(query, "&") {
		k, _, _ := strings.Cut(kv, "=")
		if published[strings.ToLower(strings.ReplaceAll(k, "_", ""))] {
			kept = append(kept, kv)
		}
	}
	if len(kept) == 0 {
		t.CreateURL = path
		return
	}
	t.CreateURL = path + "?" + strings.Join(kept, "&")
}

// applySensitive marks the overlay's secrets. An entry for a type that does
// not ship, or a path it does not have, is an error: a secret listed and
// silently not marked is the leak this exists to stop.
func applySensitive(types []*catalog.Type, entries map[string]SensitiveFields) error {
	byName := make(map[string]*catalog.Type, len(types))
	for _, t := range types {
		byName[t.Name] = t
	}
	names := make([]string, 0, len(entries))
	for n := range entries {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		t := byName[n]
		if t == nil {
			return fmt.Errorf("overlay sensitive: %s does not ship", n)
		}
		if strings.TrimSpace(entries[n].Note) == "" {
			return fmt.Errorf("overlay sensitive: %s has no note saying why", n)
		}
		for _, path := range entries[n].Fields {
			a := attrAtPath(t.Attributes, path)
			if a == nil {
				return fmt.Errorf("overlay sensitive: %s has no attribute %q", n, path)
			}
			a.Sensitive = true
		}
	}
	return nil
}

// attrAtPath walks "a.b[].c" through Fields and Elem.
func attrAtPath(attrs map[string]*catalog.Attr, path string) *catalog.Attr {
	var a *catalog.Attr
	fields := attrs
	for _, seg := range strings.Split(path, ".") {
		elem := strings.HasSuffix(seg, "[]")
		seg = strings.TrimSuffix(seg, "[]")
		if a = fields[seg]; a == nil {
			return nil
		}
		if elem {
			if a.Elem == nil {
				return nil
			}
			a = a.Elem
		}
		fields = a.Fields
	}
	return a
}
