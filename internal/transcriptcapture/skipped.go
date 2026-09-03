package transcriptcapture

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/witwave-ai/witself/internal/local"
)

const (
	skippedSessionSchemaVersion = 1
	skippedSessionReason        = "ephemeral_session"
	maxSkippedSessionBytes      = 64 * 1024
	maxSkippedSessionMarkers    = 512
	skippedSessionRetention     = 30 * 24 * time.Hour
	skippedSessionLockWait      = 2 * time.Second
	skippedSessionLockPoll      = 5 * time.Millisecond
	skippedSessionLockStale     = 30 * time.Second
)

// SkippedSession is the value-free local audit record for one non-persisted
// Codex session. SessionHash is a truncated SHA-256 digest; no session id or
// transcript content is retained.
type SkippedSession struct {
	SchemaVersion  int               `json:"schema_version"`
	Runtime        string            `json:"runtime"`
	Reason         string            `json:"reason"`
	SessionHash    string            `json:"session_hash"`
	FirstSeen      time.Time         `json:"first_seen"`
	LastSeen       time.Time         `json:"last_seen"`
	Events         uint64            `json:"events"`
	HookEvents     map[string]uint64 `json:"hook_events"`
	RuntimeVersion string            `json:"runtime_version"`
	Model          string            `json:"model"`
}

// SkippedSessions returns valid value-free skip markers ordered by first_seen.
// Corrupt and oversized markers are ignored; a later event for the same
// session replaces one instead of failing capture.
func SkippedSessions(runtime string) ([]SkippedSession, error) {
	dir, err := skippedDir(runtime)
	if err != nil {
		return nil, err
	}
	files, err := filepath.Glob(filepath.Join(dir, "*.json"))
	if err != nil {
		return nil, err
	}
	out := make([]SkippedSession, 0, len(files))
	for _, path := range files {
		marker, valid, err := readSkippedSession(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if !valid || marker.Runtime != filepath.Base(dir) || filepath.Base(path) != marker.SessionHash+".json" {
			continue
		}
		out = append(out, marker)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].FirstSeen.Equal(out[j].FirstSeen) {
			return out[i].SessionHash < out[j].SessionHash
		}
		return out[i].FirstSeen.Before(out[j].FirstSeen)
	})
	return out, nil
}

// QuarantineEphemeral moves queued Codex events without a source transcript
// path out of the upload outbox. It is deliberately a no-op for every other
// runtime. Per-file failures leave the source pending while other files keep
// moving; the returned error joins all reportable failures.
func QuarantineEphemeral(runtime string, pending []PendingEvent) (moved, remaining []PendingEvent, err error) {
	runtime, normalizeErr := NormalizeRuntime(runtime)
	if normalizeErr != nil {
		return nil, append([]PendingEvent(nil), pending...), normalizeErr
	}
	if runtime != RuntimeCodex {
		return nil, append([]PendingEvent(nil), pending...), nil
	}
	dir, dirErr := quarantineDir(runtime)
	if dirErr != nil {
		return nil, append([]PendingEvent(nil), pending...), dirErr
	}
	if dirErr := ensureOwnerOnlyDir(dir); dirErr != nil {
		return nil, append([]PendingEvent(nil), pending...), dirErr
	}

	remaining = make([]PendingEvent, 0, len(pending))
	for _, item := range pending {
		if strings.TrimSpace(item.Event.SourceTranscriptPath) != "" {
			remaining = append(remaining, item)
			continue
		}
		destination, didMove, moveErr := movePendingToQuarantine(runtime, dir, item)
		if !didMove {
			remaining = append(remaining, item)
			if moveErr != nil {
				err = errors.Join(err, moveErr)
			}
			continue
		}
		item.Path = destination
		moved = append(moved, item)
		if moveErr != nil {
			err = errors.Join(err, moveErr)
		}
		hookEvent := canonicalHookEvent(firstNonempty(item.Event.HookEvent, item.Event.NativeHookEvent))
		if markerErr := recordSkippedSession(
			runtime,
			item.Event.SessionID,
			hookEvent,
			item.Event.RuntimeVersion,
			item.Event.Model,
			time.Now().UTC(),
		); markerErr != nil {
			err = errors.Join(err, fmt.Errorf("record skipped session for %s: %w", filepath.Base(destination), markerErr))
		}
	}
	return moved, remaining, err
}

