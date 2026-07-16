package main

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/witwave-ai/witself/internal/server"
	"github.com/witwave-ai/witself/internal/store"
)

func TestMemoryAdapterPreservesAuthorityAndEvidence(t *testing.T) {
	p := server.DomainPrincipal{Kind: server.PrincipalKindAgent, ID: "agent_1", AgentName: "scott"}
	from, until, sourceVersion := int64(2), int64(4), int64(7)
	inputs := toStoreMemoryEvidenceInputs([]server.MemoryEvidenceInput{
		{State: "resolved", TranscriptID: "trn_1", EntryFromSequence: &from, EntryUntilSequence: &until},
		{State: "resolved", SourceMemoryID: "mem_source", SourceMemoryVersion: &sourceVersion},
		{State: "resolved", MessageID: "msg_1"},
		{State: "resolved", ImportArtifactID: "archive/row/9"},
	})
	wantKinds := []string{"transcript", "memory", "message", "import_artifact"}
	for i := range inputs {
		if inputs[i].ResolvedKind != wantKinds[i] {
			t.Fatalf("evidence[%d] resolved kind = %q, want %q", i, inputs[i].ResolvedKind, wantKinds[i])
		}
	}

	now := time.Date(2026, 7, 14, 12, 0, 0, 0, time.UTC)
	result := toServerMemoryMutationResult(store.MemoryMutationResult{
		Memory: store.Memory{
			ID: "mem_1", AccountID: "acc_1", RealmID: "realm_1",
			OwnerKind: "agent", OwnerID: "agent_1", AuthoredByAgentID: "agent_1",
			Version: 2, PreviousVersion: 1, Content: "ZGVjaXNpb24=", ContentEncoding: "base64",
			Kind: "decision",
			Tags: []string{}, Links: []string{}, State: store.MemoryStateActive,
			Operation: "adjusted", ActorKind: "agent", ActorID: "agent_1",
			SupersessionSetID: "mset_receipt", SupersessionSetRevision: 2,
			SupersessionReplacementCount: 3, SupersessionReplacementDigest: strings.Repeat("c", 64),
			ActiveSupersessionSetID: "mset_active", ActiveSupersessionSetRevision: 4,
			CreatedAt: now, UpdatedAt: now,
			Evidence: []store.MemoryEvidence{{
				ID: "mev_1", MemoryID: "mem_1", TargetVersion: 1,
				ResolutionState:    store.MemoryEvidenceUnavailable,
				TerminalReasonCode: "runtime_did_not_record", ActorID: "agent_1",
			}},
		},
		Receipt: store.MemoryMutationReceipt{
			Operation: "adjusted", ActorID: "agent_1", IdempotencyKey: "adjust-1",
			RequestHash: strings.Repeat("a", 64), MemoryID: "mem_1", Version: 2,
			CreatedAt: now, Replayed: true,
		},
	}, p)
	if result.Memory.Owner.AgentName != "scott" || result.Memory.Actor.Name != "scott" || result.Memory.Operation != "adjust" {
		t.Fatalf("mapped memory authority = %#v", result.Memory)
	}
	if result.Memory.PreviousVersion == nil || *result.Memory.PreviousVersion != 1 {
		t.Fatalf("previous version = %#v", result.Memory.PreviousVersion)
	}
	if result.Memory.ContentEncoding != "base64" {
		t.Fatalf("content encoding = %q", result.Memory.ContentEncoding)
	}
	if result.Memory.SupersessionSetID != "mset_receipt" || result.Memory.SupersessionSetRevision != 2 ||
		result.Memory.SupersessionReplacementCount != 3 ||
		result.Memory.SupersessionReplacementDigest != strings.Repeat("c", 64) ||
		result.Memory.ActiveSupersessionSetID != "mset_active" ||
		result.Memory.ActiveSupersessionSetRevision != 4 {
		t.Fatalf("mapped supersession metadata = %#v", result.Memory)
	}
	if len(result.Memory.Evidence) != 1 || result.Memory.Evidence[0].UnavailableReason != "runtime_did_not_record" {
		t.Fatalf("mapped evidence = %#v", result.Memory.Evidence)
	}
	if result.Receipt.Operation != "adjust" || result.Receipt.CanonicalRequestHash != strings.Repeat("a", 64) || !result.Receipt.Replayed {
		t.Fatalf("mapped receipt = %#v", result.Receipt)
	}
	version := toServerMemoryVersion(store.Memory{
		ID: "mem_1", Version: 2,
		SupersessionSetID: "mset_receipt", SupersessionSetRevision: 2,
		SupersessionReplacementCount: 3, SupersessionReplacementDigest: strings.Repeat("c", 64),
		ActiveSupersessionSetID: "mset_active", ActiveSupersessionSetRevision: 4,
	}, p)
	if version.SupersessionSetID != "mset_receipt" || version.SupersessionSetRevision != 2 ||
		version.SupersessionReplacementCount != 3 ||
		version.SupersessionReplacementDigest != strings.Repeat("c", 64) ||
		version.ActiveSupersessionSetID != "mset_active" || version.ActiveSupersessionSetRevision != 4 {
		t.Fatalf("mapped history supersession metadata = %#v", version)
	}
}

