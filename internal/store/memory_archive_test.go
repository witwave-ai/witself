package store

import (
	"encoding/base64"
	"errors"
	"strconv"
	"strings"
	"testing"
)

const (
	memoryArchiveOneID            = "mem_aaaaaaaaaaaaaaaa"
	memoryArchiveLiveID           = "mem_bbbbbbbbbbbbbbbb"
	memoryArchiveDeletedID        = "mem_cccccccccccccccc"
	memoryArchiveTargetID         = "mem_dddddddddddddddd"
	memoryArchiveReplacementAID   = "mem_eeeeeeeeeeeeeeee"
	memoryArchiveReplacementBID   = "mem_ffffffffffffffff"
	memoryArchiveReplacementCID   = "mem_gggggggggggggggg"
	memoryArchiveReplacementDID   = "mem_hhhhhhhhhhhhhhhh"
	memoryArchiveMissingReceiptID = "mem_iiiiiiiiiiiiiiii"
	memoryArchiveBadID            = "mem_jjjjjjjjjjjjjjjj"
	memoryArchiveInitialID        = "mem_kkkkkkkkkkkkkkkk"
	memoryArchiveFromID           = "mem_llllllllllllllll"
	memoryArchiveToID             = "mem_mmmmmmmmmmmmmmmm"
	memoryArchiveReferenceID      = "mdr_aaaaaaaaaaaaaaaa"
	memoryArchiveReasonRefID      = "mdr_bbbbbbbbbbbbbbbb"
	memoryArchiveCurationRefID    = "mdr_cccccccccccccccc"
	memoryArchiveRelationOneID    = "mrel_aaaaaaaaaaaaaaaa"
	memoryArchiveRelationAID      = "mrel_bbbbbbbbbbbbbbbb"
	memoryArchiveRelationBID      = "mrel_cccccccccccccccc"
	memoryArchiveRelationCID      = "mrel_dddddddddddddddd"
	memoryArchiveRelationDID      = "mrel_eeeeeeeeeeeeeeee"
	memoryArchiveRelationEID      = "mrel_ffffffffffffffff"
	memoryArchiveRelationFID      = "mrel_gggggggggggggggg"
	memoryArchiveSetOneID         = "mset_aaaaaaaaaaaaaaaa"
	memoryArchivePrimarySetID     = "mset_bbbbbbbbbbbbbbbb"
	memoryArchiveRevertedSetID    = "mset_cccccccccccccccc"
	memoryArchiveConflictSetID    = "mset_dddddddddddddddd"
)

func TestMemoryArchiveValidationPinsOwnerVersionsAndEvidence(t *testing.T) {
	ic := memoryArchiveImportContext()
	if err := ic.validateAndRecord("memory_change_clocks", map[string]any{
		"account_id": "acc_1", "realm_id": "realm_1", "owner_kind": "agent",
		"owner_id": "agent_1", "last_change_seq": float64(4),
	}); err != nil {
		t.Fatal(err)
	}
	if err := ic.validateAndRecord("memories", memoryArchiveHead(memoryArchiveOneID, float64(1))); err != nil {
		t.Fatal(err)
	}
	if err := ic.validateAndRecord("memory_versions", memoryArchiveVersion(memoryArchiveOneID, 1, nil, 1)); err != nil {
		t.Fatal(err)
	}

	pending := memoryArchiveEvidence("mev_pending", "pending", 2)
	pending["external_locator"] = "codex/session/turn-4"
	if err := ic.validateAndRecord("memory_evidence", pending); err != nil {
		t.Fatal(err)
	}
	ic.transcripts["tr_1"] = transcriptImportScope{
		realmID: "realm_1", ownerAgentID: "agent_1", nextSequence: 12,
	}
	resolved := memoryArchiveEvidence("mev_resolved", "resolved", 3)
	resolved["pending_evidence_id"] = "mev_pending"
	resolved["resolved_kind"] = "transcript"
	resolved["source_transcript_id"] = "tr_1"
	resolved["source_sequence_from"] = float64(4)
	resolved["source_sequence_until"] = float64(8)
	resolved["idempotency_key"] = "resolve-1"
	resolved["request_hash"] = strings.Repeat("b", 64)
	if err := ic.validateAndRecord("memory_evidence", resolved); err != nil {
		t.Fatal(err)
	}

	duplicate := memoryArchiveEvidence("mev_duplicate", "unresolvable", 4)
	duplicate["pending_evidence_id"] = "mev_pending"
	duplicate["terminal_reason_code"] = "not_found"
	duplicate["idempotency_key"] = "resolve-2"
	duplicate["request_hash"] = strings.Repeat("c", 64)
	if err := ic.validateAndRecord("memory_evidence", duplicate); err == nil ||
		!strings.Contains(err.Error(), "more than one terminal resolution") {
		t.Fatalf("duplicate terminal resolution error = %v", err)
	}
}

func TestMemoryArchiveValidationPinsEvidenceRetryProvenanceAndOrder(t *testing.T) {
	newContext := func(t *testing.T) *importCtx {
		t.Helper()
		ic := memoryArchiveImportContext()
		if err := ic.validateAndRecord("memories",
			memoryArchiveHead(memoryArchiveOneID, float64(1))); err != nil {
			t.Fatal(err)
		}
		if err := ic.validateAndRecord("memory_versions",
			memoryArchiveVersion(memoryArchiveOneID, 1, nil, 1)); err != nil {
			t.Fatal(err)
		}
		return ic
	}

	t.Run("initial row rejects poisoned retry key", func(t *testing.T) {
		ic := newContext(t)
		pending := memoryArchiveEvidence("mev_pending", "pending", 2)
		pending["external_locator"] = "codex/session/turn-4"
		pending["idempotency_key"] = "payload-smuggled-as-retry-key"
		pending["request_hash"] = strings.Repeat("a", 64)
		if err := ic.validateAndRecord("memory_evidence", pending); err == nil ||
			!strings.Contains(err.Error(), "idempotency_key must be null") {
			t.Fatalf("initial evidence retry-key error = %v", err)
		}
	})

	t.Run("terminal row requires retry pair", func(t *testing.T) {
		ic := newContext(t)
		pending := memoryArchiveEvidence("mev_pending", "pending", 2)
		pending["external_locator"] = "codex/session/turn-4"
		if err := ic.validateAndRecord("memory_evidence", pending); err != nil {
			t.Fatal(err)
		}
		terminal := memoryArchiveEvidence("mev_terminal", "unresolvable", 3)
		terminal["pending_evidence_id"] = "mev_pending"
		terminal["terminal_reason_code"] = "not_found"
		if err := ic.validateAndRecord("memory_evidence", terminal); err == nil ||
			!strings.Contains(err.Error(), "requires an idempotency_key") {
			t.Fatalf("terminal evidence retry-pair error = %v", err)
		}
	})

	t.Run("initial row follows target version", func(t *testing.T) {
		ic := newContext(t)
		pending := memoryArchiveEvidence("mev_pending", "pending", 1)
		pending["external_locator"] = "codex/session/turn-4"
		if err := ic.validateAndRecord("memory_evidence", pending); err == nil ||
			!strings.Contains(err.Error(), "follow its target version") {
			t.Fatalf("backwards initial evidence error = %v", err)
		}
	})

	t.Run("terminal row follows pending row", func(t *testing.T) {
		ic := newContext(t)
		pending := memoryArchiveEvidence("mev_pending", "pending", 2)
		pending["external_locator"] = "codex/session/turn-4"
		if err := ic.validateAndRecord("memory_evidence", pending); err != nil {
			t.Fatal(err)
		}
		terminal := memoryArchiveEvidence("mev_terminal", "unresolvable", 2)
		terminal["pending_evidence_id"] = "mev_pending"
		terminal["terminal_reason_code"] = "not_found"
		terminal["idempotency_key"] = "resolve-1"
		terminal["request_hash"] = strings.Repeat("a", 64)
		if err := ic.validateAndRecord("memory_evidence", terminal); err == nil ||
			!strings.Contains(err.Error(), "follow its pending evidence") {
			t.Fatalf("backwards terminal evidence error = %v", err)
		}
	})
}

