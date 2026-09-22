package gcprov

import (
	"strings"
	"testing"

	"github.com/infrena/infrena-provider-gcp/internal/catalog"
)

// TestSystemOwnedIsDecidedByNamedEvidence. Evidence, never category. Each
// case names the literal signal, and the reason string must name it too, so
// a user overriding the flag knows what they are overriding.
func TestSystemOwnedIsDecidedByNamedEvidence(t *testing.T) {
	cases := []struct {
		name     string
		ty       *catalog.Type
		body     map[string]any
		want     bool
		evidence string
	}{
		{"the auto-created default network",
			&catalog.Type{Name: "gcp.network"},
			map[string]any{"name": "default", "autoCreateSubnetworks": true},
			true, "default"},
		{"a default-allow firewall rule",
			&catalog.Type{Name: "gcp.firewall"},
			map[string]any{"name": "default-allow-internal"},
			true, "default-allow"},
		{"a GKE-created resource",
			&catalog.Type{Name: "gcp.instance"},
			map[string]any{"name": "gke-node-1", "labels": map[string]any{"goog-gke-node": "true"}},
			true, "goog-gke-node"},
		{"a Config Connector-managed resource",
			&catalog.Type{Name: "gcp.bucket"},
			map[string]any{"name": "b", "labels": map[string]any{"managed-by-cnrm": "true"}},
			true, "managed-by-cnrm"},
		{"an ordinary user network called something else",
			&catalog.Type{Name: "gcp.network"},
			map[string]any{"name": "prod-vpc", "autoCreateSubnetworks": false},
			false, ""},
		// The one that matters most: a user network someone named "default"
		// but which is NOT auto-mode is still theirs. Name alone is not
		// evidence.
		{"a custom-mode network the user named default",
			&catalog.Type{Name: "gcp.network"},
			map[string]any{"name": "default", "autoCreateSubnetworks": false},
			false, ""},
	}
	for _, c := range cases {
		got, reason := SystemOwned(c.ty, c.body)
		if got != c.want {
			t.Errorf("%s: SystemOwned = %v, want %v (reason %q)", c.name, got, c.want, reason)
		}
		if got && !strings.Contains(reason, c.evidence) {
			t.Errorf("%s: reason %q does not name the evidence %q", c.name, reason, c.evidence)
		}
		if !got && reason != "" {
			t.Errorf("%s: not system owned but gave a reason %q", c.name, reason)
		}
	}
}

// TestSystemOwnedAcceptsAFullResourceNameToo.
func TestSystemOwnedAcceptsAFullResourceNameToo(t *testing.T) {
	// A Cloud Asset Inventory result names the resource in full. Comparing
	// the whole string against "default" would never match, so every auto
	// mode network in the inventory would come back unflagged -- and the
	// CAI path is the DEFAULT discovery path, so that is the case that would
	// matter in practice.
	got, reason := SystemOwned(&catalog.Type{Name: "gcp.network"}, map[string]any{
		"name":                  "//compute.googleapis.com/projects/p/global/networks/default",
		"autoCreateSubnetworks": true,
	})
	if !got {
		t.Fatalf("a full resource name for the auto mode network was not recognised (reason %q)", reason)
	}
	if !strings.Contains(reason, "autoCreateSubnetworks") {
		t.Errorf("reason %q does not name the evidence", reason)
	}
}

// TestSystemOwnedReadsResourceLabelsAsWellAsLabels. GKE's own Cluster resource
// carries its labels under "resourceLabels", not "labels"
// (schemas/container.json). Reading only "labels" would flag nothing on a
// cluster at all, silently.
func TestSystemOwnedReadsResourceLabelsAsWellAsLabels(t *testing.T) {
	got, reason := SystemOwned(&catalog.Type{Name: "gcp.container.cluster"}, map[string]any{
		"name":           "c",
		"resourceLabels": map[string]any{"goog-composer-environment": "e"},
	})
	if !got {
		t.Fatal("a resourceLabels-carried Google label was not recognised")
	}
	if !strings.Contains(reason, "goog-composer-environment") {
		t.Errorf("reason %q does not name the label it found", reason)
	}
}

// TestADefaultAllowLookalikeIsNotFlagged. The rules Google creates alongside
// an auto mode network are a closed set of four. A rule a user named
// "default-allow-from-office" merely shares a naming habit with them, and
// flagging it withholds the user's own firewall rule on no evidence at all.
func TestADefaultAllowLookalikeIsNotFlagged(t *testing.T) {
	for _, name := range []string{"default-allow-from-office", "default-allow", "my-default-allow-ssh"} {
		got, reason := SystemOwned(&catalog.Type{Name: "gcp.firewall"}, map[string]any{"name": name})
		if got {
			t.Errorf("%q was flagged as system owned (%q)", name, reason)
		}
	}
	for _, name := range []string{"default-allow-internal", "default-allow-ssh", "default-allow-rdp", "default-allow-icmp"} {
		got, _ := SystemOwned(&catalog.Type{Name: "gcp.firewall"}, map[string]any{"name": name})
		if !got {
			t.Errorf("%q, which Google creates, was not flagged", name)
		}
	}
}

// TestTheSystemOwnedReasonDoesNotDependOnMapOrder. A resource carrying two system labels
// must always be explained by the same one: map iteration order is
// randomised per run, so an unsorted scan makes the reason a user sees
// change between two `discover` runs over an unchanged project.
func TestTheSystemOwnedReasonDoesNotDependOnMapOrder(t *testing.T) {
	body := map[string]any{
		"name": "x",
		"labels": map[string]any{
			"goog-gke-node":   "true",
			"goog-composer-x": "true",
			"managed-by-cnrm": "true",
		},
	}
	_, first := SystemOwned(&catalog.Type{Name: "gcp.instance"}, body)
	for i := 0; i < 50; i++ {
		if _, reason := SystemOwned(&catalog.Type{Name: "gcp.instance"}, body); reason != first {
			t.Fatalf("reason varies between runs: %q then %q", first, reason)
		}
	}
}
