package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/witwave-ai/witself/internal/plans"
)

type providerlessApplier struct {
	requests []ApplyRequest
	err      error
}

func (a *providerlessApplier) Apply(
	_ context.Context,
	_ string,
	request ApplyRequest,
) (ApplyAck, error) {
	a.requests = append(a.requests, request)
	if a.err != nil {
		return ApplyAck{}, a.err
	}
	return ApplyAck{Revision: request.Revision, Hash: request.Hash}, nil
}

type recoveryApplier struct {
	fence    ApplyFence
	requests []ApplyRequest
}

func (a *recoveryApplier) ReadApplyFence(
	context.Context,
	string,
) (ApplyFence, error) {
	return a.fence, nil
}

func (a *recoveryApplier) Apply(
	_ context.Context,
	_ string,
	request ApplyRequest,
) (ApplyAck, error) {
	if request.Revision <= a.fence.Revision {
		return ApplyAck{}, fmt.Errorf("stale revision %d <= %d", request.Revision, a.fence.Revision)
	}
	a.requests = append(a.requests, request)
	a.fence = ApplyFence{Revision: request.Revision, Hash: request.Hash}
	return ApplyAck{Revision: request.Revision, Hash: request.Hash}, nil
}

func TestProviderlessManagerSeedsAndAppliesPersonalWithoutBilling(t *testing.T) {
	catalog, err := plans.Load()
	if err != nil {
		t.Fatal(err)
	}
	store := NewMemStore()
	applier := &providerlessApplier{}
	manager, err := NewManager(Config{
		Catalog: catalog,
		Store:   store,
		Applier: applier,
	})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	if manager.BillingAvailable() {
		t.Fatal("providerless manager reported billing available")
	}

	created, pending, err := manager.EnsureAccount(context.Background(), "acct_manual")
	if err != nil || !created || pending {
		t.Fatalf("EnsureAccount = created:%t pending:%t err:%v", created, pending, err)
	}
	rec, ok, err := store.Get(context.Background(), "acct_manual")
	if err != nil || !ok {
		t.Fatalf("stored record = %+v, %t, %v", rec, ok, err)
	}
	if rec.Entitled != plans.Free || rec.Provider != "" || rec.CustomerID != "" {
		t.Fatalf("manual seed fabricated billing state: %+v", rec)
	}
	if len(applier.requests) != 1 ||
		applier.requests[0].Plan != plans.Free ||
		applier.requests[0].Policies[plans.TranscriptRetentionDaysPolicy] != 30 {
		t.Fatalf("applied baseline = %+v", applier.requests)
	}

	created, pending, err = manager.EnsureAccount(context.Background(), "acct_manual")
	if err != nil || created || pending {
		t.Fatalf("second EnsureAccount = created:%t pending:%t err:%v", created, pending, err)
	}
	if len(applier.requests) != 1 {
		t.Fatalf("already-acked snapshot was reapplied: %d requests", len(applier.requests))
	}
}

