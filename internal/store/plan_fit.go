package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/witwave-ai/witself/internal/plans"
)

// Plan-fit wire constants define the closed violation, scope, and apply-state
// vocabularies shared by the store and server layers.
const (
	PlanFitViolationLimitExceeded       = "limit_exceeded"
	PlanFitViolationAuthorityIncomplete = "authority_incomplete"

	PlanFitScopeAccount   = "account"
	PlanFitScopeRealm     = "realm"
	PlanFitScopeAgent     = "agent"
	PlanFitScopeAuthority = "authority"

	PlanFitApplyStateApplied = "applied"
	PlanFitApplyStateBlocked = "blocked"
)

// ErrPlanFitStateAmbiguous means the cell could not prove a coherent,
// value-free usage snapshot. A downgrade must stop rather than guess when a
// derived capacity counter disagrees with its canonical rows.
var ErrPlanFitStateAmbiguous = errors.New("account plan-fit state is ambiguous")

// AccountPlanFitTarget is the exact resolved target snapshot supplied by the
// control plane. It is intentionally separate from the cell's currently
// applied snapshot: a downgrade fit check necessarily runs before that target
// may be applied.
type AccountPlanFitTarget struct {
	Plan         string
	SnapshotHash string
	Limits       map[string]int64
	Policies     map[string]int64
	Features     []string
}

// AccountPlanFitViolation is a value-free capacity refusal. For realm- and
// agent-scoped limits, Used is the highest observed usage and SubjectCount is
// the number of scopes above Max; no realm, agent, memory, fact, secret, alias,
// domain, or email identifier crosses the system endpoint.
type AccountPlanFitViolation struct {
	Code         string `json:"code"`
	Dimension    string `json:"dimension"`
	Scope        string `json:"scope"`
	Used         int64  `json:"used"`
	Max          int64  `json:"max"`
	SubjectCount int64  `json:"subject_count"`
}

// AccountPlanFitReport is a read-only comparison against one resolved target.
type AccountPlanFitReport struct {
	AccountID          string
	TargetPlan         string
	TargetSnapshotHash string
	Violations         []AccountPlanFitViolation
}

// AccountPlanFitApplyTarget is a fenced, complete resolved snapshot. Unlike a
// read-only fit target, it carries the positive control-plane revision that the
// cell must acknowledge if the target fits.
type AccountPlanFitApplyTarget struct {
	Revision     int64
	Plan         string
	SnapshotHash string
	Limits       map[string]int64
	Policies     map[string]int64
	Features     []string
}

// AccountPlanFitApplyResult is the strict outcome of one atomic fit-and-apply
// attempt. AppliedSnapshot is set only for applied (including exact replay),
// while CurrentSnapshot and non-empty Violations are set only for blocked.
type AccountPlanFitApplyResult struct {
	State              string
	AccountID          string
	TargetRevision     int64
	TargetPlan         string
	TargetSnapshotHash string
	Violations         []AccountPlanFitViolation
	CurrentSnapshot    *AccountPlanSnapshot
	AppliedSnapshot    *AccountPlanSnapshot
}

type accountPlanFitApplyContext struct {
	target  AccountPlanFitApplyTarget
	current AccountPlanSnapshot
	applied *AccountPlanSnapshot
}

// CheckAccountPlanFit compares canonical durable account usage with every
// finite storage/count limit understood by the target snapshot. The complete
// read runs in one repeatable-read, read-only transaction. It never trims,
// disables, retires, or otherwise mutates tenant state.
func (s *Store) CheckAccountPlanFit(
	ctx context.Context,
	accountID string,
	target AccountPlanFitTarget,
) (AccountPlanFitReport, error) {
	return s.checkAccountPlanFit(ctx, accountID, target, nil)
}

