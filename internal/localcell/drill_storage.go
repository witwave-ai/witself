package localcell

import (
	"context"
	"errors"
	"math"
	"os"
	"strings"
)

// drillStorageError is a closed, value-free refusal of a drill storage admission.
type drillStorageError struct{ reason string }

func (drillStorageError) Error() string { return "drill storage refused" }

const (
	drillStorageFloor = checkpointReserve // free bytes a drill leaves after its need (provisional; owner question S2)
	drillStorageFixed = 1 << 20           // a drill record, its temporary and slack
	drillStorageEntry = 256               // per inventory entry, as checkpointPlan.need
	drillExt4Magic    = 0xEF53            // EXT4_SUPER_MAGIC, linux/magic.h
)

// drillVolume is an identified destination volume: its allocation unit and the bytes available to this user.
type drillVolume struct{ unit, free uint64 }

// drillStorageEstimate is what one drill of one kept checkpoint would need; nothing is reserved.
type drillStorageEstimate struct{ unit, clone, allowance, need, free uint64 }

// drillStorageVolume identifies the volume of a held directory; only tests replace it.
var drillStorageVolume = drillPlatformVolume

func drillStorageAdd(a, b uint64) uint64 {
	if a > math.MaxUint64-b {
		return math.MaxUint64
	}
	return a + b
}

func drillStorageMul(a, b uint64) uint64 {
	if a != 0 && b > math.MaxUint64/a {
		return math.MaxUint64
	}
	return a * b
}

func drillStorageRound(size, unit uint64) uint64 {
	if unit == 0 {
		return math.MaxUint64
	}
	whole := size / unit
	if size%unit != 0 {
		whole++
	}
	if whole > math.MaxUint64/unit {
		return math.MaxUint64
	}
	return whole * unit
}

// drillCloneBytes is the clone's need from a verified inventory: each file rounded up to the unit, a unit per
// directory plus the drill and clone directories, 256 bytes per entry and 1 MiB fixed.
func drillCloneBytes(entries []checkpointInventoryEntry, unit uint64) uint64 {
	total := drillStorageAdd(drillStorageFixed, drillStorageAdd(unit, unit))
	for _, entry := range entries {
		total = drillStorageAdd(total, drillStorageEntry)
		switch {
		case entry.Type == "dir":
			total = drillStorageAdd(total, unit)
		case entry.Type == "file" && entry.Size != nil && *entry.Size >= 0:
			total = drillStorageAdd(total, drillStorageRound(uint64(*entry.Size), unit))
		default:
			return math.MaxUint64
		}
	}
	return total
}

// drillStorageAdmits reports whether free covers need plus the floor.
func drillStorageAdmits(free, need uint64) bool {
	want := drillStorageAdd(need, drillStorageFloor)
	return need != math.MaxUint64 && want != math.MaxUint64 && free >= want
}

// drillLinuxSupported: ext4 only; fallocate mode 0 and FIEMAP prove its allocation.
func drillLinuxSupported(fsType, frsize, bsize uint64) bool {
	return fsType == drillExt4Magic && frsize > 0 && bsize > 0
}

// drillWindowsSupported: local NTFS only; new files in a compressed directory inherit compression.
func drillWindowsSupported(name string, remote, compressed bool) bool {
	return name == "NTFS" && !remote && !compressed
}

