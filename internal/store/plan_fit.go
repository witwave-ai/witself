package store

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/witwave-ai/witself/internal/plans"
)

const (
	PlanFitViolationLimitExceeded       = "limit_exceeded"
	PlanFitViolationAuthorityIncomplete = "authority_incomplete"

	PlanFitScopeAccount   = "account"
	PlanFitScopeRealm     = "realm"
	PlanFitScopeAgent     = "agent"
	PlanFitScopeAuthority = "authority"
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

// CheckAccountPlanFit compares canonical durable account usage with every
// finite storage/count limit understood by the target snapshot. The complete
// read runs in one repeatable-read, read-only transaction. It never trims,
// disables, retires, or otherwise mutates tenant state.
func (s *Store) CheckAccountPlanFit(
	ctx context.Context,
	accountID string,
	target AccountPlanFitTarget,
) (AccountPlanFitReport, error) {
	if err := validateAccountPlanFitTarget(target); err != nil {
		return AccountPlanFitReport{}, err
	}
	accountID = strings.TrimSpace(accountID)
	if accountID == "" {
		return AccountPlanFitReport{}, ErrAccountNotFound
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{
		IsoLevel:   pgx.RepeatableRead,
		AccessMode: pgx.ReadOnly,
	})
	if err != nil {
		return AccountPlanFitReport{}, fmt.Errorf("begin account plan-fit read: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var accountStatus string
	var retainedAttachmentBytes int64
	err = tx.QueryRow(ctx, `
		SELECT status,retained_agent_email_attachment_bytes
		  FROM accounts
		 WHERE id=$1`, accountID).Scan(&accountStatus, &retainedAttachmentBytes)
	if errors.Is(err, pgx.ErrNoRows) {
		return AccountPlanFitReport{}, ErrAccountNotFound
	}
	if err != nil {
		return AccountPlanFitReport{}, fmt.Errorf("read account plan-fit state: %w", err)
	}
	if accountStatus != "active" {
		return AccountPlanFitReport{}, ErrAccountNotActive
	}

	report := AccountPlanFitReport{
		AccountID:          accountID,
		TargetPlan:         target.Plan,
		TargetSnapshotHash: target.SnapshotHash,
		Violations:         []AccountPlanFitViolation{},
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

	if err := tx.Commit(ctx); err != nil {
		return AccountPlanFitReport{}, fmt.Errorf("commit account plan-fit read: %w", err)
	}
	return report, nil
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
