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

// Build runs the whole generation pass: tier first, over the whole corpus;
// then name every surviving type at once, so uniqueness is judged over the
// whole corpus rather than per API; only then build types, because a
// reference edge cannot be written until its target has a name.
func Build(in Inputs) (*Result, error) {
	overlay, err := LoadOverlay(in.OverlayPath)
	if err != nil {
		return nil, err
	}
	lock, err := LoadLock(in.LockPath)
	if err != nil {
		return nil, err
	}
	byProduct, loadErrs, err := mmv1.LoadDir(in.MMV1Dir)
	if err != nil {
		return nil, err
	}

	docs, err := loadDocs(in.SchemaDir)
	if err != nil {
		return nil, err
	}

	// Pass one: decide what ships, and collect naming candidates.
	type pending struct {
		doc  *disco.Document
		col  disco.Collection
		mm   *mmv1.Resource
		cand Candidate
		dec  Decision
		// rawName is the mm resource name, or (when there is no mm resource)
		// the Discovery collection leaf — the spelling a human reading
		// warnings.txt would recognize, as opposed to cand.Resource, which is
		// lowercased and singularized purely for name assignment. Computed
		// once here so a build failure in pass three names the SAME thing a
		// tier refusal in this same loop would have (see F3: they used to
		// disagree — "Hooked" from a tier refusal, "widget" from a build
		// failure — over a file whose whole job is answering "why is this
		// type unsupported").
		rawName string
	}
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
			shipping = append(shipping, pending{d, col, mm, Candidate{d.Name, strings.ToLower(singular(leaf))}, dec, rawName})
		}
	}

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

// WriteWarnings records every type that did not ship, plus every dropped
// reference on a type that DID ship. Never silently dropped (spec §4.1).
func WriteWarnings(path string, ws []Warning) error {
	var b strings.Builder
	fmt.Fprintf(&b, "# Types this catalog does not serve, and why.\n")
	fmt.Fprintf(&b, "# GENERATED by cmd/gen-gcp. Do not hand-edit.\n#\n")
	fmt.Fprintf(&b, "# tier 1 here means the TYPE shipped fine; one reference on it was\n")
	fmt.Fprintf(&b, "# dropped because its target name was ambiguous across products.\n")
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

	if ruling != nil && ruling.ReadVia != "" {
		t.ReadVia = ruling.ReadVia
	}

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
