// Package gcpplugin is infrena's GCP provider plugin: the plugin surface,
// instance configuration and credentials around the generic provider.
package gcpplugin

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"strings"

	"golang.org/x/oauth2"

	"github.com/infrena/infrena-provider-gcp/internal/catalog"
	"github.com/infrena/infrena-provider-gcp/internal/gcprov"
	"github.com/infrena/infrena/pkg/provider"
	"github.com/infrena/infrena/pkg/schema"
)

// PluginName is the binary's suffix, what `plugin:` names, and every type's prefix.
//
// Defined from gcprov.ProviderName rather than written again, because
// provider.Provider.Name is implemented in gcprov (which cannot import this
// package -- decision P2) and two independently spelled copies of a name the
// host matches on would rot apart silently.
const PluginName = gcprov.ProviderName

// Version is reported in the handshake. "0.0.0-dev" in every build a release did not stamp, so a
// broken -ldflags path cannot pass the release gate by coincidence.
var Version = "0.0.0-dev"

// Plugin is the GCP provider before configuration.
type Plugin struct{}

// NewPlugin returns the GCP plugin.
func NewPlugin() *Plugin { return &Plugin{} }

var _ provider.Plugin = (*Plugin)(nil)

// Name is the plugin's name.
func (pl *Plugin) Name() string { return PluginName }

// Version reports this build's version. The SDK detects this optional method.
func (pl *Plugin) Version() string { return Version }

// Definitions are the resource types this plugin offers.
//
// Loaded once and cached by the catalog package. A failure here cannot be
// returned (the interface has no error), so it is reported on stderr and the
// plugin serves nothing, which the host reports as a plugin that offers no
// types rather than as a crash.
func (pl *Plugin) Definitions() []*schema.ResourceDefinition {
	c, err := catalog.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "gcp: the embedded catalog will not load: %v\n", err)
		return nil
	}
	return c.Definitions()
}

// New constructs one instance from its resolved configuration.
//
// The token source is resolved HERE rather than lazily on the first call, so
// a project configured against credentials that do not exist fails while
// infrena is still starting plugins, with the instance named, instead of
// failing once per resource in the middle of an apply.
//
// context.Background(), because provider.Plugin.New takes no context and the
// token source outlives this call regardless: oauth2.ReuseTokenSource keeps
// it and refreshes against it an hour later. credentials.go's
// impersonationTimeout is what bounds those refreshes, and it is deliberately
// independent of this context for exactly that reason -- a deadline created
// here would have expired long before the refresh that needed it. See the
// comment on impersonationTimeout.
func (pl *Plugin) New(cfg provider.Config) (provider.Provider, error) {
	in, err := ParseConfig(cfg)
	if err != nil {
		return nil, err
	}
	c, err := catalog.Load()
	if err != nil {
		return nil, fmt.Errorf("gcp: the embedded catalog will not load: %w", err)
	}
	settings := gcprov.Settings{
		Project:          in.Project,
		Region:           in.Region,
		Zone:             in.Zone,
		DiscoverTypes:    in.DiscoverTypes,
		DiscoverProjects: in.DiscoverProjects,
	}

	if host := os.Getenv(EndpointOverrideEnv); host != "" {
		redirected, ts, err := redirect(c, host, cfg.Instance)
		if err != nil {
			return nil, err
		}
		settings.AssetInventoryBaseURL = rehost(host, gcprov.CloudAssetBaseURL)
		return gcprov.NewProvider(settings, redirected, gcprov.NewClient(ts, settings.AssetInventoryBaseURL, gcprov.ClientOptions{
			QuotaProject: in.QuotaProject,
		})), nil
	}

	ts, err := in.TokenSource(context.Background())
	if err != nil {
		// Not re-wrapped with the instance name: every error TokenSource
		// returns already carries it, and wrapping again produced
		// `provider "main": provider "main": credentials_file: ...`.
		return nil, fmt.Errorf("gcp: %w", err)
	}
	return gcprov.NewProvider(settings, c, gcprov.NewClient(ts, "", gcprov.ClientOptions{
		QuotaProject: in.QuotaProject,
	})), nil
}

