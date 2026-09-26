-- +goose NO TRANSACTION
-- +goose Up
-- Account archives keep their established row ordering. These account-prefixed
-- indexes avoid sorting wide JSON payloads before the first chunk (#555).
-- Keep the remaining account archive indexes separate from the hot-table
-- migration so retries here preserve its completed builds.
-- Rebuild on retry so an interrupted concurrent build cannot leave an invalid index.
DROP INDEX CONCURRENTLY IF EXISTS operators_export_order;
CREATE INDEX CONCURRENTLY operators_export_order ON operators (account_id, id);

DROP INDEX CONCURRENTLY IF EXISTS agent_email_addresses_export_order;
CREATE INDEX CONCURRENTLY agent_email_addresses_export_order ON agent_email_addresses (account_id, realm_id, created_at, id);

DROP INDEX CONCURRENTLY IF EXISTS agent_email_realm_aliases_export_order;
CREATE INDEX CONCURRENTLY agent_email_realm_aliases_export_order ON agent_email_realm_aliases (account_id, realm_id, domain, realm_label, claim_id);

DROP INDEX CONCURRENTLY IF EXISTS agent_email_custom_domain_routes_export_order;
CREATE INDEX CONCURRENTLY agent_email_custom_domain_routes_export_order ON agent_email_custom_domain_routes (account_id, realm_id, domain, realm_label, domain_request_id, realm_alias_claim_id);

-- The existing mailbox index has a DESC time/id suffix; with only account_id
-- fixed, it cannot supply this all-ASC archive order without incremental sorting.
DROP INDEX CONCURRENTLY IF EXISTS agent_email_messages_export_order;
CREATE INDEX CONCURRENTLY agent_email_messages_export_order ON agent_email_messages (account_id, realm_id, mailbox_id, received_at, id);

DROP INDEX CONCURRENTLY IF EXISTS agent_email_retry_canary_arms_export_order;
CREATE INDEX CONCURRENTLY agent_email_retry_canary_arms_export_order ON agent_email_retry_canary_arms (account_id, realm_id, owner_agent_id, accepted_at, challenge_sha256, mailbox_id);

-- The existing owner index likewise has a DESC time/id suffix.
DROP INDEX CONCURRENTLY IF EXISTS agent_email_outbound_messages_export_order;
CREATE INDEX CONCURRENTLY agent_email_outbound_messages_export_order ON agent_email_outbound_messages (account_id, realm_id, owner_agent_id, created_at, id);

DROP INDEX CONCURRENTLY IF EXISTS agent_vault_keys_export_order;
CREATE INDEX CONCURRENTLY agent_vault_keys_export_order ON agent_vault_keys (account_id, realm_id, owner_agent_id, key_version, id);

DROP INDEX CONCURRENTLY IF EXISTS agent_vault_key_enrollments_export_order;
CREATE INDEX CONCURRENTLY agent_vault_key_enrollments_export_order ON agent_vault_key_enrollments (account_id, realm_id, owner_agent_id, created_at, id);

DROP INDEX CONCURRENTLY IF EXISTS secrets_export_order;
CREATE INDEX CONCURRENTLY secrets_export_order ON secrets (account_id, realm_id, owner_agent_id, created_at, id);

DROP INDEX CONCURRENTLY IF EXISTS agent_vault_key_rotations_export_order;
CREATE INDEX CONCURRENTLY agent_vault_key_rotations_export_order ON agent_vault_key_rotations (account_id, realm_id, owner_agent_id, created_at, id);

DROP INDEX CONCURRENTLY IF EXISTS agent_vault_key_rotation_items_export_order;
CREATE INDEX CONCURRENTLY agent_vault_key_rotation_items_export_order ON agent_vault_key_rotation_items (account_id, realm_id, owner_agent_id, rotation_id, dek_id);

DROP INDEX CONCURRENTLY IF EXISTS agent_avatar_rejections_export_order;
CREATE INDEX CONCURRENTLY agent_avatar_rejections_export_order ON agent_avatar_rejections (account_id, rejected_at, id);

DROP INDEX CONCURRENTLY IF EXISTS agent_dashboard_preferences_export_order;
CREATE INDEX CONCURRENTLY agent_dashboard_preferences_export_order ON agent_dashboard_preferences (account_id, agent_id);

DROP INDEX CONCURRENTLY IF EXISTS fact_subjects_export_order;
CREATE INDEX CONCURRENTLY fact_subjects_export_order ON fact_subjects (account_id, id);

DROP INDEX CONCURRENTLY IF EXISTS facts_export_order;
CREATE INDEX CONCURRENTLY facts_export_order ON facts (account_id, id);

DROP INDEX CONCURRENTLY IF EXISTS fact_mutation_tombstones_export_order;
CREATE INDEX CONCURRENTLY fact_mutation_tombstones_export_order ON fact_mutation_tombstones (account_id, fact_id, surface, id);

DROP INDEX CONCURRENTLY IF EXISTS fact_candidates_export_order;
CREATE INDEX CONCURRENTLY fact_candidates_export_order ON fact_candidates (account_id, proposed_at, id);

DROP INDEX CONCURRENTLY IF EXISTS tokens_export_order;
CREATE INDEX CONCURRENTLY tokens_export_order ON tokens (account_id, id);

DROP INDEX CONCURRENTLY IF EXISTS transcript_conversations_export_order;
CREATE INDEX CONCURRENTLY transcript_conversations_export_order ON transcript_conversations (account_id, created_at, id);

DROP INDEX CONCURRENTLY IF EXISTS agent_messages_export_order;
CREATE INDEX CONCURRENTLY agent_messages_export_order ON agent_messages (account_id, created_at, id);

DROP INDEX CONCURRENTLY IF EXISTS agent_message_deliveries_export_order;
CREATE INDEX CONCURRENTLY agent_message_deliveries_export_order ON agent_message_deliveries (account_id, created_at, message_id, recipient_agent_id);

DROP INDEX CONCURRENTLY IF EXISTS agent_message_requests_export_order;
CREATE INDEX CONCURRENTLY agent_message_requests_export_order ON agent_message_requests (account_id, created_at, id);

DROP INDEX CONCURRENTLY IF EXISTS agent_message_request_candidates_export_order;
CREATE INDEX CONCURRENTLY agent_message_request_candidates_export_order ON agent_message_request_candidates (account_id, request_id, agent_id);

DROP INDEX CONCURRENTLY IF EXISTS agent_message_request_selections_export_order;
CREATE INDEX CONCURRENTLY agent_message_request_selections_export_order ON agent_message_request_selections (account_id, request_id, generation, id);

DROP INDEX CONCURRENTLY IF EXISTS agent_message_request_claims_export_order;
CREATE INDEX CONCURRENTLY agent_message_request_claims_export_order ON agent_message_request_claims (account_id, request_id, selection_id, agent_id, id);

DROP INDEX CONCURRENTLY IF EXISTS memories_export_order;
CREATE INDEX CONCURRENTLY memories_export_order ON memories (account_id, created_at, id);

DROP INDEX CONCURRENTLY IF EXISTS memory_versions_export_order;
CREATE INDEX CONCURRENTLY memory_versions_export_order ON memory_versions (account_id, memory_id, version);

DROP INDEX CONCURRENTLY IF EXISTS memory_vector_profiles_export_order;
CREATE INDEX CONCURRENTLY memory_vector_profiles_export_order ON memory_vector_profiles (account_id, created_at, id);

DROP INDEX CONCURRENTLY IF EXISTS memory_vectors_export_order;
CREATE INDEX CONCURRENTLY memory_vectors_export_order ON memory_vectors (account_id, profile_id, memory_id, memory_version);

DROP INDEX CONCURRENTLY IF EXISTS memory_evidence_export_order;
CREATE INDEX CONCURRENTLY memory_evidence_export_order ON memory_evidence (account_id, memory_id, target_version, evidence_change_seq, id);

DROP INDEX CONCURRENTLY IF EXISTS memory_relations_export_order;
CREATE INDEX CONCURRENTLY memory_relations_export_order ON memory_relations (account_id, created_at, id);

DROP INDEX CONCURRENTLY IF EXISTS memory_deleted_references_export_order;
CREATE INDEX CONCURRENTLY memory_deleted_references_export_order ON memory_deleted_references (account_id, deleted_memory_id, created_at, id);

DROP INDEX CONCURRENTLY IF EXISTS memory_curation_requests_export_order;
CREATE INDEX CONCURRENTLY memory_curation_requests_export_order ON memory_curation_requests (account_id, created_at, id);

DROP INDEX CONCURRENTLY IF EXISTS memory_curation_runs_export_order;
CREATE INDEX CONCURRENTLY memory_curation_runs_export_order ON memory_curation_runs (account_id, created_at, id);

DROP INDEX CONCURRENTLY IF EXISTS memory_curation_run_inputs_export_order;
CREATE INDEX CONCURRENTLY memory_curation_run_inputs_export_order ON memory_curation_run_inputs (account_id, run_id, ordinal);

DROP INDEX CONCURRENTLY IF EXISTS memory_curation_actions_export_order;
CREATE INDEX CONCURRENTLY memory_curation_actions_export_order ON memory_curation_actions (account_id, run_id, ordinal);

DROP INDEX CONCURRENTLY IF EXISTS memory_curation_mutations_export_order;
CREATE INDEX CONCURRENTLY memory_curation_mutations_export_order ON memory_curation_mutations (account_id, created_at, id);

-- +goose Down
DROP INDEX CONCURRENTLY IF EXISTS memory_curation_mutations_export_order;
DROP INDEX CONCURRENTLY IF EXISTS memory_curation_actions_export_order;
DROP INDEX CONCURRENTLY IF EXISTS memory_curation_run_inputs_export_order;
DROP INDEX CONCURRENTLY IF EXISTS memory_curation_runs_export_order;
DROP INDEX CONCURRENTLY IF EXISTS memory_curation_requests_export_order;
DROP INDEX CONCURRENTLY IF EXISTS memory_deleted_references_export_order;
DROP INDEX CONCURRENTLY IF EXISTS memory_relations_export_order;
DROP INDEX CONCURRENTLY IF EXISTS memory_evidence_export_order;
DROP INDEX CONCURRENTLY IF EXISTS memory_vectors_export_order;
DROP INDEX CONCURRENTLY IF EXISTS memory_vector_profiles_export_order;
DROP INDEX CONCURRENTLY IF EXISTS memory_versions_export_order;
DROP INDEX CONCURRENTLY IF EXISTS memories_export_order;
DROP INDEX CONCURRENTLY IF EXISTS agent_message_request_claims_export_order;
DROP INDEX CONCURRENTLY IF EXISTS agent_message_request_selections_export_order;
DROP INDEX CONCURRENTLY IF EXISTS agent_message_request_candidates_export_order;
DROP INDEX CONCURRENTLY IF EXISTS agent_message_requests_export_order;
DROP INDEX CONCURRENTLY IF EXISTS agent_message_deliveries_export_order;
DROP INDEX CONCURRENTLY IF EXISTS agent_messages_export_order;
DROP INDEX CONCURRENTLY IF EXISTS transcript_conversations_export_order;
DROP INDEX CONCURRENTLY IF EXISTS tokens_export_order;
DROP INDEX CONCURRENTLY IF EXISTS fact_candidates_export_order;
DROP INDEX CONCURRENTLY IF EXISTS fact_mutation_tombstones_export_order;
DROP INDEX CONCURRENTLY IF EXISTS facts_export_order;
DROP INDEX CONCURRENTLY IF EXISTS fact_subjects_export_order;
DROP INDEX CONCURRENTLY IF EXISTS agent_dashboard_preferences_export_order;
DROP INDEX CONCURRENTLY IF EXISTS agent_avatar_rejections_export_order;
DROP INDEX CONCURRENTLY IF EXISTS agent_vault_key_rotation_items_export_order;
DROP INDEX CONCURRENTLY IF EXISTS agent_vault_key_rotations_export_order;
DROP INDEX CONCURRENTLY IF EXISTS secrets_export_order;
DROP INDEX CONCURRENTLY IF EXISTS agent_vault_key_enrollments_export_order;
DROP INDEX CONCURRENTLY IF EXISTS agent_vault_keys_export_order;
DROP INDEX CONCURRENTLY IF EXISTS agent_email_outbound_messages_export_order;
DROP INDEX CONCURRENTLY IF EXISTS agent_email_retry_canary_arms_export_order;
DROP INDEX CONCURRENTLY IF EXISTS agent_email_messages_export_order;
DROP INDEX CONCURRENTLY IF EXISTS agent_email_custom_domain_routes_export_order;
DROP INDEX CONCURRENTLY IF EXISTS agent_email_realm_aliases_export_order;
DROP INDEX CONCURRENTLY IF EXISTS agent_email_addresses_export_order;
DROP INDEX CONCURRENTLY IF EXISTS operators_export_order;
