// Package scripts tests the shell scripts under scripts/. None of them run at
// build time (see fetch-schemas's own header), and none of these tests touch
// the network — fetch-schemas talks to the real Discovery API and GitHub, so
// what's checked here is the only thing cheap to verify ahead of a human
// actually running it: that it parses, that it can run, and that it still
// says what it means.
package scripts

import (
	"archive/tar"
	"compress/gzip"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/infrena/infrena-provider-gcp/internal/catalog"
)

// manifestVersion reads plugin.yaml's version, so these tests follow the manifest
// rather than hard-coding a release number the next bump would silently falsify.
func manifestVersion(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile("../plugin.yaml")
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(string(data), "\n") {
		if v, ok := strings.CutPrefix(line, "version:"); ok {
			return strings.Trim(strings.TrimSpace(v), `"'`)
		}
	}
	t.Fatal("plugin.yaml has no top-level version: line")
	return ""
}

func run(t *testing.T, env []string, script string, args ...string) (string, error) {
	t.Helper()
	cmd := exec.Command("bash", append([]string{script}, args...)...)
	cmd.Env = append(os.Environ(), env...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// TestFetchSchemasIsAValidExecutableScript. A syntax error or a missing
// execute bit would otherwise only surface the first time someone runs it by
// hand.
func TestFetchSchemasIsAValidExecutableScript(t *testing.T) {
	info, err := os.Stat("fetch-schemas")
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&0o111 == 0 {
		t.Errorf("fetch-schemas is not executable: mode %v", info.Mode())
	}
	if out, err := exec.Command("bash", "-n", "fetch-schemas").CombinedOutput(); err != nil {
		t.Errorf("bash -n fetch-schemas: %v\n%s", err, out)
	}
}

// TestFetchSchemasRefusesMPLDirectories is plan decision P4: mmv1/third_party
// and tools are MPL 2.0, not Apache 2.0, and the script must refuse to vendor
// them rather than trust a future reader to remember why.
func TestFetchSchemasRefusesMPLDirectories(t *testing.T) {
	script := readScript(t)
	for _, forbidden := range []string{"mmv1/third_party", "tools"} {
		if !strings.Contains(script, forbidden) {
			t.Errorf("script never mentions %q; it cannot be refusing to vendor it", forbidden)
		}
	}
	if !strings.Contains(script, "exit 1") {
		t.Error("script never exits non-zero, so the MPL check cannot actually refuse anything")
	}
}

// TestFetchSchemasCopiesOnlyYAML. A literal `cp -r` also vendors BUILD.bazel
// and anything else upstream keeps beside the resources, which breaks the
// yaml-only rule even though those files are Apache 2.0 like the rest of
// products/.
func TestFetchSchemasCopiesOnlyYAML(t *testing.T) {
	script := readScript(t)
	for _, line := range strings.Split(script, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue // comments may explain the danger without invoking it
		}
		if strings.Contains(line, "cp -r") {
			t.Errorf("script runs cp -r, which would also vendor BUILD.bazel and anything else upstream keeps beside the resources: %q", line)
		}
	}
	if !strings.Contains(script, "-name '*.yaml'") {
		t.Error("script does not filter to *.yaml files")
	}
}

func readScript(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile("fetch-schemas")
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}
func TestReleaseCheckPassesWhenTagManifestAndBinaryAgree(t *testing.T) {
	v := manifestVersion(t)
	out, err := run(t, nil, "release-check", "v"+v)
	if err != nil {
		t.Fatalf("release-check v%s failed: %v\n%s", v, err, out)
	}
	if !strings.Contains(out, "all say "+v) {
		t.Errorf("release-check did not confirm the agreement:\n%s", out)
	}
}

// TestReleaseCheckRefusesATagTheManifestDoesNotName. Judging a release by a manifest that
// describes a different version is the mistake this check exists to prevent.
func TestReleaseCheckRefusesATagTheManifestDoesNotName(t *testing.T) {
	v := manifestVersion(t)
	out, err := run(t, nil, "release-check", "v99.0.0")
	if err == nil {
		t.Fatalf("release-check accepted v99.0.0 against a manifest saying %s:\n%s", v, out)
	}
	for _, want := range []string{"99.0.0", v} {
		if !strings.Contains(out, want) {
			t.Errorf("the refusal does not name %q:\n%s", want, out)
		}
	}
}

// TestReleaseCheckAcceptsACRLFManifest. A checkout with CRLF line endings must not block a
// genuine release: the manifest's version still agrees, a trailing \r is not a disagreement.
func TestReleaseCheckAcceptsACRLFManifest(t *testing.T) {
	v := manifestVersion(t)
	data, err := os.ReadFile("../plugin.yaml")
	if err != nil {
		t.Fatal(err)
	}
	crlf := strings.ReplaceAll(string(data), "\n", "\r\n")
	manifest := filepath.Join(t.TempDir(), "plugin.yaml")
	if err := os.WriteFile(manifest, []byte(crlf), 0o644); err != nil {
		t.Fatal(err)
	}

	out, err := run(t, []string{"PLUGIN_MANIFEST=" + manifest}, "release-check", "v"+v)
	if err != nil {
		t.Fatalf("release-check refused a CRLF manifest that agrees with the tag: %v\n%s", err, out)
	}
	if !strings.Contains(out, "all say "+v) {
		t.Errorf("release-check did not confirm the agreement:\n%s", out)
	}
}

// TestReleaseCheckRefusesAManifestProtocolTheBinaryDoesNotSpeak. `protocol:` is what this
// release's binary speaks, which for an SDK-built binary is exactly one
// version. Each case keeps the version agreeing, so only the protocol can be the refusal: the
// previous release's [1], left behind after an infrena require bump, and the host's whole
// Supported set, [2, 1], which is what the host supports, not what this binary speaks.
func TestReleaseCheckRefusesAManifestProtocolTheBinaryDoesNotSpeak(t *testing.T) {
	v := manifestVersion(t)
	data, err := os.ReadFile("../plugin.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var current string
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "protocol:") {
			current = line
		}
	}
	if current == "" {
		t.Fatal("plugin.yaml has no top-level protocol: line to doctor")
	}
	// [5] is the previous protocol, the value left behind by an infrena require bump that
	// forgot this line -- the realistic mistake. [6, 5] is the HOST's Supported set, which is
	// what infrena can talk to, not what this binary speaks; an SDK-built binary announces
	// exactly one. [99] is simply not a protocol.
	for _, doctored := range []string{"protocol: [5]", "protocol: [6, 5]", "protocol: [99]"} {
		t.Run(doctored, func(t *testing.T) {
			if doctored == current {
				t.Fatalf("plugin.yaml already says %q, so this case changes nothing", doctored)
			}
			manifest := filepath.Join(t.TempDir(), "plugin.yaml")
			if err := os.WriteFile(manifest, []byte(strings.Replace(string(data), current, doctored, 1)), 0o644); err != nil {
				t.Fatal(err)
			}
			out, err := run(t, []string{"PLUGIN_MANIFEST=" + manifest}, "release-check", "v"+v)
			if err == nil {
				t.Fatalf("release-check accepted %q against the binary's handshake:\n%s", doctored, out)
			}
			if !strings.Contains(out, "speaks protocol") {
				t.Errorf("the refusal does not say which protocol the binary speaks:\n%s", out)
			}
		})
	}
}

