//go:build !windows

package localcell

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"
)

// reinstallLstatSnapshot is retainedSnapshot that opens only readable regular
// files: a FIFO, symlink or unreadable entry is recorded by its Lstat mode and
// size, so the snapshot itself can neither block nor follow it.
func reinstallLstatSnapshot(t *testing.T, c *Cell) map[string]retainedEntry {
	t.Helper()
	result := map[string]retainedEntry{}
	err := filepath.WalkDir(c.cfg.Root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		name, err := filepath.Rel(c.cfg.Root, path)
		if err != nil {
			return err
		}
		value := retainedEntry{mode: info.Mode()}
		if !entry.IsDir() {
			value.size = info.Size()
		}
		if info.Mode().IsRegular() && info.Mode().Perm()&0o400 != 0 && !strings.HasSuffix(name, ".lock") {
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			value.data = string(data)
		}
		result[filepath.ToSlash(name)] = value
		return nil
	})
	if err != nil {
		t.Fatal("cannot snapshot synthetic retained tree")
	}
	return result
}

// pg/PG_VERSION is read only after Lstat shows a regular file of at most 64
// bytes, as postmaster() reads its file: a symlink out of the root is never
// followed, a FIFO cannot block admission or Inspect under the operation lock,
// and a directory or unreadable file refuses (review LC-S7D2D-XR1-5). Each is
// state unsupported with no boundary call, an unchanged tree and a released lock.
func TestReinstallPGVersionSpecialFiles(t *testing.T) {
	for _, kind := range []string{"symlink", "fifo", "directory", "unreadable"} {
		t.Run(kind, func(t *testing.T) {
			if kind == "unreadable" && os.Geteuid() == 0 {
				t.Skip("root reads a mode-0 file")
			}
			f, r := reinstallFixture(t, "legacy", false)
			c := f.cell
			path := c.path("pg", "PG_VERSION")
			var prepared bool
			switch kind {
			case "symlink":
				// A short relative target outside the root, so the 64-byte bound
				// alone cannot refuse it: only the regular-file check does.
				outside := filepath.Join(filepath.Dir(c.cfg.Root), "outside-pg-version")
				prepared = os.WriteFile(outside, []byte("16\n"), 0o600) == nil && os.Remove(path) == nil &&
					os.Symlink(filepath.Join("..", "..", filepath.Base(outside)), path) == nil
			case "fifo":
				prepared = os.Remove(path) == nil && syscall.Mkfifo(path, 0o600) == nil
			case "directory":
				prepared = os.Remove(path) == nil && os.Mkdir(path, 0o700) == nil
			case "unreadable":
				prepared = os.Chmod(path, 0) == nil
			}
			if !prepared {
				t.Fatal("cannot prepare the special PG_VERSION")
			}
			// Each call is joined and bounded by the named absolute ceiling. Cleanup
			// opens a FIFO's writer end, so a reader blocked in open always returns.
			bounded := func(name string, run func() error) error {
				done := make(chan struct{})
				result := StartJoinedTestRunForTest(t, func() {
					for range 100 {
						select {
						case <-done:
							return
						default:
						}
						if kind == "fifo" {
							if writer, err := os.OpenFile(path, os.O_WRONLY|syscall.O_NONBLOCK, 0); err == nil {
								_ = writer.Close()
							}
						}
						time.Sleep(10 * time.Millisecond)
					}
				}, reinstallContentionBound, func() error {
					defer close(done)
					return run()
				})
				select {
				case err := <-result:
					return err
				case <-time.After(reinstallContentionBound):
					t.Fatal(name + " blocked on a special PG_VERSION")
					return nil
				}
			}
			before := reinstallLstatSnapshot(t, c)
			err := bounded("reinstall", func() error { return c.Reinstall(context.Background(), reinstallOptions(f)) })
			if !reflect.DeepEqual(before, reinstallLstatSnapshot(t, c)) || len(r.calls) != 0 || !errors.Is(err, errReinstallState) {
				t.Fatal("special PG_VERSION was followed, read or admitted")
			}
			// The refusal released the operation lock: a non-blocking probe takes it.
			lock, err := acquireLock(c.cfg.Root, false)
			if err != nil || lock == nil {
				t.Fatal("refused reinstall kept the operation lock")
			}
			_ = lock.Close()
			state := ""
			if err := bounded("inspect", func() error {
				inspection, err := c.Inspect(context.Background())
				state = inspection.InstallState
				return err
			}); err != nil || state != "removed; data retained; reinstall unsupported" {
				t.Fatal("inspection offered reinstall over a special PG_VERSION")
			}
		})
	}
}

// The review's reachable overclaim, through production code: a purge whose
// deletion fails after the manifest already says not installed, settled by
// Repair, keeps the marker with PGDATA present. Inspect must not offer
// reinstall; Reinstall refuses, and Remove(Purge: true) owns that root
// (review LC-S7D2D-XR1-3).
func TestReinstallInspectInterruptedPurgeSettledByRepair(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root deletes from a non-writable directory")
	}
	f, r := reinstallFixture(t, "legacy", false)
	c := f.cell
	if c.Reinstall(t.Context(), reinstallOptions(f)) != nil {
		t.Fatal("cannot readmit the fixture before its purge")
	}
	blocked := c.path("pg", "base")
	if os.Mkdir(blocked, 0o700) != nil || os.WriteFile(filepath.Join(blocked, "relation"), []byte("inert relation bytes"), 0o600) != nil || os.Chmod(blocked, 0o500) != nil {
		t.Fatal("cannot prepare an undeletable PGDATA entry")
	}
	t.Cleanup(func() { _ = os.Chmod(blocked, 0o700) })
	if err := c.Remove(t.Context(), RemoveOptions{Purge: true}); err == nil || err.Error() != "local cell remove: purge failed" {
		t.Fatal("purge did not fail inside its deletion step")
	}
	if c.Repair(t.Context()) != nil {
		t.Fatal("Repair could not settle the failed purge")
	}
	if m, err := readManifest(c.cfg.Root); err != nil || m.Installed || !m.RemovalRetained {
		t.Fatal("settled purge did not keep the marker on an uninstalled manifest")
	}
	before, calls := retainedSnapshot(t, c), len(r.calls)
	inspection, err := c.Inspect(t.Context())
	if err != nil || !inspection.PGDataPresent || inspection.InstallState != "removed; data retained; reinstall unsupported" {
		t.Fatal("inspection offered reinstall for a settled interrupted purge")
	}
	if err := c.Reinstall(t.Context(), reinstallOptions(f)); !errors.Is(err, errReinstallState) || len(r.calls) != calls || !reflect.DeepEqual(before, retainedSnapshot(t, c)) {
		t.Fatal("reinstall admitted a settled interrupted purge")
	}
}