func TestMemoryArchiveValidationReservesChangeSequenceHeadroom(t *testing.T) {
	valid := memoryArchiveImportContext()
	if err := valid.validateAndRecord("memory_change_clocks", map[string]any{
		"account_id": "acc_1", "realm_id": "realm_1", "owner_kind": "agent",
		"owner_id": "agent_1", "last_change_seq": maxImportedMemoryChangeSeq,
	}); err != nil {
		t.Fatalf("maximum importable clock: %v", err)
	}

	for _, value := range []int64{
		maxImportedMemoryChangeSeq + 1,
		maxMemoryChangeSeq - 1,
		maxMemoryChangeSeq,
	} {
		t.Run(strconv.FormatInt(value, 10), func(t *testing.T) {
			ic := memoryArchiveImportContext()
			err := ic.validateAndRecord("memory_change_clocks", map[string]any{
				"account_id": "acc_1", "realm_id": "realm_1", "owner_kind": "agent",
				"owner_id": "agent_1", "last_change_seq": value,
			})
			if err == nil || !errors.Is(err, ErrArchiveContent) ||
				!strings.Contains(err.Error(), "last_change_seq") {
				t.Fatalf("unsafe clock %d error = %v", value, err)
			}
		})
	}
}

func TestMemoryArchiveValidationRejectsCrossOwnerAndDeletedPayloads(t *testing.T) {
	ic := memoryArchiveImportContext()
	if err := ic.validateAndRecord("memories", memoryArchiveHead(memoryArchiveLiveID, float64(1))); err != nil {
		t.Fatal(err)
	}
	crossOwner := memoryArchiveVersion(memoryArchiveLiveID, 1, nil, 1)
	crossOwner["owner_id"] = "agent_2"
	if err := ic.validateAndRecord("memory_versions", crossOwner); err == nil ||
		!strings.Contains(err.Error(), "scope does not match") {
		t.Fatalf("cross-owner version error = %v", err)
	}

	shield := memoryDeleteRetryShield{Kind: "idempotency.added", Hash: strings.Repeat("c", 64)}
	deleted := memoryArchiveDeletedHead(memoryArchiveDeletedID, shield)
	if err := ic.validateAndRecord("memories", deleted); err != nil {
		t.Fatal(err)
	}
	if err := ic.validateAndRecord("memory_versions", memoryArchiveVersion(memoryArchiveDeletedID, 1, nil, 2)); err == nil ||
		!strings.Contains(err.Error(), "live memory") {
		t.Fatalf("deleted-memory version error = %v", err)
	}
	if err := ic.validateAndRecord("memory_deleted_references", map[string]any{
		"id":         memoryArchiveReferenceID,
		"account_id": "acc_1", "realm_id": "realm_1", "owner_kind": "agent",
		"owner_id": "agent_1", "deleted_memory_id": memoryArchiveDeletedID,
		"former_reference_kind": shield.Kind, "related_resource_id": shield.Hash,
		"reason_code": "permanent_delete",
	}); err != nil {
		t.Fatal(err)
	}
	for _, invalid := range []map[string]any{
		{
			"id": memoryArchiveReasonRefID, "account_id": "acc_1", "realm_id": "realm_1",
			"owner_kind": "agent", "owner_id": "agent_1", "deleted_memory_id": memoryArchiveDeletedID,
			"former_reference_kind": "idempotency.added", "related_resource_id": strings.Repeat("d", 64),
			"reason_code": "private_123456789",
		},
		{
			"id": memoryArchiveCurationRefID, "account_id": "acc_1", "realm_id": "realm_1",
			"owner_kind": "agent", "owner_id": "agent_1", "deleted_memory_id": memoryArchiveDeletedID,
			"former_reference_kind": "idempotency.adjusted", "related_resource_id": strings.Repeat("e", 64),
			"reason_code": "permanent_delete", "curation_run_id": "secret_encoded_as_id",
		},
	} {
		if err := ic.validateAndRecord("memory_deleted_references", invalid); err == nil {
			t.Fatalf("value-bearing deleted reference was accepted: %#v", invalid)
		}
	}
	if err := ic.validateAndRecord("memory_deleted_references", map[string]any{
		"id":         memoryArchiveReferenceID,
		"account_id": "acc_1", "realm_id": "realm_1", "owner_kind": "agent",
		"owner_id": "agent_1", "deleted_memory_id": memoryArchiveLiveID,
		"former_reference_kind": shield.Kind, "related_resource_id": shield.Hash,
	}); err == nil || !strings.Contains(err.Error(), "non-deleted memory") {
		t.Fatalf("live-memory deleted reference error = %v", err)
	}
	if err := validateImportedMemoryRetryShields(
		memoryArchiveDeletedID, ic.memories[memoryArchiveDeletedID], ic.memoryDeletedRetryShields[memoryArchiveDeletedID],
	); err != nil {
		t.Fatalf("complete retry shields: %v", err)
	}
	if err := validateImportedMemoryRetryShields(
		memoryArchiveDeletedID, ic.memories[memoryArchiveDeletedID], nil,
	); err == nil || !strings.Contains(err.Error(), "do not match") {
		t.Fatalf("missing retry shields error = %v", err)
	}
}

func TestMemoryArchiveValidationRequiresCanonicalDeletedTombstone(t *testing.T) {
	shield := memoryDeleteRetryShield{Kind: "idempotency.added", Hash: strings.Repeat("c", 64)}
	tests := []struct {
		name   string
		mutate func(map[string]any)
	}{
		{
			name: "payload-like origin",
			mutate: func(row map[string]any) {
				row["origin"] = "private_conversation_summary"
			},
		},
		{
			name: "payload-like capture reason",
			mutate: func(row map[string]any) {
				row["capture_reason"] = "remember_scotts_secret"
			},
		},
		{
			name: "non-authoritative delete reason",
			mutate: func(row map[string]any) {
				row["permanent_delete_reason"] = "background_curator"
			},
		},
		{
			name: "updated timestamp not deletion timestamp",
			mutate: func(row map[string]any) {
				row["updated_at"] = "2026-07-14T08:00:01Z"
			},
		},
		{
			name: "creation after deletion",
			mutate: func(row map[string]any) {
				row["created_at"] = "2026-07-14T08:00:01Z"
			},
		},
		{
			name: "payload suffix in receipt id",
			mutate: func(row map[string]any) {
				row["delete_receipt_id"] = "mdel_aaaaaaaaaaaaaaaapayload"
			},
		},
		{
			name: "short receipt id",
			mutate: func(row map[string]any) {
				row["delete_receipt_id"] = "mdel_a"
			},
		},
		{
			name: "uppercase receipt alphabet",
			mutate: func(row map[string]any) {
				row["delete_receipt_id"] = "mdel_AAAAAAAAAAAAAAAA"
			},
		},
		{
			name: "invalid receipt alphabet",
			mutate: func(row map[string]any) {
				row["delete_receipt_id"] = "mdel_1111111111111111"
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ic := memoryArchiveImportContext()
			row := memoryArchiveDeletedHead(memoryArchiveDeletedID, shield)
			test.mutate(row)
			if err := ic.validateAndRecord("memories", row); err == nil ||
				!strings.Contains(err.Error(), "deleted tombstone shape") {
				t.Fatalf("noncanonical tombstone error = %v", err)
			}
		})
	}
}

