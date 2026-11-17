package localcell

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
)

const drillRootRecordName = "drill.json"

// drillRootHere, as a path's last element, makes the element before it a
// directory component, which the kernel (pathname open) and os.Root
// (rootOpenDir, O_DIRECTORY|O_NOFOLLOW) open as directories only, so a planted
// FIFO or device is refused, never opened.
const drillRootHere = string(filepath.Separator) + "."

// drillRootSweeps is the last sweep index of each removal loop.
const drillRootSweeps = 3

type drillRootOperations struct{ stage func(string) }
type drillRootError struct{ reason string }

func (drillRootError) Error() string { return "drill root operation refused" }

// drillRootSync syncs a held directory. Windows has no portable directory sync
// (as syncDirectory). Tests replace it.
var drillRootSync = func(r *os.Root) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	d, err := r.Open(".")
	if err != nil {
		return err
	}
	err = d.Sync()
	if closed := d.Close(); err == nil {
		err = closed
	}
	return err
}

func (c *Cell) drillRootStage(name string) {
	if c.drillRootTest != nil && c.drillRootTest.stage != nil {
		c.drillRootTest.stage(name)
	}
}

// drillRootNames lists a held directory by name.
func drillRootNames(r *os.Root) ([]string, error) {
	d, err := r.Open(".")
	if err != nil {
		return nil, err
	}
	names, err := d.Readdirnames(-1)
	if closed := d.Close(); err == nil {
		err = closed
	}
	return names, err
}

// drillDir resolves one exact drill ID below the recovery boundary.
func (c *Cell) drillDir(drillID string) (string, error) {
	if !drillValidID(drillID) {
		return "", drillRootError{reason: "identity_invalid"}
	}
	dir := filepath.Join(c.recoveryDir(), drillID)
	if filepath.Dir(dir) != c.recoveryDir() || filepath.Base(dir) != drillID {
		return "", drillRootError{reason: "identity_invalid"}
	}
	return dir, nil
}

type drillRootListing struct {
	declared, remainder []string
	foreign, unknown    int
}

// drillRootClassify names one drill directory declared, remainder, foreign or unknown.
func drillRootClassify(dir, name, installation string) (string, drillRecord, []os.DirEntry) {
	if privateDir(dir, false) != nil {
		return "unknown", drillRecord{}, nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "unknown", drillRecord{}, nil
	}
	if len(entries) == 0 {
		return "remainder", drillRecord{}, entries
	}
	f, err := openPrivateFile(filepath.Join(dir, drillRootRecordName), os.O_RDONLY, 0)
	if err != nil {
		return "unknown", drillRecord{}, entries
	}
	data, err := io.ReadAll(io.LimitReader(f, drillMaxBytes+1))
	closed := f.Close()
	if err != nil || closed != nil {
		return "unknown", drillRecord{}, entries
	}
	record, err := drillParseRecord(data)
	if err != nil || record.DrillID != name {
		return "unknown", drillRecord{}, entries
	}
	if record.Binding.InstallationID != installation {
		return "foreign", record, entries
	}
	return "declared", record, entries
}

func (c *Cell) drillRootList() (drillRootListing, error) {
	var listing drillRootListing
	if c.validateRecovery() != nil {
		return listing, drillRootError{reason: "boundary_refused"}
	}
	entries, err := os.ReadDir(c.recoveryDir())
	if errors.Is(err, os.ErrNotExist) {
		return listing, nil
	}
	if err != nil {
		return listing, drillRootError{reason: "boundary_refused"}
	}
	manifest, err := readManifest(c.cfg.Root)
	if err != nil {
		return listing, drillRootError{reason: "boundary_refused"}
	}
	// ReadDir is sorted, so both identifier lists retain lexical order.
	for _, entry := range entries {
		name := entry.Name()
		if !drillValidID(name) {
			if drillValidID(strings.ToLower(name)) {
				listing.unknown++
			}
			continue
		}
		dir, err := c.drillDir(name)
		if err != nil {
			return drillRootListing{}, drillRootError{reason: "boundary_refused"}
		}
		kind, _, _ := drillRootClassify(dir, name, manifest.InstallationID)
		switch kind {
		case "declared":
			listing.declared = append(listing.declared, name)
		case "remainder":
			listing.remainder = append(listing.remainder, name)
		case "foreign":
			listing.foreign++
		default:
			listing.unknown++
		}
	}
	return listing, nil
}

// drillRootHeld names a held drill directory declared, remainder, foreign or
// unknown, reading only through the held root.
func drillRootHeld(drill *os.Root, drillID, installation string) (string, drillRecord) {
	names, err := drillRootNames(drill)
	if err != nil {
		return "unknown", drillRecord{}
	}
	if len(names) == 0 {
		return "remainder", drillRecord{}
	}
	f, err := drill.OpenFile(drillRootRecordName, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return "unknown", drillRecord{}
	}
	defer func() { _ = f.Close() }()
	// Checked before any read: a FIFO with a writer held would block the read.
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return "unknown", drillRecord{}
	}
	data, err := io.ReadAll(io.LimitReader(f, drillMaxBytes+1))
	if err != nil {
		return "unknown", drillRecord{}
	}
	record, err := drillParseRecord(data)
	if err != nil || record.DrillID != drillID {
		return "unknown", drillRecord{}
	}
	if record.Binding.InstallationID != installation {
		return "foreign", record
	}
	return "declared", record
}

