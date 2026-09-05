package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/witwave-ai/witself/internal/testenv"
)

func TestAuditAppendFailuresExcludeCallerValidation(t *testing.T) {
	st := &Store{}
	for _, in := range []EventInput{
		{AccountID: "audit-account-canary", ActorKind: ActorControlPlane, Verb: "unknown-verb-canary"},
		{AccountID: "audit-account-canary", ActorKind: ActorControlPlane, Verb: VerbAccountActivated,
			Metadata: map[string]any{"private-metadata-canary": "private-value-canary"}},
	} {
		if err := st.logEventTx(context.Background(), nil, in); err == nil {
			t.Fatal("caller validation unexpectedly succeeded")
		}
		if got := st.AuditAppendFailures(); got != 0 {
			t.Fatalf("caller validation increments insert failures: %d", got)
		}
	}
}

func TestAuditAppendFailuresPostgres(t *testing.T) {
	dsn := testenv.RequirePostgres(t)
	st, _ := newMigrationTestStore(t, dsn)
	if err := st.Migrate(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := st.pool.Exec(ctx, `INSERT INTO accounts (id) VALUES ('audit-account-canary')`); err != nil {
		t.Fatal(err)
	}
	in := EventInput{
		AccountID: "audit-account-canary", ActorKind: ActorControlPlane,
		Verb: VerbAccountActivated,
	}
	if err := st.LogEvent(ctx, in); err != nil {
		t.Fatal(err)
	}
	if got := st.AuditAppendFailures(); got != 0 {
		t.Fatalf("successful append increments failures: %d", got)
	}

	tx, err := st.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	// A transaction-local, rolled-back constraint forces a real PostgreSQL
	// INSERT failure after account lookup and shape validation succeed.
	if _, err := tx.Exec(ctx, `ALTER TABLE account_events ADD CONSTRAINT audit_insert_failure_canary
		CHECK (verb <> 'account.activated') NOT VALID`); err != nil {
		t.Fatal(err)
	}
	if err := st.logEventTx(ctx, tx, in); err == nil || !strings.Contains(err.Error(), "insert account_events:") {
		t.Fatalf("forced insert failure = %v", err)
	}
	if got := st.AuditAppendFailures(); got != 1 {
		t.Fatalf("insert failures = %d, want 1", got)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if got := st.AuditAppendFailures(); got != 1 {
		t.Fatalf("rollback lost process failure count: %d", got)
	}

	bad := in
	bad.Metadata = map[string]any{"private-metadata-canary": "private-value-canary"}
	if err := st.LogEvent(ctx, bad); !errors.Is(err, ErrBadEventMetadata) {
		t.Fatalf("invalid shape = %v", err)
	}
	missing := in
	missing.AccountID = "audit-missing-account-canary"
	if err := st.LogEvent(ctx, missing); !errors.Is(err, ErrAccountNotFound) {
		t.Fatalf("missing account = %v", err)
	}
	if got := st.AuditAppendFailures(); got != 1 {
		t.Fatalf("non-insert failures changed counter: %d", got)
	}
	if err := st.LogEvent(ctx, in); err != nil {
		t.Fatalf("append after rollback = %v", err)
	}
	if got := st.AuditAppendFailures(); got != 1 {
		t.Fatalf("successful append changed counter: %d", got)
	}
	if got := (&Store{}).AuditAppendFailures(); got != 0 {
		t.Fatalf("new Store inherited prior process counter: %d", got)
	}
}

func TestAuditAppendFailuresSurviveSuccessfulRolloutFallbackPostgres(t *testing.T) {
	dsn := testenv.RequirePostgres(t)
	serverStore, workerDSN := newMigrationTestStore(t, dsn)
	if err := serverStore.Migrate(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	workerStore, err := Open(ctx, workerDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer workerStore.Close()

	accountA, operatorA := provisionActiveRolloutAccountForTest(ctx, t, serverStore, "audit-a")
	accountB, operatorB := provisionActiveRolloutAccountForTest(ctx, t, serverStore, "audit-b")
	realmA := createRolloutRealmWithAgentsForTest(ctx, t, serverStore, accountA.AccountID, "audit-a", 1)
	realmB := createRolloutRealmWithAgentsForTest(ctx, t, serverStore, accountB.AccountID, "audit-b", 1)
	publishAvatarStyleForTest(ctx, t, serverStore, operatorA, realmA.ID, 1, 2, "audit-a-v2")
	publishAvatarStyleForTest(ctx, t, serverStore, operatorB, realmB.ID, 1, 2, "audit-b-v2")
	if _, err := serverStore.pool.Exec(ctx, `
		UPDATE avatar_style_rollout_jobs
		   SET updated_at=CASE account_id WHEN $1 THEN statement_timestamp()-interval '2 minutes'
		                                 ELSE statement_timestamp()-interval '1 minute' END`,
		accountA.AccountID); err != nil {
		t.Fatal(err)
	}
	// This isolated schema rejects only A's completion audit INSERT. A's
	// rollout changes must roll back, while B can complete in the same tick.
	constraint := fmt.Sprintf(`ALTER TABLE account_events ADD CONSTRAINT audit_rollout_failure_canary
		CHECK (account_id <> '%s' OR verb <> 'avatar.style.rollout.completed') NOT VALID`,
		strings.ReplaceAll(accountA.AccountID, "'", "''"))
	if _, err := serverStore.pool.Exec(ctx, constraint); err != nil {
		t.Fatal(err)
	}
	result, err := workerStore.ProcessAvatarStyleRolloutBatch(ctx, 2)
	if err != nil || !result.Completed || result.AccountID != accountB.AccountID {
		t.Fatalf("failing A / successful B batch = %#v / %v", result, err)
	}
	// The successful result suppresses RunAvatarStyleRolloutWorker's onError
	// callback. Only the worker Store's separate audit counter sees A's error.
	if got := workerStore.AuditAppendFailures(); got != 1 {
		t.Fatalf("worker audit insert failures = %d, want 1", got)
	}
	if got := serverStore.AuditAppendFailures(); got != 0 {
		t.Fatalf("server Store observed worker-local failure: %d", got)
	}
	var status, failureCode string
	var processed, failures, styleRevision, auditEvents int64
	if err := serverStore.pool.QueryRow(ctx, `
		SELECT j.status, j.processed_profile_count, j.failure_count, j.last_failure_code,
		       p.style_revision,
		       (SELECT count(*) FROM account_events e
		         WHERE e.account_id=j.account_id AND e.verb='avatar.style.rollout.completed')
		  FROM avatar_style_rollout_jobs j
		  JOIN agent_avatar_profiles p ON p.account_id=j.account_id AND p.realm_id=j.realm_id
		 WHERE j.account_id=$1`, accountA.AccountID).Scan(
		&status, &processed, &failures, &failureCode, &styleRevision, &auditEvents); err != nil {
		t.Fatal(err)
	}
	if status != "pending" || processed != 0 || failures != 1 ||
		failureCode != "candidate_failed" || styleRevision != 1 || auditEvents != 0 {
		t.Fatalf("A rollback/backoff = status %q, processed %d, failures %d, code %q, revision %d, audit events %d",
			status, processed, failures, failureCode, styleRevision, auditEvents)
	}
	if err := serverStore.pool.QueryRow(ctx, `
		SELECT count(*) FROM account_events
		 WHERE account_id=$1 AND verb='avatar.style.rollout.completed'`, accountB.AccountID).Scan(&auditEvents); err != nil {
		t.Fatal(err)
	}
	if auditEvents != 1 {
		t.Fatalf("B completion audit events = %d, want 1", auditEvents)
	}

	if _, err := serverStore.pool.Exec(ctx, `ALTER TABLE account_events DROP CONSTRAINT audit_rollout_failure_canary`); err != nil {
		t.Fatal(err)
	}
	if _, err := serverStore.pool.Exec(ctx, `
		UPDATE avatar_style_rollout_jobs SET retry_after=statement_timestamp()-interval '1 second'
		 WHERE account_id=$1`, accountA.AccountID); err != nil {
		t.Fatal(err)
	}
	result, err = workerStore.ProcessAvatarStyleRolloutBatch(ctx, 2)
	if err != nil || !result.Completed || result.AccountID != accountA.AccountID {
		t.Fatalf("A recovery batch = %#v / %v", result, err)
	}
	if got := workerStore.AuditAppendFailures(); got != 1 {
		t.Fatalf("successful retry changed worker failure counter: %d", got)
	}
}
