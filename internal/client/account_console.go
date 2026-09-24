package client

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// AccountConsoleIdentity is the exact operator binding, never an agent grant.
type AccountConsoleIdentity struct {
	AccountID  string `json:"account_id"`
	OperatorID string `json:"operator_id"`
	Role       string `json:"role"`
}

// AccountConsoleManager is private process configuration. Endpoint must be the
// already trusted cell origin, selected by the caller without ambient fallback.
// Identity must come from VerifyAccountConsoleManager. Reads bind both IDs and
// recheck the current role; a role change never binds a different operator.
type AccountConsoleManager struct {
	Endpoint    string                 `json:"-"`
	BearerToken string                 `json:"-"`
	Identity    AccountConsoleIdentity `json:"-"`
}

// String and GoString prevent accidental credential-bearing diagnostic output.
func (AccountConsoleManager) String() string   { return "account console manager (private)" }
func (AccountConsoleManager) GoString() string { return "account console manager (private)" }

var ErrAccountConsoleUnavailable = errors.New("account console unavailable")
var ErrAccountConsoleResponseTooLarge = errors.New("account console response too large")
var ErrAccountConsoleForbidden = errors.New("account console forbidden")

// AccountConsoleRole recognizes only roles supported by today's cell/CP source.
func AccountConsoleRole(role string) bool {
	switch role {
	case "account_owner", "account_admin", "account_billing", "account_operator":
		return true
	}
	return false
}

func AccountConsoleBillingRole(role string) bool {
	return role == "account_owner" || role == "account_admin" || role == "account_billing"
}

// AccountConsoleOrigin validates and normalizes a fixed HTTPS cell origin (HTTP
// only on loopback for local cells/tests). It never resolves account selectors.
func AccountConsoleOrigin(endpoint string) (string, error) {
	u, err := url.Parse(endpoint)
	if err != nil || !validBillingAPIEndpoint(endpoint) || u == nil ||
		(u.Path != "" && u.Path != "/") || u.RawPath != "" || u.ForceQuery || strings.Contains(endpoint, "#") {
		return "", ErrAccountConsoleUnavailable
	}
	if port := u.Port(); port != "" || strings.HasSuffix(u.Host, ":") {
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 {
			return "", ErrAccountConsoleUnavailable
		}
	}
	u.Scheme = strings.ToLower(u.Scheme)
	u.Host = strings.ToLower(u.Host)
	if (u.Scheme == "https" && u.Port() == "443") || (u.Scheme == "http" && u.Port() == "80") {
		u.Host = u.Hostname()
		if strings.Contains(u.Host, ":") {
			u.Host = "[" + u.Host + "]"
		}
	}
	u.Path = ""
	return u.String(), nil
}

// VerifyAccountConsoleManager is the strict, bounded CLI/bootstrap verifier.
// Missing or legacy schema/kind/role is forbidden. Errors contain no upstream
// strings, URLs or credentials. The request has a ten-second maximum deadline.
func VerifyAccountConsoleManager(ctx context.Context, endpoint, bearer, accountID string) (AccountConsoleIdentity, error) {
	origin, err := AccountConsoleOrigin(endpoint)
	if err != nil || !accountProvisionIDPattern.MatchString(accountID) || bearer == "" || ctx == nil {
		return AccountConsoleIdentity{}, ErrAccountConsoleForbidden
	}
	var out struct {
		Schema    string `json:"schema_version"`
		Principal struct {
			Kind       string `json:"kind"`
			AccountID  string `json:"account_id"`
			OperatorID string `json:"operator_id"`
			Role       string `json:"account_role"`
		} `json:"principal"`
	}
	if err := accountConsoleJSON(ctx, origin+"/v1/whoami", bearer, 2<<20, &out); err != nil {
		return AccountConsoleIdentity{}, err
	}
	p := out.Principal
	if out.Schema != "witself.v0" || p.Kind != "operator" || p.AccountID != accountID ||
		!accountProvisionIDPattern.MatchString(p.OperatorID) || !AccountConsoleRole(p.Role) {
		return AccountConsoleIdentity{}, ErrAccountConsoleForbidden
	}
	return AccountConsoleIdentity{AccountID: p.AccountID, OperatorID: p.OperatorID, Role: p.Role}, nil
}

// RevalidateAccountConsoleManager preserves the original principal and permits
// only recognized current roles. A changed role is returned to the caller.
func RevalidateAccountConsoleManager(ctx context.Context, m AccountConsoleManager) (AccountConsoleIdentity, error) {
	if !AccountConsoleRole(m.Identity.Role) || !accountProvisionIDPattern.MatchString(m.Identity.OperatorID) {
		return AccountConsoleIdentity{}, ErrAccountConsoleForbidden
	}
	p, err := VerifyAccountConsoleManager(ctx, m.Endpoint, m.BearerToken, m.Identity.AccountID)
	if err != nil {
		return AccountConsoleIdentity{}, err
	}
	if p.OperatorID != m.Identity.OperatorID {
		return AccountConsoleIdentity{}, ErrAccountConsoleForbidden
	}
	return p, nil
}

// AccountConsoleResource is a closed set of upstream GETs. No URL, method,
// query or credential can be supplied through a console read request.
type AccountConsoleResource uint8

const (
	AccountConsoleOverview AccountConsoleResource = iota + 1
	AccountConsolePlan
	AccountConsoleBillingSummary
	AccountConsoleInvoices
	AccountConsolePayments
	AccountConsoleSupport
	AccountConsoleSupportTicket
)

