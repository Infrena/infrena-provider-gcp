package gcprov

import (
	"fmt"
	"sort"
	"strings"

	"github.com/infrena/infrena-provider-gcp/internal/catalog"
)

// googleLabelPrefix is the label-key prefix Google reserves for labels its
// own services apply. "goog-gke-node" on a GKE node, "goog-composer-*" on an
// Airflow environment, "goog-dataproc-*" on a Dataproc VM: none of them were
// written by a user, and all of them mark a resource some other Google
// service creates and reconciles.
const googleLabelPrefix = "goog-"

// cnrmLabel is Config Connector's own marker. A resource carrying it is
// already managed by a controller running in a cluster, so adopting it here
// means two systems reconciling one resource against two different desired
// states.
const cnrmLabel = "managed-by-cnrm"

// defaultAllowRules are the firewall rules Google creates alongside an auto
// mode network, by name. A CLOSED SET, not the "default-allow" prefix:
// Google creates these four and no others, so a rule a user named
// "default-allow-from-office" is theirs and matching the prefix would take
// it out of their hands on the strength of a naming habit. The cost of the
// closed set is that a fifth rule Google adds later goes unflagged until
// this list gains it, which is the right way round -- an unflagged resource
// is merely offered for import, a wrongly flagged one is withheld.

// SystemOwned reports whether body describes a resource Google created and
// manages rather than one a user asked for, and says WHY in the caller's own
// words.
//
// EVERY RULE BELOW NAMES A LITERAL SIGNAL IN body, AND THE REASON QUOTES IT.
// That is the whole design. A user reading "gcp.compute.instance x is system
// owned" has no way to judge it; a user reading "carries the label
// \"goog-gke-node\", which GKE applies to the nodes it manages" can look at
// the resource and decide for themselves whether to override it. The flag is
// advisory -- nothing refuses to import a flagged resource
// (provider.DiscoveredResource.SystemOwned) -- so the reason is the part
// that does the work.
//
// ty IS NOT CONSULTED AT ALL -- it is in the signature because every other
// entry point in this package takes one and a caller should not have to
// wonder why this one does not, and it stays unread on purpose. Deciding by
// category ("a network is system owned", "a default service account is
// system owned") is exactly the failure this avoids: the case that matters
// most is a VPC network a user created and named "default". It is theirs.
// What makes the OTHER one Google's is not its name but that it is an auto
// mode network -- autoCreateSubnetworks true -- which is what a project's
// automatically created network is and what a user creating a network called
// "default" today does not get by default. Marking on the name alone takes a
// user's own production VPC out of their control silently.
//
// body is a GCP response body, so its keys are WIRE names. Every key read
// here (name, labels, autoCreateSubnetworks) is spelled the same in both
// namespaces -- the generator renames only reserved words (see names.go) --
// so no translation is needed, and reading a wire body directly is what lets
// the Cloud Asset Inventory path, whose search results are not catalog
// attributes at all, use the same function.
func SystemOwned(ty *catalog.Type, body map[string]any) (bool, string) {
	// A full relative resource name ("projects/p/global/networks/default")
	// and a bare one both occur: a Cloud Asset Inventory result carries the
	// first, an ordinary list response usually the second. Only the last
	// segment is ever the resource's own name.
	name := lastPathSegment(body["name"])

	for _, key := range labelKeys(body) {
		switch {
		case strings.HasPrefix(key, googleLabelPrefix):
			return true, fmt.Sprintf("carries the label %q, which Google's own services apply to resources they create and manage", key)
		case key == cnrmLabel:
			return true, fmt.Sprintf("carries the label %q, so Config Connector is already reconciling it", key)
		}
	}

	// The auto mode network a project is created with. BOTH halves are
	// required: the name is not evidence on its own (see the doc comment),
	// and autoCreateSubnetworks is not either -- a user may legitimately
	// create an auto mode network of their own and call it something else.
	if name == "default" && boolField(body, "autoCreateSubnetworks") {
		return true, `named "default" and has autoCreateSubnetworks set, which is the auto mode network Google creates with a project`
	}

	if defaultAllowRules[name] {
		return true, fmt.Sprintf("named %q, one of the firewall rules Google creates alongside an auto mode network", name)
	}

	return false, ""
}

// labelKeys returns body's label keys, sorted, so a resource carrying two
// system labels is always reported with the same one -- map iteration order
// would otherwise make the reason string differ between runs for the same
// resource.
//
// Two spellings, because GCP has two. Most APIs call the field "labels";
// container's Cluster calls its own "resourceLabels" (schemas/container.json,
// checked 2026-09-22: Cluster has resourceLabels and labelFingerprint, and no
// labels at all). Reading only the first would silently never flag anything
// on a GKE cluster. Note that gcp.container.cluster's catalog attributes
// declare NEITHER field, which is exactly why this reads the response body
// rather than going through the catalog: what the API answers with is not
// limited to what the catalog models.
func labelKeys(body map[string]any) []string {
	var keys []string
	for _, field := range []string{"labels", "resourceLabels"} {
		labels, ok := body[field].(map[string]any)
		if !ok {
			continue
		}
		for k := range labels {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	return keys
}

// boolField reads a boolean out of a JSON-decoded body. A field GCP omitted
// is false, which is what an absent autoCreateSubnetworks means.
func boolField(body map[string]any, name string) bool {
	b, _ := body[name].(bool)
	return b
}

var defaultAllowRules = map[string]bool{
	"default-allow-internal": true,
	"default-allow-ssh":      true,
	"default-allow-rdp":      true,
	"default-allow-icmp":     true,
}
