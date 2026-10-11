package store

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"testing"
)

// Every exception names an exact foreign key and a reviewed reason. Some keys
// already have a selective parent-ID or unique-subset index; bounded tables
// have their limits cited below. Public realm/agent deletion is soft, and
// whole-account hard purge removes child rows first. Hard realm/agent deletion
// can still scan a child table once per parent, including other accounts' rows.
// Revisit those exceptions as accounts grow; none exempts a new foreign key
// from an explicit index/allowlist decision.
var foreignKeyIndexAllowlist = map[string]string{
	"agent_avatar_activations.agent_avatar_activations_realm_id_fkey":                             "Realm deletion is soft (internal/store/realm.go:232); hard account purge deletes children first (internal/store/finalize_evacuation.go:182).",
	"agent_avatar_profiles.agent_avatar_profiles_realm_id_fkey":                                   "Realm deletion is soft (internal/store/realm.go:232); hard account purge deletes children first (internal/store/finalize_evacuation.go:182).",
	"agent_avatar_rejections.agent_avatar_rejections_realm_id_fkey":                               "Realm deletion is soft (internal/store/realm.go:232); hard account purge deletes children first (internal/store/finalize_evacuation.go:182).",
	"agent_avatar_resets.agent_avatar_resets_realm_id_fkey":                                       "Realm deletion is soft (internal/store/realm.go:232); hard account purge deletes children first (internal/store/finalize_evacuation.go:182).",
	"agent_avatar_versions.agent_avatar_versions_realm_id_fkey":                                   "Realm deletion is soft (internal/store/realm.go:232); hard account purge deletes children first (internal/store/finalize_evacuation.go:182).",
	"agent_dashboard_preferences.agent_dashboard_preferences_account_id_realm_id_fkey":            "Realm deletion is soft (internal/store/realm.go:232); hard account purge deletes children first (internal/store/finalize_evacuation.go:182).",
	"agent_dashboard_preferences.agent_dashboard_preferences_realm_id_agent_id_fkey":              "Agent deletion is soft (internal/store/agent.go:282); hard account purge deletes children first (internal/store/finalize_evacuation.go:182).",
	"agent_email_mailboxes.agent_email_mailboxes_realm_id_owner_agent_id_fkey":                    "Agent deletion is soft (internal/store/agent.go:282); hard account purge deletes children first (internal/store/finalize_evacuation.go:182).",
	"agent_email_outbound_messages.agent_email_outbound_messages_realm_id_owner_agent_id_fkey":    "Agent deletion is soft (internal/store/agent.go:282); hard account purge deletes children first (internal/store/finalize_evacuation.go:182).",
	"agent_email_send_controls.agent_email_send_controls_realm_id_owner_agent_id_fkey":            "Agent deletion is soft (internal/store/agent.go:282); hard account purge deletes children first (internal/store/finalize_evacuation.go:182).",
	"agent_message_deliveries.agent_message_deliveries_realm_id_fkey":                             "Realm deletion is soft (internal/store/realm.go:232); hard account purge deletes children first (internal/store/finalize_evacuation.go:182).",
	"agent_message_deliveries.agent_message_deliveries_recipient_agent_id_fkey":                   "Agent deletion is soft (internal/store/agent.go:282); hard account purge deletes children first (internal/store/finalize_evacuation.go:182).",
	"agent_message_request_candidates.agent_message_request_candidates_agent_id_fkey":             "Agent deletion is soft (internal/store/agent.go:282); hard account purge deletes children first (internal/store/finalize_evacuation.go:182).",
	"agent_message_request_claims.agent_message_request_claims_agent_id_fkey":                     "Agent deletion is soft (internal/store/agent.go:282); hard account purge deletes children first (internal/store/finalize_evacuation.go:182).",
	"agent_message_request_selections.agent_message_request_selections_coordinator_agent_id_fkey": "Agent deletion is soft (internal/store/agent.go:282); hard account purge deletes children first (internal/store/finalize_evacuation.go:182).",
	"agent_message_requests.agent_message_requests_coordinator_agent_id_fkey":                     "Agent deletion is soft (internal/store/agent.go:282); hard account purge deletes children first (internal/store/finalize_evacuation.go:182).",
	"agent_message_requests.agent_message_requests_realm_id_fkey":                                 "Realm deletion is soft (internal/store/realm.go:232); hard account purge deletes children first (internal/store/finalize_evacuation.go:182).",
	"agent_messages.agent_messages_from_agent_id_fkey":                                            "Agent deletion is soft (internal/store/agent.go:282); hard account purge deletes children first (internal/store/finalize_evacuation.go:182).",
	"agent_messages.agent_messages_realm_id_fkey":                                                 "Realm deletion is soft (internal/store/realm.go:232); hard account purge deletes children first (internal/store/finalize_evacuation.go:182).",
	"agent_messages.agent_messages_to_agent_id_fkey":                                              "Agent deletion is soft (internal/store/agent.go:282); hard account purge deletes children first (internal/store/finalize_evacuation.go:182).",
	"agent_vault_key_enrollments.agent_vault_key_enrollments_realm_id_owner_agent_id_fkey":        "Agent deletion is soft (internal/store/agent.go:282); hard account purge deletes children first (internal/store/finalize_evacuation.go:182).",
	"agent_vault_key_rotations.agent_vault_key_rotations_realm_id_owner_agent_id_fkey":            "Agent deletion is soft (internal/store/agent.go:282); hard account purge deletes children first (internal/store/finalize_evacuation.go:182).",
	"agent_vault_keys.agent_vault_keys_realm_id_owner_agent_id_fkey":                              "Agent deletion is soft (internal/store/agent.go:282); hard account purge deletes children first (internal/store/finalize_evacuation.go:182).",
	"avatar_mutation_receipts.avatar_mutation_receipts_realm_id_fkey":                             "Realm deletion is soft (internal/store/realm.go:232); hard account purge deletes children first (internal/store/finalize_evacuation.go:182).",
	"avatar_style_pack_versions.avatar_style_pack_versions_realm_id_fkey":                         "Realm deletion is soft (internal/store/realm.go:232); hard account purge deletes children first (internal/store/finalize_evacuation.go:182).",
	"avatar_style_packs.avatar_style_packs_realm_id_fkey":                                         "Realm deletion is soft (internal/store/realm.go:232); hard account purge deletes children first (internal/store/finalize_evacuation.go:182).",
	"avatar_style_rollout_jobs.avatar_style_rollout_jobs_realm_id_fkey":                           "Realm deletion is soft (internal/store/realm.go:232); hard account purge deletes children first (internal/store/finalize_evacuation.go:182).",
	"fact_assertions.fact_assertions_asserted_by_agent_id_fkey":                                   "Agent deletion is soft (internal/store/agent.go:282); hard account purge deletes children first (internal/store/finalize_evacuation.go:182).",
	"fact_assertions.fact_assertions_realm_id_fkey":                                               "Realm deletion is soft (internal/store/realm.go:232); hard account purge deletes children first (internal/store/finalize_evacuation.go:182).",
	"fact_candidates.fact_candidates_realm_id_fkey":                                               "Realm deletion is soft (internal/store/realm.go:232); hard account purge deletes children first (internal/store/finalize_evacuation.go:182).",
	"fact_mutation_tombstones.fact_mutation_tombstones_realm_id_fkey":                             "Realm deletion is soft (internal/store/realm.go:232); hard account purge deletes children first (internal/store/finalize_evacuation.go:182).",
	"fact_subjects.fact_subjects_realm_id_fkey":                                                   "Realm deletion is soft (internal/store/realm.go:232); hard account purge deletes children first (internal/store/finalize_evacuation.go:182).",
	"facts.facts_deleted_by_agent_id_fkey":                                                        "Agent deletion is soft (internal/store/agent.go:282); hard account purge deletes children first (internal/store/finalize_evacuation.go:182).",
	"facts.facts_realm_id_fkey":                                                                   "Realm deletion is soft (internal/store/realm.go:232); hard account purge deletes children first (internal/store/finalize_evacuation.go:182).",
	"memories.memories_authored_by_agent_id_fkey":                                                 "Agent deletion is soft (internal/store/agent.go:282); hard account purge deletes children first (internal/store/finalize_evacuation.go:182).",
	"memories.memories_owner_id_fkey":                                                             "Agent deletion is soft (internal/store/agent.go:282); hard account purge deletes children first (internal/store/finalize_evacuation.go:182).",
	"memories.memories_permanently_deleted_by_id_fkey":                                            "Agent deletion is soft (internal/store/agent.go:282); hard account purge deletes children first (internal/store/finalize_evacuation.go:182).",
	"memories.memories_realm_id_fkey":                                                             "Realm deletion is soft (internal/store/realm.go:232); hard account purge deletes children first (internal/store/finalize_evacuation.go:182).",
	"memory_change_clocks.memory_change_clocks_owner_id_fkey":                                     "Agent deletion is soft (internal/store/agent.go:282); hard account purge deletes children first (internal/store/finalize_evacuation.go:182).",
	"memory_change_clocks.memory_change_clocks_realm_id_fkey":                                     "Realm deletion is soft (internal/store/realm.go:232); hard account purge deletes children first (internal/store/finalize_evacuation.go:182).",
	"memory_curation_actions.memory_curation_actions_realm_id_fkey":                               "Realm deletion is soft (internal/store/realm.go:232); hard account purge deletes children first (internal/store/finalize_evacuation.go:182).",
	"memory_curation_cursors.memory_curation_cursors_realm_id_fkey":                               "Realm deletion is soft (internal/store/realm.go:232); hard account purge deletes children first (internal/store/finalize_evacuation.go:182).",
	"memory_curation_lanes.memory_curation_lanes_owner_id_fkey":                                   "Agent deletion is soft (internal/store/agent.go:282); hard account purge deletes children first (internal/store/finalize_evacuation.go:182).",
	"memory_curation_lanes.memory_curation_lanes_realm_id_fkey":                                   "Realm deletion is soft (internal/store/realm.go:232); hard account purge deletes children first (internal/store/finalize_evacuation.go:182).",
	"memory_curation_mutations.memory_curation_mutations_realm_id_fkey":                           "Realm deletion is soft (internal/store/realm.go:232); hard account purge deletes children first (internal/store/finalize_evacuation.go:182).",
	"memory_curation_requests.memory_curation_requests_realm_id_fkey":                             "Realm deletion is soft (internal/store/realm.go:232); hard account purge deletes children first (internal/store/finalize_evacuation.go:182).",
	"memory_curation_run_inputs.memory_curation_run_inputs_realm_id_fkey":                         "Realm deletion is soft (internal/store/realm.go:232); hard account purge deletes children first (internal/store/finalize_evacuation.go:182).",
	"memory_curation_runs.memory_curation_runs_realm_id_fkey":                                     "Realm deletion is soft (internal/store/realm.go:232); hard account purge deletes children first (internal/store/finalize_evacuation.go:182).",
	"memory_deleted_references.memory_deleted_references_owner_id_fkey":                           "Agent deletion is soft (internal/store/agent.go:282); hard account purge deletes children first (internal/store/finalize_evacuation.go:182).",
	"memory_deleted_references.memory_deleted_references_realm_id_fkey":                           "Realm deletion is soft (internal/store/realm.go:232); hard account purge deletes children first (internal/store/finalize_evacuation.go:182).",
	"memory_evidence.memory_evidence_actor_id_fkey":                                               "Agent deletion is soft (internal/store/agent.go:282); hard account purge deletes children first (internal/store/finalize_evacuation.go:182).",
	"memory_evidence.memory_evidence_owner_id_fkey":                                               "Agent deletion is soft (internal/store/agent.go:282); hard account purge deletes children first (internal/store/finalize_evacuation.go:182).",
	"memory_evidence.memory_evidence_realm_id_fkey":                                               "Realm deletion is soft (internal/store/realm.go:232); hard account purge deletes children first (internal/store/finalize_evacuation.go:182).",
	"memory_relations.memory_relations_owner_id_fkey":                                             "Agent deletion is soft (internal/store/agent.go:282); hard account purge deletes children first (internal/store/finalize_evacuation.go:182).",
	"memory_relations.memory_relations_realm_id_fkey":                                             "Realm deletion is soft (internal/store/realm.go:232); hard account purge deletes children first (internal/store/finalize_evacuation.go:182).",
	"memory_vector_profiles.memory_vector_profiles_created_by_agent_id_fkey":                      "Agent deletion is soft (internal/store/agent.go:282); hard account purge deletes children first (internal/store/finalize_evacuation.go:182).",
	"memory_vector_profiles.memory_vector_profiles_owner_id_fkey":                                 "Agent deletion is soft (internal/store/agent.go:282); hard account purge deletes children first (internal/store/finalize_evacuation.go:182).",
	"memory_vector_profiles.memory_vector_profiles_realm_id_fkey":                                 "Realm deletion is soft (internal/store/realm.go:232); hard account purge deletes children first (internal/store/finalize_evacuation.go:182).",
	"memory_vectors.memory_vectors_created_by_agent_id_fkey":                                      "Agent deletion is soft (internal/store/agent.go:282); hard account purge deletes children first (internal/store/finalize_evacuation.go:182).",
	"memory_vectors.memory_vectors_owner_id_fkey":                                                 "Agent deletion is soft (internal/store/agent.go:282); hard account purge deletes children first (internal/store/finalize_evacuation.go:182).",
	"memory_vectors.memory_vectors_realm_id_fkey":                                                 "Realm deletion is soft (internal/store/realm.go:232); hard account purge deletes children first (internal/store/finalize_evacuation.go:182).",
	"memory_versions.memory_versions_actor_id_fkey":                                               "Agent deletion is soft (internal/store/agent.go:282); hard account purge deletes children first (internal/store/finalize_evacuation.go:182).",
	"memory_versions.memory_versions_owner_id_fkey":                                               "Agent deletion is soft (internal/store/agent.go:282); hard account purge deletes children first (internal/store/finalize_evacuation.go:182).",
	"memory_versions.memory_versions_realm_id_fkey":                                               "Realm deletion is soft (internal/store/realm.go:232); hard account purge deletes children first (internal/store/finalize_evacuation.go:182).",
	"realm_avatar_styles.realm_avatar_styles_realm_id_fkey":                                       "Realm deletion is soft (internal/store/realm.go:232); hard account purge deletes children first (internal/store/finalize_evacuation.go:182).",
	"secret_mutation_receipts.secret_mutation_receipts_realm_id_owner_agent_id_fkey":              "Agent deletion is soft (internal/store/agent.go:282); hard account purge deletes children first (internal/store/finalize_evacuation.go:182).",
	"secrets.secrets_realm_id_owner_agent_id_fkey":                                                "Agent deletion is soft (internal/store/agent.go:282); hard account purge deletes children first (internal/store/finalize_evacuation.go:182).",
	"tokens.tokens_agent_id_fkey":                                                                 "Agent deletion is soft (internal/store/agent.go:282); hard account purge deletes children first (internal/store/finalize_evacuation.go:182).",
	"transcript_conversations.transcript_conversations_realm_id_fkey":                             "Realm deletion is soft (internal/store/realm.go:232); hard account purge deletes children first (internal/store/finalize_evacuation.go:182).",
	"transcript_entries.transcript_entries_realm_id_fkey":                                         "Realm deletion is soft (internal/store/realm.go:232); hard account purge deletes children first (internal/store/finalize_evacuation.go:182).",
	"transcript_entries.transcript_entries_recorded_by_agent_id_fkey":                             "Agent deletion is soft (internal/store/agent.go:282); hard account purge deletes children first (internal/store/finalize_evacuation.go:182).",
	"usage_events.usage_events_realm_id_fkey":                                                     "Realm deletion is soft (internal/store/realm.go:232); hard account purge deletes children first (internal/store/finalize_evacuation.go:182).",
	"usage_rollups.usage_rollups_realm_id_fkey":                                                   "Realm deletion is soft (internal/store/realm.go:232); hard account purge deletes children first (internal/store/finalize_evacuation.go:182).",
	"vault_key_enrollment_receipts.vault_key_enrollment_receipts_realm_id_owner_agent_id_fkey":    "Agent deletion is soft (internal/store/agent.go:282); hard account purge deletes children first (internal/store/finalize_evacuation.go:182).",
	"vault_key_rotation_receipts.vault_key_rotation_receipts_realm_id_owner_agent_id_fkey":        "Agent deletion is soft (internal/store/agent.go:282); hard account purge deletes children first (internal/store/finalize_evacuation.go:182).",

	// Existing parent-ID/unique-subset indexes and explicitly bounded tables.
	"agent_avatar_activations.agent_avatar_activations_account_id_realm_id_agent_id_avat_fkey":             "The by-version index narrows to one avatar identity/version before the lineage filter (internal/store/migrations/0050_add_agent_avatars.sql:269).",
	"agent_avatar_profiles.agent_avatar_profiles_active_version_fk":                                        "The unique account/realm/agent prefix bounds each version-pointer check to one profile (internal/store/migrations/0050_add_agent_avatars.sql:134).",
	"agent_avatar_profiles.agent_avatar_profiles_latest_version_fk":                                        "The unique account/realm/agent prefix bounds each version-pointer check to one profile (internal/store/migrations/0050_add_agent_avatars.sql:134).",
	"agent_avatar_profiles.agent_avatar_profiles_proposed_version_fk":                                      "The unique account/realm/agent prefix bounds each version-pointer check to one profile (internal/store/migrations/0050_add_agent_avatars.sql:134).",
	"agent_avatar_resets.agent_avatar_resets_account_id_realm_id_agent_id_retired_a_fkey":                  "The unique account/realm/agent/retired-lineage prefix bounds the check to one reset (internal/store/migrations/0050_add_agent_avatars.sql:314).",
	"agent_avatar_resets.agent_avatar_resets_account_id_realm_id_agent_id_retired_p_fkey":                  "The unique account/realm/agent/retired-lineage prefix bounds the check to one reset (internal/store/migrations/0050_add_agent_avatars.sql:314).",
	"agent_email_address_domains.agent_email_address_domains_account_id_realm_id_provisione_fkey":          "The primary-key address_id prefix directly locates this parent address's route rows (internal/store/migrations/0087_add_agent_email_address_domains.sql:20).",
	"agent_email_deliveries.agent_email_deliveries_account_id_realm_id_mailbox_id_owne_fkey":               "The message_id/mailbox_id primary key bounds this message-reference check to one delivery (internal/store/migrations/0059_add_agent_email.sql:218).",
	"agent_email_deliveries.agent_email_deliveries_account_id_realm_id_owner_agent_id__fkey":               "The owner index selects the sole mailbox per owner guaranteed by its unique key (internal/store/migrations/0059_add_agent_email.sql:251 and :66).",
	"agent_email_mailboxes.agent_email_mailboxes_account_id_realm_id_owner_agent_id_a_fkey":                "The unique account/realm/owner prefix bounds the address-reference check to one mailbox (internal/store/migrations/0059_add_agent_email.sql:66).",
	"agent_email_messages.agent_email_messages_account_id_realm_id_mailbox_id_owner__fkey":                 "The non-null duplicate-pointer index leads with account_id/possible_duplicate_of_message_id (internal/store/migrations/0069_add_agent_email_retention.sql:97).",
	"agent_email_messages.agent_email_messages_account_id_realm_id_owner_agent_id_ma_fkey":                 "The mailbox index directly locates the referenced account/realm/mailbox before redundant scope filters (internal/store/migrations/0059_add_agent_email.sql:194).",
	"agent_email_outbound_provider_events.agent_email_outbound_provider_event_account_id_outbound_id_fkey": "The outbound_id-leading index directly locates provider events for the parent send (internal/store/migrations/0089_add_agent_email_outbound.sql:226).",
	"agent_email_retention_account_scan_state.agent_email_retention_account_scan_state_account_id_fkey":    "The mode/account primary key and two-value mode check permit at most two rows per account (internal/store/migrations/0069_add_agent_email_retention.sql:16).",
	"agent_email_retry_canary_arms.agent_email_retry_canary_arms_account_id_realm_id_mailbox__fkey":        "The non-null accepted-message index leads with account_id/accepted_message_id (internal/store/migrations/0069_add_agent_email_retention.sql:105).",
	"agent_email_retry_canary_arms.agent_email_retry_canary_arms_account_id_realm_id_owner_ag_fkey":        "The primary-key account/realm/mailbox prefix directly locates this mailbox's retry arms (internal/store/migrations/0061_add_agent_email_retry_canary.sql:28).",
	"avatar_style_packs.avatar_style_packs_current_version_fk":                                             "The account/realm/style-pack primary key bounds each current-version check to one style pack (internal/store/migrations/0050_add_agent_avatars.sql:15).",

	// Other selective prefixes, bounded metadata, and soft-deleted operators.
	"agent_message_request_candidates.agent_message_request_candida_request_id_account_id_realm__fkey": "The primary key starts with request_id, restricting checks to one request's candidate snapshot; internal/store/migrations/0038_add_open_message_requests.sql:69.",
	"agent_message_request_claims.agent_message_request_claims_request_id_account_id_realm_i_fkey":     "The capacity index starts with request_id, restricting checks to one request; internal/store/migrations/0038_add_open_message_requests.sql:189.",
	"agent_message_request_claims.agent_message_request_claims_selection_id_request_id_accou_fkey":     "UNIQUE(selection_id, agent_id) restricts checks to one selection; internal/store/migrations/0038_add_open_message_requests.sql:143.",
	"agent_message_request_selections.agent_message_request_selecti_request_id_account_id_realm__fkey": "UNIQUE(request_id, generation) restricts checks to one request's selections; internal/store/migrations/0038_add_open_message_requests.sql:108.",
	"memories.memories_current_version_fk":                                                            "The id primary key bounds each referenced-version check to one memory head; internal/store/migrations/0029_add_narrative_memories.sql:19.",
	"memory_curation_actions.memory_curation_actions_run_id_account_id_realm_id_owner_k_fkey":         "UNIQUE(run_id, ordinal) restricts cascade checks to one run's actions; internal/store/migrations/0030_add_memory_curation.sql:414.",
	"memory_curation_lanes.memory_curation_lanes_active_run_fk":                                       "The active_run_id unique partial index bounds each non-null reference to one lane; internal/store/migrations/0030_add_memory_curation.sql:288.",
	"memory_curation_run_inputs.memory_curation_run_inputs_run_id_account_id_realm_id_owne_fkey":      "The (run_id, ordinal) primary key restricts cascade checks to one run's inputs; internal/store/migrations/0030_add_memory_curation.sql:328.",
	"memory_curation_run_inputs.memory_curation_run_inputs_transcript_id_account_id_realm__fkey":      "The transcript_id partial index restricts checks to one transcript's inputs; internal/store/migrations/0065_index_transcript_retention.sql:22.",
	"memory_curation_runs.memory_curation_runs_request_id_account_id_realm_id_owner__fkey":            "The request_id-leading index restricts cascade checks to one request's runs; internal/store/migrations/0030_add_memory_curation.sql:275.",
	"memory_deleted_references.memory_deleted_references_deleted_memory_id_account_id_rea_fkey":       "The deleted_memory_id-leading index restricts cascade checks to one memory's references; internal/store/migrations/0029_add_narrative_memories.sql:635.",
	"memory_evidence.memory_evidence_memory_id_target_version_account_id_realm__fkey":                 "The target index starts with memory_id and target_version, selecting evidence for that exact version; internal/store/migrations/0029_add_narrative_memories.sql:347.",
	"memory_evidence.memory_evidence_source_message_id_account_id_realm_id_fkey":                      "The source_message_id partial index selects evidence for that exact message; internal/store/migrations/0068_add_message_retention.sql:214.",
	"memory_evidence.memory_evidence_source_transcript_id_account_id_realm_id_o_fkey":                 "The source_transcript_id partial index restricts checks to one conversation's evidence; internal/store/migrations/0065_index_transcript_retention.sql:17.",
	"memory_evidence.memory_evidence_source_transcript_id_source_sequence_from_fkey":                  "The source_transcript_id partial index restricts endpoint checks to one conversation's evidence; internal/store/migrations/0065_index_transcript_retention.sql:17.",
	"memory_evidence.memory_evidence_source_transcript_id_source_sequence_until_fkey":                 "The source_transcript_id partial index restricts endpoint checks to one conversation's evidence; internal/store/migrations/0065_index_transcript_retention.sql:17.",
	"memory_relations.memory_relations_from_memory_id_from_version_account_id_re_fkey":                "The source index starts with from_memory_id and from_version, selecting relations for that exact version; internal/store/migrations/0029_add_narrative_memories.sql:517.",
	"memory_relations.memory_relations_to_memory_id_to_version_account_id_realm__fkey":                "The target index starts with to_memory_id and to_version, selecting relations for that exact version; internal/store/migrations/0029_add_narrative_memories.sql:519.",
	"memory_versions.memory_versions_memory_id_account_id_realm_id_owner_kind_o_fkey":                 "The primary key starts with memory_id, restricting cascade checks to one memory's versions; internal/store/migrations/0029_add_narrative_memories.sql:123.",
	"memory_versions.memory_versions_memory_id_previous_version_fkey":                                 "The primary key starts with memory_id, restricting predecessor checks to one memory's version history; internal/store/migrations/0029_add_narrative_memories.sql:123.",
	"message_retention_account_scan_state.message_retention_account_scan_state_account_id_fkey":       "The mode/account primary key and two-value mode check bound worker metadata to two rows per account; internal/store/migrations/0068_add_message_retention.sql:34.",
	"realm_avatar_styles.realm_avatar_styles_account_id_realm_id_style_pack_id_styl_fkey":             "The account/realm primary key bounds each style-version check to one realm selection; internal/store/migrations/0050_add_agent_avatars.sql:74.",
	"tokens.tokens_operator_id_fkey":                                                                  "Operators are soft-deleted; whole-account physical cleanup removes their tokens before operators; internal/store/operator.go:231; internal/store/finalize_evacuation.go:182.",
	"transcript_retention_account_scan_state.transcript_retention_account_scan_state_account_id_fkey": "The mode/account primary key and two-value mode check bound worker metadata to two rows per account; internal/store/migrations/0062_add_account_plan_policies.sql:41.",
}

