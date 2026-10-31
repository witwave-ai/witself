package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/witwave-ai/witself/internal/server"
	"github.com/witwave-ai/witself/internal/store"
	"github.com/witwave-ai/witself/internal/testenv"
)

type serverSyncReadinessStore struct {
	version int
	reads   int
	block   bool
}

func (s *serverSyncReadinessStore) AppliedSyncSchemaVersion(ctx context.Context) (int, error) {
	s.reads++
	if _, ok := ctx.Deadline(); !ok {
		return 0, errors.New("unbounded readiness")
	}
	if s.block {
		<-ctx.Done()
		return 0, ctx.Err()
	}
	return s.version, nil
}

func TestServerLocalRuntimeSyncReadiness(t *testing.T) {
	for _, version := range []int{0, store.SyncSchemaVersion(), store.SyncSchemaVersion() + 1} {
		st := &serverSyncReadinessStore{version: version}
		cfg := server.Config{Ready: func(context.Context) error { return nil }}
		configureLocalSyncStore(&cfg, st)
		if err := cfg.Ready(context.Background()); err != nil {
			t.Fatal("sync schema setup changed core readiness")
		}
		if err := cfg.SyncSchemaReady(context.Background()); (err == nil) != (version == store.SyncSchemaVersion()) {
			t.Fatal("sync readiness did not require the installed version")
		}
		if st.reads != 1 {
			t.Fatal("sync readiness did not read current installed state")
		}
	}
	st := &serverSyncReadinessStore{block: true}
	cfg := server.Config{}
	configureLocalSyncStore(&cfg, st)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := cfg.SyncSchemaReady(ctx); err == nil || err.Error() != "local sync schema is not ready" {
		t.Fatal("sync readiness did not fail closed on cancellation")
	}
}

func TestServerLocalRuntimeSyncStartupPostgres(t *testing.T) {
	for _, mode := range []string{"hosted", "local", "sync"} {
		t.Run(mode, func(t *testing.T) {
			base := testenv.RequirePostgres(t)
			fixture := testenv.NewPostgresSchema(t, base)
			clearServerRuntimeEnvironment(t)
			// Exercise the real serve path without consulting any token file.
			t.Setenv("WITSELF_BOOTSTRAP_TOKEN", "local-sync-startup-fixture-token")
			t.Setenv("WITSELF_DATABASE_URL", fixture.DSN())
			t.Setenv("TMPDIR", t.TempDir())
			t.Setenv("TMP", t.TempDir())
			t.Setenv("TEMP", t.TempDir())
			if mode != "hosted" {
				t.Setenv("WITSELF_RUNTIME", "local")
				spool, err := filepath.EvalSymlinks(t.TempDir())
				if err != nil {
					t.Fatal("resolve fixture spool failed")
				}
				if err := os.Chmod(spool, 0700); err != nil {
					t.Fatal("protect fixture spool failed")
				}
				t.Setenv("WITSELF_LOCAL_SPOOL_DIR", spool)
			}
			if mode == "sync" {
				t.Setenv("WITSELF_LOCAL_SYNC_SCHEMA", "1")
			}
			coreCalls, syncCalls, listeners := 0, 0, 0
			var migrationContext context.Context
			startup := serverStartup{
				migrator: func(st *store.Store) startupMigrator {
					return startupMigrationStub{
						unbounded: func() error {
							if mode != "hosted" {
								t.Fatal("local startup used the unbounded core migrator")
							}
							coreCalls++
							return st.Migrate()
						},
						bounded: func(ctx context.Context) error {
							if mode == "hosted" {
								t.Fatal("hosted startup used the bounded core migrator")
							}
							coreCalls++
							migrationContext = ctx
							return st.MigrateContext(ctx)
						},
						sync: func(ctx context.Context) error {
							if mode != "sync" || coreCalls != 1 || ctx != migrationContext {
								t.Fatal("sync startup lost explicit selection, ordering, or migration deadline")
							}
							if _, ok := ctx.Deadline(); !ok {
								t.Fatal("sync migration was unbounded")
							}
							syncCalls++
							return st.MigrateSync(ctx)
						},
					}
				},
				notifyContext: func(ctx context.Context, _ ...os.Signal) (context.Context, context.CancelFunc) {
					return context.WithCancel(ctx)
				},
				run: func(ctx context.Context, cfg server.Config) error {
					listeners++
					if err := cfg.Ready(ctx); err != nil {
						t.Fatal("core readiness failed after startup")
					}
					if (cfg.SyncSchemaReady != nil) != (mode == "sync") {
						t.Fatal("sync readiness field did not match selection")
					}
					if cfg.SyncSchemaReady != nil {
						if err := cfg.SyncSchemaReady(ctx); err != nil {
							t.Fatal("selected sync readiness failed after migration")
						}
					}
					return nil
				},
			}
			status, _ := captureMigrationStartup(t, startup)
			wantSyncCalls := 0
			if mode == "sync" {
				wantSyncCalls = 1
			}
			if status != 0 || coreCalls != 1 || syncCalls != wantSyncCalls || listeners != 1 {
				t.Fatal("production startup migration/listener ordering failed")
			}
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			db, err := pgx.Connect(ctx, fixture.DSN())
			if err != nil {
				t.Fatal("open startup schema observer failed")
			}
			defer func() { _ = db.Close(ctx) }()
			var tables int
			if err := db.QueryRow(ctx, `SELECT count(*) FROM pg_tables WHERE schemaname=current_schema() AND left(tablename,5)='sync_'`).Scan(&tables); err != nil {
				t.Fatal("read startup table inventory failed")
			}
			if (tables > 0) != (mode == "sync") {
				t.Fatal("startup created sync tables without explicit local sync selection")
			}
		})
	}
}
