package disco

import "fmt"

// maxRefDepth bounds $ref expansion, counted in $ref hops, not structural
// nesting. Nesting through plain `properties`, `items` and
// `additionalProperties` is finite: it ends when the document does, however
// deep. $ref hops are not, because Discovery documents contain real cycles —
// of 25 infrastructure APIs checked on 2026-09-21, bigquery, container and
// spanner each have one, container's being a direct self-reference
// (OperationProgress -> OperationProgress). compute does not, at revision
// 20260910. Without a bound on $ref hops the resolver recurses until the
// stack gives out, which is worse than a bad catalog because there is no
// error to read. Counting structural nesting toward the same bound would
// truncate real, finite schema data instead of only breaking cycles.
//
// 8 is deeper than any $ref chain the catalog actually exposes and shallow
// enough that a truncated tail is always something opaque nobody configures
// by hand.
const maxRefDepth = 8

// Resolve returns a copy of s with every $ref inlined, to a bounded depth.
//
// The copy matters: schemas are shared by pointer across every property that
// refers to them, so expanding in place would corrupt the document for the next
// caller.
func (d *Document) Resolve(s *Schema) (*Schema, error) {
	out, _, err := d.resolve(s, 0)
	return out, err
}

// resolveCounted is Resolve plus how many subtrees the depth bound truncated.
// It exists for tests: a bound that counts structural nesting instead of
// $ref hops truncates real schema data silently, and an error-only check
// like Resolve's can't see that. Not exported — nothing outside this
// package's tests needs the count.
func (d *Document) resolveCounted(s *Schema) (*Schema, int, error) {
	return d.resolve(s, 0)
}

func (d *Document) resolve(s *Schema, depth int) (*Schema, int, error) {
	if s == nil {
		return nil, 0, nil
	}
	if depth > maxRefDepth {
		// Truncated: an object with no properties, which every consumer already
		// treats as opaque and copies exactly.
		return &Schema{Type: "object", Description: s.Description}, 1, nil
	}
	if s.Ref != "" {
		target, ok := d.Schemas[s.Ref]
		if !ok {
			return nil, 0, fmt.Errorf("$ref %q is not a schema in %s", s.Ref, d.Name)
		}
		// Only a $ref hop advances depth. Structural nesting below is finite
		// and bounded by the document itself; it must not spend the same
		// budget as cycle detection.
		resolved, truncated, err := d.resolve(target, depth+1)
		if err != nil {
			return nil, 0, err
		}
		// The referring site's own description and readOnly flag win: a property
		// saying "[Output Only] the spec" must stay output-only even though the
		// shared spec schema says nothing about it.
		out := *resolved
		if s.Description != "" {
			out.Description = s.Description
		}
		if s.ReadOnly {
			out.ReadOnly = true
		}
		return &out, truncated, nil
	}

	out := *s
	truncated := 0
	if s.Properties != nil {
		out.Properties = make(map[string]*Schema, len(s.Properties))
		for name, p := range s.Properties {
			r, n, err := d.resolve(p, depth)
			if err != nil {
				return nil, 0, err
			}
			out.Properties[name] = r
			truncated += n
		}
	}
	if s.Items != nil {
		r, n, err := d.resolve(s.Items, depth)
		if err != nil {
			return nil, 0, err
		}
		out.Items = r
		truncated += n
	}
	if s.AdditionalProperties != nil {
		r, n, err := d.resolve(s.AdditionalProperties, depth)
		if err != nil {
			return nil, 0, err
		}
		out.AdditionalProperties = r
		truncated += n
	}
	return &out, truncated, nil
}