// ApplyAccountPlanIfFits rechecks every finite durable-capacity dimension and
// applies the fenced target snapshot in the same READ COMMITTED transaction.
// The account row is locked FOR NO KEY UPDATE before the recheck. All resource
// writers that can change these counts take a conflicting account lock, so no
// create can slip between the final fit read and the snapshot update.
//
// An exact revision/hash replay returns the already-persisted snapshot without
// re-running fit. Older revisions and same-revision/different-hash attempts are
// rejected by the same monotonic fence as SetAccountPlan.
func (s *Store) ApplyAccountPlanIfFits(
	ctx context.Context,
	accountID string,
	target AccountPlanFitApplyTarget,
) (AccountPlanFitApplyResult, error) {
	if err := validateAccountPlanFitApplyTarget(target); err != nil {
		return AccountPlanFitApplyResult{}, err
	}
	accountID = strings.TrimSpace(accountID)
	apply := &accountPlanFitApplyContext{target: target}
	report, err := s.checkAccountPlanFit(ctx, accountID, target.fitTarget(), apply)
	if err != nil {
		return AccountPlanFitApplyResult{}, err
	}
	result := AccountPlanFitApplyResult{
		AccountID: accountID, TargetRevision: target.Revision,
		TargetPlan: target.Plan, TargetSnapshotHash: target.SnapshotHash,
		Violations: append([]AccountPlanFitViolation{}, report.Violations...),
	}
	if apply.applied != nil {
		result.State = PlanFitApplyStateApplied
		result.AppliedSnapshot = apply.applied
		return result, nil
	}
	result.State = PlanFitApplyStateBlocked
	current := apply.current
	result.CurrentSnapshot = &current
	return result, nil
}