func TestMemoryArchiveValidationRequiresCanonicalRetainedMemoryIdentifiers(t *testing.T) {
	for _, memoryID := range []string{
		"mem_aaaaaaaaaaaaaaaapayload",
		"mem_a",
		"mem_AAAAAAAAAAAAAAAA",
		"mem_1111111111111111",
	} {
		t.Run("memory "+memoryID, func(t *testing.T) {
			ic := memoryArchiveImportContext()
			if err := ic.validateAndRecord("memories", memoryArchiveHead(memoryID, float64(1))); err == nil ||
				!strings.Contains(err.Error(), "id is invalid") {
				t.Fatalf("noncanonical memory id error = %v", err)
			}
		})
	}

	shield := memoryDeleteRetryShield{Kind: "idempotency.added", Hash: strings.Repeat("c", 64)}
	for _, referenceID := range []string{
		"mdr_aaaaaaaaaaaaaaaapayload",
		"mdr_a",
		"mdr_AAAAAAAAAAAAAAAA",
		"mdr_1111111111111111",
	} {
		t.Run("reference "+referenceID, func(t *testing.T) {
			ic := memoryArchiveImportContext()
			if err := ic.validateAndRecord("memories",
				memoryArchiveDeletedHead(memoryArchiveDeletedID, shield)); err != nil {
				t.Fatal(err)
			}
			row := memoryArchiveDeletedReference(referenceID, memoryArchiveDeletedID, shield)
			if err := ic.validateAndRecord("memory_deleted_references", row); err == nil ||
				!strings.Contains(err.Error(), "id is invalid") {
				t.Fatalf("noncanonical deleted-reference id error = %v", err)
			}
		})
	}
}

func TestMemoryArchiveValidationRestrictsDeletedRetryShieldKinds(t *testing.T) {
	validKinds := []string{
		"idempotency.added",
		"idempotency.adjusted",
		"idempotency.superseded",
		"idempotency.forgotten",
		"idempotency.restored",
		"idempotency.reactivated",
		"idempotency.evidence_resolution",
	}
	for _, kind := range validKinds {
		t.Run("valid "+kind, func(t *testing.T) {
			ic := memoryArchiveImportContext()
			shield := memoryDeleteRetryShield{Kind: kind, Hash: strings.Repeat("c", 64)}
			if err := ic.validateAndRecord("memories",
				memoryArchiveDeletedHead(memoryArchiveDeletedID, shield)); err != nil {
				t.Fatal(err)
			}
			if err := ic.validateAndRecord("memory_deleted_references",
				memoryArchiveDeletedReference(memoryArchiveReferenceID, memoryArchiveDeletedID, shield)); err != nil {
				t.Fatalf("writer-produced retry shield kind: %v", err)
			}
		})
	}
	for _, kind := range []string{
		"idempotency.reverted",
		"idempotency.added.payload",
		"idempotency.unknown",
		"idempotency.",
	} {
		t.Run("invalid "+kind, func(t *testing.T) {
			ic := memoryArchiveImportContext()
			shield := memoryDeleteRetryShield{Kind: "idempotency.added", Hash: strings.Repeat("c", 64)}
			if err := ic.validateAndRecord("memories",
				memoryArchiveDeletedHead(memoryArchiveDeletedID, shield)); err != nil {
				t.Fatal(err)
			}
			row := memoryArchiveDeletedReference(memoryArchiveReferenceID, memoryArchiveDeletedID, shield)
			row["former_reference_kind"] = kind
			if err := ic.validateAndRecord("memory_deleted_references", row); err == nil ||
				!strings.Contains(err.Error(), "retry shield kind is invalid") {
				t.Fatalf("non-writer retry shield kind error = %v", err)
			}
		})
	}
}

func TestMemoryArchiveValidationVerifiesContentRepresentationAndHash(t *testing.T) {
	validBase64 := base64.StdEncoding.EncodeToString([]byte("durable narrative"))
	tests := []struct {
		name      string
		content   string
		encoding  string
		hash      string
		wantError string
	}{
		{
			name: "canonical base64", content: validBase64, encoding: "base64",
			hash: memoryContentHash(validBase64),
		},
		{
			name: "hash mismatch", content: "A durable decision", encoding: "plain",
			hash: strings.Repeat("f", 64), wantError: "content_hash does not match content",
		},
		{
			name: "newline in base64", content: validBase64 + "\n", encoding: "base64",
			hash: memoryContentHash(validBase64 + "\n"), wantError: "not canonical base64",
		},
		{
			name: "non-zero base64 padding bits", content: "Zh==", encoding: "base64",
			hash: memoryContentHash("Zh=="), wantError: "not canonical base64",
		},
		{
			name: "unsupported encoding", content: "A durable decision", encoding: "rot13",
			hash: memoryContentHash("A durable decision"), wantError: "must be plain or base64",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ic := memoryArchiveImportContext()
			if err := ic.validateAndRecord("memories", memoryArchiveHead(memoryArchiveOneID, float64(1))); err != nil {
				t.Fatal(err)
			}
			version := memoryArchiveVersion(memoryArchiveOneID, 1, nil, 1)
			version["content"] = test.content
			version["content_encoding"] = test.encoding
			version["content_hash"] = test.hash
			err := ic.validateAndRecord("memory_versions", version)
			if test.wantError == "" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), test.wantError) {
				t.Fatalf("content validation error = %v, want %q", err, test.wantError)
			}
		})
	}
}

