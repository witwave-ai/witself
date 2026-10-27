-- LC-S5A/B/C synthetic fixture version 1. Explicitly loaded by opted-in tests only.
-- Harness capture/receipt/projection storage: no production migration or sync API.
-- Comparison and signing secrets live only in fixture providers, never these rows.
CREATE TABLE h_fixture_objects (
    account_id text NOT NULL REFERENCES accounts(id),
    object_id text NOT NULL,
    owner_node text NOT NULL,
    value text NOT NULL,
    PRIMARY KEY (account_id, object_id)
);
CREATE TABLE h_replica_cursors (
    account_id text NOT NULL REFERENCES accounts(id),
    source_node text NOT NULL,
    ordinal bigint NOT NULL CHECK (ordinal >= 0),
    PRIMARY KEY (account_id, source_node)
);
CREATE TABLE h_replica_status (
    account_id text NOT NULL REFERENCES accounts(id),
    replica_id text NOT NULL,
    status text NOT NULL CHECK (status IN ('pending', 'accepted', 'converged')),
    PRIMARY KEY (account_id, replica_id)
);
CREATE TABLE h_provider_actions (
    account_id text NOT NULL REFERENCES accounts(id),
    operation_id text NOT NULL,
    PRIMARY KEY (account_id, operation_id)
);
CREATE TABLE h_logical_actions (
    account_id text NOT NULL REFERENCES accounts(id),
    operation_id bytea NOT NULL CHECK (octet_length(operation_id)=16),
    action_ordinal bigint NOT NULL DEFAULT 1 CHECK (action_ordinal>0),
    actor text NOT NULL DEFAULT 'fixture_actor',
    occurred_at timestamptz NOT NULL DEFAULT '2026-01-01T12:00:00Z',
    action_class text NOT NULL DEFAULT 'fixture_mutation',
    quantity bigint NOT NULL DEFAULT 1 CHECK (quantity>=0),
    PRIMARY KEY (account_id, operation_id, action_ordinal)
);
CREATE TABLE h_rollups (
    account_id text NOT NULL REFERENCES accounts(id),
    actor text NOT NULL DEFAULT 'fixture_actor',
    dimension text NOT NULL DEFAULT 'fixture_mutation',
    bucket text NOT NULL CHECK (bucket IN ('hour','day')),
    bucket_start timestamptz NOT NULL,
    count bigint NOT NULL CHECK (count >= 0),
    quantity bigint NOT NULL CHECK (quantity >= 0),
    PRIMARY KEY (account_id, actor, dimension, bucket, bucket_start)
);

