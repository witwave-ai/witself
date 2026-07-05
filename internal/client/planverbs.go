package client

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// PlanStatus is the CLI-side view of an account's plan state — enough for
// `witself plan` to render current + pending truthfully.
type PlanStatus struct {
	AccountID    string
	Plan         string
	Applied      string
	PastDueSince *time.Time
	ApplyBlocked string
	Pending      *PlanPending
}

// PlanPending mirrors the CP's pendingView shape.
type PlanPending struct {
	Kind      string
	Plan      string
	URL       string
	Expires   *time.Time
	Effective *time.Time
	Requested time.Time
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
	var wire struct {
		AccountID    string     `json:"account_id"`
		Plan         string     `json:"plan"`
		Applied      string     `json:"applied"`
		PastDueSince *time.Time `json:"past_due_since,omitempty"`
		ApplyBlocked string     `json:"apply_blocked,omitempty"`
		Pending      *struct {
			Kind      string     `json:"kind"`
			Plan      string     `json:"plan"`
			URL       string     `json:"url,omitempty"`
			Expires   *time.Time `json:"expires,omitempty"`
			Effective *time.Time `json:"effective,omitempty"`
			Requested time.Time  `json:"requested"`
		} `json:"pending,omitempty"`
	}
	if err := doJSON(ctx, "GET", url, bearer, nil, &wire); err != nil {
		return PlanStatus{}, err
	}
	s := PlanStatus{
		AccountID: wire.AccountID, Plan: wire.Plan, Applied: wire.Applied,
		PastDueSince: wire.PastDueSince, ApplyBlocked: wire.ApplyBlocked,
	}
	if p := wire.Pending; p != nil {
		s.Pending = &PlanPending{
			Kind: p.Kind, Plan: p.Plan, URL: p.URL,
			Expires: p.Expires, Effective: p.Effective, Requested: p.Requested,
		}
	}
	return s, nil
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
