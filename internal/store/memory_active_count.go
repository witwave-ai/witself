package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// memoryActiveCountTx reads the exact derived count for an owner. An owner with
// no memory clock has never written a memory and therefore has zero active
// heads. Mutating callers still lock or create the owner clock before enforcing
// capacity; read-only plan previews may use this helper without materializing a
// row.
func memoryActiveCountTx(ctx context.Context, tx pgx.Tx, p Principal) (int64, error) {
	var count int64
	err := tx.QueryRow(ctx, `
		SELECT active_memory_count
		  FROM memory_change_clocks
		 WHERE account_id=$1 AND realm_id=$2
		   AND owner_kind='agent' AND owner_id=$3`,
		p.AccountID, p.RealmID, p.ID,
	).Scan(&count)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("read active memory count: %w", err)
	}
	return count, nil
}

// adjustActiveMemoryCountTx applies one mutation's net active-head delta to an
// owner clock the caller has already locked. The bounded conditional update
// fails closed on a missing clock, underflow, or overflow.
func adjustActiveMemoryCountTx(
	ctx context.Context,
	tx pgx.Tx,
	p Principal,
	delta int64,
) error {
	if delta == 0 {
		return nil
	}
	tag, err := tx.Exec(ctx, `
		UPDATE memory_change_clocks
		   SET active_memory_count =
		         (active_memory_count::numeric + $4::bigint)::bigint
		 WHERE account_id=$1 AND realm_id=$2
		   AND owner_kind='agent' AND owner_id=$3
		   AND active_memory_count::numeric + $4::bigint
		         BETWEEN 0 AND $5`,
		p.AccountID, p.RealmID, p.ID, delta, maxMemoryChangeSeq,
	)
	if err != nil {
		return fmt.Errorf("adjust active memory count: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return ErrMemoryConflict
	}
	return nil
}

// memoryCurationPlanActiveDelta returns the net active-head change of one
// normalized stored plan. Replace preserves an active head; create adds one;
// supersede removes one. Create outputs referenced by supersede actions are
// therefore counted exactly once.
func memoryCurationPlanActiveDelta(stored memoryCurationStoredPlan) int64 {
	var delta int64
	for _, row := range stored.Actions {
		switch row.Action.Operation {
		case MemoryCurationOperationCreate:
			delta++
		case MemoryCurationOperationSupersede:
			delta--
		}
	}
	return delta
}

// recomputeActiveMemoryCountsTx rebuilds the derived counter after archive
// rows land. It deliberately leaves clock timestamps and sequence positions
// untouched so a cell move preserves canonical mutation history.
func recomputeActiveMemoryCountsTx(
	ctx context.Context,
	tx pgx.Tx,
	accountID string,
) error {
	if _, err := tx.Exec(ctx, `
		UPDATE memory_change_clocks
		   SET active_memory_count=0
		 WHERE account_id=$1`,
		accountID,
	); err != nil {
		return fmt.Errorf("reset imported active memory counts: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		WITH active_counts AS (
		  SELECT m.account_id, m.realm_id, m.owner_kind, m.owner_id,
		         count(*)::bigint AS active_memory_count
		    FROM memories m
		    JOIN memory_versions v
		      ON v.memory_id=m.id AND v.version=m.current_version
		   WHERE m.account_id=$1
		     AND m.current_version IS NOT NULL
		     AND v.state='active'
		   GROUP BY m.account_id, m.realm_id, m.owner_kind, m.owner_id
		)
		UPDATE memory_change_clocks clock
		   SET active_memory_count=active.active_memory_count
		  FROM active_counts active
		 WHERE clock.account_id=active.account_id
		   AND clock.realm_id=active.realm_id
		   AND clock.owner_kind=active.owner_kind
		   AND clock.owner_id=active.owner_id`,
		accountID,
	); err != nil {
		return fmt.Errorf("recompute imported active memory counts: %w", err)
	}

	var missingClockCount, mismatchedCount int64
	if err := tx.QueryRow(ctx, `
		SELECT
		  (SELECT count(*)
		     FROM memories m
		     JOIN memory_versions v
		       ON v.memory_id=m.id AND v.version=m.current_version
		     LEFT JOIN memory_change_clocks clock
		       ON clock.account_id=m.account_id
		      AND clock.realm_id=m.realm_id
		      AND clock.owner_kind=m.owner_kind
		      AND clock.owner_id=m.owner_id
		    WHERE m.account_id=$1 AND m.current_version IS NOT NULL
		      AND v.state='active' AND clock.owner_id IS NULL),
		  (SELECT count(*)
		     FROM memory_change_clocks clock
		    WHERE clock.account_id=$1
		      AND clock.active_memory_count <> (
		        SELECT count(*)
		          FROM memories m
		          JOIN memory_versions v
		            ON v.memory_id=m.id AND v.version=m.current_version
		         WHERE m.account_id=clock.account_id
		           AND m.realm_id=clock.realm_id
		           AND m.owner_kind=clock.owner_kind
		           AND m.owner_id=clock.owner_id
		           AND m.current_version IS NOT NULL
		           AND v.state='active'
		      ))`,
		accountID,
	).Scan(&missingClockCount, &mismatchedCount); err != nil {
		return fmt.Errorf("validate imported active memory counts: %w", err)
	}
	if missingClockCount != 0 || mismatchedCount != 0 {
		return fmt.Errorf(
			"active memory count invariant: %d active owners lack clocks and %d clocks mismatch",
			missingClockCount, mismatchedCount,
		)
	}
	return nil
}
