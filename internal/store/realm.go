package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/witwave-ai/witself/internal/id"
)

// ErrRealmExists is returned when a realm name already exists in the account.
var ErrRealmExists = errors.New("realm already exists")

// ErrRealmNotEmpty is returned when a realm still has live agents.
var ErrRealmNotEmpty = errors.New("realm is not empty")

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
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Realm{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	// The plan-gate lock subsumes the mint lock's status check and also
	// serializes concurrent creates, so the count below cannot race past the
	// account's realm cap.
	plan, limits, err := lockAccountForPlanGate(ctx, tx, accountID)
	if err != nil {
		return Realm{}, err
	}
	if _, capped := limits["realms"]; capped {
		n, err := countLiveRealms(ctx, tx, accountID)
		if err != nil {
			return Realm{}, err
		}
		if err := checkPlanLimit(plan, limits, "realms", n); err != nil {
			return Realm{}, err
		}
	}
	if _, err = tx.Exec(ctx,
		`INSERT INTO realms (id, account_id, name) VALUES ($1, $2, $3)`,
		realmID, accountID, name); err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" { // unique_violation
			return Realm{}, ErrRealmExists
		}
		return Realm{}, fmt.Errorf("create realm: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return Realm{}, err
	}
	return Realm{ID: realmID, Name: name}, nil
}

// ListRealms returns the account's realms, oldest first.
func (s *Store) ListRealms(ctx context.Context, accountID string) ([]Realm, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT id, name FROM realms
		 WHERE account_id = $1 AND deleted_at IS NULL
		 ORDER BY created_at`, accountID)
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

// DeleteRealm soft-deletes an empty realm in the account. Realms with live
// agents are left intact so agent identity and token revocation happen
// explicitly before the container is retired.
func (s *Store) DeleteRealm(ctx context.Context, accountID, realmID string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if err := lockAccountForMint(ctx, tx, accountID, false); err != nil {
		return err
	}
	var exists bool
	err = tx.QueryRow(ctx,
		`SELECT true FROM realms
		 WHERE id = $1 AND account_id = $2 AND deleted_at IS NULL
		 FOR UPDATE`,
		realmID, accountID).Scan(&exists)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrRealmNotFound
	}
	if err != nil {
		return fmt.Errorf("verify realm: %w", err)
	}

	var agentCount int
	if err := tx.QueryRow(ctx,
		`SELECT count(*) FROM agents
		 WHERE realm_id = $1 AND deleted_at IS NULL`,
		realmID).Scan(&agentCount); err != nil {
		return fmt.Errorf("count realm agents: %w", err)
	}
	if agentCount > 0 {
		return ErrRealmNotEmpty
	}

	if _, err := tx.Exec(ctx,
		`UPDATE realms SET deleted_at = now(), updated_at = now()
		 WHERE id = $1 AND account_id = $2 AND deleted_at IS NULL`,
		realmID, accountID); err != nil {
		return fmt.Errorf("delete realm: %w", err)
	}
	return tx.Commit(ctx)
}
