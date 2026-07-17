package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/witwave-ai/witself/internal/runtimeacceptance"
)

func TestAcceptanceEvidenceOutputIsPrivate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "evidence", "runtime.json")
	if err := writeAcceptanceEvidence(path, []byte("{}\n")); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("evidence mode = %o, want 600", info.Mode().Perm())
	}
}

func TestAcceptanceCurationRequestIsDeterministicAcrossResume(t *testing.T) {
	preparedAt := time.Date(2026, 7, 16, 12, 0, 0, 123, time.FixedZone("test", -6*60*60))
	state := runtimeacceptance.RunState{
		RunID: "mra_test", PreparedAt: preparedAt,
		RetryKeys: runtimeacceptance.RetryKeys{CurationRequest: "retry-test"},
	}
	first := acceptanceCurationRequestInput(state)
	second := acceptanceCurationRequestInput(state)
	if first.DueAt == nil || second.DueAt == nil || !first.DueAt.Equal(preparedAt.UTC()) || !first.DueAt.Equal(*second.DueAt) {
		t.Fatalf("nondeterministic due_at: first=%v second=%v", first.DueAt, second.DueAt)
	}
	if first.IdempotencyKey != second.IdempotencyKey || first.CoalescingKey != second.CoalescingKey {
		t.Fatalf("nondeterministic request identity: first=%#v second=%#v", first, second)
	}
}

func TestAcceptanceReleaseBuildRequiresExactReleaseIdentity(t *testing.T) {
	valid := runtimeacceptance.Build{
		Version: "0.0.172", Commit: "abcdef1", Date: "2026-07-16T12:00:00Z",
	}
	if !acceptanceReleaseBuild(valid) {
		t.Fatal("valid release build was rejected")
	}
	for _, invalid := range []runtimeacceptance.Build{
		{Version: "dev-0.0.172", Commit: valid.Commit, Date: valid.Date},
		{Version: "0.0", Commit: valid.Commit, Date: valid.Date},
		{Version: valid.Version, Commit: "none", Date: valid.Date},
		{Version: valid.Version, Commit: valid.Commit, Date: "unknown"},
	} {
		if acceptanceReleaseBuild(invalid) {
			t.Fatalf("invalid release build was accepted: %#v", invalid)
		}
	}
}

func TestAcceptanceReleaseVersionUsesExactSemVer(t *testing.T) {
	for _, value := range []string{
		"0.0.0",
		"1.2.3",
		"v1.2.3-rc.1+build.7",
		"1.0.0-0.3.7",
		"1.0.0-x.7.z.92",
		"1.0.0+001",
	} {
		if !releaseVersionPattern.MatchString(value) {
			t.Errorf("valid SemVer rejected: %q", value)
		}
	}
	for _, value := range []string{
		"01.2.3",
		"1.02.3",
		"1.2.03",
		"1.2.3-01",
		"1.2.3-alpha.01",
		"1.2.3-",
		"1.2.3+",
		"1.2.3-alpha..1",
		"1.2.3_alpha",
	} {
		if releaseVersionPattern.MatchString(value) {
			t.Errorf("invalid SemVer accepted: %q", value)
		}
	}
}

func TestAcceptanceReleasePairPinsVersionAndCommit(t *testing.T) {
	server := runtimeacceptance.Build{Version: "0.0.172", Commit: "abcdef1", Date: "2026-07-16T12:00:00Z"}
	cli := runtimeacceptance.Build{Version: server.Version, Commit: server.Commit, Date: "2026-07-16T12:01:00Z"}
	if !acceptanceReleasePair(server, cli) {
		t.Fatal("matching release pair was rejected")
	}
	cli.Commit = "abcdef2"
	if acceptanceReleasePair(server, cli) {
		t.Fatal("mismatched release pair was accepted")
	}
}