func TestMemoryArchiveValidationEnforcesVersionTransitionSemantics(t *testing.T) {
	ic := memoryArchiveImportContext()
	if err := ic.validateAndRecord("memories", memoryArchiveHead(memoryArchiveOneID, float64(6))); err != nil {
		t.Fatal(err)
	}
	versions := []map[string]any{
		memoryArchiveVersion(memoryArchiveOneID, 1, nil, 1),
		memoryArchiveLifecycleVersion(memoryArchiveOneID, 2, 2, "adjusted", MemoryStateActive, ""),
		memoryArchiveLifecycleVersion(memoryArchiveOneID, 3, 3, "superseded", MemoryStateSuperseded, ""),
		memoryArchiveLifecycleVersion(memoryArchiveOneID, 4, 4, "forgotten", MemoryStateForgotten, MemoryStateSuperseded),
		memoryArchiveLifecycleVersion(memoryArchiveOneID, 5, 5, "restored", MemoryStateSuperseded, ""),
		memoryArchiveLifecycleVersion(memoryArchiveOneID, 6, 6, "reactivated", MemoryStateActive, ""),
	}
	for index := 1; index < len(versions); index++ {
		versions[index]["content"] = "A revised durable decision"
		versions[index]["content_hash"] = memoryContentHash("A revised durable decision")
	}
	versions[2]["supersession_set_id"] = memoryArchiveSetOneID
	versions[2]["supersession_set_revision"] = float64(1)
	versions[2]["supersession_replacement_count"] = float64(1)
	versions[2]["supersession_replacement_digest"] = strings.Repeat("d", 64)
	for _, version := range versions {
		if err := ic.validateAndRecord("memory_versions", version); err != nil {
			t.Fatalf("legal version %v: %v", version["version"], err)
		}
	}

	invalid := []struct {
		name      string
		operation string
		state     string
		prior     any
		changeSeq int64
		want      string
	}{
		{name: "added after initial", operation: "added", state: MemoryStateActive, changeSeq: 2, want: "illegal"},
		{name: "adjusted changes state", operation: "adjusted", state: MemoryStateForgotten, prior: MemoryStateActive, changeSeq: 2, want: "illegal"},
		{name: "forgotten lies about prior state", operation: "forgotten", state: MemoryStateForgotten, prior: MemoryStateSuperseded, changeSeq: 2, want: "illegal"},
		{name: "restored without forget", operation: "restored", state: MemoryStateActive, changeSeq: 2, want: "illegal"},
		{name: "reactivated active head", operation: "reactivated", state: MemoryStateActive, changeSeq: 2, want: "illegal"},
		{name: "reverted without curation attribution", operation: "reverted", state: MemoryStateReverted, changeSeq: 2, want: "curation attribution"},
		{name: "nonmonotonic change sequence", operation: "adjusted", state: MemoryStateActive, changeSeq: 1, want: "change_seq"},
		{name: "adjusted semantic no-op", operation: "adjusted", state: MemoryStateActive, changeSeq: 2, want: "does not change"},
	}
	for _, test := range invalid {
		t.Run(test.name, func(t *testing.T) {
			ctx := memoryArchiveImportContext()
			if err := ctx.validateAndRecord("memories", memoryArchiveHead(memoryArchiveBadID, float64(2))); err != nil {
				t.Fatal(err)
			}
			if err := ctx.validateAndRecord("memory_versions", memoryArchiveVersion(memoryArchiveBadID, 1, nil, 1)); err != nil {
				t.Fatal(err)
			}
			version := memoryArchiveLifecycleVersion(
				memoryArchiveBadID, 2, test.changeSeq, test.operation, test.state, "",
			)
			version["prior_state"] = test.prior
			err := ctx.validateAndRecord("memory_versions", version)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("transition validation error = %v, want %q", err, test.want)
			}
		})
	}

	initial := memoryArchiveImportContext()
	if err := initial.validateAndRecord("memories", memoryArchiveHead(memoryArchiveInitialID, float64(1))); err != nil {
		t.Fatal(err)
	}
	badInitial := memoryArchiveVersion(memoryArchiveInitialID, 1, nil, 1)
	badInitial["operation"] = "adjusted"
	if err := initial.validateAndRecord("memory_versions", badInitial); err == nil ||
		!strings.Contains(err.Error(), "initial version") {
		t.Fatalf("initial transition validation error = %v", err)
	}
}

func TestMemoryArchiveValidationRejectsLifecyclePayloadMutation(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(first, lifecycle map[string]any)
	}{
		{
			name: "content",
			mutate: func(_, lifecycle map[string]any) {
				lifecycle["content"] = "Changed during forget"
				lifecycle["content_hash"] = memoryContentHash("Changed during forget")
			},
		},
		{
			name: "content encoding",
			mutate: func(first, lifecycle map[string]any) {
				first["content"] = "Zg=="
				first["content_hash"] = memoryContentHash("Zg==")
				lifecycle["content"] = "Zg=="
				lifecycle["content_hash"] = memoryContentHash("Zg==")
				lifecycle["content_encoding"] = "base64"
			},
		},
		{
			name: "kind",
			mutate: func(_, lifecycle map[string]any) {
				lifecycle["kind"] = "decision"
			},
		},
		{
			name: "tags",
			mutate: func(_, lifecycle map[string]any) {
				lifecycle["tags"] = []any{"architecture", "mutated"}
			},
		},
		{
			name: "links",
			mutate: func(_, lifecycle map[string]any) {
				lifecycle["links"] = []any{"witself://memory/other"}
			},
		},
		{
			name: "salience",
			mutate: func(_, lifecycle map[string]any) {
				lifecycle["salience"] = float64(0.9)
			},
		},
		{
			name: "sensitive",
			mutate: func(_, lifecycle map[string]any) {
				lifecycle["sensitive"] = true
			},
		},
		{
			name: "occurrence",
			mutate: func(_, lifecycle map[string]any) {
				lifecycle["occurred_from"] = "2026-07-14T08:00:00Z"
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ic := memoryArchiveImportContext()
			if err := ic.validateAndRecord("memories",
				memoryArchiveHead(memoryArchiveBadID, float64(2))); err != nil {
				t.Fatal(err)
			}
			first := memoryArchiveVersion(memoryArchiveBadID, 1, nil, 1)
			lifecycle := memoryArchiveLifecycleVersion(
				memoryArchiveBadID, 2, 2, "forgotten", MemoryStateForgotten, MemoryStateActive,
			)
			test.mutate(first, lifecycle)
			if err := ic.validateAndRecord("memory_versions", first); err != nil {
				t.Fatal(err)
			}
			if err := ic.validateAndRecord("memory_versions", lifecycle); err == nil ||
				!strings.Contains(err.Error(), "changes the semantic payload") {
				t.Fatalf("lifecycle payload mutation error = %v", err)
			}
		})
	}
}

func TestMemoryArchiveValidationRejectsPayloadMutationForEveryLifecycleOperation(t *testing.T) {
	t.Run("superseded", func(t *testing.T) {
		ic := memoryArchiveImportContext()
		if err := ic.validateAndRecord("memories",
			memoryArchiveHead(memoryArchiveBadID, float64(2))); err != nil {
			t.Fatal(err)
		}
		if err := ic.validateAndRecord("memory_versions",
			memoryArchiveVersion(memoryArchiveBadID, 1, nil, 1)); err != nil {
			t.Fatal(err)
		}
		superseded := memoryArchiveLifecycleVersion(
			memoryArchiveBadID, 2, 2, "superseded", MemoryStateSuperseded, "",
		)
		superseded["kind"] = "decision"
		if err := ic.validateAndRecord("memory_versions", superseded); err == nil ||
			!strings.Contains(err.Error(), "superseded version changes the semantic payload") {
			t.Fatalf("superseded payload mutation error = %v", err)
		}
	})

	t.Run("restored", func(t *testing.T) {
		ic := memoryArchiveImportContext()
		if err := ic.validateAndRecord("memories",
			memoryArchiveHead(memoryArchiveBadID, float64(3))); err != nil {
			t.Fatal(err)
		}
		if err := ic.validateAndRecord("memory_versions",
			memoryArchiveVersion(memoryArchiveBadID, 1, nil, 1)); err != nil {
			t.Fatal(err)
		}
		forgotten := memoryArchiveLifecycleVersion(
			memoryArchiveBadID, 2, 2, "forgotten", MemoryStateForgotten, MemoryStateActive,
		)
		if err := ic.validateAndRecord("memory_versions", forgotten); err != nil {
			t.Fatal(err)
		}
		restored := memoryArchiveLifecycleVersion(
			memoryArchiveBadID, 3, 3, "restored", MemoryStateActive, "",
		)
		restored["salience"] = float64(0.75)
		if err := ic.validateAndRecord("memory_versions", restored); err == nil ||
			!strings.Contains(err.Error(), "restored version changes the semantic payload") {
			t.Fatalf("restored payload mutation error = %v", err)
		}
	})

	t.Run("reactivated", func(t *testing.T) {
		ic := memoryArchiveImportContext()
		if err := ic.validateAndRecord("memories",
			memoryArchiveHead(memoryArchiveBadID, float64(3))); err != nil {
			t.Fatal(err)
		}
		if err := ic.validateAndRecord("memory_versions",
			memoryArchiveVersion(memoryArchiveBadID, 1, nil, 1)); err != nil {
			t.Fatal(err)
		}
		superseded := memoryArchiveLifecycleVersion(
			memoryArchiveBadID, 2, 2, "superseded", MemoryStateSuperseded, "",
		)
		superseded["supersession_set_id"] = memoryArchivePrimarySetID
		superseded["supersession_set_revision"] = float64(1)
		superseded["supersession_replacement_count"] = float64(1)
		superseded["supersession_replacement_digest"] = strings.Repeat("d", 64)
		if err := ic.validateAndRecord("memory_versions", superseded); err != nil {
			t.Fatal(err)
		}
		reactivated := memoryArchiveLifecycleVersion(
			memoryArchiveBadID, 3, 3, "reactivated", MemoryStateActive, "",
		)
		reactivated["sensitive"] = true
		if err := ic.validateAndRecord("memory_versions", reactivated); err == nil ||
			!strings.Contains(err.Error(), "reactivated version changes the semantic payload") {
			t.Fatalf("reactivated payload mutation error = %v", err)
		}
	})
}

