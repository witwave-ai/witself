package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/witwave-ai/witself/internal/server"
	"github.com/witwave-ai/witself/internal/store"
)

const selfMemorySnippetRunes = 280

// configureMemory binds the transport-neutral server contract to the
// PostgreSQL store. Every authority-bearing owner and actor field comes from
// the authenticated principal; client-supplied provenance remains metadata.
func configureMemory(cfg *server.Config, st *store.Store) {
	cfg.CaptureMemory = func(ctx context.Context, p server.DomainPrincipal, in server.CaptureMemoryRequest) (server.MemoryMutationResult, error) {
		result, err := st.CaptureMemory(ctx, toStorePrincipal(p), store.CaptureMemoryInput{
			Content: in.Content, ContentEncoding: in.ContentEncoding,
			Kind: in.Kind, Tags: in.Tags, Links: in.Links,
			Salience: in.Salience, Sensitive: in.Sensitive,
			OccurredFrom: in.OccurredFrom, OccurredUntil: in.OccurredUntil,
			Origin: "agent", CaptureReason: in.CaptureReason,
			Evidence:       toStoreMemoryEvidenceInputs(in.Evidence),
			Client:         toStoreMemoryClient(in.Client),
			IdempotencyKey: in.IdempotencyKey,
		})
		if err != nil {
			return server.MemoryMutationResult{}, mapMemoryError(err)
		}
		return toServerMemoryMutationResult(result, p), nil
	}
	cfg.GetMemory = func(ctx context.Context, p server.DomainPrincipal, memoryID string) (server.Memory, error) {
		memory, err := st.GetMemory(ctx, toStorePrincipal(p), memoryID)
		if err != nil {
			return server.Memory{}, mapMemoryError(err)
		}
		return toServerMemory(memory, p), nil
	}
	cfg.ListMemories = func(ctx context.Context, p server.DomainPrincipal, opts server.MemoryListOptions) (server.MemoryPage, error) {
		page, err := st.ListMemories(ctx, toStorePrincipal(p), store.MemoryListOptions{
			OwnerAgentID: opts.OwnerAgentID, State: opts.State, Kind: opts.Kind,
			Tags: opts.Tags, Origin: opts.Origin, CaptureReason: opts.CaptureReason,
			IncludeSensitive: opts.IncludeSensitive,
			OccurredFrom:     opts.OccurredFrom, OccurredUntil: opts.OccurredUntil,
			CapturedFrom: opts.CapturedFrom, CapturedUntil: opts.CapturedUntil,
			Limit: opts.Limit, Cursor: opts.Cursor,
		})
		if err != nil {
			return server.MemoryPage{}, mapMemoryError(err)
		}
		items := make([]server.Memory, len(page.Memories))
		for i := range page.Memories {
			items[i] = toServerMemory(page.Memories[i], p)
		}
		return server.MemoryPage{Items: items, NextCursor: page.NextCursor}, nil
	}
	cfg.RecallMemories = func(ctx context.Context, p server.DomainPrincipal, in server.MemoryRecallRequest) (server.MemoryRecallPage, error) {
		page, err := st.RecallMemories(ctx, toStorePrincipal(p), store.MemoryRecallOptions{
			Query: in.Query, Kind: in.Kind, Tags: in.Tags, Links: in.Links,
			Origin: in.Origin, CaptureReason: in.CaptureReason,
			IncludeSensitive: in.IncludeSensitive,
			OccurredFrom:     in.OccurredFrom, OccurredUntil: in.OccurredUntil,
			CapturedFrom: in.CapturedFrom, CapturedUntil: in.CapturedUntil,
			Limit: in.Limit, Cursor: in.Cursor,
			VectorProfileID: in.VectorProfileID, QueryVector: in.QueryVector,
		})
		if err != nil {
			return server.MemoryRecallPage{}, mapMemoryError(err)
		}
		hits := make([]server.MemoryRecallHit, len(page.Hits))
		for i := range page.Hits {
			hits[i] = server.MemoryRecallHit{
				Memory: toServerMemory(page.Hits[i].Memory, p),
				Score: server.MemoryRecallScore{
					Similarity: page.Hits[i].Score.Similarity, VectorUsed: page.Hits[i].Score.VectorUsed,
					Lexical: page.Hits[i].Score.Lexical, Salience: page.Hits[i].Score.Salience,
					Recency: page.Hits[i].Score.Recency, Total: page.Hits[i].Score.Total,
				},
			}
		}
		return server.MemoryRecallPage{
			Hits: hits, NextCursor: page.NextCursor, RetrievalMode: page.RetrievalMode,
			VectorCoverage: page.VectorCoverage, Degraded: page.Degraded,
			DegradedReason:  page.DegradedReason,
			VectorProfileID: page.VectorProfileID, VectorCandidates: page.VectorCandidates,
			VectorMatches:      page.VectorMatches,
			CandidateTruncated: page.CandidateTruncated, CandidateLimit: page.CandidateLimit,
		}, nil
	}
	cfg.CreateMemoryVectorProfile = func(ctx context.Context, p server.DomainPrincipal, in server.CreateMemoryVectorProfileRequest) (server.MemoryVectorProfile, error) {
		profile, err := st.CreateMemoryVectorProfile(ctx, toStorePrincipal(p), store.CreateMemoryVectorProfileInput{
			Provider: in.Provider, Model: in.Model, Recipe: in.Recipe,
			RecipeVersion: in.RecipeVersion, Dimensions: in.Dimensions,
			DistanceMetric: in.DistanceMetric, Normalization: in.Normalization,
		})
		if err != nil {
			return server.MemoryVectorProfile{}, mapMemoryError(err)
		}
		return toServerMemoryVectorProfile(profile), nil
	}
	cfg.ListMemoryVectorProfiles = func(ctx context.Context, p server.DomainPrincipal) ([]server.MemoryVectorProfile, error) {
		profiles, err := st.ListMemoryVectorProfiles(ctx, toStorePrincipal(p))
		if err != nil {
			return nil, mapMemoryError(err)
		}
		out := make([]server.MemoryVectorProfile, len(profiles))
		for i := range profiles {
			out[i] = toServerMemoryVectorProfile(profiles[i])
		}
		return out, nil
	}
	cfg.PutMemoryVector = func(ctx context.Context, p server.DomainPrincipal, in server.PutMemoryVectorRequest) (server.MemoryVectorReceipt, error) {
		receipt, err := st.PutMemoryVector(ctx, toStorePrincipal(p), store.PutMemoryVectorInput{
			ProfileID: in.ProfileID, MemoryID: in.MemoryID, MemoryVersion: in.MemoryVersion,
			ContentHash: in.ContentHash, Vector: in.Vector,
		})
		if err != nil {
			return server.MemoryVectorReceipt{}, mapMemoryError(err)
		}
		return server.MemoryVectorReceipt{ProfileID: receipt.ProfileID, MemoryID: receipt.MemoryID,
			MemoryVersion: receipt.MemoryVersion, ContentHash: receipt.ContentHash,
			VectorHash: receipt.VectorHash, Dimensions: receipt.Dimensions,
			CreatedAt: receipt.CreatedAt, Replayed: receipt.Replayed}, nil
	}
	cfg.GetMemoryHistory = func(ctx context.Context, p server.DomainPrincipal, memoryID string, opts server.MemoryHistoryOptions) (server.MemoryHistoryPage, error) {
		page, err := st.GetMemoryHistoryPage(ctx, toStorePrincipal(p), memoryID, store.MemoryHistoryOptions{
			Limit: opts.Limit, Cursor: opts.Cursor,
		})
		if err != nil {
			return server.MemoryHistoryPage{}, mapMemoryError(err)
		}
		versions := make([]server.MemoryVersion, len(page.Versions))
		for i := range page.Versions {
			versions[i] = toServerMemoryVersion(page.Versions[i], p)
		}
		return server.MemoryHistoryPage{Versions: versions, NextCursor: page.NextCursor}, nil
	}
	cfg.AdjustMemory = func(ctx context.Context, p server.DomainPrincipal, memoryID string, in server.AdjustMemoryRequest) (server.MemoryMutationResult, error) {
		result, err := st.AdjustMemory(ctx, toStorePrincipal(p), memoryID, store.AdjustMemoryInput{
			ExpectedVersion: in.ExpectedVersion, Content: in.SetContent,
			ContentEncoding: in.SetContentEncoding, Kind: in.SetKind,
			AddTags: in.AddTags, RemoveTags: in.RemoveTags,
			Salience: in.SetSalience, AddLinks: in.AddLinks, RemoveLinks: in.RemoveLinks,
			Sensitive:    in.SetSensitive,
			OccurredFrom: in.SetOccurredFrom, ClearOccurredFrom: in.ClearOccurredFrom,
			OccurredUntil: in.SetOccurredUntil, ClearOccurredUntil: in.ClearOccurredUntil,
			Reason: in.Reason, Client: toStoreMemoryClient(in.Client),
			IdempotencyKey: in.IdempotencyKey,
		})
		if err != nil {
			return server.MemoryMutationResult{}, mapMemoryError(err)
		}
		return toServerMemoryMutationResult(result, p), nil
	}
	cfg.SupersedeMemory = func(ctx context.Context, p server.DomainPrincipal, memoryID string, in server.SupersedeMemoryRequest) (server.SupersedeMemoryResult, error) {
		result, err := st.SupersedeMemory(ctx, toStorePrincipal(p), toStoreSupersedeMemoryInput(memoryID, in))
		if err != nil {
			return server.SupersedeMemoryResult{}, mapMemoryError(err)
		}
		return toServerSupersedeMemoryResult(result, p), nil
	}
	cfg.ForgetMemory = memoryLifecycleAdapter(st.ForgetMemory)
	cfg.RestoreMemory = memoryLifecycleAdapter(st.RestoreMemory)
	cfg.ReactivateMemory = memoryLifecycleAdapter(st.ReactivateMemory)
	cfg.ResolveMemoryEvidence = func(ctx context.Context, p server.DomainPrincipal, evidenceID string, in server.ResolveMemoryEvidenceRequest) (server.MemoryEvidence, error) {
		storeInput := store.ResolveMemoryEvidenceInput{
			ResolvedKind:        resolvedMemoryEvidenceKind(in),
			SourceTranscriptID:  in.TranscriptID,
			SourceMemoryID:      in.SourceMemoryID,
			SourceMessageID:     in.MessageID,
			SourceImportLocator: in.ImportArtifactID,
			UnresolvableReason:  in.UnresolvableReason,
			SourceDigest:        in.SourceDigest,
			IdempotencyKey:      in.IdempotencyKey,
		}
		if in.EntryFromSequence != nil {
			storeInput.SourceSequenceFrom = *in.EntryFromSequence
		}
		if in.EntryUntilSequence != nil {
			storeInput.SourceSequenceUntil = *in.EntryUntilSequence
		}
		if in.SourceMemoryVersion != nil {
			storeInput.SourceMemoryVersion = *in.SourceMemoryVersion
		}
		evidence, err := st.ResolveMemoryEvidence(ctx, toStorePrincipal(p), evidenceID, storeInput)
		if err != nil {
			return server.MemoryEvidence{}, mapMemoryError(err)
		}
		return toServerMemoryEvidence(evidence, p), nil
	}
	cfg.DeleteMemory = func(ctx context.Context, p server.DomainPrincipal, in server.DeleteMemoryRequest) (server.MemoryDeletionReceipt, error) {
		result, err := st.DeleteMemory(ctx, toStorePrincipal(p), store.DeleteMemoryInput{
			MemoryID: in.MemoryID, ExpectedVersion: in.ExpectedVersion,
			ScrubSetRevision: in.ScrubSetRevision, ReasonCode: in.ReasonCode,
			IdempotencyKey: in.IdempotencyKey, Apply: in.Apply,
		})
		if err != nil {
			return server.MemoryDeletionReceipt{}, mapMemoryError(err)
		}
		return server.MemoryDeletionReceipt{
			MemoryID: result.MemoryID, ReceiptID: result.ReceiptID,
			PriorVersion: result.PriorVersion, ScrubSetRevision: result.ScrubSetRevision,
			VersionCount: result.VersionCount, EvidenceCount: result.EvidenceCount,
			RelationCount: result.RelationCount, RetryShieldCount: result.RetryShieldCount,
			RetryShieldDigest:             result.RetryShieldDigest,
			IncomingEvidenceCount:         result.IncomingEvidenceCount,
			ActiveRelationDependencyCount: result.ActiveRelationDependencyCount,
			ActiveCurationDependencyCount: result.ActiveCurationDependencyCount,
			CurationRunCount:              result.CurationRunCount,
			CurationActionCount:           result.CurationActionCount,
			CurationInputCount:            result.CurationInputCount,
			CurationMutationCount:         result.CurationMutationCount,
			Blocked:                       result.Blocked, DeletedAt: result.DeletedAt,
			Applied: result.Applied, Replayed: result.Replayed,
		}, nil
	}

	cfg.GetSelfMemories = func(ctx context.Context, p server.DomainPrincipal, limit int, includeCount bool) ([]server.SelfMemory, int, error) {
		principal := toStorePrincipal(p)
		opts := store.MemoryListOptions{
			OwnerAgentID: p.ID, State: store.MemoryStateActive,
			OrderBySalience: true, IncludeSensitive: true, Limit: limit,
		}
		page, err := st.ListMemories(ctx, principal, opts)
		if err != nil {
			return nil, 0, mapMemoryError(err)
		}
		total := len(page.Memories)
		if includeCount {
			total, err = st.CountMemories(ctx, principal, opts)
			if err != nil {
				return nil, 0, mapMemoryError(err)
			}
		} else if page.NextCursor != "" {
			// The exact total is intentionally skipped on prompt hooks; one
			// larger-than-page hint is enough for the handler to set elided.
			total++
		}
		out := make([]server.SelfMemory, len(page.Memories))
		for i, memory := range page.Memories {
			out[i] = toServerSelfMemory(memory)
		}
		return out, total, nil
	}
	cfg.CountSelfMemories = func(ctx context.Context, p server.DomainPrincipal) (int, error) {
		count, err := st.CountMemories(ctx, toStorePrincipal(p), store.MemoryListOptions{
			OwnerAgentID: p.ID, State: store.MemoryStateActive,
		})
		if err != nil {
			return 0, mapMemoryError(err)
		}
		return count, nil
	}
}

