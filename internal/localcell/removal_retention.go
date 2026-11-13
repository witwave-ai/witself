package localcell

import (
	"errors"
	"os"
	"time"
)

// All removal helpers require the root operation lock and a verified stop.
// Installed remains the initialized identity, including for service ownership.
// Repair never derives purge consent from a journal or a manifest checkpoint.
func (c *Cell) retainRemoval(m *Manifest) error {
	if !m.RemovalRetained {
		m.RemovalRetained = true
		if err := writeAtomic(c.path("manifest.json"), m, nil); err != nil {
			return err
		}
	}
	return c.removalBoundary("removal_retained")
}

func (c *Cell) removalBoundary(step string) error {
	if c.cfg.Failpoint != nil && c.cfg.Failpoint("remove", step) != nil {
		return errInterrupted
	}
	return nil
}

// finishRetainedRemoval settles only the actual open operation. A manifest-only
// checkpoint can outlive the journal (old removal), or its physical completion
// (crash before checkpoint clear). Neither permits invented or duplicate lines.
func (c *Cell) finishRetainedRemoval(m *Manifest) error {
	progress, err := c.readJournal()
	if err != nil {
		return err
	}
	if progress.pending == nil || progress.pending.Operation != "remove" || progress.tornTail {
		return errors.New("mismatched local cell journal operation")
	}
	if m.Cleanup == nil {
		m.LastOperation = "remove"
		m.Cleanup = &journalEntry{Operation: "remove", Step: "runtime_cleanup", State: "started", At: time.Now().UTC()}
		if err := writeAtomic(c.path("manifest.json"), m, nil); err != nil {
			return err
		}
		if err := c.removalBoundary("cleanup_checkpoint"); err != nil {
			return err
		}
	}
	if !progress.cleanupOnly && !progress.done["runtime_cleanup"] && progress.pending.Step != "runtime_cleanup" {
		if err := c.journal("remove", "runtime_cleanup", "started"); err != nil {
			return err
		}
	}
	// Keep the existing interruption boundaries while retaining the entire
	// private trees, including unknown entries. No credential repair occurs.
	for _, step := range []string{"cleanup_run", "cleanup_journal"} {
		if err := c.removalBoundary(step); err != nil {
			return err
		}
	}
	if !progress.cleanupOnly {
		if !progress.done["runtime_cleanup"] {
			if err := c.journal("remove", "runtime_cleanup", "done"); err != nil {
				return err
			}
			if err := c.removalBoundary("runtime_cleanup"); err != nil {
				return err
			}
		}
		if err := c.journal("remove", "operation", "done"); err != nil {
			return err
		}
		if err := c.removalBoundary("operation_done"); err != nil {
			return err
		}
	} else if progress.durableBytes != 0 {
		// A previous append may have returned a sync error after writing a
		// complete line. Re-acknowledge that physical prefix before dropping
		// the remaining checkpoint; an absent legacy journal is never created.
		f, err := openPrivateFile(c.path("journal", "operations.jsonl"), os.O_WRONLY, 0)
		if err != nil {
			return errors.New("cannot open local cell journal")
		}
		err = syncFile(f)
		closeErr := f.Close()
		if err != nil || closeErr != nil {
			return errors.New("cannot sync local cell journal")
		}
	}
	if err := c.removalBoundary("before_cleanup_clear"); err != nil {
		return err
	}
	m.Cleanup = nil
	if err := writeAtomic(c.path("manifest.json"), m, nil); err != nil {
		return err
	}
	if err := c.removalBoundary("cleanup_clear"); err != nil {
		return err
	}
	return syncDirectory(c.cfg.Root)
}
