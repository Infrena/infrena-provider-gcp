package gcprov

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"

	"github.com/infrena/infrena-provider-gcp/internal/catalog"
	"github.com/infrena/infrena/pkg/provider"
	"github.com/infrena/infrena/pkg/value"
)

// CloudAssetBaseURL is where Cloud Asset Inventory lives, from its own
// Discovery document (schemas/cloudasset.json: rootUrl
// "https://cloudasset.googleapis.com/", servicePath ""). Settings.AssetInventoryBaseURL
// overrides it; nothing else should.
//
// Exported because gcpplugin's endpoint override has to redirect CAI too,
// and it can only do that by knowing where CAI would otherwise have been --
// CAI is the one API this provider calls that has no catalog type to take a
// host from. A second copy of the literal there would be a second thing to
// keep in step.
const CloudAssetBaseURL = "https://cloudasset.googleapis.com/"

// searchAllResourcesPath is CAI's own method path, VERBATIM from its
// Discovery document: "v1/{+scope}:searchAllResources", a GET.
//
// A GET, not a POST. The custom-verb suffix makes it look like an RPC and
// the task brief and the fake both had it as a POST; schemas/cloudasset.json
// says httpMethod GET, with query parameters (scope, assetTypes, pageSize,
// pageToken, query, orderBy, readMask) and no request body at all. A POST
// against the real endpoint is not a slower path, it is a 404 -- and since
// discovery falls back silently on ANY CAI failure, it would have looked
// exactly like a project where the asset API is not enabled.
//
// {+scope} is reserved expansion: the scope is "projects/my-project", a path
// with a "/" in it that must not be escaped.
const searchAllResourcesPath = "v1/{+scope}:searchAllResources"

// Discover lists what exists in the configured projects, whether or not
// anything here manages it.
//
// Cloud Asset Inventory answers for a whole project in a handful of calls,
// which is why GCP discovery is complete by default where the AWS provider
// has to ship a curated type list to avoid roughly fifteen hundred
// ListResources calls per region. When CAI is unavailable the per-type
// fallback is narrower by necessity, and saying WHICH path ran matters: an
// empty result from CAI means "this project has nothing", while an empty
// result from the fallback may only mean "we did not look at that type".
//
// IT FAILS OPEN, per project and per type. A project that cannot be scanned
// is reported on stderr and skipped, because failing the whole call would
// let one project nobody has access to hide every resource in every other
// one.
func (p *Provider) Discover(ctx context.Context, req provider.DiscoverRequest) ([]provider.DiscoveredResource, error) {
	projects := p.settings.DiscoverProjects
	if len(projects) == 0 {
		projects = []string{p.settings.Project}
	}
	var out []provider.DiscoveredResource
	for _, project := range projects {
		if project == "" {
			fmt.Fprintf(os.Stderr, "gcp: discover: no project is configured to scan\n")
			continue
		}
		found, err := p.discoverViaCAI(ctx, project, req.Types)
		if err == nil {
			out = append(out, found...)
			continue
		}
		// One line, naming the reason. A silent fallback makes a permissions
		// problem look like an empty project.
		fmt.Fprintf(os.Stderr,
			"gcp: %s: Cloud Asset Inventory unavailable (%v); falling back to per-type listing, "+
				"which covers only the configured types\n", project, err)
		found, err = p.discoverViaList(ctx, project, req.Types)
		if err != nil {
			fmt.Fprintf(os.Stderr, "gcp: %s: discovery failed: %v\n", project, err)
			continue
		}
		out = append(out, found...)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].ProviderID != out[j].ProviderID {
			return out[i].ProviderID < out[j].ProviderID
		}
		return out[i].Type < out[j].Type
	})
	return out, nil
}

