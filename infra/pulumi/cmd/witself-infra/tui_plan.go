package main

// Plan-state persistence: previewSeen (which cells have a passed
// preview arming `u`) survives dashboard restarts, with a staleness
// window. The preview gate exists so an operator has SEEN the diff
// before applying it — that knowledge doesn't evaporate on restart,
// but it does go stale as the repo and the cloud drift, so entries
// expire after previewTTL whether or not the process restarted.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"
)

// previewTTL is how long a passed preview keeps `u` armed. Long
// enough to survive a restart or a coffee break, short enough that
// the diff the operator saw still resembles what up would apply.
const previewTTL = 60 * time.Minute

// witselfHomeDir resolves the state root: $WITSELF_HOME, else
// ~/.witself, else the system temp dir. Shared by op logs and plan
// state so everything lands under one roof.
func witselfHomeDir() string {
	root := os.Getenv("WITSELF_HOME")
	if root == "" {
		if home, err := os.UserHomeDir(); err == nil {
			root = filepath.Join(home, ".witself")
		} else {
			root = os.TempDir()
		}
	}
	return root
}

func planStatePath() string {
	return filepath.Join(witselfHomeDir(), "state", "infra-previews.json")
}

// loadPlanState reads the persisted preview timestamps. Tolerant by
// design: a missing or corrupt file yields an empty map — the worst
// outcome is the operator re-runs a preview, never a blocked startup.
// Entries already past previewTTL are dropped on load.
func loadPlanState(path string, now time.Time) map[string]time.Time {
	out := map[string]time.Time{}
	raw, err := os.ReadFile(path)
	if err != nil {
		return out
	}
	var stored map[string]time.Time
	if err := json.Unmarshal(raw, &stored); err != nil {
		return out
	}
	for cell, ts := range stored {
		if now.Sub(ts) < previewTTL {
			out[cell] = ts
		}
	}
	return out
}

// savePlanState writes the preview timestamps atomically (temp file +
// rename) so a crash mid-write can't leave a torn file. Expired
// entries are pruned on the way out. Errors are swallowed — plan
// persistence is a convenience, not a correctness requirement, and
// the in-memory map stays authoritative for the running session.
func savePlanState(path string, plans map[string]time.Time, now time.Time) {
	if path == "" {
		return
	}
	pruned := map[string]time.Time{}
	for cell, ts := range plans {
		if now.Sub(ts) < previewTTL {
			pruned[cell] = ts
		}
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return
	}
	raw, err := json.MarshalIndent(pruned, "", "  ")
	if err != nil {
		return
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return
	}
	_ = os.Rename(tmp, path)
}

// planArmed reports whether a cell's preview is recent enough to arm
// `u`. This is THE gate — the render (◆ mark, context plan line), the
// footer hint, the u-key refusal, and the confirm dialog all route
// through it, so display and behavior can't disagree. The TTL applies
// to a dashboard left open just as it does across restarts.
func (m dashboardModel) planArmed(cell string) bool {
	ts, ok := m.previewSeen[cell]
	return ok && m.now().Sub(ts) < previewTTL
}

// planAge returns how long ago the cell's preview passed (zero, false
// when there's no live plan).
func (m dashboardModel) planAge(cell string) (time.Duration, bool) {
	ts, ok := m.previewSeen[cell]
	if !ok {
		return 0, false
	}
	return m.now().Sub(ts), true
}
