//go:build live

package live

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"math/big"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// computeOp sends one compute mutation and waits for its operation, returning
// the HTTP status, the operation's error (if it finished with one), and the
// raw first answer. For probes that ask Google a question directly, beside
// what infrena does.
func (g *google) computeOp(t *testing.T, ctx context.Context, method, url string, body any) (int, string, []byte) {
	t.Helper()
	code, data, _, err := g.doRetry(ctx, method, url, body)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	if code >= 300 {
		return code, errorStatus(data), data
	}
	var op struct {
		SelfLink string `json:"selfLink"`
	}
	if json.Unmarshal(data, &op) != nil || op.SelfLink == "" {
		return code, "", data
	}
	deadline := time.Now().Add(3 * time.Minute)
	for time.Now().Before(deadline) {
		_, polled, err := g.get(ctx, op.SelfLink)
		if err == nil {
			var st struct {
				Status string `json:"status"`
				Error  *struct {
					Errors []struct {
						Code    string `json:"code"`
						Message string `json:"message"`
					} `json:"errors"`
				} `json:"error"`
			}
			if json.Unmarshal(polled, &st) == nil && st.Status == "DONE" {
				if st.Error != nil && len(st.Error.Errors) > 0 {
					return code, st.Error.Errors[0].Code + ": " + st.Error.Errors[0].Message, polled
				}
				return code, "", polled
			}
		}
		time.Sleep(2 * time.Second)
	}
	t.Fatalf("%s %s: operation %s never finished", method, url, op.SelfLink)
	return 0, "", nil
}

func liveProvider(project, sa string) string {
	return fmt.Sprintf(`environments:
  live: {}
providers:
  - plugin: gcp
    project: %s
    region: %s
    impersonate_service_account: %s
`, project, region, sa)
}

func applyOK(t *testing.T, dir, what string) {
	t.Helper()
	r := run(t, dir, "apply", "live", "--auto-approve")
	t.Logf("apply (%s):\n%s", what, r.combined())
	if r.ExitCode != exitChanges && r.ExitCode != exitOK {
		t.Fatalf("apply (%s) failed:\n%s", what, r.combined())
	}
}

func planIs(t *testing.T, dir string, want int) []change {
	t.Helper()
	out := filepath.Join(t.TempDir(), "plan.json")
	mustRun(t, dir, want, "plan", "live", "--output", out)
	return planChanges(t, out)
}

