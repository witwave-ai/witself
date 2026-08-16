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
	agentEmailCellStorageCapacitySQLState = "WE001"
	maximumAgentEmailCellStorageLimit     = int64(4611686018427387903)
	minimumAgentEmailCellStorageRowBytes  = int64(8192)
	agentEmailCellStorageFinalizeTimeout  = 5 * time.Second
)

// ErrAgentEmailDatabaseCapacity identifies the transactionally maintained
// cell-level retained-email safety boundary. It is independent from every
// commercial plan limit and from physical database/PV monitoring.
var ErrAgentEmailDatabaseCapacity = errors.New("agent-email cell storage capacity reached")

// IsAgentEmailDatabaseCapacityError recognizes both the process-side sentinel
// and schema-91's database-enforced logical refusal.
// Callers must use this helper rather than matching database error text.
func IsAgentEmailDatabaseCapacityError(err error) bool {
	if errors.Is(err, ErrAgentEmailDatabaseCapacity) {
		return true
	}
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) &&
		pgErr.Code == agentEmailCellStorageCapacitySQLState
}

// preflightAgentEmailCellStorageRootTx performs a lock-free lower-bound check
// before a caller spends CPU parsing MIME or constructing an outbound row. It
// is intentionally advisory: the schema-91 triggers remain authoritative and
// serialize the actual charged bytes and rows. Every charged row costs at
// least 8 KiB, so a refusal here cannot reject a root that could have fit.
func preflightAgentEmailCellStorageRootTx(
	ctx context.Context,
	tx interface {
		QueryRow(context.Context, string, ...any) pgx.Row
	},
	requiredCountedRows int64,
) error {
	if requiredCountedRows < 1 ||
		requiredCountedRows > maximumAgentEmailCellStorageLimit/minimumAgentEmailCellStorageRowBytes {
		return errors.New("agent-email cell storage preflight row count is invalid")
	}
	minimumHardBytes := requiredCountedRows * minimumAgentEmailCellStorageRowBytes
	var admitted bool
	err := tx.QueryRow(ctx, `
		SELECT admission_bytes >= $1
		   AND retained_bytes <= admission_bytes - $1
		   AND root_rows < admission_root_rows
		   AND hard_bytes >= $2
		   AND retained_bytes <= hard_bytes - $2
		   AND hard_counted_rows >= $3
		   AND counted_rows <= hard_counted_rows - $3
		  FROM agent_email_cell_storage_capacity
		 WHERE singleton=1`,
		minimumAgentEmailCellStorageRowBytes,
		minimumHardBytes,
		requiredCountedRows,
	).Scan(&admitted)
	if errors.Is(err, pgx.ErrNoRows) {
		return errors.New("agent-email cell storage capacity state is missing")
	}
	if err != nil {
		return fmt.Errorf("preflight agent-email cell storage capacity: %w", err)
	}
	if !admitted {
		return ErrAgentEmailDatabaseCapacity
	}
	return nil
}

// commitAgentEmailCellStorageRefusal preserves rate debt already applied by
// the outer transaction. A full cell is a permanent storage refusal, but it
// must not become a free retry path around the authoritative GCRA breakers.
// Finalization is detached from a caller disconnect but remains time-bounded.
func commitAgentEmailCellStorageRefusal(
	ctx context.Context,
	tx pgx.Tx,
	cause error,
) error {
	finalizeCtx, cancel := context.WithTimeout(
		context.WithoutCancel(ctx), agentEmailCellStorageFinalizeTimeout,
	)
	defer cancel()
	if err := tx.Commit(finalizeCtx); err != nil {
		return fmt.Errorf(
			"commit agent-email rate debt after cell storage refusal: %w", err,
		)
	}
	return cause
}

// rollbackAgentEmailCellStorageWriteAndCommitRefusal recovers the outer
// transaction from a trigger-time WE001 raised inside a savepoint, then commits
// only the already-applied rate debt. The failed root and any child written
// before the refusal remain rolled back together. Rollback and commit share one
// short detached budget so cancellation cannot intentionally erase the debt.
func rollbackAgentEmailCellStorageWriteAndCommitRefusal(
	ctx context.Context,
	tx, writeTx pgx.Tx,
	cause error,
) error {
	finalizeCtx, cancel := context.WithTimeout(
		context.WithoutCancel(ctx), agentEmailCellStorageFinalizeTimeout,
	)
	defer cancel()
	if err := writeTx.Rollback(finalizeCtx); err != nil {
		return fmt.Errorf(
			"rollback agent-email cell storage write after capacity refusal: %w", err,
		)
	}
	if err := tx.Commit(finalizeCtx); err != nil {
		return fmt.Errorf(
			"commit agent-email rate debt after cell storage refusal: %w", err,
		)
	}
	return cause
}

// ConfigureAgentEmailCellStorageLimits atomically updates the cell-local
// retained-email admission and hard boundaries installed by schema 91.
// admissionRows counts inbound/outbound correspondence roots; hardRows counts
// every charged root and lifecycle row. Lowering a boundary below current use
// is allowed and immediately closes positive admission while deletes remain
// able to recover capacity.
func (s *Store) ConfigureAgentEmailCellStorageLimits(
	ctx context.Context,
	admissionBytes, admissionRows, hardBytes, hardRows int64,
) error {
	if err := validateAgentEmailCellStorageLimits(
		admissionBytes, admissionRows, hardBytes, hardRows,
	); err != nil {
		return err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin agent-email cell storage configuration: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	tag, err := tx.Exec(ctx, `
		UPDATE agent_email_cell_storage_capacity
		   SET admission_bytes=$1,
		       admission_root_rows=$2,
		       hard_bytes=$3,
		       hard_counted_rows=$4,
		       updated_at=clock_timestamp()
		 WHERE singleton=1`,
		admissionBytes, admissionRows, hardBytes, hardRows,
	)
	if err != nil {
		return fmt.Errorf("configure agent-email cell storage limits: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return fmt.Errorf("configure agent-email cell storage limits: singleton state is missing")
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit agent-email cell storage configuration: %w", err)
	}
	return nil
}

func validateAgentEmailCellStorageLimits(
	admissionBytes, admissionRows, hardBytes, hardRows int64,
) error {
	for name, value := range map[string]int64{
		"admission bytes": admissionBytes,
		"admission rows":  admissionRows,
		"hard bytes":      hardBytes,
		"hard rows":       hardRows,
	} {
		if value < 1 || value > maximumAgentEmailCellStorageLimit {
			return fmt.Errorf(
				"agent-email cell storage %s must be between 1 and %d",
				name, maximumAgentEmailCellStorageLimit,
			)
		}
	}
	if admissionBytes >= hardBytes {
		return fmt.Errorf("agent-email cell storage admission bytes must be smaller than hard bytes")
	}
	if admissionRows >= hardRows {
		return fmt.Errorf("agent-email cell storage admission rows must be smaller than hard rows")
	}
	return nil
}
