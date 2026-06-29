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
