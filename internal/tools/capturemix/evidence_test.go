package capturemix

import (
	"reflect"
	"sort"
	"strings"
	"testing"
)

func TestJobEvidenceClassesAndAllowedTableArePinned(t *testing.T) {
	want := map[string]string{
		"avatar_style_rollout":            "guard_cause_exact",
		"message_rate_bucket_cleanup":     "guard_cause_exact",
		"agent_email_rate_bucket_cleanup": "guard_cause_exact",
		"agent_email_outbound":            "guard_cause_exact",
		"transcript_retention":            "guard_cause_exact",
		"message_retention":               "guard_cause_exact",
		"agent_email_retention":           "guard_cause_exact",
		"support_ticket_age_out":          "guard_cause_hidden",
		"transcript_storage_reconcile":    "guard_cause_hidden",
		"account_purge":                   "preguard_refusal",
	}
	got := map[string]string{}
	classes := map[string]bool{}
	for job, class := range allowedJobEvidence {
		got[job] = string(class)
		classes[string(class)] = true
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatal("capturemix allowed job evidence table changed")
	}
	if !reflect.DeepEqual(classes, map[string]bool{"guard_cause_exact": true, "guard_cause_hidden": true, "preguard_refusal": true}) {
		t.Fatal("capturemix job evidence vocabulary changed")
	}
	if len(jobLogPrefixes) != len(want) || len(workerCandidatePredicates) != len(want) {
		t.Fatal("capturemix evidence jobs differ from observed callback or predicate set")
	}
	seenPredicates := map[string]bool{}
	for _, predicate := range workerCandidatePredicates {
		if _, exists := want[predicate.job]; !exists || seenPredicates[predicate.job] || predicate.sql == "" {
			t.Fatal("capturemix unchanged-row predicates differ from evidence jobs")
		}
		seenPredicates[predicate.job] = true
	}
	for job := range jobLogPrefixes {
		if _, exists := want[job]; !exists {
			t.Fatal("capturemix callback set differs from evidence jobs")
		}
	}
}

func TestJobEvidenceRequiresAllowedObservationAndUnchangedRows(t *testing.T) {
	for job, allowed := range allowedJobEvidence {
		for _, class := range []string{"guard_cause_exact", "guard_cause_hidden", "preguard_refusal"} {
			for _, unchanged := range []bool{false, true} {
				evidence, err := observedJobEvidence(job, class == "guard_cause_exact", class == "guard_cause_hidden", class == "preguard_refusal", unchanged)
				accept := class == string(allowed) && unchanged
				if accept && (err != nil || evidence.Job != job || string(evidence.Class) != class || !evidence.RowsUnchanged || evidence.Check() != "capturemix job="+job+" evidence="+class) {
					t.Fatal("capturemix complete job evidence was not recorded exactly")
				}
				if !accept && (err == nil || evidence != (JobEvidence{})) {
					t.Fatal("capturemix unobserved, disallowed, or changed-row evidence accepted")
				}
			}
		}
		for _, observations := range [][3]bool{{}, {true, true, false}, {true, false, true}, {false, true, true}, {true, true, true}} {
			if _, err := observedJobEvidence(job, observations[0], observations[1], observations[2], true); err == nil {
				t.Fatal("capturemix missing or ambiguous evidence accepted")
			}
		}
	}
	if _, err := observedJobEvidence("unknown_job", true, false, false, true); err == nil {
		t.Fatal("capturemix unknown job evidence accepted")
	}
}

func TestJobEvidenceRejectsHiddenCauseForExactJobsAndPurge(t *testing.T) {
	for _, job := range []string{"agent_email_retention", "account_purge"} {
		t.Run(job, func(t *testing.T) {
			if _, err := observedJobEvidence(job, false, true, false, true); err == nil {
				t.Fatal("capturemix hidden cause accepted outside the two reviewed callbacks")
			}
		})
	}
}