func toServerSelfMemory(memory store.Memory) server.SelfMemory {
	snippet := ""
	redacted := memory.Redacted
	if memory.ContentEncoding == "" || memory.ContentEncoding == "plain" {
		snippet = memorySnippet(memory.Content)
	} else {
		redacted = true
	}
	return server.SelfMemory{
		ID: memory.ID, Snippet: snippet, ContentEncoding: memory.ContentEncoding, Kind: memory.Kind,
		Tags: memory.Tags, Salience: memory.Salience, Sensitive: memory.Sensitive,
		Redacted: redacted, Source: memory.Origin,
	}
}

func toServerMemoryVectorProfile(in store.MemoryVectorProfile) server.MemoryVectorProfile {
	return server.MemoryVectorProfile{ID: in.ID, Provider: in.Provider, Model: in.Model,
		Recipe: in.Recipe, RecipeVersion: in.RecipeVersion, Dimensions: in.Dimensions,
		DistanceMetric: in.DistanceMetric, Normalization: in.Normalization,
		ContractHash: in.ContractHash, CreatedAt: in.CreatedAt}
}

type storeMemoryLifecycleFunc func(context.Context, store.Principal, string, store.MemoryLifecycleInput) (store.MemoryMutationResult, error)

func memoryLifecycleAdapter(mutate storeMemoryLifecycleFunc) func(context.Context, server.DomainPrincipal, string, server.MemoryLifecycleRequest) (server.MemoryMutationResult, error) {
	return func(ctx context.Context, p server.DomainPrincipal, memoryID string, in server.MemoryLifecycleRequest) (server.MemoryMutationResult, error) {
		result, err := mutate(ctx, toStorePrincipal(p), memoryID, store.MemoryLifecycleInput{
			ExpectedVersion:                 in.ExpectedVersion,
			ExpectedSupersessionSetRevision: in.ExpectedSupersessionSetRevision,
			Reason:                          in.Reason, Client: toStoreMemoryClient(in.Client),
			IdempotencyKey: in.IdempotencyKey,
		})
		if err != nil {
			return server.MemoryMutationResult{}, mapMemoryError(err)
		}
		return toServerMemoryMutationResult(result, p), nil
	}
}

