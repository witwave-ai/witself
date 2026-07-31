package store

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/witwave-ai/witself/internal/plans"
)

func TestMessageRetentionDeletesWholeInactiveThreadsPostgres(t *testing.T) {
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

	provisioned, err := st.ProvisionAccount(
		ctx,
		"message-retention@witwave.ai",
		"message retention",
		time.Hour,
	)
	if err != nil {
		t.Fatal(err)
	}
	if activated, err := st.ActivateAccount(ctx, provisioned.AccountID); err != nil || !activated {
		t.Fatalf("activate = %v / %v", activated, err)
	}
	realm, err := st.CreateRealm(ctx, provisioned.AccountID, "default")
	if err != nil {
		t.Fatal(err)
	}
	sender, err := st.CreateAgent(ctx, provisioned.AccountID, realm.ID, "sender")
	if err != nil {
		t.Fatal(err)
	}
	recipient, err := st.CreateAgent(ctx, provisioned.AccountID, realm.ID, "recipient")
	if err != nil {
		t.Fatal(err)
	}
	senderPrincipal := Principal{
		Kind: PrincipalAgent, ID: sender.ID, AccountID: provisioned.AccountID,
		RealmID: realm.ID, AgentName: sender.Name, AccountStatus: "active",
	}
	recipientPrincipal := Principal{
		Kind: PrincipalAgent, ID: recipient.ID, AccountID: provisioned.AccountID,
		RealmID: realm.ID, AgentName: recipient.Name, AccountStatus: "active",
	}

	setPlan := func(enabled bool, finite bool) {
		t.Helper()
		policies := map[string]int64{
			plans.MessagingEntitlementVersionPolicy: plans.MessagingEntitlementVersion,
		}
		if finite {
			policies[MessageRetentionDaysPolicy] = 30
		}
		var features []string
		if enabled {
			features = []string{plans.MessagingFeature}
		}
		if _, err := st.SetAccountPlan(
			ctx,
			provisioned.AccountID,
			0,
			"",
			"test",
			map[string]int64{},
			policies,
			features,
		); err != nil {
			t.Fatal(err)
		}
	}
	setPlan(true, true)

	expiredRoot, err := st.SendMessage(ctx, senderPrincipal, SendMessageInput{
		ToAgent: recipient.ID, Body: "expired root",
		IdempotencyKey: "message-retention-expired-root",
	})
	if err != nil {
		t.Fatal(err)
	}
	expiredReply, err := st.ReplyMessage(
		ctx,
		recipientPrincipal,
		expiredRoot.ID,
		ReplyMessageInput{
			Body: "expired reply", IdempotencyKey: "message-retention-expired-reply",
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	evidenceMessage, err := st.SendMessage(ctx, senderPrincipal, SendMessageInput{
		ToAgent: recipient.ID, Body: "held by memory evidence",
		IdempotencyKey: "message-retention-evidence",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CaptureMemory(ctx, senderPrincipal, CaptureMemoryInput{
		Content: "keep the source message as provenance",
		Kind:    "decision",
		Evidence: []MemoryEvidenceInput{{
			ResolutionState: MemoryEvidenceResolved,
			ResolvedKind:    "message",
			SourceMessageID: evidenceMessage.ID,
		}},
		IdempotencyKey: "message-retention-evidence-memory",
	}); err != nil {
		t.Fatal(err)
	}
	activeMessage, err := st.SendMessage(ctx, senderPrincipal, SendMessageInput{
		ToAgent: recipient.ID, Body: "active claim",
		IdempotencyKey: "message-retention-active",
	})
	if err != nil {
		t.Fatal(err)
	}
	activeClaim, err := st.ClaimMessage(ctx, recipientPrincipal, activeMessage.ID, ClaimMessageInput{
		LeaseDuration:  10 * time.Minute,
		IdempotencyKey: "message-retention-active-claim",
	})
	if err != nil {
		t.Fatal(err)
	}
	recentMessage, err := st.SendMessage(ctx, senderPrincipal, SendMessageInput{
		ToAgent: recipient.ID, Body: "recent",
		IdempotencyKey: "message-retention-recent",
	})
	if err != nil {
		t.Fatal(err)
	}

	var projectedActivity time.Time
	if err := st.pool.QueryRow(ctx, `
		SELECT last_message_at
		  FROM message_retention_thread_activity
		 WHERE account_id=$1 AND realm_id=$2 AND thread_id=$3`,
		provisioned.AccountID, realm.ID, expiredRoot.ThreadID,
	).Scan(&projectedActivity); err != nil {
		t.Fatal(err)
	}
	if !projectedActivity.Equal(expiredReply.CreatedAt) {
		t.Fatalf(
			"thread activity = %s, want reply creation %s",
			projectedActivity,
			expiredReply.CreatedAt,
		)
	}

	old := time.Now().UTC().Add(-31 * 24 * time.Hour)
	for _, threadID := range []string{
		expiredRoot.ThreadID,
		evidenceMessage.ThreadID,
		activeMessage.ThreadID,
	} {
		if _, err := st.pool.Exec(ctx, `
			WITH aged_messages AS (
			  UPDATE agent_messages
			     SET created_at=$4
			   WHERE account_id=$1 AND realm_id=$2 AND thread_id=$3
			  RETURNING id
			)
			UPDATE message_retention_thread_activity
			   SET last_message_at=$4, updated_at=statement_timestamp()
			 WHERE account_id=$1 AND realm_id=$2 AND thread_id=$3`,
			provisioned.AccountID, realm.ID, threadID, old,
		); err != nil {
			t.Fatal(err)
		}
	}

	// Disabling messaging makes the mailbox inaccessible immediately, but its
	// finite cleanup policy remains active without any client reinstall.
	setPlan(false, true)

	preview := messageRetentionResultForAccountLane(ctx, t, st, false, 25)
	if preview.Scanned != 3 ||
		preview.EligibleThreads != 1 ||
		preview.DeletedThreads != 0 ||
		preview.DeletedMessages != 0 ||
		preview.DeferredEvidence != 1 ||
		preview.DeferredActive != 1 ||
		preview.ScanCapped {
		t.Fatalf("preview result = %+v", preview)
	}

	enforced := messageRetentionResultForAccountLane(ctx, t, st, true, 25)
	if enforced.Scanned != 3 ||
		enforced.EligibleThreads != 1 ||
		enforced.DeletedThreads != 1 ||
		enforced.DeletedMessages != 2 ||
		enforced.DeferredEvidence != 1 ||
		enforced.DeferredActive != 1 ||
		enforced.ScanCapped {
		t.Fatalf("enforce result = %+v", enforced)
	}
	assertMessageThreadCount(
		ctx, t, st, provisioned.AccountID, realm.ID, expiredRoot.ThreadID, 0,
	)
	assertMessageThreadCount(
		ctx, t, st, provisioned.AccountID, realm.ID, evidenceMessage.ThreadID, 1,
	)
	assertMessageThreadCount(
		ctx, t, st, provisioned.AccountID, realm.ID, activeMessage.ThreadID, 1,
	)
	assertMessageThreadCount(
		ctx, t, st, provisioned.AccountID, realm.ID, recentMessage.ThreadID, 1,
	)

	// A live claim is a temporary hold. Once the account is enabled long
	// enough to release it, the next disabled-account cleanup may delete it.
	setPlan(true, true)
	if _, err := st.ReleaseMessageClaim(
		ctx,
		recipientPrincipal,
		activeMessage.ID,
		ReleaseMessageClaimInput{
			ClaimID:              activeClaim.Processing.ClaimID,
			ProcessingGeneration: activeClaim.Processing.Generation,
		},
	); err != nil {
		t.Fatal(err)
	}
	setPlan(false, true)
	afterRelease := messageRetentionResultForAccountLane(ctx, t, st, true, 25)
	if afterRelease.EligibleThreads != 1 ||
		afterRelease.DeletedThreads != 1 ||
		afterRelease.DeletedMessages != 1 ||
		afterRelease.DeferredEvidence != 1 ||
		afterRelease.DeferredActive != 0 {
		t.Fatalf("after release result = %+v", afterRelease)
	}

	// Missing policy is the explicit indefinite shape. The feature can be
	// enabled while cleanup remains a clean no-op.
	setPlan(true, false)
	if _, err := st.pool.Exec(ctx, `
		WITH aged_messages AS (
		  UPDATE agent_messages
		     SET created_at=$4
		   WHERE account_id=$1 AND realm_id=$2 AND thread_id=$3
		  RETURNING id
		)
		UPDATE message_retention_thread_activity
		   SET last_message_at=$4, updated_at=statement_timestamp()
		 WHERE account_id=$1 AND realm_id=$2 AND thread_id=$3`,
		provisioned.AccountID, realm.ID, recentMessage.ThreadID, old,
	); err != nil {
		t.Fatal(err)
	}
	var scanned int64
	for range defaultMessageRetentionWorkerLaneCount {
		result, err := st.ProcessMessageRetentionBatch(ctx, 25)
		if err != nil {
			t.Fatal(err)
		}
		scanned += result.Scanned
	}
	if scanned != 0 {
		t.Fatalf("indefinite account scanned %d candidate threads", scanned)
	}
	assertMessageThreadCount(
		ctx, t, st, provisioned.AccountID, realm.ID, recentMessage.ThreadID, 1,
	)

	// A partial lane set is a schema/configuration fault. Refuse before any
	// deletion or durable cursor movement rather than deleting and reporting
	// the broken invariant afterward.
	setPlan(true, true)
	if _, err := st.pool.Exec(ctx, `
		DELETE FROM message_retention_worker_lanes
		 WHERE mode='enforce' AND lane_id=15`); err != nil {
		t.Fatal(err)
	}
	if _, err := st.ProcessMessageRetentionBatch(ctx, 25); err == nil ||
		!strings.Contains(err.Error(), "worker lane set has 15 rows, want 16") {
		t.Fatalf("incomplete lane-set error = %v", err)
	}
	assertMessageThreadCount(
		ctx, t, st, provisioned.AccountID, realm.ID, recentMessage.ThreadID, 1,
	)
}

func TestMessageRetentionHeldThreadDoesNotStarveLaterThreadsPostgres(t *testing.T) {
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

	provisioned, err := st.ProvisionAccount(
		ctx,
		"message-retention-starvation@witwave.ai",
		"message retention starvation",
		time.Hour,
	)
	if err != nil {
		t.Fatal(err)
	}
	if activated, err := st.ActivateAccount(ctx, provisioned.AccountID); err != nil || !activated {
		t.Fatalf("activate = %v / %v", activated, err)
	}
	realm, err := st.CreateRealm(ctx, provisioned.AccountID, "default")
	if err != nil {
		t.Fatal(err)
	}
	sender, err := st.CreateAgent(ctx, provisioned.AccountID, realm.ID, "sender")
	if err != nil {
		t.Fatal(err)
	}
	recipient, err := st.CreateAgent(ctx, provisioned.AccountID, realm.ID, "recipient")
	if err != nil {
		t.Fatal(err)
	}
	principal := Principal{
		Kind: PrincipalAgent, ID: sender.ID, AccountID: provisioned.AccountID,
		RealmID: realm.ID, AgentName: sender.Name, AccountStatus: "active",
	}
	if _, err := st.SetAccountPlan(
		ctx,
		provisioned.AccountID,
		0,
		"",
		"test",
		map[string]int64{},
		map[string]int64{
			MessageRetentionDaysPolicy:              30,
			plans.MessagingEntitlementVersionPolicy: plans.MessagingEntitlementVersion,
		},
		[]string{plans.MessagingFeature},
	); err != nil {
		t.Fatal(err)
	}

	send := func(body, key string) Message {
		t.Helper()
		message, err := st.SendMessage(ctx, principal, SendMessageInput{
			ToAgent: recipient.ID, Body: body, IdempotencyKey: key,
		})
		if err != nil {
			t.Fatal(err)
		}
		return message
	}
	held := send("oldest evidence-held thread", "message-retention-starvation-held")
	firstEligible := send("first eligible thread", "message-retention-starvation-first")
	secondEligible := send("second eligible thread", "message-retention-starvation-second")
	if _, err := st.CaptureMemory(ctx, principal, CaptureMemoryInput{
		Content: "retain the oldest source as provenance",
		Kind:    "decision",
		Evidence: []MemoryEvidenceInput{{
			ResolutionState: MemoryEvidenceResolved,
			ResolvedKind:    "message",
			SourceMessageID: held.ID,
		}},
		IdempotencyKey: "message-retention-starvation-memory",
	}); err != nil {
		t.Fatal(err)
	}
	for i, threadID := range []string{
		held.ThreadID,
		firstEligible.ThreadID,
		secondEligible.ThreadID,
	} {
		agedAt := time.Now().UTC().Add(
			-time.Duration(40-i) * 24 * time.Hour,
		)
		if _, err := st.pool.Exec(ctx, `
			WITH aged_messages AS (
			  UPDATE agent_messages
			     SET created_at=$4
			   WHERE account_id=$1 AND realm_id=$2 AND thread_id=$3
			  RETURNING id
			)
			UPDATE message_retention_thread_activity
			   SET last_message_at=$4, updated_at=statement_timestamp()
			 WHERE account_id=$1 AND realm_id=$2 AND thread_id=$3`,
			provisioned.AccountID, realm.ID, threadID, agedAt,
		); err != nil {
			t.Fatal(err)
		}
	}

	first := messageRetentionResultForAccountLane(ctx, t, st, true, 1)
	if first.Scanned != 1 || first.DeferredEvidence != 1 ||
		first.EligibleThreads != 0 || first.DeletedThreads != 0 ||
		!first.ScanCapped {
		t.Fatalf("first held-page result = %+v", first)
	}
	second := messageRetentionResultForAccountLane(ctx, t, st, true, 1)
	if second.Scanned != 1 || second.EligibleThreads != 1 ||
		second.DeletedThreads != 1 || second.DeletedMessages != 1 ||
		!second.ScanCapped {
		t.Fatalf("second eligible-page result = %+v", second)
	}
	assertMessageThreadCount(
		ctx, t, st, provisioned.AccountID, realm.ID, held.ThreadID, 1,
	)
	assertMessageThreadCount(
		ctx, t, st, provisioned.AccountID, realm.ID, firstEligible.ThreadID, 0,
	)
	assertMessageThreadCount(
		ctx, t, st, provisioned.AccountID, realm.ID, secondEligible.ThreadID, 1,
	)

	third := messageRetentionResultForAccountLane(ctx, t, st, true, 1)
	if third.Scanned != 1 || third.EligibleThreads != 1 ||
		third.DeletedThreads != 1 || third.DeletedMessages != 1 ||
		!third.ScanCapped {
		t.Fatalf("third eligible-page result = %+v", third)
	}
	assertMessageThreadCount(
		ctx, t, st, provisioned.AccountID, realm.ID, secondEligible.ThreadID, 0,
	)
}

func TestMessageRetentionReplicasClaimDifferentLanesPostgres(t *testing.T) {
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

	type fixture struct {
		accountID string
		realmID   string
		threadID  string
		lane      int
	}
	provision := func(index int) fixture {
		t.Helper()
		account, err := st.ProvisionAccount(
			ctx,
			fmt.Sprintf(
				"message-retention-replica-%s-%d@witwave.ai",
				time.Now().UTC().Format("150405.000000000"),
				index,
			),
			"message retention replica",
			time.Hour,
		)
		if err != nil {
			t.Fatal(err)
		}
		if activated, err := st.ActivateAccount(ctx, account.AccountID); err != nil || !activated {
			t.Fatalf("activate = %v / %v", activated, err)
		}
		realm, err := st.CreateRealm(ctx, account.AccountID, "default")
		if err != nil {
			t.Fatal(err)
		}
		sender, err := st.CreateAgent(ctx, account.AccountID, realm.ID, "sender")
		if err != nil {
			t.Fatal(err)
		}
		recipient, err := st.CreateAgent(ctx, account.AccountID, realm.ID, "recipient")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := st.SetAccountPlan(
			ctx,
			account.AccountID,
			0,
			"",
			"test",
			map[string]int64{},
			map[string]int64{
				MessageRetentionDaysPolicy:              30,
				plans.MessagingEntitlementVersionPolicy: plans.MessagingEntitlementVersion,
			},
			[]string{plans.MessagingFeature},
		); err != nil {
			t.Fatal(err)
		}
		principal := Principal{
			Kind: PrincipalAgent, ID: sender.ID, AccountID: account.AccountID,
			RealmID: realm.ID, AgentName: sender.Name, AccountStatus: "active",
		}
		message, err := st.SendMessage(ctx, principal, SendMessageInput{
			ToAgent: recipient.ID, Body: "expired",
			IdempotencyKey: "message-retention-replica",
		})
		if err != nil {
			t.Fatal(err)
		}
		old := time.Now().UTC().Add(-31 * 24 * time.Hour)
		if _, err := st.pool.Exec(ctx, `
			WITH aged_messages AS (
			  UPDATE agent_messages
			     SET created_at=$4
			   WHERE account_id=$1 AND realm_id=$2 AND thread_id=$3
			  RETURNING id
			)
			UPDATE message_retention_thread_activity
			   SET last_message_at=$4, updated_at=statement_timestamp()
			 WHERE account_id=$1 AND realm_id=$2 AND thread_id=$3`,
			account.AccountID, realm.ID, message.ThreadID, old,
		); err != nil {
			t.Fatal(err)
		}
		var lane int
		if err := st.pool.QueryRow(ctx, `
			SELECT get_byte(sha256(id::bytea), 0) % 16
			  FROM accounts
			 WHERE id=$1`, account.AccountID).Scan(&lane); err != nil {
			t.Fatal(err)
		}
		return fixture{
			accountID: account.AccountID,
			realmID:   realm.ID,
			threadID:  message.ThreadID,
			lane:      lane,
		}
	}

	first := provision(0)
	second := provision(1)
	for index := 2; second.lane == first.lane && index < 32; index++ {
		if _, err := st.SetAccountPlan(
			ctx,
			second.accountID,
			0,
			"",
			"test",
			map[string]int64{},
			map[string]int64{
				plans.MessagingEntitlementVersionPolicy: plans.MessagingEntitlementVersion,
			},
			[]string{plans.MessagingFeature},
		); err != nil {
			t.Fatal(err)
		}
		second = provision(index)
	}
	if second.lane == first.lane {
		t.Fatal("could not provision fixtures in two distinct worker lanes")
	}
	if _, err := st.pool.Exec(ctx, `
		UPDATE message_retention_worker_lanes
		   SET next_run_at='infinity'::timestamptz
		 WHERE mode='enforce'`); err != nil {
		t.Fatal(err)
	}
	if _, err := st.pool.Exec(ctx, `
		UPDATE message_retention_worker_lanes
		   SET next_run_at='-infinity'::timestamptz
		 WHERE mode='enforce' AND lane_id IN ($1,$2)`,
		first.lane, second.lane,
	); err != nil {
		t.Fatal(err)
	}

	type outcome struct {
		result MessageRetentionBatchResult
		err    error
	}
	start := make(chan struct{})
	outcomes := make(chan outcome, 2)
	var replicas sync.WaitGroup
	replicas.Add(2)
	for range 2 {
		go func() {
			defer replicas.Done()
			<-start
			result, err := st.ProcessMessageRetentionBatch(ctx, 1)
			outcomes <- outcome{result: result, err: err}
		}()
	}
	close(start)
	replicas.Wait()
	close(outcomes)

	var deletedThreads, deletedMessages int64
	for workerResult := range outcomes {
		if workerResult.err != nil {
			t.Fatal(workerResult.err)
		}
		if !workerResult.result.LaneAdvanced {
			t.Fatalf("replica did not claim a due lane: %+v", workerResult.result)
		}
		deletedThreads += workerResult.result.DeletedThreads
		deletedMessages += workerResult.result.DeletedMessages
	}
	if deletedThreads != 2 || deletedMessages != 2 {
		t.Fatalf(
			"replica totals = %d threads / %d messages, want 2 / 2",
			deletedThreads,
			deletedMessages,
		)
	}
	for _, item := range []fixture{first, second} {
		assertMessageThreadCount(
			ctx, t, st, item.accountID, item.realmID, item.threadID, 0,
		)
	}
}

func TestMessageRetentionContentionDefersWithoutBlockingUserReplyPostgres(t *testing.T) {
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

	account, err := st.ProvisionAccount(
		ctx,
		"message-retention-contention@witwave.ai",
		"message retention contention",
		time.Hour,
	)
	if err != nil {
		t.Fatal(err)
	}
	if activated, err := st.ActivateAccount(ctx, account.AccountID); err != nil || !activated {
		t.Fatalf("activate = %v / %v", activated, err)
	}
	realm, err := st.CreateRealm(ctx, account.AccountID, "default")
	if err != nil {
		t.Fatal(err)
	}
	sender, err := st.CreateAgent(ctx, account.AccountID, realm.ID, "sender")
	if err != nil {
		t.Fatal(err)
	}
	recipient, err := st.CreateAgent(ctx, account.AccountID, realm.ID, "recipient")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.SetAccountPlan(
		ctx,
		account.AccountID,
		0,
		"",
		"test",
		map[string]int64{},
		map[string]int64{
			MessageRetentionDaysPolicy:              30,
			plans.MessagingEntitlementVersionPolicy: plans.MessagingEntitlementVersion,
		},
		[]string{plans.MessagingFeature},
	); err != nil {
		t.Fatal(err)
	}
	principal := Principal{
		Kind: PrincipalAgent, ID: sender.ID, AccountID: account.AccountID,
		RealmID: realm.ID, AgentName: sender.Name, AccountStatus: "active",
	}
	parent, err := st.SendMessage(ctx, principal, SendMessageInput{
		ToAgent:        recipient.ID,
		Body:           "old parent",
		IdempotencyKey: "message-retention-contention-parent",
	})
	if err != nil {
		t.Fatal(err)
	}
	old := time.Now().UTC().Add(-31 * 24 * time.Hour)
	if _, err := st.pool.Exec(ctx, `
		WITH aged_messages AS (
		  UPDATE agent_messages
		     SET created_at=$4
		   WHERE account_id=$1 AND realm_id=$2 AND thread_id=$3
		  RETURNING id
		)
		UPDATE message_retention_thread_activity
		   SET last_message_at=$4, updated_at=statement_timestamp()
		 WHERE account_id=$1 AND realm_id=$2 AND thread_id=$3`,
		account.AccountID, realm.ID, parent.ThreadID, old,
	); err != nil {
		t.Fatal(err)
	}
	var lane int
	if err := st.pool.QueryRow(ctx, `
		SELECT get_byte(sha256(id::bytea), 0) % 16
		  FROM accounts
		 WHERE id=$1`, account.AccountID).Scan(&lane); err != nil {
		t.Fatal(err)
	}
	if _, err := st.pool.Exec(ctx, `
		UPDATE message_retention_worker_lanes
		   SET next_run_at='infinity'::timestamptz
		 WHERE mode='enforce'`); err != nil {
		t.Fatal(err)
	}
	if _, err := st.pool.Exec(ctx, `
		UPDATE message_retention_worker_lanes
		   SET next_run_at='-infinity'::timestamptz
		 WHERE mode='enforce' AND lane_id=$1`, lane); err != nil {
		t.Fatal(err)
	}
	var generationBefore int64
	var cursorBefore string
	if err := st.pool.QueryRow(ctx, `
		SELECT generation,account_cursor
		  FROM message_retention_worker_lanes
		 WHERE mode='enforce' AND lane_id=$1`, lane).
		Scan(&generationBefore, &cursorBefore); err != nil {
		t.Fatal(err)
	}

	writer, err := st.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = writer.Rollback(ctx) }()
	if _, err := writer.Exec(ctx, `
		SELECT 1
		  FROM agent_message_deliveries
		 WHERE message_id=$1 AND recipient_agent_id=$2
		 FOR UPDATE`,
		parent.ID, recipient.ID,
	); err != nil {
		t.Fatal(err)
	}

	type workerOutcome struct {
		result MessageRetentionBatchResult
		err    error
	}
	workerDone := make(chan workerOutcome, 1)
	go func() {
		result, err := st.ProcessMessageRetentionBatch(ctx, 1)
		workerDone <- workerOutcome{result: result, err: err}
	}()
	var outcome workerOutcome
	select {
	case outcome = <-workerDone:
	case <-time.After(2 * time.Second):
		t.Fatal("retention blocked behind a foreground delivery lock")
	}
	if outcome.err != nil {
		t.Fatalf("retention contention result: %v", outcome.err)
	}
	if outcome.result.DeferredLocked != 1 ||
		outcome.result.EligibleThreads != 0 ||
		outcome.result.DeletedThreads != 0 ||
		outcome.result.DeletedMessages != 0 {
		t.Fatalf("retention contention result = %+v", outcome.result)
	}

	// The foreground transaction remains healthy after cleanup skips its graph.
	// It can publish a reply and advance the activity fence normally.
	const replyID = "msg_bcdefghijklmnopq"
	if _, err := writer.Exec(ctx, `
		INSERT INTO agent_messages
		  (id,account_id,realm_id,from_agent_id,to_agent_id,body,thread_id,
		   reply_to_message_id,causal_depth)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,1)`,
		replyID,
		account.AccountID,
		realm.ID,
		recipient.ID,
		sender.ID,
		"reply wins over cleanup contention",
		parent.ThreadID,
		parent.ID,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Exec(ctx, `
		INSERT INTO agent_message_deliveries
		  (message_id,account_id,realm_id,recipient_agent_id,state,delivered_at)
		VALUES ($1,$2,$3,$4,'delivered',statement_timestamp())`,
		replyID, account.AccountID, realm.ID, sender.ID,
	); err != nil {
		t.Fatal(err)
	}
	if err := writer.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	assertMessageThreadCount(
		ctx, t, st, account.AccountID, realm.ID, parent.ThreadID, 2,
	)
	var generationAfter, scanStates int64
	var cursorAfter string
	if err := st.pool.QueryRow(ctx, `
		SELECT generation,account_cursor
		  FROM message_retention_worker_lanes
		 WHERE mode='enforce' AND lane_id=$1`, lane).
		Scan(&generationAfter, &cursorAfter); err != nil {
		t.Fatal(err)
	}
	if err := st.pool.QueryRow(ctx, `
		SELECT count(*)
		  FROM message_retention_account_scan_state
		 WHERE mode='enforce' AND account_id=$1`, account.AccountID).
		Scan(&scanStates); err != nil {
		t.Fatal(err)
	}
	if generationAfter != generationBefore+1 ||
		cursorAfter == cursorBefore ||
		scanStates != 1 {
		t.Fatalf(
			"retention did not advance past contended graph: generation %d->%d cursor %q->%q scan states %d",
			generationBefore,
			generationAfter,
			cursorBefore,
			cursorAfter,
			scanStates,
		)
	}
	var lastMessageAt time.Time
	if err := st.pool.QueryRow(ctx, `
		SELECT last_message_at
		  FROM message_retention_thread_activity
		 WHERE account_id=$1 AND realm_id=$2 AND thread_id=$3`,
		account.AccountID, realm.ID, parent.ThreadID,
	).Scan(&lastMessageAt); err != nil {
		t.Fatal(err)
	}
	if !lastMessageAt.After(old) {
		t.Fatalf("reply did not advance activity: %s <= %s", lastMessageAt, old)
	}
}

func TestMessageRetentionRepairsStaleActivityBeforeDeletePostgres(t *testing.T) {
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
	fixture := newMessageRetentionTestFixture(ctx, t, st, "stale-projection")
	message, err := st.SendMessage(ctx, fixture.principal, SendMessageInput{
		ToAgent: fixture.recipientID, Body: "recent message",
		IdempotencyKey: "message-retention-stale-projection",
	})
	if err != nil {
		t.Fatal(err)
	}
	old := time.Now().UTC().Add(-31 * 24 * time.Hour)
	if _, err := st.pool.Exec(ctx, `
		UPDATE message_retention_thread_activity
		   SET last_message_at=$4, updated_at=statement_timestamp()
		 WHERE account_id=$1 AND realm_id=$2 AND thread_id=$3`,
		fixture.accountID, fixture.realmID, message.ThreadID, old,
	); err != nil {
		t.Fatal(err)
	}

	result := messageRetentionResultForAccountLane(ctx, t, st, true, 1)
	if result.RepairedActivity != 1 ||
		result.EligibleThreads != 0 ||
		result.DeletedThreads != 0 ||
		result.DeletedMessages != 0 {
		t.Fatalf("stale projection result = %+v", result)
	}
	assertMessageThreadCount(
		ctx, t, st, fixture.accountID, fixture.realmID, message.ThreadID, 1,
	)
	var repaired time.Time
	if err := st.pool.QueryRow(ctx, `
		SELECT last_message_at
		  FROM message_retention_thread_activity
		 WHERE account_id=$1 AND realm_id=$2 AND thread_id=$3`,
		fixture.accountID, fixture.realmID, message.ThreadID,
	).Scan(&repaired); err != nil {
		t.Fatal(err)
	}
	if repaired.Before(message.CreatedAt) {
		t.Fatalf("activity was not repaired: %s < %s", repaired, message.CreatedAt)
	}
}

func TestMessageRetentionOversizeThreadQuarantinesWithoutStarvationPostgres(t *testing.T) {
	baseDSN := os.Getenv("WITSELF_TEST_DATABASE_URL")
	if baseDSN == "" {
		t.Skip("WITSELF_TEST_DATABASE_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	st, _ := newMigrationTestStore(t, baseDSN)
	if err := st.Migrate(); err != nil {
		t.Fatal(err)
	}
	fixture := newMessageRetentionTestFixture(ctx, t, st, "oversize")
	normal, err := st.SendMessage(ctx, fixture.principal, SendMessageInput{
		ToAgent: fixture.recipientID, Body: "later eligible thread",
		IdempotencyKey: "message-retention-after-oversize",
	})
	if err != nil {
		t.Fatal(err)
	}
	normalAt := time.Now().UTC().Add(-40 * 24 * time.Hour)
	if _, err := st.pool.Exec(ctx, `
		WITH aged_messages AS (
		  UPDATE agent_messages
		     SET created_at=$4
		   WHERE account_id=$1 AND realm_id=$2 AND thread_id=$3
		  RETURNING id
		)
		UPDATE message_retention_thread_activity
		   SET last_message_at=$4, updated_at=statement_timestamp()
		 WHERE account_id=$1 AND realm_id=$2 AND thread_id=$3`,
		fixture.accountID, fixture.realmID, normal.ThreadID, normalAt,
	); err != nil {
		t.Fatal(err)
	}

	const oversizedThread = "thr_oversized_retention"
	oversizedAt := time.Now().UTC().Add(-60 * 24 * time.Hour)
	if _, err := st.pool.Exec(ctx, `
		INSERT INTO agent_messages
		  (id,account_id,realm_id,from_agent_id,to_agent_id,body,thread_id,created_at)
		SELECT
		  'msg_oversized_' || lpad(value::text, 8, '0'),
		  $1,$2,$3,$4,'oversized retention fixture',$5,$6
		  FROM generate_series(1,$7::integer) value`,
		fixture.accountID,
		fixture.realmID,
		fixture.principal.ID,
		fixture.recipientID,
		oversizedThread,
		oversizedAt,
		maxMessageRetentionThreadMessages+1,
	); err != nil {
		t.Fatal(err)
	}

	result := messageRetentionResultForAccountLane(ctx, t, st, true, 2)
	if result.Scanned != 2 ||
		result.DeferredOversize != 1 ||
		result.EligibleThreads != 1 ||
		result.DeletedThreads != 1 ||
		result.DeletedMessages != 1 {
		t.Fatalf("oversize quarantine result = %+v", result)
	}
	assertMessageThreadCount(
		ctx, t, st, fixture.accountID, fixture.realmID, normal.ThreadID, 0,
	)
	assertMessageThreadCount(
		ctx,
		t,
		st,
		fixture.accountID,
		fixture.realmID,
		oversizedThread,
		maxMessageRetentionThreadMessages+1,
	)
	var reason string
	var count int64
	var retryScheduled bool
	if err := st.pool.QueryRow(ctx, `
		SELECT defer_reason,defer_count,retry_after > statement_timestamp()
		  FROM message_retention_thread_activity
		 WHERE account_id=$1 AND realm_id=$2 AND thread_id=$3`,
		fixture.accountID, fixture.realmID, oversizedThread,
	).Scan(&reason, &count, &retryScheduled); err != nil {
		t.Fatal(err)
	}
	if reason != "oversize" || count != 1 || !retryScheduled {
		t.Fatalf(
			"oversize quarantine = reason %q count %d retry scheduled %v",
			reason,
			count,
			retryScheduled,
		)
	}
}

func TestMessageRetentionCumulativeGraphBudgetDefersLaterThreadsPostgres(
	t *testing.T,
) {
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
	fixture := newMessageRetentionTestFixture(ctx, t, st, "batch-budget")
	first, err := st.SendMessage(ctx, fixture.principal, SendMessageInput{
		ToAgent:        fixture.recipientID,
		Body:           "first expired budget thread",
		IdempotencyKey: "message-retention-budget-first",
	})
	if err != nil {
		t.Fatal(err)
	}
	second, err := st.SendMessage(ctx, fixture.principal, SendMessageInput{
		ToAgent:        fixture.recipientID,
		Body:           "second expired budget thread",
		IdempotencyKey: "message-retention-budget-second",
	})
	if err != nil {
		t.Fatal(err)
	}
	firstAt := time.Now().UTC().Add(-40 * 24 * time.Hour)
	secondAt := firstAt.Add(24 * time.Hour)
	for _, item := range []struct {
		threadID string
		agedAt   time.Time
	}{
		{threadID: first.ThreadID, agedAt: firstAt},
		{threadID: second.ThreadID, agedAt: secondAt},
	} {
		if _, err := st.pool.Exec(ctx, `
			WITH aged_messages AS (
			  UPDATE agent_messages
			     SET created_at=$4
			   WHERE account_id=$1 AND realm_id=$2 AND thread_id=$3
			  RETURNING id
			)
			UPDATE message_retention_thread_activity
			   SET last_message_at=$4, updated_at=statement_timestamp()
			 WHERE account_id=$1 AND realm_id=$2 AND thread_id=$3`,
			fixture.accountID,
			fixture.realmID,
			item.threadID,
			item.agedAt,
		); err != nil {
			t.Fatal(err)
		}
	}
	var lane int
	if err := st.pool.QueryRow(ctx, `
		SELECT get_byte(sha256(id::bytea), 0) % 16
		  FROM accounts
		 WHERE id=$1`, fixture.accountID).Scan(&lane); err != nil {
		t.Fatal(err)
	}
	if _, err := st.pool.Exec(ctx, `
		UPDATE message_retention_worker_lanes
		   SET next_run_at='infinity'::timestamptz
		 WHERE mode IN ('preview','enforce')`); err != nil {
		t.Fatal(err)
	}
	if _, err := st.pool.Exec(ctx, `
		UPDATE message_retention_worker_lanes
		   SET next_run_at='-infinity'::timestamptz
		 WHERE mode='preview' AND lane_id=$1`, lane); err != nil {
		t.Fatal(err)
	}

	// One ordinary direct message has two graph rows: the message and its
	// delivery. A two-row injected test ceiling therefore admits the oldest
	// thread and defers the next one without exercising the production-sized
	// 65,536-row ceiling in a test database. Preview must advance only to the
	// admitted prefix, so its next pass reaches the deferred thread even
	// though preview deliberately leaves the first thread in place.
	firstPreview, err := st.processMessageRetentionBatchWithGraphBudget(
		ctx,
		2,
		false,
		0,
		minMessageRetentionBatchTimeout,
		defaultMessageRetentionWorkerLaneCount,
		2,
	)
	if err != nil {
		t.Fatal(err)
	}
	if firstPreview.Scanned != 2 ||
		firstPreview.EligibleThreads != 1 ||
		firstPreview.DeletedThreads != 0 ||
		firstPreview.DeletedMessages != 0 ||
		firstPreview.DeferredBudget != 1 ||
		!firstPreview.ScanCapped {
		t.Fatalf("first cumulative-budget preview = %+v", firstPreview)
	}
	secondPreview, err := st.processMessageRetentionBatchWithGraphBudget(
		ctx,
		2,
		false,
		0,
		minMessageRetentionBatchTimeout,
		defaultMessageRetentionWorkerLaneCount,
		2,
	)
	if err != nil {
		t.Fatal(err)
	}
	if secondPreview.Scanned != 1 ||
		secondPreview.EligibleThreads != 1 ||
		secondPreview.DeletedThreads != 0 ||
		secondPreview.DeletedMessages != 0 ||
		secondPreview.DeferredBudget != 0 ||
		secondPreview.ScanCapped {
		t.Fatalf("second cumulative-budget preview = %+v", secondPreview)
	}
	assertMessageThreadCount(
		ctx,
		t,
		st,
		fixture.accountID,
		fixture.realmID,
		first.ThreadID,
		1,
	)
	assertMessageThreadCount(
		ctx,
		t,
		st,
		fixture.accountID,
		fixture.realmID,
		second.ThreadID,
		1,
	)

	// In enforcement, a foreground lock on the admitted oldest thread must
	// not let that contended thread consume the budget forever. The first pass
	// advances the account scan only through that prefix; the next pass can
	// delete the later thread while the foreground owner still holds its lock.
	if _, err := st.pool.Exec(ctx, `
		UPDATE message_retention_worker_lanes
		   SET next_run_at='-infinity'::timestamptz
		 WHERE mode='enforce' AND lane_id=$1`, lane); err != nil {
		t.Fatal(err)
	}
	blocker, err := st.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = blocker.Rollback(ctx) }()
	if _, err := blocker.Exec(ctx, `
		SELECT 1
		  FROM agent_message_deliveries
		 WHERE message_id=$1
		 FOR UPDATE`, first.ID); err != nil {
		t.Fatal(err)
	}
	firstEnforce, err := st.processMessageRetentionBatchWithGraphBudget(
		ctx,
		2,
		true,
		0,
		minMessageRetentionBatchTimeout,
		defaultMessageRetentionWorkerLaneCount,
		2,
	)
	if err != nil {
		t.Fatal(err)
	}
	if firstEnforce.Scanned != 2 ||
		firstEnforce.EligibleThreads != 0 ||
		firstEnforce.DeletedThreads != 0 ||
		firstEnforce.DeletedMessages != 0 ||
		firstEnforce.DeferredLocked != 1 ||
		firstEnforce.DeferredBudget != 1 ||
		!firstEnforce.ScanCapped {
		t.Fatalf("first cumulative-budget enforcement = %+v", firstEnforce)
	}
	secondEnforce, err := st.processMessageRetentionBatchWithGraphBudget(
		ctx,
		2,
		true,
		0,
		minMessageRetentionBatchTimeout,
		defaultMessageRetentionWorkerLaneCount,
		2,
	)
	if err != nil {
		t.Fatal(err)
	}
	if secondEnforce.Scanned != 1 ||
		secondEnforce.EligibleThreads != 1 ||
		secondEnforce.DeletedThreads != 1 ||
		secondEnforce.DeletedMessages != 1 ||
		secondEnforce.DeferredLocked != 0 ||
		secondEnforce.DeferredBudget != 0 ||
		secondEnforce.ScanCapped {
		t.Fatalf("second cumulative-budget enforcement = %+v", secondEnforce)
	}
	assertMessageThreadCount(
		ctx,
		t,
		st,
		fixture.accountID,
		fixture.realmID,
		first.ThreadID,
		1,
	)
	assertMessageThreadCount(
		ctx,
		t,
		st,
		fixture.accountID,
		fixture.realmID,
		second.ThreadID,
		0,
	)
}

func TestMessageRetentionTimedOutLaneBacksOffWhileLaterLaneProgressesPostgres(
	t *testing.T,
) {
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

	type fixtureWithLane struct {
		messageRetentionTestFixture
		threadID string
		lane     int
	}
	provision := func(index int) fixtureWithLane {
		t.Helper()
		fixture := newMessageRetentionTestFixture(
			ctx,
			t,
			st,
			fmt.Sprintf("lane-timeout-%d-%d", time.Now().UnixNano(), index),
		)
		message, err := st.SendMessage(ctx, fixture.principal, SendMessageInput{
			ToAgent: fixture.recipientID,
			Body:    "expired lane-timeout fixture",
			IdempotencyKey: fmt.Sprintf(
				"message-retention-lane-timeout-%d",
				index,
			),
		})
		if err != nil {
			t.Fatal(err)
		}
		old := time.Now().UTC().Add(-31 * 24 * time.Hour)
		if _, err := st.pool.Exec(ctx, `
			WITH aged_messages AS (
			  UPDATE agent_messages
			     SET created_at=$4
			   WHERE account_id=$1 AND realm_id=$2 AND thread_id=$3
			  RETURNING id
			)
			UPDATE message_retention_thread_activity
			   SET last_message_at=$4, updated_at=statement_timestamp()
			 WHERE account_id=$1 AND realm_id=$2 AND thread_id=$3`,
			fixture.accountID,
			fixture.realmID,
			message.ThreadID,
			old,
		); err != nil {
			t.Fatal(err)
		}
		var lane int
		if err := st.pool.QueryRow(ctx, `
			SELECT get_byte(sha256(id::bytea), 0) % 16
			  FROM accounts
			 WHERE id=$1`, fixture.accountID).Scan(&lane); err != nil {
			t.Fatal(err)
		}
		return fixtureWithLane{
			messageRetentionTestFixture: fixture,
			threadID:                    message.ThreadID,
			lane:                        lane,
		}
	}

	first := provision(0)
	second := provision(1)
	for index := 2; second.lane == first.lane && index < 32; index++ {
		if _, err := st.SetAccountPlan(
			ctx,
			second.accountID,
			0,
			"",
			"test",
			map[string]int64{},
			map[string]int64{
				plans.MessagingEntitlementVersionPolicy: plans.MessagingEntitlementVersion,
			},
			[]string{plans.MessagingFeature},
		); err != nil {
			t.Fatal(err)
		}
		second = provision(index)
	}
	if second.lane == first.lane {
		t.Fatal("could not provision timeout fixtures in distinct worker lanes")
	}

	if _, err := st.pool.Exec(ctx, `
		UPDATE message_retention_worker_lanes
		   SET next_run_at='infinity'::timestamptz
		 WHERE mode='enforce'`); err != nil {
		t.Fatal(err)
	}
	if _, err := st.pool.Exec(ctx, `
		UPDATE message_retention_worker_lanes
		   SET next_run_at=CASE
		         WHEN lane_id=$1 THEN '-infinity'::timestamptz
		         WHEN lane_id=$2 THEN 'epoch'::timestamptz
		         ELSE next_run_at
		       END
		 WHERE mode='enforce' AND lane_id IN ($1,$2)`,
		first.lane,
		second.lane,
	); err != nil {
		t.Fatal(err)
	}
	var firstGenerationBefore int64
	if err := st.pool.QueryRow(ctx, `
		SELECT generation
		  FROM message_retention_worker_lanes
		 WHERE mode='enforce' AND lane_id=$1`, first.lane).
		Scan(&firstGenerationBefore); err != nil {
		t.Fatal(err)
	}

	blocker, err := st.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = blocker.Rollback(ctx) }()
	if _, err := blocker.Exec(ctx, `
		LOCK TABLE agent_messages IN ACCESS EXCLUSIVE MODE`); err != nil {
		t.Fatal(err)
	}

	timeoutCtx, timeoutCancel := context.WithTimeout(ctx, 2*time.Second)
	defer timeoutCancel()
	timedOut, err := st.processMessageRetentionBatch(
		timeoutCtx,
		1,
		true,
		0,
		minMessageRetentionBatchTimeout,
		defaultMessageRetentionWorkerLaneCount,
	)
	if err == nil || !strings.Contains(err.Error(), "context deadline exceeded") {
		t.Fatalf("blocked batch result = %+v / %v, want deadline", timedOut, err)
	}
	var firstGenerationAfter int64
	var firstLaneBackedOff bool
	if err := st.pool.QueryRow(ctx, `
		SELECT generation,next_run_at > statement_timestamp()
		  FROM message_retention_worker_lanes
		 WHERE mode='enforce' AND lane_id=$1`, first.lane).
		Scan(&firstGenerationAfter, &firstLaneBackedOff); err != nil {
		t.Fatal(err)
	}
	if firstGenerationAfter != firstGenerationBefore+1 || !firstLaneBackedOff {
		t.Fatalf(
			"timed-out lane lease = generation %d->%d backed off %v",
			firstGenerationBefore,
			firstGenerationAfter,
			firstLaneBackedOff,
		)
	}
	if err := blocker.Rollback(ctx); err != nil {
		t.Fatal(err)
	}

	next, err := st.processMessageRetentionBatch(
		ctx,
		1,
		true,
		0,
		minMessageRetentionBatchTimeout,
		defaultMessageRetentionWorkerLaneCount,
	)
	if err != nil {
		t.Fatal(err)
	}
	if !next.LaneAdvanced ||
		next.DeletedThreads != 1 ||
		next.DeletedMessages != 1 {
		t.Fatalf("later-lane result = %+v", next)
	}
	assertMessageThreadCount(
		ctx,
		t,
		st,
		first.accountID,
		first.realmID,
		first.threadID,
		1,
	)
	assertMessageThreadCount(
		ctx,
		t,
		st,
		second.accountID,
		second.realmID,
		second.threadID,
		0,
	)
}

func TestMessageRetentionStaleLaneGenerationCannotDeletePostgres(t *testing.T) {
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
	fixture := newMessageRetentionTestFixture(ctx, t, st, "stale-lane")
	message, err := st.SendMessage(ctx, fixture.principal, SendMessageInput{
		ToAgent:        fixture.recipientID,
		Body:           "expired stale-lane fixture",
		IdempotencyKey: "message-retention-stale-lane",
	})
	if err != nil {
		t.Fatal(err)
	}
	old := time.Now().UTC().Add(-31 * 24 * time.Hour)
	if _, err := st.pool.Exec(ctx, `
		WITH aged_messages AS (
		  UPDATE agent_messages
		     SET created_at=$4
		   WHERE account_id=$1 AND realm_id=$2 AND thread_id=$3
		  RETURNING id
		)
		UPDATE message_retention_thread_activity
		   SET last_message_at=$4, updated_at=statement_timestamp()
		 WHERE account_id=$1 AND realm_id=$2 AND thread_id=$3`,
		fixture.accountID,
		fixture.realmID,
		message.ThreadID,
		old,
	); err != nil {
		t.Fatal(err)
	}
	var lane int
	if err := st.pool.QueryRow(ctx, `
		SELECT get_byte(sha256(id::bytea), 0) % 16
		  FROM accounts
		 WHERE id=$1`, fixture.accountID).Scan(&lane); err != nil {
		t.Fatal(err)
	}
	if _, err := st.pool.Exec(ctx, `
		UPDATE message_retention_worker_lanes
		   SET next_run_at='infinity'::timestamptz
		 WHERE mode='enforce'`); err != nil {
		t.Fatal(err)
	}
	if _, err := st.pool.Exec(ctx, `
		UPDATE message_retention_worker_lanes
		   SET next_run_at='-infinity'::timestamptz
		 WHERE mode='enforce' AND lane_id=$1`, lane); err != nil {
		t.Fatal(err)
	}

	stale, err := st.claimMessageRetentionLane(
		ctx,
		MessageRetentionModeEnforce,
		0,
		minMessageRetentionBatchTimeout,
		defaultMessageRetentionWorkerLaneCount,
	)
	if err != nil {
		t.Fatal(err)
	}
	if stale == nil || stale.LaneID != lane {
		t.Fatalf("stale claim = %+v, want lane %d", stale, lane)
	}

	// Move the database-owned lease clock past due without changing its
	// generation, then let a replacement worker take the same lane. This is a
	// deterministic simulation of expiry without sleeping for the 10-second
	// production minimum.
	if _, err := st.pool.Exec(ctx, `
		UPDATE message_retention_worker_lanes
		   SET next_run_at='-infinity'::timestamptz
		 WHERE mode='enforce' AND lane_id=$1 AND generation=$2`,
		stale.LaneID,
		stale.Generation,
	); err != nil {
		t.Fatal(err)
	}
	replacement, err := st.claimMessageRetentionLane(
		ctx,
		MessageRetentionModeEnforce,
		0,
		minMessageRetentionBatchTimeout,
		defaultMessageRetentionWorkerLaneCount,
	)
	if err != nil {
		t.Fatal(err)
	}
	if replacement == nil ||
		replacement.LaneID != stale.LaneID ||
		replacement.Generation != stale.Generation+1 {
		t.Fatalf(
			"replacement claim = %+v, want lane %d generation %d",
			replacement,
			stale.LaneID,
			stale.Generation+1,
		)
	}

	staleResult, err := st.processClaimedMessageRetentionBatch(
		ctx,
		1,
		true,
		0,
		defaultMessageRetentionWorkerLaneCount,
		maxMessageRetentionBatchGraphRows,
		MessageRetentionModeEnforce,
		*stale,
	)
	if err != nil {
		t.Fatal(err)
	}
	if staleResult != (MessageRetentionBatchResult{}) {
		t.Fatalf("stale generation mutated lane work: %+v", staleResult)
	}
	assertMessageThreadCount(
		ctx,
		t,
		st,
		fixture.accountID,
		fixture.realmID,
		message.ThreadID,
		1,
	)

	replacementResult, err := st.processClaimedMessageRetentionBatch(
		ctx,
		1,
		true,
		0,
		defaultMessageRetentionWorkerLaneCount,
		maxMessageRetentionBatchGraphRows,
		MessageRetentionModeEnforce,
		*replacement,
	)
	if err != nil {
		t.Fatal(err)
	}
	if !replacementResult.LaneAdvanced ||
		replacementResult.DeletedThreads != 1 ||
		replacementResult.DeletedMessages != 1 {
		t.Fatalf("replacement generation result = %+v", replacementResult)
	}
	assertMessageThreadCount(
		ctx,
		t,
		st,
		fixture.accountID,
		fixture.realmID,
		message.ThreadID,
		0,
	)
}

func TestMessageRetentionMigrationBackfillsAndReappliesPostgres(t *testing.T) {
	baseDSN := os.Getenv("WITSELF_TEST_DATABASE_URL")
	if baseDSN == "" {
		t.Skip("WITSELF_TEST_DATABASE_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	st, dsn := newMigrationTestStore(t, baseDSN)

	assertArtifacts := func(want bool) {
		t.Helper()
		assertMigrationTestTable(t, st, "message_retention_thread_activity", want)
		assertMigrationTestTable(t, st, "message_retention_account_scan_state", want)
		assertMigrationTestTable(t, st, "message_retention_worker_lanes", want)
		assertMigrationTestIndex(
			t,
			st,
			"message_retention_thread_activity",
			"message_retention_activity_account_age_idx",
			want,
		)
		assertMigrationTestIndex(
			t,
			st,
			"accounts",
			"accounts_message_retention_worker_lane_idx",
			want,
		)
		assertMigrationTestIndex(
			t,
			st,
			"memory_evidence",
			"memory_evidence_by_source_message",
			want,
		)
		var triggerExists, functionExists bool
		if err := st.pool.QueryRow(ctx, `
			SELECT EXISTS (
			         SELECT 1
			           FROM pg_trigger
			          WHERE tgrelid=to_regclass('agent_messages')
			            AND tgname='agent_messages_track_retention_activity'
			            AND NOT tgisinternal
			       ),
			       to_regprocedure(
			         'witself_track_message_thread_activity()'
			       ) IS NOT NULL`,
		).Scan(&triggerExists, &functionExists); err != nil {
			t.Fatal(err)
		}
		if triggerExists != want || functionExists != want {
			t.Fatalf(
				"retention trigger/function exist = %v/%v, want %v/%v",
				triggerExists,
				functionExists,
				want,
				want,
			)
		}
	}
	assertWorkerLanes := func() {
		t.Helper()
		var total, preview, enforce, inRange, distinctLaneIDs int64
		if err := st.pool.QueryRow(ctx, `
			SELECT count(*),
			       count(*) FILTER (WHERE mode='preview'),
			       count(*) FILTER (WHERE mode='enforce'),
			       count(*) FILTER (WHERE lane_id BETWEEN 0 AND 15),
			       count(DISTINCT lane_id)
			  FROM message_retention_worker_lanes`,
		).Scan(
			&total,
			&preview,
			&enforce,
			&inRange,
			&distinctLaneIDs,
		); err != nil {
			t.Fatal(err)
		}
		if total != 32 ||
			preview != 16 ||
			enforce != 16 ||
			inRange != 32 ||
			distinctLaneIDs != 16 {
			t.Fatalf(
				"retention lanes = total %d preview %d enforce %d in-range %d distinct IDs %d",
				total,
				preview,
				enforce,
				inRange,
				distinctLaneIDs,
			)
		}
	}

	migrationTestUpTo(t, dsn, 67)
	assertMigrationTestVersion(t, dsn, 67)
	assertArtifacts(false)

	fixture := newMessageRetentionTestFixture(ctx, t, st, "migration-68")
	type historicalMessageRow struct {
		ID        string
		ThreadID  string
		CreatedAt time.Time
	}
	insertHistoricalMessage := func(
		id, fromAgentID, toAgentID, threadID, parentID, body string,
		causalDepth int64,
		createdAt time.Time,
	) historicalMessageRow {
		t.Helper()
		tx, err := st.pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = tx.Rollback(ctx) }()

		var storedAt time.Time
		if err := tx.QueryRow(ctx, `
			INSERT INTO agent_messages
			  (id,account_id,realm_id,from_agent_id,to_agent_id,body,thread_id,
			   reply_to_message_id,causal_depth,created_at)
			VALUES ($1,$2,$3,$4,$5,$6,$7,NULLIF($8,''),$9,$10)
			RETURNING created_at`,
			id,
			fixture.accountID,
			fixture.realmID,
			fromAgentID,
			toAgentID,
			body,
			threadID,
			parentID,
			causalDepth,
			createdAt.UTC().Truncate(time.Microsecond),
		).Scan(&storedAt); err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO agent_message_deliveries
			  (message_id,account_id,realm_id,recipient_agent_id,state,
			   delivered_at,created_at)
			VALUES ($1,$2,$3,$4,'delivered',$5,$5)`,
			id,
			fixture.accountID,
			fixture.realmID,
			toAgentID,
			storedAt,
		); err != nil {
			t.Fatal(err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		return historicalMessageRow{ID: id, ThreadID: threadID, CreatedAt: storedAt}
	}

	const migrationThreadID = "thr_message_retention_migration"
	preUpParentAt := time.Now().UTC().Add(-2 * time.Hour).Truncate(time.Microsecond)
	preUpReplyAt := preUpParentAt.Add(time.Hour)
	parent := insertHistoricalMessage(
		"msg_retention_migration_parent",
		fixture.principal.ID,
		fixture.recipientID,
		migrationThreadID,
		"",
		"pre-up parent",
		1,
		preUpParentAt,
	)
	preUpReply := insertHistoricalMessage(
		"msg_retention_migration_pre_up_reply",
		fixture.recipientID,
		fixture.principal.ID,
		migrationThreadID,
		parent.ID,
		"pre-up reply",
		2,
		preUpReplyAt,
	)
	if preUpReply.ThreadID != parent.ThreadID {
		t.Fatalf(
			"pre-up reply thread = %q, want %q",
			preUpReply.ThreadID,
			parent.ThreadID,
		)
	}
	assertMessageThreadCount(
		ctx, t, st, fixture.accountID, fixture.realmID, parent.ThreadID, 2,
	)

	assertThreadActivity := func(wantLastMessageAt time.Time, wantMessages int64) {
		t.Helper()
		assertMessageThreadCount(
			ctx,
			t,
			st,
			fixture.accountID,
			fixture.realmID,
			parent.ThreadID,
			wantMessages,
		)
		var activityRows int64
		if err := st.pool.QueryRow(ctx, `
			SELECT count(*)
			  FROM message_retention_thread_activity
			 WHERE account_id=$1 AND realm_id=$2 AND thread_id=$3`,
			fixture.accountID,
			fixture.realmID,
			parent.ThreadID,
		).Scan(&activityRows); err != nil {
			t.Fatal(err)
		}
		if activityRows != 1 {
			t.Fatalf("thread activity rows = %d, want 1", activityRows)
		}
		var activityAt, actualMax time.Time
		if err := st.pool.QueryRow(ctx, `
			SELECT activity.last_message_at,
			       (
			         SELECT max(message.created_at)
			           FROM agent_messages message
			          WHERE message.account_id=activity.account_id
			            AND message.realm_id=activity.realm_id
			            AND message.thread_id=activity.thread_id
			       )
			  FROM message_retention_thread_activity activity
			 WHERE activity.account_id=$1
			   AND activity.realm_id=$2
			   AND activity.thread_id=$3`,
			fixture.accountID,
			fixture.realmID,
			parent.ThreadID,
		).Scan(&activityAt, &actualMax); err != nil {
			t.Fatal(err)
		}
		if !activityAt.Equal(wantLastMessageAt) ||
			!activityAt.Equal(actualMax) {
			t.Fatalf(
				"thread activity = %s, want %s and message max %s",
				activityAt,
				wantLastMessageAt,
				actualMax,
			)
		}
	}

	migrationTestUpTo(t, dsn, 68)
	assertMigrationTestVersion(t, dsn, 68)
	assertArtifacts(true)
	assertWorkerLanes()
	assertThreadActivity(preUpReplyAt, 2)

	postUpReply := insertHistoricalMessage(
		"msg_retention_migration_post_up_reply",
		fixture.principal.ID,
		fixture.recipientID,
		migrationThreadID,
		preUpReply.ID,
		"post-up trigger reply",
		3,
		time.Now().UTC(),
	)
	if postUpReply.ThreadID != parent.ThreadID {
		t.Fatalf(
			"post-up reply thread = %q, want %q",
			postUpReply.ThreadID,
			parent.ThreadID,
		)
	}
	if !postUpReply.CreatedAt.After(preUpReplyAt) {
		t.Fatalf(
			"post-up reply time = %s, want after backfill %s",
			postUpReply.CreatedAt,
			preUpReplyAt,
		)
	}
	assertThreadActivity(postUpReply.CreatedAt, 3)

	if err := migrationTestDown(t, dsn, false); err != nil {
		t.Fatal(err)
	}
	assertMigrationTestVersion(t, dsn, 67)
	assertArtifacts(false)
	assertMessageThreadCount(
		ctx, t, st, fixture.accountID, fixture.realmID, parent.ThreadID, 3,
	)

	duringDownReply := insertHistoricalMessage(
		"msg_retention_migration_down_reply",
		fixture.recipientID,
		fixture.principal.ID,
		migrationThreadID,
		postUpReply.ID,
		"reply while migration 68 is down",
		4,
		time.Now().UTC(),
	)
	if duringDownReply.ThreadID != parent.ThreadID {
		t.Fatalf(
			"during-down reply thread = %q, want %q",
			duringDownReply.ThreadID,
			parent.ThreadID,
		)
	}
	reapplyPostUpAt := time.Now().UTC().Add(-30 * time.Minute).Truncate(time.Microsecond)
	reapplyDownAt := reapplyPostUpAt.Add(time.Minute)
	tag, err := st.pool.Exec(ctx, `
		UPDATE agent_messages
		   SET created_at=CASE id
		         WHEN $4 THEN $6::timestamptz
		         WHEN $5 THEN $7::timestamptz
		         ELSE created_at
		       END
		 WHERE account_id=$1 AND realm_id=$2 AND thread_id=$3
		   AND id IN ($4,$5)`,
		fixture.accountID,
		fixture.realmID,
		parent.ThreadID,
		postUpReply.ID,
		duringDownReply.ID,
		reapplyPostUpAt,
		reapplyDownAt,
	)
	if err != nil {
		t.Fatal(err)
	}
	if tag.RowsAffected() != 2 {
		t.Fatalf("aged Down/reapply messages = %d, want 2", tag.RowsAffected())
	}
	assertMessageThreadCount(
		ctx, t, st, fixture.accountID, fixture.realmID, parent.ThreadID, 4,
	)

	migrationTestUpTo(t, dsn, 68)
	assertMigrationTestVersion(t, dsn, 68)
	assertArtifacts(true)
	assertWorkerLanes()
	assertThreadActivity(reapplyDownAt, 4)

	// Repeating the target-version operation is the process retry path. It
	// must preserve the one activity row and exactly the schema-owned 32 lanes.
	migrationTestUpTo(t, dsn, 68)
	assertMigrationTestVersion(t, dsn, 68)
	assertArtifacts(true)
	assertWorkerLanes()
	assertThreadActivity(reapplyDownAt, 4)

	afterReapplyReply := insertHistoricalMessage(
		"msg_retention_migration_reapply_reply",
		fixture.principal.ID,
		fixture.recipientID,
		migrationThreadID,
		duringDownReply.ID,
		"reply after migration 68 reapply",
		5,
		time.Now().UTC(),
	)
	if !afterReapplyReply.CreatedAt.After(reapplyDownAt) {
		t.Fatalf(
			"post-reapply reply time = %s, want after backfill %s",
			afterReapplyReply.CreatedAt,
			reapplyDownAt,
		)
	}
	assertThreadActivity(afterReapplyReply.CreatedAt, 5)
}

func TestMessageRetentionInterruptedDownFailsClosedPostgres(t *testing.T) {
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
	fixture := newMessageRetentionTestFixture(ctx, t, st, "interrupted-down")
	parent, err := st.SendMessage(ctx, fixture.principal, SendMessageInput{
		ToAgent: fixture.recipientID, Body: "old parent",
		IdempotencyKey: "message-retention-interrupted-down-parent",
	})
	if err != nil {
		t.Fatal(err)
	}
	old := time.Now().UTC().Add(-31 * 24 * time.Hour)
	if _, err := st.pool.Exec(ctx, `
		WITH aged_messages AS (
		  UPDATE agent_messages
		     SET created_at=$4
		   WHERE account_id=$1 AND realm_id=$2 AND thread_id=$3
		  RETURNING id
		)
		UPDATE message_retention_thread_activity
		   SET last_message_at=$4, updated_at=statement_timestamp()
		 WHERE account_id=$1 AND realm_id=$2 AND thread_id=$3`,
		fixture.accountID, fixture.realmID, parent.ThreadID, old,
	); err != nil {
		t.Fatal(err)
	}

	// These are the first three statements of migration 68 Down. Removing the
	// durable lanes first makes every later worker invocation fail closed even
	// if the downgrade stops after removing the activity trigger.
	if _, err := st.pool.Exec(ctx, `
		DROP TABLE message_retention_worker_lanes;
		DROP TRIGGER agent_messages_track_retention_activity ON agent_messages;
		DROP FUNCTION witself_track_message_thread_activity()`); err != nil {
		t.Fatal(err)
	}
	const replyID = "msg_interrupted_down"
	if _, err := st.pool.Exec(ctx, `
		INSERT INTO agent_messages
		  (id,account_id,realm_id,from_agent_id,to_agent_id,body,thread_id,
		   reply_to_message_id,causal_depth)
		VALUES ($1,$2,$3,$4,$5,'new reply',$6,$7,1)`,
		replyID,
		fixture.accountID,
		fixture.realmID,
		fixture.recipientID,
		fixture.principal.ID,
		parent.ThreadID,
		parent.ID,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := st.ProcessMessageRetentionBatch(ctx, 1); err == nil ||
		!strings.Contains(err.Error(), "message_retention_worker_lanes") {
		t.Fatalf("cleanup after interrupted Down = %v, want missing lane gate", err)
	}
	assertMessageThreadCount(
		ctx, t, st, fixture.accountID, fixture.realmID, parent.ThreadID, 2,
	)
}

type messageRetentionTestFixture struct {
	accountID   string
	realmID     string
	recipientID string
	principal   Principal
}

func newMessageRetentionTestFixture(
	ctx context.Context,
	t *testing.T,
	st *Store,
	suffix string,
) messageRetentionTestFixture {
	t.Helper()
	account, err := st.ProvisionAccount(
		ctx,
		"message-retention-"+suffix+"@witwave.ai",
		"message retention "+suffix,
		time.Hour,
	)
	if err != nil {
		t.Fatal(err)
	}
	if activated, err := st.ActivateAccount(ctx, account.AccountID); err != nil || !activated {
		t.Fatalf("activate = %v / %v", activated, err)
	}
	realm, err := st.CreateRealm(ctx, account.AccountID, "default")
	if err != nil {
		t.Fatal(err)
	}
	sender, err := st.CreateAgent(ctx, account.AccountID, realm.ID, "sender")
	if err != nil {
		t.Fatal(err)
	}
	recipient, err := st.CreateAgent(ctx, account.AccountID, realm.ID, "recipient")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.SetAccountPlan(
		ctx,
		account.AccountID,
		0,
		"",
		"test",
		map[string]int64{},
		map[string]int64{
			MessageRetentionDaysPolicy:              30,
			plans.MessagingEntitlementVersionPolicy: plans.MessagingEntitlementVersion,
		},
		[]string{plans.MessagingFeature},
	); err != nil {
		t.Fatal(err)
	}
	return messageRetentionTestFixture{
		accountID:   account.AccountID,
		realmID:     realm.ID,
		recipientID: recipient.ID,
		principal: Principal{
			Kind:          PrincipalAgent,
			ID:            sender.ID,
			AccountID:     account.AccountID,
			RealmID:       realm.ID,
			AgentName:     sender.Name,
			AccountStatus: "active",
		},
	}
}

func messageRetentionResultForAccountLane(
	ctx context.Context,
	t *testing.T,
	st *Store,
	enforce bool,
	batchSize int,
) MessageRetentionBatchResult {
	t.Helper()
	for range defaultMessageRetentionWorkerLaneCount {
		var (
			result MessageRetentionBatchResult
			err    error
		)
		if enforce {
			result, err = st.ProcessMessageRetentionBatch(ctx, batchSize)
		} else {
			result, err = st.PreviewMessageRetentionBatch(ctx, batchSize)
		}
		if err != nil {
			t.Fatal(err)
		}
		if result.Scanned > 0 {
			return result
		}
	}
	t.Fatal("message retention did not reach the account's worker lane")
	return MessageRetentionBatchResult{}
}

func assertMessageThreadCount(
	ctx context.Context,
	t *testing.T,
	st *Store,
	accountID, realmID, threadID string,
	want int64,
) {
	t.Helper()
	var got int64
	if err := st.pool.QueryRow(ctx, `
		SELECT count(*)
		  FROM agent_messages
		 WHERE account_id=$1 AND realm_id=$2 AND thread_id=$3`,
		accountID, realmID, threadID,
	).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("thread %s message count = %d, want %d", threadID, got, want)
	}
}
