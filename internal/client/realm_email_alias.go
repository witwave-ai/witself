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

// RealmEmailAliasSchemaVersion identifies the control-plane alias contract.
const RealmEmailAliasSchemaVersion = "witself.realm-email-alias.v1"

// RealmEmailAliasRequest is one customer request for a globally unique,
// memorable realm email label. The control plane is authoritative for this
// lifecycle; cells and edge directories receive projections only.
type RealmEmailAliasRequest struct {
	ID          string                          `json:"id"`
	Alias       string                          `json:"alias"`
	Domain      string                          `json:"domain"`
	AccountID   string                          `json:"account_id"`
	RealmID     string                          `json:"realm_id"`
	Status      string                          `json:"status"`
	RequestedBy string                          `json:"requested_by,omitempty"`
	RequestedAt time.Time                       `json:"requested_at,omitempty"`
	UpdatedAt   time.Time                       `json:"updated_at,omitempty"`
	ActivatedAt *time.Time                      `json:"activated_at,omitempty"`
	Decision    *RealmEmailAliasRequestDecision `json:"decision,omitempty"`
}

// RealmEmailAliasRequestDecision is the platform administrator's immutable
// review record for an approved or rejected customer request.
type RealmEmailAliasRequestDecision struct {
	Action    string    `json:"action"`
	AdminID   string    `json:"admin_id"`
	Reason    string    `json:"reason"`
	DecidedAt time.Time `json:"decided_at"`
}

// RealmEmailAliasAssignment is an activated or historically reserved alias.
// Activated aliases are never transferred to another account or realm.
type RealmEmailAliasAssignment struct {
	ClaimID        string     `json:"claim_id"`
	Alias          string     `json:"alias"`
	Domain         string     `json:"domain"`
	AccountID      string     `json:"account_id"`
	RealmID        string     `json:"realm_id"`
	RequestID      string     `json:"request_id,omitempty"`
	Status         string     `json:"status"`
	AssignmentKind string     `json:"assignment_kind,omitempty"`
	Revision       int64      `json:"assignment_revision"`
	CreatedAt      time.Time  `json:"created_at,omitempty"`
	UpdatedAt      time.Time  `json:"updated_at,omitempty"`
	PlanGraceUntil *time.Time `json:"plan_grace_until,omitempty"`
	RetiredAt      *time.Time `json:"retired_at,omitempty"`
	// ProvisioningFailure is present on a terminally aborted internal
	// provisioning claim so administrators can distinguish recovery from an
	// ordinary retirement.
	ProvisioningFailure string `json:"provisioning_failure,omitempty"`
}

// RealmEmailReservedName is one platform-admin-owned protection in the global
// alias namespace. Skeleton is computed by the control plane and contains no
// customer data.
type RealmEmailReservedName struct {
	Name               string          `json:"name"`
	NormalizedName     string          `json:"normalized_name"`
	ConfusableSkeleton string          `json:"confusable_skeleton"`
	Category           string          `json:"category"`
	Reason             string          `json:"reason"`
	Enabled            bool            `json:"enabled"`
	InternalAssignable bool            `json:"internal_assignable,omitempty"`
	Version            int64           `json:"version"`
	PolicyVersion      int64           `json:"policy_version,omitempty"`
	CreatedAt          time.Time       `json:"created_at,omitempty"`
	UpdatedAt          time.Time       `json:"updated_at,omitempty"`
	RetiredAt          *time.Time      `json:"retired_at,omitempty"`
	CreatedBy          string          `json:"created_by,omitempty"`
	UpdatedBy          string          `json:"updated_by,omitempty"`
	ClaimConflict      json.RawMessage `json:"claim_conflict,omitempty"`
}

// RealmEmailAliasAuditEvent is a value-free global namespace change record.
type RealmEmailAliasAuditEvent struct {
	Sequence         int64           `json:"sequence"`
	RegistryRevision int64           `json:"registry_revision"`
	OccurredAt       time.Time       `json:"occurred_at"`
	ActorKind        string          `json:"actor_kind"`
	ActorID          string          `json:"actor_id"`
	Action           string          `json:"action"`
	Target           string          `json:"target"`
	Metadata         json.RawMessage `json:"metadata,omitempty"`
}

