package messagerunner

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/witwave-ai/witself/internal/client"
)

func TestConfigStoreEnablePersistsValueFreePrivateBinding(t *testing.T) {
	store := testConfigStore(t, "claude-code")
	config, err := store.Enable(testSettings("claude-code"))
	if err != nil {
		t.Fatal(err)
	}
	if !config.Enabled || config.RunnerID != "mrn_test" || config.Revision != 1 || config.AgentID != "agent_bob" {
		t.Fatalf("unexpected config: %+v", config)
	}
	info, err := os.Stat(store.configPath())
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("config mode = %#o, want 0600", got)
	}
	raw, err := os.ReadFile(store.configPath())
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"token", "message_body", "prompt", "provider_output"} {
		if containsFold(string(raw), forbidden) {
			t.Fatalf("persisted config contains forbidden field %q: %s", forbidden, raw)
		}
	}
	loaded, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if loaded != config {
		t.Fatalf("loaded config = %+v, want %+v", loaded, config)
	}
}

func TestConfigStorePreservesRunnerIDForSameBinding(t *testing.T) {
	store := testConfigStore(t, "claude-code")
	first, err := store.Enable(testSettings("claude-code"))
	if err != nil {
		t.Fatal(err)
	}
	settings := testSettings("claude-code")
	settings.Model = "new-model"
	second, err := store.Enable(settings)
	if err != nil {
		t.Fatal(err)
	}
	if second.RunnerID != first.RunnerID || second.Revision != 2 || second.CreatedAt != first.CreatedAt {
		t.Fatalf("runner identity was not preserved: first=%+v second=%+v", first, second)
	}
	if err := store.RecordNotification(context.Background(), testRunnerNotificationMessage("msg_same")); err != nil {
		t.Fatal(err)
	}
	settings.Model = "third-model"
	if _, err := store.Enable(settings); err != nil {
		t.Fatal(err)
	}
	if notifications, err := store.Notifications(context.Background()); err != nil || len(notifications) != 1 {
		t.Fatalf("same-binding notifications = %#v / %v", notifications, err)
	}
}

func TestConfigStoreRejectsBindingReplacementWithoutAuthority(t *testing.T) {
	store := testConfigStore(t, "claude-code")
	if _, err := store.Enable(testSettings("claude-code")); err != nil {
		t.Fatal(err)
	}
	if err := store.RecordNotification(context.Background(), testRunnerNotificationMessage("msg_old_binding")); err != nil {
		t.Fatal(err)
	}
	settings := testSettings("claude-code")
	settings.AgentID = "agent_alice"
	settings.AgentName = "Alice"
	if _, err := store.Enable(settings); !errors.Is(err, ErrRunnerBindingConflict) {
		t.Fatalf("error = %v, want binding conflict", err)
	}
	settings.ReplaceBinding = true
	store.NewID = func(string) (string, error) { return "mrn_replaced", nil }
	replaced, err := store.Enable(settings)
	if err != nil {
		t.Fatal(err)
	}
	if replaced.RunnerID != "mrn_replaced" || replaced.AgentID != "agent_alice" || replaced.Revision != 1 {
		t.Fatalf("unexpected replacement: %+v", replaced)
	}
	if notifications, err := store.Notifications(context.Background()); err != nil || len(notifications) != 0 {
		t.Fatalf("replacement retained prior-binding notifications = %#v / %v", notifications, err)
	}
}

func testRunnerNotificationMessage(messageID string) client.Message {
	return client.Message{
		ID: messageID, ThreadID: "thr_test", Kind: "result", CreatedAt: time.Now().UTC(),
		From: client.MessageAgent{AgentID: "agent_peer", AgentName: "Peer"},
	}
}

