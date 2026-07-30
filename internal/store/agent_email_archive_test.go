package store

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"maps"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/witwave-ai/witself/internal/agentemail"
	archiveexport "github.com/witwave-ai/witself/internal/export"
)

func TestAgentEmailArchiveClaimNormalizationConsumesFence(t *testing.T) {
	base := map[string]any{
		"processing_state": MessageProcessingClaimed, "processing_generation": 7,
		"failure_count": 2, "claim_id": "ecl_aaaaaaaaaaaaaaaa",
		"claim_key_hash":   strings.Repeat("a", 64),
		"lease_expires_at": "2026-07-21T13:00:00Z",
		"completed_at":     nil, "complete_key_hash": "",
	}
	row := maps.Clone(base)
	if err := newImportCtx("acc_1").normalizeImportedAgentEmailClaim("agent_email_deliveries", row); err != nil {
		t.Fatal(err)
	}
	if row["processing_state"] != MessageProcessingAvailable || row["processing_generation"] != int64(8) ||
		row["failure_count"] != 2 || row["claim_id"] != nil || row["claim_key_hash"] != "" ||
		row["lease_expires_at"] != nil || row["completed_at"] != nil || row["complete_key_hash"] != "" {
		t.Fatalf("normalized email claim = %#v", row)
	}
	if _, err := validateImportedAgentEmailProcessingShape(row); err != nil {
		t.Fatalf("normalized email claim is not available-shaped: %v", err)
	}

	completed := maps.Clone(base)
	completed["processing_state"] = MessageProcessingCompleted
	completed["lease_expires_at"] = nil
	completed["completed_at"] = "2026-07-21T13:00:00Z"
	completed["complete_key_hash"] = strings.Repeat("b", 64)
	before := maps.Clone(completed)
	if err := newImportCtx("acc_1").normalizeImportedAgentEmailClaim("agent_email_deliveries", completed); err != nil {
		t.Fatal(err)
	}
	if !maps.Equal(completed, before) {
		t.Fatalf("completed email processing changed on import: %#v", completed)
	}

	exhausted := maps.Clone(base)
	exhausted["processing_generation"] = maxMessageProcessingGeneration
	if err := newImportCtx("acc_1").normalizeImportedAgentEmailClaim("agent_email_deliveries", exhausted); !errors.Is(err, ErrArchiveContent) {
		t.Fatalf("exhausted active claim error = %v", err)
	}
}

func TestAgentEmailArchivePreservesTombstonesAndSuspectedDuplicates(t *testing.T) {
	const (
		accountID = "acc_1"
		realmID   = "realm_abcdefghijkl2345"
		agentID   = "agent_1"
		addressID = "eaddr_aaaaaaaaaaaaaaaa"
		mailboxID = "emb_aaaaaaaaaaaaaaaa"
		messageA  = "emsg_aaaaaaaaaaaaaaaa"
		messageB  = "emsg_bbbbbbbbbbbbbbbb"
	)
	ic := newImportCtx(accountID)
	ic.exportedAt = time.Date(2026, 7, 21, 14, 0, 0, 0, time.UTC)
	ic.realms[realmID] = true
	ic.agents[agentID] = true
	ic.liveAgents[agentID] = true
	ic.agentRealms[agentID] = realmID

	feedAgentEmailArchiveRow(t, ic, "agent_email_addresses", agentEmailArchiveAddressRow(
		accountID, realmID, agentID, addressID, false,
	))
	feedAgentEmailArchiveRow(t, ic, "agent_email_mailboxes", agentEmailArchiveMailboxRow(
		accountID, realmID, agentID, addressID, mailboxID,
	))
	rawA := []byte("Subject: archive A\r\n\r\nfirst")
	rawB := append([]byte(nil), rawA...)
	group := agentEmailArchiveDuplicateGroup(rawA)
	feedAgentEmailArchiveRow(t, ic, "agent_email_messages", agentEmailArchiveMessageRow(
		accountID, realmID, agentID, addressID, mailboxID, messageA, rawA, group, "",
	))
	feedAgentEmailArchiveRow(t, ic, "agent_email_messages", agentEmailArchiveMessageRow(
		accountID, realmID, agentID, addressID, mailboxID, messageB, rawB, group, messageA,
	))
	feedAgentEmailArchiveRow(t, ic, "agent_email_deliveries", agentEmailArchiveDeliveryRow(
		accountID, realmID, agentID, mailboxID, messageA,
	))
	feedAgentEmailArchiveRow(t, ic, "agent_email_deliveries", agentEmailArchiveDeliveryRow(
		accountID, realmID, agentID, mailboxID, messageB,
	))
	if err := validateImportedAgentEmailGraph(ic.agentEmailMessages, ic.agentEmailDeliveries); err != nil {
		t.Fatal(err)
	}
	if len(ic.agentEmailMessages) != 2 || ic.agentEmailMessages[messageB].possibleDuplicateID != messageA {
		t.Fatalf("suspected duplicates were not preserved: %#v", ic.agentEmailMessages)
	}

	// A retired reservation deliberately remains portable after its original
	// agent has been permanently removed from the agents stream.
	tombstone := agentEmailArchiveAddressRow(
		accountID, realmID, "agent_permanently_deleted", "eaddr_cccccccccccccccc", true,
	)
	tombstone["agent_segment"] = "former"
	tombstone["local_part"] = "former.abcdefghijkl2345"
	feedAgentEmailArchiveRow(t, ic, "agent_email_addresses", tombstone)
	if got := ic.agentEmailAddresses["eaddr_cccccccccccccccc"]; !got.retired || got.agentID != "agent_permanently_deleted" {
		t.Fatalf("address tombstone = %#v", got)
	}
}