func TestMemoryArchiveValidationRequiresNormalizedPayloadCollections(t *testing.T) {
	for _, tags := range [][]any{
		{"zeta", "alpha"},
		{"architecture", "architecture"},
		{" architecture"},
	} {
		ic := memoryArchiveImportContext()
		if err := ic.validateAndRecord("memories",
			memoryArchiveHead(memoryArchiveBadID, float64(1))); err != nil {
			t.Fatal(err)
		}
		version := memoryArchiveVersion(memoryArchiveBadID, 1, nil, 1)
		version["tags"] = tags
		if err := ic.validateAndRecord("memory_versions", version); err == nil ||
			!strings.Contains(err.Error(), "normalized stored order") {
			t.Fatalf("non-normalized tags %#v error = %v", tags, err)
		}
	}
}

func TestMemoryArchiveValidationRejectsMalformedCurationMetadata(t *testing.T) {
	for _, field := range []string{"curation_run_id", "curation_action_id"} {
		t.Run("version "+field, func(t *testing.T) {
			ic := memoryArchiveImportContext()
			if err := ic.validateAndRecord("memories", memoryArchiveHead(memoryArchiveOneID, float64(1))); err != nil {
				t.Fatal(err)
			}
			version := memoryArchiveVersion(memoryArchiveOneID, 1, nil, 1)
			version[field] = "payload_smuggled_as_identifier"
			if err := ic.validateAndRecord("memory_versions", version); err == nil ||
				!strings.Contains(err.Error(), "curation attribution is invalid") {
				t.Fatalf("malformed version metadata error = %v", err)
			}
		})
	}

	for _, field := range []string{"curation_run_id", "curation_action_id", "reverted_by_run_id", "reverted_by_action_id"} {
		t.Run("relation "+field, func(t *testing.T) {
			ic := memoryArchiveImportContext()
			for index, memoryID := range []string{memoryArchiveFromID, memoryArchiveToID} {
				if err := ic.validateAndRecord("memories", memoryArchiveHead(memoryID, float64(1))); err != nil {
					t.Fatal(err)
				}
				if err := ic.validateAndRecord("memory_versions",
					memoryArchiveVersion(memoryID, 1, nil, int64(index+1))); err != nil {
					t.Fatal(err)
				}
			}
			relation := memoryArchiveRelation(memoryArchiveRelationOneID, memoryArchiveFromID, memoryArchiveToID, memoryArchiveSetOneID, 1)
			relation[field] = "payload_smuggled_as_identifier"
			if err := ic.validateAndRecord("memory_relations", relation); err == nil ||
				!strings.Contains(err.Error(), "attribution is invalid") {
				t.Fatalf("malformed relation metadata error = %v", err)
			}
		})
	}
}

func TestMemoryArchiveValidationBindsEverySupersessionRelation(t *testing.T) {
	ic := memoryArchiveImportContext()
	if err := ic.validateAndRecord("memories", memoryArchiveHead(memoryArchiveTargetID, float64(2))); err != nil {
		t.Fatal(err)
	}
	if err := ic.validateAndRecord("memory_versions",
		memoryArchiveVersion(memoryArchiveTargetID, 1, nil, 1)); err != nil {
		t.Fatal(err)
	}
	for index, memoryID := range []string{
		memoryArchiveReplacementAID, memoryArchiveReplacementBID,
		memoryArchiveReplacementCID, memoryArchiveReplacementDID,
	} {
		if err := ic.validateAndRecord("memories", memoryArchiveHead(memoryID, float64(1))); err != nil {
			t.Fatal(err)
		}
		version := memoryArchiveVersion(memoryID, 1, nil, int64(index+2))
		if memoryID == memoryArchiveReplacementDID {
			version["request_hash"] = strings.Repeat("c", 64)
		}
		if err := ic.validateAndRecord("memory_versions", version); err != nil {
			t.Fatal(err)
		}
	}
	refs := []MemoryVersionReference{
		{MemoryID: memoryArchiveReplacementAID, Version: 1},
		{MemoryID: memoryArchiveReplacementBID, Version: 1},
	}
	superseded := memoryArchiveLifecycleVersion(
		memoryArchiveTargetID, 2, 6, "superseded", MemoryStateSuperseded, "",
	)
	superseded["supersession_set_id"] = memoryArchivePrimarySetID
	superseded["supersession_set_revision"] = float64(1)
	superseded["supersession_replacement_count"] = float64(len(refs))
	superseded["supersession_replacement_digest"] = memorySupersessionMembershipDigest(refs)
	if err := ic.validateAndRecord("memory_versions", superseded); err != nil {
		t.Fatal(err)
	}

	first := memoryArchiveRelation(
		memoryArchiveRelationAID, memoryArchiveReplacementAID, memoryArchiveTargetID, memoryArchivePrimarySetID, 1,
	)
	first["to_version"] = float64(2)
	if err := ic.validateAndRecord("memory_relations", first); err != nil {
		t.Fatal(err)
	}
	secondInSameSet := memoryArchiveRelation(
		memoryArchiveRelationBID, memoryArchiveReplacementBID, memoryArchiveTargetID, memoryArchivePrimarySetID, 1,
	)
	secondInSameSet["to_version"] = float64(2)
	if err := ic.validateAndRecord("memory_relations", secondInSameSet); err != nil {
		t.Fatalf("second replacement in the same supersession set: %v", err)
	}

	revertedOtherSet := memoryArchiveRelation(
		memoryArchiveRelationCID, memoryArchiveReplacementCID, memoryArchiveTargetID, memoryArchiveRevertedSetID, 2,
	)
	revertedOtherSet["to_version"] = float64(2)
	revertedOtherSet["reverted_at"] = "2026-07-14T08:00:00Z"
	if err := ic.validateAndRecord("memory_relations", revertedOtherSet); err == nil ||
		!strings.Contains(err.Error(), "does not match the target version lineage") {
		t.Fatalf("reverted mismatched supersession error = %v", err)
	}

	conflictingActiveSet := memoryArchiveRelation(
		memoryArchiveRelationDID, memoryArchiveReplacementDID, memoryArchiveTargetID, memoryArchiveConflictSetID, 2,
	)
	conflictingActiveSet["to_version"] = float64(2)
	if err := ic.validateAndRecord("memory_relations", conflictingActiveSet); err == nil ||
		!strings.Contains(err.Error(), "does not match the target version lineage") {
		t.Fatalf("conflicting active supersession set error = %v", err)
	}
	mismatchedRequestHash := memoryArchiveRelation(
		memoryArchiveRelationDID, memoryArchiveReplacementDID,
		memoryArchiveTargetID, memoryArchivePrimarySetID, 1,
	)
	mismatchedRequestHash["to_version"] = float64(2)
	if err := ic.validateAndRecord("memory_relations", mismatchedRequestHash); err == nil ||
		!strings.Contains(err.Error(), "request_hash does not match") {
		t.Fatalf("replacement request-hash mismatch error = %v", err)
	}

	unreceiptedTarget := memoryArchiveRelation(
		memoryArchiveRelationCID, memoryArchiveReplacementCID, memoryArchiveTargetID, memoryArchivePrimarySetID, 1,
	)
	if err := ic.validateAndRecord("memory_relations", unreceiptedTarget); err == nil ||
		!strings.Contains(err.Error(), "does not match the target version lineage") {
		t.Fatalf("unreceipted target relation error = %v", err)
	}

	adjustedReplacement := memoryArchiveLifecycleVersion(
		memoryArchiveReplacementCID, 2, 7, "adjusted", MemoryStateActive, "",
	)
	adjustedReplacement["content"] = "Adjusted replacement"
	adjustedReplacement["content_hash"] = memoryContentHash("Adjusted replacement")
	if err := ic.validateAndRecord("memory_versions", adjustedReplacement); err != nil {
		t.Fatal(err)
	}
	nonInitialReplacement := memoryArchiveRelation(
		memoryArchiveRelationCID, memoryArchiveReplacementCID, memoryArchiveTargetID, memoryArchivePrimarySetID, 1,
	)
	nonInitialReplacement["from_version"] = float64(2)
	nonInitialReplacement["to_version"] = float64(2)
	if err := ic.validateAndRecord("memory_relations", nonInitialReplacement); err == nil ||
		!strings.Contains(err.Error(), "initial added active version") {
		t.Fatalf("non-initial replacement relation error = %v", err)
	}

	unsupported := memoryArchiveRelation(
		memoryArchiveRelationCID, memoryArchiveReplacementCID,
		memoryArchiveTargetID, memoryArchivePrimarySetID, 1,
	)
	unsupported["relation_type"] = "related"
	unsupported["supersession_set_id"] = nil
	unsupported["supersession_set_revision"] = nil
	if err := ic.validateAndRecord("memory_relations", unsupported); err == nil ||
		!strings.Contains(err.Error(), "relation_type \"related\" is invalid") {
		t.Fatalf("unsupported relation type error = %v", err)
	}
	for _, relationID := range []string{
		"mrel_aaaaaaaaaaaaaaaapayload", "mrel_a",
		"mrel_AAAAAAAAAAAAAAAA", "mrel_1111111111111111",
	} {
		relation := memoryArchiveRelation(
			relationID, memoryArchiveReplacementCID,
			memoryArchiveTargetID, memoryArchivePrimarySetID, 1,
		)
		relation["to_version"] = float64(2)
		if err := ic.validateAndRecord("memory_relations", relation); err == nil ||
			!strings.Contains(err.Error(), "id is invalid") {
			t.Fatalf("noncanonical relation id %q error = %v", relationID, err)
		}
	}
	for _, setID := range []string{
		"mset_aaaaaaaaaaaaaaaapayload", "mset_a",
		"mset_AAAAAAAAAAAAAAAA", "mset_1111111111111111",
	} {
		relation := memoryArchiveRelation(
			memoryArchiveRelationCID, memoryArchiveReplacementCID,
			memoryArchiveTargetID, setID, 1,
		)
		relation["to_version"] = float64(2)
		if err := ic.validateAndRecord("memory_relations", relation); err == nil ||
			!strings.Contains(err.Error(), "requires a valid supersession set") {
			t.Fatalf("noncanonical relation set id %q error = %v", setID, err)
		}
	}
}