// drillRootBound reports whether recovery/ and the drill's name still name the
// held directories: held identities come from the handles, current ones from
// the names without following their last element.
func (c *Cell) drillRootBound(recovery, drill *os.Root, drillID string) bool {
	recoveryHeld, err := recovery.Stat(".")
	recoveryNow, nowErr := os.Lstat(c.recoveryDir())
	if err != nil || nowErr != nil || !os.SameFile(recoveryHeld, recoveryNow) {
		return false
	}
	drillHeld, err := drill.Stat(".")
	drillNow, nowErr := recovery.Lstat(drillID)
	return err == nil && nowErr == nil && os.SameFile(drillHeld, drillNow)
}

// drillRootSweep removes every entry of the held drill directory through it,
// keeping each spelling of the record name while keepRecord, and returns the
// number removed; it stops at the first failed removal.
func drillRootSweep(drill *os.Root, keepRecord bool) (int, error) {
	names, err := drillRootNames(drill)
	if err != nil {
		return 0, err
	}
	removed := 0
	for _, name := range names {
		if keepRecord && strings.EqualFold(name, drillRootRecordName) {
			continue
		}
		if err := drill.RemoveAll(name); err != nil {
			return removed, err
		}
		removed++
	}
	return removed, nil
}

// deleteDrillLocked needs the caller's operation lock and maintenance fence. It
// holds recovery/ and the drill directory as roots and classifies through the
// drill root; every removal batch first re-checks that both names still name
// the held directories. The final re-check and removal are two calls: an object
// put at the drill's name between them is removed instead if it is a file, a
// link or an empty directory, never anything with content and nothing outside
// recovery/. A final sync failure leaves the target absent, so a retry answers
// absent. No journal entry is written.
func (c *Cell) deleteDrillLocked(drillID string) error {
	if checkpointIDPattern.MatchString(drillID) {
		return drillRootError{reason: "checkpoint_target"}
	}
	if !drillValidID(drillID) {
		return drillRootError{reason: "identity_invalid"}
	}
	if progress, err := c.readJournal(); err != nil || progress.pending != nil || progress.tornTail {
		return drillRootError{reason: "journal_pending"}
	}
	if c.validateRecovery() != nil {
		return drillRootError{reason: "boundary_refused"}
	}
	manifest, err := readManifest(c.cfg.Root)
	if err != nil {
		return drillRootError{reason: "boundary_refused"}
	}
	c.drillRootStage("open")
	recovery, err := os.OpenRoot(c.recoveryDir() + drillRootHere)
	if err != nil {
		return drillRootError{reason: "boundary_refused"}
	}
	defer func() { _ = recovery.Close() }()
	names, err := drillRootNames(recovery)
	if err != nil {
		return drillRootError{reason: "boundary_refused"}
	}
	found := false
	for _, name := range names {
		if name != drillID && strings.EqualFold(name, drillID) {
			return drillRootError{reason: "alias"}
		}
		found = found || name == drillID
	}
	if !found {
		return drillRootError{reason: "absent"}
	}
	drill, err := recovery.OpenRoot(drillID + drillRootHere)
	if err != nil {
		return drillRootError{reason: "unknown_kind"}
	}
	defer func() { _ = drill.Close() }()
	kind, record := drillRootHeld(drill, drillID, manifest.InstallationID)
	switch kind {
	case "unknown":
		return drillRootError{reason: "unknown_kind"}
	case "foreign":
		return drillRootError{reason: "foreign_owner"}
	case "declared":
		switch record.State {
		case "reserved", "copied", "verified", "stopped":
		default:
			return drillRootError{reason: "running"}
		}
	}
	// Everything but the record, until a sweep finds nothing left to remove.
	for sweep := 0; ; sweep++ {
		c.drillRootStage("sweep")
		if !c.drillRootBound(recovery, drill, drillID) {
			return drillRootError{reason: "boundary_refused"}
		}
		removed, err := drillRootSweep(drill, true)
		if err != nil || (removed > 0 && sweep == drillRootSweeps) {
			return drillRootError{reason: "remove_failed"}
		}
		if removed == 0 {
			break
		}
	}
	if kind != "remainder" && (drillRootSync(drill) != nil || drill.Remove(drillRootRecordName) != nil) {
		return drillRootError{reason: "remove_failed"}
	}
	// The directory itself through the held parent; a late entry is swept first.
	for sweep := 0; ; sweep++ {
		c.drillRootStage("directory")
		if !c.drillRootBound(recovery, drill, drillID) {
			return drillRootError{reason: "boundary_refused"}
		}
		if recovery.Remove(drillID) == nil {
			break
		}
		if _, err := drillRootSweep(drill, false); err != nil || sweep == drillRootSweeps {
			return drillRootError{reason: "remove_failed"}
		}
	}
	if drillRootSync(recovery) != nil {
		return drillRootError{reason: "remove_failed"}
	}
	return nil
}
