package store

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// IdentityCapacityDimensionMetrics aggregates account capacity without
// returning tenant identifiers, plan names, or individual usage values.
type IdentityCapacityDimensionMetrics struct {
	AccountsMeasured  int64
	AccountsNearLimit int64
	AccountsAtLimit   int64
	AccountsUnlimited int64
	MinHeadroomRatio  float64
}

// IdentityCapacityMetrics has exactly the three supported identity dimensions.
type IdentityCapacityMetrics struct {
	Realms         IdentityCapacityDimensionMetrics
	AgentsPerRealm IdentityCapacityDimensionMetrics
	OperatorSeats  IdentityCapacityDimensionMetrics
}

// ReadIdentityCapacityMetrics measures active, non-purged accounts, matching
// identity-create eligibility. Each account contributes once per dimension;
// agents_per_realm uses its busiest live realm. Missing limits are unlimited.
// Zero is a finite cap with no headroom and is both near and at its limit.
func (s *Store) ReadIdentityCapacityMetrics(ctx context.Context) (IdentityCapacityMetrics, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{
		IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly,
	})
	if err != nil {
		return IdentityCapacityMetrics{}, fmt.Errorf("begin identity capacity metrics: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SET LOCAL statement_timeout = '2s'`); err != nil {
		return IdentityCapacityMetrics{}, fmt.Errorf("bound identity capacity metrics: %w", err)
	}
	rows, err := tx.Query(ctx, `
		WITH live_accounts AS (
		  SELECT id, plan_limits FROM accounts
		   WHERE status = 'active' AND purged_at IS NULL
		), live_realm_agents AS (
		  SELECT r.account_id, r.id, count(a.id) AS agents
		    FROM realms r
		    JOIN live_accounts account_record ON account_record.id = r.account_id
		    LEFT JOIN agents a ON a.realm_id = r.id AND a.deleted_at IS NULL
		   WHERE r.deleted_at IS NULL
		   GROUP BY r.account_id, r.id
		), realm_counts AS (
		  SELECT account_id, count(*) AS realms, max(agents) AS agents_per_realm
		    FROM live_realm_agents GROUP BY account_id
		), operator_counts AS (
		  SELECT o.account_id, count(*) AS operator_seats
		    FROM operators o
		    JOIN live_accounts account_record ON account_record.id = o.account_id
		   WHERE o.deleted_at IS NULL GROUP BY o.account_id
		), capacities AS (
		  SELECT dimension, used, (account_record.plan_limits->>dimension)::bigint AS cap
		    FROM live_accounts account_record
		    LEFT JOIN realm_counts r ON r.account_id = account_record.id
		    LEFT JOIN operator_counts o ON o.account_id = account_record.id
		    CROSS JOIN LATERAL (VALUES
		      ('realms', COALESCE(r.realms, 0)),
		      ('agents_per_realm', COALESCE(r.agents_per_realm, 0)),
		      ('operator_seats', COALESCE(o.operator_seats, 0))
		    ) dimensions(dimension, used)
		)
		SELECT dimension,
		       count(*) FILTER (WHERE cap IS NOT NULL),
		       count(*) FILTER (WHERE cap IS NOT NULL AND used::numeric >= cap::numeric * 0.8),
		       count(*) FILTER (WHERE cap IS NOT NULL AND used >= cap),
		       count(*) FILTER (WHERE cap IS NULL),
		       COALESCE(min(CASE WHEN cap = 0 THEN 0::double precision
		         ELSE greatest(0, least(1, (cap - used)::double precision / cap))
		         END) FILTER (WHERE cap IS NOT NULL), 1::double precision)
		  FROM capacities GROUP BY dimension`)
	if err != nil {
		return IdentityCapacityMetrics{}, fmt.Errorf("query identity capacity metrics: %w", err)
	}
	defer rows.Close()
	// An empty cell has no finite-limit accounts and therefore full headroom.
	metrics := IdentityCapacityMetrics{
		Realms:         IdentityCapacityDimensionMetrics{MinHeadroomRatio: 1},
		AgentsPerRealm: IdentityCapacityDimensionMetrics{MinHeadroomRatio: 1},
		OperatorSeats:  IdentityCapacityDimensionMetrics{MinHeadroomRatio: 1},
	}
	for rows.Next() {
		var dimension string
		var value IdentityCapacityDimensionMetrics
		if err := rows.Scan(&dimension, &value.AccountsMeasured,
			&value.AccountsNearLimit, &value.AccountsAtLimit,
			&value.AccountsUnlimited, &value.MinHeadroomRatio); err != nil {
			return IdentityCapacityMetrics{}, fmt.Errorf("scan identity capacity metrics: %w", err)
		}
		switch dimension {
		case "realms":
			metrics.Realms = value
		case "agents_per_realm":
			metrics.AgentsPerRealm = value
		case "operator_seats":
			metrics.OperatorSeats = value
		}
	}
	if err := rows.Err(); err != nil {
		return IdentityCapacityMetrics{}, fmt.Errorf("read identity capacity metrics: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return IdentityCapacityMetrics{}, fmt.Errorf("commit identity capacity metrics: %w", err)
	}
	return metrics, nil
}
