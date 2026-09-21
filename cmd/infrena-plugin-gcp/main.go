// Command infrena-plugin-gcp is infrena's GCP provider plugin.
//
// It is run by infrena, not directly: stdin and stdout are the protocol stream.
package main

import (
	"github.com/infrena/infrena-provider-gcp/internal/gcpplugin"
	"github.com/infrena/infrena/pkg/pluginsdk"
)

func main() { pluginsdk.Main(gcpplugin.NewPlugin()) }
