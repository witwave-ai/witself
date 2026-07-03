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

// ErrTokenNotFound is returned when a token does not exist in the account, is
// already revoked, or is not a revocable credential token.
var ErrTokenNotFound = errors.New("token not found")

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
	defer func() { _ = tx.Rollback(ctx) }()

	// Resolve the token first WITHOUT locking it, then lock the account, then
	// consume. Lock order is accounts-before-tokens everywhere (mints, close,
	// reap) — consuming the token row first would invert it and deadlock
	// against a reap/close sweeping this very account.
	var tokID, accountID, operatorID string
	err = tx.QueryRow(ctx,
		`SELECT t.id, t.account_id, t.operator_id FROM tokens t
		 JOIN operators o ON o.id = t.operator_id AND o.account_id = t.account_id
		 WHERE t.token_hash = $1 AND t.kind = 'bootstrap' AND t.consumed_at IS NULL
		   AND (t.expires_at IS NULL OR t.expires_at > now())
		   AND o.deleted_at IS NULL`, hashToken(plaintext)).Scan(&tokID, &accountID, &operatorID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", "", ErrInvalidBootstrap
	}
	if err != nil {
		return "", "", fmt.Errorf("resolve bootstrap token: %w", err)
	}
	// Pending is allowed — claiming the credential is how a new owner watches
	// for activation. A closed account's bootstrap is simply dead.
	if err := lockAccountForMint(ctx, tx, accountID, true); err != nil {
		if errors.Is(err, ErrAccountNotActive) || errors.Is(err, ErrAccountNotFound) {
			return "", "", ErrInvalidBootstrap
		}
		return "", "", err
	}
	// Single-use, atomically: a concurrent exchange (or a close/reap sweep
	// that fired between the SELECT and the account lock) wins here, and this
	// caller gets the same answer as any spent token.
	tag, err := tx.Exec(ctx,
		`UPDATE tokens SET consumed_at = now()
		 WHERE id = $1 AND consumed_at IS NULL
		   AND (expires_at IS NULL OR expires_at > now())`, tokID)
	if err != nil {
		return "", "", fmt.Errorf("consume bootstrap token: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return "", "", ErrInvalidBootstrap
	}

	opTok, err := token.New(token.KindOperator)
	if err != nil {
		return "", "", err
	}
	opTokID, err := id.New("tok")
	if err != nil {
		return "", "", err
	}
	// The exchanged token always belongs to the root operator (signup, seed,
	// and recovery bootstraps are all root-bound), so it is born named after
	// it — no more nameless first tokens.
	if _, err := tx.Exec(ctx,
		`INSERT INTO tokens (id, account_id, operator_id, kind, token_hash, display_name)
		 VALUES ($1, $2, $3, 'operator', $4, 'owner')`,
		opTokID, accountID, operatorID, hashToken(opTok)); err != nil {
		return "", "", fmt.Errorf("store operator token: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return "", "", err
	}
	return opTok, operatorID, nil
}

// AuthenticateOperator resolves an operator bearer token to its principal,
// including the account's lifecycle status — status is part of the principal,
// so callers can gate actions on it without a second lookup. ok is false when
// the token is not a live operator token. (Revocation later sets consumed_at,
// which excludes the token here.)
func (s *Store) AuthenticateOperator(ctx context.Context, plaintext string) (operatorID, accountID, accountStatus string, ok bool, err error) {
	err = s.pool.QueryRow(ctx,
		`SELECT t.operator_id, t.account_id, a.status FROM tokens t
		 JOIN operators o ON o.id = t.operator_id AND o.account_id = t.account_id
		 JOIN accounts a ON a.id = t.account_id
		 WHERE t.token_hash = $1 AND t.kind = 'operator' AND t.consumed_at IS NULL
		   AND (t.expires_at IS NULL OR t.expires_at > now())
		   AND o.deleted_at IS NULL`,
		hashToken(plaintext)).Scan(&operatorID, &accountID, &accountStatus)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", "", "", false, nil
	}
	if err != nil {
		return "", "", "", false, fmt.Errorf("authenticate: %w", err)
	}
	return operatorID, accountID, accountStatus, true, nil
}

