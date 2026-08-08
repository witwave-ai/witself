package client

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	neturl "net/url"
	"strings"
	"time"
)

// AgentEmailDomainSchemaVersion identifies the control-plane custom inbound
// domain request contract.
const AgentEmailDomainSchemaVersion = "witself.agent-email-domain.v1"

// AgentEmailDomainOwnershipChallenge is the public DNS challenge issued for
// one custom inbound-domain request. It is not evidence that ownership has
// been verified, and it grants no routing authority.
type AgentEmailDomainOwnershipChallenge struct {
	RecordName  string     `json:"record_name"`
	RecordType  string     `json:"record_type"`
	RecordValue string     `json:"record_value"`
	IssuedAt    *time.Time `json:"issued_at"`
	ExpiresAt   *time.Time `json:"expires_at,omitempty"`
}

// AgentEmailDomainOwnershipVerification records bounded DNS observations. A
// verified request still remains non-deliverable until the independent route
// projection and delivery gates are introduced.
type AgentEmailDomainOwnershipVerification struct {
	State               string     `json:"state"`
	LastResult          string     `json:"last_result"`
	FirstVerifiedAt     *time.Time `json:"first_verified_at,omitempty"`
	LastCheckedAt       *time.Time `json:"last_checked_at,omitempty"`
	LastVerifiedAt      *time.Time `json:"last_verified_at,omitempty"`
	NextCheckAt         *time.Time `json:"next_check_at,omitempty"`
	RRSetSHA256         string     `json:"rrset_sha256,omitempty"`
	DNSSECAuthenticated bool       `json:"dnssec_authenticated"`
	MinimumTTLSeconds   *int64     `json:"minimum_ttl_seconds,omitempty"`
	ConsecutiveFailures int64      `json:"consecutive_failures"`
}

// AgentEmailDomainExpiration records an unverified challenge that aged out.
type AgentEmailDomainExpiration struct {
	ExpiredAt *time.Time `json:"expired_at,omitempty"`
	Reason    string     `json:"reason,omitempty"`
}

// AgentEmailDomainDecision records a terminal administrator rejection.
type AgentEmailDomainDecision struct {
	Action    string     `json:"action,omitempty"`
	Reason    string     `json:"reason,omitempty"`
	DecidedBy string     `json:"decided_by,omitempty"`
	DecidedAt *time.Time `json:"decided_at,omitempty"`
}

// AgentEmailDomainRetirement records a permanent request retirement.
type AgentEmailDomainRetirement struct {
	Reason    string     `json:"reason,omitempty"`
	RetiredBy string     `json:"retired_by,omitempty"`
	RetiredAt *time.Time `json:"retired_at,omitempty"`
}

// AgentEmailDomainRequest is one account-level custom inbound-domain request
// in the exact v1 control-plane contract.
type AgentEmailDomainRequest struct {
	RequestID             string                                 `json:"id"`
	AccountID             string                                 `json:"account_id"`
	Domain                string                                 `json:"domain"`
	State                 string                                 `json:"state"`
	OwnershipChallenge    *AgentEmailDomainOwnershipChallenge    `json:"ownership_challenge"`
	RequestedBy           string                                 `json:"requested_by"`
	RequestedAt           *time.Time                             `json:"requested_at"`
	UpdatedAt             *time.Time                             `json:"updated_at"`
	DomainLimitAtRequest  *int64                                 `json:"domain_limit_at_request"`
	PlanRevision          int64                                  `json:"plan_revision"`
	PlanSnapshotHash      string                                 `json:"plan_snapshot_hash"`
	StateRevision         int64                                  `json:"state_revision"`
	Availability          string                                 `json:"availability"`
	PlanSuspended         bool                                   `json:"plan_suspended"`
	LifecycleSuspended    bool                                   `json:"lifecycle_suspended"`
	PlanGraceUntil        *time.Time                             `json:"plan_grace_until,omitempty"`
	OwnershipVerification *AgentEmailDomainOwnershipVerification `json:"ownership_verification,omitempty"`
	Expiration            *AgentEmailDomainExpiration            `json:"expiration,omitempty"`
	Decision              *AgentEmailDomainDecision              `json:"decision,omitempty"`
	Retirement            *AgentEmailDomainRetirement            `json:"retirement,omitempty"`
}