// TestReleaseCheckRefusesABinaryThatDoesNotKnowItsVersion. `go build -X` on a symbol that
// does not exist is silently ignored, so a renamed Version variable would ship every
// archive reporting 0.0.0-dev. This simulates exactly that drift.
func TestReleaseCheckRefusesABinaryThatDoesNotKnowItsVersion(t *testing.T) {
	v := manifestVersion(t)
	out, err := run(t,
		[]string{"PLUGIN_VERSION_SYMBOL=github.com/infrena/infrena-provider-aws/internal/gcpplugin.NoSuchVariable"},
		"release-check", "v"+v)
	if err == nil {
		t.Fatalf("release-check passed with a binary whose version was never stamped:\n%s", out)
	}
	if !strings.Contains(out, "0.0.0-dev") {
		t.Errorf("the refusal does not say what the binary reported:\n%s", out)
	}
}

// TestBuildReleaseNamesArchivesByTheInstallConvention. `infrena plugins install` constructs
// the download name rather than reading it, so a wrong name is an uninstallable release.
func TestBuildReleaseNamesArchivesByTheInstallConvention(t *testing.T) {
	v := manifestVersion(t)
	dist := t.TempDir()
	if out, err := run(t, []string{"PLATFORMS=linux/amd64 windows/amd64"}, "build-release", v, dist); err != nil {
		t.Fatalf("build-release failed: %v\n%s", err, out)
	}
	stem := "infrena-plugin-gcp_" + v + "_linux_amd64"
	for _, name := range []string{stem + ".tar.gz", "infrena-plugin-gcp_" + v + "_windows_amd64.zip"} {
		if _, err := os.Stat(filepath.Join(dist, name)); err != nil {
			t.Errorf("missing %s: %v", name, err)
		}
	}

	f, err := os.Open(filepath.Join(dist, stem+".tar.gz"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		got[h.Name] = true
	}
	// README.md does not exist yet (Task 19, docs); the archive carries it once it exists.
	want := []string{stem + "/infrena-plugin-gcp", stem + "/plugin.yaml"}
	if _, err := os.Stat("../README.md"); err == nil {
		want = append(want, stem+"/README.md")
	}
	for _, w := range want {
		if !got[w] {
			t.Errorf("%s.tar.gz does not contain %s; it holds %v", stem, w, got)
		}
	}
}

// infrenaSource locates the infrena checkout scripts/check-examples builds its
// host from, and SKIPS when there is none.
//
// A skip rather than a failure, for the reason e2e/e2e_test.go's TestMain
// gives: a contributor who has not cloned infrena beside this repository has a
// setup problem, not a defect, and until now `go test ./...` here has needed
// no sibling checkout at all. The message is a literal, greppable line so a CI
// job that meant to run this can tell a skip from a pass, which `go test`
// itself cannot without -v.
func infrenaSource(t *testing.T) string {
	t.Helper()
	src := os.Getenv("INFRENA_SRC")
	if src == "" {
		root, err := filepath.Abs("..")
		if err != nil {
			t.Fatal(err)
		}
		src = filepath.Join(filepath.Dir(root), "infrena")
	}
	if _, err := os.Stat(filepath.Join(src, "go.mod")); err != nil {
		t.Skipf("CHECK-EXAMPLES SKIPPED: no infrena module at %s; "+
			"clone it beside this repository or set INFRENA_SRC", src)
	}
	return src
}

// TestCheckExamplesIsAValidExecutableScript, for the same reason
// fetch-schemas has one: a syntax error or a missing execute bit would
// otherwise surface the first time somebody ran it by hand.
func TestCheckExamplesIsAValidExecutableScript(t *testing.T) {
	info, err := os.Stat("check-examples")
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&0o111 == 0 {
		t.Errorf("check-examples is not executable: mode %v", info.Mode())
	}
	if out, err := exec.Command("bash", "-n", "check-examples").CombinedOutput(); err != nil {
		t.Errorf("bash -n check-examples: %v\n%s", err, out)
	}
}

// TestCheckExamplesCompilesEveryExample is the claim examples/ makes: every
// file under it is configuration a user could copy and run.
//
// It is the only test in this repository that reads examples/ at all. The unit
// suites call into the provider and e2e compiles a fixture of its own, so a
// renamed type, an attribute that turned out to be output-only, or a variable
// that stopped resolving is invisible everywhere else until somebody copies
// the file.
func TestCheckExamplesCompilesEveryExample(t *testing.T) {
	infrenaSource(t)
	out, err := run(t, nil, "check-examples")
	if err != nil {
		t.Fatalf("check-examples failed: %v\n%s", err, out)
	}
	// Counted from the directory rather than written as a literal, so adding an
	// example is not also a test edit -- but asserted, because a checker that
	// walked past an example would otherwise pass in exactly the same words.
	want := len(exampleProjects(t))
	if !strings.Contains(out, fmt.Sprintf("all %d examples compile", want)) {
		t.Errorf("check-examples did not report all %d examples compiling:\n%s", want, out)
	}
}

// TestCheckExamplesRefusesAnExampleThatDoesNotCompile. A checker that reports
// success whatever it is fed is worse than no checker, because CI then says
// the examples are fine. gcp.vpc is the shape of the mistake this exists to
// catch: a plausible type name that is not one this plugin serves.
func TestCheckExamplesRefusesAnExampleThatDoesNotCompile(t *testing.T) {
	infrenaSource(t)

	body, err := os.ReadFile("../examples/network/infrena.yml")
	if err != nil {
		t.Fatal(err)
	}
	doctored := strings.Replace(string(body), "type: gcp.network", "type: gcp.vpc", 1)
	if doctored == string(body) {
		t.Fatal("examples/network no longer declares a gcp.network to doctor")
	}
	dir := filepath.Join(t.TempDir(), "examples", "network")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "infrena.yml"), []byte(doctored), 0o644); err != nil {
		t.Fatal(err)
	}

	out, err := run(t, []string{"EXAMPLES_DIR=" + filepath.Dir(dir)}, "check-examples")
	if err == nil {
		t.Fatalf("check-examples accepted an example naming a type this plugin does not serve:\n%s", out)
	}
	if !strings.Contains(out, "do not compile") {
		t.Errorf("the refusal does not say the example failed to compile:\n%s", out)
	}
}

