package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/witwave-ai/witself/internal/agentemail"
	"github.com/witwave-ai/witself/internal/plans"
)

// AgentEmailStorageStatus is the authenticated account-wide inbound-email
// storage posture. MaximumRawBytes is the effective per-message cap after the
// plan limit and the absolute transport ceiling are combined.
type AgentEmailStorageStatus struct {
	MaximumRawBytes    int64             `json:"maximum_raw_bytes"`
	AttachmentCapacity MemoryLimitStatus `json:"attachment_capacity"`
}

type agentEmailIngestAccountPolicy struct {
	Plan                    string
	Limits                  map[string]int64
	RetainedAttachmentBytes int64
}

// lockAgentEmailIngestAccountPolicy takes the ingestion-specific exclusive
// account lock from the outset. This avoids a share-lock upgrade deadlock and
// serializes the account-wide attachment admission decision across replicas.
// Plan snapshot transitions and retention already lock the same account row.
func lockAgentEmailIngestAccountPolicy(
	ctx context.Context,
	tx pgx.Tx,
	accountID string,
) (agentEmailIngestAccountPolicy, error) {
	var (
		status       string
		policy       agentEmailIngestAccountPolicy
		limitsJSON   []byte
		policiesJSON []byte
		featuresJSON []byte
		appliedAt    *time.Time
	)
	err := tx.QueryRow(ctx, `
		SELECT status,plan,plan_limits,plan_policies,plan_features,
		       plan_applied_at,retained_agent_email_attachment_bytes
		  FROM accounts
		 WHERE id=$1
		 FOR NO KEY UPDATE`, accountID).
		Scan(
			&status,
			&policy.Plan,
			&limitsJSON,
			&policiesJSON,
			&featuresJSON,
			&appliedAt,
			&policy.RetainedAttachmentBytes,
		)
	if errors.Is(err, pgx.ErrNoRows) {
		return agentEmailIngestAccountPolicy{}, ErrAccountNotFound
	}
	if err != nil {
		return agentEmailIngestAccountPolicy{},
			fmt.Errorf("lock account for agent-email ingestion: %w", err)
	}
	if status != "active" {
		return agentEmailIngestAccountPolicy{}, ErrAccountNotActive
	}
	if err := json.Unmarshal(limitsJSON, &policy.Limits); err != nil {
		return agentEmailIngestAccountPolicy{},
			fmt.Errorf("decode agent-email plan limits: %w", err)
	}
	var policies map[string]int64
	if err := json.Unmarshal(policiesJSON, &policies); err != nil {
		return agentEmailIngestAccountPolicy{},
			fmt.Errorf("decode agent-email plan policies: %w", err)
	}
	var features []string
	if err := json.Unmarshal(featuresJSON, &features); err != nil {
		return agentEmailIngestAccountPolicy{},
			fmt.Errorf("decode agent-email plan features: %w", err)
	}
	if appliedAt != nil {
		version, authoritative := policies[plans.AgentEmailEntitlementVersionPolicy]
		if authoritative &&
			(version != plans.AgentEmailEntitlementVersion ||
				!slices.Contains(features, plans.AgentEmailReceiveFeature)) {
			return agentEmailIngestAccountPolicy{},
				&FeatureNotEnabledError{Feature: plans.AgentEmailReceiveFeature}
		}
	}
	return policy, nil
}

func effectiveAgentEmailMaximumRawBytes(limits map[string]int64) int64 {
	maximum := int64(agentemail.RelayMaximumRawBytes)
	if planMaximum, capped := limits[plans.AgentEmailMaxRawBytesLimit]; capped && planMaximum < maximum {
		maximum = planMaximum
	}
	return maximum
}

func requireAgentEmailRawSize(
	policy agentEmailIngestAccountPolicy,
	rawBytes int64,
) error {
	maximum := effectiveAgentEmailMaximumRawBytes(policy.Limits)
	if rawBytes <= maximum {
		return nil
	}
	return &PlanLimitError{
		Dimension: plans.AgentEmailMaxRawBytesLimit,
		Used:      rawBytes,
		Max:       maximum,
		Plan:      policy.Plan,
	}
}

func agentEmailAttachmentCapacityStatus(
	used int64,
	limits map[string]int64,
) MemoryLimitStatus {
	maximum, capped := limits[plans.AgentEmailAttachmentStorageBytesLimit]
	if !capped {
		return MemoryLimitStatus{Used: used, Unlimited: true}
	}
	remaining := maximum - used
	if remaining < 0 {
		remaining = 0
	}
	// ceil(90%) without multiplication, which remains safe for every exact
	// JSON integer accepted by the plan catalog.
	warningAt := maximum - maximum/10
	return MemoryLimitStatus{
		Used:      used,
		Max:       &maximum,
		Remaining: &remaining,
		NearLimit: used >= warningAt,
		AtLimit:   used == maximum,
		OverLimit: used > maximum,
	}
}

func retainAgentEmailAttachmentPayload(
	policy agentEmailIngestAccountPolicy,
	storageBytes int64,
) bool {
	if storageBytes <= 0 {
		return true
	}
	maximum, capped := policy.Limits[plans.AgentEmailAttachmentStorageBytesLimit]
	if !capped {
		return true
	}
	return storageBytes <= maximum &&
		policy.RetainedAttachmentBytes <= maximum-storageBytes
}

// GetAgentEmailStorageStatus returns value-free account-wide storage capacity
// for one live authenticated agent. It does not require the process-local
// pilot allowlist, so account policy remains inspectable independently from a
// particular server replica's pilot configuration.
func (s *Store) GetAgentEmailStorageStatus(
	ctx context.Context,
	p Principal,
) (AgentEmailStorageStatus, error) {
	if p.Kind != PrincipalAgent ||
		p.AccountID == "" || p.RealmID == "" || p.ID == "" {
		return AgentEmailStorageStatus{}, ErrAgentEmailForbidden
	}
	var (
		accountStatus string
		limitsJSON    []byte
		used          int64
	)
	err := s.pool.QueryRow(ctx, `
		SELECT account.status,account.plan_limits,
		       account.retained_agent_email_attachment_bytes
		  FROM accounts account
		  JOIN realms realm
		    ON realm.account_id=account.id
		   AND realm.id=$2
		   AND realm.deleted_at IS NULL
		  JOIN agents agent
		    ON agent.realm_id=realm.id
		   AND agent.id=$3
		   AND agent.deleted_at IS NULL
		 WHERE account.id=$1`,
		p.AccountID, p.RealmID, p.ID,
	).Scan(&accountStatus, &limitsJSON, &used)
	if errors.Is(err, pgx.ErrNoRows) {
		return AgentEmailStorageStatus{}, ErrAgentEmailForbidden
	}
	if err != nil {
		return AgentEmailStorageStatus{},
			fmt.Errorf("read agent-email storage capacity: %w", err)
	}
	if accountStatus != "active" {
		return AgentEmailStorageStatus{}, ErrAccountNotActive
	}
	var limits map[string]int64
	if err := json.Unmarshal(limitsJSON, &limits); err != nil {
		return AgentEmailStorageStatus{},
			fmt.Errorf("decode agent-email storage limits: %w", err)
	}
	return AgentEmailStorageStatus{
		MaximumRawBytes:    effectiveAgentEmailMaximumRawBytes(limits),
		AttachmentCapacity: agentEmailAttachmentCapacityStatus(used, limits),
	}, nil
}
