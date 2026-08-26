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

// TestAgentEmailV2AttestedVerdictsPostgres pins the cell-side inert half of
// the edge-DMARC lane: a v2 relay envelope's attested verdicts land in the
// advisory columns, a v1 envelope stays all-unknown, and neither touches the
// pinned spam verdict or sender-verification posture.
func TestAgentEmailV2AttestedVerdictsPostgres(t *testing.T) {
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
		"agent-email-v2-attested@witwave.ai", "agent email v2 attested", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if activated, err := st.ActivateAccount(ctx, provisioned.AccountID); err != nil || !activated {
		t.Fatalf("activate = %v / %v", activated, err)
	}
	realm, err := st.CreateRealm(ctx, provisioned.AccountID, "attested realm")
	if err != nil {
		t.Fatal(err)
	}
	// The pilot scope requires 5-10 enrolled agents.
	agents := make([]Agent, 0, 5)
	for _, name := range []string{"attested owner", "peer two", "peer three", "peer four", "peer five"} {
		created, err := st.CreateAgent(ctx, provisioned.AccountID, realm.ID, name)
		if err != nil {
			t.Fatal(err)
		}
		agents = append(agents, created)
	}
	agent := agents[0]
	enrolled := make(map[string]bool, len(agents))
	for _, member := range agents {
		enrolled[member.ID] = true
	}
	scope := AgentEmailPilotScope{
		Enabled: true, Domain: "agent-mail.witwave.ai", Audience: "cell-attested-1",
		RealmIDs: map[string]bool{realm.ID: true},
		AgentIDs: enrolled,
	}
	address, err := st.EnsureAgentEmailMailbox(ctx, scope, provisioned.AccountID,
		realm.ID, agent.ID, "")
	if err != nil {
		t.Fatal(err)
	}

	ingest := func(version, spf, dkim, dmarc, subject string) (AgentEmailMessage, error) {
		raw := []byte("Subject: " + subject + "\r\n\r\nbody\r\n")
		digest := sha256.Sum256(raw)
		return st.IngestAgentEmailPilot(ctx, scope, AgentEmailIngestInput{
			Relay: agentemail.RelayMetadata{
				Version:   version,
				Timestamp: time.Now().Unix(), KeyID: "pilot-key-1", Audience: scope.Audience,
				EnvelopeSender: "sender@example.com", EnvelopeRecipient: address.Address,
				RawSize: int64(len(raw)), RawSHA256: hex.EncodeToString(digest[:]),
				SPFResult: spf, DKIMResult: dkim, DMARCResult: dmarc,
			},
			Raw: raw,
		})
	}

	attested, err := ingest(agentemail.RelaySignatureVersionV2, "pass", "none", "fail", "v2 attested")
	if err != nil {
		t.Fatal(err)
	}
	if attested.SPFResult != "pass" || attested.DKIMResult != "none" || attested.DMARCResult != "fail" ||
		attested.SpamVerdict != "unknown" || attested.SenderVerificationState != AgentEmailSenderUnverified {
		t.Fatalf("v2 attested ingest = %#v", attested)
	}

	legacy, err := ingest("", "", "", "", "v1 legacy")
	if err != nil {
		t.Fatal(err)
	}
	if legacy.SPFResult != "unknown" || legacy.DKIMResult != "unknown" ||
		legacy.DMARCResult != "unknown" || legacy.SpamVerdict != "unknown" {
		t.Fatalf("v1 ingest = %#v", legacy)
	}

	// The stored rows round-trip through the read model with the same values.
	read, err := st.ReadAgentEmail(ctx, scope, Principal{
		Kind: PrincipalAgent, ID: agent.ID, AccountID: provisioned.AccountID,
		RealmID: realm.ID, AgentName: agent.Name, AccountStatus: "active",
	}, attested.ID)
	if err != nil {
		t.Fatal(err)
	}
	if read.SPFResult != "pass" || read.DKIMResult != "none" || read.DMARCResult != "fail" ||
		read.SenderVerificationState != AgentEmailSenderUnverified {
		t.Fatalf("read-back verdicts = %#v", read)
	}
}
