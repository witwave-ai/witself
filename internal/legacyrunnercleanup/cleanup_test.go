package legacyrunnercleanup

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
)

func TestDarwinCleanupDeactivatesOwnedUnitBeforePurge(t *testing.T) {
	cleaner := testCleaner(t, PlatformDarwin)
	runtimeName := "claude-code"
	result := cleaner.result(runtimeName)
	writeTestFile(t, result.ServicePath, darwinDefinition(runtimeName))
	writeTestFile(t, filepath.Join(result.StatePath, "provider-credentials.json"), "private")

	var events []string
	cleaner.Run = func(_ context.Context, name string, args ...string) error {
		if _, err := os.Stat(result.StatePath); err != nil {
			t.Fatalf("state was purged before %s: %v", name, err)
		}
		if name == "launchctl" && len(args) > 0 && args[0] == "bootout" {
			assertMissing(t, result.ServicePath)
		}
		events = append(events, strings.Join(append([]string{name}, args...), " "))
		return nil
	}
	cleaner.RemoveAll = func(path string) error {
		if path != result.StatePath {
			t.Fatalf("purge path = %q", path)
		}
		events = append(events, "purge "+path)
		return os.RemoveAll(path)
	}

	got, err := cleaner.Cleanup(context.Background(), runtimeName)
	if err != nil {
		t.Fatal(err)
	}
	wantEvents := []string{
		"launchctl print gui/501/ai.witwave.witself.message-runner.claude-code",
		"launchctl disable gui/501/ai.witwave.witself.message-runner.claude-code",
		"launchctl bootout gui/501/ai.witwave.witself.message-runner.claude-code",
		"purge " + result.StatePath,
	}
	if !reflect.DeepEqual(events, wantEvents) {
		t.Fatalf("events = %#v, want %#v", events, wantEvents)
	}
	if !got.ServiceRemoved || !got.StatePurged {
		t.Fatalf("result = %#v", got)
	}
	assertMissing(t, result.ServicePath)
	assertMissing(t, result.StatePath)
}

func TestDarwinCleanupSkipsBootoutWhenUnitIsNotLoaded(t *testing.T) {
	cleaner := testCleaner(t, PlatformDarwin)
	runtimeName := "cursor"
	result := cleaner.result(runtimeName)
	writeTestFile(t, result.ServicePath, darwinDefinition(runtimeName))
	writeTestFile(t, filepath.Join(result.StatePath, "state.json"), stateDefinition())

	var events []string
	cleaner.Run = func(_ context.Context, name string, args ...string) error {
		events = append(events, strings.Join(append([]string{name}, args...), " "))
		if len(args) > 0 && args[0] == "print" {
			return ErrServiceNotLoaded
		}
		return nil
	}
	cleaner.RemoveAll = func(path string) error {
		events = append(events, "purge "+path)
		return os.RemoveAll(path)
	}

	if _, err := cleaner.Cleanup(context.Background(), runtimeName); err != nil {
		t.Fatal(err)
	}
	wantEvents := []string{
		"launchctl print gui/501/ai.witwave.witself.message-runner.cursor",
		"launchctl disable gui/501/ai.witwave.witself.message-runner.cursor",
		"purge " + result.StatePath,
	}
	if !reflect.DeepEqual(events, wantEvents) {
		t.Fatalf("events = %#v, want %#v", events, wantEvents)
	}
}

func TestLinuxCleanupUsesExactUnitAndReloadsBeforePurge(t *testing.T) {
	cleaner := testCleaner(t, PlatformLinux)
	runtimeName := "grok-build"
	result := cleaner.result(runtimeName)
	writeTestFile(t, result.ServicePath, linuxDefinition(runtimeName))
	writeTestFile(t, filepath.Join(result.StatePath, "state.json"), stateDefinition())

	var events []string
	cleaner.Run = func(_ context.Context, name string, args ...string) error {
		if _, err := os.Stat(result.StatePath); err != nil {
			t.Fatalf("state was purged before %s: %v", name, err)
		}
		if name == "systemctl" && len(args) > 1 && args[1] == "stop" {
			assertMissing(t, result.ServicePath)
		}
		events = append(events, strings.Join(append([]string{name}, args...), " "))
		return nil
	}
	cleaner.RemoveAll = func(path string) error {
		events = append(events, "purge "+path)
		return os.RemoveAll(path)
	}

	got, err := cleaner.Cleanup(context.Background(), runtimeName)
	if err != nil {
		t.Fatal(err)
	}
	wantEvents := []string{
		"systemctl --user is-active --quiet witself-message-runner-grok-build.service",
		"systemctl --user disable witself-message-runner-grok-build.service",
		"systemctl --user stop witself-message-runner-grok-build.service",
		"systemctl --user daemon-reload",
		"purge " + result.StatePath,
	}
	if !reflect.DeepEqual(events, wantEvents) {
		t.Fatalf("events = %#v, want %#v", events, wantEvents)
	}
	if !got.ServiceRemoved || !got.StatePurged {
		t.Fatalf("result = %#v", got)
	}
	assertMissing(t, result.ServicePath)
	assertMissing(t, result.StatePath)
}

