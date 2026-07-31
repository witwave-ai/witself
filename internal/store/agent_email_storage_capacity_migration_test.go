package store

import (
	"strings"
	"testing"
)

func TestAgentEmailStorageCapacitySchema80MigrationContract(t *testing.T) {
	raw, err := migrationsFS.ReadFile(
		"migrations/0080_backfill_agent_email_storage_capacity.sql",
	)
	if err != nil {
		t.Fatal(err)
	}
	migration := string(raw)
	const orderedFences = "LOCK TABLE accounts IN EXCLUSIVE MODE NOWAIT;\n" +
		"LOCK TABLE agent_email_messages IN SHARE MODE NOWAIT;"
	if !strings.Contains(migration, orderedFences) {
		t.Fatalf(
			"migration 80 lacks the exact account-first NOWAIT fences:\n%s",
			migration,
		)
	}
	if !strings.Contains(
		migration,
		"attachment_storage_accounted = true",
	) {
		t.Fatal("migration 80 does not promote every legacy row into the counter")
	}
}

func TestAgentEmailStorageCapacitySchema81MigrationContract(t *testing.T) {
	raw, err := migrationsFS.ReadFile(
		"migrations/0081_reconcile_agent_email_storage_capacity.sql",
	)
	if err != nil {
		t.Fatal(err)
	}
	migration := string(raw)
	const orderedFences = "LOCK TABLE accounts IN EXCLUSIVE MODE NOWAIT;\n" +
		"LOCK TABLE agent_email_messages IN SHARE MODE NOWAIT;"
	if !strings.Contains(migration, orderedFences) {
		t.Fatalf(
			"migration 81 lacks the exact account-first NOWAIT fences:\n%s",
			migration,
		)
	}
	if strings.Contains(
		migration,
		"LOCK TABLE accounts IN SHARE ROW EXCLUSIVE MODE",
	) {
		t.Fatal(
			"migration 81 account fence does not conflict with supported " +
				"SELECT ... FOR NO KEY UPDATE writers",
		)
	}
	if strings.Count(
		migration,
		"AND message.attachment_storage_accounted",
	) != 2 {
		t.Fatal("migration 81 reconciliation includes unadmitted old-writer rows")
	}
}

func TestAgentEmailStorageCapacitySchema82MigrationContract(t *testing.T) {
	raw, err := migrationsFS.ReadFile(
		"migrations/0082_finalize_agent_email_storage_capacity.sql",
	)
	if err != nil {
		t.Fatal(err)
	}
	migration := string(raw)
	const orderedFences = "LOCK TABLE accounts IN EXCLUSIVE MODE NOWAIT;\n" +
		"LOCK TABLE agent_email_messages IN SHARE MODE NOWAIT;"
	if !strings.Contains(migration, orderedFences) {
		t.Fatalf(
			"migration 82 lacks the exact account-first NOWAIT fences:\n%s",
			migration,
		)
	}
	for _, required := range []string{
		"WITH accounted_counts AS",
		"AND message.attachment_storage_accounted",
		"SET attachment_storage_accounted = true",
		"WHERE NOT attachment_storage_accounted",
		"WITH desired_counts AS",
		"legacy rows remain",
		"unaccounted rows remain",
		"account counters mismatch",
	} {
		if !strings.Contains(migration, required) {
			t.Errorf("migration 82 lacks %q", required)
		}
	}
	if got := strings.Count(
		migration,
		"AND message.attachment_storage_accounted",
	); got != 1 {
		t.Errorf(
			"migration 82 accounted-only predicates = %d, want exactly one baseline predicate",
			got,
		)
	}
	if got := strings.Count(
		migration,
		"LEFT JOIN agent_email_messages message",
	); got != 2 {
		t.Errorf(
			"migration 82 counter rebuilds = %d, want baseline and final rebuild",
			got,
		)
	}

	parts := strings.Split(migration, "-- +goose Down")
	if len(parts) != 2 {
		t.Fatalf("migration 82 Down split = %d parts", len(parts))
	}
	down := strings.ToUpper(parts[1])
	if strings.Contains(down, "UPDATE ") ||
		strings.Contains(down, "DROP ") ||
		!strings.Contains(parts[1], "SELECT 1;") {
		t.Fatalf("migration 82 Down is not a data-only no-op:\n%s", parts[1])
	}
}

func TestAgentEmailStorageCapacitySchema79OldWriterContract(t *testing.T) {
	raw, err := migrationsFS.ReadFile(
		"migrations/0079_add_agent_email_storage_capacity.sql",
	)
	if err != nil {
		t.Fatal(err)
	}
	migration := string(raw)
	for _, required := range []string{
		"attachment_storage_accounted BOOLEAN NOT NULL DEFAULT false",
		"IF NEW.attachment_storage_accounted THEN",
		"IF OLD.attachment_storage_accounted THEN",
		"retained_attachment_storage_bytes, attachment_storage_accounted, account_id",
	} {
		if !strings.Contains(migration, required) {
			t.Fatalf("migration 79 lacks old-writer compatibility %q", required)
		}
	}
}
