package gcprov

import (
	"strconv"
	"strings"
	"time"

	"github.com/infrena/infrena-provider-gcp/internal/catalog"
	"github.com/infrena/infrena/pkg/value"
)

// reservedLabelPrefix is the label namespace GCP keeps for itself:
// goog-dm (Deployment Manager), goog-gke-node, goog-managed-by, and the rest.
// A user cannot set or remove one, so reporting one as state makes every plan
// propose deleting something nothing can delete -- the same reason the AWS
// provider never reports aws:-prefixed tags.
const reservedLabelPrefix = "goog-"

// Reconcile makes GCP's answer comparable to what we asked for.
//
// GCP returns MORE than it was sent: fields it defaulted, fields it computed,
// fields it reordered. Compared raw against the reference, every one of those
// reads as drift, and the plan proposes a change that applying cannot fix --
// because the next read returns the same extras again. That is a plan that
// never converges, which is the single worst failure this provider can have.
//
// reference is what the user asked for (the desired or last-known value).
// incoming is what GCP just returned. The result is incoming, expressed the way
// reference is, so value.Equal between them means "no drift" and nothing else.
//
// BOTH SIDES ARE KEYED BY SCHEMA NAME. schemaAttrs (names.go) has already
// translated the response body out of GCP's spelling by the time anything
// here sees it, so this looks fields up by the same key configuration uses
// and never by Canonical.
func Reconcile(attr *catalog.Attr, reference, incoming value.Value) value.Value {
	return reconciler{}.value(attr, reference, incoming)
}

// reconciler carries the facts reconciliation needs that are not in the
// catalog: today, the two spellings this instance's project answers to.
//
// A struct rather than another parameter on five functions, because the
// recursion runs through Fields and Elem at every depth and a project
// reference can sit at any of them -- gcp.tagkey's `parent` is top-level,
// but nothing says the next one will be.
type reconciler struct {
	// aliases resolves the two spellings this instance's project answers to,
	// lazily: it is nil for the bare package-level Reconcile, and calling it
	// can cost a request, so it is only called once a value is already known
	// to be a project resource name that disagrees.
	aliases func() ProjectAliases
	// unmatched is true inside the elements of an UNORDERED list. There the
	// reference element an incoming element is reconciled against is chosen by
	// position before the list is matched and reordered, so it may be a
	// sibling. Anything that COPIES from the reference -- carrying an
	// input-only field forward -- must not trust it. Nothing that reads the
	// reference only to decide spelling or order is affected.
	//
	// The zero value is the safe, ordinary case on purpose: the reconciler
	// is constructed in more than one place (projects.go builds the one the
	// runtime uses), and a flag every constructor had to remember to set is
	// a flag that is off in production and on in the tests.
	unmatched bool
}

func (r reconciler) value(attr *catalog.Attr, reference, incoming value.Value) value.Value {
	if attr == nil || !incoming.Known {
		return incoming
	}

	// OPAQUE IS COPIED EXACTLY. A free-form map (additionalProperties with no
	// declared properties) or a $ref tail the resolver truncated has no schema
	// to reconcile against, so every key is equally unknown to us. Pruning the
	// ones the reference happens not to mention would silently delete what the
	// user wrote. Copy it and move on.
	//
	// Reserved labels are the one thing GCP adds to an opaque map that we know
	// is not the user's, and they are dropped in a separate pass rather than
	// here -- see withoutReservedLabels, and ReconcileAttrs, which runs both.
	if attr.Opaque {
		return incoming
	}

	switch {
	case attr.Kind == value.KindMap && len(attr.Fields) > 0:
		return r.object(attr, reference, incoming)
	case attr.Kind == value.KindList && attr.Elem != nil:
		return r.list(attr, reference, incoming)
	default:
		return sameByEquivalence(attr, reference, r.sameProjectSpelling(reference, asDeclaredKind(attr, incoming)))
	}
}

