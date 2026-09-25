//go:build live

package live

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"
)

// TestLiveHealthChecks. magic-modules marks a health check's `type` output,
// because Terraform's encoder derives it, and until 2026-09-24 the generator
// obeyed that: the one field Google requires could not be written, and every
// health check create failed. The ruling now makes it settable and required.
// Global and regional, created, changed in place, and destroyed.
func TestLiveHealthChecks(t *testing.T) {
	project, sa := guard(t)
	n := newNames()
	g := newGoogle(t, project, sa)
	name := "infrena-hc-" + n.run
	globalURL := g.computeURL("projects/" + project + "/global/healthChecks/" + name)
	regionalURL := g.computeURL("projects/" + project + "/regions/" + region + "/healthChecks/" + name + "-r")
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(context.Background()), 10*time.Minute)
		defer cancel()
		g.deleteAndWait(t, ctx, "gcp.healthcheck", globalURL, globalURL)
		g.deleteAndWait(t, ctx, "gcp.regionhealthcheck", regionalURL, regionalURL)
	})
	cfg := func(interval int) string {
		return fmt.Sprintf(`project: infrena-gcp-live-hc
%[1]sresources:
  global:
    type: gcp.healthcheck
    name: %[2]s
    type_value: TCP
    checkIntervalSec: %[3]d
    tcpHealthCheck:
      port: 80
  regional:
    type: gcp.regionhealthcheck
    name: %[2]s-r
    type_value: HTTP
    checkIntervalSec: %[3]d
    httpHealthCheck:
      port: 8080
      requestPath: /healthz
`, liveProvider(project, sa), name, interval)
	}
	dir := t.TempDir()
	write(t, dir, "infrena.yml", cfg(5))
	applyOK(t, dir, "create")
	for _, u := range []string{globalURL, regionalURL} {
		if code, raw, err := g.get(t.Context(), u); err != nil || code != http.StatusOK {
			t.Errorf("reading %s after the create: %d %s %v", u, code, raw, err)
		}
	}
	if changes := planIs(t, dir, exitOK); len(changes) != 0 {
		t.Errorf("the plan after creating both health checks proposes %v", changes)
	}

	write(t, dir, "infrena.yml", cfg(10))
	changes := planIs(t, dir, exitChanges)
	if len(changes) != 2 || changes[0].Kind != "update" || changes[1].Kind != "update" {
		t.Fatalf("an interval change plans %v, want two updates", changes)
	}
	applyOK(t, dir, "update")
	var got struct {
		CheckIntervalSec int `json:"checkIntervalSec"`
	}
	_, raw, _ := g.get(t.Context(), globalURL)
	if json.Unmarshal(raw, &got) != nil || got.CheckIntervalSec != 10 {
		t.Errorf("google's global health check does not carry the new interval: %s", raw)
	}
	if changes := planIs(t, dir, exitOK); len(changes) != 0 {
		t.Errorf("the plan after the update proposes %v", changes)
	}

	write(t, dir, "infrena.yml", cut(readFile(t, dir, "infrena.yml"), "resources:"))
	applyOK(t, dir, "destroy")
}