// TestCheckExamplesRefusesADirectoryWithNoExamples. An unmatched glob checks
// nothing, and "checked nothing" must not print the same thing as "checked
// everything and it was fine".
func TestCheckExamplesRefusesADirectoryWithNoExamples(t *testing.T) {
	infrenaSource(t)
	out, err := run(t, []string{"EXAMPLES_DIR=" + t.TempDir()}, "check-examples")
	if err == nil {
		t.Fatalf("check-examples passed over a directory holding no examples:\n%s", out)
	}
	if !strings.Contains(out, "checked nothing") {
		t.Errorf("the refusal does not say that nothing was checked:\n%s", out)
	}
}

// exampleProjects lists every example project directory: one `infrena.yml`
// each, which is what check-examples walks.
func exampleProjects(t *testing.T) []string {
	t.Helper()
	matches, err := filepath.Glob("../examples/*/infrena.yml")
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) == 0 {
		t.Fatal("examples/ holds no */infrena.yml")
	}
	return matches
}

// exampleDocument is as much of an example as these two tests read: the
// resource types it names, the variables it declares, and the provider
// instances whose configuration crosses to the plugin.
//
// `providers` stays a yaml.Node because what is wanted from it is every string
// anywhere inside it, at any depth and under any key -- `defaults:` and a
// plugin's own options included -- and no struct can say that.
type exampleDocument struct {
	Variables map[string]struct {
		// A yaml.Node rather than a bool: `default: false` and no `default:` at
		// all are different declarations, and every other Go type conflates them.
		// A node that was never decoded has Kind zero.
		Default yaml.Node `yaml:"default"`
	} `yaml:"variables"`
	Providers yaml.Node `yaml:"providers"`
	Resources map[string]struct {
		Type string `yaml:"type"`
	} `yaml:"resources"`
}