// sameByEquivalence returns the reference's spelling when the attribute's
// Equivalence says it and Google's answer are the same value written two
// ways. The same job sameProjectSpelling does for projects, for the rules
// the catalog names: see catalog.Attr.Equivalence.
func sameByEquivalence(attr *catalog.Attr, reference, incoming value.Value) value.Value {
	if attr.Equivalence == "" || reference.Kind != value.KindString || incoming.Kind != value.KindString || !reference.Known {
		return incoming
	}
	want, _ := reference.Raw.(string)
	got, _ := incoming.Raw.(string)
	if want == got {
		return incoming
	}
	if !equivalent(attr.Equivalence, want, got) {
		return incoming
	}
	return value.Value{Kind: value.KindString, Known: true, Raw: want, Source: incoming.Source}
}

// equivalent reports whether want and got are one value under the named rule.
// An unknown rule makes nothing equivalent, which is the behaviour without one.
func equivalent(rule, want, got string) bool {
	switch rule {
	case catalog.EquivalencePortRange:
		return portRange(want) != "" && portRange(want) == portRange(got)
	case catalog.EquivalenceSelfLink:
		if !strings.Contains(want, "/") || !strings.Contains(got, "/") {
			// A bare name against a path: the path must end in it.
			return lastSegment(want) == lastSegment(got) && want != "" && got != ""
		}
		w, g := fromProjects(want), fromProjects(got)
		return w != "" && w == g
	case catalog.EquivalenceResourceName:
		return want != "" && lastSegment(want) == lastSegment(got)
	case catalog.EquivalenceCase:
		return strings.EqualFold(want, got)
	case catalog.EquivalenceKMSKey:
		// Only the version Google appended is dropped: a configuration that
		// names a version means that version.
		if withoutKeyVersion(want) != want {
			return equivalent(catalog.EquivalenceSelfLink, want, got)
		}
		return equivalent(catalog.EquivalenceSelfLink, want, withoutKeyVersion(got))
	case catalog.EquivalenceImage:
		if equivalent(catalog.EquivalenceSelfLink, want, got) {
			return true
		}
		w, g := fromProjects(want), fromProjects(got)
		prefix, family, ok := strings.Cut(w, "/images/family/")
		if !ok || w == "" || !strings.HasPrefix(g, prefix+"/images/") {
			return false
		}
		image := lastSegment(g)
		return image == family || strings.HasPrefix(image, family+"-")
	case catalog.EquivalenceDuration:
		w, errW := time.ParseDuration(want)
		g, errG := time.ParseDuration(got)
		return errW == nil && errG == nil && w == g
	}
	return false
}

// withoutKeyVersion drops a trailing "/cryptoKeyVersions/<n>".
func withoutKeyVersion(s string) string {
	if i := strings.Index(s, "/cryptoKeyVersions/"); i >= 0 && !strings.Contains(s[i+len("/cryptoKeyVersions/"):], "/") {
		return s[:i]
	}
	return s
}

// fromProjects is a resource path from its "projects/" segment on, which is
// what a full url, a versioned path and a relative name all share, or ""
// when there is none.
func fromProjects(s string) string {
	if strings.HasPrefix(s, "projects/") {
		return s
	}
	if i := strings.Index(s, "/projects/"); i >= 0 {
		return s[i+1:]
	}
	return ""
}

func lastSegment(s string) string {
	return s[strings.LastIndexByte(s, '/')+1:]
}

// portRange is a port or port range in its "low-high" form, or "" when s is
// neither.
func portRange(s string) string {
	lo, hi, isRange := strings.Cut(s, "-")
	if !isRange {
		hi = lo
	}
	if _, err := strconv.Atoi(lo); err != nil {
		return ""
	}
	if _, err := strconv.Atoi(hi); err != nil {
		return ""
	}
	return lo + "-" + hi
}

