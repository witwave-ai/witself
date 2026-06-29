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
	defer resp.Body.Close()

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
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusUnauthorized:
		return fmt.Errorf("not authorized (check the token)")
	case resp.StatusCode == http.StatusConflict:
		return fmt.Errorf("already exists")
	case resp.StatusCode == http.StatusNotFound:
		return fmt.Errorf("not found")
	case resp.StatusCode >= 300:
		return fmt.Errorf("request failed: %s", resp.Status)
	}
	if out != nil {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			return fmt.Errorf("decode response: %w", err)
		}
	}
	return nil
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
func CreateAgentToken(ctx context.Context, endpoint, token, agentID string) (string, error) {
	var out struct {
		AgentToken string `json:"agent_token"`
	}
	url := strings.TrimRight(endpoint, "/") + "/v1/agents/" + agentID + "/tokens"
	if err := doJSON(ctx, http.MethodPost, url, token, []byte("{}"), &out); err != nil {
		return "", err
	}
	if out.AgentToken == "" {
		return "", fmt.Errorf("server returned no token")
	}
	return out.AgentToken, nil
}
