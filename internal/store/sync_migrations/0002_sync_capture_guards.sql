-- +goose Up
-- LC-S6C1: selected local-only capture. No role/principal bypass claim is made.
-- Every ALTER is separate so custody's fail-closed ALTER contract can review it.
ALTER TABLE sync_capture ADD COLUMN protocol_version bigint NOT NULL DEFAULT 1 CHECK (protocol_version=1);
ALTER TABLE sync_capture ADD COLUMN min_writer_schema bigint NOT NULL DEFAULT 2 CHECK (min_writer_schema>=2);
ALTER TABLE sync_capture ADD COLUMN min_writer_capability text NOT NULL DEFAULT 'preferences-v1';
ALTER TABLE sync_capture ADD COLUMN coverage_manifest_version text NOT NULL DEFAULT 'lc-s6c1-v1';
ALTER TABLE sync_capture ADD COLUMN coverage_manifest_commitment bytea NOT NULL DEFAULT decode(repeat('00',32),'hex') CHECK (octet_length(coverage_manifest_commitment)=32);
ALTER TABLE sync_capture ADD COLUMN snapshot_id bytea CHECK (octet_length(snapshot_id)=16);
ALTER TABLE sync_capture ADD COLUMN snapshot_watermark bigint NOT NULL DEFAULT 0 CHECK (snapshot_watermark>=0);
ALTER TABLE sync_capture ADD COLUMN suspension_reason text CHECK (suspension_reason='coverage_lost' OR suspension_reason='capability_revoked' OR suspension_reason='snapshot_lost' OR suspension_reason='rebootstrap_required');
ALTER TABLE sync_writer_capabilities ADD COLUMN capture_epoch bigint NOT NULL DEFAULT 0 CHECK (capture_epoch>=0);
ALTER TABLE sync_writer_capabilities DROP CONSTRAINT sync_writer_capabilities_enabled_check;
ALTER TABLE sync_writer_capabilities ADD CONSTRAINT sync_writer_capabilities_enabled_check CHECK (NOT enabled OR (revoked_generation=0 AND capture_epoch>0));
ALTER TABLE sync_tx_admissions ADD COLUMN actual_touches jsonb NOT NULL DEFAULT '[]'::jsonb CHECK (jsonb_typeof(actual_touches)='array');
ALTER TABLE sync_tx_admissions ADD COLUMN prior_finalized bigint CHECK (prior_finalized>=0);
ALTER TABLE sync_tx_touches ADD COLUMN table_name text;

-- Account gate keys use PostgreSQL's deterministic text hash, never sequence
-- maxima. A hash collision only adds serialization; it cannot admit a writer.
-- +goose StatementBegin
CREATE FUNCTION capture_lock_key(target_account text) RETURNS bigint
LANGUAGE sql IMMUTABLE STRICT AS $$
 SELECT hashtextextended('witself.sync.capture:' || target_account, 0);
$$;
-- +goose StatementEnd

-- The BEFORE trigger refuses before domain mutation. The same function's AFTER
-- trigger records only completed row changes: an UPSERT's speculative INSERT
-- attempt must not become a second semantic event. No trigger acquires a gate.
-- +goose StatementBegin
CREATE FUNCTION witself_sync_writer_guard() RETURNS trigger
LANGUAGE plpgsql AS $$
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
  IF EXISTS (SELECT 1 FROM sync_capture c JOIN sync_writer_capabilities w
      ON w.account_id=c.account_id AND w.capability=c.capability
      WHERE w.enabled AND w.revoked_generation=0) THEN
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
  IF NOT EXISTS (SELECT 1 FROM sync_writer_capabilities w WHERE w.account_id=target_account
      AND w.capability=capture.capability AND w.enabled AND w.revoked_generation=0) THEN
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
    OR admission.capability<>capture.capability THEN
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

-- A deferred check uses exact per-row event identities and table names, plus
-- the operation/bundle membership and counter predecessor. Equal counts cannot
-- hide an omitted second write to the same row. PostgreSQL savepoint rollback
-- restores both actual events and semantic touches together.
-- +goose StatementBegin
CREATE FUNCTION witself_sync_check_coverage() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
 admission sync_tx_admissions%ROWTYPE;
 bundle sync_bundles%ROWTYPE;
 finalized bigint;
BEGIN
 -- Historical synthetic archive fixtures are inert local custody rows.
 -- A live admission always carries the physical PostgreSQL transaction ID.
 IF NEW.tx_id<>txid_current() THEN RETURN NULL; END IF;
 SELECT * INTO admission FROM sync_tx_admissions WHERE account_id=NEW.account_id AND tx_id=NEW.tx_id;
 IF NOT FOUND THEN
  RAISE EXCEPTION 'sync writer not admitted' USING ERRCODE='P0001';
 END IF;
 SELECT * INTO bundle FROM sync_bundles WHERE account_id=NEW.account_id AND tx_id=NEW.tx_id;
 IF NOT FOUND OR NOT admission.sealed OR admission.prior_finalized IS NULL
    OR bundle.finalized<>admission.prior_finalized+1
    OR bundle.capture_epoch<>admission.capture_epoch
    OR bundle.lifecycle_epoch<>admission.lifecycle_epoch OR bundle.restore_marker<>admission.restore_marker THEN
  RAISE EXCEPTION 'sync writer not admitted' USING ERRCODE='P0001';
 END IF;
 SELECT last_finalized INTO finalized FROM sync_finalized_counter
  WHERE account_id=admission.account_id AND capture_epoch=admission.capture_epoch;
 IF NOT FOUND OR finalized<>bundle.finalized OR jsonb_array_length(admission.actual_touches)=0 THEN
  RAISE EXCEPTION 'sync writer not admitted' USING ERRCODE='P0001';
 END IF;
 IF bundle.semantic_effects IS DISTINCT FROM (SELECT coalesce(jsonb_agg(semantic_effect ORDER BY touch_id),'[]'::jsonb)
       FROM sync_tx_touches WHERE account_id=NEW.account_id AND tx_id=NEW.tx_id)
   OR bundle.dependencies IS DISTINCT FROM (SELECT coalesce(jsonb_agg(dependencies ORDER BY touch_id),'[]'::jsonb)
       FROM sync_tx_touches WHERE account_id=NEW.account_id AND tx_id=NEW.tx_id) THEN
  RAISE EXCEPTION 'sync writer not admitted' USING ERRCODE='P0001';
 END IF;
 IF EXISTS (
  SELECT 1 FROM jsonb_array_elements(admission.actual_touches) event
   FULL JOIN (SELECT * FROM sync_tx_touches WHERE account_id=NEW.account_id AND tx_id=NEW.tx_id) touch
    ON touch.touch_id=(event->>'event_id')::bigint AND touch.table_name=event->>'table_name'
   WHERE event IS NULL OR touch.touch_id IS NULL
 ) OR EXISTS (
  SELECT 1 FROM sync_tx_touches touch
   LEFT JOIN sync_operations operation ON operation.account_id=touch.account_id AND operation.operation_id=touch.operation_id
   LEFT JOIN sync_bundle_operations member ON member.account_id=touch.account_id
    AND member.bundle_id=bundle.bundle_id AND member.member_order=touch.touch_id
    AND member.operation_id=touch.operation_id AND member.atomic_group=touch.atomic_group
   WHERE touch.account_id=NEW.account_id AND touch.tx_id=NEW.tx_id
    AND (operation.operation_id IS NULL OR member.operation_id IS NULL
      OR operation.operation_kind<>'preferences.put' OR touch.table_name<>'agent_dashboard_preferences')
 ) OR EXISTS (
  SELECT 1 FROM sync_bundle_operations member LEFT JOIN sync_tx_touches touch
    ON touch.account_id=member.account_id AND touch.tx_id=NEW.tx_id AND touch.touch_id=member.member_order
     AND touch.operation_id=member.operation_id AND touch.atomic_group=member.atomic_group
   WHERE member.account_id=NEW.account_id AND member.bundle_id=bundle.bundle_id AND touch.touch_id IS NULL
 ) THEN
  RAISE EXCEPTION 'sync writer not admitted' USING ERRCODE='P0001';
 END IF;
 RETURN NULL;
