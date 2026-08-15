package store

import (
	"errors"
	"maps"
	"strings"
	"testing"
	"time"
)

func TestAgentEmailOutboundArchiveClaimNormalizationConsumesFence(t *testing.T) {
	importedAt := time.Date(2026, 8, 14, 12, 0, 0, 0, time.UTC)
	base := map[string]any{
		"id":    "esnd_aaaaaaaaaaaaaaaa",
		"state": AgentEmailOutboundClaimed, "claim_generation": 7,
		"claim_id":         "escl_aaaaaaaaaaaaaaaa",
		"lease_expires_at": "2026-08-14T12:05:00Z",
		"next_attempt_at":  nil, "provider_started_at": nil,
		"last_error_code": "",
	}
	claimed := maps.Clone(base)
	if err := newImportCtx("acc_1").normalizeImportedAgentEmailOutboundClaim(
		"agent_email_outbound_messages", claimed, importedAt,
	); err != nil {
		t.Fatal(err)
	}
	if claimed["state"] != AgentEmailOutboundQueued ||
		claimed["claim_generation"] != int64(8) || claimed["claim_id"] != nil ||
		claimed["lease_expires_at"] != nil ||
		claimed["next_attempt_at"] != importedAt.Format(time.RFC3339Nano) ||
		claimed["last_error_code"] != AgentEmailOutboundErrorWorkerLeaseExpired {
		t.Fatalf("normalized claimed outbound = %#v", claimed)
	}
	if _, _, err := validateImportedAgentEmailOutboundLifecycle(claimed); err != nil {
		t.Fatalf("normalized claimed outbound is not queued-shaped: %v", err)
	}

	providerStarted := maps.Clone(base)
	providerStarted["state"] = AgentEmailOutboundProviderStarted
	providerStarted["provider_started_at"] = "2026-08-14T11:59:00Z"
	if err := newImportCtx("acc_1").normalizeImportedAgentEmailOutboundClaim(
		"agent_email_outbound_messages", providerStarted, importedAt,
	); err != nil {
		t.Fatal(err)
	}
	if providerStarted["state"] != AgentEmailOutboundAmbiguous ||
		providerStarted["claim_generation"] != int64(8) ||
		providerStarted["claim_id"] != nil || providerStarted["lease_expires_at"] != nil ||
		providerStarted["next_attempt_at"] != nil ||
		providerStarted["ambiguous_at"] != importedAt.Format(time.RFC3339Nano) ||
		providerStarted["last_error_code"] != AgentEmailOutboundErrorWorkerLeaseExpired {
		t.Fatalf("normalized provider-started outbound = %#v", providerStarted)
	}
	if _, _, err := validateImportedAgentEmailOutboundLifecycle(providerStarted); err != nil {
		t.Fatalf("normalized provider-started outbound is not ambiguous-shaped: %v", err)
	}

	exhausted := maps.Clone(base)
	exhausted["claim_generation"] = maximumAgentEmailOutboundGeneration
	if err := newImportCtx("acc_1").normalizeImportedAgentEmailOutboundClaim(
		"agent_email_outbound_messages", exhausted, importedAt,
	); !errors.Is(err, ErrArchiveContent) {
		t.Fatalf("exhausted outbound claim error = %v", err)
	}
}

