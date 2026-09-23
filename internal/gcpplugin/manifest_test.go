package gcpplugin

import (
	"os"
	"testing"

	"github.com/infrena/infrena/pkg/pluginmanifest"
	"github.com/infrena/infrena/pkg/pluginproto"
	"github.com/infrena/infrena/pkg/semver"
)

// readManifest parses the repository's plugin.yaml with infrena's own parser: the same code
// `infrena plugins install` runs, so a manifest that passes here is one install accepts rather
// than one a second parser merely agreed with.
func readManifest(t *testing.T) *pluginmanifest.Manifest {
	t.Helper()
	data, err := os.ReadFile("../../plugin.yaml")
	if err != nil {
		t.Fatal(err)
	}
	m, warnings, err := pluginmanifest.Parse(data)
	if err != nil {
		t.Fatalf("plugin.yaml is not a manifest infrena accepts: %v", err)
	}
	if len(warnings) != 0 {
		t.Errorf("plugin.yaml parses with warnings, which install would print: %v", warnings)
	}
	return m
}

func TestTheManifestDescribesThisPlugin(t *testing.T) {
	m := readManifest(t)
	if m.Name != PluginName {
		t.Errorf("plugin.yaml names %q, but this plugin is %q", m.Name, PluginName)
	}
	// Exactly, not "includes": `protocol:` is what THIS release's binary speaks, and an
	// SDK-built binary announces exactly one.
	if len(m.Protocol) != 1 || m.Protocol[0] != pluginproto.Version {
		t.Errorf("plugin.yaml's protocol is %v, but this SDK speaks exactly [%d]; change it in the "+
			"same commit as go.mod's infrena require", m.Protocol, pluginproto.Version)
	}
}

// TestTheFloorAdmitsTheOldestHostThatWorks pins the boundary so that raising the floor is a
// deliberate act with a failing test attached, never a side effect of bumping the require.
//
// It did its job on 2026-09-23: raising the floor to 0.14.1 failed this test, which is the
// point of it. The boundary moved for a reason recorded here and in plugin.yaml.
//
// THE FLOOR IS NO LONGER THE PROTOCOL BOUNDARY. 0.14.0 is still the oldest release that speaks
// protocol 5, so a 0.14.0 host and this binary do handshake and run. They just get two things
// wrong, both silently:
//
//   - The generator emits a snake_case alias for every camelCase attribute, 1447 at the top level
//     and 9788 nested. 0.14.0 resolves aliases only on top-level attributes, so those 9788 do
//     nothing and a nested `trust_config:` goes to Google verbatim.
//   - An unresolved nested key is passed through to the provider instead of being reported.
//
// "Works" has to mean the host honours what the catalog declares, not merely that it connects.
// A host that starts and then quietly ignores 87% of this plugin's aliases is the "looks safe but
// is not" case, which is worse than a refused handshake because nothing says a word.
func TestTheFloorAdmitsTheOldestHostThatWorks(t *testing.T) {
	m := readManifest(t)
	admits := func(s string) bool {
		v, err := semver.Parse(s)
		if err != nil {
			t.Fatalf("bad version in test: %v", err)
		}
		return m.Infrena.Allows(v)
	}
	if !admits("0.14.1") {
		t.Error("the floor refuses 0.14.1, the oldest release that resolves nested aliases")
	}
	if admits("0.14.0") {
		t.Error("the floor admits 0.14.0, which speaks protocol 5 but silently ignores every " +
			"nested alias this catalog declares")
	}
	if admits("0.13.1") {
		t.Error("the floor admits 0.13.1, which does not speak protocol 5")
	}
}
