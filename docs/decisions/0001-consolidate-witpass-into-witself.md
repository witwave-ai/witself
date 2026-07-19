# ADR 0001: Consolidate Witpass into Witself

Status: accepted (2026-06-26). Supersedes the standalone Witpass product.

Sealed-custody amendment (2026-07-18): ADR 0003 supersedes the KMS-rooted key
hierarchy and server-mediated decrypt portions of this decision. Consolidation
into Witself, the two-plane model, structured secrets, and shared identity
remain accepted.

## Context

Witpass (agent secrets vault + authenticator) and Witself (agent self/identity store —
memories + facts + cross-agent policy + security groups + inter-agent messaging) were
designed as sibling products sharing one platform spine. A cross-reference of the two doc
sets found the shared spine aligned to near-verbatim, with all real differences confined to
the domain payload. Maintaining two standalone products meant duplicating that spine (the
source of every drift item found) for no offsetting benefit, while the products have strong
synergies (shared account, shared agent identity, cross-scheme references). Both projects
were still docs-only, making this the cheapest possible moment to merge.

## Decision

Consolidate Witpass into Witself as **one product, one CLI (`witself`), one backend, one
account + agent model**. The secrets/credentials capability becomes a **sealed plane** within
Witself, distinct from the **open plane** (memories + facts). Witpass is retired as a separate
product; its design content is folded into the Witself repo. (The `witpass` repo is paused and
left untouched; content was read-only and re-skinned into `witself`.)

### Two planes, one platform

- **Open plane** (memories, facts): plaintext at rest, semantically indexed (embeddings),
  recallable, cross-agent readable/curatable under the declarative policy engine, in the
  self-digest, plaintext-exportable, ingestible from CLAUDE.md/AGENTS.md.
- **Sealed plane** (secrets, TOTP): `CMK → per-realm KEK → per-secret/field DEK` envelope
  encryption, reveal-gated, hybrid `client_side_decrypt` / `server_side_decrypt` behind one
  capability switch.
- **Shared spine**: account/realm/agent model, token=identity, CLI/MCP/`witself-server` API
  adapters, one authorization layer (roles/scopes spanning both planes + the cross-agent
  identity policy engine for the open plane), audit, observability, billing, release.

### Naming & ownership unification

- `vault → realm`, `wp:// → witself://`, `WITPASS_ → WITSELF_`, `witself.v0`, `witself_at_`,
  binaries `witself` / `witself-server`, MCP `witself.*`.
- **All data (memory, fact, secret) is owned by an agent or a group.** Witpass's separate
  "vault-shared" scope is retired; shared secrets become **group-owned** (`owner_kind ∈
  {agent, group}` uniformly). `--shared` → `--group`; `wp://shared/…` →
  `witself://group/<group>/secret/…`.

### The five reconciliations (opposite postures made to coexist)

1. **Two-tier encryption** — open plane = ordinary data-at-rest (no KMS); sealed plane = KMS
   envelope. KMS is a conditional dependency, required only when the sealed plane is enabled.
2. **Export carve-out** — identity (memory + fact) is first-class plaintext export/import;
   secrets are **never** in the plaintext export (encrypted-only backup, never key material).
3. **Reveal + MCP `--no-value-tools`** return for the sealed plane only; the open plane has no
   reveal ceremony (memories/facts are plainly readable; sensitive facts use light redaction).
4. **Recall/embeddings/digest carve-out** — secrets and TOTP seeds are never embedded,
   recalled, in the self-digest, or ingested from instruction files.
5. **Dual threat model** — protects both confidentiality (secrets) and integrity/authenticity
   (identity) + PII.

## Consequences

- One product to adopt, bill, and operate; the secrets capability can be staged after the
  open-plane core (it carries the envelope/KMS dependency).
- The merge also filled genuine gaps in Witself: `data-model.md` (full Postgres schema, both
  planes) and `authorization-and-roles.md` (role/scope model) had no Witself equivalent.
- New docs: `data-model.md`, `encryption-model.md`, `key-hierarchy.md`,
  `authorization-and-roles.md`, `secret-model.md`, `totp-2fa.md`,
  `secret-size-and-attachments.md`. `storage-and-kms.md` merged into `storage.md`.
- The sealed plane keeps a clean module boundary so the credentials capability can be
  extracted to a focused, separately-auditable repo later if the security-buyer story needs it.
- Verified at merge: 44 docs (~27k lines), 1880 internal cross-links with 0 broken, 0 live
  Witpass leakage, both planes represented, the five carve-outs enforced.

## Alternatives considered

- **Keep two standalone products** — rejected: doubles spine maintenance (the drift source),
  with no benefit unless sold to genuinely separate buyers.
- **Dissolve secrets into the open data plane** (secrets as just another fact type) — rejected:
  the open plane's recall/embeddings/digest/export/cross-agent defaults would each become a
  secret-exfiltration path. The sealed-plane boundary is the safeguard.

## Related

- [requirements.md](../requirements.md) · [secret-model.md](../secret-model.md) ·
  [encryption-model.md](../encryption-model.md) · [key-hierarchy.md](../key-hierarchy.md) ·
  [authorization-and-roles.md](../authorization-and-roles.md) ·
  [data-model.md](../data-model.md) · [access-policy.md](../access-policy.md)
