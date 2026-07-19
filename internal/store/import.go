package store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/witwave-ai/witself/internal/export"
	"github.com/witwave-ai/witself/internal/placement"
)

// ErrAccountExists is returned when an import targets an account id already
// present on this cell — restore never overwrites.
var ErrAccountExists = errors.New("account already exists on this cell")

// ErrArchiveAccountMismatch is returned when the archive's manifest names a
// different account than the caller asked to import.
var ErrArchiveAccountMismatch = errors.New("archive is for a different account")

// ErrArchiveContent is returned for archives that are structurally well-formed
// (gzip/tar/checksums all check out) but whose row payload violates the
// import contract: a row scoped to a different account, an off-list column,
// an agent pointing at a realm that never arrived, extra accounts rows.
// The condition is permanent for a given archive object, so it maps to a
// 400 (server.ErrBadArchive) — the caller should quarantine the archive,
// not retry it.
var ErrArchiveContent = errors.New("archive content is not importable")

// A small allowance prevents harmless cell clock skew from invalidating a
// portable archive, while still rejecting a manifest that claims to come from
// a materially future state. Rows remain bounded by the manifest's exact
// exported_at value, not this allowance.
const maxArchiveManifestFutureSkew = 5 * time.Minute

func validateArchiveExportedAt(exportedAt, importedAt time.Time) error {
	if exportedAt.IsZero() {
		return fmt.Errorf("%w: manifest exported_at is required", ErrArchiveContent)
	}
	if exportedAt.After(importedAt.Add(maxArchiveManifestFutureSkew)) {
		return fmt.Errorf("%w: manifest exported_at is materially in the future", ErrArchiveContent)
	}
	return nil
}

// importColumns is the strict per-table allowlist of column names an archive
// may carry. It doubles as the SQL-identifier boundary — the row's JSON keys
// are looked up here and only allowlisted names are interpolated into the
// INSERT — and as the additive-migration safety net: unlisted (new) columns
// are refused rather than smuggled in with attacker-chosen values.
var importColumns = map[string]map[string]bool{
	"accounts": {
		"id": true, "is_default": true, "display_name": true, "email": true,
		"status": true, "created_at": true, "closed_at": true, "closed_reason": true,
		"suspended_at": true, "suspended_for": true, "suspended_reason": true,
		"support_policy": true,
		"plan":           true, "plan_limits": true, "plan_features": true,
		"placement_policy": true,
	},
	"operators": {
		"id": true, "account_id": true, "role": true, "is_root": true,
		"display_name": true, "created_at": true, "updated_at": true, "deleted_at": true,
	},
	"realms": {
		"id": true, "account_id": true, "name": true,
		"created_at": true, "updated_at": true, "deleted_at": true,
	},
	"avatar_style_packs": {
		"account_id": true, "realm_id": true, "id": true,
		"current_version": true, "revision": true,
		"created_at": true, "updated_at": true,
	},
	"avatar_style_pack_versions": {
		"account_id": true, "realm_id": true, "style_pack_id": true,
		"version": true, "previous_version": true, "name": true,
		"description": true, "style_spec": true,
		"reference_examples": true, "provenance": true,
		"created_by_kind": true, "created_by_id": true, "created_at": true,
	},
	"realm_avatar_styles": {
		"account_id": true, "realm_id": true, "style_pack_id": true,
		"style_pack_version": true, "revision": true,
		"created_at": true, "updated_at": true,
	},
	"avatar_style_rollout_jobs": {
		"account_id": true, "realm_id": true, "style_revision": true,
		"style_pack_id": true, "style_pack_version": true, "status": true,
		"target_profile_count": true, "processed_profile_count": true,
		"batch_count": true, "last_batch_size": true,
		"failure_count": true, "retry_after": true, "last_failure_code": true,
		"created_at": true, "started_at": true, "updated_at": true,
		"completed_at": true, "superseded_at": true,
	},
	"agents": {
		"id": true, "realm_id": true, "name": true,
		"created_at": true, "updated_at": true, "deleted_at": true,
	},
	"agent_vault_keys": {
		"id": true, "account_id": true, "realm_id": true,
		"owner_agent_id": true, "key_version": true, "algorithm": true,
		"fingerprint": true, "lifecycle_state": true, "row_version": true,
		"created_at": true, "retired_at": true,
	},
	"secrets": {
		"id": true, "account_id": true, "realm_id": true,
		"owner_agent_id": true, "name": true, "description": true,
		"template": true, "tags": true, "row_version": true,
		"created_at": true, "updated_at": true,
		"archived_at": true, "deleted_at": true,
	},
	"secret_fields": {
		"id": true, "account_id": true, "realm_id": true,
		"owner_agent_id": true, "secret_id": true, "name": true,
		"field_kind": true, "sensitive": true,
		"value_encoding": true, "value_version": true,
		"public_value": true, "envelope_version": true, "ciphertext": true,
		"aead_algorithm": true, "aad_version": true,
		"dek_id": true, "dek_generation": true, "row_version": true,
		"created_at": true, "updated_at": true,
	},
	"secret_deks": {
		"id": true, "account_id": true, "realm_id": true,
		"owner_agent_id": true, "secret_id": true, "field_id": true,
		"dek_generation": true, "wrapped_dek": true,
		"wrap_algorithm": true, "aad_version": true,
		"wrap_revision": true, "wrapping_key_id": true,
		"wrapping_key_version": true, "row_version": true,
		"created_at": true, "retired_at": true,
	},
	"secret_mutation_receipts": {
		"account_id": true, "realm_id": true, "owner_agent_id": true,
		"actor_kind": true, "actor_id": true, "operation": true,
		"idempotency_key_hash": true, "request_hash": true,
		"target_kind": true, "target_id": true,
		"result_revision": true, "result_value_version": true,
		"created_at": true,
	},
	"agent_avatar_profiles": {
		"account_id": true, "realm_id": true, "agent_id": true,
		"status": true, "lineage_generation": true, "autonomy_policy": true,
		"style_pack_id": true, "style_pack_version": true, "style_revision": true,
		"latest_avatar_version": true, "proposed_avatar_version": true,
		"active_avatar_version": true, "subject_form": true,
		"attempt_count": true, "retry_after": true,
		"fallback_seed": true, "failure_code": true, "revision": true,
		"retained_payload_count_limit":          true,
		"retained_payload_byte_limit":           true,
		"payload_quota_reconciliation_required": true,
		"created_at":                            true, "updated_at": true,
	},
	"agent_avatar_versions": {
		"account_id": true, "realm_id": true, "agent_id": true,
		"id": true, "version": true, "lineage_generation": true,
		"parent_version": true,
		"style_pack_id":  true, "style_pack_version": true,
		"subject_form": true, "svg": true, "description": true,
		"visual_spec": true, "svg_sha256": true,
		"locked_layers_sha256": true, "renderer_profile": true,
		"continuity_fingerprint": true,
		"provenance":             true,
		"payload_state":          true, "payload_bytes": true,
		"payload_compacted_at": true, "payload_compaction_reason": true,
		"proposed_by_kind": true, "proposed_by_id": true,
		"proposed_at": true,
	},
	"agent_avatar_activations": {
		"id": true, "account_id": true, "realm_id": true,
		"agent_id": true, "sequence": true,
		"lineage_generation": true, "avatar_version": true,
		"prior_active_version": true, "action": true,
		"activated_by_kind": true, "activated_by_id": true,
		"activated_at": true,
	},
	"agent_avatar_rejections": {
		"id": true, "account_id": true, "realm_id": true,
		"agent_id": true, "avatar_version": true, "reason_code": true,
		"rejected_by_kind": true, "rejected_by_id": true,
		"rejected_at": true,
	},
	"agent_avatar_resets": {
		"id": true, "account_id": true, "realm_id": true,
		"agent_id": true, "sequence": true,
		"retired_lineage_generation": true,
		"new_lineage_generation":     true,
		"retired_active_version":     true,
		"retired_proposed_version":   true,
		"reason_code":                true, "reset_by_kind": true,
		"reset_by_id": true, "reset_at": true,
	},
	"avatar_mutation_receipts": {
		"account_id": true, "realm_id": true,
		"target_kind": true, "target_id": true,
		"actor_kind": true, "actor_id": true, "operation": true,
		"idempotency_key": true, "request_hash": true,
		"result_revision": true, "result_version": true,
		"result_lineage_generation": true,
		"created_at":                true,
	},
	"agent_activity": {
		"agent_id": true, "runtime": true, "location_id": true, "location": true,
		"last_event": true, "last_event_id": true,
		"last_event_occurred_at": true, "last_activity_at": true,
	},
	"tokens": {
		"id": true, "account_id": true, "operator_id": true, "agent_id": true,
		"kind": true, "token_hash": true, "display_name": true,
		"access_profile": true, "created_at": true, "expires_at": true, "consumed_at": true,
	},
	"account_events": {
		"id": true, "account_id": true, "occurred_at": true,
		"actor_kind": true, "actor_id": true,
		"verb": true, "metadata": true, "retain_until": true,
	},
	"support_tickets": {
		"id": true, "account_id": true, "opened_at": true,
		"opened_by_kind": true, "opened_by_id": true,
		"subject": true, "category": true, "state": true,
		"priority": true, "first_response_at": true, "resolved_at": true,
		"closed_at": true, "last_activity_at": true, "last_message_id": true,
		"correlation": true, "metadata": true, "retain_until": true,
	},
	"support_ticket_messages": {
		"id": true, "ticket_id": true, "account_id": true, "posted_at": true,
		"author_kind": true, "author_id": true,
		"body": true, "attachments": true, "metadata": true,
	},
	"transcript_conversations": {
		"id": true, "account_id": true, "realm_id": true,
		"owner_agent_id": true, "external_id": true, "title": true,
		"metadata": true, "next_sequence": true,
		"created_at": true, "updated_at": true,
	},
	"transcript_entries": {
		"id": true, "account_id": true, "transcript_id": true,
		"realm_id": true, "recorded_by_agent_id": true,
		"sequence": true, "external_id": true, "role": true, "body": true,
		"payload": true, "model": true, "reply_to_entry_id": true,
		"artifacts": true, "created_at": true,
	},
	"usage_events": {
		"id": true, "account_id": true, "realm_id": true, "agent_id": true,
		"dimension": true, "quantity": true, "unit": true,
		"subject_type": true, "subject_id": true, "idempotency_key": true,
		"metadata": true, "occurred_at": true, "created_at": true,
	},
	"usage_rollups": {
		"account_id": true, "realm_id": true, "agent_id": true,
		"dimension": true, "unit": true, "bucket": true,
		"bucket_start": true, "quantity": true, "event_count": true,
		"updated_at": true,
	},
	"fact_subjects": {
		"id": true, "account_id": true, "realm_id": true,
		"owner_agent_id": true, "canonical_key": true,
		"display_name": true, "aliases": true,
		"created_at": true, "updated_at": true,
	},
	"facts": {
		"id": true, "account_id": true, "realm_id": true,
		"owner_agent_id": true, "subject_id": true, "predicate": true,
		"cardinality": true, "sensitive": true, "resolved_assertion_id": true,
		"deleted_at": true, "deleted_by_agent_id": true,
		"delete_receipt_id": true, "delete_idempotency_key_hash": true,
		"deleted_prior_assertion_id": true,
		"deleted_assertion_count":    true, "deleted_candidate_count": true,
		"deleted_usage_count": true, "deleted_mutation_key_count": true,
		"deleted_candidate_revision": true,
		"recreated_at":               true, "replacement_fact_id": true,
		"created_at": true, "updated_at": true,
	},
	"fact_mutation_tombstones": {
		"id": true, "account_id": true, "realm_id": true,
		"owner_agent_id": true, "fact_id": true, "surface": true,
		"idempotency_key_hash": true, "deleted_at": true,
	},
	"fact_assertions": {
		"id": true, "fact_id": true, "account_id": true, "realm_id": true,
		"asserted_by_agent_id": true, "value_type": true, "value": true,
		"recurrence":  true,
		"source_kind": true, "source_ref": true, "confidence": true,
		"observed_at": true, "confirmed_at": true, "valid_from": true,
		"valid_until": true, "supersedes_id": true,
		"idempotency_key": true, "idempotency_fingerprint": true,
		"created_at": true,
	},
	"fact_candidates": {
		"id": true, "account_id": true, "realm_id": true,
		"owner_agent_id": true, "subject_key": true, "predicate": true,
		"value_type": true, "value": true, "cardinality": true,
		"recurrence": true,
		"sensitive":  true, "source_ref": true, "confidence": true,
		"observed_at": true, "valid_from": true, "valid_until": true,
		"reason": true, "status": true, "conflict_fact_id": true,
		"observed_assertion_id": true,
		"resolved_fact_id":      true, "decision_assertion_id": true,
		"idempotency_key":         true,
		"idempotency_fingerprint": true, "decision_idempotency_key": true,
		"curation_run_id": true, "curation_action_id": true,
		"withdrawal_reason": true, "withdrawal_idempotency_key": true,
		"withdrawal_request_hash": true,
		"proposed_at":             true, "decided_at": true,
	},
	"agent_messages": {
		"id": true, "account_id": true, "realm_id": true,
		"from_agent_id": true, "to_agent_id": true,
		"audience_kind": true, "audience_fingerprint": true,
		"subject": true, "kind": true, "body": true, "payload": true,
		"thread_id": true, "reply_to_message_id": true, "causal_depth": true,
		"idempotency_key": true, "created_at": true,
	},
	"agent_message_deliveries": {
		"message_id": true, "account_id": true, "realm_id": true,
		"recipient_agent_id": true, "state": true, "delivered_at": true,
		"read_at": true, "acked_at": true, "created_at": true,
		"processing_state": true, "processing_generation": true,
		"failure_count": true,
		"claim_id":      true, "claim_key_hash": true,
		"lease_expires_at": true, "completed_at": true,
		"complete_key_hash": true, "result_message_id": true,
	},
	"agent_message_requests": {
		"id": true, "account_id": true, "realm_id": true,
		"opening_message_id": true, "coordinator_agent_id": true,
		"selection_policy": true, "state": true, "max_assignees": true,
		"offer_window_seconds": true, "expires_in_seconds": true,
		"offer_deadline": true, "expires_at": true,
		"selection_generation": true,
		"completed_at":         true, "cancelled_at": true, "expired_at": true,
		"created_at": true, "updated_at": true,
	},
	"agent_message_request_candidates": {
		"request_id": true, "account_id": true, "realm_id": true,
		"agent_id": true, "response_state": true,
		"offer_message_id": true, "offer_key_hash": true,
		"offer_request_hash": true, "responded_at": true, "created_at": true,
	},
	"agent_message_request_selections": {
		"id": true, "request_id": true, "account_id": true, "realm_id": true,
		"coordinator_agent_id": true, "generation": true,
		"idempotency_key_hash": true, "selection_hash": true, "created_at": true,
	},
	"agent_message_request_claims": {
		"id": true, "request_id": true, "selection_id": true,
		"account_id": true, "realm_id": true, "agent_id": true,
		"state": true, "generation": true, "claim_key_hash": true,
		"lease_expires_at": true, "failure_count": true,
		"complete_key_hash": true, "result_message_id": true,
		"selected_at": true, "claimed_at": true, "released_at": true,
		"completed_at": true, "cancelled_at": true, "updated_at": true,
	},
	"memory_change_clocks": {
		"account_id": true, "realm_id": true, "owner_kind": true,
		"owner_id": true, "last_change_seq": true,
		"created_at": true, "updated_at": true,
	},
	"memories": {
		"id": true, "account_id": true, "realm_id": true,
		"owner_kind": true, "owner_id": true, "origin": true,
		"capture_reason": true, "authored_by_agent_id": true,
		"current_version": true, "permanently_deleted_at": true,
		"permanently_deleted_by_id": true, "permanent_delete_reason": true,
		"delete_receipt_id": true, "delete_idempotency_key_hash": true,
		"deleted_prior_version": true, "deleted_scrub_set_revision": true,
		"deleted_version_count": true, "deleted_evidence_count": true,
		"deleted_relation_count": true, "deleted_retry_shield_count": true,
		"deleted_retry_shield_digest":     true,
		"deleted_curation_run_count":      true,
		"deleted_curation_action_count":   true,
		"deleted_curation_input_count":    true,
		"deleted_curation_mutation_count": true,
		"created_at":                      true, "updated_at": true,
	},
	"memory_versions": {
		"memory_id": true, "version": true, "account_id": true,
		"realm_id": true, "owner_kind": true, "owner_id": true,
		"previous_version": true, "change_seq": true, "content": true,
		"content_encoding": true, "kind": true, "tags": true,
		"links": true, "salience": true, "sensitive": true,
		"occurred_from": true, "occurred_until": true, "state": true,
		"prior_state": true, "lifecycle_reason": true, "content_hash": true,
		"actor_kind": true, "actor_id": true, "operation": true,
		"idempotency_key": true, "request_hash": true,
		"client_runtime": true, "client_model": true,
		"client_recipe": true, "client_recipe_version": true,
		"supersession_set_id": true, "supersession_set_revision": true,
		"supersession_replacement_count":  true,
		"supersession_replacement_digest": true,
		"curation_run_id":                 true, "curation_action_id": true,
		"created_at": true,
	},
	"memory_vector_profiles": {
		"id": true, "account_id": true, "realm_id": true,
		"owner_kind": true, "owner_id": true, "provider": true,
		"model": true, "recipe": true, "recipe_version": true,
		"dimensions": true, "distance_metric": true,
		"normalization": true, "contract_hash": true,
		"created_by_agent_id": true, "created_at": true,
	},
	"memory_vectors": {
		"profile_id": true, "memory_id": true, "memory_version": true,
		"account_id": true, "realm_id": true, "owner_kind": true,
		"owner_id": true, "content_hash": true, "vector": true,
		"vector_hash": true, "created_by_agent_id": true,
		"created_at": true,
	},
	"memory_evidence": {
		"id": true, "account_id": true, "realm_id": true,
		"owner_kind": true, "owner_id": true, "memory_id": true,
		"target_version": true, "evidence_change_seq": true,
		"evidence_type": true, "role": true, "resolution_state": true,
		"external_locator": true, "pending_evidence_id": true,
		"resolved_kind": true, "source_transcript_id": true,
		"source_sequence_from": true, "source_sequence_until": true,
		"source_memory_id": true, "source_memory_version": true,
		"source_message_id": true, "source_import_locator": true,
		"artifact_excerpt": true, "artifact_sensitive": true,
		"terminal_reason_code": true, "source_digest": true,
		"actor_id": true, "idempotency_key": true, "request_hash": true,
		"created_at": true,
	},
	"memory_relations": {
		"id": true, "account_id": true, "realm_id": true,
		"owner_kind": true, "owner_id": true,
		"from_memory_id": true, "from_version": true,
		"to_memory_id": true, "to_version": true, "relation_type": true,
		"supersession_set_id": true, "supersession_set_revision": true,
		"curation_run_id": true, "curation_action_id": true,
		"reverted_by_run_id": true, "reverted_by_action_id": true,
		"reverted_at": true, "created_at": true,
	},
	"memory_deleted_references": {
		"id": true, "account_id": true, "realm_id": true,
		"owner_kind": true, "owner_id": true, "deleted_memory_id": true,
		"former_reference_kind": true, "related_resource_id": true,
		"curation_run_id": true, "curation_request_id": true,
		"reason_code": true, "created_at": true,
	},
	"memory_curation_lanes": {
		"account_id": true, "realm_id": true, "owner_kind": true,
		"owner_id": true, "request_generation": true,
		"fencing_generation": true, "active_run_id": true,
		"created_at": true, "updated_at": true,
	},
	"memory_curation_cursors": {
		"account_id": true, "realm_id": true, "owner_kind": true,
		"owner_id": true, "source_kind": true, "source_stream_id": true,
		"position": true, "created_at": true, "updated_at": true,
	},
	"memory_curation_requests": {
		"id": true, "account_id": true, "realm_id": true,
		"owner_kind": true, "owner_id": true, "scope": true,
		"coalescing_key": true, "trigger_reason": true,
		"request_generation": true, "priority": true, "due_at": true,
		"state": true, "attempt_count": true, "max_attempts": true,
		"claimed_run_id": true, "fulfilled_generation": true,
		"replay_run_id": true, "read_only_replay": true,
		"actor_kind": true, "actor_id": true,
		"idempotency_key": true, "request_hash": true,
		"claimed_at": true, "fulfilled_at": true, "cancelled_at": true,
		"dead_lettered_at": true, "created_at": true, "updated_at": true,
	},
	"memory_curation_runs": {
		"id": true, "account_id": true, "realm_id": true,
		"owner_kind": true, "owner_id": true, "request_id": true,
		"request_generation": true, "fencing_generation": true,
		"state": true, "actor_kind": true, "actor_id": true,
		"idempotency_key": true, "request_hash": true,
		"lease_expires_at": true, "client_runtime": true,
		"client_model": true, "client_recipe": true,
		"client_recipe_version": true, "memory_change_upper": true,
		"evidence_change_upper": true, "input_count": true,
		"memory_input_count": true, "evidence_input_count": true,
		"transcript_input_count": true, "cursor_input_count": true,
		"plan_schema": true, "plan_revision": true, "plan_hash": true,
		"apply_receipt_id": true, "rollback_receipt_id": true,
		"conflict_reason_code": true, "terminal_reason_code": true,
		"budgets": true, "scrubbed_at": true, "scrubbed_reason_code": true,
		"started_at": true, "planned_at": true, "applied_at": true,
		"rolled_back_at": true, "terminal_at": true,
		"created_at": true, "updated_at": true,
	},
	"memory_curation_run_inputs": {
		"run_id": true, "ordinal": true, "account_id": true,
		"realm_id": true, "owner_kind": true, "owner_id": true,
		"input_kind": true, "order_key": true,
		"memory_id": true, "memory_version": true, "evidence_id": true,
		"transcript_id": true, "sequence_from": true, "sequence_until": true,
		"cursor_source_kind": true, "cursor_stream_id": true,
		"cursor_expected_prior": true, "cursor_upper": true,
		"created_at": true,
	},
	"memory_curation_actions": {
		"id": true, "run_id": true, "account_id": true,
		"realm_id": true, "owner_kind": true, "owner_id": true,
		"ordinal": true, "plan_revision": true, "primitive": true,
		"state": true, "local_ref": true, "input_refs": true,
		"expected_heads": true, "proposed_payload": true,
		"validation_result": true, "applied_result": true,
		"rollback_result": true, "action_hash": true,
		"scrubbed_at": true, "scrubbed_reason_code": true,
		"created_at": true, "validated_at": true,
		"applied_at": true, "reverted_at": true,
	},
	"memory_curation_mutations": {
		"id": true, "account_id": true, "realm_id": true,
		"owner_kind": true, "owner_id": true, "actor_kind": true,
		"actor_id": true, "operation": true, "idempotency_key": true,
		"request_hash": true, "request_id": true, "run_id": true,
		"request_generation": true, "fencing_generation": true,
		"plan_revision": true, "plan_hash": true, "lease_expires_at": true,
		"result_state": true, "receipt_id": true, "created_at": true,
	},
}

type transcriptImportScope struct {
	realmID      string
	ownerAgentID string
	nextSequence int64
}

type messageImportScope struct {
	realmID             string
	fromAgentID         string
	toAgentID           string
	audienceKind        string
	audienceFingerprint string
	recipientAgentIDs   map[string]bool
	kind                string
	threadID            string
	replyToMessageID    string
	causalDepth         int64
	createdAt           time.Time
}

type messageDeliveryImportScope struct {
	messageID            string
	recipientAgentID     string
	processingState      string
	processingGeneration int64
	failureCount         int64
	claimID              string
	resultMessageID      string
}

type messageRequestImportScope struct {
	realmID             string
	openingMessageID    string
	coordinatorAgentID  string
	state               string
	maxAssignees        int64
	selectionGeneration int64
}

type messageRequestCandidateImportKey struct {
	requestID string
	agentID   string
}

type messageRequestCandidateImportScope struct {
	responseState  string
	offerMessageID string
}

type messageRequestSelectionImportScope struct {
	requestID          string
	realmID            string
	coordinatorAgentID string
	generation         int64
}

type messageRequestClaimImportScope struct {
	requestID       string
	selectionID     string
	realmID         string
	agentID         string
	state           string
	generation      int64
	resultMessageID string
}

type factImportScope struct {
	realmID                 string
	ownerAgentID            string
	resolvedAssertionID     string
	subjectID               string
	subjectKey              string
	predicate               string
	sensitive               bool
	deleted                 bool
	replacementFactID       string
	deletedMutationKeyCount int64
}

type memoryOwnerImportKey struct {
	realmID   string
	ownerKind string
	ownerID   string
}

type memoryImportScope struct {
	owner                    memoryOwnerImportKey
	currentVersion           int64
	deleted                  bool
	deletedRetryShieldCount  int64
	deletedRetryShieldDigest string
	versionCount             int64
	maxVersion               int64
	maxChangeSeq             int64
}

type memoryVersionImportKey struct {
	memoryID string
	version  int64
}

type memoryVersionImportScope struct {
	owner                         memoryOwnerImportKey
	previousVersion               int64
	changeSeq                     int64
	payloadDigest                 string
	requestHash                   string
	state                         string
	priorState                    string
	operation                     string
	supersessionSet               memorySupersessionImportSet
	supersessionReplacementCount  int64
	supersessionReplacementDigest string
	supersessionRequestHash       string
	expectsSupersessionRelations  bool
	supersessionMembers           []MemoryVersionReference
	activeSupersessionMemberCount int64
	curationRunID                 string
	curationActionID              string
	contentHash                   string
	createdAt                     time.Time
}

type memoryVectorImportKey struct {
	profileID string
	memoryID  string
	version   int64
}

type memoryVectorProfileImportScope struct {
	owner   memoryOwnerImportKey
	profile MemoryVectorProfile
}

type memoryEvidenceImportScope struct {
	target    memoryVersionImportKey
	state     string
	changeSeq int64
}

type memorySupersessionImportSet struct {
	id       string
	revision int64
}

type memoryCurationLaneImportScope struct {
	owner             memoryOwnerImportKey
	requestGeneration int64
	fencingGeneration int64
	activeRunID       string
	normalizedActive  bool
	validated         bool
}

type memoryCurationCursorImportKey struct {
	owner      memoryOwnerImportKey
	sourceKind string
	streamID   string
}

type memoryCurationRequestImportScope struct {
	owner             memoryOwnerImportKey
	requestGeneration int64
	state             string
	claimedRunID      string
	replayRunID       string
	normalizedClaimed bool
	validated         bool
}

type memoryCurationRunImportScope struct {
	owner                 memoryOwnerImportKey
	requestID             string
	requestGeneration     int64
	fencingGeneration     int64
	state                 string
	planSchema            string
	planRevision          int64
	planHash              string
	scrubbed              bool
	inputCount            int64
	memoryInputCount      int64
	evidenceInputCount    int64
	transcriptInputCount  int64
	cursorInputCount      int64
	observedInputs        int64
	observedByKind        map[string]int64
	normalizedInterrupted bool
	observedActions       int64
	validated             bool
}

type memoryCurationActionImportScope struct {
	id             string
	owner          memoryOwnerImportKey
	runID          string
	ordinal        int64
	planRevision   int64
	state          string
	primitive      string
	scrubbed       bool
	action         MemoryCurationPlanAction
	inputRefs      []MemoryCurationActionInputRef
	expectedHeads  []MemoryCurationExpectedHead
	actionHash     string
	appliedResult  *MemoryCurationActionApplyResult
	rollbackResult *MemoryCurationActionRollbackResult
}

type factCandidateCurationImportScope struct {
	owner    memoryOwnerImportKey
	runID    string
	actionID string
	status   string
}

type memoryRelationCurationImportScope struct {
	id                 string
	owner              memoryOwnerImportKey
	relationType       string
	from               MemoryVersionReference
	to                 MemoryVersionReference
	runID              string
	actionID           string
	revertedByRunID    string
	revertedByActionID string
}

type agentActivityImportKey struct {
	agentID    string
	runtime    string
	locationID string
}

