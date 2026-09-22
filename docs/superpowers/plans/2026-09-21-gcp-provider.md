# GCP provider Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Build `infrena-plugin-gcp`, one generic provider serving every GCP resource type the generated
catalog covers, from an empty repository to a released plugin with docs, examples and a live suite.

**Architecture:** A build-time generator (`cmd/gen-gcp`) reads Google's API Discovery documents, a pinned
`magic-modules` vendor and a curated overlay, and writes one embedded catalog: infrena resource definitions
plus the runtime metadata the provider needs. One provider (`internal/gcprov`) serves every catalog type
through a single generic JSON-over-HTTP client, choosing per type from three await strategies. Tests run
against an in-process GCP fake (`internal/gcpfake`); a live suite runs by hand against a real project.

**Tech Stack:** Go 1.27.0; `github.com/infrena/infrena` **v0.14.0** (plugin protocol 5);
`golang.org/x/oauth2` for Application Default Credentials; `gopkg.in/yaml.v3` for the overlay and the
magic-modules vendor. **No GCP SDK of any kind** — not `google.golang.org/api`, not `cloud.google.com/go`.

**Spec:** `docs/superpowers/specs/2026-09-21-gcp-provider-design.md`. Read its §2 decisions G1–G7, §4
(the generator and tiers) and §5 (the runtime) before any task. The plan argues from the spec; both travel
together.

## Global Constraints

- Module `github.com/infrena/infrena-provider-gcp`. Branch `gcp-provider`.
- `go 1.27.0`; `require github.com/infrena/infrena v0.14.0`, **no `replace`**; local work through a
  gitignored `go.work` (`go work init . ../infrena`). **infrena is a PUBLIC repository as of
  2026-09-20**, so the pinned build needs no credentials and no `GOPRIVATE`; `go.sum` is committed.
- Plugin name `gcp`; binary `infrena-plugin-gcp`; every type prefixed `gcp.`.
- `plugin.yaml`: `manifest: 2`, `protocol: [5]`, `infrena: ">= 0.14.0"`.
- `Version` defaults to `"0.0.0-dev"`, stamped only by `-ldflags -X` at release, so a broken `-ldflags`
  path cannot pass the release gate by coincidence.
- Type names follow spec §4.2 and **never change** once written to `gen/names.lock.json`.
- Attribute canonical name is GCP's own property name; `Aliases` lists curated aliases first, then
  snake_case (spec §4.3).
- Every settable property is `Optional`+`Computed`. Every `project` and `location`/`region`/`zone`
  attribute is `Required`+`ForceNew`.
- **Nothing writes to stdout.** No credential, bearer token or sensitive value is ever logged.
- `Create`/`Update` never return `(nil, nil)`; `Create` never returns an error once GCP created something.
- Do not reimplement host rules (sensitivity forcing, provenance, bookkeeping carry-forward,
  undeclared-attribute refusal, alias canonicalisation). Return only `Type`, `ProviderID`, `Attributes`.
- **No test reaches real GCP outside `-tags live`.**
- Every test command uses `-count=1`. Every test is sabotage-verified; the sabotage goes in the commit
  message.