// RealmEmailAliasReviewResult includes the newly active assignment when an
// approval succeeds. Rejections contain only the reviewed request.
type RealmEmailAliasReviewResult struct {
	Request    RealmEmailAliasRequest     `json:"request"`
	Assignment *RealmEmailAliasAssignment `json:"assignment,omitempty"`
}

// RealmEmailAliasRequestPage is one bounded control-plane page. Filters are
// applied within that page, so an empty Requests slice can still carry a
// NextCursor that callers must follow to finish a filtered scan.
type RealmEmailAliasRequestPage struct {
	Requests   []RealmEmailAliasRequest `json:"requests"`
	Truncated  bool                     `json:"truncated"`
	NextCursor string                   `json:"next_cursor,omitempty"`
}

// RealmEmailAliasAssignmentPage is one bounded assignment-history page.
type RealmEmailAliasAssignmentPage struct {
	Aliases    []RealmEmailAliasAssignment `json:"aliases"`
	Truncated  bool                        `json:"truncated"`
	NextCursor string                      `json:"next_cursor,omitempty"`
}

// RealmEmailReservedNamePage is one bounded reserved-name policy page.
type RealmEmailReservedNamePage struct {
	ReservedPolicyVersion int64                    `json:"reserved_policy_version,omitempty"`
	ReservedNames         []RealmEmailReservedName `json:"reserved_names"`
	Truncated             bool                     `json:"truncated"`
	NextCursor            string                   `json:"next_cursor,omitempty"`
}

// RealmEmailAliasAuditPage is one bounded, newest-first audit page. Server-side
// action filtering can yield an empty page with a non-empty NextCursor.
type RealmEmailAliasAuditPage struct {
	Events     []RealmEmailAliasAuditEvent `json:"events"`
	Truncated  bool                        `json:"truncated"`
	NextCursor string                      `json:"next_cursor,omitempty"`
}

type realmEmailAliasRequestEnvelope struct {
	SchemaVersion string                 `json:"schema_version"`
	Request       RealmEmailAliasRequest `json:"request"`
}

type realmEmailAliasRequestsEnvelope struct {
	SchemaVersion string `json:"schema_version"`
	RealmEmailAliasRequestPage
}

type realmEmailAliasEnvelope struct {
	SchemaVersion string                    `json:"schema_version"`
	Assignment    RealmEmailAliasAssignment `json:"assignment"`
}

type realmEmailAliasesEnvelope struct {
	SchemaVersion string `json:"schema_version"`
	RealmEmailAliasAssignmentPage
}

type realmEmailReservedNameEnvelope struct {
	SchemaVersion string                 `json:"schema_version"`
	ReservedName  RealmEmailReservedName `json:"reserved_name"`
}

type realmEmailReservedNamesEnvelope struct {
	SchemaVersion string `json:"schema_version"`
	RealmEmailReservedNamePage
}

type realmEmailAliasAuditEnvelope struct {
	SchemaVersion string `json:"schema_version"`
	RealmEmailAliasAuditPage
}

func validateRealmEmailPathID(kind, value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > 128 {
		return "", fmt.Errorf("%s must be 1-128 characters", kind)
	}
	for _, r := range value {
		if r <= 0x20 || r == 0x7f || r == '/' || r == '\\' {
			return "", fmt.Errorf("%s contains an unsafe character", kind)
		}
	}
	return value, nil
}

func validateRealmEmailGeneratedID(kind, prefix, value string) (string, error) {
	value = strings.TrimSpace(value)
	body := strings.TrimPrefix(value, prefix+"_")
	if body == value || len(body) != 16 {
		return "", fmt.Errorf("%s must be a %s_ id", kind, prefix)
	}
	for _, character := range []byte(body) {
		if (character < 'a' || character > 'z') &&
			(character < '2' || character > '7') {
			return "", fmt.Errorf("%s must be a %s_ id", kind, prefix)
		}
	}
	return value, nil
}

