package lifecycle

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/witwave-ai/witself/internal/billing"
	"github.com/witwave-ai/witself/internal/billing/fake"
	"github.com/witwave-ai/witself/internal/blob"
	"github.com/witwave-ai/witself/internal/blob/blobtest"
	"github.com/witwave-ai/witself/internal/plans"
)

func newR2Store(t *testing.T) *R2Store {
	t.Helper()
	srv := blobtest.New(t)
	c, err := blob.New(blob.Config{
		Endpoint: srv.URL, Bucket: "cp",
		AccessKey: "AKTEST", SecretKey: "secret",
	})
	if err != nil {
		t.Fatalf("blob.New: %v", err)
	}
	return NewR2Store(c, "registry/")
}

// TestR2StoreContract runs the Store contract the Manager depends on — the
// same semantics MemStore provides, enforced by conditional writes instead of
// a mutex: create-only Version 0, CAS on Version, provider-scoped customer
// lookup, dangling-index tolerance, and List.
func TestR2StoreContract(t *testing.T) {
	s := newR2Store(t)
	ctx := context.Background()

	// Missing account reads as absent.
	if _, ok, err := s.Get(ctx, "acct_1"); ok || err != nil {
		t.Fatalf("Get missing = %v, %v; want absent", ok, err)
	}

	// Version 0 creates; a second create loses.
	r := Record{
		AccountID: "acct_1", Email: "s@example.com",
		Entitled: plans.Free, Applied: plans.Free,
		PlanOverride: &AccountPlanOverride{
			Plan: "enterprise", ActorID: "adm_abcdefghijklmnopqrst",
			ActorHandle: "scott", Reason: "founder account", SetAt: time.Now(),
		},
		AdminHistory: []AdminChange{{
			Kind: "plan_override_set", ActorID: "adm_abcdefghijklmnopqrst",
			ActorHandle: "scott", Reason: "founder account", At: time.Now(),
			PlanFrom: plans.Free, PlanTo: "enterprise",
		}},
	}
	if err := s.Put(ctx, r); err != nil {
		t.Fatalf("create Put: %v", err)
	}
	if err := s.Put(ctx, r); !errors.Is(err, ErrStale) {
		t.Fatalf("second create Put = %v; want ErrStale", err)
	}

	// Get returns Version 1; CAS with it succeeds; reusing it loses.
	got, ok, err := s.Get(ctx, "acct_1")
	if err != nil || !ok || got.Version != 1 {
		t.Fatalf("Get = %+v, %v, %v; want Version 1", got, ok, err)
	}
	if got.PlanOverride == nil ||
		got.PlanOverride.ActorID != "adm_abcdefghijklmnopqrst" ||
		got.PlanOverride.ActorHandle != "scott" ||
		len(got.AdminHistory) != 1 ||
		got.AdminHistory[0].ActorID != "adm_abcdefghijklmnopqrst" ||
		got.AdminHistory[0].ActorHandle != "scott" {
		t.Fatalf("immutable admin attribution did not round-trip: %+v", got)
	}
	got.Provider, got.CustomerID = "fake", "fake_cus_0001"
	if err := s.Put(ctx, got); err != nil {
		t.Fatalf("CAS Put: %v", err)
	}
	if err := s.Put(ctx, got); !errors.Is(err, ErrStale) {
		t.Fatalf("stale CAS Put = %v; want ErrStale", err)
	}

	// ByCustomer resolves through the index, scoped by provider.
	byCust, ok, err := s.ByCustomer(ctx, "fake", "fake_cus_0001")
	if err != nil || !ok || byCust.AccountID != "acct_1" {
		t.Fatalf("ByCustomer = %+v, %v, %v; want acct_1", byCust, ok, err)
	}
	if _, ok, _ := s.ByCustomer(ctx, "stripe", "fake_cus_0001"); ok {
		t.Fatal("ByCustomer must be provider-scoped: a stripe lookup found a fake-pinned record")
	}

	// List sees every record.
	r2 := Record{AccountID: "acct_2", Entitled: plans.Free, Applied: plans.Free}
	if err := s.Put(ctx, r2); err != nil {
		t.Fatalf("Put acct_2: %v", err)
	}
	all, err := s.List(ctx)
	if err != nil || len(all) != 2 {
		t.Fatalf("List = %d records, %v; want 2", len(all), err)
	}
}

