//go:build !linux && !darwin && !windows

package localcell

import (
	"context"
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
func (t *checkpointTree) open(context.Context, string) (*os.File, error) {
	return nil, errCheckpointState
}
func (t *checkpointTree) check(map[string]bool) (int, error) { return 0, errCheckpointState }
func (t *checkpointTree) record(string, []byte) error        { return errCheckpointState }
