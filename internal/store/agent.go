package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/witwave-ai/witself/internal/id"
)

var (
	// ErrAgentExists is returned when an agent name already exists in the realm.
	ErrAgentExists = errors.New("agent already exists")
	// ErrRealmNotFound is returned when the realm does not exist in the account.
	ErrRealmNotFound = errors.New("realm not found")
)

// Agent is an agent row (id + name).
type Agent struct {
	ID   string
	Name string
}

// CreateAgent creates an agent in a realm that belongs to the account. It
// returns ErrRealmNotFound if the realm is not in the account, or
// ErrAgentExists if the name is taken in that realm.
func (s *Store) CreateAgent(ctx context.Context, accountID, realmID, name string) (Agent, error) {
	agentID, err := id.New("agent")
	if err != nil {
		return Agent{}, err
	}
	var returned string
	err = s.pool.QueryRow(ctx,
		`INSERT INTO agents (id, realm_id, name)
		 SELECT $1, $2, $3
		 WHERE EXISTS (SELECT 1 FROM realms WHERE id = $2 AND account_id = $4)
		 RETURNING id`,
		agentID, realmID, name, accountID).Scan(&returned)
	if errors.Is(err, pgx.ErrNoRows) {
		return Agent{}, ErrRealmNotFound
	}
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" { // unique_violation
			return Agent{}, ErrAgentExists
		}
		return Agent{}, fmt.Errorf("create agent: %w", err)
	}
	return Agent{ID: agentID, Name: name}, nil
}

// ListAgents returns the realm's agents (account-scoped), oldest first.
func (s *Store) ListAgents(ctx context.Context, accountID, realmID string) ([]Agent, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT a.id, a.name FROM agents a
		 JOIN realms r ON r.id = a.realm_id
		 WHERE a.realm_id = $1 AND r.account_id = $2
		 ORDER BY a.created_at`, realmID, accountID)
	if err != nil {
		return nil, fmt.Errorf("list agents: %w", err)
	}
	defer rows.Close()

	var out []Agent
	for rows.Next() {
		var a Agent
		if err := rows.Scan(&a.ID, &a.Name); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}
