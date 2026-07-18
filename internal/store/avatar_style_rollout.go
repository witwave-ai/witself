package store

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
)

const (
	defaultAvatarStyleRolloutBatchSize    = 100
	maxAvatarStyleRolloutBatchSize        = 1000
	maxAvatarStyleRolloutCandidates       = 32
	defaultAvatarStyleRolloutInterval     = 2 * time.Second
	minAvatarStyleRolloutInterval         = 100 * time.Millisecond
	maxAvatarStyleRolloutInterval         = time.Hour
	defaultAvatarStyleRolloutBatchTimeout = 30 * time.Second
	minAvatarStyleRolloutBatchTimeout     = 100 * time.Millisecond
	maxAvatarStyleRolloutBatchTimeout     = 5 * time.Minute
)

// AvatarStyleRolloutWorkerConfig bounds one worker's transaction size and
// cadence. Multiple server replicas may run the same worker safely; the
// durable job row is the cross-replica fence.
type AvatarStyleRolloutWorkerConfig struct {
	BatchSize    int
	Interval     time.Duration
	BatchTimeout time.Duration
}

// DefaultAvatarStyleRolloutWorkerConfig returns conservative production
// defaults. A batch is one transaction and at most one batch runs per tick.
func DefaultAvatarStyleRolloutWorkerConfig() AvatarStyleRolloutWorkerConfig {
	return AvatarStyleRolloutWorkerConfig{
		BatchSize:    defaultAvatarStyleRolloutBatchSize,
		Interval:     defaultAvatarStyleRolloutInterval,
		BatchTimeout: defaultAvatarStyleRolloutBatchTimeout,
	}
}

// Validate rejects unbounded or busy-loop worker settings.
func (c AvatarStyleRolloutWorkerConfig) Validate() error {
	if c.BatchSize < 1 || c.BatchSize > maxAvatarStyleRolloutBatchSize {
		return fmt.Errorf("avatar style rollout batch size must be between 1 and %d", maxAvatarStyleRolloutBatchSize)
	}
	if c.Interval < minAvatarStyleRolloutInterval || c.Interval > maxAvatarStyleRolloutInterval {
		return fmt.Errorf("avatar style rollout interval must be between %s and %s", minAvatarStyleRolloutInterval, maxAvatarStyleRolloutInterval)
	}
	if c.BatchTimeout < minAvatarStyleRolloutBatchTimeout || c.BatchTimeout > maxAvatarStyleRolloutBatchTimeout {
		return fmt.Errorf("avatar style rollout batch timeout must be between %s and %s", minAvatarStyleRolloutBatchTimeout, maxAvatarStyleRolloutBatchTimeout)
	}
	return nil
}

// AvatarStyleRolloutBatchResult is value-free process observability for one
// bounded worker attempt.
type AvatarStyleRolloutBatchResult struct {
	Found             bool
	Paused            bool
	AccountID         string
	RealmID           string
	StyleRevision     int64
	ProcessedProfiles int
	Completed         bool
	Superseded        bool
}

