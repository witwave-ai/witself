package store

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/witwave-ai/witself/internal/runtimeconfig"
)

// SyncCaptureOptions binds activation to an already installed incarnation.
// It grants no transport, application, or snapshot-materialization authority.
type SyncCaptureOptions struct {
	LifecycleEpoch int64
	RestoreMarker  []byte
	Capability     string
}

// SyncCaptureActivation owns the physical session gate until acknowledgment.
// Its connection is never substituted after a failure.
type SyncCaptureActivation struct {
	store     *Store
	conn      *pgxpool.Conn
	account   string
	pid       uint32
	epoch     int64
	snapshot  []byte
	watermark int64
	closed    bool
}

// SyncSnapshotPin retains the read-only repeatable-read transaction. Its fields
// are private: only the original gate-owning activation can acknowledge it.
type SyncSnapshotPin struct {
	activation   *SyncCaptureActivation
	tx           pgx.Tx
	epoch        int64
	snapshot     []byte
	watermark    int64
	acknowledged bool
}

// Acknowledged reports only the same-session pin handshake, never readiness.
func (p *SyncSnapshotPin) Acknowledged() bool { return p != nil && p.acknowledged }

// Close releases the candidate snapshot without materializing it.
func (p *SyncSnapshotPin) Close(ctx context.Context) error { return p.tx.Rollback(ctx) }

// EnableSyncWriter explicitly enables the one supported writer capability in a
// selected local store. Real database-principal isolation belongs to LC-S6C2.
func (s *Store) EnableSyncWriter(ctx context.Context, account, principal, capability string) error {
	if !s.syncCaptureSelected.Load() || account == "" || principal == "" || capability != SyncPreferencesCapability {
		return ErrSyncWriterNotAdmitted
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return ErrSyncWriterNotAdmitted
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(capture_lock_key($1))`, account); err != nil {
		return ErrSyncWriterNotAdmitted
	}
	if !syncCaptureCoverageValid(ctx, tx) {
		return ErrSyncWriterNotAdmitted
	}
	if !syncCaptureAccountSnapshotSafe(ctx, tx, account) {
		return ErrSyncWriterNotAdmitted
	}
	// Version an existing control tuple before enabling a previously inert
	// capability. A historical RR/SERIALIZABLE writer must not use an old
	// disabled or absent capability row to bypass the guard's locking read.
	if _, err = tx.Exec(ctx, `UPDATE sync_capture SET coverage_state=coverage_state WHERE account_id=$1`, account); err != nil {
		return ErrSyncWriterNotAdmitted
	}
	_, err = tx.Exec(ctx, `INSERT INTO sync_writer_capabilities(account_id,principal,capability,manifest_version,enabled,capture_epoch)
 VALUES($1,$2,$3,'lc-s6c1-v1',true,coalesce((SELECT capture_epoch FROM sync_capture WHERE account_id=$1),1))
 ON CONFLICT(account_id,principal,capability) DO UPDATE SET enabled=true,revoked_generation=0,manifest_version=EXCLUDED.manifest_version,capture_epoch=EXCLUDED.capture_epoch`, account, principal, capability)
	if err != nil {
		return ErrSyncWriterNotAdmitted
	}
	if tx.Commit(ctx) != nil {
		return ErrSyncWriterNotAdmitted
	}
	return nil
}

// A tuple lock cannot fence a historical snapshot that predates the account
// itself: neither the account nor its control row is visible to that writer.
// Under the exclusive account gate, refuse until every such snapshot ends.
// The horizon check is deliberately conservative; a horizon equal to xmin may
// have been pinned while the account's inserting transaction was uncommitted.
// PostgreSQL's autovacuum backends cannot later issue application DML or
// TRUNCATE; only those two known server-owned types are excluded as horizons.
// Unknown cross-principal activity is also refused. C2 still owns actual role
// topology and permissions proof; this check creates or changes no roles.
func syncCaptureAccountSnapshotSafe(ctx context.Context, tx pgx.Tx, account string) bool {
	if _, err := tx.Exec(ctx, `SELECT pg_stat_clear_snapshot()`); err != nil {
		return false
	}
	var safe bool
	err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM accounts WHERE id=$1)
 AND NOT EXISTS(SELECT 1 FROM pg_stat_activity activity JOIN accounts account ON account.id=$1
 WHERE activity.datname=current_database() AND activity.pid<>pg_backend_pid()
 AND ((activity.backend_xmin IS NOT NULL AND age(activity.backend_xmin)>=age(account.xmin)
 AND (activity.backend_type IS NULL OR activity.backend_type NOT IN ('autovacuum worker','autovacuum launcher')))
 OR (activity.usesysid IS DISTINCT FROM (SELECT oid FROM pg_roles WHERE rolname=current_user)
 AND NOT (current_setting('is_superuser')='on' OR pg_has_role(current_user,'pg_read_all_stats','MEMBER')))))`, account).Scan(&safe)
	return err == nil && safe
}

