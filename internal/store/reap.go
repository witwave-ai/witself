package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// ErrAccountActive is returned by ReapPendingAccount when the account has
// activated — reaping only ever takes never-activated accounts.
var ErrAccountActive = errors.New("account is active")

// ReapPendingAccount closes an account only if it is still pending: the
// control plane's expiry sweep calls this, and the status='pending' guard is
// what makes that sweep's view safely stale — the cell is truth, so an
// account that activated moments before the sweep cannot be reaped. An
// already-closed account returns reaped=false with no error (idempotent).
// The tombstone and token sweep match an owner-initiated close exactly.
func (s *Store) ReapPendingAccount(ctx context.Context, accountID, reason string) (reaped bool, err error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var status string
	var isDefault bool
	err = tx.QueryRow(ctx,
		`SELECT status, is_default FROM accounts WHERE id = $1 FOR UPDATE`,
		accountID).Scan(&status, &isDefault)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, ErrAccountNotFound
	}
	if err != nil {
		return false, fmt.Errorf("verify reap target: %w", err)
	}
	if status == "closed" {
		// Idempotent — but still re-run the token sweep, exactly like a
		// re-close: a mint that raced the original close must stay killable.
		if _, err := tx.Exec(ctx,
			`UPDATE tokens SET consumed_at = now()
			 WHERE account_id = $1 AND consumed_at IS NULL`, accountID); err != nil {
			return false, fmt.Errorf("revoke reaped account tokens: %w", err)
		}
		if err := supersedeOpenAvatarStyleRolloutsForAccountTx(ctx, tx, accountID, "account_reaped"); err != nil {
			return false, err
		}
		return false, tx.Commit(ctx)
	}
	// Strict allowlist: an automated destroyer fails closed. Only 'pending'
	// may be reaped — 'active', the default account, and any FUTURE status
	// (suspended, verifying, delinquent, ...) are refused, not swept up.
	if isDefault || status != "pending" {
		return false, ErrAccountActive
	}

	if _, err := tx.Exec(ctx,
		`UPDATE accounts SET status = 'closed', closed_at = now(), closed_reason = $2
		 WHERE id = $1`, accountID, reason); err != nil {
		return false, fmt.Errorf("reap account: %w", err)
	}
	if _, err := tx.Exec(ctx,
		`UPDATE tokens SET consumed_at = now()
		 WHERE account_id = $1 AND consumed_at IS NULL`, accountID); err != nil {
		return false, fmt.Errorf("revoke reaped account tokens: %w", err)
	}
	if err := supersedeOpenAvatarStyleRolloutsForAccountTx(ctx, tx, accountID, "account_reaped"); err != nil {
		return false, err
	}
	// Only fired on a genuine pending→closed transition — the
	// already-closed branch above returned without touching state.
	eventMeta := map[string]any{}
	if reason != "" {
		eventMeta["reason"] = reason
	}
	if err := logEventTx(ctx, tx, EventInput{
		AccountID: accountID, ActorKind: ActorControlPlane,
		Verb: VerbAccountReaped, Metadata: eventMeta,
	}); err != nil {
		return false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return false, err
	}
	return true, nil
}
