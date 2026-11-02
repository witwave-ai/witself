package runtimeconfig

import (
	"context"
	"errors"
	"testing"
)

type syncCaptureStatusFixture struct {
	status SyncStatus
	err    error
	reads  int
}

func (s *syncCaptureStatusFixture) SyncCaptureStatus(context.Context, string) (SyncStatus, error) {
	s.reads++
	return s.status, s.err
}

func TestSyncStatusRequiresBothExplicitSelectors(t *testing.T) {
	for _, cfg := range []Config{{}, {Local: true}, {SyncSchema: true}} {
		st := &syncCaptureStatusFixture{status: SyncBootstrapping}
		status, err := ReadSyncStatus(context.Background(), cfg, st, "fixture-account")
		if err != nil || status != SyncDisabled || st.reads != 0 || cfg.SyncCaptureSelected() {
			t.Fatal("sync status: unselected configuration accessed capture state")
		}
	}
}

func TestSyncStatusVocabularyAndFailures(t *testing.T) {
	cfg := Config{Local: true, SyncSchema: true}
	for _, tc := range []struct {
		name   string
		st     SyncStatusStore
		want   SyncStatus
		failed bool
	}{
		{name: "disabled", st: &syncCaptureStatusFixture{status: SyncDisabled}, want: SyncDisabled},
		{name: "bootstrapping", st: &syncCaptureStatusFixture{status: SyncBootstrapping}, want: SyncBootstrapping},
		{name: "suspended", st: &syncCaptureStatusFixture{status: SyncSuspended}, want: SyncSuspended},
		{name: "missing store", want: SyncSuspended, failed: true},
		{name: "missing status", st: &syncCaptureStatusFixture{}, want: SyncSuspended, failed: true},
		{name: "ready forbidden", st: &syncCaptureStatusFixture{status: "ready"}, want: SyncSuspended, failed: true},
		{name: "unknown status", st: &syncCaptureStatusFixture{status: "private-status"}, want: SyncSuspended, failed: true},
		{name: "database failure", st: &syncCaptureStatusFixture{err: errors.New("private-database-error")}, want: SyncSuspended, failed: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			status, err := ReadSyncStatus(context.Background(), cfg, tc.st, "fixture-account")
			if status != tc.want || (err != nil) != tc.failed || (err != nil && err != ErrSyncStatusUnavailable) {
				t.Fatal("sync status: capture status failed closed incorrectly")
			}
			core := readinessStore{version: 7, account: "fixture-account"}
			if CheckReady(context.Background(), core, 7, "fixture-account") != nil || CheckSyncSchemaReady(context.Background(), syncSchemaReadinessStore{version: 2}, 2) != nil {
				t.Fatal("sync status: capture result changed independent readiness")
			}
		})
	}
	st := &syncCaptureStatusFixture{status: SyncDisabled}
	if status, err := ReadSyncStatus(context.Background(), cfg, st, ""); status != SyncSuspended || err != ErrSyncStatusUnavailable || st.reads != 0 {
		t.Fatal("sync status: absent account reached durable state reader")
	}
}
