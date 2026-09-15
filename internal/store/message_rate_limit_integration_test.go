package store

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/witwave-ai/witself/internal/plans"
	"github.com/witwave-ai/witself/internal/testenv"
)

func TestMessageRateLimitsAndUsagePostgres(t *testing.T) {
	baseDSN := testenv.RequirePostgres(t)
	ctx := context.Background()
	st, dsn := newMigrationTestStore(t, baseDSN)
	if err := st.Migrate(); err != nil {
		t.Fatal(err)
	}

	t.Run("sender boundary replay and usage", func(t *testing.T) {
		fixture := newMessageRateFixture(ctx, t, st, "sender", 3)
		fixture.setLimits(t, map[string]int64{
			plans.MessageSentPerAgentMinuteLimit:          2,
			plans.MessageDeliveredPerRealmMinuteLimit:     100,
			plans.MessageDeliveredPerRecipientMinuteLimit: 100,
		})
		first := fixture.send(t, 0, 1, "sender-1")
		fixture.send(t, 0, 2, "sender-2")
		_, err := st.SendMessage(ctx, fixture.principals[0], SendMessageInput{
			ToAgent: fixture.agents[1].ID, Body: "over sender limit",
			IdempotencyKey: "sender-3",
		})
		assertMessageRateError(t, err, MessageRateDimensionSent, MessageRateScopeAgent, 2, 1, MessageRateSourcePlan, true)

		replay, err := st.SendMessage(ctx, fixture.principals[0], SendMessageInput{
			ToAgent: fixture.agents[1].ID, Body: "message sender-1",
			IdempotencyKey: "sender-1",
		})
		if err != nil || replay.ID != first.ID {
			t.Fatalf("idempotent replay = %#v / %v", replay, err)
		}
		assertMessageUsage(ctx, t, st, fixture.accountID, 2, 2, 2, 2)
	})

	t.Run("fanout charges realm deliveries and refusal rolls back", func(t *testing.T) {
		fixture := newMessageRateFixture(ctx, t, st, "fanout", 3)
		fixture.setLimits(t, map[string]int64{
			plans.MessageSentPerAgentMinuteLimit:          100,
			plans.MessageDeliveredPerRealmMinuteLimit:     2,
			plans.MessageDeliveredPerRecipientMinuteLimit: 100,
		})
		fanout, err := st.SendMessage(ctx, fixture.principals[0], SendMessageInput{
			AudienceKind: MessageRecipientAgents,
			ToAgents:     []string{fixture.agents[1].ID, fixture.agents[2].ID},
			Body:         "two deliveries", IdempotencyKey: "fanout-1",
		})
		if err != nil || fanout.To.Count != 2 {
			t.Fatalf("fanout = %#v / %v", fanout, err)
		}
		beforeTAT := messageRateBucketTAT(ctx, t, st, fixture.accountID, fixture.realm.ID,
			MessageRateDimensionSent, MessageRateScopeAgent, fixture.agents[0].ID)
		_, err = st.SendMessage(ctx, fixture.principals[0], SendMessageInput{
			ToAgent: fixture.agents[1].ID, Body: "must roll back",
			IdempotencyKey: "fanout-rollback",
		})
		assertMessageRateError(t, err, MessageRateDimensionDelivered, MessageRateScopeRealm, 2, 1, MessageRateSourcePlan, true)
		afterTAT := messageRateBucketTAT(ctx, t, st, fixture.accountID, fixture.realm.ID,
			MessageRateDimensionSent, MessageRateScopeAgent, fixture.agents[0].ID)
		if afterTAT != beforeTAT {
			t.Fatalf("sender debit survived rejected fanout: before=%d after=%d", beforeTAT, afterTAT)
		}
		assertNoMessageByKey(ctx, t, st, fixture.accountID, "fanout-rollback")
		assertMessageUsage(ctx, t, st, fixture.accountID, 1, 2, 1, 1)
	})

	t.Run("zero sender limit is a hard refusal", func(t *testing.T) {
		fixture := newMessageRateFixture(ctx, t, st, "zero-limit", 2)
		fixture.setLimits(t, map[string]int64{
			plans.MessageSentPerAgentMinuteLimit:          0,
			plans.MessageDeliveredPerRealmMinuteLimit:     100,
			plans.MessageDeliveredPerRecipientMinuteLimit: 100,
		})
		_, err := st.SendMessage(ctx, fixture.principals[0], SendMessageInput{
			ToAgent: fixture.agents[1].ID, Body: "cannot fit zero limit",
			IdempotencyKey: "zero-limit-1",
		})
		assertMessageRateError(t, err, MessageRateDimensionSent, MessageRateScopeAgent,
			0, 1, MessageRateSourcePlan, false)
		assertNoMessageByKey(ctx, t, st, fixture.accountID, "zero-limit-1")
		assertMessageUsage(ctx, t, st, fixture.accountID, 0, 0, 0, 0)
	})

	t.Run("fanout larger than realm capacity is a hard refusal", func(t *testing.T) {
		fixture := newMessageRateFixture(ctx, t, st, "fanout-too-large", 3)
		fixture.setLimits(t, map[string]int64{
			plans.MessageSentPerAgentMinuteLimit:          100,
			plans.MessageDeliveredPerRealmMinuteLimit:     1,
			plans.MessageDeliveredPerRecipientMinuteLimit: 100,
		})
		_, err := st.SendMessage(ctx, fixture.principals[0], SendMessageInput{
			AudienceKind: MessageRecipientAgents,
			ToAgents:     []string{fixture.agents[1].ID, fixture.agents[2].ID},
			Body:         "fanout cannot fit realm capacity", IdempotencyKey: "fanout-too-large-1",
		})
		assertMessageRateError(t, err, MessageRateDimensionDelivered, MessageRateScopeRealm,
			1, 2, MessageRateSourcePlan, false)
		assertNoMessageByKey(ctx, t, st, fixture.accountID, "fanout-too-large-1")
		assertMessageUsage(ctx, t, st, fixture.accountID, 0, 0, 0, 0)
	})

	t.Run("recipient limit aggregates different senders", func(t *testing.T) {
		fixture := newMessageRateFixture(ctx, t, st, "recipient", 4)
		fixture.setLimits(t, map[string]int64{
			plans.MessageSentPerAgentMinuteLimit:          100,
			plans.MessageDeliveredPerRealmMinuteLimit:     100,
			plans.MessageDeliveredPerRecipientMinuteLimit: 2,
		})
		fixture.send(t, 0, 3, "recipient-1")
		fixture.send(t, 1, 3, "recipient-2")
		_, err := st.SendMessage(ctx, fixture.principals[2], SendMessageInput{
			ToAgent: fixture.agents[3].ID, Body: "recipient flood",
			IdempotencyKey: "recipient-3",
		})
		assertMessageRateError(t, err, MessageRateDimensionDelivered, MessageRateScopeRecipient, 2, 1, MessageRateSourcePlan, true)
		assertNoMessageByKey(ctx, t, st, fixture.accountID, "recipient-3")
	})

	t.Run("failed terminal delivery is rate debited but not metered delivered", func(t *testing.T) {
		fixture := newMessageRateFixture(ctx, t, st, "failed-delivery", 2)
		fixture.setLimits(t, map[string]int64{
			plans.MessageSentPerAgentMinuteLimit:          100,
			plans.MessageDeliveredPerRealmMinuteLimit:     100,
			plans.MessageDeliveredPerRecipientMinuteLimit: 100,
		})
		parent := fixture.send(t, 0, 1, "failed-parent")
		if err := st.DeleteAgent(ctx, fixture.accountID, fixture.realm.ID, fixture.agents[0].ID); err != nil {
			t.Fatal(err)
		}
		claim, err := st.ClaimMessage(ctx, fixture.principals[1], parent.ID, ClaimMessageInput{
			IdempotencyKey: "failed-claim",
		})
		if err != nil {
			t.Fatal(err)
		}
		completed, err := st.CompleteMessage(ctx, fixture.principals[1], parent.ID, CompleteMessageInput{
			ClaimID: claim.Processing.ClaimID, ProcessingGeneration: claim.Processing.Generation,
			IdempotencyKey: "failed-complete", Body: "terminal",
		})
		if err != nil || completed.ResultMessage.Delivery.State != MessageDeliveryFailed {
			t.Fatalf("failed completion = %#v / %v", completed, err)
		}
		var sentEvents, deliveredEvents int64
		if err := st.pool.QueryRow(ctx, `
			SELECT count(*) FILTER (WHERE dimension=$2),
			       count(*) FILTER (WHERE dimension=$3)
			  FROM usage_events
			 WHERE account_id=$1 AND subject_id=$4`,
			fixture.accountID, UsageDimensionMessageSent, UsageDimensionMessageDelivered,
			completed.ResultMessage.ID).Scan(&sentEvents, &deliveredEvents); err != nil {
			t.Fatal(err)
		}
		if sentEvents != 1 || deliveredEvents != 0 {
			t.Fatalf("failed completion usage sent/delivered = %d/%d, want 1/0", sentEvents, deliveredEvents)
		}
		assertMessageUsage(ctx, t, st, fixture.accountID, 2, 1, 2, 1)
		tx, err := st.pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = tx.Rollback(ctx) }()
		if err := validateImportedMessageUsageEvents(ctx, tx, fixture.accountID); err != nil {
			t.Fatalf("failed delivery's valid usage graph rejected: %v", err)
		}
	})

	t.Run("two replicas share one sender bucket", func(t *testing.T) {
		fixture := newMessageRateFixture(ctx, t, st, "concurrency", 3)
		fixture.setLimits(t, map[string]int64{
			plans.MessageSentPerAgentMinuteLimit:          1,
			plans.MessageDeliveredPerRealmMinuteLimit:     100,
			plans.MessageDeliveredPerRecipientMinuteLimit: 100,
		})
		replica, err := Open(ctx, dsn)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(replica.Close)

		start := make(chan struct{})
		var wg sync.WaitGroup
		errs := make([]error, 2)
		for index := range 2 {
			wg.Add(1)
			go func(index int) {
				defer wg.Done()
				<-start
				target := st
				if index == 1 {
					target = replica
				}
				_, errs[index] = target.SendMessage(ctx, fixture.principals[0], SendMessageInput{
					ToAgent:        fixture.agents[index+1].ID,
					Body:           fmt.Sprintf("race %d", index),
					IdempotencyKey: fmt.Sprintf("concurrent-%d", index),
				})
			}(index)
		}
		close(start)
		wg.Wait()
		successes, refusals := 0, 0
		for _, err := range errs {
			switch {
			case err == nil:
				successes++
			case errors.Is(err, ErrMessageRateLimited):
				refusals++
				assertMessageRateError(t, err, MessageRateDimensionSent, MessageRateScopeAgent, 1, 1, MessageRateSourcePlan, true)
			default:
				t.Fatalf("concurrent send error = %v", err)
			}
		}
		if successes != 1 || refusals != 1 {
			t.Fatalf("concurrent sends successes=%d refusals=%d", successes, refusals)
		}
		assertMessageUsage(ctx, t, st, fixture.accountID, 1, 1, 1, 1)
	})

	t.Run("bucket refills while a competing transaction holds its row lock", func(t *testing.T) {
		const limit int64 = 60
		intervalMicroseconds := messageRateIntervalMicroseconds(limit)
		debt := exerciseBlockedMessageRateDebit(ctx, t, st, "lock-refill-full",
			limit, limit*intervalMicroseconds,
			time.Duration(intervalMicroseconds)*time.Microsecond+250*time.Millisecond)
		if debt <= 0 || debt > time.Duration(limit*intervalMicroseconds)*time.Microsecond {
			t.Fatalf("admitted full-bucket debt = %s, want within bucket capacity", debt)
		}
	})

	t.Run("accepted debit uses post-lock clock after partial bucket expires", func(t *testing.T) {
		const limit int64 = 60
		intervalMicroseconds := messageRateIntervalMicroseconds(limit)
		interval := time.Duration(intervalMicroseconds) * time.Microsecond
		debt := exerciseBlockedMessageRateDebit(ctx, t, st, "lock-refill-partial",
			limit, intervalMicroseconds/4, interval+250*time.Millisecond)
		if debt != interval {
			t.Fatalf("accepted debit debt = %s, want one interval %s", debt, interval)
		}
	})

	t.Run("missing commercial key retains platform ceiling", func(t *testing.T) {
		fixture := newMessageRateFixture(ctx, t, st, "platform", 2)
		fixture.setLimits(t, map[string]int64{})
		_, err := st.pool.Exec(ctx, `
			INSERT INTO agent_message_rate_buckets
			  (account_id,realm_id,dimension,scope,scope_id,
			   theoretical_arrival_microseconds)
			VALUES ($1,$2,$3,$4,$5,
			        floor(extract(epoch FROM clock_timestamp() + interval '2 minutes') * 1000000)::bigint)`,
			fixture.accountID, fixture.realm.ID, MessageRateDimensionSent,
			MessageRateScopeAgent, fixture.agents[0].ID)
		if err != nil {
			t.Fatal(err)
		}
		_, err = st.SendMessage(ctx, fixture.principals[0], SendMessageInput{
			ToAgent: fixture.agents[1].ID, Body: "platform breaker",
			IdempotencyKey: "platform-breaker",
		})
		assertMessageRateError(t, err, MessageRateDimensionSent, MessageRateScopeAgent,
			plans.MaxMessageSentPerAgentMinute, 1, MessageRateSourcePlatform, true)
		assertNoMessageByKey(ctx, t, st, fixture.accountID, "platform-breaker")
	})

	t.Run("stale bucket cleanup is bounded and worker safe", func(t *testing.T) {
		fixture := newMessageRateFixture(ctx, t, st, "cleanup", 2)
		fixture.setLimits(t, map[string]int64{})
		fixture.send(t, 0, 1, "cleanup-1")
		if _, err := st.pool.Exec(ctx, `
			UPDATE agent_message_rate_buckets
			   SET updated_at=clock_timestamp() - interval '2 minutes',
			       theoretical_arrival_microseconds =
			         floor(extract(epoch FROM clock_timestamp() - interval '1 minute') * 1000000)::bigint
			 WHERE account_id=$1`, fixture.accountID); err != nil {
			t.Fatal(err)
		}
		deleted, err := st.DeleteStaleMessageRateBuckets(ctx, time.Now().UTC(), 2)
		if err != nil || deleted != 2 {
			t.Fatalf("first cleanup deleted = %d / %v, want 2", deleted, err)
		}
		deleted, err = st.DeleteStaleMessageRateBuckets(ctx, time.Now().UTC(), 2)
		if err != nil || deleted != 1 {
			t.Fatalf("second cleanup deleted = %d / %v, want 1", deleted, err)
		}
		deleted, err = st.DeleteStaleMessageRateBuckets(ctx, time.Now().UTC(), 2)
		if err != nil || deleted != 0 {
			t.Fatalf("empty cleanup deleted = %d / %v, want 0", deleted, err)
		}
	})

	t.Run("portable message usage is bound to its message graph", func(t *testing.T) {
		fixture := newMessageRateFixture(ctx, t, st, "usage-archive", 3)
		fixture.setLimits(t, map[string]int64{})
		if _, err := st.SendMessage(ctx, fixture.principals[0], SendMessageInput{
			AudienceKind:   MessageRecipientAgents,
			ToAgents:       []string{fixture.agents[1].ID, fixture.agents[2].ID},
			Body:           "portable usage",
			IdempotencyKey: "usage-archive-message",
		}); err != nil {
			t.Fatal(err)
		}

		tx, err := st.pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = tx.Rollback(ctx) }()
		if err := validateImportedMessageUsageEvents(ctx, tx, fixture.accountID); err != nil {
			t.Fatalf("valid message usage rejected: %v", err)
		}

		for _, test := range []struct {
			name   string
			update string
			args   []any
		}{
			{"sent unit", `UPDATE usage_events SET unit='delivery' WHERE account_id=$1 AND dimension='message_sent'`, nil},
			{"sent quantity", `UPDATE usage_events SET quantity=2 WHERE account_id=$1 AND dimension='message_sent'`, nil},
			{"delivered unit", `UPDATE usage_events SET unit='message' WHERE account_id=$1 AND dimension='message_delivered'`, nil},
			{"delivered quantity", `UPDATE usage_events SET quantity=1 WHERE account_id=$1 AND dimension='message_delivered'`, nil},
			{"subject type", `UPDATE usage_events SET subject_type='transcript' WHERE account_id=$1 AND dimension='message_sent'`, nil},
			{"subject id", `UPDATE usage_events SET subject_id='msg_not-valid' WHERE account_id=$1 AND dimension='message_sent'`, nil},
			{"sender", `UPDATE usage_events SET agent_id=$2 WHERE account_id=$1 AND dimension='message_sent'`, []any{fixture.agents[1].ID}},
			{"occurred at", `UPDATE usage_events SET occurred_at=occurred_at + interval '1 second' WHERE account_id=$1 AND dimension='message_sent'`, nil},
			{"metadata", `UPDATE usage_events SET metadata='{"tampered":true}'::jsonb WHERE account_id=$1 AND dimension='message_sent'`, nil},
			{"idempotency key", `UPDATE usage_events SET idempotency_key='tampered' WHERE account_id=$1 AND dimension='message_sent'`, nil},
			{"delivered without sent pair", `DELETE FROM usage_events WHERE account_id=$1 AND dimension='message_sent'`, nil},
		} {
			t.Run(test.name, func(t *testing.T) {
				if _, err := tx.Exec(ctx, `SAVEPOINT message_usage_tamper`); err != nil {
					t.Fatal(err)
				}
				args := []any{fixture.accountID}
				args = append(args, test.args...)
				if _, err := tx.Exec(ctx, test.update, args...); err != nil {
					t.Fatal(err)
				}
				err := validateImportedMessageUsageEvents(ctx, tx, fixture.accountID)
				if !errors.Is(err, ErrArchiveContent) {
					t.Fatalf("tampered message usage error = %v, want ErrArchiveContent", err)
				}
				if _, err := tx.Exec(ctx, `ROLLBACK TO SAVEPOINT message_usage_tamper`); err != nil {
					t.Fatal(err)
				}
				if _, err := tx.Exec(ctx, `RELEASE SAVEPOINT message_usage_tamper`); err != nil {
					t.Fatal(err)
				}
			})
		}
	})

	t.Run("retained usage survives message retention archive round trip", func(t *testing.T) {
		fixture := newMessageRateFixture(ctx, t, st, "usage-retained", 2)
		fixture.setLimits(t, map[string]int64{})
		message := fixture.send(t, 0, 1, "usage-retained-message")
		if _, err := st.pool.Exec(ctx, `DELETE FROM agent_messages WHERE id=$1`, message.ID); err != nil {
			t.Fatal(err)
		}
		var usageBefore int64
		if err := st.pool.QueryRow(ctx, `
			SELECT count(*) FROM usage_events
			 WHERE account_id=$1 AND subject_id=$2
			   AND dimension IN ('message_sent','message_delivered')`,
			fixture.accountID, message.ID).Scan(&usageBefore); err != nil {
			t.Fatal(err)
		}
		if usageBefore != 2 {
			t.Fatalf("retained message usage events = %d, want 2", usageBefore)
		}
		if err := st.SuspendAccountSystem(ctx, fixture.accountID, "evacuation", "retained message usage archive"); err != nil {
			t.Fatal(err)
		}
		var archive bytes.Buffer
		if err := st.ExportAccount(ctx, fixture.accountID, "test-source", "test", &archive); err != nil {
			t.Fatal(err)
		}
		if err := deleteAccountForIntegrationTest(ctx, st, fixture.accountID); err != nil {
			t.Fatal(err)
		}
		if _, err := st.ImportAccount(ctx, fixture.accountID, bytes.NewReader(archive.Bytes())); err != nil {
			t.Fatalf("import retained message usage: %v", err)
		}
		var messagesAfter, usageAfter int64
		if err := st.pool.QueryRow(ctx, `
			SELECT
			  (SELECT count(*) FROM agent_messages WHERE account_id=$1 AND id=$2),
			  (SELECT count(*) FROM usage_events WHERE account_id=$1 AND subject_id=$2
			    AND dimension IN ('message_sent','message_delivered'))`,
			fixture.accountID, message.ID).Scan(&messagesAfter, &usageAfter); err != nil {
			t.Fatal(err)
		}
		if messagesAfter != 0 || usageAfter != 2 {
			t.Fatalf("restored retained usage message/events = %d/%d, want 0/2", messagesAfter, usageAfter)
		}
	})
}

