-- +goose Up
-- LC-S6C2a: explicit local principal topology. Roles must already exist;
-- this migration never creates or alters a cluster role.
ALTER TABLE sync_tx_admissions ADD COLUMN database_principal text NOT NULL DEFAULT session_user;
ALTER TABLE sync_tx_admissions ADD COLUMN backend_pid integer NOT NULL DEFAULT pg_backend_pid();

-- SECURITY DEFINER paths put pg_temp last: omitting it would implicitly search
-- caller-controlled temporary relations first. FROM CURRENT allows PostgreSQL
-- to resolve row types in the selected schema while compiling each function.
-- The rebinding block below pins every function before this atomic migration
-- can commit; no unpinned definition is visible to another session.
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION witself_sync_writer_guard() RETURNS trigger
LANGUAGE plpgsql SECURITY DEFINER SET search_path FROM CURRENT AS $$
DECLARE
 target_account text;
 target_accounts text[] := ARRAY[]::text[];
 document jsonb;
 parent_realm text;
 capture sync_capture%ROWTYPE;
 admission sync_tx_admissions%ROWTYPE;
 admitted_tx bigint;
BEGIN
 IF TG_OP='TRUNCATE' THEN
  -- TRUNCATE has no row events from which to construct reviewed semantics.
  -- It is unsupported while any account is actively guarded, even for an
  -- admitted preferences writer; inactive databases retain original behavior.
  -- Fence the missing-control-row snapshot just as row DML does below. The
  -- all-account lock is intentional: this statement-level guard already
  -- refuses TRUNCATE whenever any account is active. First activation must
  -- update its account tuple, and an older RR/SERIALIZABLE view then fails.
  PERFORM 1 FROM accounts ORDER BY id FOR SHARE;
  -- An inert control row may already exist from schema 1. Activation versions
  -- that row instead of the account tuple, so lock it before trusting whether
  -- a capability appeared disabled in the transaction's historical snapshot.
  PERFORM 1 FROM sync_capture ORDER BY account_id FOR SHARE;
  IF EXISTS (SELECT 1 FROM sync_capture c WHERE c.snapshot_id IS NOT NULL
     OR EXISTS (SELECT 1 FROM sync_writer_capabilities w WHERE w.account_id=c.account_id
       AND w.capability=c.capability AND w.enabled AND w.revoked_generation=0)) THEN
   RAISE EXCEPTION 'sync writer not admitted' USING ERRCODE='P0001';
  END IF;
  RETURN NULL;
 END IF;
 FOR document IN
  SELECT value FROM jsonb_array_elements(CASE
   WHEN TG_OP='INSERT' THEN jsonb_build_array(to_jsonb(NEW))
   WHEN TG_OP='DELETE' THEN jsonb_build_array(to_jsonb(OLD))
   ELSE jsonb_build_array(to_jsonb(OLD),to_jsonb(NEW)) END)
 LOOP
  target_account := document->>'account_id';
  IF TG_TABLE_NAME='accounts' THEN
   target_account := document->>'id';
  ELSIF TG_TABLE_NAME='agents' THEN
   SELECT account_id INTO target_account FROM realms WHERE id=document->>'realm_id' FOR SHARE;
   IF NOT FOUND AND TG_OP<>'DELETE' THEN
    RAISE EXCEPTION 'sync writer not admitted' USING ERRCODE='P0001';
   END IF;
  ELSIF TG_TABLE_NAME='agent_activity' THEN
   SELECT realm_id INTO parent_realm FROM agents WHERE id=document->>'agent_id' FOR SHARE;
   IF FOUND THEN
    SELECT account_id INTO target_account FROM realms WHERE id=parent_realm FOR SHARE;
   END IF;
   IF target_account IS NULL AND TG_OP<>'DELETE' THEN
    RAISE EXCEPTION 'sync writer not admitted' USING ERRCODE='P0001';
   END IF;
  END IF;
  target_accounts := array_append(target_accounts,target_account);
 END LOOP;
 FOR target_account IN SELECT DISTINCT value FROM unnest(target_accounts) value WHERE value IS NOT NULL ORDER BY value
 LOOP
  -- The account tuple also fences a snapshot older than the first control
  -- row. First activation updates this tuple under the exclusive account
  -- gate, so an old RR/SERIALIZABLE writer cannot interpret an invisible
  -- control row as disabled. This is a row lock, never a missing gate acquire.
  PERFORM 1 FROM accounts WHERE id=target_account FOR SHARE;
  -- Some covered cell-local receipts intentionally outlive their account.
  -- A missing account alone must not refuse an otherwise valid inactive write.
  SELECT * INTO capture FROM sync_capture WHERE account_id=target_account FOR SHARE;
  IF NOT FOUND THEN CONTINUE; END IF;
  -- A durable snapshot marker keeps an activated account guarded after its
  -- last capability is revoked. Schema-1 inert custody rows have neither.
  IF capture.snapshot_id IS NULL AND NOT EXISTS (SELECT 1 FROM sync_writer_capabilities w
     WHERE w.account_id=target_account AND w.capability=capture.capability AND w.enabled AND w.revoked_generation=0) THEN
   CONTINUE;
  END IF;
  -- Check exact transaction identity before parsing any caller-controlled GUC.
  IF current_setting('witself.sync_tx',true) IS DISTINCT FROM txid_current()::text THEN
   RAISE EXCEPTION 'sync writer not admitted' USING ERRCODE='P0001';
  END IF;
  admitted_tx := txid_current();
  SELECT * INTO admission FROM sync_tx_admissions WHERE account_id=target_account AND tx_id=admitted_tx;
  IF NOT FOUND OR admission.sealed OR admission.capture_epoch<>capture.capture_epoch
    OR admission.lifecycle_epoch<>capture.lifecycle_epoch OR admission.restore_marker<>capture.restore_marker
    OR admission.capability<>capture.capability
    OR admission.database_principal<>session_user OR admission.backend_pid<>pg_backend_pid() THEN
   RAISE EXCEPTION 'sync writer not admitted' USING ERRCODE='P0001';
  END IF;
  IF TG_WHEN='AFTER' THEN
   UPDATE sync_tx_admissions SET actual_touches=actual_touches || jsonb_build_array(jsonb_build_object(
    'event_id',jsonb_array_length(actual_touches)+1,'table_name',TG_TABLE_NAME,'dml',TG_OP))
    WHERE account_id=target_account AND tx_id=admitted_tx;
  END IF;
 END LOOP;
 IF TG_OP='DELETE' THEN RETURN OLD; END IF;
 RETURN NEW;
END;
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE FUNCTION sync_admit(target_account text, target_principal text, target_capability text)
RETURNS TABLE(tx_id bigint, capture_epoch bigint, lifecycle_epoch bigint, restore_marker bytea)
LANGUAGE plpgsql SECURITY DEFINER SET search_path FROM CURRENT AS $$
DECLARE
 capture sync_capture%ROWTYPE;
 capability_row sync_writer_capabilities%ROWTYPE;
 physical_tx bigint;
BEGIN
 IF target_account IS NULL OR target_account='' OR target_principal IS NULL OR target_principal=''
    OR target_capability IS NULL OR target_capability=''
    OR (session_user<>current_user AND target_principal<>session_user) THEN
  RAISE EXCEPTION 'sync writer not admitted' USING ERRCODE='P0001';
 END IF;
 PERFORM pg_advisory_xact_lock_shared(capture_lock_key(target_account));
 SELECT * INTO capture FROM sync_capture WHERE account_id=target_account FOR SHARE;
 IF NOT FOUND OR capture.coverage_state NOT IN ('bootstrapping','suspended') OR capture.protocol_version<>1
    OR capture.min_writer_schema>3 OR capture.min_writer_capability<>target_capability
    OR capture.coverage_manifest_version<>'lc-s6c1-v1'
    OR capture.coverage_manifest_commitment<>sha256(convert_to($manifest$lc-s6c1-v1
account_evacuation_finalizations
account_events
account_provision_receipts
accounts
agent_activity
agent_avatar_activations
agent_avatar_profiles
agent_avatar_rejections
agent_avatar_resets
agent_avatar_versions
agent_dashboard_preferences
agent_email_account_rate_buckets
agent_email_address_domains
agent_email_addresses
agent_email_custom_domain_routes
agent_email_deliveries
agent_email_mailboxes
agent_email_messages
agent_email_outbound_messages
agent_email_outbound_provider_events
agent_email_outbound_rate_buckets
agent_email_outbound_recipient_suppressions
agent_email_rate_buckets
agent_email_realm_aliases
agent_email_realm_receive_controls
agent_email_realm_send_controls
agent_email_retention_account_scan_state
agent_email_retry_canary_arms
agent_email_send_controls
agent_message_deliveries
agent_message_rate_buckets
agent_message_request_candidates
agent_message_request_claims
agent_message_request_selections
agent_message_requests
agent_messages
agent_vault_key_enrollments
agent_vault_key_rotation_items
agent_vault_key_rotations
agent_vault_keys
agents
avatar_mutation_receipts
avatar_style_pack_versions
avatar_style_packs
avatar_style_rollout_jobs
fact_assertions
fact_candidates
fact_mutation_tombstones
fact_subjects
facts
memories
memory_change_clocks
memory_curation_actions
memory_curation_cursors
memory_curation_lanes
memory_curation_mutations
memory_curation_requests
memory_curation_run_inputs
memory_curation_runs
memory_deleted_references
memory_evidence
memory_relations
memory_vector_profiles
memory_vectors
memory_versions
message_retention_account_scan_state
message_retention_thread_activity
operators
realm_avatar_styles
realms
secret_deks
secret_fields
secret_mutation_receipts
secrets
support_ticket_messages
support_tickets
tokens
transcript_conversations
transcript_entries
transcript_retention_account_scan_state
transcript_storage_usage
usage_events
usage_rollups
vault_key_enrollment_receipts
vault_key_rotation_receipts
$manifest$,'UTF8')) THEN
  RAISE EXCEPTION 'sync writer not admitted' USING ERRCODE='P0001';
 END IF;
 SELECT * INTO capability_row FROM sync_writer_capabilities
  WHERE account_id=target_account AND principal=target_principal AND capability=target_capability FOR SHARE;
 IF NOT FOUND THEN
  RAISE EXCEPTION 'sync writer not admitted' USING ERRCODE='P0001';
 END IF;
 IF capability_row.revoked_generation<>0 THEN
  RAISE EXCEPTION 'capability_revoked' USING ERRCODE='P0001';
 END IF;
 IF NOT capability_row.enabled OR capability_row.capture_epoch<>capture.capture_epoch
    OR capability_row.manifest_version<>capture.coverage_manifest_version THEN
  RAISE EXCEPTION 'sync writer not admitted' USING ERRCODE='P0001';
 END IF;
 physical_tx:=txid_current();
 INSERT INTO sync_tx_admissions(account_id,tx_id,capability,lifecycle_epoch,restore_marker,capture_epoch,
   database_principal,backend_pid)
  VALUES(target_account,physical_tx,target_capability,capture.lifecycle_epoch,capture.restore_marker,capture.capture_epoch,
   session_user,pg_backend_pid());
 PERFORM set_config('witself.sync_tx',physical_tx::text,true);
 RETURN QUERY SELECT physical_tx,capture.capture_epoch,capture.lifecycle_epoch,capture.restore_marker;
