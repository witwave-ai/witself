package client

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/witwave-ai/witself/internal/id"
	"github.com/witwave-ai/witself/internal/placement"
)

var accountProvisionIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

// CreatedAccount is the control plane's signup result: the new account, the
// cell it was placed on, and the one-shot bootstrap token that claims it.
type CreatedAccount struct {
	ProvisionID    string `json:"provision_id"`
	Replayed       bool   `json:"replayed"`
	AccountID      string `json:"account_id"`
	OperatorID     string `json:"operator_id"`
	Email          string `json:"email"`
	Status         string `json:"status"`
	BootstrapToken string `json:"bootstrap_token"`
	// EmailSent reports whether the control plane dispatched a verification
	// email (older control planes simply omit it).
	EmailSent bool `json:"verification_email_sent"`
	Cell      struct {
		Name     string `json:"name"`
		Endpoint string `json:"endpoint"`
	} `json:"cell"`
}

// SignupChallengeError reports that account creation requires a completed
// browser challenge. ChallengeURL is the public page where the user can
// complete it and obtain a token for the retry.
type SignupChallengeError struct {
	ChallengeURL string
	Message      string
}

func (e *SignupChallengeError) Error() string {
	if e != nil && strings.TrimSpace(e.Message) != "" {
		return e.Message
	}
	return "signup challenge required"
}

func (e *SignupChallengeError) Unwrap() error { return ErrForbidden }

type accountCreateRequest struct {
	DisplayName    string `json:"display_name"`
	Email          string `json:"email"`
	Invite         string `json:"invite"`
	ProvisionID    string `json:"provision_id"`
	TurnstileToken string `json:"turnstile_token,omitempty"`
	// Dark ToS/privacy consent capture: omitted entirely when the caller
	// records no consent so the wire request stays byte-identical to
	// consent-less clients.
	ConsentTermsVersion   string `json:"consent_terms_version,omitempty"`
	ConsentPrivacyVersion string `json:"consent_privacy_version,omitempty"`
}

// CreateAccount signs up a new account via the control plane
// (POST {controlPlane}/v1/accounts, invite-gated). Server-side refusals —
// invalid invite, duplicate email, no capacity — are surfaced verbatim. The
// generous timeout covers placement plus the cell round trip.
func CreateAccount(ctx context.Context, controlPlane, email, invite, displayName string) (*CreatedAccount, error) {
	provisionID, err := id.New("prv")
	if err != nil {
		return nil, fmt.Errorf("create provision id: %w", err)
	}
	return CreateAccountExact(
		ctx, controlPlane, email, invite, displayName, provisionID, "", "", "",
	)
}

// CreateAccountExact performs one retry-safe signup using the caller's durable
// provision id. Every transport, 5xx, malformed-success, and incomplete-success
// retry reuses the exact normalized body and provision id. Callers that need to
// survive a process restart must persist provisionID before invoking this
// function. turnstileToken is optional, omitted from the JSON body when empty,
// and deliberately excluded from AccountCreateRequestFingerprint so a caller
// can complete a challenge and resume the same durable request.
//
// consentTermsVersion and consentPrivacyVersion are the optional dark
// ToS/privacy consent capture: both-or-neither, omitted from the JSON body
// when empty, and — unlike turnstileToken — INCLUDED in
// AccountCreateRequestFingerprint when present, because recorded consent is
// part of the request the durable provision id must keep binding on resume.
func CreateAccountExact(
	ctx context.Context,
	controlPlane, email, invite, displayName, provisionID, turnstileToken string,
	consentTermsVersion, consentPrivacyVersion string,
) (*CreatedAccount, error) {
	if !accountProvisionIDPattern.MatchString(provisionID) {
		return nil, fmt.Errorf("invalid provision id")
	}
	if (consentTermsVersion == "") != (consentPrivacyVersion == "") {
		return nil, fmt.Errorf(
			"consent terms and privacy versions must be provided together",
		)
	}
	controlPlane, email, invite, displayName, err := normalizeAccountCreateRequest(
		controlPlane, email, invite, displayName,
	)
	if err != nil {
		return nil, err
	}
	body, err := json.Marshal(accountCreateRequest{
		DisplayName:           displayName,
		Email:                 email,
		Invite:                invite,
		ProvisionID:           provisionID,
		TurnstileToken:        turnstileToken,
		ConsentTermsVersion:   consentTermsVersion,
		ConsentPrivacyVersion: consentPrivacyVersion,
	})
	if err != nil {
		return nil, err
	}
	url := strings.TrimRight(controlPlane, "/") + "/v1/accounts"
	client := &http.Client{Timeout: 60 * time.Second}
	const maxAttempts = 3
	var lastErr error
	for attempt := 0; attempt < maxAttempts; attempt++ {
		req, err := http.NewRequestWithContext(
			ctx, http.MethodPost, url, bytes.NewReader(body),
		)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/json")

		resp, err := client.Do(req)
		if err != nil {
			lastErr = fmt.Errorf("connect to %s: %w", controlPlane, err)
			if ctx.Err() != nil {
				return nil, lastErr
			}
			continue
		}
		if resp.StatusCode != http.StatusCreated {
			responseErr := accountCreateResponseError(
				resp, "account creation failed: "+resp.Status,
			)
			_ = resp.Body.Close()
			if resp.StatusCode < 500 || resp.StatusCode > 599 {
				return nil, responseErr
			}
			lastErr = responseErr
			continue
		}

		var out CreatedAccount
		decodeErr := json.NewDecoder(resp.Body).Decode(&out)
		_ = resp.Body.Close()
		switch {
		case decodeErr != nil:
			lastErr = fmt.Errorf("decode response: %w", decodeErr)
		case out.ProvisionID != provisionID:
			lastErr = fmt.Errorf(
				"control plane returned a mismatched provision id",
			)
		case out.AccountID == "" || out.BootstrapToken == "" ||
			out.Cell.Endpoint == "":
			lastErr = fmt.Errorf(
				"control plane returned an incomplete signup response",
			)
		default:
			return &out, nil
		}
	}
	return nil, lastErr
}

