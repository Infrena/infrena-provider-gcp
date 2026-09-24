//go:build e2e

// Package e2e drives a REAL infrena binary against a REAL build of this
// plugin, with an in-process fake GCP standing in for Google.
//
// Everything else in this repository tests the plugin by calling into it.
// Even internal/gcpplugin's protocol test, which goes through infrena's own
// in-process host, is still one process with the plugin linked in. This suite
// is the only place where the thing under test is what a user actually runs:
// a compiled `infrena`, launching a compiled `infrena-plugin-gcp` as a child
// process over a pipe, compiling real YAML, writing real state to disk and
// planning against it a second time.
//
// WHAT ONLY THIS SUITE CAN PROVE. A plan converging is not a property of any
// one function: it is the claim that Create's answer, written to state, read
// back by Read, reconciled against configuration by the compiler and diffed
// by the planner, produces no change. Six units each behaving correctly do
// not add up to that, and no unit test can express it, because the second
// plan is the only observation that ever asks the question.
//
// It is behind `//go:build e2e` because it builds two binaries and is
// therefore slow, and because it needs a checkout of infrena to build the
// host from. TestMain writes a literal "E2E SKIPPED:" line to stderr and
// exits 0 when it cannot build; CI greps for that line.
//
// NOTHING HERE REACHES REAL GCP. The plugin is pointed at the fake through
// GOOGLE_API_ENDPOINT_OVERRIDE, which also stops it resolving credentials at
// all (gcpplugin.EndpointOverrideEnv). The live-GCP suite is Task 18.
package e2e

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/infrena/infrena-provider-gcp/internal/gcpfake"
	"github.com/infrena/infrena-provider-gcp/internal/gcpplugin"
)

// infrenaSrcEnv names the infrena checkout the host binary is built from.
//
// It defaults to the sibling checkout, which is routinely AHEAD of any
// released tag -- so a pass here says this plugin works with infrena's
// working tree, and says nothing about any release. To exercise a release,
// point this at a `git archive` of that tag in a temp directory. GOWORK=off
// does NOT make this suite pinned: it changes which infrena the plugin's own
// `require` resolves to, not which one TestMain builds.
const infrenaSrcEnv = "INFRENA_SRC"

// Exit codes, from infrena's own documented set: 0 nothing to do, 1 failed,
// 2 there are (or were) changes. They are a product API, so naming them here
// rather than writing bare integers is what makes a failing assertion legible.
const (
	exitOK      = 0
	exitError   = 1
	exitChanges = 2
)

var (
	infrenaBin string
	pluginDir  string
)