-- Incarnation history is separate from the account-wide operation namespace.
CREATE TABLE h_incarnations (
    account_id text NOT NULL REFERENCES accounts(id),
    lifecycle_epoch bigint NOT NULL CHECK (lifecycle_epoch BETWEEN 0 AND 9007199254740991),
    restore_marker bytea NOT NULL CHECK (octet_length(restore_marker) = 16),
    predecessor_epoch bigint CHECK (predecessor_epoch BETWEEN 0 AND 9007199254740991),
    predecessor_marker bytea CHECK (octet_length(predecessor_marker) = 16),
    allocation_id bytea NOT NULL CHECK (octet_length(allocation_id) = 16),
    transition_id bytea NOT NULL CHECK (octet_length(transition_id) = 16),
    authority_installation text NOT NULL,
    state text NOT NULL CHECK (state IN ('allocated','installed','published','retired')),
    publication_proof_ref text,
    PRIMARY KEY (account_id,lifecycle_epoch,restore_marker),
    UNIQUE (account_id,allocation_id),
    CHECK ((predecessor_epoch IS NULL) = (predecessor_marker IS NULL))
);
CREATE TABLE h_operations (
    account_id text NOT NULL REFERENCES accounts(id),
    operation_id bytea NOT NULL CHECK (octet_length(operation_id) = 16),
    original_lifecycle_epoch bigint NOT NULL CHECK (original_lifecycle_epoch BETWEEN 0 AND 9007199254740991),
    original_restore_marker bytea NOT NULL CHECK (octet_length(original_restore_marker) = 16),
    original_capture_epoch bigint NOT NULL CHECK (original_capture_epoch > 0),
    original_installation text NOT NULL,
    original_replica text NOT NULL,
    original_write_lineage text NOT NULL,
    original_sequence bigint NOT NULL CHECK (original_sequence >= 0),
    operation_kind text NOT NULL,
    envelope_version bigint NOT NULL CHECK (envelope_version = 1),
    schema_version bigint NOT NULL CHECK (schema_version = 1),
    protocol_version bigint NOT NULL CHECK (protocol_version = 1),
    comparison_key_id text NOT NULL,
    input_commitment bytea NOT NULL CHECK (octet_length(input_commitment) = 32),
    admission_state text NOT NULL CHECK (admission_state IN ('admitted','staged','terminal')),
    receipt_ref bytea CHECK (octet_length(receipt_ref) = 16),
    PRIMARY KEY (account_id,operation_id),
    UNIQUE (account_id,operation_id,comparison_key_id),
    UNIQUE (account_id,operation_id,comparison_key_id,input_commitment),
    UNIQUE (account_id,operation_id,original_lifecycle_epoch,original_restore_marker,
        original_capture_epoch,original_installation,original_replica,original_write_lineage,original_sequence),
    CHECK ((admission_state='terminal') = (receipt_ref IS NOT NULL)),
    CHECK (receipt_ref IS NULL OR receipt_ref=operation_id),
    FOREIGN KEY (account_id,original_lifecycle_epoch,original_restore_marker)
        REFERENCES h_incarnations(account_id,lifecycle_epoch,restore_marker)
);
CREATE TABLE h_operation_payloads (
    account_id text NOT NULL,
    operation_id bytea NOT NULL CHECK (octet_length(operation_id) = 16),
    canonical_semantic_envelope bytea NOT NULL CHECK (octet_length(canonical_semantic_envelope) BETWEEN 1 AND 65536),
    governed_retirement timestamptz,
    PRIMARY KEY (account_id,operation_id),
    FOREIGN KEY (account_id,operation_id) REFERENCES h_operations(account_id,operation_id)
);
CREATE TABLE h_verifier_history (
    account_id text NOT NULL REFERENCES accounts(id),
    issuer_id text NOT NULL,
    authority_stream text NOT NULL,
    signing_key_id text NOT NULL,
    public_key bytea NOT NULL CHECK (octet_length(public_key) = 32),
    valid_from timestamptz NOT NULL,
    retired_at timestamptz,
    sealed_prefix_proof bytea,
    PRIMARY KEY (account_id,issuer_id,authority_stream,signing_key_id),
    CHECK (retired_at IS NULL OR retired_at >= valid_from)
);
CREATE TABLE h_receipts (
    account_id text NOT NULL,
    operation_id bytea NOT NULL CHECK (octet_length(operation_id) = 16),
    original_lifecycle_epoch bigint NOT NULL CHECK (original_lifecycle_epoch BETWEEN 0 AND 9007199254740991),
    original_restore_marker bytea NOT NULL CHECK (octet_length(original_restore_marker) = 16),
    original_capture_epoch bigint NOT NULL CHECK (original_capture_epoch > 0),
    original_installation text NOT NULL,
    original_replica text NOT NULL,
    original_write_lineage text NOT NULL,
    original_sequence bigint NOT NULL CHECK (original_sequence >= 0),
    issuer_id text NOT NULL,
    issuer_lifecycle_epoch bigint NOT NULL CHECK (issuer_lifecycle_epoch BETWEEN 0 AND 9007199254740991),
    issuer_restore_marker bytea NOT NULL CHECK (octet_length(issuer_restore_marker) = 16),
    authority_stream text NOT NULL,
    issuer_authority_epoch text NOT NULL,
    acceptance_ordinal bigint NOT NULL CHECK (acceptance_ordinal >= 0),
    comparison_key_id text NOT NULL,
    input_commitment bytea NOT NULL CHECK (octet_length(input_commitment) = 32),
    result_commitment bytea CHECK (octet_length(result_commitment) = 32),
    status text NOT NULL,
    reason text NOT NULL,
    causal_result bytea NOT NULL,
    signing_key_id text NOT NULL,
    canonical_body bytea NOT NULL CHECK (octet_length(canonical_body) > 0),
    signed_bytes bytea NOT NULL CHECK (octet_length(signed_bytes) = 64),
    settled_at timestamptz NOT NULL,
    retired_at timestamptz,
    retain_until timestamptz NOT NULL,
    PRIMARY KEY (account_id,operation_id),
    UNIQUE (account_id,issuer_id,authority_stream,issuer_authority_epoch,acceptance_ordinal),
    FOREIGN KEY (account_id,operation_id,comparison_key_id,input_commitment)
        REFERENCES h_operations(account_id,operation_id,comparison_key_id,input_commitment),
    FOREIGN KEY (account_id,operation_id,original_lifecycle_epoch,original_restore_marker,
        original_capture_epoch,original_installation,original_replica,original_write_lineage,original_sequence)
        REFERENCES h_operations(account_id,operation_id,original_lifecycle_epoch,original_restore_marker,
            original_capture_epoch,original_installation,original_replica,original_write_lineage,original_sequence),
    FOREIGN KEY (account_id,issuer_lifecycle_epoch,issuer_restore_marker)
        REFERENCES h_incarnations(account_id,lifecycle_epoch,restore_marker),
    FOREIGN KEY (account_id,issuer_id,authority_stream,signing_key_id)
        REFERENCES h_verifier_history(account_id,issuer_id,authority_stream,signing_key_id),
    CHECK ((status='accepted' AND reason='applied') OR
           (status='rejected' AND reason IN ('invalid_envelope','unauthorized','authorization_expired','unsupported_operation','schema_incompatible','protocol_incompatible','capacity_refused','account_closed')) OR
           (status='conflict' AND reason IN ('expected_version_mismatch','dependency_unavailable','identity_collision','deletion_conflict','authority_epoch_stale','incarnation_mismatch','operation_id_reuse'))),
    CHECK (retain_until >= settled_at + interval '37 days'),
    CHECK (retired_at IS NULL OR retain_until >= retired_at + interval '37 days')
);
CREATE TABLE h_reuse_refusals (
    account_id text NOT NULL,
    operation_id bytea NOT NULL CHECK (octet_length(operation_id) = 16),
    comparison_key_id text NOT NULL,
    conflicting_commitment bytea NOT NULL CHECK (octet_length(conflicting_commitment) = 32),
    canonical_body bytea NOT NULL CHECK (octet_length(canonical_body) > 0),
    signed_bytes bytea NOT NULL CHECK (octet_length(signed_bytes) = 64),
    retain_until timestamptz NOT NULL,
    PRIMARY KEY (account_id,operation_id,comparison_key_id,conflicting_commitment),
    FOREIGN KEY (account_id,operation_id,comparison_key_id)
        REFERENCES h_operations(account_id,operation_id,comparison_key_id)
);
ALTER TABLE h_operations ADD CONSTRAINT h_operation_receipt_ref
    FOREIGN KEY (account_id,receipt_ref) REFERENCES h_receipts(account_id,operation_id);

