// release-tool creates and audits distribution files without Python or tar.
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/iaia/telegramgw/internal/distribution"
	"github.com/iaia/telegramgw/internal/releaseauth"
)

func run(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: release-tool package|manifest|verify [options]")
	}
	options := flag.NewFlagSet(args[0], flag.ContinueOnError)
	root := options.String("root", ".", "repository root")
	dist := options.String("dist", "dist", "distribution directory")
	binary := options.String("binary", "", "component executable")
	targetOS := options.String("os", "", "target OS")
	arch := options.String("arch", "", "target architecture")
	repo := options.String("repo", "", "release repository")
	tag := options.String("tag", "", "release tag")
	if err := options.Parse(args[1:]); err != nil {
		return err
	}
	if options.NArg() != 0 {
		return fmt.Errorf("unexpected arguments: %v", options.Args())
	}
	switch args[0] {
	case "verify-provenance":
		manifest, err := os.ReadFile(filepath.Join(*dist, "SHA256SUMS"))
		if err != nil {
			return err
		}
		bundle, err := os.ReadFile(filepath.Join(*dist, releaseauth.BundleName))
		if err != nil {
			return err
		}
		return releaseauth.Verify(*repo, *tag, manifest, bundle)
	case "package":
		return distribution.Package(*root, *dist, *binary, distribution.Target{OS: *targetOS, Arch: *arch})
	case "manifest":
		return distribution.WriteManifest(*dist)
	case "verify":
		if err := distribution.Verify(*root, *dist); err != nil {
			return err
		}
		fmt.Println("Verified ten platform archives, four native managers and installer.")
		return nil
	default:
		return fmt.Errorf("unknown release-tool command: %s", args[0])
	}
}

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
