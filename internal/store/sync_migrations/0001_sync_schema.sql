-- +goose Up
-- LC-S6B: explicit local-only namespace. Core schema/version remains untouched.
-- No account is capture-ready: capability activation and domain guards belong
-- to LC-S6C. Every control row is inert, and all budgets require explicit limits.
-- The 37-day receipt/deletion floor is SC-4.7, not a fixture capacity default.

CREATE TABLE sync_incarnations (
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

CREATE TABLE sync_operations (
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
        REFERENCES sync_incarnations(account_id,lifecycle_epoch,restore_marker)
);

CREATE TABLE sync_operation_payloads (
    account_id text NOT NULL REFERENCES accounts(id),
    operation_id bytea NOT NULL CHECK (octet_length(operation_id) = 16),
    canonical_semantic_envelope bytea NOT NULL CHECK (octet_length(canonical_semantic_envelope) BETWEEN 1 AND 65536),
    governed_retirement timestamptz,
    PRIMARY KEY (account_id,operation_id),
    FOREIGN KEY (account_id,operation_id) REFERENCES sync_operations(account_id,operation_id)
);

CREATE TABLE sync_verifier_history (
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

CREATE TABLE sync_receipts (
    account_id text NOT NULL REFERENCES accounts(id),
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
        REFERENCES sync_operations(account_id,operation_id,comparison_key_id,input_commitment),
    FOREIGN KEY (account_id,operation_id,original_lifecycle_epoch,original_restore_marker,
        original_capture_epoch,original_installation,original_replica,original_write_lineage,original_sequence)
        REFERENCES sync_operations(account_id,operation_id,original_lifecycle_epoch,original_restore_marker,
            original_capture_epoch,original_installation,original_replica,original_write_lineage,original_sequence),
    FOREIGN KEY (account_id,issuer_lifecycle_epoch,issuer_restore_marker)
        REFERENCES sync_incarnations(account_id,lifecycle_epoch,restore_marker),
    FOREIGN KEY (account_id,issuer_id,authority_stream,signing_key_id)
        REFERENCES sync_verifier_history(account_id,issuer_id,authority_stream,signing_key_id),
    CHECK ((status='accepted' AND reason='applied') OR
           (status='rejected' AND reason IN ('invalid_envelope','unauthorized','authorization_expired','unsupported_operation','schema_incompatible','protocol_incompatible','capacity_refused','account_closed')) OR
           (status='conflict' AND reason IN ('expected_version_mismatch','dependency_unavailable','identity_collision','deletion_conflict','authority_epoch_stale','incarnation_mismatch','operation_id_reuse'))),
    CHECK (retain_until >= settled_at + interval '37 days'),
    CHECK (retired_at IS NULL OR retain_until >= retired_at + interval '37 days')
);

CREATE TABLE sync_reuse_refusals (
    account_id text NOT NULL REFERENCES accounts(id),
    operation_id bytea NOT NULL CHECK (octet_length(operation_id) = 16),
    comparison_key_id text NOT NULL,
    conflicting_commitment bytea NOT NULL CHECK (octet_length(conflicting_commitment) = 32),
    canonical_body bytea NOT NULL CHECK (octet_length(canonical_body) > 0),
    signed_bytes bytea NOT NULL CHECK (octet_length(signed_bytes) = 64),
    retain_until timestamptz NOT NULL,
    PRIMARY KEY (account_id,operation_id,comparison_key_id,conflicting_commitment),
    FOREIGN KEY (account_id,operation_id,comparison_key_id)
        REFERENCES sync_operations(account_id,operation_id,comparison_key_id)
);

CREATE TABLE sync_capture (
    account_id text PRIMARY KEY REFERENCES accounts(id),
    capture_epoch bigint NOT NULL CHECK(capture_epoch > 0),
    lifecycle_epoch bigint NOT NULL CHECK(lifecycle_epoch BETWEEN 0 AND 9007199254740991),
    restore_marker bytea NOT NULL CHECK(octet_length(restore_marker)=16),
    capability text NOT NULL,
    coverage_state text NOT NULL CHECK(coverage_state IN ('bootstrapping','suspended'))
);

CREATE TABLE sync_writer_capabilities (
    account_id text NOT NULL REFERENCES accounts(id), principal text NOT NULL,
    capability text NOT NULL, manifest_version text NOT NULL,
    revoked_generation bigint NOT NULL DEFAULT 0 CHECK(revoked_generation >= 0), enabled boolean NOT NULL CHECK(NOT enabled),
    PRIMARY KEY(account_id,principal,capability)
);

CREATE TABLE sync_tx_admissions (
    account_id text NOT NULL REFERENCES accounts(id), tx_id bigint NOT NULL CHECK(tx_id > 0),
    capability text NOT NULL, lifecycle_epoch bigint NOT NULL CHECK(lifecycle_epoch BETWEEN 0 AND 9007199254740991),
    restore_marker bytea NOT NULL CHECK(octet_length(restore_marker)=16),
    capture_epoch bigint NOT NULL CHECK(capture_epoch > 0), sealed boolean NOT NULL DEFAULT false,
    operation_id bytea CHECK(octet_length(operation_id)=16), atomic_group text,
    PRIMARY KEY(account_id,tx_id),
    FOREIGN KEY(account_id,operation_id) REFERENCES sync_operations(account_id,operation_id),
    CHECK((operation_id IS NULL)=(atomic_group IS NULL)),
    CHECK(atomic_group IS NULL OR atomic_group<>'')
);

CREATE TABLE sync_tx_touches (
    account_id text NOT NULL REFERENCES accounts(id), tx_id bigint NOT NULL CHECK(tx_id > 0), touch_id bigint NOT NULL CHECK(touch_id >= 0),
    semantic_effect jsonb NOT NULL, dependencies jsonb NOT NULL,
    operation_id bytea NOT NULL CHECK(octet_length(operation_id)=16), atomic_group text NOT NULL CHECK(atomic_group<>''),
    PRIMARY KEY(account_id,tx_id,touch_id),
    FOREIGN KEY(account_id,tx_id) REFERENCES sync_tx_admissions(account_id,tx_id),
    FOREIGN KEY(account_id,operation_id) REFERENCES sync_operations(account_id,operation_id)
);

CREATE TABLE sync_finalized_counter (
    account_id text NOT NULL REFERENCES accounts(id), capture_epoch bigint NOT NULL CHECK(capture_epoch > 0),
    last_finalized bigint NOT NULL CHECK(last_finalized >= 0),
    PRIMARY KEY(account_id,capture_epoch)
);

CREATE TABLE sync_bundles (
    account_id text NOT NULL REFERENCES accounts(id), bundle_id bytea NOT NULL CHECK(octet_length(bundle_id)=16), tx_id bigint NOT NULL CHECK(tx_id > 0),
    capture_epoch bigint NOT NULL CHECK(capture_epoch > 0), lifecycle_epoch bigint NOT NULL CHECK(lifecycle_epoch BETWEEN 0 AND 9007199254740991),
    restore_marker bytea NOT NULL CHECK(octet_length(restore_marker)=16),
    finalized bigint NOT NULL CHECK(finalized > 0), sealed boolean NOT NULL CHECK(sealed),
    origin_class text NOT NULL CHECK(origin_class IN ('original','replay')),
    semantic_effects jsonb NOT NULL, dependencies jsonb NOT NULL,
    PRIMARY KEY(account_id,bundle_id), UNIQUE(account_id,capture_epoch,finalized),
    UNIQUE(account_id,tx_id)
);

CREATE TABLE sync_bundle_operations (
    account_id text NOT NULL REFERENCES accounts(id), bundle_id bytea NOT NULL CHECK(octet_length(bundle_id)=16), member_order bigint NOT NULL CHECK(member_order >= 0),
    operation_id bytea NOT NULL CHECK(octet_length(operation_id)=16),
    atomic_group text NOT NULL CHECK(atomic_group<>''),
    PRIMARY KEY(account_id,bundle_id,member_order),
    FOREIGN KEY(account_id,bundle_id) REFERENCES sync_bundles(account_id,bundle_id),
    FOREIGN KEY(account_id,operation_id) REFERENCES sync_operations(account_id,operation_id)
);

CREATE TABLE sync_publication_outbox (
    account_id text NOT NULL REFERENCES accounts(id), issuer_stream text NOT NULL,
    through_ordinal bigint NOT NULL CHECK(through_ordinal>0),
    prefix_commitment bytea NOT NULL CHECK(octet_length(prefix_commitment)=32),
    prefix_members bytea NOT NULL, operation_id bytea CHECK(octet_length(operation_id)=16),
    proof_kind text NOT NULL DEFAULT 'receipt' CHECK(proof_kind IN ('receipt','control','grant')),
    state text NOT NULL DEFAULT 'pending' CHECK(state IN ('pending','pinned')),
    witness_proof_ref text,
    PRIMARY KEY(account_id,issuer_stream,through_ordinal),
    FOREIGN KEY(account_id,operation_id) REFERENCES sync_receipts(account_id,operation_id),
    CHECK((state='pinned')=(witness_proof_ref IS NOT NULL))
);

CREATE TABLE sync_acceptance_counter (
    account_id text NOT NULL REFERENCES accounts(id), issuer_stream text NOT NULL,
    last_accepted bigint NOT NULL CHECK(last_accepted>=0),
    PRIMARY KEY(account_id,issuer_stream)
);

CREATE TABLE sync_resource_budgets (
    account_id text PRIMARY KEY REFERENCES accounts(id),
    max_pending_operations bigint NOT NULL CHECK(max_pending_operations > 0),
    max_retained_bytes bigint NOT NULL CHECK(max_retained_bytes > 0),
    pending_operations bigint NOT NULL DEFAULT 0 CHECK(pending_operations >= 0),
    retained_bytes bigint NOT NULL DEFAULT 0 CHECK(retained_bytes >= 0),
    CHECK(pending_operations <= max_pending_operations),
    CHECK(retained_bytes <= max_retained_bytes)
);

CREATE TABLE sync_resource_reservations (
    account_id text NOT NULL REFERENCES accounts(id),
    operation_id bytea NOT NULL CHECK(octet_length(operation_id)=16),
    envelope_bytes bigint NOT NULL CHECK(envelope_bytes BETWEEN 0 AND 65536),
    fragment_bytes bigint NOT NULL DEFAULT 0 CHECK(fragment_bytes >= 0),
    pending boolean NOT NULL DEFAULT true,
    PRIMARY KEY(account_id,operation_id),
    FOREIGN KEY(account_id,operation_id) REFERENCES sync_operations(account_id,operation_id)
);

CREATE TABLE sync_deletion_knowledge (
    account_id text NOT NULL REFERENCES accounts(id),
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
    FOREIGN KEY(account_id,accepted_delete_id) REFERENCES sync_receipts(account_id,operation_id),
    CHECK(retain_until >= deleted_at + interval '37 days')
);

CREATE TABLE sync_retained_evidence (
    account_id text NOT NULL REFERENCES accounts(id),
    evidence_kind text NOT NULL CHECK(evidence_kind IN ('identity','evidence','key')),
    evidence_id text NOT NULL,
    retain_until timestamptz NOT NULL,
    PRIMARY KEY(account_id,evidence_kind,evidence_id)
);

CREATE TABLE sync_evidence_pins (
    account_id text NOT NULL REFERENCES accounts(id),
    evidence_kind text NOT NULL,
    evidence_id text NOT NULL,
    pin_owner text NOT NULL,
    reason text NOT NULL,
    released_at timestamptz,
    PRIMARY KEY(account_id,evidence_kind,evidence_id,pin_owner),
    FOREIGN KEY(account_id,evidence_kind,evidence_id)
      REFERENCES sync_retained_evidence(account_id,evidence_kind,evidence_id)
);

CREATE TABLE sync_payload_retirements (
    account_id text NOT NULL REFERENCES accounts(id),
    operation_id bytea NOT NULL CHECK(octet_length(operation_id)=16),
    retired_at timestamptz NOT NULL,
    PRIMARY KEY(account_id,operation_id),
    FOREIGN KEY(account_id,operation_id) REFERENCES sync_operations(account_id,operation_id)
);

ALTER TABLE sync_operations ADD CONSTRAINT sync_operation_receipt_ref
    FOREIGN KEY(account_id,receipt_ref) REFERENCES sync_receipts(account_id,operation_id)
    DEFERRABLE INITIALLY DEFERRED;

-- Evidence cannot be rewritten, cascaded away, or truncated. A later reviewed
-- retention migration must provide the governed retirement path.
-- +goose StatementBegin
CREATE FUNCTION witself_sync_immutable_evidence() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'sync evidence is immutable' USING ERRCODE='23514';
END;
$$;
-- +goose StatementEnd
CREATE TRIGGER sync_evidence_immutable BEFORE UPDATE OR DELETE ON sync_incarnations
    FOR EACH ROW EXECUTE FUNCTION witself_sync_immutable_evidence();
CREATE TRIGGER sync_evidence_no_truncate BEFORE TRUNCATE ON sync_incarnations
    FOR EACH STATEMENT EXECUTE FUNCTION witself_sync_immutable_evidence();
CREATE TRIGGER sync_evidence_immutable BEFORE UPDATE OR DELETE ON sync_operations
    FOR EACH ROW EXECUTE FUNCTION witself_sync_immutable_evidence();
CREATE TRIGGER sync_evidence_no_truncate BEFORE TRUNCATE ON sync_operations
    FOR EACH STATEMENT EXECUTE FUNCTION witself_sync_immutable_evidence();
CREATE TRIGGER sync_evidence_immutable BEFORE UPDATE OR DELETE ON sync_operation_payloads
    FOR EACH ROW EXECUTE FUNCTION witself_sync_immutable_evidence();
CREATE TRIGGER sync_evidence_no_truncate BEFORE TRUNCATE ON sync_operation_payloads
    FOR EACH STATEMENT EXECUTE FUNCTION witself_sync_immutable_evidence();
CREATE TRIGGER sync_evidence_immutable BEFORE UPDATE OR DELETE ON sync_verifier_history
    FOR EACH ROW EXECUTE FUNCTION witself_sync_immutable_evidence();
CREATE TRIGGER sync_evidence_no_truncate BEFORE TRUNCATE ON sync_verifier_history
    FOR EACH STATEMENT EXECUTE FUNCTION witself_sync_immutable_evidence();
CREATE TRIGGER sync_evidence_immutable BEFORE UPDATE OR DELETE ON sync_receipts
    FOR EACH ROW EXECUTE FUNCTION witself_sync_immutable_evidence();
CREATE TRIGGER sync_evidence_no_truncate BEFORE TRUNCATE ON sync_receipts
    FOR EACH STATEMENT EXECUTE FUNCTION witself_sync_immutable_evidence();
CREATE TRIGGER sync_evidence_immutable BEFORE UPDATE OR DELETE ON sync_reuse_refusals
    FOR EACH ROW EXECUTE FUNCTION witself_sync_immutable_evidence();
CREATE TRIGGER sync_evidence_no_truncate BEFORE TRUNCATE ON sync_reuse_refusals
    FOR EACH STATEMENT EXECUTE FUNCTION witself_sync_immutable_evidence();
CREATE TRIGGER sync_evidence_immutable BEFORE UPDATE OR DELETE ON sync_bundles
    FOR EACH ROW EXECUTE FUNCTION witself_sync_immutable_evidence();
CREATE TRIGGER sync_evidence_no_truncate BEFORE TRUNCATE ON sync_bundles
    FOR EACH STATEMENT EXECUTE FUNCTION witself_sync_immutable_evidence();
CREATE TRIGGER sync_evidence_immutable BEFORE UPDATE OR DELETE ON sync_bundle_operations
    FOR EACH ROW EXECUTE FUNCTION witself_sync_immutable_evidence();
CREATE TRIGGER sync_evidence_no_truncate BEFORE TRUNCATE ON sync_bundle_operations
    FOR EACH STATEMENT EXECUTE FUNCTION witself_sync_immutable_evidence();
CREATE TRIGGER sync_evidence_immutable BEFORE UPDATE OR DELETE ON sync_deletion_knowledge
    FOR EACH ROW EXECUTE FUNCTION witself_sync_immutable_evidence();
CREATE TRIGGER sync_evidence_no_truncate BEFORE TRUNCATE ON sync_deletion_knowledge
    FOR EACH STATEMENT EXECUTE FUNCTION witself_sync_immutable_evidence();
CREATE TRIGGER sync_evidence_immutable BEFORE UPDATE OR DELETE ON sync_retained_evidence
    FOR EACH ROW EXECUTE FUNCTION witself_sync_immutable_evidence();
CREATE TRIGGER sync_evidence_no_truncate BEFORE TRUNCATE ON sync_retained_evidence
    FOR EACH STATEMENT EXECUTE FUNCTION witself_sync_immutable_evidence();
CREATE TRIGGER sync_evidence_immutable BEFORE UPDATE OR DELETE ON sync_evidence_pins
    FOR EACH ROW EXECUTE FUNCTION witself_sync_immutable_evidence();
CREATE TRIGGER sync_evidence_no_truncate BEFORE TRUNCATE ON sync_evidence_pins
    FOR EACH STATEMENT EXECUTE FUNCTION witself_sync_immutable_evidence();
CREATE TRIGGER sync_evidence_immutable BEFORE UPDATE OR DELETE ON sync_payload_retirements
    FOR EACH ROW EXECUTE FUNCTION witself_sync_immutable_evidence();
CREATE TRIGGER sync_evidence_no_truncate BEFORE TRUNCATE ON sync_payload_retirements
    FOR EACH STATEMENT EXECUTE FUNCTION witself_sync_immutable_evidence();


-- The publication state may advance, but already committed prefix evidence may
-- never be changed or erased. This is custody, not a witness/transport adapter.
-- +goose StatementBegin
CREATE FUNCTION witself_sync_publication_transition() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'sync evidence is immutable' USING ERRCODE='23514';
    END IF;
    IF (to_jsonb(NEW) - 'state' - 'witness_proof_ref') IS DISTINCT FROM
       (to_jsonb(OLD) - 'state' - 'witness_proof_ref') OR
       OLD.state <> 'pending' OR NEW.state <> 'pinned' THEN
        RAISE EXCEPTION 'sync evidence is immutable' USING ERRCODE='23514';
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd
CREATE TRIGGER sync_evidence_immutable BEFORE UPDATE OR DELETE ON sync_publication_outbox
    FOR EACH ROW EXECUTE FUNCTION witself_sync_publication_transition();
CREATE TRIGGER sync_evidence_no_truncate BEFORE TRUNCATE ON sync_publication_outbox
    FOR EACH STATEMENT EXECUTE FUNCTION witself_sync_immutable_evidence();

-- Existing evacuation barriers cover every new account-scoped relation.
CREATE TRIGGER account_evacuation_fence BEFORE INSERT OR UPDATE OR DELETE ON sync_incarnations
    FOR EACH ROW EXECUTE FUNCTION witself_tenant_evacuation_fence();
CREATE TRIGGER account_evacuation_fence BEFORE INSERT OR UPDATE OR DELETE ON sync_operations
    FOR EACH ROW EXECUTE FUNCTION witself_tenant_evacuation_fence();
CREATE TRIGGER account_evacuation_fence BEFORE INSERT OR UPDATE OR DELETE ON sync_operation_payloads
    FOR EACH ROW EXECUTE FUNCTION witself_tenant_evacuation_fence();
CREATE TRIGGER account_evacuation_fence BEFORE INSERT OR UPDATE OR DELETE ON sync_verifier_history
    FOR EACH ROW EXECUTE FUNCTION witself_tenant_evacuation_fence();
CREATE TRIGGER account_evacuation_fence BEFORE INSERT OR UPDATE OR DELETE ON sync_receipts
    FOR EACH ROW EXECUTE FUNCTION witself_tenant_evacuation_fence();
CREATE TRIGGER account_evacuation_fence BEFORE INSERT OR UPDATE OR DELETE ON sync_reuse_refusals
    FOR EACH ROW EXECUTE FUNCTION witself_tenant_evacuation_fence();
CREATE TRIGGER account_evacuation_fence BEFORE INSERT OR UPDATE OR DELETE ON sync_capture
    FOR EACH ROW EXECUTE FUNCTION witself_tenant_evacuation_fence();
CREATE TRIGGER account_evacuation_fence BEFORE INSERT OR UPDATE OR DELETE ON sync_writer_capabilities
    FOR EACH ROW EXECUTE FUNCTION witself_tenant_evacuation_fence();
CREATE TRIGGER account_evacuation_fence BEFORE INSERT OR UPDATE OR DELETE ON sync_tx_admissions
    FOR EACH ROW EXECUTE FUNCTION witself_tenant_evacuation_fence();
CREATE TRIGGER account_evacuation_fence BEFORE INSERT OR UPDATE OR DELETE ON sync_tx_touches
    FOR EACH ROW EXECUTE FUNCTION witself_tenant_evacuation_fence();
CREATE TRIGGER account_evacuation_fence BEFORE INSERT OR UPDATE OR DELETE ON sync_finalized_counter
    FOR EACH ROW EXECUTE FUNCTION witself_tenant_evacuation_fence();
CREATE TRIGGER account_evacuation_fence BEFORE INSERT OR UPDATE OR DELETE ON sync_bundles
    FOR EACH ROW EXECUTE FUNCTION witself_tenant_evacuation_fence();
CREATE TRIGGER account_evacuation_fence BEFORE INSERT OR UPDATE OR DELETE ON sync_bundle_operations
    FOR EACH ROW EXECUTE FUNCTION witself_tenant_evacuation_fence();
CREATE TRIGGER account_evacuation_fence BEFORE INSERT OR UPDATE OR DELETE ON sync_publication_outbox
    FOR EACH ROW EXECUTE FUNCTION witself_tenant_evacuation_fence();
CREATE TRIGGER account_evacuation_fence BEFORE INSERT OR UPDATE OR DELETE ON sync_acceptance_counter
    FOR EACH ROW EXECUTE FUNCTION witself_tenant_evacuation_fence();
CREATE TRIGGER account_evacuation_fence BEFORE INSERT OR UPDATE OR DELETE ON sync_resource_budgets
    FOR EACH ROW EXECUTE FUNCTION witself_tenant_evacuation_fence();
CREATE TRIGGER account_evacuation_fence BEFORE INSERT OR UPDATE OR DELETE ON sync_resource_reservations
    FOR EACH ROW EXECUTE FUNCTION witself_tenant_evacuation_fence();
CREATE TRIGGER account_evacuation_fence BEFORE INSERT OR UPDATE OR DELETE ON sync_deletion_knowledge
    FOR EACH ROW EXECUTE FUNCTION witself_tenant_evacuation_fence();
CREATE TRIGGER account_evacuation_fence BEFORE INSERT OR UPDATE OR DELETE ON sync_retained_evidence
    FOR EACH ROW EXECUTE FUNCTION witself_tenant_evacuation_fence();
CREATE TRIGGER account_evacuation_fence BEFORE INSERT OR UPDATE OR DELETE ON sync_evidence_pins
    FOR EACH ROW EXECUTE FUNCTION witself_tenant_evacuation_fence();
CREATE TRIGGER account_evacuation_fence BEFORE INSERT OR UPDATE OR DELETE ON sync_payload_retirements
    FOR EACH ROW EXECUTE FUNCTION witself_tenant_evacuation_fence();

-- +goose Down
-- Namespace downgrade can erase replay-prevention evidence. Always refuse.
-- +goose StatementBegin
DO $$ BEGIN
    RAISE EXCEPTION 'sync schema downgrade is forbidden' USING ERRCODE='23514';
END $$;
-- +goose StatementEnd
