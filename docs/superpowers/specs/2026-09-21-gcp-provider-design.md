# The GCP provider for infrena — design

**Date:** 2026-09-21
**Status:** approved design, not yet implemented
**Repository:** `infrena-provider-gcp`, module `github.com/infrena/infrena-provider-gcp`
**Project note:** `Obsidian Vault/projects/labs/infra-tool.md`

## 1. What this is, and what it is not

One generic provider plugin serving every GCP resource type the generated catalog covers,
distributed as the binary `infrena-plugin-gcp`. It is the second provider to touch a real cloud,
after `infrena-provider-aws`, and it deliberately mirrors that repository's shape so the two read
alike.

It is **not** a port of the AWS provider. The single most important fact about this design is that
**GCP has no Cloud Control API**. There is no generic CRUD endpoint spanning resource types. The
"one provider, many types" shape survives, but it is achieved by driving each service's own REST
API from catalog metadata rather than by calling one uniform API. Every difference below follows
from that.

### Measured, on 2026-09-21

These numbers are the evidence the design rests on. Re-measure before trusting a claim that
contradicts one.

| Measurement | Value |
| --- | --- |
| Google API Discovery directory | 530 entries, **314 preferred APIs** |
| Collections in a 25-API infra sample | 395, of which **221 (56%)** are `get`+create+`delete` shaped |
| Properties in that sample | 14,124 |
| …carrying the structured `readOnly` flag | **3,095** |
| …carrying an `Immutable.` marker | **23** |
| magic-modules `mmv1/products` | **942 resource files, 190 products** |
| …with field-level `immutable: true` | 860 files (91%), **3,185 fields** |
| …with `output: true` / `required: true` | 824 (87%) / 917 (97%) |
| …with `base_url` / `import_format` / `async` | 935 (99%) / 726 (77%) / 542 (58%) |
| …with a **wire-affecting** `custom_code` hook | **423 (45%)** |
| `type: ResourceRef` fields | **314 across 153 resources** |
| Mutating methods in the sample: compute-style LRO / longrunning LRO / synchronous / no body | 300 / 189 / 187 / 23 |

The decisive one is the pair in the middle: **Discovery documents carry `readOnly` but essentially
no immutability information.** CloudFormation schemas give the AWS provider `createOnlyProperties`
for free; nothing Google publishes in Discovery does. Without another source, every `ForceNew` in a
GCP catalog would be a guess.

## 2. Decisions

Each is numbered so later work can cite it. G1–G7 were taken with James on 2026-09-21.

**G1. The catalog is generated from Discovery documents, with magic-modules as a lifecycle
overlay.** Google's Discovery documents are the authoritative schema source: they are published by
Google, always current, and cover every API. `GoogleCloudPlatform/magic-modules`'s
`mmv1/products/*/*.yaml` supplies the three things Discovery lacks — field immutability,
required-ness, and the REST/LRO/import mechanics. Those files are **Apache 2.0** by per-file header;
only `mmv1/third_party/terraform/*` and `tools/go-changelog/*` are MPL 2.0, and neither is used.

*Rejected:* magic-modules alone (the catalog would inherit Terraform's resource modelling and field
flattening wholesale); Discovery alone plus a hand overlay (hand-curating immutability across ~900
types is not shippable); Config Connector CRDs (k8s-shaped, so every resource would have to be
mapped back to its REST API by hand).

**G2. A provider instance carries `project` in `defaults:`, and any resource may override it.**
Exactly how the AWS provider treats `region`. Every type gets `project` as a `Required`+`ForceNew`
attribute, defaulted from the instance. `location` (or `region`/`zone`, per what the type's URL
template names) is a second attribute of the same kind. One instance may therefore span projects;
credentials are shared across them.

**G3. `discover` uses Cloud Asset Inventory, falling back to per-type `list`.**
`searchAllResources` returns a whole project — or an org — in a handful of paginated calls. Where
CAI is not enabled, not permitted, or does not index a type, the provider falls back to
catalog-driven per-type `list` calls. Which path ran is reported on stderr, never silently chosen.

This is the one place GCP is straightforwardly better than AWS, and the advantage should not be
thrown away: the AWS provider needs a *curated default type set* because an unfiltered scan is
roughly 1,500 `ListResources` calls per region per run. GCP has no such problem, so GCP discovery
is complete by default rather than curated.

