package main

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/witwave-ai/witself/internal/server"
	"github.com/witwave-ai/witself/internal/store"
)

type selfMemoryCheckpointStoreStub struct {
	status      store.MemoryCurationStatus
	statusErr   error
	page        store.MemoryCurationRequestPage
	listErr     error
	statusCalls int
	listCalls   int
	principal   store.Principal
	listOptions store.MemoryCurationRequestListOptions
}

func (s *selfMemoryCheckpointStoreStub) GetCurationStatus(_ context.Context, p store.Principal, _ string) (store.MemoryCurationStatus, error) {
	s.statusCalls++
	s.principal = p
	return s.status, s.statusErr
}

func (s *selfMemoryCheckpointStoreStub) ListCurationRequests(_ context.Context, p store.Principal, opts store.MemoryCurationRequestListOptions) (store.MemoryCurationRequestPage, error) {
	s.listCalls++
	s.principal = p
	s.listOptions = opts
	return s.page, s.listErr
}

func TestConfigureMemoryCurationWiresCompleteSurface(t *testing.T) {
	var cfg server.Config
	configureMemoryCuration(&cfg, nil)
	if cfg.RequestMemoryCuration == nil || cfg.ListMemoryCurationRequests == nil ||
		cfg.GetMemoryCurationRequest == nil || cfg.StartMemoryCuration == nil ||
		cfg.GetMemoryCurationRun == nil || cfg.GetMemoryCurationRunInputs == nil ||
		cfg.GetMemoryCurationPlan == nil ||
		cfg.RenewMemoryCuration == nil || cfg.PlanMemoryCuration == nil ||
		cfg.ApplyMemoryCuration == nil || cfg.CancelMemoryCuration == nil ||
		cfg.AbandonMemoryCuration == nil || cfg.RollbackMemoryCuration == nil ||
		cfg.GetMemoryCurationStatus == nil || cfg.GetSelfMemoryCheckpoint == nil {
		t.Fatal("configureMemoryCuration left part of the public surface unwired")
	}
}

func TestProjectSelfMemoryCheckpointCoversIdleDueAndActive(t *testing.T) {
	principal := store.Principal{
		Kind: store.PrincipalAgent, ID: "agt_1", AccountID: "acc_1", RealmID: "rlm_1",
	}
	dueAt := time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)
	leaseExpiresAt := dueAt.Add(5 * time.Minute)
	tests := []struct {
		name      string
		stub      selfMemoryCheckpointStoreStub
		want      server.SelfMemoryCheckpoint
		wantLists int
	}{
		{
			name: "no lane is supported and idle",
			stub: selfMemoryCheckpointStoreStub{statusErr: store.ErrMemoryCurationNotFound},
			want: server.SelfMemoryCheckpoint{Pending: false}, wantLists: 1,
		},
		{
			name: "due request",
			stub: selfMemoryCheckpointStoreStub{
				page: store.MemoryCurationRequestPage{Requests: []store.MemoryCurationRequest{{
					ID: "mcrq_due", RequestGeneration: 7, DueAt: dueAt,
				}}},
			},
			want: server.SelfMemoryCheckpoint{
				Pending: true, RequestID: "mcrq_due", RequestGeneration: 7, DueAt: &dueAt,
			},
			wantLists: 1,
		},
		{
			name: "active run wins without queue read",
			stub: selfMemoryCheckpointStoreStub{status: store.MemoryCurationStatus{
				Run: &store.MemoryCurationRun{
					ID: "mrun_active", RequestID: "mcrq_active", RequestGeneration: 9,
					FencingGeneration: 4, State: store.MemoryCurationRunOpen,
					LeaseExpiresAt: &leaseExpiresAt,
				},
			}},
			want: server.SelfMemoryCheckpoint{
				Pending: true, RequestID: "mcrq_active", RequestGeneration: 9,
				RunID: "mrun_active", RunState: store.MemoryCurationRunOpen,
				FencingGeneration: 4, LeaseExpiresAt: &leaseExpiresAt,
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			stub := test.stub
			got, err := projectSelfMemoryCheckpoint(context.Background(), &stub, principal)
			if err != nil {
				t.Fatal(err)
			}
			if got == nil || got.Pending != test.want.Pending || got.RequestID != test.want.RequestID ||
				got.RequestGeneration != test.want.RequestGeneration || got.RunID != test.want.RunID ||
				got.RunState != test.want.RunState || got.FencingGeneration != test.want.FencingGeneration ||
				!equalOptionalTime(got.DueAt, test.want.DueAt) ||
				!equalOptionalTime(got.LeaseExpiresAt, test.want.LeaseExpiresAt) {
				t.Fatalf("checkpoint = %#v, want %#v", got, test.want)
			}
			if stub.statusCalls != 1 || stub.listCalls != test.wantLists || stub.principal != principal ||
				(stub.listCalls > 0 && stub.listOptions.Limit != 1) {
				t.Fatalf("store calls = status %d list %d principal %#v opts %#v",
					stub.statusCalls, stub.listCalls, stub.principal, stub.listOptions)
			}
		})
	}
	storeFailure := errors.New("checkpoint store unavailable")
	stub := &selfMemoryCheckpointStoreStub{statusErr: storeFailure}
	if got, err := projectSelfMemoryCheckpoint(context.Background(), stub, principal); got != nil || !errors.Is(err, storeFailure) || stub.listCalls != 0 {
		t.Fatalf("checkpoint store failure = %#v / %v / list calls %d", got, err, stub.listCalls)
	}
}

func equalOptionalTime(left, right *time.Time) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return left.Equal(*right)
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
