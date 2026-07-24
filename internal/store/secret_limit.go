package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/witwave-ai/witself/internal/plans"
)

// GetSecretLimitStatus returns the current retained-secret capacity for the
// authenticated owner. It never returns secret identifiers or metadata.
func (s *Store) GetSecretLimitStatus(ctx context.Context, p Principal) (SecretLimitStatus, error) {
	if err := requireSelfSecretPrincipal(p); err != nil {
		return SecretLimitStatus{}, err
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{
		IsoLevel:   pgx.RepeatableRead,
		AccessMode: pgx.ReadOnly,
	})
	if err != nil {
		return SecretLimitStatus{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := requireActiveSecretAccountTx(ctx, tx, p.AccountID); err != nil {
		return SecretLimitStatus{}, err
	}
	status, err := secretLimitStatusTx(ctx, tx, p)
	if err != nil {
		return SecretLimitStatus{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return SecretLimitStatus{}, err
	}
	return status, nil
}

func requireActiveSecretAccountTx(ctx context.Context, tx pgx.Tx, accountID string) error {
	var status string
	err := tx.QueryRow(ctx, `SELECT status FROM accounts WHERE id=$1`, accountID).Scan(&status)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrAccountNotFound
	}
	if err != nil {
		return fmt.Errorf("read secret account: %w", err)
	}
	if status != "active" {
		return ErrAccountNotActive
	}
	return nil
}

func secretLimitStatusTx(ctx context.Context, tx pgx.Tx, p Principal) (SecretLimitStatus, error) {
	var limitsJSON []byte
	if err := tx.QueryRow(ctx, `SELECT plan_limits FROM accounts WHERE id=$1`,
		p.AccountID).Scan(&limitsJSON); err != nil {
		return SecretLimitStatus{}, fmt.Errorf("read secret plan limits: %w", err)
	}
	var limits map[string]int64
	if err := json.Unmarshal(limitsJSON, &limits); err != nil {
		return SecretLimitStatus{}, fmt.Errorf("decode secret plan limits: %w", err)
	}
	var used int64
	if err := tx.QueryRow(ctx, `
		SELECT count(*)
		  FROM secrets
		 WHERE account_id=$1 AND realm_id=$2 AND owner_agent_id=$3
		   AND deleted_at IS NULL`,
		p.AccountID, p.RealmID, p.ID).Scan(&used); err != nil {
		return SecretLimitStatus{}, fmt.Errorf("count retained secrets: %w", err)
	}
	limit, capped := limits[plans.StoredSecretLimit]
	if !capped {
		return SecretLimitStatus{Used: used, Unlimited: true}, nil
	}
	remaining := limit - used
	if remaining < 0 {
		remaining = 0
	}
	maximum := limit
	return SecretLimitStatus{
		Used: used, Max: &maximum, Remaining: &remaining,
		OverLimit: used > limit,
	}, nil
}
