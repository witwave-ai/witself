package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/witwave-ai/witself/internal/plans"
)

// MemoryLimitStatus is the authenticated owner's value-free active-memory
// capacity. Max and Remaining are nil when the resolved account snapshot
// omits stored_memory, which is the canonical unlimited representation.
type MemoryLimitStatus struct {
	Used      int64  `json:"used"`
	Max       *int64 `json:"max"`
	Remaining *int64 `json:"remaining"`
	Unlimited bool   `json:"unlimited"`
	NearLimit bool   `json:"near_limit"`
	AtLimit   bool   `json:"at_limit"`
	OverLimit bool   `json:"over_limit"`
}

// MemoryLimitError is a machine-classifiable refusal of a net-positive
// active-memory mutation. It contains only the authenticated owner's
// value-free capacity snapshot.
type MemoryLimitError struct {
	Status MemoryLimitStatus
}

func (e *MemoryLimitError) Error() string {
	if e == nil || e.Status.Max == nil {
		return ErrPlanLimitReached.Error()
	}
	return fmt.Sprintf("%s: %s %d/%d", ErrPlanLimitReached,
		plans.StoredMemoryLimit, e.Status.Used, *e.Status.Max)
}

func (e *MemoryLimitError) Unwrap() error { return ErrPlanLimitReached }

// GetMemoryLimitStatus returns the active-memory capacity for one
// authenticated owner agent. It never returns memory identifiers or content.
func (s *Store) GetMemoryLimitStatus(
	ctx context.Context,
	p Principal,
) (MemoryLimitStatus, error) {
	if p.Kind != PrincipalAgent || p.AccountID == "" || p.RealmID == "" || p.ID == "" {
		return MemoryLimitStatus{}, ErrMemoryForbidden
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{
		IsoLevel:   pgx.RepeatableRead,
		AccessMode: pgx.ReadOnly,
	})
	if err != nil {
		return MemoryLimitStatus{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	status, err := memoryLimitStatusTx(ctx, tx, p)
	if err != nil {
		return MemoryLimitStatus{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return MemoryLimitStatus{}, err
	}
	return status, nil
}

func memoryLimitStatusTx(
	ctx context.Context,
	tx pgx.Tx,
	p Principal,
) (MemoryLimitStatus, error) {
	var accountStatus string
	var limitsJSON []byte
	var used int64
	err := tx.QueryRow(ctx, `
		SELECT a.status, a.plan_limits,
		       COALESCE(c.active_memory_count, 0)
		  FROM accounts a
		  JOIN realms r
		    ON r.account_id=a.id AND r.id=$2 AND r.deleted_at IS NULL
		  JOIN agents owner
		    ON owner.realm_id=r.id AND owner.id=$3 AND owner.deleted_at IS NULL
		  LEFT JOIN memory_change_clocks c
		    ON c.account_id=a.id AND c.realm_id=r.id
		   AND c.owner_kind='agent' AND c.owner_id=owner.id
		 WHERE a.id=$1`,
		p.AccountID, p.RealmID, p.ID,
	).Scan(&accountStatus, &limitsJSON, &used)
	if errors.Is(err, pgx.ErrNoRows) {
		return MemoryLimitStatus{}, ErrMemoryForbidden
	}
	if err != nil {
		return MemoryLimitStatus{}, fmt.Errorf("read memory capacity: %w", err)
	}
	if accountStatus != "active" {
		return MemoryLimitStatus{}, ErrAccountNotActive
	}
	var limits map[string]int64
	if err := json.Unmarshal(limitsJSON, &limits); err != nil {
		return MemoryLimitStatus{}, fmt.Errorf("decode memory plan limits: %w", err)
	}
	maximum, capped := limits[plans.StoredMemoryLimit]
	if !capped {
		return MemoryLimitStatus{Used: used, Unlimited: true}, nil
	}
	remaining := maximum - used
	if remaining < 0 {
		remaining = 0
	}
	// Round 90% upward so small limits warn before the final slot whenever
	// possible. MaxPlanLimit keeps the multiplication within int64.
	warningAt := (maximum*9 + 9) / 10
	return MemoryLimitStatus{
		Used: used, Max: &maximum, Remaining: &remaining,
		NearLimit: used >= warningAt,
		AtLimit:   used == maximum,
		OverLimit: used > maximum,
	}, nil
}

// requireActiveMemoryCapacityTx admits unlimited and non-growing mutations.
// For a positive delta it uses the exact owner counter while the caller holds
// the memory-change-clock lock, so concurrent replicas cannot overshoot.
func requireActiveMemoryCapacityTx(
	ctx context.Context,
	tx pgx.Tx,
	p Principal,
	delta int64,
) (MemoryLimitStatus, error) {
	status, err := memoryLimitStatusTx(ctx, tx, p)
	if err != nil {
		return MemoryLimitStatus{}, err
	}
	if delta <= 0 || status.Unlimited || status.Max == nil {
		return status, nil
	}
	if status.Used > *status.Max-delta {
		return status, &MemoryLimitError{Status: status}
	}
	return status, nil
}
