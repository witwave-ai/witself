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
	// ErrAgentNotFound is returned when the agent does not exist in the account.
	ErrAgentNotFound = errors.New("agent not found")
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
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Agent{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	// The plan-gate lock subsumes the mint lock's status check and also
	// serializes concurrent creates, so the count below cannot race past the
	// account's agent cap (account-wide across realms).
	plan, limits, err := lockAccountForPlanGate(ctx, tx, accountID)
	if err != nil {
		return Agent{}, err
	}
	// Resolve the realm BEFORE the plan gate: a bad realm id must stay a 404
	// even when the account sits at its agent cap — a plan-limit refusal
	// there would misdiagnose the failure and steer the caller toward an
	// upgrade that cannot fix the request.
	var realmOK bool
	if err := tx.QueryRow(ctx,
		`SELECT EXISTS (
		   SELECT 1 FROM realms
		   WHERE id = $1 AND account_id = $2 AND deleted_at IS NULL
		 )`, realmID, accountID).Scan(&realmOK); err != nil {
		return Agent{}, fmt.Errorf("check realm: %w", err)
	}
	if !realmOK {
		return Agent{}, ErrRealmNotFound
	}
	if _, capped := limits["agents"]; capped {
		n, err := countLiveAgents(ctx, tx, accountID)
		if err != nil {
			return Agent{}, err
		}
		if err := checkPlanLimit(plan, limits, "agents", n); err != nil {
			return Agent{}, err
		}
	}
	var returned string
	err = tx.QueryRow(ctx,
		`INSERT INTO agents (id, realm_id, name)
		 SELECT $1, $2, $3
		 WHERE EXISTS (
		   SELECT 1 FROM realms
		   WHERE id = $2 AND account_id = $4 AND deleted_at IS NULL
		 )
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
	if err := tx.Commit(ctx); err != nil {
		return Agent{}, err
	}
	return Agent{ID: agentID, Name: name}, nil
}

// ListAgents returns the realm's agents (account-scoped), oldest first.
func (s *Store) ListAgents(ctx context.Context, accountID, realmID string) ([]Agent, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT a.id, a.name FROM agents a
		 JOIN realms r ON r.id = a.realm_id
		 WHERE a.realm_id = $1 AND r.account_id = $2
		   AND r.deleted_at IS NULL AND a.deleted_at IS NULL
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

// DeleteAgent soft-deletes an agent in a realm and immediately revokes its live
// tokens. The agent row stays as a tombstone for future audit/history surfaces.
func (s *Store) DeleteAgent(ctx context.Context, accountID, realmID, agentID string) error {
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
		`SELECT true FROM agents a
		 JOIN realms r ON r.id = a.realm_id
		 WHERE a.id = $1 AND a.realm_id = $2 AND r.account_id = $3
		   AND a.deleted_at IS NULL AND r.deleted_at IS NULL
		 FOR NO KEY UPDATE`,
		agentID, realmID, accountID).Scan(&exists)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrAgentNotFound
	}
	if err != nil {
		return fmt.Errorf("verify agent: %w", err)
	}
	if err := cancelMessageRequestsForDeletedAgentTx(ctx, tx, accountID, realmID, agentID); err != nil {
		return err
	}

	if _, err := tx.Exec(ctx,
		`UPDATE tokens SET consumed_at = now()
		 WHERE account_id = $1 AND agent_id = $2 AND consumed_at IS NULL`,
		accountID, agentID); err != nil {
		return fmt.Errorf("revoke agent tokens: %w", err)
	}
	if _, err := tx.Exec(ctx,
		`UPDATE agents SET deleted_at = now(), updated_at = now()
		 WHERE id = $1 AND realm_id = $2 AND deleted_at IS NULL`,
		agentID, realmID); err != nil {
		return fmt.Errorf("delete agent: %w", err)
	}
	return tx.Commit(ctx)
}
