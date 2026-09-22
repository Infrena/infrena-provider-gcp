//go:build e2e

package e2e

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/infrena/infrena-provider-gcp/internal/gcpfake"
)

// The type the workflow runs on is gcp.trigger, and the choice is measured
// rather than arbitrary.
//
// It must be creatable, updatable and labelled, and it must complete its
// mutations synchronously -- gcpfake answers one operation shape per server
// (SetOperationStyle), so a type whose catalog entry says AwaitComputeOperation
// would need a differently configured fake than one that says AwaitNone, and
// the workflow would then be testing the fake's configuration as much as the
// provider. Of the 233 shipped types, 52 have `labels` and an update verb;
// seven of those are AwaitNone.
//
// gcp.trigger is the one of those seven that makes the suite DIFFERENTIAL. It
// declares a nested object (`destination`), a list of nested objects
// (`event_filters`) and an integer inside a nested object
// (`retry_policy.max_attempts`), so the object, list and scalar arms of
// Reconcile all run. The first fixture this suite had was gcp.channel, whose
// only interesting attribute is a free-form `labels` map -- and a free-form
// map is copied verbatim, never reconciled. Removing reconciliation entirely
// left every subtest passing. A suite that cannot fail when the thing it
// exists to test is deleted is not testing it, and the type is where that was
// decided.
//
// NOT gcp.compute.instance, gcp.firewall or gcp.router, the compute types the
// brief names as safe: all three are AwaitComputeOperation.
const (
	workflowType = "gcp.trigger"
	channelName  = "e2e-trigger"
	// The path the fake stores it at: api base + path_prefix ("v1/") + the
	// expanded self_link. Written out rather than computed so that a change
	// to url building shows up here as a wrong path rather than as two
	// wrong things agreeing.
	channelPath = "/v1/projects/infrena-e2e/locations/us-central1/triggers/" + channelName
)

