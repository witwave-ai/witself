package store

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
)

const maxActiveFactCount int64 = 1<<62 - 1

// adjustActiveFactCountTx applies one canonical fact mutation's net delta.
// The caller already holds the owner agent row FOR UPDATE. The bounded update
// fails closed on a missing agent, underflow, or overflow.
func adjustActiveFactCountTx(
	ctx context.Context,
	tx pgx.Tx,
	p Principal,
	delta int64,
) error {
	if delta == 0 {
		return nil
	}
	tag, err := tx.Exec(ctx, `
		UPDATE agents
		   SET active_fact_count =
		         (active_fact_count::numeric + $4::bigint)::bigint
		 WHERE id=$1 AND realm_id=$2
		   AND EXISTS (
		     SELECT 1 FROM realms r
		      WHERE r.id=$2 AND r.account_id=$3 AND r.deleted_at IS NULL
		   )
		   AND deleted_at IS NULL
		   AND active_fact_count::numeric + $4::bigint
		         BETWEEN 0 AND $5`,
		p.ID, p.RealmID, p.AccountID, delta, maxActiveFactCount,
	)
	if err != nil {
		return fmt.Errorf("adjust active fact count: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return ErrFactConflict
	}
	return nil
}

// recomputeActiveFactCountsTx rebuilds the derived projection after an
// account archive lands. Canonical fact rows remain the portable truth.
func recomputeActiveFactCountsTx(
	ctx context.Context,
	tx pgx.Tx,
	accountID string,
) error {
	if _, err := tx.Exec(ctx, `
		UPDATE agents agent
		   SET active_fact_count=0
		  FROM realms realm
		 WHERE agent.realm_id=realm.id
		   AND realm.account_id=$1`,
		accountID,
	); err != nil {
		return fmt.Errorf("reset imported active fact counts: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		WITH active_counts AS (
		  SELECT f.owner_agent_id, count(*)::bigint AS active_fact_count
		    FROM facts f
		   WHERE f.account_id=$1
		     AND f.deleted_at IS NULL
		     AND f.resolved_assertion_id IS NOT NULL
		   GROUP BY f.owner_agent_id
		)
		UPDATE agents agent
		   SET active_fact_count=active.active_fact_count
		  FROM active_counts active
		 WHERE agent.id=active.owner_agent_id`,
		accountID,
	); err != nil {
		return fmt.Errorf("recompute imported active fact counts: %w", err)
	}

	var mismatchedCount int64
	if err := tx.QueryRow(ctx, `
		SELECT count(*)
		  FROM agents agent
		  JOIN realms realm ON realm.id=agent.realm_id
		 WHERE realm.account_id=$1
		   AND agent.active_fact_count <> (
		     SELECT count(*)
		       FROM facts fact
		      WHERE fact.account_id=$1
		        AND fact.owner_agent_id=agent.id
		        AND fact.deleted_at IS NULL
		        AND fact.resolved_assertion_id IS NOT NULL
		   )`,
		accountID,
	).Scan(&mismatchedCount); err != nil {
		return fmt.Errorf("validate imported active fact counts: %w", err)
	}
	if mismatchedCount != 0 {
		return fmt.Errorf("active fact count invariant: %d agent counters mismatch", mismatchedCount)
	}
	return nil
}
