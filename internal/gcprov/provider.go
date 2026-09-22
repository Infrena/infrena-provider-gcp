package gcprov

import (
	"github.com/infrena/infrena-provider-gcp/internal/catalog"
	"github.com/infrena/infrena/pkg/provider"
	"github.com/infrena/infrena/pkg/schema"
)

// NewProvider builds one configured GCP provider instance from its own
// resolved Settings, the catalog it serves, and the Client that talks to
// GCP.
//
// It deliberately does NOT take a *gcpplugin.Instance. Decision P2 is that
// gcpplugin imports gcprov and never the reverse -- gcpplugin.Plugin.New
// (Task 17) calls NewProvider to build the provider.Provider it returns, so
// a *gcpplugin.Instance parameter here would be a compile-breaking import
// cycle. gcpplugin converts its own Instance into a Settings at that one
// call site instead.
func NewProvider(s Settings, c *catalog.Catalog, cl *Client) *Provider {
	return &Provider{
		client:   cl,
		catalog:  c,
		settings: s,
	}
}

// ProviderName is this plugin's name, and the prefix of every type it
// serves.
//
// It lives HERE rather than in gcpplugin, which is the package that is
// otherwise about plugin identity, because provider.Provider.Name is
// implemented here and gcprov must not import gcpplugin (decision P2, see
// NewProvider). gcpplugin.PluginName is defined from this constant, so the
// name still has exactly one source.
const ProviderName = "gcp"

// Provider answers the whole of provider.Provider. Asserted here because
// nothing else can: the interface is satisfied by methods spread over
// crud.go, patch.go, discover.go and this file, so a missing one shows up as
// a compile error at gcpplugin.New's return statement -- a long way from the
// method that was never written.
var _ provider.Provider = (*Provider)(nil)

// Name is the provider's name, as the host knows it.
func (p *Provider) Name() string { return ProviderName }

// Definitions are the resource types this instance serves: the catalog's,
// which is the same catalog gcpplugin.Plugin.Definitions already sent during
// the handshake. The host asks the PLUGIN for schemas and never asks an
// instance, so this exists to satisfy the interface and must answer the same
// thing rather than something narrower.
func (p *Provider) Definitions() []*schema.ResourceDefinition { return p.catalog.Definitions() }

// ClassifyError says whether the operation that produced err may be retried.
// The package function is the implementation; this is the method the host
// calls. It takes nothing from the instance, so the two cannot disagree.
func (p *Provider) ClassifyError(err error) provider.Retryability { return ClassifyError(err) }