func TestMemoryArchiveValidationBindsRestoredSupersessionLineage(t *testing.T) {
	ic := memoryArchiveImportContext()
	if err := ic.validateAndRecord("memories",
		memoryArchiveHead(memoryArchiveTargetID, float64(4))); err != nil {
		t.Fatal(err)
	}
	if err := ic.validateAndRecord("memories",
		memoryArchiveHead(memoryArchiveReplacementAID, float64(1))); err != nil {
		t.Fatal(err)
	}
	if err := ic.validateAndRecord("memory_versions",
		memoryArchiveVersion(memoryArchiveTargetID, 1, nil, 1)); err != nil {
		t.Fatal(err)
	}
	if err := ic.validateAndRecord("memory_versions",
		memoryArchiveVersion(memoryArchiveReplacementAID, 1, nil, 2)); err != nil {
		t.Fatal(err)
	}
	refs := []MemoryVersionReference{{MemoryID: memoryArchiveReplacementAID, Version: 1}}
	superseded := memoryArchiveLifecycleVersion(
		memoryArchiveTargetID, 2, 3, "superseded", MemoryStateSuperseded, "",
	)
	superseded["supersession_set_id"] = memoryArchivePrimarySetID
	superseded["supersession_set_revision"] = float64(1)
	superseded["supersession_replacement_count"] = float64(1)
	superseded["supersession_replacement_digest"] = memorySupersessionMembershipDigest(refs)
	forgotten := memoryArchiveLifecycleVersion(
		memoryArchiveTargetID, 3, 4, "forgotten", MemoryStateForgotten, MemoryStateSuperseded,
	)
	restored := memoryArchiveLifecycleVersion(
		memoryArchiveTargetID, 4, 5, "restored", MemoryStateSuperseded, "",
	)
	for _, version := range []map[string]any{superseded, forgotten, restored} {
		if err := ic.validateAndRecord("memory_versions", version); err != nil {
			t.Fatal(err)
		}
	}
	original := memoryArchiveRelation(
		memoryArchiveRelationEID, memoryArchiveReplacementAID,
		memoryArchiveTargetID, memoryArchivePrimarySetID, 1,
	)
	original["to_version"] = float64(2)
	if err := ic.validateAndRecord("memory_relations", original); err != nil {
		t.Fatal(err)
	}
	copied := memoryArchiveRelation(
		memoryArchiveRelationFID, memoryArchiveReplacementAID,
		memoryArchiveTargetID, memoryArchivePrimarySetID, 1,
	)
	copied["to_version"] = float64(4)
	if err := ic.validateAndRecord("memory_relations", copied); err != nil {
		t.Fatal(err)
	}
	if err := validateImportedMemorySupersessionMembership(ic.memories, ic.memoryVersions); err != nil {
		t.Fatalf("restored supersession lineage: %v", err)
	}

	wrongSet := memoryArchiveRelation(
		memoryArchiveRelationDID, memoryArchiveReplacementAID,
		memoryArchiveTargetID, memoryArchiveConflictSetID, 1,
	)
	wrongSet["to_version"] = float64(4)
	if err := ic.validateAndRecord("memory_relations", wrongSet); err == nil ||
		!strings.Contains(err.Error(), "does not match the target version lineage") {
		t.Fatalf("restored relation with wrong lineage error = %v", err)
	}
}

