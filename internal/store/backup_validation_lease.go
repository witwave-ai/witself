package store

import (
	"context"
	"errors"
	"io"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/witwave-ai/witself/internal/export"
)

// BackupValidationAdvisoryKeySQL scopes validation ownership to one account and database.
const BackupValidationAdvisoryKeySQL = `hashtextextended(current_database() || ':witself:backup-validation:v1:' || $1, 0)`
const backupValidationAdvisoryTryLockSQL = `SELECT pg_try_advisory_lock(` + BackupValidationAdvisoryKeySQL + `)`
const backupValidationInProgressSQL = `WITH k AS (SELECT ` + BackupValidationAdvisoryKeySQL + ` AS key)
 SELECT EXISTS (SELECT 1 FROM pg_locks l, k WHERE l.locktype='advisory' AND l.granted
 AND l.database=(SELECT oid FROM pg_database WHERE datname=current_database())
 AND l.classid=((k.key >> 32) & 4294967295)::oid
 AND l.objid=(k.key & 4294967295)::oid AND l.objsubid=1)`

var (
	// ErrBackupValidationLeaseHeld means another session owns this account's validation.
	ErrBackupValidationLeaseHeld = errors.New("backup validation lease held")
	// ErrBackupValidationLeaseUnavailable means acquiring ownership failed transiently.
	ErrBackupValidationLeaseUnavailable = errors.New("backup validation lease unavailable")
)

// BackupValidationLease pins a session through validation and result retention.
type BackupValidationLease struct {
	store               *Store
	conn                *pgxpool.Conn
	accountID, backupID string
	mu                  sync.Mutex
	closed              bool
	once                sync.Once
}

// AcquireBackupValidationLease takes the account's session lock without importing.
func (s *Store) AcquireBackupValidationLease(ctx context.Context, accountID, backupID string) (*BackupValidationLease, error) {
	if err := validateBackupID(backupID); err != nil {
		return nil, err
	}
	acquireCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	conn, err := s.pool.Acquire(acquireCtx)
	if err != nil {
		return nil, backupValidationAcquireError(err)
	}
	l := &BackupValidationLease{store: s, conn: conn, accountID: accountID, backupID: backupID}
	var locked bool
	if err := conn.QueryRow(ctx, backupValidationAdvisoryTryLockSQL, accountID).Scan(&locked); err != nil {
		l.Close()
		return nil, backupValidationAcquireError(err)
	}
	if !locked {
		conn.Release()
		return nil, ErrBackupValidationLeaseHeld
	}
	if _, err := conn.Exec(ctx, `SET tcp_keepalives_idle = 30; SET tcp_keepalives_interval = 10; SET tcp_keepalives_count = 6`); err != nil {
		l.Close()
		return nil, err
	}
	return l, nil
}

// Only failures before confirmed ownership are eligible for a transient decline.
func backupValidationAcquireError(err error) error {
	var connectErr *pgconn.ConnectError
	if errors.Is(err, context.DeadlineExceeded) || errors.As(err, &connectErr) || pgconn.SafeToRetry(err) {
		return ErrBackupValidationLeaseUnavailable
	}
	return err
}

// Validate exercises the complete import on the pinned session and rolls it back.
func (l *BackupValidationLease) Validate(ctx context.Context, r io.Reader) (export.Manifest, error) {
	m, _, err := l.store.importAccount(ctx, l.conn, l.accountID, accountImportOptions{backupValidation: true, backupID: l.backupID}, r)
	return m, err
}

// Held reports the client's view of the session, including safely after Close.
func (l *BackupValidationLease) Held() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return false
	}
	return !l.conn.Conn().IsClosed()
}

// Close destroys the session and is safe to call more than once.
func (l *BackupValidationLease) Close() {
	l.once.Do(func() {
		l.mu.Lock()
		defer l.mu.Unlock()
		l.closed = true
		ctx, cancel := context.WithTimeout(context.WithoutCancel(context.Background()), 5*time.Second)
		defer cancel()
		_ = l.conn.Conn().Close(ctx)
		l.conn.Release()
	})
}

// BackupValidationInProgress observes the account lock without acquiring it.
func (s *Store) BackupValidationInProgress(ctx context.Context, accountID string) (bool, error) {
	var running bool
	err := s.pool.QueryRow(ctx, backupValidationInProgressSQL, accountID).Scan(&running)
	return running, err
}
