-- +goose Up
-- One canonical mailbox can be reached through more than one managed domain
-- during a domain transition.  Keep agent_email_addresses as the immutable
-- original reservation so older binaries and archives remain readable, and
-- add a permanent route reservation for every accepted domain.  A route row
-- deliberately has no retirement column: its parent address tombstone owns
-- lifecycle, and UNIQUE(domain,local_part) prevents reuse forever.
ALTER TABLE agent_email_addresses
    ADD CONSTRAINT agent_email_addresses_domain_route_scope_unique
      UNIQUE (account_id, realm_id, provisioned_agent_id, id, local_part);

CREATE TABLE agent_email_address_domains (
    account_id           TEXT        NOT NULL,
    realm_id             TEXT        NOT NULL,
    provisioned_agent_id TEXT        NOT NULL,
    address_id           TEXT        NOT NULL,
    domain               TEXT        NOT NULL,
    local_part           TEXT        NOT NULL,
    created_at           TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY (address_id, domain),
    UNIQUE (account_id, realm_id, address_id, domain),
    UNIQUE (domain, local_part),
    FOREIGN KEY (account_id, realm_id, provisioned_agent_id, address_id, local_part)
      REFERENCES agent_email_addresses
        (account_id, realm_id, provisioned_agent_id, id, local_part)
      ON DELETE CASCADE,
    CHECK (octet_length(domain) BETWEEN 1 AND 253 AND domain = lower(domain)),
    CHECK (domain ~ '^[a-z0-9]([a-z0-9.-]{0,251}[a-z0-9])?$' AND
           position('..' IN domain) = 0),
    CHECK (octet_length(local_part) BETWEEN 1 AND 64)
);

-- Every pre-0087 address starts with its original domain as a routable
-- reservation.  The upgraded server adds its configured primary during normal
-- startup reconciliation while retaining that immutable original route. Merely
-- configuring a legacy domain never issues it to a post-cutover mailbox.
INSERT INTO agent_email_address_domains
    (account_id,realm_id,provisioned_agent_id,address_id,domain,local_part,created_at)
SELECT account_id,realm_id,provisioned_agent_id,id,domain,local_part,created_at
  FROM agent_email_addresses;

CREATE INDEX agent_email_address_domains_by_owner
    ON agent_email_address_domains
       (account_id, realm_id, provisioned_agent_id, address_id, domain);

CREATE TRIGGER account_evacuation_fence
BEFORE INSERT OR UPDATE OR DELETE ON agent_email_address_domains
FOR EACH ROW EXECUTE FUNCTION witself_tenant_evacuation_fence();

-- +goose Down
-- Schema 0086 can represent only the original address domain.  Refuse to
-- discard an additive reservation because doing so could make a historical
-- address reusable or silently stop a still-advertised legacy route.
-- Lock the parent before the route table, matching Ensure's parent-read then
-- route-insert order.  The locks cover both the guard and the later DROP, so a
-- concurrent writer cannot add a route after the check or deadlock the down.
LOCK TABLE agent_email_addresses, agent_email_address_domains
    IN ACCESS EXCLUSIVE MODE;

-- +goose StatementBegin
DO $$
BEGIN
    IF EXISTS (
        SELECT 1
          FROM agent_email_address_domains route
          JOIN agent_email_addresses address ON address.id=route.address_id
         WHERE route.domain <> address.domain
    ) THEN
        RAISE EXCEPTION
            'cannot downgrade schema 0087 while additive agent-email domains exist';
    END IF;
END;
$$;
-- +goose StatementEnd

DROP TABLE agent_email_address_domains;

ALTER TABLE agent_email_addresses
    DROP CONSTRAINT agent_email_addresses_domain_route_scope_unique;