func TestJobEvidenceCallbackClassifierRequiresObservedFailure(t *testing.T) {
	for _, test := range []struct {
		line   string
		job    string
		exact  bool
		hidden bool
		purge  bool
	}{
		{"witself-worker: agent-email retention: sync writer not admitted\n", "agent_email_retention", true, false, false},
		{"witself-worker: support ticket age-out batch failed\n", "support_ticket_age_out", false, true, false},
		{"witself-worker: support ticket age-out: resolved=0\n", "support_ticket_age_out", false, false, false},
		{"witself-worker: transcript storage reconcile: corrected=0 error=2 skipped=0\n", "transcript_storage_reconcile", false, true, false},
		{"witself-worker: transcript storage reconcile: corrected=0 error=0 skipped=0\n", "transcript_storage_reconcile", false, false, false},
		{"witself-worker: transcript storage reconcile: corrected=0 error=-1 skipped=0\n", "transcript_storage_reconcile", false, false, false},
		{"witself-worker: transcript storage reconcile: corrected=0 error=invalid skipped=0\n", "transcript_storage_reconcile", false, false, false},
		{"witself-worker: transcript storage reconcile enabled (interval 1m)\n", "transcript_storage_reconcile", false, false, false},
		{"witself-worker: account purge: sync evidence prevents destructive finalization or purge\n", "account_purge", false, false, true},
		{"witself-worker: account purge batch failed\n", "account_purge", false, true, false},
	} {
		w := &observationWriter{refused: map[string]bool{}, registered: map[string]bool{}}
		if _, err := w.Write([]byte(test.line)); err != nil {
			t.Fatal("capturemix callback observation failed")
		}
		if w.refused[test.job] != test.exact || w.hidden[test.job] != test.hidden || w.destructionRefused != test.purge {
			t.Fatal("capturemix callback evidence classifier changed")
		}
	}
}

func TestJobEvidenceSnapshotFailsForHiddenExactJobAndPurge(t *testing.T) {
	for _, job := range []string{"agent_email_retention", "account_purge"} {
		p := &childProcess{logs: &observationWriter{hidden: map[string]bool{job: true}, receipts: map[string]int{job: 2}}}
		metrics := "witself_worker_job_running{job=\"" + job + "\"} 1\nwitself_worker_job_failures_total{job=\"" + job + "\"} 2\n"
		if got := p.missingWorkerOutcomes(metrics, []string{job}); !reflect.DeepEqual(got, []string{job + ":evidence_class"}) {
			t.Fatal("capturemix worker outcome accepted a disallowed hidden cause")
		}
	}
}

func TestJobEvidenceVerifiedTableContainsOnlyObservedJobs(t *testing.T) {
	p := &childProcess{logs: &observationWriter{refused: map[string]bool{}, hidden: map[string]bool{}, destructionRefused: true}}
	var expected []string
	for job, class := range allowedJobEvidence {
		expected = append(expected, job)
		p.logs.refused[job] = string(class) == "guard_cause_exact"
		p.logs.hidden[job] = string(class) == "guard_cause_hidden"
	}
	sort.Strings(expected)
	all, err := p.verifiedJobEvidence(expected, true)
	if err != nil || len(all) != 10 {
		t.Fatal("capturemix complete evidence table rejected")
	}
	for i, item := range all {
		if item.Job != expected[i] || !strings.HasPrefix(item.Check(), "capturemix job="+expected[i]+" evidence=") {
			t.Fatal("capturemix verified table changed observed job order")
		}
	}
	partial, err := p.verifiedJobEvidence(expected[:4], true)
	if err != nil || len(partial) != 4 {
		t.Fatal("capturemix verified table invented unobserved jobs")
	}
	if result, err := p.verifiedJobEvidence(expected, false); err == nil || result != nil {
		t.Fatal("capturemix verified table accepted changed rows")
	}
	delete(p.logs.hidden, "support_ticket_age_out")
	if result, err := p.verifiedJobEvidence(expected, true); err == nil || result != nil {
		t.Fatal("capturemix verified table inferred an unobserved hidden cause")
	}
}
