-- +goose NO TRANSACTION
-- +goose Up
-- Support referential-integrity checks and cascades on row deletion without
-- repeatedly scanning transcript history or curation bookkeeping.
-- Keep the remaining foreign-key indexes separate from the hot-table
-- migration so retries here preserve its completed builds.
-- Rebuild on retry so an interrupted concurrent build cannot leave an invalid index.
-- The catalog guard also requires indexes for fact and memory cascades,
-- curation provenance checks, and cursor cleanup on physical delete paths.
DROP INDEX CONCURRENTLY IF EXISTS fact_assertions_by_account;
CREATE INDEX CONCURRENTLY fact_assertions_by_account ON fact_assertions (account_id);

DROP INDEX CONCURRENTLY IF EXISTS fact_assertions_by_supersedes;
CREATE INDEX CONCURRENTLY fact_assertions_by_supersedes ON fact_assertions (supersedes_id) WHERE supersedes_id IS NOT NULL;

DROP INDEX CONCURRENTLY IF EXISTS facts_by_subject;
CREATE INDEX CONCURRENTLY facts_by_subject ON facts (subject_id);

DROP INDEX CONCURRENTLY IF EXISTS fact_candidates_by_conflict_fact;
CREATE INDEX CONCURRENTLY fact_candidates_by_conflict_fact ON fact_candidates (conflict_fact_id) WHERE conflict_fact_id IS NOT NULL;

DROP INDEX CONCURRENTLY IF EXISTS fact_candidates_by_resolved_fact;
CREATE INDEX CONCURRENTLY fact_candidates_by_resolved_fact ON fact_candidates (resolved_fact_id) WHERE resolved_fact_id IS NOT NULL;

DROP INDEX CONCURRENTLY IF EXISTS fact_candidates_by_observed_assertion;
CREATE INDEX CONCURRENTLY fact_candidates_by_observed_assertion ON fact_candidates (observed_assertion_id) WHERE observed_assertion_id IS NOT NULL;

DROP INDEX CONCURRENTLY IF EXISTS fact_candidates_by_decision_assertion;
CREATE INDEX CONCURRENTLY fact_candidates_by_decision_assertion ON fact_candidates (decision_assertion_id) WHERE decision_assertion_id IS NOT NULL;

DROP INDEX CONCURRENTLY IF EXISTS memory_curation_requests_by_replay_run_scope;
CREATE INDEX CONCURRENTLY memory_curation_requests_by_replay_run_scope ON memory_curation_requests (replay_run_id, account_id, realm_id, owner_kind, owner_id) WHERE replay_run_id IS NOT NULL;

DROP INDEX CONCURRENTLY IF EXISTS memory_curation_run_inputs_by_cursor_scope;
CREATE INDEX CONCURRENTLY memory_curation_run_inputs_by_cursor_scope ON memory_curation_run_inputs (account_id, realm_id, owner_kind, owner_id, cursor_source_kind, cursor_stream_id) WHERE cursor_stream_id IS NOT NULL;

DROP INDEX CONCURRENTLY IF EXISTS memory_curation_run_inputs_by_evidence_scope;
CREATE INDEX CONCURRENTLY memory_curation_run_inputs_by_evidence_scope ON memory_curation_run_inputs (evidence_id, account_id, realm_id, owner_kind, owner_id) WHERE evidence_id IS NOT NULL;

DROP INDEX CONCURRENTLY IF EXISTS memory_curation_run_inputs_by_memory_scope;
CREATE INDEX CONCURRENTLY memory_curation_run_inputs_by_memory_scope ON memory_curation_run_inputs (memory_id, memory_version, account_id, realm_id, owner_kind, owner_id) WHERE memory_id IS NOT NULL;

DROP INDEX CONCURRENTLY IF EXISTS memory_evidence_by_source_memory_scope;
CREATE INDEX CONCURRENTLY memory_evidence_by_source_memory_scope ON memory_evidence (source_memory_id, source_memory_version, account_id, realm_id, owner_kind, owner_id) WHERE source_memory_id IS NOT NULL;

DROP INDEX CONCURRENTLY IF EXISTS memory_vectors_by_memory_scope;
CREATE INDEX CONCURRENTLY memory_vectors_by_memory_scope ON memory_vectors (memory_id, memory_version, account_id, realm_id, owner_kind, owner_id);

DROP INDEX CONCURRENTLY IF EXISTS memory_versions_by_curation_scope;
CREATE INDEX CONCURRENTLY memory_versions_by_curation_scope ON memory_versions (curation_run_id, account_id, realm_id, owner_kind, owner_id, curation_action_id) WHERE curation_run_id IS NOT NULL;

DROP INDEX CONCURRENTLY IF EXISTS memory_relations_by_curation_scope;
CREATE INDEX CONCURRENTLY memory_relations_by_curation_scope ON memory_relations (curation_run_id, account_id, realm_id, owner_kind, owner_id, curation_action_id) WHERE curation_run_id IS NOT NULL;

