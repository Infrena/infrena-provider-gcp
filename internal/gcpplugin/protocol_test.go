package gcpplugin

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/infrena/infrena-provider-gcp/internal/gcpfake"
	"github.com/infrena/infrena-provider-gcp/internal/gcptest"
	"github.com/infrena/infrena/pkg/pluginproto"
	"github.com/infrena/infrena/pkg/pluginsdk"
	"github.com/infrena/infrena/pkg/plugintest"
	"github.com/infrena/infrena/pkg/provider"
	"github.com/infrena/infrena/pkg/resource"
	"github.com/infrena/infrena/pkg/value"
)

// These tests drive this plugin through INFRENA'S OWN HOST, over the real
// protocol, in one process.
//
// Everything in internal/gcprov calls the provider directly, so none of it
// sees the layer that sits between a plugin and the engine in a real run:
// JSON encoding and decoding, schema validation at load, and the rules the
// host enforces on every answer a plugin gives -- bookkeeping re-attached,
// sensitivity forced from the schema, provenance overwritten, and an
// attribute the plugin's own schema does not declare REFUSED outright. That
// last rule is not a warning: it fails the operation, and it is what made
// every create and every refresh of an ordinary project-scoped type fail
// before Task 17 (see declaredIDAttrs).
//
// The e2e suite proves the same things through two real binaries and is the
// authority; these run in a second and catch the same class of failure
// without building anything, which is what makes them worth having as well.

// TestTheHostAcceptsTheWholeCatalog is the cheapest possible check that this
// plugin is loadable at all.
//
// plugintest.Open FAILS if the schemas do not pass the checks infrena applies
// on load: a type outside the plugin's own name prefix, an attribute named
// after a lifecycle option, a malformed definition, a default that is not the
// kind its attribute declares, and a References naming a type or attribute the
// plugin does not declare. Running 233 generated definitions through them is
// the cheapest check that nothing the generator emits trips a host-side rule
// -- and the generator is the part of this repository most likely to start
// emitting something new without anybody deciding to.
func TestTheHostAcceptsTheWholeCatalog(t *testing.T) {
	gcptest.Isolate(t)
	host, err := plugintest.Open(context.Background(), NewPlugin(), t.TempDir())
	if err != nil {
		t.Fatalf("the host refused this plugin: %v", err)
	}
	defer host.Close()

	defs := host.Definitions()
	// 200, not 300, and certainly not the "roughly 500" an early draft of the
	// spec guessed before anything had been generated. The real catalog
	// measured 233 on 2026-09-22; 200 is below that with room for ordinary
	// drift in Google's own Discovery documents, and high enough to catch a
	// whole API failing to fetch.
	if len(defs) < 200 {
		t.Fatalf("the host sees %d types; the catalog measured 233, so below 200 means something broke", len(defs))
	}

	// gcp.compute.instance, NOT gcp.network. compute's Network and Subnetwork
	// are tier-2 on unruled hooks and do not ship at all, so asserting on
	// gcp.network would fail immediately and for a reason that has nothing to
	// do with the host. gcp.compute.instance is in discover_default and is
	// pinned by TestDiscoverDefaultNamesOnlyTypesWeServe, so this cannot rot
	// silently.
	var found bool
	for _, d := range defs {
		if d.Type == "gcp.compute.instance" {
			found = true
		}
	}
	if !found {
		t.Error("gcp.compute.instance is not among the types the host sees")
	}
}

