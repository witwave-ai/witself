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
	"io"
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

// AdminAuthFunc authenticates the Worker's internal bridge credential and
// validates the already-authenticated immutable admin id plus display handle it
// forwards.
type AdminAuthFunc func(
	ctx context.Context,
	bearer, claimedID, claimedHandle string,
) (actor lifecycle.AdminActor, ok bool, err error)

// InternalAuthFunc authenticates platform-only value-free observability reads.
type InternalAuthFunc func(ctx context.Context, bearer string) (bool, error)

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
	// AdminAuthenticate enables account policy/plan override routes. nil keeps
	// the routes absent; account-owner tokens can never mint these exceptions.
	AdminAuthenticate AdminAuthFunc
	// AdminAccountExists verifies the target against the routed cell before
	// an override record can be created. Required with AdminAuthenticate.
	AdminAccountExists func(ctx context.Context, accountID string) (bool, error)
	// LifecycleObserver exposes aggregate seed/apply progress when paired
	// with InternalAuthenticate. Both nil keeps the route absent.
	LifecycleObserver    *PlanLifecycleObserver
	InternalAuthenticate InternalAuthFunc
}

// Register mounts the control plane's billing/plan routes onto mux.
func Register(mux *http.ServeMux, cfg Config) error {
	if cfg.Manager == nil || cfg.Catalog == nil || cfg.Authenticate == nil {
		return fmt.Errorf("cpserver: Manager, Catalog, and Authenticate are required")
	}
	if cfg.AdminAuthenticate != nil && cfg.AdminAccountExists == nil {
		return fmt.Errorf("cpserver: AdminAccountExists is required with AdminAuthenticate")
	}
	if (cfg.LifecycleObserver == nil) != (cfg.InternalAuthenticate == nil) {
		return fmt.Errorf("cpserver: LifecycleObserver and InternalAuthenticate must be configured together")
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
	if cfg.Manager.BillingAvailable() {
		mux.HandleFunc("POST /v1/accounts/{id}/plan:upgrade", withAccount(cfg, planUpgrade))
		mux.HandleFunc("POST /v1/accounts/{id}/plan:downgrade", withAccount(cfg, planDowngrade))
		mux.HandleFunc("POST /v1/accounts/{id}/plan:cancel", withAccount(cfg, planCancel))
	}

	if cfg.AdminAuthenticate != nil {
		mux.HandleFunc("GET /v1/admin/accounts/{id}/transcript-retention",
			withAdmin(cfg, adminGetTranscriptRetention))
		mux.HandleFunc("PUT /v1/admin/accounts/{id}/transcript-retention",
			withAdmin(cfg, adminPutTranscriptRetention))
		mux.HandleFunc("DELETE /v1/admin/accounts/{id}/transcript-retention",
			withAdmin(cfg, adminDeleteTranscriptRetention))
		mux.HandleFunc("GET /v1/admin/accounts/{id}/plan-override",
			withAdmin(cfg, adminGetPlanOverride))
		mux.HandleFunc("PUT /v1/admin/accounts/{id}/plan-override",
			withAdmin(cfg, adminPutPlanOverride))
		mux.HandleFunc("DELETE /v1/admin/accounts/{id}/plan-override",
			withAdmin(cfg, adminDeletePlanOverride))
		mux.HandleFunc("GET /v1/admin/accounts/{id}/limit-overrides/{dimension}",
			withAdmin(cfg, adminGetLimitOverride))
		mux.HandleFunc("PUT /v1/admin/accounts/{id}/limit-overrides/{dimension}",
			withAdmin(cfg, adminPutLimitOverride))
		mux.HandleFunc("DELETE /v1/admin/accounts/{id}/limit-overrides/{dimension}",
			withAdmin(cfg, adminDeleteLimitOverride))
	}
	if cfg.LifecycleObserver != nil {
		mux.HandleFunc("GET /v1/plan-lifecycle/status", func(w http.ResponseWriter, r *http.Request) {
			if !authorizeInternal(cfg, w, r) {
				return
			}
			writeJSON(w, http.StatusOK, map[string]any{
				"schema_version": "witself.v0",
				"plan_lifecycle": cfg.LifecycleObserver.Snapshot(),
			})
		})
		mux.HandleFunc("POST /v1/plan-lifecycle:tick", func(w http.ResponseWriter, r *http.Request) {
			if !authorizeInternal(cfg, w, r) {
				return
			}
			r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
			var req struct {
				AccountIDs []string `json:"account_ids"`
			}
			decoder := json.NewDecoder(r.Body)
			decoder.DisallowUnknownFields()
			if err := decoder.Decode(&req); err != nil || req.AccountIDs == nil {
				writeError(w, http.StatusBadRequest, "a bounded account_ids array is required")
				return
			}
			if err := validatePlanLifecycleAccountIDs(
				req.AccountIDs, maxPlanLifecycleTickAccounts); err != nil {
				writeError(w, http.StatusBadRequest, "invalid lifecycle account page")
				return
			}

			cfg.LifecycleObserver.begin(time.Now())
			tickCtx, cancel := context.WithTimeout(
				r.Context(), planLifecycleTickTimeout)
			defer cancel()
			summary, reconcileErr := ReconcileAccountIDs(
				tickCtx, cfg.Manager, req.AccountIDs,
				maxPlanLifecycleTickAccounts,
			)
			succeeded := reconcileErr == nil
			cfg.LifecycleObserver.complete(time.Now(), summary, succeeded)
			writeJSON(w, http.StatusOK, map[string]any{
				"schema_version": "witself.v0",
				"plan_lifecycle": map[string]any{
					"scanned":       summary.Scanned,
					"seeded":        summary.Seeded,
					"apply_pending": summary.ApplyPending,
					"failed":        summary.Failed,
					"succeeded":     succeeded,
				},
			})
		})
	}

	// One webhook route per configured provider: each partner has its own
	// signature scheme, and provider-scoped event folding is what keeps
	// colliding customer ids from cross-matching.
	for name, p := range cfg.Providers {
		mux.HandleFunc("POST /v1/billing/webhook/"+name, webhook(cfg, name, p))
	}
	return nil
}

func authorizeInternal(cfg Config, w http.ResponseWriter, r *http.Request) bool {
	bearer, ok := bearerToken(r)
	if !ok {
		writeError(w, http.StatusUnauthorized, "missing internal bearer token")
		return false
	}
	authorized, err := cfg.InternalAuthenticate(r.Context(), bearer)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not authenticate")
		return false
	}
	if !authorized {
		writeError(w, http.StatusForbidden, "not authorized")
		return false
	}
	return true
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
	rec, snapshot, err := cfg.Manager.ResolvedStatus(r.Context(), accountID, r.Header.Get("X-Witself-Email"))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not read plan status")
		return
	}
	out := map[string]any{
		"schema_version":       "witself.v0",
		"account_id":           rec.AccountID,
		"billing_available":    cfg.Manager.BillingAvailable(),
		"plan":                 snapshot.Plan,
		"billing_plan":         rec.Entitled,
		"applied":              rec.Applied,
		"limits":               snapshot.Limits,
		"limit_defaults":       snapshot.DefaultLimits,
		"policies":             snapshot.Policies,
		"policy_defaults":      snapshot.DefaultPolicies,
		"transcript_retention": transcriptRetentionView(rec, snapshot, false),
		"apply_pending":        lifecycle.SnapshotApplyPending(rec, snapshot),
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

type adminAccountHandler func(
	cfg Config,
	w http.ResponseWriter,
	r *http.Request,
	accountID string,
	actor lifecycle.AdminActor,
)

func withAdmin(cfg Config, h adminAccountHandler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		accountID := strings.TrimSpace(r.PathValue("id"))
		if accountID == "" {
			writeError(w, http.StatusBadRequest, "missing account id")
			return
		}
		bearer, ok := bearerToken(r)
		if !ok {
			writeError(w, http.StatusUnauthorized, "missing admin bearer token")
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
		actor, authorized, err := cfg.AdminAuthenticate(
			r.Context(), bearer,
			r.Header.Get("X-Witself-Admin-ID"),
			r.Header.Get("X-Witself-Admin-Handle"),
		)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "could not authenticate admin")
			return
		}
		if !authorized {
			writeError(w, http.StatusForbidden, "not authorized as an admin")
			return
		}
		exists, err := cfg.AdminAccountExists(r.Context(), accountID)
		if err != nil {
			writeError(w, http.StatusBadGateway, "could not verify admin account target")
			return
		}
		if !exists {
			writeError(w, http.StatusNotFound, "account not found")
			return
		}
		h(cfg, w, r, accountID, actor)
	}
}

