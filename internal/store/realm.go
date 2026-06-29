package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/witwave-ai/witself/internal/id"
)

// ErrRealmExists is returned when a realm name already exists in the account.
var ErrRealmExists = errors.New("realm already exists")

// Realm is a realm row (id + name).
type Realm struct {
	ID   string
	Name string
}

// CreateRealm creates a realm in the account and returns it. A duplicate name
// in the same account returns ErrRealmExists.
func (s *Store) CreateRealm(ctx context.Context, accountID, name string) (Realm, error) {
	realmID, err := id.New("realm")
	if err != nil {
		return Realm{}, err
	}
	_, err = s.pool.Exec(ctx,
		`INSERT INTO realms (id, account_id, name) VALUES ($1, $2, $3)`,
		realmID, accountID, name)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" { // unique_violation
			return Realm{}, ErrRealmExists
		}
		return Realm{}, fmt.Errorf("create realm: %w", err)
	}
	return Realm{ID: realmID, Name: name}, nil
}

// ListRealms returns the account's realms, oldest first.
func (s *Store) ListRealms(ctx context.Context, accountID string) ([]Realm, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT id, name FROM realms WHERE account_id = $1 ORDER BY created_at`, accountID)
	if err != nil {
		return nil, fmt.Errorf("list realms: %w", err)
	}
	defer rows.Close()

	var out []Realm
	for rows.Next() {
		var r Realm
		if err := rows.Scan(&r.ID, &r.Name); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
