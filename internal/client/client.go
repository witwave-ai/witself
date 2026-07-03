// Package client is the witself-server HTTP client used by the ws CLI. It is a
// thin wrapper over the /v1 API; this first slice covers the bootstrap login.
package client

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// BootstrapResult is the outcome of a bootstrap login.
type BootstrapResult struct {
	OperatorToken string
	OperatorID    string
}

// OperatorTokenResult is returned when an authenticated operator mints another
// operator token. The raw token is shown once.
type OperatorTokenResult struct {
	OperatorToken string
	OperatorID    string
	TokenID       string
	DisplayName   string
	ExpiresAt     string
}

// OperatorToken is safe token metadata returned in operator listings.
type OperatorToken struct {
	ID          string     `json:"id"`
	DisplayName string     `json:"display_name"`
	CreatedAt   time.Time  `json:"created_at"`
	ExpiresAt   *time.Time `json:"expires_at,omitempty"`
}

// Operator is the API view of an operator principal.
type Operator struct {
	ID          string          `json:"id"`
	DisplayName string          `json:"display_name"`
	Role        string          `json:"role"`
	IsRoot      bool            `json:"is_root"`
	CreatedAt   time.Time       `json:"created_at"`
	UpdatedAt   time.Time       `json:"updated_at"`
	Tokens      []OperatorToken `json:"tokens"`
}

// CreateOperatorResult is returned when creating a named operator and its first
// token. The raw token is shown once.
type CreateOperatorResult struct {
	Operator       Operator
	OperatorToken  string
	TokenExpiresAt *time.Time
}

// BootstrapLogin exchanges a bootstrap token for an operator token by POSTing to
// {endpoint}/v1/auth/bootstrap.
func BootstrapLogin(ctx context.Context, endpoint, bootstrapToken string) (*BootstrapResult, error) {
	body, err := json.Marshal(map[string]string{"bootstrap_token": bootstrapToken})
	if err != nil {
		return nil, err
	}
	url := strings.TrimRight(endpoint, "/") + "/v1/auth/bootstrap"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	cl := &http.Client{Timeout: 15 * time.Second}
	resp, err := cl.Do(req)
	if err != nil {
		return nil, fmt.Errorf("connect to %s: %w", endpoint, err)
	}
	defer func() { _ = resp.Body.Close() }()

	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusUnauthorized:
		return nil, fmt.Errorf("invalid or already-used bootstrap token")
	default:
		return nil, fmt.Errorf("login failed: %s", resp.Status)
	}

	var out struct {
		OperatorToken string `json:"operator_token"`
		OperatorID    string `json:"operator_id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}
	if out.OperatorToken == "" {
		return nil, fmt.Errorf("server returned no operator token")
	}
	return &BootstrapResult{OperatorToken: out.OperatorToken, OperatorID: out.OperatorID}, nil
}

// Whoami resolves an operator token to its principal ids via GET {endpoint}/v1/whoami.
func Whoami(ctx context.Context, endpoint, token string) (operatorID, accountID string, err error) {
	var out struct {
		Principal struct {
			OperatorID string `json:"operator_id"`
			AccountID  string `json:"account_id"`
		} `json:"principal"`
	}
	url := strings.TrimRight(endpoint, "/") + "/v1/whoami"
	if err := doJSON(ctx, http.MethodGet, url, token, nil, &out); err != nil {
		return "", "", err
	}
	if out.Principal.OperatorID == "" {
		return "", "", fmt.Errorf("server returned no principal")
	}
	return out.Principal.OperatorID, out.Principal.AccountID, nil
}

// doJSON performs an authenticated JSON request and decodes the response into
// out (if non-nil), mapping common statuses to friendly errors.
func doJSON(ctx context.Context, method, url, token string, body []byte, out any) error {
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, rdr)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	switch {
	case resp.StatusCode == http.StatusUnauthorized:
		return responseError(resp, "not authorized (check the token)")
	case resp.StatusCode == http.StatusConflict:
		return responseError(resp, "conflict")
	case resp.StatusCode == http.StatusNotFound:
		return responseError(resp, "not found")
	case resp.StatusCode >= 300:
		return responseError(resp, "request failed: "+resp.Status)
	}
	if out != nil {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			return fmt.Errorf("decode response: %w", err)
		}
	}
	return nil
}

func responseError(resp *http.Response, fallback string) error {
	var out struct {
		Error string `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err == nil && out.Error != "" {
		return fmt.Errorf("%s", out.Error)
	}
	return fmt.Errorf("%s", fallback)
}

