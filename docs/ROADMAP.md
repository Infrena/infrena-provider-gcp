# Roadmap

What is deliberately not here yet, with the reason. Everything below is known, not discovered later.

## Coverage

**Most of GCP does not ship yet.** [not-shipped.md](reference/not-shipped.md) lists every type that
does not, with the reason for each, and its summary table gives the current counts.

The largest group is types whose magic-modules definition declares wire-affecting hooks and has no
ruling yet. Those are unblocked one at a time by someone reading the hooks and writing a ruling;
[CONTRIBUTING.md](../CONTRIBUTING.md) describes how, and it is the single most useful contribution
anyone can make here.

**Some types ship but cannot create** ([counted and listed](reference/not-shipped.md#shipped-but-cannot-be-created)). Their create URL names a placeholder nothing can fill. They can
be read, imported and destroyed, and `infrena explain` says so per type rather than promising a
create that would fail on string substitution. The generator binds a parent placeholder from the
API's own published `pattern` where it can; these are the ones where the pattern is too loose to be
decisive.

## Known gaps

**Nested curated aliases.** `gen/overlay.yaml`'s alias format is type → attribute → alias with no
path notation, so a hand-picked short name can only be given to a top-level attribute. infrena
resolves nested aliases from 0.14.1 and element aliases from 0.15.0, so the host side is ready and
the overlay format is the limit.

**`requestBody` output filtering at depth is fixed; the host's is not.** infrena refuses a computed
attribute only in its top-level loop, so a nested computed field written in configuration reaches
the provider rather than being rejected. This plugin drops them before they reach Google, but the
diagnostic a user gets is nothing at all rather than "you cannot set this".

**Discovery is project-scoped.** Folder and organisation scopes are not implemented. Types whose
Cloud Asset Inventory name does not follow their `self_link` shape (`gcp.storage.bucket`) are found
by the fallback path rather than the asset path.

**`gcp.bigquery.table`'s `base_url` is an item path**, so its create posts to an item URL. It works,
but it is the one type whose create collection check had to be narrowed to accommodate it.

## Not planned

**Per-API OAuth scopes.** Every type uses `cloud-platform`. A narrower per-API list would have to be
kept in sync with the catalog by hand, and would drift.

**A second source of lifecycle truth.** Discovery is authoritative for shape and magic-modules for
lifecycle. Adding a third would mean resolving three-way disagreements with no tiebreaker.

## How this list stays honest

This page states no counts; they live on generated pages. [not-shipped.md](reference/not-shipped.md) is written by
`cmd/gen-docs` from `gen/warnings.txt`, CI fails if the committed pages disagree with the catalog,
and a test fails if the README's coverage claim drifts more than 10% from the real number.

Prose about a generated artefact rots. Two claims in this repository went stale within hours of
being written — a "roughly 500 types" estimate made before anything had been generated, and a
measured element count in `plugin.yaml`. Both are gone. Anything here that can be generated, is.
