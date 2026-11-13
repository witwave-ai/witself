//go:build !linux && !darwin && !windows

package localcell

import "os"

// Checkpoints are unsupported on this platform: every boundary refuses.
func checkpointFreeSpace(string) (uint64, uint64, error) { return 0, 0, errCheckpointState }

func (c *Cell) openCheckpointSource() (func(string) (*os.File, error), func(), error) {
	return nil, nil, errCheckpointState
}

func validateRecoveryObject(string, bool) error { return errCheckpointState }
