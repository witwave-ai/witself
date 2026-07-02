package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

var (
	// ErrNotAccountOwner is returned when the acting operator is not the
	// account owner (closing an account is an owner-only action).
	ErrNotAccountOwner = errors.New("only the account owner can close the account")
	// ErrCannotCloseDefault is returned for the deployment's seeded root
	// account, which is the deployment itself and cannot be closed.
	ErrCannotCloseDefault = errors.New("the deployment's default account cannot be closed")
)

// CloseAccount permanently closes the operator's account: status -> 'closed'
// (a tombstone — the row lives forever) and every live credential on the
// account is revoked. Idempotent: closing an already-closed account succeeds.
// Owner-only; the seeded default account is refused.
func (s *Store) CloseAccount(ctx context.Context, accountID, operatorID, reason string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var isDefault bool
	var status string
	var isOwner bool
	err = tx.QueryRow(ctx,
		`SELECT a.is_default, a.status, (o.is_root OR o.role = 'account_owner')
		 FROM accounts a
		 JOIN operators o ON o.account_id = a.id
		 WHERE a.id = $1 AND o.id = $2 AND o.deleted_at IS NULL`,
		accountID, operatorID).Scan(&isDefault, &status, &isOwner)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotAccountOwner // operator not on this account at all
	}
	if err != nil {
		return fmt.Errorf("verify close authority: %w", err)
	}
	if isDefault {
		return ErrCannotCloseDefault
	}
	if !isOwner {
		return ErrNotAccountOwner
	}
	if status != "closed" {
		if _, err := tx.Exec(ctx,
			`UPDATE accounts SET status = 'closed', closed_at = now(), closed_reason = $2
			 WHERE id = $1`, accountID, reason); err != nil {
			return fmt.Errorf("close account: %w", err)
		}
	}
	// Every live credential dies with the account — operator, agent, and any
	// unclaimed bootstrap tokens alike. This sweep runs even when the account
	// is already closed (only the tombstone write above is skipped): a token
	// mint racing the original close can commit after that close's sweep, and
	// re-closing must be able to kill the straggler.
	if _, err := tx.Exec(ctx,
		`UPDATE tokens SET consumed_at = now()
		 WHERE account_id = $1 AND consumed_at IS NULL`, accountID); err != nil {
		return fmt.Errorf("revoke account tokens: %w", err)
	}
	return tx.Commit(ctx)
}
