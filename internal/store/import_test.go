package store

import (
	"context"
	"errors"
	"maps"
	"reflect"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/witwave-ai/witself/internal/plans"
)

// TestValidateAndRecordEnforcesAccountScoping pins the row-content boundary:
// the FK constraints alone would happily accept operators, realms, tokens,
// or agents pointing at ANOTHER tenant on the cell, so this validator is
// the sole gate against a tampered R2 archive grafting rows onto a victim
// account. Each case is a concrete tamper pattern.
func TestValidateAndRecordEnforcesAccountScoping(t *testing.T) {
	const acc = "acc_target"
	tests := []struct {
		name   string
		table  string
		row    map[string]any
		setup  func(*importCtx)
		wantOK bool
		want   string // error substring; ignored when wantOK
	}{
		{
			name:   "accounts row must equal manifest id",
			table:  "accounts",
			row:    map[string]any{"id": "acc_someone_else", "status": "suspended"},
			wantOK: false, want: "does not match manifest",
		},
		{
			name:   "accounts row cannot claim is_default",
			table:  "accounts",
			row:    map[string]any{"id": acc, "is_default": true, "status": "suspended"},
			wantOK: false, want: "is_default=true",
		},
		{
			name:   "second accounts row is refused",
			table:  "accounts",
			row:    map[string]any{"id": acc, "status": "suspended"},
			setup:  func(ic *importCtx) { ic.accounts = 1 },
			wantOK: false, want: "more than one accounts row",
		},
		{
			name:   "operators row missing account_id is refused",
			table:  "operators",
			row:    map[string]any{"id": "op_1", "role": "account_owner"},
			wantOK: false, want: "missing account_id",
		},
		{
			name:   "operators row for the wrong account is refused",
			table:  "operators",
			row:    map[string]any{"id": "op_1", "account_id": "acc_victim"},
			wantOK: false, want: "does not match manifest",
		},
		{
			name:   "tokens row grafting onto a victim account is refused",
			table:  "tokens",
			row:    map[string]any{"id": "tok_1", "account_id": "acc_victim", "operator_id": "op_v"},
			wantOK: false, want: "does not match manifest",
		},
		{
			name:  "tokens row referencing an operator not in this archive is refused",
			table: "tokens",
			row: map[string]any{
				"id": "tok_1", "account_id": acc, "operator_id": "op_stranger",
				"kind": "operator", "access_profile": AccessProfileFull,
			},
			wantOK: false, want: "not present in this archive",
		},
		{
			name:  "tokens row with only agent_id set, and that agent not in the archive, is refused",
			table: "tokens",
			row: map[string]any{
				"id": "tok_1", "account_id": acc, "agent_id": "agt_stranger",
				"kind": "agent", "access_profile": AccessProfileFull,
			},
			wantOK: false, want: "not present in this archive",
		},
		{
			name:  "agents row with a foreign realm_id is refused",
			table: "agents",
			row: map[string]any{
				"id": "agt_1", "realm_id": "rlm_victim", "name": "archivist",
			},
			wantOK: false, want: "not present in this archive",
		},
		{
			name:  "agents row with a realm that arrived earlier in the archive is accepted",
			table: "agents",
			row: map[string]any{
				"id": "agt_1", "realm_id": "rlm_ok", "name": "archivist",
			},
			setup:  func(ic *importCtx) { ic.realms["rlm_ok"] = true },
			wantOK: true,
		},
		{
			name:  "tokens row referencing operator + agent that arrived earlier is accepted",
			table: "tokens",
			row: map[string]any{
				"id": "tok_1", "account_id": acc,
				"operator_id": "op_ok", "agent_id": "agt_ok",
				"kind": "agent", "access_profile": AccessProfileCuratorPreview,
			},
			setup: func(ic *importCtx) {
				ic.operators["op_ok"] = true
				ic.agents["agt_ok"] = true
			},
			wantOK: true,
		},
		{
			name:  "tokens row with JSON null for the unused FK slot is accepted",
			table: "tokens",
			row: map[string]any{
				"id": "tok_bootstrap", "account_id": acc,
				"operator_id": nil, "agent_id": nil,
				"kind": "bootstrap", "access_profile": AccessProfileFull,
			},
			wantOK: true,
		},
		{
			name:  "operator token cannot carry curator profile",
			table: "tokens",
			row: map[string]any{
				"id": "tok_operator_curator", "account_id": acc,
				"operator_id": "op_ok", "kind": "operator",
				"access_profile": AccessProfileCuratorApply,
			},
			setup: func(ic *importCtx) { ic.operators["op_ok"] = true },
			want:  "access_profile is invalid",
		},
		{
			name:  "agent token rejects unknown profile",
			table: "tokens",
			row: map[string]any{
				"id": "tok_agent_unknown", "account_id": acc,
				"agent_id": "agt_ok", "kind": "agent", "access_profile": "curator-admin",
			},
			setup: func(ic *importCtx) { ic.agents["agt_ok"] = true },
			want:  "access_profile is invalid",
		},
		{
			// Regression: slice 1 of the audit-log feature initially
			// left account_events out of the second switch, so every
			// export/import broke on the first event row. This case
			// pins that gap closed.
			name:  "account_events row with account_id matching manifest is accepted",
			table: "account_events",
			row: map[string]any{
				"id": "evt_1", "account_id": acc, "actor_kind": "owner",
				"actor_id": "op_1", "verb": "account.suspended.owner",
				"metadata": map[string]any{},
			},
			wantOK: true,
		},
		{
			name:  "account_events row for a different account is refused",
			table: "account_events",
			row: map[string]any{
				"id": "evt_1", "account_id": "acc_victim", "actor_kind": "owner",
				"actor_id": "op_1", "verb": "account.suspended.owner",
			},
			wantOK: false, want: "does not match manifest",
		},
		{
			name:  "support_tickets row scoped to the manifest account is accepted and recorded",
			table: "support_tickets",
			row: map[string]any{
				"id": "tkt_1", "account_id": acc,
				"opened_by_kind": "owner", "opened_by_id": "op_1",
				"subject": "help", "state": "awaiting_admin",
			},
			wantOK: true,
		},
		{
			name:  "support_tickets row for a different account is refused",
			table: "support_tickets",
			row: map[string]any{
				"id": "tkt_1", "account_id": "acc_victim",
				"opened_by_kind": "owner", "opened_by_id": "op_1",
				"subject": "help", "state": "open",
			},
			wantOK: false, want: "does not match manifest",
		},
		{
			name:  "support_ticket_messages row for a ticket that arrived earlier is accepted",
			table: "support_ticket_messages",
			row: map[string]any{
				"id": "tkm_1", "account_id": acc, "ticket_id": "tkt_ok",
				"author_kind": "owner", "author_id": "op_1", "body": "hi",
			},
			setup:  func(ic *importCtx) { ic.tickets["tkt_ok"] = true },
			wantOK: true,
		},
		{
			name:  "support_ticket_messages row grafting onto a foreign ticket is refused",
			table: "support_ticket_messages",
			row: map[string]any{
				"id": "tkm_1", "account_id": acc, "ticket_id": "tkt_victim",
				"author_kind": "fleet_admin", "author_id": "sarah", "body": "gotcha",
			},
			wantOK: false, want: "not present in this archive",
		},
		{
			name:  "support_ticket_messages row missing ticket_id is refused",
			table: "support_ticket_messages",
			row: map[string]any{
				"id": "tkm_1", "account_id": acc,
				"author_kind": "owner", "author_id": "op_1", "body": "hi",
			},
			wantOK: false, want: "missing ticket_id",
		},
		{
			name:  "transcript conversation with archive-local realm and agent is accepted",
			table: "transcript_conversations",
			row: map[string]any{
				"id": "trn_1", "account_id": acc, "realm_id": "rlm_ok",
				"owner_agent_id": "agt_ok",
			},
			setup: func(ic *importCtx) {
				ic.realms["rlm_ok"] = true
				ic.agents["agt_ok"] = true
			},
			wantOK: true,
		},
		{
			name:  "transcript conversation cannot graft a foreign agent",
			table: "transcript_conversations",
			row: map[string]any{
				"id": "trn_1", "account_id": acc, "realm_id": "rlm_ok",
				"owner_agent_id": "agt_victim",
			},
			setup:  func(ic *importCtx) { ic.realms["rlm_ok"] = true },
			wantOK: false, want: "agent",
		},
		{
			name:  "transcript entry with matching scope is accepted",
			table: "transcript_entries",
			row: map[string]any{
				"id": "ent_1", "account_id": acc, "transcript_id": "trn_1",
				"realm_id": "rlm_ok", "recorded_by_agent_id": "agt_ok",
				"sequence": float64(1), "role": "user", "body": "hello",
			},
			setup: func(ic *importCtx) {
				ic.agents["agt_ok"] = true
				ic.transcripts["trn_1"] = transcriptImportScope{realmID: "rlm_ok", ownerAgentID: "agt_ok"}
			},
			wantOK: true,
		},
		{
			name:  "transcript reply must target an earlier entry in the same transcript",
			table: "transcript_entries",
			row: map[string]any{
				"id": "ent_2", "account_id": acc, "transcript_id": "trn_1",
				"realm_id": "rlm_ok", "recorded_by_agent_id": "agt_ok",
				"sequence": float64(2), "role": "assistant", "body": "hello",
				"reply_to_entry_id": "ent_foreign",
			},
			setup: func(ic *importCtx) {
				ic.agents["agt_ok"] = true
				ic.transcripts["trn_1"] = transcriptImportScope{realmID: "rlm_ok", ownerAgentID: "agt_ok"}
				ic.entries["ent_foreign"] = "trn_other"
			},
			wantOK: false, want: "not an earlier entry",
		},
		{
			name:  "transcript usage with matching archived scope is accepted",
			table: "usage_events",
			row: map[string]any{
				"id": "usg_1", "account_id": acc, "realm_id": "rlm_ok", "agent_id": "agt_ok",
				"dimension": "transcript_entry_write", "quantity": float64(2), "unit": "entry",
				"subject_type": "transcript", "subject_id": "trn_1", "idempotency_key": "write:1",
			},
			setup: func(ic *importCtx) {
				ic.realms["rlm_ok"] = true
				ic.agents["agt_ok"] = true
				ic.agentRealms["agt_ok"] = "rlm_ok"
				ic.transcripts["trn_1"] = transcriptImportScope{realmID: "rlm_ok", ownerAgentID: "agt_ok"}
			},
			wantOK: true,
		},
		{
			name:  "transcript usage with a retention-pruned subject is accepted",
			table: "usage_events",
			row: map[string]any{
				"id": "usg_retained", "account_id": acc,
				"realm_id": "rlm_ok", "agent_id": "agt_ok",
				"dimension": "transcript_entry_write", "quantity": float64(2), "unit": "entry",
				"subject_type": "transcript", "subject_id": "trn_aaaaaaaaaaaaaaaa",
				"idempotency_key": "write:pruned",
			},
			setup: func(ic *importCtx) {
				ic.realms["rlm_ok"] = true
				ic.agents["agt_ok"] = true
				ic.agentRealms["agt_ok"] = "rlm_ok"
			},
			wantOK: true,
		},
		{
			name:  "transcript usage cannot graft a present foreign transcript",
			table: "usage_events",
			row: map[string]any{
				"id": "usg_present_foreign", "account_id": acc,
				"realm_id": "rlm_ok", "agent_id": "agt_ok",
				"dimension": "transcript_entry_read", "quantity": float64(1), "unit": "entry",
				"subject_type": "transcript", "subject_id": "trn_bbbbbbbbbbbbbbbb",
				"idempotency_key": "read:present-foreign",
			},
			setup: func(ic *importCtx) {
				ic.realms["rlm_ok"] = true
				ic.agents["agt_ok"] = true
				ic.agentRealms["agt_ok"] = "rlm_ok"
				ic.transcripts["trn_bbbbbbbbbbbbbbbb"] = transcriptImportScope{
					realmID: "rlm_other", ownerAgentID: "agt_other",
				}
			},
			want: "does not belong to its agent scope",
		},
		{
			name:  "transcript usage cannot graft a foreign transcript",
			table: "usage_events",
			row: map[string]any{
				"id": "usg_1", "account_id": acc, "realm_id": "rlm_ok", "agent_id": "agt_ok",
				"dimension": "transcript_entry_read", "quantity": float64(1), "unit": "entry",
				"subject_type": "transcript", "subject_id": "trn_foreign", "idempotency_key": "read:1",
			},
			setup: func(ic *importCtx) {
				ic.realms["rlm_ok"] = true
				ic.agents["agt_ok"] = true
				ic.agentRealms["agt_ok"] = "rlm_ok"
			},
			wantOK: false, want: "does not belong to its agent scope",
		},
		{
			name:  "usage rollup cannot cross agent realms",
			table: "usage_rollups",
			row: map[string]any{
				"account_id": acc, "realm_id": "rlm_ok", "agent_id": "agt_other",
				"dimension": "transcript_created", "unit": "transcript", "bucket": "day",
				"quantity": float64(1), "event_count": float64(1),
			},
			setup: func(ic *importCtx) {
				ic.realms["rlm_ok"] = true
				ic.agents["agt_other"] = true
				ic.agentRealms["agt_other"] = "rlm_other"
			},
			wantOK: false, want: "outside realm",
		},
		{
			name:  "message with archive-local same-realm agents is accepted",
			table: "agent_messages",
			row: map[string]any{
				"id": "msg_1", "account_id": acc, "realm_id": "rlm_ok",
				"from_agent_id": "agt_from", "to_agent_id": "agt_to",
				"audience_kind": "agent", "audience_fingerprint": "",
				"kind": "note", "body": "hello", "thread_id": "thr_1", "causal_depth": 1,
				"created_at": "2026-07-15T10:00:00Z",
			},
			setup: func(ic *importCtx) {
				ic.realms["rlm_ok"] = true
				ic.agents["agt_from"] = true
				ic.agents["agt_to"] = true
				ic.agentRealms["agt_from"] = "rlm_ok"
				ic.agentRealms["agt_to"] = "rlm_ok"
			},
			wantOK: true,
		},
		{
			name:  "message cannot cross archived realms",
			table: "agent_messages",
			row: map[string]any{
				"id": "msg_1", "account_id": acc, "realm_id": "rlm_ok",
				"from_agent_id": "agt_from", "to_agent_id": "agt_other",
				"audience_kind": "agent", "audience_fingerprint": "",
				"created_at": "2026-07-15T10:00:00Z",
			},
			setup: func(ic *importCtx) {
				ic.realms["rlm_ok"] = true
				ic.agents["agt_from"] = true
				ic.agents["agt_other"] = true
				ic.agentRealms["agt_from"] = "rlm_ok"
				ic.agentRealms["agt_other"] = "rlm_other"
			},
			wantOK: false, want: "must belong to realm",
		},
		{
			name:  "delivery must match its message recipient",
			table: "agent_message_deliveries",
			row: map[string]any{
				"message_id": "msg_1", "account_id": acc, "realm_id": "rlm_ok",
				"recipient_agent_id": "agt_wrong", "state": "delivered",
			},
			setup: func(ic *importCtx) {
				ic.messages["msg_1"] = messageImportScope{realmID: "rlm_ok", fromAgentID: "agt_from", toAgentID: "agt_to"}
				ic.agents["agt_wrong"] = true
				ic.agentRealms["agt_wrong"] = "rlm_ok"
			},
			wantOK: false, want: "does not match message recipient",
		},
		{
			name:  "matching message delivery is accepted",
			table: "agent_message_deliveries",
			row: map[string]any{
				"message_id": "msg_1", "account_id": acc, "realm_id": "rlm_ok",
				"recipient_agent_id": "agt_to", "state": "delivered",
				"processing_state": "available", "processing_generation": 0,
				"failure_count": 0,
				"claim_id":      nil, "claim_key_hash": "", "lease_expires_at": nil,
				"completed_at": nil, "complete_key_hash": "", "result_message_id": nil,
			},
			setup: func(ic *importCtx) {
				ic.messages["msg_1"] = messageImportScope{realmID: "rlm_ok", fromAgentID: "agt_from", toAgentID: "agt_to"}
				ic.agents["agt_to"] = true
				ic.agentRealms["agt_to"] = "rlm_ok"
			},
			wantOK: true,
		},
		{
			name:   "unknown table is refused",
			table:  "audit_log",
			row:    map[string]any{"id": "audit_1", "account_id": acc},
			wantOK: false, want: "not importable",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ic := newImportCtx(acc)
			if tc.setup != nil {
				tc.setup(ic)
			}
			err := ic.validateAndRecord(tc.table, tc.row)
			if tc.wantOK {
				if err != nil {
					t.Fatalf("wantOK, got err = %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("wanted error containing %q, got nil", tc.want)
			}
			if !errors.Is(err, ErrArchiveContent) {
				t.Errorf("error not wrapped in ErrArchiveContent: %v", err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %v, want substring %q", err, tc.want)
			}
		})
	}
}

func TestValidateImportedMessageReplies(t *testing.T) {
	message := func(from, to, thread, parent string, explicitDepth ...int64) messageImportScope {
		depth := int64(1)
		if parent != "" {
			depth = 2
		}
		if len(explicitDepth) > 0 {
			depth = explicitDepth[0]
		}
		return messageImportScope{
			realmID: "rlm_1", fromAgentID: from, toAgentID: to,
			threadID: thread, replyToMessageID: parent, causalDepth: depth,
		}
	}
	tests := []struct {
		name     string
		messages map[string]messageImportScope
		want     string
	}{
		{
			name: "root and alternating reply chain",
			messages: map[string]messageImportScope{
				"msg_reply_2": message("agt_a", "agt_b", "thr_1", "msg_reply_1", 3),
				"msg_root":    message("agt_a", "agt_b", "thr_1", ""),
				"msg_reply_1": message("agt_b", "agt_a", "thr_1", "msg_root"),
			},
		},
		{
			name: "missing parent",
			messages: map[string]messageImportScope{
				"msg_reply": message("agt_b", "agt_a", "thr_1", "msg_missing"),
			},
			want: "references missing parent",
		},
		{
			name: "self reply",
			messages: map[string]messageImportScope{
				"msg_reply": message("agt_b", "agt_a", "thr_1", "msg_reply"),
			},
			want: "replies to itself",
		},
		{
			name: "thread mismatch",
			messages: map[string]messageImportScope{
				"msg_root":  message("agt_a", "agt_b", "thr_1", ""),
				"msg_reply": message("agt_b", "agt_a", "thr_2", "msg_root"),
			},
			want: "does not preserve parent participants, realm, and thread",
		},
		{
			name: "participant mismatch",
			messages: map[string]messageImportScope{
				"msg_root":  message("agt_a", "agt_b", "thr_1", ""),
				"msg_reply": message("agt_c", "agt_a", "thr_1", "msg_root"),
			},
			want: "does not preserve parent participants, realm, and thread",
		},
		{
			name: "realm mismatch",
			messages: map[string]messageImportScope{
				"msg_root": message("agt_a", "agt_b", "thr_1", ""),
				"msg_reply": {
					realmID: "rlm_2", fromAgentID: "agt_b", toAgentID: "agt_a",
					threadID: "thr_1", replyToMessageID: "msg_root",
				},
			},
			want: "does not preserve parent participants, realm, and thread",
		},
		{
			name: "forged causal depth",
			messages: map[string]messageImportScope{
				"msg_root":  message("agt_a", "agt_b", "thr_1", ""),
				"msg_reply": message("agt_b", "agt_a", "thr_1", "msg_root", 99),
			},
			want: "does not match derived depth",
		},
		{
			name: "two-message cycle",
			messages: map[string]messageImportScope{
				"msg_a": message("agt_a", "agt_b", "thr_1", "msg_b"),
				"msg_b": message("agt_b", "agt_a", "thr_1", "msg_a"),
			},
			want: "reply graph contains a cycle",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := validateImportedMessageReplies(tc.messages)
			if tc.want == "" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want substring %q", err, tc.want)
			}
		})
	}
}

