package gcprov

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
)

// ResourceManagerBaseURL is where Cloud Resource Manager lives, from its own
// Discovery document's rootUrl. A field on Settings overrides it for a test
// the same way AssetInventoryBaseURL does, and for the same reason: the
// project lookup below is about the INSTANCE, not about any one type, so
// there is no catalog entry whose APIBaseURL a test could rewrite to reach a
// fake.
const ResourceManagerBaseURL = "https://cloudresourcemanager.googleapis.com/"

// ProjectAliases is the pair of spellings one project answers to: the id a
// user writes and the number GCP canonicalises it to.
//
// GCP accepts "projects/example-project-1234" and answers
// "projects/123456789012". Same project, canonical form, different string --
// and for an attribute that is ForceNew, that difference makes every plan
// after a successful apply propose destroying and recreating the resource.
// It never converges.
//
// The zero value knows nothing and treats the two as different, which is the
// behaviour before this existed and the safe answer when the number cannot
// be resolved: a spurious replacement plan a user can see beats a silent
// wrong equality.
type ProjectAliases struct {
	ID     string
	Number string
}

// sameProject reports whether two project RESOURCE NAMES name one project.
//
// The pair must be exactly the {id, number} this instance resolved. Nothing
// wider: letting "123456789012" stand in for an arbitrary id would be an
// assumption about a project this provider never looked up.
func (a ProjectAliases) sameProject(want, got string) bool {
	if a.ID == "" || a.Number == "" {
		return false
	}
	want, got = strings.TrimPrefix(want, "projects/"), strings.TrimPrefix(got, "projects/")
	return (want == a.ID && got == a.Number) || (want == a.Number && got == a.ID)
}

// looksLikeAProjectName reports whether a string is a project RESOURCE NAME
// -- "projects/<x>", one segment after the collection.
//
// It is the gate on resolving the number at all, and it is deliberately
// narrow. A bare "example-project-1234" in some attribute is
// indistinguishable from any other identifier, and treating every differing
// pair of short strings as a candidate would send a Cloud Resource Manager
// request for every resource this provider reads. GCP's canonicalisation
// shows up in resource NAMES, which carry the collection in front, and that
// is the shape this handles. An attribute holding a bare project id that
// comes back as a bare number is NOT covered; it has not been observed, and
// inventing coverage for it would cost a request per read.
func looksLikeAProjectName(s string) bool {
	rest, ok := strings.CutPrefix(s, "projects/")
	return ok && rest != "" && !strings.Contains(rest, "/")
}

// projectAliases resolves this instance's project number once and caches it.
//
// ONCE, on the provider, because it is a fact about the instance and not
// about any resource: cloudresourcemanager's projects.get answers with
// `name: "projects/<number>"` for a project addressed by its id, and that
// answer cannot change for the life of the process.
//
// A failure is reported on stderr, once, and leaves the aliases empty --
// which makes the two spellings unequal, so the plan proposes a replacement
// the user can see and ask about. The alternative, assuming they are equal
// because the lookup failed, would silently accept a state naming a
// different project.
func (p *Provider) projectAliases(ctx context.Context) ProjectAliases {
	if p.settings.Project == "" {
		return ProjectAliases{}
	}
	p.projectOnce.Do(func() {
		base := p.settings.ResourceManagerBaseURL
		if base == "" {
			base = ResourceManagerBaseURL
		}
		body, err := p.client.Do(ctx, http.MethodGet,
			base+"v3/projects/"+p.settings.Project, nil)
		if err != nil {
			fmt.Fprintf(os.Stderr, "gcp: could not resolve the number of project %q (%v); "+
				"a resource GCP answers with projects/<number> where the configuration wrote "+
				"projects/<id> will be reported as changed\n", p.settings.Project, err)
			return
		}
		name, _ := body["name"].(string)
		number := strings.TrimPrefix(name, "projects/")
		if number == "" || number == p.settings.Project {
			// Either the project was addressed by its number to begin with,
			// or the answer carries no name. Either way there is no second
			// spelling to learn, and there is nothing wrong.
			return
		}
		p.projects = ProjectAliases{ID: p.settings.Project, Number: number}
	})
	return p.projects
}

// reconciler is the provider's own, carrying the project aliases so that
// every state this instance produces knows the two spellings are one
// project. THE ONLY WAY ReconcileAttrs IS REACHED FROM PRODUCTION CODE: a
// path that used the bare package function would silently reconcile without
// them, which is the shape of defect a field with no reader has.
func (p *Provider) reconciler(ctx context.Context) reconciler {
	// A FUNCTION, not the value. Resolving the number costs a Cloud Resource
	// Manager request, and the great majority of reads have no project
	// resource name in them to disagree about -- so the lookup happens the
	// first time a reconciliation actually sees two different "projects/<x>"
	// spellings, and never otherwise. sync.Once behind it keeps it to one
	// request for the life of the provider either way.
	return reconciler{aliases: func() ProjectAliases { return p.projectAliases(ctx) }}
}

// projectState is the per-provider cache projectAliases fills. Embedded in
// Provider (client.go) rather than declared there so the whole mechanism
// reads in one file.
type projectState struct {
	projectOnce sync.Once
	projects    ProjectAliases
}
