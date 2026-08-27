package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

const (
	defaultAccountPurgeBatchSize      = 100
	maxAccountPurgeBatchSize          = 1000
	defaultAccountPurgeInterval       = 5 * time.Minute
	defaultAccountPurgeBatchTimeout   = 2 * time.Minute
	minAccountPurgeBatchTimeout       = time.Second
	maxAccountPurgeBatchTimeout       = 5 * time.Minute
	defaultAccountPurgeGrace          = 720 * time.Hour
	minAccountPurgeGrace              = time.Minute
	maxAccountPurgeGrace              = 8760 * time.Hour
	maxAccountPurgeTxAttempts         = 3
	purgedProvisionRequestFingerprint = "0000000000000000000000000000000000000000000000000000000000000000"
	accountPurgeAttachmentUnderflow   = "agent-email attachment counter underflow"
)

// AccountPurgeMode selects read-only preview or destructive enforcement.
type AccountPurgeMode string

const (
	// AccountPurgeModePreview counts content for eligible closed accounts
	// without deleting or anonymizing anything.
	AccountPurgeModePreview AccountPurgeMode = "preview"
	// AccountPurgeModeEnforce deletes content and leaves an anonymized account
	// tombstone for each eligible closed account.
	AccountPurgeModeEnforce AccountPurgeMode = "enforce"
)

// AccountPurgeWorkerConfig controls one cell worker's bounded batch,
// cadence, deadline, closure grace window, and preview/enforcement mode.
type AccountPurgeWorkerConfig struct {
	BatchSize    int
	Interval     time.Duration
	BatchTimeout time.Duration
	Grace        time.Duration
	Mode         AccountPurgeMode
}

// DefaultAccountPurgeWorkerConfig returns conservative dark-rollout defaults:
// preview at most 100 closed accounts every five minutes after a 30-day grace.
func DefaultAccountPurgeWorkerConfig() AccountPurgeWorkerConfig {
	return AccountPurgeWorkerConfig{
		BatchSize:    defaultAccountPurgeBatchSize,
		Interval:     defaultAccountPurgeInterval,
		BatchTimeout: defaultAccountPurgeBatchTimeout,
		Grace:        defaultAccountPurgeGrace,
		Mode:         AccountPurgeModePreview,
	}
}

// Validate checks the worker's bounded operational settings.
func (c AccountPurgeWorkerConfig) Validate() error {
	if err := validateAccountPurgeBatchSize(c.BatchSize); err != nil {
		return err
	}
	if c.Interval < time.Minute || c.Interval > 24*time.Hour {
		return errors.New("account purge interval must be between 1 minute and 24 hours")
	}
	if c.BatchTimeout < minAccountPurgeBatchTimeout ||
		c.BatchTimeout > maxAccountPurgeBatchTimeout {
		return fmt.Errorf(
			"account purge batch timeout must be between %s and %s",
			minAccountPurgeBatchTimeout,
			maxAccountPurgeBatchTimeout,
		)
	}
	if err := validateAccountPurgeGrace(c.Grace); err != nil {
		return err
	}
	switch c.Mode {
	case AccountPurgeModePreview, AccountPurgeModeEnforce:
	default:
		return fmt.Errorf(
			"account purge mode must be %q or %q",
			AccountPurgeModePreview,
			AccountPurgeModeEnforce,
		)
	}
	return nil
}

func validateAccountPurgeBatchSize(batchSize int) error {
	if batchSize < 1 || batchSize > maxAccountPurgeBatchSize {
		return fmt.Errorf(
			"account purge batch size must be between 1 and %d",
			maxAccountPurgeBatchSize,
		)
	}
	return nil
}

func validateAccountPurgeGrace(grace time.Duration) error {
	if grace < minAccountPurgeGrace || grace > maxAccountPurgeGrace {
		return fmt.Errorf(
			"account purge grace must be between %s and %s",
			minAccountPurgeGrace,
			maxAccountPurgeGrace,
		)
	}
	return nil
}