func realmEmailAliasRequestsURL(controlPlane, accountID, realmID string) (string, error) {
	accountID, err := validateRealmEmailPathID("account id", accountID)
	if err != nil {
		return "", err
	}
	realmID, err = validateRealmEmailGeneratedID("realm id", "realm", realmID)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%s/v1/accounts/%s/realms/%s/email-alias-requests",
		strings.TrimRight(controlPlane, "/"), neturl.PathEscape(accountID),
		neturl.PathEscape(realmID)), nil
}

// RequestRealmEmailAlias submits one retry-safe customer request. Approval and
// global ownership remain platform-admin decisions.
func RequestRealmEmailAlias(
	ctx context.Context,
	controlPlane, operatorToken, accountID, realmID, alias, idempotencyKey string,
) (*RealmEmailAliasRequest, error) {
	url, err := realmEmailAliasRequestsURL(controlPlane, accountID, realmID)
	if err != nil {
		return nil, err
	}
	alias = strings.TrimSpace(alias)
	idempotencyKey = strings.TrimSpace(idempotencyKey)
	if alias == "" || idempotencyKey == "" {
		return nil, fmt.Errorf("alias and idempotency key are required")
	}
	body, err := json.Marshal(map[string]string{
		"alias": alias, "idempotency_key": idempotencyKey,
	})
	if err != nil {
		return nil, err
	}
	var out realmEmailAliasRequestEnvelope
	if err := doJSON(ctx, http.MethodPost, url, operatorToken, body, &out); err != nil {
		return nil, err
	}
	return &out.Request, nil
}

// ListRealmEmailAliasRequestsPage lists one bounded request page for a
// customer-owned realm. Cursor is the opaque next_cursor from the prior page.
func ListRealmEmailAliasRequestsPage(
	ctx context.Context,
	controlPlane, operatorToken, accountID, realmID, cursor string,
) (*RealmEmailAliasRequestPage, error) {
	url, err := realmEmailAliasRequestsURL(controlPlane, accountID, realmID)
	if err != nil {
		return nil, err
	}
	url = addRealmEmailAliasFilter(url, map[string]string{"cursor": cursor})
	var out realmEmailAliasRequestsEnvelope
	if err := doJSON(ctx, http.MethodGet, url, operatorToken, nil, &out); err != nil {
		return nil, err
	}
	return &out.RealmEmailAliasRequestPage, nil
}

// ListRealmEmailAliasRequests lists the first request page for one
// customer-owned realm. Use ListRealmEmailAliasRequestsPage when continuation
// beyond the control plane's bounded page is required.
func ListRealmEmailAliasRequests(
	ctx context.Context,
	controlPlane, operatorToken, accountID, realmID string,
) ([]RealmEmailAliasRequest, error) {
	page, err := ListRealmEmailAliasRequestsPage(ctx, controlPlane, operatorToken,
		accountID, realmID, "")
	if err != nil {
		return nil, err
	}
	return page.Requests, nil
}

// AdminRealmEmailAliasRequestFilter limits the platform review queue.
type AdminRealmEmailAliasRequestFilter struct {
	Status    string
	AccountID string
	RealmID   string
	Cursor    string
}

// AdminRealmEmailAliasFilter limits assignment history.
type AdminRealmEmailAliasFilter struct {
	Status    string
	AccountID string
	RealmID   string
	Cursor    string
}

// AdminRealmEmailReservedNameFilter limits one reserved-name page. Enabled is
// tri-state: nil omits the filter, while true and false select exact values.
type AdminRealmEmailReservedNameFilter struct {
	Category string
	Enabled  *bool
	Cursor   string
}

// AdminRealmEmailAliasAuditFilter limits one newest-first audit page. Limit is
// the bounded number of underlying audit rows scanned by the control plane,
// not a promise that an action-filtered page contains that many matches.
type AdminRealmEmailAliasAuditFilter struct {
	Action string
	Limit  int
	Cursor string
}

func addRealmEmailAliasFilter(raw string, values map[string]string) string {
	u, _ := neturl.Parse(raw)
	q := u.Query()
	for key, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			q.Set(key, value)
		}
	}
	u.RawQuery = q.Encode()
	return u.String()
}

