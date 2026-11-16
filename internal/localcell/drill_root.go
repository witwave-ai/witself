package localcell

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
)

const drillRootRecordName = "drill.json"

type drillRootOperations struct{ beforeRemove func(string) }
type drillRootError struct{ reason string }

func (drillRootError) Error() string { return "drill root operation refused" }

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

// drillRootClassify is shared by observation and exact-identifier deletion.
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

// deleteDrillLocked requires the caller's operation lock and maintenance fence.
// Removals act on the held directory. A final boundary sync failure leaves the
// target absent, so a retry answers absent; no journal entry is written.
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
	entries, err := os.ReadDir(c.recoveryDir())
	if err != nil {
		return drillRootError{reason: "boundary_refused"}
	}
	found := false
	for _, entry := range entries {
		if entry.Name() != drillID && strings.EqualFold(entry.Name(), drillID) {
			return drillRootError{reason: "alias"}
		}
		found = found || entry.Name() == drillID
	}
	if !found {
		return drillRootError{reason: "absent"}
	}
	dir, err := c.drillDir(drillID)
	if err != nil {
		return drillRootError{reason: "identity_invalid"}
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return drillRootError{reason: "unknown_kind"}
	}
	defer func() { _ = root.Close() }()
	held, err := root.Stat(".")
	if err != nil {
		return drillRootError{reason: "unknown_kind"}
	}
	kind, record, _ := drillRootClassify(dir, drillID, manifest.InstallationID)
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
	if c.drillRootTest != nil && c.drillRootTest.beforeRemove != nil {
		c.drillRootTest.beforeRemove(dir)
	}
	now, err := os.Lstat(dir)
	if err != nil || !now.IsDir() || now.Mode()&(os.ModeSymlink|os.ModeIrregular) != 0 || !os.SameFile(held, now) {
		return drillRootError{reason: "boundary_refused"}
	}
	f, err := root.Open(".")
	if err != nil {
		return drillRootError{reason: "remove_failed"}
	}
	names, err := f.Readdirnames(-1)
	closed := f.Close()
	if err != nil || closed != nil {
		return drillRootError{reason: "remove_failed"}
	}
	for _, name := range names {
		if name != drillRootRecordName && root.RemoveAll(name) != nil {
			return drillRootError{reason: "remove_failed"}
		}
	}
	if kind != "remainder" {
		if syncCheckpointDirectory(dir) != nil {
			return drillRootError{reason: "remove_failed"}
		}
		if root.Remove(drillRootRecordName) != nil {
			return drillRootError{reason: "remove_failed"}
		}
	}
	if root.Close() != nil || os.Remove(dir) != nil {
		return drillRootError{reason: "remove_failed"}
	}
	if syncCheckpointDirectory(c.recoveryDir()) != nil {
		return drillRootError{reason: "remove_failed"}
	}
	return nil
}
