package main

import (
	"context"
	"errors"
	"fmt"

	"github.com/witwave-ai/witself/internal/server"
	"github.com/witwave-ai/witself/internal/store"
)

// configureMemoryCuration binds the HTTP work-queue contract to the store.
// The server performs no inference: plans and provenance come from the calling
// client, while the store owns authorization, leases, fences, and transactions.
func configureMemoryCuration(cfg *server.Config, st *store.Store) {
	cfg.RequestMemoryCuration = func(ctx context.Context, p server.DomainPrincipal, in server.RequestMemoryCurationRequest) (any, error) {
		result, err := st.RequestCuration(ctx, toStorePrincipal(p), store.RequestMemoryCurationInput{
			Scope: store.MemoryCurationScope{
				Sources: in.Scope.Sources, MemoryStates: in.Scope.MemoryStates,
				IncludeSensitive: in.Scope.IncludeSensitive,
				MaxMemories:      in.Scope.MaxMemories, MaxEvidence: in.Scope.MaxEvidence,
				MaxTranscriptEntries: in.Scope.MaxTranscriptEntries,
			},
			CoalescingKey: in.CoalescingKey, TriggerReason: in.TriggerReason,
			TriggerGeneration: in.TriggerGeneration, Priority: in.Priority,
			DueAt: in.DueAt, MaxAttempts: in.MaxAttempts, IdempotencyKey: in.IdempotencyKey,
		})
		return result, mapMemoryCurationError(err)
	}
	cfg.ListMemoryCurationRequests = func(ctx context.Context, p server.DomainPrincipal, opts server.MemoryCurationRequestListOptions) (any, error) {
		result, err := st.ListCurationRequests(ctx, toStorePrincipal(p), store.MemoryCurationRequestListOptions{
			State: opts.State, Limit: opts.Limit, Cursor: opts.Cursor,
			ExcludeSensitive: opts.ExcludeSensitive,
		})
		return result, mapMemoryCurationError(err)
	}
	cfg.GetMemoryCurationRequest = func(ctx context.Context, p server.DomainPrincipal, requestID string) (any, error) {
		result, err := st.GetCurationRequest(ctx, toStorePrincipal(p), requestID)
		return result, mapMemoryCurationError(err)
	}
	cfg.StartMemoryCuration = func(ctx context.Context, p server.DomainPrincipal, in server.StartMemoryCurationRequest) (any, error) {
		result, err := st.StartCuration(ctx, toStorePrincipal(p), store.StartMemoryCurationInput{
			RequestID: in.RequestID,
			Caps: store.MemoryCurationInputCaps{
				MaxMemories: in.Caps.MaxMemories, MaxEvidence: in.Caps.MaxEvidence,
				MaxTranscriptEntries: in.Caps.MaxTranscriptEntries,
			},
			LeaseDuration: in.LeaseDuration,
			Client: store.MemoryClientProvenance{
				Runtime: in.Client.Runtime, Model: in.Client.Model,
				Recipe: in.Client.Recipe, RecipeVersion: in.Client.RecipeVersion,
			},
			Budgets: in.Budgets, IdempotencyKey: in.IdempotencyKey,
		})
		return result, mapMemoryCurationError(err)
	}
	cfg.GetMemoryCurationRun = func(ctx context.Context, p server.DomainPrincipal, runID string) (any, error) {
		result, err := st.GetCurationRun(ctx, toStorePrincipal(p), runID)
		return result, mapMemoryCurationError(err)
	}
	cfg.GetMemoryCurationRunInputs = func(ctx context.Context, p server.DomainPrincipal, runID string, opts server.MemoryCurationRunInputOptions) (any, error) {
		result, err := st.GetCurationRunInputs(ctx, toStorePrincipal(p), runID,
			opts.FencingGeneration, opts.Cursor, opts.Limit)
		return result, mapMemoryCurationError(err)
	}
	cfg.RenewMemoryCuration = func(ctx context.Context, p server.DomainPrincipal, runID string, in server.RenewMemoryCurationRequest) (any, error) {
		result, err := st.RenewCuration(ctx, toStorePrincipal(p), runID, store.RenewMemoryCurationInput{
			FencingGeneration: in.FencingGeneration, Extension: in.Extension,
			IdempotencyKey: in.IdempotencyKey,
		})
		return result, mapMemoryCurationError(err)
	}
	cfg.PlanMemoryCuration = func(ctx context.Context, p server.DomainPrincipal, runID string, in server.PlanMemoryCurationRequest) (any, error) {
		result, err := st.PlanCuration(ctx, toStorePrincipal(p), runID, store.PlanMemoryCurationInput{
			FencingGeneration: in.FencingGeneration, Draft: in.Draft,
			IdempotencyKey: in.IdempotencyKey,
		})
		return result, mapMemoryCurationError(err)
	}
	cfg.ApplyMemoryCuration = func(ctx context.Context, p server.DomainPrincipal, runID string, in server.ApplyMemoryCurationRequest) (any, error) {
		result, err := st.ApplyCuration(ctx, toStorePrincipal(p), runID, store.ApplyMemoryCurationInput{
			FencingGeneration: in.FencingGeneration, PlanRevision: in.PlanRevision,
			PlanHash: in.PlanHash, IdempotencyKey: in.IdempotencyKey,
		})
		return result, mapMemoryCurationError(err)
	}
	cfg.CancelMemoryCuration = finishMemoryCurationAdapter(st.CancelCuration)
	cfg.AbandonMemoryCuration = finishMemoryCurationAdapter(st.AbandonCuration)
	cfg.RollbackMemoryCuration = func(ctx context.Context, p server.DomainPrincipal, runID string, in server.RollbackMemoryCurationRequest) (any, error) {
		heads := make([]store.MemoryVersionReference, len(in.ExpectedProducedHeads))
		for index := range in.ExpectedProducedHeads {
			heads[index] = store.MemoryVersionReference{
				MemoryID: in.ExpectedProducedHeads[index].MemoryID,
				Version:  in.ExpectedProducedHeads[index].Version,
			}
		}
		result, err := st.RollbackCuration(ctx, toStorePrincipal(p), runID, store.RollbackMemoryCurationInput{
			ApplyReceiptID: in.ApplyReceiptID, ExpectedProducedHeads: heads,
			Reason: in.Reason, IdempotencyKey: in.IdempotencyKey,
		})
		return result, mapMemoryCurationError(err)
	}
	cfg.GetMemoryCurationStatus = func(ctx context.Context, p server.DomainPrincipal, runID string) (any, error) {
		result, err := st.GetCurationStatus(ctx, toStorePrincipal(p), runID)
		return result, mapMemoryCurationError(err)
	}
}

