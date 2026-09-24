package gcprov

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/infrena/infrena-provider-gcp/internal/catalog"
	"github.com/infrena/infrena-provider-gcp/internal/gcpfake"
	"github.com/infrena/infrena-provider-gcp/internal/gcptest"
	"github.com/infrena/infrena/pkg/resource"
	"github.com/infrena/infrena/pkg/value"
)

const labelledPath = "/v1/projects/p/locations/r/widgets/one"

// labelledWidget is a widget whose labels change only through setLabels,
// guarded by a labelFingerprint, the way compute's are. The fake refuses a
// setLabels that quotes a stale fingerprint.
func labelledWidget(t *testing.T, verb string) (*gcpfake.Server, *Provider) {
	t.Helper()
	s := gcpfake.New(t)
	s.Seed(labelledPath, map[string]any{
		"name": "one", "sizeGb": float64(10),
		"labels": map[string]any{"team": "a"}, "labelFingerprint": "lfp-current",
	})
	ty := widgetType()
	ty.UpdateVerb = verb
	ty.UpdateMask = false
	ty.Attributes["labels"] = &catalog.Attr{Canonical: "labels", Kind: value.KindMap}
	ty.Attributes["labelFingerprint"] = &catalog.Attr{Canonical: "labelFingerprint", Kind: value.KindString, Output: true}
	ty.Setters = []catalog.Setter{{
		Method: "setLabels", Verb: "POST", Path: ty.SelfLink + "/setLabels",
		Fields: []string{"labels"}, Lock: "labelFingerprint",
	}}
	return s, testProviderWithCatalog(t, s, &catalog.Catalog{Types: []*catalog.Type{ty}})
}

func labelledState() *resource.ResourceState {
	return &resource.ResourceState{
		Type:       "gcp.widget",
		ProviderID: "projects/p/locations/r/widgets/one",
		Attributes: attrsMixed(map[string]any{
			"project": "p", "region": "r", "name": "one", "sizeGb": int64(10),
			"labels": map[string]any{"team": "a"}, "labelFingerprint": "lfp-current",
		}),
	}
}

func sentBody(t *testing.T, r gcpfake.Request) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(r.Body, &out); err != nil {
		t.Fatalf("body is not json: %v (%s)", err, r.Body)
	}
	return out
}

// TestALabelChangeGoesToItsSetter. Compute: labels "can only be added or
// modified by the setLabels method". Patched instead, the change is dropped
// and the plan proposes it for ever.
func TestALabelChangeGoesToItsSetter(t *testing.T) {
	gcptest.Isolate(t)
	s, p := labelledWidget(t, "PATCH")
	defer s.Close()

	st, err := p.Update(context.Background(), labelledState(), widgetDesired(map[string]any{
		"project": "p", "region": "r", "name": "one", "sizeGb": int64(10),
		"labels": map[string]any{"team": "b"},
	}))
	if err != nil {
		t.Fatal(err)
	}
	body := sentBody(t, requestOf(t, s, "POST", labelledPath+"/setLabels"))
	if l, _ := body["labels"].(map[string]any); l["team"] != "b" {
		t.Errorf("setLabels body labels = %v, want team=b", body["labels"])
	}
	if body["labelFingerprint"] != "lfp-current" {
		t.Errorf("setLabels body labelFingerprint = %v, want the current one", body["labelFingerprint"])
	}
	for _, r := range s.Requests() {
		if r.Method == "PATCH" {
			t.Errorf("a label change was also patched: %s", r.Body)
		}
	}
	if got, _ := st.Attributes["labels"].Raw.(map[string]value.Value); got["team"].Raw != "b" {
		t.Errorf("state after the update has labels %v, want the read-back team=b", st.Attributes["labels"])
	}
}