// TestLiveArtifactRegistryCredentialsChangeInPlace. magic-modules marks a
// remote repository's whole remoteRepositoryConfig immutable, while
// Terraform updates the upstream credentials in place. Obeyed, changing the
// upstream username REPLACED the repository, deleting every artifact in it.
// The ruling's in_place makes the credentials an update. The repository's
// createTime proves it is the same repository afterwards.
func TestLiveArtifactRegistryCredentialsChangeInPlace(t *testing.T) {
	project, sa := guard(t)
	n := newNames()
	g := newGoogle(t, project, sa)
	repo := "infrena-ar-" + n.run
	secret := "infrena-ar-pw-" + n.run
	repoURL := "https://artifactregistry.googleapis.com/v1/projects/" + project + "/locations/" + region + "/repositories/" + repo
	secretURL := "https://secretmanager.googleapis.com/v1/projects/" + project + "/secrets/" + secret
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(context.Background()), 10*time.Minute)
		defer cancel()
		g.deleteAndWait(t, ctx, "gcp.artifactregistry.repository", repoURL, repoURL)
		g.deleteAndWait(t, ctx, "secret", secretURL, secretURL)
	})

	// The password lives in Secret Manager; the repository names a version.
	// Seeded directly: the secret is not what this test is about.
	code, raw, _, err := g.doRetry(t.Context(), http.MethodPost,
		"https://secretmanager.googleapis.com/v1/projects/"+project+"/secrets?secretId="+secret,
		map[string]any{"replication": map[string]any{"automatic": map[string]any{}}})
	if err != nil || code >= 300 {
		t.Fatalf("seeding the password secret: %d %s %v", code, raw, err)
	}
	code, raw, _, err = g.doRetry(t.Context(), http.MethodPost, secretURL+":addVersion",
		map[string]any{"payload": map[string]any{"data": base64.StdEncoding.EncodeToString([]byte("not-a-real-password"))}})
	if err != nil || code >= 300 {
		t.Fatalf("seeding the password version: %d %s %v", code, raw, err)
	}
	// Artifact Registry reads the password as its own service agent, even
	// with upstream validation off, and refuses the create without access.
	// Granted on this one secret, not the project.
	number, err := g.projectNumber(t.Context(), project)
	if err != nil {
		t.Fatalf("reading the project number: %v", err)
	}
	agent := "serviceAccount:service-" + number + "@gcp-sa-artifactregistry.iam.gserviceaccount.com"
	code, raw, _, err = g.doRetry(t.Context(), http.MethodPost, secretURL+":setIamPolicy",
		map[string]any{"policy": map[string]any{"bindings": []any{map[string]any{
			"role": "roles/secretmanager.secretAccessor", "members": []string{agent},
		}}}})
	if err != nil || code >= 300 {
		t.Fatalf("granting the Artifact Registry agent access to the secret: %d %s %v", code, raw, err)
	}
	// A grant takes a while to reach the service that checks it.
	time.Sleep(90 * time.Second)
	// Written as a user would, with the project id; Google may answer with
	// the number.
	version := "projects/" + project + "/secrets/" + secret + "/versions/1"

	cfg := func(user string) string {
		return fmt.Sprintf(`project: infrena-gcp-live-ar
%[1]sresources:
  repo:
    type: gcp.artifactregistry.repository
    repository_id: %[2]s
    format: DOCKER
    mode: REMOTE_REPOSITORY
    remoteRepositoryConfig:
      disableUpstreamValidation: true
      dockerRepository:
        customRepository:
          uri: https://registry-1.docker.io
      upstreamCredentials:
        usernamePasswordCredentials:
          username: %[3]s
          passwordSecretVersion: %[4]s
`, liveProvider(project, sa), repo, user, version)
	}
	dir := t.TempDir()
	write(t, dir, "infrena.yml", cfg("user-a"))
	applyOK(t, dir, "create")
	created := func() (string, string) {
		var got struct {
			CreateTime             string `json:"createTime"`
			RemoteRepositoryConfig struct {
				UpstreamCredentials struct {
					UsernamePasswordCredentials struct {
						Username string `json:"username"`
					} `json:"usernamePasswordCredentials"`
				} `json:"upstreamCredentials"`
			} `json:"remoteRepositoryConfig"`
		}
		code, raw, err := g.get(t.Context(), repoURL)
		if err != nil || code != http.StatusOK || json.Unmarshal(raw, &got) != nil {
			t.Fatalf("reading the repository: %d %s %v", code, raw, err)
		}
		t.Logf("google holds: %s", raw)
		return got.CreateTime, got.RemoteRepositoryConfig.UpstreamCredentials.UsernamePasswordCredentials.Username
	}
	before, _ := created()
	if changes := planIs(t, dir, exitOK); len(changes) != 0 {
		t.Errorf("the plan after creating the repository proposes %v", changes)
	}

	write(t, dir, "infrena.yml", cfg("user-b"))
	changes := planIs(t, dir, exitChanges)
	if len(changes) != 1 || changes[0].Kind != "update" {
		t.Fatalf("an upstream username change plans %v; it must be an UPDATE, a replace deletes every artifact", changes)
	}
	applyOK(t, dir, "update")
	after, user := created()
	if user != "user-b" {
		t.Errorf("google's repository has username %q, want user-b", user)
	}
	if before == "" || after != before {
		t.Errorf("the repository's createTime went from %q to %q: it was replaced", before, after)
	}
	if changes := planIs(t, dir, exitOK); len(changes) != 0 {
		t.Errorf("the plan after the update proposes %v", changes)
	}

	write(t, dir, "infrena.yml", cut(readFile(t, dir, "infrena.yml"), "resources:"))
	applyOK(t, dir, "destroy")
}

