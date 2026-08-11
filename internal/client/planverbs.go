package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/witwave-ai/witself/internal/plans"
)

const billingMutationTransportTimeout = 5 * time.Minute

var (
	// ErrBillingMutationIdempotencyConflict means one account-scoped retry key
	// was reused with changed immutable request semantics.
	ErrBillingMutationIdempotencyConflict = errors.New("billing mutation idempotency conflict")
	// ErrBillingMutationInProgress means the exact operation is currently
	// owned by another control-plane worker and may be retried as-is.
	ErrBillingMutationInProgress = errors.New("billing mutation in progress")
	// ErrBillingMutationSuperseded means a newer account operation owns the
	// billing lane; callers must preview again and use a new retry key.
	ErrBillingMutationSuperseded = errors.New("billing mutation superseded")
)

// BillingMutationError preserves the safe retry contract returned by guarded
// billing apply routes. Provider, account, request, and raw key details are
// intentionally absent.
type BillingMutationError struct {
	Code       string
	Retryable  bool
	RetryAfter time.Duration
	Message    string
}

func (e *BillingMutationError) Error() string {
	if e != nil && strings.TrimSpace(e.Message) != "" {
		return e.Message
	}
	if sentinel := billingMutationErrorSentinel(e); sentinel != nil {
		return sentinel.Error()
	}
	return "billing mutation failed"
}

func (e *BillingMutationError) Unwrap() error {
	return billingMutationErrorSentinel(e)
}

func billingMutationErrorSentinel(e *BillingMutationError) error {
	if e == nil {
		return nil
	}
	switch e.Code {
	case "idempotency_conflict":
		return ErrBillingMutationIdempotencyConflict
	case "operation_in_progress":
		return ErrBillingMutationInProgress
	case "operation_superseded":
		return ErrBillingMutationSuperseded
	default:
		return nil
	}
}

func recognizedBillingMutationErrorCode(code string) bool {
	return billingMutationErrorSentinel(&BillingMutationError{Code: code}) != nil
}

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

// BillingMutationOperation is the stable wire identity for one customer
// billing mutation. Preview and apply use the same operation name, but only an
// apply request carries confirmation and an idempotency key.
type BillingMutationOperation string

const (
	// BillingMutationSetup creates or replays a hosted payment setup flow.
	BillingMutationSetup BillingMutationOperation = "billing_setup"
	// BillingMutationUpgrade purchases or requests a higher plan.
	BillingMutationUpgrade BillingMutationOperation = "plan_upgrade"
	// BillingMutationDowngrade schedules a lower plan.
	BillingMutationDowngrade BillingMutationOperation = "plan_downgrade"
	// BillingMutationCancel cancels one pending plan change.
	BillingMutationCancel BillingMutationOperation = "plan_cancel"
)

// BillingMutationOptions is the explicit mutation guard envelope. Callers
// must choose confirmation; the client never silently upgrades an old call to
// a confirmed operation.
type BillingMutationOptions struct {
	Reason         string
	Confirmed      bool
	IdempotencyKey string
}

// BillingMutationPreview is a side-effect-free policy result. Effects and
// violations are bounded, value-free descriptions supplied by the control
// plane; no provider flow or durable mutation receipt is created.
type BillingMutationPreview struct {
	SchemaVersion        string                   `json:"schema_version"`
	Operation            BillingMutationOperation `json:"operation"`
	Plan                 string                   `json:"plan,omitempty"`
	Allowed              bool                     `json:"allowed"`
	ConfirmationRequired bool                     `json:"confirmation_required"`
	Effects              []string                 `json:"effects"`
	Violations           []string                 `json:"violations"`
}

