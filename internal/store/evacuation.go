package store

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"time"

	"github.com/jackc/pgx/v5"
)

var (
	// ErrEvacuationIDInvalid means the control plane did not provide one
	// bounded opaque lifecycle epoch.
	ErrEvacuationIDInvalid = errors.New("invalid evacuation id")
	// ErrAccountEvacuationInProgress means another durable move epoch owns the
	// account's cell-side write fence.
	ErrAccountEvacuationInProgress = errors.New("account evacuation is in progress")
	// ErrAccountEvacuationMismatch means a lifecycle maintenance call did not
	// carry the exact epoch that installed the account's write fence.
	ErrAccountEvacuationMismatch = errors.New("account evacuation id does not match")
	// ErrAccountEvacuationOutboundInFlight means at least one outbound email
	// has crossed the provider boundary and must finish its bounded exact-replay
	// lifecycle before the account can move to another cell.
	ErrAccountEvacuationOutboundInFlight = errors.New("outbound email delivery is still in flight")
)

var evacuationIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

// AccountEvacuation is the value-free cell acknowledgement returned to the
// control plane. Status is the account's real status: an owner-suspended or
// closed account remains that way while the independent evacuation marker is
// active.
type AccountEvacuation struct {
	AccountID    string
	EvacuationID string
	Role         string
	Status       string
	StartedAt    *time.Time
	CompletedAt  *time.Time
	Completed    bool
	Aborted      bool
}

func validateEvacuationID(evacuationID string) error {
	if !evacuationIDPattern.MatchString(evacuationID) {
		return ErrEvacuationIDInvalid
	}
	return nil
}

// setEvacuationAuthorityTx installs the exact transaction-local authority
// consumed by the schema-level account/tenant mutation triggers. The setting
// is local to this transaction and never leaks back into a pooled connection.
func setEvacuationAuthorityTx(
	ctx context.Context,
	tx pgx.Tx,
	evacuationID string,
) error {
	if err := validateEvacuationID(evacuationID); err != nil {
		return err
	}
	if _, err := tx.Exec(
		ctx,
		`SELECT set_config('witself.evacuation_id', $1, true)`,
		evacuationID,
	); err != nil {
		return fmt.Errorf("set evacuation authority: %w", err)
	}
	return nil
}