func mapMemoryError(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, store.ErrMemoryInputInvalid):
		return fmt.Errorf("%w: %v", server.ErrBadInput, err)
	case errors.Is(err, store.ErrMemoryNotFound):
		return server.ErrNotFound
	case errors.Is(err, store.ErrMemoryForbidden), errors.Is(err, store.ErrAccountNotActive), errors.Is(err, store.ErrAgentNotFound):
		return server.ErrForbidden
	case errors.Is(err, store.ErrMemoryIdempotencyConflict):
		return server.ErrIdempotencyConflict
	case errors.Is(err, store.ErrMemoryDeleted):
		return server.ErrMemoryDeleted
	case errors.Is(err, store.ErrMemoryDependency):
		return server.ErrMemoryDependency
	case errors.Is(err, store.ErrMemoryConflict), errors.Is(err, store.ErrMemoryEvidenceConflict):
		return server.ErrConflict
	default:
		return err
	}
}

func toStoreMemoryClient(in server.MemoryClientProvenance) store.MemoryClientProvenance {
	return store.MemoryClientProvenance{
		Runtime: in.Runtime, Model: in.Model, Recipe: in.Recipe,
		RecipeVersion: in.RecipeVersion,
	}
}

func toStoreSupersedeMemoryInput(memoryID string, in server.SupersedeMemoryRequest) store.SupersedeMemoryInput {
	replacements := make([]store.CaptureMemoryInput, len(in.Replacements))
	for i, replacement := range in.Replacements {
		replacements[i] = store.CaptureMemoryInput{
			Content: replacement.Content, ContentEncoding: replacement.ContentEncoding,
			Kind: replacement.Kind,
			Tags: replacement.Tags, Links: replacement.Links,
			Salience: replacement.Salience, Sensitive: replacement.Sensitive,
			OccurredFrom: replacement.OccurredFrom, OccurredUntil: replacement.OccurredUntil,
			Origin: "agent", CaptureReason: replacement.CaptureReason,
			Evidence:       toStoreMemoryEvidenceInputs(replacement.Evidence),
			Client:         toStoreMemoryClient(replacement.Client),
			IdempotencyKey: replacement.IdempotencyKey,
		}
	}
	return store.SupersedeMemoryInput{
		MemoryID: memoryID, ExpectedVersion: in.ExpectedVersion,
		Replacements: replacements, Reason: in.Reason,
		Client: toStoreMemoryClient(in.Client), IdempotencyKey: in.IdempotencyKey,
	}
}