// PlanOutcome is what a change verb resolved to.
type PlanOutcome struct {
	OperationID string                   `json:"operation_id"`
	Operation   BillingMutationOperation `json:"operation"`
	ActorID     string                   `json:"actor_id"`
	ActorRole   string                   `json:"actor_role"`
	Confirmed   bool                     `json:"confirmed"`
	Replayed    bool                     `json:"replayed"`
	Kind        string                   `json:"kind"` // done | action | scheduled | cancelled | resolved | contact
	Plan        string                   `json:"plan,omitempty"`
	URL         string                   `json:"url,omitempty"`       // action (checkout)
	Effective   time.Time                `json:"effective,omitempty"` // scheduled downgrade
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
	url := apiV1Base(controlPlane) + "/plans"
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

// PreviewBillingMutation evaluates a billing change without creating a
// receipt or calling a provider.
func PreviewBillingMutation(
	ctx context.Context,
	controlPlane, accountID, bearer string,
	operation BillingMutationOperation,
	targetPlan, email, reason string,
) (BillingMutationPreview, error) {
	body, err := json.Marshal(struct {
		Operation BillingMutationOperation `json:"operation"`
		Plan      string                   `json:"plan,omitempty"`
		Reason    string                   `json:"reason"`
	}{Operation: operation, Plan: targetPlan, Reason: reason})
	if err != nil {
		return BillingMutationPreview{}, fmt.Errorf("encode billing mutation preview: %w", err)
	}
	var preview BillingMutationPreview
	headers := map[string]string{}
	if strings.TrimSpace(email) != "" {
		headers["X-Witself-Email"] = email
	}
	if err := doJSONWithHeaders(ctx, http.MethodPost,
		billingURL(controlPlane, accountID, ":preview"), bearer, headers, body, &preview); err != nil {
		return BillingMutationPreview{}, err
	}
	if preview.SchemaVersion != "witself.v0" || preview.Operation != operation ||
		strings.TrimSpace(string(preview.Operation)) == "" ||
		((operation == BillingMutationUpgrade || operation == BillingMutationDowngrade) &&
			preview.Plan != targetPlan) ||
		((operation == BillingMutationSetup || operation == BillingMutationCancel) &&
			preview.Plan != "") || !preview.ConfirmationRequired ||
		preview.Allowed != (len(preview.Violations) == 0) {
		return BillingMutationPreview{}, fmt.Errorf("control plane returned an invalid billing mutation preview")
	}
	if preview.Effects == nil {
		preview.Effects = []string{}
	}
	if preview.Violations == nil {
		preview.Violations = []string{}
	}
	return preview, nil
}

// UpgradePlan runs a guarded POST .../plan:upgrade.
func UpgradePlan(
	ctx context.Context,
	controlPlane, accountID, bearer, targetPlan, email string,
	options BillingMutationOptions,
) (PlanOutcome, error) {
	return planChange(ctx, controlPlane, accountID, bearer,
		BillingMutationUpgrade, "upgrade", targetPlan, email, options)
}

// DowngradePlan runs a guarded POST .../plan:downgrade.
func DowngradePlan(
	ctx context.Context,
	controlPlane, accountID, bearer, targetPlan, email string,
	options BillingMutationOptions,
) (PlanOutcome, error) {
	return planChange(ctx, controlPlane, accountID, bearer,
		BillingMutationDowngrade, "downgrade", targetPlan, email, options)
}

// CancelPlanChange runs a guarded POST .../plan:cancel.
func CancelPlanChange(
	ctx context.Context,
	controlPlane, accountID, bearer string,
	options BillingMutationOptions,
) (PlanOutcome, error) {
	body, err := encodeBillingMutationApplyBody("", options)
	if err != nil {
		return PlanOutcome{}, err
	}
	return doPlanMutation(ctx, planURL(controlPlane, accountID, ":cancel"),
		bearer, "", BillingMutationCancel, body, options.IdempotencyKey)
}

func planChange(
	ctx context.Context,
	controlPlane, accountID, bearer string,
	operation BillingMutationOperation,
	verb, targetPlan, email string,
	options BillingMutationOptions,
) (PlanOutcome, error) {
	body, err := encodeBillingMutationApplyBody(targetPlan, options)
	if err != nil {
		return PlanOutcome{}, err
	}
	url := planURL(controlPlane, accountID, ":"+verb)
	out, err := doPlanMutation(ctx, url, bearer, email, operation, body,
		options.IdempotencyKey)
	if err != nil {
		return PlanOutcome{}, err
	}
	if out.Plan != targetPlan {
		return PlanOutcome{}, fmt.Errorf("control plane returned an invalid billing mutation outcome")
	}
	return out, nil
}

func encodeBillingMutationApplyBody(targetPlan string, options BillingMutationOptions) ([]byte, error) {
	body, err := json.Marshal(struct {
		Plan      string `json:"plan,omitempty"`
		Reason    string `json:"reason"`
		Confirmed bool   `json:"confirmed"`
	}{Plan: targetPlan, Reason: options.Reason, Confirmed: options.Confirmed})
	if err != nil {
		return nil, fmt.Errorf("encode billing mutation: %w", err)
	}
	return body, nil
}

func doPlanMutation(
	ctx context.Context,
	url, bearer, email string,
	operation BillingMutationOperation,
	body []byte,
	idempotencyKey string,
) (PlanOutcome, error) {
	headers := map[string]string{"Idempotency-Key": idempotencyKey}
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
		Plan          string                   `json:"plan"`
		URL           string                   `json:"url,omitempty"`
		Effective     time.Time                `json:"effective,omitempty"`
		Cancelled     bool                     `json:"cancelled"`
	}
	if err := doJSONWithHeadersTimeout(ctx, http.MethodPost, url, bearer,
		headers, body, &wire, billingMutationTransportTimeout); err != nil {
		return PlanOutcome{}, err
	}
	if wire.SchemaVersion != "witself.v0" || wire.Operation != operation ||
		strings.TrimSpace(wire.OperationID) == "" ||
		strings.TrimSpace(wire.ActorID) == "" || strings.TrimSpace(wire.ActorRole) == "" ||
		!wire.Confirmed {
		return PlanOutcome{}, fmt.Errorf("control plane returned an invalid billing mutation outcome")
	}
	switch wire.Kind {
	case "done":
		if operation != BillingMutationUpgrade || strings.TrimSpace(wire.Plan) == "" ||
			wire.URL != "" || !wire.Effective.IsZero() || wire.Cancelled {
			return PlanOutcome{}, fmt.Errorf("control plane returned an invalid billing mutation outcome")
		}
	case "action":
		if operation != BillingMutationUpgrade || strings.TrimSpace(wire.Plan) == "" ||
			!validBillingHostedURL(wire.URL) || !wire.Effective.IsZero() || wire.Cancelled {
			return PlanOutcome{}, fmt.Errorf("control plane returned an invalid billing mutation outcome")
		}
	case "scheduled":
		if operation != BillingMutationDowngrade || strings.TrimSpace(wire.Plan) == "" ||
			wire.URL != "" || wire.Effective.IsZero() || wire.Cancelled {
			return PlanOutcome{}, fmt.Errorf("control plane returned an invalid billing mutation outcome")
		}
	case "contact":
		if operation != BillingMutationUpgrade || strings.TrimSpace(wire.Plan) == "" ||
			wire.URL != "" || !wire.Effective.IsZero() || wire.Cancelled {
			return PlanOutcome{}, fmt.Errorf("control plane returned an invalid billing mutation outcome")
		}
	case "cancelled":
		if operation != BillingMutationCancel || wire.Plan != "" || wire.URL != "" ||
			!wire.Effective.IsZero() || !wire.Cancelled {
			return PlanOutcome{}, fmt.Errorf("control plane returned an invalid billing mutation outcome")
		}
	case "resolved":
		if operation != BillingMutationCancel || wire.Plan != "" || wire.URL != "" ||
			!wire.Effective.IsZero() || wire.Cancelled {
			return PlanOutcome{}, fmt.Errorf("control plane returned an invalid billing mutation outcome")
		}
	default:
		return PlanOutcome{}, fmt.Errorf("control plane returned an unknown billing mutation outcome")
	}
	return PlanOutcome{
		OperationID: wire.OperationID, Operation: wire.Operation,
		ActorID: wire.ActorID, ActorRole: wire.ActorRole,
		Confirmed: wire.Confirmed, Replayed: wire.Replayed,
		Kind: wire.Kind, Plan: wire.Plan, URL: wire.URL, Effective: wire.Effective,
	}, nil
}

func planURL(controlPlane, accountID, suffix string) string {
	return apiV1Base(controlPlane) + "/accounts/" + accountID + "/plan" + suffix
}

// apiV1Base accepts either a deployment origin/prefix or an advertised API
// base that already ends in /v1. Cells historically documented both shapes;
// normalizing here prevents /v1/v1 for discovery and control-plane calls.
func apiV1Base(endpoint string) string {
	base := strings.TrimRight(endpoint, "/")
	if strings.HasSuffix(base, "/v1") {
		return base
	}
	return base + "/v1"
}