END;
$$;
-- +goose StatementEnd

CREATE CONSTRAINT TRIGGER sync_tx_coverage AFTER INSERT OR UPDATE ON sync_tx_admissions
 DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION witself_sync_check_coverage();

-- Relation-lock drain: the complete reviewed inventory in fixed order.
LOCK TABLE account_evacuation_finalizations,
 account_events,
 account_provision_receipts,
 accounts,
 agent_activity,
 agent_avatar_activations,
 agent_avatar_profiles,
 agent_avatar_rejections,
 agent_avatar_resets,
 agent_avatar_versions,
 agent_dashboard_preferences,
 agent_email_account_rate_buckets,
 agent_email_address_domains,
 agent_email_addresses,
 agent_email_custom_domain_routes,
 agent_email_deliveries,
 agent_email_mailboxes,
 agent_email_messages,
 agent_email_outbound_messages,
 agent_email_outbound_provider_events,
 agent_email_outbound_rate_buckets,
 agent_email_outbound_recipient_suppressions,
 agent_email_rate_buckets,
 agent_email_realm_aliases,
 agent_email_realm_receive_controls,
 agent_email_realm_send_controls,
 agent_email_retention_account_scan_state,
 agent_email_retry_canary_arms,
 agent_email_send_controls,
 agent_message_deliveries,
 agent_message_rate_buckets,
 agent_message_request_candidates,
 agent_message_request_claims,
 agent_message_request_selections,
 agent_message_requests,
 agent_messages,
 agent_vault_key_enrollments,
 agent_vault_key_rotation_items,
 agent_vault_key_rotations,
 agent_vault_keys,
 agents,
 avatar_mutation_receipts,
 avatar_style_pack_versions,
 avatar_style_packs,
 avatar_style_rollout_jobs,
 fact_assertions,
 fact_candidates,
 fact_mutation_tombstones,
 fact_subjects,
 facts,
 memories,
 memory_change_clocks,
 memory_curation_actions,
 memory_curation_cursors,
 memory_curation_lanes,
 memory_curation_mutations,
 memory_curation_requests,
 memory_curation_run_inputs,
 memory_curation_runs,
 memory_deleted_references,
 memory_evidence,
 memory_relations,
 memory_vector_profiles,
 memory_vectors,
 memory_versions,
 message_retention_account_scan_state,
 message_retention_thread_activity,
 operators,
 realm_avatar_styles,
 realms,
 secret_deks,
 secret_fields,
 secret_mutation_receipts,
 secrets,
 support_ticket_messages,
 support_tickets,
 tokens,
 transcript_conversations,
 transcript_entries,
 transcript_retention_account_scan_state,
 transcript_storage_usage,
 usage_events,
 usage_rollups,
 vault_key_enrollment_receipts,
 vault_key_rotation_receipts IN ACCESS EXCLUSIVE MODE;

CREATE TRIGGER sync_writer_guard BEFORE INSERT OR UPDATE OR DELETE ON account_evacuation_finalizations
 FOR EACH ROW EXECUTE FUNCTION witself_sync_writer_guard();
CREATE TRIGGER sync_writer_touch AFTER INSERT OR UPDATE OR DELETE ON account_evacuation_finalizations
 FOR EACH ROW EXECUTE FUNCTION witself_sync_writer_guard();
CREATE TRIGGER sync_writer_truncate BEFORE TRUNCATE ON account_evacuation_finalizations
 FOR EACH STATEMENT EXECUTE FUNCTION witself_sync_writer_guard();
CREATE TRIGGER sync_writer_guard BEFORE INSERT OR UPDATE OR DELETE ON account_events
 FOR EACH ROW EXECUTE FUNCTION witself_sync_writer_guard();
CREATE TRIGGER sync_writer_touch AFTER INSERT OR UPDATE OR DELETE ON account_events
 FOR EACH ROW EXECUTE FUNCTION witself_sync_writer_guard();
CREATE TRIGGER sync_writer_truncate BEFORE TRUNCATE ON account_events
 FOR EACH STATEMENT EXECUTE FUNCTION witself_sync_writer_guard();
CREATE TRIGGER sync_writer_guard BEFORE INSERT OR UPDATE OR DELETE ON account_provision_receipts
 FOR EACH ROW EXECUTE FUNCTION witself_sync_writer_guard();
CREATE TRIGGER sync_writer_touch AFTER INSERT OR UPDATE OR DELETE ON account_provision_receipts
 FOR EACH ROW EXECUTE FUNCTION witself_sync_writer_guard();
CREATE TRIGGER sync_writer_truncate BEFORE TRUNCATE ON account_provision_receipts
 FOR EACH STATEMENT EXECUTE FUNCTION witself_sync_writer_guard();
CREATE TRIGGER sync_writer_guard BEFORE INSERT OR UPDATE OR DELETE ON accounts
 FOR EACH ROW EXECUTE FUNCTION witself_sync_writer_guard();
CREATE TRIGGER sync_writer_touch AFTER INSERT OR UPDATE OR DELETE ON accounts
 FOR EACH ROW EXECUTE FUNCTION witself_sync_writer_guard();
CREATE TRIGGER sync_writer_truncate BEFORE TRUNCATE ON accounts
 FOR EACH STATEMENT EXECUTE FUNCTION witself_sync_writer_guard();
CREATE TRIGGER sync_writer_guard BEFORE INSERT OR UPDATE OR DELETE ON agent_activity
 FOR EACH ROW EXECUTE FUNCTION witself_sync_writer_guard();
CREATE TRIGGER sync_writer_touch AFTER INSERT OR UPDATE OR DELETE ON agent_activity
 FOR EACH ROW EXECUTE FUNCTION witself_sync_writer_guard();
CREATE TRIGGER sync_writer_truncate BEFORE TRUNCATE ON agent_activity
 FOR EACH STATEMENT EXECUTE FUNCTION witself_sync_writer_guard();
CREATE TRIGGER sync_writer_guard BEFORE INSERT OR UPDATE OR DELETE ON agent_avatar_activations
 FOR EACH ROW EXECUTE FUNCTION witself_sync_writer_guard();
CREATE TRIGGER sync_writer_touch AFTER INSERT OR UPDATE OR DELETE ON agent_avatar_activations
 FOR EACH ROW EXECUTE FUNCTION witself_sync_writer_guard();
CREATE TRIGGER sync_writer_truncate BEFORE TRUNCATE ON agent_avatar_activations
 FOR EACH STATEMENT EXECUTE FUNCTION witself_sync_writer_guard();
CREATE TRIGGER sync_writer_guard BEFORE INSERT OR UPDATE OR DELETE ON agent_avatar_profiles
 FOR EACH ROW EXECUTE FUNCTION witself_sync_writer_guard();
CREATE TRIGGER sync_writer_touch AFTER INSERT OR UPDATE OR DELETE ON agent_avatar_profiles
 FOR EACH ROW EXECUTE FUNCTION witself_sync_writer_guard();
CREATE TRIGGER sync_writer_truncate BEFORE TRUNCATE ON agent_avatar_profiles
 FOR EACH STATEMENT EXECUTE FUNCTION witself_sync_writer_guard();
CREATE TRIGGER sync_writer_guard BEFORE INSERT OR UPDATE OR DELETE ON agent_avatar_rejections
 FOR EACH ROW EXECUTE FUNCTION witself_sync_writer_guard();
CREATE TRIGGER sync_writer_touch AFTER INSERT OR UPDATE OR DELETE ON agent_avatar_rejections
 FOR EACH ROW EXECUTE FUNCTION witself_sync_writer_guard();
CREATE TRIGGER sync_writer_truncate BEFORE TRUNCATE ON agent_avatar_rejections
 FOR EACH STATEMENT EXECUTE FUNCTION witself_sync_writer_guard();
CREATE TRIGGER sync_writer_guard BEFORE INSERT OR UPDATE OR DELETE ON agent_avatar_resets
 FOR EACH ROW EXECUTE FUNCTION witself_sync_writer_guard();
