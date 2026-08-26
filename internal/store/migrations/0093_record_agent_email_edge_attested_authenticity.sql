-- +goose Up
-- Schema 0093: let the Cloudflare receive integration store the edge-attested
-- SPF/DKIM/DMARC verdicts carried by the signed v2 relay envelope. The
-- per-column vocabulary CHECKs from 0059 are unchanged; only the compound
-- provider-posture clause pinning the three verdicts to 'unknown' is relaxed.
-- provider_message_id stays absent, spam_verdict stays 'unknown' (the
-- provider exposes none), and sender_verification_state stays 'unverified':
-- attested verdicts are advisory domain-granularity evidence, never sender
-- authentication. The replacement is added NOT VALID and validated inside
-- this same migration so no snapshot ever holds an unvalidated constraint
-- (the offline backup verifier fails otherwise) and the ACCESS EXCLUSIVE
-- window stays bounded.

-- +goose StatementBegin
DO $$
DECLARE constraint_name text;
BEGIN
  FOR constraint_name IN
    SELECT con.conname
      FROM pg_constraint con
     WHERE con.conrelid = 'agent_email_messages'::regclass
       AND con.contype = 'c'
       AND pg_get_constraintdef(con.oid) LIKE '%cloudflare_email_routing%'
  LOOP
    EXECUTE format(
      'ALTER TABLE agent_email_messages DROP CONSTRAINT %I',
      constraint_name
    );
  END LOOP;
END
$$;
-- +goose StatementEnd

ALTER TABLE agent_email_messages
  ADD CONSTRAINT agent_email_messages_pilot_provider_posture_check CHECK (
    provider <> 'cloudflare_email_routing' OR
    (provider_message_id IS NULL AND
     spf_result IS NOT NULL AND
     dkim_result IS NOT NULL AND
     dmarc_result IS NOT NULL AND
     spam_verdict IS NOT DISTINCT FROM 'unknown' AND
     sender_verification_state = 'unverified')
  ) NOT VALID;

ALTER TABLE agent_email_messages
  VALIDATE CONSTRAINT agent_email_messages_pilot_provider_posture_check;

-- +goose Down
-- Rows carrying real attested verdicts cannot re-enter the strict 0059
-- posture without destroying recorded evidence; refuse instead of clamping.
-- +goose StatementBegin
DO $$
BEGIN
  IF EXISTS (
    SELECT 1 FROM agent_email_messages
     WHERE provider = 'cloudflare_email_routing'
       AND (spf_result IS DISTINCT FROM 'unknown' OR
            dkim_result IS DISTINCT FROM 'unknown' OR
            dmarc_result IS DISTINCT FROM 'unknown')
  ) THEN
    RAISE EXCEPTION
      'cannot downgrade schema 0093 while edge-attested verdict rows exist';
  END IF;
END;
$$;
-- +goose StatementEnd

ALTER TABLE agent_email_messages
  DROP CONSTRAINT agent_email_messages_pilot_provider_posture_check;

ALTER TABLE agent_email_messages
  ADD CONSTRAINT agent_email_messages_pilot_provider_posture_check CHECK (
    provider <> 'cloudflare_email_routing' OR
    (provider_message_id IS NULL AND
     spf_result IS NOT DISTINCT FROM 'unknown' AND
     dkim_result IS NOT DISTINCT FROM 'unknown' AND
     dmarc_result IS NOT DISTINCT FROM 'unknown' AND
     spam_verdict IS NOT DISTINCT FROM 'unknown' AND
     sender_verification_state = 'unverified')
  ) NOT VALID;

ALTER TABLE agent_email_messages
  VALIDATE CONSTRAINT agent_email_messages_pilot_provider_posture_check;
