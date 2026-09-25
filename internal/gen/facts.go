package gen

import (
	"bytes"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/infrena/infrena-provider-gcp/internal/catalog"
)

// The sources a fact can come from, strongest first. See docs/FACTS.md.
const (
	SourceObserved  = "observed"
	SourceRuling    = "ruling"
	SourceDiscovery = "discovery"
	SourceMM        = "mm"
	SourceDefault   = "default"
)

// A source prefixed with lost said the opposite and was overruled: "!mm" on
// a health check's output is magic-modules saying output, and a ruling
// saying not.
const lost = "!"

// The closed vocabulary. An observation may only name one of these, because
// only these are derived and can be compared.
var (
	// TypeFacts are recorded for EVERY type, with SourceDefault where
	// nothing said: a type fact nobody set is the unknown worth seeing
	// (subnetwork's one-field-per-patch was one).
	TypeFacts = []string{
		"create_verb", "create_url", "create_await",
		"update_verb", "update_mask", "update_await",
		"delete_url", "delete_await",
		"setters", "lock_field", "patch_one_field", "clear_before_delete",
		"timeout",
	}
	// AttrFacts are recorded only where set or where some source spoke.
	AttrFacts = []string{
		"output", "required", "immutable", "input_only", "sensitive",
		"equivalence", "unordered", "send_with_update",
	}
)

// setTypeSource records who decided a type fact. The last decision wins,
// so this replaces rather than appends.
func setTypeSource(t *catalog.Type, fact string, srcs ...string) {
	if t.Sources == nil {
		t.Sources = map[string][]string{}
	}
	t.Sources[fact] = srcs
}

// addSource records that src says fact about a. Sources agreeing
// accumulate; a repeat is not recorded twice.
func addSource(a *catalog.Attr, fact, src string) {
	if a.Sources == nil {
		a.Sources = map[string][]string{}
	}
	for _, s := range a.Sources[fact] {
		if s == src {
			return
		}
	}
	a.Sources[fact] = append(a.Sources[fact], src)
}

// overrule records that winner decided fact against every source that said
// otherwise so far.
func overrule(a *catalog.Attr, fact, winner string) {
	var kept []string
	for _, s := range a.Sources[fact] {
		if !strings.HasPrefix(s, lost) {
			s = lost + s
		}
		kept = append(kept, s)
	}
	if a.Sources == nil {
		a.Sources = map[string][]string{}
	}
	a.Sources[fact] = kept
	addSource(a, fact, winner)
}

// typeFactValue is the catalog's value of one type fact, as a string.
func typeFactValue(t *catalog.Type, fact string) string {
	switch fact {
	case "create_verb":
		if t.CreateVerb == "" {
			return "POST"
		}
		return t.CreateVerb
	case "create_url":
		return t.CreateTemplate()
	case "create_await":
		return awaitName(t.Await)
	case "update_verb":
		if t.UpdateVerb == "" {
			return "none"
		}
		return t.UpdateVerb
	case "update_mask":
		switch {
		case t.UpdateWrapper != "":
			return "body:" + t.UpdateMaskField
		case t.UpdateMask:
			return "query"
		}
		return "none"
	case "update_await":
		if t.UpdateVerb == "" {
			return "none"
		}
		return awaitName(t.UpdateAwaitKind())
	case "delete_url":
		if t.DeleteURL != "" {
			return t.DeleteURL
		}
		return t.SelfLink
	case "delete_await":
		return awaitName(t.DeleteAwaitKind())
	case "setters":
		var parts []string
		for _, s := range t.Setters {
			parts = append(parts, strings.Join(s.Fields, "+")+"="+s.Method)
		}
		sort.Strings(parts)
		if len(parts) == 0 {
			return "none"
		}
		return strings.Join(parts, ",")
	case "lock_field":
		if t.LockField == "" {
			return "none"
		}
		return t.LockField
	case "patch_one_field":
		return strconv.FormatBool(t.PatchOneField)
	case "clear_before_delete":
		if len(t.ClearBeforeDelete) == 0 {
			return "none"
		}
		return strings.Join(t.ClearBeforeDelete, ",")
	case "timeout":
		return strconv.Itoa(t.TimeoutSeconds)
	}
	return ""
}

func awaitName(k catalog.AwaitKind) string {
	switch k {
	case catalog.AwaitLongRunning:
		return "longrunning"
	case catalog.AwaitComputeOperation:
		return "compute_operation"
	}
	return "none"
}

// attrFactValue is the catalog's value of one attribute fact, and whether
// it is set at all.
func attrFactValue(a *catalog.Attr, fact string) (string, bool) {
	switch fact {
	case "output":
		return strconv.FormatBool(a.Output), a.Output
	case "required":
		return strconv.FormatBool(a.Required), a.Required
	case "immutable":
		return strconv.FormatBool(a.ForceNew), a.ForceNew
	case "input_only":
		return strconv.FormatBool(a.InputOnly), a.InputOnly
	case "sensitive":
		return strconv.FormatBool(a.Sensitive), a.Sensitive
	case "equivalence":
		return a.Equivalence, a.Equivalence != ""
	case "unordered":
		return strconv.FormatBool(a.Unordered), a.Unordered
	case "send_with_update":
		return strconv.FormatBool(a.SendWithUpdate), a.SendWithUpdate
	}
	return "", false
}

