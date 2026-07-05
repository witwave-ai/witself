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
)

// Admin is the public shape of a fleet-admin credential (as returned
// by the control-plane admin registry). The raw token is only present
// on MintAdminResult — it's shown once, never persisted here.
type Admin struct {
	AdminID     string     `json:"admin_id"`
	Handle      string     `json:"handle"`
	Note        string     `json:"note,omitempty"`
	CreatedAt   time.Time  `json:"created_at"`
	CreatedBy   string     `json:"created_by,omitempty"`
	Disabled    bool       `json:"disabled"`
	DisabledAt  *time.Time `json:"disabled_at,omitempty"`
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
