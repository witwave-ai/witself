// Package stubcell supplies deterministic cell fixtures for dashboard tests and release acceptance.
package stubcell

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"time"

	"github.com/witwave-ai/witself/internal/client"
)

// Avatar returns a deterministic canonical SVG and the digest checked by the dashboard proxy.
func Avatar() any {
	const svg = `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 256 256"><title>Acceptance fixture avatar</title><circle cx="128" cy="128" r="96" fill="#2563eb"></circle></svg>`
	digest := sha256.Sum256([]byte(svg))
	return map[string]any{"avatar": client.AvatarView{Active: &client.AvatarVersion{
		ID: "avatar_1", Version: 1, IsActive: true, SVG: svg, SVGSHA256: hex.EncodeToString(digest[:]),
	}}}
}

// Transcripts returns the shared cell response used by proxy and acceptance tests.
func Transcripts() any {
	return map[string]any{"transcripts": []client.Transcript{{ID: "tr_1"}}}
}

// TranscriptDetail returns the shared cell response used by proxy and acceptance tests.
func TranscriptDetail() any {
	return client.TranscriptDetail{Transcript: client.Transcript{ID: "tr_1"}}
}

// Memories returns the shared cell response used by proxy and acceptance tests.
func Memories() any {
	return client.MemoryPage{Items: []client.Memory{{ID: "mem_1"}}}
}

// MemoryDetail returns the shared cell response used by proxy and acceptance tests.
func MemoryDetail() any {
	return map[string]any{"memory": client.Memory{ID: "mem_1"}}
}

// MemoryHistory returns the shared cell response used by proxy and acceptance tests.
func MemoryHistory() any {
	return client.MemoryHistoryPage{}
}

// Messages returns the shared cell response used by proxy and acceptance tests.
func Messages() any {
	return client.MessagePage{Messages: []client.Message{{
		ID:      "msg_1",
		Subject: "greetings",
		Body:    "leaked-body-text",
		Payload: json.RawMessage(`{"leaked":"payload"}`),
	}}}
}

// EmailAddress returns the shared cell response used by proxy and acceptance tests.
func EmailAddress() any {
	now := time.Date(2026, 7, 21, 20, 1, 2, 0, time.UTC)
	return map[string]any{"address": client.AgentEmailAddress{
		ID: "private-address-id", MailboxID: "private-mailbox-id", OwnerAgentID: "private-owner-id",
		Address: "dash@agents.example", Domain: "private-domain", LocalPart: "private-local-part",
		AgentSegment: "private-agent-segment", RealmLabel: "private-realm-label",
		ProvisioningKind: "pilot", ReceiveState: "disabled",
		AgentReceiveState: "enabled", RealmReceiveState: "disabled",
		CreatedAt: now, UpdatedAt: now,
	}}
}

// EmailStatus returns the shared cell response used by proxy and acceptance tests.
func EmailStatus() any {
	return map[string]any{
		"schema_version":    "witself.v0",
		"maximum_raw_bytes": 25 * 1024 * 1024,
		"attachment_capacity": map[string]any{
			"used": 4096, "max": 8192, "remaining": 4096,
			"unlimited": false, "near_limit": false,
			"at_limit": false, "over_limit": false,
			"private_account_id": "leaked-status-account",
		},
		"private_policy_revision": "leaked-policy-revision",
	}
}

