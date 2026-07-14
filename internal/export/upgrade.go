package export

// The upgrader chain is the schema-evolution contract for archives: an
// archive written at schema version N restores into a cell at schema M > N
// by lifting each row through the registered upgraders N -> N+1 -> ... -> M.
//
// Discipline (enforceable in CI): any migration that CHANGES DATA SHAPE must
// register its upgrader in the same commit. Additive migrations (new nullable
// columns) need no entry — absent means no-op, and destination defaults
// apply. Upgraders are NEVER deleted: archives sleep in cold storage for
// years, and the chain back to the oldest supported schema stays alive as
// long as any archive might exist.
//
// A destination OLDER than the archive (M < N) refuses loudly at import —
// downgraders are written when a real need appears, not speculatively.
//
// The registry lives here from day one (empty) so the seam exists before the
// heavy tables — memories, conversations, audit — arrive.

// Upgrader lifts one row of one table from schema version v to v+1. Returning
// nil drops the row (for tables/columns a migration removed).
type Upgrader func(table string, row map[string]any) (map[string]any, error)

// upgraders maps a schema version to the function lifting rows to the NEXT
// version. Absent = no shape change at that migration = no-op.
var upgraders = map[int]Upgrader{
	25: addFactIdempotencyDefaults,
	26: addFactDeletionDefaults,
	27: preserveSchema27Rows,
}

// preserveSchema27Rows is an explicit archive-discipline acknowledgement for
// migration 0028. That migration removes a redundant full-address UNIQUE
// constraint after the schema-27 writer-compatibility rollout; it changes what
// relational states Postgres accepts but does not add, remove, or transform an
// archive field. Registering the identity step keeps that non-additive schema
// decision visible to the enforced upgrader chain.
func preserveSchema27Rows(_ string, row map[string]any) (map[string]any, error) {
	return row, nil
}

// addFactIdempotencyDefaults lifts schema-25 archives into the constrained
// schema-26 shape. Empty keys opt out and therefore cannot collide with the
// partial unique indexes added by migration 0026.
func addFactIdempotencyDefaults(table string, row map[string]any) (map[string]any, error) {
	switch table {
	case "fact_assertions":
		row["idempotency_key"] = ""
		row["idempotency_fingerprint"] = ""
	case "fact_candidates":
		row["idempotency_key"] = ""
		row["idempotency_fingerprint"] = ""
		row["decision_idempotency_key"] = ""
		row["decision_assertion_id"] = nil
	}
	return row, nil
}

// addFactDeletionDefaults lifts active schema-26 facts into schema 27. Older
// archives cannot contain deletion tombstones, so every imported fact receives
// the value-free active defaults and continues to require its resolved
// assertion. The new retry-tombstone table is absent, which is equivalent to
// an empty table for a pre-deletion archive.
func addFactDeletionDefaults(table string, row map[string]any) (map[string]any, error) {
	if table != "facts" {
		return row, nil
	}
	row["deleted_at"] = nil
	row["deleted_by_agent_id"] = nil
	row["delete_receipt_id"] = ""
	row["delete_idempotency_key_hash"] = ""
	row["deleted_prior_assertion_id"] = ""
	row["deleted_assertion_count"] = 0
	row["deleted_candidate_count"] = 0
	row["deleted_usage_count"] = 0
	row["deleted_mutation_key_count"] = 0
	row["deleted_candidate_revision"] = ""
	row["recreated_at"] = nil
	row["replacement_fact_id"] = nil
	return row, nil
}

// UpgraderFor returns the upgrader lifting rows from schema version v to
// v+1, or nil when that migration changed no data shape.
func UpgraderFor(v int) Upgrader {
	return upgraders[v]
}
