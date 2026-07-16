// Package legacyrunnercleanup removes the retired client-local autonomous
// message runner without touching Witself's durable message or request state.
package legacyrunnercleanup

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

const (
	// PlatformDarwin selects the retired per-user launchd integration.
	PlatformDarwin = "darwin"
	// PlatformLinux selects the retired per-user systemd integration.
	PlatformLinux = "linux"

	managedMarker = "witself-managed-message-runner-v1"
	stateSchema   = "witself.message-runner-state.v1"
	markerName    = "foreground-messaging-v1"
	maxStateBytes = 4 * 1024 * 1024
)

var (
	// ErrUnownedService protects a file that does not exactly match the retired
	// Witself-managed service contract.
	ErrUnownedService = errors.New("legacy message runner service file is not owned by Witself")
	// ErrServiceNotLoaded is the narrow idempotent launchd result accepted while
	// removing a definition that is no longer loaded.
	ErrServiceNotLoaded = errors.New("legacy message runner service is not loaded")
	// ErrServiceManagerUnavailable identifies a missing per-user service-manager
	// session. It is safe to ignore only for a state-only installation with no
	// service definition; an owned definition still requires verified retirement.
	ErrServiceManagerUnavailable = errors.New("legacy message runner service manager is unavailable")
	// ErrPendingNotifications prevents an upgrade from silently discarding
	// pointers to canonical messages that the retired runner already acknowledged.
	ErrPendingNotifications = errors.New("legacy message runner has pending local notification pointers")
	// ErrStatePreserved reports that an owned service was retired but malformed
	// or non-private local state was kept for explicit inspection or force purge.
	ErrStatePreserved = errors.New("legacy message runner state was preserved")
)

// Runtimes returns every canonical runtime slug accepted by the retired runner.
func Runtimes() []string {
	return []string{"codex", "claude-code", "grok-build", "cursor"}
}

// Command runs one operating-system service-manager command.
type Command func(context.Context, string, ...string) error

// Cleaner owns the narrow local migration boundary. RemoveAll is injectable so
// tests can prove that credentials and state are purged only after deactivation.
type Cleaner struct {
	Platform    string
	UserHome    string
	ConfigHome  string
	WitselfHome string
	UID         int
	Run         Command
	RemoveAll   func(string) error
	// Force explicitly permits discarding pending local notification pointers.
	// Canonical messages remain in Postgres, but the retired runner may already
	// have acknowledged them, so this is never the default.
	Force bool
	// AllowLoadedWithoutDefinition is set only by the removed serve-command
	// tombstone. That invocation proves the currently running exact unit command;
	// ordinary cleanup remains fail-closed without an owned definition file.
	AllowLoadedWithoutDefinition bool
}

// Result contains paths and booleans only; it never exposes runner credentials,
// message pointers, configuration values, or message content.
type Result struct {
	Runtime              string `json:"runtime"`
	ServicePath          string `json:"service_path"`
	ServiceRemoved       bool   `json:"service_removed"`
	StatePath            string `json:"state_path"`
	StatePurged          bool   `json:"state_purged"`
	StateScrubbed        bool   `json:"state_scrubbed"`
	PendingNotifications int    `json:"pending_notifications"`
}

// Default resolves the retired per-user service and WITSELF_HOME locations.
func Default() (Cleaner, error) {
	userHome, err := os.UserHomeDir()
	if err != nil {
		return Cleaner{}, err
	}
	userHome = filepath.Clean(userHome)
	configHome := strings.TrimSpace(os.Getenv("XDG_CONFIG_HOME"))
	if configHome == "" {
		configHome = filepath.Join(userHome, ".config")
	}
	witselfHome := strings.TrimSpace(os.Getenv("WITSELF_HOME"))
	if witselfHome == "" {
		witselfHome = filepath.Join(userHome, ".witself")
	} else {
		witselfHome, err = filepath.Abs(witselfHome)
		if err != nil {
			return Cleaner{}, err
		}
	}
	for label, path := range map[string]string{
		"user home": userHome, "config home": configHome, "Witself home": witselfHome,
	} {
		if !filepath.IsAbs(path) || filepath.Clean(path) != path || strings.ContainsAny(path, "\x00\r\n") {
			return Cleaner{}, fmt.Errorf("legacy message runner %s must be a clean absolute path", label)
		}
	}
	return Cleaner{
		Platform: runtime.GOOS, UserHome: userHome, ConfigHome: configHome,
		WitselfHome: witselfHome, UID: os.Getuid(), Run: runCommand,
		RemoveAll: os.RemoveAll,
	}, nil
}

