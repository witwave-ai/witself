package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// ErrAccountNotSuspended is returned by ResumeAccount when the account is not
// in a suspended state — nothing to resume.
var ErrAccountNotSuspended = errors.New("account is not suspended")

// ErrCannotSelfResume is returned when the owner tries to resume a suspension
// they did not initiate (fleet-admin, migration, etc.). The authority that
// suspended must be the authority that resumes.
var ErrCannotSelfResume = errors.New("this suspension is not owner-resumable")

// SuspendAccountOwner freezes the account at the owner's request. Owner-only
// (same tier as close/change-email), active-only. suspendedFor names the
// suspending authority ("owner_request" for this path); reason is optional
// free-text metadata the owner attaches for their own bookkeeping.
func (s *Store) SuspendAccountOwner(ctx context.Context, accountID, operatorID, reason string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var status string
	var isOwner, isDefault bool
	err = tx.QueryRow(ctx,
		`SELECT a.status, a.is_default, (o.is_root OR o.role = 'account_owner')
		 FROM accounts a
		 JOIN operators o ON o.account_id = a.id
		 WHERE a.id = $1 AND o.id = $2 AND o.deleted_at IS NULL
		 FOR UPDATE OF a`,
		accountID, operatorID).Scan(&status, &isDefault, &isOwner)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrAccountNotFound
	}
	if err != nil {
		return fmt.Errorf("verify suspend authority: %w", err)
	}
	if !isOwner {
		return ErrNotAccountOwner
	}
	if isDefault {
		// The seeded default account is the deployment itself; nothing to
		// suspend against, and preventing writes there would freeze the
		// self-hosted instance.
		return ErrCannotCloseDefault
	}
	if status == "suspended" {
		return nil // idempotent — a retry after a lost response completes cleanly
	}
	if status != "active" {
		return ErrAccountNotActive
	}

	var reasonValue any
	if reason != "" {
		reasonValue = reason
	}
	if _, err := tx.Exec(ctx,
		`UPDATE accounts SET status = 'suspended', suspended_at = now(),
		   suspended_for = 'owner_request', suspended_reason = $2
		 WHERE id = $1`, accountID, reasonValue); err != nil {
		return fmt.Errorf("suspend account: %w", err)
	}
	return tx.Commit(ctx)
}

// ResumeAccountOwner un-suspends an owner-suspended account. Owner-only, and
// crucially refuses to un-suspend a fleet-admin/migration/etc. suspension:
// the authority that suspended is the authority that resumes.
func (s *Store) ResumeAccountOwner(ctx context.Context, accountID, operatorID string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var status string
	var isOwner bool
	var suspendedFor *string
	err = tx.QueryRow(ctx,
		`SELECT a.status, a.suspended_for, (o.is_root OR o.role = 'account_owner')
		 FROM accounts a
		 JOIN operators o ON o.account_id = a.id
		 WHERE a.id = $1 AND o.id = $2 AND o.deleted_at IS NULL
		 FOR UPDATE OF a`,
		accountID, operatorID).Scan(&status, &suspendedFor, &isOwner)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrAccountNotFound
	}
	if err != nil {
		return fmt.Errorf("verify resume authority: %w", err)
	}
	if !isOwner {
		return ErrNotAccountOwner
	}
	if status != "suspended" {
		return ErrAccountNotSuspended
	}
	if suspendedFor == nil || *suspendedFor != "owner_request" {
		return ErrCannotSelfResume
	}
	if _, err := tx.Exec(ctx,
		`UPDATE accounts SET status = 'active', suspended_at = NULL,
		   suspended_for = NULL, suspended_reason = NULL
		 WHERE id = $1`, accountID); err != nil {
		return fmt.Errorf("resume account: %w", err)
	}
	return tx.Commit(ctx)
}