// importCtx accumulates per-import state: how many accounts rows we have seen
// (must be exactly one), and the set of ids inserted for each table. The FK
// targets an incoming row references (agents.realm_id, tokens.operator_id,
// tokens.agent_id) must have already been inserted by THIS import — the FK
// constraint alone would accept a target belonging to any tenant on the cell.
type importCtx struct {
	accountID                    string
	accountStatus                string
	schemaVersion                int
	exportedAt                   time.Time
	importedAt                   time.Time
	accounts                     int
	operators                    map[string]bool
	realms                       map[string]bool
	agents                       map[string]bool
	liveAgents                   map[string]bool
	agentRealms                  map[string]string
	agentActivity                map[agentActivityImportKey]bool
	vaultKeys                    map[string]secretVaultKeyImportScope
	vaultKeyVersions             map[secretVaultKeyVersionImportKey]string
	vaultCurrentKeys             map[string]int
	vaultFingerprints            map[string]bool
	secrets                      map[string]secretImportScope
	secretNames                  map[string]map[string]string
	secretFields                 map[string]secretFieldImportScope
	secretFieldNames             map[string]map[string]bool
	secretFieldCounts            map[string]int
	secretMaxValueVersions       map[string]int64
	secretDEKs                   map[string]secretDEKImportScope
	secretDEKGenerations         map[secretDEKGenerationImportKey]bool
	secretCurrentDEKs            map[string]int
	secretReceipts               map[string]bool
	avatarStyleHeads             map[avatarStyleHeadImportKey]avatarStyleHeadImportScope
	avatarStyleVersions          map[avatarStyleVersionImportKey]avatarStyleVersionImportScope
	realmAvatarStyles            map[string]realmAvatarStyleImportScope
	avatarStyleRollouts          map[avatarStyleRolloutImportKey]avatarStyleRolloutImportScope
	avatarStyleOpenRollouts      map[string]int
	avatarProfiles               map[string]avatarProfileImportScope
	avatarVersions               map[avatarVersionImportKey]avatarVersionImportScope
	avatarVersionIDs             map[string]bool
	avatarLedgerIDs              map[string]bool
	avatarActivationHeads        map[avatarLineageImportKey]int64
	avatarActivationCount        map[avatarLineageImportKey]int64
	avatarActivationSequence     map[string]int64
	avatarActivatedVersions      map[avatarVersionImportKey]bool
	avatarLastActivatedAt        map[avatarVersionImportKey]time.Time
	avatarRejectedVersions       map[avatarVersionImportKey]bool
	avatarRejectedAt             map[avatarVersionImportKey]time.Time
	avatarResets                 map[avatarLineageImportKey]avatarResetImportScope
	avatarResetReceipts          map[avatarLineageImportKey]bool
	avatarResetCount             map[string]int64
	avatarLineageEarliestAt      map[avatarLineageImportKey]time.Time
	avatarLineageLatestAt        map[avatarLineageImportKey]time.Time
	avatarLastResetAt            map[string]time.Time
	tickets                      map[string]bool
	transcripts                  map[string]transcriptImportScope
	entries                      map[string]string
	factSubjects                 map[string]factImportScope
	factSubjectNames             map[string]map[string]string
	facts                        map[string]factImportScope
	assertions                   map[string]string
	factMutationTombstoneCounts  map[string]int64
	messages                     map[string]messageImportScope
	deliveries                   map[string]messageDeliveryImportScope
	messageRequests              map[string]messageRequestImportScope
	messageRequestCandidates     map[messageRequestCandidateImportKey]messageRequestCandidateImportScope
	messageRequestSelections     map[string]messageRequestSelectionImportScope
	messageRequestClaims         map[string]messageRequestClaimImportScope
	memoryClocks                 map[memoryOwnerImportKey]int64
	memories                     map[string]memoryImportScope
	memoryVersions               map[memoryVersionImportKey]memoryVersionImportScope
	memoryVectorProfiles         map[string]memoryVectorProfileImportScope
	memoryVectors                map[memoryVectorImportKey]bool
	memoryEvidence               map[string]memoryEvidenceImportScope
	memoryEvidenceTerminal       map[string]bool
	memoryActiveSupersessionSets map[memoryVersionImportKey]memorySupersessionImportSet
	memoryDeletedRetryShields    map[string][]memoryDeleteRetryShield
	memoryCurationLanes          map[memoryOwnerImportKey]memoryCurationLaneImportScope
	memoryCurationCursors        map[memoryCurationCursorImportKey]int64
	memoryCurationRequests       map[string]memoryCurationRequestImportScope
	memoryCurationRuns           map[string]memoryCurationRunImportScope
	memoryCurationActions        map[string]memoryCurationActionImportScope
	memoryCurationMutations      map[string]bool
	factCandidateCurations       map[string]factCandidateCurationImportScope
	memoryRelationCurations      []memoryRelationCurationImportScope
	memoryRelations              map[string]memoryRelationCurationImportScope
}

func newImportCtx(accountID string) *importCtx {
	return &importCtx{
		accountID:                    accountID,
		schemaVersion:                SchemaVersion(),
		operators:                    map[string]bool{},
		realms:                       map[string]bool{},
		agents:                       map[string]bool{},
		liveAgents:                   map[string]bool{},
		agentRealms:                  map[string]string{},
		agentActivity:                map[agentActivityImportKey]bool{},
		vaultKeys:                    map[string]secretVaultKeyImportScope{},
		vaultKeyVersions:             map[secretVaultKeyVersionImportKey]string{},
		vaultCurrentKeys:             map[string]int{},
		vaultFingerprints:            map[string]bool{},
		secrets:                      map[string]secretImportScope{},
		secretNames:                  map[string]map[string]string{},
		secretFields:                 map[string]secretFieldImportScope{},
		secretFieldNames:             map[string]map[string]bool{},
		secretFieldCounts:            map[string]int{},
		secretMaxValueVersions:       map[string]int64{},
		secretDEKs:                   map[string]secretDEKImportScope{},
		secretDEKGenerations:         map[secretDEKGenerationImportKey]bool{},
		secretCurrentDEKs:            map[string]int{},
		secretReceipts:               map[string]bool{},
		avatarStyleHeads:             map[avatarStyleHeadImportKey]avatarStyleHeadImportScope{},
		avatarStyleVersions:          map[avatarStyleVersionImportKey]avatarStyleVersionImportScope{},
		realmAvatarStyles:            map[string]realmAvatarStyleImportScope{},
		avatarStyleRollouts:          map[avatarStyleRolloutImportKey]avatarStyleRolloutImportScope{},
		avatarStyleOpenRollouts:      map[string]int{},
		avatarProfiles:               map[string]avatarProfileImportScope{},
		avatarVersions:               map[avatarVersionImportKey]avatarVersionImportScope{},
		avatarVersionIDs:             map[string]bool{},
		avatarLedgerIDs:              map[string]bool{},
		avatarActivationHeads:        map[avatarLineageImportKey]int64{},
		avatarActivationCount:        map[avatarLineageImportKey]int64{},
		avatarActivationSequence:     map[string]int64{},
		avatarActivatedVersions:      map[avatarVersionImportKey]bool{},
		avatarLastActivatedAt:        map[avatarVersionImportKey]time.Time{},
		avatarRejectedVersions:       map[avatarVersionImportKey]bool{},
		avatarRejectedAt:             map[avatarVersionImportKey]time.Time{},
		avatarResets:                 map[avatarLineageImportKey]avatarResetImportScope{},
		avatarResetReceipts:          map[avatarLineageImportKey]bool{},
		avatarResetCount:             map[string]int64{},
		avatarLineageEarliestAt:      map[avatarLineageImportKey]time.Time{},
		avatarLineageLatestAt:        map[avatarLineageImportKey]time.Time{},
		avatarLastResetAt:            map[string]time.Time{},
		tickets:                      map[string]bool{},
		transcripts:                  map[string]transcriptImportScope{},
		entries:                      map[string]string{},
		factSubjects:                 map[string]factImportScope{},
		factSubjectNames:             map[string]map[string]string{},
		facts:                        map[string]factImportScope{},
		assertions:                   map[string]string{},
		factMutationTombstoneCounts:  map[string]int64{},
		messages:                     map[string]messageImportScope{},
		deliveries:                   map[string]messageDeliveryImportScope{},
		messageRequests:              map[string]messageRequestImportScope{},
		messageRequestCandidates:     map[messageRequestCandidateImportKey]messageRequestCandidateImportScope{},
		messageRequestSelections:     map[string]messageRequestSelectionImportScope{},
		messageRequestClaims:         map[string]messageRequestClaimImportScope{},
		memoryClocks:                 map[memoryOwnerImportKey]int64{},
		memories:                     map[string]memoryImportScope{},
		memoryVersions:               map[memoryVersionImportKey]memoryVersionImportScope{},
		memoryVectorProfiles:         map[string]memoryVectorProfileImportScope{},
		memoryVectors:                map[memoryVectorImportKey]bool{},
		memoryEvidence:               map[string]memoryEvidenceImportScope{},
		memoryEvidenceTerminal:       map[string]bool{},
		memoryActiveSupersessionSets: map[memoryVersionImportKey]memorySupersessionImportSet{},
		memoryDeletedRetryShields:    map[string][]memoryDeleteRetryShield{},
		memoryCurationLanes:          map[memoryOwnerImportKey]memoryCurationLaneImportScope{},
		memoryCurationCursors:        map[memoryCurationCursorImportKey]int64{},
		memoryCurationRequests:       map[string]memoryCurationRequestImportScope{},
		memoryCurationRuns:           map[string]memoryCurationRunImportScope{},
		memoryCurationActions:        map[string]memoryCurationActionImportScope{},
		memoryCurationMutations:      map[string]bool{},
		factCandidateCurations:       map[string]factCandidateCurationImportScope{},
		memoryRelationCurations:      []memoryRelationCurationImportScope{},
		memoryRelations:              map[string]memoryRelationCurationImportScope{},
	}
}

// normalizeImportedCurationLease prevents an archive from carrying live work
// across cells. It runs before row validation and insertion, but records the
// original linkage so the final cross-row validator can prove that only a
// coherent active lane/request/run triple was interrupted. Generations never
// move backwards: clearing an active lane consumes one fencing generation.
func (ic *importCtx) normalizeImportedCurationLease(table string, obj map[string]any, importedAt time.Time) error {
	badf := func(format string, args ...any) error {
		return fmt.Errorf("%w: %s", ErrArchiveContent, fmt.Sprintf(format, args...))
	}
	switch table {
	case "memory_curation_lanes":
		activeRunID, active := optionalStringField(obj, "active_run_id")
		if !active {
			return nil
		}
		if !validImportedGeneratedID(activeRunID, "mrun") {
			return badf("memory_curation_lanes row active_run_id is invalid")
		}
		owner, err := ic.importedMemoryOwner(obj)
		if err != nil {
			return badf("memory_curation_lanes row %v", err)
		}
		requestGeneration, ok := importedGeneration(obj["request_generation"], true)
		if !ok {
			return badf("memory_curation_lanes row request_generation is invalid")
		}
		fencingGeneration, ok := importedGeneration(obj["fencing_generation"], true)
		if !ok || fencingGeneration >= maxImportedCurationGeneration {
			return badf("memory_curation_lanes active fence has no import reserve")
		}
		fencingGeneration++
		ic.memoryCurationLanes[owner] = memoryCurationLaneImportScope{
			owner: owner, requestGeneration: requestGeneration,
			fencingGeneration: fencingGeneration, activeRunID: activeRunID,
			normalizedActive: true,
		}
		obj["active_run_id"] = nil
		obj["fencing_generation"] = fencingGeneration
		obj["updated_at"] = importedAt.Format(time.RFC3339Nano)
	case "memory_curation_requests":
		state, _ := obj["state"].(string)
		if state != "claimed" {
			return nil
		}
		requestID, _ := obj["id"].(string)
		claimedRunID, present := optionalStringField(obj, "claimed_run_id")
		if !validImportedGeneratedID(requestID, "mcrq") || !present ||
			!validImportedGeneratedID(claimedRunID, "mrun") {
			return badf("memory_curation_requests claimed row identifiers are invalid")
		}
		if _, present, err := importedOptionalTimestamp(obj, "claimed_at"); err != nil || !present {
			return badf("memory_curation_requests claimed row requires claimed_at")
		}
		for _, field := range []string{"fulfilled_at", "cancelled_at", "dead_lettered_at"} {
			if _, present, err := importedOptionalTimestamp(obj, field); err != nil || present {
				return badf("memory_curation_requests claimed row carries terminal timestamp")
			}
		}
		ic.memoryCurationRequests[requestID] = memoryCurationRequestImportScope{
			claimedRunID: claimedRunID, normalizedClaimed: true,
		}
		obj["state"] = "queued"
		obj["claimed_run_id"] = nil
		obj["claimed_at"] = nil
		obj["due_at"] = importedAt.Format(time.RFC3339Nano)
		obj["updated_at"] = importedAt.Format(time.RFC3339Nano)
	case "memory_curation_runs":
		state, _ := obj["state"].(string)
		if state != "open" && state != "planned" {
			return nil
		}
		runID, _ := obj["id"].(string)
		if !validImportedGeneratedID(runID, "mrun") {
			return badf("memory_curation_runs active row id is invalid")
		}
		if _, present, err := importedOptionalTimestamp(obj, "lease_expires_at"); err != nil || !present {
			return badf("memory_curation_runs active row requires lease_expires_at")
		}
		if _, present, err := importedOptionalTimestamp(obj, "terminal_at"); err != nil || present {
			return badf("memory_curation_runs active row carries terminal_at")
		}
		ic.memoryCurationRuns[runID] = memoryCurationRunImportScope{
			state: state, normalizedInterrupted: true,
		}
		obj["state"] = "interrupted"
		obj["lease_expires_at"] = nil
		obj["terminal_reason_code"] = "cell_import"
		obj["terminal_at"] = importedAt.Format(time.RFC3339Nano)
		obj["updated_at"] = importedAt.Format(time.RFC3339Nano)
	}
	return nil
}

// normalizeImportedMessageClaim prevents a runner lease from crossing cells.
// The archive must first prove the complete strict source shape; an active
// claim then becomes available and consumes one generation so every old fence
// is permanently stale on the destination. Completed links are preserved.
func (ic *importCtx) normalizeImportedMessageClaim(table string, obj map[string]any) error {
	if table != "agent_message_deliveries" {
		return nil
	}
	scope, err := validateImportedMessageProcessingShape(obj)
	if err != nil {
		return fmt.Errorf("%w: agent_message_deliveries row %v", ErrArchiveContent, err)
	}
	if scope.processingState != MessageProcessingClaimed {
		return nil
	}
	if scope.processingGeneration >= maxMessageProcessingGeneration {
		return fmt.Errorf("%w: agent_message_deliveries active claim has no import fence reserve", ErrArchiveContent)
	}
	obj["processing_state"] = MessageProcessingAvailable
	obj["processing_generation"] = scope.processingGeneration + 1
	obj["claim_id"] = nil
	obj["claim_key_hash"] = ""
	obj["lease_expires_at"] = nil
	obj["completed_at"] = nil
	obj["complete_key_hash"] = ""
	obj["result_message_id"] = nil
	return nil
}

// normalizeImportedMessageRequestClaim prevents a selection reservation or
// runner fence from becoming live authority on a different cell. The source
// row must first prove its strict lifecycle shape. Reserved and claimed work
// is then retained as cancelled history; claimed work also consumes one
// generation so every source-cell fence is permanently stale. Released,
// completed, and already-cancelled history is preserved byte-for-byte.
func (ic *importCtx) normalizeImportedMessageRequestClaim(
	table string,
	obj map[string]any,
	importedAt time.Time,
) error {
	if table != "agent_message_request_claims" {
		return nil
	}
	scope, err := validateImportedMessageRequestClaimContent(obj)
	if err != nil {
		return fmt.Errorf("%w: agent_message_request_claims row %v", ErrArchiveContent, err)
	}
	switch scope.state {
	case "reserved":
		obj["state"] = "cancelled"
		obj["lease_expires_at"] = nil
		obj["cancelled_at"] = importedAt.Format(time.RFC3339Nano)
		obj["updated_at"] = importedAt.Format(time.RFC3339Nano)
	case "claimed":
		if scope.generation >= maxMessageProcessingGeneration {
			return fmt.Errorf(
				"%w: agent_message_request_claims active claim has no import fence reserve",
				ErrArchiveContent,
			)
		}
		obj["state"] = "cancelled"
		obj["generation"] = scope.generation + 1
		obj["lease_expires_at"] = nil
		obj["cancelled_at"] = importedAt.Format(time.RFC3339Nano)
		obj["updated_at"] = importedAt.Format(time.RFC3339Nano)
	}
	return nil
}

const maxImportedCurationGeneration int64 = 4611686018427387903

func importedGeneration(raw any, allowZero bool) (int64, bool) {
	value, ok := importedNonnegativeInteger(raw)
	if !ok || value > maxImportedCurationGeneration || (!allowZero && value == 0) {
		return 0, false
	}
	return value, true
}

