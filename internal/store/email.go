package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// UndoAccountEmail is the control-plane-initiated revert: the human clicked
// the 48-hour undo link in their old inbox, so the account goes back to the
// address it came from. The cell enforces one guard — the current email must
// still match what the control plane snapshotted at change time — so a stale
// link cannot roll back a subsequent legitimate change.
func (s *Store) UndoAccountEmail(ctx context.Context, accountID, expectedCurrent, newEmail string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var current *string
	err = tx.QueryRow(ctx,
		`SELECT email FROM accounts WHERE id = $1 FOR UPDATE`,
		accountID).Scan(&current)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrAccountNotFound
	}
	if err != nil {
		return fmt.Errorf("verify email undo: %w", err)
	}
	if current == nil || *current != expectedCurrent {
		return ErrConflictingUndo
	}
	if _, err := tx.Exec(ctx,
		`UPDATE accounts SET email = $2 WHERE id = $1`, accountID, newEmail); err != nil {
		return fmt.Errorf("undo email change: %w", err)
	}
	return tx.Commit(ctx)
}

// ErrConflictingUndo is returned when the undo target does not match the
// current email — i.e. a stale undo link after a subsequent legitimate change.
var ErrConflictingUndo = errors.New("email has changed since this undo was issued")

// UpdateAccountDisplayName changes the account's server-side display name —
// cosmetic, but account-level, so it keeps the same owner-only tier as email
// and close. (Local names are a per-machine concept and live in the CLI.)
func (s *Store) UpdateAccountDisplayName(ctx context.Context, accountID, operatorID, displayName string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var isOwner bool
	err = tx.QueryRow(ctx,
		`SELECT (o.is_root OR o.role = 'account_owner')
		 FROM accounts a
		 JOIN operators o ON o.account_id = a.id
		 WHERE a.id = $1 AND o.id = $2 AND o.deleted_at IS NULL
		 FOR UPDATE OF a`,
		accountID, operatorID).Scan(&isOwner)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrAccountNotFound
	}
	if err != nil {
		return fmt.Errorf("verify rename authority: %w", err)
	}
	if !isOwner {
		return ErrNotAccountOwner
	}
	if _, err := tx.Exec(ctx,
		`UPDATE accounts SET display_name = $2 WHERE id = $1`, accountID, displayName); err != nil {
		return fmt.Errorf("rename account: %w", err)
	}
	return tx.Commit(ctx)
}

// UpdateAccountEmail changes an account's contact email. Owner-only (email is
// account-level contact, same authority tier as close) and active-only — the
// control plane calls this after proving the NEW inbox can receive (emailed
// code), and it passes the acting operator so ownership is enforced here,
// where the truth lives.
func (s *Store) UpdateAccountEmail(ctx context.Context, accountID, operatorID, newEmail string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var status string
	var isOwner bool
	err = tx.QueryRow(ctx,
		`SELECT a.status, (o.is_root OR o.role = 'account_owner')
		 FROM accounts a
		 JOIN operators o ON o.account_id = a.id
		 WHERE a.id = $1 AND o.id = $2 AND o.deleted_at IS NULL
		 FOR UPDATE OF a`,
		accountID, operatorID).Scan(&status, &isOwner)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrAccountNotFound
	}
	if err != nil {
		return fmt.Errorf("verify email change authority: %w", err)
	}
	if !isOwner {
		return ErrNotAccountOwner
	}
	if status != "active" {
		return ErrAccountNotActive
	}
	if _, err := tx.Exec(ctx,
		`UPDATE accounts SET email = $2 WHERE id = $1`, accountID, newEmail); err != nil {
		return fmt.Errorf("update account email: %w", err)
	}
	return tx.Commit(ctx)
}
