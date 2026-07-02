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

// CreatedAccount is the control plane's signup result: the new account, the
// cell it was placed on, and the one-shot bootstrap token that claims it.
type CreatedAccount struct {
	AccountID      string `json:"account_id"`
	OperatorID     string `json:"operator_id"`
	Email          string `json:"email"`
	Status         string `json:"status"`
	BootstrapToken string `json:"bootstrap_token"`
	Cell           struct {
		Name     string `json:"name"`
		Endpoint string `json:"endpoint"`
	} `json:"cell"`
}

// CreateAccount signs up a new account via the control plane
// (POST {controlPlane}/v1/accounts, invite-gated). Server-side refusals —
// invalid invite, duplicate email, no capacity — are surfaced verbatim. The
// generous timeout covers placement plus the cell round trip.
func CreateAccount(ctx context.Context, controlPlane, email, invite, displayName string) (*CreatedAccount, error) {
	body, err := json.Marshal(map[string]string{
		"email":        email,
		"invite":       invite,
		"display_name": displayName,
	})
	if err != nil {
		return nil, err
	}
	url := strings.TrimRight(controlPlane, "/") + "/v1/accounts"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := (&http.Client{Timeout: 60 * time.Second}).Do(req)
	if err != nil {
		return nil, fmt.Errorf("connect to %s: %w", controlPlane, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusCreated {
		return nil, responseError(resp, "account creation failed: "+resp.Status)
	}
	var out CreatedAccount
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}
	if out.AccountID == "" || out.BootstrapToken == "" || out.Cell.Endpoint == "" {
		return nil, fmt.Errorf("control plane returned an incomplete signup response")
	}
	return &out, nil
}

// AccountRecord is an account's lifecycle record as served by its cell.
type AccountRecord struct {
	ID           string     `json:"id"`
	Email        string     `json:"email,omitempty"`
	DisplayName  string     `json:"display_name,omitempty"`
	Status       string     `json:"status"`
	CreatedAt    time.Time  `json:"created_at"`
	ClosedAt     *time.Time `json:"closed_at,omitempty"`
	ClosedReason string     `json:"closed_reason,omitempty"`
}

// GetAccount reads the authenticated operator's account record from its cell
// (GET {endpoint}/v1/account). Works at any status — checking whether a
// pending account has been activated is its main job.
func GetAccount(ctx context.Context, endpoint, token string) (*AccountRecord, error) {
	var out struct {
		Account AccountRecord `json:"account"`
	}
	url := strings.TrimRight(endpoint, "/") + "/v1/account"
	if err := doJSON(ctx, http.MethodGet, url, token, nil, &out); err != nil {
		return nil, err
	}
	if out.Account.ID == "" {
		return nil, fmt.Errorf("server returned no account")
	}
	return &out.Account, nil
}

// CloseAccount permanently closes an account via the control plane
// (POST {controlPlane}/v1/accounts/{id}:close). The operator token is forwarded
// to the account's cell, which authorizes (owner-only) and tombstones; the
// control plane then removes its routing pointer. Refusals surface verbatim.
func CloseAccount(ctx context.Context, controlPlane, accountID, operatorToken, reason string) error {
	body, err := json.Marshal(map[string]string{"reason": reason})
	if err != nil {
		return err
	}
	url := strings.TrimRight(controlPlane, "/") + "/v1/accounts/" + accountID + ":close"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+operatorToken)

	resp, err := (&http.Client{Timeout: 60 * time.Second}).Do(req)
	if err != nil {
		return fmt.Errorf("connect to %s: %w", controlPlane, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return responseError(resp, "close failed: "+resp.Status)
	}
	return nil
}