-- Row UPDATE/DELETE immutability is enforced inside this synthetic schema.
-- This is not a production-role or trigger-bypass proof. Retention/recovery
-- mutations are specialized by the governed C retirement path below.
CREATE FUNCTION h_immutable_row() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'immutable harness evidence' USING ERRCODE='23514';
END $$;
CREATE TRIGGER h_incarnations_immutable BEFORE UPDATE OR DELETE ON h_incarnations FOR EACH ROW EXECUTE FUNCTION h_immutable_row();
CREATE TRIGGER h_payloads_immutable BEFORE UPDATE OR DELETE ON h_operation_payloads FOR EACH ROW EXECUTE FUNCTION h_immutable_row();
CREATE TRIGGER h_receipts_immutable BEFORE UPDATE OR DELETE ON h_receipts FOR EACH ROW EXECUTE FUNCTION h_immutable_row();
CREATE TRIGGER h_refusals_immutable BEFORE UPDATE OR DELETE ON h_reuse_refusals FOR EACH ROW EXECUTE FUNCTION h_immutable_row();
CREATE TRIGGER h_verifiers_immutable BEFORE UPDATE OR DELETE ON h_verifier_history FOR EACH ROW EXECUTE FUNCTION h_immutable_row();
CREATE FUNCTION h_operation_transition() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'immutable harness admission' USING ERRCODE='23514';
    END IF;
    IF (to_jsonb(NEW) - 'admission_state' - 'receipt_ref') IS DISTINCT FROM
       (to_jsonb(OLD) - 'admission_state' - 'receipt_ref') OR
       OLD.admission_state = 'terminal' OR
       (OLD.admission_state = 'staged' AND NEW.admission_state = 'admitted') THEN
        RAISE EXCEPTION 'immutable harness admission' USING ERRCODE='23514';
    END IF;
    RETURN NEW;
END $$;
CREATE TRIGGER h_operations_immutable BEFORE UPDATE OR DELETE ON h_operations FOR EACH ROW EXECUTE FUNCTION h_operation_transition();


-- Per-schema (node), per-account capacity is reserved under this row lock.
-- Operation reservations are separate from immutable commitment evidence.
CREATE TABLE h_resource_budgets (
    account_id text PRIMARY KEY REFERENCES accounts(id),
    pending_operations bigint NOT NULL DEFAULT 0 CHECK(pending_operations BETWEEN 0 AND 256),
    retained_bytes bigint NOT NULL DEFAULT 0 CHECK(retained_bytes BETWEEN 0 AND 8388608)
);
CREATE TABLE h_resource_reservations (
    account_id text NOT NULL,
    operation_id bytea NOT NULL,
    envelope_bytes bigint NOT NULL CHECK(envelope_bytes BETWEEN 0 AND 65536),
    fragment_bytes bigint NOT NULL DEFAULT 0 CHECK(fragment_bytes >= 0),
    pending boolean NOT NULL DEFAULT true,
    PRIMARY KEY(account_id,operation_id),
    FOREIGN KEY(account_id,operation_id) REFERENCES h_operations(account_id,operation_id)
);
CREATE FUNCTION h_release_payload_capacity() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE r h_resource_reservations;
BEGIN
    PERFORM 1 FROM h_resource_budgets WHERE account_id=OLD.account_id FOR UPDATE;
    SELECT * INTO r FROM h_resource_reservations WHERE account_id=OLD.account_id AND operation_id=OLD.operation_id FOR UPDATE;
    IF FOUND THEN
        UPDATE h_resource_budgets SET pending_operations=pending_operations-CASE WHEN r.pending THEN 1 ELSE 0 END,
          retained_bytes=retained_bytes-r.envelope_bytes WHERE account_id=OLD.account_id;
        UPDATE h_resource_reservations SET envelope_bytes=0,pending=false
          WHERE account_id=OLD.account_id AND operation_id=OLD.operation_id;
    END IF;
    RETURN OLD;
