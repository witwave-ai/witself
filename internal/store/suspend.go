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

// SuspendAccountSystem freezes an account under a MACHINE authority — the
// control plane acting for the fleet, not an operator. Only known categories
// are accepted ("evacuation" today; migration/billing later), and owners
// cannot resume these (ResumeAccountOwner's category check). Idempotent and
// preserving: an already-suspended account keeps its ORIGINAL category — an
// owner-suspended account being evacuated must come back owner-suspended.
// Closed accounts are a no-op (their tombstones export as-is); pending
// accounts refuse (the reaper clears them during drain).
func (s *Store) SuspendAccountSystem(ctx context.Context, accountID, category, reason string) error {
	if category != "evacuation" {
		return fmt.Errorf("unknown suspension category %q", category)
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var status string
	var isDefault bool
	err = tx.QueryRow(ctx,
		`SELECT status, is_default FROM accounts WHERE id = $1 FOR UPDATE`,
		accountID).Scan(&status, &isDefault)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrAccountNotFound
	}
	if err != nil {
		return fmt.Errorf("verify system suspend: %w", err)
	}
	if isDefault {
		return ErrCannotCloseDefault
	}
	switch status {
	case "suspended", "closed":
		return nil // idempotent; existing category/tombstone preserved
	case "active":
	default:
		return ErrAccountNotActive // pending — drain + reaper window clears these
	}

	var reasonValue any
	if reason != "" {
		reasonValue = reason
	}
	if _, err := tx.Exec(ctx,
		`UPDATE accounts SET status = 'suspended', suspended_at = now(),
		   suspended_for = $2, suspended_reason = $3
		 WHERE id = $1`, accountID, category, reasonValue); err != nil {
		return fmt.Errorf("system suspend account: %w", err)
	}
	return tx.Commit(ctx)
}

// ErrResumeWrongCategory is returned when a system resume names a category
// that does not match what the account is suspended for — the authority that
// suspended is the authority that resumes, in both directions.
var ErrResumeWrongCategory = errors.New("suspension category does not match")

// ResumeAccountSystem lifts a MACHINE-authority suspension — the restore path
// un-freezing an account after it lands on a new cell. Only known categories
// are accepted, and only a suspension of that same category is lifted: an
// owner-suspended account that was evacuated comes back owner-suspended, and
// this call leaves it that way. Idempotent when already active.
func (s *Store) ResumeAccountSystem(ctx context.Context, accountID, category string) error {
	if category != "evacuation" {
		return fmt.Errorf("unknown suspension category %q", category)
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var status string
	var suspendedFor *string
	err = tx.QueryRow(ctx,
		`SELECT status, suspended_for FROM accounts WHERE id = $1 FOR UPDATE`,
		accountID).Scan(&status, &suspendedFor)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrAccountNotFound
	}
	if err != nil {
		return fmt.Errorf("verify system resume: %w", err)
	}
	if status == "active" {
		return nil // idempotent — a retry after a lost response completes cleanly
	}
	if status != "suspended" {
		return ErrAccountNotSuspended // pending or closed — nothing to lift
	}
	if suspendedFor == nil || *suspendedFor != category {
		return ErrResumeWrongCategory
	}
	if _, err := tx.Exec(ctx,
		`UPDATE accounts SET status = 'active', suspended_at = NULL,
		   suspended_for = NULL, suspended_reason = NULL
		 WHERE id = $1`, accountID); err != nil {
		return fmt.Errorf("system resume account: %w", err)
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
