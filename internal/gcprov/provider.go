package gcprov

import (
	"github.com/infrena/infrena-provider-gcp/internal/catalog"
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