func TestAgentEmailArchiveAcceptsOmittedCapacityPayloadAndRejectsInvalidShapes(t *testing.T) {
	const (
		accountID = "acc_1"
		realmID   = "realm_abcdefghijkl2345"
		agentID   = "agent_1"
		addressID = "eaddr_aaaaaaaaaaaaaaaa"
		mailboxID = "emb_aaaaaaaaaaaaaaaa"
		messageID = "emsg_aaaaaaaaaaaaaaaa"
	)
	newContext := func(t *testing.T) *importCtx {
		t.Helper()
		ic := newImportCtx(accountID)
		ic.exportedAt = time.Date(2026, 7, 21, 14, 0, 0, 0, time.UTC)
		ic.realms[realmID] = true
		ic.agents[agentID] = true
		ic.liveAgents[agentID] = true
		ic.agentRealms[agentID] = realmID
		feedAgentEmailArchiveRow(t, ic, "agent_email_addresses", agentEmailArchiveAddressRow(
			accountID, realmID, agentID, addressID, false,
		))
		feedAgentEmailArchiveRow(t, ic, "agent_email_mailboxes", agentEmailArchiveMailboxRow(
			accountID, realmID, agentID, addressID, mailboxID,
		))
		return ic
	}
	raw := []byte(strings.Join([]string{
		"Subject: omitted attachment",
		"Content-Type: multipart/mixed; boundary=mix",
		"",
		"--mix",
		"Content-Type: text/plain",
		"",
		"bounded body",
		"--mix",
		"Content-Type: application/octet-stream",
		"Content-Disposition: attachment; filename=payload.bin",
		"",
		"attachment bytes",
		"--mix--",
		"",
	}, "\r\n"))
	newOmittedRow := func() map[string]any {
		row := agentEmailArchiveMessageRow(
			accountID, realmID, agentID, addressID, mailboxID, messageID,
			raw, agentEmailArchiveDuplicateGroup(raw), "",
		)
		row["raw_mime"] = nil
		row["payload_retention_state"] = importedAgentEmailPayloadOmittedCapacity
		row["retained_attachment_storage_bytes"] = 0
		return row
	}

	ic := newContext(t)
	feedAgentEmailArchiveRow(t, ic, "agent_email_messages", newOmittedRow())
	if _, ok := ic.agentEmailMessages[messageID]; !ok {
		t.Fatal("valid omitted-capacity message was not recorded")
	}

	for _, test := range []struct {
		name   string
		mutate func(map[string]any)
	}{
		{
			name: "omitted row carries raw MIME",
			mutate: func(row map[string]any) {
				row["raw_mime"] = `\x` + hex.EncodeToString(raw)
			},
		},
		{
			name: "retained state lacks raw MIME",
			mutate: func(row map[string]any) {
				row["payload_retention_state"] = importedAgentEmailPayloadRetained
			},
		},
		{
			name: "storage is not conservative raw size",
			mutate: func(row map[string]any) {
				row["attachment_storage_bytes"] = len(raw) - 1
			},
		},
		{
			name: "omitted row retains attachment bytes",
			mutate: func(row map[string]any) {
				row["retained_attachment_storage_bytes"] = 1
			},
		},
		{
			name: "body text exceeds bound",
			mutate: func(row map[string]any) {
				row["body_text"] = strings.Repeat("x", maximumImportedAgentEmailBodyTextBytes+1)
			},
		},
		{
			name: "body text kind is invalid",
			mutate: func(row map[string]any) {
				row["body_text_kind"] = "application/octet-stream"
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			row := newOmittedRow()
			test.mutate(row)
			err := newContext(t).validateAndRecord("agent_email_messages", row)
			if !errors.Is(err, ErrArchiveContent) {
				t.Fatalf("invalid omitted payload error = %v", err)
			}
		})
	}
}