// admitDrillStorageLocked is accounting-only preparation for a restore drill:
// nothing is reserved, and a later slice allocates. It needs the caller's
// operation lock and maintenance fence and writes nothing. After the boundary
// validation it holds recovery/ as a directory only, lists it without opening an
// entry, identifies the volume through that held directory and reads only the
// named checkpoint's witness and inventory. It admits when the bytes available
// to this user cover the estimate's need plus the floor. The volume itself
// charges allocated drill bytes, so this arithmetic never adds them again; the
// checkpoint storage rule is unchanged. No branch concludes "absent, so proceed"
// from a not-exist error: absence is read only from a successful listing, and
// every not-exist error is a refusal, a closed drillStorageError, zero estimate.
func (c *Cell) admitDrillStorageLocked(ctx context.Context, checkpointID string, allowance uint64) (drillStorageEstimate, error) {
	if !checkpointIDPattern.MatchString(checkpointID) {
		return drillStorageEstimate{}, drillStorageError{reason: "identity_invalid"}
	}
	if allowance == 0 || allowance > math.MaxInt64 {
		return drillStorageEstimate{}, drillStorageError{reason: "allowance_invalid"}
	}
	if ctx.Err() != nil {
		return drillStorageEstimate{}, drillStorageError{reason: "cancelled"}
	}
	if progress, err := c.readJournal(context.Background()); err != nil || progress.pending != nil || progress.tornTail {
		return drillStorageEstimate{}, drillStorageError{reason: "journal_pending"}
	}
	if c.validateRecovery() != nil {
		return drillStorageEstimate{}, drillStorageError{reason: "boundary_refused"}
	}
	manifest, err := readManifest(c.cfg.Root)
	if err != nil {
		return drillStorageEstimate{}, drillStorageError{reason: "boundary_refused"}
	}
	boundary, err := os.OpenRoot(c.recoveryDir() + drillRootHere)
	if err != nil {
		return drillStorageEstimate{}, drillStorageError{reason: "boundary_refused"}
	}
	defer func() { _ = boundary.Close() }()
	names, err := drillRootNames(boundary)
	if err != nil {
		return drillStorageEstimate{}, drillStorageError{reason: "boundary_refused"}
	}
	found := false
	for _, name := range names {
		if drillValidID(name) || drillValidID(strings.ToLower(name)) {
			return drillStorageEstimate{}, drillStorageError{reason: "drill_outstanding"}
		}
		if name != checkpointID && strings.EqualFold(name, checkpointID) {
			return drillStorageEstimate{}, drillStorageError{reason: "checkpoint_absent"}
		}
		found = found || name == checkpointID
	}
	if !found {
		return drillStorageEstimate{}, drillStorageError{reason: "checkpoint_absent"}
	}
	held, err := boundary.Open(".")
	if err != nil {
		return drillStorageEstimate{}, drillStorageError{reason: "boundary_refused"}
	}
	volume, err := drillStorageVolume(held)
	_ = held.Close()
	var refused drillStorageError
	if errors.As(err, &refused) {
		return drillStorageEstimate{}, refused
	}
	if err != nil || volume.unit < checkpointBlock {
		return drillStorageEstimate{}, drillStorageError{reason: "boundary_refused"}
	}
	dir, err := c.checkpointDir(checkpointID)
	if err != nil {
		return drillStorageEstimate{}, drillStorageError{reason: "checkpoint_absent"}
	}
	budget := checkpointRetryBudget{limit: checkpointRetryFloor}
	witness, inventory, err := verifyCheckpointDocuments(ctx, &budget, c.checkpointOps(), dir, checkpointID)
	if ctx.Err() != nil {
		return drillStorageEstimate{}, drillStorageError{reason: "cancelled"}
	}
	if err != nil || checkpointVerifyShape(inventory, witness, &budget) != nil || witness.InstallationID != manifest.InstallationID {
		return drillStorageEstimate{}, drillStorageError{reason: "checkpoint_unverified"}
	}
	estimate := drillStorageEstimate{unit: volume.unit, free: volume.free, clone: drillCloneBytes(inventory.Entries, volume.unit), allowance: drillStorageRound(allowance, volume.unit)}
	estimate.need = drillStorageAdd(estimate.clone, estimate.allowance)
	if !drillStorageAdmits(estimate.free, estimate.need) {
		return drillStorageEstimate{}, drillStorageError{reason: "storage_insufficient"}
	}
	return estimate, nil
}