// TestLiveLoadBalancerSetters is the setter path on real Google, through the
// load balancer P1 made shippable: a bucket behind a backend bucket, two url
// maps, a global target HTTP proxy and a global forwarding rule.
//
// Swapping the proxy's url map is setUrlMap, which compute publishes WITHOUT
// the /global/ segment the proxy is read at -- the one path shape only
// Discovery's own method path gets right. Changing the forwarding rule's
// labels is setLabels with its labelFingerprint. Each must plan as an UPDATE:
// before setters existed, both replaced the resource.
func TestLiveLoadBalancerSetters(t *testing.T) {
	project, sa := guard(t)
	n := newNames()
	g := newGoogle(t, project, sa)
	id := "infrena-lb-" + n.run
	p := "projects/" + project + "/global/"
	paths := []struct{ ty, url string }{
		{"gcp.globalforwardingrule", g.computeURL(p + "forwardingRules/" + id)},
		{"gcp.targethttpproxy", g.computeURL(p + "targetHttpProxies/" + id)},
		{"gcp.urlmap", g.computeURL(p + "urlMaps/" + id + "-a")},
		{"gcp.urlmap", g.computeURL(p + "urlMaps/" + id + "-b")},
		{"gcp.backendbucket", g.computeURL(p + "backendBuckets/" + id)},
		{"gcp.storage.bucket", g.storageURL("b/" + id)},
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(context.Background()), 15*time.Minute)
		defer cancel()
		for _, c := range paths {
			g.deleteAndWait(t, ctx, c.ty, c.url, c.url)
		}
	})

	var cfgDesc func(urlMap, team, desc string) string
	cfg := func(urlMap, team string) string {
		return cfgDesc(urlMap, team, "")
	}
	cfgDesc = func(urlMap, team, desc string) string {
		return fmt.Sprintf(`project: infrena-gcp-live-lb
%[1]sresources:
  bucket:
    type: gcp.storage.bucket
    name: %[2]s
    location: US-CENTRAL1
  backend:
    type: gcp.backendbucket
    name: %[2]s
    bucketName: ${bucket.name}
  map_a:
    type: gcp.urlmap
    name: %[2]s-a
    defaultService: ${backend.selfLink}
  map_b:
    type: gcp.urlmap
    name: %[2]s-b
    defaultService: ${backend.selfLink}
  proxy:
    type: gcp.targethttpproxy
    name: %[2]s
    urlMap: ${%[3]s.selfLink}
  rule:
    type: gcp.globalforwardingrule
    name: %[2]s
    target: ${proxy.selfLink}
    portRange: "80"
    loadBalancingScheme: EXTERNAL_MANAGED
%[5]s    labels:
      infrena-live: "true"
      team: %[4]s
`, liveProvider(project, sa), id, urlMap, team, desc)
	}
	dir := t.TempDir()
	write(t, dir, "infrena.yml", cfg("map_a", "a"))
	applyOK(t, dir, "create")

	t.Run("a_second_plan_is_clean", func(t *testing.T) {
		if changes := planIs(t, dir, exitOK); len(changes) != 0 {
			t.Errorf("the plan after creating the load balancer proposes %v", changes)
		}
	})

	t.Run("swapping_the_url_map_is_setUrlMap", func(t *testing.T) {
		write(t, dir, "infrena.yml", cfg("map_b", "a"))
		changes := planIs(t, dir, exitChanges)
		if len(changes) != 1 || changes[0].Kind != "update" {
			t.Fatalf("swapping the proxy's url map plans %v; it must be one UPDATE of proxy", changes)
		}
		applyOK(t, dir, "setUrlMap")
		var proxy struct {
			URLMap string `json:"urlMap"`
		}
		code, raw, err := g.get(t.Context(), paths[1].url)
		if err == nil && code == http.StatusOK {
			err = json.Unmarshal(raw, &proxy)
		}
		if err != nil || !strings.HasSuffix(proxy.URLMap, "/urlMaps/"+id+"-b") {
			t.Errorf("google's proxy does not point at map b: %d %s %v", code, raw, err)
		}
		if changes := planIs(t, dir, exitOK); len(changes) != 0 {
			t.Errorf("the plan after setUrlMap proposes %v", changes)
		}
	})

	t.Run("a_label_change_is_setLabels", func(t *testing.T) {
		write(t, dir, "infrena.yml", cfg("map_b", "b"))
		changes := planIs(t, dir, exitChanges)
		if len(changes) != 1 || changes[0].Kind != "update" {
			t.Fatalf("a label change on the forwarding rule plans %v; it must be one UPDATE", changes)
		}
		applyOK(t, dir, "setLabels")
		var rule struct {
			Labels map[string]string `json:"labels"`
		}
		code, raw, err := g.get(t.Context(), paths[0].url)
		if err == nil && code == http.StatusOK {
			err = json.Unmarshal(raw, &rule)
		}
		if err != nil || rule.Labels["team"] != "b" {
			t.Errorf("google's forwarding rule does not carry team=b: %d %s %v", code, raw, err)
		}
		if changes := planIs(t, dir, exitOK); len(changes) != 0 {
			t.Errorf("the plan after setLabels proposes %v", changes)
		}
	})

	// The patchable: list, through infrena: description is a PATCH carrying
	// the fingerprint the forwarding rule advises, not a replacement.
	t.Run("a_description_change_is_a_patch", func(t *testing.T) {
		write(t, dir, "infrena.yml", cfgDesc("map_b", "b", "    description: patched by infrena\n"))
		changes := planIs(t, dir, exitChanges)
		if len(changes) != 1 || changes[0].Kind != "update" {
			t.Fatalf("a description change on the global forwarding rule plans %v; it must be one UPDATE", changes)
		}
		applyOK(t, dir, "patch description")
		var rule struct {
			Description string `json:"description"`
		}
		code, raw, err := g.get(t.Context(), paths[0].url)
		if err == nil && code == http.StatusOK {
			err = json.Unmarshal(raw, &rule)
		}
		if err != nil || rule.Description != "patched by infrena" {
			t.Errorf("google's forwarding rule does not carry the new description: %d %s %v", code, raw, err)
		}
		if changes := planIs(t, dir, exitOK); len(changes) != 0 {
			t.Errorf("the plan after the description patch proposes %v", changes)
		}
	})

	// A question for Google, not a check of infrena: the forwarding rule's
	// patch says "Currently, you can only patch the network_tier field", and
	// Terraform patches other fields anyway. A patch of description is the
	// cheapest field to ask about. Logged, not asserted, because either
	// answer is a fact to record.
	t.Run("probe_does_a_forwarding_rule_patch_take_description", func(t *testing.T) {
		code, raw, err := g.get(t.Context(), paths[0].url)
		var rule struct {
			Fingerprint string `json:"fingerprint"`
		}
		if err != nil || code != http.StatusOK || json.Unmarshal(raw, &rule) != nil {
			t.Fatalf("reading the rule: %d %s %v", code, raw, err)
		}
		status, opErr, answer := g.computeOp(t, t.Context(), http.MethodPatch, paths[0].url,
			map[string]any{"description": "patched by the live probe", "fingerprint": rule.Fingerprint})
		t.Logf("PROBE forwarding rule PATCH description: status %d, operation error %q, answer %s", status, opErr, answer)
		_, raw, _ = g.get(t.Context(), paths[0].url)
		var after struct {
			Description string `json:"description"`
		}
		_ = json.Unmarshal(raw, &after)
		t.Logf("PROBE forwarding rule description after the patch: %q", after.Description)
	})

	t.Run("destroy_removes_everything", func(t *testing.T) {
		write(t, dir, "infrena.yml", cut(readFile(t, dir, "infrena.yml"), "resources:"))
		applyOK(t, dir, "destroy")
	})
}

