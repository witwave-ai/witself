//go:build linux || windows

package localcell

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strconv"
	"strings"
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

// exact fakes 4 KiB units whose free space covers the floor and exactly the need left at each probe, one byte less
// while short says so; the need left is recomputed without the production arithmetic (P1): all of it before the first
// file, then less 1 MiB and two units, 256 bytes and a unit per directory and 256 bytes and the size rounded up per file
// before the one being allocated, that file's 256 bytes (the allowance has none) and its 64 KiB chunks so far.
func (r *drillReserveRun) exact(short func() bool) {
	need := drillStorageNeed(r.checkpointVerifyCase, 4096, r.allowance).need
	r.volume(func(int) drillVolume {
		counted, files, raw := uint64(1<<20+2*4096), 0, uint64(0)
		for _, entry := range r.inventory.Entries {
			if entry.Type == "dir" {
				counted += 256 + 4096
			} else if files++; files == r.stages["reserve_file"] {
				counted += 256
				break
			} else {
				counted, raw = counted+256+(uint64(*entry.Size)+4095)/4096*4096, raw+uint64(*entry.Size)
			}
		}
		if counted += r.allocated - raw; r.stages["reserve_file"] == 0 {
			counted = 0
		}
		return drillVolume{unit: 4096, free: need - counted + drillStorageFloor - map[bool]uint64{true: 1}[short()]}
	})
}

// charged is what the file system charges the drill directory dir so far, read through a root held on it: the
// directory, the record, clone/, each inventory entry and the allowance that stand, each opened by the no-follow walk.
func (r *drillReserveRun) charged(dir string) (charged uint64) {
	drill, err := os.OpenRoot(dir)
	if err != nil {
		return 0 // admission's probe and the reservation's own come before the drill directory
	}
	defer func() { _ = drill.Close() }()
	held, err := drill.Open(".")
	drillAssert(r.t, err == nil, "cannot hold the drill directory charged so far")
	charged = drillReserveCharged(r.t, held)
	drillAssert(r.t, held.Close() == nil, "cannot close the drill directory charged so far")
	for _, entry := range append(slices.Clone(r.inventory.Entries), checkpointInventoryEntry{Path: ".", Type: "dir"},
		checkpointInventoryEntry{Path: "../" + drillRootRecordName}, checkpointInventoryEntry{Path: "../" + drillReserveAllowance}) {
		if f, err := drillPlatformOpen(drill, path.Join(drillReserveClone, entry.Path), entry.Type == "dir"); err == nil {
			charged += drillReserveCharged(r.t, f)
			drillAssert(r.t, f.Close() == nil, "cannot close an object of the drill charged so far")
		}
	}
	return charged
}

// unsound reseals the inventory with a zero-size file at each path, kept sorted.
func (r *drillReserveRun) unsound(paths ...string) {
	for _, name := range paths {
		r.inventory.Entries = append(r.inventory.Entries, checkpointInventoryEntry{Path: name, Type: "file", Size: new(int64), SHA256: strings.Repeat("0", 64)})
		r.witness.Files++
	}
	slices.SortFunc(r.inventory.Entries, func(a, b checkpointInventoryEntry) int { return strings.Compare(a.Path, b.Path) })
	r.inventorySeal()
}

