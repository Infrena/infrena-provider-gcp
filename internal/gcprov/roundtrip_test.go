package gcprov

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/infrena/infrena-provider-gcp/internal/catalog"
	"github.com/infrena/infrena-provider-gcp/internal/gcpfake"
	"github.com/infrena/infrena-provider-gcp/internal/gcptest"
	"github.com/infrena/infrena/pkg/resource"
	"github.com/infrena/infrena/pkg/value"
)

// minimalValue is the smallest value of a's shape that is not empty: a
// required field gets one, and a map or list is filled with its own required
// fields so that nothing required is missing at any depth.
func minimalValue(a *catalog.Attr, name string) value.Value {
	src := value.SourceExplicit
	switch a.Kind {
	case value.KindString:
		return value.String("rt-"+strings.ToLower(name), src)
	case value.KindInt:
		return value.Int(1, src)
	case value.KindFloat:
		return value.Float(1.5, src)
	case value.KindBool:
		return value.Bool(true, src)
	case value.KindList:
		if a.Elem == nil {
			return value.List([]value.Value{value.String("rt", src)}, src)
		}
		return value.List([]value.Value{minimalValue(a.Elem, name)}, src)
	case value.KindMap:
		out := map[string]value.Value{}
		if len(a.Fields) == 0 {
			out["rt"] = value.String("rt", src)
		}
		for n, f := range a.Fields {
			if f.Required && !f.Output {
				out[n] = minimalValue(f, n)
			}
		}
		return value.Map(out, src)
	}
	return value.String("rt", src)
}

// minimalDesired is a configuration of ty with every required attribute and
// every create-url placeholder an attribute answers, and nothing else.
func minimalDesired(ty *catalog.Type) map[string]value.Value {
	out := map[string]value.Value{}
	for name, a := range ty.Attributes {
		// Create-only attributes are the ids a create is named by (roleId,
		// accountId), which every real configuration writes.
		if (a.Required || a.CreateOnly) && !a.Output {
			out[name] = minimalValue(a, name)
		}
	}
	// A settable name is written by nearly every real configuration, even
	// where the schema does not mark it required -- unless a create-only id
	// names the resource instead (an IAM role's roleId), in which case
	// Google refuses a name on create.
	hasCreateID := false
	for _, a := range ty.Attributes {
		hasCreateID = hasCreateID || (a.CreateOnly && !a.Output)
	}
	if a := ty.Attributes["name"]; a != nil && !a.Output && !hasCreateID {
		if _, set := out["name"]; !set {
			out["name"] = value.String("rt-name", value.SourceExplicit)
		}
	}
	// A placeholder bound to an attribute path (a BigQuery table's dataset,
	// tableReference.datasetId) is written there by configuration.
	for _, b := range ty.CreateBindings {
		if b.Attr == "" {
			continue
		}
		top, rest, nested := strings.Cut(b.Attr, ".")
		a := ty.Attributes[top]
		if a == nil || a.Output {
			continue
		}
		if !nested {
			out[top] = minimalValue(a, top)
			continue
		}
		m, _ := out[top].Raw.(map[string]value.Value)
		if m == nil {
			m = map[string]value.Value{}
		}
		if f := a.Fields[rest]; f != nil {
			m[rest] = minimalValue(f, rest)
		}
		out[top] = value.Map(m, value.SourceExplicit)
	}
	for ph := range placeholderNames(ty.CreateTemplate()) {
		for name, a := range ty.Attributes {
			spelled := name == ph || a.Canonical == ph
			for _, al := range a.Aliases {
				spelled = spelled || al == ph
			}
			if spelled && !a.Output {
				if _, set := out[name]; !set {
					out[name] = minimalValue(a, name)
				}
			}
		}
	}
	return out
}

func styleFor(k catalog.AwaitKind) gcpfake.OperationStyle {
	switch k {
	case catalog.AwaitLongRunning:
		return gcpfake.OpLongRunning
	case catalog.AwaitComputeOperation:
		return gcpfake.OpCompute
	}
	return gcpfake.OpSync
}

