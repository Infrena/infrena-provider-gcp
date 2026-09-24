//go:build live

package live

import (
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"
)

// TestLiveDNSPolicyIsDestroyedWithItsNetworksAttached. Google refuses to
// delete a Cloud DNS policy while networks are attached, and removing them
// from configuration does not detach them (a field configuration stops
// mentioning keeps its value). Destroying the policy only works because
// Delete clears networks first (clear_before_delete in its ruling).
func TestLiveDNSPolicyIsDestroyedWithItsNetworksAttached(t *testing.T) {
	project, sa := guard(t)
	n := newNames()
	g := newGoogle(t, project, sa)
	netName := "infrena-dnsnet-" + n.run
	polName := "infrena-dnspol-" + n.run
	netURL := g.computeURL("projects/" + project + "/global/networks/" + netName)
	polURL := "https://dns.googleapis.com/dns/v1/projects/" + project + "/policies/" + polName
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(context.Background()), 10*time.Minute)
		defer cancel()
		// Detach before deleting, or the cleanup meets the same refusal the
		// provider is being tested against.
		_, _, _, _ = g.doRetry(ctx, http.MethodPatch, polURL, map[string]any{"networks": []any{}})
		g.deleteAndWait(t, ctx, "gcp.dns.policy", polURL, polURL)
		g.deleteAndWait(t, ctx, "gcp.network", netURL, netURL)
	})

	withPolicy := fmt.Sprintf(`project: infrena-gcp-live-dns
%[1]sresources:
  net:
    type: gcp.network
    name: %[2]s
    autoCreateSubnetworks: false
  policy:
    type: gcp.dns.policy
    name: %[3]s
    description: created by the infrena live suite
    networks:
      - networkUrl: ${net.selfLink}
`, liveProvider(project, sa), netName, polName)
	dir := t.TempDir()
	write(t, dir, "infrena.yml", withPolicy)
	applyOK(t, dir, "create")
	if changes := planIs(t, dir, exitOK); len(changes) != 0 {
		t.Errorf("the plan after creating the policy proposes %v", changes)
	}

	write(t, dir, "infrena.yml", cut(withPolicy, "  policy:"))
	changes := planIs(t, dir, exitChanges)
	if len(changes) != 1 || changes[0].Kind != "destroy" {
		t.Fatalf("removing the policy from configuration plans %v, want one destroy", changes)
	}
	applyOK(t, dir, "destroy the policy")
	code, raw, err := g.get(t.Context(), polURL)
	if err == nil && code == http.StatusOK {
		t.Errorf("the policy is still there after a successful destroy: %s", raw)
	}

	write(t, dir, "infrena.yml", cut(withPolicy, "resources:"))
	applyOK(t, dir, "destroy the network")
}

// TestLiveProbeDNSDescriptionRequired asks whether Cloud DNS refuses a
// response policy and a managed zone created without a description, as it
// refuses a policy. Terraform always sends one, so nothing else has asked.
// Logged, not asserted: each answer decides whether that type's ruling gets
// `required: [description]`.
func TestLiveProbeDNSDescriptionRequired(t *testing.T) {
	project, sa := guard(t)
	n := newNames()
	g := newGoogle(t, project, sa)
	base := "https://dns.googleapis.com/dns/v1/projects/" + project
	for _, c := range []struct {
		what, collection, name string
		body                   map[string]any
	}{
		{"response policy", "/responsePolicies", "infrena-probe-rp-" + n.run,
			map[string]any{"responsePolicyName": "infrena-probe-rp-" + n.run}},
		{"managed zone", "/managedZones", "infrena-probe-mz-" + n.run,
			map[string]any{"name": "infrena-probe-mz-" + n.run, "dnsName": "infrena-probe-" + n.run + ".example.com.", "visibility": "private"}},
	} {
		code, raw, _, err := g.doRetry(t.Context(), http.MethodPost, base+c.collection, c.body)
		t.Logf("PROBE %s created without a description: %d %v %s", c.what, code, err, raw)
		if code < 300 {
			url := base + c.collection + "/" + c.name
			ctx, cancel := context.WithTimeout(context.WithoutCancel(context.Background()), 5*time.Minute)
			g.deleteAndWait(t, ctx, c.what, url, url)
			cancel()
		}
	}
}