// TestR2StoreDanglingIndex: the index is written before the record, so a
// crash between the writes leaves an index pointing at a record that does not
// (yet) carry the pair. Such an index must read as not-found, not misroute.
func TestR2StoreDanglingIndex(t *testing.T) {
	s := newR2Store(t)
	ctx := context.Background()

	// A record exists WITHOUT billing objects...
	if err := s.Put(ctx, Record{AccountID: "acct_1", Entitled: plans.Free, Applied: plans.Free}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	// ...and a dangling index points at it (simulated crash after index write).
	if _, err := s.c.Put(ctx, "registry/customers/fake/fake_cus_0009", []byte("acct_1"), blob.Cond{}); err != nil {
		t.Fatalf("plant dangling index: %v", err)
	}
	if _, ok, err := s.ByCustomer(ctx, "fake", "fake_cus_0009"); ok || err != nil {
		t.Fatalf("ByCustomer via dangling index = %v, %v; want not-found", ok, err)
	}
}

// TestManagerOnR2Store proves the swap the interface promises: the SAME
// hardened state machine runs on the no-database store — including the
// cancel-vs-activation race, where the conditional write (not a mutex) is
// what saves the paid entitlement.
func TestManagerOnR2Store(t *testing.T) {
	catalog, err := plans.Load()
	if err != nil {
		t.Fatalf("plans.Load: %v", err)
	}
	ck := &clock{t: time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC)}
	f := fake.New(fake.Config{Prices: catalog.Prices(), Interactive: true, Now: ck.now})
	store := newR2Store(t)
	hooked := &hookStore{Store: store}
	ap := &recApplier{}
	m, err := NewManager(Config{
		Catalog: catalog, Providers: map[string]billing.Provider{"fake": f}, Default: "fake",
		Store: hooked, Applier: ap, Now: ck.now,
	})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	ctx := context.Background()

	if _, err := m.RequestUpgrade(ctx, "acct_1", "s@example.com", "standard"); err != nil {
		t.Fatalf("RequestUpgrade: %v", err)
	}
	r, _, err := store.Get(ctx, "acct_1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}

	// The reviewer's race, replayed on R2: checkout completes inside the
	// disarm window. The re-read (backed by R2's conditional writes) must see
	// the activation and refuse instead of clobbering.
	hookedP := &hookProvider{Provider: f, beforeCancel: func() {
		ck.t = ck.t.Add(time.Minute)
		events, err := f.Complete(r.CustomerID)
		if err != nil {
			t.Errorf("Complete: %v", err)
		}
		if err := m.OnEvents(ctx, "fake", events); err != nil {
			t.Errorf("OnEvents: %v", err)
		}
	}}
	m2, err := NewManager(Config{
		Catalog: catalog, Providers: map[string]billing.Provider{"fake": hookedP}, Default: "fake",
		Store: hooked, Applier: ap, Now: ck.now,
	})
	if err != nil {
		t.Fatalf("NewManager m2: %v", err)
	}
	err = m2.CancelPending(ctx, "acct_1")
	if err == nil || !strings.Contains(err.Error(), "nothing is pending") {
		t.Fatalf("CancelPending racing activation = %v; want 'nothing is pending'", err)
	}
	final, _, _ := store.Get(ctx, "acct_1")
	if final.Entitled != "standard" || final.Applied != "standard" {
		t.Fatalf("record = %+v; the paid entitlement must survive the racing cancel on R2", final)
	}
}

// TestR2StoreLive runs the contract against a REAL bucket when credentials
// are provided (WITSELF_TEST_R2_{ENDPOINT,BUCKET,ACCESS_KEY,SECRET_KEY}) —
// this is what verifies R2's S3-compatible API honors If-Match from Go.
// Skipped otherwise.
func TestR2StoreLive(t *testing.T) {
	endpoint := os.Getenv("WITSELF_TEST_R2_ENDPOINT")
	bucket := os.Getenv("WITSELF_TEST_R2_BUCKET")
	access := os.Getenv("WITSELF_TEST_R2_ACCESS_KEY")
	secret := os.Getenv("WITSELF_TEST_R2_SECRET_KEY")
	if endpoint == "" || bucket == "" || access == "" || secret == "" {
		t.Skip("set WITSELF_TEST_R2_* to run against real R2")
	}
	c, err := blob.New(blob.Config{Endpoint: endpoint, Bucket: bucket, AccessKey: access, SecretKey: secret})
	if err != nil {
		t.Fatalf("blob.New: %v", err)
	}
	s := NewR2Store(c, "test-registry/")
	ctx := context.Background()
	id := "acct_live_" + time.Now().UTC().Format("20060102T150405Z")

	r := Record{AccountID: id, Entitled: plans.Free, Applied: plans.Free}
	if err := s.Put(ctx, r); err != nil {
		t.Fatalf("create on real R2: %v", err)
	}
	got, ok, err := s.Get(ctx, id)
	if err != nil || !ok || got.Version != 1 {
		t.Fatalf("Get on real R2 = %+v, %v, %v", got, ok, err)
	}
	if err := s.Put(ctx, got); err != nil {
		t.Fatalf("CAS on real R2: %v", err)
	}
	// The load-bearing assertion: real R2 must 412 the stale writer.
	if err := s.Put(ctx, got); !errors.Is(err, ErrStale) {
		t.Fatalf("stale CAS on real R2 = %v; want ErrStale — If-Match must be honored", err)
	}
}