// TestEveryCreatableTypeRoundTripsAgainstTheFake takes every type in the real
// catalog that can be created through its whole life against the fake:
// create, read back, re-apply the same configuration, delete, read again.
//
// It exists because every structural defect found live on 2026-09-24 was
// one type at a time: a DNS policy's id that could not be computed, a create
// url token nobody filled, a constant read back as drift. Each would have
// failed here, for every type with the same shape, before anything reached
// Google. The fake cannot say what Google does with a field; it can say
// whether this provider can hold on to what it created.
func TestEveryCreatableTypeRoundTripsAgainstTheFake(t *testing.T) {
	gcptest.Isolate(t)
	c := mustCatalog(t)
	var (
		mu       sync.Mutex
		failures []string
		checked  int
		changed  int
	)
	counts := &discoStyleCounts{unmatchedByType: map[string][]string{}}
	// One subtest per type, in parallel: each has its own fake, and a type
	// whose read-back 404s spends seconds in the eventual-consistency retry.
	t.Run("types", func(t *testing.T) {
		for _, ty := range c.Types {
			if len(ty.UnresolvedCreatePlaceholders()) > 0 {
				continue
			}
			checked++
			t.Run(ty.Name, func(t *testing.T) {
				t.Parallel()
				msg, didChange := roundTrip(t, ty, counts)
				mu.Lock()
				if msg != "" {
					failures = append(failures, ty.Name+": "+msg)
				}
				if didChange {
					changed++
				}
				mu.Unlock()
			})
		}
	})
	sort.Strings(failures)
	// The types the fake cannot stand in for, each for a reason that is
	// about the fake or the harness, not a defect found here.
	known := map[string]string{
		// A binding's parent must be a real resource's full name, which the
		// round trip cannot invent; the live suite covers tag bindings.
		"gcp.tagbinding": "read after create finds nothing",
		// Its id ends {name}/{type}, and Google refuses a record set with no
		// type before creating anything, so the missing id is the harness's
		// minimal configuration, not an orphan.
		"gcp.resourcerecordset": `needs "type"`,
	}
	var unexpected []string
	seen := map[string]bool{}
	for _, f := range failures {
		name, _, _ := strings.Cut(f, ": ")
		if want, ok := known[name]; ok && strings.Contains(f, want) {
			seen[name] = true
			continue
		}
		unexpected = append(unexpected, f)
	}
	// An exception that no longer fails is stale, and a stale one would
	// hide the next real failure of that type. BigQuery's table sat here
	// after the fix that made it pass.
	for name := range known {
		if !seen[name] {
			unexpected = append(unexpected, name+": listed as a known exception but round-tripped cleanly; remove it")
		}
	}
	failures = unexpected
	if path := os.Getenv("GCP_ROUNDTRIP_REPORT"); path != "" {
		_ = os.WriteFile(path, []byte(strings.Join(failures, "\n")+"\n"), 0o644)
	}
	if checked < 150 {
		t.Errorf("only %d types were round-tripped; the catalog has far more creatable types", checked)
	}
	// A resolver that stopped matching would quietly hand every answer back
	// to the catalog, which is the thing under test.
	t.Logf("answers from Discovery: %d matched, %d fell back to the catalog; %d types changed a field",
		counts.matched, counts.unmatched, changed)
	// A mutation no Discovery method answers is a request Google does not
	// publish: this is how BigQuery's job delete, Cloud SQL's user delete
	// and Firestore's change-stream update were found (2026-09-25). The
	// exceptions are the harness's, not the provider's: its minimal name
	// is a bare word where these APIs take a full resource name.
	harness := map[string]bool{
		"gcp.topic":              true, // name is projects/{p}/topics/{t}
		"gcp.subscription":       true, // name is projects/{p}/subscriptions/{s}
		"gcp.container.nodepool": true, // parent is projects/{p}/locations/{l}/clusters/{c}
	}
	for name, reqs := range counts.unmatchedByType {
		if harness[name] {
			delete(harness, name)
			continue
		}
		t.Errorf("%s: sent a mutation no Discovery method answers: %v", name, reqs)
	}
	for name := range harness {
		t.Errorf("%s: listed as sending unpublished requests only because of the harness, but every one matched; remove it", name)
	}
	if counts.matched < 10*counts.unmatched {
		t.Errorf("only %d of %d mutations were answered from Discovery", counts.matched, counts.matched+counts.unmatched)
	}
	if changed < 60 {
		t.Errorf("only %d types changed a field; an update that changes nothing tests no mask", changed)
	}
	for _, f := range failures {
		t.Error(f)
	}
}

