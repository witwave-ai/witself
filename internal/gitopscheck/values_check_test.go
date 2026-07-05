// Package gitopscheck holds tests that verify the .gitops tree is
// well-formed. It exists as a package (rather than living beside the
// gitops files) because `go test ./...` picks it up automatically in CI
// and no other file in .gitops has any reason to be Go.
package gitopscheck

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"
)

// TestUpstreamChartVersionsAreNotWitselfTags is the regression guard for
// issue #21. It reads every cell values.yaml under .gitops/cells and
// asserts that no platform.*chartVersion field looks like a Witself
// release tag (MAJOR.MINOR.PATCH under our current versioning window).
//
// Background: previous rolls of the sandbox cell had substituted the
// Witself release version into EVERY chartVersion field in the cell
// values file — including the upstream helm charts (cert-manager,
// external-dns, external-secrets, keda, metrics-server). The bug was
// silent because the cluster wasn't re-provisioned between rolls, so
// the CRDs installed at first provision kept working. Only a fresh up
// on an empty cluster exposed it (Argo couldn't fetch
// external-secrets/2.7.0 with targetRevision 0.0.81 — that version
// doesn't exist upstream).
//
// The test's discipline: whitelist the two Witself-owned fields
// (apps.witselfServer.chartVersion and apps.witselfServer.imageTag) as
// legal places for a Witself-shaped version. Everywhere else, refuse.
func TestUpstreamChartVersionsAreNotWitselfTags(t *testing.T) {
	root := repoRoot(t)
	cellsDir := filepath.Join(root, ".gitops", "cells")

	entries, err := os.ReadDir(cellsDir)
	if err != nil {
		t.Fatalf("read %s: %v", cellsDir, err)
	}

	// A Witself release tag matches roughly 0.0.<3 or fewer digits>
	// today; our releases are in the 0.0.x line. Once we cross 0.1.0
	// or 1.0.0 this will need to broaden — the goal is "matches our
	// git tag suffix," not "matches any conceivable semver."
	witselfShape := regexp.MustCompile(`^0\.\d+\.\d+$`)

	// The chartVersion regex captures the value. We accept quoted or
	// unquoted YAML scalars, since yq round-trips both forms.
	chartVersionRe := regexp.MustCompile(`(?m)^\s+chartVersion:\s*["']?([^"'\s]+)["']?\s*$`)
	imageTagRe := regexp.MustCompile(`(?m)^\s+imageTag:\s*["']?([^"'\s]+)["']?\s*$`)
	// Anchor blocks: a chartVersion under apps.witselfServer is allowed
	// to be a Witself tag; a chartVersion anywhere else is not. We split
	// the file into two windows: everything before `platform:` and
	// everything from `platform:` onward. Witself-shaped versions after
	// `platform:` are the bug.
	platformMarker := regexp.MustCompile(`(?m)^platform:\s*$`)

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		cellName := entry.Name()
		valuesPath := filepath.Join(cellsDir, cellName, "values.yaml")
		body, err := os.ReadFile(valuesPath)
		if err != nil {
			// Cell dir without a values.yaml is unusual but not this
			// test's concern.
			continue
		}

		src := string(body)

		// Split at `platform:`. Everything before it is the apps section
		// (Witself-owned); everything from `platform:` on is upstream
		// chart territory.
		var appsWindow, platformWindow string
		if loc := platformMarker.FindStringIndex(src); loc != nil {
			appsWindow = src[:loc[0]]
			platformWindow = src[loc[0]:]
		} else {
			// No platform: block. Then everything is apps; nothing to check.
			continue
		}

		_ = appsWindow // apps-side Witself tags are legal; no check needed here.

		// Every chartVersion under platform must NOT match the Witself tag shape.
		for _, m := range chartVersionRe.FindAllStringSubmatch(platformWindow, -1) {
			ver := m[1]
			if witselfShape.MatchString(ver) {
				t.Errorf("%s: platform-side chartVersion=%q looks like a Witself release tag — the roll process is poisoning upstream chart versions again (see issue #21)", cellName, ver)
			}
		}
		// Same rule for imageTag if it ever appears platform-side — no
		// upstream image today uses the imageTag key, so any Witself-
		// looking value there is drift.
		for _, m := range imageTagRe.FindAllStringSubmatch(platformWindow, -1) {
			ver := m[1]
			if witselfShape.MatchString(ver) {
				t.Errorf("%s: platform-side imageTag=%q looks like a Witself release tag — should not appear under platform.*", cellName, ver)
			}
		}
	}
}