// validateAndRecord is the row-content boundary: it enforces the account
// scoping the FKs alone cannot, and records the id (if any) for later FK
// targets. Called BEFORE the INSERT so a bad row aborts the transaction
// without touching the database.
func (ic *importCtx) validateAndRecord(table string, obj map[string]any) error {
	badf := func(format string, args ...any) error {
		return fmt.Errorf("%w: %s", ErrArchiveContent, fmt.Sprintf(format, args...))
	}
	// Tables that carry account_id must have it equal the manifest's.
	// Agents scope transitively: their realm_id lands in ic.realms only
	// for realms this archive itself just wrote, so the realm_id check
	// below is the FK-safety boundary for that table.
	switch table {
	case "operators", "realms", "tokens", "account_events",
		"agent_vault_keys", "secrets", "secret_fields", "secret_deks",
		"secret_mutation_receipts",
		"avatar_style_packs", "avatar_style_pack_versions", "realm_avatar_styles",
		"avatar_style_rollout_jobs",
		"agent_avatar_profiles", "agent_avatar_versions",
		"agent_avatar_activations", "agent_avatar_rejections", "agent_avatar_resets",
		"avatar_mutation_receipts",
		"support_tickets", "support_ticket_messages",
		"transcript_conversations", "transcript_entries",
		"usage_events", "usage_rollups",
		"fact_subjects", "facts", "fact_mutation_tombstones", "fact_assertions", "fact_candidates",
		"agent_messages", "agent_message_deliveries",
		"agent_message_requests", "agent_message_request_candidates",
		"agent_message_request_selections", "agent_message_request_claims",
		"memory_change_clocks", "memories", "memory_versions",
		"memory_vector_profiles", "memory_vectors",
		"memory_evidence", "memory_relations", "memory_deleted_references",
		"memory_curation_lanes", "memory_curation_cursors",
		"memory_curation_requests", "memory_curation_runs",
		"memory_curation_run_inputs", "memory_curation_actions",
		"memory_curation_mutations":
		id, err := requireStringField(obj, "account_id")
		if err != nil {
			return badf("%s row missing account_id", table)
		}
		if id != ic.accountID {
			return badf("%s row account_id %q does not match manifest %q", table, id, ic.accountID)
		}
	}
	switch table {
	case "accounts":
		id, err := requireStringField(obj, "id")
		if err != nil || id != ic.accountID {
			return badf("accounts row id %q does not match manifest %q", id, ic.accountID)
		}
		if v, ok := obj["is_default"]; ok {
			if b, _ := v.(bool); b {
				return badf("accounts row claims is_default=true")
			}
		}
		if status, ok := obj["status"].(string); ok {
			ic.accountStatus = status
		}
		// Plan-snapshot shape checks: these jsonb columns are decoded into
		// typed Go values on every read (map[string]int64 / []string), so a
		// malformed value would import fine and then poison the account —
		// every GetAccount and every gated create fails until the control
		// plane re-applies a snapshot. Content-hostile streams must land
		// nothing, so refuse the shapes here (absent keys are fine: archives
		// from before migration 0017 fall back to the column defaults).
		if v, present := obj["plan"]; present {
			if _, ok := v.(string); !ok {
				return badf("accounts row plan must be a string")
			}
		}
		if v, present := obj["plan_limits"]; present {
			m, ok := v.(map[string]any)
			if !ok {
				return badf("accounts row plan_limits must be an object of integer limits")
			}
			for key, raw := range m {
				if _, ok := importedInteger(raw); !ok {
					return badf("accounts row plan_limits[%q] must be an integer", key)
				}
			}
		}
		if v, present := obj["plan_features"]; present {
			fs, ok := v.([]any)
			if !ok {
				return badf("accounts row plan_features must be an array of strings")
			}
			for _, raw := range fs {
				if _, ok := raw.(string); !ok {
					return badf("accounts row plan_features entries must be strings")
				}
			}
		}
		if v, present := obj["placement_policy"]; present {
			if _, err := placement.FromAny(v); err != nil {
				return badf("accounts row %v", err)
			}
		}
		ic.accounts++
		if ic.accounts > 1 {
			return badf("archive contains more than one accounts row")
		}
	case "operators":
		if id, ok := stringField(obj, "id"); ok {
			ic.operators[id] = true
		}
	case "realms":
		if id, ok := stringField(obj, "id"); ok {
			ic.realms[id] = true
		}
	case "avatar_style_packs":
		key, scope, err := ic.validateImportedAvatarStyleHead(obj)
		if err != nil {
			return badf("avatar_style_packs row %v", err)
		}
		if _, duplicate := ic.avatarStyleHeads[key]; duplicate {
			return badf("avatar_style_packs row duplicates %s/%s", key.realmID, key.stylePackID)
		}
		ic.avatarStyleHeads[key] = scope
	case "avatar_style_pack_versions":
		key, scope, err := ic.validateImportedAvatarStyleVersion(obj)
		if err != nil {
			return badf("avatar_style_pack_versions row %v", err)
		}
		if _, duplicate := ic.avatarStyleVersions[key]; duplicate {
			return badf("avatar_style_pack_versions row duplicates %s/%s@%d", key.realmID, key.stylePackID, key.version)
		}
		ic.avatarStyleVersions[key] = scope
	case "realm_avatar_styles":
		scope, err := ic.validateImportedRealmAvatarStyle(obj)
		if err != nil {
			return badf("realm_avatar_styles row %v", err)
		}
		if _, duplicate := ic.realmAvatarStyles[scope.style.realmID]; duplicate {
			return badf("realm_avatar_styles row duplicates realm %q", scope.style.realmID)
		}
		ic.realmAvatarStyles[scope.style.realmID] = scope
	case "avatar_style_rollout_jobs":
		key, scope, err := ic.validateImportedAvatarStyleRollout(obj)
		if err != nil {
			return badf("avatar_style_rollout_jobs row %v", err)
		}
		if _, duplicate := ic.avatarStyleRollouts[key]; duplicate {
			return badf("avatar_style_rollout_jobs row duplicates realm %q revision %d", key.realmID, key.styleRevision)
		}
		if scope.status == "pending" || scope.status == "running" {
			ic.avatarStyleOpenRollouts[key.realmID]++
			if ic.avatarStyleOpenRollouts[key.realmID] > 1 {
				return badf("avatar_style_rollout_jobs has multiple open jobs for realm %q", key.realmID)
			}
		}
		ic.avatarStyleRollouts[key] = scope
	case "agents":
		realmID, err := requireStringField(obj, "realm_id")
		if err != nil {
			return badf("agents row missing realm_id")
		}
		if !ic.realms[realmID] {
			return badf("agents row references realm %q not present in this archive", realmID)
		}
		_, deleted, err := importedOptionalTimestamp(obj, "deleted_at")
		if err != nil {
			return badf("agents row %v", err)
		}
		if id, ok := stringField(obj, "id"); ok {
			ic.agents[id] = true
			ic.liveAgents[id] = !deleted
			ic.agentRealms[id] = realmID
		}
	case "agent_vault_keys":
		scope, err := ic.validateImportedVaultKey(obj)
		if err != nil {
			return badf("agent_vault_keys row %v", err)
		}
		versionKey := secretVaultKeyVersionImportKey{agentID: scope.agentID, version: scope.version}
		if _, duplicateID := ic.vaultKeys[scope.id]; duplicateID || ic.vaultKeyVersions[versionKey] != "" {
			return badf("agent_vault_keys row duplicates id or agent key version")
		}
		if ic.vaultFingerprints[scope.fingerprint] {
			return badf("agent_vault_keys row reuses a key fingerprint")
		}
		if scope.state == "current" {
			ic.vaultCurrentKeys[scope.agentID]++
			if ic.vaultCurrentKeys[scope.agentID] > 1 {
				return badf("agent_vault_keys has multiple current keys for one agent")
			}
		}
		ic.vaultKeys[scope.id] = scope
		ic.vaultKeyVersions[versionKey] = scope.id
		ic.vaultFingerprints[scope.fingerprint] = true
	case "secrets":
		scope, normalizedName, err := ic.validateImportedSecret(obj)
		if err != nil {
			return badf("secrets row %v", err)
		}
		id, _ := requireStringField(obj, "id")
		if _, duplicate := ic.secrets[id]; duplicate {
			return badf("secrets row duplicates id")
		}
		if !scope.archived && !scope.deleted {
			names := ic.secretNames[scope.agentID]
			if names == nil {
				names = map[string]string{}
				ic.secretNames[scope.agentID] = names
			}
			if previous := names[normalizedName]; previous != "" {
				return badf("secrets row duplicates a live name")
			}
			names[normalizedName] = id
		}
		ic.secrets[id] = scope
	case "secret_fields":
		scope, name, err := ic.validateImportedSecretField(obj)
		if err != nil {
			return badf("secret_fields row %v", err)
		}
		id, _ := requireStringField(obj, "id")
		if _, duplicate := ic.secretFields[id]; duplicate {
			return badf("secret_fields row duplicates id")
		}
		names := ic.secretFieldNames[scope.secretID]
		if names == nil {
			names = map[string]bool{}
			ic.secretFieldNames[scope.secretID] = names
		}
		if names[name] {
			return badf("secret_fields row duplicates a field name")
		}
		names[name] = true
		ic.secretFieldCounts[scope.secretID]++
		if ic.secretFieldCounts[scope.secretID] > maxSecretFields {
			return badf("secrets row has more than %d fields", maxSecretFields)
		}
		if scope.valueVersion > ic.secretMaxValueVersions[scope.secretID] {
			ic.secretMaxValueVersions[scope.secretID] = scope.valueVersion
		}
		ic.secretFields[id] = scope
	case "secret_deks":
		scope, err := ic.validateImportedSecretDEK(obj)
		if err != nil {
			return badf("secret_deks row %v", err)
		}
		id, _ := requireStringField(obj, "id")
		generationKey := secretDEKGenerationImportKey{fieldID: scope.fieldID, generation: scope.generation}
		if _, duplicate := ic.secretDEKs[id]; duplicate || ic.secretDEKGenerations[generationKey] {
			return badf("secret_deks row duplicates id or field generation")
		}
		if !scope.retired {
			ic.secretCurrentDEKs[scope.fieldID]++
			if ic.secretCurrentDEKs[scope.fieldID] > 1 {
				return badf("secret_deks has multiple current DEKs for one field")
			}
		}
		ic.secretDEKs[id] = scope
		ic.secretDEKGenerations[generationKey] = true
	case "secret_mutation_receipts":
		key, err := ic.validateImportedSecretReceipt(obj)
		if err != nil {
			return badf("secret_mutation_receipts row %v", err)
		}
		if ic.secretReceipts[key] {
			return badf("secret_mutation_receipts row duplicates its retry fence")
		}
		ic.secretReceipts[key] = true
	case "agent_avatar_profiles":
		scope, err := ic.validateImportedAvatarProfile(obj)
		if err != nil {
			return badf("agent_avatar_profiles row %v", err)
		}
		if _, duplicate := ic.avatarProfiles[scope.agentID]; duplicate {
			return badf("agent_avatar_profiles row duplicates agent %q", scope.agentID)
		}
		ic.avatarProfiles[scope.agentID] = scope
	case "agent_avatar_versions":
		key, scope, err := ic.validateImportedAvatarVersion(obj)
		if err != nil {
			return badf("agent_avatar_versions row %v", err)
		}
		if _, duplicate := ic.avatarVersions[key]; duplicate || ic.avatarVersionIDs[scope.id] {
			return badf("agent_avatar_versions row duplicates version or id")
		}
		ic.avatarVersions[key] = scope
		ic.avatarVersionIDs[scope.id] = true
		ic.recordAvatarLineageTime(avatarLineageImportKey{
			agentID: key.agentID, generation: scope.lineage,
		}, scope.proposedAt)
	case "agent_avatar_activations":
		if err := ic.validateImportedAvatarActivation(obj); err != nil {
			return badf("agent_avatar_activations row %v", err)
		}
	case "agent_avatar_rejections":
		key, err := ic.validateImportedAvatarRejection(obj)
		if err != nil {
			return badf("agent_avatar_rejections row %v", err)
		}
		if ic.avatarRejectedVersions[key] {
			return badf("agent_avatar_rejections row duplicates agent version")
		}
		ic.avatarRejectedVersions[key] = true
	case "agent_avatar_resets":
		if err := ic.validateImportedAvatarReset(obj); err != nil {
			return badf("agent_avatar_resets row %v", err)
		}
	case "avatar_mutation_receipts":
		if err := ic.validateImportedAvatarReceipt(obj); err != nil {
			return badf("avatar_mutation_receipts row %v", err)
		}
	case "agent_activity":
		agentID, err := requireStringField(obj, "agent_id")
		if err != nil || !ic.agents[agentID] {
			return badf("agent_activity row references agent %q not present in this archive", agentID)
		}
		labels := make(map[string]string, 5)
		for _, field := range []string{"runtime", "location_id", "last_event", "last_event_id"} {
			value, err := requireStringField(obj, field)
			if err != nil {
				return badf("agent_activity row missing %s", field)
			}
			cleaned, err := cleanAgentActivityLabel(field, value, maxAgentActivityLabelBytes, true)
			if err != nil || cleaned != value {
				return badf("agent_activity row %s is not a canonical clean label", field)
			}
			labels[field] = value
		}
		location, ok := obj["location"].(string)
		if !ok {
			return badf("agent_activity row location must be a string")
		}
		cleanedLocation, err := cleanAgentActivityLabel("location", location, maxAgentActivityLocationBytes, false)
		if err != nil || cleanedLocation != location {
			return badf("agent_activity row location is not a canonical clean label")
		}
		occurredAt, err := requireImportedTimestamp(obj, "last_event_occurred_at")
		if err != nil {
			return badf("agent_activity row last_event_occurred_at is invalid")
		}
		if err := ic.requireTimestampAtOrBeforeExport("last_event_occurred_at", *occurredAt); err != nil {
			return badf("agent_activity row %v", err)
		}
		activityAt, err := requireImportedTimestamp(obj, "last_activity_at")
		if err != nil {
			return badf("agent_activity row last_activity_at is invalid")
		}
		if err := ic.requireTimestampAtOrBeforeExport("last_activity_at", *activityAt); err != nil {
			return badf("agent_activity row %v", err)
		}
		if activityAt.Before(*occurredAt) {
			return badf("agent_activity row last_activity_at precedes last_event_occurred_at")
		}
		key := agentActivityImportKey{agentID: agentID, runtime: labels["runtime"], locationID: labels["location_id"]}
		if ic.agentActivity[key] {
			return badf("agent_activity row duplicates an agent/runtime/location projection")
		}
		ic.agentActivity[key] = true
	case "tokens":
		kind, kindErr := requireStringField(obj, "kind")
		accessProfile, profileErr := requireStringField(obj, "access_profile")
		if kindErr != nil || profileErr != nil || !validTokenAccessProfile(kind, accessProfile) {
			return badf("tokens row access_profile is invalid for kind %q", kind)
		}
		if opID, present := optionalStringField(obj, "operator_id"); present && !ic.operators[opID] {
			return badf("tokens row references operator %q not present in this archive", opID)
		}
		if agID, present := optionalStringField(obj, "agent_id"); present && !ic.agents[agID] {
			return badf("tokens row references agent %q not present in this archive", agID)
		}
	case "fact_subjects":
		realmID, err := requireStringField(obj, "realm_id")
		agentID, agentErr := requireStringField(obj, "owner_agent_id")
		if err != nil || agentErr != nil || !ic.agents[agentID] || ic.agentRealms[agentID] != realmID {
			return badf("fact_subjects row owner %q is outside realm %q", agentID, realmID)
		}
		id, err := requireStringField(obj, "id")
		if err != nil {
			return badf("fact_subjects row missing id")
		}
		canonicalKey, err := validateImportedFactSubjectContent(obj)
		if err != nil {
			return badf("fact_subjects row %v", err)
		}
		namespace := ic.factSubjectNames[agentID]
		if namespace == nil {
			namespace = map[string]string{}
			ic.factSubjectNames[agentID] = namespace
		}
		names := []string{canonicalKey}
		for _, raw := range obj["aliases"].([]any) {
			names = append(names, raw.(string))
		}
		for _, name := range names {
			if existing := namespace[name]; existing != "" && existing != id {
				return badf("fact_subjects row name %q conflicts with subject %q", name, existing)
			}
			namespace[name] = id
		}
		ic.factSubjects[id] = factImportScope{realmID: realmID, ownerAgentID: agentID, subjectKey: canonicalKey}
	case "facts":
		subjectID, err := requireStringField(obj, "subject_id")
		scope, ok := ic.factSubjects[subjectID]
		if err != nil || !ok {
			return badf("facts row references subject %q not present in this archive", subjectID)
		}
		realmID, _ := requireStringField(obj, "realm_id")
		agentID, _ := requireStringField(obj, "owner_agent_id")
		if realmID != scope.realmID || agentID != scope.ownerAgentID {
			return badf("facts row scope does not match subject %q", subjectID)
		}
		id, err := requireStringField(obj, "id")
		if err != nil {
			return badf("facts row missing id")
		}
		resolvedID, deleted, replacementID, err := validateImportedFactContent(obj, scope.ownerAgentID)
		if err != nil {
			return badf("facts row %v", err)
		}
		scope.resolvedAssertionID = resolvedID
		scope.subjectID = subjectID
		scope.predicate, _ = requireStringField(obj, "predicate")
		scope.sensitive, _ = obj["sensitive"].(bool)
		scope.deleted = deleted
		scope.replacementFactID = replacementID
		scope.deletedMutationKeyCount, _ = importedNonnegativeInteger(obj["deleted_mutation_key_count"])
		ic.facts[id] = scope
	case "fact_mutation_tombstones":
		factID, err := requireStringField(obj, "fact_id")
		scope, ok := ic.facts[factID]
		if err != nil || !ok || !scope.deleted {
			return badf("fact_mutation_tombstones row references non-deleted fact %q", factID)
		}
		realmID, _ := requireStringField(obj, "realm_id")
		agentID, _ := requireStringField(obj, "owner_agent_id")
		if realmID != scope.realmID || agentID != scope.ownerAgentID {
			return badf("fact_mutation_tombstones row scope does not match fact %q", factID)
		}
		if err := validateImportedFactMutationTombstone(obj); err != nil {
			return badf("fact_mutation_tombstones row %v", err)
		}
		ic.factMutationTombstoneCounts[factID]++
	case "fact_assertions":
		factID, err := requireStringField(obj, "fact_id")
		scope, ok := ic.facts[factID]
		if err != nil || !ok {
			return badf("fact_assertions row references fact %q not present in this archive", factID)
		}
		if scope.deleted {
			return badf("fact_assertions row references deleted fact %q", factID)
		}
		realmID, _ := requireStringField(obj, "realm_id")
		if realmID != scope.realmID {
			return badf("fact_assertions row realm does not match fact %q", factID)
		}
		if agentID, present := optionalStringField(obj, "asserted_by_agent_id"); present && agentID != scope.ownerAgentID {
			return badf("fact_assertions row actor %q does not own fact %q", agentID, factID)
		}
		if err := validateImportedFactAssertionContent(obj); err != nil {
			return badf("fact_assertions row %v", err)
		}
		if prior, present := optionalStringField(obj, "supersedes_id"); present && ic.assertions[prior] != factID {
			return badf("fact_assertions row supersedes %q outside fact %q", prior, factID)
		}
		id, err := requireStringField(obj, "id")
		if err != nil {
			return badf("fact_assertions row missing id")
		}
		ic.assertions[id] = factID
	case "fact_candidates":
		realmID, err := requireStringField(obj, "realm_id")
		agentID, agentErr := requireStringField(obj, "owner_agent_id")
		if err != nil || agentErr != nil || !ic.agents[agentID] || ic.agentRealms[agentID] != realmID {
			return badf("fact_candidates row owner %q is outside realm %q", agentID, realmID)
		}
		if err := validateImportedFactCandidateContent(obj); err != nil {
			return badf("fact_candidates row %v", err)
		}
		subjectKey, _ := requireStringField(obj, "subject_key")
		predicate, _ := requireStringField(obj, "predicate")
		addressSubjectKey := subjectKey
		if subjectID := ic.factSubjectNames[agentID][subjectKey]; subjectID != "" {
			addressSubjectKey = ic.factSubjects[subjectID].subjectKey
		}
		for _, factScope := range ic.facts {
			if factScope.deleted && factScope.replacementFactID == "" &&
				factScope.realmID == realmID && factScope.ownerAgentID == agentID &&
				factScope.subjectKey == addressSubjectKey && factScope.predicate == predicate {
				return badf("fact_candidates row occupies an unrecreated deleted fact address")
			}
		}
		for _, field := range []string{"conflict_fact_id", "resolved_fact_id"} {
			factID, present := optionalStringField(obj, field)
			if !present {
				continue
			}
			scope, ok := ic.facts[factID]
			if !ok || scope.realmID != realmID || scope.ownerAgentID != agentID {
				return badf("fact_candidates row %s %q is outside its agent scope", field, factID)
			}
			if scope.subjectKey != addressSubjectKey || scope.predicate != predicate {
				return badf("fact_candidates row %s %q is at a different fact address", field, factID)
			}
		}
		observedAssertionID, observed := optionalStringField(obj, "observed_assertion_id")
		if observed {
			factID, ok := ic.assertions[observedAssertionID]
			scope, scoped := ic.facts[factID]
			if !ok || !scoped || scope.realmID != realmID || scope.ownerAgentID != agentID {
				return badf("fact_candidates row observed_assertion_id %q is outside its agent scope", observedAssertionID)
			}
			if scope.subjectKey != addressSubjectKey || scope.predicate != predicate {
				return badf("fact_candidates row observed_assertion_id %q is at a different fact address", observedAssertionID)
			}
			if conflictFactID, conflict := optionalStringField(obj, "conflict_fact_id"); conflict && factID != conflictFactID {
				return badf("fact_candidates row observed_assertion_id %q does not belong to conflict fact %q", observedAssertionID, conflictFactID)
			}
		}
		decisionAssertionID, decided := optionalStringField(obj, "decision_assertion_id")
		if decided {
			factID, ok := ic.assertions[decisionAssertionID]
			resolvedFactID, resolved := optionalStringField(obj, "resolved_fact_id")
			if !ok || !resolved || factID != resolvedFactID {
				return badf("fact_candidates row decision_assertion_id %q does not belong to resolved fact %q", decisionAssertionID, resolvedFactID)
			}
			scope := ic.facts[factID]
			if scope.subjectKey != addressSubjectKey || scope.predicate != predicate {
				return badf("fact_candidates row decision_assertion_id %q is at a different fact address", decisionAssertionID)
			}
		}
		candidateID, err := requireStringField(obj, "id")
		if err != nil || !strings.HasPrefix(candidateID, "fcand_") {
			return badf("fact_candidates row id is invalid")
		}
		curationRunID, hasRun := optionalStringField(obj, "curation_run_id")
		curationActionID, hasAction := optionalStringField(obj, "curation_action_id")
		if hasRun != hasAction || (hasRun &&
			(!validImportedGeneratedID(curationRunID, "mrun") ||
				!validImportedGeneratedID(curationActionID, "mact"))) {
			return badf("fact_candidates row curation attribution is invalid")
		}
		status, _ := requireStringField(obj, "status")
		if status == "withdrawn" && !hasRun {
			return badf("fact_candidates withdrawn row requires curation attribution")
		}
		if hasRun {
			ic.factCandidateCurations[candidateID] = factCandidateCurationImportScope{
				owner: memoryOwnerImportKey{realmID: realmID, ownerKind: "agent", ownerID: agentID},
				runID: curationRunID, actionID: curationActionID, status: status,
			}
		}
	case "account_events":
		// The account_id scoping check already ran in the first switch;
		// no downstream table references account_events, so nothing to
		// record here. Metadata is opaque JSONB — the write-time verb
		// contract was enforced when the event was created and doesn't
		// need to be re-enforced at import time (an old cell may have
		// written events under a schema this cell no longer knows).
	case "support_tickets":
		// Record the ticket id so incoming support_ticket_messages
		// rows can be FK-validated against this-archive tickets only.
		if id, ok := stringField(obj, "id"); ok {
			ic.tickets[id] = true
		}
	case "support_ticket_messages":
		// FK-scope check: the ticket_id must belong to a ticket this
		// same archive already inserted. Cross-tenant grafting is
		// blocked the same way agents.realm_id is checked against
		// ic.realms.
		ticketID, err := requireStringField(obj, "ticket_id")
		if err != nil {
			return badf("support_ticket_messages row missing ticket_id")
		}
		if !ic.tickets[ticketID] {
			return badf("support_ticket_messages row references ticket %q not present in this archive", ticketID)
		}
	case "transcript_conversations":
		realmID, err := requireStringField(obj, "realm_id")
		if err != nil || !ic.realms[realmID] {
			return badf("transcript_conversations row references realm %q not present in this archive", realmID)
		}
		agentID, err := requireStringField(obj, "owner_agent_id")
		if err != nil || !ic.agents[agentID] {
			return badf("transcript_conversations row references agent %q not present in this archive", agentID)
		}
		id, err := requireStringField(obj, "id")
		if err != nil {
			return badf("transcript_conversations row missing id")
		}
		nextSequence := int64(math.MaxInt64)
		if raw, present := obj["next_sequence"]; present {
			var ok bool
			nextSequence, ok = importedPositiveInteger(raw)
			if !ok {
				return badf("transcript_conversations row next_sequence must be a positive integer")
			}
		}
		ic.transcripts[id] = transcriptImportScope{
			realmID: realmID, ownerAgentID: agentID, nextSequence: nextSequence,
		}
	case "transcript_entries":
		transcriptID, err := requireStringField(obj, "transcript_id")
		if err != nil {
			return badf("transcript_entries row missing transcript_id")
		}
		scope, ok := ic.transcripts[transcriptID]
		if !ok {
			return badf("transcript_entries row references transcript %q not present in this archive", transcriptID)
		}
		realmID, err := requireStringField(obj, "realm_id")
		if err != nil || realmID != scope.realmID {
			return badf("transcript_entries row realm %q does not match transcript realm %q", realmID, scope.realmID)
		}
		agentID, err := requireStringField(obj, "recorded_by_agent_id")
		if err != nil || agentID != scope.ownerAgentID || !ic.agents[agentID] {
			return badf("transcript_entries row recorder %q does not match transcript owner %q", agentID, scope.ownerAgentID)
		}
		if replyID, present := optionalStringField(obj, "reply_to_entry_id"); present {
			if parentTranscript, ok := ic.entries[replyID]; !ok || parentTranscript != transcriptID {
				return badf("transcript_entries row reply target %q is not an earlier entry in transcript %q", replyID, transcriptID)
			}
		}
		id, err := requireStringField(obj, "id")
		if err != nil {
			return badf("transcript_entries row missing id")
		}
		ic.entries[id] = transcriptID
	case "usage_events":
		if err := ic.validateUsageScope(obj, badf, "usage_events"); err != nil {
			return err
		}
		dimension, _ := stringField(obj, "dimension")
		if strings.HasPrefix(dimension, "transcript_") {
			subjectType, _ := stringField(obj, "subject_type")
			subjectID, _ := stringField(obj, "subject_id")
			scope, ok := ic.transcripts[subjectID]
			agentID, _ := stringField(obj, "agent_id")
			realmID, _ := stringField(obj, "realm_id")
			if subjectType != "transcript" || !ok || scope.ownerAgentID != agentID || scope.realmID != realmID {
				return badf("usage_events row transcript subject %q does not belong to its agent scope", subjectID)
			}
		}
		if strings.HasPrefix(dimension, "fact_") {
			subjectType, _ := stringField(obj, "subject_type")
			subjectID, _ := stringField(obj, "subject_id")
			scope, ok := ic.facts[subjectID]
			agentID, _ := stringField(obj, "agent_id")
			realmID, _ := stringField(obj, "realm_id")
			if subjectType != "fact" || !ok || scope.ownerAgentID != agentID || scope.realmID != realmID {
				return badf("usage_events row fact subject %q does not belong to its agent scope", subjectID)
			}
		}
	case "usage_rollups":
		if err := ic.validateUsageScope(obj, badf, "usage_rollups"); err != nil {
			return err
		}
	case "agent_messages":
		realmID, err := requireStringField(obj, "realm_id")
		if err != nil || !ic.realms[realmID] {
			return badf("agent_messages row references realm %q not present in this archive", realmID)
		}
		fromAgentID, err := requireStringField(obj, "from_agent_id")
		if err != nil || !ic.agents[fromAgentID] {
			return badf("agent_messages row references sender %q not present in this archive", fromAgentID)
		}
		if ic.agentRealms[fromAgentID] != realmID {
			return badf("agent_messages row sender must belong to realm %q", realmID)
		}
		audienceKind, err := requireStringField(obj, "audience_kind")
		if err != nil {
			return badf("agent_messages row missing audience_kind")
		}
		audienceFingerprint, ok := obj["audience_fingerprint"].(string)
		if !ok {
			return badf("agent_messages row audience_fingerprint must be a string")
		}
		toAgentID, hasToAgent := optionalStringField(obj, "to_agent_id")
		switch audienceKind {
		case MessageRecipientAgent:
			if !hasToAgent || !ic.agents[toAgentID] || ic.agentRealms[toAgentID] != realmID || audienceFingerprint != "" {
				return badf("agent_messages direct audience recipient must belong to realm %q", realmID)
			}
		case MessageRecipientAgents:
			if hasToAgent || !isSHA256Hex(audienceFingerprint) {
				return badf("agent_messages agents audience is invalid")
			}
		case MessageRecipientRealm:
			if hasToAgent || audienceFingerprint != messageRealmAudienceFingerprint() {
				return badf("agent_messages realm audience is invalid")
			}
		default:
			return badf("agent_messages audience_kind %q is invalid", audienceKind)
		}
		id, err := requireStringField(obj, "id")
		if err != nil {
			return badf("agent_messages row missing id")
		}
		threadID, err := requireStringField(obj, "thread_id")
		if err != nil {
			return badf("agent_messages row missing thread_id")
		}
		replyTo, _ := stringField(obj, "reply_to_message_id")
		kind, err := requireStringField(obj, "kind")
		if err != nil {
			return badf("agent_messages row missing kind")
		}
		causalDepth, ok := importedPositiveInteger(obj["causal_depth"])
		if !ok || causalDepth > maxMessageCausalDepth {
			return badf("agent_messages row causal_depth is invalid")
		}
		createdAt, err := requireImportedTimestamp(obj, "created_at")
		if err != nil {
			return badf("agent_messages row created_at must be an RFC3339 timestamp")
		}
		if err := ic.requireTimestampAtOrBeforeExport("agent_messages created_at", *createdAt); err != nil {
			return badf("%v", err)
		}
		ic.messages[id] = messageImportScope{
			realmID: realmID, fromAgentID: fromAgentID, toAgentID: toAgentID,
			audienceKind: audienceKind, audienceFingerprint: audienceFingerprint,
			recipientAgentIDs: map[string]bool{},
			kind:              kind, threadID: threadID, replyToMessageID: replyTo, causalDepth: causalDepth,
			createdAt: *createdAt,
		}
	case "agent_message_deliveries":
		messageID, err := requireStringField(obj, "message_id")
		if err != nil {
			return badf("agent_message_deliveries row missing message_id")
		}
		scope, ok := ic.messages[messageID]
		if !ok {
			return badf("agent_message_deliveries row references message %q not present in this archive", messageID)
		}
		realmID, err := requireStringField(obj, "realm_id")
		if err != nil || realmID != scope.realmID {
			return badf("agent_message_deliveries row realm %q does not match message realm %q", realmID, scope.realmID)
		}
		recipientID, err := requireStringField(obj, "recipient_agent_id")
		audienceKind := scope.audienceKind
		if audienceKind == "" {
			audienceKind = MessageRecipientAgent
		}
		if audienceKind == MessageRecipientAgent && recipientID != scope.toAgentID {
			return badf("agent_message_deliveries recipient %q does not match message recipient %q", recipientID, scope.toAgentID)
		}
		if err != nil || !ic.agents[recipientID] || ic.agentRealms[recipientID] != realmID {
			return badf("agent_message_deliveries recipient %q is outside message realm %q", recipientID, realmID)
		}
		if audienceKind == MessageRecipientRealm && recipientID == scope.fromAgentID {
			return badf("agent_message_deliveries realm audience includes its sender %q", recipientID)
		}
		processing, err := validateImportedMessageProcessingShape(obj)
		if err != nil {
			return badf("agent_message_deliveries row %v", err)
		}
		deliveryKey := messageID + "\x00" + recipientID
		if _, duplicate := ic.deliveries[deliveryKey]; duplicate {
			return badf("agent_message_deliveries duplicates message %q recipient %q", messageID, recipientID)
		}
		processing.messageID = messageID
		processing.recipientAgentID = recipientID
		if ic.deliveries == nil {
			ic.deliveries = map[string]messageDeliveryImportScope{}
		}
		ic.deliveries[deliveryKey] = processing
		if scope.recipientAgentIDs == nil {
			scope.recipientAgentIDs = map[string]bool{}
			ic.messages[messageID] = scope
		}
		scope.recipientAgentIDs[recipientID] = true
	case "agent_message_requests":
		scope, err := ic.validateImportedMessageRequest(obj)
		if err != nil {
			return badf("agent_message_requests row %v", err)
		}
		requestID, _ := requireStringField(obj, "id")
		if _, duplicate := ic.messageRequests[requestID]; duplicate {
			return badf("agent_message_requests duplicates id %q", requestID)
		}
		if ic.messageRequests == nil {
			ic.messageRequests = map[string]messageRequestImportScope{}
		}
		ic.messageRequests[requestID] = scope
	case "agent_message_request_candidates":
		key, scope, err := ic.validateImportedMessageRequestCandidate(obj)
		if err != nil {
			return badf("agent_message_request_candidates row %v", err)
		}
		if _, duplicate := ic.messageRequestCandidates[key]; duplicate {
			return badf("agent_message_request_candidates duplicates request %q agent %q", key.requestID, key.agentID)
		}
		if ic.messageRequestCandidates == nil {
			ic.messageRequestCandidates = map[messageRequestCandidateImportKey]messageRequestCandidateImportScope{}
		}
		ic.messageRequestCandidates[key] = scope
	case "agent_message_request_selections":
		selectionID, scope, err := ic.validateImportedMessageRequestSelection(obj)
		if err != nil {
			return badf("agent_message_request_selections row %v", err)
		}
		if _, duplicate := ic.messageRequestSelections[selectionID]; duplicate {
			return badf("agent_message_request_selections duplicates id %q", selectionID)
		}
		if ic.messageRequestSelections == nil {
			ic.messageRequestSelections = map[string]messageRequestSelectionImportScope{}
		}
		ic.messageRequestSelections[selectionID] = scope
	case "agent_message_request_claims":
		claimID, scope, err := ic.validateImportedMessageRequestClaim(obj)
		if err != nil {
			return badf("agent_message_request_claims row %v", err)
		}
		if _, duplicate := ic.messageRequestClaims[claimID]; duplicate {
			return badf("agent_message_request_claims duplicates id %q", claimID)
		}
		if ic.messageRequestClaims == nil {
			ic.messageRequestClaims = map[string]messageRequestClaimImportScope{}
		}
		ic.messageRequestClaims[claimID] = scope
	case "memory_change_clocks":
		owner, err := ic.importedMemoryOwner(obj)
		if err != nil {
			return badf("memory_change_clocks row %v", err)
		}
		last, ok := importedNonnegativeInteger(obj["last_change_seq"])
		if !ok || last > maxImportedMemoryChangeSeq {
			return badf(
				"memory_change_clocks row last_change_seq must be between 0 and %d",
				maxImportedMemoryChangeSeq,
			)
		}
		if _, exists := ic.memoryClocks[owner]; exists {
			return badf("memory_change_clocks row duplicates owner %q", owner.ownerID)
		}
		ic.memoryClocks[owner] = last
	case "memories":
		owner, err := ic.importedMemoryOwner(obj)
		if err != nil {
			return badf("memories row %v", err)
		}
		id, err := requireStringField(obj, "id")
		if err != nil || !validImportedGeneratedID(id, "mem") {
			return badf("memories row id is invalid")
		}
		if _, exists := ic.memories[id]; exists {
			return badf("memories row duplicates id %q", id)
		}
		authorID, err := requireStringField(obj, "authored_by_agent_id")
		if err != nil || authorID != owner.ownerID {
			return badf("memories row author %q does not match owner %q", authorID, owner.ownerID)
		}
		origin, err := requireStringField(obj, "origin")
		if err != nil || !memoryCodePattern.MatchString(origin) {
			return badf("memories row origin is invalid")
		}
		captureReason, err := requireStringField(obj, "capture_reason")
		if err != nil || !memoryCodePattern.MatchString(captureReason) {
			return badf("memories row capture_reason is invalid")
		}
		currentVersion, hasCurrent, ok := importedOptionalPositiveInteger(obj, "current_version")
		if !ok {
			return badf("memories row current_version must be null or a positive integer")
		}
		deletedAt, hasDeletedAt, deletedAtErr := importedOptionalTimestamp(obj, "permanently_deleted_at")
		deletedBy, hasDeletedBy := optionalStringField(obj, "permanently_deleted_by_id")
		deleteReason, hasDeleteReason := optionalStringField(obj, "permanent_delete_reason")
		if deletedAtErr != nil {
			return badf("memories row permanently_deleted_at is invalid")
		}
		deleteReceipt, receiptOK := obj["delete_receipt_id"].(string)
		deleteKeyHash, keyHashOK := obj["delete_idempotency_key_hash"].(string)
		deletedPriorVersion, priorOK := importedNonnegativeInteger(obj["deleted_prior_version"])
		deletedScrubRevision, scrubOK := obj["deleted_scrub_set_revision"].(string)
		deletedVersionCount, versionCountOK := importedNonnegativeInteger(obj["deleted_version_count"])
		deletedEvidenceCount, evidenceCountOK := importedNonnegativeInteger(obj["deleted_evidence_count"])
		deletedRelationCount, relationCountOK := importedNonnegativeInteger(obj["deleted_relation_count"])
		deletedShieldCount, shieldCountOK := importedNonnegativeInteger(obj["deleted_retry_shield_count"])
		deletedShieldDigest, shieldDigestOK := obj["deleted_retry_shield_digest"].(string)
		deletedCurationRunCount, curationRunCountOK := importedOptionalNonnegativeInteger(obj, "deleted_curation_run_count")
		deletedCurationActionCount, curationActionCountOK := importedOptionalNonnegativeInteger(obj, "deleted_curation_action_count")
		deletedCurationInputCount, curationInputCountOK := importedOptionalNonnegativeInteger(obj, "deleted_curation_input_count")
		deletedCurationMutationCount, curationMutationCountOK := importedOptionalNonnegativeInteger(obj, "deleted_curation_mutation_count")
		if !receiptOK || !keyHashOK || !priorOK || !scrubOK || !versionCountOK ||
			!evidenceCountOK || !relationCountOK || !shieldCountOK || !shieldDigestOK ||
			!curationRunCountOK || !curationActionCountOK || !curationInputCountOK ||
			!curationMutationCountOK {
			return badf("memories row permanent-deletion receipt fields are invalid")
		}
		deleted := !hasCurrent
		if deleted {
			createdAt, hasCreatedAt, createdAtErr := importedOptionalTimestamp(obj, "created_at")
			updatedAt, hasUpdatedAt, updatedAtErr := importedOptionalTimestamp(obj, "updated_at")
			if !hasDeletedAt || !hasDeletedBy || !hasDeleteReason ||
				origin != "deleted" || captureReason != "deleted" ||
				deletedBy != owner.ownerID || deleteReason != "direct_user_request" ||
				!validImportedGeneratedID(deleteReceipt, "mdel") || !validFactSHA256(deleteKeyHash) ||
				deletedPriorVersion < 1 || !validFactSHA256(deletedScrubRevision) ||
				deletedVersionCount != deletedPriorVersion || deletedEvidenceCount < 1 ||
				deletedShieldCount < deletedVersionCount || !validFactSHA256(deletedShieldDigest) ||
				createdAtErr != nil || updatedAtErr != nil || !hasCreatedAt || !hasUpdatedAt ||
				createdAt.After(*deletedAt) || !updatedAt.Equal(*deletedAt) {
				return badf("memories row deleted tombstone shape is invalid")
			}
		} else if hasDeletedAt || hasDeletedBy || hasDeleteReason ||
			deleteReceipt != "" || deleteKeyHash != "" || deletedPriorVersion != 0 ||
			deletedScrubRevision != "" || deletedVersionCount != 0 || deletedEvidenceCount != 0 ||
			deletedRelationCount != 0 || deletedShieldCount != 0 || deletedShieldDigest != "" ||
			deletedCurationRunCount != 0 || deletedCurationActionCount != 0 ||
			deletedCurationInputCount != 0 || deletedCurationMutationCount != 0 {
			return badf("memories row live head carries permanent-deletion metadata")
		}
		ic.memories[id] = memoryImportScope{
			owner: owner, currentVersion: currentVersion, deleted: deleted,
			deletedRetryShieldCount: deletedShieldCount, deletedRetryShieldDigest: deletedShieldDigest,
		}
	case "memory_versions":
		owner, err := ic.importedMemoryOwner(obj)
		if err != nil {
			return badf("memory_versions row %v", err)
		}
		memoryID, err := requireStringField(obj, "memory_id")
		memory, exists := ic.memories[memoryID]
		if err != nil || !exists {
			return badf("memory_versions row references memory %q not present in this archive", memoryID)
		}
		if memory.owner != owner || memory.deleted {
			return badf("memory_versions row scope does not match live memory %q", memoryID)
		}
		version, ok := importedPositiveInteger(obj["version"])
		if !ok {
			return badf("memory_versions row version must be a positive integer")
		}
		key := memoryVersionImportKey{memoryID: memoryID, version: version}
		if _, exists := ic.memoryVersions[key]; exists {
			return badf("memory_versions row duplicates memory %q version %d", memoryID, version)
		}
		previous, hasPrevious, validPrevious := importedOptionalPositiveInteger(obj, "previous_version")
		if !validPrevious || (version == 1 && hasPrevious) ||
			(version > 1 && (!hasPrevious || previous != version-1)) {
			return badf("memory_versions row previous_version is invalid")
		}
		var previousScope memoryVersionImportScope
		if hasPrevious {
			var exists bool
			previousScope, exists = ic.memoryVersions[memoryVersionImportKey{memoryID: memoryID, version: previous}]
			if !exists {
				return badf("memory_versions row previous version %d is not earlier in memory %q", previous, memoryID)
			}
		}
		changeSeq, ok := importedPositiveInteger(obj["change_seq"])
		if !ok {
			return badf("memory_versions row change_seq must be a positive integer")
		}
		actorID, err := requireStringField(obj, "actor_id")
		if err != nil || actorID != owner.ownerID {
			return badf("memory_versions row actor %q does not match owner %q", actorID, owner.ownerID)
		}
		content, contentOK := obj["content"].(string)
		if !contentOK || strings.TrimSpace(content) == "" || len(content) > maxMemoryContentBytes {
			return badf("memory_versions row content must be a nonempty string")
		}
		contentEncoding, err := requireStringField(obj, "content_encoding")
		if err != nil {
			return badf("memory_versions row content_encoding is required")
		}
		contentHash, hashOK := obj["content_hash"].(string)
		if !hashOK || !validFactSHA256(contentHash) || memoryContentHash(content) != contentHash {
			return badf("memory_versions row content_hash does not match content")
		}
		if err := validateMemoryContentEncoding(content, contentEncoding); err != nil {
			return badf("memory_versions row %v", err)
		}
		versionCreatedAt, hasVersionCreatedAt, versionCreatedAtErr := importedOptionalTimestamp(obj, "created_at")
		if versionCreatedAtErr != nil ||
			(hasVersionCreatedAt && hasPrevious && !previousScope.createdAt.IsZero() &&
				versionCreatedAt.Before(previousScope.createdAt)) {
			return badf("memory_versions row created_at is invalid or precedes its previous version")
		}
		if !hasVersionCreatedAt {
			versionCreatedAt = new(time.Time)
		}
		kind, err := requireStringField(obj, "kind")
		if err != nil || !memoryKindPattern.MatchString(kind) {
			return badf("memory_versions row kind is invalid")
		}
		tags, err := importedNormalizedMemoryStrings(obj["tags"], 64, 128, "tag")
		if err != nil {
			return badf("memory_versions row %v", err)
		}
		links, err := importedNormalizedMemoryStrings(obj["links"], 256, 2048, "link")
		if err != nil {
			return badf("memory_versions row %v", err)
		}
		salience, salienceOK := importedFloat64(obj["salience"])
		if !salienceOK || salience < 0 || salience > 1 {
			return badf("memory_versions row salience must be between 0 and 1")
		}
		if salience == 0 {
			salience = 0 // canonicalize negative zero before hashing.
		}
		sensitive, sensitiveOK := obj["sensitive"].(bool)
		if !sensitiveOK {
			return badf("memory_versions row sensitive must be a boolean")
		}
		occurredFrom, _, occurredFromErr := importedOptionalTimestamp(obj, "occurred_from")
		occurredUntil, _, occurredUntilErr := importedOptionalTimestamp(obj, "occurred_until")
		if occurredFromErr != nil || occurredUntilErr != nil ||
			(occurredFrom != nil && occurredUntil != nil && occurredUntil.Before(*occurredFrom)) {
			return badf("memory_versions row occurrence range is invalid")
		}
		if occurredFrom != nil {
			value := occurredFrom.UTC()
			occurredFrom = &value
		}
		if occurredUntil != nil {
			value := occurredUntil.UTC()
			occurredUntil = &value
		}
		payloadDigest, err := memoryRequestHash(struct {
			Content         string     `json:"content"`
			ContentEncoding string     `json:"content_encoding"`
			Kind            string     `json:"kind"`
			Tags            []string   `json:"tags"`
			Links           []string   `json:"links"`
			Salience        float64    `json:"salience"`
			Sensitive       bool       `json:"sensitive"`
			OccurredFrom    *time.Time `json:"occurred_from"`
			OccurredUntil   *time.Time `json:"occurred_until"`
		}{
			Content: content, ContentEncoding: contentEncoding, Kind: kind,
			Tags: tags, Links: links, Salience: salience, Sensitive: sensitive,
			OccurredFrom: occurredFrom, OccurredUntil: occurredUntil,
		})
		if err != nil {
			return badf("memory_versions row payload digest is invalid")
		}
		requestHash, requestHashOK := obj["request_hash"].(string)
		if !requestHashOK || !validFactSHA256(requestHash) {
			return badf("memory_versions row request_hash is invalid")
		}
		if keyValue, ok := obj["idempotency_key"].(string); !ok || keyValue == "" {
			return badf("memory_versions row idempotency_key is invalid")
		}
		state, err := requireStringField(obj, "state")
		if err != nil {
			return badf("memory_versions row state is required")
		}
		operation, err := requireStringField(obj, "operation")
		if err != nil {
			return badf("memory_versions row operation is required")
		}
		priorState := ""
		if rawPriorState, present := obj["prior_state"]; present && rawPriorState != nil {
			var ok bool
			priorState, ok = rawPriorState.(string)
			if !ok || priorState == "" {
				return badf("memory_versions row prior_state is invalid")
			}
		}
		if err := validateImportedMemoryVersionTransition(
			version, changeSeq, payloadDigest, state, priorState, operation, previousScope,
		); err != nil {
			return badf("memory_versions row %v", err)
		}
		curationRunID, hasCurationRun := optionalStringField(obj, "curation_run_id")
		curationActionID, hasCurationAction := optionalStringField(obj, "curation_action_id")
		if hasCurationRun != hasCurationAction || (hasCurationRun &&
			(!validImportedGeneratedID(curationRunID, "mrun") ||
				!validImportedGeneratedID(curationActionID, "mact"))) {
			return badf("memory_versions row curation attribution is invalid")
		}
		if operation == "reverted" && !hasCurationRun {
			return badf("memory_versions reverted row requires curation attribution")
		}
		setID, setIDOK := obj["supersession_set_id"].(string)
		setRevision, revisionOK := importedNonnegativeInteger(obj["supersession_set_revision"])
		replacementCount, countOK := importedNonnegativeInteger(obj["supersession_replacement_count"])
		replacementDigest, digestOK := obj["supersession_replacement_digest"].(string)
		if !setIDOK || !revisionOK || !countOK || !digestOK {
			return badf("memory_versions row supersession receipt fields are invalid")
		}
		var supersessionSet memorySupersessionImportSet
		supersessionRequestHash := ""
		if operation == "superseded" {
			if state != MemoryStateSuperseded || !validImportedGeneratedID(setID, "mset") || setRevision < 1 ||
				replacementCount < 1 || replacementCount > maxMemorySupersessionReplacements ||
				!validFactSHA256(replacementDigest) {
				return badf("memory_versions row supersession receipt is invalid")
			}
			supersessionSet = memorySupersessionImportSet{id: setID, revision: setRevision}
			supersessionRequestHash = requestHash
		} else if setID != "" || setRevision != 0 || replacementCount != 0 || replacementDigest != "" {
			return badf("memory_versions non-superseded row carries supersession receipt")
		}
		expectsSupersessionRelations := operation == "superseded" ||
			(operation == "restored" && state == MemoryStateSuperseded)
		if operation != "superseded" && previousScope.supersessionReplacementCount > 0 {
			supersessionSet = previousScope.supersessionSet
			replacementCount = previousScope.supersessionReplacementCount
			replacementDigest = previousScope.supersessionReplacementDigest
			supersessionRequestHash = previousScope.supersessionRequestHash
		}
		if expectsSupersessionRelations &&
			(replacementCount < 1 || supersessionSet.id == "" || supersessionRequestHash == "") {
			return badf("memory_versions row superseded lifecycle lineage is missing")
		}
		ic.memoryVersions[key] = memoryVersionImportScope{
			owner: owner, previousVersion: previous, changeSeq: changeSeq,
			payloadDigest: payloadDigest, requestHash: requestHash,
			state: state, priorState: priorState, operation: operation,
			supersessionSet:               supersessionSet,
			supersessionReplacementCount:  replacementCount,
			supersessionReplacementDigest: replacementDigest,
			supersessionRequestHash:       supersessionRequestHash,
			expectsSupersessionRelations:  expectsSupersessionRelations,
			curationRunID:                 curationRunID,
			curationActionID:              curationActionID,
			contentHash:                   contentHash,
			createdAt:                     *versionCreatedAt,
		}
		memory.versionCount++
		if version > memory.maxVersion {
			memory.maxVersion = version
		}
		if changeSeq > memory.maxChangeSeq {
			memory.maxChangeSeq = changeSeq
		}
		ic.memories[memoryID] = memory
	case "memory_vector_profiles":
		owner, err := ic.importedMemoryOwner(obj)
		if err != nil {
			return badf("memory_vector_profiles row %v", err)
		}
		profileID, err := requireStringField(obj, "id")
		if err != nil || !validImportedGeneratedID(profileID, "mvp") {
			return badf("memory_vector_profiles row id is invalid")
		}
		if _, exists := ic.memoryVectorProfiles[profileID]; exists {
			return badf("memory_vector_profiles row duplicates id %q", profileID)
		}
		ownerProfileCount := 0
		for _, existing := range ic.memoryVectorProfiles {
			if existing.owner == owner {
				ownerProfileCount++
			}
		}
		if ownerProfileCount >= maxMemoryVectorProfiles {
			return badf("memory_vector_profiles owner exceeds limit %d", maxMemoryVectorProfiles)
		}
		createdBy, err := requireStringField(obj, "created_by_agent_id")
		if err != nil || createdBy != owner.ownerID {
			return badf("memory_vector_profiles row creator does not match owner")
		}
		dimensions, ok := importedPositiveInteger(obj["dimensions"])
		if !ok || dimensions > maxMemoryVectorDimensions {
			return badf("memory_vector_profiles row dimensions are invalid")
		}
		provider, providerErr := requireStringField(obj, "provider")
		model, modelErr := requireStringField(obj, "model")
		recipe, recipeErr := requireStringField(obj, "recipe")
		recipeVersion, recipeVersionErr := requireStringField(obj, "recipe_version")
		metric, metricErr := requireStringField(obj, "distance_metric")
		normalization, normalizationErr := requireStringField(obj, "normalization")
		if providerErr != nil || modelErr != nil || recipeErr != nil || recipeVersionErr != nil || metricErr != nil || normalizationErr != nil {
			return badf("memory_vector_profiles row contract fields are required")
		}
		normalized, contractHash, err := normalizeMemoryVectorProfileInput(CreateMemoryVectorProfileInput{
			Provider: provider, Model: model, Recipe: recipe,
			RecipeVersion: recipeVersion, Dimensions: int(dimensions),
			DistanceMetric: metric, Normalization: normalization,
		})
		storedContractHash, _ := obj["contract_hash"].(string)
		if err != nil || normalized.Provider != provider || normalized.Model != model ||
			normalized.Recipe != recipe || normalized.RecipeVersion != recipeVersion ||
			normalized.DistanceMetric != metric || normalized.Normalization != normalization ||
			contractHash != storedContractHash {
			return badf("memory_vector_profiles row contract is not canonical")
		}
		for _, existing := range ic.memoryVectorProfiles {
			if existing.owner == owner && existing.profile.ContractHash == contractHash {
				return badf("memory_vector_profiles row duplicates an owner contract")
			}
		}
		createdAt, present, timestampErr := importedOptionalTimestamp(obj, "created_at")
		if timestampErr != nil || !present || ic.exportedAt.IsZero() || createdAt.After(ic.exportedAt) {
			return badf("memory_vector_profiles row created_at is invalid")
		}
		profile := MemoryVectorProfile{ID: profileID, Provider: provider, Model: model,
			Recipe: recipe, RecipeVersion: recipeVersion, Dimensions: int(dimensions),
			DistanceMetric: metric, Normalization: normalization,
			ContractHash: contractHash, CreatedAt: *createdAt}
		ic.memoryVectorProfiles[profileID] = memoryVectorProfileImportScope{owner: owner, profile: profile}
	case "memory_vectors":
		owner, err := ic.importedMemoryOwner(obj)
		if err != nil {
			return badf("memory_vectors row %v", err)
		}
		profileID, profileErr := requireStringField(obj, "profile_id")
		profileScope, profileOK := ic.memoryVectorProfiles[profileID]
		memoryID, memoryErr := requireStringField(obj, "memory_id")
		version, versionOK := importedPositiveInteger(obj["memory_version"])
		versionScope, memoryVersionOK := ic.memoryVersions[memoryVersionImportKey{memoryID: memoryID, version: version}]
		if profileErr != nil || !profileOK || profileScope.owner != owner || memoryErr != nil ||
			!versionOK || !memoryVersionOK || versionScope.owner != owner {
			return badf("memory_vectors row profile or memory version is outside its owner scope")
		}
		key := memoryVectorImportKey{profileID: profileID, memoryID: memoryID, version: version}
		if ic.memoryVectors[key] {
			return badf("memory_vectors row duplicates an exact vector key")
		}
		createdBy, err := requireStringField(obj, "created_by_agent_id")
		if err != nil || createdBy != owner.ownerID {
			return badf("memory_vectors row creator does not match owner")
		}
		contentHash, _ := obj["content_hash"].(string)
		if contentHash != versionScope.contentHash {
			return badf("memory_vectors row content_hash does not match the exact memory version")
		}
		rawVector, ok := obj["vector"].([]any)
		if !ok {
			return badf("memory_vectors row vector must be an array")
		}
		vector := make([]float64, len(rawVector))
		for i, raw := range rawVector {
			component, valid := importedFloat64(raw)
			if !valid {
				return badf("memory_vectors row component %d is invalid", i)
			}
			vector[i] = component
		}
		_, vectorHash, err := normalizeMemoryVector(vector, profileScope.profile)
		storedVectorHash, _ := obj["vector_hash"].(string)
		if err != nil || vectorHash != storedVectorHash {
			return badf("memory_vectors row vector violates its profile or hash")
		}
		vectorCreatedAt, present, timestampErr := importedOptionalTimestamp(obj, "created_at")
		if timestampErr != nil || !present || ic.exportedAt.IsZero() ||
			vectorCreatedAt.After(ic.exportedAt) || versionScope.createdAt.IsZero() ||
			vectorCreatedAt.Before(profileScope.profile.CreatedAt) ||
			vectorCreatedAt.Before(versionScope.createdAt) {
			return badf("memory_vectors row created_at is invalid")
		}
		ic.memoryVectors[key] = true
	case "memory_evidence":
		owner, err := ic.importedMemoryOwner(obj)
		if err != nil {
			return badf("memory_evidence row %v", err)
		}
		memoryID, err := requireStringField(obj, "memory_id")
		version, versionOK := importedPositiveInteger(obj["target_version"])
		target := memoryVersionImportKey{memoryID: memoryID, version: version}
		versionScope, targetOK := ic.memoryVersions[target]
		if err != nil || !versionOK || !targetOK || versionScope.owner != owner {
			return badf("memory_evidence row target %q version %d is outside its owner scope", memoryID, version)
		}
		id, err := requireStringField(obj, "id")
		if err != nil || !strings.HasPrefix(id, "mev_") {
			return badf("memory_evidence row id is invalid")
		}
		if _, exists := ic.memoryEvidence[id]; exists {
			return badf("memory_evidence row duplicates id %q", id)
		}
		actorID, err := requireStringField(obj, "actor_id")
		if err != nil || actorID != owner.ownerID {
			return badf("memory_evidence row actor %q does not match owner %q", actorID, owner.ownerID)
		}
		changeSeq, ok := importedPositiveInteger(obj["evidence_change_seq"])
		if !ok {
			return badf("memory_evidence row evidence_change_seq must be a positive integer")
		}
		if err := ic.validateImportedMemoryEvidence(obj, target, owner, changeSeq); err != nil {
			return badf("memory_evidence row %v", err)
		}
		state, _ := requireStringField(obj, "resolution_state")
		ic.memoryEvidence[id] = memoryEvidenceImportScope{
			target: target, state: state, changeSeq: changeSeq,
		}
		memory := ic.memories[memoryID]
		if changeSeq > memory.maxChangeSeq {
			memory.maxChangeSeq = changeSeq
		}
		ic.memories[memoryID] = memory
	case "memory_relations":
		owner, err := ic.importedMemoryOwner(obj)
		if err != nil {
			return badf("memory_relations row %v", err)
		}
		relationID, err := requireStringField(obj, "id")
		if err != nil || !validImportedGeneratedID(relationID, "mrel") {
			return badf("memory_relations row id is invalid")
		}
		fromID, fromErr := requireStringField(obj, "from_memory_id")
		fromVersion, fromVersionOK := importedPositiveInteger(obj["from_version"])
		toID, toErr := requireStringField(obj, "to_memory_id")
		toVersion, toVersionOK := importedPositiveInteger(obj["to_version"])
		fromKey := memoryVersionImportKey{memoryID: fromID, version: fromVersion}
		toKey := memoryVersionImportKey{memoryID: toID, version: toVersion}
		from := ic.memoryVersions[fromKey]
		to := ic.memoryVersions[toKey]
		if fromErr != nil || toErr != nil || !fromVersionOK || !toVersionOK ||
			from.owner != owner || to.owner != owner {
			return badf("memory_relations row endpoints are outside its owner scope")
		}
		relationType, err := requireStringField(obj, "relation_type")
		if err != nil {
			return badf("memory_relations row relation_type is required")
		}
		curationRunID, hasCurationRun := optionalStringField(obj, "curation_run_id")
		curationActionID, hasCurationAction := optionalStringField(obj, "curation_action_id")
		if hasCurationRun != hasCurationAction || (hasCurationRun &&
			(!validImportedGeneratedID(curationRunID, "mrun") ||
				!validImportedGeneratedID(curationActionID, "mact"))) {
			return badf("memory_relations row curation attribution is invalid")
		}
		revertedByRunID, hasRevertedRun := optionalStringField(obj, "reverted_by_run_id")
		revertedByActionID, hasRevertedAction := optionalStringField(obj, "reverted_by_action_id")
		if hasRevertedRun != hasRevertedAction || (hasRevertedRun &&
			(!validImportedGeneratedID(revertedByRunID, "mrun") ||
				!validImportedGeneratedID(revertedByActionID, "mact"))) {
			return badf("memory_relations row revert attribution is invalid")
		}
		_, reverted, err := importedOptionalTimestamp(obj, "reverted_at")
		if err != nil {
			return badf("memory_relations row %v", err)
		}
		if hasRevertedRun && !reverted {
			return badf("memory_relations row revert attribution requires reverted_at")
		}
		relationScope := memoryRelationCurationImportScope{
			id: relationID, owner: owner, relationType: relationType,
			from:  MemoryVersionReference{MemoryID: fromID, Version: fromVersion},
			to:    MemoryVersionReference{MemoryID: toID, Version: toVersion},
			runID: curationRunID, actionID: curationActionID,
			revertedByRunID: revertedByRunID, revertedByActionID: revertedByActionID,
		}
		if _, duplicate := ic.memoryRelations[relationID]; duplicate {
			return badf("memory_relations row duplicates id %q", relationID)
		}
		setID, hasSetID := optionalStringField(obj, "supersession_set_id")
		setRevision, hasSetRevision, validSetRevision := importedOptionalPositiveInteger(obj, "supersession_set_revision")
		switch relationType {
		case "derived_from", "summarizes", "merged_from", "split_from", "conflicts_with":
			if !hasCurationRun || hasSetID || hasSetRevision {
				return badf("memory_relations generic lineage shape is invalid")
			}
		case "supersedes":
			// Validated below against the immutable supersession receipt.
		default:
			return badf("memory_relations row relation_type %q is invalid", relationType)
		}
		if relationType != "supersedes" {
			ic.memoryRelations[relationID] = relationScope
			ic.memoryRelationCurations = append(ic.memoryRelationCurations, relationScope)
			break
		}
		if !hasSetID || !validImportedGeneratedID(setID, "mset") ||
			!hasSetRevision || !validSetRevision {
			return badf("memory_relations supersedes row requires a valid supersession set id and revision")
		}
		set := memorySupersessionImportSet{id: setID, revision: setRevision}
		if !to.expectsSupersessionRelations || to.supersessionReplacementCount < 1 ||
			to.supersessionSet != set {
			return badf("memory_relations supersedes row does not match the target version lineage")
		}
		if fromVersion != 1 || from.previousVersion != 0 ||
			from.operation != "added" || from.state != MemoryStateActive {
			return badf("memory_relations supersedes replacement must be its initial added active version")
		}
		if from.requestHash != to.supersessionRequestHash {
			return badf("memory_relations supersedes replacement request_hash does not match its source operation")
		}
		ic.memoryRelations[relationID] = relationScope
		if hasCurationRun || hasRevertedRun {
			ic.memoryRelationCurations = append(ic.memoryRelationCurations, relationScope)
		}
		to.supersessionMembers = append(to.supersessionMembers,
			MemoryVersionReference{MemoryID: fromID, Version: fromVersion})
		if !reverted {
			to.activeSupersessionMemberCount++
		}
		ic.memoryVersions[toKey] = to
		if reverted {
			break
		}
		if active, exists := ic.memoryActiveSupersessionSets[toKey]; exists && active != set {
			return badf("memory version %q version %d belongs to multiple active supersession sets", toID, toVersion)
		}
		ic.memoryActiveSupersessionSets[toKey] = set
	case "memory_deleted_references":
		owner, err := ic.importedMemoryOwner(obj)
		if err != nil {
			return badf("memory_deleted_references row %v", err)
		}
		referenceID, err := requireStringField(obj, "id")
		if err != nil || !validImportedGeneratedID(referenceID, "mdr") {
			return badf("memory_deleted_references row id is invalid")
		}
		memoryID, err := requireStringField(obj, "deleted_memory_id")
		memory, exists := ic.memories[memoryID]
		if err != nil || !exists || !memory.deleted || memory.owner != owner {
			return badf("memory_deleted_references row references non-deleted memory %q", memoryID)
		}
		kind, err := requireStringField(obj, "former_reference_kind")
		if err != nil || !validImportedMemoryRetryShieldKind(kind) {
			return badf("memory_deleted_references row retry shield kind is invalid")
		}
		hash, present := optionalStringField(obj, "related_resource_id")
		if !present || !validFactSHA256(hash) {
			return badf("memory_deleted_references retry shield hash is invalid")
		}
		reason, err := requireStringField(obj, "reason_code")
		if err != nil || reason != "permanent_delete" {
			return badf("memory_deleted_references retry shield reason is invalid")
		}
		for _, field := range []string{"curation_run_id", "curation_request_id"} {
			if value, supplied := obj[field]; supplied && value != nil {
				return badf("memory_deleted_references retry shield %s must be null", field)
			}
		}
		ic.memoryDeletedRetryShields[memoryID] = append(
			ic.memoryDeletedRetryShields[memoryID],
			memoryDeleteRetryShield{Kind: kind, Hash: hash},
		)
	case "memory_curation_lanes":
		owner, err := ic.importedMemoryOwner(obj)
		if err != nil {
			return badf("memory_curation_lanes row %v", err)
		}
		requestGeneration, ok := importedGeneration(obj["request_generation"], true)
		fencingGeneration, fenceOK := importedGeneration(obj["fencing_generation"], true)
		if !ok || !fenceOK {
			return badf("memory_curation_lanes row generations are invalid")
		}
		if active, present := optionalStringField(obj, "active_run_id"); present || active != "" {
			return badf("memory_curation_lanes row retained an active lease during import")
		}
		existing, preRecorded := ic.memoryCurationLanes[owner]
		if existing.validated || preRecorded && !existing.normalizedActive {
			return badf("memory_curation_lanes row duplicates an owner lane")
		}
		if existing.normalizedActive &&
			(existing.requestGeneration != requestGeneration || existing.fencingGeneration != fencingGeneration) {
			return badf("memory_curation_lanes normalized generations changed unexpectedly")
		}
		existing.owner = owner
		existing.requestGeneration = requestGeneration
		existing.fencingGeneration = fencingGeneration
		existing.validated = true
		ic.memoryCurationLanes[owner] = existing
	case "memory_curation_cursors":
		owner, err := ic.importedMemoryOwner(obj)
		if err != nil {
			return badf("memory_curation_cursors row %v", err)
		}
		if _, ok := ic.memoryCurationLanes[owner]; !ok {
			return badf("memory_curation_cursors row has no imported owner lane")
		}
		sourceKind, err := requireStringField(obj, "source_kind")
		streamID, streamErr := requireStringField(obj, "source_stream_id")
		position, positionOK := importedGeneration(obj["position"], true)
		if err != nil || streamErr != nil || !positionOK || len(streamID) > 512 {
			return badf("memory_curation_cursors row source or position is invalid")
		}
		switch sourceKind {
		case "memory":
			if !validImportedMemoryCurationFilteredStreamID(streamID) {
				return badf("memory curation cursor stream is invalid")
			}
		case "evidence":
			if !validImportedMemoryCurationFilteredStreamID(streamID) {
				return badf("evidence curation cursor stream is invalid")
			}
		case "transcript":
			transcript, exists := ic.transcripts[streamID]
			if !exists || transcript.realmID != owner.realmID || transcript.ownerAgentID != owner.ownerID {
				return badf("transcript curation cursor stream is outside its owner scope")
			}
		default:
			return badf("memory_curation_cursors row source_kind is invalid")
		}
		key := memoryCurationCursorImportKey{owner: owner, sourceKind: sourceKind, streamID: streamID}
		if _, duplicate := ic.memoryCurationCursors[key]; duplicate {
			return badf("memory_curation_cursors row duplicates a source stream")
		}
		ic.memoryCurationCursors[key] = position
	case "memory_curation_requests":
		owner, err := ic.importedMemoryOwner(obj)
		if err != nil {
			return badf("memory_curation_requests row %v", err)
		}
		lane, exists := ic.memoryCurationLanes[owner]
		if !exists {
			return badf("memory_curation_requests row has no imported owner lane")
		}
		requestID, err := requireStringField(obj, "id")
		generation, generationOK := importedGeneration(obj["request_generation"], false)
		if err != nil || !validImportedGeneratedID(requestID, "mcrq") ||
			!generationOK || generation > lane.requestGeneration {
			return badf("memory_curation_requests row id or generation is invalid")
		}
		if err := validateImportedCurationActor(obj, owner); err != nil {
			return badf("memory_curation_requests row %v", err)
		}
		if err := validateImportedCurationRequestContent(obj); err != nil {
			return badf("memory_curation_requests row %v", err)
		}
		state, _ := requireStringField(obj, "state")
		claimedRunID, _ := optionalStringField(obj, "claimed_run_id")
		replayRunID, _ := optionalStringField(obj, "replay_run_id")
		existing := ic.memoryCurationRequests[requestID]
		if existing.validated {
			return badf("memory_curation_requests row duplicates id %q", requestID)
		}
		existing.owner = owner
		existing.requestGeneration = generation
		existing.state = state
		existing.replayRunID = replayRunID
		if !existing.normalizedClaimed {
			existing.claimedRunID = claimedRunID
		}
		existing.validated = true
		ic.memoryCurationRequests[requestID] = existing
	case "memory_curation_runs":
		owner, err := ic.importedMemoryOwner(obj)
		if err != nil {
			return badf("memory_curation_runs row %v", err)
		}
		lane, exists := ic.memoryCurationLanes[owner]
		if !exists {
			return badf("memory_curation_runs row has no imported owner lane")
		}
		runID, err := requireStringField(obj, "id")
		requestID, requestErr := requireStringField(obj, "request_id")
		request, requestExists := ic.memoryCurationRequests[requestID]
		generation, generationOK := importedGeneration(obj["request_generation"], false)
		fence, fenceOK := importedGeneration(obj["fencing_generation"], false)
		if err != nil || requestErr != nil || !validImportedGeneratedID(runID, "mrun") ||
			!requestExists || request.owner != owner || !generationOK ||
			generation > request.requestGeneration || !fenceOK || fence > lane.fencingGeneration {
			return badf("memory_curation_runs row identity, request, or generation is invalid")
		}
		if err := validateImportedCurationActor(obj, owner); err != nil {
			return badf("memory_curation_runs row %v", err)
		}
		counts := make([]int64, 5)
		for index, field := range []string{"input_count", "memory_input_count", "evidence_input_count", "transcript_input_count", "cursor_input_count"} {
			value, ok := importedNonnegativeInteger(obj[field])
			if !ok || value > 10000 {
				return badf("memory_curation_runs row %s is invalid", field)
			}
			counts[index] = value
		}
		if counts[0] != counts[1]+counts[2]+counts[3]+counts[4] {
			return badf("memory_curation_runs row input counts do not sum")
		}
		planRevision, ok := importedNonnegativeInteger(obj["plan_revision"])
		if !ok || planRevision > math.MaxInt32 {
			return badf("memory_curation_runs row plan_revision is invalid")
		}
		planSchema, schemaOK := obj["plan_schema"].(string)
		planHash, hashOK := obj["plan_hash"].(string)
		_, scrubbed, scrubErr := importedOptionalTimestamp(obj, "scrubbed_at")
		if !schemaOK || !hashOK || scrubErr != nil {
			return badf("memory_curation_runs row plan or scrub metadata is invalid")
		}
		switch {
		case planRevision == 0 && (planSchema != "" || planHash != ""):
			return badf("memory_curation_runs unplanned row carries plan metadata")
		case planRevision > 0 && planSchema != MemoryCurationPlanSchemaV1:
			return badf("memory_curation_runs row plan schema is invalid")
		case planRevision > 0 && !scrubbed && !isSHA256Hex(planHash):
			return badf("memory_curation_runs row plan hash is invalid")
		case scrubbed && planHash != "":
			return badf("memory_curation_runs scrubbed row retains a plan hash")
		}
		state, _ := requireStringField(obj, "state")
		existing := ic.memoryCurationRuns[runID]
		if existing.validated {
			return badf("memory_curation_runs row duplicates id %q", runID)
		}
		existing.owner = owner
		existing.requestID = requestID
		existing.requestGeneration = generation
		existing.fencingGeneration = fence
		if !existing.normalizedInterrupted {
			existing.state = state
		}
		existing.planRevision = planRevision
		existing.planSchema = planSchema
		existing.planHash = planHash
		existing.scrubbed = scrubbed
		existing.inputCount = counts[0]
		existing.memoryInputCount = counts[1]
		existing.evidenceInputCount = counts[2]
		existing.transcriptInputCount = counts[3]
		existing.cursorInputCount = counts[4]
		existing.observedByKind = map[string]int64{}
		existing.validated = true
		ic.memoryCurationRuns[runID] = existing
	case "memory_curation_run_inputs":
		owner, err := ic.importedMemoryOwner(obj)
		if err != nil {
			return badf("memory_curation_run_inputs row %v", err)
		}
		runID, err := requireStringField(obj, "run_id")
		run, exists := ic.memoryCurationRuns[runID]
		ordinal, ordinalOK := importedGeneration(obj["ordinal"], false)
		kind, kindErr := requireStringField(obj, "input_kind")
		if err != nil || !exists || run.owner != owner || !ordinalOK ||
			ordinal != run.observedInputs+1 || kindErr != nil {
			return badf("memory_curation_run_inputs row run, ordinal, or kind is invalid")
		}
		if err := ic.validateImportedCurationRunInput(obj, run, kind); err != nil {
			return badf("memory_curation_run_inputs row %v", err)
		}
		run.observedInputs++
		run.observedByKind[kind]++
		ic.memoryCurationRuns[runID] = run
	case "memory_curation_actions":
		owner, err := ic.importedMemoryOwner(obj)
		if err != nil {
			return badf("memory_curation_actions row %v", err)
		}
		actionID, err := requireStringField(obj, "id")
		runID, runErr := requireStringField(obj, "run_id")
		run, exists := ic.memoryCurationRuns[runID]
		planRevision, revisionOK := importedPositiveInteger(obj["plan_revision"])
		ordinal, ordinalOK := importedGeneration(obj["ordinal"], false)
		state, stateErr := requireStringField(obj, "state")
		primitive, primitiveErr := requireStringField(obj, "primitive")
		if err != nil || runErr != nil || !validImportedGeneratedID(actionID, "mact") ||
			!exists || run.owner != owner || !revisionOK || planRevision != run.planRevision ||
			!ordinalOK || ordinal != run.observedActions+1 ||
			stateErr != nil || primitiveErr != nil || !validCurationPrimitive(primitive) {
			return badf("memory_curation_actions row identity, run, or plan revision is invalid")
		}
		if _, duplicate := ic.memoryCurationActions[actionID]; duplicate {
			return badf("memory_curation_actions row duplicates id %q", actionID)
		}
		action, err := decodeImportedMemoryCurationAction(
			obj, owner, run, actionID, runID, ordinal, planRevision, state, primitive,
		)
		if err != nil {
			return badf("memory_curation_actions row %v", err)
		}
		ic.memoryCurationActions[actionID] = action
		run.observedActions++
		ic.memoryCurationRuns[runID] = run
	case "memory_curation_mutations":
		owner, err := ic.importedMemoryOwner(obj)
		if err != nil {
			return badf("memory_curation_mutations row %v", err)
		}
		mutationID, err := requireStringField(obj, "id")
		requestID, requestErr := requireStringField(obj, "request_id")
		request, requestExists := ic.memoryCurationRequests[requestID]
		operation, operationErr := requireStringField(obj, "operation")
		if err != nil || !validImportedGeneratedID(mutationID, "mcmu") ||
			requestErr != nil || !requestExists || request.owner != owner || operationErr != nil ||
			!validCurationMutationOperation(operation) {
			return badf("memory_curation_mutations row identity, request, or operation is invalid")
		}
		if err := validateImportedCurationActor(obj, owner); err != nil {
			return badf("memory_curation_mutations row %v", err)
		}
		generation, generationOK := importedGeneration(obj["request_generation"], false)
		fence, fenceOK := importedGeneration(obj["fencing_generation"], true)
		if !generationOK || generation > request.requestGeneration || !fenceOK {
			return badf("memory_curation_mutations row generations are invalid")
		}
		runID, hasRun := optionalStringField(obj, "run_id")
		if operation == "request" {
			if hasRun || fence != 0 {
				return badf("memory_curation_mutations request receipt carries a run fence")
			}
		} else {
			run, exists := ic.memoryCurationRuns[runID]
			if !hasRun || !exists || run.requestID != requestID || run.owner != owner ||
				fence > run.fencingGeneration {
				return badf("memory_curation_mutations row run is outside its request scope")
			}
		}
		if _, duplicate := ic.memoryCurationMutations[mutationID]; duplicate {
			return badf("memory_curation_mutations row duplicates id %q", mutationID)
		}
		ic.memoryCurationMutations[mutationID] = true
	default:
		return badf("table %q is not importable", table)
	}
	return nil
}

