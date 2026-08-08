package client

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// AgentEmailDomainRecoveryHeader is the independent credential boundary for
// custom-domain journal maintenance and empty-target recovery.
const AgentEmailDomainRecoveryHeader = "X-Witself-Agent-Email-Domain-Recovery"

// AgentEmailDomainJournalHead is the exact value-free authority fence exposed
// by the control plane.
type AgentEmailDomainJournalHead struct {
	SchemaVersion    string `json:"schema_version,omitempty"`
	StreamID         string `json:"stream_id"`
	Sequence         int64  `json:"sequence"`
	Hash             string `json:"hash"`
	AuthorityEpoch   int64  `json:"authority_epoch"`
	RegistryRevision int64  `json:"registry_revision"`
	AuditSequence    int64  `json:"audit_sequence"`
	UpdatedAt        string `json:"updated_at,omitempty"`
}

// AgentEmailDomainExactHead is the caller-supplied recovery point. The stream
// is carried separately, so this fence intentionally contains only the two
// fields the server accepts.
type AgentEmailDomainExactHead struct {
	Sequence int64  `json:"sequence"`
	Hash     string `json:"hash"`
}

// AgentEmailDomainJournalProgress is one bounded bootstrap or checkpoint
// response. Incomplete work remains frozen and must be retried with the exact
// same idempotency key and reason.
type AgentEmailDomainJournalProgress struct {
	SchemaVersion string                       `json:"schema_version,omitempty"`
	Kind          string                       `json:"kind"`
	Phase         string                       `json:"phase"`
	Complete      bool                         `json:"complete"`
	Frozen        bool                         `json:"frozen"`
	AuthorityKeys int64                        `json:"authority_keys"`
	ScannedKeys   int64                        `json:"scanned_keys"`
	Head          *AgentEmailDomainJournalHead `json:"head"`
	Pending       bool                         `json:"pending"`
}

// AgentEmailDomainJournalStatus is the value-free active-registry health
// response. It never includes a domain, ownership challenge, or request body.
type AgentEmailDomainJournalStatus struct {
	SchemaVersion string                           `json:"schema_version"`
	Enabled       bool                             `json:"enabled"`
	Required      bool                             `json:"required"`
	Head          *AgentEmailDomainJournalHead     `json:"head"`
	Pending       bool                             `json:"pending"`
	Forked        bool                             `json:"forked"`
	Bootstrap     *AgentEmailDomainJournalProgress `json:"bootstrap"`
}

// AgentEmailDomainRecoveryStatus is one named empty-target recovery fence.
// A sealed target is drill evidence only; this API has no promotion selector.
type AgentEmailDomainRecoveryStatus struct {
	SchemaVersion string                       `json:"schema_version,omitempty"`
	RecoveryID    string                       `json:"recovery_id"`
	SourceStream  string                       `json:"source_stream_id"`
	ExpectedHead  *AgentEmailDomainExactHead   `json:"expected_head"`
	ReplayHead    *AgentEmailDomainJournalHead `json:"replay_head"`
	Phase         string                       `json:"phase"`
	AuthorityKeys int64                        `json:"authority_keys"`
	DerivedKeys   int64                        `json:"derived_keys"`
	StateDigest   string                       `json:"state_digest,omitempty"`
	ActionFence   string                       `json:"action_fence,omitempty"`
	Sealed        bool                         `json:"sealed"`
	Failed        bool                         `json:"failed"`
	FailureCode   string                       `json:"failure_code,omitempty"`
	CreatedAt     string                       `json:"created_at,omitempty"`
	UpdatedAt     string                       `json:"updated_at,omitempty"`
	SealedAt      string                       `json:"sealed_at,omitempty"`
}

func agentEmailDomainJournalURL(controlPlane string) string {
	return strings.TrimRight(controlPlane, "/") +
		"/v1/admin/agent-email-domain-journal"
}

