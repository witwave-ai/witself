package localcell

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/witwave-ai/witself/internal/id"
)

const (
	drillReserveClone     = "clone"               // the clone's tree inside the drill directory
	drillReserveAllowance = "allowance"           // the runtime allowance, created and allocated last
	drillReservePending   = ".pending-drill.json" // the record's one temporary, renamed to drill.json
	drillExtentLast       = 0x1                   // FIEMAP_EXTENT_LAST, linux/fiemap.h
	drillExtentUnwritten  = 0x800                 // FIEMAP_EXTENT_UNWRITTEN, linux/fiemap.h
)

// drillCleanupError: a reservation failed after its directory existed and the
// exact-identifier deletion did not remove it; the drill stays visible and the
// space its files were allocated stays charged to the volume.
type drillCleanupError struct{ cause drillStorageError }

func (drillCleanupError) Error() string   { return "drill storage refused; cleanup incomplete" }
func (e drillCleanupError) Unwrap() error { return e.cause }

// The reservation's seams; only tests replace them.
var (
	drillReserveNewID    = func() (string, error) { return id.New("drill") }
	drillReserveAllocate = drillPlatformAllocate
	drillReserveChunk    = uint64(64 << 20)
)

// drillExtent is one mapped range of a file: its logical start, length and flags.
type drillExtent struct {
	logical, length uint64
	flags           uint32
}

// drillExtentsCover reports whether extents, in logical order, cover [0, size)
// with no gap and no flag but last and unwritten.
func drillExtentsCover(extents []drillExtent, size uint64) bool {
	next := uint64(0)
	for _, extent := range extents {
		if next >= size {
			break
		}
		if extent.logical != next || extent.flags&^(drillExtentLast|drillExtentUnwritten) != 0 {
			return false
		}
		next = drillStorageAdd(next, extent.length)
	}
	return next >= size
}

// drillWindowsRefused are the attributes that keep an end-of-file allocation
// from proving its range: directory 0x10, sparse 0x200, reparse point 0x400,
// compressed 0x800, offline 0x1000, encrypted 0x4000, integrity stream 0x8000,
// recall on data access 0x400000 (a placeholder whose data is elsewhere).
const drillWindowsRefused = 0x10 | 0x200 | 0x400 | 0x800 | 0x1000 | 0x4000 | 0x8000 | 0x400000

// drillWindowsProved: a file exactly size long whose allocation covers its end
// and that carries no refused attribute.
func drillWindowsProved(allocation, end int64, attributes uint32, size uint64) bool {
	return end >= 0 && uint64(end) == size && allocation >= end && attributes&drillWindowsRefused == 0
}

// drillWindowsInherits: a directory carrying compression 0x800 or encryption
// 0x4000, refused attributes a new file inherits from its directory, so no
// allocation below it could be proved.
func drillWindowsInherits(attributes uint32) bool {
	return attributes&(0x800|0x4000) != 0
}

// drillReservePathsFit applies the Windows path bound to the drill layout; the
// clone directory's bound covers the shorter record temporary and allowance.
func drillReservePathsFit(boundaryDir string, entries []checkpointInventoryEntry) bool {
	sample := filepath.Join(boundaryDir, "drill_"+strings.Repeat("a", 16))
	fits := checkpointPathFits(filepath.Join(sample, drillReserveClone), true)
	for _, entry := range entries {
		fits = fits && checkpointPathFits(filepath.Join(sample, drillReserveClone, filepath.FromSlash(entry.Path)), entry.Type == "dir")
	}
	return fits
}

// drillReserveReason is the closed reason a create, write or allocation error
// carries: a closed refusal as itself, an existing name, the platform's lack of
// space or of support, else otherwise.
func drillReserveReason(err error, otherwise string) string {
	var refused drillStorageError
	switch platform := drillPlatformRefusal(err); {
	case errors.As(err, &refused):
		return refused.reason
	case errors.Is(err, fs.ErrExist):
		return "allocation_collision"
	case platform != "":
		return platform
	}
	return otherwise
}