func TestNormalizeLegacyImportedMessageCausalDepthsUsesWholeGraph(t *testing.T) {
	messages := map[string]messageImportScope{
		"msg_reply_2": {
			realmID: "rlm_1", fromAgentID: "agt_a", toAgentID: "agt_b",
			threadID: "thr_1", replyToMessageID: "msg_reply_1", causalDepth: 1,
		},
		"msg_root": {
			realmID: "rlm_1", fromAgentID: "agt_a", toAgentID: "agt_b",
			threadID: "thr_1", causalDepth: 1,
		},
		"msg_reply_1": {
			realmID: "rlm_1", fromAgentID: "agt_b", toAgentID: "agt_a",
			threadID: "thr_1", replyToMessageID: "msg_root", causalDepth: 1,
		},
	}
	fake := &recordingExec{}
	if err := normalizeLegacyImportedMessageCausalDepths(
		context.Background(), fake, messages, "acc_1",
	); err != nil {
		t.Fatal(err)
	}
	if fake.calls != 3 {
		t.Fatalf("causal depth updates = %d, want 3", fake.calls)
	}
	if messages["msg_root"].causalDepth != 1 || messages["msg_reply_1"].causalDepth != 2 ||
		messages["msg_reply_2"].causalDepth != 3 {
		t.Fatalf("normalized legacy graph = %#v", messages)
	}
	if err := validateImportedMessageReplies(messages); err != nil {
		t.Fatalf("normalized legacy graph is not valid: %v", err)
	}
}