// discoverViaCAI searches one project's whole asset inventory and maps each
// result back to the catalog type that serves it.
//
// A result whose asset type the catalog does not serve is skipped in
// silence: that is not an error, it is a GCP resource this provider does not
// model, and there are many.
//
// The attributes it reports are the ones the resource's own NAME encodes --
// project, location, the resource's own name -- and no more. A CAI search
// result is not a resource body: it carries the full resource name, the
// asset type, labels and a handful of index fields, and the resource's
// actual configuration only if a readMask asks for it, per version, in a
// shape that differs from what the resource's own API answers with. Reading
// the real attributes means reading the resource, which is what Import
// already does for the resources a user chooses to adopt; doing it here
// would mean one GET per resource in the project, which is exactly the cost
// CAI exists to avoid.
func (p *Provider) discoverViaCAI(ctx context.Context, project string, want []string) ([]provider.DiscoveredResource, error) {
	byAsset := p.assetTypeIndex()
	wanted := nameSet(want)

	base := p.settings.AssetInventoryBaseURL
	if base == "" {
		base = CloudAssetBaseURL
	}
	rel, err := ExpandURL(searchAllResourcesPath, map[string]value.Value{
		"scope": value.String("projects/"+project, value.SourceProvider),
	})
	if err != nil {
		return nil, err
	}

	var out []provider.DiscoveredResource
	pageToken := ""
	for {
		q := url.Values{}
		// Narrow server-side when the caller named types. The filter is
		// applied again below, because one asset type can serve several
		// catalog types and only some of them may have been asked for.
		for _, at := range assetTypesFor(byAsset, wanted) {
			q.Add("assetTypes", at)
		}
		if pageToken != "" {
			q.Set("pageToken", pageToken)
		}
		reqURL := base + rel
		if len(q) > 0 {
			reqURL += "?" + q.Encode()
		}
		body, err := p.client.Do(ctx, http.MethodGet, reqURL, nil)
		if err != nil {
			return nil, err
		}
		for _, result := range listItems(body, "results") {
			r, ok := p.discoveredFromAsset(byAsset, wanted, result)
			if ok {
				out = append(out, r)
			}
		}
		next, _ := body["nextPageToken"].(string)
		if next == "" {
			return out, nil
		}
		if next == pageToken {
			return nil, fmt.Errorf("cloud asset search returned the same page token twice; stopping")
		}
		pageToken = next
	}
}

// discoveredFromAsset turns one CAI search result into a discovered
// resource, or reports that it could not be attributed to exactly one
// catalog type.
//
// AN ASSET TYPE DOES NOT IDENTIFY A CATALOG TYPE. Measured on this catalog,
// 2026-09-22: 27 of the 191 distinct asset types are shared by two or more
// of the 233 catalog types, and they are exactly the scope and parent
// variants the naming work created -- compute.googleapis.com/Autoscaler is
// both gcp.autoscaler (zonal) and gcp.regionautoscaler, and
// logging.googleapis.com/LogBucket is four types, one per hierarchy root. A
// flat assetType -> type map picks whichever was indexed last and MISLABELS
// the rest: a zonal autoscaler reported as gcp.regionautoscaler generates
// configuration that looks right and cannot apply.
//
// So the resource's OWN NAME decides, which CAI returns in full
// ("//compute.googleapis.com/projects/p/zones/us-central1-a/autoscalers/x").
// See candidatesMatching for the two questions it is asked. Where exactly
// one candidate answers both, that is the type; where none or several do,
// the result is SKIPPED and the skip is reported, naming the asset type and
// every candidate. Guessing here is worse than not reporting: the user gets
// a resource labelled as a type it is not, and configuration generated from
// it cannot work.
func (p *Provider) discoveredFromAsset(byAsset map[string][]*catalog.Type, wanted map[string]bool, result map[string]any) (provider.DiscoveredResource, bool) {
	assetType, _ := result["assetType"].(string)
	fullName, _ := result["name"].(string)
	candidates := byAsset[assetType]
	if len(candidates) == 0 {
		return provider.DiscoveredResource{}, false // a type this provider does not model
	}

	rel := relativeResourceName(fullName)
	matched := candidatesMatching(candidates, rel)
	if len(matched) != 1 {
		fmt.Fprintf(os.Stderr,
			"gcp: discover: %q (%s) matches %d of the %d types that serve that asset type (%s); skipping it "+
				"rather than labelling it as one of them\n",
			fullName, assetType, len(matched), len(candidates), strings.Join(typeNames(candidates), ", "))
		return provider.DiscoveredResource{}, false
	}
	ty := matched[0]
	if len(wanted) > 0 && !wanted[ty.Name] {
		return provider.DiscoveredResource{}, false
	}

	owned, reason := SystemOwned(ty, result)
	return provider.DiscoveredResource{
		Type:              ty.Name,
		ProviderID:        rel,
		Attributes:        idAttributes(ty, rel),
		SystemOwned:       owned,
		SystemOwnedReason: reason,
	}, true
}

