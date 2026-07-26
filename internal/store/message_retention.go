package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/witwave-ai/witself/internal/plans"
)

const (
	// MessageRetentionDaysPolicy is the resolved cell-side account policy key.
	// Absence means indefinite retention; it is intentionally independent from
	// the messaging feature entitlement.
	MessageRetentionDaysPolicy = plans.MessageRetentionDaysPolicy

	defaultMessageRetentionBatchSize       = 25
	maxMessageRetentionBatchSize           = 100
	defaultMessageRetentionInterval        = 5 * time.Minute
	defaultMessageRetentionBatchTimeout    = 2 * time.Minute
	minMessageRetentionBatchTimeout        = 10 * time.Second
	maxMessageRetentionBatchTimeout        = 5 * time.Minute
	defaultMessageRetentionWorkerLaneCount = 16
	// Cleanup bounds each individual thread graph before it takes graph locks
	// or executes cascades. Pathological graphs are durably quarantined and
	// surfaced instead of monopolizing one lane forever.
	maxMessageRetentionThreadMessages  = 4096
	maxMessageRetentionThreadGraphRows = 4096
	maxMessageRetentionBatchGraphRows  = 65536
)

// MessageRetentionMode selects read-only preview or destructive enforcement.
type MessageRetentionMode string

const (
	// MessageRetentionModePreview computes eligibility without deleting.
	MessageRetentionModePreview MessageRetentionMode = "preview"
	// MessageRetentionModeEnforce deletes eligible whole inactive threads.
	MessageRetentionModeEnforce MessageRetentionMode = "enforce"
)

// MessageRetentionBatchResult contains value-free operational counts only.
// A unit of eligibility and deletion is a whole inactive thread.
type MessageRetentionBatchResult struct {
	Scanned          int64
	SkippedLocked    int64
	ScanCapped       bool
	EligibleThreads  int64
	DeletedThreads   int64
	DeletedMessages  int64
	DeferredEvidence int64
	DeferredActive   int64
	DeferredLocked   int64
	DeferredOversize int64
	DeferredBudget   int64
	RepairedActivity int64
	LaneAdvanced     bool
}

// MessageRetentionWorkerConfig controls one process-local loop. LaneCount is
// fixed by the schema and is deliberately not a replica/concurrency setting.
type MessageRetentionWorkerConfig struct {
	BatchSize    int
	Interval     time.Duration
	BatchTimeout time.Duration
	LaneCount    int
	Mode         MessageRetentionMode
}

// DefaultMessageRetentionWorkerConfig returns conservative preview defaults.
func DefaultMessageRetentionWorkerConfig() MessageRetentionWorkerConfig {
	return MessageRetentionWorkerConfig{
		BatchSize:    defaultMessageRetentionBatchSize,
		Interval:     defaultMessageRetentionInterval,
		BatchTimeout: defaultMessageRetentionBatchTimeout,
		LaneCount:    defaultMessageRetentionWorkerLaneCount,
		Mode:         MessageRetentionModePreview,
	}
}

// Validate checks bounded operational settings.
func (c MessageRetentionWorkerConfig) Validate() error {
	if c.BatchSize < 1 || c.BatchSize > maxMessageRetentionBatchSize {
		return fmt.Errorf(
			"message retention batch size must be between 1 and %d",
			maxMessageRetentionBatchSize,
		)
	}
	if c.Interval < time.Minute || c.Interval > 24*time.Hour {
		return errors.New("message retention interval must be between 1 minute and 24 hours")
	}
	if c.BatchTimeout < minMessageRetentionBatchTimeout ||
		c.BatchTimeout > maxMessageRetentionBatchTimeout {
		return fmt.Errorf(
			"message retention batch timeout must be between %s and %s",
			minMessageRetentionBatchTimeout,
			maxMessageRetentionBatchTimeout,
		)
	}
	if c.LaneCount != defaultMessageRetentionWorkerLaneCount {
		return fmt.Errorf(
			"message retention worker lane count must be %d",
			defaultMessageRetentionWorkerLaneCount,
		)
	}
	switch c.Mode {
	case MessageRetentionModePreview, MessageRetentionModeEnforce:
	default:
		return fmt.Errorf(
			"message retention mode must be %q or %q",
			MessageRetentionModePreview,
			MessageRetentionModeEnforce,
		)
	}
	return nil
}

// PreviewMessageRetentionBatch runs production eligibility and lock checks
// without deleting messages. Repeated direct calls rotate across the durable
// lanes but do not impose the scheduled worker interval.
func (s *Store) PreviewMessageRetentionBatch(
	ctx context.Context,
	batchSize int,
) (MessageRetentionBatchResult, error) {
	return s.processMessageRetentionBatch(
		ctx, batchSize, false, 0, defaultMessageRetentionBatchTimeout,
		defaultMessageRetentionWorkerLaneCount,
	)
}

// ProcessMessageRetentionBatch deletes at most batchSize eligible whole
// threads. Missing message_retention_days means indefinite retention.
func (s *Store) ProcessMessageRetentionBatch(
	ctx context.Context,
	batchSize int,
) (MessageRetentionBatchResult, error) {
	return s.processMessageRetentionBatch(
		ctx, batchSize, true, 0, defaultMessageRetentionBatchTimeout,
		defaultMessageRetentionWorkerLaneCount,
	)
}

type messageRetentionLaneClaim struct {
	LaneID        int
	AccountCursor string
	Generation    int64
}

