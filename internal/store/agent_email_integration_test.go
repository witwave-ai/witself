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
)

func TestAgentEmailPilotPostgresLifecycle(t *testing.T) {
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
		"agent-email-pilot-lifecycle@witwave.ai", "agent email pilot lifecycle", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if activated, err := st.ActivateAccount(ctx, provisioned.AccountID); err != nil || !activated {
		t.Fatalf("activate = %v / %v", activated, err)
	}
	realm, err := st.CreateRealm(ctx, provisioned.AccountID, "email pilot")
	if err != nil {
		t.Fatal(err)
	}
	agents := make([]Agent, 0, 6)
	for _, name := range []string{"mail owner", "bystander", "worker three", "worker four", "worker five", "not enrolled"} {
		agent, err := st.CreateAgent(ctx, provisioned.AccountID, realm.ID, name)
		if err != nil {
			t.Fatal(err)
		}
		agents = append(agents, agent)
	}
	principal := func(agent Agent) Principal {
		return Principal{
			Kind: PrincipalAgent, ID: agent.ID, AccountID: provisioned.AccountID,
			RealmID: realm.ID, AgentName: agent.Name, AccountStatus: "active",
		}
	}
	owner := principal(agents[0])
	bystander := principal(agents[1])
	enrolled := make(map[string]bool, 5)
	for _, agent := range agents[:5] {
		enrolled[agent.ID] = true
	}
	scope := AgentEmailPilotScope{
		Enabled: true, Domain: "agent-mail.witwave.ai", Audience: "cell-pilot-1",
		RealmIDs: map[string]bool{realm.ID: true}, AgentIDs: enrolled,
	}
	disabledScope := scope
	disabledScope.Enabled = false

	if _, err := st.EnsureAgentEmailMailbox(ctx, disabledScope, provisioned.AccountID,
		realm.ID, owner.ID, ""); !errors.Is(err, ErrAgentEmailPilotDisabled) {
		t.Fatalf("disabled provisioning error = %v", err)
	}
	address, err := st.EnsureAgentEmailMailbox(ctx, scope, provisioned.AccountID,
		realm.ID, owner.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	retryAddress, err := st.EnsureAgentEmailMailbox(ctx, scope, provisioned.AccountID,
		realm.ID, owner.ID, "")
	if err != nil || retryAddress.ID != address.ID || retryAddress.MailboxID != address.MailboxID {
		t.Fatalf("idempotent address = %#v / %v", retryAddress, err)
	}
	if shown, err := st.GetAgentEmailAddress(ctx, scope, owner); err != nil || shown.Address != address.Address {
		t.Fatalf("shown address = %#v / %v", shown, err)
	}
	alternateEnrolled := make(map[string]bool, 5)
	for _, agent := range agents[1:6] {
		alternateEnrolled[agent.ID] = true
	}
	alternateScope := scope
	alternateScope.AgentIDs = alternateEnrolled
	unenrolledAddress, err := st.EnsureAgentEmailMailbox(ctx, alternateScope, provisioned.AccountID,
		realm.ID, agents[5].ID, "outside-pilot")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.GetAgentEmailAddress(ctx, scope, principal(agents[5])); !errors.Is(err, ErrAgentEmailPilotNotEnrolled) {
		t.Fatalf("unenrolled address access error = %v", err)
	}
	raw := []byte(strings.Join([]string{
		"From: Example Service <sender@example.com>",
		"To: " + address.Address,
		"Subject: Your verification code",
		"Message-ID: <pilot-1@example.com>",
		"MIME-Version: 1.0",
		"Content-Type: multipart/mixed; boundary=pilot-boundary",
		"",
		"--pilot-boundary",
		"Content-Type: text/plain; charset=utf-8",
		"",
		"Your expected verification code is 123456.",
		"--pilot-boundary",
		"Content-Type: application/octet-stream; name=ignored.bin",
		"Content-Disposition: attachment; filename=ignored.bin",
		"Content-Transfer-Encoding: base64",
		"",
		"c2VjcmV0IGF0dGFjaG1lbnQ=",
		"--pilot-boundary--",
		"",
	}, "\r\n"))
	ingest := func(recipient string, body []byte) (AgentEmailMessage, error) {
		digest := sha256.Sum256(body)
		return st.IngestAgentEmailPilot(ctx, scope, AgentEmailIngestInput{
			Relay: agentemail.RelayMetadata{
				Timestamp: time.Now().Unix(), KeyID: "pilot-key-1", Audience: scope.Audience,
				EnvelopeSender: "sender@example.com", EnvelopeRecipient: recipient,
				RawSize: int64(len(body)), RawSHA256: hex.EncodeToString(digest[:]),
			},
			Raw: body,
		})
	}
	taggedAddress := strings.Replace(address.Address, "@", "+login@", 1)
	first, err := ingest(taggedAddress, raw)
	if err != nil {
		t.Fatal(err)
	}
	if first.PossibleDuplicate || first.AttachmentCount != 1 ||
		first.SenderVerificationState != AgentEmailSenderUnverified ||
		first.SPFResult != "unknown" || first.DKIMResult != "unknown" ||
		first.DMARCResult != "unknown" || first.SpamVerdict != "unknown" {
		t.Fatalf("first ingest = %#v", first)
	}
	second, err := ingest(taggedAddress, raw)
	if err != nil {
		t.Fatal(err)
	}
	if !second.PossibleDuplicate || second.PossibleDuplicateOfMessage != first.ID || second.ID == first.ID {
		t.Fatalf("suspected duplicate = %#v", second)
	}
	if _, err := ingest("unknown."+address.RealmLabel+"@"+address.Domain, raw); !errors.Is(err, ErrAgentEmailUnknownRecipient) {
		t.Fatalf("unknown recipient error = %v", err)
	}
	if _, err := ingest(unenrolledAddress.Address, raw); !errors.Is(err, ErrAgentEmailPilotNotEnrolled) {
		t.Fatalf("unenrolled recipient error = %v", err)
	}
	unsupportedTransfer := []byte(strings.Join([]string{
		"Subject: unsupported transfer",
		"Content-Type: text/plain; charset=utf-8",
		"Content-Transfer-Encoding: attacker-chosen",
		"",
		"body",
	}, "\r\n"))
	parseFailure, err := ingest(address.Address, unsupportedTransfer)
	if err != nil || parseFailure.ParseState != AgentEmailParseError ||
		parseFailure.ParseErrorCode != "transfer_encoding" {
		t.Fatalf("bounded-text ingest validation = %#v / %v", parseFailure, err)
	}
	digest := sha256.Sum256(raw)
	if _, err := st.IngestAgentEmailPilot(ctx, disabledScope, AgentEmailIngestInput{
		Relay: agentemail.RelayMetadata{
			Timestamp: time.Now().Unix(), KeyID: "pilot-key-1", Audience: scope.Audience,
			EnvelopeSender: "sender@example.com", EnvelopeRecipient: address.Address,
			RawSize: int64(len(raw)), RawSHA256: hex.EncodeToString(digest[:]),
		}, Raw: raw,
	}); !errors.Is(err, ErrAgentEmailPilotDisabled) {
		t.Fatalf("disabled pilot error = %v", err)
	}

	if _, err := st.ListAgentEmails(ctx, disabledScope, owner, AgentEmailFilter{Limit: 10}); !errors.Is(err, ErrAgentEmailPilotDisabled) {
		t.Fatalf("disabled list error = %v", err)
	}
	page, err := st.ListAgentEmails(ctx, scope, owner, AgentEmailFilter{Unacked: true, Limit: 10})
	if err != nil || len(page.Messages) != 3 {
		t.Fatalf("owner page = %#v / %v", page, err)
	}
	for _, message := range page.Messages {
		if message.Text != "" || message.TextKind != "" || len(message.rawMIME) != 0 ||
			message.Processing.ClaimID != "" || message.Processing.LeaseExpiresAt != nil {
			t.Fatalf("metadata projection leaked content or fence: %#v", message)
		}
	}
	if otherPage, err := st.ListAgentEmails(ctx, scope, bystander, AgentEmailFilter{Limit: 10}); err != nil || len(otherPage.Messages) != 0 {
		t.Fatalf("bystander page = %#v / %v", otherPage, err)
	}
	if _, err := st.ReadAgentEmail(ctx, scope, bystander, first.ID); !errors.Is(err, ErrAgentEmailNotFound) {
		t.Fatalf("bystander read error = %v", err)
	}
	read, err := st.ReadAgentEmail(ctx, scope, owner, first.ID)
	if err != nil || !strings.Contains(read.Text, "123456") || strings.Contains(read.Text, "secret attachment") ||
		read.TextKind != "text/plain" || read.ReadState.ReadAt == nil {
		t.Fatalf("explicit read = %#v / %v", read, err)
	}
	if _, err := st.MarkAgentEmailCodeConsumed(ctx, scope, owner, first.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.MarkAgentEmailCodeConsumed(ctx, scope, owner, first.ID); !errors.Is(err, ErrAgentEmailCodeConsumed) {
		t.Fatalf("repeat code consumption error = %v", err)
	}
	if checkpoint, err := st.GetSelfAgentEmailCheckpoint(ctx, scope, owner); err != nil || !checkpoint.Pending {
		t.Fatalf("pending checkpoint = %#v / %v", checkpoint, err)
	}

	claim, err := st.ClaimAgentEmail(ctx, scope, owner, second.ID, ClaimAgentEmailInput{
		LeaseDuration: 5 * time.Minute, IdempotencyKey: "email-claim-1",
	})
	if err != nil || claim.State != AgentEmailProcessingClaimed || claim.Generation != 1 ||
		claim.ClaimID == "" || claim.LeaseExpiresAt == nil {
		t.Fatalf("claim = %#v / %v", claim, err)
	}
	claimRetry, err := st.ClaimAgentEmail(ctx, scope, owner, second.ID, ClaimAgentEmailInput{
		LeaseDuration: 15 * time.Minute, IdempotencyKey: "email-claim-1",
	})
	if err != nil || claimRetry.ClaimID != claim.ClaimID ||
		claimRetry.Generation != claim.Generation || !claimRetry.LeaseExpiresAt.Equal(*claim.LeaseExpiresAt) {
		t.Fatalf("claim retry = %#v / %v", claimRetry, err)
	}
	if _, err := st.ClaimAgentEmail(ctx, scope, owner, second.ID, ClaimAgentEmailInput{
		IdempotencyKey: "email-claim-other",
	}); !errors.Is(err, ErrAgentEmailBusy) {
		t.Fatalf("competing claim error = %v", err)
	}
	if _, err := st.AckAgentEmail(ctx, scope, owner, second.ID); !errors.Is(err, ErrAgentEmailBusy) {
		t.Fatalf("ack claimed email error = %v", err)
	}
	claimedPage, err := st.ListAgentEmails(ctx, scope, owner, AgentEmailFilter{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	for _, message := range claimedPage.Messages {
		if message.ID == second.ID && (message.Processing.ClaimID != "" || message.Processing.LeaseExpiresAt != nil) {
			t.Fatalf("claimed list projection exposed fence: %#v", message)
		}
	}
	renewed, err := st.RenewAgentEmailClaim(ctx, scope, owner, second.ID, RenewAgentEmailClaimInput{
		ClaimID: claim.ClaimID, Generation: claim.Generation, LeaseDuration: 10 * time.Minute,
	})
	if err != nil || renewed.LeaseExpiresAt == nil || !renewed.LeaseExpiresAt.After(*claim.LeaseExpiresAt) {
		t.Fatalf("renew = %#v / %v", renewed, err)
	}
	released, err := st.ReleaseAgentEmailClaim(ctx, scope, owner, second.ID, ReleaseAgentEmailClaimInput{
		ClaimID: claim.ClaimID, Generation: claim.Generation, DeterministicFailure: true,
	})
	if err != nil || released.State != AgentEmailProcessingAvailable || released.Generation != 1 || released.FailureCount != 1 {
		t.Fatalf("release = %#v / %v", released, err)
	}
	if _, err := st.ReleaseAgentEmailClaim(ctx, scope, owner, second.ID, ReleaseAgentEmailClaimInput{
		ClaimID: claim.ClaimID, Generation: claim.Generation,
	}); !errors.Is(err, ErrAgentEmailClaimLost) {
		t.Fatalf("stale release error = %v", err)
	}
	secondClaim, err := st.ClaimAgentEmail(ctx, scope, owner, second.ID, ClaimAgentEmailInput{
		IdempotencyKey: "email-claim-2",
	})
	if err != nil || secondClaim.Generation != 2 || secondClaim.FailureCount != 1 {
		t.Fatalf("second claim = %#v / %v", secondClaim, err)
	}
	completed, err := st.CompleteAgentEmail(ctx, scope, owner, second.ID, CompleteAgentEmailInput{
		ClaimID: secondClaim.ClaimID, Generation: secondClaim.Generation,
		IdempotencyKey: "email-complete-1",
	})
	if err != nil || completed.State != AgentEmailProcessingCompleted || completed.CompletedAt == nil {
		t.Fatalf("complete = %#v / %v", completed, err)
	}
	if retry, err := st.CompleteAgentEmail(ctx, scope, owner, second.ID, CompleteAgentEmailInput{
		ClaimID: secondClaim.ClaimID, Generation: secondClaim.Generation,
		IdempotencyKey: "email-complete-1",
	}); err != nil || retry.State != AgentEmailProcessingCompleted {
		t.Fatalf("complete retry = %#v / %v", retry, err)
	}
	if _, err := st.CompleteAgentEmail(ctx, scope, owner, second.ID, CompleteAgentEmailInput{
		ClaimID: secondClaim.ClaimID, Generation: secondClaim.Generation,
		IdempotencyKey: "email-complete-other",
	}); !errors.Is(err, ErrAgentEmailConflict) {
		t.Fatalf("changed completion key error = %v", err)
	}
	if _, err := st.AckAgentEmail(ctx, scope, owner, first.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AckAgentEmail(ctx, scope, owner, second.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AckAgentEmail(ctx, scope, owner, parseFailure.ID); err != nil {
		t.Fatal(err)
	}
	if checkpoint, err := st.GetSelfAgentEmailCheckpoint(ctx, scope, owner); err != nil || checkpoint.Pending {
		t.Fatalf("cleared checkpoint = %#v / %v", checkpoint, err)
	}
	var emailEvents, leakedEmailEvents int
	if err := st.pool.QueryRow(ctx, `
		SELECT count(*),count(*) FILTER (WHERE metadata ?| ARRAY[
		  'raw_mime','text','body','header_from','header_to','header_subject','subject',
		  'envelope_sender','envelope_recipient','claim_id','claim_key_hash',
		  'complete_key_hash','idempotency_key'])
		FROM account_events
		WHERE account_id=$1 AND verb LIKE 'agent_email.%'`, provisioned.AccountID).
		Scan(&emailEvents, &leakedEmailEvents); err != nil {
		t.Fatal(err)
	}
	if emailEvents != 17 || leakedEmailEvents != 0 {
		t.Fatalf("email audit events = %d leaked=%d, want 17/0", emailEvents, leakedEmailEvents)
	}

	if err := st.DeleteAgent(ctx, provisioned.AccountID, realm.ID, owner.ID); err != nil {
		t.Fatal(err)
	}
	var receiveState string
	var addressRetired, mailboxRetired *time.Time
	var reason string
	if err := st.pool.QueryRow(ctx, `
		SELECT mb.receive_state,addr.retired_at,mb.retired_at,addr.retirement_reason_code
		FROM agent_email_addresses addr JOIN agent_email_mailboxes mb ON mb.address_id=addr.id
		WHERE addr.id=$1`, address.ID).Scan(&receiveState, &addressRetired, &mailboxRetired, &reason); err != nil {
		t.Fatal(err)
	}
	if receiveState != AgentEmailReceiveRetired || addressRetired == nil || mailboxRetired == nil || reason != "agent_deleted" {
		t.Fatalf("retired mailbox = %q/%v/%v/%q", receiveState, addressRetired, mailboxRetired, reason)
	}
	if _, err := st.EnsureAgentEmailMailbox(ctx, scope, provisioned.AccountID,
		realm.ID, owner.ID, ""); !errors.Is(err, ErrAgentNotFound) {
		t.Fatalf("deleted owner reprovision error = %v", err)
	}
}
