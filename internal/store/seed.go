package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/witwave-ai/witself/internal/id"
)

// EnsureDefaultAccount seeds the single default (root) account if none exists
// and returns its id. Idempotent and race-safe: the INSERT is a no-op when a
// default already exists, the partial unique index is the hard guarantee, and a
// concurrent boot's winner is read back. Local and self-managed always have
// exactly one; Cloud creates accounts on signup instead.
func (s *Store) EnsureDefaultAccount(ctx context.Context) (string, error) {
	var acctID string
	err := s.pool.QueryRow(ctx, `SELECT id FROM accounts WHERE is_default`).Scan(&acctID)
	if err == nil {
		return acctID, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return "", fmt.Errorf("query default account: %w", err)
	}

	newID, err := id.New("acc")
	if err != nil {
		return "", err
	}
	if _, err := s.pool.Exec(ctx,
		`INSERT INTO accounts (id, is_default, display_name)
		 SELECT $1, true, $2
		 WHERE NOT EXISTS (SELECT 1 FROM accounts WHERE is_default)`,
		newID, "default"); err != nil {
		return "", fmt.Errorf("seed default account: %w", err)
	}

	if err := s.pool.QueryRow(ctx, `SELECT id FROM accounts WHERE is_default`).Scan(&acctID); err != nil {
		return "", fmt.Errorf("read default account: %w", err)
	}
	return acctID, nil
}

// EnsureRootOperator seeds the single root operator (account_owner) bound to the
// account if none exists, and returns its id. Idempotent and race-safe (the
// partial unique index operators_one_root_per_account is the hard guarantee).
// This is the admin identity the bootstrap token is later exchanged for.
func (s *Store) EnsureRootOperator(ctx context.Context, accountID string) (string, error) {
	var oprID string
	err := s.pool.QueryRow(ctx,
		`SELECT id FROM operators WHERE account_id = $1 AND is_root`, accountID).Scan(&oprID)
	if err == nil {
		return oprID, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return "", fmt.Errorf("query root operator: %w", err)
	}

	newID, err := id.New("opr")
	if err != nil {
		return "", err
	}
	if _, err := s.pool.Exec(ctx,
		`INSERT INTO operators (id, account_id, role, is_root, display_name)
		 SELECT $1, $2, 'account_owner', true, 'owner'
		 WHERE NOT EXISTS (SELECT 1 FROM operators WHERE account_id = $2 AND is_root)`,
		newID, accountID); err != nil {
		return "", fmt.Errorf("seed root operator: %w", err)
	}

	if err := s.pool.QueryRow(ctx,
		`SELECT id FROM operators WHERE account_id = $1 AND is_root`, accountID).Scan(&oprID); err != nil {
		return "", fmt.Errorf("read root operator: %w", err)
	}
	return oprID, nil
}