func validImportedMemoryCurationFilteredStreamID(value string) bool {
	const prefix = "filtered_v1_"
	return strings.HasPrefix(value, prefix) && isSHA256Hex(strings.TrimPrefix(value, prefix))
}

func (ic *importCtx) importedMemoryOwner(obj map[string]any) (memoryOwnerImportKey, error) {
	realmID, err := requireStringField(obj, "realm_id")
	if err != nil || !ic.realms[realmID] {
		return memoryOwnerImportKey{}, fmt.Errorf("references realm %q not present in this archive", realmID)
	}
	ownerKind, err := requireStringField(obj, "owner_kind")
	if err != nil || ownerKind != "agent" {
		return memoryOwnerImportKey{}, fmt.Errorf("owner_kind must be agent")
	}
	ownerID, err := requireStringField(obj, "owner_id")
	if err != nil || !ic.agents[ownerID] || ic.agentRealms[ownerID] != realmID {
		return memoryOwnerImportKey{}, fmt.Errorf("owner %q is outside realm %q", ownerID, realmID)
	}
	return memoryOwnerImportKey{
		realmID: realmID, ownerKind: ownerKind, ownerID: ownerID,
	}, nil
}

func validateImportedCurationActor(obj map[string]any, owner memoryOwnerImportKey) error {
	actorKind, err := requireStringField(obj, "actor_kind")
	actorID, actorErr := requireStringField(obj, "actor_id")
	if err != nil || actorErr != nil || actorKind != "agent" || actorID != owner.ownerID {
		return fmt.Errorf("actor must be the owner agent")
	}
	key, err := requireStringField(obj, "idempotency_key")
	hash, hashErr := requireStringField(obj, "request_hash")
	if err != nil || len(key) > 512 || hashErr != nil || !validFactSHA256(hash) {
		return fmt.Errorf("idempotency receipt is invalid")
	}
	return nil
}