func TestSchema78AgentEmailArchiveUpgradeProducesCurrentImportShape(t *testing.T) {
	const (
		accountID = "acc_1"
		realmID   = "realm_abcdefghijkl2345"
		agentID   = "agent_1"
		addressID = "eaddr_aaaaaaaaaaaaaaaa"
		mailboxID = "emb_aaaaaaaaaaaaaaaa"
		messageID = "emsg_aaaaaaaaaaaaaaaa"
	)
	ic := newImportCtx(accountID)
	ic.exportedAt = time.Date(2026, 7, 21, 14, 0, 0, 0, time.UTC)
	ic.realms[realmID] = true
	ic.agents[agentID] = true
	ic.liveAgents[agentID] = true
	ic.agentRealms[agentID] = realmID
	feedAgentEmailArchiveRow(t, ic, "agent_email_addresses", agentEmailArchiveAddressRow(
		accountID, realmID, agentID, addressID, false,
	))
	feedAgentEmailArchiveRow(t, ic, "agent_email_mailboxes", agentEmailArchiveMailboxRow(
		accountID, realmID, agentID, addressID, mailboxID,
	))
	raw := []byte(strings.Join([]string{
		"Subject: legacy attachment",
		"Content-Type: multipart/mixed; boundary=mix",
		"",
		"--mix",
		"Content-Type: text/plain",
		"",
		"legacy body",
		"--mix",
		"Content-Type: application/octet-stream",
		"",
		"legacy attachment",
		"--mix--",
		"",
	}, "\r\n"))
	legacy := agentEmailArchiveMessageRow(
		accountID, realmID, agentID, addressID, mailboxID, messageID,
		raw, agentEmailArchiveDuplicateGroup(raw), "",
	)
	for _, field := range []string{
		"body_text", "body_text_kind", "attachment_storage_bytes",
		"retained_attachment_storage_bytes", "payload_retention_state",
	} {
		delete(legacy, field)
	}
	legacy["raw_size_bytes"] = json.Number(strconv.Itoa(len(raw)))
	legacy["attachment_count"] = json.Number("1")
	upgraded, err := archiveexport.UpgraderFor(78)("agent_email_messages", legacy)
	if err != nil {
		t.Fatal(err)
	}
	if err := ic.validateAndRecord("agent_email_messages", upgraded); err != nil {
		t.Fatalf("validate upgraded schema-78 message: %v", err)
	}
	if upgraded["body_text"] != nil || upgraded["body_text_kind"] != nil ||
		upgraded["attachment_storage_bytes"] != int64(len(raw)) ||
		upgraded["retained_attachment_storage_bytes"] != int64(len(raw)) ||
		upgraded["payload_retention_state"] != importedAgentEmailPayloadRetained {
		t.Fatalf("upgraded schema-78 payload shape = %#v", upgraded)
	}
}

