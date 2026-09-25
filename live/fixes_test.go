//go:build live

package live

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

// createTimeOf reads a resource's createTime straight from Google, which is
// how an update is told from a replacement: a replaced resource is a new one.
func (g *google) createTimeOf(t *testing.T, url string) string {
	t.Helper()
	var got struct {
		CreateTime        string `json:"createTime"`
		CreationTimestamp string `json:"creationTimestamp"`
	}
	code, raw, err := g.get(t.Context(), url)
	if err != nil || code != http.StatusOK || json.Unmarshal(raw, &got) != nil {
		t.Fatalf("reading %s: %d %s %v", url, code, raw, err)
	}
	if got.CreateTime != "" {
		return got.CreateTime
	}
	return got.CreationTimestamp
}

// updateInPlace changes configuration from before to after and requires
// exactly one update, a clean plan afterwards, and the same resource.
func updateInPlace(t *testing.T, g *google, dir, url, after string) {
	t.Helper()
	born := g.createTimeOf(t, url)
	write(t, dir, "infrena.yml", after)
	changes := planIs(t, dir, exitChanges)
	if len(changes) != 1 || changes[0].Kind != "update" {
		t.Fatalf("the change plans %v, want one update", changes)
	}
	applyOK(t, dir, "update")
	if now := g.createTimeOf(t, url); born == "" || now != born {
		t.Errorf("createTime went from %q to %q: the resource was replaced, not updated", born, now)
	}
	if changes := planIs(t, dir, exitOK); len(changes) != 0 {
		t.Errorf("the plan after the update proposes %v", changes)
	}
}

// TestLiveBigtableInstanceUpdatesInPlace. Bigtable publishes its instance
// PATCH as partialUpdateInstance, and the generator looked only for a method
// named patch, so every change REPLACED the instance, deleting every table
// in it (found 2026-09-25). One HDD node for a few minutes.
func TestLiveBigtableInstanceUpdatesInPlace(t *testing.T) {
	project, sa := guard(t)
	n := newNames()
	g := newGoogle(t, project, sa)
	id := "infrena-bt-" + n.run
	url := "https://bigtableadmin.googleapis.com/v2/projects/" + project + "/instances/" + id
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(context.Background()), 10*time.Minute)
		defer cancel()
		g.deleteAndWait(t, ctx, "gcp.bigtableadmin.instance", url, url)
	})
	cfg := func(team string) string {
		return fmt.Sprintf(`project: infrena-gcp-live-bt
%[1]sresources:
  bt:
    type: gcp.bigtableadmin.instance
    parent: projects/%[2]s
    instanceId: %[3]s
    displayName: infrena bt %[4]s
    labels:
      team: %[4]s
    clusters:
      %[3]s-c1:
        location: projects/%[2]s/locations/%[5]s-b
        serveNodes: 1
        defaultStorageType: HDD
`, liveProvider(project, sa), project, id, team, region)
	}
	dir := t.TempDir()
	write(t, dir, "infrena.yml", cfg("a"))
	applyOK(t, dir, "create")
	if changes := planIs(t, dir, exitOK); len(changes) != 0 {
		t.Errorf("the plan after creating the instance proposes %v", changes)
	}
	updateInPlace(t, g, dir, url, cfg("b"))
	write(t, dir, "infrena.yml", cut(readFile(t, dir, "infrena.yml"), "resources:"))
	applyOK(t, dir, "destroy")
}

// TestLiveTrustConfigUpdates. magic-modules declares the PATCH with no
// update_mask, and Certificate Manager's updateMask is "Required.", so
// every update went without one (found 2026-09-25).
func TestLiveTrustConfigUpdates(t *testing.T) {
	project, sa := guard(t)
	n := newNames()
	g := newGoogle(t, project, sa)
	name := "infrena-tc-" + n.run
	url := "https://certificatemanager.googleapis.com/v1/projects/" + project + "/locations/" + region + "/trustConfigs/" + name
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(context.Background()), 5*time.Minute)
		defer cancel()
		g.deleteAndWait(t, ctx, "gcp.trustconfig", url, url)
	})
	cert, _ := selfSigned(t)
	cfg := func(desc string) string {
		return fmt.Sprintf(`project: infrena-gcp-live-tc
%[1]sresources:
  tc:
    type: gcp.trustconfig
    name: %[2]s
    description: %[3]s
    allowlistedCertificates:
      - pemCertificate: %[4]q
`, liveProvider(project, sa), name, desc, cert)
	}
	dir := t.TempDir()
	write(t, dir, "infrena.yml", cfg("created by the infrena live suite"))
	applyOK(t, dir, "create")
	if changes := planIs(t, dir, exitOK); len(changes) != 0 {
		t.Errorf("the plan after creating the trust config proposes %v", changes)
	}
	updateInPlace(t, g, dir, url, cfg("changed by the infrena live suite"))
	write(t, dir, "infrena.yml", cut(readFile(t, dir, "infrena.yml"), "resources:"))
	applyOK(t, dir, "destroy")
}

