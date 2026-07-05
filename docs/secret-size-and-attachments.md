# Secret size and attachments

Status: draft. Decision: v0 stores normal secret fields inline (sealed plane,
envelope-encrypted in Postgres) and defers large attachments to an encrypted
object/blob path. Attachments are stubbed but not built in the first v0 slice.

Scope: this doc covers the SEALED PLANE only (secrets and their fields). Sealed
material is envelope-encrypted (CMK -> per-realm KEK -> per-secret/field DEK) and
is NEVER embedded, NEVER returned by semantic recall, NEVER in the self-digest,
NEVER ingested from CLAUDE.md/AGENTS.md, and NEVER in the plaintext export. The
size limits and attachment rules below inherit those carve-outs. Open-plane data
(memories, facts) has its own sizing and is out of scope here.

## Decision

V0 should optimize for credential-sized secrets, not arbitrary file storage.

Default v0 limits:

- Maximum sensitive field value size: 64 KiB before encoding.
- Maximum non-sensitive field value size: 16 KiB.
- Maximum total inline secret payload: 256 KiB before storage overhead.
- Maximum field count per secret: 100.
- Maximum secret name length: 255 characters.
- Maximum description length: 4 KiB.

These limits should be enforced with deterministic `limit_exceeded` or
`usage_error` responses and should be visible through backend capabilities when
practical.

Sizes are measured on the cleartext value before encryption and encoding; the
envelope overhead (per-field DEK, nonce, AEAD tag for `XCHACHA20_POLY1305` /
`AES_256_GCM`) is storage overhead and is not counted against the inline budget.

## Attachments

Attachments are not required for the first v0 slice. They are reserved here so
the data model and reference forms do not have to change later: attachment rows
carry the `att_` ID prefix and live alongside the secret they belong to.

When attachments are added, they should use encrypted object/blob storage rather
than large ordinary database columns. Good candidates include:

- Certificates and chains that exceed inline limits.
- Support diagnostic bundles.
- Encrypted import/export artifacts.
- Oversized encrypted blobs.
- Future file-like secret material.

Attachment rules:

- Attachments are sealed-plane material: they are encrypted under the same
  envelope hierarchy as secret fields (per-realm KEK -> per-attachment DEK) and
  carry the same carve-outs (never embedded, recalled, in the self-digest, or in
  the plaintext export).
- Attachment metadata (`att_` id, owner, content type, size, checksum, KEK/DEK
  key identity) can live in Postgres; the ciphertext lives in object/blob storage.
- Object/blob storage should not become a default dependency for small secrets;
  it is enabled only when the attachment path is in use.
- Attachment ownership follows the secret: `owner_kind` is `agent` or `group`
  (group-owned attachments are the consolidated form of the former vault-shared
  material).
- Attachment reveal/download must be explicit, authorized, and audited — the same
  reveal ceremony as `witself secret reveal`, gated by `secret:reveal`, suppressed
  when MCP runs with `--no-value-tools`.
- Support bundles must redact secret material by default; encrypted attachment
  ciphertext is never decrypted into a diagnostic bundle.

## Related docs

- [requirements.md](requirements.md)
- [secret-model.md](secret-model.md)
- [data-model.md](data-model.md)
- [storage.md](storage.md)
- [api-contract.md](api-contract.md)
- [json-contracts.md](json-contracts.md)
- [billing-and-limits.md](billing-and-limits.md)
- [backup-and-recovery.md](backup-and-recovery.md)
