package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/witwave-ai/witself/internal/plans"
)

// ErrPlanLimitReached is returned when creating a resource would exceed the
// account's plan-limit snapshot. The message carries the human-readable
// detail ("plan limit reached: agents 25/25 on the free plan") — the HTTP
// layer surfaces it verbatim so the refusal explains itself.
var ErrPlanLimitReached = errors.New("plan limit reached")

// ErrPlanPolicyInvalid means a cell policy snapshot contains an unknown key or
// a value outside the policy's accepted representation bounds.
var ErrPlanPolicyInvalid = errors.New("invalid plan policy snapshot")

// ErrPlanSnapshotStale means the cell has already accepted a newer snapshot
// revision (or the same revision with a different digest). A stale control
// plane writer must reload and must never overwrite the newer cell state.
var ErrPlanSnapshotStale = errors.New("stale plan snapshot")

// ErrPlanSnapshotInvalid means the revision/hash envelope does not describe
// the supplied snapshot exactly.
var ErrPlanSnapshotInvalid = errors.New("invalid plan snapshot")

// AccountPlanSnapshot is the exact account policy state durably accepted by a
// cell. Revision/hash are the cross-control-plane fencing acknowledgement.
type AccountPlanSnapshot struct {
	AccountID string
	Revision  int64
	Hash      string
	Plan      string
	Limits    map[string]int64
	Policies  map[string]int64
	Features  []string
	AppliedAt *time.Time
}