// candidatesMatching narrows the types that share one asset type down to the
// ones a resource with this relative name could actually be, by asking two
// questions the catalog can answer:
//
//   - is the name of the type's own id shape (matchesIDShape, which is
//     ParseProviderID's matcher plus one allowance for a multi-segment
//     parent)? A candidate that passes is one whose id shape this name
//     genuinely is. It separates the zonal autoscaler from the regional one
//     ("/zones/" against "/regions/") and the 14 shared asset types whose
//     templates differ at all.
//   - does the name start at the same hierarchy root the type's collection
//     hangs off (catalog.Type.ParentRoot)?
//
// The second question is not redundant. For 13 of the 27 shared asset types
// the sharing types' templates are CHARACTER-IDENTICAL -- logging's four
// LogBucket variants are all self_link "{+name}", which matches every string
// there is -- so the first question alone leaves all four matching and every
// log bucket in the project would be skipped as ambiguous. With the root
// checked too, all 27 shared asset types resolve to exactly one candidate
// (verified against the regenerated catalog: 0 of 27 remain ambiguous on the
// (parent root, self_link shape) pair).
//
// self_link ALONE, never ImportFormat's other accepted shapes. Those exist
// so a user can type a shorthand, and 11 types accept a bare "{{name}}" that
// matches literally any single segment -- trying them here would make
// candidates match things they are not.
func candidatesMatching(candidates []*catalog.Type, rel string) []*catalog.Type {
	var out []*catalog.Type
	for _, ty := range candidates {
		if root := ty.ParentRoot; root != "" && firstPathSegment(rel) != root {
			continue
		}
		if matchesIDShape(ty, rel) {
			out = append(out, ty)
		}
	}
	return out
}

// matchesIDShape reports whether rel could be a resource name of ty's own id
// shape, by matching it against ty.SelfLink the way ParseProviderID does.
//
// With ONE addition ParseProviderID does not make, because it is answering a
// different question. A plain "{{parent}}" placeholder is written as a
// single segment but holds a whole hierarchy node: magic-modules' own
// templates say "{{parent}}/locations/{{location}}/addressGroups/{{name}}"
// and the resource that template describes is really called
// "projects/123/locations/us/addressGroups/g" -- five template segments
// against six real ones, so a strict segment-by-segment match fails for
// every resource of that type. 17 types across 6 of the 27 shared asset
// types are shaped that way (capabilityConfigs, savedQueries, addressGroups,
// firewallEndpoints, securityProfiles, securityProfileGroups), and without
// this every one of their resources would be skipped as unattributable.
//
// The absorption is deliberately narrow: only the template's FIRST segment,
// only when it is a plain placeholder, only for a type whose ParentRoot says
// where its names begin, and only when what is absorbed starts at that root
// and is at least a root plus an id. Everything after the first segment
// still has to match exactly, so this loosens which names a type accepts
// without loosening which types a name picks out.
func matchesIDShape(ty *catalog.Type, rel string) bool {
	if ty.SelfLink == "" {
		return false
	}
	if _, err := parseAgainstTemplate(ty, ty.SelfLink, rel); err == nil {
		return true
	}
	root := ty.ParentRoot
	if root == "" || !strings.HasPrefix(rel, root+"/") {
		return false
	}
	tmplSegs := strings.Split(ty.SelfLink, "/")
	if len(tmplSegs) < 2 {
		return false
	}
	if _, reserved, ok := parsePlaceholderSegment(tmplSegs[0]); !ok || reserved {
		return false
	}
	relSegs := strings.Split(rel, "/")
	// Everything after the parent must line up segment for segment, leaving
	// at least "<root>/<id>" for the parent itself.
	head := len(relSegs) - (len(tmplSegs) - 1)
	if head < 2 {
		return false
	}
	_, err := parseAgainstTemplate(ty, strings.Join(tmplSegs[1:], "/"), strings.Join(relSegs[head:], "/"))
	return err == nil
}