func TestMemorySupersedeAdapterPreservesAtomicSetAndAuthority(t *testing.T) {
	p := server.DomainPrincipal{Kind: server.PrincipalKindAgent, ID: "agent_1", AgentName: "scott"}
	salience := 0.95
	storeInput := toStoreSupersedeMemoryInput("mem_source", server.SupersedeMemoryRequest{
		ExpectedVersion: 2, Reason: "split decision", IdempotencyKey: "supersede-1",
		Client: server.MemoryClientProvenance{Runtime: "codex", Model: "gpt-5"},
		Replacements: []server.SupersedeMemoryReplacementRequest{{
			Content: "cmVwbGFjZW1lbnQ=", ContentEncoding: "base64",
			Kind: "decision", Salience: &salience,
			CaptureReason: "curation", IdempotencyKey: "replacement-1",
			Client:   server.MemoryClientProvenance{Runtime: "claude", Model: "sonnet"},
			Evidence: []server.MemoryEvidenceInput{{State: "pending", ExternalLocator: "claude/turn/9"}},
		}},
	})
	if storeInput.MemoryID != "mem_source" || storeInput.ExpectedVersion != 2 ||
		storeInput.IdempotencyKey != "supersede-1" || storeInput.Client.Runtime != "codex" ||
		len(storeInput.Replacements) != 1 {
		t.Fatalf("mapped supersede input = %#v", storeInput)
	}
	replacement := storeInput.Replacements[0]
	if replacement.Origin != "agent" || replacement.CaptureReason != "curation" ||
		replacement.ContentEncoding != "base64" ||
		replacement.IdempotencyKey != "replacement-1" || replacement.Client.Runtime != "claude" ||
		len(replacement.Evidence) != 1 || replacement.Evidence[0].ExternalLocator != "claude/turn/9" {
		t.Fatalf("mapped supersede replacement = %#v", replacement)
	}

	now := time.Date(2026, 7, 14, 13, 0, 0, 0, time.UTC)
	mapped := toServerSupersedeMemoryResult(store.SupersedeMemoryResult{
		Source: store.Memory{
			ID: "mem_source", AccountID: "acc_1", RealmID: "realm_1",
			OwnerKind: "agent", OwnerID: "agent_1", AuthoredByAgentID: "agent_1",
			Version: 3, PreviousVersion: 2, Content: "source value", ContentEncoding: "plain", Kind: "decision",
			State: store.MemoryStateSuperseded, Operation: "superseded",
			SupersessionSetID: "mset_1", SupersessionSetRevision: 1,
			SupersessionReplacementCount: 1, SupersessionReplacementDigest: strings.Repeat("d", 64),
			ActiveSupersessionSetID: "mset_1", ActiveSupersessionSetRevision: 1,
			ActorKind: "agent", ActorID: "agent_1", CreatedAt: now, UpdatedAt: now,
		},
		Replacements: []store.Memory{{
			ID: "mem_replacement", AccountID: "acc_1", RealmID: "realm_1",
			OwnerKind: "agent", OwnerID: "agent_1", AuthoredByAgentID: "agent_1",
			Version: 1, Content: "cmVwbGFjZW1lbnQgdmFsdWU=", ContentEncoding: "base64", Kind: "decision",
			State: store.MemoryStateActive, Operation: "added",
			ActorKind: "agent", ActorID: "agent_1", CreatedAt: now, UpdatedAt: now,
		}},
		Receipt: store.MemorySupersessionReceipt{
			Operation: "superseded", ActorID: "agent_1", IdempotencyKey: "supersede-1",
			RequestHash: strings.Repeat("c", 64), SupersessionSetID: "mset_1",
			SupersessionSetRevision: 1,
			ReplacementCount:        1, ReplacementDigest: strings.Repeat("d", 64),
			Source:       store.MemoryVersionReference{MemoryID: "mem_source", Version: 3},
			Replacements: []store.MemoryVersionReference{{MemoryID: "mem_replacement", Version: 1}},
			CreatedAt:    now, Replayed: true,
		},
	}, p)
	if mapped.Source.Operation != "supersede" || mapped.Source.Actor.Name != "scott" ||
		len(mapped.Replacements) != 1 || mapped.Replacements[0].Content != "cmVwbGFjZW1lbnQgdmFsdWU=" ||
		mapped.Source.ContentEncoding != "plain" || mapped.Replacements[0].ContentEncoding != "base64" ||
		mapped.Receipt.Operation != "supersede" || mapped.Receipt.Actor.Name != "scott" ||
		mapped.Receipt.CanonicalRequestHash != strings.Repeat("c", 64) ||
		mapped.Receipt.ReplacementCount != 1 || mapped.Receipt.ReplacementDigest != strings.Repeat("d", 64) ||
		mapped.Source.SupersessionSetID != "mset_1" || mapped.Source.SupersessionSetRevision != 1 ||
		mapped.Source.SupersessionReplacementCount != 1 ||
		mapped.Source.SupersessionReplacementDigest != strings.Repeat("d", 64) ||
		mapped.Source.ActiveSupersessionSetID != "mset_1" || mapped.Source.ActiveSupersessionSetRevision != 1 ||
		mapped.Receipt.Source.MemoryID != "mem_source" || len(mapped.Receipt.Replacements) != 1 ||
		!mapped.Receipt.Replayed {
		t.Fatalf("mapped supersede result = %#v", mapped)
	}
}