END $$;
CREATE TRIGGER h_payload_capacity_release AFTER DELETE ON h_operation_payloads
  FOR EACH ROW EXECUTE FUNCTION h_release_payload_capacity();

-- LC-S5C original accounting, governed payload retirement and value-free pins.
CREATE TABLE h_replay_work (
    account_id text NOT NULL REFERENCES accounts(id),
    event_id text NOT NULL,
    operation_id bytea NOT NULL CHECK(octet_length(operation_id)=16),
    node text NOT NULL,
    reason text NOT NULL,
    PRIMARY KEY(account_id,event_id)
);
CREATE TRIGGER h_actions_immutable BEFORE UPDATE OR DELETE ON h_logical_actions
    FOR EACH ROW EXECUTE FUNCTION h_immutable_row();
CREATE TABLE h_deletion_knowledge (
    account_id text NOT NULL,
    family text NOT NULL,
    object_id text NOT NULL,
    generation bytea NOT NULL CHECK(octet_length(generation)=16),
    deletion_generation bytea NOT NULL CHECK(octet_length(deletion_generation)=16),
    accepted_delete_id bytea NOT NULL CHECK(octet_length(accepted_delete_id)=16),
    issuer_stream text NOT NULL,
    frontier bigint NOT NULL CHECK(frontier>0),
    evidence_ref text NOT NULL,
    deleted_at timestamptz NOT NULL,
    retain_until timestamptz NOT NULL,
    PRIMARY KEY(account_id,family,object_id,generation,deletion_generation),
    FOREIGN KEY(account_id,accepted_delete_id) REFERENCES h_receipts(account_id,operation_id),
    CHECK(retain_until >= deleted_at + interval '37 days')
);
CREATE TABLE h_retained_evidence (
    account_id text NOT NULL REFERENCES accounts(id),
    evidence_kind text NOT NULL CHECK(evidence_kind IN ('identity','evidence','key')),
    evidence_id text NOT NULL,
    retain_until timestamptz NOT NULL,
    PRIMARY KEY(account_id,evidence_kind,evidence_id)
);
CREATE TABLE h_evidence_pins (
    account_id text NOT NULL,
    evidence_kind text NOT NULL,
    evidence_id text NOT NULL,
    pin_owner text NOT NULL,
    reason text NOT NULL,
    released_at timestamptz,
    PRIMARY KEY(account_id,evidence_kind,evidence_id,pin_owner),
    FOREIGN KEY(account_id,evidence_kind,evidence_id)
      REFERENCES h_retained_evidence(account_id,evidence_kind,evidence_id)
);
CREATE FUNCTION h_collect_evidence(a text, k text, e text, at_time timestamptz)
RETURNS boolean LANGUAGE plpgsql AS $$
BEGIN
    -- Lock against concurrent pin creation through the scoped FK.
    PERFORM 1 FROM h_retained_evidence WHERE account_id=a AND evidence_kind=k AND evidence_id=e FOR UPDATE;
    IF EXISTS(SELECT 1 FROM h_evidence_pins WHERE account_id=a AND evidence_kind=k AND evidence_id=e AND released_at IS NULL) THEN
        RETURN false;
    END IF;
    IF NOT EXISTS(SELECT 1 FROM h_retained_evidence WHERE account_id=a AND evidence_kind=k AND evidence_id=e AND retain_until<=at_time) THEN
        RETURN false;
    END IF;
    DELETE FROM h_evidence_pins WHERE account_id=a AND evidence_kind=k AND evidence_id=e AND released_at IS NOT NULL;
    DELETE FROM h_retained_evidence WHERE account_id=a AND evidence_kind=k AND evidence_id=e;
    RETURN true;
END $$;
CREATE TABLE h_payload_retirements (
    account_id text NOT NULL,
    operation_id bytea NOT NULL CHECK(octet_length(operation_id)=16),
    retired_at timestamptz NOT NULL,
    PRIMARY KEY(account_id,operation_id),
    FOREIGN KEY(account_id,operation_id) REFERENCES h_operations(account_id,operation_id)
);
DROP TRIGGER h_payloads_immutable ON h_operation_payloads;
CREATE FUNCTION h_payload_retirement_guard() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP='DELETE' AND EXISTS (
        SELECT 1 FROM h_payload_retirements p JOIN h_receipts r USING(account_id,operation_id)
        WHERE p.account_id=OLD.account_id AND p.operation_id=OLD.operation_id
        AND r.retain_until>=p.retired_at+interval '37 days') THEN
        RETURN OLD;
    END IF;
    RAISE EXCEPTION 'immutable harness payload outside governed retirement' USING ERRCODE='23514';
END $$;
CREATE TRIGGER h_payloads_immutable BEFORE UPDATE OR DELETE ON h_operation_payloads
    FOR EACH ROW EXECUTE FUNCTION h_payload_retirement_guard();