// AgentEmailDomainRequestPage is one bounded customer or administrator page.
type AgentEmailDomainRequestPage struct {
	Requests                  []AgentEmailDomainRequest `json:"requests"`
	Truncated                 bool                      `json:"truncated"`
	NextCursor                string                    `json:"next_cursor,omitempty"`
	TechnicalOpenRequestLimit int                       `json:"technical_open_request_limit,omitempty"`
	OpenRequests              *int                      `json:"open_requests,omitempty"`
	AllocatedDomains          *int                      `json:"allocated_domains,omitempty"`
}

// AgentEmailDomainAuditEvent is one administrator-visible registry lifecycle
// event. Metadata can include the bounded reason supplied by the administrator.
type AgentEmailDomainAuditEvent struct {
	Sequence         int64           `json:"sequence"`
	RegistryRevision int64           `json:"registry_revision"`
	OccurredAt       time.Time       `json:"occurred_at"`
	ActorKind        string          `json:"actor_kind"`
	ActorID          string          `json:"actor_id"`
	Action           string          `json:"action"`
	Target           string          `json:"target"`
	Metadata         json.RawMessage `json:"metadata,omitempty"`
}

// AgentEmailDomainAuditPage is one bounded, newest-first audit page.
type AgentEmailDomainAuditPage struct {
	Events     []AgentEmailDomainAuditEvent `json:"events"`
	Truncated  bool                         `json:"truncated"`
	NextCursor string                       `json:"next_cursor,omitempty"`
}

// AdminAgentEmailDomainRequestFilter limits the custom-domain review queue.
type AdminAgentEmailDomainRequestFilter struct {
	State     string
	AccountID string
	Domain    string
	Cursor    string
}

// AdminAgentEmailDomainAuditFilter limits one newest-first audit page.
type AdminAgentEmailDomainAuditFilter struct {
	Action    string
	AccountID string
	Domain    string
	Limit     int
	Cursor    string
}

type agentEmailDomainRequestEnvelope struct {
	SchemaVersion string                  `json:"schema_version"`
	Request       AgentEmailDomainRequest `json:"request"`
}

type agentEmailDomainRequestsEnvelope struct {
	SchemaVersion string `json:"schema_version"`
	AgentEmailDomainRequestPage
}

type agentEmailDomainAuditEnvelope struct {
	SchemaVersion string `json:"schema_version"`
	AgentEmailDomainAuditPage
}

func agentEmailDomainCustomerURL(controlPlane, accountID string) (string, error) {
	accountID, err := validateRealmEmailPathID("account id", accountID)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%s/v1/accounts/%s/email-domain-requests",
		strings.TrimRight(controlPlane, "/"), neturl.PathEscape(accountID)), nil
}

func agentEmailDomainAdminURL(controlPlane string) string {
	return strings.TrimRight(controlPlane, "/") +
		"/v1/admin/agent-email-domain-requests"
}

func agentEmailDomainRequestID(value string) (string, error) {
	return validateRealmEmailGeneratedID("request id", "aedr", value)
}

// RequestAgentEmailDomain submits one retry-safe custom inbound-domain
// request. The control plane remains authoritative for ownership and review.
func RequestAgentEmailDomain(
	ctx context.Context,
	controlPlane, operatorToken, accountID, domain, idempotencyKey string,
) (*AgentEmailDomainRequest, error) {
	url, err := agentEmailDomainCustomerURL(controlPlane, accountID)
	if err != nil {
		return nil, err
	}
	domain = strings.TrimSpace(domain)
	idempotencyKey = strings.TrimSpace(idempotencyKey)
	if domain == "" || idempotencyKey == "" {
		return nil, fmt.Errorf("domain and idempotency key are required")
	}
	body, err := json.Marshal(map[string]string{
		"domain": domain, "idempotency_key": idempotencyKey,
	})
	if err != nil {
		return nil, err
	}
	var out agentEmailDomainRequestEnvelope
	if err := doJSON(ctx, http.MethodPost, url, operatorToken, body, &out); err != nil {
		return nil, err
	}
	return &out.Request, nil
}

