package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/witwave-ai/witself/internal/server"
	"github.com/witwave-ai/witself/internal/store"
	"github.com/witwave-ai/witself/internal/testenv"
)

type startupMigrationStub struct {
	unbounded func() error
	bounded   func(context.Context) error
}

func (s startupMigrationStub) Migrate() error { return s.unbounded() }
func (s startupMigrationStub) MigrateContext(ctx context.Context) error {
	return s.bounded(ctx)
}

func migrationStartupEnvironment(t *testing.T) {
	t.Helper()
	dsn := testenv.RequirePostgres(t)
	clearServerRuntimeEnvironment(t)
	t.Setenv("WITSELF_DATABASE_URL", dsn)
	// Keep the hosted spool sweep within this fixture too.
	t.Setenv("TMPDIR", t.TempDir())
	t.Setenv("TMP", t.TempDir())
	t.Setenv("TEMP", t.TempDir())
}

func captureMigrationStartup(t *testing.T, startup serverStartup) (int, string) {
	t.Helper()
	log, err := os.CreateTemp(t.TempDir(), "startup-log-")
	if err != nil {
		t.Fatal(err)
	}
	previous := os.Stderr
	os.Stderr = log
	defer func() {
		os.Stderr = previous
		_ = log.Close()
	}()
	status := serveWithStartup(startup)
	output, err := os.ReadFile(log.Name())
	if err != nil {
		t.Fatal(err)
	}
	return status, string(output)
}

func TestServerMigrationStartupHostedUsesOnlyUnbounded(t *testing.T) {
	for _, selector := range []string{"unset", "empty"} {
		t.Run(selector, func(t *testing.T) {
			migrationStartupEnvironment(t)
			if selector == "empty" {
				t.Setenv("WITSELF_RUNTIME", "")
			}
			// A local-only setting must have no effect without the selector.
			t.Setenv("WITSELF_LOCAL_MIGRATION_WAIT", "17ms")
			unbounded, bounded, listeners := 0, 0, 0
			stopMigration := errors.New("fixture migration stop")
			startup := serverStartup{
				migrator: func(*store.Store) startupMigrator {
					return startupMigrationStub{
						unbounded: func() error { unbounded++; return stopMigration },
						bounded: func(context.Context) error {
							bounded++
							t.Error("hosted startup called bounded MigrateContext")
							return stopMigration
						},
					}
				},
				notifyContext: func(ctx context.Context, _ ...os.Signal) (context.Context, context.CancelFunc) {
					return context.WithCancel(ctx)
				},
				run: func(context.Context, server.Config) error { listeners++; return nil },
			}
			status, output := captureMigrationStartup(t, startup)
			if status != 1 || unbounded != 1 || bounded != 0 || listeners != 0 {
				t.Fatalf("status=%d unbounded=%d bounded=%d listeners=%d", status, unbounded, bounded, listeners)
			}
			if strings.Count(output, "witself-server: fixture migration stop\n") != 1 || strings.Contains(output, "migration wait exceeded") {
				t.Fatal("hosted startup changed its ordinary migration error diagnostic")
			}
		})
	}
}

func TestServerMigrationStartupLocalCancellationBeforeListeners(t *testing.T) {
	for _, mode := range []string{"signal", "deadline"} {
		t.Run(mode, func(t *testing.T) {
			migrationStartupEnvironment(t)
			t.Setenv("WITSELF_RUNTIME", "local")
			spool, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(spool, 0o700); err != nil {
				t.Fatal(err)
			}
			t.Setenv("WITSELF_LOCAL_SPOOL_DIR", spool)
			wait := 100 * time.Millisecond
			if mode == "signal" {
				wait = 5 * time.Second
			}
			t.Setenv("WITSELF_LOCAL_MIGRATION_WAIT", wait.String())
			unbounded, bounded, listeners, signalContexts := 0, 0, 0, 0
			var signalContext context.Context
			var signalCancel context.CancelFunc
			startup := serverStartup{
				notifyContext: func(parent context.Context, signals ...os.Signal) (context.Context, context.CancelFunc) {
					signalContexts++
					if !reflect.DeepEqual(signals, []os.Signal{syscall.SIGINT, syscall.SIGTERM}) {
						t.Fatal("serve changed its shutdown signals")
					}
					signalContext, signalCancel = context.WithCancel(parent)
					return signalContext, signalCancel
				},
				migrator: func(st *store.Store) startupMigrator {
					beforeBound := time.Now()
					return startupMigrationStub{
						unbounded: func() error {
							unbounded++
							return errors.New("local startup called unbounded Migrate")
						},
						bounded: func(ctx context.Context) error {
							bounded++
							afterBound := time.Now()
							deadline, ok := ctx.Deadline()
							if !ok || deadline.Before(beforeBound.Add(wait)) || deadline.After(afterBound.Add(wait)) {
								t.Fatal("migration deadline does not equal configured wait from startup")
							}
							if signalContext == nil || signalContext.Err() != nil {
								t.Fatal("migration did not start under the live shutdown context")
							}
							if mode == "signal" {
								signalCancel()
							}
							select {
							case <-ctx.Done():
							case <-time.After(10 * wait):
								t.Fatal("bounded migration did not observe shutdown or deadline")
							}
							want := context.DeadlineExceeded
							if mode == "signal" {
								want = context.Canceled
							}
							if !errors.Is(ctx.Err(), want) {
								t.Fatalf("migration context error = %v, want %v", ctx.Err(), want)
							}
							// An already-canceled context produces the real typed store
							// sentinel before it can acquire a database connection.
							canceled := st.MigrateContext(ctx)
							if !errors.Is(canceled, store.ErrMigrationWaitCanceled) || store.MigrationWaitPhase(canceled) != "advisory lock" {
								t.Fatal("fixture did not produce the typed migration cancellation")
							}
							return fmt.Errorf("private fixture detail\nnot a log line: %w", canceled)
						},
					}
				},
				run: func(context.Context, server.Config) error { listeners++; return nil },
			}
			status, output := captureMigrationStartup(t, startup)
			if status != 1 || bounded != 1 || unbounded != 0 || listeners != 0 || signalContexts != 1 {
				t.Fatalf("status=%d bounded=%d unbounded=%d listeners=%d signalContexts=%d", status, bounded, unbounded, listeners, signalContexts)
			}
			want := "witself-server: migration wait exceeded " + wait.String() + " during advisory lock\n"
			if strings.Count(output, want) != 1 || strings.Count(output, "migration wait exceeded") != 1 || !strings.HasSuffix(output, want) || strings.Contains(output, "private fixture") || strings.Contains(output, "not a log line") {
				t.Fatal("cancellation did not log exactly one value-free duration/phase diagnostic")
			}
		})
	}
}
