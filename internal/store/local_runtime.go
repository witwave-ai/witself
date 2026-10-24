package store

import (
	"context"
	"errors"
)

// AppliedSchemaVersion reads Goose's applied migration coordinate without
// changing migration state. A later rollback entry overrides an earlier apply.
func (s *Store) AppliedSchemaVersion(ctx context.Context) (int, error) {
	var version int
	err := s.pool.QueryRow(ctx, `
		SELECT coalesce(max(version_id), 0)
		FROM (
			SELECT DISTINCT ON (version_id) version_id, is_applied
			FROM goose_db_version ORDER BY version_id, id DESC
		) AS latest_version WHERE is_applied`).Scan(&version)
	if err != nil {
		return 0, errors.New("read applied schema version failed")
	}
	return version, nil
}

// LocalBootstrapAccount reads the active default account without seeding or
// reviving it. Readiness must stop succeeding if the account is removed or closed.
func (s *Store) LocalBootstrapAccount(ctx context.Context) (string, error) {
	var accountID string
	err := s.pool.QueryRow(ctx,
		`SELECT id FROM accounts WHERE is_default AND status = 'active'`).Scan(&accountID)
	if err != nil {
		return "", errors.New("local bootstrap account is unavailable")
	}
	return accountID, nil
}
