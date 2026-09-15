package store

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/witwave-ai/witself/internal/testenv"
)

func TestSupportTicketRateLimitDefaultsMatchEmailChannel(t *testing.T) {
	data, err := os.ReadFile("../../infra/cloudflare/support-email-intake/wrangler.template.jsonc")
	if err != nil {
		t.Fatal(err)
	}
	// The binding object itself is JSON even though the containing file has comments.
	var bindings []struct {
		Name   string                      `json:"name"`
		Simple struct{ Limit, Period int } `json:"simple"`
	}
	_, block, ok := strings.Cut(string(data), `"ratelimits": [`)
	if !ok {
		t.Fatal("rate limiter config missing")
	}
	block, _, ok = strings.Cut(block, "]")
	if !ok {
		t.Fatal("rate limiter config unterminated")
	}
	if err := json.Unmarshal([]byte("["+block+"]"), &bindings); err != nil {
		t.Fatal(err)
	}
	for _, binding := range bindings {
		if binding.Name == "SUPPORT_EMAIL_SENDER_LIMITER" {
			want := SupportTicketRateLimitConfig{Limit: binding.Simple.Limit, Window: time.Duration(binding.Simple.Period) * time.Second}
			if got := DefaultSupportTicketRateLimitConfig(); got != want {
				t.Fatalf("API admission = %+v; email sender limit = %+v", got, want)
			}
			return
		}
	}
	t.Fatal("support email sender limiter not found")
}

func TestSupportTicketRateLimitConfigBounds(t *testing.T) {
	for _, cfg := range []SupportTicketRateLimitConfig{
		{0, time.Minute}, {1001, time.Minute}, {1, 0}, {1, 1500 * time.Millisecond}, {1, 24*time.Hour + time.Second},
	} {
		if err := cfg.Validate(); err == nil {
			t.Fatalf("accepted %+v", cfg)
		}
	}
	for _, cfg := range []SupportTicketRateLimitConfig{{1, time.Second}, {1000, 24 * time.Hour}} {
		if err := cfg.Validate(); err != nil {
			t.Fatal(err)
		}
	}
}

func supportLimitsTestStore(t *testing.T) *Store {
	t.Helper()
	dsn := testenv.RequirePostgres(t)
	st, _ := newMigrationTestStore(t, dsn)
	if err := st.Migrate(); err != nil {
		t.Fatal(err)
	}
	for _, accountID := range []string{"acc_support_a", "acc_support_b"} {
		if _, err := st.pool.Exec(context.Background(), `INSERT INTO accounts(id,display_name,status)
			VALUES($1,'support limits test','active')`, accountID); err != nil {
			t.Fatal(err)
		}
		for _, suffix := range []string{"one", "two"} {
			if _, err := st.pool.Exec(context.Background(), `INSERT INTO operators(id,account_id,display_name,role)
				VALUES($1,$2,'support test','account_owner')`, accountID+"_"+suffix, accountID); err != nil {
				t.Fatal(err)
			}
		}
	}
	return st
}

