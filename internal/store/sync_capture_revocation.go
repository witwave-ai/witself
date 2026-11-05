package store

import (
	"context"
	"time"
)

// RevokeSyncCapability drains the account's admitted writers before advancing
// the principal's revocation generation and capability epoch. This management
// call requires control-table privileges; runtime pools do not receive them.
func (s *Store) RevokeSyncCapability(ctx context.Context, account, principal string) error {
	if !s.syncCaptureSelected.Load() || account == "" || principal == "" {
		return ErrSyncWriterNotAdmitted
	}
	wait, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	conn, err := s.pool.Acquire(wait)
	if err != nil {
		return ErrSyncWriterNotAdmitted
	}
	defer func() {
		physical := conn.Hijack()
		cleanup, done := context.WithTimeout(context.Background(), time.Second)
		defer done()
		_ = physical.Close(cleanup)
	}()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		var locked bool
		if conn.QueryRow(wait, `SELECT pg_try_advisory_lock(capture_lock_key($1))`, account).Scan(&locked) != nil {
			return ErrSyncWriterNotAdmitted
		}
		if locked {
			break
		}
		select {
		case <-wait.Done():
			return ErrSyncWriterNotAdmitted
		case <-ticker.C:
		}
	}
	tx, err := conn.Begin(wait)
	if err != nil {
		return ErrSyncWriterNotAdmitted
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	result, err := tx.Exec(wait, `UPDATE sync_writer_capabilities
 SET enabled=false,revoked_generation=revoked_generation+1,capture_epoch=capture_epoch+1
 WHERE account_id=$1 AND principal=$2`, account, principal)
	if err != nil || result.RowsAffected() == 0 {
		return ErrSyncWriterNotAdmitted
	}
	if _, err = tx.Exec(wait, `UPDATE sync_capture SET coverage_state='suspended',suspension_reason='capability_revoked'
 WHERE account_id=$1`, account); err != nil {
		return ErrSyncWriterNotAdmitted
	}
	if tx.Commit(wait) != nil {
		return ErrSyncWriterNotAdmitted
	}
	return nil
}
