package store

import (
	"context"
	"errors"
	"math/rand/v2"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

const transcriptStorageUsageBulkSetting = "witself.transcript_storage_usage_bulk"
const transcriptStorageReconcileLimit int64 = 4611686018427387903
const transcriptStorageReconcileEligibleSQL = `a.evacuation_id IS NULL AND NOT (a.status = 'closed' AND a.purged_at IS NULL)`
const transcriptStorageReconcileLeaseKeySQL = `hashtextextended(current_database() || ':witself:transcript-storage-reconcile:v1', 0)`

// enableTranscriptStorageUsageBulkTx skips trigger accounting until this
// transaction ends. Import marks every shard pending after loading all entries.
func enableTranscriptStorageUsageBulkTx(ctx context.Context, tx pgx.Tx) error {
	_, err := tx.Exec(ctx, `SELECT set_config($1, 'on', true)`, transcriptStorageUsageBulkSetting)
	return err
}

// markImportedTranscriptStorageUsagePendingTx invalidates earlier measurements
// without scanning entries. It also resets orphan rows from an earlier import.
func markImportedTranscriptStorageUsagePendingTx(ctx context.Context, tx pgx.Tx, accountID string) error {
	_, err := tx.Exec(ctx, `
 INSERT INTO transcript_storage_usage AS u (account_id, shard, retained_bytes, exact, verified_at)
 SELECT $1, g.s::smallint, 0, false, NULL FROM generate_series(0, 15) AS g(s) ORDER BY 2
 ON CONFLICT (account_id, shard) DO UPDATE
 SET retained_bytes = 0, exact = false, verified_at = NULL,
     reconcile_epoch = nextval('transcript_storage_usage_epoch_seq')`, accountID)
	return err
}

// transcriptStorageUsageTx reports retained bytes and their reliability. A2 must
// fail open, and never refuse an append, when exact is false.
func transcriptStorageUsageTx(ctx context.Context, tx pgx.Tx, accountID string) (used int64, exact bool, err error) {
	err = tx.QueryRow(ctx, `
 SELECT GREATEST(COALESCE(sum(retained_bytes), 0), 0)::bigint,
        COALESCE(bool_and(exact AND retained_bytes >= 0), true)
 FROM transcript_storage_usage WHERE account_id = $1`, accountID).Scan(&used, &exact)
	return
}

func transcriptEntriesStorageBytesTx(ctx context.Context, tx pgx.Tx, accountID string, entryIDs []string) (int64, error) {
	var used int64
	err := tx.QueryRow(ctx, `
 SELECT COALESCE(sum(witself_transcript_entry_storage_bytes(
 external_id, role, body, payload, model, reply_to_entry_id, artifacts
 )),0)::bigint FROM transcript_entries WHERE account_id=$1 AND id = ANY($2)`, accountID, entryIDs).Scan(&used)
	return used, err
}

func (s *Store) acquireTranscriptStorageReconcileLease(ctx context.Context) (*transcriptStorageReconcileLease, bool, error) {
	acquireCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	conn, err := s.pool.Acquire(acquireCtx)
	if err != nil {
		return nil, false, err
	}
	lease := &transcriptStorageReconcileLease{conn: conn}
	if err = conn.QueryRow(acquireCtx, `SELECT pg_try_advisory_lock(`+transcriptStorageReconcileLeaseKeySQL+`)`).Scan(&lease.held); err != nil {
		lease.discard = true
		lease.close()
		return nil, false, err
	}
	if !lease.held {
		return lease, false, nil
	}
	_, err = conn.Exec(acquireCtx, `SET application_name = 'witself-transcript-storage-reconcile';
 SET tcp_keepalives_idle = 30; SET tcp_keepalives_interval = 10; SET tcp_keepalives_count = 6;
 SET client_connection_check_interval = '2s'; SET lock_timeout = '500ms'`)
	if err == nil {
		err = conn.QueryRow(acquireCtx, `SELECT pg_backend_pid()`).Scan(&lease.pid)
	}
	if err != nil {
		lease.discard = true
		lease.close()
		return nil, false, err
	}
	return lease, true, nil
}

func (l *transcriptStorageReconcileLease) close() {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if !l.discard && l.held {
		var unlocked bool
		err := l.conn.QueryRow(ctx, `SELECT pg_advisory_unlock(`+transcriptStorageReconcileLeaseKeySQL+`)`).Scan(&unlocked)
		if err != nil || !unlocked {
			l.discard = true
		}
	}
	if !l.discard {
		if _, err := l.conn.Exec(ctx, `RESET ALL`); err != nil {
			l.discard = true
		}
	}
	if l.discard {
		_ = l.conn.Hijack().Close(ctx)
	} else {
		l.conn.Release()
	}
}

func (l *transcriptStorageReconcileLease) status(ctx context.Context) (TranscriptStorageReconcileStatus, error) {
	var result TranscriptStorageReconcileStatus
	err := l.conn.QueryRow(ctx, `
 SELECT count(*) FILTER (WHERE `+transcriptStorageReconcileEligibleSQL+`),
        count(*) FILTER (WHERE NOT (`+transcriptStorageReconcileEligibleSQL+`))
 FROM transcript_storage_usage u JOIN accounts a ON a.id=u.account_id WHERE NOT u.exact`).Scan(&result.UnverifiedEligible, &result.UnverifiedDeferred)
	return result, err
}

// TranscriptStorageReconcileStatus excludes harmless orphan rows by joining accounts.
// Stuck is runner-local and is supplied by the worker's status callbacks.
func (s *Store) TranscriptStorageReconcileStatus(ctx context.Context) (TranscriptStorageReconcileStatus, error) {
	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		return TranscriptStorageReconcileStatus{}, err
	}
	defer conn.Release()
	return (&transcriptStorageReconcileLease{conn: conn}).status(ctx)
}