// ListAdminRealmEmailAliasRequestsPage returns one bounded platform review page.
func ListAdminRealmEmailAliasRequestsPage(
	ctx context.Context, controlPlane, adminToken string,
	filter AdminRealmEmailAliasRequestFilter,
) (*RealmEmailAliasRequestPage, error) {
	url := strings.TrimRight(controlPlane, "/") +
		"/v1/admin/realm-email-alias-requests"
	url = addRealmEmailAliasFilter(url, map[string]string{
		"status": filter.Status, "account_id": filter.AccountID,
		"realm_id": filter.RealmID, "cursor": filter.Cursor,
	})
	var out realmEmailAliasRequestsEnvelope
	if err := doJSON(ctx, http.MethodGet, url, adminToken, nil, &out); err != nil {
		return nil, err
	}
	return &out.RealmEmailAliasRequestPage, nil
}

// ListAdminRealmEmailAliasRequests lists one request page for compatibility.
// Use ListAdminRealmEmailAliasRequestsPage to retain next_cursor.
func ListAdminRealmEmailAliasRequests(
	ctx context.Context, controlPlane, adminToken string,
	filter AdminRealmEmailAliasRequestFilter,
) ([]RealmEmailAliasRequest, error) {
	page, err := ListAdminRealmEmailAliasRequestsPage(ctx, controlPlane,
		adminToken, filter)
	if err != nil {
		return nil, err
	}
	return page.Requests, nil
}