func syncCaptureCoverageValid(ctx context.Context, q syncVersionReader) bool {
	var valid bool
	err := q.QueryRow(ctx, `SELECT NOT EXISTS (SELECT 1 FROM unnest($1::text[]) AS inventory(name)
 WHERE (SELECT count(*) FROM pg_trigger t WHERE t.tgrelid=to_regclass(inventory.name)
 AND NOT t.tgisinternal AND t.tgenabled IN ('O','A') AND t.tgfoid='witself_sync_writer_guard()'::regprocedure
 AND ((t.tgname='sync_writer_guard' AND t.tgtype=31) OR (t.tgname='sync_writer_touch' AND t.tgtype=29) OR (t.tgname='sync_writer_truncate' AND t.tgtype=34)))<>3)
 AND EXISTS(SELECT 1 FROM pg_trigger WHERE tgrelid='sync_tx_admissions'::regclass AND tgname='sync_tx_coverage'
 AND tgenabled IN ('O','A') AND tgtype=21 AND tgdeferrable AND tginitdeferred AND tgfoid='witself_sync_check_coverage()'::regprocedure)`, SyncWriterInventory()).Scan(&valid)
	return err == nil && valid
}

// syncCaptureControlValid verifies the durable manifest and the currently
// enabled capability in the control row's exact epoch. It is also rechecked by
// the original session immediately before acknowledging a candidate snapshot.
func syncCaptureControlValid(ctx context.Context, q syncVersionReader, account string) bool {
	commitment := SyncWriterCoverageCommitment()
	var valid bool
	err := q.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM sync_capture c
 WHERE c.account_id=$1 AND c.coverage_state IN ('bootstrapping','suspended')
 AND c.protocol_version=1 AND c.min_writer_schema <= $2
 AND c.capability=$3 AND c.min_writer_capability=$3
 AND c.coverage_manifest_version=$4 AND c.coverage_manifest_commitment=$5
 AND EXISTS(SELECT 1 FROM sync_writer_capabilities w WHERE w.account_id=c.account_id
  AND w.capability=c.min_writer_capability AND w.enabled AND w.revoked_generation=0
  AND w.capture_epoch=c.capture_epoch AND w.manifest_version=c.coverage_manifest_version)
 AND EXISTS(SELECT 1 FROM sync_incarnations i WHERE i.account_id=c.account_id
  AND i.lifecycle_epoch=c.lifecycle_epoch AND i.restore_marker=c.restore_marker AND i.state='installed'))`,
		account, SyncSchemaVersion(), SyncPreferencesCapability, SyncCoverageManifestVersion, commitment[:]).Scan(&valid)
	return err == nil && valid
}

// ActivateSyncCapture drains admitted writers using an exclusive session gate.
// The returned owner must pin and acknowledge on this same physical session.
func (s *Store) ActivateSyncCapture(ctx context.Context, account string, options SyncCaptureOptions) (*SyncCaptureActivation, error) {
	if !s.syncCaptureSelected.Load() || account == "" || options.LifecycleEpoch < 0 || len(options.RestoreMarker) != 16 || options.Capability != SyncPreferencesCapability {
		return nil, ErrSyncWriterNotAdmitted
	}
	wait, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	conn, err := s.pool.Acquire(wait)
	if err != nil {
		return nil, ErrSyncWriterNotAdmitted
	}
	a := &SyncCaptureActivation{store: s, conn: conn, account: account, pid: conn.Conn().PgConn().PID()}
	good := false
	defer func() {
		if !good {
			// The later rollback defer finishes first; acknowledge gate release before Terminate can race server cleanup. // LC-S6C1 fix 3 (G1)
			cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), time.Second)      // LC-S6C1 fix 3 (G1)
			_, _ = conn.Exec(cleanupCtx, `SELECT pg_advisory_unlock(capture_lock_key($1))`, account) // LC-S6C1 fix 3 (G1)
			cleanupCancel()                                                                          // LC-S6C1 fix 3 (G1)
			a.Close()
		}
	}()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		var locked bool
		if conn.QueryRow(wait, `SELECT pg_try_advisory_lock(capture_lock_key($1))`, account).Scan(&locked) != nil {
			return nil, ErrSyncWriterNotAdmitted
		}
		if locked {
			break
		}
		select {
		case <-wait.Done():
			return nil, ErrSyncWriterNotAdmitted
		case <-ticker.C:
		}
	}
	tx, err := conn.Begin(ctx)
	if err != nil {
		return nil, ErrSyncWriterNotAdmitted
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if !syncCaptureCoverageValid(ctx, tx) {
		return nil, ErrSyncWriterNotAdmitted
	}
	if !syncCaptureAccountSnapshotSafe(ctx, tx, account) {
		return nil, ErrSyncWriterNotAdmitted
	}
	var eligible bool
	if tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM sync_writer_capabilities WHERE account_id=$1 AND capability=$2 AND enabled AND revoked_generation=0 AND manifest_version='lc-s6c1-v1') AND EXISTS(SELECT 1 FROM sync_incarnations WHERE account_id=$1 AND lifecycle_epoch=$3 AND restore_marker=$4 AND state='installed')`, account, options.Capability, options.LifecycleEpoch, options.RestoreMarker).Scan(&eligible) != nil || !eligible {
		return nil, ErrSyncWriterNotAdmitted
	}
	var previous int64
	err = tx.QueryRow(ctx, `SELECT capture_epoch FROM sync_capture WHERE account_id=$1 FOR UPDATE`, account).Scan(&previous)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrSyncWriterNotAdmitted
	}
	// Version the already-existing account tuple before the first control row.
	// A pre-activation RR/serializable snapshot that cannot see the new control
	// row must serialization-fail when its guard locks this account tuple.
	// The self-assignment preserves every domain value and is guarded normally.
	if errors.Is(err, pgx.ErrNoRows) {
		if _, err = tx.Exec(ctx, `UPDATE accounts SET display_name=display_name WHERE id=$1`, account); err != nil {
			return nil, ErrSyncWriterNotAdmitted
		}
	}
	a.epoch = previous + 1
	a.snapshot = make([]byte, 16)
	if _, err = rand.Read(a.snapshot); err != nil {
		return nil, ErrSyncWriterNotAdmitted
	}
	// A new epoch starts its own prefix; prior epochs and their bundles remain.
	if _, err = tx.Exec(ctx, `INSERT INTO sync_finalized_counter(account_id,capture_epoch,last_finalized) VALUES($1,$2,0)`, account, a.epoch); err != nil {
		return nil, ErrSyncWriterNotAdmitted
	}
	if tx.QueryRow(ctx, `SELECT last_finalized FROM sync_finalized_counter WHERE account_id=$1 AND capture_epoch=$2 FOR UPDATE`, account, a.epoch).Scan(&a.watermark) != nil {
		return nil, ErrSyncWriterNotAdmitted
	}
	commitment := SyncWriterCoverageCommitment()
	_, err = tx.Exec(ctx, `INSERT INTO sync_capture(account_id,capture_epoch,lifecycle_epoch,restore_marker,capability,coverage_state,protocol_version,min_writer_schema,min_writer_capability,coverage_manifest_version,coverage_manifest_commitment,snapshot_id,snapshot_watermark)
 VALUES($1,$2,$3,$4,$5,'bootstrapping',1,2,$5,'lc-s6c1-v1',$6,$7,$8)
 ON CONFLICT(account_id) DO UPDATE SET capture_epoch=EXCLUDED.capture_epoch,lifecycle_epoch=EXCLUDED.lifecycle_epoch,restore_marker=EXCLUDED.restore_marker,capability=EXCLUDED.capability,coverage_state='bootstrapping',protocol_version=1,min_writer_schema=2,min_writer_capability=EXCLUDED.min_writer_capability,coverage_manifest_version=EXCLUDED.coverage_manifest_version,coverage_manifest_commitment=EXCLUDED.coverage_manifest_commitment,snapshot_id=EXCLUDED.snapshot_id,snapshot_watermark=EXCLUDED.snapshot_watermark,suspension_reason=NULL`, account, a.epoch, options.LifecycleEpoch, options.RestoreMarker, options.Capability, commitment[:], a.snapshot, a.watermark)
	if err != nil {
		return nil, ErrSyncWriterNotAdmitted
	}
	if _, err = tx.Exec(ctx, `UPDATE sync_writer_capabilities SET capture_epoch=$2 WHERE account_id=$1 AND enabled AND revoked_generation=0`, account, a.epoch); err != nil {
		return nil, ErrSyncWriterNotAdmitted
	}
	if tx.Commit(ctx) != nil {
		return nil, ErrSyncWriterNotAdmitted
	}
	good = true
	return a, nil
}