func TestCleanupFailsClosedOnUnownedDefinition(t *testing.T) {
	cleaner := testCleaner(t, PlatformDarwin)
	result := cleaner.result("cursor")
	writeTestFile(t, result.ServicePath, "user-owned launch agent")
	writeTestFile(t, filepath.Join(result.StatePath, "provider-credentials.json"), "private")
	called := false
	cleaner.Run = func(context.Context, string, ...string) error {
		called = true
		return nil
	}
	cleaner.RemoveAll = func(string) error {
		called = true
		return nil
	}

	_, err := cleaner.Cleanup(context.Background(), "cursor")
	if !errors.Is(err, ErrUnownedService) {
		t.Fatalf("error = %v", err)
	}
	if called {
		t.Fatal("unowned preflight performed a mutation")
	}
	if _, err := os.Stat(result.ServicePath); err != nil {
		t.Fatalf("unowned service file changed: %v", err)
	}
	if _, err := os.Stat(result.StatePath); err != nil {
		t.Fatalf("state purged after unowned preflight: %v", err)
	}
}

func TestCleanupIsIdempotentWithoutLegacyState(t *testing.T) {
	cleaner := testCleaner(t, PlatformLinux)
	inspections, mutations := 0, 0
	cleaner.Run = func(_ context.Context, _ string, args ...string) error {
		if len(args) >= 2 && args[1] == "is-active" {
			inspections++
			return ErrServiceNotLoaded
		}
		mutations++
		return nil
	}
	for range 2 {
		result, err := cleaner.Cleanup(context.Background(), "codex")
		if err != nil {
			t.Fatal(err)
		}
		if result.ServiceRemoved || !result.StatePurged {
			t.Fatalf("result = %#v", result)
		}
	}
	if inspections != 2 || mutations != 0 {
		t.Fatalf("idempotent cleanup inspections/mutations = %d/%d", inspections, mutations)
	}
}

func TestCleanupRetriesTransientStatePurgeRace(t *testing.T) {
	cleaner := testCleaner(t, PlatformLinux)
	result := cleaner.result("grok-build")
	writeTestFile(t, filepath.Join(result.StatePath, "state.json"), stateDefinition())
	calls := 0
	cleaner.RemoveAll = func(path string) error {
		calls++
		if calls == 1 {
			return errors.New("transient directory not empty")
		}
		return os.RemoveAll(path)
	}

	got, err := cleaner.Cleanup(context.Background(), "grok-build")
	if err != nil {
		t.Fatal(err)
	}
	if calls != 2 || !got.StatePurged {
		t.Fatalf("purge calls/result = %d / %#v", calls, got)
	}
	assertMissing(t, result.StatePath)
}

func TestCleanupAllPreflightsBeforeAnyMutation(t *testing.T) {
	cleaner := testCleaner(t, PlatformLinux)
	writeTestFile(t, cleaner.result("codex").ServicePath, linuxDefinition("codex"))
	unowned := cleaner.result("cursor")
	writeTestFile(t, unowned.ServicePath, "not Witself owned")
	mutated := false
	cleaner.Run = func(_ context.Context, _ string, args ...string) error {
		if len(args) >= 2 && args[1] == "is-active" {
			return ErrServiceNotLoaded
		}
		mutated = true
		return nil
	}
	cleaner.RemoveAll = func(string) error {
		mutated = true
		return nil
	}

	_, err := cleaner.CleanupAll(context.Background())
	if !errors.Is(err, ErrUnownedService) {
		t.Fatalf("error = %v", err)
	}
	if mutated {
		t.Fatal("multi-runtime preflight partially mutated local state")
	}
}