// ListAgentEmailDomainRequestsPage lists one bounded request page for an
// account. Cursor is the opaque next_cursor from the prior page.
func ListAgentEmailDomainRequestsPage(
	ctx context.Context,
	controlPlane, operatorToken, accountID, cursor string,
) (*AgentEmailDomainRequestPage, error) {
	url, err := agentEmailDomainCustomerURL(controlPlane, accountID)
	if err != nil {
		return nil, err
	}
	u, _ := neturl.Parse(url)
	if cursor = strings.TrimSpace(cursor); cursor != "" {
		query := u.Query()
		query.Set("cursor", cursor)
		u.RawQuery = query.Encode()
	}
	var out agentEmailDomainRequestsEnvelope
	if err := doJSON(ctx, http.MethodGet, u.String(), operatorToken, nil, &out); err != nil {
		return nil, err
	}
	return &out.AgentEmailDomainRequestPage, nil
}

// ListAgentEmailDomainRequests returns the first request page for one account.
func ListAgentEmailDomainRequests(
	ctx context.Context, controlPlane, operatorToken, accountID string,
) ([]AgentEmailDomainRequest, error) {
	page, err := ListAgentEmailDomainRequestsPage(ctx, controlPlane,
		operatorToken, accountID, "")
	if err != nil {
		return nil, err
	}
	return page.Requests, nil
}

func addAgentEmailDomainFilter(raw string, filter AdminAgentEmailDomainRequestFilter) string {
	u, _ := neturl.Parse(raw)
	query := u.Query()
	for key, value := range map[string]string{
		"state": filter.State, "account_id": filter.AccountID,
		"domain": filter.Domain, "cursor": filter.Cursor,
	} {
		if value = strings.TrimSpace(value); value != "" {
			query.Set(key, value)
		}
	}
	u.RawQuery = query.Encode()
	return u.String()
}

// ListAdminAgentEmailDomainRequestsPage returns one bounded custom-domain
// review page. Cursor is the opaque next_cursor from a prior response.
func ListAdminAgentEmailDomainRequestsPage(
	ctx context.Context, controlPlane, adminToken string,
	filter AdminAgentEmailDomainRequestFilter,
) (*AgentEmailDomainRequestPage, error) {
	url := addAgentEmailDomainFilter(agentEmailDomainAdminURL(controlPlane), filter)
	var out agentEmailDomainRequestsEnvelope
	if err := doJSON(ctx, http.MethodGet, url, adminToken, nil, &out); err != nil {
		return nil, err
	}
	return &out.AgentEmailDomainRequestPage, nil
}

// ListAdminAgentEmailDomainRequests returns the first matching page. Use the
// Page variant when the continuation cursor is needed.
func ListAdminAgentEmailDomainRequests(
	ctx context.Context, controlPlane, adminToken string,
	filter AdminAgentEmailDomainRequestFilter,
) ([]AgentEmailDomainRequest, error) {
	page, err := ListAdminAgentEmailDomainRequestsPage(ctx, controlPlane,
		adminToken, filter)
	if err != nil {
		return nil, err
	}
	return page.Requests, nil
}

// GetAdminAgentEmailDomainRequest returns one exact custom-domain request.
func GetAdminAgentEmailDomainRequest(
	ctx context.Context, controlPlane, adminToken, requestID string,
) (*AgentEmailDomainRequest, error) {
	requestID, err := agentEmailDomainRequestID(requestID)
	if err != nil {
		return nil, err
	}
	url := agentEmailDomainAdminURL(controlPlane) + "/" + neturl.PathEscape(requestID)
	var out agentEmailDomainRequestEnvelope
	if err := doJSON(ctx, http.MethodGet, url, adminToken, nil, &out); err != nil {
		return nil, err
	}
	return &out.Request, nil
}

func mutateAdminAgentEmailDomainRequest(
	ctx context.Context,
	controlPlane, adminToken, requestID, action, idempotencyKey, reason string,
) (*AgentEmailDomainRequest, error) {
	requestID, err := agentEmailDomainRequestID(requestID)
	if err != nil {
		return nil, err
	}
	idempotencyKey = strings.TrimSpace(idempotencyKey)
	reason = strings.TrimSpace(reason)
	if idempotencyKey == "" || reason == "" {
		return nil, fmt.Errorf("idempotency key and reason are required")
	}
	body, err := json.Marshal(map[string]string{
		"idempotency_key": idempotencyKey,
		"reason":          reason,
	})
	if err != nil {
		return nil, err
	}
	url := fmt.Sprintf("%s/%s:%s", agentEmailDomainAdminURL(controlPlane),
		neturl.PathEscape(requestID), action)
	var out agentEmailDomainRequestEnvelope
	if err := doJSON(ctx, http.MethodPost, url, adminToken, body, &out); err != nil {
		return nil, err
	}
	return &out.Request, nil
}