// TestLiveBigQueryTable asks whether gcp.bigquery.table creates at all. Its
// create url comes from magic-modules' base_url, which is the table's own
// ITEM path (Terraform implements the table by hand and never runs that
// YAML); tables.insert is a POST to the collection. The fake round trip
// cannot answer this; only Google can.
func TestLiveBigQueryTable(t *testing.T) {
	project, sa := guard(t)
	n := newNames()
	g := newGoogle(t, project, sa)
	ds := "infrena_ds_" + n.run
	table := "infrena_t_" + n.run
	dsURL := "https://bigquery.googleapis.com/bigquery/v2/projects/" + project + "/datasets/" + ds
	tableURL := dsURL + "/tables/" + table
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(context.Background()), 5*time.Minute)
		defer cancel()
		g.deleteAndWait(t, ctx, "gcp.bigquery.table", tableURL, tableURL)
		g.deleteAndWait(t, ctx, "gcp.dataset", dsURL, dsURL+"?deleteContents=true")
	})
	cfg := func(desc string) string {
		return fmt.Sprintf(`project: infrena-gcp-live-bq
%[1]sresources:
  ds:
    type: gcp.dataset
    datasetReference:
      datasetId: %[2]s
    location: US
  table:
    type: gcp.bigquery.table
    tableReference:
      datasetId: ${ds.datasetReference.datasetId}
      tableId: %[3]s
    description: %[4]s
    schema:
      fields:
        - name: a
          type: STRING
`, liveProvider(project, sa), ds, table, desc)
	}
	dir := t.TempDir()
	write(t, dir, "infrena.yml", cfg("created by the infrena live suite"))
	applyOK(t, dir, "create")
	if code, raw, err := g.get(t.Context(), tableURL); err != nil || code != http.StatusOK {
		t.Fatalf("the table is not at its own url after the create: %d %s %v", code, raw, err)
	}
	if changes := planIs(t, dir, exitOK); len(changes) != 0 {
		t.Errorf("the plan after creating the table proposes %v", changes)
	}

	write(t, dir, "infrena.yml", cfg("changed by the infrena live suite"))
	changes := planIs(t, dir, exitChanges)
	if len(changes) != 1 || changes[0].Kind != "update" {
		t.Fatalf("a table description change plans %v, want one update", changes)
	}
	applyOK(t, dir, "update")
	if changes := planIs(t, dir, exitOK); len(changes) != 0 {
		t.Errorf("the plan after the update proposes %v", changes)
	}

	write(t, dir, "infrena.yml", cut(readFile(t, dir, "infrena.yml"), "  table:"))
	applyOK(t, dir, "destroy the table")
	write(t, dir, "infrena.yml", cut(readFile(t, dir, "infrena.yml"), "resources:"))
	applyOK(t, dir, "destroy the dataset")
}

// TestLiveProbeAddressInsertLabels asks, for a regional and a global address
// in the same run, whether an insert keeps the labels it was sent. Our run
// on 2026-09-24 saw a regional insert drop them; a recorded Config Connector
// log shows one keeping them. Logged, not asserted: the answer decides
// whether the create-time setLabels is still needed, and for which.
func TestLiveProbeAddressInsertLabels(t *testing.T) {
	project, sa := guard(t)
	n := newNames()
	g := newGoogle(t, project, sa)
	for _, c := range []struct{ what, collection string }{
		{"regional", "projects/" + project + "/regions/" + region + "/addresses"},
		{"global", "projects/" + project + "/global/addresses"},
	} {
		name := "infrena-probe-" + c.what + "-" + n.run
		url := g.computeURL(c.collection + "/" + name)
		status, opErr, _ := g.computeOp(t, t.Context(), http.MethodPost, g.computeURL(c.collection),
			map[string]any{"name": name, "labels": map[string]string{"infrena-live": "true"}})
		if status < 300 && opErr == "" {
			_, raw, _ := g.get(t.Context(), url)
			var got struct {
				Labels           map[string]string `json:"labels"`
				LabelFingerprint string            `json:"labelFingerprint"`
			}
			_ = json.Unmarshal(raw, &got)
			t.Logf("PROBE %s address insert with labels: Google holds labels %v (fingerprint %s)",
				c.what, got.Labels, got.LabelFingerprint)
		} else {
			t.Errorf("inserting the %s probe address: %d %s", c.what, status, opErr)
		}
		ctx, cancel := context.WithTimeout(context.WithoutCancel(context.Background()), 5*time.Minute)
		g.deleteAndWait(t, ctx, "address", url, url)
		cancel()
	}
}
