package client

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	neturl "net/url"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/witwave-ai/witself/internal/placement"
)

// Admin is the public shape of a fleet-admin credential (as returned
// by the control-plane admin registry). The raw token is only present
// on MintAdminResult — it's shown once, never persisted here.
type Admin struct {
	AdminID    string     `json:"admin_id"`
	Handle     string     `json:"handle"`
	Note       string     `json:"note,omitempty"`
	CreatedAt  time.Time  `json:"created_at"`
	CreatedBy  string     `json:"created_by,omitempty"`
	Disabled   bool       `json:"disabled"`
	DisabledAt *time.Time `json:"disabled_at,omitempty"`
}

// MintAdminInput is the payload for MintAdmin. Note is optional
// bookkeeping (up to 200 chars).
type MintAdminInput struct {
	Handle string `json:"handle"`
	Note   string `json:"note,omitempty"`
}

// MintAdminResult carries the newly minted admin AND the raw token,
// which will only ever appear once.
type MintAdminResult struct {
	AdminToken string `json:"admin_token"`
	Admin      Admin  `json:"admin"`
}

// MintAdmin creates a new fleet-admin credential. Requires the shared
// fleet token (WITSELF_FLEET_TOKEN in the CLI).
func MintAdmin(ctx context.Context, cpEndpoint, fleetToken string, in MintAdminInput) (*MintAdminResult, error) {
	body, err := json.Marshal(in)
	if err != nil {
		return nil, err
	}
	url := strings.TrimRight(cpEndpoint, "/") + "/v1/admins"
	var out MintAdminResult
	if err := doJSON(ctx, http.MethodPost, url, fleetToken, body, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ListAdmins returns every admin ever minted (including revoked ones).
// Requires the fleet token.
func ListAdmins(ctx context.Context, cpEndpoint, fleetToken string) ([]Admin, error) {
	url := strings.TrimRight(cpEndpoint, "/") + "/v1/admins"
	var out struct {
		Admins []Admin `json:"admins"`
	}
	if err := doJSON(ctx, http.MethodGet, url, fleetToken, nil, &out); err != nil {
		return nil, err
	}
	return out.Admins, nil
}

// RevokeAdmin soft-disables an admin: the token stops authenticating on
// the next request, but the record + reserved handle remain so past
// events keep a distinguishable author. Requires the fleet token.
func RevokeAdmin(ctx context.Context, cpEndpoint, fleetToken, adminID string) (*Admin, error) {
	url := strings.TrimRight(cpEndpoint, "/") + "/v1/admins/" + adminID + ":revoke"
	var out struct {
		Admin Admin `json:"admin"`
	}
	if err := doJSON(ctx, http.MethodPost, url, fleetToken, []byte("{}"), &out); err != nil {
		return nil, err
	}
	return &out.Admin, nil
}

// DeleteAdmin removes a previously-revoked admin record entirely,
// freeing the handle for reuse. Requires the fleet token.
func DeleteAdmin(ctx context.Context, cpEndpoint, fleetToken, adminID string) error {
	url := strings.TrimRight(cpEndpoint, "/") + "/v1/admins/" + adminID
	return doJSON(ctx, http.MethodDelete, url, fleetToken, nil, nil)
}

// AdminWhoami is a cheap round-trip that also verifies the admin token
// is live. Returns the admin's id + handle.
type AdminWhoami struct {
	AdminID string `json:"admin_id"`
	Handle  string `json:"handle"`
}

// GetAdminWhoami calls /v1/admin/whoami with the admin token.
func GetAdminWhoami(ctx context.Context, cpEndpoint, adminToken string) (*AdminWhoami, error) {
	url := strings.TrimRight(cpEndpoint, "/") + "/v1/admin/whoami"
	var out AdminWhoami
	if err := doJSON(ctx, http.MethodGet, url, adminToken, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// AdminTicket is a SupportTicket annotated with the cell it lives on
// (the CP tags each row during fan-out).
type AdminTicket struct {
	SupportTicket
	Cell string `json:"cell"`
}

// AdminCellStatus reports how one cell responded to the fan-out.
type AdminCellStatus struct {
	Name   string `json:"name"`
	Status string `json:"status"` // "ok" | "error" | "timeout"
	Count  int    `json:"count"`
	Error  string `json:"error,omitempty"`
}

// AdminTicketFilter constrains ListAdminTickets. Empty States means any
// state; zero Limit is server default (100, per-cell).
type AdminTicketFilter struct {
	States []string
	Since  *time.Time
	Limit  int
}

// AdminTicketList is the fleet-wide list result.
type AdminTicketList struct {
	Tickets         []AdminTicket     `json:"tickets"`
	Cells           []AdminCellStatus `json:"cells"`
	AggregateCapped bool              `json:"aggregate_capped,omitempty"`
}

// ListAdminTickets fetches the fleet-wide fan-out list of tickets.
func ListAdminTickets(ctx context.Context, cpEndpoint, adminToken string, f AdminTicketFilter) (*AdminTicketList, error) {
	url := strings.TrimRight(cpEndpoint, "/") + "/v1/admin/tickets"
	params := neturl.Values{}
	if len(f.States) > 0 {
		params.Set("state", strings.Join(f.States, ","))
	}
	if f.Since != nil {
		params.Set("since", f.Since.UTC().Format(time.RFC3339))
	}
	if f.Limit > 0 {
		params.Set("limit", strconv.Itoa(f.Limit))
	}
	if len(params) > 0 {
		url += "?" + params.Encode()
	}
	var out AdminTicketList
	if err := doJSON(ctx, http.MethodGet, url, adminToken, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// GetAdminTicket fetches one ticket + its full thread by (account, ticket).
func GetAdminTicket(ctx context.Context, cpEndpoint, adminToken, accountID, ticketID string) (*GetSupportTicketResult, error) {
	url := fmt.Sprintf("%s/v1/admin/accounts/%s/tickets/%s",
		strings.TrimRight(cpEndpoint, "/"), accountID, ticketID)
	var out GetSupportTicketResult
	if err := doJSON(ctx, http.MethodGet, url, adminToken, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ReplyAdminTicket posts a reply on behalf of the authenticated admin.
// The admin handle is derived server-side from the token.
func ReplyAdminTicket(ctx context.Context, cpEndpoint, adminToken, accountID, ticketID, body string) (*SupportTicketMessage, error) {
	payload, err := json.Marshal(map[string]string{"body": body})
	if err != nil {
		return nil, err
	}
	url := fmt.Sprintf("%s/v1/admin/accounts/%s/tickets/%s/messages",
		strings.TrimRight(cpEndpoint, "/"), accountID, ticketID)
	var out struct {
		Message SupportTicketMessage `json:"message"`
	}
	if err := doJSON(ctx, http.MethodPost, url, adminToken, payload, &out); err != nil {
		return nil, err
	}
	return &out.Message, nil
}

// AdminCell is one fleet cell as the dashboard sees it: the public
// registry entry plus the CP-directory account count and the software
// version the cell reported at fan-out time (empty = unreachable).
type AdminCell struct {
	Name              string `json:"name"`
	Cloud             string `json:"cloud,omitempty"`
	Region            string `json:"region,omitempty"`
	RegionCode        string `json:"region_code,omitempty"`
	Channel           string `json:"channel,omitempty"`
	Endpoint          string `json:"endpoint,omitempty"`
	Accepting         bool   `json:"accepting"`
	HasProvisionToken bool   `json:"has_provision_token"`
	AccountCount      int    `json:"account_count"`
	ArchivedCount     int    `json:"archived_count"`
	Version           string `json:"version,omitempty"`
}

// ListAdminCells returns the fleet registry with per-cell account
// counts (admin-token authorized).
func ListAdminCells(ctx context.Context, cpEndpoint, adminToken string) ([]AdminCell, error) {
	url := strings.TrimRight(cpEndpoint, "/") + "/v1/admin/cells"
	var out struct {
		Cells []AdminCell `json:"cells"`
	}
	if err := doJSON(ctx, http.MethodGet, url, adminToken, nil, &out); err != nil {
		return nil, err
	}
	return out.Cells, nil
}

// AdminEvent is one audit-ledger row annotated with its origin cell.
type AdminEvent struct {
	ID         string          `json:"id"`
	AccountID  string          `json:"account_id"`
	OccurredAt time.Time       `json:"occurred_at"`
	ActorKind  string          `json:"actor_kind"`
	ActorID    string          `json:"actor_id,omitempty"`
	Verb       string          `json:"verb"`
	Metadata   json.RawMessage `json:"metadata"`
	Cell       string          `json:"cell"`
}

// AdminEventFilter constrains ListAdminEvents. Zero Limit is the
// server default (50, per-cell).
type AdminEventFilter struct {
	Since *time.Time
	Verb  string
	Limit int
}

// AdminEventList is the fleet-wide fan-out result.
type AdminEventList struct {
	Events          []AdminEvent      `json:"events"`
	Cells           []AdminCellStatus `json:"cells"`
	AggregateCapped bool              `json:"aggregate_capped,omitempty"`
}

// ListAdminEvents fetches the fleet-wide audit-event tail.
func ListAdminEvents(ctx context.Context, cpEndpoint, adminToken string, f AdminEventFilter) (*AdminEventList, error) {
	url := strings.TrimRight(cpEndpoint, "/") + "/v1/admin/events"
	params := neturl.Values{}
	if f.Since != nil {
		params.Set("since", f.Since.UTC().Format(time.RFC3339))
	}
	if f.Verb != "" {
		params.Set("verb", f.Verb)
	}
	if f.Limit > 0 {
		params.Set("limit", strconv.Itoa(f.Limit))
	}
	if len(params) > 0 {
		url += "?" + params.Encode()
	}
	var out AdminEventList
	if err := doJSON(ctx, http.MethodGet, url, adminToken, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// SupportPolicyRead is the response shape from
// GET /v1/admin/accounts/{a}/support-policy.
type SupportPolicyRead struct {
	AccountID     string `json:"account_id"`
	SupportPolicy string `json:"support_policy"`
}

// GetAdminSupportPolicy reads the current support_policy on account.
func GetAdminSupportPolicy(ctx context.Context, cpEndpoint, adminToken, accountID string) (*SupportPolicyRead, error) {
	url := fmt.Sprintf("%s/v1/admin/accounts/%s/support-policy",
		strings.TrimRight(cpEndpoint, "/"), accountID)
	var out SupportPolicyRead
	if err := doJSON(ctx, http.MethodGet, url, adminToken, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// SupportPolicyChange is the response shape from
// PATCH /v1/admin/accounts/{a}/support-policy — reports the old and
// new value so a caller can render "flipped enabled -> disabled".
type SupportPolicyChange struct {
	AccountID  string `json:"account_id"`
	PolicyFrom string `json:"policy_from"`
	PolicyTo   string `json:"policy_to"`
}

// SetAdminSupportPolicy flips an account's support_policy to newPolicy.
// Idempotent — same-value calls return {policy_from == policy_to}
// without an audit event.
func SetAdminSupportPolicy(ctx context.Context, cpEndpoint, adminToken, accountID, newPolicy string) (*SupportPolicyChange, error) {
	body, err := json.Marshal(map[string]string{"policy": newPolicy})
	if err != nil {
		return nil, err
	}
	url := fmt.Sprintf("%s/v1/admin/accounts/%s/support-policy",
		strings.TrimRight(cpEndpoint, "/"), accountID)
	var out SupportPolicyChange
	if err := doJSON(ctx, http.MethodPatch, url, adminToken, body, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// AdminAccountPolicy is the resolved control-plane view returned by both
// transcript-retention and plan-override admin routes. BillingPlan remains the
// provider-backed entitlement; Plan is the effective classification after an
// optional administrator override.
type AdminAccountPolicy struct {
	SchemaVersion       string                            `json:"schema_version"`
	AccountID           string                            `json:"account_id"`
	Plan                string                            `json:"plan"`
	BillingPlan         string                            `json:"billing_plan"`
	Applied             string                            `json:"applied"`
	PlanOverride        *AdminAccountPlanOverride         `json:"plan_override"`
	TranscriptRetention AdminTranscriptRetention          `json:"transcript_retention"`
	AdminHistory        []AdminAccountPolicyHistoryChange `json:"admin_history"`
	ApplyPending        bool                              `json:"apply_pending"`
	DesiredRevision     int64                             `json:"desired_revision"`
	AppliedRevision     int64                             `json:"applied_revision"`
}

// AdminAccountPlanOverride is the audited account classification exception.
type AdminAccountPlanOverride struct {
	Plan        string    `json:"plan"`
	ActorID     string    `json:"actor_id"`
	ActorHandle string    `json:"actor_handle"`
	Reason      string    `json:"reason"`
	SetAt       time.Time `json:"set_at"`
}

// AdminTranscriptRetention describes inherited and effective retention.
// A nil day value means indefinite. Override is present only when Overridden
// is true; Override.Days=nil represents an explicit indefinite exception.
type AdminTranscriptRetention struct {
	DefaultDays   *int64                            `json:"default_days"`
	EffectiveDays *int64                            `json:"effective_days"`
	Overridden    bool                              `json:"overridden"`
	Override      *AdminTranscriptRetentionOverride `json:"override,omitempty"`
}

// AdminTranscriptRetentionOverride is the audited account retention exception.
// Days is nil when the administrator selected explicit indefinite retention.
type AdminTranscriptRetentionOverride struct {
	Days        *int64    `json:"days"`
	ActorID     string    `json:"actor_id"`
	ActorHandle string    `json:"actor_handle"`
	Reason      string    `json:"reason"`
	SetAt       time.Time `json:"set_at"`
}

// AdminAccountPolicyHistoryChange is one append-only administrator policy
// transition returned by the control plane.
type AdminAccountPolicyHistoryChange struct {
	Kind          string    `json:"kind"`
	ActorID       string    `json:"actor_id"`
	ActorHandle   string    `json:"actor_handle"`
	Reason        string    `json:"reason"`
	At            time.Time `json:"at"`
	PlanFrom      string    `json:"plan_from,omitempty"`
	PlanTo        string    `json:"plan_to,omitempty"`
	RetentionFrom *int64    `json:"retention_from,omitempty"`
	RetentionTo   *int64    `json:"retention_to,omitempty"`
}

// AdminTranscriptRetentionInput sets either a finite day window or an
// explicit indefinite exception. Exactly one of Days and Indefinite must be
// selected and Reason is always required.
type AdminTranscriptRetentionInput struct {
	Days       *int64
	Indefinite bool
	Reason     string
}

// MaxAdminTranscriptRetentionDays is the finite retention representation
// bound accepted by both the control-plane client and cell.
const MaxAdminTranscriptRetentionDays int64 = 36500

func validateAdminPolicyTarget(accountID string) error {
	accountID = strings.TrimSpace(accountID)
	if accountID == "" || len(accountID) > 128 {
		return fmt.Errorf("account id must be 1-128 characters")
	}
	for _, r := range accountID {
		if unicode.IsSpace(r) || unicode.IsControl(r) || r == '/' || r == '\\' {
			return fmt.Errorf("account id contains an unsafe character")
		}
	}
	return nil
}

func validateAdminPolicyReason(reason string) (string, error) {
	reason = strings.TrimSpace(reason)
	if reason == "" || len(reason) > 512 {
		return "", fmt.Errorf("reason must be 1-512 characters")
	}
	return reason, nil
}

func adminAccountPolicyURL(cpEndpoint, accountID, resource string) (string, error) {
	if err := validateAdminPolicyTarget(accountID); err != nil {
		return "", err
	}
	return fmt.Sprintf("%s/v1/admin/accounts/%s/%s",
		strings.TrimRight(cpEndpoint, "/"), neturl.PathEscape(strings.TrimSpace(accountID)), resource), nil
}

func getAdminAccountPolicy(ctx context.Context, cpEndpoint, adminToken, accountID, resource string) (*AdminAccountPolicy, error) {
	url, err := adminAccountPolicyURL(cpEndpoint, accountID, resource)
	if err != nil {
		return nil, err
	}
	var out AdminAccountPolicy
	if err := doJSON(ctx, http.MethodGet, url, adminToken, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// GetAdminTranscriptRetention returns the inherited and effective retention
// policy for one account.
func GetAdminTranscriptRetention(ctx context.Context, cpEndpoint, adminToken, accountID string) (*AdminAccountPolicy, error) {
	return getAdminAccountPolicy(ctx, cpEndpoint, adminToken, accountID, "transcript-retention")
}

// SetAdminTranscriptRetention creates or replaces an account-level retention
// exception without changing the account's billing plan.
func SetAdminTranscriptRetention(ctx context.Context, cpEndpoint, adminToken, accountID string, in AdminTranscriptRetentionInput) (*AdminAccountPolicy, error) {
	if (in.Days == nil) == !in.Indefinite {
		return nil, fmt.Errorf("set exactly one of days or indefinite")
	}
	reason, err := validateAdminPolicyReason(in.Reason)
	if err != nil {
		return nil, err
	}
	payload := map[string]any{"reason": reason}
	if in.Indefinite {
		payload["indefinite"] = true
	} else {
		if *in.Days < 1 || *in.Days > MaxAdminTranscriptRetentionDays {
			return nil, fmt.Errorf("days must be between 1 and %d", MaxAdminTranscriptRetentionDays)
		}
		payload["days"] = *in.Days
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	url, err := adminAccountPolicyURL(cpEndpoint, accountID, "transcript-retention")
	if err != nil {
		return nil, err
	}
	var out AdminAccountPolicy
	if err := doJSON(ctx, http.MethodPut, url, adminToken, body, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ClearAdminTranscriptRetention removes an account exception so the account
// inherits its effective plan's current default again.
func ClearAdminTranscriptRetention(ctx context.Context, cpEndpoint, adminToken, accountID, reason string) (*AdminAccountPolicy, error) {
	return changeAdminAccountPolicy(ctx, cpEndpoint, adminToken, accountID,
		"transcript-retention", http.MethodDelete, map[string]string{"reason": reason})
}

// GetAdminPlanOverride returns the account's effective and billing plan plus
// any administrator-owned classification override.
func GetAdminPlanOverride(ctx context.Context, cpEndpoint, adminToken, accountID string) (*AdminAccountPolicy, error) {
	return getAdminAccountPolicy(ctx, cpEndpoint, adminToken, accountID, "plan-override")
}

// SetAdminPlanOverride changes effective classification without mutating the
// provider-backed billing relationship.
func SetAdminPlanOverride(ctx context.Context, cpEndpoint, adminToken, accountID, plan, reason string) (*AdminAccountPolicy, error) {
	plan = strings.TrimSpace(plan)
	if plan == "" || len(plan) > 64 {
		return nil, fmt.Errorf("plan must be 1-64 characters")
	}
	for _, r := range plan {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '-' && r != '_' {
			return nil, fmt.Errorf("plan contains an unsafe character")
		}
	}
	return changeAdminAccountPolicy(ctx, cpEndpoint, adminToken, accountID,
		"plan-override", http.MethodPut, map[string]string{"plan": plan, "reason": reason})
}

// ClearAdminPlanOverride restores the provider-backed plan classification.
func ClearAdminPlanOverride(ctx context.Context, cpEndpoint, adminToken, accountID, reason string) (*AdminAccountPolicy, error) {
	return changeAdminAccountPolicy(ctx, cpEndpoint, adminToken, accountID,
		"plan-override", http.MethodDelete, map[string]string{"reason": reason})
}

func changeAdminAccountPolicy(ctx context.Context, cpEndpoint, adminToken, accountID, resource, method string, fields map[string]string) (*AdminAccountPolicy, error) {
	reason, err := validateAdminPolicyReason(fields["reason"])
	if err != nil {
		return nil, err
	}
	fields["reason"] = reason
	body, err := json.Marshal(fields)
	if err != nil {
		return nil, err
	}
	url, err := adminAccountPolicyURL(cpEndpoint, accountID, resource)
	if err != nil {
		return nil, err
	}
	var out AdminAccountPolicy
	if err := doJSON(ctx, method, url, adminToken, body, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ArchivedPlacementRescue reports an operator override applied to one
// archived account that could not satisfy its hard placement pins.
type ArchivedPlacementRescue struct {
	AccountID       string           `json:"account_id"`
	Changed         bool             `json:"changed"`
	ClearedAxes     []string         `json:"cleared_axes"`
	PlacementPolicy placement.Policy `json:"placement_policy"`
}

// RescueArchivedPlacement clears selected hard-pin axes on an archived
// account. It requires the fleet token because an archived account has no
// running cell against which to authenticate its owner token.
func RescueArchivedPlacement(ctx context.Context, cpEndpoint, fleetToken, accountID string, axes []string) (*ArchivedPlacementRescue, error) {
	body, err := json.Marshal(map[string]any{"axes": axes})
	if err != nil {
		return nil, err
	}
	url := fmt.Sprintf("%s/v1/placement/archives/%s:rescue",
		strings.TrimRight(cpEndpoint, "/"), accountID)
	var out ArchivedPlacementRescue
	if err := doJSON(ctx, http.MethodPost, url, fleetToken, body, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ChangeAdminTicketState transitions a ticket to newState.
func ChangeAdminTicketState(ctx context.Context, cpEndpoint, adminToken, accountID, ticketID, newState string) (*SupportTicket, error) {
	body, err := json.Marshal(map[string]string{"state": newState})
	if err != nil {
		return nil, err
	}
	url := fmt.Sprintf("%s/v1/admin/accounts/%s/tickets/%s/state",
		strings.TrimRight(cpEndpoint, "/"), accountID, ticketID)
	var out struct {
		Ticket SupportTicket `json:"ticket"`
	}
	if err := doJSON(ctx, http.MethodPatch, url, adminToken, body, &out); err != nil {
		return nil, err
	}
	return &out.Ticket, nil
}