// cmekKey is one key ring and one key under fixed names, created once and
// reused by every run: a key ring can never be deleted, and a key version
// costs until destroyed, so a new one per run would accumulate. Compute's
// service agent is granted use of the key.
func (g *google) cmekKey(t *testing.T, project string) string {
	t.Helper()
	base := "https://cloudkms.googleapis.com/v1/projects/" + project + "/locations/global"
	ring := base + "/keyRings?keyRingId=infrena-live"
	if code, raw, _, err := g.doRetry(t.Context(), http.MethodPost, ring, map[string]any{}); err != nil || (code >= 300 && code != http.StatusConflict) {
		t.Fatalf("creating the key ring: %d %s %v", code, raw, err)
	}
	keyURL := base + "/keyRings/infrena-live/cryptoKeys"
	if code, raw, _, err := g.doRetry(t.Context(), http.MethodPost, keyURL+"?cryptoKeyId=infrena-live-cmek",
		map[string]any{"purpose": "ENCRYPT_DECRYPT"}); err != nil || (code >= 300 && code != http.StatusConflict) {
		t.Fatalf("creating the key: %d %s %v", code, raw, err)
	}
	number, err := g.projectNumber(t.Context(), project)
	if err != nil {
		t.Fatalf("reading the project number: %v", err)
	}
	agent := "serviceAccount:service-" + number + "@compute-system.iam.gserviceaccount.com"
	if code, raw, _, err := g.doRetry(t.Context(), http.MethodPost, keyURL+"/infrena-live-cmek:setIamPolicy",
		map[string]any{"policy": map[string]any{"bindings": []any{map[string]any{
			"role": "roles/cloudkms.cryptoKeyEncrypterDecrypter", "members": []string{agent},
		}}}}); err != nil || code >= 300 {
		t.Fatalf("granting compute the key: %d %s %v", code, raw, err)
	}
	return "projects/" + project + "/locations/global/keyRings/infrena-live/cryptoKeys/infrena-live-cmek"
}

// TestLiveCMEKImagePlansClean. Google answers a customer-managed key name
// with the key version appended, and imageEncryptionKey is immutable: until
// the kms_key equivalence, a CMEK image planned its own replacement on every
// run (found 2026-09-25).
func TestLiveCMEKImagePlansClean(t *testing.T) {
	project, sa := guard(t)
	n := newNames()
	g := newGoogle(t, project, sa)
	key := g.cmekKey(t, project)
	// A grant takes a while to reach compute.
	time.Sleep(60 * time.Second)
	name := "infrena-cmek-" + n.run
	url := g.computeURL("projects/" + project + "/global/images/" + name)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(context.Background()), 10*time.Minute)
		defer cancel()
		g.deleteAndWait(t, ctx, "gcp.image", url, url)
	})
	dir := t.TempDir()
	write(t, dir, "infrena.yml", fmt.Sprintf(`project: infrena-gcp-live-cmek
%sresources:
  img:
    type: gcp.image
    name: %s
    sourceImage: projects/debian-cloud/global/images/family/debian-12
    imageEncryptionKey:
      kmsKeyName: %s
`, liveProvider(project, sa), name, key))
	applyOK(t, dir, "create")
	var got struct {
		ImageEncryptionKey struct {
			KmsKeyName string `json:"kmsKeyName"`
		} `json:"imageEncryptionKey"`
	}
	_, raw, _ := g.get(t.Context(), url)
	_ = json.Unmarshal(raw, &got)
	t.Logf("PROBE Google answers imageEncryptionKey.kmsKeyName %q for %q", got.ImageEncryptionKey.KmsKeyName, key)
	if changes := planIs(t, dir, exitOK); len(changes) != 0 {
		t.Errorf("the plan after creating a CMEK image proposes %v", changes)
	}
	write(t, dir, "infrena.yml", cut(readFile(t, dir, "infrena.yml"), "resources:"))
	applyOK(t, dir, "destroy")
}

