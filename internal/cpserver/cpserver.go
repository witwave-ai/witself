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
	neturl "net/url"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/witwave-ai/witself/internal/billing"
	"github.com/witwave-ai/witself/internal/billing/lifecycle"
	"github.com/witwave-ai/witself/internal/plans"
)

// AccountPermission is the account-level capability required by a customer
// billing route. Reads and mutations are deliberately distinct: mere account
// membership is never billing authority.
type AccountPermission string

const (
	// AccountPermissionBillingRead allows provider-neutral account billing and
	// plan reads without granting any provider or lifecycle mutation.
	AccountPermissionBillingRead AccountPermission = "billing:read"
	// AccountPermissionBillingManage allows provider-hosted setup/portal and
	// subscription lifecycle mutations.
	AccountPermissionBillingManage AccountPermission = "billing:manage"
)

// AccountAccess is authenticated, role-derived authority. ActorID and Role
// are carried to the handler boundary so mutation auditing can attribute the
// caller instead of reconstructing identity from untrusted request input.
type AccountAccess struct {
	ActorID    string
	Role       string
	Permission AccountPermission
}

// AuthFunc authorizes one explicit account permission. The production
// implementation asks the account's cell to validate the token and return its
// account role; the zero configuration refuses everything.
type AuthFunc func(
	ctx context.Context,
	accountID, bearer string,
	permission AccountPermission,
) (access AccountAccess, ok bool, err error)

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
	mux.HandleFunc("GET /v1/accounts/{id}/plan",
		withAccount(cfg, AccountPermissionBillingRead, planStatus))
	// Provider-neutral billing reads exist in both managed and providerless
	// modes. Reading a Personal/free account is side-effect free. Mutation
	// routes stay registered so a provider can be enabled without a client
	// reinstall; a providerless deployment returns a stable unsupported
	// response without creating lifecycle state.
	mux.HandleFunc("GET /v1/accounts/{id}/billing",
		withAccount(cfg, AccountPermissionBillingRead, billingStatus))
	mux.HandleFunc("GET /v1/accounts/{id}/billing/invoices",
		withAccount(cfg, AccountPermissionBillingRead, billingInvoices))
	mux.HandleFunc("GET /v1/accounts/{id}/billing/payments",
		withAccount(cfg, AccountPermissionBillingRead, billingPayments))
	mux.HandleFunc("POST /v1/accounts/{id}/billing:portal",
		withAccount(cfg, AccountPermissionBillingManage, billingPortal))
	mux.HandleFunc("POST /v1/accounts/{id}/billing:preview",
		withAccount(cfg, AccountPermissionBillingManage, billingPreview))
	mux.HandleFunc("POST /v1/accounts/{id}/billing:setup",
		withAccount(cfg, AccountPermissionBillingManage, billingSetup))
	if cfg.Manager.BillingAvailable() {
		mux.HandleFunc("POST /v1/accounts/{id}/plan:upgrade",
			withAccount(cfg, AccountPermissionBillingManage, planUpgrade))
		mux.HandleFunc("POST /v1/accounts/{id}/plan:downgrade",
			withAccount(cfg, AccountPermissionBillingManage, planDowngrade))
		mux.HandleFunc("POST /v1/accounts/{id}/plan:cancel",
			withAccount(cfg, AccountPermissionBillingManage, planCancel))
	}

	if cfg.AdminAuthenticate != nil {
		mux.HandleFunc("GET /v1/admin/accounts/{id}/transcript-retention",
			withAdmin(cfg, adminGetTranscriptRetention))
		mux.HandleFunc("PUT /v1/admin/accounts/{id}/transcript-retention",
			withAdmin(cfg, adminPutTranscriptRetention))
		mux.HandleFunc("DELETE /v1/admin/accounts/{id}/transcript-retention",
			withAdmin(cfg, adminDeleteTranscriptRetention))
		mux.HandleFunc("GET /v1/admin/accounts/{id}/messaging",
			withAdmin(cfg, adminGetMessaging))
		mux.HandleFunc("PUT /v1/admin/accounts/{id}/messaging",
			withAdmin(cfg, adminPutMessaging))
		mux.HandleFunc("DELETE /v1/admin/accounts/{id}/messaging",
			withAdmin(cfg, adminDeleteMessaging))
		mux.HandleFunc("GET /v1/admin/accounts/{id}/message-retention",
			withAdmin(cfg, adminGetMessageRetention))
		mux.HandleFunc("PUT /v1/admin/accounts/{id}/message-retention",
			withAdmin(cfg, adminPutMessageRetention))
		mux.HandleFunc("DELETE /v1/admin/accounts/{id}/message-retention",
			withAdmin(cfg, adminDeleteMessageRetention))
		mux.HandleFunc("GET /v1/admin/accounts/{id}/email-receive",
			withAdmin(cfg, adminGetAgentEmailReceive))
		mux.HandleFunc("PUT /v1/admin/accounts/{id}/email-receive",
			withAdmin(cfg, adminPutAgentEmailReceive))
		mux.HandleFunc("DELETE /v1/admin/accounts/{id}/email-receive",
			withAdmin(cfg, adminDeleteAgentEmailReceive))
		mux.HandleFunc("GET /v1/admin/accounts/{id}/email-retention",
			withAdmin(cfg, adminGetAgentEmailRetention))
		mux.HandleFunc("PUT /v1/admin/accounts/{id}/email-retention",
			withAdmin(cfg, adminPutAgentEmailRetention))
		mux.HandleFunc("DELETE /v1/admin/accounts/{id}/email-retention",
			withAdmin(cfg, adminDeleteAgentEmailRetention))
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
	if cfg.LifecycleObserver != nil && cfg.InternalAuthenticate != nil {
		mux.HandleFunc("GET /v1/plan-lifecycle/status", func(w http.ResponseWriter, r *http.Request) {
			if !authorizeInternal(cfg, w, r) {
				return
			}
			writeJSON(w, http.StatusOK, map[string]any{
				"schema_version": "witself.v0",
				"plan_lifecycle": cfg.LifecycleObserver.Snapshot(),
			})
		})
		mux.HandleFunc("GET /v1/plan-lifecycle/metrics", func(w http.ResponseWriter, r *http.Request) {
			if !authorizeInternal(cfg, w, r) {
				return
			}
			w.Header().Set("Content-Type",
				"text/plain; version=0.0.4; charset=utf-8")
			w.WriteHeader(http.StatusOK)
			_ = cfg.LifecycleObserver.WritePrometheus(w)
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
			mutationSummary, mutationErr :=
				reconcileBillingMutationBatch(tickCtx, cfg.Manager)
			summary, reconcileErr := ReconcileAccountIDs(
				tickCtx, cfg.Manager, req.AccountIDs,
				maxPlanLifecycleTickAccounts,
			)
			summary.BillingMutations = billingMutationBatchView(
				mutationSummary, mutationErr == nil)
			succeeded := reconcileErr == nil && mutationErr == nil
			cfg.LifecycleObserver.complete(time.Now(), summary,
				succeeded)
			writeJSON(w, http.StatusOK, map[string]any{
				"schema_version": "witself.v0",
				"plan_lifecycle": map[string]any{
					"scanned":           summary.Scanned,
					"seeded":            summary.Seeded,
					"apply_pending":     summary.ApplyPending,
					"failed":            summary.Failed,
					"succeeded":         succeeded,
					"billing_mutations": summary.BillingMutations,
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
type accountHandler func(
	cfg Config,
	w http.ResponseWriter,
	r *http.Request,
	accountID string,
	access AccountAccess,
)

// withAccount authenticates the bearer for the path's account, mirroring the
// cell's requireOperator shape. The path id is the account id; the email used
// on first billing contact rides an optional header set by the CLI.
func withAccount(
	cfg Config,
	permission AccountPermission,
	h accountHandler,
) http.HandlerFunc {
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
		access, authorized, err := cfg.Authenticate(
			r.Context(), accountID, bearer, permission,
		)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "could not authenticate")
			return
		}
		if !authorized {
			writeError(w, http.StatusForbidden, "not authorized for this account")
			return
		}
		if strings.TrimSpace(access.ActorID) == "" ||
			strings.TrimSpace(access.Role) == "" || access.Permission != permission {
			writeError(w, http.StatusInternalServerError, "invalid authorization result")
			return
		}
		h(cfg, w, r, accountID, access)
	}
}

// pendingView is the wire shape of an in-flight change.
type pendingView struct {
	Kind      string     `json:"kind"`
	Plan      string     `json:"plan"`
	PlanName  string     `json:"plan_name,omitempty"`
	URL       string     `json:"url,omitempty"`
	Expires   *time.Time `json:"expires,omitempty"`
	Effective *time.Time `json:"effective,omitempty"`
	Requested time.Time  `json:"requested"`
}

// billingSummaryView is deliberately provider-neutral. Provider and customer
// identifiers remain control-plane internals; Configured is the only public
// indication that a durable provider relationship exists.
type billingSummaryView struct {
	SchemaVersion      string                    `json:"schema_version"`
	AccountID          string                    `json:"account_id"`
	BillingAvailable   bool                      `json:"billing_available"`
	Configured         bool                      `json:"configured"`
	SubscriptionStatus string                    `json:"subscription_status"`
	BillingPlan        string                    `json:"billing_plan"`
	BillingPlanName    string                    `json:"billing_plan_name"`
	EffectivePlan      string                    `json:"effective_plan"`
	EffectivePlanName  string                    `json:"effective_plan_name"`
	AppliedPlan        string                    `json:"applied_plan"`
	EntitledAt         *time.Time                `json:"entitled_at,omitempty"`
	PastDueSince       *time.Time                `json:"past_due_since,omitempty"`
	PaymentMethod      *billingPaymentMethodView `json:"payment_method"`
	NextCharge         *billingChargeView        `json:"next_charge"`
	Pending            *pendingView              `json:"pending,omitempty"`
}

type billingPaymentMethodView struct {
	Label string `json:"label"`
}

type billingChargeView struct {
	Date        time.Time `json:"date"`
	AmountCents int64     `json:"amount_cents"`
	Currency    string    `json:"currency"`
}

type billingInvoiceView struct {
	Number      string    `json:"number"`
	Date        time.Time `json:"date"`
	AmountCents int64     `json:"amount_cents"`
	Currency    string    `json:"currency"`
	Status      string    `json:"status"`
	PDFURL      string    `json:"pdf_url,omitempty"`
	HostedURL   string    `json:"hosted_url,omitempty"`
}

type billingPaymentView struct {
	Date        time.Time `json:"date"`
	AmountCents int64     `json:"amount_cents"`
	Currency    string    `json:"currency"`
	Method      string    `json:"method"`
	Status      string    `json:"status"`
	ReceiptURL  string    `json:"receipt_url,omitempty"`
}

const maxBillingCollectionEntries = 100

func billingStatus(cfg Config, w http.ResponseWriter, r *http.Request, accountID string, _ AccountAccess) {
	w.Header().Set("Cache-Control", "no-store")
	email := r.Header.Get("X-Witself-Email")
	rec, snapshot, err := cfg.Manager.ResolvedStatus(r.Context(), accountID, email)
	if err != nil {
		writeBillingManagerError(w, err)
		return
	}
	providerSummary, err := cfg.Manager.ReadBillingSummary(r.Context(), accountID, email)
	if err != nil {
		writeBillingManagerError(w, err)
		return
	}

	pending, err := billingPendingView(cfg.Catalog, rec.Pending)
	if err != nil {
		writeBillingManagerError(w, err)
		return
	}
	view := billingSummaryView{
		SchemaVersion:      "witself.v0",
		AccountID:          accountID,
		BillingAvailable:   cfg.Manager.BillingAvailable(),
		Configured:         providerSummary.Configured,
		SubscriptionStatus: billingSubscriptionStatus(rec),
		BillingPlan:        rec.Entitled,
		BillingPlanName:    billingPlanName(cfg.Catalog, rec.Entitled),
		EffectivePlan:      snapshot.Plan,
		EffectivePlanName:  billingPlanName(cfg.Catalog, snapshot.Plan),
		AppliedPlan:        rec.Applied,
		PastDueSince:       copyTimePointer(rec.PastDueSince),
		Pending:            pending,
	}
	if !rec.EntitledAt.IsZero() {
		entitledAt := rec.EntitledAt
		view.EntitledAt = &entitledAt
	}
	if providerSummary.PaymentMethod != nil {
		view.PaymentMethod = &billingPaymentMethodView{
			Label: providerSummary.PaymentMethod.Label,
		}
	}
	if providerSummary.NextCharge != nil {
		view.NextCharge = &billingChargeView{
			Date:        providerSummary.NextCharge.Date,
			AmountCents: providerSummary.NextCharge.AmountCents,
			Currency:    providerSummary.NextCharge.Currency,
		}
	}
	writeJSON(w, http.StatusOK, view)
}

func billingInvoices(cfg Config, w http.ResponseWriter, r *http.Request, accountID string, _ AccountAccess) {
	w.Header().Set("Cache-Control", "no-store")
	invoices, err := cfg.Manager.ListBillingInvoices(
		r.Context(), accountID, r.Header.Get("X-Witself-Email"))
	if err != nil {
		writeBillingManagerError(w, err)
		return
	}
	if len(invoices) > maxBillingCollectionEntries {
		invoices = invoices[:maxBillingCollectionEntries]
	}
	out := make([]billingInvoiceView, 0, len(invoices))
	for _, invoice := range invoices {
		out = append(out, billingInvoiceView{
			Number: invoice.Number, Date: invoice.Date,
			AmountCents: invoice.AmountCents, Currency: invoice.Currency,
			Status: invoice.Status,
			// Optional provider links are conveniences, not the invoice record.
			// Drop an unsafe link while preserving the safe financial history.
			PDFURL:    safeOptionalBillingURL(invoice.PDFURL),
			HostedURL: safeOptionalBillingURL(invoice.HostedURL),
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"schema_version": "witself.v0",
		"account_id":     accountID,
		"invoices":       out,
	})
}

func billingPayments(cfg Config, w http.ResponseWriter, r *http.Request, accountID string, _ AccountAccess) {
	w.Header().Set("Cache-Control", "no-store")
	payments, err := cfg.Manager.ListBillingPayments(
		r.Context(), accountID, r.Header.Get("X-Witself-Email"))
	if err != nil {
		writeBillingManagerError(w, err)
		return
	}
	if len(payments) > maxBillingCollectionEntries {
		payments = payments[:maxBillingCollectionEntries]
	}
	out := make([]billingPaymentView, 0, len(payments))
	for _, payment := range payments {
		out = append(out, billingPaymentView{
			Date: payment.Date, AmountCents: payment.AmountCents,
			Currency: payment.Currency, Method: payment.Method,
			Status:     payment.Status,
			ReceiptURL: safeOptionalBillingURL(payment.ReceiptURL),
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"schema_version": "witself.v0",
		"account_id":     accountID,
		"payments":       out,
	})
}

func billingPreview(cfg Config, w http.ResponseWriter, r *http.Request, accountID string, _ AccountAccess) {
	w.Header().Set("Cache-Control", "no-store")
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	if len(r.Header.Values("Idempotency-Key")) != 0 {
		writeBillingMutationEnvelopeError(w,
			"Idempotency-Key is not accepted for a billing preview")
		return
	}
	var req struct {
		Operation lifecycle.BillingMutationOperation `json:"operation"`
		Plan      string                             `json:"plan,omitempty"`
		Reason    string                             `json:"reason"`
	}
	if err := decodeStrictJSON(r, &req); err != nil {
		writeBillingMutationEnvelopeError(w, "a strict billing preview body is required")
		return
	}
	command := lifecycle.BillingMutationCommand{
		Operation: req.Operation,
		Plan:      strings.TrimSpace(req.Plan),
		Reason:    strings.TrimSpace(req.Reason),
	}
	if err := validateBillingMutationEnvelope(command, false); err != nil {
		writeBillingMutationEnvelopeError(w, err.Error())
		return
	}
	preview, err := cfg.Manager.PreviewBillingMutation(
		r.Context(), accountID, r.Header.Get("X-Witself-Email"), command)
	if err != nil {
		writeBillingMutationManagerError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"schema_version":        "witself.v0",
		"operation":             preview.Operation,
		"plan":                  preview.Plan,
		"allowed":               preview.Allowed,
		"confirmation_required": preview.ConfirmationRequired,
		"effects":               preview.Effects,
		"violations":            preview.Violations,
	})
}

func billingSetup(cfg Config, w http.ResponseWriter, r *http.Request, accountID string, access AccountAccess) {
	w.Header().Set("Cache-Control", "no-store")
	command, err := decodeBillingMutationApply(
		w, r, lifecycle.BillingMutationSetup, false)
	if err != nil {
		return
	}
	if !cfg.Manager.BillingAvailable() {
		writeBillingManagerError(w, lifecycle.ErrBillingUnavailable)
		return
	}
	execution, err := cfg.Manager.ExecuteBillingMutation(
		r.Context(), accountID, r.Header.Get("X-Witself-Email"),
		billingActor(access), command)
	if err != nil {
		writeBillingMutationManagerError(w, err)
		return
	}
	writeBillingMutationExecution(w, execution)
}

func billingPortal(cfg Config, w http.ResponseWriter, r *http.Request, accountID string, _ AccountAccess) {
	w.Header().Set("Cache-Control", "no-store")
	url, err := cfg.Manager.CreateBillingPortal(
		r.Context(), accountID, r.Header.Get("X-Witself-Email"))
	if err != nil {
		writeBillingManagerError(w, err)
		return
	}
	writeBillingAction(w, billing.Action{URL: url})
}

func writeBillingAction(w http.ResponseWriter, action billing.Action) {
	doc := map[string]any{"schema_version": "witself.v0"}
	switch {
	case action.Done && action.URL == "":
		doc["kind"] = "done"
	case !action.Done && safeBillingURL(action.URL):
		doc["kind"] = "action"
		doc["url"] = action.URL
	default:
		writeBillingManagerError(w, invalidBillingProviderProjection("hosted action"))
		return
	}
	writeJSON(w, http.StatusOK, doc)
}

func billingSubscriptionStatus(rec lifecycle.Record) string {
	switch {
	case rec.PastDueSince != nil:
		return "past_due"
	case rec.Entitled != plans.Free:
		return "active"
	case rec.Pending != nil &&
		(rec.Pending.Kind == lifecycle.PendingUpgrade ||
			rec.Pending.Kind == lifecycle.PendingDowngrade):
		return "pending"
	default:
		return "none"
	}
}

func billingPlanName(catalog *plans.Catalog, planID string) string {
	if plan, ok := catalog.Get(planID); ok {
		return plan.Name
	}
	return planID
}

func billingPendingView(catalog *plans.Catalog, pending *lifecycle.Pending) (*pendingView, error) {
	if pending == nil {
		return nil, nil
	}
	view := &pendingView{
		Kind: string(pending.Kind), Plan: pending.Plan,
		PlanName:  billingPlanName(catalog, pending.Plan),
		Requested: pending.Requested,
	}
	if pending.URL != "" {
		if !safeBillingURL(pending.URL) {
			return nil, invalidBillingProviderProjection("pending action")
		}
		view.URL = pending.URL
	}
	if !pending.Expires.IsZero() {
		expires := pending.Expires
		view.Expires = &expires
	}
	if !pending.Effective.IsZero() {
		effective := pending.Effective
		view.Effective = &effective
	}
	return view, nil
}

func copyTimePointer(value *time.Time) *time.Time {
	if value == nil {
		return nil
	}
	cloned := *value
	return &cloned
}

func safeOptionalBillingURL(raw string) string {
	if raw == "" || !safeBillingURL(raw) {
		return ""
	}
	return raw
}

func safeBillingURL(raw string) bool {
	if raw == "" || raw != strings.TrimSpace(raw) || !utf8.ValidString(raw) ||
		strings.ContainsRune(raw, '\\') || billingURLHasUnsafeRune(raw) {
		return false
	}
	decoded, err := neturl.PathUnescape(raw)
	if err != nil || strings.ContainsRune(decoded, '\\') || billingURLHasUnsafeRune(decoded) {
		return false
	}
	parsed, err := neturl.Parse(raw)
	if err != nil || !strings.EqualFold(parsed.Scheme, "https") ||
		parsed.Opaque != "" || parsed.User != nil || parsed.Host == "" ||
		parsed.Hostname() == "" {
		return false
	}
	return true
}

func billingURLHasUnsafeRune(raw string) bool {
	for _, r := range raw {
		if unicode.IsControl(r) || r == '\u2028' || r == '\u2029' || isBidiControl(r) {
			return true
		}
	}
	return false
}

func isBidiControl(r rune) bool {
	switch r {
	case '\u061c', '\u200e', '\u200f', '\u202a', '\u202b', '\u202c',
		'\u202d', '\u202e', '\u2066', '\u2067', '\u2068', '\u2069':
		return true
	default:
		return false
	}
}

func invalidBillingProviderProjection(kind string) error {
	return fmt.Errorf("%w: invalid %s", lifecycle.ErrProviderRequest, kind)
}

func planStatus(cfg Config, w http.ResponseWriter, r *http.Request, accountID string, _ AccountAccess) {
	rec, snapshot, err := cfg.Manager.ResolvedStatus(r.Context(), accountID, r.Header.Get("X-Witself-Email"))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not read plan status")
		return
	}
	planName := snapshot.Plan
	if plan, ok := cfg.Catalog.Get(snapshot.Plan); ok {
		planName = plan.Name
	}
	billingPlanName := rec.Entitled
	if plan, ok := cfg.Catalog.Get(rec.Entitled); ok {
		billingPlanName = plan.Name
	}
	out := map[string]any{
		"schema_version":       "witself.v0",
		"account_id":           rec.AccountID,
		"billing_available":    cfg.Manager.BillingAvailable(),
		"plan":                 snapshot.Plan,
		"plan_name":            planName,
		"billing_plan":         rec.Entitled,
		"billing_plan_name":    billingPlanName,
		"applied":              rec.Applied,
		"limits":               snapshot.Limits,
		"limit_defaults":       snapshot.DefaultLimits,
		"policies":             snapshot.Policies,
		"policy_defaults":      snapshot.DefaultPolicies,
		"features":             snapshot.Features,
		"feature_defaults":     snapshot.DefaultFeatures,
		"messaging":            messagingView(rec, snapshot, false),
		"message_retention":    messageRetentionView(rec, snapshot, false),
		"email_receive":        agentEmailReceiveView(rec, snapshot, false),
		"email_retention":      agentEmailRetentionView(rec, snapshot, false),
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
		if plan, ok := cfg.Catalog.Get(p.Plan); ok {
			pv.PlanName = plan.Name
		}
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

func messagingView(
	rec lifecycle.Record,
	snapshot lifecycle.PlanSnapshot,
	includeAdminDetail bool,
) map[string]any {
	out := map[string]any{
		"default_enabled": slices.Contains(
			snapshot.DefaultFeatures, plans.MessagingFeature),
		"enabled":    slices.Contains(snapshot.Features, plans.MessagingFeature),
		"overridden": rec.MessagingOverride != nil,
	}
	if includeAdminDetail && rec.MessagingOverride != nil {
		out["override"] = rec.MessagingOverride
	}
	return out
}

func messageRetentionView(
	rec lifecycle.Record,
	snapshot lifecycle.PlanSnapshot,
	includeAdminDetail bool,
) map[string]any {
	var defaultDays any
	if days, ok := snapshot.DefaultPolicies[plans.MessageRetentionDaysPolicy]; ok {
		defaultDays = days
	}
	var effectiveDays any
	if days, ok := snapshot.Policies[plans.MessageRetentionDaysPolicy]; ok {
		effectiveDays = days
	}
	out := map[string]any{
		"default_days":   defaultDays,
		"effective_days": effectiveDays,
		"overridden":     rec.MessageRetentionOverride != nil,
	}
	if includeAdminDetail && rec.MessageRetentionOverride != nil {
		out["override"] = rec.MessageRetentionOverride
	}
	return out
}

func agentEmailReceiveView(
	rec lifecycle.Record,
	snapshot lifecycle.PlanSnapshot,
	includeAdminDetail bool,
) map[string]any {
	out := map[string]any{
		"default_enabled": slices.Contains(
			snapshot.DefaultFeatures, plans.AgentEmailReceiveFeature),
		"enabled": slices.Contains(
			snapshot.Features, plans.AgentEmailReceiveFeature),
		"overridden": rec.AgentEmailReceiveOverride != nil,
	}
	if includeAdminDetail && rec.AgentEmailReceiveOverride != nil {
		out["override"] = rec.AgentEmailReceiveOverride
	}
	return out
}

func agentEmailRetentionView(
	rec lifecycle.Record,
	snapshot lifecycle.PlanSnapshot,
	includeAdminDetail bool,
) map[string]any {
	var defaultDays any
	if days, ok := snapshot.DefaultPolicies[plans.AgentEmailRetentionDaysPolicy]; ok {
		defaultDays = days
	}
	var effectiveDays any
	if days, ok := snapshot.Policies[plans.AgentEmailRetentionDaysPolicy]; ok {
		effectiveDays = days
	}
	out := map[string]any{
		"default_days": defaultDays, "effective_days": effectiveDays,
		"overridden": rec.AgentEmailRetentionOverride != nil,
	}
	if includeAdminDetail && rec.AgentEmailRetentionOverride != nil {
		out["override"] = rec.AgentEmailRetentionOverride
	}
	return out
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
		"features":             snapshot.Features,
		"feature_defaults":     snapshot.DefaultFeatures,
		"plan_override":        rec.PlanOverride,
		"messaging":            messagingView(rec, snapshot, true),
		"message_retention":    messageRetentionView(rec, snapshot, true),
		"email_receive":        agentEmailReceiveView(rec, snapshot, true),
		"email_retention":      agentEmailRetentionView(rec, snapshot, true),
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

func adminGetMessaging(
	cfg Config,
	w http.ResponseWriter,
	r *http.Request,
	accountID string,
	_ lifecycle.AdminActor,
) {
	writeAdminAccountPolicy(cfg, w, r, accountID, false, "")
}

func adminPutMessaging(
	cfg Config,
	w http.ResponseWriter,
	r *http.Request,
	accountID string,
	actor lifecycle.AdminActor,
) {
	var req struct {
		Enabled *bool  `json:"enabled"`
		Reason  string `json:"reason"`
	}
	if err := decodeStrictJSON(r, &req); err != nil || req.Enabled == nil {
		writeError(w, http.StatusBadRequest,
			"a JSON body with enabled and reason is required")
		return
	}
	if _, err := cfg.Manager.SetMessagingOverride(
		r.Context(), accountID, *req.Enabled, actor, req.Reason,
	); err != nil {
		writeManagerError(w, err)
		return
	}
	writeAdminAccountPolicy(cfg, w, r, accountID, true, "")
}

func adminDeleteMessaging(
	cfg Config,
	w http.ResponseWriter,
	r *http.Request,
	accountID string,
	actor lifecycle.AdminActor,
) {
	var req struct {
		Reason string `json:"reason"`
	}
	if err := decodeStrictJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "a JSON body with reason is required")
		return
	}
	if _, err := cfg.Manager.ClearMessagingOverride(
		r.Context(), accountID, actor, req.Reason,
	); err != nil {
		writeManagerError(w, err)
		return
	}
	writeAdminAccountPolicy(cfg, w, r, accountID, true, "")
}

func adminGetMessageRetention(
	cfg Config,
	w http.ResponseWriter,
	r *http.Request,
	accountID string,
	_ lifecycle.AdminActor,
) {
	writeAdminAccountPolicy(cfg, w, r, accountID, false, "")
}

func adminPutMessageRetention(
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
	if err := decodeStrictJSON(r, &req); err != nil ||
		(req.Days == nil) == !req.Indefinite {
		writeError(w, http.StatusBadRequest,
			"set exactly one of days or indefinite=true")
		return
	}
	var days *int64
	if !req.Indefinite {
		days = req.Days
	}
	if _, err := cfg.Manager.SetMessageRetentionOverride(
		r.Context(), accountID, days, actor, req.Reason,
	); err != nil {
		writeManagerError(w, err)
		return
	}
	writeAdminAccountPolicy(cfg, w, r, accountID, true, "")
}

func adminDeleteMessageRetention(
	cfg Config,
	w http.ResponseWriter,
	r *http.Request,
	accountID string,
	actor lifecycle.AdminActor,
) {
	var req struct {
		Reason string `json:"reason"`
	}
	if err := decodeStrictJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "a JSON body with reason is required")
		return
	}
	if _, err := cfg.Manager.ClearMessageRetentionOverride(
		r.Context(), accountID, actor, req.Reason,
	); err != nil {
		writeManagerError(w, err)
		return
	}
	writeAdminAccountPolicy(cfg, w, r, accountID, true, "")
}

func adminGetAgentEmailReceive(
	cfg Config,
	w http.ResponseWriter,
	r *http.Request,
	accountID string,
	_ lifecycle.AdminActor,
) {
	writeAdminAccountPolicy(cfg, w, r, accountID, false, "")
}

func adminPutAgentEmailReceive(
	cfg Config,
	w http.ResponseWriter,
	r *http.Request,
	accountID string,
	actor lifecycle.AdminActor,
) {
	var req struct {
		Enabled *bool  `json:"enabled"`
		Reason  string `json:"reason"`
	}
	if err := decodeStrictJSON(r, &req); err != nil || req.Enabled == nil {
		writeError(w, http.StatusBadRequest,
			"a JSON body with enabled and reason is required")
		return
	}
	if _, err := cfg.Manager.SetAgentEmailReceiveOverride(
		r.Context(), accountID, *req.Enabled, actor, req.Reason,
	); err != nil {
		writeManagerError(w, err)
		return
	}
	writeAdminAccountPolicy(cfg, w, r, accountID, true, "")
}

func adminDeleteAgentEmailReceive(
	cfg Config,
	w http.ResponseWriter,
	r *http.Request,
	accountID string,
	actor lifecycle.AdminActor,
) {
	var req struct {
		Reason string `json:"reason"`
	}
	if err := decodeStrictJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest,
			"a JSON body with reason is required")
		return
	}
	if _, err := cfg.Manager.ClearAgentEmailReceiveOverride(
		r.Context(), accountID, actor, req.Reason,
	); err != nil {
		writeManagerError(w, err)
		return
	}
	writeAdminAccountPolicy(cfg, w, r, accountID, true, "")
}

func adminGetAgentEmailRetention(
	cfg Config,
	w http.ResponseWriter,
	r *http.Request,
	accountID string,
	_ lifecycle.AdminActor,
) {
	writeAdminAccountPolicy(cfg, w, r, accountID, false, "")
}

func adminPutAgentEmailRetention(
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
	if err := decodeStrictJSON(r, &req); err != nil ||
		(req.Days == nil) == !req.Indefinite {
		writeError(w, http.StatusBadRequest,
			"set exactly one of days or indefinite=true")
		return
	}
	var days *int64
	if !req.Indefinite {
		days = req.Days
	}
	if _, err := cfg.Manager.SetAgentEmailRetentionOverride(
		r.Context(), accountID, days, actor, req.Reason,
	); err != nil {
		writeManagerError(w, err)
		return
	}
	writeAdminAccountPolicy(cfg, w, r, accountID, true, "")
}

func adminDeleteAgentEmailRetention(
	cfg Config,
	w http.ResponseWriter,
	r *http.Request,
	accountID string,
	actor lifecycle.AdminActor,
) {
	var req struct {
		Reason string `json:"reason"`
	}
	if err := decodeStrictJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest,
			"a JSON body with reason is required")
		return
	}
	if _, err := cfg.Manager.ClearAgentEmailRetentionOverride(
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

func decodeBillingMutationApply(
	w http.ResponseWriter,
	r *http.Request,
	operation lifecycle.BillingMutationOperation,
	planRequired bool,
) (lifecycle.BillingMutationCommand, error) {
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	values := r.Header.Values("Idempotency-Key")
	if len(values) != 1 || strings.TrimSpace(values[0]) == "" ||
		values[0] != strings.TrimSpace(values[0]) {
		writeBillingMutationEnvelopeError(w,
			"exactly one non-empty Idempotency-Key header is required")
		return lifecycle.BillingMutationCommand{}, errors.New("invalid idempotency key")
	}
	command := lifecycle.BillingMutationCommand{
		Operation: operation, Confirmed: true,
		IdempotencyKey: values[0],
	}
	if planRequired {
		var req struct {
			Plan      string `json:"plan"`
			Reason    string `json:"reason"`
			Confirmed bool   `json:"confirmed"`
		}
		if err := decodeStrictJSON(r, &req); err != nil {
			writeBillingMutationEnvelopeError(w,
				"a strict plan, reason, and confirmed body is required")
			return lifecycle.BillingMutationCommand{}, err
		}
		command.Plan = strings.TrimSpace(req.Plan)
		command.Reason = strings.TrimSpace(req.Reason)
		command.Confirmed = req.Confirmed
	} else {
		var req struct {
			Reason    string `json:"reason"`
			Confirmed bool   `json:"confirmed"`
		}
		if err := decodeStrictJSON(r, &req); err != nil {
			writeBillingMutationEnvelopeError(w,
				"a strict reason and confirmed body is required")
			return lifecycle.BillingMutationCommand{}, err
		}
		command.Reason = strings.TrimSpace(req.Reason)
		command.Confirmed = req.Confirmed
	}
	if err := validateBillingMutationEnvelope(command, true); err != nil {
		writeBillingMutationEnvelopeError(w, err.Error())
		return lifecycle.BillingMutationCommand{}, err
	}
	return command, nil
}

func validateBillingMutationEnvelope(
	command lifecycle.BillingMutationCommand,
	apply bool,
) error {
	if strings.TrimSpace(command.Reason) == "" {
		return errors.New("reason is required")
	}
	switch command.Operation {
	case lifecycle.BillingMutationSetup, lifecycle.BillingMutationCancel:
		if command.Plan != "" {
			return errors.New("this billing operation cannot include a plan")
		}
	case lifecycle.BillingMutationUpgrade, lifecycle.BillingMutationDowngrade:
		if strings.TrimSpace(command.Plan) == "" {
			return errors.New("plan is required for this billing operation")
		}
	default:
		return errors.New("unsupported billing operation")
	}
	if apply {
		if !command.Confirmed {
			return errors.New("confirmed=true is required")
		}
		if strings.TrimSpace(command.IdempotencyKey) == "" {
			return errors.New("Idempotency-Key is required")
		}
	} else if command.Confirmed || command.IdempotencyKey != "" {
		return errors.New("preview cannot be confirmed or carry an idempotency key")
	}
	return nil
}

func billingActor(access AccountAccess) lifecycle.BillingActor {
	return lifecycle.BillingActor{
		ID: strings.TrimSpace(access.ActorID), Role: strings.TrimSpace(access.Role),
	}
}

func writeBillingMutationExecution(
	w http.ResponseWriter,
	execution lifecycle.BillingMutationExecution,
) {
	out := execution.Outcome
	doc := map[string]any{
		"schema_version": "witself.v0",
		"operation_id":   execution.OperationID,
		"operation":      execution.Operation,
		"actor_id":       execution.Actor.ID,
		"actor_role":     execution.Actor.Role,
		"confirmed":      execution.Confirmed,
		"replayed":       execution.Replayed,
		"kind":           out.Kind,
	}
	if out.Plan != "" {
		doc["plan"] = out.Plan
	}
	if out.URL != "" {
		if !safeBillingURL(out.URL) {
			writeBillingManagerError(w,
				invalidBillingProviderProjection("hosted billing mutation action"))
			return
		}
		doc["url"] = out.URL
	}
	if !out.Effective.IsZero() {
		doc["effective"] = out.Effective
	}
	if out.Kind == "cancelled" {
		doc["cancelled"] = true
	}
	writeJSON(w, http.StatusOK, doc)
}

func planUpgrade(cfg Config, w http.ResponseWriter, r *http.Request, accountID string, access AccountAccess) {
	w.Header().Set("Cache-Control", "no-store")
	command, err := decodeBillingMutationApply(
		w, r, lifecycle.BillingMutationUpgrade, true)
	if err != nil {
		return
	}
	execution, err := cfg.Manager.ExecuteBillingMutation(
		r.Context(), accountID, r.Header.Get("X-Witself-Email"),
		billingActor(access), command)
	if err != nil {
		writeBillingMutationManagerError(w, err)
		return
	}
	writeBillingMutationExecution(w, execution)
}

func planDowngrade(cfg Config, w http.ResponseWriter, r *http.Request, accountID string, access AccountAccess) {
	w.Header().Set("Cache-Control", "no-store")
	command, err := decodeBillingMutationApply(
		w, r, lifecycle.BillingMutationDowngrade, true)
	if err != nil {
		return
	}
	execution, err := cfg.Manager.ExecuteBillingMutation(
		r.Context(), accountID, r.Header.Get("X-Witself-Email"),
		billingActor(access), command)
	if err != nil {
		writeBillingMutationManagerError(w, err)
		return
	}
	writeBillingMutationExecution(w, execution)
}

func planCancel(cfg Config, w http.ResponseWriter, r *http.Request, accountID string, access AccountAccess) {
	w.Header().Set("Cache-Control", "no-store")
	command, err := decodeBillingMutationApply(
		w, r, lifecycle.BillingMutationCancel, false)
	if err != nil {
		return
	}
	execution, err := cfg.Manager.ExecuteBillingMutation(
		r.Context(), accountID, r.Header.Get("X-Witself-Email"),
		billingActor(access), command)
	if err != nil {
		writeBillingMutationManagerError(w, err)
		return
	}
	writeBillingMutationExecution(w, execution)
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

func writeBillingMutationEnvelopeError(w http.ResponseWriter, message string) {
	writeJSON(w, http.StatusBadRequest, map[string]any{
		"schema_version": "witself.v0",
		"code":           "invalid_request",
		"error":          message,
		"retryable":      false,
	})
}

func writeBillingMutationManagerError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, lifecycle.ErrBillingMutationInput):
		writeBillingMutationEnvelopeError(w,
			strings.TrimPrefix(err.Error(), lifecycle.ErrBillingMutationInput.Error()+": "))
	case errors.Is(err, lifecycle.ErrBillingMutationConflict):
		writeJSON(w, http.StatusConflict, map[string]any{
			"schema_version": "witself.v0",
			"code":           "idempotency_conflict",
			"error":          "Idempotency-Key was already used for a different billing request",
			"retryable":      false,
		})
	case errors.Is(err, lifecycle.ErrBillingMutationSuperseded):
		writeJSON(w, http.StatusConflict, map[string]any{
			"schema_version": "witself.v0",
			"code":           "operation_superseded",
			"error":          "billing operation was superseded; preview again and use a new Idempotency-Key",
			"retryable":      false,
		})
	case errors.Is(err, lifecycle.ErrBillingMutationInProgress):
		w.Header().Set("Retry-After", "1")
		writeJSON(w, http.StatusConflict, map[string]any{
			"schema_version": "witself.v0",
			"code":           "operation_in_progress",
			"error":          "billing operation is already in progress; retry the exact request",
			"retryable":      true,
		})
	case errors.Is(err, lifecycle.ErrBillingUnavailable):
		writeBillingManagerError(w, err)
	case errors.Is(err, lifecycle.ErrProviderRequest):
		writeBillingManagerError(w, err)
	case errors.Is(err, lifecycle.ErrRefusal):
		writeManagerError(w, err)
	default:
		writeError(w, http.StatusInternalServerError,
			"billing operation failed — retry the exact request")
	}
}

// writeBillingManagerError keeps provider and storage detail on the server
// side. Only lifecycle refusals are customer-addressed and safe to return.
func writeBillingManagerError(w http.ResponseWriter, err error) {
	if errors.Is(err, lifecycle.ErrBillingUnavailable) {
		writeJSON(w, http.StatusNotImplemented, map[string]any{
			"schema_version": "witself.v0",
			"code":           "unsupported_operation",
			"error":          "billing is not supported by this control plane",
			"retryable":      false,
		})
		return
	}
	if errors.Is(err, lifecycle.ErrProviderRequest) {
		writeError(w, http.StatusBadGateway, "billing provider unavailable — please retry")
		return
	}
	if errors.Is(err, lifecycle.ErrRefusal) {
		writeError(w, http.StatusConflict,
			strings.TrimPrefix(err.Error(), lifecycle.ErrRefusal.Error()+": "))
		return
	}
	writeError(w, http.StatusInternalServerError, "billing request failed — please retry")
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
