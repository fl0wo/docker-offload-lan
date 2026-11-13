// Package version exposes build metadata, set via -ldflags by GoReleaser.
package version

import "runtime/debug"

var (
	// Version is the semantic version, e.g. "0.1.0". "dev" for local builds.
	Version = "dev"
	// Commit is the git commit the binary was built from.
	Commit = ""
)

// String returns "version (commit)".
func String() string {
	c := Commit
	if c == "" {
		if bi, ok := debug.ReadBuildInfo(); ok {
			for _, s := range bi.Settings {
				if s.Key == "vcs.revision" && len(s.Value) >= 12 {
					c = s.Value[:12]
				}
			}
		}
	}
	if c == "" {
		return Version
	}
	return Version + " (" + c + ")"
}