func TestAgentEmailOutboundArchiveNormalizedLifecycleUsesDestinationClock(t *testing.T) {
	const (
		accountID = "acc_1"
		realmID   = "realm_abcdefghijkl2345"
		agentID   = "agent_1"
		addressID = "eaddr_aaaaaaaaaaaaaaaa"
		sendID    = "esnd_aaaaaaaaaaaaaaaa"
	)
	exportedAt := time.Date(2026, 8, 14, 13, 0, 0, 0, time.UTC)
	importedAt := exportedAt.Add(time.Hour)

	for _, tc := range []struct {
		name      string
		source    string
		wantState string
	}{
		{name: "claimed is requeued", source: AgentEmailOutboundClaimed, wantState: AgentEmailOutboundQueued},
		{name: "provider started is made ambiguous", source: AgentEmailOutboundProviderStarted, wantState: AgentEmailOutboundAmbiguous},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ic := agentEmailOutboundArchiveImportContext(
				accountID, realmID, agentID, addressID, exportedAt,
			)
			row := agentEmailOutboundArchiveAcceptedRow(
				accountID, realmID, agentID, addressID, sendID,
			)
			row["state"] = tc.source
			row["provider_state"] = ""
			row["provider"] = ""
			row["provider_message_id"] = ""
			row["last_error_code"] = ""
			row["claim_generation"] = 7
			row["claim_id"] = "escl_aaaaaaaaaaaaaaaa"
			row["lease_expires_at"] = "2026-08-14T13:05:00Z"
			row["next_attempt_at"] = nil
			row["accepted_at"] = nil
			if tc.source == AgentEmailOutboundProviderStarted {
				row["provider_started_at"] = "2026-08-14T12:30:00Z"
			} else {
				row["provider_started_at"] = nil
			}
			if err := ic.normalizeImportedAgentEmailOutboundClaim(
				"agent_email_outbound_messages", row, importedAt,
			); err != nil {
				t.Fatal(err)
			}
			if err := ic.validateAndRecord("agent_email_outbound_messages", row); err != nil {
				t.Fatalf("validate normalized outbound: %v", err)
			}
			if row["state"] != tc.wantState ||
				row["updated_at"] != importedAt.Format(time.RFC3339Nano) {
				t.Fatalf("normalized outbound = %#v", row)
			}
		})
	}

	t.Run("ordinary source timestamp remains export bounded", func(t *testing.T) {
		ic := agentEmailOutboundArchiveImportContext(
			accountID, realmID, agentID, addressID, exportedAt,
		)
		row := agentEmailOutboundArchiveAcceptedRow(
			accountID, realmID, agentID, addressID, sendID,
		)
		row["updated_at"] = importedAt.Format(time.RFC3339Nano)
		if err := ic.validateAndRecord(
			"agent_email_outbound_messages", row,
		); !errors.Is(err, ErrArchiveContent) {
			t.Fatalf("future source timestamp error = %v", err)
		}
	})

	t.Run("normalization does not excuse future provider history", func(t *testing.T) {
		ic := agentEmailOutboundArchiveImportContext(
			accountID, realmID, agentID, addressID, exportedAt,
		)
		row := agentEmailOutboundArchiveAcceptedRow(
			accountID, realmID, agentID, addressID, sendID,
		)
		row["state"] = AgentEmailOutboundProviderStarted
		row["provider_state"] = ""
		row["provider"] = ""
		row["provider_message_id"] = ""
		row["last_error_code"] = ""
		row["claim_generation"] = 7
		row["claim_id"] = "escl_aaaaaaaaaaaaaaaa"
		row["lease_expires_at"] = "2026-08-14T13:05:00Z"
		row["next_attempt_at"] = nil
		row["provider_started_at"] = "2026-08-14T13:01:00Z"
		row["accepted_at"] = nil
		if err := ic.normalizeImportedAgentEmailOutboundClaim(
			"agent_email_outbound_messages", row, importedAt,
		); err != nil {
			t.Fatal(err)
		}
		if err := ic.validateAndRecord(
			"agent_email_outbound_messages", row,
		); !errors.Is(err, ErrArchiveContent) {
			t.Fatalf("future provider history error = %v", err)
		}
	})
}

func TestAgentEmailOutboundArchiveAllowsRetainedReplyAfterInboundExpiry(t *testing.T) {
	const (
		accountID = "acc_1"
		realmID   = "realm_abcdefghijkl2345"
		agentID   = "agent_1"
		addressID = "eaddr_aaaaaaaaaaaaaaaa"
		sendID    = "esnd_aaaaaaaaaaaaaaaa"
		replyID   = "emsg_bbbbbbbbbbbbbbbb"
	)
	exportedAt := time.Date(2026, 8, 14, 13, 0, 0, 0, time.UTC)
	row := agentEmailOutboundArchiveAcceptedRow(
		accountID, realmID, agentID, addressID, sendID,
	)
	row["request_kind"] = AgentEmailOutboundRequestReply
	row["reply_to_inbound_message_id"] = replyID
	row["thread_key"] = "reply:retained"

	t.Run("missing retained parent is detached provenance", func(t *testing.T) {
		ic := agentEmailOutboundArchiveImportContext(
			accountID, realmID, agentID, addressID, exportedAt,
		)
		if err := ic.validateAndRecord("agent_email_outbound_messages", maps.Clone(row)); err != nil {
			t.Fatalf("validate detached reply provenance: %v", err)
		}
	})

	t.Run("present parent still enforces owner scope", func(t *testing.T) {
		ic := agentEmailOutboundArchiveImportContext(
			accountID, realmID, agentID, addressID, exportedAt,
		)
		ic.agentEmailMessages[replyID] = agentEmailMessageImportScope{
			realmID: realmID, ownerAgentID: "agent_other",
		}
		if err := ic.validateAndRecord(
			"agent_email_outbound_messages", maps.Clone(row),
		); !errors.Is(err, ErrArchiveContent) {
			t.Fatalf("cross-owner retained reply error = %v", err)
		}
	})
}