// PinSnapshot pins S while A still owns the session gate. A dead owner cannot
// certify a candidate even when the durable epoch/id/watermark still match.
func (a *SyncCaptureActivation) PinSnapshot(ctx context.Context) (*SyncSnapshotPin, error) {
	if a.closed {
		return nil, ErrSyncWriterNotAdmitted
	}
	tx, err := a.store.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, ErrSyncWriterNotAdmitted
	}
	p := &SyncSnapshotPin{activation: a, tx: tx}
	err = tx.QueryRow(ctx, `SELECT capture_epoch,snapshot_id,snapshot_watermark FROM sync_capture WHERE account_id=$1 AND coverage_state='bootstrapping'`, a.account).Scan(&p.epoch, &p.snapshot, &p.watermark)
	if err != nil || p.epoch != a.epoch || p.watermark != a.watermark || !bytes.Equal(p.snapshot, a.snapshot) {
		_ = tx.Rollback(ctx)
		return nil, ErrSyncWriterNotAdmitted
	}
	return p, nil
}

// Acknowledge verifies the gate on the original physical connection, then
// releases it. A reconnect is never a substitute for this ownership proof.
func (a *SyncCaptureActivation) Acknowledge(ctx context.Context, p *SyncSnapshotPin) error {
	return a.acknowledge(ctx, p, nil)
}

