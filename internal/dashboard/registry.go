package dashboard

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"github.com/witwave-ai/witself/internal/local"
)

// RegistrySchemaVersion marks every durable dashboard registry file.
const RegistrySchemaVersion = "witself.dashboard.v1"

// MarkerHeader is the response header every serving dashboard tags its
// responses with; the registry liveness probe accepts any HTTP response
// carrying it.
const MarkerHeader = markerHeader

// agentIDPattern accepts agent ids (agt_..., with underscores) as safe path
// components; local.namePattern would reject them.
var agentIDPattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,127}$`)

// RegistryEntry is the discovery record one serving dashboard writes to
// ~/.witself/dashboards/<agentID>.json so local tooling can find it.
// AccessURL carries the tokened open URL so tooling that did not start the
// serve can still open it; the 0600 file's same-user exposure is identical
// to the agent token file, which already grants the underlying reads (ADR
// 0004).
type RegistryEntry struct {
	SchemaVersion string    `json:"schema_version"`
	AgentID       string    `json:"agent_id"`
	AgentName     string    `json:"agent_name"`
	Account       string    `json:"account"`
	Realm         string    `json:"realm"`
	Port          int       `json:"port"`
	PID           int       `json:"pid"`
	URL           string    `json:"url"`
	AccessURL     string    `json:"access_url"`
	StartedAt     time.Time `json:"started_at"`
}

// DefaultPort maps an agent id onto a stable port in [50000, 59999]: the
// first eight bytes of sha256(agentID), mod 10000 (ADR 0004). Stable ports
// keep bookmarks and multi-agent muscle memory working.
func DefaultPort(agentID string) int {
	sum := sha256.Sum256([]byte(agentID))
	return 50000 + int(binary.BigEndian.Uint64(sum[:8])%10000)
}

// registryDir returns the directory every serving dashboard registers in.
func registryDir() (string, error) {
	home, err := local.Home()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "dashboards"), nil
}

// RegistryPath returns the registry file for one agent id, refusing any id
// that is not a safe path component.
func RegistryPath(agentID string) (string, error) {
	if !agentIDPattern.MatchString(agentID) {
		return "", fmt.Errorf("dashboard: invalid agent id %q", agentID)
	}
	dir, err := registryDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, agentID+".json"), nil
}

// WriteRegistryEntry durably records a serving dashboard (0700 directory,
// 0600 file, temp+rename).
func WriteRegistryEntry(entry RegistryEntry) error {
	entry.SchemaVersion = RegistrySchemaVersion
	path, err := RegistryPath(entry.AgentID)
	if err != nil {
		return err
	}
	return writeJSONAtomic(path, entry)
}

// ClaimRegistryEntry atomically claims the agent's registry slot for a
// dashboard already answering on entry.Port. A sibling flock file serializes
// racing serves, so the liveness check and the write are one critical
// section: exactly one of two concurrent claims wins, and the loser gets the
// survivor's entry back. The caller must already be serving before claiming
// — the loser's probe of the winner has to see the marker header — and an
// existing entry recording the claimant's own port is stale by construction,
// because no other process can answer on a port this process has bound.
func ClaimRegistryEntry(entry RegistryEntry) (RegistryEntry, bool, error) {
	path, err := RegistryPath(entry.AgentID)
	if err != nil {
		return RegistryEntry{}, false, err
	}
	unlock, err := lockRegistryClaim(path + ".lock")
	if err != nil {
		return RegistryEntry{}, false, err
	}
	defer unlock()
	raw, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return RegistryEntry{}, false, err
	}
	if err == nil {
		// A corrupt file is stale, never live — the same verdict as
		// LiveRegistryEntry — and so is an entry recording our own port.
		var existing RegistryEntry
		if json.Unmarshal(raw, &existing) == nil && existing.Port != entry.Port && EntryLive(existing) {
			return existing, false, nil
		}
	}
	if err := WriteRegistryEntry(entry); err != nil {
		return RegistryEntry{}, false, err
	}
	return entry, true, nil
}

// lockRegistryClaim takes the exclusive advisory lock guarding one agent's
// registry slot, blocking until it is free (the critical section is a
// bounded liveness probe plus one atomic write). The lock file persists
// after release; it holds no state.
func lockRegistryClaim(path string) (func(), error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := unix.Flock(int(file.Fd()), unix.LOCK_EX); err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("dashboard: lock registry claim %s: %w", path, err)
	}
	return func() {
		_ = unix.Flock(int(file.Fd()), unix.LOCK_UN)
		_ = file.Close()
	}, nil
}

// ReadRegistryEntry loads one agent's registry file.
func ReadRegistryEntry(agentID string) (RegistryEntry, error) {
	path, err := RegistryPath(agentID)
	if err != nil {
		return RegistryEntry{}, err
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return RegistryEntry{}, err
	}
	var entry RegistryEntry
	if err := json.Unmarshal(raw, &entry); err != nil {
		return RegistryEntry{}, fmt.Errorf("dashboard: decode %s: %w", path, err)
	}
	return entry, nil
}

// RemoveRegistryEntry deletes the agent's registry file. A missing file is
// not an error, so graceful shutdown stays idempotent.
func RemoveRegistryEntry(agentID string) error {
	path, err := RegistryPath(agentID)
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// ReleaseRegistryEntry deletes the agent's registry file only when the entry
// on disk still belongs to ownerPID, so a shutting-down process that lost a
// startup race never deletes the surviving dashboard's discovery record. A
// missing file is not an error; a file owned by another PID is reported and
// left in place.
func ReleaseRegistryEntry(agentID string, ownerPID int) error {
	entry, err := ReadRegistryEntry(agentID)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		// Unreadable or corrupt: nothing trustworthy to preserve.
		return RemoveRegistryEntry(agentID)
	}
	if entry.PID != ownerPID {
		return fmt.Errorf("dashboard: registry entry for agent %q is owned by pid %d, not %d; leaving it in place",
			agentID, entry.PID, ownerPID)
	}
	return RemoveRegistryEntry(agentID)
}

// LiveRegistryEntry reports whether a still-running dashboard already owns
// this agent's registry slot. A missing, corrupt, or stale entry is not
// live; the caller may overwrite it.
func LiveRegistryEntry(agentID string) (RegistryEntry, bool, error) {
	path, err := RegistryPath(agentID)
	if err != nil {
		return RegistryEntry{}, false, err
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return RegistryEntry{}, false, nil
		}
		return RegistryEntry{}, false, err
	}
	var entry RegistryEntry
	if err := json.Unmarshal(raw, &entry); err != nil {
		// A corrupt registry file is stale, not live; the next serve
		// overwrites it.
		return RegistryEntry{}, false, nil
	}
	return entry, EntryLive(entry), nil
}

// EntryLive reports whether the entry describes a still-serving dashboard.
// PID liveness alone is not proof — the PID may have been reused by an
// unrelated process after a crash (and a permission error maps
// reused-by-root to "running") — so a live verdict also requires an HTTP
// probe of the recorded loopback port answering with the dashboard marker
// header.
func EntryLive(entry RegistryEntry) bool {
	if running, known := pidRunning(entry.PID); !known || !running {
		return false
	}
	return dashboardResponds(entry.Port)
}

// EntryOwned reports whether the dashboard answering on the entry's recorded
// port is the process that wrote the entry. EntryLive's marker probe proves
// only that some witself dashboard answers there — after a crash, another
// agent's serve (derived-port fallback collision, or an explicit --port) can
// occupy the recorded port while the recorded PID is reused by an unrelated
// process — so a caller about to signal entry.PID needs this stronger
// verdict. The entry's own tokened AccessURL settles it with no new surface:
// the serve that minted the token answers the one-time ?token= exchange with
// 303 See Other, while every other dashboard rejects the foreign token with
// 401.
func EntryOwned(entry RegistryEntry) bool {
	if entry.AccessURL == "" {
		return false
	}
	probe := &http.Client{
		Timeout: time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	resp, err := probe.Get(entry.AccessURL)
	if err != nil {
		return false
	}
	defer func() { _ = resp.Body.Close() }()
	return resp.Header.Get(markerHeader) != "" && resp.StatusCode == http.StatusSeeOther
}

// ListRegistryEntries loads every readable registry entry, sorted by agent
// name then id for stable output. A missing registry directory means no
// dashboards; corrupt files are stale (never live), so the scan skips them
// the same way LiveRegistryEntry does.
func ListRegistryEntries() ([]RegistryEntry, error) {
	dir, err := registryDir()
	if err != nil {
		return nil, err
	}
	files, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var entries []RegistryEntry
	for _, file := range files {
		if file.IsDir() || filepath.Ext(file.Name()) != ".json" {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, file.Name()))
		if err != nil {
			continue
		}
		var entry RegistryEntry
		if err := json.Unmarshal(raw, &entry); err != nil {
			continue
		}
		entries = append(entries, entry)
	}
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].AgentName != entries[j].AgentName {
			return entries[i].AgentName < entries[j].AgentName
		}
		return entries[i].AgentID < entries[j].AgentID
	})
	return entries, nil
}

// dashboardResponds reports whether a witself dashboard answers on the given
// loopback port: any HTTP response carrying the marker header counts (an
// unauthenticated probe gets 401, which is fine). Connection failures,
// timeouts, and foreign services are all "not a dashboard".
func dashboardResponds(port int) bool {
	if port <= 0 || port > 65535 {
		return false
	}
	probe := &http.Client{Timeout: time.Second}
	resp, err := probe.Get(fmt.Sprintf("http://127.0.0.1:%d/", port))
	if err != nil {
		return false
	}
	defer func() { _ = resp.Body.Close() }()
	return resp.Header.Get(markerHeader) != ""
}

// pidRunning mirrors transcriptcapture's flush-lock liveness probe: signal 0,
// treating permission errors as alive and any unusable pid as unknown.
func pidRunning(pid int) (running, known bool) {
	if pid <= 0 {
		return false, false
	}
	process, err := os.FindProcess(pid)
	if err != nil {
		return false, true
	}
	err = process.Signal(syscall.Signal(0))
	return err == nil || os.IsPermission(err), true
}

// writeJSONAtomic is this package's private copy of the repo-conventional
// atomic JSON writer (internal/transcriptcapture/config.go shape).
func writeJSONAtomic(path string, value any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer func() { _ = os.Remove(tmpPath) }()
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(append(raw, '\n')); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpPath, path)
}
