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
	defaultAgentEmailRetentionBatchSize       = 25
	maxAgentEmailRetentionBatchSize           = 100
	defaultAgentEmailRetentionInterval        = 5 * time.Minute
	defaultAgentEmailRetentionBatchTimeout    = 2 * time.Minute
	minAgentEmailRetentionBatchTimeout        = 10 * time.Second
	maxAgentEmailRetentionBatchTimeout        = 5 * time.Minute
	defaultAgentEmailRetentionWorkerLaneCount = 16

	// raw_mime is currently bounded to 5 MiB per row. The additional batch
	// ceiling keeps WAL, cascades, and transaction duration predictable even
	// when every selected message is at that per-message maximum.
	maxAgentEmailRetentionBatchRawBytes int64 = 32 * 1024 * 1024

	// A suspected-duplicate root may be referenced by later retained mail.
	// Bound both one pathological root and the cumulative update set so link
	// repair cannot monopolize a worker lane.
	maxAgentEmailRetentionMessageBacklinks       = 4096
	maxAgentEmailRetentionBatchBacklinks         = 65536
	maxAgentEmailRetentionGeneration       int64 = 4611686018427387903
)

// AgentEmailRetentionMode selects read-only preview or destructive
// enforcement. The two modes have independent durable lanes and scan cursors.
type AgentEmailRetentionMode string

const (
	// AgentEmailRetentionModePreview scans eligible mail without deleting it.
	AgentEmailRetentionModePreview AgentEmailRetentionMode = "preview"
	// AgentEmailRetentionModeEnforce deletes eligible mail within bounded batches.
	AgentEmailRetentionModeEnforce AgentEmailRetentionMode = "enforce"
)

// AgentEmailRetentionBatchResult contains value-free operational counts only.
type AgentEmailRetentionBatchResult struct {
	Scanned               int64
	SkippedLocked         int64
	Eligible              int64
	Deleted               int64
	DeletedRawBytes       int64
	DeferredActive        int64
	DeferredLocked        int64
	DeferredOversize      int64
	DeferredBudget        int64
	ClearedDuplicateLinks int64
	DeletedCanaryProofs   int64
	ScanCapped            bool
	LaneAdvanced          bool
}

// AgentEmailRetentionWorkerConfig controls one process-local job loop.
// LaneCount is schema-owned and deliberately not a replica setting.
type AgentEmailRetentionWorkerConfig struct {
	BatchSize    int
	Interval     time.Duration
	BatchTimeout time.Duration
	LaneCount    int
	Mode         AgentEmailRetentionMode
}

// DefaultAgentEmailRetentionWorkerConfig returns deletion-safe preview
// defaults. The job itself remains disabled unless the worker environment
// explicitly enables it.
func DefaultAgentEmailRetentionWorkerConfig() AgentEmailRetentionWorkerConfig {
	return AgentEmailRetentionWorkerConfig{
		BatchSize:    defaultAgentEmailRetentionBatchSize,
		Interval:     defaultAgentEmailRetentionInterval,
		BatchTimeout: defaultAgentEmailRetentionBatchTimeout,
		LaneCount:    defaultAgentEmailRetentionWorkerLaneCount,
		Mode:         AgentEmailRetentionModePreview,
	}
}

// Validate checks bounded operational settings.
func (c AgentEmailRetentionWorkerConfig) Validate() error {
	if c.BatchSize < 1 || c.BatchSize > maxAgentEmailRetentionBatchSize {
		return fmt.Errorf(
			"agent-email retention batch size must be between 1 and %d",
			maxAgentEmailRetentionBatchSize,
		)
	}
	if c.Interval < time.Minute || c.Interval > 24*time.Hour {
		return errors.New(
			"agent-email retention interval must be between 1 minute and 24 hours",
		)
	}
	if c.BatchTimeout < minAgentEmailRetentionBatchTimeout ||
		c.BatchTimeout > maxAgentEmailRetentionBatchTimeout {
		return fmt.Errorf(
			"agent-email retention batch timeout must be between %s and %s",
			minAgentEmailRetentionBatchTimeout,
			maxAgentEmailRetentionBatchTimeout,
		)
	}
	if c.LaneCount != defaultAgentEmailRetentionWorkerLaneCount {
		return fmt.Errorf(
			"agent-email retention worker lane count must be %d",
			defaultAgentEmailRetentionWorkerLaneCount,
		)
	}
	switch c.Mode {
	case AgentEmailRetentionModePreview, AgentEmailRetentionModeEnforce:
	default:
		return fmt.Errorf(
			"agent-email retention mode must be %q or %q",
			AgentEmailRetentionModePreview,
			AgentEmailRetentionModeEnforce,
		)
	}
	return nil
}