func (s *Store) checkAccountPlanFit(
	ctx context.Context,
	accountID string,
	target AccountPlanFitTarget,
	apply *accountPlanFitApplyContext,
) (AccountPlanFitReport, error) {
	if err := validateAccountPlanFitTarget(target); err != nil {
		return AccountPlanFitReport{}, err
	}
	accountID = strings.TrimSpace(accountID)
	if accountID == "" {
		return AccountPlanFitReport{}, ErrAccountNotFound
	}
	txOptions := pgx.TxOptions{
		IsoLevel:   pgx.RepeatableRead,
		AccessMode: pgx.ReadOnly,
	}
	if apply != nil {
		txOptions = pgx.TxOptions{IsoLevel: pgx.ReadCommitted}
	}
	tx, err := s.pool.BeginTx(ctx, txOptions)
	if err != nil {
		return AccountPlanFitReport{}, fmt.Errorf("begin account plan-fit read: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var accountStatus string
	var retainedAttachmentBytes int64
	if apply == nil {
		err = tx.QueryRow(ctx, `
			SELECT status,retained_agent_email_attachment_bytes
			  FROM accounts
			 WHERE id=$1`, accountID).Scan(&accountStatus, &retainedAttachmentBytes)
	} else {
		err = lockAccountPlanForFitApply(
			ctx, tx, accountID, &accountStatus, &retainedAttachmentBytes,
			&apply.current,
		)
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return AccountPlanFitReport{}, ErrAccountNotFound
	}
	if err != nil {
		return AccountPlanFitReport{}, fmt.Errorf("read account plan-fit state: %w", err)
	}
	if apply != nil && !validAccountPlanSnapshotForFitApply(apply.current) {
		return AccountPlanFitReport{}, fmt.Errorf(
			"%w: persisted current plan snapshot is invalid",
			ErrPlanFitStateAmbiguous,
		)
	}

	report := AccountPlanFitReport{
		AccountID:          accountID,
		TargetPlan:         target.Plan,
		TargetSnapshotHash: target.SnapshotHash,
		Violations:         []AccountPlanFitViolation{},
	}
	if apply != nil {
		switch {
		case apply.target.Revision < apply.current.Revision,
			apply.target.Revision == apply.current.Revision &&
				apply.target.SnapshotHash != apply.current.Hash:
			return AccountPlanFitReport{}, ErrPlanSnapshotStale
		case apply.target.Revision == apply.current.Revision:
			if !accountPlanSnapshotMatchesFitApplyTarget(apply.current, apply.target) {
				return AccountPlanFitReport{}, fmt.Errorf(
					"%w: persisted plan snapshot does not match its fence",
					ErrPlanFitStateAmbiguous,
				)
			}
			current := apply.current
			apply.applied = &current
			if err := tx.Commit(ctx); err != nil {
				return AccountPlanFitReport{}, fmt.Errorf(
					"commit account plan-fit apply replay: %w", err,
				)
			}
			return report, nil
		}
	}
	if accountStatus != "active" {
		return AccountPlanFitReport{}, ErrAccountNotActive
	}
	if maximum, finite := target.Limits[plans.RealmLimit]; finite {
		used, err := accountPlanFitCount(ctx, tx, `
			SELECT count(*) FROM realms
			 WHERE account_id=$1 AND deleted_at IS NULL`, accountID)
		if err != nil {
			return AccountPlanFitReport{}, fmt.Errorf("count plan-fit realms: %w", err)
		}
		report.addAccountViolation(plans.RealmLimit, used, maximum)
	}
	if maximum, finite := target.Limits[plans.OperatorSeatsLimit]; finite {
		used, err := accountPlanFitCount(ctx, tx, `
			SELECT count(*) FROM operators
			 WHERE account_id=$1 AND deleted_at IS NULL`, accountID)
		if err != nil {
			return AccountPlanFitReport{}, fmt.Errorf("count plan-fit operator seats: %w", err)
		}
		report.addAccountViolation(plans.OperatorSeatsLimit, used, maximum)
	}
	if maximum, finite := target.Limits[plans.AgentLimit]; finite {
		used, err := accountPlanFitCount(ctx, tx, `
			SELECT count(*)
			  FROM agents agent
			  JOIN realms realm ON realm.id=agent.realm_id
			 WHERE realm.account_id=$1
			   AND realm.deleted_at IS NULL AND agent.deleted_at IS NULL`, accountID)
		if err != nil {
			return AccountPlanFitReport{}, fmt.Errorf("count plan-fit agents: %w", err)
		}
		report.addAccountViolation(plans.AgentLimit, used, maximum)
	}
	if maximum, finite := target.Limits[plans.AgentPerRealmLimit]; finite {
		over, highest, err := accountPlanFitScopedCount(ctx, tx, `
			SELECT count(*) FILTER (WHERE used > $2),COALESCE(max(used),0)
			  FROM (
			    SELECT realm.id,count(agent.id)::bigint AS used
			      FROM realms realm
			      LEFT JOIN agents agent
			        ON agent.realm_id=realm.id AND agent.deleted_at IS NULL
			     WHERE realm.account_id=$1 AND realm.deleted_at IS NULL
			     GROUP BY realm.id
			  ) scoped`, accountID, maximum)
		if err != nil {
			return AccountPlanFitReport{}, fmt.Errorf("count plan-fit agents per realm: %w", err)
		}
		report.addScopedViolation(
			plans.AgentPerRealmLimit, PlanFitScopeRealm, over, highest, maximum,
		)
	}
	if maximum, finite := target.Limits[plans.StoredMemoryLimit]; finite {
		over, highest, mismatched, err := accountPlanFitOwnerCount(ctx, tx, `
			WITH usage AS (
			  SELECT agent.id,
			         count(memory.id) FILTER (WHERE version.state='active')::bigint AS used,
			         COALESCE(clock.active_memory_count,0)::bigint AS derived
			    FROM realms realm
			    JOIN agents agent
			      ON agent.realm_id=realm.id AND agent.deleted_at IS NULL
			    LEFT JOIN memories memory
			      ON memory.account_id=realm.account_id
			     AND memory.realm_id=realm.id
			     AND memory.owner_kind='agent'
			     AND memory.owner_id=agent.id
			     AND memory.current_version IS NOT NULL
			    LEFT JOIN memory_versions version
			      ON version.memory_id=memory.id
			     AND version.version=memory.current_version
			    LEFT JOIN memory_change_clocks clock
			      ON clock.account_id=realm.account_id
			     AND clock.realm_id=realm.id
			     AND clock.owner_kind='agent'
			     AND clock.owner_id=agent.id
			   WHERE realm.account_id=$1 AND realm.deleted_at IS NULL
			   GROUP BY agent.id,clock.active_memory_count
			)
			SELECT count(*) FILTER (WHERE used > $2),COALESCE(max(used),0),
			       count(*) FILTER (WHERE used <> derived)
			  FROM usage`, accountID, maximum)
		if err != nil {
			return AccountPlanFitReport{}, fmt.Errorf("count plan-fit active memories: %w", err)
		}
		if mismatched != 0 {
			return AccountPlanFitReport{}, fmt.Errorf(
				"%w: active memory counters disagree for %d agents",
				ErrPlanFitStateAmbiguous, mismatched,
			)
		}
		report.addScopedViolation(
			plans.StoredMemoryLimit, PlanFitScopeAgent, over, highest, maximum,
		)
	}
	if maximum, finite := target.Limits[plans.StoredFactLimit]; finite {
		over, highest, mismatched, err := accountPlanFitOwnerCount(ctx, tx, `
			WITH usage AS (
			  SELECT agent.id,count(fact.id)::bigint AS used,
			         agent.active_fact_count::bigint AS derived
			    FROM realms realm
			    JOIN agents agent
			      ON agent.realm_id=realm.id AND agent.deleted_at IS NULL
			    LEFT JOIN facts fact
			      ON fact.account_id=realm.account_id
			     AND fact.realm_id=realm.id
			     AND fact.owner_agent_id=agent.id
			     AND fact.deleted_at IS NULL
			     AND fact.resolved_assertion_id IS NOT NULL
			   WHERE realm.account_id=$1 AND realm.deleted_at IS NULL
			   GROUP BY agent.id,agent.active_fact_count
			)
			SELECT count(*) FILTER (WHERE used > $2),COALESCE(max(used),0),
			       count(*) FILTER (WHERE used <> derived)
			  FROM usage`, accountID, maximum)
		if err != nil {
			return AccountPlanFitReport{}, fmt.Errorf("count plan-fit active facts: %w", err)
		}
		if mismatched != 0 {
			return AccountPlanFitReport{}, fmt.Errorf(
				"%w: active fact counters disagree for %d agents",
				ErrPlanFitStateAmbiguous, mismatched,
			)
		}
		report.addScopedViolation(
			plans.StoredFactLimit, PlanFitScopeAgent, over, highest, maximum,
		)
	}
	if maximum, finite := target.Limits[plans.StoredSecretLimit]; finite {
		over, highest, err := accountPlanFitScopedCount(ctx, tx, `
			SELECT count(*) FILTER (WHERE used > $2),COALESCE(max(used),0)
			  FROM (
			    SELECT agent.id,count(secret.id)::bigint AS used
			      FROM realms realm
			      JOIN agents agent
			        ON agent.realm_id=realm.id AND agent.deleted_at IS NULL
			      LEFT JOIN secrets secret
			        ON secret.account_id=realm.account_id
			       AND secret.realm_id=realm.id
			       AND secret.owner_agent_id=agent.id
			       AND secret.deleted_at IS NULL
			     WHERE realm.account_id=$1 AND realm.deleted_at IS NULL
			     GROUP BY agent.id
			  ) scoped`, accountID, maximum)
		if err != nil {
			return AccountPlanFitReport{}, fmt.Errorf("count plan-fit retained secrets: %w", err)
		}
		report.addScopedViolation(
			plans.StoredSecretLimit, PlanFitScopeAgent, over, highest, maximum,
		)
	}
	if maximum, finite := target.Limits[plans.AgentEmailAttachmentStorageBytesLimit]; finite {
		canonical, err := accountPlanFitCount(ctx, tx, `
			SELECT COALESCE(sum(retained_attachment_storage_bytes),0)::bigint
			  FROM agent_email_messages
			 WHERE account_id=$1`, accountID)
		if err != nil {
			return AccountPlanFitReport{}, fmt.Errorf("count plan-fit retained email attachment bytes: %w", err)
		}
		if canonical != retainedAttachmentBytes {
			return AccountPlanFitReport{}, fmt.Errorf(
				"%w: retained email attachment counter is %d but canonical usage is %d",
				ErrPlanFitStateAmbiguous, retainedAttachmentBytes, canonical,
			)
		}
		report.addAccountViolation(
			plans.AgentEmailAttachmentStorageBytesLimit, canonical, maximum,
		)
	}
	if maximum, finite := target.Limits[plans.AgentEmailRealmAliasesPerRealmLimit]; finite {
		over, highest, err := accountPlanFitScopedCount(ctx, tx, `
			SELECT count(*) FILTER (WHERE used > $2),COALESCE(max(used),0)
			  FROM (
			    SELECT realm.id,count(alias.claim_id)::bigint AS used
			      FROM realms realm
			      LEFT JOIN agent_email_realm_aliases alias
			        ON alias.account_id=realm.account_id
			       AND alias.realm_id=realm.id
			       AND alias.state='applied'
			     WHERE realm.account_id=$1 AND realm.deleted_at IS NULL
			     GROUP BY realm.id
			  ) scoped`, accountID, maximum)
		if err != nil {
			return AccountPlanFitReport{}, fmt.Errorf("count plan-fit active realm aliases: %w", err)
		}
		report.addScopedViolation(
			plans.AgentEmailRealmAliasesPerRealmLimit,
			PlanFitScopeRealm, over, highest, maximum,
		)
	}
	if maximum, finite := target.Limits[plans.AgentEmailCustomDomainsPerAccountLimit]; finite {
		used, err := accountPlanFitCount(ctx, tx, `
			SELECT count(DISTINCT domain_request_id)
			  FROM agent_email_custom_domain_routes
			 WHERE account_id=$1 AND state='applied'`, accountID)
		if err != nil {
			return AccountPlanFitReport{}, fmt.Errorf("count plan-fit active custom domains: %w", err)
		}
		report.addAccountViolation(
			plans.AgentEmailCustomDomainsPerAccountLimit, used, maximum,
		)
	}
	if apply != nil && len(report.Violations) == 0 {
		applied, err := applyAccountPlanFitTargetTx(
			ctx, tx, accountID, apply.current, apply.target,
		)
		if err != nil {
			return AccountPlanFitReport{}, err
		}
		apply.applied = &applied
	}

	if err := tx.Commit(ctx); err != nil {
		operation := "read"
		if apply != nil {
			operation = "apply"
		}
		return AccountPlanFitReport{}, fmt.Errorf("commit account plan-fit %s: %w", operation, err)
	}
	return report, nil
}

func (target AccountPlanFitApplyTarget) fitTarget() AccountPlanFitTarget {
	return AccountPlanFitTarget{
		Plan: target.Plan, SnapshotHash: target.SnapshotHash,
		Limits: target.Limits, Policies: target.Policies, Features: target.Features,
	}
}

func validateAccountPlanFitApplyTarget(target AccountPlanFitApplyTarget) error {
	if target.Revision < 1 {
		return fmt.Errorf("%w: revision must be positive", ErrPlanSnapshotInvalid)
	}
	return validateAccountPlanFitTarget(target.fitTarget())
}

func lockAccountPlanForFitApply(
	ctx context.Context,
	tx pgx.Tx,
	accountID string,
	status *string,
	retainedAttachmentBytes *int64,
	snapshot *AccountPlanSnapshot,
) error {
	var limits, policies, features []byte
	err := tx.QueryRow(ctx, `
		SELECT status,retained_agent_email_attachment_bytes,
		       id,plan_snapshot_revision,plan_snapshot_hash,plan,
		       plan_limits,plan_policies,plan_features,plan_applied_at
		  FROM accounts
		 WHERE id=$1
		 FOR NO KEY UPDATE`, accountID).Scan(
		status, retainedAttachmentBytes,
		&snapshot.AccountID, &snapshot.Revision, &snapshot.Hash, &snapshot.Plan,
		&limits, &policies, &features, &snapshot.AppliedAt,
	)
	if err != nil {
		return err
	}
	if err := decodeAccountPlanSnapshot(snapshot, limits, policies, features); err != nil {
		return fmt.Errorf("decode current account plan for fit apply: %w", err)
	}
	return nil
}

func accountPlanSnapshotMatchesFitApplyTarget(
	snapshot AccountPlanSnapshot,
	target AccountPlanFitApplyTarget,
) bool {
	if snapshot.AccountID == "" || snapshot.Revision != target.Revision ||
		snapshot.Hash != target.SnapshotHash || snapshot.Plan != target.Plan ||
		!maps.Equal(snapshot.Limits, target.Limits) ||
		!maps.Equal(snapshot.Policies, target.Policies) {
		return false
	}
	wantFeatures := append([]string{}, target.Features...)
	slices.Sort(wantFeatures)
	if !slices.Equal(snapshot.Features, wantFeatures) {
		return false
	}
	hash, err := plans.SnapshotHash(
		snapshot.Plan, snapshot.Limits, snapshot.Policies, snapshot.Features,
	)
	return err == nil && hash == snapshot.Hash
}

func validAccountPlanSnapshotForFitApply(snapshot AccountPlanSnapshot) bool {
	if snapshot.AccountID == "" || snapshot.Revision < 0 || snapshot.Plan == "" ||
		snapshot.Plan != strings.TrimSpace(snapshot.Plan) || snapshot.Limits == nil ||
		snapshot.Policies == nil || snapshot.Features == nil {
		return false
	}
	if err := plans.ValidateLimits(snapshot.Limits); err != nil {
		return false
	}
	if err := plans.ValidatePolicies(snapshot.Policies); err != nil {
		return false
	}
	if err := plans.ValidateFeatures(snapshot.Features); err != nil {
		return false
	}
	if snapshot.Revision == 0 {
		return snapshot.Hash == ""
	}
	if snapshot.AppliedAt == nil {
		return false
	}
	hash, err := plans.SnapshotHash(
		snapshot.Plan, snapshot.Limits, snapshot.Policies, snapshot.Features,
	)
	return err == nil && hash == snapshot.Hash
}

func applyAccountPlanFitTargetTx(
	ctx context.Context,
	tx pgx.Tx,
	accountID string,
	current AccountPlanSnapshot,
	target AccountPlanFitApplyTarget,
) (AccountPlanSnapshot, error) {
	features := append([]string{}, target.Features...)
	slices.Sort(features)
	limitsJSON, err := json.Marshal(target.Limits)
	if err != nil {
		return AccountPlanSnapshot{}, fmt.Errorf("marshal plan-fit apply limits: %w", err)
	}
	policiesJSON, err := json.Marshal(target.Policies)
	if err != nil {
		return AccountPlanSnapshot{}, fmt.Errorf("marshal plan-fit apply policies: %w", err)
	}
	featuresJSON, err := json.Marshal(features)
	if err != nil {
		return AccountPlanSnapshot{}, fmt.Errorf("marshal plan-fit apply features: %w", err)
	}

	var applied AccountPlanSnapshot
	var appliedLimits, appliedPolicies, appliedFeatures []byte
	err = tx.QueryRow(ctx, `
		UPDATE accounts
		   SET plan=$2,plan_limits=$3,plan_policies=$4,plan_features=$5,
		       plan_applied_at=statement_timestamp(),
		       plan_snapshot_revision=$6,plan_snapshot_hash=$7
		 WHERE id=$1
		   AND plan_snapshot_revision=$8 AND plan_snapshot_hash=$9
		 RETURNING id,plan_snapshot_revision,plan_snapshot_hash,plan,
		           plan_limits,plan_policies,plan_features,plan_applied_at`,
		accountID, target.Plan, limitsJSON, policiesJSON, featuresJSON,
		target.Revision, target.SnapshotHash, current.Revision, current.Hash,
	).Scan(
		&applied.AccountID, &applied.Revision, &applied.Hash, &applied.Plan,
		&appliedLimits, &appliedPolicies, &appliedFeatures, &applied.AppliedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return AccountPlanSnapshot{}, ErrPlanSnapshotStale
	}
	if err != nil {
		return AccountPlanSnapshot{}, fmt.Errorf("apply account plan after fit: %w", err)
	}
	if err := decodeAccountPlanSnapshot(
		&applied, appliedLimits, appliedPolicies, appliedFeatures,
	); err != nil {
		return AccountPlanSnapshot{}, err
	}
	if !accountPlanSnapshotMatchesFitApplyTarget(applied, target) {
		return AccountPlanSnapshot{}, fmt.Errorf(
			"%w: applied plan snapshot does not match target", ErrPlanFitStateAmbiguous,
		)
	}
	return applied, nil
}

func validateAccountPlanFitTarget(target AccountPlanFitTarget) error {
	if target.Plan == "" || target.Plan != strings.TrimSpace(target.Plan) ||
		target.SnapshotHash == "" || target.Limits == nil ||
		target.Policies == nil || target.Features == nil {
		return fmt.Errorf("%w: complete resolved target snapshot is required", ErrPlanSnapshotInvalid)
	}
	if err := plans.ValidateLimits(target.Limits); err != nil {
		return fmt.Errorf("%w: %v", ErrPlanSnapshotInvalid, err)
	}
	if err := plans.ValidatePolicies(target.Policies); err != nil {
		return fmt.Errorf("%w: %v", ErrPlanSnapshotInvalid, err)
	}
	if err := plans.ValidateFeatures(target.Features); err != nil {
		return fmt.Errorf("%w: %v", ErrPlanSnapshotInvalid, err)
	}
	expected, err := plans.SnapshotHash(
		target.Plan, target.Limits, target.Policies, target.Features,
	)
	if err != nil || target.SnapshotHash != expected {
		return fmt.Errorf("%w: target snapshot hash does not match payload", ErrPlanSnapshotInvalid)
	}
	return nil
}

func accountPlanFitCount(
	ctx context.Context,
	tx pgx.Tx,
	query string,
	args ...any,
) (int64, error) {
	var count int64
	err := tx.QueryRow(ctx, query, args...).Scan(&count)
	return count, err
}

func accountPlanFitScopedCount(
	ctx context.Context,
	tx pgx.Tx,
	query string,
	args ...any,
) (over, highest int64, err error) {
	err = tx.QueryRow(ctx, query, args...).Scan(&over, &highest)
	return over, highest, err
}

func accountPlanFitOwnerCount(
	ctx context.Context,
	tx pgx.Tx,
	query string,
	args ...any,
) (over, highest, mismatched int64, err error) {
	err = tx.QueryRow(ctx, query, args...).Scan(&over, &highest, &mismatched)
	return over, highest, mismatched, err
}

func (report *AccountPlanFitReport) addAccountViolation(
	dimension string,
	used, maximum int64,
) {
	if used <= maximum {
		return
	}
	report.Violations = append(report.Violations, AccountPlanFitViolation{
		Code: PlanFitViolationLimitExceeded, Dimension: dimension,
		Scope: PlanFitScopeAccount, Used: used, Max: maximum, SubjectCount: 1,
	})
}

func (report *AccountPlanFitReport) addScopedViolation(
	dimension, scope string,
	over, highest, maximum int64,
) {
	if over == 0 {
		return
	}
	report.Violations = append(report.Violations, AccountPlanFitViolation{
		Code: PlanFitViolationLimitExceeded, Dimension: dimension,
		Scope: scope, Used: highest, Max: maximum, SubjectCount: over,
	})
}
