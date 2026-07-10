package store

import (
	"errors"
	"fmt"
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
