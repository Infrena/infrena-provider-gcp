// Package gcpplugin is infrena's GCP provider plugin: the plugin surface,
// instance configuration and credentials around the generic provider.
package gcpplugin

import (
	"fmt"
	"os"

	"github.com/infrena/infrena-provider-gcp/internal/catalog"
	"github.com/infrena/infrena/pkg/provider"
	"github.com/infrena/infrena/pkg/schema"
)

// PluginName is the binary's suffix, what `plugin:` names, and every type's prefix.
const PluginName = "gcp"

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
func (pl *Plugin) New(cfg provider.Config) (provider.Provider, error) {
	return nil, fmt.Errorf("gcp: not implemented until the catalog is wired")
}
