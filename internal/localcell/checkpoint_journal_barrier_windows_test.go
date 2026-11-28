//go:build windows

package localcell

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"testing"
	"time"
	"unsafe"

	"github.com/witwave-ai/witself/internal/runtimeconfig"
	"golang.org/x/sys/windows"
)

var barrierStages = []string{"file"}

// The two relative opens carry synthetic names. The full-path writer can
// either append or truncate a torn tail; only the latter has FILE_WRITE_DATA.
func barrierDescriptor(t *testing.T, f *os.File, log string) (string, bool) {
	t.Helper()
	switch f.Name() {
	case "private flush", "private hold":
		return "file", f.Name() == "private flush"
	}
	if f.Name() != log {
		return "", false
	}
	var access uint32
	var status windows.IO_STATUS_BLOCK
	err := windows.NtQueryInformationFile(windows.Handle(f.Fd()), &status, (*byte)(unsafe.Pointer(&access)), 4, 8) // FileAccessInformation
	drillAssert(t, err == nil, "cannot observe journal access")
	if access&windows.FILE_WRITE_DATA != 0 {
		return "file", false
	}
	return "append", false
}

func denyJournalWrites(t *testing.T, c *Cell) *os.File {
	t.Helper()
	name, err := windows.UTF16PtrFromString(c.path(repairLog))
	drillAssert(t, err == nil, "cannot encode journal name")
	h, err := windows.CreateFile(name, windows.GENERIC_READ, windows.FILE_SHARE_READ, nil, windows.OPEN_EXISTING, 0, 0)
	drillAssert(t, err == nil, "cannot hold a reader that denies writing")
	reader := os.NewFile(uintptr(h), "deny-write reader")
	t.Cleanup(func() { _ = reader.Close() })
	return reader
}

func TestWindowsJournalBarrierFailure(t *testing.T) {
	drillAssert(t, new(Cell).syncCheckpointJournalPrefix(t.Context(), journalProgress{absent: true}) == nil, "barrier refused an absent journal")
	for _, row := range []string{"deny-write", "released", "vanished", "close"} {
		t.Run(row, func(t *testing.T) {
			f, call := barrierFixture(t, "create")
			c := f.cell
			before := checkpointTreeState(t, c)
			trace := traceBarrier(t, c)
			switch row {
			case "vanished":
				progress, err := c.readJournal(t.Context())
				drillAssert(t, err == nil && os.Remove(c.path(repairLog)) == nil, "cannot remove the observed journal")
				drillAssert(t, errors.Is(c.syncCheckpointJournalPrefix(t.Context(), progress), errCheckpointJournalIO), "barrier admitted a vanished journal")
				return
			case "released":
				reader, barrier := denyJournalWrites(t, c), runWindowsJournalBarrier
				t.Cleanup(func() { runWindowsJournalBarrier = barrier })
				runWindowsJournalBarrier = func(c *Cell, ctx context.Context, p journalProgress) error {
					err := barrier(c, ctx, p)
					drillAssert(t, reader.Close() == nil, "cannot release the reader before append")
					return err
				}
				drillAssert(t, call() == nil, "released reader still prevented the checkpoint")
				drillAssert(t, !slices.Contains(trace.events, "file"), "barrier flushed beside a deny-write reader")
				checkBarrierRecords(t, f, "create", false)
				entries, valid := journalDirectorySyncLines(retainedRead(t, c, repairLog)[len(f.prefix):])
				drillAssert(t, valid, "released checkpoint wrote an invalid journal record")
				drillAssert(t, !slices.ContainsFunc(entries, func(e journalEntry) bool { return e.Cause != "" }), "released checkpoint recorded a failure cause")
				return
			case "deny-write":
				denyJournalWrites(t, c)
				parent := c.journal(t.Context(), "checkpoint", "operation", "started")
				drillAssert(t, checkpointSameError(parent, "cannot open local cell journal"), "parent append admitted a deny-write holder")
				err := call()
				drillAssert(t, checkpointSameError(err, parent.Error()), "deny-write changed the append open error")
				drillAssert(t, ClassifyCheckpointFailure(err).Class == ClassifyCheckpointFailure(parent).Class, "barrier changed a refusal class")
				drillAssert(t, checkpointSameError(c.Stop(t.Context()), parent.Error()), "deny-write changed the lifecycle append open error")
				drillAssert(t, len(trace.events) == 0, "a refused append flushed the journal")
			case "close":
				file := syncFile
				t.Cleanup(func() { syncFile = file })
				syncFile = func(f *os.File) error {
					if f.Name() == "private flush" {
						return f.Close() // successful hook, followed by the barrier's failed close
					}
					return file(f)
				}
				drillAssert(t, errors.Is(call(), errCheckpointJournalIO), "journal close failure was acknowledged")
			}
			checkpointUnchanged(t, c, before)
		})
	}
}

