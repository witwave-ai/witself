package gitopsvalues

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// FirstSyncCheck returns the API host of a never-provisioned catalog cell whose
// existing server pins are no newer than version. It only reads local files.
func FirstSyncCheck(root, cell, version string) (string, error) {
	if !releaseVersionPattern.MatchString(version) {
		return "", fmt.Errorf("release version must look like MAJOR.MINOR.PATCH")
	}
	cfg, err := loadCatalog(root)
	if err != nil {
		return "", err
	}
	entry, ok := cfg.Cells[cell]
	if !ok {
		return "", fmt.Errorf("first sync: cell is not in the cell catalog")
	}
	if !entry.Unprovisioned {
		return "", fmt.Errorf("first sync: cell is not recorded as unprovisioned in %s", catalogRelPath)
	}
	raw, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(valuesRel(cell))))
	if err != nil {
		if os.IsNotExist(err) {
			return "", fmt.Errorf("first sync: cell has no generated values.yaml; onboard it first")
		}
		return "", fmt.Errorf("first sync: %w", err)
	}
	pins, err := parseServerPins(raw)
	if err != nil {
		return "", fmt.Errorf("first sync: %s: %w", valuesRel(cell), err)
	}
	for _, pin := range []struct{ field, value string }{
		{"chartVersion", pins.ChartVersion},
		{"imageTag", pins.ImageTag},
	} {
		if !releaseVersionPattern.MatchString(pin.value) {
			return "", fmt.Errorf("first sync: pinned %s %q is not MAJOR.MINOR.PATCH", pin.field, pin.value)
		}
		comparison, err := compareFirstSyncVersions(version, pin.value)
		if err != nil {
			return "", fmt.Errorf("first sync: version component out of range comparing %s with the pinned %s %s", version, pin.field, pin.value)
		}
		if comparison < 0 {
			return "", fmt.Errorf("first sync: target %s is lower than the pinned %s %s", version, pin.field, pin.value)
		}
	}
	resolved, err := resolveCell(cell, cfg)
	if err != nil {
		return "", fmt.Errorf("first sync: %w", err)
	}
	return resolved.APIHost, nil
}

func compareFirstSyncVersions(target, pinned string) (int, error) {
	var components [2][3]uint64
	for side, version := range []string{target, pinned} {
		for index, part := range strings.Split(version, ".") {
			value, err := strconv.ParseUint(part, 10, 64)
			if err != nil {
				return 0, err
			}
			components[side][index] = value
		}
	}
	for index, value := range components[0] {
		if value < components[1][index] {
			return -1, nil
		}
		if value > components[1][index] {
			return 1, nil
		}
	}
	return 0, nil
}