var foreignKeyIndexCitationPattern = regexp.MustCompile(`([[:alnum:]_./-]+):([0-9]+)|\band\s+:([0-9]+)`)

func validateForeignKeyIndexAllowlistCitation(repoRoot, reason string) error {
	citations := foreignKeyIndexCitationPattern.FindAllStringSubmatch(reason, -1)
	if len(citations) == 0 {
		return fmt.Errorf("needs a reason with a file:line citation")
	}
	var path string
	for _, citation := range citations {
		lineText := citation[2]
		if citation[1] != "" {
			path = citation[1]
		} else {
			lineText = citation[3]
		}
		if path == "" {
			return fmt.Errorf("shorthand citation %q has no preceding file", citation[0])
		}
		if !filepath.IsLocal(path) {
			return fmt.Errorf("citation %q must use a repository-relative path", citation[0])
		}
		line, err := strconv.Atoi(lineText)
		if err != nil || line < 1 {
			return fmt.Errorf("citation %q needs a positive line number", citation[0])
		}
		body, err := os.ReadFile(filepath.Join(repoRoot, filepath.FromSlash(path)))
		if err != nil {
			return fmt.Errorf("read citation %s:%d: %w", path, line, err)
		}
		lines := bytes.Count(body, []byte{'\n'})
		if len(body) > 0 && body[len(body)-1] != '\n' {
			lines++
		}
		if line > lines {
			return fmt.Errorf("citation %s:%d exceeds file's %d lines", path, line, lines)
		}
	}
	return nil
}