func TestAgentEmailArchiveRejectsHostileContentAndCrossScopeLinks(t *testing.T) {
	const (
		accountID = "acc_1"
		realmID   = "realm_abcdefghijkl2345"
		agentID   = "agent_1"
		addressID = "eaddr_aaaaaaaaaaaaaaaa"
		mailboxID = "emb_aaaaaaaaaaaaaaaa"
	)
	newContext := func(t *testing.T) *importCtx {
		t.Helper()
		ic := newImportCtx(accountID)
		ic.exportedAt = time.Date(2026, 7, 21, 14, 0, 0, 0, time.UTC)
		ic.realms[realmID] = true
		ic.agents[agentID] = true
		ic.liveAgents[agentID] = true
		ic.agentRealms[agentID] = realmID
		feedAgentEmailArchiveRow(t, ic, "agent_email_addresses", agentEmailArchiveAddressRow(
			accountID, realmID, agentID, addressID, false,
		))
		feedAgentEmailArchiveRow(t, ic, "agent_email_mailboxes", agentEmailArchiveMailboxRow(
			accountID, realmID, agentID, addressID, mailboxID,
		))
		return ic
	}

	t.Run("raw digest mismatch", func(t *testing.T) {
		ic := newContext(t)
		raw := []byte("Subject: forged\r\n\r\nbody")
		row := agentEmailArchiveMessageRow(accountID, realmID, agentID, addressID, mailboxID,
			"emsg_aaaaaaaaaaaaaaaa", raw, agentEmailArchiveDuplicateGroup(raw), "")
		row["raw_sha256"] = strings.Repeat("0", 64)
		err := ic.validateAndRecord("agent_email_messages", row)
		if !errors.Is(err, ErrArchiveContent) || !strings.Contains(err.Error(), "does not match raw_mime") {
			t.Fatalf("raw mismatch error = %v", err)
		}
	})

	t.Run("MIME projection must match raw message", func(t *testing.T) {
		ic := newContext(t)
		raw := []byte("From: sender@example.com\r\nSubject: canonical\r\n\r\nbody")
		row := agentEmailArchiveMessageRow(accountID, realmID, agentID, addressID, mailboxID,
			"emsg_aaaaaaaaaaaaaaaa", raw, agentEmailArchiveDuplicateGroup(raw), "")
		row["header_subject"] = "forged trusted projection"
		err := ic.validateAndRecord("agent_email_messages", row)
		if !errors.Is(err, ErrArchiveContent) || !strings.Contains(err.Error(), "header_subject does not match raw_mime") {
			t.Fatalf("forged MIME projection error = %v", err)
		}
	})

	t.Run("body text projection must match retained raw message", func(t *testing.T) {
		ic := newContext(t)
		raw := []byte("Subject: canonical body\r\n\r\nbody")
		row := agentEmailArchiveMessageRow(accountID, realmID, agentID, addressID, mailboxID,
			"emsg_aaaaaaaaaaaaaaaa", raw, agentEmailArchiveDuplicateGroup(raw), "")
		row["body_text"] = "forged body"
		err := ic.validateAndRecord("agent_email_messages", row)
		if !errors.Is(err, ErrArchiveContent) || !strings.Contains(err.Error(), "body text projection") {
			t.Fatalf("forged body projection error = %v", err)
		}
	})

	t.Run("retained attachment storage must match conservative accounting", func(t *testing.T) {
		ic := newContext(t)
		raw := []byte("Subject: no attachment\r\n\r\nbody")
		row := agentEmailArchiveMessageRow(accountID, realmID, agentID, addressID, mailboxID,
			"emsg_aaaaaaaaaaaaaaaa", raw, agentEmailArchiveDuplicateGroup(raw), "")
		row["attachment_storage_bytes"] = 1
		row["retained_attachment_storage_bytes"] = 1
		err := ic.validateAndRecord("agent_email_messages", row)
		if !errors.Is(err, ErrArchiveContent) || !strings.Contains(err.Error(), "conservative raw-size accounting") {
			t.Fatalf("forged attachment accounting error = %v", err)
		}
	})

	t.Run("pilot trust posture cannot be elevated", func(t *testing.T) {
		raw := []byte("Subject: posture\r\n\r\nbody")
		for name, mutate := range map[string]func(map[string]any){
			"provider id": func(row map[string]any) { row["provider_message_id"] = "forged-provider-id" },
			"spf pass":    func(row map[string]any) { row["spf_result"] = "pass" },
			"verified":    func(row map[string]any) { row["sender_verification_state"] = "verified" },
		} {
			t.Run(name, func(t *testing.T) {
				ic := newContext(t)
				row := agentEmailArchiveMessageRow(accountID, realmID, agentID, addressID, mailboxID,
					"emsg_aaaaaaaaaaaaaaaa", raw, agentEmailArchiveDuplicateGroup(raw), "")
				mutate(row)
				err := ic.validateAndRecord("agent_email_messages", row)
				if !errors.Is(err, ErrArchiveContent) {
					t.Fatalf("elevated pilot posture error = %v", err)
				}
			})
		}
	})

	t.Run("mailbox cannot graft foreign address", func(t *testing.T) {
		ic := newContext(t)
		ic.realms["realm_2"] = true
		ic.agents["agent_2"] = true
		ic.liveAgents["agent_2"] = true
		ic.agentRealms["agent_2"] = "realm_2"
		row := agentEmailArchiveMailboxRow(accountID, "realm_2", "agent_2", addressID,
			"emb_bbbbbbbbbbbbbbbb")
		err := ic.validateAndRecord("agent_email_mailboxes", row)
		if !errors.Is(err, ErrArchiveContent) || !strings.Contains(err.Error(), "outside mailbox scope") {
			t.Fatalf("cross-scope mailbox error = %v", err)
		}
	})

	t.Run("possible duplicate must stay in one group", func(t *testing.T) {
		ic := newContext(t)
		rawA := []byte("Subject: A\r\n\r\nA")
		rawB := []byte("Subject: B\r\n\r\nB")
		feedAgentEmailArchiveRow(t, ic, "agent_email_messages", agentEmailArchiveMessageRow(
			accountID, realmID, agentID, addressID, mailboxID, "emsg_aaaaaaaaaaaaaaaa",
			rawA, agentEmailArchiveDuplicateGroup(rawA), "",
		))
		feedAgentEmailArchiveRow(t, ic, "agent_email_messages", agentEmailArchiveMessageRow(
			accountID, realmID, agentID, addressID, mailboxID, "emsg_bbbbbbbbbbbbbbbb",
			rawB, agentEmailArchiveDuplicateGroup(rawB), "emsg_aaaaaaaaaaaaaaaa",
		))
		feedAgentEmailArchiveRow(t, ic, "agent_email_deliveries", agentEmailArchiveDeliveryRow(
			accountID, realmID, agentID, mailboxID, "emsg_aaaaaaaaaaaaaaaa",
		))
		feedAgentEmailArchiveRow(t, ic, "agent_email_deliveries", agentEmailArchiveDeliveryRow(
			accountID, realmID, agentID, mailboxID, "emsg_bbbbbbbbbbbbbbbb",
		))
		if err := validateImportedAgentEmailGraph(ic.agentEmailMessages, ic.agentEmailDeliveries); err == nil ||
			!strings.Contains(err.Error(), "outside its duplicate group") {
			t.Fatalf("duplicate-group error = %v", err)
		}
	})
}