type storeFinishMemoryCurationFunc func(
	context.Context,
	store.Principal,
	string,
	store.FinishMemoryCurationInput,
) (store.FinishMemoryCurationResult, error)

func finishMemoryCurationAdapter(finish storeFinishMemoryCurationFunc) func(
	context.Context,
	server.DomainPrincipal,
	string,
	server.FinishMemoryCurationRequest,
) (any, error) {
	return func(ctx context.Context, p server.DomainPrincipal, runID string, in server.FinishMemoryCurationRequest) (any, error) {
		result, err := finish(ctx, toStorePrincipal(p), runID, store.FinishMemoryCurationInput{
			FencingGeneration: in.FencingGeneration, Reason: in.Reason,
			IdempotencyKey: in.IdempotencyKey,
		})
		return result, mapMemoryCurationError(err)
	}
}

func mapMemoryCurationError(err error) error {
	if err == nil {
		return nil
	}
	var blocked *store.MemoryCurationRollbackBlockedError
	if errors.As(err, &blocked) {
		out := &server.MemoryCurationRollbackBlockedError{
			Blockers: make([]server.MemoryCurationRollbackBlocker, len(blocked.Blockers)),
		}
		for index := range blocked.Blockers {
			out.Blockers[index] = server.MemoryCurationRollbackBlocker{
				Kind: blocked.Blockers[index].Kind, MemoryID: blocked.Blockers[index].MemoryID,
				Version: blocked.Blockers[index].Version, ResourceID: blocked.Blockers[index].ResourceID,
			}
		}
		return out
	}
	switch {
	case errors.Is(err, store.ErrMemoryCurationInputInvalid):
		return fmt.Errorf("%w: %v", server.ErrBadInput, err)
	case errors.Is(err, store.ErrMemoryCurationNotFound):
		return server.ErrNotFound
	case errors.Is(err, store.ErrMemoryCurationForbidden),
		errors.Is(err, store.ErrAccountNotActive),
		errors.Is(err, store.ErrAgentNotFound):
		return server.ErrForbidden
	case errors.Is(err, store.ErrMemoryCurationIdempotencyConflict):
		return server.ErrIdempotencyConflict
	case errors.Is(err, store.ErrMemoryCurationBusy):
		return server.ErrMemoryCurationBusy
	case errors.Is(err, store.ErrMemoryCurationNotDue):
		return server.ErrMemoryCurationNotDue
	case errors.Is(err, store.ErrMemoryCurationLeaseExpired):
		return server.ErrMemoryCurationLeaseExpired
	case errors.Is(err, store.ErrMemoryCurationFenceMismatch):
		return server.ErrMemoryCurationFenceMismatch
	case errors.Is(err, store.ErrMemoryCurationConflict):
		return server.ErrConflict
	default:
		return err
	}
}
