package store

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/witwave-ai/witself/internal/testenv"
)

// TestAgentEmailCustomDomainRereadLockPostgres pins the answer of the locked
// custom-domain route re-read in agentEmailRouteByRecipientTx. A failure to
// read the route under lock is returned as an ordinary error, which the relay
// answers as temporary, and the same delivery succeeds once the lock is free.
func TestAgentEmailCustomDomainRereadLockPostgres(t *testing.T) {
	f := newCustomDomainEmailFixture(t, testenv.RequirePostgres(t), "reread-lock")
	// Hang guard only; no assertion depends on this bound.
	ctx, stop := context.WithTimeout(context.Background(), 2*time.Minute)
	defer stop()
	if _, err := f.store.ApplyAgentEmailCustomDomainRoute(ctx, f.accountID, f.customInput); err != nil {
		t.Fatalf("apply fixture custom-domain route: %T", err)
	}
	// Production receive with the account in the cohort, as on a live cell.
	listed := cohortScope(f.accountID, "", f.scope.Audience)
	recipient := "owner.founder+signup@" + f.customInput.Domain
	before := deferralRowCounts(t, f.store, f.accountID)

	// Hold the route row. SELECT ... FOR UPDATE fires no trigger and takes no
	// account, agent or alias lock, so ingest passes those locks and waits at
	// the locked re-read in agentEmailCustomDomainRouteByIdentityTx.
	blocker, err := f.store.pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin blocker: %T", err)
	}
	defer func() { _ = blocker.Rollback(context.Background()) }()
	blockerPID := postgresBackendPIDForMessageRaceTest(ctx, t, blocker)
	var lockedID string
	if err := blocker.QueryRow(ctx, `
		SELECT domain_request_id FROM agent_email_custom_domain_routes
		WHERE domain_request_id=$1 AND realm_alias_claim_id=$2 FOR UPDATE`,
		f.customInput.DomainRequestID, f.customInput.RealmAliasClaimID,
	).Scan(&lockedID); err != nil {
		t.Fatalf("lock fixture custom-domain route: %T", err)
	}

	ingestCtx, cancelIngest := context.WithCancel(ctx)
	defer cancelIngest()
	done := make(chan error, 1)
	go func() {
		_, err := f.store.IngestAgentEmailPilot(ingestCtx, listed, deferralIngestInput(recipient, listed.Audience))
		done <- err
	}()
	waitForPostgresBlockWaiters(ctx, t, f.store, blockerPID, 1)

	cancelIngest()
	var rereadErr error
	select {
	case rereadErr = <-done:
	case <-ctx.Done():
		t.Fatal("ingest did not return after its context was cancelled")
	}
	if rereadErr == nil ||
		errors.Is(rereadErr, ErrAgentEmailUnknownRecipient) ||
		errors.Is(rereadErr, ErrAgentEmailPilotNotEnrolled) ||
		errors.Is(rereadErr, ErrAgentEmailReceiveCohortDeferred) ||
		errors.Is(rereadErr, ErrAgentEmailCustomDomainRouteNotFound) ||
		!strings.HasPrefix(rereadErr.Error(), "read custom-domain route identity: ") {
		t.Fatalf("a failed custom-domain route re-read was not returned as an ordinary error: %T", rereadErr)
	}
	t.Logf("custom-domain re-read failure matches context.Canceled: %t", errors.Is(rereadErr, context.Canceled))
	if err := blocker.Rollback(ctx); err != nil {
		t.Fatalf("release blocker: %T", err)
	}
	if deferralRowCounts(t, f.store, f.accountID) != before {
		t.Fatal("failed ingest stored a row or debited a rate bucket")
	}

	// The same delivery succeeds once the lock is free: the failure was
	// temporary, not a property of the recipient.
	message, err := f.store.IngestAgentEmailPilot(ctx, listed, deferralIngestInput(recipient, listed.Audience))
	if err != nil || message.ID == "" ||
		message.RecipientCustomDomainRequestID != f.customInput.DomainRequestID {
		t.Fatalf("retry after the custom-domain re-read failure was not accepted: %T", err)
	}
}