func TestImportedMessageProcessingShapeAndClaimNormalization(t *testing.T) {
	base := func(state string) map[string]any {
		return map[string]any{
			"processing_state": state, "processing_generation": 0,
			"failure_count": 0,
			"claim_id":      nil, "claim_key_hash": "", "lease_expires_at": nil,
			"completed_at": nil, "complete_key_hash": "", "result_message_id": nil,
		}
	}
	available := base(MessageProcessingAvailable)
	if got, err := validateImportedMessageProcessingShape(available); err != nil ||
		got.processingState != MessageProcessingAvailable || got.processingGeneration != 0 {
		t.Fatalf("available shape = %#v / %v", got, err)
	}

	claimed := base(MessageProcessingClaimed)
	claimed["processing_generation"] = 7
	claimed["failure_count"] = 2
	claimed["claim_id"] = "mcl_aaaaaaaaaaaaaaaa"
	claimed["claim_key_hash"] = strings.Repeat("a", 64)
	claimed["lease_expires_at"] = "2026-07-15T01:00:00Z"
	if err := newImportCtx("acc_1").normalizeImportedMessageClaim("agent_message_deliveries", claimed); err != nil {
		t.Fatal(err)
	}
	if claimed["processing_state"] != MessageProcessingAvailable || claimed["processing_generation"] != int64(8) ||
		claimed["failure_count"] != 2 ||
		claimed["claim_id"] != nil || claimed["claim_key_hash"] != "" || claimed["lease_expires_at"] != nil {
		t.Fatalf("normalized active claim = %#v", claimed)
	}

	completed := base(MessageProcessingCompleted)
	completed["processing_generation"] = 3
	completed["claim_id"] = "mcl_bbbbbbbbbbbbbbbb"
	completed["claim_key_hash"] = strings.Repeat("b", 64)
	completed["completed_at"] = "2026-07-15T01:00:00Z"
	completed["complete_key_hash"] = strings.Repeat("c", 64)
	completed["result_message_id"] = "msg_cccccccccccccccc"
	before := maps.Clone(completed)
	if err := newImportCtx("acc_1").normalizeImportedMessageClaim("agent_message_deliveries", completed); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(completed, before) {
		t.Fatalf("completed processing changed on import: %#v", completed)
	}

	invalid := []struct {
		name string
		edit func(map[string]any)
	}{
		{name: "missing field", edit: func(row map[string]any) { delete(row, "claim_id") }},
		{name: "available claim", edit: func(row map[string]any) { row["claim_id"] = "mcl_aaaaaaaaaaaaaaaa" }},
		{name: "claimed without lease", edit: func(row map[string]any) {
			row["processing_state"] = MessageProcessingClaimed
			row["processing_generation"] = 1
			row["claim_id"] = "mcl_aaaaaaaaaaaaaaaa"
			row["claim_key_hash"] = strings.Repeat("a", 64)
		}},
		{name: "invalid state", edit: func(row map[string]any) { row["processing_state"] = "running" }},
		{name: "negative failure count", edit: func(row map[string]any) { row["failure_count"] = -1 }},
		{name: "exhausted failure count", edit: func(row map[string]any) { row["failure_count"] = "4611686018427387904" }},
	}
	for _, tc := range invalid {
		t.Run(tc.name, func(t *testing.T) {
			row := base(MessageProcessingAvailable)
			tc.edit(row)
			if _, err := validateImportedMessageProcessingShape(row); err == nil {
				t.Fatalf("invalid processing shape accepted: %#v", row)
			}
		})
	}

	exhausted := base(MessageProcessingClaimed)
	exhausted["processing_generation"] = maxMessageProcessingGeneration
	exhausted["claim_id"] = "mcl_aaaaaaaaaaaaaaaa"
	exhausted["claim_key_hash"] = strings.Repeat("a", 64)
	exhausted["lease_expires_at"] = "2026-07-15T01:00:00Z"
	if err := newImportCtx("acc_1").normalizeImportedMessageClaim("agent_message_deliveries", exhausted); !errors.Is(err, ErrArchiveContent) {
		t.Fatalf("exhausted active claim error = %v", err)
	}
}

