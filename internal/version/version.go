// Package version holds build information set with -ldflags.
package version

// Version and Commit are set at build time.
var (
	Version = "dev"
	Commit  = "unknown"
)

// String returns "version (commit)".
func String() string { return Version + " (" + Commit + ")" }