func transcriptRetentionView(
	rec lifecycle.Record,
	snapshot lifecycle.PlanSnapshot,
	includeAdminDetail bool,
) map[string]any {
	var defaultDays any
	if days, ok := snapshot.DefaultPolicies[plans.TranscriptRetentionDaysPolicy]; ok {
		defaultDays = days
	}
	var effectiveDays any
	if days, ok := snapshot.Policies[plans.TranscriptRetentionDaysPolicy]; ok {
		effectiveDays = days
	}
	out := map[string]any{
		"default_days":   defaultDays,
		"effective_days": effectiveDays,
		"overridden":     rec.TranscriptRetentionOverride != nil,
	}
	if includeAdminDetail && rec.TranscriptRetentionOverride != nil {
		out["override"] = rec.TranscriptRetentionOverride
	}
	return out
}

func writeAdminAccountPolicy(
	cfg Config,
	w http.ResponseWriter,
	r *http.Request,
	accountID string,
	mutation bool,
	limitDimension string,
) {
	rec, snapshot, err := cfg.Manager.ResolvedStatus(r.Context(), accountID, "")
	if err != nil {
		writeManagerError(w, err)
		return
	}
	pending := lifecycle.SnapshotApplyPending(rec, snapshot)
	status := http.StatusOK
	if mutation && pending {
		status = http.StatusAccepted
	}
	out := map[string]any{
		"schema_version":       "witself.v0",
		"account_id":           accountID,
		"plan":                 snapshot.Plan,
		"billing_plan":         rec.Entitled,
		"applied":              rec.Applied,
		"limits":               snapshot.Limits,
		"limit_defaults":       snapshot.DefaultLimits,
		"limit_overrides":      rec.LimitOverrides,
		"plan_override":        rec.PlanOverride,
		"transcript_retention": transcriptRetentionView(rec, snapshot, true),
		"admin_history":        rec.AdminHistory,
		"apply_pending":        pending,
		"desired_revision":     rec.SnapshotRevision,
		"applied_revision":     rec.AppliedSnapshotRevision,
	}
	if limitDimension != "" {
		var defaultMax any
		if value, ok := snapshot.DefaultLimits[limitDimension]; ok {
			defaultMax = value
		}
		var effectiveMax any
		if value, ok := snapshot.Limits[limitDimension]; ok {
			effectiveMax = value
		}
		override, overridden := rec.LimitOverrides[limitDimension]
		limit := map[string]any{
			"dimension":     limitDimension,
			"default_max":   defaultMax,
			"effective_max": effectiveMax,
			"overridden":    overridden,
		}
		if overridden {
			limit["override"] = override
		}
		out["limit"] = limit
	}
	writeJSON(w, status, out)
}

