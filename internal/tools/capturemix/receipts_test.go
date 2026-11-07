package capturemix

import (
	"strings"
	"testing"

	"github.com/witwave-ai/witself/internal/syncharness"
)

func completeMixedResultFixture() Result {
	binary := func(value string) syncharness.BinaryBuildIdentity {
		return syncharness.BinaryBuildIdentity{Digest: strings.Repeat(value, 64), GoVersion: "go1.26.3", GOOS: "linux", GOARCH: "amd64"}
	}
	result := Result{Identity: syncharness.MixedBuildIdentity{
		PredecessorCommit: syncPredecessorCommit,
		CurrentServer:     binary("1"), CurrentWorker: binary("2"), PredecessorServer: binary("3"), PredecessorWorker: binary("4"),
	}}
	for _, server := range []string{"old", "new"} {
		for _, worker := range []string{"old", "new"} {
			row := RowResult{Server: server, Worker: worker, Completion: "commit_and_rollback", JobEvidence: []syncharness.MixedJobEvidence{
				{Job: "account_purge", Class: "preguard_refusal", RowsUnchanged: true},
				{Job: "agent_email_outbound", Class: "guard_cause_exact", RowsUnchanged: true},
				{Job: "agent_email_rate_bucket_cleanup", Class: "guard_cause_exact", RowsUnchanged: true},
				{Job: "agent_email_retention", Class: "guard_cause_exact", RowsUnchanged: true},
				{Job: "avatar_style_rollout", Class: "guard_cause_exact", RowsUnchanged: true},
				{Job: "message_rate_bucket_cleanup", Class: "guard_cause_exact", RowsUnchanged: true},
				{Job: "message_retention", Class: "guard_cause_exact", RowsUnchanged: true},
				{Job: "support_ticket_age_out", Class: "guard_cause_hidden", RowsUnchanged: true},
				{Job: "transcript_retention", Class: "guard_cause_exact", RowsUnchanged: true},
				{Job: "transcript_storage_reconcile", Class: "guard_cause_hidden", RowsUnchanged: true},
			}}
			for _, completion := range []string{"commit", "rollback"} {
				row.Checks = append(row.Checks, "processes_ready", "legacy_transactions_held", "installation_started", "installation_lock_observed",
					"legacy_"+completion, "guards_installed", "capture_activated", "writer_assertions", "downgrade_refused")
				for _, job := range row.JobEvidence {
					row.Checks = append(row.Checks, "capturemix job="+job.Job+" evidence="+string(job.Class))
				}
			}
			result.Rows = append(result.Rows, row)
		}
	}
	return result
}

func TestMixedRuntimeDisposableCompletionRequiresEveryRow(t *testing.T) {
	cases := map[string]func(*Result){
		"last_row_pending":   func(r *Result) { r.Rows = r.Rows[:3] },
		"last_drain_pending": func(r *Result) { r.Rows[3].Completion = "commit" },
		"failed_last_check":  func(r *Result) { r.Rows[3].Checks = r.Rows[3].Checks[:len(r.Rows[3].Checks)-1] },
		"failed_stage":       func(r *Result) { r.Rows[3].Checks[8] = "downgrade_failed" },
		"global_not_run":     func(r *Result) { r.NotRun = []string{"NOT RUN proof_profile_database_unavailable"} },
		"row_not_run":        func(r *Result) { r.Rows[3].NotRun = []string{"NOT RUN mixed_binaries_roles_unavailable"} },
		"duplicate_row":      func(r *Result) { r.Rows[3] = r.Rows[0] },
		"unbound_identity":   func(r *Result) { r.Identity.CurrentServer.Digest = "" },
		"missing_job":        func(r *Result) { r.Rows[3].JobEvidence = r.Rows[3].JobEvidence[:9] },
		"changed_rows":       func(r *Result) { r.Rows[3].JobEvidence[0].RowsUnchanged = false },
		"wrong_job_class":    func(r *Result) { r.Rows[3].JobEvidence[0].Class = "guard_cause_hidden" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			result := completeMixedResultFixture()
			mutate(&result)
			err := finalizeRuntimeResults(&result, true)
			for _, receipt := range result.RuntimeResults {
				if receipt.Outcome == syncharness.Pass {
					t.Fatal("capturemix incomplete disposable matrix published PASS before final row")
				}
			}
			if len(result.RuntimeArtifacts) != 0 {
				t.Fatal("capturemix incomplete disposable matrix published private PASS artifact")
			}
			if err == nil || result.VerifyMixedExecution() == nil {
				t.Fatal("capturemix incomplete disposable matrix accepted")
			}
		})
	}
}

func TestMixedRuntimeDisposableCompleteIdentityReceipts(t *testing.T) {
	result := completeMixedResultFixture()
	if err := finalizeRuntimeResults(&result, true); err != nil || result.VerifyMixedExecution() != nil {
		t.Fatal("capturemix complete disposable matrix rejected")
	}
	if len(result.RuntimeResults) != 5 || len(result.RuntimeArtifacts) != 5 {
		t.Fatal("capturemix five identity-bound receipts missing")
	}
	for i := range result.RuntimeResults {
		original := result.RuntimeResults[i].BuildIdentity
		result.RuntimeResults[i].BuildIdentity = nil
		if result.VerifyMixedExecution() == nil {
			t.Fatal("capturemix CI accepted a receipt without identity")
		}
		result.RuntimeResults[i].BuildIdentity = original
	}
	result.RuntimeArtifacts[2].JobEvidence = nil
	if result.VerifyMixedExecution() == nil {
		t.Fatal("capturemix CI accepted a receipt without per-job evidence")
	}
}

func TestMixedRuntimeLocalCompletionCannotClaimExecution(t *testing.T) {
	result := completeMixedResultFixture()
	if err := finalizeRuntimeResults(&result, false); err != nil || len(result.RuntimeResults) != 5 {
		t.Fatal("capturemix local rehearsal receipts missing")
	}
	for _, receipt := range result.RuntimeResults {
		if receipt.Outcome != syncharness.NotRun || receipt.BuildIdentity != nil {
			t.Fatal("capturemix local rehearsal published PASS")
		}
	}
	if len(result.RuntimeArtifacts) != 0 || result.VerifyMixedExecution() == nil {
		t.Fatal("capturemix local rehearsal accepted as CI evidence")
	}
}
