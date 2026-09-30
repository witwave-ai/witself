package gitopsvalues

import (
	"bytes"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestFirstSyncCheck(t *testing.T) {
	const production = "civo-prod-use1-serving"
	const host = "api.civo-prod-use1-serving.cells.witself.witwave.ai"
	for _, tc := range []struct {
		name      string
		cell      string
		version   string
		values    string
		unmarked  bool
		equal     bool
		remove    bool
		wantError string
		contains  bool
	}{
		{name: "marked production", version: "99.0.0"},
		{name: "equal to higher fixture pin", equal: true},
		{name: "older than chart", version: "0.0.1", wantError: "first sync: target 0.0.1 is lower than the pinned chartVersion", contains: true},
		{
			name: "older than image", version: "1.5.0",
			values:    "apps:\n  witselfServer:\n    chartVersion: 1.0.0\n    imageTag: 2.0.0\n",
			wantError: "is lower than the pinned imageTag 2.0.0", contains: true,
		},
		{
			name: "nonrelease chart", version: "99.0.0",
			values:    "apps:\n  witselfServer:\n    chartVersion: latest\n    imageTag: 2.0.0\n",
			wantError: `first sync: pinned chartVersion "latest" is not MAJOR.MINOR.PATCH`,
		},
		{
			name: "four component chart", version: "99.0.0",
			values:    "apps:\n  witselfServer:\n    chartVersion: 1.0.0.0\n    imageTag: 2.0.0\n",
			wantError: `first sync: pinned chartVersion "1.0.0.0" is not MAJOR.MINOR.PATCH`,
		},
		{
			name: "target component overflow", version: "18446744073709551616.0.0",
			wantError: "first sync: version component out of range comparing 18446744073709551616.0.0 with the pinned chartVersion", contains: true,
		},
		{
			name: "unmarked production", version: "99.0.0", unmarked: true,
			wantError: "first sync: cell is not recorded as unprovisioned in .gitops/cells/catalog.yaml",
		},
		{
			name: "unmarked backup", cell: "civo-sandbox-use1-backup", version: "99.0.0",
			wantError: "first sync: cell is not recorded as unprovisioned in .gitops/cells/catalog.yaml",
		},
		{
			name: "unknown cell", cell: "civo-prod-use1-nope", version: "99.0.0",
			wantError: "first sync: cell is not in the cell catalog",
		},
		{
			name: "missing values", version: "99.0.0", remove: true,
			wantError: "first sync: cell has no generated values.yaml; onboard it first",
		},
		{name: "invalid target", version: "v1.2.3", wantError: "release version must look like MAJOR.MINOR.PATCH"},
		{
			name: "numeric minor comparison", version: "1.10.0",
			values: "apps:\n  witselfServer:\n    chartVersion: 1.9.0\n    imageTag: 1.9.0\n",
		},
		{
			name: "later target component overflow", version: "99.0.18446744073709551616",
			wantError: "first sync: version component out of range comparing 99.0.18446744073709551616 with the pinned chartVersion", contains: true,
		},
		{
			name: "later pinned component overflow", version: "99.0.0",
			values:    "apps:\n  witselfServer:\n    chartVersion: 1.18446744073709551616.0\n    imageTag: 2.0.0\n",
			wantError: "first sync: version component out of range comparing 99.0.0 with the pinned chartVersion 1.18446744073709551616.0",
		},
		{
			name: "missing image pin", version: "99.0.0",
			values:    "apps:\n  witselfServer:\n    chartVersion: 1.0.0\n",
			wantError: `first sync: pinned imageTag "" is not MAJOR.MINOR.PATCH`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := copyGenerationFixture(t)
			setFirstSyncMarker(t, root, production, !tc.unmarked)
			cell := tc.cell
			if cell == "" {
				cell = production
			}
			path := filepath.Join(root, filepath.FromSlash(valuesRel(cell)))
			if tc.values != "" {
				if err := os.WriteFile(path, []byte(tc.values), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			version := tc.version
			if tc.equal {
				pins, err := readServerPins(path, chartPins{})
				if err != nil {
					t.Fatal(err)
				}
				version = pins.ChartVersion
				chartParts, imageParts := strings.Split(pins.ChartVersion, "."), strings.Split(pins.ImageTag, ".")
				for index, part := range chartParts {
					chart, err := strconv.ParseUint(part, 10, 64)
					if err != nil {
						t.Fatal(err)
					}
					image, err := strconv.ParseUint(imageParts[index], 10, 64)
					if err != nil {
						t.Fatal(err)
					}
					if image > chart {
						version = pins.ImageTag
					}
					if image != chart {
						break
					}
				}
			}
			if tc.remove {
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
			}
			got, err := FirstSyncCheck(root, cell, version)
			if tc.wantError != "" {
				if err == nil {
					t.Fatalf("FirstSyncCheck succeeded, want %q", tc.wantError)
				}
				matches := err.Error() == tc.wantError
				if tc.contains {
					matches = strings.Contains(err.Error(), tc.wantError)
				}
				if !matches {
					t.Fatalf("FirstSyncCheck error = %q, want %q", err, tc.wantError)
				}
				if got != "" {
					t.Fatal("refused first sync returned an API host")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got != host {
				t.Fatalf("FirstSyncCheck host = %q, want %q", got, host)
			}
		})
	}
}

func TestCatalogRejectsUnprovisionedDocumentationOnlyCell(t *testing.T) {
	root := copyGenerationFixture(t)
	setFirstSyncMarker(t, root, "civo-sandbox-use1-serving", true)
	_, err := loadCatalog(root)
	if err == nil || !strings.Contains(err.Error(), "sets unprovisioned with domain_documentation_only") {
		t.Fatalf("loadCatalog error = %v, want documentation-only host refusal", err)
	}
}

func TestUnprovisionedMarkerIsNotRendered(t *testing.T) {
	root := copyGenerationFixture(t)
	setFirstSyncMarker(t, root, "civo-prod-use1-serving", true)
	marked, err := generateAll(root)
	if err != nil {
		t.Fatal(err)
	}
	setFirstSyncMarker(t, root, "civo-prod-use1-serving", false)
	unmarked, err := generateAll(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(marked) != len(unmarked) {
		t.Fatal("unprovisioned marker changed the generated cell set")
	}
	for cell, body := range marked {
		if !bytes.Equal(body, unmarked[cell]) {
			t.Fatalf("unprovisioned marker changed generated bytes for %s", cell)
		}
	}
}

func setFirstSyncMarker(t *testing.T, root, cell string, marked bool) {
	t.Helper()
	path := filepath.Join(root, catalogRelPath)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var output []string
	inside, found := false, false
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.HasPrefix(line, "  ") && !strings.HasPrefix(line, "   ") && strings.HasSuffix(line, ":") {
			inside = line == "  "+cell+":"
			if inside {
				found = true
			}
		}
		if inside && strings.HasPrefix(line, "    unprovisioned:") {
			continue
		}
		output = append(output, line)
		if inside && line == "  "+cell+":" && marked {
			output = append(output, "    unprovisioned: true")
		}
	}
	if !found {
		t.Fatalf("catalog lacks cell %s", cell)
	}
	if err := os.WriteFile(path, []byte(strings.Join(output, "\n")), 0o600); err != nil {
		t.Fatal(err)
	}
}