// PreviewAgentEmailRetentionBatch runs the production cutoff, claim, and
// duplicate-link eligibility checks without changing canonical email rows.
func (s *Store) PreviewAgentEmailRetentionBatch(
	ctx context.Context,
	batchSize int,
) (AgentEmailRetentionBatchResult, error) {
	return s.processAgentEmailRetentionBatch(
		ctx,
		batchSize,
		false,
		0,
		defaultAgentEmailRetentionBatchTimeout,
		defaultAgentEmailRetentionWorkerLaneCount,
	)
}

// ProcessAgentEmailRetentionBatch deletes bounded eligible inbound email.
// Missing plans.AgentEmailRetentionDaysPolicy means indefinite retention.
func (s *Store) ProcessAgentEmailRetentionBatch(
	ctx context.Context,
	batchSize int,
) (AgentEmailRetentionBatchResult, error) {
	return s.processAgentEmailRetentionBatch(
		ctx,
		batchSize,
		true,
		0,
		defaultAgentEmailRetentionBatchTimeout,
		defaultAgentEmailRetentionWorkerLaneCount,
	)
}

type agentEmailRetentionLaneClaim struct {
	LaneID        int
	AccountCursor string
	Generation    int64
}

type agentEmailRetentionCandidate struct {
	ID         string
	ReceivedAt time.Time
	RawBytes   int64
}

type agentEmailRetentionScanState struct {
	RetentionDays int
	CycleCutoff   time.Time
	LastReceived  *time.Time
	LastMessageID *string
	Generation    int64
}

func nextAgentEmailRetentionGeneration(current int64) int64 {
	if current >= maxAgentEmailRetentionGeneration {
		return 1
	}
	return current + 1
}