CREATE TRIGGER sync_writer_touch AFTER INSERT OR UPDATE OR DELETE ON agent_avatar_resets
 FOR EACH ROW EXECUTE FUNCTION witself_sync_writer_guard();
CREATE TRIGGER sync_writer_truncate BEFORE TRUNCATE ON agent_avatar_resets
 FOR EACH STATEMENT EXECUTE FUNCTION witself_sync_writer_guard();
CREATE TRIGGER sync_writer_guard BEFORE INSERT OR UPDATE OR DELETE ON agent_avatar_versions
 FOR EACH ROW EXECUTE FUNCTION witself_sync_writer_guard();
CREATE TRIGGER sync_writer_touch AFTER INSERT OR UPDATE OR DELETE ON agent_avatar_versions
 FOR EACH ROW EXECUTE FUNCTION witself_sync_writer_guard();
CREATE TRIGGER sync_writer_truncate BEFORE TRUNCATE ON agent_avatar_versions
 FOR EACH STATEMENT EXECUTE FUNCTION witself_sync_writer_guard();
CREATE TRIGGER sync_writer_guard BEFORE INSERT OR UPDATE OR DELETE ON agent_dashboard_preferences
 FOR EACH ROW EXECUTE FUNCTION witself_sync_writer_guard();
CREATE TRIGGER sync_writer_touch AFTER INSERT OR UPDATE OR DELETE ON agent_dashboard_preferences
 FOR EACH ROW EXECUTE FUNCTION witself_sync_writer_guard();
CREATE TRIGGER sync_writer_truncate BEFORE TRUNCATE ON agent_dashboard_preferences
 FOR EACH STATEMENT EXECUTE FUNCTION witself_sync_writer_guard();
CREATE TRIGGER sync_writer_guard BEFORE INSERT OR UPDATE OR DELETE ON agent_email_account_rate_buckets
 FOR EACH ROW EXECUTE FUNCTION witself_sync_writer_guard();
CREATE TRIGGER sync_writer_touch AFTER INSERT OR UPDATE OR DELETE ON agent_email_account_rate_buckets
 FOR EACH ROW EXECUTE FUNCTION witself_sync_writer_guard();
CREATE TRIGGER sync_writer_truncate BEFORE TRUNCATE ON agent_email_account_rate_buckets
 FOR EACH STATEMENT EXECUTE FUNCTION witself_sync_writer_guard();
CREATE TRIGGER sync_writer_guard BEFORE INSERT OR UPDATE OR DELETE ON agent_email_address_domains
 FOR EACH ROW EXECUTE FUNCTION witself_sync_writer_guard();
CREATE TRIGGER sync_writer_touch AFTER INSERT OR UPDATE OR DELETE ON agent_email_address_domains
 FOR EACH ROW EXECUTE FUNCTION witself_sync_writer_guard();
CREATE TRIGGER sync_writer_truncate BEFORE TRUNCATE ON agent_email_address_domains
 FOR EACH STATEMENT EXECUTE FUNCTION witself_sync_writer_guard();
CREATE TRIGGER sync_writer_guard BEFORE INSERT OR UPDATE OR DELETE ON agent_email_addresses
 FOR EACH ROW EXECUTE FUNCTION witself_sync_writer_guard();
CREATE TRIGGER sync_writer_touch AFTER INSERT OR UPDATE OR DELETE ON agent_email_addresses
 FOR EACH ROW EXECUTE FUNCTION witself_sync_writer_guard();
CREATE TRIGGER sync_writer_truncate BEFORE TRUNCATE ON agent_email_addresses
 FOR EACH STATEMENT EXECUTE FUNCTION witself_sync_writer_guard();
CREATE TRIGGER sync_writer_guard BEFORE INSERT OR UPDATE OR DELETE ON agent_email_custom_domain_routes
 FOR EACH ROW EXECUTE FUNCTION witself_sync_writer_guard();
CREATE TRIGGER sync_writer_touch AFTER INSERT OR UPDATE OR DELETE ON agent_email_custom_domain_routes
 FOR EACH ROW EXECUTE FUNCTION witself_sync_writer_guard();
CREATE TRIGGER sync_writer_truncate BEFORE TRUNCATE ON agent_email_custom_domain_routes
 FOR EACH STATEMENT EXECUTE FUNCTION witself_sync_writer_guard();
CREATE TRIGGER sync_writer_guard BEFORE INSERT OR UPDATE OR DELETE ON agent_email_deliveries
 FOR EACH ROW EXECUTE FUNCTION witself_sync_writer_guard();
CREATE TRIGGER sync_writer_touch AFTER INSERT OR UPDATE OR DELETE ON agent_email_deliveries
 FOR EACH ROW EXECUTE FUNCTION witself_sync_writer_guard();
CREATE TRIGGER sync_writer_truncate BEFORE TRUNCATE ON agent_email_deliveries
 FOR EACH STATEMENT EXECUTE FUNCTION witself_sync_writer_guard();
CREATE TRIGGER sync_writer_guard BEFORE INSERT OR UPDATE OR DELETE ON agent_email_mailboxes
 FOR EACH ROW EXECUTE FUNCTION witself_sync_writer_guard();
CREATE TRIGGER sync_writer_touch AFTER INSERT OR UPDATE OR DELETE ON agent_email_mailboxes
 FOR EACH ROW EXECUTE FUNCTION witself_sync_writer_guard();
CREATE TRIGGER sync_writer_truncate BEFORE TRUNCATE ON agent_email_mailboxes
 FOR EACH STATEMENT EXECUTE FUNCTION witself_sync_writer_guard();
CREATE TRIGGER sync_writer_guard BEFORE INSERT OR UPDATE OR DELETE ON agent_email_messages
 FOR EACH ROW EXECUTE FUNCTION witself_sync_writer_guard();
CREATE TRIGGER sync_writer_touch AFTER INSERT OR UPDATE OR DELETE ON agent_email_messages
 FOR EACH ROW EXECUTE FUNCTION witself_sync_writer_guard();
CREATE TRIGGER sync_writer_truncate BEFORE TRUNCATE ON agent_email_messages
 FOR EACH STATEMENT EXECUTE FUNCTION witself_sync_writer_guard();
CREATE TRIGGER sync_writer_guard BEFORE INSERT OR UPDATE OR DELETE ON agent_email_outbound_messages
 FOR EACH ROW EXECUTE FUNCTION witself_sync_writer_guard();
CREATE TRIGGER sync_writer_touch AFTER INSERT OR UPDATE OR DELETE ON agent_email_outbound_messages
 FOR EACH ROW EXECUTE FUNCTION witself_sync_writer_guard();
CREATE TRIGGER sync_writer_truncate BEFORE TRUNCATE ON agent_email_outbound_messages
 FOR EACH STATEMENT EXECUTE FUNCTION witself_sync_writer_guard();
CREATE TRIGGER sync_writer_guard BEFORE INSERT OR UPDATE OR DELETE ON agent_email_outbound_provider_events
 FOR EACH ROW EXECUTE FUNCTION witself_sync_writer_guard();
CREATE TRIGGER sync_writer_touch AFTER INSERT OR UPDATE OR DELETE ON agent_email_outbound_provider_events
 FOR EACH ROW EXECUTE FUNCTION witself_sync_writer_guard();
CREATE TRIGGER sync_writer_truncate BEFORE TRUNCATE ON agent_email_outbound_provider_events
 FOR EACH STATEMENT EXECUTE FUNCTION witself_sync_writer_guard();
CREATE TRIGGER sync_writer_guard BEFORE INSERT OR UPDATE OR DELETE ON agent_email_outbound_rate_buckets
 FOR EACH ROW EXECUTE FUNCTION witself_sync_writer_guard();
CREATE TRIGGER sync_writer_touch AFTER INSERT OR UPDATE OR DELETE ON agent_email_outbound_rate_buckets
 FOR EACH ROW EXECUTE FUNCTION witself_sync_writer_guard();
CREATE TRIGGER sync_writer_truncate BEFORE TRUNCATE ON agent_email_outbound_rate_buckets
 FOR EACH STATEMENT EXECUTE FUNCTION witself_sync_writer_guard();
