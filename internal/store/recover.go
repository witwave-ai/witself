package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/witwave-ai/witself/internal/id"
	"github.com/witwave-ai/witself/internal/token"
)

// RecoverAccount rotates the root operator's credentials after the control
// plane has verified inbox control: every live root token dies (lost is
// indistinguishable from stolen) and a fresh short-lived bootstrap token is
// minted for the root operator — the ordinary claim exchange finishes the
// job. Only ACTIVE accounts recover: a pending account's path is the reaper
// and a fresh signup; a closed account is a tombstone. Agents and other
// operators are untouched.
func (s *Store) RecoverAccount(ctx context.Context, accountID string, bootstrapTTL time.Duration) (ProvisionedAccount, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return ProvisionedAccount{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var status string
	var email *string
	err = tx.QueryRow(ctx,
		`SELECT status, email FROM accounts WHERE id = $1 FOR UPDATE`,
		accountID).Scan(&status, &email)
	if errors.Is(err, pgx.ErrNoRows) {
		return ProvisionedAccount{}, ErrAccountNotFound
	}
	if err != nil {
		return ProvisionedAccount{}, fmt.Errorf("verify recovery target: %w", err)
	}
	// Suspended is deliberately allowed: recovery is CREDENTIAL recovery, not
	// account activation. Rotating the root token preserves the suspended
	// status, so the recovered owner can only inspect/resume/close — the
	// minimum path back. Pending and closed remain refused: a pending
	// account has no root credential to rotate yet, and a closed one is a
	// tombstone.
	if status != "active" && status != "suspended" {
		return ProvisionedAccount{}, ErrAccountNotActive
	}

	var rootID string
	err = tx.QueryRow(ctx,
		`SELECT id FROM operators WHERE account_id = $1 AND is_root AND deleted_at IS NULL`,
		accountID).Scan(&rootID)
	if errors.Is(err, pgx.ErrNoRows) {
		return ProvisionedAccount{}, fmt.Errorf("account %s has no root operator", accountID)
	}
	if err != nil {
		return ProvisionedAccount{}, fmt.Errorf("find root operator: %w", err)
	}

	// The rotation: every live credential of the ROOT operator dies —
	// operator tokens and any outstanding recovery bootstraps alike, so only
	// the newest recovery code's exchange can succeed.
	if _, err := tx.Exec(ctx,
		`UPDATE tokens SET consumed_at = now()
		 WHERE account_id = $1 AND operator_id = $2 AND consumed_at IS NULL`,
		accountID, rootID); err != nil {
		return ProvisionedAccount{}, fmt.Errorf("revoke root tokens: %w", err)
	}

	bootTok, err := token.New(token.KindBootstrap)
	if err != nil {
		return ProvisionedAccount{}, err
	}
	tokID, err := id.New("tok")
	if err != nil {
		return ProvisionedAccount{}, err
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO tokens (id, account_id, operator_id, kind, token_hash, expires_at, display_name)
		 VALUES ($1, $2, $3, 'bootstrap', $4, $5, 'recovery')`,
		tokID, accountID, rootID, hashToken(bootTok), time.Now().UTC().Add(bootstrapTTL)); err != nil {
		return ProvisionedAccount{}, fmt.Errorf("mint recovery bootstrap: %w", err)
	}
	// The root-token rotation IS the audit event. Every prior root
	// credential died and a fresh bootstrap was minted; the operator id
	// stays the same (recovery is credential rotation, not identity
	// replacement) but the audit trail records the new bootstrap's
	// operator_id so the owner can trace the recovery back.
	if err := logEventTx(ctx, tx, EventInput{
		AccountID: accountID, ActorKind: ActorControlPlane,
		Verb:     VerbRecoveryCompleted,
		Metadata: map[string]any{"new_operator_id": rootID},
	}); err != nil {
		return ProvisionedAccount{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return ProvisionedAccount{}, err
	}

	out := ProvisionedAccount{
		AccountID:      accountID,
		OperatorID:     rootID,
		Status:         status,
		BootstrapToken: bootTok,
	}
	if email != nil {
		out.Email = *email
	}
	return out, nil
}
