-- LC-S5A/B synthetic fixture version 1. Explicitly loaded by opted-in tests only.
-- Harness admission/receipt storage: not a migration, capture journal or sync API.
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
    operation_id text NOT NULL,
    PRIMARY KEY (account_id, operation_id)
);
CREATE TABLE h_rollups (
    account_id text NOT NULL REFERENCES accounts(id),
    bucket text NOT NULL,
    count bigint NOT NULL CHECK (count >= 0),
    PRIMARY KEY (account_id, bucket)
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
-- mutation is intentionally not implemented in B.
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
