package export

import (
	"encoding/json"
	"fmt"
	"time"
)

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
	36: addMessageAudienceDefaults,
	49: preserveSchema49Rows,
	50: addAvatarPayloadQuotaDefaults,
	51: preserveSchema51Rows,
	53: addAvatarRendererProfileDefault,
	54: preserveSchema54Rows,
	55: preserveSchema55Rows,
	56: preserveSchema56Rows,
	57: preserveSchema57Rows,
	58: preserveSchema58Rows,
}

const (
	legacyAvatarRetainedPayloadCountLimit = 20
	legacyAvatarRetainedPayloadByteLimit  = 2 * 1024 * 1024
	legacyAvatarMaximumPayloadBytes       = 128 * 1024
)

// preserveSchema56Rows acknowledges the agent_dashboard_preferences table
// added by migration 0057. A schema-56 archive carries no dashboard
// preference stream and none of its existing rows change shape, so every row
// passes through unchanged; the destination simply imports the new table as
// an empty stream.
func preserveSchema56Rows(_ string, row map[string]any) (map[string]any, error) {
	return row, nil
}

// preserveSchema57Rows acknowledges the transcript_coverage input kind added
// by migration 0058. The migration only widens the input-kind check and adds
// a nullable coverage_counts column, so a schema-57 archive — which can only
// contain the four original input kinds and no coverage counts — imports
// unchanged into the widened shape.
func preserveSchema57Rows(_ string, row map[string]any) (map[string]any, error) {
	return row, nil
}

// preserveSchema58Rows acknowledges the four agent-email tables introduced
// by migration 0059. A schema-58 archive has none of those streams and no
// existing row changes shape, so its rows pass through unchanged while the
// destination imports the new tables as empty.
func preserveSchema58Rows(_ string, row map[string]any) (map[string]any, error) {
	return row, nil
}

// preserveSchema54Rows acknowledges the tenant-scope uniqueness keys added
// alongside the new schema-55 sealed tables. Schema-54 archives already use
// globally unique realm and agent ids and contain no sealed streams, so their
// existing rows need no transformation.
func preserveSchema54Rows(_ string, row map[string]any) (map[string]any, error) {
	return row, nil
}

// preserveSchema55Rows lifts the only ambiguous schema-55 AVK state. Schema
// 55 permitted pending keys but had no rotation aggregate that could own or
// complete them, so migration 56 deterministically retires those orphan
// candidates at their creation timestamp. Archive upgrades must perform the
// identical normalization before the schema-56 importer validates the row.
func preserveSchema55Rows(table string, row map[string]any) (map[string]any, error) {
	if table != "agent_vault_keys" {
		return row, nil
	}

	stateValue, exists := row["lifecycle_state"]
	if !exists {
		return nil, fmt.Errorf("legacy vault key lifecycle_state is missing")
	}
	state, ok := stateValue.(string)
	if !ok {
		return nil, fmt.Errorf("legacy vault key lifecycle_state must be a string")
	}
	switch state {
	case "current", "retired":
		return row, nil
	case "pending":
		// Continue below: only pending rows need normalization.
	default:
		return nil, fmt.Errorf("legacy vault key lifecycle_state is invalid")
	}

	createdValue, exists := row["created_at"]
	if !exists {
		return nil, fmt.Errorf("legacy pending vault key created_at is missing")
	}
	createdAt, ok := createdValue.(string)
	if !ok {
		return nil, fmt.Errorf("legacy pending vault key created_at must be a string")
	}
	if _, err := time.Parse(time.RFC3339Nano, createdAt); err != nil {
		return nil, fmt.Errorf("legacy pending vault key created_at is invalid: %w", err)
	}
	retiredAt, exists := row["retired_at"]
	if !exists {
		return nil, fmt.Errorf("legacy pending vault key retired_at is missing")
	}
	if retiredAt != nil {
		return nil, fmt.Errorf("legacy pending vault key retired_at must be null")
	}
	revisionValue, exists := row["row_version"]
	if !exists {
		return nil, fmt.Errorf("legacy pending vault key row_version is missing")
	}
	revision, ok := revisionValue.(json.Number)
	if !ok {
		return nil, fmt.Errorf("legacy pending vault key row_version must be an integer")
	}
	parsedRevision, err := revision.Int64()
	if err != nil || parsedRevision < 1 {
		return nil, fmt.Errorf("legacy pending vault key row_version is invalid")
	}

	row["lifecycle_state"] = "retired"
	row["retired_at"] = createdAt
	return row, nil
}

