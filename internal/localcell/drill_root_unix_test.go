//go:build linux || darwin

package localcell

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"syscall"
	"testing"
	"time"
)

// drillRootWatchdog runs the delete in a goroutine; one still running after ten
// seconds is unblocked, awaited again and fails the row.
func drillRootWatchdog(t *testing.T, c *Cell, drillID string, unblock func()) error {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- c.deleteDrillLocked(drillID) }()
	select {
	case err := <-done:
		return err
	case <-time.After(10 * time.Second):
	}
	unblock()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
	}
	t.Fatal("drill delete blocked on a planted fifo")
	return nil
}

// drillRootFifo puts a FIFO at path, moving the drill aside or removing the
// record first, and for writer holds a writer open with no reader left.
func drillRootFifo(path, aside string, atName, writer bool, writers chan<- *os.File) error {
	var err error
	if atName {
		err = os.Rename(path, aside)
	} else {
		err = os.Remove(path)
	}
	if err = errors.Join(err, syscall.Mkfifo(path, 0o600)); err != nil || !writer {
		return err
	}
	reader, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return err
	}
	held, err := os.OpenFile(path, os.O_WRONLY|syscall.O_NONBLOCK, 0)
	if err == nil {
		writers <- held
	}
	return errors.Join(err, reader.Close())
}

func init() {
	drillRootLink = func(t *testing.T, path, target string) {
		drillAssert(t, os.Symlink(target, path) == nil, "cannot prepare the drill root fixture")
	}
	drillRootLock = func(t *testing.T, file string) func() {
		if os.Geteuid() == 0 {
			t.Skip("root deletes from a non writable directory")
		}
		dir := filepath.Dir(file)
		drillAssert(t, os.Chmod(dir, 0o500) == nil, "cannot prepare the drill root fixture")
		t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
		return func() { drillAssert(t, os.Chmod(dir, 0o700) == nil, "cannot prepare the drill root fixture") }
	}
	drillRootPlatformDeleteRows = func(t *testing.T) {
		t.Run("name_linked", func(t *testing.T) {
			c, v := drillRootFixture(t)
			dir := drillRootPlant(t, c, v.DrillID, v)
			aside := filepath.Join(c.recoveryDir(), "aside")
			drillRootAt(c, "sweep", 1, func() {
				drillAssert(t, os.Rename(dir, aside) == nil && os.Symlink("aside", dir) == nil, "cannot prepare the drill root fixture")
			})
			drillAssert(t, c.deleteDrillLocked(v.DrillID) == (drillRootError{reason: "boundary_refused"}) && drillRootKept(t, aside, v),
				"drill delete followed a replaced drill name")
		})
		for _, row := range []struct {
			name           string
			atName, writer bool
		}{{"fifo_name", true, false}, {"fifo_record/reader_only", false, false}, {"fifo_record/writer_held", false, true}} {
			t.Run(row.name, func(t *testing.T) {
				c, v := drillRootFixture(t)
				dir := drillRootPlant(t, c, v.DrillID, v)
				fifo, payload, writers := filepath.Join(dir, drillRootRecordName), filepath.Join(dir, "pg", "payload"), make(chan *os.File, 2)
				if row.atName {
					fifo, payload = dir, filepath.Join(c.path("aside"), "pg", "payload")
				}
				defer func() {
					close(writers)
					for writer := range writers {
						_ = writer.Close()
					}
				}()
				prepared := errors.New("stage not reached")
				drillRootAt(c, "open", 1, func() { prepared = drillRootFifo(fifo, c.path("aside"), row.atName, row.writer, writers) })
				err := drillRootWatchdog(t, c, v.DrillID, func() {
					select {
					case writer := <-writers:
						_ = writer.Close()
					default:
						if writer, err := os.OpenFile(fifo, os.O_WRONLY|syscall.O_NONBLOCK, 0); err == nil {
							writers <- writer
						}
					}
				})
				data, readErr := os.ReadFile(payload)
				drillAssert(t, prepared == nil && err == (drillRootError{reason: "unknown_kind"}) && readErr == nil && string(data) == "inert payload",
					"drill delete blocked on a planted fifo")
			})
		}
	}
	drillRootPlatformRows = func(t *testing.T) {
		t.Run("link", func(t *testing.T) {
			c, v := drillRootFixture(t)
			dir := drillRootPlant(t, c, v.DrillID, v)
			target := c.path("outside")
			// Deliberately malformed boundary object: a Unix symbolic link.
			drillAssert(t, os.Rename(dir, target) == nil && os.Symlink(target, dir) == nil, "cannot prepare the drill root fixture")
			before := checkpointTreeState(t, c)
			_, err := c.drillRootList()
			drillAssert(t, err == (drillRootError{reason: "boundary_refused"}), "drill delete followed a link")
			drillRootRefused(t, c, v.DrillID, "boundary_refused", "drill delete followed a link")
			drillAssert(t, reflect.DeepEqual(before, checkpointTreeState(t, c)), "drill delete followed a link")
		})
	}
}
