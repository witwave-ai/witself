package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"testing"
	"time"

	"github.com/witwave-ai/witself/internal/agentemail"
)

// The limited Cloudflare pilot has no authoritative spam or abuse verdict.
// Provisioning and ingestion therefore remain operationally observable but
// must not write usage that could be interpreted as a customer charge or
// quota debit.
func TestAgentEmailPilotEmitsNoBillableUsage(t *testing.T) {
	baseDSN := os.Getenv("WITSELF_TEST_DATABASE_URL")
	if baseDSN == "" {
		t.Skip("WITSELF_TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	st, _ := newMigrationTestStore(t, baseDSN)
	if err := st.Migrate(); err != nil {
		t.Fatal(err)
	}
	provisioned, err := st.ProvisionAccount(ctx,
		"agent-email-pilot-metering@witwave.ai", "agent email pilot metering", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if activated, err := st.ActivateAccount(ctx, provisioned.AccountID); err != nil || !activated {
		t.Fatalf("activate = %v / %v", activated, err)
	}
	realm, err := st.CreateRealm(ctx, provisioned.AccountID, "email metering pilot")
	if err != nil {
		t.Fatal(err)
	}

	agents := make([]Agent, 0, 5)
	enrolled := make(map[string]bool, 5)
	for _, name := range []string{"meter owner", "meter two", "meter three", "meter four", "meter five"} {
		agent, err := st.CreateAgent(ctx, provisioned.AccountID, realm.ID, name)
		if err != nil {
			t.Fatal(err)
		}
		agents = append(agents, agent)
		enrolled[agent.ID] = true
	}
	scope := AgentEmailPilotScope{
		Enabled: true, Domain: "agent-mail.witwave.ai", Audience: "cell-metering-pilot",
		RealmIDs: map[string]bool{realm.ID: true}, AgentIDs: enrolled,
	}
	address, err := st.EnsureAgentEmailMailbox(ctx, scope, provisioned.AccountID,
		realm.ID, agents[0].ID, "")
	if err != nil {
		t.Fatal(err)
	}
	raw := []byte("From: sender@example.com\r\nTo: " + address.Address +
		"\r\nSubject: pilot accounting\r\n\r\nOperational only.\r\n")
	digest := sha256.Sum256(raw)
	if _, err := st.IngestAgentEmailPilot(ctx, scope, AgentEmailIngestInput{
		Relay: agentemail.RelayMetadata{
			Timestamp: time.Now().Unix(), KeyID: "pilot-key-1", Audience: scope.Audience,
			EnvelopeSender: "sender@example.com", EnvelopeRecipient: address.Address,
			RawSize: int64(len(raw)), RawSHA256: hex.EncodeToString(digest[:]),
		},
		Raw: raw,
	}); err != nil {
		t.Fatal(err)
	}

	var events, rollups int
	if err := st.pool.QueryRow(ctx, `
		SELECT count(*) FROM usage_events
		WHERE account_id=$1 AND dimension LIKE 'email%'`, provisioned.AccountID).Scan(&events); err != nil {
		t.Fatal(err)
	}
	if err := st.pool.QueryRow(ctx, `
		SELECT count(*) FROM usage_rollups
		WHERE account_id=$1 AND dimension LIKE 'email%'`, provisioned.AccountID).Scan(&rollups); err != nil {
		t.Fatal(err)
	}
	if events != 0 || rollups != 0 {
		t.Fatalf("pilot billable email usage = %d events / %d rollups, want 0 / 0", events, rollups)
	}
}