func validateImportedCurationRequestContent(obj map[string]any) error {
	scope, ok := obj["scope"].(map[string]any)
	if !ok {
		return fmt.Errorf("scope must be an object")
	}
	encoded, err := json.Marshal(scope)
	if err != nil || len(encoded) > 32768 {
		return fmt.Errorf("scope exceeds the size limit")
	}
	var decoded MemoryCurationScope
	if err := decodeImportedCurationJSON(scope, &decoded); err != nil {
		return fmt.Errorf("scope is not a strict curation scope")
	}
	normalized, err := normalizeMemoryCurationScope(decoded)
	if err != nil || !sameCanonicalMemoryCurationJSON(decoded, normalized) {
		return fmt.Errorf("scope is not canonical")
	}
	coalescingKey, err := requireStringField(obj, "coalescing_key")
	triggerReason, triggerErr := requireStringField(obj, "trigger_reason")
	if err != nil || len(coalescingKey) > 256 || triggerErr != nil ||
		!memoryKindPattern.MatchString(triggerReason) {
		return fmt.Errorf("coalescing key or trigger reason is invalid")
	}
	state, err := requireStringField(obj, "state")
	if err != nil {
		return fmt.Errorf("state is required")
	}
	switch state {
	case "queued", "retry_wait", "fulfilled", "cancelled", "dead_letter":
	default:
		// A claimed request must have been normalized to queued before this
		// boundary; retaining it would carry an old-cell lease.
		return fmt.Errorf("state %q is invalid after lease normalization", state)
	}
	attempts, attemptsOK := importedNonnegativeInteger(obj["attempt_count"])
	maxAttempts, maxOK := importedPositiveInteger(obj["max_attempts"])
	if !attemptsOK || !maxOK || maxAttempts > 100 || attempts > maxAttempts {
		return fmt.Errorf("attempt counts are invalid")
	}
	if _, ok := obj["read_only_replay"].(bool); !ok {
		return fmt.Errorf("read_only_replay must be a boolean")
	}
	if coalescingKey == automaticMemoryCurationCoalescingKey {
		defaultScope, scopeErr := normalizeMemoryCurationScope(MemoryCurationScope{})
		priority, priorityOK := importedNonnegativeInteger(obj["priority"])
		idempotencyKey, keyOK := obj["idempotency_key"].(string)
		readOnlyReplay, replayOK := obj["read_only_replay"].(bool)
		_, hasReplayRun := optionalStringField(obj, "replay_run_id")
		if scopeErr != nil || !sameCanonicalMemoryCurationJSON(decoded, defaultScope) ||
			!priorityOK || priority != 0 || !keyOK ||
			!strings.HasPrefix(idempotencyKey, "automatic:") || !replayOK ||
			readOnlyReplay || hasReplayRun {
			return fmt.Errorf("reserved automatic request shape is invalid")
		}
	}
	for _, field := range []string{"due_at", "created_at", "updated_at"} {
		if _, present, err := importedOptionalTimestamp(obj, field); err != nil || !present {
			return fmt.Errorf("%s must be an RFC3339 timestamp", field)
		}
	}
	return nil
}

func (ic *importCtx) validateImportedCurationRunInput(
	obj map[string]any,
	run memoryCurationRunImportScope,
	kind string,
) error {
	orderKey, err := requireStringField(obj, "order_key")
	if err != nil || len(orderKey) > 512 {
		return fmt.Errorf("order_key is invalid")
	}
	switch kind {
	case "memory":
		memoryID, err := requireStringField(obj, "memory_id")
		version, ok := importedPositiveInteger(obj["memory_version"])
		scope, exists := ic.memoryVersions[memoryVersionImportKey{memoryID: memoryID, version: version}]
		if err != nil || !ok || !exists || scope.owner != run.owner {
			return fmt.Errorf("memory input is outside its owner scope")
		}
	case "evidence":
		evidenceID, err := requireStringField(obj, "evidence_id")
		evidence, exists := ic.memoryEvidence[evidenceID]
		version := ic.memoryVersions[evidence.target]
		if err != nil || !exists || version.owner != run.owner {
			return fmt.Errorf("evidence input is outside its owner scope")
		}
	case "transcript":
		transcriptID, err := requireStringField(obj, "transcript_id")
		transcript, exists := ic.transcripts[transcriptID]
		from, fromOK := importedGeneration(obj["sequence_from"], false)
		until, untilOK := importedGeneration(obj["sequence_until"], false)
		if err != nil || !exists || transcript.realmID != run.owner.realmID ||
			transcript.ownerAgentID != run.owner.ownerID || !fromOK || !untilOK ||
			until < from || until >= transcript.nextSequence {
			return fmt.Errorf("transcript input range is outside its owner stream")
		}
	case "cursor":
		sourceKind, err := requireStringField(obj, "cursor_source_kind")
		streamID, streamErr := requireStringField(obj, "cursor_stream_id")
		expected, expectedOK := importedGeneration(obj["cursor_expected_prior"], true)
		upper, upperOK := importedGeneration(obj["cursor_upper"], true)
		position, exists := ic.memoryCurationCursors[memoryCurationCursorImportKey{
			owner: run.owner, sourceKind: sourceKind, streamID: streamID,
		}]
		if err != nil || streamErr != nil || !expectedOK || !upperOK ||
			upper < expected || !exists || position < expected {
			return fmt.Errorf("cursor input interval is invalid")
		}
	default:
		return fmt.Errorf("input_kind %q is invalid", kind)
	}
	return nil
}

func validCurationPrimitive(value string) bool {
	switch value {
	case "create", "replace", "supersede", "relate", "propose_fact":
		return true
	default:
		return false
	}
}

func decodeImportedMemoryCurationAction(
	obj map[string]any,
	owner memoryOwnerImportKey,
	run memoryCurationRunImportScope,
	actionID, runID string,
	ordinal, planRevision int64,
	state, primitive string,
) (memoryCurationActionImportScope, error) {
	out := memoryCurationActionImportScope{
		id: actionID, owner: owner, runID: runID, ordinal: ordinal,
		planRevision: planRevision, state: state, primitive: primitive,
	}
	_, scrubbed, err := importedOptionalTimestamp(obj, "scrubbed_at")
	if err != nil {
		return out, err
	}
	out.scrubbed = scrubbed
	if scrubbed {
		if !run.scrubbed || !importedEmptyCurationJSON(obj["input_refs"], []any{}) ||
			!importedEmptyCurationJSON(obj["expected_heads"], []any{}) ||
			!importedEmptyCurationJSON(obj["proposed_payload"], map[string]any{}) ||
			!importedEmptyCurationJSON(obj["validation_result"], map[string]any{}) ||
			!importedEmptyCurationJSON(obj["applied_result"], map[string]any{}) ||
			!importedEmptyCurationJSON(obj["rollback_result"], map[string]any{}) {
			return out, fmt.Errorf("scrubbed action retains value-bearing JSON")
		}
		actionHash, ok := obj["action_hash"].(string)
		localRef, localOK := obj["local_ref"].(string)
		if !ok || actionHash != "" || !localOK || localRef != "" {
			return out, fmt.Errorf("scrubbed action retains its hash or local reference")
		}
		return out, nil
	}
	if run.scrubbed {
		return out, fmt.Errorf("unscrubbed action belongs to a scrubbed run")
	}
	if err := decodeImportedCurationJSON(obj["proposed_payload"], &out.action); err != nil {
		return out, fmt.Errorf("proposed_payload is invalid: %w", err)
	}
	if out.action.Ordinal != ordinal || out.action.Operation != primitive ||
		!validMemoryCurationStoredActionEnvelope(out.action) {
		return out, fmt.Errorf("proposed_payload does not match its action envelope")
	}
	localRef, ok := obj["local_ref"].(string)
	if !ok || (out.action.Create != nil && localRef != out.action.Create.LocalRef) ||
		(out.action.Create == nil && localRef != "") {
		return out, fmt.Errorf("local_ref does not match proposed_payload")
	}
	if err := decodeImportedCurationJSON(obj["input_refs"], &out.inputRefs); err != nil {
		return out, fmt.Errorf("input_refs are invalid: %w", err)
	}
	normalizedRefs, err := normalizeMemoryCurationActionInputRefs(out.inputRefs)
	if err != nil || !sameCanonicalMemoryCurationJSON(out.inputRefs, normalizedRefs) {
		return out, fmt.Errorf("input_refs are not canonical")
	}
	if err := decodeImportedCurationJSON(obj["expected_heads"], &out.expectedHeads); err != nil {
		return out, fmt.Errorf("expected_heads are invalid: %w", err)
	}
	normalizedHeads, err := normalizeMemoryCurationExpectedHeads(out.expectedHeads)
	if err != nil || !sameCanonicalMemoryCurationJSON(out.expectedHeads, normalizedHeads) {
		return out, fmt.Errorf("expected_heads are not canonical")
	}
	var validation struct {
		Authorized        bool `json:"authorized"`
		InputRefCount     int  `json:"input_ref_count"`
		ExpectedHeadCount int  `json:"expected_head_count"`
	}
	if err := decodeImportedCurationJSON(obj["validation_result"], &validation); err != nil ||
		!validation.Authorized || validation.InputRefCount != len(out.inputRefs) ||
		validation.ExpectedHeadCount != len(out.expectedHeads) {
		return out, fmt.Errorf("validation_result does not match authorization indexes")
	}
	canonical, err := canonicalMemoryCurationJSON(out.action)
	if err != nil {
		return out, fmt.Errorf("proposed_payload is not canonicalizable")
	}
	sum := sha256.Sum256(canonical)
	out.actionHash, ok = obj["action_hash"].(string)
	if !ok || out.actionHash != hex.EncodeToString(sum[:]) {
		return out, fmt.Errorf("action_hash does not match proposed_payload")
	}

	switch state {
	case "applied", "reverted":
		var result MemoryCurationActionApplyResult
		if err := decodeImportedCurationJSON(obj["applied_result"], &result); err != nil ||
			result.ActionID != actionID || result.Ordinal != ordinal || result.Operation != primitive {
			return out, fmt.Errorf("applied_result does not match its action")
		}
		out.appliedResult = &result
	default:
		if !importedEmptyCurationJSON(obj["applied_result"], map[string]any{}) {
			return out, fmt.Errorf("unapplied action carries applied_result")
		}
	}
	if state == "reverted" {
		var result MemoryCurationActionRollbackResult
		if err := decodeImportedCurationJSON(obj["rollback_result"], &result); err != nil ||
			result.ActionID != actionID || result.Ordinal != ordinal || result.Operation != primitive {
			return out, fmt.Errorf("rollback_result does not match its action")
		}
		out.rollbackResult = &result
	} else if !importedEmptyCurationJSON(obj["rollback_result"], map[string]any{}) {
		return out, fmt.Errorf("unreverted action carries rollback_result")
	}
	return out, nil
}

func decodeImportedCurationJSON(value any, destination any) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return decodeMemoryCurationStoredJSON(raw, destination)
}

func importedEmptyCurationJSON(value, empty any) bool {
	return sameCanonicalMemoryCurationJSON(value, empty)
}

func validCurationMutationOperation(value string) bool {
	switch value {
	case "request", "start", "renew", "plan", "apply", "cancel", "abandon", "rollback":
		return true
	default:
		return false
	}
}

func (ic *importCtx) validateImportedMemoryEvidence(
	obj map[string]any,
	target memoryVersionImportKey,
	owner memoryOwnerImportKey,
	changeSeq int64,
) error {
	state, err := requireStringField(obj, "resolution_state")
	if err != nil {
		return fmt.Errorf("resolution_state is required")
	}
	pendingID, hasPending := optionalStringField(obj, "pending_evidence_id")
	if hasPending {
		pending, exists := ic.memoryEvidence[pendingID]
		if !exists || pending.target != target || pending.state != "pending" {
			return fmt.Errorf("pending evidence %q is not a pending row on the target version", pendingID)
		}
		if changeSeq <= pending.changeSeq {
			return fmt.Errorf("terminal resolution change_seq must follow its pending evidence")
		}
	} else if changeSeq <= ic.memoryVersions[target].changeSeq {
		return fmt.Errorf("initial evidence change_seq must follow its target version")
	}
	key, hasKey := optionalStringField(obj, "idempotency_key")
	hash, hasHash := optionalStringField(obj, "request_hash")
	if hasPending {
		if !hasKey || key == "" || !hasHash || !validFactSHA256(hash) {
			return fmt.Errorf("terminal resolution requires an idempotency_key and request_hash")
		}
	} else {
		for _, field := range []string{"idempotency_key", "request_hash"} {
			if value, supplied := obj[field]; supplied && value != nil {
				return fmt.Errorf("initial evidence %s must be null", field)
			}
		}
	}

	switch state {
	case "pending":
		locator, present := optionalStringField(obj, "external_locator")
		if !present || locator == "" || hasPending {
			return fmt.Errorf("pending evidence requires only an external locator")
		}
	case "unavailable":
		reason, present := optionalStringField(obj, "terminal_reason_code")
		if !present || reason == "" || hasPending {
			return fmt.Errorf("unavailable evidence requires only a terminal reason")
		}
	case "unresolvable":
		reason, present := optionalStringField(obj, "terminal_reason_code")
		if !hasPending || !present || reason == "" {
			return fmt.Errorf("unresolvable evidence requires pending evidence and a reason")
		}
		if ic.memoryEvidenceTerminal[pendingID] {
			return fmt.Errorf("pending evidence %q has more than one terminal resolution", pendingID)
		}
		ic.memoryEvidenceTerminal[pendingID] = true
	case "resolved":
		kind, err := requireStringField(obj, "resolved_kind")
		if err != nil {
			return fmt.Errorf("resolved evidence requires resolved_kind")
		}
		switch kind {
		case "transcript":
			transcriptID, err := requireStringField(obj, "source_transcript_id")
			scope, exists := ic.transcripts[transcriptID]
			from, fromOK := importedPositiveInteger(obj["source_sequence_from"])
			until, untilOK := importedPositiveInteger(obj["source_sequence_until"])
			if err != nil || !exists || scope.realmID != owner.realmID ||
				scope.ownerAgentID != owner.ownerID || !fromOK || !untilOK ||
				until < from || until >= scope.nextSequence {
				return fmt.Errorf("transcript evidence range is outside its owner transcript")
			}
		case "memory":
			memoryID, err := requireStringField(obj, "source_memory_id")
			version, ok := importedPositiveInteger(obj["source_memory_version"])
			source, exists := ic.memoryVersions[memoryVersionImportKey{
				memoryID: memoryID, version: version,
			}]
			if err != nil || !ok || !exists || source.owner != owner {
				return fmt.Errorf("source memory version is outside its owner scope")
			}
		case "message":
			messageID, err := requireStringField(obj, "source_message_id")
			message, exists := ic.messages[messageID]
			if err != nil || !exists || message.realmID != owner.realmID ||
				(message.fromAgentID != owner.ownerID && message.toAgentID != owner.ownerID) {
				return fmt.Errorf("source message is outside its owner scope")
			}
		case "import_artifact":
			locator, present := optionalStringField(obj, "source_import_locator")
			if !present || locator == "" {
				return fmt.Errorf("import artifact evidence requires a locator")
			}
		case "artifact":
			artifact, present := obj["artifact_excerpt"]
			if !present || artifact == nil {
				return fmt.Errorf("artifact evidence requires an excerpt")
			}
		default:
			return fmt.Errorf("resolved_kind %q is invalid", kind)
		}
		if hasPending {
			if ic.memoryEvidenceTerminal[pendingID] {
				return fmt.Errorf("pending evidence %q has more than one terminal resolution", pendingID)
			}
			ic.memoryEvidenceTerminal[pendingID] = true
		}
	default:
		return fmt.Errorf("resolution_state %q is invalid", state)
	}
	return nil
}

func (ic *importCtx) validateUsageScope(obj map[string]any, badf func(string, ...any) error, table string) error {
	realmID, err := requireStringField(obj, "realm_id")
	if err != nil || !ic.realms[realmID] {
		return badf("%s row references realm %q not present in this archive", table, realmID)
	}
	agentID, err := requireStringField(obj, "agent_id")
	if err != nil || !ic.agents[agentID] || ic.agentRealms[agentID] != realmID {
		return badf("%s row references agent %q outside realm %q", table, agentID, realmID)
	}
	return nil
}

// requireStringField reads a JSON string field; treats JSON null / missing / wrong-type as absent.
func requireStringField(obj map[string]any, key string) (string, error) {
	s, ok := stringField(obj, key)
	if !ok {
		return "", fmt.Errorf("required %s absent", key)
	}
	return s, nil
}

func stringField(obj map[string]any, key string) (string, bool) {
	v, present := obj[key]
	if !present || v == nil {
		return "", false
	}
	s, ok := v.(string)
	if !ok || s == "" {
		return "", false
	}
	return s, true
}

// optionalStringField distinguishes "the field is a non-empty string" from
// "the field is absent or JSON null" (both legal — FKs are nullable). A
// present-but-non-string value is treated as absent, since it can't be a
// valid FK target anyway; the subsequent INSERT will fail its type coercion.
func optionalStringField(obj map[string]any, key string) (string, bool) {
	return stringField(obj, key)
}

func validateImportedFactSubjectContent(obj map[string]any) (string, error) {
	canonicalKey, ok := obj["canonical_key"].(string)
	if !ok || !factSubjectPattern.MatchString(canonicalKey) || normalizeFactSubject(canonicalKey) != canonicalKey {
		return "", fmt.Errorf("canonical_key is invalid")
	}
	displayName, ok := obj["display_name"].(string)
	if !ok || !validFactSubjectDisplayName(displayName) {
		return "", fmt.Errorf("display_name is invalid")
	}
	aliases, ok := obj["aliases"].([]any)
	if !ok {
		return "", fmt.Errorf("aliases must be an array of strings")
	}
	encodedAliases, err := json.Marshal(aliases)
	if err != nil || len(encodedAliases) > maxFactSubjectAliasesJSONBytes {
		return "", fmt.Errorf("aliases exceed the size limit")
	}
	seen := map[string]bool{canonicalKey: true}
	for _, raw := range aliases {
		alias, ok := raw.(string)
		if !ok || !validFactSubjectAlias(alias) || normalizeFactSubjectAlias(alias) != alias {
			return "", fmt.Errorf("aliases must contain normalized strings")
		}
		if normalizeFactSubject(alias) == "self" && canonicalKey != "self" {
			return "", fmt.Errorf("alias %q is reserved for self", alias)
		}
		if seen[alias] {
			return "", fmt.Errorf("alias %q is duplicated", alias)
		}
		seen[alias] = true
	}
	return canonicalKey, nil
}

func validateImportedFactContent(obj map[string]any, ownerAgentID string) (resolvedID string, deleted bool, replacementID string, err error) {
	predicate, ok := obj["predicate"].(string)
	if !ok || !validFactPredicate(predicate) {
		return "", false, "", fmt.Errorf("predicate is invalid")
	}
	if _, ok := obj["sensitive"].(bool); !ok {
		return "", false, "", fmt.Errorf("sensitive must be a boolean")
	}
	cardinality, ok := obj["cardinality"].(string)
	if !ok || (cardinality != FactCardinalityOne && cardinality != FactCardinalityMany && cardinality != FactCardinalityOneAtTime) {
		return "", false, "", fmt.Errorf("cardinality is invalid")
	}
	resolvedID, _ = optionalStringField(obj, "resolved_assertion_id")
	deletedAt, hasDeletedAt, err := importedOptionalTimestamp(obj, "deleted_at")
	if err != nil {
		return "", false, "", err
	}
	_ = deletedAt
	deleted = hasDeletedAt
	deletedBy, hasDeletedBy := optionalStringField(obj, "deleted_by_agent_id")
	receipt, _ := obj["delete_receipt_id"].(string)
	deleteKeyHash, _ := obj["delete_idempotency_key_hash"].(string)
	priorAssertionID, _ := obj["deleted_prior_assertion_id"].(string)
	assertionCount, assertionCountOK := importedNonnegativeInteger(obj["deleted_assertion_count"])
	candidateCount, candidateCountOK := importedNonnegativeInteger(obj["deleted_candidate_count"])
	usageCount, usageCountOK := importedNonnegativeInteger(obj["deleted_usage_count"])
	mutationKeyCount, mutationKeyCountOK := importedNonnegativeInteger(obj["deleted_mutation_key_count"])
	candidateRevision, _ := obj["deleted_candidate_revision"].(string)
	_, hasRecreatedAt, err := importedOptionalTimestamp(obj, "recreated_at")
	if err != nil {
		return "", false, "", err
	}
	replacementID, hasReplacement := optionalStringField(obj, "replacement_fact_id")

	if !deleted {
		if resolvedID == "" {
			return "", false, "", fmt.Errorf("active fact requires resolved_assertion_id")
		}
		if hasDeletedBy || receipt != "" || deleteKeyHash != "" || priorAssertionID != "" ||
			!assertionCountOK || assertionCount != 0 || !candidateCountOK || candidateCount != 0 ||
			!usageCountOK || usageCount != 0 ||
			!mutationKeyCountOK || mutationKeyCount != 0 ||
			candidateRevision != "" ||
			hasRecreatedAt || hasReplacement {
			return "", false, "", fmt.Errorf("active fact carries deletion metadata")
		}
		return resolvedID, false, "", nil
	}
	if resolvedID != "" || !hasDeletedBy || deletedBy != ownerAgentID ||
		!strings.HasPrefix(receipt, "fdel_") || !validFactSHA256(deleteKeyHash) ||
		!strings.HasPrefix(priorAssertionID, "fas_") || !assertionCountOK || assertionCount < 1 ||
		!candidateCountOK || !usageCountOK || !mutationKeyCountOK || !validFactSHA256(candidateRevision) {
		return "", false, "", fmt.Errorf("deleted fact receipt is invalid")
	}
	if hasRecreatedAt != hasReplacement {
		return "", false, "", fmt.Errorf("deleted fact replacement metadata is incomplete")
	}
	if hasReplacement && (!strings.HasPrefix(replacementID, "fact_") || replacementID == obj["id"]) {
		return "", false, "", fmt.Errorf("replacement_fact_id is invalid")
	}
	return "", true, replacementID, nil
}

func validateImportedFactMutationTombstone(obj map[string]any) error {
	id, ok := obj["id"].(string)
	if !ok || !strings.HasPrefix(id, "fmt_") {
		return fmt.Errorf("id is invalid")
	}
	surface, ok := obj["surface"].(string)
	if !ok || (surface != "set" && surface != "proposal") {
		return fmt.Errorf("surface is invalid")
	}
	hash, ok := obj["idempotency_key_hash"].(string)
	if !ok || !validFactSHA256(hash) {
		return fmt.Errorf("idempotency_key_hash is invalid")
	}
	if _, present, err := importedOptionalTimestamp(obj, "deleted_at"); err != nil || !present {
		return fmt.Errorf("deleted_at must be an RFC3339 timestamp")
	}
	return nil
}

func importedOptionalTimestamp(obj map[string]any, key string) (*time.Time, bool, error) {
	raw, present := obj[key]
	if !present || raw == nil {
		return nil, false, nil
	}
	value, ok := raw.(string)
	if !ok || value == "" {
		return nil, false, fmt.Errorf("%s must be an RFC3339 timestamp", key)
	}
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return nil, false, fmt.Errorf("%s must be an RFC3339 timestamp", key)
	}
	return &parsed, true, nil
}

func importedNonnegativeInteger(raw any) (int64, bool) {
	value, ok := importedInteger(raw)
	if !ok || value < 0 {
		return 0, false
	}
	return value, true
}

func importedOptionalNonnegativeInteger(obj map[string]any, key string) (int64, bool) {
	raw, present := obj[key]
	if !present {
		return 0, true
	}
	return importedNonnegativeInteger(raw)
}

// importedInteger parses archive integers without ever routing JSON numbers
// through float64. PostgreSQL BIGINT values can exceed JavaScript's 53-bit
// integer range, and rounding them would corrupt change clocks and version
// ordering before the original NDJSON reaches jsonb_populate_record.
//
// The concrete integer and float cases keep direct validator unit tests and
// callers useful; ImportAccount itself always decodes JSON numbers as
// json.Number via decodeImportRow.
func importedInteger(raw any) (int64, bool) {
	switch value := raw.(type) {
	case json.Number:
		parsed, err := value.Int64()
		return parsed, err == nil
	case int:
		return int64(value), true
	case int64:
		return value, true
	case int32:
		return int64(value), true
	case float64:
		// float64 is accepted only for in-process callers. Reject the
		// unrepresentable int64 boundary and every fractional/non-finite
		// value rather than depending on implementation-defined conversion.
		if math.IsNaN(value) || math.IsInf(value, 0) || value != math.Trunc(value) ||
			value < -float64(uint64(1)<<63) || value >= float64(uint64(1)<<63) {
			return 0, false
		}
		return int64(value), true
	default:
		return 0, false
	}
}

func importedFloat64(raw any) (float64, bool) {
	var value float64
	switch raw := raw.(type) {
	case json.Number:
		parsed, err := raw.Float64()
		if err != nil {
			return 0, false
		}
		value = parsed
	case float64:
		value = raw
	case int:
		value = float64(raw)
	case int64:
		value = float64(raw)
	default:
		return 0, false
	}
	return value, !math.IsNaN(value) && !math.IsInf(value, 0)
}

func importedPositiveInteger(raw any) (int64, bool) {
	value, ok := importedNonnegativeInteger(raw)
	return value, ok && value > 0
}

func importedOptionalPositiveInteger(obj map[string]any, key string) (int64, bool, bool) {
	raw, present := obj[key]
	if !present || raw == nil {
		return 0, false, true
	}
	value, ok := importedPositiveInteger(raw)
	return value, true, ok
}

func importedNormalizedMemoryStrings(raw any, maxCount, maxBytes int, label string) ([]string, error) {
	values, ok := raw.([]any)
	if !ok {
		return nil, fmt.Errorf("%ss must be an array of strings", label)
	}
	original := make([]string, len(values))
	for _, value := range values {
		if _, ok := value.(string); !ok {
			return nil, fmt.Errorf("%ss must be an array of strings", label)
		}
	}
	for index := range values {
		original[index] = values[index].(string)
	}
	normalized, err := normalizeMemoryStrings(original, maxCount, maxBytes, label)
	if err != nil {
		return nil, err
	}
	if !equalStrings(original, normalized) {
		return nil, fmt.Errorf("%ss are not in normalized stored order", label)
	}
	return normalized, nil
}