// beforeAcknowledgment is an unexported failure-cut seam, never a production
// option. The final database roundtrip must execute on A after every check on
// S, giving acknowledgment a physical-session linearization point.
func (a *SyncCaptureActivation) acknowledge(ctx context.Context, p *SyncSnapshotPin, beforeAcknowledgment func()) error {
	if a.closed || p == nil || p.activation != a || p.acknowledged || a.conn.Conn().PgConn().PID() != a.pid {
		return ErrSyncWriterNotAdmitted
	}
	// Ensure S is still live before A makes its final acknowledgment. Checking
	// S afterwards would permit A to die during that roundtrip and still have
	// the candidate marked acknowledged by this client.
	var one int
	if p.tx.QueryRow(ctx, `SELECT 1`).Scan(&one) != nil {
		a.Close()
		return ErrSyncWriterNotAdmitted
	}
	if !syncCaptureCoverageValid(ctx, a.conn) || !syncCaptureControlValid(ctx, a.conn, a.account) {
		_ = a.store.SuspendSyncCapture(ctx, a.account)
		a.Close()
		return ErrSyncWriterNotAdmitted
	}
	if beforeAcknowledgment != nil {
		beforeAcknowledgment()
	}
	var held bool
	err := a.conn.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_locks WHERE pid=pg_backend_pid() AND locktype='advisory' AND mode='ExclusiveLock' AND granted AND objsubid=1 AND classid=((capture_lock_key($1)>>32)&4294967295)::oid AND objid=(capture_lock_key($1)&4294967295)::oid)
 AND EXISTS(SELECT 1 FROM sync_capture WHERE account_id=$1 AND capture_epoch=$2 AND snapshot_id=$3 AND snapshot_watermark=$4 AND coverage_state='bootstrapping')`, a.account, p.epoch, p.snapshot, p.watermark).Scan(&held)
	if err != nil || !held {
		a.Close()
		return ErrSyncWriterNotAdmitted
	}
	p.acknowledged = true
	a.Close()
	return nil
}

// Close destroys the physical session, ensuring session locks cannot leak into
// the pool, including after cancellation or an ambiguous network failure.
func (a *SyncCaptureActivation) Close() {
	if a == nil || a.closed {
		return
	}
	a.closed = true
	conn := a.conn.Hijack()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_ = conn.Close(ctx)
}

// SuspendSyncCapture retains all capture evidence and exposes a fixed reason.
func (s *Store) SuspendSyncCapture(ctx context.Context, account string) error {
	if !s.syncCaptureSelected.Load() {
		return ErrSyncWriterNotAdmitted
	}
	_, err := s.pool.Exec(ctx, `UPDATE sync_capture SET coverage_state='suspended',suspension_reason='coverage_lost' WHERE account_id=$1`, account)
	if err != nil {
		return ErrSyncWriterNotAdmitted
	}
	return nil
}

// SyncCaptureStatus is separate from core readiness and schema availability.
func (s *Store) SyncCaptureStatus(ctx context.Context, account string) (runtimeconfig.SyncStatus, error) {
	if !s.syncCaptureSelected.Load() {
		return runtimeconfig.SyncDisabled, nil
	}
	var state string
	err := s.pool.QueryRow(ctx, `SELECT coverage_state FROM sync_capture WHERE account_id=$1`, account).Scan(&state)
	if errors.Is(err, pgx.ErrNoRows) {
		return runtimeconfig.SyncDisabled, nil
	}
	if err != nil {
		return runtimeconfig.SyncSuspended, runtimeconfig.ErrSyncStatusUnavailable
	}
	if !syncCaptureControlValid(ctx, s.pool, account) || !syncCaptureCoverageValid(ctx, s.pool) {
		if s.SuspendSyncCapture(ctx, account) != nil {
			return runtimeconfig.SyncSuspended, runtimeconfig.ErrSyncStatusUnavailable
		}
		return runtimeconfig.SyncSuspended, nil
	}
	switch state {
	case "bootstrapping":
		return runtimeconfig.SyncBootstrapping, nil
	case "suspended":
		return runtimeconfig.SyncSuspended, nil
	default:
		return runtimeconfig.SyncSuspended, runtimeconfig.ErrSyncStatusUnavailable
	}
}
