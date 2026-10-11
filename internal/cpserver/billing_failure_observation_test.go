package cpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/witwave-ai/witself/internal/billing"
	"github.com/witwave-ai/witself/internal/billing/fake"
	"github.com/witwave-ai/witself/internal/billing/lifecycle"
	"github.com/witwave-ai/witself/internal/plans"
)

func TestWebhookProcessingFailureCountsOnlyStripe(t *testing.T) {
	catalog, err := plans.Load()
	if err != nil {
		t.Fatal(err)
	}
	provider := fake.New(fake.Config{Prices: catalog.Prices()})
	store := &stripeObservationStore{MemStore: lifecycle.NewMemStore(), failReceive: true}
	manager, err := lifecycle.NewManager(lifecycle.Config{
		Catalog: catalog, Providers: map[string]billing.Provider{"fake": provider},
		Default: "fake", Store: store, Applier: noopApplier{},
	})
	if err != nil {
		t.Fatal(err)
	}
	observer := NewPlanLifecycleObserver(true)
	// The fake emits legacy events without receipt identities, so this fold
	// fails at unknown-customer resolution rather than ReceiveEvent.
	req := httptest.NewRequest(http.MethodPost, "/v1/billing/webhook/fake",
		strings.NewReader(`{"customer_id":"private_fixture","type":"payment_failed"}`))
	rec := httptest.NewRecorder()
	webhook(Config{Manager: manager, LifecycleObserver: observer}, "fake", provider)(rec, req)
	if rec.Code != http.StatusInternalServerError || rec.Body.String() != "{\"error\":\"could not process events\",\"schema_version\":\"witself.v0\"}\n" {
		t.Fatalf("non-Stripe fold response = %d %s", rec.Code, rec.Body.String())
	}
	if got := observer.StripeSnapshot(time.Now(), 0).WebhookEvents; got != (StripeWebhookEvents{}) {
		t.Fatalf("non-Stripe fold affected Stripe counters: %+v", got)
	}
}

func TestStripeObservationV2ContractFixture(t *testing.T) {
	now := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	oldest := now.Add(-20 * time.Minute)
	observer := NewPlanLifecycleObserver(true)
	for range 7 {
		observer.observeStripeVerification(true)
	}
	for range 2 {
		observer.observeStripeVerification(false)
	}
	observer.observeStripeProcessingFailure()
	observer.complete(now, PlanLifecycleSummary{
		Failed: 1, Blocked: 3, BillingMutations: BillingMutationBatchView{Succeeded: false},
	}, false)
	observer.complete(now, PlanLifecycleSummary{
		Blocked: 3, BillingMutations: BillingMutationBatchView{Succeeded: false, Failed: 1},
	}, false)
	observer.complete(now, PlanLifecycleSummary{
		Failed: 1, BillingMutations: BillingMutationBatchView{Succeeded: true, OldestObservedPendingAt: &oldest},
	}, false)
	got, err := json.Marshal(observer.StripeSnapshot(now, 3))
	if err != nil {
		t.Fatal(err)
	}
	fixture, err := os.ReadFile("testdata/stripe-observation-v2.json")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(append(got, '\n'), fixture) {
		t.Fatalf("observation differs from schema 2 contract fixture: %s", got)
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(fixture, &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc) != 9 {
		t.Fatalf("schema 2 top-level keys = %d; want 9", len(doc))
	}
	var events map[string]json.RawMessage
	if err := json.Unmarshal(doc["webhook_events"], &events); err != nil {
		t.Fatal(err)
	}
	if len(events) != 4 {
		t.Fatalf("schema 2 webhook event keys = %d; want 4", len(events))
	}
}

func TestStripeObservationMutationBatchFailuresDistinctFromRejections(t *testing.T) {
	now := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	observer := NewPlanLifecycleObserver(true)
	for range 3 {
		observer.observeStripeVerification(false)
	}
	for range 2 {
		observer.complete(now, PlanLifecycleSummary{
			BillingMutations: BillingMutationBatchView{Succeeded: false},
		}, false)
	}
	got := observer.StripeSnapshot(now, 0)
	if got.WebhookEvents.Rejected != 3 || got.MutationBatchFailures != 2 {
		t.Fatalf("independent rejection and batch failure counters = %+v", got)
	}
}

type emptyApplyBlockedJoin struct{}

func (emptyApplyBlockedJoin) Error() string   { return "empty join" }
func (emptyApplyBlockedJoin) Unwrap() []error { return nil }

func TestApplyBlockedOnlyClassification(t *testing.T) {
	blocked := fmt.Errorf("%w: r", lifecycle.ErrApplyBlocked)
	for _, test := range []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"sentinel", lifecycle.ErrApplyBlocked, true},
		{"blocked", blocked, true},
		{"wrapped blocked", fmt.Errorf("ctx: %w", blocked), true},
		{"blocked and read failure", errors.Join(blocked, errors.New("read")), false},
		{"wrapped blocked and read failure", fmt.Errorf("ctx: %w", errors.Join(blocked, errors.New("read"))), false},
		{"wrapped all blocked join", fmt.Errorf("ctx: %w", errors.Join(blocked, blocked)), true},
		{"missing atomic capability", errors.New("plan apply blocked: atomic downgrade fit-and-apply is unavailable"), false},
		{"other", errors.New("other"), false},
		{"empty join", emptyApplyBlockedJoin{}, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := applyBlockedOnly(test.err); got != test.want {
				t.Fatalf("classification = %t; want %t", got, test.want)
			}
		})
	}
}

