**Title:** Nested attribute names are never canonicalised, so the same file accepts one spelling at the top level and demands another one level down

**Version:** `efbfaaa` (`v0.14.0-1-gefbfaaa`), branch `main`

**What happened:**

`ResourceDefinition.Canonical` (`pkg/schema/alias.go:22`) resolves a written attribute spelling —
case-folded, and across declared aliases — but only against the top-level attribute map. It never
descends into `Attribute.Fields`:

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

`schema.Attribute.Fields map[string]Attribute` (`pkg/schema/attribute.go:103`) exists and describes
nested structure, but nothing recurses into it.

The function's own doc comment states the invariant this breaks:

> One place resolves aliases, and it is the compiler's boundary with the schema. Every lookup
> downstream of that uses Attribute and an exact name, **because by then the name is canonical.**

That is true for top-level names only. For nested names it is false, and everything downstream is
built on it.

The failure is silent in both directions. `remoteProvider.check`
(`internal/pluginhost/adapter.go:349`) is the guard that catches a name the schema does not declare,
and it iterates the top-level map only — a nested name that failed to canonicalise sits inside a
composite value and is never checked. So an unresolved nested name is neither rewritten nor
rejected; it passes through to the provider, which puts it on the wire. Against GCP that means an
unknown field reaching Google's API.

For an attribute whose declared name is `cloudRun`:

- at top level, `cloud_run:` resolves — `foldName` matches it
- nested inside another block, `cloud_run:` does not resolve and is sent as `cloud_run`

Same file, same spelling convention, two different outcomes, no diagnostic.

**What you expected to happen:**

A spelling that resolves at the top level should resolve at any depth, and a nested name that
resolves to nothing should be reported rather than passed through.

The inconsistency is the bug rather than either rule on its own — both spelling rules are
defensible; having both at different depths is not. The second half matters more than the first:
today there is no diagnostic at all for an unresolvable nested name, which is what turns this into
debugging time rather than an error message.

Two viable shapes for the fix:

1. **Resolve at the compiler boundary, recursively.** Keeps the existing "one place resolves
   aliases" invariant and makes it true at every depth: walk the written value alongside the
   declared `Attribute.Fields`, rewriting keys as it goes.
2. **A path-aware `Canonical`** taking `["network", "cloud_run"]` and resolving each segment against
   the previous segment's `Fields`.

(1) preserves the current architecture and its safety argument; (2) is a smaller change but moves
the invariant's boundary.

**Whichever is chosen, recurse through every nesting edge, not just one.** In the GCP provider the
equivalent fix had to follow both the object edge and the list-element edge. Following only the
object edge under-counted the affected attributes by 7x — 43 against a real 306, with 263 of them
inside a list. Two people measured it independently and made the same omission, because both
measurements were written from the same mental model of the structure. If `schema.Attribute` ever
grows an element edge for lists, the same trap applies here.

One related thing worth an explicit decision rather than a fix: a provider that declared aliases on
*nested* attributes would find them silently inert today. Either support them, or reject them at
schema-validation time — accepting and ignoring them is the worst of the three.

**A configuration that reproduces it:** (OPTIONAL)

I do not have a tested one, and I would rather say so than paste a file I never ran. This was found
by reading source while building the GCP provider plugin.

The shape that should reproduce it, untested: any resource type with a nested object attribute whose
declared field name is camelCase, with configuration writing that nested field in snake_case, and
the same spelling written against a top-level camelCase attribute for contrast. The top-level one
resolves; the nested one reaches the provider verbatim.

**Output:** (OPTIONAL)

None — no live run, so no plan or error output to attach.

**Scope:** every provider. Surfaced against the GCP plugin, but nothing about it is GCP-specific.
For GCP the live part is case and spelling folding rather than curated aliases, since the GCP
catalog only declares aliases at top level — its overlay format (`map[type]map[attribute]alias`) has
no path notation to address a nested attribute.
