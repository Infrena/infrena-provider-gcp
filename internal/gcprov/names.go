package gcprov

import (
	"github.com/infrena/infrena-provider-gcp/internal/catalog"
	"github.com/infrena/infrena/pkg/value"
)

// Two namespaces meet in this package, and until this file existed nothing
// translated between them.
//
// catalog.Type.Attributes -- and Attr.Fields and Attr.Elem below it, at every
// depth -- is keyed by the SCHEMA name: what a user writes in configuration,
// what infrena's schema declares, and what the host diffs state and desire
// by. Attr.Canonical is the WIRE name: what GCP puts in a response body and
// expects in a request body. They are equal for the great majority of
// attributes and deliberately not for the ones the generator renamed around
// a reserved word -- "type" becomes "type_value", "provider" becomes
// "provider_value", "lifecycle" becomes "lifecycle_value".
//
// MEASURED ON THE EMBEDDED CATALOG, 2026-09-22: 306 attributes across 56 of
// the 233 types have a schema key that differs from their wire name, 7 of
// them Required, at nesting depths up to 9. 18 of the 86 updatable types are
// affected, 8 of them ONLY below the top level.
//
// Only 43 of those 306 are reachable by walking Fields alone. The other 263
// sit inside a LIST and are reached through Attr.Elem, so a translation that
// recurses into Fields but not Elem fixes one seventh of the corpus and
// hides the rest -- the same shape of half-fix as translating only the top
// level, one turn further in. gcp.grpcroute, gcp.responsepolicyrule and
// gcp.router are updatable types whose renamed attributes live ONLY inside a
// list, so a Fields-only translation reports them as unaffected.
// TestTheCatalogStillRenamesAttributes (internal/catalog) keeps those counts
// honest against a regeneration.

// toWire maps a schema attribute name to the name GCP expects on the wire,
// among the attributes declared at ONE level -- a Type's own Attributes, or
// one Attr's Fields. Depth is the caller's job: it walks down carrying the
// matching level (see wireBody).
//
// Identity for a name the level does not declare. A body may legitimately
// carry a field the catalog never modelled, and inventing a translation for
// one would corrupt it.
func toWire(attrs map[string]*catalog.Attr, name string) string {
	if a, ok := attrs[name]; ok && a.Canonical != "" {
		return a.Canonical
	}
	return name
}

// toSchema maps a wire field name back to the schema name configuration
// uses, at one level. Identity for an undeclared name, for the same reason
// toWire is.
//
// A name that is itself a declared key whose own Canonical matches is
// answered without scanning -- every attribute but the renamed 306. The scan
// behind that is linear over the level's declared attributes; the widest
// level in the corpus is gcp.container.cluster's "cluster" at 87, so the
// bound is a few thousand string comparisons per response body, once per
// Read.
//
// The answer is unambiguous only because the generator's renaming is
// injective within a level: no level declares two attributes with the same
// Canonical, and no renamed attribute's Canonical is also one of its
// siblings' keys. Both are measured as zero and pinned by
// TestNoLevelRenamesTwoAttributesOntoOneWireName (internal/catalog), rather
// than papered over with a tiebreak here -- a tiebreak would let a
// regenerated catalog silently pick one of two meanings for a field, which
// is worse than a test that fails at generation time.
func toSchema(attrs map[string]*catalog.Attr, name string) string {
	if a, ok := attrs[name]; ok && a.Canonical == name {
		return name
	}
	for schemaName, a := range attrs {
		if a.Canonical == name {
			return schemaName
		}
	}
	return name
}

// wireBody converts a schema-keyed tree of resolved values into the
// wire-keyed JSON datum GCP accepts: what a create POSTs and what a patch
// body carries. It recurses through Fields AND Elem, because 263 of the
// corpus's 306 renamed attributes are only reachable through a list.
// wireBody translates a level of a body into wire names, DROPPING attributes
// GCP computes for itself.
//
// The Output filter has to be here rather than only at the top level, because
// an API rejects a body that sets a server-computed field at ANY depth. 922
// attributes below the top level are Output, across 101 types, and the host
// does not stop one reaching us: infrena refuses a computed attribute only in
// its top-level loop, and checkNestedKeys -- the only thing that walks deeper
// -- checks that a key EXISTS and nothing else. So a nested computed field
// written in configuration is accepted by the host and arrives here.
//
// The patch path has always done this (buildNested skips f.Output). The create
// path did not, which is the asymmetry: the same field GCP would reject on a
// POST was correctly withheld from a PATCH.
func wireBody(attrs map[string]*catalog.Attr, vals map[string]value.Value) map[string]any {
	out := make(map[string]any, len(vals))
	for name, v := range vals {
		if a := attrs[name]; a != nil && a.Output {
			continue
		}
		out[toWire(attrs, name)] = wireValue(attrs[name], v)
	}
	return out
}

