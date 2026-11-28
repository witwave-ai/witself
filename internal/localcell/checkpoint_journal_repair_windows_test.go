//go:build windows

package localcell

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"testing"
	"unsafe"

	"github.com/witwave-ai/witself/internal/runtimeconfig"
	"golang.org/x/sys/windows"
)

// windowsBasicTimes is FILE_BASIC_INFO; a zero time or attribute is left as it is.
type windowsBasicTimes struct {
	creation, access, write, change int64
	attributes, _                   uint32
}

// Each row plants one fault at one stage: before the repair, after the root is
// held, or after the parse with every hold taken. None may cut or report status.
func TestRepairCheckpointJournalWindowsBinding(t *testing.T) {
	ioClass, layout := "local_checkpoint_operation_failed", "local_cell_layout_refused"
	for _, tc := range []struct{ name, stage, class, failure string }{
		{"alias", "prepare", layout, "repair accepted an aliased journal"}, {"owner-alias", "prepare", layout, "repair bypassed the ownership gate"},
		{"open-writer", "prepare", ioClass, "repair truncated beside an open writer"}, {"mapped-view", "prepare", ioClass, "repair truncated beside a mapped view"},
		{"read-only", "prepare", ioClass, "repair modified a protected object"},
		{"junction-dir", "hold", layout, "repair accepted a reparse journal"}, {"symlink-file", "hold", layout, "repair accepted a reparse journal"},
		{"journal-file", "hold", ioClass, "a journal/ that is not a directory was treated as absence"},
		{"journal-dir", "hold", layout, "repair accepted a directory as the journal"},
		{"absent-dir-owner-alias", "hold", layout, "failed metadata was treated as absence"},
		{"absent-cancelled", "hold", "local_checkpoint_interrupted", "cancelled repair reported absence"},
		{"owner-replaced", "validated", ioClass, "repair accepted a different journal"}, {"privacy", "validated", layout, "repair accepted a shared journal"},
		{"seek", "validated", ioClass, "repair trusted a parse that did not end at the observed size"},
		{"size", "validated", ioClass, "repair trusted a journal whose size moved"},
		{"change-time", "validated", ioClass, "repair trusted a journal whose change time moved"},
		{"write-time", "validated", ioClass, "repair trusted a journal whose write time moved"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, _ := checkpointFixture(t, "legacy", false)
			c, outside := f.cell, filepath.Join(testPrivateRoot(t), "other")
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			log, expect, stages := c.path(repairLog), string(f.prefix)+"{", ""
			repairSeed(t, c, expect)
			setup := func(err error) { t.Helper(); drillAssert(t, err == nil, "cannot prepare binding fixture") }
			c.beforeJournalSettlement = func(stage string, held *os.File) {
				if stages += stage + " "; stage != tc.stage {
					return
				}
				switch tc.name {
				case "alias":
					setup(os.Link(log, outside))
				case "owner-alias":
					setup(os.Link(c.path(".owner.lock"), outside))
				case "absent-dir-owner-alias": // only the absence path's own proof can refuse
					setup(os.RemoveAll(c.path("journal")))
					setup(os.Link(c.path(".owner.lock"), outside))
				case "absent-cancelled": // the absence is seen with the context already ended
					setup(os.RemoveAll(c.path("journal")))
					cancel()
				case "journal-file": // a private regular file where journal/ was
					setup(errors.Join(os.Rename(c.path("journal"), c.path("journal.saved")), testWriteFile(c.path("journal"), nil, 0o600)))
					log = c.path("journal.saved", "operations.jsonl")
				case "journal-dir": // a private directory where the journal was
					setup(errors.Join(os.Rename(log, log+".saved"), testMkdir(log, 0o700)))
					log += ".saved"
				case "open-writer":
					writer, err := runtimeconfig.WindowsPrivateOpenFile(log, os.O_WRONLY|os.O_APPEND)
					setup(err)
					t.Cleanup(func() { _ = writer.Close() })
				case "mapped-view": // a writable view outlives its file and mapping handles
					name, err := windows.UTF16PtrFromString(log)
					setup(err)
					file, err := windows.CreateFile(name, windows.GENERIC_READ|windows.GENERIC_WRITE, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil, windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL, 0)
					setup(err)
					mapping, err := windows.CreateFileMapping(file, nil, windows.PAGE_READWRITE, 0, 0, nil)
					setup(errors.Join(err, windows.CloseHandle(file)))
					view, err := windows.MapViewOfFile(mapping, windows.FILE_MAP_WRITE, 0, 0, 0)
					setup(errors.Join(err, windows.CloseHandle(mapping)))
					t.Cleanup(func() { _ = windows.UnmapViewOfFile(view) })
				case "read-only":
					name, err := windows.UTF16PtrFromString(log)
					setup(errors.Join(err, windows.SetFileAttributes(name, windows.FILE_ATTRIBUTE_READONLY)))
					t.Cleanup(func() { _ = windows.SetFileAttributes(name, windows.FILE_ATTRIBUTE_NORMAL) })
				case "junction-dir":
					setup(os.Rename(c.path("journal"), c.path("journal.saved")))
					createTestJunction(t, c.path("journal"), c.path("journal.saved"))
					log = c.path("journal.saved", "operations.jsonl")
				case "symlink-file":
					setup(os.Rename(log, log+".saved"))
					if err := os.Symlink(log+".saved", log); errors.Is(err, windows.ERROR_PRIVILEGE_NOT_HELD) {
						t.Skip("LC-S2-PW3: Windows symlink fixture privileges unavailable")
					} else {
						setup(err)
					}
					log += ".saved"
				case "owner-replaced": // the held lock keeps its one link under another name
					setup(errors.Join(os.Rename(c.path(".owner.lock"), outside), testWriteFile(c.path(".owner.lock"), nil, 0o600)))
				case "privacy":
					setTestDACL(t, log, "(A;;FR;;;WD)", false, false)
				case "seek":
					_, err := held.Seek(0, io.SeekStart)
					setup(err)
				case "size", "change-time", "write-time": // exactly one property moves; the bytes read stay the same
					observed, err := runtimeconfig.WindowsPrivateIdentity(held)
					setup(err)
					times := windowsBasicTimes{write: observed.WriteTime, change: observed.ChangeTime}
					switch tc.name {
					case "size":
						setup(held.Truncate(observed.Size + 1))
						expect += "\x00"
					case "change-time":
						times.change++
					case "write-time":
						times.write++
					}
					setup(windows.SetFileInformationByHandle(windows.Handle(held.Fd()), windows.FileBasicInfo, (*byte)(unsafe.Pointer(&times)), uint32(unsafe.Sizeof(times))))
					now, err := runtimeconfig.WindowsPrivateIdentity(held)
					setup(err)
					drillAssert(t, (now.Size != observed.Size) == (tc.name == "size") && (now.ChangeTime != observed.ChangeTime) == (tc.name == "change-time") &&
						(now.WriteTime != observed.WriteTime) == (tc.name == "write-time"), "cannot move exactly one journal property")
				}
			}
			c.beforeJournalSettlement("prepare", nil)
			status, err := c.RepairCheckpointJournal(ctx)
			drillAssert(t, err != nil, tc.failure)
			repairDiagnostic(t, err, tc.class, "repair diagnostic class changed")
			drillAssert(t, status == "", "repair reported status on failure")
			drillAssert(t, tc.stage != "prepare" || !strings.Contains(stages, "validated"), "repair parsed a journal it had not bound")
			if _, err := os.Lstat(c.path("journal")); strings.HasPrefix(tc.name, "absent-") {
				drillAssert(t, errors.Is(err, os.ErrNotExist), "absence refusal created a journal")
				return
			}
			assertFileBytes(t, log, []byte(expect), "refusal changed journal bytes")
		})
	}
}