**G4. One generic JSON-over-HTTP client, driven by catalog metadata.** Dependencies are `net/http`
and `golang.org/x/oauth2/google`. No per-service SDKs.

*Rejected:* `google.golang.org/api` generated clients and `cloud.google.com/go` idiomatic clients.
The AWS repository records that adding **one** AWS SDK service took cold build from 8.7 s to 28.9 s,
with a standing instruction to measure before adding another; GCP would need ~100 such packages.
The idiomatic clients are heavier still, gRPC-based, and their resource shapes diverge from the
Discovery schema the catalog is generated from.

**G5. v1.0 ships tier-1 types, plus only those tier-2 types carrying an explicit written
ruling.** See §4. Roughly 500 reviewed types rather than ~900 unreviewed ones. "Mature and ready to
use" has to mean every type in the catalog is one the generator, or a recorded human decision, can
vouch for. At v1.0 the ruled exceptions are exactly the short list G6 forces; everything else
tier-2 waits.

**G6. Both labelling systems ship at v1.0.** Native `labels` inline on each resource, *and*
Resource Manager tags (`gcp.tagkey`, `gcp.tagvalue`, `gcp.tagbinding`) as their own catalog types.

**G7. A live suite against real GCP is a prerequisite, not a follow-up.** James does not yet have a
GCP project for this. Creating the project, the dedicated service account and the enabled APIs is an
explicit numbered step in the implementation plan, ahead of any claim that the provider works.

## 3. Identity and repository shape

| | |
| --- | --- |
| Module | `github.com/infrena/infrena-provider-gcp` |
| Binary | `infrena-plugin-gcp`, from `cmd/infrena-plugin-gcp` |
| Plugin name | `gcp` — so `infrena plugins install gcp` |
| Go | 1.27.0, matching infrena's own `go.mod` floor |

Packages mirror the AWS repository deliberately:

| Package | Role | AWS counterpart |
| --- | --- | --- |
| `internal/disco` | Discovery-document parsing | `internal/cfn` |
| `internal/gen` | Catalog generation, tiering, naming | `internal/gen` |
| `internal/catalog` | The embedded `catalog.json.gz` | `internal/catalog` |
| `internal/gcprov` | The one runtime provider | `internal/ccprov` |
| `internal/gcpfake` | Wire-level fake GCP | `internal/ccfake` |
| `internal/gcptest` | Credential isolation for tests | `internal/awstest` |
| `cmd/gen-gcp` | The build-time generator | `cmd/gen-cloudcontrol` |

`gen/` holds `overlay.yaml` (hand-curated), `names.lock.json` (append-only), `warnings.txt`
(generated), and the pinned magic-modules vendor. `go.mod` requires a real infrena tag with **no
`replace`**; a gitignored `go.work` serves local work against a sibling checkout. `plugin.yaml`'s
`infrena:` floor is the oldest host that accepts the binary and is deliberately a different number
from the require — the AWS repository's rule, and its reasoning, apply unchanged.

## 4. The generator and the catalog

Three inputs, one committed artifact (`internal/catalog/catalog.json.gz`), regenerated and reviewed
as a diff like any other change. **Never hand-edited.**

1. **Discovery documents** — schema: property types, nesting, enums, descriptions, and the
   structured `readOnly` flag.
2. **magic-modules `mmv1/products`**, vendored at a pinned commit — lifecycle: `immutable`,
   `required`, `base_url`, `create_url`, `update_url`, `delete_url`, `update_verb`, `update_mask`,
   `async`, `import_format`, `self_link`, `ResourceRef`.
3. **`gen/overlay.yaml`** — human rulings: friendly aliases, tier-2 decisions, and any correction
   the two machine sources get wrong.

Read-only detection **unions both signals**. Compute is the reason: it carries 1,520 `readOnly`
flags but 2,202 `[Output Only]` prose markers, so neither source alone is sufficient.

### 4.1 Fidelity tiers

Every candidate type is assigned a tier, recorded in the catalog.

- **Tier 1 — generic-safe.** Discovery and the overlay agree; standard CRUD; an await strategy the
  runtime implements; no wire-affecting `custom_code` hook. Ships enabled.
