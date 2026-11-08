package main

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/witwave-ai/witself/internal/localcell"
	"github.com/witwave-ai/witself/internal/runtimeconfig"
	"github.com/witwave-ai/witself/internal/store"
	"github.com/witwave-ai/witself/internal/worker"
)

func TestManagedPrincipalsCommandSelection(t *testing.T) {
	for _, selector := range []string{"WITSELF_LOCAL_MANAGED_PRINCIPALS", "WITSELF_LOCAL_MIGRATION_PHASE"} {
		called := 0
		var output bytes.Buffer
		lookup := func(name string) (string, bool) { return "1", name == selector }
		code := managedWorkerReceiptReplay(nil, lookup, &output, func([]string) int { called++; return 0 })
		if code != 1 || called != 0 {
			t.Fatal("managed worker selector allowed operational command")
		}
	}
}

type managedWorkerFixture struct {
	core, sync, migrations int
	identity               error
	topology               error
	role                   store.SyncPrincipalRole
	reads                  int
	readyAfter             int
}

func (s *managedWorkerFixture) AppliedSchemaVersion(context.Context) (int, error) {
	s.reads++
	if s.readyAfter > 0 && s.reads >= s.readyAfter {
		return store.SchemaVersion(), nil
	}
	return s.core, nil
}
func (s *managedWorkerFixture) AppliedSyncSchemaVersion(context.Context) (int, error) {
	return s.sync, nil
}
func (s *managedWorkerFixture) VerifyManagedRuntimePrincipal(_ context.Context, role store.SyncPrincipalRole) error {
	s.role = role
	return s.topology
}
func (s *managedWorkerFixture) VerifyManagedRuntimeIdentity(_ context.Context, role store.SyncPrincipalRole) error {
	s.role = role
	return s.identity
}
func (s *managedWorkerFixture) Migrate() error                    { s.migrations++; return nil }
func (s *managedWorkerFixture) MigrateSync(context.Context) error { s.migrations++; return nil }
func (s *managedWorkerFixture) Ping(context.Context) error        { return nil }
func (s *managedWorkerFixture) LocalBootstrapAccount(context.Context) (string, error) {
	return "managed_fixture_account", nil
}

func TestManagedPrincipalsStartup(t *testing.T) {
	for _, wrong := range []string{"", "core", "sync", "identity", "identity_before_versions", "topology"} {
		t.Run("worker_"+wrong, func(t *testing.T) {
			st := &managedWorkerFixture{core: store.SchemaVersion(), sync: store.SyncSchemaVersion()}
			switch wrong {
			case "core":
				st.core--
			case "sync":
				st.sync++
			case "identity":
				st.identity = store.ErrSyncPrincipalTopologyUnverified
			case "identity_before_versions":
				st.identity, st.core, st.sync = store.ErrSyncPrincipalTopologyUnverified, 0, 0
			case "topology":
				st.topology = store.ErrSyncPrincipalTopologyUnverified
			}
			cfg := runtimeconfig.Config{Local: true, SyncSchema: true, ManagedPrincipals: true, MigrationWait: time.Millisecond}
			err := workerMigrationStartup(context.Background(), st, cfg)
			if st.migrations != 0 {
				t.Fatal("managed worker invoked migration with runtime login")
			}
			if (err == nil) != (wrong == "") {
				t.Fatal("managed worker accepted refused startup")
			}
			if (wrong == "identity" || wrong == "identity_before_versions" || wrong == "topology") && err != localcell.ErrLocalRuntimeLoginOwnsTables {
				t.Fatal("managed worker lost fixed identity refusal")
			}
			if wrong == "identity_before_versions" && st.reads != 0 {
				t.Fatal("managed worker waited for versions before refusing identity")
			}
			if (wrong == "core" || wrong == "sync") && err != localcell.ErrLocalMigrationPhaseRequired {
				t.Fatal("managed worker lost fixed migration refusal")
			}
			registrations := 0
			code := serveWorker(context.Background(), nil, st, cfg, jobConfig{}, mapLookup(nil), func(*worker.Registry, context.Context, worker.Config) error { registrations++; return nil })
			if (code == 0 && registrations == 1) != (wrong == "") {
				t.Fatal("managed worker registered jobs before required preflight")
			}
			if wrong == "" && st.role != store.SyncPrincipalWorker {
				t.Fatal("managed worker checked incorrect restricted identity")
			}
		})
	}
	t.Run("bounded_wait", func(t *testing.T) {
		st := &managedWorkerFixture{core: store.SchemaVersion() - 1, sync: store.SyncSchemaVersion(), readyAfter: 2}
		cfg := runtimeconfig.Config{Local: true, SyncSchema: true, ManagedPrincipals: true, MigrationWait: time.Second}
		if workerMigrationStartup(context.Background(), st, cfg) != nil || st.reads < 2 || st.migrations != 0 {
			t.Fatal("managed worker did not retain read-only bounded migration wait")
		}
	})
}