// sameProjectSpelling returns the REFERENCE's spelling of a project
// reference when GCP answered with the other one.
//
// GCP canonicalises a project reference to the project NUMBER across many
// APIs. A configuration says `parent: projects/example-project-1234` and
// Cloud Resource Manager answers `parent: "projects/123456789012"` -- the
// same project, in canonical form, as a different string. On gcp.tagkey
// `parent` is ForceNew, so every plan after a successful apply proposed
// destroying and recreating the tag key, forever. Task 18a's live re-run
// found it, and only after its own defect C was fixed: until then the create
// failed and there was no second plan to look at.
//
// THIS IS THE SAME JOB REORDERING AN UNORDERED LIST DOES. GCP's answer is
// correct and spelled differently from what the configuration wrote, and
// this function exists to express it the way the configuration did -- which
// is what the doc comment on Reconcile says the whole file is for. It is
// NOT a rewrite of the user's configuration, and it is not a lenient id
// parser: both of those were considered and refused. It is the state, and
// the state is what the plan compares.
//
// Nothing happens unless the provider actually resolved the number (see
// ProjectAliases). An unresolved instance leaves the two unequal, and a plan
// that proposes a replacement is a thing a user can see and ask about.
func (r reconciler) sameProjectSpelling(reference, incoming value.Value) value.Value {
	if reference.Kind != value.KindString || incoming.Kind != value.KindString {
		return incoming
	}
	want, _ := reference.Raw.(string)
	got, _ := incoming.Raw.(string)
	if want == got || r.aliases == nil {
		return incoming
	}
	if !looksLikeAProjectName(want) || !looksLikeAProjectName(got) {
		return incoming
	}
	if !r.aliases().sameProject(want, got) {
		return incoming
	}
	return value.Value{Kind: value.KindString, Known: true, Raw: want, Source: incoming.Source}
}

// ReconcileAttrs reconciles a whole response body's worth of values against
// the ones they are going to be compared with, and drops the labels GCP
// reserves for itself. It is what the read path calls; Reconcile is one
// attribute of it.
//
// reference may be nil -- a create's readback has no previous state -- and
// then nothing is reordered, because there is no order to reorder to.
//
// AN UNDECLARED TOP-LEVEL KEY IS KEPT HERE, and dropped where state is
// assembled (declaredOnly): the host refuses a state carrying one and fails
// the whole operation, which a field Google added after the pinned Discovery
// document did to a live Spanner create (2026-09-25). Kept this far so the
// id is still read from the full answer.
func ReconcileAttrs(attrs map[string]*catalog.Attr, reference, incoming map[string]value.Value) map[string]value.Value {
	return reconciler{}.attrs(attrs, reference, incoming)
}

func (r reconciler) attrs(attrs map[string]*catalog.Attr, reference, incoming map[string]value.Value) map[string]value.Value {
	out := make(map[string]value.Value, len(incoming))
	for name, v := range incoming {
		a := attrs[name]
		out[name] = withoutReservedLabels(a, r.value(a, reference[name], v))
	}
	for name, a := range attrs {
		if carried, ok := r.carryInputOnly(a, reference[name], incoming, name); ok {
			out[name] = carried
		} else if carried, ok := r.carryOmittedZero(reference[name], incoming, name); ok {
			out[name] = carried
		}
	}
	return out
}

// carryOmittedZero reports a configured zero value -- false, 0, "", an empty
// list or map -- that the answer left out, as configured. proto3 JSON never
// writes a field at its default, so Google's answer to `enable: false` has no
// `enable` at all, and reported missing it was drift on every plan against a
// resource exactly as configured (a replacement where the field is
// immutable). alloydb's Instance decoder exists only to put these back.
//
// Only a zero: a configured true that comes back absent is a real
// difference. Not inside an unordered list, where the reference element may
// be a sibling.
func (r reconciler) carryOmittedZero(ref value.Value, in map[string]value.Value, name string) (value.Value, bool) {
	if r.unmatched || !isZero(ref) {
		return value.Value{}, false
	}
	if _, answered := in[name]; answered {
		return value.Value{}, false
	}
	return ref, true
}

func isZero(v value.Value) bool {
	if !v.Known {
		return false
	}
	switch raw := v.Raw.(type) {
	case bool:
		return !raw
	case int64:
		return raw == 0
	case float64:
		return raw == 0
	case string:
		return raw == ""
	case []value.Value:
		return len(raw) == 0
	case map[string]value.Value:
		return len(raw) == 0
	}
	return false
}

