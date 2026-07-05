// Package cpserver is the control plane's HTTP layer: the plan lifecycle
// verbs the CLI drives, the per-provider billing webhook routes, and the
// public plan catalog — the service surface over the lifecycle Manager
// (issue #31). Cells never call this; they only receive applied snapshots.
//
// Auth: plan verbs act on behalf of an account owner, but operator tokens
// live on CELLS (the cell's database holds the hashes), so the control plane
// cannot validate them locally. Authenticate is that seam: production wires
// it to introspect the token against the account's cell (via the directory);
// tests stub it. Webhook routes carry no bearer auth by design — each
// provider authenticates its own callbacks (signatures) inside HandleWebhook.
package cpserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/witwave-ai/witself/internal/billing"
	"github.com/witwave-ai/witself/internal/billing/lifecycle"
	"github.com/witwave-ai/witself/internal/plans"
)

// AuthFunc reports whether bearer may operate on accountID's plan. The
// production implementation asks the account's cell to validate the token;
// the zero configuration refuses everything.
type AuthFunc func(ctx context.Context, accountID, bearer string) (bool, error)

// Config assembles the HTTP layer.
type Config struct {
	Manager *lifecycle.Manager
	Catalog *plans.Catalog
	// Providers are the named billing partners, for webhook routing. Must
	// match the Manager's configuration.
	Providers map[string]billing.Provider
	// Authenticate guards the plan verbs. Required — there is deliberately
	// no default-open mode.
	Authenticate AuthFunc
}

// Register mounts the control plane's billing/plan routes onto mux.
func Register(mux *http.ServeMux, cfg Config) error {
	if cfg.Manager == nil || cfg.Catalog == nil || cfg.Authenticate == nil {
		return fmt.Errorf("cpserver: Manager, Catalog, and Authenticate are required")
	}

	// The public plan catalog — the same witself.plans.v0 document the
	// Cloudflare Worker serves; here so the CLI needs exactly one host.
	mux.HandleFunc("GET /v1/plans", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{
			"schema_version": "witself.plans.v0",
			"updated":        cfg.Catalog.Updated,
			"currency":       cfg.Catalog.Currency,
			"plans":          cfg.Catalog.Plans,
		})
	})

	// Plan verbs, mirroring the cell's :verb idiom.
	mux.HandleFunc("GET /v1/accounts/{id}/plan", withAccount(cfg, planStatus))
	mux.HandleFunc("POST /v1/accounts/{id}/plan:upgrade", withAccount(cfg, planUpgrade))
	mux.HandleFunc("POST /v1/accounts/{id}/plan:downgrade", withAccount(cfg, planDowngrade))
	mux.HandleFunc("POST /v1/accounts/{id}/plan:cancel", withAccount(cfg, planCancel))

	// One webhook route per configured provider: each partner has its own
	// signature scheme, and provider-scoped event folding is what keeps
	// colliding customer ids from cross-matching.
	for name, p := range cfg.Providers {
		mux.HandleFunc("POST /v1/billing/webhook/"+name, webhook(cfg, name, p))
	}
	return nil
}

// RunReconciler sweeps the Manager on interval until ctx ends: expiring
// abandoned checkouts and converging entitled != applied — the loop that
// makes "never rests" true in production.
func RunReconciler(ctx context.Context, m *lifecycle.Manager, interval time.Duration, logf func(string, ...any)) {
	if interval <= 0 {
		interval = time.Minute
	}
	if logf == nil {
		logf = func(string, ...any) {}
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := m.Reconcile(ctx); err != nil {
				logf("cpserver: reconcile: %v", err)
			}
		}
	}
}

// accountHandler is a plan-verb handler bound to an authenticated account.
type accountHandler func(cfg Config, w http.ResponseWriter, r *http.Request, accountID string)

// withAccount authenticates the bearer for the path's account, mirroring the
// cell's requireOperator shape. The path id is the account id; the email used
// on first billing contact rides an optional header set by the CLI.
func withAccount(cfg Config, h accountHandler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		accountID := r.PathValue("id")
		if accountID == "" {
			writeError(w, http.StatusBadRequest, "missing account id")
			return
		}
		bearer, ok := bearerToken(r)
		if !ok {
			writeError(w, http.StatusUnauthorized, "missing bearer token")
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
		authorized, err := cfg.Authenticate(r.Context(), accountID, bearer)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "could not authenticate")
			return
		}
		if !authorized {
			writeError(w, http.StatusForbidden, "not authorized for this account")
			return
		}
		h(cfg, w, r, accountID)
	}
}

// pendingView is the wire shape of an in-flight change.
type pendingView struct {
	Kind      string     `json:"kind"`
	Plan      string     `json:"plan"`
	URL       string     `json:"url,omitempty"`
	Expires   *time.Time `json:"expires,omitempty"`
	Effective *time.Time `json:"effective,omitempty"`
	Requested time.Time  `json:"requested"`
}