func TestAgentEmailOutboundArchiveValidatesTenantGraphAndSafetyRows(t *testing.T) {
	const (
		accountID = "acc_1"
		realmID   = "realm_abcdefghijkl2345"
		agentID   = "agent_1"
		addressID = "eaddr_aaaaaaaaaaaaaaaa"
		sendID    = "esnd_aaaaaaaaaaaaaaaa"
	)
	ic := newImportCtx(accountID)
	ic.exportedAt = time.Date(2026, 8, 14, 13, 0, 0, 0, time.UTC)
	ic.realms[realmID] = true
	ic.agents[agentID] = true
	ic.liveAgents[agentID] = true
	ic.agentRealms[agentID] = realmID
	feedAgentEmailArchiveRow(t, ic, "agent_email_realm_send_controls",
		agentEmailOutboundArchiveRealmControlRow(accountID, realmID))
	feedAgentEmailArchiveRow(t, ic, "agent_email_send_controls",
		agentEmailOutboundArchiveAgentControlRow(accountID, realmID, agentID))
	feedAgentEmailArchiveRow(t, ic, "agent_email_addresses",
		agentEmailArchiveAddressRow(accountID, realmID, agentID, addressID, false))
	feedAgentEmailArchiveRow(t, ic, "agent_email_outbound_messages",
		agentEmailOutboundArchiveAcceptedRow(accountID, realmID, agentID, addressID, sendID))
	feedAgentEmailArchiveRow(t, ic, "agent_email_outbound_provider_events", map[string]any{
		"account_id": accountID,
		"provider":   "cloudflare", "event_id_hash": strings.Repeat("a", 64),
		"event_request_hash": strings.Repeat("b", 64), "outbound_id": sendID,
		"event_class": AgentEmailOutboundProviderEventDelivered,
		"occurred_at": "2026-08-14T12:02:00Z", "received_at": "2026-08-14T12:02:01Z",
	})
	// Provider clocks may be ahead by the same bounded five minutes accepted by
	// the live schema. Only the local receipt must precede export; preserving
	// the exact provider time must not strand an otherwise portable account.
	feedAgentEmailArchiveRow(t, ic, "agent_email_outbound_provider_events", map[string]any{
		"account_id": accountID,
		"provider":   "cloudflare", "event_id_hash": strings.Repeat("d", 64),
		"event_request_hash": strings.Repeat("e", 64), "outbound_id": sendID,
		"event_class": AgentEmailOutboundProviderEventDeferred,
		"occurred_at": "2026-08-14T13:04:00Z", "received_at": "2026-08-14T12:59:00Z",
	})
	feedAgentEmailArchiveRow(t, ic, "agent_email_outbound_recipient_suppressions", map[string]any{
		"account_id": accountID, "recipient_sha256": strings.Repeat("c", 64),
		"reason": "hard_bounce", "source_send_id": sendID, "provider": "cloudflare",
		"created_at": "2026-08-14T12:03:00Z", "updated_at": "2026-08-14T12:03:00Z",
	})
	if len(ic.agentEmailOutboundMessages) != 1 || len(ic.agentEmailOutboundEvents) != 2 ||
		len(ic.agentEmailOutboundSuppressions) != 1 {
		t.Fatalf("outbound archive graph = messages:%d events:%d suppressions:%d",
			len(ic.agentEmailOutboundMessages), len(ic.agentEmailOutboundEvents),
			len(ic.agentEmailOutboundSuppressions))
	}

	t.Run("provider event cannot cross provider scope", func(t *testing.T) {
		row := map[string]any{
			"account_id": accountID,
			"provider":   "other", "event_id_hash": strings.Repeat("d", 64),
			"event_request_hash": strings.Repeat("e", 64), "outbound_id": sendID,
			"event_class": AgentEmailOutboundProviderEventDelivered,
			"occurred_at": "2026-08-14T12:02:00Z", "received_at": "2026-08-14T12:02:01Z",
		}
		if err := ic.validateAndRecord("agent_email_outbound_provider_events", row); !errors.Is(err, ErrArchiveContent) {
			t.Fatalf("cross-provider event error = %v", err)
		}
	})

	t.Run("suppression cannot smuggle a recipient", func(t *testing.T) {
		row := map[string]any{
			"account_id": accountID, "recipient_sha256": "person@example.com",
			"reason": "complained", "source_send_id": sendID, "provider": "cloudflare",
			"created_at": "2026-08-14T12:03:00Z", "updated_at": "2026-08-14T12:03:00Z",
		}
		if err := ic.validateAndRecord("agent_email_outbound_recipient_suppressions", row); !errors.Is(err, ErrArchiveContent) {
			t.Fatalf("plaintext suppression error = %v", err)
		}
	})
}