// SetAccountPlan applies a plan snapshot to the account: the plan label, the
// resolved limits, and the feature list, exactly as the control plane
// computed them (the cell never consults a catalog). nil limits/features
// normalize to empty — "no caps, no features" — which is also the state
// every account starts in.
//
// A positive revision is fenced: only a newer revision, or an idempotent
// replay of the same revision/hash, is accepted. The revision-zero/empty-hash
// shape is a rollout-only legacy request and is accepted only while the
// account has never accepted a fenced snapshot.
func (s *Store) SetAccountPlan(
	ctx context.Context,
	accountID string,
	revision int64,
	snapshotHash, plan string,
	limits, policies map[string]int64,
	features []string,
) (AccountPlanSnapshot, error) {
	plan = strings.TrimSpace(plan)
	if plan == "" {
		return AccountPlanSnapshot{}, fmt.Errorf("%w: plan is required", ErrPlanSnapshotInvalid)
	}
	if limits == nil {
		limits = map[string]int64{}
	}
	if err := plans.ValidateLimits(limits); err != nil {
		return AccountPlanSnapshot{}, fmt.Errorf("%w: %v", ErrPlanSnapshotInvalid, err)
	}
	if features == nil {
		features = []string{}
	}
	// Preserve the documented nil-to-empty normalization in the durable JSON
	// shape. Appending to a nil destination turns an empty slice back into nil
	// and json.Marshal would emit null instead of [].
	features = append([]string{}, features...)
	slices.Sort(features)
	if policies == nil {
		policies = map[string]int64{}
	}
	for key, value := range policies {
		if key != TranscriptRetentionDaysPolicy {
			return AccountPlanSnapshot{}, fmt.Errorf("%w: unknown policy %q", ErrPlanPolicyInvalid, key)
		}
		if value < 1 || value > 36500 {
			return AccountPlanSnapshot{}, fmt.Errorf("%w: %s must be between 1 and 36500 days",
				ErrPlanPolicyInvalid, TranscriptRetentionDaysPolicy)
		}
	}
	legacy := revision == 0 && snapshotHash == ""
	if !legacy {
		if revision < 1 {
			return AccountPlanSnapshot{}, fmt.Errorf("%w: revision must be positive", ErrPlanSnapshotInvalid)
		}
		expectedHash, err := plans.SnapshotHash(plan, limits, policies, features)
		if err != nil {
			return AccountPlanSnapshot{}, fmt.Errorf("%w: %v", ErrPlanSnapshotInvalid, err)
		}
		if snapshotHash != expectedHash {
			return AccountPlanSnapshot{}, fmt.Errorf("%w: snapshot hash does not match payload", ErrPlanSnapshotInvalid)
		}
	}
	limitsJSON, err := json.Marshal(limits)
	if err != nil {
		return AccountPlanSnapshot{}, fmt.Errorf("marshal plan limits: %w", err)
	}
	featuresJSON, err := json.Marshal(features)
	if err != nil {
		return AccountPlanSnapshot{}, fmt.Errorf("marshal plan features: %w", err)
	}
	policiesJSON, err := json.Marshal(policies)
	if err != nil {
		return AccountPlanSnapshot{}, fmt.Errorf("marshal plan policies: %w", err)
	}
	var applied AccountPlanSnapshot
	var appliedLimits, appliedPolicies, appliedFeatures []byte
	err = s.pool.QueryRow(ctx,
		`UPDATE accounts
		 SET plan = $2, plan_limits = $3, plan_policies = $4,
		     plan_features = $5, plan_applied_at = statement_timestamp(),
		     plan_snapshot_revision = $6, plan_snapshot_hash = $7
		 WHERE id = $1
		   AND (
		     ($6 = 0 AND $7 = '' AND plan_snapshot_revision = 0)
		     OR ($6 > plan_snapshot_revision)
		     OR ($6 = plan_snapshot_revision AND $7 = plan_snapshot_hash)
		   )
		 RETURNING id, plan_snapshot_revision, plan_snapshot_hash, plan,
		           plan_limits, plan_policies, plan_features, plan_applied_at`,
		accountID, plan, limitsJSON, policiesJSON, featuresJSON, revision, snapshotHash).
		Scan(&applied.AccountID, &applied.Revision, &applied.Hash, &applied.Plan,
			&appliedLimits, &appliedPolicies, &appliedFeatures, &applied.AppliedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		var exists bool
		if err := s.pool.QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM accounts WHERE id = $1)`, accountID).
			Scan(&exists); err != nil {
			return AccountPlanSnapshot{}, fmt.Errorf("check stale plan target: %w", err)
		}
		if !exists {
			return AccountPlanSnapshot{}, ErrAccountNotFound
		}
		return AccountPlanSnapshot{}, ErrPlanSnapshotStale
	}
	if err != nil {
		return AccountPlanSnapshot{}, fmt.Errorf("set account plan: %w", err)
	}
	if err := decodeAccountPlanSnapshot(&applied, appliedLimits, appliedPolicies, appliedFeatures); err != nil {
		return AccountPlanSnapshot{}, err
	}
	return applied, nil
}

// GetAccountPlan returns the cell's current acknowledged account snapshot.
func (s *Store) GetAccountPlan(ctx context.Context, accountID string) (AccountPlanSnapshot, error) {
	var snapshot AccountPlanSnapshot
	var limits, policies, features []byte
	err := s.pool.QueryRow(ctx, `
		SELECT id, plan_snapshot_revision, plan_snapshot_hash, plan,
		       plan_limits, plan_policies, plan_features, plan_applied_at
		  FROM accounts
		 WHERE id = $1`,
		accountID).Scan(
		&snapshot.AccountID, &snapshot.Revision, &snapshot.Hash, &snapshot.Plan,
		&limits, &policies, &features, &snapshot.AppliedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return AccountPlanSnapshot{}, ErrAccountNotFound
	}
	if err != nil {
		return AccountPlanSnapshot{}, fmt.Errorf("get account plan: %w", err)
	}
	if err := decodeAccountPlanSnapshot(&snapshot, limits, policies, features); err != nil {
		return AccountPlanSnapshot{}, err
	}
	return snapshot, nil
}

func decodeAccountPlanSnapshot(
	snapshot *AccountPlanSnapshot,
	limits, policies, features []byte,
) error {
	if err := json.Unmarshal(limits, &snapshot.Limits); err != nil {
		return fmt.Errorf("decode plan limits: %w", err)
	}
	if err := json.Unmarshal(policies, &snapshot.Policies); err != nil {
		return fmt.Errorf("decode plan policies: %w", err)
	}
	if err := json.Unmarshal(features, &snapshot.Features); err != nil {
		return fmt.Errorf("decode plan features: %w", err)
	}
	return nil
}

// lockAccountForPlanGate is the create-path lock for account-wide plan-capped
// resources.
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