func TestProviderlessManagerRefusesCustomerBillingMutations(t *testing.T) {
	catalog, err := plans.Load()
	if err != nil {
		t.Fatal(err)
	}
	manager, err := NewManager(Config{
		Catalog: catalog,
		Store:   NewMemStore(),
		Applier: &providerlessApplier{},
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := manager.RequestUpgrade(ctx, "acct_manual", "", "standard"); !errors.Is(err, ErrBillingUnavailable) {
		t.Fatalf("upgrade error = %v; want ErrBillingUnavailable", err)
	}
	if _, err := manager.RequestDowngrade(ctx, "acct_manual", "", plans.Free); !errors.Is(err, ErrBillingUnavailable) {
		t.Fatalf("downgrade error = %v; want ErrBillingUnavailable", err)
	}
	if err := manager.CancelPending(ctx, "acct_manual"); !errors.Is(err, ErrBillingUnavailable) {
		t.Fatalf("cancel error = %v; want ErrBillingUnavailable", err)
	}
	if _, ok, err := manager.cfg.Store.Get(ctx, "acct_manual"); err != nil || ok {
		t.Fatalf("billing refusal created state: ok=%t err=%v", ok, err)
	}
}

func TestMissingLifecycleRecordAdvancesAboveCellFenceAndReappliesAuthority(t *testing.T) {
	catalog, err := plans.Load()
	if err != nil {
		t.Fatal(err)
	}
	// Simulate an R2 restore that lost the lifecycle record while the cell
	// retained a newer, different snapshot. The cell payload is intentionally
	// unavailable to Manager; only its fence crosses this seam.
	applier := &recoveryApplier{fence: ApplyFence{
		Revision: 41,
		Hash:     strings.Repeat("b", 64),
	}}
	store := NewMemStore()
	manager, err := NewManager(Config{
		Catalog: catalog,
		Store:   store,
		Applier: applier,
	})
	if err != nil {
		t.Fatal(err)
	}
	created, pending, err := manager.EnsureAccount(context.Background(), "acct_restored")
	if err != nil || !created || pending {
		t.Fatalf("EnsureAccount = created:%t pending:%t err:%v", created, pending, err)
	}
	if len(applier.requests) != 1 {
		t.Fatalf("apply requests = %d; want one", len(applier.requests))
	}
	request := applier.requests[0]
	if request.Revision != 42 {
		t.Fatalf("recovery revision = %d; want 42", request.Revision)
	}
	if request.Plan != plans.Free ||
		request.Policies[plans.TranscriptRetentionDaysPolicy] != 30 {
		t.Fatalf("cell payload displaced CP authority: %+v", request.PlanSnapshot)
	}
	rec, _, err := manager.ResolvedStatus(context.Background(), "acct_restored", "")
	if err != nil {
		t.Fatal(err)
	}
	if rec.AppliedSnapshotRevision != 42 ||
		rec.AppliedSnapshotHash != request.Hash {
		t.Fatalf("stored acknowledgement = %+v", rec)
	}
}

func TestProviderlessReconcileKeepsExpiredPinnedUpgradePending(t *testing.T) {
	catalog, err := plans.Load()
	if err != nil {
		t.Fatal(err)
	}
	store := NewMemStore()
	now := time.Date(2026, 7, 23, 12, 0, 0, 0, time.UTC)
	requested := now.Add(-2 * time.Hour)
	record := Record{
		AccountID:  "acct_pinned",
		Provider:   "stripe",
		CustomerID: "cus_existing",
		Entitled:   plans.Free,
		Applied:    plans.Free,
		Pending: &Pending{
			Kind:      PendingUpgrade,
			Plan:      "standard",
			Requested: requested,
			Expires:   now.Add(-time.Hour),
		},
	}
	if err := store.Put(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	manager, err := NewManager(Config{
		Catalog: catalog,
		Store:   store,
		Applier: &providerlessApplier{},
		Now:     func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.Reconcile(context.Background()); !errors.Is(err, ErrBillingUnavailable) {
		t.Fatalf("Reconcile error = %v; want ErrBillingUnavailable", err)
	}
	got, ok, err := store.Get(context.Background(), "acct_pinned")
	if err != nil || !ok {
		t.Fatalf("Get = %+v %t %v", got, ok, err)
	}
	if got.Pending == nil ||
		got.Pending.Kind != PendingUpgrade ||
		!got.Pending.Requested.Equal(requested) {
		t.Fatalf("providerless reconcile cleared retryable provider state: %+v", got.Pending)
	}
}

func TestReconcileReturnsApplyFailure(t *testing.T) {
	catalog, err := plans.Load()
	if err != nil {
		t.Fatal(err)
	}
	store := NewMemStore()
	if err := store.Put(context.Background(), Record{
		AccountID: "acct_apply_error",
		Entitled:  plans.Free,
		Applied:   plans.Free,
	}); err != nil {
		t.Fatal(err)
	}
	sentinel := errors.New("cell unavailable")
	manager, err := NewManager(Config{
		Catalog: catalog,
		Store:   store,
		Applier: &providerlessApplier{err: sentinel},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.Reconcile(context.Background()); !errors.Is(err, sentinel) {
		t.Fatalf("Reconcile error = %v; want apply failure", err)
	}
}
