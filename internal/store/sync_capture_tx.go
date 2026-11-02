package store

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"strconv"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/witwave-ai/witself/internal/runtimeconfig"
)

const (
	// SyncPreferencesPrincipal is the first explicitly registered local writer.
	SyncPreferencesPrincipal = "preferences"
	// SyncPreferencesCapability identifies the first reviewed semantic writer.
	SyncPreferencesCapability = "preferences.put"
)

// ErrSyncWriterNotAdmitted is deliberately value-free. PostgreSQL guards use
// the same message with SQLSTATE P0001; callers must never retry a refusal.
var ErrSyncWriterNotAdmitted = errors.New("sync writer not admitted")

func syncCoverageMatches(version string, commitment []byte) bool {
	want := SyncWriterCoverageCommitment()
	return version == SyncCoverageManifestVersion && bytes.Equal(commitment, want[:])
}

func syncCaptureError(err error) error {
	var pgerr *pgconn.PgError
	if errors.As(err, &pgerr) && pgerr.Code == "P0001" && pgerr.Message == ErrSyncWriterNotAdmitted.Error() {
		return ErrSyncWriterNotAdmitted
	}
	return err
}

// SelectSyncCapture selects only the dark store API. No shipped command calls
// this method, and schema installation alone never enables a writer.
func (s *Store) SelectSyncCapture(config runtimeconfig.Config) error {
	if !config.SyncCaptureSelected() {
		return ErrSyncWriterNotAdmitted
	}
	s.syncCaptureSelected.Store(true)
	return nil
}

// SyncTx owns one admitted physical transaction. Every durable part of its
// state, including touch identities, lives in PostgreSQL so rollback to a
// savepoint also rolls back semantic fragments. It is not safe for concurrent
// use, just like pgx.Tx.
type SyncTx struct {
	tx         pgx.Tx
	account    string
	id         int64
	epoch      int64
	lifecycle  int64
	restore    []byte
	principal  string
	capability string
	sealed     bool
}

// Tx exposes the admitted transaction for the reviewed domain writer. Domain
// writes through another pool connection are intentionally refused by guards.
func (tx *SyncTx) Tx() pgx.Tx { return tx.tx }

// AdmitSyncTx acquires the shared account gate before any protected access.
func (s *Store) AdmitSyncTx(ctx context.Context, account, principal, capability string) (*SyncTx, error) {
	if !s.syncCaptureSelected.Load() || account == "" || principal == "" || capability == "" {
		return nil, ErrSyncWriterNotAdmitted
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	admitted, err := admitSyncTx(ctx, tx, account, principal, capability)
	if err != nil {
		_ = tx.Rollback(ctx)
		return nil, err
	}
	return admitted, nil
}

// admitSyncTx also supports an existing transaction in the stale-snapshot
// proof. FOR SHARE is essential: a historical MVCC view must never authorize
// a writer against a newer capture epoch.
func admitSyncTx(ctx context.Context, tx pgx.Tx, account, principal, capability string) (*SyncTx, error) {
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock_shared(capture_lock_key($1))`, account); err != nil {
		return nil, syncCaptureError(err)
	}
	out := &SyncTx{tx: tx, account: account, principal: principal, capability: capability}
	var state, requiredCapability, manifest string
	var protocol, minimum int
	var commitment []byte
	err := tx.QueryRow(ctx, `SELECT capture_epoch,lifecycle_epoch,restore_marker,coverage_state,
		protocol_version,min_writer_schema,min_writer_capability,coverage_manifest_version,coverage_manifest_commitment
		FROM sync_capture WHERE account_id=$1 FOR SHARE`, account).Scan(
		&out.epoch, &out.lifecycle, &out.restore, &state, &protocol, &minimum, &requiredCapability, &manifest, &commitment)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrSyncWriterNotAdmitted
	}
	if err != nil {
		return nil, syncCaptureError(err)
	}
	if (state != "bootstrapping" && state != "suspended") || protocol != 1 || minimum > SyncSchemaVersion() ||
		requiredCapability != capability || !syncCoverageMatches(manifest, commitment) {
		return nil, ErrSyncWriterNotAdmitted
	}
	var enabled bool
	var generation, epoch int64
	var capManifest string
	err = tx.QueryRow(ctx, `SELECT enabled,revoked_generation,capture_epoch,manifest_version
		FROM sync_writer_capabilities WHERE account_id=$1 AND principal=$2 AND capability=$3 FOR SHARE`,
		account, principal, capability).Scan(&enabled, &generation, &epoch, &capManifest)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrSyncWriterNotAdmitted
	}
	if err != nil {
		return nil, syncCaptureError(err)
	}
	if !enabled || generation != 0 || epoch != out.epoch || capManifest != manifest {
		return nil, ErrSyncWriterNotAdmitted
	}
	if err := tx.QueryRow(ctx, `SELECT txid_current()`).Scan(&out.id); err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO sync_tx_admissions
		(account_id,tx_id,capability,lifecycle_epoch,restore_marker,capture_epoch)
		VALUES($1,$2,$3,$4,$5,$6)`, account, out.id, capability, out.lifecycle, out.restore, out.epoch); err != nil {
		return nil, syncCaptureError(err)
	}
	if _, err := tx.Exec(ctx, `SELECT set_config('witself.sync_tx',$1,true)`, strconv.FormatInt(out.id, 10)); err != nil {
		return nil, err
	}
	return out, nil
}

