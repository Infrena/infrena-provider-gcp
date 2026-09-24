//go:build live

package live

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"
)

// TestLiveSecrets is Secret Manager on real Google: a global secret, and a
// regional one, which exists only at its region's own endpoint
// (secretmanager.<location>.rep.googleapis.com). The regional secret is read
// back THROUGH THAT HOST here, independently of the provider, so a secret
// created somewhere else fails this rather than passing by accident. Both
// take a label change as an update, not a replacement.
func TestLiveSecrets(t *testing.T) {
	project, sa := guard(t)
	n := newNames()
	g := newGoogle(t, project, sa)
	id := "infrena-secret-" + n.run
	globalURL := "https://secretmanager.googleapis.com/v1/projects/" + project + "/secrets/" + id
	regionalURL := "https://secretmanager." + region + ".rep.googleapis.com/v1/projects/" + project +
		"/locations/" + region + "/secrets/" + id + "-r"
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(context.Background()), 5*time.Minute)
		defer cancel()
		g.deleteAndWait(t, ctx, "gcp.secret", globalURL, globalURL)
		g.deleteAndWait(t, ctx, "gcp.regionalsecret", regionalURL, regionalURL)
	})

	cfg := func(team string) string {
		return fmt.Sprintf(`project: infrena-gcp-live-secrets
%[1]sresources:
  global:
    type: gcp.secret
    secret_id: %[2]s
    replication:
      automatic: {}
    labels:
      infrena-live: "true"
      team: %[3]s
  regional:
    type: gcp.regionalsecret
    secret_id: %[2]s-r
    labels:
      infrena-live: "true"
      team: %[3]s
`, liveProvider(project, sa), id, team)
	}
	dir := t.TempDir()
	write(t, dir, "infrena.yml", cfg("a"))
	applyOK(t, dir, "create")

	labels := func(url string) map[string]string {
		var got struct {
			Labels map[string]string `json:"labels"`
		}
		code, raw, err := g.get(t.Context(), url)
		if err == nil && code == http.StatusOK {
			err = json.Unmarshal(raw, &got)
		}
		if err != nil || code != http.StatusOK {
			t.Errorf("reading %s: %d %s %v", url, code, raw, err)
		}
		return got.Labels
	}
	if labels(regionalURL)["team"] != "a" {
		t.Fatal("the regional secret is not at its region's endpoint")
	}
	if changes := planIs(t, dir, exitOK); len(changes) != 0 {
		t.Errorf("the plan after creating both secrets proposes %v", changes)
	}

	write(t, dir, "infrena.yml", cfg("b"))
	changes := planIs(t, dir, exitChanges)
	if len(changes) != 2 {
		t.Fatalf("a label change on both secrets plans %v, want two updates", changes)
	}
	for _, c := range changes {
		if c.Kind != "update" {
			t.Errorf("%s plans a %s for a label change: %v", c.Address, c.Kind, c.Reasons)
		}
	}
	applyOK(t, dir, "relabel")
	for what, url := range map[string]string{"global": globalURL, "regional": regionalURL} {
		if labels(url)["team"] != "b" {
			t.Errorf("the %s secret does not carry team=b after the update", what)
		}
	}
	if changes := planIs(t, dir, exitOK); len(changes) != 0 {
		t.Errorf("the plan after the label change proposes %v", changes)
	}

	write(t, dir, "infrena.yml", cut(readFile(t, dir, "infrena.yml"), "resources:"))
	applyOK(t, dir, "destroy")
}