func TestConfigStoreDisableIsIdempotent(t *testing.T) {
	store := testConfigStore(t, "grok-build")
	if _, err := store.Enable(testSettings("grok-build")); err != nil {
		t.Fatal(err)
	}
	disabled, err := store.Disable()
	if err != nil {
		t.Fatal(err)
	}
	again, err := store.Disable()
	if err != nil {
		t.Fatal(err)
	}
	if disabled.Enabled || again.Revision != disabled.Revision {
		t.Fatalf("disable was not idempotent: first=%+v second=%+v", disabled, again)
	}
}

func TestConfigStoreRejectsLoosePermissionsAndTrailingData(t *testing.T) {
	store := testConfigStore(t, "claude-code")
	if _, err := store.Enable(testSettings("claude-code")); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(store.configPath(), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Load(); err == nil {
		t.Fatal("expected loose config permissions to be rejected")
	}
	if err := os.Chmod(store.configPath(), 0o600); err != nil {
		t.Fatal(err)
	}
	file, err := os.OpenFile(store.configPath(), os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString("{}\n"); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Load(); err == nil {
		t.Fatal("expected trailing config data to be rejected")
	}
}

func TestConfigStoreAcquireIsSingleInstance(t *testing.T) {
	store := testConfigStore(t, "claude-code")
	release, acquired, err := store.Acquire()
	if err != nil || !acquired {
		t.Fatalf("first acquire = (%v, %v), want acquired", acquired, err)
	}
	defer func() { _ = release() }()
	secondRelease, secondAcquired, err := store.Acquire()
	if err != nil || secondAcquired {
		t.Fatalf("second acquire = (%v, %v), want busy", secondAcquired, err)
	}
	if err := secondRelease(); err != nil {
		t.Fatal(err)
	}
	if err := release(); err != nil {
		t.Fatal(err)
	}
	release = func() error { return nil }
	thirdRelease, thirdAcquired, err := store.Acquire()
	if err != nil || !thirdAcquired {
		t.Fatalf("third acquire = (%v, %v), want acquired", thirdAcquired, err)
	}
	if err := thirdRelease(); err != nil {
		t.Fatal(err)
	}
}

func TestNewConfigStoreRejectsTraversalRuntime(t *testing.T) {
	if _, err := NewConfigStore(t.TempDir(), "../claude-code"); !errors.Is(err, ErrInvalidConfiguration) {
		t.Fatalf("error = %v, want invalid configuration", err)
	}
}

func testConfigStore(t *testing.T, runtimeName string) ConfigStore {
	t.Helper()
	store, err := NewConfigStore(filepath.Join(t.TempDir(), "message-runners"), runtimeName)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 7, 14, 12, 0, 0, 0, time.UTC)
	store.Now = func() time.Time {
		now = now.Add(time.Second)
		return now
	}
	store.NewID = func(string) (string, error) { return "mrn_test", nil }
	return store
}

func testSettings(runtimeName string) Settings {
	return Settings{
		Runtime: runtimeName, AccountID: "account_1", RealmID: "realm_1",
		AgentID: "agent_bob", AgentName: "Bob", Provider: runtimeName,
		ProviderPath: "/usr/local/bin/provider", Model: "model-1", MaximumTurns: 12,
	}
}

func containsFold(text, substring string) bool {
	return len(substring) != 0 && len(text) >= len(substring) &&
		indexFold(text, substring) >= 0
}

func indexFold(text, substring string) int {
	lowerText := []byte(text)
	lowerSubstring := []byte(substring)
	for i := range lowerText {
		if lowerText[i] >= 'A' && lowerText[i] <= 'Z' {
			lowerText[i] += 'a' - 'A'
		}
	}
	for i := range lowerSubstring {
		if lowerSubstring[i] >= 'A' && lowerSubstring[i] <= 'Z' {
			lowerSubstring[i] += 'a' - 'A'
		}
	}
	for i := 0; i+len(lowerSubstring) <= len(lowerText); i++ {
		match := true
		for j := range lowerSubstring {
			if lowerText[i+j] != lowerSubstring[j] {
				match = false
				break
			}
		}
		if match {
			return i
		}
	}
	return -1
}
