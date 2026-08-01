package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/witwave-ai/witself/internal/agentemail"
	"github.com/witwave-ai/witself/internal/plans"
)

func TestAgentEmailRateLimitsPostgres(t *testing.T) {
	baseDSN := os.Getenv("WITSELF_TEST_DATABASE_URL")
	if baseDSN == "" {
		t.Skip("WITSELF_TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	st, dsn := newMigrationTestStore(t, baseDSN)
	if err := st.Migrate(); err != nil {
		t.Fatal(err)
	}

	t.Run("count refusal isolates unverified senders", func(t *testing.T) {
		fixture := newAgentEmailRateFixture(ctx, t, st, "source", 1, 1)
		limits := agentEmailMaximumRateLimits()
		limits[plans.AgentEmailReceivedPerSenderMinuteLimit] = 1
		setAgentEmailRatePlan(ctx, t, st, fixture.accountID, 1, limits)

		realm := fixture.realms[0]
		rawFirst := agentEmailRateRaw(realm.addresses[0].Address, "source-a")
		if _, err := ingestAgentEmailRate(
			ctx, st, realm, 0, "sender-a@example.com", rawFirst,
		); err != nil {
			t.Fatal(err)
		}

		before := countAgentEmailMessages(ctx, t, st, fixture.accountID)
		rawRefused := agentEmailRateRaw(realm.addresses[0].Address, "source-b")
		_, err := ingestAgentEmailRate(
			ctx, st, realm, 0, "sender-a@example.com", rawRefused,
		)
		assertAgentEmailRateError(
			t, err,
			AgentEmailRateDimensionReceived,
			AgentEmailRateScopeSender,
			1,
			1,
			AgentEmailRateSourcePlan,
			true,
		)
		if after := countAgentEmailMessages(ctx, t, st, fixture.accountID); after != before {
			t.Fatalf("rate refusal stored email: before=%d after=%d", before, after)
		}

		// The source bucket is scoped to the unverified sender/recipient pair.
		// Exhausting one sender must not consume another sender's bucket.
		rawOther := agentEmailRateRaw(realm.addresses[0].Address, "source-c")
		if _, err := ingestAgentEmailRate(
			ctx, st, realm, 0, "sender-b@example.com", rawOther,
		); err != nil {
			t.Fatalf("independent sender was refused: %v", err)
		}
		if got := countAgentEmailMessages(ctx, t, st, fixture.accountID); got != 2 {
			t.Fatalf("stored messages = %d, want 2", got)
		}
	})

	t.Run("weighted byte refusal isolates recipients", func(t *testing.T) {
		fixture := newAgentEmailRateFixture(ctx, t, st, "recipient", 1, 2)
		realm := fixture.realms[0]
		firstRaw := agentEmailRateRaw(realm.addresses[0].Address, "weight-a")
		secondRaw := agentEmailRateRaw(realm.addresses[0].Address, "weight-b")
		if len(firstRaw) != len(secondRaw) {
			t.Fatalf("weighted fixtures differ in size: %d != %d", len(firstRaw), len(secondRaw))
		}

		limits := agentEmailMaximumRateLimits()
		limits[plans.AgentEmailReceivedBytesPerRecipientMinuteLimit] = int64(len(firstRaw))
		setAgentEmailRatePlan(ctx, t, st, fixture.accountID, 1, limits)

		if _, err := ingestAgentEmailRate(
			ctx, st, realm, 0, "sender-a@example.com", firstRaw,
		); err != nil {
			t.Fatal(err)
		}
		before := countAgentEmailMessages(ctx, t, st, fixture.accountID)
		_, err := ingestAgentEmailRate(
			ctx, st, realm, 0, "sender-b@example.com", secondRaw,
		)
		assertAgentEmailRateError(
			t, err,
			AgentEmailRateDimensionReceivedBytes,
			AgentEmailRateScopeRecipient,
			int64(len(firstRaw)),
			int64(len(secondRaw)),
			AgentEmailRateSourcePlan,
			true,
		)
		if after := countAgentEmailMessages(ctx, t, st, fixture.accountID); after != before {
			t.Fatalf("weighted refusal stored email: before=%d after=%d", before, after)
		}

		otherRaw := agentEmailRateRaw(realm.addresses[1].Address, "weight-c")
		if _, err := ingestAgentEmailRate(
			ctx, st, realm, 1, "sender-a@example.com", otherRaw,
		); err != nil {
			t.Fatalf("independent recipient was refused: %v", err)
		}
		if got := countAgentEmailMessages(ctx, t, st, fixture.accountID); got != 2 {
			t.Fatalf("stored messages = %d, want 2", got)
		}
	})

	t.Run("realm breaker is isolated between realms", func(t *testing.T) {
		fixture := newAgentEmailRateFixture(ctx, t, st, "realm", 2, 1)
		limits := agentEmailMaximumRateLimits()
		limits[plans.AgentEmailReceivedPerRealmMinuteLimit] = 1
		setAgentEmailRatePlan(ctx, t, st, fixture.accountID, 1, limits)

		firstRealm := fixture.realms[0]
		firstRaw := agentEmailRateRaw(firstRealm.addresses[0].Address, "realm-a")
		if _, err := ingestAgentEmailRate(
			ctx, st, firstRealm, 0, "sender@example.com", firstRaw,
		); err != nil {
			t.Fatal(err)
		}
		_, err := ingestAgentEmailRate(
			ctx,
			st,
			firstRealm,
			0,
			"other@example.com",
			agentEmailRateRaw(firstRealm.addresses[0].Address, "realm-b"),
		)
		assertAgentEmailRateError(
			t, err,
			AgentEmailRateDimensionReceived,
			AgentEmailRateScopeRealm,
			1,
			1,
			AgentEmailRateSourcePlan,
			true,
		)

		secondRealm := fixture.realms[1]
		if _, err := ingestAgentEmailRate(
			ctx,
			st,
			secondRealm,
			0,
			"sender@example.com",
			agentEmailRateRaw(secondRealm.addresses[0].Address, "realm-c"),
		); err != nil {
			t.Fatalf("independent realm was refused: %v", err)
		}
		if got := countAgentEmailMessages(ctx, t, st, fixture.accountID); got != 2 {
			t.Fatalf("stored messages = %d, want 2", got)
		}
	})

	t.Run("zero late cap rolls back earlier debits", func(t *testing.T) {
		fixture := newAgentEmailRateFixture(ctx, t, st, "rollback", 1, 1)
		limits := agentEmailMaximumRateLimits()
		// Sender bytes is the sixth and final debit. Its refusal must roll the
		// preceding realm, recipient, and sender-count debits back atomically.
		limits[plans.AgentEmailReceivedBytesPerSenderMinuteLimit] = 0
		setAgentEmailRatePlan(ctx, t, st, fixture.accountID, 1, limits)

		realm := fixture.realms[0]
		raw := agentEmailRateRaw(realm.addresses[0].Address, "rollback-a")
		_, err := ingestAgentEmailRate(
			ctx, st, realm, 0, "sender@example.com", raw,
		)
		assertAgentEmailRateError(
			t, err,
			AgentEmailRateDimensionReceivedBytes,
			AgentEmailRateScopeSender,
			0,
			int64(len(raw)),
			AgentEmailRateSourcePlan,
			false,
		)
		if got := countAgentEmailMessages(ctx, t, st, fixture.accountID); got != 0 {
			t.Fatalf("zero-cap refusal stored %d emails", got)
		}
		if got := countAgentEmailRateBuckets(ctx, t, st, fixture.accountID); got != 0 {
			t.Fatalf("earlier debits survived rolled-back refusal: %d buckets", got)
		}

		limits[plans.AgentEmailReceivedBytesPerSenderMinuteLimit] =
			plans.MaxAgentEmailReceivedBytesPerSenderMinute
		setAgentEmailRatePlan(ctx, t, st, fixture.accountID, 2, limits)
		if _, err := ingestAgentEmailRate(
			ctx,
			st,
			realm,
			0,
			"sender@example.com",
			agentEmailRateRaw(realm.addresses[0].Address, "rollback-b"),
		); err != nil {
			t.Fatalf("ingest after removing zero cap failed: %v", err)
		}
		if got := countAgentEmailRateBuckets(ctx, t, st, fixture.accountID); got != 6 {
			t.Fatalf("accepted ingest rate buckets = %d, want 6", got)
		}
	})

	t.Run("missing commercial keys retain platform breaker", func(t *testing.T) {
		fixture := newAgentEmailRateFixture(ctx, t, st, "platform", 1, 1)
		setAgentEmailRatePlan(ctx, t, st, fixture.accountID, 1, map[string]int64{})
		realm := fixture.realms[0]
		const sender = "sender@example.com"
		senderScopeID := agentEmailSenderScopeID(sender, realm.addresses[0].Address)
		if _, err := st.pool.Exec(ctx, `
			INSERT INTO agent_email_rate_buckets
			  (account_id,realm_id,dimension,scope,scope_id,
			   theoretical_arrival_nanoseconds)
			VALUES ($1,$2,$3,$4,$5,
			        floor(extract(epoch FROM clock_timestamp() + interval '2 minutes') * 1000000000)::bigint)`,
			fixture.accountID,
			realm.realm.ID,
			AgentEmailRateDimensionReceived,
			AgentEmailRateScopeSender,
			senderScopeID,
		); err != nil {
			t.Fatal(err)
		}

		raw := agentEmailRateRaw(realm.addresses[0].Address, "platform-a")
		_, err := ingestAgentEmailRate(ctx, st, realm, 0, sender, raw)
		assertAgentEmailRateError(
			t, err,
			AgentEmailRateDimensionReceived,
			AgentEmailRateScopeSender,
			plans.MaxAgentEmailReceivedPerSenderMinute,
			1,
			AgentEmailRateSourcePlatform,
			true,
		)
		if got := countAgentEmailMessages(ctx, t, st, fixture.accountID); got != 0 {
			t.Fatalf("platform refusal stored %d emails", got)
		}
		// The four aggregate debits that precede the sender refusal must not
		// survive; only the deliberately seeded sender bucket remains.
		if got := countAgentEmailRateBuckets(ctx, t, st, fixture.accountID); got != 1 {
			t.Fatalf("platform refusal left %d buckets, want seeded bucket only", got)
		}
	})

	t.Run("two stores share one sender bucket", func(t *testing.T) {
		fixture := newAgentEmailRateFixture(ctx, t, st, "concurrency", 1, 1)
		limits := agentEmailMaximumRateLimits()
		limits[plans.AgentEmailReceivedPerSenderMinuteLimit] = 1
		setAgentEmailRatePlan(ctx, t, st, fixture.accountID, 1, limits)

		replica, err := Open(ctx, dsn)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(replica.Close)

		runCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
		defer cancel()
		start := make(chan struct{})
		results := make(chan error, 2)
		realm := fixture.realms[0]
		for index, target := range []*Store{st, replica} {
			index, target := index, target
			go func() {
				<-start
				_, ingestErr := ingestAgentEmailRate(
					runCtx,
					target,
					realm,
					0,
					"sender@example.com",
					agentEmailRateRaw(
						realm.addresses[0].Address,
						fmt.Sprintf("race-%d", index),
					),
				)
				results <- ingestErr
			}()
		}
		close(start)

		successes, refusals := 0, 0
		for range 2 {
			err := <-results
			switch {
			case err == nil:
				successes++
			case errors.Is(err, ErrAgentEmailRateLimited):
				refusals++
				assertAgentEmailRateError(
					t,
					err,
					AgentEmailRateDimensionReceived,
					AgentEmailRateScopeSender,
					1,
					1,
					AgentEmailRateSourcePlan,
					true,
				)
			default:
				t.Fatalf("concurrent ingest error = %v", err)
			}
		}
		if successes != 1 || refusals != 1 {
			t.Fatalf("concurrent ingests successes=%d refusals=%d, want 1/1",
				successes, refusals)
		}
		if got := countAgentEmailMessages(ctx, t, st, fixture.accountID); got != 1 {
			t.Fatalf("concurrent ingests stored %d emails, want 1", got)
		}
	})

	t.Run("stale cleanup is bounded and preserves live debt", func(t *testing.T) {
		fixture := newAgentEmailRateFixture(ctx, t, st, "cleanup", 1, 1)
		setAgentEmailRatePlan(
			ctx, t, st, fixture.accountID, 1, agentEmailMaximumRateLimits(),
		)
		realm := fixture.realms[0]
		if _, err := ingestAgentEmailRate(
			ctx,
			st,
			realm,
			0,
			"sender@example.com",
			agentEmailRateRaw(realm.addresses[0].Address, "cleanup-a"),
		); err != nil {
			t.Fatal(err)
		}
		if got := countAgentEmailRateBuckets(ctx, t, st, fixture.accountID); got != 6 {
			t.Fatalf("accepted ingest rate buckets = %d, want 6", got)
		}
		if _, err := st.pool.Exec(ctx, `
			UPDATE agent_email_rate_buckets
			   SET updated_at=clock_timestamp() - interval '2 minutes',
			       theoretical_arrival_nanoseconds=
			         floor(extract(epoch FROM clock_timestamp() - interval '1 minute') * 1000000000)::bigint
			 WHERE account_id=$1`, fixture.accountID); err != nil {
			t.Fatal(err)
		}
		if _, err := st.pool.Exec(ctx, `
			UPDATE agent_email_rate_buckets
			   SET theoretical_arrival_nanoseconds=
			         floor(extract(epoch FROM clock_timestamp() + interval '1 minute') * 1000000000)::bigint
			 WHERE ctid IN (
			   SELECT ctid FROM agent_email_rate_buckets
			    WHERE account_id=$1
			    ORDER BY dimension,scope,scope_id
			    LIMIT 1
			 )`, fixture.accountID); err != nil {
			t.Fatal(err)
		}

		if _, err := st.DeleteStaleAgentEmailRateBuckets(ctx, time.Time{}, 1); err == nil {
			t.Fatal("cleanup accepted a zero cutoff")
		}
		if _, err := st.DeleteStaleAgentEmailRateBuckets(ctx, time.Now().UTC(), 0); err == nil {
			t.Fatal("cleanup accepted a zero batch limit")
		}

		for index, want := range []int64{2, 2, 1, 0} {
			deleted, err := st.DeleteStaleAgentEmailRateBuckets(
				ctx, time.Now().UTC(), 2,
			)
			if err != nil || deleted != want {
				t.Fatalf("cleanup pass %d deleted=%d / %v, want %d",
					index+1, deleted, err, want)
			}
		}
		if got := countAgentEmailRateBuckets(ctx, t, st, fixture.accountID); got != 1 {
			t.Fatalf("cleanup removed live debt or left stale rows: %d buckets", got)
		}

		if _, err := st.pool.Exec(ctx, `
			UPDATE agent_email_rate_buckets
			   SET updated_at=clock_timestamp() - interval '2 minutes',
			       theoretical_arrival_nanoseconds=
			         floor(extract(epoch FROM clock_timestamp() - interval '1 minute') * 1000000000)::bigint
			 WHERE account_id=$1`, fixture.accountID); err != nil {
			t.Fatal(err)
		}
		deleted, err := st.DeleteStaleAgentEmailRateBuckets(
			ctx, time.Now().UTC(), 1,
		)
		if err != nil || deleted != 1 {
			t.Fatalf("expired live-debt cleanup deleted=%d / %v, want 1", deleted, err)
		}
		if got := countAgentEmailRateBuckets(ctx, t, st, fixture.accountID); got != 0 {
			t.Fatalf("final cleanup left %d buckets", got)
		}
	})

	t.Run("two stores cooperatively clean stale buckets", func(t *testing.T) {
		fixture := newAgentEmailRateFixture(ctx, t, st, "cleanup-concurrent", 1, 1)
		setAgentEmailRatePlan(
			ctx, t, st, fixture.accountID, 1, agentEmailMaximumRateLimits(),
		)
		realm := fixture.realms[0]
		if _, err := ingestAgentEmailRate(
			ctx,
			st,
			realm,
			0,
			"sender@example.com",
			agentEmailRateRaw(realm.addresses[0].Address, "cleanup-concurrent"),
		); err != nil {
			t.Fatal(err)
		}
		if _, err := st.pool.Exec(ctx, `
			UPDATE agent_email_rate_buckets
			   SET updated_at=clock_timestamp() - interval '2 minutes',
			       theoretical_arrival_nanoseconds=
			         floor(extract(epoch FROM clock_timestamp() - interval '1 minute') * 1000000000)::bigint
			 WHERE account_id=$1`, fixture.accountID); err != nil {
			t.Fatal(err)
		}

		replica, err := Open(ctx, dsn)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(replica.Close)
		runCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
		defer cancel()
		start := make(chan struct{})
		type cleanupResult struct {
			deleted int64
			err     error
		}
		results := make(chan cleanupResult, 2)
		for _, target := range []*Store{st, replica} {
			target := target
			go func() {
				<-start
				deleted, cleanupErr := target.DeleteStaleAgentEmailRateBuckets(
					runCtx, time.Now().UTC(), 4,
				)
				results <- cleanupResult{deleted: deleted, err: cleanupErr}
			}()
		}
		close(start)

		var total int64
		for range 2 {
			result := <-results
			if result.err != nil {
				t.Fatalf("concurrent cleanup: %v", result.err)
			}
			if result.deleted < 0 || result.deleted > 4 {
				t.Fatalf("concurrent cleanup deleted %d rows, want 0-4", result.deleted)
			}
			total += result.deleted
		}
		if total != 6 {
			t.Fatalf("concurrent cleanup deleted %d rows, want 6", total)
		}
		if got := countAgentEmailRateBuckets(ctx, t, st, fixture.accountID); got != 0 {
			t.Fatalf("concurrent cleanup left %d stale buckets", got)
		}
	})
}

type agentEmailRateFixture struct {
	accountID string
	realms    []agentEmailRateRealmFixture
}

type agentEmailRateRealmFixture struct {
	realm     Realm
	scope     AgentEmailPilotScope
	addresses []AgentEmailAddress
}

func newAgentEmailRateFixture(
	ctx context.Context,
	t *testing.T,
	st *Store,
	suffix string,
	realmCount, mailboxCount int,
) agentEmailRateFixture {
	t.Helper()
	if realmCount < 1 || mailboxCount < 1 || mailboxCount > 5 {
		t.Fatalf("invalid agent-email rate fixture shape realms=%d mailboxes=%d",
			realmCount, mailboxCount)
	}
	account, err := st.ProvisionAccount(
		ctx,
		fmt.Sprintf("agent-email-rate-%s-%d@witwave.ai", suffix, time.Now().UnixNano()),
		"agent email rate "+suffix,
		time.Hour,
	)
	if err != nil {
		t.Fatal(err)
	}
	if active, err := st.ActivateAccount(ctx, account.AccountID); err != nil || !active {
		t.Fatalf("activate = %t / %v", active, err)
	}

	fixture := agentEmailRateFixture{
		accountID: account.AccountID,
		realms:    make([]agentEmailRateRealmFixture, 0, realmCount),
	}
	for realmIndex := range realmCount {
		realm, err := st.CreateRealm(
			ctx,
			account.AccountID,
			fmt.Sprintf("rate %s %d", suffix, realmIndex+1),
		)
		if err != nil {
			t.Fatal(err)
		}
		enrolled := make(map[string]bool, 5)
		agents := make([]Agent, 0, 5)
		for agentIndex := range 5 {
			agent, err := st.CreateAgent(
				ctx,
				account.AccountID,
				realm.ID,
				fmt.Sprintf("%s r%d agent %d", suffix, realmIndex+1, agentIndex+1),
			)
			if err != nil {
				t.Fatal(err)
			}
			enrolled[agent.ID] = true
			agents = append(agents, agent)
		}
		scope := AgentEmailPilotScope{
			Enabled:  true,
			Domain:   "agent-mail.witwave.ai",
			Audience: fmt.Sprintf("rate-%s-%d", suffix, realmIndex+1),
			RealmIDs: map[string]bool{realm.ID: true},
			AgentIDs: enrolled,
		}
		addresses := make([]AgentEmailAddress, 0, mailboxCount)
		for _, agent := range agents[:mailboxCount] {
			address, err := st.EnsureAgentEmailMailbox(
				ctx, scope, account.AccountID, realm.ID, agent.ID, "",
			)
			if err != nil {
				t.Fatal(err)
			}
			addresses = append(addresses, address)
		}
		fixture.realms = append(fixture.realms, agentEmailRateRealmFixture{
			realm: realm, scope: scope, addresses: addresses,
		})
	}
	return fixture
}

func agentEmailMaximumRateLimits() map[string]int64 {
	return map[string]int64{
		plans.AgentEmailReceivedPerSenderMinuteLimit:         plans.MaxAgentEmailReceivedPerSenderMinute,
		plans.AgentEmailReceivedPerRecipientMinuteLimit:      plans.MaxAgentEmailReceivedPerRecipientMinute,
		plans.AgentEmailReceivedPerRealmMinuteLimit:          plans.MaxAgentEmailReceivedPerRealmMinute,
		plans.AgentEmailReceivedBytesPerSenderMinuteLimit:    plans.MaxAgentEmailReceivedBytesPerSenderMinute,
		plans.AgentEmailReceivedBytesPerRecipientMinuteLimit: plans.MaxAgentEmailReceivedBytesPerRecipientMinute,
		plans.AgentEmailReceivedBytesPerRealmMinuteLimit:     plans.MaxAgentEmailReceivedBytesPerRealmMinute,
	}
}

func setAgentEmailRatePlan(
	ctx context.Context,
	t *testing.T,
	st *Store,
	accountID string,
	revision int64,
	limits map[string]int64,
) {
	t.Helper()
	policies := map[string]int64{
		plans.AgentEmailEntitlementVersionPolicy: plans.AgentEmailEntitlementVersion,
		plans.AgentEmailRetentionDaysPolicy:      90,
	}
	features := []string{plans.AgentEmailReceiveFeature}
	hash, err := plans.SnapshotHash("rate-test", limits, policies, features)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.SetAccountPlan(
		ctx,
		accountID,
		revision,
		hash,
		"rate-test",
		limits,
		policies,
		features,
	); err != nil {
		t.Fatal(err)
	}
}

func ingestAgentEmailRate(
	ctx context.Context,
	st *Store,
	realm agentEmailRateRealmFixture,
	recipientIndex int,
	envelopeSender string,
	raw []byte,
) (AgentEmailMessage, error) {
	recipient := realm.addresses[recipientIndex].Address
	digest := sha256.Sum256(raw)
	return st.IngestAgentEmailPilot(ctx, realm.scope, AgentEmailIngestInput{
		Relay: agentemail.RelayMetadata{
			Timestamp:         time.Now().Unix(),
			KeyID:             "rate-test",
			Audience:          realm.scope.Audience,
			EnvelopeSender:    envelopeSender,
			EnvelopeRecipient: recipient,
			RawSize:           int64(len(raw)),
			RawSHA256:         hex.EncodeToString(digest[:]),
		},
		Raw: raw,
	})
}

func agentEmailRateRaw(recipient, token string) []byte {
	return []byte(strings.Join([]string{
		"From: Sender <sender@example.com>",
		"To: " + recipient,
		"Subject: rate " + token,
		"Message-ID: <rate-" + token + "@example.com>",
		"MIME-Version: 1.0",
		"Content-Type: text/plain; charset=utf-8",
		"",
		"bounded body " + token,
		"",
	}, "\r\n"))
}

func assertAgentEmailRateError(
	t *testing.T,
	err error,
	dimension, scope string,
	limit, attempted int64,
	source string,
	retryable bool,
) {
	t.Helper()
	if !errors.Is(err, ErrAgentEmailRateLimited) {
		t.Fatalf("rate error = %v, want ErrAgentEmailRateLimited", err)
	}
	var detail *AgentEmailRateLimitError
	if !errors.As(err, &detail) || detail == nil {
		t.Fatalf("rate error detail = %#v / %v", detail, err)
	}
	if detail.Dimension != dimension ||
		detail.Scope != scope ||
		detail.Limit != limit ||
		detail.Attempted != attempted ||
		detail.WindowSeconds != agentEmailRateWindowSeconds ||
		detail.Source != source ||
		detail.Retryable != retryable {
		t.Fatalf("rate error detail = %#v; want dimension=%q scope=%q limit=%d attempted=%d window=%d source=%q retryable=%t",
			detail,
			dimension,
			scope,
			limit,
			attempted,
			agentEmailRateWindowSeconds,
			source,
			retryable,
		)
	}
	if retryable && (detail.RetryAfter <= 0 || detail.ResetAt.IsZero()) {
		t.Fatalf("retryable rate error lacks retry fence: %#v", detail)
	}
	if !retryable && (detail.RetryAfter != 0 || !detail.ResetAt.IsZero()) {
		t.Fatalf("non-retryable rate error has retry fence: %#v", detail)
	}
	if strings.Contains(err.Error(), "@") {
		t.Fatalf("rate error leaked an email address: %v", err)
	}
}

func countAgentEmailMessages(
	ctx context.Context,
	t *testing.T,
	st *Store,
	accountID string,
) int64 {
	t.Helper()
	var count int64
	if err := st.pool.QueryRow(ctx, `
		SELECT count(*) FROM agent_email_messages WHERE account_id=$1`,
		accountID,
	).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

func countAgentEmailRateBuckets(
	ctx context.Context,
	t *testing.T,
	st *Store,
	accountID string,
) int64 {
	t.Helper()
	var count int64
	if err := st.pool.QueryRow(ctx, `
		SELECT count(*) FROM agent_email_rate_buckets WHERE account_id=$1`,
		accountID,
	).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}