func (l *transcriptStorageReconcileLease) pick(ctx context.Context, cursor transcriptStorageReconcileUnit) ([]transcriptStorageReconcileUnit, error) {
	rows, err := l.conn.Query(ctx, `
 SELECT u.account_id,u.shard FROM transcript_storage_usage u JOIN accounts a ON a.id=u.account_id
 WHERE NOT u.exact AND `+transcriptStorageReconcileEligibleSQL+` AND (u.account_id,u.shard)>($1,$2)
 ORDER BY u.account_id,u.shard LIMIT 32`, cursor.accountID, cursor.shard)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var units []transcriptStorageReconcileUnit
	for rows.Next() {
		var u transcriptStorageReconcileUnit
		if err = rows.Scan(&u.accountID, &u.shard); err != nil {
			return nil, err
		}
		units = append(units, u)
	}
	return units, rows.Err()
}

// The recursion performs one index probe for each distinct transcript id, never
// materializing or sorting the account's entries. The cursor advances over all
// ids, including ids in other shards, so each statement has a bounded 256 probes.
const transcriptStorageReconcileListingSQL = `
 WITH RECURSIVE ids(id,n) AS (
   (SELECT transcript_id,1 FROM transcript_entries
    WHERE account_id=$1 AND transcript_id>$2 ORDER BY transcript_id LIMIT 1)
   UNION ALL
   SELECT next.id,ids.n+1 FROM ids CROSS JOIN LATERAL (
     SELECT transcript_id AS id FROM transcript_entries
     WHERE account_id=$1 AND transcript_id>ids.id ORDER BY transcript_id LIMIT 1
   ) next WHERE ids.n<256
 ) SELECT id,witself_transcript_storage_shard(id)=$3 FROM ids ORDER BY id`

func transcriptStorageReconcileList(ctx context.Context, tx pgx.Tx, unit transcriptStorageReconcileUnit, after string) (ids []string, last string, count int, err error) {
	rows, err := tx.Query(ctx, transcriptStorageReconcileListingSQL, unit.accountID, after, unit.shard)
	if err != nil {
		return nil, "", 0, err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var matches bool
		if err = rows.Scan(&id, &matches); err != nil {
			return nil, "", 0, err
		}
		last = id
		count++
		if matches {
			ids = append(ids, id)
		}
	}
	return ids, last, count, rows.Err()
}

