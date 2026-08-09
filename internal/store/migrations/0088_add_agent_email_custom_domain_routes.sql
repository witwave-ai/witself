-- +goose Up
-- A custom-domain route is the cell-local, fenced projection of two pieces of
-- global authority: one verified account domain allocation and one immutable
-- realm-alias claim. The composite primary key permits one verified domain to
-- route multiple realm aliases while UNIQUE(domain, realm_label) prevents two
-- projections from ever competing for the same SMTP namespace.
CREATE TABLE agent_email_custom_domain_routes (
    domain_request_id          TEXT        NOT NULL,
    realm_alias_claim_id       TEXT        NOT NULL,
    account_id                 TEXT        NOT NULL,
    realm_id                   TEXT        NOT NULL,
    domain                     TEXT        NOT NULL,
    realm_label                TEXT        NOT NULL,
    domain_allocation_revision BIGINT      NOT NULL,
    domain_state_revision      BIGINT      NOT NULL,
    realm_alias_revision       BIGINT      NOT NULL,
    state                      TEXT        NOT NULL,
    suspension_disposition     TEXT,
    controller_revision        BIGINT      NOT NULL,
    created_at                 TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    updated_at                 TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    suspended_at               TIMESTAMPTZ,
    retired_at                 TIMESTAMPTZ,
    PRIMARY KEY (domain_request_id, realm_alias_claim_id),
    UNIQUE (domain, realm_label),
    UNIQUE (account_id, realm_id, domain_request_id,
            realm_alias_claim_id, realm_label),
    FOREIGN KEY (account_id, realm_id, realm_alias_claim_id, realm_label)
      REFERENCES agent_email_realm_aliases
        (account_id, realm_id, claim_id, realm_label),
    CHECK (domain_request_id ~ '^aedr_[a-z2-7]{16}$'),
    CHECK (realm_alias_claim_id ~ '^era_[a-z2-7]{16}$'),
    CHECK (octet_length(domain) BETWEEN 1 AND 253 AND domain = lower(domain)),
    CHECK (domain ~ '^[a-z0-9]([a-z0-9.-]{0,251}[a-z0-9])?$' AND
           position('..' IN domain) = 0),
    CONSTRAINT agent_email_custom_domain_routes_label_check CHECK (
      octet_length(realm_label) BETWEEN 3 AND 16 AND
      realm_label = lower(realm_label) AND
      realm_label ~ '^[a-z0-9][a-z0-9-]*[a-z0-9]$' AND
      position('--' IN realm_label) = 0 AND
      realm_label !~ '^xn--' AND
      realm_label !~ '^[a-z2-7]{16}$'
    ),
    CHECK (domain_allocation_revision BETWEEN 1 AND 4611686018427387903),
    CHECK (domain_state_revision BETWEEN 1 AND 4611686018427387903),
    CHECK (realm_alias_revision BETWEEN 1 AND 4611686018427387903),
    CHECK (controller_revision BETWEEN 1 AND 4611686018427387903),
    CHECK (state IN ('applied', 'suspended', 'retired')),
    CHECK (suspension_disposition IS NULL OR
           suspension_disposition IN ('retry', 'inactive')),
    CHECK (updated_at >= created_at),
    CHECK (suspended_at IS NULL OR suspended_at >= created_at),
    CHECK (retired_at IS NULL OR retired_at >= created_at),
    CONSTRAINT agent_email_custom_domain_routes_state_shape CHECK (
      (state = 'applied' AND suspension_disposition IS NULL AND
       suspended_at IS NULL AND retired_at IS NULL) OR
      (state = 'suspended' AND suspension_disposition IS NOT NULL AND
       suspended_at IS NOT NULL AND retired_at IS NULL) OR
      (state = 'retired' AND suspension_disposition IS NULL AND
       retired_at IS NOT NULL)
    )
);

CREATE INDEX agent_email_custom_domain_routes_by_owner
    ON agent_email_custom_domain_routes
       (account_id, realm_id, state, domain, realm_label);