// cancellable gives the row a context it cancels itself.
func (r *drillReserveRun) cancellable() context.CancelFunc {
	ctx, cancel := context.WithCancel(r.ctx)
	r.t.Cleanup(cancel)
	r.ctx = ctx
	return cancel
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

// at runs act at call of stage, once the fixture has counted that call.
func (r *drillReserveRun) at(stage string, call int, act func()) {
	counted := r.c.drillRootTest.stage
	r.c.drillRootTest.stage = func(name string) {
		if counted(name); name == stage && r.stages[name] == call {
			act()
		}
	}
}

// swap replaces the record's temporary in the drill directory dir, its writer open, by a private file of the same
// bytes: the writer's file is moved aside through a root held on dir and the replacement created through the walk.
func (r *drillReserveRun) swap(dir string) os.FileInfo {
	root, err := os.OpenRoot(dir)
	drillAssert(r.t, err == nil, "cannot hold the drill directory for the swap")
	defer func() { _ = root.Close() }()
	data, err := root.ReadFile(drillReservePending)
	drillAssert(r.t, err == nil && len(data) > 0 && root.Rename(drillReservePending, drillReservePending+"-moved") == nil,
		"cannot move the record's temporary aside")
	f, err := drillPlatformCreate(nil, nil, root, "", drillReservePending)
	drillAssert(r.t, err == nil, "cannot create the replacing temporary")
	_, err = f.Write(data)
	info, statErr := f.Stat()
	drillAssert(r.t, errors.Join(err, statErr, f.Close()) == nil, "cannot write the replacing temporary")
	return info
}

// refused requires the closed reason want, no identifier, the tree as before the call and no drill listed; made
// says the drill directory was made and then discarded, else nothing was written.
func (r *drillReserveRun) refused(drillID string, err error, want string, made bool) {
	r.t.Helper()
	listing, listErr := r.c.drillRootList()
	drillReserveExpect(r.t, err == error(drillStorageError{reason: want}), err, "the reservation gave another outcome")
	drillAssert(r.t, drillID == "", "the refused reservation returned an identifier")
	drillAssert(r.t, r.stages["reserve_mkdir"] == map[bool]int{true: 1}[made], "the reservation wrote other than its row says")
	drillAssert(r.t, (r.stages["directory"] > 0) == made, "the reservation discarded other than its row says")
	drillAssert(r.t, listErr == nil && reflect.DeepEqual(listing, drillRootListing{}), "the reservation left a drill listed")
	drillAssert(r.t, reflect.DeepEqual(r.before, checkpointTreeState(r.t, r.c)), "the reservation left the tree changed")
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
		manifest, err := readManifest(t.Context(), r.c.cfg.Root)
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
		r, syncs, directories, synced := drillReserveStart(t), 0, 0, []string(nil)
		sync := drillRootSync
		t.Cleanup(func() { drillRootSync = sync })
		drillRootSync = func(root *os.Root) error {
			if syncs++; r.stages["reserve_sync"] > 0 {
				synced = append(synced, filepath.Clean(root.Name()))
			}
			return sync(root)
		}
		drillID, err := r.reserve()
		for _, entry := range r.inventory.Entries {
			directories += map[bool]int{true: 1}[entry.Type == "dir"]
		}
		drillAssert(t, err == nil && syncs == 2+directories+1+1, "the reservation did not sync the recovery directory, the drill directory twice and each directory it made")
		// G2: children before parents, the drill directory last: no directory is synced after one that holds it.
		for index, name := range synced {
			for _, later := range synced[index+1:] {
				drillAssert(t, !strings.HasPrefix(later, name+string(filepath.Separator)), "a directory was synced before a directory below it")
			}
		}
		drillAssert(t, r.c.deleteDrillLocked(drillID) == nil, "the synced drill was not deleted")
	})
	t.Run("floor_exact", func(t *testing.T) {
		r := drillReserveStart(t)
		r.exact(func() bool { return false })
		drillID, err := r.reserve()
		drillAssert(t, err == nil && r.c.deleteDrillLocked(drillID) == nil && reflect.DeepEqual(r.before, checkpointTreeState(t, r.c)),
			"the reservation refused exactly its remaining need and floor")
	})
	for _, row := range []struct {
		name, want string
		made       bool
		setup      func(r *drillReserveRun)
	}{
		{"floor_crossed", "storage_insufficient", true, func(r *drillReserveRun) { r.exact(func() bool { return r.stages["reserve_chunk"] > 0 }) }},
		// G3: one byte short from the allowance's second chunk on, after the floor was met at every earlier chunk.
		{"floor_crossed_later", "storage_insufficient", true, func(r *drillReserveRun) { r.exact(func() bool { return r.allocated > uint64(r.witness.Bytes) }) }},
		// P3: an entry below a directory the inventory does not list, or below a file it lists, is refused before any write.
		{"documents_unsound/unlisted", "checkpoint_unverified", false, func(r *drillReserveRun) { r.unsound("zz-absent/zz-file") }},
		{"documents_unsound/below_file", "checkpoint_unverified", false, func(r *drillReserveRun) { r.unsound("zz-file", "zz-file/zz-child") }},
		// N1: a cancellation once the identifier is drawn; G8: one at the reservation's own probe, before its document read.
		{"cancelled_drawn", "cancelled", false, func(r *drillReserveRun) {
			cancel := r.cancellable()
			drillReserveNewID = func() (string, error) { cancel(); return "drill_aaaaaaaaaaaaaaaa", nil }
		}},
		{"cancelled_documents", "cancelled", false, func(r *drillReserveRun) { r.second(r.cancellable()) }},
		// G12: a reason only admission finds; the reservation's own probe would answer boundary_refused to a zero estimate.
		{"admission_only", "allowance_invalid", false, func(r *drillReserveRun) { r.allowance = 0 }},
		// G4: a directory planted at clone/'s name before its mkdir is a collision, discarded with the drill.
		{"clone_planted", "allocation_collision", true, func(r *drillReserveRun) {
			dir := r.fixed()
			r.on["reserve_tree"] = func() {
				drillAssert(r.t, privateDir(filepath.Join(dir, drillReserveClone), true) == nil, "cannot plant a directory at the name of clone")
			}
		}},
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
		{"sparse", "allocation_unproved", true, func(r *drillReserveRun) { r.allocate = drillReserveSparse(r.t) }},
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
			drillAssert(t, row.name != "floor_crossed_later" || r.allocated == uint64(r.witness.Bytes)+64<<10, "the floor was not re-checked at the allowance's second chunk")
		})
	}
	// P1: free space that falls by exactly what the file system charges the drill, from its need and floor at admission
	// (the fixture, and with 100 one-byte files more): reserved.
	for _, files := range []int{0, 100} {
		t.Run(fmt.Sprintf("floor_charged/%d", files), func(t *testing.T) {
			r := drillReserveStart(t)
			for index := range files { // each sorts after every entry before it
				size := int64(1)
				r.inventory.Entries = append(r.inventory.Entries, checkpointInventoryEntry{Path: fmt.Sprintf("zz-one-%03d", index), Type: "file", Size: &size, SHA256: strings.Repeat("0", 64)})
				r.witness.Files, r.witness.Bytes = r.witness.Files+1, r.witness.Bytes+1
			}
			r.inventorySeal()
			dir, need := r.fixed(), drillStorageNeed(r.checkpointVerifyCase, 4096, r.allowance).need
			r.volume(func(int) drillVolume { return drillVolume{unit: 4096, free: need + drillStorageFloor - r.charged(dir)} })
			drillID, err := r.reserve()
			drillReserveExpect(t, err == nil, err, "a reservation admitted at its need and floor was refused by what the file system charged it")
			drillAssert(t, r.c.deleteDrillLocked(drillID) == nil && reflect.DeepEqual(r.before, checkpointTreeState(t, r.c)), "the reserved drill was not deleted back to the tree before")
		})
	}
	// P2: at drillRootSync's first call, right after the mkdir, a private directory holding a private file replaces the
	// empty drill directory: refused before the record, never discarded, the replacement and its file untouched.
	t.Run("drill_replaced", func(t *testing.T) {
		r, planted, sync := drillReserveStart(t), false, drillRootSync
		dir := r.fixed()
		r.fault(drillStorageError{reason: "storage_exhausted"})
		t.Cleanup(func() { drillRootSync = sync })
		drillRootSync = func(root *os.Root) error {
			if !planted {
				planted = true
				drillAssert(t, privateDir(dir+"-other", true) == nil, "cannot make the replacing directory")
				other, err := os.OpenRoot(dir + "-other")
				drillAssert(t, err == nil, "cannot hold the replacing directory")
				f, err := drillPlatformCreate(nil, nil, other, "", "foreign")
				drillAssert(t, err == nil && f.Close() == nil && other.Close() == nil, "cannot make the replacing directory's file")
				drillReserveReplace(t, dir+"-other", dir)
			}
			return sync(root)
		}
		drillID, err := r.reserve()
		entries, readErr := os.ReadDir(dir)
		drillReserveExpect(t, err == error(drillStorageError{reason: "allocation_collision"}), err, "a directory put at the drill's name was not refused as a collision")
		drillAssert(t, drillID == "", "the refused reservation returned an identifier")
		drillAssert(t, r.stages["reserve_record"] == 0, "the record was written into the replacing directory")
		drillAssert(t, r.stages["open"] == 0, "the replacing directory was discarded")
		drillAssert(t, readErr == nil && len(entries) == 1 && entries[0].Name() == "foreign" && entries[0].Type().IsRegular(), "the replacing directory or its file changed")
		drillAssert(t, os.RemoveAll(dir) == nil, "the replacing directory cannot be removed: a handle on it was kept")
		drillAssert(t, reflect.DeepEqual(r.before, checkpointTreeState(t, r.c)), "the refused reservation wrote elsewhere")
	})
	// G4: the n-th directory sync refused, for each n until a reservation makes no n-th: recovery/ after the mkdir, the
	// drill directory after the record, each directory made, the drill directory last; every one is discarded.
	t.Run("sync_refused", func(t *testing.T) {
		done := false
		for n := 1; !done && n <= 64; n++ {
			t.Run(strconv.Itoa(n), func(t *testing.T) {
				r, calls, sync := drillReserveStart(t), 0, drillRootSync
				t.Cleanup(func() { drillRootSync = sync })
				drillRootSync = func(root *os.Root) error {
					if calls++; calls == n {
						return errors.New("drill reserve fixture sync refused")
					}
					return sync(root)
				}
				drillID, err := r.reserve()
				if done = calls < n; !done {
					r.refused(drillID, err, "boundary_refused", true)
					return
				}
				directories := 0
				drillRootSync = sync // the deletion's own syncs come after the reservation's last
				for _, entry := range r.inventory.Entries {
					directories += map[bool]int{true: 1}[entry.Type == "dir"]
				}
				drillAssert(t, err == nil && n == 2+directories+1+1+1 && r.c.deleteDrillLocked(drillID) == nil, "the syncs refused were not every sync the reservation makes")
			})
		}
		drillAssert(t, done, "the reservation made more syncs than the row refuses")
	})
	// G4: the directory the sync loop opens first replaced by a link at reserve_sync: refused, its discard refused.
	t.Run("sync_link", func(t *testing.T) {
		r, deepest := drillReserveStart(t), ""
		for _, entry := range r.inventory.Entries {
			if entry.Type == "dir" {
				deepest = entry.Path
			}
		}
		dir := r.fixed()
		moved := filepath.Join(dir, drillReserveClone, filepath.FromSlash(deepest))
		r.on["reserve_sync"] = func() { drillReserveLink(t, moved+"-moved", moved) }
		drillID, err := r.reserve()
		var cleanup drillCleanupError
		drillReserveExpect(t, errors.As(err, &cleanup) && cleanup.cause == drillStorageError{reason: "boundary_refused"} && drillID == filepath.Base(dir), err,
			"a link at a made directory was not refused at its sync with its discard refused")
	})
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
	// The planted-link rows (moved here by G4B2's cut, OQ7). Each link is relative and stays inside the cell, so an open
	// that followed it would reach the moved object; the binds and the no-follow walk refuse it, and a discard refuses
	// a tree holding it (OQ6).
	t.Run("boundary_link", func(t *testing.T) {
		r := drillReserveStart(t)
		recovery := r.c.recoveryDir()
		plant := func() { drillReserveLink(t, recovery+"-moved", recovery); r.before = checkpointTreeState(t, r.c) }
		if runtime.GOOS == "windows" {
			// Admission's os.OpenRoot handle on recovery/ shares no delete (syscall.Open), so recovery/ cannot move inside
			// admission on Windows: there the link is planted at reserve_boundary, once admission has closed it (G10).
			r.on["reserve_boundary"] = plant
		} else {
			volume, calls := drillStorageVolume, 0
			drillStorageSeam(t, &drillStorageVolume, func(f *os.File) (drillVolume, error) {
				if calls++; calls == 1 {
					plant()
				}
				return volume(f)
			})
		}
		drillID, err := r.reserve()
		drillReserveExpect(t, err == error(drillStorageError{reason: "boundary_refused"}), err, "a link at the name of recovery was not refused")
		drillAssert(t, drillID == "", "the refused reservation returned an identifier")
		drillAssert(t, r.stages["reserve_mkdir"] == 0, "the reservation wrote through a link at the name of recovery")
		drillAssert(t, reflect.DeepEqual(r.before, checkpointTreeState(t, r.c)), "the reservation changed the directory a link at the name of recovery names")
	})
	t.Run("drill_link", func(t *testing.T) {
		r, planted, sync := drillReserveStart(t), false, drillRootSync
		dir := r.fixed()
		t.Cleanup(func() { drillRootSync = sync })
		drillRootSync = func(root *os.Root) error { // its first call follows the drill directory's mkdir
			if !planted {
				planted = true
				drillReserveLink(t, dir+"-moved", dir)
			}
			return sync(root)
		}
		drillID, err := r.reserve()
		var cleanup drillCleanupError
		entries, readErr := os.ReadDir(dir + "-moved")
		drillReserveExpect(t, errors.As(err, &cleanup) && cleanup.cause == drillStorageError{reason: "boundary_refused"} && drillID == filepath.Base(dir), err,
			"a link at the drill's name was not refused with its discard refused")
		drillAssert(t, readErr == nil && len(entries) == 0, "the reservation wrote through a link at the drill's name")
	})
	t.Run("record_swap", func(t *testing.T) {
		r := drillReserveStart(t)
		dir := r.fixed()
		r.on["reserve_publish"] = func() { r.swap(dir) }
		drillID, err := r.reserve()
		r.refused(drillID, err, "boundary_refused", true)
	})
	t.Run("publish_swapped", func(t *testing.T) {
		r, replacement := drillReserveStart(t), os.FileInfo(nil)
		dir := r.fixed()
		r.on["reserve_publish"] = func() {
			replacement = r.swap(dir)
			drillAssert(t, privateDir(filepath.Join(dir, drillRootRecordName), true) == nil, "cannot plant a directory at the record's name")
		}
		drillID, err := r.reserve()
		var cleanup drillCleanupError
		info, statErr := os.Lstat(filepath.Join(dir, drillReservePending))
		// As publish_refused: os.Root.Rename refuses the planted directory with EEXIST, an allocation collision.
		drillReserveExpect(t, errors.As(err, &cleanup) && cleanup.cause == drillStorageError{reason: "allocation_collision"} && drillID == filepath.Base(dir), err,
			"a refused publication of a swapped temporary was not an incomplete cleanup of a collision")
		drillAssert(t, statErr == nil && os.SameFile(replacement, info), "the replacing temporary, not the writer's, was removed")
	})
	// An added file entry sorts after the created directory pg and before pg's first child, so at that file's
	// reserve_file stage nothing below pg is open (NTFS refuses to rename a directory with an open file below it) and
	// pg's children are still to come: the next creation below pg meets the link.
	t.Run("tree_link", func(t *testing.T) {
		r, files := drillReserveStart(t), 0
		drillAssert(t, r.entry("pg").Type == "dir", "the reservation fixture holds no pg directory")
		r.inventory.Entries = append(r.inventory.Entries, checkpointInventoryEntry{Path: "pg-later", Type: "file", Size: new(int64), SHA256: strings.Repeat("0", 64)})
		slices.SortFunc(r.inventory.Entries, func(a, b checkpointInventoryEntry) int { return strings.Compare(a.Path, b.Path) })
		r.witness.Files++
		r.inventorySeal()
		for _, entry := range r.inventory.Entries {
			if files += map[bool]int{true: 1}[entry.Type == "file"]; entry.Path == "pg-later" {
				break
			}
		}
		dir := r.fixed()
		moved := filepath.Join(dir, drillReserveClone, "pg")
		r.at("reserve_file", files, func() { drillReserveLink(t, moved+"-moved", moved) })
		drillID, err := r.reserve()
		var cleanup drillCleanupError
		entries, readErr := os.ReadDir(moved + "-moved")
		drillReserveExpect(t, errors.As(err, &cleanup) && cleanup.cause == drillStorageError{reason: "boundary_refused"} && drillID == filepath.Base(dir), err,
			"a creation below a link at a created directory was not refused with its discard refused")
		drillAssert(t, readErr == nil && len(entries) == 0, "the reservation created something through a link at a created directory")
	})
	// The last file below a directory D, its writer open at its proof's reopen: D, or on Windows the file itself (NTFS
	// refuses to rename a directory with an open file below it), replaced by a link the no-follow reopen refuses.
	t.Run("verify_link", func(t *testing.T) {
		r, call, files, name := drillReserveStart(t), 0, 0, ""
		for _, entry := range r.inventory.Entries {
			if entry.Type == "file" {
				if files++; path.Dir(entry.Path) != "." {
					call, name = files, entry.Path
				}
			}
		}
		drillAssert(t, call > 0, "the reservation fixture holds no file below a directory")
		if runtime.GOOS != "windows" {
			name = path.Dir(name)
		}
		dir := r.fixed()
		planted := filepath.Join(dir, drillReserveClone, filepath.FromSlash(name))
		r.at("reserve_verify", call, func() { drillReserveLink(t, planted+"-moved", planted) })
		drillID, err := r.reserve()
		var cleanup drillCleanupError
		drillReserveExpect(t, errors.As(err, &cleanup) && cleanup.cause == drillStorageError{reason: "allocation_unproved"} && drillID == filepath.Base(dir), err,
			"a link on the reopened file's path was not refused at its proof with its discard refused")
	})
}