func TestValidateImportedMessageProcessingLinks(t *testing.T) {
	const (
		sourceID = "msg_aaaaaaaaaaaaaaaa"
		resultID = "msg_bbbbbbbbbbbbbbbb"
	)
	messages := map[string]messageImportScope{
		sourceID: {
			realmID: "rlm_1", fromAgentID: "agt_sender", toAgentID: "agt_worker", threadID: "thr_1",
		},
		resultID: {
			realmID: "rlm_1", fromAgentID: "agt_worker", toAgentID: "agt_sender",
			threadID: "thr_1", replyToMessageID: sourceID,
		},
	}
	deliveries := map[string]messageDeliveryImportScope{
		sourceID: {
			processingState: MessageProcessingCompleted, processingGeneration: 1,
			claimID: "mcl_aaaaaaaaaaaaaaaa", resultMessageID: resultID,
		},
		resultID: {processingState: MessageProcessingAvailable},
	}
	if err := validateImportedMessageProcessingLinks(messages, deliveries); err != nil {
		t.Fatal(err)
	}

	missing := maps.Clone(deliveries)
	missing[sourceID] = messageDeliveryImportScope{
		processingState: MessageProcessingCompleted, resultMessageID: "msg_cccccccccccccccc",
	}
	if err := validateImportedMessageProcessingLinks(messages, missing); err == nil || !strings.Contains(err.Error(), "missing") {
		t.Fatalf("missing result error = %v", err)
	}

	wrongMessages := maps.Clone(messages)
	wrong := wrongMessages[resultID]
	wrong.threadID = "thr_other"
	wrongMessages[resultID] = wrong
	if err := validateImportedMessageProcessingLinks(wrongMessages, deliveries); err == nil || !strings.Contains(err.Error(), "invalid") {
		t.Fatalf("wrong result relationship error = %v", err)
	}
}

