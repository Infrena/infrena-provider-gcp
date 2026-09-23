# Documentation

- **[Type reference](reference/README.md)** — 235 types across 35 services. Generated from the
  catalog the plugin itself loads by `cmd/gen-docs`, so it describes what the plugin does rather
  than what it intends to. Do not edit those pages by hand; CI checks them against the catalog.
- **[What is not served, and why](reference/not-shipped.md)** — the honest answer to "is GCP
  resource X supported". 380 entries with a reason each, and the hooks named for the ones waiting on
  a ruling.
- **[Roadmap](ROADMAP.md)** — what is deliberately not here yet.
- **[The live suite](../live/README.md)** — how to run the tests that talk to real Google Cloud, and
  the guard that stops them running anywhere else.

Start from the [README](../README.md) if you are looking for how to use the provider.