func toServerMemoryClient(in store.MemoryClientProvenance) server.MemoryClientProvenance {
	return server.MemoryClientProvenance{
		Runtime: in.Runtime, Model: in.Model, Recipe: in.Recipe,
		RecipeVersion: in.RecipeVersion,
	}
}

func toStoreMemoryEvidenceInputs(inputs []server.MemoryEvidenceInput) []store.MemoryEvidenceInput {
	out := make([]store.MemoryEvidenceInput, len(inputs))
	for i, in := range inputs {
		out[i] = store.MemoryEvidenceInput{
			Type: in.Type, Role: in.Role, ResolutionState: in.State,
			ExternalLocator:     in.ExternalLocator,
			ResolvedKind:        memoryEvidenceResolvedKind(in),
			SourceTranscriptID:  in.TranscriptID,
			SourceMemoryID:      in.SourceMemoryID,
			SourceMessageID:     in.MessageID,
			SourceImportLocator: in.ImportArtifactID,
			TerminalReasonCode:  in.UnavailableReason,
			SourceDigest:        in.SourceDigest,
		}
		if in.EntryFromSequence != nil {
			out[i].SourceSequenceFrom = *in.EntryFromSequence
		}
		if in.EntryUntilSequence != nil {
			out[i].SourceSequenceUntil = *in.EntryUntilSequence
		}
		if in.SourceMemoryVersion != nil {
			out[i].SourceMemoryVersion = *in.SourceMemoryVersion
		}
	}
	return out
}