func agentEmailOutboundArchiveRealmControlRow(accountID, realmID string) map[string]any {
	return map[string]any{
		"account_id": accountID, "realm_id": realmID,
		"send_state": AgentEmailSendEnabled, "row_version": 1,
		"created_at": "2026-08-14T11:00:00Z", "updated_at": "2026-08-14T11:00:00Z",
		"disabled_at": nil,
	}
}

func agentEmailOutboundArchiveAgentControlRow(accountID, realmID, agentID string) map[string]any {
	row := agentEmailOutboundArchiveRealmControlRow(accountID, realmID)
	row["owner_agent_id"] = agentID
	return row
}

func agentEmailOutboundArchiveImportContext(
	accountID string,
	realmID string,
	agentID string,
	addressID string,
	exportedAt time.Time,
) *importCtx {
	ic := newImportCtx(accountID)
	ic.exportedAt = exportedAt
	ic.realms[realmID] = true
	ic.agents[agentID] = true
	ic.liveAgents[agentID] = true
	ic.agentRealms[agentID] = realmID
	ic.agentEmailAddresses[addressID] = agentEmailAddressImportScope{
		realmID: realmID, agentID: agentID, localPart: "owner.abcdefghijkl2345",
	}
	return ic
}

func agentEmailOutboundArchiveAcceptedRow(
	accountID, realmID, agentID, addressID, sendID string,
) map[string]any {
	localPart := "owner.abcdefghijkl2345"
	return map[string]any{
		"id": sendID, "account_id": accountID, "realm_id": realmID,
		"owner_agent_id": agentID, "address_id": addressID,
		"from_address":     localPart + "@send.witmail.net",
		"reply_to_address": localPart + "@witmail.net",
		"to_address":       "recipient@example.com", "subject": "archive",
		"body_text": "portable body", "request_kind": AgentEmailOutboundRequestDirect,
		"reply_to_inbound_message_id": nil, "thread_key": "direct:archive",
		"in_reply_to_header": nil, "references_headers": []any{},
		"idempotency_key_hash": strings.Repeat("1", 64),
		"request_hash":         strings.Repeat("2", 64),
		"state":                AgentEmailOutboundAccepted, "provider_state": AgentEmailOutboundAccepted,
		"provider": "cloudflare", "provider_message_id": "provider-message-1",
		"last_error_code": "", "attempt_count": 1, "claim_generation": 1,
		"claim_id": nil, "lease_expires_at": nil, "next_attempt_at": nil,
		"queued_at":           "2026-08-14T12:00:00Z",
		"provider_started_at": "2026-08-14T12:01:00Z",
		"accepted_at":         "2026-08-14T12:01:01Z", "delivered_at": nil,
		"deferred_at": nil, "failed_at": nil, "ambiguous_at": nil, "canceled_at": nil,
		"created_at": "2026-08-14T12:00:00Z", "updated_at": "2026-08-14T12:01:01Z",
	}
}