func TestMessageRateMigrationIsolatedFromNewerPublicSchemaPostgres(t *testing.T) {
	baseDSN := testenv.RequirePostgres(t)
	ctx := context.Background()
	publicStore, err := Open(ctx, baseDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer publicStore.Close()
	if err := publicStore.Migrate(); err != nil {
		t.Fatalf("migrate public schema: %v", err)
	}

	isolatedStore, _ := newMigrationTestStore(t, baseDSN)
	if err := isolatedStore.Migrate(); err != nil {
		t.Fatalf("migrate isolated schema after public schema 83: %v", err)
	}
	var tablePresent bool
	if err := isolatedStore.pool.QueryRow(ctx, `
		SELECT to_regclass(current_schema() || '.agent_message_rate_buckets') IS NOT NULL`,
	).Scan(&tablePresent); err != nil {
		t.Fatal(err)
	}
	if !tablePresent {
		t.Fatal("isolated schema migration omitted agent_message_rate_buckets")
	}
}

func exerciseBlockedMessageRateDebit(
	ctx context.Context,
	t *testing.T,
	st *Store,
	suffix string,
	limit, initialDebtMicroseconds int64,
	hold time.Duration,
) time.Duration {
	t.Helper()
	fixture := newMessageRateFixture(ctx, t, st, suffix, 2)
	debit := messageRateDebit{
		dimension: MessageRateDimensionSent, scope: MessageRateScopeAgent,
		scopeID: fixture.agents[0].ID, quantity: 1,
	}
	if _, err := st.pool.Exec(ctx, `
		INSERT INTO agent_message_rate_buckets
		  (account_id,realm_id,dimension,scope,scope_id,theoretical_arrival_microseconds)
		VALUES ($1,$2,$3,$4,$5,
		        floor(extract(epoch FROM clock_timestamp()) * 1000000)::bigint)`,
		fixture.accountID, fixture.realm.ID, debit.dimension, debit.scope, debit.scopeID,
	); err != nil {
		t.Fatal(err)
	}

	lockCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	locker, err := st.pool.Begin(lockCtx)
	if err != nil {
		t.Fatal(err)
	}
	waiter, err := st.pool.Begin(lockCtx)
	if err != nil {
		_ = locker.Rollback(lockCtx)
		t.Fatal(err)
	}
	applicationName := "message-rate-lock-" + fixture.accountID
	if _, err := waiter.Exec(lockCtx, `SELECT set_config('application_name',$1,true)`, applicationName); err != nil {
		_ = waiter.Rollback(lockCtx)
		_ = locker.Rollback(lockCtx)
		t.Fatal(err)
	}
	if _, err := locker.Exec(lockCtx, `
		UPDATE agent_message_rate_buckets
		   SET theoretical_arrival_microseconds =
		         floor(extract(epoch FROM clock_timestamp()) * 1000000)::bigint + $6::bigint,
		       updated_at=clock_timestamp()
		 WHERE account_id=$1 AND realm_id=$2 AND dimension=$3 AND scope=$4 AND scope_id=$5`,
		fixture.accountID, fixture.realm.ID, debit.dimension, debit.scope, debit.scopeID,
		initialDebtMicroseconds,
	); err != nil {
		_ = waiter.Rollback(lockCtx)
		_ = locker.Rollback(lockCtx)
		t.Fatal(err)
	}

	type consumeResult struct {
		debt time.Duration
		err  error
	}
	done := make(chan consumeResult, 1)
	go func() {
		decision, err := consumeMessageRateBucketTx(lockCtx, waiter,
			fixture.accountID, fixture.realm.ID, debit, limit)
		if err == nil && !decision.admitted {
			err = fmt.Errorf("rate decision refused debit: %#v", decision)
		}
		if err != nil {
			_ = waiter.Rollback(lockCtx)
			done <- consumeResult{err: err}
			return
		}
		err = waiter.Commit(lockCtx)
		done <- consumeResult{
			debt: time.Duration(decision.currentTAT-decision.nowMicroseconds) * time.Microsecond,
			err:  err,
		}
	}()

	waiting := false
	var waitErr error
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		waitErr = st.pool.QueryRow(lockCtx, `
			SELECT EXISTS (
			  SELECT 1 FROM pg_stat_activity
			   WHERE application_name=$1 AND wait_event_type='Lock'
			)`, applicationName).Scan(&waiting)
		if waitErr != nil || waiting {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if waitErr != nil || !waiting {
		_ = locker.Rollback(lockCtx)
		result := <-done
		t.Fatalf("waiter did not block on bucket row: waiting=%t query_err=%v consume_err=%v",
			waiting, waitErr, result.err)
	}

	time.Sleep(hold)
	commitErr := locker.Commit(lockCtx)
	result := <-done
	if commitErr != nil {
		t.Fatalf("release bucket row lock: %v", commitErr)
	}
	if result.err != nil {
		t.Fatalf("debit after bucket row-lock wait = %v, want admitted", result.err)
	}
	return result.debt
}

type messageRateFixture struct {
	ctx        context.Context
	st         *Store
	accountID  string
	realm      Realm
	agents     []Agent
	principals []Principal
	revision   int64
}

func newMessageRateFixture(ctx context.Context, t *testing.T, st *Store, suffix string, agentCount int) *messageRateFixture {
	t.Helper()
	accountID := provisionActiveResourceLimitAccount(ctx, t, st, "message-rate-"+suffix)
	realm, err := st.CreateRealm(ctx, accountID, "message-rate-"+suffix)
	if err != nil {
		t.Fatal(err)
	}
	fixture := &messageRateFixture{ctx: ctx, st: st, accountID: accountID, realm: realm}
	for index := range agentCount {
		agent, err := st.CreateAgent(ctx, accountID, realm.ID, fmt.Sprintf("agent-%d", index))
		if err != nil {
			t.Fatal(err)
		}
		fixture.agents = append(fixture.agents, agent)
		fixture.principals = append(fixture.principals, Principal{
			Kind: PrincipalAgent, ID: agent.ID, AccountID: accountID,
			RealmID: realm.ID, AgentName: agent.Name, AccountStatus: "active",
		})
	}
	return fixture
}

func (f *messageRateFixture) setLimits(t *testing.T, limits map[string]int64) {
	t.Helper()
	f.revision++
	setResourceLimitPlan(f.ctx, t, f.st, f.accountID, f.revision, limits)
}

func (f *messageRateFixture) send(t *testing.T, from, to int, key string) Message {
	t.Helper()
	message, err := f.st.SendMessage(f.ctx, f.principals[from], SendMessageInput{
		ToAgent: f.agents[to].ID, Body: "message " + key, IdempotencyKey: key,
	})
	if err != nil {
		t.Fatal(err)
	}
	return message
}

func assertMessageRateError(
	t *testing.T,
	err error,
	dimension, scope string,
	limit, attempted int64,
	source string,
	retryable bool,
) {
	t.Helper()
	if !errors.Is(err, ErrMessageRateLimited) {
		t.Fatalf("rate error = %v, want ErrMessageRateLimited", err)
	}
	var detail *MessageRateLimitError
	if !errors.As(err, &detail) {
		t.Fatalf("rate error detail = %T, want *MessageRateLimitError", err)
	}
	if detail.Dimension != dimension || detail.Scope != scope || detail.Limit != limit ||
		detail.Attempted != attempted || detail.WindowSeconds != 60 ||
		detail.Source != source || detail.Retryable != retryable {
		t.Fatalf("rate error detail = %#v", detail)
	}
	if retryable && (detail.RetryAfter <= 0 || detail.ResetAt.IsZero()) {
		t.Fatalf("retryable rate error lacks retry timing: %#v", detail)
	}
	if !retryable && (detail.RetryAfter != 0 || !detail.ResetAt.IsZero()) {
		t.Fatalf("hard rate error carries retry timing: %#v", detail)
	}
}

func assertMessageUsage(
	ctx context.Context,
	t *testing.T,
	st *Store,
	accountID string,
	sentQuantity, deliveredQuantity, sentEvents, deliveredEvents int64,
) {
	t.Helper()
	for _, expected := range []struct {
		dimension string
		quantity  int64
		events    int64
	}{
		{UsageDimensionMessageSent, sentQuantity, sentEvents},
		{UsageDimensionMessageDelivered, deliveredQuantity, deliveredEvents},
	} {
		var quantity, events int64
		if err := st.pool.QueryRow(ctx, `
			SELECT COALESCE(sum(quantity),0), count(*)
			  FROM usage_events
			 WHERE account_id=$1 AND dimension=$2`, accountID, expected.dimension).
			Scan(&quantity, &events); err != nil {
			t.Fatal(err)
		}
		if quantity != expected.quantity || events != expected.events {
			t.Fatalf("%s usage events quantity/events = %d/%d, want %d/%d",
				expected.dimension, quantity, events, expected.quantity, expected.events)
		}
		for _, bucket := range []string{UsageBucketHour, UsageBucketDay} {
			var rollupQuantity, rollupEvents int64
			if err := st.pool.QueryRow(ctx, `
				SELECT COALESCE(sum(quantity),0), COALESCE(sum(event_count),0)
				  FROM usage_rollups
				 WHERE account_id=$1 AND dimension=$2 AND bucket=$3`,
				accountID, expected.dimension, bucket).Scan(&rollupQuantity, &rollupEvents); err != nil {
				t.Fatal(err)
			}
			if rollupQuantity != expected.quantity || rollupEvents != expected.events {
				t.Fatalf("%s %s rollup quantity/events = %d/%d, want %d/%d",
					expected.dimension, bucket, rollupQuantity, rollupEvents,
					expected.quantity, expected.events)
			}
		}
	}
}

func messageRateBucketTAT(
	ctx context.Context,
	t *testing.T,
	st *Store,
	accountID, realmID, dimension, scope, scopeID string,
) int64 {
	t.Helper()
	var tat int64
	if err := st.pool.QueryRow(ctx, `
		SELECT theoretical_arrival_microseconds
		  FROM agent_message_rate_buckets
		 WHERE account_id=$1 AND realm_id=$2 AND dimension=$3 AND scope=$4 AND scope_id=$5`,
		accountID, realmID, dimension, scope, scopeID).Scan(&tat); err != nil {
		t.Fatal(err)
	}
	return tat
}

func assertNoMessageByKey(ctx context.Context, t *testing.T, st *Store, accountID, key string) {
	t.Helper()
	var count int64
	if err := st.pool.QueryRow(ctx, `
		SELECT count(*) FROM agent_messages WHERE account_id=$1 AND idempotency_key=$2`,
		accountID, key).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("rejected key %q persisted %d messages", key, count)
	}
}