// While the repair holds the journal, the cell's own writer cannot open it,
// nothing can rename, replace or remove it, and a reader still opens it. After
// the repair, repaired or refused, journal/ and the root can be renamed: with
// the collector off no finalizer can close a leaked hold, only the repair's own.
func TestRepairCheckpointJournalWindowsHold(t *testing.T) {
	defer debug.SetGCPercent(debug.SetGCPercent(-1))
	for _, row := range []string{"repaired", "refused"} {
		t.Run(row, func(t *testing.T) {
			f, _ := checkpointFixture(t, "legacy", false)
			c := f.cell
			log, other, torn := c.path(repairLog), c.path("journal", "other.jsonl"), string(f.prefix)+"{"
			repairSeed(t, c, torn)
			drillAssert(t, testWriteFile(other, f.prefix, 0o600) == nil, "cannot prepare hold fixture")
			c.beforeJournalSettlement = func(stage string, held *os.File) {
				if stage != "validated" {
					return
				}
				writer, err := openRegularWriter(t.Context(), log, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600, errors.New("irregular"), errors.New("failed"))
				if err == nil {
					_ = writer.Close()
				}
				drillAssert(t, err != nil, "repair admitted a concurrent writer")
				drillAssert(t, os.Rename(log, log+".moved") != nil, "repair let the journal be renamed")
				drillAssert(t, os.Rename(other, log) != nil, "repair let the journal be replaced")
				drillAssert(t, os.Remove(log) != nil, "repair let the journal be removed")
				reader, err := openPrivateReader(t.Context(), log)
				drillAssert(t, err == nil && reader.Close() == nil, "repair refused a reader during its hold")
				if row == "refused" {
					_, err := held.Seek(0, io.SeekStart)
					drillAssert(t, err == nil, "cannot prepare hold fixture")
				}
			}
			status, err := c.RepairCheckpointJournal(t.Context())
			if row == "refused" {
				drillAssert(t, err != nil, "repair trusted a parse that did not end at the observed size")
				repairDiagnostic(t, err, "local_checkpoint_operation_failed", "repair diagnostic class changed")
				drillAssert(t, status == "", "repair reported status on failure")
				assertFileBytes(t, log, []byte(torn), "refusal changed journal bytes")
			} else {
				drillAssert(t, err == nil && status == CheckpointJournalTailDiscarded, "eligible repair status changed")
				assertFileBytes(t, log, f.prefix, "repair changed a validated byte")
			}
			for _, kept := range []struct{ name, failure string }{{c.path("journal"), "a repair left journal/ held"}, {c.cfg.Root, "a repair left the cell root held"}} {
				drillAssert(t, os.Rename(kept.name, kept.name+".moved") == nil && os.Rename(kept.name+".moved", kept.name) == nil, kept.failure)
			}
		})
	}
}

