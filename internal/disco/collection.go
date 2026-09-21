package disco

import "sort"

// Collection is one flattened node of the resources tree, with the full path
// that reached it.
type Collection struct {
	Path    []string
	Methods map[string]*Method
}

// Collections flattens the resource tree, keeping only nodes that have methods
// of their own, in a deterministic order.
func (d *Document) Collections() []Collection {
	var out []Collection
	var walk func(res map[string]*Resource, path []string)
	walk = func(res map[string]*Resource, path []string) {
		names := make([]string, 0, len(res))
		for n := range res {
			names = append(names, n)
		}
		sort.Strings(names)
		for _, n := range names {
			r := res[n]
			p := append(append([]string{}, path...), n)
			if len(r.Methods) > 0 {
				out = append(out, Collection{Path: p, Methods: r.Methods})
			}
			walk(r.Resources, p)
		}
	}
	walk(d.Resources, nil)
	return out
}
