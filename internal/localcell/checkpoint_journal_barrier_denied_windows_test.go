//go:build windows

package localcell

import (
	"errors"
	"os"
	"testing"

	"github.com/witwave-ai/witself/internal/runtimeconfig"
	"golang.org/x/sys/windows"
)

// A directory whose access control denies the barrier's hold is tolerated like
// one that another handle holds: the barrier makes no flush and returns, and
// the append that follows makes its own opens.
func TestWindowsJournalBarrierDirectoryDenied(t *testing.T) {
	for _, where := range []string{"root", "journal"} {
		t.Run(where, func(t *testing.T) {
			f, _ := barrierFixture(t, "create")
			c := f.cell
			progress, err := c.readJournal(t.Context())
			if err != nil || progress.readFile == nil {
				t.Fatal("cannot observe the journal before the directory is denied")
			}
			path := c.cfg.Root
			if where == "journal" {
				path = c.path("journal")
			}
			previous := testSecurityDescriptor(t, path)
			name, err := windows.UTF16PtrFromString(path)
			if err != nil {
				t.Fatal("cannot encode the denied directory's name")
			}
			// This handle, opened before the denial, keeps the right to undo it.
			h, err := windows.CreateFile(name, windows.READ_CONTROL|windows.WRITE_DAC, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
				nil, windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
			if err != nil {
				t.Fatal("cannot keep a handle that restores the access control list")
			}
			t.Cleanup(func() {
				if windows.SetKernelObjectSecurity(h, windows.DACL_SECURITY_INFORMATION, previous) != nil {
					t.Error("cannot restore the directory's access control list")
				}
				_ = windows.CloseHandle(h)
			})
			// The owner loses the right to read the security descriptor, which
			// the barrier's hold asks for.
			denying, err := windows.SecurityDescriptorFromString("D:P(D;;RC;;;OW)(A;;FA;;;SY)(A;;FA;;;" + currentTestSID(t).String() + ")")
			if err != nil {
				t.Fatal("cannot build the denying access control list")
			}
			if windows.SetKernelObjectSecurity(h, windows.DACL_SECURITY_INFORMATION, denying) != nil {
				t.Fatal("cannot install the denying access control list")
			}
			refusal := errors.New("not refused")
			if where == "root" {
				_, refusal = runtimeconfig.WindowsPrivateHoldPath(c.cfg.Root)
			} else if root, holdErr := runtimeconfig.WindowsPrivateHoldPath(c.cfg.Root); holdErr == nil {
				_, refusal = runtimeconfig.WindowsPrivateHold(root, "journal", true)
				_ = root.Close()
			}
			if !errors.Is(refusal, windows.ERROR_ACCESS_DENIED) {
				t.Fatal("the access control list did not deny the directory hold")
			}
			file, flushes := syncFile, 0
			t.Cleanup(func() { syncFile = file })
			syncFile = func(f *os.File) error {
				flushes++
				return file(f)
			}
			if c.syncCheckpointJournalPrefix(t.Context(), progress) != nil {
				t.Fatal("the barrier refused a directory whose hold was denied")
			}
			if flushes != 0 {
				t.Fatal("the barrier flushed without its directory holds")
			}
		})
	}
}

// The tolerance covers the barrier's three opens only: an access denial from
// the flush itself, after every open succeeded, is a failure.
func TestWindowsJournalBarrierFlushDenied(t *testing.T) {
	f, _ := barrierFixture(t, "create")
	progress, err := f.cell.readJournal(t.Context())
	if err != nil || progress.readFile == nil {
		t.Fatal("cannot observe the journal before the flush is denied")
	}
	file, flushes := syncFile, 0
	t.Cleanup(func() { syncFile = file })
	syncFile = func(*os.File) error {
		flushes++
		return windows.ERROR_ACCESS_DENIED
	}
	if !errors.Is(f.cell.syncCheckpointJournalPrefix(t.Context(), progress), errCheckpointJournalIO) {
		t.Fatal("the barrier tolerated a flush that was denied after its opens succeeded")
	}
	if flushes != 1 {
		t.Fatal("the barrier did not reach its flush exactly once")
	}
}