// drillReserveSame reports whether f, still open, and info read through a
// name are one file: an identity compared while f holds the file cannot be
// matched by a freed and reused file number. A pathname FileInfo's identity is
// read at this comparison (Windows resolves it lazily), as the deletion's bind.
func drillReserveSame(f *os.File, info os.FileInfo, err error) bool {
	mine, statErr := f.Stat()
	return err == nil && statErr == nil && os.SameFile(mine, info)
}

// drillReserveRoot opens name below parent as a root and proves that the root
// is the directory object the no-follow walk reached at that name: the anchor,
// opened through the walk, and the root are compared while both are open. That
// object renamed between the two opens and reached again through an in-root
// link is accepted, being the same object; any other is boundary_refused, and
// so is the proved directory failing to open as a root (that error would carry
// the name). The anchor serves identity only; a parent renamed meanwhile is
// followed as its directory object.
func drillReserveRoot(parent *os.Root, name string) (*os.Root, error) {
	anchor, err := drillPlatformOpen(parent, name, true)
	if err != nil {
		return nil, err
	}
	defer func() { _ = anchor.Close() }()
	root, err := parent.OpenRoot(filepath.FromSlash(name) + drillRootHere)
	if err != nil {
		return nil, drillStorageError{reason: "boundary_refused"}
	}
	info, err := root.Stat(".")
	if !drillReserveSame(anchor, info, err) {
		_ = root.Close()
		return nil, drillStorageError{reason: "boundary_refused"}
	}
	return root, nil
}

