package memorycurator

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type autoFakeClock struct {
	now time.Time
}

func (c *autoFakeClock) Now() time.Time { return c.now }
func (c *autoFakeClock) Advance(duration time.Duration) {
	c.now = c.now.Add(duration)
}

func newAutoTestStore(t *testing.T) (AutoStore, *autoFakeClock) {
	t.Helper()
	clock := &autoFakeClock{now: time.Date(2026, 7, 14, 18, 0, 0, 0, time.UTC)}
	store, err := NewAutoStore(filepath.Join(t.TempDir(), "curation", "agent_1"), "agent_1")
	if err != nil {
		t.Fatal(err)
	}
	counter := 0
	store.Now = clock.Now
	store.NewID = func(prefix string) (string, error) {
		counter++
		return fmt.Sprintf("%s_%d", prefix, counter), nil
	}
	return store, clock
}

func validAutoSettings(agentID string) AutoSettings {
	return AutoSettings{
		AccountID: "acc_1", RealmID: "realm_1", AgentID: agentID,
		Provider: ProviderClaudeCode, ProviderPath: "/usr/local/bin/claude",
		Model: "claude-test", ApplyPolicy: ApplyPolicyPreview,
		AllowTranscriptContent: true,
		Debounce:               DefaultAutoDebounce, MinimumInterval: DefaultAutoMinimumInterval,
		MaxRuns: DefaultAutoMaxRuns,
	}
}

func enableAutoTestStore(t *testing.T, store AutoStore, mutate func(*AutoSettings)) AutoConfig {
	t.Helper()
	settings := validAutoSettings(store.AgentID)
	if mutate != nil {
		mutate(&settings)
	}
	config, err := store.Enable(settings)
	if err != nil {
		t.Fatalf("Enable() error = %v", err)
	}
	return config
}

func TestAutoStoreEnableIsPrivateValueFreeAndRequiresTranscriptGrant(t *testing.T) {
	t.Setenv("WITSELF_HOME", filepath.Join(t.TempDir(), ".witself"))
	store, err := DefaultAutoStore("agent_1")
	if err != nil {
		t.Fatal(err)
	}
	settings := validAutoSettings("agent_1")
	settings.AllowTranscriptContent = false
	if _, err := store.Enable(settings); err == nil || !strings.Contains(err.Error(), "transcript content authorization") {
		t.Fatalf("Enable() without transcript grant error = %v", err)
	}
	settings.AllowTranscriptContent = true
	config, err := store.Enable(settings)
	if err != nil {
		t.Fatal(err)
	}
	if !config.Enabled || !config.AllowTranscriptContent || config.Provider != ProviderClaudeCode ||
		config.ApplyPolicy != ApplyPolicyPreview || config.Debounce() != DefaultAutoDebounce ||
		config.MinimumInterval() != DefaultAutoMinimumInterval || config.MaxRuns != DefaultAutoMaxRuns {
		t.Fatalf("config = %#v", config)
	}
	if want := filepath.Join(os.Getenv("WITSELF_HOME"), "curation", "agent_1"); store.Root != want {
		t.Fatalf("root = %q, want %q", store.Root, want)
	}
	for _, path := range []string{store.configPath(), store.statusPath()} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if got := info.Mode().Perm(); got != 0o600 {
			t.Fatalf("%s mode = %#o, want 0600", path, got)
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		for _, forbidden := range []string{
			"WITSELF_TOKEN", "Bearer ", "private narrative body", "materialized_inputs",
			"transcript_entries", "memory_content", "evidence_artifact", "planner_envelope",
		} {
			if strings.Contains(string(raw), forbidden) {
				t.Fatalf("%s contains forbidden value %q: %s", path, forbidden, raw)
			}
		}
	}
	rootInfo, err := os.Stat(store.Root)
	if err != nil {
		t.Fatal(err)
	}
	if got := rootInfo.Mode().Perm(); got != 0o700 {
		t.Fatalf("root mode = %#o, want 0700", got)
	}

	inspection, err := store.Inspect()
	if err != nil {
		t.Fatal(err)
	}
	if !inspection.Configured || inspection.Status.State != AutoStateIdle || inspection.PendingWakeCount != 0 {
		t.Fatalf("inspection = %#v", inspection)
	}
}

