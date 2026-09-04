package store

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/witwave-ai/witself/internal/plans"
	"github.com/witwave-ai/witself/internal/testenv"
)

func TestAgentEmailCellStorageRefusalsPreserveRateDebtPostgres(t *testing.T) {
	baseDSN := testenv.RequirePostgres(t)
	ctx := context.Background()
	st, _ := newMigrationTestStore(t, baseDSN)
	if err := st.Migrate(); err != nil {
		t.Fatal(err)
	}

	t.Run("inbound preflight refusal commits debt", func(t *testing.T) {
		fixture := newAgentEmailRateFixture(ctx, t, st, "capacity-preflight", 1, 1)
		limits := agentEmailMaximumRateLimits()
		limits[plans.AgentEmailReceivedPerSenderMinuteLimit] = 1
		setAgentEmailRatePlan(ctx, t, st, fixture.accountID, 1, limits)
		if err := st.ConfigureAgentEmailCellStorageLimits(
			ctx, minimumAgentEmailCellStorageRowBytes-1, 100, 1<<20, 1000,
		); err != nil {
			t.Fatal(err)
		}

		realm := fixture.realms[0]
		raw := agentEmailRateRaw(realm.addresses[0].Address, "capacity-preflight")
		_, err := ingestAgentEmailRate(
			ctx, st, realm, 0, "sender@example.com", raw,
		)
		if !IsAgentEmailDatabaseCapacityError(err) {
			t.Fatalf("first preflight refusal = %v", err)
		}
		if got := countAgentEmailRateBuckets(ctx, t, st, fixture.accountID); got != 8 {
			t.Fatalf("preflight refusal retained %d rate buckets, want 8", got)
		}
		_, err = ingestAgentEmailRate(
			ctx, st, realm, 0, "sender@example.com", raw,
		)
		if !errors.Is(err, ErrAgentEmailRateLimited) {
			t.Fatalf("second preflight attempt = %v, want retained rate debt", err)
		}
		if got := countAgentEmailMessages(ctx, t, st, fixture.accountID); got != 0 {
			t.Fatalf("preflight refusals stored %d messages", got)
		}
	})

	t.Run("inbound delivery race rolls back root and commits debt", func(t *testing.T) {
		fixture := newAgentEmailRateFixture(ctx, t, st, "capacity-delivery", 1, 1)
		limits := agentEmailMaximumRateLimits()
		setAgentEmailRatePlan(ctx, t, st, fixture.accountID, 1, limits)
		if err := st.ConfigureAgentEmailCellStorageLimits(
			ctx, 1<<20, 100, 2<<20, 1000,
		); err != nil {
			t.Fatal(err)
		}

		realm := fixture.realms[0]
		raw := agentEmailRateRaw(realm.addresses[0].Address, "capacity-delivery")
		probe, err := ingestAgentEmailRate(
			ctx, st, realm, 0, "sender@example.com", raw,
		)
		if err != nil {
			t.Fatal(err)
		}
		var rootCharge int64
		if err := st.pool.QueryRow(ctx, `
			SELECT witself_agent_email_message_cell_storage_bytes(row_value)
			  FROM agent_email_messages AS row_value
			 WHERE id=$1`, probe.ID).Scan(&rootCharge); err != nil {
			t.Fatal(err)
		}
		if _, err := st.pool.Exec(ctx, `
			DELETE FROM agent_email_messages WHERE id=$1`, probe.ID); err != nil {
			t.Fatal(err)
		}
		clearAgentEmailCapacityRateDebt(ctx, t, st, fixture.accountID)
		limits[plans.AgentEmailReceivedPerSenderMinuteLimit] = 1
		setAgentEmailRatePlan(ctx, t, st, fixture.accountID, 2, limits)
		if err := st.ConfigureAgentEmailCellStorageLimits(
			ctx, rootCharge, 100, rootCharge+1, 1000,
		); err != nil {
			t.Fatal(err)
		}

		_, err = ingestAgentEmailRate(
			ctx, st, realm, 0, "sender@example.com", raw,
		)
		if !IsAgentEmailDatabaseCapacityError(err) {
			t.Fatalf("delivery reserve refusal = %v", err)
		}
		if got := countAgentEmailRateBuckets(ctx, t, st, fixture.accountID); got != 8 {
			t.Fatalf("delivery refusal retained %d rate buckets, want 8", got)
		}
		var messages, deliveries int64
		if err := st.pool.QueryRow(ctx, `
			SELECT
			  (SELECT count(*) FROM agent_email_messages WHERE account_id=$1),
			  (SELECT count(*) FROM agent_email_deliveries WHERE account_id=$1)`,
			fixture.accountID,
		).Scan(&messages, &deliveries); err != nil {
			t.Fatal(err)
		}
		if messages != 0 || deliveries != 0 {
			t.Fatalf("delivery refusal left message/delivery = %d/%d", messages, deliveries)
		}
		if state := readAgentEmailCellStorageState(t, st); state != (agentEmailCellStorageState{}) {
			t.Fatalf("delivery refusal left cell ledger state %+v", state)
		}
		_, err = ingestAgentEmailRate(
			ctx, st, realm, 0, "sender@example.com", raw,
		)
		if !errors.Is(err, ErrAgentEmailRateLimited) {
			t.Fatalf("post-delivery-refusal attempt = %v, want retained rate debt", err)
		}
	})

	t.Run("outbound trigger refusal commits debt", func(t *testing.T) {
		fixture := newAgentEmailRetentionAccountFixture(
			ctx, t, st, fmt.Sprintf("capacity-outbound-%d", time.Now().UnixNano()),
		)
		configureAgentEmailCapacityOutboundFixture(ctx, t, st, &fixture)
		setAgentEmailCapacityOutboundPlan(ctx, t, st, fixture.accountID, 1, 1)
		if err := st.ConfigureAgentEmailCellStorageLimits(
			ctx, minimumAgentEmailCellStorageRowBytes, 100, 1<<20, 1000,
		); err != nil {
			t.Fatal(err)
		}

		input := SendAgentEmailInput{
			To: "recipient@example.com", Subject: "capacity refusal",
			Text: "bounded body", IdempotencyKey: "capacity-refusal-1",
		}
		_, err := st.QueueAgentEmail(ctx, fixture.owner, input)
		if !IsAgentEmailDatabaseCapacityError(err) {
			t.Fatalf("first outbound capacity refusal = %v", err)
		}
		input.IdempotencyKey = "capacity-refusal-2"
		_, err = st.QueueAgentEmail(ctx, fixture.owner, input)
		if !errors.Is(err, ErrAgentEmailOutboundRateLimited) {
			t.Fatalf("second outbound attempt = %v, want retained rate debt", err)
		}
		if got := countAgentEmailOutboundMessages(ctx, t, st, fixture.accountID); got != 0 {
			t.Fatalf("outbound capacity refusals stored %d messages", got)
		}
		if got := countAgentEmailCapacityOutboundRateBuckets(
			ctx, t, st, fixture.accountID,
		); got != 5 {
			t.Fatalf("outbound trigger refusal retained %d rate buckets, want 5", got)
		}

		clearAgentEmailCapacityRateDebt(ctx, t, st, fixture.accountID)
		setAgentEmailCapacityOutboundPlan(ctx, t, st, fixture.accountID, 2, 1)
		if err := st.ConfigureAgentEmailCellStorageLimits(
			ctx, minimumAgentEmailCellStorageRowBytes-1, 100, 2<<20, 1000,
		); err != nil {
			t.Fatal(err)
		}
		preflight := input
		preflight.Subject = "preflight refusal"
		preflight.IdempotencyKey = "preflight-refusal-1"
		_, err = st.QueueAgentEmail(ctx, fixture.owner, preflight)
		if !IsAgentEmailDatabaseCapacityError(err) {
			t.Fatalf("outbound preflight refusal = %v", err)
		}
		if got := countAgentEmailCapacityOutboundRateBuckets(
			ctx, t, st, fixture.accountID,
		); got != 5 {
			t.Fatalf("outbound preflight refusal retained %d rate buckets, want 5", got)
		}
		preflight.IdempotencyKey = "preflight-refusal-2"
		if _, err := st.QueueAgentEmail(ctx, fixture.owner, preflight); !errors.Is(err, ErrAgentEmailOutboundRateLimited) {
			t.Fatalf("second outbound preflight attempt = %v, want retained debt", err)
		}

		clearAgentEmailCapacityRateDebt(ctx, t, st, fixture.accountID)
		setAgentEmailCapacityOutboundPlan(ctx, t, st, fixture.accountID, 3, 2)
		if err := st.ConfigureAgentEmailCellStorageLimits(
			ctx, 1<<20, 100, 2<<20, 1000,
		); err != nil {
			t.Fatal(err)
		}

		exact := SendAgentEmailInput{
			To: "recipient@example.com", Subject: "concurrent replay",
			Text: "bounded body", IdempotencyKey: "concurrent-replay",
		}
		results := make([]AgentEmailOutboundMessage, 2)
		errs := make([]error, 2)
		var group sync.WaitGroup
		for index := range results {
			index := index
			group.Add(1)
			go func() {
				defer group.Done()
				results[index], errs[index] = st.QueueAgentEmail(ctx, fixture.owner, exact)
			}()
		}
		group.Wait()
		if errs[0] != nil || errs[1] != nil || results[0].ID == "" ||
			results[0].ID != results[1].ID {
			t.Fatalf("concurrent exact replay = %#v / %#v", results, errs)
		}
		second := exact
		second.Subject = "second logical send"
		second.IdempotencyKey = "second-logical-send"
		if _, err := st.QueueAgentEmail(ctx, fixture.owner, second); err != nil {
			t.Fatalf("exact replay consumed a second debit: %v", err)
		}
		third := exact
		third.Subject = "third logical send"
		third.IdempotencyKey = "third-logical-send"
		if _, err := st.QueueAgentEmail(ctx, fixture.owner, third); !errors.Is(err, ErrAgentEmailOutboundRateLimited) {
			t.Fatalf("third logical send = %v, want rate refusal", err)
		}
	})

	t.Run("caller cancellation cannot erase finalized debt", func(t *testing.T) {
		fixture := newAgentEmailRateFixture(ctx, t, st, "capacity-cancel", 1, 1)
		insertDebt := func(tx pgx.Tx, dimension string) {
			t.Helper()
			if _, err := tx.Exec(ctx, `
				INSERT INTO agent_email_account_rate_buckets
				  (account_id,dimension,theoretical_arrival_nanoseconds)
				VALUES ($1,$2,1)`, fixture.accountID, dimension); err != nil {
				t.Fatal(err)
			}
		}
		assertDebt := func(received, receivedBytes int64) {
			t.Helper()
			var gotReceived, gotBytes int64
			if err := st.pool.QueryRow(ctx, `
				SELECT
				  count(*) FILTER (WHERE dimension='email_received'),
				  count(*) FILTER (WHERE dimension='email_received_bytes')
				FROM agent_email_account_rate_buckets
				WHERE account_id=$1`, fixture.accountID,
			).Scan(&gotReceived, &gotBytes); err != nil {
				t.Fatal(err)
			}
			if gotReceived != received || gotBytes != receivedBytes {
				t.Fatalf("finalized debt rows = %d/%d, want %d/%d",
					gotReceived, gotBytes, received, receivedBytes)
			}
		}

		tx, err := st.pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		insertDebt(tx, AgentEmailRateDimensionReceived)
		canceledCtx, cancel := context.WithCancel(ctx)
		cancel()
		if err := commitAgentEmailCellStorageRefusal(
			canceledCtx, tx, ErrAgentEmailDatabaseCapacity,
		); !errors.Is(err, ErrAgentEmailDatabaseCapacity) {
			t.Fatalf("canceled preflight finalization = %v", err)
		}
		assertDebt(1, 0)

		clearAgentEmailCapacityRateDebt(ctx, t, st, fixture.accountID)
		tx, err = st.pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		insertDebt(tx, AgentEmailRateDimensionReceived)
		writeTx, err := tx.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		insertDebt(writeTx, AgentEmailRateDimensionReceivedBytes)
		canceledCtx, cancel = context.WithCancel(ctx)
		cancel()
		if err := rollbackAgentEmailCellStorageWriteAndCommitRefusal(
			canceledCtx, tx, writeTx, ErrAgentEmailDatabaseCapacity,
		); !errors.Is(err, ErrAgentEmailDatabaseCapacity) {
			t.Fatalf("canceled trigger finalization = %v", err)
		}
		assertDebt(1, 0)
	})
}