DROP INDEX CONCURRENTLY IF EXISTS fact_candidates_by_curation_scope;
CREATE INDEX CONCURRENTLY fact_candidates_by_curation_scope ON fact_candidates (curation_run_id, account_id, realm_id, owner_agent_id, curation_action_id) WHERE curation_run_id IS NOT NULL;

DROP INDEX CONCURRENTLY IF EXISTS memory_relations_by_reverted_action_scope;
CREATE INDEX CONCURRENTLY memory_relations_by_reverted_action_scope ON memory_relations (reverted_by_action_id, reverted_by_run_id, account_id, realm_id, owner_kind, owner_id) WHERE reverted_by_action_id IS NOT NULL;

DROP INDEX CONCURRENTLY IF EXISTS memory_evidence_by_pending_scope;
CREATE INDEX CONCURRENTLY memory_evidence_by_pending_scope ON memory_evidence (pending_evidence_id, memory_id, target_version) WHERE pending_evidence_id IS NOT NULL;

-- Physical account cleanup also deletes avatar, email-route and vault parents;
-- cover their history references rather than scanning each child table per row.
DROP INDEX CONCURRENTLY IF EXISTS agent_avatar_activations_by_prior_scope;
CREATE INDEX CONCURRENTLY agent_avatar_activations_by_prior_scope ON agent_avatar_activations (account_id, realm_id, agent_id, prior_active_version, lineage_generation);

DROP INDEX CONCURRENTLY IF EXISTS agent_avatar_profiles_by_style_scope;
CREATE INDEX CONCURRENTLY agent_avatar_profiles_by_style_scope ON agent_avatar_profiles (account_id, realm_id, style_pack_id, style_pack_version);

DROP INDEX CONCURRENTLY IF EXISTS agent_avatar_versions_by_parent_scope;
CREATE INDEX CONCURRENTLY agent_avatar_versions_by_parent_scope ON agent_avatar_versions (account_id, realm_id, agent_id, parent_version, lineage_generation);

DROP INDEX CONCURRENTLY IF EXISTS agent_avatar_versions_by_style_scope;
CREATE INDEX CONCURRENTLY agent_avatar_versions_by_style_scope ON agent_avatar_versions (account_id, realm_id, style_pack_id, style_pack_version);

DROP INDEX CONCURRENTLY IF EXISTS agent_email_custom_domain_routes_by_alias_scope;
CREATE INDEX CONCURRENTLY agent_email_custom_domain_routes_by_alias_scope ON agent_email_custom_domain_routes (account_id, realm_id, realm_alias_claim_id, realm_label);

DROP INDEX CONCURRENTLY IF EXISTS agent_email_messages_by_address_scope;
CREATE INDEX CONCURRENTLY agent_email_messages_by_address_scope ON agent_email_messages (account_id, realm_id, owner_agent_id, address_id);

DROP INDEX CONCURRENTLY IF EXISTS agent_email_messages_by_recipient_route_scope;
CREATE INDEX CONCURRENTLY agent_email_messages_by_recipient_route_scope ON agent_email_messages (account_id, realm_id, recipient_realm_alias_claim_id, realm_label, recipient_custom_domain_request_id);

DROP INDEX CONCURRENTLY IF EXISTS agent_email_outbound_messages_by_address_scope;
CREATE INDEX CONCURRENTLY agent_email_outbound_messages_by_address_scope ON agent_email_outbound_messages (account_id, realm_id, owner_agent_id, address_id);

DROP INDEX CONCURRENTLY IF EXISTS agent_vault_key_enrollments_by_key_scope;
CREATE INDEX CONCURRENTLY agent_vault_key_enrollments_by_key_scope ON agent_vault_key_enrollments (account_id, realm_id, owner_agent_id, vault_key_id, vault_key_version);

DROP INDEX CONCURRENTLY IF EXISTS agent_vault_key_rotation_items_by_dek_scope;
CREATE INDEX CONCURRENTLY agent_vault_key_rotation_items_by_dek_scope ON agent_vault_key_rotation_items (account_id, realm_id, owner_agent_id, secret_id, field_id, dek_id, dek_generation);

DROP INDEX CONCURRENTLY IF EXISTS agent_vault_key_rotations_by_source_scope;
CREATE INDEX CONCURRENTLY agent_vault_key_rotations_by_source_scope ON agent_vault_key_rotations (account_id, realm_id, owner_agent_id, source_key_id, source_key_version);

DROP INDEX CONCURRENTLY IF EXISTS agent_vault_key_rotations_by_target_scope;
CREATE INDEX CONCURRENTLY agent_vault_key_rotations_by_target_scope ON agent_vault_key_rotations (account_id, realm_id, owner_agent_id, target_key_id, target_key_version);

DROP INDEX CONCURRENTLY IF EXISTS avatar_style_pack_versions_by_previous_scope;
CREATE INDEX CONCURRENTLY avatar_style_pack_versions_by_previous_scope ON avatar_style_pack_versions (account_id, realm_id, style_pack_id, previous_version);