CREATE TRIGGER sync_writer_guard BEFORE INSERT OR UPDATE OR DELETE ON agent_email_outbound_recipient_suppressions
 FOR EACH ROW EXECUTE FUNCTION witself_sync_writer_guard();
CREATE TRIGGER sync_writer_touch AFTER INSERT OR UPDATE OR DELETE ON agent_email_outbound_recipient_suppressions
 FOR EACH ROW EXECUTE FUNCTION witself_sync_writer_guard();
CREATE TRIGGER sync_writer_truncate BEFORE TRUNCATE ON agent_email_outbound_recipient_suppressions
 FOR EACH STATEMENT EXECUTE FUNCTION witself_sync_writer_guard();
CREATE TRIGGER sync_writer_guard BEFORE INSERT OR UPDATE OR DELETE ON agent_email_rate_buckets
 FOR EACH ROW EXECUTE FUNCTION witself_sync_writer_guard();
CREATE TRIGGER sync_writer_touch AFTER INSERT OR UPDATE OR DELETE ON agent_email_rate_buckets
 FOR EACH ROW EXECUTE FUNCTION witself_sync_writer_guard();
CREATE TRIGGER sync_writer_truncate BEFORE TRUNCATE ON agent_email_rate_buckets
 FOR EACH STATEMENT EXECUTE FUNCTION witself_sync_writer_guard();
CREATE TRIGGER sync_writer_guard BEFORE INSERT OR UPDATE OR DELETE ON agent_email_realm_aliases
 FOR EACH ROW EXECUTE FUNCTION witself_sync_writer_guard();
CREATE TRIGGER sync_writer_touch AFTER INSERT OR UPDATE OR DELETE ON agent_email_realm_aliases
 FOR EACH ROW EXECUTE FUNCTION witself_sync_writer_guard();
CREATE TRIGGER sync_writer_truncate BEFORE TRUNCATE ON agent_email_realm_aliases
 FOR EACH STATEMENT EXECUTE FUNCTION witself_sync_writer_guard();
CREATE TRIGGER sync_writer_guard BEFORE INSERT OR UPDATE OR DELETE ON agent_email_realm_receive_controls
 FOR EACH ROW EXECUTE FUNCTION witself_sync_writer_guard();
CREATE TRIGGER sync_writer_touch AFTER INSERT OR UPDATE OR DELETE ON agent_email_realm_receive_controls
 FOR EACH ROW EXECUTE FUNCTION witself_sync_writer_guard();
CREATE TRIGGER sync_writer_truncate BEFORE TRUNCATE ON agent_email_realm_receive_controls
 FOR EACH STATEMENT EXECUTE FUNCTION witself_sync_writer_guard();
CREATE TRIGGER sync_writer_guard BEFORE INSERT OR UPDATE OR DELETE ON agent_email_realm_send_controls
 FOR EACH ROW EXECUTE FUNCTION witself_sync_writer_guard();
CREATE TRIGGER sync_writer_touch AFTER INSERT OR UPDATE OR DELETE ON agent_email_realm_send_controls
 FOR EACH ROW EXECUTE FUNCTION witself_sync_writer_guard();
CREATE TRIGGER sync_writer_truncate BEFORE TRUNCATE ON agent_email_realm_send_controls
 FOR EACH STATEMENT EXECUTE FUNCTION witself_sync_writer_guard();
CREATE TRIGGER sync_writer_guard BEFORE INSERT OR UPDATE OR DELETE ON agent_email_retention_account_scan_state
 FOR EACH ROW EXECUTE FUNCTION witself_sync_writer_guard();
CREATE TRIGGER sync_writer_touch AFTER INSERT OR UPDATE OR DELETE ON agent_email_retention_account_scan_state
 FOR EACH ROW EXECUTE FUNCTION witself_sync_writer_guard();
CREATE TRIGGER sync_writer_truncate BEFORE TRUNCATE ON agent_email_retention_account_scan_state
 FOR EACH STATEMENT EXECUTE FUNCTION witself_sync_writer_guard();
CREATE TRIGGER sync_writer_guard BEFORE INSERT OR UPDATE OR DELETE ON agent_email_retry_canary_arms
 FOR EACH ROW EXECUTE FUNCTION witself_sync_writer_guard();
CREATE TRIGGER sync_writer_touch AFTER INSERT OR UPDATE OR DELETE ON agent_email_retry_canary_arms
 FOR EACH ROW EXECUTE FUNCTION witself_sync_writer_guard();
CREATE TRIGGER sync_writer_truncate BEFORE TRUNCATE ON agent_email_retry_canary_arms
 FOR EACH STATEMENT EXECUTE FUNCTION witself_sync_writer_guard();
CREATE TRIGGER sync_writer_guard BEFORE INSERT OR UPDATE OR DELETE ON agent_email_send_controls
 FOR EACH ROW EXECUTE FUNCTION witself_sync_writer_guard();
CREATE TRIGGER sync_writer_touch AFTER INSERT OR UPDATE OR DELETE ON agent_email_send_controls
 FOR EACH ROW EXECUTE FUNCTION witself_sync_writer_guard();
CREATE TRIGGER sync_writer_truncate BEFORE TRUNCATE ON agent_email_send_controls
 FOR EACH STATEMENT EXECUTE FUNCTION witself_sync_writer_guard();
CREATE TRIGGER sync_writer_guard BEFORE INSERT OR UPDATE OR DELETE ON agent_message_deliveries
 FOR EACH ROW EXECUTE FUNCTION witself_sync_writer_guard();
CREATE TRIGGER sync_writer_touch AFTER INSERT OR UPDATE OR DELETE ON agent_message_deliveries
 FOR EACH ROW EXECUTE FUNCTION witself_sync_writer_guard();
CREATE TRIGGER sync_writer_truncate BEFORE TRUNCATE ON agent_message_deliveries
 FOR EACH STATEMENT EXECUTE FUNCTION witself_sync_writer_guard();
CREATE TRIGGER sync_writer_guard BEFORE INSERT OR UPDATE OR DELETE ON agent_message_rate_buckets
 FOR EACH ROW EXECUTE FUNCTION witself_sync_writer_guard();
CREATE TRIGGER sync_writer_touch AFTER INSERT OR UPDATE OR DELETE ON agent_message_rate_buckets
 FOR EACH ROW EXECUTE FUNCTION witself_sync_writer_guard();
CREATE TRIGGER sync_writer_truncate BEFORE TRUNCATE ON agent_message_rate_buckets
 FOR EACH STATEMENT EXECUTE FUNCTION witself_sync_writer_guard();
CREATE TRIGGER sync_writer_guard BEFORE INSERT OR UPDATE OR DELETE ON agent_message_request_candidates
 FOR EACH ROW EXECUTE FUNCTION witself_sync_writer_guard();
CREATE TRIGGER sync_writer_touch AFTER INSERT OR UPDATE OR DELETE ON agent_message_request_candidates
 FOR EACH ROW EXECUTE FUNCTION witself_sync_writer_guard();
CREATE TRIGGER sync_writer_truncate BEFORE TRUNCATE ON agent_message_request_candidates
 FOR EACH STATEMENT EXECUTE FUNCTION witself_sync_writer_guard();
CREATE TRIGGER sync_writer_guard BEFORE INSERT OR UPDATE OR DELETE ON agent_message_request_claims
 FOR EACH ROW EXECUTE FUNCTION witself_sync_writer_guard();
CREATE TRIGGER sync_writer_touch AFTER INSERT OR UPDATE OR DELETE ON agent_message_request_claims
 FOR EACH ROW EXECUTE FUNCTION witself_sync_writer_guard();
CREATE TRIGGER sync_writer_truncate BEFORE TRUNCATE ON agent_message_request_claims
 FOR EACH STATEMENT EXECUTE FUNCTION witself_sync_writer_guard();
CREATE TRIGGER sync_writer_guard BEFORE INSERT OR UPDATE OR DELETE ON agent_message_request_selections
 FOR EACH ROW EXECUTE FUNCTION witself_sync_writer_guard();
