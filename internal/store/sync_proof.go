package store

import (
	"context"
	"crypto/subtle"
	_ "embed"
	"errors"
	"strings"
	"time"
)

// The proof-only marker is deliberately outside sync_migrations: ordinary
// MigrateSync never installs it and SyncSchemaVersion remains unchanged. It is
// excluded from account archives and the covered writer inventory.
//
//go:embed sync_proof/0004_capture_proof_marker.sql
var syncCaptureProofMarkerSQL string

// ErrSyncCaptureProofDatabaseUnidentified is value-free even for malformed or
// expired markers. Neither the token nor the database identity is diagnostic.
var ErrSyncCaptureProofDatabaseUnidentified = errors.New("capture_proof_database_unidentified")

// InstallSyncCaptureProofMarker explicitly marks an already provisioned proof
// database. The migration owner calls this once before starting proof processes.
// It cannot create or mark a shared rehearsal database.
func (s *Store) InstallSyncCaptureProofMarker(ctx context.Context, token string, expiresAt time.Time) error {
	if token == "" || !expiresAt.After(time.Now()) {
		return ErrSyncCaptureProofDatabaseUnidentified
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return ErrSyncCaptureProofDatabaseUnidentified
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var name string
	var future bool
	if tx.QueryRow(ctx, `SELECT current_database(), $1::timestamptz > clock_timestamp()`, expiresAt).Scan(&name, &future) != nil ||
		!strings.HasPrefix(name, "witself_captureproof_") || !future {
		return ErrSyncCaptureProofDatabaseUnidentified
	}
	if _, err := tx.Exec(ctx, syncCaptureProofMarkerSQL); err != nil {
		return ErrSyncCaptureProofDatabaseUnidentified
	}
	if _, err := tx.Exec(ctx, `INSERT INTO sync_capture_proof_marker(token,expires_at) VALUES($1,$2)`, token, expiresAt); err != nil {
		return ErrSyncCaptureProofDatabaseUnidentified
	}
	if err := tx.Commit(ctx); err != nil {
		return ErrSyncCaptureProofDatabaseUnidentified
	}
	return nil
}

// VerifySyncCaptureProofDatabase requires both the disposable database name and
// exactly one live marker matching this process's private per-run token.
func (s *Store) VerifySyncCaptureProofDatabase(ctx context.Context, token string) error {
	return verifySyncCaptureProofDatabase(ctx, s.pool, token)
}

func verifySyncCaptureProofDatabase(ctx context.Context, q syncVersionReader, token string) error {
	if token == "" || q == nil {
		return ErrSyncCaptureProofDatabaseUnidentified
	}
	var name string
	var exists bool
	if q.QueryRow(ctx, `SELECT current_database(),
		to_regclass(format('%I.sync_capture_proof_marker',current_schema())) IS NOT NULL`).Scan(&name, &exists) != nil ||
		!strings.HasPrefix(name, "witself_captureproof_") || !exists {
		return ErrSyncCaptureProofDatabaseUnidentified
	}
	var count int64
	var markerToken string
	var future bool
	if q.QueryRow(ctx, `SELECT count(*), coalesce(min(token),''), coalesce(min(expires_at) > clock_timestamp(),false)
		FROM sync_capture_proof_marker`).Scan(&count, &markerToken, &future) != nil || count != 1 || !future ||
		subtle.ConstantTimeCompare([]byte(markerToken), []byte(token)) != 1 {
		return ErrSyncCaptureProofDatabaseUnidentified
	}
	return nil
}