// Realm is the API view of a realm.
type Realm struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// CreateRealm creates a realm via POST {endpoint}/v1/realms.
func CreateRealm(ctx context.Context, endpoint, token, name string) (*Realm, error) {
	body, err := json.Marshal(map[string]string{"name": name})
	if err != nil {
		return nil, err
	}
	var out struct {
		Realm Realm `json:"realm"`
	}
	if err := doJSON(ctx, http.MethodPost, realmsURL(endpoint), token, body, &out); err != nil {
		return nil, err
	}
	return &out.Realm, nil
}

// ListRealms lists realms via GET {endpoint}/v1/realms.
func ListRealms(ctx context.Context, endpoint, token string) ([]Realm, error) {
	var out struct {
		Realms []Realm `json:"realms"`
	}
	if err := doJSON(ctx, http.MethodGet, realmsURL(endpoint), token, nil, &out); err != nil {
		return nil, err
	}
	return out.Realms, nil
}

// DeleteRealm soft-deletes an empty realm via DELETE {endpoint}/v1/realms/{realm}.
func DeleteRealm(ctx context.Context, endpoint, token, realmID string) error {
	return doJSON(ctx, http.MethodDelete, realmsURL(endpoint)+"/"+realmID, token, nil, nil)
}

func realmsURL(endpoint string) string {
	return strings.TrimRight(endpoint, "/") + "/v1/realms"
}

// Agent is the API view of an agent.
type Agent struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// CreateAgent creates an agent in a realm via POST {endpoint}/v1/realms/{realm}/agents.
func CreateAgent(ctx context.Context, endpoint, token, realmID, name string) (*Agent, error) {
	body, err := json.Marshal(map[string]string{"name": name})
	if err != nil {
		return nil, err
	}
	var out struct {
		Agent Agent `json:"agent"`
	}
	if err := doJSON(ctx, http.MethodPost, agentsURL(endpoint, realmID), token, body, &out); err != nil {
		return nil, err
	}
	return &out.Agent, nil
}

// ListAgents lists a realm's agents via GET {endpoint}/v1/realms/{realm}/agents.
func ListAgents(ctx context.Context, endpoint, token, realmID string) ([]Agent, error) {
	var out struct {
		Agents []Agent `json:"agents"`
	}
	if err := doJSON(ctx, http.MethodGet, agentsURL(endpoint, realmID), token, nil, &out); err != nil {
		return nil, err
	}
	return out.Agents, nil
}

func agentsURL(endpoint, realmID string) string {
	return strings.TrimRight(endpoint, "/") + "/v1/realms/" + realmID + "/agents"
}

// CreateAgentToken mints an agent token via POST {endpoint}/v1/agents/{agent}/tokens.
// agentName is empty when the cell predates the field.
func CreateAgentToken(ctx context.Context, endpoint, token, agentID string) (agentToken, tokenID, agentName string, err error) {
	var out struct {
		AgentToken string `json:"agent_token"`
		TokenID    string `json:"token_id"`
		AgentName  string `json:"agent_name"`
	}
	url := strings.TrimRight(endpoint, "/") + "/v1/agents/" + agentID + "/tokens"
	if err := doJSON(ctx, http.MethodPost, url, token, []byte("{}"), &out); err != nil {
		return "", "", "", err
	}
	if out.AgentToken == "" {
		return "", "", "", fmt.Errorf("server returned no token")
	}
	return out.AgentToken, out.TokenID, out.AgentName, nil
}