// EndpointOverrideEnv points every GCP endpoint this plugin calls at another
// host. IT IS FOR TESTS, and for nothing else.
//
// The e2e suite runs a real infrena binary against a real build of this
// plugin and an in-process fake GCP. infrena starts a plugin as a child
// process with its own environment inherited (pluginhost's connect.go passes
// os.Environ through), so an environment variable is the only channel a test
// on the other side of that boundary has: nothing in `providers:` reaches a
// plugin except the keys ParseConfig accepts, and adding a `base_url:` key
// there would be a documented, supported way to point a production apply at
// somebody else's endpoint.
//
// Three things keep it honest. The override replaces the api host of every
// catalog type AND Cloud Asset Inventory, so nothing is left pointing at
// Google by accident. Real credentials are never resolved when it is set --
// a static placeholder token is used instead, so a redirected endpoint
// cannot be handed a token minted for the real cloud. And it announces
// itself on stderr, which infrena forwards to the user's terminal, so a
// machine that has it set by accident says so on every single run rather
// than quietly talking to the wrong cloud.
//
// Name chosen to read like gcloud's own CLOUDSDK_API_ENDPOINT_OVERRIDES_*
// family rather than like something private to this repository, because the
// person who finds it set on a CI runner will search for it.
const EndpointOverrideEnv = "GOOGLE_API_ENDPOINT_OVERRIDE"

// overrideToken is what an instance authenticates with while
// EndpointOverrideEnv is in force: a fixed, meaningless string. It is not a
// credential and cannot be mistaken for one.
const overrideToken = "endpoint-override-no-credentials"

// redirect returns a copy of c with every type's api HOST replaced by host,
// and the token source such an instance uses.
//
// THE HOST ONLY. Each API's own path is kept, so compute's
// "https://compute.googleapis.com/compute/v1/" becomes
// "http://127.0.0.1:PORT/compute/v1/" and not "http://127.0.0.1:PORT/". That
// is not tidiness: a self link GCP answers with is reduced to a provider id
// by stripping exactly that path (reduceSelfLink), so a base with the path
// flattened away leaves every self link unreducible -- "no api prefix to
// reduce ... by" -- and every discovered compute resource loses its
// identity. Keeping the path also means the fake sees the paths the real API
// would, which is the point of faking the cloud rather than the code.
//
// A COPY, never a mutation of c in place: catalog.Load caches one instance
// for the whole process, so rewriting it directly would leak this instance's
// endpoint into every other instance the same plugin process serves.
func redirect(c *catalog.Catalog, host, instance string) (*catalog.Catalog, oauth2.TokenSource, error) {
	u, err := url.Parse(host)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, nil, fmt.Errorf("gcp: provider %q: %s must be an absolute http(s) url, got %q",
			instance, EndpointOverrideEnv, host)
	}
	fmt.Fprintf(os.Stderr, "gcp: %s is set: provider %q talks to %s://%s, NOT to Google, "+
		"and sends no real credentials. This is a test-only setting.\n",
		EndpointOverrideEnv, instance, u.Scheme, u.Host)

	types := make([]*catalog.Type, len(c.Types))
	for i, t := range c.Types {
		cp := *t
		cp.APIBaseURL = rehost(host, t.APIBaseURL)
		// Cleared, never rehosted: a regional endpoint template is a Google
		// host with a placeholder in it, and the override's one promise is
		// that nothing reaches the real cloud. Every request then goes to
		// the rehosted APIBaseURL.
		cp.EndpointTemplate = ""
		types[i] = &cp
	}
	return &catalog.Catalog{
		Generated:       c.Generated,
		MMV1Commit:      c.MMV1Commit,
		Types:           types,
		DiscoverDefault: c.DiscoverDefault,
	}, oauth2.StaticTokenSource(&oauth2.Token{AccessToken: overrideToken, TokenType: "Bearer"}), nil
}

// rehost returns original with its scheme and host replaced by host's,
// keeping original's path. An original that will not parse is replaced
// wholesale rather than left pointing at Google: the override's one promise
// is that nothing reaches the real cloud.
func rehost(host, original string) string {
	base, err := url.Parse(host)
	if err != nil {
		return host
	}
	u, err := url.Parse(original)
	if err != nil {
		return strings.TrimSuffix(host, "/") + "/"
	}
	u.Scheme, u.Host = base.Scheme, base.Host
	out := u.String()
	if !strings.HasSuffix(out, "/") {
		out += "/"
	}
	return out
}

// MaxConcurrency is how many operations infrena may run against this plugin at
// once (plugin protocol 5). It REPLACES the host's per-provider default of 8.
//
// The number is derived, not chosen. GCP quota is per API per project per
// minute, and the common infrastructure APIs allow well over a thousand
// requests a minute. One infrena operation is at most one simultaneous
// request: crud.go sends a single mutation and await polls one call at a
// time, sleeping between polls, so N concurrent operations peak at N
// simultaneous requests.
//
// 16 is twice the host's default and a small fraction of what a project
// allows, which is the point: the quota belongs to the PROJECT, not to this
// process. A colleague's apply, a CI run and the console all draw on the same
// budget.
//
// Task 18 measures the real headroom on a live project. If that measurement
// disagrees with this number, change the number and this comment together.
func (pl *Plugin) MaxConcurrency() int { return 16 }