// A root named in another letter case is admitted, as the released verbs admit
// it. One named by its 8.3 short name is refused as noncanonical, although the
// released verbs admit it: the departure the design names.
func TestRepairCheckpointJournalWindowsRootSpelling(t *testing.T) {
	for _, spelling := range []string{"case", "short"} {
		t.Run(spelling, func(t *testing.T) {
			f, _ := checkpointFixture(t, "legacy", false)
			root, prefix := f.cell.cfg.Root, string(f.prefix)
			repairSeed(t, f.cell, prefix+"{")
			configured := filepath.Join(filepath.Dir(root), strings.ToUpper(filepath.Base(root)))
			if spelling == "short" {
				long, buffer := root+"-with-a-long-name", make([]uint16, windows.MAX_LONG_PATH)
				pointer, err := windows.UTF16PtrFromString(long)
				drillAssert(t, err == nil && os.Rename(root, long) == nil, "cannot prepare root spelling fixture")
				n, err := windows.GetShortPathName(pointer, &buffer[0], uint32(len(buffer)))
				drillAssert(t, err == nil && n > 0 && n < uint32(len(buffer)), "cannot prepare root spelling fixture")
				root, configured = long, filepath.Join(filepath.Dir(long), filepath.Base(windows.UTF16ToString(buffer[:n])))
				if strings.EqualFold(filepath.Base(configured), filepath.Base(long)) {
					_, _ = fmt.Fprintf(os.Stderr, "NOT RUN %s: this volume gives the root no 8.3 short name\n", t.Name())
					return
				}
			}
			cfg := f.cell.cfg
			cfg.Root = configured
			c, err := New(cfg)
			drillAssert(t, err == nil && filepath.Base(c.cfg.Root) == filepath.Base(configured), "cannot prepare root spelling fixture")
			_, err = c.Inspect(t.Context())
			drillAssert(t, err == nil, "released inspection refused the root spelling")
			status, err := c.RepairCheckpointJournal(t.Context())
			log := filepath.Join(root, "journal", "operations.jsonl")
			if spelling == "case" {
				drillAssert(t, err == nil && status == CheckpointJournalTailDiscarded, "repair refused a root the released verbs admit")
				assertFileBytes(t, log, []byte(prefix), "repair changed a validated byte")
				return
			}
			repairDiagnostic(t, err, "local_checkpoint_operation_failed", "repair accepted a noncanonical root")
			drillAssert(t, status == "", "repair reported status on failure")
			assertFileBytes(t, log, []byte(prefix+"{"), "refusal changed journal bytes")
		})
	}
}

// Windows flushes no directory: the production directory step returns before it
// touches the root it is given, so even a closed one succeeds.
func TestRepairCheckpointJournalWindowsNoDirectorySync(t *testing.T) {
	root, err := os.OpenRoot(t.TempDir())
	drillAssert(t, err == nil && root.Close() == nil, "cannot prepare directory fixture")
	drillAssert(t, syncCheckpointRoot(root) == nil, "Windows flushed a journal directory")
}
