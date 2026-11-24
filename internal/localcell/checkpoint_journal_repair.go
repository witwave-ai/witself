package localcell

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"os"
	"runtime"
)

// CheckpointJournalStatus describes only the journal after successful settlement.
type CheckpointJournalStatus string

// Successful settlements describe only the disposition of the journal.
const (
	CheckpointJournalAbsent        CheckpointJournalStatus = "absent"
	CheckpointJournalClean         CheckpointJournalStatus = "clean"
	CheckpointJournalTailDiscarded CheckpointJournalStatus = "tail_discarded"
)

var errCheckpointJournalIO = errors.New("cannot settle local cell checkpoint journal")

// RepairCheckpointJournal durably discards at most one uncommitted suffix on a
// cold, owned cell. Its writer header limits eligible tears without proving
// their origin. It leaves a pending operation open and never appends a record.
func (c *Cell) RepairCheckpointJournal(ctx context.Context) (CheckpointJournalStatus, error) {
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		return "", errCheckpointState
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if err := c.validateRoot(false); err != nil {
		return "", err
	}
	lock, err := acquireLock(ctx, c.cfg.Root, false)
	if err != nil {
		return "", err
	}
	if lock == nil {
		return "", errCheckpointState
	}
	defer func() { _ = lock.Close() }()
	root, err := os.OpenRoot(c.cfg.Root + checkpointRootHere)
	if err != nil {
		return "", errCheckpointJournalIO
	}
	defer func() { _ = root.Close() }()
	if err := c.validateRoot(false); err != nil {
		return "", err
	}
	// Only proven absence admits an ordinary operation. No contents of a
	// present maintenance record could grant this caller a generation.
	if _, err := os.Lstat(c.path("journal", "maintenance.json")); !errors.Is(err, os.ErrNotExist) {
		return "", errMaintenanceFenceActive
	}
	m, err := readManifest(ctx, c.cfg.Root)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	if m != nil && m.Cleanup != nil {
		return "", errCheckpointState
	}
	if _, err := os.Lstat(c.path("pg", "postmaster.pid")); err == nil {
		return "", errCheckpointState
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", errCheckpointJournalIO
	}
	if err := c.checkpointCold(ctx); err != nil {
		return "", err
	}
	return c.repairOwnedJournal(ctx, lock.files[0], root)
}

func (c *Cell) settleCheckpointJournal(ctx context.Context, rootDir, journalDir *os.Root, f *os.File, size int64, revalidate func() error) (status CheckpointJournalStatus, err error) {
	defer func() {
		c.journalSettlementBoundary("close", f)
		if closeErr := f.Close(); closeErr != nil && err == nil {
			status, err = "", errCheckpointJournalIO
		}
	}()
	first := sha256.New()
	progress, err := parseJournalFile(io.TeeReader(io.LimitReader(f, size), first), checkpointTailCompatible)
	if err != nil {
		return "", err
	}
	if progress.pending != nil && !checkpointLeftover(journalProgress{pending: progress.pending}) {
		return "", errCheckpointState
	}
	c.journalSettlementBoundary("validated", f)
	// On a filesystem with coarse timestamps a same-size edit can commit a
	// whole line without changing the observed metadata. Compare every byte.
	// The same pass hashes the validated prefix for the check after settlement.
	second, validated := sha256.New(), sha256.New()
	body := io.NewSectionReader(f, 0, size)
	n, err := io.Copy(io.MultiWriter(second, validated), io.LimitReader(body, progress.durableBytes))
	if rest, restErr := io.Copy(second, body); err == nil {
		n, err = n+rest, restErr
	}
	if err != nil || n != size || !bytes.Equal(first.Sum(nil), second.Sum(nil)) {
		return "", errCheckpointJournalIO
	}
	if err := revalidate(); err != nil {
		return "", err
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	status = CheckpointJournalClean
	if progress.tornTail {
		c.journalSettlementBoundary("truncate", f)
		if err := f.Truncate(progress.durableBytes); err != nil {
			return "", errCheckpointJournalIO
		}
		status = CheckpointJournalTailDiscarded
	}
	if err := syncFile(f); err != nil {
		return "", errCheckpointJournalIO
	}
	if err := syncCheckpointRoot(journalDir); err != nil {
		return "", errCheckpointJournalIO
	}
	if err := syncCheckpointRoot(rootDir); err != nil {
		return "", errCheckpointJournalIO
	}
	// A writer that ignores the owner lock can still change the held file after
	// the final check. Report success only for exactly the validated prefix.
	c.journalSettlementBoundary("settled", f)
	info, err := f.Stat()
	if err != nil || info.Size() != progress.durableBytes {
		return "", errCheckpointJournalIO
	}
	settled := sha256.New()
	if n, err := io.Copy(settled, io.NewSectionReader(f, 0, progress.durableBytes)); err != nil || n != progress.durableBytes || !bytes.Equal(settled.Sum(nil), validated.Sum(nil)) {
		return "", errCheckpointJournalIO
	}
	return status, nil
}

func (c *Cell) journalSettlementBoundary(stage string, f *os.File) {
	if c.beforeJournalSettlement != nil {
		c.beforeJournalSettlement(stage, f)
	}
}

// checkpointTailCompatible compares only the writer's fixed header. The
// parser has already checked for an uncommitted JSON value or prefix and
// removed terminal NUL fill, including fill larger than its read buffer.
func checkpointTailCompatible(pending *Interruption, tail []byte) bool {
	matches := func(operation, step, state string) bool {
		header := []byte(`{"operation":"` + operation + `","step":"` + step + `","state":"` + state + `","at":`)
		return bytes.HasPrefix(header, tail) || bytes.HasPrefix(tail, header)
	}
	if pending == nil {
		return matches("checkpoint", "operation", "started") || matches("checkpoint_delete", "operation", "started")
	}
	for _, step := range []string{"operation", "copy", "inventory", "witness", "unseal", "remove"} {
		if !checkpointLeftover(journalProgress{pending: &Interruption{Operation: pending.Operation, Step: step}}) {
			continue
		}
		if matches(pending.Operation, step, "done") || (step != "operation" &&
			(matches(pending.Operation, step, "started") || matches(pending.Operation, step, "failed"))) {
			return true
		}
	}
	return false
}
