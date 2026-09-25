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
// their own patch method, and nothing else. The "can only patch" shape was
// added on 2026-09-24 for compute's forwarding rules ("Currently, you can only
// patch the network_tier field."), which the first shape misses because no
// "can be" follows the "only". Measured the same way, it matches those two
// collections and nothing else.
//
// Prose matching is a poor instrument and it is used here in the SAFE
// direction only: a match REFUSES an update, it never permits one. A false
// positive costs a replacement, which is what the type did before any of this
// existed. A false negative costs what we have today. Neither is silent, and
// the allowlist is how a human overrides either.
var restrictionProse = []*regexp.Regexp{
	regexp.MustCompile(`(?i)\bonly\b[^.]{0,90}\b(?:can be|are)\b[^.]{0,40}\b(?:modified|updated|changed|mutable)`),
	regexp.MustCompile(`(?i)\bcan only (?:patch|update|modify|change)\b`),
	regexp.MustCompile(`(?i)only certain fields`),
	regexp.MustCompile(`(?i)fingerprint`),
}

// restrictedPatch reports whether the collection's patch method documents that
// it accepts only some of the resource's fields, and returns Google's own
// sentence so the refusal can quote it rather than paraphrase it.
func restrictedPatch(col disco.Collection) (bool, string) {
	patch := patchMethodOf(col)
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
func applyPatchAllowlist(attrs map[string]*catalog.Attr, fields []string, src string) {
	allowed := make(map[string]bool, len(fields))
	for _, f := range fields {
		allowed[f] = true
	}
	for name, a := range attrs {
		if a.Output || allowed[name] || allowed[a.Canonical] {
			continue
		}
		a.ForceNew = true
		addSource(a, "immutable", src)
	}
}

// discoveredUpdateWrapper detects the AIP UpdateXRequest shape on a
// collection's patch method: a request schema that is NOT the resource, but
// holds it under one property, alongside the field mask.
//
// pubsub's topics.patch is the canonical one -- UpdateTopicRequest{topic,
// updateMask} -- and it is why Provider.Update cannot simply send the
// resource: the mask is a REQUIRED field of the body, not a query parameter,
// so a bare resource with ?updateMask=... is rejected outright.
//
// This is resourceSchema's wrapper detection (build.go) pointed at patch
// instead of create. It is deliberately a separate function rather than a flag
// on that one: create reads its leftover properties as create-time PARAMETERS
// and turns them into attributes, while the leftover here is the mask, which is
// machinery rather than anything a user sets.
//
// AIP-134 is what it is detecting: Google's own standard says an update takes
// UpdateXRequest{x, update_mask}. The eight collections that use it here are
// not an oddity, they are the newer convention, and the older bare-body APIs
// (compute and friends) are the ones out of step.
//
// It returns "" unless the shape resolves completely -- one property that refs
// the resource, and exactly one field-mask property beside it. A partly-understood
// envelope is refused, because a request sent in a shape we only half recognise
// is a request we cannot predict the effect of.
func discoveredUpdateWrapper(d *disco.Document, col disco.Collection) (wrapper, maskField string) {
	patch, get := patchMethodOf(col), col.Methods["get"]
	if patch == nil || get == nil || patch.HTTPMethod != "PATCH" {
		return "", ""
	}
	if !sameAddress(patch, get) || patch.Request == nil || get.Response == nil {
		return "", ""
	}
	resourceRef := get.Response.Ref
	if resourceRef == "" || patch.Request.Ref == resourceRef {
		return "", "" // the bare-body case; discoveredUpdate handles it
	}
	raw := d.Schemas[patch.Request.Ref]
	if raw == nil {
		return "", ""
	}
	var masks []string
	for _, name := range sortedKeys(raw.Properties) {
		p := raw.Properties[name]
		switch {
		case p == nil:
		case p.Ref == resourceRef:
			if wrapper != "" {
				return "", "" // two properties claim to be the resource
			}
			wrapper = name
		case p.Format == "google-fieldmask":
			// The protobuf google.protobuf.FieldMask type surviving into
			// Discovery as a format. Keyed on that rather than on the field's
			// NAME, because the name is not stable: six of the eight say
			// "updateMask" and spanner's instances and instancePartitions say
			// "fieldMask". All eight carry this format.
			masks = append(masks, name)
		default:
			// Anything else in the envelope is a knob we would be silently
			// leaving unset -- spanner's validateOnly, say. Leaving it unset is
			// correct (it defaults), so it does not disqualify the shape, but it
			// is why this function refuses anything it cannot name.
		}
	}
	if wrapper == "" || len(masks) != 1 {
		return "", ""
	}
	return wrapper, masks[0]
}

// updateURLMatchesPatch reports whether an update_url magic-modules declared is
// the address of the collection's own patch method.
//
// magic-modules defaults update_verb to PUT (mmv1/api/resource.go), so a
// resource that declares an update_url and no verb has written that url for a
// PUT, and pairing it with a PATCH we inferred would be exactly the kind of
// reconstruction this codebase keeps getting wrong. Checking it instead of
// guessing turns the question into one the documents can answer: the url's path
// must be where the patch lives, and every query parameter it carries must be
// one the patch declares.
//
// Four resources in the vendored tree are in this position -- apigee's
// TargetServer, bigquery's Dataset and compute's Autoscaler and
// RegionAutoscaler -- and the compute pair are why it matters. Their patch is
// published on the COLLECTION, with the resource named by "?autoscaler=", so
// magic-modules' url is not merely compatible with a PATCH, it is the only
// address a PATCH can be sent to.
func updateURLMatchesPatch(updateURL string, patch *disco.Method) bool {
	if patch == nil {
		return false
	}
	path, query, _ := strings.Cut(updateURL, "?")
	if !samePathShape(path, patch.Path) {
		return false
	}
	for _, kv := range strings.Split(query, "&") {
		if kv == "" {
			continue
		}
		k, _, _ := strings.Cut(kv, "=")
		if p := patch.Parameters[k]; p == nil || p.Location != "query" {
			return false
		}
	}
	return true
}

// samePathShape compares two url templates ignoring what their placeholders are
// called and ignoring a leading api-version prefix, which Discovery paths carry
// ("compute/v1/projects/...") and stored templates do not -- the version lives
// in PathPrefix here, which is why absURL exists.
func samePathShape(a, b string) bool {
	na, nb := normalizePathShape(a), normalizePathShape(b)
	return na != "" && (na == nb || strings.HasSuffix(nb, "/"+na) || strings.HasSuffix(na, "/"+nb))
}

func normalizePathShape(p string) string {
	var out []string
	for _, seg := range strings.Split(strings.Trim(p, "/"), "/") {
		switch {
		case seg == "":
		case strings.HasPrefix(seg, "{"):
			out = append(out, "*")
		default:
			out = append(out, seg)
		}
	}
	return strings.Join(out, "/")
}

// lockFieldRE is how compute says a field is its optimistic lock: "You must
// always provide an up-to-date fingerprint hash in order to update the
// instance", "An up-to-date fingerprint must be provided in order to update
// the Subnetwork". It is prose because Discovery has no structure for it.
// Any whitespace between the words: backend services break the line inside
// "in order to\nupdate", and a literal space shipped both of them without
// their lock. Forwarding rules say it as advice ("Include the fingerprint in
// patch request to ensure that you do not overwrite changes"): optional to
// Google, sent here, because it is what keeps a patch from overwriting a
// concurrent change and the live probe that sent it was accepted.
var lockFieldRE = regexp.MustCompile(`(?i)in\s+order\s+to\s+(?:update|patch)|include\s+the\s+fingerprint\s+in\s+patch\s+request`)

// lockFieldOf names the field t's API requires, current, in every update, or
// "". Only a top-level `fingerprint` that says so counts: labelFingerprint
// guards setLabels, a separate method this provider does not call, and
// sending it in a patch is not what the API asks for.
func lockFieldOf(attrs map[string]*catalog.Attr) string {
	a := attrs["fingerprint"]
	if a == nil || !lockFieldRE.MatchString(a.Description) {
		return ""
	}
	return a.Canonical
}

// sameAddress reports whether two methods address the same resource, which is
// not the same as their paths being the same string. Pub/Sub's topics.get is
// "v1/{+topic}" and topics.patch is "v1/{+name}": two spellings of one
// address, since both parameters carry the pattern
// "^projects/[^/]+/topics/[^/]+$". A string comparison refused the update
// envelope for both Pub/Sub types on that difference alone.
//
// Each placeholder is replaced by its parameter's published pattern when it
// has one, and by a bare wildcard when it does not; the version prefix is
// dropped. A method at a genuinely different address -- a ":verb" custom
// method, a collection path -- still compares unequal.
func sameAddress(a, b *disco.Method) bool {
	return a != nil && b != nil && addressOf(a) == addressOf(b)
}

var placeholderRE = regexp.MustCompile(`\{\+?(\w+)\}`)

func addressOf(m *disco.Method) string {
	path := strings.TrimPrefix(m.Path, pathPrefixOf(m.Path))
	return placeholderRE.ReplaceAllStringFunc(path, func(ph string) string {
		name := placeholderRE.FindStringSubmatch(ph)[1]
		if p := m.Parameters[name]; p != nil && p.Pattern != "" {
			return "<" + strings.Trim(p.Pattern, "^$") + ">"
		}
		return "<*>"
	})
}

// templateAddressesMethod reports whether a url template names the same
// address as a method, comparing shapes -- literal segments and wildcards --
// with the method's placeholders read through their published patterns.
//
// It exists for magic-modules' update_url on Pub/Sub's Topic and
// Subscription: "projects/{{project}}/topics/{{name}}", where {{name}} means
// the SHORT name. The catalog's `name` for those types is the FULL resource
// path, because that is what the API's own create path and every response
// use, so expanding magic-modules' template with it would address
// ".../topics/projects%2Fp%2Ftopics%2Ft". When the template and the API's
// patch method are the same address, the API's spelling is the one to use.
func templateAddressesMethod(tmpl string, m *disco.Method) bool {
	if m == nil {
		return false
	}
	path, _, _ := strings.Cut(tmpl, "?")
	want := normalizePathShape(path)
	got := addressOf(m)
	got = strings.NewReplacer("<*>", "*", "[^/]+", "*", "<", "", ">", "").Replace(got)
	return want != "" && normalizePathShape(got) == want
}