func (l *transcriptStorageReconcileLease) measure(ctx context.Context, cfg TranscriptStorageReconcileConfig, unit transcriptStorageReconcileUnit) (m transcriptStorageReconcileMeasurement, outcome string, err error) {
	defer func() {
		if err != nil && l.conn.Conn().IsClosed() {
			l.discard = true
		}
	}()
	tx, err := l.conn.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return m, "", err
	}
	defer func() {
		rollbackCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = tx.Rollback(rollbackCtx)
	}()
	statementTimeout := min(60*time.Second, cfg.UnitTimeout)
	idleTimeout := 10 * time.Second
	if h := transcriptStorageReconcileTestHooks; h != nil && h.measureIdleTimeout != nil {
		idleTimeout = *h.measureIdleTimeout
	}
	// This first statement establishes the same snapshot for both C and E.
	_, err = tx.Exec(ctx, `SELECT set_config('statement_timeout',$1,true), set_config('idle_in_transaction_session_timeout',$2,true)`, transcriptStorageReconcilePostgresDuration(statementTimeout), transcriptStorageReconcilePostgresDuration(idleTimeout))
	if err != nil {
		return m, "", err
	}
	var exact bool
	err = tx.QueryRow(ctx, `SELECT retained_bytes,reconcile_epoch,exact FROM transcript_storage_usage WHERE account_id=$1 AND shard=$2`, unit.accountID, unit.shard).Scan(&m.counter, &m.epoch, &exact)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && exact) {
		return m, "skipped", nil
	}
	if err != nil {
		return m, "", err
	}
	if h := transcriptStorageReconcileTestHooks; h != nil {
		if err = transcriptStorageReconcileHook(ctx, l.pid, h.afterCounterRead); err != nil {
			return m, "", err
		}
	}
	pace := func(elapsed time.Duration) error {
		return transcriptStorageReconcilePace(ctx, elapsed, cfg.DutyCycle, statementTimeout, time.Now, func(ctx context.Context, d time.Duration) error {
			_, sleepErr := tx.Exec(ctx, `SELECT pg_sleep($1)`, d.Seconds())
			return sleepErr
		})
	}
	after := ""
	for {
		start := time.Now()
		ids, last, count, listErr := transcriptStorageReconcileList(ctx, tx, unit, after)
		elapsed := time.Since(start)
		if listErr != nil {
			return m, "", listErr
		}
		if err = pace(elapsed); err != nil {
			return m, "", err
		}
		for _, id := range ids {
			var sequence int64
			for {
				var entries, bytes, next int64
				start = time.Now()
				err = tx.QueryRow(ctx, `
    SELECT count(*),COALESCE(sum(witself_transcript_entry_storage_bytes(
      external_id,role,body,payload,model,reply_to_entry_id,artifacts)),0)::bigint,
      COALESCE(max(sequence),0) FROM (
      SELECT sequence,external_id,role,body,payload,model,reply_to_entry_id,artifacts FROM transcript_entries
      WHERE account_id=$1 AND transcript_id=$2 AND sequence>$3 ORDER BY sequence LIMIT $4
    ) page`, unit.accountID, id, sequence, cfg.PageEntries).Scan(&entries, &bytes, &next)
				elapsed = time.Since(start)
				if err != nil {
					return m, "", err
				}
				if bytes < 0 || bytes > transcriptStorageReconcileLimit-m.bytes {
					return m, "", errors.New("transcript storage reconcile byte sum out of range")
				}
				m.entries += entries
				m.bytes += bytes
				if h := transcriptStorageReconcileTestHooks; h != nil {
					if err = transcriptStorageReconcileHook(ctx, l.pid, h.afterPage); err != nil {
						return m, "", err
					}
				}
				if err = pace(elapsed); err != nil {
					return m, "", err
				}
				if entries < int64(cfg.PageEntries) {
					break
				}
				sequence = next
			}
		}
		if count < 256 {
			break
		}
		after = last
	}
	if err = tx.Commit(ctx); err != nil {
		return m, "", err
	}
	m.delta = m.bytes - m.counter
	// E and C are each bounded by the counter's half-int64 range. Their
	// difference fits int64 even when repairing a large negative counter.
	return m, "", nil
}

func (l *transcriptStorageReconcileLease) apply(ctx context.Context, unit transcriptStorageReconcileUnit, m transcriptStorageReconcileMeasurement) (outcome string, err error) {
	defer func() {
		if err != nil && l.conn.Conn().IsClosed() {
			l.discard = true
		}
	}()
	if h := transcriptStorageReconcileTestHooks; h != nil {
		if err := transcriptStorageReconcileHook(ctx, l.pid, h.beforeApply); err != nil {
			return "", err
		}
		if h.applyLockTimeout != nil {
			if _, err := l.conn.Exec(ctx, `SELECT set_config('lock_timeout',$1,false)`, transcriptStorageReconcilePostgresDuration(*h.applyLockTimeout)); err != nil {
				return "", err
			}
		}
	}
	applyCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	for attempt := 0; attempt < 20; attempt++ {
		var retained int64
		err := l.conn.QueryRow(applyCtx, `
  UPDATE transcript_storage_usage u
  SET retained_bytes=u.retained_bytes+$3,
      exact=(u.retained_bytes+$3>=0),
      reconcile_epoch=nextval('transcript_storage_usage_epoch_seq'),verified_at=clock_timestamp()
  WHERE u.account_id=$1 AND u.shard=$2 AND u.reconcile_epoch=$4
    AND EXISTS (SELECT 1 FROM accounts a WHERE a.id=u.account_id AND `+transcriptStorageReconcileEligibleSQL+`)
  RETURNING u.retained_bytes`, unit.accountID, unit.shard, m.delta, m.epoch).Scan(&retained)
		if errors.Is(err, pgx.ErrNoRows) {
			return "raced", nil
		}
		if err == nil {
			if h := transcriptStorageReconcileTestHooks; h != nil {
				if err = transcriptStorageReconcileHook(ctx, l.pid, h.afterApply); err != nil {
					return "", err
				}
			}
			if retained < 0 {
				return "anomaly", nil
			}
			if m.delta == 0 {
				return "confirmed", nil
			}
			return "corrected", nil
		}
		if applyCtx.Err() != nil && ctx.Err() == nil {
			// Exhausting the apply retry budget is busy; only the enclosing
			// unit deadline is a timeout. A cancelled statement discards its
			// connection even when the unit still has time remaining.
			l.discard = true
			return "busy", nil
		}
		var pgerr *pgconn.PgError
		if !errors.As(err, &pgerr) || pgerr.Code != "55P03" {
			return "", err
		}
		if attempt == 19 {
			break
		}
		delay := 200*time.Millisecond + time.Duration(rand.Int64N(int64(800*time.Millisecond)+1))
		if !waitForMessageRateBucketCleanupInterval(applyCtx, delay) {
			if ctx.Err() != nil {
				return "", ctx.Err()
			}
			return "busy", nil
		}
	}
	return "busy", nil
}