// AccountPurgeBatchResult contains value-free operational counts only.
// DeletedByTable reports would-delete row counts in preview mode and committed
// deleted row counts in enforce mode. ProvisionReceiptScrubs follows the same
// preview/enforce convention for the retained contact-derived fingerprint.
// AttachmentInvariantFailures counts accounts skipped after a value-free
// derived-counter assertion failed. Table names come only from the compiled
// archive and cell-local registries.
type AccountPurgeBatchResult struct {
	Scanned                     int64
	SkippedLocked               int64
	Eligible                    int64
	PurgedAccounts              int64
	DeletedByTable              map[string]int64
	DeferredVaultLifecycle      int64
	AttachmentInvariantFailures int64
	ProvisionReceiptScrubs      int64
}

// PreviewAccountPurgeBatch runs the production selection, lifecycle guard,
// cross-account guard, and per-table counts without deleting content,
// anonymizing accounts, setting purged_at, or writing audit events.
func (s *Store) PreviewAccountPurgeBatch(
	ctx context.Context,
	batchSize int,
	grace time.Duration,
) (AccountPurgeBatchResult, error) {
	return s.processAccountPurgeBatch(ctx, batchSize, grace, false, 0)
}

// ProcessAccountPurgeBatch deletes and anonymizes at most batchSize eligible
// accounts. Each account is independently revalidated and purged in one
// serializable transaction.
func (s *Store) ProcessAccountPurgeBatch(
	ctx context.Context,
	batchSize int,
	grace time.Duration,
) (AccountPurgeBatchResult, error) {
	return s.processAccountPurgeBatch(ctx, batchSize, grace, true, 0)
}

func (s *Store) processAccountPurgeBatch(
	ctx context.Context,
	batchSize int,
	grace time.Duration,
	enforce bool,
	workerInterval time.Duration,
) (AccountPurgeBatchResult, error) {
	if err := validateAccountPurgeBatchSize(batchSize); err != nil {
		return AccountPurgeBatchResult{}, err
	}
	if err := validateAccountPurgeGrace(grace); err != nil {
		return AccountPurgeBatchResult{}, err
	}
	if workerInterval < 0 {
		return AccountPurgeBatchResult{},
			errors.New("account purge worker interval cannot be negative")
	}

	mode := AccountPurgeModePreview
	if enforce {
		mode = AccountPurgeModeEnforce
	}
	claim, err := s.claimAccountPurgeCandidates(
		ctx, batchSize, grace, mode, workerInterval,
	)
	if err != nil {
		return AccountPurgeBatchResult{}, err
	}
	result := AccountPurgeBatchResult{
		Scanned:        claim.scanned,
		SkippedLocked:  claim.skippedLocked,
		DeletedByTable: make(map[string]int64),
	}
	for _, accountID := range claim.accountIDs {
		accountResult, err := s.processAccountPurgeCandidate(
			ctx, accountID, grace, enforce,
		)
		if accountResult.eligible {
			result.Eligible++
		}
		if accountResult.deferredVaultLifecycle {
			result.DeferredVaultLifecycle++
		}
		if accountResult.attachmentInvariantFailure {
			result.AttachmentInvariantFailures++
		}
		result.ProvisionReceiptScrubs += accountResult.provisionReceiptScrubs
		if accountResult.purged {
			result.PurgedAccounts++
		}
		mergeAccountPurgeTableCounts(
			result.DeletedByTable,
			accountResult.deletedByTable,
		)
		if err != nil {
			return result, fmt.Errorf("process account purge candidate: %w", err)
		}
	}
	return result, nil
}

type accountPurgeClaim struct {
	accountIDs    []string
	scanned       int64
	skippedLocked int64
}

func (s *Store) claimAccountPurgeCandidates(
	ctx context.Context,
	batchSize int,
	grace time.Duration,
	mode AccountPurgeMode,
	workerInterval time.Duration,
) (accountPurgeClaim, error) {
	var lastErr error
	for attempt := 0; attempt < maxAccountPurgeTxAttempts; attempt++ {
		claim, err := s.claimAccountPurgeCandidatesOnce(
			ctx, batchSize, grace, mode, workerInterval,
		)
		if err == nil || !isAccountPurgeSerializationFailure(err) {
			return claim, err
		}
		lastErr = err
	}
	return accountPurgeClaim{}, lastErr
}

