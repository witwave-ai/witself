package capturemix

import (
	"errors"

	"github.com/witwave-ai/witself/internal/syncharness"
)

// JobEvidence is a verified callback observation with its unchanged-row predicate.
type JobEvidence = syncharness.MixedJobEvidence

// The pinned predecessor's callbacks expose exactly these evidence classes.
// Hidden causes are qualifications for these two callbacks only; purge refuses
// at the nonzero-sync-version destruction fence before covered domain DML.
// Never infer a class from job membership alone: observedJobEvidence requires
// the corresponding callback observation and the unchanged-row predicate.
var allowedJobEvidence = map[string]syncharness.MixedJobEvidenceClass{
	"avatar_style_rollout":            syncharness.GuardCauseExact,
	"message_rate_bucket_cleanup":     syncharness.GuardCauseExact,
	"agent_email_rate_bucket_cleanup": syncharness.GuardCauseExact,
	"agent_email_outbound":            syncharness.GuardCauseExact,
	"transcript_retention":            syncharness.GuardCauseExact,
	"message_retention":               syncharness.GuardCauseExact,
	"agent_email_retention":           syncharness.GuardCauseExact,
	"support_ticket_age_out":          syncharness.GuardCauseHidden,
	"transcript_storage_reconcile":    syncharness.GuardCauseHidden,
	"account_purge":                   syncharness.PreguardRefusal,
}

func observedJobEvidence(job string, exact, hidden, preguard, unchanged bool) (JobEvidence, error) {
	allowed, known := allowedJobEvidence[job]
	if !known || !unchanged {
		return JobEvidence{}, errors.New("capturemix job evidence predicate missing")
	}
	var observed syncharness.MixedJobEvidenceClass
	count := 0
	for _, candidate := range []struct {
		present bool
		class   syncharness.MixedJobEvidenceClass
	}{
		{exact, syncharness.GuardCauseExact},
		{hidden, syncharness.GuardCauseHidden},
		{preguard, syncharness.PreguardRefusal},
	} {
		if candidate.present {
			observed = candidate.class
			count++
		}
	}
	if count != 1 || observed != allowed {
		return JobEvidence{}, errors.New("capturemix job evidence class mismatch: " + job)
	}
	return JobEvidence{Job: job, Class: observed, RowsUnchanged: true}, nil
}

func (p *childProcess) verifiedJobEvidence(expected []string, unchanged bool) ([]JobEvidence, error) {
	p.logs.mu.Lock()
	defer p.logs.mu.Unlock()
	result := make([]JobEvidence, 0, len(expected))
	for _, job := range expected {
		evidence, err := observedJobEvidence(job, p.logs.refused[job], p.logs.hidden[job], job == "account_purge" && p.logs.destructionRefused, unchanged)
		if err != nil {
			return nil, err
		}
		result = append(result, evidence)
	}
	return result, nil
}
