package runtimeconfig

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestLocalSyncSchemaSelection(t *testing.T) {
	for _, role := range []string{"server", "worker"} {
		t.Run(role, func(t *testing.T) {
			values := localValues(t)
			baseline, err := FromEnv(role, testLookup(values), nil)
			if err != nil || !baseline.Local || baseline.SyncSchema {
				t.Fatal("ordinary local runtime did not leave sync off")
			}
			values["WITSELF_LOCAL_SYNC_SCHEMA"] = "1"
			selected, err := FromEnv(role, testLookup(values), nil)
			if err != nil || !selected.SyncSchema {
				t.Fatal("explicit local sync schema selection failed")
			}
			selected.SyncSchema = false
			if !reflect.DeepEqual(selected, baseline) {
				t.Fatal("sync selection changed another local setting or job eligibility")
			}
			for _, value := range []string{"", "0", "true", " 1 ", "private-sync-value"} {
				values["WITSELF_LOCAL_SYNC_SCHEMA"] = value
				_, err := FromEnv(role, testLookup(values), nil)
				if err == nil || !strings.Contains(err.Error(), "WITSELF_LOCAL_SYNC_SCHEMA") || strings.Contains(err.Error(), "private-sync-value") {
					t.Fatal("invalid sync selection was not refused value-free")
				}
			}
		})
	}
}

func TestLocalSyncSchemaForbiddenOutsideLocal(t *testing.T) {
	for _, role := range []string{"server", "worker"} {
		for _, selector := range []string{"unset", "", "hosted", "LOCAL"} {
			for _, name := range []string{"WITSELF_LOCAL_SYNC_SCHEMA", "witself_local_sync_schema"} {
				for _, value := range []string{"", "0", "1", "private-sync-value"} {
					t.Run(role+"/"+selector+"/"+name+"/"+value, func(t *testing.T) {
						values := map[string]string{name: value}
						if selector != "unset" {
							values["WITSELF_RUNTIME"] = selector
						}
						_, err := FromEnv(role, testLookup(values), []string{name + "=" + value})
						if err == nil || !strings.Contains(err.Error(), name) || !strings.Contains(err.Error(), "forbidden") || strings.Contains(err.Error(), "private-sync-value") {
							t.Fatal("non-local sync variable presence was not refused value-free")
						}
					})
				}
			}
		}
	}
}

type syncSchemaReadinessStore struct {
	version int
	err     error
}

func (s syncSchemaReadinessStore) AppliedSyncSchemaVersion(context.Context) (int, error) {
	return s.version, s.err
}

func TestLocalSyncSchemaReadinessSeparateFromCore(t *testing.T) {
	core := readinessStore{version: 7, account: "fixture-account"}
	for _, tc := range []struct {
		name  string
		st    SyncSchemaStore
		ready bool
	}{
		{name: "missing reader"},
		{name: "absent", st: syncSchemaReadinessStore{}},
		{name: "older", st: syncSchemaReadinessStore{version: 1}},
		{name: "current", st: syncSchemaReadinessStore{version: 2}, ready: true},
		{name: "ahead", st: syncSchemaReadinessStore{version: 3}},
		{name: "query failure", st: syncSchemaReadinessStore{version: 2, err: errors.New("private-database-error")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := CheckSyncSchemaReady(context.Background(), tc.st, 2)
			if (err == nil) != tc.ready || (err != nil && err.Error() != "local sync schema is not ready") {
				t.Fatal("sync schema readiness did not fail closed with fixed diagnostics")
			}
			if err := CheckReady(context.Background(), core, 7, "fixture-account"); err != nil {
				t.Fatal("sync status changed core readiness")
			}
			if err := CheckReady(context.Background(), core, 8, "fixture-account"); err == nil || err.Error() != "local database schema is not ready" {
				t.Fatal("core readiness result or error changed")
			}
		})
	}
	if err := CheckSyncSchemaReady(context.Background(), syncSchemaReadinessStore{}, 0); err == nil {
		t.Fatal("uncompiled namespace became ready")
	}
}
