package mmv1

import "slices"

// wireHooks are the custom_code keys that change what goes on the wire or what
// is read back from it. Each is hand-written Go in the Terraform provider, so a
// generated REST type that ignores one is wrong in a way no schema shows.
//
// Measured 2026-09-21 across 942 resources: 423 carry at least one of these.
// Keys deliberately NOT here, because they emit Go that never touches a request
// or response: constants (145), test_check_destroy (45), pre_read (34),
// post_read (21), post_import (19), extra_schema_entry (13), and every tgc_*
// key. Moving a key into this list moves resources into tier 2, so do it only
// with a reason written down.
var wireHooks = []string{
	"custom_create",
	"custom_delete",
	"custom_import",
	"custom_update",
	"decoder",
	"encoder",
	"post_create",
	"post_delete",
	"post_update",
	"pre_create",
	"pre_delete",
	"pre_update",
	"resource_definition",
	"update_encoder",
}

// WireHooks returns the wire-affecting custom_code keys this resource declares,
// sorted. Empty means the resource is a candidate for tier 1.
func (r *Resource) WireHooks() []string {
	var out []string
	for k := range r.CustomCode {
		if slices.Contains(wireHooks, k) {
			out = append(out, k)
		}
	}
	slices.Sort(out)
	return out
}
