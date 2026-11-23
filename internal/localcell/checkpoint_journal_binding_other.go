//go:build !linux && !darwin

package localcell

import (
	"context"
	"os"
)

func (c *Cell) repairOwnedJournal(context.Context, *os.File, *os.Root) (CheckpointJournalStatus, error) {
	return "", errCheckpointState
}