// TestMain builds both binaries once.
//
// A build failure SKIPS the suite rather than failing it: a contributor who
// has not cloned infrena beside this repository has a setup problem, not a
// defect. The skip is a literal, greppable line on stderr naming what was
// missing, so a CI job that was meant to run this suite can tell "skipped"
// from "passed" -- which `go test` itself cannot, since a package whose
// TestMain exits 0 reports ok either way.
//
// CI MUST RUN THIS PACKAGE WITH -v, or there is nothing to grep: `go test`
// buffers a package's own output and prints it only on failure or under -v,
// so a silent skip looks exactly like a pass. The test binary cannot get
// around that, which is why it is said here rather than assumed.
func TestMain(m *testing.M) {
	if err := build(); err != nil {
		fmt.Fprintf(os.Stderr, "E2E SKIPPED: %v\n", err)
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func build() error {
	root, err := repoRoot()
	if err != nil {
		return err
	}
	src := os.Getenv(infrenaSrcEnv)
	if src == "" {
		src = filepath.Join(filepath.Dir(root), "infrena")
	}
	if _, err := os.Stat(filepath.Join(src, "go.mod")); err != nil {
		return fmt.Errorf("no infrena module at %s; clone it beside this repository or set %s", src, infrenaSrcEnv)
	}

	out, err := os.MkdirTemp("", "infrena-e2e-*")
	if err != nil {
		return err
	}

	// A WORKSPACE JOINING THE TWO CHECKOUTS, written here rather than relying
	// on either repository's own go.work.
	//
	// It is what makes the host and the plugin the SAME infrena. This repo's
	// go.mod requires a released infrena; built without the workspace, the
	// plugin would speak the SDK of that release while the host under test is
	// the working tree, and a protocol change would be tested against the
	// protocol it replaced -- by two processes that each still agree with
	// themselves. It is also why GOWORK is set explicitly on both builds: a
	// go.work sitting in either checkout must not be able to change what is
	// built.
	work := filepath.Join(out, "go.work")
	if err := os.WriteFile(work, []byte(workspace(root, src)), 0o644); err != nil {
		return err
	}

	infrenaBin = filepath.Join(out, "infrena")
	if err := goBuild(src, work, infrenaBin, "./cmd/infrena"); err != nil {
		return fmt.Errorf("building infrena from %s: %w", src, err)
	}
	// The binary NAME is how the host finds a plugin, so it is not arbitrary.
	pluginDir = out
	if err := goBuild(root, work, filepath.Join(out, "infrena-plugin-"+gcpplugin.PluginName), "./cmd/infrena-plugin-gcp"); err != nil {
		return fmt.Errorf("building the plugin: %w", err)
	}
	return nil
}

func goBuild(dir, work, bin, pkg string) error {
	cmd := exec.Command("go", "build", "-o", bin, pkg)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GOWORK="+work)
	if combined, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("%v\n%s", err, combined)
	}
	return nil
}

// workspace reads the `go` directive from this repository rather than writing
// a literal: a workspace declaring a version below either module's is refused
// outright, so a hardcoded one turns the next toolchain bump into a failure
// in a file nobody would think to look in.
func workspace(root, src string) string {
	version := "1.27.0"
	if data, err := os.ReadFile(filepath.Join(root, "go.mod")); err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			if rest, ok := strings.CutPrefix(strings.TrimSpace(line), "go "); ok {
				version = strings.TrimSpace(rest)
				break
			}
		}
	}
	return "go " + version + "\n\nuse (\n\t" + root + "\n\t" + src + "\n)\n"
}

func repoRoot() (string, error) {
	wd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	return filepath.Dir(wd), nil // e2e/ -> repo root
}

type result struct {
	Stdout   string
	Stderr   string
	ExitCode int
}

func (r result) combined() string { return r.Stdout + r.Stderr }

// cloud is one running fake GCP plus the project directory pointed at it.
type cloud struct {
	fake *gcpfake.Server
	dir  string
}

// start writes the testdata project into a temp directory and brings up the
// fake.
func start(t *testing.T) *cloud {
	t.Helper()
	body, err := os.ReadFile(filepath.Join("testdata", "project", "infrena.yml"))
	if err != nil {
		t.Fatalf("reading the project fixture: %v", err)
	}
	dir := t.TempDir()
	write(t, dir, "infrena.yml", string(body))
	fake := gcpfake.New(t)
	// The only resource the fixture MUTATES is an Eventarc trigger, and Eventarc
	// answers every create, patch and delete with a google.longrunning.Operation.
	// The compute resources some tests seed are read and discovered, never
	// changed, so this is the one shape the fake ever needs to speak.
	fake.SetOperationStyle(gcpfake.OpLongRunning)
	return &cloud{fake: fake, dir: dir}
}

// run executes the real infrena binary against the fake.
func (c *cloud) run(t *testing.T, args ...string) result {
	t.Helper()
	full := append([]string{"--chdir", c.dir, "--plugin-dir", pluginDir}, args...)
	cmd := exec.Command(infrenaBin, full...)
	cmd.Env = append(os.Environ(),
		gcpplugin.EndpointOverrideEnv+"="+c.fake.URL()+"/",
		// Belt and braces. The override already stops the plugin resolving
		// Application Default Credentials at all, but a suite whose whole
		// promise is "reaches no real cloud" should not depend on one switch:
		// these are the same variables internal/gcptest.Isolate clears, and
		// with them cleared there is no identity on the machine to find.
		"GOOGLE_APPLICATION_CREDENTIALS=",
		"CLOUDSDK_CONFIG="+t.TempDir(),
		"GCE_METADATA_HOST=127.0.0.1:1",
		"GOOGLE_CLOUD_PROJECT=",
	)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	code := 0
	if err := cmd.Run(); err != nil {
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) {
			t.Fatalf("running infrena %v: %v", args, err)
		}
		code = exitErr.ExitCode()
	}
	return result{Stdout: stdout.String(), Stderr: stderr.String(), ExitCode: code}
}