func adminGetTranscriptRetention(
	cfg Config,
	w http.ResponseWriter,
	r *http.Request,
	accountID string,
	_ lifecycle.AdminActor,
) {
	writeAdminAccountPolicy(cfg, w, r, accountID, false, "")
}

func adminPutTranscriptRetention(
	cfg Config,
	w http.ResponseWriter,
	r *http.Request,
	accountID string,
	actor lifecycle.AdminActor,
) {
	var req struct {
		Days       *int64 `json:"days"`
		Indefinite bool   `json:"indefinite"`
		Reason     string `json:"reason"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil ||
		(req.Days == nil) == !req.Indefinite {
		writeError(w, http.StatusBadRequest, "set exactly one of days or indefinite=true")
		return
	}
	var days *int64
	if !req.Indefinite {
		days = req.Days
	}
	if _, err := cfg.Manager.SetTranscriptRetentionOverride(
		r.Context(), accountID, days, actor, req.Reason,
	); err != nil {
		writeManagerError(w, err)
		return
	}
	writeAdminAccountPolicy(cfg, w, r, accountID, true, "")
}

func adminDeleteTranscriptRetention(
	cfg Config,
	w http.ResponseWriter,
	r *http.Request,
	accountID string,
	actor lifecycle.AdminActor,
) {
	var req struct {
		Reason string `json:"reason"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "a JSON body with reason is required")
		return
	}
	if _, err := cfg.Manager.ClearTranscriptRetentionOverride(
		r.Context(), accountID, actor, req.Reason,
	); err != nil {
		writeManagerError(w, err)
		return
	}
	writeAdminAccountPolicy(cfg, w, r, accountID, true, "")
}