// TestLiveAddressLabels is setLabels on a type with no patch at all, where a
// label change used to REPLACE the address and hand back a different IP.
// The probe first asks Google whether an insert keeps labels, which decides
// whether infrena's create had to call setLabels itself.
func TestLiveAddressLabels(t *testing.T) {
	project, sa := guard(t)
	n := newNames()
	g := newGoogle(t, project, sa)
	id := "infrena-addr-" + n.run
	base := "projects/" + project + "/regions/" + region + "/addresses/"
	url := g.computeURL(base + id)
	probeURL := g.computeURL(base + id + "-probe")
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(context.Background()), 10*time.Minute)
		defer cancel()
		g.deleteAndWait(t, ctx, "gcp.address", url, url)
		g.deleteAndWait(t, ctx, "gcp.address", probeURL, probeURL)
	})

	t.Run("probe_does_an_insert_keep_labels", func(t *testing.T) {
		status, opErr, _ := g.computeOp(t, t.Context(), http.MethodPost,
			g.computeURL("projects/"+project+"/regions/"+region+"/addresses"),
			map[string]any{"name": id + "-probe", "labels": map[string]string{"infrena-live": "true"}})
		if status >= 300 || opErr != "" {
			t.Fatalf("inserting the probe address: %d %s", status, opErr)
		}
		_, raw, _ := g.get(t.Context(), probeURL)
		var got struct {
			Labels map[string]string `json:"labels"`
		}
		_ = json.Unmarshal(raw, &got)
		t.Logf("PROBE address insert with labels: Google holds labels %v", got.Labels)
	})

	cfg := func(team string) string {
		return fmt.Sprintf(`project: infrena-gcp-live-addr
%sresources:
  ip:
    type: gcp.address
    name: %s
    labels:
      infrena-live: "true"
      team: %s
`, liveProvider(project, sa), id, team)
	}
	dir := t.TempDir()
	write(t, dir, "infrena.yml", cfg("a"))
	applyOK(t, dir, "create")
	if changes := planIs(t, dir, exitOK); len(changes) != 0 {
		t.Errorf("the plan after creating the address proposes %v; its labels did not survive the create", changes)
	}
	var before struct {
		Address string `json:"address"`
	}
	_, raw, _ := g.get(t.Context(), url)
	_ = json.Unmarshal(raw, &before)

	write(t, dir, "infrena.yml", cfg("b"))
	changes := planIs(t, dir, exitChanges)
	if len(changes) != 1 || changes[0].Kind != "update" {
		t.Fatalf("a label change on an address plans %v; it must be an UPDATE, a replace changes its IP", changes)
	}
	applyOK(t, dir, "setLabels")
	var after struct {
		Address string            `json:"address"`
		Labels  map[string]string `json:"labels"`
	}
	code, raw, err := g.get(t.Context(), url)
	if err == nil && code == http.StatusOK {
		err = json.Unmarshal(raw, &after)
	}
	if err != nil || after.Labels["team"] != "b" {
		t.Errorf("google's address does not carry team=b: %d %s %v", code, raw, err)
	}
	if before.Address == "" || after.Address != before.Address {
		t.Errorf("the address's IP went from %q to %q across a label change", before.Address, after.Address)
	}
	if changes := planIs(t, dir, exitOK); len(changes) != 0 {
		t.Errorf("the plan after setLabels proposes %v", changes)
	}

	write(t, dir, "infrena.yml", cut(readFile(t, dir, "infrena.yml"), "resources:"))
	applyOK(t, dir, "destroy")
}