// Emails returns the shared cell response used by proxy and acceptance tests.
func Emails() any {
	now := time.Date(2026, 7, 21, 20, 1, 2, 0, time.UTC)
	lease := now.Add(time.Minute)
	return client.AgentEmailPage{
		Messages: []client.AgentEmailMessage{{
			ID: "claimable-message-id", MailboxID: "private-mailbox-id", OwnerAgentID: "private-owner-id",
			AddressID: "private-address-id", Provider: "cloudflare", EnvelopeSender: "sender@example.net",
			EnvelopeRecipient: "private-recipient", AgentSegment: "private-agent-segment",
			RealmLabel: "private-realm-label", SubaddressTag: "private-subaddress",
			RawSizeBytes: 2048, ParseState: "parsed", HeaderFrom: "leaked-header-from",
			HeaderTo: "leaked-header-to", Subject: "safe subject", MIMEMessageID: "leaked-mime-id",
			MessageDate: &now, AttachmentCount: 2,
			AttachmentStorageBytes: 1536, RetainedAttachmentStorageBytes: 0,
			PayloadRetentionState: "omitted_capacity",
			SPFResult:             "pass", DKIMResult: "pass",
			DMARCResult: "pass", SpamVerdict: "none", SenderVerificationState: "unverified",
			PossibleDuplicate: true, PossibleDuplicateOfMessage: "leaked-duplicate-id",
			ReceivedAt: now, Folder: "inbox", DeliveredAt: now,
			ReadState: client.AgentEmailReadState{State: "unread"},
			Processing: client.AgentEmailProcessing{
				State: "claimed", Generation: 9, FailureCount: 2,
				ClaimID: "leaked-claim-id", LeaseExpiresAt: &lease,
			},
			Text: "leaked decoded body", TextKind: "plain",
		}},
		NextCursor: "cursor-2",
	}
}

// SentEmails returns the shared cell response used by proxy and acceptance tests.
func SentEmails() any {
	now := time.Date(2026, 8, 17, 14, 15, 16, 0, time.UTC)
	providerStarted := now.Add(time.Second)
	accepted := now.Add(2 * time.Second)
	delivered := now.Add(3 * time.Second)
	return map[string]any{
		"messages": []map[string]any{{
			"id": "leaked-sent-message-id", "account_id": "leaked-account-id",
			"realm_id": "leaked-realm-id", "owner_agent_id": "leaked-owner-id",
			"from": "dash@witmail.net", "reply_to": "reply@witmail.net",
			"to": "person@example.net", "subject": "safe sent subject",
			"state": "delivered", "provider_state": "delivered",
			"provider": "leaked-provider-name", "provider_message_id": "leaked-provider-message-id",
			"error_code": "provider_timeout", "request_kind": "send", "attempt_count": 2,
			"reply_to_inbound_message_id": "leaked-inbound-reply-target",
			"thread_key":                  "leaked-thread-key", "text": "leaked submitted body",
			"future_private_field": "leaked-future-field",
			"queued_at":            now, "created_at": now, "updated_at": delivered,
			"provider_started_at": providerStarted, "accepted_at": accepted, "delivered_at": delivered,
		}},
		"next_cursor": "leaked-sent-cursor",
	}
}

// Facts returns the shared cell response used by proxy and acceptance tests.
func Facts() any {
	return map[string]any{"facts": []client.Fact{
		{ID: "fact_1", Subject: "self", Predicate: "identity/name", Value: json.RawMessage(`"Scott"`)},
		{ID: "fact_2", Subject: "self", Predicate: "identity/ssn", Sensitive: true,
			Value: json.RawMessage(`"leaked-fact-value"`), SourceRef: "leaked-source-ref"},
	}}
}

// LeakySecret includes synthetic forbidden material to exercise the browser projection.
func LeakySecret() map[string]any {
	return map[string]any{
		"id": "sec_1", "name": "prod-db", "template": "credential",
		"tags": []string{"prod"}, "lifecycle": "active",
		"sensitive_field_count": 1,
		"created_at":            "2026-07-01T00:00:00Z",
		"updated_at":            "2026-07-02T00:00:00Z",
		"ciphertext":            "leaked-ciphertext",
		"plaintext":             "leaked-plaintext",
		"private_value":         SecretCanary,
		"wrapped_dek":           "leaked-wrapped-dek",
		"fields": []map[string]any{
			{
				"id": "fld_1", "name": "password", "kind": "password", "sensitive": true,
				"public_value": "leaked-public-value",
				"sealed": map[string]any{
					"ciphertext": "leaked-ciphertext",
					"aad":        "leaked-aad",
					"nonce":      "leaked-nonce",
					"dek":        map[string]any{"wrapped_dek": "leaked-wrapped-dek", "key_material": "leaked-key-material"},
				},
			},
			{"id": "fld_2", "name": "username", "kind": "text", "sensitive": false,
				"public_value": "leaked-public-value"},
		},
	}
}