// TestLiveWorkflow is gcp.workflow, shipped 2026-09-25.
func TestLiveWorkflowType(t *testing.T) {
	project, sa := guard(t)
	n := newNames()
	g := newGoogle(t, project, sa)
	name := "infrena-wf-" + n.run
	url := "https://workflows.googleapis.com/v1/projects/" + project + "/locations/" + region + "/workflows/" + name
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(context.Background()), 5*time.Minute)
		defer cancel()
		g.deleteAndWait(t, ctx, "gcp.workflow", url, url)
	})
	cfg := func(desc string) string {
		return fmt.Sprintf(`project: infrena-gcp-live-wf
%[1]sresources:
  wf:
    type: gcp.workflow
    name: %[2]s
    description: %[3]s
    serviceAccount: %[4]s
    sourceContents: |
      main:
        steps:
          - done:
              return: "ok"
`, liveProvider(project, sa), name, desc, sa)
	}
	dir := t.TempDir()
	write(t, dir, "infrena.yml", cfg("created by the infrena live suite"))
	applyOK(t, dir, "create")
	if changes := planIs(t, dir, exitOK); len(changes) != 0 {
		t.Errorf("the plan after creating the workflow proposes %v", changes)
	}
	updateInPlace(t, g, dir, url, cfg("changed by the infrena live suite"))
	write(t, dir, "infrena.yml", cut(readFile(t, dir, "infrena.yml"), "resources:"))
	applyOK(t, dir, "destroy")
}

// TestLiveLogScope is gcp.logscope, shipped 2026-09-25. Log scopes live in
// the global location, which this provider takes from the instance's region.
func TestLiveLogScope(t *testing.T) {
	project, sa := guard(t)
	n := newNames()
	g := newGoogle(t, project, sa)
	id := "infrena-ls-" + n.run
	url := "https://logging.googleapis.com/v2/projects/" + project + "/locations/global/logScopes/" + id
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(context.Background()), 5*time.Minute)
		defer cancel()
		g.deleteAndWait(t, ctx, "gcp.logscope", url, url)
	})
	provider := strings.Replace(liveProvider(project, sa), "region: "+region, "region: global", 1)
	cfg := func(desc string) string {
		return fmt.Sprintf(`project: infrena-gcp-live-ls
%[1]sresources:
  ls:
    type: gcp.logscope
    logScopeId: %[2]s
    description: %[3]s
    resourceNames:
      - projects/%[4]s
`, provider, id, desc, project)
	}
	dir := t.TempDir()
	write(t, dir, "infrena.yml", cfg("created by the infrena live suite"))
	applyOK(t, dir, "create")
	if changes := planIs(t, dir, exitOK); len(changes) != 0 {
		t.Errorf("the plan after creating the log scope proposes %v", changes)
	}
	updateInPlace(t, g, dir, url, cfg("changed by the infrena live suite"))
	write(t, dir, "infrena.yml", cut(readFile(t, dir, "infrena.yml"), "resources:"))
	applyOK(t, dir, "destroy")
}

// TestLiveRedisInstance is gcp.redis.instance, shipped 2026-09-25: a 1 GB
// BASIC instance, a label change in place, and a destroy.
func TestLiveRedisInstance(t *testing.T) {
	project, sa := guard(t)
	n := newNames()
	g := newGoogle(t, project, sa)
	name := "infrena-redis-" + n.run
	url := "https://redis.googleapis.com/v1/projects/" + project + "/locations/" + region + "/instances/" + name
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(context.Background()), 15*time.Minute)
		defer cancel()
		g.deleteAndWait(t, ctx, "gcp.redis.instance", url, url)
	})
	cfg := func(team string) string {
		return fmt.Sprintf(`project: infrena-gcp-live-redis
%[1]sresources:
  redis:
    type: gcp.redis.instance
    name: %[2]s
    tier: BASIC
    memorySizeGb: 1
    labels:
      team: %[3]s
`, liveProvider(project, sa), name, team)
	}
	dir := t.TempDir()
	write(t, dir, "infrena.yml", cfg("a"))
	applyOK(t, dir, "create")
	if changes := planIs(t, dir, exitOK); len(changes) != 0 {
		t.Errorf("the plan after creating the instance proposes %v", changes)
	}
	updateInPlace(t, g, dir, url, cfg("b"))
	write(t, dir, "infrena.yml", cut(readFile(t, dir, "infrena.yml"), "resources:"))
	applyOK(t, dir, "destroy")
}