// Cleanup removes one retired runtime binding. It first proves ownership of the
// exact service definition, then deactivates that exact unit, removes only that
// definition, and finally purges its local runner directory. PostgreSQL is never
// contacted: durable messages, requests, deliveries, and leases are untouched.
func (c Cleaner) Cleanup(ctx context.Context, runtimeName string) (Result, error) {
	if err := c.validate(ctx, runtimeName); err != nil {
		return Result{}, err
	}
	result := c.result(runtimeName)
	exists, owned, err := inspectServiceFile(result.ServicePath)
	if err != nil {
		return Result{}, err
	}
	if exists && !owned {
		return Result{}, fmt.Errorf("%w: %s", ErrUnownedService, result.ServicePath)
	}
	loaded, err := c.serviceLoaded(ctx, runtimeName)
	if err != nil {
		if !exists && errors.Is(err, ErrServiceManagerUnavailable) {
			loaded = false
		} else {
			return Result{}, err
		}
	}
	if loaded && !exists && !c.AllowLoadedWithoutDefinition {
		return Result{}, fmt.Errorf(
			"%w: loaded unit has no owned definition at %s; verify that the loaded command is the retired Witself runner, then run `%s`",
			ErrUnownedService, result.ServicePath, c.manualRetirementCommand(runtimeName),
		)
	}
	if exists || loaded {
		// Prevent a restart before removing the definition. This order also lets
		// the removed serve-command tombstone clean up its own unit: stopping a
		// job may terminate this process before it can execute another command.
		if err := c.disable(ctx, runtimeName); err != nil {
			return Result{}, err
		}
		// Recheck ownership immediately before removal. A changed definition is
		// never removed even if it passed the earlier preflight.
		if err := removeOwnedServiceFile(result.ServicePath); err != nil {
			return Result{}, err
		}
		result.ServiceRemoved = true
		if loaded {
			if err := c.stop(ctx, runtimeName); err != nil {
				return result, err
			}
		}
		if c.Platform == PlatformLinux {
			if err := c.Run(ctx, "systemctl", "--user", "daemon-reload"); err != nil {
				return Result{}, fmt.Errorf("reload systemd after legacy message runner removal: %w", err)
			}
		}
	}
	result.PendingNotifications, err = pendingNotificationCount(result.StatePath)
	if err != nil {
		if c.Force {
			if removeErr := c.purgeState(ctx, result.StatePath); removeErr != nil {
				return result, fmt.Errorf("force purge unreadable legacy message runner state: %w", removeErr)
			}
			result.StatePurged = true
			return result, nil
		}
		// A malformed state file is not authority to keep credentials or other
		// obsolete adapter files. Scrub everything except state.json whenever the
		// enclosing directory is private; otherwise preserve it byte-for-byte.
		if scrubErr := c.scrubStateExceptNotifications(result.StatePath); scrubErr == nil {
			result.StateScrubbed = true
		} else {
			return result, fmt.Errorf("%w: %s: %v (state scrub skipped: %v)", ErrStatePreserved, result.StatePath, err, scrubErr)
		}
		return result, fmt.Errorf("%w: %s: %v", ErrStatePreserved, result.StatePath, err)
	}
	if result.PendingNotifications > 0 && !c.Force {
		if err := c.scrubStateExceptNotifications(result.StatePath); err != nil {
			return result, fmt.Errorf("scrub retired runner credentials while preserving notification pointers: %w", err)
		}
		result.StateScrubbed = true
		return result, fmt.Errorf(
			"%w: runtime %s has %d pointer(s) in %s; inspect the private state or rerun with --force to discard them",
			ErrPendingNotifications, runtimeName, result.PendingNotifications, result.StatePath,
		)
	}
	if err := c.purgeState(ctx, result.StatePath); err != nil {
		return Result{}, fmt.Errorf("purge legacy message runner state: %w", err)
	}
	result.StatePurged = true
	return result, nil
}

