package lifecycle

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/witwave-ai/witself/internal/billing"
	"github.com/witwave-ai/witself/internal/billing/fake"
	"github.com/witwave-ai/witself/internal/plans"
)

type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }

type applyCall struct {
	accountID, plan string
	revision        int64
	hash            string
	limits          map[string]int64
	policies        map[string]int64
	features        []string
}

// recApplier records Apply calls and can simulate cell-push failures.
type recApplier struct {
	mu    sync.Mutex
	fail  bool
	calls []applyCall
}

func (a *recApplier) Apply(_ context.Context, accountID string, request ApplyRequest) (ApplyAck, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.fail {
		return ApplyAck{}, context.DeadlineExceeded
	}
	a.calls = append(a.calls, applyCall{
		accountID: accountID, plan: request.Plan, revision: request.Revision,
		hash: request.Hash, limits: request.Limits, policies: request.Policies,
		features: request.Features,
	})
	return ApplyAck{Revision: request.Revision, Hash: request.Hash}, nil
}

func (a *recApplier) last(t *testing.T) applyCall {
	t.Helper()
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.calls) == 0 {
		t.Fatal("no Apply calls recorded")
	}
	return a.calls[len(a.calls)-1]
}

type fitStub struct {
	mu         sync.Mutex
	violations []string
}

func (f *fitStub) set(v []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.violations = v
}

func (f *fitStub) Fit(context.Context, string, plans.Plan) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.violations, nil
}

// hookStore interposes on Put to deterministically interleave a competing
// writer inside another operation's read-decide-write window.
type hookStore struct {
	Store
	mu        sync.Mutex
	beforePut func(Record)
}

func (h *hookStore) Put(ctx context.Context, r Record) error {
	h.mu.Lock()
	f := h.beforePut
	h.beforePut = nil
	h.mu.Unlock()
	if f != nil {
		f(r)
	}
	return h.Store.Put(ctx, r)
}

type harness struct {
	m       *Manager
	fake    *fake.Fake
	store   *MemStore
	hooked  *hookStore
	applier *recApplier
	ck      *clock
	fit     *fitStub
}

func testAdminActor() AdminActor {
	return AdminActor{ID: "adm_abcdefghijklmnopqrst", Handle: "scott"}
}

func newHarness(t *testing.T, interactive bool) *harness {
	t.Helper()
	catalog, err := plans.Load()
	if err != nil {
		t.Fatalf("plans.Load: %v", err)
	}
	ck := &clock{t: time.Date(2026, 7, 4, 12, 0, 0, 0, time.UTC)}
	f := fake.New(fake.Config{Prices: catalog.Prices(), Interactive: interactive, Now: ck.now})
	st := NewMemStore()
	hooked := &hookStore{Store: st}
	ap := &recApplier{}
	fit := &fitStub{}
	m, err := NewManager(Config{
		Catalog: catalog, Providers: map[string]billing.Provider{"fake": f}, Default: "fake",
		Store: hooked, Applier: ap, Fit: fit, Now: ck.now,
	})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	return &harness{m: m, fake: f, store: st, hooked: hooked, applier: ap, ck: ck, fit: fit}
}

func (h *harness) record(t *testing.T, accountID string) Record {
	t.Helper()
	r, ok, err := h.store.Get(context.Background(), accountID)
	if err != nil || !ok {
		t.Fatalf("record %s: ok=%v err=%v", accountID, ok, err)
	}
	return r
}

func TestStatusIsReadOnly(t *testing.T) {
	h := newHarness(t, false)
	r, err := h.m.Status(context.Background(), "acct_1", "s@example.com")
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if r.Entitled != plans.Free || r.Applied != plans.Free || r.Pending != nil || r.CustomerID != "" {
		t.Fatalf("fresh record = %+v; want free/free, no pending, no customer", r)
	}
	// Probing an account id must not create phantom rows.
	if all, _ := h.store.List(context.Background()); len(all) != 0 {
		t.Fatalf("Status persisted %d record(s); reads must not write", len(all))
	}
}

