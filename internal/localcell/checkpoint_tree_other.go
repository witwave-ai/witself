//go:build !linux && !darwin && !windows

package localcell

import (
	"io/fs"
	"os"
)

// checkpointTree refuses on other systems; checkpoints refuse there before any write.
type checkpointTree struct{}

func (c *Cell) checkpointTree(string) *checkpointTree { return &checkpointTree{} }

func (t *checkpointTree) hold() error                       { return errCheckpointState }
func (t *checkpointTree) close()                            {}
func (t *checkpointTree) lstat(string) (fs.FileInfo, error) { return nil, errCheckpointState }
func (t *checkpointTree) mkdir(string) error                { return errCheckpointState }
func (t *checkpointTree) createTemp(string) (*os.File, string, error) {
	return nil, "", errCheckpointState
}
func (t *checkpointTree) publish(string, string) error { return errCheckpointState }
func (t *checkpointTree) sync(string) error            { return errCheckpointState }
func (t *checkpointTree) discard() error               { return errCheckpointState }
