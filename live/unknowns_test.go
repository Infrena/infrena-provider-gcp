//go:build live

package live

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"
)

// fingerprintOf reads a compute resource's current fingerprint.
func (g *google) fingerprintOf(t *testing.T, url string) string {
	t.Helper()
	var got struct {
		Fingerprint string `json:"fingerprint"`
	}
	_, raw, _ := g.get(t.Context(), url)
	_ = json.Unmarshal(raw, &got)
	return got.Fingerprint
}

// probePatch sends one compute PATCH and logs Google's verdict and what the
// resource holds afterwards, for the fields named.
func (g *google) probePatch(t *testing.T, what, url string, body map[string]any, show ...string) {
	t.Helper()
	status, opErr, answer := g.computeOp(t, t.Context(), http.MethodPatch, url, body)
	if len(answer) > 300 {
		answer = answer[:300]
	}
	var after map[string]any
	_, raw, _ := g.get(t.Context(), url)
	_ = json.Unmarshal(raw, &after)
	held := map[string]any{}
	for _, f := range show {
		held[f] = after[f]
	}
	t.Logf("PROBE %s: status %d, operation error %q, now holds %v; first answer %s", what, status, opErr, held, answer)
}

// TestLiveProbeUnknowns asks Google about facts gen/unknowns.txt lists as
// resting on magic-modules or a default alone, one cheap type per class.
// Logged, not asserted: each answer decides a ruling.
//
//   - immutable, magic-modules only: does a PATCH of the field apply? If it
//     does, a change to it REPLACES the resource for nothing.
//   - one field per patch, never stated: does a PATCH of two fields at once
//     apply? Compute's subnetwork refuses it, and nothing says which others do.
func TestLiveProbeUnknowns(t *testing.T) {
	project, sa := guard(t)
	n := newNames()
	g := newGoogle(t, project, sa)
	cleanup := func(ty, url string) {
		t.Cleanup(func() {
			ctx, cancel := context.WithTimeout(context.WithoutCancel(context.Background()), 10*time.Minute)
			defer cancel()
			g.deleteAndWait(t, ctx, ty, url, url)
		})
	}
	create := func(t *testing.T, collection string, body map[string]any) {
		t.Helper()
		if s, e, raw := g.computeOp(t, t.Context(), http.MethodPost, g.computeURL(collection), body); s >= 300 || e != "" {
			t.Fatalf("creating %v in %s: %d %s %s", body["name"], collection, s, e, raw)
		}
	}
	defaultNet := g.computeURL("projects/" + project + "/global/networks/default")

	t.Run("firewall", func(t *testing.T) {
		name := "infrena-probe-fw-" + n.run
		url := g.computeURL("projects/" + project + "/global/firewalls/" + name)
		cleanup("gcp.firewall", url)
		create(t, "projects/"+project+"/global/firewalls", map[string]any{
			"name": name, "network": defaultNet, "direction": "INGRESS", "priority": 1000,
			"sourceRanges": []string{"10.254.0.0/24"},
			"allowed":      []any{map[string]any{"IPProtocol": "tcp", "ports": []string{"9"}}},
		})
		g.probePatch(t, "firewall two fields (description, priority)", url,
			map[string]any{"description": "two fields", "priority": 1001}, "description", "priority")
		g.probePatch(t, "firewall direction (magic-modules immutable)", url,
			map[string]any{"direction": "EGRESS", "destinationRanges": []string{"10.254.0.0/24"}, "sourceRanges": []string{}},
			"direction")
	})

	t.Run("health_check", func(t *testing.T) {
		name := "infrena-probe-hc-" + n.run
		url := g.computeURL("projects/" + project + "/global/healthChecks/" + name)
		cleanup("gcp.healthcheck", url)
		create(t, "projects/"+project+"/global/healthChecks", map[string]any{
			"name": name, "type": "TCP", "tcpHealthCheck": map[string]any{"port": 80},
		})
		g.probePatch(t, "health check two fields (checkIntervalSec, timeoutSec)", url,
			map[string]any{"checkIntervalSec": 11, "timeoutSec": 6}, "checkIntervalSec", "timeoutSec")
	})

	t.Run("ssl_policy", func(t *testing.T) {
		name := "infrena-probe-ssl-" + n.run
		url := g.computeURL("projects/" + project + "/global/sslPolicies/" + name)
		cleanup("gcp.sslpolicy", url)
		create(t, "projects/"+project+"/global/sslPolicies", map[string]any{"name": name, "profile": "COMPATIBLE", "minTlsVersion": "TLS_1_0"})
		g.probePatch(t, "ssl policy two fields (minTlsVersion, profile)", url,
			map[string]any{"minTlsVersion": "TLS_1_2", "profile": "MODERN", "fingerprint": g.fingerprintOf(t, url)},
			"minTlsVersion", "profile")
		g.probePatch(t, "ssl policy description (magic-modules immutable)", url,
			map[string]any{"description": "changed", "fingerprint": g.fingerprintOf(t, url)}, "description")
	})

	t.Run("router", func(t *testing.T) {
		name := "infrena-probe-rt-" + n.run
		url := g.computeURL("projects/" + project + "/regions/" + region + "/routers/" + name)
		cleanup("gcp.router", url)
		create(t, "projects/"+project+"/regions/"+region+"/routers", map[string]any{
			"name": name, "network": defaultNet, "bgp": map[string]any{"asn": 64514},
		})
		g.probePatch(t, "router two fields (description, bgp.keepaliveInterval)", url,
			map[string]any{"description": "two fields", "bgp": map[string]any{"asn": 64514, "keepaliveInterval": 30}},
			"description", "bgp")
	})

	t.Run("bucket_default_object_acl", func(t *testing.T) {
		name := "infrena-probe-acl-" + n.run
		url := g.storageURL("b/" + name)
		cleanup("gcp.storage.bucket", url)
		code, raw, _, err := g.doRetry(t.Context(), http.MethodPost, g.storageURL("b?project="+project),
			map[string]any{"name": name, "location": "US-CENTRAL1"})
		if err != nil || code >= 300 {
			t.Fatalf("creating the probe bucket: %d %s %v", code, raw, err)
		}
		number, err := g.projectNumber(t.Context(), project)
		if err != nil {
			t.Fatal(err)
		}
		code, raw, _, err = g.doRetry(t.Context(), http.MethodPatch, url, map[string]any{
			"defaultObjectAcl": []any{map[string]any{"entity": "project-owners-" + number, "role": "OWNER"}},
		})
		if len(raw) > 300 {
			raw = raw[:300]
		}
		t.Logf("PROBE bucket PATCH defaultObjectAcl (magic-modules immutable): status %d %v answer %s", code, err, raw)
		_, full, _ := g.get(t.Context(), url+"?projection=full")
		var got struct {
			DefaultObjectACL []map[string]any `json:"defaultObjectAcl"`
			IAM              map[string]any   `json:"iamConfiguration"`
		}
		_ = json.Unmarshal(full, &got)
		t.Logf("PROBE bucket now holds defaultObjectAcl %v (iamConfiguration %v)", got.DefaultObjectACL, got.IAM)
	})
}