// discoverViaList scans one type at a time. Used ONLY when CAI is
// unavailable, and narrower than CAI by necessity: it can only look at types
// somebody named.
func (p *Provider) discoverViaList(ctx context.Context, project string, want []string) ([]provider.DiscoveredResource, error) {
	types := want
	if len(types) == 0 {
		types = p.settings.DiscoverTypes
	}
	if len(types) == 0 {
		types = p.catalog.DiscoverDefault
	}
	var out []provider.DiscoveredResource
	for _, name := range types {
		ty, ok := p.catalog.Type(name)
		if !ok {
			fmt.Fprintf(os.Stderr, "gcp: discover: no such type %q\n", name)
			continue
		}
		if ty.ListField == "" {
			// No list method, or none with a usable array property: types
			// that exist only under a parent, and compute's aggregatedList-only
			// shapes. Skipped, and SAID SO -- silently omitting a type is
			// indistinguishable from that type having nothing in it.
			fmt.Fprintf(os.Stderr, "gcp: discover: %s cannot be listed; skipping\n", name)
			continue
		}
		found, err := p.listOneType(ctx, project, ty)
		if err != nil {
			// Fail open per type, for the same reason as per project.
			fmt.Fprintf(os.Stderr, "gcp: discover: %s: %v\n", name, err)
			continue
		}
		out = append(out, found...)
	}
	return out, nil
}

// listOneType pages through one type's collection in one project.
//
// AN EMPTY COLLECTION IS A 200 WITH NO RESULTS, NOT A 404. This path scans
// many types across a project and finds nothing for most of them, so empty
// is the ordinary case and needs no special handling at all. A 404 here is
// therefore a real answer -- the API is not enabled, or the collection's url
// is wrong for this project -- and it is returned as an error rather than
// swallowed, because swallowing it hides exactly the thing this path exists
// to surface. isNotFound is deliberately NOT consulted.
func (p *Provider) listOneType(ctx context.Context, project string, ty *catalog.Type) ([]provider.DiscoveredResource, error) {
	scope := p.scopeAttrs(project)
	rel, err := ExpandURL(ty.BaseURL, scope)
	if err != nil {
		return nil, err
	}
	collection := absURL(ty, rel)

	var out []provider.DiscoveredResource
	pageToken := ""
	seen := map[string]bool{}
	for {
		reqURL := collection
		if pageToken != "" {
			// A collection url may carry its own query (a dataset's
			// accessPolicyVersion). Joined with a second "?", the token was
			// never read, Google answered the first page again with the same
			// token, and the loop never ended: a discover reply passed 16 MB
			// and hung the host (live run, 2026-09-25).
			sep := "?"
			if strings.Contains(reqURL, "?") {
				sep = "&"
			}
			reqURL += sep + "pageToken=" + url.QueryEscape(pageToken)
		}
		body, err := p.client.Do(ctx, http.MethodGet, reqURL, nil)
		if err != nil {
			return nil, err
		}
		for _, item := range listItems(body, ty.ListField) {
			if googleDefinedMetric(ty, item) {
				continue
			}
			id, err := ProviderID(ty, item, scope)
			if err != nil {
				// Reported, not dropped in silence: a listed resource whose
				// identity cannot be expressed is a gap in the catalog, and
				// an unreported one looks like a project that does not have it.
				fmt.Fprintf(os.Stderr, "gcp: discover: %s: a listed resource has no usable id (%v)\n", ty.Name, err)
				continue
			}
			owned, reason := SystemOwned(ty, item)
			out = append(out, provider.DiscoveredResource{
				Type:              ty.Name,
				ProviderID:        id,
				Attributes:        listedAttributes(ty, id, item),
				SystemOwned:       owned,
				SystemOwnedReason: reason,
			})
		}
		pageToken, _ = body["nextPageToken"].(string)
		if pageToken == "" {
			return out, nil
		}
		// The same token twice is a page that will come round for ever.
		if seen[pageToken] {
			return nil, fmt.Errorf("%s: the list returned the same page token twice; stopping", ty.Name)
		}
		seen[pageToken] = true
	}
}

