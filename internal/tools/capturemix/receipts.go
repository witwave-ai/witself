package capturemix

import (
	"errors"
	"reflect"

	"github.com/witwave-ai/witself/internal/syncharness"
)

var mixedCapabilities = [...]string{
	"production_writer_admission", "mixed_executable_versions", "legacy_writer_refusal",
	"proof_profile_registration", "production_downgrade_denial_mixed",
}

// completedMatrixEvidence is the single promotion boundary. Every schedule's
// observed checks, every job predicate, and every qualification are inspected
// before any capability can receive a PASS receipt.
func completedMatrixEvidence(result Result) ([]syncharness.MixedJobEvidence, error) {
	failure := errors.New("capturemix required mixed binary assertions not completed")
	if len(result.NotRun) != 0 || len(result.Rows) != 4 {
		return nil, failure
	}
	seen := make(map[string]bool)
	var evidence []syncharness.MixedJobEvidence
	for _, row := range result.Rows {
		key := row.Server + "/" + row.Worker
		if (row.Server != "old" && row.Server != "new") || (row.Worker != "old" && row.Worker != "new") || seen[key] ||
			row.Completion != "commit_and_rollback" || len(row.NotRun) != 0 {
			return nil, failure
		}
		seen[key] = true
		if evidence == nil {
			evidence = row.JobEvidence
		} else if !reflect.DeepEqual(evidence, row.JobEvidence) {
			return nil, failure
		}
		var expected []string
		for _, completion := range []string{"commit", "rollback"} {
			expected = append(expected, "processes_ready", "legacy_transactions_held", "installation_started", "installation_lock_observed",
				"legacy_"+completion, "guards_installed", "capture_activated", "writer_assertions", "downgrade_refused")
			for _, job := range row.JobEvidence {
				expected = append(expected, job.Check())
			}
		}
		if !reflect.DeepEqual(row.Checks, expected) {
			return nil, failure
		}
	}
	// The constructor validates the complete closed job table and the private
	// identities as well as the matrix count. This provisional value is never
	// published: all five receipts are assigned together by the caller.
	if _, err := syncharness.VerifyMixedRuntimeArtifact("legacy_writer_refusal", len(result.Rows), result.Identity, evidence...); err != nil {
		return nil, failure
	}
	return evidence, nil
}

func finalizeRuntimeResults(result *Result, disposable bool) error {
	result.RuntimeResults = nil
	result.RuntimeArtifacts = nil
	if !disposable {
		for _, capability := range mixedCapabilities {
			reason := "mixed_binaries_roles_unavailable"
			if capability == "proof_profile_registration" {
				reason = "proof_profile_database_unavailable"
			}
			record, err := syncharness.MixedRuntimeNotRun(capability, reason)
			if err != nil {
				return errors.New("capturemix runtime record failed")
			}
			result.RuntimeResults = append(result.RuntimeResults, record)
		}
		return nil
	}
	evidence, err := completedMatrixEvidence(*result)
	if err != nil {
		return err
	}
	var records []syncharness.RuntimeResultRecord
	var artifacts []syncharness.RuntimeArtifact
	for _, capability := range mixedCapabilities {
		var jobs []syncharness.MixedJobEvidence
		if capability == "legacy_writer_refusal" {
			jobs = evidence
		}
		artifact, err := syncharness.VerifyMixedRuntimeArtifact(capability, len(result.Rows), result.Identity, jobs...)
		if err != nil {
			return errors.New("capturemix runtime record failed")
		}
		records = append(records, artifact.Result)
		artifacts = append(artifacts, artifact)
	}
	result.RuntimeResults, result.RuntimeArtifacts = records, artifacts
	return nil
}

// VerifyMixedExecution checks the CI result without logging private identity
// data. Rehearsal results deliberately cannot satisfy this acceptance boundary.
func (result Result) VerifyMixedExecution() error {
	if _, err := completedMatrixEvidence(result); err != nil {
		return err
	}
	expected := result
	if err := finalizeRuntimeResults(&expected, true); err != nil {
		return err
	}
	if !reflect.DeepEqual(result.RuntimeResults, expected.RuntimeResults) || !reflect.DeepEqual(result.RuntimeArtifacts, expected.RuntimeArtifacts) {
		return errors.New("capturemix required identity-bound receipts missing")
	}
	return nil
}
