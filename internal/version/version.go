// Package version exposes the build version of the Witself binaries. The values
// are overridden at release time with -ldflags; see .goreleaser.yaml.
package version

var (
	// Version is the semantic version, e.g. "0.0.1". "dev" for local builds.
	Version = "dev"
	// Commit is the short git commit the binary was built from.
	Commit = "none"
	// Date is the build timestamp.
	Date = "unknown"
)

// String returns a human-readable one-line version string for the named program.
func String(prog string) string {
	return prog + " " + Version + " (commit " + Commit + ", built " + Date + ")"
}