-- LC-S5C capture is activated per synthetic account only. These guards do not
-- instrument production tables and do not certify distinct database principals.
CREATE TABLE h_capture (
    account_id text PRIMARY KEY REFERENCES accounts(id),
    capture_epoch bigint NOT NULL CHECK(capture_epoch > 0),
    lifecycle_epoch bigint NOT NULL CHECK(lifecycle_epoch >= 0),
    restore_marker bytea NOT NULL CHECK(octet_length(restore_marker)=16),
    capability text NOT NULL,
    coverage_state text NOT NULL CHECK(coverage_state IN ('active','suspended'))
);
CREATE TABLE h_writer_capabilities (
    account_id text NOT NULL REFERENCES accounts(id), principal text NOT NULL,
    capability text NOT NULL, manifest_version text NOT NULL,
    revoked_generation bigint NOT NULL DEFAULT 0, enabled boolean NOT NULL,
    PRIMARY KEY(account_id,principal,capability)
);
CREATE TABLE h_tx_admissions (
    account_id text NOT NULL REFERENCES accounts(id), tx_id bigint NOT NULL,
    capability text NOT NULL, lifecycle_epoch bigint NOT NULL,
    restore_marker bytea NOT NULL CHECK(octet_length(restore_marker)=16),
    capture_epoch bigint NOT NULL, sealed boolean NOT NULL DEFAULT false,
    operation_id bytea CHECK(octet_length(operation_id)=16), atomic_group text,
    PRIMARY KEY(account_id,tx_id),
    FOREIGN KEY(account_id,operation_id) REFERENCES h_operations(account_id,operation_id),
    CHECK((operation_id IS NULL)=(atomic_group IS NULL)),
    CHECK(atomic_group IS NULL OR atomic_group<>'')
);
CREATE TABLE h_tx_touches (
    account_id text NOT NULL, tx_id bigint NOT NULL, touch_id bigint NOT NULL,
    semantic_effect jsonb NOT NULL, dependencies jsonb NOT NULL,
    operation_id bytea NOT NULL CHECK(octet_length(operation_id)=16), atomic_group text NOT NULL CHECK(atomic_group<>''),
    PRIMARY KEY(account_id,tx_id,touch_id),
    FOREIGN KEY(account_id,tx_id) REFERENCES h_tx_admissions(account_id,tx_id),
    FOREIGN KEY(account_id,operation_id) REFERENCES h_operations(account_id,operation_id)
);
CREATE TABLE h_finalized_counter (
    account_id text NOT NULL REFERENCES accounts(id), capture_epoch bigint NOT NULL,
    last_finalized bigint NOT NULL CHECK(last_finalized >= 0),
    PRIMARY KEY(account_id,capture_epoch)
);
CREATE TABLE h_bundles (
    account_id text NOT NULL, bundle_id text NOT NULL, tx_id bigint NOT NULL,
    capture_epoch bigint NOT NULL, lifecycle_epoch bigint NOT NULL,
    restore_marker bytea NOT NULL CHECK(octet_length(restore_marker)=16),
    finalized bigint NOT NULL CHECK(finalized > 0), sealed boolean NOT NULL CHECK(sealed),
    origin_class text NOT NULL CHECK(origin_class IN ('original','replay')),
    semantic_effects jsonb NOT NULL, dependencies jsonb NOT NULL,
    PRIMARY KEY(account_id,bundle_id), UNIQUE(account_id,capture_epoch,finalized),
    UNIQUE(account_id,tx_id),
    FOREIGN KEY(account_id,tx_id) REFERENCES h_tx_admissions(account_id,tx_id)
);
CREATE TABLE h_bundle_operations (
    account_id text NOT NULL, bundle_id text NOT NULL, member_order bigint NOT NULL,
    operation_id bytea NOT NULL CHECK(octet_length(operation_id)=16),
    atomic_group text NOT NULL CHECK(atomic_group<>''),
    PRIMARY KEY(account_id,bundle_id,member_order),
    FOREIGN KEY(account_id,bundle_id) REFERENCES h_bundles(account_id,bundle_id),
    FOREIGN KEY(account_id,operation_id) REFERENCES h_operations(account_id,operation_id)
);
CREATE TABLE h_publication_outbox (
    account_id text NOT NULL REFERENCES accounts(id), issuer_stream text NOT NULL,
    through_ordinal bigint NOT NULL CHECK(through_ordinal>0),
    prefix_commitment bytea NOT NULL CHECK(octet_length(prefix_commitment)=32),
    prefix_members bytea NOT NULL, operation_id bytea,
    proof_kind text NOT NULL DEFAULT 'receipt' CHECK(proof_kind IN ('receipt','control','grant')),
    state text NOT NULL DEFAULT 'pending' CHECK(state IN ('pending','pinned')),
    witness_proof_ref text,
    PRIMARY KEY(account_id,issuer_stream,through_ordinal),
    FOREIGN KEY(account_id,operation_id) REFERENCES h_receipts(account_id,operation_id),
    CHECK((state='pinned')=(witness_proof_ref IS NOT NULL))
);
ALTER TABLE h_fixture_objects ADD COLUMN dependencies jsonb NOT NULL DEFAULT '[]';
CREATE FUNCTION h_admit_capture(a text) RETURNS void LANGUAGE plpgsql AS $$
DECLARE c h_capture; w h_writer_capabilities;
BEGIN
    PERFORM pg_advisory_xact_lock_shared(hashtextextended(current_schema()||chr(31)||a||chr(31)||'capture',0));
    SELECT * INTO w FROM h_writer_capabilities WHERE account_id=a AND principal='fixture_writer' AND capability='capture_v1' FOR SHARE;
    IF NOT FOUND OR NOT w.enabled THEN RAISE EXCEPTION 'writer capability refused' USING ERRCODE='23514'; END IF;
    SELECT * INTO c FROM h_capture WHERE account_id=a FOR SHARE;
    IF NOT FOUND OR c.coverage_state<>'active' OR c.capability<>w.capability THEN RAISE EXCEPTION 'capture control refused' USING ERRCODE='23514'; END IF;
    INSERT INTO h_tx_admissions(account_id,tx_id,capability,lifecycle_epoch,restore_marker,capture_epoch)
      VALUES(a,txid_current(),w.capability,c.lifecycle_epoch,c.restore_marker,c.capture_epoch);
