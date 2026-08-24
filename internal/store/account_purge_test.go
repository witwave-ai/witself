package store

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

func TestDefaultAccountPurgeWorkerConfigAndValidation(t *testing.T) {
	want := AccountPurgeWorkerConfig{
		BatchSize:    100,
		Interval:     5 * time.Minute,
		BatchTimeout: 2 * time.Minute,
		Grace:        720 * time.Hour,
		Mode:         AccountPurgeModePreview,
	}
	if got := DefaultAccountPurgeWorkerConfig(); got != want {
		t.Fatalf("defaults = %#v, want %#v", got, want)
	}
	if err := want.Validate(); err != nil {
		t.Fatalf("default config: %v", err)
	}
	want.Mode = AccountPurgeModeEnforce
	if err := want.Validate(); err != nil {
		t.Fatalf("enforce config: %v", err)
	}

	tests := []struct {
		name string
		edit func(*AccountPurgeWorkerConfig)
		want string
	}{
		{"batch low", func(c *AccountPurgeWorkerConfig) { c.BatchSize = 0 }, "batch size"},
		{"batch high", func(c *AccountPurgeWorkerConfig) { c.BatchSize = 1001 }, "batch size"},
		{"interval low", func(c *AccountPurgeWorkerConfig) { c.Interval = 59 * time.Second }, "interval"},
		{"interval high", func(c *AccountPurgeWorkerConfig) { c.Interval = 25 * time.Hour }, "interval"},
		{"timeout low", func(c *AccountPurgeWorkerConfig) { c.BatchTimeout = time.Millisecond }, "batch timeout"},
		{"timeout high", func(c *AccountPurgeWorkerConfig) { c.BatchTimeout = 6 * time.Minute }, "batch timeout"},
		{"grace low", func(c *AccountPurgeWorkerConfig) { c.Grace = 59 * time.Second }, "grace"},
		{"grace high", func(c *AccountPurgeWorkerConfig) { c.Grace = 8760*time.Hour + time.Second }, "grace"},
		{"mode", func(c *AccountPurgeWorkerConfig) { c.Mode = "delete" }, "mode"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cfg := DefaultAccountPurgeWorkerConfig()
			test.edit(&cfg)
			err := cfg.Validate()
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestAccountPurgeTableCountMergeAndClassificationShape(t *testing.T) {
	result := AccountPurgeBatchResult{
		DeletedByTable: map[string]int64{"memories": 2},
	}
	mergeAccountPurgeTableCounts(result.DeletedByTable, map[string]int64{
		"memories":       3,
		"account_events": 4,
	})
	if result.DeletedByTable["memories"] != 5 ||
		result.DeletedByTable["account_events"] != 4 {
		t.Fatalf("merged counts = %#v", result.DeletedByTable)
	}

	wantTables := []string{
		"transcript_retention_account_scan_state",
		"message_retention_account_scan_state",
		"agent_email_retention_account_scan_state",
		"message_retention_thread_activity",
		"agent_message_rate_buckets",
		"agent_email_rate_buckets",
		"agent_email_outbound_rate_buckets",
		"agent_email_account_rate_buckets",
	}
	if len(cellLocalAccountPurgeTables) != len(wantTables) {
		t.Fatalf("cell-local purge tables = %#v", cellLocalAccountPurgeTables)
	}
	for index, wantTable := range wantTables {
		if cellLocalAccountPurgeTables[index] != wantTable {
			t.Fatalf(
				"cell-local purge table %d = %q, want %q",
				index,
				cellLocalAccountPurgeTables[index],
				wantTable,
			)
		}
	}
}

func TestIsAccountPurgeAttachmentUnderflow(t *testing.T) {
	underflow := fmt.Errorf(
		"delete trigger: %w",
		&pgconn.PgError{Message: accountPurgeAttachmentUnderflow},
	)
	if !isAccountPurgeAttachmentUnderflow(underflow) {
		t.Fatal("wrapped attachment counter underflow was not classified")
	}
	if isAccountPurgeAttachmentUnderflow(
		&pgconn.PgError{Message: "different invariant"},
	) {
		t.Fatal("unrelated PostgreSQL error was classified as underflow")
	}
	if isAccountPurgeAttachmentUnderflow(fmt.Errorf("%s", accountPurgeAttachmentUnderflow)) {
		t.Fatal("plain error text was classified as PostgreSQL underflow")
	}
}
