// Command gen-gcp regenerates internal/catalog/catalog.json.gz and gen/warnings.txt.
//
// Run it by hand and commit the diff. Nothing runs it at build time.
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/infrena/infrena-provider-gcp/internal/catalog"
	"github.com/infrena/infrena-provider-gcp/internal/gen"
)

func main() {
	in := gen.Inputs{}
	flag.StringVar(&in.SchemaDir, "schemas", "schemas", "Discovery documents")
	flag.StringVar(&in.MMV1Dir, "mmv1", "gen/mmv1/products", "vendored magic-modules products")
	flag.StringVar(&in.OverlayPath, "overlay", "gen/overlay.yaml", "the curated overlay")
	flag.StringVar(&in.LockPath, "lock", "gen/names.lock.json", "the append-only name lock")
	out := flag.String("out", "internal/catalog/catalog.json.gz", "where to write the catalog")
	warn := flag.String("warnings", "gen/warnings.txt", "where to write the warnings")
	flag.Parse()

	res, err := gen.Build(in)
	if err != nil {
		fmt.Fprintf(os.Stderr, "gen-gcp: %v\n", err)
		os.Exit(1)
	}
	blob, err := catalog.Encode(res.Catalog)
	if err != nil {
		fmt.Fprintf(os.Stderr, "gen-gcp: %v\n", err)
		os.Exit(1)
	}
	if err := os.WriteFile(*out, blob, 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "gen-gcp: %v\n", err)
		os.Exit(1)
	}
	if err := gen.WriteWarnings(*warn, res.Warnings, res.Uncreatable, res.Unpatchable); err != nil {
		fmt.Fprintf(os.Stderr, "gen-gcp: %v\n", err)
		os.Exit(1)
	}
	var tier2 int
	for _, w := range res.Warnings {
		if w.Tier == gen.TierHooked {
			tier2++
		}
	}
	fmt.Fprintf(os.Stderr, "%d types; %d refused (%d awaiting a ruling); %d ship without create; %d replaced not patched\n",
		len(res.Catalog.Types), len(res.Warnings), tier2, len(res.Uncreatable), len(res.Unpatchable))
}
