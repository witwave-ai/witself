package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/witwave-ai/witself/internal/agentemail"
	"github.com/witwave-ai/witself/internal/plans"
	"github.com/witwave-ai/witself/internal/testenv"
)

func TestAgentEmailOutboundLifecyclePostgres(t *testing.T) {
	baseDSN := testenv.RequirePostgres(t)
	ctx := context.Background()
	st, _ := newMigrationTestStore(t, baseDSN)
	if err := st.Migrate(); err != nil {
		t.Fatal(err)
	}
	provisioned, err := st.ProvisionAccount(ctx,
		"outbound-email-store@witwave.ai", "outbound email store", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if activated, err := st.ActivateAccount(ctx, provisioned.AccountID); err != nil || !activated {
		t.Fatalf("activate = %t / %v", activated, err)
	}
	realm, err := st.CreateRealm(ctx, provisioned.AccountID, "outbound email")
	if err != nil {
		t.Fatal(err)
	}
	agent, err := st.CreateAgent(ctx, provisioned.AccountID, realm.ID, "sender")
	if err != nil {
		t.Fatal(err)
	}
	enrolledAgentIDs := map[string]bool{agent.ID: true}
	var otherAgent Agent
	for _, name := range []string{"helper one", "helper two", "helper three", "helper four"} {
		helper, err := st.CreateAgent(ctx, provisioned.AccountID, realm.ID, name)
		if err != nil {
			t.Fatal(err)
		}
		if otherAgent.ID == "" {
			otherAgent = helper
		}
		enrolledAgentIDs[helper.ID] = true
	}
	p := Principal{
		Kind: PrincipalAgent, ID: agent.ID, AccountID: provisioned.AccountID,
		RealmID: realm.ID, AgentName: agent.Name, AccountStatus: "active",
		AccessProfile: AccessProfileFull,
	}
	scope := AgentEmailPilotScope{
		Enabled: true, Domain: "witmail.net", Audience: "outbound-store-test",
		RealmIDs: map[string]bool{realm.ID: true},
		AgentIDs: enrolledAgentIDs,
	}
	addresses, err := st.ReconcileAgentEmailPilot(ctx, scope)
	if err != nil || len(addresses) != len(enrolledAgentIDs) {
		t.Fatalf("reconcile address = %#v / %v", addresses, err)
	}
	var address AgentEmailAddress
	for _, candidate := range addresses {
		if candidate.OwnerAgentID == agent.ID {
			address = candidate
			break
		}
	}
	if address.ID == "" {
		t.Fatalf("sender address missing from %#v", addresses)
	}

	applyPlan := func(revision int64, limits map[string]int64, features []string) {
		t.Helper()
		policies := map[string]int64{
			plans.AgentEmailEntitlementVersionPolicy: plans.AgentEmailEntitlementVersion,
			plans.AgentEmailRetentionDaysPolicy:      90,
		}
		hash, err := plans.SnapshotHash("test", limits, policies, features)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := st.SetAccountPlan(ctx, provisioned.AccountID, revision,
			hash, "test", limits, policies, features); err != nil {
			t.Fatal(err)
		}
	}
	applyPlan(1, map[string]int64{}, []string{
		plans.AgentEmailReceiveFeature, plans.AgentEmailSendFeature,
	})
	controlCounts := func() (int64, int64) {
		t.Helper()
		var realmControls, agentControls int64
		if err := st.pool.QueryRow(ctx, `
			SELECT
			  (SELECT count(*) FROM agent_email_realm_send_controls WHERE account_id=$1),
			  (SELECT count(*) FROM agent_email_send_controls WHERE account_id=$1)`,
			p.AccountID).Scan(&realmControls, &agentControls); err != nil {
			t.Fatal(err)
		}
		return realmControls, agentControls
	}
	if realmControls, agentControls := controlCounts(); realmControls != 0 || agentControls != 0 {
		t.Fatalf("initial send control counts = realm %d agent %d", realmControls, agentControls)
	}
	if err := st.RequireAgentEmailSendEnabled(ctx, p); err != nil {
		t.Fatalf("read-only send entitlement = %v", err)
	}
	if realmControls, agentControls := controlCounts(); realmControls != 0 || agentControls != 0 {
		t.Fatalf("send entitlement materialized controls = realm %d agent %d", realmControls, agentControls)
	}

	directInput := SendAgentEmailInput{
		To: "recipient@example.com", Subject: "hello", Text: "plain body",
		IdempotencyKey: "direct-1",
	}
	direct, err := st.QueueAgentEmail(ctx, p, directInput)
	if err != nil {
		t.Fatal(err)
	}
	if direct.State != AgentEmailOutboundQueued ||
		direct.FromAddress != address.LocalPart+"@send.witmail.net" ||
		direct.ReplyToAddress != address.LocalPart+"@witmail.net" ||
		direct.ToAddress != directInput.To || direct.ID == "" {
		t.Fatalf("queued direct = %#v", direct)
	}
	if realmControls, agentControls := controlCounts(); realmControls != 1 || agentControls != 1 {
		t.Fatalf("queue send control counts = realm %d agent %d, want 1/1", realmControls, agentControls)
	}
	replay, err := st.QueueAgentEmail(ctx, p, directInput)
	if err != nil || replay.ID != direct.ID {
		t.Fatalf("exact replay = %#v / %v", replay, err)
	}
	changed := directInput
	changed.Text = "changed"
	if _, err := st.QueueAgentEmail(ctx, p, changed); !errors.Is(err, ErrAgentEmailOutboundConflict) {
		t.Fatalf("changed replay error = %v", err)
	}
	shown, err := st.GetAgentEmailOutbound(ctx, p, direct.ID)
	if err != nil || shown.bodyText != "" || shown.ProviderMessageID != "" {
		t.Fatalf("public get leaked private fields = %#v / %v", shown, err)
	}
	page, err := st.ListAgentEmailOutbox(ctx, p, AgentEmailOutboundFilter{Limit: 1})
	if err != nil || len(page.Messages) != 1 || page.Messages[0].ID != direct.ID ||
		page.Messages[0].bodyText != "" {
		t.Fatalf("outbox page = %#v / %v", page, err)
	}
	otherPrincipal := Principal{
		Kind: PrincipalAgent, ID: otherAgent.ID, AccountID: provisioned.AccountID,
		RealmID: realm.ID, AgentName: otherAgent.Name, AccountStatus: "active",
		AccessProfile: AccessProfileFull,
	}
	if _, err := st.GetAgentEmailOutbound(ctx, otherPrincipal, direct.ID); !errors.Is(err, ErrAgentEmailOutboundNotFound) {
		t.Fatalf("cross-owner outbound get error = %v", err)
	}
	otherPage, err := st.ListAgentEmailOutbox(ctx, otherPrincipal, AgentEmailOutboundFilter{Limit: 10})
	if err != nil || len(otherPage.Messages) != 0 {
		t.Fatalf("cross-owner outbox page = %#v / %v", otherPage, err)
	}
	restricted := p
	restricted.AccessProfile = AccessProfileCuratorPreview
	if _, err := st.GetAgentEmailOutbound(ctx, restricted, direct.ID); !errors.Is(err, ErrAgentEmailOutboundForbidden) {
		t.Fatalf("restricted-profile outbound get error = %v", err)
	}
	if _, err := st.ListAgentEmailOutbox(ctx, restricted, AgentEmailOutboundFilter{Limit: 10}); !errors.Is(err, ErrAgentEmailOutboundForbidden) {
		t.Fatalf("restricted-profile outbox list error = %v", err)
	}
	providerEventCanarySend, err := st.QueueAgentEmail(ctx, p, SendAgentEmailInput{
		To: "provider-event-canary@example.com", Subject: "provider event canary",
		Text: "disposable", IdempotencyKey: "provider-event-canary",
	})
	if err != nil {
		t.Fatal(err)
	}

	raw := []byte("From: Header Sender <sender@example.com>\r\n" +
		"Reply-To: Reply Desk <reply@example.com>\r\n" +
		"To: " + address.Address + "\r\n" +
		"Subject: inbound topic\r\n" +
		"Message-ID: <thread-1@example.com>\r\n\r\ninbound")
	digest := sha256.Sum256(raw)
	inbound, err := st.IngestAgentEmailPilot(ctx, scope, AgentEmailIngestInput{
		Relay: agentemail.RelayMetadata{
			Timestamp: time.Now().Unix(), KeyID: "outbound-test", Audience: scope.Audience,
			EnvelopeSender: "bounce+thread@example.net", EnvelopeRecipient: address.Address,
			RawSize: int64(len(raw)), RawSHA256: hex.EncodeToString(digest[:]),
		},
		Raw: raw,
	})
	if err != nil {
		t.Fatal(err)
	}
	reply, err := st.ReplyAgentEmail(ctx, p, inbound.ID, ReplyAgentEmailInput{
		Text: "reply body", IdempotencyKey: "reply-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if reply.ToAddress != "reply@example.com" ||
		reply.Subject != "Re: inbound topic" || reply.ThreadKey != inbound.ID ||
		reply.ReplyToInboundMessageID != inbound.ID {
		t.Fatalf("derived reply = %#v", reply)
	}
	if _, err := st.ReplyAgentEmail(ctx, otherPrincipal, inbound.ID, ReplyAgentEmailInput{
		Text: "cross-owner reply", IdempotencyKey: "reply-cross-owner",
	}); !errors.Is(err, ErrAgentEmailOutboundNotFound) {
		t.Fatalf("cross-owner reply error = %v", err)
	}
	if _, err := st.ReplyAgentEmail(ctx, restricted, inbound.ID, ReplyAgentEmailInput{
		Text: "restricted reply", IdempotencyKey: "reply-restricted",
	}); !errors.Is(err, ErrAgentEmailOutboundForbidden) {
		t.Fatalf("restricted-profile reply error = %v", err)
	}

	// An expired claim that never crossed the provider boundary re-enters the
	// safe retry path and is immediately claimable by another replica.
	firstClaim, err := st.ClaimAgentEmailOutbound(ctx, AgentEmailOutboundClaimInput{})
	if err != nil || firstClaim.Message.ID != direct.ID {
		t.Fatalf("first claim = %#v / %v", firstClaim, err)
	}
	if _, err := st.pool.Exec(ctx, `
		UPDATE agent_email_outbound_messages
		   SET lease_expires_at=clock_timestamp()-interval '1 second'
		 WHERE id=$1`, direct.ID); err != nil {
		t.Fatal(err)
	}
	reclaimed, err := st.ClaimAgentEmailOutbound(ctx, AgentEmailOutboundClaimInput{})
	if err != nil || reclaimed.Message.ID != direct.ID ||
		reclaimed.Claim.Generation != firstClaim.Claim.Generation+1 ||
		reclaimed.Claim.ClaimID == firstClaim.Claim.ClaimID {
		t.Fatalf("reclaimed pre-provider send = %#v / %v", reclaimed, err)
	}
	started, err := st.StartAgentEmailOutboundProviderCall(
		ctx, direct.ID, reclaimed.Claim,
	)
	if err != nil || started.Message.State != AgentEmailOutboundProviderStarted ||
		started.Text != directInput.Text {
		t.Fatalf("provider start = %#v / %v", started, err)
	}
	var admissionMinuteBuckets, admissionDailyBuckets int
	var dispatchMinuteBuckets, dispatchDailyBuckets, accountBuckets, recipientBuckets int
	if err := st.pool.QueryRow(ctx, `
		SELECT count(*) FILTER (WHERE lane='admission'),
		       count(*) FILTER (WHERE lane='admission_daily'),
		       count(*) FILTER (WHERE lane='dispatch'),
		       count(*) FILTER (WHERE lane='dispatch_daily'),
		       count(*) FILTER (WHERE scope='account' AND realm_id=''),
		       count(*) FILTER (WHERE scope='recipient' AND realm_id='')
		  FROM agent_email_outbound_rate_buckets WHERE account_id=$1`,
		p.AccountID).Scan(
		&admissionMinuteBuckets, &admissionDailyBuckets,
		&dispatchMinuteBuckets, &dispatchDailyBuckets,
		&accountBuckets, &recipientBuckets,
	); err != nil {
		t.Fatal(err)
	}
	if admissionMinuteBuckets != 3 || admissionDailyBuckets != 4 ||
		dispatchMinuteBuckets != 3 || dispatchDailyBuckets != 2 ||
		accountBuckets != 4 || recipientBuckets != 4 {
		t.Fatalf(
			"outbound rate buckets admission=%d/%d dispatch=%d/%d account=%d recipient=%d",
			admissionMinuteBuckets, admissionDailyBuckets,
			dispatchMinuteBuckets, dispatchDailyBuckets,
			accountBuckets, recipientBuckets,
		)
	}
	// The general agent-email maintenance lane calls this outbound-specific
	// bounded primitive alongside the inbound table. Stale debt drains in fixed
	// batches while live debt remains protected by the database-time predicate.
	if _, err := st.pool.Exec(ctx, `
		UPDATE agent_email_outbound_rate_buckets
		   SET updated_at=clock_timestamp()-interval '2 minutes',
		       theoretical_arrival_microseconds=
		         floor(extract(epoch FROM clock_timestamp()-interval '1 minute')*1000000)::bigint
		 WHERE account_id=$1`, p.AccountID); err != nil {
		t.Fatal(err)
	}
	for index, want := range []int64{2, 2, 2, 2, 2, 2, 0} {
		deleted, err := st.DeleteStaleAgentEmailOutboundRateBuckets(
			ctx, time.Now().UTC(), 2,
		)
		if err != nil || deleted != want {
			t.Fatalf("outbound rate cleanup pass %d deleted=%d / %v, want %d",
				index+1, deleted, err, want)
		}
	}
	var rateBucketCountBeforeReplay int64
	if err := st.pool.QueryRow(ctx, `
		SELECT count(*) FROM agent_email_outbound_rate_buckets WHERE account_id=$1`,
		p.AccountID,
	).Scan(&rateBucketCountBeforeReplay); err != nil {
		t.Fatal(err)
	}
	if _, err := st.StartAgentEmailOutboundProviderCall(ctx, direct.ID, reclaimed.Claim); !errors.Is(err, ErrAgentEmailOutboundProviderAlreadyStarted) {
		t.Fatalf("replayed provider start error = %v", err)
	}
	var rateBucketCountAfterReplay int64
	if err := st.pool.QueryRow(ctx, `
		SELECT count(*) FROM agent_email_outbound_rate_buckets WHERE account_id=$1`,
		p.AccountID,
	).Scan(&rateBucketCountAfterReplay); err != nil {
		t.Fatal(err)
	}
	if rateBucketCountAfterReplay != rateBucketCountBeforeReplay {
		t.Fatalf(
			"provider receipt replay changed dispatch rate buckets: before=%d after=%d",
			rateBucketCountBeforeReplay, rateBucketCountAfterReplay,
		)
	}
	accepted, err := st.FinalizeAgentEmailOutbound(ctx, direct.ID,
		FinalizeAgentEmailOutboundInput{
			Claim: reclaimed.Claim, State: AgentEmailOutboundAccepted,
			Provider: "cloudflare_email_sending", ProviderMessageID: "provider-direct-1",
		})
	if err != nil || accepted.State != AgentEmailOutboundAccepted ||
		accepted.ProviderState != AgentEmailOutboundAccepted || accepted.AcceptedAt == nil {
		t.Fatalf("accepted = %#v / %v", accepted, err)
	}
	var sentUsage int
	if err := st.pool.QueryRow(ctx, `
		SELECT count(*) FROM usage_events
		 WHERE account_id=$1 AND dimension='email_sent' AND subject_id=$2`,
		p.AccountID, direct.ID).Scan(&sentUsage); err != nil || sentUsage != 1 {
		t.Fatalf("email_sent usage = %d / %v", sentUsage, err)
	}
	// Preserve a provider's skewed historical timestamp in its receipt while
	// clamping the folded message lifecycle to the local provider boundary so
	// the live row remains portable between cells.
	eventTime := accepted.CreatedAt.Add(-time.Hour)
	event := AgentEmailOutboundProviderEventInput{
		Provider: "cloudflare_email_sending", EventID: "event-direct-delivered",
		ProviderMessageID: "provider-direct-1",
		EventClass:        AgentEmailOutboundProviderEventDelivered, OccurredAt: eventTime,
	}
	delivered, err := st.ApplyAgentEmailOutboundProviderEvent(ctx, event)
	if err != nil || !delivered.Applied ||
		delivered.Message.State != AgentEmailOutboundDelivered ||
		delivered.Message.DeliveredAt == nil ||
		accepted.ProviderStartedAt == nil ||
		delivered.Message.DeliveredAt.Before(*accepted.ProviderStartedAt) {
		t.Fatalf("delivered event = %#v / %v", delivered, err)
	}
	var storedEventOccurredAt time.Time
	if err := st.pool.QueryRow(ctx, `
		SELECT occurred_at
		  FROM agent_email_outbound_provider_events
		 WHERE provider=$1 AND event_id_hash=$2`,
		event.Provider, agentEmailOutboundSHA256(event.EventID),
	).Scan(&storedEventOccurredAt); err != nil {
		t.Fatal(err)
	}
	if !storedEventOccurredAt.Equal(eventTime) {
		t.Fatalf("provider receipt occurred_at = %v, want raw %v",
			storedEventOccurredAt, eventTime)
	}
	duplicate, err := st.ApplyAgentEmailOutboundProviderEvent(ctx, event)
	if err != nil || duplicate.Applied || duplicate.Message.State != AgentEmailOutboundDelivered {
		t.Fatalf("duplicate event = %#v / %v", duplicate, err)
	}
	changedEvent := event
	changedEvent.EventClass = AgentEmailOutboundProviderEventDeferred
	if _, err := st.ApplyAgentEmailOutboundProviderEvent(ctx, changedEvent); !errors.Is(err, ErrAgentEmailOutboundConflict) {
		t.Fatalf("changed provider-event replay error = %v", err)
	}

	canaryClaim, err := st.ClaimAgentEmailOutbound(ctx, AgentEmailOutboundClaimInput{})
	if err != nil || canaryClaim.Message.ID != providerEventCanarySend.ID {
		t.Fatalf("provider-event canary claim = %#v / %v", canaryClaim, err)
	}
	if _, err := st.StartAgentEmailOutboundProviderCall(
		ctx, providerEventCanarySend.ID, canaryClaim.Claim,
	); err != nil {
		t.Fatal(err)
	}
	canaryAccepted, err := st.FinalizeAgentEmailOutbound(
		ctx, providerEventCanarySend.ID,
		FinalizeAgentEmailOutboundInput{
			Claim: canaryClaim.Claim, State: AgentEmailOutboundAccepted,
			Provider: "cloudflare_email_sending", ProviderMessageID: "provider-canary-1",
		},
	)
	if err != nil || canaryAccepted.AcceptedAt == nil {
		t.Fatalf("provider-event canary accepted = %#v / %v", canaryAccepted, err)
	}
	var canarySentUsage int
	if err := st.pool.QueryRow(ctx, `
		SELECT count(*) FROM usage_events
		 WHERE account_id=$1 AND dimension='email_sent' AND subject_id=$2`,
		p.AccountID, providerEventCanarySend.ID).Scan(&canarySentUsage); err != nil ||
		canarySentUsage != 1 {
		t.Fatalf("provider-event canary email_sent usage = %d / %v",
			canarySentUsage, err)
	}
	const providerEventCanaryID = "event-provider-canary-delivered"
	identityCollision := AgentEmailOutboundProviderEventInput{
		Provider: "cloudflare_email_sending", EventID: providerEventCanaryID,
		ProviderMessageID: "provider-direct-1",
		EventClass:        AgentEmailOutboundProviderEventDelivered,
		OccurredAt:        accepted.AcceptedAt.UTC(),
	}
	if collision, err := st.ApplyAgentEmailOutboundProviderEvent(
		ctx, identityCollision,
	); err != nil || !collision.Applied {
		t.Fatalf("provider-event canary identity collision fixture = %#v / %v",
			collision, err)
	}
	if _, err := st.PrepareAgentEmailProviderEventCanary(
		ctx, p.AccountID, providerEventCanarySend.ID,
		*canaryAccepted.AcceptedAt, providerEventCanaryID,
	); !errors.Is(err, ErrAgentEmailProviderEventCanaryFence) {
		t.Fatalf("cross-send provider-event canary identity error = %v", err)
	}
	if _, err := st.pool.Exec(ctx, `
		DELETE FROM agent_email_outbound_provider_events
		 WHERE provider=$1 AND event_id_hash=$2`, identityCollision.Provider,
		agentEmailOutboundSHA256(identityCollision.EventID)); err != nil {
		t.Fatal(err)
	}
	if _, err := st.PrepareAgentEmailProviderEventCanary(
		ctx, p.AccountID, providerEventCanarySend.ID,
		canaryAccepted.AcceptedAt.Add(time.Microsecond), providerEventCanaryID,
	); !errors.Is(err, ErrAgentEmailProviderEventCanaryFence) {
		t.Fatalf("stale provider-event canary fence error = %v", err)
	}
	canaryTarget, err := st.PrepareAgentEmailProviderEventCanary(
		ctx, p.AccountID, providerEventCanarySend.ID,
		*canaryAccepted.AcceptedAt, providerEventCanaryID,
	)
	if err != nil || canaryTarget.ProviderMessageID != "provider-canary-1" {
		t.Fatalf("provider-event canary target = %#v / %v", canaryTarget, err)
	}

	// The exact accepted-at event proves that the folded lifecycle and receipt
	// can be recognized safely by a later retry preflight.
	canaryEventTime := canaryAccepted.AcceptedAt.UTC()
	canaryEvent := AgentEmailOutboundProviderEventInput{
		Provider: "cloudflare_email_sending", EventID: providerEventCanaryID,
		ProviderMessageID: "provider-canary-1",
		EventClass:        AgentEmailOutboundProviderEventDelivered,
		OccurredAt:        canaryEventTime,
	}
	canaryDelivered, err := st.ApplyAgentEmailOutboundProviderEvent(ctx, canaryEvent)
	if err != nil || !canaryDelivered.Applied ||
		canaryDelivered.Message.State != AgentEmailOutboundDelivered ||
		canaryDelivered.Message.DeliveredAt == nil ||
		!canaryDelivered.Message.DeliveredAt.Equal(canaryEventTime) {
		t.Fatalf("provider-event canary delivered = %#v / %v", canaryDelivered, err)
	}
	continuedTarget, err := st.PrepareAgentEmailProviderEventCanary(
		ctx, p.AccountID, providerEventCanarySend.ID,
		*canaryAccepted.AcceptedAt, providerEventCanaryID,
	)
	if err != nil || continuedTarget != canaryTarget {
		t.Fatalf("completed provider-event canary continuation = %#v / %v",
			continuedTarget, err)
	}
	canaryDuplicate, err := st.ApplyAgentEmailOutboundProviderEvent(ctx, canaryEvent)
	if err != nil || canaryDuplicate.Applied ||
		canaryDuplicate.Message.State != AgentEmailOutboundDelivered {
		t.Fatalf("duplicate provider-event canary = %#v / %v", canaryDuplicate, err)
	}
	changedCanaryEvent := canaryEvent
	changedCanaryEvent.EventClass = AgentEmailOutboundProviderEventDeferred
	if _, err := st.ApplyAgentEmailOutboundProviderEvent(
		ctx, changedCanaryEvent,
	); !errors.Is(err, ErrAgentEmailOutboundConflict) {
		t.Fatalf("changed provider-event canary replay error = %v", err)
	}
	canaryVerification, err := st.VerifyAgentEmailProviderEventCanary(
		ctx, canaryTarget, canaryEvent.EventID,
	)
	if err != nil || canaryVerification.SendID != providerEventCanarySend.ID ||
		canaryVerification.State != AgentEmailOutboundDelivered ||
		canaryVerification.ProviderEventReceiptCount != 1 ||
		canaryVerification.EmailSentUsageEventCount != 1 {
		t.Fatalf("provider-event canary verification = %#v / %v",
			canaryVerification, err)
	}

	// A completed-send continuation is admitted only for the exact canonical
	// canary receipt. These rows are restored after each negative probe so the
	// remainder of this integration graph keeps its canonical event.
	exactEventIDHash := agentEmailOutboundSHA256(canaryEvent.EventID)
	exactRequestHash := agentEmailOutboundProviderEventRequestHash(canaryEvent)
	lookalikeTime := canaryEventTime.Add(time.Nanosecond)
	lookalikeTimestamp := canaryEvent
	lookalikeTimestamp.OccurredAt = lookalikeTime
	lookalikeClass := canaryEvent
	lookalikeClass.EventClass = AgentEmailOutboundProviderEventDeferred
	lookalikeProviderID := canaryEvent
	lookalikeProviderID.ProviderMessageID = "provider-lookalike"
	for _, candidate := range []struct {
		name, provider, eventIDHash, requestHash, eventClass string
		occurredAt                                           time.Time
	}{
		{name: "unrelated event id", provider: canaryEvent.Provider,
			eventIDHash: agentEmailOutboundSHA256("real-provider-event"),
			requestHash: exactRequestHash, eventClass: canaryEvent.EventClass,
			occurredAt: canaryEventTime},
		{name: "different provider", provider: "other_provider",
			eventIDHash: exactEventIDHash, requestHash: exactRequestHash,
			eventClass: canaryEvent.EventClass, occurredAt: canaryEventTime},
		{name: "wrong request hash", provider: canaryEvent.Provider,
			eventIDHash: exactEventIDHash, requestHash: strings.Repeat("0", 64),
			eventClass: canaryEvent.EventClass, occurredAt: canaryEventTime},
		{name: "different provider id", provider: canaryEvent.Provider,
			eventIDHash: exactEventIDHash,
			requestHash: agentEmailOutboundProviderEventRequestHash(lookalikeProviderID),
			eventClass:  canaryEvent.EventClass, occurredAt: canaryEventTime},
		{name: "different class", provider: canaryEvent.Provider,
			eventIDHash: exactEventIDHash,
			requestHash: agentEmailOutboundProviderEventRequestHash(lookalikeClass),
			eventClass:  lookalikeClass.EventClass, occurredAt: canaryEventTime},
		{name: "different timestamp", provider: canaryEvent.Provider,
			eventIDHash: exactEventIDHash,
			requestHash: agentEmailOutboundProviderEventRequestHash(lookalikeTimestamp),
			eventClass:  canaryEvent.EventClass, occurredAt: lookalikeTime},
	} {
		t.Run("provider event canary rejects "+candidate.name, func(t *testing.T) {
			if _, err := st.pool.Exec(ctx, `
				UPDATE agent_email_outbound_provider_events
				   SET provider=$2,event_id_hash=$3,event_request_hash=$4,
				       event_class=$5,occurred_at=$6
				 WHERE account_id=$1 AND outbound_id=$7`,
				p.AccountID, candidate.provider, candidate.eventIDHash,
				candidate.requestHash, candidate.eventClass, candidate.occurredAt,
				providerEventCanarySend.ID); err != nil {
				t.Fatal(err)
			}
			if _, err := st.PrepareAgentEmailProviderEventCanary(
				ctx, p.AccountID, providerEventCanarySend.ID,
				*canaryAccepted.AcceptedAt,
				providerEventCanaryID,
			); !errors.Is(err, ErrAgentEmailProviderEventCanaryFence) {
				t.Fatalf("lookalike continuation error = %v", err)
			}
			if _, err := st.pool.Exec(ctx, `
				UPDATE agent_email_outbound_provider_events
				   SET provider=$2,event_id_hash=$3,event_request_hash=$4,
				       event_class=$5,occurred_at=$6
				 WHERE account_id=$1 AND outbound_id=$7`,
				p.AccountID, canaryEvent.Provider, exactEventIDHash, exactRequestHash,
				canaryEvent.EventClass, canaryEventTime,
				providerEventCanarySend.ID); err != nil {
				t.Fatal(err)
			}
		})
	}
	extraEvent := canaryEvent
	extraEvent.EventID = "real-provider-extra-event"
	if _, err := st.pool.Exec(ctx, `
		INSERT INTO agent_email_outbound_provider_events
		  (account_id,provider,event_id_hash,event_request_hash,outbound_id,
		   event_class,occurred_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7)`,
		p.AccountID, extraEvent.Provider, agentEmailOutboundSHA256(extraEvent.EventID),
		agentEmailOutboundProviderEventRequestHash(extraEvent), providerEventCanarySend.ID,
		extraEvent.EventClass, extraEvent.OccurredAt); err != nil {
		t.Fatal(err)
	}
	if _, err := st.PrepareAgentEmailProviderEventCanary(
		ctx, p.AccountID, providerEventCanarySend.ID,
		*canaryAccepted.AcceptedAt, providerEventCanaryID,
	); !errors.Is(err, ErrAgentEmailProviderEventCanaryFence) {
		t.Fatalf("extra real receipt continuation error = %v", err)
	}
	if _, err := st.pool.Exec(ctx, `
		DELETE FROM agent_email_outbound_provider_events
		 WHERE provider=$1 AND event_id_hash=$2`, extraEvent.Provider,
		agentEmailOutboundSHA256(extraEvent.EventID)); err != nil {
		t.Fatal(err)
	}

	// An expired provider_started row is reclaimed only for an exact replay of
	// the immutable adapter receipt. It becomes ambiguous only after the
	// bounded attempt budget is exhausted.
	replyClaim, err := st.ClaimAgentEmailOutbound(ctx, AgentEmailOutboundClaimInput{})
	if err != nil || replyClaim.Message.ID != reply.ID ||
		replyClaim.InReplyTo != "<thread-1@example.com>" ||
		len(replyClaim.References) != 1 {
		t.Fatalf("reply claim = %#v / %v", replyClaim, err)
	}
	if _, err := st.StartAgentEmailOutboundProviderCall(ctx, reply.ID, replyClaim.Claim); err != nil {
		t.Fatal(err)
	}
	const blockedEvacuationID = "evac_outbound_provider_started"
	if _, err := st.BeginAccountEvacuation(
		ctx, p.AccountID, blockedEvacuationID,
		"must wait for provider receipt replay",
	); !errors.Is(err, ErrAccountEvacuationOutboundInFlight) {
		t.Fatalf("provider-started evacuation error = %v, want %v",
			err, ErrAccountEvacuationOutboundInFlight)
	}
	var accountStatus string
	var accountEvacuationID *string
	if err := st.pool.QueryRow(ctx, `
		SELECT status, evacuation_id FROM accounts WHERE id=$1`,
		p.AccountID,
	).Scan(&accountStatus, &accountEvacuationID); err != nil {
		t.Fatal(err)
	}
	if accountStatus != "active" || accountEvacuationID != nil {
		t.Fatalf("blocked evacuation mutated account: status=%q id=%v",
			accountStatus, accountEvacuationID)
	}
	if _, err := st.pool.Exec(ctx, `
		UPDATE agent_email_outbound_messages
		   SET lease_expires_at=clock_timestamp()-interval '1 second'
		 WHERE id=$1`, reply.ID); err != nil {
		t.Fatal(err)
	}
	if count, err := st.ReconcileExhaustedAgentEmailOutbound(ctx, 10); err != nil || count != 0 {
		t.Fatalf("premature ambiguity reconcile = %d / %v", count, err)
	}
	replayClaim, err := st.ClaimAgentEmailOutbound(ctx, AgentEmailOutboundClaimInput{})
	if err != nil || replayClaim.Message.ID != reply.ID ||
		replayClaim.Message.State != AgentEmailOutboundProviderStarted ||
		replayClaim.Claim.Generation != replyClaim.Claim.Generation+1 {
		t.Fatalf("provider receipt replay claim = %#v / %v", replayClaim, err)
	}
	replayDispatch, err := st.StartAgentEmailOutboundProviderCall(
		ctx, reply.ID, replayClaim.Claim,
	)
	if !errors.Is(err, ErrAgentEmailOutboundProviderAlreadyStarted) ||
		replayDispatch.Text != "reply body" {
		t.Fatalf("provider receipt replay start = %#v / %v", replayDispatch, err)
	}
	if _, err := st.pool.Exec(ctx, `
		UPDATE agent_email_outbound_messages
		   SET attempt_count=$2,
		       lease_expires_at=clock_timestamp()-interval '1 second'
		 WHERE id=$1`, reply.ID, maximumAgentEmailOutboundAttempts); err != nil {
		t.Fatal(err)
	}
	if count, err := st.ReconcileExhaustedAgentEmailOutbound(ctx, 10); err != nil || count != 1 {
		t.Fatalf("exhausted ambiguity reconcile = %d / %v", count, err)
	}
	ambiguous, err := st.GetAgentEmailOutbound(ctx, p, reply.ID)
	if err != nil || ambiguous.State != AgentEmailOutboundAmbiguous ||
		ambiguous.LastErrorCode != AgentEmailOutboundErrorWorkerLeaseExpired {
		t.Fatalf("expired provider start = %#v / %v", ambiguous, err)
	}

	bounce, err := st.QueueAgentEmail(ctx, p, SendAgentEmailInput{
		To: "hard-bounce@example.com", Subject: "bounce", Text: "body",
		IdempotencyKey: "bounce-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	queuedBeforeSuppression, err := st.QueueAgentEmail(ctx, p, SendAgentEmailInput{
		To: "hard-bounce@example.com", Subject: "already queued", Text: "body",
		IdempotencyKey: "bounce-already-queued",
	})
	if err != nil {
		t.Fatal(err)
	}
	bounceClaim, err := st.ClaimAgentEmailOutbound(ctx, AgentEmailOutboundClaimInput{})
	if err != nil || bounceClaim.Message.ID != bounce.ID {
		t.Fatalf("bounce claim = %#v / %v", bounceClaim, err)
	}
	if _, err := st.StartAgentEmailOutboundProviderCall(ctx, bounce.ID, bounceClaim.Claim); err != nil {
		t.Fatal(err)
	}
	if _, err := st.FinalizeAgentEmailOutbound(ctx, bounce.ID,
		FinalizeAgentEmailOutboundInput{
			Claim: bounceClaim.Claim, State: AgentEmailOutboundAccepted,
			Provider: "cloudflare_email_sending", ProviderMessageID: "provider-bounce-1",
		}); err != nil {
		t.Fatal(err)
	}
	bounceEvent := AgentEmailOutboundProviderEventInput{
		Provider: "cloudflare_email_sending", EventID: "event-hard-bounce",
		ProviderMessageID: "provider-bounce-1",
		EventClass:        AgentEmailOutboundProviderEventBounced,
		OccurredAt:        time.Now().UTC().Truncate(time.Microsecond).Add(time.Minute),
	}
	bounced, err := st.ApplyAgentEmailOutboundProviderEvent(ctx, bounceEvent)
	if err != nil || bounced.Message.State != AgentEmailOutboundBounced {
		t.Fatalf("hard bounce = %#v / %v", bounced, err)
	}
	var bounceOccurredAt, bounceReceivedAt time.Time
	if err := st.pool.QueryRow(ctx, `
		SELECT occurred_at,received_at
		  FROM agent_email_outbound_provider_events
		 WHERE provider=$1 AND event_id_hash=$2`,
		bounceEvent.Provider, agentEmailOutboundSHA256(bounceEvent.EventID),
	).Scan(&bounceOccurredAt, &bounceReceivedAt); err != nil {
		t.Fatal(err)
	}
	if !bounceOccurredAt.Equal(bounceEvent.OccurredAt) ||
		bounced.Message.FailedAt == nil || bounced.Message.FailedAt.After(bounceReceivedAt) {
		t.Fatalf("future-skew event receipt=%v/%v folded=%v",
			bounceOccurredAt, bounceReceivedAt, bounced.Message.FailedAt)
	}
	if _, err := st.QueueAgentEmail(ctx, p, SendAgentEmailInput{
		To: "hard-bounce@example.com", Subject: "again", Text: "again", IdempotencyKey: "bounce-2",
	}); !errors.Is(err, ErrAgentEmailRecipientSuppressed) {
		t.Fatalf("suppressed recipient error = %v", err)
	}
	queuedSuppressedClaim, err := st.ClaimAgentEmailOutbound(
		ctx, AgentEmailOutboundClaimInput{},
	)
	if err != nil || queuedSuppressedClaim.Message.ID != queuedBeforeSuppression.ID {
		t.Fatalf("queued-before-suppression claim = %#v / %v", queuedSuppressedClaim, err)
	}
	canceledSuppressed, err := st.StartAgentEmailOutboundProviderCall(
		ctx, queuedBeforeSuppression.ID, queuedSuppressedClaim.Claim,
	)
	if !errors.Is(err, ErrAgentEmailRecipientSuppressed) ||
		canceledSuppressed.Message.State != AgentEmailOutboundCanceled {
		t.Fatalf("final suppression check = %#v / %v", canceledSuppressed, err)
	}

	// SKIP LOCKED lets another worker make progress even while the oldest send
	// row is held by a long transaction.
	lockedSend, err := st.QueueAgentEmail(ctx, p, SendAgentEmailInput{
		To: "locked@example.com", Subject: "locked", Text: "locked", IdempotencyKey: "locked-send",
	})
	if err != nil {
		t.Fatal(err)
	}
	otherSend, err := st.QueueAgentEmail(ctx, p, SendAgentEmailInput{
		To: "other@example.com", Subject: "other", Text: "other", IdempotencyKey: "other-send",
	})
	if err != nil {
		t.Fatal(err)
	}
	lockTx, err := st.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lockTx.Rollback(ctx) }()
	var lockedID string
	if err := lockTx.QueryRow(ctx, `
		SELECT id FROM agent_email_outbound_messages
		 WHERE id=$1 FOR UPDATE`, lockedSend.ID).Scan(&lockedID); err != nil {
		t.Fatal(err)
	}
	progress, err := st.ClaimAgentEmailOutbound(ctx, AgentEmailOutboundClaimInput{})
	if err != nil || progress.Message.ID != otherSend.ID {
		t.Fatalf("skip-locked progress = %#v / %v", progress, err)
	}
	if err := lockTx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}

	// A claimed send is canceled if its account becomes inactive before the
	// provider boundary. Reactivating the account must not revive stale mail.
	if err := st.SuspendAccountOwner(ctx, p.AccountID, provisioned.OperatorID,
		"outbound cancellation test"); err != nil {
		t.Fatal(err)
	}
	canceled, err := st.StartAgentEmailOutboundProviderCall(
		ctx, otherSend.ID, progress.Claim,
	)
	if !errors.Is(err, ErrAccountNotActive) ||
		canceled.Message.State != AgentEmailOutboundCanceled ||
		canceled.Message.LastErrorCode != AgentEmailOutboundErrorDispatchCanceled {
		t.Fatalf("inactive-account provider boundary = %#v / %v", canceled, err)
	}
	if err := st.ResumeAccountOwner(ctx, p.AccountID, provisioned.OperatorID); err != nil {
		t.Fatal(err)
	}
	afterResume, err := st.GetAgentEmailOutbound(ctx, p, otherSend.ID)
	if err != nil || afterResume.State != AgentEmailOutboundCanceled {
		t.Fatalf("inactive-account send revived after resume = %#v / %v", afterResume, err)
	}

	agentControl, err := st.GetAgentEmailSendControl(
		ctx, p.AccountID, provisioned.OperatorID, p.ID,
	)
	if err != nil || agentControl.SendState != AgentEmailSendEnabled {
		t.Fatalf("initial agent send control = %#v / %v", agentControl, err)
	}
	agentControl, err = st.SetAgentEmailSendControl(
		ctx, p.AccountID, provisioned.OperatorID, p.ID,
		AgentEmailSendDisabled, agentControl.RowVersion,
	)
	if err != nil || agentControl.AgentSendState != AgentEmailSendDisabled ||
		agentControl.SendState != AgentEmailSendDisabled {
		t.Fatalf("disabled agent send control = %#v / %v", agentControl, err)
	}
	if _, err := st.QueueAgentEmail(ctx, p, SendAgentEmailInput{
		To: "blocked@example.com", Subject: "blocked", Text: "blocked", IdempotencyKey: "blocked-agent",
	}); !errors.Is(err, ErrAgentEmailSendDisabled) {
		t.Fatalf("agent-disabled queue error = %v", err)
	}
	agentControl, err = st.SetAgentEmailSendControl(
		ctx, p.AccountID, provisioned.OperatorID, p.ID,
		AgentEmailSendEnabled, agentControl.RowVersion,
	)
	if err != nil {
		t.Fatal(err)
	}
	realmControl, err := st.GetAgentEmailRealmSendControl(
		ctx, p.AccountID, provisioned.OperatorID, p.RealmID,
	)
	if err != nil {
		t.Fatal(err)
	}
	realmControl, err = st.SetAgentEmailRealmSendControl(
		ctx, p.AccountID, provisioned.OperatorID, p.RealmID,
		AgentEmailSendDisabled, realmControl.RowVersion,
	)
	if err != nil || realmControl.SendState != AgentEmailSendDisabled {
		t.Fatalf("disabled realm send control = %#v / %v", realmControl, err)
	}
	agentControl, err = st.GetAgentEmailSendControl(
		ctx, p.AccountID, provisioned.OperatorID, p.ID,
	)
	if err != nil || agentControl.AgentSendState != AgentEmailSendEnabled ||
		agentControl.RealmSendState != AgentEmailSendDisabled ||
		agentControl.SendState != AgentEmailSendDisabled {
		t.Fatalf("independent controls = %#v / %v", agentControl, err)
	}

	applyPlan(2, map[string]int64{}, []string{plans.AgentEmailReceiveFeature})
	if _, err := st.QueueAgentEmail(ctx, p, SendAgentEmailInput{
		To: "feature@example.com", Subject: "disabled", Text: "disabled", IdempotencyKey: "feature-off",
	}); !errors.Is(err, ErrFeatureNotEnabled) {
		t.Fatalf("feature-disabled queue error = %v", err)
	}

	// Provider-event receipts are canonical tenant data even when the event is
	// a monotonic no-op. Their insert takes the account evacuation fence before
	// the event row, so Begin cannot overtake an in-flight receipt and a receipt
	// arriving after the marker cannot commit.
	preFenceEvent := AgentEmailOutboundProviderEventInput{
		Provider: "cloudflare_email_sending", EventID: "event-before-evacuation",
		ProviderMessageID: "provider-direct-1",
		EventClass:        AgentEmailOutboundProviderEventDelivered,
		OccurredAt:        time.Now().UTC(),
	}
	eventTx, err := st.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = eventTx.Rollback(ctx) }()
	if _, err := eventTx.Exec(ctx, `
		INSERT INTO agent_email_outbound_provider_events
		  (account_id,provider,event_id_hash,event_request_hash,outbound_id,
		   event_class,occurred_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7)`,
		p.AccountID, preFenceEvent.Provider,
		agentEmailOutboundSHA256(preFenceEvent.EventID),
		agentEmailOutboundProviderEventRequestHash(preFenceEvent), direct.ID,
		preFenceEvent.EventClass, preFenceEvent.OccurredAt); err != nil {
		t.Fatal(err)
	}
	var lockedAccountID string
	err = st.pool.QueryRow(ctx, `
		SELECT id FROM accounts WHERE id=$1 FOR UPDATE NOWAIT`, p.AccountID).
		Scan(&lockedAccountID)
	if err == nil {
		t.Fatal("provider event insert did not hold the account evacuation fence")
	}
	assertPostgresCode(t, err, "55P03")

	const evacuationID = "evac_outbound_provider_event"
	evacuationDone := make(chan error, 1)
	go func() {
		_, beginErr := st.BeginAccountEvacuation(
			ctx, p.AccountID, evacuationID, "outbound provider-event fence test",
		)
		evacuationDone <- beginErr
	}()
	select {
	case beginErr := <-evacuationDone:
		_ = eventTx.Rollback(ctx)
		t.Fatalf("evacuation overtook open provider event: %v", beginErr)
	case <-time.After(100 * time.Millisecond):
	}
	if err := eventTx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case beginErr := <-evacuationDone:
		if beginErr != nil {
			t.Fatal(beginErr)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("evacuation did not continue after provider event committed")
	}

	postFenceEvent := preFenceEvent
	postFenceEvent.EventID = "event-after-evacuation"
	postFenceEvent.OccurredAt = time.Now().UTC()
	if _, err := st.ApplyAgentEmailOutboundProviderEvent(
		ctx, postFenceEvent,
	); err == nil {
		t.Fatal("provider event committed after evacuation marker")
	} else {
		assertAccountEvacuationFenceError(t, err)
	}
	var postFenceReceipts int
	if err := st.pool.QueryRow(ctx, `
		SELECT count(*) FROM agent_email_outbound_provider_events
		 WHERE provider=$1 AND event_id_hash=$2`, postFenceEvent.Provider,
		agentEmailOutboundSHA256(postFenceEvent.EventID)).Scan(&postFenceReceipts); err != nil {
		t.Fatal(err)
	}
	if postFenceReceipts != 0 {
		t.Fatalf("post-fence provider event receipts = %d, want 0", postFenceReceipts)
	}
	if _, err := st.AbortAccountEvacuation(
		ctx, p.AccountID, evacuationID,
	); err != nil {
		t.Fatal(err)
	}
}

