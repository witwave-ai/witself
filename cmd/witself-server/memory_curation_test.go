package main

import (
	"errors"
	"fmt"
	"testing"

	"github.com/witwave-ai/witself/internal/server"
	"github.com/witwave-ai/witself/internal/store"
)

func TestConfigureMemoryCurationWiresCompleteSurface(t *testing.T) {
	var cfg server.Config
	configureMemoryCuration(&cfg, nil)
	if cfg.RequestMemoryCuration == nil || cfg.ListMemoryCurationRequests == nil ||
		cfg.GetMemoryCurationRequest == nil || cfg.StartMemoryCuration == nil ||
		cfg.GetMemoryCurationRun == nil || cfg.GetMemoryCurationRunInputs == nil ||
		cfg.RenewMemoryCuration == nil || cfg.PlanMemoryCuration == nil ||
		cfg.ApplyMemoryCuration == nil || cfg.CancelMemoryCuration == nil ||
		cfg.AbandonMemoryCuration == nil || cfg.RollbackMemoryCuration == nil ||
		cfg.GetMemoryCurationStatus == nil {
		t.Fatal("configureMemoryCuration left part of the public surface unwired")
	}
}

func TestMapMemoryCurationErrorPreservesExactConditions(t *testing.T) {
	tests := []struct {
		storeErr  error
		serverErr error
	}{
		{store.ErrMemoryCurationInputInvalid, server.ErrBadInput},
		{store.ErrMemoryCurationNotFound, server.ErrNotFound},
		{store.ErrMemoryCurationForbidden, server.ErrForbidden},
		{store.ErrMemoryCurationIdempotencyConflict, server.ErrIdempotencyConflict},
		{store.ErrMemoryCurationBusy, server.ErrMemoryCurationBusy},
		{store.ErrMemoryCurationNotDue, server.ErrMemoryCurationNotDue},
		{store.ErrMemoryCurationLeaseExpired, server.ErrMemoryCurationLeaseExpired},
		{store.ErrMemoryCurationFenceMismatch, server.ErrMemoryCurationFenceMismatch},
		{store.ErrMemoryCurationConflict, server.ErrConflict},
	}
	for _, test := range tests {
		mapped := mapMemoryCurationError(fmt.Errorf("wrapped: %w", test.storeErr))
		if !errors.Is(mapped, test.serverErr) {
			t.Errorf("mapMemoryCurationError(%v) = %v, want errors.Is(_, %v)", test.storeErr, mapped, test.serverErr)
		}
	}

	blocked := &store.MemoryCurationRollbackBlockedError{Blockers: []store.MemoryCurationRollbackBlocker{{
		Kind: "dependent_memory", MemoryID: "mem_1", Version: 2,
	}}}
	mapped := mapMemoryCurationError(blocked)
	var serverBlocked *server.MemoryCurationRollbackBlockedError
	if !errors.As(mapped, &serverBlocked) || len(serverBlocked.Blockers) != 1 ||
		serverBlocked.Blockers[0].MemoryID != "mem_1" ||
		!errors.Is(serverBlocked, server.ErrConflict) {
		t.Fatalf("rollback blocker mapping = %#v", mapped)
	}
}