func TestWindowsJournalBarrierInstall(t *testing.T) {
	f, _ := checkpointFixture(t, "legacy", false)
	trace := traceBarrier(t, f.cell)
	err := f.cell.Install(t.Context(), InstallOptions{})
	drillAssert(t, checkpointSameError(err, "local cell is already installed"), "install admission refusal changed")
	drillAssert(t, len(trace.events) == 0, "refused install flushed the journal")
	drillAssert(t, f.cell.begin(t.Context(), "install") == nil, "install begin refused a healthy prefix")
	drillAssert(t, slices.Equal(trace.events, []string{"file", "append"}), "install begin appended before the prefix was durable")
}

// Restore the prefix outside the timer. Each real create supplies its paired delete.
func TestWindowsJournalBarrierLatency(t *testing.T) {
	if os.Getenv("WITSELF_TEST_BARRIER_LATENCY") != "1" {
		return
	}
	f, recorder := checkpointFixture(t, "legacy", false)
	recorder.nextID = func() (string, error) { return locklessTarget, nil }
	verbs := []string{"create", "delete", "stop"}
	calls := []func() error{leftoverCreate(t, f), leftoverDelete(t, f.cell, locklessTarget), func() error { return f.cell.Stop(t.Context()) }}
	realBarrier := runWindowsJournalBarrier
	t.Cleanup(func() { runWindowsJournalBarrier = realBarrier })
	var samples [2][3][]time.Duration
	for i := range 400 {
		mode, barriers := i%2, 0
		runWindowsJournalBarrier = func(c *Cell, ctx context.Context, p journalProgress) error {
			barriers++
			if mode == 0 {
				return nil
			}
			return realBarrier(c, ctx, p)
		}
		repairSeed(t, f.cell, string(f.prefix))
		for v, call := range calls {
			start := time.Now()
			err := call()
			samples[mode][v] = append(samples[mode][v], time.Since(start))
			drillAssert(t, err == nil, "timed journal writer failed")
			drillAssert(t, barriers == v+1, "timed writer missed its single prefix barrier")
		}
	}
	for mode, rows := range samples {
		for v, times := range rows {
			slices.Sort(times)
			fmt.Printf("BARRIER_LATENCY verb=%s flush=%t comparison=whole_barrier n=%d median=%s p90=%s p99=%s max=%s\n", verbs[v], mode == 1, len(times), (times[99]+times[100])/2, times[179], times[197], times[199])
		}
	}
}

// Another program holds journal/ or the root with delete access while the
// barrier runs and lets go before the append: the barrier makes no flush and
// the verb finishes as it does at the parent.
func TestWindowsJournalBarrierDirectoryOverlap(t *testing.T) {
	for _, held := range []string{"journal", "root"} {
		t.Run(held, func(t *testing.T) {
			f, create := barrierFixture(t, "create")
			path := f.cell.cfg.Root
			if held == "journal" {
				path = f.cell.path("journal")
			}
			barrier, file, flushes, entered := runWindowsJournalBarrier, syncFile, 0, 0
			t.Cleanup(func() { runWindowsJournalBarrier, syncFile = barrier, file })
			syncFile = func(f *os.File) error {
				if f.Name() == "private flush" {
					flushes++
				}
				return file(f)
			}
			runWindowsJournalBarrier = func(c *Cell, ctx context.Context, p journalProgress) error {
				name, err := windows.UTF16PtrFromString(path)
				drillAssert(t, err == nil, "cannot encode the held directory's name")
				h, err := windows.CreateFile(name, windows.DELETE, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
					nil, windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
				drillAssert(t, err == nil, "cannot hold the directory with delete access")
				// The peer's handle is what refuses the hold, with a sharing violation.
				entered++
				refusal := errors.New("not refused")
				if held == "root" {
					_, refusal = runtimeconfig.WindowsPrivateHoldPath(c.cfg.Root)
				} else if rootHold, holdErr := runtimeconfig.WindowsPrivateHoldPath(c.cfg.Root); holdErr == nil {
					_, refusal = runtimeconfig.WindowsPrivateHold(rootHold, "journal", true)
					_ = rootHold.Close()
				}
				drillAssert(t, errors.Is(refusal, windows.ERROR_SHARING_VIOLATION), "the peer did not refuse the directory hold with a sharing violation")
				err = barrier(c, ctx, p)
				drillAssert(t, windows.CloseHandle(h) == nil, "cannot release the directory holder")
				drillAssert(t, err == nil, "the barrier refused while another handle held a directory")
				return err
			}
			drillAssert(t, create() == nil, "the verb failed after the directory holder left")
			drillAssert(t, entered == 1, "the barrier was not dispatched exactly once")
			drillAssert(t, flushes == 0, "the barrier flushed without its holds")
			checkBarrierRecords(t, f, "create", false)
		})
	}
}