// claimMessageRetentionLane durably leases one due lane before the potentially
// expensive graph transaction begins. A timeout or process crash therefore
// backs off that exact lane while other replicas continue with later lanes.
func (s *Store) claimMessageRetentionLane(
	ctx context.Context,
	mode MessageRetentionMode,
	workerInterval time.Duration,
	batchTimeout time.Duration,
	workerLaneCount int,
) (*messageRetentionLaneClaim, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin message retention lane claim: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var laneCount int64
	if err := tx.QueryRow(ctx, `
		SELECT count(*)
		  FROM message_retention_worker_lanes
		 WHERE mode=$1 AND lane_id < $2`,
		string(mode),
		workerLaneCount,
	).Scan(&laneCount); err != nil {
		return nil, fmt.Errorf("count message retention worker lanes: %w", err)
	}
	if laneCount != int64(workerLaneCount) {
		return nil, fmt.Errorf(
			"message retention worker lane set has %d rows, want %d",
			laneCount,
			workerLaneCount,
		)
	}

	var claim messageRetentionLaneClaim
	var previousGeneration int64
	err = tx.QueryRow(ctx, `
		SELECT lane_id,account_cursor,generation
		  FROM message_retention_worker_lanes
		 WHERE mode=$1
		   AND lane_id < $2
		   AND next_run_at <= statement_timestamp()
		 ORDER BY next_run_at,lane_id
		 LIMIT 1
		 FOR UPDATE SKIP LOCKED`,
		string(mode),
		workerLaneCount,
	).Scan(&claim.LaneID, &claim.AccountCursor, &previousGeneration)
	if errors.Is(err, pgx.ErrNoRows) {
		if err := tx.Commit(ctx); err != nil {
			return nil, fmt.Errorf(
				"commit empty message retention lane claim: %w",
				err,
			)
		}
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("select message retention worker lane: %w", err)
	}
	claim.Generation = previousGeneration + 1
	if previousGeneration == 4611686018427387903 {
		claim.Generation = 1
	}
	lease := batchTimeout + workerInterval
	command, err := tx.Exec(ctx, `
		UPDATE message_retention_worker_lanes
		   SET generation=$4,
		       next_run_at=statement_timestamp() +
		         ($5::bigint * interval '1 microsecond'),
		       updated_at=statement_timestamp()
		 WHERE mode=$1 AND lane_id=$2 AND generation=$3`,
		string(mode),
		claim.LaneID,
		previousGeneration,
		claim.Generation,
		lease.Microseconds(),
	)
	if err != nil {
		return nil, fmt.Errorf("lease message retention worker lane: %w", err)
	}
	if command.RowsAffected() != 1 {
		return nil, errors.New("lease message retention worker lane lost its fence")
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit message retention lane claim: %w", err)
	}
	return &claim, nil
}

// processMessageRetentionBatch owns one due durable lane and one bounded
// age-first thread page. The per-thread activity row is locked before the
// messages: its BEFORE INSERT trigger makes that lock a fence against new
// content. Account FOR SHARE locks make a concurrent policy update serialize
// before deletion. Every graph relation uses SKIP LOCKED plus exact
// completeness accounting, so cleanup never waits behind foreground work
// after taking the activity fence.
func (s *Store) processMessageRetentionBatch(
	ctx context.Context,
	batchSize int,
	enforce bool,
	workerInterval time.Duration,
	batchTimeout time.Duration,
	workerLaneCount int,
) (MessageRetentionBatchResult, error) {
	return s.processMessageRetentionBatchWithGraphBudget(
		ctx,
		batchSize,
		enforce,
		workerInterval,
		batchTimeout,
		workerLaneCount,
		maxMessageRetentionBatchGraphRows,
	)
}

// processMessageRetentionBatchWithGraphBudget keeps the production graph
// ceiling injectable only for bounded integration tests.
func (s *Store) processMessageRetentionBatchWithGraphBudget(
	ctx context.Context,
	batchSize int,
	enforce bool,
	workerInterval time.Duration,
	batchTimeout time.Duration,
	workerLaneCount int,
	batchGraphRowBudget int,
) (MessageRetentionBatchResult, error) {
	if batchSize < 1 || batchSize > maxMessageRetentionBatchSize {
		return MessageRetentionBatchResult{}, fmt.Errorf(
			"message retention batch size must be between 1 and %d",
			maxMessageRetentionBatchSize,
		)
	}
	if workerInterval < 0 {
		return MessageRetentionBatchResult{},
			errors.New("message retention worker interval cannot be negative")
	}
	if batchTimeout < minMessageRetentionBatchTimeout ||
		batchTimeout > maxMessageRetentionBatchTimeout {
		return MessageRetentionBatchResult{}, fmt.Errorf(
			"message retention batch timeout must be between %s and %s",
			minMessageRetentionBatchTimeout,
			maxMessageRetentionBatchTimeout,
		)
	}
	if workerLaneCount != defaultMessageRetentionWorkerLaneCount {
		return MessageRetentionBatchResult{}, fmt.Errorf(
			"message retention worker lane count must be %d",
			defaultMessageRetentionWorkerLaneCount,
		)
	}
	if batchGraphRowBudget < 1 ||
		batchGraphRowBudget > maxMessageRetentionBatchGraphRows {
		return MessageRetentionBatchResult{}, fmt.Errorf(
			"message retention batch graph row budget must be between 1 and %d",
			maxMessageRetentionBatchGraphRows,
		)
	}
	mode := MessageRetentionModePreview
	if enforce {
		mode = MessageRetentionModeEnforce
	}
	claim, err := s.claimMessageRetentionLane(
		ctx,
		mode,
		workerInterval,
		batchTimeout,
		workerLaneCount,
	)
	if err != nil {
		return MessageRetentionBatchResult{}, err
	}
	if claim == nil {
		return MessageRetentionBatchResult{}, nil
	}
	return s.processClaimedMessageRetentionBatch(
		ctx,
		batchSize,
		enforce,
		workerInterval,
		workerLaneCount,
		batchGraphRowBudget,
		mode,
		*claim,
	)
}