// addAvatarRendererProfileDefault lifts every schema-53 avatar version as
// legacy. Older writers never performed the exact perceptual-v1 baseline
// checks, so an upgrader must not infer or fabricate stronger provenance from
// SVG bytes that happen to pass today's validator.
func addAvatarRendererProfileDefault(table string, row map[string]any) (map[string]any, error) {
	if table == "agent_avatar_versions" {
		row["renderer_profile"] = "legacy"
		// Pre-profile fingerprints cannot prove that their source SVG used the
		// exact perceptual-v1 renderer contract. The locked-layer digest remains
		// the portable continuity authority for quarantined compacted rows.
		row["continuity_fingerprint"] = nil
	}
	return row, nil
}

// addAvatarPayloadQuotaDefaults lifts schema-50 avatar rows into the explicit
// full-or-compacted payload representation introduced by migration 0051.
// Every schema-50 version necessarily retained its complete creative payload,
// so the upgrader can derive an exact byte count without inventing history.
func addAvatarPayloadQuotaDefaults(table string, row map[string]any) (map[string]any, error) {
	switch table {
	case "agent_avatar_profiles":
		row["retained_payload_count_limit"] = legacyAvatarRetainedPayloadCountLimit
		row["retained_payload_byte_limit"] = legacyAvatarRetainedPayloadByteLimit
		row["payload_quota_reconciliation_required"] = true
	case "agent_avatar_versions":
		svg, svgOK := row["svg"].(string)
		description, descriptionOK := row["description"].(string)
		visualSpec, specOK := row["visual_spec"]
		if !svgOK || !descriptionOK || !specOK || visualSpec == nil {
			return nil, fmt.Errorf("legacy avatar version payload is incomplete")
		}
		rawSpec, err := json.Marshal(visualSpec)
		if err != nil {
			return nil, fmt.Errorf("legacy avatar visual_spec is invalid: %w", err)
		}
		payloadBytes := len(svg) + len(description) + len(rawSpec)
		if payloadBytes < 1 || payloadBytes > legacyAvatarMaximumPayloadBytes {
			return nil, fmt.Errorf("legacy avatar payload byte count is invalid")
		}
		row["payload_state"] = "full"
		row["payload_bytes"] = payloadBytes
		row["payload_compacted_at"] = nil
		row["payload_compaction_reason"] = nil
		// Row-local archive upgrades cannot derive a locked-layer projection
		// without the referenced style pack. The account importer derives and
		// validates it after style rows have been loaded.
		row["locked_layers_sha256"] = nil
		row["continuity_fingerprint"] = nil
	}
	return row, nil
}

// preserveSchema51Rows acknowledges the one-open-avatar-style-rollout
// uniqueness invariant introduced by migration 0052. A schema-51 archive has
// no rollout-job stream, so there are no preexisting rows to reconcile.
func preserveSchema51Rows(table string, row map[string]any) (map[string]any, error) {
	if table == "agent_avatar_profiles" {
		// Existing projections predate the durable selection-revision fence.
		// NULL makes the first later rollout discover them lazily.
		row["style_revision"] = nil
	}
	return row, nil
}

// preserveSchema49Rows acknowledges the avatar aggregate, deferred foreign
// keys, and uniqueness constraints introduced by migration 0050. A schema-49
// archive has no avatar streams to transform; the account importer creates
// deterministic realm style and agent placeholder rows after its legacy
// realms and agents have landed.
func preserveSchema49Rows(_ string, row map[string]any) (map[string]any, error) {
	return row, nil
}

// addMessageAudienceDefaults lifts the one-recipient message shape written
// before migration 0037 into the explicit direct-audience representation.
// A schema-36 archive can only contain the to_agent_id-backed delivery that
// was authoritative at the time, so no audience snapshot is synthesized.
func addMessageAudienceDefaults(table string, row map[string]any) (map[string]any, error) {
	if table == "agent_messages" {
		row["audience_kind"] = "agent"
		row["audience_fingerprint"] = ""
	}
	return row, nil
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
