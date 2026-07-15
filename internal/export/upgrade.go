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
	28: preserveSchema28Rows,
	29: addMemoryCurationDefaults,
	30: addTokenAccessProfileDefault,
	32: preserveSchema32Rows,
	33: addMessageProcessingDefaults,
	34: addMessageCausalDepthDefault,
	35: addMessageFailureCountDefault,
}

// addMessageFailureCountDefault supplies the backend-owned deterministic
// failure counter introduced by migration 0036. Earlier archives have no
// durable poison-attempt history, so zero is the only faithful default.
func addMessageFailureCountDefault(table string, row map[string]any) (map[string]any, error) {
	if table == "agent_message_deliveries" {
		row["failure_count"] = 0
	}
	return row, nil
}

// addMessageCausalDepthDefault supplies the destination column for archives
// written before migration 0035. The account importer recalculates reply
// depths from the validated parent graph after all rows have landed.
func addMessageCausalDepthDefault(table string, row map[string]any) (map[string]any, error) {
	if table == "agent_messages" {
		row["causal_depth"] = 1
	}
	return row, nil
}

// addMessageProcessingDefaults lifts schema-33 delivery rows into the
// unclaimed processing state introduced by migration 0034. Older archives
// cannot contain a live claim or linked completion result.
func addMessageProcessingDefaults(table string, row map[string]any) (map[string]any, error) {
	if table != "agent_message_deliveries" {
		return row, nil
	}
	row["processing_state"] = "available"
	row["processing_generation"] = 0
	row["claim_id"] = nil
	row["claim_key_hash"] = ""
	row["lease_expires_at"] = nil
	row["completed_at"] = nil
	row["complete_key_hash"] = ""
	row["result_message_id"] = nil
	return row, nil
}

// preserveSchema32Rows acknowledges the nullable reply-causality column and
// scoped parent foreign key added by migration 0033. Schema-32 archives cannot
// contain replies, so their existing message rows remain valid with a NULL
// reply_to_message_id on import.
func preserveSchema32Rows(_ string, row map[string]any) (map[string]any, error) {
	return row, nil
}

// addTokenAccessProfileDefault preserves the pre-schema-31 authority of every
// archived credential. Curator profiles are only created explicitly after the
// migration; an older archive can never acquire restricted or elevated
// semantics by inference during import.
func addTokenAccessProfileDefault(table string, row map[string]any) (map[string]any, error) {
	if table == "tokens" {
		row["access_profile"] = "full"
	}
	return row, nil
}

// addMemoryCurationDefaults lifts schema-29 rows through the columns added to
// existing tables by migration 0030. New curation tables are absent from an
// old archive; the destination therefore imports them as empty streams.
func addMemoryCurationDefaults(table string, row map[string]any) (map[string]any, error) {
	switch table {
	case "fact_candidates":
		row["curation_run_id"] = nil
		row["curation_action_id"] = nil
		row["withdrawal_reason"] = ""
		row["withdrawal_idempotency_key"] = ""
		row["withdrawal_request_hash"] = ""
	case "memory_relations":
		row["reverted_by_action_id"] = nil
	case "memories":
		row["deleted_curation_run_count"] = 0
		row["deleted_curation_action_count"] = 0
		row["deleted_curation_input_count"] = 0
		row["deleted_curation_mutation_count"] = 0
	}
	return row, nil
}

// preserveSchema28Rows is the archive-discipline acknowledgement for the
// narrative-memory schema introduced by migration 0029. That migration adds
// new tables and foreign keys but does not change the shape of any row an
// older archive can contain. Archives written at schema 28 therefore pass
// through unchanged; the destination simply imports no rows for the new
// tables.
func preserveSchema28Rows(_ string, row map[string]any) (map[string]any, error) {
	return row, nil
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