CREATE TRIGGER sync_writer_touch AFTER INSERT OR UPDATE OR DELETE ON agent_message_request_selections
 FOR EACH ROW EXECUTE FUNCTION witself_sync_writer_guard();
CREATE TRIGGER sync_writer_truncate BEFORE TRUNCATE ON agent_message_request_selections
 FOR EACH STATEMENT EXECUTE FUNCTION witself_sync_writer_guard();
CREATE TRIGGER sync_writer_guard BEFORE INSERT OR UPDATE OR DELETE ON agent_message_requests
 FOR EACH ROW EXECUTE FUNCTION witself_sync_writer_guard();
CREATE TRIGGER sync_writer_touch AFTER INSERT OR UPDATE OR DELETE ON agent_message_requests
 FOR EACH ROW EXECUTE FUNCTION witself_sync_writer_guard();
CREATE TRIGGER sync_writer_truncate BEFORE TRUNCATE ON agent_message_requests
 FOR EACH STATEMENT EXECUTE FUNCTION witself_sync_writer_guard();
CREATE TRIGGER sync_writer_guard BEFORE INSERT OR UPDATE OR DELETE ON agent_messages
 FOR EACH ROW EXECUTE FUNCTION witself_sync_writer_guard();
CREATE TRIGGER sync_writer_touch AFTER INSERT OR UPDATE OR DELETE ON agent_messages
 FOR EACH ROW EXECUTE FUNCTION witself_sync_writer_guard();
CREATE TRIGGER sync_writer_truncate BEFORE TRUNCATE ON agent_messages
 FOR EACH STATEMENT EXECUTE FUNCTION witself_sync_writer_guard();
CREATE TRIGGER sync_writer_guard BEFORE INSERT OR UPDATE OR DELETE ON agent_vault_key_enrollments
 FOR EACH ROW EXECUTE FUNCTION witself_sync_writer_guard();
CREATE TRIGGER sync_writer_touch AFTER INSERT OR UPDATE OR DELETE ON agent_vault_key_enrollments
 FOR EACH ROW EXECUTE FUNCTION witself_sync_writer_guard();
CREATE TRIGGER sync_writer_truncate BEFORE TRUNCATE ON agent_vault_key_enrollments
 FOR EACH STATEMENT EXECUTE FUNCTION witself_sync_writer_guard();
CREATE TRIGGER sync_writer_guard BEFORE INSERT OR UPDATE OR DELETE ON agent_vault_key_rotation_items
 FOR EACH ROW EXECUTE FUNCTION witself_sync_writer_guard();
CREATE TRIGGER sync_writer_touch AFTER INSERT OR UPDATE OR DELETE ON agent_vault_key_rotation_items
 FOR EACH ROW EXECUTE FUNCTION witself_sync_writer_guard();
CREATE TRIGGER sync_writer_truncate BEFORE TRUNCATE ON agent_vault_key_rotation_items
 FOR EACH STATEMENT EXECUTE FUNCTION witself_sync_writer_guard();
CREATE TRIGGER sync_writer_guard BEFORE INSERT OR UPDATE OR DELETE ON agent_vault_key_rotations
 FOR EACH ROW EXECUTE FUNCTION witself_sync_writer_guard();
CREATE TRIGGER sync_writer_touch AFTER INSERT OR UPDATE OR DELETE ON agent_vault_key_rotations
 FOR EACH ROW EXECUTE FUNCTION witself_sync_writer_guard();
CREATE TRIGGER sync_writer_truncate BEFORE TRUNCATE ON agent_vault_key_rotations
 FOR EACH STATEMENT EXECUTE FUNCTION witself_sync_writer_guard();
CREATE TRIGGER sync_writer_guard BEFORE INSERT OR UPDATE OR DELETE ON agent_vault_keys
 FOR EACH ROW EXECUTE FUNCTION witself_sync_writer_guard();
CREATE TRIGGER sync_writer_touch AFTER INSERT OR UPDATE OR DELETE ON agent_vault_keys
 FOR EACH ROW EXECUTE FUNCTION witself_sync_writer_guard();
CREATE TRIGGER sync_writer_truncate BEFORE TRUNCATE ON agent_vault_keys
 FOR EACH STATEMENT EXECUTE FUNCTION witself_sync_writer_guard();
CREATE TRIGGER sync_writer_guard BEFORE INSERT OR UPDATE OR DELETE ON agents
 FOR EACH ROW EXECUTE FUNCTION witself_sync_writer_guard();
CREATE TRIGGER sync_writer_touch AFTER INSERT OR UPDATE OR DELETE ON agents
 FOR EACH ROW EXECUTE FUNCTION witself_sync_writer_guard();
CREATE TRIGGER sync_writer_truncate BEFORE TRUNCATE ON agents
 FOR EACH STATEMENT EXECUTE FUNCTION witself_sync_writer_guard();
CREATE TRIGGER sync_writer_guard BEFORE INSERT OR UPDATE OR DELETE ON avatar_mutation_receipts
 FOR EACH ROW EXECUTE FUNCTION witself_sync_writer_guard();
CREATE TRIGGER sync_writer_touch AFTER INSERT OR UPDATE OR DELETE ON avatar_mutation_receipts
 FOR EACH ROW EXECUTE FUNCTION witself_sync_writer_guard();
CREATE TRIGGER sync_writer_truncate BEFORE TRUNCATE ON avatar_mutation_receipts
 FOR EACH STATEMENT EXECUTE FUNCTION witself_sync_writer_guard();
CREATE TRIGGER sync_writer_guard BEFORE INSERT OR UPDATE OR DELETE ON avatar_style_pack_versions
 FOR EACH ROW EXECUTE FUNCTION witself_sync_writer_guard();
CREATE TRIGGER sync_writer_touch AFTER INSERT OR UPDATE OR DELETE ON avatar_style_pack_versions
 FOR EACH ROW EXECUTE FUNCTION witself_sync_writer_guard();
CREATE TRIGGER sync_writer_truncate BEFORE TRUNCATE ON avatar_style_pack_versions
 FOR EACH STATEMENT EXECUTE FUNCTION witself_sync_writer_guard();
CREATE TRIGGER sync_writer_guard BEFORE INSERT OR UPDATE OR DELETE ON avatar_style_packs
 FOR EACH ROW EXECUTE FUNCTION witself_sync_writer_guard();
CREATE TRIGGER sync_writer_touch AFTER INSERT OR UPDATE OR DELETE ON avatar_style_packs
 FOR EACH ROW EXECUTE FUNCTION witself_sync_writer_guard();
CREATE TRIGGER sync_writer_truncate BEFORE TRUNCATE ON avatar_style_packs
 FOR EACH STATEMENT EXECUTE FUNCTION witself_sync_writer_guard();
CREATE TRIGGER sync_writer_guard BEFORE INSERT OR UPDATE OR DELETE ON avatar_style_rollout_jobs
 FOR EACH ROW EXECUTE FUNCTION witself_sync_writer_guard();
CREATE TRIGGER sync_writer_touch AFTER INSERT OR UPDATE OR DELETE ON avatar_style_rollout_jobs
 FOR EACH ROW EXECUTE FUNCTION witself_sync_writer_guard();
CREATE TRIGGER sync_writer_truncate BEFORE TRUNCATE ON avatar_style_rollout_jobs
 FOR EACH STATEMENT EXECUTE FUNCTION witself_sync_writer_guard();
CREATE TRIGGER sync_writer_guard BEFORE INSERT OR UPDATE OR DELETE ON fact_assertions
 FOR EACH ROW EXECUTE FUNCTION witself_sync_writer_guard();
CREATE TRIGGER sync_writer_touch AFTER INSERT OR UPDATE OR DELETE ON fact_assertions
 FOR EACH ROW EXECUTE FUNCTION witself_sync_writer_guard();
CREATE TRIGGER sync_writer_truncate BEFORE TRUNCATE ON fact_assertions
 FOR EACH STATEMENT EXECUTE FUNCTION witself_sync_writer_guard();