func accountCreateResponseError(resp *http.Response, fallback string) error {
	if resp.StatusCode != http.StatusForbidden {
		return responseError(resp, fallback)
	}
	body, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	resp.Body = io.NopCloser(bytes.NewReader(body))
	if err != nil {
		return responseError(resp, fallback)
	}
	var out struct {
		Error        json.RawMessage `json:"error"`
		ChallengeURL string          `json:"challenge_url"`
	}
	_ = json.Unmarshal(body, &out)
	challengeURL := strings.TrimSpace(out.ChallengeURL)
	if challengeURL == "" {
		return responseError(resp, fallback)
	}
	message := ""
	_ = json.Unmarshal(out.Error, &message)
	return &SignupChallengeError{
		ChallengeURL: challengeURL,
		Message:      strings.TrimSpace(message),
	}
}

// AccountCreateRequestFingerprint binds a durable local provision id to the
// exact effective signup request and local destination name without persisting
// the email, invite, display name, or control-plane endpoint. The length-prefixed
// encoding avoids delimiter ambiguity. Recorded consent versions, when present,
// are appended as a domain-separated consent/v1 block so a resumed journal
// keeps binding the same consent; when absent the hashed input stays
// byte-identical to the historical consent-less algorithm (dark contract).
func AccountCreateRequestFingerprint(
	controlPlane, localName, email, invite, displayName string,
	consentTermsVersion, consentPrivacyVersion string,
) (string, error) {
	controlPlane, email, invite, displayName, err := normalizeAccountCreateRequest(
		controlPlane, email, invite, displayName,
	)
	if err != nil {
		return "", err
	}
	localName = strings.TrimSpace(localName)
	if localName == "" {
		return "", fmt.Errorf("local account name is required")
	}
	values := []string{
		"witself.account-create.v1",
		controlPlane,
		localName,
		email,
		invite,
		displayName,
	}
	if consentTermsVersion != "" || consentPrivacyVersion != "" {
		values = append(
			values, "consent/v1",
			consentTermsVersion, consentPrivacyVersion,
		)
	}
	hash := sha256.New()
	for _, value := range values {
		var size [8]byte
		binary.BigEndian.PutUint64(size[:], uint64(len(value)))
		_, _ = hash.Write(size[:])
		_, _ = hash.Write([]byte(value))
	}
	return fmt.Sprintf("%x", hash.Sum(nil)), nil
}

func normalizeAccountCreateRequest(
	controlPlane, email, invite, displayName string,
) (string, string, string, string, error) {
	controlPlane = strings.TrimRight(strings.TrimSpace(controlPlane), "/")
	email = strings.ToLower(strings.TrimSpace(email))
	invite = strings.TrimSpace(invite)
	displayName = strings.TrimSpace(displayName)
	if displayName == "" {
		displayName = email
	}
	switch {
	case controlPlane == "":
		return "", "", "", "", fmt.Errorf("control plane endpoint is required")
	case email == "":
		return "", "", "", "", fmt.Errorf("account email is required")
	case invite == "":
		return "", "", "", "", fmt.Errorf("invite code is required")
	default:
		return controlPlane, email, invite, displayName, nil
	}
}

