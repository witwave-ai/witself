package store

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/witwave-ai/witself/internal/plans"
)

func TestMessagingFeatureGateCoversMailboxAndRequestOperationsPostgres(t *testing.T) {
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

	provisioned, err := st.ProvisionAccount(ctx,
		"message-feature-gate@witwave.ai", "message feature gate", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = deleteAccountForIntegrationTest(context.Background(), st, provisioned.AccountID) }()
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
	principal := Principal{
		Kind: PrincipalAgent, ID: sender.ID, AccountID: provisioned.AccountID,
		RealmID: realm.ID, AgentName: sender.Name, AccountStatus: "active",
	}

	applyPlan := func(revision int64, policies map[string]int64, features []string) {
		t.Helper()
		hash, err := plans.SnapshotHash("test", map[string]int64{}, policies, features)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := st.SetAccountPlan(ctx, provisioned.AccountID, revision, hash, "test",
			map[string]int64{}, policies, features); err != nil {
			t.Fatal(err)
		}
	}
	legacyPolicies := map[string]int64{TranscriptRetentionDaysPolicy: 30}
	applyPlan(1, legacyPolicies, []string{"memory", "facts"})
	if _, err := st.ListMessages(ctx, principal, MessageFilter{}); err != nil {
		t.Fatalf("legacy applied snapshot without messaging marker was gated: %v", err)
	}
	legacyCheckpoint, err := st.GetSelfMessageCheckpoint(ctx, principal)
	if err != nil || !legacyCheckpoint.Enabled {
		t.Fatalf("legacy applied checkpoint = %+v / %v", legacyCheckpoint, err)
	}
	messagePolicies := map[string]int64{
		TranscriptRetentionDaysPolicy:           30,
		plans.MessageRetentionDaysPolicy:        30,
		plans.MessagingEntitlementVersionPolicy: plans.MessagingEntitlementVersion,
	}
	applyPlan(2, messagePolicies, []string{"memory", "facts"})

	messageID := "msg_abcdefghijklmnop"
	messageClaimID := "mcl_abcdefghijklmnop"
	requestID := "mrq_abcdefghijklmnop"
	requestClaimID := "mrc_abcdefghijklmnop"
	disabledOperations := []struct {
		name string
		call func() error
	}{
		{"send", func() error {
			_, err := st.SendMessage(ctx, principal, SendMessageInput{
				ToAgent: recipient.ID, Body: "must never persist", Payload: json.RawMessage(`{"private":"payload"}`),
				IdempotencyKey: "disabled-send",
			})
			return err
		}},
		{"reply", func() error {
			_, err := st.ReplyMessage(ctx, principal, messageID, ReplyMessageInput{
				Body: "reply", IdempotencyKey: "disabled-reply",
			})
			return err
		}},
		{"claim", func() error {
			_, err := st.ClaimMessage(ctx, principal, messageID, ClaimMessageInput{
				IdempotencyKey: "disabled-claim",
			})
			return err
		}},
		{"renew", func() error {
			_, err := st.RenewMessageClaim(ctx, principal, messageID, RenewMessageClaimInput{
				ClaimID: messageClaimID, ProcessingGeneration: 1,
			})
			return err
		}},
		{"release", func() error {
			_, err := st.ReleaseMessageClaim(ctx, principal, messageID, ReleaseMessageClaimInput{
				ClaimID: messageClaimID, ProcessingGeneration: 1,
			})
			return err
		}},
		{"complete", func() error {
			_, err := st.CompleteMessage(ctx, principal, messageID, CompleteMessageInput{
				ClaimID: messageClaimID, ProcessingGeneration: 1,
				Body: "complete", IdempotencyKey: "disabled-complete",
			})
			return err
		}},
		{"list", func() error {
			_, err := st.ListMessages(ctx, principal, MessageFilter{})
			return err
		}},
		{"read", func() error {
			_, err := st.ReadMessage(ctx, principal, messageID)
			return err
		}},
		{"ack", func() error {
			_, err := st.AckMessage(ctx, principal, messageID)
			return err
		}},
		{"request open", func() error {
			_, err := st.OpenMessageRequest(ctx, principal, OpenMessageRequestInput{
				Body: "open", IdempotencyKey: "disabled-open",
			})
			return err
		}},
		{"request list", func() error {
			_, err := st.ListMessageRequests(ctx, principal, MessageRequestFilter{})
			return err
		}},
		{"request get", func() error {
			_, err := st.GetMessageRequest(ctx, principal, requestID)
			return err
		}},
		{"request offer", func() error {
			_, err := st.OfferMessageRequest(ctx, principal, requestID, OfferMessageRequestInput{
				Body: "offer", IdempotencyKey: "disabled-offer",
			})
			return err
		}},
		{"request decline", func() error {
			_, err := st.DeclineMessageRequest(ctx, principal, requestID, DeclineMessageRequestInput{})
			return err
		}},
		{"request select", func() error {
			_, err := st.SelectMessageRequest(ctx, principal, requestID, SelectMessageRequestInput{
				SelectedAgentIDs: []string{recipient.ID}, IdempotencyKey: "disabled-select",
			})
			return err
		}},
		{"request cancel", func() error {
			_, err := st.CancelMessageRequest(ctx, principal, requestID)
			return err
		}},
		{"request claim", func() error {
			_, err := st.ClaimMessageRequest(ctx, principal, requestID, ClaimMessageRequestInput{
				IdempotencyKey: "disabled-request-claim",
			})
			return err
		}},
		{"request renew", func() error {
			_, err := st.RenewMessageRequest(ctx, principal, requestID, RenewMessageRequestInput{
				ClaimID: requestClaimID, Generation: 1,
			})
			return err
		}},
		{"request release", func() error {
			_, err := st.ReleaseMessageRequest(ctx, principal, requestID, ReleaseMessageRequestInput{
				ClaimID: requestClaimID, Generation: 1,
			})
			return err
		}},
		{"request complete", func() error {
			_, err := st.CompleteMessageRequest(ctx, principal, requestID, CompleteMessageRequestInput{
				ClaimID: requestClaimID, Generation: 1, Body: "done",
				IdempotencyKey: "disabled-request-complete",
			})
			return err
		}},
	}
	for _, operation := range disabledOperations {
		t.Run(operation.name, func(t *testing.T) {
			err := operation.call()
			var featureErr *FeatureNotEnabledError
			if !errors.Is(err, ErrFeatureNotEnabled) || !errors.As(err, &featureErr) ||
				featureErr.Feature != plans.MessagingFeature {
				t.Fatalf("error = %v, want messaging FeatureNotEnabledError", err)
			}
		})
	}

	var messages, requests int
	if err := st.pool.QueryRow(ctx,
		`SELECT count(*) FROM agent_messages WHERE account_id=$1`, provisioned.AccountID).Scan(&messages); err != nil {
		t.Fatal(err)
	}
	if err := st.pool.QueryRow(ctx,
		`SELECT count(*) FROM agent_message_requests WHERE account_id=$1`, provisioned.AccountID).Scan(&requests); err != nil {
		t.Fatal(err)
	}
	if messages != 0 || requests != 0 {
		t.Fatalf("disabled operations persisted messages/requests = %d/%d", messages, requests)
	}
	checkpoint, err := st.GetSelfMessageCheckpoint(ctx, principal)
	if err != nil {
		t.Fatal(err)
	}
	if checkpoint.Enabled || checkpoint.Pending {
		t.Fatalf("disabled checkpoint = %+v", checkpoint)
	}

	indefiniteMessagePolicies := map[string]int64{
		TranscriptRetentionDaysPolicy:           30,
		plans.MessagingEntitlementVersionPolicy: plans.MessagingEntitlementVersion,
	}
	applyPlan(3, indefiniteMessagePolicies, []string{"memory", "facts", plans.MessagingFeature})
	msg, err := st.SendMessage(ctx, principal, SendMessageInput{
		ToAgent: recipient.ID, Body: "enabled", IdempotencyKey: "enabled-send",
	})
	if err != nil || msg.ID == "" {
		t.Fatalf("enabled send = %+v / %v", msg, err)
	}
	checkpoint, err = st.GetSelfMessageCheckpoint(ctx, principal)
	if err != nil || !checkpoint.Enabled {
		t.Fatalf("enabled checkpoint = %+v / %v", checkpoint, err)
	}
	pending, err := st.CaptureMemory(ctx, principal, CaptureMemoryInput{
		Content: "pending message evidence",
		Kind:    "decision",
		Evidence: []MemoryEvidenceInput{{
			ResolutionState: MemoryEvidencePending,
			ExternalLocator: "witself://message/pending",
		}},
		IdempotencyKey: "enabled-pending-message-evidence",
	})
	if err != nil {
		t.Fatal(err)
	}
	pendingEvidenceID := pending.Memory.Evidence[0].ID

	applyPlan(4, messagePolicies, []string{"memory", "facts"})
	if _, err := st.ListMessages(ctx, principal, MessageFilter{}); !errors.Is(err, ErrFeatureNotEnabled) {
		t.Fatalf("post-transition list error = %v", err)
	}
	if _, err := st.CaptureMemory(ctx, principal, CaptureMemoryInput{
		Content: "must roll back with disabled message evidence",
		Kind:    "decision",
		Evidence: []MemoryEvidenceInput{{
			ResolutionState: MemoryEvidenceResolved,
			ResolvedKind:    "message",
			SourceMessageID: msg.ID,
		}},
		IdempotencyKey: "disabled-message-evidence-capture",
	}); !errors.Is(err, ErrFeatureNotEnabled) {
		t.Fatalf("disabled message-evidence capture error = %v", err)
	}
	if _, err := st.ResolveMemoryEvidence(
		ctx,
		principal,
		pendingEvidenceID,
		ResolveMemoryEvidenceInput{
			ResolvedKind:    "message",
			SourceMessageID: msg.ID,
			IdempotencyKey:  "disabled-message-evidence-resolve",
		},
	); !errors.Is(err, ErrFeatureNotEnabled) {
		t.Fatalf("disabled message-evidence resolution error = %v", err)
	}
	var memoryCount, terminalResolutionCount int
	if err := st.pool.QueryRow(ctx, `
		SELECT
		  (SELECT count(*) FROM memories WHERE account_id=$1),
		  (SELECT count(*) FROM memory_evidence
		    WHERE pending_evidence_id=$2
		      AND resolution_state IN ('resolved','unresolvable'))`,
		provisioned.AccountID,
		pendingEvidenceID,
	).Scan(&memoryCount, &terminalResolutionCount); err != nil {
		t.Fatal(err)
	}
	if memoryCount != 1 || terminalResolutionCount != 0 {
		t.Fatalf(
			"disabled message evidence persisted memory/terminal rows = %d/%d, want 1/0",
			memoryCount,
			terminalResolutionCount,
		)
	}
}