// TestTheWorkflow is the whole loop, in order, against one fake cloud.
//
// The subtests SHARE state on purpose: each one's precondition is the
// previous one's result, which is the only way a claim like "the second plan
// is clean" can be made at all. They are therefore not independent and must
// not be run with t.Parallel(). The three that need a cloud of their own --
// discovery of something infrena never created, import, and the system-owned
// rule -- say so where they start one.
func TestTheWorkflow(t *testing.T) {
	c := start(t)

	t.Run("validate_accepts_the_project", func(t *testing.T) {
		// Validate compiles the configuration against the schemas the plugin
		// sent, so this is the cheapest end-to-end check that the generated
		// catalog is something a real host can compile a real project
		// against. It contacts no provider.
		r := c.mustRun(t, exitOK, "validate")
		if !strings.Contains(r.combined(), "valid") {
			t.Errorf("validate said nothing about validity:\n%s", r.combined())
		}
	})

	t.Run("plan_proposes_a_create", func(t *testing.T) {
		out := filepath.Join(t.TempDir(), "plan.json")
		c.mustRun(t, exitChanges, "plan", "dev", "--output", out)
		changes := planChanges(t, out)
		if len(changes) != 1 {
			t.Fatalf("plan holds %d changes, want exactly one: %v", len(changes), changes)
		}
		if changes[0].action != "create" || changes[0].typ != workflowType {
			t.Errorf("plan proposes %s of a %s, want a create of a %s",
				changes[0].action, changes[0].typ, workflowType)
		}
	})

	t.Run("apply_creates_it", func(t *testing.T) {
		// GCP RETURNS MORE THAN IT WAS SENT. gcpfake does not: it stores the
		// create body verbatim and answers with that. A real API fills fields
		// in, including ones this catalog does not model -- eventarc's
		// retryPolicy really does carry retry delays the generator did not
		// pick up for this type -- and every one of those reads as drift
		// against configuration that has not changed unless the read path
		// reconciles it away.
		//
		// Armed on the create's own readback, so the resource carries the
		// server-added field from the moment it exists rather than acquiring
		// it later, which would be drift and is a different claim.
		//
		// WITHOUT THIS THE SUITE IS BLIND TO RECONCILIATION. Measured:
		// replacing Reconcile's body with `return incoming` left every
		// subtest below passing.
		serverAdds(t, c, "retryPolicy", "minRetryDelay", "1s")

		c.mustRun(t, exitChanges, "apply", "dev", "--auto-approve")

		// The resource is REALLY THERE, in the fake, at the path the
		// catalog's own url templates address it by. Asserting on infrena's
		// output alone would pass for a provider that reported success and
		// called nothing.
		body, ok := c.fake.Get(channelPath)
		if !ok {
			t.Fatalf("nothing was created at %s; the fake holds %v", channelPath, c.fake.Requests())
		}
		// The labels, not the name: eventarc's create takes the id as a query
		// parameter ("?channelId=e2e-channel"), so `name` is correctly absent
		// from the request body and therefore from what the fake stored. The
		// path it was stored AT is what carries the name, and the assertion
		// above is about exactly that.
		labels, _ := body["labels"].(map[string]any)
		if labels["owner"] != "platform" {
			t.Errorf("the created resource is %v, want the configured label", body)
		}
		// And state records the identity, which is what every later command
		// rebuilds its urls from.
		if got := providerIDOf(t, c.dir, "bus"); got != "projects/infrena-e2e/locations/us-central1/triggers/"+channelName {
			t.Errorf("state records provider id %q", got)
		}
	})

	t.Run("a_second_plan_is_clean", func(t *testing.T) {
		// THE SUBTEST THIS SUITE EXISTS FOR.
		//
		// A plan that converges is not a property of any one function: it is
		// the claim that what Create returned, written to state, read back by
		// Read, reconciled against configuration and diffed by the planner,
		// proposes nothing. Every unit test in internal/gcprov can pass while
		// this fails, because no unit test ever asks the question -- and a
		// provider whose plans do not converge is unusable while looking
		// entirely healthy one call at a time.
		out := filepath.Join(t.TempDir(), "plan.json")
		r := c.run(t, "plan", "dev", "--output", out)
		if r.ExitCode != exitOK {
			t.Fatalf("the plan right after an apply is not clean (exit %d):\n%s\n%v",
				r.ExitCode, r.combined(), planChanges(t, out))
		}
	})

	t.Run("a_label_changed_outside_infrena_is_an_update", func(t *testing.T) {
		// Drift, and the one place the update path is exercised end to end.
		//
		// It also pins that Update diffs against the REFRESHED observation
		// rather than against what state remembered: state and configuration
		// agree here (the previous subtest just proved it), so a diff taken
		// against state alone finds nothing to do and the drift is never
		// corrected.
		drift(t, c, map[string]any{"owner": "someone-else", "added": "by-hand"})

		out := filepath.Join(t.TempDir(), "plan.json")
		c.mustRun(t, exitChanges, "plan", "dev", "--output", out)
		changes := planChanges(t, out)
		if len(changes) != 1 || changes[0].action != "update" {
			t.Fatalf("plan proposes %v, want exactly one update -- a replace here would mean "+
				"the diff decided a mutable attribute forces a new resource", changes)
		}

		c.mustRun(t, exitChanges, "apply", "dev", "--auto-approve")

		body, ok := c.fake.Get(channelPath)
		if !ok {
			t.Fatal("the resource is gone after an update")
		}
		labels, _ := body["labels"].(map[string]any)
		if labels["owner"] != "platform" {
			t.Errorf("labels are %v after the update, want owner back at %q", labels, "platform")
		}
		// And the correction converges: an update that left the resource
		// disagreeing with configuration would plan the same change forever.
		if r := c.run(t, "plan", "dev"); r.ExitCode != exitOK {
			t.Errorf("the plan after correcting drift is not clean (exit %d):\n%s", r.ExitCode, r.combined())
		}
	})

	t.Run("removing_it_from_config_proposes_a_destroy", func(t *testing.T) {
		original := readFile(t, c.dir, "infrena.yml")
		// The resource block dropped, the provider instance kept: state still
		// names the resource, and a resource in state that no configuration
		// declares is scheduled for destruction.
		write(t, c.dir, "infrena.yml", cut(original, "resources:"))
		t.Cleanup(func() { write(t, c.dir, "infrena.yml", original) })

		out := filepath.Join(t.TempDir(), "plan.json")
		c.mustRun(t, exitChanges, "plan", "dev", "--output", out)
		changes := planChanges(t, out)
		if len(changes) != 1 || changes[0].action != "destroy" {
			t.Fatalf("plan proposes %v, want exactly one destroy", changes)
		}

		c.mustRun(t, exitChanges, "apply", "dev", "--auto-approve")
		if _, ok := c.fake.Get(channelPath); ok {
			t.Error("the resource is still in the cloud after a destroy was applied")
		}
	})
}

