package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	archiveexport "github.com/witwave-ai/witself/internal/export"
)

func TestDeleteFactResultIsValueFree(t *testing.T) {
	deletedAt := time.Date(2026, 7, 13, 12, 0, 0, 0, time.UTC)
	raw, err := json.Marshal(DeleteFactResult{
		FactID: "fact_1", SubjectID: "sub_1", Subject: "self",
		Predicate: "identity/name", PriorResolvedAssertionID: "fas_1",
		AssertionCount: 2, CandidateCount: 1, UsageCount: 3,
		Sensitive: true, DeletedAt: &deletedAt, Applied: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	for _, forbidden := range []string{
		"value", "value_type", "source", "reason", "fingerprint", "idempotency",
	} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("deletion receipt contains forbidden field %q: %s", forbidden, text)
		}
	}
}

func TestFactDeletionArchiveRoundTrip(t *testing.T) {
	dsn := os.Getenv("WITSELF_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("WITSELF_TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	st, err := Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.Migrate(); err != nil {
		t.Fatal(err)
	}
	provisioned, err := st.ProvisionAccount(ctx, "fact-delete-archive@witwave.ai", "fact delete archive", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	cleanup := func() { _ = deleteFactTestAccount(ctx, st, provisioned.AccountID) }
	defer cleanup()
	if activated, err := st.ActivateAccount(ctx, provisioned.AccountID); err != nil || !activated {
		t.Fatalf("activate = %v / %v", activated, err)
	}
	realm, err := st.CreateRealm(ctx, provisioned.AccountID, "default")
	if err != nil {
		t.Fatal(err)
	}
	agent, err := st.CreateAgent(ctx, provisioned.AccountID, realm.ID, "archivist")
	if err != nil {
		t.Fatal(err)
	}
	p := Principal{Kind: PrincipalAgent, ID: agent.ID, AccountID: provisioned.AccountID, RealmID: realm.ID, AccountStatus: "active"}
	fact, err := st.SetFact(ctx, p, SetFactInput{
		Predicate: "preferences/archive-secret", Value: json.RawMessage(`"erase me"`),
		Sensitive: true, IdempotencyKey: "archive-old-set",
	})
	if err != nil {
		t.Fatal(err)
	}
	preview, err := st.DeleteFact(ctx, p, DeleteFactInput{FactID: fact.ID})
	if err != nil {
		t.Fatal(err)
	}
	receipt, err := st.DeleteFact(ctx, p, DeleteFactInput{
		FactID: fact.ID, ExpectedResolvedAssertionID: preview.PriorResolvedAssertionID,
		ExpectedCandidateRevision: preview.CandidateRevision,
		IdempotencyKey:            "archive-delete", Apply: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SuspendAccountSystem(ctx, p.AccountID, "evacuation", "fact deletion archive test"); err != nil {
		t.Fatal(err)
	}
	var archive bytes.Buffer
	if err := st.ExportAccount(ctx, p.AccountID, "test-source", "test", &archive); err != nil {
		t.Fatal(err)
	}
	tombstoneSeen, retryRows, committedRetryRows := false, 0, 0
	if _, err := archiveexport.Read(ctx, bytes.NewReader(archive.Bytes()), archiveexport.ImportOptions{
		CurrentSchema: SchemaVersion(),
		Row: func(table string, row []byte) error {
			for _, forbidden := range []string{"erase me", "archive-old-set", "archive-delete"} {
				if bytes.Contains(row, []byte(forbidden)) {
					t.Fatalf("%s archive row retained deleted content/key %q: %s", table, forbidden, row)
				}
			}
			var obj map[string]any
			if err := json.Unmarshal(row, &obj); err != nil {
				return err
			}
			if table == "facts" && obj["id"] == fact.ID {
				tombstoneSeen = true
				if obj["resolved_assertion_id"] != nil || obj["deleted_at"] == nil || obj["deleted_prior_assertion_id"] != fact.ResolvedAssertionID {
					t.Fatalf("exported fact tombstone = %#v", obj)
				}
				if revision, _ := obj["deleted_candidate_revision"].(string); !validFactSHA256(revision) {
					t.Fatalf("exported candidate revision = %#v", revision)
				}
				committedRetryRows = int(obj["deleted_mutation_key_count"].(float64))
			}
			if table == "fact_assertions" && obj["fact_id"] == fact.ID {
				t.Fatalf("deleted assertion exported: %#v", obj)
			}
			if table == "fact_mutation_tombstones" && obj["fact_id"] == fact.ID {
				retryRows++
				if _, exists := obj["idempotency_key"]; exists {
					t.Fatalf("raw retry key column exported: %#v", obj)
				}
				if _, exists := obj["idempotency_fingerprint"]; exists {
					t.Fatalf("value fingerprint column exported: %#v", obj)
				}
			}
			return nil
		},
	}); err != nil {
		t.Fatal(err)
	}
	if !tombstoneSeen || retryRows != 1 || committedRetryRows != retryRows {
		t.Fatalf("archive tombstone coverage: fact=%v retry_rows=%d committed=%d", tombstoneSeen, retryRows, committedRetryRows)
	}
	cleanup()
	if _, err := st.ImportAccount(ctx, p.AccountID, bytes.NewReader(archive.Bytes())); err != nil {
		t.Fatal(err)
	}
	if err := st.ResumeAccountSystem(ctx, p.AccountID, "evacuation"); err != nil {
		t.Fatal(err)
	}
	replayed, err := st.DeleteFact(ctx, p, DeleteFactInput{
		FactID: fact.ID, ExpectedResolvedAssertionID: preview.PriorResolvedAssertionID,
		ExpectedCandidateRevision: preview.CandidateRevision,
		IdempotencyKey:            "archive-delete", Apply: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !replayed.Replayed || replayed.ReceiptID == "" || replayed.ReceiptID != receipt.ReceiptID ||
		replayed.PriorResolvedAssertionID != receipt.PriorResolvedAssertionID ||
		replayed.AssertionCount != receipt.AssertionCount || replayed.CandidateCount != receipt.CandidateCount {
		t.Fatalf("restored delete replay = %#v, original %#v", replayed, receipt)
	}
	if _, err := st.SetFact(ctx, p, SetFactInput{
		Predicate: "preferences/archive-secret", Value: json.RawMessage(`"erase me"`),
		IdempotencyKey: "archive-old-set",
	}); !errors.Is(err, ErrFactDeleted) {
		t.Fatalf("restored delayed retry = %v", err)
	}
	recreated, err := st.SetFact(ctx, p, SetFactInput{
		Predicate: "preferences/archive-secret", Value: json.RawMessage(`"new value"`),
		Sensitive: false, IdempotencyKey: "archive-recreate", RecreateDeleted: true,
	})
	phaseA, phaseErr := factDeleteTestHasFullAddressUnique(ctx, st)
	if phaseErr != nil {
		t.Fatal(phaseErr)
	}
	if phaseA {
		if !factDeleteTestUniqueViolation(err) {
			t.Fatalf("phase-A restored recreation = %v, want retained full-UNIQUE rejection", err)
		}
	} else {
		if err != nil {
			t.Fatal(err)
		}
		if recreated.ID == fact.ID || !recreated.Sensitive {
			t.Fatalf("restored recreation = %#v", recreated)
		}
	}
}

func TestFactDeletionMutationChangesIdempotencyFingerprint(t *testing.T) {
	base := SetFactInput{Predicate: "preferences/editor", Value: json.RawMessage(`"zed"`)}
	ordinary, err := factSetFingerprint(base)
	if err != nil {
		t.Fatal(err)
	}
	base.RecreateDeleted = true
	recreated, err := factSetFingerprint(base)
	if err != nil {
		t.Fatal(err)
	}
	if ordinary == recreated {
		t.Fatal("recreate_deleted did not change mutation fingerprint")
	}
}

func TestOrdinarySetFingerprintRemainsSchema26Compatible(t *testing.T) {
	base := SetFactInput{Predicate: "preferences/editor", Value: json.RawMessage(`"zed"`)}
	current, err := factSetFingerprint(base)
	if err != nil {
		t.Fatal(err)
	}
	// This is the exact pre-schema-27 payload. Keeping recreate_deleted
	// omitted when false means retries of assertions written by v26 continue
	// to match their durable fingerprints after upgrade.
	legacy, err := factMutationFingerprint(struct {
		Subject     string          `json:"subject"`
		Predicate   string          `json:"predicate"`
		ValueType   string          `json:"value_type"`
		Value       json.RawMessage `json:"value"`
		Recurrence  string          `json:"recurrence"`
		Cardinality string          `json:"cardinality"`
		Sensitive   bool            `json:"sensitive"`
		SourceKind  string          `json:"source_kind"`
		SourceRef   string          `json:"source_ref"`
		Confidence  float64         `json:"confidence"`
		ObservedAt  *time.Time      `json:"observed_at,omitempty"`
		ConfirmedAt *time.Time      `json:"confirmed_at,omitempty"`
		ValidFrom   *time.Time      `json:"valid_from,omitempty"`
		ValidUntil  *time.Time      `json:"valid_until,omitempty"`
	}{
		Subject: "self", Predicate: "preferences/editor", ValueType: "string",
		Value: json.RawMessage(`"zed"`), Cardinality: FactCardinalityOne,
		SourceKind: FactSourceSelf, Confidence: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if current != legacy {
		t.Fatalf("schema-26 retry fingerprint changed: current=%s legacy=%s", current, legacy)
	}
}

func TestFactDeletedEventShapeIsValueFree(t *testing.T) {
	err := checkEventShape(EventInput{
		AccountID: "acc_1", ActorKind: ActorAgent, ActorID: "agt_1",
		Verb: VerbFactDeleted,
		Metadata: map[string]any{
			"fact_id": "fact_1", "subject_id": "sub_1", "subject": "self",
			"predicate": "identity/name", "receipt_id": "fdel_1",
			"assertion_count": int64(1), "candidate_count": int64(0),
			"usage_count": int64(2), "sensitive": true,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	err = checkEventShape(EventInput{
		AccountID: "acc_1", ActorKind: ActorAgent, ActorID: "agt_1",
		Verb: VerbFactDeleted,
		Metadata: map[string]any{
			"fact_id": "fact_1", "subject_id": "sub_1", "subject": "self",
			"predicate": "identity/name", "receipt_id": "fdel_1",
			"value": "must never enter the ledger",
		},
	})
	if !errors.Is(err, ErrBadEventMetadata) {
		t.Fatalf("value-bearing event error = %v", err)
	}
}

func TestValidateImportedFactMutationTombstone(t *testing.T) {
	valid := map[string]any{
		"id": "fmt_1", "surface": "set",
		"idempotency_key_hash": strings.Repeat("a", 64),
		"deleted_at":           "2026-07-13T12:00:00Z",
	}
	if err := validateImportedFactMutationTombstone(valid); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(map[string]any){
		"raw key": func(row map[string]any) { row["idempotency_key_hash"] = "raw-retry-key" },
		"surface": func(row map[string]any) { row["surface"] = "delete" },
		"time":    func(row map[string]any) { row["deleted_at"] = "yesterday" },
	} {
		t.Run(name, func(t *testing.T) {
			row := make(map[string]any, len(valid))
			for key, value := range valid {
				row[key] = value
			}
			mutate(row)
			if err := validateImportedFactMutationTombstone(row); err == nil {
				t.Fatal("invalid tombstone accepted")
			}
		})
	}
}

func TestValidateImportedFactDeletionContent(t *testing.T) {
	active := map[string]any{
		"id": "fact_active", "predicate": "preferences/editor",
		"cardinality": FactCardinalityOne, "sensitive": false,
		"resolved_assertion_id": "fas_active", "deleted_at": nil,
		"deleted_by_agent_id": nil, "delete_receipt_id": "",
		"delete_idempotency_key_hash": "", "deleted_prior_assertion_id": "",
		"deleted_assertion_count": float64(0), "deleted_candidate_count": float64(0),
		"deleted_usage_count":        float64(0),
		"deleted_mutation_key_count": float64(0),
		"deleted_candidate_revision": "",
		"recreated_at":               nil, "replacement_fact_id": nil,
	}
	resolved, deleted, replacement, err := validateImportedFactContent(active, "agt_1")
	if err != nil || resolved != "fas_active" || deleted || replacement != "" {
		t.Fatalf("active = %q / %v / %q / %v", resolved, deleted, replacement, err)
	}
	tombstone := map[string]any{
		"id": "fact_deleted", "predicate": "preferences/editor",
		"cardinality": FactCardinalityOne, "sensitive": true,
		"resolved_assertion_id": nil, "deleted_at": "2026-07-13T12:00:00Z",
		"deleted_by_agent_id": "agt_1", "delete_receipt_id": "fdel_1",
		"delete_idempotency_key_hash": strings.Repeat("a", 64),
		"deleted_prior_assertion_id":  "fas_old",
		"deleted_assertion_count":     float64(2), "deleted_candidate_count": float64(1),
		"deleted_usage_count":        float64(3),
		"deleted_mutation_key_count": float64(2),
		"deleted_candidate_revision": strings.Repeat("b", 64),
		"recreated_at":               "2026-07-13T12:01:00Z", "replacement_fact_id": "fact_new",
	}
	resolved, deleted, replacement, err = validateImportedFactContent(tombstone, "agt_1")
	if err != nil || resolved != "" || !deleted || replacement != "fact_new" {
		t.Fatalf("tombstone = %q / %v / %q / %v", resolved, deleted, replacement, err)
	}
	for name, mutate := range map[string]func(map[string]any){
		"resolved content":       func(row map[string]any) { row["resolved_assertion_id"] = "fas_leaked" },
		"wrong actor":            func(row map[string]any) { row["deleted_by_agent_id"] = "agt_other" },
		"raw retry key":          func(row map[string]any) { row["delete_idempotency_key_hash"] = "raw-key" },
		"bad candidate revision": func(row map[string]any) { row["deleted_candidate_revision"] = "count-only" },
		"zero assertions":        func(row map[string]any) { row["deleted_assertion_count"] = float64(0) },
		"partial recreate":       func(row map[string]any) { row["recreated_at"] = nil },
	} {
		t.Run(name, func(t *testing.T) {
			row := make(map[string]any, len(tombstone))
			for key, value := range tombstone {
				row[key] = value
			}
			mutate(row)
			if _, _, _, err := validateImportedFactContent(row, "agt_1"); err == nil {
				t.Fatal("invalid deletion content accepted")
			}
		})
	}
}

func TestHostileArchiveFactReplacementTopology(t *testing.T) {
	scope := func(deleted bool, replacement string) factImportScope {
		return factImportScope{
			realmID: "rlm_1", ownerAgentID: "agt_1", subjectID: "sub_1",
			subjectKey: "self", predicate: "preferences/editor",
			deleted: deleted, replacementFactID: replacement,
		}
	}
	valid := map[string]factImportScope{
		"fact_old":    scope(true, "fact_middle"),
		"fact_middle": scope(true, "fact_active"),
		"fact_active": scope(false, ""),
	}
	if err := validateImportedFactReplacementTopology(valid); err != nil {
		t.Fatalf("valid replacement chain = %v", err)
	}
	validDeletedTail := map[string]factImportScope{
		"fact_old":  scope(true, "fact_tail"),
		"fact_tail": scope(true, ""),
	}
	if err := validateImportedFactReplacementTopology(validDeletedTail); err != nil {
		t.Fatalf("valid deleted tail = %v", err)
	}
	sensitivePredecessor := scope(true, "fact_active")
	sensitivePredecessor.sensitive = true
	if err := validateImportedFactReplacementTopology(map[string]factImportScope{
		"fact_sensitive_old": sensitivePredecessor,
		"fact_active":        scope(false, ""),
	}); err == nil {
		t.Fatal("sensitive predecessor with non-sensitive replacement was accepted")
	}

	for name, facts := range map[string]map[string]factImportScope{
		"duplicate unrecreated tails": {
			"fact_a": scope(true, ""), "fact_b": scope(true, ""),
		},
		"replacement cycle": {
			"fact_a": scope(true, "fact_b"), "fact_b": scope(true, "fact_a"),
		},
		"disconnected from active": {
			"fact_active": scope(false, ""), "fact_deleted": scope(true, ""),
		},
		"merged histories": {
			"fact_a":      scope(true, "fact_active"),
			"fact_b":      scope(true, "fact_active"),
			"fact_active": scope(false, ""),
		},
		"multiple active": {
			"fact_a": scope(false, ""), "fact_b": scope(false, ""),
		},
	} {
		t.Run(name, func(t *testing.T) {
			if err := validateImportedFactReplacementTopology(facts); err == nil {
				t.Fatal("hostile topology accepted")
			}
		})
	}
}

func TestHostileArchiveFactMutationTombstoneCompleteness(t *testing.T) {
	facts := map[string]factImportScope{
		"fact_deleted": {deleted: true, deletedMutationKeyCount: 1},
		"fact_active":  {deleted: false, deletedMutationKeyCount: 0},
	}
	if err := validateImportedFactMutationTombstoneCompleteness(facts, map[string]int64{"fact_deleted": 1}); err != nil {
		t.Fatalf("complete retry tombstones = %v", err)
	}
	if err := validateImportedFactMutationTombstoneCompleteness(facts, nil); err == nil {
		t.Fatal("archive omitting committed retry tombstone table was accepted")
	}
	if err := validateImportedFactMutationTombstoneCompleteness(facts, map[string]int64{"fact_deleted": 2}); err == nil {
		t.Fatal("archive adding an extra retry tombstone row was accepted")
	}
}

// TestFactDeletionPostgres is opt-in because it needs disposable Postgres. It
// covers preview/apply, concurrent idempotent replay, content erasure,
// delayed-retry blocking, explicit recreation, zero rank, and audit safety.
func TestFactDeletionPostgres(t *testing.T) {
	dsn := os.Getenv("WITSELF_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("WITSELF_TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	st, err := Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.Migrate(); err != nil {
		t.Fatal(err)
	}
	phaseA, err := factDeleteTestHasFullAddressUnique(ctx, st)
	if err != nil {
		t.Fatal(err)
	}
	provisioned, err := st.ProvisionAccount(ctx, "fact-delete@witwave.ai", "fact delete", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = deleteFactTestAccount(ctx, st, provisioned.AccountID) }()
	if activated, err := st.ActivateAccount(ctx, provisioned.AccountID); err != nil || !activated {
		t.Fatalf("activate = %v / %v", activated, err)
	}
	realm, err := st.CreateRealm(ctx, provisioned.AccountID, "default")
	if err != nil {
		t.Fatal(err)
	}
	agent, err := st.CreateAgent(ctx, provisioned.AccountID, realm.ID, "deleter")
	if err != nil {
		t.Fatal(err)
	}
	p := Principal{Kind: PrincipalAgent, ID: agent.ID, AccountID: provisioned.AccountID, RealmID: realm.ID, AccountStatus: "active"}

	subject, err := st.UpsertFactSubject(ctx, p, UpsertFactSubjectInput{CanonicalKey: "person_spouse", DisplayName: "Spouse"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddFactSubjectAlias(ctx, p, subject.CanonicalKey, "my wife"); err != nil {
		t.Fatal(err)
	}
	fact, err := st.SetFact(ctx, p, SetFactInput{
		Subject: subject.CanonicalKey, Predicate: "identity/name",
		Value: json.RawMessage(`"Delete Canary"`), Sensitive: true,
		IdempotencyKey: "old-set-key",
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Run("schema 27 retains the legacy full-address upsert", func(t *testing.T) {
		if !phaseA {
			t.Skip("full address UNIQUE is intentionally removed after phase A")
		}
		// This is the unqualified v0.0.159 conflict target. Phase A must keep
		// its full UNIQUE constraint so an old pod can continue writing after
		// a new pod applies additive migration 0027.
		legacyInsertID := fact.ID + "_legacy_phase_a"
		var gotFactID, gotAssertionID string
		err := st.pool.QueryRow(ctx, `
			INSERT INTO facts
			  (id, account_id, realm_id, owner_agent_id, subject_id, predicate,
			   cardinality, sensitive, created_at, updated_at)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,clock_timestamp(),clock_timestamp())
			ON CONFLICT (owner_agent_id, subject_id, predicate)
			DO UPDATE SET cardinality=EXCLUDED.cardinality,
			              sensitive=facts.sensitive OR EXCLUDED.sensitive,
			              updated_at=clock_timestamp()
			RETURNING id, COALESCE(resolved_assertion_id,'')`, legacyInsertID,
			p.AccountID, p.RealmID, p.ID, subject.ID, fact.Predicate,
			FactCardinalityOne, false).Scan(&gotFactID, &gotAssertionID)
		if err != nil {
			t.Fatalf("legacy v0.0.159 upsert after schema 27: %v", err)
		}
		if gotFactID != fact.ID || gotAssertionID != fact.ResolvedAssertionID {
			t.Fatalf("legacy upsert returned %q/%q, want %q/%q", gotFactID, gotAssertionID, fact.ID, fact.ResolvedAssertionID)
		}
	})
	lateAliasProposal := ProposeFactInput{
		SetFactInput: SetFactInput{Subject: "wife_late", Predicate: "identity/name",
			Value: json.RawMessage(`"Late Alias Secret"`), IdempotencyKey: "late-alias-proposal-key"},
		Reason: "candidate predates subject alias",
	}
	lateAliasCandidate, err := st.ProposeFact(ctx, p, lateAliasProposal)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddFactSubjectAlias(ctx, p, subject.CanonicalKey, "wife_late"); err != nil {
		t.Fatal(err)
	}
	canonicalized, err := st.GetFactCandidate(ctx, p, lateAliasCandidate.ID)
	if err != nil {
		t.Fatal(err)
	}
	if canonicalized.Subject != subject.CanonicalKey {
		t.Fatalf("late alias candidate subject = %q, want %q", canonicalized.Subject, subject.CanonicalKey)
	}
	lateAliasReplay, err := st.ProposeFact(ctx, p, lateAliasProposal)
	if err != nil {
		t.Fatal(err)
	}
	if lateAliasReplay.ID != lateAliasCandidate.ID || lateAliasReplay.Subject != subject.CanonicalKey {
		t.Fatalf("late alias proposal replay = %#v, original %#v", lateAliasReplay, lateAliasCandidate)
	}
	// Simulate a schema-26/pre-fix legacy row to prove deletion itself covers
	// both canonical and alias addresses, not only newly canonicalized rows.
	if _, err := st.pool.Exec(ctx, `UPDATE fact_candidates SET subject_key='wife_late' WHERE id=$1`, lateAliasCandidate.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.ProposeFact(ctx, p, ProposeFactInput{
		SetFactInput: SetFactInput{Subject: "my wife", Predicate: "identity/name",
			Value: json.RawMessage(`"Candidate Secret"`), IdempotencyKey: "old-proposal-key"},
		Reason: "private candidate reason",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.GetFact(ctx, p, "my wife", "identity/name"); err != nil {
		t.Fatal(err)
	}

	var usageBeforePreview int
	if err := st.pool.QueryRow(ctx, `SELECT count(*) FROM usage_events WHERE subject_type='fact' AND subject_id=$1`, fact.ID).Scan(&usageBeforePreview); err != nil {
		t.Fatal(err)
	}
	preview, err := st.DeleteFact(ctx, p, DeleteFactInput{Subject: "my wife", Predicate: "identity/name"})
	if err != nil {
		t.Fatal(err)
	}
	var usageAfterPreview int
	if err := st.pool.QueryRow(ctx, `SELECT count(*) FROM usage_events WHERE subject_type='fact' AND subject_id=$1`, fact.ID).Scan(&usageAfterPreview); err != nil {
		t.Fatal(err)
	}
	if usageAfterPreview != usageBeforePreview {
		t.Fatalf("address preview recorded usage: before=%d after=%d", usageBeforePreview, usageAfterPreview)
	}
	if preview.FactID != fact.ID || preview.Applied || preview.DeletedAt != nil || preview.AssertionCount != 1 ||
		preview.CandidateCount != 2 || preview.UsageCount != 1 || !preview.Sensitive {
		t.Fatalf("preview = %#v", preview)
	}
	if !validFactSHA256(preview.CandidateRevision) {
		t.Fatalf("preview candidate revision = %q", preview.CandidateRevision)
	}

	// Candidate cardinality alone is not a sufficient preview guard. Replace a
	// candidate id without changing the count and prove the ordered-set
	// revision rejects the stale apply before any content is erased.
	replacementCandidateID := "fcand_revision_replacement"
	if _, err := st.pool.Exec(ctx, `UPDATE fact_candidates SET id=$1 WHERE id=$2`, replacementCandidateID, lateAliasCandidate.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DeleteFact(ctx, p, DeleteFactInput{
		FactID: fact.ID, ExpectedResolvedAssertionID: preview.PriorResolvedAssertionID,
		ExpectedCandidateRevision: preview.CandidateRevision,
		IdempotencyKey:            "stale-candidate-set-delete", Apply: true,
	}); !errors.Is(err, ErrFactConflict) {
		t.Fatalf("same-count candidate replacement after preview = %v", err)
	}
	preview, err = st.DeleteFact(ctx, p, DeleteFactInput{FactID: fact.ID})
	if err != nil {
		t.Fatal(err)
	}
	beforeRejectRevision := preview.CandidateRevision
	if _, err := st.RejectFactCandidateIdempotent(ctx, p, replacementCandidateID, "reject-before-delete"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DeleteFact(ctx, p, DeleteFactInput{
		FactID: fact.ID, ExpectedResolvedAssertionID: preview.PriorResolvedAssertionID,
		ExpectedCandidateRevision: preview.CandidateRevision,
		IdempotencyKey:            "stale-candidate-lifecycle-delete", Apply: true,
	}); !errors.Is(err, ErrFactConflict) {
		t.Fatalf("candidate lifecycle change after preview = %v", err)
	}
	preview, err = st.DeleteFact(ctx, p, DeleteFactInput{FactID: fact.ID})
	if err != nil {
		t.Fatal(err)
	}
	if preview.CandidateRevision == beforeRejectRevision {
		t.Fatal("candidate lifecycle mutation did not change revision")
	}

	apply := DeleteFactInput{FactID: fact.ID,
		ExpectedResolvedAssertionID: preview.PriorResolvedAssertionID,
		ExpectedCandidateRevision:   preview.CandidateRevision,
		IdempotencyKey:              "delete-fact-key", Apply: true}
	results := make([]DeleteFactResult, 2)
	errs := make([]error, 2)
	var wg sync.WaitGroup
	for i := range results {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i], errs[i] = st.DeleteFact(ctx, p, apply)
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("delete %d = %v", i, err)
		}
		if !results[i].Applied || results[i].ReceiptID == "" || results[i].DeletedAt == nil ||
			results[i].PriorResolvedAssertionID != fact.ResolvedAssertionID {
			t.Fatalf("delete %d result = %#v", i, results[i])
		}
	}
	if results[0].Replayed == results[1].Replayed {
		t.Fatalf("replay flags = %v / %v", results[0].Replayed, results[1].Replayed)
	}
	wrongConcurrencyToken := apply
	wrongConcurrencyToken.ExpectedResolvedAssertionID = "fas_wrong"
	if _, err := st.DeleteFact(ctx, p, wrongConcurrencyToken); !errors.Is(err, ErrFactIdempotencyConflict) {
		t.Fatalf("delete replay with different expected assertion = %v", err)
	}
	wrongCandidateToken := apply
	wrongCandidateToken.ExpectedCandidateRevision = strings.Repeat("0", 64)
	if _, err := st.DeleteFact(ctx, p, wrongCandidateToken); !errors.Is(err, ErrFactIdempotencyConflict) {
		t.Fatalf("delete replay with different candidate revision = %v", err)
	}
	// GetFact meters in a companion transaction after returning the fact. A
	// delivery that began before deletion can therefore land after the delete
	// commit. Preserve that immutable event, but replay the original frozen
	// receipt count rather than drifting with the later ledger row.
	if err := st.recordFactRetrievals(ctx, p, FactRetrievalModeExact, []Fact{{ID: fact.ID}}); err != nil {
		t.Fatal(err)
	}
	lateReplay, err := st.DeleteFact(ctx, p, apply)
	if err != nil {
		t.Fatal(err)
	}
	if !lateReplay.Replayed || lateReplay.UsageCount != preview.UsageCount {
		t.Fatalf("late usage changed deletion receipt: preview=%d replay=%#v", preview.UsageCount, lateReplay)
	}
	if _, err := st.DeleteFact(ctx, p, DeleteFactInput{
		FactID: fact.ID, ExpectedResolvedAssertionID: preview.PriorResolvedAssertionID,
		ExpectedCandidateRevision: preview.CandidateRevision,
		IdempotencyKey:            "different-delete-key", Apply: true,
	}); !errors.Is(err, ErrFactDeleted) {
		t.Fatalf("new deletion key against tombstone = %v", err)
	}
	other, err := st.SetFact(ctx, p, SetFactInput{
		Predicate: "preferences/other", Value: json.RawMessage(`"other"`),
		IdempotencyKey: "other-set-key",
	})
	if err != nil {
		t.Fatal(err)
	}
	otherPreview, err := st.DeleteFact(ctx, p, DeleteFactInput{FactID: other.ID})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.SetFact(ctx, p, SetFactInput{
		Predicate: "preferences/other", Value: json.RawMessage(`"newer"`),
		IdempotencyKey: "other-update-key",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DeleteFact(ctx, p, DeleteFactInput{
		FactID: other.ID, ExpectedResolvedAssertionID: otherPreview.PriorResolvedAssertionID,
		ExpectedCandidateRevision: otherPreview.CandidateRevision,
		IdempotencyKey:            "other-stale-delete-key", Apply: true,
	}); !errors.Is(err, ErrFactConflict) {
		t.Fatalf("stale deletion preview = %v", err)
	}
	otherPreview, err = st.DeleteFact(ctx, p, DeleteFactInput{FactID: other.ID})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.DeleteFact(ctx, p, DeleteFactInput{
		FactID: other.ID, ExpectedResolvedAssertionID: otherPreview.PriorResolvedAssertionID,
		ExpectedCandidateRevision: otherPreview.CandidateRevision,
		IdempotencyKey:            apply.IdempotencyKey, Apply: true,
	}); !errors.Is(err, ErrFactIdempotencyConflict) {
		t.Fatalf("deletion key reused for another fact = %v", err)
	}
	if _, err := st.DeleteFact(ctx, p, DeleteFactInput{
		FactID: other.ID, ExpectedResolvedAssertionID: otherPreview.PriorResolvedAssertionID,
		ExpectedCandidateRevision: otherPreview.CandidateRevision,
		IdempotencyKey:            "other-delete-key", Apply: true,
	}); err != nil {
		t.Fatalf("delete second fact after idempotency conflict: %v", err)
	}

	// Reject and delete take the same subject-namespace lock. Whichever wins,
	// the loser observes a complete transaction: either reject commits first
	// and the preview token becomes stale, or delete commits first and the
	// candidate is gone. There is no window after comparison and before erase.
	raceFact, err := st.SetFact(ctx, p, SetFactInput{
		Predicate: "incidents/concurrent-delete", Value: json.RawMessage(`"canonical"`),
		IdempotencyKey: "race-fact-set",
	})
	if err != nil {
		t.Fatal(err)
	}
	raceCandidate, err := st.ProposeFact(ctx, p, ProposeFactInput{
		SetFactInput: SetFactInput{Predicate: "incidents/concurrent-delete",
			Value: json.RawMessage(`"candidate"`), IdempotencyKey: "race-candidate-proposal"},
		Reason: "exercise reject versus delete serialization",
	})
	if err != nil {
		t.Fatal(err)
	}
	racePreview, err := st.DeleteFact(ctx, p, DeleteFactInput{FactID: raceFact.ID})
	if err != nil {
		t.Fatal(err)
	}
	raceApply := DeleteFactInput{
		FactID: raceFact.ID, ExpectedResolvedAssertionID: racePreview.PriorResolvedAssertionID,
		ExpectedCandidateRevision: racePreview.CandidateRevision,
		IdempotencyKey:            "race-fact-delete", Apply: true,
	}
	startRace := make(chan struct{})
	var raceDeleteErr, raceRejectErr error
	var raceWG sync.WaitGroup
	raceWG.Add(2)
	go func() {
		defer raceWG.Done()
		<-startRace
		_, raceDeleteErr = st.DeleteFact(ctx, p, raceApply)
	}()
	go func() {
		defer raceWG.Done()
		<-startRace
		_, raceRejectErr = st.RejectFactCandidateIdempotent(ctx, p, raceCandidate.ID, "race-candidate-reject")
	}()
	close(startRace)
	raceWG.Wait()
	switch {
	case raceDeleteErr == nil:
		if !errors.Is(raceRejectErr, ErrFactNotFound) {
			t.Fatalf("delete won race but reject = %v", raceRejectErr)
		}
	case errors.Is(raceDeleteErr, ErrFactConflict):
		if raceRejectErr != nil {
			t.Fatalf("reject won race but reject = %v", raceRejectErr)
		}
		fresh, err := st.DeleteFact(ctx, p, DeleteFactInput{FactID: raceFact.ID})
		if err != nil {
			t.Fatal(err)
		}
		raceApply.ExpectedCandidateRevision = fresh.CandidateRevision
		if _, err := st.DeleteFact(ctx, p, raceApply); err != nil {
			t.Fatalf("delete after serialized reject = %v", err)
		}
	default:
		t.Fatalf("concurrent reject/delete = reject %v, delete %v", raceRejectErr, raceDeleteErr)
	}
	if _, err := st.GetFact(ctx, p, "my wife", "identity/name"); !errors.Is(err, ErrFactNotFound) {
		t.Fatalf("get deleted fact = %v", err)
	}

	var assertions, candidates, subjects, usage, retryTombstones, committedRetryTombstones int
	if err := st.pool.QueryRow(ctx, `SELECT
		(SELECT count(*) FROM fact_assertions WHERE fact_id=$1),
		(SELECT count(*) FROM fact_candidates WHERE account_id=$2 AND owner_agent_id=$3 AND subject_key='person_spouse' AND predicate='identity/name'),
		(SELECT count(*) FROM fact_subjects WHERE id=$4 AND aliases ? 'my wife'),
		(SELECT count(*) FROM usage_events WHERE subject_type='fact' AND subject_id=$1),
		(SELECT count(*) FROM fact_mutation_tombstones WHERE fact_id=$1),
		(SELECT deleted_mutation_key_count FROM facts WHERE id=$1)`, fact.ID,
		p.AccountID, p.ID, subject.ID).Scan(&assertions, &candidates, &subjects, &usage, &retryTombstones, &committedRetryTombstones); err != nil {
		t.Fatal(err)
	}
	if assertions != 0 || candidates != 0 || subjects != 1 || usage != 2 || retryTombstones != 3 || committedRetryTombstones != retryTombstones {
		t.Fatalf("preserved state assertions=%d candidates=%d subjects=%d usage=%d retry_tombstones=%d committed=%d",
			assertions, candidates, subjects, usage, retryTombstones, committedRetryTombstones)
	}
	for _, retry := range []struct {
		name string
		call func() error
	}{
		{"set", func() error {
			_, err := st.SetFact(ctx, p, SetFactInput{Subject: subject.CanonicalKey, Predicate: "identity/name", Value: json.RawMessage(`"Delete Canary"`), IdempotencyKey: "old-set-key"})
			return err
		}},
		{"proposal", func() error {
			_, err := st.ProposeFact(ctx, p, ProposeFactInput{SetFactInput: SetFactInput{Subject: subject.CanonicalKey, Predicate: "identity/name", Value: json.RawMessage(`"Candidate Secret"`), IdempotencyKey: "old-proposal-key"}})
			return err
		}},
		{"ordinary new set", func() error {
			_, err := st.SetFact(ctx, p, SetFactInput{Subject: subject.CanonicalKey, Predicate: "identity/name", Value: json.RawMessage(`"new"`), IdempotencyKey: "new-set-without-recreate"})
			return err
		}},
	} {
		t.Run(retry.name, func(t *testing.T) {
			if err := retry.call(); !errors.Is(err, ErrFactDeleted) {
				t.Fatalf("error = %v", err)
			}
		})
	}
	if _, err := st.SetFact(ctx, p, SetFactInput{
		Subject: subject.CanonicalKey, Predicate: "identity/name",
		Value: json.RawMessage(`"Fresh Value"`), RecreateDeleted: true,
	}); !errors.Is(err, ErrFactInputInvalid) {
		t.Fatalf("recreation without fresh retry key = %v", err)
	}

	recreated, err := st.SetFact(ctx, p, SetFactInput{
		Subject: subject.CanonicalKey, Predicate: "identity/name",
		Value: json.RawMessage(`"Fresh Value"`), Sensitive: false,
		IdempotencyKey: "explicit-recreate", RecreateDeleted: true,
	})
	if phaseA {
		if !factDeleteTestUniqueViolation(err) {
			t.Fatalf("phase-A recreation = %v, want retained full-UNIQUE rejection", err)
		}
		var replacementID *string
		if err := st.pool.QueryRow(ctx, `SELECT replacement_fact_id FROM facts WHERE id=$1`, fact.ID).Scan(&replacementID); err != nil {
			t.Fatal(err)
		}
		if replacementID != nil {
			t.Fatalf("failed phase-A recreation marked replacement = %q", *replacementID)
		}
	} else {
		if err != nil {
			t.Fatal(err)
		}
		if recreated.ID == fact.ID || recreated.UsageCount != 0 || !recreated.Sensitive {
			t.Fatalf("recreated fact = %#v", recreated)
		}
		listed, err := st.ListFacts(ctx, p, FactListOptions{IncludeSensitive: true})
		if err != nil {
			t.Fatal(err)
		}
		if len(listed) != 1 || listed[0].ID != recreated.ID || listed[0].UsageCount != 0 || !listed[0].Sensitive {
			t.Fatalf("recreated list = %#v", listed)
		}
		var replacementID string
		if err := st.pool.QueryRow(ctx, `SELECT replacement_fact_id FROM facts WHERE id=$1`, fact.ID).Scan(&replacementID); err != nil {
			t.Fatal(err)
		}
		if replacementID != recreated.ID {
			t.Fatalf("replacement = %q, want %q", replacementID, recreated.ID)
		}
	}
	var metadata json.RawMessage
	if err := st.pool.QueryRow(ctx, `SELECT metadata FROM account_events WHERE account_id=$1 AND verb='fact.deleted'`, p.AccountID).Scan(&metadata); err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"Delete Canary", "Candidate Secret", "Late Alias Secret", "private candidate reason", "old-set-key", "late-alias-proposal-key", "delete-fact-key"} {
		if strings.Contains(string(metadata), secret) {
			t.Fatalf("audit metadata leaked %q: %s", secret, metadata)
		}
	}
}

func factDeleteTestUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

func factDeleteTestHasFullAddressUnique(ctx context.Context, st *Store) (bool, error) {
	var exists bool
	err := st.pool.QueryRow(ctx, `
		SELECT EXISTS (
		  SELECT 1 FROM pg_constraint
		  WHERE conrelid='facts'::regclass
		    AND conname='facts_owner_agent_id_subject_id_predicate_key'
		    AND contype='u'
		)`).Scan(&exists)
	return exists, err
}
