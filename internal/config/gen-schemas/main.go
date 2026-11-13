// Command gen-schemas writes the config JSON Schemas into a directory.
package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/fl0wo/docker-offload-lan/internal/config"
)

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: gen-schemas <dir>")
		os.Exit(2)
	}
	s, err := config.Schemas()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	for name, b := range s {
		//nolint:gosec // schema files are public and meant to be world-readable
		if err := os.WriteFile(filepath.Join(os.Args[1], name), b, 0o644); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	}
}
