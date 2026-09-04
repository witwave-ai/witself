package version

import "testing"

func TestDevelopmentFallback(t *testing.T) {
	if Version != "dev" || Commit != "none" || Date != "unknown" {
		t.Fatalf("development version identity = (%q, %q, %q); want (%q, %q, %q)",
			Version, Commit, Date, "dev", "none", "unknown")
	}
	if got, want := String("witself"), "witself dev (commit none, built unknown)"; got != want {
		t.Fatalf("String() development fallback = %q; want %q", got, want)
	}
}

func TestStringFormatsStampedIdentity(t *testing.T) {
	originalVersion, originalCommit, originalDate := Version, Commit, Date
	t.Cleanup(func() {
		Version, Commit, Date = originalVersion, originalCommit, originalDate
	})

	Version = "1.2.3"
	Commit = "0123abc"
	Date = "2026-09-04T12:34:56Z"
	if got, want := String("witself-server"), "witself-server 1.2.3 (commit 0123abc, built 2026-09-04T12:34:56Z)"; got != want {
		t.Fatalf("String() = %q; want %q", got, want)
	}
}