// RejectAdminAgentEmailDomainRequest terminally rejects one pending request.
func RejectAdminAgentEmailDomainRequest(
	ctx context.Context,
	controlPlane, adminToken, requestID, idempotencyKey, reason string,
) (*AgentEmailDomainRequest, error) {
	return mutateAdminAgentEmailDomainRequest(ctx, controlPlane, adminToken,
		requestID, "reject", idempotencyKey, reason)
}

// RetireAdminAgentEmailDomainRequest permanently retires one domain request
// without releasing the domain to a different account.
func RetireAdminAgentEmailDomainRequest(
	ctx context.Context,
	controlPlane, adminToken, requestID, idempotencyKey, reason string,
) (*AgentEmailDomainRequest, error) {
	return mutateAdminAgentEmailDomainRequest(ctx, controlPlane, adminToken,
		requestID, "retire", idempotencyKey, reason)
}

// VerifyAdminAgentEmailDomainRequest performs one retry-safe exact TXT
// ownership check. Verification records authority only; it does not activate
// mail routing or delivery.
func VerifyAdminAgentEmailDomainRequest(
	ctx context.Context,
	controlPlane, adminToken, requestID, idempotencyKey string,
) (*AgentEmailDomainRequest, error) {
	requestID, err := agentEmailDomainRequestID(requestID)
	if err != nil {
		return nil, err
	}
	idempotencyKey = strings.TrimSpace(idempotencyKey)
	if idempotencyKey == "" {
		return nil, fmt.Errorf("idempotency key is required")
	}
	body, err := json.Marshal(map[string]string{
		"idempotency_key": idempotencyKey,
	})
	if err != nil {
		return nil, err
	}
	url := fmt.Sprintf("%s/%s:verify", agentEmailDomainAdminURL(controlPlane),
		neturl.PathEscape(requestID))
	var out agentEmailDomainRequestEnvelope
	if err := doJSON(ctx, http.MethodPost, url, adminToken, body, &out); err != nil {
		return nil, err
	}
	return &out.Request, nil
}

// ListAdminAgentEmailDomainAuditPage returns one bounded, newest-first audit
// page. Limit zero selects the server default; otherwise it must be 1-100.
func ListAdminAgentEmailDomainAuditPage(
	ctx context.Context, controlPlane, adminToken string,
	filter AdminAgentEmailDomainAuditFilter,
) (*AgentEmailDomainAuditPage, error) {
	if filter.Limit < 0 || filter.Limit > 100 {
		return nil, fmt.Errorf("audit limit must be between 1 and 100")
	}
	url := strings.TrimRight(controlPlane, "/") +
		"/v1/admin/agent-email-domain-audit"
	u, _ := neturl.Parse(url)
	query := u.Query()
	for key, value := range map[string]string{
		"action": filter.Action, "account_id": filter.AccountID,
		"domain": filter.Domain, "cursor": filter.Cursor,
	} {
		if value = strings.TrimSpace(value); value != "" {
			query.Set(key, value)
		}
	}
	if filter.Limit > 0 {
		query.Set("limit", fmt.Sprintf("%d", filter.Limit))
	}
	u.RawQuery = query.Encode()
	var out agentEmailDomainAuditEnvelope
	if err := doJSON(ctx, http.MethodGet, u.String(), adminToken, nil, &out); err != nil {
		return nil, err
	}
	return &out.AgentEmailDomainAuditPage, nil
}

// ListAdminAgentEmailDomainAudit returns one audit page for compatibility.
func ListAdminAgentEmailDomainAudit(
	ctx context.Context, controlPlane, adminToken, action string, limit int,
) ([]AgentEmailDomainAuditEvent, error) {
	page, err := ListAdminAgentEmailDomainAuditPage(ctx, controlPlane,
		adminToken, AdminAgentEmailDomainAuditFilter{Action: action, Limit: limit})
	if err != nil {
		return nil, err
	}
	return page.Events, nil
}