// TestLiveSpannerInstanceAndBackupSchedule are gcp.spanner.instance and
// gcp.spanner.backupschedule, shipped 2026-09-25. 100 processing units, the
// smallest instance, and a database seeded directly: the catalog has no
// Spanner database type.
func TestLiveSpannerInstanceAndBackupSchedule(t *testing.T) {
	project, sa := guard(t)
	n := newNames()
	g := newGoogle(t, project, sa)
	id := "infrena-sp-" + n.run
	instURL := "https://spanner.googleapis.com/v1/projects/" + project + "/instances/" + id
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(context.Background()), 10*time.Minute)
		defer cancel()
		// The instance takes its databases and schedules with it.
		g.deleteAndWait(t, ctx, "gcp.spanner.instance", instURL, instURL)
	})
	instance := func(display string) string {
		return fmt.Sprintf(`  sp:
    type: gcp.spanner.instance
    instanceId: %[1]s
    name: projects/%[2]s/instances/%[1]s
    config: projects/%[2]s/instanceConfigs/regional-%[3]s
    displayName: %[4]s
    processingUnits: 100
`, id, project, region, display)
	}
	head := "project: infrena-gcp-live-spanner\n" + liveProvider(project, sa) + "resources:\n"
	dir := t.TempDir()
	write(t, dir, "infrena.yml", head+instance("infrena live a"))
	applyOK(t, dir, "create the instance")
	if changes := planIs(t, dir, exitOK); len(changes) != 0 {
		t.Errorf("the plan after creating the instance proposes %v", changes)
	}
	updateInPlace(t, g, dir, instURL, head+instance("infrena live b"))

	// The schedule needs a database.
	code, raw, _, err := g.doRetry(t.Context(), http.MethodPost, instURL+"/databases",
		map[string]any{"createStatement": "CREATE DATABASE `db1`"})
	if err != nil || code >= 300 {
		t.Fatalf("seeding the database: %d %s %v", code, raw, err)
	}
	waitFor(t, 3*time.Minute, func() bool {
		c, _, _ := g.get(t.Context(), instURL+"/databases/db1")
		return c == http.StatusOK
	}, "the seeded database")
	schedURL := instURL + "/databases/db1/backupSchedules/nightly"
	schedule := func(cron string) string {
		return fmt.Sprintf(`  sched:
    type: gcp.spanner.backupschedule
    instance: %s
    database: db1
    name: nightly
    retentionDuration: 86400s
    spec:
      cronSpec:
        text: %q
    fullBackupSpec: {}
`, id, cron)
	}
	write(t, dir, "infrena.yml", head+instance("infrena live b")+schedule("0 2 * * *"))
	applyOK(t, dir, "create the schedule")
	if changes := planIs(t, dir, exitOK); len(changes) != 0 {
		t.Errorf("the plan after creating the schedule proposes %v", changes)
	}
	write(t, dir, "infrena.yml", head+instance("infrena live b")+schedule("0 3 * * *"))
	changes := planIs(t, dir, exitChanges)
	if len(changes) != 1 || changes[0].Kind != "update" {
		t.Fatalf("a cron change plans %v, want one update", changes)
	}
	applyOK(t, dir, "update the schedule")
	var got struct {
		Spec struct {
			CronSpec struct {
				Text string `json:"text"`
			} `json:"cronSpec"`
		} `json:"spec"`
	}
	_, raw, _ = g.get(t.Context(), schedURL)
	if json.Unmarshal(raw, &got) != nil || got.Spec.CronSpec.Text != "0 3 * * *" {
		t.Errorf("Google's schedule does not carry the new cron: %s", raw)
	}
	if changes := planIs(t, dir, exitOK); len(changes) != 0 {
		t.Errorf("the plan after the schedule update proposes %v", changes)
	}
	write(t, dir, "infrena.yml", head+instance("infrena live b"))
	applyOK(t, dir, "destroy the schedule")
	write(t, dir, "infrena.yml", cut(readFile(t, dir, "infrena.yml"), "resources:"))
	applyOK(t, dir, "destroy the instance")
}