func clearAgentEmailCapacityRateDebt(
	ctx context.Context,
	t *testing.T,
	st *Store,
	accountID string,
) {
	t.Helper()
	for _, query := range []string{
		`DELETE FROM agent_email_rate_buckets WHERE account_id=$1`,
		`DELETE FROM agent_email_account_rate_buckets WHERE account_id=$1`,
		`DELETE FROM agent_email_outbound_rate_buckets WHERE account_id=$1`,
	} {
		if _, err := st.pool.Exec(ctx, query, accountID); err != nil {
			t.Fatal(err)
		}
	}
}

func countAgentEmailCapacityOutboundRateBuckets(
	ctx context.Context,
	t *testing.T,
	st *Store,
	accountID string,
) int64 {
	t.Helper()
	var count int64
	if err := st.pool.QueryRow(ctx, `
		SELECT count(*) FROM agent_email_outbound_rate_buckets
		 WHERE account_id=$1`, accountID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

func configureAgentEmailCapacityOutboundFixture(
	ctx context.Context,
	t *testing.T,
	st *Store,
	fixture *agentEmailRetentionAccountFixture,
) {
	t.Helper()
	fixture.scope.Domain = "witmail.net"
	fixture.scope.LegacyDomains = []string{"agent-mail.witwave.ai"}
	fixture.scope.Audience = "capacity-outbound"
	addresses, err := st.ReconcileAgentEmailPilot(ctx, fixture.scope)
	if err != nil {
		t.Fatal(err)
	}
	for _, address := range addresses {
		if address.OwnerAgentID == fixture.owner.ID {
			fixture.address = address
			return
		}
	}
	t.Fatal("capacity outbound fixture sender address is missing")
}

func setAgentEmailCapacityOutboundPlan(
	ctx context.Context,
	t *testing.T,
	st *Store,
	accountID string,
	revision, agentMinuteLimit int64,
) {
	t.Helper()
	limits := map[string]int64{
		plans.AgentEmailSentPerAgentMinuteLimit: agentMinuteLimit,
	}
	policies := map[string]int64{
		plans.AgentEmailEntitlementVersionPolicy: plans.AgentEmailEntitlementVersion,
		plans.AgentEmailRetentionDaysPolicy:      30,
	}
	features := []string{plans.AgentEmailReceiveFeature, plans.AgentEmailSendFeature}
	hash, err := plans.SnapshotHash("capacity-test", limits, policies, features)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.SetAccountPlan(
		ctx, accountID, revision, hash, "capacity-test",
		limits, policies, features,
	); err != nil {
		t.Fatal(err)
	}
}
