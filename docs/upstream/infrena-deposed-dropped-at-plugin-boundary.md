# Deposed records are silently dropped by every out-of-process provider

**Repo:** infrena
**Observed at:** `efbfaaa` (v0.14.0-1-gefbfaaa), branch `main`
**Affects:** every provider that runs as a plugin (all of them today, AWS included). In-process
providers are unaffected: nothing serialises, so nothing is lost.
**Severity:** high impact, narrow trigger. It only fires when `Deposed` is non-empty, which is
exactly the crash-recovery state the field was added for.

## Summary

`ResourceState.Deposed` is erased from state by an ordinary `refresh` or `update` of the affected
address. Since a deposed entry holds the only record of a live object's `ProviderID`, erasing it
leaves a real, billing resource that nothing can name, plan or destroy.

This is the precise outcome the field's own doc comment says it exists to prevent:

> A real object nothing can name is a leak that bills monthly; one state still names is a line in
> the next plan.
> — `pkg/resource/resource.go:154-173`

## Mechanism

Four steps, none of them wrong on its own.

**1. The plugin wire carries only two fields.** `pkg/pluginsdk/serve.go:364`

```go
func resultOf(rs *resource.ResourceState) pluginproto.ResourceResult {
	if rs == nil {
		return pluginproto.ResourceResult{Absent: true}
	}
	return pluginproto.ResourceResult{
		ProviderID: rs.ProviderID,
		Attributes: rs.Attributes,
	}
}
```

Correct by design: a plugin does not own `Deposed` and should never be trusted with it.

**2. The host restores everything else from `carry` — except `Deposed`.**
`internal/pluginhost/adapter.go:305`

```go
// rebuild turns a plugin's answer into state the engine may trust.
//
// carry is the state the host already held, or nil for a resource that did not
// exist before. Everything the plugin does not own comes from there.
...
	if carry != nil {
		// The bookkeeping the plugin was never sent and therefore cannot have
		// lost. Dependencies is the only source of destroy-ordering edges once a
		// resource leaves configuration; Lifecycle is prevent_destroy.
		out.Address = carry.Address
		if carry.Provider != "" {
			out.Provider = carry.Provider
		}
		out.Dependencies = carry.Dependencies
		out.Lifecycle = carry.Lifecycle
		out.CreatedAt = carry.CreatedAt
		out.UpdatedAt = carry.UpdatedAt
		if out.ProviderID == "" {
			out.ProviderID = carry.ProviderID
		}
	}
```

`Deposed` is exactly "bookkeeping the plugin was never sent and therefore cannot have lost". It is
simply not in the list. **This is the defect.**

**3. Two call paths reach it with a real `carry`.**

| caller | line | carry | drops Deposed |
|---|---|---|---|
| `Read` | adapter.go:174 → 183 | `current` | yes |
| `Update` | adapter.go:208 → 221 | `current` | yes |
| `Create` | adapter.go:186 → 203 | synthetic literal | n/a |
| `Import` | adapter.go:261 → 272 | `nil` | n/a (new resource) |

So a plain **`refresh` drops it too**, not only an apply. `internal/refresh/refresh.go:210` reads,
and its own comment at :228 notes "refresh persists what Read returns".

**4. Nothing merges it back.** `internal/state/state.go:48`

```go
func (s *State) Set(r *resource.ResourceState) {
	...
	s.Resources[r.Address.String()] = r
}
```

Wholesale replacement. And `record` (`internal/executor/apply.go:262`) re-attaches `Deposed` only
in the `OpReplace` + `PhaseCreate` + `CreateBeforeDestroy` arm; every other operation falls to
`r.st.Set(res.state)`.

## Why the leak is permanent

The planner only proposes the cleanup for addresses that still carry the record
(`internal/planner/planner.go:182-186`):

```go
	rs, ok := st.Get(addr)
	if !ok || len(rs.Deposed) == 0 {
		continue
	}
```

with the comment:

> Emitting it here is what makes a deposed record a step in a process rather than a place leaks
> accumulate — every plan proposes the cleanup until it works.

Once `Deposed` is nil, that loop skips the address forever. The plan stops proposing the cleanup,
and the object's `ProviderID` — carried only in `d.ProviderID`, used for the plan's own reason
string — is gone from disk.

## Reproduction

1. A resource with `create_before_destroy`, managed by any out-of-process provider.
2. Trigger a replacement. The create half succeeds and `record` stores the old object under
   `Deposed`. State is persisted.
3. The destroy half fails (provider error, or the process dies between phases). `Deposed` survives
   on disk — intended behaviour, and why it is persisted rather than held in memory.
4. Run `infrena refresh`, or any apply that updates that address.
5. Inspect state: the `deposed` array is gone.
6. `infrena plan` no longer proposes the cleanup. The object still exists.

Step 4 is an ordinary, blameless command, which is what makes this bad: recovery from a partial
replacement is destroyed by the next routine operation.

## Suggested fix

One line in `rebuild`, alongside the other carried fields:

```go
		out.Deposed = carry.Deposed
```

There is already precedent for this exact class one directory over. `internal/refresh/refresh.go:234`
calls `value.CarrySensitivityAttrs` for the same reason, with the reasoning spelled out:

> A provider re-derives sensitivity from its own schema and knows nothing of the propagated kind,
> so an observation carries back only half of what state already knew.
>
> That matters twice: refresh persists what Read returns, so without this it would erase the flag
> rather than leave it stale...

`Deposed` is the same shape as that flag. It was recognised for sensitivity and missed here.

## Suggested regression test

Assert at the `rebuild` boundary rather than end to end, so it cannot rot:

- build a `carry` with a non-empty `Deposed`
- pass a `ResourceResult` that (necessarily) has none
- assert the rebuilt state still carries it

A stronger version, worth having: a table over every field of `ResourceState`, asserting that each
one is either transmitted on the wire or carried from `carry`. That closes the whole class instead
of this instance — the next field added to `ResourceState` gets the same treatment automatically.

## What I did not verify

I did not write a failing test in this repo or run a live reproduction; the chain above is read from
source. I had a standing instruction not to modify `infrena`, so nothing here was patched or tested
in place. Found while building the GCP provider plugin, where the boundary is exercised heavily.
