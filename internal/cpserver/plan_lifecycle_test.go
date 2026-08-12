package cpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/witwave-ai/witself/internal/billing/lifecycle"
	"github.com/witwave-ai/witself/internal/plans"
)

type lifecycleTestApplier struct {
	requests atomic.Int64
}

func (a *lifecycleTestApplier) Apply(
	_ context.Context,
	_ string,
	request lifecycle.ApplyRequest,
) (lifecycle.ApplyAck, error) {
	a.requests.Add(1)
	return lifecycle.ApplyAck{Revision: request.Revision, Hash: request.Hash}, nil
}

type pagedAccountLister struct {
	calls   []string
	byStart map[string]AccountPage
}

func (l *pagedAccountLister) ListActiveAccounts(
	_ context.Context,
	cursor string,
	_ int,
) (AccountPage, error) {
	l.calls = append(l.calls, cursor)
	page, ok := l.byStart[cursor]
	if !ok {
		return AccountPage{}, fmt.Errorf("unexpected cursor")
	}
	return page, nil
}

func providerlessLifecycleManager(t *testing.T) (*lifecycle.Manager, *lifecycle.MemStore, *lifecycleTestApplier) {
	t.Helper()
	catalog, err := plans.Load()
	if err != nil {
		t.Fatal(err)
	}
	store := lifecycle.NewMemStore()
	applier := &lifecycleTestApplier{}
	manager, err := lifecycle.NewManager(lifecycle.Config{
		Catalog: catalog,
		Store:   store,
		Applier: applier,
	})
	if err != nil {
		t.Fatal(err)
	}
	return manager, store, applier
}

func TestReconcileActiveAccountsBoundsWorkAndCarriesCursor(t *testing.T) {
	manager, store, applier := providerlessLifecycleManager(t)
	lister := &pagedAccountLister{byStart: map[string]AccountPage{
		"": {
			AccountIDs: []string{"acct_1", "acct_2"},
			NextCursor: "page-two",
		},
		"page-two": {
			AccountIDs: []string{"acct_3"},
			NextCursor: "",
		},
	}}

	first, cursor, err := ReconcileActiveAccounts(
		context.Background(), manager, lister, "", 2, 1)
	if err != nil {
		t.Fatalf("first reconcile: %v", err)
	}
	if first.Scanned != 2 || first.Seeded != 2 || cursor != "page-two" {
		t.Fatalf("first = %+v cursor=%q", first, cursor)
	}
	if len(lister.calls) != 1 {
		t.Fatalf("first run listed %d pages; want one", len(lister.calls))
	}

	second, cursor, err := ReconcileActiveAccounts(
		context.Background(), manager, lister, cursor, 2, 1)
	if err != nil {
		t.Fatalf("second reconcile: %v", err)
	}
	if second.Scanned != 1 || second.Seeded != 1 || cursor != "" {
		t.Fatalf("second = %+v cursor=%q", second, cursor)
	}
	if len(lister.calls) != 2 || lister.calls[1] != "page-two" {
		t.Fatalf("cursor progression = %v", lister.calls)
	}

	third, cursor, err := ReconcileActiveAccounts(
		context.Background(), manager, lister, cursor, 2, 1)
	if err != nil {
		t.Fatalf("third reconcile: %v", err)
	}
	if third.Seeded != 0 || cursor != "page-two" {
		t.Fatalf("restart = %+v cursor=%q", third, cursor)
	}
	if records, err := store.List(context.Background()); err != nil || len(records) != 3 {
		t.Fatalf("stored records = %d, %v", len(records), err)
	}
	if applier.requests.Load() != 3 {
		t.Fatalf("fenced applies = %d; want one per new account", applier.requests.Load())
	}
}

