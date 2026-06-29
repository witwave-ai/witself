// Package client is the witself-server HTTP client used by the ws CLI. It is a
// thin wrapper over the /v1 API; this first slice covers the bootstrap login.
package client

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
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
