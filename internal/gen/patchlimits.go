// patchlimits.go handles the APIs that publish a PATCH which only accepts SOME
// of the resource's fields.
//
// Discovery cannot express that. It publishes the request schema, which is the
// whole resource, and says the rest in PROSE on the method's description:
//
//	compute.networks.patch     "Only routingConfig can be modified."
//	compute.subnetworks.patch  "Only certain fields can be updated with a patch
//	                            request as indicated in the field descriptions.
//	                            You must specify the current fingerprint"
//	compute.images.patch       "Only the following fields can be modified:
//	                            family, description, deprecation status."
//
// That matters because of what a missing update verb DOES. A type with no verb
// is replaced on every change: destructive, but it converges, and the plan says
// so out loud. A type with a verb whose patch silently drops the field you
// changed does NOT converge -- the next plan proposes the same change again,
// for ever. This project has already shipped that failure once, on compute
// instances created with initializeParams.
//
// So a restricted patch is refused by default and admitted only by a human
// writing down which fields the API really patches, exactly as a wire hook is.
// The allowlist is what is PATCHABLE rather than what is not, because a field
// added upstream then defaults to replacement (loud, convergent) instead of to
// a patch that is quietly ignored.
package gen

import (
	"regexp"
	"strings"

	"github.com/infrena/infrena-provider-gcp/internal/catalog"
	"github.com/infrena/infrena-provider-gcp/internal/disco"
)

// restrictionProse are the shapes Google writes when a patch takes only some
// fields. Measured against every patch description in the pinned documents on
// 2026-09-23: they match 3 of the 153 updatable types that could be matched to
// their own patch method, and nothing else.
//
// Prose matching is a poor instrument and it is used here in the SAFE
// direction only: a match REFUSES an update, it never permits one. A false
// positive costs a replacement, which is what the type did before any of this
// existed. A false negative costs what we have today. Neither is silent, and
// the allowlist is how a human overrides either.
var restrictionProse = []*regexp.Regexp{
	regexp.MustCompile(`(?i)\bonly\b[^.]{0,90}\b(?:can be|are)\b[^.]{0,40}\b(?:modified|updated|changed|mutable)`),
	regexp.MustCompile(`(?i)only certain fields`),
	regexp.MustCompile(`(?i)fingerprint`),
}

// restrictedPatch reports whether the collection's patch method documents that
// it accepts only some of the resource's fields, and returns Google's own
// sentence so the refusal can quote it rather than paraphrase it.
func restrictedPatch(col disco.Collection) (bool, string) {
	patch := col.Methods["patch"]
	if patch == nil {
		return false, ""
	}
	d := strings.Join(strings.Fields(patch.Description), " ")
	for _, rx := range restrictionProse {
		if rx.MatchString(d) {
			return true, d
		}
	}
	return false, ""
}

// applyPatchAllowlist marks every settable attribute OUTSIDE fields as
// ForceNew, so a change to one of them replaces the resource -- which is what
// the API does anyway -- while a change to a listed field is patched.
//
// Top level only, and deliberately: every restriction Google documents is a
// top-level field, and a nested allowlist would need path syntax the overlay
// has no way to spell (the same limit the alias table carries).
func applyPatchAllowlist(attrs map[string]*catalog.Attr, fields []string) {
	allowed := make(map[string]bool, len(fields))
	for _, f := range fields {
		allowed[f] = true
	}
	for name, a := range attrs {
		if a.Output || allowed[name] || allowed[a.Canonical] {
			continue
		}
		a.ForceNew = true
	}
}
