package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/witwave-ai/witself/internal/plans"
)

// FactLimitStatus is the authenticated owner's value-free current-fact
// capacity. It intentionally matches MemoryLimitStatus so clients can present
// every retained-resource capacity consistently.
type FactLimitStatus = MemoryLimitStatus

// FactLimitError is a machine-classifiable refusal of a net-positive
// current-fact mutation. It contains no fact identifiers or values.
type FactLimitError struct {
	Status FactLimitStatus
}

func (e *FactLimitError) Error() string {
	if e == nil || e.Status.Max == nil {
		return ErrPlanLimitReached.Error()
	}
	return fmt.Sprintf("%s: %s %d/%d", ErrPlanLimitReached,
		plans.StoredFactLimit, e.Status.Used, *e.Status.Max)
}

func (e *FactLimitError) Unwrap() error { return ErrPlanLimitReached }

// GetFactLimitStatus returns current-fact capacity for one authenticated owner
// agent. It never reads or returns fact content.
func (s *Store) GetFactLimitStatus(
	ctx context.Context,
	p Principal,
) (FactLimitStatus, error) {
	if p.Kind != PrincipalAgent || p.AccountID == "" || p.RealmID == "" || p.ID == "" {
		return FactLimitStatus{}, ErrFactForbidden
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{
		IsoLevel:   pgx.RepeatableRead,
		AccessMode: pgx.ReadOnly,
	})
	if err != nil {
		return FactLimitStatus{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	status, err := factLimitStatusTx(ctx, tx, p)
	if err != nil {
		return FactLimitStatus{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return FactLimitStatus{}, err
	}
	return status, nil
}

func factLimitStatusTx(
	ctx context.Context,
	tx pgx.Tx,
	p Principal,
) (FactLimitStatus, error) {
	var accountStatus string
	var limitsJSON []byte
	var used int64
	err := tx.QueryRow(ctx, `
		SELECT a.status, a.plan_limits, owner.active_fact_count
		  FROM accounts a
		  JOIN realms r
		    ON r.account_id=a.id AND r.id=$2 AND r.deleted_at IS NULL
		  JOIN agents owner
		    ON owner.realm_id=r.id AND owner.id=$3 AND owner.deleted_at IS NULL
		 WHERE a.id=$1`,
		p.AccountID, p.RealmID, p.ID,
	).Scan(&accountStatus, &limitsJSON, &used)
	if errors.Is(err, pgx.ErrNoRows) {
		return FactLimitStatus{}, ErrFactForbidden
	}
	if err != nil {
		return FactLimitStatus{}, fmt.Errorf("read fact capacity: %w", err)
	}
	if accountStatus != "active" {
		return FactLimitStatus{}, ErrAccountNotActive
	}
	var limits map[string]int64
	if err := json.Unmarshal(limitsJSON, &limits); err != nil {
		return FactLimitStatus{}, fmt.Errorf("decode fact plan limits: %w", err)
	}
	maximum, capped := limits[plans.StoredFactLimit]
	if !capped {
		return FactLimitStatus{Used: used, Unlimited: true}, nil
	}
	remaining := maximum - used
	if remaining < 0 {
		remaining = 0
	}
	warningAt := (maximum*9 + 9) / 10
	return FactLimitStatus{
		Used: used, Max: &maximum, Remaining: &remaining,
		NearLimit: used >= warningAt,
		AtLimit:   used == maximum,
		OverLimit: used > maximum,
	}, nil
}

// requireActiveFactCapacityTx admits unlimited and non-growing mutations. The
// caller must already hold the owner agent's exclusive namespace lock so the
// derived counter and canonical fact address cannot race another replica.
func requireActiveFactCapacityTx(
	ctx context.Context,
	tx pgx.Tx,
	p Principal,
	delta int64,
) (FactLimitStatus, error) {
	status, err := factLimitStatusTx(ctx, tx, p)
	if err != nil {
		return FactLimitStatus{}, err
	}
	if delta <= 0 || status.Unlimited || status.Max == nil {
		return status, nil
	}
	if status.Used > *status.Max-delta {
		return status, &FactLimitError{Status: status}
	}
	return status, nil
}