// TestLiveSslCertificateKeepsItsPrivateKey. Google never returns an SSL
// certificate's privateKey, and the field is required and ForceNew. Until
// magic-modules' ignore_read was read, every plan after a create saw it as
// removed and proposed replacing the certificate.
func TestLiveSslCertificateKeepsItsPrivateKey(t *testing.T) {
	project, sa := guard(t)
	n := newNames()
	g := newGoogle(t, project, sa)
	id := "infrena-cert-" + n.run
	url := g.computeURL("projects/" + project + "/global/sslCertificates/" + id)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(context.Background()), 5*time.Minute)
		defer cancel()
		g.deleteAndWait(t, ctx, "gcp.sslcertificate", url, url)
	})

	certPEM, keyPEM := selfSigned(t)
	dir := t.TempDir()
	write(t, dir, "cert.pem", certPEM)
	write(t, dir, "key.pem", keyPEM)
	write(t, dir, "infrena.yml", fmt.Sprintf(`project: infrena-gcp-live-cert
%sresources:
  cert:
    type: gcp.sslcertificate
    name: %s
    certificate: %q
    privateKey: %q
`, liveProvider(project, sa), id, certPEM, keyPEM))
	applyOK(t, dir, "create")
	if changes := planIs(t, dir, exitOK); len(changes) != 0 {
		t.Errorf("the plan after creating the certificate proposes %v; the private key Google never "+
			"returns is reading as removed", changes)
	}
	write(t, dir, "infrena.yml", cut(readFile(t, dir, "infrena.yml"), "resources:"))
	applyOK(t, dir, "destroy")
}

func selfSigned(t *testing.T) (certPEM, keyPEM string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: "infrena-live.example.com"},
		DNSNames:     []string{"infrena-live.example.com"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	kd, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})),
		string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kd}))
}