// reserveDrillLocked allocates, through the file system, the storage of one
// restore drill of one kept checkpoint and returns the drill's identifier. It
// needs the caller's operation lock and maintenance fence. After admission
// (unchanged) it holds recovery/ as a directory only, re-identifies the volume
// through that handle, refuses a recovery/ whose new files would inherit an
// attribute the proof refuses, re-reads the checkpoint's documents, makes the
// drill directory, publishes the record first (state reserved), then creates
// clone/ with every inventory entry and allowance last, each name created or
// opened through the platform's no-follow walk, allocating each file in chunks
// with the floor re-checked before each chunk and proving its allocation on a
// reopened handle after the writer's close; it then syncs every created
// directory, deepest first. The proof shows the space allocated by the file
// system and claims nothing about the device below it. On any failure after
// the directory exists it discards the drill by exact-identifier deletion;
// when that fails the error is drillCleanupError and the identifier is
// returned. A crash leaves the drill visible and charged until that deletion.
// No journal entry is written and the checkpoint storage rule is unchanged.
func (c *Cell) reserveDrillLocked(ctx context.Context, checkpointID, operationID string, allowance uint64) (string, error) {
	if !drillValidOperationID(operationID) {
		return "", drillStorageError{reason: "identity_invalid"}
	}
	estimate, err := c.admitDrillStorageLocked(ctx, checkpointID, allowance)
	if err != nil {
		return "", err
	}
	boundary, err := os.OpenRoot(c.recoveryDir() + drillRootHere)
	if err != nil {
		return "", drillStorageError{reason: "boundary_refused"}
	}
	held, err := boundary.Open(".")
	if err != nil {
		_ = boundary.Close()
		return "", drillStorageError{reason: "boundary_refused"}
	}
	var drill *os.Root
	release := func() {
		if drill != nil {
			_ = drill.Close()
			drill = nil
		}
		if held != nil {
			_ = held.Close()
			_ = boundary.Close()
			held = nil
		}
	}
	defer release()
	// recovery/ itself, not a link's target, before anything is written: the held
	// boundary is bound to the name read without following its last element.
	if named, err := os.Lstat(c.recoveryDir()); !drillReserveSame(held, named, err) {
		return "", drillStorageError{reason: "boundary_refused"}
	}
	volume, err := drillStorageVolume(held)
	if err != nil || volume.unit != estimate.unit {
		return "", drillStorageError{reason: drillReserveReason(err, "boundary_refused")}
	}
	// A new file inherits compression and encryption from its directory and the
	// proof refuses both: refused here with the proof's reason, before any write.
	if drillPlatformInherits(held) {
		return "", drillStorageError{reason: "allocation_unproved"}
	}
	if !drillStorageAdmits(volume.free, estimate.need) {
		return "", drillStorageError{reason: "storage_insufficient"}
	}
	manifest, err := readManifest(c.cfg.Root)
	if err != nil {
		return "", drillStorageError{reason: "boundary_refused"}
	}
	dir, err := c.checkpointDir(checkpointID)
	if err != nil {
		return "", drillStorageError{reason: "checkpoint_absent"}
	}
	budget := checkpointRetryBudget{limit: checkpointRetryFloor}
	witness, inventory, err := verifyCheckpointDocuments(ctx, &budget, c.checkpointOps(), dir, checkpointID)
	if ctx.Err() != nil {
		return "", drillStorageError{reason: "cancelled"}
	}
	if err != nil || checkpointVerifyShape(inventory, witness, &budget) != nil || witness.InstallationID != manifest.InstallationID ||
		drillCloneBytes(inventory.Entries, volume.unit) != estimate.clone {
		return "", drillStorageError{reason: "checkpoint_unverified"}
	}
	if runtime.GOOS == "windows" && !drillReservePathsFit(c.recoveryDir(), inventory.Entries) {
		return "", drillStorageError{reason: "path_too_long"}
	}
	drillID, err := drillReserveNewID()
	if err != nil || !drillValidID(drillID) {
		return "", drillStorageError{reason: "identity_invalid"}
	}
	record := drillRecord{Format: drillFormat, DrillID: drillID, OperationID: operationID, State: "reserved", Binding: drillBinding{
		InstallationID: manifest.InstallationID, CheckpointID: checkpointID, InventorySHA256: witness.InventorySHA256, LayoutVersion: witness.LayoutVersion,
		PrincipalLayoutVersion: witness.PrincipalLayoutVersion, SchemaVersion: witness.SchemaVersion, PostgresMajor: witness.PostgresMajor}}
	data, err := json.Marshal(record)
	if err != nil {
		return "", drillStorageError{reason: "checkpoint_unverified"}
	}
	if _, err := drillParseRecord(data); err != nil {
		return "", drillStorageError{reason: "checkpoint_unverified"}
	}
	c.drillRootStage("reserve_mkdir")
	if err := drillPlatformMkdir(c, boundary, nil, drillID, ""); err != nil {
		return "", drillStorageError{reason: drillReserveReason(err, "boundary_refused")}
	}
	// From here every failure discards the drill by its exact identifier.
	fail := func(reason string) (string, error) {
		release()
		if c.deleteDrillLocked(drillID) != nil {
			return drillID, drillCleanupError{cause: drillStorageError{reason: reason}}
		}
		return "", drillStorageError{reason: reason}
	}
	if drillRootSync(boundary) != nil {
		return fail("boundary_refused")
	}
	if drill, err = drillReserveRoot(boundary, drillID); err != nil {
		return fail("boundary_refused")
	}
	c.drillRootStage("reserve_record")
	if reason := c.drillReserveRecord(boundary, drill, drillID, data); reason != "" {
		return fail(reason)
	}
	if drillRootSync(drill) != nil {
		return fail("boundary_refused")
	}
	c.drillRootStage("reserve_tree")
	created := []string{drillReserveClone}
	if err := drillPlatformMkdir(c, boundary, drill, drillID, drillReserveClone); err != nil {
		return fail(drillReserveReason(err, "boundary_refused"))
	}
	allocated := uint64(0)
	for _, entry := range inventory.Entries {
		name := path.Join(drillReserveClone, entry.Path)
		if entry.Type == "dir" {
			if err := drillPlatformMkdir(c, boundary, drill, drillID, name); err != nil {
				return fail(drillReserveReason(err, "boundary_refused"))
			}
			created = append(created, name)
		} else if reason := c.drillReserveFile(ctx, boundary, drill, held, drillID, name, uint64(*entry.Size), estimate.need, &allocated); reason != "" {
			return fail(reason)
		}
	}
	c.drillRootStage("reserve_allowance")
	if reason := c.drillReserveFile(ctx, boundary, drill, held, drillID, drillReserveAllowance, estimate.allowance, estimate.need, &allocated); reason != "" {
		return fail(reason)
	}
	c.drillRootStage("reserve_sync")
	for index := len(created) - 1; index >= 0; index-- {
		sub, err := drillReserveRoot(drill, created[index])
		if err != nil {
			return fail("boundary_refused")
		}
		if err = errors.Join(drillRootSync(sub), sub.Close()); err != nil {
			return fail("boundary_refused")
		}
	}
	if drillRootSync(drill) != nil {
		return fail("boundary_refused")
	}
	c.drillRootStage("reserve_done")
	return drillID, nil
}