// RenameAccount changes the account's server-side display name (owner-only)
// via POST {endpoint}/v1/account:rename.
func RenameAccount(ctx context.Context, endpoint, token, displayName string) error {
	body, err := json.Marshal(map[string]string{"display_name": displayName})
	if err != nil {
		return err
	}
	url := strings.TrimRight(endpoint, "/") + "/v1/account:rename"
	return doJSON(ctx, http.MethodPost, url, token, body, nil)
}

// DeleteAgent soft-deletes an agent and revokes its tokens.
func DeleteAgent(ctx context.Context, endpoint, token, realmID, agentID string) error {
	return doJSON(ctx, http.MethodDelete, agentsURL(endpoint, realmID)+"/"+agentID, token, nil, nil)
}

// CreateOperatorToken mints another token for the authenticated operator.
func CreateOperatorToken(ctx context.Context, endpoint, token, displayName, ttl string) (*OperatorTokenResult, error) {
	body := map[string]string{}
	if displayName != "" {
		body["display_name"] = displayName
	}
	if ttl != "" {
		body["ttl"] = ttl
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	var out struct {
		OperatorToken string `json:"operator_token"`
		OperatorID    string `json:"operator_id"`
		TokenID       string `json:"token_id"`
		DisplayName   string `json:"display_name,omitempty"`
		ExpiresAt     string `json:"expires_at,omitempty"`
	}
	url := strings.TrimRight(endpoint, "/") + "/v1/operators/self/tokens"
	if err := doJSON(ctx, http.MethodPost, url, token, raw, &out); err != nil {
		return nil, err
	}
	if out.OperatorToken == "" {
		return nil, fmt.Errorf("server returned no token")
	}
	return &OperatorTokenResult{
		OperatorToken: out.OperatorToken,
		OperatorID:    out.OperatorID,
		TokenID:       out.TokenID,
		DisplayName:   out.DisplayName,
		ExpiresAt:     out.ExpiresAt,
	}, nil
}

// ListOperators lists current operators for the authenticated account.
func ListOperators(ctx context.Context, endpoint, token string) ([]Operator, error) {
	var out struct {
		Operators []Operator `json:"operators"`
	}
	if err := doJSON(ctx, http.MethodGet, operatorsURL(endpoint), token, nil, &out); err != nil {
		return nil, err
	}
	return out.Operators, nil
}

// CreateOperator creates a named operator and returns its first token once.
func CreateOperator(ctx context.Context, endpoint, token, displayName, tokenDisplayName, ttl string) (*CreateOperatorResult, error) {
	body := map[string]string{"display_name": displayName}
	if tokenDisplayName != "" {
		body["token_display_name"] = tokenDisplayName
	}
	if ttl != "" {
		body["ttl"] = ttl
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	var out struct {
		Operator       Operator   `json:"operator"`
		OperatorToken  string     `json:"operator_token"`
		TokenExpiresAt *time.Time `json:"token_expires_at,omitempty"`
	}
	if err := doJSON(ctx, http.MethodPost, operatorsURL(endpoint), token, raw, &out); err != nil {
		return nil, err
	}
	if out.OperatorToken == "" {
		return nil, fmt.Errorf("server returned no operator token")
	}
	return &CreateOperatorResult{
		Operator:       out.Operator,
		OperatorToken:  out.OperatorToken,
		TokenExpiresAt: out.TokenExpiresAt,
	}, nil
}

// DeleteOperator soft-deletes another operator and revokes its tokens.
func DeleteOperator(ctx context.Context, endpoint, token, operatorID string) error {
	return doJSON(ctx, http.MethodDelete, operatorsURL(endpoint)+"/"+operatorID, token, nil, nil)
}

// RevokeToken immediately revokes a live operator or agent token by token id.
func RevokeToken(ctx context.Context, endpoint, token, tokenID string) error {
	url := strings.TrimRight(endpoint, "/") + "/v1/tokens/" + tokenID + ":revoke"
	return doJSON(ctx, http.MethodPost, url, token, []byte("{}"), nil)
}

func operatorsURL(endpoint string) string {
	return strings.TrimRight(endpoint, "/") + "/v1/operators"
}
