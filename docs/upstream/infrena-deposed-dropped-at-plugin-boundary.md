**Title:** Deposed records are silently dropped by every out-of-process provider, so a failed `create_before_destroy` cleanup is lost on the next refresh

**Version:** `efbfaaa` (`v0.14.0-1-gefbfaaa`), branch `main`

**What happened:**

`ResourceState.Deposed` does not survive a round trip through the plugin boundary. An ordinary
`refresh` — or any apply that updates the affected address — erases it from state.

`resultOf` (`pkg/pluginsdk/serve.go:364`) puts only `ProviderID` and `Attributes` on the wire, which
is correct: a plugin does not own `Deposed`. The host is supposed to restore the rest from the state
it already held, in `rebuild` (`internal/pluginhost/adapter.go:305`):

```go
	if carry != nil {
		// The bookkeeping the plugin was never sent and therefore cannot have
		// lost. ...
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

`Deposed` is exactly "bookkeeping the plugin was never sent and therefore cannot have lost", and it
is not in the list. Two callers reach `rebuild` with a real `carry` and so drop it:

| caller | line | carry | drops `Deposed` |
|---|---|---|---|
| `Read` | adapter.go:174 → 183 | `current` | yes |
| `Update` | adapter.go:208 → 221 | `current` | yes |
| `Create` | adapter.go:186 → 203 | synthetic literal | n/a |
| `Import` | adapter.go:261 → 272 | `nil` | n/a, new resource |

Nothing merges it back afterwards. `State.Set` (`internal/state/state.go:48`) replaces the map entry
wholesale, and `record` (`internal/executor/apply.go:262`) re-attaches `Deposed` only in the
`OpReplace` + `PhaseCreate` + `CreateBeforeDestroy` arm — every other operation falls through to
`r.st.Set(res.state)`. `internal/refresh/refresh.go:228` notes in its own comment that "refresh
persists what Read returns".

The loss is permanent, because the mechanism that would have recovered it is gated on the thing that
was erased (`internal/planner/planner.go:182`):

```go
	rs, ok := st.Get(addr)
	if !ok || len(rs.Deposed) == 0 {
		continue
	}
```

under the comment "every plan proposes the cleanup until it works". Once `Deposed` is nil that loop
skips the address forever, and `d.ProviderID` — the only handle anything had on the object — is gone
from disk.

**What you expected to happen:**

`rebuild` should carry `Deposed` forward from `carry` the way it already carries `Dependencies`,
`Lifecycle`, `CreatedAt` and `UpdatedAt`, so that a refresh or update leaves the deposed record
intact and the next plan still proposes the cleanup.

Concretely, the field's own documentation states the guarantee being broken
(`pkg/resource/resource.go:154`):

> Normally empty, and an entry that outlives a run means the destroy failed, so the next plan
> schedules it.
>
> A real object nothing can name is a leak that bills monthly; one state still names is a line in
> the next plan.

The suggested fix is one line alongside the other carried fields:

```go
		out.Deposed = carry.Deposed
```

There is already precedent for this exact class one directory over: `internal/refresh/refresh.go:234`
calls `value.CarrySensitivityAttrs` because "a provider re-derives sensitivity from its own schema
and knows nothing of the propagated kind, so an observation carries back only half of what state
already knew... refresh persists what Read returns, so without this it would erase the flag rather
than leave it stale". `Deposed` is the same shape as that flag; it was recognised for sensitivity and
missed here.

A stronger regression test than the obvious one: a table over every field of `ResourceState`,
asserting each is either transmitted on the wire or carried from `carry`. That closes the class
rather than this instance, so the next field added to `ResourceState` is covered automatically.

**A configuration that reproduces it:** (OPTIONAL)

I do not have one, and I would rather say so than paste a file I never ran.

This was found by reading source while building the GCP provider plugin, not by hitting it in
practice. The sequence that should reproduce it, untested:

1. Any resource with `create_before_destroy`, managed by an out-of-process provider.
2. Trigger a replacement. The create half succeeds and `record` stores the old object under
   `Deposed`; state is persisted.
3. Make the destroy half fail — a provider error, or kill the process between the two phases.
   `Deposed` survives on disk, which is intended.
4. Run `infrena refresh`.
5. Inspect state: the `deposed` array is gone, and `infrena plan` no longer proposes the cleanup.

Step 4 is what makes this worth fixing: recovery from a partial replacement is destroyed by a
routine, blameless command.

**Output:** (OPTIONAL)

None — see above. No live run, so no plan or apply output to attach.

**Scope:** every provider that runs as a plugin, which is all of them today, AWS included.
In-process providers are unaffected: nothing serialises, so nothing is lost.
