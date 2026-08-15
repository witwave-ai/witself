package client_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/witwave-ai/witself/internal/billing"
	"github.com/witwave-ai/witself/internal/billing/fake"
	"github.com/witwave-ai/witself/internal/billing/lifecycle"
	"github.com/witwave-ai/witself/internal/client"
	"github.com/witwave-ai/witself/internal/cpserver"
	"github.com/witwave-ai/witself/internal/plans"
)

type billingRoundTripFunc func(*http.Request) (*http.Response, error)

func (f billingRoundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

type noopApplier struct{}

func (noopApplier) Apply(_ context.Context, _ string, request lifecycle.ApplyRequest) (lifecycle.ApplyAck, error) {
	return lifecycle.ApplyAck{Revision: request.Revision, Hash: request.Hash}, nil
}

// TestCLIPlanFlowAgainstCPServer wires the CP HTTP server, then drives the
// full plan flow through the CLIENT (the same functions the CLI verbs call).
// This is the belt-and-suspenders across the whole billing arc: catalog → CP
// → Manager → Store → client → CLI shape.
func TestCLIPlanFlowAgainstCPServer(t *testing.T) {
	catalog, err := plans.Load()
	if err != nil {
		t.Fatalf("plans.Load: %v", err)
	}
	f := fake.New(fake.Config{Prices: catalog.Prices()})
	providers := map[string]billing.Provider{"fake": f}
	m, err := lifecycle.NewManager(lifecycle.Config{
		Catalog: catalog, Providers: providers, Default: "fake",
		Store: lifecycle.NewMemStore(), Applier: noopApplier{},
	})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	mux := http.NewServeMux()
	if err := cpserver.Register(mux, cpserver.Config{
		Manager: m, Catalog: catalog, Providers: providers,
		Authenticate: func(_ context.Context, accountID, bearer string, permission cpserver.AccountPermission) (cpserver.AccountAccess, bool, error) {
			ok := bearer == "good" && accountID == "acct_1"
			return cpserver.AccountAccess{ActorID: "opr_owner", Role: "account_owner", Permission: permission}, ok, nil
		},
	}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	ctx := context.Background()

	status, err := client.GetPlan(ctx, srv.URL, "acct_1", "good")
	if err != nil || status.Plan != "free" || status.PlanName != "Personal" ||
		status.BillingPlan != "free" || status.BillingPlanName != "Personal" ||
		status.EmailReceive == nil || status.EmailReceive.Enabled ||
		status.EmailSend == nil || status.EmailSend.Enabled ||
		status.EmailRetention == nil || status.EmailRetention.EffectiveDays == nil ||
		*status.EmailRetention.EffectiveDays != 30 ||
		status.Transcript == nil || status.Transcript.EffectiveDays == nil ||
		*status.Transcript.EffectiveDays != 30 {
		t.Fatalf("GetPlan initial = %+v, %v; want free", status, err)
	}
	catalogView, err := client.GetPlanCatalog(ctx, srv.URL)
	if err != nil {
		t.Fatalf("GetPlanCatalog: %v", err)
	}
	professional, ok := catalogView.Get("standard")
	if !ok || !professional.Recommended || professional.Badge != "Most popular" {
		t.Fatalf("professional catalog view = %+v, present=%t", professional, ok)
	}

	preview, err := client.PreviewBillingMutation(
		ctx, srv.URL, "acct_1", "good", client.BillingMutationUpgrade,
		"standard", "s@example.com", "Move to Professional")
	if err != nil || !preview.Allowed || !preview.ConfirmationRequired ||
		preview.Operation != client.BillingMutationUpgrade || preview.Plan != "standard" {
		t.Fatalf("upgrade preview = %+v, %v; want allowed", preview, err)
	}
	out, err := client.UpgradePlan(
		ctx, srv.URL, "acct_1", "good", "standard", "s@example.com",
		client.BillingMutationOptions{
			Reason: "Move to Professional", Confirmed: true,
			IdempotencyKey: "test-upgrade-standard",
		})
	if err != nil || out.Kind != "done" || out.Plan != "standard" {
		t.Fatalf("UpgradePlan = %+v, %v; want done+standard", out, err)
	}
	if out.OperationID == "" || out.Operation != client.BillingMutationUpgrade ||
		out.ActorID != "opr_owner" || out.ActorRole != "account_owner" ||
		!out.Confirmed || out.Replayed {
		t.Fatalf("UpgradePlan metadata = %+v", out)
	}
	replayed, err := client.UpgradePlan(
		ctx, srv.URL, "acct_1", "good", "standard", "s@example.com",
		client.BillingMutationOptions{
			Reason: "Move to Professional", Confirmed: true,
			IdempotencyKey: "test-upgrade-standard",
		})
	if err != nil || !replayed.Replayed || replayed.OperationID != out.OperationID {
		t.Fatalf("UpgradePlan replay = %+v, %v", replayed, err)
	}

	status, err = client.GetPlan(ctx, srv.URL, "acct_1", "good")
	if err != nil || status.Plan != "standard" || status.PlanName != "Professional" ||
		status.Applied != "standard" || status.EmailReceive == nil || !status.EmailReceive.Enabled ||
		status.EmailSend == nil || status.EmailSend.Enabled ||
		status.EmailRetention == nil || status.EmailRetention.EffectiveDays == nil ||
		*status.EmailRetention.EffectiveDays != 90 {
		t.Fatalf("GetPlan after = %+v; want standard/standard", status)
	}

	if _, err := client.DowngradePlan(
		ctx, srv.URL, "acct_1", "good", "free", "",
		client.BillingMutationOptions{
			Reason: "Return to Personal", Confirmed: true,
			IdempotencyKey: "test-downgrade-free",
		}); err != nil {
		t.Fatalf("DowngradePlan: %v", err)
	}
	status, _ = client.GetPlan(ctx, srv.URL, "acct_1", "good")
	if status.Pending == nil || status.Pending.Kind != "downgrade" {
		t.Fatalf("status.Pending = %+v; want scheduled downgrade", status.Pending)
	}

	cancelled, err := client.CancelPlanChange(
		ctx, srv.URL, "acct_1", "good", client.BillingMutationOptions{
			Reason: "Keep Professional", Confirmed: true,
			IdempotencyKey: "test-cancel-downgrade",
		})
	if err != nil || cancelled.Kind != "cancelled" ||
		cancelled.Operation != client.BillingMutationCancel {
		t.Fatalf("CancelPlanChange = %+v, %v", cancelled, err)
	}
	status, _ = client.GetPlan(ctx, srv.URL, "acct_1", "good")
	if status.Pending != nil {
		t.Fatalf("pending survived cancel: %+v", status.Pending)
	}

	// A refusal from the Manager surfaces through the client with the
	// message intact (409 → error text preserved).
	if _, err := client.UpgradePlan(
		ctx, srv.URL, "acct_1", "good", "standard", "",
		client.BillingMutationOptions{
			Reason: "Repeat Professional", Confirmed: true,
			IdempotencyKey: "test-repeat-standard",
		}); err == nil ||
		err.Error() == "" {
		t.Fatalf("re-upgrade to same plan = %v; want the refusal message", err)
	}
}

func TestPlanClientRejectsInvalidMutationOutcomes(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
	}{
		{
			name: "done carries URL",
			body: `{"schema_version":"witself.v0","operation_id":"bop_test","operation":"plan_upgrade","actor_id":"opr_owner","actor_role":"account_owner","confirmed":true,"replayed":false,"kind":"done","plan":"standard","url":"https://billing.example/unexpected"}`,
		},
		{
			name: "upgrade claims scheduled",
			body: `{"schema_version":"witself.v0","operation_id":"bop_test","operation":"plan_upgrade","actor_id":"opr_owner","actor_role":"account_owner","confirmed":true,"replayed":false,"kind":"scheduled","plan":"standard","effective":"2026-09-01T00:00:00Z"}`,
		},
		{
			name: "wrong target plan",
			body: `{"schema_version":"witself.v0","operation_id":"bop_test","operation":"plan_upgrade","actor_id":"opr_owner","actor_role":"account_owner","confirmed":true,"replayed":false,"kind":"done","plan":"team"}`,
		},
		{
			name: "non-cancel outcome claims cancelled",
			body: `{"schema_version":"witself.v0","operation_id":"bop_test","operation":"plan_upgrade","actor_id":"opr_owner","actor_role":"account_owner","confirmed":true,"replayed":false,"kind":"done","plan":"standard","cancelled":true}`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(tc.body))
			}))
			t.Cleanup(srv.Close)
			if _, err := client.UpgradePlan(
				context.Background(), srv.URL, "acct_1", "owner", "standard", "",
				client.BillingMutationOptions{
					Reason: "test", Confirmed: true, IdempotencyKey: "test-key",
				}); err == nil {
				t.Fatal("invalid mutation outcome was accepted")
			}
		})
	}

	t.Run("cancel outcome omits confirmation marker", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"schema_version":"witself.v0","operation_id":"bop_cancel","operation":"plan_cancel","actor_id":"opr_owner","actor_role":"account_owner","confirmed":true,"replayed":false,"kind":"cancelled"}`))
		}))
		t.Cleanup(srv.Close)
		if _, err := client.CancelPlanChange(
			context.Background(), srv.URL, "acct_1", "owner",
			client.BillingMutationOptions{
				Reason: "test", Confirmed: true, IdempotencyKey: "test-key",
			}); err == nil {
			t.Fatal("cancel outcome without cancelled=true was accepted")
		}
	})

	t.Run("resolved cancel is accepted without cancellation marker", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"schema_version":"witself.v0","operation_id":"bop_resolved","operation":"plan_cancel","actor_id":"opr_owner","actor_role":"account_owner","confirmed":true,"replayed":false,"kind":"resolved"}`))
		}))
		t.Cleanup(srv.Close)
		out, err := client.CancelPlanChange(
			context.Background(), srv.URL, "acct_1", "owner",
			client.BillingMutationOptions{
				Reason: "test", Confirmed: true, IdempotencyKey: "test-key",
			})
		if err != nil || out.Kind != "resolved" || out.Plan != "" ||
			out.URL != "" || !out.Effective.IsZero() {
			t.Fatalf("resolved cancel = %+v, %v", out, err)
		}
	})

	t.Run("resolved cancel rejects cancellation marker", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"schema_version":"witself.v0","operation_id":"bop_resolved","operation":"plan_cancel","actor_id":"opr_owner","actor_role":"account_owner","confirmed":true,"replayed":false,"kind":"resolved","cancelled":true}`))
		}))
		t.Cleanup(srv.Close)
		if _, err := client.CancelPlanChange(
			context.Background(), srv.URL, "acct_1", "owner",
			client.BillingMutationOptions{
				Reason: "test", Confirmed: true, IdempotencyKey: "test-key",
			}); err == nil {
			t.Fatal("resolved cancel with cancelled=true was accepted")
		}
	})
}

func TestBillingMutationErrorsPreserveRetryContract(t *testing.T) {
	for _, tc := range []struct {
		name       string
		code       string
		message    string
		retryable  bool
		retryAfter string
		wantAfter  time.Duration
		sentinel   error
	}{
		{
			name: "changed key", code: "idempotency_conflict",
			message:  "key belongs to another request",
			sentinel: client.ErrBillingMutationIdempotencyConflict,
		},
		{
			name: "active operation", code: "operation_in_progress",
			message: "operation is still running", retryable: true,
			retryAfter: "7", wantAfter: 7 * time.Second,
			sentinel: client.ErrBillingMutationInProgress,
		},
		{
			name: "superseded operation", code: "operation_superseded",
			message:  "preview again and use a new key",
			sentinel: client.ErrBillingMutationSuperseded,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if tc.retryAfter != "" {
					w.Header().Set("Retry-After", tc.retryAfter)
				}
				w.WriteHeader(http.StatusConflict)
				_ = json.NewEncoder(w).Encode(map[string]any{
					"schema_version": "witself.v0",
					"code":           tc.code,
					"error":          tc.message,
					"retryable":      tc.retryable,
				})
			}))
			t.Cleanup(srv.Close)

			_, err := client.UpgradePlan(
				context.Background(), srv.URL, "acct_1", "owner", "standard", "",
				client.BillingMutationOptions{
					Reason: "test", Confirmed: true, IdempotencyKey: "test-key",
				})
			if err == nil {
				t.Fatal("mutation succeeded; want typed conflict")
			}
			var mutationErr *client.BillingMutationError
			if !errors.As(err, &mutationErr) {
				t.Fatalf("error = %T %v; want BillingMutationError", err, err)
			}
			if mutationErr.Code != tc.code || mutationErr.Retryable != tc.retryable ||
				mutationErr.RetryAfter != tc.wantAfter || mutationErr.Message != tc.message {
				t.Fatalf("BillingMutationError = %+v", mutationErr)
			}
			if !errors.Is(err, tc.sentinel) {
				t.Fatalf("errors.Is(%v, %v) = false", err, tc.sentinel)
			}
		})
	}

	t.Run("unknown conflict stays generic", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusConflict)
			_, _ = w.Write([]byte(`{"code":"other_conflict","error":"plain conflict","retryable":true}`))
		}))
		t.Cleanup(srv.Close)
		_, err := client.UpgradePlan(
			context.Background(), srv.URL, "acct_1", "owner", "standard", "",
			client.BillingMutationOptions{
				Reason: "test", Confirmed: true, IdempotencyKey: "test-key",
			})
		var mutationErr *client.BillingMutationError
		if err == nil || err.Error() != "plain conflict" || errors.As(err, &mutationErr) ||
			errors.Is(err, client.ErrBillingMutationIdempotencyConflict) ||
			errors.Is(err, client.ErrBillingMutationInProgress) ||
			errors.Is(err, client.ErrBillingMutationSuperseded) {
			t.Fatalf("unknown conflict = %T %v", err, err)
		}
	})
}

func TestBillingMutationApplyUsesExtendedTransportTimeout(t *testing.T) {
	originalTransport := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = originalTransport })
	remaining := map[string]time.Duration{}
	http.DefaultTransport = billingRoundTripFunc(func(r *http.Request) (*http.Response, error) {
		deadline, ok := r.Context().Deadline()
		if !ok {
			t.Errorf("%s request has no deadline", r.URL.Path)
		} else {
			remaining[r.URL.Path] = time.Until(deadline)
		}
		body := ""
		switch r.URL.Path {
		case "/v1/accounts/acct_1/plan:upgrade":
			body = `{"schema_version":"witself.v0","operation_id":"bop_upgrade","operation":"plan_upgrade","actor_id":"opr_owner","actor_role":"account_owner","confirmed":true,"replayed":false,"kind":"done","plan":"standard"}`
		case "/v1/accounts/acct_1/plan:downgrade":
			body = `{"schema_version":"witself.v0","operation_id":"bop_downgrade","operation":"plan_downgrade","actor_id":"opr_owner","actor_role":"account_owner","confirmed":true,"replayed":false,"kind":"scheduled","plan":"free","effective":"2026-09-01T00:00:00Z"}`
		case "/v1/accounts/acct_1/plan:cancel":
			body = `{"schema_version":"witself.v0","operation_id":"bop_cancel","operation":"plan_cancel","actor_id":"opr_owner","actor_role":"account_owner","confirmed":true,"replayed":false,"kind":"cancelled","cancelled":true}`
		case "/v1/accounts/acct_1/billing:setup":
			body = `{"schema_version":"witself.v0","operation_id":"bop_setup","operation":"billing_setup","actor_id":"opr_owner","actor_role":"account_owner","confirmed":true,"replayed":false,"kind":"done"}`
		case "/v1/accounts/acct_1/billing:preview":
			body = `{"schema_version":"witself.v0","operation":"billing_setup","allowed":true,"confirmation_required":true,"effects":[],"violations":[]}`
		case "/v1/accounts/acct_1/billing:portal":
			body = `{"schema_version":"witself.v0","kind":"done"}`
		default:
			t.Errorf("unexpected request path %q", r.URL.Path)
			body = `{}`
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(body)),
			Request:    r,
		}, nil
	})

	ctx := context.Background()
	if _, err := client.UpgradePlan(
		ctx, "https://control.example", "acct_1", "owner", "standard", "",
		client.BillingMutationOptions{
			Reason: "test", Confirmed: true, IdempotencyKey: "upgrade-key",
		}); err != nil {
		t.Fatalf("UpgradePlan: %v", err)
	}
	if _, err := client.DowngradePlan(
		ctx, "https://control.example", "acct_1", "owner", "free", "",
		client.BillingMutationOptions{
			Reason: "test", Confirmed: true, IdempotencyKey: "downgrade-key",
		}); err != nil {
		t.Fatalf("DowngradePlan: %v", err)
	}
	if _, err := client.CancelPlanChange(
		ctx, "https://control.example", "acct_1", "owner",
		client.BillingMutationOptions{
			Reason: "test", Confirmed: true, IdempotencyKey: "cancel-key",
		}); err != nil {
		t.Fatalf("CancelPlanChange: %v", err)
	}
	if _, err := client.CreateBillingSetup(
		ctx, "https://control.example", "acct_1", "owner", "",
		client.BillingMutationOptions{
			Reason: "test", Confirmed: true, IdempotencyKey: "setup-key",
		}); err != nil {
		t.Fatalf("CreateBillingSetup: %v", err)
	}
	if _, err := client.PreviewBillingMutation(
		ctx, "https://control.example", "acct_1", "owner",
		client.BillingMutationSetup, "", "", "test"); err != nil {
		t.Fatalf("PreviewBillingMutation: %v", err)
	}
	if _, err := client.CreateBillingPortal(
		ctx, "https://control.example", "acct_1", "owner"); err != nil {
		t.Fatalf("CreateBillingPortal: %v", err)
	}

	for _, path := range []string{
		"/v1/accounts/acct_1/plan:upgrade",
		"/v1/accounts/acct_1/plan:downgrade",
		"/v1/accounts/acct_1/plan:cancel",
		"/v1/accounts/acct_1/billing:setup",
	} {
		if got := remaining[path]; got <= 4*time.Minute {
			t.Errorf("%s timeout remaining = %v; want more than server 4m window", path, got)
		}
	}
	for _, path := range []string{
		"/v1/accounts/acct_1/billing:preview",
		"/v1/accounts/acct_1/billing:portal",
	} {
		if got := remaining[path]; got <= 10*time.Second || got > 20*time.Second {
			t.Errorf("%s timeout remaining = %v; want ordinary 15s transport window", path, got)
		}
	}
}

func TestResolveAccountViaBridgeRequiresSecureCellEndpoint(t *testing.T) {
	tests := []struct {
		name     string
		endpoint string
		want     string
		wantErr  bool
	}{
		{
			name:     "https",
			endpoint: "https://cell.example.invalid/",
			want:     "https://cell.example.invalid",
		},
		{name: "plaintext", endpoint: "http://cell.example.invalid", wantErr: true},
		{
			name:     "userinfo",
			endpoint: "https://user:secret@cell.example.invalid",
			wantErr:  true,
		},
		{
			name:     "query",
			endpoint: "https://cell.example.invalid?token=secret",
			wantErr:  true,
		},
		{
			name:     "fragment",
			endpoint: "https://cell.example.invalid#fragment",
			wantErr:  true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet ||
					r.URL.Path != "/v1/internal/accounts/acct_1:resolve" {
					t.Fatalf("request = %s %s", r.Method, r.URL.RequestURI())
				}
				if got := r.Header.Get("Authorization"); got != "Bearer bridge-secret" {
					t.Fatalf("Authorization = %q", got)
				}
				w.Header().Set("Content-Type", "application/json")
				if err := json.NewEncoder(w).Encode(map[string]string{
					"schema_version": "witself.v0",
					"account_id":     "acct_1",
					"state":          "active",
					"cell":           "cell-a",
					"endpoint":       tc.endpoint,
				}); err != nil {
					t.Fatal(err)
				}
			}))
			t.Cleanup(srv.Close)

			got, err := client.ResolveAccountViaBridge(
				context.Background(), srv.URL, "bridge-secret", "acct_1")
			if tc.wantErr {
				if err == nil {
					t.Fatalf("ResolveAccountViaBridge endpoint %q = %q; want error", tc.endpoint, got)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Fatalf("ResolveAccountViaBridge = %q, %v; want %q", got, err, tc.want)
			}
		})
	}
}