func validFactSHA256(value string) bool {
	if len(value) != 64 {
		return false
	}
	for _, char := range value {
		if (char < '0' || char > '9') && (char < 'a' || char > 'f') {
			return false
		}
	}
	return true
}

func validateImportedFactAssertionContent(obj map[string]any) error {
	requiredString := func(key string) (string, error) {
		value, ok := obj[key].(string)
		if !ok || value == "" {
			return "", fmt.Errorf("%s must be a non-empty string", key)
		}
		return value, nil
	}
	stringValue := func(key string) (string, error) {
		value, ok := obj[key].(string)
		if !ok {
			return "", fmt.Errorf("%s must be a string", key)
		}
		return value, nil
	}
	parseTimestamp := func(key string, required bool) (*time.Time, error) {
		raw, present := obj[key]
		if !present || raw == nil {
			if required {
				return nil, fmt.Errorf("%s must be an RFC3339 timestamp", key)
			}
			return nil, nil
		}
		value, ok := raw.(string)
		if !ok || value == "" {
			return nil, fmt.Errorf("%s must be an RFC3339 timestamp", key)
		}
		parsed, err := time.Parse(time.RFC3339Nano, value)
		if err != nil {
			return nil, fmt.Errorf("%s must be an RFC3339 timestamp", key)
		}
		return &parsed, nil
	}

	valueType, err := requiredString("value_type")
	if err != nil {
		return err
	}
	recurrence := ""
	if raw, present := obj["recurrence"]; present {
		var ok bool
		recurrence, ok = raw.(string)
		if !ok {
			return fmt.Errorf("recurrence must be a string")
		}
	}
	sourceKind, err := requiredString("source_kind")
	if err != nil {
		return err
	}
	sourceRef, err := stringValue("source_ref")
	if err != nil {
		return err
	}
	confidence, ok := importedFloat64(obj["confidence"])
	if !ok {
		return fmt.Errorf("confidence must be a number")
	}
	value, present := obj["value"]
	if !present {
		return fmt.Errorf("value is required")
	}
	rawValue, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("value is not JSON: %v", err)
	}
	observedAt, err := parseTimestamp("observed_at", true)
	if err != nil {
		return err
	}
	confirmedAt, err := parseTimestamp("confirmed_at", false)
	if err != nil {
		return err
	}
	validFrom, err := parseTimestamp("valid_from", false)
	if err != nil {
		return err
	}
	validUntil, err := parseTimestamp("valid_until", false)
	if err != nil {
		return err
	}
	if _, err := parseTimestamp("created_at", true); err != nil {
		return err
	}
	normalized, err := normalizeSetFactInput(SetFactInput{
		Subject: "self", Predicate: "archive/value", ValueType: valueType,
		Value: rawValue, Recurrence: recurrence, Cardinality: FactCardinalityOne,
		SourceKind: sourceKind, SourceRef: sourceRef, Confidence: &confidence,
		ObservedAt: *observedAt, ConfirmedAt: confirmedAt,
		ValidFrom: validFrom, ValidUntil: validUntil,
	})
	if err != nil {
		return err
	}
	if normalized.ValueType != valueType || normalized.Recurrence != recurrence ||
		normalized.SourceKind != sourceKind || !jsonValuesEqual(normalized.Value, rawValue) {
		return fmt.Errorf("logical content is not canonical")
	}
	return nil
}

func validateImportedFactCandidateContent(obj map[string]any) error {
	required := func(key string) (string, error) {
		value, ok := obj[key].(string)
		if !ok || value == "" {
			return "", fmt.Errorf("%s must be a non-empty string", key)
		}
		return value, nil
	}
	stringValue := func(key string) (string, error) {
		value, ok := obj[key].(string)
		if !ok {
			return "", fmt.Errorf("%s must be a string", key)
		}
		return value, nil
	}
	parseTimestamp := func(key string, required bool) (*time.Time, error) {
		raw, present := obj[key]
		if !present || raw == nil {
			if required {
				return nil, fmt.Errorf("%s must be an RFC3339 timestamp", key)
			}
			return nil, nil
		}
		value, ok := raw.(string)
		if !ok || value == "" {
			return nil, fmt.Errorf("%s must be an RFC3339 timestamp", key)
		}
		parsed, err := time.Parse(time.RFC3339Nano, value)
		if err != nil {
			return nil, fmt.Errorf("%s must be an RFC3339 timestamp", key)
		}
		return &parsed, nil
	}
	optionalID := func(key string) (string, bool, error) {
		raw, present := obj[key]
		if !present || raw == nil {
			return "", false, nil
		}
		value, ok := raw.(string)
		if !ok || value == "" {
			return "", false, fmt.Errorf("%s must be null or a non-empty string", key)
		}
		return value, true, nil
	}

	subject, err := required("subject_key")
	if err != nil {
		return err
	}
	predicate, err := required("predicate")
	if err != nil {
		return err
	}
	valueType, err := required("value_type")
	if err != nil {
		return err
	}
	recurrence := ""
	if raw, present := obj["recurrence"]; present {
		var ok bool
		recurrence, ok = raw.(string)
		if !ok {
			return fmt.Errorf("recurrence must be a string")
		}
	}
	cardinality, err := required("cardinality")
	if err != nil {
		return err
	}
	sourceRef, err := stringValue("source_ref")
	if err != nil {
		return err
	}
	reason, err := stringValue("reason")
	if err != nil {
		return err
	}
	if len(reason) > 1024 {
		return fmt.Errorf("reason exceeds 1024 bytes")
	}
	sensitive, ok := obj["sensitive"].(bool)
	if !ok {
		return fmt.Errorf("sensitive must be a boolean")
	}
	confidence, ok := importedFloat64(obj["confidence"])
	if !ok {
		return fmt.Errorf("confidence must be a number")
	}
	value, present := obj["value"]
	if !present {
		return fmt.Errorf("value is required")
	}
	rawValue, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("value is not JSON: %v", err)
	}
	observedAt, err := parseTimestamp("observed_at", true)
	if err != nil {
		return err
	}
	validFrom, err := parseTimestamp("valid_from", false)
	if err != nil {
		return err
	}
	validUntil, err := parseTimestamp("valid_until", false)
	if err != nil {
		return err
	}
	proposedAt, err := parseTimestamp("proposed_at", true)
	if err != nil {
		return err
	}
	decidedAt, err := parseTimestamp("decided_at", false)
	if err != nil {
		return err
	}
	if decidedAt != nil && decidedAt.Before(*proposedAt) {
		return fmt.Errorf("decided_at precedes proposed_at")
	}
	normalized, err := normalizeSetFactInput(SetFactInput{
		Subject: subject, Predicate: predicate, ValueType: valueType,
		Recurrence: recurrence,
		Value:      rawValue, Cardinality: cardinality, Sensitive: sensitive,
		SourceKind: FactSourceInference, SourceRef: sourceRef,
		Confidence: &confidence, ObservedAt: *observedAt,
		ValidFrom: validFrom, ValidUntil: validUntil,
	})
	if err != nil {
		return err
	}
	if normalized.Subject != subject || normalized.Predicate != predicate ||
		normalized.ValueType != valueType || normalized.Recurrence != recurrence || normalized.Cardinality != cardinality ||
		!jsonValuesEqual(normalized.Value, rawValue) {
		return fmt.Errorf("logical content is not canonical")
	}

	status, err := required("status")
	if err != nil {
		return err
	}
	_, hasConflict, err := optionalID("conflict_fact_id")
	if err != nil {
		return err
	}
	_, hasObserved, err := optionalID("observed_assertion_id")
	if err != nil {
		return err
	}
	_, hasResolved, err := optionalID("resolved_fact_id")
	if err != nil {
		return err
	}
	switch status {
	case "pending":
		if hasConflict || hasResolved || decidedAt != nil {
			return fmt.Errorf("pending lifecycle fields are inconsistent")
		}
	case "conflict":
		if !hasConflict || !hasObserved || hasResolved || decidedAt != nil {
			return fmt.Errorf("conflict lifecycle fields are inconsistent")
		}
	case "confirmed":
		if !hasResolved || decidedAt == nil {
			return fmt.Errorf("confirmed lifecycle fields are inconsistent")
		}
	case "rejected":
		if hasResolved || decidedAt == nil {
			return fmt.Errorf("rejected lifecycle fields are inconsistent")
		}
	case "withdrawn":
		if hasResolved || decidedAt == nil || hasConflict != hasObserved {
			return fmt.Errorf("withdrawn lifecycle fields are inconsistent")
		}
	default:
		return fmt.Errorf("status %q is invalid", status)
	}
	withdrawalReason, _ := obj["withdrawal_reason"].(string)
	withdrawalKey, _ := obj["withdrawal_idempotency_key"].(string)
	withdrawalHash, _ := obj["withdrawal_request_hash"].(string)
	if status == "withdrawn" {
		if withdrawalReason != "curation_rollback" || len(withdrawalKey) < 1 ||
			len(withdrawalKey) > 512 || !validFactSHA256(withdrawalHash) {
			return fmt.Errorf("withdrawn receipt is invalid")
		}
	} else if withdrawalReason != "" || withdrawalKey != "" || withdrawalHash != "" {
		return fmt.Errorf("non-withdrawn candidate carries withdrawal receipt")
	}
	return nil
}