func recordSkippedSession(runtime, sessionID, hookEvent, runtimeVersion, model string, seenAt time.Time) error {
	runtime, err := NormalizeRuntime(runtime)
	if err != nil {
		return err
	}
	if runtime != RuntimeCodex {
		return nil
	}
	seenAt = seenAt.UTC()
	if seenAt.IsZero() {
		seenAt = time.Now().UTC()
	}
	hash := sessionHash(sessionID)
	dir, err := skippedDir(runtime)
	if err != nil {
		return err
	}
	if err := ensureOwnerOnlyDir(dir); err != nil {
		return err
	}
	path := filepath.Join(dir, hash+".json")
	release, err := acquireSkippedSessionLock(path)
	if err != nil {
		return err
	}
	defer release()
	marker, valid, readErr := readSkippedSession(path)
	if readErr != nil && !errors.Is(readErr, os.ErrNotExist) {
		return readErr
	}
	if !valid || marker.Runtime != runtime || marker.SessionHash != hash {
		marker = SkippedSession{
			SchemaVersion: skippedSessionSchemaVersion,
			Runtime:       runtime,
			Reason:        skippedSessionReason,
			SessionHash:   hash,
			FirstSeen:     seenAt,
			HookEvents:    make(map[string]uint64),
		}
	}
	if marker.HookEvents == nil {
		marker.HookEvents = make(map[string]uint64)
	}
	if marker.FirstSeen.IsZero() || seenAt.Before(marker.FirstSeen) {
		marker.FirstSeen = seenAt
	}
	if marker.LastSeen.IsZero() || seenAt.After(marker.LastSeen) {
		marker.LastSeen = seenAt
	}
	if runtimeVersion = truncateUTF8(strings.TrimSpace(runtimeVersion), 256); runtimeVersion != "" || marker.Events == 0 {
		marker.RuntimeVersion = runtimeVersion
	}
	if model = truncateUTF8(strings.TrimSpace(model), 256); model != "" || marker.Events == 0 {
		marker.Model = model
	}
	marker.Events++
	marker.HookEvents[canonicalSkippedHookEvent(hookEvent)]++
	if err := writeJSONAtomic(path, marker); err != nil {
		return err
	}
	pruneSkippedSessions(dir, path, seenAt)
	return nil
}