// AccountRecord is an account's lifecycle record as served by its cell.
type AccountRecord struct {
	ID              string           `json:"id"`
	Email           string           `json:"email,omitempty"`
	DisplayName     string           `json:"display_name,omitempty"`
	Status          string           `json:"status"`
	CreatedAt       time.Time        `json:"created_at"`
	ClosedAt        *time.Time       `json:"closed_at,omitempty"`
	ClosedReason    string           `json:"closed_reason,omitempty"`
	SuspendedAt     *time.Time       `json:"suspended_at,omitempty"`
	SuspendedFor    string           `json:"suspended_for,omitempty"`
	SuspendedReason string           `json:"suspended_reason,omitempty"`
	SupportPolicy   string           `json:"support_policy,omitempty"`
	PlacementPolicy placement.Policy `json:"placement_policy,omitempty"`
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

// GetPlacementPolicy reads the authenticated account owner's placement policy.
func GetPlacementPolicy(ctx context.Context, endpoint, token string) (placement.Policy, error) {
	var out struct {
		PlacementPolicy placement.Policy `json:"placement_policy"`
	}
	url := strings.TrimRight(endpoint, "/") + "/v1/account/placement-policy"
	if err := doJSON(ctx, http.MethodGet, url, token, nil, &out); err != nil {
		return placement.Policy{}, err
	}
	return placement.Normalize(out.PlacementPolicy)
}

// SetPlacementPolicy updates the authenticated account owner's placement policy.
func SetPlacementPolicy(ctx context.Context, endpoint, token string, policy placement.Policy) (placement.Policy, error) {
	policy, err := placement.Normalize(policy)
	if err != nil {
		return placement.Policy{}, err
	}
	body, err := json.Marshal(policy)
	if err != nil {
		return placement.Policy{}, err
	}
	var out struct {
		PlacementPolicy placement.Policy `json:"placement_policy"`
	}
	url := strings.TrimRight(endpoint, "/") + "/v1/account/placement-policy"
	if err := doJSON(ctx, http.MethodPatch, url, token, body, &out); err != nil {
		return placement.Policy{}, err
	}
	return placement.Normalize(out.PlacementPolicy)
}

// ResendVerification asks the control plane to email a fresh verification
// link for a still-pending account (POST
// {controlPlane}/v1/accounts/{id}:resend-verification). The operator token
// proves ownership — the control plane forwards it to the account's cell and
// only a "still pending" answer sends. Refusals ("account is already
// active", dead token) surface verbatim. Returns the address written to.
func ResendVerification(ctx context.Context, controlPlane, accountID, operatorToken string) (string, error) {
	url := strings.TrimRight(controlPlane, "/") + "/v1/accounts/" + accountID + ":resend-verification"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+operatorToken)

	resp, err := (&http.Client{Timeout: 60 * time.Second}).Do(req)
	if err != nil {
		return "", fmt.Errorf("connect to %s: %w", controlPlane, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return "", responseError(resp, "resend failed: "+resp.Status)
	}
	var out struct {
		Email string `json:"email"`
		Sent  bool   `json:"verification_email_sent"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", fmt.Errorf("decode response: %w", err)
	}
	if !out.Sent {
		return "", fmt.Errorf("control plane did not send the email")
	}
	return out.Email, nil
}

// RequestRecovery asks the control plane to email a recovery code for an
// account whose credentials are lost (POST {controlPlane}/v1/accounts/{id}:recover
// with an empty body). Unauthenticated by design; the answer is deliberately
// the same whether the account exists or not.
func RequestRecovery(ctx context.Context, controlPlane, accountID string) error {
	url := strings.TrimRight(controlPlane, "/") + "/v1/accounts/" + accountID + ":recover"
	return doJSON(ctx, http.MethodPost, url, "", []byte("{}"), nil)
}

// RedeemRecovery exchanges an emailed recovery code for a fresh root-bound
// bootstrap token (same shape as signup — the ordinary claim exchange
// finishes the job). Refusals (bad code, too many attempts) surface verbatim.
func RedeemRecovery(ctx context.Context, controlPlane, accountID, code string) (*CreatedAccount, error) {
	body, err := json.Marshal(map[string]string{"code": code})
	if err != nil {
		return nil, err
	}
	var out CreatedAccount
	url := strings.TrimRight(controlPlane, "/") + "/v1/accounts/" + accountID + ":recover"
	if err := doJSON(ctx, http.MethodPost, url, "", body, &out); err != nil {
		return nil, err
	}
	if out.AccountID == "" || out.BootstrapToken == "" || out.Cell.Endpoint == "" {
		return nil, fmt.Errorf("control plane returned an incomplete recovery response")
	}
	return &out, nil
}

// RequestEmailChange asks the control plane to email a confirmation code to
// the NEW address (proving it can receive) and a notice to the old one.
// Operator-token authenticated — this is a routine change by a logged-in
// owner, not a recovery.
func RequestEmailChange(ctx context.Context, controlPlane, accountID, operatorToken, newEmail string) error {
	body, err := json.Marshal(map[string]string{"new_email": newEmail})
	if err != nil {
		return err
	}
	url := strings.TrimRight(controlPlane, "/") + "/v1/accounts/" + accountID + ":change-email"
	return doJSON(ctx, http.MethodPost, url, operatorToken, body, nil)
}

// RedeemEmailChange commits the change with the emailed code and returns the
// committed address.
func RedeemEmailChange(ctx context.Context, controlPlane, accountID, operatorToken, newEmail, code string) (string, error) {
	body, err := json.Marshal(map[string]string{"new_email": newEmail, "code": code})
	if err != nil {
		return "", err
	}
	var out struct {
		Email string `json:"email"`
	}
	url := strings.TrimRight(controlPlane, "/") + "/v1/accounts/" + accountID + ":change-email"
	if err := doJSON(ctx, http.MethodPost, url, operatorToken, body, &out); err != nil {
		return "", err
	}
	if out.Email == "" {
		return "", fmt.Errorf("control plane returned no email")
	}
	return out.Email, nil
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