// ImportAccount restores one account's logical archive from r into this cell.
// The entire restore is a single transaction committed only after the
// archive's trailing checksums verify AND every row's account/FK scoping
// checks pass, so a truncated, tampered, or content-hostile stream lands
// nothing. The account arrives in its exported state — suspended (or a
// closed tombstone); resuming is the caller's separate, explicit step.
//
// expectedAccountID pins the archive to the account the caller believes it
// is restoring; a manifest naming anyone else refuses before rows stream.
func (s *Store) ImportAccount(ctx context.Context, expectedAccountID string, r io.Reader) (export.Manifest, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return export.Manifest{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	ic := newImportCtx(expectedAccountID)
	var importedAt time.Time
	if err := tx.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&importedAt); err != nil {
		return export.Manifest{}, fmt.Errorf("read destination database clock: %w", err)
	}
	importedAt = importedAt.UTC()
	ic.importedAt = importedAt

	m, err := export.Read(ctx, r, export.ImportOptions{
		CurrentSchema: SchemaVersion(),
		OnManifest: func(m export.Manifest) error {
			ic.schemaVersion = m.SchemaVersion
			if m.AccountID == "" || m.AccountID != expectedAccountID {
				return fmt.Errorf("%w: archive is for %q", ErrArchiveAccountMismatch, m.AccountID)
			}
			if m.Status != "suspended" && m.Status != "closed" {
				return fmt.Errorf("%w: manifest status %q — exports are only taken frozen", ErrArchiveContent, m.Status)
			}
			if err := validateArchiveManifestTables(m.SchemaVersion, m.Tables); err != nil {
				return err
			}
			ic.schemaVersion = m.SchemaVersion
			if err := validateArchiveExportedAt(m.ExportedAt, importedAt); err != nil {
				return err
			}
			ic.exportedAt = m.ExportedAt.UTC()
			var exists bool
			if err := tx.QueryRow(ctx,
				`SELECT EXISTS(SELECT 1 FROM accounts WHERE id = $1)`,
				m.AccountID).Scan(&exists); err != nil {
				return fmt.Errorf("check import target: %w", err)
			}
			if exists {
				return ErrAccountExists
			}
			return nil
		},
		Row: func(table string, row []byte) error {
			if _, ok := importColumns[table]; !ok {
				return fmt.Errorf("%w: table %q not importable", ErrArchiveContent, table)
			}
			obj, err := decodeImportRow(row)
			if err != nil {
				return fmt.Errorf("%w: %s row is not JSON: %v", ErrArchiveContent, table, err)
			}
			if err := ic.normalizeImportedCurationLease(table, obj, importedAt); err != nil {
				return err
			}
			if err := ic.normalizeImportedMessageClaim(table, obj); err != nil {
				return err
			}
			if err := ic.normalizeImportedMessageRequestClaim(table, obj, importedAt); err != nil {
				return err
			}
			if err := ic.normalizeLegacyImportedAvatarPayloadFields(table, obj); err != nil {
				return err
			}
			if err := ic.validateAndRecord(table, obj); err != nil {
				return err
			}
			normalizedRow, err := json.Marshal(obj)
			if err != nil {
				return fmt.Errorf("%w: marshal normalized %s row: %v", ErrArchiveContent, table, err)
			}
			return insertProjected(ctx, tx, table, obj, normalizedRow)
		},
	})
	if err != nil {
		return export.Manifest{}, err
	}
	if m.SchemaVersion < 50 {
		if err := synthesizeLegacyImportedAvatars(ctx, tx, ic); err != nil {
			return export.Manifest{}, fmt.Errorf("synthesize legacy avatar defaults: %w", err)
		}
	} else {
		if err := ic.validateImportedAvatarGraph(); err != nil {
			return export.Manifest{}, fmt.Errorf("%w: avatar graph: %v", ErrArchiveContent, err)
		}
	}
	if err := ic.validateImportedSecretGraph(); err != nil {
		return export.Manifest{}, fmt.Errorf("%w: secret graph: %v", ErrArchiveContent, err)
	}
	if m.SchemaVersion < 35 {
		if err := normalizeLegacyImportedMessageCausalDepths(ctx, tx, ic.messages, expectedAccountID); err != nil {
			return export.Manifest{}, fmt.Errorf("%w: normalize legacy message causal depth: %v", ErrArchiveContent, err)
		}
	}
	if err := validateImportedMessageAudienceSnapshots(ic.messages); err != nil {
		return export.Manifest{}, fmt.Errorf("%w: message audience snapshot: %v", ErrArchiveContent, err)
	}
	if err := validateImportedMessageReplies(ic.messages); err != nil {
		return export.Manifest{}, fmt.Errorf("%w: message reply graph: %v", ErrArchiveContent, err)
	}
	if err := validateImportedMessageProcessingLinks(ic.messages, ic.deliveries); err != nil {
		return export.Manifest{}, fmt.Errorf("%w: message processing graph: %v", ErrArchiveContent, err)
	}
	if err := validateImportedMessageRequestGraph(ic); err != nil {
		return export.Manifest{}, fmt.Errorf("%w: message request graph: %v", ErrArchiveContent, err)
	}
	for memoryID, memory := range ic.memories {
		clock, hasClock := ic.memoryClocks[memory.owner]
		if !hasClock || clock < memory.maxChangeSeq {
			return export.Manifest{}, fmt.Errorf(
				"%w: memory %q change clock %d is below imported change sequence %d",
				ErrArchiveContent, memoryID, clock, memory.maxChangeSeq,
			)
		}
		if memory.deleted {
			if memory.versionCount != 0 {
				return export.Manifest{}, fmt.Errorf(
					"%w: permanently deleted memory %q still has %d versions",
					ErrArchiveContent, memoryID, memory.versionCount,
				)
			}
			if err := validateImportedMemoryRetryShields(
				memoryID, memory, ic.memoryDeletedRetryShields[memoryID],
			); err != nil {
				return export.Manifest{}, err
			}
			continue
		}
		head, ok := ic.memoryVersions[memoryVersionImportKey{
			memoryID: memoryID, version: memory.currentVersion,
		}]
		if !ok || head.owner != memory.owner || memory.currentVersion != memory.maxVersion ||
			memory.versionCount != memory.maxVersion {
			return export.Manifest{}, fmt.Errorf(
				"%w: memory %q current version %d is not the contiguous latest version %d",
				ErrArchiveContent, memoryID, memory.currentVersion, memory.maxVersion,
			)
		}
	}
	if err := validateImportedMemorySupersessionMembership(ic.memories, ic.memoryVersions); err != nil {
		return export.Manifest{}, fmt.Errorf("%w: memory supersession membership: %v", ErrArchiveContent, err)
	}
	if err := validateImportedMemoryCurationGraph(ic); err != nil {
		return export.Manifest{}, fmt.Errorf("%w: memory curation graph: %v", ErrArchiveContent, err)
	}
	for factID, scope := range ic.facts {
		if scope.deleted {
			if scope.resolvedAssertionID != "" {
				return export.Manifest{}, fmt.Errorf("%w: deleted fact %q has a resolved assertion", ErrArchiveContent, factID)
			}
			if scope.replacementFactID != "" {
				replacement, ok := ic.facts[scope.replacementFactID]
				if !ok || replacement.subjectID != scope.subjectID || replacement.predicate != scope.predicate ||
					replacement.realmID != scope.realmID || replacement.ownerAgentID != scope.ownerAgentID {
					return export.Manifest{}, fmt.Errorf("%w: deleted fact %q has invalid replacement %q", ErrArchiveContent, factID, scope.replacementFactID)
				}
				if scope.sensitive && !replacement.sensitive {
					return export.Manifest{}, fmt.Errorf("%w: sensitive deleted fact %q has non-sensitive replacement %q", ErrArchiveContent, factID, scope.replacementFactID)
				}
			}
			continue
		}
		if scope.resolvedAssertionID == "" || ic.assertions[scope.resolvedAssertionID] != factID {
			return export.Manifest{}, fmt.Errorf("%w: fact %q has no valid resolved assertion", ErrArchiveContent, factID)
		}
	}
	if err := validateImportedFactReplacementTopology(ic.facts); err != nil {
		return export.Manifest{}, fmt.Errorf("%w: fact replacement topology: %v", ErrArchiveContent, err)
	}
	if err := validateImportedFactMutationTombstoneCompleteness(ic.facts, ic.factMutationTombstoneCounts); err != nil {
		return export.Manifest{}, fmt.Errorf("%w: fact mutation tombstones: %v", ErrArchiveContent, err)
	}
	if err := validateImportedFactDecisionAssertions(ctx, tx, m.AccountID); err != nil {
		return export.Manifest{}, err
	}
	if err := validateImportedUsageRollups(ctx, tx, m.AccountID); err != nil {
		return export.Manifest{}, err
	}

	// The archive's own account row must have actually landed, and must not
	// claim the deployment's default seat. These are all permanent
	// archive-content defects (missing accounts row, is_default lie,
	// status mismatch), so they wrap ErrArchiveContent and surface as
	// 400 — retrying against the same object cannot recover.
	var isDefault bool
	var status string
	err = tx.QueryRow(ctx,
		`SELECT is_default, status FROM accounts WHERE id = $1`,
		m.AccountID).Scan(&isDefault, &status)
	if errors.Is(err, pgx.ErrNoRows) {
		return export.Manifest{}, fmt.Errorf("%w: no accounts row landed for %s", ErrArchiveContent, m.AccountID)
	}
	if err != nil {
		return export.Manifest{}, fmt.Errorf("verify landed account: %w", err)
	}
	if isDefault {
		return export.Manifest{}, fmt.Errorf("%w: landed row claims the default seat", ErrArchiveContent)
	}
	if status != m.Status {
		return export.Manifest{}, fmt.Errorf("%w: landed account row status %q disagrees with manifest %q", ErrArchiveContent, status, m.Status)
	}
	// Import may happen long after the source snapshot. Materialize any request
	// deadlines crossed in transit before the account becomes visible on this
	// cell. The state transition, active-fence cancellation, and value-free
	// system audit share this import transaction and are exactly-once because
	// only state=open rows can transition.
	if _, _, err := drainMessageRequestReconciliationTx(ctx, tx, m.AccountID); err != nil {
		return export.Manifest{}, fmt.Errorf("reconcile imported message requests: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return export.Manifest{}, err
	}
	return m, nil
}

func deriveImportedMessageCausalDepths(messages map[string]messageImportScope) (map[string]int64, error) {
	for messageID, message := range messages {
		if message.replyToMessageID == "" {
			continue
		}
		if message.replyToMessageID == messageID {
			return nil, fmt.Errorf("message %q replies to itself", messageID)
		}
		parent, ok := messages[message.replyToMessageID]
		if !ok {
			return nil, fmt.Errorf("message %q references missing parent %q", messageID, message.replyToMessageID)
		}
		parentDeliveredToSender := parent.recipientAgentIDs[message.fromAgentID]
		if parent.audienceKind == "" || parent.audienceKind == MessageRecipientAgent {
			parentDeliveredToSender = parent.toAgentID == message.fromAgentID
		}
		messageAudience := message.audienceKind
		if messageAudience == "" {
			messageAudience = MessageRecipientAgent
		}
		if parent.realmID != message.realmID || parent.threadID != message.threadID ||
			messageAudience != MessageRecipientAgent ||
			parent.fromAgentID != message.toAgentID || !parentDeliveredToSender {
			return nil, fmt.Errorf("message %q does not preserve parent participants, realm, and thread", messageID)
		}
	}

	// Each message has at most one parent. A three-state walk both calculates
	// the backend-owned depth and rejects a checksum-valid archive that uses
	// deferred FKs to manufacture a causal cycle.
	state := make(map[string]uint8, len(messages))
	depths := make(map[string]int64, len(messages))
	var visit func(string) (int64, error)
	visit = func(messageID string) (int64, error) {
		switch state[messageID] {
		case 1:
			return 0, fmt.Errorf("reply graph contains a cycle at %q", messageID)
		case 2:
			return depths[messageID], nil
		}
		state[messageID] = 1
		depth := int64(1)
		if parentID := messages[messageID].replyToMessageID; parentID != "" {
			parentDepth, err := visit(parentID)
			if err != nil {
				return 0, err
			}
			if parentDepth >= maxMessageCausalDepth {
				return 0, fmt.Errorf("message %q exceeds maximum causal depth", messageID)
			}
			depth = parentDepth + 1
		}
		state[messageID] = 2
		depths[messageID] = depth
		return depth, nil
	}
	for messageID := range messages {
		if _, err := visit(messageID); err != nil {
			return nil, err
		}
	}
	return depths, nil
}

func validateImportedMessageReplies(messages map[string]messageImportScope) error {
	depths, err := deriveImportedMessageCausalDepths(messages)
	if err != nil {
		return err
	}
	for messageID, expected := range depths {
		if messages[messageID].causalDepth != expected {
			return fmt.Errorf("message %q causal_depth %d does not match derived depth %d",
				messageID, messages[messageID].causalDepth, expected)
		}
	}
	return nil
}

func validateImportedMessageAudienceSnapshots(messages map[string]messageImportScope) error {
	for messageID, message := range messages {
		count := len(message.recipientAgentIDs)
		audience := message.audienceKind
		if audience == "" {
			audience = MessageRecipientAgent
		}
		switch audience {
		case MessageRecipientAgent:
			if count != 1 || !message.recipientAgentIDs[message.toAgentID] {
				return fmt.Errorf("direct message %q does not have its one recipient delivery", messageID)
			}
		case MessageRecipientAgents, MessageRecipientRealm:
			if count < 1 || count > maxMessageAudienceRecipients {
				return fmt.Errorf("message %q audience snapshot has %d recipients", messageID, count)
			}
		default:
			return fmt.Errorf("message %q audience kind %q is invalid", messageID, audience)
		}
	}
	return nil
}

func normalizeLegacyImportedMessageCausalDepths(
	ctx context.Context,
	tx pgxExec,
	messages map[string]messageImportScope,
	accountID string,
) error {
	depths, err := deriveImportedMessageCausalDepths(messages)
	if err != nil {
		return err
	}
	messageIDs := make([]string, 0, len(depths))
	for messageID := range depths {
		messageIDs = append(messageIDs, messageID)
	}
	sort.Strings(messageIDs)
	for _, messageID := range messageIDs {
		depth := depths[messageID]
		if _, err := tx.Exec(ctx, `
			UPDATE agent_messages SET causal_depth=$1
			WHERE id=$2 AND account_id=$3`, depth, messageID, accountID); err != nil {
			return fmt.Errorf("update message %q causal depth: %w", messageID, err)
		}
		scope := messages[messageID]
		scope.causalDepth = depth
		messages[messageID] = scope
	}
	return nil
}

func validateImportedMessageProcessingShape(obj map[string]any) (messageDeliveryImportScope, error) {
	for _, field := range []string{
		"processing_state", "processing_generation", "failure_count", "claim_id", "claim_key_hash",
		"lease_expires_at", "completed_at", "complete_key_hash", "result_message_id",
	} {
		if _, present := obj[field]; !present {
			return messageDeliveryImportScope{}, fmt.Errorf("missing %s", field)
		}
	}
	state, ok := obj["processing_state"].(string)
	if !ok {
		return messageDeliveryImportScope{}, fmt.Errorf("processing_state is invalid")
	}
	generation, ok := importedGeneration(obj["processing_generation"], true)
	if !ok {
		return messageDeliveryImportScope{}, fmt.Errorf("processing_generation is invalid")
	}
	failureCount, ok := importedGeneration(obj["failure_count"], true)
	if !ok {
		return messageDeliveryImportScope{}, fmt.Errorf("failure_count is invalid")
	}
	claimID, hasClaim, err := importedNullableNonemptyString(obj, "claim_id")
	if err != nil {
		return messageDeliveryImportScope{}, err
	}
	claimKeyHash, ok := obj["claim_key_hash"].(string)
	if !ok {
		return messageDeliveryImportScope{}, fmt.Errorf("claim_key_hash is invalid")
	}
	_, hasLease, err := importedOptionalTimestamp(obj, "lease_expires_at")
	if err != nil {
		return messageDeliveryImportScope{}, err
	}
	_, hasCompletedAt, err := importedOptionalTimestamp(obj, "completed_at")
	if err != nil {
		return messageDeliveryImportScope{}, err
	}
	completeKeyHash, ok := obj["complete_key_hash"].(string)
	if !ok {
		return messageDeliveryImportScope{}, fmt.Errorf("complete_key_hash is invalid")
	}
	resultMessageID, hasResult, err := importedNullableNonemptyString(obj, "result_message_id")
	if err != nil {
		return messageDeliveryImportScope{}, err
	}

	scope := messageDeliveryImportScope{
		processingState: state, processingGeneration: generation,
		failureCount: failureCount,
		claimID:      claimID, resultMessageID: resultMessageID,
	}
	switch state {
	case MessageProcessingAvailable:
		if hasClaim || claimKeyHash != "" || hasLease || hasCompletedAt ||
			completeKeyHash != "" || hasResult {
			return messageDeliveryImportScope{}, fmt.Errorf("available processing shape is invalid")
		}
	case MessageProcessingClaimed:
		if generation < 1 || !hasClaim || !validImportedGeneratedID(claimID, "mcl") ||
			!isSHA256Hex(claimKeyHash) || !hasLease || hasCompletedAt ||
			completeKeyHash != "" || hasResult {
			return messageDeliveryImportScope{}, fmt.Errorf("claimed processing shape is invalid")
		}
	case MessageProcessingCompleted:
		if generation < 1 || !hasClaim || !validImportedGeneratedID(claimID, "mcl") ||
			!isSHA256Hex(claimKeyHash) || hasLease || !hasCompletedAt ||
			!isSHA256Hex(completeKeyHash) || !hasResult ||
			!validImportedGeneratedID(resultMessageID, "msg") {
			return messageDeliveryImportScope{}, fmt.Errorf("completed processing shape is invalid")
		}
	default:
		return messageDeliveryImportScope{}, fmt.Errorf("processing_state is invalid")
	}
	return scope, nil
}

func importedNullableNonemptyString(obj map[string]any, field string) (string, bool, error) {
	raw, present := obj[field]
	if !present {
		return "", false, fmt.Errorf("missing %s", field)
	}
	if raw == nil {
		return "", false, nil
	}
	value, ok := raw.(string)
	if !ok || value == "" {
		return "", false, fmt.Errorf("%s is invalid", field)
	}
	return value, true, nil
}

func messageRealmAudienceFingerprint() string {
	sum := sha256.Sum256([]byte(MessageRecipientRealm))
	return hex.EncodeToString(sum[:])
}

func requireImportedTimestamp(obj map[string]any, field string) (*time.Time, error) {
	value, present, err := importedOptionalTimestamp(obj, field)
	if err != nil || !present {
		return nil, fmt.Errorf("%s must be an RFC3339 timestamp", field)
	}
	return value, nil
}

// requireTimestampAtOrBeforeExport bounds row-authored history by the exact
// database timestamp in the manifest. The manifest itself may use the small
// cross-cell skew allowance checked by validateArchiveExportedAt; rows never
// receive an additional allowance.
func (ic *importCtx) requireTimestampAtOrBeforeExport(field string, value time.Time) error {
	// Direct validator tests construct partial import contexts. ImportAccount
	// always sets exportedAt before streaming the first row.
	if ic.exportedAt.IsZero() {
		return nil
	}
	if value.After(ic.exportedAt) {
		return fmt.Errorf("%s is later than manifest exported_at", field)
	}
	return nil
}

func (ic *importCtx) validateImportedMessageRequest(obj map[string]any) (messageRequestImportScope, error) {
	requestID, err := requireStringField(obj, "id")
	if err != nil || !validImportedGeneratedID(requestID, "mrq") {
		return messageRequestImportScope{}, fmt.Errorf("id is invalid")
	}
	realmID, err := requireStringField(obj, "realm_id")
	if err != nil || !ic.realms[realmID] {
		return messageRequestImportScope{}, fmt.Errorf("realm %q is not present in this archive", realmID)
	}
	coordinatorID, err := requireStringField(obj, "coordinator_agent_id")
	if err != nil || !ic.agents[coordinatorID] || ic.agentRealms[coordinatorID] != realmID {
		return messageRequestImportScope{}, fmt.Errorf("coordinator %q is outside realm %q", coordinatorID, realmID)
	}
	openingID, err := requireStringField(obj, "opening_message_id")
	opening, ok := ic.messages[openingID]
	if err != nil || !ok {
		return messageRequestImportScope{}, fmt.Errorf("opening message %q is not present in this archive", openingID)
	}
	if opening.realmID != realmID || opening.fromAgentID != coordinatorID || opening.kind != "open_request" ||
		opening.audienceKind != MessageRecipientRealm || opening.replyToMessageID != "" {
		return messageRequestImportScope{}, fmt.Errorf("opening message %q has invalid request routing", openingID)
	}
	policy, err := requireStringField(obj, "selection_policy")
	if err != nil || policy != "client_ranked" {
		return messageRequestImportScope{}, fmt.Errorf("selection_policy is invalid")
	}
	maxAssignees, ok := importedPositiveInteger(obj["max_assignees"])
	if !ok || maxAssignees > 8 {
		return messageRequestImportScope{}, fmt.Errorf("max_assignees is invalid")
	}
	offerWindow, ok := importedPositiveInteger(obj["offer_window_seconds"])
	if !ok || offerWindow > 900 {
		return messageRequestImportScope{}, fmt.Errorf("offer_window_seconds is invalid")
	}
	expiresIn, ok := importedPositiveInteger(obj["expires_in_seconds"])
	if !ok || expiresIn < 2 || expiresIn > 604800 || expiresIn <= offerWindow {
		return messageRequestImportScope{}, fmt.Errorf("expires_in_seconds is invalid")
	}
	selectionGeneration, ok := importedGeneration(obj["selection_generation"], true)
	if !ok || selectionGeneration > maxMessageRequestSelectionHistory {
		return messageRequestImportScope{}, fmt.Errorf("selection_generation is invalid")
	}
	createdAt, err := requireImportedTimestamp(obj, "created_at")
	if err != nil {
		return messageRequestImportScope{}, err
	}
	if err := ic.requireTimestampAtOrBeforeExport("agent_message_requests created_at", *createdAt); err != nil {
		return messageRequestImportScope{}, err
	}
	offerDeadline, err := requireImportedTimestamp(obj, "offer_deadline")
	if err != nil {
		return messageRequestImportScope{}, err
	}
	expiresAt, err := requireImportedTimestamp(obj, "expires_at")
	if err != nil {
		return messageRequestImportScope{}, err
	}
	if !createdAt.Equal(opening.createdAt) ||
		!offerDeadline.Equal(opening.createdAt.Add(time.Duration(offerWindow)*time.Second)) ||
		!expiresAt.Equal(opening.createdAt.Add(time.Duration(expiresIn)*time.Second)) {
		return messageRequestImportScope{}, fmt.Errorf("request deadlines are invalid")
	}
	// Deadlines are prospective schedules, so a portable open request may
	// legitimately expire after export. Their exact derivation from a bounded
	// creation time plus the protocol-capped 15-minute/7-day durations prevents
	// an attacker from smuggling an unbounded future lifetime.
	updatedAt, err := requireImportedTimestamp(obj, "updated_at")
	if err != nil {
		return messageRequestImportScope{}, err
	}
	if err := ic.requireTimestampAtOrBeforeExport("agent_message_requests updated_at", *updatedAt); err != nil {
		return messageRequestImportScope{}, err
	}
	completedAt, hasCompleted, err := importedOptionalTimestamp(obj, "completed_at")
	if err != nil {
		return messageRequestImportScope{}, err
	}
	cancelledAt, hasCancelled, err := importedOptionalTimestamp(obj, "cancelled_at")
	if err != nil {
		return messageRequestImportScope{}, err
	}
	expiredAt, hasExpired, err := importedOptionalTimestamp(obj, "expired_at")
	if err != nil {
		return messageRequestImportScope{}, err
	}
	for field, value := range map[string]*time.Time{
		"completed_at": completedAt,
		"cancelled_at": cancelledAt,
		"expired_at":   expiredAt,
	} {
		if value != nil {
			if err := ic.requireTimestampAtOrBeforeExport("agent_message_requests "+field, *value); err != nil {
				return messageRequestImportScope{}, err
			}
		}
	}
	state, err := requireStringField(obj, "state")
	if err != nil {
		return messageRequestImportScope{}, fmt.Errorf("state is invalid")
	}
	validLifecycle := state == "open" && !hasCompleted && !hasCancelled && !hasExpired ||
		state == "completed" && hasCompleted && !hasCancelled && !hasExpired ||
		state == "cancelled" && !hasCompleted && hasCancelled && !hasExpired ||
		state == "expired" && !hasCompleted && !hasCancelled && hasExpired
	if !validLifecycle {
		return messageRequestImportScope{}, fmt.Errorf("state lifecycle is invalid")
	}
	if state == MessageRequestStateOpen && !ic.liveAgents[coordinatorID] {
		return messageRequestImportScope{}, fmt.Errorf("open request coordinator %q is deleted", coordinatorID)
	}
	return messageRequestImportScope{
		realmID: realmID, openingMessageID: openingID,
		coordinatorAgentID: coordinatorID, state: state,
		maxAssignees: maxAssignees, selectionGeneration: selectionGeneration,
	}, nil
}

func (ic *importCtx) validateImportedMessageRequestCandidate(
	obj map[string]any,
) (messageRequestCandidateImportKey, messageRequestCandidateImportScope, error) {
	requestID, err := requireStringField(obj, "request_id")
	request, ok := ic.messageRequests[requestID]
	if err != nil || !ok {
		return messageRequestCandidateImportKey{}, messageRequestCandidateImportScope{}, fmt.Errorf("request %q is not present in this archive", requestID)
	}
	realmID, err := requireStringField(obj, "realm_id")
	if err != nil || realmID != request.realmID {
		return messageRequestCandidateImportKey{}, messageRequestCandidateImportScope{}, fmt.Errorf("realm does not match request %q", requestID)
	}
	agentID, err := requireStringField(obj, "agent_id")
	opening := ic.messages[request.openingMessageID]
	if err != nil || !ic.agents[agentID] || ic.agentRealms[agentID] != realmID ||
		agentID == request.coordinatorAgentID || !opening.recipientAgentIDs[agentID] {
		return messageRequestCandidateImportKey{}, messageRequestCandidateImportScope{}, fmt.Errorf("candidate %q is outside request audience", agentID)
	}
	state, err := requireStringField(obj, "response_state")
	if err != nil {
		return messageRequestCandidateImportKey{}, messageRequestCandidateImportScope{}, fmt.Errorf("response_state is invalid")
	}
	offerID, hasOffer, err := importedNullableNonemptyString(obj, "offer_message_id")
	if err != nil {
		return messageRequestCandidateImportKey{}, messageRequestCandidateImportScope{}, err
	}
	offerKey, keyOK := obj["offer_key_hash"].(string)
	offerHash, hashOK := obj["offer_request_hash"].(string)
	_, hasResponded, err := importedOptionalTimestamp(obj, "responded_at")
	if err != nil {
		return messageRequestCandidateImportKey{}, messageRequestCandidateImportScope{}, err
	}
	if _, err := requireImportedTimestamp(obj, "created_at"); err != nil {
		return messageRequestCandidateImportKey{}, messageRequestCandidateImportScope{}, err
	}
	switch state {
	case "pending":
		if hasOffer || !keyOK || offerKey != "" || !hashOK || offerHash != "" || hasResponded {
			return messageRequestCandidateImportKey{}, messageRequestCandidateImportScope{}, fmt.Errorf("pending response shape is invalid")
		}
		if request.state == MessageRequestStateOpen && !ic.liveAgents[agentID] {
			return messageRequestCandidateImportKey{}, messageRequestCandidateImportScope{}, fmt.Errorf("pending candidate %q is deleted", agentID)
		}
	case "declined":
		if hasOffer || !keyOK || offerKey != "" || !hashOK || offerHash != "" || !hasResponded {
			return messageRequestCandidateImportKey{}, messageRequestCandidateImportScope{}, fmt.Errorf("declined response shape is invalid")
		}
	case "offered":
		offer, exists := ic.messages[offerID]
		if !hasOffer || !exists || !keyOK || !isSHA256Hex(offerKey) || !hashOK ||
			!isSHA256Hex(offerHash) || !hasResponded {
			return messageRequestCandidateImportKey{}, messageRequestCandidateImportScope{}, fmt.Errorf("offered response shape is invalid")
		}
		if offer.realmID != realmID || offer.kind != "offer" || offer.audienceKind != MessageRecipientAgent ||
			offer.fromAgentID != agentID || offer.toAgentID != request.coordinatorAgentID ||
			offer.replyToMessageID != request.openingMessageID || offer.threadID != opening.threadID {
			return messageRequestCandidateImportKey{}, messageRequestCandidateImportScope{}, fmt.Errorf("offer message %q has invalid routing", offerID)
		}
	default:
		return messageRequestCandidateImportKey{}, messageRequestCandidateImportScope{}, fmt.Errorf("response_state is invalid")
	}
	return messageRequestCandidateImportKey{requestID: requestID, agentID: agentID},
		messageRequestCandidateImportScope{responseState: state, offerMessageID: offerID}, nil
}

func (ic *importCtx) validateImportedMessageRequestSelection(
	obj map[string]any,
) (string, messageRequestSelectionImportScope, error) {
	selectionID, err := requireStringField(obj, "id")
	if err != nil || !validImportedGeneratedID(selectionID, "msel") {
		return "", messageRequestSelectionImportScope{}, fmt.Errorf("id is invalid")
	}
	requestID, err := requireStringField(obj, "request_id")
	request, ok := ic.messageRequests[requestID]
	if err != nil || !ok {
		return "", messageRequestSelectionImportScope{}, fmt.Errorf("request %q is not present in this archive", requestID)
	}
	realmID, err := requireStringField(obj, "realm_id")
	coordinatorID, coordinatorErr := requireStringField(obj, "coordinator_agent_id")
	if err != nil || coordinatorErr != nil || realmID != request.realmID || coordinatorID != request.coordinatorAgentID {
		return "", messageRequestSelectionImportScope{}, fmt.Errorf("selection scope does not match request %q", requestID)
	}
	generation, ok := importedGeneration(obj["generation"], false)
	if !ok || generation > maxMessageRequestSelectionHistory || generation > request.selectionGeneration {
		return "", messageRequestSelectionImportScope{}, fmt.Errorf("generation is invalid")
	}
	for _, existing := range ic.messageRequestSelections {
		if existing.requestID == requestID && existing.generation == generation {
			return "", messageRequestSelectionImportScope{}, fmt.Errorf("request %q repeats generation %d", requestID, generation)
		}
	}
	keyHash, keyOK := obj["idempotency_key_hash"].(string)
	selectionHash, selectionOK := obj["selection_hash"].(string)
	if !keyOK || !isSHA256Hex(keyHash) || !selectionOK || !isSHA256Hex(selectionHash) {
		return "", messageRequestSelectionImportScope{}, fmt.Errorf("selection hashes are invalid")
	}
	if _, err := requireImportedTimestamp(obj, "created_at"); err != nil {
		return "", messageRequestSelectionImportScope{}, err
	}
	return selectionID, messageRequestSelectionImportScope{
		requestID: requestID, realmID: realmID,
		coordinatorAgentID: coordinatorID, generation: generation,
	}, nil
}

func validateImportedMessageRequestClaimContent(obj map[string]any) (messageRequestClaimImportScope, error) {
	claimID, err := requireStringField(obj, "id")
	if err != nil || !validImportedGeneratedID(claimID, "mrc") {
		return messageRequestClaimImportScope{}, fmt.Errorf("id is invalid")
	}
	requestID, err := requireStringField(obj, "request_id")
	if err != nil || !validImportedGeneratedID(requestID, "mrq") {
		return messageRequestClaimImportScope{}, fmt.Errorf("request_id is invalid")
	}
	selectionID, err := requireStringField(obj, "selection_id")
	if err != nil || !validImportedGeneratedID(selectionID, "msel") {
		return messageRequestClaimImportScope{}, fmt.Errorf("selection_id is invalid")
	}
	realmID, err := requireStringField(obj, "realm_id")
	if err != nil {
		return messageRequestClaimImportScope{}, fmt.Errorf("realm_id is invalid")
	}
	agentID, err := requireStringField(obj, "agent_id")
	if err != nil {
		return messageRequestClaimImportScope{}, fmt.Errorf("agent_id is invalid")
	}
	state, err := requireStringField(obj, "state")
	if err != nil {
		return messageRequestClaimImportScope{}, fmt.Errorf("state is invalid")
	}
	generation, ok := importedGeneration(obj["generation"], true)
	if !ok {
		return messageRequestClaimImportScope{}, fmt.Errorf("generation is invalid")
	}
	failureCount, ok := importedGeneration(obj["failure_count"], true)
	if !ok {
		return messageRequestClaimImportScope{}, fmt.Errorf("failure_count is invalid")
	}
	_ = failureCount
	claimKey, keyOK := obj["claim_key_hash"].(string)
	completeKey, completeOK := obj["complete_key_hash"].(string)
	resultID, hasResult, err := importedNullableNonemptyString(obj, "result_message_id")
	if err != nil || !keyOK || !completeOK {
		return messageRequestClaimImportScope{}, fmt.Errorf("claim receipt fields are invalid")
	}
	_, hasLease, err := importedOptionalTimestamp(obj, "lease_expires_at")
	if err != nil {
		return messageRequestClaimImportScope{}, err
	}
	_, hasClaimed, err := importedOptionalTimestamp(obj, "claimed_at")
	if err != nil {
		return messageRequestClaimImportScope{}, err
	}
	_, hasReleased, err := importedOptionalTimestamp(obj, "released_at")
	if err != nil {
		return messageRequestClaimImportScope{}, err
	}
	_, hasCompleted, err := importedOptionalTimestamp(obj, "completed_at")
	if err != nil {
		return messageRequestClaimImportScope{}, err
	}
	_, hasCancelled, err := importedOptionalTimestamp(obj, "cancelled_at")
	if err != nil {
		return messageRequestClaimImportScope{}, err
	}
	if _, err := requireImportedTimestamp(obj, "selected_at"); err != nil {
		return messageRequestClaimImportScope{}, err
	}
	if _, err := requireImportedTimestamp(obj, "updated_at"); err != nil {
		return messageRequestClaimImportScope{}, err
	}
	valid := false
	switch state {
	case "reserved":
		valid = generation == 0 && claimKey == "" && hasLease && !hasClaimed &&
			!hasReleased && !hasCompleted && !hasCancelled && completeKey == "" && !hasResult
	case "claimed":
		valid = generation >= 1 && isSHA256Hex(claimKey) && hasLease && hasClaimed &&
			!hasReleased && !hasCompleted && !hasCancelled && completeKey == "" && !hasResult
	case "released":
		valid = generation >= 1 && isSHA256Hex(claimKey) && !hasLease && hasClaimed &&
			hasReleased && !hasCompleted && !hasCancelled && completeKey == "" && !hasResult
	case "completed":
		valid = generation >= 1 && isSHA256Hex(claimKey) && !hasLease && hasClaimed &&
			!hasReleased && hasCompleted && !hasCancelled && isSHA256Hex(completeKey) &&
			hasResult && validImportedGeneratedID(resultID, "msg")
	case "cancelled":
		valid = !hasLease && !hasReleased && !hasCompleted && hasCancelled &&
			completeKey == "" && !hasResult &&
			(generation == 0 && claimKey == "" && !hasClaimed ||
				generation >= 1 && isSHA256Hex(claimKey) && hasClaimed)
	}
	if !valid {
		return messageRequestClaimImportScope{}, fmt.Errorf("%s claim shape is invalid", state)
	}
	return messageRequestClaimImportScope{
		requestID: requestID, selectionID: selectionID, realmID: realmID,
		agentID: agentID, state: state, generation: generation,
		resultMessageID: resultID,
	}, nil
}

func (ic *importCtx) validateImportedMessageRequestClaim(
	obj map[string]any,
) (string, messageRequestClaimImportScope, error) {
	scope, err := validateImportedMessageRequestClaimContent(obj)
	if err != nil {
		return "", messageRequestClaimImportScope{}, err
	}
	claimID, _ := requireStringField(obj, "id")
	request, requestOK := ic.messageRequests[scope.requestID]
	selection, selectionOK := ic.messageRequestSelections[scope.selectionID]
	if !requestOK || !selectionOK || selection.requestID != scope.requestID ||
		scope.realmID != request.realmID || selection.realmID != request.realmID {
		return "", messageRequestClaimImportScope{}, fmt.Errorf("claim scope does not match its request and selection")
	}
	if !ic.agents[scope.agentID] || ic.agentRealms[scope.agentID] != scope.realmID {
		return "", messageRequestClaimImportScope{}, fmt.Errorf("agent %q is outside claim realm", scope.agentID)
	}
	candidate, ok := ic.messageRequestCandidates[messageRequestCandidateImportKey{
		requestID: scope.requestID, agentID: scope.agentID,
	}]
	if !ok || candidate.responseState != "offered" {
		return "", messageRequestClaimImportScope{}, fmt.Errorf("agent %q has no offer for request %q", scope.agentID, scope.requestID)
	}
	for _, existing := range ic.messageRequestClaims {
		if existing.selectionID == scope.selectionID && existing.agentID == scope.agentID {
			return "", messageRequestClaimImportScope{}, fmt.Errorf("selection %q repeats agent %q", scope.selectionID, scope.agentID)
		}
	}
	if scope.state == "completed" {
		result, ok := ic.messages[scope.resultMessageID]
		opening := ic.messages[request.openingMessageID]
		if !ok || result.realmID != request.realmID || result.kind != "result" ||
			result.audienceKind != MessageRecipientAgent ||
			result.fromAgentID != scope.agentID || result.toAgentID != request.coordinatorAgentID ||
			result.replyToMessageID != request.openingMessageID || result.threadID != opening.threadID {
			return "", messageRequestClaimImportScope{}, fmt.Errorf("result message %q has invalid routing", scope.resultMessageID)
		}
	}
	return claimID, scope, nil
}

func validateImportedMessageRequestGraph(ic *importCtx) error {
	candidateCounts := make(map[string]int64)
	for key := range ic.messageRequestCandidates {
		candidateCounts[key.requestID]++
	}
	selectionCounts := make(map[string]int64)
	maxSelectionGeneration := make(map[string]int64)
	claimCounts := make(map[string]int64)
	completedClaimCounts := make(map[string]int64)
	for selectionID, selection := range ic.messageRequestSelections {
		selectionCounts[selection.requestID]++
		if selection.generation > maxSelectionGeneration[selection.requestID] {
			maxSelectionGeneration[selection.requestID] = selection.generation
		}
		for _, claim := range ic.messageRequestClaims {
			if claim.selectionID == selectionID {
				claimCounts[selectionID]++
			}
		}
	}
	for _, claim := range ic.messageRequestClaims {
		if claim.state == MessageRequestClaimCompleted {
			completedClaimCounts[claim.requestID]++
		}
	}
	for requestID, request := range ic.messageRequests {
		opening := ic.messages[request.openingMessageID]
		if candidateCounts[requestID] != int64(len(opening.recipientAgentIDs)) ||
			candidateCounts[requestID] < 1 || candidateCounts[requestID] > maxMessageAudienceRecipients {
			return fmt.Errorf("request %q candidate snapshot does not match its opening audience", requestID)
		}
		if maxSelectionGeneration[requestID] != request.selectionGeneration {
			return fmt.Errorf("request %q selection generation does not match its selection history", requestID)
		}
		if selectionCounts[requestID] != request.selectionGeneration {
			return fmt.Errorf("request %q selection history is not contiguous", requestID)
		}
		if request.selectionGeneration == 0 && selectionCounts[requestID] != 0 {
			return fmt.Errorf("request %q has selections at generation zero", requestID)
		}
		completed := completedClaimCounts[requestID]
		if completed > request.maxAssignees {
			return fmt.Errorf("request %q has more completed results than its capacity", requestID)
		}
		if request.state == MessageRequestStateCompleted && completed == 0 {
			return fmt.Errorf("completed request %q does not have a result", requestID)
		}
	}
	for selectionID, selection := range ic.messageRequestSelections {
		request := ic.messageRequests[selection.requestID]
		if claimCounts[selectionID] < 1 || claimCounts[selectionID] > request.maxAssignees {
			return fmt.Errorf("selection %q has %d claims outside request capacity", selectionID, claimCounts[selectionID])
		}
	}
	return nil
}

func validateImportedMessageProcessingLinks(messages map[string]messageImportScope, deliveries map[string]messageDeliveryImportScope) error {
	seenResults := make(map[string]string)
	for deliveryKey, delivery := range deliveries {
		if delivery.processingState != MessageProcessingCompleted {
			continue
		}
		messageID := delivery.messageID
		if messageID == "" {
			messageID = deliveryKey
		}
		result, ok := messages[delivery.resultMessageID]
		if !ok {
			return fmt.Errorf("message %q references missing processing result %q", messageID, delivery.resultMessageID)
		}
		source := messages[messageID]
		recipientAgentID := delivery.recipientAgentID
		if recipientAgentID == "" {
			recipientAgentID = source.toAgentID
		}
		if result.replyToMessageID != messageID || result.realmID != source.realmID ||
			result.threadID != source.threadID || result.fromAgentID != recipientAgentID ||
			result.toAgentID != source.fromAgentID {
			return fmt.Errorf("message %q has an invalid processing result %q", messageID, delivery.resultMessageID)
		}
		if prior, duplicate := seenResults[delivery.resultMessageID]; duplicate {
			return fmt.Errorf("processing result %q is linked by messages %q and %q", delivery.resultMessageID, prior, messageID)
		}
		seenResults[delivery.resultMessageID] = messageID
	}
	return nil
}

func validateImportedMemorySupersessionMembership(
	memories map[string]memoryImportScope,
	versions map[memoryVersionImportKey]memoryVersionImportScope,
) error {
	type setActivity struct {
		active   int64
		reverted int64
	}
	activities := map[memorySupersessionImportSet]setActivity{}
	for key, version := range versions {
		if !version.expectsSupersessionRelations {
			continue
		}
		observed := int64(len(version.supersessionMembers))
		activity := activities[version.supersessionSet]
		activity.active += version.activeSupersessionMemberCount
		activity.reverted += observed - version.activeSupersessionMemberCount
		activities[version.supersessionSet] = activity
		if observed > version.supersessionReplacementCount {
			return fmt.Errorf(
				"memory %q version %d has %d replacement relations, receipt commits to %d",
				key.memoryID, key.version, observed, version.supersessionReplacementCount,
			)
		}
		if observed == version.supersessionReplacementCount {
			if memorySupersessionMembershipDigest(version.supersessionMembers) !=
				version.supersessionReplacementDigest {
				return fmt.Errorf("memory %q version %d replacement digest does not match its receipt",
					key.memoryID, key.version)
			}
			continue
		}

		// A smaller set is a legitimate post-delete archive only after the
		// original set was reverted. Keeping any surviving active edge, or
		// importing the incomplete receipt as the current head, would create a
		// live partial supersession. Historical incomplete receipts remain
		// portable but can only fail closed on an exact retry.
		memory := memories[key.memoryID]
		if version.activeSupersessionMemberCount != 0 || memory.currentVersion == key.version {
			return fmt.Errorf(
				"memory %q version %d has an incomplete live replacement set",
				key.memoryID, key.version,
			)
		}
	}
	for set, activity := range activities {
		if activity.active > 0 && activity.reverted > 0 {
			return fmt.Errorf(
				"supersession set %q revision %d mixes active and reverted edges",
				set.id, set.revision,
			)
		}
	}
	for memoryID, memory := range memories {
		if memory.deleted {
			continue
		}
		head := versions[memoryVersionImportKey{memoryID: memoryID, version: memory.currentVersion}]
		requireCompleteActive := func(version memoryVersionImportScope) error {
			if !version.expectsSupersessionRelations ||
				int64(len(version.supersessionMembers)) != version.supersessionReplacementCount ||
				version.activeSupersessionMemberCount != version.supersessionReplacementCount {
				return fmt.Errorf("memory %q current superseded lineage is not complete and active", memoryID)
			}
			return nil
		}
		switch {
		case head.state == MemoryStateSuperseded:
			if err := requireCompleteActive(head); err != nil {
				return err
			}
		case head.state == MemoryStateForgotten && head.priorState == MemoryStateSuperseded:
			previous := versions[memoryVersionImportKey{
				memoryID: memoryID, version: head.previousVersion,
			}]
			if err := requireCompleteActive(previous); err != nil {
				return err
			}
		case (head.state == MemoryStateActive ||
			head.state == MemoryStateForgotten && head.priorState == MemoryStateActive) &&
			head.supersessionReplacementCount > 0:
			if activities[head.supersessionSet].active > 0 {
				return fmt.Errorf("memory %q active lineage retains active supersession edges", memoryID)
			}
		}
	}
	return nil
}

func validateImportedMemoryCurationGraph(ic *importCtx) error {
	maxRequestByOwner := map[memoryOwnerImportKey]int64{}
	maxFenceByOwner := map[memoryOwnerImportKey]int64{}
	for requestID, request := range ic.memoryCurationRequests {
		if request.requestGeneration > maxRequestByOwner[request.owner] {
			maxRequestByOwner[request.owner] = request.requestGeneration
		}
		if request.claimedRunID != "" {
			run, ok := ic.memoryCurationRuns[request.claimedRunID]
			if !ok || run.requestID != requestID || run.owner != request.owner {
				return fmt.Errorf("request %q claimed run is outside its owner scope", requestID)
			}
			if request.normalizedClaimed && !run.normalizedInterrupted {
				return fmt.Errorf("request %q active run was not interrupted on import", requestID)
			}
		}
		if request.replayRunID != "" {
			run, ok := ic.memoryCurationRuns[request.replayRunID]
			if !ok || run.owner != request.owner || run.state != "rolled_back" || run.scrubbed {
				return fmt.Errorf("request %q replay run is outside its owner scope", requestID)
			}
		}
	}
	for runID, run := range ic.memoryCurationRuns {
		if run.fencingGeneration > maxFenceByOwner[run.owner] {
			maxFenceByOwner[run.owner] = run.fencingGeneration
		}
		if run.observedInputs != run.inputCount ||
			run.observedByKind["memory"] != run.memoryInputCount ||
			run.observedByKind["evidence"] != run.evidenceInputCount ||
			run.observedByKind["transcript"] != run.transcriptInputCount ||
			run.observedByKind["cursor"] != run.cursorInputCount {
			return fmt.Errorf("run %q materialized input counts do not match its receipt", runID)
		}
	}
	for owner, lane := range ic.memoryCurationLanes {
		if lane.requestGeneration != maxRequestByOwner[owner] {
			return fmt.Errorf("owner lane request generation %d does not match imported requests %d",
				lane.requestGeneration, maxRequestByOwner[owner])
		}
		expectedFence := maxFenceByOwner[owner]
		if lane.normalizedActive {
			run, ok := ic.memoryCurationRuns[lane.activeRunID]
			if !ok || run.owner != owner || !run.normalizedInterrupted ||
				run.fencingGeneration+1 != lane.fencingGeneration {
				return fmt.Errorf("owner lane active run was not coherently interrupted")
			}
			expectedFence++
		} else if lane.fencingGeneration == expectedFence+1 {
			// Expiry, cancel/abandon, and apply-conflict transitions consume
			// the active fence when they release the lane. Prove the maximum
			// imported run is one of those terminal outcomes before accepting
			// the otherwise-idle +1 generation.
			terminalFence := false
			for _, run := range ic.memoryCurationRuns {
				if run.owner == owner && run.fencingGeneration == expectedFence &&
					(run.state == "abandoned" || run.state == "interrupted" || run.state == "conflict") {
					terminalFence = true
					break
				}
			}
			if !terminalFence {
				return fmt.Errorf("owner lane idle fence advance has no terminal run")
			}
			expectedFence++
		}
		if lane.fencingGeneration != expectedFence {
			return fmt.Errorf("owner lane fencing generation %d does not match imported runs %d",
				lane.fencingGeneration, expectedFence)
		}
	}
	for actionID, action := range ic.memoryCurationActions {
		run := ic.memoryCurationRuns[action.runID]
		valid := true
		switch run.state {
		case "open":
			valid = action.state == "draft"
		case "planned", "conflict", "abandoned", "interrupted":
			valid = action.state == "validated"
		case "applied":
			valid = action.state == "applied"
		case "rolled_back":
			valid = action.state == "reverted"
		}
		if !valid {
			return fmt.Errorf("action %q state %q is inconsistent with run state %q",
				actionID, action.state, run.state)
		}
	}
	for runID, run := range ic.memoryCurationRuns {
		actions := make([]MemoryCurationPlanAction, run.observedActions)
		scrubbedActions := int64(0)
		for _, action := range ic.memoryCurationActions {
			if action.runID != runID {
				continue
			}
			if action.scrubbed {
				scrubbedActions++
				continue
			}
			if action.ordinal < 1 || action.ordinal > int64(len(actions)) {
				return fmt.Errorf("run %q action ordinal is outside its plan", runID)
			}
			actions[action.ordinal-1] = action.action
		}
		if run.scrubbed {
			if scrubbedActions != run.observedActions {
				return fmt.Errorf("run %q scrub state does not cover every action", runID)
			}
			continue
		}
		if scrubbedActions != 0 {
			return fmt.Errorf("run %q contains an independently scrubbed action", runID)
		}
		if run.planRevision == 0 {
			if run.observedActions != 0 {
				return fmt.Errorf("unplanned run %q contains actions", runID)
			}
			continue
		}
		for index := range actions {
			if actions[index].Ordinal != int64(index+1) {
				return fmt.Errorf("run %q plan action sequence is incomplete", runID)
			}
		}
		plan := MemoryCurationPlan{
			Schema: run.planSchema, PlanRevision: run.planRevision, Actions: actions,
		}
		canonical, err := canonicalMemoryCurationJSON(plan)
		if err != nil {
			return fmt.Errorf("run %q plan cannot be canonicalized", runID)
		}
		sum := sha256.Sum256(canonical)
		if run.planHash != hex.EncodeToString(sum[:]) {
			return fmt.Errorf("run %q plan hash does not match its action rows", runID)
		}
	}
	validateAttribution := func(runID, actionID string, owner memoryOwnerImportKey) error {
		run, runOK := ic.memoryCurationRuns[runID]
		action, actionOK := ic.memoryCurationActions[actionID]
		if !runOK || !actionOK || run.owner != owner || action.owner != owner || action.runID != runID {
			return fmt.Errorf("curation run/action attribution is outside its owner scope")
		}
		return nil
	}
	for _, version := range ic.memoryVersions {
		if version.curationRunID == "" {
			continue
		}
		if err := validateAttribution(version.curationRunID, version.curationActionID, version.owner); err != nil {
			return err
		}
	}
	for _, relation := range ic.memoryRelationCurations {
		if relation.runID != "" {
			if err := validateAttribution(relation.runID, relation.actionID, relation.owner); err != nil {
				return err
			}
		}
		if relation.revertedByRunID != "" {
			if err := validateAttribution(relation.revertedByRunID, relation.revertedByActionID, relation.owner); err != nil {
				return err
			}
		}
	}
	for candidateID, attribution := range ic.factCandidateCurations {
		action, ok := ic.memoryCurationActions[attribution.actionID]
		if !ok || action.owner != attribution.owner || action.runID != attribution.runID ||
			action.primitive != "propose_fact" {
			return fmt.Errorf("fact candidate %q curation attribution is invalid", candidateID)
		}
		if attribution.status == "withdrawn" && action.state != "reverted" {
			return fmt.Errorf("withdrawn fact candidate %q does not belong to a reverted action", candidateID)
		}
		if action.state == "reverted" && attribution.status != "withdrawn" && attribution.status != "rejected" {
			return fmt.Errorf("reverted fact candidate %q is neither withdrawn nor independently rejected", candidateID)
		}
	}
	for actionID, action := range ic.memoryCurationActions {
		if action.scrubbed {
			continue
		}
		if err := validateImportedMemoryCurationActionResources(ic, action); err != nil {
			return fmt.Errorf("action %q result provenance: %v", actionID, err)
		}
	}
	return nil
}

func validateImportedMemoryCurationActionResources(
	ic *importCtx,
	action memoryCurationActionImportScope,
) error {
	if action.appliedResult == nil {
		if action.rollbackResult != nil {
			return fmt.Errorf("rollback result exists without an apply result")
		}
		return nil
	}
	applied := *action.appliedResult
	if err := validateImportedCurationResultUniqueIDs(applied); err != nil {
		return err
	}
	version := func(ref MemoryVersionReference, produced, compensation bool) (memoryVersionImportScope, error) {
		if !validMemoryID(ref.MemoryID) || ref.Version < 1 {
			return memoryVersionImportScope{}, fmt.Errorf("invalid memory version reference")
		}
		got, ok := ic.memoryVersions[memoryVersionImportKey{memoryID: ref.MemoryID, version: ref.Version}]
		if !ok || got.owner != action.owner {
			return memoryVersionImportScope{}, fmt.Errorf("memory version %s/%d is outside the action owner", ref.MemoryID, ref.Version)
		}
		if produced && (got.curationRunID != action.runID || got.curationActionID != action.id) {
			return memoryVersionImportScope{}, fmt.Errorf("memory version %s/%d lacks exact action attribution", ref.MemoryID, ref.Version)
		}
		if compensation && got.operation != "reverted" {
			return memoryVersionImportScope{}, fmt.Errorf("compensation head %s/%d is not a reverted version", ref.MemoryID, ref.Version)
		}
		return got, nil
	}
	for _, ref := range applied.BeforeHeads {
		if _, err := version(ref, false, false); err != nil {
			return err
		}
	}
	for _, ref := range applied.AfterHeads {
		if _, err := version(ref, true, false); err != nil {
			return err
		}
	}
	for _, memoryID := range applied.CreatedMemoryIDs {
		memory, ok := ic.memories[memoryID]
		created, versionOK := ic.memoryVersions[memoryVersionImportKey{memoryID: memoryID, version: 1}]
		if !ok || memory.owner != action.owner || !versionOK ||
			created.curationRunID != action.runID || created.curationActionID != action.id {
			return fmt.Errorf("created memory %q lacks exact action attribution", memoryID)
		}
	}
	for _, evidenceID := range applied.EvidenceIDs {
		evidence, ok := ic.memoryEvidence[evidenceID]
		produced := ic.memoryVersions[evidence.target]
		if !ok || produced.owner != action.owner ||
			produced.curationRunID != action.runID || produced.curationActionID != action.id {
			return fmt.Errorf("evidence %q is not attached to this action output", evidenceID)
		}
	}
	for _, relationID := range applied.RelationIDs {
		relation, ok := ic.memoryRelations[relationID]
		if !ok || relation.owner != action.owner || relation.runID != action.runID || relation.actionID != action.id {
			return fmt.Errorf("relation %q lacks exact action attribution", relationID)
		}
	}
	for _, candidateID := range applied.CandidateIDs {
		candidate, ok := ic.factCandidateCurations[candidateID]
		if !ok || candidate.owner != action.owner || candidate.runID != action.runID || candidate.actionID != action.id {
			return fmt.Errorf("fact candidate %q lacks exact action attribution", candidateID)
		}
	}
	if err := validateImportedCurationAppliedShape(ic, action, applied); err != nil {
		return err
	}

	if action.rollbackResult == nil {
		return nil
	}
	rollback := *action.rollbackResult
	if err := validateImportedCurationRollbackUniqueIDs(rollback); err != nil {
		return err
	}
	for _, ref := range rollback.CompensationHeads {
		if _, err := version(ref, true, true); err != nil {
			return err
		}
	}
	expectedWithdrawnCandidates := make([]string, 0, len(applied.CandidateIDs))
	for _, candidateID := range applied.CandidateIDs {
		switch ic.factCandidateCurations[candidateID].status {
		case "withdrawn":
			expectedWithdrawnCandidates = append(expectedWithdrawnCandidates, candidateID)
		case "rejected":
			// A human rejection is already terminal and is never rewritten to
			// make a later curation rollback look like the rejecting authority.
		default:
			return fmt.Errorf("rolled-back fact candidate %q has a live state", candidateID)
		}
	}
	if !sameStringSlice(rollback.RevertedRelationIDs, applied.RelationIDs) ||
		!sameStringSlice(rollback.WithdrawnCandidateIDs, expectedWithdrawnCandidates) {
		return fmt.Errorf("rollback resources do not match the apply receipt")
	}
	for _, relationID := range rollback.RevertedRelationIDs {
		relation := ic.memoryRelations[relationID]
		if relation.revertedByRunID != action.runID || relation.revertedByActionID != action.id {
			return fmt.Errorf("relation %q lacks exact rollback attribution", relationID)
		}
	}
	for _, candidateID := range rollback.WithdrawnCandidateIDs {
		if ic.factCandidateCurations[candidateID].status != "withdrawn" {
			return fmt.Errorf("fact candidate %q was not withdrawn", candidateID)
		}
	}
	var expectedCompensation []MemoryVersionReference
	switch action.primitive {
	case MemoryCurationOperationCreate:
		expectedCompensation = []MemoryVersionReference{{
			MemoryID: action.action.Create.MemoryID, Version: applied.AfterHeads[0].Version + 1,
		}}
	case MemoryCurationOperationReplace, MemoryCurationOperationSupersede:
		expectedCompensation = []MemoryVersionReference{{
			MemoryID: applied.AfterHeads[0].MemoryID, Version: applied.AfterHeads[0].Version + 1,
		}}
	default:
		expectedCompensation = []MemoryVersionReference{}
	}
	if !sameMemoryVersionReferenceSlice(rollback.CompensationHeads, expectedCompensation) {
		return fmt.Errorf("rollback compensation heads do not match the applied action")
	}
	return nil
}

func validateImportedCurationAppliedShape(
	ic *importCtx,
	action memoryCurationActionImportScope,
	result MemoryCurationActionApplyResult,
) error {
	empty := func(values ...int) bool {
		for _, value := range values {
			if value != 0 {
				return false
			}
		}
		return true
	}
	switch action.primitive {
	case MemoryCurationOperationCreate:
		create := action.action.Create
		if len(result.BeforeHeads) != 0 || len(result.AfterHeads) != 1 ||
			len(result.CreatedMemoryIDs) != 1 || result.CreatedMemoryIDs[0] != create.MemoryID ||
			result.AfterHeads[0] != (MemoryVersionReference{MemoryID: create.MemoryID, Version: 1}) ||
			len(result.EvidenceIDs) != len(create.Snapshot.Evidence) ||
			len(result.RelationIDs) != len(create.Relations) ||
			!empty(len(result.CandidateIDs), len(result.SupersessionReplacementIDs)) ||
			result.SupersessionSetID != "" || result.SupersessionSetRevision != 0 {
			return fmt.Errorf("create applied_result shape is invalid")
		}
		for index, relationID := range result.RelationIDs {
			relation := ic.memoryRelations[relationID]
			want := create.Relations[index]
			if relation.relationType != want.RelationType || relation.from != result.AfterHeads[0] ||
				relation.to != importedMemoryCurationVersionReference(want.To) {
				return fmt.Errorf("create relation %q does not match the plan", relationID)
			}
		}
	case MemoryCurationOperationReplace:
		replace := action.action.Replace
		before := MemoryVersionReference{MemoryID: replace.Target.MemoryID, Version: replace.Target.ExpectedVersion}
		after := MemoryVersionReference{MemoryID: before.MemoryID, Version: before.Version + 1}
		if !sameMemoryVersionReferenceSlice(result.BeforeHeads, []MemoryVersionReference{before}) ||
			!sameMemoryVersionReferenceSlice(result.AfterHeads, []MemoryVersionReference{after}) ||
			len(result.EvidenceIDs) != len(replace.Snapshot.Evidence) ||
			!empty(len(result.CreatedMemoryIDs), len(result.RelationIDs), len(result.CandidateIDs), len(result.SupersessionReplacementIDs)) ||
			result.SupersessionSetID != "" || result.SupersessionSetRevision != 0 {
			return fmt.Errorf("replace applied_result shape is invalid")
		}
	case MemoryCurationOperationSupersede:
		supersede := action.action.Supersede
		before := MemoryVersionReference{MemoryID: supersede.Target.MemoryID, Version: supersede.Target.ExpectedVersion}
		after := MemoryVersionReference{MemoryID: before.MemoryID, Version: before.Version + 1}
		replacements := make([]MemoryVersionReference, len(supersede.Replacements))
		for index := range supersede.Replacements {
			replacements[index] = importedMemoryCurationVersionReference(supersede.Replacements[index])
		}
		if !sameMemoryVersionReferenceSlice(result.BeforeHeads, []MemoryVersionReference{before}) ||
			!sameMemoryVersionReferenceSlice(result.AfterHeads, []MemoryVersionReference{after}) ||
			!sameMemoryVersionReferenceSlice(result.SupersessionReplacementIDs, replacements) ||
			len(result.RelationIDs) != len(supersede.Replacements) ||
			!empty(len(result.CreatedMemoryIDs), len(result.EvidenceIDs), len(result.CandidateIDs)) ||
			!validCurationID(result.SupersessionSetID, "mset") || result.SupersessionSetRevision != 1 {
			return fmt.Errorf("supersede applied_result shape is invalid")
		}
		for index, relationID := range result.RelationIDs {
			relation := ic.memoryRelations[relationID]
			if relation.relationType != "supersedes" || relation.from != replacements[index] || relation.to != after {
				return fmt.Errorf("supersede relation %q does not match the plan", relationID)
			}
		}
	case MemoryCurationOperationRelate:
		relate := action.action.Relate
		if len(result.RelationIDs) != 1 ||
			!empty(len(result.BeforeHeads), len(result.AfterHeads), len(result.CreatedMemoryIDs), len(result.EvidenceIDs), len(result.CandidateIDs), len(result.SupersessionReplacementIDs)) ||
			result.SupersessionSetID != "" || result.SupersessionSetRevision != 0 {
			return fmt.Errorf("relate applied_result shape is invalid")
		}
		relation := ic.memoryRelations[result.RelationIDs[0]]
		if relation.relationType != relate.RelationType ||
			relation.from != importedMemoryCurationVersionReference(relate.From) ||
			relation.to != importedMemoryCurationVersionReference(relate.To) {
			return fmt.Errorf("relate result does not match the plan")
		}
	case MemoryCurationOperationProposeFact:
		if len(result.CandidateIDs) != 1 ||
			!empty(len(result.BeforeHeads), len(result.AfterHeads), len(result.CreatedMemoryIDs), len(result.EvidenceIDs), len(result.RelationIDs), len(result.SupersessionReplacementIDs)) ||
			result.SupersessionSetID != "" || result.SupersessionSetRevision != 0 {
			return fmt.Errorf("propose_fact applied_result shape is invalid")
		}
	default:
		return fmt.Errorf("unknown action primitive")
	}
	return nil
}

func validateImportedCurationResultUniqueIDs(result MemoryCurationActionApplyResult) error {
	seen := map[string]bool{}
	for _, values := range [][]string{
		result.CreatedMemoryIDs, result.EvidenceIDs, result.RelationIDs, result.CandidateIDs,
	} {
		for _, value := range values {
			if value == "" || seen[value] {
				return fmt.Errorf("applied_result contains an empty or duplicate resource id")
			}
			seen[value] = true
		}
	}
	if hasDuplicateMemoryVersionReference(result.BeforeHeads) ||
		hasDuplicateMemoryVersionReference(result.AfterHeads) ||
		hasDuplicateMemoryVersionReference(result.SupersessionReplacementIDs) {
		return fmt.Errorf("applied_result contains duplicate memory versions")
	}
	return nil
}

func validateImportedCurationRollbackUniqueIDs(result MemoryCurationActionRollbackResult) error {
	seen := map[string]bool{}
	for _, values := range [][]string{result.RevertedRelationIDs, result.WithdrawnCandidateIDs} {
		for _, value := range values {
			if value == "" || seen[value] {
				return fmt.Errorf("rollback_result contains an empty or duplicate resource id")
			}
			seen[value] = true
		}
	}
	if hasDuplicateMemoryVersionReference(result.CompensationHeads) {
		return fmt.Errorf("rollback_result contains duplicate compensation heads")
	}
	return nil
}

func hasDuplicateMemoryVersionReference(values []MemoryVersionReference) bool {
	seen := map[MemoryVersionReference]bool{}
	for _, value := range values {
		if seen[value] {
			return true
		}
		seen[value] = true
	}
	return false
}

func sameMemoryVersionReferenceSlice(a, b []MemoryVersionReference) bool {
	if len(a) != len(b) {
		return false
	}
	for index := range a {
		if a[index] != b[index] {
			return false
		}
	}
	return true
}

func importedMemoryCurationVersionReference(value MemoryCurationVersionReference) MemoryVersionReference {
	return MemoryVersionReference{MemoryID: value.MemoryID, Version: value.Version}
}

func sameStringSlice(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for index := range a {
		if a[index] != b[index] {
			return false
		}
	}
	return true
}

// validateImportedMemoryVersionTransition keeps a checksum-valid hostile
// archive from relabelling arbitrary snapshots as lifecycle operations. The
// schema-29 API produces one initial added/active version, then only these
// state transitions. Future-only curation rollback/reverted transitions remain
// invalid until a later archive schema can require their provenance.
func validateImportedMemoryVersionTransition(
	version, changeSeq int64,
	payloadDigest string,
	state, priorState, operation string,
	previous memoryVersionImportScope,
) error {
	if version == 1 {
		if operation != "added" || state != MemoryStateActive || priorState != "" {
			return errors.New("initial version must be added and active without prior_state")
		}
		return nil
	}
	if changeSeq <= previous.changeSeq {
		return errors.New("change_seq must increase across contiguous versions")
	}

	valid := false
	switch operation {
	case "adjusted":
		valid = previous.state == MemoryStateActive &&
			state == MemoryStateActive && priorState == ""
	case "superseded":
		valid = previous.state == MemoryStateActive &&
			state == MemoryStateSuperseded && priorState == ""
	case "forgotten":
		valid = (previous.state == MemoryStateActive || previous.state == MemoryStateSuperseded) &&
			state == MemoryStateForgotten && priorState == previous.state
	case "restored":
		valid = previous.state == MemoryStateForgotten &&
			(previous.priorState == MemoryStateActive || previous.priorState == MemoryStateSuperseded) &&
			state == previous.priorState && priorState == ""
	case "reactivated":
		valid = (previous.state == MemoryStateSuperseded || previous.state == "reverted") &&
			state == MemoryStateActive && priorState == ""
	case "reverted":
		valid = ((previous.state == MemoryStateActive && state == "reverted") ||
			(previous.state == MemoryStateSuperseded && state == MemoryStateActive) ||
			(previous.state == MemoryStateActive && state == MemoryStateActive)) &&
			priorState == ""
	}
	if !valid {
		return fmt.Errorf(
			"illegal %s transition from state %q (prior_state %q) to state %q (prior_state %q)",
			operation, previous.state, previous.priorState, state, priorState,
		)
	}
	if operation == "adjusted" || operation == "reverted" &&
		previous.state == MemoryStateActive && state == MemoryStateActive {
		if payloadDigest == previous.payloadDigest {
			return fmt.Errorf("%s version does not change the semantic payload", operation)
		}
	} else if payloadDigest != previous.payloadDigest {
		return fmt.Errorf("%s version changes the semantic payload", operation)
	}
	return nil
}

// validImportedGeneratedID proves the exact representation emitted by id.New:
// a fixed prefix and 80 random bits encoded as 16 lowercase, unpadded base32
// characters. Prefix-only validation would let a tombstone carry payload text
// in the otherwise value-free deletion receipt id.
func validImportedGeneratedID(value, prefix string) bool {
	body := strings.TrimPrefix(value, prefix+"_")
	if body == value || len(body) != 16 {
		return false
	}
	for _, char := range body {
		if (char < 'a' || char > 'z') && (char < '2' || char > '7') {
			return false
		}
	}
	return true
}

func validImportedMemoryRetryShieldKind(kind string) bool {
	switch kind {
	case "idempotency.added",
		"idempotency.adjusted",
		"idempotency.superseded",
		"idempotency.forgotten",
		"idempotency.restored",
		"idempotency.reactivated",
		"idempotency.evidence_resolution":
		return true
	default:
		return false
	}
}

func validateImportedMemoryRetryShields(memoryID string, memory memoryImportScope, imported []memoryDeleteRetryShield) error {
	shields := append([]memoryDeleteRetryShield(nil), imported...)
	sort.Slice(shields, func(i, j int) bool {
		if shields[i].Kind != shields[j].Kind {
			return shields[i].Kind < shields[j].Kind
		}
		return shields[i].Hash < shields[j].Hash
	})
	if int64(len(shields)) != memory.deletedRetryShieldCount ||
		memoryDeleteRetryShieldDigest(shields) != memory.deletedRetryShieldDigest {
		return fmt.Errorf(
			"%w: permanently deleted memory %q retry shields do not match its receipt",
			ErrArchiveContent, memoryID,
		)
	}
	return nil
}

// decodeImportRow preserves the exact spelling of every JSON number. In
// particular, BIGINT counters such as change_seq must not pass through the
// default interface{} float64 representation, which loses integer precision
// above 2^53. Requiring EOF retains json.Unmarshal's one-value contract.
func decodeImportRow(row []byte) (map[string]any, error) {
	if err := rejectDuplicateJSONNames(row); err != nil {
		return nil, fmt.Errorf("duplicate JSON member: %w", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(row))
	decoder.UseNumber()

	var obj map[string]any
	if err := decoder.Decode(&obj); err != nil {
		return nil, err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return nil, errors.New("multiple JSON values")
		}
		return nil, err
	}
	if obj == nil {
		return nil, errors.New("expected JSON object")
	}
	return obj, nil
}

func validateImportedFactMutationTombstoneCompleteness(facts map[string]factImportScope, counts map[string]int64) error {
	for factID, scope := range facts {
		got := counts[factID]
		want := scope.deletedMutationKeyCount
		if got != want {
			return fmt.Errorf("fact %q carries %d retry tombstones, receipt commits to %d", factID, got, want)
		}
	}
	return nil
}

// validateImportedFactReplacementTopology proves that each fact address is one
// linear history. A checksummed but hostile archive must not be able to create
// duplicate unrecreated tombstones (which wedge explicit recreation), a cycle
// (which removes the required tail), or disconnected histories (which let an
// ordinary set bypass a deletion guard).
func validateImportedFactReplacementTopology(facts map[string]factImportScope) error {
	type address struct {
		realmID, ownerAgentID, subjectID, predicate string
	}
	groups := map[address]map[string]factImportScope{}
	for factID, scope := range facts {
		key := address{scope.realmID, scope.ownerAgentID, scope.subjectID, scope.predicate}
		if groups[key] == nil {
			groups[key] = map[string]factImportScope{}
		}
		groups[key][factID] = scope
	}
	for _, nodes := range groups {
		activeID := ""
		incoming := map[string]int{}
		for factID, scope := range nodes {
			if !scope.deleted {
				if activeID != "" {
					return fmt.Errorf("address has multiple active facts %q and %q", activeID, factID)
				}
				activeID = factID
			}
			if scope.replacementFactID != "" {
				replacement, ok := nodes[scope.replacementFactID]
				if !ok {
					return fmt.Errorf("fact %q replacement %q is outside its address", factID, scope.replacementFactID)
				}
				if scope.sensitive && !replacement.sensitive {
					return fmt.Errorf("sensitive fact %q has non-sensitive replacement %q", factID, scope.replacementFactID)
				}
				incoming[scope.replacementFactID]++
				if incoming[scope.replacementFactID] > 1 {
					return fmt.Errorf("fact %q has multiple replacement predecessors", scope.replacementFactID)
				}
			}
		}

		tailID := activeID
		if tailID == "" {
			for factID, scope := range nodes {
				if scope.replacementFactID == "" {
					if tailID != "" {
						return fmt.Errorf("address has multiple unrecreated tombstones %q and %q", tailID, factID)
					}
					tailID = factID
				}
			}
			if tailID == "" {
				return fmt.Errorf("deleted address has no unrecreated tail")
			}
		}

		for start := range nodes {
			seen := map[string]bool{}
			current := start
			for nodes[current].replacementFactID != "" {
				if seen[current] {
					return fmt.Errorf("replacement cycle contains fact %q", current)
				}
				seen[current] = true
				current = nodes[current].replacementFactID
			}
			if current != tailID {
				return fmt.Errorf("fact %q replacement chain ends at %q, want %q", start, current, tailID)
			}
		}
	}
	return nil
}

// validateImportedFactDecisionAssertions proves that every persisted confirm
// retry points at the exact assertion promoted by that candidate. A foreign
// key to the same fact is insufficient: a content-hostile but correctly
// checksummed archive could otherwise repoint the retry to an older assertion
// and make a restored confirm return the wrong historical result.
func validateImportedFactDecisionAssertions(ctx context.Context, q factQuerier, accountID string) error {
	var mismatch bool
	err := q.QueryRow(ctx, `
		WITH RECURSIVE fact_chain(fact_id, assertion_id) AS (
		  SELECT id, resolved_assertion_id
		  FROM facts
		  WHERE account_id = $1
		  UNION
		  SELECT chain.fact_id, assertion.supersedes_id
		  FROM fact_chain chain
		  JOIN fact_assertions assertion ON assertion.id = chain.assertion_id
		  WHERE assertion.supersedes_id IS NOT NULL
		)
		SELECT EXISTS (
		  SELECT 1
		  FROM fact_candidates candidate
		  LEFT JOIN fact_assertions assertion ON assertion.id = candidate.decision_assertion_id
		  LEFT JOIN facts fact ON fact.id = candidate.resolved_fact_id
		  LEFT JOIN fact_subjects subject ON subject.id = fact.subject_id
		  WHERE candidate.account_id = $1
		    AND candidate.decision_assertion_id IS NOT NULL
		    AND (
		      candidate.status <> 'confirmed'
		      OR assertion.id IS NULL
		      OR fact.id IS NULL
		      OR assertion.fact_id IS DISTINCT FROM fact.id
		      OR assertion.account_id IS DISTINCT FROM candidate.account_id
		      OR assertion.realm_id IS DISTINCT FROM candidate.realm_id
		      OR assertion.asserted_by_agent_id IS DISTINCT FROM candidate.owner_agent_id
		      OR fact.account_id IS DISTINCT FROM candidate.account_id
		      OR fact.realm_id IS DISTINCT FROM candidate.realm_id
		      OR fact.owner_agent_id IS DISTINCT FROM candidate.owner_agent_id
		      OR subject.canonical_key IS DISTINCT FROM candidate.subject_key
		      OR fact.predicate IS DISTINCT FROM candidate.predicate
		      OR assertion.value_type IS DISTINCT FROM candidate.value_type
		      OR assertion.value IS DISTINCT FROM candidate.value
		      OR assertion.recurrence IS DISTINCT FROM candidate.recurrence
		      OR assertion.source_kind IS DISTINCT FROM 'inference'
		      OR assertion.source_ref IS DISTINCT FROM candidate.source_ref
		      OR assertion.confidence IS DISTINCT FROM candidate.confidence
		      OR assertion.observed_at IS DISTINCT FROM candidate.observed_at
		      OR assertion.confirmed_at IS NULL
		      OR assertion.valid_from IS DISTINCT FROM candidate.valid_from
		      OR assertion.valid_until IS DISTINCT FROM candidate.valid_until
		      OR assertion.supersedes_id IS DISTINCT FROM candidate.observed_assertion_id
		      OR NOT EXISTS (
		        SELECT 1 FROM fact_chain chain
		        WHERE chain.fact_id = fact.id
		          AND chain.assertion_id = assertion.id
		      )
		    )
		)`, accountID).Scan(&mismatch)
	if err != nil {
		return fmt.Errorf("validate imported fact decision assertions: %w", err)
	}
	if mismatch {
		return fmt.Errorf("%w: confirmed fact candidate does not match its decision assertion", ErrArchiveContent)
	}
	return nil
}

// validateImportedUsageRollups prevents a stale or edited projection from
// landing beside an otherwise valid event ledger. date_bin uses a fixed UTC
// epoch, matching usageBucketStart without depending on the DB session zone.
func validateImportedUsageRollups(ctx context.Context, tx pgx.Tx, accountID string) error {
	var mismatch bool
	err := tx.QueryRow(ctx, `
		WITH expected AS (
		  SELECT account_id, realm_id, agent_id, dimension, unit,
		         'hour'::text AS bucket,
		         date_bin('1 hour', occurred_at, '1970-01-01 00:00:00+00'::timestamptz) AS bucket_start,
		         sum(quantity)::bigint AS quantity, count(*)::bigint AS event_count
		  FROM usage_events WHERE account_id = $1
		  GROUP BY account_id, realm_id, agent_id, dimension, unit, bucket_start
		  UNION ALL
		  SELECT account_id, realm_id, agent_id, dimension, unit,
		         'day'::text AS bucket,
		         date_bin('1 day', occurred_at, '1970-01-01 00:00:00+00'::timestamptz) AS bucket_start,
		         sum(quantity)::bigint AS quantity, count(*)::bigint AS event_count
		  FROM usage_events WHERE account_id = $1
		  GROUP BY account_id, realm_id, agent_id, dimension, unit, bucket_start
		), actual AS (
		  SELECT account_id, realm_id, agent_id, dimension, unit, bucket,
		         bucket_start, quantity, event_count
		  FROM usage_rollups WHERE account_id = $1
		)
		SELECT EXISTS (
		  SELECT 1 FROM expected e
		  FULL OUTER JOIN actual a
		    USING (account_id, realm_id, agent_id, dimension, unit, bucket, bucket_start)
		  WHERE e.account_id IS NULL OR a.account_id IS NULL
		     OR e.quantity <> a.quantity OR e.event_count <> a.event_count
		)`, accountID).Scan(&mismatch)
	if err != nil {
		return fmt.Errorf("validate imported usage rollups: %w", err)
	}
	if mismatch {
		return fmt.Errorf("%w: usage rollups do not match usage events", ErrArchiveContent)
	}
	return nil
}

// insertProjected inserts one row using ONLY the columns the archive
// actually carries, so columns the archive omits take their destination
// DEFAULT — the additive-migration contract in export/upgrade.go. The set
// of legal column names per table is fixed at compile time (importColumns);
// any JSON key outside it is refused, so no attacker-chosen identifier
// reaches the SQL text.
//
// Concurrent same-account imports collide on the accounts primary-key
// insert: the loser's INSERT blocks on the winner's uncommitted tuple, and
// on the winner's commit fails with unique_violation (23505). That maps to
// ErrAccountExists here — a clean 409 for the retry — instead of falling to
// the generic 500 arm.
func insertProjected(ctx context.Context, tx pgxExec, table string, obj map[string]any, raw []byte) error {
	allowed := importColumns[table]
	keys := make([]string, 0, len(obj))
	for k := range obj {
		if !allowed[k] {
			return fmt.Errorf("%w: %s row has unknown column %q", ErrArchiveContent, table, k)
		}
		keys = append(keys, k)
	}
	sort.Strings(keys) // deterministic SQL text, useful for logs
	if len(keys) == 0 {
		return fmt.Errorf("%w: empty %s row", ErrArchiveContent, table)
	}
	colList := strings.Join(keys, ", ")
	// INSERT INTO t (c1,c2) SELECT c1,c2 FROM jsonb_populate_record(NULL::t, $1)
	// projects only the columns present in the JSON. Unlisted columns take
	// their DEFAULT — new NOT NULL DEFAULT columns land correctly without
	// needing an upgrader; new nullable-with-default columns land at their
	// default instead of the silent NULL a full-record insert would leave.
	stmt := fmt.Sprintf(
		`INSERT INTO %s (%s) SELECT %s FROM jsonb_populate_record(NULL::%s, $1::jsonb)`,
		table, colList, colList, table)
	if _, err := tx.Exec(ctx, stmt, raw); err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" && table == "accounts" {
			// A concurrent import (or a retry racing the winner) landed
			// this account first. Surface it as the same 409 an early
			// EXISTS-check would give.
			return ErrAccountExists
		}
		return fmt.Errorf("import %s row: %w", table, err)
	}
	return nil
}

// pgxExec is the minimal Exec surface insertProjected needs from a pgx.Tx —
// declared here so the helper can be unit-tested with an in-memory fake
// without pulling in a live database.
type pgxExec interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}