// carryInputOnly is the whole of input-only handling: a field the API never
// returns is reported as the reference holds it, because there is nothing of
// GCP's to compare it against. Reported missing instead, it is drift on every
// plan against a resource that is exactly as configured -- the compute
// instance that proposed replacing itself for ever was this, on
// disks[].initializeParams.
//
// Only when the API really did leave it out: an answer that carries the field
// is believed, the same rule CreateOnly follows. And only a KNOWN reference
// value is carried; an unknown one would be a claim about something nobody
// has resolved.
func (r reconciler) carryInputOnly(a *catalog.Attr, ref value.Value, in map[string]value.Value, name string) (value.Value, bool) {
	if a == nil || !a.InputOnly || r.unmatched || !ref.Known {
		return value.Value{}, false
	}
	if _, answered := in[name]; answered {
		return value.Value{}, false
	}
	return ref, true
}

// reconcileObject drops keys the schema does not declare and recurses into the
// ones it does.
//
// An undeclared key is one GCP added and we never modelled -- a fingerprint, an
// etag, a server-assigned id. Keeping it means comparing it, and comparing it
// means drift forever.
func (r reconciler) object(attr *catalog.Attr, reference, incoming value.Value) value.Value {
	in, ok := incoming.Raw.(map[string]value.Value)
	if !ok {
		// Kind and Raw disagree; there is no object here to walk. Copying is
		// the honest answer, the same one wireValue gives for the same case.
		return incoming
	}
	ref, hasRef := reference.Raw.(map[string]value.Value)
	hasRef = hasRef && reference.Known
	out := make(map[string]value.Value, len(in))
	for name, field := range attr.Fields {
		v, found := in[name]
		if !found {
			if carried, ok := r.carryInputOnly(field, ref[name], in, name); ok {
				out[name] = carried
			} else if carried, ok := r.carryOmittedZero(ref[name], in, name); ok && hasRef {
				out[name] = carried
			}
			continue
		}
		// A DECLARED nested field the reference never set is the server's
		// choice, and it is dropped the same as an undeclared one.
		//
		// infrena's planner forgives a key configuration does not mention only
		// at the top level, where it can ask the schema whether the attribute
		// is computed. Inside a composite it has no per-leaf schema, so it
		// forgives nothing but an empty collection -- and a real compute disk
		// comes back with deviceName, source, mode, interface and type filled
		// in, which made every instance plan its own replacement. This is the
		// nested form of infrena's own top-level rule: when configuration sets
		// no value, the provider's choice is authoritative.
		//
		// Only against a real reference object. With none -- an import, a
		// discovery -- there is nothing to say what was asked for, so
		// everything is reported. And never inside an unordered list, where the
		// reference element was picked by position and may be a sibling.
		//
		// The cost, accepted on 2026-09-24: an out-of-band change to a nested
		// field nobody configured is no longer drift. One somebody configured
		// still is.
		if hasRef && !r.unmatched {
			if _, asked := ref[name]; !asked {
				continue
			}
		}
		// An output-only field can never be in configuration, so inside
		// an element that answers a configured one it is always drift, even
		// in an unordered list where the pruning above is off (a sibling
		// picked by position still holds only what configuration can write).
		// Cloud DNS answers every network of a policy with kind
		// "dns#policyNetwork", and the plan after each create proposed an
		// update (live run, 2026-09-24).
		if hasRef && r.unmatched && field.Output {
			continue
		}
		out[name] = r.value(field, ref[name], v)
	}
	return value.Value{Kind: value.KindMap, Known: true, Raw: out, Source: incoming.Source}
}

