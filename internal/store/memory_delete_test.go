package store

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestDeleteMemoryResultIsValueFree(t *testing.T) {
	deletedAt := time.Date(2026, 7, 14, 8, 0, 0, 0, time.UTC)
	raw, err := json.Marshal(DeleteMemoryResult{
		MemoryID: "mem_1", ReceiptID: "mdel_1", PriorVersion: 3,
		ScrubSetRevision: strings.Repeat("a", 64), VersionCount: 3,
		EvidenceCount: 2, RelationCount: 1, RetryShieldCount: 4,
		RetryShieldDigest: strings.Repeat("b", 64), DeletedAt: &deletedAt,
		Applied: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	for _, secret := range []string{"private narrative", "raw-retry-key", "secret locator"} {
		if strings.Contains(text, secret) {
			t.Fatalf("memory deletion result leaked %q: %s", secret, text)
		}
	}
	var shape map[string]any
	if err := json.Unmarshal(raw, &shape); err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{
		"content", "content_hash", "tags", "links", "evidence", "origin",
		"capture_reason", "client", "idempotency_key", "request_hash", "reason_code",
	} {
		if _, ok := shape[forbidden]; ok {
			t.Fatalf("memory deletion result contains value-bearing field %q: %s", forbidden, text)
		}
	}
}

func TestNormalizeDeleteMemoryInput(t *testing.T) {
	if _, err := normalizeDeleteMemoryInput(DeleteMemoryInput{MemoryID: "mem_1"}); err != nil {
		t.Fatalf("preview: %v", err)
	}
	if _, err := normalizeDeleteMemoryInput(DeleteMemoryInput{
		MemoryID: "mem_1", ExpectedVersion: 1,
	}); !errors.Is(err, ErrMemoryInputInvalid) {
		t.Fatalf("partial preview guard = %v", err)
	}
	apply, err := normalizeDeleteMemoryInput(DeleteMemoryInput{
		MemoryID: "mem_1", ExpectedVersion: 1,
		ScrubSetRevision: strings.Repeat("A", 64),
		IdempotencyKey:   "delete-1", Apply: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if apply.ReasonCode != "direct_user_request" || apply.ScrubSetRevision != strings.Repeat("a", 64) {
		t.Fatalf("normalized apply = %#v", apply)
	}
	if _, err := normalizeDeleteMemoryInput(DeleteMemoryInput{
		MemoryID: "mem_1", ExpectedVersion: 1,
		ScrubSetRevision: strings.Repeat("a", 64), IdempotencyKey: "delete-2",
		ReasonCode: "private_123456789", Apply: true,
	}); !errors.Is(err, ErrMemoryInputInvalid) {
		t.Fatalf("caller-controlled deletion reason = %v", err)
	}
}

func TestMemoryDeleteRetryShieldDigestIsOrderedAndValueFree(t *testing.T) {
	first := []memoryDeleteRetryShield{
		{Kind: "idempotency.added", Hash: strings.Repeat("a", 64)},
		{Kind: "idempotency.adjusted", Hash: strings.Repeat("b", 64)},
	}
	second := []memoryDeleteRetryShield{first[1], first[0]}
	if got, want := memoryDeleteRetryShieldDigest(first), memoryDeleteRetryShieldDigest(second); got != want || !isSHA256Hex(got) {
		t.Fatalf("retry shield digest = %q / %q", got, want)
	}
	second[1].Hash = strings.Repeat("c", 64)
	if memoryDeleteRetryShieldDigest(first) == memoryDeleteRetryShieldDigest(second) {
		t.Fatal("retry shield digest ignored a shield change")
	}
}

func TestMemoryAuditEventShapesAreValueFree(t *testing.T) {
	if err := checkEventShape(EventInput{
		AccountID: "acc_1", ActorKind: ActorAgent, ActorID: "agent_1",
		Verb: VerbMemoryDeleted,
		Metadata: map[string]any{
			"memory_id": "mem_1", "receipt_id": "mdel_1", "prior_version": "3",
			"version_count": "3", "evidence_count": "2", "relation_count": "1",
			"retry_shield_count": "4",
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := checkEventShape(EventInput{
		AccountID: "acc_1", ActorKind: ActorAgent, ActorID: "agent_1",
		Verb: VerbMemoryDeleted,
		Metadata: map[string]any{
			"memory_id": "mem_1", "receipt_id": "mdel_1", "prior_version": "3",
			"content": "private narrative",
		},
	}); !errors.Is(err, ErrBadEventMetadata) {
		t.Fatalf("value-bearing memory audit event = %v", err)
	}
}