func TestDecodeImportRowPreservesChangeSequencesAboveTwoToThe53(t *testing.T) {
	obj, err := decodeImportRow([]byte(`{
		"last_change_seq": 9007199254740992,
		"change_seq": 9007199254740993
	}`))
	if err != nil {
		t.Fatal(err)
	}
	clock, ok := importedNonnegativeInteger(obj["last_change_seq"])
	if !ok || clock != 9007199254740992 {
		t.Fatalf("last_change_seq = %d / %v", clock, ok)
	}
	change, ok := importedPositiveInteger(obj["change_seq"])
	if !ok || change != 9007199254740993 {
		t.Fatalf("change_seq = %d / %v", change, ok)
	}
	if clock == change {
		t.Fatal("adjacent BIGINT change sequences collapsed to one float64 value")
	}
}

func TestDecodeImportRowPreservesInt64Boundary(t *testing.T) {
	obj, err := decodeImportRow([]byte(`{
		"valid": 9223372036854775807,
		"overflow": 9223372036854775808
	}`))
	if err != nil {
		t.Fatal(err)
	}
	value, ok := importedNonnegativeInteger(obj["valid"])
	if !ok || value != int64(9223372036854775807) {
		t.Fatalf("max BIGINT = %d / %v", value, ok)
	}
	if value, ok := importedNonnegativeInteger(obj["overflow"]); ok {
		t.Fatalf("overflow BIGINT accepted as %d", value)
	}
}

