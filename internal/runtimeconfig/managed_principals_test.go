package runtimeconfig

import (
	"reflect"
	"testing"
)

func TestManagedPrincipalsRuntimeSelection(t *testing.T) {
	for _, role := range []string{"server", "worker"} {
		t.Run(role, func(t *testing.T) {
			values := localValues(t)
			base, err := FromEnv(role, testLookup(values), nil)
			if err != nil || base.ManagedPrincipals || base.MigrationPhase {
				t.Fatal("managed selection changed legacy runtime")
			}
			values[managedPrincipalsVariable] = "1"
			values["WITSELF_LOCAL_SYNC_SCHEMA"] = "1"
			cfg, err := FromEnv(role, testLookup(values), nil)
			if err != nil || !cfg.ManagedPrincipals || cfg.MigrationPhase || !cfg.SyncSchema {
				t.Fatal("managed selection rejected supported runtime")
			}
			for _, raw := range []string{"", "0", "true", " 1", "1 "} {
				values[managedPrincipalsVariable] = raw
				if _, err := FromEnv(role, testLookup(values), nil); err == nil {
					t.Fatal("managed selection accepted malformed selector")
				}
			}
			values[managedPrincipalsVariable] = "1"
			for _, key := range []string{"WITSELF_RUNTIME", "WITSELF_LOCAL_SYNC_SCHEMA"} {
				saved := values[key]
				delete(values, key)
				if _, err := FromEnv(role, testLookup(values), nil); err == nil {
					t.Fatal("managed selection accepted missing prerequisite")
				}
				values[key] = saved
			}
			for _, alias := range []string{"witself_local_managed_principals", "WITSELF_LOCAL_MANAGED_PRINCIPALS_EXTRA", "witself_local_migration_phase"} {
				if _, err := FromEnv(role, testLookup(values), []string{alias + "=1"}); err == nil {
					t.Fatal("managed selection accepted selector alias")
				}
			}
			values["WITSELF_PROVISION_TOKEN"] = "fixture-private-refusal"
			if _, err := FromEnv(role, testLookup(values), nil); err == nil {
				t.Fatal("managed selection accepted forbidden ambient setting")
			}
		})
	}
	values := localValues(t)
	values[managedPrincipalsVariable] = "1"
	values[migrationPhaseVariable] = "1"
	values["WITSELF_LOCAL_SYNC_SCHEMA"] = "1"
	values["WITSELF_DATABASE_URL"] = "postgres://witself_sync_migrator:fixture-migrator-password@127.0.0.1:1234/witself?sslmode=disable&options=-c+role%3Dwitself_sync_owner"
	if cfg, err := FromEnv("server", testLookup(values), nil); err != nil || !cfg.MigrationPhase {
		t.Fatal("managed selection rejected bounded migration phase")
	}
	if _, err := FromEnv("worker", testLookup(values), nil); err == nil {
		t.Fatal("managed selection accepted worker migration phase")
	}
	for _, key := range []string{"WITSELF_LOCAL_SHUTDOWN_DESCRIPTOR", "WITSELF_LOCAL_SHUTDOWN_TRANSPORT", captureProofPrefix, captureProofPrefix + "_TOKEN"} {
		values[key] = "fixture-private-refusal"
		if _, err := FromEnv("server", testLookup(values), nil); err == nil {
			t.Fatal("managed selection accepted migration profile conflict")
		}
		delete(values, key)
	}
	values[migrationPhaseVariable] = "true"
	if _, err := FromEnv("server", testLookup(values), nil); err == nil {
		t.Fatal("managed selection accepted malformed migration selector")
	}
	if cfg, err := FromEnv("server", testLookup(nil), nil); err != nil || !reflect.DeepEqual(cfg, Config{}) {
		t.Fatal("managed selection changed hosted zero snapshot")
	}
}

func TestManagedPrincipalsMigrationDSNCustody(t *testing.T) {
	for _, dsn := range []string{
		"postgres://witself_sync_owner@127.0.0.1/witself?options=-c+role%3Dwitself_sync_owner",
		"postgres://witself_sync_migrator@127.0.0.1/witself",
		"postgres://witself_sync_migrator@127.0.0.1/witself?options=-c+role%3Dwitself_sync_owner&options=unsafe",
		"postgres://witself_sync_migrator@127.0.0.1/witself?options=-c+role%3Dwitself_sync_owner&role=unsafe",
		"postgres://witself_sync_migrator@127.0.0.1/witself?options=-c+role%3Dwitself_sync_owner&user=unsafe",
	} {
		if ManagedMigrationDSN(dsn) {
			t.Fatal("managed migration DSN accepted lost owner custody")
		}
	}
}
