# infrena-provider-gcp

The Google Cloud provider for [infrena](https://github.com/Infrena/infrena). It serves **253
resource types** across 35 GCP services, generated from Google's own API Discovery documents with a
[magic-modules](https://github.com/GoogleCloudPlatform/magic-modules) overlay for the lifecycle
facts Discovery does not carry.

- **[Type reference](docs/reference/README.md)** — every type, every attribute, every spelling.
- **[What is not served, and why](docs/reference/not-shipped.md)** — read this first if your type is
  missing. It is generated too.

Requires infrena **0.15.0 or newer** (plugin protocol 6).

## Installing

```bash
infrena plugins install gcp
```

Or build it yourself:

```bash
git clone https://github.com/Infrena/infrena-provider-gcp
cd infrena-provider-gcp
go build ./cmd/infrena-plugin-gcp
```

The binary speaks infrena's plugin protocol on stdin and stdout. Running it by hand prints what it
is and exits.

## A first configuration

```yaml
variables:
  gcp_project:
    default: my-project

providers:
  - plugin: gcp
    project: ${var.gcp_project}
    region: us-central1

resources:
  net:
    type: gcp.network
    name: example-net
    autoCreateSubnetworks: false

  sub:
    type: gcp.subnetwork
    name: example-subnet
    cidr: 10.184.0.0/24
    network: ${net}
```

More in [`examples/`](examples/), all of which are compiled by CI so a stale one fails us rather
than you.

## Type names

`gcp.` plus the resource, lowercased: `gcp.network`, `gcp.firewall`, `gcp.serviceaccount`. Where two
GCP resources would collide, or where a resource exists once per scope or per parent, the name
carries what distinguishes them: `gcp.storage.bucket`, `gcp.compute.instance`, `gcp.autoscaler`
(zonal) against `gcp.regionautoscaler` (regional).

Names never change once published — `gen/names.lock.json` only grows.

## Attribute names

Attributes are GCP's own field names, so the reference page for a type reads like Google's API
documentation, because it is generated from it.

**Every camelCase attribute also answers to snake_case**: write `autoCreateSubnetworks` or
`auto_create_subnetworks`, whichever you prefer. A handful carry a shorter hand-picked name as well
where the mechanical spelling is not what anyone says — `cidr` for `ipCidrRange`, `machine` for
`machineType`, `sources` for a firewall's `sourceRanges`. Each type's page lists every spelling it
accepts.

Aliases are additive and permanent. A published spelling keeps working.

## Values GCP chooses

Most attributes are optional and computed: leave one out and GCP's value stands, rather than the
plugin proposing to clear it. That is how `updateMask` behaves — a field a request omits is left
alone — so **removing an attribute from configuration does not reset it**. To clear something, set
it explicitly to its empty value.

Attributes marked output-only on a type's page are GCP's to set. Configuration may not.

## Credentials, projects and locations

Authentication is Application Default Credentials, the same chain `gcloud` uses. The provider
instance takes:

| key | |
| --- | --- |
| `project` | required; the project resources are created in |
| `region`, `zone` | defaults for types that need one |
| `credentials_file` | a service account key file, if you must use one |
| `impersonate_service_account` | preferred over a key file; no key ever touches disk |
| `quota_project` | bills quota elsewhere |
| `discover_types`, `discover_projects` | narrow what `infrena discover` looks at |

`project`, `region` and `zone` are provider configuration, not resource attributes: almost no type
declares a `project` field, because a project is a path segment rather than something in a request
body.

## Discovery

`infrena discover` asks Cloud Asset Inventory what exists, which answers for a whole project in a
few calls. Where the asset API is unavailable it falls back to listing a short default set of types
per project.

An asset type can map to more than one of ours — a zonal autoscaler and a regional one share
`compute.googleapis.com/Autoscaler` — so discovery resolves which by the resource's own name, and
skips with a warning rather than guessing when it cannot.

## What the tier gate means

**The catalog does not cover all of GCP, and the gap is deliberate.** 253 types ship; 363 entries
are listed in [not-shipped.md](docs/reference/not-shipped.md) with the reason for each.

The generator ships a type only when it can vouch for it. Roughly 45% of magic-modules resources
carry hand-written Terraform hooks that change what goes on the wire — a field renamed, a second
request sent, a value computed before sending. A generated provider that ignored them would produce
types that look correct and misbehave against the real API.

So a type carrying such hooks does not ship until someone reads them and writes a ruling in
`gen/overlay.yaml` naming **every** hook, with the gap that ruling accepts stated plainly. That is
how `gcp.network` and `gcp.subnetwork` arrived: their five hooks turned out to be the Terraform
provider's own bookkeeping, gated on convenience fields this provider does not offer, and the
rulings say exactly that.

If a type you need is missing, its entry on that page names the hooks. A ruling is a small pull
request.

"Additive" in a release note describes a schema. Read the not-shipped page for what you can actually
use.

## Development

```bash
go test -count=1 ./...                      # unit suite
go test -tags e2e -count=1 ./e2e/           # against a real infrena binary and an in-process fake GCP
go vet -tags e2e,live ./...
scripts/check-examples                      # every example still compiles
go run ./cmd/gen-docs -check                # the committed reference matches the catalog
```

The e2e and live suites expect a checkout of infrena beside this one, or `INFRENA_SRC` pointing at
one. `live/` talks to real Google Cloud, is behind `-tags live`, and refuses to run against a
project that is not labelled for it — see [`live/README.md`](live/README.md).

Regenerating the catalog needs `scripts/fetch-schemas` first; it writes to `schemas/`, which is not
committed.

## Licence

Apache 2.0. See [LICENSE](LICENSE) and [NOTICE](NOTICE) — the latter records what is vendored from
magic-modules and the licence boundary the vendoring script enforces.
