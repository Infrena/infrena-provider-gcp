# Nested attribute names are never canonicalised, so a spelling one level down reaches the provider verbatim

**Repo:** infrena
**Observed at:** `efbfaaa` (v0.14.0-1-gefbfaaa), branch `main`
**Affects:** every provider. Surfaced against the GCP plugin, but nothing about it is GCP-specific.
**Severity:** medium. Not data loss; a silent, inconsistent rejection that looks like a provider bug.

## Summary

`ResourceDefinition.Canonical` resolves a written attribute spelling — case-folded, and across
declared aliases — but only for **top-level** attributes. It never descends into
`Attribute.Fields`. So the same configuration file accepts `snake_case` at the top level and
silently demands the provider's exact wire spelling one level down.

The unresolved name is not rejected either; it passes through to the provider, which puts it on the
wire. Against GCP that means an unknown field reaching Google's API.

## Mechanism

`pkg/schema/alias.go:22`

```go
func (d *ResourceDefinition) Canonical(written string) (string, bool) {
	if _, ok := d.Attributes[written]; ok {
		return written, true
	}
	folded := foldName(written)
	for name, attr := range d.Attributes {
		if foldName(name) == folded {
			return name, true
		}
		for _, alias := range attr.Aliases {
			if foldName(alias) == folded {
				return name, true
			}
		}
	}
	return "", false
}
```

It walks `d.Attributes` — the top-level map — and stops. `schema.Attribute.Fields
map[string]Attribute` (`pkg/schema/attribute.go:103`) exists and describes nested structure, but
nothing recurses into it.

The function's own doc comment states the invariant this breaks:

> One place resolves aliases, and it is the compiler's boundary with the schema. Every lookup
> downstream of that uses Attribute and an exact name, **because by then the name is canonical.**

That holds for top-level names only. For nested names, "by then the name is canonical" is false,
and everything downstream is built on it.

## Why it is silent

`remoteProvider.check` (`internal/pluginhost/adapter.go:349`) is the guard that catches a name the
schema does not declare:

```go
		attr, declared := def.Attribute(name)
		if !declared {
			return nil, fmt.Errorf(
				"%s returned attribute %s on a %s, which its own schema does not declare\n"+ ...
```

It iterates the top-level `attrs` map only. A nested name that failed to canonicalise is inside a
composite value, so it is never checked. Neither half of the system looks at it.

## Effect a user sees

Given an attribute whose wire name is `cloudRun`:

- at top level, `cloud_run:` resolves — `foldName` matches it
- nested inside another block, `cloud_run:` does not resolve and is sent as `cloud_run`

Same file, same spelling convention, two different outcomes, no diagnostic. The failure surfaces as
whatever the provider's API says about an unrecognised field, which reads as a provider bug rather
than a naming rule.

The inconsistency is the bug. Either spelling rule would be defensible on its own; having both at
different depths is not.

## Note on aliases specifically

For GCP the live part is case/spelling folding, not curated aliases: the GCP catalog only declares
aliases at top level, because its overlay format (`map[type]map[attribute]alias`) has no path
notation to address a nested attribute. A provider that *did* declare nested aliases would find
them inert, which is worth deciding about explicitly — either support them or reject them at
schema-validation time rather than accepting and ignoring them.

## Suggested fix

Make resolution structural rather than a flat lookup. Two viable shapes:

1. **Resolve at the compiler boundary, recursively.** Keep the existing "one place resolves
   aliases" invariant and make it true at every depth: walk the written value alongside the
   declared `Attribute.Fields`, rewriting keys as it goes.
2. **Add a path-aware `Canonical`** taking a path (`["network", "cloud_run"]`) and resolving each
   segment against the previous segment's `Fields`.

(1) preserves the existing architecture and its safety argument; (2) is a smaller change but moves
the invariant's boundary.

**Whichever is chosen, recurse through every nesting edge, not just one.** In the GCP provider the
equivalent fix had to follow both the object edge and the list-element edge. Following only the
object edge under-counted the affected attributes by 7x — 43 versus the real 306, with 263 of them
inside a list. Two of us measured it independently and made the same omission, because both
measurements were written from the same mental model of the structure. If `schema.Attribute` grows
an element edge for lists, the same trap applies here.

## Suggested regression test

- a definition with a nested attribute whose declared name is `cloudRun`
- configuration writing `cloud_run` at that nested position
- assert the value reaches the provider under `cloudRun`
- and the negative: a nested name that resolves to nothing is *reported*, not passed through

The second half matters more than the first. Today there is no diagnostic at all for an
unresolvable nested name, which is what makes this cost debugging time rather than producing an
error message.

## What I did not verify

Read from source; no failing test written in this repo and no live reproduction. I had a standing
instruction not to modify `infrena`. Found while building the GCP provider plugin.