// TestValidateAndRecordAccumulatesOverAStream ties the guarantees together:
// the ORDER the exporter writes tables (accounts, operators, realms,
// agents, tokens) matches the order rows must be recorded so later rows can
// reference earlier ids. The test walks a legal archive stream by feeding
// its rows to the validator in that order.
func TestValidateAndRecordAccumulatesOverAStream(t *testing.T) {
	const acc = "acc_stream"
	ic := newImportCtx(acc)

	feed := func(table string, row map[string]any) {
		t.Helper()
		if err := ic.validateAndRecord(table, row); err != nil {
			t.Fatalf("%s row failed: %v", table, err)
		}
	}
	feed("accounts", map[string]any{"id": acc, "status": "suspended", "is_default": false})
	feed("operators", map[string]any{"id": "op_root", "account_id": acc, "role": "account_owner"})
	feed("realms", map[string]any{"id": "rlm_default", "account_id": acc, "name": "default"})
	feed("agents", map[string]any{"id": "agt_1", "realm_id": "rlm_default", "name": "archivist"})
	feed("agents", map[string]any{"id": "agt_2", "realm_id": "rlm_default", "name": "coordinator"})
	feed("tokens", map[string]any{
		"id": "tok_op", "account_id": acc, "operator_id": "op_root", "kind": "operator",
		"access_profile": AccessProfileFull,
	})
	feed("tokens", map[string]any{
		"id": "tok_agent", "account_id": acc, "agent_id": "agt_1", "kind": "agent",
		"access_profile": AccessProfileCuratorApply,
	})
	// Support tickets stream after account_events; messages FK-depend on
	// tickets recorded here.
	feed("support_tickets", map[string]any{
		"id": "tkt_1", "account_id": acc,
		"opened_by_kind": "owner", "opened_by_id": "op_root",
		"subject": "help", "state": "awaiting_admin",
	})
	feed("support_ticket_messages", map[string]any{
		"id": "tkm_1", "ticket_id": "tkt_1", "account_id": acc,
		"author_kind": "owner", "author_id": "op_root", "body": "please",
	})
	feed("transcript_conversations", map[string]any{
		"id": "trn_1", "account_id": acc, "realm_id": "rlm_default",
		"owner_agent_id": "agt_1",
	})
	feed("transcript_entries", map[string]any{
		"id": "ent_1", "account_id": acc, "transcript_id": "trn_1",
		"realm_id": "rlm_default", "recorded_by_agent_id": "agt_1",
		"sequence": float64(1), "role": "user", "body": "hello",
	})
	feed("transcript_entries", map[string]any{
		"id": "ent_2", "account_id": acc, "transcript_id": "trn_1",
		"realm_id": "rlm_default", "recorded_by_agent_id": "agt_1",
		"sequence": float64(2), "role": "assistant", "body": "hi",
		"reply_to_entry_id": "ent_1",
	})
	feed("usage_events", map[string]any{
		"id": "usg_1", "account_id": acc, "realm_id": "rlm_default", "agent_id": "agt_1",
		"dimension": "transcript_entry_write", "quantity": float64(2), "unit": "entry",
		"subject_type": "transcript", "subject_id": "trn_1", "idempotency_key": "write:1",
	})
	feed("usage_rollups", map[string]any{
		"account_id": acc, "realm_id": "rlm_default", "agent_id": "agt_1",
		"dimension": "transcript_entry_write", "unit": "entry", "bucket": "day",
		"quantity": float64(2), "event_count": float64(1),
	})
	feed("agent_messages", map[string]any{
		"id": "msg_1", "account_id": acc, "realm_id": "rlm_default",
		"from_agent_id": "agt_1", "to_agent_id": "agt_2",
		"audience_kind": "agent", "audience_fingerprint": "",
		"kind": "handoff", "body": "your turn", "thread_id": "thr_1", "causal_depth": 1,
		"created_at": "2026-07-15T10:00:00Z",
	})
	feed("agent_message_deliveries", map[string]any{
		"message_id": "msg_1", "account_id": acc, "realm_id": "rlm_default",
		"recipient_agent_id": "agt_2", "state": "delivered",
		"processing_state": "available", "processing_generation": 0,
		"failure_count": 0,
		"claim_id":      nil, "claim_key_hash": "", "lease_expires_at": nil,
		"completed_at": nil, "complete_key_hash": "", "result_message_id": nil,
	})

	if ic.accounts != 1 {
		t.Errorf("accounts count = %d, want 1", ic.accounts)
	}
	if !ic.operators["op_root"] || !ic.realms["rlm_default"] || !ic.agents["agt_1"] {
		t.Error("ids not recorded across a legal stream")
	}
	if !ic.tickets["tkt_1"] {
		t.Error("support ticket id not recorded across a legal stream")
	}
	if _, ok := ic.transcripts["trn_1"]; !ok || ic.entries["ent_2"] != "trn_1" {
		t.Error("transcript ids not recorded across a legal stream")
	}
	if _, ok := ic.messages["msg_1"]; !ok {
		t.Error("message id not recorded across a legal stream")
	} else if _, ok := ic.deliveries["msg_1\x00agt_2"]; !ok {
		t.Error("message and delivery ids not recorded across a legal stream")
	}
}

