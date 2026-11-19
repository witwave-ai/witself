//go:build linux || windows

package localcell

import (
	"context"
	"errors"
	"math"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"testing"
)

// drillReserveRun is one reservation row: a cell with one sealed checkpoint whose lock it holds, on the file system the rows
// need (R13), its stages counted, the row's action at a stage's first call, and the allocator fenced at 4 MiB (R7) and
// required to start each chunk where its file's last one ended, so no end ever decreases.
type drillReserveRun struct {
	*checkpointVerifyCase
	ctx       context.Context
	operation string
	allowance uint64
	before    map[string]retainedEntry
	stages    map[string]int
	on        map[string]func()
	allocate  func(f *os.File, offset, length uint64) error
	ends      map[*os.File]uint64
	allocated uint64
}

func drillReserveStart(t *testing.T) *drillReserveRun {
	t.Helper()
	v := newCheckpointVerifyCase(t, false)
	_, err := drillStorageOpened(t, v.c.recoveryDir())
	drillAssert(t, err == nil, map[bool]string{false: "the reservation rows need an ext4 temporary directory",
		true: "the reservation rows need an NTFS temporary directory"}[runtime.GOOS == "windows"])
	r := &drillReserveRun{checkpointVerifyCase: v, ctx: t.Context(), operation: "op_reserve", allowance: 200 << 10, on: map[string]func(){},
		allocate: drillReserveAllocate, ends: map[*os.File]uint64{}}
	allocate, chunk, mint := drillReserveAllocate, drillReserveChunk, drillReserveNewID
	t.Cleanup(func() { drillReserveAllocate, drillReserveChunk, drillReserveNewID = allocate, chunk, mint })
	drillReserveChunk = 64 << 10
	drillReserveAllocate = func(f *os.File, offset, length uint64) error {
		drillAssert(t, offset == r.ends[f] && length > 0 && r.allocated+length <= 4<<20, "an allocation did not start at its file's last end or passed the fence")
		r.ends[f], r.allocated = offset+length, r.allocated+length
		return r.allocate(f, offset, length)
	}
	r.stages = drillRootStages(v.c, func(stage string, call int) {
		if call == 1 && r.on[stage] != nil {
			r.on[stage]()
		}
	})
	return r
}

// reserve runs the row's reservation; the tree before is read at the call.
func (r *drillReserveRun) reserve() (string, error) {
	r.before = checkpointTreeState(r.t, r.c)
	return r.c.reserveDrillLocked(r.ctx, r.id, r.operation, r.allowance)
}

// fixed makes the identifier seam return one identifier and returns that drill directory's path.
func (r *drillReserveRun) fixed() string {
	drillReserveNewID = func() (string, error) { return "drill_aaaaaaaaaaaaaaaa", nil }
	return filepath.Join(r.c.recoveryDir(), "drill_aaaaaaaaaaaaaaaa")
}

// file is the inventory's first file entry, the first one the reservation allocates.
func (r *drillReserveRun) file() *checkpointInventoryEntry {
	index := slices.IndexFunc(r.inventory.Entries, func(entry checkpointInventoryEntry) bool { return entry.Type == "file" })
	drillAssert(r.t, index >= 0 && *r.inventory.Entries[index].Size > 0, "the reservation fixture holds no file with content")
	return &r.inventory.Entries[index]
}

// volume fakes the volume seam: probe answers each call by its number, admission's being the first.
func (r *drillReserveRun) volume(probe func(call int) drillVolume) {
	calls := 0
	drillStorageSeam(r.t, &drillStorageVolume, func(*os.File) (drillVolume, error) { calls++; return probe(calls), nil })
}

// short fakes 4 KiB units whose free space covers the need and floor exactly up to call after, one byte less from then on.
func (r *drillReserveRun) short(after int) {
	need := drillStorageNeed(r.checkpointVerifyCase, 4096, r.allowance).need
	r.volume(func(call int) drillVolume {
		return drillVolume{unit: 4096, free: need + drillStorageFloor - map[bool]uint64{true: 1}[call > after]}
	})
}