END;
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE FUNCTION sync_touch(target_account text, target_tx bigint, target_operation text, target_atomic_group text,
 target_effect jsonb, target_dependencies jsonb, target_operation_id bytea, target_envelope bytea, target_commitment bytea)
RETURNS void LANGUAGE plpgsql SECURITY DEFINER SET search_path FROM CURRENT AS $$
DECLARE
 admission sync_tx_admissions%ROWTYPE;
 event_id bigint;
BEGIN
 SELECT * INTO admission FROM sync_tx_admissions WHERE account_id=target_account AND tx_id=target_tx;
 IF NOT FOUND OR admission.database_principal<>session_user OR admission.backend_pid<>pg_backend_pid()
    OR target_tx<>txid_current() OR current_setting('witself.sync_tx',true) IS DISTINCT FROM target_tx::text THEN
  RAISE EXCEPTION 'admission principal mismatch' USING ERRCODE='P0001';
 END IF;

 IF admission.sealed OR target_operation IS DISTINCT FROM 'preferences.put'
    OR target_atomic_group IS NULL OR target_atomic_group='' OR target_effect IS NULL OR target_dependencies IS NULL
    OR target_operation_id IS NULL OR octet_length(target_operation_id)<>16
    OR target_envelope IS NULL OR octet_length(target_envelope)>65536
    OR target_commitment IS DISTINCT FROM sha256(target_envelope)
    OR convert_from(target_envelope,'UTF8')::jsonb IS DISTINCT FROM
       jsonb_build_object('operation',target_operation,'effect',target_effect,'dependencies',target_dependencies) THEN
  RAISE EXCEPTION 'sync writer not admitted' USING ERRCODE='P0001';
 END IF;
 SELECT (event->>'event_id')::bigint INTO event_id FROM jsonb_array_elements(admission.actual_touches) event
  WHERE event->>'table_name'='agent_dashboard_preferences'
   AND NOT EXISTS (SELECT 1 FROM sync_tx_touches t WHERE t.account_id=target_account AND t.tx_id=target_tx
    AND t.touch_id=(event->>'event_id')::bigint)
  ORDER BY (event->>'event_id')::bigint LIMIT 1;
 IF NOT FOUND THEN
  RAISE EXCEPTION 'sync writer not admitted' USING ERRCODE='P0001';
 END IF;
 INSERT INTO sync_operations(account_id,operation_id,original_lifecycle_epoch,original_restore_marker,
  original_capture_epoch,original_installation,original_replica,original_write_lineage,original_sequence,
  operation_kind,envelope_version,schema_version,protocol_version,comparison_key_id,input_commitment,admission_state)
 VALUES(target_account,target_operation_id,admission.lifecycle_epoch,admission.restore_marker,admission.capture_epoch,
  'local','local','local-capture',event_id,target_operation,1,1,1,'local-capture-v1',target_commitment,'admitted');
 INSERT INTO sync_operation_payloads(account_id,operation_id,canonical_semantic_envelope)
 VALUES(target_account,target_operation_id,target_envelope);
 INSERT INTO sync_tx_touches(account_id,tx_id,touch_id,semantic_effect,dependencies,operation_id,atomic_group,table_name)
 VALUES(target_account,target_tx,event_id,target_effect,target_dependencies,target_operation_id,target_atomic_group,
  'agent_dashboard_preferences');
END;
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE FUNCTION sync_seal(target_account text, target_tx bigint, target_bundle_id bytea)
RETURNS void LANGUAGE plpgsql SECURITY DEFINER SET search_path FROM CURRENT AS $$
DECLARE
 admission sync_tx_admissions%ROWTYPE;
 prior bigint;
BEGIN
 SELECT * INTO admission FROM sync_tx_admissions WHERE account_id=target_account AND tx_id=target_tx;
 IF NOT FOUND OR admission.database_principal<>session_user OR admission.backend_pid<>pg_backend_pid()
    OR target_tx<>txid_current() OR current_setting('witself.sync_tx',true) IS DISTINCT FROM target_tx::text THEN
  RAISE EXCEPTION 'admission principal mismatch' USING ERRCODE='P0001';
 END IF;

 IF admission.sealed OR target_bundle_id IS NULL OR octet_length(target_bundle_id)<>16 THEN
  RAISE EXCEPTION 'sync writer not admitted' USING ERRCODE='P0001';
 END IF;
 SELECT last_finalized INTO prior FROM sync_finalized_counter
  WHERE account_id=target_account AND capture_epoch=admission.capture_epoch FOR UPDATE;
 IF NOT FOUND THEN
  RAISE EXCEPTION 'sync writer not admitted' USING ERRCODE='P0001';
 END IF;
 INSERT INTO sync_bundles(account_id,bundle_id,tx_id,capture_epoch,lifecycle_epoch,restore_marker,finalized,sealed,
  origin_class,semantic_effects,dependencies)
 SELECT target_account,target_bundle_id,target_tx,admission.capture_epoch,admission.lifecycle_epoch,
  admission.restore_marker,prior+1,true,'original',
  coalesce(jsonb_agg(semantic_effect ORDER BY touch_id),'[]'::jsonb),
  coalesce(jsonb_agg(dependencies ORDER BY touch_id),'[]'::jsonb)
 FROM sync_tx_touches WHERE account_id=target_account AND tx_id=target_tx;
 INSERT INTO sync_bundle_operations(account_id,bundle_id,member_order,operation_id,atomic_group)
 SELECT account_id,target_bundle_id,touch_id,operation_id,atomic_group FROM sync_tx_touches
 WHERE account_id=target_account AND tx_id=target_tx;
 UPDATE sync_tx_admissions SET sealed=true,prior_finalized=prior
 WHERE account_id=target_account AND tx_id=target_tx AND NOT sealed;
 UPDATE sync_finalized_counter SET last_finalized=prior+1
 WHERE account_id=target_account AND capture_epoch=admission.capture_epoch;
END;
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE FUNCTION sync_finalize(target_account text, target_tx bigint)
RETURNS void LANGUAGE plpgsql SECURITY DEFINER SET search_path FROM CURRENT AS $$
DECLARE
 admission sync_tx_admissions%ROWTYPE;
BEGIN
 SELECT * INTO admission FROM sync_tx_admissions WHERE account_id=target_account AND tx_id=target_tx;
 IF NOT FOUND OR admission.database_principal<>session_user OR admission.backend_pid<>pg_backend_pid()
    OR target_tx<>txid_current() OR current_setting('witself.sync_tx',true) IS DISTINCT FROM target_tx::text THEN
  RAISE EXCEPTION 'admission principal mismatch' USING ERRCODE='P0001';
 END IF;

 IF NOT admission.sealed OR admission.prior_finalized IS NULL OR NOT EXISTS (
  SELECT 1 FROM sync_bundles b JOIN sync_finalized_counter c USING(account_id,capture_epoch)
  WHERE b.account_id=target_account AND b.tx_id=target_tx AND b.finalized=admission.prior_finalized+1
    AND c.last_finalized=b.finalized) THEN
  RAISE EXCEPTION 'sync writer not admitted' USING ERRCODE='P0001';
 END IF;
 -- The deferred constraint trigger performs the complete exact-event coverage
 -- check at COMMIT. Finalization never releases the transaction's counter lock.
END;
$$;
-- +goose StatementEnd

ALTER FUNCTION witself_sync_check_coverage() SECURITY DEFINER;
-- +goose StatementBegin
DO $$
DECLARE signature text;
BEGIN
 FOREACH signature IN ARRAY ARRAY['capture_lock_key(text)','witself_sync_writer_guard()','witself_sync_check_coverage()','witself_sync_immutable_evidence()','witself_sync_publication_transition()','sync_admit(text,text,text)','sync_touch(text,bigint,text,text,jsonb,jsonb,bytea,bytea,bytea)','sync_seal(text,bigint,bytea)','sync_finalize(text,bigint)'] LOOP
  EXECUTE format('ALTER FUNCTION %s SET search_path = pg_catalog, %I, pg_temp',signature,current_schema());
 END LOOP;
END;
$$;
-- +goose StatementEnd

