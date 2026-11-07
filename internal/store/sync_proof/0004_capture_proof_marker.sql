-- Proof-only. Explicitly installed by the disposable capture proof driver.
-- Excluded from archives, covered writers, and ordinary sync migrations.
CREATE TABLE sync_capture_proof_marker (
    token text NOT NULL,
    expires_at timestamptz NOT NULL
);
ALTER TABLE sync_capture_proof_marker OWNER TO witself_sync_owner;
REVOKE ALL ON sync_capture_proof_marker FROM PUBLIC;
GRANT SELECT ON sync_capture_proof_marker TO witself_sync_server, witself_sync_worker;
