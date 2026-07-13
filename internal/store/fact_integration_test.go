package store

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"
)

// TestFactPostgresRoundTrip is opt-in because it needs disposable Postgres. It
// covers migration 0022, assertion supersession, redaction, and usage emission.
func TestFactPostgresRoundTrip(t *testing.T) {
	dsn := os.Getenv("WITSELF_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("WITSELF_TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	st, err := Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.Migrate(); err != nil {
		t.Fatal(err)
	}

	provisioned, err := st.ProvisionAccount(ctx, "fact-test@witwave.ai", "fact test", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = deleteFactTestAccount(ctx, st, provisioned.AccountID) }()
	if activated, err := st.ActivateAccount(ctx, provisioned.AccountID); err != nil || !activated {
		t.Fatalf("activate = %v / %v", activated, err)
	}
	realm, err := st.CreateRealm(ctx, provisioned.AccountID, "default")
	if err != nil {
		t.Fatal(err)
	}
	agent, err := st.CreateAgent(ctx, provisioned.AccountID, realm.ID, "scott")
	if err != nil {
		t.Fatal(err)
	}
	p := Principal{Kind: PrincipalAgent, ID: agent.ID, AccountID: provisioned.AccountID, RealmID: realm.ID, AccountStatus: "active"}

	first, err := st.SetFact(ctx, p, SetFactInput{Predicate: "preferences/editor", Value: json.RawMessage(`"vim"`), SourceKind: FactSourceAgent})
	if err != nil {
		t.Fatal(err)
	}
	second, err := st.SetFact(ctx, p, SetFactInput{Predicate: "preferences/editor", Value: json.RawMessage(`"zed"`), SourceKind: FactSourceAgent, Sensitive: true})
	if err != nil {
		t.Fatal(err)
	}
	if first.ID != second.ID || second.ResolvedAssertionID == first.ResolvedAssertionID {
		t.Fatalf("fact identity/assertions = %#v -> %#v", first, second)
	}
	got, err := st.GetFact(ctx, p, "self", "preferences/editor")
	if err != nil {
		t.Fatal(err)
	}
	if string(got.Value) != `"zed"` {
		t.Fatalf("value = %s", got.Value)
	}
	listed, err := st.ListFacts(ctx, p, FactListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 1 || string(listed[0].Value) != "null" {
		t.Fatalf("listed = %#v", listed)
	}
	history, err := st.FactHistory(ctx, p, first.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(history) != 2 || history[0].SupersedesID != history[1].ID {
		t.Fatalf("history = %#v", history)
	}
	var usageCount int
	if err := st.pool.QueryRow(ctx, `SELECT count(*) FROM usage_events WHERE subject_type = 'fact' AND subject_id = $1`, first.ID).Scan(&usageCount); err != nil {
		t.Fatal(err)
	}
	if usageCount != 1 {
		t.Fatalf("fact usage events = %d", usageCount)
	}
}

func deleteFactTestAccount(ctx context.Context, st *Store, accountID string) error {
	tx, err := st.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	for _, statement := range []string{
		`DELETE FROM usage_rollups WHERE account_id = $1`,
		`DELETE FROM usage_events WHERE account_id = $1`,
		`DELETE FROM fact_assertions WHERE account_id = $1`,
		`DELETE FROM facts WHERE account_id = $1`,
		`DELETE FROM fact_subjects WHERE account_id = $1`,
		`DELETE FROM tokens WHERE account_id = $1`,
		`DELETE FROM agents WHERE realm_id IN (SELECT id FROM realms WHERE account_id = $1)`,
		`DELETE FROM realms WHERE account_id = $1`,
		`DELETE FROM operators WHERE account_id = $1`,
		`DELETE FROM accounts WHERE id = $1`,
	} {
		if _, err := tx.Exec(ctx, statement, accountID); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}