// TestAChangeToBothPatchesTheRestAndSetsTheLabels. The patch must not carry
// the labels, in its body or its mask: compute drops them there, and a mask
// naming them is refused outright on the APIs that take one.
func TestAChangeToBothPatchesTheRestAndSetsTheLabels(t *testing.T) {
	gcptest.Isolate(t)
	s, p := labelledWidget(t, "PATCH")
	defer s.Close()

	if _, err := p.Update(context.Background(), labelledState(), widgetDesired(map[string]any{
		"project": "p", "region": "r", "name": "one", "sizeGb": int64(20),
		"labels": map[string]any{"team": "b"},
	})); err != nil {
		t.Fatal(err)
	}
	patch := sentBody(t, requestOf(t, s, "PATCH", labelledPath))
	if patch["sizeGb"] != float64(20) {
		t.Errorf("patch sizeGb = %v, want 20", patch["sizeGb"])
	}
	if _, present := patch["labels"]; present {
		t.Errorf("the patch carries labels, which only setLabels changes: %v", patch)
	}
	requestOf(t, s, "POST", labelledPath+"/setLabels")
}

// TestASetterMakesATypeWithoutAnUpdateVerbUpdatable. Most compute types
// that take setLabels publish no patch at all (addresses, VPN gateways,
// snapshots). Until setters existed, changing a label replaced them.
func TestASetterMakesATypeWithoutAnUpdateVerbUpdatable(t *testing.T) {
	gcptest.Isolate(t)
	s, p := labelledWidget(t, "")
	defer s.Close()

	if _, err := p.Update(context.Background(), labelledState(), widgetDesired(map[string]any{
		"project": "p", "region": "r", "name": "one", "sizeGb": int64(10),
		"labels": map[string]any{"team": "b"},
	})); err != nil {
		t.Fatalf("a label change on a type whose only update is setLabels: %v", err)
	}
	requestOf(t, s, "POST", labelledPath+"/setLabels")
}

// TestAnUnchangedSetterFieldSendsNothing. Labels that already match are not
// a reason to call setLabels, and a fingerprint is never a reason at all.
func TestAnUnchangedSetterFieldSendsNothing(t *testing.T) {
	gcptest.Isolate(t)
	s, p := labelledWidget(t, "PATCH")
	defer s.Close()

	if _, err := p.Update(context.Background(), labelledState(), widgetDesired(map[string]any{
		"project": "p", "region": "r", "name": "one", "sizeGb": int64(10),
		"labels": map[string]any{"team": "a"},
	})); err != nil {
		t.Fatal(err)
	}
	for _, r := range s.Requests() {
		if r.Method != "GET" {
			t.Errorf("sent %s %s with nothing changed", r.Method, r.Path)
		}
	}
}

// TestACreateSetsWhatTheInsertIgnored. An insert drops what only a setter
// changes (a backend service's securityPolicy), so the first plan after the
// create would propose it again. The create calls the setter itself.
func TestACreateSetsWhatTheInsertIgnored(t *testing.T) {
	gcptest.Isolate(t)
	s, p := labelledWidget(t, "PATCH")
	defer s.Close()
	s.DropOnCreate("labels")

	st, err := p.Create(context.Background(), widgetDesired(map[string]any{
		"project": "p", "region": "r", "name": "two", "sizeGb": int64(10),
		"labels": map[string]any{"team": "c"},
	}))
	if err != nil {
		t.Fatal(err)
	}
	path := strings.Replace(labelledPath, "/one", "/two", 1)
	body := sentBody(t, requestOf(t, s, "POST", path+"/setLabels"))
	if l, _ := body["labels"].(map[string]any); l["team"] != "c" {
		t.Errorf("setLabels after create sent labels %v, want team=c", body["labels"])
	}
	if got, _ := st.Attributes["labels"].Raw.(map[string]value.Value); got["team"].Raw != "c" {
		t.Errorf("state after create has labels %v, want team=c read back after setLabels", st.Attributes["labels"])
	}
}

// TestACreateTheInsertHonouredCallsNoSetter. When the insert kept the value,
// calling the setter again is a second mutation for nothing.
func TestACreateTheInsertHonouredCallsNoSetter(t *testing.T) {
	gcptest.Isolate(t)
	s, p := labelledWidget(t, "PATCH")
	defer s.Close()

	if _, err := p.Create(context.Background(), widgetDesired(map[string]any{
		"project": "p", "region": "r", "name": "two", "sizeGb": int64(10),
		"labels": map[string]any{"team": "c"},
	})); err != nil {
		t.Fatal(err)
	}
	for _, r := range s.Requests() {
		if strings.HasSuffix(r.Path, "/setLabels") {
			t.Errorf("called setLabels after a create that already applied the labels: %s", r.Body)
		}
	}
}