// TestUpstreamChartVersionsMatchPlatformDefaults tightens the guard: not
// only must upstream chartVersions not be Witself tags, they must match
// the pinned values in .gitops/charts/platform/values.yaml. This catches
// silent drift where a cell values file accidentally lags a platform
// bump (or a cell values file is manually edited to a newer chart
// version without updating platform's default).
func TestUpstreamChartVersionsMatchPlatformDefaults(t *testing.T) {
	root := repoRoot(t)

	platformValues, err := os.ReadFile(filepath.Join(root, ".gitops", "charts", "platform", "values.yaml"))
	if err != nil {
		t.Fatalf("read platform values.yaml: %v", err)
	}

	// Extract expected upstream chart versions from platform defaults.
	// The shape in platform/values.yaml is:
	//   platform:
	//     certManager:
	//       chartVersion: v1.20.3
	// so we walk key blocks and collect chartVersion per named key.
	defaults := extractPlatformDefaults(string(platformValues))
	if len(defaults) == 0 {
		t.Fatalf("no platform chart defaults parsed — parser or platform/values.yaml layout has drifted")
	}

	cellsDir := filepath.Join(root, ".gitops", "cells")
	entries, _ := os.ReadDir(cellsDir)
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		cellName := entry.Name()
		body, err := os.ReadFile(filepath.Join(cellsDir, cellName, "values.yaml"))
		if err != nil {
			continue
		}
		cellDefaults := extractPlatformDefaults(string(body))
		for chart, wantVer := range defaults {
			gotVer, ok := cellDefaults[chart]
			if !ok {
				// Cell may legitimately omit an upstream chart; that's
				// fine — the platform chart's default applies. No error.
				continue
			}
			if gotVer != wantVer {
				t.Errorf("%s: platform.%s.chartVersion=%q, want %q (matching .gitops/charts/platform/values.yaml)",
					cellName, chart, gotVer, wantVer)
			}
		}
	}
}

// extractPlatformDefaults walks a values.yaml file and returns
// map[chartKey] -> chartVersion for every subkey under `platform:` that
// has a chartVersion field. Regex-based, tolerant of the shape our
// values files use today; not a full YAML parser.
func extractPlatformDefaults(src string) map[string]string {
	platformMarker := regexp.MustCompile(`(?m)^platform:\s*$`)
	loc := platformMarker.FindStringIndex(src)
	if loc == nil {
		return nil
	}
	window := src[loc[1]:]

	// A subkey block looks like:
	//   <indent><name>:
	//     chartVersion: <value>
	// where indent is 2 spaces (top-level nested under platform). We
	// pair a subkey name with the FIRST chartVersion that follows it
	// before the next same-indent subkey.
	subkeyRe := regexp.MustCompile(`(?m)^  ([a-zA-Z][a-zA-Z0-9_-]*):\s*$`)
	chartVersionRe := regexp.MustCompile(`(?m)^\s+chartVersion:\s*["']?([^"'\s]+)["']?\s*$`)

	subkeyLocs := subkeyRe.FindAllStringSubmatchIndex(window, -1)
	out := map[string]string{}
	for i, sk := range subkeyLocs {
		name := window[sk[2]:sk[3]]
		blockStart := sk[1]
		blockEnd := len(window)
		if i+1 < len(subkeyLocs) {
			blockEnd = subkeyLocs[i+1][0]
		}
		block := window[blockStart:blockEnd]
		if cv := chartVersionRe.FindStringSubmatch(block); cv != nil {
			out[name] = cv[1]
		}
	}
	return out
}

// repoRoot walks up from the test file until it finds go.mod.
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("could not find go.mod walking upward")
		}
		dir = parent
	}
}