func TestCleanupAllPurgesStateOnlyInstallWithoutUserSystemd(t *testing.T) {
	cleaner := testCleaner(t, PlatformLinux)
	result := cleaner.result("cursor")
	writeTestFile(t, filepath.Join(result.StatePath, "state.json"), stateDefinition())
	writeTestFile(t, filepath.Join(result.StatePath, "provider-credentials.json"), "private")
	cleaner.Run = func(_ context.Context, name string, args ...string) error {
		if name == "systemctl" && len(args) >= 2 && args[1] == "is-active" {
			return ErrServiceManagerUnavailable
		}
		t.Fatalf("state-only cleanup ran mutating service command: %s %v", name, args)
		return nil
	}

	results, err := cleaner.CleanupAll(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != len(Runtimes()) {
		t.Fatalf("result count = %d", len(results))
	}
	assertMissing(t, result.StatePath)
}

func TestCleanupKeepsOwnedDefinitionWhenServiceManagerIsUnavailable(t *testing.T) {
	cleaner := testCleaner(t, PlatformLinux)
	result := cleaner.result("codex")
	writeTestFile(t, result.ServicePath, linuxDefinition("codex"))
	writeTestFile(t, filepath.Join(result.StatePath, "state.json"), stateDefinition())
	cleaner.Run = func(_ context.Context, name string, args ...string) error {
		if name == "systemctl" && len(args) >= 2 && args[1] == "is-active" {
			return ErrServiceManagerUnavailable
		}
		t.Fatalf("unverified service retirement ran mutation: %s %v", name, args)
		return nil
	}

	_, err := cleaner.Cleanup(context.Background(), "codex")
	if !errors.Is(err, ErrServiceManagerUnavailable) {
		t.Fatalf("error = %v, want ErrServiceManagerUnavailable", err)
	}
	if _, statErr := os.Stat(result.ServicePath); statErr != nil {
		t.Fatalf("owned definition changed after unavailable manager: %v", statErr)
	}
	if _, statErr := os.Stat(result.StatePath); statErr != nil {
		t.Fatalf("state changed after unavailable manager: %v", statErr)
	}
}

func TestCleanupRetiresServiceButPreservesPendingPointersWithoutForce(t *testing.T) {
	cleaner := testCleaner(t, PlatformDarwin)
	runtimeName := "claude-code"
	result := cleaner.result(runtimeName)
	writeTestFile(t, result.ServicePath, darwinDefinition(runtimeName))
	writeTestFile(t, filepath.Join(result.StatePath, "state.json"), stateDefinition("msg_legacy"))
	writeTestFile(t, filepath.Join(result.StatePath, "provider-credentials.json"), "private")
	cleaner.Run = func(_ context.Context, name string, args ...string) error {
		if name == "launchctl" && len(args) > 0 && args[0] == "print" {
			return nil
		}
		return nil
	}

	got, err := cleaner.Cleanup(context.Background(), runtimeName)
	if !errors.Is(err, ErrPendingNotifications) {
		t.Fatalf("error = %v, want ErrPendingNotifications", err)
	}
	if !got.ServiceRemoved || got.StatePurged || !got.StateScrubbed || got.PendingNotifications != 1 {
		t.Fatalf("result = %#v", got)
	}
	assertMissing(t, result.ServicePath)
	if _, err := os.Stat(filepath.Join(result.StatePath, "state.json")); err != nil {
		t.Fatalf("pending state was not preserved: %v", err)
	}
	assertMissing(t, filepath.Join(result.StatePath, "provider-credentials.json"))
}

func TestCleanupForcePurgesPendingPointers(t *testing.T) {
	cleaner := testCleaner(t, PlatformLinux)
	cleaner.Force = true
	result := cleaner.result("cursor")
	writeTestFile(t, filepath.Join(result.StatePath, "state.json"), stateDefinition("msg_legacy"))

	got, err := cleaner.Cleanup(context.Background(), "cursor")
	if err != nil {
		t.Fatal(err)
	}
	if !got.StatePurged || got.PendingNotifications != 1 {
		t.Fatalf("result = %#v", got)
	}
	assertMissing(t, result.StatePath)
}

func TestCleanupFailsClosedOnLoadedUnitWithoutOwnedDefinition(t *testing.T) {
	cleaner := testCleaner(t, PlatformLinux)
	cleaner.Run = func(_ context.Context, name string, args ...string) error {
		if name == "systemctl" && len(args) >= 2 && args[1] == "is-active" {
			return nil
		}
		t.Fatal("loaded unowned unit reached a mutating service command")
		return nil
	}
	called := false
	cleaner.RemoveAll = func(string) error {
		called = true
		return nil
	}

	_, err := cleaner.Cleanup(context.Background(), "codex")
	if !errors.Is(err, ErrUnownedService) {
		t.Fatalf("error = %v, want ErrUnownedService", err)
	}
	if !strings.Contains(err.Error(), "systemctl --user disable --now witself-message-runner-codex.service") {
		t.Fatalf("error lacks exact remediation: %v", err)
	}
	if called {
		t.Fatal("loaded unowned unit caused state removal")
	}
}

func TestCleanupServeTombstoneRetiresLoadedUnitWithoutDefinition(t *testing.T) {
	cleaner := testCleaner(t, PlatformLinux)
	cleaner.AllowLoadedWithoutDefinition = true
	result := cleaner.result("codex")
	writeTestFile(t, filepath.Join(result.StatePath, "state.json"), stateDefinition())
	var events []string
	cleaner.Run = func(_ context.Context, name string, args ...string) error {
		events = append(events, strings.Join(append([]string{name}, args...), " "))
		return nil
	}

	got, err := cleaner.Cleanup(context.Background(), "codex")
	if err != nil {
		t.Fatal(err)
	}
	wantEvents := []string{
		"systemctl --user is-active --quiet witself-message-runner-codex.service",
		"systemctl --user disable witself-message-runner-codex.service",
		"systemctl --user stop witself-message-runner-codex.service",
		"systemctl --user daemon-reload",
	}
	if !reflect.DeepEqual(events, wantEvents) {
		t.Fatalf("events = %#v, want %#v", events, wantEvents)
	}
	if !got.ServiceRemoved || !got.StatePurged {
		t.Fatalf("result = %#v", got)
	}
}

func TestCleanupRetiresServiceAndPreservesMalformedState(t *testing.T) {
	cleaner := testCleaner(t, PlatformDarwin)
	runtimeName := "grok-build"
	result := cleaner.result(runtimeName)
	writeTestFile(t, result.ServicePath, darwinDefinition(runtimeName))
	writeTestFile(t, filepath.Join(result.StatePath, "state.json"), "{malformed")
	writeTestFile(t, filepath.Join(result.StatePath, "provider-credentials.json"), "private")

	got, err := cleaner.Cleanup(context.Background(), runtimeName)
	if !errors.Is(err, ErrStatePreserved) {
		t.Fatalf("error = %v, want ErrStatePreserved", err)
	}
	if !got.ServiceRemoved || got.StatePurged || !got.StateScrubbed {
		t.Fatalf("result = %#v", got)
	}
	assertMissing(t, result.ServicePath)
	if _, statErr := os.Stat(filepath.Join(result.StatePath, "state.json")); statErr != nil {
		t.Fatalf("malformed state was not preserved: %v", statErr)
	}
	assertMissing(t, filepath.Join(result.StatePath, "provider-credentials.json"))

	cleaner.Force = true
	got, err = cleaner.Cleanup(context.Background(), runtimeName)
	if err != nil {
		t.Fatal(err)
	}
	if !got.StatePurged {
		t.Fatalf("force result = %#v", got)
	}
	assertMissing(t, result.StatePath)
}

func TestCleanupPreservesNonPrivateStateRootWithoutForce(t *testing.T) {
	cleaner := testCleaner(t, PlatformLinux)
	result := cleaner.result("cursor")
	writeTestFile(t, filepath.Join(result.StatePath, "state.json"), stateDefinition())
	writeTestFile(t, filepath.Join(result.StatePath, "provider-credentials.json"), "private")
	if err := os.Chmod(result.StatePath, 0o755); err != nil {
		t.Fatal(err)
	}

	got, err := cleaner.Cleanup(context.Background(), "cursor")
	if !errors.Is(err, ErrStatePreserved) {
		t.Fatalf("error = %v, want ErrStatePreserved", err)
	}
	if got.StatePurged || got.StateScrubbed {
		t.Fatalf("result = %#v", got)
	}
	if _, statErr := os.Stat(filepath.Join(result.StatePath, "provider-credentials.json")); statErr != nil {
		t.Fatalf("non-private state was mutated: %v", statErr)
	}

	cleaner.Force = true
	got, err = cleaner.Cleanup(context.Background(), "cursor")
	if err != nil {
		t.Fatal(err)
	}
	if !got.StatePurged {
		t.Fatalf("force result = %#v", got)
	}
	assertMissing(t, result.StatePath)
}

func TestHasArtifactsDoesNotContactServiceManager(t *testing.T) {
	cleaner := testCleaner(t, PlatformLinux)
	cleaner.Run = func(context.Context, string, ...string) error {
		t.Fatal("filesystem artifact check contacted service manager")
		return nil
	}
	hasArtifacts, err := cleaner.HasArtifacts()
	if err != nil || hasArtifacts {
		t.Fatalf("HasArtifacts = %t / %v", hasArtifacts, err)
	}
	writeTestFile(t, filepath.Join(cleaner.result("cursor").StatePath, "state.json"), stateDefinition())
	hasArtifacts, err = cleaner.HasArtifacts()
	if err != nil || !hasArtifacts {
		t.Fatalf("HasArtifacts with state = %t / %v", hasArtifacts, err)
	}
}

func TestRunCommandClassifiesUnavailableUserSystemd(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test helper uses a POSIX executable")
	}
	directory := t.TempDir()
	systemctl := filepath.Join(directory, "systemctl")
	if err := os.WriteFile(systemctl, []byte("#!/bin/sh\nprintf 'Failed to connect to bus: No medium found\\n' >&2\nexit 1\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", directory)
	err := runCommand(context.Background(), "systemctl", "--user", "is-active", "--quiet", "witself-message-runner-codex.service")
	if !errors.Is(err, ErrServiceManagerUnavailable) {
		t.Fatalf("error = %v, want ErrServiceManagerUnavailable", err)
	}
}

func TestCleanupCompletionMarkerIsPrivateAndIdempotent(t *testing.T) {
	cleaner := testCleaner(t, PlatformDarwin)
	if complete, err := cleaner.Completed(); err != nil || complete {
		t.Fatalf("initial completed = %t / %v", complete, err)
	}
	for range 2 {
		if err := cleaner.MarkCompleted(); err != nil {
			t.Fatal(err)
		}
	}
	if complete, err := cleaner.Completed(); err != nil || !complete {
		t.Fatalf("completed = %t / %v", complete, err)
	}
	info, err := os.Stat(cleaner.completionMarkerPath())
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("marker mode = %o", info.Mode().Perm())
	}
}