- **Tier 2 — hooked.** magic-modules defines an `encoder`, `decoder`, `custom_import`,
  `custom_create`, `custom_update`, `custom_delete`, `update_encoder`, or a `pre_*`/`post_*` mutation
  hook. Those are hand-written Go that fixes up the request or response for resources whose plain
  REST shape is not enough; a generator that ignored them would emit types that look correct and
  misbehave against real GCP. **The generator refuses to ship a tier-2 type until
  `gen/overlay.yaml` records a ruling naming the specific hook** — either "inspected, irrelevant to
  a REST-shaped provider" or an explicit correction. Per G5, a tier-2 type ships **only** where such
  a ruling exists. At v1.0 that is the short list G6 forces — see the worked example below — and the
  remainder land release by release as rulings are written.
- **Tier 3 — excluded.** No create method, no stable identity, `exclude: true`, or beta-only. Named
  with a reason in `gen/warnings.txt`. Never silently dropped.

**Worked example of a tier-2 ruling.** `cloudresourcemanager.tagBindings` has `create`, `delete` and
`list` — **no `get` and no `patch`**. It therefore needs a read implemented as list-by-parent, and is
wholly `ForceNew`. That falls outside the generic tier-1 CRUD shape, so `gcp.tagbinding` ships only
because `gen/overlay.yaml` states the read path and the immutability explicitly. `gcp.tagkey` and
`gcp.tagvalue` are ordinary tier-1 types. (G6 requires all three at v1.0, so this ruling is written
during initial implementation rather than deferred.)

### 4.2 Names

`gcp.<resource>` when the resource segment is unique across the catalog, otherwise
`gcp.<service>.<resource>` — `gcp.subnetwork`, `gcp.compute.instance`. A name never moves once
released: `gen/names.lock.json` only grows, and a later GCP type that would clash gets the qualified
form while the short name keeps its original owner.

### 4.3 Attributes

Every attribute accepts GCP's own property name in any case, its generated snake_case form, and a
curated friendly alias where the overlay defines one. Plans, `infrena explain` and
`import --generate` show the friendly alias when one exists, otherwise snake_case. A property whose
name would collide with an infrena resource keyword shows as `type_value`, `provider_value` or
`lifecycle_value`. Nested keys accept GCP's own spelling and the generated snake_case form, the same as
top level; a CURATED alias is top-level only, because the overlay names an attribute as
`aliases[<type>][<attribute>]` and has no notation for one at depth. `labels:` is always a map.

Every settable property is `Optional`+`Computed`, per PLAN §14.1: an attribute left unset keeps
whatever GCP assigns, and removing one from configuration after GCP has set it keeps GCP's current
value. infrena cannot tell "never set" from "no longer configured", so both are treated the same.

### 4.4 References

Derived from magic-modules' 314 declared `ResourceRef` fields, each naming its target `resource:`.
**Read, never inferred** — the same rule the AWS catalog follows. This is what makes
`network: ${my-network}` work and what lets `import --generate` write a reference instead of a
literal when both resources are imported in the same run.

## 5. The runtime provider

### 5.1 Transport and credentials

`net/http` plus `golang.org/x/oauth2/google` for Application Default Credentials. Instance
configuration mirrors the AWS provider's `profile`/`assume_role_arn`: `credentials_file`,
`impersonate_service_account`, `quota_project`. A per-(credential, project) client cache — kept for
the same reason the AWS repository keeps its per-region one, that it is what gives the rate limiter
a life longer than one request. **Do not "simplify" the cache away.**

### 5.2 URLs

Built from the catalog's `base_url`/`create_url`/`update_url`/`delete_url` templates, substituting
`{{project}}`, `{{region}}`, `{{zone}}` and `{{name}}` from resolved attributes.

### 5.3 Await

Exactly three strategies, chosen per type at generation time. The sample showed no long tail.

- **Synchronous** (187 methods) — the mutation returns the resource; no await.
- **Compute-style** (300) — poll `globalOperations`/`regionOperations`/`zoneOperations` `.wait`,
  which long-polls to a 2-minute deadline instead of burning requests, until `status: DONE`; report
  `error.errors[]`.
- **Longrunning** (189) — poll the operation by name with backoff, using `.wait` where the API offers
  one (`run` does; `redis` and `iam` do not), until `done`; then read `error` or `response`.