func newSyncIdentity() ([]byte, error) {
	id := make([]byte, 16)
	if _, err := rand.Read(id); err != nil {
		return nil, errors.New("sync identity generation failed")
	}
	return id, nil
}

// Touch binds one actual database mutation to one reviewed semantic operation.
// Repeated updates of the same row receive distinct event IDs; claiming a table
// once cannot conceal an extra write. Future writers must extend this mapping
// in a separately reviewed slice.
func (tx *SyncTx) Touch(ctx context.Context, operation, atomicGroup string, semanticEffect, dependencies json.RawMessage) error {
	if tx.sealed || operation != SyncPreferencesCapability || atomicGroup == "" ||
		!json.Valid(semanticEffect) || !json.Valid(dependencies) {
		return ErrSyncWriterNotAdmitted
	}
	var eventID int64
	const tableName = "agent_dashboard_preferences"
	err := tx.tx.QueryRow(ctx, `SELECT (event->>'event_id')::bigint
		FROM sync_tx_admissions a CROSS JOIN LATERAL jsonb_array_elements(a.actual_touches) event
		WHERE a.account_id=$1 AND a.tx_id=$2 AND NOT a.sealed AND event->>'table_name'=$3
		AND NOT EXISTS (SELECT 1 FROM sync_tx_touches t WHERE t.account_id=a.account_id AND t.tx_id=a.tx_id
		AND t.touch_id=(event->>'event_id')::bigint) ORDER BY (event->>'event_id')::bigint LIMIT 1`,
		tx.account, tx.id, tableName).Scan(&eventID)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrSyncWriterNotAdmitted
	}
	if err != nil {
		return syncCaptureError(err)
	}
	operationID, err := newSyncIdentity()
	if err != nil {
		return err
	}
	envelope, err := json.Marshal(struct {
		Operation    string          `json:"operation"`
		Effect       json.RawMessage `json:"effect"`
		Dependencies json.RawMessage `json:"dependencies"`
	}{operation, semanticEffect, dependencies})
	if err != nil || len(envelope) > 65536 {
		return ErrSyncWriterNotAdmitted
	}
	commitment := sha256.Sum256(envelope)
	_, err = tx.tx.Exec(ctx, `INSERT INTO sync_operations(account_id,operation_id,original_lifecycle_epoch,
		original_restore_marker,original_capture_epoch,original_installation,original_replica,
		original_write_lineage,original_sequence,operation_kind,envelope_version,schema_version,
		protocol_version,comparison_key_id,input_commitment,admission_state)
		VALUES($1,$2,$3,$4,$5,'local','local','local-capture',$6,$7,1,1,1,'local-capture-v1',$8,'admitted')`,
		tx.account, operationID, tx.lifecycle, tx.restore, tx.epoch, eventID, operation, commitment[:])
	if err != nil {
		return syncCaptureError(err)
	}
	if _, err := tx.tx.Exec(ctx, `INSERT INTO sync_operation_payloads(account_id,operation_id,canonical_semantic_envelope)
		VALUES($1,$2,$3)`, tx.account, operationID, envelope); err != nil {
		return syncCaptureError(err)
	}
	_, err = tx.tx.Exec(ctx, `INSERT INTO sync_tx_touches(account_id,tx_id,touch_id,semantic_effect,dependencies,
		operation_id,atomic_group,table_name) VALUES($1,$2,$3,$4::jsonb,$5::jsonb,$6,$7,$8)`,
		tx.account, tx.id, eventID, semanticEffect, dependencies, operationID, atomicGroup, tableName)
	return syncCaptureError(err)
}