-- Realm aliases and customer-domain routes share one SMTP namespace even
-- though their lifecycle authorities live in separate tables. PostgreSQL
-- cannot express cross-table uniqueness with an ordinary UNIQUE constraint,
-- so both insert paths take the same transaction-scoped advisory lock before
-- proving that the opposite table is empty. This also protects archive import,
-- which deliberately restores rows directly rather than calling store methods.
-- The Go projection paths take this exact lock as well so they can return their
-- stable typed conflict before attempting an insert.
-- +goose StatementBegin
CREATE FUNCTION witself_agent_email_route_namespace_fence()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
    PERFORM pg_advisory_xact_lock(hashtextextended(
        'witself:agent-email:route-namespace:' || NEW.domain || ':' || NEW.realm_label,
        0
    ));

    IF TG_TABLE_NAME = 'agent_email_realm_aliases' THEN
        IF EXISTS (
            SELECT 1
              FROM agent_email_custom_domain_routes
             WHERE domain = NEW.domain AND realm_label = NEW.realm_label
        ) THEN
            RAISE EXCEPTION USING
                ERRCODE = '23505',
                MESSAGE = 'agent email route namespace is already reserved';
        END IF;
    ELSIF TG_TABLE_NAME = 'agent_email_custom_domain_routes' THEN
        IF EXISTS (
            SELECT 1
              FROM agent_email_realm_aliases
             WHERE domain = NEW.domain AND realm_label = NEW.realm_label
        ) THEN
            RAISE EXCEPTION USING
                ERRCODE = '23505',
                MESSAGE = 'agent email route namespace is already reserved';
        END IF;
        -- One domain request may bind many aliases, but its account and domain
        -- are immutable. The application takes this lock before its friendly
        -- precheck; retain the same fence for direct archive inserts.
        PERFORM pg_advisory_xact_lock(hashtextextended(
            'witself:agent-email:custom-domain-request:' || NEW.domain_request_id,
            0
        ));
        IF EXISTS (
            SELECT 1
              FROM agent_email_custom_domain_routes
             WHERE domain_request_id = NEW.domain_request_id
               AND (account_id <> NEW.account_id OR domain <> NEW.domain)
        ) THEN
            RAISE EXCEPTION USING
                ERRCODE = '23505',
                MESSAGE = 'agent email custom-domain request identity conflicts';
        END IF;
    ELSE
        RAISE EXCEPTION 'unexpected agent email route namespace table';
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER agent_email_route_namespace_fence
BEFORE INSERT OR UPDATE OF domain, realm_label ON agent_email_realm_aliases
FOR EACH ROW EXECUTE FUNCTION witself_agent_email_route_namespace_fence();

CREATE TRIGGER agent_email_route_namespace_fence
BEFORE INSERT OR UPDATE ON agent_email_custom_domain_routes
FOR EACH ROW EXECUTE FUNCTION witself_agent_email_route_namespace_fence();

-- Migration 0070 attaches the evacuation barrier only to tables that existed
-- at that schema. Every later account-scoped authority table must attach the
-- same trigger explicitly.
CREATE TRIGGER account_evacuation_fence
BEFORE INSERT OR UPDATE OR DELETE ON agent_email_custom_domain_routes
FOR EACH ROW EXECUTE FUNCTION witself_tenant_evacuation_fence();

-- Preserve the exact custom-domain authority used for every immutable
-- delivery. The existing alias claim id remains present as the other half of
-- the route identity; neither value is inferred during archive restore.
ALTER TABLE agent_email_messages
    ADD COLUMN recipient_custom_domain_request_id TEXT;