// claimAgentEmailRetentionLane records a short durable lease before the
// heavier account transaction. A timed-out process cannot later mutate the
// lane after another replica advances its generation.
func (s *Store) claimAgentEmailRetentionLane(
	ctx context.Context,
	mode AgentEmailRetentionMode,
	workerInterval time.Duration,
	batchTimeout time.Duration,
	workerLaneCount int,
) (*agentEmailRetentionLaneClaim, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin agent-email retention lane claim: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var laneCount int64
	if err := tx.QueryRow(ctx, `
		SELECT count(*)
		  FROM agent_email_retention_worker_lanes
		 WHERE mode=$1 AND lane_id < $2`,
		string(mode), workerLaneCount,
	).Scan(&laneCount); err != nil {
		return nil, fmt.Errorf("count agent-email retention worker lanes: %w", err)
	}
	if laneCount != int64(workerLaneCount) {
		return nil, fmt.Errorf(
			"agent-email retention worker lane set has %d rows, want %d",
			laneCount, workerLaneCount,
		)
	}

	var claim agentEmailRetentionLaneClaim
	var previousGeneration int64
	err = tx.QueryRow(ctx, `
		SELECT lane_id,account_cursor,generation
		  FROM agent_email_retention_worker_lanes
		 WHERE mode=$1
		   AND lane_id < $2
		   AND next_run_at <= statement_timestamp()
		 ORDER BY next_run_at,lane_id
		 LIMIT 1
		 FOR UPDATE SKIP LOCKED`,
		string(mode), workerLaneCount,
	).Scan(&claim.LaneID, &claim.AccountCursor, &previousGeneration)
	if errors.Is(err, pgx.ErrNoRows) {
		if err := tx.Commit(ctx); err != nil {
			return nil, fmt.Errorf(
				"commit empty agent-email retention lane claim: %w", err,
			)
		}
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("select agent-email retention worker lane: %w", err)
	}

	claim.Generation = nextAgentEmailRetentionGeneration(previousGeneration)
	lease := batchTimeout + workerInterval
	command, err := tx.Exec(ctx, `
		UPDATE agent_email_retention_worker_lanes
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
		return nil, fmt.Errorf("lease agent-email retention worker lane: %w", err)
	}
	if command.RowsAffected() != 1 {
		return nil, errors.New(
			"lease agent-email retention worker lane lost its fence",
		)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit agent-email retention lane claim: %w", err)
	}
	return &claim, nil
}

func (s *Store) processAgentEmailRetentionBatch(
	ctx context.Context,
	batchSize int,
	enforce bool,
	workerInterval time.Duration,
	batchTimeout time.Duration,
	workerLaneCount int,
) (AgentEmailRetentionBatchResult, error) {
	if batchSize < 1 || batchSize > maxAgentEmailRetentionBatchSize {
		return AgentEmailRetentionBatchResult{}, fmt.Errorf(
			"agent-email retention batch size must be between 1 and %d",
			maxAgentEmailRetentionBatchSize,
		)
	}
	if workerInterval < 0 {
		return AgentEmailRetentionBatchResult{},
			errors.New("agent-email retention worker interval cannot be negative")
	}
	if batchTimeout < minAgentEmailRetentionBatchTimeout ||
		batchTimeout > maxAgentEmailRetentionBatchTimeout {
		return AgentEmailRetentionBatchResult{}, fmt.Errorf(
			"agent-email retention batch timeout must be between %s and %s",
			minAgentEmailRetentionBatchTimeout,
			maxAgentEmailRetentionBatchTimeout,
		)
	}
	if workerLaneCount != defaultAgentEmailRetentionWorkerLaneCount {
		return AgentEmailRetentionBatchResult{}, fmt.Errorf(
			"agent-email retention worker lane count must be %d",
			defaultAgentEmailRetentionWorkerLaneCount,
		)
	}

	mode := AgentEmailRetentionModePreview
	if enforce {
		mode = AgentEmailRetentionModeEnforce
	}
	claim, err := s.claimAgentEmailRetentionLane(
		ctx, mode, workerInterval, batchTimeout, workerLaneCount,
	)
	if err != nil {
		return AgentEmailRetentionBatchResult{}, err
	}
	if claim == nil {
		return AgentEmailRetentionBatchResult{}, nil
	}
	return s.processClaimedAgentEmailRetentionBatch(
		ctx, batchSize, enforce, workerInterval, workerLaneCount, mode, *claim,
	)
}

// processClaimedAgentEmailRetentionBatch holds an exclusive account row while
// examining its email graph. Foreground email operations take an account
// share lock first, so SKIP LOCKED avoids waiting for active work and the
// acquired account fence prevents new claims, duplicate links, or canary
// proofs from racing deletion.
func (s *Store) processClaimedAgentEmailRetentionBatch(
	ctx context.Context,
	batchSize int,
	enforce bool,
	workerInterval time.Duration,
	workerLaneCount int,
	mode AgentEmailRetentionMode,
	claim agentEmailRetentionLaneClaim,
) (AgentEmailRetentionBatchResult, error) {
	var result AgentEmailRetentionBatchResult
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return result, fmt.Errorf("begin agent-email retention batch: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var laneCursor string
	var laneCount int64
	err = tx.QueryRow(ctx, `
		SELECT lane.account_cursor,
		       (SELECT count(*)
		          FROM agent_email_retention_worker_lanes
		         WHERE mode=$1 AND lane_id < $2)
		  FROM agent_email_retention_worker_lanes lane
		 WHERE lane.mode=$1 AND lane.lane_id=$3 AND lane.generation=$4
		 FOR UPDATE`,
		string(mode), workerLaneCount, claim.LaneID, claim.Generation,
	).Scan(&laneCursor, &laneCount)
	if errors.Is(err, pgx.ErrNoRows) {
		return result, nil
	}
	if err != nil {
		return result, fmt.Errorf("fence agent-email retention worker lane: %w", err)
	}
	if laneCount != int64(workerLaneCount) {
		return result, fmt.Errorf(
			"agent-email retention worker lane set has %d rows, want %d",
			laneCount, workerLaneCount,
		)
	}

	seenAccounts := make(map[string]struct{})
	rawBudgetRemaining := maxAgentEmailRetentionBatchRawBytes
	backlinkBudgetRemaining := int64(maxAgentEmailRetentionBatchBacklinks)
	lastAccountCursor := laneCursor
	stopForBudget := false

	for result.Scanned < int64(batchSize) && len(seenAccounts) < batchSize {
		accountID, retentionDays, found, locked, selectErr :=
			selectAgentEmailRetentionAccount(
				ctx, tx, claim.LaneID, lastAccountCursor,
			)
		if selectErr != nil {
			return AgentEmailRetentionBatchResult{}, selectErr
		}
		if accountID == "" {
			break
		}
		if _, seen := seenAccounts[accountID]; seen {
			break
		}
		seenAccounts[accountID] = struct{}{}
		lastAccountCursor = accountID
		if locked {
			result.SkippedLocked++
			result.DeferredLocked++
			continue
		}
		// A policy update may commit between the unlocked page lookup and the
		// exact lock. Advance past that no-longer-finite account without
		// reporting a lock deferral.
		if !found {
			continue
		}

		remaining := batchSize - int(result.Scanned)
		accountResult, rawUsed, backlinksUsed, budgetHit, accountErr :=
			processAgentEmailRetentionAccount(
				ctx,
				tx,
				mode,
				accountID,
				retentionDays,
				remaining,
				rawBudgetRemaining,
				backlinkBudgetRemaining,
				enforce,
			)
		if accountErr != nil {
			return AgentEmailRetentionBatchResult{}, accountErr
		}
		addAgentEmailRetentionResult(&result, accountResult)
		rawBudgetRemaining -= rawUsed
		backlinkBudgetRemaining -= backlinksUsed
		if budgetHit {
			stopForBudget = true
			break
		}
	}

	result.ScanCapped = result.Scanned >= int64(batchSize) || stopForBudget
	command, err := tx.Exec(ctx, `
		UPDATE agent_email_retention_worker_lanes
		   SET account_cursor=$4,
		       next_run_at=statement_timestamp() +
		         ($5::bigint * interval '1 microsecond'),
		       updated_at=statement_timestamp()
		 WHERE mode=$1 AND lane_id=$2 AND generation=$3
		`,
		string(mode),
		claim.LaneID,
		claim.Generation,
		lastAccountCursor,
		workerInterval.Microseconds(),
	)
	if err != nil {
		return AgentEmailRetentionBatchResult{},
			fmt.Errorf("advance agent-email retention worker lane: %w", err)
	}
	if command.RowsAffected() != 1 {
		return AgentEmailRetentionBatchResult{}, nil
	}
	result.LaneAdvanced = true
	if err := tx.Commit(ctx); err != nil {
		return AgentEmailRetentionBatchResult{},
			fmt.Errorf("commit agent-email retention batch: %w", err)
	}
	return result, nil
}

func selectAgentEmailRetentionAccount(
	ctx context.Context,
	tx pgx.Tx,
	laneID int,
	cursor string,
) (string, int, bool, bool, error) {
	for _, after := range []bool{true, false} {
		operator := ">"
		if !after {
			operator = "<="
		}
		query := fmt.Sprintf(`
			SELECT id,
			       (plan_policies ->> $3)::integer
			  FROM accounts
			 WHERE plan_policies ? 'agent_email_retention_days'
			   AND get_byte(sha256(id::bytea), 0) %% 16 = $1
			   AND id %s $2
			 ORDER BY id
			 LIMIT 1`, operator)
		var accountID string
		var retentionDays int
		err := tx.QueryRow(
			ctx,
			query,
			laneID,
			cursor,
			plans.AgentEmailRetentionDaysPolicy,
		).Scan(&accountID, &retentionDays)
		if errors.Is(err, pgx.ErrNoRows) {
			continue
		}
		if err != nil {
			return "", 0, false, false,
				fmt.Errorf("select agent-email retention account: %w", err)
		}
		var lockedRetentionDays int
		err = tx.QueryRow(ctx, `
			SELECT (plan_policies ->> $2)::integer
			  FROM accounts
			 WHERE id=$1
			   AND plan_policies ? 'agent_email_retention_days'
			 FOR UPDATE SKIP LOCKED`,
			accountID,
			plans.AgentEmailRetentionDaysPolicy,
		).Scan(&lockedRetentionDays)
		if errors.Is(err, pgx.ErrNoRows) {
			var stillFinite bool
			if err := tx.QueryRow(ctx, `
				SELECT EXISTS (
				  SELECT 1
				    FROM accounts
				   WHERE id=$1
				     AND plan_policies ? 'agent_email_retention_days'
				)`,
				accountID,
			).Scan(&stillFinite); err != nil {
				return "", 0, false, false,
					fmt.Errorf("check locked agent-email retention account: %w", err)
			}
			return accountID, retentionDays, false, stillFinite, nil
		}
		if err != nil {
			return "", 0, false, false,
				fmt.Errorf("lock agent-email retention account: %w", err)
		}
		retentionDays = lockedRetentionDays
		if retentionDays < 1 ||
			int64(retentionDays) > plans.MaxAgentEmailRetentionDays {
			return "", 0, false, false, fmt.Errorf(
				"select agent-email retention account: %s is outside the defensive bound",
				plans.AgentEmailRetentionDaysPolicy,
			)
		}
		return accountID, retentionDays, true, false, nil
	}
	return "", 0, false, false, nil
}

func processAgentEmailRetentionAccount(
	ctx context.Context,
	tx pgx.Tx,
	mode AgentEmailRetentionMode,
	accountID string,
	retentionDays int,
	limit int,
	rawBudget int64,
	backlinkBudget int64,
	enforce bool,
) (
	AgentEmailRetentionBatchResult,
	int64,
	int64,
	bool,
	error,
) {
	var result AgentEmailRetentionBatchResult
	var currentCutoff time.Time
	if err := tx.QueryRow(ctx, `
		SELECT statement_timestamp() - make_interval(days => $1)`,
		retentionDays,
	).Scan(&currentCutoff); err != nil {
		return result, 0, 0, false,
			fmt.Errorf("read agent-email retention cutoff: %w", err)
	}

	state := agentEmailRetentionScanState{
		RetentionDays: retentionDays,
		CycleCutoff:   currentCutoff,
	}
	var stored agentEmailRetentionScanState
	err := tx.QueryRow(ctx, `
		SELECT retention_days,cycle_cutoff,last_received_at,last_message_id,generation
		  FROM agent_email_retention_account_scan_state
		 WHERE mode=$1 AND account_id=$2
		 FOR UPDATE`,
		string(mode), accountID,
	).Scan(
		&stored.RetentionDays,
		&stored.CycleCutoff,
		&stored.LastReceived,
		&stored.LastMessageID,
		&stored.Generation,
	)
	if err == nil {
		state.Generation = stored.Generation
		if stored.RetentionDays == retentionDays {
			state = stored
		}
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return result, 0, 0, false,
			fmt.Errorf("lock agent-email retention account scan: %w", err)
	}

	lastMessageID := ""
	if state.LastMessageID != nil {
		lastMessageID = *state.LastMessageID
	}
	rows, err := tx.Query(ctx, `
		SELECT id,received_at,raw_size_bytes
		  FROM agent_email_messages
		 WHERE account_id=$1
		   AND received_at < LEAST(
		         $2::timestamptz,
		         statement_timestamp() - make_interval(days => $3)
		       )
		   AND (
		     $4::timestamptz IS NULL
		     OR received_at > $4
		     OR (received_at = $4 AND id > $5)
		   )
		 ORDER BY received_at,id
		 LIMIT $6
		 FOR UPDATE`,
		accountID,
		state.CycleCutoff,
		retentionDays,
		state.LastReceived,
		lastMessageID,
		limit,
	)
	if err != nil {
		return result, 0, 0, false,
			fmt.Errorf("select agent-email retention candidates: %w", err)
	}
	candidates := make([]agentEmailRetentionCandidate, 0, limit)
	for rows.Next() {
		var candidate agentEmailRetentionCandidate
		if err := rows.Scan(
			&candidate.ID, &candidate.ReceivedAt, &candidate.RawBytes,
		); err != nil {
			rows.Close()
			return result, 0, 0, false,
				fmt.Errorf("scan agent-email retention candidate: %w", err)
		}
		candidates = append(candidates, candidate)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return result, 0, 0, false,
			fmt.Errorf("iterate agent-email retention candidates: %w", err)
	}
	rows.Close()
	result.Scanned = int64(len(candidates))

	ids := make([]string, 0, len(candidates))
	for _, candidate := range candidates {
		ids = append(ids, candidate.ID)
	}
	active := make(map[string]bool, len(candidates))
	backlinkCounts := make(map[string]int64, len(candidates))
	if len(ids) > 0 {
		deliveryRows, queryErr := tx.Query(ctx, `
			SELECT message_id,
			       processing_state='claimed'
			         AND lease_expires_at > statement_timestamp()
			  FROM agent_email_deliveries
			 WHERE account_id=$1 AND message_id=ANY($2::text[])
			 ORDER BY message_id
			 FOR UPDATE`,
			accountID, ids,
		)
		if queryErr != nil {
			return result, 0, 0, false,
				fmt.Errorf("lock agent-email retention deliveries: %w", queryErr)
		}
		for deliveryRows.Next() {
			var messageID string
			var hasActiveClaim bool
			if err := deliveryRows.Scan(&messageID, &hasActiveClaim); err != nil {
				deliveryRows.Close()
				return result, 0, 0, false,
					fmt.Errorf("scan agent-email retention delivery: %w", err)
			}
			active[messageID] = hasActiveClaim
		}
		if err := deliveryRows.Err(); err != nil {
			deliveryRows.Close()
			return result, 0, 0, false,
				fmt.Errorf("iterate agent-email retention deliveries: %w", err)
		}
		deliveryRows.Close()

		countRows, queryErr := tx.Query(ctx, `
			SELECT possible_duplicate_of_message_id,count(*)
			  FROM agent_email_messages
			 WHERE account_id=$1
			   AND possible_duplicate_of_message_id=ANY($2::text[])
			 GROUP BY possible_duplicate_of_message_id`,
			accountID, ids,
		)
		if queryErr != nil {
			return result, 0, 0, false,
				fmt.Errorf("count agent-email duplicate backlinks: %w", queryErr)
		}
		for countRows.Next() {
			var messageID string
			var count int64
			if err := countRows.Scan(&messageID, &count); err != nil {
				countRows.Close()
				return result, 0, 0, false,
					fmt.Errorf("scan agent-email duplicate backlink count: %w", err)
			}
			backlinkCounts[messageID] = count
		}
		if err := countRows.Err(); err != nil {
			countRows.Close()
			return result, 0, 0, false,
				fmt.Errorf("iterate agent-email duplicate backlink counts: %w", err)
		}
		countRows.Close()
	}

	eligibleIDs := make([]string, 0, len(candidates))
	var rawUsed, backlinksUsed int64
	progressIndex := len(candidates) - 1
	budgetHit := false
	for index, candidate := range candidates {
		if active[candidate.ID] {
			result.DeferredActive++
			continue
		}
		backlinks := backlinkCounts[candidate.ID]
		if backlinks > maxAgentEmailRetentionMessageBacklinks {
			result.DeferredOversize++
			continue
		}
		if candidate.RawBytes > rawBudget-rawUsed ||
			backlinks > backlinkBudget-backlinksUsed {
			result.DeferredBudget++
			progressIndex = index - 1
			budgetHit = true
			break
		}
		rawUsed += candidate.RawBytes
		backlinksUsed += backlinks
		eligibleIDs = append(eligibleIDs, candidate.ID)
	}
	result.Eligible = int64(len(eligibleIDs))

	if enforce && len(eligibleIDs) > 0 {
		cleared, clearErr := clearAgentEmailRetentionBacklinks(
			ctx, tx, accountID, eligibleIDs,
		)
		if clearErr != nil {
			return result, 0, 0, false, clearErr
		}
		result.ClearedDuplicateLinks = cleared

		canaries, canaryErr := lockAgentEmailRetentionCanaries(
			ctx, tx, accountID, eligibleIDs,
		)
		if canaryErr != nil {
			return result, 0, 0, false, canaryErr
		}
		result.DeletedCanaryProofs = canaries

		deletedRows, deleteErr := tx.Query(ctx, `
			DELETE FROM agent_email_messages
			 WHERE account_id=$1 AND id=ANY($2::text[])
			RETURNING raw_size_bytes`,
			accountID, eligibleIDs,
		)
		if deleteErr != nil {
			return result, 0, 0, false,
				fmt.Errorf("delete retained agent email: %w", deleteErr)
		}
		for deletedRows.Next() {
			var deletedBytes int64
			if err := deletedRows.Scan(&deletedBytes); err != nil {
				deletedRows.Close()
				return result, 0, 0, false,
					fmt.Errorf("scan deleted agent-email bytes: %w", err)
			}
			result.Deleted++
			result.DeletedRawBytes += deletedBytes
		}
		if err := deletedRows.Err(); err != nil {
			deletedRows.Close()
			return result, 0, 0, false,
				fmt.Errorf("iterate deleted agent email: %w", err)
		}
		deletedRows.Close()
		if result.Deleted != int64(len(eligibleIDs)) {
			return result, 0, 0, false,
				errors.New("delete retained agent email lost a selected row")
		}
	}

	nextState := state
	nextState.RetentionDays = retentionDays
	nextState.Generation = nextAgentEmailRetentionGeneration(state.Generation)
	if budgetHit {
		if progressIndex >= 0 {
			received := candidates[progressIndex].ReceivedAt
			messageID := candidates[progressIndex].ID
			nextState.LastReceived = &received
			nextState.LastMessageID = &messageID
		}
	} else if len(candidates) < limit {
		nextState.CycleCutoff = currentCutoff
		nextState.LastReceived = nil
		nextState.LastMessageID = nil
	} else if len(candidates) > 0 {
		received := candidates[len(candidates)-1].ReceivedAt
		messageID := candidates[len(candidates)-1].ID
		nextState.LastReceived = &received
		nextState.LastMessageID = &messageID
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO agent_email_retention_account_scan_state
		  (mode,account_id,retention_days,cycle_cutoff,last_received_at,
		   last_message_id,generation,updated_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,statement_timestamp())
		ON CONFLICT (mode,account_id) DO UPDATE
		  SET retention_days=EXCLUDED.retention_days,
		      cycle_cutoff=EXCLUDED.cycle_cutoff,
		      last_received_at=EXCLUDED.last_received_at,
		      last_message_id=EXCLUDED.last_message_id,
		      generation=EXCLUDED.generation,
		      updated_at=EXCLUDED.updated_at`,
		string(mode),
		accountID,
		nextState.RetentionDays,
		nextState.CycleCutoff,
		nextState.LastReceived,
		nextState.LastMessageID,
		nextState.Generation,
	); err != nil {
		return result, 0, 0, false,
			fmt.Errorf("advance agent-email retention account scan: %w", err)
	}
	return result, rawUsed, backlinksUsed, budgetHit, nil
}