func (c Cleaner) purgeState(ctx context.Context, statePath string) error {
	// A just-stopped runner can briefly recreate or release a lock file while
	// RemoveAll walks the directory. Retry within the caller's bounded context;
	// permanent permission or ownership failures still surface unchanged.
	const attempts = 3
	var lastErr error
	for attempt := 0; attempt < attempts; attempt++ {
		err := c.RemoveAll(statePath)
		if err == nil {
			return nil
		}
		lastErr = err
		if attempt == attempts-1 {
			break
		}
		timer := time.NewTimer(50 * time.Millisecond)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			return errors.Join(ctx.Err(), lastErr)
		case <-timer.C:
		}
	}
	return lastErr
}

func (c Cleaner) scrubStateExceptNotifications(stateRoot string) error {
	info, err := os.Lstat(stateRoot)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("legacy message runner state directory %s must be private", stateRoot)
	}
	entries, err := os.ReadDir(stateRoot)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.Name() == "state.json" {
			continue
		}
		if err := c.RemoveAll(filepath.Join(stateRoot, entry.Name())); err != nil {
			return err
		}
	}
	return nil
}

// CleanupAll preflights every definition before changing any runtime, so one
// unowned file prevents a partial multi-runtime cleanup.
func (c Cleaner) CleanupAll(ctx context.Context) ([]Result, error) {
	for _, runtimeName := range Runtimes() {
		if err := c.validate(ctx, runtimeName); err != nil {
			return nil, err
		}
		path := c.result(runtimeName).ServicePath
		exists, owned, err := inspectServiceFile(path)
		if err != nil {
			return nil, err
		}
		if exists && !owned {
			return nil, fmt.Errorf("%w: %s", ErrUnownedService, path)
		}
		loaded, err := c.serviceLoaded(ctx, runtimeName)
		if err != nil {
			if !exists && errors.Is(err, ErrServiceManagerUnavailable) {
				loaded = false
			} else {
				return nil, err
			}
		}
		if loaded && !exists && !c.AllowLoadedWithoutDefinition {
			return nil, fmt.Errorf(
				"%w: loaded unit has no owned definition at %s; verify that the loaded command is the retired Witself runner, then run `%s`",
				ErrUnownedService, path, c.manualRetirementCommand(runtimeName),
			)
		}
	}
	results := make([]Result, 0, len(Runtimes()))
	var preservedErrors []error
	for _, runtimeName := range Runtimes() {
		result, err := c.Cleanup(ctx, runtimeName)
		if err != nil {
			results = append(results, result)
			if errors.Is(err, ErrPendingNotifications) || errors.Is(err, ErrStatePreserved) {
				preservedErrors = append(preservedErrors, err)
				continue
			}
			return results, err
		}
		results = append(results, result)
	}
	return results, errors.Join(preservedErrors...)
}

// HasArtifacts is a filesystem-only startup fast path. It deliberately avoids
// contacting a service manager on hosts that never installed the retired
// runner. A live removed-definition unit reaches the separate serve tombstone.
func (c Cleaner) HasArtifacts() (bool, error) {
	for _, runtimeName := range Runtimes() {
		result := c.result(runtimeName)
		for _, path := range []string{result.ServicePath, result.StatePath} {
			if _, err := os.Lstat(path); err == nil {
				return true, nil
			} else if !errors.Is(err, os.ErrNotExist) {
				return false, err
			}
		}
	}
	return false, nil
}