// second fakes a volume with all space free that runs change at its second call, the reservation's own probe.
func (r *drillReserveRun) second(change func()) {
	r.volume(func(call int) drillVolume {
		if call == 2 {
			change()
			r.before = checkpointTreeState(r.t, r.c)
		}
		return drillVolume{unit: 4096, free: math.MaxUint64}
	})
}

// fault makes every allocation from the allowance's on fail with err.
func (r *drillReserveRun) fault(err error) {
	allocate := r.allocate
	r.allocate = func(f *os.File, offset, length uint64) error {
		if r.stages["reserve_allowance"] > 0 {
			return err
		}
		return allocate(f, offset, length)
	}
}

// refused requires the closed reason want, no identifier, the tree as before the call and no drill listed; made
// says the drill directory was made and then discarded, else nothing was written.
func (r *drillReserveRun) refused(drillID string, err error, want string, made bool) {
	r.t.Helper()
	listing, listErr := r.c.drillRootList()
	drillStorageReason(r.t, err, want, "the reservation gave another outcome")
	drillAssert(r.t, drillID == "" && r.stages["reserve_mkdir"] == map[bool]int{true: 1}[made] && (r.stages["directory"] > 0) == made,
		"the reservation returned an identifier, or wrote or discarded other than its row says")
	drillAssert(r.t, listErr == nil && reflect.DeepEqual(listing, drillRootListing{}) && reflect.DeepEqual(r.before, checkpointTreeState(r.t, r.c)),
		"the reservation left a drill listed or the tree changed")
}