func readSkippedSession(path string) (SkippedSession, bool, error) {
	linked, err := os.Lstat(path)
	if err != nil {
		return SkippedSession{}, false, err
	}
	if linked.Mode()&os.ModeSymlink != 0 || !linked.Mode().IsRegular() {
		return SkippedSession{}, false, nil
	}
	file, err := os.Open(path)
	if err != nil {
		return SkippedSession{}, false, err
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil {
		return SkippedSession{}, false, err
	}
	if !trustedPathIdentity(path, info) {
		return SkippedSession{}, false, nil
	}
	raw, err := io.ReadAll(io.LimitReader(file, maxSkippedSessionBytes+1))
	if err != nil {
		return SkippedSession{}, false, err
	}
	if len(raw) == 0 || len(raw) > maxSkippedSessionBytes {
		return SkippedSession{}, false, nil
	}
	var marker SkippedSession
	if err := json.Unmarshal(raw, &marker); err != nil || !validSkippedSession(marker) {
		return SkippedSession{}, false, nil
	}
	return marker, true, nil
}

func canonicalSkippedHookEvent(event string) string {
	event = canonicalHookEvent(event)
	switch event {
	case "SessionStart", "SessionEnd", "UserPromptSubmit", "AgentResponse", "AgentThought",
		"Stop", "StopFailure", "PreToolUse", "PostToolUse", "PostToolUseFailure",
		"SubagentStart", "SubagentStop", "PreCompact", "PostCompact", "PermissionRequest",
		"PermissionDenied", "Notification", HookEventCodexPermissionReview:
		return event
	default:
		// Hook names outside the installed canonical vocabulary are data, not
		// suitable keys for a value-free local audit marker.
		return "Unknown"
	}
}

func validSkippedSession(marker SkippedSession) bool {
	if marker.SchemaVersion != skippedSessionSchemaVersion ||
		marker.Runtime != RuntimeCodex || marker.Reason != skippedSessionReason ||
		!validSkippedSessionHash(marker.SessionHash) || marker.FirstSeen.IsZero() || marker.LastSeen.IsZero() ||
		marker.LastSeen.Before(marker.FirstSeen) || marker.Events == 0 || len(marker.HookEvents) == 0 {
		return false
	}
	var events uint64
	for hookEvent, count := range marker.HookEvents {
		if canonicalSkippedHookEvent(hookEvent) != hookEvent || count == 0 || ^uint64(0)-events < count {
			return false
		}
		events += count
	}
	return events == marker.Events
}

func validSkippedSessionHash(value string) bool {
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == 16 && hex.EncodeToString(decoded) == value
}

func acquireSkippedSessionLock(markerPath string) (func(), error) {
	lockPath := markerPath + ".lock"
	deadline := time.Now().Add(skippedSessionLockWait)
	for {
		file, err := os.OpenFile(lockPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err == nil {
			if _, writeErr := fmt.Fprintf(file, "%d\n", os.Getpid()); writeErr != nil {
				_ = file.Close()
				_ = os.Remove(lockPath)
				return nil, writeErr
			}
			if closeErr := file.Close(); closeErr != nil {
				_ = os.Remove(lockPath)
				return nil, closeErr
			}
			return func() { _ = os.Remove(lockPath) }, nil
		}
		if !errors.Is(err, os.ErrExist) {
			return nil, err
		}
		if running, known := flushLockOwnerRunning(lockPath); known && !running {
			_ = os.Remove(lockPath)
			continue
		}
		info, statErr := os.Lstat(lockPath)
		if errors.Is(statErr, os.ErrNotExist) {
			continue
		}
		if statErr != nil {
			return nil, fmt.Errorf("inspect skipped session marker lock: %w", statErr)
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return nil, errors.New("skipped session marker lock is not a regular file")
		}
		if time.Since(info.ModTime()) > skippedSessionLockStale {
			_ = os.Remove(lockPath)
			continue
		}
		if !time.Now().Before(deadline) {
			return nil, errors.New("timed out acquiring skipped session marker lock")
		}
		time.Sleep(skippedSessionLockPoll)
	}
}

func pruneSkippedSessions(dir, currentPath string, now time.Time) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	markers := make([]os.DirEntry, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() && filepath.Ext(entry.Name()) == ".json" {
			markers = append(markers, entry)
		}
	}
	if len(markers) <= maxSkippedSessionMarkers {
		return
	}
	cutoff := now.Add(-skippedSessionRetention)
	for _, entry := range markers {
		path := filepath.Join(dir, entry.Name())
		if path == currentPath {
			continue
		}
		marker, valid, err := readSkippedSession(path)
		if err == nil && valid && marker.LastSeen.Before(cutoff) {
			_ = os.Remove(path)
		}
	}
}

func movePendingToQuarantine(runtime, destinationDir string, item PendingEvent) (destination string, moved bool, err error) {
	if item.Event.Runtime != runtime {
		return "", false, fmt.Errorf("quarantine pending event %s: event runtime %q does not match %q", filepath.Base(item.Path), item.Event.Runtime, runtime)
	}
	if err := validatePendingRewritePath(item.Path, item.Event); err != nil {
		return "", false, fmt.Errorf("quarantine pending event %s: %w", filepath.Base(item.Path), err)
	}
	sourceDir, err := outboxDir(runtime)
	if err != nil {
		return "", false, err
	}
	sourceDir, err = filepath.Abs(sourceDir)
	if err != nil {
		return "", false, err
	}
	source, err := filepath.Abs(item.Path)
	if err != nil {
		return "", false, err
	}
	rel, err := filepath.Rel(sourceDir, source)
	if err != nil || filepath.Dir(rel) != "." || filepath.Ext(rel) != ".json" {
		return "", false, fmt.Errorf("quarantine pending event %s: path is outside the runtime outbox", filepath.Base(item.Path))
	}
	info, err := os.Lstat(source)
	if err != nil {
		return "", false, fmt.Errorf("quarantine pending event %s: %w", filepath.Base(item.Path), err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || !trustedPathIdentity(source, info) {
		return "", false, fmt.Errorf("quarantine pending event %s: source is not a trusted regular file", filepath.Base(item.Path))
	}
	destination = filepath.Join(destinationDir, filepath.Base(source))
	if _, err := os.Lstat(destination); err == nil {
		return "", false, fmt.Errorf("quarantine pending event %s: destination already exists", filepath.Base(item.Path))
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", false, fmt.Errorf("quarantine pending event %s: inspect destination: %w", filepath.Base(item.Path), err)
	}
	if err := os.Chmod(source, 0o600); err != nil {
		return "", false, fmt.Errorf("secure pending event %s before quarantine: %w", filepath.Base(item.Path), err)
	}
	if renameErr := os.Rename(source, destination); renameErr == nil {
		return destination, true, nil
	} else if !errors.Is(renameErr, syscall.EXDEV) {
		return "", false, fmt.Errorf("quarantine pending event %s: %w", filepath.Base(item.Path), renameErr)
	}
	if err := copyThenRemove(source, destination); err != nil {
		return "", false, fmt.Errorf("quarantine pending event %s across filesystems: %w", filepath.Base(item.Path), err)
	}
	return destination, true, nil
}

func copyThenRemove(source, destination string) error {
	src, err := os.Open(source)
	if err != nil {
		return err
	}
	defer func() { _ = src.Close() }()
	dst, err := os.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	copyOK := false
	defer func() {
		_ = dst.Close()
		if !copyOK {
			_ = os.Remove(destination)
		}
	}()
	if _, err := io.Copy(dst, src); err != nil {
		return err
	}
	if err := dst.Sync(); err != nil {
		return err
	}
	if err := dst.Close(); err != nil {
		return err
	}
	if err := src.Close(); err != nil {
		return err
	}
	if err := os.Remove(source); err != nil {
		return err
	}
	copyOK = true
	return nil
}

func skippedDir(runtime string) (string, error) {
	return captureRuntimeDir("skipped", runtime)
}

func quarantineDir(runtime string) (string, error) {
	return captureRuntimeDir("quarantine", runtime)
}

func captureRuntimeDir(kind, runtime string) (string, error) {
	runtime, err := NormalizeRuntime(runtime)
	if err != nil {
		return "", err
	}
	home, err := local.Home()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "capture", kind, runtime), nil
}

func ensureOwnerOnlyDir(path string) error {
	if err := os.MkdirAll(path, 0o700); err != nil {
		return err
	}
	return os.Chmod(path, 0o700)
}
