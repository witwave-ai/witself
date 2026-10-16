package store

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/witwave-ai/witself/internal/testenv"
)

func TestCheckAccountCloseAndFinalRecheckPostgres(t *testing.T) {
	dsn := testenv.RequirePostgres(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	st, _ := newMigrationTestStore(t, dsn)
	if err := st.Migrate(); err != nil {
		t.Fatal(err)
	}
	a, err := st.ProvisionAccount(ctx, "close-preflight@example.test", "close preflight", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.ActivateAccount(ctx, a.AccountID); err != nil {
		t.Fatal(err)
	}
	_, tokenID, _, err := st.CreateOperatorToken(ctx, a.AccountID, a.OperatorID, "close test", nil)
	if err != nil {
		t.Fatal(err)
	}

	assertUnchanged := func(wantStatus string) {
		t.Helper()
		var status string
		var hasClosedAt, consumed bool
		var closeEvents int
		if err := st.pool.QueryRow(ctx, `SELECT status, closed_at IS NOT NULL FROM accounts WHERE id=$1`, a.AccountID).Scan(&status, &hasClosedAt); err != nil {
			t.Fatal(err)
		}
		if err := st.pool.QueryRow(ctx, `SELECT consumed_at IS NOT NULL FROM tokens WHERE id=$1`, tokenID).Scan(&consumed); err != nil {
			t.Fatal(err)
		}
		if err := st.pool.QueryRow(ctx, `SELECT count(*) FROM account_events WHERE account_id=$1 AND verb='account.closed'`, a.AccountID).Scan(&closeEvents); err != nil {
			t.Fatal(err)
		}
		if status != wantStatus || hasClosedAt || consumed || closeEvents != 0 {
			t.Fatalf("preflight changed account or credentials: %s, closed=%t, consumed=%t, events=%d", status, hasClosedAt, consumed, closeEvents)
		}
	}
	for _, status := range []string{"pending", "suspended", "active"} {
		if _, err := st.pool.Exec(ctx, `UPDATE accounts SET status=$2 WHERE id=$1`, a.AccountID, status); err != nil {
			t.Fatal(err)
		}
		if err := st.CheckAccountClose(ctx, a.AccountID, a.OperatorID); err != nil {
			t.Fatal(err)
		}
		assertUnchanged(status)
		// A second transaction must acquire the row immediately: external billing
		// never inherits a transaction or row lock from the preflight.
		tx, err := st.pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		_, lockErr := tx.Exec(ctx, `SELECT id FROM accounts WHERE id=$1 FOR UPDATE NOWAIT`, a.AccountID)
		_ = tx.Rollback(ctx)
		if lockErr != nil {
			t.Fatalf("preflight retained its row lock: %v", lockErr)
		}
	}
	if _, err := st.pool.Exec(ctx, `INSERT INTO operators (id,account_id,role) VALUES ('opr_close_member',$1,'account_operator')`, a.AccountID); err != nil {
		t.Fatal(err)
	}
	for _, op := range []string{"opr_close_member", "opr_missing"} {
		if err := st.CheckAccountClose(ctx, a.AccountID, op); !errors.Is(err, ErrNotAccountOwner) {
			t.Fatalf("nonowner preflight: %v", err)
		}
	}
	if _, err := st.pool.Exec(ctx, `UPDATE accounts SET is_default=true WHERE id=$1`, a.AccountID); err != nil {
		t.Fatal(err)
	}
	if err := st.CheckAccountClose(ctx, a.AccountID, a.OperatorID); !errors.Is(err, ErrCannotCloseDefault) {
		t.Fatalf("default preflight: %v", err)
	}
	if _, err := st.pool.Exec(ctx, `UPDATE accounts SET is_default=false WHERE id=$1`, a.AccountID); err != nil {
		t.Fatal(err)
	}

	realm, err := st.CreateRealm(ctx, a.AccountID, "default")
	if err != nil {
		t.Fatal(err)
	}
	agent, err := st.CreateAgent(ctx, a.AccountID, realm.ID, "close-test")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.CheckAccountClose(ctx, a.AccountID, a.OperatorID); err != nil {
		t.Fatal(err)
	}
	// Simulate a lifecycle starting while the control plane cancels billing.
	if _, err := st.pool.Exec(ctx, `INSERT INTO agent_vault_keys
		(id,account_id,realm_id,owner_agent_id,key_version,algorithm,fingerprint,lifecycle_state,row_version)
		VALUES ('avk_zzzzzzzzzzzzzzzz',$1,$2,$3,1,$4,$5,'pending',1)`, a.AccountID, realm.ID, agent.ID, SecretAEADAlgorithm, strings.Repeat("9", 64)); err != nil {
		t.Fatal(err)
	}
	if err := st.CheckAccountClose(ctx, a.AccountID, a.OperatorID); !errors.Is(err, ErrVaultLifecycleInProgress) {
		t.Fatalf("vault preflight: %v", err)
	}
	if err := st.CloseAccount(ctx, a.AccountID, a.OperatorID, "requested"); !errors.Is(err, ErrVaultLifecycleInProgress) {
		t.Fatalf("final vault recheck: %v", err)
	}
	assertUnchanged("active")
	if _, err := st.pool.Exec(ctx, `UPDATE agent_vault_keys SET lifecycle_state='retired',retired_at=created_at WHERE id='avk_zzzzzzzzzzzzzzzz'`); err != nil {
		t.Fatal(err)
	}
	if err := st.CloseAccount(ctx, a.AccountID, a.OperatorID, "requested"); err != nil {
		t.Fatal(err)
	}
	// Reclosing preserves the tombstone/audit and sweeps a simulated straggler.
	if _, err := st.pool.Exec(ctx, `UPDATE tokens SET consumed_at=NULL WHERE id=$1`, tokenID); err != nil {
		t.Fatal(err)
	}
	if err := st.CheckAccountClose(ctx, a.AccountID, a.OperatorID); err != nil {
		t.Fatal(err)
	}
	if err := st.CloseAccount(ctx, a.AccountID, a.OperatorID, "retry"); err != nil {
		t.Fatal(err)
	}
	var events, liveTokens int
	var reason string
	if err := st.pool.QueryRow(ctx, `SELECT count(*) FROM account_events WHERE account_id=$1 AND verb='account.closed'`, a.AccountID).Scan(&events); err != nil {
		t.Fatal(err)
	}
	if err := st.pool.QueryRow(ctx, `SELECT count(*) FROM tokens WHERE account_id=$1 AND consumed_at IS NULL`, a.AccountID).Scan(&liveTokens); err != nil {
		t.Fatal(err)
	}
	if err := st.pool.QueryRow(ctx, `SELECT closed_reason FROM accounts WHERE id=$1 AND status='closed'`, a.AccountID).Scan(&reason); err != nil {
		t.Fatal(err)
	}
	if events != 1 || liveTokens != 0 || reason != "requested" {
		t.Fatalf("close replay events=%d liveTokens=%d reason=%q", events, liveTokens, reason)
	}
}