-- BEGIN SYNC PRINCIPAL TOPOLOGY
ALTER TABLE account_evacuation_finalizations OWNER TO witself_sync_owner;
ALTER TABLE account_events OWNER TO witself_sync_owner;
ALTER TABLE account_provision_receipts OWNER TO witself_sync_owner;
ALTER TABLE accounts OWNER TO witself_sync_owner;
ALTER TABLE agent_activity OWNER TO witself_sync_owner;
ALTER TABLE agent_avatar_activations OWNER TO witself_sync_owner;
ALTER TABLE agent_avatar_profiles OWNER TO witself_sync_owner;
ALTER TABLE agent_avatar_rejections OWNER TO witself_sync_owner;
ALTER TABLE agent_avatar_resets OWNER TO witself_sync_owner;
ALTER TABLE agent_avatar_versions OWNER TO witself_sync_owner;
ALTER TABLE agent_dashboard_preferences OWNER TO witself_sync_owner;
ALTER TABLE agent_email_account_rate_buckets OWNER TO witself_sync_owner;
ALTER TABLE agent_email_address_domains OWNER TO witself_sync_owner;
ALTER TABLE agent_email_addresses OWNER TO witself_sync_owner;
ALTER TABLE agent_email_custom_domain_routes OWNER TO witself_sync_owner;
ALTER TABLE agent_email_deliveries OWNER TO witself_sync_owner;
ALTER TABLE agent_email_mailboxes OWNER TO witself_sync_owner;
ALTER TABLE agent_email_messages OWNER TO witself_sync_owner;
ALTER TABLE agent_email_outbound_messages OWNER TO witself_sync_owner;
ALTER TABLE agent_email_outbound_provider_events OWNER TO witself_sync_owner;
ALTER TABLE agent_email_outbound_rate_buckets OWNER TO witself_sync_owner;
ALTER TABLE agent_email_outbound_recipient_suppressions OWNER TO witself_sync_owner;
ALTER TABLE agent_email_rate_buckets OWNER TO witself_sync_owner;
ALTER TABLE agent_email_realm_aliases OWNER TO witself_sync_owner;
ALTER TABLE agent_email_realm_receive_controls OWNER TO witself_sync_owner;
ALTER TABLE agent_email_realm_send_controls OWNER TO witself_sync_owner;
ALTER TABLE agent_email_retention_account_scan_state OWNER TO witself_sync_owner;
ALTER TABLE agent_email_retry_canary_arms OWNER TO witself_sync_owner;
ALTER TABLE agent_email_send_controls OWNER TO witself_sync_owner;
ALTER TABLE agent_message_deliveries OWNER TO witself_sync_owner;
ALTER TABLE agent_message_rate_buckets OWNER TO witself_sync_owner;
ALTER TABLE agent_message_request_candidates OWNER TO witself_sync_owner;
ALTER TABLE agent_message_request_claims OWNER TO witself_sync_owner;
ALTER TABLE agent_message_request_selections OWNER TO witself_sync_owner;
ALTER TABLE agent_message_requests OWNER TO witself_sync_owner;
ALTER TABLE agent_messages OWNER TO witself_sync_owner;
ALTER TABLE agent_vault_key_enrollments OWNER TO witself_sync_owner;
ALTER TABLE agent_vault_key_rotation_items OWNER TO witself_sync_owner;
ALTER TABLE agent_vault_key_rotations OWNER TO witself_sync_owner;
ALTER TABLE agent_vault_keys OWNER TO witself_sync_owner;
ALTER TABLE agents OWNER TO witself_sync_owner;
ALTER TABLE avatar_mutation_receipts OWNER TO witself_sync_owner;
ALTER TABLE avatar_style_pack_versions OWNER TO witself_sync_owner;
ALTER TABLE avatar_style_packs OWNER TO witself_sync_owner;
ALTER TABLE avatar_style_rollout_jobs OWNER TO witself_sync_owner;
ALTER TABLE fact_assertions OWNER TO witself_sync_owner;
ALTER TABLE fact_candidates OWNER TO witself_sync_owner;
ALTER TABLE fact_mutation_tombstones OWNER TO witself_sync_owner;
ALTER TABLE fact_subjects OWNER TO witself_sync_owner;
ALTER TABLE facts OWNER TO witself_sync_owner;
ALTER TABLE memories OWNER TO witself_sync_owner;
ALTER TABLE memory_change_clocks OWNER TO witself_sync_owner;
ALTER TABLE memory_curation_actions OWNER TO witself_sync_owner;
ALTER TABLE memory_curation_cursors OWNER TO witself_sync_owner;
ALTER TABLE memory_curation_lanes OWNER TO witself_sync_owner;
ALTER TABLE memory_curation_mutations OWNER TO witself_sync_owner;
ALTER TABLE memory_curation_requests OWNER TO witself_sync_owner;
ALTER TABLE memory_curation_run_inputs OWNER TO witself_sync_owner;
ALTER TABLE memory_curation_runs OWNER TO witself_sync_owner;
ALTER TABLE memory_deleted_references OWNER TO witself_sync_owner;
ALTER TABLE memory_evidence OWNER TO witself_sync_owner;
ALTER TABLE memory_relations OWNER TO witself_sync_owner;
ALTER TABLE memory_vector_profiles OWNER TO witself_sync_owner;
ALTER TABLE memory_vectors OWNER TO witself_sync_owner;
ALTER TABLE memory_versions OWNER TO witself_sync_owner;
ALTER TABLE message_retention_account_scan_state OWNER TO witself_sync_owner;
ALTER TABLE message_retention_thread_activity OWNER TO witself_sync_owner;
ALTER TABLE operators OWNER TO witself_sync_owner;
ALTER TABLE realm_avatar_styles OWNER TO witself_sync_owner;
ALTER TABLE realms OWNER TO witself_sync_owner;
ALTER TABLE secret_deks OWNER TO witself_sync_owner;
ALTER TABLE secret_fields OWNER TO witself_sync_owner;
ALTER TABLE secret_mutation_receipts OWNER TO witself_sync_owner;
ALTER TABLE secrets OWNER TO witself_sync_owner;
ALTER TABLE support_ticket_messages OWNER TO witself_sync_owner;
ALTER TABLE support_tickets OWNER TO witself_sync_owner;
ALTER TABLE tokens OWNER TO witself_sync_owner;
ALTER TABLE transcript_conversations OWNER TO witself_sync_owner;
ALTER TABLE transcript_entries OWNER TO witself_sync_owner;
ALTER TABLE transcript_retention_account_scan_state OWNER TO witself_sync_owner;
ALTER TABLE transcript_storage_usage OWNER TO witself_sync_owner;
ALTER TABLE usage_events OWNER TO witself_sync_owner;
ALTER TABLE usage_rollups OWNER TO witself_sync_owner;
ALTER TABLE vault_key_enrollment_receipts OWNER TO witself_sync_owner;
ALTER TABLE vault_key_rotation_receipts OWNER TO witself_sync_owner;
ALTER TABLE sync_incarnations OWNER TO witself_sync_owner;
ALTER TABLE sync_operations OWNER TO witself_sync_owner;
ALTER TABLE sync_operation_payloads OWNER TO witself_sync_owner;
ALTER TABLE sync_verifier_history OWNER TO witself_sync_owner;
ALTER TABLE sync_receipts OWNER TO witself_sync_owner;
ALTER TABLE sync_reuse_refusals OWNER TO witself_sync_owner;
ALTER TABLE sync_capture OWNER TO witself_sync_owner;
ALTER TABLE sync_writer_capabilities OWNER TO witself_sync_owner;
ALTER TABLE sync_tx_admissions OWNER TO witself_sync_owner;
ALTER TABLE sync_tx_touches OWNER TO witself_sync_owner;
ALTER TABLE sync_finalized_counter OWNER TO witself_sync_owner;
ALTER TABLE sync_bundles OWNER TO witself_sync_owner;
ALTER TABLE sync_bundle_operations OWNER TO witself_sync_owner;
ALTER TABLE sync_publication_outbox OWNER TO witself_sync_owner;
ALTER TABLE sync_acceptance_counter OWNER TO witself_sync_owner;
ALTER TABLE sync_resource_budgets OWNER TO witself_sync_owner;
ALTER TABLE sync_resource_reservations OWNER TO witself_sync_owner;
ALTER TABLE sync_deletion_knowledge OWNER TO witself_sync_owner;
ALTER TABLE sync_retained_evidence OWNER TO witself_sync_owner;
ALTER TABLE sync_evidence_pins OWNER TO witself_sync_owner;
ALTER TABLE sync_payload_retirements OWNER TO witself_sync_owner;
ALTER TABLE witself_sync_schema_version OWNER TO witself_sync_owner;
ALTER FUNCTION capture_lock_key(text) OWNER TO witself_sync_owner;
ALTER FUNCTION witself_sync_writer_guard() OWNER TO witself_sync_owner;
ALTER FUNCTION witself_sync_check_coverage() OWNER TO witself_sync_owner;
ALTER FUNCTION witself_sync_immutable_evidence() OWNER TO witself_sync_owner;
ALTER FUNCTION witself_sync_publication_transition() OWNER TO witself_sync_owner;
ALTER FUNCTION sync_admit(text,text,text) OWNER TO witself_sync_owner;
ALTER FUNCTION sync_touch(text,bigint,text,text,jsonb,jsonb,bytea,bytea,bytea) OWNER TO witself_sync_owner;
ALTER FUNCTION sync_seal(text,bigint,bytea) OWNER TO witself_sync_owner;
ALTER FUNCTION sync_finalize(text,bigint) OWNER TO witself_sync_owner;
REVOKE ALL ON TABLE account_evacuation_finalizations FROM PUBLIC, witself_sync_server, witself_sync_worker, witself_sync_maintenance, witself_sync_snapshot, witself_sync_legacy;
GRANT SELECT, INSERT, UPDATE, DELETE, TRUNCATE ON TABLE account_evacuation_finalizations TO witself_sync_server, witself_sync_worker, witself_sync_maintenance, witself_sync_legacy;
GRANT SELECT ON TABLE account_evacuation_finalizations TO witself_sync_snapshot;
REVOKE ALL ON TABLE account_events FROM PUBLIC, witself_sync_server, witself_sync_worker, witself_sync_maintenance, witself_sync_snapshot, witself_sync_legacy;
GRANT SELECT, INSERT, UPDATE, DELETE, TRUNCATE ON TABLE account_events TO witself_sync_server, witself_sync_worker, witself_sync_maintenance, witself_sync_legacy;
GRANT SELECT ON TABLE account_events TO witself_sync_snapshot;
REVOKE ALL ON TABLE account_provision_receipts FROM PUBLIC, witself_sync_server, witself_sync_worker, witself_sync_maintenance, witself_sync_snapshot, witself_sync_legacy;
GRANT SELECT, INSERT, UPDATE, DELETE, TRUNCATE ON TABLE account_provision_receipts TO witself_sync_server, witself_sync_worker, witself_sync_maintenance, witself_sync_legacy;
GRANT SELECT ON TABLE account_provision_receipts TO witself_sync_snapshot;
REVOKE ALL ON TABLE accounts FROM PUBLIC, witself_sync_server, witself_sync_worker, witself_sync_maintenance, witself_sync_snapshot, witself_sync_legacy;
GRANT SELECT, INSERT, UPDATE, DELETE, TRUNCATE ON TABLE accounts TO witself_sync_server, witself_sync_worker, witself_sync_maintenance, witself_sync_legacy;
GRANT SELECT ON TABLE accounts TO witself_sync_snapshot;
REVOKE ALL ON TABLE agent_activity FROM PUBLIC, witself_sync_server, witself_sync_worker, witself_sync_maintenance, witself_sync_snapshot, witself_sync_legacy;
GRANT SELECT, INSERT, UPDATE, DELETE, TRUNCATE ON TABLE agent_activity TO witself_sync_server, witself_sync_worker, witself_sync_maintenance, witself_sync_legacy;
GRANT SELECT ON TABLE agent_activity TO witself_sync_snapshot;
REVOKE ALL ON TABLE agent_avatar_activations FROM PUBLIC, witself_sync_server, witself_sync_worker, witself_sync_maintenance, witself_sync_snapshot, witself_sync_legacy;
GRANT SELECT, INSERT, UPDATE, DELETE, TRUNCATE ON TABLE agent_avatar_activations TO witself_sync_server, witself_sync_worker, witself_sync_maintenance, witself_sync_legacy;
GRANT SELECT ON TABLE agent_avatar_activations TO witself_sync_snapshot;
REVOKE ALL ON TABLE agent_avatar_profiles FROM PUBLIC, witself_sync_server, witself_sync_worker, witself_sync_maintenance, witself_sync_snapshot, witself_sync_legacy;
GRANT SELECT, INSERT, UPDATE, DELETE, TRUNCATE ON TABLE agent_avatar_profiles TO witself_sync_server, witself_sync_worker, witself_sync_maintenance, witself_sync_legacy;
GRANT SELECT ON TABLE agent_avatar_profiles TO witself_sync_snapshot;
REVOKE ALL ON TABLE agent_avatar_rejections FROM PUBLIC, witself_sync_server, witself_sync_worker, witself_sync_maintenance, witself_sync_snapshot, witself_sync_legacy;
GRANT SELECT, INSERT, UPDATE, DELETE, TRUNCATE ON TABLE agent_avatar_rejections TO witself_sync_server, witself_sync_worker, witself_sync_maintenance, witself_sync_legacy;
GRANT SELECT ON TABLE agent_avatar_rejections TO witself_sync_snapshot;
REVOKE ALL ON TABLE agent_avatar_resets FROM PUBLIC, witself_sync_server, witself_sync_worker, witself_sync_maintenance, witself_sync_snapshot, witself_sync_legacy;
GRANT SELECT, INSERT, UPDATE, DELETE, TRUNCATE ON TABLE agent_avatar_resets TO witself_sync_server, witself_sync_worker, witself_sync_maintenance, witself_sync_legacy;
GRANT SELECT ON TABLE agent_avatar_resets TO witself_sync_snapshot;
REVOKE ALL ON TABLE agent_avatar_versions FROM PUBLIC, witself_sync_server, witself_sync_worker, witself_sync_maintenance, witself_sync_snapshot, witself_sync_legacy;
GRANT SELECT, INSERT, UPDATE, DELETE, TRUNCATE ON TABLE agent_avatar_versions TO witself_sync_server, witself_sync_worker, witself_sync_maintenance, witself_sync_legacy;
GRANT SELECT ON TABLE agent_avatar_versions TO witself_sync_snapshot;
REVOKE ALL ON TABLE agent_dashboard_preferences FROM PUBLIC, witself_sync_server, witself_sync_worker, witself_sync_maintenance, witself_sync_snapshot, witself_sync_legacy;
GRANT SELECT, INSERT, UPDATE, DELETE, TRUNCATE ON TABLE agent_dashboard_preferences TO witself_sync_server, witself_sync_worker, witself_sync_maintenance, witself_sync_legacy;
GRANT SELECT ON TABLE agent_dashboard_preferences TO witself_sync_snapshot;
REVOKE ALL ON TABLE agent_email_account_rate_buckets FROM PUBLIC, witself_sync_server, witself_sync_worker, witself_sync_maintenance, witself_sync_snapshot, witself_sync_legacy;
GRANT SELECT, INSERT, UPDATE, DELETE, TRUNCATE ON TABLE agent_email_account_rate_buckets TO witself_sync_server, witself_sync_worker, witself_sync_maintenance, witself_sync_legacy;
GRANT SELECT ON TABLE agent_email_account_rate_buckets TO witself_sync_snapshot;
REVOKE ALL ON TABLE agent_email_address_domains FROM PUBLIC, witself_sync_server, witself_sync_worker, witself_sync_maintenance, witself_sync_snapshot, witself_sync_legacy;
GRANT SELECT, INSERT, UPDATE, DELETE, TRUNCATE ON TABLE agent_email_address_domains TO witself_sync_server, witself_sync_worker, witself_sync_maintenance, witself_sync_legacy;
GRANT SELECT ON TABLE agent_email_address_domains TO witself_sync_snapshot;
REVOKE ALL ON TABLE agent_email_addresses FROM PUBLIC, witself_sync_server, witself_sync_worker, witself_sync_maintenance, witself_sync_snapshot, witself_sync_legacy;
GRANT SELECT, INSERT, UPDATE, DELETE, TRUNCATE ON TABLE agent_email_addresses TO witself_sync_server, witself_sync_worker, witself_sync_maintenance, witself_sync_legacy;
GRANT SELECT ON TABLE agent_email_addresses TO witself_sync_snapshot;
REVOKE ALL ON TABLE agent_email_custom_domain_routes FROM PUBLIC, witself_sync_server, witself_sync_worker, witself_sync_maintenance, witself_sync_snapshot, witself_sync_legacy;
GRANT SELECT, INSERT, UPDATE, DELETE, TRUNCATE ON TABLE agent_email_custom_domain_routes TO witself_sync_server, witself_sync_worker, witself_sync_maintenance, witself_sync_legacy;
GRANT SELECT ON TABLE agent_email_custom_domain_routes TO witself_sync_snapshot;
REVOKE ALL ON TABLE agent_email_deliveries FROM PUBLIC, witself_sync_server, witself_sync_worker, witself_sync_maintenance, witself_sync_snapshot, witself_sync_legacy;
GRANT SELECT, INSERT, UPDATE, DELETE, TRUNCATE ON TABLE agent_email_deliveries TO witself_sync_server, witself_sync_worker, witself_sync_maintenance, witself_sync_legacy;
GRANT SELECT ON TABLE agent_email_deliveries TO witself_sync_snapshot;
REVOKE ALL ON TABLE agent_email_mailboxes FROM PUBLIC, witself_sync_server, witself_sync_worker, witself_sync_maintenance, witself_sync_snapshot, witself_sync_legacy;
GRANT SELECT, INSERT, UPDATE, DELETE, TRUNCATE ON TABLE agent_email_mailboxes TO witself_sync_server, witself_sync_worker, witself_sync_maintenance, witself_sync_legacy;
GRANT SELECT ON TABLE agent_email_mailboxes TO witself_sync_snapshot;
REVOKE ALL ON TABLE agent_email_messages FROM PUBLIC, witself_sync_server, witself_sync_worker, witself_sync_maintenance, witself_sync_snapshot, witself_sync_legacy;
GRANT SELECT, INSERT, UPDATE, DELETE, TRUNCATE ON TABLE agent_email_messages TO witself_sync_server, witself_sync_worker, witself_sync_maintenance, witself_sync_legacy;
GRANT SELECT ON TABLE agent_email_messages TO witself_sync_snapshot;
REVOKE ALL ON TABLE agent_email_outbound_messages FROM PUBLIC, witself_sync_server, witself_sync_worker, witself_sync_maintenance, witself_sync_snapshot, witself_sync_legacy;
GRANT SELECT, INSERT, UPDATE, DELETE, TRUNCATE ON TABLE agent_email_outbound_messages TO witself_sync_server, witself_sync_worker, witself_sync_maintenance, witself_sync_legacy;
GRANT SELECT ON TABLE agent_email_outbound_messages TO witself_sync_snapshot;
REVOKE ALL ON TABLE agent_email_outbound_provider_events FROM PUBLIC, witself_sync_server, witself_sync_worker, witself_sync_maintenance, witself_sync_snapshot, witself_sync_legacy;
GRANT SELECT, INSERT, UPDATE, DELETE, TRUNCATE ON TABLE agent_email_outbound_provider_events TO witself_sync_server, witself_sync_worker, witself_sync_maintenance, witself_sync_legacy;
GRANT SELECT ON TABLE agent_email_outbound_provider_events TO witself_sync_snapshot;
REVOKE ALL ON TABLE agent_email_outbound_rate_buckets FROM PUBLIC, witself_sync_server, witself_sync_worker, witself_sync_maintenance, witself_sync_snapshot, witself_sync_legacy;
GRANT SELECT, INSERT, UPDATE, DELETE, TRUNCATE ON TABLE agent_email_outbound_rate_buckets TO witself_sync_server, witself_sync_worker, witself_sync_maintenance, witself_sync_legacy;
GRANT SELECT ON TABLE agent_email_outbound_rate_buckets TO witself_sync_snapshot;
REVOKE ALL ON TABLE agent_email_outbound_recipient_suppressions FROM PUBLIC, witself_sync_server, witself_sync_worker, witself_sync_maintenance, witself_sync_snapshot, witself_sync_legacy;
GRANT SELECT, INSERT, UPDATE, DELETE, TRUNCATE ON TABLE agent_email_outbound_recipient_suppressions TO witself_sync_server, witself_sync_worker, witself_sync_maintenance, witself_sync_legacy;
GRANT SELECT ON TABLE agent_email_outbound_recipient_suppressions TO witself_sync_snapshot;
REVOKE ALL ON TABLE agent_email_rate_buckets FROM PUBLIC, witself_sync_server, witself_sync_worker, witself_sync_maintenance, witself_sync_snapshot, witself_sync_legacy;
GRANT SELECT, INSERT, UPDATE, DELETE, TRUNCATE ON TABLE agent_email_rate_buckets TO witself_sync_server, witself_sync_worker, witself_sync_maintenance, witself_sync_legacy;
GRANT SELECT ON TABLE agent_email_rate_buckets TO witself_sync_snapshot;
REVOKE ALL ON TABLE agent_email_realm_aliases FROM PUBLIC, witself_sync_server, witself_sync_worker, witself_sync_maintenance, witself_sync_snapshot, witself_sync_legacy;
GRANT SELECT, INSERT, UPDATE, DELETE, TRUNCATE ON TABLE agent_email_realm_aliases TO witself_sync_server, witself_sync_worker, witself_sync_maintenance, witself_sync_legacy;
GRANT SELECT ON TABLE agent_email_realm_aliases TO witself_sync_snapshot;
REVOKE ALL ON TABLE agent_email_realm_receive_controls FROM PUBLIC, witself_sync_server, witself_sync_worker, witself_sync_maintenance, witself_sync_snapshot, witself_sync_legacy;
GRANT SELECT, INSERT, UPDATE, DELETE, TRUNCATE ON TABLE agent_email_realm_receive_controls TO witself_sync_server, witself_sync_worker, witself_sync_maintenance, witself_sync_legacy;
GRANT SELECT ON TABLE agent_email_realm_receive_controls TO witself_sync_snapshot;
REVOKE ALL ON TABLE agent_email_realm_send_controls FROM PUBLIC, witself_sync_server, witself_sync_worker, witself_sync_maintenance, witself_sync_snapshot, witself_sync_legacy;
GRANT SELECT, INSERT, UPDATE, DELETE, TRUNCATE ON TABLE agent_email_realm_send_controls TO witself_sync_server, witself_sync_worker, witself_sync_maintenance, witself_sync_legacy;
GRANT SELECT ON TABLE agent_email_realm_send_controls TO witself_sync_snapshot;
REVOKE ALL ON TABLE agent_email_retention_account_scan_state FROM PUBLIC, witself_sync_server, witself_sync_worker, witself_sync_maintenance, witself_sync_snapshot, witself_sync_legacy;
GRANT SELECT, INSERT, UPDATE, DELETE, TRUNCATE ON TABLE agent_email_retention_account_scan_state TO witself_sync_server, witself_sync_worker, witself_sync_maintenance, witself_sync_legacy;
GRANT SELECT ON TABLE agent_email_retention_account_scan_state TO witself_sync_snapshot;
REVOKE ALL ON TABLE agent_email_retry_canary_arms FROM PUBLIC, witself_sync_server, witself_sync_worker, witself_sync_maintenance, witself_sync_snapshot, witself_sync_legacy;
GRANT SELECT, INSERT, UPDATE, DELETE, TRUNCATE ON TABLE agent_email_retry_canary_arms TO witself_sync_server, witself_sync_worker, witself_sync_maintenance, witself_sync_legacy;
GRANT SELECT ON TABLE agent_email_retry_canary_arms TO witself_sync_snapshot;
REVOKE ALL ON TABLE agent_email_send_controls FROM PUBLIC, witself_sync_server, witself_sync_worker, witself_sync_maintenance, witself_sync_snapshot, witself_sync_legacy;
GRANT SELECT, INSERT, UPDATE, DELETE, TRUNCATE ON TABLE agent_email_send_controls TO witself_sync_server, witself_sync_worker, witself_sync_maintenance, witself_sync_legacy;
GRANT SELECT ON TABLE agent_email_send_controls TO witself_sync_snapshot;
REVOKE ALL ON TABLE agent_message_deliveries FROM PUBLIC, witself_sync_server, witself_sync_worker, witself_sync_maintenance, witself_sync_snapshot, witself_sync_legacy;
GRANT SELECT, INSERT, UPDATE, DELETE, TRUNCATE ON TABLE agent_message_deliveries TO witself_sync_server, witself_sync_worker, witself_sync_maintenance, witself_sync_legacy;
GRANT SELECT ON TABLE agent_message_deliveries TO witself_sync_snapshot;
REVOKE ALL ON TABLE agent_message_rate_buckets FROM PUBLIC, witself_sync_server, witself_sync_worker, witself_sync_maintenance, witself_sync_snapshot, witself_sync_legacy;
GRANT SELECT, INSERT, UPDATE, DELETE, TRUNCATE ON TABLE agent_message_rate_buckets TO witself_sync_server, witself_sync_worker, witself_sync_maintenance, witself_sync_legacy;
GRANT SELECT ON TABLE agent_message_rate_buckets TO witself_sync_snapshot;
REVOKE ALL ON TABLE agent_message_request_candidates FROM PUBLIC, witself_sync_server, witself_sync_worker, witself_sync_maintenance, witself_sync_snapshot, witself_sync_legacy;
GRANT SELECT, INSERT, UPDATE, DELETE, TRUNCATE ON TABLE agent_message_request_candidates TO witself_sync_server, witself_sync_worker, witself_sync_maintenance, witself_sync_legacy;
GRANT SELECT ON TABLE agent_message_request_candidates TO witself_sync_snapshot;
REVOKE ALL ON TABLE agent_message_request_claims FROM PUBLIC, witself_sync_server, witself_sync_worker, witself_sync_maintenance, witself_sync_snapshot, witself_sync_legacy;
GRANT SELECT, INSERT, UPDATE, DELETE, TRUNCATE ON TABLE agent_message_request_claims TO witself_sync_server, witself_sync_worker, witself_sync_maintenance, witself_sync_legacy;
GRANT SELECT ON TABLE agent_message_request_claims TO witself_sync_snapshot;
REVOKE ALL ON TABLE agent_message_request_selections FROM PUBLIC, witself_sync_server, witself_sync_worker, witself_sync_maintenance, witself_sync_snapshot, witself_sync_legacy;
GRANT SELECT, INSERT, UPDATE, DELETE, TRUNCATE ON TABLE agent_message_request_selections TO witself_sync_server, witself_sync_worker, witself_sync_maintenance, witself_sync_legacy;
GRANT SELECT ON TABLE agent_message_request_selections TO witself_sync_snapshot;
REVOKE ALL ON TABLE agent_message_requests FROM PUBLIC, witself_sync_server, witself_sync_worker, witself_sync_maintenance, witself_sync_snapshot, witself_sync_legacy;
GRANT SELECT, INSERT, UPDATE, DELETE, TRUNCATE ON TABLE agent_message_requests TO witself_sync_server, witself_sync_worker, witself_sync_maintenance, witself_sync_legacy;
GRANT SELECT ON TABLE agent_message_requests TO witself_sync_snapshot;
REVOKE ALL ON TABLE agent_messages FROM PUBLIC, witself_sync_server, witself_sync_worker, witself_sync_maintenance, witself_sync_snapshot, witself_sync_legacy;
GRANT SELECT, INSERT, UPDATE, DELETE, TRUNCATE ON TABLE agent_messages TO witself_sync_server, witself_sync_worker, witself_sync_maintenance, witself_sync_legacy;
GRANT SELECT ON TABLE agent_messages TO witself_sync_snapshot;
REVOKE ALL ON TABLE agent_vault_key_enrollments FROM PUBLIC, witself_sync_server, witself_sync_worker, witself_sync_maintenance, witself_sync_snapshot, witself_sync_legacy;
GRANT SELECT, INSERT, UPDATE, DELETE, TRUNCATE ON TABLE agent_vault_key_enrollments TO witself_sync_server, witself_sync_worker, witself_sync_maintenance, witself_sync_legacy;
GRANT SELECT ON TABLE agent_vault_key_enrollments TO witself_sync_snapshot;
REVOKE ALL ON TABLE agent_vault_key_rotation_items FROM PUBLIC, witself_sync_server, witself_sync_worker, witself_sync_maintenance, witself_sync_snapshot, witself_sync_legacy;
GRANT SELECT, INSERT, UPDATE, DELETE, TRUNCATE ON TABLE agent_vault_key_rotation_items TO witself_sync_server, witself_sync_worker, witself_sync_maintenance, witself_sync_legacy;
GRANT SELECT ON TABLE agent_vault_key_rotation_items TO witself_sync_snapshot;
REVOKE ALL ON TABLE agent_vault_key_rotations FROM PUBLIC, witself_sync_server, witself_sync_worker, witself_sync_maintenance, witself_sync_snapshot, witself_sync_legacy;
GRANT SELECT, INSERT, UPDATE, DELETE, TRUNCATE ON TABLE agent_vault_key_rotations TO witself_sync_server, witself_sync_worker, witself_sync_maintenance, witself_sync_legacy;
GRANT SELECT ON TABLE agent_vault_key_rotations TO witself_sync_snapshot;
REVOKE ALL ON TABLE agent_vault_keys FROM PUBLIC, witself_sync_server, witself_sync_worker, witself_sync_maintenance, witself_sync_snapshot, witself_sync_legacy;
GRANT SELECT, INSERT, UPDATE, DELETE, TRUNCATE ON TABLE agent_vault_keys TO witself_sync_server, witself_sync_worker, witself_sync_maintenance, witself_sync_legacy;
GRANT SELECT ON TABLE agent_vault_keys TO witself_sync_snapshot;
REVOKE ALL ON TABLE agents FROM PUBLIC, witself_sync_server, witself_sync_worker, witself_sync_maintenance, witself_sync_snapshot, witself_sync_legacy;
GRANT SELECT, INSERT, UPDATE, DELETE, TRUNCATE ON TABLE agents TO witself_sync_server, witself_sync_worker, witself_sync_maintenance, witself_sync_legacy;
GRANT SELECT ON TABLE agents TO witself_sync_snapshot;
REVOKE ALL ON TABLE avatar_mutation_receipts FROM PUBLIC, witself_sync_server, witself_sync_worker, witself_sync_maintenance, witself_sync_snapshot, witself_sync_legacy;
GRANT SELECT, INSERT, UPDATE, DELETE, TRUNCATE ON TABLE avatar_mutation_receipts TO witself_sync_server, witself_sync_worker, witself_sync_maintenance, witself_sync_legacy;
GRANT SELECT ON TABLE avatar_mutation_receipts TO witself_sync_snapshot;
REVOKE ALL ON TABLE avatar_style_pack_versions FROM PUBLIC, witself_sync_server, witself_sync_worker, witself_sync_maintenance, witself_sync_snapshot, witself_sync_legacy;
GRANT SELECT, INSERT, UPDATE, DELETE, TRUNCATE ON TABLE avatar_style_pack_versions TO witself_sync_server, witself_sync_worker, witself_sync_maintenance, witself_sync_legacy;
GRANT SELECT ON TABLE avatar_style_pack_versions TO witself_sync_snapshot;
REVOKE ALL ON TABLE avatar_style_packs FROM PUBLIC, witself_sync_server, witself_sync_worker, witself_sync_maintenance, witself_sync_snapshot, witself_sync_legacy;
GRANT SELECT, INSERT, UPDATE, DELETE, TRUNCATE ON TABLE avatar_style_packs TO witself_sync_server, witself_sync_worker, witself_sync_maintenance, witself_sync_legacy;
GRANT SELECT ON TABLE avatar_style_packs TO witself_sync_snapshot;
REVOKE ALL ON TABLE avatar_style_rollout_jobs FROM PUBLIC, witself_sync_server, witself_sync_worker, witself_sync_maintenance, witself_sync_snapshot, witself_sync_legacy;
GRANT SELECT, INSERT, UPDATE, DELETE, TRUNCATE ON TABLE avatar_style_rollout_jobs TO witself_sync_server, witself_sync_worker, witself_sync_maintenance, witself_sync_legacy;
GRANT SELECT ON TABLE avatar_style_rollout_jobs TO witself_sync_snapshot;
REVOKE ALL ON TABLE fact_assertions FROM PUBLIC, witself_sync_server, witself_sync_worker, witself_sync_maintenance, witself_sync_snapshot, witself_sync_legacy;
GRANT SELECT, INSERT, UPDATE, DELETE, TRUNCATE ON TABLE fact_assertions TO witself_sync_server, witself_sync_worker, witself_sync_maintenance, witself_sync_legacy;
GRANT SELECT ON TABLE fact_assertions TO witself_sync_snapshot;
REVOKE ALL ON TABLE fact_candidates FROM PUBLIC, witself_sync_server, witself_sync_worker, witself_sync_maintenance, witself_sync_snapshot, witself_sync_legacy;
GRANT SELECT, INSERT, UPDATE, DELETE, TRUNCATE ON TABLE fact_candidates TO witself_sync_server, witself_sync_worker, witself_sync_maintenance, witself_sync_legacy;
GRANT SELECT ON TABLE fact_candidates TO witself_sync_snapshot;
REVOKE ALL ON TABLE fact_mutation_tombstones FROM PUBLIC, witself_sync_server, witself_sync_worker, witself_sync_maintenance, witself_sync_snapshot, witself_sync_legacy;
GRANT SELECT, INSERT, UPDATE, DELETE, TRUNCATE ON TABLE fact_mutation_tombstones TO witself_sync_server, witself_sync_worker, witself_sync_maintenance, witself_sync_legacy;
GRANT SELECT ON TABLE fact_mutation_tombstones TO witself_sync_snapshot;
REVOKE ALL ON TABLE fact_subjects FROM PUBLIC, witself_sync_server, witself_sync_worker, witself_sync_maintenance, witself_sync_snapshot, witself_sync_legacy;
GRANT SELECT, INSERT, UPDATE, DELETE, TRUNCATE ON TABLE fact_subjects TO witself_sync_server, witself_sync_worker, witself_sync_maintenance, witself_sync_legacy;
GRANT SELECT ON TABLE fact_subjects TO witself_sync_snapshot;
REVOKE ALL ON TABLE facts FROM PUBLIC, witself_sync_server, witself_sync_worker, witself_sync_maintenance, witself_sync_snapshot, witself_sync_legacy;
GRANT SELECT, INSERT, UPDATE, DELETE, TRUNCATE ON TABLE facts TO witself_sync_server, witself_sync_worker, witself_sync_maintenance, witself_sync_legacy;
GRANT SELECT ON TABLE facts TO witself_sync_snapshot;
REVOKE ALL ON TABLE memories FROM PUBLIC, witself_sync_server, witself_sync_worker, witself_sync_maintenance, witself_sync_snapshot, witself_sync_legacy;
GRANT SELECT, INSERT, UPDATE, DELETE, TRUNCATE ON TABLE memories TO witself_sync_server, witself_sync_worker, witself_sync_maintenance, witself_sync_legacy;
GRANT SELECT ON TABLE memories TO witself_sync_snapshot;
REVOKE ALL ON TABLE memory_change_clocks FROM PUBLIC, witself_sync_server, witself_sync_worker, witself_sync_maintenance, witself_sync_snapshot, witself_sync_legacy;
GRANT SELECT, INSERT, UPDATE, DELETE, TRUNCATE ON TABLE memory_change_clocks TO witself_sync_server, witself_sync_worker, witself_sync_maintenance, witself_sync_legacy;
GRANT SELECT ON TABLE memory_change_clocks TO witself_sync_snapshot;
REVOKE ALL ON TABLE memory_curation_actions FROM PUBLIC, witself_sync_server, witself_sync_worker, witself_sync_maintenance, witself_sync_snapshot, witself_sync_legacy;
GRANT SELECT, INSERT, UPDATE, DELETE, TRUNCATE ON TABLE memory_curation_actions TO witself_sync_server, witself_sync_worker, witself_sync_maintenance, witself_sync_legacy;
GRANT SELECT ON TABLE memory_curation_actions TO witself_sync_snapshot;
REVOKE ALL ON TABLE memory_curation_cursors FROM PUBLIC, witself_sync_server, witself_sync_worker, witself_sync_maintenance, witself_sync_snapshot, witself_sync_legacy;
GRANT SELECT, INSERT, UPDATE, DELETE, TRUNCATE ON TABLE memory_curation_cursors TO witself_sync_server, witself_sync_worker, witself_sync_maintenance, witself_sync_legacy;
GRANT SELECT ON TABLE memory_curation_cursors TO witself_sync_snapshot;
REVOKE ALL ON TABLE memory_curation_lanes FROM PUBLIC, witself_sync_server, witself_sync_worker, witself_sync_maintenance, witself_sync_snapshot, witself_sync_legacy;
GRANT SELECT, INSERT, UPDATE, DELETE, TRUNCATE ON TABLE memory_curation_lanes TO witself_sync_server, witself_sync_worker, witself_sync_maintenance, witself_sync_legacy;
GRANT SELECT ON TABLE memory_curation_lanes TO witself_sync_snapshot;
REVOKE ALL ON TABLE memory_curation_mutations FROM PUBLIC, witself_sync_server, witself_sync_worker, witself_sync_maintenance, witself_sync_snapshot, witself_sync_legacy;
GRANT SELECT, INSERT, UPDATE, DELETE, TRUNCATE ON TABLE memory_curation_mutations TO witself_sync_server, witself_sync_worker, witself_sync_maintenance, witself_sync_legacy;
GRANT SELECT ON TABLE memory_curation_mutations TO witself_sync_snapshot;
REVOKE ALL ON TABLE memory_curation_requests FROM PUBLIC, witself_sync_server, witself_sync_worker, witself_sync_maintenance, witself_sync_snapshot, witself_sync_legacy;
GRANT SELECT, INSERT, UPDATE, DELETE, TRUNCATE ON TABLE memory_curation_requests TO witself_sync_server, witself_sync_worker, witself_sync_maintenance, witself_sync_legacy;
GRANT SELECT ON TABLE memory_curation_requests TO witself_sync_snapshot;
REVOKE ALL ON TABLE memory_curation_run_inputs FROM PUBLIC, witself_sync_server, witself_sync_worker, witself_sync_maintenance, witself_sync_snapshot, witself_sync_legacy;
GRANT SELECT, INSERT, UPDATE, DELETE, TRUNCATE ON TABLE memory_curation_run_inputs TO witself_sync_server, witself_sync_worker, witself_sync_maintenance, witself_sync_legacy;
GRANT SELECT ON TABLE memory_curation_run_inputs TO witself_sync_snapshot;
REVOKE ALL ON TABLE memory_curation_runs FROM PUBLIC, witself_sync_server, witself_sync_worker, witself_sync_maintenance, witself_sync_snapshot, witself_sync_legacy;
GRANT SELECT, INSERT, UPDATE, DELETE, TRUNCATE ON TABLE memory_curation_runs TO witself_sync_server, witself_sync_worker, witself_sync_maintenance, witself_sync_legacy;
GRANT SELECT ON TABLE memory_curation_runs TO witself_sync_snapshot;
REVOKE ALL ON TABLE memory_deleted_references FROM PUBLIC, witself_sync_server, witself_sync_worker, witself_sync_maintenance, witself_sync_snapshot, witself_sync_legacy;
GRANT SELECT, INSERT, UPDATE, DELETE, TRUNCATE ON TABLE memory_deleted_references TO witself_sync_server, witself_sync_worker, witself_sync_maintenance, witself_sync_legacy;
GRANT SELECT ON TABLE memory_deleted_references TO witself_sync_snapshot;
REVOKE ALL ON TABLE memory_evidence FROM PUBLIC, witself_sync_server, witself_sync_worker, witself_sync_maintenance, witself_sync_snapshot, witself_sync_legacy;
GRANT SELECT, INSERT, UPDATE, DELETE, TRUNCATE ON TABLE memory_evidence TO witself_sync_server, witself_sync_worker, witself_sync_maintenance, witself_sync_legacy;
GRANT SELECT ON TABLE memory_evidence TO witself_sync_snapshot;
REVOKE ALL ON TABLE memory_relations FROM PUBLIC, witself_sync_server, witself_sync_worker, witself_sync_maintenance, witself_sync_snapshot, witself_sync_legacy;
GRANT SELECT, INSERT, UPDATE, DELETE, TRUNCATE ON TABLE memory_relations TO witself_sync_server, witself_sync_worker, witself_sync_maintenance, witself_sync_legacy;
GRANT SELECT ON TABLE memory_relations TO witself_sync_snapshot;
REVOKE ALL ON TABLE memory_vector_profiles FROM PUBLIC, witself_sync_server, witself_sync_worker, witself_sync_maintenance, witself_sync_snapshot, witself_sync_legacy;
GRANT SELECT, INSERT, UPDATE, DELETE, TRUNCATE ON TABLE memory_vector_profiles TO witself_sync_server, witself_sync_worker, witself_sync_maintenance, witself_sync_legacy;
GRANT SELECT ON TABLE memory_vector_profiles TO witself_sync_snapshot;
REVOKE ALL ON TABLE memory_vectors FROM PUBLIC, witself_sync_server, witself_sync_worker, witself_sync_maintenance, witself_sync_snapshot, witself_sync_legacy;
GRANT SELECT, INSERT, UPDATE, DELETE, TRUNCATE ON TABLE memory_vectors TO witself_sync_server, witself_sync_worker, witself_sync_maintenance, witself_sync_legacy;
GRANT SELECT ON TABLE memory_vectors TO witself_sync_snapshot;
REVOKE ALL ON TABLE memory_versions FROM PUBLIC, witself_sync_server, witself_sync_worker, witself_sync_maintenance, witself_sync_snapshot, witself_sync_legacy;
GRANT SELECT, INSERT, UPDATE, DELETE, TRUNCATE ON TABLE memory_versions TO witself_sync_server, witself_sync_worker, witself_sync_maintenance, witself_sync_legacy;
GRANT SELECT ON TABLE memory_versions TO witself_sync_snapshot;
REVOKE ALL ON TABLE message_retention_account_scan_state FROM PUBLIC, witself_sync_server, witself_sync_worker, witself_sync_maintenance, witself_sync_snapshot, witself_sync_legacy;
GRANT SELECT, INSERT, UPDATE, DELETE, TRUNCATE ON TABLE message_retention_account_scan_state TO witself_sync_server, witself_sync_worker, witself_sync_maintenance, witself_sync_legacy;
GRANT SELECT ON TABLE message_retention_account_scan_state TO witself_sync_snapshot;
REVOKE ALL ON TABLE message_retention_thread_activity FROM PUBLIC, witself_sync_server, witself_sync_worker, witself_sync_maintenance, witself_sync_snapshot, witself_sync_legacy;
GRANT SELECT, INSERT, UPDATE, DELETE, TRUNCATE ON TABLE message_retention_thread_activity TO witself_sync_server, witself_sync_worker, witself_sync_maintenance, witself_sync_legacy;
GRANT SELECT ON TABLE message_retention_thread_activity TO witself_sync_snapshot;
REVOKE ALL ON TABLE operators FROM PUBLIC, witself_sync_server, witself_sync_worker, witself_sync_maintenance, witself_sync_snapshot, witself_sync_legacy;
GRANT SELECT, INSERT, UPDATE, DELETE, TRUNCATE ON TABLE operators TO witself_sync_server, witself_sync_worker, witself_sync_maintenance, witself_sync_legacy;
GRANT SELECT ON TABLE operators TO witself_sync_snapshot;
REVOKE ALL ON TABLE realm_avatar_styles FROM PUBLIC, witself_sync_server, witself_sync_worker, witself_sync_maintenance, witself_sync_snapshot, witself_sync_legacy;
GRANT SELECT, INSERT, UPDATE, DELETE, TRUNCATE ON TABLE realm_avatar_styles TO witself_sync_server, witself_sync_worker, witself_sync_maintenance, witself_sync_legacy;
GRANT SELECT ON TABLE realm_avatar_styles TO witself_sync_snapshot;
REVOKE ALL ON TABLE realms FROM PUBLIC, witself_sync_server, witself_sync_worker, witself_sync_maintenance, witself_sync_snapshot, witself_sync_legacy;
GRANT SELECT, INSERT, UPDATE, DELETE, TRUNCATE ON TABLE realms TO witself_sync_server, witself_sync_worker, witself_sync_maintenance, witself_sync_legacy;
GRANT SELECT ON TABLE realms TO witself_sync_snapshot;
REVOKE ALL ON TABLE secret_deks FROM PUBLIC, witself_sync_server, witself_sync_worker, witself_sync_maintenance, witself_sync_snapshot, witself_sync_legacy;
GRANT SELECT, INSERT, UPDATE, DELETE, TRUNCATE ON TABLE secret_deks TO witself_sync_server, witself_sync_worker, witself_sync_maintenance, witself_sync_legacy;
GRANT SELECT ON TABLE secret_deks TO witself_sync_snapshot;
REVOKE ALL ON TABLE secret_fields FROM PUBLIC, witself_sync_server, witself_sync_worker, witself_sync_maintenance, witself_sync_snapshot, witself_sync_legacy;
GRANT SELECT, INSERT, UPDATE, DELETE, TRUNCATE ON TABLE secret_fields TO witself_sync_server, witself_sync_worker, witself_sync_maintenance, witself_sync_legacy;
GRANT SELECT ON TABLE secret_fields TO witself_sync_snapshot;
REVOKE ALL ON TABLE secret_mutation_receipts FROM PUBLIC, witself_sync_server, witself_sync_worker, witself_sync_maintenance, witself_sync_snapshot, witself_sync_legacy;
GRANT SELECT, INSERT, UPDATE, DELETE, TRUNCATE ON TABLE secret_mutation_receipts TO witself_sync_server, witself_sync_worker, witself_sync_maintenance, witself_sync_legacy;
GRANT SELECT ON TABLE secret_mutation_receipts TO witself_sync_snapshot;
REVOKE ALL ON TABLE secrets FROM PUBLIC, witself_sync_server, witself_sync_worker, witself_sync_maintenance, witself_sync_snapshot, witself_sync_legacy;
GRANT SELECT, INSERT, UPDATE, DELETE, TRUNCATE ON TABLE secrets TO witself_sync_server, witself_sync_worker, witself_sync_maintenance, witself_sync_legacy;
GRANT SELECT ON TABLE secrets TO witself_sync_snapshot;
REVOKE ALL ON TABLE support_ticket_messages FROM PUBLIC, witself_sync_server, witself_sync_worker, witself_sync_maintenance, witself_sync_snapshot, witself_sync_legacy;
GRANT SELECT, INSERT, UPDATE, DELETE, TRUNCATE ON TABLE support_ticket_messages TO witself_sync_server, witself_sync_worker, witself_sync_maintenance, witself_sync_legacy;
GRANT SELECT ON TABLE support_ticket_messages TO witself_sync_snapshot;
REVOKE ALL ON TABLE support_tickets FROM PUBLIC, witself_sync_server, witself_sync_worker, witself_sync_maintenance, witself_sync_snapshot, witself_sync_legacy;
GRANT SELECT, INSERT, UPDATE, DELETE, TRUNCATE ON TABLE support_tickets TO witself_sync_server, witself_sync_worker, witself_sync_maintenance, witself_sync_legacy;
GRANT SELECT ON TABLE support_tickets TO witself_sync_snapshot;
REVOKE ALL ON TABLE tokens FROM PUBLIC, witself_sync_server, witself_sync_worker, witself_sync_maintenance, witself_sync_snapshot, witself_sync_legacy;
GRANT SELECT, INSERT, UPDATE, DELETE, TRUNCATE ON TABLE tokens TO witself_sync_server, witself_sync_worker, witself_sync_maintenance, witself_sync_legacy;
GRANT SELECT ON TABLE tokens TO witself_sync_snapshot;
REVOKE ALL ON TABLE transcript_conversations FROM PUBLIC, witself_sync_server, witself_sync_worker, witself_sync_maintenance, witself_sync_snapshot, witself_sync_legacy;
GRANT SELECT, INSERT, UPDATE, DELETE, TRUNCATE ON TABLE transcript_conversations TO witself_sync_server, witself_sync_worker, witself_sync_maintenance, witself_sync_legacy;
GRANT SELECT ON TABLE transcript_conversations TO witself_sync_snapshot;
REVOKE ALL ON TABLE transcript_entries FROM PUBLIC, witself_sync_server, witself_sync_worker, witself_sync_maintenance, witself_sync_snapshot, witself_sync_legacy;
GRANT SELECT, INSERT, UPDATE, DELETE, TRUNCATE ON TABLE transcript_entries TO witself_sync_server, witself_sync_worker, witself_sync_maintenance, witself_sync_legacy;
GRANT SELECT ON TABLE transcript_entries TO witself_sync_snapshot;
REVOKE ALL ON TABLE transcript_retention_account_scan_state FROM PUBLIC, witself_sync_server, witself_sync_worker, witself_sync_maintenance, witself_sync_snapshot, witself_sync_legacy;
GRANT SELECT, INSERT, UPDATE, DELETE, TRUNCATE ON TABLE transcript_retention_account_scan_state TO witself_sync_server, witself_sync_worker, witself_sync_maintenance, witself_sync_legacy;
GRANT SELECT ON TABLE transcript_retention_account_scan_state TO witself_sync_snapshot;
REVOKE ALL ON TABLE transcript_storage_usage FROM PUBLIC, witself_sync_server, witself_sync_worker, witself_sync_maintenance, witself_sync_snapshot, witself_sync_legacy;
GRANT SELECT, INSERT, UPDATE, DELETE, TRUNCATE ON TABLE transcript_storage_usage TO witself_sync_server, witself_sync_worker, witself_sync_maintenance, witself_sync_legacy;
GRANT SELECT ON TABLE transcript_storage_usage TO witself_sync_snapshot;
REVOKE ALL ON TABLE usage_events FROM PUBLIC, witself_sync_server, witself_sync_worker, witself_sync_maintenance, witself_sync_snapshot, witself_sync_legacy;
GRANT SELECT, INSERT, UPDATE, DELETE, TRUNCATE ON TABLE usage_events TO witself_sync_server, witself_sync_worker, witself_sync_maintenance, witself_sync_legacy;
GRANT SELECT ON TABLE usage_events TO witself_sync_snapshot;
REVOKE ALL ON TABLE usage_rollups FROM PUBLIC, witself_sync_server, witself_sync_worker, witself_sync_maintenance, witself_sync_snapshot, witself_sync_legacy;
GRANT SELECT, INSERT, UPDATE, DELETE, TRUNCATE ON TABLE usage_rollups TO witself_sync_server, witself_sync_worker, witself_sync_maintenance, witself_sync_legacy;
GRANT SELECT ON TABLE usage_rollups TO witself_sync_snapshot;
REVOKE ALL ON TABLE vault_key_enrollment_receipts FROM PUBLIC, witself_sync_server, witself_sync_worker, witself_sync_maintenance, witself_sync_snapshot, witself_sync_legacy;
GRANT SELECT, INSERT, UPDATE, DELETE, TRUNCATE ON TABLE vault_key_enrollment_receipts TO witself_sync_server, witself_sync_worker, witself_sync_maintenance, witself_sync_legacy;
GRANT SELECT ON TABLE vault_key_enrollment_receipts TO witself_sync_snapshot;
REVOKE ALL ON TABLE vault_key_rotation_receipts FROM PUBLIC, witself_sync_server, witself_sync_worker, witself_sync_maintenance, witself_sync_snapshot, witself_sync_legacy;
GRANT SELECT, INSERT, UPDATE, DELETE, TRUNCATE ON TABLE vault_key_rotation_receipts TO witself_sync_server, witself_sync_worker, witself_sync_maintenance, witself_sync_legacy;
GRANT SELECT ON TABLE vault_key_rotation_receipts TO witself_sync_snapshot;
REVOKE ALL ON TABLE sync_incarnations FROM PUBLIC, witself_sync_server, witself_sync_worker, witself_sync_maintenance, witself_sync_snapshot, witself_sync_legacy;
GRANT SELECT ON TABLE sync_incarnations TO witself_sync_server, witself_sync_worker, witself_sync_maintenance, witself_sync_snapshot, witself_sync_legacy;
REVOKE ALL ON TABLE sync_operations FROM PUBLIC, witself_sync_server, witself_sync_worker, witself_sync_maintenance, witself_sync_snapshot, witself_sync_legacy;
GRANT SELECT ON TABLE sync_operations TO witself_sync_server, witself_sync_worker, witself_sync_maintenance, witself_sync_snapshot, witself_sync_legacy;
REVOKE ALL ON TABLE sync_operation_payloads FROM PUBLIC, witself_sync_server, witself_sync_worker, witself_sync_maintenance, witself_sync_snapshot, witself_sync_legacy;
GRANT SELECT ON TABLE sync_operation_payloads TO witself_sync_server, witself_sync_worker, witself_sync_maintenance, witself_sync_snapshot, witself_sync_legacy;
REVOKE ALL ON TABLE sync_verifier_history FROM PUBLIC, witself_sync_server, witself_sync_worker, witself_sync_maintenance, witself_sync_snapshot, witself_sync_legacy;
GRANT SELECT ON TABLE sync_verifier_history TO witself_sync_server, witself_sync_worker, witself_sync_maintenance, witself_sync_snapshot, witself_sync_legacy;
REVOKE ALL ON TABLE sync_receipts FROM PUBLIC, witself_sync_server, witself_sync_worker, witself_sync_maintenance, witself_sync_snapshot, witself_sync_legacy;
GRANT SELECT ON TABLE sync_receipts TO witself_sync_server, witself_sync_worker, witself_sync_maintenance, witself_sync_snapshot, witself_sync_legacy;
REVOKE ALL ON TABLE sync_reuse_refusals FROM PUBLIC, witself_sync_server, witself_sync_worker, witself_sync_maintenance, witself_sync_snapshot, witself_sync_legacy;
GRANT SELECT ON TABLE sync_reuse_refusals TO witself_sync_server, witself_sync_worker, witself_sync_maintenance, witself_sync_snapshot, witself_sync_legacy;
REVOKE ALL ON TABLE sync_capture FROM PUBLIC, witself_sync_server, witself_sync_worker, witself_sync_maintenance, witself_sync_snapshot, witself_sync_legacy;
GRANT SELECT ON TABLE sync_capture TO witself_sync_server, witself_sync_worker, witself_sync_maintenance, witself_sync_snapshot, witself_sync_legacy;
REVOKE ALL ON TABLE sync_writer_capabilities FROM PUBLIC, witself_sync_server, witself_sync_worker, witself_sync_maintenance, witself_sync_snapshot, witself_sync_legacy;
GRANT SELECT ON TABLE sync_writer_capabilities TO witself_sync_server, witself_sync_worker, witself_sync_maintenance, witself_sync_snapshot, witself_sync_legacy;
REVOKE ALL ON TABLE sync_tx_admissions FROM PUBLIC, witself_sync_server, witself_sync_worker, witself_sync_maintenance, witself_sync_snapshot, witself_sync_legacy;
GRANT SELECT ON TABLE sync_tx_admissions TO witself_sync_server, witself_sync_worker, witself_sync_maintenance, witself_sync_snapshot, witself_sync_legacy;
REVOKE ALL ON TABLE sync_tx_touches FROM PUBLIC, witself_sync_server, witself_sync_worker, witself_sync_maintenance, witself_sync_snapshot, witself_sync_legacy;
GRANT SELECT ON TABLE sync_tx_touches TO witself_sync_server, witself_sync_worker, witself_sync_maintenance, witself_sync_snapshot, witself_sync_legacy;
REVOKE ALL ON TABLE sync_finalized_counter FROM PUBLIC, witself_sync_server, witself_sync_worker, witself_sync_maintenance, witself_sync_snapshot, witself_sync_legacy;
GRANT SELECT ON TABLE sync_finalized_counter TO witself_sync_server, witself_sync_worker, witself_sync_maintenance, witself_sync_snapshot, witself_sync_legacy;
REVOKE ALL ON TABLE sync_bundles FROM PUBLIC, witself_sync_server, witself_sync_worker, witself_sync_maintenance, witself_sync_snapshot, witself_sync_legacy;
GRANT SELECT ON TABLE sync_bundles TO witself_sync_server, witself_sync_worker, witself_sync_maintenance, witself_sync_snapshot, witself_sync_legacy;
REVOKE ALL ON TABLE sync_bundle_operations FROM PUBLIC, witself_sync_server, witself_sync_worker, witself_sync_maintenance, witself_sync_snapshot, witself_sync_legacy;
GRANT SELECT ON TABLE sync_bundle_operations TO witself_sync_server, witself_sync_worker, witself_sync_maintenance, witself_sync_snapshot, witself_sync_legacy;
REVOKE ALL ON TABLE sync_publication_outbox FROM PUBLIC, witself_sync_server, witself_sync_worker, witself_sync_maintenance, witself_sync_snapshot, witself_sync_legacy;
GRANT SELECT ON TABLE sync_publication_outbox TO witself_sync_server, witself_sync_worker, witself_sync_maintenance, witself_sync_snapshot, witself_sync_legacy;
REVOKE ALL ON TABLE sync_acceptance_counter FROM PUBLIC, witself_sync_server, witself_sync_worker, witself_sync_maintenance, witself_sync_snapshot, witself_sync_legacy;
GRANT SELECT ON TABLE sync_acceptance_counter TO witself_sync_server, witself_sync_worker, witself_sync_maintenance, witself_sync_snapshot, witself_sync_legacy;
REVOKE ALL ON TABLE sync_resource_budgets FROM PUBLIC, witself_sync_server, witself_sync_worker, witself_sync_maintenance, witself_sync_snapshot, witself_sync_legacy;
GRANT SELECT ON TABLE sync_resource_budgets TO witself_sync_server, witself_sync_worker, witself_sync_maintenance, witself_sync_snapshot, witself_sync_legacy;
REVOKE ALL ON TABLE sync_resource_reservations FROM PUBLIC, witself_sync_server, witself_sync_worker, witself_sync_maintenance, witself_sync_snapshot, witself_sync_legacy;
GRANT SELECT ON TABLE sync_resource_reservations TO witself_sync_server, witself_sync_worker, witself_sync_maintenance, witself_sync_snapshot, witself_sync_legacy;
REVOKE ALL ON TABLE sync_deletion_knowledge FROM PUBLIC, witself_sync_server, witself_sync_worker, witself_sync_maintenance, witself_sync_snapshot, witself_sync_legacy;
GRANT SELECT ON TABLE sync_deletion_knowledge TO witself_sync_server, witself_sync_worker, witself_sync_maintenance, witself_sync_snapshot, witself_sync_legacy;
REVOKE ALL ON TABLE sync_retained_evidence FROM PUBLIC, witself_sync_server, witself_sync_worker, witself_sync_maintenance, witself_sync_snapshot, witself_sync_legacy;
GRANT SELECT ON TABLE sync_retained_evidence TO witself_sync_server, witself_sync_worker, witself_sync_maintenance, witself_sync_snapshot, witself_sync_legacy;
REVOKE ALL ON TABLE sync_evidence_pins FROM PUBLIC, witself_sync_server, witself_sync_worker, witself_sync_maintenance, witself_sync_snapshot, witself_sync_legacy;
GRANT SELECT ON TABLE sync_evidence_pins TO witself_sync_server, witself_sync_worker, witself_sync_maintenance, witself_sync_snapshot, witself_sync_legacy;
REVOKE ALL ON TABLE sync_payload_retirements FROM PUBLIC, witself_sync_server, witself_sync_worker, witself_sync_maintenance, witself_sync_snapshot, witself_sync_legacy;
GRANT SELECT ON TABLE sync_payload_retirements TO witself_sync_server, witself_sync_worker, witself_sync_maintenance, witself_sync_snapshot, witself_sync_legacy;
REVOKE ALL ON TABLE witself_sync_schema_version FROM PUBLIC, witself_sync_server, witself_sync_worker, witself_sync_maintenance, witself_sync_snapshot, witself_sync_legacy;
GRANT SELECT ON TABLE witself_sync_schema_version TO witself_sync_server, witself_sync_worker, witself_sync_maintenance, witself_sync_snapshot, witself_sync_legacy;
REVOKE EXECUTE ON FUNCTION capture_lock_key(text) FROM PUBLIC, witself_sync_server, witself_sync_worker, witself_sync_maintenance, witself_sync_snapshot, witself_sync_legacy;
GRANT EXECUTE ON FUNCTION capture_lock_key(text) TO witself_sync_server, witself_sync_worker, witself_sync_maintenance, witself_sync_snapshot, witself_sync_legacy;
REVOKE EXECUTE ON FUNCTION witself_sync_writer_guard() FROM PUBLIC, witself_sync_server, witself_sync_worker, witself_sync_maintenance, witself_sync_snapshot, witself_sync_legacy;
REVOKE EXECUTE ON FUNCTION witself_sync_check_coverage() FROM PUBLIC, witself_sync_server, witself_sync_worker, witself_sync_maintenance, witself_sync_snapshot, witself_sync_legacy;
REVOKE EXECUTE ON FUNCTION witself_sync_immutable_evidence() FROM PUBLIC, witself_sync_server, witself_sync_worker, witself_sync_maintenance, witself_sync_snapshot, witself_sync_legacy;
REVOKE EXECUTE ON FUNCTION witself_sync_publication_transition() FROM PUBLIC, witself_sync_server, witself_sync_worker, witself_sync_maintenance, witself_sync_snapshot, witself_sync_legacy;
REVOKE EXECUTE ON FUNCTION sync_admit(text,text,text) FROM PUBLIC, witself_sync_server, witself_sync_worker, witself_sync_maintenance, witself_sync_snapshot, witself_sync_legacy;
GRANT EXECUTE ON FUNCTION sync_admit(text,text,text) TO witself_sync_server, witself_sync_worker, witself_sync_maintenance;
REVOKE EXECUTE ON FUNCTION sync_touch(text,bigint,text,text,jsonb,jsonb,bytea,bytea,bytea) FROM PUBLIC, witself_sync_server, witself_sync_worker, witself_sync_maintenance, witself_sync_snapshot, witself_sync_legacy;
GRANT EXECUTE ON FUNCTION sync_touch(text,bigint,text,text,jsonb,jsonb,bytea,bytea,bytea) TO witself_sync_server, witself_sync_worker, witself_sync_maintenance;
REVOKE EXECUTE ON FUNCTION sync_seal(text,bigint,bytea) FROM PUBLIC, witself_sync_server, witself_sync_worker, witself_sync_maintenance, witself_sync_snapshot, witself_sync_legacy;
GRANT EXECUTE ON FUNCTION sync_seal(text,bigint,bytea) TO witself_sync_server, witself_sync_worker, witself_sync_maintenance;
REVOKE EXECUTE ON FUNCTION sync_finalize(text,bigint) FROM PUBLIC, witself_sync_server, witself_sync_worker, witself_sync_maintenance, witself_sync_snapshot, witself_sync_legacy;
GRANT EXECUTE ON FUNCTION sync_finalize(text,bigint) TO witself_sync_server, witself_sync_worker, witself_sync_maintenance;
-- Creator default ACLs and earlier guard grants may name unrelated roles.
-- Close the six protected function ACLs to the reviewed owner/runtime set;
-- catalog identities are used only to revoke unexpected grants, never admit.
-- +goose StatementBegin
DO $$
DECLARE unexpected record;
BEGIN
 FOR unexpected IN
  SELECT p.oid::regprocedure AS signature,a.grantee,r.rolname
  FROM pg_proc p JOIN pg_namespace n ON n.oid=p.pronamespace
  CROSS JOIN LATERAL aclexplode(coalesce(p.proacl,acldefault('f',p.proowner))) a
  LEFT JOIN pg_roles r ON r.oid=a.grantee
  WHERE n.nspname=current_schema() AND p.oid=ANY(ARRAY[
   'sync_admit(text,text,text)'::regprocedure,
   'sync_touch(text,bigint,text,text,jsonb,jsonb,bytea,bytea,bytea)'::regprocedure,
   'sync_seal(text,bigint,bytea)'::regprocedure,
   'sync_finalize(text,bigint)'::regprocedure,
   'witself_sync_writer_guard()'::regprocedure,
   'witself_sync_check_coverage()'::regprocedure])
  AND a.privilege_type='EXECUTE' AND a.grantee<>p.proowner
  AND NOT (p.proname IN ('sync_admit','sync_touch','sync_seal','sync_finalize')
   AND coalesce(r.rolname IN ('witself_sync_server','witself_sync_worker','witself_sync_maintenance'),false))
 LOOP
  IF unexpected.grantee=0 THEN
   EXECUTE format('REVOKE EXECUTE ON FUNCTION %s FROM PUBLIC CASCADE',unexpected.signature);
  ELSE
   EXECUTE format('REVOKE EXECUTE ON FUNCTION %s FROM %I CASCADE',unexpected.signature,unexpected.rolname);
  END IF;
 END LOOP;
