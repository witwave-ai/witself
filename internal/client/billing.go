package client

import (
	"context"
	"fmt"
	"net"
	"net/http"
	neturl "net/url"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// BillingSummary is the customer-safe account billing projection returned by
// the control plane. Provider and customer identifiers deliberately stay
// private; Configured only says whether a provider relationship exists.
type BillingSummary struct {
	SchemaVersion      string                `json:"schema_version"`
	AccountID          string                `json:"account_id"`
	BillingAvailable   bool                  `json:"billing_available"`
	Configured         bool                  `json:"configured"`
	SubscriptionStatus string                `json:"subscription_status"`
	BillingPlan        string                `json:"billing_plan"`
	BillingPlanName    string                `json:"billing_plan_name"`
	EffectivePlan      string                `json:"effective_plan"`
	EffectivePlanName  string                `json:"effective_plan_name"`
	AppliedPlan        string                `json:"applied_plan"`
	EntitledAt         *time.Time            `json:"entitled_at,omitempty"`
	PastDueSince       *time.Time            `json:"past_due_since,omitempty"`
	PaymentMethod      *BillingPaymentMethod `json:"payment_method"`
	NextCharge         *BillingCharge        `json:"next_charge"`
	Pending            *PlanPending          `json:"pending,omitempty"`
}

// BillingPaymentMethod contains display-only, redacted provider metadata.
type BillingPaymentMethod struct {
	Label string `json:"label"`
}

// BillingCharge is an exact provider-reported amount in cents.
// It is never derived from the current catalog because existing subscribers
// may be grandfathered onto an older price.
type BillingCharge struct {
	Date        time.Time `json:"date"`
	AmountCents int64     `json:"amount_cents"`
	Currency    string    `json:"currency"`
}

// BillingInvoice is one provider-rendered invoice. URLs remain provider-hosted;
// Witself never accepts card data or renders invoice PDFs itself.
type BillingInvoice struct {
	Number      string    `json:"number"`
	Date        time.Time `json:"date"`
	AmountCents int64     `json:"amount_cents"`
	Currency    string    `json:"currency"`
	Status      string    `json:"status"`
	PDFURL      string    `json:"pdf_url,omitempty"`
	HostedURL   string    `json:"hosted_url,omitempty"`
}

// BillingPayment is one normalized provider charge or refund.
type BillingPayment struct {
	Date        time.Time `json:"date"`
	AmountCents int64     `json:"amount_cents"`
	Currency    string    `json:"currency"`
	Method      string    `json:"method"`
	Status      string    `json:"status"`
	ReceiptURL  string    `json:"receipt_url,omitempty"`
}

// BillingInvoices is the bounded recent invoice collection.
type BillingInvoices struct {
	SchemaVersion string           `json:"schema_version"`
	AccountID     string           `json:"account_id"`
	Invoices      []BillingInvoice `json:"invoices"`
}

// BillingPayments is the bounded recent payment collection.
type BillingPayments struct {
	SchemaVersion string           `json:"schema_version"`
	AccountID     string           `json:"account_id"`
	Payments      []BillingPayment `json:"payments"`
}

// BillingAction is either already complete or a provider-hosted HTTPS flow.
type BillingAction struct {
	OperationID string                   `json:"operation_id,omitempty"`
	Operation   BillingMutationOperation `json:"operation,omitempty"`
	ActorID     string                   `json:"actor_id,omitempty"`
	ActorRole   string                   `json:"actor_role,omitempty"`
	Confirmed   bool                     `json:"confirmed,omitempty"`
	Replayed    bool                     `json:"replayed,omitempty"`
	Kind        string                   `json:"kind"` // done | action
	URL         string                   `json:"url,omitempty"`
}

// BillingCapability is the account cell's routing decision for billing. A
// client first authenticates the operator against that cell, then discovers
// this public, value-free projection before sending the bearer to a control
// plane.
type BillingCapability struct {
	Supported bool
	Endpoint  string
	Reason    string
}

// GetBillingCapability authenticates the operator against the selected cell,
// verifies that the token belongs to expectedAccountID, and then reads the
// cell's public billing route. This works for managed cells that host many
// accounts without trusting their deployment-level account block as tenant
// identity.
func GetBillingCapability(
	ctx context.Context,
	cellEndpoint, expectedAccountID, bearer string,
) (BillingCapability, error) {
	if !validBillingAPIEndpoint(cellEndpoint) {
		return BillingCapability{}, fmt.Errorf("unsafe cell endpoint")
	}
	_, tokenAccountID, err := Whoami(ctx, cellEndpoint, bearer)
	if err != nil {
		return BillingCapability{}, fmt.Errorf("authenticate billing cell: %w", err)
	}
	if tokenAccountID != expectedAccountID {
		return BillingCapability{}, fmt.Errorf("billing cell authenticated the wrong account")
	}
	var out struct {
		SchemaVersion string `json:"schema_version"`
		Backend       struct {
			Kind string `json:"kind"`
		} `json:"backend"`
		Account *struct {
			ID string `json:"id"`
		} `json:"account"`
		Billing struct {
			Supported bool   `json:"supported"`
			Endpoint  string `json:"endpoint,omitempty"`
			Reason    string `json:"reason,omitempty"`
		} `json:"billing"`
	}
	url := apiV1Base(cellEndpoint) + "/capabilities"
	if err := doJSON(ctx, http.MethodGet, url, "", nil, &out); err != nil {
		return BillingCapability{}, err
	}
	managed := strings.EqualFold(strings.TrimSpace(out.Backend.Kind), "managed")
	if out.SchemaVersion != "witself.v0" ||
		(!managed &&
			(out.Account == nil || out.Account.ID != expectedAccountID)) {
		return BillingCapability{}, fmt.Errorf("cell returned an invalid billing capability")
	}
	if !out.Billing.Supported {
		if strings.TrimSpace(out.Billing.Endpoint) != "" {
			return BillingCapability{}, fmt.Errorf("cell returned an invalid disabled billing capability")
		}
		return BillingCapability{Reason: strings.TrimSpace(out.Billing.Reason)}, nil
	}
	if !validBillingAPIEndpoint(out.Billing.Endpoint) {
		return BillingCapability{}, fmt.Errorf("cell returned an unsafe billing endpoint")
	}
	return BillingCapability{
		Supported: true,
		Endpoint:  strings.TrimRight(out.Billing.Endpoint, "/"),
	}, nil
}

// GetBillingSummary reads the account's provider-neutral billing projection.
func GetBillingSummary(ctx context.Context, controlPlane, accountID, bearer string) (BillingSummary, error) {
	var out BillingSummary
	if err := doJSON(ctx, http.MethodGet, billingURL(controlPlane, accountID, ""), bearer, nil, &out); err != nil {
		return BillingSummary{}, err
	}
	if err := validateBillingEnvelope(out.SchemaVersion, out.AccountID, accountID); err != nil {
		return BillingSummary{}, err
	}
	if strings.TrimSpace(out.BillingPlan) == "" || strings.TrimSpace(out.EffectivePlan) == "" {
		return BillingSummary{}, fmt.Errorf("control plane returned an invalid billing summary")
	}
	if out.Pending != nil && out.Pending.URL != "" && !validBillingHostedURL(out.Pending.URL) {
		return BillingSummary{}, fmt.Errorf("control plane returned an unsafe pending billing URL")
	}
	return out, nil
}

// GetBillingInvoices reads the provider's bounded recent invoice history.
func GetBillingInvoices(ctx context.Context, controlPlane, accountID, bearer string) (BillingInvoices, error) {
	var out BillingInvoices
	if err := doJSON(ctx, http.MethodGet, billingURL(controlPlane, accountID, "/invoices"), bearer, nil, &out); err != nil {
		return BillingInvoices{}, err
	}
	if err := validateBillingEnvelope(out.SchemaVersion, out.AccountID, accountID); err != nil {
		return BillingInvoices{}, err
	}
	if out.Invoices == nil {
		out.Invoices = []BillingInvoice{}
	}
	if len(out.Invoices) > 100 {
		return BillingInvoices{}, fmt.Errorf("control plane returned too many invoices")
	}
	for _, invoice := range out.Invoices {
		if (invoice.PDFURL != "" && !validBillingHostedURL(invoice.PDFURL)) ||
			(invoice.HostedURL != "" && !validBillingHostedURL(invoice.HostedURL)) {
			return BillingInvoices{}, fmt.Errorf("control plane returned an unsafe invoice URL")
		}
	}
	return out, nil
}

// GetBillingPayments reads the provider's bounded recent payment history.
func GetBillingPayments(ctx context.Context, controlPlane, accountID, bearer string) (BillingPayments, error) {
	var out BillingPayments
	if err := doJSON(ctx, http.MethodGet, billingURL(controlPlane, accountID, "/payments"), bearer, nil, &out); err != nil {
		return BillingPayments{}, err
	}
	if err := validateBillingEnvelope(out.SchemaVersion, out.AccountID, accountID); err != nil {
		return BillingPayments{}, err
	}
	if out.Payments == nil {
		out.Payments = []BillingPayment{}
	}
	if len(out.Payments) > 100 {
		return BillingPayments{}, fmt.Errorf("control plane returned too many payments")
	}
	for _, payment := range out.Payments {
		if payment.ReceiptURL != "" && !validBillingHostedURL(payment.ReceiptURL) {
			return BillingPayments{}, fmt.Errorf("control plane returned an unsafe payment URL")
		}
	}
	return out, nil
}

// CreateBillingPortal requests a provider-hosted self-service portal URL.
func CreateBillingPortal(ctx context.Context, controlPlane, accountID, bearer string) (BillingAction, error) {
	return billingAction(ctx, billingURL(controlPlane, accountID, ":portal"), bearer, "")
}

// CreateBillingSetup requests a guarded provider-hosted payment-method setup
// flow. email is only a customer hint on first provider contact; raw payment
// details never cross this API.
func CreateBillingSetup(
	ctx context.Context,
	controlPlane, accountID, bearer, email string,
	options BillingMutationOptions,
) (BillingAction, error) {
	body, err := encodeBillingMutationApplyBody("", options)
	if err != nil {
		return BillingAction{}, err
	}
	headers := map[string]string{"Idempotency-Key": options.IdempotencyKey}
	if strings.TrimSpace(email) != "" {
		headers["X-Witself-Email"] = email
	}
	var wire struct {
		SchemaVersion string                   `json:"schema_version"`
		OperationID   string                   `json:"operation_id"`
		Operation     BillingMutationOperation `json:"operation"`
		ActorID       string                   `json:"actor_id"`
		ActorRole     string                   `json:"actor_role"`
		Confirmed     bool                     `json:"confirmed"`
		Replayed      bool                     `json:"replayed"`
		Kind          string                   `json:"kind"`
		URL           string                   `json:"url,omitempty"`
		Cancelled     bool                     `json:"cancelled"`
	}
	if err := doJSONWithHeadersTimeout(ctx, http.MethodPost,
		billingURL(controlPlane, accountID, ":setup"), bearer, headers, body, &wire,
		billingMutationTransportTimeout); err != nil {
		return BillingAction{}, err
	}
	if wire.SchemaVersion != "witself.v0" || wire.Operation != BillingMutationSetup ||
		strings.TrimSpace(wire.OperationID) == "" ||
		strings.TrimSpace(wire.ActorID) == "" || strings.TrimSpace(wire.ActorRole) == "" ||
		!wire.Confirmed || wire.Cancelled {
		return BillingAction{}, fmt.Errorf("control plane returned an invalid billing setup outcome")
	}
	if err := validateBillingAction(wire.Kind, wire.URL); err != nil {
		return BillingAction{}, err
	}
	return BillingAction{
		OperationID: wire.OperationID, Operation: wire.Operation,
		ActorID: wire.ActorID, ActorRole: wire.ActorRole,
		Confirmed: wire.Confirmed, Replayed: wire.Replayed,
		Kind: wire.Kind, URL: wire.URL,
	}, nil
}

func billingAction(ctx context.Context, url, bearer, email string) (BillingAction, error) {
	headers := map[string]string{}
	if strings.TrimSpace(email) != "" {
		headers["X-Witself-Email"] = email
	}
	var wire struct {
		SchemaVersion string `json:"schema_version"`
		Kind          string `json:"kind"`
		URL           string `json:"url,omitempty"`
	}
	if err := doJSONWithHeaders(ctx, http.MethodPost, url, bearer, headers, nil, &wire); err != nil {
		return BillingAction{}, err
	}
	if wire.SchemaVersion != "witself.v0" {
		return BillingAction{}, fmt.Errorf("control plane returned an invalid billing action schema")
	}
	if err := validateBillingAction(wire.Kind, wire.URL); err != nil {
		return BillingAction{}, err
	}
	return BillingAction{Kind: wire.Kind, URL: wire.URL}, nil
}

func validateBillingAction(kind, url string) error {
	switch kind {
	case "done":
		if url != "" {
			return fmt.Errorf("control plane returned an invalid completed billing action")
		}
	case "action":
		if !validBillingHostedURL(url) {
			return fmt.Errorf("control plane returned an unsafe billing action URL")
		}
	default:
		return fmt.Errorf("control plane returned an unknown billing action")
	}
	return nil
}

func validateBillingEnvelope(schemaVersion, gotAccountID, wantAccountID string) error {
	if schemaVersion != "witself.v0" || gotAccountID != wantAccountID {
		return fmt.Errorf("control plane returned an invalid billing response")
	}
	return nil
}

func billingURL(controlPlane, accountID, suffix string) string {
	return apiV1Base(controlPlane) + "/accounts/" + accountID + "/billing" + suffix
}

// validBillingHostedURL is the final client-side trust boundary for provider
// links. The control plane validates the same properties, but a CLI must not
// print, return, or open an active URL merely because an older or compromised
// server serialized it.
func validBillingHostedURL(raw string) bool {
	if raw == "" || raw != strings.TrimSpace(raw) || !utf8.ValidString(raw) ||
		strings.ContainsRune(raw, '\\') || billingURLHasUnsafeRune(raw) {
		return false
	}
	decoded, err := neturl.PathUnescape(raw)
	if err != nil || strings.ContainsRune(decoded, '\\') || billingURLHasUnsafeRune(decoded) {
		return false
	}
	parsed, err := neturl.Parse(raw)
	return err == nil && strings.EqualFold(parsed.Scheme, "https") &&
		parsed.Opaque == "" && parsed.User == nil && parsed.Host != "" &&
		parsed.Hostname() != ""
}

func validBillingAPIEndpoint(raw string) bool {
	if raw == "" || raw != strings.TrimSpace(raw) || !utf8.ValidString(raw) ||
		strings.ContainsRune(raw, '\\') || billingURLHasUnsafeRune(raw) {
		return false
	}
	parsed, err := neturl.Parse(raw)
	if err != nil || parsed.Host == "" || parsed.Hostname() == "" ||
		parsed.User != nil || parsed.Opaque != "" || parsed.RawQuery != "" ||
		parsed.Fragment != "" {
		return false
	}
	switch strings.ToLower(parsed.Scheme) {
	case "https":
		return true
	case "http":
		host := parsed.Hostname()
		if strings.EqualFold(host, "localhost") {
			return true
		}
		ip := net.ParseIP(host)
		return ip != nil && ip.IsLoopback()
	default:
		return false
	}
}

func billingURLHasUnsafeRune(raw string) bool {
	for _, r := range raw {
		if unicode.IsControl(r) || r == '\u2028' || r == '\u2029' || isBillingBidiControl(r) {
			return true
		}
	}
	return false
}

func isBillingBidiControl(r rune) bool {
	switch r {
	case '\u061c', '\u200e', '\u200f', '\u202a', '\u202b', '\u202c',
		'\u202d', '\u202e', '\u2066', '\u2067', '\u2068', '\u2069':
		return true
	default:
		return false
	}
}