// Completed reports whether this one-time upgrade cleanup already finished.
func (c Cleaner) Completed() (bool, error) {
	path := c.completionMarkerPath()
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
		return false, fmt.Errorf("legacy runner cleanup marker %s must be a private regular file", path)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return false, err
	}
	if string(raw) != "completed\n" {
		return false, fmt.Errorf("legacy runner cleanup marker %s has unexpected content", path)
	}
	return true, nil
}

// MarkCompleted records successful cleanup so normal CLI startup does not
// repeatedly query launchd or systemd after the one-time upgrade migration.
func (c Cleaner) MarkCompleted() error {
	path := c.completionMarkerPath()
	if complete, err := c.Completed(); err != nil || complete {
		return err
	}
	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return err
	}
	info, err := os.Lstat(directory)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("legacy runner cleanup marker directory %s must be private", directory)
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if errors.Is(err, os.ErrExist) {
		complete, verifyErr := c.Completed()
		if verifyErr != nil {
			return verifyErr
		}
		if complete {
			return nil
		}
	}
	if err != nil {
		return err
	}
	if _, err := file.WriteString("completed\n"); err != nil {
		_ = file.Close()
		return err
	}
	return file.Close()
}

func (c Cleaner) completionMarkerPath() string {
	return filepath.Join(c.WitselfHome, "migrations", markerName)
}

