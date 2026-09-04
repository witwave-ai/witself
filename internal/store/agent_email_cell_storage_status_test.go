package store

import (
	"context"
	"strings"
	"testing"

	"github.com/witwave-ai/witself/internal/testenv"
)

func TestValidateAgentEmailCellStorageStatus(t *testing.T) {
	t.Parallel()
	valid := AgentEmailCellStorageStatus{
		RetainedBytes: 10, RootRows: 1, CountedRows: 2,
		AdmissionBytes: 100, AdmissionRootRows: 10,
		HardBytes: 200, HardCountedRows: 20,
	}
	if err := validateAgentEmailCellStorageStatus(valid); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name   string
		mutate func(*AgentEmailCellStorageStatus)
		want   string
	}{
		{name: "negative bytes", mutate: func(s *AgentEmailCellStorageStatus) { s.RetainedBytes = -1 }, want: "usage counters"},
		{name: "roots exceed rows", mutate: func(s *AgentEmailCellStorageStatus) { s.RootRows = 3 }, want: "usage counters"},
		{name: "invalid limits", mutate: func(s *AgentEmailCellStorageStatus) { s.AdmissionBytes = s.HardBytes }, want: "invalid limits"},
	} {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			candidate := valid
			test.mutate(&candidate)
			err := validateAgentEmailCellStorageStatus(candidate)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("validation = %v, want %q", err, test.want)
			}
		})
	}
}

func TestReadAgentEmailCellStorageStatusPostgres(t *testing.T) {
	baseDSN := testenv.RequirePostgres(t)
	st, dsn := newMigrationTestStore(t, baseDSN)
	migrationTestUpTo(t, dsn, 91)
	status, err := st.ReadAgentEmailCellStorageStatus(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if status.RetainedBytes != 0 || status.RootRows != 0 || status.CountedRows != 0 {
		t.Fatalf("empty status = %+v", status)
	}
	if status.AdmissionBytes != 3*1024*1024*1024 ||
		status.AdmissionRootRows != 25000 ||
		status.HardBytes != 4*1024*1024*1024 ||
		status.HardCountedRows != 100000 {
		t.Fatalf("default status = %+v", status)
	}
}
