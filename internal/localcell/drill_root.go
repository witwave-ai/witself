package localcell

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
)

const drillRootRecordName = "drill.json"

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
