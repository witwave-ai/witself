package store

import (
	"context"
	"errors"
	"fmt"
	"time"
)

const (
	// TranscriptRetentionDaysPolicy is the cell-side policy key for a finite
	// account transcript-retention window. Absence means indefinite retention.
	TranscriptRetentionDaysPolicy       = "transcript_retention_days"
	defaultTranscriptRetentionBatchSize = 100
	maxTranscriptRetentionBatchSize     = 1000
	defaultTranscriptRetentionInterval  = 5 * time.Minute
)

// TranscriptRetentionMode selects read-only preview or destructive
// enforcement for the cell worker.
type TranscriptRetentionMode string

const (
	// TranscriptRetentionModePreview runs the production eligibility and hold
	// query without deleting conversations.
	TranscriptRetentionModePreview TranscriptRetentionMode = "preview"
	// TranscriptRetentionModeEnforce deletes the bounded set selected by the
	// production eligibility and hold query.
	TranscriptRetentionModeEnforce TranscriptRetentionMode = "enforce"
)

// TranscriptRetentionBatchResult is value-free operational observability.
// Scanned is the raw candidate-key count and never exceeds the configured
// batch size. SkippedLocked counts candidates that could not be locked and
// revalidated. Deferred counts are a bounded sample of held conversations;
// when ScanCapped is true they are a lower bound, not a full inventory.
type TranscriptRetentionBatchResult struct {
	Scanned                int64
	SkippedLocked          int64
	ScanCapped             bool
	Eligible               int64
	EligibleScanCapped     bool
	Deleted                int64
	DeferredEvidence       int64
	DeferredCuration       int64
	DeferredScanCapped     bool
	ReleasedCurationInputs int64
	DeletedCurationCursors int64
}

// TranscriptRetentionWorkerConfig controls one cell worker's bounded batch
// size, cadence, and preview/enforcement mode.
type TranscriptRetentionWorkerConfig struct {
	BatchSize int
	Interval  time.Duration
	Mode      TranscriptRetentionMode
}

// DefaultTranscriptRetentionWorkerConfig returns the conservative production
// defaults: preview 100 conversations every five minutes.
func DefaultTranscriptRetentionWorkerConfig() TranscriptRetentionWorkerConfig {
	return TranscriptRetentionWorkerConfig{
		BatchSize: defaultTranscriptRetentionBatchSize,
		Interval:  defaultTranscriptRetentionInterval,
		Mode:      TranscriptRetentionModePreview,
	}
}

// Validate checks the worker's bounded operational settings.
func (c TranscriptRetentionWorkerConfig) Validate() error {
	if c.BatchSize < 1 || c.BatchSize > maxTranscriptRetentionBatchSize {
		return fmt.Errorf("transcript retention batch size must be between 1 and %d", maxTranscriptRetentionBatchSize)
	}
	if c.Interval < time.Minute || c.Interval > 24*time.Hour {
		return errors.New("transcript retention interval must be between 1 minute and 24 hours")
	}
	switch c.Mode {
	case TranscriptRetentionModePreview, TranscriptRetentionModeEnforce:
	default:
		return fmt.Errorf("transcript retention mode must be %q or %q",
			TranscriptRetentionModePreview, TranscriptRetentionModeEnforce)
	}
	return nil
}

// ProcessTranscriptRetentionBatch removes at most batchSize whole
// conversations whose last activity is outside their account's resolved
// retention window. Missing policy means indefinite retention.
//
// Evidence and active frozen curation inputs are provenance holds, so those
// conversations are reported and deferred. Terminal curation inputs retain
// their immutable rows and counters but their transcript pointers are marked
// pruned before the conversation is deleted.
//
// Durable, mode-separated cursors select a wraparound page of policy accounts
// and give each selected account an exact share of the candidate budget.
// Candidate keys are limited before provenance checks or row locks, so held or
// busy backlogs cannot turn a batch into an unbounded scan. Fixed finite cycle
// cutoffs and per-account keyset progress ensure held rows are revisited while
// continuous new expirations cannot prevent wraparound. Account locks prevent
// a concurrent policy change from racing a delete under the old window.
// Cutoffs use the PostgreSQL statement clock so host drift cannot delete early.
func (s *Store) ProcessTranscriptRetentionBatch(
	ctx context.Context,
	batchSize int,
) (TranscriptRetentionBatchResult, error) {
	return s.processTranscriptRetentionBatch(ctx, batchSize, true, 0)
}