// TestInsertProjectedRejectsUnlistedColumn — the column allowlist doubles as
// the SQL-identifier boundary. A JSON key outside the per-table set must
// refuse before any SQL runs.
func TestInsertProjectedRejectsUnlistedColumn(t *testing.T) {
	fake := &recordingExec{}
	err := insertProjected(context.Background(), fake, "accounts",
		map[string]any{"id": "acc_x", "status": "suspended", "; DROP TABLE": "gotcha"},
		[]byte(`{"id":"acc_x"}`))
	if !errors.Is(err, ErrArchiveContent) {
		t.Errorf("error not ErrArchiveContent: %v", err)
	}
	if fake.calls != 0 {
		t.Errorf("SQL executed despite unlisted column: %d calls", fake.calls)
	}
}

// TestInsertProjectedProjectsOnlyRowKeys — the whole point of the projected
// insert is that unlisted columns take their DEFAULT instead of an explicit
// NULL from jsonb_populate_record. Confirm by inspecting the generated SQL:
// only the JSON's keys appear.
func TestInsertProjectedProjectsOnlyRowKeys(t *testing.T) {
	fake := &recordingExec{}
	err := insertProjected(context.Background(), fake, "accounts",
		map[string]any{"id": "acc_x", "status": "suspended"},
		[]byte(`{"id":"acc_x","status":"suspended"}`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if fake.calls != 1 {
		t.Fatalf("calls = %d, want 1", fake.calls)
	}
	got := fake.lastSQL
	// Column list appears in the two-column projected form. Every listed
	// column MUST be in the row's JSON — no other column may appear.
	for _, col := range []string{"display_name", "email", "created_at", "is_default"} {
		if strings.Contains(got, col) {
			t.Errorf("SQL mentions column %q the row did not carry — DEFAULT will be bypassed: %s", col, got)
		}
	}
	if !strings.Contains(got, "id, status") || !strings.Contains(got, "SELECT id, status") {
		t.Errorf("SQL does not project only (id, status): %s", got)
	}
}

// TestInsertProjectedMapsUniqueViolationOnAccounts — the concurrent-import
// race path: a 23505 on the accounts insert must surface as ErrAccountExists,
// which the server maps to 409, not the generic 500.
func TestInsertProjectedMapsUniqueViolationOnAccounts(t *testing.T) {
	fake := &recordingExec{err: &pgconn.PgError{Code: "23505", ConstraintName: "accounts_pkey"}}
	err := insertProjected(context.Background(), fake, "accounts",
		map[string]any{"id": "acc_x", "status": "suspended"},
		[]byte(`{"id":"acc_x"}`))
	if !errors.Is(err, ErrAccountExists) {
		t.Errorf("23505 on accounts insert = %v, want ErrAccountExists", err)
	}
}

// TestInsertProjectedDoesNotMapUniqueViolationOnOtherTables — 23505 on
// tokens (a token_hash collision) is a legitimate archive-vs-cell conflict
// that should NOT masquerade as "account already exists"; it should bubble
// up as a generic error the caller can log verbatim.
func TestInsertProjectedDoesNotMapUniqueViolationOnOtherTables(t *testing.T) {
	fake := &recordingExec{err: &pgconn.PgError{Code: "23505", ConstraintName: "tokens_token_hash_key"}}
	err := insertProjected(context.Background(), fake, "tokens",
		map[string]any{"id": "tok_x", "account_id": "acc_x", "token_hash": "abcd"},
		[]byte(`{"id":"tok_x"}`))
	if errors.Is(err, ErrAccountExists) {
		t.Error("tokens 23505 masqueraded as ErrAccountExists — mapping is too broad")
	}
	if err == nil {
		t.Error("expected the pg error to bubble up")
	}
}

// recordingExec is a pgxExec that records what SQL was sent (and optionally
// fails with a caller-supplied error), letting the tests inspect the
// generated INSERT text without a live database.
type recordingExec struct {
	calls   int
	lastSQL string
	err     error
}

func (r *recordingExec) Exec(_ context.Context, sql string, _ ...any) (pgconn.CommandTag, error) {
	r.calls++
	r.lastSQL = sql
	return pgconn.CommandTag{}, r.err
}

// TestValidateAndRecordPlanShapes: the plan snapshot columns are decoded into
// typed Go values on every read, so a malformed archive value would import
// cleanly and then poison the account (every read and every gated create
// fails). Content-hostile streams must land nothing — the shapes are refused
// at validate time. Absent keys stay legal: archives from before migration
// 0017 fall back to the column defaults.
func TestValidateAndRecordPlanShapes(t *testing.T) {
	const acc = "acc_target"
	fencedHash, err := plans.SnapshotHash(
		"standard",
		map[string]int64{"agents": 250, "realms": 10},
		map[string]int64{plans.TranscriptRetentionDaysPolicy: 90},
		[]string{"memory", "facts", "secrets"},
	)
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name   string
		row    map[string]any
		wantOK bool
		want   string
	}{
		{
			name: "well-formed snapshot is accepted",
			row: map[string]any{"id": acc, "plan": "standard",
				"plan_limits":   map[string]any{"agents": float64(250), "realms": float64(10)},
				"plan_features": []any{"memory", "facts", "secrets"}},
			wantOK: true,
		},
		{
			name: "matching fenced snapshot is accepted",
			row: map[string]any{
				"id": acc, "plan": "standard",
				"plan_limits":            map[string]any{"agents": float64(250), "realms": float64(10)},
				"plan_policies":          map[string]any{plans.TranscriptRetentionDaysPolicy: float64(90)},
				"plan_features":          []any{"memory", "facts", "secrets"},
				"plan_snapshot_revision": float64(7),
				"plan_snapshot_hash":     fencedHash,
			},
			wantOK: true,
		},
		{
			name: "fenced snapshot hash must match payload",
			row: map[string]any{
				"id": acc, "plan": "standard",
				"plan_limits":            map[string]any{"agents": float64(250), "realms": float64(10)},
				"plan_policies":          map[string]any{plans.TranscriptRetentionDaysPolicy: float64(30)},
				"plan_features":          []any{"memory", "facts", "secrets"},
				"plan_snapshot_revision": float64(7),
				"plan_snapshot_hash":     fencedHash,
			},
			wantOK: false, want: "snapshot hash does not match payload",
		},
		{
			name:   "absent plan keys are accepted (pre-0017 archives)",
			row:    map[string]any{"id": acc, "status": "suspended"},
			wantOK: true,
		},
		{
			name:   "plan must be a string",
			row:    map[string]any{"id": acc, "plan": 7},
			wantOK: false, want: "plan must be a string",
		},
		{
			name:   "null plan_limits is refused (NOT NULL column)",
			row:    map[string]any{"id": acc, "plan_limits": nil},
			wantOK: false, want: "plan_limits must be an object",
		},
		{
			name:   "plan_limits must be an object",
			row:    map[string]any{"id": acc, "plan_limits": "junk"},
			wantOK: false, want: "plan_limits must be an object",
		},
		{
			name:   "plan_limits values must be integers",
			row:    map[string]any{"id": acc, "plan_limits": map[string]any{"agents": 2.5}},
			wantOK: false, want: `plan_limits["agents"] must be an integer`,
		},
		{
			name:   "plan_features must be an array",
			row:    map[string]any{"id": acc, "plan_features": map[string]any{"x": true}},
			wantOK: false, want: "plan_features must be an array",
		},
		{
			name:   "plan_features entries must be strings",
			row:    map[string]any{"id": acc, "plan_features": []any{"memory", 3}},
			wantOK: false, want: "entries must be strings",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ic := newImportCtx(acc)
			err := ic.validateAndRecord("accounts", tc.row)
			if tc.wantOK {
				if err != nil {
					t.Fatalf("wantOK, got err = %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("wanted error containing %q, got nil", tc.want)
			}
			if !errors.Is(err, ErrArchiveContent) {
				t.Errorf("error not wrapped in ErrArchiveContent: %v", err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %v, want substring %q", err, tc.want)
			}
		})
	}
}

func TestValidateAndRecordPlacementPolicyShape(t *testing.T) {
	const acc = "acc_target"
	tests := []struct {
		name   string
		row    map[string]any
		wantOK bool
		want   string
	}{
		{
			name: "well-formed placement policy is accepted",
			row: map[string]any{"id": acc, "placement_policy": map[string]any{
				"preferred_regions":  []any{"usw2", "use1"},
				"preferred_channels": []any{"stable", "edge", "experimental"},
				"rebalance_on":       []any{"cloud", "channel"},
			}},
			wantOK: true,
		},
		{
			name:   "absent placement_policy is accepted (pre-0018 archives)",
			row:    map[string]any{"id": acc, "status": "suspended"},
			wantOK: true,
		},
		{
			name:   "placement_policy must be an object",
			row:    map[string]any{"id": acc, "placement_policy": []any{"nope"}},
			wantOK: false, want: "placement_policy must be an object",
		},
		{
			name:   "placement_policy rejects unknown regions",
			row:    map[string]any{"id": acc, "placement_policy": map[string]any{"preferred_regions": []any{"use9"}}},
			wantOK: false, want: "unknown value",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ic := newImportCtx(acc)
			err := ic.validateAndRecord("accounts", tc.row)
			if tc.wantOK {
				if err != nil {
					t.Fatalf("wantOK, got err = %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("wanted error containing %q, got nil", tc.want)
			}
			if !errors.Is(err, ErrArchiveContent) {
				t.Errorf("error not wrapped in ErrArchiveContent: %v", err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %v, want substring %q", err, tc.want)
			}
		})
	}
}