func TestSupportTicketRateLimitPostgres(t *testing.T) {
	st := supportLimitsTestStore(t)
	st.supportTicketRateLimit = SupportTicketRateLimitConfig{Limit: 3, Window: time.Minute}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	open := func(accountID, operatorID string) error {
		_, _, err := st.OpenTicket(ctx, OpenTicketInput{
			AccountID: accountID, OperatorID: operatorID, Subject: "private subject", Body: "private body",
		})
		return err
	}
	var wg sync.WaitGroup
	results := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			operatorID := "acc_support_a_one"
			if i%2 == 1 {
				operatorID = "acc_support_a_two"
			}
			results <- open("acc_support_a", operatorID)
		}(i)
	}
	wg.Wait()
	close(results)
	admitted, refused := 0, 0
	for err := range results {
		if err == nil {
			admitted++
			continue
		}
		var detail *SupportRateLimitError
		if !errors.Is(err, ErrSupportRateLimited) || !errors.As(err, &detail) || detail.Limit != 3 || detail.WindowSeconds != 60 || detail.RetryAfterSeconds != 60 {
			t.Fatalf("unexpected refusal: %v", err)
		}
		refused++
	}
	if admitted != 3 || refused != 5 {
		t.Fatalf("admitted/refused=%d/%d", admitted, refused)
	}
	var mismatchedOpeningTimestamps int
	if err := st.pool.QueryRow(ctx, `SELECT count(*) FROM support_tickets t
		JOIN support_ticket_messages m ON m.id=t.last_message_id
		WHERE t.account_id='acc_support_a' AND
		  (t.opened_at <> m.posted_at OR t.opened_at <> t.last_activity_at)`).Scan(&mismatchedOpeningTimestamps); err != nil {
		t.Fatal(err)
	}
	if mismatchedOpeningTimestamps != 0 {
		t.Fatalf("opening ticket/message timestamp mismatches=%d", mismatchedOpeningTimestamps)
	}
	for _, table := range []string{"support_tickets", "support_ticket_messages", "account_events"} {
		var count int
		if err := st.pool.QueryRow(ctx, "SELECT count(*) FROM "+table+" WHERE account_id=$1", "acc_support_a").Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 3 {
			t.Fatalf("%s rows=%d, want 3", table, count)
		}
	}
	if err := open("acc_support_b", "acc_support_b_one"); err != nil {
		t.Fatalf("account isolation: %v", err)
	}
	if err := open("acc_support_b", "acc_support_a_one"); !errors.Is(err, ErrNotAccountOwner) {
		t.Fatalf("cross-account operator: %v", err)
	}
	var ticketID string
	if err := st.pool.QueryRow(ctx, `SELECT id FROM support_tickets WHERE account_id='acc_support_a' LIMIT 1`).Scan(&ticketID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.ReplyToTicket(ctx, "acc_support_a", "acc_support_a_one", ticketID, "follow-up remains available"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.pool.Exec(ctx, `UPDATE support_tickets SET opened_at=clock_timestamp()-interval '61 seconds' WHERE account_id='acc_support_a'`); err != nil {
		t.Fatal(err)
	}
	if err := open("acc_support_a", "acc_support_a_one"); err != nil {
		t.Fatalf("expired window: %v", err)
	}
	// Hostile imported cardinality cannot overflow quota arithmetic or amplify
	// the returned refusal, and never spills one account's refusal to another.
	if _, err := st.pool.Exec(ctx, `INSERT INTO support_tickets(id,account_id,opened_by_kind,opened_by_id,subject)
		SELECT 'tkt_import_'||n,'acc_support_a','owner','acc_support_a_one','private imported subject'
		FROM generate_series(1,1000) n`); err != nil {
		t.Fatal(err)
	}
	if err := open("acc_support_a", "acc_support_a_one"); !errors.Is(err, ErrSupportRateLimited) {
		t.Fatalf("high cardinality admission: %v", err)
	}
}

func TestMigration95SupportTicketAdmissionIndexPostgres(t *testing.T) {
	st, dsn := newMigrationTestStore(t, testenv.RequirePostgres(t))
	migrationTestUpTo(t, dsn, 94)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := st.pool.Exec(ctx, `INSERT INTO accounts(id,display_name,status)
		VALUES('acc_support_index','support index test','active')`); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	// A large old history and a small recent window exercise selectivity under
	// the normal planner settings, without forcing enable_seqscan off.
	if _, err := st.pool.Exec(ctx, `INSERT INTO support_tickets
		(id,account_id,opened_at,opened_by_kind,opened_by_id,subject)
		SELECT 'tkt_support_index_'||n,'acc_support_index',
		       CASE WHEN n <= 20000 THEN $1::timestamptz - interval '30 days'
		            ELSE $1::timestamptz - interval '30 seconds' END,
		       'owner','op_support_index','support index test'
		FROM generate_series(1,20020) n`, now); err != nil {
		t.Fatal(err)
	}
	if err := st.Migrate(); err != nil {
		t.Fatal(err)
	}
	assertMigrationTestVersion(t, dsn, 95)
	if _, err := st.pool.Exec(ctx, `ANALYZE support_tickets`); err != nil {
		t.Fatal(err)
	}
	tx, err := st.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	var accountID string
	if err := tx.QueryRow(ctx, `SELECT id FROM accounts WHERE id=$1 FOR UPDATE`, "acc_support_index").Scan(&accountID); err != nil {
		t.Fatal(err)
	}
	var rawPlan []byte
	if err := tx.QueryRow(ctx, "EXPLAIN (FORMAT JSON) "+supportTicketAdmissionCountSQL,
		accountID, now.Add(-time.Minute), 10).Scan(&rawPlan); err != nil {
		t.Fatal(err)
	}
	type planNode struct {
		NodeType  string     `json:"Node Type"`
		IndexName string     `json:"Index Name"`
		IndexCond string     `json:"Index Cond"`
		Plans     []planNode `json:"Plans"`
	}
	var plans []struct {
		Plan planNode `json:"Plan"`
	}
	if err := json.Unmarshal(rawPlan, &plans); err != nil {
		t.Fatal(err)
	}
	foundIndex := false
	var inspect func(planNode)
	inspect = func(node planNode) {
		if node.NodeType == "Seq Scan" || node.NodeType == "Sort" {
			t.Fatalf("admission scans or sorts the ticket history: %s", rawPlan)
		}
		if node.IndexName == "support_tickets_by_account_opened" &&
			(node.NodeType == "Index Scan" || node.NodeType == "Index Only Scan") &&
			strings.Contains(node.IndexCond, "account_id") && strings.Contains(node.IndexCond, "opened_at") {
			foundIndex = true
		}
		for _, child := range node.Plans {
			inspect(child)
		}
	}
	for _, plan := range plans {
		inspect(plan.Plan)
	}
	if !foundIndex {
		t.Fatalf("admission plan does not use its covering window index: %s", rawPlan)
	}
	var count int
	if err := tx.QueryRow(ctx, supportTicketAdmissionCountSQL,
		accountID, now.Add(-time.Minute), 10).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 10 {
		t.Fatalf("admission count = %d, want bounded count 10", count)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if err := migrationTestDown(t, dsn, false); err != nil {
		t.Fatal(err)
	}
	assertMigrationTestVersion(t, dsn, 94)
	var indexAbsent bool
	if err := st.pool.QueryRow(ctx, `SELECT to_regclass('support_tickets_by_account_opened') IS NULL`).Scan(&indexAbsent); err != nil {
		t.Fatal(err)
	}
	if !indexAbsent {
		t.Fatal("migration 0095 Down retained the admission index")
	}
	if err := st.pool.QueryRow(ctx, `SELECT count(*) FROM support_tickets WHERE account_id=$1`, accountID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 20020 {
		t.Fatalf("index-only migration changed ticket rows: count = %d, want 20020", count)
	}
}
