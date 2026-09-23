# CLAUDE.md

The GCP provider for infrena: an out-of-process plugin serving 235 resource types generated from
Google's API Discovery documents plus a vendored magic-modules overlay.

## Where the contract lives

`../infrena` — `pkg/provider`, `pkg/schema`, `pkg/value`, `pkg/pluginproto`, `pkg/pluginsdk`,
`pkg/plugintest`. Read it there rather than recalling it. **Never change that repository from
here.**

`plugin.yaml` carries three numbers that move together: `protocol:`, the `infrena:` floor, and
go.mod's require. The floor is not "the oldest host that handshakes" — a host whose `Supported` list
lacks our protocol installs us per the manifest and then refuses us at the handshake.
`TestTheFloorAdmitsTheOldestHostThatWorks` is what enforces that; it is meant to fail when you move
the floor.

## Commands

```bash
go test -count=1 ./...                  # -count=1 is mandatory, always
go test -tags e2e -count=1 ./e2e/       # real infrena binary, in-process fake GCP
go test -tags live -count=1 ./live/     # REAL Google Cloud; see live/README.md
go vet -tags e2e,live ./...
gofmt -l .
go run ./cmd/gen-gcp                    # regenerate the catalog (needs scripts/fetch-schemas first)
go run ./cmd/gen-docs -check            # the committed reference matches the catalog
scripts/check-examples
scripts/release-check vX.Y.Z
```

## Rules that are easy and expensive to get wrong

- **The catalog is generated. Never hand-edit `catalog.json.gz`.** Change `gen/overlay.yaml` or the
  generator, then regenerate.
- **`gen/names.lock.json` only grows.** A published type name never changes.
- **The tier gate**: a type whose magic-modules definition declares wire-affecting hooks does not
  ship until a ruling names **every** hook. A ruling naming some is treated as stale, because it is.
  Read the hook templates before ruling — the names do not tell you what they do.
- **Only `mmv1/products/**/*.yaml` is vendored.** `mmv1/third_party` and `tools` are MPL 2.0 and
  must never be copied. `scripts/fetch-schemas` refuses them and a test keeps that refusal honest.
- **stdout is the protocol.** Never print to it. Diagnostics go to stderr.
- **`Create` never returns an error once GCP has created something.** The host drops a failed
  create's result, so a resource that exists becomes one nothing tracks. `await` strips cancellation
  for the same reason, and `createdID`/`stateFrom` fall back rather than erroring when an identity
  cannot be reduced.
- **`Update` takes no extra read.** It diffs against the observation it was handed.
- **Provider IDs are relative resource names** (`projects/p/zones/z/instances/web1`). None of the
  AWS `<region>/<identifier>` machinery applies: GCP names are already hierarchical and unique,
  which is what that machinery exists to fake.
- **Build every URL with `absURL(ty, rel)`**, never `ty.APIBaseURL + rel`. The API version lives in
  `ty.PathPrefix` and no stored template carries one. Operation URLs in `await.go` are the single
  deliberate exception.
- **`catalog.Attr` has two nesting edges, `Fields` and `Elem`.** A walk following only `Fields`
  reaches 8,017 of 19,730 attribute paths — it misses 59%. Every walk follows both, and every count
  you report should say which edges it followed.
- **`Retry-After`: Google never sends it.** Measured on 2026-09-23 across cloudresourcemanager,
  compute and storage: 0 of 34,000+ throttled responses carried the header. Backoff is ours to
  choose; do not write code that waits on it.
- **No test reaches real GCP outside `-tags live`.**
- **Sabotage every test.** Break the code so it still compiles, confirm the test fails with the
  assertion you expect, restore by re-applying — never `git checkout --`. A sabotage that passes is
  a finding: the boundary has no coverage. That has happened repeatedly here.
- A skip reads exactly like a pass. Guard against silent skips rather than trusting a green run.

## Commit discipline

- Stage explicit paths. Never `git add -A`, `git add .`, or `git commit -am`.
- New commits only: no amend, reset, rebase or force-push.
- **No AI, Claude or model attribution in commit messages.** Plain English, no em-dashes, minimal.
- Ask before any push, and before any tag — a tag starts the release workflow.