CREATE TRIGGER sync_writer_guard BEFORE INSERT OR UPDATE OR DELETE ON fact_candidates
 FOR EACH ROW EXECUTE FUNCTION witself_sync_writer_guard();
CREATE TRIGGER sync_writer_touch AFTER INSERT OR UPDATE OR DELETE ON fact_candidates
 FOR EACH ROW EXECUTE FUNCTION witself_sync_writer_guard();
CREATE TRIGGER sync_writer_truncate BEFORE TRUNCATE ON fact_candidates
 FOR EACH STATEMENT EXECUTE FUNCTION witself_sync_writer_guard();
CREATE TRIGGER sync_writer_guard BEFORE INSERT OR UPDATE OR DELETE ON fact_mutation_tombstones
 FOR EACH ROW EXECUTE FUNCTION witself_sync_writer_guard();
CREATE TRIGGER sync_writer_touch AFTER INSERT OR UPDATE OR DELETE ON fact_mutation_tombstones
 FOR EACH ROW EXECUTE FUNCTION witself_sync_writer_guard();
CREATE TRIGGER sync_writer_truncate BEFORE TRUNCATE ON fact_mutation_tombstones
 FOR EACH STATEMENT EXECUTE FUNCTION witself_sync_writer_guard();
CREATE TRIGGER sync_writer_guard BEFORE INSERT OR UPDATE OR DELETE ON fact_subjects
 FOR EACH ROW EXECUTE FUNCTION witself_sync_writer_guard();
CREATE TRIGGER sync_writer_touch AFTER INSERT OR UPDATE OR DELETE ON fact_subjects
 FOR EACH ROW EXECUTE FUNCTION witself_sync_writer_guard();
CREATE TRIGGER sync_writer_truncate BEFORE TRUNCATE ON fact_subjects
 FOR EACH STATEMENT EXECUTE FUNCTION witself_sync_writer_guard();
CREATE TRIGGER sync_writer_guard BEFORE INSERT OR UPDATE OR DELETE ON facts
 FOR EACH ROW EXECUTE FUNCTION witself_sync_writer_guard();
CREATE TRIGGER sync_writer_touch AFTER INSERT OR UPDATE OR DELETE ON facts
 FOR EACH ROW EXECUTE FUNCTION witself_sync_writer_guard();
CREATE TRIGGER sync_writer_truncate BEFORE TRUNCATE ON facts
 FOR EACH STATEMENT EXECUTE FUNCTION witself_sync_writer_guard();
CREATE TRIGGER sync_writer_guard BEFORE INSERT OR UPDATE OR DELETE ON memories
 FOR EACH ROW EXECUTE FUNCTION witself_sync_writer_guard();
CREATE TRIGGER sync_writer_touch AFTER INSERT OR UPDATE OR DELETE ON memories
 FOR EACH ROW EXECUTE FUNCTION witself_sync_writer_guard();
CREATE TRIGGER sync_writer_truncate BEFORE TRUNCATE ON memories
 FOR EACH STATEMENT EXECUTE FUNCTION witself_sync_writer_guard();
CREATE TRIGGER sync_writer_guard BEFORE INSERT OR UPDATE OR DELETE ON memory_change_clocks
 FOR EACH ROW EXECUTE FUNCTION witself_sync_writer_guard();
CREATE TRIGGER sync_writer_touch AFTER INSERT OR UPDATE OR DELETE ON memory_change_clocks
 FOR EACH ROW EXECUTE FUNCTION witself_sync_writer_guard();
CREATE TRIGGER sync_writer_truncate BEFORE TRUNCATE ON memory_change_clocks
 FOR EACH STATEMENT EXECUTE FUNCTION witself_sync_writer_guard();
CREATE TRIGGER sync_writer_guard BEFORE INSERT OR UPDATE OR DELETE ON memory_curation_actions
 FOR EACH ROW EXECUTE FUNCTION witself_sync_writer_guard();
CREATE TRIGGER sync_writer_touch AFTER INSERT OR UPDATE OR DELETE ON memory_curation_actions
 FOR EACH ROW EXECUTE FUNCTION witself_sync_writer_guard();
CREATE TRIGGER sync_writer_truncate BEFORE TRUNCATE ON memory_curation_actions
 FOR EACH STATEMENT EXECUTE FUNCTION witself_sync_writer_guard();
CREATE TRIGGER sync_writer_guard BEFORE INSERT OR UPDATE OR DELETE ON memory_curation_cursors
 FOR EACH ROW EXECUTE FUNCTION witself_sync_writer_guard();
CREATE TRIGGER sync_writer_touch AFTER INSERT OR UPDATE OR DELETE ON memory_curation_cursors
 FOR EACH ROW EXECUTE FUNCTION witself_sync_writer_guard();
CREATE TRIGGER sync_writer_truncate BEFORE TRUNCATE ON memory_curation_cursors
 FOR EACH STATEMENT EXECUTE FUNCTION witself_sync_writer_guard();
CREATE TRIGGER sync_writer_guard BEFORE INSERT OR UPDATE OR DELETE ON memory_curation_lanes
 FOR EACH ROW EXECUTE FUNCTION witself_sync_writer_guard();
CREATE TRIGGER sync_writer_touch AFTER INSERT OR UPDATE OR DELETE ON memory_curation_lanes
 FOR EACH ROW EXECUTE FUNCTION witself_sync_writer_guard();
CREATE TRIGGER sync_writer_truncate BEFORE TRUNCATE ON memory_curation_lanes
 FOR EACH STATEMENT EXECUTE FUNCTION witself_sync_writer_guard();
CREATE TRIGGER sync_writer_guard BEFORE INSERT OR UPDATE OR DELETE ON memory_curation_mutations
 FOR EACH ROW EXECUTE FUNCTION witself_sync_writer_guard();
CREATE TRIGGER sync_writer_touch AFTER INSERT OR UPDATE OR DELETE ON memory_curation_mutations
 FOR EACH ROW EXECUTE FUNCTION witself_sync_writer_guard();
CREATE TRIGGER sync_writer_truncate BEFORE TRUNCATE ON memory_curation_mutations
 FOR EACH STATEMENT EXECUTE FUNCTION witself_sync_writer_guard();
CREATE TRIGGER sync_writer_guard BEFORE INSERT OR UPDATE OR DELETE ON memory_curation_requests
 FOR EACH ROW EXECUTE FUNCTION witself_sync_writer_guard();
CREATE TRIGGER sync_writer_touch AFTER INSERT OR UPDATE OR DELETE ON memory_curation_requests
 FOR EACH ROW EXECUTE FUNCTION witself_sync_writer_guard();
CREATE TRIGGER sync_writer_truncate BEFORE TRUNCATE ON memory_curation_requests
 FOR EACH STATEMENT EXECUTE FUNCTION witself_sync_writer_guard();
CREATE TRIGGER sync_writer_guard BEFORE INSERT OR UPDATE OR DELETE ON memory_curation_run_inputs
 FOR EACH ROW EXECUTE FUNCTION witself_sync_writer_guard();
CREATE TRIGGER sync_writer_touch AFTER INSERT OR UPDATE OR DELETE ON memory_curation_run_inputs
 FOR EACH ROW EXECUTE FUNCTION witself_sync_writer_guard();
CREATE TRIGGER sync_writer_truncate BEFORE TRUNCATE ON memory_curation_run_inputs
 FOR EACH STATEMENT EXECUTE FUNCTION witself_sync_writer_guard();
CREATE TRIGGER sync_writer_guard BEFORE INSERT OR UPDATE OR DELETE ON memory_curation_runs
 FOR EACH ROW EXECUTE FUNCTION witself_sync_writer_guard();
CREATE TRIGGER sync_writer_touch AFTER INSERT OR UPDATE OR DELETE ON memory_curation_runs
 FOR EACH ROW EXECUTE FUNCTION witself_sync_writer_guard();
CREATE TRIGGER sync_writer_truncate BEFORE TRUNCATE ON memory_curation_runs
 FOR EACH STATEMENT EXECUTE FUNCTION witself_sync_writer_guard();
