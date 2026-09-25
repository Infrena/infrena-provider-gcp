# Fact sources

Every catalog fact the provider acts on was decided from one or more sources, and they do not all
deserve the same trust. magic-modules' facts are Terraform's: its `output: true` on a health check's
`type` was true of Terraform's encoder and false of Google. Every live batch so far has found a fact
like that. The generator records which sources said each fact, so a fact resting on Terraform's word
alone is visible before a user finds it.

"Fact sources", not "provenance": in infrena core, provenance means where a VALUE came from
(explicit, default, variable) in plans and `pkg/value`. This is about the schema.

## Sources

| source | what it is | trust |
|---|---|---|
| `observed` | seen on real Google by a live test, in `gen/overlay.yaml` `observed:` | highest, and enforced: see below |
| `ruling` | a human decision in `gen/overlay.yaml`, its evidence in the note | high |
| `discovery` | Google's API Discovery document, structure or its prose tags | Google's own, but prose-only facts are easy to miss |
| `mm` | magic-modules YAML | Terraform's, not Google's |
| `default` | nothing said; the generator's own choice | a guess |

A source prefixed `!` said the opposite and was overruled: a health check's `type` output is
`!mm+ruling`.

Deferred: Config Connector's direct controllers are Go code, not data, so they are cited in ruling
notes rather than recorded here. If its recorded HTTP responses are ever used as fake fixtures, they
join as `recorded-kcc`, below `observed`: their provenance per file cannot be verified.

## The vocabulary

Closed. An observation can only assert a fact the generator derives.

Type facts, recorded for EVERY type, with `default` where nothing said:
`create_verb`, `create_url`, `create_await`, `update_verb`, `update_mask`, `update_await`,
`delete_url`, `delete_await`, `setters`, `lock_field`, `patch_one_field`, `clear_before_delete`,
`timeout`. A type fact nobody set is exactly the unknown worth seeing: subnetwork's one field per
patch was one.

Attribute facts, at every depth through both `Fields` and `Elem`, recorded where set or where some
source spoke: `output`, `required`, `immutable`, `input_only`, `sensitive`, `equivalence`,
`unordered`, `send_with_update`.

## Files

- `gen/facts.tsv`: `type  path  fact  value  sources`, one line per fact. The generator refuses to
  write a fact that is set with no source: that is a decision site that does not say so.
- `gen/unknowns.txt`: the facts that decide a request or a replace and that nothing has checked, in
  two tiers:
  - `terraform-only`: backed by `mm` or `default` alone. Live probes are chosen from here.
  - `unverified`: type facts Discovery backs that no observation or ruling has checked. Discovery
    says what is legal; the prose says what happens, and they differ (subnetwork patch limits).

Both are generated and committed, like `gen/warnings.txt`. `go run ./cmd/gen-gcp -check` fails when
any committed output is stale; it needs the fetched `schemas/`, so it runs locally, not in CI. Neither goes into the embedded catalog: `Sources` is never
serialised, so the runtime and the plugin binary do not change.

## Observations are assertions

```yaml
observed:
  gcp.artifactregistry.repository:
    - path: remoteRepositoryConfig.disableUpstreamValidation
      fact: send_with_update
      value: "true"
      seen: 2026-09-25 TestLiveArtifactRegistryCredentialsChangeInPlace
```

- The generator refuses an observation of a type that does not ship, of a fact outside the
  vocabulary, of an attribute that does not exist, or with a `seen` that is not
  `YYYY-MM-DD TestLiveName`.
- It refuses a catalog that CONTRADICTS an observation, whatever decided the fact: a ruling or a
  generator change that disagrees with what Google did is stale, and says so loudly rather than
  winning on rank.
- `TestEveryObservationNamesALiveTestThatExists` fails when the named live test is gone: an
  observation nothing can reproduce is a claim, not evidence.
- Regional and global variants are separate types, so each observation covers exactly one scope.
- `setters` is observed one setter at a time: a test sees the setters it exercised.

Only what a live test actually exercised belongs here.
