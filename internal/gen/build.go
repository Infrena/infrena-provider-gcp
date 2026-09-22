// build.go assembles Tasks 2-7 into the generator: read the inputs, decide
// tiers, name everything, build each type, and hand back the catalog plus
// everything left out of it.
package gen

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/infrena/infrena-provider-gcp/internal/catalog"
	"github.com/infrena/infrena-provider-gcp/internal/disco"
	"github.com/infrena/infrena-provider-gcp/internal/mmv1"
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

// Result is the catalog plus everything left out of it.
type Result struct {
	Catalog  *catalog.Catalog
	Warnings []Warning
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
			mm := matchResource(mms, leaf)
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
	sort.Slice(c.Types, func(i, j int) bool { return c.Types[i].Name < c.Types[j].Name })
	sort.Slice(warnings, func(i, j int) bool {
		if warnings[i].Service != warnings[j].Service {
			return warnings[i].Service < warnings[j].Service
		}
		return warnings[i].Resource < warnings[j].Resource
	})
	return &Result{Catalog: c, Warnings: warnings}, nil
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
func WriteWarnings(path string, ws []Warning) error {
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
func matchResource(mms []*mmv1.Resource, leaf string) *mmv1.Resource {
	want := strings.ToLower(singular(leaf))
	for _, mm := range mms {
		if strings.ToLower(mm.Name) == want {
			return mm
		}
	}
	return nil
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
	body, err := requestBodySchema(doc, create)
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
	if ruling != nil && ruling.AllForceNew {
		forceNewAll(attrs)
	}

	t := &catalog.Type{
		Name:       name,
		Service:    doc.Name,
		APIBaseURL: doc.ResolvedBaseURL(),
		Attributes: attrs,
	}
	if mm != nil {
		t.Description = mm.Description
		t.BaseURL = mm.BaseURL
		t.CreateURL = mm.CreateURL
		t.UpdateURL = mm.UpdateURL
		t.DeleteURL = mm.DeleteURL
		t.SelfLink = mm.SelfLink
		t.UpdateVerb = mm.UpdateVerb
		t.UpdateMask = mm.UpdateMask
		if len(mm.ImportFormat) > 0 {
			t.ImportFormat = strings.Join(mm.ImportFormat, "\n")
		}
	} else {
		t.Description = strings.TrimSpace(body.Description)
	}
	if t.BaseURL == "" {
		// No magic-modules definition, or one with no base_url: fall back to
		// the create method's own path, which for a REST-style insert is the
		// collection path itself.
		t.BaseURL = create.Path
	}
	t.Scope = ScopeOf(t.BaseURL)

	await, _ := AwaitOf(doc, create)
	t.Await = await
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
		// AwaitOf never names an operation scope (it can't: that's an axis of
		// the RESOURCE's own URL, not of the Operation schema it inspects), so
		// it's read off the scope just computed above.
		switch t.Scope {
		case catalog.ScopeGlobal:
			t.OperationScope = "global"
		case catalog.ScopeRegional:
			t.OperationScope = "region"
		case catalog.ScopeZonal:
			t.OperationScope = "zone"
		}
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
	if t.Await == catalog.AwaitLongRunning {
		t.OperationPollPath = operationPollPath(doc)
	}

	if ruling != nil && ruling.ReadVia != "" {
		t.ReadVia = ruling.ReadVia
	}

	t.ListField = ListFieldOf(doc, col)

	return t, nil
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

// operationPollPath finds the API's own operations.get method path, e.g.
// "v1/{+name}".
//
// Measured across the corpus on 2026-09-22: every one of the 97 longrunning
// types' APIs publishes one, in three shapes — v1/{+name} (72), v2/{+name}
// (17), v3/{+name} (8). An empty return therefore means something changed
// upstream, not that this API never had one, and the caller should treat it as
// a type that cannot be awaited rather than guessing a path.
func operationPollPath(doc *disco.Document) string {
	var found string
	var walk func(res map[string]*disco.Resource)
	walk = func(res map[string]*disco.Resource) {
		for name, r := range res {
			if found != "" {
				return
			}
			if name == "operations" {
				if m := r.Methods["get"]; m != nil && m.Path != "" {
					found = m.Path
					return
				}
			}
			walk(r.Resources)
		}
	}
	walk(doc.Resources)
	return found
}
