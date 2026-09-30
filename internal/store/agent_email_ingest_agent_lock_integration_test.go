package store

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/witwave-ai/witself/internal/testenv"
)

// TestAgentEmailIngestAgentLockPostgres pins the answer of the agent lock in
// IngestAgentEmailPilot. A tombstoned agent stays a permanent refusal. Any
// other failure to take the lock is returned as an ordinary error, which the
// relay answers as temporary, and the same delivery succeeds once the lock is
// free.
func TestAgentEmailIngestAgentLockPostgres(t *testing.T) {
	st := cohortStore(t, testenv.RequirePostgres(t))
	const audience = "agent-lock-cell"

	t.Run("tombstoned_agent_permanent", func(t *testing.T) {
		ctx := context.Background()
		account, realm, agent := deferralAccount(t, st, 1)
		listed := cohortScope(account.AccountID, "", audience)
		address, err := st.EnsureAgentEmailMailbox(ctx, listed, account.AccountID, realm.ID, agent.ID, "")
		if err != nil {
			t.Fatalf("create fixture mailbox: %T", err)
		}
		// A committed tombstone without the address retirement that DeleteAgent
		// performs: the route still resolves and only the agent lock can refuse.
		tag, err := st.pool.Exec(ctx, `
			UPDATE agents SET deleted_at=clock_timestamp(), updated_at=clock_timestamp()
			WHERE id=$1 AND deleted_at IS NULL`, agent.ID)
		if err != nil || tag.RowsAffected() != 1 {
			t.Fatalf("tombstone fixture agent: %T rows=%d", err, tag.RowsAffected())
		}
		before := deferralRowCounts(t, st, account.AccountID)
		_, err = st.IngestAgentEmailPilot(ctx, listed, deferralIngestInput(address.Address, audience))
		if !errors.Is(err, ErrAgentEmailPilotNotEnrolled) || errors.Is(err, ErrAgentEmailReceiveCohortDeferred) {
			t.Fatal("a tombstoned agent at the ingest agent lock did not stay a permanent refusal")
		}
		if deferralRowCounts(t, st, account.AccountID) != before {
			t.Fatal("permanent refusal stored a row or debited a rate bucket")
		}
	})

	t.Run("lock_failure_temporary", func(t *testing.T) {
		// Hang guard only; no assertion depends on this bound.
		ctx, stop := context.WithTimeout(context.Background(), 2*time.Minute)
		defer stop()
		account, realm, agent := deferralAccount(t, st, 2)
		listed := cohortScope(account.AccountID, "", audience)
		omitted := cohortScope("acc_zzzzzzzzzzzzzzzz", "", audience)
		address, err := st.EnsureAgentEmailMailbox(ctx, listed, account.AccountID, realm.ID, agent.ID, "")
		if err != nil {
			t.Fatalf("create fixture mailbox: %T", err)
		}
		before := deferralRowCounts(t, st, account.AccountID)

		// Hold the agent row. SELECT ... FOR UPDATE fires no trigger and takes
		// no account lock, so ingest passes its account lock and waits at
		// lockLiveMessageAgentScope's FOR SHARE.
		blocker, err := st.pool.Begin(ctx)
		if err != nil {
			t.Fatalf("begin blocker: %T", err)
		}
		defer func() { _ = blocker.Rollback(context.Background()) }()
		blockerPID := postgresBackendPIDForMessageRaceTest(ctx, t, blocker)
		var lockedID string
		if err := blocker.QueryRow(ctx, `SELECT id FROM agents WHERE id=$1 FOR UPDATE`, agent.ID).Scan(&lockedID); err != nil {
			t.Fatalf("lock fixture agent: %T", err)
		}

		ingestCtx, cancelIngest := context.WithCancel(ctx)
		defer cancelIngest()
		done := make(chan error, 1)
		go func() {
			_, err := st.IngestAgentEmailPilot(ingestCtx, listed, deferralIngestInput(address.Address, audience))
			done <- err
		}()
		waitForPostgresBlockWaiters(ctx, t, st, blockerPID, 1)

		// A left-out account never reaches the agent lock: its scope miss is
		// decided first, so it defers while the lock is held.
		_, err = st.IngestAgentEmailPilot(ctx, omitted, deferralIngestInput(address.Address, audience))
		if !errors.Is(err, ErrAgentEmailReceiveCohortDeferred) {
			t.Fatal("a left-out resident account did not defer while the agent lock was held")
		}

		cancelIngest()
		var lockErr error
		select {
		case lockErr = <-done:
		case <-ctx.Done():
			t.Fatal("ingest did not return after its context was cancelled")
		}
		if lockErr == nil ||
			errors.Is(lockErr, ErrAgentEmailPilotNotEnrolled) ||
			errors.Is(lockErr, ErrAgentEmailUnknownRecipient) ||
			errors.Is(lockErr, ErrAgentEmailReceiveCohortDeferred) ||
			errors.Is(lockErr, ErrAgentNotFound) ||
			!strings.HasPrefix(lockErr.Error(), "lock live message agent: ") {
			t.Fatalf("a failed agent lock was not returned as an ordinary error: %T", lockErr)
		}
		t.Logf("agent-lock failure matches context.Canceled: %t", errors.Is(lockErr, context.Canceled))
		if err := blocker.Rollback(ctx); err != nil {
			t.Fatalf("release blocker: %T", err)
		}
		if deferralRowCounts(t, st, account.AccountID) != before {
			t.Fatal("failed ingest stored a row or debited a rate bucket")
		}

		// The same delivery succeeds once the lock is free: the failure was
		// temporary, not a property of the recipient.
		message, err := st.IngestAgentEmailPilot(ctx, listed, deferralIngestInput(address.Address, audience))
		if err != nil || message.ID == "" {
			t.Fatalf("retry after the agent-lock failure was not accepted: %T", err)
		}
	})
}