// roundTrip runs one type's life against a fresh fake and returns what went
// wrong first, or "".
func roundTrip(t *testing.T, ty *catalog.Type, counts *discoStyleCounts) (msg string, changed bool) {
	defer func() {
		if r := recover(); r != nil {
			msg = fmt.Sprintf("panic: %v", r)
		}
	}()
	s := gcpfake.New(t)
	defer s.Close()
	// Each mutation answers the way its own Discovery method does, not the
	// way the catalog says; the catalog's style is only the fallback.
	s.SetOperationStyleFor(discoStyleResolver(t, ty, counts))
	if os.Getenv("GCP_ROUNDTRIP_TRACE") == ty.Name {
		// Every request the fake saw, to find where a create and a read
		// disagree about where the resource lives.
		defer func() {
			for _, r := range s.Requests() {
				t.Logf("%s %s?%s %s", r.Method, r.Path, r.Query.Encode(), r.Body)
			}
		}()
	}
	s.SetOperationStyle(styleFor(ty.Await))
	// A copy with a short operation timeout: the fake answers at once or
	// never, and a type's real timeout is up to twenty minutes.
	short := *ty
	short.TimeoutSeconds = 3
	ty = &short
	p := testProviderWithCatalog(t, s, &catalog.Catalog{Types: []*catalog.Type{ty}})
	p.settings.Zone = "r-a"
	ctx := context.Background()

	desired := &resource.DesiredResource{Type: ty.Name, Attrs: minimalDesired(ty)}
	st, err := p.Create(ctx, desired)
	if err != nil {
		return "create: " + err.Error(), changed
	}
	if st == nil || st.ProviderID == "" {
		return "create returned no provider id", changed
	}
	if _, err := ParseProviderID(ty, st.ProviderID); err != nil {
		return "the provider id it returned does not parse: " + err.Error(), changed
	}
	for name, want := range desired.Attrs {
		if got := st.Attributes[name]; !got.Equal(want) {
			if os.Getenv("GCP_ROUNDTRIP_TRACE") == ty.Name {
				t.Logf("state after create: id %s attrs %v", st.ProviderID, st.Attributes)
				if held, ok := s.Get("/v1/" + st.ProviderID); ok {
					t.Logf("fake holds %v", held)
				}
			}
			return fmt.Sprintf("after create, %s is %v, configured %v", name, got.Raw, want.Raw), changed
		}
	}

	read, err := p.Read(ctx, st)
	if err != nil {
		return "read: " + err.Error(), changed
	}
	if read == nil {
		return "read after create finds nothing at " + st.ProviderID, changed
	}
	for name, want := range desired.Attrs {
		if got := read.Attributes[name]; !got.Equal(want) {
			return fmt.Sprintf("after a read, %s is %v, configured %v", name, got.Raw, want.Raw), changed
		}
	}

	if ty.UpdateVerb != "" || len(ty.Setters) > 0 {
		before := len(s.Requests())
		if _, err := p.Update(ctx, read, desired); err != nil {
			return "an update with nothing changed: " + err.Error(), changed
		}
		for _, r := range s.Requests()[before:] {
			if r.Method != "GET" {
				return fmt.Sprintf("an update with nothing changed sent %s %s %s", r.Method, r.Path, r.Body), changed
			}
		}

		// Then a real change, of one field an update may change. The fake
		// applies only what the mask names, answers the way the method's
		// own Discovery entry does, and a read afterwards must see it.
		if name := oneFieldToChange(ty); name != "" {
			next := &resource.DesiredResource{Type: ty.Name, Attrs: map[string]value.Value{}}
			for k, v := range desired.Attrs {
				next.Attrs[k] = v
			}
			want := value.String("rt-changed", value.SourceExplicit)
			next.Attrs[name] = want
			began := time.Now()
			updated, err := p.Update(ctx, read, next)
			if err != nil {
				return fmt.Sprintf("an update changing %s: %v", name, err), false
			}
			// An update that waits for an operation that never comes times
			// out, then reads back and reports success: against Google that
			// was twenty minutes per update. The fake answers at once, so
			// waiting out the timeout is always the bug.
			if time.Since(began) >= time.Duration(ty.TimeoutSeconds)*time.Second {
				return fmt.Sprintf("an update changing %s waited out its whole %ds timeout", name, ty.TimeoutSeconds), false
			}
			if updated == nil || !updated.Attributes[name].Equal(want) {
				return fmt.Sprintf("after an update changing %s, state holds %v", name, updated.Attributes[name].Raw), false
			}
			again, err := p.Read(ctx, updated)
			if err != nil || again == nil {
				return fmt.Sprintf("read after changing %s: %v", name, err), false
			}
			if got := again.Attributes[name]; !got.Equal(want) {
				return fmt.Sprintf("after changing %s, a read finds %v: the change never reached the resource", name, got.Raw), false
			}
			read, changed = again, true
		}
	}

	s.SetOperationStyle(styleFor(ty.DeleteAwaitKind()))
	began := time.Now()
	if err := p.Delete(ctx, read); err != nil {
		return "delete: " + err.Error(), changed
	}
	if time.Since(began) >= time.Duration(ty.TimeoutSeconds)*time.Second {
		return fmt.Sprintf("the delete waited out its whole %ds timeout", ty.TimeoutSeconds), changed
	}
	gone, err := p.Read(ctx, read)
	if err != nil {
		return "read after delete: " + err.Error(), changed
	}
	if gone != nil {
		return "still readable after delete at " + read.ProviderID, changed
	}
	return "", changed
}

// oneFieldToChange picks a top-level string an update may change and the
// fake can hold any value of: settable, not ForceNew, not in the url, not a
// reference, a lock, a secret, input-only or compared by a rule.
// description first, because nearly every API has one and none validates
// it.
func oneFieldToChange(ty *catalog.Type) string {
	inURL := urlIdentifying(ty)
	ok := func(name string) bool {
		a := ty.Attributes[name]
		if a == nil || a.Kind != value.KindString || a.Output || a.ForceNew || a.CreateOnly ||
			a.InputOnly || a.Sensitive || a.Ref != nil || a.Equivalence != "" ||
			inURL[name] || inURL[a.Canonical] || a.Canonical == ty.LockField {
			return false
		}
		return ty.UpdateVerb != "" || ty.SetterFor(a.Canonical) != nil
	}
	if ok("description") {
		return "description"
	}
	for _, name := range sortedAttrNames(ty.Attributes) {
		if ok(name) {
			return name
		}
	}
	return ""
}