// Seal creates exactly one bundle and holds the counter row lock until commit
// or rollback. There is no PostgreSQL sequence in the committed-prefix path.
func (tx *SyncTx) Seal(ctx context.Context) error {
	if tx.sealed {
		return ErrSyncWriterNotAdmitted
	}
	var prior int64
	if err := tx.tx.QueryRow(ctx, `SELECT last_finalized FROM sync_finalized_counter
		WHERE account_id=$1 AND capture_epoch=$2 FOR UPDATE`, tx.account, tx.epoch).Scan(&prior); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrSyncWriterNotAdmitted
		}
		return syncCaptureError(err)
	}
	bundleID, err := newSyncIdentity()
	if err != nil {
		return err
	}
	_, err = tx.tx.Exec(ctx, `INSERT INTO sync_bundles(account_id,bundle_id,tx_id,capture_epoch,lifecycle_epoch,
		restore_marker,finalized,sealed,origin_class,semantic_effects,dependencies)
		SELECT $1,$2,$3,$4,$5,$6,$7,true,'original',
		coalesce(jsonb_agg(semantic_effect ORDER BY touch_id),'[]'::jsonb),
		coalesce(jsonb_agg(dependencies ORDER BY touch_id),'[]'::jsonb)
		FROM sync_tx_touches WHERE account_id=$1 AND tx_id=$3`,
		tx.account, bundleID, tx.id, tx.epoch, tx.lifecycle, tx.restore, prior+1)
	if err != nil {
		return syncCaptureError(err)
	}
	_, err = tx.tx.Exec(ctx, `INSERT INTO sync_bundle_operations(account_id,bundle_id,member_order,operation_id,atomic_group)
		SELECT account_id,$3,touch_id,operation_id,atomic_group
		FROM sync_tx_touches WHERE account_id=$1 AND tx_id=$2`, tx.account, tx.id, bundleID)
	if err != nil {
		return syncCaptureError(err)
	}
	_, err = tx.tx.Exec(ctx, `UPDATE sync_tx_admissions SET sealed=true,prior_finalized=$3
		WHERE account_id=$1 AND tx_id=$2 AND NOT sealed`, tx.account, tx.id, prior)
	if err != nil {
		return syncCaptureError(err)
	}
	_, err = tx.tx.Exec(ctx, `UPDATE sync_finalized_counter SET last_finalized=$3
		WHERE account_id=$1 AND capture_epoch=$2`, tx.account, tx.epoch, prior+1)
	if err == nil {
		tx.sealed = true
	}
	return syncCaptureError(err)
}

// Savepoint restores the Go handle and all SQL touch state together. Sealing
// inside the callback is supported: a rollback restores the pre-seal handle.
func (tx *SyncTx) Savepoint(ctx context.Context, fn func(*SyncTx) error) error {
	if tx.sealed || fn == nil {
		return ErrSyncWriterNotAdmitted
	}
	nested, err := tx.tx.Begin(ctx)
	if err != nil {
		return err
	}
	child := *tx
	child.tx = nested
	if err := fn(&child); err != nil {
		_ = nested.Rollback(ctx)
		return syncCaptureError(err)
	}
	if err := nested.Commit(ctx); err != nil {
		return syncCaptureError(err)
	}
	tx.sealed = child.sealed
	return nil
}

// Commit lets the deferred coverage trigger make the final decision. A missing
// seal is refused by PostgreSQL and the whole domain transaction is rolled back.
func (tx *SyncTx) Commit(ctx context.Context) error { return syncCaptureError(tx.tx.Commit(ctx)) }

// Rollback discards domain changes and all capture state atomically.
func (tx *SyncTx) Rollback(ctx context.Context) error { return tx.tx.Rollback(ctx) }
