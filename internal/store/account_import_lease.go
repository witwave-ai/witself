package store

import (
	"context"
	"errors"
	"io"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/witwave-ai/witself/internal/export"
)

// AccountImportAdvisoryKeySQL scopes import ownership to one database and epoch.
const AccountImportAdvisoryKeySQL = `hashtextextended(current_database() || ':witself:account-import:v1:' || $1 || ':' || $2, 0)`
const accountImportAdvisoryTryLockSQL = `SELECT pg_try_advisory_lock(` + AccountImportAdvisoryKeySQL + `)`
const accountImportInProgressSQL = `WITH k AS (SELECT ` + AccountImportAdvisoryKeySQL + ` AS key)
 SELECT EXISTS (SELECT 1 FROM pg_locks l, k WHERE l.locktype='advisory' AND l.granted
 AND l.database=(SELECT oid FROM pg_database WHERE datname=current_database())
 AND l.classid=((k.key >> 32) & 4294967295)::oid
 AND l.objid=(k.key & 4294967295)::oid AND l.objsubid=1)`

var (
	// ErrAccountImportLeaseHeld means another session owns this exact import.
	ErrAccountImportLeaseHeld = errors.New("account import lease held")
	// ErrAccountImportLeaseUnavailable means pool acquisition exceeded its budget.
	ErrAccountImportLeaseUnavailable = errors.New("account import lease unavailable")
	// ErrAccountImportBusy refuses a synchronous import racing another owner.
	ErrAccountImportBusy = errors.New("account import busy")
)

type queryer interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}
type txBeginner interface {
	Begin(context.Context) (pgx.Tx, error)
}

// AccountImportReceipt reads durable evidence without claiming import ownership.
func AccountImportReceipt(ctx context.Context, q queryer, accountID, evacuationID string) (AccountImportDisposition, bool, error) {
	var d AccountImportDisposition
	var currentID, role, lastID, outcome *string
	err := q.QueryRow(ctx, `SELECT status, evacuation_id, evacuation_role, last_evacuation_id, last_evacuation_outcome FROM accounts WHERE id=$1`, accountID).Scan(&d.CurrentStatus, &currentID, &role, &lastID, &outcome)
	if errors.Is(err, pgx.ErrNoRows) {
		return d, false, nil
	}
	if err != nil {
		return d, false, err
	}
	if currentID != nil && *currentID == evacuationID {
		if role == nil {
			return d, false, ErrAccountEvacuationMismatch
		}
		switch *role {
		case "source":
			return d, false, nil
		case "target":
			d.AlreadyImported = true
			d.EvacuationRole = "target"
			return d, true, nil
		default:
			return d, false, ErrAccountEvacuationMismatch
		}
	}
	if currentID == nil && lastID != nil && *lastID == evacuationID && outcome != nil && *outcome == "completed" {
		d.AlreadyImported = true
		d.EvacuationCompleted = true
		d.EvacuationRole = "target"
		return d, true, nil
	}
	if currentID != nil && *currentID != evacuationID {
		return d, false, ErrAccountEvacuationInProgress
	}
	return d, false, ErrAccountExists
}

// AccountImportLease pins a session until Close, including across download time.
type AccountImportLease struct {
	store                   *Store
	conn                    *pgxpool.Conn
	accountID, evacuationID string
	receipt                 AccountImportDisposition
	present                 bool
	once                    sync.Once
}

// AcquireAccountImportLease takes the session lock before reading its receipt.
func (s *Store) AcquireAccountImportLease(ctx context.Context, accountID, evacuationID string) (*AccountImportLease, error) {
	if err := validateEvacuationID(evacuationID); err != nil {
		return nil, err
	}
	acquireCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	conn, err := s.pool.Acquire(acquireCtx)
	if errors.Is(err, context.DeadlineExceeded) {
		return nil, ErrAccountImportLeaseUnavailable
	}
	if err != nil {
		return nil, err
	}
	l := &AccountImportLease{store: s, conn: conn, accountID: accountID, evacuationID: evacuationID}
	var locked bool
	if err := conn.QueryRow(ctx, accountImportAdvisoryTryLockSQL, accountID, evacuationID).Scan(&locked); err != nil {
		l.Close()
		return nil, err
	}
	if !locked {
		conn.Release()
		return nil, ErrAccountImportLeaseHeld
	}
	if _, err := conn.Exec(ctx, `SET tcp_keepalives_idle = 30; SET tcp_keepalives_interval = 10; SET tcp_keepalives_count = 6`); err != nil {
		l.Close()
		return nil, err
	}
	l.receipt, l.present, err = AccountImportReceipt(ctx, conn, accountID, evacuationID)
	if err != nil {
		l.Close()
		return nil, err
	}
	return l, nil
}

// Receipt returns the evidence read after acquiring ownership.
func (l *AccountImportLease) Receipt() (AccountImportDisposition, bool) { return l.receipt, l.present }

// Import uses the pinned session; its transaction lock is reentrant.
func (l *AccountImportLease) Import(ctx context.Context, r io.Reader) (export.Manifest, AccountImportDisposition, error) {
	return l.store.importAccount(ctx, l.conn, l.accountID, accountImportOptions{evacuationID: l.evacuationID}, r)
}

// Close destroys the session, releasing its locks even after cancellation.
func (l *AccountImportLease) Close() {
	l.once.Do(func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(context.Background()), 5*time.Second)
		defer cancel()
		_ = l.conn.Conn().Close(ctx)
		l.conn.Release()
	})
}

// AccountImportInProgress observes session and transaction holders without locking.
func (s *Store) AccountImportInProgress(ctx context.Context, accountID, evacuationID string) (bool, error) {
	var running bool
	err := s.pool.QueryRow(ctx, accountImportInProgressSQL, accountID, evacuationID).Scan(&running)
	return running, err
}

// AccountImportStatus reads receipt then lock on one pooled connection.
func (s *Store) AccountImportStatus(ctx context.Context, accountID, evacuationID string) (AccountImportDisposition, bool, bool, error) {
	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		return AccountImportDisposition{}, false, false, err
	}
	defer conn.Release()
	d, present, err := AccountImportReceipt(ctx, conn, accountID, evacuationID)
	if err != nil || present {
		return d, present, false, err
	}
	var running bool
	err = conn.QueryRow(ctx, accountImportInProgressSQL, accountID, evacuationID).Scan(&running)
	return d, false, running, err
}