func TestAutoStoreEnableFailureNeverActivatesRequestedConfig(t *testing.T) {
	tests := []struct {
		name             string
		previousDisabled bool
		failConfigWrite  bool
	}{
		{name: "new status write fails"},
		{name: "new config commit fails", failConfigWrite: true},
		{name: "replacement status write fails", previousDisabled: true},
		{name: "replacement config commit fails", previousDisabled: true, failConfigWrite: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store, _ := newAutoTestStore(t)
			if test.previousDisabled {
				enableAutoTestStore(t, store, nil)
				if err := store.Disable(); err != nil {
					t.Fatal(err)
				}
			}
			beforeConfig, configExisted := readOptionalAutoTestFile(t, store.configPath())
			beforeStatus, statusExisted := readOptionalAutoTestFile(t, store.statusPath())
			failPath := store.statusPath()
			if test.failConfigWrite {
				failPath = store.configPath()
			}
			injected := errors.New("injected curator automation state write failure")
			store.BeforeWrite = func(path string) error {
				if path == failPath {
					return injected
				}
				return nil
			}
			if _, err := store.Enable(validAutoSettings(store.AgentID)); !errors.Is(err, injected) {
				t.Fatalf("Enable() error = %v, want injected failure", err)
			}
			assertOptionalAutoTestFile(t, store.configPath(), beforeConfig, configExisted)
			assertOptionalAutoTestFile(t, store.statusPath(), beforeStatus, statusExisted)
			if test.previousDisabled {
				config, err := store.LoadConfig()
				if err != nil {
					t.Fatal(err)
				}
				if config.Enabled {
					t.Fatal("failed Enable() replaced the previous disabled config")
				}
			}
		})
	}

	t.Run("active replacement preserves old policy", func(t *testing.T) {
		store, _ := newAutoTestStore(t)
		previous := enableAutoTestStore(t, store, func(settings *AutoSettings) {
			settings.Model = "claude-old-policy"
			settings.ApplyPolicy = ApplyPolicyPreview
			settings.MinimumInterval = 20 * time.Minute
		})
		beforeConfig, _ := readOptionalAutoTestFile(t, store.configPath())
		beforeStatus, _ := readOptionalAutoTestFile(t, store.statusPath())
		injected := errors.New("injected final config commit failure")
		store.BeforeWrite = func(path string) error {
			if path == store.configPath() {
				return injected
			}
			return nil
		}
		replacement := validAutoSettings(store.AgentID)
		replacement.Provider = ProviderCodex
		replacement.ProviderPath = "/usr/local/bin/codex"
		replacement.Model = "gpt-new-policy"
		replacement.ApplyPolicy = ApplyPolicyApply
		replacement.MinimumInterval = time.Minute
		if _, err := store.Enable(replacement); !errors.Is(err, injected) {
			t.Fatalf("Enable() replacement error = %v, want injected failure", err)
		}
		assertOptionalAutoTestFile(t, store.configPath(), beforeConfig, true)
		assertOptionalAutoTestFile(t, store.statusPath(), beforeStatus, true)
		got, err := store.LoadConfig()
		if err != nil {
			t.Fatal(err)
		}
		if !got.Enabled || got.Revision != previous.Revision || got.Provider != previous.Provider ||
			got.Model != previous.Model || got.ApplyPolicy != previous.ApplyPolicy ||
			got.MinimumInterval() != previous.MinimumInterval() {
			t.Fatalf("failed replacement changed active policy: got %#v, want %#v", got, previous)
		}
	})
}

func readOptionalAutoTestFile(t *testing.T, path string) (string, bool) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return "", false
	}
	if err != nil {
		t.Fatal(err)
	}
	return string(raw), true
}

func assertOptionalAutoTestFile(t *testing.T, path, want string, wantExists bool) {
	t.Helper()
	got, gotExists := readOptionalAutoTestFile(t, path)
	if gotExists != wantExists || got != want {
		t.Fatalf("state file %s after failed Enable() = (exists %t, %q), want (exists %t, %q)",
			path, gotExists, got, wantExists, want)
	}
}

