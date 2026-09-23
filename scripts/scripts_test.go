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
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
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