Once a mutation is sent it is awaited under `context.WithoutCancel`, bounded by the catalog's
timeout. `ctx` is checked **before** sending and between pages of a paginated read or list.
Cancellation is not a licence to abandon a mutation in flight.

### 5.4 Update

`PATCH` with an `updateMask` naming exactly the changed fields. This gives AWS's "patch only adds or
replaces, never removes" rule for free: an attribute dropped from configuration contributes no mask
entry, so GCP keeps its current value — enforced by the API rather than by our patch builder.

**The patch is computed straight from the `current` the host hands `Update`, with no extra read.**
infrena ≥ 0.7.1 passes `Update` and `Delete` the refreshed observation from immediately before
planning. The AWS repository once worked around the older behaviour with an extra read; that
workaround is gone there and must not be introduced here.

### 5.5 Provider IDs and import

**The provider ID is the relative resource name itself** —
`projects/p/zones/us-central1-a/instances/web1`. Import takes the same string.

No synthesized scope prefix, no `|`-joined composite identifiers, no split-at-the-first-slash rule:
GCP resource names are already globally unique, stable, and hierarchical. The AWS provider's
`<region>/<identifier>` and `global/<identifier>` machinery exists to compensate for Cloud Control
identifiers that carry none of that, and simply is not needed here.

### 5.6 Errors, retry and throttling

`ClassifyError` is a pure function of the response: HTTP status plus the `error.status` enum.

| Classification | Signals |
| --- | --- |
| Safe to retry | `RESOURCE_EXHAUSTED`, `UNAVAILABLE`, `ABORTED`; HTTP 429, 500, 503 |
| Not safe to retry | `INVALID_ARGUMENT`, `PERMISSION_DENIED`, `FAILED_PRECONDITION`, `ALREADY_EXISTS` |
| Not safe to retry | anything unrecognised |

Because the client is ours rather than an SDK's, backoff is ours to implement: exponential with
jitter, per-(project, API) rate limiting, since GCP quota is per-API per-project per-minute.

**`Retry-After` is an open question here, and the AWS answer does not transfer.** The AWS repository
established with measurement that Cloud Control never sends `X-Amz-Retry-After` and closed the
question. GCP does sometimes send `Retry-After`. Measure it on the first live run and record the
answer the same way; do not assume either direction.

### 5.7 Rules carried over unchanged from the plugin contract

- **stdout is the protocol. Never print to it.** Log to stderr. A direct write to fd 1 escapes the
  SDK's redirect.
- **Never return `(nil, nil)` from `Create` or `Update`.** And never return an error from `Create`
  once GCP has created something — the host drops a failed create's result, so the resource would
  exist untracked. Report the truthful state and let the next plan converge it.
- **Do not reimplement what the host enforces**: sensitivity, provenance, bookkeeping carry-forward,
  undeclared-attribute rejection. Return only `Type`, `ProviderID` and `Attributes`.
- **Never log a credential, a bearer token, or a `Sensitive` value.** stderr reaches CI logs and its
  tail is quoted in crash errors.
- **A `NotFound` on read is not proof of absence** for a resource created seconds ago. `Read` retries
  a bounded number of times before reporting `(nil, nil)`.

## 6. Discovery, import and system-owned

CAI `searchAllResources` with an `assetTypes` filter, falling back to per-type `list`. The generator
emits the asset-type mapping (`compute.googleapis.com/Instance` → `gcp.instance`) into the catalog.

**Naming discovered resources is simpler than on AWS.** GCP resources carry a real `name`, so there
is no dependence on a `Name` tag and no fallback to a sanitised provider ID: `instance-web1` comes
straight from the resource, prefixed with the type's last segment.

**System-owned marking is by evidence, never by category** — the AWS rule, and the thing that bit
James on 2026-09-14. Marked: the auto-created `default` network and its auto-mode subnetworks;
`default-allow-*` firewall rules; the default and Google-managed service accounts; and resources
carrying `goog-dm`, `goog-gke-*` or `managed-by-cnrm` labels. No CIDR or naming heuristics beyond
those literal, documented signals.

Reason strings name the evidence, not the category. **Fail-open:** a CAI or list failure leaves the
flag unset and logs, rather than failing discovery — an unset enrichment degrades to older
behaviour, whereas a discovery that fails on an optional call is worse than one that never had the
feature.