// listedAttributes are a listed resource's attributes as Read would report
// them: the response body translated out of GCP's wire names into the schema
// names configuration and state use (names.go), with the scoping attributes
// the id's own hierarchy encodes merged in underneath.
//
// The merge order is stateFrom's, and for the same reason: most GCP APIs
// never echo project/region/zone back in a body at all, so without the id's
// own segments an adopted resource would show them unset and every plan
// afterwards would propose setting them. They go UNDERNEATH, because where
// the body does carry one it is the authority.
//
// NOT reconciled against a reference (see reconcile.go): reconciliation
// expresses an answer in the shape of a state that already exists, and a
// discovered resource is by definition one nothing holds a state for yet.
func listedAttributes(ty *catalog.Type, id string, body map[string]any) map[string]value.Value {
	attrs := idAttributes(ty, id)
	for k, v := range schemaAttrs(ty.Attributes, body) {
		attrs[k] = v
	}
	return attrs
}

// idAttributes recovers what a provider id's own hierarchy encodes, keyed by
// schema name. An id that does not parse yields an empty map rather than an
// error: on the discovery path the id came from GCP itself, so failing to
// decompose it is a reason to report less, never a reason to report nothing.
func idAttributes(ty *catalog.Type, id string) map[string]value.Value {
	parsed, err := ParseProviderID(ty, id)
	if err != nil {
		return map[string]value.Value{}
	}
	// Declared attributes only: the host refuses a discovered resource
	// carrying one its own schema does not declare, and fails the whole
	// discovery walk for that instance rather than dropping the attribute.
	// See declaredIDAttrs.
	return declaredIDAttrs(ty.Attributes, parsed)
}

// scopeAttrs are the values a collection url template can need that are not
// part of any one resource: the project being scanned, and the instance's
// own default region and zone.
//
// A type whose collection is regional or zonal cannot be listed without one,
// and an instance that configured neither gets an error from ExpandURL
// naming the attribute -- which discoverViaList reports and skips past. That
// is the right failure: expanding a missing zone to "" builds
// "projects/p/zones//instances", which GCP answers with a confusing 404.
func (p *Provider) scopeAttrs(project string) map[string]value.Value {
	out := map[string]value.Value{
		"project": value.String(project, value.SourceProvider),
	}
	if p.settings.Region != "" {
		out["region"] = value.String(p.settings.Region, value.SourceProvider)
		// Several APIs spell the same axis "location". Offered as well as,
		// never instead of: a template asks for exactly one of them.
		out["location"] = value.String(p.settings.Region, value.SourceProvider)
	}
	if p.settings.Zone != "" {
		out["zone"] = value.String(p.settings.Zone, value.SourceProvider)
	}
	return out
}

// withScope returns attrs with the instance's own scope -- its project, and
// whichever of the region/location/zone axes it configured -- filled in
// wherever attrs does not already carry a known value for that name.
//
// THIS IS WHAT MAKES A CREATE POSSIBLE AT ALL, and it is not an optimisation.
// 230 of the 233 shipped types have "{{project}}" in the url they are created
// at, and only 3 declare `project` as an attribute -- the generator takes a
// type's attributes from its API body schema, and a project is a path
// segment, never a body field. So the project cannot come from the resource
// block (infrena refuses configuration naming an attribute the schema does
// not declare, and an instance `defaults:` entry for an undeclared attribute
// is silently skipped -- compiler/schema.go's applyInstanceDefaults). It has
// to come from the provider instance, which is where a user writes it once:
//
//	providers:
//	  - plugin: gcp
//	    project: my-project
//	    region: us-central1
//
// Read, Update and Delete need no such thing: their url is rebuilt from the
// provider id, which already carries every segment (ParseProviderID). Create
// is the one call with nothing but configuration to go on.
//
// A value already in attrs WINS, so a type that does declare `project` (three
// do) still uses what the resource itself says. An unset or empty setting
// fills in nothing: expanding "{{project}}" to "" would send
// "projects//global/firewalls", which GCP answers with a 404 naming nothing,
// rather than an error naming what nobody configured.
func (p *Provider) withScope(attrs map[string]value.Value) map[string]value.Value {
	out := make(map[string]value.Value, len(attrs)+4)
	for k, v := range attrs {
		out[k] = v
	}
	for k, v := range p.scopeAttrs(p.settings.Project) {
		if s, _ := v.Raw.(string); s == "" {
			continue
		}
		if have, ok := out[k]; ok && have.Known {
			continue
		}
		out[k] = v
	}
	return out
}

