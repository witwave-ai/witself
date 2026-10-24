package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/witwave-ai/witself/internal/server"
	"github.com/witwave-ai/witself/internal/store"
)

func clearServerRuntimeEnvironment(t *testing.T) {
	t.Helper()
	for _, entry := range os.Environ() {
		name, _, _ := strings.Cut(entry, "=")
		if strings.HasPrefix(name, "WITSELF_") && name != "WITSELF_TEST_DATABASE_URL" && name != "WITSELF_TEST_REQUIRE_DATABASE" {
			t.Setenv(name, "")
			if err := os.Unsetenv(name); err != nil {
				t.Fatal("unset test configuration")
			}
		}
	}
	t.Setenv("WITSELF_HOME", t.TempDir())
	t.Setenv("DSH_HOME", t.TempDir())
}

func TestServerRuntimeConfiguration(t *testing.T) {
	for _, selector := range []string{"unset", "", "local", "private-selector-value"} {
		t.Run(selector, func(t *testing.T) {
			clearServerRuntimeEnvironment(t)
			if selector != "unset" {
				t.Setenv("WITSELF_RUNTIME", selector)
			}
			spool := t.TempDir()
			spool, err := filepath.EvalSymlinks(spool)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(spool, 0o700); err != nil {
				t.Fatal(err)
			}
			t.Setenv("WITSELF_LOCAL_SPOOL_DIR", spool)
			t.Setenv("WITSELF_DATABASE_URL", "postgres://fixture.invalid/test")
			cfg, runtime, err := serverRuntimeConfigFromEnv()
			if selector == "private-selector-value" {
				if err == nil || !strings.Contains(err.Error(), "WITSELF_RUNTIME") || strings.Contains(err.Error(), selector) {
					t.Fatal("unknown selector was not refused value-free")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if cfg.SpoolRoot != nil {
				t.Cleanup(func() { _ = cfg.SpoolRoot.Close() })
			}
			if selector != "local" {
				want := server.Config{APIAddr: ":8080", HealthAddr: ":8081", MetricsAddr: ":9090"}
				if runtime.Local || !reflect.DeepEqual(cfg, want) {
					t.Fatal("hosted configuration differs from pre-local snapshot")
				}
				return
			}
			if !runtime.Local || !cfg.LocalRuntime || cfg.SpoolDir != spool || cfg.SpoolRoot == nil || cfg.APIAddr != "127.0.0.1:8080" || cfg.HealthAddr != "127.0.0.1:8081" || cfg.MetricsAddr != "127.0.0.1:9090" {
				t.Fatal("local runtime overrides were not wired to server configuration")
			}
		})
	}
}

func TestServerRuntimeDispatchGuard(t *testing.T) {
	// Include every literal in dispatchServerCommand's top-level switch. The
	// dispatch seam stands in for all handlers, including store/client creation.
	commands := []struct {
		name    string
		allowed bool
	}{
		{"version", false}, {"--version", true}, {"-v", true},
		{"help", false}, {"--help", true}, {"-h", true},
		{"serve", true}, {"agent-email", false},
		{"unknown-command", false}, {"", false},
	}
	for _, selector := range []string{"unset", "", "local", "private-unknown-runtime"} {
		for _, command := range commands {
			t.Run(selector+"/"+command.name, func(t *testing.T) {
				var stderr bytes.Buffer
				lookups, dispatched := 0, false
				allowed := selector == "unset" || selector == "" || command.allowed
				lookup := func(name string) (string, bool) {
					lookups++
					if name != "WITSELF_RUNTIME" {
						t.Fatalf("dispatcher inspected another variable: %s", name)
					}
					if selector == "unset" {
						return "", false
					}
					return selector, true
				}
				var args []string
				if command.name != "" {
					args = []string{command.name}
				}
				dispatch := func(got []string) int {
					if !allowed {
						t.Fatal("blocked command reached its store/client dispatch seam")
					}
					if !reflect.DeepEqual(got, args) {
						t.Fatal("dispatcher changed command arguments")
					}
					dispatched = true
					return 37
				}
				status := runWithRuntime(args, lookup, dispatch, &stderr)
				if lookups != 1 {
					t.Fatal("runtime selector must be the only environment lookup")
				}
				if allowed {
					if status != 37 || !dispatched || stderr.Len() != 0 {
						t.Fatal("allowed command dispatch changed")
					}
				} else if status != 1 || dispatched || stderr.String() != "operational subcommands are unavailable while WITSELF_RUNTIME is set\n" {
					t.Fatal("operational command did not receive the exact value-free refusal")
				}
			})
		}
	}
}

type blockingLocalReadinessStore struct {
	deadline chan time.Time
}

func (s blockingLocalReadinessStore) Ping(ctx context.Context) error {
	deadline, _ := ctx.Deadline()
	s.deadline <- deadline
	<-ctx.Done()
	return ctx.Err()
}

func (blockingLocalReadinessStore) AppliedSchemaVersion(context.Context) (int, error) {
	return store.SchemaVersion(), nil
}

func (blockingLocalReadinessStore) LocalBootstrapAccount(context.Context) (string, error) {
	return "local-test-account", nil
}

func TestServerLocalReadinessDeadline(t *testing.T) {
	st := blockingLocalReadinessStore{deadline: make(chan time.Time, 1)}
	cfg := server.Config{LocalRuntime: true}
	if err := configureLocalStore(context.Background(), &cfg, st, "local-test-account"); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	started := time.Now()
	done := make(chan error, 1)
	go func() { done <- cfg.Ready(ctx) }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("blocked database passed readiness")
		}
	case <-time.After(3 * time.Second):
		cancel()
		<-done
		t.Fatal("local readiness did not return within its two-second deadline")
	}
	deadline := <-st.deadline
	if deadline.IsZero() || deadline.Before(started) || deadline.After(started.Add(2100*time.Millisecond)) {
		t.Fatal("local readiness did not give the store a two-second deadline")
	}
}