END $$;
CREATE FUNCTION h_capture_guard() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE a text; admitted h_tx_admissions; n bigint;
BEGIN
    IF TG_OP='UPDATE' AND NEW.account_id<>OLD.account_id THEN RAISE EXCEPTION 'capture scope refused' USING ERRCODE='23514'; END IF;
    a:=COALESCE(OLD.account_id,NEW.account_id);
    IF NOT EXISTS(SELECT 1 FROM h_capture WHERE account_id=a) THEN RETURN COALESCE(NEW,OLD); END IF;
    SELECT * INTO admitted FROM h_tx_admissions WHERE account_id=a AND tx_id=txid_current();
    IF NOT FOUND OR admitted.sealed OR admitted.operation_id IS NULL THEN RAISE EXCEPTION 'capture admission or seal refused' USING ERRCODE='23514'; END IF;
    IF TG_OP='UPDATE' AND NEW.account_id<>OLD.account_id THEN RAISE EXCEPTION 'capture scope refused' USING ERRCODE='23514'; END IF;
    SELECT COALESCE(max(touch_id),0)+1 INTO n FROM h_tx_touches WHERE account_id=a AND tx_id=txid_current();
    INSERT INTO h_tx_touches VALUES(a,txid_current(),n,jsonb_build_object('table','h_fixture_objects','action',TG_OP,'before',to_jsonb(OLD),'after',to_jsonb(NEW)),COALESCE(NEW.dependencies,OLD.dependencies),admitted.operation_id,admitted.atomic_group);
    RETURN COALESCE(NEW,OLD);
END $$;
CREATE TRIGGER h_fixture_capture BEFORE INSERT OR UPDATE OR DELETE ON h_fixture_objects FOR EACH ROW EXECUTE FUNCTION h_capture_guard();
CREATE FUNCTION h_check_capture_coverage() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE b h_bundles; effects jsonb; deps jsonb;
BEGIN
    IF NOT EXISTS(SELECT 1 FROM h_tx_admissions WHERE account_id=NEW.account_id AND tx_id=NEW.tx_id AND sealed) THEN RAISE EXCEPTION 'capture transaction not sealed' USING ERRCODE='23514'; END IF;
    SELECT * INTO b FROM h_bundles WHERE account_id=NEW.account_id AND tx_id=NEW.tx_id;
    IF NOT FOUND THEN RAISE EXCEPTION 'semantic journal omitted' USING ERRCODE='23514'; END IF;
    SELECT jsonb_agg(semantic_effect ORDER BY touch_id),jsonb_agg(dependencies ORDER BY touch_id) INTO effects,deps
      FROM h_tx_touches WHERE account_id=NEW.account_id AND tx_id=NEW.tx_id;
    IF b.semantic_effects IS DISTINCT FROM COALESCE(effects,'[]'::jsonb) OR b.dependencies IS DISTINCT FROM COALESCE(deps,'[]'::jsonb) OR
       EXISTS((SELECT operation_id,atomic_group FROM h_tx_touches WHERE account_id=NEW.account_id AND tx_id=NEW.tx_id)
              EXCEPT (SELECT operation_id,atomic_group FROM h_bundle_operations WHERE account_id=b.account_id AND bundle_id=b.bundle_id)) OR
       EXISTS((SELECT operation_id,atomic_group FROM h_bundle_operations WHERE account_id=b.account_id AND bundle_id=b.bundle_id)
              EXCEPT (SELECT operation_id,atomic_group FROM h_tx_touches WHERE account_id=NEW.account_id AND tx_id=NEW.tx_id)) THEN
      RAISE EXCEPTION 'semantic journal incomplete' USING ERRCODE='23514';
    END IF;
    RETURN NULL;