func TestAgentEmailArchiveAcceptsOnlyMessageBoundTerminalCanaryProof(t *testing.T) {
	const (
		accountID = "acc_1"
		realmID   = "realm_abcdefghijkl2345"
		agentID   = "agent_1"
		addressID = "eaddr_aaaaaaaaaaaaaaaa"
		mailboxID = "emb_aaaaaaaaaaaaaaaa"
		messageID = "emsg_aaaaaaaaaaaaaaaa"
		challenge = "11111111-2222-4333-8444-555555555555"
	)
	newContext := func(t *testing.T) (*importCtx, string, string, string) {
		t.Helper()
		ic := newImportCtx(accountID)
		ic.exportedAt = time.Date(2026, 7, 21, 14, 0, 0, 0, time.UTC)
		ic.realms[realmID] = true
		ic.agents[agentID] = true
		ic.liveAgents[agentID] = true
		ic.agentRealms[agentID] = realmID
		feedAgentEmailArchiveRow(t, ic, "agent_email_addresses", agentEmailArchiveAddressRow(
			accountID, realmID, agentID, addressID, false,
		))
		feedAgentEmailArchiveRow(t, ic, "agent_email_mailboxes", agentEmailArchiveMailboxRow(
			accountID, realmID, agentID, addressID, mailboxID,
		))
		raw := []byte("X-Witself-Canary-Retry: " + challenge + "\r\nSubject: retry\r\n\r\nbody")
		legacyFingerprint := agentEmailArchiveDuplicateGroup(raw)
		stableFingerprint, _ := mustAgentEmailRetryCanaryFingerprint(t, raw,
			"sender@example.com", "owner.abcdefghijkl2345@agent-mail.witwave.ai")
		feedAgentEmailArchiveRow(t, ic, "agent_email_messages", agentEmailArchiveMessageRow(
			accountID, realmID, agentID, addressID, mailboxID, messageID, raw, legacyFingerprint, "",
		))
		feedAgentEmailArchiveRow(t, ic, "agent_email_deliveries", agentEmailArchiveDeliveryRow(
			accountID, realmID, agentID, mailboxID, messageID,
		))
		digest := sha256.Sum256([]byte(challenge))
		return ic, hex.EncodeToString(digest[:]), legacyFingerprint, stableFingerprint
	}
	row := func(challengeHash, fingerprint string) map[string]any {
		return map[string]any{
			"account_id": accountID, "realm_id": realmID, "mailbox_id": mailboxID,
			"owner_agent_id": agentID, "challenge_sha256": challengeHash,
			"state": "accepted", "delivery_fingerprint_sha256": fingerprint,
			"accepted_message_id": messageID, "tempfail_count": 1, "row_version": 3,
			"armed_at": "2026-07-21T12:00:00Z", "expires_at": "2026-07-21T12:15:00Z",
			"tempfailed_at": "2026-07-21T12:00:01Z", "retry_expires_at": "2026-07-22T12:00:01Z",
			"accepted_at": "2026-07-21T12:00:02Z",
		}
	}
	ic, challengeHash, legacyFingerprint, stableFingerprint := newContext(t)
	feedAgentEmailArchiveRow(t, ic, "agent_email_retry_canary_arms", row(challengeHash, stableFingerprint))
	if len(ic.agentEmailRetryCanaries) != 1 {
		t.Fatalf("accepted retry proofs = %#v", ic.agentEmailRetryCanaries)
	}
	t.Run("legacy exact raw fingerprint", func(t *testing.T) {
		ic, challengeHash, legacyFingerprint, _ := newContext(t)
		feedAgentEmailArchiveRow(t, ic, "agent_email_retry_canary_arms",
			row(challengeHash, legacyFingerprint))
		if len(ic.agentEmailRetryCanaries) != 1 {
			t.Fatalf("legacy accepted retry proofs = %#v", ic.agentEmailRetryCanaries)
		}
	})
	if legacyFingerprint == stableFingerprint {
		t.Fatal("legacy and stable retry fingerprints unexpectedly match")
	}

	for name, mutate := range map[string]func(map[string]any){
		"live arm": func(candidate map[string]any) { candidate["state"] = "tempfailed" },
		"wrong fingerprint": func(candidate map[string]any) {
			candidate["delivery_fingerprint_sha256"] = strings.Repeat("f", 64)
		},
		"wrong challenge": func(candidate map[string]any) {
			candidate["challenge_sha256"] = strings.Repeat("e", 64)
		},
	} {
		t.Run(name, func(t *testing.T) {
			ic, challengeHash, _, stableFingerprint := newContext(t)
			candidate := row(challengeHash, stableFingerprint)
			mutate(candidate)
			if err := ic.validateAndRecord("agent_email_retry_canary_arms", candidate); !errors.Is(err, ErrArchiveContent) {
				t.Fatalf("error = %v", err)
			}
		})
	}
}