// finishTypeSources gives every type fact nobody decided SourceDefault.
func finishTypeSources(t *catalog.Type) {
	for _, f := range TypeFacts {
		if len(t.Sources[f]) == 0 {
			setTypeSource(t, f, SourceDefault)
		}
	}
}

// Fact is one line of gen/facts.tsv.
type Fact struct {
	Type, Path, Fact, Value string
	Sources                 []string
}

// Facts lists every type fact of every type, and every attribute fact that
// is set or that some source spoke to, walking both Fields and Elem.
func Facts(c *catalog.Catalog) []Fact {
	var out []Fact
	for _, t := range c.Types {
		for _, f := range TypeFacts {
			out = append(out, Fact{t.Name, "", f, typeFactValue(t, f), t.Sources[f]})
		}
		walkAttrs(t.Attributes, "", func(path string, a *catalog.Attr) {
			for _, f := range AttrFacts {
				v, set := attrFactValue(a, f)
				if set || len(a.Sources[f]) > 0 {
					srcs := a.Sources[f]
					if set && winning(srcs) == 0 {
						// Set with no recorded reason: a site that decides
						// this fact and does not say so. TestEverySetFactHasASource
						// fails on it.
						srcs = append(append([]string{}, srcs...), "unrecorded")
					}
					out = append(out, Fact{t.Name, path, f, v, srcs})
				}
			}
		})
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.Type != b.Type {
			return a.Type < b.Type
		}
		if a.Path != b.Path {
			return a.Path < b.Path
		}
		return a.Fact < b.Fact
	})
	return out
}

// Unrecorded are the facts set with no source.
func Unrecorded(facts []Fact) []Fact {
	var out []Fact
	for _, f := range facts {
		if containsString(f.Sources, "unrecorded") {
			out = append(out, f)
		}
	}
	return out
}

func winning(srcs []string) int {
	n := 0
	for _, s := range srcs {
		if !strings.HasPrefix(s, lost) {
			n++
		}
	}
	return n
}

// walkAttrs visits every attribute at every depth, through Fields AND Elem.
// A list's element is addressed as "name[]".
func walkAttrs(attrs map[string]*catalog.Attr, prefix string, fn func(path string, a *catalog.Attr)) {
	names := make([]string, 0, len(attrs))
	for n := range attrs {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		a := attrs[n]
		path := n
		if prefix != "" {
			path = prefix + "." + n
		}
		fn(path, a)
		walkAttrs(a.Fields, path, fn)
		if a.Elem != nil {
			fn(path+"[]", a.Elem)
			walkAttrs(a.Elem.Fields, path+"[]", fn)
		}
	}
}

// requestShaping are the facts that decide what is sent or whether a change
// replaces: the ones an unknown costs something on.
var requestShaping = map[string]bool{
	"create_verb": true, "create_url": true, "create_await": true,
	"update_verb": true, "update_mask": true, "update_await": true,
	"delete_url": true, "delete_await": true,
	"setters": true, "lock_field": true, "patch_one_field": true, "clear_before_delete": true,
	"output": true, "required": true, "immutable": true, "input_only": true, "send_with_update": true,
}

// Unknowns splits the request-shaping facts no one has checked into two
// tiers. terraform-only: magic-modules or the generator's default alone,
// where live probes are chosen from. unverified: type facts no observation
// or ruling backs, whatever else says so. Attribute facts from Discovery are
// left out of the second tier: there are thousands, and Discovery is
// Google's own word.
func Unknowns(facts []Fact) (terraformOnly, unverified []Fact) {
	for _, f := range facts {
		if !requestShaping[f.Fact] {
			continue
		}
		win := map[string]bool{}
		for _, s := range f.Sources {
			if !strings.HasPrefix(s, lost) {
				win[s] = true
			}
		}
		if win[SourceObserved] || win[SourceRuling] {
			continue
		}
		if !win[SourceDiscovery] {
			// A fact that is not set and that nothing but a default speaks
			// to (an attribute is not required) says nothing; skip those.
			if f.Path != "" && (f.Value == "false" || f.Value == "") {
				continue
			}
			terraformOnly = append(terraformOnly, f)
			continue
		}
		if f.Path == "" {
			unverified = append(unverified, f)
		}
	}
	return terraformOnly, unverified
}

// RenderFacts is gen/facts.tsv.
func RenderFacts(facts []Fact) []byte {
	var b bytes.Buffer
	b.WriteString("# Every fact this catalog acts on, and which inputs decided it. See docs/FACTS.md.\n")
	b.WriteString("# GENERATED by cmd/gen-gcp. Do not hand-edit.\n")
	b.WriteString("# A source prefixed ! said the opposite and was overruled.\n")
	b.WriteString("#\n# type\tpath\tfact\tvalue\tsources\n")
	for _, f := range facts {
		fmt.Fprintf(&b, "%s\t%s\t%s\t%s\t%s\n", f.Type, f.Path, f.Fact, f.Value, strings.Join(f.Sources, "+"))
	}
	return b.Bytes()
}

