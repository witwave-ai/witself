package store

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"github.com/jackc/pgx/v5"
)

// ReconcileAgentEmailPilot preflights the complete process allowlist and then
// idempotently provisions exactly those mailboxes. It is intended for bounded
// startup reconciliation, not a public request path. A failure returns no
// partial result and keeps the server fail-closed; already-created enrolled
// mailboxes are safe and make a later retry convergent.
func (s *Store) ReconcileAgentEmailPilot(
	ctx context.Context,
	scope AgentEmailPilotScope,
) ([]AgentEmailAddress, error) {
	if !scope.Enabled {
		return nil, ErrAgentEmailPilotDisabled
	}
	domain, err := normalizeAgentEmailPilotScope(scope)
	if err != nil {
		return nil, err
	}
	realmIDs := enabledAgentEmailPilotIDs(scope.RealmIDs)
	agentIDs := enabledAgentEmailPilotIDs(scope.AgentIDs)
	// normalizeAgentEmailPilotScope already pins these cardinalities. Keep the
	// explicit check here so this method cannot silently widen if that shared
	// validator changes later.
	if len(realmIDs) != 1 || len(agentIDs) < 5 || len(agentIDs) > 10 {
		return nil, fmt.Errorf("%w: reconciliation scope is outside pilot bounds", ErrAgentEmailInputInvalid)
	}
	realmID := realmIDs[0]
	if _, err := requireAgentEmailPilotEnrollment(scope, realmID, agentIDs[0]); err != nil {
		return nil, err
	}

	// Preflight in one short read transaction. It closes before Ensure takes
	// account->agent write locks, preserving the global lock order.
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var accountID, accountStatus string
	err = tx.QueryRow(ctx, `
		SELECT r.account_id,a.status
		FROM realms r
		JOIN accounts a ON a.id=r.account_id
		WHERE r.id=$1 AND r.deleted_at IS NULL`, realmID).
		Scan(&accountID, &accountStatus)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("%w: enrolled pilot realm is not live", ErrAgentEmailPilotNotEnrolled)
	}
	if err != nil {
		return nil, fmt.Errorf("resolve agent-email pilot realm: %w", err)
	}
	if accountStatus != "active" {
		return nil, ErrAccountNotActive
	}
	rows, err := tx.Query(ctx, `
		SELECT id
		FROM agents
		WHERE realm_id=$1 AND deleted_at IS NULL AND id=ANY($2::text[])
		ORDER BY id`, realmID, agentIDs)
	if err != nil {
		return nil, fmt.Errorf("preflight agent-email pilot agents: %w", err)
	}
	found := make(map[string]bool, len(agentIDs))
	for rows.Next() {
		var agentID string
		if err := rows.Scan(&agentID); err != nil {
			rows.Close()
			return nil, err
		}
		found[agentID] = true
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()
	for _, agentID := range agentIDs {
		if !found[agentID] {
			return nil, fmt.Errorf(
				"%w: enrolled pilot agent %s is not live in realm %s",
				ErrAgentEmailPilotNotEnrolled, agentID, realmID,
			)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}

	addresses := make([]AgentEmailAddress, 0, len(agentIDs))
	for _, agentID := range agentIDs {
		address, err := s.EnsureAgentEmailMailbox(ctx, scope, accountID, realmID, agentID, "")
		if errors.Is(err, ErrAgentEmailAddressConflict) {
			// A second cell replica can race the same startup reconciliation.
			// PostgreSQL resolves the unique insert before returning the conflict;
			// accept only an exact same-owner/domain row committed by that peer.
			existing, lookupErr := s.GetAgentEmailAddress(ctx, scope, Principal{
				Kind: PrincipalAgent, ID: agentID, AccessProfile: AccessProfileFull,
				AccountID: accountID, RealmID: realmID, AccountStatus: "active",
			})
			if lookupErr == nil && existing.Domain == domain {
				address, err = existing, nil
			}
		}
		if err != nil {
			return nil, fmt.Errorf("reconcile agent-email mailbox for %s: %w", agentID, err)
		}
		addresses = append(addresses, address)
	}
	return addresses, nil
}

func enabledAgentEmailPilotIDs(values map[string]bool) []string {
	result := make([]string, 0, len(values))
	for value, enabled := range values {
		if enabled {
			result = append(result, value)
		}
	}
	sort.Strings(result)
	return result
}