CREATE TRIGGER sync_writer_guard BEFORE INSERT OR UPDATE OR DELETE ON memory_deleted_references
 FOR EACH ROW EXECUTE FUNCTION witself_sync_writer_guard();
CREATE TRIGGER sync_writer_touch AFTER INSERT OR UPDATE OR DELETE ON memory_deleted_references
 FOR EACH ROW EXECUTE FUNCTION witself_sync_writer_guard();
CREATE TRIGGER sync_writer_truncate BEFORE TRUNCATE ON memory_deleted_references
 FOR EACH STATEMENT EXECUTE FUNCTION witself_sync_writer_guard();
CREATE TRIGGER sync_writer_guard BEFORE INSERT OR UPDATE OR DELETE ON memory_evidence
 FOR EACH ROW EXECUTE FUNCTION witself_sync_writer_guard();
CREATE TRIGGER sync_writer_touch AFTER INSERT OR UPDATE OR DELETE ON memory_evidence
 FOR EACH ROW EXECUTE FUNCTION witself_sync_writer_guard();
CREATE TRIGGER sync_writer_truncate BEFORE TRUNCATE ON memory_evidence
 FOR EACH STATEMENT EXECUTE FUNCTION witself_sync_writer_guard();
CREATE TRIGGER sync_writer_guard BEFORE INSERT OR UPDATE OR DELETE ON memory_relations
 FOR EACH ROW EXECUTE FUNCTION witself_sync_writer_guard();
CREATE TRIGGER sync_writer_touch AFTER INSERT OR UPDATE OR DELETE ON memory_relations
 FOR EACH ROW EXECUTE FUNCTION witself_sync_writer_guard();
CREATE TRIGGER sync_writer_truncate BEFORE TRUNCATE ON memory_relations
 FOR EACH STATEMENT EXECUTE FUNCTION witself_sync_writer_guard();
CREATE TRIGGER sync_writer_guard BEFORE INSERT OR UPDATE OR DELETE ON memory_vector_profiles
 FOR EACH ROW EXECUTE FUNCTION witself_sync_writer_guard();
CREATE TRIGGER sync_writer_touch AFTER INSERT OR UPDATE OR DELETE ON memory_vector_profiles
 FOR EACH ROW EXECUTE FUNCTION witself_sync_writer_guard();
CREATE TRIGGER sync_writer_truncate BEFORE TRUNCATE ON memory_vector_profiles
 FOR EACH STATEMENT EXECUTE FUNCTION witself_sync_writer_guard();
CREATE TRIGGER sync_writer_guard BEFORE INSERT OR UPDATE OR DELETE ON memory_vectors
 FOR EACH ROW EXECUTE FUNCTION witself_sync_writer_guard();
CREATE TRIGGER sync_writer_touch AFTER INSERT OR UPDATE OR DELETE ON memory_vectors
 FOR EACH ROW EXECUTE FUNCTION witself_sync_writer_guard();
CREATE TRIGGER sync_writer_truncate BEFORE TRUNCATE ON memory_vectors
 FOR EACH STATEMENT EXECUTE FUNCTION witself_sync_writer_guard();
CREATE TRIGGER sync_writer_guard BEFORE INSERT OR UPDATE OR DELETE ON memory_versions
 FOR EACH ROW EXECUTE FUNCTION witself_sync_writer_guard();
CREATE TRIGGER sync_writer_touch AFTER INSERT OR UPDATE OR DELETE ON memory_versions
 FOR EACH ROW EXECUTE FUNCTION witself_sync_writer_guard();
CREATE TRIGGER sync_writer_truncate BEFORE TRUNCATE ON memory_versions
 FOR EACH STATEMENT EXECUTE FUNCTION witself_sync_writer_guard();
CREATE TRIGGER sync_writer_guard BEFORE INSERT OR UPDATE OR DELETE ON message_retention_account_scan_state
 FOR EACH ROW EXECUTE FUNCTION witself_sync_writer_guard();
CREATE TRIGGER sync_writer_touch AFTER INSERT OR UPDATE OR DELETE ON message_retention_account_scan_state
 FOR EACH ROW EXECUTE FUNCTION witself_sync_writer_guard();
CREATE TRIGGER sync_writer_truncate BEFORE TRUNCATE ON message_retention_account_scan_state
 FOR EACH STATEMENT EXECUTE FUNCTION witself_sync_writer_guard();
CREATE TRIGGER sync_writer_guard BEFORE INSERT OR UPDATE OR DELETE ON message_retention_thread_activity
 FOR EACH ROW EXECUTE FUNCTION witself_sync_writer_guard();
CREATE TRIGGER sync_writer_touch AFTER INSERT OR UPDATE OR DELETE ON message_retention_thread_activity
 FOR EACH ROW EXECUTE FUNCTION witself_sync_writer_guard();
CREATE TRIGGER sync_writer_truncate BEFORE TRUNCATE ON message_retention_thread_activity
 FOR EACH STATEMENT EXECUTE FUNCTION witself_sync_writer_guard();
CREATE TRIGGER sync_writer_guard BEFORE INSERT OR UPDATE OR DELETE ON operators
 FOR EACH ROW EXECUTE FUNCTION witself_sync_writer_guard();
CREATE TRIGGER sync_writer_touch AFTER INSERT OR UPDATE OR DELETE ON operators
 FOR EACH ROW EXECUTE FUNCTION witself_sync_writer_guard();
CREATE TRIGGER sync_writer_truncate BEFORE TRUNCATE ON operators
 FOR EACH STATEMENT EXECUTE FUNCTION witself_sync_writer_guard();
CREATE TRIGGER sync_writer_guard BEFORE INSERT OR UPDATE OR DELETE ON realm_avatar_styles
 FOR EACH ROW EXECUTE FUNCTION witself_sync_writer_guard();
CREATE TRIGGER sync_writer_touch AFTER INSERT OR UPDATE OR DELETE ON realm_avatar_styles
 FOR EACH ROW EXECUTE FUNCTION witself_sync_writer_guard();
CREATE TRIGGER sync_writer_truncate BEFORE TRUNCATE ON realm_avatar_styles
 FOR EACH STATEMENT EXECUTE FUNCTION witself_sync_writer_guard();
CREATE TRIGGER sync_writer_guard BEFORE INSERT OR UPDATE OR DELETE ON realms
 FOR EACH ROW EXECUTE FUNCTION witself_sync_writer_guard();
CREATE TRIGGER sync_writer_touch AFTER INSERT OR UPDATE OR DELETE ON realms
 FOR EACH ROW EXECUTE FUNCTION witself_sync_writer_guard();
CREATE TRIGGER sync_writer_truncate BEFORE TRUNCATE ON realms
 FOR EACH STATEMENT EXECUTE FUNCTION witself_sync_writer_guard();
CREATE TRIGGER sync_writer_guard BEFORE INSERT OR UPDATE OR DELETE ON secret_deks
 FOR EACH ROW EXECUTE FUNCTION witself_sync_writer_guard();
CREATE TRIGGER sync_writer_touch AFTER INSERT OR UPDATE OR DELETE ON secret_deks
 FOR EACH ROW EXECUTE FUNCTION witself_sync_writer_guard();
CREATE TRIGGER sync_writer_truncate BEFORE TRUNCATE ON secret_deks
 FOR EACH STATEMENT EXECUTE FUNCTION witself_sync_writer_guard();
CREATE TRIGGER sync_writer_guard BEFORE INSERT OR UPDATE OR DELETE ON secret_fields
 FOR EACH ROW EXECUTE FUNCTION witself_sync_writer_guard();
CREATE TRIGGER sync_writer_touch AFTER INSERT OR UPDATE OR DELETE ON secret_fields
 FOR EACH ROW EXECUTE FUNCTION witself_sync_writer_guard();
CREATE TRIGGER sync_writer_truncate BEFORE TRUNCATE ON secret_fields
 FOR EACH STATEMENT EXECUTE FUNCTION witself_sync_writer_guard();
CREATE TRIGGER sync_writer_guard BEFORE INSERT OR UPDATE OR DELETE ON secret_mutation_receipts
 FOR EACH ROW EXECUTE FUNCTION witself_sync_writer_guard();
