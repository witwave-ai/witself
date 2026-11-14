package localcell

import (
	"os"
	"path/filepath"
)

// Per-cell failure injection; production always uses the writer's remover.
type postgresPendingOperations struct {
	readDir func(string) ([]os.DirEntry, error)
	remove  func(string) error
}

// configurePostgres holds the cell lock with PostgreSQL stopped. Only its own
// top-level temporary files are eligible; all other checkpoint refusals remain.
func (c *Cell) sweepPostgresPending() {
	// In particular, validate the Windows PGDATA boundary exactly as the writer
	// does before inspecting children. The writer still reports its own errors.
	if c.validatePostgresDirectory() != nil {
		return
	}
	ops := postgresPendingOperations{readDir: os.ReadDir, remove: removePrivateFile}
	if c.postgresPendingTest != nil {
		ops = *c.postgresPendingTest
	}
	root := c.path("pg")
	entries, err := ops.readDir(root)
	if err != nil {
		return
	}
	for _, entry := range entries {
		if !privateTempName(entry.Name()) {
			continue
		}
		path := filepath.Join(root, entry.Name())
		if postgresPendingRegular(root, path) {
			_ = ops.remove(path)
		}
	}
}