func memoryEvidenceResolvedKind(in server.MemoryEvidenceInput) string {
	switch {
	case in.TranscriptID != "":
		return "transcript"
	case in.SourceMemoryID != "":
		return "memory"
	case in.MessageID != "":
		return "message"
	case in.ImportArtifactID != "":
		return "import_artifact"
	default:
		return ""
	}
}

func resolvedMemoryEvidenceKind(in server.ResolveMemoryEvidenceRequest) string {
	switch {
	case in.TranscriptID != "":
		return "transcript"
	case in.SourceMemoryID != "":
		return "memory"
	case in.MessageID != "":
		return "message"
	case in.ImportArtifactID != "":
		return "import_artifact"
	default:
		return ""
	}
}

func toServerMemoryMutationResult(in store.MemoryMutationResult, p server.DomainPrincipal) server.MemoryMutationResult {
	return server.MemoryMutationResult{
		Memory: toServerMemory(in.Memory, p),
		Receipt: server.MemoryMutationReceipt{
			Operation:            memoryOperation(in.Receipt.Operation),
			Actor:                server.MemoryActor{Kind: server.PrincipalKindAgent, ID: in.Receipt.ActorID, Name: principalAgentName(p, in.Receipt.ActorID)},
			IdempotencyKey:       in.Receipt.IdempotencyKey,
			CanonicalRequestHash: in.Receipt.RequestHash,
			MemoryID:             in.Receipt.MemoryID, Version: in.Receipt.Version,
			CreatedAt: in.Receipt.CreatedAt, Replayed: in.Receipt.Replayed,
		},
	}
}