CREATE TRIGGER sync_writer_touch AFTER INSERT OR UPDATE OR DELETE ON secret_mutation_receipts
 FOR EACH ROW EXECUTE FUNCTION witself_sync_writer_guard();
CREATE TRIGGER sync_writer_truncate BEFORE TRUNCATE ON secret_mutation_receipts
 FOR EACH STATEMENT EXECUTE FUNCTION witself_sync_writer_guard();
CREATE TRIGGER sync_writer_guard BEFORE INSERT OR UPDATE OR DELETE ON secrets
 FOR EACH ROW EXECUTE FUNCTION witself_sync_writer_guard();
CREATE TRIGGER sync_writer_touch AFTER INSERT OR UPDATE OR DELETE ON secrets
 FOR EACH ROW EXECUTE FUNCTION witself_sync_writer_guard();
CREATE TRIGGER sync_writer_truncate BEFORE TRUNCATE ON secrets
 FOR EACH STATEMENT EXECUTE FUNCTION witself_sync_writer_guard();
CREATE TRIGGER sync_writer_guard BEFORE INSERT OR UPDATE OR DELETE ON support_ticket_messages
 FOR EACH ROW EXECUTE FUNCTION witself_sync_writer_guard();
CREATE TRIGGER sync_writer_touch AFTER INSERT OR UPDATE OR DELETE ON support_ticket_messages
 FOR EACH ROW EXECUTE FUNCTION witself_sync_writer_guard();
CREATE TRIGGER sync_writer_truncate BEFORE TRUNCATE ON support_ticket_messages
 FOR EACH STATEMENT EXECUTE FUNCTION witself_sync_writer_guard();
CREATE TRIGGER sync_writer_guard BEFORE INSERT OR UPDATE OR DELETE ON support_tickets
 FOR EACH ROW EXECUTE FUNCTION witself_sync_writer_guard();
CREATE TRIGGER sync_writer_touch AFTER INSERT OR UPDATE OR DELETE ON support_tickets
 FOR EACH ROW EXECUTE FUNCTION witself_sync_writer_guard();
CREATE TRIGGER sync_writer_truncate BEFORE TRUNCATE ON support_tickets
 FOR EACH STATEMENT EXECUTE FUNCTION witself_sync_writer_guard();
CREATE TRIGGER sync_writer_guard BEFORE INSERT OR UPDATE OR DELETE ON tokens
 FOR EACH ROW EXECUTE FUNCTION witself_sync_writer_guard();
CREATE TRIGGER sync_writer_touch AFTER INSERT OR UPDATE OR DELETE ON tokens
 FOR EACH ROW EXECUTE FUNCTION witself_sync_writer_guard();
CREATE TRIGGER sync_writer_truncate BEFORE TRUNCATE ON tokens
 FOR EACH STATEMENT EXECUTE FUNCTION witself_sync_writer_guard();
CREATE TRIGGER sync_writer_guard BEFORE INSERT OR UPDATE OR DELETE ON transcript_conversations
 FOR EACH ROW EXECUTE FUNCTION witself_sync_writer_guard();
CREATE TRIGGER sync_writer_touch AFTER INSERT OR UPDATE OR DELETE ON transcript_conversations
 FOR EACH ROW EXECUTE FUNCTION witself_sync_writer_guard();
CREATE TRIGGER sync_writer_truncate BEFORE TRUNCATE ON transcript_conversations
 FOR EACH STATEMENT EXECUTE FUNCTION witself_sync_writer_guard();
CREATE TRIGGER sync_writer_guard BEFORE INSERT OR UPDATE OR DELETE ON transcript_entries
 FOR EACH ROW EXECUTE FUNCTION witself_sync_writer_guard();
CREATE TRIGGER sync_writer_touch AFTER INSERT OR UPDATE OR DELETE ON transcript_entries
 FOR EACH ROW EXECUTE FUNCTION witself_sync_writer_guard();
CREATE TRIGGER sync_writer_truncate BEFORE TRUNCATE ON transcript_entries
 FOR EACH STATEMENT EXECUTE FUNCTION witself_sync_writer_guard();
CREATE TRIGGER sync_writer_guard BEFORE INSERT OR UPDATE OR DELETE ON transcript_retention_account_scan_state
 FOR EACH ROW EXECUTE FUNCTION witself_sync_writer_guard();
CREATE TRIGGER sync_writer_touch AFTER INSERT OR UPDATE OR DELETE ON transcript_retention_account_scan_state
 FOR EACH ROW EXECUTE FUNCTION witself_sync_writer_guard();
CREATE TRIGGER sync_writer_truncate BEFORE TRUNCATE ON transcript_retention_account_scan_state
 FOR EACH STATEMENT EXECUTE FUNCTION witself_sync_writer_guard();
CREATE TRIGGER sync_writer_guard BEFORE INSERT OR UPDATE OR DELETE ON transcript_storage_usage
 FOR EACH ROW EXECUTE FUNCTION witself_sync_writer_guard();
CREATE TRIGGER sync_writer_touch AFTER INSERT OR UPDATE OR DELETE ON transcript_storage_usage
 FOR EACH ROW EXECUTE FUNCTION witself_sync_writer_guard();
CREATE TRIGGER sync_writer_truncate BEFORE TRUNCATE ON transcript_storage_usage
 FOR EACH STATEMENT EXECUTE FUNCTION witself_sync_writer_guard();
CREATE TRIGGER sync_writer_guard BEFORE INSERT OR UPDATE OR DELETE ON usage_events
 FOR EACH ROW EXECUTE FUNCTION witself_sync_writer_guard();
CREATE TRIGGER sync_writer_touch AFTER INSERT OR UPDATE OR DELETE ON usage_events
 FOR EACH ROW EXECUTE FUNCTION witself_sync_writer_guard();
CREATE TRIGGER sync_writer_truncate BEFORE TRUNCATE ON usage_events
 FOR EACH STATEMENT EXECUTE FUNCTION witself_sync_writer_guard();
CREATE TRIGGER sync_writer_guard BEFORE INSERT OR UPDATE OR DELETE ON usage_rollups
 FOR EACH ROW EXECUTE FUNCTION witself_sync_writer_guard();
CREATE TRIGGER sync_writer_touch AFTER INSERT OR UPDATE OR DELETE ON usage_rollups
 FOR EACH ROW EXECUTE FUNCTION witself_sync_writer_guard();
CREATE TRIGGER sync_writer_truncate BEFORE TRUNCATE ON usage_rollups
 FOR EACH STATEMENT EXECUTE FUNCTION witself_sync_writer_guard();
CREATE TRIGGER sync_writer_guard BEFORE INSERT OR UPDATE OR DELETE ON vault_key_enrollment_receipts
 FOR EACH ROW EXECUTE FUNCTION witself_sync_writer_guard();
CREATE TRIGGER sync_writer_touch AFTER INSERT OR UPDATE OR DELETE ON vault_key_enrollment_receipts
 FOR EACH ROW EXECUTE FUNCTION witself_sync_writer_guard();
CREATE TRIGGER sync_writer_truncate BEFORE TRUNCATE ON vault_key_enrollment_receipts
 FOR EACH STATEMENT EXECUTE FUNCTION witself_sync_writer_guard();
CREATE TRIGGER sync_writer_guard BEFORE INSERT OR UPDATE OR DELETE ON vault_key_rotation_receipts
 FOR EACH ROW EXECUTE FUNCTION witself_sync_writer_guard();
CREATE TRIGGER sync_writer_touch AFTER INSERT OR UPDATE OR DELETE ON vault_key_rotation_receipts
 FOR EACH ROW EXECUTE FUNCTION witself_sync_writer_guard();
CREATE TRIGGER sync_writer_truncate BEFORE TRUNCATE ON vault_key_rotation_receipts
 FOR EACH STATEMENT EXECUTE FUNCTION witself_sync_writer_guard();

-- +goose Down
-- +goose StatementBegin
DO $$ BEGIN
 RAISE EXCEPTION 'sync schema downgrade is forbidden' USING ERRCODE='23514';
END $$;
-- +goose StatementEnd