func TestAccountLimitOverrideLifecycleAndAttribution(t *testing.T) {
	h := newHarness(t, false)
	ctx := context.Background()
	const accountID = "acct_limits"

	_, inherited, err := h.m.ResolvedStatus(ctx, accountID, "")
	if err != nil {
		t.Fatal(err)
	}
	if inherited.DefaultLimits[plans.AgentLimit] != 25 ||
		inherited.Limits[plans.AgentLimit] != 25 {
		t.Fatalf("inherited agent limit = defaults %v effective %v; want 25",
			inherited.DefaultLimits, inherited.Limits)
	}
	if got, ok := inherited.DefaultLimits[plans.StoredSecretLimit]; !ok || got != 0 {
		t.Fatalf("inherited stored-secret default = %v; want explicit zero",
			inherited.DefaultLimits)
	}
	if got, ok := inherited.Limits[plans.StoredSecretLimit]; !ok || got != 0 {
		t.Fatalf("inherited stored-secret limit = %v; want explicit zero",
			inherited.Limits)
	}

	zero := int64(0)
	if _, err := h.m.SetAccountLimitOverride(
		ctx, accountID, plans.StoredSecretLimit, &zero,
		testAdminActor(), "founder account test cap",
	); err != nil {
		t.Fatalf("SetAccountLimitOverride zero: %v", err)
	}
	r, snapshot, err := h.m.ResolvedStatus(ctx, accountID, "")
	if err != nil {
		t.Fatal(err)
	}
	override, ok := r.LimitOverrides[plans.StoredSecretLimit]
	if !ok || override.Max == nil || *override.Max != 0 ||
		override.ActorID != testAdminActor().ID ||
		override.ActorHandle != testAdminActor().Handle ||
		override.Reason != "founder account test cap" {
		t.Fatalf("stored-secret override attribution = %+v, present=%v", override, ok)
	}
	if r.Entitled != plans.Free || r.Provider != "" || r.CustomerID != "" {
		t.Fatalf("limit override mutated billing state: %+v", r)
	}
	if got, ok := snapshot.DefaultLimits[plans.StoredSecretLimit]; !ok || got != 0 ||
		snapshot.Limits[plans.StoredSecretLimit] != 0 {
		t.Fatalf("zero snapshot = defaults %v effective %v",
			snapshot.DefaultLimits, snapshot.Limits)
	}
	if got := h.applier.last(t).limits[plans.StoredSecretLimit]; got != 0 {
		t.Fatalf("applied stored-secret max = %d; want zero", got)
	}
	if len(r.AdminHistory) != 1 ||
		r.AdminHistory[0].LimitDimension != plans.StoredSecretLimit ||
		r.AdminHistory[0].LimitFromSource != "inherited" ||
		r.AdminHistory[0].LimitToSource != "override" ||
		r.AdminHistory[0].LimitTo == nil ||
		r.AdminHistory[0].LimitTo.Max == nil ||
		*r.AdminHistory[0].LimitTo.Max != 0 {
		t.Fatalf("set audit = %+v", r.AdminHistory)
	}

	// Returned records must not alias MemStore's nested map or Max pointer.
	override.Max = new(int64)
	*override.Max = 99
	r.LimitOverrides[plans.StoredSecretLimit] = override
	again := h.record(t, accountID)
	if got := *again.LimitOverrides[plans.StoredSecretLimit].Max; got != 0 {
		t.Fatalf("MemStore override aliased caller mutation: got %d", got)
	}

	version, historyLen, applyCalls := again.Version, len(again.AdminHistory), len(h.applier.calls)
	if _, err := h.m.SetAccountLimitOverride(
		ctx, accountID, plans.StoredSecretLimit, &zero,
		testAdminActor(), "idempotent retry",
	); err != nil {
		t.Fatalf("idempotent SetAccountLimitOverride: %v", err)
	}
	again = h.record(t, accountID)
	if again.Version != version || len(again.AdminHistory) != historyLen ||
		len(h.applier.calls) != applyCalls {
		t.Fatalf("idempotent set wrote or applied: before version/history/apply=%d/%d/%d after=%d/%d/%d",
			version, historyLen, applyCalls,
			again.Version, len(again.AdminHistory), len(h.applier.calls))
	}

	// A present nil Max is an explicit unlimited override to the finite
	// Personal catalog default.
	if _, err := h.m.SetAccountLimitOverride(
		ctx, accountID, plans.StoredSecretLimit, nil,
		testAdminActor(), "founder account is unlimited",
	); err != nil {
		t.Fatalf("SetAccountLimitOverride unlimited: %v", err)
	}
	r, snapshot, err = h.m.ResolvedStatus(ctx, accountID, "")
	if err != nil {
		t.Fatal(err)
	}
	override, ok = r.LimitOverrides[plans.StoredSecretLimit]
	if !ok || override.Max != nil {
		t.Fatalf("explicit unlimited override = %+v, present=%v", override, ok)
	}
	if got, ok := snapshot.DefaultLimits[plans.StoredSecretLimit]; !ok || got != 0 {
		t.Fatalf("unlimited default limits = %v; want Personal zero", snapshot.DefaultLimits)
	}
	if _, finite := snapshot.Limits[plans.StoredSecretLimit]; finite {
		t.Fatalf("unlimited effective limits = %v", snapshot.Limits)
	}
	unlimitedAudit := r.AdminHistory[len(r.AdminHistory)-1]
	if unlimitedAudit.Kind != "limit_override_set" ||
		unlimitedAudit.LimitFromSource != "override" ||
		unlimitedAudit.LimitToSource != "override" ||
		unlimitedAudit.LimitTo == nil || unlimitedAudit.LimitTo.Max != nil {
		t.Fatalf("explicit-unlimited audit is ambiguous: %+v", unlimitedAudit)
	}

	historyLen = len(r.AdminHistory)
	if _, err := h.m.SetAccountLimitOverride(
		ctx, accountID, plans.StoredSecretLimit, nil,
		testAdminActor(), "idempotent unlimited retry",
	); err != nil {
		t.Fatal(err)
	}
	if got := len(h.record(t, accountID).AdminHistory); got != historyLen {
		t.Fatalf("idempotent unlimited appended audit: %d -> %d", historyLen, got)
	}

	if _, err := h.m.ClearAccountLimitOverride(
		ctx, accountID, plans.StoredSecretLimit,
		testAdminActor(), "resume catalog inheritance",
	); err != nil {
		t.Fatalf("ClearAccountLimitOverride: %v", err)
	}
	r, snapshot, err = h.m.ResolvedStatus(ctx, accountID, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := r.LimitOverrides[plans.StoredSecretLimit]; ok ||
		r.LimitOverrides != nil {
		t.Fatalf("cleared overrides = %v; want absent inheritance", r.LimitOverrides)
	}
	if got, ok := snapshot.DefaultLimits[plans.StoredSecretLimit]; !ok || got != 0 ||
		snapshot.Limits[plans.StoredSecretLimit] != 0 {
		t.Fatalf("cleared effective limits = defaults %v effective %v; want catalog zero",
			snapshot.DefaultLimits, snapshot.Limits)
	}
	clearAudit := r.AdminHistory[len(r.AdminHistory)-1]
	if clearAudit.Kind != "limit_override_cleared" ||
		clearAudit.LimitFromSource != "override" ||
		clearAudit.LimitToSource != "inherited" {
		t.Fatalf("clear audit = %+v", clearAudit)
	}

	// A finite override on a finite catalog dimension restores the current
	// catalog default on clear.
	if _, err := h.m.SetAccountLimitOverride(
		ctx, accountID, plans.AgentLimit, &zero,
		testAdminActor(), "pause agent creation",
	); err != nil {
		t.Fatal(err)
	}
	if _, err := h.m.ClearAccountLimitOverride(
		ctx, accountID, plans.AgentLimit,
		testAdminActor(), "restore plan capacity",
	); err != nil {
		t.Fatal(err)
	}
	_, snapshot, err = h.m.ResolvedStatus(ctx, accountID, "")
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.DefaultLimits[plans.AgentLimit] != 25 ||
		snapshot.Limits[plans.AgentLimit] != 25 {
		t.Fatalf("cleared agent limit = defaults %v effective %v; want 25",
			snapshot.DefaultLimits, snapshot.Limits)
	}

	beforeClear := h.record(t, accountID)
	if _, err := h.m.ClearAccountLimitOverride(
		ctx, accountID, plans.AgentLimit,
		testAdminActor(), "idempotent clear",
	); err != nil {
		t.Fatal(err)
	}
	afterClear := h.record(t, accountID)
	if afterClear.Version != beforeClear.Version ||
		len(afterClear.AdminHistory) != len(beforeClear.AdminHistory) {
		t.Fatalf("idempotent clear wrote: before=%+v after=%+v", beforeClear, afterClear)
	}
}

func TestFounderRealmAndAgentLimitsCanBeExplicitlyUnlimited(t *testing.T) {
	h := newHarness(t, false)
	ctx := t.Context()
	const accountID = "acct_founder_resource_limits"

	for _, dimension := range []string{
		plans.RealmLimit,
		plans.AgentLimit,
		plans.AgentPerRealmLimit,
	} {
		if _, err := h.m.SetAccountLimitOverride(
			ctx,
			accountID,
			dimension,
			nil,
			testAdminActor(),
			"founder resource capacity is unlimited",
		); err != nil {
			t.Fatalf("set %s unlimited: %v", dimension, err)
		}
		record, snapshot, err := h.m.ResolvedStatus(ctx, accountID, "")
		if err != nil {
			t.Fatal(err)
		}
		override, present := record.LimitOverrides[dimension]
		if !present || override.Max != nil ||
			override.ActorID != testAdminActor().ID ||
			override.ActorHandle != testAdminActor().Handle {
			t.Fatalf("%s override = %+v, present=%t", dimension, override, present)
		}
		if _, finite := snapshot.Limits[dimension]; finite {
			t.Fatalf("%s remained finite in effective limits %v", dimension, snapshot.Limits)
		}
		if record.Entitled != plans.Free || record.Provider != "" ||
			record.CustomerID != "" {
			t.Fatalf("%s override mutated billing state: %+v", dimension, record)
		}
		audit := record.AdminHistory[len(record.AdminHistory)-1]
		if audit.Kind != "limit_override_set" ||
			audit.LimitDimension != dimension ||
			audit.LimitTo == nil ||
			audit.LimitTo.Max != nil {
			t.Fatalf("%s audit = %+v", dimension, audit)
		}
	}

	record := h.record(t, accountID)
	if len(record.LimitOverrides) != 3 {
		t.Fatalf("founder overrides = %v; want all three dimensions", record.LimitOverrides)
	}
}

func TestFounderResourceOverridesSurviveAgentsPerRealmCatalogPromotion(t *testing.T) {
	h := newHarness(t, false)
	ctx := t.Context()
	const accountID = "acct_founder_resource_promotion"

	phaseA, err := plans.Parse([]byte(`{
		"schema_version":"witself.plans.v0",
		"plans":[{
			"id":"free","name":"Personal","price_monthly":0,"available":true,
			"usage_billed":false,
			"limits":{"agents":25,"realms":1},
			"features":["memory","facts"]
		}]
	}`))
	if err != nil {
		t.Fatal(err)
	}
	h.m.cfg.Catalog = phaseA
	if created, pending, err := h.m.EnsureAccount(
		ctx, accountID,
	); err != nil || !created || pending {
		t.Fatalf("EnsureAccount = created=%v pending=%v err=%v", created, pending, err)
	}
	for _, dimension := range []string{
		plans.RealmLimit,
		plans.AgentLimit,
		plans.AgentPerRealmLimit,
	} {
		if _, err := h.m.SetAccountLimitOverride(
			ctx,
			accountID,
			dimension,
			nil,
			testAdminActor(),
			"founder resource capacity is explicitly unlimited",
		); err != nil {
			t.Fatalf("set %s unlimited: %v", dimension, err)
		}
	}
	before, beforeSnapshot, err := h.m.ResolvedStatus(ctx, accountID, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(beforeSnapshot.Limits) != 0 ||
		SnapshotApplyPending(before, beforeSnapshot) {
		t.Fatalf("founder Phase A snapshot = %+v, record=%+v", beforeSnapshot, before)
	}
	applyCalls := len(h.applier.calls)

	phaseB, err := plans.Parse([]byte(`{
		"schema_version":"witself.plans.v0",
		"plans":[{
			"id":"free","name":"Personal","price_monthly":0,"available":true,
			"usage_billed":false,
			"limits":{"agents":10,"agents_per_realm":10,"realms":1},
			"features":["memory","facts"]
		}]
	}`))
	if err != nil {
		t.Fatal(err)
	}
	h.m.cfg.Catalog = phaseB
	if err := h.m.ReconcileAccount(ctx, accountID); err != nil {
		t.Fatal(err)
	}
	after, afterSnapshot, err := h.m.ResolvedStatus(ctx, accountID, "")
	if err != nil {
		t.Fatal(err)
	}
	if afterSnapshot.DefaultLimits[plans.RealmLimit] != 1 ||
		afterSnapshot.DefaultLimits[plans.AgentLimit] != 10 ||
		afterSnapshot.DefaultLimits[plans.AgentPerRealmLimit] != 10 {
		t.Fatalf("Phase B defaults = %v", afterSnapshot.DefaultLimits)
	}
	if len(afterSnapshot.Limits) != 0 ||
		afterSnapshot.Hash != beforeSnapshot.Hash ||
		SnapshotApplyPending(after, afterSnapshot) ||
		len(h.applier.calls) != applyCalls {
		t.Fatalf(
			"founder promotion changed effective snapshot: before=%s after=%s limits=%v pending=%v calls=%d->%d",
			beforeSnapshot.Hash,
			afterSnapshot.Hash,
			afterSnapshot.Limits,
			SnapshotApplyPending(after, afterSnapshot),
			applyCalls,
			len(h.applier.calls),
		)
	}
}

func TestFounderUnlimitedOverrideOnAppliedUnlimitedSnapshotDoesNotReapply(t *testing.T) {
	h := newHarness(t, false)
	ctx := t.Context()
	const accountID = "acct_founder"

	phaseA, err := plans.Parse([]byte(`{
		"schema_version":"witself.plans.v0",
		"plans":[{
			"id":"free",
			"name":"Personal",
			"price_monthly":0,
			"available":true,
			"usage_billed":false,
			"limits":{"agents":25,"realms":1},
			"policies":{"transcript_retention_days":30},
			"features":["memory","facts"]
		}]
	}`))
	if err != nil {
		t.Fatal(err)
	}
	h.m.cfg.Catalog = phaseA

	created, pending, err := h.m.EnsureAccount(ctx, accountID)
	if err != nil || !created || pending {
		t.Fatalf("EnsureAccount = created=%v pending=%v err=%v", created, pending, err)
	}
	before, beforeSnapshot, err := h.m.ResolvedStatus(ctx, accountID, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, finite := beforeSnapshot.Limits[plans.StoredSecretLimit]; finite {
		t.Fatalf("test requires Phase A stored_secret absence, got %v", beforeSnapshot.Limits)
	}
	if SnapshotApplyPending(before, beforeSnapshot) || len(h.applier.calls) != 1 {
		t.Fatalf("baseline not converged: record=%+v calls=%d", before, len(h.applier.calls))
	}

	if _, err := h.m.SetAccountLimitOverride(
		ctx, accountID, plans.StoredSecretLimit, nil,
		testAdminActor(), "founder account is explicitly unlimited",
	); err != nil {
		t.Fatal(err)
	}
	after, afterSnapshot, err := h.m.ResolvedStatus(ctx, accountID, "")
	if err != nil {
		t.Fatal(err)
	}
	override, present := after.LimitOverrides[plans.StoredSecretLimit]
	if !present || override.Max != nil ||
		override.ActorID != testAdminActor().ID ||
		override.ActorHandle != testAdminActor().Handle ||
		override.Reason != "founder account is explicitly unlimited" {
		t.Fatalf("founder unlimited attribution = %+v, present=%v", override, present)
	}
	if len(after.AdminHistory) != 1 ||
		after.AdminHistory[0].Kind != "limit_override_set" ||
		after.AdminHistory[0].LimitDimension != plans.StoredSecretLimit ||
		after.AdminHistory[0].LimitFromSource != "inherited" ||
		after.AdminHistory[0].LimitToSource != "override" ||
		after.AdminHistory[0].LimitTo == nil ||
		after.AdminHistory[0].LimitTo.Max != nil {
		t.Fatalf("founder unlimited audit = %+v", after.AdminHistory)
	}
	if afterSnapshot.Hash != beforeSnapshot.Hash {
		t.Fatalf("explicit unlimited changed effective hash: %s -> %s",
			beforeSnapshot.Hash, afterSnapshot.Hash)
	}
	if SnapshotApplyPending(after, afterSnapshot) {
		t.Fatalf("unchanged founder snapshot became apply-pending: %+v", after)
	}
	if len(h.applier.calls) != 1 {
		t.Fatalf("unchanged founder snapshot made %d cell calls; want baseline call only",
			len(h.applier.calls))
	}

	phaseB, err := plans.Load()
	if err != nil {
		t.Fatal(err)
	}
	h.m.cfg.Catalog = phaseB
	if err := h.m.ReconcileAccount(ctx, accountID); err != nil {
		t.Fatal(err)
	}
	promoted, promotedSnapshot, err := h.m.ResolvedStatus(ctx, accountID, "")
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := promotedSnapshot.DefaultLimits[plans.StoredSecretLimit]; !ok || got != 0 {
		t.Fatalf("promoted founder default limits = %v; want Personal zero",
			promotedSnapshot.DefaultLimits)
	}
	if _, finite := promotedSnapshot.Limits[plans.StoredSecretLimit]; finite {
		t.Fatalf("promoted founder effective limits = %v; want explicit unlimited",
			promotedSnapshot.Limits)
	}
	if promotedSnapshot.Hash != afterSnapshot.Hash ||
		SnapshotApplyPending(promoted, promotedSnapshot) ||
		len(h.applier.calls) != 1 {
		t.Fatalf("catalog promotion changed founder effective snapshot: before=%s after=%s pending=%v calls=%d",
			afterSnapshot.Hash, promotedSnapshot.Hash,
			SnapshotApplyPending(promoted, promotedSnapshot), len(h.applier.calls))
	}
}

func TestAccountLimitOverrideApplyFailureAndCASRetry(t *testing.T) {
	t.Run("apply failure remains pending", func(t *testing.T) {
		h := newHarness(t, false)
		h.applier.fail = true
		zero := int64(0)
		if _, err := h.m.SetAccountLimitOverride(
			t.Context(), "acct_limit_pending", plans.AgentLimit, &zero,
			testAdminActor(), "emergency cap",
		); err != nil {
			t.Fatal(err)
		}
		r, snapshot, err := h.m.ResolvedStatus(t.Context(), "acct_limit_pending", "")
		if err != nil {
			t.Fatal(err)
		}
		if r.SnapshotRevision == 0 || r.DesiredSnapshotHash != snapshot.Hash ||
			!SnapshotApplyPending(r, snapshot) || r.AppliedSnapshotRevision != 0 {
			t.Fatalf("failed limit apply was not durably pending: record=%+v snapshot=%+v", r, snapshot)
		}
	})

	t.Run("CAS retry preserves competing override", func(t *testing.T) {
		h := newHarness(t, false)
		ctx := t.Context()
		const accountID = "acct_limit_race"
		h.hooked.beforePut = func(Record) {
			err := h.store.Put(ctx, Record{
				AccountID: accountID,
				Entitled:  plans.Free,
				Applied:   plans.Free,
				LimitOverrides: map[string]AccountLimitOverride{
					plans.StoredSecretLimit: {
						Max: nil, ActorID: "adm_competing",
						ActorHandle: "other", Reason: "concurrent founder exception",
						SetAt: h.ck.now(),
					},
				},
			})
			if err != nil {
				t.Errorf("competing Put: %v", err)
			}
		}
		seven := int64(7)
		if _, err := h.m.SetAccountLimitOverride(
			ctx, accountID, plans.AgentLimit, &seven,
			testAdminActor(), "requested capacity",
		); err != nil {
			t.Fatal(err)
		}
		r := h.record(t, accountID)
		if len(r.LimitOverrides) != 2 ||
			r.LimitOverrides[plans.StoredSecretLimit].Max != nil ||
			r.LimitOverrides[plans.AgentLimit].Max == nil ||
			*r.LimitOverrides[plans.AgentLimit].Max != 7 {
			t.Fatalf("CAS retry lost a limit override: %+v", r.LimitOverrides)
		}
	})
}

func TestAccountTranscriptRetentionOverrideIsIndependentOfBilling(t *testing.T) {
	h := newHarness(t, false)
	ctx := context.Background()
	days := int64(60)
	if _, err := h.m.SetTranscriptRetentionOverride(
		ctx, "acct_retention", &days, testAdminActor(), "early enterprise evaluation",
	); err != nil {
		t.Fatalf("SetTranscriptRetentionOverride: %v", err)
	}
	r, snapshot, err := h.m.ResolvedStatus(ctx, "acct_retention", "")
	if err != nil {
		t.Fatal(err)
	}
	if r.Entitled != plans.Free || r.Provider != "" || r.CustomerID != "" {
		t.Fatalf("billing state changed by policy override: %+v", r)
	}
	if snapshot.Plan != plans.Free ||
		snapshot.DefaultPolicies[plans.TranscriptRetentionDaysPolicy] != 30 ||
		snapshot.Policies[plans.TranscriptRetentionDaysPolicy] != 60 {
		t.Fatalf("resolved snapshot = %+v; want free default 30, effective 60", snapshot)
	}
	if r.TranscriptRetentionOverride == nil ||
		r.TranscriptRetentionOverride.ActorID != testAdminActor().ID ||
		r.TranscriptRetentionOverride.ActorHandle != testAdminActor().Handle ||
		r.TranscriptRetentionOverride.Reason != "early enterprise evaluation" {
		t.Fatalf("override attribution = %+v", r.TranscriptRetentionOverride)
	}
	if len(h.applier.calls) != 1 ||
		h.applier.calls[0].policies[plans.TranscriptRetentionDaysPolicy] != 60 {
		t.Fatalf("apply calls = %+v; want one 60-day snapshot", h.applier.calls)
	}

	if _, err := h.m.ClearTranscriptRetentionOverride(
		ctx, "acct_retention", testAdminActor(), "restore Personal default",
	); err != nil {
		t.Fatalf("ClearTranscriptRetentionOverride: %v", err)
	}
	r, snapshot, err = h.m.ResolvedStatus(ctx, "acct_retention", "")
	if err != nil {
		t.Fatal(err)
	}
	if r.TranscriptRetentionOverride != nil ||
		snapshot.Policies[plans.TranscriptRetentionDaysPolicy] != 30 {
		t.Fatalf("cleared snapshot = %+v record=%+v", snapshot, r)
	}
	if len(r.AdminHistory) != 2 ||
		r.AdminHistory[0].Kind != "transcript_retention_override_set" ||
		r.AdminHistory[1].Kind != "transcript_retention_override_cleared" ||
		r.AdminHistory[0].ActorID != testAdminActor().ID ||
		r.AdminHistory[0].ActorHandle != testAdminActor().Handle {
		t.Fatalf("admin history = %+v", r.AdminHistory)
	}
	if len(h.applier.calls) != 2 ||
		h.applier.calls[1].policies[plans.TranscriptRetentionDaysPolicy] != 30 {
		t.Fatalf("apply calls after clear = %+v", h.applier.calls)
	}
}

func TestRetentionOverrideFailureRemainsExplicitlyPending(t *testing.T) {
	h := newHarness(t, false)
	h.applier.fail = true
	days := int64(60)
	if _, err := h.m.SetTranscriptRetentionOverride(
		context.Background(), "acct_pending", &days, testAdminActor(), "pending test",
	); err != nil {
		t.Fatal(err)
	}
	r, snapshot, err := h.m.ResolvedStatus(context.Background(), "acct_pending", "")
	if err != nil {
		t.Fatal(err)
	}
	if r.SnapshotRevision == 0 || r.DesiredSnapshotHash != snapshot.Hash {
		t.Fatalf("desired fence was not persisted: record=%+v snapshot=%+v", r, snapshot)
	}
	if !SnapshotApplyPending(r, snapshot) || r.AppliedSnapshotRevision != 0 {
		t.Fatalf("failed cell apply was reported converged: %+v", r)
	}
}

func TestAccountTranscriptRetentionCanBeExplicitlyIndefinite(t *testing.T) {
	h := newHarness(t, false)
	ctx := context.Background()
	if _, err := h.m.SetTranscriptRetentionOverride(
		ctx, "acct_indefinite", nil, testAdminActor(), "contract exception",
	); err != nil {
		t.Fatalf("SetTranscriptRetentionOverride: %v", err)
	}
	r, snapshot, err := h.m.ResolvedStatus(ctx, "acct_indefinite", "")
	if err != nil {
		t.Fatal(err)
	}
	if r.TranscriptRetentionOverride == nil || r.TranscriptRetentionOverride.Days != nil {
		t.Fatalf("override = %+v; want explicit indefinite", r.TranscriptRetentionOverride)
	}
	if _, capped := snapshot.Policies[plans.TranscriptRetentionDaysPolicy]; capped {
		t.Fatalf("effective policies = %v; want indefinite", snapshot.Policies)
	}
}

func TestEnterpriseBackfillOverrideDoesNotFabricateBilling(t *testing.T) {
	h := newHarness(t, false)
	ctx := context.Background()
	if _, err := h.m.SetAccountPlanOverride(
		ctx, "acct_scott", "enterprise", testAdminActor(), "founder account backfill",
	); err != nil {
		t.Fatalf("SetAccountPlanOverride: %v", err)
	}
	r, snapshot, err := h.m.ResolvedStatus(ctx, "acct_scott", "")
	if err != nil {
		t.Fatal(err)
	}
	if r.Entitled != plans.Free || r.Provider != "" || r.CustomerID != "" {
		t.Fatalf("provider billing was fabricated: %+v", r)
	}
	if r.PlanOverride == nil || snapshot.Plan != "enterprise" {
		t.Fatalf("override/snapshot = %+v / %+v", r.PlanOverride, snapshot)
	}
	if r.PlanOverride.ActorID != testAdminActor().ID ||
		r.PlanOverride.ActorHandle != testAdminActor().Handle {
		t.Fatalf("plan override attribution = %+v", r.PlanOverride)
	}
	if _, capped := snapshot.Policies[plans.TranscriptRetentionDaysPolicy]; capped {
		t.Fatalf("enterprise backfill should inherit indefinite retention: %v", snapshot.Policies)
	}
	for _, feature := range []string{"memory", "facts", "secrets", "collaboration", "support"} {
		if !slices.Contains(snapshot.Features, feature) {
			t.Fatalf("enterprise backfill features = %v; missing %q", snapshot.Features, feature)
		}
	}
	if len(h.applier.calls) != 1 || h.applier.calls[0].plan != "enterprise" {
		t.Fatalf("apply calls = %+v", h.applier.calls)
	}
	for _, feature := range snapshot.Features {
		if !slices.Contains(h.applier.calls[0].features, feature) {
			t.Fatalf("applied enterprise features = %v; missing %q", h.applier.calls[0].features, feature)
		}
	}

	if _, err := h.m.ClearAccountPlanOverride(
		ctx, "acct_scott", testAdminActor(), "backfill rollback test",
	); err != nil {
		t.Fatal(err)
	}
	r, snapshot, err = h.m.ResolvedStatus(ctx, "acct_scott", "")
	if err != nil {
		t.Fatal(err)
	}
	if r.PlanOverride != nil || snapshot.Plan != plans.Free || r.Entitled != plans.Free {
		t.Fatalf("cleared override = %+v / %+v", r, snapshot)
	}
}

func TestClearingEnterpriseOverrideCannotBypassPersonalFitCheck(t *testing.T) {
	h := newHarness(t, false)
	ctx := context.Background()
	if _, err := h.m.SetAccountPlanOverride(
		ctx, "acct_scott_fit", "enterprise", testAdminActor(), "founder account backfill",
	); err != nil {
		t.Fatal(err)
	}
	h.fit.set([]string{"agents 26 > 25"})
	if _, err := h.m.ClearAccountPlanOverride(
		ctx, "acct_scott_fit", testAdminActor(), "restore Personal classification",
	); err != nil {
		t.Fatal(err)
	}
	r, snapshot, err := h.m.ResolvedStatus(ctx, "acct_scott_fit", "")
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Plan != plans.Free || r.Applied != "enterprise" ||
		!strings.Contains(r.ApplyBlocked, "agents 26 > 25") ||
		!SnapshotApplyPending(r, snapshot) {
		t.Fatalf("unsafe override clear was reported applied: record=%+v snapshot=%+v", r, snapshot)
	}
	if call := h.applier.last(t); call.plan != "enterprise" {
		t.Fatalf("Personal snapshot bypassed the fit check: %+v", call)
	}

	h.fit.set(nil)
	if err := h.m.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	r, snapshot, err = h.m.ResolvedStatus(ctx, "acct_scott_fit", "")
	if err != nil {
		t.Fatal(err)
	}
	if r.Applied != plans.Free || r.ApplyBlocked != "" || SnapshotApplyPending(r, snapshot) {
		t.Fatalf("pruned account did not converge to Personal: record=%+v snapshot=%+v", r, snapshot)
	}
}

func TestRetentionOverrideValidation(t *testing.T) {
	h := newHarness(t, false)
	ctx := context.Background()
	for _, days := range []int64{0, plans.MaxTranscriptRetentionDays + 1} {
		days := days
		if _, err := h.m.SetTranscriptRetentionOverride(
			ctx, "acct_bad", &days, testAdminActor(), "invalid test",
		); !errors.Is(err, ErrAdminInput) {
			t.Fatalf("days=%d error=%v; want ErrAdminInput", days, err)
		}
	}
	if _, err := h.m.SetTranscriptRetentionOverride(
		ctx, "acct_bad", nil, AdminActor{Handle: "scott"}, "missing actor",
	); !errors.Is(err, ErrAdminInput) {
		t.Fatalf("missing actor id error=%v; want ErrAdminInput", err)
	}
	if _, err := h.m.SetTranscriptRetentionOverride(
		ctx, "acct_bad", nil, AdminActor{ID: testAdminActor().ID}, "missing handle",
	); !errors.Is(err, ErrAdminInput) {
		t.Fatalf("missing actor handle error=%v; want ErrAdminInput", err)
	}
}

func TestHeadlessUpgrade(t *testing.T) {
	h := newHarness(t, false)
	ctx := context.Background()

	out, err := h.m.RequestUpgrade(ctx, "acct_1", "s@example.com", "standard")
	if err != nil || out.Kind != "done" {
		t.Fatalf("RequestUpgrade = %+v, %v; want done", out, err)
	}
	r := h.record(t, "acct_1")
	if r.Entitled != "standard" || r.Applied != "standard" || r.Pending != nil {
		t.Fatalf("record = %+v; want entitled+applied standard", r)
	}
	call := h.applier.last(t)
	if call.plan != "standard" || call.limits["agents"] != 250 || call.limits["realms"] != 10 {
		t.Fatalf("applied snapshot = %+v; want standard 250/10", call)
	}
	joined := strings.Join(call.features, ",")
	if !strings.Contains(joined, "secrets") || !strings.Contains(joined, "collaboration") {
		t.Fatalf("applied features = %v; want secrets+collaboration", call.features)
	}
}

func TestInteractiveUpgradeFlow(t *testing.T) {
	h := newHarness(t, true)
	ctx := context.Background()

	out, err := h.m.RequestUpgrade(ctx, "acct_1", "s@example.com", "standard")
	if err != nil || out.Kind != "action" || out.URL == "" {
		t.Fatalf("RequestUpgrade = %+v, %v; want action+URL", out, err)
	}
	r := h.record(t, "acct_1")
	if r.Pending == nil || r.Pending.Kind != PendingUpgrade || r.Pending.Plan != "standard" {
		t.Fatalf("pending = %+v; want upgrade->standard", r.Pending)
	}
	if !r.Pending.Expires.Equal(h.ck.t.Add(DefaultPendingTTL)) {
		t.Fatalf("expires = %v; want request+TTL", r.Pending.Expires)
	}
	if r.Entitled != plans.Free {
		t.Fatalf("entitled = %q before payment; want free", r.Entitled)
	}

	// The payer completes checkout; the provider's events arrive (webhook).
	h.ck.t = h.ck.t.Add(time.Minute)
	events, err := h.fake.Complete(r.CustomerID)
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if err := h.m.OnEvents(ctx, "fake", events); err != nil {
		t.Fatalf("OnEvents: %v", err)
	}
	r = h.record(t, "acct_1")
	if r.Entitled != "standard" || r.Applied != "standard" || r.Pending != nil {
		t.Fatalf("after events record = %+v; want entitled+applied standard, no pending", r)
	}
}

func TestPendingUpgradeExpires(t *testing.T) {
	h := newHarness(t, true)
	ctx := context.Background()

	if _, err := h.m.RequestUpgrade(ctx, "acct_1", "s@example.com", "standard"); err != nil {
		t.Fatalf("RequestUpgrade: %v", err)
	}
	r := h.record(t, "acct_1")

	h.ck.t = h.ck.t.Add(DefaultPendingTTL + time.Hour) // the payer never returns
	if err := h.m.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if r = h.record(t, "acct_1"); r.Pending != nil {
		t.Fatalf("pending survived expiry: %+v", r.Pending)
	}
	// The provider-side checkout was cancelled too.
	if _, err := h.fake.Complete(r.CustomerID); err == nil {
		t.Fatal("provider checkout should have been cancelled at expiry")
	}
}

func TestUpgradeValidation(t *testing.T) {
	h := newHarness(t, false)
	ctx := context.Background()

	if _, err := h.m.RequestUpgrade(ctx, "acct_1", "s@example.com", "nope"); err == nil {
		t.Fatal("unknown plan should error")
	}
	if _, err := h.m.RequestUpgrade(ctx, "acct_1", "s@example.com", "free"); err == nil ||
		!strings.Contains(err.Error(), "already on") {
		t.Fatal("upgrading free->free should read as already-on")
	}
	if _, err := h.m.RequestUpgrade(ctx, "acct_1", "s@example.com", "standard"); err != nil {
		t.Fatalf("RequestUpgrade: %v", err)
	}
	if _, err := h.m.RequestUpgrade(ctx, "acct_1", "s@example.com", "standard"); err == nil ||
		!strings.Contains(err.Error(), "already on") {
		t.Fatal("re-upgrading to the current plan should error")
	}
	// Direction enforcement: from a paid plan, a cheaper target is a downgrade.
	if _, err := h.m.RequestUpgrade(ctx, "acct_1", "s@example.com", "free"); err == nil ||
		!strings.Contains(err.Error(), "not an upgrade") {
		t.Fatal("upgrading standard->free should point at downgrade")
	}
	if _, err := h.m.RequestDowngrade(ctx, "acct_1", "s@example.com", "team"); err == nil ||
		!strings.Contains(err.Error(), "not a downgrade") {
		t.Fatal("downgrading standard->team should point at upgrade")
	}
}

func TestUnavailableTierRecordsLead(t *testing.T) {
	h := newHarness(t, false)
	ctx := context.Background()

	out, err := h.m.RequestUpgrade(ctx, "acct_1", "s@example.com", "enterprise")
	if err != nil || out.Kind != "contact" {
		t.Fatalf("RequestUpgrade enterprise = %+v, %v; want contact", out, err)
	}
	r := h.record(t, "acct_1")
	if r.Pending == nil || r.Pending.Kind != PendingContact || r.Pending.Plan != "enterprise" {
		t.Fatalf("pending = %+v; want a recorded enterprise lead", r.Pending)
	}
	if r.CustomerID != "" {
		t.Fatal("a sales lead must not create billing objects")
	}
	if err := h.m.CancelPending(ctx, "acct_1"); err != nil {
		t.Fatalf("CancelPending: %v", err)
	}
	if r = h.record(t, "acct_1"); r.Pending != nil {
		t.Fatalf("lead survived cancel: %+v", r.Pending)
	}
}

func TestDowngradeFitBlocked(t *testing.T) {
	h := newHarness(t, false)
	ctx := context.Background()
	if _, err := h.m.RequestUpgrade(ctx, "acct_1", "s@example.com", "standard"); err != nil {
		t.Fatalf("RequestUpgrade: %v", err)
	}

	h.fit.set([]string{"agents 112 > 25", "3 realms > 1", "14 secrets in use"})
	_, err := h.m.RequestDowngrade(ctx, "acct_1", "s@example.com", "free")
	if err == nil || !strings.Contains(err.Error(), "agents 112 > 25") {
		t.Fatalf("blocked downgrade error = %v; want the violation report", err)
	}
	if r := h.record(t, "acct_1"); r.Pending != nil {
		t.Fatalf("blocked downgrade must not park a pending change: %+v", r.Pending)
	}
}

func TestScheduledDowngradeFlow(t *testing.T) {
	h := newHarness(t, false)
	ctx := context.Background()
	if _, err := h.m.RequestUpgrade(ctx, "acct_1", "s@example.com", "standard"); err != nil {
		t.Fatalf("RequestUpgrade: %v", err)
	}
	periodEnd := h.ck.t.AddDate(0, 0, 30)

	out, err := h.m.RequestDowngrade(ctx, "acct_1", "s@example.com", "free")
	if err != nil || out.Kind != "scheduled" || !out.Effective.Equal(periodEnd) {
		t.Fatalf("RequestDowngrade = %+v, %v; want scheduled at period end", out, err)
	}
	r := h.record(t, "acct_1")
	if r.Pending == nil || r.Pending.Kind != PendingDowngrade || r.Entitled != "standard" {
		t.Fatalf("record = %+v; want pending downgrade, still entitled standard", r)
	}

	// The period ends; the provider announces it (webhook-equivalent).
	h.ck.t = periodEnd.Add(time.Hour)
	if err := h.m.OnEvents(ctx, "fake", h.fake.ApplyDue()); err != nil {
		t.Fatalf("OnEvents: %v", err)
	}
	r = h.record(t, "acct_1")
	if r.Entitled != plans.Free || r.Applied != plans.Free || r.Pending != nil {
		t.Fatalf("after period end record = %+v; want free/free, no pending", r)
	}
	if call := h.applier.last(t); call.plan != plans.Free || call.limits["agents"] != 25 {
		t.Fatalf("applied snapshot = %+v; want free 25/1", call)
	}
}

func TestApplierFailureThenReconcile(t *testing.T) {
	h := newHarness(t, false)
	ctx := context.Background()

	h.applier.fail = true // the cell push fails: paid but not applied
	out, err := h.m.RequestUpgrade(ctx, "acct_1", "s@example.com", "standard")
	if err != nil || out.Kind != "done" {
		t.Fatalf("RequestUpgrade = %+v, %v", out, err)
	}
	r := h.record(t, "acct_1")
	if r.Entitled != "standard" || r.Applied != plans.Free {
		t.Fatalf("record = %+v; want entitled standard, applied still free", r)
	}

	h.applier.fail = false // the cell recovers; the sweep converges the gap
	if err := h.m.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if r = h.record(t, "acct_1"); r.Applied != "standard" {
		t.Fatalf("Reconcile did not converge applied: %+v", r)
	}
}

func TestPastDueTracking(t *testing.T) {
	h := newHarness(t, false)
	ctx := context.Background()
	if _, err := h.m.RequestUpgrade(ctx, "acct_1", "s@example.com", "standard"); err != nil {
		t.Fatalf("RequestUpgrade: %v", err)
	}
	r := h.record(t, "acct_1")

	fail := []billing.Event{{Type: billing.EventPaymentFailed, CustomerID: r.CustomerID, At: h.ck.t}}
	if err := h.m.OnEvents(ctx, "fake", fail); err != nil {
		t.Fatalf("OnEvents: %v", err)
	}
	if r = h.record(t, "acct_1"); r.PastDueSince == nil {
		t.Fatal("payment failure should set PastDueSince")
	}
	// Redelivery must not move the original past-due timestamp.
	later := []billing.Event{{Type: billing.EventPaymentFailed, CustomerID: r.CustomerID, At: h.ck.t.Add(time.Hour)}}
	if err := h.m.OnEvents(ctx, "fake", later); err != nil {
		t.Fatalf("OnEvents: %v", err)
	}
	if r2 := h.record(t, "acct_1"); !r2.PastDueSince.Equal(*r.PastDueSince) {
		t.Fatal("redelivered failure moved PastDueSince")
	}

	rec := []billing.Event{{Type: billing.EventPaymentRecovered, CustomerID: r.CustomerID, At: h.ck.t.Add(2 * time.Hour)}}
	if err := h.m.OnEvents(ctx, "fake", rec); err != nil {
		t.Fatalf("OnEvents: %v", err)
	}
	if r = h.record(t, "acct_1"); r.PastDueSince != nil {
		t.Fatal("recovery should clear PastDueSince")
	}
}

// TestProviderCutover proves the fake -> real migration path: switching the
// DEFAULT provider routes new purchases to the new partner while existing
// accounts keep routing to the provider that holds their subscription, card,
// and invoice history. The cutover is a config change, not a migration event.
func TestProviderCutover(t *testing.T) {
	ctx := context.Background()
	catalog, err := plans.Load()
	if err != nil {
		t.Fatalf("plans.Load: %v", err)
	}
	ck := &clock{t: time.Date(2026, 7, 4, 12, 0, 0, 0, time.UTC)}
	oldP := fake.New(fake.Config{Prices: catalog.Prices(), Now: ck.now})
	st := NewMemStore()
	ap := &recApplier{}

	// Era 1: only the fake exists; an account buys standard on it.
	m1, err := NewManager(Config{
		Catalog: catalog, Providers: map[string]billing.Provider{"fake": oldP}, Default: "fake",
		Store: st, Applier: ap, Now: ck.now,
	})
	if err != nil {
		t.Fatalf("NewManager era1: %v", err)
	}
	if _, err := m1.RequestUpgrade(ctx, "acct_old", "old@example.com", "standard"); err != nil {
		t.Fatalf("era1 upgrade: %v", err)
	}

	// Era 2: the real partner arrives; SAME store, default flips to "stripe".
	newP := fake.New(fake.Config{Prices: catalog.Prices(), Now: ck.now})
	m2, err := NewManager(Config{
		Catalog:   catalog,
		Providers: map[string]billing.Provider{"fake": oldP, "stripe": newP},
		Default:   "stripe",
		Store:     st, Applier: ap, Now: ck.now,
	})
	if err != nil {
		t.Fatalf("NewManager era2: %v", err)
	}

	// New purchases land on the new default...
	if _, err := m2.RequestUpgrade(ctx, "acct_new", "new@example.com", "standard"); err != nil {
		t.Fatalf("era2 new-account upgrade: %v", err)
	}
	rNew, _, _ := st.Get(ctx, "acct_new")
	if rNew.Provider != "stripe" {
		t.Fatalf("new account pinned to %q; want stripe", rNew.Provider)
	}
	if inv, err := newP.ListInvoices(ctx, rNew.CustomerID); err != nil || len(inv) != 1 {
		t.Fatalf("new account's invoice should live at the new provider: %v, %v", inv, err)
	}

	// ...while the existing account still routes to its pinned provider.
	rOld, _, _ := st.Get(ctx, "acct_old")
	if rOld.Provider != "fake" {
		t.Fatalf("old account pinned to %q; want fake", rOld.Provider)
	}
	out, err := m2.RequestDowngrade(ctx, "acct_old", "old@example.com", "free")
	if err != nil || out.Kind != "scheduled" {
		t.Fatalf("era2 old-account downgrade = %+v, %v; want scheduled via the OLD provider", out, err)
	}
	if _, err := oldP.ListInvoices(ctx, rOld.CustomerID); err != nil {
		t.Fatalf("old account's history must stay readable at the old provider: %v", err)
	}

	// Both partners minted the SAME customer id for different accounts (each
	// fake numbers from 1) — exactly the cross-provider collision the scoped
	// lookup exists for. A stripe-scoped event for this id must touch only
	// the stripe-pinned account, never the fake-pinned one.
	if rOld.CustomerID != rNew.CustomerID {
		t.Fatalf("test premise: expected colliding customer ids, got %q vs %q", rOld.CustomerID, rNew.CustomerID)
	}
	ck.t = ck.t.Add(time.Minute)
	ev := []billing.Event{{Type: billing.EventSubscriptionCanceled, CustomerID: rOld.CustomerID, At: ck.t}}
	if err := m2.OnEvents(ctx, "stripe", ev); err != nil {
		t.Fatalf("OnEvents: %v", err)
	}
	if r, _, _ := st.Get(ctx, "acct_old"); r.Entitled != "standard" {
		t.Fatalf("a stripe-scoped event must not touch a fake-pinned record; entitled = %q", r.Entitled)
	}
	if r, _, _ := st.Get(ctx, "acct_new"); r.Entitled != plans.Free {
		t.Fatalf("the stripe-scoped event should have reached the stripe-pinned record; entitled = %q", r.Entitled)
	}
}

// TestCancelRacingActivation reproduces the review's HIGH finding: the payer
// completes checkout in exactly the window between CancelPending's read and
// its write. Without CAS the stale write landed last and the customer paid
// for a plan the record denied; with CAS the cancel loses, re-reads, and
// reports "nothing is pending" while the paid entitlement stands.
func TestCancelRacingActivation(t *testing.T) {
	h := newHarness(t, true)
	ctx := context.Background()

	if _, err := h.m.RequestUpgrade(ctx, "acct_1", "s@example.com", "standard"); err != nil {
		t.Fatalf("RequestUpgrade: %v", err)
	}
	r := h.record(t, "acct_1")

	// The race window under disarm-first ordering: the payer completes
	// checkout in the instant between CancelPending's read and its provider
	// disarm. Simulated with a provider hook (the store hook can no longer
	// reach this window — that is the point of the reordering).
	hooked := &hookProvider{Provider: h.fake, beforeCancel: func() {
		h.ck.t = h.ck.t.Add(time.Minute)
		events, err := h.fake.Complete(r.CustomerID)
		if err != nil {
			t.Errorf("Complete: %v", err)
		}
		if err := h.m.OnEvents(ctx, "fake", events); err != nil {
			t.Errorf("OnEvents: %v", err)
		}
	}}
	m2, err := NewManager(Config{
		Catalog: mustCatalog(t), Providers: map[string]billing.Provider{"fake": hooked}, Default: "fake",
		Store: h.store, Applier: h.applier, Now: h.ck.now,
	})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	err = m2.CancelPending(ctx, "acct_1")
	if err == nil || !strings.Contains(err.Error(), "nothing is pending") {
		t.Fatalf("CancelPending racing activation = %v; want 'nothing is pending'", err)
	}
	final := h.record(t, "acct_1")
	if final.Entitled != "standard" || final.Applied != "standard" {
		t.Fatalf("record = %+v; the paid entitlement must survive the racing cancel", final)
	}
}

// hookProvider interposes on CancelPending to interleave work into the
// disarm window.
type hookProvider struct {
	billing.Provider
	beforeCancel func()
}

func (h *hookProvider) CancelPending(ctx context.Context, customerID string) error {
	if f := h.beforeCancel; f != nil {
		h.beforeCancel = nil
		f()
	}
	return h.Provider.CancelPending(ctx, customerID)
}

// TestStaleCancelDropped reproduces the review's redelivery finding: a
// cancellation whose partner timestamp predates the current entitlement (a
// redelivered or out-of-order webhook) must be dropped, not clobber a newer
// paid subscription.
func TestStaleCancelDropped(t *testing.T) {
	h := newHarness(t, false)
	ctx := context.Background()

	if _, err := h.m.RequestUpgrade(ctx, "acct_1", "s@example.com", "standard"); err != nil {
		t.Fatalf("RequestUpgrade: %v", err)
	}
	r := h.record(t, "acct_1")

	// Dunning cancels the subscription at T1...
	t1 := h.ck.t.Add(time.Hour)
	cancel := []billing.Event{{Type: billing.EventSubscriptionCanceled, CustomerID: r.CustomerID, At: t1}}
	if err := h.m.OnEvents(ctx, "fake", cancel); err != nil {
		t.Fatalf("OnEvents: %v", err)
	}
	if r = h.record(t, "acct_1"); r.Entitled != plans.Free {
		t.Fatalf("entitled = %q after cancel; want free", r.Entitled)
	}

	// ...the customer re-subscribes at T2 > T1...
	h.ck.t = t1.Add(time.Hour)
	if _, err := h.m.RequestUpgrade(ctx, "acct_1", "s@example.com", "standard"); err != nil {
		t.Fatalf("re-upgrade: %v", err)
	}

	// ...and the T1 cancellation is REDELIVERED (ack was lost). It is stale
	// against the T2 entitlement and must be dropped.
	if err := h.m.OnEvents(ctx, "fake", cancel); err != nil {
		t.Fatalf("OnEvents redelivery: %v", err)
	}
	if r = h.record(t, "acct_1"); r.Entitled != "standard" || r.Applied != "standard" {
		t.Fatalf("record = %+v; a stale redelivered cancel clobbered a newer paid subscription", r)
	}
}

// TestApplyTimeFitCheck reproduces the review's missing-authoritative-check
// finding: usage grows past the target's caps during the 30-day wait, so the
// period-end downgrade must NOT be applied to the cell — the gap stays
// visible (Entitled != Applied, ApplyBlocked) until the account fits.
func TestApplyTimeFitCheck(t *testing.T) {
	h := newHarness(t, false)
	ctx := context.Background()

	if _, err := h.m.RequestUpgrade(ctx, "acct_1", "s@example.com", "standard"); err != nil {
		t.Fatalf("RequestUpgrade: %v", err)
	}
	periodEnd := h.ck.t.AddDate(0, 0, 30)
	if _, err := h.m.RequestDowngrade(ctx, "acct_1", "s@example.com", "free"); err != nil {
		t.Fatalf("RequestDowngrade: %v", err) // fits at request time
	}

	// Usage grows during the wait; at period end billing cancels regardless.
	h.fit.set([]string{"agents 112 > 25"})
	h.ck.t = periodEnd.Add(time.Hour)
	if err := h.m.OnEvents(ctx, "fake", h.fake.ApplyDue()); err != nil {
		t.Fatalf("OnEvents: %v", err)
	}
	r := h.record(t, "acct_1")
	if r.Entitled != plans.Free {
		t.Fatalf("entitled = %q; billing did end the subscription", r.Entitled)
	}
	if r.Applied != "standard" || !strings.Contains(r.ApplyBlocked, "agents 112 > 25") {
		t.Fatalf("record = %+v; the cell must keep the old plan and the block must be visible", r)
	}
	if call := h.applier.last(t); call.plan != "standard" {
		t.Fatalf("a free snapshot was pushed despite the failed fit check: %+v", call)
	}

	// The account is pruned; the next sweep converges.
	h.fit.set(nil)
	if err := h.m.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	r = h.record(t, "acct_1")
	if r.Applied != plans.Free || r.ApplyBlocked != "" {
		t.Fatalf("record = %+v; want applied free with the block cleared", r)
	}
}

// TestReplacingPendingCancelsProviderSide reproduces the review's finding
// that replacing a pending change must also cancel it at the provider — a
// "replaced" scheduled downgrade must not fire at period end.
func TestReplacingPendingCancelsProviderSide(t *testing.T) {
	h := newHarness(t, false)
	ctx := context.Background()

	if _, err := h.m.RequestUpgrade(ctx, "acct_1", "s@example.com", "standard"); err != nil {
		t.Fatalf("RequestUpgrade: %v", err)
	}
	periodEnd := h.ck.t.AddDate(0, 0, 30)
	if _, err := h.m.RequestDowngrade(ctx, "acct_1", "s@example.com", "free"); err != nil {
		t.Fatalf("RequestDowngrade: %v", err)
	}
	// The customer changes their mind toward enterprise: the lead replaces
	// the scheduled downgrade — INCLUDING at the provider.
	if out, err := h.m.RequestUpgrade(ctx, "acct_1", "s@example.com", "enterprise"); err != nil || out.Kind != "contact" {
		t.Fatalf("RequestUpgrade enterprise = %+v, %v", out, err)
	}

	h.ck.t = periodEnd.Add(time.Hour)
	if events := h.fake.ApplyDue(); len(events) != 0 {
		t.Fatalf("the replaced downgrade still fired at the provider: %+v", events)
	}
	r := h.record(t, "acct_1")
	if r.Entitled != "standard" || r.Pending == nil || r.Pending.Kind != PendingContact {
		t.Fatalf("record = %+v; want standard entitlement with the enterprise lead pending", r)
	}
}

// TestUnknownPlanEventErrors: an activation for a plan the catalog does not
// know is deploy skew — it must fail the batch (provider redelivers until the
// catalog catches up) and must not corrupt entitlement meanwhile.
func TestUnknownPlanEventErrors(t *testing.T) {
	h := newHarness(t, false)
	ctx := context.Background()
	if _, err := h.m.RequestUpgrade(ctx, "acct_1", "s@example.com", "standard"); err != nil {
		t.Fatalf("RequestUpgrade: %v", err)
	}
	r := h.record(t, "acct_1")

	bogus := []billing.Event{{Type: billing.EventSubscriptionActivated, CustomerID: r.CustomerID, Plan: "mystery", At: h.ck.t.Add(time.Hour)}}
	if err := h.m.OnEvents(ctx, "fake", bogus); err == nil {
		t.Fatal("unknown-plan activation must fail the batch for redelivery")
	}
	if r = h.record(t, "acct_1"); r.Entitled != "standard" {
		t.Fatalf("entitled = %q; the unroutable event must not corrupt state", r.Entitled)
	}
}

// TestCancelClearsPastDue: terminal dunning ends with a cancellation — the
// account lands on free, not on free-and-eternally-past-due.
func TestCancelClearsPastDue(t *testing.T) {
	h := newHarness(t, false)
	ctx := context.Background()
	if _, err := h.m.RequestUpgrade(ctx, "acct_1", "s@example.com", "standard"); err != nil {
		t.Fatalf("RequestUpgrade: %v", err)
	}
	r := h.record(t, "acct_1")

	events := []billing.Event{
		{Type: billing.EventPaymentFailed, CustomerID: r.CustomerID, At: h.ck.t.Add(time.Hour)},
		{Type: billing.EventSubscriptionCanceled, CustomerID: r.CustomerID, At: h.ck.t.Add(2 * time.Hour)},
	}
	if err := h.m.OnEvents(ctx, "fake", events); err != nil {
		t.Fatalf("OnEvents: %v", err)
	}
	r = h.record(t, "acct_1")
	if r.Entitled != plans.Free || r.PastDueSince != nil {
		t.Fatalf("record = %+v; want free with PastDueSince cleared", r)
	}
}

// TestDunningFence reproduces the review's redelivered-dunning finding: a
// payment_failed folded AFTER its recovery (partial-batch redelivery) must
// not re-mark a paid-up account past due.
func TestDunningFence(t *testing.T) {
	h := newHarness(t, false)
	ctx := context.Background()
	if _, err := h.m.RequestUpgrade(ctx, "acct_1", "s@example.com", "standard"); err != nil {
		t.Fatalf("RequestUpgrade: %v", err)
	}
	r := h.record(t, "acct_1")

	t1, t2 := h.ck.t.Add(time.Hour), h.ck.t.Add(2*time.Hour)
	failed := []billing.Event{{Type: billing.EventPaymentFailed, CustomerID: r.CustomerID, At: t1}}
	recovered := []billing.Event{{Type: billing.EventPaymentRecovered, CustomerID: r.CustomerID, At: t2}}
	if err := h.m.OnEvents(ctx, "fake", failed); err != nil {
		t.Fatalf("OnEvents failed: %v", err)
	}
	if err := h.m.OnEvents(ctx, "fake", recovered); err != nil {
		t.Fatalf("OnEvents recovered: %v", err)
	}
	// The stale T1 failure redelivers (lost ACK / partial batch): fenced out.
	if err := h.m.OnEvents(ctx, "fake", failed); err != nil {
		t.Fatalf("OnEvents redelivered: %v", err)
	}
	if r = h.record(t, "acct_1"); r.PastDueSince != nil {
		t.Fatalf("redelivered stale payment_failed re-marked a recovered account: %+v", r)
	}
}

// TestUnroutableEventsFailTheBatch: unknown customers and unknown plans must
// error (so webhooks 500 and the provider redelivers) — a silent ACK would
// drop a possibly-paid event forever.
func TestUnroutableEventsFailTheBatch(t *testing.T) {
	h := newHarness(t, false)
	ctx := context.Background()
	if err := h.m.OnEvents(ctx, "fake", []billing.Event{
		{Type: billing.EventSubscriptionActivated, CustomerID: "fake_cus_9999", Plan: "standard", At: h.ck.t},
	}); err == nil {
		t.Fatal("unknown-customer event must fail the batch, not be ACKed away")
	}
	if _, err := h.m.RequestUpgrade(ctx, "acct_1", "s@example.com", "standard"); err != nil {
		t.Fatalf("RequestUpgrade: %v", err)
	}
	r := h.record(t, "acct_1")
	if err := h.m.OnEvents(ctx, "fake", []billing.Event{
		{Type: billing.EventSubscriptionActivated, CustomerID: r.CustomerID, Plan: "mystery", At: h.ck.t.Add(time.Hour)},
	}); err == nil {
		t.Fatal("unknown-plan activation must fail the batch (deploy skew), not vanish")
	}
}

// TestCancelDisarmsProviderFirst reproduces the review's stuck-state finding:
// if the provider's cancel fails, the record must still show the pending
// change (truthful + retryable) — not "nothing pending" with a schedule
// silently armed at the partner.
func TestCancelDisarmsProviderFirst(t *testing.T) {
	h := newHarness(t, false)
	ctx := context.Background()
	if _, err := h.m.RequestUpgrade(ctx, "acct_1", "s@example.com", "standard"); err != nil {
		t.Fatalf("RequestUpgrade: %v", err)
	}
	if _, err := h.m.RequestDowngrade(ctx, "acct_1", "s@example.com", "free"); err != nil {
		t.Fatalf("RequestDowngrade: %v", err)
	}
	// Simulate the partner API failing the disarm: the fake errors on an
	// unknown customer, so point the record's customer at a vanished id by
	// building a manager whose provider has no such customer.
	other := fake.New(fake.Config{Prices: map[string]int64{"standard": 3000}})
	m2, err := NewManager(Config{
		Catalog: mustCatalog(t), Providers: map[string]billing.Provider{"fake": other}, Default: "fake",
		Store: h.store, Applier: h.applier, Now: h.ck.now,
	})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	if err := m2.CancelPending(ctx, "acct_1"); err == nil {
		t.Fatal("provider disarm failure must surface as an error")
	}
	if r := h.record(t, "acct_1"); r.Pending == nil {
		t.Fatal("provider disarm failed but the local pending was cleared — the stuck state the review found")
	}
	// The original manager (working provider) retries successfully.
	if err := h.m.CancelPending(ctx, "acct_1"); err != nil {
		t.Fatalf("retry after provider recovery: %v", err)
	}
}

func mustCatalog(t *testing.T) *plans.Catalog {
	t.Helper()
	c, err := plans.Load()
	if err != nil {
		t.Fatalf("plans.Load: %v", err)
	}
	return c
}

// TestRefusalSentinel: user refusals carry ErrRefusal; infra errors don't.
func TestRefusalSentinel(t *testing.T) {
	h := newHarness(t, false)
	ctx := context.Background()
	_, err := h.m.RequestUpgrade(ctx, "acct_1", "s@example.com", "nope")
	if !errors.Is(err, ErrRefusal) {
		t.Fatalf("unknown plan = %v; want ErrRefusal", err)
	}
	if err := h.m.CancelPending(ctx, "acct_1"); !errors.Is(err, ErrRefusal) {
		t.Fatalf("nothing-pending = %v; want ErrRefusal", err)
	}
}

// TestApplyIsSerializedPerAccount: concurrent apply() calls for the SAME
// account must not overlap — otherwise a stale snapshot could reach the
// cell after a fresher one. The mutex in Manager.apply is what enforces it.
func TestApplyIsSerializedPerAccount(t *testing.T) {
	h := newHarness(t, false)
	ctx := context.Background()

	// A slow applier that reports overlap if a second call enters while it
	// is in flight.
	var (
		mu       sync.Mutex
		inFlight int
		overlap  bool
	)
	h.applier.calls = nil
	slow := &slowApplier{onApply: func(string) {
		mu.Lock()
		inFlight++
		if inFlight > 1 {
			overlap = true
		}
		mu.Unlock()
		time.Sleep(10 * time.Millisecond)
		mu.Lock()
		inFlight--
		mu.Unlock()
	}}
	// Rebuild the Manager with the slow applier.
	catalog, _ := plans.Load()
	m, err := NewManager(Config{
		Catalog: catalog, Providers: map[string]billing.Provider{"fake": h.fake}, Default: "fake",
		Store: h.store, Applier: slow, Now: h.ck.now,
	})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	if _, err := m.RequestUpgrade(ctx, "acct_1", "s@example.com", "standard"); err != nil {
		t.Fatalf("RequestUpgrade: %v", err)
	}

	// Fire two Reconciles concurrently — both would touch acct_1's apply().
	// With per-account serialization they must NOT overlap.
	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = m.Reconcile(ctx)
		}()
	}
	wg.Wait()
	mu.Lock()
	defer mu.Unlock()
	if overlap {
		t.Fatal("concurrent apply for the same account overlapped — Apply must be serialized per account")
	}
}

type slowApplier struct {
	onApply func(accountID string)
}

func (s *slowApplier) Apply(_ context.Context, accountID string, request ApplyRequest) (ApplyAck, error) {
	s.onApply(accountID)
	return ApplyAck{Revision: request.Revision, Hash: request.Hash}, nil
}