// PreviewTranscriptRetentionBatch runs the same bounded eligibility, hold, and
// locking query as ProcessTranscriptRetentionBatch but performs no deletes.
// Eligible reports the number that the matching enforcement batch would
// attempt to remove. The result contains counts only.
func (s *Store) PreviewTranscriptRetentionBatch(
	ctx context.Context,
	batchSize int,
) (TranscriptRetentionBatchResult, error) {
	return s.processTranscriptRetentionBatch(ctx, batchSize, false, 0)
}

func (s *Store) processTranscriptRetentionBatch(
	ctx context.Context,
	batchSize int,
	enforce bool,
	workerInterval time.Duration,
) (TranscriptRetentionBatchResult, error) {
	if batchSize < 1 || batchSize > maxTranscriptRetentionBatchSize {
		return TranscriptRetentionBatchResult{}, fmt.Errorf(
			"transcript retention batch size must be between 1 and %d",
			maxTranscriptRetentionBatchSize,
		)
	}
	if workerInterval < 0 {
		return TranscriptRetentionBatchResult{}, errors.New(
			"transcript retention worker interval cannot be negative",
		)
	}
	mode := TranscriptRetentionModePreview
	if enforce {
		mode = TranscriptRetentionModeEnforce
	}
	var result TranscriptRetentionBatchResult
	var lockedCandidateCount int64
	var advancedSweepCount int64
	var advancedAccountScanCount int64
	var sweepStateExists bool
	var scanCapped bool
	err := s.pool.QueryRow(ctx, `
WITH sweep_state_presence AS MATERIALIZED (
  SELECT EXISTS (
    SELECT 1 FROM transcript_retention_sweep_state WHERE singleton
  ) AS value
),
sweep_state AS MATERIALIZED (
  SELECT
    CASE $4
      WHEN 'preview' THEN preview_account_cursor
      ELSE enforce_account_cursor
    END AS account_cursor,
    generation,
    next_run_at
    FROM transcript_retention_sweep_state
   WHERE singleton
     AND (
       $3::bigint = 0
       OR next_run_at <= statement_timestamp()
     )
   FOR UPDATE SKIP LOCKED
),
account_page AS MATERIALIZED (
  SELECT page.id, page.segment
    FROM (
      (
        SELECT a.id, 0 AS segment
          FROM accounts a
          CROSS JOIN sweep_state sweep
         WHERE a.plan_policies ? 'transcript_retention_days'
           AND a.id > sweep.account_cursor
         ORDER BY a.id
         LIMIT $1
      )
      UNION ALL
      (
        SELECT a.id, 1 AS segment
          FROM accounts a
          CROSS JOIN sweep_state sweep
         WHERE a.plan_policies ? 'transcript_retention_days'
           AND a.id <= sweep.account_cursor
         ORDER BY a.id
         LIMIT $1
      )
    ) page
   ORDER BY page.segment, page.id
   LIMIT $1
),
locked_accounts AS MATERIALIZED (
  SELECT
    a.id,
    (a.plan_policies ->> 'transcript_retention_days')::integer AS retention_days,
    page.segment
    FROM account_page page
    JOIN accounts a ON a.id = page.id
   WHERE a.plan_policies ? 'transcript_retention_days'
   ORDER BY page.segment, page.id
   FOR UPDATE OF a SKIP LOCKED
),
selected_accounts AS MATERIALIZED (
  SELECT
    locked.*,
    row_number() OVER (ORDER BY locked.segment, locked.id) AS account_ordinal
    FROM locked_accounts locked
),
selected_account_count AS MATERIALIZED (
  SELECT count(*) AS value FROM selected_accounts
),
account_scans AS MATERIALIZED (
  SELECT
    a.id AS account_id,
    a.retention_days,
    a.segment,
    a.account_ordinal,
    CASE
      WHEN state.retention_days = a.retention_days
        THEN state.cycle_cutoff
      ELSE statement_timestamp() - make_interval(days => a.retention_days)
    END AS cycle_cutoff,
    CASE
      WHEN state.retention_days = a.retention_days
        THEN state.last_activity_at
      ELSE NULL
    END AS last_activity_at,
    CASE
      WHEN state.retention_days = a.retention_days
        THEN state.last_conversation_id
      ELSE NULL
    END AS last_conversation_id,
    COALESCE(state.generation, 0) AS generation,
    (
      $1::bigint / NULLIF(selected.value, 0)
      + CASE
          WHEN a.account_ordinal <=
               ($1::bigint % NULLIF(selected.value, 0))
            THEN 1
          ELSE 0
        END
    ) AS quota
    FROM selected_accounts a
    CROSS JOIN selected_account_count selected
    LEFT JOIN transcript_retention_account_scan_state state
      ON state.mode = $4
     AND state.account_id = a.id
),
raw_candidates AS MATERIALIZED (
  SELECT
    scan.account_id,
    scan.account_ordinal,
    picked.id,
    picked.realm_id,
    picked.owner_agent_id,
    picked.updated_at
    FROM account_scans scan
    CROSS JOIN LATERAL (
      SELECT c.id, c.realm_id, c.owner_agent_id, c.updated_at
        FROM transcript_conversations c
       WHERE c.account_id = scan.account_id
         AND c.updated_at < LEAST(
               scan.cycle_cutoff,
               statement_timestamp() -
                 make_interval(days => scan.retention_days)
             )
         AND (
           scan.last_activity_at IS NULL
           OR c.updated_at > scan.last_activity_at
           OR (
             c.updated_at = scan.last_activity_at
             AND c.id < scan.last_conversation_id
           )
         )
       ORDER BY c.updated_at, c.id DESC
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
         account_id, updated_at, id
    FROM raw_candidates
   ORDER BY account_id, updated_at DESC, id
),
locked_candidates AS MATERIALIZED (
  SELECT
    c.id,
    c.account_id,
    c.realm_id,
    c.owner_agent_id,
    c.updated_at,
    raw.account_ordinal
    FROM raw_candidates raw
    JOIN transcript_conversations c
      ON c.id = raw.id
     AND c.account_id = raw.account_id
    JOIN account_scans scan
      ON scan.account_id = raw.account_id
   WHERE c.updated_at = raw.updated_at
     AND c.updated_at < statement_timestamp() -
         make_interval(days => scan.retention_days)
   ORDER BY raw.account_ordinal, c.updated_at, c.id DESC
   FOR UPDATE OF c SKIP LOCKED
),
classified_candidates AS MATERIALIZED (
  SELECT
    candidate.*,
    EXISTS (
      SELECT 1
        FROM memory_evidence me
       WHERE me.source_transcript_id = candidate.id
    ) AS has_evidence,
    (
      EXISTS (
        SELECT 1
          FROM memory_curation_run_inputs mci
          JOIN memory_curation_runs mcr ON mcr.id = mci.run_id
         WHERE mci.transcript_id = candidate.id
           AND mcr.state IN ('open', 'planned')
      )
      OR EXISTS (
        SELECT 1
          FROM memory_curation_run_inputs mci
          JOIN memory_curation_runs mcr ON mcr.id = mci.run_id
         WHERE mci.input_kind = 'cursor'
           AND mci.cursor_source_kind = 'transcript'
           AND mci.cursor_stream_id = candidate.id
           AND mcr.state IN ('open', 'planned')
      )
    ) AS has_curation
    FROM locked_candidates candidate
),
eligible AS MATERIALIZED (
  SELECT id, account_id, realm_id, owner_agent_id
    FROM classified_candidates
   WHERE NOT has_evidence
     AND NOT has_curation
),
scan_progress AS MATERIALIZED (
  SELECT
    scan.account_id,
    scan.retention_days,
    scan.quota,
    scan.cycle_cutoff,
    scan.generation,
    COALESCE(raw_count.value, 0) AS raw_count,
    last_raw.updated_at AS last_activity_at,
    last_raw.id AS last_conversation_id
    FROM account_scans scan
    LEFT JOIN raw_counts raw_count
      ON raw_count.account_id = scan.account_id
    LEFT JOIN last_raw_candidates last_raw
      ON last_raw.account_id = scan.account_id
),
advanced_account_scans AS (
  INSERT INTO transcript_retention_account_scan_state
    (mode, account_id, retention_days, cycle_cutoff,
     last_activity_at, last_conversation_id, generation, updated_at)
  SELECT
    $4,
    progress.account_id,
    progress.retention_days,
    CASE
      WHEN progress.raw_count < progress.quota
        THEN statement_timestamp() -
             make_interval(days => progress.retention_days)
      ELSE progress.cycle_cutoff
    END,
    CASE
      WHEN progress.raw_count < progress.quota THEN NULL
      ELSE progress.last_activity_at
    END,
    CASE
      WHEN progress.raw_count < progress.quota THEN NULL
      ELSE progress.last_conversation_id
    END,
    CASE
      WHEN progress.generation = 4611686018427387903 THEN 1
      ELSE progress.generation + 1
    END,
    statement_timestamp()
    FROM scan_progress progress
  ON CONFLICT (mode, account_id) DO UPDATE
    SET retention_days = EXCLUDED.retention_days,
        cycle_cutoff = EXCLUDED.cycle_cutoff,
        last_activity_at = EXCLUDED.last_activity_at,
        last_conversation_id = EXCLUDED.last_conversation_id,
        generation = EXCLUDED.generation,
        updated_at = EXCLUDED.updated_at
  RETURNING account_id
),
advanced_sweep AS (
  UPDATE transcript_retention_sweep_state state
     SET preview_account_cursor = CASE
           WHEN $4 = 'preview' THEN COALESCE(
             (
               SELECT page.id
                 FROM account_page page
                ORDER BY page.segment DESC, page.id DESC
                LIMIT 1
             ),
             state.preview_account_cursor
           )
           ELSE state.preview_account_cursor
         END,
         enforce_account_cursor = CASE
           WHEN $4 = 'enforce' THEN COALESCE(
             (
               SELECT page.id
                 FROM account_page page
                ORDER BY page.segment DESC, page.id DESC
                LIMIT 1
             ),
             state.enforce_account_cursor
           )
           ELSE state.enforce_account_cursor
         END,
         generation = CASE
           WHEN state.generation = 4611686018427387903 THEN 1
           ELSE state.generation + 1
         END,
         next_run_at = CASE
           WHEN $3::bigint > 0
             THEN statement_timestamp() +
                  ($3::bigint * interval '1 microsecond')
           ELSE state.next_run_at
         END,
         updated_at = statement_timestamp()
    FROM sweep_state locked
   WHERE state.singleton
  RETURNING state.generation
),
detached_curation_inputs AS (
  UPDATE memory_curation_run_inputs mci
     SET transcript_id = CASE
           WHEN mci.input_kind IN ('transcript', 'transcript_coverage')
             THEN NULL
           ELSE mci.transcript_id
         END,
         cursor_stream_id = CASE
           WHEN mci.input_kind = 'cursor'
            AND mci.cursor_source_kind = 'transcript'
             THEN NULL
           ELSE mci.cursor_stream_id
         END,
         transcript_pruned_at = statement_timestamp()
    FROM eligible e, memory_curation_runs mcr
   WHERE (
           mci.transcript_id = e.id
           OR (
             mci.input_kind = 'cursor'
             AND mci.cursor_source_kind = 'transcript'
             AND mci.cursor_stream_id = e.id
           )
         )
     AND mcr.id = mci.run_id
     AND mcr.state NOT IN ('open', 'planned')
     AND $2::boolean
  RETURNING mci.run_id
),
deleted_curation_cursors AS (
  DELETE FROM memory_curation_cursors cursor
   USING eligible e
   WHERE cursor.account_id = e.account_id
     AND cursor.realm_id = e.realm_id
     AND cursor.owner_kind = 'agent'
     AND cursor.owner_id = e.owner_agent_id
     AND cursor.source_kind = 'transcript'
     AND cursor.source_stream_id = e.id
     AND $2::boolean
  RETURNING cursor.source_stream_id
),
deleted AS (
  DELETE FROM transcript_conversations c
   USING eligible e
   WHERE c.id = e.id
     AND c.account_id = e.account_id
     AND $2::boolean
  RETURNING c.id
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
  (SELECT count(*) FROM deleted),
  (SELECT count(*) FROM classified_candidates WHERE has_evidence),
  (SELECT count(*) FROM classified_candidates WHERE NOT has_evidence AND has_curation),
  (SELECT count(*) FROM detached_curation_inputs),
  (SELECT count(*) FROM deleted_curation_cursors),
  (SELECT count(*) FROM advanced_account_scans),
  (SELECT count(*) FROM advanced_sweep),
  (SELECT value FROM sweep_state_presence)
`, batchSize, enforce, workerInterval.Microseconds(), string(mode)).Scan(
		&result.Scanned,
		&lockedCandidateCount,
		&scanCapped,
		&result.Eligible,
		&result.Deleted,
		&result.DeferredEvidence,
		&result.DeferredCuration,
		&result.ReleasedCurationInputs,
		&result.DeletedCurationCursors,
		&advancedAccountScanCount,
		&advancedSweepCount,
		&sweepStateExists,
	)
	if err != nil {
		return TranscriptRetentionBatchResult{}, fmt.Errorf("process transcript retention batch: %w", err)
	}
	if advancedSweepCount != 1 {
		if sweepStateExists {
			// Another replica owns the singleton sweep row. SKIP LOCKED makes
			// this invocation an immediate no-op instead of queueing a burst of
			// duplicate-tick batches behind the winner.
			return TranscriptRetentionBatchResult{}, nil
		}
		return TranscriptRetentionBatchResult{}, errors.New(
			"process transcript retention batch: sweep state is missing",
		)
	}
	if advancedAccountScanCount > int64(batchSize) {
		return TranscriptRetentionBatchResult{}, errors.New(
			"process transcript retention batch: account scan bound exceeded",
		)
	}
	result.SkippedLocked = result.Scanned - lockedCandidateCount
	result.ScanCapped = scanCapped
	result.EligibleScanCapped = scanCapped
	result.DeferredScanCapped = scanCapped
	return result, nil
}

// RunTranscriptRetentionWorker attempts one bounded batch immediately and on
// each local interval. The cell-local next-run fence admits at most one
// successful worker batch per configured interval across staggered replicas
// and restarts. Direct public preview/process calls intentionally bypass that
// cadence fence for tests and explicit operator runs.
func (s *Store) RunTranscriptRetentionWorker(
	ctx context.Context,
	cfg TranscriptRetentionWorkerConfig,
	onResult func(TranscriptRetentionBatchResult),
	onError func(error),
) error {
	if err := cfg.Validate(); err != nil {
		return err
	}
	run := func() {
		var result TranscriptRetentionBatchResult
		var err error
		switch cfg.Mode {
		case TranscriptRetentionModePreview:
			result, err = s.processTranscriptRetentionBatch(
				ctx, cfg.BatchSize, false, cfg.Interval,
			)
		case TranscriptRetentionModeEnforce:
			result, err = s.processTranscriptRetentionBatch(
				ctx, cfg.BatchSize, true, cfg.Interval,
			)
		}
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
