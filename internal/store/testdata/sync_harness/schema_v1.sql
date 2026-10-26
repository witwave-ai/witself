-- LC-S5A synthetic fixture version 1. Explicitly loaded by opted-in tests only.
-- This is not a migration, capture journal, receipt implementation or sync API.
-- Later slices may extend these miniature tables with separately reviewed rules.
CREATE TABLE h_fixture_objects (
    account_id text NOT NULL REFERENCES accounts(id),
    object_id text NOT NULL,
    owner_node text NOT NULL,
    value text NOT NULL,
    PRIMARY KEY (account_id, object_id)
);
CREATE TABLE h_receipts (
    account_id text NOT NULL REFERENCES accounts(id),
    operation_id text NOT NULL,
    source_node text NOT NULL,
    PRIMARY KEY (account_id, operation_id)
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
