package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// SelfMessageCheckpoint is a content-free projection of durable messaging work
// that may need the authenticated agent's attention. It is advisory discovery
// state, not a processing fence, availability signal, or authorization grant.
type SelfMessageCheckpoint struct {
	Pending                     bool
	MailboxPending              bool
	CandidateOfferPending       bool
	CoordinatorSelectionPending bool
	CandidateAssignmentPending  bool
}

// GetSelfMessageCheckpoint reads one content-free snapshot of messaging work
// for the token-derived agent. The query is deliberately observational: it does
// not reconcile request lifecycle state, read or acknowledge a delivery, or
// acquire a processing claim.
func (s *Store) GetSelfMessageCheckpoint(ctx context.Context, p Principal) (SelfMessageCheckpoint, error) {
	if p.Kind != PrincipalAgent {
		return SelfMessageCheckpoint{}, ErrMessageForbidden
	}

	var live bool
	var checkpoint SelfMessageCheckpoint
	err := s.pool.QueryRow(ctx, `
		WITH checkpoint_clock AS MATERIALIZED (
			SELECT clock_timestamp() AS now
		), live_scope AS MATERIALIZED (
			SELECT 1
			  FROM agents a
			  JOIN realms r ON r.id = a.realm_id
			  JOIN accounts ac ON ac.id = r.account_id
			 WHERE a.id = $1 AND a.realm_id = $2 AND r.account_id = $3
			   AND a.deleted_at IS NULL AND r.deleted_at IS NULL
			   AND ac.status = 'active'
		)
		SELECT
			EXISTS (SELECT 1 FROM live_scope),
			EXISTS (
				SELECT 1
				  FROM agent_message_deliveries d
				  JOIN agent_messages m
				    ON m.id = d.message_id
				   AND m.account_id = d.account_id
				   AND m.realm_id = d.realm_id
				 WHERE EXISTS (SELECT 1 FROM live_scope)
				   AND d.account_id = $3 AND d.realm_id = $2
				   AND d.recipient_agent_id = $1 AND d.acked_at IS NULL
			),
			EXISTS (
				SELECT 1
				  FROM agent_message_request_candidates c
				  JOIN agent_message_requests r
				    ON r.id = c.request_id
				   AND r.account_id = c.account_id
				   AND r.realm_id = c.realm_id
				 CROSS JOIN checkpoint_clock
				 WHERE EXISTS (SELECT 1 FROM live_scope)
				   AND c.account_id = $3 AND c.realm_id = $2 AND c.agent_id = $1
				   AND c.response_state = 'pending'
				   AND r.state = 'open' AND r.expires_at > checkpoint_clock.now
				   AND r.offer_deadline > checkpoint_clock.now
			),
			EXISTS (
				SELECT 1
				  FROM agent_message_requests r
				 CROSS JOIN checkpoint_clock
				 WHERE EXISTS (SELECT 1 FROM live_scope)
				   AND r.account_id = $3 AND r.realm_id = $2
				   AND r.coordinator_agent_id = $1
				   AND r.state = 'open' AND r.expires_at > checkpoint_clock.now
				   AND (
					r.offer_deadline <= checkpoint_clock.now
					OR NOT EXISTS (
						SELECT 1
						  FROM agent_message_request_candidates pending
						 WHERE pending.request_id = r.id
						   AND pending.response_state = 'pending'
					)
				   )
				   AND NOT EXISTS (
					SELECT 1
					  FROM agent_message_request_claims active
					 WHERE active.request_id = r.id
					   AND active.state IN ('reserved', 'claimed')
					   AND active.lease_expires_at > checkpoint_clock.now
				   )
			),
			EXISTS (
				SELECT 1
				  FROM agent_message_request_claims own
				  JOIN agent_message_requests r
				    ON r.id = own.request_id
				   AND r.account_id = own.account_id
				   AND r.realm_id = own.realm_id
				 CROSS JOIN checkpoint_clock
				 WHERE EXISTS (SELECT 1 FROM live_scope)
				   AND own.account_id = $3 AND own.realm_id = $2 AND own.agent_id = $1
				   AND own.state IN ('reserved', 'claimed')
				   AND own.lease_expires_at > checkpoint_clock.now
				   AND r.state = 'open' AND r.expires_at > checkpoint_clock.now
			)`, p.ID, p.RealmID, p.AccountID).Scan(
		&live,
		&checkpoint.MailboxPending,
		&checkpoint.CandidateOfferPending,
		&checkpoint.CoordinatorSelectionPending,
		&checkpoint.CandidateAssignmentPending,
	)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && !live) {
		return SelfMessageCheckpoint{}, ErrAgentNotFound
	}
	if err != nil {
		return SelfMessageCheckpoint{}, fmt.Errorf("read self message checkpoint: %w", err)
	}
	checkpoint.Pending = checkpoint.MailboxPending || checkpoint.CandidateOfferPending ||
		checkpoint.CoordinatorSelectionPending || checkpoint.CandidateAssignmentPending
	return checkpoint, nil
}