// drift changes the resource's labels IN THE CLOUD, behind infrena's back --
// somebody editing it in the console, which is what drift is.
//
// It reads what the fake holds and writes it back with new labels rather than
// seeding a whole body, so the resource keeps every other field the create
// gave it. A test that replaced the body would also be changing fields
// nothing asked about, and the update it then observed would not be about
// labels.
func drift(t *testing.T, c *cloud, labels map[string]any) {
	t.Helper()
	body, ok := c.fake.Get(channelPath)
	if !ok {
		t.Fatalf("nothing at %s to drift", channelPath)
	}
	body["labels"] = labels
	c.fake.Seed(channelPath, body)
}

// TestDiscoverFindsWhatInfrenaDidNotCreate, TestImportGenerate... and
// TestASystemOwnedResource... each run on a cloud of their own.
//
// They are about resources infrena never created, so they cannot be told from
// the workflow's own resource if they share its state file: "discovery found
// it" would be satisfied by the thing the apply above made. A fresh project
// makes the claim unambiguous.

// caiParent is the scope a searchAllResources call is taken under, derived
// from the provider instance's `project:`.
const caiParent = "projects/infrena-e2e"

// TestTheWorkflowDiscoversWhatItDidNotCreate is `discover_finds_it`.
//
// It asserts BOTH halves, and the second is what makes the first mean
// anything: the resource this project never created is listed, and the one it
// manages is not. A discover that listed everything would satisfy the first
// assertion while being exactly as useless as one that listed nothing --
// discovery's whole job is to answer "what has not been adopted yet".
func TestTheWorkflowDiscoversWhatItDidNotCreate(t *testing.T) {
	c := start(t)
	c.mustRun(t, exitChanges, "apply", "dev", "--auto-approve")

	const strayName = "made-in-the-console"
	strayID := "projects/infrena-e2e/locations/us-central1/triggers/" + strayName
	c.fake.Seed("/v1/"+strayID, map[string]any{"labels": map[string]any{"owner": "someone"}})
	c.fake.SeedCAI(caiParent, []gcpfake.Asset{
		{AssetType: channelAsset, Name: "//eventarc.googleapis.com/" + strayID},
		// The one the apply above made, offered to discovery exactly as CAI
		// would offer it. It must still be left out, because it is managed.
		{AssetType: channelAsset, Name: "//eventarc.googleapis.com/projects/infrena-e2e/locations/us-central1/triggers/" + channelName},
	})

	r := c.mustRun(t, exitOK, "discover")
	if !strings.Contains(r.combined(), strayName) {
		t.Errorf("discover does not list %s, which exists and is not managed:\n%s", strayName, r.combined())
	}
	if strings.Contains(r.combined(), channelName) {
		t.Errorf("discover lists %s, which this project already manages:\n%s", channelName, r.combined())
	}
	if !strings.Contains(r.combined(), workflowType) {
		t.Errorf("discover does not name the type it found:\n%s", r.combined())
	}
}

// channelAsset is gcp.trigger's Cloud Asset Inventory asset type, the key
// discovery maps a search result back through the catalog by.
const channelAsset = "eventarc.googleapis.com/Trigger"

// The reference pair. gcp.targethttpproxy's `urlMap` is one of only SEVEN
// attributes in the whole 233-type catalog that carry a References
// declaration, and it is the one whose two ends are both ordinary,
// project-global, listable compute types. The reference points at
// gcp.urlmap's `selfLink`, so the proxy's stored `urlMap` value and the url
// map's own `selfLink` are the same string -- which is exactly the equality
// infrena's generator projects a `${...}` edge from.
const (
	urlMapType  = "gcp.urlmap"
	urlMapAsset = "compute.googleapis.com/UrlMap"
	urlMapID    = "projects/infrena-e2e/global/urlMaps/site-map"
	proxyType   = "gcp.targethttpproxy"
	proxyAsset  = "compute.googleapis.com/TargetHttpProxy"
	proxyID     = "projects/infrena-e2e/global/targetHttpProxies/site-proxy"
	// The address `import` proposes for an adopted resource: the type's last
	// segment and the resource's own name. Written out because the reference
	// the generator writes NAMES it, so a change to how import names things
	// is a change to the edge this test is about.
	urlMapName   = "urlmap-site-map"
	computeOwner = "//compute.googleapis.com/"
	// computePath is the path every compute request carries, from the API's
	// own base url ("https://compute.googleapis.com/compute/v1/"). The
	// endpoint override swaps the HOST and keeps this, so the fake sees the
	// same paths Google would -- which is also what lets a self link reduce
	// back to a provider id.
	computePath = "/compute/v1/"
)