func planStatus(cfg Config, w http.ResponseWriter, r *http.Request, accountID string) {
	rec, err := cfg.Manager.Status(r.Context(), accountID, r.Header.Get("X-Witself-Email"))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not read plan status")
		return
	}
	out := map[string]any{
		"schema_version": "witself.v0",
		"account_id":     rec.AccountID,
		"plan":           rec.Entitled,
		"applied":        rec.Applied,
	}
	if rec.PastDueSince != nil {
		out["past_due_since"] = rec.PastDueSince
	}
	if rec.ApplyBlocked != "" {
		out["apply_blocked"] = rec.ApplyBlocked
	}
	if p := rec.Pending; p != nil {
		pv := pendingView{Kind: string(p.Kind), Plan: p.Plan, URL: p.URL, Requested: p.Requested}
		if !p.Expires.IsZero() {
			pv.Expires = &p.Expires
		}
		if !p.Effective.IsZero() {
			pv.Effective = &p.Effective
		}
		out["pending"] = pv
	}
	writeJSON(w, http.StatusOK, out)
}

// decodePlan reads the {"plan": "..."} body the change verbs share.
func decodePlan(r *http.Request) (string, error) {
	var req struct {
		Plan string `json:"plan"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || strings.TrimSpace(req.Plan) == "" {
		return "", fmt.Errorf("a plan is required")
	}
	return strings.TrimSpace(req.Plan), nil
}

// writeOutcome renders a lifecycle.Outcome; the CLI branches on "kind".
func writeOutcome(w http.ResponseWriter, out lifecycle.Outcome) {
	doc := map[string]any{
		"schema_version": "witself.v0",
		"kind":           out.Kind,
		"plan":           out.Plan,
	}
	if out.URL != "" {
		doc["url"] = out.URL
	}
	if !out.Effective.IsZero() {
		doc["effective"] = out.Effective
	}
	writeJSON(w, http.StatusOK, doc)
}

func planUpgrade(cfg Config, w http.ResponseWriter, r *http.Request, accountID string) {
	plan, err := decodePlan(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	out, err := cfg.Manager.RequestUpgrade(r.Context(), accountID, r.Header.Get("X-Witself-Email"), plan)
	if err != nil {
		writeManagerError(w, err)
		return
	}
	writeOutcome(w, out)
}

func planDowngrade(cfg Config, w http.ResponseWriter, r *http.Request, accountID string) {
	plan, err := decodePlan(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	out, err := cfg.Manager.RequestDowngrade(r.Context(), accountID, r.Header.Get("X-Witself-Email"), plan)
	if err != nil {
		// Includes the fit-check block with its violations report.
		writeManagerError(w, err)
		return
	}
	writeOutcome(w, out)
}

func planCancel(cfg Config, w http.ResponseWriter, r *http.Request, accountID string) {
	if err := cfg.Manager.CancelPending(r.Context(), accountID); err != nil {
		writeManagerError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"schema_version": "witself.v0",
		"cancelled":      true,
	})
}

// webhook verifies and folds one provider's callback. Non-2xx responses make
// providers redeliver, so: signature/parse failures are 400 (retrying won't
// help a forged or malformed delivery), but folding errors are 500 (transient
// — redelivery is the safety net, and OnEvents folds idempotently).
func webhook(cfg Config, name string, p billing.Provider) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// The one anonymous route: cap the body before the provider reads it,
		// or arbitrary callers could stream gigabytes into memory. Real
		// webhook payloads are a few KiB.
		r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
		events, err := p.HandleWebhook(r)
		if err != nil {
			writeError(w, http.StatusBadRequest, "webhook rejected")
			return
		}
		if err := cfg.Manager.OnEvents(r.Context(), name, events); err != nil {
			writeError(w, http.StatusInternalServerError, "could not process events")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"schema_version": "witself.v0",
			"received":       len(events),
		})
	}
}

// writeManagerError maps a Manager error onto HTTP: user-addressed refusals
// (lifecycle.ErrRefusal) become 409 with the message verbatim — the message
// IS the product there — while everything else (Store I/O, provider API
// failures, CAS exhaustion) becomes a generic 500 so infrastructure trouble
// neither masquerades as a policy refusal nor leaks backend detail.
func writeManagerError(w http.ResponseWriter, err error) {
	if errors.Is(err, lifecycle.ErrRefusal) {
		writeError(w, http.StatusConflict, strings.TrimPrefix(err.Error(), lifecycle.ErrRefusal.Error()+": "))
		return
	}
	writeError(w, http.StatusInternalServerError, "plan change failed — please retry")
}

func bearerToken(r *http.Request) (string, bool) {
	h := r.Header.Get("Authorization")
	tok, ok := strings.CutPrefix(h, "Bearer ")
	tok = strings.TrimSpace(tok)
	return tok, ok && tok != ""
}

func writeJSON(w http.ResponseWriter, status int, doc any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(doc)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{
		"schema_version": "witself.v0",
		"error":          msg,
	})
}
