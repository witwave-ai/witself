// Package version exposes the build identity (version, commit, date) shared by
// both binaries (cmd/witself and cmd/witself-server). The values default to
// "dev" and are overridden at build time via -ldflags.
package version

import "fmt"

// These values are set at build time with -ldflags, for example:
//
//	-ldflags "-X github.com/witwave-ai/witself/internal/version.Version=v0.1.0"
//
// They default to "dev" for local and untagged builds.
var (
	// Version is the release version (e.g. "v0.1.0").
	Version = "dev"
	// Commit is the git commit hash the binary was built from.
	Commit = "dev"
	// Date is the build date (RFC3339 or similar).
	Date = "dev"
)

// String returns a single formatted version line suitable for printing from a
// version command.
func String() string {
	return fmt.Sprintf("witself %s (commit %s, built %s)", Version, Commit, Date)
}