func TestAgentEmailOutboundDailyBreakersPostgres(t *testing.T) {
	baseDSN := testenv.RequirePostgres(t)
	ctx := context.Background()
	st, _ := newMigrationTestStore(t, baseDSN)
	if err := st.Migrate(); err != nil {
		t.Fatal(err)
	}
	fixture := newAgentEmailRetentionAccountFixture(
		ctx, t, st, "outbound-daily-"+fmt.Sprintf("%d", time.Now().UnixNano()),
	)
	sendScope := fixture.scope
	sendScope.Domain = "witmail.net"
	sendScope.LegacyDomains = []string{fixture.scope.Domain}
	sendScope.Audience = "outbound-daily-test"
	if _, err := st.ReconcileAgentEmailPilot(ctx, sendScope); err != nil {
		t.Fatal(err)
	}
	policies := map[string]int64{
		plans.AgentEmailEntitlementVersionPolicy: plans.AgentEmailEntitlementVersion,
		plans.AgentEmailRetentionDaysPolicy:      30,
	}
	features := []string{plans.AgentEmailReceiveFeature, plans.AgentEmailSendFeature}
	hash, err := plans.SnapshotHash("daily-test", map[string]int64{}, policies, features)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.SetAccountPlan(
		ctx, fixture.accountID, 1, hash, "daily-test",
		map[string]int64{}, policies, features,
	); err != nil {
		t.Fatal(err)
	}

	const recipient = "daily-recipient@example.com"
	for _, tc := range []struct {
		name    string
		scope   string
		scopeID string
		limit   int64
	}{
		{
			name: "account", scope: AgentEmailOutboundRateScopeAccount,
			scopeID: fixture.accountID, limit: plans.MaxAgentEmailSentPerAccountDay,
		},
		{
			name: "recipient", scope: AgentEmailOutboundRateScopeRecipient,
			scopeID: agentEmailOutboundRecipientScopeID(fixture.accountID, recipient),
			limit:   plans.MaxAgentEmailSentPerRecipientDay,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := st.pool.Exec(ctx, `
				DELETE FROM agent_email_outbound_rate_buckets WHERE account_id=$1`,
				fixture.accountID,
			); err != nil {
				t.Fatal(err)
			}
			if _, err := st.pool.Exec(ctx, `
				INSERT INTO agent_email_outbound_rate_buckets
				  (account_id,realm_id,lane,scope,scope_id,
				   theoretical_arrival_microseconds)
				VALUES ($1,'','admission_daily',$2,$3,
				        floor(extract(epoch FROM clock_timestamp()+interval '2 days')*1000000)::bigint)`,
				fixture.accountID, tc.scope, tc.scopeID,
			); err != nil {
				t.Fatal(err)
			}
			_, err := st.QueueAgentEmail(ctx, fixture.owner, SendAgentEmailInput{
				To: recipient, Subject: "daily bound", Text: "bounded body",
				IdempotencyKey: "daily-" + tc.name,
			})
			var detail *AgentEmailOutboundRateLimitError
			if !errors.As(err, &detail) || detail == nil ||
				detail.Scope != tc.scope || detail.Limit != tc.limit ||
				detail.WindowSeconds != agentEmailOutboundRateDaySeconds ||
				detail.Source != AgentEmailOutboundRateSourcePlatform ||
				!detail.Retryable {
				t.Fatalf("daily %s refusal = %#v / %v", tc.name, detail, err)
			}
			if strings.Contains(err.Error(), recipient) {
				t.Fatalf("daily refusal leaked recipient: %v", err)
			}
			if got := countAgentEmailOutboundMessages(ctx, t, st, fixture.accountID); got != 0 {
				t.Fatalf("daily refusal stored %d outbound messages", got)
			}
		})
	}
}

func countAgentEmailOutboundMessages(
	ctx context.Context,
	t *testing.T,
	st *Store,
	accountID string,
) int64 {
	t.Helper()
	var count int64
	if err := st.pool.QueryRow(ctx, `
		SELECT count(*) FROM agent_email_outbound_messages WHERE account_id=$1`,
		accountID,
	).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}