// BeginAccountEvacuation is the cell-side linearization point for an account
// move. The accounts row is locked FOR UPDATE; every tenant-table mutation
// takes the same row FOR SHARE through the database trigger. Therefore this
// call cannot install the marker until every pre-marker writer commits, and no
// post-marker writer can commit without this exact evacuation id.
func (s *Store) BeginAccountEvacuation(
	ctx context.Context,
	accountID, evacuationID, reason string,
) (AccountEvacuation, error) {
	if err := validateEvacuationID(evacuationID); err != nil {
		return AccountEvacuation{}, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return AccountEvacuation{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var status string
	var isDefault bool
	var suspendedFor, currentID, currentRole, lastID, lastOutcome *string
	var startedAt, completedAt *time.Time
	err = tx.QueryRow(ctx, `
		SELECT status, is_default, suspended_for,
		       evacuation_id, evacuation_started_at, evacuation_role,
		       last_evacuation_id, last_evacuation_completed_at,
		       last_evacuation_outcome
		  FROM accounts
		 WHERE id = $1
		 FOR UPDATE`,
		accountID,
	).Scan(
		&status, &isDefault, &suspendedFor,
		&currentID, &startedAt, &currentRole,
		&lastID, &completedAt, &lastOutcome,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return AccountEvacuation{}, ErrAccountNotFound
	}
	if err != nil {
		return AccountEvacuation{}, fmt.Errorf("lock evacuation target: %w", err)
	}
	if isDefault {
		return AccountEvacuation{}, ErrCannotCloseDefault
	}
	if currentID != nil {
		if *currentID != evacuationID {
			return AccountEvacuation{}, ErrAccountEvacuationInProgress
		}
		if currentRole == nil || *currentRole != "source" {
			return AccountEvacuation{}, ErrAccountEvacuationMismatch
		}
		if err := tx.Commit(ctx); err != nil {
			return AccountEvacuation{}, err
		}
		return AccountEvacuation{
			AccountID: accountID, EvacuationID: evacuationID,
			Role: "source", Status: status, StartedAt: startedAt,
		}, nil
	}
	if lastID != nil && *lastID == evacuationID {
		return AccountEvacuation{}, ErrAccountEvacuationMismatch
	}
	if status == "suspended" && suspendedFor != nil &&
		*suspendedFor == "evacuation" {
		// An old server could create this state without an exact epoch. A new
		// Begin must not adopt it: abort/complete would otherwise be unable to
		// tell whether this operation created the suspension and could
		// accidentally activate an account frozen by an older operation.
		return AccountEvacuation{}, ErrAccountEvacuationMismatch
	}
	switch status {
	case "active", "suspended", "closed":
		// All three are portable. Owner suspension and closed tombstones retain
		// their original status/category while the independent marker freezes
		// every mutation.
	default:
		return AccountEvacuation{}, ErrAccountNotActive
	}
	// The accounts row is already held FOR UPDATE. Outbound provider starts
	// take the corresponding tenant-write FOR SHARE fence first, so every
	// pre-marker start has drained and no new start can race this stable check.
	// Keep the account active and let workers finish exact receipt replay rather
	// than exporting provider_started as an irreversible ambiguous outcome. A
	// current server must also remain able to operate on a historical schema
	// while a migration is being verified or rolled forward, so probe for the
	// schema-89 table before referring to it.
	var outboundMessagesExist bool
	if err := tx.QueryRow(ctx, `
		SELECT to_regclass(
		         format('%I.%I', current_schema(),
		                'agent_email_outbound_messages')
		       ) IS NOT NULL`).Scan(&outboundMessagesExist); err != nil {
		return AccountEvacuation{}, fmt.Errorf(
			"check outbound email schema before evacuation: %w", err,
		)
	}
	var outboundProviderCallInFlight bool
	if outboundMessagesExist {
		if err := tx.QueryRow(ctx, `
			SELECT EXISTS (
				SELECT 1
				  FROM agent_email_outbound_messages
				 WHERE account_id = $1
				   AND state = 'provider_started'
			)`, accountID).Scan(&outboundProviderCallInFlight); err != nil {
			return AccountEvacuation{}, fmt.Errorf(
				"check outbound email delivery before evacuation: %w", err,
			)
		}
	}
	if outboundProviderCallInFlight {
		return AccountEvacuation{}, ErrAccountEvacuationOutboundInFlight
	}
	if err := setEvacuationAuthorityTx(ctx, tx, evacuationID); err != nil {
		return AccountEvacuation{}, err
	}

	var reasonValue any
	if reason != "" {
		reasonValue = reason
	}
	if status == "active" {
		err = tx.QueryRow(ctx, `
			UPDATE accounts
			   SET status = 'suspended',
			       suspended_at = clock_timestamp(),
			       suspended_for = 'evacuation',
			       suspended_reason = $3,
			       evacuation_id = $2,
			       evacuation_started_at = clock_timestamp(),
			       evacuation_role = 'source'
			 WHERE id = $1
			 RETURNING status, evacuation_started_at`,
			accountID, evacuationID, reasonValue,
		).Scan(&status, &startedAt)
		if err != nil {
			return AccountEvacuation{}, fmt.Errorf("begin account evacuation: %w", err)
		}
		eventMeta := map[string]any{"category": "evacuation"}
		if reason != "" {
			eventMeta["reason"] = reason
		}
		if err := logEventTx(ctx, tx, EventInput{
			AccountID: accountID, ActorKind: ActorControlPlane,
			Verb: VerbAccountSuspendedBySystem, Metadata: eventMeta,
		}); err != nil {
			return AccountEvacuation{}, err
		}
	} else {
		err = tx.QueryRow(ctx, `
			UPDATE accounts
			   SET evacuation_id = $2,
			       evacuation_started_at = clock_timestamp(),
			       evacuation_role = 'source'
			 WHERE id = $1
			 RETURNING evacuation_started_at`,
			accountID, evacuationID,
		).Scan(&startedAt)
		if err != nil {
			return AccountEvacuation{}, fmt.Errorf("begin account evacuation: %w", err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return AccountEvacuation{}, err
	}
	return AccountEvacuation{
		AccountID: accountID, EvacuationID: evacuationID,
		Role: "source", Status: status, StartedAt: startedAt,
	}, nil
}

// CompleteAccountEvacuation removes the destination's write fence only for
// the exact move epoch. A lost response is safely retryable because the last
// completed epoch remains as an attestation after the active marker clears.
func (s *Store) CompleteAccountEvacuation(
	ctx context.Context,
	accountID, evacuationID string,
) (AccountEvacuation, error) {
	if err := validateEvacuationID(evacuationID); err != nil {
		return AccountEvacuation{}, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return AccountEvacuation{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var status string
	var purged bool
	var suspendedFor, currentID, currentRole, lastID, lastOutcome *string
	var startedAt, completedAt *time.Time
	err = tx.QueryRow(ctx, `
		SELECT status, purged_at IS NOT NULL, suspended_for,
		       evacuation_id, evacuation_started_at,
		       evacuation_role,
		       last_evacuation_id, last_evacuation_completed_at,
		       last_evacuation_outcome
		  FROM accounts
		 WHERE id = $1
		 FOR UPDATE`,
		accountID,
	).Scan(
		&status, &purged, &suspendedFor, &currentID, &startedAt, &currentRole,
		&lastID, &completedAt, &lastOutcome,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return AccountEvacuation{}, ErrAccountNotFound
	}
	if err != nil {
		return AccountEvacuation{}, fmt.Errorf("lock evacuation completion: %w", err)
	}
	if currentID == nil {
		if lastID != nil && *lastID == evacuationID &&
			lastOutcome != nil && *lastOutcome == "completed" {
			if err := tx.Commit(ctx); err != nil {
				return AccountEvacuation{}, err
			}
			return AccountEvacuation{
				AccountID: accountID, EvacuationID: evacuationID,
				Role: "target", Status: status,
				CompletedAt: completedAt, Completed: true,
			}, nil
		}
		return AccountEvacuation{}, ErrAccountEvacuationMismatch
	}
	if *currentID != evacuationID {
		return AccountEvacuation{}, ErrAccountEvacuationMismatch
	}
	if currentRole == nil || *currentRole != "target" {
		return AccountEvacuation{}, ErrAccountEvacuationMismatch
	}
	if err := setEvacuationAuthorityTx(ctx, tx, evacuationID); err != nil {
		return AccountEvacuation{}, err
	}

	resumed := status == "suspended" &&
		suspendedFor != nil && *suspendedFor == "evacuation"
	if resumed {
		err = tx.QueryRow(ctx, `
			UPDATE accounts
			   SET status = 'active',
			       suspended_at = NULL,
			       suspended_for = NULL,
			       suspended_reason = NULL,
			       evacuation_id = NULL,
			       evacuation_started_at = NULL,
			       evacuation_role = NULL,
			       last_evacuation_id = $2,
			       last_evacuation_completed_at = clock_timestamp(),
			       last_evacuation_outcome = 'completed'
			 WHERE id = $1
			 RETURNING status, last_evacuation_completed_at`,
			accountID, evacuationID,
		).Scan(&status, &completedAt)
	} else {
		// Owner-suspended accounts and closed tombstones keep their original
		// lifecycle state. Only the independent move fence is retired.
		err = tx.QueryRow(ctx, `
			UPDATE accounts
			   SET evacuation_id = NULL,
			       evacuation_started_at = NULL,
			       evacuation_role = NULL,
			       last_evacuation_id = $2,
			       last_evacuation_completed_at = clock_timestamp(),
			       last_evacuation_outcome = 'completed'
			 WHERE id = $1
			 RETURNING status, last_evacuation_completed_at`,
			accountID, evacuationID,
		).Scan(&status, &completedAt)
	}
	if err != nil {
		return AccountEvacuation{}, fmt.Errorf("complete account evacuation: %w", err)
	}
	if resumed {
		if err := logEventTx(ctx, tx, EventInput{
			AccountID: accountID, ActorKind: ActorControlPlane,
			Verb:     VerbAccountResumedBySystem,
			Metadata: map[string]any{"category": "evacuation"},
		}); err != nil {
			return AccountEvacuation{}, err
		}
	}
	// The target-side arrival is part of the same exact completion transaction
	// as clearing its write fence. A retry returns from the receipt branch
	// above, so the admin event is emitted exactly once. A purged tombstone must
	// retain only its single value-free account.purged erasure record; the
	// last_evacuation_* receipt above remains its value-free move evidence.
	if !purged {
		if err := logEventTx(ctx, tx, EventInput{
			AccountID: accountID, ActorKind: ActorSystem,
			Verb: VerbAccountRestored, Metadata: map[string]any{},
		}); err != nil {
			return AccountEvacuation{}, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return AccountEvacuation{}, err
	}
	return AccountEvacuation{
		AccountID: accountID, EvacuationID: evacuationID,
		Role: "target", Status: status,
		CompletedAt: completedAt, Completed: true,
	}, nil
}

// AbortAccountEvacuation safely gives a source account back to its original
// serving lifecycle when no verified archive became authoritative. It is
// exact-id and receipt-backed so a lost response is retryable, while an abort
// can never be mistaken for a completed target import.
func (s *Store) AbortAccountEvacuation(
	ctx context.Context,
	accountID, evacuationID string,
) (AccountEvacuation, error) {
	if err := validateEvacuationID(evacuationID); err != nil {
		return AccountEvacuation{}, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return AccountEvacuation{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var status string
	var suspendedFor, currentID, currentRole, lastID, lastOutcome *string
	var completedAt *time.Time
	err = tx.QueryRow(ctx, `
		SELECT status, suspended_for, evacuation_id, evacuation_role,
		       last_evacuation_id, last_evacuation_completed_at,
		       last_evacuation_outcome
		  FROM accounts
		 WHERE id = $1
		 FOR UPDATE`,
		accountID,
	).Scan(
		&status, &suspendedFor, &currentID, &currentRole,
		&lastID, &completedAt, &lastOutcome,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return AccountEvacuation{}, ErrAccountNotFound
	}
	if err != nil {
		return AccountEvacuation{}, fmt.Errorf("lock evacuation abort: %w", err)
	}
	if currentID == nil {
		if lastID != nil && *lastID == evacuationID &&
			lastOutcome != nil && *lastOutcome == "aborted" {
			if err := tx.Commit(ctx); err != nil {
				return AccountEvacuation{}, err
			}
			return AccountEvacuation{
				AccountID: accountID, EvacuationID: evacuationID,
				Role: "source", Status: status,
				CompletedAt: completedAt, Aborted: true,
			}, nil
		}
		return AccountEvacuation{}, ErrAccountEvacuationMismatch
	}
	if *currentID != evacuationID {
		return AccountEvacuation{}, ErrAccountEvacuationMismatch
	}
	if currentRole == nil || *currentRole != "source" {
		return AccountEvacuation{}, ErrAccountEvacuationMismatch
	}
	if err := setEvacuationAuthorityTx(ctx, tx, evacuationID); err != nil {
		return AccountEvacuation{}, err
	}

	resumed := status == "suspended" &&
		suspendedFor != nil && *suspendedFor == "evacuation"
	if resumed {
		err = tx.QueryRow(ctx, `
			UPDATE accounts
			   SET status = 'active',
			       suspended_at = NULL,
			       suspended_for = NULL,
			       suspended_reason = NULL,
			       evacuation_id = NULL,
			       evacuation_started_at = NULL,
			       evacuation_role = NULL,
			       last_evacuation_id = $2,
			       last_evacuation_completed_at = clock_timestamp(),
			       last_evacuation_outcome = 'aborted'
			 WHERE id = $1
			 RETURNING status, last_evacuation_completed_at`,
			accountID, evacuationID,
		).Scan(&status, &completedAt)
	} else {
		err = tx.QueryRow(ctx, `
			UPDATE accounts
			   SET evacuation_id = NULL,
			       evacuation_started_at = NULL,
			       evacuation_role = NULL,
			       last_evacuation_id = $2,
			       last_evacuation_completed_at = clock_timestamp(),
			       last_evacuation_outcome = 'aborted'
			 WHERE id = $1
			 RETURNING status, last_evacuation_completed_at`,
			accountID, evacuationID,
		).Scan(&status, &completedAt)
	}
	if err != nil {
		return AccountEvacuation{}, fmt.Errorf("abort account evacuation: %w", err)
	}
	if resumed {
		if err := logEventTx(ctx, tx, EventInput{
			AccountID: accountID, ActorKind: ActorControlPlane,
			Verb:     VerbAccountResumedBySystem,
			Metadata: map[string]any{"category": "evacuation"},
		}); err != nil {
			return AccountEvacuation{}, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return AccountEvacuation{}, err
	}
	return AccountEvacuation{
		AccountID: accountID, EvacuationID: evacuationID,
		Role: "source", Status: status,
		CompletedAt: completedAt, Aborted: true,
	}, nil
}