END $$;
-- +goose StatementEnd
-- Table REVOKE does not revoke existing column ACLs. Remove those too before
-- runtime access is restored, including snapshot column DML on covered tables.
-- +goose StatementBegin
DO $$
DECLARE relation_name text; column_names text;
BEGIN
 FOR relation_name,column_names IN
  SELECT c.relname,string_agg(quote_ident(a.attname),',' ORDER BY a.attnum)
  FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace
  JOIN pg_attribute a ON a.attrelid=c.oid AND a.attnum>0 AND NOT a.attisdropped
  WHERE n.nspname=current_schema() AND c.relkind IN ('r','p')
  GROUP BY c.relname
 LOOP
  EXECUTE format('REVOKE ALL (%s) ON TABLE %I FROM PUBLIC, witself_sync_server, witself_sync_worker, witself_sync_maintenance, witself_sync_snapshot, witself_sync_legacy',column_names,relation_name);
 END LOOP;
END $$;
-- +goose StatementEnd
-- +goose StatementBegin
DO $$ BEGIN
 EXECUTE format('ALTER SCHEMA %I OWNER TO witself_sync_owner',current_schema());
 EXECUTE format('REVOKE ALL ON SCHEMA %I FROM PUBLIC, witself_sync_server, witself_sync_worker, witself_sync_maintenance, witself_sync_snapshot, witself_sync_legacy',current_schema());
 EXECUTE format('GRANT USAGE ON SCHEMA %I TO witself_sync_server, witself_sync_worker, witself_sync_maintenance, witself_sync_snapshot, witself_sync_legacy',current_schema());
 EXECUTE format('GRANT USAGE, SELECT ON ALL SEQUENCES IN SCHEMA %I TO witself_sync_server, witself_sync_worker, witself_sync_maintenance, witself_sync_legacy',current_schema());
END $$;
-- +goose StatementEnd
-- END SYNC PRINCIPAL TOPOLOGY

-- +goose Down
-- +goose StatementBegin
DO $$ BEGIN
 IF NOT EXISTS (SELECT 1 FROM pg_class c JOIN pg_roles r ON r.oid=c.relowner
   WHERE c.oid='sync_capture'::regclass AND r.rolname=current_user)
   AND current_setting('is_superuser')<>'on' THEN
  RAISE EXCEPTION 'sync schema downgrade permission denied' USING ERRCODE='42501';
 END IF;
 RAISE EXCEPTION 'sync schema downgrade is forbidden' USING ERRCODE='23514';
END $$;
-- +goose StatementEnd