// RunAvatarStyleRolloutWorker processes one bounded batch immediately and
// then one per interval until ctx is cancelled. Errors are reported and
// retried on the next tick; durable job state remains the source of truth.
func (s *Store) RunAvatarStyleRolloutWorker(
	ctx context.Context,
	cfg AvatarStyleRolloutWorkerConfig,
	onError func(error),
) error {
	if err := cfg.Validate(); err != nil {
		return err
	}
	run := func() {
		if _, err := s.processAvatarStyleRolloutBatch(ctx, cfg.BatchSize, cfg.BatchTimeout); err != nil &&
			!errors.Is(err, context.Canceled) && onError != nil {
			onError(err)
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

// ProcessAvatarStyleRolloutBatch advances at most one job and at most
// batchSize profile projection fences. Updated rows leave the indexed older-
// revision range, so crashes, deletes, and retries cannot create cursor gaps.
func (s *Store) ProcessAvatarStyleRolloutBatch(ctx context.Context, batchSize int) (AvatarStyleRolloutBatchResult, error) {
	return s.processAvatarStyleRolloutBatch(ctx, batchSize, defaultAvatarStyleRolloutBatchTimeout)
}

func (s *Store) processAvatarStyleRolloutBatch(
	ctx context.Context,
	batchSize int,
	batchTimeout time.Duration,
) (AvatarStyleRolloutBatchResult, error) {
	if batchSize < 1 || batchSize > maxAvatarStyleRolloutBatchSize {
		return AvatarStyleRolloutBatchResult{}, fmt.Errorf("avatar style rollout batch size must be between 1 and %d", maxAvatarStyleRolloutBatchSize)
	}
	if batchTimeout < minAvatarStyleRolloutBatchTimeout || batchTimeout > maxAvatarStyleRolloutBatchTimeout {
		return AvatarStyleRolloutBatchResult{}, fmt.Errorf("avatar style rollout batch timeout must be between %s and %s", minAvatarStyleRolloutBatchTimeout, maxAvatarStyleRolloutBatchTimeout)
	}
	attemptCtx, cancelAttempt := context.WithTimeout(ctx, batchTimeout)
	defer cancelAttempt()

	// Candidate discovery is deliberately unlocked and bounded. Each candidate
	// is then attempted in its own transaction using the same account -> job ->
	// selected-style lock order as publishing. Scanning several ordered rows
	// keeps one busy realm from head-of-line blocking unrelated work. The outer
	// attempt deadline caps the entire tick even if several candidates fail;
	// each candidate still gets its own cancellation scope.
	rows, err := s.pool.Query(attemptCtx, `
		SELECT j.account_id, j.realm_id, j.style_revision
		  FROM avatar_style_rollout_jobs j
		  JOIN accounts ac ON ac.id=j.account_id AND ac.status IN ('active','closed')
		  JOIN realms r ON r.id=j.realm_id AND r.account_id=j.account_id
		 WHERE j.status IN ('pending','running')
		   AND (ac.status='closed' OR j.retry_after IS NULL OR
		        j.retry_after <= statement_timestamp())
		 ORDER BY j.updated_at, j.created_at, j.account_id, j.realm_id, j.style_revision
		 LIMIT $1`, maxAvatarStyleRolloutCandidates)
	if err != nil {
		return AvatarStyleRolloutBatchResult{}, fmt.Errorf("find avatar style rollout: %w", err)
	}
	defer rows.Close()
	candidates := make([]AvatarStyleRolloutBatchResult, 0, maxAvatarStyleRolloutCandidates)
	for rows.Next() {
		var candidate AvatarStyleRolloutBatchResult
		if err := rows.Scan(&candidate.AccountID, &candidate.RealmID, &candidate.StyleRevision); err != nil {
			return AvatarStyleRolloutBatchResult{}, fmt.Errorf("scan avatar style rollout candidate: %w", err)
		}
		candidates = append(candidates, candidate)
	}
	if err := rows.Err(); err != nil {
		return AvatarStyleRolloutBatchResult{}, fmt.Errorf("list avatar style rollout candidates: %w", err)
	}
	rows.Close()

	var firstCandidateErr error
	for _, candidate := range candidates {
		candidateCtx, cancelCandidate := context.WithTimeout(attemptCtx, batchTimeout)
		result, err := s.processAvatarStyleRolloutCandidate(candidateCtx, batchSize, batchTimeout, candidate)
		cancelCandidate()
		if err != nil {
			// Caller cancellation is not a job failure and must not author durable
			// retry state during server shutdown or an abandoned request.
			if ctx.Err() != nil {
				return result, ctx.Err()
			}
			if !result.Found {
				return AvatarStyleRolloutBatchResult{}, err
			}
			failureCode := classifyAvatarStyleRolloutFailure(err)
			cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
			recordErr := s.recordAvatarStyleRolloutFailure(cleanupCtx, result, failureCode)
			cancel()
			if recordErr != nil {
				return AvatarStyleRolloutBatchResult{}, errors.Join(err, recordErr)
			}
			firstCandidateErr = errors.Join(firstCandidateErr, err)
			continue
		}
		if result.Found {
			// Earlier failed jobs already carry durable value-free backoff state.
			// Successful progress in this same tick remains a successful call.
			return result, nil
		}
	}
	return AvatarStyleRolloutBatchResult{}, firstCandidateErr
}

func (s *Store) processAvatarStyleRolloutCandidate(
	ctx context.Context,
	batchSize int,
	batchTimeout time.Duration,
	candidate AvatarStyleRolloutBatchResult,
) (AvatarStyleRolloutBatchResult, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return AvatarStyleRolloutBatchResult{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := setAvatarStyleRolloutTransactionTimeouts(ctx, tx, batchTimeout); err != nil {
		return AvatarStyleRolloutBatchResult{}, err
	}

	var accountStatus string
	err = tx.QueryRow(ctx, `SELECT status FROM accounts WHERE id=$1 FOR SHARE SKIP LOCKED`,
		candidate.AccountID).Scan(&accountStatus)
	if errors.Is(err, pgx.ErrNoRows) {
		// The account disappeared or an incompatible account mutation owns it.
		// Try another bounded candidate rather than waiting behind that account.
		return AvatarStyleRolloutBatchResult{}, nil
	}
	if err != nil {
		return AvatarStyleRolloutBatchResult{}, fmt.Errorf("lock rollout account: %w", err)
	}
	if accountStatus != "active" && accountStatus != "closed" {
		return AvatarStyleRolloutBatchResult{}, nil
	}

	var desiredPackID string
	var desiredPackVersion int
	err = tx.QueryRow(ctx, `
		SELECT style_pack_id, style_pack_version
		  FROM avatar_style_rollout_jobs
		 WHERE account_id=$1 AND realm_id=$2 AND style_revision=$3
		   AND status IN ('pending','running')
		 FOR UPDATE SKIP LOCKED`, candidate.AccountID, candidate.RealmID,
		candidate.StyleRevision).Scan(&desiredPackID, &desiredPackVersion)
	if errors.Is(err, pgx.ErrNoRows) {
		// Another replica owns the job or it completed after discovery. The
		// caller will try the next candidate from its bounded snapshot.
		return AvatarStyleRolloutBatchResult{}, nil
	}
	if err != nil {
		return AvatarStyleRolloutBatchResult{}, fmt.Errorf("lock avatar style rollout: %w", err)
	}
	candidate.Found = true
	if accountStatus == "closed" {
		// A pre-rollout binary may close an account during a mixed-version
		// deployment without knowing about the job table. Reconcile that
		// irreversible lifecycle terminal even though ordinary work discovery is
		// active-only, so the job cannot remain open forever.
		if err := supersedeAvatarStyleRolloutJobTx(ctx, tx, candidate.AccountID,
			candidate.RealmID, candidate.StyleRevision, desiredPackID,
			desiredPackVersion, "account_closed", 0); err != nil {
			return candidate, err
		}
		candidate.Superseded = true
		if err := tx.Commit(ctx); err != nil {
			return candidate, err
		}
		return candidate, nil
	}

	var selectedRevision int64
	var selectedPackID string
	var selectedPackVersion int
	err = tx.QueryRow(ctx, `
		SELECT ras.revision, ras.style_pack_id, ras.style_pack_version
		  FROM realm_avatar_styles ras
		  JOIN realms r ON r.id=ras.realm_id AND r.account_id=ras.account_id
		 WHERE ras.account_id=$1 AND ras.realm_id=$2 AND r.deleted_at IS NULL
		 FOR SHARE OF ras`, candidate.AccountID, candidate.RealmID).Scan(
		&selectedRevision, &selectedPackID, &selectedPackVersion)
	if errors.Is(err, pgx.ErrNoRows) {
		if err := supersedeAvatarStyleRolloutJobTx(ctx, tx, candidate.AccountID,
			candidate.RealmID, candidate.StyleRevision, desiredPackID,
			desiredPackVersion, "realm_deleted", 0); err != nil {
			return candidate, err
		}
		candidate.Superseded = true
		if err := tx.Commit(ctx); err != nil {
			return candidate, err
		}
		return candidate, nil
	}
	if err != nil {
		return candidate, fmt.Errorf("lock selected avatar style: %w", err)
	}
	if selectedRevision != candidate.StyleRevision || selectedPackID != desiredPackID ||
		selectedPackVersion != desiredPackVersion {
		if err := supersedeAvatarStyleRolloutJobTx(ctx, tx, candidate.AccountID,
			candidate.RealmID, candidate.StyleRevision, desiredPackID,
			desiredPackVersion, "newer_style_selected", selectedRevision); err != nil {
			return candidate, err
		}
		candidate.Superseded = true
		if err := tx.Commit(ctx); err != nil {
			return candidate, err
		}
		return candidate, nil
	}

	// The expression index walks only projection revisions older than the
	// selected style. Every locked row leaves that range in this transaction,
	// including a soft-deleted tombstone; live rows additionally receive the
	// style projection. This keeps every batch O(batchSize) without rewriting
	// immutable active/version rows.
	var advanced, processed int
	err = tx.QueryRow(ctx, `
		WITH stamp AS MATERIALIZED (
		  SELECT statement_timestamp() AS at
		), targets AS MATERIALIZED (
		  SELECT p.account_id, p.realm_id, p.agent_id
		    FROM agent_avatar_profiles p
		   WHERE p.account_id=$1 AND p.realm_id=$2
		     AND COALESCE(p.style_revision, 0) < $5
		   ORDER BY COALESCE(p.style_revision, 0), p.agent_id
		   FOR UPDATE OF p
		   LIMIT $6
		), changed AS (
		  UPDATE agent_avatar_profiles p
		     SET style_revision=$5,
		         style_pack_id=CASE WHEN a.deleted_at IS NULL THEN $3 ELSE p.style_pack_id END,
		         style_pack_version=CASE WHEN a.deleted_at IS NULL THEN $4 ELSE p.style_pack_version END,
		         proposed_avatar_version=CASE WHEN a.deleted_at IS NULL THEN NULL ELSE p.proposed_avatar_version END,
		         subject_form=CASE WHEN a.deleted_at IS NULL THEN COALESCE((
		           SELECT v.subject_form FROM agent_avatar_versions v
		            WHERE v.account_id=p.account_id AND v.realm_id=p.realm_id
		              AND v.agent_id=p.agent_id
		              AND v.version=p.active_avatar_version
		         ), 'human') ELSE p.subject_form END,
		         status=CASE WHEN a.deleted_at IS NOT NULL THEN p.status
		                     WHEN p.active_avatar_version IS NULL THEN 'generation_due'
		                     ELSE 'evolution_due' END,
		         attempt_count=CASE WHEN a.deleted_at IS NULL THEN 0 ELSE p.attempt_count END,
		         retry_after=CASE WHEN a.deleted_at IS NULL THEN NULL ELSE p.retry_after END,
		         failure_code=CASE WHEN a.deleted_at IS NULL THEN '' ELSE p.failure_code END,
		         revision=CASE WHEN a.deleted_at IS NULL THEN p.revision+1 ELSE p.revision END,
		         updated_at=CASE WHEN a.deleted_at IS NULL THEN s.at ELSE p.updated_at END
		    FROM targets t
		    JOIN agents a ON a.id=t.agent_id AND a.realm_id=t.realm_id
		    CROSS JOIN stamp s
		   WHERE p.account_id=t.account_id AND p.realm_id=t.realm_id
		     AND p.agent_id=t.agent_id
		   RETURNING a.deleted_at IS NULL AS live
		)
		SELECT count(*), count(*) FILTER (WHERE live) FROM changed`,
		candidate.AccountID, candidate.RealmID, desiredPackID,
		desiredPackVersion, candidate.StyleRevision, batchSize).Scan(&advanced, &processed)
	if err != nil {
		return candidate, fmt.Errorf("advance avatar style rollout: %w", err)
	}
	candidate.ProcessedProfiles = processed
	candidate.Completed = advanced < batchSize

	status := "running"
	if candidate.Completed {
		status = "completed"
	}
	if _, err := tx.Exec(ctx, `
		WITH stamp AS MATERIALIZED (SELECT statement_timestamp() AS at)
		UPDATE avatar_style_rollout_jobs
		   SET status=$4,
		       target_profile_count=CASE WHEN $4='completed'
		                                 THEN processed_profile_count+$5
		                                 ELSE NULL END,
		       processed_profile_count=processed_profile_count+$5,
		       batch_count=batch_count+1, last_batch_size=$5,
		       failure_count=0, retry_after=NULL, last_failure_code='',
		       started_at=COALESCE(started_at, stamp.at),
		       completed_at=CASE WHEN $4='completed' THEN stamp.at ELSE NULL END,
		       updated_at=stamp.at
		  FROM stamp
		 WHERE account_id=$1 AND realm_id=$2 AND style_revision=$3`,
		candidate.AccountID, candidate.RealmID, candidate.StyleRevision, status,
		processed); err != nil {
		return candidate, fmt.Errorf("record avatar style rollout progress: %w", err)
	}
	if candidate.Completed {
		if err := logEventTx(ctx, tx, EventInput{
			AccountID: candidate.AccountID, ActorKind: ActorSystem,
			Verb: VerbAvatarStyleRolloutCompleted,
			Metadata: map[string]any{
				"realm_id": candidate.RealmID, "style_revision": strconv.FormatInt(candidate.StyleRevision, 10),
				"style_pack_id": desiredPackID, "style_pack_version": strconv.Itoa(desiredPackVersion),
			},
		}); err != nil {
			return candidate, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return candidate, err
	}
	return candidate, nil
}

func setAvatarStyleRolloutTransactionTimeouts(
	ctx context.Context,
	tx pgx.Tx,
	timeout time.Duration,
) error {
	milliseconds := timeout.Milliseconds()
	if milliseconds < 1 {
		milliseconds = 1
	}
	lockMilliseconds := milliseconds / 2
	if lockMilliseconds < 1 {
		lockMilliseconds = 1
	}
	statementValue := strconv.FormatInt(milliseconds, 10) + "ms"
	lockValue := strconv.FormatInt(lockMilliseconds, 10) + "ms"
	if _, err := tx.Exec(ctx, `
		SELECT set_config('lock_timeout', $1, true),
		       set_config('statement_timeout', $2, true)`, lockValue, statementValue); err != nil {
		return fmt.Errorf("set avatar style rollout transaction timeouts: %w", err)
	}
	return nil
}

func classifyAvatarStyleRolloutFailure(err error) string {
	if errors.Is(err, context.DeadlineExceeded) {
		return "batch_timeout"
	}
	var pgErr interface{ SQLState() string }
	if errors.As(err, &pgErr) {
		switch pgErr.SQLState() {
		case "55P03":
			return "lock_timeout"
		case "57014":
			return "statement_timeout"
		}
	}
	return "candidate_failed"
}

func (s *Store) recordAvatarStyleRolloutFailure(
	ctx context.Context,
	candidate AvatarStyleRolloutBatchResult,
	failureCode string,
) error {
	_, err := s.pool.Exec(ctx, `
		WITH stamp AS MATERIALIZED (SELECT statement_timestamp() AS at)
		UPDATE avatar_style_rollout_jobs
		   SET failure_count=LEAST(failure_count+1, 1000000),
		       retry_after=stamp.at + make_interval(secs =>
		         CASE WHEN failure_count >= 8 THEN 300 ELSE (1 << failure_count) END),
		       last_failure_code=$4, updated_at=stamp.at
		  FROM stamp
		 WHERE account_id=$1 AND realm_id=$2 AND style_revision=$3
		   AND status IN ('pending','running')`, candidate.AccountID,
		candidate.RealmID, candidate.StyleRevision, failureCode)
	if err != nil {
		return fmt.Errorf("record avatar style rollout failure: %w", err)
	}
	return nil
}

func supersedeAvatarStyleRolloutJobTx(
	ctx context.Context,
	tx pgx.Tx,
	accountID, realmID string,
	styleRevision int64,
	stylePackID string,
	stylePackVersion int,
	reason string,
	supersededByRevision int64,
) error {
	tag, err := tx.Exec(ctx, `
		WITH stamp AS MATERIALIZED (SELECT statement_timestamp() AS at)
		UPDATE avatar_style_rollout_jobs
		   SET status='superseded', target_profile_count=processed_profile_count,
		       failure_count=0, retry_after=NULL, last_failure_code='',
		       superseded_at=stamp.at, updated_at=stamp.at
		  FROM stamp
		 WHERE account_id=$1 AND realm_id=$2 AND style_revision=$3
		   AND status IN ('pending','running')`, accountID, realmID, styleRevision)
	if err != nil {
		return fmt.Errorf("supersede avatar style rollout: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return nil
	}
	metadata := map[string]any{
		"realm_id": realmID, "style_revision": strconv.FormatInt(styleRevision, 10),
		"style_pack_id": stylePackID, "style_pack_version": strconv.Itoa(stylePackVersion),
		"reason": reason,
	}
	if supersededByRevision > 0 {
		metadata["superseded_by_style_revision"] = strconv.FormatInt(supersededByRevision, 10)
	}
	return logEventTx(ctx, tx, EventInput{
		AccountID: accountID, ActorKind: ActorSystem,
		Verb: VerbAvatarStyleRolloutSuperseded, Metadata: metadata,
	})
}

// supersedeOpenAvatarStyleRolloutsForAccountTx terminalizes every open job
// before an account becomes permanently ineligible for worker discovery. The
// caller must already hold the account row lock, preserving the global
// account -> rollout-job order used by publishers and workers.
func supersedeOpenAvatarStyleRolloutsForAccountTx(
	ctx context.Context,
	tx pgx.Tx,
	accountID, reason string,
) error {
	rows, err := tx.Query(ctx, `
		SELECT realm_id,style_revision,style_pack_id,style_pack_version
		  FROM avatar_style_rollout_jobs
		 WHERE account_id=$1 AND status IN ('pending','running')
		 ORDER BY realm_id,style_revision
		 FOR UPDATE`, accountID)
	if err != nil {
		return fmt.Errorf("lock account avatar style rollouts: %w", err)
	}
	type openRollout struct {
		realmID, stylePackID string
		styleRevision        int64
		stylePackVersion     int
	}
	jobs := make([]openRollout, 0)
	for rows.Next() {
		var job openRollout
		if err := rows.Scan(&job.realmID, &job.styleRevision, &job.stylePackID,
			&job.stylePackVersion); err != nil {
			rows.Close()
			return fmt.Errorf("scan account avatar style rollout: %w", err)
		}
		jobs = append(jobs, job)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("list account avatar style rollouts: %w", err)
	}
	rows.Close()
	for _, job := range jobs {
		if err := supersedeAvatarStyleRolloutJobTx(ctx, tx, accountID, job.realmID,
			job.styleRevision, job.stylePackID, job.stylePackVersion, reason, 0); err != nil {
			return err
		}
	}
	return nil
}