func TestMemoryAdapterErrorsAndSnippet(t *testing.T) {
	if !errors.Is(mapMemoryError(store.ErrMemoryInputInvalid), server.ErrBadInput) {
		t.Fatal("invalid memory input was not mapped to bad input")
	}
	if !errors.Is(mapMemoryError(store.ErrMemoryEvidenceConflict), server.ErrConflict) {
		t.Fatal("evidence conflict was not mapped to conflict")
	}
	if !errors.Is(mapMemoryError(store.ErrMemoryIdempotencyConflict), server.ErrIdempotencyConflict) {
		t.Fatal("memory idempotency conflict was not mapped")
	}
	if !errors.Is(mapMemoryError(store.ErrMemoryDeleted), server.ErrMemoryDeleted) {
		t.Fatal("permanently deleted memory was not mapped to gone")
	}
	if !errors.Is(mapMemoryError(store.ErrMemoryDependency), server.ErrMemoryDependency) {
		t.Fatal("memory deletion dependency was not mapped to conflict")
	}
	long := strings.Repeat("🙂", selfMemorySnippetRunes+5)
	snippet := memorySnippet(long)
	if strings.Count(snippet, "🙂") != selfMemorySnippetRunes || !strings.HasSuffix(snippet, "…") {
		t.Fatalf("snippet rune bound failed: runes=%d suffix=%q", len([]rune(snippet)), snippet[len(snippet)-3:])
	}
}

func TestSelfMemoryAdapterOmitsNonPlainContent(t *testing.T) {
	plain := toServerSelfMemory(store.Memory{
		ID: "mem_plain", Content: "private preference", ContentEncoding: "plain",
		Kind: "profile", Sensitive: true,
	})
	if plain.Snippet != "private preference" || plain.Redacted || plain.ContentEncoding != "plain" || !plain.Sensitive {
		t.Fatalf("plain self memory = %#v", plain)
	}

	nonPlain := toServerSelfMemory(store.Memory{
		ID: "mem_base64", Content: "c2VjcmV0", ContentEncoding: "base64", Kind: "profile",
	})
	if nonPlain.Snippet != "" || !nonPlain.Redacted || nonPlain.ContentEncoding != "base64" {
		t.Fatalf("non-plain self memory = %#v", nonPlain)
	}
}