func feedAgentEmailArchiveRow(t *testing.T, ic *importCtx, table string, row map[string]any) {
	t.Helper()
	if err := ic.validateAndRecord(table, row); err != nil {
		t.Fatalf("%s: %v", table, err)
	}
}

func agentEmailArchiveAddressRow(accountID, realmID, agentID, addressID string, retired bool) map[string]any {
	row := map[string]any{
		"id": addressID, "account_id": accountID, "realm_id": realmID,
		"provisioned_agent_id": agentID, "domain": "agent-mail.witwave.ai",
		"agent_segment": "owner", "realm_label": "abcdefghijkl2345",
		"local_part": "owner.abcdefghijkl2345", "provisioning_kind": "derived",
		"created_at": "2026-07-21T12:00:00Z",
		"retired_at": nil, "retirement_reason_code": nil,
	}
	if retired {
		row["retired_at"] = "2026-07-21T12:30:00Z"
		row["retirement_reason_code"] = "agent_deleted"
	}
	return row
}

func agentEmailArchiveMailboxRow(accountID, realmID, agentID, addressID, mailboxID string) map[string]any {
	return map[string]any{
		"id": mailboxID, "account_id": accountID, "realm_id": realmID,
		"owner_agent_id": agentID, "address_id": addressID,
		"receive_state": "enabled", "row_version": 1,
		"created_at": "2026-07-21T12:01:00Z", "updated_at": "2026-07-21T12:01:00Z",
		"disabled_at": nil, "retired_at": nil,
	}
}