type localVersionStore struct {
	*store.Store
	version int
}

func (s localVersionStore) AppliedSchemaVersion(context.Context) (int, error) {
	return s.version, nil
}

func TestConfigureLocalStorePostgres(t *testing.T) {
	st := startupCohortStore(t)
	ctx := context.Background()
	cfg := server.Config{LocalRuntime: true}
	if err := configureLocalStore(ctx, &cfg, st, ""); err == nil {
		t.Fatal("missing bootstrap account accepted at startup")
	}
	accountID, err := st.EnsureDefaultAccount(ctx)
	if err != nil {
		t.Fatal(err)
	}
	const mismatch = "private-wrong-account"
	if err := configureLocalStore(ctx, &cfg, st, mismatch); err == nil || !strings.Contains(err.Error(), "WITSELF_LOCAL_ACCOUNT_ID") || strings.Contains(err.Error(), mismatch) || strings.Contains(err.Error(), accountID) {
		t.Fatal("account mismatch was not refused value-free")
	}
	if err := configureLocalStore(ctx, &cfg, st, accountID); err != nil {
		t.Fatal(err)
	}
	if err := cfg.Ready(ctx); err != nil {
		t.Fatal(err)
	}
	if got, err := cfg.AppliedSchemaVersion(ctx); err != nil || got != store.SchemaVersion() {
		t.Fatal("database schema reader was not wired", err)
	}
	for _, version := range []int{store.SchemaVersion() - 1, store.SchemaVersion() + 1} {
		if err := configureLocalStore(ctx, &cfg, localVersionStore{Store: st, version: version}, accountID); err != nil {
			t.Fatal(err)
		}
		if err := cfg.Ready(ctx); err == nil {
			t.Fatal("schema mismatch passed readiness")
		}
		if got, err := cfg.AppliedSchemaVersion(ctx); err != nil || got != version {
			t.Fatal("applied reader substituted compiled version", err)
		}
	}
	if err := configureLocalStore(ctx, &cfg, st, accountID); err != nil {
		t.Fatal(err)
	}
	st.Close()
	if err := cfg.Ready(ctx); err == nil {
		t.Fatal("closed database passed readiness")
	}
}

// The production entry point must consult the selector itself; the guard tests
// above exercise the injected helper, so a disconnected caller would survive them.
func TestServerRunEntryHonorsRuntimeSelector(t *testing.T) {
	for _, selector := range []string{"local", "private-unknown-runtime"} {
		t.Run(selector, func(t *testing.T) {
			t.Setenv("WITSELF_RUNTIME", selector)
			for _, args := range [][]string{{"version"}, {"agent-email", "canary-manifest"}, nil} {
				if code := run(args); code != 1 {
					t.Fatalf("run(%q) under selector %q = %d, want 1", args, selector, code)
				}
			}
		})
	}
	t.Setenv("WITSELF_RUNTIME", "")
	if code := run([]string{"version"}); code != 0 {
		t.Fatalf("hosted run(version) = %d, want 0", code)
	}
}