func clearAgentEmailRetentionBacklinks(
	ctx context.Context,
	tx pgx.Tx,
	accountID string,
	messageIDs []string,
) (int64, error) {
	rows, err := tx.Query(ctx, `
		SELECT id
		  FROM agent_email_messages
		 WHERE account_id=$1
		   AND possible_duplicate_of_message_id=ANY($2::text[])
		 ORDER BY id
		 FOR UPDATE`,
		accountID, messageIDs,
	)
	if err != nil {
		return 0, fmt.Errorf("lock agent-email duplicate backlinks: %w", err)
	}
	var locked int64
	for rows.Next() {
		var ignored string
		if err := rows.Scan(&ignored); err != nil {
			rows.Close()
			return 0, fmt.Errorf("scan agent-email duplicate backlink: %w", err)
		}
		locked++
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, fmt.Errorf("iterate agent-email duplicate backlinks: %w", err)
	}
	rows.Close()
	command, err := tx.Exec(ctx, `
		UPDATE agent_email_messages
		   SET possible_duplicate_of_message_id=NULL
		 WHERE account_id=$1
		   AND possible_duplicate_of_message_id=ANY($2::text[])`,
		accountID, messageIDs,
	)
	if err != nil {
		return 0, fmt.Errorf("clear agent-email duplicate backlinks: %w", err)
	}
	if command.RowsAffected() != locked {
		return 0, errors.New(
			"clear agent-email duplicate backlinks lost a locked row",
		)
	}
	return locked, nil
}