// drillReserveRecord publishes the record first: an exclusive private
// temporary is written and synced, renamed to drill.json while the writer is
// still open, and the published name proved to be the writer's file. On a
// failure the temporary is removed only once proved to be the writer's.
func (c *Cell) drillReserveRecord(boundary, drill *os.Root, drillID string, data []byte) string {
	f, err := drillPlatformCreate(c, boundary, drill, drillID, drillReservePending)
	if err != nil {
		return drillReserveReason(err, "boundary_refused")
	}
	defer func() { _ = f.Close() }()
	_, err = f.Write(data)
	if err = errors.Join(err, f.Sync()); err == nil {
		c.drillRootStage("reserve_publish")
		err = drill.Rename(drillReservePending, drillRootRecordName)
	}
	if err != nil {
		if info, statErr := drill.Lstat(drillReservePending); drillReserveSame(f, info, statErr) {
			_ = drill.Remove(drillReservePending)
		}
		return drillReserveReason(err, "boundary_refused")
	}
	if info, err := drill.Lstat(drillRootRecordName); !drillReserveSame(f, info, err) || f.Close() != nil {
		return "boundary_refused"
	}
	return ""
}

// drillReserveFile creates one file exclusively, allocates [0, size) in chunks
// with the floor re-checked before each and syncs it; it reopens the name
// through the no-follow walk, proves the reopened file is the writer's while
// the writer is open, closes the writer and proves the allocation on the
// reopened handle; it returns a closed reason or "". Each chunk starts where
// the last one ended, so no allocation asks for an end below the current one.
func (c *Cell) drillReserveFile(ctx context.Context, boundary, drill *os.Root, held *os.File, drillID, name string, size, need uint64, allocated *uint64) string {
	f, err := drillPlatformCreate(c, boundary, drill, drillID, name)
	if err != nil {
		return drillReserveReason(err, "boundary_refused")
	}
	c.drillRootStage("reserve_file")
	reason := ""
	for offset := uint64(0); offset < size && reason == ""; {
		c.drillRootStage("reserve_chunk")
		volume, err := drillStorageVolume(held)
		switch length := min(drillReserveChunk, size-offset); {
		case ctx.Err() != nil:
			reason = "cancelled"
		case err != nil:
			reason = drillReserveReason(err, "boundary_refused")
		case !drillStorageAdmits(volume.free, need-min(need, *allocated)):
			reason = "storage_insufficient"
		default:
			if err := drillReserveAllocate(f, offset, length); err != nil {
				reason = drillReserveReason(err, "allocation_unproved")
			}
			offset, *allocated = offset+length, drillStorageAdd(*allocated, length)
		}
	}
	if reason == "" && f.Sync() != nil {
		reason = "allocation_unproved"
	}
	if reason != "" {
		_ = f.Close()
		return reason
	}
	c.drillRootStage("reserve_verify")
	reopened, err := drillPlatformOpen(drill, name, false)
	if err != nil {
		_ = f.Close()
		return "allocation_unproved"
	}
	defer func() { _ = reopened.Close() }()
	// The writer still holds the file while the identities are compared; the
	// proof runs after its close, on the reopened handle.
	info, err := reopened.Stat()
	same := drillReserveSame(f, info, err)
	if closed := f.Close(); !same || closed != nil || drillPlatformVerify(reopened, size) != nil {
		return "allocation_unproved"
	}
	return ""
}
