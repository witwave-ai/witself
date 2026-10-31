package server

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/witwave-ai/witself/internal/runtimeconfig"
	"github.com/witwave-ai/witself/internal/store"
)

func TestLocalRuntimeSyncReadinessSeparation(t *testing.T) {
	for _, selected := range []bool{false, true} {
		for _, ready := range []bool{false, true} {
			cfg := Config{LocalRuntime: true, Ready: func(context.Context) error { return nil }}
			if selected {
				cfg.SyncSchemaReady = func(context.Context) error {
					if !ready {
						return errors.New("private-sync-diagnostic")
					}
					return nil
				}
			}
			handler := runtimeHealthMux(cfg)
			core := httptest.NewRecorder()
			handler.ServeHTTP(core, httptest.NewRequest(http.MethodGet, "/readyz", nil))
			if core.Code != http.StatusOK || core.Body.String() != "ok\n" {
				t.Fatal("sync readiness changed core readiness bytes")
			}
			sync := httptest.NewRecorder()
			handler.ServeHTTP(sync, httptest.NewRequest(http.MethodGet, "/sync-readyz", nil))
			wantStatus, wantBody := http.StatusNotFound, "404 page not found\n"
			if selected {
				wantStatus, wantBody = http.StatusServiceUnavailable, "local sync schema is not ready\n"
				if ready {
					wantStatus, wantBody = http.StatusOK, "ok\n"
				}
			}
			if sync.Code != wantStatus || sync.Body.String() != wantBody {
				t.Fatal("sync readiness route selection or fixed response changed")
			}
		}
	}
}

func TestHostedRuntimeNeverRegistersSyncReadiness(t *testing.T) {
	cfg := Config{SyncSchemaReady: func(context.Context) error {
		t.Fatal("hosted readiness called the sync database reader")
		return nil
	}}
	response := httptest.NewRecorder()
	runtimeHealthMux(cfg).ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/sync-readyz", nil))
	if response.Code != http.StatusNotFound {
		t.Fatal("hosted runtime registered the sync readiness route")
	}
}

func TestLocalRuntimeUnselectedSyncPostgres(t *testing.T) {
	st, dsn := exportWriteDeadlineStore(t)
	ctx := context.Background()
	account, err := st.EnsureDefaultAccount(ctx)
	if err != nil {
		t.Fatal("create local fixture account failed")
	}
	db, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal("open local fixture observer failed")
	}
	defer func() { _ = db.Close(ctx) }()
	cfg := Config{LocalRuntime: true, AppliedSchemaVersion: st.AppliedSchemaVersion,
		Ready: func(ctx context.Context) error {
			return runtimeconfig.CheckReady(ctx, st, store.SchemaVersion(), account)
		},
	}
	before := localVersionResponse(t, cfg)
	beforeReady := httptest.NewRecorder()
	runtimeHealthMux(cfg).ServeHTTP(beforeReady, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if err := st.MigrateContext(ctx); err != nil {
		t.Fatal("rerun unselected core migration failed")
	}
	if err := cfg.Ready(ctx); err != nil {
		t.Fatal("unselected core readiness failed")
	}
	if version, err := st.AppliedSyncSchemaVersion(ctx); err != nil || version != 0 {
		t.Fatal("unselected runtime gained an installed sync requirement")
	}
	var tables int
	if err := db.QueryRow(ctx, `SELECT count(*) FROM pg_tables WHERE schemaname=current_schema() AND (left(tablename,5)='sync_' OR tablename='witself_sync_schema_version')`).Scan(&tables); err != nil || tables != 0 {
		t.Fatal("unselected runtime created sync tables or version state")
	}
	after := localVersionResponse(t, cfg)
	afterReady := httptest.NewRecorder()
	runtimeHealthMux(cfg).ServeHTTP(afterReady, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if before.Code != after.Code || before.Body.String() != versionResponseSnapshot(store.SchemaVersion()) || before.Body.String() != after.Body.String() || !reflect.DeepEqual(before.Header(), after.Header()) || beforeReady.Code != afterReady.Code || beforeReady.Body.String() != afterReady.Body.String() {
		t.Fatal("unselected local before/after response bytes changed")
	}
	if cfg.SyncSchemaReady != nil {
		t.Fatal("unselected local runtime gained a sync readiness field")
	}
	absent := httptest.NewRecorder()
	runtimeHealthMux(cfg).ServeHTTP(absent, httptest.NewRequest(http.MethodGet, "/sync-readyz", nil))
	if absent.Code != http.StatusNotFound {
		t.Fatal("unselected local runtime registered sync readiness")
	}
}
