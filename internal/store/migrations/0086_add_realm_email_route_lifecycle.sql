-- +goose Up
-- The canonical realm-id email route is controlled outside the cell, but the
-- realm is the portable owner of its lifecycle fence.  Keeping the compact
-- state here prevents a control-plane recovery scan from resurrecting a route
-- whose realm is closing or already retired.
ALTER TABLE realms
    ADD COLUMN email_route_state TEXT,
    ADD COLUMN email_route_generation BIGINT,
    ADD COLUMN email_route_operation_id TEXT;

-- Existing live realms become the first live generation.  Existing soft
-- deletes become terminal tombstones with a deterministic legacy operation so
-- an archive/restore can never make their canonical routes look live again.
UPDATE realms
   SET email_route_state = CASE
         WHEN deleted_at IS NULL THEN 'live'
         ELSE 'retired'
       END,
       email_route_generation = CASE
         WHEN deleted_at IS NULL THEN 1
         ELSE 2
       END,
       email_route_operation_id = CASE
         WHEN deleted_at IS NULL THEN NULL
         ELSE 'legacy_delete'
       END;

ALTER TABLE realms
    ALTER COLUMN email_route_state SET DEFAULT 'live',
    ALTER COLUMN email_route_state SET NOT NULL,
    ALTER COLUMN email_route_generation SET DEFAULT 1,
    ALTER COLUMN email_route_generation SET NOT NULL,
    ADD CONSTRAINT realms_email_route_state_check
      CHECK (email_route_state IN ('live', 'closing', 'retired')),
    ADD CONSTRAINT realms_email_route_generation_check
      CHECK (email_route_generation BETWEEN 1 AND 4611686018427387903),
    ADD CONSTRAINT realms_email_route_operation_id_check
      CHECK (
        email_route_operation_id IS NULL OR
        (octet_length(email_route_operation_id) BETWEEN 1 AND 128 AND
         email_route_operation_id ~ '^[A-Za-z0-9._:-]+$')
      ),
    ADD CONSTRAINT realms_email_route_lifecycle_shape_check
      CHECK (
        (email_route_state = 'live' AND
         email_route_generation >= 1 AND
         email_route_operation_id IS NULL AND
         deleted_at IS NULL)
        OR
        (email_route_state = 'closing' AND
         email_route_generation >= 2 AND
         email_route_operation_id IS NOT NULL AND
         deleted_at IS NULL)
        OR
        (email_route_state = 'retired' AND
         email_route_generation >= 2 AND
         email_route_operation_id IS NOT NULL AND
         deleted_at IS NOT NULL)
      );

-- +goose Down
-- Do not abandon an in-flight external retirement operation.  Live and
-- retired rows remain representable by schema 0085 (deleted_at is the latter's
-- durable tombstone), but closing has no safe older-schema representation.
-- +goose StatementBegin
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM realms WHERE email_route_state = 'closing') THEN
        RAISE EXCEPTION
            'cannot downgrade schema 0086 while realm email routes are closing';
    END IF;
END;
$$;
-- +goose StatementEnd

ALTER TABLE realms
    DROP CONSTRAINT realms_email_route_lifecycle_shape_check,
    DROP CONSTRAINT realms_email_route_operation_id_check,
    DROP CONSTRAINT realms_email_route_generation_check,
    DROP CONSTRAINT realms_email_route_state_check,
    DROP COLUMN email_route_operation_id,
    DROP COLUMN email_route_generation,
    DROP COLUMN email_route_state;