// reconcileList reorders an UNORDERED list to match the reference, and leaves an
// ordered one strictly alone.
//
// GCP reorders lists it does not consider ordered, so a diff on order alone
// plans a change forever. But reordering a list where order carries meaning --
// a rule evaluation sequence, a priority list -- would silently rewrite the
// user's intent. The catalog records which is which; never guess.
func (r reconciler) list(attr *catalog.Attr, reference, incoming value.Value) value.Value {
	in, ok := incoming.Raw.([]value.Value)
	if !ok {
		return incoming
	}
	ref, _ := reference.Raw.([]value.Value)

	items := make([]value.Value, len(in))
	for i, item := range in {
		// Positionally against the reference, which is exact for an ordered
		// list and approximate for an unordered one -- there the elements are
		// matched below, after each has been reconciled. Everything the
		// element-level pass does except reordering a list nested inside an
		// element is independent of which reference element it is handed.
		var refItem value.Value
		if i < len(ref) {
			refItem = ref[i]
		}
		elem := r
		elem.unmatched = r.unmatched || attr.Unordered
		items[i] = elem.value(attr.Elem, refItem, item)
	}
	if !attr.Unordered {
		return value.Value{Kind: value.KindList, Known: true, Raw: items, Source: incoming.Source}
	}

	// Unordered: emit the reference's order for everything both sides hold,
	// then whatever GCP added, so an addition still shows as drift while a pure
	// reordering does not.
	ordered := make([]value.Value, 0, len(items))
	used := make([]bool, len(items))
	for _, want := range ref {
		for i, got := range items {
			if !used[i] && answersTo(want, got) {
				ordered = append(ordered, got)
				used[i] = true
				break
			}
		}
	}
	for i, got := range items {
		if !used[i] {
			ordered = append(ordered, got)
		}
	}
	return value.Value{Kind: value.KindList, Known: true, Raw: ordered, Source: incoming.Source}
}

// answersTo reports whether got is the element want names -- used ONLY to
// pair an unordered list's elements up with the reference's, never to decide
// whether anything changed.
//
// Equality is too strict for that job. An element of a set-typed list is
// routinely an object GCP fills fields into: gcp.packetmirroring's
// mirroredResources.subnetworks is sent as {url} and comes back as
// {url, canonicalUrl}, and canonicalUrl is a DECLARED attribute (output-only),
// so reconcileObject rightly keeps it. Under value.Equal -- which compares
// maps by length before content -- no element would ever match its own
// reference, the reordering would silently not happen, and the plan for a
// resource nobody touched would propose reordering that list forever.
//
// So: an element answers to the reference when it agrees with everything the
// reference actually says, and is free to carry more. Pairing the wrong two
// elements costs an order that still does not match, which is the same thing
// not pairing them costs; it can never change a value, because only the
// ORDER of already-reconciled elements is decided here.
func answersTo(want, got value.Value) bool {
	switch {
	case want.Kind == value.KindMap && got.Kind == value.KindMap:
		wf, wok := want.Raw.(map[string]value.Value)
		gf, gok := got.Raw.(map[string]value.Value)
		if !wok || !gok {
			return want.Equal(got)
		}
		for k, w := range wf {
			g, present := gf[k]
			if !present || !answersTo(w, g) {
				return false
			}
		}
		return true
	case want.Kind == value.KindList && got.Kind == value.KindList:
		wl, wok := want.Raw.([]value.Value)
		gl, gok := got.Raw.([]value.Value)
		if !wok || !gok || len(wl) != len(gl) {
			return want.Equal(got)
		}
		for i := range wl {
			if !answersTo(wl[i], gl[i]) {
				return false
			}
		}
		return true
	default:
		return want.Equal(got)
	}
}

// asDeclaredKind retypes a numeric leaf to the Kind the catalog declares for
// it, when the two disagree and the conversion is exact.
//
// JSON has one number type, so a response carrying 1 and a response carrying
// 1.0 are the same bytes, and fromRaw (ids.go) types a whole number as
// KindInt -- correct for the great majority of attributes and wrong for the
// 134 the catalog declares as KindFloat (28 types, 60 of them inside a list,
// at depths up to 9; measured 2026-09-22 following Fields AND Elem).
// Value.Equal compares Kind BEFORE Raw, so a KindInt 1 never equals the
// configured KindFloat 1, and that attribute patches on every update forever
// however little the user changes.
//
// value.Coerce is the exactness rule, not a cast: it converts only across a
// numeric pair, refuses a fractional part and refuses an int64 too large for
// a float64 to hold. Anything it refuses is left exactly as it arrived, which
// is a visible difference rather than a silently rounded value.
func asDeclaredKind(attr *catalog.Attr, incoming value.Value) value.Value {
	if attr.Kind == incoming.Kind {
		return incoming
	}
	if out, ok := value.Coerce(incoming, attr.Kind); ok {
		return out
	}
	return incoming
}