func mutateAdminRealmEmailAliasRequest(
	ctx context.Context,
	controlPlane, adminToken, requestID, action, idempotencyKey, reason string,
) (*RealmEmailAliasReviewResult, error) {
	requestID, err := validateRealmEmailGeneratedID("request id", "earq", requestID)
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
	url := fmt.Sprintf("%s/v1/admin/realm-email-alias-requests/%s:%s",
		strings.TrimRight(controlPlane, "/"), neturl.PathEscape(requestID), action)
	var out RealmEmailAliasReviewResult
	if err := doJSON(ctx, http.MethodPost, url, adminToken, body, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ApproveAdminRealmEmailAliasRequest approves and provisions one customer request.
func ApproveAdminRealmEmailAliasRequest(
	ctx context.Context,
	controlPlane, adminToken, requestID, idempotencyKey, reason string,
) (*RealmEmailAliasReviewResult, error) {
	return mutateAdminRealmEmailAliasRequest(ctx, controlPlane, adminToken,
		requestID, "approve", idempotencyKey, reason)
}

// RejectAdminRealmEmailAliasRequest terminally rejects one customer request.
func RejectAdminRealmEmailAliasRequest(
	ctx context.Context,
	controlPlane, adminToken, requestID, idempotencyKey, reason string,
) (*RealmEmailAliasRequest, error) {
	result, err := mutateAdminRealmEmailAliasRequest(ctx, controlPlane, adminToken,
		requestID, "reject", idempotencyKey, reason)
	if err != nil {
		return nil, err
	}
	return &result.Request, nil
}

// ListAdminRealmEmailAliasesPage returns one bounded assignment-history page.
func ListAdminRealmEmailAliasesPage(
	ctx context.Context, controlPlane, adminToken string,
	filter AdminRealmEmailAliasFilter,
) (*RealmEmailAliasAssignmentPage, error) {
	url := strings.TrimRight(controlPlane, "/") +
		"/v1/admin/realm-email-aliases"
	url = addRealmEmailAliasFilter(url, map[string]string{
		"status": filter.Status, "account_id": filter.AccountID,
		"realm_id": filter.RealmID, "cursor": filter.Cursor,
	})
	var out realmEmailAliasesEnvelope
	if err := doJSON(ctx, http.MethodGet, url, adminToken, nil, &out); err != nil {
		return nil, err
	}
	return &out.RealmEmailAliasAssignmentPage, nil
}

// ListAdminRealmEmailAliases lists one assignment page for compatibility. Use
// ListAdminRealmEmailAliasesPage to retain next_cursor.
func ListAdminRealmEmailAliases(
	ctx context.Context, controlPlane, adminToken string,
	filter AdminRealmEmailAliasFilter,
) ([]RealmEmailAliasAssignment, error) {
	page, err := ListAdminRealmEmailAliasesPage(ctx, controlPlane, adminToken, filter)
	if err != nil {
		return nil, err
	}
	return page.Aliases, nil
}

func mutateAdminRealmEmailAlias(
	ctx context.Context,
	controlPlane, adminToken, alias, action, idempotencyKey, reason string,
) (*RealmEmailAliasAssignment, error) {
	alias = strings.TrimSpace(alias)
	if alias == "" || strings.TrimSpace(idempotencyKey) == "" ||
		strings.TrimSpace(reason) == "" {
		return nil, fmt.Errorf("alias, idempotency key, and reason are required")
	}
	body, err := json.Marshal(map[string]string{
		"idempotency_key": strings.TrimSpace(idempotencyKey),
		"reason":          strings.TrimSpace(reason),
	})
	if err != nil {
		return nil, err
	}
	url := fmt.Sprintf("%s/v1/admin/realm-email-aliases/%s:%s",
		strings.TrimRight(controlPlane, "/"), neturl.PathEscape(alias), action)
	var out realmEmailAliasEnvelope
	if err := doJSON(ctx, http.MethodPost, url, adminToken, body, &out); err != nil {
		return nil, err
	}
	return &out.Assignment, nil
}

// SuspendAdminRealmEmailAlias disables delivery while preserving ownership.
func SuspendAdminRealmEmailAlias(ctx context.Context, controlPlane, adminToken,
	alias, idempotencyKey, reason string) (*RealmEmailAliasAssignment, error) {
	return mutateAdminRealmEmailAlias(ctx, controlPlane, adminToken, alias,
		"suspend", idempotencyKey, reason)
}

// ReactivateAdminRealmEmailAlias restores delivery for an eligible assignment.
func ReactivateAdminRealmEmailAlias(ctx context.Context, controlPlane, adminToken,
	alias, idempotencyKey, reason string) (*RealmEmailAliasAssignment, error) {
	return mutateAdminRealmEmailAlias(ctx, controlPlane, adminToken, alias,
		"reactivate", idempotencyKey, reason)
}

// RetireAdminRealmEmailAlias permanently retires an assignment without reuse.
func RetireAdminRealmEmailAlias(ctx context.Context, controlPlane, adminToken,
	alias, idempotencyKey, reason string) (*RealmEmailAliasAssignment, error) {
	return mutateAdminRealmEmailAlias(ctx, controlPlane, adminToken, alias,
		"retire", idempotencyKey, reason)
}

// AbortAdminRealmEmailAliasProvisioning terminally retires one failed or stuck
// internal provisioning intent. It does not operate on active customer or
// internal assignments.
func AbortAdminRealmEmailAliasProvisioning(
	ctx context.Context,
	controlPlane, adminToken, alias, idempotencyKey, reason string,
) (*RealmEmailAliasAssignment, error) {
	return mutateAdminRealmEmailAlias(ctx, controlPlane, adminToken, alias,
		"abort-provisioning", idempotencyKey, reason)
}

// AssignInternalAdminRealmEmailAlias provisions one protected platform alias.
func AssignInternalAdminRealmEmailAlias(
	ctx context.Context,
	controlPlane, adminToken, accountID, realmID, alias, idempotencyKey, reason string,
) (*RealmEmailAliasAssignment, error) {
	accountID = strings.TrimSpace(accountID)
	realmID = strings.TrimSpace(realmID)
	alias = strings.TrimSpace(alias)
	idempotencyKey = strings.TrimSpace(idempotencyKey)
	reason = strings.TrimSpace(reason)
	if _, err := validateRealmEmailPathID("account id", accountID); err != nil {
		return nil, err
	}
	if _, err := validateRealmEmailGeneratedID("realm id", "realm", realmID); err != nil {
		return nil, err
	}
	if strings.TrimSpace(alias) == "" || strings.TrimSpace(idempotencyKey) == "" ||
		strings.TrimSpace(reason) == "" {
		return nil, fmt.Errorf("alias, idempotency key, and reason are required")
	}
	body, err := json.Marshal(map[string]string{
		"account_id": accountID, "realm_id": realmID, "alias": alias,
		"idempotency_key": idempotencyKey, "reason": reason,
	})
	if err != nil {
		return nil, err
	}
	url := strings.TrimRight(controlPlane, "/") +
		"/v1/admin/realm-email-aliases:assign-internal"
	var out realmEmailAliasEnvelope
	if err := doJSON(ctx, http.MethodPost, url, adminToken, body, &out); err != nil {
		return nil, err
	}
	return &out.Assignment, nil
}

// ListAdminRealmEmailReservedNamesPage returns one bounded policy page.
func ListAdminRealmEmailReservedNamesPage(
	ctx context.Context, controlPlane, adminToken string,
	filter AdminRealmEmailReservedNameFilter,
) (*RealmEmailReservedNamePage, error) {
	url := strings.TrimRight(controlPlane, "/") +
		"/v1/admin/realm-email-reserved-names"
	values := map[string]string{
		"category": filter.Category,
		"cursor":   filter.Cursor,
	}
	if filter.Enabled != nil {
		values["enabled"] = fmt.Sprintf("%t", *filter.Enabled)
	}
	url = addRealmEmailAliasFilter(url, values)
	var out realmEmailReservedNamesEnvelope
	if err := doJSON(ctx, http.MethodGet, url, adminToken, nil, &out); err != nil {
		return nil, err
	}
	return &out.RealmEmailReservedNamePage, nil
}

// ListAdminRealmEmailReservedNames lists the first unfiltered page for
// compatibility. Use ListAdminRealmEmailReservedNamesPage to retain
// next_cursor or apply filters.
func ListAdminRealmEmailReservedNames(
	ctx context.Context, controlPlane, adminToken string,
) ([]RealmEmailReservedName, error) {
	page, err := ListAdminRealmEmailReservedNamesPage(ctx, controlPlane,
		adminToken, AdminRealmEmailReservedNameFilter{})
	if err != nil {
		return nil, err
	}
	return page.ReservedNames, nil
}

// GetAdminRealmEmailReservedName returns one exact reserved-name policy record.
func GetAdminRealmEmailReservedName(
	ctx context.Context, controlPlane, adminToken, name string,
) (*RealmEmailReservedName, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, fmt.Errorf("name is required")
	}
	url := strings.TrimRight(controlPlane, "/") +
		"/v1/admin/realm-email-reserved-names/" + neturl.PathEscape(name)
	var out realmEmailReservedNameEnvelope
	if err := doJSON(ctx, http.MethodGet, url, adminToken, nil, &out); err != nil {
		return nil, err
	}
	return &out.ReservedName, nil
}

// CreateAdminRealmEmailReservedName adds one protected namespace entry.
func CreateAdminRealmEmailReservedName(
	ctx context.Context,
	controlPlane, adminToken, name, category, reason, idempotencyKey string,
	internalAssignable bool,
) (*RealmEmailReservedName, error) {
	name = strings.TrimSpace(name)
	category = strings.TrimSpace(category)
	reason = strings.TrimSpace(reason)
	idempotencyKey = strings.TrimSpace(idempotencyKey)
	if name == "" || category == "" || reason == "" || idempotencyKey == "" {
		return nil, fmt.Errorf("name, category, reason, and idempotency key are required")
	}
	body, err := json.Marshal(map[string]any{
		"name": name, "category": category, "reason": reason,
		"idempotency_key":     idempotencyKey,
		"internal_assignable": internalAssignable,
	})
	if err != nil {
		return nil, err
	}
	url := strings.TrimRight(controlPlane, "/") +
		"/v1/admin/realm-email-reserved-names"
	var out realmEmailReservedNameEnvelope
	if err := doJSON(ctx, http.MethodPost, url, adminToken, body, &out); err != nil {
		return nil, err
	}
	return &out.ReservedName, nil
}

// UpdateAdminRealmEmailReservedName updates one versioned namespace entry.
func UpdateAdminRealmEmailReservedName(
	ctx context.Context,
	controlPlane, adminToken, name, category, reason, idempotencyKey string,
	enabled, internalAssignable *bool,
) (*RealmEmailReservedName, error) {
	name = strings.TrimSpace(name)
	category = strings.TrimSpace(category)
	reason = strings.TrimSpace(reason)
	idempotencyKey = strings.TrimSpace(idempotencyKey)
	if name == "" || reason == "" || idempotencyKey == "" {
		return nil, fmt.Errorf("name, reason, and idempotency key are required")
	}
	payload := map[string]any{
		"reason": reason, "idempotency_key": idempotencyKey,
	}
	if strings.TrimSpace(category) != "" {
		payload["category"] = category
	}
	if enabled != nil {
		payload["enabled"] = *enabled
	}
	if internalAssignable != nil {
		payload["internal_assignable"] = *internalAssignable
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	url := strings.TrimRight(controlPlane, "/") +
		"/v1/admin/realm-email-reserved-names/" + neturl.PathEscape(name)
	var out realmEmailReservedNameEnvelope
	if err := doJSON(ctx, http.MethodPatch, url, adminToken, body, &out); err != nil {
		return nil, err
	}
	return &out.ReservedName, nil
}

// ListAdminRealmEmailAliasAuditPage returns one newest-first, bounded global
// namespace audit page. The control plane clamps limit to 1-500.
func ListAdminRealmEmailAliasAuditPage(
	ctx context.Context, controlPlane, adminToken string,
	filter AdminRealmEmailAliasAuditFilter,
) (*RealmEmailAliasAuditPage, error) {
	url := strings.TrimRight(controlPlane, "/") +
		"/v1/admin/realm-email-alias-audit"
	values := map[string]string{"action": filter.Action, "cursor": filter.Cursor}
	if filter.Limit > 0 {
		values["limit"] = fmt.Sprintf("%d", filter.Limit)
	}
	url = addRealmEmailAliasFilter(url, values)
	var out realmEmailAliasAuditEnvelope
	if err := doJSON(ctx, http.MethodGet, url, adminToken, nil, &out); err != nil {
		return nil, err
	}
	return &out.RealmEmailAliasAuditPage, nil
}

// ListAdminRealmEmailAliasAudit returns one audit page for compatibility. Use
// ListAdminRealmEmailAliasAuditPage to retain next_cursor.
func ListAdminRealmEmailAliasAudit(
	ctx context.Context, controlPlane, adminToken, action string, limit int,
) ([]RealmEmailAliasAuditEvent, error) {
	page, err := ListAdminRealmEmailAliasAuditPage(ctx, controlPlane, adminToken,
		AdminRealmEmailAliasAuditFilter{Action: action, Limit: limit})
	if err != nil {
		return nil, err
	}
	return page.Events, nil
}

// RetireAdminRealmEmailReservedName retires one policy entry without releasing claims.
func RetireAdminRealmEmailReservedName(
	ctx context.Context,
	controlPlane, adminToken, name, reason, idempotencyKey string,
) (*RealmEmailReservedName, error) {
	name = strings.TrimSpace(name)
	reason = strings.TrimSpace(reason)
	idempotencyKey = strings.TrimSpace(idempotencyKey)
	if name == "" || reason == "" || idempotencyKey == "" {
		return nil, fmt.Errorf("name, reason, and idempotency key are required")
	}
	body, err := json.Marshal(map[string]string{
		"reason": reason, "idempotency_key": idempotencyKey,
	})
	if err != nil {
		return nil, err
	}
	url := strings.TrimRight(controlPlane, "/") +
		"/v1/admin/realm-email-reserved-names/" + neturl.PathEscape(name)
	var out realmEmailReservedNameEnvelope
	if err := doJSON(ctx, http.MethodDelete, url, adminToken, body, &out); err != nil {
		return nil, err
	}
	return &out.ReservedName, nil
}
