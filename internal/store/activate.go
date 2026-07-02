package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// ErrAccountNotActivatable is returned by ActivateAccount when the account is
// in a state activation must not touch — closed (the verification link
// outlived its account), or any future non-pending state. Fail closed: only
// 'pending' activates.
var ErrAccountNotActivatable = errors.New("account cannot be activated")

// ActivateAccount flips a pending account to active — the moment its
// activation gate (email verification) passes. The mirror of
// ReapPendingAccount: provision-token callers only, FOR UPDATE, and a strict
// status allowlist. Already-active returns activated=false with no error
// (an idempotent second click); everything else refuses.
func (s *Store) ActivateAccount(ctx context.Context, accountID string) (activated bool, err error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var status string
	err = tx.QueryRow(ctx,
		`SELECT status FROM accounts WHERE id = $1 FOR UPDATE`,
		accountID).Scan(&status)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, ErrAccountNotFound
	}
	if err != nil {
		return false, fmt.Errorf("verify activation target: %w", err)
	}
	switch status {
	case "active":
		return false, nil // idempotent: a second click on the same link
	case "pending":
	default:
		return false, ErrAccountNotActivatable
	}

	if _, err := tx.Exec(ctx,
		`UPDATE accounts SET status = 'active' WHERE id = $1`, accountID); err != nil {
		return false, fmt.Errorf("activate account: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return false, err
	}
	return true, nil
}