// TestImportGenerateWritesAReferenceAndTheNextPlanIsClean is
// `import_generate_writes_a_reference_and_the_next_plan_is_clean`.
//
// Two claims, and the second is the one that is easy to get wrong. That
// `--generate` wrote a `${...}` instead of the literal url is visible in the
// file. That the configuration it wrote actually DESCRIBES what is in the
// cloud is only visible from the plan that follows: a generator that omitted
// an attribute the resource really has, or wrote one the schema will not
// take, produces a file that reads fine and proposes a change the moment it
// is planned -- which is a resource infrena would immediately modify on
// adoption.
//
// Nothing is created here. Both resources are seeded straight into the fake,
// the way something that already existed before infrena arrived would be,
// which is also why the compute types' AwaitComputeOperation never comes
// into it: import reads, it does not mutate.
func TestImportGenerateWritesAReferenceAndTheNextPlanIsClean(t *testing.T) {
	c := start(t)
	// A project declaring the provider and NO resources: everything adopted
	// below has to come from `--generate`, not from configuration that was
	// already there.
	//
	// DISCOVERY RUNS THROUGH THE PER-TYPE LISTING FALLBACK HERE, not through
	// Cloud Asset Inventory, and that is forced deliberately (FailCAI below,
	// which is what a project with the Cloud Asset API switched off looks
	// like). A CAI search result is not a resource body -- it carries the
	// asset type, the full resource name and labels -- so the CAI path
	// reports only the attributes the NAME encodes, and `--generate` then has
	// nothing to write but `type:`. The listing path reads real bodies, so
	// this is the path on which generated configuration is configuration.
	// See the task report: the gap is real and is not this task's to close.
	write(t, c.dir, "infrena.yml", cut(readFile(t, c.dir, "infrena.yml"), "resources:")+
		"    discover_types:\n      - "+urlMapType+"\n      - "+proxyType+"\n")
	c.fake.FailCAI(403, "PERMISSION_DENIED", "Cloud Asset API has not been used in this project")

	mapLink := c.fake.URL() + computePath + urlMapID
	c.fake.Seed(computePath+urlMapID, map[string]any{"name": "site-map", "selfLink": mapLink})
	c.fake.Seed(computePath+proxyID, map[string]any{
		"name": "site-proxy", "selfLink": c.fake.URL() + computePath + proxyID,
		// THE EDGE, as the cloud really holds it: a target proxy stores the
		// url map's self link, not its name.
		"urlMap": mapLink,
	})
	c.fake.SeedCAI(caiParent, []gcpfake.Asset{
		{AssetType: urlMapAsset, Name: computeOwner + urlMapID},
		{AssetType: proxyAsset, Name: computeOwner + proxyID},
	})

	r := c.mustRun(t, exitOK, "import", "dev", "--generate")
	t.Logf("import:\n%s", r.combined())

	state := stateOf(t, c.dir)
	if len(state) != 2 {
		t.Fatalf("state holds %d resources after importing two: %v", len(state), state)
	}
	if got := state[urlMapName].ProviderID; got != urlMapID {
		t.Errorf("the url map was adopted under provider id %q, want %q", got, urlMapID)
	}

	generated := generatedConfig(t, c.dir)
	// THE EDGE, in the file: the proxy's url_map refers to the url map
	// resource by address, not by the literal self link the cloud holds.
	// Asserting on the reference's target and not merely on "${" is what
	// makes this about this pair rather than about any interpolation
	// anywhere in the generated project.
	want := "url_map: ${" + urlMapName + "}"
	if !strings.Contains(generated, want) {
		t.Errorf("the generated proxy does not carry %q; it should refer to the url map "+
			"beside it rather than repeating its self link:\n%s", want, generated)
	}
	if strings.Contains(generated, mapLink) {
		t.Errorf("the generated configuration repeats the url map's self link literally:\n%s", generated)
	}

	if r := c.run(t, "plan", "dev"); r.ExitCode != exitOK {
		t.Fatalf("the plan right after `import --generate` is not clean (exit %d):\n%s",
			r.ExitCode, r.combined())
	}
}

