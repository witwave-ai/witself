package main

// Plan-state persistence: previewSeen (which cells have a passed
// preview arming `u`) survives dashboard restarts, with two guards.
// First, a staleness window — the diff the operator saw goes stale as
// the repo and cloud drift, so entries expire after previewTTL whether
// or not the process restarted. Second, a config binding — each entry
// records a fingerprint of the config file it was previewed against,
// and planArmed refuses to fire if that no longer matches the loaded
// config. Without the binding, a preview run under one -config (or
// before a config edit that retargets the cell) could arm `u` for a
// different target — the exact wrong-cell/wrong-account footgun the
// preview gate exists to prevent.

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"time"
)

// previewTTL is how long a passed preview keeps `u` armed. Long
// enough to survive a restart or a coffee break, short enough that
// the diff the operator saw still resembles what up would apply.
const previewTTL = 60 * time.Minute

// planEntry is one persisted preview: when it passed, and a
// fingerprint of the config it ran against.
type planEntry struct {
	At           time.Time `json:"at"`
	ConfigFinger string    `json:"config_finger"`
}

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

// configFileFingerprint hashes the config's absolute path together
// with its full contents. The path component distinguishes two
// configs that happen to have identical bytes; the content component
// invalidates any plan when the file is edited (including a change to
// the defaults block a cell inherits without its own entry changing).
// An unreadable file yields "" — which never equals a real
// fingerprint, so plans fail closed.
func configFileFingerprint(configPath string) string {
	abs, err := filepath.Abs(configPath)
	if err != nil {
		abs = configPath
	}
	raw, err := os.ReadFile(configPath)
	if err != nil {
		return ""
	}
	h := sha256.New()
	h.Write([]byte(abs))
	h.Write([]byte{0})
	h.Write(raw)
	return hex.EncodeToString(h.Sum(nil))
}

// loadPlanState reads the persisted preview entries. Tolerant by
// design: a missing or corrupt file yields an empty map — the worst
// outcome is the operator re-runs a preview, never a blocked startup.
// Entries already past previewTTL are dropped on load. The config
// fingerprint is NOT checked here (the loaded config isn't known yet);
// planArmed enforces it at use time.
func loadPlanState(path string, now time.Time) map[string]planEntry {
	out := map[string]planEntry{}
	raw, err := os.ReadFile(path)
	if err != nil {
		return out
	}
	var stored map[string]planEntry
	if err := json.Unmarshal(raw, &stored); err != nil {
		return out
	}
	for cell, pe := range stored {
		if now.Sub(pe.At) < previewTTL {
			out[cell] = pe
		}
	}
	return out
}

// savePlanState writes the preview entries atomically (temp file +
// rename) so a crash mid-write can't leave a torn file. Expired
// entries are pruned on the way out. Errors are swallowed — plan
// persistence is a convenience, not a correctness requirement, and
// the in-memory map stays authoritative for the running session.
func savePlanState(path string, plans map[string]planEntry, now time.Time) {
	if path == "" {
		return
	}
	pruned := map[string]planEntry{}
	for cell, pe := range plans {
		if now.Sub(pe.At) < previewTTL {
			pruned[cell] = pe
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

// planArmed reports whether a cell's preview is recent enough AND was
// run against the currently-loaded config. This is THE gate — the
// render (◆ mark, context plan line), the footer hint, the u-key
// refusal, and the confirm dialog all route through it, so display and
// behavior can't disagree. The TTL applies to a dashboard left open
// just as it does across restarts; the fingerprint check catches a
// different -config or a mid-session config edit.
func (m dashboardModel) planArmed(cell string) bool {
	pe, ok := m.previewSeen[cell]
	if !ok || m.now().Sub(pe.At) >= previewTTL {
		return false
	}
	return pe.ConfigFinger == m.configFinger
}

// planAge returns how long ago the cell's preview passed (zero, false
// when there's no entry at all). Note this reports age even for a plan
// whose config fingerprint no longer matches — the caller pairs it
// with planArmed to tell "expired" from "stale config" from "armed".
func (m dashboardModel) planAge(cell string) (time.Duration, bool) {
	pe, ok := m.previewSeen[cell]
	if !ok {
		return 0, false
	}
	return m.now().Sub(pe.At), true
}
