package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/witwave-ai/witself/internal/id"
	tokpkg "github.com/witwave-ai/witself/internal/token"
)

const defaultOperatorRole = "account_operator"

var (
	// ErrCannotDeleteSelf is returned when an operator tries to delete itself.
	ErrCannotDeleteSelf = errors.New("cannot delete the authenticated operator")
	// ErrCannotDeleteRootOperator is returned when deleting the seeded root operator.
	ErrCannotDeleteRootOperator = errors.New("cannot delete the root operator")
	// ErrLastOperator is returned when deleting an operator would leave no live operators.
	ErrLastOperator = errors.New("cannot delete the last operator")
)

// OperatorToken is safe token metadata for an operator. It never contains the
// raw token value or token hash.
type OperatorToken struct {
	ID          string
	DisplayName string
	CreatedAt   time.Time
	ExpiresAt   *time.Time
}

// Operator is an operator row plus safe live token metadata.
type Operator struct {
	ID          string
	DisplayName string
	Role        string
	IsRoot      bool
	CreatedAt   time.Time
	UpdatedAt   time.Time
	Tokens      []OperatorToken
}

// ListOperators returns live operators for an account, including safe metadata
// for their live operator tokens.
func (s *Store) ListOperators(ctx context.Context, accountID string) ([]Operator, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT id, display_name, role, is_root, created_at, updated_at
		 FROM operators
		 WHERE account_id = $1 AND deleted_at IS NULL
		 ORDER BY created_at`, accountID)
	if err != nil {
		return nil, fmt.Errorf("list operators: %w", err)
	}
	defer rows.Close()

	var out []Operator
	byID := map[string]int{}
	for rows.Next() {
		var op Operator
		if err := rows.Scan(&op.ID, &op.DisplayName, &op.Role, &op.IsRoot, &op.CreatedAt, &op.UpdatedAt); err != nil {
			return nil, err
		}
		byID[op.ID] = len(out)
		out = append(out, op)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return out, nil
	}

	tokenRows, err := s.pool.Query(ctx,
		`SELECT id, operator_id, display_name, created_at, expires_at
		 FROM tokens
		 WHERE account_id = $1 AND kind = 'operator' AND operator_id IS NOT NULL
		   AND consumed_at IS NULL
		   AND (expires_at IS NULL OR expires_at > now())
		 ORDER BY created_at`, accountID)
	if err != nil {
		return nil, fmt.Errorf("list operator tokens: %w", err)
	}
	defer tokenRows.Close()

	for tokenRows.Next() {
		var meta OperatorToken
		var operatorID string
		var expiresAt sql.NullTime
		if err := tokenRows.Scan(&meta.ID, &operatorID, &meta.DisplayName, &meta.CreatedAt, &expiresAt); err != nil {
			return nil, err
		}
		if expiresAt.Valid {
			t := expiresAt.Time
			meta.ExpiresAt = &t
		}
		if idx, ok := byID[operatorID]; ok {
			out[idx].Tokens = append(out[idx].Tokens, meta)
		}
	}
	if err := tokenRows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// CreateOperator creates a non-root operator and mints its first token. The raw
// token is returned once; only its hash is stored.
func (s *Store) CreateOperator(ctx context.Context, accountID, displayName, tokenDisplayName string, ttl *time.Duration) (Operator, string, *time.Time, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Operator{}, "", nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	operatorID, err := id.New("opr")
	if err != nil {
		return Operator{}, "", nil, err
	}
	tokenID, err := id.New("tok")
	if err != nil {
		return Operator{}, "", nil, err
	}
	rawToken, err := tokpkg.New(tokpkg.KindOperator)
	if err != nil {
		return Operator{}, "", nil, err
	}

	op := Operator{
		ID:          operatorID,
		DisplayName: displayName,
		Role:        defaultOperatorRole,
	}
	err = tx.QueryRow(ctx,
		`INSERT INTO operators (id, account_id, role, is_root, display_name)
		 VALUES ($1, $2, $3, false, $4)
		 RETURNING created_at, updated_at`,
		operatorID, accountID, defaultOperatorRole, displayName).Scan(&op.CreatedAt, &op.UpdatedAt)
	if err != nil {
		return Operator{}, "", nil, fmt.Errorf("create operator: %w", err)
	}

	var expiresAt *time.Time
	var expiresValue any
	if ttl != nil {
		t := time.Now().UTC().Add(*ttl)
		expiresAt = &t
		expiresValue = t
	}

	var tokenCreatedAt time.Time
	err = tx.QueryRow(ctx,
		`INSERT INTO tokens (id, account_id, operator_id, kind, token_hash, expires_at, display_name)
		 VALUES ($1, $2, $3, 'operator', $4, $5, $6)
		 RETURNING created_at`,
		tokenID, accountID, operatorID, hashToken(rawToken), expiresValue, tokenDisplayName).Scan(&tokenCreatedAt)
	if err != nil {
		return Operator{}, "", nil, fmt.Errorf("store operator token: %w", err)
	}

	op.Tokens = []OperatorToken{{
		ID:          tokenID,
		DisplayName: tokenDisplayName,
		CreatedAt:   tokenCreatedAt,
		ExpiresAt:   expiresAt,
	}}

	if err := tx.Commit(ctx); err != nil {
		return Operator{}, "", nil, err
	}
	return op, rawToken, expiresAt, nil
}

// DeleteOperator soft-deletes a non-root operator and revokes all live tokens
// bound to it. The row stays as a tombstone for audit/history.
func (s *Store) DeleteOperator(ctx context.Context, accountID, actorOperatorID, targetOperatorID string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var isRoot bool
	err = tx.QueryRow(ctx,
		`SELECT is_root FROM operators
		 WHERE account_id = $1 AND id = $2 AND deleted_at IS NULL
		 FOR UPDATE`,
		accountID, targetOperatorID).Scan(&isRoot)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrOperatorNotFound
	}
	if err != nil {
		return fmt.Errorf("verify operator: %w", err)
	}
	if targetOperatorID == actorOperatorID {
		return ErrCannotDeleteSelf
	}
	if isRoot {
		return ErrCannotDeleteRootOperator
	}

	var liveOperators int
	if err := tx.QueryRow(ctx,
		`SELECT count(*) FROM operators
		 WHERE account_id = $1 AND deleted_at IS NULL`,
		accountID).Scan(&liveOperators); err != nil {
		return fmt.Errorf("count operators: %w", err)
	}
	if liveOperators <= 1 {
		return ErrLastOperator
	}

	if _, err := tx.Exec(ctx,
		`UPDATE tokens SET consumed_at = now()
		 WHERE account_id = $1 AND operator_id = $2 AND consumed_at IS NULL`,
		accountID, targetOperatorID); err != nil {
		return fmt.Errorf("revoke operator tokens: %w", err)
	}
	if _, err := tx.Exec(ctx,
		`UPDATE operators SET deleted_at = now(), updated_at = now()
		 WHERE account_id = $1 AND id = $2 AND deleted_at IS NULL`,
		accountID, targetOperatorID); err != nil {
		return fmt.Errorf("delete operator: %w", err)
	}
	return tx.Commit(ctx)
}
