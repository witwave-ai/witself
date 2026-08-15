package store

import (
	"bytes"
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/witwave-ai/witself/internal/plans"
)

func TestAgentEmailSuppressionRetentionDaysIsBounded(t *testing.T) {
	finite := 30
	large := maxAgentEmailSuppressionRetentionDays
	if got := agentEmailSuppressionRetentionDays(&finite); got != 395 {
		t.Fatalf("30-day message suppression lifetime = %d, want 395", got)
	}
	if got := agentEmailSuppressionRetentionDays(&large); got != maxAgentEmailSuppressionRetentionDays {
		t.Fatalf("large finite suppression lifetime = %d, want cap %d",
			got, maxAgentEmailSuppressionRetentionDays)
	}
	if got := agentEmailSuppressionRetentionDays(nil); got != maxAgentEmailSuppressionRetentionDays {
		t.Fatalf("indefinite-message suppression lifetime = %d, want cap %d",
			got, maxAgentEmailSuppressionRetentionDays)
	}
}

func TestAgentEmailOutboundRetentionLifecyclePostgres(t *testing.T) {
	baseDSN := os.Getenv("WITSELF_TEST_DATABASE_URL")
	if baseDSN == "" {
		t.Skip("WITSELF_TEST_DATABASE_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	st, _ := newMigrationTestStore(t, baseDSN)
	if err := st.Migrate(); err != nil {
		t.Fatal(err)
	}
	fixture := newAgentEmailRetentionAccountFixture(ctx, t, st, "outbound")
	localPart, _, ok := strings.Cut(fixture.address.Address, "@")
	if !ok {
		t.Fatalf("fixture address = %q", fixture.address.Address)
	}
	old := time.Now().UTC().Add(-31 * 24 * time.Hour)
	recent := time.Now().UTC().Add(-time.Hour)
	if _, err := st.SetAccountPlan(
		ctx, fixture.accountID, 0, "", "test", map[string]int64{},
		map[string]int64{
			plans.AgentEmailEntitlementVersionPolicy: plans.AgentEmailEntitlementVersion,
			plans.AgentEmailRetentionDaysPolicy:      30,
		},
		[]string{plans.AgentEmailReceiveFeature, plans.AgentEmailSendFeature},
	); err != nil {
		t.Fatal(err)
	}
	if _, err := st.pool.Exec(ctx, `
		INSERT INTO agent_email_address_domains
		  (account_id,realm_id,provisioned_agent_id,address_id,domain,local_part)
		VALUES ($1,$2,$3,$4,'witmail.net',$5)`, fixture.accountID,
		fixture.realmID, fixture.owner.ID, fixture.address.ID, localPart); err != nil {
		t.Fatal(err)
	}
	queue := func(recipient, key string) AgentEmailOutboundMessage {
		t.Helper()
		msg, err := st.QueueAgentEmail(ctx, fixture.owner, SendAgentEmailInput{
			To: recipient, Subject: "retention", Text: "bounded body",
			IdempotencyKey: key,
		})
		if err != nil {
			t.Fatal(err)
		}
		return msg
	}
	claim := func(wantID string) AgentEmailOutboundDispatch {
		t.Helper()
		dispatch, err := st.ClaimAgentEmailOutbound(ctx, AgentEmailOutboundClaimInput{})
		if err != nil || dispatch.Message.ID != wantID {
			t.Fatalf("claim outbound %s = %#v / %v", wantID, dispatch, err)
		}
		return dispatch
	}
	start := func(dispatch AgentEmailOutboundDispatch) AgentEmailOutboundDispatch {
		t.Helper()
		started, err := st.StartAgentEmailOutboundProviderCall(
			ctx, dispatch.Message.ID, dispatch.Claim,
		)
		if err != nil {
			t.Fatal(err)
		}
		return started
	}
	accept := func(dispatch AgentEmailOutboundDispatch, providerID string) {
		t.Helper()
		if _, err := st.FinalizeAgentEmailOutbound(
			ctx, dispatch.Message.ID, FinalizeAgentEmailOutboundInput{
				Claim: dispatch.Claim, State: AgentEmailOutboundAccepted,
				Provider: "cloudflare", ProviderMessageID: providerID,
			},
		); err != nil {
			t.Fatal(err)
		}
	}

	oldTerminal := queue("old@example.com", "retention-old")
	oldDispatch := start(claim(oldTerminal.ID))
	accept(oldDispatch, "cloudflare-old")
	if _, err := st.ApplyAgentEmailOutboundProviderEvent(ctx,
		AgentEmailOutboundProviderEventInput{
			Provider: "cloudflare", EventID: "retention-delivered",
			ProviderMessageID: "cloudflare-old",
			EventClass:        AgentEmailOutboundProviderEventDelivered,
			OccurredAt:        old.Add(2 * time.Minute),
		}); err != nil {
		t.Fatal(err)
	}
	claimed := queue("claimed@example.com", "retention-claimed")
	_ = claim(claimed.ID)
	providerStarted := queue("started@example.com", "retention-started")
	_ = start(claim(providerStarted.ID))
	recentTerminal := queue("recent@example.com", "retention-recent")
	recentDispatch := start(claim(recentTerminal.ID))
	accept(recentDispatch, "cloudflare-recent")
	if _, err := st.pool.Exec(ctx, `
		UPDATE agent_email_outbound_messages
		   SET created_at=$2::timestamptz,queued_at=$2::timestamptz,
		       provider_started_at=CASE WHEN provider_started_at IS NULL THEN NULL ELSE $2::timestamptz END,
		       accepted_at=CASE WHEN accepted_at IS NULL THEN NULL ELSE $2::timestamptz END,
		       delivered_at=CASE WHEN delivered_at IS NULL THEN NULL ELSE $2::timestamptz END,
		       updated_at=$2::timestamptz
		 WHERE id=ANY($1::text[])`,
		[]string{oldTerminal.ID, claimed.ID, providerStarted.ID}, old); err != nil {
		t.Fatal(err)
	}
	if _, err := st.pool.Exec(ctx, `
		UPDATE agent_email_outbound_provider_events
		   SET occurred_at=$2,received_at=$2
		 WHERE outbound_id=$1`, oldTerminal.ID, old.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, err := st.pool.Exec(ctx, `
		INSERT INTO agent_email_outbound_recipient_suppressions
		  (account_id,recipient_sha256,reason,source_send_id,provider,created_at,updated_at)
		VALUES
		  ($1,$2,'hard_bounce',$4,'cloudflare',$6,$6),
		  ($1,$3,'complained',$5,'cloudflare',$7,$7)`,
		fixture.accountID, strings.Repeat("c", 64), strings.Repeat("d", 64),
		oldTerminal.ID, recentTerminal.ID,
		time.Now().UTC().Add(-396*24*time.Hour), recent); err != nil {
		t.Fatal(err)
	}

	configureAgentEmailRetentionTestLanes(
		ctx, t, st, AgentEmailRetentionModePreview, fixture.laneID,
	)
	preview, err := st.PreviewAgentEmailRetentionBatch(ctx, 25)
	if err != nil {
		t.Fatal(err)
	}
	if preview.ScannedProviderEvents != 1 || preview.DeletedProviderEvents != 0 ||
		preview.DeletedOutbound != 0 || preview.ScannedSuppressions != 1 ||
		preview.DeletedSuppressions != 0 {
		t.Fatalf("outbound retention preview = %+v", preview)
	}

	configureAgentEmailRetentionTestLanes(
		ctx, t, st, AgentEmailRetentionModeEnforce, fixture.laneID,
	)
	enforced, err := st.ProcessAgentEmailRetentionBatch(ctx, 25)
	if err != nil {
		t.Fatal(err)
	}
	if enforced.ScannedProviderEvents != 1 || enforced.DeletedProviderEvents != 1 ||
		enforced.ScannedOutbound != 1 || enforced.EligibleOutbound != 1 ||
		enforced.DeletedOutbound != 1 || enforced.DeletedOutboundBytes == 0 ||
		enforced.ScannedSuppressions != 1 || enforced.DeletedSuppressions != 1 {
		t.Fatalf("outbound retention enforce = %+v", enforced)
	}
	assertAgentEmailOutboundRetentionCount(
		ctx, t, st, oldTerminal.ID, 0,
	)
	assertAgentEmailOutboundRetentionCount(
		ctx, t, st, claimed.ID, 1,
	)
	assertAgentEmailOutboundRetentionCount(
		ctx, t, st, providerStarted.ID, 1,
	)
	assertAgentEmailOutboundRetentionCount(
		ctx, t, st, recentTerminal.ID, 1,
	)
	var events, suppressions int64
	if err := st.pool.QueryRow(ctx, `
		SELECT
		  (SELECT count(*) FROM agent_email_outbound_provider_events),
		  (SELECT count(*) FROM agent_email_outbound_recipient_suppressions
		    WHERE account_id=$1)`, fixture.accountID).Scan(&events, &suppressions); err != nil {
		t.Fatal(err)
	}
	if events != 0 || suppressions != 1 {
		t.Fatalf("retained events/suppressions = %d/%d, want 0/1", events, suppressions)
	}

	// A terminal row locked by another transaction is skipped immediately and
	// remains intact. Once the lock is released, the next bounded pass removes it.
	if _, err := st.pool.Exec(ctx, `
		UPDATE agent_email_outbound_messages
		   SET created_at=$2,queued_at=$2,provider_started_at=$2,
		       accepted_at=$2,updated_at=$2
		 WHERE id=$1`, recentTerminal.ID, old); err != nil {
		t.Fatal(err)
	}
	locker, err := st.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := locker.Exec(ctx, `
		SELECT 1 FROM agent_email_outbound_messages
		 WHERE id=$1 FOR UPDATE`, recentTerminal.ID); err != nil {
		_ = locker.Rollback(ctx)
		t.Fatal(err)
	}
	configureAgentEmailRetentionTestLanes(
		ctx, t, st, AgentEmailRetentionModeEnforce, fixture.laneID,
	)
	lockedCtx, cancelLocked := context.WithTimeout(ctx, 2*time.Second)
	lockedResult, err := st.ProcessAgentEmailRetentionBatch(lockedCtx, 25)
	cancelLocked()
	if err != nil {
		_ = locker.Rollback(ctx)
		t.Fatal(err)
	}
	if lockedResult.DeletedOutbound != 0 {
		_ = locker.Rollback(ctx)
		t.Fatalf("locked outbound retention result = %+v", lockedResult)
	}
	assertAgentEmailOutboundRetentionCount(
		ctx, t, st, recentTerminal.ID, 1,
	)
	if err := locker.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	configureAgentEmailRetentionTestLanes(
		ctx, t, st, AgentEmailRetentionModeEnforce, fixture.laneID,
	)
	afterUnlock, err := st.ProcessAgentEmailRetentionBatch(ctx, 25)
	if err != nil {
		t.Fatal(err)
	}
	if afterUnlock.DeletedOutbound != 1 {
		t.Fatalf("unlocked outbound retention result = %+v", afterUnlock)
	}
	assertAgentEmailOutboundRetentionCount(
		ctx, t, st, recentTerminal.ID, 0,
	)
}

func TestAgentEmailOutboundRetainedReplyAndClaimArchiveRoundTripPostgres(t *testing.T) {
	baseDSN := os.Getenv("WITSELF_TEST_DATABASE_URL")
	if baseDSN == "" {
		t.Skip("WITSELF_TEST_DATABASE_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	source, _ := newMigrationTestStore(t, baseDSN)
	destination, _ := newMigrationTestStore(t, baseDSN)
	if err := source.Migrate(); err != nil {
		t.Fatal(err)
	}
	if err := destination.Migrate(); err != nil {
		t.Fatal(err)
	}

	fixture := newAgentEmailRetentionAccountFixture(ctx, t, source, "reply-archive")
	if _, err := source.SetAccountPlan(
		ctx, fixture.accountID, 0, "", "test", map[string]int64{},
		map[string]int64{
			plans.AgentEmailEntitlementVersionPolicy: plans.AgentEmailEntitlementVersion,
			plans.AgentEmailRetentionDaysPolicy:      30,
		},
		[]string{plans.AgentEmailReceiveFeature, plans.AgentEmailSendFeature},
	); err != nil {
		t.Fatal(err)
	}
	localPart, _, ok := strings.Cut(fixture.address.Address, "@")
	if !ok {
		t.Fatalf("fixture address = %q", fixture.address.Address)
	}
	if _, err := source.pool.Exec(ctx, `
		INSERT INTO agent_email_address_domains
		  (account_id,realm_id,provisioned_agent_id,address_id,domain,local_part)
		VALUES ($1,$2,$3,$4,'witmail.net',$5)`, fixture.accountID,
		fixture.realmID, fixture.owner.ID, fixture.address.ID, localPart); err != nil {
		t.Fatal(err)
	}

	inbound := ingestAgentEmailRetentionFixture(
		ctx, t, source, fixture, "reply parent expires first",
	)
	reply, err := source.ReplyAgentEmail(ctx, fixture.owner, inbound.ID, ReplyAgentEmailInput{
		Text: "newer retained reply", IdempotencyKey: "retained-reply-archive",
	})
	if err != nil {
		t.Fatal(err)
	}
	old := time.Now().UTC().Add(-31 * 24 * time.Hour)
	if _, err := source.pool.Exec(ctx, `
		UPDATE agent_email_messages
		   SET received_at=$2,created_at=$2
		 WHERE account_id=$1 AND id=$3`, fixture.accountID, old, inbound.ID); err != nil {
		t.Fatal(err)
	}
	configureAgentEmailRetentionTestLanes(
		ctx, t, source, AgentEmailRetentionModeEnforce, fixture.laneID,
	)
	retained, err := source.ProcessAgentEmailRetentionBatch(ctx, 25)
	if err != nil {
		t.Fatal(err)
	}
	if retained.Deleted != 1 || retained.DeletedOutbound != 0 {
		t.Fatalf("reply-parent retention result = %+v", retained)
	}
	assertAgentEmailRetentionMessageCount(ctx, t, source, inbound.ID, 0)
	assertAgentEmailOutboundRetentionCount(ctx, t, source, reply.ID, 1)

	claimed, err := source.ClaimAgentEmailOutbound(ctx, AgentEmailOutboundClaimInput{})
	if err != nil || claimed.Message.ID != reply.ID {
		t.Fatalf("claim retained reply = %#v / %v", claimed, err)
	}
	const evacuationID = "evac_email_reply_claim_archive"
	if _, err := source.BeginAccountEvacuation(
		ctx, fixture.accountID, evacuationID, "retained reply archive round trip",
	); err != nil {
		t.Fatal(err)
	}
	var archive bytes.Buffer
	if err := source.ExportAccountEvacuation(
		ctx, fixture.accountID, evacuationID, "reply-source", "test", &archive,
	); err != nil {
		t.Fatal(err)
	}
	if _, disposition, err := destination.ImportAccountEvacuation(
		ctx, fixture.accountID, evacuationID, bytes.NewReader(archive.Bytes()),
	); err != nil {
		t.Fatal(err)
	} else if disposition.EvacuationRole != "target" || disposition.AlreadyImported {
		t.Fatalf("retained reply import disposition = %#v", disposition)
	}

	var (
		state, replyTarget, lastError string
		generation                    int64
		claimID                       *string
		lease                         *time.Time
		nextAttempt, updatedAt        time.Time
		inboundCount                  int64
	)
	if err := destination.pool.QueryRow(ctx, `
		SELECT state,reply_to_inbound_message_id,last_error_code,
		       claim_generation,claim_id,lease_expires_at,next_attempt_at,updated_at
		  FROM agent_email_outbound_messages
		 WHERE account_id=$1 AND id=$2`, fixture.accountID, reply.ID).Scan(
		&state, &replyTarget, &lastError, &generation, &claimID, &lease,
		&nextAttempt, &updatedAt,
	); err != nil {
		t.Fatal(err)
	}
	if err := destination.pool.QueryRow(ctx, `
		SELECT count(*) FROM agent_email_messages
		 WHERE account_id=$1 AND id=$2`, fixture.accountID, inbound.ID,
	).Scan(&inboundCount); err != nil {
		t.Fatal(err)
	}
	if state != AgentEmailOutboundQueued || replyTarget != inbound.ID ||
		lastError != AgentEmailOutboundErrorWorkerLeaseExpired ||
		generation != claimed.Claim.Generation+1 || claimID != nil || lease != nil ||
		!nextAttempt.Equal(updatedAt) || inboundCount != 0 {
		t.Fatalf(
			"restored retained reply = state:%q target:%q error:%q generation:%d claim:%v lease:%v next:%s updated:%s inbound:%d",
			state, replyTarget, lastError, generation, claimID, lease,
			nextAttempt, updatedAt, inboundCount,
		)
	}
}

func assertAgentEmailOutboundRetentionCount(
	ctx context.Context,
	t *testing.T,
	st *Store,
	sendID string,
	want int64,
) {
	t.Helper()
	var got int64
	if err := st.pool.QueryRow(ctx, `
		SELECT count(*) FROM agent_email_outbound_messages WHERE id=$1`, sendID,
	).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("outbound %s count = %d, want %d", sendID, got, want)
	}
}
