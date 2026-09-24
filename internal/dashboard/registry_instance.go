package dashboard

import (
	"context"
	"errors"
	"io"
	"os"
)

// SameRegistryInstance compares the complete private discovery fence. PID
// alone is insufficient when a process or port is reused.
func SameRegistryInstance(a, b RegistryEntry) bool {
	return sameRegistryManager(a.Manager, b.Manager) && a.ViewerContract == b.ViewerContract && a.SchemaVersion == b.SchemaVersion && a.AgentID == b.AgentID &&
		a.PID == b.PID && a.Port == b.Port && a.StartedAt.Equal(b.StartedAt) &&
		a.URL == b.URL && a.AccessURL == b.AccessURL && a.AccountID == b.AccountID &&
		a.RealmID == b.RealmID && a.Endpoint == b.Endpoint && a.Account == b.Account &&
		a.Realm == b.Realm && a.AgentName == b.AgentName
}

// ReadRegistryInstance reads only one bounded regular registry record. It does
// not resolve aliases, scan accounts, probe a URL, or repair malformed records.
func ReadRegistryInstance(agentID string) (RegistryEntry, error) {
	path, err := RegistryPath(agentID)
	if err != nil {
		return RegistryEntry{}, err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return RegistryEntry{}, err
	}
	if !info.Mode().IsRegular() || info.Size() > 16384 {
		return RegistryEntry{}, errors.New("dashboard registry record is invalid")
	}
	file, err := os.Open(path)
	if err != nil {
		return RegistryEntry{}, err
	}
	defer func() { _ = file.Close() }()
	opened, err := file.Stat()
	if err != nil || !os.SameFile(info, opened) {
		return RegistryEntry{}, errors.New("dashboard registry record changed")
	}
	raw, err := io.ReadAll(io.LimitReader(file, 16385))
	if err != nil || len(raw) > 16384 {
		return RegistryEntry{}, errors.New("dashboard registry record is unreadable")
	}
	var entry RegistryEntry
	if err := decodeRegistryRecord(raw, &entry); err != nil {
		return RegistryEntry{}, err
	}
	if entry.SchemaVersion != RegistrySchemaVersion || entry.AgentID != agentID {
		return RegistryEntry{}, errors.New("dashboard registry identity is invalid")
	}
	return entry, nil
}

// WithRegistryInstance holds the claim lock while re-reading the exact private
// fence and performing one bounded action. A missing or replaced record never
// authorizes the action. Callbacks must not acquire the same lock.
func WithRegistryInstance(ctx context.Context, entry RegistryEntry, action func() error) (bool, error) {
	path, err := RegistryPath(entry.AgentID)
	if err != nil {
		return false, err
	}
	unlock, err := lockRegistryClaimContext(ctx, path+".lock")
	if err != nil {
		return false, err
	}
	defer unlock()
	current, err := ReadRegistryInstance(entry.AgentID)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !SameRegistryInstance(current, entry) {
		return false, nil
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	return true, action()
}

// ReleaseRegistryInstance removes only this exact instance under the same
// lock used by claims. Corrupt, unreadable and successor records survive.
func ReleaseRegistryInstance(ctx context.Context, entry RegistryEntry) (bool, error) {
	return WithRegistryInstance(ctx, entry, func() error { return RemoveRegistryEntry(entry.AgentID) })
}

// ClaimManagedRegistryEntry claims an empty slot without probing untrusted
// existing records or overwriting them. The parent verifies any race winner.
func ClaimManagedRegistryEntry(ctx context.Context, entry RegistryEntry) (bool, error) {
	path, err := RegistryPath(entry.AgentID)
	if err != nil {
		return false, err
	}
	unlock, err := lockRegistryClaimContext(ctx, path+".lock")
	if err != nil {
		return false, err
	}
	defer unlock()
	if _, err := os.Lstat(path); err == nil {
		return false, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return false, err
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	return true, WriteRegistryEntry(entry)
}

// RegistryPIDRunning checks process existence without probing any URL or
// delivering a shutdown request. An unknown result is never treated as live.
func RegistryPIDRunning(pid int) bool {
	running, known := pidRunning(pid)
	return known && running
}

func sameRegistryManager(a, b *ViewerBinding) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

// PublicRegistryEntry removes opening authority from manager session output,
// including stale sessions. Never mutate the private on-disk fence.
func PublicRegistryEntry(entry RegistryEntry) RegistryEntry {
	if entry.Manager != nil {
		entry.AccessURL = ""
	}
	return entry
}