// ReadAccountConsole returns bounded upstream JSON for allowlisted projection
// by dashboard. It revalidates the bound operator before every read. Billing
// routing uses the same capability trust rules and CP paths as billing.go,
// with a separate transport so existing clients retain their behavior.
func ReadAccountConsole(ctx context.Context, m AccountConsoleManager, resource AccountConsoleResource, id string) (json.RawMessage, error) {
	if ctx == nil {
		return nil, ErrAccountConsoleUnavailable
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	var path string
	billing := false
	maxBytes := int64(2 << 20)
	switch resource {
	case AccountConsoleOverview:
		path = "/account"
	case AccountConsoleSupport:
		path = "/support/tickets"
	case AccountConsoleSupportTicket:
		if !accountProvisionIDPattern.MatchString(id) {
			return nil, ErrAccountConsoleForbidden
		}
		path = "/support/tickets/" + id
		maxBytes = 4 << 20
	case AccountConsolePlan:
		path = "/accounts/" + m.Identity.AccountID + "/plan"
		billing = true
	case AccountConsoleBillingSummary:
		path = "/accounts/" + m.Identity.AccountID + "/billing"
		billing = true
	case AccountConsoleInvoices:
		path = "/accounts/" + m.Identity.AccountID + "/billing/invoices"
		billing = true
	case AccountConsolePayments:
		path = "/accounts/" + m.Identity.AccountID + "/billing/payments"
		billing = true
	default:
		return nil, ErrAccountConsoleForbidden
	}
	if resource != AccountConsoleSupportTicket && id != "" {
		return nil, ErrAccountConsoleForbidden
	}
	p, err := RevalidateAccountConsoleManager(ctx, m)
	if err != nil {
		return nil, err
	}
	endpoint, err := AccountConsoleOrigin(m.Endpoint)
	if err != nil {
		return nil, err
	}
	if billing {
		if !AccountConsoleBillingRole(p.Role) {
			return nil, ErrAccountConsoleForbidden
		}
		endpoint, err = accountConsoleBillingEndpoint(ctx, endpoint, p.AccountID)
		if err != nil {
			return nil, err
		}
	}
	var raw json.RawMessage
	if err := accountConsoleJSON(ctx, apiV1Base(endpoint)+path, m.BearerToken, maxBytes, &raw); err != nil {
		return nil, err
	}
	var envelope struct {
		Schema    string `json:"schema_version"`
		AccountID string `json:"account_id"`
	}
	if json.Unmarshal(raw, &envelope) != nil || envelope.Schema != "witself.v0" {
		return nil, ErrAccountConsoleUnavailable
	}
	if billing && validateBillingEnvelope(envelope.Schema, envelope.AccountID, p.AccountID) != nil {
		return nil, ErrAccountConsoleUnavailable
	}
	return raw, nil
}

func accountConsoleBillingEndpoint(ctx context.Context, endpoint, accountID string) (string, error) {
	var out struct {
		Schema  string `json:"schema_version"`
		Backend struct {
			Kind string `json:"kind"`
		} `json:"backend"`
		Account *struct {
			ID string `json:"id"`
		} `json:"account"`
		Billing struct {
			Supported bool   `json:"supported"`
			Endpoint  string `json:"endpoint"`
		} `json:"billing"`
	}
	if err := accountConsoleJSON(ctx, apiV1Base(endpoint)+"/capabilities", "", 2<<20, &out); err != nil {
		if errors.Is(err, ErrAccountConsoleResponseTooLarge) {
			return "", err
		}
		return "", ErrAccountConsoleUnavailable
	}
	if out.Schema != "witself.v0" ||
		(!strings.EqualFold(strings.TrimSpace(out.Backend.Kind), "managed") && (out.Account == nil || out.Account.ID != accountID)) ||
		!out.Billing.Supported || !validBillingAPIEndpoint(out.Billing.Endpoint) {
		return "", ErrAccountConsoleUnavailable
	}
	return strings.TrimRight(out.Billing.Endpoint, "/"), nil
}

// Dedicated transport: no environment proxy and no redirect credential replay.
// Response bytes are capped before JSON decoding, including chunked bodies.
var accountConsoleHTTP = &http.Client{
	Timeout:       10 * time.Second,
	Transport:     &http.Transport{Proxy: nil, MaxIdleConns: 20, MaxIdleConnsPerHost: 4, IdleConnTimeout: 30 * time.Second, TLSHandshakeTimeout: 10 * time.Second},
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
}

func accountConsoleJSON(ctx context.Context, target, bearer string, maxBytes int64, out any) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return ErrAccountConsoleUnavailable
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := accountConsoleHTTP.Do(req)
	if err != nil {
		return ErrAccountConsoleUnavailable
	}
	defer resp.Body.Close()
	if resp.StatusCode == 401 || resp.StatusCode == 403 {
		return ErrAccountConsoleForbidden
	}
	if resp.ContentLength > maxBytes {
		return ErrAccountConsoleResponseTooLarge
	}
	if resp.StatusCode != http.StatusOK {
		return ErrAccountConsoleUnavailable
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxBytes+1))
	if int64(len(data)) > maxBytes {
		return ErrAccountConsoleResponseTooLarge
	}
	if err != nil || json.Unmarshal(data, out) != nil {
		return ErrAccountConsoleUnavailable
	}
	return nil
}