DROP INDEX CONCURRENTLY IF EXISTS avatar_style_rollout_jobs_by_style_scope;
CREATE INDEX CONCURRENTLY avatar_style_rollout_jobs_by_style_scope ON avatar_style_rollout_jobs (account_id, realm_id, style_pack_id, style_pack_version);

DROP INDEX CONCURRENTLY IF EXISTS secret_deks_by_wrapping_key_scope;
CREATE INDEX CONCURRENTLY secret_deks_by_wrapping_key_scope ON secret_deks (account_id, realm_id, owner_agent_id, wrapping_key_id, wrapping_key_version);

DROP INDEX CONCURRENTLY IF EXISTS vault_key_enrollment_receipts_by_parent_scope;
CREATE INDEX CONCURRENTLY vault_key_enrollment_receipts_by_parent_scope ON vault_key_enrollment_receipts (account_id, realm_id, owner_agent_id, enrollment_id);

DROP INDEX CONCURRENTLY IF EXISTS vault_key_rotation_receipts_by_parent_scope;
CREATE INDEX CONCURRENTLY vault_key_rotation_receipts_by_parent_scope ON vault_key_rotation_receipts (account_id, realm_id, owner_agent_id, rotation_id);

-- +goose Down
DROP INDEX CONCURRENTLY IF EXISTS vault_key_rotation_receipts_by_parent_scope;
DROP INDEX CONCURRENTLY IF EXISTS vault_key_enrollment_receipts_by_parent_scope;
DROP INDEX CONCURRENTLY IF EXISTS secret_deks_by_wrapping_key_scope;
DROP INDEX CONCURRENTLY IF EXISTS avatar_style_rollout_jobs_by_style_scope;
DROP INDEX CONCURRENTLY IF EXISTS avatar_style_pack_versions_by_previous_scope;
DROP INDEX CONCURRENTLY IF EXISTS agent_vault_key_rotations_by_target_scope;
DROP INDEX CONCURRENTLY IF EXISTS agent_vault_key_rotations_by_source_scope;
DROP INDEX CONCURRENTLY IF EXISTS agent_vault_key_rotation_items_by_dek_scope;
DROP INDEX CONCURRENTLY IF EXISTS agent_vault_key_enrollments_by_key_scope;
DROP INDEX CONCURRENTLY IF EXISTS agent_email_outbound_messages_by_address_scope;
DROP INDEX CONCURRENTLY IF EXISTS agent_email_messages_by_recipient_route_scope;
DROP INDEX CONCURRENTLY IF EXISTS agent_email_messages_by_address_scope;
DROP INDEX CONCURRENTLY IF EXISTS agent_email_custom_domain_routes_by_alias_scope;
DROP INDEX CONCURRENTLY IF EXISTS agent_avatar_versions_by_style_scope;
DROP INDEX CONCURRENTLY IF EXISTS agent_avatar_versions_by_parent_scope;
DROP INDEX CONCURRENTLY IF EXISTS agent_avatar_profiles_by_style_scope;
DROP INDEX CONCURRENTLY IF EXISTS agent_avatar_activations_by_prior_scope;
DROP INDEX CONCURRENTLY IF EXISTS memory_evidence_by_pending_scope;
DROP INDEX CONCURRENTLY IF EXISTS memory_relations_by_reverted_action_scope;
DROP INDEX CONCURRENTLY IF EXISTS fact_candidates_by_curation_scope;
DROP INDEX CONCURRENTLY IF EXISTS memory_relations_by_curation_scope;
DROP INDEX CONCURRENTLY IF EXISTS memory_versions_by_curation_scope;
DROP INDEX CONCURRENTLY IF EXISTS memory_vectors_by_memory_scope;
DROP INDEX CONCURRENTLY IF EXISTS memory_evidence_by_source_memory_scope;
DROP INDEX CONCURRENTLY IF EXISTS memory_curation_run_inputs_by_memory_scope;
DROP INDEX CONCURRENTLY IF EXISTS memory_curation_run_inputs_by_evidence_scope;
DROP INDEX CONCURRENTLY IF EXISTS memory_curation_run_inputs_by_cursor_scope;
DROP INDEX CONCURRENTLY IF EXISTS memory_curation_requests_by_replay_run_scope;
DROP INDEX CONCURRENTLY IF EXISTS fact_candidates_by_decision_assertion;
DROP INDEX CONCURRENTLY IF EXISTS fact_candidates_by_observed_assertion;
DROP INDEX CONCURRENTLY IF EXISTS fact_candidates_by_resolved_fact;
DROP INDEX CONCURRENTLY IF EXISTS fact_candidates_by_conflict_fact;
DROP INDEX CONCURRENTLY IF EXISTS facts_by_subject;
DROP INDEX CONCURRENTLY IF EXISTS fact_assertions_by_supersedes;
DROP INDEX CONCURRENTLY IF EXISTS fact_assertions_by_account;
