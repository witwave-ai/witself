package store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	archiveexport "github.com/witwave-ai/witself/internal/export"
)

func TestAgentEmailArchiveCellMovePostgres(t *testing.T) {
	dsn := os.Getenv("WITSELF_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("WITSELF_TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	source, _ := newMigrationTestStore(t, dsn)
	destination, _ := newMigrationTestStore(t, dsn)
	if err := source.Migrate(); err != nil {
		t.Fatal(err)
	}
	if err := destination.Migrate(); err != nil {
		t.Fatal(err)
	}

	provisioned, err := source.ProvisionAccount(ctx,
		"agent-email-archive-source@witwave.ai", "agent email archive source", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if activated, err := source.ActivateAccount(ctx, provisioned.AccountID); err != nil || !activated {
		t.Fatalf("activate = %v / %v", activated, err)
	}
	realm, err := source.CreateRealm(ctx, provisioned.AccountID, "email archive")
	if err != nil {
		t.Fatal(err)
	}
	owner, err := source.CreateAgent(ctx, provisioned.AccountID, realm.ID, "owner")
	if err != nil {
		t.Fatal(err)
	}
	former, err := source.CreateAgent(ctx, provisioned.AccountID, realm.ID, "former")
	if err != nil {
		t.Fatal(err)
	}
	pilotAgents := []Agent{owner, former}
	for _, name := range []string{"pilot three", "pilot four", "pilot five"} {
		agent, err := source.CreateAgent(ctx, provisioned.AccountID, realm.ID, name)
		if err != nil {
			t.Fatal(err)
		}
		pilotAgents = append(pilotAgents, agent)
	}
	enrolled := make(map[string]bool, len(pilotAgents))
	for _, agent := range pilotAgents {
		enrolled[agent.ID] = true
	}
	scope := AgentEmailPilotScope{
		Enabled: true, Domain: "agent-mail.witwave.ai", Audience: "archive-pilot",
		RealmIDs: map[string]bool{realm.ID: true}, AgentIDs: enrolled,
	}
	ownerAddress, err := source.EnsureAgentEmailMailbox(ctx, scope, provisioned.AccountID,
		realm.ID, owner.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	formerAddress, err := source.EnsureAgentEmailMailbox(ctx, scope, provisioned.AccountID,
		realm.ID, former.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	realmControl, err := source.SetRealmAgentEmailReceiveControl(ctx, scope,
		provisioned.AccountID, provisioned.OperatorID, realm.ID, AgentEmailReceiveDisabled)
	if err != nil || realmControl.ReceiveState != AgentEmailReceiveDisabled || realmControl.RowVersion != 2 {
		t.Fatalf("disable source realm receive = %#v / %v", realmControl, err)
	}

	// Retire one address and permanently remove its original agent. Its mailbox
	// cascades, but the address reservation must remain in the archive without
	// relying on an agents row that no longer exists.
	if err := source.DeleteAgent(ctx, provisioned.AccountID, realm.ID, former.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := source.pool.Exec(ctx, `DELETE FROM agents WHERE id=$1 AND realm_id=$2`, former.ID, realm.ID); err != nil {
		t.Fatal(err)
	}

	const (
		messageA  = "emsg_aaaaaaaaaaaaaaaa"
		messageB  = "emsg_bbbbbbbbbbbbbbbb"
		challenge = "11111111-2222-4333-8444-555555555555"
	)
	raw := []byte("From: sender@example.com\r\nTo: owner@example.com\r\nX-Witself-Canary-Retry: " + challenge + "\r\nSubject: portable\r\n\r\ncode 123456\r\n")
	digest := sha256.Sum256(raw)
	rawSHA := hex.EncodeToString(digest[:])
	duplicateGroup := agentEmailDuplicateGroup(rawSHA, ownerAddress.Address, "sender@example.com")
	insertMessage := func(id, possibleDuplicate string) {
		t.Helper()
		var duplicate any
		if possibleDuplicate != "" {
			duplicate = possibleDuplicate
		}
		if _, err := source.pool.Exec(ctx, `
			INSERT INTO agent_email_messages
			  (id,account_id,realm_id,mailbox_id,owner_agent_id,address_id,
			   provider,provider_message_id,envelope_sender,envelope_recipient,
			   agent_segment,realm_label,subaddress_tag,raw_mime,raw_size_bytes,
			   raw_sha256,parse_state,parse_error,header_from,header_to,
			   header_subject,mime_message_id,message_date,attachment_count,
			   spf_result,dkim_result,dmarc_result,spam_verdict,
			   sender_verification_state,duplicate_group_sha256,
			   possible_duplicate_of_message_id,received_at)
			VALUES
			  ($1,$2,$3,$4,$5,$6,'cloudflare_email_routing',NULL,
			   'sender@example.com',$7,$8,$9,NULL,$10,$11,$12,'parsed',NULL,
			   'sender@example.com','owner@example.com','portable',NULL,NULL,0,
			   'unknown','unknown','unknown','unknown','unverified',$13,$14,
			   clock_timestamp())`, id, provisioned.AccountID, realm.ID,
			ownerAddress.MailboxID, owner.ID, ownerAddress.ID, ownerAddress.Address,
			ownerAddress.AgentSegment, ownerAddress.RealmLabel, raw, len(raw), rawSHA,
			duplicateGroup, duplicate); err != nil {
			t.Fatal(err)
		}
	}
	insertMessage(messageA, "")
	insertMessage(messageB, messageA)
	if _, err := source.pool.Exec(ctx, `
		INSERT INTO agent_email_deliveries
		  (message_id,account_id,realm_id,mailbox_id,owner_agent_id,
		   processing_state,processing_generation,failure_count,
		   claim_id,claim_key_hash,lease_expires_at)
		VALUES
		  ($1,$3,$4,$5,$6,'claimed',4,2,'ecl_aaaaaaaaaaaaaaaa',$7,
		   clock_timestamp()+interval '10 minutes'),
		  ($2,$3,$4,$5,$6,'available',0,0,NULL,'',NULL)`,
		messageA, messageB, provisioned.AccountID, realm.ID,
		ownerAddress.MailboxID, owner.ID, strings.Repeat("a", 64)); err != nil {
		t.Fatal(err)
	}
	challengeDigest := sha256.Sum256([]byte(challenge))
	if _, err := source.pool.Exec(ctx, `
		WITH anchor AS (SELECT clock_timestamp() AS at)
		INSERT INTO agent_email_retry_canary_arms
		  (account_id,realm_id,mailbox_id,owner_agent_id,challenge_sha256,
		   state,delivery_fingerprint_sha256,accepted_message_id,
		   tempfail_count,row_version,armed_at,expires_at,tempfailed_at,retry_expires_at,accepted_at)
		SELECT $1,$2,$3,$4,$5,'accepted',$6,$7,1,3,
		       at-interval '3 seconds',at+interval '14 minutes 57 seconds',
		       at-interval '2 seconds',at+interval '23 hours 59 minutes 58 seconds',
		       at-interval '1 second'
		FROM anchor`,
		provisioned.AccountID, realm.ID, ownerAddress.MailboxID, owner.ID,
		hex.EncodeToString(challengeDigest[:]), duplicateGroup, messageA); err != nil {
		t.Fatal(err)
	}

	if err := source.SuspendAccountSystem(ctx, provisioned.AccountID,
		"evacuation", "move agent email to another cell"); err != nil {
		t.Fatal(err)
	}
	var archive bytes.Buffer
	if err := source.ExportAccount(ctx, provisioned.AccountID, "email-source", "test", &archive); err != nil {
		t.Fatal(err)
	}
	archiveBytes := archive.Bytes()
	var archivedMessages, archivedReceiveControls, archivedCanaryProofs int
	if _, err := archiveexport.Read(ctx, bytes.NewReader(archiveBytes), archiveexport.ImportOptions{
		CurrentSchema: SchemaVersion(),
		Row: func(table string, row []byte) error {
			var object map[string]any
			switch table {
			case "agent_email_messages":
				archivedMessages++
				if err := json.Unmarshal(row, &object); err != nil {
					return err
				}
				if object["raw_mime"] != `\x`+hex.EncodeToString(raw) {
					t.Fatalf("archived raw_mime = %#v", object["raw_mime"])
				}
			case "agent_email_realm_receive_controls":
				archivedReceiveControls++
				if err := json.Unmarshal(row, &object); err != nil {
					return err
				}
				if object["realm_id"] != realm.ID || object["receive_state"] != AgentEmailReceiveDisabled {
					t.Fatalf("archived realm receive control = %#v", object)
				}
			case "agent_email_retry_canary_arms":
				archivedCanaryProofs++
				if err := json.Unmarshal(row, &object); err != nil {
					return err
				}
				if object["state"] != agentEmailRetryCanaryAccepted || object["accepted_message_id"] != messageA {
					t.Fatalf("archived retry canary proof = %#v", object)
				}
			}
			return nil
		},
	}); err != nil {
		t.Fatal(err)
	}
	if archivedMessages != 2 {
		t.Fatalf("archived agent-email messages = %d, want 2", archivedMessages)
	}
	if archivedReceiveControls != 1 {
		t.Fatalf("archived realm receive controls = %d, want 1", archivedReceiveControls)
	}
	if archivedCanaryProofs != 1 {
		t.Fatalf("archived retry canary proofs = %d, want 1", archivedCanaryProofs)
	}

	if _, err := destination.ImportAccount(ctx, provisioned.AccountID,
		bytes.NewReader(archiveBytes)); err != nil {
		t.Fatal(err)
	}
	var restoredRaw []byte
	var restoredPossible *string
	if err := destination.pool.QueryRow(ctx, `
		SELECT raw_mime,possible_duplicate_of_message_id
		FROM agent_email_messages WHERE id=$1 AND account_id=$2`,
		messageB, provisioned.AccountID).Scan(&restoredRaw, &restoredPossible); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(restoredRaw, raw) || restoredPossible == nil || *restoredPossible != messageA {
		t.Fatalf("restored duplicate raw=%q possible=%v", restoredRaw, restoredPossible)
	}
	var duplicateCount int
	if err := destination.pool.QueryRow(ctx, `
		SELECT count(*) FROM agent_email_messages
		WHERE account_id=$1 AND duplicate_group_sha256=$2`,
		provisioned.AccountID, duplicateGroup).Scan(&duplicateCount); err != nil {
		t.Fatal(err)
	}
	if duplicateCount != 2 {
		t.Fatalf("restored suspected duplicate rows = %d, want 2", duplicateCount)
	}
	var state string
	var generation, failures int64
	var claimID, claimHash *string
	var lease, completed *time.Time
	var completeHash string
	if err := destination.pool.QueryRow(ctx, `
		SELECT processing_state,processing_generation,failure_count,
		       claim_id,NULLIF(claim_key_hash,''),lease_expires_at,completed_at,
		       complete_key_hash
		FROM agent_email_deliveries WHERE message_id=$1 AND mailbox_id=$2`,
		messageA, ownerAddress.MailboxID).Scan(
		&state, &generation, &failures, &claimID, &claimHash, &lease, &completed,
		&completeHash); err != nil {
		t.Fatal(err)
	}
	if state != AgentEmailProcessingAvailable || generation != 5 || failures != 2 ||
		claimID != nil || claimHash != nil || lease != nil || completed != nil || completeHash != "" {
		t.Fatalf("restored active claim = state=%s generation=%d failures=%d claim=%v hash=%v lease=%v completed=%v complete_hash=%q",
			state, generation, failures, claimID, claimHash, lease, completed, completeHash)
	}
	var tombstones, formerAgents int
	if err := destination.pool.QueryRow(ctx, `
		SELECT
		  (SELECT count(*) FROM agent_email_addresses
		    WHERE id=$1 AND account_id=$2 AND provisioned_agent_id=$3
		      AND retired_at IS NOT NULL AND retirement_reason_code='agent_deleted'),
		  (SELECT count(*) FROM agents WHERE id=$3)`,
		formerAddress.ID, provisioned.AccountID, former.ID).Scan(&tombstones, &formerAgents); err != nil {
		t.Fatal(err)
	}
	if tombstones != 1 || formerAgents != 0 {
		t.Fatalf("restored tombstone rows=%d former agents=%d", tombstones, formerAgents)
	}
	var restoredAgentReceiveState, restoredRealmReceiveState string
	var restoredRealmRowVersion int64
	var restoredRealmDisabledAt *time.Time
	if err := destination.pool.QueryRow(ctx, `
		SELECT mb.receive_state,rc.receive_state,rc.row_version,rc.disabled_at
		FROM agent_email_mailboxes mb
		JOIN agent_email_realm_receive_controls rc
		  ON rc.account_id=mb.account_id AND rc.realm_id=mb.realm_id
		WHERE mb.account_id=$1 AND mb.realm_id=$2 AND mb.owner_agent_id=$3`,
		provisioned.AccountID, realm.ID, owner.ID).Scan(
		&restoredAgentReceiveState, &restoredRealmReceiveState,
		&restoredRealmRowVersion, &restoredRealmDisabledAt); err != nil {
		t.Fatal(err)
	}
	if restoredAgentReceiveState != AgentEmailReceiveEnabled ||
		restoredRealmReceiveState != AgentEmailReceiveDisabled ||
		restoredRealmRowVersion != 2 || restoredRealmDisabledAt == nil {
		t.Fatalf("restored receive layers = agent=%s realm=%s version=%d disabled_at=%v",
			restoredAgentReceiveState, restoredRealmReceiveState,
			restoredRealmRowVersion, restoredRealmDisabledAt)
	}
	var restoredCanaryState, restoredCanaryMessage string
	if err := destination.pool.QueryRow(ctx, `
		SELECT state,accepted_message_id
		FROM agent_email_retry_canary_arms
		WHERE account_id=$1 AND mailbox_id=$2`, provisioned.AccountID,
		ownerAddress.MailboxID).Scan(&restoredCanaryState, &restoredCanaryMessage); err != nil {
		t.Fatal(err)
	}
	if restoredCanaryState != agentEmailRetryCanaryAccepted || restoredCanaryMessage != messageA {
		t.Fatalf("restored retry canary = %q/%q", restoredCanaryState, restoredCanaryMessage)
	}
}
