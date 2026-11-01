package server

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/witwave-ai/witself/internal/runtimeconfig"
	"github.com/witwave-ai/witself/internal/store"
	"github.com/witwave-ai/witself/internal/testenv"
)

// The clock trigger is only a deterministic fixture. The real production
// handler and store upsert execute in all three phases; no response field is
// removed or normalized before comparing exact bytes.
func TestSyncCapturePreferencesHTTPDefaultBytesPostgres(t *testing.T) {
	dsn := testenv.RequirePostgres(t)
	if testenv.ValidateHarnessPostgresDSN(dsn) != nil {
		t.Fatal("sync HTTP fixture database selection refused")
	}
	fixture := testenv.NewPostgresSchema(t, dsn)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	st, err := store.Open(ctx, fixture.DSN())
	if err != nil {
		t.Fatal("sync HTTP fixture store construction failed")
	}
	t.Cleanup(st.Close)
	if st.MigrateContext(ctx) != nil {
		t.Fatal("sync HTTP fixture core migration failed")
	}
	observer, err := pgx.Connect(ctx, fixture.DSN())
	if err != nil {
		t.Fatal("sync HTTP fixture observer connection failed")
	}
	t.Cleanup(func() { _ = observer.Close(context.Background()) })
	for _, sql := range []string{
		`INSERT INTO accounts(id,display_name,is_default,status) VALUES('acc_1','HTTP fixture',false,'active')`,
		`INSERT INTO realms(id,account_id,name) VALUES('realm_1','acc_1','default')`,
		`INSERT INTO agents(id,realm_id,name) VALUES('agent_1','realm_1','fixture')`,
		`CREATE FUNCTION fixture_preferences_clock() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN NEW.updated_at='2026-01-01T00:00:00Z'::timestamptz; RETURN NEW; END; $$`,
		`CREATE TRIGGER fixture_preferences_clock BEFORE INSERT OR UPDATE ON agent_dashboard_preferences
		FOR EACH ROW EXECUTE FUNCTION fixture_preferences_clock()`,
	} {
		if _, err := observer.Exec(ctx, sql); err != nil {
			t.Fatal("sync HTTP fixture setup statement failed")
		}
	}
	handler := apiMux(Config{
		AuthenticatePrincipal: secretTestAuth,
		PutDashboardPreferences: func(ctx context.Context, p DomainPrincipal, in PutDashboardPreferencesRequest) (DashboardPreferences, error) {
			out, err := st.PutDashboardPreferences(ctx, store.Principal{Kind: store.PrincipalAgent, ID: p.ID,
				AccountID: p.AccountID, RealmID: p.RealmID, AccountStatus: p.AccountStatus, AccessProfile: p.AccessProfile}, in.Prefs)
			return DashboardPreferences{AgentID: out.AgentID, Prefs: out.Prefs, UpdatedAt: out.UpdatedAt}, err
		},
	})
	var baseline *httptest.ResponseRecorder
	var baselineDomain []byte
	for _, phase := range []string{"core_only", "sync_installed_unselected", "sync_selected_inactive"} {
		t.Run(phase, func(t *testing.T) {
			if phase == "sync_installed_unselected" {
				if st.MigrateSync(ctx) != nil {
					t.Fatal("sync HTTP selected schema migration failed")
				}
			}
			if phase == "sync_selected_inactive" {
				if st.SelectSyncCapture(runtimeconfig.Config{Local: true, SyncSchema: true}) != nil {
					t.Fatal("sync HTTP explicit selection failed")
				}
			}
			r := httptest.NewRequestWithContext(ctx, http.MethodPut, "/v1/self/dashboard-preferences",
				strings.NewReader(`{"prefs":{"schema":"witself.dashboard-prefs.v1","theme":"amber"}}`))
			r.Header.Set("Authorization", "Bearer agent-token")
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, r)
			if response.Code != http.StatusOK {
				t.Fatalf("sync HTTP fixed fixture returned unexpected status: %d", response.Code)
			}
			var domain []byte
			if observer.QueryRow(ctx, `SELECT row_to_json(p)::text FROM agent_dashboard_preferences p WHERE agent_id='agent_1'`).Scan(&domain) != nil {
				t.Fatal("sync HTTP fixed domain projection failed")
			}
			if baseline == nil {
				baseline, baselineDomain = response, domain
			} else if baseline.Code != response.Code || !bytes.Equal(baseline.Body.Bytes(), response.Body.Bytes()) ||
				!reflect.DeepEqual(baseline.Header(), response.Header()) || !bytes.Equal(baselineDomain, domain) {
				t.Fatal("sync HTTP fixed status body headers or domain bytes changed")
			}
			for _, table := range []string{"account_events", "usage_events", "usage_rollups"} {
				var count int
				if observer.QueryRow(ctx, `SELECT count(*) FROM `+pgx.Identifier{table}.Sanitize()+` WHERE account_id='acc_1'`).Scan(&count) != nil || count != 0 {
					t.Fatalf("sync HTTP preference write added audit or usage rows: %s", table)
				}
			}
			if phase != "core_only" {
				rows, err := observer.Query(ctx, `SELECT tablename FROM pg_tables WHERE schemaname=current_schema() AND left(tablename,5)='sync_' ORDER BY tablename`)
				if err != nil {
					t.Fatal("sync HTTP sync inventory query failed")
				}
				names, err := pgx.CollectRows(rows, pgx.RowTo[string])
				if err != nil || len(names) == 0 {
					t.Fatal("sync HTTP installed inventory unavailable")
				}
				for _, table := range names {
					var count int
					if observer.QueryRow(ctx, `SELECT count(*) FROM `+pgx.Identifier{table}.Sanitize()).Scan(&count) != nil || count != 0 {
						t.Fatalf("sync HTTP inactive write created sync rows: %s", table)
					}
				}
			}
		})
	}
}