// TestTheHandshakeDeclaresMaxConcurrency pins that the number MaxConcurrency
// returns actually reaches the host.
//
// MaxConcurrency is an OPTIONAL method the SDK detects by type assertion
// (pluginsdk's maxConcurrencyOf), so a signature that drifted -- an int32
// return, a value receiver where the plugin is used as a pointer -- would
// stop it being found, silently, and the host would fall back to its own
// default of 8 with nothing anywhere saying so. Asserting `pl.MaxConcurrency()
// == 16` would not catch that: it would still be 16, and still unread.
//
// The handshake is the first line of the protocol stream, so serving the
// plugin over a closed input is enough to read it.
func TestTheHandshakeDeclaresMaxConcurrency(t *testing.T) {
	gcptest.Isolate(t)
	var out bytes.Buffer
	if err := pluginsdk.Serve(NewPlugin(), strings.NewReader(""), &out); err != nil {
		t.Fatalf("serving: %v", err)
	}
	line, _, _ := strings.Cut(out.String(), "\n")
	var hs pluginproto.Handshake
	if err := json.Unmarshal([]byte(line), &hs); err != nil {
		t.Fatalf("the first line of the stream is not a handshake: %v\n%s", err, line)
	}
	if hs.MaxConcurrency != 16 {
		t.Errorf("the handshake declares max_concurrency %d, want 16 -- the host reads this one, "+
			"not the method", hs.MaxConcurrency)
	}
	if hs.Name != PluginName {
		t.Errorf("the handshake names the plugin %q, want %q", hs.Name, PluginName)
	}
	if hs.Protocol != pluginproto.Version {
		t.Errorf("the handshake speaks protocol %d, want %d", hs.Protocol, pluginproto.Version)
	}
}

// TestTheHostAcceptsACreateReadUpdateDelete runs the whole resource lifecycle
// through the host's own adapter against the fake cloud.
//
// EVERY ANSWER HERE HAS BEEN CHECKED BY THE HOST, which is the point. A
// gcprov test asserting the same four calls passes on a state the host would
// throw away: the provider id segments a create recovers from a url are
// PLACEHOLDER names ("project", "location"), not attributes any generated
// type declares, and the host refuses a result carrying one. Before Task 17
// that refusal was live on every project-scoped type and no test in this
// repository could see it.
func TestTheHostAcceptsACreateReadUpdateDelete(t *testing.T) {
	gcptest.Isolate(t)
	s := gcpfake.New(t)
	defer s.Close()
	// Eventarc answers every mutation with a google.longrunning.Operation, so
	// the fake must too. It used to answer synchronously, which matched what
	// the catalog then believed about gcp.channel and was wrong about the API:
	// the generator only recognised an operation schema called exactly
	// "Operation", and eventarc calls it "GoogleLongrunningOperation". A fake
	// that agrees with a wrong catalog is the one failure no test can see.
	s.SetOperationStyle(gcpfake.OpLongRunning)
	prov := configured(t, s)
	ctx := context.Background()

	desired := &resource.DesiredResource{
		Type: "gcp.channel",
		Attrs: map[string]value.Value{
			"name":   value.String("host-channel", value.SourceExplicit),
			"labels": value.Map(map[string]value.Value{"owner": value.String("platform", value.SourceExplicit)}, value.SourceExplicit),
		},
	}
	created, err := prov.Create(ctx, desired)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	const wantID = "projects/host-project/locations/host-region/channels/host-channel"
	if created.ProviderID != wantID {
		t.Fatalf("created provider id = %q, want %q", created.ProviderID, wantID)
	}
	// The project and the region came from the INSTANCE, not from the
	// resource: no generated type declares them, so this create could not
	// have built its url without them. See gcprov's withScope.
	if _, ok := s.Get("/v1/" + wantID); !ok {
		t.Fatalf("nothing was created at the path the instance's project and region address")
	}

	read, err := prov.Read(ctx, created)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if read == nil {
		t.Fatal("the resource this plugin just created reads as absent")
	}
	if _, leaked := read.Attributes["project"]; leaked {
		t.Error("state carries `project`, which gcp.channel does not declare; the host refuses that")
	}

	desired.Attrs["labels"] = value.Map(
		map[string]value.Value{"owner": value.String("someone-else", value.SourceExplicit)},
		value.SourceExplicit)
	updated, err := prov.Update(ctx, read, desired)
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	labels, _ := updated.Attributes["labels"].Raw.(map[string]value.Value)
	if got, _ := labels["owner"].Raw.(string); got != "someone-else" {
		t.Errorf("after the update the owner label is %q, want %q", got, "someone-else")
	}

	if err := prov.Delete(ctx, updated); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, still := s.Get("/v1/" + wantID); still {
		t.Error("the resource is still there after a delete the host reported as successful")
	}
}