// assetTypeIndex groups the catalog by CAI asset type. A LIST per asset
// type, never one type: see discoveredFromAsset for what a flat map would
// cost. The slice is sorted by type name so a warning about an ambiguous
// result names its candidates in the same order every run.
func (p *Provider) assetTypeIndex() map[string][]*catalog.Type {
	out := make(map[string][]*catalog.Type, len(p.catalog.Types))
	for _, ty := range p.catalog.Types {
		if ty.AssetType == "" {
			continue
		}
		out[ty.AssetType] = append(out[ty.AssetType], ty)
	}
	for _, types := range out {
		sort.Slice(types, func(i, j int) bool { return types[i].Name < types[j].Name })
	}
	return out
}

// assetTypesFor is the set of CAI asset types the named catalog types are
// served by, sorted, for the search's own assetTypes filter. Empty when
// nothing was named, which asks CAI for everything.
func assetTypesFor(byAsset map[string][]*catalog.Type, wanted map[string]bool) []string {
	if len(wanted) == 0 {
		return nil
	}
	var out []string
	for assetType, types := range byAsset {
		for _, ty := range types {
			if wanted[ty.Name] {
				out = append(out, assetType)
				break
			}
		}
	}
	sort.Strings(out)
	return out
}

// nameSet turns a type filter into a set. nil for an empty filter, which
// every caller reads as "everything".
func nameSet(names []string) map[string]bool {
	if len(names) == 0 {
		return nil
	}
	out := make(map[string]bool, len(names))
	for _, n := range names {
		out[n] = true
	}
	return out
}

// typeNames is the names of types, for a message.
func typeNames(types []*catalog.Type) []string {
	out := make([]string, len(types))
	for i, ty := range types {
		out[i] = ty.Name
	}
	return out
}

// relativeResourceName strips the "//<service>/" head off a Cloud Asset
// Inventory full resource name, leaving the relative resource name that is
// this provider's own id (spec §5.5). A name that does not carry that head
// is returned unchanged -- it is already relative.
func relativeResourceName(full string) string {
	rest, ok := strings.CutPrefix(full, "//")
	if !ok {
		return full
	}
	if i := strings.IndexByte(rest, '/'); i >= 0 {
		return rest[i+1:]
	}
	return ""
}

// firstPathSegment is the leading "/"-separated segment of s -- the
// hierarchy root of a relative resource name ("projects", "folders",
// "organizations", "billingAccounts").
func firstPathSegment(s string) string {
	if i := strings.IndexByte(s, '/'); i >= 0 {
		return s[:i]
	}
	return s
}

// userMetricPrefixes are the metric types a project defines itself. Every
// other metric descriptor a project lists is Google's -- 8,999 of them on
// the live project, one per built-in metric, the same in every project.
var userMetricPrefixes = []string{
	"custom.googleapis.com/", "external.googleapis.com/", "workload.googleapis.com/",
	"logging.googleapis.com/user/", "prometheus.googleapis.com/",
}

// googleDefinedMetric reports a listed metric descriptor Google defines. It is
// not a resource of the project at all, so discovery does not offer it:
// reported, the 8,999 of them made one discover reply larger than the host
// reads (16 MB), and the host then waited on the plugin for ever (live run,
// 2026-09-25). Decided by the descriptor's own `type`, as SystemOwned decides
// by the body.
func googleDefinedMetric(ty *catalog.Type, item map[string]any) bool {
	if ty.ListField != "metricDescriptors" {
		return false
	}
	metric, _ := item["type"].(string)
	for _, prefix := range userMetricPrefixes {
		if strings.HasPrefix(metric, prefix) {
			return false
		}
	}
	return true
}