func TestMemoryArchiveValidationEnforcesSupersessionActivityCoherence(t *testing.T) {
	newSingle := func(t *testing.T, currentVersion int64) *importCtx {
		t.Helper()
		ic := memoryArchiveImportContext()
		if err := ic.validateAndRecord("memories",
			memoryArchiveHead(memoryArchiveTargetID, float64(currentVersion))); err != nil {
			t.Fatal(err)
		}
		if err := ic.validateAndRecord("memories",
			memoryArchiveHead(memoryArchiveReplacementAID, float64(1))); err != nil {
			t.Fatal(err)
		}
		if err := ic.validateAndRecord("memory_versions",
			memoryArchiveVersion(memoryArchiveTargetID, 1, nil, 1)); err != nil {
			t.Fatal(err)
		}
		if err := ic.validateAndRecord("memory_versions",
			memoryArchiveVersion(memoryArchiveReplacementAID, 1, nil, 2)); err != nil {
			t.Fatal(err)
		}
		refs := []MemoryVersionReference{{MemoryID: memoryArchiveReplacementAID, Version: 1}}
		superseded := memoryArchiveLifecycleVersion(
			memoryArchiveTargetID, 2, 3, "superseded", MemoryStateSuperseded, "",
		)
		superseded["supersession_set_id"] = memoryArchivePrimarySetID
		superseded["supersession_set_revision"] = float64(1)
		superseded["supersession_replacement_count"] = float64(1)
		superseded["supersession_replacement_digest"] = memorySupersessionMembershipDigest(refs)
		if err := ic.validateAndRecord("memory_versions", superseded); err != nil {
			t.Fatal(err)
		}
		return ic
	}
	addRelation := func(t *testing.T, ic *importCtx, reverted bool) {
		t.Helper()
		relation := memoryArchiveRelation(
			memoryArchiveRelationAID, memoryArchiveReplacementAID,
			memoryArchiveTargetID, memoryArchivePrimarySetID, 1,
		)
		relation["to_version"] = float64(2)
		if reverted {
			relation["reverted_at"] = "2026-07-14T08:00:00Z"
		}
		if err := ic.validateAndRecord("memory_relations", relation); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("current superseded set cannot be reverted", func(t *testing.T) {
		ic := newSingle(t, 2)
		addRelation(t, ic, true)
		if err := validateImportedMemorySupersessionMembership(ic.memories, ic.memoryVersions); err == nil ||
			!strings.Contains(err.Error(), "complete and active") {
			t.Fatalf("reverted current supersession error = %v", err)
		}
	})

	t.Run("forgotten superseded head retains active set", func(t *testing.T) {
		ic := newSingle(t, 3)
		forgotten := memoryArchiveLifecycleVersion(
			memoryArchiveTargetID, 3, 4, "forgotten", MemoryStateForgotten, MemoryStateSuperseded,
		)
		if err := ic.validateAndRecord("memory_versions", forgotten); err != nil {
			t.Fatal(err)
		}
		addRelation(t, ic, false)
		if err := validateImportedMemorySupersessionMembership(ic.memories, ic.memoryVersions); err != nil {
			t.Fatalf("forgotten superseded lineage: %v", err)
		}
	})

	t.Run("reactivated head cannot retain active set", func(t *testing.T) {
		ic := newSingle(t, 3)
		reactivated := memoryArchiveLifecycleVersion(
			memoryArchiveTargetID, 3, 4, "reactivated", MemoryStateActive, "",
		)
		if err := ic.validateAndRecord("memory_versions", reactivated); err != nil {
			t.Fatal(err)
		}
		addRelation(t, ic, false)
		if err := validateImportedMemorySupersessionMembership(ic.memories, ic.memoryVersions); err == nil ||
			!strings.Contains(err.Error(), "active lineage retains active") {
			t.Fatalf("reactivated active-edge error = %v", err)
		}
	})

	t.Run("set cannot mix active and reverted edges", func(t *testing.T) {
		ic := memoryArchiveImportContext()
		if err := ic.validateAndRecord("memories",
			memoryArchiveHead(memoryArchiveTargetID, float64(2))); err != nil {
			t.Fatal(err)
		}
		if err := ic.validateAndRecord("memory_versions",
			memoryArchiveVersion(memoryArchiveTargetID, 1, nil, 1)); err != nil {
			t.Fatal(err)
		}
		refs := []MemoryVersionReference{
			{MemoryID: memoryArchiveReplacementAID, Version: 1},
			{MemoryID: memoryArchiveReplacementBID, Version: 1},
		}
		for index, memoryID := range []string{memoryArchiveReplacementAID, memoryArchiveReplacementBID} {
			if err := ic.validateAndRecord("memories", memoryArchiveHead(memoryID, float64(1))); err != nil {
				t.Fatal(err)
			}
			if err := ic.validateAndRecord("memory_versions",
				memoryArchiveVersion(memoryID, 1, nil, int64(index+2))); err != nil {
				t.Fatal(err)
			}
		}
		superseded := memoryArchiveLifecycleVersion(
			memoryArchiveTargetID, 2, 4, "superseded", MemoryStateSuperseded, "",
		)
		superseded["supersession_set_id"] = memoryArchivePrimarySetID
		superseded["supersession_set_revision"] = float64(1)
		superseded["supersession_replacement_count"] = float64(2)
		superseded["supersession_replacement_digest"] = memorySupersessionMembershipDigest(refs)
		if err := ic.validateAndRecord("memory_versions", superseded); err != nil {
			t.Fatal(err)
		}
		for index, memoryID := range []string{memoryArchiveReplacementAID, memoryArchiveReplacementBID} {
			relation := memoryArchiveRelation(
				[]string{memoryArchiveRelationAID, memoryArchiveRelationBID}[index],
				memoryID, memoryArchiveTargetID, memoryArchivePrimarySetID, 1,
			)
			relation["to_version"] = float64(2)
			if index == 1 {
				relation["reverted_at"] = "2026-07-14T08:00:00Z"
			}
			if err := ic.validateAndRecord("memory_relations", relation); err != nil {
				t.Fatal(err)
			}
		}
		if err := validateImportedMemorySupersessionMembership(ic.memories, ic.memoryVersions); err == nil ||
			!strings.Contains(err.Error(), "mixes active and reverted") {
			t.Fatalf("mixed supersession activity error = %v", err)
		}
	})
}

func TestMemoryArchiveValidationBindsSupersessionMembership(t *testing.T) {
	ic := memoryArchiveImportContext()
	for index, memoryID := range []string{memoryArchiveTargetID, memoryArchiveReplacementAID, memoryArchiveReplacementBID} {
		currentVersion := any(float64(1))
		if memoryID == memoryArchiveTargetID {
			currentVersion = float64(2)
		}
		if err := ic.validateAndRecord("memories", memoryArchiveHead(memoryID, currentVersion)); err != nil {
			t.Fatal(err)
		}
		version := memoryArchiveVersion(memoryID, 1, nil, int64(index+1))
		if memoryID == memoryArchiveTargetID {
			if err := ic.validateAndRecord("memory_versions", version); err != nil {
				t.Fatal(err)
			}
			refs := []MemoryVersionReference{
				{MemoryID: memoryArchiveReplacementAID, Version: 1},
				{MemoryID: memoryArchiveReplacementBID, Version: 1},
			}
			version = memoryArchiveVersion(memoryID, 2, float64(1), 4)
			version["state"] = "superseded"
			version["operation"] = "superseded"
			version["supersession_set_id"] = memoryArchivePrimarySetID
			version["supersession_set_revision"] = float64(1)
			version["supersession_replacement_count"] = float64(len(refs))
			version["supersession_replacement_digest"] = memorySupersessionMembershipDigest(refs)
		}
		if err := ic.validateAndRecord("memory_versions", version); err != nil {
			t.Fatal(err)
		}
	}
	for _, relation := range []map[string]any{
		memoryArchiveRelation(memoryArchiveRelationAID, memoryArchiveReplacementAID, memoryArchiveTargetID, memoryArchivePrimarySetID, 1),
		memoryArchiveRelation(memoryArchiveRelationBID, memoryArchiveReplacementBID, memoryArchiveTargetID, memoryArchivePrimarySetID, 1),
	} {
		relation["to_version"] = float64(2)
		if err := ic.validateAndRecord("memory_relations", relation); err != nil {
			t.Fatal(err)
		}
	}
	if err := validateImportedMemorySupersessionMembership(ic.memories, ic.memoryVersions); err != nil {
		t.Fatalf("intact supersession receipt: %v", err)
	}

	target := memoryVersionImportKey{memoryID: memoryArchiveTargetID, version: 2}
	scope := ic.memoryVersions[target]
	scope.supersessionReplacementDigest = strings.Repeat("f", 64)
	ic.memoryVersions[target] = scope
	if err := validateImportedMemorySupersessionMembership(ic.memories, ic.memoryVersions); err == nil ||
		!strings.Contains(err.Error(), "digest does not match") {
		t.Fatalf("tampered membership digest error = %v", err)
	}

	// A checksummed archive may carry fewer historical relations after an
	// authorized replacement deletion, but it must never import that smaller
	// set as a live/current supersession.
	scope.supersessionReplacementDigest = memorySupersessionMembershipDigest([]MemoryVersionReference{
		{MemoryID: memoryArchiveReplacementAID, Version: 1},
		{MemoryID: memoryArchiveReplacementBID, Version: 1},
	})
	scope.supersessionMembers = scope.supersessionMembers[:1]
	scope.activeSupersessionMemberCount = 1
	ic.memoryVersions[target] = scope
	if err := validateImportedMemorySupersessionMembership(ic.memories, ic.memoryVersions); err == nil ||
		!strings.Contains(err.Error(), "incomplete live replacement set") {
		t.Fatalf("partial live membership error = %v", err)
	}

	scope.activeSupersessionMemberCount = 0
	ic.memoryVersions[target] = scope
	memory := ic.memories[memoryArchiveTargetID]
	memory.currentVersion = 3
	ic.memories[memoryArchiveTargetID] = memory
	if err := validateImportedMemorySupersessionMembership(ic.memories, ic.memoryVersions); err != nil {
		t.Fatalf("historical post-delete membership must remain portable: %v", err)
	}

	missingReceipt := memoryArchiveVersion(memoryArchiveMissingReceiptID, 2, float64(1), 9)
	missingReceipt["state"] = "superseded"
	missingReceipt["operation"] = "superseded"
	missingContext := memoryArchiveImportContext()
	if err := missingContext.validateAndRecord("memories",
		memoryArchiveHead(memoryArchiveMissingReceiptID, float64(2))); err != nil {
		t.Fatal(err)
	}
	if err := missingContext.validateAndRecord("memory_versions",
		memoryArchiveVersion(memoryArchiveMissingReceiptID, 1, nil, 8)); err != nil {
		t.Fatal(err)
	}
	if err := missingContext.validateAndRecord("memory_versions", missingReceipt); err == nil {
		t.Fatal("superseded version without an immutable receipt was accepted")
	}
}

func memoryArchiveImportContext() *importCtx {
	ic := newImportCtx("acc_1")
	ic.realms["realm_1"] = true
	ic.agents["agent_1"] = true
	ic.agents["agent_2"] = true
	ic.agentRealms["agent_1"] = "realm_1"
	ic.agentRealms["agent_2"] = "realm_1"
	return ic
}

func memoryArchiveHead(id string, currentVersion any) map[string]any {
	return map[string]any{
		"id": id, "account_id": "acc_1", "realm_id": "realm_1",
		"owner_kind": "agent", "owner_id": "agent_1",
		"origin": "self", "capture_reason": "manual",
		"authored_by_agent_id": "agent_1", "current_version": currentVersion,
		"delete_receipt_id": "", "delete_idempotency_key_hash": "",
		"deleted_prior_version": float64(0), "deleted_scrub_set_revision": "",
		"deleted_version_count": float64(0), "deleted_evidence_count": float64(0),
		"deleted_relation_count": float64(0), "deleted_retry_shield_count": float64(0),
		"deleted_retry_shield_digest": "",
	}
}

func memoryArchiveVersion(memoryID string, version int64, previous any, changeSeq int64) map[string]any {
	content := "A durable decision"
	return map[string]any{
		"memory_id": memoryID, "version": float64(version),
		"account_id": "acc_1", "realm_id": "realm_1",
		"owner_kind": "agent", "owner_id": "agent_1",
		"previous_version": previous, "change_seq": float64(changeSeq),
		"content": content, "content_encoding": "plain",
		"kind": "episodic", "tags": []any{"architecture"},
		"links": []any{}, "salience": float64(0.5), "sensitive": false,
		"occurred_from": nil, "occurred_until": nil,
		"actor_id": "agent_1", "state": "active",
		"prior_state": nil,
		"operation":   "added", "supersession_set_id": "",
		"supersession_set_revision":       float64(0),
		"supersession_replacement_count":  float64(0),
		"supersession_replacement_digest": "",
		"content_hash":                    memoryContentHash(content),
		"request_hash":                    strings.Repeat("b", 64),
		"idempotency_key":                 "capture-1",
	}
}

func memoryArchiveLifecycleVersion(
	memoryID string,
	version, changeSeq int64,
	operation, state, priorState string,
) map[string]any {
	row := memoryArchiveVersion(memoryID, version, float64(version-1), changeSeq)
	row["operation"] = operation
	row["state"] = state
	if priorState == "" {
		row["prior_state"] = nil
	} else {
		row["prior_state"] = priorState
	}
	return row
}

func memoryArchiveDeletedHead(id string, shield memoryDeleteRetryShield) map[string]any {
	deleted := memoryArchiveHead(id, nil)
	deleted["origin"] = "deleted"
	deleted["capture_reason"] = "deleted"
	deleted["created_at"] = "2026-07-14T07:00:00Z"
	deleted["updated_at"] = "2026-07-14T08:00:00Z"
	deleted["permanently_deleted_at"] = "2026-07-14T08:00:00Z"
	deleted["permanently_deleted_by_id"] = "agent_1"
	deleted["permanent_delete_reason"] = "direct_user_request"
	deleted["delete_receipt_id"] = "mdel_aaaaaaaaaaaaaaaa"
	deleted["delete_idempotency_key_hash"] = strings.Repeat("a", 64)
	deleted["deleted_prior_version"] = float64(1)
	deleted["deleted_scrub_set_revision"] = strings.Repeat("b", 64)
	deleted["deleted_version_count"] = float64(1)
	deleted["deleted_evidence_count"] = float64(1)
	deleted["deleted_relation_count"] = float64(0)
	deleted["deleted_retry_shield_count"] = float64(1)
	deleted["deleted_retry_shield_digest"] = memoryDeleteRetryShieldDigest([]memoryDeleteRetryShield{shield})
	return deleted
}

func memoryArchiveDeletedReference(
	id, memoryID string,
	shield memoryDeleteRetryShield,
) map[string]any {
	return map[string]any{
		"id": id, "account_id": "acc_1", "realm_id": "realm_1",
		"owner_kind": "agent", "owner_id": "agent_1",
		"deleted_memory_id":     memoryID,
		"former_reference_kind": shield.Kind,
		"related_resource_id":   shield.Hash,
		"reason_code":           "permanent_delete",
	}
}

func memoryArchiveEvidence(id, state string, changeSeq int64) map[string]any {
	return map[string]any{
		"id": id, "account_id": "acc_1", "realm_id": "realm_1",
		"owner_kind": "agent", "owner_id": "agent_1",
		"memory_id": memoryArchiveOneID, "target_version": float64(1),
		"evidence_change_seq": float64(changeSeq),
		"resolution_state":    state, "actor_id": "agent_1",
	}
}

func memoryArchiveRelation(id, fromMemoryID, toMemoryID, setID string, revision int64) map[string]any {
	return map[string]any{
		"id": id, "account_id": "acc_1", "realm_id": "realm_1",
		"owner_kind": "agent", "owner_id": "agent_1",
		"from_memory_id": fromMemoryID, "from_version": float64(1),
		"to_memory_id": toMemoryID, "to_version": float64(1),
		"relation_type": "supersedes", "supersession_set_id": setID,
		"supersession_set_revision": float64(revision),
	}
}