// RenderUnknowns is gen/unknowns.txt.
func RenderUnknowns(terraformOnly, unverified []Fact) []byte {
	var b bytes.Buffer
	b.WriteString("# Facts that decide a request or a replace, and that nothing has checked. See docs/FACTS.md.\n")
	b.WriteString("# GENERATED by cmd/gen-gcp. Do not hand-edit.\n#\n")
	for _, tier := range []struct {
		name  string
		facts []Fact
	}{{"terraform-only", terraformOnly}, {"unverified", unverified}} {
		counts := map[string]int{}
		for _, f := range tier.facts {
			counts[f.Fact]++
		}
		keys := make([]string, 0, len(counts))
		for k := range counts {
			keys = append(keys, k)
		}
		sort.Slice(keys, func(i, j int) bool {
			return counts[keys[i]] > counts[keys[j]] || counts[keys[i]] == counts[keys[j]] && keys[i] < keys[j]
		})
		fmt.Fprintf(&b, "# %s by fact:", tier.name)
		for _, k := range keys {
			fmt.Fprintf(&b, " %s %d,", k, counts[k])
		}
		b.WriteString("\n")
	}
	b.WriteString("#\n")
	fmt.Fprintf(&b, "# terraform-only: %d facts backed by magic-modules or a default alone. Live probes start here.\n", len(terraformOnly))
	for _, f := range terraformOnly {
		fmt.Fprintf(&b, "terraform-only\t%s\t%s\t%s\t%s\t%s\n", f.Type, f.Path, f.Fact, f.Value, strings.Join(f.Sources, "+"))
	}
	fmt.Fprintf(&b, "#\n# unverified: %d type facts Discovery backs and no observation or ruling has checked.\n", len(unverified))
	for _, f := range unverified {
		fmt.Fprintf(&b, "unverified\t%s\t%s\t%s\t%s\t%s\n", f.Type, f.Path, f.Fact, f.Value, strings.Join(f.Sources, "+"))
	}
	return b.Bytes()
}

// WriteFacts writes both files. It refuses a fact that is set with no
// source: some place in the generator decided it without saying so.
func WriteFacts(factsPath, unknownsPath string, c *catalog.Catalog) error {
	facts := Facts(c)
	if bad := Unrecorded(facts); len(bad) > 0 {
		return fmt.Errorf("%d facts are set with no recorded source, first %s %s %s", len(bad), bad[0].Type, bad[0].Path, bad[0].Fact)
	}
	if err := os.WriteFile(factsPath, RenderFacts(facts), 0o644); err != nil {
		return err
	}
	to, un := Unknowns(facts)
	return os.WriteFile(unknownsPath, RenderUnknowns(to, un), 0o644)
}

// seenRE is an observation's Seen: a date and the live test.
var seenRE = regexp.MustCompile(`^\d{4}-\d{2}-\d{2} TestLive\w+$`)

// applyObserved checks every observation against the catalog and records
// it as a source. A catalog that contradicts one is refused: a ruling or a
// generator change that disagrees with what Google did is stale, and must
// say so loudly rather than win on rank.
func applyObserved(types []*catalog.Type, observed map[string][]Observation) error {
	byName := make(map[string]*catalog.Type, len(types))
	for _, t := range types {
		byName[t.Name] = t
	}
	names := make([]string, 0, len(observed))
	for n := range observed {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		t := byName[n]
		if t == nil {
			return fmt.Errorf("observed: %s does not ship", n)
		}
		for _, o := range observed[n] {
			if !seenRE.MatchString(o.Seen) {
				return fmt.Errorf("observed: %s %s: seen %q is not \"YYYY-MM-DD TestLiveName\"", n, o.Fact, o.Seen)
			}
			var got string
			switch {
			case o.Path == "" && containsString(TypeFacts, o.Fact):
				got = typeFactValue(t, o.Fact)
				// A test sees the setters it exercised, not all of them.
				if o.Fact == "setters" && containsString(strings.Split(got, ","), o.Value) {
					got = o.Value
				}
			case o.Path != "" && containsString(AttrFacts, o.Fact):
				a := attrAtPath(t.Attributes, o.Path)
				if a == nil {
					return fmt.Errorf("observed: %s has no attribute %q", n, o.Path)
				}
				got, _ = attrFactValue(a, o.Fact)
			default:
				return fmt.Errorf("observed: %s: %q is not a fact of the vocabulary for %s", n, o.Fact,
					map[bool]string{true: "a type", false: "an attribute"}[o.Path == ""])
			}
			if got != o.Value {
				return fmt.Errorf("observed: %s %s %s is %q on Google (%s), but the catalog says %q: "+
					"whatever changed it contradicts evidence", n, o.Path, o.Fact, o.Value, o.Seen, got)
			}
			if o.Path == "" {
				setTypeSource(t, o.Fact, append([]string{SourceObserved}, t.Sources[o.Fact]...)...)
			} else {
				addSource(attrAtPath(t.Attributes, o.Path), o.Fact, SourceObserved)
			}
		}
	}
	return nil
}

func containsString(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}
