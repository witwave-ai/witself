package client

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/witwave-ai/witself/internal/plans"
)

// PlanStatus is the CLI-side view of an account's plan state — enough for
// `witself plan` to render current, applied, effective-policy, and pending
// truthfully without exposing administrator attribution or audit history.
type PlanStatus struct {
	SchemaVersion    string               `json:"schema_version"`
	AccountID        string               `json:"account_id"`
	BillingAvailable bool                 `json:"billing_available"`
	Plan             string               `json:"plan"`
	PlanName         string               `json:"plan_name"`
	BillingPlan      string               `json:"billing_plan"`
	BillingPlanName  string               `json:"billing_plan_name"`
	Applied          string               `json:"applied"`
	Limits           map[string]int64     `json:"limits"`
	LimitDefaults    map[string]int64     `json:"limit_defaults"`
	Policies         map[string]int64     `json:"policies"`
	PolicyDefaults   map[string]int64     `json:"policy_defaults"`
	Features         []string             `json:"features"`
	FeatureDefaults  []string             `json:"feature_defaults"`
	Messaging        *PlanFeatureStatus   `json:"messaging,omitempty"`
	MessageRetention *PlanRetentionStatus `json:"message_retention,omitempty"`
	EmailReceive     *PlanFeatureStatus   `json:"email_receive,omitempty"`
	EmailRetention   *PlanRetentionStatus `json:"email_retention,omitempty"`
	Transcript       *PlanRetentionStatus `json:"transcript_retention,omitempty"`
	ApplyPending     bool                 `json:"apply_pending"`
	PastDueSince     *time.Time           `json:"past_due_since,omitempty"`
	ApplyBlocked     string               `json:"apply_blocked,omitempty"`
	Pending          *PlanPending         `json:"pending,omitempty"`
}

// PlanFeatureStatus is the value-free effective/default view for one
// switchable account capability such as messaging or inbound email.
type PlanFeatureStatus struct {
	DefaultEnabled bool `json:"default_enabled"`
	Enabled        bool `json:"enabled"`
	Overridden     bool `json:"overridden"`
}

// PlanRetentionStatus represents finite days with a pointer. A nil value is
// the control plane's explicit indefinite policy, not a missing or zero value.
type PlanRetentionStatus struct {
	DefaultDays   *int64 `json:"default_days"`
	EffectiveDays *int64 `json:"effective_days"`
	Overridden    bool   `json:"overridden"`
}

// PlanPending mirrors the CP's pendingView shape.
type PlanPending struct {
	Kind      string     `json:"kind"`
	Plan      string     `json:"plan"`
	PlanName  string     `json:"plan_name,omitempty"`
	URL       string     `json:"url,omitempty"`
	Expires   *time.Time `json:"expires,omitempty"`
	Effective *time.Time `json:"effective,omitempty"`
	Requested time.Time  `json:"requested"`
}

// PlanOutcome is what a change verb resolved to.
type PlanOutcome struct {
	Kind      string // "done" | "action" | "scheduled" | "contact"
	Plan      string
	URL       string    // set for action (checkout)
	Effective time.Time // set for scheduled downgrades
}

// GetPlan reads current plan state from the control plane.
func GetPlan(ctx context.Context, controlPlane, accountID, bearer string) (PlanStatus, error) {
	url := planURL(controlPlane, accountID, "")
	var status PlanStatus
	if err := doJSON(ctx, http.MethodGet, url, bearer, nil, &status); err != nil {
		return PlanStatus{}, err
	}
	if status.SchemaVersion != "witself.v0" || status.AccountID != accountID ||
		strings.TrimSpace(status.Plan) == "" {
		return PlanStatus{}, fmt.Errorf("control plane returned an invalid plan status")
	}
	return status, nil
}

// GetPlanCatalog reads and validates the public catalog from the control
// plane. The endpoint needs no account binding or bearer token.
func GetPlanCatalog(ctx context.Context, controlPlane string) (*plans.Catalog, error) {
	url := strings.TrimRight(controlPlane, "/") + "/v1/plans"
	var raw json.RawMessage
	if err := doJSON(ctx, http.MethodGet, url, "", nil, &raw); err != nil {
		return nil, err
	}
	catalog, err := plans.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("control plane returned an invalid plan catalog: %w", err)
	}
	return catalog, nil
}

// UpgradePlan runs POST .../plan:upgrade.
func UpgradePlan(ctx context.Context, controlPlane, accountID, bearer, targetPlan, email string) (PlanOutcome, error) {
	return planChange(ctx, controlPlane, accountID, bearer, "upgrade", targetPlan, email)
}

// DowngradePlan runs POST .../plan:downgrade.
func DowngradePlan(ctx context.Context, controlPlane, accountID, bearer, targetPlan, email string) (PlanOutcome, error) {
	return planChange(ctx, controlPlane, accountID, bearer, "downgrade", targetPlan, email)
}

// CancelPlanChange runs POST .../plan:cancel.
func CancelPlanChange(ctx context.Context, controlPlane, accountID, bearer string) error {
	url := planURL(controlPlane, accountID, ":cancel")
	return doJSONWithHeaders(ctx, "POST", url, bearer, nil, nil, nil)
}

func planChange(ctx context.Context, controlPlane, accountID, bearer, verb, targetPlan, email string) (PlanOutcome, error) {
	body, err := json.Marshal(map[string]string{"plan": targetPlan})
	if err != nil {
		return PlanOutcome{}, fmt.Errorf("encode plan: %w", err)
	}
	url := planURL(controlPlane, accountID, ":"+verb)
	headers := map[string]string{}
	if email != "" {
		headers["X-Witself-Email"] = email
	}
	var wire struct {
		Kind      string    `json:"kind"`
		Plan      string    `json:"plan"`
		URL       string    `json:"url,omitempty"`
		Effective time.Time `json:"effective,omitempty"`
	}
	if err := doJSONWithHeaders(ctx, "POST", url, bearer, headers, body, &wire); err != nil {
		return PlanOutcome{}, err
	}
	return PlanOutcome{Kind: wire.Kind, Plan: wire.Plan, URL: wire.URL, Effective: wire.Effective}, nil
}

func planURL(controlPlane, accountID, suffix string) string {
	return strings.TrimRight(controlPlane, "/") + "/v1/accounts/" + accountID + "/plan" + suffix
}
