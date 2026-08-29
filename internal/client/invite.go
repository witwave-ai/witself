package client

import (
	"context"
	"encoding/json"
	"net/http"
	neturl "net/url"
	"strings"
)

// AdminInvite is the control-plane projection of one signup invite. Optional
// values are pointers so JSON output preserves the route's null fields.
type AdminInvite struct {
	Code        string  `json:"code"`
	Enabled     bool    `json:"enabled"`
	NotBefore   *string `json:"not_before"`
	ExpiresAt   *string `json:"expires_at"`
	MaxUses     *int64  `json:"max_uses"`
	Cell        *string `json:"cell"`
	Region      *string `json:"region"`
	Uses        int64   `json:"uses"`
	Note        string  `json:"note"`
	CreatedAt   string  `json:"created_at"`
	Valid       bool    `json:"valid"`
	Reason      string  `json:"reason,omitempty"`
	Exhausted   bool    `json:"exhausted"`
	Expired     bool    `json:"expired"`
	NotYetValid bool    `json:"not_yet_valid"`
}

// AdminInviteList is the envelope returned by GET /v1/invites.
type AdminInviteList struct {
	SchemaVersion string        `json:"schema_version"`
	Invites       []AdminInvite `json:"invites"`
}

// AdminInviteResult is the envelope returned by invite reads and upserts.
type AdminInviteResult struct {
	SchemaVersion string      `json:"schema_version"`
	Invite        AdminInvite `json:"invite"`
}

// AdminInviteInput is the optional-field-preserving POST /v1/invites body.
// Enabled is omitted for create so the control plane supplies its enabled
// default; enable and disable set only Code and Enabled.
type AdminInviteInput struct {
	Code      *string `json:"code,omitempty"`
	Enabled   *bool   `json:"enabled,omitempty"`
	NotBefore *string `json:"not_before,omitempty"`
	ExpiresAt *string `json:"expires_at,omitempty"`
	MaxUses   *int64  `json:"max_uses,omitempty"`
	Cell      *string `json:"cell,omitempty"`
	Region    *string `json:"region,omitempty"`
	Note      *string `json:"note,omitempty"`
}

// AdminInviteDeleteResult is the idempotent DELETE /v1/invites/{code}
// response. Deleted is false when the code was already absent.
type AdminInviteDeleteResult struct {
	SchemaVersion string `json:"schema_version"`
	Deleted       bool   `json:"deleted"`
}

// ListAdminInvites returns every invite from the fleet-token-authorized
// control-plane route.
func ListAdminInvites(
	ctx context.Context,
	cpEndpoint, fleetToken string,
) (*AdminInviteList, error) {
	var out AdminInviteList
	if err := doJSON(ctx, http.MethodGet, adminInvitesURL(cpEndpoint), fleetToken, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// GetAdminInvite returns one invite with the control plane's live use count
// and derived verdict fields.
func GetAdminInvite(
	ctx context.Context,
	cpEndpoint, fleetToken, code string,
) (*AdminInviteResult, error) {
	var out AdminInviteResult
	err := doJSON(ctx, http.MethodGet, adminInviteURL(cpEndpoint, code), fleetToken, nil, &out)
	if err != nil {
		return nil, redactInviteCodeError(err, code)
	}
	return &out, nil
}

// CreateAdminInvite creates or upserts an invite.
func CreateAdminInvite(
	ctx context.Context,
	cpEndpoint, fleetToken string,
	in AdminInviteInput,
) (*AdminInviteResult, error) {
	body, err := json.Marshal(in)
	if err != nil {
		return nil, err
	}
	var out AdminInviteResult
	if err := doJSON(ctx, http.MethodPost, adminInvitesURL(cpEndpoint), fleetToken, body, &out); err != nil {
		code := ""
		if in.Code != nil {
			code = *in.Code
		}
		return nil, redactInviteCodeError(err, code)
	}
	return &out, nil
}

// SetAdminInviteEnabled performs the minimal enable/disable upsert. Omitted
// invite fields remain under control-plane ownership and are not cleared.
func SetAdminInviteEnabled(
	ctx context.Context,
	cpEndpoint, fleetToken, code string,
	enabled bool,
) (*AdminInviteResult, error) {
	return CreateAdminInvite(ctx, cpEndpoint, fleetToken, AdminInviteInput{
		Code:    &code,
		Enabled: &enabled,
	})
}

// DeleteAdminInvite hard-removes the invite projection. Durable invite-use
// records are deliberately outside this route and remain audit history.
func DeleteAdminInvite(
	ctx context.Context,
	cpEndpoint, fleetToken, code string,
) (*AdminInviteDeleteResult, error) {
	var out AdminInviteDeleteResult
	err := doJSON(ctx, http.MethodDelete, adminInviteURL(cpEndpoint, code), fleetToken, nil, &out)
	if err != nil {
		return nil, redactInviteCodeError(err, code)
	}
	return &out, nil
}

func adminInvitesURL(endpoint string) string {
	return strings.TrimRight(endpoint, "/") + "/v1/invites"
}

func adminInviteURL(endpoint, code string) string {
	return adminInvitesURL(endpoint) + "/" + neturl.PathEscape(code)
}

type inviteCodeError struct {
	cause error
	code  string
}

func (e inviteCodeError) Error() string {
	message := e.cause.Error()
	for _, value := range []string{e.code, neturl.PathEscape(e.code)} {
		if value != "" {
			message = strings.ReplaceAll(message, value, "[invite]")
		}
	}
	return message
}

func (e inviteCodeError) Unwrap() error { return e.cause }

func redactInviteCodeError(err error, code string) error {
	if err == nil || code == "" {
		return err
	}
	return inviteCodeError{cause: err, code: code}
}