// VerifyTranscriptStorageUsage is an independent SQL recount, for operators and
// tests only. For a large account it reads every entry: the founder costs about
// the old aggregate, spread over sixteen bounded snapshots. It never uses the
// paged measurement or calls the worker. Absent rows count as zero and exact.
func (s *Store) VerifyTranscriptStorageUsage(ctx context.Context, accountID string) (result TranscriptStorageVerification, err error) {
	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		return result, err
	}
	lease := &transcriptStorageReconcileLease{conn: conn}
	defer lease.close()
	_, err = conn.Exec(ctx, `SET application_name='witself-transcript-storage-verify'`)
	if err != nil {
		lease.discard = true
		return result, err
	}
	result.Exact = true
	for shard := int16(0); shard < 16; shard++ {
		cfg := DefaultTranscriptStorageReconcileConfig()
		unitCtx, cancel := context.WithTimeout(ctx, cfg.UnitTimeout)
		mismatch, exact, verifyErr := verifyTranscriptStorageShard(unitCtx, conn, transcriptStorageReconcileUnit{accountID, shard}, cfg.UnitTimeout)
		cancel()
		if verifyErr != nil {
			lease.discard = true
			return result, verifyErr
		}
		if mismatch {
			result.Mismatches++
		}
		result.Exact = result.Exact && exact
	}
	return result, nil
}

func verifyTranscriptStorageShard(ctx context.Context, conn interface {
	BeginTx(context.Context, pgx.TxOptions) (pgx.Tx, error)
}, unit transcriptStorageReconcileUnit, unitTimeout time.Duration) (mismatch, exact bool, err error) {
	tx, err := conn.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return false, false, err
	}
	defer func() {
		rollbackCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = tx.Rollback(rollbackCtx)
	}()
	if _, err = tx.Exec(ctx, `SELECT set_config('statement_timeout',$1,true)`, transcriptStorageReconcilePostgresDuration(min(60*time.Second, unitTimeout))); err != nil {
		return false, false, err
	}
	var ids []string
	after := ""
	for {
		batch, last, count, listErr := transcriptStorageReconcileList(ctx, tx, unit, after)
		if listErr != nil {
			return false, false, listErr
		}
		ids = append(ids, batch...)
		if count < 256 {
			break
		}
		after = last
	}
	var actual, counter int64
	err = tx.QueryRow(ctx, `
 SELECT COALESCE(sum(witself_transcript_entry_storage_bytes(external_id,role,body,payload,model,reply_to_entry_id,artifacts)),0)::bigint
 FROM transcript_entries WHERE account_id=$1 AND transcript_id=ANY($2)`, unit.accountID, ids).Scan(&actual)
	if err != nil {
		return false, false, err
	}
	exact = true
	err = tx.QueryRow(ctx, `SELECT retained_bytes,exact AND retained_bytes>=0 FROM transcript_storage_usage WHERE account_id=$1 AND shard=$2`, unit.accountID, unit.shard).Scan(&counter, &exact)
	if errors.Is(err, pgx.ErrNoRows) {
		err = nil
		exact = true
	}
	if err != nil {
		return false, false, err
	}
	return counter != actual, exact, tx.Commit(ctx)
}

func transcriptStorageReconcilePostgresDuration(d time.Duration) string {
	return strconv.FormatInt(d.Milliseconds(), 10) + "ms"
}
