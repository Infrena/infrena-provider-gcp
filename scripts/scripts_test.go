// Package scripts tests the shell scripts under scripts/. None of them run at
// build time (see fetch-schemas's own header), and none of these tests touch
// the network — fetch-schemas talks to the real Discovery API and GitHub, so
// what's checked here is the only thing cheap to verify ahead of a human
// actually running it: that it parses, that it can run, and that it still
// says what it means.
package scripts

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

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