// TestTheHostAcceptsWhatDiscoverReports is the same rule on the discovery
// path, which does not go through Read at all and therefore had its own copy
// of the same defect.
func TestTheHostAcceptsWhatDiscoverReports(t *testing.T) {
	gcptest.Isolate(t)
	s := gcpfake.New(t)
	defer s.Close()
	const id = "projects/host-project/locations/host-region/channels/found-here"
	s.Seed("/v1/"+id, map[string]any{"labels": map[string]any{"owner": "someone"}})
	s.SeedCAI("projects/host-project", []gcpfake.Asset{
		{AssetType: "eventarc.googleapis.com/Channel", Name: "//eventarc.googleapis.com/" + id},
	})

	found, err := configured(t, s).Discover(context.Background(), provider.DiscoverRequest{})
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	if len(found) != 1 {
		t.Fatalf("discover reported %d resources, want 1: %v", len(found), found)
	}
	if found[0].ProviderID != id {
		t.Errorf("discovered provider id = %q, want %q", found[0].ProviderID, id)
	}
}

// configured opens the plugin through infrena's host and returns one
// instance pointed at the fake.
//
// The endpoint override is an ENVIRONMENT variable rather than a
// configuration key on purpose: in a real run the plugin is a child process,
// and an environment variable is the only channel a test on the other side of
// that boundary has. Setting it here means these tests and the e2e suite
// reach the fake by exactly the same route.
func configured(t *testing.T, s *gcpfake.Server) provider.Provider {
	t.Helper()
	t.Setenv(EndpointOverrideEnv, s.URL())
	host, err := plugintest.Open(context.Background(), NewPlugin(), t.TempDir())
	if err != nil {
		t.Fatalf("the host refused this plugin: %v", err)
	}
	t.Cleanup(func() { host.Close() })
	prov, err := host.Configure(provider.Config{
		Instance: "main",
		Values: map[string]value.Value{
			"project": value.String("host-project", value.SourceExplicit),
			"region":  value.String("host-region", value.SourceExplicit),
		},
		ProjectDir: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("configuring an instance: %v", err)
	}
	return prov
}

// TestTheOverrideNeverResolvesCredentials pins the safety half of the
// endpoint override: while it is set, the credential chain is not consulted
// at all.
//
// Proved DIFFERENTIALLY, with an instance whose `credentials_file` names
// something that does not exist. Without the override that is a definite
// failure, naming the instance -- which is also the behaviour a misconfigured
// project wants, failing while infrena is still starting plugins rather than
// once per resource in the middle of an apply. With the override set the same
// instance configures cleanly, which it could only do by never opening the
// file.
//
// A file that cannot be read, rather than an absent Application Default
// Credentials chain: ADC is not a reliable negative. google.FindDefaultCredentials
// can succeed on a machine with nothing configured (measured on this one), so a
// test built on its failing would pass or fail according to whose laptop ran it.
func TestTheOverrideNeverResolvesCredentials(t *testing.T) {
	gcptest.Isolate(t)
	cfg := provider.Config{
		Instance: "named-instance",
		Values: map[string]value.Value{
			"project":          value.String("p", value.SourceExplicit),
			"credentials_file": value.String(filepath.Join(t.TempDir(), "no-such-key.json"), value.SourceExplicit),
		},
	}

	t.Setenv(EndpointOverrideEnv, "")
	_, err := NewPlugin().New(cfg)
	if err == nil {
		t.Fatal("an instance whose credentials_file does not exist was configured successfully")
	}
	if !strings.Contains(err.Error(), "named-instance") {
		t.Errorf("the error does not name the instance that failed: %v", err)
	}

	s := gcpfake.New(t)
	defer s.Close()
	t.Setenv(EndpointOverrideEnv, s.URL())
	if _, err := NewPlugin().New(cfg); err != nil {
		t.Fatalf("the override is set, so credentials should never have been read: %v", err)
	}
}

// TestAnEndpointOverrideThatIsNotAUrlIsRefused. The override's whole promise
// is that nothing reaches the real cloud, so a value it cannot make sense of
// must fail loudly rather than be ignored -- ignoring it would leave the
// instance pointed at Google with the operator believing otherwise.
func TestAnEndpointOverrideThatIsNotAUrlIsRefused(t *testing.T) {
	gcptest.Isolate(t)
	t.Setenv(EndpointOverrideEnv, "localhost:8080") // no scheme
	_, err := NewPlugin().New(provider.Config{Instance: "main"})
	if err == nil {
		t.Fatal("an override with no scheme was accepted, so the instance talks to Google")
	}
	if !strings.Contains(err.Error(), EndpointOverrideEnv) {
		t.Errorf("the error does not name the variable at fault: %v", err)
	}
}

// TestAPubSubTopicLivesAndDiesThroughTheHost runs gcp.topic -- the REAL
// generated catalog entry, through the host's own adapter -- from create to
// delete. Pub/Sub is the one API whose shape differs at both ends: a create is
// a PUT to the topic's own path, where every other API in the catalog POSTs to
// a collection, and an update is AIP-134's UpdateTopicRequest envelope with the
// field mask in the body. Either one wrong and every call fails, so this
// asserts the wire, not just the resulting state.
func TestAPubSubTopicLivesAndDiesThroughTheHost(t *testing.T) {
	gcptest.Isolate(t)
	s := gcpfake.New(t)
	defer s.Close()
	prov := configured(t, s)
	ctx := context.Background()

	const id = "projects/host-project/topics/orders"
	const path = "/v1/" + id
	labels := func(owner string) value.Value {
		return value.Map(map[string]value.Value{"owner": value.String(owner, value.SourceExplicit)}, value.SourceExplicit)
	}
	desired := &resource.DesiredResource{Type: "gcp.topic", Attrs: map[string]value.Value{
		"name":   value.String(id, value.SourceExplicit),
		"labels": labels("platform"),
	}}

	created, err := prov.Create(ctx, desired)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if created.ProviderID != id {
		t.Fatalf("provider id = %q, want %q", created.ProviderID, id)
	}
	var sawPut bool
	for _, r := range s.Requests() {
		if r.Path == path && r.Method == "PUT" {
			sawPut = true
		}
		if r.Method == "POST" {
			t.Errorf("create POSTed to %s; Pub/Sub creates with a PUT to the topic's own path", r.Path)
		}
	}
	if !sawPut {
		t.Fatalf("no PUT to %s; requests were %v", path, s.Requests())
	}

	read, err := prov.Read(ctx, created)
	if err != nil || read == nil {
		t.Fatalf("read: %v (state %v)", err, read)
	}

	desired.Attrs["labels"] = labels("someone-else")
	updated, err := prov.Update(ctx, read, desired)
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	var patch *gcpfake.Request
	for _, r := range s.Requests() {
		if r.Method == "PATCH" && r.Path == path {
			r := r
			patch = &r
		}
	}
	if patch == nil {
		t.Fatal("no PATCH was sent")
	}
	if _, onQuery := patch.Query["updateMask"]; onQuery {
		t.Error("the mask went on the query string; UpdateTopicRequest carries it in the body")
	}
	var envelope map[string]any
	if err := json.Unmarshal(patch.Body, &envelope); err != nil {
		t.Fatal(err)
	}
	if _, wrapped := envelope["topic"].(map[string]any); !wrapped || envelope["updateMask"] != "labels" {
		t.Errorf("update body is not UpdateTopicRequest{topic, updateMask: labels}: %s", patch.Body)
	}
	got, _ := updated.Attributes["labels"].Raw.(map[string]value.Value)
	if owner, _ := got["owner"].Raw.(string); owner != "someone-else" {
		t.Errorf("after the update the owner label is %q, want %q", owner, "someone-else")
	}

	if err := prov.Delete(ctx, updated); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, still := s.Get(path); still {
		t.Error("the topic is still there after a delete the host reported as successful")
	}
}