`import` discovers first, so the same rules apply there. A marked resource is left out unless named
explicitly.

## 7. Labels and Resource Manager tags

Two distinct systems, both shipped at v1.0 per G6.

- **`labels`** — a native map on most resources, settable inline. The direct analogue of AWS `tags:`,
  and always presented as a map.
- **Resource Manager tags** — `gcp.tagkey` and `gcp.tagvalue` (ordinary tier-1 CRUD types) and
  `gcp.tagbinding` (the tier-2 ruling in §4.1). These are what IAM conditions and org policy bind
  against, so a project using tag-based policy can be fully managed.

Labels GCP reserves for itself (the `goog-` prefixes) are never reported, the same way the AWS
provider never reports `aws:`-prefixed tags.

## 8. Testing

- **`internal/gcpfake`** — an `httptest` server speaking GCP's real JSON REST and **both** operation
  shapes, so every test goes through the real client path: URL templating, serialisation, error
  decoding, await and backoff. **Fake the cloud, not the code.** The real API is the oracle for the
  fake's wire format.
- **`internal/gcptest`** — isolates every test from the developer's machine: no ADC, no metadata
  server, no `GOOGLE_APPLICATION_CREDENTIALS`, endpoints pointed at the fake. **No test reaches real
  GCP outside `-tags live`.**
- **`e2e/`** — a real infrena binary against this binary, the full workflow: create, re-plan clean,
  drift, update, destroy, discover, import, `import --generate` with a reference and a clean plan
  after.
- **`live/`** — `-tags live`, gated on `INFRENA_GCP_LIVE_PROJECT` and `INFRENA_GCP_LIVE_SA`, with a
  guard that refuses to run as anything but the dedicated service account **before making any API
  call**. Needs James's explicit approval each run.

**`-count=1` is mandatory** — a cached pass hides a fixture edit. **A fixture must contradict its
expected output**, or it asserts nothing. **Sabotage every test**: break the code it covers so it
still compiles and behaves wrongly, confirm the test fails, restore it by re-applying the edit (not
`git checkout --`, which can wipe uncommitted work), and record the sabotage in the commit message.

## 9. Docs and release

Parity with the AWS repository: a generated reference page per type, service guides, and example
projects compiled by a real infrena binary in CI. `scripts/` gets `fetch-schemas` (Discovery
documents plus the pinned magic-modules vendor), `check-examples`, `measure-load`, `release-check`
and `build-release`. Releases publish one archive per platform named as `infrena plugins install`
expects, alongside `SHA256SUMS`.

**Commit discipline:** stage explicit paths. `git add -A`, `git add .` and `git commit -am` are
forbidden. Ask James before any `git push` and before creating a tag — a tag starts the release
workflow.

**Do not change the infrena repository from here.** A separate session owns it. Write findings down
with evidence and tell James.

## 10. Risks

1. **magic-modules is a moving third-party input.** Mitigated by vendoring at a pinned commit and
   committing the catalog, so every upstream change arrives as a reviewable diff. Bumping the pin is
   a deliberate, reviewed act.
2. **The 45% with wire-affecting hooks is the real coverage limit.** G5's strict gate makes this
   visible rather than latent, but it does mean v1.0's catalog is roughly half the AWS provider's
   size. The tier-2 backlog is the roadmap.
3. **Two sources can disagree.** Discovery is always current; the vendored overlay is not. The
   generator must fail loudly on a property the overlay marks immutable that Discovery no longer
   has, rather than dropping it.
4. **Quota is per-API per-project per-minute**, a different shape from AWS's per-account throttling,
   and the client is ours. The backoff and limiter are code we own and must test, not an SDK
   behaviour we inherit.
5. **Nothing has touched real GCP yet** (G7). No claim that the provider works is supportable until
   the live suite has run.

## 11. Explicitly out of scope

Unruled tier-2 types, and all tier-3 types, at v1.0 (G5). Beta and alpha API versions. IAM policy management on
individual resources (`setIamPolicy`/`getIamPolicy` are a distinct model from CRUD and deserve their
own design). Org-level resources beyond what CAI discovery reports. GKE workload management — this
provider manages the cluster, not what runs in it.