func TestPlanLifecycleTickIsAuthenticatedBoundedAndValueFree(t *testing.T) {
	manager, store, _ := providerlessLifecycleManager(t)
	catalog, err := plans.Load()
	if err != nil {
		t.Fatal(err)
	}
	observer := NewPlanLifecycleObserver(false)
	mux := http.NewServeMux()
	if err := Register(mux, Config{
		Manager: manager,
		Catalog: catalog,
		Authenticate: func(context.Context, string, string, AccountPermission) (AccountAccess, bool, error) {
			return AccountAccess{}, false, nil
		},
		LifecycleObserver: observer,
		InternalAuthenticate: func(_ context.Context, bearer string) (bool, error) {
			return bearer == "bridge", nil
		},
	}); err != nil {
		t.Fatal(err)
	}

	call := func(token, body string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(
			http.MethodPost,
			"/v1/plan-lifecycle:tick",
			strings.NewReader(body),
		)
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		return rec
	}

	if got := call("", `{"account_ids":[]}`).Code; got != http.StatusUnauthorized {
		t.Fatalf("unauthenticated tick = %d; want 401", got)
	}
	invalid := call("bridge", `{"account_ids":["../registry"]}`)
	if invalid.Code != http.StatusBadRequest ||
		strings.Contains(invalid.Body.String(), "registry") {
		t.Fatalf("invalid tick = %d %s; want value-free 400",
			invalid.Code, invalid.Body.String())
	}

	applied := call("bridge", `{"account_ids":["acct_a","acct_b"]}`)
	if applied.Code != http.StatusOK {
		t.Fatalf("tick = %d %s", applied.Code, applied.Body.String())
	}
	if strings.Contains(applied.Body.String(), "acct_a") ||
		strings.Contains(applied.Body.String(), "acct_b") {
		t.Fatalf("tick response disclosed account ids: %s", applied.Body.String())
	}
	var doc struct {
		SchemaVersion string `json:"schema_version"`
		PlanLifecycle struct {
			Scanned          int                      `json:"scanned"`
			Seeded           int                      `json:"seeded"`
			ApplyPending     int                      `json:"apply_pending"`
			Failed           int                      `json:"failed"`
			Succeeded        bool                     `json:"succeeded"`
			BillingMutations BillingMutationBatchView `json:"billing_mutations"`
		} `json:"plan_lifecycle"`
	}
	if err := json.Unmarshal(applied.Body.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	if doc.SchemaVersion != "witself.v0" ||
		doc.PlanLifecycle.Scanned != 2 ||
		doc.PlanLifecycle.Seeded != 2 ||
		doc.PlanLifecycle.ApplyPending != 0 ||
		doc.PlanLifecycle.Failed != 0 ||
		!doc.PlanLifecycle.Succeeded {
		t.Fatalf("tick response = %+v", doc)
	}
	mutations := doc.PlanLifecycle.BillingMutations
	if mutations.Scanned != 0 || mutations.Attempted != 0 ||
		mutations.Completed != 0 || mutations.Superseded != 0 ||
		mutations.Busy != 0 || mutations.Failed != 0 ||
		mutations.TerminalCleaned != 0 || mutations.ScanCapped ||
		mutations.OldestObservedPendingAt != nil || !mutations.Succeeded {
		t.Fatalf("billing mutation batch = %+v; want successful no-work batch", mutations)
	}
	records, err := store.List(context.Background())
	if err != nil || len(records) != 2 {
		t.Fatalf("stored records = %d, %v", len(records), err)
	}
	status := observer.Snapshot()
	if status.Runs != 1 || status.LastScanned != 2 ||
		status.LastSeeded != 2 || !status.LastSucceeded ||
		status.BillingMutations == nil || !status.BillingMutations.Succeeded {
		t.Fatalf("observer = %+v", status)
	}
}

func TestPlanLifecycleTickContinuesAccountWorkAfterMutationFailure(t *testing.T) {
	manager, store, _ := providerlessLifecycleManager(t)
	catalog, err := plans.Load()
	if err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	now := time.Now().UTC().Add(-time.Hour)
	const (
		accountID   = "acct_recovery"
		operationID = "bop_legacy_unpinned"
	)
	lease, acquired, err := store.ClaimBillingMutationAccount(
		ctx, accountID, operationID, 0, "bcl_seed_legacy",
		now, now.Add(time.Minute))
	if err != nil || !acquired {
		t.Fatalf("seed account mutation lane = %+v acquired=%t err=%v",
			lease, acquired, err)
	}
	if err := store.ReleaseBillingMutationAccount(
		ctx, lease, now.Add(time.Second)); err != nil {
		t.Fatalf("release seed account mutation lane: %v", err)
	}
	if _, created, err := store.ReceiveBillingMutation(ctx,
		lifecycle.BillingMutationReceipt{
			SchemaVersion:        1,
			OperationID:          operationID,
			AccountID:            accountID,
			ActorID:              "opr_legacy",
			ActorRole:            "account_owner",
			Operation:            lifecycle.BillingMutationPlanUpgrade,
			AccountGeneration:    lease.OperationGeneration,
			IdempotencyKeySHA256: strings.Repeat("a", 64),
			RequestSHA256:        strings.Repeat("b", 64),
			Reason:               "legacy recovery safety test",
			ConfirmedAt:          now,
			TargetPlan:           "standard",
			Status:               lifecycle.BillingMutationPending,
			CreatedAt:            now,
			UpdatedAt:            now,
		}); err != nil || !created {
		t.Fatalf("seed legacy receipt created=%t err=%v", created, err)
	}

	observer := NewPlanLifecycleObserver(false)
	mux := http.NewServeMux()
	if err := Register(mux, Config{
		Manager: manager,
		Catalog: catalog,
		Authenticate: func(context.Context, string, string, AccountPermission) (AccountAccess, bool, error) {
			return AccountAccess{}, false, nil
		},
		LifecycleObserver: observer,
		InternalAuthenticate: func(_ context.Context, bearer string) (bool, error) {
			return bearer == "bridge", nil
		},
	}); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost,
		"/v1/plan-lifecycle:tick",
		strings.NewReader(`{"account_ids":["acct_recovery"]}`))
	req.Header.Set("Authorization", "Bearer bridge")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("tick = %d %s", rec.Code, rec.Body.String())
	}
	var doc struct {
		PlanLifecycle struct {
			Seeded           int                      `json:"seeded"`
			Succeeded        bool                     `json:"succeeded"`
			BillingMutations BillingMutationBatchView `json:"billing_mutations"`
		} `json:"plan_lifecycle"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	if doc.PlanLifecycle.Seeded != 1 || doc.PlanLifecycle.Succeeded {
		t.Fatalf("account work did not continue after mutation failure: %+v", doc)
	}
	mutations := doc.PlanLifecycle.BillingMutations
	if mutations.Scanned != 1 || mutations.Attempted != 1 ||
		mutations.Failed != 1 || mutations.Succeeded {
		t.Fatalf("mutation failure projection = %+v", mutations)
	}
	if status := observer.Snapshot(); status.LastSucceeded {
		t.Fatalf("overall observer incorrectly succeeded: %+v", status)
	}
	if _, ok, err := store.Get(ctx, accountID); err != nil || !ok {
		t.Fatalf("account baseline was not persisted: ok=%t err=%v", ok, err)
	}
	pending, ok, err := store.GetBillingMutation(ctx, operationID)
	if err != nil || !ok || pending.Status != lifecycle.BillingMutationPending ||
		pending.ClaimToken != "" {
		t.Fatalf("legacy receipt was not safely left pending: %+v ok=%t err=%v",
			pending, ok, err)
	}
}

func TestPlanLifecycleStatusAndMetricsAreAuthenticatedNestedAndValueFree(t *testing.T) {
	manager, _, _ := providerlessLifecycleManager(t)
	catalog, err := plans.Load()
	if err != nil {
		t.Fatal(err)
	}
	observer := NewPlanLifecycleObserver(true)
	mux := http.NewServeMux()
	if err := Register(mux, Config{
		Manager: manager,
		Catalog: catalog,
		Authenticate: func(context.Context, string, string, AccountPermission) (AccountAccess, bool, error) {
			return AccountAccess{}, false, nil
		},
		LifecycleObserver: observer,
		InternalAuthenticate: func(_ context.Context, bearer string) (bool, error) {
			return bearer == "bridge", nil
		},
	}); err != nil {
		t.Fatal(err)
	}

	call := func(method, path, token string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(method, path, nil)
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		return rec
	}

	for _, path := range []string{
		"/v1/plan-lifecycle/status",
		"/v1/plan-lifecycle/metrics",
	} {
		if got := call(http.MethodGet, path, "").Code; got != http.StatusUnauthorized {
			t.Fatalf("%s without token = %d; want 401", path, got)
		}
		if got := call(http.MethodGet, path, "wrong").Code; got != http.StatusForbidden {
			t.Fatalf("%s with bad token = %d; want 403", path, got)
		}
	}
	if got := call(http.MethodPost, "/v1/plan-lifecycle/metrics", "bridge").Code; got != http.StatusMethodNotAllowed {
		t.Fatalf("POST metrics = %d; want 405", got)
	}

	before := call(http.MethodGet, "/v1/plan-lifecycle/status", "bridge")
	if before.Code != http.StatusOK || strings.Contains(before.Body.String(), "billing_mutations") {
		t.Fatalf("status before first batch = %d %s; want no batch projection",
			before.Code, before.Body.String())
	}
	zeroMetrics := call(http.MethodGet, "/v1/plan-lifecycle/metrics", "bridge")
	if zeroMetrics.Code != http.StatusOK {
		t.Fatalf("initial metrics = %d %s", zeroMetrics.Code, zeroMetrics.Body.String())
	}
	if got := zeroMetrics.Header().Get("Content-Type"); got != "text/plain; version=0.0.4; charset=utf-8" {
		t.Fatalf("metrics content type = %q", got)
	}
	for _, want := range []string{
		`witself_control_plane_billing_mutation_reconciliation_batches_total{result="success"} 0`,
		`witself_control_plane_billing_mutation_reconciliation_batches_total{result="error"} 0`,
		`witself_control_plane_billing_mutation_reconciliation_items_total{kind="scanned"} 0`,
		`witself_control_plane_billing_mutation_reconciliation_last_batch_items{kind="terminal_cleaned"} 0`,
		`witself_control_plane_billing_mutation_reconciliation_last_success_timestamp_seconds 0`,
		`witself_control_plane_billing_mutation_reconciliation_oldest_observed_pending_timestamp_seconds 0`,
	} {
		if !strings.Contains(zeroMetrics.Body.String(), want) {
			t.Errorf("initial metrics missing %q:\n%s", want, zeroMetrics.Body.String())
		}
	}

	oldest := time.Unix(1000, 0).UTC()
	observer.begin(time.Unix(1900, 0))
	observer.complete(time.Unix(2000, 0), PlanLifecycleSummary{
		BillingMutations: BillingMutationBatchView{
			Scanned: 7, Attempted: 3, Completed: 2, Superseded: 1,
			Busy: 2, TerminalCleaned: 2, ScanCapped: true,
			OldestObservedPendingAt: &oldest, Succeeded: true,
		},
	}, true)

	// Snapshot callers must not be able to mutate the observer's nested state.
	copyOne := observer.Snapshot()
	copyOne.BillingMutations.Scanned = 999
	*copyOne.BillingMutations.OldestObservedPendingAt = time.Unix(9999, 0)
	copyTwo := observer.Snapshot()
	if copyTwo.BillingMutations == nil || copyTwo.BillingMutations.Scanned != 7 ||
		copyTwo.BillingMutations.OldestObservedPendingAt == nil ||
		copyTwo.BillingMutations.OldestObservedPendingAt.Unix() != 1000 {
		t.Fatalf("observer nested snapshot was aliased: %+v", copyTwo.BillingMutations)
	}

	observer.begin(time.Unix(2900, 0))
	observer.complete(time.Unix(3000, 0), PlanLifecycleSummary{
		BillingMutations: BillingMutationBatchView{
			Scanned: 2, Attempted: 1, Failed: 1, TerminalCleaned: 1,
			Succeeded: false,
		},
	}, false)

	status := call(http.MethodGet, "/v1/plan-lifecycle/status", "bridge")
	if status.Code != http.StatusOK {
		t.Fatalf("status = %d %s", status.Code, status.Body.String())
	}
	var statusDoc struct {
		SchemaVersion string `json:"schema_version"`
		PlanLifecycle struct {
			LastSucceeded    bool                      `json:"last_succeeded"`
			BillingMutations *BillingMutationBatchView `json:"billing_mutations"`
		} `json:"plan_lifecycle"`
	}
	if err := json.Unmarshal(status.Body.Bytes(), &statusDoc); err != nil {
		t.Fatal(err)
	}
	if statusDoc.SchemaVersion != "witself.v0" ||
		statusDoc.PlanLifecycle.LastSucceeded ||
		statusDoc.PlanLifecycle.BillingMutations == nil ||
		statusDoc.PlanLifecycle.BillingMutations.Scanned != 2 ||
		statusDoc.PlanLifecycle.BillingMutations.Failed != 1 ||
		statusDoc.PlanLifecycle.BillingMutations.OldestObservedPendingAt != nil ||
		statusDoc.PlanLifecycle.BillingMutations.Succeeded {
		t.Fatalf("nested status = %+v", statusDoc)
	}
	if !strings.Contains(status.Body.String(), `"oldest_observed_pending_at":null`) {
		t.Fatalf("status must explicitly reset oldest observed pending time: %s",
			status.Body.String())
	}

	metrics := call(http.MethodGet, "/v1/plan-lifecycle/metrics", "bridge")
	if metrics.Code != http.StatusOK {
		t.Fatalf("metrics = %d %s", metrics.Code, metrics.Body.String())
	}
	for _, want := range []string{
		`witself_control_plane_billing_mutation_reconciliation_batches_total{result="success"} 1`,
		`witself_control_plane_billing_mutation_reconciliation_batches_total{result="error"} 1`,
		`witself_control_plane_billing_mutation_reconciliation_items_total{kind="scanned"} 9`,
		`witself_control_plane_billing_mutation_reconciliation_items_total{kind="attempted"} 4`,
		`witself_control_plane_billing_mutation_reconciliation_items_total{kind="completed"} 2`,
		`witself_control_plane_billing_mutation_reconciliation_items_total{kind="superseded"} 1`,
		`witself_control_plane_billing_mutation_reconciliation_items_total{kind="busy"} 2`,
		`witself_control_plane_billing_mutation_reconciliation_items_total{kind="failed"} 1`,
		`witself_control_plane_billing_mutation_reconciliation_items_total{kind="terminal_cleaned"} 3`,
		`witself_control_plane_billing_mutation_reconciliation_last_batch_items{kind="scanned"} 2`,
		`witself_control_plane_billing_mutation_reconciliation_last_batch_items{kind="failed"} 1`,
		`witself_control_plane_billing_mutation_reconciliation_scan_capped_batches_total 1`,
		`witself_control_plane_billing_mutation_reconciliation_last_batch_scan_capped 0`,
		`witself_control_plane_billing_mutation_reconciliation_last_success_timestamp_seconds 2000`,
		`witself_control_plane_billing_mutation_reconciliation_oldest_observed_pending_timestamp_seconds 0`,
	} {
		if !strings.Contains(metrics.Body.String(), want) {
			t.Errorf("metrics missing %q:\n%s", want, metrics.Body.String())
		}
	}
	for _, forbidden := range []string{
		"acct_private", "bop_private", "stripe", "provider error",
	} {
		if strings.Contains(metrics.Body.String(), forbidden) {
			t.Errorf("metrics exposed forbidden value %q:\n%s",
				forbidden, metrics.Body.String())
		}
	}
}

func TestProviderlessRoutesKeepStatusAndAdminButHideBillingMutations(t *testing.T) {
	manager, _, _ := providerlessLifecycleManager(t)
	catalog, err := plans.Load()
	if err != nil {
		t.Fatal(err)
	}
	observer := NewPlanLifecycleObserver(false)
	mux := http.NewServeMux()
	if err := Register(mux, Config{
		Manager: manager,
		Catalog: catalog,
		Authenticate: func(_ context.Context, accountID, bearer string, permission AccountPermission) (AccountAccess, bool, error) {
			ok := accountID == "acct_1" && bearer == "owner"
			return AccountAccess{ActorID: "opr_owner", Role: "account_owner", Permission: permission}, ok, nil
		},
		AdminAuthenticate: func(
			_ context.Context,
			bearer, adminID, handle string,
		) (lifecycle.AdminActor, bool, error) {
			return lifecycle.AdminActor{ID: adminID, Handle: handle},
				bearer == "bridge" &&
					adminID == "adm_abcdefghijklmnopqrst" &&
					handle == "scott", nil
		},
		AdminAccountExists: func(context.Context, string) (bool, error) {
			return true, nil
		},
		LifecycleObserver: observer,
		InternalAuthenticate: func(_ context.Context, bearer string) (bool, error) {
			return bearer == "bridge", nil
		},
	}); err != nil {
		t.Fatal(err)
	}

	call := func(method, path, token string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(method, path, nil)
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		return rec
	}

	status := call(http.MethodGet, "/v1/accounts/acct_1/plan", "owner")
	if status.Code != http.StatusOK {
		t.Fatalf("status = %d %s", status.Code, status.Body.String())
	}
	var doc map[string]any
	if err := json.Unmarshal(status.Body.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	if doc["billing_available"] != false || doc["plan"] != plans.Free {
		t.Fatalf("providerless status = %v", doc)
	}
	if got := call(http.MethodPost, "/v1/accounts/acct_1/plan:upgrade", "owner").Code; got != http.StatusNotFound {
		t.Fatalf("providerless upgrade = %d; want 404", got)
	}
	if got := call(http.MethodGet, "/v1/admin/accounts/acct_1/transcript-retention", "bridge").Code; got != http.StatusForbidden {
		// The bridge credential alone is insufficient: Worker must also
		// supply the verified immutable admin id and display handle.
		t.Fatalf("admin without asserted identity = %d; want 403", got)
	}
	if got := call(http.MethodGet, "/v1/plan-lifecycle/status", "bridge").Code; got != http.StatusOK {
		t.Fatalf("internal status = %d; want 200", got)
	}
	if got := call(http.MethodGet, "/v1/plan-lifecycle/status", "bad").Code; got != http.StatusForbidden {
		t.Fatalf("internal status bad token = %d; want 403", got)
	}
}
