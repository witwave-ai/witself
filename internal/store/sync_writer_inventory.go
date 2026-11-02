package store

import (
	"crypto/sha256"
	"slices"
	"strings"
)

// SyncCoverageManifestVersion identifies the reviewed table inventory. It does
// not certify mixed binaries, database roles, or snapshot materialization.
const SyncCoverageManifestVersion = "lc-s6c1-v1"

// syncWriterTables is the single fixed lock order for installation and coverage.
// It includes cell-local tables when they nevertheless carry account mutations.
// Indirect account ownership is resolved by the guard for agents and activity.
var syncWriterTables = []string{
	"account_evacuation_finalizations",
	"account_events",
	"account_provision_receipts",
	"accounts",
	"agent_activity",
	"agent_avatar_activations",
	"agent_avatar_profiles",
	"agent_avatar_rejections",
	"agent_avatar_resets",
	"agent_avatar_versions",
	"agent_dashboard_preferences",
	"agent_email_account_rate_buckets",
	"agent_email_address_domains",
	"agent_email_addresses",
	"agent_email_custom_domain_routes",
	"agent_email_deliveries",
	"agent_email_mailboxes",
	"agent_email_messages",
	"agent_email_outbound_messages",
	"agent_email_outbound_provider_events",
	"agent_email_outbound_rate_buckets",
	"agent_email_outbound_recipient_suppressions",
	"agent_email_rate_buckets",
	"agent_email_realm_aliases",
	"agent_email_realm_receive_controls",
	"agent_email_realm_send_controls",
	"agent_email_retention_account_scan_state",
	"agent_email_retry_canary_arms",
	"agent_email_send_controls",
	"agent_message_deliveries",
	"agent_message_rate_buckets",
	"agent_message_request_candidates",
	"agent_message_request_claims",
	"agent_message_request_selections",
	"agent_message_requests",
	"agent_messages",
	"agent_vault_key_enrollments",
	"agent_vault_key_rotation_items",
	"agent_vault_key_rotations",
	"agent_vault_keys",
	"agents",
	"avatar_mutation_receipts",
	"avatar_style_pack_versions",
	"avatar_style_packs",
	"avatar_style_rollout_jobs",
	"fact_assertions",
	"fact_candidates",
	"fact_mutation_tombstones",
	"fact_subjects",
	"facts",
	"memories",
	"memory_change_clocks",
	"memory_curation_actions",
	"memory_curation_cursors",
	"memory_curation_lanes",
	"memory_curation_mutations",
	"memory_curation_requests",
	"memory_curation_run_inputs",
	"memory_curation_runs",
	"memory_deleted_references",
	"memory_evidence",
	"memory_relations",
	"memory_vector_profiles",
	"memory_vectors",
	"memory_versions",
	"message_retention_account_scan_state",
	"message_retention_thread_activity",
	"operators",
	"realm_avatar_styles",
	"realms",
	"secret_deks",
	"secret_fields",
	"secret_mutation_receipts",
	"secrets",
	"support_ticket_messages",
	"support_tickets",
	"tokens",
	"transcript_conversations",
	"transcript_entries",
	"transcript_retention_account_scan_state",
	"transcript_storage_usage",
	"usage_events",
	"usage_rollups",
	"vault_key_enrollment_receipts",
	"vault_key_rotation_receipts",
}

// SyncWriterInventory returns a copy so callers cannot change the lock order.
func SyncWriterInventory() []string { return slices.Clone(syncWriterTables) }

// SyncWriterCoverageCommitment commits to the version and exact ordered names.
func SyncWriterCoverageCommitment() [32]byte {
	return sha256.Sum256([]byte(SyncCoverageManifestVersion + "\n" + strings.Join(syncWriterTables, "\n") + "\n"))
}

// Exclusions are explicit and reconciled with both namespaces and custody
// registries. Protocol journals cannot recursively journal their own writes;
// their direct-principal privilege boundary is a separate LC-S6C2 obligation.
var syncWriterExclusions = map[string]string{
	"account_purge_sweep_state":          "cell-wide coordination has no account column or account-owned parent",
	"agent_email_cell_storage_capacity":  "cell-wide coordination has no account column or account-owned parent",
	"agent_email_retention_worker_lanes": "cell-wide coordination has no account column or account-owned parent",
	"message_retention_worker_lanes":     "cell-wide coordination has no account column or account-owned parent",
	"transcript_retention_sweep_state":   "cell-wide coordination has no account column or account-owned parent",
	"transcript_retention_worker_lanes":  "cell-wide coordination has no account column or account-owned parent",
	"sync_incarnations":                  "sync protocol control or evidence machinery; recursive domain capture is forbidden",
	"sync_operations":                    "sync protocol control or evidence machinery; recursive domain capture is forbidden",
	"sync_operation_payloads":            "sync protocol control or evidence machinery; recursive domain capture is forbidden",
	"sync_verifier_history":              "sync protocol control or evidence machinery; recursive domain capture is forbidden",
	"sync_receipts":                      "sync protocol control or evidence machinery; recursive domain capture is forbidden",
	"sync_reuse_refusals":                "sync protocol control or evidence machinery; recursive domain capture is forbidden",
	"sync_capture":                       "sync protocol control or evidence machinery; recursive domain capture is forbidden",
	"sync_writer_capabilities":           "sync protocol control or evidence machinery; recursive domain capture is forbidden",
	"sync_tx_admissions":                 "sync protocol control or evidence machinery; recursive domain capture is forbidden",
	"sync_tx_touches":                    "sync protocol control or evidence machinery; recursive domain capture is forbidden",
	"sync_finalized_counter":             "sync protocol control or evidence machinery; recursive domain capture is forbidden",
	"sync_bundles":                       "sync protocol control or evidence machinery; recursive domain capture is forbidden",
	"sync_bundle_operations":             "sync protocol control or evidence machinery; recursive domain capture is forbidden",
	"sync_publication_outbox":            "sync protocol control or evidence machinery; recursive domain capture is forbidden",
	"sync_acceptance_counter":            "sync protocol control or evidence machinery; recursive domain capture is forbidden",
	"sync_resource_budgets":              "sync protocol control or evidence machinery; recursive domain capture is forbidden",
	"sync_resource_reservations":         "sync protocol control or evidence machinery; recursive domain capture is forbidden",
	"sync_deletion_knowledge":            "sync protocol control or evidence machinery; recursive domain capture is forbidden",
	"sync_retained_evidence":             "sync protocol control or evidence machinery; recursive domain capture is forbidden",
	"sync_evidence_pins":                 "sync protocol control or evidence machinery; recursive domain capture is forbidden",
	"sync_payload_retirements":           "sync protocol control or evidence machinery; recursive domain capture is forbidden",
}
