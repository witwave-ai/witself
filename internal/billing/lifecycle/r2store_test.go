package lifecycle

import (
	"context"
	"encoding/json"
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
	zero := int64(0)
	r := Record{
		AccountID: "acct_1", Email: "s@example.com",
		Entitled: plans.Free, Applied: plans.Free,
		PlanOverride: &AccountPlanOverride{
			Plan: "enterprise", ActorID: "adm_abcdefghijklmnopqrst",
			ActorHandle: "scott", Reason: "founder account", SetAt: time.Now(),
		},
		LimitOverrides: map[string]AccountLimitOverride{
			plans.StoredSecretLimit: {
				Max: nil, ActorID: "adm_abcdefghijklmnopqrst",
				ActorHandle: "scott", Reason: "founder unlimited", SetAt: time.Now(),
			},
			plans.AgentLimit: {
				Max: &zero, ActorID: "adm_abcdefghijklmnopqrst",
				ActorHandle: "scott", Reason: "paused", SetAt: time.Now(),
			},
		},
		AdminHistory: []AdminChange{
			{
				Kind: "plan_override_set", ActorID: "adm_abcdefghijklmnopqrst",
				ActorHandle: "scott", Reason: "founder account", At: time.Now(),
				PlanFrom: plans.Free, PlanTo: "enterprise",
			},
			{
				Kind: "limit_override_set", ActorID: "adm_abcdefghijklmnopqrst",
				ActorHandle: "scott", Reason: "founder unlimited", At: time.Now(),
				LimitDimension:  plans.StoredSecretLimit,
				LimitFrom:       &AccountLimitValue{Max: nil},
				LimitTo:         &AccountLimitValue{Max: nil},
				LimitFromSource: "inherited", LimitToSource: "override",
			},
		},
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
		len(got.AdminHistory) != 2 ||
		got.AdminHistory[0].ActorID != "adm_abcdefghijklmnopqrst" ||
		got.AdminHistory[0].ActorHandle != "scott" ||
		got.LimitOverrides[plans.StoredSecretLimit].Max != nil ||
		got.LimitOverrides[plans.AgentLimit].Max == nil ||
		*got.LimitOverrides[plans.AgentLimit].Max != 0 ||
		got.AdminHistory[1].LimitTo == nil ||
		got.AdminHistory[1].LimitTo.Max != nil ||
		got.AdminHistory[1].LimitToSource != "override" {
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

func TestLegacyR2RecordJSONWithoutLimitOverridesStillDecodes(t *testing.T) {
	// R2 records are direct Record JSON. Objects written before the generic
	// limit-override field existed simply omit it and must retain inherited
	// semantics without a data migration.
	var got Record
	if err := json.Unmarshal([]byte(`{
		"AccountID":"acct_legacy",
		"Entitled":"free",
		"Applied":"free",
		"Version":7,
		"AdminHistory":[]
	}`), &got); err != nil {
		t.Fatalf("decode legacy R2 JSON: %v", err)
	}
	if got.AccountID != "acct_legacy" || got.Version != 7 ||
		got.LimitOverrides != nil {
		t.Fatalf("legacy record = %+v; want nil inherited limit overrides", got)
	}
}

func TestR2LimitAuditSurvivesExactLegacyRoundTrip(t *testing.T) {
	// These local types are the pre-Phase-A wire knowledge: unknown
	// LimitOverrides and limit-specific AdminChange fields are deliberately
	// absent, while Kind and immutable attribution remain known.
	type legacyAdminChange struct {
		Kind          string    `json:"kind"`
		ActorID       string    `json:"actor_id"`
		ActorHandle   string    `json:"actor_handle"`
		Reason        string    `json:"reason"`
		At            time.Time `json:"at"`
		PlanFrom      string    `json:"plan_from,omitempty"`
		PlanTo        string    `json:"plan_to,omitempty"`
		RetentionFrom *int64    `json:"retention_from,omitempty"`
		RetentionTo   *int64    `json:"retention_to,omitempty"`
	}
	type legacyRecord struct {
		AccountID                   string
		Email                       string
		Provider                    string
		CustomerID                  string
		Entitled                    string
		Applied                     string
		EntitledAt                  time.Time
		Pending                     *Pending
		PlanOverride                *AccountPlanOverride
		TranscriptRetentionOverride *TranscriptRetentionOverride
		AdminHistory                []legacyAdminChange
		PastDueSince                *time.Time
		DunningAt                   time.Time
		ApplyBlocked                string
		DesiredSnapshotHash         string
		SnapshotRevision            int64
		AppliedSnapshotRevision     int64
		AppliedSnapshotHash         string
		Version                     int64
	}

	at := time.Date(2026, 7, 23, 18, 0, 0, 0, time.UTC)
	zero, one, two, twentyFive := int64(0), int64(1), int64(2), int64(25)
	record := Record{
		AccountID: "acct_founder", Email: "founder@example.com",
		Entitled: plans.Free, Applied: plans.Free, Version: 4,
		LimitOverrides: map[string]AccountLimitOverride{
			plans.StoredSecretLimit: {
				Max: nil, ActorID: "adm_founder",
				ActorHandle: "scott", Reason: "founder is unlimited", SetAt: at,
			},
			plans.AgentLimit: {
				Max: &zero, ActorID: "adm_capacity",
				ActorHandle: "ops", Reason: "pause agents", SetAt: at.Add(time.Minute),
			},
			// This stale current-map entry proves replayed clear history wins.
			plans.RealmLimit: {
				Max: &two, ActorID: "adm_capacity",
				ActorHandle: "ops", Reason: "temporary realms", SetAt: at.Add(2 * time.Minute),
			},
		},
		AdminHistory: []AdminChange{
			{
				Kind: "limit_override_set", ActorID: "adm_founder",
				ActorHandle: "scott", Reason: "founder is unlimited", At: at,
				LimitDimension:  plans.StoredSecretLimit,
				LimitFrom:       &AccountLimitValue{Max: nil},
				LimitTo:         &AccountLimitValue{Max: nil},
				LimitFromSource: "inherited", LimitToSource: "override",
			},
			{
				Kind: "limit_override_set", ActorID: "adm_capacity",
				ActorHandle: "ops", Reason: "pause agents", At: at.Add(time.Minute),
				LimitDimension:  plans.AgentLimit,
				LimitFrom:       &AccountLimitValue{Max: &twentyFive},
				LimitTo:         &AccountLimitValue{Max: &zero},
				LimitFromSource: "inherited", LimitToSource: "override",
			},
			{
				Kind: "limit_override_set", ActorID: "adm_capacity",
				ActorHandle: "ops", Reason: "temporary realms", At: at.Add(2 * time.Minute),
				LimitDimension:  plans.RealmLimit,
				LimitFrom:       &AccountLimitValue{Max: &one},
				LimitTo:         &AccountLimitValue{Max: &two},
				LimitFromSource: "inherited", LimitToSource: "override",
			},
			{
				Kind: "limit_override_cleared", ActorID: "adm_founder",
				ActorHandle: "scott", Reason: "restore realm inheritance", At: at.Add(3 * time.Minute),
				LimitDimension:  plans.RealmLimit,
				LimitFrom:       &AccountLimitValue{Max: &two},
				LimitTo:         &AccountLimitValue{Max: &one},
				LimitFromSource: "override", LimitToSource: "inherited",
			},
		},
	}

	encoded, err := marshalR2Record(record)
	if err != nil {
		t.Fatalf("new encode: %v", err)
	}
	for i, change := range record.AdminHistory {
		if change.Kind != []string{
			"limit_override_set", "limit_override_set",
			"limit_override_set", "limit_override_cleared",
		}[i] {
			t.Fatalf("marshal mutated in-memory API kind %d: %q", i, change.Kind)
		}
	}

	var legacy legacyRecord
	if err := json.Unmarshal(encoded, &legacy); err != nil {
		t.Fatalf("legacy unmarshal: %v", err)
	}
	if len(legacy.AdminHistory) != 4 {
		t.Fatalf("legacy history = %+v", legacy.AdminHistory)
	}
	for i, change := range legacy.AdminHistory {
		if !strings.HasPrefix(change.Kind, r2LimitAuditKindPrefix) {
			t.Fatalf("legacy change %d lost rollback envelope: %q", i, change.Kind)
		}
	}

	// Simulate an old binary making and persisting an unrelated record
	// mutation. Its marshal drops every Phase A field it never knew.
	legacy.Email = "legacy-mutated@example.com"
	legacy.Version++
	legacyEncoded, err := json.Marshal(legacy)
	if err != nil {
		t.Fatalf("legacy marshal: %v", err)
	}

	got, err := unmarshalR2Record(legacyEncoded)
	if err != nil {
		t.Fatalf("new decode after rollback: %v", err)
	}
	if got.Email != "legacy-mutated@example.com" || got.Version != 5 {
		t.Fatalf("legacy mutation did not survive: %+v", got)
	}
	founder, ok := got.LimitOverrides[plans.StoredSecretLimit]
	if !ok || founder.Max != nil ||
		founder.ActorID != "adm_founder" ||
		founder.ActorHandle != "scott" ||
		founder.Reason != "founder is unlimited" ||
		!founder.SetAt.Equal(at) {
		t.Fatalf("founder unlimited was not reconstructed: %+v, present=%v", founder, ok)
	}
	agents, ok := got.LimitOverrides[plans.AgentLimit]
	if !ok || agents.Max == nil || *agents.Max != 0 ||
		agents.ActorID != "adm_capacity" ||
		agents.ActorHandle != "ops" ||
		agents.Reason != "pause agents" ||
		!agents.SetAt.Equal(at.Add(time.Minute)) {
		t.Fatalf("finite zero was not reconstructed: %+v, present=%v", agents, ok)
	}
	if _, ok := got.LimitOverrides[plans.RealmLimit]; ok {
		t.Fatalf("replayed clear did not restore inheritance: %+v", got.LimitOverrides)
	}
	if len(got.AdminHistory) != 4 {
		t.Fatalf("restored audit length = %d", len(got.AdminHistory))
	}
	for i, wantKind := range []string{
		"limit_override_set", "limit_override_set",
		"limit_override_set", "limit_override_cleared",
	} {
		change := got.AdminHistory[i]
		if change.Kind != wantKind ||
			change.LimitDimension == "" ||
			change.LimitFrom == nil || change.LimitTo == nil ||
			change.LimitFromSource == "" || change.LimitToSource == "" {
			t.Fatalf("restored audit %d = %+v", i, change)
		}
	}
	if got.AdminHistory[0].LimitTo.Max != nil ||
		got.AdminHistory[1].LimitTo.Max == nil ||
		*got.AdminHistory[1].LimitTo.Max != 0 ||
		got.AdminHistory[3].LimitToSource != "inherited" {
		t.Fatalf("restored audit values = %+v", got.AdminHistory)
	}
}

func TestR2LimitAuditMalformedReservedKindFailsClosed(t *testing.T) {
	for _, kind := range []string{
		r2LimitAuditKindPrefix,
		r2LimitAuditKindPrefix + "not-base64!",
		r2LimitAuditKindPrefix + "e30", // {} lacks every required field.
	} {
		raw, err := json.Marshal(Record{
			AccountID: "acct_bad",
			AdminHistory: []AdminChange{{
				Kind: kind, ActorID: "adm_bad", ActorHandle: "bad",
				Reason: "bad", At: time.Now(),
			}},
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := unmarshalR2Record(raw); err == nil ||
			!strings.Contains(err.Error(), "malformed reserved kind") {
			t.Fatalf("unmarshal reserved kind %q = %v; want fail closed", kind, err)
		}
		if _, err := marshalR2Record(Record{
			AccountID: "acct_bad",
			AdminHistory: []AdminChange{{
				Kind: kind, ActorID: "adm_bad", ActorHandle: "bad",
				Reason: "bad", At: time.Now(),
			}},
		}); err == nil || !strings.Contains(err.Error(), "malformed reserved kind") {
			t.Fatalf("marshal reserved kind %q = %v; want fail closed", kind, err)
		}
	}

	s := newR2Store(t)
	ctx := t.Context()
	raw := []byte(`{
		"AccountID":"acct_bad",
		"AdminHistory":[{
			"kind":"witself.limit-override.v1:not-base64!",
			"actor_id":"adm_bad",
			"actor_handle":"bad",
			"reason":"bad",
			"at":"2026-07-23T18:00:00Z"
		}]
	}`)
	if _, err := s.c.Put(
		ctx, s.accountKey("acct_bad"), raw,
		blob.Cond{IfNoneMatchAny: true},
	); err != nil {
		t.Fatalf("seed malformed R2 object: %v", err)
	}
	if _, _, err := s.Get(ctx, "acct_bad"); err == nil {
		t.Fatal("R2 Get accepted malformed reserved Kind")
	}
	if _, err := s.List(ctx); err == nil {
		t.Fatal("R2 List accepted malformed reserved Kind")
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