ALTER TABLE agent_email_messages
    DROP CONSTRAINT agent_email_messages_recipient_route_check,
    ADD CONSTRAINT agent_email_messages_custom_domain_route_fk
      FOREIGN KEY (account_id, realm_id,
                   recipient_custom_domain_request_id,
                   recipient_realm_alias_claim_id, realm_label)
      REFERENCES agent_email_custom_domain_routes
        (account_id, realm_id, domain_request_id,
         realm_alias_claim_id, realm_label)
      NOT VALID,
    ADD CONSTRAINT agent_email_messages_recipient_route_check CHECK (
      (recipient_route_kind = 'canonical' AND
       recipient_realm_alias_claim_id IS NULL AND
       recipient_custom_domain_request_id IS NULL AND
       realm_label ~ '^[a-z2-7]{16}$')
      OR
      (recipient_route_kind = 'realm_alias' AND
       recipient_realm_alias_claim_id IS NOT NULL AND
       recipient_custom_domain_request_id IS NULL AND
       octet_length(realm_label) BETWEEN 3 AND 16 AND
       realm_label = lower(realm_label) AND
       realm_label ~ '^[a-z0-9][a-z0-9-]*[a-z0-9]$' AND
       position('--' IN realm_label) = 0 AND
       realm_label !~ '^xn--' AND
       realm_label !~ '^[a-z2-7]{16}$')
      OR
      (recipient_route_kind = 'custom_domain' AND
       recipient_realm_alias_claim_id IS NOT NULL AND
       recipient_custom_domain_request_id IS NOT NULL AND
       octet_length(realm_label) BETWEEN 3 AND 16 AND
       realm_label = lower(realm_label) AND
       realm_label ~ '^[a-z0-9][a-z0-9-]*[a-z0-9]$' AND
       position('--' IN realm_label) = 0 AND
       realm_label !~ '^xn--' AND
       realm_label !~ '^[a-z2-7]{16}$')
    ) NOT VALID;

ALTER TABLE agent_email_messages
    VALIDATE CONSTRAINT agent_email_messages_custom_domain_route_fk;
ALTER TABLE agent_email_messages
    VALIDATE CONSTRAINT agent_email_messages_recipient_route_check;

-- +goose Down
-- Schema 0087 cannot represent either custom-domain routing authority or the
-- exact provenance of a delivery accepted through it. Retired route rows are
-- permanent identity tombstones too, so refuse to discard any lifecycle.
LOCK TABLE agent_email_realm_aliases,
           agent_email_custom_domain_routes,
           agent_email_messages
    IN ACCESS EXCLUSIVE MODE;

-- +goose StatementBegin
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM agent_email_custom_domain_routes) THEN
        RAISE EXCEPTION
            'cannot downgrade schema 0088 while custom-domain email routes exist';
    END IF;
    IF EXISTS (
        SELECT 1 FROM agent_email_messages
         WHERE recipient_route_kind = 'custom_domain'
    ) THEN
        RAISE EXCEPTION
            'cannot downgrade schema 0088 while custom-domain email messages exist';
    END IF;
END;
$$;
-- +goose StatementEnd

ALTER TABLE agent_email_messages
    DROP CONSTRAINT agent_email_messages_recipient_route_check,
    DROP CONSTRAINT agent_email_messages_custom_domain_route_fk,
    DROP COLUMN recipient_custom_domain_request_id;

ALTER TABLE agent_email_messages
    ADD CONSTRAINT agent_email_messages_recipient_route_check CHECK (
      (recipient_route_kind = 'canonical' AND
       recipient_realm_alias_claim_id IS NULL AND
       realm_label ~ '^[a-z2-7]{16}$')
      OR
      (recipient_route_kind = 'realm_alias' AND
       recipient_realm_alias_claim_id IS NOT NULL AND
       octet_length(realm_label) BETWEEN 3 AND 16 AND
       realm_label = lower(realm_label) AND
       realm_label ~ '^[a-z0-9][a-z0-9-]*[a-z0-9]$' AND
       position('--' IN realm_label) = 0 AND
       realm_label !~ '^xn--' AND
       realm_label !~ '^[a-z2-7]{16}$')
    );

DROP TRIGGER agent_email_route_namespace_fence
    ON agent_email_custom_domain_routes;
DROP TRIGGER agent_email_route_namespace_fence
    ON agent_email_realm_aliases;
DROP FUNCTION witself_agent_email_route_namespace_fence();

DROP TABLE agent_email_custom_domain_routes;