func testCleaner(t *testing.T, platform string) Cleaner {
	t.Helper()
	root := t.TempDir()
	return Cleaner{
		Platform: platform, UserHome: filepath.Join(root, "home"),
		ConfigHome: filepath.Join(root, "config"), WitselfHome: filepath.Join(root, "witself"),
		UID: 501, Run: func(_ context.Context, name string, args ...string) error {
			if (name == "launchctl" && len(args) > 0 && args[0] == "print") ||
				(name == "systemctl" && len(args) >= 2 && args[1] == "is-active") {
				return ErrServiceNotLoaded
			}
			return nil
		},
		RemoveAll: os.RemoveAll,
	}
}

func darwinDefinition(runtimeName string) string {
	label := launchdLabel(runtimeName)
	return "<?xml version=\"1.0\" encoding=\"UTF-8\"?>\n" +
		"<!DOCTYPE plist PUBLIC \"-//Apple//DTD PLIST 1.0//EN\" \"http://www.apple.com/DTDs/PropertyList-1.0.dtd\">\n" +
		"<!-- " + managedMarker + " -->\n<plist version=\"1.0\">\n<dict>\n" +
		"  <key>Label</key>\n  <string>" + label + "</string>\n" +
		"  <key>ProgramArguments</key>\n  <array>\n    <string>/usr/local/bin/witself</string>\n" +
		"    <string>message</string>\n    <string>runner</string>\n    <string>serve</string>\n" +
		"    <string>--runtime</string>\n    <string>" + runtimeName + "</string>\n  </array>\n</dict>\n</plist>\n"
}

func linuxDefinition(runtimeName string) string {
	return "# " + managedMarker + "\n[Unit]\nDescription=Witself autonomous message runner\n\n" +
		"[Service]\nExecStart=\"/usr/local/bin/witself\" \"message\" \"runner\" \"serve\" \"--runtime\" \"" + runtimeName + "\"\n"
}

func stateDefinition(messageIDs ...string) string {
	items := make([]string, 0, len(messageIDs))
	for _, messageID := range messageIDs {
		items = append(items, `{"message_id":"`+messageID+`"}`)
	}
	return `{"schema":"` + stateSchema + `","notifications":[` + strings.Join(items, ",") + `]}`
}

func writeTestFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func assertMissing(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("%s still exists: %v", path, err)
	}
}
