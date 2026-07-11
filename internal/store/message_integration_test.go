package store

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"
)

// TestMessagePostgresRoundTrip is opt-in because it needs a disposable real
// Postgres database. It covers migration 0020, send/read/ack, and account
// archive export/import as one lifecycle.
func TestMessagePostgresRoundTrip(t *testing.T) {
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

	provisioned, err := st.ProvisionAccount(ctx, "message-test@witwave.ai", "message test", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = deleteAccountForMessageTest(ctx, st, provisioned.AccountID) }()
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

	msg, err := st.SendMessage(ctx, Principal{
		Kind: PrincipalAgent, ID: sender.ID, AccountID: provisioned.AccountID,
		RealmID: realm.ID, AgentName: sender.Name, AccountStatus: "active",
	}, SendMessageInput{
		ToAgent: recipient.ID, Kind: "request", Body: "preserve me",
		Payload: json.RawMessage(`{"task":42}`), IdempotencyKey: "round-trip-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	recipientPrincipal := Principal{
		Kind: PrincipalAgent, ID: recipient.ID, AccountID: provisioned.AccountID,
		RealmID: realm.ID, AgentName: recipient.Name, AccountStatus: "active",
	}
	if _, err := st.ReadMessage(ctx, recipientPrincipal, msg.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AckMessage(ctx, recipientPrincipal, msg.ID); err != nil {
		t.Fatal(err)
	}
	if err := st.SuspendAccountSystem(ctx, provisioned.AccountID, "evacuation", "archive round trip"); err != nil {
		t.Fatal(err)
	}

	var archive bytes.Buffer
	if err := st.ExportAccount(ctx, provisioned.AccountID, "test-source", "test", &archive); err != nil {
		t.Fatal(err)
	}
	if err := deleteAccountForMessageTest(ctx, st, provisioned.AccountID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.ImportAccount(ctx, provisioned.AccountID, bytes.NewReader(archive.Bytes())); err != nil {
		t.Fatal(err)
	}

	var body string
	var payload json.RawMessage
	var readAt, ackedAt *time.Time
	if err := st.pool.QueryRow(ctx, `
		SELECT m.body, m.payload, d.read_at, d.acked_at
		FROM agent_messages m
		JOIN agent_message_deliveries d ON d.message_id = m.id
		WHERE m.id = $1`, msg.ID).Scan(&body, &payload, &readAt, &ackedAt); err != nil {
		t.Fatal(err)
	}
	if body != "preserve me" || !rawJSONEqual(payload, json.RawMessage(`{"task":42}`)) || readAt == nil || ackedAt == nil {
		t.Fatalf("restored message = body:%q payload:%s read:%v acked:%v", body, payload, readAt, ackedAt)
	}
}

func deleteAccountForMessageTest(ctx context.Context, st *Store, accountID string) error {
	tx, err := st.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	statements := []string{
		`DELETE FROM agent_message_deliveries WHERE account_id = $1`,
		`DELETE FROM agent_messages WHERE account_id = $1`,
		`DELETE FROM transcript_entries WHERE account_id = $1`,
		`DELETE FROM transcript_conversations WHERE account_id = $1`,
		`DELETE FROM support_ticket_messages WHERE account_id = $1`,
		`DELETE FROM support_tickets WHERE account_id = $1`,
		`DELETE FROM account_events WHERE account_id = $1`,
		`DELETE FROM tokens WHERE account_id = $1`,
		`DELETE FROM agents WHERE realm_id IN (SELECT id FROM realms WHERE account_id = $1)`,
		`DELETE FROM realms WHERE account_id = $1`,
		`DELETE FROM operators WHERE account_id = $1`,
		`DELETE FROM accounts WHERE id = $1`,
	}
	for _, statement := range statements {
		if _, err := tx.Exec(ctx, statement, accountID); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}