// withoutReservedLabels returns v with GCP's own labels dropped, at every
// depth a declared attribute reaches -- through Fields AND through Elem,
// because a label map inside a list element (gcp.automation's
// selector.targets[].labels) is reachable only through the second.
//
// It is a separate pass from Reconcile rather than a case inside it because
// the two rules genuinely disagree about the same value: a labels map is
// Opaque, and Opaque means copied exactly. Copying a goog- label into state
// costs a plan that proposes removing it, GCP putting it straight back, and a
// plan that never converges; 123 of the catalog's 124 label attributes are
// opaque maps, so the opaque rule alone would leave every one of them in that
// state.
//
// Only a map called "labels" is filtered. The exception proves why the test
// is not on the name alone: gcp.bigquery.table's model.modelOptions.labels is
// a LIST of strings, nothing to do with resource labels, and a filter keyed on
// the name by itself would reach into it.
func withoutReservedLabels(attr *catalog.Attr, v value.Value) value.Value {
	if attr == nil || !v.Known {
		return v
	}
	if isLabelMap(attr) {
		in, ok := v.Raw.(map[string]value.Value)
		if !ok {
			return v
		}
		out := make(map[string]value.Value, len(in))
		for k, item := range in {
			if strings.HasPrefix(k, reservedLabelPrefix) {
				continue
			}
			out[k] = item
		}
		return value.Value{Kind: v.Kind, Known: true, Raw: out, Source: v.Source}
	}
	switch {
	case len(attr.Fields) > 0:
		in, ok := v.Raw.(map[string]value.Value)
		if !ok {
			return v
		}
		out := make(map[string]value.Value, len(in))
		for k, item := range in {
			out[k] = withoutReservedLabels(attr.Fields[k], item)
		}
		return value.Value{Kind: v.Kind, Known: true, Raw: out, Source: v.Source}
	case attr.Elem != nil:
		in, ok := v.Raw.([]value.Value)
		if !ok {
			return v
		}
		out := make([]value.Value, len(in))
		for i, item := range in {
			out[i] = withoutReservedLabels(attr.Elem, item)
		}
		return value.Value{Kind: v.Kind, Known: true, Raw: out, Source: v.Source}
	}
	return v
}

// isLabelMap reports whether attr is a resource's labels: the free-form map
// GCP lets a user hang key/value pairs on, and the only place the goog-
// prefix means "GCP put this here".
func isLabelMap(attr *catalog.Attr) bool {
	return attr.Canonical == "labels" && attr.Kind == value.KindMap
}

// LabelsIn converts GCP's labels into the map infrena compares, dropping the
// ones GCP reserves for itself.
//
// goog-prefixed labels are applied by GCP (goog-dm, goog-gke-node,
// goog-managed-by) and cannot be removed by a user. Reporting one makes every
// plan propose deleting it, forever -- the same reason the AWS provider never
// reports aws:-prefixed tags.
//
// A value that is not a known map, and an entry that is not a known string,
// is left out rather than guessed at: labels are strings on the wire, and a
// label whose value we had to invent would be a difference the user never
// wrote.
func LabelsIn(v value.Value) map[string]string {
	entries, ok := v.Raw.(map[string]value.Value)
	if !v.Known || !ok {
		return nil
	}
	out := make(map[string]string, len(entries))
	for k, item := range entries {
		if strings.HasPrefix(k, reservedLabelPrefix) {
			continue
		}
		s, ok := item.AsString()
		if !ok {
			continue
		}
		out[k] = s
	}
	return out
}

// LabelsOut renders a label map for the wire. An EMPTY map is omitted entirely
// rather than sent as {}: some APIs read an explicit empty object as "remove
// every label", which is a destructive reading of "this resource has none".
//
// "Omitted" is the zero Value -- not Known, so a caller that checks Known
// before writing the key sends nothing at all. There is no way to say
// "absent" in a value.Value otherwise, and returning a known empty map here
// would put the destructive {} on the wire.
func LabelsOut(m map[string]string) value.Value {
	if len(m) == 0 {
		return value.Value{}
	}
	entries := make(map[string]value.Value, len(m))
	for k, s := range m {
		entries[k] = value.String(s, value.SourceProvider)
	}
	return value.Map(entries, value.SourceProvider)
}
