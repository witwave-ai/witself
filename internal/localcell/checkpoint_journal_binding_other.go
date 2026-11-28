//go:build !linux && !darwin && !windows

package localcell

import (
	"context"
	"os"
)

// This system has no native binding of the journal yet, so the operation
// refuses here. The shared settlement stays compiled and referenced, so that a
// binding only has to add its own file.
var _ = (*Cell).settleCheckpointJournal

func (c *Cell) repairOwnedJournal(context.Context, *os.File, *os.Root) (CheckpointJournalStatus, error) {
	return "", errCheckpointState
}

func (c *Cell) syncCheckpointJournalPrefix(context.Context, journalProgress) error { return nil }