func lockAgentEmailRetentionCanaries(
	ctx context.Context,
	tx pgx.Tx,
	accountID string,
	messageIDs []string,
) (int64, error) {
	rows, err := tx.Query(ctx, `
		SELECT challenge_sha256
		  FROM agent_email_retry_canary_arms
		 WHERE account_id=$1
		   AND accepted_message_id=ANY($2::text[])
		 ORDER BY realm_id,mailbox_id,challenge_sha256
		 FOR UPDATE`,
		accountID, messageIDs,
	)
	if err != nil {
		return 0, fmt.Errorf("lock agent-email retention canary proofs: %w", err)
	}
	var count int64
	for rows.Next() {
		var ignored string
		if err := rows.Scan(&ignored); err != nil {
			rows.Close()
			return 0, fmt.Errorf("scan agent-email retention canary proof: %w", err)
		}
		count++
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, fmt.Errorf("iterate agent-email retention canary proofs: %w", err)
	}
	rows.Close()
	return count, nil
}

func addAgentEmailRetentionResult(
	target *AgentEmailRetentionBatchResult,
	value AgentEmailRetentionBatchResult,
) {
	target.Scanned += value.Scanned
	target.SkippedLocked += value.SkippedLocked
	target.Eligible += value.Eligible
	target.Deleted += value.Deleted
	target.DeletedRawBytes += value.DeletedRawBytes
	target.DeferredActive += value.DeferredActive
	target.DeferredLocked += value.DeferredLocked
	target.DeferredOversize += value.DeferredOversize
	target.DeferredBudget += value.DeferredBudget
	target.ClearedDuplicateLinks += value.ClearedDuplicateLinks
	target.DeletedCanaryProofs += value.DeletedCanaryProofs
}

