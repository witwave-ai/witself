package store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	archiveexport "github.com/witwave-ai/witself/internal/export"
	"github.com/witwave-ai/witself/internal/testenv"
)

func TestAgentEmailArchiveCellMovePostgres(t *testing.T) {
	dsn := testenv.RequirePostgres(t)
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
	legacyOwnerAddress := ownerAddress.Address
	formerAddress, err := source.EnsureAgentEmailMailbox(ctx, scope, provisioned.AccountID,
		realm.ID, former.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	legacyFormerAddress := formerAddress.Address
	scope.Domain = "witmail.net"
	scope.LegacyDomains = []string{"agent-mail.witwave.ai"}
	ownerAddress, err = source.EnsureAgentEmailMailbox(ctx, scope, provisioned.AccountID,
		realm.ID, owner.ID, "")
	if err != nil || ownerAddress.Address == legacyOwnerAddress || len(ownerAddress.Addresses) != 2 {
		t.Fatalf("transition owner address = %+v / %v", ownerAddress, err)
	}
	formerAddress, err = source.EnsureAgentEmailMailbox(ctx, scope, provisioned.AccountID,
		realm.ID, former.ID, "")
	if err != nil || formerAddress.Address == legacyFormerAddress || len(formerAddress.Addresses) != 2 {
		t.Fatalf("transition former address = %+v / %v", formerAddress, err)
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
		messageC  = "emsg_cccccccccccccccc"
		challenge = "11111111-2222-4333-8444-555555555555"
	)
	raw := []byte(strings.Join([]string{
		"From: sender@example.com",
		"To: owner@example.com",
		"X-Witself-Canary-Retry: " + challenge,
		"Subject: portable",
		"MIME-Version: 1.0",
		"Content-Type: multipart/mixed; boundary=mix",
		"",
		"--mix",
		"Content-Type: text/plain; charset=utf-8",
		"",
		"code 123456",
		"--mix",
		"Content-Type: application/octet-stream",
		"Content-Disposition: attachment; filename=portable.bin",
		"",
		"portable attachment",
		"--mix--",
		"",
	}, "\r\n"))
	digest := sha256.Sum256(raw)
	rawSHA := hex.EncodeToString(digest[:])
	legacyDuplicateGroup := agentEmailDuplicateGroup(rawSHA, legacyOwnerAddress, "sender@example.com")
	primaryDuplicateGroup := agentEmailDuplicateGroup(rawSHA, ownerAddress.Address, "sender@example.com")
	insertMessage := func(id, recipient, duplicateGroup, possibleDuplicate string) {
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
			   'sender@example.com','owner@example.com','portable',NULL,NULL,1,
			   'unknown','unknown','unknown','unknown','unverified',$13,$14,
			   clock_timestamp())`, id, provisioned.AccountID, realm.ID,
			ownerAddress.MailboxID, owner.ID, ownerAddress.ID, recipient,
			ownerAddress.AgentSegment, ownerAddress.RealmLabel, raw, len(raw), rawSHA,
			duplicateGroup, duplicate); err != nil {
			t.Fatal(err)
		}
	}
	insertMessage(messageA, legacyOwnerAddress, legacyDuplicateGroup, "")
	insertMessage(messageB, legacyOwnerAddress, legacyDuplicateGroup, messageA)
	if _, err := source.pool.Exec(ctx, `
		INSERT INTO agent_email_messages
		  (id,account_id,realm_id,mailbox_id,owner_agent_id,address_id,
		   provider,provider_message_id,envelope_sender,envelope_recipient,
		   agent_segment,realm_label,subaddress_tag,raw_mime,raw_size_bytes,
		   raw_sha256,parse_state,parse_error,header_from,header_to,
		   header_subject,mime_message_id,message_date,attachment_count,
		   body_text,body_text_kind,attachment_storage_bytes,
		   retained_attachment_storage_bytes,payload_retention_state,
		   spf_result,dkim_result,dmarc_result,spam_verdict,
		   sender_verification_state,duplicate_group_sha256,
		   possible_duplicate_of_message_id,received_at)
		VALUES
		  ($1,$2,$3,$4,$5,$6,'cloudflare_email_routing',NULL,
		   'sender@example.com',$7,$8,$9,NULL,NULL,$10,$11,'parsed',NULL,
		   'sender@example.com','owner@example.com','portable',NULL,NULL,1,
		   'code 123456','text/plain',$12,0,'omitted_capacity',
		   'unknown','unknown','unknown','unknown','unverified',$13,$14,
		   clock_timestamp())`,
		messageC, provisioned.AccountID, realm.ID, ownerAddress.MailboxID,
		owner.ID, ownerAddress.ID, ownerAddress.Address, ownerAddress.AgentSegment,
		ownerAddress.RealmLabel, len(raw), rawSHA, len(raw), primaryDuplicateGroup,
		nil,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := source.pool.Exec(ctx, `
		INSERT INTO agent_email_deliveries
		  (message_id,account_id,realm_id,mailbox_id,owner_agent_id,
		   processing_state,processing_generation,failure_count,
		   claim_id,claim_key_hash,lease_expires_at)
		VALUES
		  ($1,$4,$5,$6,$7,'claimed',4,2,'ecl_aaaaaaaaaaaaaaaa',$8,
		   clock_timestamp()+interval '10 minutes'),
		  ($2,$4,$5,$6,$7,'available',0,0,NULL,'',NULL),
		  ($3,$4,$5,$6,$7,'available',0,0,NULL,'',NULL)`,
		messageA, messageB, messageC, provisioned.AccountID, realm.ID,
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
		hex.EncodeToString(challengeDigest[:]), legacyDuplicateGroup, messageA); err != nil {
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
	var archivedAddressDomains int
	archivedEnvelopeDomains := map[string]int{}
	if _, err := archiveexport.Read(ctx, bytes.NewReader(archiveBytes), archiveexport.ImportOptions{
		CurrentSchema: SchemaVersion(),
		Row: func(table string, row []byte) error {
			var object map[string]any
			switch table {
			case "accounts":
				if err := json.Unmarshal(row, &object); err != nil {
					return err
				}
				if _, archived := object["retained_agent_email_attachment_bytes"]; archived {
					t.Fatal("derived retained_agent_email_attachment_bytes was archived")
				}
			case "agent_email_messages":
				archivedMessages++
				if err := json.Unmarshal(row, &object); err != nil {
					return err
				}
				recipient, _ := object["envelope_recipient"].(string)
				if at := strings.LastIndexByte(recipient, '@'); at >= 0 {
					archivedEnvelopeDomains[recipient[at+1:]]++
				}
				if object["id"] == messageC {
					if object["raw_mime"] != nil || object["body_text"] != "code 123456" ||
						object["body_text_kind"] != "text/plain" ||
						object["attachment_storage_bytes"] != float64(len(raw)) ||
						object["retained_attachment_storage_bytes"] != float64(0) ||
						object["payload_retention_state"] != importedAgentEmailPayloadOmittedCapacity {
						t.Fatalf("archived omitted payload projection = %#v", object)
					}
				} else {
					if object["raw_mime"] != `\x`+hex.EncodeToString(raw) {
						t.Fatalf("archived raw_mime = %#v", object["raw_mime"])
					}
					if object["body_text"] != nil || object["body_text_kind"] != nil ||
						object["attachment_storage_bytes"] != float64(len(raw)) ||
						object["retained_attachment_storage_bytes"] != float64(len(raw)) ||
						object["payload_retention_state"] != importedAgentEmailPayloadRetained {
						t.Fatalf("archived retained payload projection = %#v", object)
					}
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
			case "agent_email_address_domains":
				archivedAddressDomains++
			}
			return nil
		},
	}); err != nil {
		t.Fatal(err)
	}
	if archivedMessages != 3 {
		t.Fatalf("archived agent-email messages = %d, want 3", archivedMessages)
	}
	if archivedReceiveControls != 1 {
		t.Fatalf("archived realm receive controls = %d, want 1", archivedReceiveControls)
	}
	if archivedCanaryProofs != 1 {
		t.Fatalf("archived retry canary proofs = %d, want 1", archivedCanaryProofs)
	}
	if archivedAddressDomains != 4 {
		t.Fatalf("archived agent-email address domains = %d, want 4", archivedAddressDomains)
	}
	if archivedEnvelopeDomains["agent-mail.witwave.ai"] != 2 ||
		archivedEnvelopeDomains["witmail.net"] != 1 {
		t.Fatalf("archived envelope domains = %#v", archivedEnvelopeDomains)
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
		provisioned.AccountID, legacyDuplicateGroup).Scan(&duplicateCount); err != nil {
		t.Fatal(err)
	}
	if duplicateCount != 2 {
		t.Fatalf("restored suspected duplicate rows = %d, want 2", duplicateCount)
	}
	var restoredPrimaryEnvelopes, restoredLegacyEnvelopes int
	if err := destination.pool.QueryRow(ctx, `
		SELECT
		  count(*) FILTER (WHERE envelope_recipient LIKE '%@witmail.net'),
		  count(*) FILTER (WHERE envelope_recipient LIKE '%@agent-mail.witwave.ai')
		FROM agent_email_messages WHERE account_id=$1`, provisioned.AccountID).
		Scan(&restoredPrimaryEnvelopes, &restoredLegacyEnvelopes); err != nil {
		t.Fatal(err)
	}
	if restoredPrimaryEnvelopes != 1 || restoredLegacyEnvelopes != 2 {
		t.Fatalf("restored envelope domains primary=%d legacy=%d",
			restoredPrimaryEnvelopes, restoredLegacyEnvelopes)
	}
	var omittedRaw []byte
	var omittedBody, omittedBodyKind *string
	var omittedStorage, omittedRetained int64
	var omittedState string
	if err := destination.pool.QueryRow(ctx, `
		SELECT raw_mime,body_text,body_text_kind,attachment_storage_bytes,
		       retained_attachment_storage_bytes,payload_retention_state
		FROM agent_email_messages WHERE id=$1 AND account_id=$2`,
		messageC, provisioned.AccountID,
	).Scan(&omittedRaw, &omittedBody, &omittedBodyKind, &omittedStorage,
		&omittedRetained, &omittedState); err != nil {
		t.Fatal(err)
	}
	if omittedRaw != nil || omittedBody == nil || *omittedBody != "code 123456" ||
		omittedBodyKind == nil || *omittedBodyKind != "text/plain" ||
		omittedStorage != int64(len(raw)) || omittedRetained != 0 ||
		omittedState != importedAgentEmailPayloadOmittedCapacity {
		t.Fatalf("restored omitted payload raw=%v body=%v kind=%v storage=%d retained=%d state=%q",
			omittedRaw, omittedBody, omittedBodyKind, omittedStorage, omittedRetained, omittedState)
	}
	var retainedAttachmentBytes int64
	if err := destination.pool.QueryRow(ctx, `
		SELECT retained_agent_email_attachment_bytes
		FROM accounts WHERE id=$1`,
		provisioned.AccountID,
	).Scan(&retainedAttachmentBytes); err != nil {
		t.Fatal(err)
	}
	if retainedAttachmentBytes != int64(2*len(raw)) {
		t.Fatalf("restored retained attachment bytes = %d, want %d",
			retainedAttachmentBytes, 2*len(raw))
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
	var ownerRoutes, formerRoutes int
	if err := destination.pool.QueryRow(ctx, `
		SELECT
		  count(*) FILTER (WHERE address_id=$1),
		  count(*) FILTER (WHERE address_id=$2)
		FROM agent_email_address_domains WHERE account_id=$3`,
		ownerAddress.ID, formerAddress.ID, provisioned.AccountID).
		Scan(&ownerRoutes, &formerRoutes); err != nil {
		t.Fatal(err)
	}
	if ownerRoutes != 2 || formerRoutes != 2 {
		t.Fatalf("restored address routes owner=%d former=%d", ownerRoutes, formerRoutes)
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

	// A retired route remains a global domain/local-part tombstone after a
	// cell move. Recreating the former segment on the primary domain must fail
	// atomically even though the original address row was on the legacy domain.
	if err := destination.ResumeAccountSystem(ctx, provisioned.AccountID, "evacuation"); err != nil {
		t.Fatal(err)
	}
	replacement, err := destination.CreateAgent(
		ctx, provisioned.AccountID, realm.ID, "former",
	)
	if err != nil {
		t.Fatal(err)
	}
	replacementScope := scope
	replacementScope.AgentIDs = make(map[string]bool, len(enrolled))
	for agentID := range enrolled {
		if agentID != former.ID {
			replacementScope.AgentIDs[agentID] = true
		}
	}
	replacementScope.AgentIDs[replacement.ID] = true
	if _, err := destination.EnsureAgentEmailMailbox(
		ctx, replacementScope, provisioned.AccountID, realm.ID, replacement.ID, "former",
	); !errors.Is(err, ErrAgentEmailAddressConflict) {
		t.Fatalf("retired cross-domain address reuse error = %v", err)
	}
	var replacementAddresses int
	if err := destination.pool.QueryRow(ctx, `
		SELECT count(*) FROM agent_email_addresses
		WHERE account_id=$1 AND realm_id=$2 AND provisioned_agent_id=$3`,
		provisioned.AccountID, realm.ID, replacement.ID).Scan(&replacementAddresses); err != nil {
		t.Fatal(err)
	}
	if replacementAddresses != 0 {
		t.Fatalf("conflicted replacement left %d address rows", replacementAddresses)
	}
}

func TestLegacyAgentEmailArchiveImportScopesReceiveControlSynthesisPostgres(t *testing.T) {
	dsn := testenv.RequirePostgres(t)
	ctx := context.Background()
	source, _ := newMigrationTestStore(t, dsn)
	destination, _ := newMigrationTestStore(t, dsn)
	if err := source.Migrate(); err != nil {
		t.Fatal(err)
	}
	if err := destination.Migrate(); err != nil {
		t.Fatal(err)
	}

	createLegacyMailbox := func(t *testing.T, st *Store, email, accountName, realmName, agentName, addressID, mailboxID string) (string, string) {
		t.Helper()
		provisioned, err := st.ProvisionAccount(ctx, email, accountName, time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		if activated, err := st.ActivateAccount(ctx, provisioned.AccountID); err != nil || !activated {
			t.Fatalf("activate = %t / %v", activated, err)
		}
		realm, err := st.CreateRealm(ctx, provisioned.AccountID, realmName)
		if err != nil {
			t.Fatal(err)
		}
		agent, err := st.CreateAgent(ctx, provisioned.AccountID, realm.ID, agentName)
		if err != nil {
			t.Fatal(err)
		}
		realmLabel := strings.TrimPrefix(realm.ID, "realm_")
		agentSegment := strings.ReplaceAll(agentName, " ", "-")
		if _, err := st.pool.Exec(ctx, `
			INSERT INTO agent_email_addresses
			  (id,account_id,realm_id,provisioned_agent_id,domain,agent_segment,
			   realm_label,local_part,provisioning_kind)
			VALUES ($1,$2,$3,$4,'agent-mail.witwave.ai',$5,$6,$5 || '.' || $6,'derived')`,
			addressID, provisioned.AccountID, realm.ID, agent.ID, agentSegment, realmLabel); err != nil {
			t.Fatal(err)
		}
		if _, err := st.pool.Exec(ctx, `
			INSERT INTO agent_email_mailboxes
			  (id,account_id,realm_id,owner_agent_id,address_id,receive_state)
			VALUES ($1,$2,$3,$4,$5,'enabled')`,
			mailboxID, provisioned.AccountID, realm.ID, agent.ID, addressID); err != nil {
			t.Fatal(err)
		}
		return provisioned.AccountID, realm.ID
	}

	importedAccountID, importedRealmID := createLegacyMailbox(t, source,
		"legacy-email-import-source@witwave.ai", "legacy email import source",
		"legacy source", "legacy source", "eaddr_aaaaaaaaaaaaaaaa", "emb_aaaaaaaaaaaaaaaa")
	if err := source.SuspendAccountSystem(ctx, importedAccountID,
		"evacuation", "legacy schema 59 import isolation"); err != nil {
		t.Fatal(err)
	}
	var current bytes.Buffer
	if err := source.ExportAccount(ctx, importedAccountID, "legacy-source", "test", &current); err != nil {
		t.Fatal(err)
	}
	manifest, rows := readAvatarArchiveRows(t, current.Bytes(), SchemaVersion())
	legacy := writeAvatarArchiveRows(t, archiveexport.Manifest{
		SchemaVersion: 59, ServerVersion: manifest.ServerVersion,
		AccountID: importedAccountID, Cell: manifest.Cell,
		Status: manifest.Status, ExportedAt: manifest.ExportedAt,
	}, canonicalArchiveTableNamesForSchema(59), rows)

	unrelatedAccountID, unrelatedRealmID := createLegacyMailbox(t, destination,
		"legacy-email-import-unrelated@witwave.ai", "legacy email import unrelated",
		"unrelated destination", "unrelated agent", "eaddr_bbbbbbbbbbbbbbbb", "emb_bbbbbbbbbbbbbbbb")
	var unrelatedBefore int
	if err := destination.pool.QueryRow(ctx, `
		SELECT count(*) FROM agent_email_realm_receive_controls
		WHERE account_id=$1 AND realm_id=$2`, unrelatedAccountID, unrelatedRealmID).
		Scan(&unrelatedBefore); err != nil {
		t.Fatal(err)
	}
	if unrelatedBefore != 0 {
		t.Fatalf("unrelated receive controls before import = %d, want 0", unrelatedBefore)
	}

	imported, err := destination.ImportAccount(ctx, importedAccountID, bytes.NewReader(legacy))
	if err != nil {
		t.Fatal(err)
	}
	if imported.SchemaVersion != 59 {
		t.Fatalf("imported manifest schema = %d, want 59", imported.SchemaVersion)
	}
	var importedControls, unrelatedControls int
	if err := destination.pool.QueryRow(ctx, `
		SELECT
		  (SELECT count(*) FROM agent_email_realm_receive_controls
		   WHERE account_id=$1 AND realm_id=$2),
		  (SELECT count(*) FROM agent_email_realm_receive_controls
		   WHERE account_id=$3 AND realm_id=$4)`,
		importedAccountID, importedRealmID, unrelatedAccountID, unrelatedRealmID).
		Scan(&importedControls, &unrelatedControls); err != nil {
		t.Fatal(err)
	}
	if importedControls != 1 {
		t.Fatalf("imported account receive controls = %d, want 1", importedControls)
	}
	if unrelatedControls != 0 {
		t.Fatalf("unrelated account receive controls after import = %d, want 0", unrelatedControls)
	}
	var importedRoutes int
	var importedRouteDomain string
	if err := destination.pool.QueryRow(ctx, `
		SELECT count(*),COALESCE(min(domain),'')
		FROM agent_email_address_domains
		WHERE account_id=$1`, importedAccountID).
		Scan(&importedRoutes, &importedRouteDomain); err != nil {
		t.Fatal(err)
	}
	if importedRoutes != 1 || importedRouteDomain != "agent-mail.witwave.ai" {
		t.Fatalf("schema-59 synthesized routes=%d domain=%q",
			importedRoutes, importedRouteDomain)
	}
}
