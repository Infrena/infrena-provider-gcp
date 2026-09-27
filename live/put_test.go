//go:build live

package live

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"
)

// TestLiveLogMetricUpdatesByPut. A log-based metric's only update is a PUT,
// which replaces the metric with the body it is sent. Until 2026-09-25 the
// rule was PATCH or nothing, so every change REPLACED the metric, and its
// history with it. Now the update reads the metric fresh and PUTs it whole
// with the change written in: changing the description must leave the
// filter Google holds exactly as it was, and the metric must be the same one.
func TestLiveLogMetricUpdatesByPut(t *testing.T) {
	project, sa := guard(t)
	n := newNames()
	g := newGoogle(t, project, sa)
	name := "infrena-metric-" + n.run
	url := "https://logging.googleapis.com/v2/projects/" + project + "/metrics/" + name
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(context.Background()), 5*time.Minute)
		defer cancel()
		g.deleteAndWait(t, ctx, "gcp.logging.metric", url, url)
	})
	filter := `resource.type="gce_instance" AND severity>=ERROR`
	cfg := func(desc string) string {
		return fmt.Sprintf(`project: infrena-gcp-live-metric
%[1]sresources:
  metric:
    type: gcp.logging.metric
    name: %[2]s
    filter: %[3]q
    description: %[4]s
`, liveProvider(project, sa), name, filter, desc)
	}
	dir := t.TempDir()
	write(t, dir, "infrena.yml", cfg("created by the infrena live suite"))
	applyOK(t, dir, "create")
	if changes := planIs(t, dir, exitOK); len(changes) != 0 {
		t.Errorf("the plan after creating the metric proposes %v", changes)
	}
	updateInPlace(t, g, dir, url, cfg("changed by the infrena live suite"))
	var got struct {
		Filter      string `json:"filter"`
		Description string `json:"description"`
	}
	code, raw, err := g.get(t.Context(), url)
	if err != nil || code != http.StatusOK || json.Unmarshal(raw, &got) != nil {
		t.Fatalf("reading the metric: %d %s %v", code, raw, err)
	}
	if got.Description != "changed by the infrena live suite" || got.Filter != filter {
		t.Errorf("after the PUT Google holds description %q and filter %q; the filter must be untouched",
			got.Description, got.Filter)
	}
	write(t, dir, "infrena.yml", cut(readFile(t, dir, "infrena.yml"), "resources:"))
	applyOK(t, dir, "destroy")
}

