# Contributing

## The most useful contribution: a ruling

[Many more types are held back than ship](docs/reference/not-shipped.md), and that page counts them. Most of the gap is types whose
magic-modules definition declares hand-written Terraform hooks that change what goes on the wire,
and which therefore wait for a human to read those hooks and decide.

That decision is a ruling in `gen/overlay.yaml`, and writing one is a small pull request:

1. Find the type on [not-shipped.md](docs/reference/not-shipped.md). Its row names the hooks.
2. **Read the hook templates**, at the commit in `gen/mmv1.lock`. Their names do not tell you what
   they do — of the five that held `gcp.network` and `gcp.subnetwork` back, every one turned out to
   be the Terraform provider's own bookkeeping, gated on convenience fields this provider does not
   offer.
3. Write the ruling: name **every** hook, and say what each does. A ruling naming some is treated as
   stale, because it is.
4. **State the bounded gap.** If the hook does something this provider will not, say so plainly in
   the ruling rather than implying there is no difference. The existing rulings show the shape.
5. Regenerate (`go run ./cmd/gen-gcp`), and verify. If you can run the live suite against your own
   project, do — a wrong ruling is worse than no ruling, because it ships a type that looks
   supported and misbehaves.

## Tests

`-count=1` always. Sabotage anything you add: break the code so it still compiles, confirm your test
fails with the assertion you expect, restore by re-applying. If the sabotage passes, you have found
something — the boundary was never covered.

Assert on what the code does, not on what a fixture makes convenient. Several bugs here survived a
full suite because the in-process fake echoed back whatever it was sent, so the request and the
response could never disagree.

## What not to do

- Do not hand-edit `internal/catalog/catalog.json.gz`. It is generated.
- Do not vendor anything from magic-modules outside `mmv1/products/**/*.yaml`; the rest is MPL 2.0.
- Do not print to stdout. It is the plugin protocol.
- Do not put AI or model attribution in commit messages.

## Running things

See the Development section of the [README](README.md). The e2e and live suites want a checkout of
infrena beside this one, or `INFRENA_SRC` pointing at one.
