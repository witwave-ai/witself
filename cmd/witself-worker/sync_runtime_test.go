package main

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/witwave-ai/witself/internal/runtimeconfig"
	"github.com/witwave-ai/witself/internal/store"
	"github.com/witwave-ai/witself/internal/testenv"
	"github.com/witwave-ai/witself/internal/worker"
)

type workerSyncMigrator struct {
	core func() error
	sync func(context.Context) error
}

func (m workerSyncMigrator) Migrate() error                        { return m.core() }
func (m workerSyncMigrator) MigrateSync(ctx context.Context) error { return m.sync(ctx) }

func TestWorkerLocalRuntimeSyncMigrationSelection(t *testing.T) {
	for _, local := range []bool{false, true} {
		for _, selected := range []bool{false, true} {
			calls := []string{}
			cfg := runtimeconfig.Config{Local: local, SyncSchema: selected, MigrationWait: time.Second}
			migrator := workerSyncMigrator{
				core: func() error { calls = append(calls, "core"); return nil },
				sync: func(ctx context.Context) error {
					calls = append(calls, "sync")
					if deadline, ok := ctx.Deadline(); !ok || time.Until(deadline) > time.Second || time.Until(deadline) <= 0 {
						t.Fatal("sync migration did not inherit the bounded local wait")
					}
					return nil
				},
			}
			if err := migrateWorkerStore(context.Background(), migrator, cfg); err != nil {
				t.Fatal(err)
			}
			want := []string{"core"}
			if local && selected {
				want = append(want, "sync")
			}
			if !reflect.DeepEqual(calls, want) {
				t.Fatal("sync migration ran without both explicit local and sync selection")
			}
		}
	}
	stop := errors.New("fixture migration stop")
	migrator := workerSyncMigrator{core: func() error { return stop }, sync: func(context.Context) error { t.Fatal("sync migration ran after core failure"); return nil }}
	if err := migrateWorkerStore(context.Background(), migrator, runtimeconfig.Config{Local: true, SyncSchema: true}); !errors.Is(err, stop) {
		t.Fatal("core migration failure was not preserved")
	}
	migrator.core = func() error { return nil }
	migrator.sync = func(ctx context.Context) error { <-ctx.Done(); return ctx.Err() }
	if err := migrateWorkerStore(context.Background(), migrator, runtimeconfig.Config{Local: true, SyncSchema: true, MigrationWait: time.Millisecond}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("sync migration wait was not cancellable")
	}
}

type workerSyncReadinessStore struct {
	workerBootstrapStore
	syncVersion int
	reads       *int
}

func (s workerSyncReadinessStore) AppliedSyncSchemaVersion(ctx context.Context) (int, error) {
	*s.reads++
	if _, ok := ctx.Deadline(); !ok {
		return 0, errors.New("sync readiness is not bounded")
	}
	return s.syncVersion, nil
}

func TestWorkerLocalRuntimeSyncReadinessSelection(t *testing.T) {
	for _, local := range []bool{false, true} {
		for _, selected := range []bool{false, true} {
			for _, version := range []int{0, store.SyncSchemaVersion(), store.SyncSchemaVersion() + 1} {
				reads := 0
				st := workerSyncReadinessStore{workerBootstrapStore: workerBootstrapStore{account: "fixture-account", version: store.SchemaVersion()}, syncVersion: version, reads: &reads}
				cfg := runtimeconfig.Config{Local: local, SyncSchema: selected}
				captureWorkerStderr(t, func() {
					code := serveWorker(context.Background(), nil, st, cfg, jobConfig{}, mapLookup(nil), func(_ *worker.Registry, ctx context.Context, wc worker.Config) error {
						if (wc.SyncSchemaReady != nil) != (local && selected) {
							t.Fatal("sync readiness selection did not reach worker configuration")
						}
						if err := wc.Ready(ctx); err != nil {
							t.Fatal("sync schema status changed core readiness")
						}
						if wc.SyncSchemaReady != nil {
							err := wc.SyncSchemaReady(ctx)
							if (err == nil) != (version == store.SyncSchemaVersion()) {
								t.Fatal("sync readiness accepted the wrong installed schema")
							}
						}
						return nil
					})
					if code != 0 {
						t.Fatal("worker startup failed")
					}
				})
				wantReads := 0
				if local && selected {
					wantReads = 1
				}
				if reads != wantReads {
					t.Fatal("unselected worker read sync database state")
				}
			}
		}
	}
}

func TestWorkerLocalRuntimeSyncPostgres(t *testing.T) {
	for _, mode := range []string{"hosted", "local", "sync"} {
		t.Run(mode, func(t *testing.T) {
			fixture := testenv.NewPostgresSchema(t, testenv.RequirePostgres(t))
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			st, err := store.Open(ctx, fixture.DSN())
			if err != nil {
				t.Fatal("open worker startup fixture failed")
			}
			defer st.Close()
			cfg := runtimeconfig.Config{Local: mode != "hosted", SyncSchema: mode == "sync", MigrationWait: time.Minute}
			if err := migrateWorkerStore(ctx, st, cfg); err != nil {
				t.Fatal("worker startup migrations failed")
			}
			if _, err := st.EnsureDefaultAccount(ctx); err != nil {
				t.Fatal("create worker bootstrap account failed")
			}
			captureWorkerStderr(t, func() {
				code := serveWorker(ctx, st, st, cfg, jobConfig{}, mapLookup(nil), func(_ *worker.Registry, ctx context.Context, wc worker.Config) error {
					if err := wc.Ready(ctx); err != nil {
						t.Fatal("core worker readiness failed")
					}
					if (wc.SyncSchemaReady != nil) != cfg.SyncSchema {
						t.Fatal("worker sync readiness field did not match selection")
					}
					if wc.SyncSchemaReady != nil {
						if err := wc.SyncSchemaReady(ctx); err != nil {
							t.Fatal("selected worker sync readiness failed")
						}
					}
					return nil
				})
				if code != 0 {
					t.Fatal("worker serve path failed")
				}
			})
			db, err := pgx.Connect(ctx, fixture.DSN())
			if err != nil {
				t.Fatal("open worker schema observer failed")
			}
			defer func() { _ = db.Close(ctx) }()
			var tables int
			if err := db.QueryRow(ctx, `SELECT count(*) FROM pg_tables WHERE schemaname=current_schema() AND left(tablename,5)='sync_'`).Scan(&tables); err != nil {
				t.Fatal("read worker sync table inventory failed")
			}
			if (tables > 0) != cfg.SyncSchema {
				t.Fatal("worker startup created sync tables without explicit local sync selection")
			}
		})
	}
}
