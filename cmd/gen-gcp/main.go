// Command gen-gcp regenerates internal/catalog/catalog.json.gz,
// gen/warnings.txt, gen/facts.tsv, gen/unknowns.txt and gen/methods.tsv.
//
// Run it by hand and commit the diff. Nothing runs it at build time. With
// -check it writes nothing and fails if any committed output is stale. It
// needs the fetched schemas/, which are gitignored, so it is a local check:
// CI cannot regenerate against the documents the catalog was built from.
package main

import (
	"bytes"
	"flag"
	"fmt"
	"os"
	"path/filepath"

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
	facts := flag.String("facts", "gen/facts.tsv", "where to write every fact and its sources")
	unknowns := flag.String("unknowns", "gen/unknowns.txt", "where to write the facts nothing has checked")
	methods := flag.String("methods", "gen/methods.tsv", "where to write the Discovery method digest the round trip reads")
	check := flag.Bool("check", false, "write nothing; fail if a committed output is stale")
	flag.Parse()

	if !*check {
		if err := generate(in, *out, *warn, *facts, *unknowns, *methods); err != nil {
			fmt.Fprintf(os.Stderr, "gen-gcp: %v\n", err)
			os.Exit(1)
		}
		return
	}

	// Everything goes to a scratch directory, the name lock included: Build
	// saves the lock, and a check must not grow it.
	tmp, err := os.MkdirTemp("", "gen-gcp-check")
	if err != nil {
		fmt.Fprintf(os.Stderr, "gen-gcp: %v\n", err)
		os.Exit(1)
	}
	defer os.RemoveAll(tmp)
	lock, err := os.ReadFile(in.LockPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "gen-gcp: %v\n", err)
		os.Exit(1)
	}
	committedLock := in.LockPath
	in.LockPath = filepath.Join(tmp, "names.lock.json")
	if err := os.WriteFile(in.LockPath, lock, 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "gen-gcp: %v\n", err)
		os.Exit(1)
	}
	pairs := [][2]string{
		{*out, filepath.Join(tmp, "catalog.json.gz")},
		{*warn, filepath.Join(tmp, "warnings.txt")},
		{*facts, filepath.Join(tmp, "facts.tsv")},
		{*unknowns, filepath.Join(tmp, "unknowns.txt")},
		{*methods, filepath.Join(tmp, "methods.tsv")},
		{committedLock, in.LockPath},
	}
	if err := generate(in, pairs[0][1], pairs[1][1], pairs[2][1], pairs[3][1], pairs[4][1]); err != nil {
		fmt.Fprintf(os.Stderr, "gen-gcp: %v\n", err)
		os.Exit(1)
	}
	stale := 0
	for _, p := range pairs {
		have, err1 := os.ReadFile(p[0])
		want, err2 := os.ReadFile(p[1])
		if err1 != nil || err2 != nil || !bytes.Equal(have, want) {
			fmt.Fprintf(os.Stderr, "gen-gcp: %s is stale; run go run ./cmd/gen-gcp and commit the result\n", p[0])
			stale++
		}
	}
	if stale > 0 {
		os.Exit(1)
	}
	fmt.Fprintln(os.Stderr, "gen-gcp: every committed output is up to date")
}

func generate(in gen.Inputs, out, warn, facts, unknowns, methods string) error {
	res, err := gen.Build(in)
	if err != nil {
		return err
	}
	blob, err := catalog.Encode(res.Catalog)
	if err != nil {
		return err
	}
	if err := os.WriteFile(out, blob, 0o644); err != nil {
		return err
	}
	if err := gen.WriteWarnings(warn, res.Warnings, res.Uncreatable, res.Unpatchable); err != nil {
		return err
	}
	if err := gen.WriteFacts(facts, unknowns, res.Catalog); err != nil {
		return err
	}
	if err := gen.WriteMethods(in.SchemaDir, methods); err != nil {
		return err
	}
	var tier2 int
	for _, w := range res.Warnings {
		if w.Tier == gen.TierHooked {
			tier2++
		}
	}
	fmt.Fprintf(os.Stderr, "%d types; %d refused (%d awaiting a ruling); %d ship without create; %d replaced not patched\n",
		len(res.Catalog.Types), len(res.Warnings), tier2, len(res.Uncreatable), len(res.Unpatchable))
	return nil
}