func checkForeignKeyIndexAllowlistCitations(t *testing.T) {
	t.Helper()
	for key, reason := range foreignKeyIndexAllowlist {
		if err := validateForeignKeyIndexAllowlistCitation(filepath.Join("..", ".."), reason); err != nil {
			t.Errorf("allowlist %s: %v", key, err)
		}
	}
	if t.Failed() {
		t.FailNow()
	}
}

func TestForeignKeyIndexAllowlistCitations(t *testing.T) {
	checkForeignKeyIndexAllowlistCitations(t)
}

func TestValidateForeignKeyIndexAllowlistCitation(t *testing.T) {
	repoRoot := t.TempDir()
	for path, body := range map[string]string{"first.go": "first\nsecond\n", "second.sql": "first\nsecond", "shorter.go": "first\n", "empty.go": ""} {
		if err := os.WriteFile(filepath.Join(repoRoot, path), []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct {
		name   string
		reason string
		valid  bool
	}{
		{"last line with newline", "A selective index (first.go:2).", true},
		{"last line without newline", "A selective index (second.sql:2).", true},
		{"multiple files and shorthand", "first.go:1 and :2; second.sql:2 and :1", true},
		{"missing citation", "A selective index.", false},
		{"missing first file", "missing.go:1; first.go:1", false},
		{"missing later file", "first.go:1; missing.go:1", false},
		{"past newline terminated file", "first.go:3", false},
		{"past later file", "first.go:1; second.sql:3", false},
		{"past shorthand line", "first.go:1 and :3", false},
		{"shorthand follows latest file", "first.go:1; shorter.go:1 and :2", false},
		{"shorthand without file", "and :1", false},
		{"zero line", "first.go:0", false},
		{"empty file", "empty.go:1", false},
		{"outside repository", "../first.go:1", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := validateForeignKeyIndexAllowlistCitation(repoRoot, tc.reason)
			if (err == nil) != tc.valid {
				t.Fatalf("validate citation error = %v, valid = %t", err, tc.valid)
			}
		})
	}
}