// CreateOperatorToken mints a durable operator token bound to an operator that
// belongs to the account, and returns the plaintext (shown once). Expiration is
// optional; nil ttl means no explicit expiry.
func (s *Store) CreateOperatorToken(ctx context.Context, accountID, operatorID, displayName string, ttl *time.Duration) (string, string, *time.Time, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return "", "", nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if err := lockAccountForMint(ctx, tx, accountID, false); err != nil {
		return "", "", nil, err
	}
	var exists bool
	err = tx.QueryRow(ctx,
		`SELECT true FROM operators
		 WHERE id = $1 AND account_id = $2 AND deleted_at IS NULL`,
		operatorID, accountID).Scan(&exists)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", "", nil, ErrOperatorNotFound
	}
	if err != nil {
		return "", "", nil, fmt.Errorf("verify operator: %w", err)
	}

	opTok, err := token.New(token.KindOperator)
	if err != nil {
		return "", "", nil, err
	}
	tokID, err := id.New("tok")
	if err != nil {
		return "", "", nil, err
	}

	var expiresAt *time.Time
	var expiresValue any
	if ttl != nil {
		t := time.Now().UTC().Add(*ttl)
		expiresAt = &t
		expiresValue = t
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO tokens (id, account_id, operator_id, kind, token_hash, expires_at, display_name)
		 VALUES ($1, $2, $3, 'operator', $4, $5, $6)`,
		tokID, accountID, operatorID, hashToken(opTok), expiresValue, displayName); err != nil {
		return "", "", nil, fmt.Errorf("store operator token: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return "", "", nil, err
	}
	return opTok, tokID, expiresAt, nil
}

// CreateAgentToken mints a durable agent token bound to an agent that belongs to
// the account, and returns the plaintext (shown once). ErrAgentNotFound if the
// agent is not in the account.
func (s *Store) CreateAgentToken(ctx context.Context, accountID, agentID string) (tok, tokenID, agentName string, err error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return "", "", "", err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if err := lockAccountForMint(ctx, tx, accountID, false); err != nil {
		return "", "", "", err
	}
	err = tx.QueryRow(ctx,
		`SELECT a.name FROM agents a
		 JOIN realms r ON r.id = a.realm_id
		 WHERE a.id = $1 AND r.account_id = $2
		   AND a.deleted_at IS NULL AND r.deleted_at IS NULL`, agentID, accountID).Scan(&agentName)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", "", "", ErrAgentNotFound
	}
	if err != nil {
		return "", "", "", fmt.Errorf("verify agent: %w", err)
	}

	agtTok, err := token.New(token.KindAgent)
	if err != nil {
		return "", "", "", err
	}
	tokID, err := id.New("tok")
	if err != nil {
		return "", "", "", err
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO tokens (id, account_id, agent_id, kind, token_hash)
		 VALUES ($1, $2, $3, 'agent', $4)`,
		tokID, accountID, agentID, hashToken(agtTok)); err != nil {
		return "", "", "", fmt.Errorf("store agent token: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return "", "", "", err
	}
	return agtTok, tokID, agentName, nil
}

// RevokeToken immediately invalidates a live operator or agent token in the
// account. Bootstrap token consumption remains part of ExchangeBootstrap.
func (s *Store) RevokeToken(ctx context.Context, accountID, tokenID string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	// Allowed during suspension: revoking a leaked token is safety-preserving,
	// exactly the class of write the freeze must not block.
	if err := lockAccountForSafetyWrite(ctx, tx, accountID); err != nil {
		return err
	}
	tag, err := tx.Exec(ctx,
		`UPDATE tokens SET consumed_at = now()
		 WHERE account_id = $1 AND id = $2
		   AND kind IN ('operator', 'agent')
		   AND consumed_at IS NULL
		   AND (expires_at IS NULL OR expires_at > now())`,
		accountID, tokenID)
	if err != nil {
		return fmt.Errorf("revoke token: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrTokenNotFound
	}
	return tx.Commit(ctx)
}