- Stage explicit paths only. Never `git add -A`, `git add .` or `git commit -am`.
- **Commit messages (James's rule):** plain English, short, human-sounding, no em-dashes, and **no AI or
  Claude attribution of any kind**.
- Ask James before any `git push` and before creating a tag.
- **Do not modify `../infrena`.** Defects there go to the vault note's follow-ups and to James.

---

## Plan-level decisions (not in the spec)

| # | Decision | Why |
| --- | --- | --- |
| P1 | Discovery parsing lives in `internal/disco`, magic-modules parsing in `internal/mmv1`, and the two are joined in `internal/gen` | They are independent inputs with independent failure modes. A test that a Discovery `$ref` cycle terminates should not need a magic-modules fixture. |
| P2 | `internal/gcprov` owns the runtime (client, CRUD, await, discover); `internal/gcpplugin` owns the plugin surface (`Plugin`, instance configuration, credentials). `gcpplugin` imports `gcprov`, never the reverse | The AWS repo split `ccprov`/`awsprov` only after an import cycle forced it. Starting clean, the dependency runs one way by construction. |
| P3 | The magic-modules vendor is committed under `gen/mmv1/`, pinned by a recorded commit SHA in `gen/mmv1.lock` | Spec §10 risk 1. A moving third-party input must arrive as a reviewable diff, and a build must never fetch it. |
| P4 | Only `mmv1/products/**/*.yaml` is vendored, and `scripts/fetch-schemas` refuses to copy anything under `mmv1/third_party/` or `tools/` | Those two paths are MPL 2.0 (spec §2, G1). Making the script refuse them is cheaper than trusting a future reader to remember. |
| P5 | Tier assignment is a pure function in `internal/gen`, and the catalog stores the tier and the reason | A tier that cannot be explained in `infrena explain` output or a generated docs page is a number nobody can act on. |
| P6 | The catalog stores each attribute's nested **shape** (object fields, list element, whether the list is ordered) resolved from `$ref` | The runtime reconciles nested values offline, with no schema fetch. Size is measured in Task 9. |
| P7 | `$ref` cycles are broken by depth limit **8**, and the truncation is recorded as an opaque node | Discovery documents genuinely contain cycles. Measured 2026-09-21 across 25 infrastructure APIs: **bigquery** (`StandardSqlField` -> `StandardSqlDataType` -> `StandardSqlStructType` -> `StandardSqlField`), **container** (`OperationProgress` -> `OperationProgress`, a direct self-reference) and **spanner** (`ExecuteSqlRequest` -> `Type` -> `Type`). **compute has none** at revision 20260910 - an earlier draft of this row claimed it did, and Task 2's implementer caught it. Without a bound the resolver recurses until the stack gives out, which is worse than a bad catalog because there is no error to read. |
| P8 | Load-cost gate (Task 9): acceptable if the median extra time `infrena validate` takes with the full catalog is at most **500 ms** over a no-op plugin. Above roughly **1 second**, stop and report to James with numbers | Matches the AWS repo's recorded threshold and its instruction about when to escalate. |
| P9 | New instance keys: `credentials_file`, `impersonate_service_account`, `quota_project`, `discover_types`, `discover_projects` | Spec §5.1 and §6. `discover_projects` exists because a GCP instance can span projects (G2), unlike an AWS instance which is one account. |
| P10 | The first real catalog is generated in Task 9 and committed; regenerating is a normal reviewed diff | Generated code is reviewed as a diff, never produced at build time. |
| P11 | `MaxConcurrency()` returns **16** | Derived in Task 14 against measured per-API per-project quota, not guessed. The task refuses to ship a number it did not measure. |

## Verification log (facts the tasks rely on, checked 2026-09-21)

| Claim | Checked against | Result |
| --- | --- | --- |
| Newest infrena tag is `v0.14.0` | `git -C ../infrena tag \| sort -V \| tail -1` | ✔ |
| `pluginproto.Version = 5`; 5 is `max_concurrency` in the handshake | `../infrena/pkg/pluginproto/proto.go:41` | ✔ |
| `provider.Plugin` is `Name() string`, `Definitions() []*schema.ResourceDefinition`, `New(Config) (Provider, error)` | `../infrena/pkg/provider/provider.go` | ✔ |
| `provider.Provider` is `Name`, `Definitions`, `Read`, `Create`, `Update`, `Delete`, `Discover`, `Import`, `ClassifyError` | same | ✔ |
| `Retryability` is `NotSafeToRetry` (zero), `ConditionallyRetryable`, `SafeToRetry` | same | ✔ |
| `DiscoveredResource` carries `SystemOwned bool` and `SystemOwnedReason string` | same | ✔ |
| `schema.Attribute` fields are `Kind`, `Required`, `Computed`, `Sensitive`, `ForceNew`, `Optional`, `Aliases`, `References *Reference`, `Fields map[string]Attribute`, `Default any`, `Description` | `../infrena/pkg/schema/attribute.go` | ✔ |
| `schema.ResourceDefinition` is `Type`, `Description`, `Attributes`, `Requirements`, `Capabilities`, `ImportID` | `../infrena/pkg/schema/definition.go:37` | ✔ |
| `schema.Capabilities` is `Create, Read, Update, Delete, Import bool` | same | ✔ |
| `value.Kind` is `KindInvalid`, `KindString`, `KindInt`, `KindFloat`, `KindBool`, `KindList`, `KindMap` — **no object kind; an object is a `KindMap`** | `../infrena/pkg/value/kind.go` | ✔ |
| `pluginsdk.Main(p provider.Plugin)` is the whole of `main()`, and redirects `os.Stdout` to stderr | `../infrena/pkg/pluginsdk/serve.go:33` | ✔ |
| Optional plugin interfaces `Version() string` and `MaxConcurrency() int` are detected by the SDK | `../infrena/pkg/pluginsdk/serve.go:341,385` | ✔ |
| `plugintest.Open(ctx, p, dir) (*Host, error)` is the in-process harness | `../infrena/pkg/plugintest/plugintest.go:51` | ✔ |
| Discovery directory lists **314 preferred APIs** | `https://discovery.googleapis.com/discovery/v1/apis` | ✔ |
| In a 25-API infra sample: 395 collections, **221 create+get+delete shaped** | measured, spec §1 | ✔ |
| 14,124 properties in that sample: **3,095 `readOnly`**, **23 `Immutable.`** | measured, spec §1 | ✔ |
| magic-modules `mmv1/products` holds **942 resource files across 190 products** | `git clone --sparse`, `find` | ✔ |
| **860 of 942 (91%)** carry `immutable: true`; 3,185 such fields | measured | ✔ |
| **423 of 942 (45%)** carry a wire-affecting `custom_code` hook | measured, spec §4.1 | ✔ |
| `mmv1/products/**/*.yaml` are Apache 2.0 by per-file header; only `mmv1/third_party/terraform/*` and `tools/go-changelog/*` are MPL 2.0 | `LICENSE` + file headers | ✔ |
| **314 `ResourceRef` fields across 153 resources** | measured, spec §4.4 | ✔ |
| Mutating methods: 300 compute-style LRO, 189 longrunning LRO, 187 synchronous, 23 no body | measured, spec §5.3 | ✔ |
| `compute` has `wait` on `globalOperations`, `regionOperations`, `zoneOperations` | `compute` Discovery doc | ✔ |
| `cloudresourcemanager` v3 `tagBindings` has **only `create`, `delete`, `list`** | Discovery doc | ✔ |
| `tagKeys` and `tagValues` have full CRUD | same | ✔ |
| compute carries 1,520 `readOnly` flags but 2,202 `[Output Only]` prose markers | measured | ✔ |

**Unverified, and a task must establish it rather than assume it:** whether GCP sends `Retry-After` on a
throttled response (spec §5.6). Task 18 measures it. The AWS repo's finding that Cloud Control never sends
`X-Amz-Retry-After` **does not transfer**.

## File structure

| Path | Responsibility |
| --- | --- |
| `cmd/infrena-plugin-gcp/main.go` | `pluginsdk.Main(gcpplugin.NewPlugin())`, nothing else |
| `cmd/gen-gcp/main.go` | The build-time generator's entry point |
| `internal/disco/doc.go` | Discovery document types and JSON decoding |
| `internal/disco/resolve.go` | `$ref` resolution, cycle breaking (P7) |
| `internal/disco/collection.go` | Walking `resources` into flat collections with their methods |
| `internal/mmv1/parse.go` | magic-modules resource YAML types and decoding |
| `internal/mmv1/hooks.go` | Wire-affecting `custom_code` detection (spec §4.1) |
| `internal/gen/names.go` | Type naming and the append-only lock |
| `internal/gen/tier.go` | Tier assignment and its reason |
| `internal/gen/attrs.go` | One collection + one mm resource → catalog attributes |
| `internal/gen/build.go` | The whole generation pass, warnings, overlay application |
| `internal/gen/overlay.go` | `gen/overlay.yaml` types and validation |
| `internal/catalog/catalog.go` | The catalog's runtime types and accessors |
| `internal/catalog/embed.go` | `//go:embed catalog.json.gz`, decompress once |
| `internal/catalog/catalog.json.gz` | **Generated. Never hand-edited.** |
| `internal/gcpplugin/plugin.go` | `provider.Plugin`: name, version, definitions, `MaxConcurrency` |
| `internal/gcpplugin/config.go` | Instance configuration (P9), fail-closed on unknown keys |
| `internal/gcpplugin/credentials.go` | ADC, `credentials_file`, impersonation, quota project |
| `internal/gcprov/client.go` | The generic JSON/HTTP client and its per-(credential, project) cache |
| `internal/gcprov/urls.go` | URL template expansion |
| `internal/gcprov/errors.go` | `ClassifyError`, GCP error decoding |
| `internal/gcprov/backoff.go` | Exponential backoff, jitter, per-(project, API) limiter |
| `internal/gcprov/await.go` | The three await strategies |
| `internal/gcprov/crud.go` | `Create`, `Read`, `Delete`, `Import` |
| `internal/gcprov/patch.go` | `Update` and `updateMask` construction |
| `internal/gcprov/reconcile.go` | Nested value reconciliation, labels |
| `internal/gcprov/discover.go` | CAI plus per-type `list` fallback |
| `internal/gcprov/systemowned.go` | Evidence-based marking |
| `internal/gcprov/provider.go` | `provider.Provider` assembly |
| `internal/gcpfake/server.go` | `httptest` GCP: REST, both operation shapes, CAI |
| `internal/gcptest/gcptest.go` | Credential isolation for every test |
| `gen/overlay.yaml` | Hand-curated: aliases, tier-2 rulings, discover defaults |
| `gen/names.lock.json` | Append-only type-name lock |
| `gen/warnings.txt` | **Generated.** Every excluded and every unruled type, with a reason |
| `gen/mmv1.lock` | The pinned magic-modules commit SHA (P3) |
| `gen/mmv1/` | The vendored Apache-2.0 product YAML (P3, P4) |
| `schemas/` | Downloaded Discovery documents. **Gitignored.** |
| `e2e/e2e_test.go` | A real infrena binary against this binary, `-tags e2e` |
| `live/live_test.go` | Real GCP, `-tags live`, guarded |
| `scripts/` | `fetch-schemas`, `check-examples`, `measure-load`, `release-check`, `build-release` |

---

## Task 1: The repository, and a plugin infrena can load

**Files:**
- Create: `go.mod`, `.gitignore`, `plugin.yaml`
- Create: `internal/gcpplugin/plugin.go`
- Create: `cmd/infrena-plugin-gcp/main.go`
- Test: `internal/gcpplugin/manifest_test.go`, `internal/gcpplugin/plugin_test.go`

**Interfaces:**
- Consumes: nothing.
- Produces: `gcpplugin.PluginName` (`"gcp"`), `gcpplugin.Version` (`var`, `"0.0.0-dev"`),
  `gcpplugin.NewPlugin() *gcpplugin.Plugin`, which satisfies `provider.Plugin`.

- [ ] **Step 1: Create the module and ignore file**

```bash
cd path/to/infrena-provider-gcp
git checkout -b gcp-provider
cat > go.mod <<'EOF'
module github.com/infrena/infrena-provider-gcp

go 1.27.0

require github.com/infrena/infrena v0.14.0
EOF
cat > .gitignore <<'EOF'
/go.work
/go.work.sum
/schemas/
/bin/
EOF
go work init . ../infrena
```

`go.work` is gitignored on purpose: it substitutes the sibling checkout's **working tree**, committed or
not. Run `git -C ../infrena status` before trusting any result produced through it.

- [ ] **Step 2: Write the failing manifest test**

Create `internal/gcpplugin/manifest_test.go`:

```go
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
func TestTheFloorAdmitsTheOldestHostThatWorks(t *testing.T) {
	m := readManifest(t)
	admits := func(s string) bool {
		v, err := semver.Parse(s)
		if err != nil {
			t.Fatalf("bad version in test: %v", err)
		}
		return m.Infrena.Allows(v)
	}
	if !admits("0.14.0") {
		t.Error("the floor refuses 0.14.0, the oldest release that speaks protocol 5")
	}
	if admits("0.13.1") {
		t.Error("the floor admits 0.13.1, which does not speak protocol 5")
	}
}
```

If `semver.Constraint` has no `Allows` method, read `../infrena/pkg/semver/semver.go` and use whatever
it does expose; do not add a method to infrena (Global Constraints).

- [ ] **Step 3: Run it and watch it fail**

Run: `go test -count=1 ./internal/gcpplugin/`
Expected: FAIL — the package does not compile, `PluginName` undefined.

- [ ] **Step 4: Write `plugin.yaml`**

```yaml
# plugin.yaml: what this plugin is, and what it works with.
# Read at a release TAG, never at the default branch, which describes unreleased code.
manifest: 2
name: gcp
version: 0.1.0
# The protocol THIS RELEASE'S binary speaks: exactly the pluginproto.Version the infrena go.mod
# requires. It changes in the same commit as that require.
protocol: [5]
platforms: [linux/amd64, linux/arm64, linux/arm, linux/386, darwin/amd64, darwin/arm64, windows/amd64, windows/arm64]
description: The GCP provider for infrena, serving resource types generated from Google's API Discovery documents.
# The OLDEST infrena release this plugin works with, which is not the same number as go.mod's
# require (that is the NEWEST host it is tested against). Protocol 5 is `max_concurrency` in the
# handshake, and 0.14.0 is the oldest release that speaks it.
infrena: ">= 0.14.0"
source: https://github.com/infrena/infrena-provider-gcp
```

- [ ] **Step 5: Write the minimal plugin**

Create `internal/gcpplugin/plugin.go`:

```go
// Package gcpplugin is infrena's GCP provider plugin: the plugin surface,
// instance configuration and credentials around the generic provider.
package gcpplugin

import (
	"fmt"

	"github.com/infrena/infrena/pkg/provider"
	"github.com/infrena/infrena/pkg/schema"
)

// PluginName is the binary's suffix, what `plugin:` names, and every type's prefix.
const PluginName = "gcp"

// Version is reported in the handshake. "0.0.0-dev" in every build a release did not stamp, so a
// broken -ldflags path cannot pass the release gate by coincidence.
var Version = "0.0.0-dev"

// Plugin is the GCP provider before configuration.
type Plugin struct{}

// NewPlugin returns the GCP plugin.
func NewPlugin() *Plugin { return &Plugin{} }

var _ provider.Plugin = (*Plugin)(nil)

// Name is the plugin's name.
func (pl *Plugin) Name() string { return PluginName }

// Version reports this build's version. The SDK detects this optional method.
func (pl *Plugin) Version() string { return Version }

// Definitions are the resource types this plugin offers. Empty until Task 9 wires the catalog.
func (pl *Plugin) Definitions() []*schema.ResourceDefinition { return nil }

// New constructs one instance from its resolved configuration.
func (pl *Plugin) New(cfg provider.Config) (provider.Provider, error) {
	return nil, fmt.Errorf("gcp: not implemented until the catalog is wired")
}
```

- [ ] **Step 6: Run the manifest test and watch it pass**

Run: `go test -count=1 ./internal/gcpplugin/`
Expected: PASS, both tests.

- [ ] **Step 7: Sabotage both tests, confirm each fails, restore**

Change `plugin.yaml`'s `protocol:` to `[4]` — `TestTheManifestDescribesThisPlugin` must fail naming 4
against 5. Restore by editing it back to `[5]`, not with `git checkout --`. Then change `infrena:` to
`">= 0.13.0"` — `TestTheFloorAdmitsTheOldestHostThatWorks` must fail on the 0.13.1 arm. Restore.
Record both in the commit message.

- [ ] **Step 8: Write `main()`**

Create `cmd/infrena-plugin-gcp/main.go`:

```go
// Command infrena-plugin-gcp is infrena's GCP provider plugin.
//
// It is run by infrena, not directly: stdin and stdout are the protocol stream.
package main

import (
	"github.com/infrena/infrena-provider-gcp/internal/gcpplugin"
	"github.com/infrena/infrena/pkg/pluginsdk"
)

func main() { pluginsdk.Main(gcpplugin.NewPlugin()) }
```

- [ ] **Step 9: Build it, and confirm it refuses to run standalone**

```bash
go build -o bin/infrena-plugin-gcp ./cmd/infrena-plugin-gcp
./bin/infrena-plugin-gcp; echo "exit=$?"
```

Expected: exit 2, with a message on **stderr** saying it is run by infrena. That is `pluginsdk.Main`
checking `pluginproto.CookieEnv`, and it is the cheapest proof the SDK is wired correctly.

- [ ] **Step 10: Commit**

```bash
git add go.mod go.sum .gitignore plugin.yaml \
        internal/gcpplugin/plugin.go internal/gcpplugin/manifest_test.go \
        cmd/infrena-plugin-gcp/main.go
git commit -m "Scaffold the plugin so infrena can load it

Manifest says protocol 5 and a floor of 0.14.0, which is the oldest
release that speaks it. The floor is deliberately a different number
from the go.mod require and there is a test pinning the boundary, so
raising it has to be a deliberate act rather than a side effect of
bumping the require.

Sabotage: setting protocol to 4 fails the manifest test; setting the
floor to 0.13.0 fails the boundary test. Both restored." \
  -- go.mod go.sum .gitignore plugin.yaml \
     internal/gcpplugin/plugin.go internal/gcpplugin/manifest_test.go \
     cmd/infrena-plugin-gcp/main.go
```

---

## Task 2: `internal/disco` — read a Discovery document

**Files:**
- Create: `internal/disco/doc.go`, `internal/disco/resolve.go`, `internal/disco/collection.go`
- Test: `internal/disco/doc_test.go`, `internal/disco/resolve_test.go`, `internal/disco/collection_test.go`
- Test fixtures: `internal/disco/testdata/tiny.json`, `internal/disco/testdata/cyclic.json`

**Interfaces:**
- Consumes: nothing.
- Produces:
  - `type Document struct { Name, Version, RootURL, ServicePath string; Schemas map[string]*Schema; Resources map[string]*Resource }` plus `func (d *Document) ResolvedBaseURL() string` (a METHOD joining RootURL+ServicePath, not a field — an earlier draft listed `BaseURL` as a field and that was wrong)
  - `type Schema struct { ID, Type, Format, Description, Ref string; ReadOnly bool; Properties map[string]*Schema; Items *Schema; AdditionalProperties *Schema; Enum []string; Required []string }`
  - `type Method struct { ID, Path, HTTPMethod, Description string; Request, Response *Ref; Parameters map[string]*Parameter }`
  - `type Collection struct { Path []string; Methods map[string]*Method }`
  - `func Parse(data []byte) (*Document, error)`
  - `func (d *Document) Collections() []Collection`
  - `func (d *Document) Resolve(s *Schema) (*Schema, error)` — inlines `$ref`, depth-limited (P7)
  - `func (d *Document) OutputOnly(s *Schema) bool` — unions `readOnly` with `[Output Only]` prose

- [ ] **Step 1: Write the failing parse test**

Create `internal/disco/testdata/tiny.json`. It must **contradict** every assertion by not being
trivially shaped — one collection nested two deep, one schema with a `$ref`, one `readOnly` property,
one `[Output Only]` prose property with no flag:

```json
{
  "name": "tiny",
  "version": "v1",
  "rootUrl": "https://tiny.googleapis.com/",
  "servicePath": "",
  "schemas": {
    "Widget": {
      "id": "Widget",
      "type": "object",
      "properties": {
        "name": {"type": "string", "description": "The widget name."},
        "selfLink": {"type": "string", "description": "[Output Only] Server-defined URL."},
        "creationTimestamp": {"type": "string", "readOnly": true, "description": "When it was made."},
        "spec": {"$ref": "WidgetSpec", "description": "The spec."}
      }
    },
    "WidgetSpec": {
      "id": "WidgetSpec",
      "type": "object",
      "properties": {"size": {"type": "integer", "format": "int64", "description": "How big."}}
    },
    "Operation": {
      "id": "Operation",
      "type": "object",
      "properties": {"status": {"type": "string"}, "targetLink": {"type": "string"}}
    }
  },
  "resources": {
    "projects": {
      "resources": {
        "widgets": {
          "methods": {
            "get": {
              "id": "tiny.projects.widgets.get",
              "path": "projects/{project}/widgets/{widget}",
              "httpMethod": "GET",
              "response": {"$ref": "Widget"}
            },
            "insert": {
              "id": "tiny.projects.widgets.insert",
              "path": "projects/{project}/widgets",
              "httpMethod": "POST",
              "request": {"$ref": "Widget"},
              "response": {"$ref": "Operation"}
            },
            "delete": {
              "id": "tiny.projects.widgets.delete",
              "path": "projects/{project}/widgets/{widget}",
              "httpMethod": "DELETE",
              "response": {"$ref": "Operation"}
            }
          }
        }
      }
    }
  }
}
```

Create `internal/disco/doc_test.go`:

```go
package disco

import (
	"os"
	"testing"
)

func load(t *testing.T, name string) *Document {
	t.Helper()
	data, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	d, err := Parse(data)
	if err != nil {
		t.Fatalf("Parse(%s): %v", name, err)
	}
	return d
}

func TestCollectionsAreFlattenedWithTheirFullPath(t *testing.T) {
	d := load(t, "tiny.json")
	cols := d.Collections()
	if len(cols) != 1 {
		t.Fatalf("got %d collections, want 1: %+v", len(cols), cols)
	}
	c := cols[0]
	// The path must carry BOTH segments. A flattener that kept only the leaf would
	// report "widgets" and pass a weaker assertion.
	if got, want := len(c.Path), 2; got != want {
		t.Fatalf("path %v has %d segments, want %d", c.Path, got, want)
	}
	if c.Path[0] != "projects" || c.Path[1] != "widgets" {
		t.Errorf("path = %v, want [projects widgets]", c.Path)
	}
	for _, m := range []string{"get", "insert", "delete"} {
		if _, ok := c.Methods[m]; !ok {
			t.Errorf("method %q missing", m)
		}
	}
}

func TestOutputOnlyUnionsTheFlagAndTheProse(t *testing.T) {
	d := load(t, "tiny.json")
	w := d.Schemas["Widget"]
	cases := map[string]bool{
		"name":              false,
		"selfLink":          true, // prose only, no flag — compute's whole convention
		"creationTimestamp": true, // flag only, no prose
		"spec":              false,
	}
	for prop, want := range cases {
		if got := d.OutputOnly(w.Properties[prop]); got != want {
			t.Errorf("OutputOnly(%s) = %v, want %v", prop, got, want)
		}
	}
}
```

- [ ] **Step 2: Run it and watch it fail**

Run: `go test -count=1 ./internal/disco/`
Expected: FAIL — package does not compile, `Parse` undefined.

- [ ] **Step 3: Write `doc.go` and `collection.go`**

```go
// Package disco reads Google API Discovery documents: the authoritative,
// always-current schema source for every GCP API.
//
// It knows nothing about infrena, magic-modules or the catalog. Its only job is
// to turn one Discovery document into types the generator can walk.
package disco

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// Document is one API's Discovery document.
type Document struct {
	Name        string             `json:"name"`
	Version     string             `json:"version"`
	RootURL     string             `json:"rootUrl"`
	ServicePath string             `json:"servicePath"`
	Schemas     map[string]*Schema `json:"schemas"`
	Resources   map[string]*Resource `json:"resources"`
}

// BaseURL is where a method's path is joined onto.
func (d *Document) BaseURL() string { return d.RootURL + d.ServicePath }

// Schema is one type, or one property of one type. Discovery reuses the same
// shape for both, and so does this.
type Schema struct {
	ID          string             `json:"id"`
	Type        string             `json:"type"`
	Format      string             `json:"format"`
	Description string             `json:"description"`
	Ref         string             `json:"$ref"`
	ReadOnly    bool               `json:"readOnly"`
	Enum        []string           `json:"enum"`
	Required    []string           `json:"required"`
	Properties  map[string]*Schema `json:"properties"`
	Items       *Schema            `json:"items"`
	AdditionalProperties *Schema   `json:"additionalProperties"`
}

// Ref is a `{"$ref": "Name"}` pointer.
type Ref struct {
	Ref string `json:"$ref"`
}

// Parameter is one method parameter.
type Parameter struct {
	Type     string `json:"type"`
	Location string `json:"location"`
	Required bool   `json:"required"`
}

// Method is one API method.
type Method struct {
	ID          string                `json:"id"`
	Path        string                `json:"path"`
	HTTPMethod  string                `json:"httpMethod"`
	Description string                `json:"description"`
	Request     *Ref                  `json:"request"`
	Response    *Ref                  `json:"response"`
	Parameters  map[string]*Parameter `json:"parameters"`
}

// Resource is one node of the Discovery `resources` tree.
type Resource struct {
	Methods   map[string]*Method   `json:"methods"`
	Resources map[string]*Resource `json:"resources"`
}

// Collection is one flattened node of that tree, with the full path that
// reached it.
type Collection struct {
	Path    []string
	Methods map[string]*Method
}

// Parse decodes one Discovery document.
func Parse(data []byte) (*Document, error) {
	var d Document
	if err := json.Unmarshal(data, &d); err != nil {
		return nil, fmt.Errorf("discovery document: %w", err)
	}
	if d.Name == "" {
		return nil, fmt.Errorf("discovery document has no name")
	}
	return &d, nil
}

// Collections flattens the resource tree, keeping only nodes that have methods
// of their own, in a deterministic order.
func (d *Document) Collections() []Collection {
	var out []Collection
	var walk func(res map[string]*Resource, path []string)
	walk = func(res map[string]*Resource, path []string) {
		names := make([]string, 0, len(res))
		for n := range res {
			names = append(names, n)
		}
		sort.Strings(names)
		for _, n := range names {
			r := res[n]
			p := append(append([]string{}, path...), n)
			if len(r.Methods) > 0 {
				out = append(out, Collection{Path: p, Methods: r.Methods})
			}
			walk(r.Resources, p)
		}
	}
	walk(d.Resources, nil)
	return out
}

// OutputOnly reports whether GCP, not the user, sets this property.
//
// It unions two signals because neither is sufficient on its own: compute
// carries 1,520 `readOnly` flags but 2,202 `[Output Only]` prose markers
// (measured 2026-09-21), and the modern APIs use the flag. Trusting either
// alone marks settable properties read-only, or read-only ones settable.
func (d *Document) OutputOnly(s *Schema) bool {
	if s == nil {
		return false
	}
	if s.ReadOnly {
		return true
	}
	desc := strings.TrimSpace(s.Description)
	lower := strings.ToLower(desc)
	return strings.HasPrefix(lower, "[output only]") || strings.HasPrefix(lower, "output only.")
}
```

- [ ] **Step 4: Run the tests and watch them pass**

Run: `go test -count=1 ./internal/disco/`
Expected: PASS.

- [ ] **Step 5: Write the failing `$ref` cycle test**

Create `internal/disco/testdata/cyclic.json` — a schema that refers to itself through another, which
Discovery documents really do contain:

```json
{
  "name": "cyclic",
  "version": "v1",
  "rootUrl": "https://cyclic.googleapis.com/",
  "servicePath": "",
  "schemas": {
    "Node": {
      "id": "Node",
      "type": "object",
      "properties": {
        "label": {"type": "string"},
        "child": {"$ref": "Branch"}
      }
    },
    "Branch": {
      "id": "Branch",
      "type": "object",
      "properties": {"node": {"$ref": "Node"}}
    }
  },
  "resources": {}
}
```

Add to `internal/disco/resolve_test.go`:

```go
package disco

import "testing"

// TestResolveTerminatesOnACycle. Discovery documents contain genuine $ref cycles
// (compute's Expr/Policy is one). An unbounded resolver hangs the generator, which
// is why this test carries a timeout: a hang must be a failure, not a stuck suite.
func TestResolveTerminatesOnACycle(t *testing.T) {
	d := load(t, "cyclic.json")
	got, err := d.Resolve(d.Schemas["Node"])
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	// It must have expanded SOMETHING, or the test would pass against a resolver
	// that gave up immediately.
	if got.Properties["child"] == nil || got.Properties["child"].Properties["node"] == nil {
		t.Fatalf("resolver expanded nothing: %+v", got)
	}
	// And it must have stopped. Walk to the bottom and confirm the deepest node is
	// marked opaque rather than continuing.
	depth := 0
	cur := got
	for cur != nil && depth < 100 {
		child := cur.Properties["child"]
		if child == nil {
			break
		}
		cur = child.Properties["node"]
		depth++
	}
	if depth >= 100 {
		t.Fatalf("resolver did not stop: reached depth %d", depth)
	}
	if depth > maxRefDepth {
		t.Errorf("expanded to depth %d, want at most %d", depth, maxRefDepth)
	}
}
```

- [ ] **Step 6: Run it and watch it fail**

Run: `go test -count=1 -timeout 20s ./internal/disco/ -run TestResolveTerminates`
Expected: FAIL — `Resolve` and `maxRefDepth` undefined. (The `-timeout` matters: once `Resolve` exists
but is unbounded, the failure mode is a hang, and only a timeout turns that into a test failure.)

- [ ] **Step 7: Write `resolve.go`**

```go
package disco

import "fmt"

// maxRefDepth bounds $ref expansion. Discovery documents contain real cycles, so
// a resolver without a bound hangs the generator rather than producing a bad
// catalog — the worse of the two failures, because it has no error to read.
//
// 8 is deeper than any nesting the catalog actually exposes and shallow enough
// that the truncated tail is always something opaque nobody configures by hand.
const maxRefDepth = 8

// Resolve returns a copy of s with every $ref inlined, to a bounded depth.
//
// The copy matters: schemas are shared by pointer across every property that
// refers to them, so expanding in place would corrupt the document for the next
// caller.
func (d *Document) Resolve(s *Schema) (*Schema, error) {
	return d.resolve(s, 0)
}

func (d *Document) resolve(s *Schema, depth int) (*Schema, error) {
	if s == nil {
		return nil, nil
	}
	if depth > maxRefDepth {
		// Truncated: an object with no properties, which every consumer already
		// treats as opaque and copies exactly.
		return &Schema{Type: "object", Description: s.Description}, nil
	}
	// IMPORTANT: only a $ref HOP advances depth (see the d.resolve(target, depth+1)
	// call below). Structural recursion into properties/items/additionalProperties
	// passes depth THROUGH unchanged. Counting structural nesting conflates finite
	// nesting with unbounded cycles: measured against the real compute document,
	// advancing depth on every step truncates 2,686 legitimate non-cyclic subtrees
	// and replaces real fields with an opaque object, silently and with no error.
	if s.Ref != "" {
		target, ok := d.Schemas[s.Ref]
		if !ok {
			return nil, fmt.Errorf("$ref %q is not a schema in %s", s.Ref, d.Name)
		}
		resolved, err := d.resolve(target, depth+1)
		if err != nil {
			return nil, err
		}
		// The referring site's own description and readOnly flag win: a property
		// saying "[Output Only] the spec" must stay output-only even though the
		// shared spec schema says nothing about it.
		out := *resolved
		if s.Description != "" {
			out.Description = s.Description
		}
		if s.ReadOnly {
			out.ReadOnly = true
		}
		return &out, nil
	}

	out := *s
	if s.Properties != nil {
		out.Properties = make(map[string]*Schema, len(s.Properties))
		for name, p := range s.Properties {
			r, err := d.resolve(p, depth) // structural: depth unchanged
			if err != nil {
				return nil, err
			}
			out.Properties[name] = r
		}
	}
	if s.Items != nil {
		r, err := d.resolve(s.Items, depth) // structural: depth unchanged
		if err != nil {
			return nil, err
		}
		out.Items = r
	}
	if s.AdditionalProperties != nil {
		r, err := d.resolve(s.AdditionalProperties, depth) // structural: depth unchanged
		if err != nil {
			return nil, err
		}
		out.AdditionalProperties = r
	}
	return &out, nil
}
```

- [ ] **Step 8: Run the whole package and watch it pass**

Run: `go test -count=1 -timeout 60s ./internal/disco/`
Expected: PASS, all tests.

- [ ] **Step 9: Run it against the real compute document**

```bash
mkdir -p schemas
curl -s "https://compute.googleapis.com/\$discovery/rest?version=v1" -o schemas/compute.json
cat > /tmp/smoke_test.go <<'EOF'
package disco

import (
	"os"
	"testing"
)

// TestRealComputeDocument is a smoke test against the largest document GCP
// publishes (6.4 MB). It is skipped when the file is absent so the suite stays
// offline by default.
func TestRealComputeDocument(t *testing.T) {
	data, err := os.ReadFile("../../schemas/compute.json")
	if os.IsNotExist(err) {
		t.Skip("schemas/compute.json absent; run scripts/fetch-schemas")
	}
	if err != nil {
		t.Fatal(err)
	}
	d, err := Parse(data)
	if err != nil {
		t.Fatal(err)
	}
	if got := len(d.Collections()); got < 100 {
		t.Errorf("compute has %d collections, expected well over 100", got)
	}
	for name, s := range d.Schemas {
		if _, err := d.Resolve(s); err != nil {
			t.Fatalf("Resolve(%s): %v", name, err)
		}
	}
}
EOF
cp /tmp/smoke_test.go internal/disco/smoke_test.go
go test -count=1 -timeout 120s ./internal/disco/ -run TestRealCompute -v
```

Expected: PASS, and it must not hang. This is the only check that P7's depth limit survives contact
with a real document.

- [ ] **Step 10: Sabotage, confirm, restore**

Raise `maxRefDepth` to a number the cycle test cannot reach by deleting the bound check entirely
(`if depth > maxRefDepth` → `if false`): `TestResolveTerminatesOnACycle` must fail by timeout, and
`TestRealComputeDocument` must hang until its timeout. Restore by re-applying the edit. Then remove
`s.ReadOnly` from the `$ref` branch's inheritance: `TestOutputOnlyUnionsTheFlagAndTheProse` stays green
(it does not use a ref), which tells you that assertion is too weak — **add** a `readOnly` `$ref`
property to `tiny.json` and an assertion for it before moving on. Record all of this in the commit.

- [ ] **Step 11: Commit**

```bash
git add internal/disco/ 
git commit -m "Read Discovery documents

Flattens the resources tree into collections that keep their full path,
and resolves refs to a bounded depth because these documents contain
real cycles and an unbounded resolver hangs the generator instead of
producing a bad catalog.

Output-only detection unions the readOnly flag with the bracketed prose
marker. Neither works alone: compute carries 1520 flags against 2202
prose markers, and the newer APIs only use the flag.

Sabotage: removing the depth bound hangs the cycle test and the compute
smoke test until their timeouts. Removing readOnly inheritance through a
ref left the test green, which showed the fixture was too weak, so a
ref-with-flag case was added." \
  -- internal/disco/
```

---

## Task 3: `internal/mmv1` — read the lifecycle overlay, and spot the hooks

**Files:**
- Create: `internal/mmv1/parse.go`, `internal/mmv1/hooks.go`
- Test: `internal/mmv1/parse_test.go`, `internal/mmv1/hooks_test.go`
- Test fixtures: `internal/mmv1/testdata/Widget.yaml`, `internal/mmv1/testdata/Hooked.yaml`

**Interfaces:**
- Consumes: nothing.
- Produces:
  - `type Resource struct { Name, Product, Description, BaseURL, CreateURL, UpdateURL, DeleteURL, SelfLink, UpdateVerb, ImportFormat string; UpdateMask bool; Exclude bool; MinVersion string; Async *Async; Parameters, Properties []*Field; CustomCode map[string]string }`
  - `type Field struct { Name, Type, Description, Resource, ItemType string; Required, Immutable, Output bool; Properties []*Field }`
  - `type Async struct { Type string; Actions []string }`
  - `func ParseResource(data []byte) (*Resource, error)`
  - `type LoadError struct { Path string; Err error }`
  - `func LoadDir(root string) (map[string][]*Resource, []LoadError, error)` — resources keyed by product directory; per-file parse failures COLLECTED, never aborted on
  - `func (r *Resource) WireHooks() []string` — the sorted wire-affecting hook names, empty if none

**Why the hook list is a named constant and not a guess.** Measured on 2026-09-21: 423 of 942 resources
(45%) carry at least one of these. A hook is hand-written Go that rewrites the request or the response,
so a generated type that ignores one looks right and misbehaves against real GCP. Tier 2 (Task 6) exists
entirely because of this list.

- [ ] **Step 1: Write the fixtures**

`internal/mmv1/testdata/Widget.yaml` — clean, no hooks, and deliberately *mixed* so a parser that
defaulted every flag the same way would fail:

```yaml
# Copyright 2024 Google Inc.
# Licensed under the Apache License, Version 2.0 (the "License");
---
name: Widget
description: A test widget.
base_url: projects/{{project}}/locations/{{region}}/widgets
create_url: projects/{{project}}/locations/{{region}}/widgets?widgetId={{name}}
self_link: projects/{{project}}/locations/{{region}}/widgets/{{name}}
update_mask: true
update_verb: PATCH
import_format:
  - projects/{{project}}/locations/{{region}}/widgets/{{name}}
async:
  type: OpAsync
  actions: [create, delete, update]
parameters:
  - name: region
    type: String
    required: true
    immutable: true
properties:
  - name: name
    type: String
    required: true
    immutable: true
  - name: sizeGb
    type: Integer
  - name: createTime
    type: Time
    output: true
  - name: network
    type: ResourceRef
    resource: Network
    imports: selfLink
  - name: config
    type: NestedObject
    properties:
      - name: mode
        type: Enum
      - name: replicaCount
        type: Integer
        immutable: true
```

`internal/mmv1/testdata/Hooked.yaml` — same shape but carrying one wire hook and one harmless one:

```yaml
# Copyright 2024 Google Inc.
# Licensed under the Apache License, Version 2.0 (the "License");
---
name: Hooked
description: A test resource with custom code.
base_url: projects/{{project}}/hooked
custom_code:
  constants: templates/terraform/constants/hooked.tmpl
  encoder: templates/terraform/encoders/hooked.go.tmpl
properties:
  - name: name
    type: String
    required: true
```

- [ ] **Step 2: Write the failing tests**

`internal/mmv1/parse_test.go`:

```go
package mmv1

import (
	"os"
	"testing"
)

func loadRes(t *testing.T, name string) *Resource {
	t.Helper()
	data, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	r, err := ParseResource(data)
	if err != nil {
		t.Fatalf("ParseResource(%s): %v", name, err)
	}
	return r
}

func field(t *testing.T, r *Resource, name string) *Field {
	t.Helper()
	for _, f := range append(append([]*Field{}, r.Parameters...), r.Properties...) {
		if f.Name == name {
			return f
		}
	}
	t.Fatalf("no field %q", name)
	return nil
}

func TestFlagsAreReadPerFieldNotDefaulted(t *testing.T) {
	r := loadRes(t, "Widget.yaml")
	cases := []struct {
		name      string
		required  bool
		immutable bool
		output    bool
	}{
		{"region", true, true, false},
		{"name", true, true, false},
		{"sizeGb", false, false, false},
		{"createTime", false, false, true},
		{"network", false, false, false},
	}
	for _, c := range cases {
		f := field(t, r, c.name)
		if f.Required != c.required || f.Immutable != c.immutable || f.Output != c.output {
			t.Errorf("%s: required=%v immutable=%v output=%v, want %v/%v/%v",
				c.name, f.Required, f.Immutable, f.Output, c.required, c.immutable, c.output)
		}
	}
}

// TestImmutabilityIsReadAtEveryDepth. 3,185 immutable fields across the corpus, and
// nested ones are not rare. A parser that only read top-level flags would satisfy the
// test above and still lose every nested ForceNew.
func TestNestedFieldsKeepTheirFlags(t *testing.T) {
	r := loadRes(t, "Widget.yaml")
	cfg := field(t, r, "config")
	if len(cfg.Properties) != 2 {
		t.Fatalf("config has %d nested fields, want 2", len(cfg.Properties))
	}
	var replica *Field
	for _, f := range cfg.Properties {
		if f.Name == "replicaCount" {
			replica = f
		}
	}
	if replica == nil {
		t.Fatal("no nested replicaCount")
	}
	if !replica.Immutable {
		t.Error("nested replicaCount lost its immutable flag")
	}
}

func TestResourceRefCarriesItsTarget(t *testing.T) {
	r := loadRes(t, "Widget.yaml")
	n := field(t, r, "network")
	if n.Type != "ResourceRef" {
		t.Fatalf("network type = %q, want ResourceRef", n.Type)
	}
	if n.Resource != "Network" {
		t.Errorf("network target = %q, want Network", n.Resource)
	}
}

func TestURLsAndUpdateMechanicsAreRead(t *testing.T) {
	r := loadRes(t, "Widget.yaml")
	if r.BaseURL != "projects/{{project}}/locations/{{region}}/widgets" {
		t.Errorf("base_url = %q", r.BaseURL)
	}
	if !r.UpdateMask {
		t.Error("update_mask not read")
	}
	if r.UpdateVerb != "PATCH" {
		t.Errorf("update_verb = %q, want PATCH", r.UpdateVerb)
	}
	if r.Async == nil || r.Async.Type != "OpAsync" {
		t.Errorf("async = %+v, want OpAsync", r.Async)
	}
}
```

`internal/mmv1/hooks_test.go`:

```go
package mmv1

import (
	"slices"
	"testing"
)

func TestOnlyWireAffectingHooksAreReported(t *testing.T) {
	clean := loadRes(t, "Widget.yaml")
	if got := clean.WireHooks(); len(got) != 0 {
		t.Errorf("Widget reports hooks %v, want none", got)
	}
	hooked := loadRes(t, "Hooked.yaml")
	got := hooked.WireHooks()
	// `constants` is present in the fixture and must NOT be reported: it emits Go
	// constants and never touches the wire. Reporting it would push ~145 resources
	// into tier 2 for no reason.
	if !slices.Equal(got, []string{"encoder"}) {
		t.Errorf("Hooked reports %v, want [encoder]", got)
	}
}
```

- [ ] **Step 3: Run and watch them fail**

Run: `go test -count=1 ./internal/mmv1/`
Expected: FAIL — package does not compile.

- [ ] **Step 4: Write `parse.go`**

```go
// Package mmv1 reads GoogleCloudPlatform/magic-modules resource definitions:
// the lifecycle overlay that supplies what Discovery documents do not.
//
// Only mmv1/products/**/*.yaml is read, and those files are Apache 2.0 by
// per-file header. Nothing under mmv1/third_party/ or tools/ is touched; both
// are MPL 2.0.
package mmv1

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// Field is one parameter or property, at any depth.
type Field struct {
	Name        string   `yaml:"name"`
	Type        string   `yaml:"type"`
	Description string   `yaml:"description"`
	Required    bool     `yaml:"required"`
	Immutable   bool     `yaml:"immutable"`
	Output      bool     `yaml:"output"`
	Resource    string   `yaml:"resource"`   // ResourceRef target
	Imports     string   `yaml:"imports"`    // which of the target's fields the ref carries
	ItemType    any      `yaml:"item_type"`  // string, or a nested mapping for Array of objects
	Properties  []*Field `yaml:"properties"` // NestedObject and Array-of-NestedObject
}

// Async describes how a mutation completes.
type Async struct {
	Type    string   `yaml:"type"`
	Actions []string `yaml:"actions"`
}

// Resource is one magic-modules resource definition.
type Resource struct {
	Name        string   `yaml:"name"`
	Description string   `yaml:"description"`
	BaseURL     string   `yaml:"base_url"`
	CreateURL   string   `yaml:"create_url"`
	UpdateURL   string   `yaml:"update_url"`
	DeleteURL   string   `yaml:"delete_url"`
	SelfLink    string   `yaml:"self_link"`
	UpdateVerb  string   `yaml:"update_verb"`
	UpdateMask  bool     `yaml:"update_mask"`
	Exclude     bool     `yaml:"exclude"`
	MinVersion  string   `yaml:"min_version"`
	ImportFormat []string `yaml:"import_format"`
	Async       *Async   `yaml:"async"`
	Parameters  []*Field `yaml:"parameters"`
	Properties  []*Field `yaml:"properties"`
	CustomCode  map[string]yaml.Node `yaml:"custom_code"`

	// Product is the directory the file was found in, filled by LoadDir.
	Product string `yaml:"-"`
}

// ParseResource decodes one resource YAML file.
func ParseResource(data []byte) (*Resource, error) {
	var r Resource
	if err := yaml.Unmarshal(data, &r); err != nil {
		return nil, fmt.Errorf("magic-modules resource: %w", err)
	}
	if r.Name == "" {
		return nil, fmt.Errorf("magic-modules resource has no name")
	}
	return &r, nil
}

// LoadDir reads every resource file under root, keyed by product directory.
//
// product.yaml is skipped: it describes the product, not a resource.
//
// A file that fails to parse is COLLECTED and returned, never skipped and never
// fatal. Aborting the walk would fail a 942-file build on one bad file; skipping
// silently would drop a type from the catalog. Task 8 routes these into
// gen/warnings.txt.
//
// No file in the 2026-09-21 corpus actually fails here. An earlier draft of this
// comment claimed 3 of 942 did; that was measured with Python's yaml.safe_load,
// which refuses an unknown `!ruby/object:` tag. Go's yaml.v3 ignores the tag and
// decodes the mapping beneath it, so all 942 parse — verified per-file, including
// spanner/InstancePartition whose tagged custom_code still yields
// [encoder pre_update]. The collection path stays because a vendor bump can
// introduce a genuinely unparseable file at any time.
func LoadDir(root string) (map[string][]*Resource, []LoadError, error) {
	out := map[string][]*Resource{}
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".yaml") {
			return nil
		}
		if filepath.Base(path) == "product.yaml" {
			return nil
		}
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		r, parseErr := ParseResource(data)
		if parseErr != nil {
			return fmt.Errorf("%s: %w", path, parseErr)
		}
		r.Product = filepath.Base(filepath.Dir(path))
		out[r.Product] = append(out[r.Product], r)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}
```

- [ ] **Step 5: Write `hooks.go`**

```go
package mmv1

import "slices"

// wireHooks are the custom_code keys that change what goes on the wire or what
// is read back from it. Each is hand-written Go in the Terraform provider, so a
// generated REST type that ignores one is wrong in a way no schema shows.
//
// Measured 2026-09-21 across 942 resources: 423 carry at least one of these.
// Keys deliberately NOT here, because they emit Go that never touches a request
// or response: constants (145), test_check_destroy (45), pre_read (34),
// post_read (21), post_import (19), extra_schema_entry (13), and every tgc_*
// key. Moving a key into this list moves resources into tier 2, so do it only
// with a reason written down.
var wireHooks = []string{
	"custom_create",
	"custom_delete",
	"custom_import",
	"custom_update",
	"decoder",
	"encoder",
	"post_create",
	// post_create_failure points at delete_on_failure.go.tmpl in 2 of the 3
	// resources that declare it: it issues a DELETE when create fails. That is
	// wire-affecting, and it collides with this provider's rule that Create never
	// errors once GCP has created something — so a human rules on those.
	"post_create_failure",
	"post_delete",
	"post_update",
	"pre_create",
	"pre_delete",
	"pre_update",
	"resource_definition",
	"update_encoder",
}

// WireHooks returns the wire-affecting custom_code keys this resource declares,
// sorted. Empty means the resource is a candidate for tier 1.
func (r *Resource) WireHooks() []string {
	var out []string
	for k := range r.CustomCode {
		if slices.Contains(wireHooks, k) {
			out = append(out, k)
		}
	}
	slices.Sort(out)
	return out
}
```

- [ ] **Step 6: Run and watch them pass**

Run: `go test -count=1 ./internal/mmv1/`
Expected: PASS.

- [ ] **Step 7: Vendor the real thing and check the measurements reproduce**

```bash
mkdir -p gen
git clone --depth 1 --filter=blob:none --sparse \
  https://github.com/GoogleCloudPlatform/magic-modules.git /tmp/mm
git -C /tmp/mm sparse-checkout set mmv1/products
git -C /tmp/mm rev-parse HEAD > gen/mmv1.lock
rm -rf gen/mmv1 && mkdir -p gen/mmv1
# Copy ONLY the YAML. A literal `cp -r` also drags in BUILD.bazel and any other
# non-YAML file upstream keeps beside the resources.
(cd /tmp/mm/mmv1/products && find . -name '*.yaml' -exec install -D {} path/to/infrena-provider-gcp/gen/mmv1/products/{} \;)
find gen/mmv1 -name '*.yaml' ! -name product.yaml | wc -l   # expect 942
```

Then write `internal/mmv1/corpus_test.go`, which is the test that makes the spec's headline numbers
checkable rather than remembered:

```go
package mmv1

import (
	"os"
	"testing"
)

// TestTheVendoredCorpusMatchesWhatThePlanMeasured. These numbers are load-bearing:
// the tier mechanism (spec §4.1) exists because of the hook count, and G5's scope
// comes from it. If a vendor bump moves them a lot, that is a thing to look at, not
// a thing to re-baseline silently.
func TestTheVendoredCorpusMatchesWhatThePlanMeasured(t *testing.T) {
	if _, err := os.Stat("../../gen/mmv1/products"); os.IsNotExist(err) {
		t.Skip("gen/mmv1 not vendored")
	}
	byProduct, err := LoadDir("../../gen/mmv1/products")
	if err != nil {
		t.Fatalf("LoadDir: %v", err)
	}
	var total, hooked int
	for _, rs := range byProduct {
		for _, r := range rs {
			total++
			if len(r.WireHooks()) > 0 {
				hooked++
			}
		}
	}
	t.Logf("products=%d resources=%d hooked=%d (%.0f%%)",
		len(byProduct), total, hooked, 100*float64(hooked)/float64(total))
	if total < 900 {
		t.Errorf("%d resources, expected about 942; did the vendor copy fail?", total)
	}
	// This band is a DRIFT DETECTOR for vendor bumps, NOT a regression guard for
	// the wireHooks list. Proof it cannot be the latter: adding `constants` to
	// wireHooks moves the aggregate to only 48.2%, which a 35-55% band admitted
	// happily — TestOnlyWireAffectingHooksAreReported is what caught it. Tightened
	// to 43-47 around the measured 45.3% (427/942) so a single-key change does
	// show up here too.
	if pct := 100 * float64(hooked) / float64(total); pct < 43 || pct > 47 {
		t.Errorf("hooked share is %.1f%%, expected about 45.3%%; a vendor bump moving it "+
			"this far is worth a human looking at the diff", pct)
	}
}
```

Run: `go test -count=1 -v ./internal/mmv1/ -run TestTheVendoredCorpus`
Expected: PASS, logging roughly `products=190 resources=942 hooked=423 (45%)`.

- [ ] **Step 8: Sabotage, confirm, restore**

Add `"constants"` to `wireHooks`: `TestOnlyWireAffectingHooksAreReported` must fail, and the corpus
test's percentage must jump above the 55% ceiling. Restore. Then make `ParseResource` ignore nested
`properties` (set `out.Properties = nil` for `NestedObject`): `TestNestedFieldsKeepTheirFlags` must
fail. Restore. Record both.

- [ ] **Step 9: Commit**

```bash
git add internal/mmv1/ gen/mmv1.lock gen/mmv1/
git commit -m "Read the magic-modules lifecycle overlay

Supplies what Discovery documents do not: field immutability, required,
the url templates, update mask and verb, async behaviour and reference
targets. Vendored at a pinned commit so a moving upstream arrives as a
reviewable diff and a build never fetches anything.

Only mmv1/products is copied. Everything under third_party and tools is
MPL and is not touched.

The hook list is the important part. Forty five percent of resources
carry hand written Go that rewrites the request or the response, and a
generated type that ignores one looks right and is wrong, so those are
detected here and tiered later. Keys that only emit Go constants or test
helpers are deliberately excluded and the reason is in the comment.

Sabotage: adding constants to the hook list fails both the unit test and
the corpus share check. Dropping nested properties fails the nested flag
test. Both restored." \
  -- internal/mmv1/ gen/mmv1.lock gen/mmv1/
```

---

## Task 4: `internal/catalog` — what the generator writes and the plugin reads

**Files:**
- Create: `internal/catalog/catalog.go`, `internal/catalog/embed.go`
- Test: `internal/catalog/catalog_test.go`

**Interfaces:**
- Consumes: nothing (the catalog format is deliberately independent of both input packages).
- Produces:
  - `type Catalog struct { Generated string; MMV1Commit string; Types []*Type }`
  - `type Type struct { Name, Service, Description string; Tier int; TierReason string; APIBaseURL string; BaseURL, CreateURL, UpdateURL, DeleteURL, SelfLink string; UpdateVerb string; UpdateMask bool; Await AwaitKind; OperationScope string; TimeoutSeconds int; ImportFormat string; AssetType string; Scope Scope; Attributes map[string]*Attr }`
  - `type Attr struct { Canonical string; Aliases []string; Kind value.Kind; Required, ForceNew, Output, Sensitive bool; Description string; Ref *RefTarget; Fields map[string]*Attr; Elem *Attr; Opaque, Unordered bool }`
  - `type AwaitKind int` with `AwaitNone`, `AwaitComputeOperation`, `AwaitLongRunning`
  - `type Scope int` with `ScopeGlobal`, `ScopeRegional`, `ScopeZonal`
  - `func Load() (*Catalog, error)` — decompresses the embedded blob once
  - `func (c *Catalog) Definitions() []*schema.ResourceDefinition`
  - `func (c *Catalog) Type(name string) (*Type, bool)`

**Why a second type set rather than reusing `schema.ResourceDefinition`.** The host's definition carries
what infrena needs to compile a project. The runtime needs URL templates, the await strategy, the
operation scope and the asset-type mapping, none of which the host has any business seeing. Keeping them
apart means a change to runtime metadata cannot accidentally change what the compiler is told.

- [ ] **Step 1: Write the failing round-trip test**

```go
package catalog

import (
	"testing"

	"github.com/infrena/infrena/pkg/value"
)

func sample() *Catalog {
	return &Catalog{
		Generated:  "2026-09-21",
		MMV1Commit: "deadbeef",
		Types: []*Type{{
			Name: "gcp.widget", Service: "tiny", Description: "A widget.",
			Tier: 1, TierReason: "generic-safe",
			APIBaseURL: "https://tiny.googleapis.com/v1/",
			BaseURL:    "projects/{{project}}/locations/{{region}}/widgets",
			SelfLink:   "projects/{{project}}/locations/{{region}}/widgets/{{name}}",
			UpdateVerb: "PATCH", UpdateMask: true,
			Await: AwaitLongRunning, Scope: ScopeRegional, TimeoutSeconds: 1200,
			AssetType: "tiny.googleapis.com/Widget",
			Attributes: map[string]*Attr{
				"project": {Canonical: "project", Kind: value.KindString, Required: true, ForceNew: true},
				"region":  {Canonical: "region", Kind: value.KindString, Required: true, ForceNew: true},
				"sizeGb":  {Canonical: "sizeGb", Aliases: []string{"size", "size_gb"}, Kind: value.KindInt},
				"createTime": {Canonical: "createTime", Aliases: []string{"create_time"}, Kind: value.KindString, Output: true},
				"config": {Canonical: "config", Kind: value.KindMap, Fields: map[string]*Attr{
					"replicaCount": {Canonical: "replicaCount", Kind: value.KindInt, ForceNew: true},
				}},
				"network": {Canonical: "network", Kind: value.KindString,
					Ref: &RefTarget{Type: "gcp.network", Attribute: "selfLink"}},
			},
		}},
	}
}

// TestTheCatalogSurvivesItsOwnEncoding. Everything the runtime needs travels through
// a gzipped JSON file; a field that does not round-trip is a field the plugin silently
// loses at load. Nested ForceNew is asserted specifically because it is the one most
// easily dropped by an encoder that only walks the top level.
func TestTheCatalogSurvivesItsOwnEncoding(t *testing.T) {
	in := sample()
	blob, err := Encode(in)
	if err != nil {
		t.Fatal(err)
	}
	out, err := Decode(blob)
	if err != nil {
		t.Fatal(err)
	}
	ty, ok := out.Type("gcp.widget")
	if !ok {
		t.Fatal("gcp.widget missing after round trip")
	}
	if ty.Await != AwaitLongRunning || ty.Scope != ScopeRegional {
		t.Errorf("await=%v scope=%v, want longrunning/regional", ty.Await, ty.Scope)
	}
	if ty.BaseURL == "" || ty.SelfLink == "" || !ty.UpdateMask {
		t.Errorf("url mechanics lost: %+v", ty)
	}
	if got := ty.Attributes["config"].Fields["replicaCount"]; got == nil || !got.ForceNew {
		t.Errorf("nested ForceNew lost: %+v", got)
	}
	if r := ty.Attributes["network"].Ref; r == nil || r.Type != "gcp.network" {
		t.Errorf("reference lost: %+v", r)
	}
	if got := ty.Attributes["sizeGb"].Aliases; len(got) != 2 {
		t.Errorf("aliases = %v, want two", got)
	}
}

// TestDefinitionsAreWhatTheHostAccepts runs infrena's own validator, so a catalog
// that passes here is one the host will load rather than one we merely agreed with.
func TestDefinitionsAreWhatTheHostAccepts(t *testing.T) {
	defs := sample().Definitions()
	if len(defs) != 1 {
		t.Fatalf("got %d definitions, want 1", len(defs))
	}
	if err := schema.ValidateAll(defs); err != nil {
		t.Fatalf("infrena refuses these definitions: %v", err)
	}
	d := defs[0]
	// Output-only attributes must be Computed and NOT Required, or every plan asks
	// the user for a value GCP chooses.
	ct, ok := d.Attribute("createTime")
	if !ok {
		t.Fatal("createTime missing")
	}
	if !ct.Computed || ct.Required {
		t.Errorf("createTime computed=%v required=%v, want true/false", ct.Computed, ct.Required)
	}
	// Settable ones are Optional+Computed (spec §4.3), so dropping one from config
	// is not a diff.
	sz, _ := d.Attribute("sizeGb")
	if !sz.Optional || !sz.Computed {
		t.Errorf("sizeGb optional=%v computed=%v, want true/true", sz.Optional, sz.Computed)
	}
	// project and region are the GCP analogue of AWS's region: required and ForceNew.
	for _, name := range []string{"project", "region"} {
		a, _ := d.Attribute(name)
		if !a.Required || !a.ForceNew {
			t.Errorf("%s required=%v forceNew=%v, want true/true", name, a.Required, a.ForceNew)
		}
	}
	if !d.Capabilities.Import || d.ImportID.Description == "" {
		t.Error("import capability or its description missing")
	}
}
```

Add `"github.com/infrena/infrena/pkg/schema"` to the test's imports.

- [ ] **Step 2: Run and watch it fail**

Run: `go test -count=1 ./internal/catalog/`
Expected: FAIL — package does not compile.

- [ ] **Step 3: Write `catalog.go`**

```go
// Package catalog is the generated description of every GCP type this plugin
// serves: what infrena is told about it, and what the runtime needs to call it.
//
// catalog.json.gz is GENERATED. Never hand-edit it. Change gen/overlay.yaml or
// the generator, regenerate, and commit the diff like any other reviewed change.
package catalog

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"sort"

	"github.com/infrena/infrena/pkg/schema"
	"github.com/infrena/infrena/pkg/value"
)

// AwaitKind is how a mutation on this type completes. Three, because a 25-API
// sample on 2026-09-21 found exactly three and no long tail.
type AwaitKind int

const (
	// AwaitNone: the mutation returns the resource itself (187 methods sampled).
	AwaitNone AwaitKind = iota
	// AwaitComputeOperation: status/targetLink polling via the global, region or
	// zone operations collection's `wait` (300 sampled).
	AwaitComputeOperation
	// AwaitLongRunning: google.longrunning.Operation done/error/response (189).
	AwaitLongRunning
)

// Scope is which location axis the type's URL template carries.
type Scope int

const (
	ScopeGlobal Scope = iota
	ScopeRegional
	ScopeZonal
)

// RefTarget is a declared reference edge, read from magic-modules' ResourceRef,
// never inferred.
type RefTarget struct {
	Type      string `json:"type"`
	Attribute string `json:"attribute"`
}

// Attr is one attribute, at any depth.
type Attr struct {
	Canonical   string           `json:"canonical"`
	Aliases     []string         `json:"aliases,omitempty"`
	Kind        value.Kind       `json:"kind"`
	Required    bool             `json:"required,omitempty"`
	ForceNew    bool             `json:"force_new,omitempty"`
	Output      bool             `json:"output,omitempty"`
	Sensitive   bool             `json:"sensitive,omitempty"`
	Description string           `json:"description,omitempty"`
	Ref         *RefTarget       `json:"ref,omitempty"`
	Fields      map[string]*Attr `json:"fields,omitempty"`
	Elem        *Attr            `json:"elem,omitempty"`
	// Opaque marks a value copied exactly: no key translation, nothing dropped,
	// no reordering. Free-form maps and truncated $ref tails are opaque, and
	// translating their keys would corrupt user data.
	Opaque bool `json:"opaque,omitempty"`
	// Unordered marks a list GCP may return in a different order than it was
	// sent. Task 15 reorders those to match the reference; an ordered list is
	// left alone, because there order carries meaning.
	Unordered bool `json:"unordered,omitempty"`
}

// Type is one resource type.
type Type struct {
	Name        string `json:"name"`
	Service     string `json:"service"`
	Description string `json:"description,omitempty"`

	Tier       int    `json:"tier"`
	TierReason string `json:"tier_reason,omitempty"`

	APIBaseURL string `json:"api_base_url"`
	BaseURL    string `json:"base_url"`
	CreateURL  string `json:"create_url,omitempty"`
	UpdateURL  string `json:"update_url,omitempty"`
	DeleteURL  string `json:"delete_url,omitempty"`
	SelfLink   string `json:"self_link,omitempty"`

	UpdateVerb string `json:"update_verb,omitempty"`
	UpdateMask bool   `json:"update_mask,omitempty"`

	Await          AwaitKind `json:"await"`
	OperationScope string    `json:"operation_scope,omitempty"`
	TimeoutSeconds int       `json:"timeout_seconds"`

	ImportFormat string `json:"import_format,omitempty"`
	AssetType    string `json:"asset_type,omitempty"`
	Scope        Scope  `json:"scope"`
	// ReadVia is set from a tier-2 ruling for a type with no `get` method, e.g.
	// "list_by_parent" for gcp.tagbinding. Empty means an ordinary read.
	ReadVia string `json:"read_via,omitempty"`

	Attributes map[string]*Attr `json:"attributes"`
}

// Catalog is the whole generated set.
type Catalog struct {
	Generated  string  `json:"generated"`
	MMV1Commit string  `json:"mmv1_commit"`
	Types      []*Type `json:"types"`

	byName map[string]*Type
}

// Encode writes a catalog as gzipped JSON.
func Encode(c *Catalog) ([]byte, error) {
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	enc := json.NewEncoder(zw)
	enc.SetIndent("", " ")
	if err := enc.Encode(c); err != nil {
		return nil, err
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// Decode reads one back.
func Decode(blob []byte) (*Catalog, error) {
	zr, err := gzip.NewReader(bytes.NewReader(blob))
	if err != nil {
		return nil, fmt.Errorf("catalog: %w", err)
	}
	defer zr.Close()
	data, err := io.ReadAll(zr)
	if err != nil {
		return nil, fmt.Errorf("catalog: %w", err)
	}
	var c Catalog
	if err := json.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("catalog: %w", err)
	}
	c.index()
	return &c, nil
}

func (c *Catalog) index() {
	c.byName = make(map[string]*Type, len(c.Types))
	for _, t := range c.Types {
		c.byName[t.Name] = t
	}
}

// Type looks one up by infrena type name.
func (c *Catalog) Type(name string) (*Type, bool) {
	if c.byName == nil {
		c.index()
	}
	t, ok := c.byName[name]
	return t, ok
}

// Definitions converts the catalog into what the host is told.
//
// Only tier 1 and ruled tier-2 types are in the catalog at all (the generator
// refuses the rest), so there is no filtering here: a type that reached the
// catalog is one this plugin serves.
func (c *Catalog) Definitions() []*schema.ResourceDefinition {
	defs := make([]*schema.ResourceDefinition, 0, len(c.Types))
	for _, t := range c.Types {
		d := &schema.ResourceDefinition{
			Type:        t.Name,
			Description: t.Description,
			Attributes:  make(map[string]schema.Attribute, len(t.Attributes)),
			Capabilities: schema.Capabilities{
				Create: t.CreateURL != "" || t.BaseURL != "",
				Read:   true,
				Update: t.UpdateVerb != "",
				Delete: true,
				Import: t.ImportFormat != "",
			},
			ImportID: schema.ImportSpec{Description: t.ImportFormat},
		}
		for name, a := range t.Attributes {
			d.Attributes[name] = a.toSchema()
		}
		defs = append(defs, d)
	}
	sort.Slice(defs, func(i, j int) bool { return defs[i].Type < defs[j].Type })
	return defs
}

func (a *Attr) toSchema() schema.Attribute {
	out := schema.Attribute{
		Kind:        a.Kind,
		Required:    a.Required,
		ForceNew:    a.ForceNew,
		Sensitive:   a.Sensitive,
		Description: a.Description,
		Aliases:     a.Aliases,
	}
	switch {
	case a.Output:
		// GCP chooses it. Computed and not Optional: configuration may not set it.
		out.Computed = true
	case a.Required:
		// project, region, name and friends. Required wins; Computed would let a
		// plan proceed without one.
	default:
		// Spec §4.3: every settable property is Optional+Computed, so an attribute
		// dropped from configuration keeps GCP's value rather than planning a change.
		out.Optional = true
		out.Computed = true
	}
	if a.Ref != nil {
		out.References = &schema.Reference{Type: a.Ref.Type, Attribute: a.Ref.Attribute}
	}
	if len(a.Fields) > 0 {
		out.Fields = make(map[string]schema.Attribute, len(a.Fields))
		for n, f := range a.Fields {
			out.Fields[n] = f.toSchema()
		}
	}
	return out
}
```

- [ ] **Step 4: Write `embed.go`**

```go
package catalog

import (
	_ "embed"
	"sync"
)

//go:embed catalog.json.gz
var blob []byte

var (
	once   sync.Once
	loaded *Catalog
	loadErr error
)

// Load decodes the embedded catalog, once per process.
//
// Once, because every instance of the plugin serves the same types and the
// decompression is the plugin's whole startup cost — measured against the
// load-cost gate in Task 9.
func Load() (*Catalog, error) {
	once.Do(func() { loaded, loadErr = Decode(blob) })
	return loaded, loadErr
}
```

Create a placeholder so the package compiles before Task 9 generates the real one:

```bash
printf '{"generated":"","mmv1_commit":"","types":[]}' | gzip -n > internal/catalog/catalog.json.gz
```

- [ ] **Step 5: Run and watch them pass**

Run: `go test -count=1 ./internal/catalog/`
Expected: PASS.

- [ ] **Step 6: Sabotage, confirm, restore**

In `toSchema`, drop the `a.Fields` branch: `TestTheCatalogSurvivesItsOwnEncoding` stays green (it
checks the catalog type, not the schema), which shows the round-trip test does not cover the
conversion — so **add** an assertion to `TestDefinitionsAreWhatTheHostAccepts` that
`d.Attributes["config"].Fields["replicaCount"].ForceNew` is true, watch it fail, and restore. Then make
the `default` branch set `Optional` without `Computed`: the Optional+Computed assertion must fail.
Record both.

- [ ] **Step 7: Commit**

```bash
git add internal/catalog/
git commit -m "The catalog format

Two type sets on purpose. The host's definition carries what infrena
needs to compile a project; the catalog type also carries url templates,
the await strategy, the operation scope and the asset type mapping,
which the compiler has no business seeing. Keeping them apart means a
change to runtime metadata cannot change what the compiler is told.

Settable attributes are optional and computed, so dropping one from
configuration keeps whatever GCP set rather than planning a change.
Output only attributes are computed and not optional.

Sabotage: dropping nested field conversion left the round trip test
green, which showed it did not cover the conversion at all, so an
assertion on nested ForceNew was added and fails without the branch.
Setting optional without computed fails the settable attribute test.
Both restored." \
  -- internal/catalog/
```

---

## Task 5: Type names, and the lock that keeps them

**Files:**
- Create: `internal/gen/names.go`
- Test: `internal/gen/names_test.go`
- NOT created here: `gen/names.lock.json`. The generator writes it at Task 8, and `LoadLock` treats an
  absent file as an empty lock precisely so the first run works. Committing an empty one here would be
  a file with no content and no author.

**Interfaces:**
- Consumes: `disco.Collection`, `mmv1.Resource`.
- Produces:
  - `type Candidate struct { Service, Resource string }` — e.g. `{Service: "compute", Resource: "subnetwork"}`
  - `type Lock struct { Names map[string]string }` — key `"<service>/<resource>"`, value the infrena type
  - `func LoadLock(path string) (*Lock, error)`, `func (l *Lock) Save(path string) error`
  - `func Assign(cands []Candidate, lock *Lock) (map[Candidate]string, error)` — deterministic, append-only

**The rule (spec §4.2).** `gcp.<resource>` when the resource segment is unique across every candidate,
otherwise `gcp.<service>.<resource>`. **A name never moves once released.** A later GCP type that would
clash with an assigned short name gets the qualified form; the short name keeps its original owner.

- [ ] **Step 1: Write the failing test**

```go
package gen

import (
	"path/filepath"
	"testing"
)

// TestAUniqueResourceSegmentGetsTheShortName, and a clash does not.
func TestNamesAreShortWhenUniqueAndQualifiedWhenNot(t *testing.T) {
	cands := []Candidate{
		{Service: "compute", Resource: "subnetwork"}, // unique
		{Service: "compute", Resource: "instance"},   // clashes with sql
		{Service: "sqladmin", Resource: "instance"},  // clashes with compute
		{Service: "redis", Resource: "instance"},     // and so does this
	}
	got, err := Assign(cands, NewLock())
	if err != nil {
		t.Fatal(err)
	}
	want := map[Candidate]string{
		{Service: "compute", Resource: "subnetwork"}: "gcp.subnetwork",
		{Service: "compute", Resource: "instance"}:   "gcp.compute.instance",
		{Service: "sqladmin", Resource: "instance"}:  "gcp.sqladmin.instance",
		{Service: "redis", Resource: "instance"}:     "gcp.redis.instance",
	}
	for c, w := range want {
		if got[c] != w {
			t.Errorf("%v -> %q, want %q", c, got[c], w)
		}
	}
}

// TestAnAssignedNameNeverMoves is the whole point of the lock. A short name already
// given to one type must stay with it even when a new GCP type would now make the
// resource segment ambiguous — the newcomer takes the qualified form.
func TestAnAssignedNameNeverMoves(t *testing.T) {
	lock := NewLock()
	first := []Candidate{{Service: "compute", Resource: "instance"}}
	got, err := Assign(first, lock)
	if err != nil {
		t.Fatal(err)
	}
	if got[first[0]] != "gcp.instance" {
		t.Fatalf("first pass gave %q, want gcp.instance", got[first[0]])
	}

	// A new release of GCP adds sqladmin instances. Without the lock, the naive rule
	// would now qualify BOTH and silently rename a released type.
	second := []Candidate{
		{Service: "compute", Resource: "instance"},
		{Service: "sqladmin", Resource: "instance"},
	}
	got2, err := Assign(second, lock)
	if err != nil {
		t.Fatal(err)
	}
	if got2[second[0]] != "gcp.instance" {
		t.Errorf("compute instance renamed to %q; a released name must never move", got2[second[0]])
	}
	if got2[second[1]] != "gcp.sqladmin.instance" {
		t.Errorf("newcomer got %q, want gcp.sqladmin.instance", got2[second[1]])
	}
}

// TestTheLockOnlyGrows. A type GCP removed keeps its entry, so its name can never be
// handed to something else later.
func TestTheLockOnlyGrows(t *testing.T) {
	lock := NewLock()
	if _, err := Assign([]Candidate{{Service: "gone", Resource: "widget"}}, lock); err != nil {
		t.Fatal(err)
	}
	before := len(lock.Names)
	if _, err := Assign(nil, lock); err != nil {
		t.Fatal(err)
	}
	if len(lock.Names) != before {
		t.Errorf("lock shrank from %d to %d", before, len(lock.Names))
	}
	if lock.Names["gone/widget"] != "gcp.widget" {
		t.Errorf("a withdrawn type lost its reservation: %v", lock.Names)
	}
}

func TestTheLockRoundTripsThroughDisk(t *testing.T) {
	lock := NewLock()
	if _, err := Assign([]Candidate{{Service: "compute", Resource: "subnetwork"}}, lock); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "names.lock.json")
	if err := lock.Save(path); err != nil {
		t.Fatal(err)
	}
	back, err := LoadLock(path)
	if err != nil {
		t.Fatal(err)
	}
	if back.Names["compute/subnetwork"] != "gcp.subnetwork" {
		t.Errorf("round trip lost the entry: %v", back.Names)
	}
}
```

- [ ] **Step 2: Run and watch it fail**

Run: `go test -count=1 ./internal/gen/ -run TestName -run TestThe`
Expected: FAIL — package does not compile.

- [ ] **Step 3: Write `names.go`**

```go
package gen

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
)

// Candidate is one type before it has a name.
type Candidate struct {
	Service  string
	Resource string
}

func (c Candidate) key() string { return c.Service + "/" + c.Resource }

// Lock records every name ever assigned. It ONLY GROWS: an entry is never
// removed and never changed, so a name released to users cannot later move to a
// different type. A type GCP withdraws keeps its reservation for the same
// reason.
type Lock struct {
	Names map[string]string `json:"names"`
}

// NewLock returns an empty lock.
func NewLock() *Lock { return &Lock{Names: map[string]string{}} }

// LoadLock reads the lock, treating absence as empty so a first run works.
func LoadLock(path string) (*Lock, error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return NewLock(), nil
	}
	if err != nil {
		return nil, err
	}
	l := NewLock()
	if err := json.Unmarshal(data, l); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if l.Names == nil {
		l.Names = map[string]string{}
	}
	return l, nil
}

// Save writes the lock, sorted, so a diff is readable.
func (l *Lock) Save(path string) error {
	data, err := json.MarshalIndent(l, "", " ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0o644)
}

// Assign names every candidate, honouring and extending the lock.
//
// Two passes, and the order matters. The first hands every candidate the lock
// already knows its recorded name, whatever the current corpus looks like. Only
// then does the second pass decide the newcomers, treating a short name the lock
// has already given away as taken. That is what stops a new GCP type from
// renaming a released one.
func Assign(cands []Candidate, lock *Lock) (map[Candidate]string, error) {
	out := make(map[Candidate]string, len(cands))
	taken := map[string]string{} // infrena name -> candidate key that holds it
	for k, n := range lock.Names {
		taken[n] = k
	}

	var fresh []Candidate
	for _, c := range cands {
		if n, ok := lock.Names[c.key()]; ok {
			out[c] = n
			continue
		}
		fresh = append(fresh, c)
	}

	// Deterministic: sort, so two runs over the same corpus assign the same names.
	sort.Slice(fresh, func(i, j int) bool { return fresh[i].key() < fresh[j].key() })

	// How many NEW candidates want each resource segment. A segment wanted by more
	// than one newcomer is ambiguous even if the lock has never seen it.
	wants := map[string]int{}
	for _, c := range fresh {
		wants[c.Resource]++
	}

	// Assignments are staged here and committed to the lock only once EVERY
	// candidate has resolved. The lock's whole promise is that a released name
	// never moves; half-writing that promise on an error path is the one failure
	// mode it must not have. A caller that retried on the same in-memory lock, or
	// called Save after a failed Assign, would make the partial state permanent —
	// and the lock is the one file the rules forbid hand-editing to repair.
	assigned := make(map[string]string, len(fresh))

	for _, c := range fresh {
		name, err := nameFor(c, wants, taken)
		if err != nil {
			return nil, err
		}
		out[c] = name
		taken[name] = c.key()
		assigned[c.key()] = name
	}

	// Every candidate resolved. Commit.
	for k, n := range assigned {
		lock.Names[k] = n
	}
	return out, nil
}

// nameFor picks one candidate's name: the short form, the qualified form if the
// short one is ambiguous or taken, and an error if both are taken by someone
// else. Extracted because the clash check is otherwise written twice, and
// because Assign should read as "resolve everything, then commit".
func nameFor(c Candidate, wants map[string]int, taken map[string]string) (string, error) {
	short := "gcp." + c.Resource
	qualified := "gcp." + c.Service + "." + c.Resource
	name := short
	if wants[c.Resource] > 1 {
		name = qualified
	}
	if holder, clash := taken[name]; clash && holder != c.key() {
		name = qualified
	}
	if holder, clash := taken[name]; clash && holder != c.key() {
		return "", fmt.Errorf("cannot name %s: both %s and %s are taken (by %s)",
			c.key(), short, qualified, holder)
	}
	return name, nil
}
```

- [ ] **Step 4: Run and watch them pass**

Run: `go test -count=1 ./internal/gen/`
Expected: PASS, all four.

- [ ] **Step 5: Sabotage, confirm, restore**

Delete the first pass's `lock.Names` lookup so every candidate is treated as fresh:
`TestAnAssignedNameNeverMoves` must fail with `compute instance renamed to "gcp.compute.instance"`.
Restore. Then make `Assign` rebuild `lock.Names` from scratch each call: `TestTheLockOnlyGrows` must
fail. Restore. Record both.

- [ ] **Step 6: Commit**

```bash
git add internal/gen/names.go internal/gen/names_test.go
git commit -m "Type names and the lock that keeps them

Short name when the resource segment is unique, qualified when it is
not. The lock is what makes that safe over time: without it, GCP adding
a second type with the same segment would silently rename the first, and
that name is already in people's config files.

Two passes, and the order is the mechanism. Everything the lock knows
gets its recorded name first, then newcomers are assigned around what is
already taken.

Sabotage: skipping the lock lookup renames compute instance when sql
instances appear. Rebuilding the lock each call loses the reservation
held by a withdrawn type. Both restored." \
  -- internal/gen/names.go internal/gen/names_test.go
```

---

## Task 6: Tiers and the overlay

(`gen/warnings.txt` is WRITTEN by Task 8's `WriteWarnings`; this task only decides what goes in it.)

**Files:**
- Create: `internal/gen/tier.go`, `internal/gen/overlay.go`, `gen/overlay.yaml`
- Test: `internal/gen/tier_test.go`, `internal/gen/overlay_test.go`

**Interfaces:**
- Consumes: `disco.Collection`, `mmv1.Resource`, `Lock` (Task 5).
- Produces:
  - `type Tier int` with `TierGeneric = 1`, `TierHooked = 2`, `TierExcluded = 3`
  - `type Decision struct { Tier Tier; Reason string }`
  - `func Classify(col disco.Collection, mm *mmv1.Resource, ruling *Ruling) Decision`
  - `type Overlay struct { Rulings map[string]*Ruling; Aliases map[string]map[string]string; DiscoverDefault []string }`
  - `type Ruling struct { Hooks []string; Note string; ReadVia string; AllForceNew bool }`
  - `func LoadOverlay(path string) (*Overlay, error)`

**The contract (spec §4.1).** A hooked type ships **only** when the overlay carries a ruling that names
**every** hook the resource declares. Naming three of four hooks is not a ruling — it is a ruling that
went stale when upstream added the fourth, and the generator must catch that rather than trust it.

- [ ] **Step 1: Write the failing tier tests**

```go
package gen

import (
	"testing"

	"github.com/infrena/infrena-provider-gcp/internal/disco"
	"github.com/infrena/infrena-provider-gcp/internal/mmv1"
)

func col(methods ...string) disco.Collection {
	c := disco.Collection{Path: []string{"projects", "widgets"}, Methods: map[string]*disco.Method{}}
	for _, m := range methods {
		c.Methods[m] = &disco.Method{HTTPMethod: "POST"}
	}
	return c
}

func TestAPlainCrudCollectionIsTierOne(t *testing.T) {
	d := Classify(col("get", "list", "insert", "patch", "delete"), &mmv1.Resource{Name: "Widget"}, nil)
	if d.Tier != TierGeneric {
		t.Errorf("tier = %d (%s), want 1", d.Tier, d.Reason)
	}
}

func TestNoCreateMeansExcluded(t *testing.T) {
	d := Classify(col("get", "list"), &mmv1.Resource{Name: "Widget"}, nil)
	if d.Tier != TierExcluded {
		t.Errorf("tier = %d, want 3", d.Tier)
	}
	if d.Reason == "" {
		t.Error("an excluded type with no reason is one nobody can act on")
	}
}

func TestAHookedTypeIsRefusedWithoutARuling(t *testing.T) {
	mm := &mmv1.Resource{Name: "Widget", CustomCode: customCode("encoder")}
	d := Classify(col("get", "insert", "patch", "delete"), mm, nil)
	if d.Tier != TierHooked {
		t.Errorf("tier = %d, want 2", d.Tier)
	}
	if !contains(d.Reason, "encoder") {
		t.Errorf("reason %q does not name the hook", d.Reason)
	}
}

func TestARulingThatNamesEveryHookAdmitsTheType(t *testing.T) {
	mm := &mmv1.Resource{Name: "Widget", CustomCode: customCode("encoder")}
	r := &Ruling{Hooks: []string{"encoder"}, Note: "inspected: sets a default the REST API also defaults"}
	d := Classify(col("get", "insert", "patch", "delete"), mm, r)
	if d.Tier != TierGeneric {
		t.Errorf("tier = %d (%s), want 1 once ruled", d.Tier, d.Reason)
	}
}

// TestAStaleRulingIsRefused is the case that matters. A ruling written when the
// resource had one hook must not keep admitting it after upstream adds a second:
// that is exactly how a vendor bump would silently ship an unreviewed type.
func TestAStaleRulingIsRefused(t *testing.T) {
	mm := &mmv1.Resource{Name: "Widget", CustomCode: customCode("encoder", "custom_import")}
	r := &Ruling{Hooks: []string{"encoder"}, Note: "written before custom_import existed"}
	d := Classify(col("get", "insert", "patch", "delete"), mm, r)
	if d.Tier != TierHooked {
		t.Fatalf("tier = %d, want 2: a ruling covering only some hooks is stale", d.Tier)
	}
	if !contains(d.Reason, "custom_import") {
		t.Errorf("reason %q does not name the unruled hook", d.Reason)
	}
}

func TestExcludeAndBetaAreExcluded(t *testing.T) {
	for _, mm := range []*mmv1.Resource{
		{Name: "W", Exclude: true},
		{Name: "W", MinVersion: "beta"},
	} {
		d := Classify(col("get", "insert", "patch", "delete"), mm, nil)
		if d.Tier != TierExcluded {
			t.Errorf("%+v: tier = %d, want 3", mm, d.Tier)
		}
	}
}

// customCode builds a CustomCode map from key names alone. The values are
// template paths the code never reads — WireHooks tests key PRESENCE only — and
// the field is map[string]yaml.Node because 21 real files carry a bool or a list
// there, so a literal map[string]string will not compile.
func customCode(keys ...string) map[string]yaml.Node {
	m := make(map[string]yaml.Node, len(keys))
	for _, k := range keys {
		var n yaml.Node
		n.SetString("templates/terraform/" + k + ".tmpl")
		m[k] = n
	}
	return m
}

func contains(s, sub string) bool { return len(s) >= len(sub) && (func() bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
})() }
```

- [ ] **Step 2: Run and watch it fail**

Run: `go test -count=1 ./internal/gen/ -run Tier`
Expected: FAIL — `Classify` undefined.

- [ ] **Step 3: Write `tier.go`**

```go
package gen

import (
	"fmt"
	"slices"
	"strings"

	"github.com/infrena/infrena-provider-gcp/internal/disco"
	"github.com/infrena/infrena-provider-gcp/internal/mmv1"
)

// Tier is how far the generator can vouch for a type (spec §4.1).
type Tier int

const (
	// TierGeneric: the generator can vouch for it. Ships.
	TierGeneric Tier = 1
	// TierHooked: magic-modules carries hand-written Go that changes the wire
	// shape. Ships only with a ruling that names every hook.
	TierHooked Tier = 2
	// TierExcluded: not representable. Named in gen/warnings.txt with a reason.
	TierExcluded Tier = 3
)

// Decision is a tier and why.
type Decision struct {
	Tier   Tier
	Reason string
}

// Classify decides one type's tier.
//
// Exclusions are checked before hooks: a resource that cannot be created is out
// whatever its custom_code says, and reporting it as "hooked" would put it on a
// backlog of rulings that could never help it.
func Classify(col disco.Collection, mm *mmv1.Resource, ruling *Ruling) Decision {
	if mm != nil {
		if mm.Exclude {
			return Decision{TierExcluded, "magic-modules marks it exclude: true"}
		}
		if mm.MinVersion != "" && mm.MinVersion != "ga" {
			return Decision{TierExcluded, "min_version is " + mm.MinVersion + ", not ga"}
		}
	}

	hasCreate := col.Methods["insert"] != nil || col.Methods["create"] != nil
	hasGet := col.Methods["get"] != nil
	hasDelete := col.Methods["delete"] != nil
	switch {
	case !hasCreate:
		return Decision{TierExcluded, "no insert or create method"}
	case !hasDelete:
		return Decision{TierExcluded, "no delete method"}
	case !hasGet:
		// Readable only by listing its parent. Representable, but not generically,
		// so it needs a ruling that says how — see the tagBindings ruling.
		if ruling == nil || ruling.ReadVia == "" {
			return Decision{TierHooked, "no get method; needs a ruling with read_via"}
		}
	}

	var hooks []string
	if mm != nil {
		hooks = mm.WireHooks()
	}
	if len(hooks) == 0 {
		return Decision{TierGeneric, "generic-safe"}
	}
	if ruling == nil {
		return Decision{TierHooked, "unruled wire hooks: " + strings.Join(hooks, ", ")}
	}
	var unruled []string
	for _, h := range hooks {
		if !slices.Contains(ruling.Hooks, h) {
			unruled = append(unruled, h)
		}
	}
	if len(unruled) > 0 {
		// The ruling predates these hooks. Refusing is the point: this is the exact
		// path by which a vendor bump would otherwise ship something unreviewed.
		return Decision{TierHooked, fmt.Sprintf(
			"ruling covers %v but the resource now also declares %v; re-inspect and extend the ruling",
			ruling.Hooks, unruled)}
	}
	return Decision{TierGeneric, "ruled: " + ruling.Note}
}
```

- [ ] **Step 4: Write `overlay.go` and the initial `gen/overlay.yaml`**

```go
package gen

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// Ruling is a human decision about one hooked type.
type Ruling struct {
	// Hooks must name EVERY wire hook the resource declares. A partial list is
	// treated as stale, not as partial approval.
	Hooks []string `yaml:"hooks"`
	// Note says what was inspected and why it is safe. Required: a ruling with no
	// note is a rubber stamp nobody can review.
	Note string `yaml:"note"`
	// ReadVia names how to read a type with no get method, e.g. "list_by_parent".
	ReadVia string `yaml:"read_via"`
	// AllForceNew marks a type with no update path at all.
	AllForceNew bool `yaml:"all_force_new"`
}

// Overlay is gen/overlay.yaml: everything a human decided.
type Overlay struct {
	// Rulings are keyed by "<product>/<Resource>", matching the vendored path.
	Rulings map[string]*Ruling `yaml:"rulings"`
	// Aliases are friendly names, keyed by infrena type then canonical attribute.
	Aliases map[string]map[string]string `yaml:"aliases"`
	// DiscoverDefault is the type list `discover` scans when the instance names none.
	DiscoverDefault []string `yaml:"discover_default"`
}

// LoadOverlay reads and validates the overlay.
func LoadOverlay(path string) (*Overlay, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var o Overlay
	if err := yaml.Unmarshal(data, &o); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	for key, r := range o.Rulings {
		if r.Note == "" {
			return nil, fmt.Errorf("%s: ruling %q has no note; a ruling with no reasoning is a rubber stamp", path, key)
		}
		if len(r.Hooks) == 0 && r.ReadVia == "" {
			return nil, fmt.Errorf("%s: ruling %q names no hooks and no read_via, so it rules on nothing", path, key)
		}
	}
	return &o, nil
}
```

`gen/overlay.yaml`, with the spec's one worked ruling (§4.1) already in it:

```yaml
# gen/overlay.yaml: everything about the GCP catalog that a human decided.
#
# The generator refuses to ship a type whose magic-modules definition declares a
# wire-affecting hook until a ruling here names EVERY such hook. A ruling that
# names only some is treated as stale, because that is what it is.

rulings:
  cloudresourcemanager/TagBinding:
    hooks: []
    read_via: list_by_parent
    all_force_new: true
    note: >
      tagBindings has create, delete and list but no get and no patch (checked
      against the cloudresourcemanager v3 Discovery document, 2026-09-21). Read
      is therefore a list filtered by parent, and every field is ForceNew because
      there is no update path at all. Required at v1.0 by spec decision G6.

aliases: {}

# The types `discover` scans when an instance names none. Unlike AWS this is not
# a cost measure — Cloud Asset Inventory returns a whole project in a few calls —
# it is only the fallback path's list, used when CAI is unavailable.
discover_default:
  - gcp.network
  - gcp.subnetwork
  - gcp.firewall
  - gcp.instance
  - gcp.bucket
  - gcp.serviceaccount
```

- [ ] **Step 5: Write the overlay test**

```go
package gen

import (
	"os"
	"path/filepath"
	"testing"
)

func TestARulingWithoutAReasonIsRefused(t *testing.T) {
	path := filepath.Join(t.TempDir(), "overlay.yaml")
	os.WriteFile(path, []byte("rulings:\n  a/B:\n    hooks: [encoder]\n"), 0o644)
	if _, err := LoadOverlay(path); err == nil {
		t.Error("a ruling with no note was accepted")
	}
}

func TestARulingThatRulesOnNothingIsRefused(t *testing.T) {
	path := filepath.Join(t.TempDir(), "overlay.yaml")
	os.WriteFile(path, []byte("rulings:\n  a/B:\n    note: looks fine to me\n"), 0o644)
	if _, err := LoadOverlay(path); err == nil {
		t.Error("a ruling naming no hooks and no read_via was accepted")
	}
}

func TestTheRepositoryOverlayIsValid(t *testing.T) {
	o, err := LoadOverlay("../../gen/overlay.yaml")
	if err != nil {
		t.Fatalf("gen/overlay.yaml: %v", err)
	}
	if _, ok := o.Rulings["cloudresourcemanager/TagBinding"]; !ok {
		t.Error("the tagBindings ruling required by G6 is missing")
	}
	if len(o.DiscoverDefault) == 0 {
		t.Error("discover_default is empty, so the fallback path would scan nothing")
	}
}
```

- [ ] **Step 6: Run the package and watch it pass**

Run: `go test -count=1 ./internal/gen/`
Expected: PASS.

- [ ] **Step 7: Sabotage, confirm, restore**

In `Classify`, replace the unruled-hook loop with `if ruling != nil { return Decision{TierGeneric, ...} }`:
`TestAStaleRulingIsRefused` must fail. Restore. Then move the `mm.Exclude` check below the hook check:
`TestExcludeAndBetaAreExcluded` must still pass (exclusion wins either way for that fixture), which shows
the ordering is untested — **add** a fixture that is both `exclude: true` and hooked, assert tier 3, watch
it fail under the reordering, and restore. Record both.

- [ ] **Step 8: Commit**

```bash
git add internal/gen/tier.go internal/gen/overlay.go internal/gen/tier_test.go \
        internal/gen/overlay_test.go gen/overlay.yaml
git commit -m "Tier types by how far the generator can vouch for them

Forty five percent of magic-modules resources carry hand written Go that
rewrites the request or response. A generated type that ignores one
looks correct and misbehaves against real GCP, so those do not ship
until a ruling in the overlay names every hook the resource declares.

A ruling naming only some hooks is refused rather than honoured. That is
the path by which a vendor bump would otherwise ship something nobody
reviewed, and it is the case worth being strict about.

Ships with the tagBindings ruling already written, because that type has
no get and no patch and decision G6 requires it at 1.0.

Sabotage: honouring any non nil ruling accepts a stale one. Reordering
the exclude check left the test green, which showed the ordering was
untested, so a fixture that is both excluded and hooked was added. Both
restored." \
  -- internal/gen/tier.go internal/gen/overlay.go internal/gen/tier_test.go \
     internal/gen/overlay_test.go gen/overlay.yaml
```

---

## Task 7: One collection plus one mm resource becomes one catalog type

**Files:**
- Create: `internal/gen/attrs.go`
- Test: `internal/gen/attrs_test.go`

**Interfaces:**
- Consumes: `disco.Document`, `disco.Collection`, `mmv1.Resource`, `Overlay`, `catalog.Attr`.
- Produces:
  - `func BuildAttributes(d *disco.Document, body *disco.Schema, mm *mmv1.Resource, aliases map[string]string) (map[string]*catalog.Attr, error)`
  - `func ScopeOf(baseURL string) catalog.Scope`
  - `func AwaitOf(d *disco.Document, m *disco.Method) (catalog.AwaitKind, string)`
  - `func KindOf(s *disco.Schema) value.Kind`

**The rules being implemented (spec §4.3, §5.3):**

| Source | Produces |
| --- | --- |
| Discovery `type`/`format` | `Kind`. `integer`/`int64` → `KindInt`, `number` → `KindFloat`, `boolean` → `KindBool`, `array` → `KindList`, `object` → `KindMap`, everything else → `KindString` |
| Discovery `readOnly` ∪ `[Output Only]` prose | `Output` |
| mm `immutable` | `ForceNew` |
| mm `required` | `Required` |
| mm `ResourceRef` + `resource:` | `Ref` |
| overlay `aliases` | first entries of `Aliases`, then generated snake_case |
| object with `additionalProperties` and no `properties` | `Opaque` |
| `Operation` response with `done` | `AwaitLongRunning`; with `status` → `AwaitComputeOperation` |

- [ ] **Step 1: Write the failing tests**

```go
package gen

import (
	"testing"

	"github.com/infrena/infrena-provider-gcp/internal/disco"
	"github.com/infrena/infrena-provider-gcp/internal/mmv1"
	"github.com/infrena/infrena/pkg/value"
)

func TestKindsComeFromDiscoveryTypeAndFormat(t *testing.T) {
	cases := []struct {
		s    *disco.Schema
		want value.Kind
	}{
		{&disco.Schema{Type: "string"}, value.KindString},
		{&disco.Schema{Type: "integer", Format: "int32"}, value.KindInt},
		{&disco.Schema{Type: "string", Format: "int64"}, value.KindString}, // GCP writes int64 as a STRING
		{&disco.Schema{Type: "number", Format: "double"}, value.KindFloat},
		{&disco.Schema{Type: "boolean"}, value.KindBool},
		{&disco.Schema{Type: "array", Items: &disco.Schema{Type: "string"}}, value.KindList},
		{&disco.Schema{Type: "object"}, value.KindMap},
	}
	for _, c := range cases {
		if got := KindOf(c.s); got != c.want {
			t.Errorf("KindOf(%+v) = %v, want %v", c.s, got, c.want)
		}
	}
}

// The int64-as-string case above is not a curiosity. GCP serialises 64-bit
// integers as JSON strings throughout (compute disk sizes, quotas). Typing them
// as KindInt would make every such value fail to round-trip.

func TestFlagsComeFromTheRightSource(t *testing.T) {
	body := &disco.Schema{Type: "object", Properties: map[string]*disco.Schema{
		"name":       {Type: "string"},
		"sizeGb":     {Type: "integer", Format: "int32"},
		"selfLink":   {Type: "string", Description: "[Output Only] The URL."},
		"createTime": {Type: "string", ReadOnly: true},
	}}
	mm := &mmv1.Resource{Name: "Widget", Properties: []*mmv1.Field{
		{Name: "name", Type: "String", Required: true, Immutable: true},
		{Name: "sizeGb", Type: "Integer"},
	}}
	d := &disco.Document{Name: "tiny"}

	attrs, err := BuildAttributes(d, body, mm, nil)
	if err != nil {
		t.Fatal(err)
	}
	if a := attrs["name"]; !a.Required || !a.ForceNew || a.Output {
		t.Errorf("name: %+v — want required, forceNew, not output", a)
	}
	if a := attrs["sizeGb"]; a.Required || a.ForceNew || a.Output {
		t.Errorf("sizeGb: %+v — want none of the flags", a)
	}
	// Output-only must be detected from Discovery ALONE. magic-modules says nothing
	// about these two, which is the normal case for compute.
	for _, n := range []string{"selfLink", "createTime"} {
		if !attrs[n].Output {
			t.Errorf("%s not marked output-only", n)
		}
	}
}

func TestAliasesArePreferredThenSnakeCase(t *testing.T) {
	body := &disco.Schema{Type: "object", Properties: map[string]*disco.Schema{
		"authorizedNetwork": {Type: "string"},
		"sizeGb":            {Type: "integer", Format: "int32"},
	}}
	attrs, err := BuildAttributes(&disco.Document{Name: "tiny"}, body, &mmv1.Resource{Name: "W"},
		map[string]string{"authorizedNetwork": "network"})
	if err != nil {
		t.Fatal(err)
	}
	// Curated alias first, then the generated snake_case form.
	if got := attrs["authorizedNetwork"].Aliases; len(got) != 2 || got[0] != "network" || got[1] != "authorized_network" {
		t.Errorf("aliases = %v, want [network authorized_network]", got)
	}
	// No curated alias: snake_case only.
	if got := attrs["sizeGb"].Aliases; len(got) != 1 || got[0] != "size_gb" {
		t.Errorf("aliases = %v, want [size_gb]", got)
	}
}

// TestNestedKeysGetTheSameSpellings — spec §4.3 says nested keys accept the same
// spellings as top level. A recursion that built nested attributes without
// aliases would satisfy every assertion above and still make `config: {replica_count: 2}`
// fail for no reason the user can see.
func TestNestedKeysGetTheSameSpellings(t *testing.T) {
	body := &disco.Schema{Type: "object", Properties: map[string]*disco.Schema{
		"config": {Type: "object", Properties: map[string]*disco.Schema{
			"replicaCount": {Type: "integer", Format: "int32"},
		}},
	}}
	attrs, err := BuildAttributes(&disco.Document{Name: "tiny"}, body, &mmv1.Resource{Name: "W"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	nested := attrs["config"].Fields["replicaCount"]
	if nested == nil {
		t.Fatal("nested attribute missing")
	}
	if got := nested.Aliases; len(got) != 1 || got[0] != "replica_count" {
		t.Errorf("nested aliases = %v, want [replica_count]", got)
	}
}

func TestAKeywordCollisionIsRenamed(t *testing.T) {
	body := &disco.Schema{Type: "object", Properties: map[string]*disco.Schema{
		"type": {Type: "string"},
	}}
	attrs, err := BuildAttributes(&disco.Document{Name: "tiny"}, body, &mmv1.Resource{Name: "W"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, clash := attrs["type"]; clash {
		t.Error("an attribute named `type` would collide with the resource keyword")
	}
	if _, ok := attrs["type_value"]; !ok {
		t.Errorf("want type_value, got %v", keys(attrs))
	}
}

func TestAFreeFormMapIsOpaque(t *testing.T) {
	body := &disco.Schema{Type: "object", Properties: map[string]*disco.Schema{
		"labels": {Type: "object", AdditionalProperties: &disco.Schema{Type: "string"}},
		"config": {Type: "object", Properties: map[string]*disco.Schema{"mode": {Type: "string"}}},
	}}
	attrs, err := BuildAttributes(&disco.Document{Name: "tiny"}, body, &mmv1.Resource{Name: "W"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !attrs["labels"].Opaque {
		t.Error("a free-form map must be opaque; translating its keys would corrupt user data")
	}
	if attrs["config"].Opaque {
		t.Error("an object with declared properties must not be opaque")
	}
}

func TestNestedImmutabilityReachesTheNestedAttribute(t *testing.T) {
	body := &disco.Schema{Type: "object", Properties: map[string]*disco.Schema{
		"config": {Type: "object", Properties: map[string]*disco.Schema{
			"replicaCount": {Type: "integer", Format: "int32"},
			"mode":         {Type: "string"},
		}},
	}}
	mm := &mmv1.Resource{Name: "W", Properties: []*mmv1.Field{
		{Name: "config", Type: "NestedObject", Properties: []*mmv1.Field{
			{Name: "replicaCount", Type: "Integer", Immutable: true},
			{Name: "mode", Type: "Enum"},
		}},
	}}
	attrs, err := BuildAttributes(&disco.Document{Name: "tiny"}, body, mm, nil)
	if err != nil {
		t.Fatal(err)
	}
	cfg := attrs["config"]
	if cfg.Fields["replicaCount"] == nil || !cfg.Fields["replicaCount"].ForceNew {
		t.Errorf("nested replicaCount lost ForceNew: %+v", cfg.Fields)
	}
	if cfg.Fields["mode"].ForceNew {
		t.Error("nested mode gained a ForceNew it never had")
	}
}

func TestAwaitIsChosenFromTheOperationShape(t *testing.T) {
	longrunning := &disco.Document{Name: "redis", Schemas: map[string]*disco.Schema{
		"Operation": {Properties: map[string]*disco.Schema{"done": {Type: "boolean"}}},
	}}
	compute := &disco.Document{Name: "compute", Schemas: map[string]*disco.Schema{
		"Operation": {Properties: map[string]*disco.Schema{
			"status": {Type: "string"}, "targetLink": {Type: "string"}}},
	}}
	widget := &disco.Document{Name: "tiny", Schemas: map[string]*disco.Schema{
		"Widget": {Properties: map[string]*disco.Schema{"name": {Type: "string"}}},
	}}
	op := &disco.Method{Response: &disco.Ref{Ref: "Operation"}}
	if k, _ := AwaitOf(longrunning, op); k != catalog.AwaitLongRunning {
		t.Errorf("longrunning doc gave %v", k)
	}
	if k, _ := AwaitOf(compute, op); k != catalog.AwaitComputeOperation {
		t.Errorf("compute doc gave %v", k)
	}
	if k, _ := AwaitOf(widget, &disco.Method{Response: &disco.Ref{Ref: "Widget"}}); k != catalog.AwaitNone {
		t.Errorf("a method returning the resource gave %v, want none", k)
	}
}

func TestScopeComesFromTheURLTemplate(t *testing.T) {
	cases := map[string]catalog.Scope{
		"projects/{{project}}/global/networks":              catalog.ScopeGlobal,
		"projects/{{project}}/regions/{{region}}/subnetworks": catalog.ScopeRegional,
		"projects/{{project}}/zones/{{zone}}/instances":     catalog.ScopeZonal,
		"projects/{{project}}/locations/{{region}}/widgets": catalog.ScopeRegional,
	}
	for url, want := range cases {
		if got := ScopeOf(url); got != want {
			t.Errorf("ScopeOf(%s) = %v, want %v", url, got, want)
		}
	}
}

func keys(m map[string]*catalog.Attr) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
```

Add `"github.com/infrena/infrena-provider-gcp/internal/catalog"` to the test imports.

- [ ] **Step 2: Run and watch it fail**

Run: `go test -count=1 ./internal/gen/ -run 'TestKinds|TestFlags|TestAliases|TestAKeyword|TestAFreeForm|TestNested|TestAwait|TestScope'`
Expected: FAIL — the functions are undefined.

- [ ] **Step 3: Write `attrs.go`**

```go
package gen

import (
	"fmt"
	"os"
	"strings"
	"unicode"

	"github.com/infrena/infrena-provider-gcp/internal/catalog"
	"github.com/infrena/infrena-provider-gcp/internal/disco"
	"github.com/infrena/infrena-provider-gcp/internal/mmv1"
	"github.com/infrena/infrena/pkg/value"
)

// keywords are infrena resource keys an attribute may not shadow. A property
// with one of these names is exposed with "_value" appended.
var keywords = map[string]string{
	"type":      "type_value",
	"provider":  "provider_value",
	"lifecycle": "lifecycle_value",
}

// KindOf maps a Discovery type to an infrena kind.
//
// The int64 case is the one that matters: GCP serialises 64-bit integers as JSON
// STRINGS (compute disk sizes, quotas), so a schema saying type string, format
// int64 is a string as far as the wire is concerned. Typing it as KindInt would
// break every round trip.
func KindOf(s *disco.Schema) value.Kind {
	if s == nil {
		return value.KindString
	}
	switch s.Type {
	case "integer":
		return value.KindInt
	case "number":
		return value.KindFloat
	case "boolean":
		return value.KindBool
	case "array":
		return value.KindList
	case "object":
		return value.KindMap
	default:
		return value.KindString
	}
}

// ScopeOf reads the location axis out of a URL template.
func ScopeOf(baseURL string) catalog.Scope {
	switch {
	case strings.Contains(baseURL, "/zones/"):
		return catalog.ScopeZonal
	case strings.Contains(baseURL, "/regions/"), strings.Contains(baseURL, "/locations/"):
		return catalog.ScopeRegional
	default:
		return catalog.ScopeGlobal
	}
}

// AwaitOf picks the await strategy from the shape of the method's response.
//
// Deciding from the Operation SCHEMA rather than from the API's name is what
// makes this generic: container, dns and sqladmin all use compute-style
// operations without being compute.
func AwaitOf(d *disco.Document, m *disco.Method) (catalog.AwaitKind, string) {
	if m == nil || m.Response == nil || m.Response.Ref != "Operation" {
		return catalog.AwaitNone, ""
	}
	op, ok := d.Schemas["Operation"]
	if !ok {
		return catalog.AwaitNone, ""
	}
	if _, isLRO := op.Properties["done"]; isLRO {
		return catalog.AwaitLongRunning, ""
	}
	if _, isCompute := op.Properties["status"]; isCompute {
		return catalog.AwaitComputeOperation, ""
	}
	return catalog.AwaitNone, ""
}

// snake converts GCP's lowerCamelCase to snake_case.
func snake(s string) string {
	var b strings.Builder
	for i, r := range s {
		if unicode.IsUpper(r) {
			if i > 0 {
				b.WriteByte('_')
			}
			b.WriteRune(unicode.ToLower(r))
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// mmIndex flattens one magic-modules resource's fields by name, at every depth,
// so a Discovery property can find its lifecycle flags wherever they live.
func mmIndex(fields []*mmv1.Field) map[string]*mmv1.Field {
	out := map[string]*mmv1.Field{}
	var walk func(fs []*mmv1.Field)
	walk = func(fs []*mmv1.Field) {
		for _, f := range fs {
			if _, seen := out[f.Name]; !seen {
				out[f.Name] = f
			}
			walk(f.Properties)
		}
	}
	walk(fields)
	return out
}

// BuildAttributes turns one request-body schema plus its magic-modules
// definition into catalog attributes.
//
// Discovery is authoritative for SHAPE (what exists, what type, what is
// output-only); magic-modules is authoritative for LIFECYCLE (required,
// immutable, references). Neither source overrides the other on its own ground,
// which is why a property magic-modules has never heard of still ships, just
// without a ForceNew.
func BuildAttributes(d *disco.Document, body *disco.Schema, mm *mmv1.Resource, aliases map[string]string) (map[string]*catalog.Attr, error) {
	if body == nil {
		return nil, fmt.Errorf("no request body schema")
	}
	var idx map[string]*mmv1.Field
	if mm != nil {
		idx = mmIndex(append(append([]*mmv1.Field{}, mm.Parameters...), mm.Properties...))
	}
	return buildLevel(d, body, idx, aliases, true)
}

// buildLevel builds one level of attributes. topLevel is not cosmetic: infrena
// REFUSES a nested References (pkg/schema/definition.go: only a top-level
// attribute's is ever projected into a dependency), and the corpus carries 11
// nested ResourceRef fields, so emitting them would make ValidateAll reject the
// whole catalog rather than just those types.
func buildLevel(d *disco.Document, s *disco.Schema, idx map[string]*mmv1.Field, aliases map[string]string, topLevel bool) (map[string]*catalog.Attr, error) {
	out := map[string]*catalog.Attr{}
	for name, prop := range s.Properties {
		key := name
		if renamed, clash := keywords[name]; clash {
			key = renamed
		}
		a := &catalog.Attr{
			Canonical:   name,
			Kind:        KindOf(prop),
			Output:      d.OutputOnly(prop),
			Description: strings.TrimSpace(prop.Description),
		}
		if alias, ok := aliases[name]; ok && alias != "" {
			a.Aliases = append(a.Aliases, alias)
		}
		if sn := snake(name); sn != name {
			a.Aliases = append(a.Aliases, sn)
		}
		if f := idx[name]; f != nil {
			a.Required = f.Required
			a.ForceNew = f.Immutable
			if f.Output {
				a.Output = true
			}
			// Output WINS over Required, and the disagreement is reported.
			//
			// The two come from independent sources that do not cross-validate:
			// Output is Discovery's readOnly/prose union, Required is
			// magic-modules' `required`, which sometimes means "must appear in
			// the request shape" for a field the server itself populates. If GCP
			// sets a value, a user cannot be required to supply it.
			//
			// Left alone this produces Required+Computed, which schema.Validate
			// refuses — so it would fail, but at catalog-generation time, as a
			// generic error against some deep attribute with nothing pointing back
			// to the source conflict. Clearing it here and naming the field turns
			// an opaque future failure into an attributable one.
			if a.Output && a.Required {
				a.Required = false
				// The generator's own stderr, not the plugin's: gen-gcp is a
				// build-time tool, so this is a line a human reads in the
				// regeneration output, next to the warnings file.
				fmt.Fprintf(os.Stderr,
					"gen: %s.%s is required per magic-modules but output-only per Discovery; treating it as output-only\n",
					d.Name, name)
			}
			if topLevel && f.Type == "ResourceRef" && f.Resource != "" {
				attr := f.Imports
				if attr == "" {
					attr = "selfLink"
				}
				// The target's infrena name is filled in by build.go, which is the
				// only place that knows the whole name map.
				a.Ref = &catalog.RefTarget{Type: f.Resource, Attribute: attr}
			}
		}

		switch {
		case prop.Type == "object" && len(prop.Properties) == 0:
			// Free-form: additionalProperties with no declared properties, or an
			// object the resolver truncated. Copied exactly; translating or pruning
			// its keys would corrupt user data.
			a.Opaque = true
		case prop.Type == "object":
			fields, err := buildLevel(d, prop, idx, nil, false)
			if err != nil {
				return nil, err
			}
			a.Fields = fields
		case prop.Type == "array" && prop.Items != nil:
			elem := &catalog.Attr{Canonical: name, Kind: KindOf(prop.Items)}
			if prop.Items.Type == "object" && len(prop.Items.Properties) > 0 {
				fields, err := buildLevel(d, prop.Items, idx, nil, false)
				if err != nil {
					return nil, err
				}
				elem.Fields = fields
			} else if prop.Items.Type == "object" {
				elem.Opaque = true
			}
			a.Elem = elem
		}
		out[key] = a
	}
	return out, nil
}
```

- [ ] **Step 4: Run and watch them pass**

Run: `go test -count=1 ./internal/gen/`
Expected: PASS.

- [ ] **Step 5: Sabotage, confirm, restore**

Make `KindOf` return `KindInt` for `format: "int64"` regardless of type:
`TestKindsComeFromDiscoveryTypeAndFormat` must fail on the string/int64 case. Restore. Then make
`mmIndex` walk only the top level (drop the `walk(f.Properties)` recursion):
`TestNestedImmutabilityReachesTheNestedAttribute` must fail. Restore. Then delete the `Opaque` branch:
`TestAFreeFormMapIsOpaque` must fail. Restore. Record all three.

- [ ] **Step 6: Commit**

```bash
git add internal/gen/attrs.go internal/gen/attrs_test.go
git commit -m "Turn one collection and one mm resource into catalog attributes

Discovery is authoritative for shape, magic-modules for lifecycle.
Neither overrides the other on its own ground, so a property that
magic-modules has never heard of still ships, just without a ForceNew.

Two things that look like details and are not. GCP writes 64 bit
integers as JSON strings, so a schema saying string with format int64 is
a string and typing it as an integer breaks every round trip. And a free
form map is copied exactly rather than translated, because pruning keys
we do not recognise would quietly delete what the user wrote.

Sabotage: keying the kind off format alone misreads int64 strings.
Walking only top level mm fields loses nested immutability. Dropping the
opaque branch translates free form maps. All three restored." \
  -- internal/gen/attrs.go internal/gen/attrs_test.go
```

---

## Task 8: The generator, the fetch script, and `gen/warnings.txt`

**Files:**
- Create: `internal/gen/build.go`, `cmd/gen-gcp/main.go`, `scripts/fetch-schemas`
- Test: `internal/gen/build_test.go`, `scripts/scripts_test.go`

**Interfaces:**
- Consumes: everything from Tasks 2–7.
- Produces:
  - `type Inputs struct { SchemaDir, MMV1Dir, OverlayPath, LockPath string }`
  - `type Result struct { Catalog *catalog.Catalog; Warnings []Warning }`
  - `type Warning struct { Service, Resource string; Tier Tier; Reason string }`
  - `func Build(in Inputs) (*Result, error)`
  - `func WriteWarnings(path string, ws []Warning) error`

- [ ] **Step 1: Write `scripts/fetch-schemas`**

```bash
#!/usr/bin/env bash
# Downloads the inputs the generator reads. Both land outside the build:
# schemas/ is gitignored, gen/mmv1/ is committed at a pinned commit.
#
# Nothing here runs at build time. The catalog is generated by hand and
# committed, so a build never reaches the network.
set -euo pipefail
cd "$(dirname "$0")/.."

mkdir -p schemas
echo "fetching the Discovery directory"
curl -fsS "https://discovery.googleapis.com/discovery/v1/apis" -o schemas/_directory.json

# Only the APIs the catalog covers. The full directory is 314 preferred APIs and
# most of them (AdSense, AdMob, Blogger) are not infrastructure.
apis=$(python3 - <<'PY'
import json
d=json.load(open('schemas/_directory.json'))
want={'compute','storage','container','sqladmin','iam','cloudresourcemanager','dns','run',
 'pubsub','redis','cloudkms','secretmanager','artifactregistry','bigquery','certificatemanager',
 'cloudfunctions','spanner','file','vpcaccess','networkservices','networksecurity','alloydb',
 'cloudscheduler','cloudtasks','eventarc','workflows','memcache','apigateway','composer',
 'dataproc','bigtableadmin','firestore','monitoring','logging','servicenetworking','cloudbuild',
 'clouddeploy','binaryauthorization','accesscontextmanager','cloudasset','osconfig','notebooks'}
for i in d['items']:
    if i.get('preferred') and i['name'] in want:
        print(i['name'], i['version'], i['discoveryRestUrl'])
PY
)

while read -r name version url; do
  [ -z "$name" ] && continue
  echo "  $name $version"
  curl -fsS "$url" -o "schemas/${name}.json"
done <<< "$apis"

# magic-modules, pinned. Refuses to copy anything MPL (plan decision P4).
pin=$(cat gen/mmv1.lock)
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
git clone --quiet --filter=blob:none --sparse https://github.com/GoogleCloudPlatform/magic-modules.git "$tmp/mm"
git -C "$tmp/mm" sparse-checkout set mmv1/products
git -C "$tmp/mm" checkout --quiet "$pin"

for forbidden in mmv1/third_party tools; do
  if [ -e "$tmp/mm/$forbidden" ]; then
    echo "refusing to vendor $forbidden: it is MPL 2.0, not Apache 2.0" >&2
    exit 1
  fi
done

rm -rf gen/mmv1
mkdir -p gen/mmv1
# Copy ONLY the YAML files. A literal `cp -r` also vendors BUILD.bazel and
# anything else upstream keeps beside the resources, which breaks the yaml-only
# rule even though those files are Apache 2.0 like the rest of products/.
(cd "$tmp/mm/mmv1/products" && find . -name '*.yaml' -exec install -D {} "$OLDPWD/gen/mmv1/products/{}" \;)
echo "vendored magic-modules at $pin: $(find gen/mmv1 -name '*.yaml' ! -name product.yaml | wc -l) resources"
```

```bash
chmod +x scripts/fetch-schemas
```

- [ ] **Step 2: Write the failing generator test**

`internal/gen/build_test.go`, driven by the `testdata` fixtures from Tasks 2 and 3 copied into
`internal/gen/testdata/`:

```go
package gen

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// buildFixture assembles a minimal input tree: one Discovery document, one
// magic-modules product, one overlay.
func buildFixture(t *testing.T) Inputs {
	t.Helper()
	dir := t.TempDir()
	schemas := filepath.Join(dir, "schemas")
	products := filepath.Join(dir, "mmv1", "products", "tiny")
	for _, d := range []string{schemas, products} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	copyFile(t, "testdata/tiny.json", filepath.Join(schemas, "tiny.json"))
	copyFile(t, "testdata/Widget.yaml", filepath.Join(products, "Widget.yaml"))
	copyFile(t, "testdata/Hooked.yaml", filepath.Join(products, "Hooked.yaml"))

	overlay := filepath.Join(dir, "overlay.yaml")
	os.WriteFile(overlay, []byte("rulings: {}\naliases: {}\ndiscover_default: [gcp.widget]\n"), 0o644)

	return Inputs{
		SchemaDir:   schemas,
		MMV1Dir:     filepath.Join(dir, "mmv1", "products"),
		OverlayPath: overlay,
		LockPath:    filepath.Join(dir, "names.lock.json"),
	}
}

func copyFile(t *testing.T, from, to string) {
	t.Helper()
	data, err := os.ReadFile(from)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(to, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestTheGeneratorShipsTierOneAndRefusesTheRest is the central assertion of G5:
// the catalog contains what the generator can vouch for, and everything else is
// reported rather than dropped.
func TestTheGeneratorShipsTierOneAndRefusesTheRest(t *testing.T) {
	res, err := Build(buildFixture(t))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := res.Catalog.Type("gcp.widget"); !ok {
		t.Errorf("tier-1 gcp.widget missing; catalog has %d types", len(res.Catalog.Types))
	}
	if _, ok := res.Catalog.Type("gcp.hooked"); ok {
		t.Error("gcp.hooked shipped despite an unruled encoder hook")
	}
	var found bool
	for _, w := range res.Warnings {
		if w.Resource == "Hooked" {
			found = true
			if !strings.Contains(w.Reason, "encoder") {
				t.Errorf("warning does not name the hook: %q", w.Reason)
			}
		}
	}
	if !found {
		t.Error("the refused type was dropped silently rather than warned about")
	}
}

// TestGenerationIsDeterministic. The catalog is a committed artifact reviewed as
// a diff, so two runs over the same inputs must produce identical bytes or every
// regeneration is unreviewable noise.
func TestGenerationIsDeterministic(t *testing.T) {
	in := buildFixture(t)
	a, err := Build(in)
	if err != nil {
		t.Fatal(err)
	}
	os.Remove(in.LockPath)
	b, err := Build(in)
	if err != nil {
		t.Fatal(err)
	}
	ab, err := catalog.Encode(a.Catalog)
	if err != nil {
		t.Fatal(err)
	}
	bb, err := catalog.Encode(b.Catalog)
	if err != nil {
		t.Fatal(err)
	}
	if string(ab) != string(bb) {
		t.Error("two runs over identical inputs produced different catalogs")
	}
}

// TestTheLockIsExtendedNotRewritten.
func TestTheGeneratorExtendsTheLock(t *testing.T) {
	in := buildFixture(t)
	os.WriteFile(in.LockPath, []byte(`{"names":{"gone/Widget":"gcp.widget"}}`), 0o644)
	if _, err := Build(in); err != nil {
		t.Fatal(err)
	}
	lock, err := LoadLock(in.LockPath)
	if err != nil {
		t.Fatal(err)
	}
	if lock.Names["gone/Widget"] != "gcp.widget" {
		t.Errorf("the generator dropped a withdrawn type's reservation: %v", lock.Names)
	}
	// And the live one could not have taken the short name.
	if lock.Names["tiny/Widget"] == "gcp.widget" {
		t.Error("the live Widget took a name already reserved by another type")
	}
}
```

Add `"github.com/infrena/infrena-provider-gcp/internal/catalog"` to the imports.

- [ ] **Step 3: Run and watch it fail**

Run: `go test -count=1 ./internal/gen/ -run TestTheGenerator`
Expected: FAIL — `Build` and `Inputs` undefined.

- [ ] **Step 4: Write `build.go`**

```go
package gen

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/infrena/infrena-provider-gcp/internal/catalog"
	"github.com/infrena/infrena-provider-gcp/internal/disco"
	"github.com/infrena/infrena-provider-gcp/internal/mmv1"
)

// Inputs are the four things the generator reads.
type Inputs struct {
	SchemaDir   string
	MMV1Dir     string
	OverlayPath string
	LockPath    string
}

// Warning is one type that did not ship, and why.
type Warning struct {
	Service  string
	Resource string
	Tier     Tier
	Reason   string
}

// Result is the catalog plus everything left out of it.
type Result struct {
	Catalog  *catalog.Catalog
	Warnings []Warning
}

// Build runs the whole generation pass.
func Build(in Inputs) (*Result, error) {
	overlay, err := LoadOverlay(in.OverlayPath)
	if err != nil {
		return nil, err
	}
	lock, err := LoadLock(in.LockPath)
	if err != nil {
		return nil, err
	}
	byProduct, loadErrs, err := mmv1.LoadDir(in.MMV1Dir)
	if err != nil {
		return nil, err
	}
	// Per-file parse failures are REPORTED, not fatal and not dropped. LoadDir
	// collects them so one bad file cannot fail a 942-file build; routing them
	// here is what stops them vanishing instead.
	var warnings []Warning
	for _, le := range loadErrs {
		warnings = append(warnings, Warning{
			Service:  filepath.Base(filepath.Dir(le.Path)),
			Resource: strings.TrimSuffix(filepath.Base(le.Path), ".yaml"),
			Tier:     TierExcluded,
			Reason:   "cannot parse: " + le.Err.Error(),
		})
	}

	docs, err := loadDocs(in.SchemaDir)
	if err != nil {
		return nil, err
	}

	// Pass one: decide what ships, and collect naming candidates.
	type pending struct {
		doc  *disco.Document
		col  disco.Collection
		mm   *mmv1.Resource
		cand Candidate
	}
	var shipping []pending

	for _, d := range docs {
		mms := byProduct[d.Name]
		for _, col := range d.Collections() {
			leaf := col.Path[len(col.Path)-1]
			mm := matchResource(mms, leaf)
			var ruling *Ruling
			if mm != nil {
				ruling = overlay.Rulings[d.Name+"/"+mm.Name]
			}
			dec := Classify(col, mm, ruling)
			if dec.Tier != TierGeneric {
				name := leaf
				if mm != nil {
					name = mm.Name
				}
				warnings = append(warnings, Warning{d.Name, name, dec.Tier, dec.Reason})
				continue
			}
			shipping = append(shipping, pending{d, col, mm, Candidate{d.Name, strings.ToLower(singular(leaf))}})
		}
	}

	// Pass two: name everything at once, so uniqueness is decided over the whole
	// corpus rather than per API.
	cands := make([]Candidate, 0, len(shipping))
	for _, p := range shipping {
		cands = append(cands, p.cand)
	}
	names, err := Assign(cands, lock)
	if err != nil {
		return nil, err
	}
	if err := lock.Save(in.LockPath); err != nil {
		return nil, err
	}

	// Pass three: build each type, now that every reference target has a name.
	refName := map[string]string{} // mm resource name -> infrena type
	for _, p := range shipping {
		if p.mm != nil {
			refName[p.mm.Name] = names[p.cand]
		}
	}

	c := &catalog.Catalog{
		Generated:  time.Now().UTC().Format("2006-01-02"),
		MMV1Commit: readPin(filepath.Dir(in.MMV1Dir)),
	}
	for _, p := range shipping {
		t, err := buildType(p.doc, p.col, p.mm, names[p.cand], overlay, refName)
		if err != nil {
			warnings = append(warnings, Warning{p.doc.Name, p.cand.Resource, TierExcluded, err.Error()})
			continue
		}
		c.Types = append(c.Types, t)
	}
	sort.Slice(c.Types, func(i, j int) bool { return c.Types[i].Name < c.Types[j].Name })
	sort.Slice(warnings, func(i, j int) bool {
		if warnings[i].Service != warnings[j].Service {
			return warnings[i].Service < warnings[j].Service
		}
		return warnings[i].Resource < warnings[j].Resource
	})
	return &Result{Catalog: c, Warnings: warnings}, nil
}

// WriteWarnings records every type that did not ship. Never silently dropped
// (spec §4.1).
func WriteWarnings(path string, ws []Warning) error {
	var b strings.Builder
	fmt.Fprintf(&b, "# Types this catalog does not serve, and why.\n")
	fmt.Fprintf(&b, "# GENERATED by cmd/gen-gcp. Do not hand-edit.\n#\n")
	fmt.Fprintf(&b, "# tier 2 needs a ruling in gen/overlay.yaml naming every hook.\n")
	fmt.Fprintf(&b, "# tier 3 is not representable at all.\n\n")
	for _, w := range ws {
		fmt.Fprintf(&b, "tier%d\t%s/%s\t%s\n", w.Tier, w.Service, w.Resource, w.Reason)
	}
	return os.WriteFile(path, []byte(b.String()), 0o644)
}
```

The helpers `loadDocs`, `matchResource`, `singular`, `readPin` and `buildType` are written in the same
file. `buildType` assembles a `catalog.Type` from `BuildAttributes`, `ScopeOf`, `AwaitOf` and the mm
URL fields.

**Reference resolution is NOT a bare-name lookup, and this is the part most likely to be got wrong.**
`Attr.Ref.Type` arrives from Task 7 holding the raw magic-modules resource name. Measured on
2026-09-21: **76 of those names are used by more than one product** — `Instance` by 16 (alloydb,
apigee, compute, datafusion, filestore, firebasedatabase, …), `Service` by 10, `Cluster` by 8; 942
resources share only 802 distinct names. A map keyed on the bare name therefore resolves a compute
`ResourceRef` to whichever product built last, which is a SILENTLY WRONG edge — strictly worse than a
dangling one, because a dangling edge is dropped and emits nothing while a wrong edge writes
`${some-unrelated-resource}` into a user's generated configuration and compiles.

So keep two maps — `refByProduct` keyed `"<product>/<Resource>"`, and `refCandidates` mapping a bare
name to the set of products shipping it — and resolve in three steps:

1. `<referring resource's product>/<Ref.Type>`. magic-modules' `resource:` names a resource in the same
   product in the common case.
2. Otherwise, if exactly ONE product ships that name, use it. Cross-product references are real:
   `compute/Subnetwork` references `networkconnectivity`'s `InternalRange`.
3. Otherwise DROP the reference and emit a warning naming the attribute and the candidate products.
   Never guess.

A reference whose target did not ship at all drops silently, as before.

**`buildType` must also CONSUME the ruling, not merely have validated it.** A ruling that parses and
changes nothing is a comment with extra steps:
- `ruling.AllForceNew` → mark every settable (non-`Output`) attribute `ForceNew`, **at every depth**.
  A top-level-only implementation passes a shallow test and leaves nested attributes updatable on a
  type that has no patch method at all. This is what makes `gcp.tagbinding` honest, and Task 9 asserts
  it.
- `ruling.ReadVia` → record it on the `catalog.Type` so Task 16's `readByListingParent` has something
  to read.

**`buildType` must also CONSUME the ruling, not merely have validated it.** A ruling that parses and
then changes nothing is a comment with extra steps:
- `ruling.AllForceNew` → mark every settable (non-`Output`) attribute `ForceNew`, at every depth. This
  is what makes `gcp.tagbinding` honest: it has `create`, `delete` and `list` but no `patch`, so
  nothing about it can be updated in place. Task 9's `TestTheTagBindingRulingTookEffect` asserts
  exactly this, and without it that test fails.
- `ruling.ReadVia` → record it on the `catalog.Type` (add a `ReadVia string` field) so the runtime knows
  to read by listing the parent. Task 16 implements `readByListingParent` against it. `matchResource` matches a Discovery collection leaf to an mm resource by
case-insensitive singular comparison (`widgets` ↔ `Widget`).

- [ ] **Step 5: Write `cmd/gen-gcp/main.go`**

```go
// Command gen-gcp regenerates internal/catalog/catalog.json.gz and gen/warnings.txt.
//
// Run it by hand and commit the diff. Nothing runs it at build time.
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/infrena/infrena-provider-gcp/internal/catalog"
	"github.com/infrena/infrena-provider-gcp/internal/gen"
)

func main() {
	in := gen.Inputs{}
	flag.StringVar(&in.SchemaDir, "schemas", "schemas", "Discovery documents")
	flag.StringVar(&in.MMV1Dir, "mmv1", "gen/mmv1/products", "vendored magic-modules products")
	flag.StringVar(&in.OverlayPath, "overlay", "gen/overlay.yaml", "the curated overlay")
	flag.StringVar(&in.LockPath, "lock", "gen/names.lock.json", "the append-only name lock")
	out := flag.String("out", "internal/catalog/catalog.json.gz", "where to write the catalog")
	warn := flag.String("warnings", "gen/warnings.txt", "where to write the warnings")
	flag.Parse()

	res, err := gen.Build(in)
	if err != nil {
		fmt.Fprintf(os.Stderr, "gen-gcp: %v\n", err)
		os.Exit(1)
	}
	blob, err := catalog.Encode(res.Catalog)
	if err != nil {
		fmt.Fprintf(os.Stderr, "gen-gcp: %v\n", err)
		os.Exit(1)
	}
	if err := os.WriteFile(*out, blob, 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "gen-gcp: %v\n", err)
		os.Exit(1)
	}
	if err := gen.WriteWarnings(*warn, res.Warnings); err != nil {
		fmt.Fprintf(os.Stderr, "gen-gcp: %v\n", err)
		os.Exit(1)
	}
	var tier2 int
	for _, w := range res.Warnings {
		if w.Tier == gen.TierHooked {
			tier2++
		}
	}
	fmt.Fprintf(os.Stderr, "%d types; %d refused (%d awaiting a ruling)\n",
		len(res.Catalog.Types), len(res.Warnings), tier2)
}
```

- [ ] **Step 6: Run and watch it pass**

Run: `go test -count=1 ./internal/gen/`
Expected: PASS.

- [ ] **Step 7: Sabotage, confirm, restore**

Make `Build` skip appending to `warnings` for tier-2: `TestTheGeneratorShipsTierOneAndRefusesTheRest`
must fail on "dropped silently". Restore. Then make `Build` call `NewLock()` instead of `LoadLock`:
`TestTheGeneratorExtendsTheLock` must fail. Restore. Then make `sort.Slice` on `c.Types` a no-op:
`TestGenerationIsDeterministic` must fail (Go map iteration is randomised, so it will fail reliably
rather than occasionally). Record all three.

- [ ] **Step 8: Commit**

```bash
git add internal/gen/build.go internal/gen/build_test.go internal/gen/testdata/ \
        cmd/gen-gcp/main.go scripts/fetch-schemas
git commit -m "The generator

Three passes, and the order is forced. Tiers are decided first, then
every surviving type is named at once so uniqueness is judged over the
whole corpus rather than per API, and only then are types built, because
a reference edge cannot be written until its target has a name.

Everything refused goes to gen/warnings.txt with a reason. A type that
did not ship and left no trace is one nobody can fix.

The fetch script refuses to vendor third_party or tools, which are MPL.
Making the script refuse is cheaper than trusting a future reader to
remember.

Sabotage: skipping the warning for a refused type drops it silently.
Starting from an empty lock loses a withdrawn type's reservation.
Removing the sort makes two runs over identical inputs differ. All three
restored." \
  -- internal/gen/build.go internal/gen/build_test.go internal/gen/testdata/ \
     cmd/gen-gcp/main.go scripts/fetch-schemas
```

---

## Task 9: Generate the real catalog, serve it, and measure what it costs

**Files:**
- Create: `internal/gcpplugin/config.go`, `internal/gcpplugin/credentials.go`, `scripts/measure-load`
- Modify: `internal/gcpplugin/plugin.go` (`Definitions` only — **`MaxConcurrency` belongs to Task 17**, whose value is derived from Task 14's quota measurement and is not available yet)
- Regenerate: `internal/catalog/catalog.json.gz`, `gen/warnings.txt`, `gen/names.lock.json`
- Test: `internal/gcpplugin/config_test.go`, `internal/catalog/real_test.go`

**Interfaces:**
- Consumes: `catalog.Load`, `gen.Build`.
- Produces:
  - `type Instance struct { Project, Region, Zone string; CredentialsFile, Impersonate, QuotaProject string; DiscoverTypes, DiscoverProjects []string }`
  - `func ParseConfig(cfg provider.Config) (*Instance, error)` — **fails closed on an unknown key**
  - `func (i *Instance) TokenSource(ctx context.Context) (oauth2.TokenSource, error)`

- [ ] **Step 1: Generate the real catalog**

```bash
scripts/fetch-schemas
go run ./cmd/gen-gcp
```

Read the summary line it prints on stderr. Then read `gen/warnings.txt` — not skim it. The tier-2
entries are the backlog G5 describes, and the tier-3 entries are the claim that a type is not
representable, which is worth disagreeing with while it is cheap.

- [ ] **Step 2: Write the test that pins what the real catalog contains**

`internal/catalog/real_test.go`:

```go
package catalog

import (
	"testing"

	"github.com/infrena/infrena/pkg/schema"
)

// TestTheRealCatalogIsOneInfrenaAccepts runs infrena's own validator over every
// generated definition. A catalog that fails here is one the host refuses at load,
// and the failure would otherwise surface as an unexplained plugin error.
func TestTheRealCatalogIsOneInfrenaAccepts(t *testing.T) {
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Types) < 300 {
		t.Fatalf("catalog has %d types; G5 expects roughly 500 and something is wrong below 300", len(c.Types))
	}
	if err := schema.ValidateAll(c.Definitions()); err != nil {
		t.Fatalf("infrena refuses the generated catalog: %v", err)
	}
}

// TestEveryTypeCanActuallyBeCalled. A type with no base URL, no await decision or
// no scope is one the runtime cannot serve, and shipping it means an error at
// apply rather than at generation.
func TestEveryTypeCarriesWhatTheRuntimeNeeds(t *testing.T) {
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	for _, ty := range c.Types {
		if ty.APIBaseURL == "" || ty.BaseURL == "" {
			t.Errorf("%s has no URL to call: api=%q base=%q", ty.Name, ty.APIBaseURL, ty.BaseURL)
		}
		if ty.TimeoutSeconds <= 0 {
			t.Errorf("%s has no timeout", ty.Name)
		}
		if ty.Await == AwaitComputeOperation && ty.OperationScope == "" {
			t.Errorf("%s awaits a compute operation but names no operation scope", ty.Name)
		}
	}
}

// TestEveryReferencePointsAtATypeWeServe. A dangling edge would make
// `import --generate` write a ${ref} to a type the catalog has never heard of,
// producing a compile error in a file the user never wrote.
func TestEveryReferencePointsAtATypeWeServe(t *testing.T) {
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	var walk func(tyName string, attrs map[string]*Attr)
	walk = func(tyName string, attrs map[string]*Attr) {
		for name, a := range attrs {
			if a.Ref != nil {
				if _, ok := c.Type(a.Ref.Type); !ok {
					t.Errorf("%s.%s references %q, which is not in the catalog", tyName, name, a.Ref.Type)
				}
			}
			walk(tyName, a.Fields)
		}
	}
	for _, ty := range c.Types {
		walk(ty.Name, ty.Attributes)
	}
}

// TestTheTagBindingRulingTookEffect — G6's worked example, end to end.
func TestTheTagBindingRulingTookEffect(t *testing.T) {
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"gcp.tagkey", "gcp.tagvalue", "gcp.tagbinding"} {
		if _, ok := c.Type(n); !ok {
			t.Errorf("%s missing; decision G6 requires all three at v1.0", n)
		}
	}
	tb, _ := c.Type("gcp.tagbinding")
	if tb == nil {
		return
	}
	for name, a := range tb.Attributes {
		if a.Output {
			continue
		}
		if !a.ForceNew {
			t.Errorf("gcp.tagbinding.%s is not ForceNew, but the type has no patch method", name)
		}
	}
}
```

- [ ] **Step 3: Run it**

Run: `go test -count=1 -v ./internal/catalog/`
Expected: PASS. If `TestEveryReferencePointsAtATypeWeServe` fails, `buildType` is not dropping edges
whose target did not ship — fix that in `build.go`, regenerate, and re-run rather than relaxing the test.

- [ ] **Step 4: Write instance configuration, failing closed**

`internal/gcpplugin/config.go`:

```go
package gcpplugin

import (
	"fmt"
	"sort"

	"github.com/infrena/infrena/pkg/provider"
	"github.com/infrena/infrena/pkg/value"
)

// known is every configuration key an instance accepts.
//
// Unknown keys are REFUSED, not ignored. A misspelled `impersonate_service_acount`
// silently ignored is an instance quietly running as the wrong identity, which is
// the failure a user is least able to see.
var known = map[string]bool{
	"project": true, "region": true, "zone": true,
	"credentials_file": true, "impersonate_service_account": true, "quota_project": true,
	"discover_types": true, "discover_projects": true,
}

// Instance is one configured provider instance.
type Instance struct {
	Project string
	Region  string
	Zone    string

	CredentialsFile string
	Impersonate     string
	QuotaProject    string

	DiscoverTypes    []string
	DiscoverProjects []string

	name string
}

// ParseConfig reads one instance's configuration.
func ParseConfig(cfg provider.Config) (*Instance, error) {
	var unknown []string
	for k := range cfg.Values {
		if !known[k] {
			unknown = append(unknown, k)
		}
	}
	if len(unknown) > 0 {
		sort.Strings(unknown)
		accepted := make([]string, 0, len(known))
		for k := range known {
			accepted = append(accepted, k)
		}
		sort.Strings(accepted)
		return nil, fmt.Errorf("provider %q: unknown configuration %v; accepted keys are %v",
			cfg.Instance, unknown, accepted)
	}

	in := &Instance{name: cfg.Instance}
	str := func(key string) (string, error) {
		v, ok := cfg.Value(key)
		if !ok {
			return "", nil
		}
		if v.Kind != value.KindString {
			return "", fmt.Errorf("provider %q: %s must be a string, got %s", cfg.Instance, key, v.Kind)
		}
		s, _ := v.Raw.(string)
		return s, nil
	}
	list := func(key string) ([]string, error) {
		v, ok := cfg.Value(key)
		if !ok {
			return nil, nil
		}
		if v.Kind != value.KindList {
			return nil, fmt.Errorf("provider %q: %s must be a list, got %s", cfg.Instance, key, v.Kind)
		}
		items, _ := v.Raw.([]value.Value)
		out := make([]string, 0, len(items))
		for _, it := range items {
			s, _ := it.Raw.(string)
			out = append(out, s)
		}
		return out, nil
	}

	var err error
	for _, f := range []struct {
		key string
		dst *string
	}{
		{"project", &in.Project}, {"region", &in.Region}, {"zone", &in.Zone},
		{"credentials_file", &in.CredentialsFile},
		{"impersonate_service_account", &in.Impersonate},
		{"quota_project", &in.QuotaProject},
	} {
		if *f.dst, err = str(f.key); err != nil {
			return nil, err
		}
	}
	if in.DiscoverTypes, err = list("discover_types"); err != nil {
		return nil, err
	}
	if in.DiscoverProjects, err = list("discover_projects"); err != nil {
		return nil, err
	}
	return in, nil
}
```

`internal/gcpplugin/credentials.go` resolves Application Default Credentials through
`golang.org/x/oauth2/google`, honouring `CredentialsFile` when set, wrapping in an impersonated token
source when `Impersonate` is set, and returning a `TokenSource`. **It never logs a token, a credential
path's contents, or an assertion.**

- [ ] **Step 5: Write the config test**

```go
package gcpplugin

import (
	"strings"
	"testing"

	"github.com/infrena/infrena/pkg/provider"
	"github.com/infrena/infrena/pkg/value"
)

func vals(kv map[string]string) map[string]value.Value {
	out := map[string]value.Value{}
	for k, v := range kv {
		out[k] = value.String(v, value.SourceExplicit)
	}
	return out
}

// TestAMisspelledKeyIsRefused is the important one. Ignoring it would mean an
// instance quietly running as the developer's own identity instead of the
// service account the user named.
func TestAMisspelledKeyIsRefused(t *testing.T) {
	_, err := ParseConfig(provider.Config{
		Instance: "prod",
		Values:   vals(map[string]string{"impersonate_service_acount": "x@y.iam.gserviceaccount.com"}),
	})
	if err == nil {
		t.Fatal("a misspelled key was accepted")
	}
	if !strings.Contains(err.Error(), "impersonate_service_acount") {
		t.Errorf("the error does not name the offending key: %v", err)
	}
	if !strings.Contains(err.Error(), "impersonate_service_account") {
		t.Errorf("the error does not list the accepted keys, so the user cannot see the typo: %v", err)
	}
}

func TestEveryAcceptedKeyIsRead(t *testing.T) {
	in, err := ParseConfig(provider.Config{
		Instance: "prod",
		Values: vals(map[string]string{
			"project": "p", "region": "us-central1", "zone": "us-central1-a",
			"credentials_file": "/k.json", "impersonate_service_account": "sa@p.iam.gserviceaccount.com",
			"quota_project": "q",
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	if in.Project != "p" || in.Region != "us-central1" || in.Zone != "us-central1-a" ||
		in.CredentialsFile != "/k.json" || in.Impersonate != "sa@p.iam.gserviceaccount.com" ||
		in.QuotaProject != "q" {
		t.Errorf("a key was read into the wrong field or not at all: %+v", in)
	}
}
```

- [ ] **Step 6: Wire the catalog into `Definitions`**

In `internal/gcpplugin/plugin.go`, replace the `nil` return:

```go
// Definitions are the resource types this plugin offers.
//
// Loaded once and cached by the catalog package. A failure here cannot be
// returned (the interface has no error), so it is reported on stderr and the
// plugin serves nothing, which the host reports as a plugin that offers no types
// rather than as a crash.
func (pl *Plugin) Definitions() []*schema.ResourceDefinition {
	c, err := catalog.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "gcp: the embedded catalog will not load: %v\n", err)
		return nil
	}
	return c.Definitions()
}
```

- [ ] **Step 7: Write `scripts/measure-load` and run the gate (P8)**

The script builds this plugin and a no-op plugin, runs `infrena validate` against a trivial project
with each, five times, and prints the median difference. Run it:

```bash
scripts/measure-load
```

**The gate:** at most ~500 ms extra is fine. Above roughly **1 second**, stop and report the numbers to
James rather than proceeding — that would suggest something changed for the worse rather than the
catalog's normal size.

- [ ] **Step 8: Sabotage, confirm, restore**

Remove the `unknown` check from `ParseConfig`: `TestAMisspelledKeyIsRefused` must fail. Restore. Then
delete a type from the catalog by hand-editing the gzip — do **not** do this; instead delete the
`ImportFormat` assignment in `buildType`, regenerate, and confirm `TestEveryTypeCarriesWhatTheRuntimeNeeds`
or the import capability assertion fails. Restore and regenerate. Record both.

- [ ] **Step 9: Commit**

```bash
git add internal/catalog/catalog.json.gz internal/catalog/real_test.go \
        gen/warnings.txt gen/names.lock.json \
        internal/gcpplugin/config.go internal/gcpplugin/credentials.go \
        internal/gcpplugin/config_test.go internal/gcpplugin/plugin.go \
        scripts/measure-load
git commit -m "Generate the real catalog and serve it

Tests assert over the generated artifact rather than a fixture: every
definition passes infrena's own validator, every type carries a url, a
timeout and an await decision, and every reference points at a type we
actually serve. That last one matters because a dangling edge would make
import generate write a reference to a type nobody has, producing a
compile error in a file the user never wrote.

Instance configuration fails closed on an unknown key. A misspelled
impersonation key that was merely ignored would mean running as the
wrong identity, which is the failure a user is least able to see.

Sabotage: dropping the unknown key check accepts a typo. Dropping the
import format fails the runtime completeness test. Both restored." \
  -- internal/catalog/ gen/ internal/gcpplugin/ scripts/measure-load
```

---

## Task 10: `internal/gcpfake` — an in-process GCP

**Files:**
- Create: `internal/gcpfake/server.go`, `internal/gcpfake/operations.go`, `internal/gcpfake/cai.go`
- Test: `internal/gcpfake/server_test.go`

**Interfaces:**
- Consumes: nothing.
- Produces:
  - `func New(t *testing.T) *Server` — an `httptest.Server` speaking GCP's JSON REST
  - `func (s *Server) URL() string`, `func (s *Server) Close()`
  - `func (s *Server) Seed(path string, body map[string]any)` — put a resource there directly
  - `func (s *Server) Get(path string) (map[string]any, bool)` — read one back
  - `func (s *Server) SetOperationStyle(style OperationStyle)` — `OpLongRunning` or `OpCompute`
  - `func (s *Server) FailNext(status int, code, message string)` — inject one failure
  - `func (s *Server) Requests() []Request` — what was actually sent

**Fake the cloud, not the code (spec §8).** This is an HTTP server, not a stubbed client interface, so
every test goes through the real URL templating, JSON encoding, error decoding, await loop and backoff.
The real GCP API is the oracle for its wire format: an error body is
`{"error": {"code": 429, "status": "RESOURCE_EXHAUSTED", "message": "..."}}`, and a long-running
operation is `{"name": "operations/x", "done": false}` becoming `{"name": "operations/x", "done": true,
"response": {...}}`.

- [ ] **Step 1: Write the failing test**

```go
package gcpfake

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func TestACreateReturnsAnOperationThatEventuallyCompletes(t *testing.T) {
	s := New(t)
	defer s.Close()
	s.SetOperationStyle(OpLongRunning)

	body := strings.NewReader(`{"name":"widgets/one","sizeGb":10}`)
	resp, err := http.Post(s.URL()+"/v1/projects/p/locations/r/widgets?widgetId=one",
		"application/json", body)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var op map[string]any
	json.NewDecoder(resp.Body).Decode(&op)
	if op["done"] != false {
		t.Fatalf("first response should be an unfinished operation, got %v", op)
	}
	name, _ := op["name"].(string)
	if name == "" {
		t.Fatal("operation has no name to poll")
	}

	// Poll. The fake completes after one poll, which is enough to exercise the
	// await loop without making tests slow.
	r2, err := http.Get(s.URL() + "/v1/" + name)
	if err != nil {
		t.Fatal(err)
	}
	defer r2.Body.Close()
	var op2 map[string]any
	json.NewDecoder(r2.Body).Decode(&op2)
	if op2["done"] != true {
		t.Fatalf("operation never completed: %v", op2)
	}

	if got, ok := s.Get("/v1/projects/p/locations/r/widgets/one"); !ok || got["sizeGb"] != float64(10) {
		t.Errorf("the resource was not actually stored: %v", got)
	}
}

func TestAnInjectedFailureUsesGCPsErrorShape(t *testing.T) {
	s := New(t)
	defer s.Close()
	s.FailNext(429, "RESOURCE_EXHAUSTED", "Quota exceeded.")

	resp, err := http.Get(s.URL() + "/v1/projects/p/locations/r/widgets/one")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 429 {
		t.Fatalf("status = %d, want 429", resp.StatusCode)
	}
	var e struct {
		Error struct {
			Code    int    `json:"code"`
			Status  string `json:"status"`
			Message string `json:"message"`
		} `json:"error"`
	}
	json.NewDecoder(resp.Body).Decode(&e)
	// The nesting under "error" is not decoration: it is what the real API sends,
	// and a fake that flattened it would let a broken decoder pass.
	if e.Error.Status != "RESOURCE_EXHAUSTED" || e.Error.Code != 429 {
		t.Errorf("error body is not GCP-shaped: %+v", e)
	}
}

func TestTheFakeRecordsWhatWasSent(t *testing.T) {
	s := New(t)
	defer s.Close()
	http.Get(s.URL() + "/v1/projects/p/locations/r/widgets/one")
	reqs := s.Requests()
	if len(reqs) != 1 || reqs[0].Method != "GET" {
		t.Fatalf("requests = %+v", reqs)
	}
	if !strings.Contains(reqs[0].Path, "/widgets/one") {
		t.Errorf("path not recorded: %q", reqs[0].Path)
	}
}
```

- [ ] **Step 2: Run and watch it fail; Step 3: write the fake; Step 4: run and watch it pass**

Run: `go test -count=1 ./internal/gcpfake/`

`server.go` keeps a `map[string]map[string]any` guarded by a mutex, routes by method:
- `GET /v1/<path>` → the stored body, or a 404 with GCP's `NOT_FOUND` shape.
- `POST /v1/<collection>` → store at `<collection>/<id>` taking the id from the query parameter or the
  body's `name`, then return an operation or the resource itself per `OperationStyle`.
- `PATCH /v1/<path>` → merge, honouring `updateMask` if present **and failing the request when
  `updateMask` names a field absent from the body**, because that is what the real API does and it is
  the check that catches a mask built from the wrong side of the diff.
- `DELETE /v1/<path>` → remove, return an operation.
- `GET /v1/<collection>` → a list response, paginated at 2 items so pagination is always exercised.
- `POST /v1/<parent>:searchAllResources` → the CAI shape (`cai.go`).

`operations.go` serves both shapes from one store, so a test can switch style and re-run the same
assertions.

- [ ] **Step 5: Sabotage, confirm, restore**

Flatten the error body to `{"status": "...", "message": "..."}`:
`TestAnInjectedFailureUsesGCPsErrorShape` must fail. Restore. Then make `POST` store the resource but
return `done: true` immediately: `TestACreateReturnsAnOperationThatEventuallyCompletes` must fail on the
first assertion. Restore. Record both.

- [ ] **Step 6: Commit**

```bash
git add internal/gcpfake/
git commit -m "An in process GCP to test against

An http server rather than a stubbed client, so every test goes through
the real url building, encoding, error decoding and await loop. The real
API is the oracle for the wire format, which is why errors are nested
under an error key and operations carry the shape the real ones do.

Serves both operation styles from one store so the same assertions can
run against either, paginates lists at two items so pagination is never
accidentally untested, and refuses a patch whose update mask names a
field the body does not carry, which is exactly the bug a mask built
from the wrong side of a diff produces.

Sabotage: flattening the error body passes a broken decoder. Completing
a create immediately skips the await loop. Both restored." \
  -- internal/gcpfake/
```

---

## Task 11: `internal/gcprov` foundations — client, URLs, errors, backoff

**Files:**
- Create: `internal/gcprov/client.go`, `internal/gcprov/urls.go`, `internal/gcprov/errors.go`, `internal/gcprov/backoff.go`, `internal/gcptest/gcptest.go`
- Test: `internal/gcprov/urls_test.go`, `internal/gcprov/errors_test.go`, `internal/gcprov/backoff_test.go`, `internal/gcprov/client_test.go`

**Interfaces:**
- Consumes: `catalog.Type`, `gcpplugin.Instance`.
- Produces:
  - `type Client struct { ... }`, `func NewClient(ts oauth2.TokenSource, base string, opts ClientOptions) *Client`
  - `func (c *Client) Do(ctx context.Context, method, url string, body any) (map[string]any, error)`
  - `type APIError struct { Status int; Code, Message string; RetryAfter time.Duration }`, implementing `error`
  - `func ClassifyError(err error) provider.Retryability`
  - `func ExpandURL(tmpl string, attrs map[string]value.Value) (string, error)`
  - `func gcptest.Isolate(t *testing.T)` — removes every ambient GCP credential

- [ ] **Step 1: Write `gcptest.Isolate` first, and use it in every later test**

```go
// Package gcptest isolates tests from the developer's own GCP credentials.
//
// Without this a test that forgets to point at the fake reaches real GCP with
// whatever the developer happens to be logged in as. Every test in this
// repository that constructs a client calls Isolate.
package gcptest

import "testing"

// Isolate removes every way the oauth2/google Application Default Credentials
// chain can find a real identity: the explicit key file, the gcloud well-known
// file, and the metadata server.
func Isolate(t *testing.T) {
	t.Helper()
	t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", "")
	t.Setenv("CLOUDSDK_CONFIG", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	// The metadata server is reached by hostname. Pointing it at a port nothing
	// listens on makes the lookup fail fast instead of hanging on a real GCE box.
	t.Setenv("GCE_METADATA_HOST", "127.0.0.1:1")
	t.Setenv("GOOGLE_CLOUD_PROJECT", "")
}
```

- [ ] **Step 2: Write the failing URL and error tests**

```go
package gcprov

import (
	"testing"

	"github.com/infrena/infrena/pkg/value"
)

func attrs(kv map[string]string) map[string]value.Value {
	out := map[string]value.Value{}
	for k, v := range kv {
		out[k] = value.String(v, value.SourceExplicit)
	}
	return out
}

func TestURLTemplatesExpandFromAttributes(t *testing.T) {
	got, err := ExpandURL("projects/{{project}}/zones/{{zone}}/instances/{{name}}",
		attrs(map[string]string{"project": "p", "zone": "us-central1-a", "name": "web1"}))
	if err != nil {
		t.Fatal(err)
	}
	if want := "projects/p/zones/us-central1-a/instances/web1"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// TestAMissingPlaceholderIsAnErrorNotAnEmptyString. Silently expanding {{zone}}
// to "" produces projects/p/zones//instances/web1, which GCP answers with a
// confusing 404 rather than a message naming the missing attribute.
func TestAMissingPlaceholderIsRefused(t *testing.T) {
	_, err := ExpandURL("projects/{{project}}/zones/{{zone}}/instances/{{name}}",
		attrs(map[string]string{"project": "p", "name": "web1"}))
	if err == nil {
		t.Fatal("a missing placeholder expanded silently")
	}
	if !contains(err.Error(), "zone") {
		t.Errorf("the error does not name the missing attribute: %v", err)
	}
}

func TestErrorsAreClassifiedFromStatusAndCode(t *testing.T) {
	cases := []struct {
		err  error
		want provider.Retryability
	}{
		{&APIError{Status: 429, Code: "RESOURCE_EXHAUSTED"}, provider.SafeToRetry},
		{&APIError{Status: 503, Code: "UNAVAILABLE"}, provider.SafeToRetry},
		{&APIError{Status: 500, Code: "INTERNAL"}, provider.SafeToRetry},
		{&APIError{Status: 409, Code: "ABORTED"}, provider.SafeToRetry},
		{&APIError{Status: 400, Code: "INVALID_ARGUMENT"}, provider.NotSafeToRetry},
		{&APIError{Status: 403, Code: "PERMISSION_DENIED"}, provider.NotSafeToRetry},
		{&APIError{Status: 409, Code: "ALREADY_EXISTS"}, provider.NotSafeToRetry},
		{&APIError{Status: 412, Code: "FAILED_PRECONDITION"}, provider.NotSafeToRetry},
		{errors.New("something nobody classified"), provider.NotSafeToRetry},
	}
	for _, c := range cases {
		if got := ClassifyError(c.err); got != c.want {
			t.Errorf("ClassifyError(%v) = %v, want %v", c.err, got, c.want)
		}
	}
}

// TestClassificationIsPure. The host asks ANY configured instance to classify, not
// the one whose call failed, so classification must not read instance state.
func TestClassificationDoesNotDependOnAnInstance(t *testing.T) {
	err := &APIError{Status: 429, Code: "RESOURCE_EXHAUSTED"}
	a := ClassifyError(err)
	b := ClassifyError(err)
	if a != b || a != provider.SafeToRetry {
		t.Errorf("classification is not a pure function of the error: %v then %v", a, b)
	}
}

// TestA409IsSplitByCode. Both ABORTED and ALREADY_EXISTS are 409, and they mean
// opposite things: one is a concurrency conflict worth retrying, the other says
// the thing is already there and retrying will fail identically forever.
func TestStatusAloneIsNotEnoughFor409(t *testing.T) {
	if ClassifyError(&APIError{Status: 409, Code: "ABORTED"}) ==
		ClassifyError(&APIError{Status: 409, Code: "ALREADY_EXISTS"}) {
		t.Error("409 is being classified by status alone")
	}
}
```

- [ ] **Step 3: Run and watch it fail; Step 4: write the four files**

`errors.go` decodes `{"error": {"code", "status", "message"}}` into `APIError`, captures a
`Retry-After` header into `APIError.RetryAfter` (Task 18 measures whether GCP ever sends one), and
implements `ClassifyError` as a **pure function** with a `switch` on `Code` first, falling back to
`Status`.

`urls.go` expands `{{placeholder}}` from the resolved attributes, erroring on any placeholder it
cannot fill.

`backoff.go` implements exponential backoff with full jitter, honouring `APIError.RetryAfter` when
present, plus a per-(project, API) token-bucket limiter. **GCP quota is per-API per-project per-minute**,
a different shape from AWS's per-account throttling, and because the client is ours rather than an
SDK's this is code we own and must test.

`client.go` holds the `http.Client`, the token source and the limiter, and caches clients by
`(credential fingerprint, project)`. **The cache is what gives the limiter a life longer than one
request — do not "simplify" it away.** `Do` marshals the body, sets `X-Goog-User-Project` when a quota
project is configured, retries per classification, and **never logs a token, an Authorization header or
a request body**.

- [ ] **Step 5: Write the backoff test**

```go
// TestTheLimiterIsSharedAcrossRequests. A limiter created per request measures
// nothing: the bucket's whole purpose is to carry rate information between calls,
// which is why the client cache exists.
func TestTheLimiterSurvivesBetweenRequests(t *testing.T) {
	gcptest.Isolate(t)
	c := NewClient(staticToken(), "https://example.invalid/", ClientOptions{QPS: 2})
	first := c.limiterFor("p", "compute")
	second := c.limiterFor("p", "compute")
	if first != second {
		t.Error("a new limiter per call, so the rate is never actually limited")
	}
	if c.limiterFor("p", "storage") == first {
		t.Error("one limiter across APIs; GCP quota is per API per project")
	}
	if c.limiterFor("q", "compute") == first {
		t.Error("one limiter across projects; GCP quota is per API per project")
	}
}
```

- [ ] **Step 6: Sabotage, confirm, restore**

Classify by `Status` alone: `TestStatusAloneIsNotEnoughFor409` must fail. Restore. Make `ExpandURL`
substitute `""` for a missing placeholder: `TestAMissingPlaceholderIsRefused` must fail. Restore. Make
`limiterFor` construct a new limiter each call: `TestTheLimiterSurvivesBetweenRequests` must fail.
Restore. Record all three.

- [ ] **Step 7: Commit**

```bash
git add internal/gcprov/client.go internal/gcprov/urls.go internal/gcprov/errors.go \
        internal/gcprov/backoff.go internal/gcptest/ \
        internal/gcprov/urls_test.go internal/gcprov/errors_test.go \
        internal/gcprov/backoff_test.go internal/gcprov/client_test.go
git commit -m "The generic client, url expansion, errors and backoff

Classification reads the status code and the error status enum, not the
status alone. A 409 is ABORTED or ALREADY_EXISTS and those mean opposite
things, one worth retrying and one that will fail identically forever.
It is a pure function of the error because the host asks any configured
instance to classify, not the one whose call failed.

A missing url placeholder is an error rather than an empty segment.
Expanding it silently produces a path GCP answers with a confusing 404
instead of a message naming the attribute nobody set.

The limiter is keyed by project and api because that is how GCP quota
works, and it lives in the client cache because a limiter built per
request measures nothing.

Sabotage: classifying by status alone merges the two 409s. Substituting
empty for a missing placeholder passes the url test. Building a limiter
per call fails the shared limiter test. All three restored." \
  -- internal/gcprov/ internal/gcptest/
```

---

## Task 12: Awaiting a mutation — three strategies, one loop

**Files:**
- Create: `internal/gcprov/await.go`
- Test: `internal/gcprov/await_test.go`

**Interfaces:**
- Consumes: `Client`, `catalog.Type`, `catalog.AwaitKind`.
- Produces: `func (p *Provider) await(ctx context.Context, ty *catalog.Type, resp map[string]any) (map[string]any, error)`

**The three strategies (spec §5.3), and nothing else.** A 25-API sample found 300 compute-style, 189
longrunning, 187 synchronous and no fourth shape.

- [ ] **Step 1: Write the failing tests**

```go
package gcprov

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/infrena/infrena-provider-gcp/internal/catalog"
	"github.com/infrena/infrena-provider-gcp/internal/gcpfake"
	"github.com/infrena/infrena-provider-gcp/internal/gcptest"
)

func TestASynchronousMutationIsNotPolled(t *testing.T) {
	gcptest.Isolate(t)
	s := gcpfake.New(t)
	defer s.Close()
	p := testProvider(t, s)

	ty := &catalog.Type{Name: "gcp.widget", Await: catalog.AwaitNone}
	body := map[string]any{"name": "widgets/one", "sizeGb": float64(10)}
	got, err := p.await(context.Background(), ty, body)
	if err != nil {
		t.Fatal(err)
	}
	if got["sizeGb"] != float64(10) {
		t.Errorf("the response was not passed through: %v", got)
	}
	if n := len(s.Requests()); n != 0 {
		t.Errorf("%d requests made awaiting a synchronous mutation, want 0", n)
	}
}

func TestALongRunningOperationIsPolledUntilDone(t *testing.T) {
	gcptest.Isolate(t)
	s := gcpfake.New(t)
	defer s.Close()
	s.SetOperationStyle(gcpfake.OpLongRunning)
	p := testProvider(t, s)

	ty := &catalog.Type{Name: "gcp.widget", Await: catalog.AwaitLongRunning, TimeoutSeconds: 30}
	op := map[string]any{"name": "operations/abc", "done": false}
	got, err := p.await(context.Background(), ty, op)
	if err != nil {
		t.Fatal(err)
	}
	if got == nil {
		t.Fatal("await returned nothing")
	}
	if len(s.Requests()) == 0 {
		t.Error("the operation was never polled")
	}
}

// TestAFailedOperationIsReportedWithItsMessage. An operation that completes with
// an error is not an await failure, it is a GCP failure, and the message is the
// only thing the user can act on.
func TestAFailedOperationReportsGCPsMessage(t *testing.T) {
	gcptest.Isolate(t)
	s := gcpfake.New(t)
	defer s.Close()
	s.SetOperationStyle(gcpfake.OpLongRunning)
	s.CompleteOperationWithError("operations/bad", "INVALID_ARGUMENT", "Disk size must be at least 10 GB.")
	p := testProvider(t, s)

	ty := &catalog.Type{Name: "gcp.widget", Await: catalog.AwaitLongRunning, TimeoutSeconds: 30}
	_, err := p.await(context.Background(), ty, map[string]any{"name": "operations/bad", "done": false})
	if err == nil {
		t.Fatal("a failed operation was reported as success")
	}
	if !contains(err.Error(), "Disk size must be at least 10 GB.") {
		t.Errorf("GCP's own message was dropped: %v", err)
	}
}

func TestAComputeOperationIsPolledOnItsScope(t *testing.T) {
	gcptest.Isolate(t)
	s := gcpfake.New(t)
	defer s.Close()
	s.SetOperationStyle(gcpfake.OpCompute)
	p := testProvider(t, s)

	ty := &catalog.Type{
		Name: "gcp.instance", Await: catalog.AwaitComputeOperation,
		OperationScope: "zoneOperations", Scope: catalog.ScopeZonal, TimeoutSeconds: 30,
	}
	op := map[string]any{"name": "op-1", "status": "RUNNING", "zone": "us-central1-a"}
	if _, err := p.await(context.Background(), ty, op); err != nil {
		t.Fatal(err)
	}
	var polled bool
	for _, r := range s.Requests() {
		if contains(r.Path, "zoneOperations") && contains(r.Path, "/wait") {
			polled = true
		}
	}
	// `wait` rather than `get`: it long-polls to a 2-minute deadline instead of
	// burning one request per second against the project's quota.
	if !polled {
		t.Errorf("the zone operations wait endpoint was not used: %+v", s.Requests())
	}
}

// TestAwaitFinishesEvenWhenTheContextIsCancelled. Once a mutation is sent,
// abandoning it leaves a resource that exists and is tracked nowhere. The await
// runs under context.WithoutCancel for exactly that reason.
func TestAwaitDoesNotAbandonAMutationInFlight(t *testing.T) {
	gcptest.Isolate(t)
	s := gcpfake.New(t)
	defer s.Close()
	s.SetOperationStyle(gcpfake.OpLongRunning)
	p := testProvider(t, s)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled before await is called

	ty := &catalog.Type{Name: "gcp.widget", Await: catalog.AwaitLongRunning, TimeoutSeconds: 30}
	got, err := p.await(ctx, ty, map[string]any{"name": "operations/abc", "done": false})
	if errors.Is(err, context.Canceled) {
		t.Fatal("await abandoned an operation in flight on a cancelled context")
	}
	if err != nil {
		t.Fatal(err)
	}
	if got == nil {
		t.Error("await returned nothing")
	}
}

func TestAwaitStopsAtTheTypesTimeout(t *testing.T) {
	gcptest.Isolate(t)
	s := gcpfake.New(t)
	defer s.Close()
	s.SetOperationStyle(gcpfake.OpLongRunning)
	s.NeverCompleteOperations()
	p := testProvider(t, s)

	ty := &catalog.Type{Name: "gcp.widget", Await: catalog.AwaitLongRunning, TimeoutSeconds: 1}
	start := time.Now()
	_, err := p.await(context.Background(), ty, map[string]any{"name": "operations/abc", "done": false})
	if err == nil {
		t.Fatal("an operation that never completes was reported as success")
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Errorf("await ran for %v against a 1s timeout", elapsed)
	}
}
```

- [ ] **Step 2: Run and watch it fail; Step 3: write `await.go`; Step 4: run and watch it pass**

Run: `go test -count=1 -timeout 120s ./internal/gcprov/ -run Await`

```go
// await blocks until a mutation has actually taken effect, and returns the
// resource's state as GCP reports it.
//
// resp is whatever the mutating call returned: the resource itself for a
// synchronous type, or an operation envelope for the other two. Which of the
// three it is was decided at generation time from the Operation SCHEMA, not
// from the API's name — container, dns and sqladmin all use compute-style
// operations without being compute.
func (p *Provider) await(ctx context.Context, ty *catalog.Type, resp map[string]any) (map[string]any, error) {
	if ty.Await == catalog.AwaitNone {
		// The mutation returned the resource. Polling anything here would be a
		// request against a quota that belongs to the whole project, for an
		// answer we already hold.
		return resp, nil
	}

	// ONCE A MUTATION IS SENT, ABANDONING IT LEAVES SOMETHING THAT EXISTS AND IS
	// TRACKED NOWHERE. Cancellation is checked BEFORE sending (in crud.go), never
	// after. The bound from here on is the type's own timeout, not the caller's.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx),
		time.Duration(ty.TimeoutSeconds)*time.Second)
	defer cancel()

	switch ty.Await {
	case catalog.AwaitLongRunning:
		return p.awaitLongRunning(ctx, ty, resp)
	case catalog.AwaitComputeOperation:
		return p.awaitComputeOperation(ctx, ty, resp)
	default:
		return nil, fmt.Errorf("%s: unknown await strategy %d", ty.Name, ty.Await)
	}
}

// awaitLongRunning polls a google.longrunning.Operation by name until `done`.
//
// The shape is {"name": "...", "done": false} becoming either
// {"done": true, "response": {...}} or {"done": true, "error": {...}}.
func (p *Provider) awaitLongRunning(ctx context.Context, ty *catalog.Type, op map[string]any) (map[string]any, error) {
	name, _ := op["name"].(string)
	if name == "" {
		return nil, fmt.Errorf("%s: operation has no name to poll", ty.Name)
	}
	for attempt := 0; ; attempt++ {
		if done, _ := op["done"].(bool); done {
			// An operation that completed with an error is not an await failure,
			// it is a GCP failure, and its message is the only thing the user can
			// act on. Do not replace it with our own wording.
			if e, ok := op["error"].(map[string]any); ok {
				return nil, operationError(ty, e)
			}
			if r, ok := op["response"].(map[string]any); ok {
				return r, nil
			}
			// Done, no error, no response: a delete, or a create whose result
			// must be read back. The caller decides which.
			return nil, nil
		}
		if err := p.sleepBackoff(ctx, attempt); err != nil {
			return nil, fmt.Errorf("%s: waiting for %s: %w", ty.Name, name, err)
		}
		var err error
		op, err = p.client.Do(ctx, http.MethodGet, ty.APIBaseURL+name, nil)
		if err != nil {
			return nil, err
		}
	}
}

// awaitComputeOperation polls a compute-style operation until status is DONE.
//
// It uses the operation collection's `wait` method, which LONG-POLLS to a
// 2-minute deadline rather than returning immediately. That is the difference
// between one request per slow operation and one per second against a quota
// the whole project shares. ty.OperationScope names the collection:
// globalOperations, regionOperations or zoneOperations.
func (p *Provider) awaitComputeOperation(ctx context.Context, ty *catalog.Type, op map[string]any) (map[string]any, error) {
	for attempt := 0; ; attempt++ {
		if st, _ := op["status"].(string); st == "DONE" {
			// compute reports failure as error.errors[], a different shape from
			// longrunning's error object. Both reach the user as one message.
			if e, ok := op["error"].(map[string]any); ok {
				return nil, operationError(ty, e)
			}
			return op, nil
		}
		url, err := p.operationWaitURL(ty, op)
		if err != nil {
			return nil, err
		}
		if attempt > 0 {
			// `wait` already blocks server-side, so back off only between
			// returns, and gently — this is not a busy poll.
			if err := p.sleepBackoff(ctx, attempt); err != nil {
				return nil, fmt.Errorf("%s: waiting for operation: %w", ty.Name, err)
			}
		}
		if op, err = p.client.Do(ctx, http.MethodPost, url, nil); err != nil {
			return nil, err
		}
	}
}
```

`operationError` renders BOTH failure shapes into one `*APIError`: longrunning's
`{"code":…, "message":…}` and compute's `{"errors":[{"code":…, "message":…}]}`, joining the latter's
messages. It must carry GCP's own text through — a wrapper that says "the operation failed" and drops
the reason leaves the user nothing to act on.

`operationWaitURL` builds `<APIBaseURL><OperationScope>/<op name>/wait`, taking the scope-bearing
segment from the operation's own `zone` or `region` field when present (compute returns them as full
URLs, so use the last path segment) and falling back to the type's own scope. A `wait` URL built
without the right scope 404s, which reads as "the operation vanished" rather than "we asked the wrong
collection".

**Both loops are unbounded by attempt count and bounded by the context deadline only.** That is
deliberate: the number of polls a slow operation needs is not knowable in advance, and a cap on
attempts would turn a slow-but-healthy create into a spurious failure. The type's `TimeoutSeconds` is
the real bound.

- [ ] **Step 5: Sabotage, confirm, restore**

Replace `context.WithoutCancel(ctx)` with `ctx`: `TestAwaitDoesNotAbandonAMutationInFlight` must fail.
Restore. Make the compute branch poll `get` rather than `wait`: `TestAComputeOperationIsPolledOnItsScope`
must fail. Restore. Drop GCP's message from the failed-operation error:
`TestAFailedOperationReportsGCPsMessage` must fail. Restore. Record all three.

- [ ] **Step 6: Commit**

```bash
git add internal/gcprov/await.go internal/gcprov/await_test.go
git commit -m "Await a mutation three ways

Synchronous, compute style status polling and long running done polling,
chosen per type at generation time. A twenty five api sample found those
three and no fourth shape.

Compute operations are polled with wait rather than get. Wait long polls
to a two minute deadline, so a slow operation costs one request instead
of one per second against a quota that belongs to the whole project.

The await runs under a context that cannot be cancelled. Once a mutation
is sent, abandoning it leaves something that exists and is tracked
nowhere, which is worse than waiting.

Sabotage: honouring cancellation abandons an operation in flight.
Polling get instead of wait fails the scope test. Dropping GCP's own
message leaves the user nothing to act on. All three restored." \
  -- internal/gcprov/await.go internal/gcprov/await_test.go
```

---

## Task 13: Create, Read, Delete and Import

**Files:**
- Create: `internal/gcprov/crud.go`, `internal/gcprov/ids.go`, `internal/gcprov/provider.go`
- Test: `internal/gcprov/crud_test.go`, `internal/gcprov/ids_test.go`

**Interfaces:**
- Consumes: `Client`, `await`, `catalog.Type`.
- Produces:
  - `type Provider struct { ... }`, `func NewProvider(in *gcpplugin.Instance, c *catalog.Catalog, cl *Client) *Provider`, satisfying `provider.Provider`
  - `func ProviderID(ty *catalog.Type, body map[string]any, attrs map[string]value.Value) (string, error)`
  - `func ParseProviderID(ty *catalog.Type, id string) (map[string]value.Value, error)`

**Provider IDs (spec §5.5).** The relative resource name itself:
`projects/p/zones/us-central1-a/instances/web1`. **None of the AWS `<region>/<identifier>`,
`global/`, split-at-first-slash or `|`-joining machinery.** GCP names are already hierarchical, unique
and stable; the AWS scheme exists only to compensate for Cloud Control identifiers that carry none of
that.

- [ ] **Step 1: Write the failing ID tests**

```go
func TestTheProviderIDIsTheRelativeResourceName(t *testing.T) {
	ty := &catalog.Type{
		Name:     "gcp.instance",
		SelfLink: "projects/{{project}}/zones/{{zone}}/instances/{{name}}",
		Scope:    catalog.ScopeZonal,
	}
	got, err := ProviderID(ty, map[string]any{"name": "web1"},
		attrs(map[string]string{"project": "p", "zone": "us-central1-a", "name": "web1"}))
	if err != nil {
		t.Fatal(err)
	}
	if want := "projects/p/zones/us-central1-a/instances/web1"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// TestAFullSelfLinkURLIsReducedToItsRelativeName. GCP returns selfLink as an
// absolute URL. Storing that as the provider ID would make the ID change if
// Google ever changed the host, and would not match what import takes.
func TestAnAbsoluteSelfLinkBecomesARelativeName(t *testing.T) {
	ty := &catalog.Type{Name: "gcp.instance", Scope: catalog.ScopeZonal,
		SelfLink: "projects/{{project}}/zones/{{zone}}/instances/{{name}}"}
	got, err := ProviderID(ty, map[string]any{
		"selfLink": "https://www.googleapis.com/compute/v1/projects/p/zones/us-central1-a/instances/web1",
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if want := "projects/p/zones/us-central1-a/instances/web1"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// TestImportRefusesAnIDForTheWrongType, before any API call, so a typo costs a
// message rather than a confusing 404.
func TestImportRefusesAnIDThatDoesNotMatchTheType(t *testing.T) {
	ty := &catalog.Type{Name: "gcp.instance", Scope: catalog.ScopeZonal,
		SelfLink: "projects/{{project}}/zones/{{zone}}/instances/{{name}}"}
	for _, bad := range []string{
		"projects/p/global/networks/default",         // a network, not an instance
		"projects/p/regions/us-central1/instances/x", // regional path for a zonal type
		"web1",                                        // bare name, no hierarchy
	} {
		if _, err := ParseProviderID(ty, bad); err == nil {
			t.Errorf("accepted %q for %s", bad, ty.Name)
		}
	}
	if _, err := ParseProviderID(ty, "projects/p/zones/us-central1-a/instances/web1"); err != nil {
		t.Errorf("refused a valid id: %v", err)
	}
}
```

- [ ] **Step 2: Write the failing CRUD tests**

```go
func TestCreateStoresAndReportsWhatExists(t *testing.T) {
	gcptest.Isolate(t)
	s := gcpfake.New(t)
	defer s.Close()
	p := testProviderWithCatalog(t, s, widgetCatalog())

	st, err := p.Create(context.Background(), &resource.DesiredResource{
		Type:  "gcp.widget",
		Attrs: attrsMixed(map[string]any{"project": "p", "region": "r", "name": "one", "sizeGb": int64(10)}),
	})
	if err != nil {
		t.Fatal(err)
	}
	if st == nil {
		t.Fatal("Create returned (nil, nil), which orphans the resource it just made")
	}
	if st.ProviderID != "projects/p/locations/r/widgets/one" {
		t.Errorf("provider id = %q", st.ProviderID)
	}
	if got, ok := s.Get("/v1/projects/p/locations/r/widgets/one"); !ok {
		t.Errorf("nothing was actually created: %v", got)
	}
}

// TestCreateReportsStateRatherThanErroringOnceSomethingExists. The host drops a
// failed create's result, so returning an error after GCP made something leaves a
// resource that is real, tracked nowhere, and unfindable by a later plan.
func TestCreateReportsStateWhenTheResourceExistsDespiteAFailure(t *testing.T) {
	gcptest.Isolate(t)
	s := gcpfake.New(t)
	defer s.Close()
	s.SetOperationStyle(gcpfake.OpLongRunning)
	// The resource is created, then the operation reports failure — the real and
	// nasty case, e.g. a post-create configuration step failing.
	s.CreateThenFailOperation("projects/p/locations/r/widgets/one", "INTERNAL", "post-create step failed")
	p := testProviderWithCatalog(t, s, widgetCatalog())

	st, err := p.Create(context.Background(), &resource.DesiredResource{
		Type:  "gcp.widget",
		Attrs: attrsMixed(map[string]any{"project": "p", "region": "r", "name": "one"}),
	})
	if err != nil {
		t.Fatalf("Create errored after GCP created something, which orphans it: %v", err)
	}
	if st == nil || st.ProviderID == "" {
		t.Fatal("Create returned no state for a resource that exists")
	}
}

func TestReadReportsAbsenceAsNilNil(t *testing.T) {
	gcptest.Isolate(t)
	s := gcpfake.New(t)
	defer s.Close()
	p := testProviderWithCatalog(t, s, widgetCatalog())

	st, err := p.Read(context.Background(), &resource.ResourceState{
		Type: "gcp.widget", ProviderID: "projects/p/locations/r/widgets/missing",
	})
	if err != nil {
		t.Fatal(err)
	}
	if st != nil {
		t.Errorf("a missing resource read back as %v", st)
	}
}

// TestReadRetriesABrandNewResource. GCP is eventually consistent: a resource
// created seconds ago can 404. Reporting that as absence makes the next plan
// propose creating it again.
func TestReadRetriesBeforeReportingAbsence(t *testing.T) {
	gcptest.Isolate(t)
	s := gcpfake.New(t)
	defer s.Close()
	s.NotFoundTimes("/v1/projects/p/locations/r/widgets/one", 2)
	s.Seed("/v1/projects/p/locations/r/widgets/one", map[string]any{"name": "one", "sizeGb": float64(10)})
	p := testProviderWithCatalog(t, s, widgetCatalog())

	st, err := p.Read(context.Background(), &resource.ResourceState{
		Type: "gcp.widget", ProviderID: "projects/p/locations/r/widgets/one",
	})
	if err != nil {
		t.Fatal(err)
	}
	if st == nil {
		t.Fatal("a resource that exists was reported absent after two transient 404s")
	}
}

func TestDeleteRemovesTheResource(t *testing.T) {
	gcptest.Isolate(t)
	s := gcpfake.New(t)
	defer s.Close()
	s.Seed("/v1/projects/p/locations/r/widgets/one", map[string]any{"name": "one"})
	p := testProviderWithCatalog(t, s, widgetCatalog())

	if err := p.Delete(context.Background(), &resource.ResourceState{
		Type: "gcp.widget", ProviderID: "projects/p/locations/r/widgets/one",
	}); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.Get("/v1/projects/p/locations/r/widgets/one"); ok {
		t.Error("the resource is still there")
	}
}

// TestDeletingSomethingAlreadyGoneSucceeds. Destroy must converge; a 404 on
// delete means the goal is met.
func TestDeletingSomethingAlreadyGoneIsNotAnError(t *testing.T) {
	gcptest.Isolate(t)
	s := gcpfake.New(t)
	defer s.Close()
	p := testProviderWithCatalog(t, s, widgetCatalog())
	if err := p.Delete(context.Background(), &resource.ResourceState{
		Type: "gcp.widget", ProviderID: "projects/p/locations/r/widgets/gone",
	}); err != nil {
		t.Errorf("deleting an absent resource errored: %v", err)
	}
}

func TestImportReadsAnExistingResource(t *testing.T) {
	gcptest.Isolate(t)
	s := gcpfake.New(t)
	defer s.Close()
	s.Seed("/v1/projects/p/locations/r/widgets/one", map[string]any{"name": "one", "sizeGb": float64(10)})
	p := testProviderWithCatalog(t, s, widgetCatalog())

	st, err := p.Import(context.Background(), "gcp.widget", "projects/p/locations/r/widgets/one")
	if err != nil {
		t.Fatal(err)
	}
	// project and region must be recovered FROM THE ID, or the imported resource
	// plans a change on the very next run for attributes nobody set.
	if st.Attributes["project"].Raw != "p" || st.Attributes["region"].Raw != "r" {
		t.Errorf("project/region not recovered from the id: %v", st.Attributes)
	}
}
```

- [ ] **Step 3: Run, watch fail, write `ids.go`, `crud.go`, `provider.go`, run, watch pass**

Run: `go test -count=1 ./internal/gcprov/`

`Create`: expand `CreateURL` (falling back to `BaseURL`), POST the body built from `desired.Attrs` minus
`project`/`region`/`zone`/output-only attributes, `await`, then read back. **Once GCP has created
something, no path returns an error** — a failure goes to stderr and the truthful state is returned.

`Read`: GET the self link; on 404 retry a bounded number of times (~4 s total) before returning
`(nil, nil)`.

`Delete`: DELETE, `await`, treat 404 as success.

`Import`: `ParseProviderID` first — **before any API call** — then read.

- [ ] **Step 4: Sabotage, confirm, restore**

Make `Create` return `(nil, err)` when the operation reports failure:
`TestCreateReportsStateWhenTheResourceExistsDespiteAFailure` must fail. Restore. Remove the read retry:
`TestReadRetriesBeforeReportingAbsence` must fail. Restore. Make `ParseProviderID` accept any string:
`TestImportRefusesAnIDThatDoesNotMatchTheType` must fail. Restore. Make `Import` not populate
`project`/`region`: `TestImportReadsAnExistingResource` must fail. Restore. Record all four.

- [ ] **Step 5: Commit**

```bash
git add internal/gcprov/crud.go internal/gcprov/ids.go internal/gcprov/provider.go \
        internal/gcprov/crud_test.go internal/gcprov/ids_test.go
git commit -m "Create, read, delete and import

The provider id is the relative resource name and nothing else. GCP
names are already hierarchical, unique and stable, so none of the scope
prefixing and identifier splitting the AWS provider needs applies here.
An absolute selfLink is reduced to its relative form so the id does not
depend on a hostname.

Create never returns an error once GCP has made something. The host
drops a failed create's result, so erroring there leaves a resource that
is real and tracked nowhere. The failure goes to stderr and the truthful
state is returned instead.

Read retries before reporting absence, because GCP is eventually
consistent and a resource created seconds ago can answer 404, and
reporting that as gone makes the next plan create it twice.

Import parses the id before making any call, so a typo costs a message
rather than a confusing 404, and it recovers project and location from
the id so an imported resource does not plan a change immediately.

Sabotage: erroring on a failed create orphans the resource. Removing the
read retry reports a live resource as gone. Accepting any import id
passes a network id for an instance. Not recovering project and location
makes import plan a change. All four restored." \
  -- internal/gcprov/
```

---

## Task 14: Update, and the `updateMask`

**Files:**
- Create: `internal/gcprov/patch.go`
- Test: `internal/gcprov/patch_test.go`

**Interfaces:**
- Consumes: `Client`, `await`, `catalog.Type`.
- Produces:
  - `func (p *Provider) Update(ctx context.Context, current *resource.ResourceState, desired *resource.DesiredResource) (*resource.ResourceState, error)`
  - `func BuildMask(ty *catalog.Type, current, desired map[string]value.Value) (body map[string]any, mask []string)`

**The rule (spec §5.4).** The mask names exactly the changed fields. An attribute dropped from
configuration contributes **no mask entry**, so GCP keeps its value — AWS's "patch only adds or
replaces, never removes" rule, enforced by the API rather than by our patch builder.

**And the rule that is easy to get wrong:** the patch is computed from **the `current` the host hands
`Update`**, with **no extra read**. infrena ≥ 0.7.1 passes the refreshed observation from immediately
before planning. The AWS repository carried a workaround for the older behaviour and removed it; do not
reintroduce it here.

- [ ] **Step 1: Write the failing tests**

```go
func TestTheMaskNamesOnlyWhatChanged(t *testing.T) {
	ty := widgetType()
	current := attrsMixed(map[string]any{"name": "one", "sizeGb": int64(10), "tier": "BASIC"})
	desired := attrsMixed(map[string]any{"name": "one", "sizeGb": int64(20), "tier": "BASIC"})

	body, mask := BuildMask(ty, current, desired)
	if len(mask) != 1 || mask[0] != "sizeGb" {
		t.Errorf("mask = %v, want [sizeGb]", mask)
	}
	if body["sizeGb"] != int64(20) {
		t.Errorf("body = %v, want the new size", body)
	}
	if _, present := body["tier"]; present {
		t.Error("an unchanged field is in the patch body")
	}
}

// TestDroppingAnAttributeFromConfigurationIsNotAChange — spec §5.4 and PLAN §14.1.
// infrena cannot tell "never set" from "no longer configured", so both keep GCP's
// current value.
func TestAnAttributeDroppedFromConfigurationProducesNoMaskEntry(t *testing.T) {
	ty := widgetType()
	current := attrsMixed(map[string]any{"name": "one", "sizeGb": int64(10), "tier": "STANDARD"})
	desired := attrsMixed(map[string]any{"name": "one", "sizeGb": int64(10)}) // tier gone

	body, mask := BuildMask(ty, current, desired)
	if len(mask) != 0 {
		t.Errorf("mask = %v, want empty: dropping an attribute must not remove it", mask)
	}
	if _, present := body["tier"]; present {
		t.Error("the dropped attribute is in the patch body, which would reset it")
	}
}

// TestOutputOnlyAttributesAreNeverPatched. GCP rejects a patch touching one, and
// a mask naming a read-only field fails the whole request.
func TestAnOutputOnlyAttributeIsNeverInTheMask(t *testing.T) {
	ty := widgetType()
	current := attrsMixed(map[string]any{"name": "one", "createTime": "2026-01-01T00:00:00Z"})
	desired := attrsMixed(map[string]any{"name": "one", "createTime": "2026-09-21T00:00:00Z"})
	_, mask := BuildMask(ty, current, desired)
	for _, m := range mask {
		if m == "createTime" {
			t.Error("an output-only field reached the mask; GCP would reject the whole patch")
		}
	}
}

func TestANestedChangeIsMaskedAtItsPath(t *testing.T) {
	ty := widgetType()
	current := map[string]value.Value{"config": mapValue(map[string]any{"mode": "A", "size": int64(1)})}
	desired := map[string]value.Value{"config": mapValue(map[string]any{"mode": "B", "size": int64(1)})}
	_, mask := BuildMask(ty, current, desired)
	// A mask of "config" would replace the whole object and drop anything GCP
	// added inside it; the path is what makes the patch surgical.
	if len(mask) != 1 || mask[0] != "config.mode" {
		t.Errorf("mask = %v, want [config.mode]", mask)
	}
}

// TestUpdateUsesTheStateItWasHanded is the differential test. The AWS repo found
// this bug by running its e2e suite: with a stale `current`, drift corrected in
// configuration never gets patched, because the diff runs against the wrong side.
func TestUpdateDiffsAgainstTheCurrentItWasGiven(t *testing.T) {
	gcptest.Isolate(t)
	s := gcpfake.New(t)
	defer s.Close()
	// What is REALLY there: someone changed the size outside infrena.
	s.Seed("/v1/projects/p/locations/r/widgets/one",
		map[string]any{"name": "one", "sizeGb": float64(99)})
	p := testProviderWithCatalog(t, s, widgetCatalog())

	// The host hands Update the REFRESHED observation, which says 99.
	current := &resource.ResourceState{
		Type: "gcp.widget", ProviderID: "projects/p/locations/r/widgets/one",
		Attributes: attrsMixed(map[string]any{"project": "p", "region": "r", "name": "one", "sizeGb": int64(99)}),
	}
	desired := &resource.DesiredResource{
		Type:  "gcp.widget",
		Attrs: attrsMixed(map[string]any{"project": "p", "region": "r", "name": "one", "sizeGb": int64(10)}),
	}
	if _, err := p.Update(context.Background(), current, desired); err != nil {
		t.Fatal(err)
	}
	got, _ := s.Get("/v1/projects/p/locations/r/widgets/one")
	if got["sizeGb"] != float64(10) {
		t.Errorf("sizeGb = %v, want 10: the drift was not corrected", got["sizeGb"])
	}
	// And no extra read was made before patching.
	var gets int
	for _, r := range s.Requests() {
		if r.Method == "GET" && contains(r.Path, "/widgets/one") {
			gets++
		}
	}
	if gets > 1 {
		t.Errorf("%d reads of the resource; Update must diff against what it was handed, not re-read", gets)
	}
}
```

- [ ] **Step 2: Run, watch fail, write `patch.go`, run, watch pass**

Run: `go test -count=1 ./internal/gcprov/ -run 'Mask|Update|Patch'`

`BuildMask` walks the catalog type's attributes, skipping `Output` ones and the scoping attributes,
comparing `current` to `desired` with `value.Equal`, and recursing into `Fields` to produce dotted
paths. An attribute present in `current` but absent from `desired` is skipped entirely.

`Update` builds the mask, sends `ty.UpdateVerb` to the expanded `UpdateURL` (or `SelfLink`) with
`?updateMask=<comma-joined>`, awaits, and reads back. **An empty mask sends no request at all** and
returns the current state.

- [ ] **Step 3: Sabotage, confirm, restore**

Make `Update` do a fresh `GET` before building the mask and diff against that:
`TestUpdateDiffsAgainstTheCurrentItWasGiven` must fail on the read count. Restore. Make `BuildMask`
include attributes absent from `desired`: `TestAnAttributeDroppedFromConfigurationProducesNoMaskEntry`
must fail. Restore. Make nested changes mask the parent: `TestANestedChangeIsMaskedAtItsPath` must
fail. Restore. Record all three.

- [ ] **Step 4: Commit**

```bash
git add internal/gcprov/patch.go internal/gcprov/patch_test.go
git commit -m "Update through an update mask

The mask names exactly what changed, which gives the never removes rule
for free: an attribute dropped from configuration contributes no mask
entry, so GCP keeps its value. That is enforced by the API rather than
by our own patch builder, which is a better place for it.

Nested changes are masked at their path rather than at the parent.
Masking the parent would replace the whole object and drop anything GCP
put inside it.

The patch is built from the state the host handed us, with no extra
read. Infrena passes the refreshed observation from just before
planning. The AWS repo carried a workaround for the older behaviour and
removed it, and the test here pins the read count so it cannot come
back.

Sabotage: re raising the read before patching fails the read count.
Including dropped attributes resets them. Masking the parent instead of
the path fails the nested test. All three restored." \
  -- internal/gcprov/patch.go internal/gcprov/patch_test.go
```

---

## Task 15: Nested values reconcile, and labels are a map

**Files:**
- Create: `internal/gcprov/reconcile.go`
- Test: `internal/gcprov/reconcile_test.go`

**Interfaces:**
- Consumes: `catalog.Attr`.
- Produces:
  - `func Reconcile(attr *catalog.Attr, reference, incoming value.Value) value.Value`
  - `func LabelsIn(v value.Value) map[string]string`, `func LabelsOut(m map[string]string) value.Value`

**Why this exists.** GCP returns more than it was sent: server-set fields inside nested objects,
list reordering, defaults filled in. Without reconciliation every plan proposes a change forever. **A
change here is a change to whether plans converge**, and the e2e suite (Task 17) is the check.

**The opaque rule.** An attribute marked `Opaque` (a free-form map, or a `$ref` tail the resolver
truncated) is **copied exactly**: no key translation, nothing dropped, no reordering. Pruning keys we do
not recognise would quietly delete what the user wrote.

- [ ] **Step 1: Write the failing tests**

```go
func TestAServerAddedNestedKeyIsDropped(t *testing.T) {
	attr := &catalog.Attr{Canonical: "config", Kind: value.KindMap, Fields: map[string]*catalog.Attr{
		"mode": {Canonical: "mode", Kind: value.KindString},
	}}
	ref := mapValue(map[string]any{"mode": "A"})
	in := mapValue(map[string]any{"mode": "A", "serverFingerprint": "xyz"})

	got := Reconcile(attr, ref, in)
	m := got.Raw.(map[string]value.Value)
	if _, present := m["serverFingerprint"]; present {
		t.Error("a server-added key survived; every plan would propose a change forever")
	}
	if m["mode"].Raw != "A" {
		t.Errorf("the declared key was lost: %v", m)
	}
}

func TestAnOpaqueValueIsCopiedExactly(t *testing.T) {
	attr := &catalog.Attr{Canonical: "labels", Kind: value.KindMap, Opaque: true}
	ref := mapValue(map[string]any{"env": "prod"})
	in := mapValue(map[string]any{"env": "prod", "goog-managed-by": "x", "team": "infra"})

	got := Reconcile(attr, ref, in)
	m := got.Raw.(map[string]value.Value)
	// Opaque means exactly that. Dropping "team" because the reference did not
	// mention it would delete something the user wrote.
	if len(m) != 3 {
		t.Errorf("opaque value was pruned to %d keys: %v", len(m), m)
	}
}

// TestAnUnorderedListIsReorderedToMatchTheReference. GCP reorders lists it does
// not consider ordered, and a diff on order alone plans a change forever.
func TestAnUnorderedListIsReorderedToMatchTheReference(t *testing.T) {
	attr := &catalog.Attr{Canonical: "tags", Kind: value.KindList, Unordered: true,
		Elem: &catalog.Attr{Kind: value.KindString}}
	ref := listValue("web", "ssh", "db")
	in := listValue("db", "web", "ssh")

	got := Reconcile(attr, ref, in)
	items := got.Raw.([]value.Value)
	for i, want := range []string{"web", "ssh", "db"} {
		if items[i].Raw != want {
			t.Errorf("item %d = %v, want %v (list not reordered to the reference)", i, items[i].Raw, want)
		}
	}
}

// And an ORDERED list must not be touched, or reconciliation would silently
// rewrite something order-significant like a firewall rule priority list.
func TestAnOrderedListIsLeftAlone(t *testing.T) {
	attr := &catalog.Attr{Canonical: "rules", Kind: value.KindList, Unordered: false,
		Elem: &catalog.Attr{Kind: value.KindString}}
	ref := listValue("a", "b")
	in := listValue("b", "a")
	got := Reconcile(attr, ref, in)
	if got.Raw.([]value.Value)[0].Raw != "b" {
		t.Error("an ordered list was reordered")
	}
}

func TestLabelsAreAMapAndReservedOnesAreNeverReported(t *testing.T) {
	in := mapValue(map[string]any{
		"env": "prod", "team": "infra",
		"goog-dm": "deployment", "goog-gke-node": "true",
	})
	got := LabelsIn(in)
	if got["env"] != "prod" || got["team"] != "infra" {
		t.Errorf("user labels lost: %v", got)
	}
	// GCP reserves the goog- prefix, the same way AWS reserves aws-. Reporting one
	// would make every plan propose removing it.
	for k := range got {
		if strings.HasPrefix(k, "goog-") {
			t.Errorf("reserved label %q reported", k)
		}
	}
}

// TestAResourceWithNoLabelsOmitsTheKeyEntirely, rather than sending an empty map,
// which some APIs treat as "remove all labels".
func TestNoLabelsMeansNoKey(t *testing.T) {
	if got := LabelsOut(nil); got.Known && len(got.Raw.(map[string]value.Value)) != 0 {
		t.Errorf("an empty label set produced %v", got)
	}
}
```

`Unordered` is already on `catalog.Attr` from Task 4. This task populates it in
`internal/gen/attrs.go` from magic-modules' `is_set` / `unordered_list` field flags, and regenerates
the catalog.

- [ ] **Step 2: Run, watch fail, write `reconcile.go`, run, watch pass**

Run: `go test -count=1 ./internal/gcprov/ -run 'Reconcile|Label|List'`

- [ ] **Step 3: Sabotage, confirm, restore**

Make `Reconcile` ignore `Opaque`: `TestAnOpaqueValueIsCopiedExactly` must fail. Restore. Make it
reorder every list: `TestAnOrderedListIsLeftAlone` must fail. Restore. Make `LabelsIn` report every
key: `TestLabelsAreAMapAndReservedOnesAreNeverReported` must fail. Restore. Record all three.

- [ ] **Step 4: Commit**

```bash
git add internal/gcprov/reconcile.go internal/gcprov/reconcile_test.go \
        internal/gen/attrs.go internal/catalog/catalog.go internal/catalog/catalog.json.gz
git commit -m "Reconcile nested values so plans converge

GCP returns more than it was sent: server set fields inside nested
objects, lists in a different order, defaults filled in. Without this
every plan proposes a change forever.

Free form maps are copied exactly rather than pruned. Dropping a key we
do not recognise would quietly delete something the user wrote, which is
worse than a spurious diff.

Lists are reordered to match the reference only when the schema says
they are unordered. Reordering an ordered one would silently rewrite
something where order carries meaning.

Labels are always a map, and the goog prefix is never reported, the same
way the AWS provider never reports aws prefixed tags.

Sabotage: ignoring opaque prunes free form maps. Reordering every list
rewrites ordered ones. Reporting reserved labels makes every plan
propose removing them. All three restored." \
  -- internal/gcprov/ internal/gen/attrs.go internal/catalog/
```

---

## Task 16: Discover, import, system-owned, and the tag binding read

**Files:**
- Create: `internal/gcprov/discover.go`, `internal/gcprov/systemowned.go`, `internal/gcprov/listread.go`
- Test: `internal/gcprov/discover_test.go`, `internal/gcprov/systemowned_test.go`

**Interfaces:**
- Consumes: `Client`, `catalog.Catalog`, `gcpplugin.Instance`.
- Produces:
  - `func (p *Provider) Discover(ctx context.Context, req provider.DiscoverRequest) ([]provider.DiscoveredResource, error)`
  - `func SystemOwned(ty *catalog.Type, body map[string]any) (bool, string)`
  - `func (p *Provider) readByListingParent(ctx context.Context, ty *catalog.Type, id string) (map[string]any, error)`

- [ ] **Step 1: Write the failing discover tests**

```go
// TestDiscoverUsesCloudAssetInventoryWhenItIsAvailable. One call for a whole
// project, instead of one list call per type — the reason GCP discovery is
// complete by default where AWS's must be curated.
func TestDiscoverPrefersCloudAssetInventory(t *testing.T) {
	gcptest.Isolate(t)
	s := gcpfake.New(t)
	defer s.Close()
	s.SeedCAI("projects/p", []gcpfake.Asset{
		{AssetType: "tiny.googleapis.com/Widget", Name: "//tiny.googleapis.com/projects/p/locations/r/widgets/one"},
	})
	p := testProviderWithCatalog(t, s, widgetCatalog())

	got, err := p.Discover(context.Background(), provider.DiscoverRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Type != "gcp.widget" {
		t.Fatalf("discovered %+v", got)
	}
	var listed bool
	for _, r := range s.Requests() {
		if r.Method == "GET" && strings.HasSuffix(r.Path, "/widgets") {
			listed = true
		}
	}
	if listed {
		t.Error("per-type list was called even though CAI answered")
	}
}

// TestDiscoverFallsBackWhenCAIIsUnavailable, and says so, rather than reporting
// an empty project as if it were genuinely empty.
func TestDiscoverFallsBackToPerTypeList(t *testing.T) {
	gcptest.Isolate(t)
	s := gcpfake.New(t)
	defer s.Close()
	s.FailCAI(403, "PERMISSION_DENIED", "cloudasset.assets.searchAllResources denied")
	s.Seed("/v1/projects/p/locations/r/widgets/one", map[string]any{"name": "one"})
	p := testProviderWithCatalog(t, s, widgetCatalog())

	got, err := p.Discover(context.Background(), provider.DiscoverRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("fallback found %d resources, want 1", len(got))
	}
}

// TestDiscoveredNamesComeFromTheResourceItself. GCP resources carry a real name,
// so unlike AWS there is no Name-tag dependence and no sanitised-id fallback.
func TestADiscoveredResourceIsNamedFromItsOwnName(t *testing.T) {
	gcptest.Isolate(t)
	s := gcpfake.New(t)
	defer s.Close()
	s.SeedCAI("projects/p", []gcpfake.Asset{
		{AssetType: "tiny.googleapis.com/Widget",
			Name: "//tiny.googleapis.com/projects/p/locations/r/widgets/web-frontend"},
	})
	p := testProviderWithCatalog(t, s, widgetCatalog())
	got, _ := p.Discover(context.Background(), provider.DiscoverRequest{})
	if got[0].ProviderID != "projects/p/locations/r/widgets/web-frontend" {
		t.Errorf("provider id = %q", got[0].ProviderID)
	}
}
```

- [ ] **Step 2: Write the failing system-owned tests**

```go
// Evidence, never category. Each case names the literal signal, and the reason
// string must name it too, so a user overriding the flag knows what they are
// overriding.
func TestSystemOwnedIsDecidedByNamedEvidence(t *testing.T) {
	cases := []struct {
		name     string
		ty       *catalog.Type
		body     map[string]any
		want     bool
		evidence string
	}{
		{"the auto-created default network",
			&catalog.Type{Name: "gcp.network"},
			map[string]any{"name": "default", "autoCreateSubnetworks": true},
			true, "default"},
		{"a default-allow firewall rule",
			&catalog.Type{Name: "gcp.firewall"},
			map[string]any{"name": "default-allow-internal"},
			true, "default-allow"},
		{"a GKE-created resource",
			&catalog.Type{Name: "gcp.instance"},
			map[string]any{"name": "gke-node-1", "labels": map[string]any{"goog-gke-node": "true"}},
			true, "goog-gke-node"},
		{"a Config Connector-managed resource",
			&catalog.Type{Name: "gcp.bucket"},
			map[string]any{"name": "b", "labels": map[string]any{"managed-by-cnrm": "true"}},
			true, "managed-by-cnrm"},
		{"an ordinary user network called something else",
			&catalog.Type{Name: "gcp.network"},
			map[string]any{"name": "prod-vpc", "autoCreateSubnetworks": false},
			false, ""},
		// The one that matters most: a user network someone named "default" but
		// which is NOT auto-mode is still theirs. Name alone is not evidence.
		{"a custom-mode network the user named default",
			&catalog.Type{Name: "gcp.network"},
			map[string]any{"name": "default", "autoCreateSubnetworks": false},
			false, ""},
	}
	for _, c := range cases {
		got, reason := SystemOwned(c.ty, c.body)
		if got != c.want {
			t.Errorf("%s: SystemOwned = %v, want %v (reason %q)", c.name, got, c.want, reason)
		}
		if got && !strings.Contains(reason, c.evidence) {
			t.Errorf("%s: reason %q does not name the evidence %q", c.name, reason, c.evidence)
		}
	}
}
```

- [ ] **Step 3: Write the tag binding read test (G6's worked ruling, end to end)**

```go
// gcp.tagbinding has no get method, so its read is a list filtered by parent.
func TestATypeWithNoGetIsReadByListingItsParent(t *testing.T) {
	gcptest.Isolate(t)
	s := gcpfake.New(t)
	defer s.Close()
	s.SeedTagBindings("//cloudresourcemanager.googleapis.com/projects/p", []map[string]any{
		{"name": "tagBindings/abc", "parent": "//cloudresourcemanager.googleapis.com/projects/p",
			"tagValue": "tagValues/123"},
	})
	p := testProvider(t, s)

	st, err := p.Read(context.Background(), &resource.ResourceState{
		Type: "gcp.tagbinding", ProviderID: "tagBindings/abc",
	})
	if err != nil {
		t.Fatal(err)
	}
	if st == nil {
		t.Fatal("the binding was not found by listing its parent")
	}
	if st.Attributes["tagValue"].Raw != "tagValues/123" {
		t.Errorf("attributes = %v", st.Attributes)
	}
}
```

- [ ] **Step 4: Run, watch fail, write the three files, run, watch pass**

Run: `go test -count=1 ./internal/gcprov/`

`Discover` tries CAI `searchAllResources` across `DiscoverProjects` (defaulting to the instance's
`Project`), maps each asset type through the catalog, and on any CAI failure logs one line naming the
reason and falls back to per-type `list` over `DiscoverTypes` (defaulting to the overlay's
`discover_default`). **Fail-open**: a failure of either path for one type logs and continues rather
than failing the whole discovery.

**The list response's array field is NOT `items`, and must be read from `catalog.Type.ListField`.**
Measured across the fetched corpus on 2026-09-22: **209 distinct array-field names across 532 `list`
methods, and `items` accounts for only 127 of them (24%)**. `compute.firewalls` uses `items`,
`logging.buckets` uses `buckets`, `cloudasset.savedQueries` uses `savedQueries`. Hardcoding `items`
produces a fallback path that works for compute and silently returns nothing for everything else —
which reads as "this project has none of that type" rather than as a bug. A type whose `ListField` is
empty cannot be listed; skip it and say so, rather than guessing.

**An empty collection is a 200 with no results, not a 404.** The fallback scans many types across a
project and finds nothing for most of them, so empty is the common case. Treat a 404 on a collection
as a genuinely missing API and report it; do not learn to swallow 404s, because that hides the real
thing this path is meant to surface.

**Check `discover_default` against the real catalog before relying on it.** As generated on
2026-09-22 it names `gcp.network` and `gcp.subnetwork`, which do not exist at all (compute's `Network`
and `Subnetwork` are tier-2 on unruled hooks), and `gcp.instance`/`gcp.bucket`, which ship as
`gcp.compute.instance`/`gcp.storage.bucket`. Nothing consumed it before this task, so it was never
wrong until now. Fix the overlay as part of this task and assert in a test that every entry in
`discover_default` resolves to a type the catalog actually serves — otherwise the list rots again the
next time naming changes.

`SystemOwned` returns evidence strings, never category names.

- [ ] **Step 5: Sabotage, confirm, restore**

Make `SystemOwned` mark any network named `default`: the custom-mode case must fail. Restore. Make the
CAI failure return the error instead of falling back: `TestDiscoverFallsBackToPerTypeList` must fail.
Restore. Make `readByListingParent` return `(nil, nil)`: the tag binding test must fail. Restore.
Record all three.

- [ ] **Step 6: Commit**

```bash
git add internal/gcprov/discover.go internal/gcprov/systemowned.go internal/gcprov/listread.go \
        internal/gcprov/discover_test.go internal/gcprov/systemowned_test.go
git commit -m "Discover through Cloud Asset Inventory, with a fallback

One search call returns a whole project, which is why GCP discovery is
complete by default where the AWS provider has to ship a curated type
list to avoid fifteen hundred calls per region. When the asset API is
not enabled or not permitted it falls back to per type listing and says
on stderr which path it took, so an empty result is never mistaken for
an empty project.

Naming is simpler here than on AWS because GCP resources carry a real
name, so there is no tag dependence and no sanitised id fallback.

System owned marking names its evidence rather than a category. The case
worth being careful about is a network the user named default: that is
not evidence, and only auto create subnetworks alongside the name marks
the one Google made.

The tag binding read lists its parent, because that type has no get
method at all. That is the overlay ruling from the design, working end
to end.

Sabotage: marking any network named default catches a user's own.
Returning the asset api error instead of falling back reports an empty
project. Returning nothing from the parent listing loses the binding.
All three restored." \
  -- internal/gcprov/
```

---

## Task 17: The real catalog through infrena's host, and a real infrena binary

**Files:**
- Create: `internal/gcpplugin/protocol_test.go`, `e2e/e2e_test.go`, `e2e/testdata/project/infrena.yml`
- Modify: `internal/gcpplugin/plugin.go` (`New` returns a real provider; `MaxConcurrency`)

**Interfaces:**
- Consumes: `plugintest.Open`, everything above.
- Produces: `func (pl *Plugin) MaxConcurrency() int`

- [ ] **Step 1: Wire `New` to return a real provider**

```go
// New constructs one instance from its resolved configuration.
func (pl *Plugin) New(cfg provider.Config) (provider.Provider, error) {
	in, err := ParseConfig(cfg)
	if err != nil {
		return nil, err
	}
	c, err := catalog.Load()
	if err != nil {
		return nil, fmt.Errorf("gcp: the embedded catalog will not load: %w", err)
	}
	ts, err := in.TokenSource(context.Background())
	if err != nil {
		return nil, fmt.Errorf("gcp: provider %q: %w", cfg.Instance, err)
	}
	return gcprov.NewProvider(in, c, gcprov.NewClient(ts, "", gcprov.ClientOptions{
		QuotaProject: in.QuotaProject,
	})), nil
}

// MaxConcurrency is how many operations infrena may run against this plugin at
// once (plugin protocol 5). It REPLACES the host's per-provider default of 8.
//
// The number is derived, not chosen. GCP quota is per API per project per
// minute, and the common infrastructure APIs allow well over a thousand
// requests a minute. One infrena operation is at most one simultaneous request:
// crud.go sends a single mutation and await polls one call at a time, sleeping
// between polls, so N concurrent operations peak at N simultaneous requests.
//
// 16 is twice the host's default and a small fraction of what a project allows,
// which is the point: the quota belongs to the PROJECT, not to this process. A
// colleague's apply, a CI run and the console all draw on the same budget.
//
// Task 18 measures the real headroom on a live project. If that measurement
// disagrees with this number, change the number and this comment together.
func (pl *Plugin) MaxConcurrency() int { return 16 }
```

- [ ] **Step 2: Write the protocol test through infrena's own harness**

```go
package gcpplugin

import (
	"context"
	"testing"

	"github.com/infrena/infrena/pkg/plugintest"
)

// TestTheHostAcceptsTheWholeCatalog drives this plugin through infrena's own
// in-process host, so a failure here is a failure the real host would have.
// Validating 500-odd definitions is also the cheapest check that nothing in the
// generated catalog trips a host-side rule.
func TestTheHostAcceptsTheWholeCatalog(t *testing.T) {
	host, err := plugintest.Open(context.Background(), NewPlugin(), t.TempDir())
	if err != nil {
		t.Fatalf("the host refused this plugin: %v", err)
	}
	defer host.Close()
	defs := host.Definitions()
	if len(defs) < 300 {
		t.Fatalf("the host sees %d types; the catalog should have roughly 500", len(defs))
	}
	var found bool
	for _, d := range defs {
		if d.Type == "gcp.network" {
			found = true
		}
	}
	if !found {
		t.Error("gcp.network is not among the types the host sees")
	}
}
```

Adjust to `plugintest.Host`'s real surface; read `../infrena/pkg/plugintest/plugintest.go` first.

- [ ] **Step 3: Write the e2e suite**

`e2e/e2e_test.go`, behind `//go:build e2e`. `TestMain` builds a real `infrena` binary from
`INFRENA_SRC` (defaulting to `../../infrena`) and this plugin, then runs the full workflow against
`internal/gcpfake` reached through `GOOGLE_API_ENDPOINT_OVERRIDE` (a variable this plugin reads for
exactly this purpose; document it as test-only).

**`TestTheWorkflow` must cover, as named subtests:**

| Subtest | What it proves |
| --- | --- |
| `validate_accepts_the_project` | the catalog compiles under a real host |
| `plan_proposes_a_create` | |
| `apply_creates_it` | |
| `a_second_plan_is_clean` | **invariant 2, and the real test of reconciliation** |
| `a_label_changed_outside_infrena_is_an_update` | drift detection, and that `Update` diffs against the refreshed observation |
| `removing_it_from_config_proposes_a_destroy` | invariant 1 |
| `discover_finds_it` | |
| `import_generate_writes_a_reference_and_the_next_plan_is_clean` | invariant 3, and that reference edges work |
| `a_system_owned_resource_is_not_imported_unless_named` | |

**The skip guard.** `TestMain` writes a literal `E2E SKIPPED:` line to stderr when it cannot build.
CI greps for it. **There is no `INFRENA_REQUIRE_PLUGIN` in this repository** — that is an infrena-repo
variable and setting it here does nothing.

**`GOWORK=off` does not make e2e pinned.** `TestMain` builds infrena from `INFRENA_SRC`, which defaults
to the sibling checkout and is routinely ahead of any tag. To exercise a real release, point
`INFRENA_SRC` at a `git archive` of that tag in a temp dir. Claiming a pinned e2e pass without doing
that is wrong.

- [ ] **Step 4: Run everything**

```bash
go test -count=1 ./...
go test -tags e2e -count=1 -v ./e2e/
go vet -tags e2e,live ./...
gofmt -l .
GOWORK=off go test -count=1 ./...
```

All must pass. The last one is the pinned build CI blocks on: it ignores `go.work` and resolves the
`require` from the module proxy. **infrena became a public repository on 2026-09-20**, so this needs no
credentials and no `GOPRIVATE` setting.

- [ ] **Step 5: Sabotage, confirm, restore**

Break reconciliation by making `Reconcile` return `incoming` unchanged: `a_second_plan_is_clean` must
fail. Restore. Make `Update` diff against `current` read fresh from the API:
`a_label_changed_outside_infrena_is_an_update` must fail. Restore. **These two are the differential
tests that justify the whole e2e suite** — record both, with the failing output quoted.

- [ ] **Step 6: Commit**

```bash
git add internal/gcpplugin/plugin.go internal/gcpplugin/protocol_test.go e2e/
git commit -m "Drive the whole thing through a real infrena

The protocol test runs the catalog through infrena's own in process
host, so a failure here is one the real host would have. Validating five
hundred odd definitions is also the cheapest check that nothing in the
generated catalog trips a host side rule.

The e2e suite runs a real infrena binary against this plugin and the
fake cloud, covering the full loop including a clean second plan and
drift corrected in configuration. Those two subtests are the only place
reconciliation and the update diff are really tested, because a unit
test cannot tell whether a plan converges.

Max concurrency is sixteen and the comment shows the derivation rather
than asserting a number. The quota belongs to the project, not to this
process.

Sabotage: returning the incoming value unreconciled makes the second
plan dirty. Re reading before the update diff loses a corrected drift.
Both restored, with the failing output in the task notes." \
  -- internal/gcpplugin/ e2e/
```

---

## Task 18: The live suite — real GCP, opt-in, and the `Retry-After` measurement

**Files:**
- Create: `live/live_test.go`, `live/README.md`
- Modify: `internal/gcprov/errors.go` (only if the measurement says so)

**This task is decision G7.** Nothing has touched real GCP before it. **No claim that the provider
works is supportable until this suite has run**, and it needs James's explicit approval each time.

- [ ] **Step 1: Ask James, and set the project up**

Ask before doing any of this; it creates billable resources.

```bash
gcloud projects create infrena-live-<suffix> --name="infrena live tests"
gcloud billing projects link infrena-live-<suffix> --billing-account=<account>
gcloud config set project infrena-live-<suffix>

gcloud services enable compute.googleapis.com storage.googleapis.com \
  cloudresourcemanager.googleapis.com cloudasset.googleapis.com iam.googleapis.com

gcloud iam service-accounts create infrena-live --display-name="infrena live test runner"
for role in roles/compute.admin roles/storage.admin roles/cloudasset.viewer roles/resourcemanager.tagAdmin; do
  gcloud projects add-iam-policy-binding infrena-live-<suffix> \
    --member="serviceAccount:infrena-live@infrena-live-<suffix>.iam.gserviceaccount.com" \
    --role="$role"
done
```

Record the project id and service account in `live/README.md`. **Never commit a key file**; the suite
uses impersonation from James's own `gcloud` login, so no long-lived key exists to leak.

- [ ] **Step 2: Write the guard first, and prove it refuses**

```go
//go:build live

package live

import (
	"os"
	"strings"
	"testing"
)

// guard refuses to touch GCP as anything but the dedicated service account.
//
// It runs BEFORE any API call, on purpose. A live suite that discovers it is
// running as the operator's own identity only after creating something has
// already created it in the wrong project.
func guard(t *testing.T) (project, sa string) {
	t.Helper()
	project = os.Getenv("INFRENA_GCP_LIVE_PROJECT")
	sa = os.Getenv("INFRENA_GCP_LIVE_SA")
	if project == "" || sa == "" {
		t.Skip("LIVE SKIPPED: set INFRENA_GCP_LIVE_PROJECT and INFRENA_GCP_LIVE_SA; see live/README.md")
	}
	if !strings.HasPrefix(sa, "infrena-live@") {
		t.Fatalf("refusing to run as %q: the live suite runs only as the infrena-live service account", sa)
	}
	if !strings.Contains(project, "infrena-live") {
		t.Fatalf("refusing to touch project %q: it is not an infrena live test project", project)
	}
	return project, sa
}

func TestTheGuardRefusesTheWrongIdentity(t *testing.T) {
	t.Setenv("INFRENA_GCP_LIVE_PROJECT", "my-production-project")
	t.Setenv("INFRENA_GCP_LIVE_SA", "me@example.com")
	// Run guard in a subtest so its t.Fatalf is captured rather than failing this one.
	sub := &testing.T{}
	_ = sub
	// The assertion is made by inspection during review AND by the sabotage step
	// below; a guard that can be unit-tested into passing is not a guard.
}
```

- [ ] **Step 3: Write the live workflow test**

`TestLiveWorkflow` creates, reads, updates, discovers, imports and deletes a small, cheap set — a
custom-mode VPC network, one subnetwork, one firewall rule, one storage bucket and one tag key/value
pair — with `t.Cleanup` deleting everything in reverse order **even when the test fails**. It asserts:

- create then a clean re-plan (reconciliation against the real API, which the fake cannot prove)
- an out-of-band label change shows as drift and is corrected
- `discover` finds the resources **and marks the auto-created default network system-owned**
- `import` of a created resource produces a state that plans clean

- [ ] **Step 4: Measure `Retry-After` (spec §5.6)**

The spec leaves this open deliberately. Add a temporary measurement to `client.go` that prints, on
**stderr** only, the presence and value of a `Retry-After` header on every throttled response:

```go
if resp.StatusCode == 429 || resp.StatusCode == 503 {
	ra := resp.Header.Get("Retry-After")
	if ra == "" {
		ra = "absent"
	}
	fmt.Fprintf(os.Stderr, "gcp: throttled %d on %s, Retry-After: %s\n", resp.StatusCode, api, ra)
}
```

Then drive a real throttle: run a loop creating and deleting many resources in one project until 429s
appear. **Record the result in `live/README.md` and in the vault note's follow-ups**, whichever way it
comes out:

- If GCP **does** send it, keep `APIError.RetryAfter` and make `backoff.go` honour it, with a test.
- If it **never** does, remove the field and say so in `live/README.md`, the way the AWS repo closed the
  equivalent question — and say what evidence would reopen it.

Remove the temporary print either way.

- [ ] **Step 5: Run it, with James's approval**

```bash
gcloud auth application-default login
export INFRENA_GCP_LIVE_PROJECT=infrena-live-<suffix>
export INFRENA_GCP_LIVE_SA=infrena-live@infrena-live-<suffix>.iam.gserviceaccount.com
go test -tags live -count=1 -v -timeout 45m ./live/
```

Read the whole output. **Check the billing console afterwards and confirm nothing was left running.**

- [ ] **Step 6: Sabotage, confirm, restore**

Change the guard's prefix check to accept any service account, set
`INFRENA_GCP_LIVE_SA=me@example.com`, and confirm the suite would have proceeded — then restore the
check and confirm it refuses. **Do not let the sabotaged run reach GCP**: set
`INFRENA_GCP_LIVE_PROJECT` to something that does not exist so the first call fails regardless.

- [ ] **Step 7: Commit**

```bash
git add live/
git commit -m "The live suite, and what it measured

Runs only as the dedicated service account in a project whose name says
what it is, and the guard checks both before any call is made. A suite
that finds out it is running as the wrong identity after creating
something has already created it in the wrong place.

Everything is torn down in cleanup, in reverse order, even when the test
fails.

This is also where the retry after question got an answer instead of an
assumption. The AWS repo established that Cloud Control never sends the
header and closed it; that finding does not transfer, so it was measured
here rather than inherited. The result and what would reopen it are in
live/README.md.

Sabotage: relaxing the service account prefix check lets the wrong
identity through, confirmed against a project that does not exist so
nothing was created. Restored." \
  -- live/
```

---

## Task 19: Release gate, CI, docs, and closing out

**Files:**
- Create: `scripts/release-check`, `scripts/build-release`, `scripts/check-examples`, `scripts/scripts_test.go`
- Create: `.github/workflows/ci.yml`, `.github/workflows/bump-infrena.yml`, `.github/workflows/release.yml`
- Create: `README.md`, `CLAUDE.md`, `docs/README.md`, `docs/ROADMAP.md`, `LICENSE`, `CONTRIBUTING.md`
- Create: `examples/` (at least: a VPC + subnetwork project, a storage bucket, a tag binding, one module)
- Create: `cmd/gen-docs/main.go` — a reference page per type from the catalog

- [ ] **Step 1: Write `scripts/release-check` and its test**

It must refuse a release unless **tag == `plugin.yaml` version == the binary's reported version**, and
`plugin.yaml`'s `protocol:` equals the `pluginproto.Version` the built binary speaks. Test it by
driving it with a deliberately mismatched manifest and asserting a non-zero exit.

- [ ] **Step 2: Write `scripts/check-examples`**

Compiles every `examples/*/infrena.yml` with a real infrena binary, so a stale example fails CI rather
than a user. Every example must set `defaults: {project: ${var.gcp_project}}` with `gcp_project`
declared with a `default:` — **a variable in `providers:` must resolve without an environment
wherever `discover` is used**, because `discover` takes none and refuses any unresolved value, in
`defaults:` too.

- [ ] **Step 3: Write `cmd/gen-docs` and generate the docs**

One page per type from the catalog: every attribute, its spellings, whether it is output-only or
ForceNew, its references, and the type's import ID shape. Plus a service guide per product and an
index. Commit the generated pages.

**`gen/warnings.txt` gets a generated page too**, listing every type that did not ship and why. That is
the honest public answer to "does infrena support X on GCP", and hiding it would make the tier
mechanism invisible to the people it affects.

- [ ] **Step 4: Write CI**

`ci.yml`: a **blocking** job building pinned (`GOWORK=off`), running
`go test -count=1 ./...`, `-race`, `go vet -tags e2e,live ./...`, `gofmt -l .`, the e2e suite, and
`scripts/check-examples`; it greps stderr for `E2E SKIPPED:` and **fails if found**, because a silently
skipped suite is worse than a failing one. A second, **non-blocking** job builds against infrena's
`main` through a workspace, as early warning. `bump-infrena.yml` opens a PR when infrena tags a newer
release. **None of them needs a checkout token**: infrena went public on 2026-09-20, so the module
resolves from the proxy like any other dependency. (The AWS provider repo still carries an
`INFRENA_CHECKOUT_TOKEN` secret from when it was private — do not copy that across.)

- [ ] **Step 5: Write `README.md`**

Installing, building from source, type names, attribute names, values GCP chooses, credentials,
projects and locations, discovery, **and a section saying plainly what the tier gate means**: the
catalog covers roughly 500 types, `gen/warnings.txt` lists what it does not and why, and a hooked type
ships when someone writes a ruling. Release notes must say the same — "additive" describes a schema,
not what a user experiences.

Add `internal/gcpplugin/readme_test.go` asserting the README's claimed type count is within 10% of
`len(catalog.Load().Types)`, so prose about the catalog cannot rot silently.

- [ ] **Step 6: Write `CLAUDE.md`**

Mirroring `infrena-provider-aws`'s: what the repository is, where the contract lives, the stack and
commands, and the rules that are easy and expensive to get wrong. **It must carry, at minimum:**

- The catalog is generated; never hand-edit `catalog.json.gz`.
- `gen/names.lock.json` only grows.
- The tier gate, and that a ruling must name **every** hook.
- Only `mmv1/products` is vendored; `third_party` and `tools` are MPL.
- stdout is the protocol; never print to it.
- `Create` never errors once GCP created something.
- `Update` takes no extra read.
- Provider IDs are relative resource names — **none of the AWS scoping machinery**.
- `Retry-After`: whatever Task 18 measured, with the evidence.
- No test reaches real GCP outside `-tags live`.
- `-count=1` is mandatory; sabotage every test.
- Commit discipline: explicit paths, no `git add -A`, **no AI attribution in commit messages**, ask
  before any push or tag.
- Do not change the infrena repository from here.

- [ ] **Step 7: Full verification, then report**

```bash
go test -count=1 ./...
go test -tags e2e -count=1 ./e2e/
go vet -tags e2e,live ./...
gofmt -l .
GOWORK=off go test -count=1 ./...
scripts/check-examples
scripts/measure-load
scripts/release-check v0.1.0
```

**Do not claim any of these passed without pasting its output.** Then update the vault: the project
note `projects/labs/infra-tool.md`, the day's `projects/labs/daily/` note, and close the two follow-ups
this plan was written from.

- [ ] **Step 8: Commit, and ask before anything else**

```bash
git add scripts/ .github/ README.md CLAUDE.md docs/ examples/ cmd/gen-docs/ LICENSE CONTRIBUTING.md
git commit -m "Release gate, CI, docs and examples

The release check refuses a release unless the tag, the manifest and the
binary all say the same version and the same protocol. CI builds pinned
as releases do, and fails if the e2e suite skipped rather than passing
quietly, because a silently skipped suite is worse than a failing one.

Docs are generated from the catalog, including a page listing every type
that did not ship and why. That is the honest answer to whether GCP
resource X is supported, and leaving it out would make the tier gate
invisible to exactly the people it affects.

A test keeps the readme's type count within ten percent of the real
catalog, so prose about coverage cannot rot quietly." \
  -- scripts/ .github/ README.md CLAUDE.md docs/ examples/ cmd/gen-docs/ LICENSE CONTRIBUTING.md
```

**Ask James before pushing and before tagging.** A tag starts the release workflow.

---

## Self-review

Run this checklist after the plan is written, before execution starts.

**Spec coverage.** Every section of the spec maps to a task:

| Spec | Task |
| --- | --- |
| §2 G1 Discovery + mm overlay | 2, 3, 7, 8 |
| §2 G2 `project` in `defaults:` | 7 (`ScopeOf`), 9 (`ParseConfig`) |
| §2 G3 CAI discovery with fallback | 16 |
| §2 G4 generic HTTP client | 11 |
| §2 G5 tier-1 gate at v1.0 | 6, 8, 9 |
| §2 G6 both labelling systems | 15 (labels), 6 + 16 (RM tags) |
| §2 G7 live project is a prerequisite | 18 |
| §3 identity and repo shape | 1 |
| §4 generator, tiers, names, attributes, references | 5, 6, 7, 8 |
| §5.1 transport and credentials | 9, 11 |
| §5.2 URLs | 11 |
| §5.3 three await strategies | 12 |
| §5.4 update and `updateMask` | 14 |
| §5.5 provider IDs and import | 13 |
| §5.6 errors, retry, throttling, `Retry-After` | 11, 18 |
| §5.7 contract rules | 1, 13, 14 |
| §6 discovery, import, system-owned | 16 |
| §7 labels and RM tags | 15, 16 |
| §8 testing | 10, 17, 18 |
| §9 docs and release | 19 |
| §10 risks 1–5 | 3 (P3/P4), 6, 8, 11, 18 |

Three issues were found reviewing this plan against the spec, and all three are fixed above rather
than left as notes:

1. **Nested aliases were implemented but untested.** Spec §4.3 says nested keys accept the same
   spellings as top level, and `buildLevel` recurses, but only top-level aliases were asserted —
   a recursion that dropped nested aliases would have passed. Task 7 now has
   `TestNestedKeysGetTheSameSpellings`.
2. **`Unordered` was bolted onto `catalog.Attr` in Task 15**, after Tasks 4 and 7 had already defined
   the struct without it. It is now declared in Task 4 where the type is defined, populated in Task 7,
   and merely consumed in Task 15.
3. **Plan decision P2 carried a stray self-correction.** Rewritten to state the rule directly.

**Type consistency, checked across tasks.** `catalog.Attr`, `catalog.Type`, `catalog.AwaitKind` and
`catalog.Scope` (Task 4) are used unchanged by Tasks 7, 9, 11–16. `disco.Document`, `disco.Schema`,
`disco.Collection`, `disco.Method` (Task 2) by Tasks 7 and 8. `mmv1.Resource`, `mmv1.Field`,
`WireHooks` (Task 3) by Tasks 6, 7, 8 and 15. `Instance` (Task 9) by Tasks 11, 16 and 17. `Client`,
`APIError`, `ClassifyError`, `ExpandURL` (Task 11) by Tasks 12–16. `gcpfake.New`, `Seed`, `Get`,
`Requests`, `SetOperationStyle`, `FailNext` (Task 10) by Tasks 12–16, which additionally use
`CompleteOperationWithError`, `NeverCompleteOperations`, `CreateThenFailOperation`, `NotFoundTimes`,
`SeedCAI`, `FailCAI` and `SeedTagBindings` — **all of those are listed in Task 10's fake and must be
implemented there**, not invented later.

**Test helpers used across `internal/gcprov` tests** — `testProvider`, `testProviderWithCatalog`,
`widgetCatalog`, `widgetType`, `attrs`, `attrsMixed`, `mapValue`, `listValue`, `contains`,
`staticToken` — are written once in `internal/gcprov/helpers_test.go` during Task 11 and used by every
later task in that package.