func (c Cleaner) validate(ctx context.Context, runtimeName string) error {
	if ctx == nil {
		return errors.New("legacy message runner cleanup context is required")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if !supportedRuntime(runtimeName) {
		return fmt.Errorf("unsupported legacy message runner runtime %q", runtimeName)
	}
	if c.Platform != PlatformDarwin && c.Platform != PlatformLinux {
		return fmt.Errorf("legacy message runner cleanup is unsupported on %s", c.Platform)
	}
	if c.Platform == PlatformDarwin && c.UID < 0 {
		return errors.New("legacy message runner cleanup uid is invalid")
	}
	for label, path := range map[string]string{
		"user home": c.UserHome, "config home": c.ConfigHome, "Witself home": c.WitselfHome,
	} {
		if !filepath.IsAbs(path) || filepath.Clean(path) != path || strings.ContainsAny(path, "\x00\r\n") {
			return fmt.Errorf("legacy message runner %s must be a clean absolute path", label)
		}
	}
	if c.Run == nil || c.RemoveAll == nil {
		return errors.New("legacy message runner cleanup dependencies are required")
	}
	return nil
}

func (c Cleaner) result(runtimeName string) Result {
	servicePath := ""
	if c.Platform == PlatformDarwin {
		servicePath = filepath.Join(c.UserHome, "Library", "LaunchAgents", launchdLabel(runtimeName)+".plist")
	} else {
		servicePath = filepath.Join(c.ConfigHome, "systemd", "user", systemdServiceName(runtimeName))
	}
	return Result{
		Runtime: runtimeName, ServicePath: servicePath,
		StatePath: filepath.Join(c.WitselfHome, "message-runners", runtimeName),
	}
}

func (c Cleaner) serviceLoaded(ctx context.Context, runtimeName string) (bool, error) {
	switch c.Platform {
	case PlatformDarwin:
		target := "gui/" + strconv.Itoa(c.UID) + "/" + launchdLabel(runtimeName)
		if err := c.Run(ctx, "launchctl", "print", target); err != nil {
			if errors.Is(err, ErrServiceNotLoaded) {
				return false, nil
			}
			return false, fmt.Errorf("inspect legacy launchd message runner: %w", err)
		}
		return true, nil
	case PlatformLinux:
		if err := c.Run(ctx, "systemctl", "--user", "is-active", "--quiet", systemdServiceName(runtimeName)); err != nil {
			if errors.Is(err, ErrServiceNotLoaded) {
				return false, nil
			}
			return false, fmt.Errorf("inspect legacy systemd message runner: %w", err)
		}
		return true, nil
	default:
		return false, fmt.Errorf("legacy message runner cleanup is unsupported on %s", c.Platform)
	}
}

func (c Cleaner) disable(ctx context.Context, runtimeName string) error {
	switch c.Platform {
	case PlatformDarwin:
		target := "gui/" + strconv.Itoa(c.UID) + "/" + launchdLabel(runtimeName)
		if err := c.Run(ctx, "launchctl", "disable", target); err != nil {
			return fmt.Errorf("disable legacy launchd message runner: %w", err)
		}
	case PlatformLinux:
		if err := c.Run(ctx, "systemctl", "--user", "disable", systemdServiceName(runtimeName)); err != nil {
			return fmt.Errorf("disable legacy systemd message runner: %w", err)
		}
	}
	return nil
}

func (c Cleaner) stop(ctx context.Context, runtimeName string) error {
	switch c.Platform {
	case PlatformDarwin:
		target := "gui/" + strconv.Itoa(c.UID) + "/" + launchdLabel(runtimeName)
		if err := c.Run(ctx, "launchctl", "bootout", target); err != nil {
			return fmt.Errorf("stop legacy launchd message runner: %w", err)
		}
	case PlatformLinux:
		if err := c.Run(ctx, "systemctl", "--user", "stop", systemdServiceName(runtimeName)); err != nil {
			return fmt.Errorf("stop legacy systemd message runner: %w", err)
		}
	}
	return nil
}

func (c Cleaner) manualRetirementCommand(runtimeName string) string {
	switch c.Platform {
	case PlatformDarwin:
		target := "gui/" + strconv.Itoa(c.UID) + "/" + launchdLabel(runtimeName)
		return "launchctl disable " + target + " && launchctl bootout " + target
	case PlatformLinux:
		return "systemctl --user disable --now " + systemdServiceName(runtimeName)
	default:
		return "stop and disable the exact per-user service"
	}
}

func inspectServiceFile(path string) (exists, owned bool, err error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, false, nil
	}
	if err != nil {
		return false, false, err
	}
	if !info.Mode().IsRegular() {
		return true, false, nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return true, false, err
	}
	return true, ownedServiceFile(path, raw), nil
}

func ownedServiceFile(path string, raw []byte) bool {
	base := filepath.Base(path)
	switch {
	case strings.HasPrefix(base, "ai.witwave.witself.message-runner.") && strings.HasSuffix(base, ".plist"):
		label := strings.TrimSuffix(base, ".plist")
		prefix := "<?xml version=\"1.0\" encoding=\"UTF-8\"?>\n" +
			"<!DOCTYPE plist PUBLIC \"-//Apple//DTD PLIST 1.0//EN\" \"http://www.apple.com/DTDs/PropertyList-1.0.dtd\">\n" +
			"<!-- " + managedMarker + " -->\n<plist version=\"1.0\">\n"
		return bytes.HasPrefix(raw, []byte(prefix)) &&
			bytes.Contains(raw, []byte("<key>Label</key>\n  <string>"+label+"</string>")) &&
			bytes.Contains(raw, []byte("<string>message</string>\n    <string>runner</string>\n    <string>serve</string>"))
	case strings.HasPrefix(base, "witself-message-runner-") && strings.HasSuffix(base, ".service"):
		runtimeName := strings.TrimSuffix(strings.TrimPrefix(base, "witself-message-runner-"), ".service")
		prefix := "# " + managedMarker + "\n[Unit]\n"
		return bytes.HasPrefix(raw, []byte(prefix)) &&
			bytes.Contains(raw, []byte("\n[Service]\n")) &&
			bytes.Contains(raw, []byte(`"message" "runner" "serve" "--runtime" "`+runtimeName+`"`))
	default:
		return false
	}
}

