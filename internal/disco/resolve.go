package disco

import "fmt"

// maxRefDepth bounds $ref expansion. Discovery documents contain real cycles, so
// a resolver without a bound hangs the generator rather than producing a bad
// catalog — the worse of the two failures, because it has no error to read.
//
// 8 is deeper than any nesting the catalog actually exposes and shallow enough
// that the truncated tail is always something opaque nobody configures by hand.
const maxRefDepth = 8

// Resolve returns a copy of s with every $ref inlined, to a bounded depth.
//
// The copy matters: schemas are shared by pointer across every property that
// refers to them, so expanding in place would corrupt the document for the next
// caller.
func (d *Document) Resolve(s *Schema) (*Schema, error) {
	return d.resolve(s, 0)
}

func (d *Document) resolve(s *Schema, depth int) (*Schema, error) {
	if s == nil {
		return nil, nil
	}
	if depth > maxRefDepth {
		// Truncated: an object with no properties, which every consumer already
		// treats as opaque and copies exactly.
		return &Schema{Type: "object", Description: s.Description}, nil
	}
	if s.Ref != "" {
		target, ok := d.Schemas[s.Ref]
		if !ok {
			return nil, fmt.Errorf("$ref %q is not a schema in %s", s.Ref, d.Name)
		}
		resolved, err := d.resolve(target, depth+1)
		if err != nil {
			return nil, err
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
		return &out, nil
	}

	out := *s
	if s.Properties != nil {
		out.Properties = make(map[string]*Schema, len(s.Properties))
		for name, p := range s.Properties {
			r, err := d.resolve(p, depth+1)
			if err != nil {
				return nil, err
			}
			out.Properties[name] = r
		}
	}
	if s.Items != nil {
		r, err := d.resolve(s.Items, depth+1)
		if err != nil {
			return nil, err
		}
		out.Items = r
	}
	if s.AdditionalProperties != nil {
		r, err := d.resolve(s.AdditionalProperties, depth+1)
		if err != nil {
			return nil, err
		}
		out.AdditionalProperties = r
	}
	return &out, nil
}
