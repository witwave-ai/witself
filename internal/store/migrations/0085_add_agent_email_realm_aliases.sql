-- +goose Up
-- A realm alias is an additive routing designator applied by the globally
-- authoritative control plane. Canonical realm-id-body addresses remain in
-- agent_email_addresses and are never replaced. Retired rows stay here as
-- tombstones so a label cannot be reused for a different claim.
CREATE TABLE agent_email_realm_aliases (
    claim_id            TEXT        PRIMARY KEY,
    account_id          TEXT        NOT NULL,
    realm_id            TEXT        NOT NULL,
    domain              TEXT        NOT NULL,
    realm_label         TEXT        NOT NULL,
    state               TEXT        NOT NULL,
    controller_revision BIGINT      NOT NULL,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    suspended_at        TIMESTAMPTZ,
    retired_at          TIMESTAMPTZ,
    UNIQUE (account_id, realm_id, claim_id),
    UNIQUE (account_id, realm_id, claim_id, realm_label),
    UNIQUE (domain, realm_label),
    FOREIGN KEY (account_id, realm_id)
      REFERENCES realms (account_id, id) ON DELETE CASCADE,
    CHECK (claim_id ~ '^era_[a-z2-7]{16}$'),
    CHECK (octet_length(domain) BETWEEN 1 AND 253 AND domain = lower(domain)),
    CHECK (domain ~ '^[a-z0-9]([a-z0-9.-]{0,251}[a-z0-9])?$' AND
           position('..' IN domain) = 0),
    CONSTRAINT agent_email_realm_aliases_label_check CHECK (
      octet_length(realm_label) BETWEEN 3 AND 16 AND
      realm_label = lower(realm_label) AND
      realm_label ~ '^[a-z0-9][a-z0-9-]*[a-z0-9]$' AND
      position('--' IN realm_label) = 0 AND
      realm_label !~ '^xn--' AND
      realm_label !~ '^[a-z2-7]{16}$'
    ),
    CHECK (state IN ('applied', 'suspended', 'retired')),
    CHECK (controller_revision BETWEEN 1 AND 4611686018427387903),
    CHECK (updated_at >= created_at),
    CHECK (suspended_at IS NULL OR suspended_at >= created_at),
    CHECK (retired_at IS NULL OR retired_at >= created_at),
    CONSTRAINT agent_email_realm_aliases_state_shape CHECK (
      (state = 'applied' AND suspended_at IS NULL AND retired_at IS NULL) OR
      (state = 'suspended' AND suspended_at IS NOT NULL AND retired_at IS NULL) OR
      (state = 'retired' AND retired_at IS NOT NULL)
    )
);

CREATE INDEX agent_email_realm_aliases_by_realm
    ON agent_email_realm_aliases
       (account_id, realm_id, state, realm_label);

-- Migration 0070 attached the evacuation barrier to every tenant table that
-- existed at that point in the schema. Additive account-scoped tables must
-- attach it themselves so a source export/finalization cannot race a later
-- alias projection.
CREATE TRIGGER account_evacuation_fence
BEFORE INSERT OR UPDATE OR DELETE ON agent_email_realm_aliases
FOR EACH ROW EXECUTE FUNCTION witself_tenant_evacuation_fence();

-- Keep the exact received route on each immutable message. address_id still
-- points at the canonical mailbox reservation; these fields distinguish a
-- canonical delivery from a delivery accepted through an additive alias.
ALTER TABLE agent_email_messages
    ADD COLUMN recipient_route_kind TEXT NOT NULL DEFAULT 'canonical',
    ADD COLUMN recipient_realm_alias_claim_id TEXT;

ALTER TABLE agent_email_messages
    ADD CONSTRAINT agent_email_messages_realm_alias_fk
      FOREIGN KEY (account_id, realm_id,
                   recipient_realm_alias_claim_id, realm_label)
      REFERENCES agent_email_realm_aliases
        (account_id, realm_id, claim_id, realm_label)
      NOT VALID,
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
    ) NOT VALID;

-- Validate under locks that do not block ordinary reads/writes for the whole
-- scan, then retire the now-redundant canonical-only check. New writes are
-- constrained from the moment each NOT VALID constraint is installed.
ALTER TABLE agent_email_messages
    VALIDATE CONSTRAINT agent_email_messages_realm_alias_fk;
ALTER TABLE agent_email_messages
    VALIDATE CONSTRAINT agent_email_messages_recipient_route_check;
ALTER TABLE agent_email_messages
    DROP CONSTRAINT agent_email_messages_realm_label_check;

-- +goose Down
-- Schema 0084 has no structured field for alias-route provenance. Rewriting
-- realm_label would make its exact envelope recipient fail schema-0084 archive
-- validation, while rewriting the envelope would destroy received history.
-- Refuse the downgrade before changing any schema or data instead. Lock in
-- routing-then-message order, matching ingestion, so neither a projection nor
-- a newly accepted alias delivery can race the guard.
LOCK TABLE agent_email_realm_aliases, agent_email_messages
    IN ACCESS EXCLUSIVE MODE;

-- +goose StatementBegin
DO $$
BEGIN
    IF EXISTS (
        SELECT 1
          FROM agent_email_messages
         WHERE recipient_route_kind = 'realm_alias'
    ) THEN
        RAISE EXCEPTION
            'cannot downgrade schema 0085 while realm-alias email messages exist';
    END IF;
END;
$$;
-- +goose StatementEnd

ALTER TABLE agent_email_messages
    DROP CONSTRAINT agent_email_messages_recipient_route_check,
    DROP CONSTRAINT agent_email_messages_realm_alias_fk;

ALTER TABLE agent_email_messages
    DROP COLUMN recipient_realm_alias_claim_id,
    DROP COLUMN recipient_route_kind,
    ADD CHECK (realm_label ~ '^[a-z2-7]{16}$');

DROP INDEX agent_email_realm_aliases_by_realm;
DROP TABLE agent_email_realm_aliases;