// TestLiveSqlUserUpdatesByPut. Cloud SQL's users.update is a PUT to the
// COLLECTION, naming the user in ?name=, and it reads role changes from its
// query: "body.database_roles will be ignored for update request". Until
// 2026-09-26 every change REPLACED the user, and Google never returns the
// password, so every plan proposed setting it again.
//
// On one db-f1-micro Postgres instance, created here directly and deleted
// after: a password change and two role changes must each plan as one update
// and apply cleanly, the roles Google holds must be exactly the configured
// ones (the second change REVOKES the first role, which only
// revokeExistingRoles=true does), and every plan after must be clean.
func TestLiveSqlUserUpdatesByPut(t *testing.T) {
	project, sa := guard(t)
	n := newNames()
	g := newGoogle(t, project, sa)
	instance := "infrena-sql-" + n.run
	base := "https://sqladmin.googleapis.com/v1/projects/" + project + "/instances/"
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(context.Background()), 15*time.Minute)
		defer cancel()
		g.deleteAndWait(t, ctx, "gcp.sqladmin.instance", instance, base+instance)
	})
	g.createSQLInstance(t, base, instance)

	userURL := base + instance + "/users/bob"
	dir := t.TempDir()
	cfg := func(roles string) string {
		return fmt.Sprintf(`project: infrena-gcp-live-sqluser
%[1]sresources:
  bob:
    type: gcp.user
    instance: %[2]s
    name: bob
    password: ${secret.INFRENA_LIVE_SQL_PASSWORD}
%[3]s`, liveProvider(project, sa), instance, roles)
	}
	roles := func(names ...string) string {
		if len(names) == 0 {
			return ""
		}
		return fmt.Sprintf("    database_roles: [%s]\n", strings.Join(names, ", "))
	}
	held := func() []string {
		var got struct {
			DatabaseRoles []string `json:"databaseRoles"`
		}
		code, raw, err := g.get(t.Context(), userURL)
		if err != nil || code != http.StatusOK || json.Unmarshal(raw, &got) != nil {
			t.Fatalf("reading the user: %d %s %v", code, raw, err)
		}
		sort.Strings(got.DatabaseRoles)
		return got.DatabaseRoles
	}
	oneUpdate := func(what string) {
		t.Helper()
		changes := planIs(t, dir, exitChanges)
		if len(changes) != 1 || changes[0].Kind != "update" {
			t.Fatalf("%s plans %v, want one update: anything else replaces the user", what, changes)
		}
		applyOK(t, dir, what)
		if changes := planIs(t, dir, exitOK); len(changes) != 0 {
			t.Errorf("the plan after %s proposes %v", what, changes)
		}
	}

	t.Setenv("INFRENA_LIVE_SQL_PASSWORD", "first-"+n.run)
	write(t, dir, "infrena.yml", cfg(roles()))
	applyOK(t, dir, "create")
	if changes := planIs(t, dir, exitOK); len(changes) != 0 {
		t.Errorf("the plan after creating the user proposes %v: a password Google never returns reads as drift", changes)
	}

	t.Setenv("INFRENA_LIVE_SQL_PASSWORD", "second-"+n.run)
	oneUpdate("the password change")

	write(t, dir, "infrena.yml", cfg(roles("pg_read_all_data")))
	oneUpdate("granting a role")
	if got := held(); !slices.Equal(got, []string{"pg_read_all_data"}) {
		t.Errorf("after granting pg_read_all_data Google holds %v", got)
	}

	write(t, dir, "infrena.yml", cfg(roles("pg_monitor")))
	oneUpdate("swapping the role")
	if got := held(); !slices.Equal(got, []string{"pg_monitor"}) {
		t.Errorf("after swapping to pg_monitor Google holds %v: the old role must be revoked", got)
	}

	write(t, dir, "infrena.yml", cut(readFile(t, dir, "infrena.yml"), "resources:"))
	applyOK(t, dir, "destroy")
}

// createSQLInstance creates the cheapest Postgres instance Cloud SQL offers
// and waits for it. It is a fixture, not what is under test, so it goes
// straight to the API. Cloud SQL answers with its own operation, polled at
// its selfLink until DONE.
func (g *google) createSQLInstance(t *testing.T, base, name string) {
	t.Helper()
	body := map[string]any{
		"name": name, "region": region, "databaseVersion": "POSTGRES_16",
		"settings": map[string]any{
			"tier": "db-f1-micro", "edition": "ENTERPRISE",
			"backupConfiguration": map[string]any{"enabled": false},
			"userLabels":          map[string]any{"infrena-live": "true"},
		},
	}
	code, raw, _, err := g.doRetry(t.Context(), http.MethodPost, strings.TrimSuffix(base, "/"), body)
	if err != nil || code >= 300 {
		t.Fatalf("creating the Cloud SQL instance: %d %s %v", code, raw, err)
	}
	var op struct {
		SelfLink string `json:"selfLink"`
		Status   string `json:"status"`
		Error    any    `json:"error"`
	}
	if err := json.Unmarshal(raw, &op); err != nil || op.SelfLink == "" {
		t.Fatalf("the create answered no operation: %s", raw)
	}
	waitFor(t, 20*time.Minute, func() bool {
		code, raw, err := g.get(t.Context(), op.SelfLink)
		if err != nil || code != http.StatusOK || json.Unmarshal(raw, &op) != nil {
			return false
		}
		if op.Error != nil {
			t.Fatalf("creating the Cloud SQL instance failed: %s", raw)
		}
		return op.Status == "DONE"
	}, "the Cloud SQL instance to be created")
}