END $$;
CREATE CONSTRAINT TRIGGER h_deferred_coverage AFTER INSERT ON h_tx_admissions DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION h_check_capture_coverage();
CREATE TRIGGER h_bundle_immutable BEFORE UPDATE OR DELETE ON h_bundles FOR EACH ROW EXECUTE FUNCTION h_immutable_row();
CREATE TRIGGER h_bundle_members_immutable BEFORE UPDATE OR DELETE ON h_bundle_operations FOR EACH ROW EXECUTE FUNCTION h_immutable_row();
CREATE TRIGGER h_touch_immutable BEFORE UPDATE OR DELETE ON h_tx_touches FOR EACH ROW EXECUTE FUNCTION h_immutable_row();

-- LC-S5C miniature compound staging and inert projection; no production domain.
CREATE TABLE h_graph_authority (
    account_id text NOT NULL REFERENCES accounts(id),
    request_id text NOT NULL,
    graph_epoch bigint NOT NULL CHECK (graph_epoch > 0),
    graph_revision bigint NOT NULL CHECK (graph_revision >= 0),
    authority_installation text NOT NULL,
    state text NOT NULL CHECK (state IN ('active','fenced','retired')),
    PRIMARY KEY (account_id,request_id,graph_epoch)
);
CREATE UNIQUE INDEX h_one_active_graph ON h_graph_authority(account_id,request_id) WHERE state='active';
CREATE TABLE h_compounds (
    account_id text NOT NULL,
    group_id text NOT NULL,
    operation_id bytea NOT NULL CHECK (octet_length(operation_id)=16),
    request_id text NOT NULL,
    graph_epoch bigint NOT NULL CHECK (graph_epoch > 0),
    expected_revision bigint NOT NULL CHECK (expected_revision >= 0),
    result_revision bigint NOT NULL CHECK (result_revision > expected_revision),
    authority_installation text NOT NULL,
    manifest bytea NOT NULL,
    required_fragments text[] NOT NULL CHECK (cardinality(required_fragments) BETWEEN 1 AND 64),
    dependency_object text NOT NULL,
    state text NOT NULL CHECK (state IN ('staged','publication_pending','published')),
    complete boolean NOT NULL DEFAULT false,
    finalized bigint CHECK (finalized > 0),
    PRIMARY KEY (account_id,group_id),
    UNIQUE (account_id,group_id,request_id),
    UNIQUE (account_id,group_id,operation_id),
    UNIQUE (account_id,operation_id),
    FOREIGN KEY (account_id,operation_id) REFERENCES h_operations(account_id,operation_id),
    FOREIGN KEY (account_id,request_id,graph_epoch) REFERENCES h_graph_authority(account_id,request_id,graph_epoch),
    CHECK ((state='staged')=(finalized IS NULL)),
    CHECK (state='staged' OR complete)
);
CREATE TABLE h_inbound (
    account_id text NOT NULL,
    operation_id bytea NOT NULL CHECK (octet_length(operation_id)=16),
    group_id text NOT NULL,
    state text NOT NULL CHECK (state IN ('staged','applied')),
    PRIMARY KEY (account_id,operation_id),
    FOREIGN KEY (account_id,operation_id) REFERENCES h_operations(account_id,operation_id),
    FOREIGN KEY (account_id,group_id,operation_id) REFERENCES h_compounds(account_id,group_id,operation_id)
);
CREATE TABLE h_fragments (
    account_id text NOT NULL,
    group_id text NOT NULL,
    fragment_id text NOT NULL,
    member_index integer NOT NULL CHECK (member_index BETWEEN 0 AND 63),
    commitment bytea NOT NULL CHECK (octet_length(commitment)=32),
    fragment_bytes bytea NOT NULL CHECK (octet_length(fragment_bytes) BETWEEN 1 AND 65536),
    PRIMARY KEY (account_id,group_id,fragment_id),
    UNIQUE (account_id,group_id,member_index),
    FOREIGN KEY (account_id,group_id) REFERENCES h_compounds(account_id,group_id)
);
CREATE TRIGGER h_fragments_immutable BEFORE UPDATE OR DELETE ON h_fragments FOR EACH ROW EXECUTE FUNCTION h_immutable_row();
CREATE TABLE h_fixture_messages (
    account_id text NOT NULL REFERENCES accounts(id),
    message_id text NOT NULL,
    group_id text,
    request_id text,
    visible boolean NOT NULL,
    publication_state text NOT NULL CHECK (publication_state IN ('ordinary','durability_publication_pending','published')),
    claimed boolean NOT NULL DEFAULT false,
    PRIMARY KEY (account_id,message_id),
    UNIQUE (account_id,message_id,group_id,request_id),
    FOREIGN KEY (account_id,group_id,request_id) REFERENCES h_compounds(account_id,group_id,request_id),
    CHECK ((group_id IS NULL)=(request_id IS NULL)),
    CHECK ((group_id IS NULL)=(publication_state='ordinary'))
);
CREATE TABLE h_fixture_request_links (
    account_id text NOT NULL,
    group_id text NOT NULL,
    request_id text NOT NULL,
    message_id text NOT NULL,
    link_kind text NOT NULL CHECK (link_kind IN ('graph','candidate','sibling','claim','notification')),
    visible boolean NOT NULL,
    PRIMARY KEY (account_id,group_id,link_kind),
    FOREIGN KEY (account_id,group_id,request_id) REFERENCES h_compounds(account_id,group_id,request_id),
    FOREIGN KEY (account_id,message_id,group_id,request_id) REFERENCES h_fixture_messages(account_id,message_id,group_id,request_id)
);
CREATE FUNCTION h_compound_projection_complete() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE c h_compounds%ROWTYPE; a text; g text; visible_messages bigint; visible_links bigint;
BEGIN
    IF TG_OP='DELETE' THEN a:=OLD.account_id; g:=OLD.group_id;
    ELSE a:=NEW.account_id; g:=NEW.group_id; END IF;
    IF g IS NULL THEN RETURN NULL; END IF;
    SELECT count(*) INTO visible_messages FROM h_fixture_messages WHERE account_id=a AND group_id=g AND visible;
    SELECT count(*) INTO visible_links FROM h_fixture_request_links WHERE account_id=a AND group_id=g AND visible;
    IF visible_messages=0 AND visible_links=0 THEN RETURN NULL; END IF;
    SELECT * INTO c FROM h_compounds WHERE account_id=a AND group_id=g;
    IF c.state='staged' OR NOT c.complete OR
       visible_messages<>1 OR visible_links<>cardinality(c.required_fragments) OR
       NOT EXISTS (SELECT 1 FROM h_receipts WHERE account_id=c.account_id AND operation_id=c.operation_id) OR
       NOT EXISTS (SELECT 1 FROM h_logical_actions WHERE account_id=c.account_id AND operation_id=c.operation_id) THEN
        RAISE EXCEPTION 'incomplete harness compound projection' USING ERRCODE='23514';
    END IF;
    RETURN NULL;
