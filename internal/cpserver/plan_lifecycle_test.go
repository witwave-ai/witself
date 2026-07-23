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
		Authenticate: func(context.Context, string, string) (bool, error) {
			return false, nil
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
			Scanned      int  `json:"scanned"`
			Seeded       int  `json:"seeded"`
			ApplyPending int  `json:"apply_pending"`
			Failed       int  `json:"failed"`
			Succeeded    bool `json:"succeeded"`
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
	records, err := store.List(context.Background())
	if err != nil || len(records) != 2 {
		t.Fatalf("stored records = %d, %v", len(records), err)
	}
	status := observer.Snapshot()
	if status.Runs != 1 || status.LastScanned != 2 ||
		status.LastSeeded != 2 || !status.LastSucceeded {
		t.Fatalf("observer = %+v", status)
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
		Authenticate: func(_ context.Context, accountID, bearer string) (bool, error) {
			return accountID == "acct_1" && bearer == "owner", nil
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
