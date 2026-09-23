**Title:** A repeated block's element keys cannot be declared: validation refuses `Fields` on a list, but the compiler's own list walk requires it

**Version:** `v0.14.1` (`1aee741`), branch `main`

**What happened:**

`canonicaliseNested` (`internal/compiler/schema.go:192`) walks a list by applying the SAME `fields`
to every element, and says why:

```go
// Lists are walked for their elements, because Fields describes a map's keys and
// a list of maps is the ordinary shape for a repeated block.
...
	case []value.Value:
		for i, item := range raw {
			out[i] = item
			if rewritten, deeper := canonicaliseNested(item, fields); deeper {
```

So the intended way to describe a repeated block is `Fields` on the list attribute itself.

Validation forbids exactly that, at every depth:

```go
// pkg/schema/definition.go:106  (top level)
		case attr.Fields != nil && attr.Kind != value.KindMap:
			return fmt.Errorf("%s: attribute %q declares Fields but its Kind is not a map; Fields describes a map's known keys", d.Type, name)

// pkg/schema/definition.go, validateFields  (every nested level)
		case nested.Fields != nil && nested.Kind != value.KindMap:
			return fmt.Errorf("%s: attribute %q declares Fields but its Kind is not a map; ...
```

The two cannot both be satisfied. Any attribute carrying `Fields` must be `KindMap`, so
`canonicaliseNested`'s list branch can only be reached when a `KindMap` attribute's value is
actually a list — a Kind/value mismatch a well-formed provider never produces. The branch is
effectively dead for the case its own comment describes.

**Consequence.** A list-of-objects attribute has no way to declare its element keys, so `Fields` is
nil there, and nil means open: keys are the user's, the walk stops without complaint. For every
repeated block, element keys are

- never canonicalised — a declared alias or a case variant does not resolve
- never collision-checked by `checkFieldSpellings`
- never reported when unknown — they pass through to the provider and go out on the wire

That last one is the silent behaviour v0.14.1's nested-name work was meant to end. It still holds
for anything inside a repeated block.

**Measured on the GCP catalog:** 1,901 attributes carry a list element with its own fields, and
**5,841 of the catalog's ~11,200 generated aliases are reachable only through a list element**,
across 95 of 233 types. None of them resolve today, on any version.

**What you expected to happen:**

A repeated block's element keys should be declarable and should behave like a map's keys —
canonicalised, collision-checked, and reported when unknown.

Either of two shapes would do it, and the choice is yours:

1. **Permit `Fields` on `KindList`** and define it as describing each element. That matches
   `canonicaliseNested` as already written — the list branch starts working rather than being
   unreachable — and the validation change is the two `Kind != value.KindMap` clauses becoming
   "not a map and not a list". `checkFieldSpellings` already recurses on `Fields` with no Kind
   gate, so it needs nothing.
2. **Add an explicit element attribute** (`Elem *Attribute`) and walk it in the list branch. More
   honest about the difference between "this map's keys" and "every element's keys", at the cost
   of a new field on the wire and in every walk.

(1) is smaller and makes an existing comment true. (2) is what our own catalog uses internally and
is easier to reason about, but it is a protocol-visible change.

**A configuration that reproduces it:** (OPTIONAL)

No config needed — it is refused at schema load, before any configuration is read. Any provider
declaring `Fields` on an attribute whose `Kind` is `KindList` reproduces it. In our case the
one-line change was mapping our own element fields onto the list attribute's `Fields` in the
catalog-to-schema conversion.

**Output:** (OPTIONAL)

```
infrena refuses the generated catalog: gcp.aclpolicy: attribute "clusterAclPolicyAttachments"
declares Fields but its Kind is not a map; Fields describes a map's known keys
```

**How this was found.** By taking the advice that a list's element keys belong on the list
attribute's own `Fields`, implementing it, and watching `schema.ValidateAll` refuse the result. The
advice matched `canonicaliseNested` exactly; it was validation that disagreed. Worth noting because
reading either side alone supports the opposite conclusion — only the attempt shows they conflict.

**Scope:** every provider with a repeated block. Surfaced on GCP because its catalog is generated
and heavily nested (depth 9), so the proportion behind a list element is large.