func adminGetPlanOverride(
	cfg Config,
	w http.ResponseWriter,
	r *http.Request,
	accountID string,
	_ lifecycle.AdminActor,
) {
	writeAdminAccountPolicy(cfg, w, r, accountID, false, "")
}

func adminPutPlanOverride(
	cfg Config,
	w http.ResponseWriter,
	r *http.Request,
	accountID string,
	actor lifecycle.AdminActor,
) {
	var req struct {
		Plan   string `json:"plan"`
		Reason string `json:"reason"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "a JSON body with plan and reason is required")
		return
	}
	if _, err := cfg.Manager.SetAccountPlanOverride(
		r.Context(), accountID, req.Plan, actor, req.Reason,
	); err != nil {
		writeManagerError(w, err)
		return
	}
	writeAdminAccountPolicy(cfg, w, r, accountID, true, "")
}

func adminDeletePlanOverride(
	cfg Config,
	w http.ResponseWriter,
	r *http.Request,
	accountID string,
	actor lifecycle.AdminActor,
) {
	var req struct {
		Reason string `json:"reason"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "a JSON body with reason is required")
		return
	}
	if _, err := cfg.Manager.ClearAccountPlanOverride(
		r.Context(), accountID, actor, req.Reason,
	); err != nil {
		writeManagerError(w, err)
		return
	}
	writeAdminAccountPolicy(cfg, w, r, accountID, true, "")
}

func adminLimitDimension(r *http.Request) (string, error) {
	dimension := strings.TrimSpace(r.PathValue("dimension"))
	if err := plans.ValidateLimits(map[string]int64{dimension: 0}); err != nil {
		return "", err
	}
	return dimension, nil
}

func decodeStrictJSON(r *http.Request, target any) error {
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		if err == nil {
			return errors.New("multiple JSON values")
		}
		return err
	}
	return nil
}

func adminGetLimitOverride(
	cfg Config,
	w http.ResponseWriter,
	r *http.Request,
	accountID string,
	_ lifecycle.AdminActor,
) {
	dimension, err := adminLimitDimension(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "unknown limit dimension")
		return
	}
	writeAdminAccountPolicy(cfg, w, r, accountID, false, dimension)
}

func adminPutLimitOverride(
	cfg Config,
	w http.ResponseWriter,
	r *http.Request,
	accountID string,
	actor lifecycle.AdminActor,
) {
	dimension, err := adminLimitDimension(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "unknown limit dimension")
		return
	}
	var req struct {
		Max       *int64 `json:"max"`
		Unlimited bool   `json:"unlimited"`
		Reason    string `json:"reason"`
	}
	if err := decodeStrictJSON(r, &req); err != nil ||
		(req.Max == nil) == !req.Unlimited {
		writeError(w, http.StatusBadRequest, "set exactly one of max or unlimited=true")
		return
	}
	var limitMax *int64
	if !req.Unlimited {
		limitMax = req.Max
	}
	if _, err := cfg.Manager.SetAccountLimitOverride(
		r.Context(), accountID, dimension, limitMax, actor, req.Reason,
	); err != nil {
		writeManagerError(w, err)
		return
	}
	writeAdminAccountPolicy(cfg, w, r, accountID, true, dimension)
}

func adminDeleteLimitOverride(
	cfg Config,
	w http.ResponseWriter,
	r *http.Request,
	accountID string,
	actor lifecycle.AdminActor,
) {
	dimension, err := adminLimitDimension(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "unknown limit dimension")
		return
	}
	var req struct {
		Reason string `json:"reason"`
	}
	if err := decodeStrictJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "a JSON body with reason is required")
		return
	}
	if _, err := cfg.Manager.ClearAccountLimitOverride(
		r.Context(), accountID, dimension, actor, req.Reason,
	); err != nil {
		writeManagerError(w, err)
		return
	}
	writeAdminAccountPolicy(cfg, w, r, accountID, true, dimension)
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
	if errors.Is(err, lifecycle.ErrAdminInput) {
		writeError(w, http.StatusBadRequest, strings.TrimPrefix(err.Error(), lifecycle.ErrAdminInput.Error()+": "))
		return
	}
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
