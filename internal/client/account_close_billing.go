package client

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// ErrAccountBillingClose means durable cancellation could not be confirmed.
// It intentionally excludes provider responses, endpoint URLs, and credentials.
var ErrAccountBillingClose = errors.New("account billing closure could not be confirmed")

// CloseAccountBilling is a cell-only bridge. Both the owner bearer and the
// routed cell's provision token are required; neither may be redirected.
func CloseAccountBilling(ctx context.Context, endpoint, accountID, ownerBearer, provisionToken string) error {
	if !validBillingAPIEndpoint(endpoint) || !accountProvisionIDPattern.MatchString(accountID) ||
		!validAccountCloseCredential(ownerBearer) || !validAccountCloseCredential(provisionToken) {
		return ErrAccountBillingClose
	}
	decoded, err := url.PathUnescape(endpoint)
	if err != nil || strings.ContainsRune(decoded, '\\') || billingURLHasUnsafeRune(decoded) {
		return ErrAccountBillingClose
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, billingURL(endpoint, accountID, ":close"), strings.NewReader("{}"))
	if err != nil {
		return ErrAccountBillingClose
	}
	req.Header.Set("Authorization", "Bearer "+ownerBearer)
	req.Header.Set("X-Witself-Cell-Provision-Token", provisionToken)
	req.Header.Set("Content-Type", "application/json")
	var result struct {
		SchemaVersion string `json:"schema_version"`
		AccountID     string `json:"account_id"`
		Status        string `json:"status"`
	}
	if err := accountCloseRequest(req, billingMutationTransportTimeout, &result); err != nil ||
		result.SchemaVersion != "witself.v0" || result.AccountID != accountID || result.Status != "billing_closed" {
		return ErrAccountBillingClose
	}
	return nil
}

// AccountCloseEligibility is a cell-authenticated owner principal that passed
// the default-account and vault lifecycle fences, including while suspended
// or pending. It is not a lease; the cell rechecks before final closure.
type AccountCloseEligibility struct {
	SchemaVersion string `json:"schema_version"`
	Kind          string `json:"kind"`
	AccountID     string `json:"account_id"`
	OperatorID    string `json:"operator_id"`
	AccountRole   string `json:"account_role"`
}

// GetAccountCloseEligibility authenticates through the cell's dedicated
// status-independent close preflight. Ordinary whoami is active-only.
func GetAccountCloseEligibility(ctx context.Context, endpoint, bearer string) (AccountCloseEligibility, error) {
	var result AccountCloseEligibility
	// Cell registry endpoints may use private HTTP networking. They are not
	// customer-supplied billing destinations, but credentials still cannot
	// follow redirects or be sent in URL userinfo/query/fragment fields.
	parsed, err := url.Parse(endpoint)
	if err != nil || endpoint != strings.TrimSpace(endpoint) || billingURLHasUnsafeRune(endpoint) ||
		parsed.Hostname() == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" ||
		(parsed.Scheme != "https" && parsed.Scheme != "http") || !validAccountCloseCredential(bearer) {
		return result, ErrAccountBillingClose
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiV1Base(endpoint)+"/account/close-eligibility", nil)
	if err != nil {
		return result, ErrAccountBillingClose
	}
	req.Header.Set("Authorization", "Bearer "+bearer)
	if err := accountCloseRequest(req, 30*time.Second, &result); err != nil {
		return AccountCloseEligibility{}, err
	}
	if result.SchemaVersion != "witself.v0" || result.Kind != "operator" || result.AccountRole != "account_owner" ||
		!accountProvisionIDPattern.MatchString(result.AccountID) || !accountProvisionIDPattern.MatchString(result.OperatorID) {
		return AccountCloseEligibility{}, ErrAccountBillingClose
	}
	return result, nil
}

func validAccountCloseCredential(value string) bool {
	return value != "" && value == strings.TrimSpace(value) && !billingURLHasUnsafeRune(value)
}

func accountCloseRequest(req *http.Request, timeout time.Duration, result any) error {
	hc := &http.Client{Timeout: timeout, CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	resp, err := hc.Do(req)
	if err != nil {
		return ErrAccountBillingClose
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusUnauthorized {
		return ErrUnauthorized
	}
	if resp.StatusCode == http.StatusForbidden {
		return ErrForbidden
	}
	if resp.StatusCode != http.StatusOK {
		return ErrAccountBillingClose
	}
	const maxResponse = 8 << 10
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponse+1))
	if err != nil || len(body) > maxResponse || json.Unmarshal(body, result) != nil {
		return ErrAccountBillingClose
	}
	return nil
}