// generatedConfig returns everything `import --generate` wrote, concatenated.
//
// The file names are the generator's business and this suite has no opinion
// about them, so it reads the directory rather than naming a file -- an
// assertion that broke because the generator renamed its output would be
// reporting on the wrong thing.
func generatedConfig(t *testing.T, dir string) string {
	t.Helper()
	root := filepath.Join(dir, "discovered")
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("`import --generate` wrote no %s directory: %v", root, err)
	}
	var b strings.Builder
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		data, err := os.ReadFile(filepath.Join(root, e.Name()))
		if err != nil {
			t.Fatalf("reading %s: %v", e.Name(), err)
		}
		b.WriteString("# " + e.Name() + "\n")
		b.Write(data)
		b.WriteString("\n")
	}
	if b.Len() == 0 {
		t.Fatalf("%s is empty", root)
	}
	return b.String()
}

// TestASystemOwnedResourceIsNotImportedUnlessNamed is
// `a_system_owned_resource_is_not_imported_unless_named`.
//
// The rule it pins is the one that stops the worst foot gun in discovery. A
// resource Google created and reconciles -- a GKE node pool's instances, a
// project's automatic network -- adopted by accident is a resource the next
// `apply` proposes destroying the moment it leaves configuration. So it is
// never adopted by a bare `import`, and adopting one on purpose still works,
// because occasionally it is right and the engine is not the party to forbid
// it.
//
// The signal here is a "goog-" label, which is what GCP's own services stamp
// on the resources they manage, and it is asserted through CAI's search
// result rather than a resource body: CAI indexes labels without a readMask,
// which is exactly why the plugin can decide ownership from a search alone.
func TestASystemOwnedResourceIsNotImportedUnlessNamed(t *testing.T) {
	c := start(t)
	write(t, c.dir, "infrena.yml", cut(readFile(t, c.dir, "infrena.yml"), "resources:"))

	const googleOwned = "goog-managed-trigger"
	ownedID := "projects/infrena-e2e/locations/us-central1/triggers/" + googleOwned
	c.fake.Seed("/v1/"+ownedID, map[string]any{"labels": map[string]any{"goog-composer-env": "x"}})
	c.fake.SeedCAI(caiParent, []gcpfake.Asset{{
		AssetType: channelAsset,
		Name:      "//eventarc.googleapis.com/" + ownedID,
		// The label is on the SEARCH RESULT, not only on the resource:
		// that is where the plugin reads it from on the CAI path.
		Labels: map[string]string{"goog-composer-env": "x"},
	}})

	// Bare import: nothing is adopted, and the skip is reported with the
	// plugin's own reason. A silent skip would be worse than adopting it,
	// because the user would not know to look.
	r := c.mustRun(t, exitOK, "import", "dev")
	if _, err := os.Stat(filepath.Join(c.dir, ".infrena", "state", "dev.json")); err == nil {
		if len(stateOf(t, c.dir)) != 0 {
			t.Fatalf("a bare import adopted a system-owned resource: %v", stateOf(t, c.dir))
		}
	}
	out := r.combined()
	if !strings.Contains(out, googleOwned) {
		t.Errorf("import never mentions the resource it skipped:\n%s", out)
	}
	if !strings.Contains(out, "goog-composer-env") {
		t.Errorf("import does not say WHY it was skipped, so a user cannot judge the "+
			"decision or override it:\n%s", out)
	}

	// Named explicitly: adopted. The flag is advisory, and this is the half
	// that makes it advisory rather than a refusal.
	c.mustRun(t, exitOK, "import", "dev", workflowType+"."+ownedID)
	state := stateOf(t, c.dir)
	if len(state) != 1 {
		t.Fatalf("naming a system-owned resource did not adopt it; state holds %v", state)
	}
	for _, r := range state {
		if r.ProviderID != ownedID {
			t.Errorf("adopted %q, want %q", r.ProviderID, ownedID)
		}
	}
}

// serverAdds arms the fake so that the first read of the resource finds an
// extra key inside a nested object -- a field GCP set and this catalog does
// not model.
//
// One-shot, on the item path, so it fires on the create's readback GET: the
// POST goes to the collection, not here. The key it adds stays for the rest
// of the run, because it is written into the stored resource rather than
// into one response.
func serverAdds(t *testing.T, c *cloud, object, key, val string) {
	t.Helper()
	c.fake.OnRequest(channelPath, func() {
		body, ok := c.fake.Get(channelPath)
		if !ok {
			return
		}
		nested, ok := body[object].(map[string]any)
		if !ok {
			nested = map[string]any{}
		}
		nested[key] = val
		body[object] = nested
		c.fake.Seed(channelPath, body)
	})
}