func blockedObservationTickFixture(t *testing.T, applier lifecycle.Applier) (*lifecycle.Manager, *lifecycle.MemStore, *PlanLifecycleObserver, http.Handler) {
	t.Helper()
	catalog, err := plans.Load()
	if err != nil {
		t.Fatal(err)
	}
	store := lifecycle.NewMemStore()
	manager, err := lifecycle.NewManager(lifecycle.Config{Catalog: catalog, Store: store, Applier: applier})
	if err != nil {
		t.Fatal(err)
	}
	observer := NewPlanLifecycleObserver(false)
	mux := http.NewServeMux()
	if err := Register(mux, Config{
		Manager: manager, Catalog: catalog, LifecycleObserver: observer,
		Authenticate: func(context.Context, string, string, AccountPermission) (AccountAccess, bool, error) {
			return AccountAccess{}, false, nil
		},
		InternalAuthenticate: func(_ context.Context, token string) (bool, error) { return token == "fixture", nil },
	}); err != nil {
		t.Fatal(err)
	}
	return manager, store, observer, mux
}

func callBlockedObservationTick(t *testing.T, handler http.Handler, accountIDs ...string) map[string]any {
	t.Helper()
	body, err := json.Marshal(map[string]any{"account_ids": accountIDs})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/plan-lifecycle:tick",
		bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer fixture")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("tick response = %d", rec.Code)
	}
	var doc struct {
		Lifecycle map[string]any `json:"plan_lifecycle"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	return doc.Lifecycle
}

func TestPlanLifecycleTickBlockedDowngradeIsPendingNotFailed(t *testing.T) {
	manager, store, observer, handler := blockedObservationTickFixture(t, &deliveryObservationApplier{mode: "blocked"})
	const accountID = "acct_blocked_fixture"
	if err := store.Put(context.Background(), lifecycle.Record{AccountID: accountID, Entitled: plans.Free, Applied: "team"}); err != nil {
		t.Fatal(err)
	}
	for visit := uint64(1); visit <= 2; visit++ {
		got := callBlockedObservationTick(t, handler, accountID)
		if got["scanned"] != float64(1) || got["apply_pending"] != float64(1) || got["failed"] != float64(0) || got["succeeded"] != true {
			t.Fatalf("blocked tick aggregate = %+v", got)
		}
		keys := make([]string, 0, len(got))
		for key := range got {
			keys = append(keys, key)
		}
		slices.Sort(keys)
		wantKeys := []string{"apply_pending", "billing_mutations", "failed", "scanned", "seeded", "succeeded"}
		if !reflect.DeepEqual(keys, wantKeys) {
			t.Fatalf("tick acknowledgement keys = %v; want %v", keys, wantKeys)
		}
		observation := observer.StripeSnapshot(time.Now(), 0)
		if observation.EntitlementApplyBlocked != visit || observation.ReconciliationFailures != 0 {
			t.Fatalf("blocked observation = %+v", observation)
		}
		status := observer.Snapshot()
		if status.LastFailed != 0 || !status.LastSucceeded {
			t.Fatalf("blocked lifecycle status = %+v", status)
		}
	}
	record, snapshot, err := manager.ResolvedStatus(context.Background(), accountID, "")
	if err != nil {
		t.Fatal(err)
	}
	if record.ApplyBlocked == "" || !lifecycle.SnapshotApplyPending(record, snapshot) {
		t.Fatal("blocked account did not retain its pending refusal")
	}
}

func TestPlanLifecycleTickBlockedRestoreWithAcknowledgedSnapshotStillFails(t *testing.T) {
	applier := &deliveryObservationApplier{mode: "blocked"}
	manager, store, observer, handler := blockedObservationTickFixture(t, applier)
	ctx := context.Background()
	const accountID = "acct_restore_fixture"
	if err := store.Put(ctx, lifecycle.Record{AccountID: accountID, Entitled: plans.Free, Applied: plans.Free}); err != nil {
		t.Fatal(err)
	}
	record, snapshot, err := manager.ResolvedStatus(ctx, accountID, "")
	if err != nil {
		t.Fatal(err)
	}
	// Mirror A-T6(b): the CP acknowledges revision 18, but the restored cell
	// has revision 15. The unchanged plan needs a conditional reapply without
	// minting a revision or changing the CP's snapshot-pending classification.
	record.SnapshotRevision = 18
	record.DesiredSnapshotHash = snapshot.Hash
	record.AppliedSnapshotRevision = 18
	record.AppliedSnapshotHash = snapshot.Hash
	applier.fence = lifecycle.ApplyFence{Revision: 15, Hash: snapshot.Hash}
	if lifecycle.SnapshotApplyPending(record, snapshot) {
		t.Fatal("restore fixture unexpectedly has a pending snapshot")
	}
	if err := store.Put(ctx, record); err != nil {
		t.Fatal(err)
	}
	got := callBlockedObservationTick(t, handler, accountID)
	if got["scanned"] != float64(1) || got["apply_pending"] != float64(0) || got["failed"] != float64(1) || got["succeeded"] != false {
		t.Fatalf("acknowledged restore refusal aggregate = %+v", got)
	}
	if observation := observer.StripeSnapshot(time.Now(), 0); observation.EntitlementApplyBlocked != 0 || observation.ReconciliationFailures != 1 {
		t.Fatalf("acknowledged restore refusal observation = %+v", observation)
	}
	if status := observer.Snapshot(); status.LastFailed != 1 || status.LastSucceeded {
		t.Fatalf("acknowledged restore refusal status = %+v", status)
	}
	record, snapshot, err = manager.ResolvedStatus(ctx, accountID, "")
	if err != nil {
		t.Fatal(err)
	}
	if record.ApplyBlocked == "" || lifecycle.SnapshotApplyPending(record, snapshot) ||
		record.SnapshotRevision != 18 || record.AppliedSnapshotRevision != 18 || applier.fence.Revision != 15 {
		t.Fatal("restore refusal changed acknowledged revisions, cell fence or pending semantics")
	}
}

// Each account gets its own transport state because a page reconciles accounts
// concurrently. The map is fixed before reconciliation starts.
type accountDeliveryObservationApplier map[string]*deliveryObservationApplier

func (a accountDeliveryObservationApplier) ReadApplyFence(ctx context.Context, id string) (lifecycle.ApplyFence, error) {
	return a[id].ReadApplyFence(ctx, id)
}

func (a accountDeliveryObservationApplier) Apply(ctx context.Context, id string, request lifecycle.ApplyRequest) (lifecycle.ApplyAck, error) {
	return a[id].Apply(ctx, id, request)
}

func (a accountDeliveryObservationApplier) ApplyIfFits(ctx context.Context, id string, request lifecycle.ApplyRequest) (lifecycle.ConditionalApplyResult, error) {
	return a[id].ApplyIfFits(ctx, id, request)
}

func TestPlanLifecycleTickMixedBlockedAndDeliveryFailure(t *testing.T) {
	const blockedID = "acct_blocked_fixture"
	const undeliveredID = "acct_undelivered_fixture"
	applier := accountDeliveryObservationApplier{
		blockedID:     {mode: "blocked"},
		undeliveredID: {mode: "never-delivered"},
	}
	manager, store, observer, handler := blockedObservationTickFixture(t, applier)
	if err := store.Put(context.Background(), lifecycle.Record{AccountID: blockedID, Entitled: plans.Free, Applied: "team"}); err != nil {
		t.Fatal(err)
	}
	got := callBlockedObservationTick(t, handler, blockedID, undeliveredID)
	if got["scanned"] != float64(2) || got["apply_pending"] != float64(2) || got["failed"] != float64(1) || got["succeeded"] != false {
		t.Fatalf("mixed page aggregate = %+v", got)
	}
	if observation := observer.StripeSnapshot(time.Now(), 0); observation.EntitlementApplyBlocked != 1 || observation.ReconciliationFailures != 1 {
		t.Fatalf("mixed page observation = %+v", observation)
	}
	if status := observer.Snapshot(); status.LastFailed != 1 || status.LastSucceeded {
		t.Fatalf("mixed page lifecycle status = %+v", status)
	}
	summary, err := ReconcileAccountIDs(context.Background(), manager, []string{blockedID, undeliveredID}, 2)
	if err == nil || summary.Scanned != 2 || summary.ApplyPending != 2 || summary.Blocked != 1 || summary.Failed != 1 {
		t.Fatalf("mixed page reconciliation = %+v; error=%v", summary, err)
	}
}

func TestPlanLifecycleTickMissingAtomicApplyStillFails(t *testing.T) {
	_, store, observer, handler := blockedObservationTickFixture(t, &lifecycleTestApplier{})
	const accountID = "acct_no_atomic_fixture"
	if err := store.Put(context.Background(), lifecycle.Record{AccountID: accountID, Entitled: plans.Free, Applied: "team"}); err != nil {
		t.Fatal(err)
	}
	got := callBlockedObservationTick(t, handler, accountID)
	if got["failed"] != float64(1) || got["succeeded"] != false {
		t.Fatalf("missing atomic capability aggregate = %+v", got)
	}
	if observation := observer.StripeSnapshot(time.Now(), 0); observation.EntitlementApplyBlocked != 0 || observation.ReconciliationFailures != 1 {
		t.Fatalf("missing atomic capability observation = %+v", observation)
	}
}

func TestPlanLifecycleReconcileActiveAccountsCountsBlockedAcrossPages(t *testing.T) {
	manager, store, observer, _ := blockedObservationTickFixture(t, &deliveryObservationApplier{mode: "blocked"})
	for _, id := range []string{"acct_first_fixture", "acct_second_fixture"} {
		if err := store.Put(context.Background(), lifecycle.Record{AccountID: id, Entitled: plans.Free, Applied: "team"}); err != nil {
			t.Fatal(err)
		}
	}
	lister := &pagedAccountLister{byStart: map[string]AccountPage{
		"":     {AccountIDs: []string{"acct_first_fixture"}, NextCursor: "next"},
		"next": {AccountIDs: []string{"acct_second_fixture"}},
	}}
	summary, cursor, err := ReconcileActiveAccounts(context.Background(), manager, lister, "", 1, 2)
	if err != nil || cursor != "" || summary.Scanned != 2 || summary.ApplyPending != 2 || summary.Blocked != 2 || summary.Failed != 0 {
		t.Fatalf("blocked traversal = %+v; completed=%t; error=%v", summary, cursor == "", err)
	}
	observer.complete(time.Now(), summary, err == nil)
	if observation := observer.StripeSnapshot(time.Now(), 0); observation.EntitlementApplyBlocked != 2 || observation.ReconciliationFailures != 0 {
		t.Fatalf("blocked traversal observation = %+v", observation)
	}
}
