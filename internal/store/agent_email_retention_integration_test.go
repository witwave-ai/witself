package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/witwave-ai/witself/internal/agentemail"
	"github.com/witwave-ai/witself/internal/plans"
	"github.com/witwave-ai/witself/internal/testenv"
)

func TestAgentEmailRetentionLifecyclePostgres(t *testing.T) {
	baseDSN := testenv.RequirePostgres(t)
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	st, _ := newMigrationTestStore(t, baseDSN)
	if err := st.Migrate(); err != nil {
		t.Fatal(err)
	}

	provisioned, err := st.ProvisionAccount(
		ctx,
		"agent-email-retention@witwave.ai",
		"agent email retention",
		time.Hour,
	)
	if err != nil {
		t.Fatal(err)
	}
	if activated, err := st.ActivateAccount(
		ctx, provisioned.AccountID,
	); err != nil || !activated {
		t.Fatalf("activate = %v / %v", activated, err)
	}
	if _, err := st.SetAccountPlan(
		ctx,
		provisioned.AccountID,
		0,
		"",
		"test",
		map[string]int64{},
		map[string]int64{
			plans.AgentEmailEntitlementVersionPolicy: plans.AgentEmailEntitlementVersion,
			plans.AgentEmailRetentionDaysPolicy:      30,
		},
		[]string{plans.AgentEmailReceiveFeature},
	); err != nil {
		t.Fatal(err)
	}
	realm, err := st.CreateRealm(ctx, provisioned.AccountID, "email retention")
	if err != nil {
		t.Fatal(err)
	}
	var agent Agent
	enrolled := make(map[string]bool, 5)
	for index, name := range []string{
		"mail owner", "email worker two", "email worker three",
		"email worker four", "email worker five",
	} {
		created, err := st.CreateAgent(ctx, provisioned.AccountID, realm.ID, name)
		if err != nil {
			t.Fatal(err)
		}
		if index == 0 {
			agent = created
		}
		enrolled[created.ID] = true
	}
	owner := Principal{
		Kind: PrincipalAgent, ID: agent.ID, AccountID: provisioned.AccountID,
		RealmID: realm.ID, AgentName: agent.Name, AccountStatus: "active",
	}
	scope := AgentEmailPilotScope{
		Enabled: true, Domain: "agent-mail.witwave.ai", Audience: "retention-test",
		RealmIDs: map[string]bool{realm.ID: true},
		AgentIDs: enrolled,
	}
	address, err := st.EnsureAgentEmailMailbox(
		ctx, scope, provisioned.AccountID, realm.ID, agent.ID, "",
	)
	if err != nil {
		t.Fatal(err)
	}

	ingest := func(subject, body string) AgentEmailMessage {
		t.Helper()
		raw := []byte(strings.Join([]string{
			"From: sender@example.com",
			"To: " + address.Address,
			"Subject: " + subject,
			"",
			body,
		}, "\r\n"))
		digest := sha256.Sum256(raw)
		message, err := st.IngestAgentEmailPilot(
			ctx,
			scope,
			AgentEmailIngestInput{
				Relay: agentemail.RelayMetadata{
					Timestamp:         time.Now().Unix(),
					KeyID:             "retention-test",
					Audience:          scope.Audience,
					EnvelopeSender:    "sender@example.com",
					EnvelopeRecipient: address.Address,
					RawSize:           int64(len(raw)),
					RawSHA256:         hex.EncodeToString(digest[:]),
				},
				Raw: raw,
			},
		)
		if err != nil {
			t.Fatal(err)
		}
		return message
	}

	expiredRoot := ingest("duplicate root", "same duplicate body")
	recentDuplicate := ingest("duplicate root", "same duplicate body")
	if recentDuplicate.PossibleDuplicateOfMessage != expiredRoot.ID {
		t.Fatalf("duplicate link = %#v", recentDuplicate)
	}
	activeExpired := ingest("active expired", "claimed body")
	recent := ingest("recent", "recent body")
	claim, err := st.ClaimAgentEmail(
		ctx,
		scope,
		owner,
		activeExpired.ID,
		ClaimAgentEmailInput{
			LeaseDuration:  10 * time.Minute,
			IdempotencyKey: "agent-email-retention-live-claim",
		},
	)
	if err != nil || claim.LeaseExpiresAt == nil {
		t.Fatalf("claim = %#v / %v", claim, err)
	}

	old := time.Now().UTC().Add(-31 * 24 * time.Hour)
	if _, err := st.pool.Exec(ctx, `
		UPDATE agent_email_messages
		   SET received_at=$2
		 WHERE id=ANY($1::text[])`,
		[]string{expiredRoot.ID, activeExpired.ID}, old,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := st.pool.Exec(ctx, `
		INSERT INTO agent_email_retry_canary_arms
		  (account_id,realm_id,mailbox_id,owner_agent_id,challenge_sha256,
		   state,delivery_fingerprint_sha256,accepted_message_id,tempfail_count,
		   armed_at,expires_at,tempfailed_at,retry_expires_at,accepted_at)
		VALUES
		  ($1,$2,$3,$4,$5,'accepted',$6,$7,1,
		   clock_timestamp()-interval '4 minutes',
		   clock_timestamp()+interval '1 minute',
		   clock_timestamp()-interval '3 minutes',
		   clock_timestamp()+interval '7 minutes',
		   clock_timestamp()-interval '2 minutes')`,
		provisioned.AccountID,
		realm.ID,
		address.MailboxID,
		agent.ID,
		strings.Repeat("a", 64),
		strings.Repeat("b", 64),
		expiredRoot.ID,
	); err != nil {
		t.Fatal(err)
	}

	preview := agentEmailRetentionResultForAccountLane(
		ctx, t, st, false, 25,
	)
	if preview.Scanned != 2 ||
		preview.Eligible != 1 ||
		preview.Deleted != 0 ||
		preview.DeferredActive != 1 ||
		preview.ClearedDuplicateLinks != 0 ||
		preview.DeletedCanaryProofs != 0 {
		t.Fatalf("preview result = %+v", preview)
	}

	var eventsBefore, usageBefore int64
	if err := st.pool.QueryRow(ctx, `
		SELECT count(*) FROM account_events WHERE account_id=$1`,
		provisioned.AccountID,
	).Scan(&eventsBefore); err != nil {
		t.Fatal(err)
	}
	if err := st.pool.QueryRow(ctx, `
		SELECT count(*) FROM usage_events WHERE account_id=$1`,
		provisioned.AccountID,
	).Scan(&usageBefore); err != nil {
		t.Fatal(err)
	}

	enforced := agentEmailRetentionResultForAccountLane(
		ctx, t, st, true, 25,
	)
	if enforced.Scanned != 2 ||
		enforced.Eligible != 1 ||
		enforced.Deleted != 1 ||
		enforced.DeferredActive != 1 ||
		enforced.ClearedDuplicateLinks != 1 ||
		enforced.DeletedCanaryProofs != 1 ||
		enforced.DeletedRawBytes != expiredRoot.RawSizeBytes {
		t.Fatalf("enforce result = %+v", enforced)
	}
	assertAgentEmailRetentionMessageCount(ctx, t, st, expiredRoot.ID, 0)
	assertAgentEmailRetentionMessageCount(ctx, t, st, activeExpired.ID, 1)
	assertAgentEmailRetentionMessageCount(ctx, t, st, recentDuplicate.ID, 1)
	assertAgentEmailRetentionMessageCount(ctx, t, st, recent.ID, 1)

	var duplicateTarget *string
	if err := st.pool.QueryRow(ctx, `
		SELECT possible_duplicate_of_message_id
		  FROM agent_email_messages
		 WHERE id=$1`,
		recentDuplicate.ID,
	).Scan(&duplicateTarget); err != nil {
		t.Fatal(err)
	}
	if duplicateTarget != nil {
		t.Fatalf("surviving duplicate target = %q, want null", *duplicateTarget)
	}
	var rootDeliveries, rootCanaries, mailboxes, addresses int64
	if err := st.pool.QueryRow(ctx, `
		SELECT
		  (SELECT count(*) FROM agent_email_deliveries WHERE message_id=$1),
		  (SELECT count(*) FROM agent_email_retry_canary_arms
		    WHERE accepted_message_id=$1),
		  (SELECT count(*) FROM agent_email_mailboxes WHERE account_id=$2),
		  (SELECT count(*) FROM agent_email_addresses WHERE account_id=$2)`,
		expiredRoot.ID, provisioned.AccountID,
	).Scan(&rootDeliveries, &rootCanaries, &mailboxes, &addresses); err != nil {
		t.Fatal(err)
	}
	if rootDeliveries != 0 || rootCanaries != 0 ||
		mailboxes != 1 || addresses != 1 {
		t.Fatalf(
			"cascade/preservation = deliveries:%d canaries:%d mailboxes:%d addresses:%d",
			rootDeliveries, rootCanaries, mailboxes, addresses,
		)
	}
	var eventsAfter, usageAfter int64
	if err := st.pool.QueryRow(ctx, `
		SELECT count(*) FROM account_events WHERE account_id=$1`,
		provisioned.AccountID,
	).Scan(&eventsAfter); err != nil {
		t.Fatal(err)
	}
	if err := st.pool.QueryRow(ctx, `
		SELECT count(*) FROM usage_events WHERE account_id=$1`,
		provisioned.AccountID,
	).Scan(&usageAfter); err != nil {
		t.Fatal(err)
	}
	if eventsAfter != eventsBefore || usageAfter != usageBefore {
		t.Fatalf(
			"audit/usage counts changed = events %d->%d usage %d->%d",
			eventsBefore, eventsAfter, usageBefore, usageAfter,
		)
	}

	// An expired processing lease is no longer a hold, even though the row
	// remains in the claimed state until ordinary mailbox maintenance sees it.
	if _, err := st.pool.Exec(ctx, `
		UPDATE agent_email_deliveries
		   SET lease_expires_at=clock_timestamp()-interval '1 second'
		 WHERE message_id=$1`,
		activeExpired.ID,
	); err != nil {
		t.Fatal(err)
	}
	afterExpiry := agentEmailRetentionResultForAccountLane(
		ctx, t, st, true, 25,
	)
	if afterExpiry.Eligible != 1 ||
		afterExpiry.Deleted != 1 ||
		afterExpiry.DeferredActive != 0 {
		t.Fatalf("after claim expiry result = %+v", afterExpiry)
	}
	assertAgentEmailRetentionMessageCount(ctx, t, st, activeExpired.ID, 0)

	// Missing policy is the canonical indefinite shape. Disabling cleanup does
	// not require changing the receive feature or reinstalling a client.
	if _, err := st.SetAccountPlan(
		ctx,
		provisioned.AccountID,
		0,
		"",
		"test",
		map[string]int64{},
		map[string]int64{
			plans.AgentEmailEntitlementVersionPolicy: plans.AgentEmailEntitlementVersion,
		},
		[]string{plans.AgentEmailReceiveFeature},
	); err != nil {
		t.Fatal(err)
	}
	if _, err := st.pool.Exec(ctx, `
		UPDATE agent_email_messages SET received_at=$2 WHERE id=$1`,
		recent.ID, old,
	); err != nil {
		t.Fatal(err)
	}
	for range defaultAgentEmailRetentionWorkerLaneCount {
		result, err := st.ProcessAgentEmailRetentionBatch(ctx, 25)
		if err != nil {
			t.Fatal(err)
		}
		if result.Scanned != 0 || result.Deleted != 0 {
			t.Fatalf("indefinite retention result = %+v", result)
		}
	}
	assertAgentEmailRetentionMessageCount(ctx, t, st, recent.ID, 1)
}

func TestMigration69AgentEmailRetentionPostgres(t *testing.T) {
	baseDSN := testenv.RequirePostgres(t)
	st, dsn := newMigrationTestStore(t, baseDSN)
	migrationTestUpTo(t, dsn, 68)
	assertMigrationTestVersion(t, dsn, 68)
	assertMigrationTestTable(
		t, st, "agent_email_retention_account_scan_state", false,
	)
	assertMigrationTestTable(
		t, st, "agent_email_retention_worker_lanes", false,
	)

	migrationTestUpTo(t, dsn, 69)
	assertMigrationTestVersion(t, dsn, 69)
	assertMigrationTestTable(
		t, st, "agent_email_retention_account_scan_state", true,
	)
	assertMigrationTestTable(
		t, st, "agent_email_retention_worker_lanes", true,
	)
	assertMigrationTestIndexShape(
		t,
		st,
		"agent_email_messages",
		"agent_email_messages_account_received_idx",
		[]string{"account_id", "received_at", "id"},
		nil,
	)
	assertMigrationTestIndexShape(
		t,
		st,
		"agent_email_messages",
		"agent_email_messages_possible_duplicate_idx",
		[]string{"account_id", "possible_duplicate_of_message_id", "id"},
		[]string{"possible_duplicate_of_message_id IS NOT NULL"},
	)
	var previewLanes, enforceLanes int64
	if err := st.pool.QueryRow(context.Background(), `
		SELECT
		  count(*) FILTER (WHERE mode='preview'),
		  count(*) FILTER (WHERE mode='enforce')
		FROM agent_email_retention_worker_lanes`,
	).Scan(&previewLanes, &enforceLanes); err != nil {
		t.Fatal(err)
	}
	if previewLanes != 16 || enforceLanes != 16 {
		t.Fatalf("agent-email retention lanes = %d/%d", previewLanes, enforceLanes)
	}

	if err := migrationTestDown(t, dsn, false); err != nil {
		t.Fatal(err)
	}
	assertMigrationTestVersion(t, dsn, 68)
	assertMigrationTestTable(
		t, st, "agent_email_retention_account_scan_state", false,
	)
	assertMigrationTestTable(
		t, st, "agent_email_retention_worker_lanes", false,
	)
	migrationTestUpTo(t, dsn, 69)
	assertMigrationTestVersion(t, dsn, 69)
}

func TestAgentEmailRetentionReplicasUseDifferentLanesPostgres(t *testing.T) {
	baseDSN := testenv.RequirePostgres(t)
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	st, _ := newMigrationTestStore(t, baseDSN)
	if err := st.Migrate(); err != nil {
		t.Fatal(err)
	}

	first := newAgentEmailRetentionAccountFixture(ctx, t, st, "replica-a")
	fixtures := []agentEmailRetentionAccountFixture{first}
	var second agentEmailRetentionAccountFixture
	for attempt := 0; attempt < defaultAgentEmailRetentionWorkerLaneCount; attempt++ {
		candidate := newAgentEmailRetentionAccountFixture(
			ctx, t, st, fmt.Sprintf("replica-b-%d", attempt),
		)
		fixtures = append(fixtures, candidate)
		if candidate.laneID != first.laneID {
			second = candidate
			break
		}
	}
	if second.accountID == "" {
		t.Fatal("could not provision fixtures in two distinct retention lanes")
	}
	firstMessage := ingestAgentEmailRetentionFixture(
		ctx, t, st, first, "replica first",
	)
	secondMessage := ingestAgentEmailRetentionFixture(
		ctx, t, st, second, "replica second",
	)
	old := time.Now().UTC().Add(-31 * 24 * time.Hour)
	if _, err := st.pool.Exec(ctx, `
		UPDATE agent_email_messages
		   SET received_at=$2
		 WHERE id=ANY($1::text[])`,
		[]string{firstMessage.ID, secondMessage.ID}, old,
	); err != nil {
		t.Fatal(err)
	}
	configureAgentEmailRetentionTestLanes(
		ctx, t, st, AgentEmailRetentionModeEnforce,
		first.laneID, second.laneID,
	)

	firstClaim, err := st.claimAgentEmailRetentionLane(
		ctx,
		AgentEmailRetentionModeEnforce,
		0,
		minAgentEmailRetentionBatchTimeout,
		defaultAgentEmailRetentionWorkerLaneCount,
	)
	if err != nil {
		t.Fatal(err)
	}
	secondClaim, err := st.claimAgentEmailRetentionLane(
		ctx,
		AgentEmailRetentionModeEnforce,
		0,
		minAgentEmailRetentionBatchTimeout,
		defaultAgentEmailRetentionWorkerLaneCount,
	)
	if err != nil {
		t.Fatal(err)
	}
	if firstClaim == nil || secondClaim == nil ||
		firstClaim.LaneID == secondClaim.LaneID {
		t.Fatalf("replica lane claims = %#v / %#v", firstClaim, secondClaim)
	}

	results := make(chan AgentEmailRetentionBatchResult, 2)
	errs := make(chan error, 2)
	start := make(chan struct{})
	var workers sync.WaitGroup
	for _, claim := range []agentEmailRetentionLaneClaim{*firstClaim, *secondClaim} {
		claim := claim
		workers.Add(1)
		go func() {
			defer workers.Done()
			<-start
			result, err := st.processClaimedAgentEmailRetentionBatch(
				ctx,
				25,
				true,
				0,
				defaultAgentEmailRetentionWorkerLaneCount,
				AgentEmailRetentionModeEnforce,
				claim,
			)
			if err != nil {
				errs <- err
				return
			}
			results <- result
		}()
	}
	close(start)
	workers.Wait()
	close(results)
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	var deleted, eligible int64
	for result := range results {
		if !result.LaneAdvanced {
			t.Fatalf("replica did not advance its lane: %+v", result)
		}
		deleted += result.Deleted
		eligible += result.Eligible
	}
	if deleted != 2 || eligible != 2 {
		t.Fatalf("replica aggregate = eligible:%d deleted:%d, want 2/2", eligible, deleted)
	}
	assertAgentEmailRetentionMessageCount(ctx, t, st, firstMessage.ID, 0)
	assertAgentEmailRetentionMessageCount(ctx, t, st, secondMessage.ID, 0)

	// Every extra fixture exists only to find a distinct lane and carries no
	// messages. Keep the variable live so future fixture changes cannot
	// silently turn those accounts into unasserted cleanup work.
	for _, fixture := range fixtures {
		var messages int64
		if err := st.pool.QueryRow(ctx, `
			SELECT count(*) FROM agent_email_messages WHERE account_id=$1`,
			fixture.accountID,
		).Scan(&messages); err != nil {
			t.Fatal(err)
		}
		if fixture.accountID != first.accountID &&
			fixture.accountID != second.accountID &&
			messages != 0 {
			t.Fatalf("selection-only fixture has %d messages", messages)
		}
	}
}

func TestAgentEmailRetentionIndefinitePolicyWinsRacePostgres(t *testing.T) {
	baseDSN := testenv.RequirePostgres(t)
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	st, _ := newMigrationTestStore(t, baseDSN)
	if err := st.Migrate(); err != nil {
		t.Fatal(err)
	}
	fixture := newAgentEmailRetentionAccountFixture(ctx, t, st, "policy-race")
	message := ingestAgentEmailRetentionFixture(
		ctx, t, st, fixture, "policy race",
	)
	if _, err := st.pool.Exec(ctx, `
		UPDATE agent_email_messages
		   SET received_at=clock_timestamp()-interval '31 days'
		 WHERE id=$1`,
		message.ID,
	); err != nil {
		t.Fatal(err)
	}
	configureAgentEmailRetentionTestLanes(
		ctx, t, st, AgentEmailRetentionModeEnforce, fixture.laneID,
	)
	claim, err := st.claimAgentEmailRetentionLane(
		ctx,
		AgentEmailRetentionModeEnforce,
		0,
		minAgentEmailRetentionBatchTimeout,
		defaultAgentEmailRetentionWorkerLaneCount,
	)
	if err != nil || claim == nil || claim.LaneID != fixture.laneID {
		t.Fatalf("policy-race lane claim = %#v / %v", claim, err)
	}

	// The control-plane apply wins the account row first but remains
	// uncommitted. Retention must skip rather than wait behind it.
	policyTx, err := st.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := policyTx.Exec(ctx, `
		UPDATE accounts
		   SET plan_policies=plan_policies-$2
		 WHERE id=$1`,
		fixture.accountID,
		plans.AgentEmailRetentionDaysPolicy,
	); err != nil {
		_ = policyTx.Rollback(ctx)
		t.Fatal(err)
	}
	attemptCtx, cancelAttempt := context.WithTimeout(ctx, 2*time.Second)
	result, err := st.processClaimedAgentEmailRetentionBatch(
		attemptCtx,
		25,
		true,
		0,
		defaultAgentEmailRetentionWorkerLaneCount,
		AgentEmailRetentionModeEnforce,
		*claim,
	)
	cancelAttempt()
	if err != nil {
		_ = policyTx.Rollback(ctx)
		t.Fatal(err)
	}
	if result.Deleted != 0 ||
		result.DeferredLocked != 1 ||
		result.SkippedLocked != 1 ||
		!result.LaneAdvanced {
		_ = policyTx.Rollback(ctx)
		t.Fatalf("policy-race locked result = %+v", result)
	}
	if err := policyTx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	assertAgentEmailRetentionMessageCount(ctx, t, st, message.ID, 1)

	// Once the missing-policy snapshot commits, later attempts see no finite
	// account and the old message remains indefinitely.
	if _, err := st.pool.Exec(ctx, `
		UPDATE agent_email_retention_worker_lanes
		   SET next_run_at='-infinity'::timestamptz
		 WHERE mode='enforce' AND lane_id=$1`,
		fixture.laneID,
	); err != nil {
		t.Fatal(err)
	}
	later, err := st.ProcessAgentEmailRetentionBatch(ctx, 25)
	if err != nil {
		t.Fatal(err)
	}
	if later.Deleted != 0 || later.Scanned != 0 {
		t.Fatalf("post-policy indefinite result = %+v", later)
	}
	assertAgentEmailRetentionMessageCount(ctx, t, st, message.ID, 1)
}

func TestAgentEmailRetentionWorkerPreservesLockOnlyLaneMetricsPostgres(t *testing.T) {
	baseDSN := testenv.RequirePostgres(t)
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	st, _ := newMigrationTestStore(t, baseDSN)
	if err := st.Migrate(); err != nil {
		t.Fatal(err)
	}
	fixture := newAgentEmailRetentionAccountFixture(
		ctx, t, st, "worker-lock-metrics",
	)
	configureAgentEmailRetentionTestLanes(
		ctx, t, st, AgentEmailRetentionModeEnforce, fixture.laneID,
	)

	policyTx, err := st.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = policyTx.Rollback(ctx) }()
	if _, err := policyTx.Exec(ctx, `
		UPDATE accounts
		   SET plan_policies=plan_policies-$2
		 WHERE id=$1`,
		fixture.accountID,
		plans.AgentEmailRetentionDaysPolicy,
	); err != nil {
		t.Fatal(err)
	}

	cfg := DefaultAgentEmailRetentionWorkerConfig()
	cfg.Mode = AgentEmailRetentionModeEnforce
	cfg.Interval = time.Minute
	cfg.BatchTimeout = minAgentEmailRetentionBatchTimeout
	result, err := st.processAgentEmailRetentionWorkerBatch(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if result.Scanned != 0 ||
		result.SkippedLocked != 1 ||
		result.DeferredLocked != 1 ||
		!result.LaneAdvanced {
		t.Fatalf("lock-only worker result = %+v", result)
	}
}

func TestAgentEmailRetentionWorkerDrainsConsecutiveCappedBatchesPostgres(t *testing.T) {
	baseDSN := testenv.RequirePostgres(t)
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	st, _ := newMigrationTestStore(t, baseDSN)
	if err := st.Migrate(); err != nil {
		t.Fatal(err)
	}
	fixture := newAgentEmailRetentionAccountFixture(
		ctx, t, st, "worker-consecutive-drain",
	)
	messages := []AgentEmailMessage{
		ingestAgentEmailRetentionFixture(ctx, t, st, fixture, "drain one"),
		ingestAgentEmailRetentionFixture(ctx, t, st, fixture, "drain two"),
		ingestAgentEmailRetentionFixture(ctx, t, st, fixture, "drain three"),
	}
	messageIDs := make([]string, 0, len(messages))
	for _, message := range messages {
		messageIDs = append(messageIDs, message.ID)
	}
	if _, err := st.pool.Exec(ctx, `
		UPDATE agent_email_messages
		   SET received_at=clock_timestamp()-interval '31 days'
		 WHERE id=ANY($1::text[])`, messageIDs); err != nil {
		t.Fatal(err)
	}
	configureAgentEmailRetentionTestLanes(
		ctx, t, st, AgentEmailRetentionModeEnforce, fixture.laneID,
	)

	cfg := DefaultAgentEmailRetentionWorkerConfig()
	cfg.Mode = AgentEmailRetentionModeEnforce
	cfg.BatchSize = 1
	cfg.Interval = time.Minute
	cfg.BatchTimeout = minAgentEmailRetentionBatchTimeout
	result, err := st.processAgentEmailRetentionWorkerBatch(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if result.Scanned != 3 || result.Eligible != 3 || result.Deleted != 3 ||
		!result.ScanCapped || !result.LaneAdvanced {
		t.Fatalf("consecutive drain result = %+v", result)
	}
	for _, message := range messages {
		assertAgentEmailRetentionMessageCount(ctx, t, st, message.ID, 0)
	}
	var delayed bool
	if err := st.pool.QueryRow(ctx, `
		SELECT next_run_at > clock_timestamp()
		  FROM agent_email_retention_worker_lanes
		 WHERE mode='enforce' AND lane_id=$1`, fixture.laneID).Scan(&delayed); err != nil {
		t.Fatal(err)
	}
	if !delayed {
		t.Fatal("drained lane remained immediately due after its empty pass")
	}
}

func TestAgentEmailRetentionStaleLaneGenerationCannotDeletePostgres(t *testing.T) {
	baseDSN := testenv.RequirePostgres(t)
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	st, _ := newMigrationTestStore(t, baseDSN)
	if err := st.Migrate(); err != nil {
		t.Fatal(err)
	}
	fixture := newAgentEmailRetentionAccountFixture(ctx, t, st, "stale-lane")
	message := ingestAgentEmailRetentionFixture(
		ctx, t, st, fixture, "stale generation",
	)
	if _, err := st.pool.Exec(ctx, `
		UPDATE agent_email_messages
		   SET received_at=clock_timestamp()-interval '31 days'
		 WHERE id=$1`,
		message.ID,
	); err != nil {
		t.Fatal(err)
	}
	configureAgentEmailRetentionTestLanes(
		ctx, t, st, AgentEmailRetentionModeEnforce, fixture.laneID,
	)
	claim, err := st.claimAgentEmailRetentionLane(
		ctx,
		AgentEmailRetentionModeEnforce,
		0,
		minAgentEmailRetentionBatchTimeout,
		defaultAgentEmailRetentionWorkerLaneCount,
	)
	if err != nil || claim == nil {
		t.Fatalf("stale lane claim = %#v / %v", claim, err)
	}
	if _, err := st.pool.Exec(ctx, `
		UPDATE agent_email_retention_worker_lanes
		   SET generation=generation+1
		 WHERE mode='enforce' AND lane_id=$1`,
		claim.LaneID,
	); err != nil {
		t.Fatal(err)
	}
	result, err := st.processClaimedAgentEmailRetentionBatch(
		ctx,
		25,
		true,
		0,
		defaultAgentEmailRetentionWorkerLaneCount,
		AgentEmailRetentionModeEnforce,
		*claim,
	)
	if err != nil {
		t.Fatal(err)
	}
	if result != (AgentEmailRetentionBatchResult{}) {
		t.Fatalf("stale generation result = %+v", result)
	}
	assertAgentEmailRetentionMessageCount(ctx, t, st, message.ID, 1)
}

type agentEmailRetentionAccountFixture struct {
	accountID string
	realmID   string
	owner     Principal
	scope     AgentEmailPilotScope
	address   AgentEmailAddress
	laneID    int
}

func newAgentEmailRetentionAccountFixture(
	ctx context.Context,
	t *testing.T,
	st *Store,
	suffix string,
) agentEmailRetentionAccountFixture {
	t.Helper()
	provisioned, err := st.ProvisionAccount(
		ctx,
		fmt.Sprintf("agent-email-retention-%s@witwave.ai", suffix),
		"agent email retention "+suffix,
		time.Hour,
	)
	if err != nil {
		t.Fatal(err)
	}
	if activated, err := st.ActivateAccount(
		ctx, provisioned.AccountID,
	); err != nil || !activated {
		t.Fatalf("activate = %v / %v", activated, err)
	}
	if _, err := st.SetAccountPlan(
		ctx,
		provisioned.AccountID,
		0,
		"",
		"test",
		map[string]int64{},
		map[string]int64{
			plans.AgentEmailEntitlementVersionPolicy: plans.AgentEmailEntitlementVersion,
			plans.AgentEmailRetentionDaysPolicy:      30,
		},
		[]string{plans.AgentEmailReceiveFeature},
	); err != nil {
		t.Fatal(err)
	}
	realm, err := st.CreateRealm(ctx, provisioned.AccountID, "email "+suffix)
	if err != nil {
		t.Fatal(err)
	}
	var ownerAgent Agent
	enrolled := make(map[string]bool, 5)
	for index := 0; index < 5; index++ {
		agent, err := st.CreateAgent(
			ctx,
			provisioned.AccountID,
			realm.ID,
			fmt.Sprintf("%s agent %d", suffix, index+1),
		)
		if err != nil {
			t.Fatal(err)
		}
		if index == 0 {
			ownerAgent = agent
		}
		enrolled[agent.ID] = true
	}
	scope := AgentEmailPilotScope{
		Enabled: true, Domain: "agent-mail.witwave.ai", Audience: "retention-" + suffix,
		RealmIDs: map[string]bool{realm.ID: true}, AgentIDs: enrolled,
	}
	address, err := st.EnsureAgentEmailMailbox(
		ctx,
		scope,
		provisioned.AccountID,
		realm.ID,
		ownerAgent.ID,
		"",
	)
	if err != nil {
		t.Fatal(err)
	}
	var laneID int
	if err := st.pool.QueryRow(ctx, `
		SELECT get_byte(sha256($1::bytea), 0) % 16`,
		provisioned.AccountID,
	).Scan(&laneID); err != nil {
		t.Fatal(err)
	}
	return agentEmailRetentionAccountFixture{
		accountID: provisioned.AccountID,
		realmID:   realm.ID,
		owner: Principal{
			Kind: PrincipalAgent, ID: ownerAgent.ID, AccountID: provisioned.AccountID,
			RealmID: realm.ID, AgentName: ownerAgent.Name, AccountStatus: "active",
		},
		scope: scope, address: address, laneID: laneID,
	}
}

func ingestAgentEmailRetentionFixture(
	ctx context.Context,
	t *testing.T,
	st *Store,
	fixture agentEmailRetentionAccountFixture,
	subject string,
) AgentEmailMessage {
	t.Helper()
	raw := []byte(strings.Join([]string{
		"From: sender@example.com",
		"To: " + fixture.address.Address,
		"Subject: " + subject,
		"",
		"retention fixture " + subject,
	}, "\r\n"))
	digest := sha256.Sum256(raw)
	message, err := st.IngestAgentEmailPilot(
		ctx,
		fixture.scope,
		AgentEmailIngestInput{
			Relay: agentemail.RelayMetadata{
				Timestamp:         time.Now().Unix(),
				KeyID:             "retention-test",
				Audience:          fixture.scope.Audience,
				EnvelopeSender:    "sender@example.com",
				EnvelopeRecipient: fixture.address.Address,
				RawSize:           int64(len(raw)),
				RawSHA256:         hex.EncodeToString(digest[:]),
			},
			Raw: raw,
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	return message
}

func configureAgentEmailRetentionTestLanes(
	ctx context.Context,
	t *testing.T,
	st *Store,
	mode AgentEmailRetentionMode,
	dueLaneIDs ...int,
) {
	t.Helper()
	if _, err := st.pool.Exec(ctx, `
		UPDATE agent_email_retention_worker_lanes
		   SET next_run_at='infinity'::timestamptz,account_cursor='',
		       generation=0
		 WHERE mode=$1`,
		string(mode),
	); err != nil {
		t.Fatal(err)
	}
	if _, err := st.pool.Exec(ctx, `
		UPDATE agent_email_retention_worker_lanes
		   SET next_run_at='-infinity'::timestamptz
		 WHERE mode=$1 AND lane_id=ANY($2::integer[])`,
		string(mode), dueLaneIDs,
	); err != nil {
		t.Fatal(err)
	}
}

func agentEmailRetentionResultForAccountLane(
	ctx context.Context,
	t *testing.T,
	st *Store,
	enforce bool,
	batchSize int,
) AgentEmailRetentionBatchResult {
	t.Helper()
	for range defaultAgentEmailRetentionWorkerLaneCount {
		var (
			result AgentEmailRetentionBatchResult
			err    error
		)
		if enforce {
			result, err = st.ProcessAgentEmailRetentionBatch(ctx, batchSize)
		} else {
			result, err = st.PreviewAgentEmailRetentionBatch(ctx, batchSize)
		}
		if err != nil {
			t.Fatal(err)
		}
		if result.Scanned > 0 {
			return result
		}
	}
	t.Fatal("agent-email retention did not reach the account's worker lane")
	return AgentEmailRetentionBatchResult{}
}

func assertAgentEmailRetentionMessageCount(
	ctx context.Context,
	t *testing.T,
	st *Store,
	messageID string,
	want int64,
) {
	t.Helper()
	var got int64
	if err := st.pool.QueryRow(ctx, `
		SELECT count(*) FROM agent_email_messages WHERE id=$1`,
		messageID,
	).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("agent-email message %s count = %d, want %d", messageID, got, want)
	}
}