func agentEmailDomainRecoveriesURL(controlPlane string) string {
	return strings.TrimRight(controlPlane, "/") +
		"/v1/admin/agent-email-domain-recoveries"
}

func agentEmailDomainRecoveryHeaders(recoveryToken string) (map[string]string, error) {
	recoveryToken = strings.TrimSpace(recoveryToken)
	if recoveryToken == "" {
		return nil, fmt.Errorf("agent email domain recovery token is required")
	}
	return map[string]string{AgentEmailDomainRecoveryHeader: recoveryToken}, nil
}

// GetAdminAgentEmailDomainJournal reads the exact active journal fence.
func GetAdminAgentEmailDomainJournal(
	ctx context.Context, controlPlane, adminToken, recoveryToken string,
) (*AgentEmailDomainJournalStatus, error) {
	headers, err := agentEmailDomainRecoveryHeaders(recoveryToken)
	if err != nil {
		return nil, err
	}
	var out AgentEmailDomainJournalStatus
	if err := doJSONWithHeaders(ctx, http.MethodGet,
		agentEmailDomainJournalURL(controlPlane), adminToken, headers, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func mutateAdminAgentEmailDomainJournal(
	ctx context.Context,
	controlPlane, adminToken, recoveryToken, action, reason, idempotencyKey string,
) (*AgentEmailDomainJournalProgress, error) {
	headers, err := agentEmailDomainRecoveryHeaders(recoveryToken)
	if err != nil {
		return nil, err
	}
	reason = strings.TrimSpace(reason)
	idempotencyKey = strings.TrimSpace(idempotencyKey)
	if reason == "" || idempotencyKey == "" {
		return nil, fmt.Errorf("reason and idempotency key are required")
	}
	body, err := json.Marshal(map[string]string{
		"reason": reason, "idempotency_key": idempotencyKey,
	})
	if err != nil {
		return nil, err
	}
	var out AgentEmailDomainJournalProgress
	if err := doJSONWithHeaders(ctx, http.MethodPost,
		agentEmailDomainJournalURL(controlPlane)+":"+action,
		adminToken, headers, body, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// BootstrapAdminAgentEmailDomainJournal snapshots an existing global registry.
func BootstrapAdminAgentEmailDomainJournal(
	ctx context.Context,
	controlPlane, adminToken, recoveryToken, reason, idempotencyKey string,
) (*AgentEmailDomainJournalProgress, error) {
	return mutateAdminAgentEmailDomainJournal(ctx, controlPlane, adminToken,
		recoveryToken, "bootstrap", reason, idempotencyKey)
}

// CheckpointAdminAgentEmailDomainJournal appends one complete authority fence.
func CheckpointAdminAgentEmailDomainJournal(
	ctx context.Context,
	controlPlane, adminToken, recoveryToken, reason, idempotencyKey string,
) (*AgentEmailDomainJournalProgress, error) {
	return mutateAdminAgentEmailDomainJournal(ctx, controlPlane, adminToken,
		recoveryToken, "checkpoint", reason, idempotencyKey)
}

// StartAdminAgentEmailDomainRecovery begins replay into the only permitted
// named empty target for recoveryID.
func StartAdminAgentEmailDomainRecovery(
	ctx context.Context,
	controlPlane, adminToken, recoveryToken, recoveryID, sourceStream string,
	expectedSequence int64, expectedHash, reason, idempotencyKey string,
) (*AgentEmailDomainRecoveryStatus, error) {
	headers, err := agentEmailDomainRecoveryHeaders(recoveryToken)
	if err != nil {
		return nil, err
	}
	recoveryID = strings.TrimSpace(recoveryID)
	sourceStream = strings.TrimSpace(sourceStream)
	expectedHash = strings.TrimSpace(expectedHash)
	reason = strings.TrimSpace(reason)
	idempotencyKey = strings.TrimSpace(idempotencyKey)
	if recoveryID == "" || sourceStream == "" || expectedSequence < 1 ||
		expectedHash == "" || reason == "" || idempotencyKey == "" {
		return nil, fmt.Errorf("recovery id, source stream, exact expected head, reason, and idempotency key are required")
	}
	body, err := json.Marshal(map[string]any{
		"recovery_id":      recoveryID,
		"source_stream_id": sourceStream,
		"expected_head": map[string]any{
			"sequence": expectedSequence,
			"hash":     expectedHash,
		},
		"reason":          reason,
		"idempotency_key": idempotencyKey,
	})
	if err != nil {
		return nil, err
	}
	var out AgentEmailDomainRecoveryStatus
	if err := doJSONWithHeaders(ctx, http.MethodPost,
		agentEmailDomainRecoveriesURL(controlPlane), adminToken, headers, body, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// GetAdminAgentEmailDomainRecovery reads one exact named recovery fence.
func GetAdminAgentEmailDomainRecovery(
	ctx context.Context,
	controlPlane, adminToken, recoveryToken, recoveryID string,
) (*AgentEmailDomainRecoveryStatus, error) {
	headers, err := agentEmailDomainRecoveryHeaders(recoveryToken)
	if err != nil {
		return nil, err
	}
	recoveryID = strings.TrimSpace(recoveryID)
	if recoveryID == "" {
		return nil, fmt.Errorf("recovery id is required")
	}
	var out AgentEmailDomainRecoveryStatus
	endpoint := agentEmailDomainRecoveriesURL(controlPlane) + "/" +
		url.PathEscape(recoveryID)
	if err := doJSONWithHeaders(ctx, http.MethodGet, endpoint, adminToken,
		headers, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func mutateAdminAgentEmailDomainRecovery(
	ctx context.Context,
	controlPlane, adminToken, recoveryToken, recoveryID, action,
	idempotencyKey, expectedActionFence string,
) (*AgentEmailDomainRecoveryStatus, error) {
	headers, err := agentEmailDomainRecoveryHeaders(recoveryToken)
	if err != nil {
		return nil, err
	}
	recoveryID = strings.TrimSpace(recoveryID)
	idempotencyKey = strings.TrimSpace(idempotencyKey)
	expectedActionFence = strings.TrimSpace(expectedActionFence)
	if recoveryID == "" || idempotencyKey == "" || expectedActionFence == "" {
		return nil, fmt.Errorf("recovery id, idempotency key, and expected action fence are required")
	}
	body, err := json.Marshal(map[string]string{
		"idempotency_key":       idempotencyKey,
		"expected_action_fence": expectedActionFence,
	})
	if err != nil {
		return nil, err
	}
	var out AgentEmailDomainRecoveryStatus
	endpoint := agentEmailDomainRecoveriesURL(controlPlane) + "/" +
		url.PathEscape(recoveryID) + ":" + action
	if err := doJSONWithHeaders(ctx, http.MethodPost, endpoint, adminToken,
		headers, body, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// AdvanceAdminAgentEmailDomainRecovery replays exactly one journal entry.
func AdvanceAdminAgentEmailDomainRecovery(
	ctx context.Context,
	controlPlane, adminToken, recoveryToken, recoveryID,
	idempotencyKey, expectedActionFence string,
) (*AgentEmailDomainRecoveryStatus, error) {
	return mutateAdminAgentEmailDomainRecovery(ctx, controlPlane, adminToken,
		recoveryToken, recoveryID, "advance", idempotencyKey,
		expectedActionFence)
}

// VerifyAdminAgentEmailDomainRecovery advances one bounded derived-state
// rebuild/verification page and seals the target only after exact validation.
func VerifyAdminAgentEmailDomainRecovery(
	ctx context.Context,
	controlPlane, adminToken, recoveryToken, recoveryID,
	idempotencyKey, expectedActionFence string,
) (*AgentEmailDomainRecoveryStatus, error) {
	return mutateAdminAgentEmailDomainRecovery(ctx, controlPlane, adminToken,
		recoveryToken, recoveryID, "verify", idempotencyKey,
		expectedActionFence)
}
