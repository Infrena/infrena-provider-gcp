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