func toServerSupersedeMemoryResult(in store.SupersedeMemoryResult, p server.DomainPrincipal) server.SupersedeMemoryResult {
	replacements := make([]server.Memory, len(in.Replacements))
	for i := range in.Replacements {
		replacements[i] = toServerMemory(in.Replacements[i], p)
	}
	replacementRefs := make([]server.MemoryVersionReference, len(in.Receipt.Replacements))
	for i, replacement := range in.Receipt.Replacements {
		replacementRefs[i] = server.MemoryVersionReference{
			MemoryID: replacement.MemoryID, Version: replacement.Version,
		}
	}
	return server.SupersedeMemoryResult{
		Source: toServerMemory(in.Source, p), Replacements: replacements,
		Receipt: server.MemorySupersessionReceipt{
			Operation: memoryOperation(in.Receipt.Operation),
			Actor: server.MemoryActor{
				Kind: server.PrincipalKindAgent, ID: in.Receipt.ActorID,
				Name: principalAgentName(p, in.Receipt.ActorID),
			},
			IdempotencyKey:          in.Receipt.IdempotencyKey,
			CanonicalRequestHash:    in.Receipt.RequestHash,
			SupersessionSetID:       in.Receipt.SupersessionSetID,
			SupersessionSetRevision: in.Receipt.SupersessionSetRevision,
			ReplacementCount:        in.Receipt.ReplacementCount,
			ReplacementDigest:       in.Receipt.ReplacementDigest,
			Source: server.MemoryVersionReference{
				MemoryID: in.Receipt.Source.MemoryID, Version: in.Receipt.Source.Version,
			},
			Replacements: replacementRefs, CreatedAt: in.Receipt.CreatedAt,
			Replayed: in.Receipt.Replayed,
		},
	}
}

func toServerMemory(in store.Memory, p server.DomainPrincipal) server.Memory {
	var previousVersion *int64
	if in.PreviousVersion > 0 {
		value := in.PreviousVersion
		previousVersion = &value
	}
	evidence := make([]server.MemoryEvidence, len(in.Evidence))
	for i := range in.Evidence {
		evidence[i] = toServerMemoryEvidence(in.Evidence[i], p)
	}
	return server.Memory{
		ID: in.ID, AccountID: in.AccountID, RealmID: in.RealmID,
		Owner:  server.MemoryOwner{Kind: in.OwnerKind, AgentID: in.OwnerID, AgentName: principalAgentName(p, in.OwnerID)},
		Origin: in.Origin, CaptureReason: in.CaptureReason,
		OriginalAuthor: server.MemoryActor{Kind: server.PrincipalKindAgent, ID: in.AuthoredByAgentID, Name: principalAgentName(p, in.AuthoredByAgentID)},
		Version:        in.Version, PreviousVersion: previousVersion, ChangeSeq: in.ChangeSeq,
		Content: in.Content, ContentEncoding: in.ContentEncoding,
		Kind: in.Kind, Tags: in.Tags, Salience: in.Salience,
		Links: in.Links, Sensitive: in.Sensitive, Redacted: in.Redacted,
		OccurredFrom: in.OccurredFrom, OccurredUntil: in.OccurredUntil,
		State: in.State, StateReason: in.LifecycleReason, PriorState: in.PriorState,
		ContentHash: in.ContentHash, Operation: memoryOperation(in.Operation),
		SupersessionSetID:             in.SupersessionSetID,
		SupersessionSetRevision:       in.SupersessionSetRevision,
		SupersessionReplacementCount:  in.SupersessionReplacementCount,
		SupersessionReplacementDigest: in.SupersessionReplacementDigest,
		ActiveSupersessionSetID:       in.ActiveSupersessionSetID,
		ActiveSupersessionSetRevision: in.ActiveSupersessionSetRevision,
		Actor:                         server.MemoryActor{Kind: in.ActorKind, ID: in.ActorID, Name: principalAgentName(p, in.ActorID)},
		Client:                        toServerMemoryClient(in.Client), Evidence: evidence,
		CreatedAt: in.CreatedAt, UpdatedAt: in.UpdatedAt,
	}
}