func (s *Store) claimAccountPurgeCandidatesOnce(
	ctx context.Context,
	batchSize int,
	grace time.Duration,
	mode AccountPurgeMode,
	workerInterval time.Duration,
) (accountPurgeClaim, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return accountPurgeClaim{}, fmt.Errorf("begin account purge selection: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var previewCursor, enforceCursor string
	var generation int64
	err = tx.QueryRow(ctx, `
		SELECT preview_account_cursor,enforce_account_cursor,generation
		  FROM account_purge_sweep_state
		 WHERE singleton
		   AND (
		     $1::bigint = 0
		     OR next_run_at <= statement_timestamp()
		   )
		 FOR UPDATE SKIP LOCKED`,
		workerInterval.Microseconds(),
	).Scan(&previewCursor, &enforceCursor, &generation)
	if errors.Is(err, pgx.ErrNoRows) {
		var stateExists bool
		if err := tx.QueryRow(ctx, `
			SELECT EXISTS (
			  SELECT 1 FROM account_purge_sweep_state WHERE singleton
			)`).Scan(&stateExists); err != nil {
			return accountPurgeClaim{},
				fmt.Errorf("check account purge sweep state: %w", err)
		}
		if !stateExists {
			return accountPurgeClaim{},
				errors.New("account purge sweep state is missing")
		}
		if err := tx.Commit(ctx); err != nil {
			return accountPurgeClaim{},
				fmt.Errorf("commit idle account purge selection: %w", err)
		}
		return accountPurgeClaim{}, nil
	}
	if err != nil {
		return accountPurgeClaim{},
			fmt.Errorf("lock account purge sweep state: %w", err)
	}

	cursor := previewCursor
	if mode == AccountPurgeModeEnforce {
		cursor = enforceCursor
	}
	rows, err := tx.Query(ctx, `
		WITH cursor_position AS MATERIALIZED (
		  SELECT id,closed_at
		    FROM accounts
		   WHERE id=$2
		),
		candidate_page AS MATERIALIZED (
		  (
		    SELECT account.id,account.closed_at,0 AS segment
		      FROM accounts account
		      LEFT JOIN cursor_position cursor ON true
		     WHERE account.status='closed'
		       AND account.purged_at IS NULL
		       AND account.closed_at <
		           statement_timestamp() - ($3::bigint * interval '1 microsecond')
		       AND account.evacuation_id IS NULL
		       AND NOT account.is_default
		       AND (
		         cursor.id IS NULL
		         OR (account.closed_at,account.id) >
		            (cursor.closed_at,cursor.id)
		       )
		     ORDER BY account.closed_at,account.id
		     LIMIT $1
		  )
		  UNION ALL
		  (
		    SELECT account.id,account.closed_at,1 AS segment
		      FROM accounts account
		      JOIN cursor_position cursor ON true
		     WHERE account.status='closed'
		       AND account.purged_at IS NULL
		       AND account.closed_at <
		           statement_timestamp() - ($3::bigint * interval '1 microsecond')
		       AND account.evacuation_id IS NULL
		       AND NOT account.is_default
		       AND (account.closed_at,account.id) <=
		           (cursor.closed_at,cursor.id)
		     ORDER BY account.closed_at,account.id
		     LIMIT $1
		  )
		)
		SELECT id
		  FROM candidate_page
		 ORDER BY segment,closed_at,id
		 LIMIT $1`,
		batchSize,
		cursor,
		grace.Microseconds(),
	)
	if err != nil {
		return accountPurgeClaim{},
			fmt.Errorf("select account purge candidates: %w", err)
	}
	var candidateIDs []string
	for rows.Next() {
		var accountID string
		if err := rows.Scan(&accountID); err != nil {
			rows.Close()
			return accountPurgeClaim{},
				fmt.Errorf("read account purge candidate: %w", err)
		}
		candidateIDs = append(candidateIDs, accountID)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return accountPurgeClaim{},
			fmt.Errorf("scan account purge candidates: %w", err)
	}
	rows.Close()

	lockedIDs := make([]string, 0, len(candidateIDs))
	if len(candidateIDs) > 0 {
		rows, err = tx.Query(ctx, `
			SELECT account.id
			  FROM unnest($1::text[]) WITH ORDINALITY candidate(id,ordinality)
			  JOIN accounts account ON account.id=candidate.id
			 WHERE account.status='closed'
			   AND account.purged_at IS NULL
			   AND account.closed_at <
			       statement_timestamp() - ($2::bigint * interval '1 microsecond')
			   AND account.evacuation_id IS NULL
			   AND NOT account.is_default
			 ORDER BY candidate.ordinality
			 FOR UPDATE OF account SKIP LOCKED`,
			candidateIDs,
			grace.Microseconds(),
		)
		if err != nil {
			return accountPurgeClaim{},
				fmt.Errorf("lock account purge candidates: %w", err)
		}
		for rows.Next() {
			var accountID string
			if err := rows.Scan(&accountID); err != nil {
				rows.Close()
				return accountPurgeClaim{},
					fmt.Errorf("read locked account purge candidate: %w", err)
			}
			lockedIDs = append(lockedIDs, accountID)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return accountPurgeClaim{},
				fmt.Errorf("scan locked account purge candidates: %w", err)
		}
		rows.Close()
	}

	nextCursor := ""
	if len(candidateIDs) > 0 {
		nextCursor = candidateIDs[len(candidateIDs)-1]
	}
	nextGeneration := generation + 1
	if generation == 4611686018427387903 {
		nextGeneration = 1
	}
	tag, err := tx.Exec(ctx, `
		UPDATE account_purge_sweep_state
		   SET preview_account_cursor=CASE
		         WHEN $1='preview' AND $2<>'' THEN $2
		         ELSE preview_account_cursor
		       END,
		       enforce_account_cursor=CASE
		         WHEN $1='enforce' AND $2<>'' THEN $2
		         ELSE enforce_account_cursor
		       END,
		       generation=$3,
		       next_run_at=CASE
		         WHEN $4::bigint > 0
		         THEN statement_timestamp() +
		              ($4::bigint * interval '1 microsecond')
		         ELSE next_run_at
		       END,
		       updated_at=statement_timestamp()
		 WHERE singleton AND generation=$5`,
		string(mode),
		nextCursor,
		nextGeneration,
		workerInterval.Microseconds(),
		generation,
	)
	if err != nil {
		return accountPurgeClaim{},
			fmt.Errorf("advance account purge sweep state: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return accountPurgeClaim{},
			errors.New("advance account purge sweep state lost its fence")
	}
	if err := tx.Commit(ctx); err != nil {
		return accountPurgeClaim{},
			fmt.Errorf("commit account purge selection: %w", err)
	}
	return accountPurgeClaim{
		accountIDs:    lockedIDs,
		scanned:       int64(len(candidateIDs)),
		skippedLocked: int64(len(candidateIDs) - len(lockedIDs)),
	}, nil
}

type accountPurgeCandidateResult struct {
	eligible                   bool
	purged                     bool
	deferredVaultLifecycle     bool
	attachmentInvariantFailure bool
	provisionReceiptScrubs     int64
	deletedByTable             map[string]int64
}

func (s *Store) processAccountPurgeCandidate(
	ctx context.Context,
	accountID string,
	grace time.Duration,
	enforce bool,
) (accountPurgeCandidateResult, error) {
	var lastErr error
	for attempt := 0; attempt < maxAccountPurgeTxAttempts; attempt++ {
		result, err := s.processAccountPurgeCandidateOnce(
			ctx, accountID, grace, enforce,
		)
		if err == nil || !isAccountPurgeSerializationFailure(err) {
			return result, err
		}
		lastErr = err
	}
	return accountPurgeCandidateResult{}, lastErr
}

func (s *Store) processAccountPurgeCandidateOnce(
	ctx context.Context,
	accountID string,
	grace time.Duration,
	enforce bool,
) (accountPurgeCandidateResult, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return accountPurgeCandidateResult{},
			fmt.Errorf("begin account purge transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var retainedAttachmentBytes int64
	err = tx.QueryRow(ctx, `
		SELECT retained_agent_email_attachment_bytes
		  FROM accounts
		 WHERE id=$1
		   AND status='closed'
		   AND purged_at IS NULL
		   AND closed_at <
		       statement_timestamp() - ($2::bigint * interval '1 microsecond')
		   AND evacuation_id IS NULL
		   AND NOT is_default
		 FOR UPDATE`,
		accountID,
		grace.Microseconds(),
	).Scan(&retainedAttachmentBytes)
	if errors.Is(err, pgx.ErrNoRows) {
		if err := tx.Commit(ctx); err != nil {
			return accountPurgeCandidateResult{},
				fmt.Errorf("commit ineligible account purge: %w", err)
		}
		return accountPurgeCandidateResult{}, nil
	}
	if err != nil {
		return accountPurgeCandidateResult{},
			fmt.Errorf("lock account purge candidate: %w", err)
	}

	result := accountPurgeCandidateResult{
		eligible:       true,
		deletedByTable: make(map[string]int64),
	}
	deferred, err := accountPurgeVaultLifecycleActiveTx(ctx, tx, accountID)
	if err != nil {
		return result, err
	}
	if deferred {
		result.deferredVaultLifecycle = true
		if err := tx.Commit(ctx); err != nil {
			return result,
				fmt.Errorf("commit deferred account purge: %w", err)
		}
		return result, nil
	}

	schemaVersion, err := accountPurgeSchemaVersionTx(ctx, tx)
	if err != nil {
		return result, err
	}
	if !enforce {
		if err := rejectCrossAccountEvacuationCascadesTx(
			ctx, tx, accountID,
		); err != nil {
			return result, err
		}
		portableCounts, err := countPortableAccountRowsTx(
			ctx, tx, accountID, schemaVersion,
		)
		if err != nil {
			return result, err
		}
		localCounts, err := countCellLocalAccountRowsTx(ctx, tx, accountID)
		if err != nil {
			return result, err
		}
		provisionReceiptScrubs, err := countAccountProvisionReceiptScrubsTx(
			ctx, tx, accountID,
		)
		if err != nil {
			return result, err
		}
		mergeAccountPurgeTableCounts(result.deletedByTable, portableCounts)
		mergeAccountPurgeTableCounts(result.deletedByTable, localCounts)
		result.provisionReceiptScrubs = provisionReceiptScrubs
		if err := tx.Commit(ctx); err != nil {
			return result,
				fmt.Errorf("commit account purge preview: %w", err)
		}
		return result, nil
	}

	localCounts, err := countCellLocalAccountRowsTx(ctx, tx, accountID)
	if err != nil {
		return result, err
	}
	portableCounts, err := purgePortableAccountRowsTx(
		ctx, tx, accountID, schemaVersion,
	)
	if err != nil {
		if isAccountPurgeAttachmentUnderflow(err) {
			return skipAccountPurgeAttachmentInvariant(ctx, tx, result)
		}
		return result, err
	}
	if err := purgeCellLocalAccountRowsTx(ctx, tx, accountID); err != nil {
		return result, err
	}
	if err := tx.QueryRow(ctx, `
		SELECT retained_agent_email_attachment_bytes
		  FROM accounts
		 WHERE id=$1`,
		accountID,
	).Scan(&retainedAttachmentBytes); err != nil {
		return result,
			fmt.Errorf("read account purge attachment counter: %w", err)
	}
	if retainedAttachmentBytes != 0 {
		// This is a per-account invariant failure, not a reason to starve later
		// accounts in the already-claimed page. The explicit rollback restores
		// every delete; the aggregate result reports only a value-free failure
		// count.
		return skipAccountPurgeAttachmentInvariant(ctx, tx, result)
	}

	provisionReceiptScrubs, err := scrubAccountProvisionReceiptTx(
		ctx, tx, accountID,
	)
	if err != nil {
		return result, err
	}

	tag, err := tx.Exec(ctx, `
		UPDATE accounts
		   SET email=NULL,
		       display_name='',
		       consent_terms_version=NULL,
		       consent_privacy_version=NULL,
		       consent_recorded_at=NULL,
		       closed_reason='',
		       suspended_for=NULL,
		       suspended_reason=NULL,
		       plan_limits='{}'::jsonb,
		       plan_features='[]'::jsonb,
		       plan_policies='{}'::jsonb,
		       placement_policy=DEFAULT,
		       purged_at=clock_timestamp()
		 WHERE id=$1
		   AND status='closed'
		   AND purged_at IS NULL
		   AND closed_at <
		       statement_timestamp() - ($2::bigint * interval '1 microsecond')
		   AND evacuation_id IS NULL
		   AND NOT is_default
		   AND retained_agent_email_attachment_bytes=0`,
		accountID,
		grace.Microseconds(),
	)
	if err != nil {
		return result, fmt.Errorf("anonymize purged account: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return result,
			errors.New("anonymize purged account lost its eligibility fence")
	}
	if err := logEventTx(ctx, tx, EventInput{
		AccountID:              accountID,
		ActorKind:              ActorSystem,
		Verb:                   VerbAccountPurged,
		Metadata:               map[string]any{},
		accountPurgeTransition: true,
	}); err != nil {
		return result, fmt.Errorf("record account purge event: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return result, fmt.Errorf("commit account purge: %w", err)
	}
	mergeAccountPurgeTableCounts(result.deletedByTable, portableCounts)
	mergeAccountPurgeTableCounts(result.deletedByTable, localCounts)
	result.provisionReceiptScrubs = provisionReceiptScrubs
	result.purged = true
	return result, nil
}

func skipAccountPurgeAttachmentInvariant(
	ctx context.Context,
	tx pgx.Tx,
	result accountPurgeCandidateResult,
) (accountPurgeCandidateResult, error) {
	result.attachmentInvariantFailure = true
	if err := tx.Rollback(ctx); err != nil {
		return result, fmt.Errorf(
			"rollback account purge attachment invariant failure: %w",
			err,
		)
	}
	return result, nil
}

func isAccountPurgeAttachmentUnderflow(err error) bool {
	var postgresError *pgconn.PgError
	return errors.As(err, &postgresError) &&
		postgresError.Message == accountPurgeAttachmentUnderflow
}

func countAccountProvisionReceiptScrubsTx(
	ctx context.Context,
	tx pgx.Tx,
	accountID string,
) (int64, error) {
	var count int64
	if err := tx.QueryRow(ctx, `
		SELECT count(*)
		  FROM account_provision_receipts
		 WHERE account_id=$1
		   AND request_fingerprint<>$2`,
		accountID,
		purgedProvisionRequestFingerprint,
	).Scan(&count); err != nil {
		return 0, fmt.Errorf(
			"count account provision receipts requiring purge scrub: %w",
			err,
		)
	}
	return count, nil
}

func scrubAccountProvisionReceiptTx(
	ctx context.Context,
	tx pgx.Tx,
	accountID string,
) (int64, error) {
	tag, err := tx.Exec(ctx, `
		UPDATE account_provision_receipts
		   SET request_fingerprint=$2
		 WHERE account_id=$1
		   AND request_fingerprint<>$2`,
		accountID,
		purgedProvisionRequestFingerprint,
	)
	if err != nil {
		return 0, fmt.Errorf("scrub account provision receipt: %w", err)
	}
	return tag.RowsAffected(), nil
}

func accountPurgeVaultLifecycleActiveTx(
	ctx context.Context,
	tx pgx.Tx,
	accountID string,
) (bool, error) {
	var active bool
	if err := tx.QueryRow(ctx, `
		SELECT EXISTS (
		  SELECT 1 FROM agent_vault_key_enrollments
		   WHERE account_id=$1 AND lifecycle_state IN ('pending','approved')
		) OR EXISTS (
		  SELECT 1 FROM agent_vault_key_rotations
		   WHERE account_id=$1 AND lifecycle_state='open'
		) OR EXISTS (
		  SELECT 1 FROM agent_vault_keys
		   WHERE account_id=$1 AND lifecycle_state='pending'
		)`,
		accountID,
	).Scan(&active); err != nil {
		return false, fmt.Errorf(
			"check vault key lifecycle before account purge: %w",
			err,
		)
	}
	return active, nil
}

func accountPurgeSchemaVersionTx(
	ctx context.Context,
	tx pgx.Tx,
) (int, error) {
	var schemaVersion int
	if err := tx.QueryRow(ctx, `
		SELECT coalesce(max(version_id), 0)
		  FROM (
		        SELECT DISTINCT ON (version_id)
		               version_id, is_applied
		          FROM goose_db_version
		         ORDER BY version_id, id DESC
		       ) AS latest_version
		 WHERE is_applied`).Scan(&schemaVersion); err != nil {
		return 0, fmt.Errorf("read account purge schema version: %w", err)
	}
	return schemaVersion, nil
}

func countPortableAccountRowsTx(
	ctx context.Context,
	tx pgx.Tx,
	accountID string,
	schemaVersion int,
) (map[string]int64, error) {
	counts := make(map[string]int64)
	for _, table := range canonicalArchiveTableNamesForSchema(schemaVersion) {
		var query string
		switch table {
		case "accounts":
			continue
		case "agents":
			query = `
				SELECT count(*) FROM agents
				 WHERE realm_id IN (
				       SELECT id FROM realms WHERE account_id=$1
				 )`
		case "agent_activity":
			query = `
				SELECT count(*) FROM agent_activity
				 WHERE agent_id IN (
				       SELECT agent.id
				         FROM agents agent
				         JOIN realms realm ON realm.id=agent.realm_id
				        WHERE realm.account_id=$1
				 )`
		default:
			query = fmt.Sprintf(
				"SELECT count(*) FROM %s WHERE account_id=$1",
				pgx.Identifier{table}.Sanitize(),
			)
		}
		var count int64
		if err := tx.QueryRow(ctx, query, accountID).Scan(&count); err != nil {
			return nil, fmt.Errorf(
				"count account purge table %s: %w",
				table,
				err,
			)
		}
		counts[table] = count
	}
	return counts, nil
}

var cellLocalAccountPurgeTables = []string{
	"transcript_retention_account_scan_state",
	"message_retention_account_scan_state",
	"agent_email_retention_account_scan_state",
	"message_retention_thread_activity",
	"agent_message_rate_buckets",
	"agent_email_rate_buckets",
	"agent_email_outbound_rate_buckets",
	"agent_email_account_rate_buckets",
}

func countCellLocalAccountRowsTx(
	ctx context.Context,
	tx pgx.Tx,
	accountID string,
) (map[string]int64, error) {
	counts := make(map[string]int64, len(cellLocalAccountPurgeTables))
	for _, table := range cellLocalAccountPurgeTables {
		query := fmt.Sprintf(
			"SELECT count(*) FROM %s WHERE account_id=$1",
			pgx.Identifier{table}.Sanitize(),
		)
		var count int64
		if err := tx.QueryRow(ctx, query, accountID).Scan(&count); err != nil {
			return nil, fmt.Errorf(
				"count cell-local account purge table %s: %w",
				table,
				err,
			)
		}
		counts[table] = count
	}
	return counts, nil
}

func purgeCellLocalAccountRowsTx(
	ctx context.Context,
	tx pgx.Tx,
	accountID string,
) error {
	for _, table := range cellLocalAccountPurgeTables {
		query := fmt.Sprintf(
			"DELETE FROM %s WHERE account_id=$1",
			pgx.Identifier{table}.Sanitize(),
		)
		if _, err := tx.Exec(ctx, query, accountID); err != nil {
			return fmt.Errorf(
				"purge cell-local account table %s: %w",
				table,
				err,
			)
		}
	}
	return nil
}

func mergeAccountPurgeTableCounts(
	target map[string]int64,
	values map[string]int64,
) {
	for table, count := range values {
		target[table] += count
	}
}

func isAccountPurgeSerializationFailure(err error) bool {
	var postgresError *pgconn.PgError
	return errors.As(err, &postgresError) &&
		postgresError.Code == "40001"
}

// RunAccountPurgeWorker attempts one bounded batch immediately and on each
// local interval. The singleton sweep row supplies the cross-replica durable
// next-run fence, while each selected account is purged in its own deadline-
// bounded serializable transaction.
func (s *Store) RunAccountPurgeWorker(
	ctx context.Context,
	cfg AccountPurgeWorkerConfig,
	onResult func(AccountPurgeBatchResult),
	onError func(error),
) error {
	if err := cfg.Validate(); err != nil {
		return err
	}
	run := func() {
		attemptCtx, cancelAttempt := context.WithTimeout(ctx, cfg.BatchTimeout)
		defer cancelAttempt()
		result, err := s.processAccountPurgeBatch(
			attemptCtx,
			cfg.BatchSize,
			cfg.Grace,
			cfg.Mode == AccountPurgeModeEnforce,
			cfg.Interval,
		)
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
