package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/witwave-ai/witself/internal/agentemail"
	"github.com/witwave-ai/witself/internal/plans"
)

func TestAgentEmailFeatureGateCoversOwnerAndIngressOperationsPostgres(t *testing.T) {
	dsn := os.Getenv("WITSELF_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("WITSELF_TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	st, _ := newMigrationTestStore(t, dsn)
	if err := st.Migrate(); err != nil {
		t.Fatal(err)
	}
	provisioned, err := st.ProvisionAccount(
		ctx, "agent-email-feature-gate@witwave.ai", "agent email feature gate", time.Hour,
	)
	if err != nil {
		t.Fatal(err)
	}
	if activated, err := st.ActivateAccount(ctx, provisioned.AccountID); err != nil || !activated {
		t.Fatalf("activate = %v / %v", activated, err)
	}
	realm, err := st.CreateRealm(ctx, provisioned.AccountID, "email feature gate")
	if err != nil {
		t.Fatal(err)
	}
	agents := make([]Agent, 0, 5)
	enrolled := make(map[string]bool, 5)
	for _, name := range []string{"owner", "two", "three", "four", "five"} {
		agent, err := st.CreateAgent(ctx, provisioned.AccountID, realm.ID, name)
		if err != nil {
			t.Fatal(err)
		}
		agents = append(agents, agent)
		enrolled[agent.ID] = true
	}
	owner := Principal{
		Kind: PrincipalAgent, ID: agents[0].ID, AccountID: provisioned.AccountID,
		RealmID: realm.ID, AgentName: agents[0].Name, AccountStatus: "active",
	}
	scope := AgentEmailPilotScope{
		Enabled: true, Domain: "agent-mail.witwave.ai", Audience: "cell-feature-gate",
		RealmIDs: map[string]bool{realm.ID: true}, AgentIDs: enrolled,
		RetryCanaryAgentID: owner.ID,
	}
	address, err := st.EnsureAgentEmailMailbox(
		ctx, scope, provisioned.AccountID, realm.ID, owner.ID, "",
	)
	if err != nil {
		t.Fatal(err)
	}
	applyPlan := func(revision int64, policies map[string]int64, features []string) {
		t.Helper()
		hash, err := plans.SnapshotHash("test", map[string]int64{}, policies, features)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := st.SetAccountPlan(
			ctx, provisioned.AccountID, revision, hash, "test",
			map[string]int64{}, policies, features,
		); err != nil {
			t.Fatal(err)
		}
	}
	applyPlan(1, map[string]int64{TranscriptRetentionDaysPolicy: 30}, []string{"memory"})
	if err := st.RequireAgentEmailReceiveEnabled(ctx, owner); err != nil {
		t.Fatalf("legacy entitlement precheck was gated: %v", err)
	}
	if checkpoint, err := st.GetSelfAgentEmailCheckpoint(ctx, scope, owner); err != nil || !checkpoint.Enabled {
		t.Fatalf("legacy checkpoint = %+v / %v", checkpoint, err)
	}

	emailPolicies := map[string]int64{
		plans.AgentEmailRetentionDaysPolicy:      30,
		plans.AgentEmailEntitlementVersionPolicy: plans.AgentEmailEntitlementVersion,
	}
	applyPlan(2, emailPolicies, []string{"memory", "facts"})
	assertDisabled := func(name string, call func() error) {
		t.Helper()
		err := call()
		var featureErr *FeatureNotEnabledError
		if !errors.Is(err, ErrFeatureNotEnabled) || !errors.As(err, &featureErr) ||
			featureErr.Feature != plans.AgentEmailReceiveFeature {
			t.Fatalf("%s error = %v, want agent-email FeatureNotEnabledError", name, err)
		}
	}
	assertDisabled("precheck", func() error {
		return st.RequireAgentEmailReceiveEnabled(ctx, owner)
	})

	raw := []byte(strings.Join([]string{
		"From: sender@example.com",
		"To: " + address.Address,
		"Subject: must be discarded",
		"",
		"never persist",
	}, "\r\n"))
	digest := sha256.Sum256(raw)
	ingestInput := AgentEmailIngestInput{
		Relay: agentemail.RelayMetadata{
			Timestamp: time.Now().Unix(), KeyID: "pilot-key", Audience: scope.Audience,
			EnvelopeSender: "sender@example.com", EnvelopeRecipient: address.Address,
			RawSize: int64(len(raw)), RawSHA256: hex.EncodeToString(digest[:]),
		},
		Raw: raw,
	}
	messageID := "emsg_aaaaaaaaaaaaaaaa"
	claimID := "ecl_aaaaaaaaaaaaaaaa"
	challenge := "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	disabledOperations := []struct {
		name string
		call func() error
	}{
		{"address", func() error {
			_, err := st.GetAgentEmailAddress(ctx, scope, owner)
			return err
		}},
		{"list", func() error {
			_, err := st.ListAgentEmails(ctx, scope, owner, AgentEmailFilter{})
			return err
		}},
		{"read", func() error {
			_, err := st.ReadAgentEmail(ctx, scope, owner, messageID)
			return err
		}},
		{"ack", func() error {
			_, err := st.AckAgentEmail(ctx, scope, owner, messageID)
			return err
		}},
		{"code consumed", func() error {
			_, err := st.MarkAgentEmailCodeConsumed(ctx, scope, owner, messageID)
			return err
		}},
		{"claim", func() error {
			_, err := st.ClaimAgentEmail(ctx, scope, owner, messageID, ClaimAgentEmailInput{
				IdempotencyKey: "disabled-claim",
			})
			return err
		}},
		{"renew", func() error {
			_, err := st.RenewAgentEmailClaim(ctx, scope, owner, messageID, RenewAgentEmailClaimInput{
				ClaimID: claimID, Generation: 1,
			})
			return err
		}},
		{"release", func() error {
			_, err := st.ReleaseAgentEmailClaim(ctx, scope, owner, messageID, ReleaseAgentEmailClaimInput{
				ClaimID: claimID, Generation: 1,
			})
			return err
		}},
		{"complete", func() error {
			_, err := st.CompleteAgentEmail(ctx, scope, owner, messageID, CompleteAgentEmailInput{
				ClaimID: claimID, Generation: 1, IdempotencyKey: "disabled-complete",
			})
			return err
		}},
		{"retry canary arm", func() error {
			_, err := st.ArmAgentEmailRetryCanary(ctx, scope, owner, challenge)
			return err
		}},
		{"retry canary status", func() error {
			_, err := st.GetAgentEmailRetryCanaryStatus(ctx, scope, owner, challenge)
			return err
		}},
		{"ingest", func() error {
			_, err := st.IngestAgentEmailPilot(ctx, scope, ingestInput)
			return err
		}},
	}
	for _, operation := range disabledOperations {
		assertDisabled(operation.name, operation.call)
	}
	var messages, deliveries, canaries int
	if err := st.pool.QueryRow(ctx, `
		SELECT
		  (SELECT count(*) FROM agent_email_messages WHERE account_id=$1),
		  (SELECT count(*) FROM agent_email_deliveries WHERE account_id=$1),
		  (SELECT count(*) FROM agent_email_retry_canary_arms WHERE account_id=$1)`,
		provisioned.AccountID,
	).Scan(&messages, &deliveries, &canaries); err != nil {
		t.Fatal(err)
	}
	if messages != 0 || deliveries != 0 || canaries != 0 {
		t.Fatalf("disabled email mutated messages/deliveries/canaries = %d/%d/%d",
			messages, deliveries, canaries)
	}
	checkpoint, err := st.GetSelfAgentEmailCheckpoint(ctx, scope, owner)
	if err != nil || checkpoint.Enabled || checkpoint.Pending || checkpoint.MailboxPending {
		t.Fatalf("disabled checkpoint = %+v / %v", checkpoint, err)
	}

	// Operator receive controls stay configurable while the account entitlement
	// is disabled; they take effect if receipt is enabled again.
	control, err := st.SetAgentEmailReceiveControl(
		ctx, scope, provisioned.AccountID, "operator-test", owner.ID,
		AgentEmailReceiveDisabled,
	)
	if err != nil || control.AgentReceiveState != AgentEmailReceiveDisabled {
		t.Fatalf("disabled-plan operator control = %+v / %v", control, err)
	}
	if _, err := st.SetAgentEmailReceiveControl(
		ctx, scope, provisioned.AccountID, "operator-test", owner.ID,
		AgentEmailReceiveEnabled,
	); err != nil {
		t.Fatalf("re-enable operator control under disabled plan: %v", err)
	}

	applyPlan(3, emailPolicies, []string{
		"memory", "facts", plans.AgentEmailReceiveFeature,
	})
	if err := st.RequireAgentEmailReceiveEnabled(ctx, owner); err != nil {
		t.Fatalf("enabled entitlement precheck = %v", err)
	}
	if checkpoint, err := st.GetSelfAgentEmailCheckpoint(ctx, scope, owner); err != nil || !checkpoint.Enabled {
		t.Fatalf("enabled checkpoint = %+v / %v", checkpoint, err)
	}
	if message, err := st.IngestAgentEmailPilot(ctx, scope, ingestInput); err != nil || message.ID == "" {
		t.Fatalf("enabled ingest = %+v / %v", message, err)
	}
}