// mustRun runs and fails the test unless the exit code is want.
func (c *cloud) mustRun(t *testing.T, want int, args ...string) result {
	t.Helper()
	r := c.run(t, args...)
	if r.ExitCode != want {
		t.Fatalf("infrena %s: exit = %d, want %d\n%s", strings.Join(args, " "), r.ExitCode, want, r.combined())
	}
	return r
}

func write(t *testing.T, dir, name, body string) {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("writing %s: %v", name, err)
	}
}

// change is one proposed operation from a plan artifact: what it would do,
// to which address, of which type.
type change struct {
	address string
	typ     string
	action  string
}

// planChanges returns what an `infrena plan --output` run proposes.
//
// The file is a REPORT STREAM, not a bare JSON document: one NDJSON line per
// event, with the plan artifact riding on the line typed "plan" so that a
// frontend tails one format for every command. Decoding the whole file as a
// document fails, and grepping the raw bytes matches text from the meta line
// and the envelope too -- an assertion at that level is about the wrong
// thing and can pass for the wrong reason. Every assertion here goes through
// this one parser, so an envelope change breaks one place.
func planChanges(t *testing.T, path string) []change {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading the plan report at %s: %v", path, err)
	}
	for _, line := range bytes.Split(data, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		var envelope struct {
			Type string `json:"type"`
			Plan struct {
				Operations []struct {
					Address string `json:"address"`
					Type    string `json:"type"`
					Kind    string `json:"kind"`
				} `json:"operations"`
			} `json:"plan"`
		}
		if err := json.Unmarshal(line, &envelope); err != nil || envelope.Type != "plan" {
			continue
		}
		out := make([]change, 0, len(envelope.Plan.Operations))
		for _, op := range envelope.Plan.Operations {
			out = append(out, change{address: op.Address, typ: op.Type, action: op.Kind})
		}
		return out
	}
	t.Fatalf("%s carries no plan line:\n%s", path, data)
	return nil
}

// stateResource is one entry of an environment's state file, read as JSON.
//
// This suite decodes state by hand rather than importing infrena's own state
// package, for the same reason infrena's integration suite does: it is a
// suite about what the BINARY does, so the binary's output is the only thing
// it is allowed to consult. Importing the package would let a change that
// breaks the written format pass, because both sides would have moved
// together.
type stateResource struct {
	Type       string `json:"type"`
	ProviderID string `json:"provider_id"`
	Attributes map[string]struct {
		Raw json.RawMessage `json:"raw"`
	} `json:"attributes"`
}

func stateOf(t *testing.T, dir string) map[string]stateResource {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, ".infrena", "state", "dev.json"))
	if err != nil {
		t.Fatalf("reading state: %v", err)
	}
	var st struct {
		Resources map[string]stateResource `json:"resources"`
	}
	if err := json.Unmarshal(data, &st); err != nil {
		t.Fatalf("decoding state: %v", err)
	}
	return st.Resources
}

func providerIDOf(t *testing.T, dir, name string) string {
	t.Helper()
	r, ok := stateOf(t, dir)[name]
	if !ok {
		t.Fatalf("state holds no resource named %q", name)
	}
	return r.ProviderID
}

func readFile(t *testing.T, dir, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		t.Fatalf("reading %s: %v", name, err)
	}
	return string(data)
}

// cut returns everything in body before the line beginning with marker, which
// is how a resource block is removed from the fixture without a second copy
// of the fixture drifting out of step with the first.
func cut(body, marker string) string {
	if i := strings.Index(body, "\n"+marker); i >= 0 {
		return body[:i+1]
	}
	return body
}
