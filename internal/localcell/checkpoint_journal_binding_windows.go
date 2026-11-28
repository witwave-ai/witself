//go:build windows

package localcell

import (
	"cmp"
	"context"
	"errors"
	"io"
	"os"
	"strings"

	"github.com/witwave-ai/witself/internal/runtimeconfig"
	"golang.org/x/sys/windows"
)

// A held object and the name in the cell that must still open it.
type windowsJournalHold struct {
	path      string
	file      *os.File
	directory bool
}

// windowsJournalBound proves by handle that the root's final path is the
// configured root (in any letter case, as the released verbs admit it), that
// every name still opens its held object, and that each held object is private
// (owner, exact protected DACL, type, no reparse point), on the root's volume
// and, for a file, has one link and no pending deletion. A privacy, volume or
// link refusal is the private layout's; every other one is the journal's.
func (c *Cell) windowsJournalBound(holds []windowsJournalHold) error {
	final := make([]uint16, windows.MAX_LONG_PATH)
	n, err := windows.GetFinalPathNameByHandle(windows.Handle(holds[0].file.Fd()), &final[0], uint32(len(final)), 0)
	if err != nil || n >= uint32(len(final)) || !strings.EqualFold(strings.TrimPrefix(windows.UTF16ToString(final[:n]), `\\?\`), c.cfg.Root) {
		return errCheckpointJournalIO
	}
	var volume uint32
	for i, h := range holds {
		held, err := runtimeconfig.WindowsPrivateIdentity(h.file)
		named, nameErr := runtimeconfig.WindowsPrivateNameIdentity(h.path)
		if err != nil || nameErr != nil || named.Volume != held.Volume || named.Index != held.Index {
			return errCheckpointJournalIO
		}
		if i == 0 {
			volume = held.Volume
		}
		if held.Volume != volume || runtimeconfig.WindowsPrivateValidateFile(h.file, h.directory) != nil {
			return errPrivateLayout
		}
		if !h.directory && (held.Links != 1 || held.DeletePending) {
			return errPrivateLayout
		}
	}
	return nil
}

// repairOwnedJournal holds the root by path, then journal/ and the journal
// relative to their held parents, never by name and never following a
// reparse point. The journal is held for reading and writing sharing read
// only, before the parse and until settlement closes it: another writer,
// appender or deleter refuses it, and none can open it, rename or replace it
// meanwhile. The directories are not flushed: a directory flush needs its own
// handle with the right to add files and is not built (ruling D1 of slice 1W),
// and the shared directory step returns nil here.
func (c *Cell) repairOwnedJournal(ctx context.Context, owner *os.File, root *os.Root) (CheckpointJournalStatus, error) {
	rootHold, err := runtimeconfig.WindowsPrivateHoldPath(c.cfg.Root)
	if err != nil {
		return "", errCheckpointJournalIO
	}
	defer func() { _ = rootHold.Close() }()
	holds := []windowsJournalHold{{c.cfg.Root, rootHold, true}, {c.path(".owner.lock"), owner, false}}
	c.journalSettlementBoundary("hold", nil)
	// Only a name its bound, held parent does not contain is absent.
	absent := func() (CheckpointJournalStatus, error) {
		if err := errors.Join(c.windowsJournalBound(holds), ctx.Err()); err != nil {
			return "", err
		}
		return CheckpointJournalAbsent, nil
	}
	dirHold, err := runtimeconfig.WindowsPrivateHold(rootHold, "journal", true)
	if errors.Is(err, windows.ERROR_FILE_NOT_FOUND) {
		return absent()
	}
	if err != nil {
		return "", errCheckpointJournalIO
	}
	defer func() { _ = dirHold.Close() }()
	holds = append(holds, windowsJournalHold{c.path("journal"), dirHold, true})
	// Nothing below a held reparse point resolves, a present name no more than
	// a missing one (ERROR_CANT_RESOLVE_FILENAME or ERROR_UNTRUSTED_MOUNT_POINT,
	// by the caller's token; never "not found"), so journal/ is proven before
	// anything is opened below it: a reparse journal/ is the private layout's.
	if bindErr := c.windowsJournalBound(holds); bindErr != nil {
		return "", bindErr
	}
	const name = "operations.jsonl"
	f, err := runtimeconfig.WindowsPrivateHold(dirHold, name, false)
	if errors.Is(err, windows.ERROR_FILE_NOT_FOUND) {
		return absent()
	}
	if err != nil {
		return "", errCheckpointJournalIO
	}
	holds = append(holds, windowsJournalHold{c.path("journal", name), f, false})
	before, identityErr := runtimeconfig.WindowsPrivateIdentity(f)
	if err := c.windowsJournalBound(holds); err != nil || identityErr != nil {
		_ = f.Close()
		return "", cmp.Or(err, errCheckpointJournalIO)
	}
	// The shared settlement takes journal/ as a root only for its directory
	// sync, which does nothing here; the bound hold fences the name it opens.
	dir, err := root.OpenRoot("journal" + checkpointRootHere)
	if err != nil {
		_ = f.Close()
		return "", errCheckpointJournalIO
	}
	defer func() { _ = dir.Close() }()
	// The parser has consumed EOF on this handle. Its position, end of file,
	// change time and last-write time must still cover exactly that extent.
	revalidate := func() error {
		if err := c.windowsJournalBound(holds); err != nil {
			return err
		}
		now, err := runtimeconfig.WindowsPrivateIdentity(f)
		end, seekErr := f.Seek(0, io.SeekCurrent)
		if err != nil || seekErr != nil || end != before.Size || now.Size != before.Size || now.ChangeTime != before.ChangeTime || now.WriteTime != before.WriteTime {
			return errCheckpointJournalIO
		}
		return nil
	}
	return c.settleCheckpointJournal(ctx, root, dir, f, before.Size, revalidate)
}

// The prefix barrier is not built on Windows yet (checkpointJournalBarrierSupported
// is false here, so nothing calls this).
func (c *Cell) syncCheckpointJournalPrefix(context.Context, journalProgress) error { return nil }