// processClaimedMessageRetentionBatch applies one exact durable lane
// generation. A worker that pauses between the short claim transaction and
// this heavier transaction cannot mutate state after another worker takes over
// the expired lease with a newer generation.
func (s *Store) processClaimedMessageRetentionBatch(
	ctx context.Context,
	batchSize int,
	enforce bool,
	workerInterval time.Duration,
	workerLaneCount int,
	batchGraphRowBudget int,
	mode MessageRetentionMode,
	claim messageRetentionLaneClaim,
) (MessageRetentionBatchResult, error) {
	var result MessageRetentionBatchResult
	var lockedCandidateCount int64
	var advancedAccountScanCount int64
	var advancedLaneCount int64
	var laneCount int64
	var fencedGraphRowCount int64
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return MessageRetentionBatchResult{},
			fmt.Errorf("begin message retention batch: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	// Keep message_retention_days literal in the account predicates. PostgreSQL
	// can then prove they imply the migration's partial worker-lane index even
	// when pgx chooses a generic prepared plan.
	err = tx.QueryRow(ctx, `
WITH lane_presence AS MATERIALIZED (
  SELECT count(*) AS value
    FROM message_retention_worker_lanes
   WHERE mode = $4
     AND lane_id < $5::integer
),
worker_sweep_state AS MATERIALIZED (
  SELECT mode, lane_id, account_cursor, generation, next_run_at
    FROM message_retention_worker_lanes
    CROSS JOIN lane_presence
   WHERE mode = $4
     AND lane_id = $8::integer
     AND generation = $9::bigint
     AND lane_presence.value = $5::integer
   FOR UPDATE
),
account_page AS MATERIALIZED (
  SELECT page.id, page.segment
    FROM (
      (
        SELECT account.id, 0 AS segment
          FROM accounts account
          CROSS JOIN worker_sweep_state sweep
         WHERE account.plan_policies ? 'message_retention_days'
           AND get_byte(sha256(account.id::bytea), 0) % 16 =
               sweep.lane_id
           AND account.id > sweep.account_cursor
         ORDER BY account.id
         LIMIT $1
      )
      UNION ALL
      (
        SELECT account.id, 1 AS segment
          FROM accounts account
          CROSS JOIN worker_sweep_state sweep
         WHERE account.plan_policies ? 'message_retention_days'
           AND get_byte(sha256(account.id::bytea), 0) % 16 =
               sweep.lane_id
           AND account.id <= sweep.account_cursor
         ORDER BY account.id
         LIMIT $1
      )
    ) page
   ORDER BY page.segment, page.id
   LIMIT $1
),
locked_accounts AS MATERIALIZED (
  SELECT
    account.id,
    (account.plan_policies ->> 'message_retention_days')::integer AS retention_days,
    page.segment
    FROM account_page page
    JOIN accounts account ON account.id = page.id
   WHERE account.plan_policies ? 'message_retention_days'
   ORDER BY page.segment, account.id
   FOR SHARE OF account SKIP LOCKED
),
selected_accounts AS MATERIALIZED (
  SELECT
    locked.*,
    row_number() OVER (
      ORDER BY locked.segment, locked.id
    ) AS account_ordinal
    FROM locked_accounts locked
),
selected_account_count AS MATERIALIZED (
  SELECT count(*) AS value FROM selected_accounts
),
account_scans AS MATERIALIZED (
  SELECT
    account.id AS account_id,
    account.retention_days,
    account.segment,
    account.account_ordinal,
    CASE
      WHEN state.retention_days = account.retention_days
        THEN state.cycle_cutoff
      ELSE statement_timestamp() -
           make_interval(days => account.retention_days)
    END AS cycle_cutoff,
    CASE
      WHEN state.retention_days = account.retention_days
        THEN state.last_activity_at
      ELSE NULL
    END AS last_activity_at,
    CASE
      WHEN state.retention_days = account.retention_days
        THEN state.last_realm_id
      ELSE NULL
    END AS last_realm_id,
    CASE
      WHEN state.retention_days = account.retention_days
        THEN state.last_thread_id
      ELSE NULL
    END AS last_thread_id,
    COALESCE(state.generation, 0) AS generation,
    (
      $1::bigint / NULLIF(selected.value, 0)
      + CASE
          WHEN account.account_ordinal <=
               ($1::bigint % NULLIF(selected.value, 0))
            THEN 1
          ELSE 0
        END
    ) AS quota
    FROM selected_accounts account
    CROSS JOIN selected_account_count selected
    LEFT JOIN message_retention_account_scan_state state
      ON state.mode = $4
     AND state.account_id = account.id
),
raw_candidates AS MATERIALIZED (
  SELECT
    scan.account_id,
    scan.account_ordinal,
    picked.realm_id,
    picked.thread_id,
    picked.last_message_at
    FROM account_scans scan
    CROSS JOIN LATERAL (
      SELECT
        activity.realm_id,
        activity.thread_id,
        activity.last_message_at
        FROM message_retention_thread_activity activity
       WHERE activity.account_id = scan.account_id
         AND activity.retry_after <= statement_timestamp()
         AND activity.last_message_at < LEAST(
               scan.cycle_cutoff,
               statement_timestamp() -
                 make_interval(days => scan.retention_days)
             )
         AND (
           scan.last_activity_at IS NULL
           OR activity.last_message_at > scan.last_activity_at
           OR (
             activity.last_message_at = scan.last_activity_at
             AND activity.realm_id > scan.last_realm_id
           )
           OR (
             activity.last_message_at = scan.last_activity_at
             AND activity.realm_id = scan.last_realm_id
             AND activity.thread_id > scan.last_thread_id
           )
         )
       ORDER BY activity.last_message_at, activity.realm_id,
                activity.thread_id
       LIMIT scan.quota
    ) picked
),
raw_counts AS MATERIALIZED (
  SELECT account_id, count(*) AS value
    FROM raw_candidates
   GROUP BY account_id
),
last_raw_candidates AS MATERIALIZED (
  SELECT DISTINCT ON (account_id)
         account_id, last_message_at, realm_id, thread_id
    FROM raw_candidates
   ORDER BY account_id, last_message_at DESC, realm_id DESC, thread_id DESC
),
locked_candidates AS MATERIALIZED (
  SELECT
    activity.account_id,
    activity.realm_id,
    activity.thread_id,
    activity.last_message_at,
    scan.retention_days
    FROM raw_candidates raw
    JOIN account_scans scan ON scan.account_id = raw.account_id
    JOIN message_retention_thread_activity activity
      ON activity.account_id = raw.account_id
     AND activity.realm_id = raw.realm_id
     AND activity.thread_id = raw.thread_id
   WHERE activity.last_message_at = raw.last_message_at
     AND activity.last_message_at <
         statement_timestamp() -
         make_interval(days => scan.retention_days)
   ORDER BY raw.account_ordinal, activity.last_message_at,
            activity.realm_id, activity.thread_id
   FOR UPDATE OF activity SKIP LOCKED
),
scan_progress AS MATERIALIZED (
  SELECT
    scan.account_id,
    scan.retention_days,
    scan.quota,
    scan.cycle_cutoff,
    scan.generation,
    COALESCE(raw_count.value, 0) AS raw_count,
    last_raw.last_message_at,
    last_raw.realm_id,
    last_raw.thread_id
    FROM account_scans scan
    LEFT JOIN raw_counts raw_count
      ON raw_count.account_id = scan.account_id
    LEFT JOIN last_raw_candidates last_raw
      ON last_raw.account_id = scan.account_id
),
all_messages AS MATERIALIZED (
  SELECT
    candidate.account_id,
    candidate.realm_id,
    candidate.thread_id,
    message.id,
    message.created_at
    FROM locked_candidates candidate
    CROSS JOIN LATERAL (
      SELECT source.id, source.created_at
        FROM agent_messages source
       WHERE source.account_id = candidate.account_id
         AND source.realm_id = candidate.realm_id
         AND source.thread_id = candidate.thread_id
       ORDER BY source.created_at DESC, source.id DESC
       LIMIT ($6::integer + 1)
    ) message
),
message_counts AS MATERIALIZED (
  SELECT account_id, realm_id, thread_id, count(*) AS value,
         max(created_at) AS latest_message_at
    FROM all_messages
   GROUP BY account_id, realm_id, thread_id
),
message_sized_candidates AS MATERIALIZED (
  SELECT candidate.*
    FROM locked_candidates candidate
    LEFT JOIN message_counts counts
      ON counts.account_id = candidate.account_id
     AND counts.realm_id = candidate.realm_id
     AND counts.thread_id = candidate.thread_id
   WHERE COALESCE(counts.value, 0) <= $6::integer
),
all_deliveries AS MATERIALIZED (
  SELECT
    candidate.account_id,
    candidate.realm_id,
    candidate.thread_id,
    delivery.message_id,
    delivery.recipient_agent_id,
    delivery.processing_state,
    delivery.lease_expires_at
    FROM message_sized_candidates candidate
    CROSS JOIN LATERAL (
      SELECT source.message_id, source.recipient_agent_id,
             source.processing_state, source.lease_expires_at
        FROM all_messages message
        JOIN agent_message_deliveries source
          ON source.message_id = message.id
         AND source.account_id = message.account_id
         AND source.realm_id = message.realm_id
       WHERE message.account_id = candidate.account_id
         AND message.realm_id = candidate.realm_id
         AND message.thread_id = candidate.thread_id
       ORDER BY source.message_id, source.recipient_agent_id
       LIMIT ($7::integer + 1)
    ) delivery
),
all_requests AS MATERIALIZED (
  SELECT
    candidate.account_id,
    candidate.realm_id,
    candidate.thread_id,
    request.id,
    request.opening_message_id,
    request.state,
    request.expires_at
    FROM message_sized_candidates candidate
    CROSS JOIN LATERAL (
      SELECT source.id, source.opening_message_id, source.state,
             source.expires_at
        FROM all_messages opening
        JOIN agent_message_requests source
          ON source.opening_message_id = opening.id
         AND source.account_id = opening.account_id
         AND source.realm_id = opening.realm_id
       WHERE opening.account_id = candidate.account_id
         AND opening.realm_id = candidate.realm_id
         AND opening.thread_id = candidate.thread_id
       ORDER BY source.id
       LIMIT ($7::integer + 1)
    ) request
),
all_request_candidates AS MATERIALIZED (
  SELECT
    candidate.account_id,
    candidate.realm_id,
    candidate.thread_id,
    request_candidate.request_id,
    request_candidate.agent_id
    FROM message_sized_candidates candidate
    CROSS JOIN LATERAL (
      SELECT source.request_id, source.agent_id
        FROM all_requests request
        JOIN agent_message_request_candidates source
          ON source.request_id = request.id
         AND source.account_id = request.account_id
         AND source.realm_id = request.realm_id
       WHERE request.account_id = candidate.account_id
         AND request.realm_id = candidate.realm_id
         AND request.thread_id = candidate.thread_id
       ORDER BY source.request_id, source.agent_id
       LIMIT ($7::integer + 1)
    ) request_candidate
),
all_request_selections AS MATERIALIZED (
  SELECT
    candidate.account_id,
    candidate.realm_id,
    candidate.thread_id,
    selection.id
    FROM message_sized_candidates candidate
    CROSS JOIN LATERAL (
      SELECT source.id
        FROM all_requests request
        JOIN agent_message_request_selections source
          ON source.request_id = request.id
         AND source.account_id = request.account_id
         AND source.realm_id = request.realm_id
       WHERE request.account_id = candidate.account_id
         AND request.realm_id = candidate.realm_id
         AND request.thread_id = candidate.thread_id
       ORDER BY source.id
       LIMIT ($7::integer + 1)
    ) selection
),
all_request_claims AS MATERIALIZED (
  SELECT
    candidate.account_id,
    candidate.realm_id,
    candidate.thread_id,
    claim.id,
    claim.state,
    claim.lease_expires_at
    FROM message_sized_candidates candidate
    CROSS JOIN LATERAL (
      SELECT source.id, source.state, source.lease_expires_at
        FROM all_requests request
        JOIN agent_message_request_claims source
          ON source.request_id = request.id
         AND source.account_id = request.account_id
         AND source.realm_id = request.realm_id
       WHERE request.account_id = candidate.account_id
         AND request.realm_id = candidate.realm_id
         AND request.thread_id = candidate.thread_id
       ORDER BY source.id
       LIMIT ($7::integer + 1)
    ) claim
),
delivery_counts AS MATERIALIZED (
  SELECT
    account_id,realm_id,thread_id,count(*) AS value,
    bool_or(
      processing_state = 'claimed'
      AND lease_expires_at > statement_timestamp()
    ) AS has_active
    FROM all_deliveries
   GROUP BY account_id,realm_id,thread_id
),
request_counts AS MATERIALIZED (
  SELECT
    account_id,realm_id,thread_id,count(*) AS value,
    bool_or(
      state = 'open'
      AND expires_at > statement_timestamp()
    ) AS has_active
    FROM all_requests
   GROUP BY account_id,realm_id,thread_id
),
request_candidate_counts AS MATERIALIZED (
  SELECT account_id,realm_id,thread_id,count(*) AS value
    FROM all_request_candidates
   GROUP BY account_id,realm_id,thread_id
),
selection_counts AS MATERIALIZED (
  SELECT account_id,realm_id,thread_id,count(*) AS value
    FROM all_request_selections
   GROUP BY account_id,realm_id,thread_id
),
claim_counts AS MATERIALIZED (
  SELECT
    account_id,realm_id,thread_id,count(*) AS value,
    bool_or(
      state IN ('reserved', 'claimed')
      AND lease_expires_at > statement_timestamp()
    ) AS has_active
    FROM all_request_claims
   GROUP BY account_id,realm_id,thread_id
),
evidence_threads AS MATERIALIZED (
  SELECT DISTINCT
    message.account_id,message.realm_id,message.thread_id
    FROM all_messages message
    JOIN LATERAL (
     SELECT true AS held
       FROM memory_evidence evidence
      WHERE evidence.source_message_id = message.id
      LIMIT 1
    ) evidence_hold ON evidence_hold.held
),
candidate_graph_sizes AS MATERIALIZED (
  SELECT
    candidate.*,
    COALESCE(messages.value, 0) AS message_count,
    COALESCE(deliveries.value, 0) AS delivery_count,
    COALESCE(requests.value, 0) AS request_count,
    COALESCE(request_candidates.value, 0) AS request_candidate_count,
    COALESCE(selections.value, 0) AS selection_count,
    COALESCE(claims.value, 0) AS claim_count,
    messages.latest_message_at,
    evidence.account_id IS NOT NULL AS has_evidence,
    COALESCE(deliveries.has_active, false)
      OR COALESCE(requests.has_active, false)
      OR COALESCE(claims.has_active, false) AS has_active_work
    FROM locked_candidates candidate
    LEFT JOIN message_counts messages
      ON messages.account_id = candidate.account_id
     AND messages.realm_id = candidate.realm_id
     AND messages.thread_id = candidate.thread_id
    LEFT JOIN delivery_counts deliveries
      ON deliveries.account_id = candidate.account_id
     AND deliveries.realm_id = candidate.realm_id
     AND deliveries.thread_id = candidate.thread_id
    LEFT JOIN request_counts requests
      ON requests.account_id = candidate.account_id
     AND requests.realm_id = candidate.realm_id
     AND requests.thread_id = candidate.thread_id
    LEFT JOIN request_candidate_counts request_candidates
      ON request_candidates.account_id = candidate.account_id
     AND request_candidates.realm_id = candidate.realm_id
     AND request_candidates.thread_id = candidate.thread_id
    LEFT JOIN selection_counts selections
      ON selections.account_id = candidate.account_id
     AND selections.realm_id = candidate.realm_id
     AND selections.thread_id = candidate.thread_id
    LEFT JOIN claim_counts claims
      ON claims.account_id = candidate.account_id
     AND claims.realm_id = candidate.realm_id
     AND claims.thread_id = candidate.thread_id
    LEFT JOIN evidence_threads evidence
      ON evidence.account_id = candidate.account_id
     AND evidence.realm_id = candidate.realm_id
     AND evidence.thread_id = candidate.thread_id
),
candidate_prechecks AS MATERIALIZED (
  SELECT
    candidate.*,
    candidate.message_count + candidate.delivery_count +
      candidate.request_count + candidate.request_candidate_count +
      candidate.selection_count + candidate.claim_count AS graph_row_count,
    (
      candidate.message_count > $6::integer
      OR candidate.delivery_count > $7::integer
      OR candidate.request_count > $7::integer
      OR candidate.request_candidate_count > $7::integer
      OR candidate.selection_count > $7::integer
      OR candidate.claim_count > $7::integer
      OR candidate.message_count + candidate.delivery_count +
           candidate.request_count + candidate.request_candidate_count +
           candidate.selection_count + candidate.claim_count > $7::integer
    ) AS is_oversize,
    COALESCE(
      candidate.latest_message_at >= statement_timestamp() -
        make_interval(days => candidate.retention_days),
      false
    ) AS has_recent_message
    FROM candidate_graph_sizes candidate
),
budget_ranked_candidates AS MATERIALIZED (
  SELECT
    candidate.*,
    sum(
      CASE
        WHEN NOT candidate.is_oversize
          AND NOT candidate.has_recent_message
          AND NOT candidate.has_evidence
          AND NOT candidate.has_active_work
          THEN candidate.graph_row_count
        ELSE 0
      END
    ) OVER (
      ORDER BY candidate.last_message_at,candidate.account_id,
               candidate.realm_id,candidate.thread_id
      ROWS BETWEEN UNBOUNDED PRECEDING AND CURRENT ROW
    ) AS cumulative_graph_rows
    FROM candidate_prechecks candidate
),
graph_sized_candidates AS MATERIALIZED (
  SELECT candidate.*
    FROM budget_ranked_candidates candidate
   WHERE NOT candidate.is_oversize
     AND NOT candidate.has_recent_message
     AND NOT candidate.has_evidence
     AND NOT candidate.has_active_work
     AND candidate.cumulative_graph_rows <= $10::bigint
),
locked_messages AS MATERIALIZED (
  SELECT
    message.id,
    message.account_id,
    message.realm_id,
    message.thread_id,
    message.created_at
    FROM graph_sized_candidates candidate
    JOIN all_messages snapshot
      ON snapshot.account_id = candidate.account_id
     AND snapshot.realm_id = candidate.realm_id
     AND snapshot.thread_id = candidate.thread_id
    JOIN agent_messages message
      ON message.id = snapshot.id
     AND message.account_id = snapshot.account_id
     AND message.realm_id = snapshot.realm_id
   ORDER BY message.account_id, message.realm_id, message.thread_id,
            message.created_at, message.id
   FOR UPDATE OF message SKIP LOCKED
),
locked_deliveries AS MATERIALIZED (
  SELECT
    delivery.message_id,
    delivery.recipient_agent_id,
    delivery.account_id,
    delivery.realm_id,
    snapshot.thread_id,
    delivery.processing_state,
    delivery.lease_expires_at
    FROM graph_sized_candidates candidate
    JOIN all_deliveries snapshot
      ON snapshot.account_id = candidate.account_id
     AND snapshot.realm_id = candidate.realm_id
     AND snapshot.thread_id = candidate.thread_id
    JOIN agent_message_deliveries delivery
      ON delivery.message_id = snapshot.message_id
     AND delivery.recipient_agent_id = snapshot.recipient_agent_id
   ORDER BY delivery.message_id, delivery.recipient_agent_id
   FOR UPDATE OF delivery SKIP LOCKED
),
locked_requests AS MATERIALIZED (
  SELECT
    request.id,
    request.account_id,
    request.realm_id,
    snapshot.thread_id,
    request.opening_message_id,
    request.state,
    request.expires_at
    FROM graph_sized_candidates candidate
    JOIN all_requests snapshot
      ON snapshot.account_id = candidate.account_id
     AND snapshot.realm_id = candidate.realm_id
     AND snapshot.thread_id = candidate.thread_id
    JOIN agent_message_requests request
      ON request.id = snapshot.id
     AND request.account_id = snapshot.account_id
     AND request.realm_id = snapshot.realm_id
   ORDER BY request.id
   FOR UPDATE OF request SKIP LOCKED
),
locked_request_candidates AS MATERIALIZED (
  SELECT
    request_candidate.request_id,
    request_candidate.agent_id,
    snapshot.account_id,
    snapshot.realm_id,
    snapshot.thread_id
    FROM graph_sized_candidates candidate
    JOIN all_request_candidates snapshot
      ON snapshot.account_id = candidate.account_id
     AND snapshot.realm_id = candidate.realm_id
     AND snapshot.thread_id = candidate.thread_id
    JOIN agent_message_request_candidates request_candidate
      ON request_candidate.request_id = snapshot.request_id
     AND request_candidate.agent_id = snapshot.agent_id
   ORDER BY request_candidate.request_id, request_candidate.agent_id
   FOR UPDATE OF request_candidate SKIP LOCKED
),
locked_request_selections AS MATERIALIZED (
  SELECT
    selection.id,
    snapshot.account_id,
    snapshot.realm_id,
    snapshot.thread_id
    FROM graph_sized_candidates candidate
    JOIN all_request_selections snapshot
      ON snapshot.account_id = candidate.account_id
     AND snapshot.realm_id = candidate.realm_id
     AND snapshot.thread_id = candidate.thread_id
    JOIN agent_message_request_selections selection
      ON selection.id = snapshot.id
   ORDER BY selection.id
   FOR UPDATE OF selection SKIP LOCKED
),
locked_request_claims AS MATERIALIZED (
  SELECT
    claim.id,
    claim.request_id,
    snapshot.account_id,
    snapshot.realm_id,
    snapshot.thread_id,
    claim.state,
    claim.lease_expires_at
    FROM graph_sized_candidates candidate
    JOIN all_request_claims snapshot
      ON snapshot.account_id = candidate.account_id
     AND snapshot.realm_id = candidate.realm_id
     AND snapshot.thread_id = candidate.thread_id
    JOIN agent_message_request_claims claim
      ON claim.id = snapshot.id
   ORDER BY claim.id
   FOR UPDATE OF claim SKIP LOCKED
),
locked_message_counts AS MATERIALIZED (
  SELECT account_id,realm_id,thread_id,count(*) AS value
    FROM locked_messages
   GROUP BY account_id,realm_id,thread_id
),
locked_delivery_counts AS MATERIALIZED (
  SELECT account_id,realm_id,thread_id,count(*) AS value
    FROM locked_deliveries
   GROUP BY account_id,realm_id,thread_id
),
locked_request_counts AS MATERIALIZED (
  SELECT account_id,realm_id,thread_id,count(*) AS value
    FROM locked_requests
   GROUP BY account_id,realm_id,thread_id
),
locked_request_candidate_counts AS MATERIALIZED (
  SELECT account_id,realm_id,thread_id,count(*) AS value
    FROM locked_request_candidates
   GROUP BY account_id,realm_id,thread_id
),
locked_selection_counts AS MATERIALIZED (
  SELECT account_id,realm_id,thread_id,count(*) AS value
    FROM locked_request_selections
   GROUP BY account_id,realm_id,thread_id
),
locked_claim_counts AS MATERIALIZED (
  SELECT account_id,realm_id,thread_id,count(*) AS value
    FROM locked_request_claims
   GROUP BY account_id,realm_id,thread_id
),
locked_graph_counts AS MATERIALIZED (
  SELECT
    candidate.account_id,
    candidate.realm_id,
    candidate.thread_id,
    COALESCE(messages.value, 0) AS message_count,
    COALESCE(deliveries.value, 0) AS delivery_count,
    COALESCE(requests.value, 0) AS request_count,
    COALESCE(request_candidates.value, 0) AS request_candidate_count,
    COALESCE(selections.value, 0) AS selection_count,
    COALESCE(claims.value, 0) AS claim_count
    FROM graph_sized_candidates candidate
    LEFT JOIN locked_message_counts messages
      ON messages.account_id = candidate.account_id
     AND messages.realm_id = candidate.realm_id
     AND messages.thread_id = candidate.thread_id
    LEFT JOIN locked_delivery_counts deliveries
      ON deliveries.account_id = candidate.account_id
     AND deliveries.realm_id = candidate.realm_id
     AND deliveries.thread_id = candidate.thread_id
    LEFT JOIN locked_request_counts requests
      ON requests.account_id = candidate.account_id
     AND requests.realm_id = candidate.realm_id
     AND requests.thread_id = candidate.thread_id
    LEFT JOIN locked_request_candidate_counts request_candidates
      ON request_candidates.account_id = candidate.account_id
     AND request_candidates.realm_id = candidate.realm_id
     AND request_candidates.thread_id = candidate.thread_id
    LEFT JOIN locked_selection_counts selections
      ON selections.account_id = candidate.account_id
     AND selections.realm_id = candidate.realm_id
     AND selections.thread_id = candidate.thread_id
    LEFT JOIN locked_claim_counts claims
      ON claims.account_id = candidate.account_id
     AND claims.realm_id = candidate.realm_id
     AND claims.thread_id = candidate.thread_id
),
classified_candidates AS MATERIALIZED (
  SELECT
    candidate.*,
    (
      NOT candidate.is_oversize
      AND NOT candidate.has_recent_message
      AND NOT candidate.has_evidence
      AND NOT candidate.has_active_work
      AND candidate.cumulative_graph_rows > $10::bigint
    ) AS is_budget_deferred,
    (
      locked.account_id IS NOT NULL
      AND (
        locked.message_count <> candidate.message_count
        OR locked.delivery_count <> candidate.delivery_count
        OR locked.request_count <> candidate.request_count
        OR locked.request_candidate_count <> candidate.request_candidate_count
        OR locked.selection_count <> candidate.selection_count
        OR locked.claim_count <> candidate.claim_count
      )
    ) AS has_graph_contention
    FROM budget_ranked_candidates candidate
    LEFT JOIN locked_graph_counts locked
      ON locked.account_id = candidate.account_id
     AND locked.realm_id = candidate.realm_id
     AND locked.thread_id = candidate.thread_id
),
first_budget_deferred AS MATERIALIZED (
  SELECT DISTINCT ON (account_id)
         account_id,last_message_at,realm_id,thread_id
    FROM classified_candidates
   WHERE is_budget_deferred
   ORDER BY account_id,last_message_at,realm_id,thread_id
),
last_progress_candidates AS MATERIALIZED (
  SELECT DISTINCT ON (raw.account_id)
         raw.account_id,
         raw.last_message_at,
         raw.realm_id,
         raw.thread_id
    FROM raw_candidates raw
    LEFT JOIN first_budget_deferred first_deferred
      ON first_deferred.account_id = raw.account_id
   WHERE first_deferred.account_id IS NULL
      OR ROW(raw.last_message_at,raw.realm_id,raw.thread_id) <
         ROW(first_deferred.last_message_at,
             first_deferred.realm_id,
             first_deferred.thread_id)
   ORDER BY raw.account_id,raw.last_message_at DESC,
            raw.realm_id DESC,raw.thread_id DESC
),
budget_scan_progress AS MATERIALIZED (
  SELECT
    progress.*,
    first_deferred.account_id IS NOT NULL AS budget_limited,
    last_progress.last_message_at AS progress_last_activity_at,
    last_progress.realm_id AS progress_last_realm_id,
    last_progress.thread_id AS progress_last_thread_id
    FROM scan_progress progress
    LEFT JOIN first_budget_deferred first_deferred
      ON first_deferred.account_id = progress.account_id
    LEFT JOIN last_progress_candidates last_progress
      ON last_progress.account_id = progress.account_id
),
advanced_account_scans AS (
  INSERT INTO message_retention_account_scan_state
    (mode, account_id, retention_days, cycle_cutoff,
     last_activity_at, last_realm_id, last_thread_id, generation, updated_at)
  SELECT
    $4,
    progress.account_id,
    progress.retention_days,
    CASE
      WHEN progress.budget_limited
        THEN progress.cycle_cutoff
      WHEN progress.raw_count < progress.quota
        THEN statement_timestamp() -
             make_interval(days => progress.retention_days)
      ELSE progress.cycle_cutoff
    END,
    CASE
      WHEN progress.budget_limited
        THEN progress.progress_last_activity_at
      WHEN progress.raw_count < progress.quota THEN NULL
      ELSE progress.last_message_at
    END,
    CASE
      WHEN progress.budget_limited
        THEN progress.progress_last_realm_id
      WHEN progress.raw_count < progress.quota THEN NULL
      ELSE progress.realm_id
    END,
    CASE
      WHEN progress.budget_limited
        THEN progress.progress_last_thread_id
      WHEN progress.raw_count < progress.quota THEN NULL
      ELSE progress.thread_id
    END,
    CASE
      WHEN progress.generation = 4611686018427387903 THEN 1
      ELSE progress.generation + 1
    END,
    statement_timestamp()
    FROM budget_scan_progress progress
   WHERE NOT progress.budget_limited
      OR progress.progress_last_activity_at IS NOT NULL
  ON CONFLICT (mode, account_id) DO UPDATE
    SET retention_days = EXCLUDED.retention_days,
        cycle_cutoff = EXCLUDED.cycle_cutoff,
        last_activity_at = EXCLUDED.last_activity_at,
        last_realm_id = EXCLUDED.last_realm_id,
        last_thread_id = EXCLUDED.last_thread_id,
        generation = EXCLUDED.generation,
        updated_at = EXCLUDED.updated_at
  RETURNING account_id
),
eligible AS MATERIALIZED (
  SELECT account_id, realm_id, thread_id
    FROM classified_candidates
   WHERE NOT is_oversize
     AND NOT is_budget_deferred
     AND NOT has_graph_contention
     AND NOT has_recent_message
     AND NOT has_evidence
     AND NOT has_active_work
),
deferred_activity AS (
  UPDATE message_retention_thread_activity activity
     SET last_message_at = GREATEST(
           activity.last_message_at,
           COALESCE(candidate.latest_message_at, activity.last_message_at)
         ),
         retry_after = CASE
           WHEN candidate.is_oversize
             THEN statement_timestamp() + interval '24 hours'
           ELSE '-infinity'::timestamptz
         END,
         defer_reason = CASE
           WHEN candidate.is_oversize THEN 'oversize'
           ELSE ''
         END,
         defer_count = CASE
           WHEN candidate.has_recent_message THEN 0
           WHEN NOT candidate.is_oversize THEN activity.defer_count
           WHEN activity.defer_count = 4611686018427387903 THEN 1
           ELSE activity.defer_count + 1
         END,
         updated_at = statement_timestamp()
    FROM classified_candidates candidate
   WHERE activity.account_id = candidate.account_id
     AND activity.realm_id = candidate.realm_id
     AND activity.thread_id = candidate.thread_id
     AND (
       candidate.is_oversize
       OR candidate.has_recent_message
     )
     AND ($2::boolean OR candidate.has_recent_message)
  RETURNING activity.thread_id
),
deleted_messages AS (
  DELETE FROM agent_messages message
   USING locked_messages locked, eligible thread
   WHERE message.id = locked.id
     AND message.account_id = thread.account_id
     AND message.realm_id = thread.realm_id
     AND message.thread_id = thread.thread_id
     AND $2::boolean
  RETURNING message.id
),
deleted_activity AS (
  DELETE FROM message_retention_thread_activity activity
   USING eligible thread
   WHERE activity.account_id = thread.account_id
     AND activity.realm_id = thread.realm_id
     AND activity.thread_id = thread.thread_id
     AND $2::boolean
  RETURNING activity.thread_id
),
advanced_lane AS (
  UPDATE message_retention_worker_lanes lane
     SET account_cursor = COALESCE(
           (
             SELECT page.id
               FROM account_page page
              ORDER BY page.segment DESC, page.id DESC
              LIMIT 1
         ),
         lane.account_cursor
         ),
         next_run_at = statement_timestamp() +
           ($3::bigint * interval '1 microsecond'),
         updated_at = statement_timestamp()
    FROM worker_sweep_state locked
   WHERE lane.mode = locked.mode
     AND lane.lane_id = locked.lane_id
     AND lane.generation = locked.generation
  RETURNING lane.lane_id
)
SELECT
  (SELECT count(*) FROM raw_candidates),
  (SELECT count(*) FROM locked_candidates),
  EXISTS (
    SELECT 1
      FROM scan_progress
     WHERE raw_count = quota
  ),
  (SELECT count(*) FROM eligible),
  (SELECT count(*) FROM deleted_activity),
  (SELECT count(*) FROM deleted_messages),
  (SELECT count(*) FROM classified_candidates
    WHERE NOT is_oversize
      AND NOT has_graph_contention
      AND NOT has_recent_message
      AND has_evidence),
  (SELECT count(*) FROM classified_candidates
    WHERE NOT is_oversize
      AND NOT has_graph_contention
      AND NOT has_recent_message
      AND NOT has_evidence
      AND has_active_work),
  (SELECT count(*) FROM classified_candidates
    WHERE NOT is_oversize AND has_graph_contention),
  (SELECT count(*) FROM classified_candidates WHERE is_oversize),
  (SELECT count(*) FROM classified_candidates WHERE is_budget_deferred),
  (SELECT count(*) FROM classified_candidates WHERE has_recent_message),
  (SELECT count(*) FROM advanced_account_scans),
  (SELECT count(*) FROM advanced_lane),
  (SELECT value FROM lane_presence),
  (SELECT count(*) FROM deferred_activity)
`, batchSize, enforce, workerInterval.Microseconds(), string(mode),
		workerLaneCount,
		maxMessageRetentionThreadMessages,
		maxMessageRetentionThreadGraphRows,
		claim.LaneID,
		claim.Generation,
		batchGraphRowBudget,
	).Scan(
		&result.Scanned,
		&lockedCandidateCount,
		&result.ScanCapped,
		&result.EligibleThreads,
		&result.DeletedThreads,
		&result.DeletedMessages,
		&result.DeferredEvidence,
		&result.DeferredActive,
		&result.DeferredLocked,
		&result.DeferredOversize,
		&result.DeferredBudget,
		&result.RepairedActivity,
		&advancedAccountScanCount,
		&advancedLaneCount,
		&laneCount,
		&fencedGraphRowCount,
	)
	if err != nil {
		return MessageRetentionBatchResult{},
			fmt.Errorf("process message retention batch: %w", err)
	}
	if laneCount != int64(workerLaneCount) {
		return MessageRetentionBatchResult{}, fmt.Errorf(
			"process message retention batch: worker lane set has %d rows, want %d",
			laneCount,
			workerLaneCount,
		)
	}
	if advancedLaneCount == 0 {
		// Every due lane is already owned by another replica, or no lane is due.
		return MessageRetentionBatchResult{}, nil
	}
	if advancedLaneCount != 1 {
		return MessageRetentionBatchResult{},
			errors.New("process message retention batch: advanced more than one worker lane")
	}
	if advancedAccountScanCount > int64(batchSize) {
		return MessageRetentionBatchResult{},
			errors.New("process message retention batch: account scan bound exceeded")
	}
	result.LaneAdvanced = true
	result.SkippedLocked = result.Scanned - lockedCandidateCount
	if err := tx.Commit(ctx); err != nil {
		return MessageRetentionBatchResult{},
			fmt.Errorf("commit message retention batch: %w", err)
	}
	return result, nil
}

// RunMessageRetentionWorker attempts a bounded non-empty lane immediately and
// on each interval. Separate replicas can claim separate due lanes.
func (s *Store) RunMessageRetentionWorker(
	ctx context.Context,
	cfg MessageRetentionWorkerConfig,
	onResult func(MessageRetentionBatchResult),
	onError func(error),
) error {
	if err := cfg.Validate(); err != nil {
		return err
	}
	run := func() {
		result, err := s.processMessageRetentionWorkerBatch(ctx, cfg)
		if err != nil {
			if !errors.Is(err, context.Canceled) && onError != nil {
				onError(err)
			}
			return
		}
		if onResult != nil {
			onResult(result)
		}
	}
	run()
	ticker := time.NewTicker(cfg.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			run()
		}
	}
}

func (s *Store) processMessageRetentionWorkerBatch(
	ctx context.Context,
	cfg MessageRetentionWorkerConfig,
) (MessageRetentionBatchResult, error) {
	if err := cfg.Validate(); err != nil {
		return MessageRetentionBatchResult{}, err
	}
	attemptCtx, cancelAttempt := context.WithTimeout(ctx, cfg.BatchTimeout)
	defer cancelAttempt()
	var result MessageRetentionBatchResult
	for attempt := 0; attempt < cfg.LaneCount; attempt++ {
		var err error
		switch cfg.Mode {
		case MessageRetentionModePreview:
			result, err = s.processMessageRetentionBatch(
				attemptCtx,
				cfg.BatchSize,
				false,
				cfg.Interval,
				cfg.BatchTimeout,
				cfg.LaneCount,
			)
		case MessageRetentionModeEnforce:
			result, err = s.processMessageRetentionBatch(
				attemptCtx,
				cfg.BatchSize,
				true,
				cfg.Interval,
				cfg.BatchTimeout,
				cfg.LaneCount,
			)
		}
		if err != nil || result.Scanned > 0 || !result.LaneAdvanced {
			return result, err
		}
	}
	return MessageRetentionBatchResult{}, nil
}
