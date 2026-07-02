package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/witwave-ai/witself/internal/id"
	"github.com/witwave-ai/witself/internal/token"
)

// ErrInvalidBootstrap is returned when a presented bootstrap token does not
// match an unconsumed bootstrap record.
var ErrInvalidBootstrap = errors.New("invalid or already-used bootstrap token")

// ErrOperatorNotFound is returned when an operator is not in the account.
var ErrOperatorNotFound = errors.New("operator not found")

func hashToken(plaintext string) string {
	sum := sha256.Sum256([]byte(plaintext))
	return hex.EncodeToString(sum[:])
}

// AdoptBootstrapToken records the hash of a bootstrap token bound to the root
// operator, so it can later be exchanged. Idempotent: re-adopting the same token
// is a no-op, so a restart does not extend the token's original expiry.
func (s *Store) AdoptBootstrapToken(ctx context.Context, accountID, operatorID, plaintext string, ttl time.Duration) error {
	tokID, err := id.New("tok")
	if err != nil {
		return err
	}
	expiresAt := time.Now().UTC().Add(ttl)
	_, err = s.pool.Exec(ctx,
		`INSERT INTO tokens (id, account_id, operator_id, kind, token_hash, expires_at)
		 VALUES ($1, $2, $3, 'bootstrap', $4, $5)
		 ON CONFLICT (token_hash) DO NOTHING`,
		tokID, accountID, operatorID, hashToken(plaintext), expiresAt)
	if err != nil {
		return fmt.Errorf("adopt bootstrap token: %w", err)
	}
	return nil
}

// ExchangeBootstrap consumes a valid bootstrap token (single-use, atomically)
// and mints a durable operator token bound to the same operator. It returns the
// operator token plaintext (shown once) and the operator id, or
// ErrInvalidBootstrap if the token is unknown or already used.
func (s *Store) ExchangeBootstrap(ctx context.Context, plaintext string) (string, string, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return "", "", err
	}
	defer tx.Rollback(ctx)

	var accountID, operatorID string
	err = tx.QueryRow(ctx,
		`UPDATE tokens SET consumed_at = now()
		 WHERE token_hash = $1 AND kind = 'bootstrap' AND consumed_at IS NULL
		   AND (expires_at IS NULL OR expires_at > now())
		 RETURNING account_id, operator_id`, hashToken(plaintext)).Scan(&accountID, &operatorID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", "", ErrInvalidBootstrap
	}
	if err != nil {
		return "", "", fmt.Errorf("consume bootstrap token: %w", err)
	}

	opTok, err := token.New(token.KindOperator)
	if err != nil {
		return "", "", err
	}
	tokID, err := id.New("tok")
	if err != nil {
		return "", "", err
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO tokens (id, account_id, operator_id, kind, token_hash)
		 VALUES ($1, $2, $3, 'operator', $4)`,
		tokID, accountID, operatorID, hashToken(opTok)); err != nil {
		return "", "", fmt.Errorf("store operator token: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return "", "", err
	}
	return opTok, operatorID, nil
}

// AuthenticateOperator resolves an operator bearer token to its principal.
// ok is false when the token is not a live operator token. (Revocation later
// sets consumed_at, which excludes the token here.)
func (s *Store) AuthenticateOperator(ctx context.Context, plaintext string) (operatorID, accountID string, ok bool, err error) {
	err = s.pool.QueryRow(ctx,
		`SELECT operator_id, account_id FROM tokens
		 WHERE token_hash = $1 AND kind = 'operator' AND consumed_at IS NULL
		   AND (expires_at IS NULL OR expires_at > now())`,
		hashToken(plaintext)).Scan(&operatorID, &accountID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", "", false, nil
	}
	if err != nil {
		return "", "", false, fmt.Errorf("authenticate: %w", err)
	}
	return operatorID, accountID, true, nil
}

// CreateOperatorToken mints a durable operator token bound to an operator that
// belongs to the account, and returns the plaintext (shown once). Expiration is
// optional; nil ttl means no explicit expiry.
func (s *Store) CreateOperatorToken(ctx context.Context, accountID, operatorID, displayName string, ttl *time.Duration) (string, *time.Time, error) {
	var exists bool
	err := s.pool.QueryRow(ctx,
		`SELECT true FROM operators WHERE id = $1 AND account_id = $2`,
		operatorID, accountID).Scan(&exists)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil, ErrOperatorNotFound
	}
	if err != nil {
		return "", nil, fmt.Errorf("verify operator: %w", err)
	}

	opTok, err := token.New(token.KindOperator)
	if err != nil {
		return "", nil, err
	}
	tokID, err := id.New("tok")
	if err != nil {
		return "", nil, err
	}

	var expiresAt *time.Time
	var expiresValue any
	if ttl != nil {
		t := time.Now().UTC().Add(*ttl)
		expiresAt = &t
		expiresValue = t
	}
	if _, err := s.pool.Exec(ctx,
		`INSERT INTO tokens (id, account_id, operator_id, kind, token_hash, expires_at, display_name)
		 VALUES ($1, $2, $3, 'operator', $4, $5, $6)`,
		tokID, accountID, operatorID, hashToken(opTok), expiresValue, displayName); err != nil {
		return "", nil, fmt.Errorf("store operator token: %w", err)
	}
	return opTok, expiresAt, nil
}

// CreateAgentToken mints a durable agent token bound to an agent that belongs to
// the account, and returns the plaintext (shown once). ErrAgentNotFound if the
// agent is not in the account.
func (s *Store) CreateAgentToken(ctx context.Context, accountID, agentID string) (string, error) {
	var realmID string
	err := s.pool.QueryRow(ctx,
		`SELECT a.realm_id FROM agents a
		 JOIN realms r ON r.id = a.realm_id
		 WHERE a.id = $1 AND r.account_id = $2`, agentID, accountID).Scan(&realmID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrAgentNotFound
	}
	if err != nil {
		return "", fmt.Errorf("verify agent: %w", err)
	}

	agtTok, err := token.New(token.KindAgent)
	if err != nil {
		return "", err
	}
	tokID, err := id.New("tok")
	if err != nil {
		return "", err
	}
	if _, err := s.pool.Exec(ctx,
		`INSERT INTO tokens (id, account_id, agent_id, kind, token_hash)
		 VALUES ($1, $2, $3, 'agent', $4)`,
		tokID, accountID, agentID, hashToken(agtTok)); err != nil {
		return "", fmt.Errorf("store agent token: %w", err)
	}
	return agtTok, nil
}