func agentEmailArchiveMessageRow(
	accountID, realmID, agentID, addressID, mailboxID, messageID string,
	raw []byte, duplicateGroup, possibleDuplicate string,
) map[string]any {
	digest := sha256.Sum256(raw)
	parsed, parseErr := agentemail.ParseMessage(raw, true)
	parseState := AgentEmailParseParsed
	parseError := any(nil)
	if parseErr != nil {
		parseState = AgentEmailParseError
		parseError = agentemail.ParseErrorCode(parseErr)
	}
	nullable := func(value string) any {
		if value == "" {
			return nil
		}
		return value
	}
	messageDate := any(nil)
	if parsed.MessageDate != nil {
		messageDate = parsed.MessageDate.Format(time.RFC3339Nano)
	}
	attachmentStorageBytes := 0
	if parsed.AttachmentCount > 0 || parseState == AgentEmailParseError {
		attachmentStorageBytes = len(raw)
	}
	possible := any(nil)
	if possibleDuplicate != "" {
		possible = possibleDuplicate
	}
	return map[string]any{
		"id": messageID, "account_id": accountID, "realm_id": realmID,
		"mailbox_id": mailboxID, "owner_agent_id": agentID, "address_id": addressID,
		"provider": agentEmailPilotProvider, "provider_message_id": nil,
		"envelope_sender":    "sender@example.com",
		"envelope_recipient": "owner.abcdefghijkl2345@agent-mail.witwave.ai",
		"agent_segment":      "owner", "realm_label": "abcdefghijkl2345", "subaddress_tag": nil,
		"raw_mime": `\x` + hex.EncodeToString(raw), "raw_size_bytes": len(raw),
		"raw_sha256":  hex.EncodeToString(digest[:]),
		"parse_state": parseState, "parse_error": parseError,
		"header_from": nullable(parsed.HeaderFrom), "header_to": nullable(parsed.HeaderTo),
		"header_subject":  nullable(parsed.HeaderSubject),
		"mime_message_id": nullable(parsed.MIMEMessageID), "message_date": messageDate,
		"attachment_count": parsed.AttachmentCount,
		"body_text":        nullable(parsed.Text), "body_text_kind": nullable(parsed.TextKind),
		"attachment_storage_bytes":          attachmentStorageBytes,
		"retained_attachment_storage_bytes": attachmentStorageBytes,
		"payload_retention_state":           importedAgentEmailPayloadRetained,
		"spf_result":                        "unknown", "dkim_result": "unknown",
		"dmarc_result": "unknown", "spam_verdict": "unknown",
		"sender_verification_state":        "unverified",
		"duplicate_group_sha256":           duplicateGroup,
		"possible_duplicate_of_message_id": possible,
		"received_at":                      "2026-07-21T12:02:00Z", "created_at": "2026-07-21T12:02:00Z",
	}
}

func agentEmailArchiveDeliveryRow(accountID, realmID, agentID, mailboxID, messageID string) map[string]any {
	return map[string]any{
		"message_id": messageID, "account_id": accountID, "realm_id": realmID,
		"mailbox_id": mailboxID, "owner_agent_id": agentID, "folder": "inbox",
		"delivered_at": "2026-07-21T12:03:00Z", "read_at": nil,
		"acked_at": nil, "code_consumed_at": nil,
		"processing_state": MessageProcessingAvailable, "processing_generation": 0,
		"failure_count": 0, "claim_id": nil, "claim_key_hash": "",
		"lease_expires_at": nil, "completed_at": nil, "complete_key_hash": "",
		"created_at": "2026-07-21T12:03:00Z",
	}
}

func agentEmailArchiveDuplicateGroup(raw []byte) string {
	digest := sha256.Sum256(raw)
	return agentEmailDuplicateGroup(
		hex.EncodeToString(digest[:]),
		"owner.abcdefghijkl2345@agent-mail.witwave.ai",
		"sender@example.com",
	)
}