func removeOwnedServiceFile(path string) error {
	exists, owned, err := inspectServiceFile(path)
	if err != nil {
		return err
	}
	if !exists {
		return nil
	}
	if !owned {
		return fmt.Errorf("%w: %s", ErrUnownedService, path)
	}
	return os.Remove(path)
}

func supportedRuntime(runtimeName string) bool {
	for _, candidate := range Runtimes() {
		if runtimeName == candidate {
			return true
		}
	}
	return false
}

func launchdLabel(runtimeName string) string {
	return "ai.witwave.witself.message-runner." + runtimeName
}

func systemdServiceName(runtimeName string) string {
	return "witself-message-runner-" + runtimeName + ".service"
}

func runCommand(ctx context.Context, name string, args ...string) error {
	command := exec.CommandContext(ctx, name, args...)
	command.Stdin = nil
	var output limitedBuffer
	command.Stdout = &output
	command.Stderr = &output
	err := command.Run()
	if err == nil {
		return nil
	}
	if name == "launchctl" && len(args) > 0 && args[0] == "print" {
		message := strings.ToLower(output.String())
		if strings.Contains(message, "could not find service") || strings.Contains(message, "service not found") {
			return fmt.Errorf("%w: %v", ErrServiceNotLoaded, err)
		}
	}
	if name == "systemctl" && len(args) >= 4 && args[0] == "--user" && args[1] == "is-active" {
		if errors.Is(err, exec.ErrNotFound) {
			return fmt.Errorf("%w: %v", ErrServiceManagerUnavailable, err)
		}
		message := strings.ToLower(output.String())
		if strings.Contains(message, "failed to connect to bus") ||
			strings.Contains(message, "no medium found") {
			return fmt.Errorf("%w: %v", ErrServiceManagerUnavailable, err)
		}
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && (exitErr.ExitCode() == 3 || exitErr.ExitCode() == 4) {
			return fmt.Errorf("%w: %v", ErrServiceNotLoaded, err)
		}
	}
	return err
}

func pendingNotificationCount(stateRoot string) (int, error) {
	rootInfo, err := os.Lstat(stateRoot)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	if !rootInfo.IsDir() || rootInfo.Mode().Perm()&0o077 != 0 {
		return 0, fmt.Errorf("legacy message runner state root %s must be a private directory", stateRoot)
	}
	path := filepath.Join(stateRoot, "state.json")
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
		return 0, fmt.Errorf("legacy message runner state %s must be a private regular file", path)
	}
	if info.Size() > maxStateBytes {
		return 0, fmt.Errorf("legacy message runner state %s exceeds its size limit", path)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	var state struct {
		Schema        string `json:"schema"`
		Notifications []struct {
			MessageID string `json:"message_id"`
		} `json:"notifications"`
	}
	if err := json.Unmarshal(raw, &state); err != nil {
		return 0, fmt.Errorf("parse legacy message runner state: %w", err)
	}
	if state.Schema != stateSchema {
		return 0, fmt.Errorf("legacy message runner state has unsupported schema %q", state.Schema)
	}
	for _, notification := range state.Notifications {
		if strings.TrimSpace(notification.MessageID) == "" {
			return 0, errors.New("legacy message runner state has a notification without a message id")
		}
	}
	return len(state.Notifications), nil
}

type limitedBuffer struct {
	buffer bytes.Buffer
}

func (b *limitedBuffer) String() string {
	return b.buffer.String()
}

func (b *limitedBuffer) Write(value []byte) (int, error) {
	const limit = 32 * 1024
	if b.buffer.Len() >= limit {
		return len(value), nil
	}
	remaining := limit - b.buffer.Len()
	if len(value) > remaining {
		_, _ = b.buffer.Write(value[:remaining])
		return len(value), nil
	}
	return b.buffer.Write(value)
}
