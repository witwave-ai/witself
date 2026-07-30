package store

import (
	"strings"
	"testing"
)

func TestActiveFactCountSchema78MigrationContract(t *testing.T) {
	raw, err := migrationsFS.ReadFile("migrations/0078_reconcile_active_fact_count.sql")
	if err != nil {
		t.Fatal(err)
	}
	migration := string(raw)
	for _, want := range []string{
		"LOCK TABLE agents IN EXCLUSIVE MODE NOWAIT;\nLOCK TABLE facts IN SHARE MODE NOWAIT;",
		"LEFT JOIN facts fact",
		"fact.deleted_at IS NULL",
		"fact.resolved_assertion_id IS NOT NULL",
		"owner.active_fact_count IS DISTINCT FROM desired.active_fact_count",
		"active fact reconciliation failed",
	} {
		if !strings.Contains(migration, want) {
			t.Errorf("migration 78 lacks %q", want)
		}
	}
	parts := strings.Split(migration, "-- +goose Down")
	if len(parts) != 2 {
		t.Fatalf("migration 78 Down split = %d parts", len(parts))
	}
	if strings.Contains(strings.ToUpper(parts[1]), "UPDATE ") ||
		strings.Contains(strings.ToUpper(parts[1]), "DROP ") ||
		!strings.Contains(parts[1], "SELECT 1;") {
		t.Fatalf("migration 78 Down is not a data-only no-op:\n%s", parts[1])
	}
}