func readExample(t *testing.T, path string) exampleDocument {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var doc exampleDocument
	if err := yaml.Unmarshal(data, &doc); err != nil {
		t.Fatalf("%s is not valid YAML: %v", path, err)
	}
	return doc
}

// TestEveryExampleNamesTypesTheCatalogServes.
//
// check-examples catches this too, and more thoroughly, but it needs a
// checkout of infrena to build a host from and skips without one. This needs
// nothing but the embedded catalog, so the single most likely way an example
// goes stale -- a regeneration renaming a type, or a plausible name that was
// never one this plugin serves -- is caught on every machine.
func TestEveryExampleNamesTypesTheCatalogServes(t *testing.T) {
	c, err := catalog.Load()
	if err != nil {
		t.Fatal(err)
	}
	// Modules too: a module's resources are as real as a project's, and
	// examples/module/ keeps two of them out of the file check-examples names.
	paths := exampleProjects(t)
	modules, err := filepath.Glob("../examples/*/modules/*/module.yml")
	if err != nil {
		t.Fatal(err)
	}
	paths = append(paths, modules...)

	for _, path := range paths {
		for name, r := range readExample(t, path).Resources {
			if !strings.HasPrefix(r.Type, "gcp.") {
				continue // module.vpc and friends are infrena's, not this catalog's
			}
			if _, ok := c.Type(r.Type); !ok {
				t.Errorf("%s: resource %q is a %s, which this plugin does not serve", path, name, r.Type)
			}
		}
	}
}

// TestEveryProviderVariableInAnExampleHasADefault is the rule that makes an
// example runnable by a reader who has typed nothing yet.
//
// A provider instance's configuration crosses to the plugin's Configure, so
// infrena resolves it for EVERY command -- `discover` included, and `discover`
// takes no environment, so it has no environment-specific value to resolve
// from. A variable with no `default:` is unresolvable there and infrena
// refuses to run rather than let the plugin fall back to whatever project the
// machine's credentials name.
//
// check-examples would catch it in `validate` as well. This says why, in the
// place the rule lives, and without a host binary.
func TestEveryProviderVariableInAnExampleHasADefault(t *testing.T) {
	reference := regexp.MustCompile(`\$\{\s*var\.([A-Za-z_][A-Za-z0-9_]*)`)

	for _, path := range exampleProjects(t) {
		doc := readExample(t, path)
		for _, text := range scalarsOf(&doc.Providers) {
			for _, m := range reference.FindAllStringSubmatch(text, -1) {
				name := m[1]
				v, declared := doc.Variables[name]
				if !declared {
					t.Errorf("%s: `providers:` reads ${var.%s}, which the file does not declare", path, name)
					continue
				}
				if v.Default.Kind == 0 {
					t.Errorf("%s: `providers:` reads ${var.%s}, which is declared with no `default:`. "+
						"Provider configuration is resolved for every command, including `discover`, "+
						"which takes no environment -- so this example refuses to run as committed",
						path, name)
				}
			}
		}
	}
}

// scalarsOf returns every scalar in a YAML subtree, keys included: a variable
// reference is a string wherever it appears, and which key it sat under does
// not change that.
func scalarsOf(node *yaml.Node) []string {
	if node == nil {
		return nil
	}
	if node.Kind == yaml.ScalarNode {
		return []string{node.Value}
	}
	var out []string
	for _, child := range node.Content {
		out = append(out, scalarsOf(child)...)
	}
	return out
}