END $$;
CREATE CONSTRAINT TRIGGER h_message_complete AFTER INSERT OR UPDATE OR DELETE ON h_fixture_messages DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION h_compound_projection_complete();
CREATE CONSTRAINT TRIGGER h_link_complete AFTER INSERT OR UPDATE OR DELETE ON h_fixture_request_links DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION h_compound_projection_complete();

-- Deletion knowledge and retirement authorizations remain immutable; collection
-- of resolved deletion records is outside this local fixture's retention lane.
CREATE TRIGGER h_deletion_immutable BEFORE UPDATE OR DELETE ON h_deletion_knowledge
    FOR EACH ROW EXECUTE FUNCTION h_immutable_row();
CREATE TRIGGER h_retirements_immutable BEFORE UPDATE OR DELETE ON h_payload_retirements
    FOR EACH ROW EXECUTE FUNCTION h_immutable_row();

-- Publication acknowledgements may only settle the previously committed proof.
CREATE FUNCTION h_publication_transition() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP='DELETE' OR
       (to_jsonb(NEW)-'state'-'witness_proof_ref') IS DISTINCT FROM (to_jsonb(OLD)-'state'-'witness_proof_ref') OR
       (OLD.state='pinned' AND NEW IS DISTINCT FROM OLD) THEN
       RAISE EXCEPTION 'immutable publication proof' USING ERRCODE='23514';
    END IF;
    RETURN NEW;
END $$;
CREATE TRIGGER h_publication_immutable BEFORE UPDATE OR DELETE ON h_publication_outbox FOR EACH ROW EXECUTE FUNCTION h_publication_transition();
CREATE FUNCTION h_capture_admission_transition() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP='DELETE' OR OLD.sealed OR
       (to_jsonb(NEW)-'sealed'-'operation_id'-'atomic_group') IS DISTINCT FROM (to_jsonb(OLD)-'sealed'-'operation_id'-'atomic_group') OR
       NOT ((NOT NEW.sealed AND OLD.operation_id IS NULL AND NEW.operation_id IS NOT NULL) OR
            (NEW.sealed AND NEW.operation_id IS NOT DISTINCT FROM OLD.operation_id AND NEW.atomic_group IS NOT DISTINCT FROM OLD.atomic_group)) THEN
       RAISE EXCEPTION 'immutable capture admission' USING ERRCODE='23514';
    END IF;
    RETURN NEW;
END $$;
CREATE TRIGGER h_capture_admission_immutable BEFORE UPDATE OR DELETE ON h_tx_admissions FOR EACH ROW EXECUTE FUNCTION h_capture_admission_transition();

-- Local provisional capture positions and terminal issuer acceptance positions
-- are separate streams. A capture is never fabricated into a terminal receipt.
CREATE TABLE h_acceptance_counter (
    account_id text NOT NULL REFERENCES accounts(id), issuer_stream text NOT NULL,
    last_accepted bigint NOT NULL CHECK(last_accepted>=0),
    PRIMARY KEY(account_id,issuer_stream)
);
