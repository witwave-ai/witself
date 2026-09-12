package store

import (
	"context"
	"fmt"
)

// SealedPlanePostureMetrics contains only cell-wide counts and whole-second
// ages. Agent delivery counts are reduced to a single maximum inside SQL.
type SealedPlanePostureMetrics struct {
	OpenRotations                  int64
	OldestOpenRotationSeconds      int64
	PendingEnrollments             int64
	OldestPendingEnrollmentSeconds int64
	MaxAgentDeliveries15m          int64
}

// ReadSealedPlanePostureMetrics reads a single statement snapshot. Pending
// enrollments exclude elapsed expirations even before lazy expiry runs. Ages
// are floored to whole seconds and clamped at zero for future creation times.
func (s *Store) ReadSealedPlanePostureMetrics(ctx context.Context) (SealedPlanePostureMetrics, error) {
	var m SealedPlanePostureMetrics
	// Each account's time-bounded delivery lookup uses the existing
	// usage_events_by_account_dimension_time leading columns. A global time
	// predicate alone would scan historical events on every metrics scrape.
	err := s.pool.QueryRow(ctx, `
		SELECT rotations.open_count, rotations.oldest_seconds,
		       enrollments.pending_count, enrollments.oldest_seconds,
		       deliveries.max_agent_count
		FROM (
			SELECT count(*) AS open_count,
			       GREATEST(COALESCE(FLOOR(EXTRACT(EPOCH FROM
			         statement_timestamp() - min(created_at))), 0), 0)::bigint AS oldest_seconds
			FROM agent_vault_key_rotations
			WHERE lifecycle_state = 'open'
		) rotations
		CROSS JOIN (
			SELECT count(*) AS pending_count,
			       GREATEST(COALESCE(FLOOR(EXTRACT(EPOCH FROM
			         statement_timestamp() - min(created_at))), 0), 0)::bigint AS oldest_seconds
			FROM agent_vault_key_enrollments
			WHERE lifecycle_state IN ('pending', 'approved')
			  AND expires_at > statement_timestamp()
		) enrollments
		CROSS JOIN (
			SELECT COALESCE(max(agent_deliveries.delivery_count), 0) AS max_agent_count
			FROM accounts
			CROSS JOIN LATERAL (
				SELECT count(*) AS delivery_count
				FROM usage_events
				WHERE account_id = accounts.id AND dimension = 'secret_read'
				  AND occurred_at > statement_timestamp() - interval '15 minutes'
				GROUP BY agent_id
			) agent_deliveries
		) deliveries`,
	).Scan(&m.OpenRotations, &m.OldestOpenRotationSeconds, &m.PendingEnrollments,
		&m.OldestPendingEnrollmentSeconds, &m.MaxAgentDeliveries15m)
	if err != nil {
		return SealedPlanePostureMetrics{}, fmt.Errorf("read sealed-plane posture metrics: %w", err)
	}
	return m, nil
}
