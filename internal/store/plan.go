package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// ErrPlanLimitReached is returned when creating a resource would exceed the
// account's plan-limit snapshot. The message carries the human-readable
// detail ("plan limit reached: agents 25/25 on the free plan") — the HTTP
// layer surfaces it verbatim so the refusal explains itself.
var ErrPlanLimitReached = errors.New("plan limit reached")

// SetAccountPlan applies a plan snapshot to the account: the plan label, the
// resolved limits, and the feature list, exactly as the control plane
// computed them (the cell never consults a catalog). nil limits/features
// normalize to empty — "no caps, no features" — which is also the state
// every account starts in.
func (s *Store) SetAccountPlan(ctx context.Context, accountID, plan string, limits map[string]int64, features []string) error {
	if limits == nil {
		limits = map[string]int64{}
	}
	if features == nil {
		features = []string{}
	}
	limitsJSON, err := json.Marshal(limits)
	if err != nil {
		return fmt.Errorf("marshal plan limits: %w", err)
	}
	featuresJSON, err := json.Marshal(features)
	if err != nil {
		return fmt.Errorf("marshal plan features: %w", err)
	}
	tag, err := s.pool.Exec(ctx,
		`UPDATE accounts SET plan = $2, plan_limits = $3, plan_features = $4 WHERE id = $1`,
		accountID, plan, limitsJSON, featuresJSON)
	if err != nil {
		return fmt.Errorf("set account plan: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrAccountNotFound
	}
	return nil
}

// lockAccountForPlanGate is the create-path lock for plan-capped resources.
// It reuses lockAccountForMint's status semantics (active only) but takes
// FOR NO KEY UPDATE instead of FOR SHARE: share locks do not conflict with
// each other, so two concurrent creates could both count N-1 under the cap
// and both insert. NO KEY UPDATE conflicts with itself AND with the minters'
// share locks, so gated creates on one account serialize — the in-tx count
// is authoritative — while inserts into other tables that merely reference
// the account (KEY SHARE) are unaffected.
func lockAccountForPlanGate(ctx context.Context, tx pgx.Tx, accountID string) (plan string, limits map[string]int64, err error) {
	var status string
	var limitsJSON []byte
	err = tx.QueryRow(ctx,
		`SELECT status, plan, plan_limits FROM accounts WHERE id = $1 FOR NO KEY UPDATE`,
		accountID).Scan(&status, &plan, &limitsJSON)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil, ErrAccountNotFound
	}
	if err != nil {
		return "", nil, fmt.Errorf("lock account: %w", err)
	}
	if status != "active" {
		return "", nil, ErrAccountNotActive
	}
	if err := json.Unmarshal(limitsJSON, &limits); err != nil {
		return "", nil, fmt.Errorf("decode plan limits: %w", err)
	}
	return plan, limits, nil
}

// checkPlanLimit enforces one plan-limit key against the LIVE count of that
// resource, inside the caller's transaction (the plan-gate lock makes the
// count race-safe). A missing key means unlimited.
func checkPlanLimit(plan string, limits map[string]int64, resource string, count int64) error {
	limit, capped := limits[resource]
	if !capped || count < limit {
		return nil
	}
	return fmt.Errorf("%w: %s %d/%d on the %s plan", ErrPlanLimitReached, resource, count, limit, plan)
}

// countLiveRealms counts the account's live realms inside tx.
func countLiveRealms(ctx context.Context, tx pgx.Tx, accountID string) (int64, error) {
	var n int64
	err := tx.QueryRow(ctx,
		`SELECT count(*) FROM realms WHERE account_id = $1 AND deleted_at IS NULL`,
		accountID).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("count realms: %w", err)
	}
	return n, nil
}

// countLiveAgents counts the account's live agents across all live realms
// inside tx. Plan caps are account-wide ("25 agents"), not per-realm.
func countLiveAgents(ctx context.Context, tx pgx.Tx, accountID string) (int64, error) {
	var n int64
	err := tx.QueryRow(ctx,
		`SELECT count(*) FROM agents a
		 JOIN realms r ON r.id = a.realm_id
		 WHERE r.account_id = $1 AND a.deleted_at IS NULL AND r.deleted_at IS NULL`,
		accountID).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("count agents: %w", err)
	}
	return n, nil
}