// TestLiveProbes asks Google two questions this provider's rulings depend on
// and nothing else answers. Both are logged: each answer is a fact to write
// down, and the follow-up that asked for it says what to change either way.
func TestLiveProbes(t *testing.T) {
	project, sa := guard(t)
	n := newNames()
	g := newGoogle(t, project, sa)

	// Does a subnetwork patch take logConfig? magic-modules patches it with
	// the fingerprint; Google's field description says nothing, so today it
	// is not on the subnetwork's patchable: list and a change replaces it.
	t.Run("subnetwork_logConfig_patch", func(t *testing.T) {
		net := "infrena-probe-" + n.run
		netURL := g.computeURL("projects/" + project + "/global/networks/" + net)
		subURL := g.computeURL("projects/" + project + "/regions/" + region + "/subnetworks/" + net)
		t.Cleanup(func() {
			ctx, cancel := context.WithTimeout(context.WithoutCancel(context.Background()), 10*time.Minute)
			defer cancel()
			g.deleteAndWait(t, ctx, "gcp.subnetwork", subURL, subURL)
			g.deleteAndWait(t, ctx, "gcp.network", netURL, netURL)
		})
		if s, e, _ := g.computeOp(t, t.Context(), http.MethodPost, g.computeURL("projects/"+project+"/global/networks"),
			map[string]any{"name": net, "autoCreateSubnetworks": false}); s >= 300 || e != "" {
			t.Fatalf("creating the probe network: %d %s", s, e)
		}
		if s, e, _ := g.computeOp(t, t.Context(), http.MethodPost, g.computeURL("projects/"+project+"/regions/"+region+"/subnetworks"),
			map[string]any{"name": net, "network": netURL, "ipCidrRange": "10.186.0.0/24"}); s >= 300 || e != "" {
			t.Fatalf("creating the probe subnetwork: %d %s", s, e)
		}
		_, raw, _ := g.get(t.Context(), subURL)
		var sub struct {
			Fingerprint string `json:"fingerprint"`
		}
		_ = json.Unmarshal(raw, &sub)
		status, opErr, answer := g.computeOp(t, t.Context(), http.MethodPatch, subURL, map[string]any{
			"fingerprint": sub.Fingerprint,
			"logConfig":   map[string]any{"enable": true, "aggregationInterval": "INTERVAL_10_MIN"},
		})
		t.Logf("PROBE subnetwork PATCH logConfig: status %d, operation error %q, answer %s", status, opErr, answer)
		_, raw, _ = g.get(t.Context(), subURL)
		var after struct {
			LogConfig map[string]any `json:"logConfig"`
		}
		_ = json.Unmarshal(raw, &after)
		t.Logf("PROBE subnetwork logConfig after the patch: %v", after.LogConfig)
	})

	// Is iam.googleapis.com/ServiceAccount an asset type Cloud Asset
	// Inventory answers for? gcp.serviceaccount's discovery depends on it.
	// The suite's own service account lives in the project, so a real type
	// finds at least that one.
	t.Run("serviceaccount_asset_type", func(t *testing.T) {
		url := "https://cloudasset.googleapis.com/v1/projects/" + project +
			":searchAllResources?assetTypes=iam.googleapis.com/ServiceAccount&query=" +
			urlQueryEscape("name:"+strings.TrimSuffix(os.Getenv(saEnv), "@"+project+".iam.gserviceaccount.com"))
		code, raw, err := g.get(t.Context(), url)
		if err != nil || code != http.StatusOK {
			t.Fatalf("searching cloud asset inventory: %d %s %v", code, raw, err)
		}
		var res struct {
			Results []struct {
				Name      string `json:"name"`
				AssetType string `json:"assetType"`
			} `json:"results"`
		}
		_ = json.Unmarshal(raw, &res)
		t.Logf("PROBE service account assets found: %+v", res.Results)
		if len(res.Results) == 0 {
			t.Errorf("cloud asset inventory finds no iam.googleapis.com/ServiceAccount for the suite's own "+
				"account in %s, so gcp.serviceaccount's discovery cannot find one either", project)
		}
	})
}