// RunAgentEmailRetentionWorker attempts one bounded non-empty lane immediately
// and on each interval. Separate replicas can claim separate due lanes.
func (s *Store) RunAgentEmailRetentionWorker(
	ctx context.Context,
	cfg AgentEmailRetentionWorkerConfig,
	onResult func(AgentEmailRetentionBatchResult),
	onError func(error),
) error {
	if err := cfg.Validate(); err != nil {
		return err
	}
	run := func() {
		result, err := s.processAgentEmailRetentionWorkerBatch(ctx, cfg)
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

func (s *Store) processAgentEmailRetentionWorkerBatch(
	ctx context.Context,
	cfg AgentEmailRetentionWorkerConfig,
) (AgentEmailRetentionBatchResult, error) {
	if err := cfg.Validate(); err != nil {
		return AgentEmailRetentionBatchResult{}, err
	}
	attemptCtx, cancelAttempt := context.WithTimeout(ctx, cfg.BatchTimeout)
	defer cancelAttempt()
	var aggregate AgentEmailRetentionBatchResult
	for attempt := 0; attempt < cfg.LaneCount; attempt++ {
		result, err := s.processAgentEmailRetentionBatch(
			attemptCtx,
			cfg.BatchSize,
			cfg.Mode == AgentEmailRetentionModeEnforce,
			cfg.Interval,
			cfg.BatchTimeout,
			cfg.LaneCount,
		)
		addAgentEmailRetentionResult(&aggregate, result)
		aggregate.ScanCapped = aggregate.ScanCapped || result.ScanCapped
		aggregate.LaneAdvanced = aggregate.LaneAdvanced || result.LaneAdvanced
		if err != nil || result.Scanned > 0 || !result.LaneAdvanced {
			return aggregate, err
		}
	}
	return aggregate, nil
}