// wireValue converts one resolved value, translating the names inside it.
//
// a is what the catalog declares for that value, or nil when it declares
// nothing. A nil or Opaque attribute is copied exactly, keys and all: Opaque
// says so in as many words ("a value copied exactly: no key translation,
// nothing dropped, no reordering" -- catalog.Attr), because a free-form map
// holds user data whose keys mean whatever the user meant, and an undeclared
// value is one this catalog cannot claim to understand.
func wireValue(a *catalog.Attr, v value.Value) any {
	if a == nil || a.Opaque {
		return toRaw(v)
	}
	switch {
	case v.Kind == value.KindMap && len(a.Fields) > 0:
		fields, ok := v.Raw.(map[string]value.Value)
		if !ok {
			// Kind and Raw disagree. toRaw is the honest copy; guessing a
			// shape here would invent one.
			return toRaw(v)
		}
		return wireBody(a.Fields, fields)
	case v.Kind == value.KindList && a.Elem != nil:
		items, ok := v.Raw.([]value.Value)
		if !ok {
			return toRaw(v)
		}
		out := make([]any, 0, len(items))
		for _, item := range items {
			out = append(out, wireValue(a.Elem, item))
		}
		return out
	default:
		return toRaw(v)
	}
}

// schemaAttrs converts a wire-keyed response body into the schema-keyed
// attributes state holds -- the exact inverse of wireBody, and the reason a
// refreshed resource stops disagreeing with the configuration that made it.
func schemaAttrs(attrs map[string]*catalog.Attr, body map[string]any) map[string]value.Value {
	out := make(map[string]value.Value, len(body))
	for name, raw := range body {
		schemaName := toSchema(attrs, name)
		out[schemaName] = schemaValue(attrs[schemaName], raw)
	}
	return out
}

// schemaValue converts one response datum, translating the names inside it.
// Same nil/Opaque rule as wireValue, for the same reason in the other
// direction.
func schemaValue(a *catalog.Attr, raw any) value.Value {
	if a == nil || a.Opaque {
		return fromRaw(raw)
	}
	switch x := raw.(type) {
	case map[string]any:
		if len(a.Fields) == 0 {
			return fromRaw(raw)
		}
		return value.Map(schemaAttrs(a.Fields, x), value.SourceProvider)
	case []any:
		if a.Elem == nil {
			return fromRaw(raw)
		}
		items := make([]value.Value, len(x))
		for i, e := range x {
			items[i] = schemaValue(a.Elem, e)
		}
		return value.List(items, value.SourceProvider)
	default:
		return fromRaw(raw)
	}
}

// wireAliases returns vals with an ADDITIONAL entry under the wire name for
// every top-level attribute the generator renamed, leaving the schema-keyed
// entries exactly where they were.
//
// It exists because a url template is not written in either namespace
// consistently. magic-modules spells placeholders with the schema name, but
// a template taken from a service's own Discovery document spells them the
// way the API does. One type in the corpus is caught between the two:
// gcp.resourcerecordset's self_link and import_format are
// "projects/{project}/managedZones/{managedZone}/rrsets/{name}/{type}", so
// they ask for "type" while configuration supplies it under "type_value"
// (measured 2026-09-22: it is the only type whose url templates name a
// renamed attribute at all).
//
// The cost of not doing this is not cosmetic. ProviderID's expansion is what
// createdID falls back to when an operation's target cannot be reduced --
// the orphan rule's last resort -- and for that type it would fail outright
// with `url template "..." needs "type", which is not set`, turning a
// successful create into an error the host drops, leaving a real resource
// tracked nowhere.
//
// Both spellings rather than a replacement, because the other 232 types'
// templates use the schema name. The two can never collide: no renamed
// attribute's Canonical is also a sibling's key (see toSchema).
func wireAliases(attrs map[string]*catalog.Attr, vals map[string]value.Value) map[string]value.Value {
	var out map[string]value.Value
	for name, v := range vals {
		a, ok := attrs[name]
		if !ok || a.Canonical == "" || a.Canonical == name {
			continue
		}
		if _, taken := vals[a.Canonical]; taken {
			continue
		}
		if out == nil {
			out = make(map[string]value.Value, len(vals)+1)
			for k, existing := range vals {
				out[k] = existing
			}
		}
		out[a.Canonical] = v
	}
	if out == nil {
		return vals
	}
	return out
}

// declaredIDAttrs translates the attributes ParseProviderID recovered from a
// provider id into schema names, and DROPS the ones the type does not
// declare.
//
// A provider id is a path, and its segments are named after url
// PLACEHOLDERS, which are not the same namespace as a type's attributes:
// gcp.channel's id names "{{project}}" and "{{location}}" and gcp.channel
// declares neither, because the generator takes a type's attributes from its
// API BODY schema and those two are path segments. Only 3 of the 233 shipped
// types declare `project` at all.
//
// The host REFUSES a resource state carrying an attribute the plugin's own
// schema does not declare -- "gcp returned attribute \"location\" on a
// gcp.channel, which its own schema does not declare" -- and fails the
// operation. It is not a warning and not a filter on its side: the create,
// the refresh or the discovery walk that produced it fails outright. So
// every path that turns an id back into attributes must come through here.
//
// Nothing is lost by dropping them. Every url this provider builds for an
// existing resource is built from the provider id itself (see itemURL), not
// from state, so the segments are still there when they are needed.
func declaredIDAttrs(attrs map[string]*catalog.Attr, idAttrs map[string]value.Value) map[string]value.Value {
	out := make(map[string]value.Value, len(idAttrs))
	for k, v := range idAttrs {
		name := toSchema(attrs, k)
		if _, declared := attrs[name]; !declared {
			continue
		}
		out[name] = v
	}
	return out
}