// The reservation on the runner's ext4 or NTFS; a row allocates at most 1 MiB, lack of space only injected.
func TestDrillReservation(t *testing.T) {
	t.Run("reserved", func(t *testing.T) {
		r := drillReserveStart(t)
		drillID, err := r.reserve()
		drillAssert(t, err == nil && drillValidID(drillID), "the reservation did not return a valid identifier")
		listing, err := r.c.drillRootList()
		drillAssert(t, err == nil && slices.Equal(listing.declared, []string{drillID}) && listing.remainder == nil && listing.foreign+listing.unknown == 0,
			"the listing does not declare exactly the reserved drill")
		drill, err := os.OpenRoot(filepath.Join(r.c.recoveryDir(), drillID))
		drillAssert(t, err == nil, "cannot hold the reserved drill directory")
		t.Cleanup(func() { _ = drill.Close() })
		manifest, err := readManifest(r.c.cfg.Root)
		drillAssert(t, err == nil, "cannot read the reservation fixture's manifest")
		kind, record := drillRootHeld(drill, drillID, manifest.InstallationID)
		w := r.witness
		drillAssert(t, kind == "declared" && record.State == "reserved" && record.OperationID == r.operation && record.Binding == drillBinding{
			InstallationID: manifest.InstallationID, CheckpointID: r.id, InventorySHA256: w.InventorySHA256, LayoutVersion: w.LayoutVersion,
			PrincipalLayoutVersion: w.PrincipalLayoutVersion, SchemaVersion: w.SchemaVersion, PostgresMajor: w.PostgresMajor}, "the record does not declare this reservation")
		// Every inventory entry below clone/, and the allowance beside it.
		chunks, allowance := 0, int64(200<<10)
		for _, entry := range append(slices.Clone(r.inventory.Entries), checkpointInventoryEntry{Path: "../" + drillReserveAllowance, Type: "file", Size: &allowance}) {
			f, err := drillPlatformOpen(drill, path.Join(drillReserveClone, entry.Path), entry.Type == "dir")
			drillAssert(t, err == nil && (entry.Type == "dir" || drillPlatformVerify(f, uint64(*entry.Size)) == nil) && f.Close() == nil,
				"a reserved directory is absent or a reserved file is not proved at its size")
			if entry.Type == "file" {
				chunks += int((*entry.Size + 64<<10 - 1) / (64 << 10))
			}
		}
		drillAssert(t, r.stages["reserve_chunk"] == chunks, "the reservation did not allocate each file in 64 KiB chunks")
		_, err = r.c.admitDrillStorageLocked(t.Context(), r.id, 1)
		drillStorageReason(t, err, "drill_outstanding", "admission did not see the reserved drill")
		drillAssert(t, r.c.validateRecovery() == nil, "the reserved tree is not private")
		drillAssert(t, drill.Close() == nil && r.c.deleteDrillLocked(drillID) == nil && reflect.DeepEqual(r.before, checkpointTreeState(t, r.c)),
			"the reserved drill was not deleted back to the tree before")
	})
	t.Run("synced", func(t *testing.T) {
		r, syncs, directories := drillReserveStart(t), 0, 0
		sync := drillRootSync
		t.Cleanup(func() { drillRootSync = sync })
		drillRootSync = func(root *os.Root) error { syncs++; return sync(root) }
		drillID, err := r.reserve()
		for _, entry := range r.inventory.Entries {
			directories += map[bool]int{true: 1}[entry.Type == "dir"]
		}
		drillAssert(t, err == nil && syncs == 2+directories+1+1, "the reservation did not sync the recovery directory, the drill directory twice and each directory it made")
		drillAssert(t, r.c.deleteDrillLocked(drillID) == nil, "the synced drill was not deleted")
	})
	t.Run("floor_exact", func(t *testing.T) {
		r := drillReserveStart(t)
		need := drillStorageNeed(r.checkpointVerifyCase, 4096, r.allowance).need
		r.volume(func(int) drillVolume { return drillVolume{unit: 4096, free: need - r.allocated + drillStorageFloor} })
		drillID, err := r.reserve()
		drillAssert(t, err == nil && r.c.deleteDrillLocked(drillID) == nil && reflect.DeepEqual(r.before, checkpointTreeState(t, r.c)),
			"the reservation refused exactly its remaining need and floor")
	})
	for _, row := range []struct {
		name, want string
		made       bool
		setup      func(r *drillReserveRun)
	}{
		{"floor_crossed", "storage_insufficient", true, func(r *drillReserveRun) { r.short(2) }},
		{"reidentified", "boundary_refused", false, func(r *drillReserveRun) {
			r.volume(func(call int) drillVolume {
				return drillVolume{unit: map[bool]uint64{false: 4096, true: 65536}[call > 1], free: math.MaxUint64}
			})
		}},
		{"free_dropped", "storage_insufficient", false, func(r *drillReserveRun) { r.short(1) }},
		{"documents_changed", "checkpoint_unverified", false, func(r *drillReserveRun) {
			r.second(func() {
				entry := r.file()
				size := *entry.Size + 8192
				entry.Size, r.witness.Bytes = &size, r.witness.Bytes+8192
				r.inventorySeal()
			})
		}},
		{"foreign_witness", "checkpoint_unverified", false, func(r *drillReserveRun) {
			r.second(func() { r.witness.InstallationID = "lc_other"; r.document(checkpointWitnessName, r.witness) })
		}},
		{"binding_invalid", "checkpoint_unverified", false, func(r *drillReserveRun) {
			r.witness.PostgresMajor = "15"
			r.document(checkpointWitnessName, r.witness)
		}},
		{"identity_minted", "identity_invalid", false, func(*drillReserveRun) {
			drillReserveNewID = func() (string, error) { return "drill_AAAAAAAAAAAAAAAA", nil }
		}},
		{"operation_invalid", "identity_invalid", false, func(r *drillReserveRun) { // a torn journal tail would refuse any later check
			r.operation = "op invalid"
			drillStorageJournal(r.checkpointVerifyCase, `{"operation":"stop"`)
		}},
		{"admission_refusal", "unsupported_fs", false, func(r *drillReserveRun) {
			drillStorageSeam(r.t, &drillStorageVolume, func(*os.File) (drillVolume, error) { return drillVolume{}, drillStorageError{reason: "unsupported_fs"} })
		}},
		{"exhausted", "storage_exhausted", true, func(r *drillReserveRun) { r.fault(drillStorageError{reason: "storage_exhausted"}) }},
		{"exhausted_raw", "storage_exhausted", true, func(r *drillReserveRun) { r.fault(drillReserveFull) }},
		{"sparse", "allocation_unproved", true, func(r *drillReserveRun) { r.allocate = drillReserveSparse }},
		{"cancelled", "cancelled", true, func(r *drillReserveRun) {
			ctx, cancel := context.WithCancel(r.ctx)
			r.t.Cleanup(cancel)
			r.ctx, r.on["reserve_chunk"] = ctx, cancel
		}},
		// R5: on Windows a not-exist error at the reopen is a refusal like any other.
		{"verify/vanished", "allocation_unproved", true, func(r *drillReserveRun) {
			name := filepath.Join(r.fixed(), drillReserveClone, filepath.FromSlash(r.file().Path))
			r.on["reserve_verify"] = func() { drillAssert(r.t, os.Remove(name) == nil, "cannot remove the file under proof") }
		}},
	} {
		t.Run(row.name, func(t *testing.T) {
			r := drillReserveStart(t)
			row.setup(r)
			drillID, err := r.reserve()
			r.refused(drillID, err, row.want, row.made)
			drillAssert(t, row.name != "floor_crossed" || r.allocated == 0, "a chunk was allocated below the floor")
		})
	}
	t.Run("collision", func(t *testing.T) {
		r := drillReserveStart(t)
		dir := r.fixed()
		r.on["reserve_mkdir"] = func() { drillAssert(t, privateDir(dir, true) == nil, "cannot plant the colliding drill directory") }
		drillID, err := r.reserve()
		listing, listErr := r.c.drillRootList()
		drillStorageReason(t, err, "allocation_collision", "a colliding drill directory was not refused as a collision")
		drillAssert(t, drillID == "" && r.stages["open"] == 0 && listErr == nil && slices.Equal(listing.remainder, []string{filepath.Base(dir)}) && listing.declared == nil,
			"the colliding directory was discarded or changed")
	})
	t.Run("cleanup_incomplete", func(t *testing.T) {
		r, failing := drillReserveStart(t), true
		r.fault(drillStorageError{reason: "storage_exhausted"})
		sync := drillRootSync
		t.Cleanup(func() { drillRootSync = sync })
		drillRootSync = func(root *os.Root) error {
			if failing && r.stages["sweep"] > 0 {
				return errors.New("drill reserve fixture sync refused")
			}
			return sync(root)
		}
		drillID, err := r.reserve()
		var cleanup drillCleanupError
		listing, listErr := r.c.drillRootList()
		drillAssert(t, errors.As(err, &cleanup) && cleanup.cause == drillStorageError{reason: "storage_exhausted"} && err.Error() == "drill storage refused; cleanup incomplete",
			"a failed discard was not an incomplete cleanup of the exhausted reservation")
		drillAssert(t, drillValidID(drillID) && listErr == nil && slices.Equal(listing.declared, []string{drillID}), "the undiscarded drill was not returned and declared")
		failing = false
		drillAssert(t, r.c.deleteDrillLocked(drillID) == nil && reflect.DeepEqual(r.before, checkpointTreeState(t, r.c)), "the undiscarded drill was not deleted back to the tree before")
	})
	t.Run("publish_refused", func(t *testing.T) {
		r := drillReserveStart(t)
		dir := r.fixed()
		r.on["reserve_publish"] = func() {
			drillAssert(t, privateDir(filepath.Join(dir, drillRootRecordName), true) == nil, "cannot plant a directory at the record's name")
		}
		drillID, err := r.reserve()
		var cleanup drillCleanupError
		entries, listErr := os.ReadDir(dir)
		// os.Root.Rename refuses an existing directory at the new name with EEXIST before any rename (os/root_openat.go).
		drillAssert(t, errors.As(err, &cleanup) && cleanup.cause == drillStorageError{reason: "allocation_collision"} && drillID == filepath.Base(dir),
			"a refused publication was not an incomplete cleanup of a collision")
		drillAssert(t, listErr == nil && len(entries) == 1 && entries[0].Name() == drillRootRecordName && entries[0].IsDir(), "the record's temporary was left or the planted directory not kept")
	})
}