func toServerMemoryVersion(in store.MemoryVersion, p server.DomainPrincipal) server.MemoryVersion {
	memory := toServerMemory(in, p)
	return server.MemoryVersion{
		MemoryID: memory.ID, Version: memory.Version,
		PreviousVersion: memory.PreviousVersion, ChangeSeq: memory.ChangeSeq,
		Content: memory.Content, ContentEncoding: memory.ContentEncoding,
		Kind: memory.Kind, Tags: memory.Tags,
		Salience: memory.Salience, Links: memory.Links,
		Sensitive: memory.Sensitive, Redacted: memory.Redacted,
		OccurredFrom: memory.OccurredFrom, OccurredUntil: memory.OccurredUntil,
		State: memory.State, StateReason: memory.StateReason, PriorState: memory.PriorState,
		ContentHash: memory.ContentHash, Operation: memory.Operation,
		SupersessionSetID:             memory.SupersessionSetID,
		SupersessionSetRevision:       memory.SupersessionSetRevision,
		SupersessionReplacementCount:  memory.SupersessionReplacementCount,
		SupersessionReplacementDigest: memory.SupersessionReplacementDigest,
		ActiveSupersessionSetID:       memory.ActiveSupersessionSetID,
		ActiveSupersessionSetRevision: memory.ActiveSupersessionSetRevision,
		Actor:                         memory.Actor, Client: memory.Client, Evidence: memory.Evidence,
		CreatedAt: memory.CreatedAt,
	}
}

func toServerMemoryEvidence(in store.MemoryEvidence, p server.DomainPrincipal) server.MemoryEvidence {
	var from, until, sourceVersion *int64
	if in.SourceSequenceFrom > 0 {
		value := in.SourceSequenceFrom
		from = &value
	}
	if in.SourceSequenceUntil > 0 {
		value := in.SourceSequenceUntil
		until = &value
	}
	if in.SourceMemoryVersion > 0 {
		value := in.SourceMemoryVersion
		sourceVersion = &value
	}
	out := server.MemoryEvidence{
		ID: in.ID, MemoryID: in.MemoryID, MemoryVersion: in.TargetVersion,
		State: in.ResolutionState, Type: in.Type, Role: in.Role,
		PendingEvidenceID: in.PendingEvidenceID, ExternalLocator: in.ExternalLocator,
		TranscriptID: in.SourceTranscriptID, EntryFromSequence: from, EntryUntilSequence: until,
		SourceMemoryID: in.SourceMemoryID, SourceMemoryVersion: sourceVersion,
		MessageID: in.SourceMessageID, ImportArtifactID: in.SourceImportLocator,
		SourceDigest: in.SourceDigest, ChangeSeq: in.EvidenceChangeSeq,
		Actor:     server.MemoryActor{Kind: server.PrincipalKindAgent, ID: in.ActorID, Name: principalAgentName(p, in.ActorID)},
		CreatedAt: in.CreatedAt,
	}
	switch in.ResolutionState {
	case store.MemoryEvidenceUnavailable:
		out.UnavailableReason = in.TerminalReasonCode
	case store.MemoryEvidenceUnresolvable:
		out.UnresolvableReason = in.TerminalReasonCode
	}
	return out
}

func memoryOperation(operation string) string {
	switch operation {
	case "added":
		return "capture"
	case "adjusted":
		return "adjust"
	case "superseded":
		return "supersede"
	case "forgotten":
		return "forget"
	case "restored":
		return "restore"
	case "reactivated":
		return "reactivate"
	default:
		return operation
	}
}

func principalAgentName(p server.DomainPrincipal, id string) string {
	if id == p.ID {
		return p.AgentName
	}
	return ""
}

func memorySnippet(content string) string {
	content = strings.TrimSpace(content)
	if utf8.RuneCountInString(content) <= selfMemorySnippetRunes {
		return content
	}
	runes := []rune(content)
	return strings.TrimSpace(string(runes[:selfMemorySnippetRunes])) + "…"
}