func TestAutoStoreRejectsUnsafeOrAmbiguousSettings(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*AutoSettings)
		want   string
	}{
		{"wrong agent", func(s *AutoSettings) { s.AgentID = "agent_2" }, "agent identity"},
		{"one identity", func(s *AutoSettings) { s.RealmID = "" }, "appear together"},
		{"no provider", func(s *AutoSettings) { s.Provider = "" }, "explicit supported provider"},
		{"relative provider path", func(s *AutoSettings) { s.ProviderPath = "bin/claude" }, "clean absolute"},
		{"unclean provider path", func(s *AutoSettings) { s.ProviderPath = "/usr/local/../bin/claude" }, "clean absolute"},
		{"invalid model", func(s *AutoSettings) { s.Model = "model with spaces" }, "model name"},
		{"invalid policy", func(s *AutoSettings) { s.ApplyPolicy = "delete" }, "apply policy"},
		{"missing grant", func(s *AutoSettings) { s.AllowTranscriptContent = false }, "transcript content"},
		{"fractional debounce", func(s *AutoSettings) { s.Debounce = 1500 * time.Millisecond }, "whole seconds"},
		{"short debounce", func(s *AutoSettings) { s.Debounce = 0 }, "debounce"},
		{"long debounce", func(s *AutoSettings) { s.Debounce = MaxAutoDebounce + time.Second }, "debounce"},
		{"negative interval", func(s *AutoSettings) { s.MinimumInterval = -time.Second }, "minimum interval"},
		{"long interval", func(s *AutoSettings) { s.MinimumInterval = MaxAutoMinimumInterval + time.Second }, "minimum interval"},
		{"no runs", func(s *AutoSettings) { s.MaxRuns = 0 }, "max runs"},
		{"too many runs", func(s *AutoSettings) { s.MaxRuns = MaxAutoRuns + 1 }, "max runs"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store, _ := newAutoTestStore(t)
			settings := validAutoSettings(store.AgentID)
			test.mutate(&settings)
			if _, err := store.Enable(settings); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Enable() error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestAutoWakeSnapshotAcknowledgesOnlyServicedMarkers(t *testing.T) {
	store, clock := newAutoTestStore(t)
	enableAutoTestStore(t, store, func(settings *AutoSettings) {
		settings.Debounce = time.Second
		settings.MinimumInterval = 0
	})
	first, err := store.RecordWake(AutoWakeTerminalFlush)
	if err != nil {
		t.Fatal(err)
	}
	clock.Advance(time.Second)
	var during AutoWakeMarker
	result, err := store.RunPending(context.Background(), func(context.Context, AutoConfig) (AutoWorkResult, error) {
		var recordErr error
		during, recordErr = store.RecordWake(AutoWakeScheduledPoll)
		return AutoWorkResult{Outcome: AutoOutcomeNoWork}, recordErr
	})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Acquired || !result.Attempted || result.Runs != 1 || result.PendingWakeCount != 1 {
		t.Fatalf("result = %#v", result)
	}
	pending, err := store.PendingWakes()
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 || pending[0].ID != during.ID || pending[0].ID == first.ID {
		t.Fatalf("pending = %#v", pending)
	}
	markerInfo, err := os.Stat(filepath.Join(store.wakeDir(), during.ID+".json"))
	if err != nil {
		t.Fatal(err)
	}
	if got := markerInfo.Mode().Perm(); got != 0o600 {
		t.Fatalf("marker mode = %#o, want 0600", got)
	}
	clock.Advance(time.Second)
	if _, err := store.RunPending(context.Background(), func(context.Context, AutoConfig) (AutoWorkResult, error) {
		return AutoWorkResult{Outcome: AutoOutcomeNoWork}, nil
	}); err != nil {
		t.Fatal(err)
	}
	pending, err = store.PendingWakes()
	if err != nil || len(pending) != 0 {
		t.Fatalf("pending after second run = %#v, %v", pending, err)
	}
	status, err := store.LoadStatus()
	if err != nil {
		t.Fatal(err)
	}
	if status.TotalRuns != 2 || status.State != AutoStateIdle || status.LastOutcome != AutoOutcomeNoWork {
		t.Fatalf("status = %#v", status)
	}
}

func TestAutoWakeMarkersAreAtomicUniqueUnderConcurrency(t *testing.T) {
	store, err := NewAutoStore(filepath.Join(t.TempDir(), "curation", "agent_1"), "agent_1")
	if err != nil {
		t.Fatal(err)
	}
	const count = 40
	var group sync.WaitGroup
	errorsSeen := make(chan error, count)
	for i := 0; i < count; i++ {
		group.Add(1)
		go func() {
			defer group.Done()
			_, err := store.RecordWake(AutoWakeTerminalFlush)
			errorsSeen <- err
		}()
	}
	group.Wait()
	close(errorsSeen)
	for err := range errorsSeen {
		if err != nil {
			t.Fatal(err)
		}
	}
	markers, err := store.PendingWakes()
	if err != nil {
		t.Fatal(err)
	}
	if len(markers) != count {
		t.Fatalf("marker count = %d, want %d", len(markers), count)
	}
	seen := make(map[string]bool, count)
	for _, marker := range markers {
		if seen[marker.ID] {
			t.Fatalf("duplicate marker id %q", marker.ID)
		}
		seen[marker.ID] = true
	}
	entries, err := os.ReadDir(store.wakeDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".wake-") {
			t.Fatalf("temporary marker remained visible: %s", entry.Name())
		}
	}
}

func TestAutoRunPendingDebouncesNewMarkersAndHonorsMinimumInterval(t *testing.T) {
	store, clock := newAutoTestStore(t)
	enableAutoTestStore(t, store, func(settings *AutoSettings) {
		settings.Debounce = 30 * time.Second
		settings.MinimumInterval = time.Minute
	})
	if _, err := store.RecordWake(AutoWakeTerminalFlush); err != nil {
		t.Fatal(err)
	}
	var sleeps []time.Duration
	store.Sleep = func(_ context.Context, duration time.Duration) error {
		sleeps = append(sleeps, duration)
		if len(sleeps) == 1 {
			clock.Advance(10 * time.Second)
			if _, err := store.RecordWake(AutoWakeTerminalFlush); err != nil {
				return err
			}
			clock.Advance(duration - 10*time.Second)
			return nil
		}
		clock.Advance(duration)
		return nil
	}
	firstWorkAt := time.Time{}
	if _, err := store.RunPending(context.Background(), func(context.Context, AutoConfig) (AutoWorkResult, error) {
		firstWorkAt = clock.Now()
		return AutoWorkResult{Outcome: AutoOutcomeApplied}, nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(sleeps) != 2 || sleeps[0] != 30*time.Second || sleeps[1] != 10*time.Second {
		t.Fatalf("debounce sleeps = %v", sleeps)
	}
	if want := time.Date(2026, 7, 14, 18, 0, 40, 0, time.UTC); !firstWorkAt.Equal(want) {
		t.Fatalf("first work at %s, want %s", firstWorkAt, want)
	}

	if _, err := store.RecordWake(AutoWakeTerminalFlush); err != nil {
		t.Fatal(err)
	}
	sleeps = nil
	secondWorkAt := time.Time{}
	if _, err := store.RunPending(context.Background(), func(context.Context, AutoConfig) (AutoWorkResult, error) {
		secondWorkAt = clock.Now()
		return AutoWorkResult{Outcome: AutoOutcomeNoWork}, nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(sleeps) != 1 || sleeps[0] != time.Minute {
		t.Fatalf("minimum-interval sleeps = %v", sleeps)
	}
	if want := firstWorkAt.Add(time.Minute); !secondWorkAt.Equal(want) {
		t.Fatalf("second work at %s, want %s", secondWorkAt, want)
	}
}

func TestAutoRunPendingPersistsBoundedValueFreeBackoff(t *testing.T) {
	store, clock := newAutoTestStore(t)
	enableAutoTestStore(t, store, func(settings *AutoSettings) {
		settings.Debounce = time.Second
		settings.MinimumInterval = 0
	})
	if _, err := store.RecordWake(AutoWakeTerminalFlush); err != nil {
		t.Fatal(err)
	}
	clock.Advance(time.Second)
	const secret = "private narrative body must never reach status"
	_, err := store.RunPending(context.Background(), func(context.Context, AutoConfig) (AutoWorkResult, error) {
		return AutoWorkResult{}, NewAutoWorkError(AutoFailurePreflight, errors.New(secret))
	})
	if err == nil || !strings.Contains(err.Error(), secret) {
		t.Fatalf("RunPending() error = %v", err)
	}
	status, err := store.LoadStatus()
	if err != nil {
		t.Fatal(err)
	}
	if status.State != AutoStateBackoff || status.LastFailureCode != AutoFailurePreflight ||
		status.ConsecutiveFailures != 1 || status.RetryNotBefore == nil ||
		!status.RetryNotBefore.Equal(clock.Now().Add(time.Minute)) {
		t.Fatalf("failure status = %#v", status)
	}
	raw, err := os.ReadFile(store.statusPath())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), secret) {
		t.Fatalf("status leaked error detail: %s", raw)
	}
	pending, err := store.PendingWakes()
	if err != nil || len(pending) != 1 {
		t.Fatalf("pending after failure = %#v, %v", pending, err)
	}

	var slept time.Duration
	store.Sleep = func(_ context.Context, duration time.Duration) error {
		slept += duration
		clock.Advance(duration)
		return nil
	}
	result, err := store.RunPending(context.Background(), func(context.Context, AutoConfig) (AutoWorkResult, error) {
		return AutoWorkResult{Outcome: AutoOutcomeApplied}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if slept != time.Minute || result.Outcome != AutoOutcomeApplied || result.PendingWakeCount != 0 {
		t.Fatalf("recovery result/sleep = %#v / %s", result, slept)
	}
	status, err = store.LoadStatus()
	if err != nil {
		t.Fatal(err)
	}
	if status.State != AutoStateIdle || status.ConsecutiveFailures != 0 || status.RetryNotBefore != nil || status.LastFailureCode != "" {
		t.Fatalf("recovered status = %#v", status)
	}

	if got := NewAutoWorkError("secret_value", errors.New("detail")).(*AutoWorkError).Code; got != AutoFailureWorker {
		t.Fatalf("unrecognized failure code = %q, want %q", got, AutoFailureWorker)
	}
	if got := autoFailureBackoff(100); got != maxAutoFailureBackoff {
		t.Fatalf("bounded backoff = %s, want %s", got, maxAutoFailureBackoff)
	}
}

func TestAutoRunPendingBoundsRunsAndRetainsWakeForMoreWork(t *testing.T) {
	store, clock := newAutoTestStore(t)
	enableAutoTestStore(t, store, func(settings *AutoSettings) {
		settings.Debounce = time.Second
		settings.MinimumInterval = 0
		settings.MaxRuns = 2
		settings.ApplyPolicy = ApplyPolicyApply
	})
	if _, err := store.RecordWake(AutoWakeTerminalFlush); err != nil {
		t.Fatal(err)
	}
	clock.Advance(time.Second)
	calls := 0
	result, err := store.RunPending(context.Background(), func(context.Context, AutoConfig) (AutoWorkResult, error) {
		calls++
		return AutoWorkResult{Outcome: AutoOutcomeApplied, MoreWork: true}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if calls != 2 || result.Runs != 2 || !result.MoreWork || result.PendingWakeCount != 1 {
		t.Fatalf("bounded result = %#v, calls = %d", result, calls)
	}
	result, err = store.RunPending(context.Background(), func(context.Context, AutoConfig) (AutoWorkResult, error) {
		return AutoWorkResult{Outcome: AutoOutcomeNoWork}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.PendingWakeCount != 0 || result.MoreWork {
		t.Fatalf("drain result = %#v", result)
	}
}

func TestAutoStoreAgentSingleflightUsesPersistentAdvisoryLock(t *testing.T) {
	store, _ := newAutoTestStore(t)
	if err := os.MkdirAll(store.Root, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(store.lockPath(), []byte("malformed crashed-owner state"), 0o644); err != nil {
		t.Fatal(err)
	}
	releaseFirst, acquired, err := store.acquire()
	if err != nil || !acquired {
		t.Fatalf("first acquire over malformed contents = %t, %v", acquired, err)
	}
	_, acquired, err = store.acquire()
	if err != nil || acquired {
		t.Fatalf("concurrent contender acquire = %t, %v", acquired, err)
	}
	releaseFirst()

	releaseSecond, acquired, err := store.acquire()
	if err != nil || !acquired {
		t.Fatalf("acquire after release = %t, %v", acquired, err)
	}
	releaseSecond()
	info, err := os.Stat(store.lockPath())
	if err != nil {
		t.Fatalf("persistent lock stat = %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("lock mode = %#o, want 0600", got)
	}
	raw, err := os.ReadFile(store.lockPath())
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != AutoLockSchemaV1+"\n" {
		t.Fatalf("lock contents = %q", raw)
	}
}

func TestAutoStoreAdvisoryLockIsReleasedWhenOwnerCrashes(t *testing.T) {
	root := filepath.Join(t.TempDir(), "curation", "agent_1")
	readyPath := filepath.Join(t.TempDir(), "lock-ready")
	var output bytes.Buffer
	command := exec.Command(os.Args[0], "-test.run=^TestAutoStoreAdvisoryLockCrashHelper$", "-test.count=1")
	command.Env = append(os.Environ(),
		"WITSELF_AUTO_FLOCK_CRASH_HELPER=1",
		"WITSELF_AUTO_FLOCK_ROOT="+root,
		"WITSELF_AUTO_FLOCK_READY="+readyPath,
	)
	command.Stdout, command.Stderr = &output, &output
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	wait := make(chan error, 1)
	go func() { wait <- command.Wait() }()
	childDone := false
	defer func() {
		if !childDone {
			_ = command.Process.Kill()
			<-wait
		}
	}()

	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
waitForReady:
	for {
		if _, err := os.Stat(readyPath); err == nil {
			break waitForReady
		} else if !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
		select {
		case err := <-wait:
			childDone = true
			t.Fatalf("lock helper exited before ready: %v\n%s", err, output.String())
		case <-deadline.C:
			t.Fatalf("timed out waiting for lock helper\n%s", output.String())
		case <-ticker.C:
		}
	}

	store, err := NewAutoStore(root, "agent_1")
	if err != nil {
		t.Fatal(err)
	}
	if release, acquired, err := store.acquire(); err != nil || acquired {
		if acquired {
			release()
		}
		t.Fatalf("contender while child is alive = %t, %v", acquired, err)
	}
	if err := command.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	if err := <-wait; err == nil {
		childDone = true
		t.Fatal("killed lock helper exited without an error")
	}
	childDone = true

	release, acquired, err := store.acquire()
	if err != nil || !acquired {
		t.Fatalf("immediate acquire after crashed owner = %t, %v", acquired, err)
	}
	release()
}

func TestAutoStoreAdvisoryLockCrashHelper(t *testing.T) {
	if os.Getenv("WITSELF_AUTO_FLOCK_CRASH_HELPER") != "1" {
		return
	}
	store, err := NewAutoStore(os.Getenv("WITSELF_AUTO_FLOCK_ROOT"), "agent_1")
	if err != nil {
		t.Fatal(err)
	}
	release, acquired, err := store.acquire()
	if err != nil || !acquired {
		t.Fatalf("helper acquire = %t, %v", acquired, err)
	}
	defer release()
	if err := os.WriteFile(os.Getenv("WITSELF_AUTO_FLOCK_READY"), []byte("ready\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for {
		time.Sleep(time.Second)
	}
}

func TestAutoRunPendingCancellationAndDisablePreserveWake(t *testing.T) {
	store, _ := newAutoTestStore(t)
	enableAutoTestStore(t, store, nil)
	if _, err := store.RecordWake(AutoWakeTerminalFlush); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	called := false
	if _, err := store.RunPending(ctx, func(context.Context, AutoConfig) (AutoWorkResult, error) {
		called = true
		return AutoWorkResult{Outcome: AutoOutcomeNoWork}, nil
	}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled RunPending() error = %v", err)
	}
	if called {
		t.Fatal("work ran after context cancellation")
	}
	if err := store.Disable(); err != nil {
		t.Fatal(err)
	}
	if _, err := store.RunPending(context.Background(), func(context.Context, AutoConfig) (AutoWorkResult, error) {
		called = true
		return AutoWorkResult{Outcome: AutoOutcomeNoWork}, nil
	}); !errors.Is(err, ErrAutoDisabled) {
		t.Fatalf("disabled RunPending() error = %v", err)
	}
	inspection, err := store.Inspect()
	if err != nil {
		t.Fatal(err)
	}
	if inspection.Config.Enabled || inspection.Status.State != AutoStateDisabled || inspection.PendingWakeCount != 1 {
		t.Fatalf("disabled inspection = %#v", inspection)
	}
}

func TestAutoDisableDuringWorkStopsFurtherRunsAndPreservesDisabledStatus(t *testing.T) {
	store, clock := newAutoTestStore(t)
	enableAutoTestStore(t, store, func(settings *AutoSettings) {
		settings.Debounce = time.Second
		settings.MinimumInterval = 0
		settings.MaxRuns = 3
	})
	if _, err := store.RecordWake(AutoWakeTerminalFlush); err != nil {
		t.Fatal(err)
	}
	clock.Advance(time.Second)
	calls := 0
	result, err := store.RunPending(context.Background(), func(context.Context, AutoConfig) (AutoWorkResult, error) {
		calls++
		if err := store.Disable(); err != nil {
			return AutoWorkResult{}, err
		}
		return AutoWorkResult{Outcome: AutoOutcomeApplied, MoreWork: true}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if calls != 1 || result.Runs != 1 || !result.MoreWork || result.PendingWakeCount != 1 {
		t.Fatalf("disabled-during-work result = %#v, calls = %d", result, calls)
	}
	status, err := store.LoadStatus()
	if err != nil {
		t.Fatal(err)
	}
	if status.State != AutoStateDisabled || status.LastOutcome != AutoOutcomeApplied || status.RetryNotBefore != nil {
		t.Fatalf("disabled-during-work status = %#v", status)
	}
}

func TestAutoStoreRejectsBroadPermissionsAndCoexistsWithLaunchState(t *testing.T) {
	store, _ := newAutoTestStore(t)
	enableAutoTestStore(t, store, nil)
	launchStore := FileStateStore{Root: store.Root}
	launch := validLaunchState(PhaseStarted)
	if err := launchStore.Save(launch); err != nil {
		t.Fatal(err)
	}
	if _, err := launchStore.Load(launch.LaunchID); err != nil {
		t.Fatalf("existing launch state no longer loads: %v", err)
	}
	if err := os.Chmod(store.configPath(), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := store.LoadConfig(); err == nil || !strings.Contains(err.Error(), "group or other") {
		t.Fatalf("LoadConfig() with broad permissions error = %v", err)
	}
}

func TestAutoStoreRejectsArbitraryWakeReasonAndInvalidWorkerResult(t *testing.T) {
	store, clock := newAutoTestStore(t)
	enableAutoTestStore(t, store, func(settings *AutoSettings) {
		settings.Debounce = time.Second
		settings.MinimumInterval = 0
	})
	if _, err := store.RecordWake(AutoWakeReason("private_narrative_body")); err == nil {
		t.Fatal("RecordWake() accepted an arbitrary reason")
	}
	if _, err := store.RecordWake(AutoWakeManualPoll); err != nil {
		t.Fatal(err)
	}
	clock.Advance(time.Second)
	if _, err := store.RunPending(context.Background(), func(context.Context, AutoConfig) (AutoWorkResult, error) {
		return AutoWorkResult{Outcome: AutoOutcomeNoWork, MoreWork: true}, nil
	}); !errors.Is(err, ErrAutoInvalidResult) {
		t.Fatalf("invalid worker result error = %v", err)
	}
	status, err := store.LoadStatus()
	if err != nil {
		t.Fatal(err)
	}
	if status.LastFailureCode != AutoFailureContract || status.ConsecutiveFailures != 1 {
		t.Fatalf("invalid-result status = %#v", status)
	}
}
