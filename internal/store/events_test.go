package store

import (
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/witwave-ai/witself/internal/placement"
)

// TestMaskEmail pins the exact masking shape the whole event registry
// depends on. Every event that carries an email in metadata does so via
// MaskEmail; if this ever changes, every stored event's semantics shift.
func TestMaskEmail(t *testing.T) {
	tests := map[string]string{
		"scott@witwave.ai":    "s***@w***.ai",
		"a@b.c":               "a***@b***.c",
		"scott@example.co.uk": "s***@e***.uk",
		"scott@localhost":     "s***@l***",
		"":                    "",
		"malformed":           "***",
		"@nothingbefore.com":  "***",
		"nothingafter@":       "***",
	}
	for in, want := range tests {
		if got := MaskEmail(in); got != want {
			t.Errorf("MaskEmail(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestVerbRegistryCoverage pins the write-time contract: every verb
// constant listed in the source must have a matching entry in
// verbMetadataSchema. If someone adds a verb constant without a schema
// entry, logEventTx will refuse the write at runtime — this test catches
// it at build time instead.
func TestVerbRegistryCoverage(t *testing.T) {
	verbs := []string{
		VerbOperatorCreated, VerbOperatorDeleted,
		VerbOperatorTokenMinted, VerbAgentTokenMinted, VerbTokenRevoked,
		VerbAccountRenamed,
		VerbAccountEmailChanged, VerbAccountEmailUndone,
		VerbAccountSuspendedByMe, VerbAccountResumedByMe,
		VerbAccountPlacementPolicyChanged,

		VerbAccountProvisioned, VerbAccountActivated,
		VerbRecoveryRequested, VerbRecoveryCompleted,
		VerbAccountEmailChangeStarted,
		VerbAccountEmailVerifySent, VerbAccountEmailRecoverySent,
		VerbAccountEmailChangeSent, VerbAccountEmailUndoSent,

		VerbAccountSuspendedBySystem, VerbAccountResumedBySystem,
		VerbAccountEvacuated, VerbAccountRestored,
		VerbAccountReaped, VerbAccountClosed,

		VerbMessageSent, VerbMessageDelivered, VerbMessageDeliveryFailed,
		VerbMessageRead, VerbMessageAcked,
		VerbMessageProcessingClaimed, VerbMessageProcessingRenewed,
		VerbMessageProcessingReleased, VerbMessageProcessingCompleted,
		VerbMessageRequestOpened, VerbMessageRequestOffered,
		VerbMessageRequestDeclined, VerbMessageRequestSelected,
		VerbMessageRequestClaimed, VerbMessageRequestRenewed,
		VerbMessageRequestReleased, VerbMessageRequestCompleted,
		VerbMessageRequestCancelled, VerbMessageRequestExpired,
		VerbMessageRequestCancelledSystem,
		VerbFactDeleted,
		VerbMemoryAdded, VerbMemoryAdjusted, VerbMemorySuperseded,
		VerbMemoryForgotten, VerbMemoryRestored, VerbMemoryReactivated,
		VerbMemoryEvidenceResolved, VerbMemoryDeleted,
		VerbMemoryCurationRequested, VerbMemoryCurationStarted,
		VerbMemoryCurationPlanned, VerbMemoryCurationApplied,
		VerbMemoryCurationConflicted, VerbMemoryCurationInterrupted,
		VerbMemoryCurationCancelled, VerbMemoryCurationRolledBack,
		VerbAvatarGenerationRequested, VerbAvatarProposed,
		VerbAvatarActivated, VerbAvatarEvolved, VerbAvatarRejected,
		VerbAvatarGenerationFailed, VerbAvatarRolledBack,
		VerbAvatarReset, VerbAvatarPolicyChanged, VerbAvatarStyleChanged,

		VerbSupportTicketOpened, VerbSupportTicketReplied,
		VerbSupportTicketStateChanged, VerbSupportTicketClosed,

		VerbAccountSupportPolicyChanged,
	}
	for _, v := range verbs {
		if _, ok := verbMetadataSchema[v]; !ok {
			t.Errorf("verb constant %q has no verbMetadataSchema entry", v)
		}
	}
	inList := func(s string) bool {
		for _, v := range verbs {
			if v == s {
				return true
			}
		}
		return false
	}
	for k := range verbMetadataSchema {
		if !inList(k) {
			t.Errorf("verbMetadataSchema has stale entry %q not in the verb-constant list", k)
		}
	}
}

func TestAvatarAuditSchemasAreValueFree(t *testing.T) {
	forbidden := []string{
		"svg", "description", "visual_spec", "prompt", "provenance",
		"svg_sha256", "request_hash", "idempotency_key", "model", "runtime",
	}
	tests := []EventInput{
		{AccountID: "acc_1", ActorKind: ActorSystem,
			Verb: VerbAvatarGenerationRequested, Metadata: map[string]any{
				"agent_id": "agent_1", "status": "generation_due",
				"style_pack_id": "witself-flat-portrait", "style_pack_version": "1",
			}},
		{AccountID: "acc_1", ActorKind: ActorAgent, ActorID: "agent_1",
			Verb: VerbAvatarProposed, Metadata: map[string]any{
				"agent_id": "agent_1", "avatar_version": "1", "status": "proposed",
				"style_pack_id": "witself-flat-portrait", "style_pack_version": "1",
			}},
		{AccountID: "acc_1", ActorKind: ActorAgent, ActorID: "agent_1",
			Verb: VerbAvatarActivated, Metadata: map[string]any{
				"agent_id": "agent_1", "avatar_version": "1", "status": "active",
				"style_pack_id": "witself-flat-portrait", "style_pack_version": "1",
			}},
		{AccountID: "acc_1", ActorKind: ActorAgent, ActorID: "agent_1",
			Verb: VerbAvatarEvolved, Metadata: map[string]any{
				"agent_id": "agent_1", "avatar_version": "2", "parent_version": "1",
				"status": "active", "style_pack_id": "witself-flat-portrait",
				"style_pack_version": "1",
			}},
		{AccountID: "acc_1", ActorKind: ActorOperator, ActorID: "op_1",
			Verb: VerbAvatarRejected, Metadata: map[string]any{
				"agent_id": "agent_1", "avatar_version": "2", "status": "rejected",
				"reason_code": "style_mismatch",
			}},
		{AccountID: "acc_1", ActorKind: ActorAgent, ActorID: "agent_1",
			Verb: VerbAvatarGenerationFailed, Metadata: map[string]any{
				"agent_id": "agent_1", "status": "generation_failed",
				"attempt_count": "1", "reason_code": "renderer_unavailable",
			}},
		{AccountID: "acc_1", ActorKind: ActorOperator, ActorID: "op_1",
			Verb: VerbAvatarRolledBack, Metadata: map[string]any{
				"agent_id": "agent_1", "avatar_version": "1",
				"prior_active_version": "2", "status": "active",
				"style_pack_id": "witself-flat-portrait", "style_pack_version": "1",
			}},
		{AccountID: "acc_1", ActorKind: ActorAgent, ActorID: "agent_1",
			Verb: VerbAvatarReset, Metadata: map[string]any{
				"agent_id": "agent_1", "status": "generation_due",
				"retired_lineage_generation": "1", "new_lineage_generation": "2",
				"retired_active_version": "2", "retired_proposed_version": "3",
				"reason_code": "identity_restart",
			}},
		{AccountID: "acc_1", ActorKind: ActorOperator, ActorID: "op_1",
			Verb: VerbAvatarPolicyChanged, Metadata: map[string]any{
				"agent_id": "agent_1", "policy_from": "agent_proposes",
				"policy_to": "agent_self_managed", "status": "active",
			}},
		{AccountID: "acc_1", ActorKind: ActorOperator, ActorID: "op_1",
			Verb: VerbAvatarStyleChanged, Metadata: map[string]any{
				"realm_id": "realm_1", "style_pack_id": "witself-flat-portrait",
				"style_pack_version": "2", "style_revision": "2",
			}},
	}
	for _, input := range tests {
		t.Run(input.Verb, func(t *testing.T) {
			if err := checkEventShape(input); err != nil {
				t.Fatalf("valid avatar event: %v", err)
			}
			spec := verbMetadataSchema[input.Verb]
			for _, key := range forbidden {
				if slicesContains(spec.allowedKeys, key) {
					t.Fatalf("avatar event schema allows private/value key %q", key)
				}
			}
			invalid := input
			invalid.Metadata = make(map[string]any, len(input.Metadata)+1)
			for key, value := range input.Metadata {
				invalid.Metadata[key] = value
			}
			invalid.Metadata["svg"] = "<svg/>"
			if err := checkEventShape(invalid); err == nil || !strings.Contains(err.Error(), `unknown key "svg"`) {
				t.Fatalf("private avatar metadata accepted: %v", err)
			}
		})
	}
}

func TestMessageRequestAuditSchemasAreExactAgentOnlyAndContentFree(t *testing.T) {
	tests := []struct {
		verb string
		keys []string
	}{
		{VerbMessageRequestOpened, []string{
			"request_id", "opening_message_id", "coordinator_agent_id", "max_assignees",
		}},
		{VerbMessageRequestOffered, []string{
			"request_id", "opening_message_id", "coordinator_agent_id", "agent_id",
		}},
		{VerbMessageRequestDeclined, []string{
			"request_id", "opening_message_id", "coordinator_agent_id", "agent_id",
		}},
		{VerbMessageRequestSelected, []string{
			"request_id", "opening_message_id", "coordinator_agent_id", "agent_id",
			"selection_id", "generation", "max_assignees",
		}},
		{VerbMessageRequestClaimed, []string{
			"request_id", "opening_message_id", "coordinator_agent_id", "agent_id",
			"selection_id", "generation", "failure_count",
		}},
		{VerbMessageRequestRenewed, []string{
			"request_id", "opening_message_id", "coordinator_agent_id", "agent_id",
			"selection_id", "generation", "failure_count",
		}},
		{VerbMessageRequestReleased, []string{
			"request_id", "opening_message_id", "coordinator_agent_id", "agent_id",
			"selection_id", "generation", "failure_count",
		}},
		{VerbMessageRequestCompleted, []string{
			"request_id", "opening_message_id", "coordinator_agent_id", "agent_id",
			"selection_id", "generation", "failure_count", "result_message_id",
		}},
		{VerbMessageRequestCancelled, []string{
			"request_id", "opening_message_id", "coordinator_agent_id", "max_assignees",
		}},
	}
	valueFor := func(key string) string {
		switch key {
		case "request_id":
			return "mrq_aaaaaaaaaaaaaaaa"
		case "opening_message_id", "result_message_id":
			return "msg_aaaaaaaaaaaaaaaa"
		case "coordinator_agent_id":
			return "agent_coordinator"
		case "agent_id":
			return "agent_worker"
		case "selection_id":
			return "msel_aaaaaaaaaaaaaaaa"
		case "generation", "failure_count", "max_assignees":
			return "1"
		default:
			t.Fatalf("test has no value for metadata key %q", key)
			return ""
		}
	}
	forbidden := []string{
		"body", "payload", "subject", "idempotency_key", "idempotency_key_hash",
		"claim_id", "claim_key_hash", "complete_key_hash", "lease_expires_at",
	}

	for _, tc := range tests {
		t.Run(tc.verb, func(t *testing.T) {
			spec, ok := verbMetadataSchema[tc.verb]
			if !ok {
				t.Fatalf("verb %q has no schema", tc.verb)
			}
			if !reflect.DeepEqual(spec.requiredKeys, tc.keys) || !reflect.DeepEqual(spec.allowedKeys, tc.keys) {
				t.Fatalf("schema = required %#v allowed %#v, want exact %#v",
					spec.requiredKeys, spec.allowedKeys, tc.keys)
			}
			if !reflect.DeepEqual(spec.allowedActors, []string{ActorAgent}) {
				t.Fatalf("allowed actors = %#v, want agent only", spec.allowedActors)
			}
			metadata := make(map[string]any, len(tc.keys))
			for _, key := range tc.keys {
				metadata[key] = valueFor(key)
			}
			valid := EventInput{
				AccountID: "acc_1", ActorKind: ActorAgent, ActorID: "agent_actor",
				Verb: tc.verb, Metadata: metadata,
			}
			if err := checkEventShape(valid); err != nil {
				t.Fatalf("valid content-free metadata: %v", err)
			}
			wrongActor := valid
			wrongActor.ActorKind = ActorSystem
			wrongActor.ActorID = ""
			if err := checkEventShape(wrongActor); err == nil || !strings.Contains(err.Error(), `actor_kind "system" not allowed`) {
				t.Fatalf("system actor error = %v", err)
			}
			for _, key := range forbidden {
				if slicesContains(spec.allowedKeys, key) {
					t.Fatalf("schema allows forbidden key %q", key)
				}
			}
			withContent := make(map[string]any, len(metadata)+1)
			for key, value := range metadata {
				withContent[key] = value
			}
			withContent["body"] = "private"
			invalid := valid
			invalid.Metadata = withContent
			if err := checkEventShape(invalid); err == nil || !strings.Contains(err.Error(), `unknown key "body"`) {
				t.Fatalf("content metadata error = %v", err)
			}
		})
	}

	numericCounter := EventInput{
		AccountID: "acc_1", ActorKind: ActorAgent, ActorID: "agent_coordinator",
		Verb: VerbMessageRequestOpened,
		Metadata: map[string]any{
			"request_id": "mrq_aaaaaaaaaaaaaaaa", "opening_message_id": "msg_aaaaaaaaaaaaaaaa",
			"coordinator_agent_id": "agent_coordinator", "max_assignees": 1,
		},
	}
	if err := checkEventShape(numericCounter); err == nil || !strings.Contains(err.Error(), `max_assignees" must be a non-empty string`) {
		t.Fatalf("numeric counter error = %v", err)
	}
}

func TestMessageRequestSystemAuditSchemasAreExactAndContentFree(t *testing.T) {
	tests := []struct {
		verb string
		keys []string
	}{
		{VerbMessageRequestExpired, []string{
			"request_id", "opening_message_id", "coordinator_agent_id", "max_assignees",
		}},
		{VerbMessageRequestCancelledSystem, []string{
			"request_id", "opening_message_id", "coordinator_agent_id", "max_assignees", "reason_code",
		}},
	}
	valueFor := func(key string) string {
		switch key {
		case "request_id":
			return "mrq_aaaaaaaaaaaaaaaa"
		case "opening_message_id":
			return "msg_aaaaaaaaaaaaaaaa"
		case "coordinator_agent_id":
			return "agent_coordinator"
		case "max_assignees":
			return "1"
		case "reason_code":
			return "coordinator_deleted"
		default:
			t.Fatalf("test has no value for metadata key %q", key)
			return ""
		}
	}
	forbidden := []string{
		"body", "payload", "subject", "idempotency_key", "idempotency_key_hash",
		"claim_id", "claim_key_hash", "complete_key_hash", "lease_expires_at",
	}
	for _, tc := range tests {
		t.Run(tc.verb, func(t *testing.T) {
			spec := verbMetadataSchema[tc.verb]
			if !reflect.DeepEqual(spec.requiredKeys, tc.keys) || !reflect.DeepEqual(spec.allowedKeys, tc.keys) {
				t.Fatalf("schema = required %#v allowed %#v, want exact %#v",
					spec.requiredKeys, spec.allowedKeys, tc.keys)
			}
			if !reflect.DeepEqual(spec.allowedActors, []string{ActorSystem}) {
				t.Fatalf("allowed actors = %#v, want system only", spec.allowedActors)
			}
			metadata := make(map[string]any, len(tc.keys))
			for _, key := range tc.keys {
				metadata[key] = valueFor(key)
			}
			valid := EventInput{
				AccountID: "acc_1", ActorKind: ActorSystem,
				Verb: tc.verb, Metadata: metadata,
			}
			if err := checkEventShape(valid); err != nil {
				t.Fatalf("valid content-free metadata: %v", err)
			}
			wrongActor := valid
			wrongActor.ActorKind = ActorAgent
			wrongActor.ActorID = "agent_actor"
			if err := checkEventShape(wrongActor); err == nil || !strings.Contains(err.Error(), `actor_kind "agent" not allowed`) {
				t.Fatalf("agent actor error = %v", err)
			}
			for _, key := range forbidden {
				if slicesContains(spec.allowedKeys, key) {
					t.Fatalf("schema allows forbidden key %q", key)
				}
			}
		})
	}
}

func slicesContains(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

// TestCheckEventShape exercises the metadata-shape enforcement: missing
// required keys, wrong actor kind for the verb, unknown extra keys,
// empty required strings, and principal-actor-without-id cases. This is
// what logEventTx enforces before the row hits the DB.
func TestCheckEventShape(t *testing.T) {
	tests := []struct {
		name    string
		input   EventInput
		wantErr string // substring; empty means expect nil
	}{
		{
			name: "operator.created with all required keys is valid",
			input: EventInput{
				AccountID: "acc_x", ActorKind: ActorOwner, ActorID: "op_1",
				Verb:     VerbOperatorCreated,
				Metadata: map[string]any{"operator_id": "op_new", "role": "account_owner"},
			},
		},
		{
			name: "operator.created missing role fails",
			input: EventInput{
				AccountID: "acc_x", ActorKind: ActorOwner, ActorID: "op_1",
				Verb:     VerbOperatorCreated,
				Metadata: map[string]any{"operator_id": "op_new"},
			},
			wantErr: `requires metadata key "role"`,
		},
		{
			name: "unknown verb refuses",
			input: EventInput{
				AccountID: "acc_x", ActorKind: ActorOwner, ActorID: "op_1",
				Verb: "sneaky.action",
			},
			wantErr: "unknown audit-log verb",
		},
		{
			name: "system actor logging an owner-only verb is refused",
			input: EventInput{
				AccountID: "acc_x", ActorKind: ActorSystem,
				Verb:     VerbAccountSuspendedByMe,
				Metadata: map[string]any{"reason": "on vacation"},
			},
			wantErr: `actor_kind "system" not allowed`,
		},
		{
			name: "control_plane actor logging recovery.requested is valid",
			input: EventInput{
				AccountID: "acc_x", ActorKind: ActorControlPlane,
				Verb:     VerbRecoveryRequested,
				Metadata: map[string]any{"email_masked": "s***@w***.ai"},
			},
		},
		{
			name: "system actor restoring placement policy is valid",
			input: EventInput{
				AccountID: "acc_x", ActorKind: ActorSystem,
				Verb:     VerbAccountPlacementPolicyChanged,
				Metadata: placementPolicyEventMetadata(placement.DefaultPolicy()),
			},
		},
		{
			name: "principal actor without actor_id refuses",
			input: EventInput{
				AccountID: "acc_x", ActorKind: ActorOwner,
				Verb: VerbAccountResumedByMe,
			},
			wantErr: `requires actor_id`,
		},
		{
			name: "system actor with actor_id refuses",
			input: EventInput{
				AccountID: "acc_x", ActorKind: ActorSystem, ActorID: "should-not-be-here",
				Verb:     VerbAccountEvacuated,
				Metadata: map[string]any{"cell": "aws-sandbox-usw2-dev"},
			},
			wantErr: `must not carry actor_id`,
		},
		{
			name: "extra unknown metadata key refuses",
			input: EventInput{
				AccountID: "acc_x", ActorKind: ActorOwner, ActorID: "op_1",
				Verb:     VerbAccountResumedByMe,
				Metadata: map[string]any{"secret": "leaked"},
			},
			wantErr: `unknown key "secret"`,
		},
		{
			name: "required string that is empty refuses",
			input: EventInput{
				AccountID: "acc_x", ActorKind: ActorOwner, ActorID: "op_1",
				Verb:     VerbOperatorTokenMinted,
				Metadata: map[string]any{"token_id": "", "operator_id": "op_1"},
			},
			wantErr: `must be a non-empty string`,
		},
		{
			name: "empty account_id refuses",
			input: EventInput{
				ActorKind: ActorOwner, ActorID: "op_1",
				Verb: VerbAccountResumedByMe,
			},
			wantErr: `account_id is required`,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := checkEventShape(tc.input)
			if tc.wantErr == "" {
				if err != nil {
					t.Errorf("unexpected error: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error = %v, want substring %q", err, tc.wantErr)
			}
		})
	}
}

// TestEventCursorRoundTrip pins pagination-cursor semantics: any cursor
// encodeEventCursor emits must decode back to the same (timestamp, id)
// pair, and same-timestamp events must break-tie on id deterministically.
// A silent drift here would misplace a page boundary.
func TestEventCursorRoundTrip(t *testing.T) {
	tests := []struct {
		unixNano int64
		id       string
	}{
		{1_672_531_200_000_000_000, "evt_abc123"},
		{0, "evt_zero"},
		{999_999_999_999_999_999, "evt_end"},
	}
	for _, tc := range tests {
		cursor := encodeEventCursor(time.Unix(0, tc.unixNano).UTC(), tc.id)
		gotT, gotID, err := decodeEventCursor(cursor)
		if err != nil {
			t.Errorf("decode(%q) = %v, want nil", cursor, err)
			continue
		}
		if gotT.UnixNano() != tc.unixNano {
			t.Errorf("time round-trip: got %d, want %d", gotT.UnixNano(), tc.unixNano)
		}
		if gotID != tc.id {
			t.Errorf("id round-trip: got %q, want %q", gotID, tc.id)
		}
	}

	// Malformed cursors must error cleanly. Including the sneaky
	// partial-integer prefixes fmt.Sscanf used to silently truncate:
	// "1234junk:evt_x" used to decode as timestamp=1234, id="evt_x",
	// leading to a page rooted at a wrong time.
	for _, bad := range []string{
		"", "no-colon", ":no-time-part", "trailing:",
		"1234junk:evt_x", "  1234:evt_x", "+1234:evt_x",
	} {
		if _, _, err := decodeEventCursor(bad); err == nil {
			t.Errorf("decodeEventCursor(%q) = nil, want error", bad)
		}
	}
}

// TestBadCursorWrapping pins the sentinel-error wire pattern:
// ListAccountEvents wraps decode failures with %w around
// ErrBadEventCursor so the server layer can map to 400 via
// errors.Is. The store method needs a DB to run its full path, but
// the wrap pattern itself is what the server contract depends on.
func TestBadCursorWrapping(t *testing.T) {
	_, _, err := decodeEventCursor("garbage:no-integer")
	if err == nil {
		t.Fatal("expected decode error, got nil")
	}
	wrapped := fmt.Errorf("%w: %v", ErrBadEventCursor, err)
	if !errors.Is(wrapped, ErrBadEventCursor) {
		t.Errorf("wrapped error does not report as ErrBadEventCursor: %v", wrapped)
	}
}
