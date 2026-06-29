# Witself JSON Contracts

Status: draft. This document defines the JSON contract for CLI `--json` output,
MCP tool results, managed API responses, self-hosted API responses, and local
development responses before implementation.

## Goals

- Give agents deterministic output that is safe to parse.
- Keep CLI, MCP, managed API, self-hosted API, and local development responses
  aligned.
- Make cross-agent and destructive identity mutations explicit and easy to
  audit.
- Keep open-plane identity data readable by default while redacting `sensitive`
  facts and `sensitive`-flagged memory content in list/scan responses.
- Make sealed-plane (secret / TOTP) reveal responses explicit and easy to audit,
  and keep sealed material redacted everywhere else.
- Prevent memory content, fact values, message bodies/payloads, embedding
  vectors, secret values, TOTP seeds, TOTP codes, and raw tokens from appearing
  in errors, logs, or audit records.
- Keep managed-service and self-hosted responses aligned while leaving room for
  a local mock/development backend.

Witself spans two planes. The **open plane** (memories + facts) protects the
*integrity and authenticity* of identity data: there is no reveal ceremony, an
authorized read of a single record returns its value directly, and the only
`sensitive` facts and `sensitive`-flagged memory content are redacted in
list/scan output as a PII/display posture, not an encryption boundary. The
**sealed plane** (secrets + TOTP) protects the *confidentiality* of secret
material: values are KMS-backed envelope-encrypted, redacted by default, and
returned only through the explicit, audited reveal / TOTP-code ceremony (see the
[Sealed-Plane Shapes](#sealed-plane-shapes)). Sealed material is never embedded,
recalled, in the self-digest, or plaintext-exported.

Once implementation starts, exact JSON Schemas should be generated from the Go
contract structs used by the shared core. This keeps CLI, MCP, managed API,
self-hosted API, and local development contracts aligned without
hand-maintaining parallel schema definitions.

## Response Envelope

All `--json` responses should use a stable envelope.

Successful response:

```json
{
  "schema_version": "witself.v0",
  "ok": true,
  "data": {},
  "warnings": []
}
```

Error response:

```json
{
  "schema_version": "witself.v0",
  "ok": false,
  "error": {
    "code": "access_denied",
    "message": "access denied",
    "retryable": false,
    "details": {}
  }
}
```

Rules:

- `schema_version` is required and is always `witself.v0` in v0.
- `ok` is required.
- Successful responses use `data`.
- Failed responses use `error`.
- `warnings` is optional, but should be an array when present. Recall responses
  use `warnings` to surface degraded semantic recall (see
  [Recall Result](#recall-result)). Write paths use `warnings` to surface
  dedup/supersede outcomes with the `memory_duplicate` and `memory_merged` codes
  (see [Mutation Result](#mutation-result) and [Remember Result](#remember-result)).
- `retryable` indicates whether retrying the identical request may later
  succeed. Transient codes (`backend_unavailable`, `rate_limited`) are
  `retryable: true`; hard conditions (`limit_exceeded`, `access_denied`,
  `auth_failed`, `not_found`, `conflict`, `unsupported_operation`) are
  `retryable: false`.
- `rate_limited` responses should include `details.retry_after` in seconds when
  a wait is known; the HTTP API should also send a `Retry-After` header.
- Memory content, fact values, message bodies/payloads, embedding vectors, raw
  tokens, secret values, TOTP seeds, TOTP codes, and wrapped key material must
  never appear in `error.message` or `error.details`.

## Error Codes

JSON error codes should align with CLI exit-code categories.

| Code | CLI Exit | Meaning |
|---|---:|---|
| `internal_error` | 1 | Unexpected internal error. |
| `usage_error` | 2 | Invalid command, flag, input, or request shape. |
| `access_denied` | 3 | Authenticated principal lacks permission, or no policy allows the cross-agent access. |
| `auth_failed` | 4 | Authentication or local unlock failed. |
| `not_found` | 5 | Memory, fact, policy, group, message, secret, field, grant, agent, realm, token, or event not found. |
| `conflict` | 6 | Already exists, stale version, fact name/primary collision, or other conflict. |
| `backend_unavailable` | 7 | Backend or network unavailable. |
| `rate_limited` | 7 | Transient service-protection or throttle limit; `retryable: true`, honor `retry_after`. |
| `limit_exceeded` | 7 | Plan, quota, or hard usage cap; `retryable: false`. |
| `store_integrity` | 8 | Local store integrity or corruption failure. |
| `unsupported_operation` | 9 | Current backend does not support the operation. |

CLI exit `7` collapses three distinct conditions (`backend_unavailable`,
`rate_limited`, `limit_exceeded`). Scripts that branch on retry behavior must
read `error.code` and `retryable` from `--json` output, not the exit code alone.

Cross-agent denials use `access_denied` and should include the deciding context
when practical, so callers can reconcile a result against `policy test` (see
[Policy](#policy)):

```json
{
  "code": "access_denied",
  "message": "no policy allows read on agent archivist",
  "retryable": false,
  "details": {
    "permission": "read",
    "scope": "memory",
    "target": {
      "kind": "agent",
      "agent_id": "agent_456",
      "agent_name": "archivist"
    },
    "decision": "deny",
    "policy_id": null
  }
}
```

`unsupported_operation` errors should include capability context when practical:

```json
{
  "code": "unsupported_operation",
  "message": "billing is not supported by this backend",
  "retryable": false,
  "details": {
    "feature": "billing",
    "backend_kind": "self-hosted",
    "reason": "not_configured"
  }
}
```

## Common Types

Identifiers:

- IDs are strings with stable prefixes: `realm_`, `agent_`, `opr_`, `mem_`,
  `fact_`, `grp_`, `pol_`, `msg_`, `tok_`, `aud_`, and the sealed-plane prefixes
  `acct_`, `sec_`, `fld_`, `grt_`, `totp_`, `kek_`, `dek_`, `att_`, `usg_`, and
  `idem_`.
- Names are user-visible strings.
- Local-file mode may generate stable local IDs, but callers should not parse ID
  internals.

Time:

- Timestamps should be RFC3339 UTC strings.
- Durations accepted from CLI flags may use Go-style strings such as `15m`, but
  JSON responses should include explicit timestamps or integer seconds.

Pagination:

```json
{
  "items": [],
  "next_cursor": null
}
```

Resource owner:

```json
{
  "owner": {
    "kind": "agent",
    "agent_id": "agent_123",
    "agent_name": "browser-agent"
  }
}
```

Group-owned resources use:

```json
{
  "owner": {
    "kind": "group",
    "group_id": "grp_123",
    "group_name": "shared-context"
  }
}
```

Resource-targeting inputs should use `owner_kind` when a caller can explicitly
target more than the current agent:

- `current`: the token-bound agent (the default).
- `agent`: a specific owning agent; requires `owner_agent` and is policy-gated.
- `group`: a group-owned scope; requires `owner_group`.

CLI maps this to the default target, `--owner-agent`, and `--owner-group`.

Identity reference:

```json
{
  "reference": "witself://agent/archivist/fact/email",
  "scheme": "witself",
  "kind": "fact",
  "owner_kind": "agent",
  "owner": "archivist",
  "leaf": "email",
  "valid": true
}
```

`witself://` references let memories, facts, messages, scripts, config files,
and MCP tools point at identity data without copying it. Reference shapes and
resolution rules are defined in
[Reference Parse and Resolve](#reference-parse-and-resolve).

## Principal Shape

Used by `whoami`, MCP session metadata, and audit records.

```json
{
  "principal": {
    "kind": "agent",
    "id": "agent_123",
    "name": "browser-agent",
    "realm_id": "realm_123",
    "realm_name": "default",
    "scopes": ["memory:create", "memory:read", "fact:read", "message:send"]
  }
}
```

Principal `kind` values:

- `agent`
- `operator`
- `admin`
- `service`

## Token Metadata

Used by token list/show responses. Raw token values are not included.

```json
{
  "id": "tok_123",
  "name": "browser-agent default",
  "principal_kind": "agent",
  "realm_id": "realm_123",
  "realm_name": "prod",
  "agent_id": "agent_123",
  "agent_name": "browser-agent",
  "scopes": ["memory:create", "memory:read", "fact:read", "message:send"],
  "created_at": "2026-06-26T18:00:00Z",
  "last_used_at": "2026-06-26T18:10:00Z",
  "expires_at": null,
  "revoked_at": null,
  "disabled_reason": null
}
```

Token create and rotate responses may include a raw token exactly once:

```json
{
  "token": "ws_at_example",
  "token_file": "./tokens/browser-agent.token",
  "metadata": {
    "id": "tok_123",
    "name": "browser-agent default",
    "principal_kind": "agent",
    "realm_id": "realm_123",
    "agent_id": "agent_123",
    "agent_name": "browser-agent",
    "expires_at": null
  }
}
```

Rules:

- Token files contain plain token text in v0.
- `expires_at: null` means no explicit expiration; v0 agent tokens do not expire
  automatically.
- Raw token values appear only in explicit token create or rotate results.
- Setup JSON should prefer `token_file` and avoid embedding raw token values.

The token lifecycle is tracked in [token-lifecycle.md](token-lifecycle.md).

## Capability Result

Used by `ws capabilities` and `/v1/capabilities`.

The HTTP `GET /v1/capabilities` response is **bare/flat**: a top-level
`schema_version` sits alongside `backend`, `principal`, `features`, and `limits`,
not the `ok`/`data` envelope. (Over CLI `--json` the same object is carried as the
`data` payload of the standard envelope.)

```json
{
  "schema_version": "witself.v0",
  "backend": {
    "kind": "self-hosted",
    "version": "v0.1.0",
    "api_version": "v1",
    "endpoint": "https://witself.internal.example.com"
  },
  "account": {
    "id": "acc_123"
  },
  "principal": {
    "kind": "operator",
    "id": "opr_123",
    "name": "scott",
    "realm_id": "realm_123",
    "realm_name": "prod",
    "scopes": ["realm:admin"]
  },
  "features": {
    "memories": {
      "supported": true
    },
    "facts": {
      "supported": true
    },
    "semantic_recall": {
      "supported": true,
      "degraded": false,
      "provider": "voyage",
      "model": "voyage-3",
      "dimensions": 1024
    },
    "policies": {
      "supported": true
    },
    "groups": {
      "supported": true
    },
    "messaging": {
      "supported": true
    },
    "audit": {
      "supported": true
    },
    "field_level_encryption": {
      "supported": false,
      "reason": "not_enabled"
    },
    "secrets": {
      "supported": true
    },
    "totp": {
      "supported": true
    },
    "client_side_decrypt": {
      "supported": false,
      "reason": "byok_post_v0"
    },
    "server_side_decrypt": {
      "supported": true,
      "kms_provider": "aws-kms"
    },
    "billing": {
      "supported": false,
      "reason": "not_configured"
    },
    "payments": {
      "supported": false,
      "reason": "not_configured"
    },
    "crypto_payments": {
      "supported": false,
      "reason": "provider_not_configured"
    },
    "support": {
      "supported": false,
      "reason": "managed_only"
    },
    "cross_realm_collaboration": {
      "supported": true
    },
    "federation": {
      "supported": true
    },
    "agent_card": {
      "supported": true
    },
    "multi_cell": {
      "supported": true
    }
  },
  "limits": {
    "active_agent": {
      "max": 30,
      "used": 12
    },
    "stored_memory": {
      "max": 100000,
      "used": 4821
    },
    "memory_recall": {
      "unit": "minute",
      "included": 1000,
      "used": 18,
      "soft_limit": 800,
      "hard_limit": 1000,
      "overage_behavior": "throttle"
    },
    "stored_secret": {
      "max": 10000,
      "used": 482
    },
    "secret_read": {
      "unit": "minute",
      "included": 1000,
      "used": 18,
      "soft_limit": 800,
      "hard_limit": 1000,
      "overage_behavior": "throttle"
    }
  }
}
```

Rules:

- `backend.kind` values are `managed`, `self-hosted`, and `local`. `backend.kind`
  is a configured value, not inferred: it comes from `WITSELF_BACKEND_KIND` and
  defaults to `self-hosted`; `managed` is set only by Witself's managed
  deployment, and `local` is reported by the CLI's local adapter and is never the
  server. `kind` is advisory — clients should branch on specific feature flags,
  and each feature is independently gated so a mislabeled kind unlocks nothing.
- `semantic_recall` reports the active embedding provider, model, and vector
  dimensionality. `degraded: true` means the provider is unavailable or disabled
  and recall has fallen back to keyword/tag/kind/time ranking (see
  [Recall Result](#recall-result)). The embedding-provider abstraction is
  tracked in [memory-model.md](memory-model.md).
- `field_level_encryption` reflects optional encryption of `sensitive` fact
  values; it is a capability, not the default (see [storage.md](storage.md)).
- `secrets` and `totp` advertise the **sealed plane** (secrets, TOTP). It is a
  defined v0 slice that may be staged after the open-plane core; an
  open-plane-only deployment reports `supported: false` with a stable `reason`
  (see [v0-scope.md](v0-scope.md)).
- `client_side_decrypt` and `server_side_decrypt` advertise the two sealed-plane
  custody modes (see [key-hierarchy.md](key-hierarchy.md)). Clients use them to
  pick the [Secret Reveal Result](#secret-reveal-result) shape they receive
  rather than probing. Per the v0 crypto subset, remote backends advertise
  `client_side_decrypt: false` and `server_side_decrypt: true`; client-held
  decrypt over the wire (BYOK) is post-v0. `server_side_decrypt` carries the
  active `kms_provider` (`aws-kms` | `gcp-kms` | `azure-key-vault` | `local-dev`)
  so callers can see which root key custody is in force.
- Capability responses never include secret values, TOTP seeds, TOTP codes,
  passphrases, private keys, key material, or wrapped key blobs. The sealed plane
  is never embedded, recalled, in the self-digest, or plaintext-exported.
- `limits` keys use the canonical metered-dimension names from
  [billing-and-limits.md](billing-and-limits.md) (e.g. `active_agent`,
  `stored_memory`, `memory_recall`), so they join directly to
  `/v1/billing/usage` items and the `limit_dimension` metric label.
- `cross_realm_collaboration`, `federation`, `agent_card`, and `multi_cell`
  advertise the cross-realm and multi-cloud capabilities:
  `cross_realm_collaboration` (conversations and cross-realm messaging via the
  relay), `federation` (the realm's accepted-peer allow-list / trust registry),
  `agent_card` (a signed [realm card](#realm-card) is published and served at
  `/.well-known/witself-card.json`), and `multi_cell` (managed placement and
  tenant migration across cells). The collaboration substrate is specified in
  [agent-collaboration.md](agent-collaboration.md) and the cell model in
  [deployment-cells.md](deployment-cells.md). A deployment that does not offer
  one of these reports `supported: false` with a stable `reason`.
- `features` values must include at least `supported`.
- Unsupported features should include a stable `reason` when known. In v0.0.x the
  `features` are reported as `{"supported": false, "reason": "not_implemented"}`
  until each subsystem ships.
- Capability responses must not include memory content, fact values, message
  bodies/payloads, embedding vectors, raw tokens, provider secrets, payment
  credentials, wallet credentials, or private infrastructure credentials.
- Clients should use capability data to present clear unsupported-operation
  errors instead of probing routes blindly.

## Memory Summary

Used by `memory list` and operator realm scans. Identity metadata is shown by
default; only `sensitive`-flagged content is withheld here.

```json
{
  "id": "mem_123",
  "kind": "episodic",
  "owner": {
    "kind": "agent",
    "agent_id": "agent_123",
    "agent_name": "browser-agent"
  },
  "preview": "Visited the staging console and noted the slow recall path...",
  "tags": ["staging", "performance"],
  "source": "self",
  "salience": 0.8,
  "link_count": 2,
  "sensitive": false,
  "version": 3,
  "archived": false,
  "created_at": "2026-06-26T18:00:00Z",
  "updated_at": "2026-06-26T18:05:00Z",
  "last_accessed_at": "2026-06-26T18:10:00Z"
}
```

Rules:

- `preview` is a short, truncated excerpt of `content` for ordinary
  (non-sensitive) memories.
- When `sensitive` is `true`, `preview` is omitted or set to `null` and the
  summary sets `redacted: true`. Full content is available only through an
  authorized `memory read` of that single record.
- `archived: true` marks a memory that has been forgotten (soft-deleted /
  tombstoned) and is restorable within the retention window.
- `kind` is a convention-driven label (for example `episodic`, `semantic`,
  `profile`, `note`); unknown kinds are allowed.
- `source` records authorship/provenance. Values are `self` (the owning agent
  authored it), `agent:<name>` (a cross-agent contribution), `operator`, and
  `import:<file>` (ingested from a CLAUDE.md/AGENTS.md/GEMINI.md file). The
  self-digest and `memory consolidate` use `source` to prioritize and to avoid
  silently overwriting human- or import-authored records.

The memory model and lifecycle are tracked in
[memory-model.md](memory-model.md).

## Memory Detail

Used by `memory read`/`memory show`. An authorized read returns full content
directly, including content of `sensitive` memories.

```json
{
  "id": "mem_123",
  "kind": "episodic",
  "owner": {
    "kind": "agent",
    "agent_id": "agent_123",
    "agent_name": "browser-agent"
  },
  "realm_id": "realm_123",
  "content": "Visited the staging console and noted the slow recall path on cold start.",
  "content_encoding": "plain",
  "tags": ["staging", "performance"],
  "source": "self",
  "salience": 0.8,
  "links": [
    "witself://memory/mem_777",
    "witself://fact/home-region"
  ],
  "sensitive": false,
  "version": 3,
  "archived": false,
  "history": [
    {
      "version": 1,
      "action": "added",
      "actor": {
        "kind": "agent",
        "id": "agent_123",
        "name": "browser-agent"
      },
      "changed_fields": ["content", "kind", "tags"],
      "timestamp": "2026-06-26T18:00:00Z"
    },
    {
      "version": 3,
      "action": "adjusted",
      "actor": {
        "kind": "agent",
        "id": "agent_123",
        "name": "browser-agent"
      },
      "changed_fields": ["content", "salience"],
      "timestamp": "2026-06-26T18:05:00Z"
    }
  ],
  "created_at": "2026-06-26T18:00:00Z",
  "updated_at": "2026-06-26T18:05:00Z",
  "last_accessed_at": "2026-06-26T18:10:00Z"
}
```

Rules:

- `content` is returned in clear for an authorized read, including for
  `sensitive` memories. There is no reveal ceremony.
- Binary-safe content should use `content_encoding: "base64"`.
- `links` are `witself://` references resolvable through authorized commands or
  MCP tools (see [Reference Parse and Resolve](#reference-parse-and-resolve)).
- `history` lists versioned edits in ascending version order. History entries
  carry `changed_fields` and the acting principal, never the prior values
  themselves beyond what `content` already exposes. History is included in
  identity export and inherits the memory's `sensitive` posture.
- A cross-agent read requires a policy granting `read` on the target and is
  metered as a cross-agent access (see [Policy](#policy)).
- Reading updates `last_accessed_at`.

## Recall Result

Used by `memory recall` and `/v1/memories:recall`. Returns ranked hits with
per-hit scores. Recall is semantic by default.

```json
{
  "query": "slow recall on cold start",
  "mode": "semantic",
  "degraded": false,
  "hits": [
    {
      "memory": {
        "id": "mem_123",
        "kind": "episodic",
        "owner": {
          "kind": "agent",
          "agent_id": "agent_123",
          "agent_name": "browser-agent"
        },
        "preview": "Visited the staging console and noted the slow recall path...",
        "tags": ["staging", "performance"],
        "source": "self",
        "salience": 0.8,
        "sensitive": false,
        "created_at": "2026-06-26T18:00:00Z",
        "last_accessed_at": "2026-06-26T18:10:00Z"
      },
      "score": 0.91,
      "score_components": {
        "similarity": 0.88,
        "lexical": 0.42,
        "tag_match": 1.0,
        "kind_match": 0.0,
        "recency": 0.73,
        "salience": 0.8
      }
    }
  ],
  "next_cursor": null
}
```

Rules:

- Each hit embeds a [Memory Summary](#memory-summary) under `memory` and adds a
  blended `score` plus `score_components`. Hits are ordered by descending
  `score`.
- `mode` is `semantic` when embeddings drive ranking and `keyword` when recall
  has degraded to keyword/tag/kind/time ranking.
- `degraded: true` (mirrored by `mode: "keyword"`) means the embedding provider
  was unavailable or disabled. When degraded, the response envelope should also
  carry a `warnings` entry so callers never mistake a degraded result for a
  fully ranked one. Recall never silently returns unranked or empty results
  without surfacing the degraded state.
- `score_components` weights are tunable; defaults and the hybrid ranking model
  are documented in [memory-model.md](memory-model.md).
- Sensitive hits follow the [Memory Summary](#memory-summary) redaction posture:
  `preview` is omitted and `redacted: true` is set, but the hit and its score
  are still returned.
- Recall over another agent's or a group's memories requires a policy granting
  `read` on the target and is metered as a cross-agent access (see
  [Policy](#policy)).

## Remember Result

Used by `remember` and `/v1/remember`. `remember` auto-routes a quick capture to
either a fact upsert (a clear name→value assertion) or a verbatim memory add, and
returns which one happened. See [context-hydration.md](context-hydration.md).

```json
{
  "kind": "fact",
  "id": "fact_123",
  "echo": "Remembered fact display-name=Atlas",
  "duplicate_of": null
}
```

When the capture routes to a memory that matched a near-duplicate:

```json
{
  "kind": "memory",
  "id": "mem_120",
  "echo": "Merged into mem_120 (duplicate)",
  "duplicate_of": "mem_120"
}
```

Rules:

- `kind` is `fact` (the text was a name→value assertion, upserted idempotently by
  name) or `memory` (anything else, added verbatim with dedup/supersede).
- `id` is the created or updated resource. `remember` never bypasses validation,
  limits, or the `source` provenance contract; agent-authored captures are
  `source: "self"`.
- On a dedup hit, `duplicate_of` is the surviving `mem_` id and the envelope
  carries a `memory_duplicate`/`memory_merged` warning, mirroring the
  [Mutation Result](#mutation-result) write contract. When no duplicate was
  found, `duplicate_of` is `null`.
- `remember` does not emit its own audit event; it routes to the existing
  `memory.added`, `fact.created`, or `fact.updated` events.

## Self Digest

Used by `self show` and `GET /v1/self`. The bounded, always-loadable
session-start digest: primary facts first, then top-N salient memories, then a
one-line index. It is cheap and never requires the embedding provider. The
digest shape, hard cap, and `elided` behavior are defined in
[context-hydration.md](context-hydration.md).

```json
{
  "identity": {
    "agent_id": "agent_123",
    "agent_name": "browser-agent",
    "realm_id": "realm_123",
    "realm_name": "prod"
  },
  "primary_facts": [
    {
      "id": "fact_123",
      "name": "display-name",
      "value": "Atlas",
      "primary": true,
      "sensitive": false,
      "redacted": false,
      "source": "self"
    }
  ],
  "salient_memories": [
    {
      "id": "mem_123",
      "snippet": "Prefers pnpm as the package manager for this project.",
      "kind": "profile",
      "salience": 0.8
    }
  ],
  "index": {
    "kinds": ["profile", "episodic", "session"],
    "tags": ["staging", "performance"],
    "counts": {
      "facts": 6,
      "memories": 41
    }
  },
  "elided": false
}
```

Rules:

- `primary_facts` are the owner's primary facts (one per logical kind), shaped as
  trimmed [Fact](#fact) entries and honoring the same `sensitive` redaction
  posture (`value: null`, `redacted: true`) used in list output.
- `salient_memories` is the top-N set selected by a blended salience+recency
  score (with pinned kinds such as `profile`/`session`), excluding
  archived/forgotten records. Selection is deterministic and never calls the
  embedding provider; the algorithm is defined in
  [memory-model.md](memory-model.md). Each entry carries a short `snippet`
  (redacted for `sensitive` content), its `kind`, and its `salience`.
- `index` is a one-line summary of the store: the `kinds` and `tags` present and
  `counts` of facts and memories.
- The digest has a hard byte/line cap (default ~8 KiB / ~200 lines,
  configurable). When the cap forces omission, `elided` is `true` and callers
  should follow up with `memory recall`; the digest is never silently truncated.

## Session Start and End

Used by `session start`/`session end` and `POST /v1/sessions:start` /
`POST /v1/sessions:end`. Start hydrates identity, open goals, and last progress
in one round-trip; end persists a progress memory and updates open goals. See
[context-hydration.md](context-hydration.md).

Session start result:

```json
{
  "identity": {
    "agent_id": "agent_123",
    "agent_name": "browser-agent",
    "realm_id": "realm_123",
    "realm_name": "prod"
  },
  "open_goals": [
    "finish the recall regression writeup",
    "import the staging AGENTS.md"
  ],
  "last_progress": {
    "id": "mem_140",
    "snippet": "Reproduced the cold-start recall slowdown on staging.",
    "created_at": "2026-06-25T18:00:00Z"
  }
}
```

Session end result:

```json
{
  "saved": true,
  "progress_memory_id": "mem_141"
}
```

Rules:

- `identity` matches the [Self Digest](#self-digest) `identity` block.
- `last_progress` is the most recent `session`-kind progress memory, or `null`
  when none exists. Its `snippet` honors the `sensitive` redaction posture.
- `session end` persists a progress memory of kind `session` (`source: "self"`),
  records its id in `progress_memory_id`, and updates `open_goals`. It emits the
  `session.ended` audit event; `session start` emits `session.started`.

## Consolidation Result

Used by `memory consolidate` and `POST /v1/memories:consolidate`. The garbage
collection verb merges near-duplicate memories, supersedes stale ones, surfaces
(does not auto-pick) conflicting facts, and trims the digest index. Dry run is
the default. See [memory-model.md](memory-model.md).

```json
{
  "dry_run": true,
  "merged": [
    {
      "into": "mem_120",
      "from": ["mem_123", "mem_131"],
      "summary": "merged 2 near-duplicate staging notes into mem_120"
    }
  ],
  "superseded": [
    {
      "id": "mem_090",
      "by": "mem_140",
      "summary": "stale progress note superseded by latest session memory"
    }
  ],
  "conflicts": [
    {
      "kind": "fact",
      "name": "package-manager",
      "values": [
        { "id": "fact_201", "source": "import:AGENTS.md" },
        { "id": "fact_202", "source": "self" }
      ],
      "summary": "two values for package-manager; resolve manually"
    }
  ],
  "trimmed_index": {
    "kinds": ["profile", "episodic", "session"],
    "tags": ["staging", "performance"],
    "counts": {
      "facts": 6,
      "memories": 38
    }
  },
  "audit_event_id": null
}
```

Rules:

- `dry_run` defaults to `true`; a dry run reports `merged`/`superseded`/
  `conflicts` as planned actions, persists nothing, and leaves `audit_event_id`
  `null`. An applied run sets `dry_run: false`, performs the changes, and emits
  the `memory.consolidated` audit event.
- `merged` records each near-duplicate collapse (`into` survivor, `from` sources);
  `superseded` records each stale memory tombstoned in favor of a newer one.
- `conflicts` SURFACES conflicting facts for human resolution rather than
  auto-picking a winner. Consolidate never silently overwrites `operator`- or
  `import:`-authored records; it respects the `source` provenance contract (see
  [Fact](#fact)).
- `trimmed_index` is the post-consolidation [Self Digest](#self-digest) `index`,
  so callers can see the digest shrink.
- `consolidate` is a mutating, guarded verb and is excluded in `--read-only` MCP
  mode.

## Fact

Used by `fact get`, `fact list`, and operator realm scans. Facts are ordinary
readable identity data; only `sensitive` facts are redacted by default.

`fact get` (single, authorized read — value returned in clear):

```json
{
  "id": "fact_123",
  "name": "email",
  "owner": {
    "kind": "agent",
    "agent_id": "agent_123",
    "agent_name": "browser-agent"
  },
  "realm_id": "realm_123",
  "value": "browser-agent@example.com",
  "value_encoding": "plain",
  "primary": true,
  "sensitive": false,
  "redacted": false,
  "format": "email",
  "source": "self",
  "version": 2,
  "created_at": "2026-06-26T18:00:00Z",
  "updated_at": "2026-06-26T18:05:00Z"
}
```

`fact list` entry where a fact is `sensitive` (redacted in list/scan output):

```json
{
  "id": "fact_456",
  "name": "account-number",
  "owner": {
    "kind": "agent",
    "agent_id": "agent_123",
    "agent_name": "browser-agent"
  },
  "value": null,
  "value_encoding": null,
  "primary": false,
  "sensitive": true,
  "redacted": true,
  "format": "string",
  "source": "self",
  "version": 1,
  "created_at": "2026-06-26T18:00:00Z",
  "updated_at": "2026-06-26T18:00:00Z"
}
```

Rules:

- Lookup is deterministic by `name` within the owner; `fact get email` returns
  the one true value for the caller's identity.
- Non-sensitive facts include `value` in both `get` and `list`/`scan`.
- `sensitive` facts are redacted in `list`/`scan` output (`value: null`,
  `redacted: true`), but an authorized single-record `fact get` returns the
  value in clear. There is no reveal ceremony and no separate sensitive
  value-size budget.
- `primary: true` marks the canonical value of the fact's logical kind. At most
  one primary per logical kind per owner; primary facts are surfaced first in
  `whoami`, profile, and export.
- Binary-safe values should use `value_encoding: "base64"`.
- `format` is an optional display/validation hint such as `string`, `email`,
  `url`, `date`, or `number`.
- `source` records authorship/provenance with the same value set as memories:
  `self`, `agent:<name>`, `operator`, and `import:<file>`. `consolidate` and the
  self-digest never silently overwrite an `operator`- or `import:`-authored fact;
  conflicting values are surfaced rather than auto-resolved.
- A `fact show --history` view returns a `history` array shaped like the memory
  [edit history](#memory-detail).

The facts model is tracked in [facts-model.md](facts-model.md).

## Policy

Used by `policy list`/`policy show`. A policy binds a subject, a permission, and
a target, scoped to memories and/or facts, with an optional filter.

```json
{
  "id": "pol_123",
  "realm_id": "realm_123",
  "description": "Analysts may read the archivist's notes.",
  "subject": {
    "kind": "group",
    "id": "grp_123",
    "name": "analysts"
  },
  "permission": "read",
  "target": {
    "kind": "agent",
    "id": "agent_456",
    "name": "archivist"
  },
  "scope": ["memory", "fact"],
  "filter": {
    "memory_kind": ["semantic", "note"],
    "tag": ["public"],
    "fact_name": null,
    "include_sensitive": false
  },
  "effect": "allow",
  "created_by": {
    "kind": "operator",
    "id": "opr_123",
    "name": "scott"
  },
  "created_at": "2026-06-26T18:00:00Z",
  "updated_at": "2026-06-26T18:00:00Z"
}
```

Rules:

- `subject` and `target` `kind` values are `agent` and `group`.
- `permission` is one verb, escalating in danger: `read`, `contribute`,
  `curate`, `forget`.
- `scope` is a subset of `["memory", "fact"]`.
- `filter` is optional; absent keys mean no narrowing. `include_sensitive: false`
  means the policy does not grant access to `sensitive` records.
- `effect` is `allow` in v0; the absence of a matching `allow` is deny. Policy
  `deny` effects are post-v0.

The policy engine and guardrails are tracked in
[access-policy.md](access-policy.md).

### Policy Test Result

Used by `policy test`, `/v1/policies:test`, and the `witself.policy.test` MCP
tool. This is the canonical dry-run for access decisions.

```json
{
  "subject": {
    "kind": "group",
    "id": "grp_123",
    "name": "analysts"
  },
  "permission": "read",
  "target": {
    "kind": "agent",
    "id": "agent_456",
    "name": "archivist"
  },
  "scope": ["memory"],
  "decision": "allow",
  "policy_id": "pol_123",
  "reason": "matched policy pol_123"
}
```

Rules:

- `decision` is `allow` or `deny`.
- On `allow`, `policy_id` is the deciding policy.
- On `deny`, `policy_id` is `null` and `reason` explains the default-deny
  outcome. The deny `reason` must not leak target content.

## Security Group

Used by `group list`/`group show`. A group is both a policy subject and a policy
target and can own group-scoped shared memories and facts.

```json
{
  "id": "grp_123",
  "realm_id": "realm_123",
  "name": "analysts",
  "description": "Read access to shared research context.",
  "members": [
    {
      "agent_id": "agent_123",
      "agent_name": "browser-agent"
    },
    {
      "agent_id": "agent_789",
      "agent_name": "researcher"
    }
  ],
  "admins": [
    {
      "agent_id": "agent_123",
      "agent_name": "browser-agent"
    }
  ],
  "member_count": 2,
  "owned_memory_count": 12,
  "owned_fact_count": 3,
  "created_at": "2026-06-26T18:00:00Z",
  "updated_at": "2026-06-26T18:00:00Z"
}
```

Rules:

- `name` is unique within the realm.
- `admins` lists agents allowed to manage membership under `group:manage`.
- `owned_memory_count`/`owned_fact_count` reflect group-scoped shared records,
  which use the same [Memory](#memory-detail)/[Fact](#fact) shapes with an
  `owner.kind` of `group`.
- `group list` entries may omit `members` and return only `member_count`;
  `group show` returns the full membership.

Security groups are tracked in [security-groups.md](security-groups.md).

## Message

Used by `message list`/`message read` and the messaging API. The sender is
always derived from the authenticated token; `from` is never accepted as input.

```json
{
  "id": "msg_123",
  "realm_id": "realm_123",
  "from": {
    "kind": "agent",
    "agent_id": "agent_123",
    "agent_name": "browser-agent"
  },
  "to": {
    "kind": "agent",
    "realm": "research-lab",
    "agent_id": "agent_456",
    "agent_name": "archivist"
  },
  "subject": "recall regression",
  "kind": "notice",
  "body": "Cold-start recall is slow on staging; see mem_123.",
  "payload": {
    "memory_ref": "witself://agent/browser-agent/memory/mem_123"
  },
  "thread_id": "thr_123",
  "conversation_id": "thr_123",
  "envelope": {
    "hop_count": 0,
    "max_hops": 8,
    "sequence": 3,
    "nonce": "b3f1c2...",
    "expires_at": "2026-06-26T19:00:00Z",
    "signature": "<JWS over the canonicalized envelope>"
  },
  "created_at": "2026-06-26T18:00:00Z",
  "delivery": {
    "state": "delivered",
    "delivered_at": "2026-06-26T18:00:01Z"
  },
  "read_state": {
    "state": "read",
    "read_at": "2026-06-26T18:02:00Z",
    "acked_at": "2026-06-26T18:02:05Z"
  }
}
```

Rules:

- `from` is always the token-bound sender. Sender forgery is structurally
  impossible through the API; passing a `from` field is rejected or ignored.
- `to.kind` is `agent` or `group`. A message addressed to a group is fanned out
  to current members, each with its own per-member `delivery` and `read_state`.
- `delivery.state` values: `queued`, `delivered`, `failed`.
- `read_state.state` values: `unread`, `read`, `acked`.
- `body` and `payload` are message content. They must not appear in errors,
  logs, audit records, or metrics. Receiving agents must treat `body` and
  `payload` as untrusted input; a message cannot itself authorize a cross-agent
  write (writes still require policy).
- `subject`/`kind` are short classifications safe for list views.
- `thread_id`/`conversation_id` are optional and drive per-conversation
  ordering. `conversation_id` is the first-class cross-realm conversation
  handle and reuses the `thr_` prefix; an in-realm thread and its cross-realm
  conversation share one id space (see [Conversation](#conversation)).
- `to.realm` and `from.realm` are optional realm handles that make a recipient
  or sender cross-realm. When `realm` is absent the participant is local
  (unchanged from in-realm messaging); when present it qualifies the address as
  `witself://<realm-handle>/agent/<name>`. As with `from`, the sending realm in
  `from.realm` is derived from the authenticated, signed envelope, never
  accepted as free input.
- `envelope` carries the cross-realm safety fields and is present only for
  messages that cross a realm boundary:
  - `hop_count` / `max_hops` — relay-hop governor; each relay increments
    `hop_count`, and a message exceeding `max_hops` (default `8`) is dropped and
    audited (`loop.suspended`).
  - `sequence` — per-conversation monotonic ordering counter.
  - `nonce` — single-use value for replay rejection within the TTL window.
  - `expires_at` — envelope TTL (default 1h); an expired envelope is rejected.
  - `signature` — the sending realm's JWS over the canonicalized envelope,
    verified against that realm's published JWKS (from its signed
    [realm card](#realm-card)).
  These governors and the relay model are specified in
  [agent-collaboration.md](agent-collaboration.md). A cross-realm message still
  carries no standing authority: the receiving realm must independently allow
  the sender (federation allow-list + policy).
- `direction` selects a mailbox view in `message list` and the messaging API.
  Its value set is `inbox` or `outbox`; there is no `all` value in v0. The MCP
  `direction` enum references this set, and the CLI selector maps to it.

Inter-agent messaging is tracked in
[inter-agent-messaging.md](inter-agent-messaging.md).

## Conversation

Used by `GET /v1/conversations` / `GET /v1/conversations/{id}` and the
cross-realm collaboration commands. A conversation is the first-class
cross-realm task/thread resource: it carries an A2A-style task state machine and
the per-conversation budget governors. It reuses the `thr_` id prefix so an
in-realm thread and its cross-realm conversation share one id space.

```json
{
  "id": "thr_123",
  "state": "working",
  "participants": [
    {
      "kind": "agent",
      "realm": "default",
      "agent_id": "agent_123",
      "agent_name": "browser-agent"
    },
    {
      "kind": "agent",
      "realm": "research-lab",
      "agent_name": "archivist"
    }
  ],
  "turn_budget": 24,
  "turns_used": 6,
  "cost_budget": "5.00",
  "remaining_turns": 18,
  "created_at": "2026-06-26T18:00:00Z",
  "updated_at": "2026-06-26T18:05:00Z"
}
```

Rules:

- `state` values are `submitted`, `working`, `input_required`, `auth_required`,
  `completed`, `failed`, and `canceled`. Transitions emit the
  `conversation.started` / `conversation.state_changed` /
  `conversation.completed` / `conversation.failed` / `conversation.canceled`
  audit events.
- `participants[]` lists the agents on each side, each carrying an optional
  `realm` handle; a participant without `realm` is local (matching the
  [Message](#message) `to.realm` / `from.realm` convention).
- `turn_budget` / `turns_used` / `remaining_turns` track the per-conversation
  turn governor; exhaustion suspends the conversation and emits `loop.suspended`
  and `budget.exhausted`. `cost_budget` is the optional cost ceiling for the
  conversation.
- Conversations never include message `body`/`payload` content; bodies follow
  the [Message](#message) redaction posture.

The cross-realm conversation and its governors are specified in
[agent-collaboration.md](agent-collaboration.md).

## Realm Card

Used by `GET /.well-known/witself-card.json` (served outside `/v1`) and the
`federation` command group. The card is the signed, fetchable description of
what a realm offers across the federation: its handle, the agents and skills it
exposes, its endpoint, accepted auth, signing key, and delivery modes.

```json
{
  "realm_handle": "research-lab",
  "agents": [
    {
      "handle": "archivist",
      "skills": ["recall", "summarize"]
    }
  ],
  "endpoint": "https://research-lab.example.com",
  "accepted_auth": ["bearer", "mtls"],
  "signing": {
    "kty": "OKP",
    "crv": "Ed25519",
    "kid": "realm-key-1",
    "x": "<base64url public key>"
  },
  "delivery_modes": ["mailbox", "listen"],
  "ttl": 3600,
  "expires_at": "2026-06-26T19:00:00Z",
  "signature": "<JWS over the canonicalized card>"
}
```

Rules:

- `realm_handle` is the federation-visible handle that resolves (through the
  shared global directory) to the realm's home cell, endpoint, and signing key
  (see [deployment-cells.md](deployment-cells.md)).
- `agents[]` advertises each exposed agent `handle` and its `skills[]`;
  cross-realm sends address `witself://<realm-handle>/agent/<handle>`.
- `signing` is the realm signing **public key** (a JWKS-style entry) used to
  verify message envelopes and the card itself.
- `delivery_modes[]` advertises how inbound is received (e.g. durable `mailbox`
  and long-poll `listen`); agents run no inbound HTTP servers.
- `ttl` / `expires_at` bound the card's freshness so consumers refetch and
  re-verify; revocation is real-time, not cache-only.
- `signature` is **mandatory**: the card is a JWS over its canonicalized JSON,
  and a consumer must verify it against the `signing` key before trusting the
  card. An unsigned or unverifiable card is rejected.

The signed card, resolution model, and verify-before-trust rule are specified in
[agent-collaboration.md](agent-collaboration.md).

## Service Administration Shapes

Used by `setup`, `account`, `billing`, and `support` commands.

Setup result:

```json
{
  "account": {
    "id": "acct_123",
    "display_name": "Acme Agents",
    "status": "reused"
  },
  "realm": {
    "id": "realm_123",
    "name": "prod",
    "status": "created"
  },
  "agents": [
    {
      "id": "agent_123",
      "name": "browser-agent",
      "status": "reused",
      "token_action": "created",
      "token_file": "./witself-tokens/browser-agent.token",
      "token_file_mode": "0600",
      "token_file_write": "created",
      "env": {
        "WITSELF_TOKEN_FILE": "./witself-tokens/browser-agent.token",
        "WITSELF_REALM": "prod"
      },
      "verified": true
    }
  ],
  "kubernetes": {
    "manifest_file": "./witself-agent-secret.yaml",
    "namespace": "agents",
    "secret_name": "witself-prod-agents"
  }
}
```

Rules:

- `status` values should be stable strings such as `created`, `reused`, or
  `unchanged`.
- `token_action` values should be stable strings such as `created`, `reused`,
  `rotated`, `skipped`, or `blocked`.
- Raw token values should not be included in setup JSON when `token_file` is
  used.
- If a raw token must be returned for a specific command, it must be a one-time
  explicit token-create or token-rotate response.
- Existing token reuse or rotation must be explicit. When setup cannot proceed
  without that choice, it should return `conflict` with
  `details.reason: "token_choice_required"`.
- `token_file_mode` should report the intended owner-only mode when applicable,
  such as `0600`, or `platform_default` when the platform does not expose
  POSIX-style modes.
- `token_file_write` values should be stable strings such as `created`,
  `reused_existing`, `rotated`, `skipped`, or `blocked_existing_path`.
- Setup output should include enough environment mapping for ephemeral agents to
  start immediately in managed-service mode.
- Kubernetes output should describe the emitted manifest path and target names
  without printing token contents.

Account summary:

```json
{
  "id": "acct_123",
  "display_name": "Acme Agents",
  "legal_name": "Acme Agents LLC",
  "primary_email": "ops@example.com",
  "billing_email": "billing@example.com",
  "support_email": "support@example.com",
  "created_at": "2026-06-26T18:00:00Z",
  "updated_at": "2026-06-26T18:00:00Z"
}
```

Usage summary:

```json
{
  "plan": {
    "id": "plan_team",
    "name": "Team"
  },
  "window": {
    "since": "2026-06-01T00:00:00Z",
    "until": "2026-06-26T18:00:00Z",
    "resets_at": "2026-07-01T00:00:00Z"
  },
  "items": [
    {
      "dimension": "memory_recall",
      "quantity": 1234,
      "unit": "event",
      "realm_id": "realm_123",
      "agent_id": "agent_123",
      "limit": {
        "included": 5000,
        "soft_limit": 4000,
        "hard_limit": 6000,
        "overage_behavior": "throttle",
        "status": "ok"
      }
    }
  ]
}
```

Limit summary:

```json
{
  "plan": {
    "id": "plan_team",
    "name": "Team"
  },
  "realm": {
    "id": "realm_123",
    "name": "prod"
  },
  "items": [
    {
      "dimension": "active_agent",
      "unit": "agent",
      "used": 12,
      "included": 15,
      "soft_limit": 12,
      "hard_limit": 15,
      "overage_behavior": "block",
      "status": "near_limit",
      "resets_at": null
    },
    {
      "dimension": "api_request",
      "unit": "request",
      "used": 18420,
      "included": 100000,
      "soft_limit": 80000,
      "hard_limit": 120000,
      "overage_behavior": "throttle",
      "status": "ok",
      "resets_at": "2026-07-01T00:00:00Z"
    }
  ],
  "next_command": "ws billing usage --realm prod --show-limits"
}
```

Limit status values should include:

- `ok`
- `near_limit`
- `over_limit`
- `throttled`
- `blocked`

Overage behavior values should include:

- `warn`
- `throttle`
- `block`

Invoice summary:

```json
{
  "id": "inv_123",
  "status": "paid",
  "currency": "usd",
  "total_cents": 1200,
  "period_start": "2026-06-01T00:00:00Z",
  "period_end": "2026-06-30T23:59:59Z"
}
```

Hosted provider session result:

Used when a command starts a provider-hosted flow such as checkout,
payment-method setup, crypto payment, identity verification, or another external
approval session.

```json
{
  "session_id": "hps_123",
  "kind": "billing.crypto.checkout",
  "provider": "example-provider",
  "status": "open",
  "url": "https://payments.example/checkout/hps_123",
  "expires_at": "2026-06-26T18:15:00Z",
  "next_command": "ws billing crypto status hps_123 --watch",
  "metadata": {
    "invoice_id": "inv_123",
    "promo_code": "FOUNDERS25",
    "asset": "USDC",
    "network": "base",
    "amount": "12.00",
    "currency": "usd"
  },
  "audit_event_id": "aud_126"
}
```

Rules:

- `session_id` is required for resumable hosted flows.
- `status` should use stable values such as `open`, `pending`, `completed`,
  `failed`, `expired`, or `canceled`.
- `url` should be present when the operator must open a browser or wallet flow.
- `next_command` should be a complete CLI command that can resume, inspect, or
  watch the flow.
- Hosted provider URLs may grant access to a payment or setup flow until they
  expire. They are not raw payment credentials, but logs and audit records
  should avoid persisting full URLs unless policy explicitly allows it.
- `metadata` must contain only non-sensitive context.

Crypto payment quote:

```json
{
  "id": "cpq_123",
  "session_id": "hps_123",
  "provider": "example-provider",
  "status": "open",
  "invoice_id": "inv_123",
  "subscription_id": null,
  "amount": "12.00",
  "currency": "usd",
  "asset": "USDC",
  "network": "base",
  "settlement_currency": "usd",
  "checkout_url": "https://payments.example/checkout/cpq_123",
  "expires_at": "2026-06-26T18:15:00Z",
  "next_command": "ws billing crypto status hps_123 --watch"
}
```

Crypto payment status:

```json
{
  "id": "cps_123",
  "quote_id": "cpq_123",
  "provider": "example-provider",
  "status": "confirmed",
  "invoice_id": "inv_123",
  "amount": "12.00",
  "currency": "usd",
  "asset": "USDC",
  "network": "base",
  "settlement_currency": "usd",
  "confirmed_at": "2026-06-26T18:05:00Z"
}
```

Support ticket summary:

```json
{
  "id": "sup_123",
  "subject": "Agent token rotation question",
  "status": "open",
  "priority": "normal",
  "category": "account",
  "created_at": "2026-06-26T18:00:00Z",
  "updated_at": "2026-06-26T18:00:00Z"
}
```

Rules:

- Payment methods must be redacted summaries, not raw payment details.
- Crypto payment responses may include provider names, quote IDs, checkout URLs,
  hosted session IDs, next commands, redacted wallet/payment metadata, assets,
  networks, amounts, and statuses. They must not include wallet seed phrases,
  private keys, raw wallet credentials, or provider secrets.
- Crypto payment support is a payment rail, not a Witself utility-token
  requirement.
- Support tickets and diagnostics must not include memory content, fact values,
  message bodies/payloads, embedding vectors, or raw tokens unless a future
  explicit secure-support channel is designed for that purpose.
- Billing and support mutations should use the standard mutation result shape.

The billing and limits model is tracked in
[billing-and-limits.md](billing-and-limits.md).

## Mutation Result

Used by create, update, rename, copy, archive, restore, delete, agent
lifecycle, token lifecycle, realm lifecycle, account lifecycle, policy, group,
message, billing, support, and import/export commands.

```json
{
  "changed": true,
  "dry_run": false,
  "resource": {
    "kind": "memory",
    "id": "mem_123",
    "name": null
  },
  "echo": "Added mem_123 (kind=episodic, salience=0.8)",
  "planned_changes": [],
  "audit_event_id": "aud_125"
}
```

Cross-agent and destructive mutations attribute the deciding policy and require
a reason. A `memory forget` across agents under a dry run:

```json
{
  "changed": false,
  "dry_run": true,
  "resource": {
    "kind": "memory",
    "id": "mem_777",
    "name": null
  },
  "owner": {
    "kind": "agent",
    "agent_id": "agent_456",
    "agent_name": "archivist"
  },
  "policy_id": "pol_888",
  "reason": "pruning stale staging notes",
  "planned_changes": [
    {
      "action": "forget",
      "resource": {
        "kind": "memory",
        "id": "mem_777"
      },
      "summary": "soft-delete (tombstone) memory mem_777 of agent archivist; reversible within retention window"
    }
  ],
  "audit_event_id": null
}
```

Rules:

- Mutations should report whether anything changed.
- Every successful mutation carries a deterministic, human-readable `echo` string
  the model can self-verify and chain on, for example
  `"Remembered fact display-name=Atlas"`,
  `"Added mem_123 (kind=profile, salience=0.6)"`, or
  `"Merged into mem_120 (duplicate)"`. `echo` never includes `sensitive` fact
  values or `sensitive`-flagged memory content.
- A write that hits a near-duplicate returns the existing `mem_` id and adds a
  `memory_duplicate` (or `memory_merged`, when the records were combined) entry
  to the envelope `warnings` array instead of silently creating a near-dup; the
  `echo` reflects the merge.
- Dry-run mutations should set `dry_run` to `true`, `changed` to `false`, and
  include `planned_changes`.
- Each `planned_changes` entry should be an object with at least `action`,
  `resource.kind`, and a human-readable `summary`.
- `planned_changes` must not include memory content, fact values, message
  bodies/payloads, embedding vectors, raw tokens, or raw payment details.
- Dry-run mutations must not persist state, generate tokens, create hosted
  provider sessions, charge payment methods, send messages, or send
  customer/support notifications.
- Cross-agent and group-owned `curate`/`forget`/`delete` mutations include
  `owner`, the deciding `policy_id`, and the audit `reason`. They are
  soft/tombstoned by default and reversible within the retention window; hard
  delete is a further-guarded step.
- `forget` reports a tombstone (`changed: true`, `resource.kind: "memory"`);
  `restore` reverses it within the retention window. Facts have no
  forget/restore tombstone; a fact mutation is a delete-only hard delete.
- Mutations should include the affected resource and `audit_event_id` when audit
  is available.
- Token create and rotate responses may include the raw token once, but only for
  commands explicitly designed to return the token.

## Audit Event

Used by `audit list` and `audit show`.

```json
{
  "id": "aud_123",
  "action": "secret.reveal",
  "actor": {
    "kind": "agent",
    "id": "agent_456",
    "name": "archivist"
  },
  "target": {
    "kind": "secret",
    "id": "sec_123",
    "name": "github/builder"
  },
  "owner": {
    "kind": "agent",
    "agent_id": "agent_123",
    "agent_name": "browser-agent"
  },
  "policy_id": null,
  "grant_id": "grt_123",
  "reason": "CI runner needs the deploy token",
  "timestamp": "2026-06-26T18:00:00Z",
  "metadata": {
    "field": "password",
    "server_side_decrypt": true
  }
}
```

Rules:

- Audit `action` values are stable dotted `<resource>.<verb>` event names drawn
  from the canonical registry. The authoritative, complete registry lives in
  [audit-retention.md](audit-retention.md); this list mirrors it. The complete
  set, grouped by family, is:
  - Platform: `auth.succeeded`, `auth.failed`, `account.profile_changed`,
    `account.member_changed`, `account.role_changed`, `operator.override`,
    `agent.created`, `agent.renamed`, `agent.copied`, `agent.disabled`,
    `agent.enabled`, `agent.archived`, `agent.deleted`, `token.created`,
    `token.rotated`, `token.revoked`, `token.use_failed`, `token.file_choice`,
    `audit.retention.swept`, `limit.decision`.
  - Open plane (identity): `memory.added`, `memory.adjusted`, `memory.read`,
    `memory.recalled`, `memory.forgotten`, `memory.restored`, `memory.deleted`,
    `memory.consolidated`, `memory.imported`, `fact.created`, `fact.updated`,
    `fact.deleted`, `fact.primary_changed`, `fact.imported`, `crossagent.read`,
    `crossagent.contributed`, `crossagent.curated`, `crossagent.forgotten`,
    `policy.created`, `policy.updated`, `policy.deleted`, `policy.tested`,
    `policy.access_allowed`, `policy.access_denied`, `group.created`,
    `group.deleted`, `group.member_added`, `group.member_removed`,
    `group.record_changed`, `message.sent`, `message.delivered`, `message.read`,
    `message.acked`, `session.started`, `session.ended`, `self.digest.emitted`,
    `identity.exported`, `identity.imported`.
  - Sealed plane (credentials): `secret.created`, `secret.updated`,
    `secret.renamed`, `secret.copied`, `secret.archived`, `secret.restored`,
    `secret.deleted`, `secret.reveal`, `secret.grant`, `secret.revoke`,
    `totp.enrolled`, `totp.code`, `totp.seed_revealed`, `totp.deleted`,
    `key.rotated`.
  - Billing and support (managed service): `billing.subscription.created`,
    `billing.subscription.updated`, `billing.subscription.canceled`,
    `billing.payment_method.added`, `billing.payment_method.removed`,
    `billing.payment_method.default_changed`, `billing.invoice.created`,
    `billing.invoice.paid`, `billing.invoice.payment_failed`,
    `billing.refund.created`, `billing.crypto.quote.created`,
    `billing.crypto.checkout.started`, `billing.crypto.payment.confirmed`,
    `billing.crypto.payment.failed`, `billing.crypto.refund.created`,
    `billing.crypto.provider_event.reconciled`, `support.ticket.created`,
    `support.ticket.commented`, `support.ticket.closed`, `support.bundle.created`.
  - Collaboration (cross-realm): `conversation.started`,
    `conversation.state_changed`, `conversation.completed`,
    `conversation.failed`, `conversation.canceled`, `federation.peer_allowed`,
    `federation.peer_denied`, `federation.consent_accepted`, `loop.suspended`,
    `budget.exhausted` (see [agent-collaboration.md](agent-collaboration.md)).
  - Deployment cells: `tenant.placed`, `tenant.migration_started`,
    `tenant.migration_completed`, `tenant.migration_failed` (see
    [deployment-cells.md](deployment-cells.md)).
- The `fact set` / `remember` upsert is not its own event: it emits
  `fact.created` for a new fact or `fact.updated` for an existing one.
- Cross-agent and group-owned mutation events include `owner`, the deciding
  `policy_id`, and the audit `reason` so each action is fully attributed (for
  example "memory mem_777 of agent archivist was pruned by agent browser-agent
  under policy pol_888").
- Operator override actions are audited like agent actions and carry the
  operator `actor` plus a `reason` on destructive/cross-agent actions.
- Audit events must not include memory content, fact values, message
  bodies/payloads, embedding vectors, raw tokens, or raw payment details.
- Billing and payment audit events may include non-sensitive metadata such as
  invoice ID, subscription ID, payment provider, payment method type, crypto
  asset, network, amount, currency, status, and provider event ID. They must not
  include raw payment details, card numbers, provider tokens, wallet seed
  phrases, wallet private keys, raw wallet credentials, or full wallet
  identifiers.
- `metadata` should contain only non-sensitive request context such as record
  ids, owner agent/group, memory kind, tags, fact name, policy id, message id,
  recipient, and decision outcome.

The audit retention model is tracked in
[audit-retention.md](audit-retention.md).

## Reference Parse and Resolve

Used by `ws reference parse`/`ws reference resolve` and the
`witself.reference.parse`/`witself.reference.resolve` MCP tools. References use
the `witself://` scheme (never `ws://`, which collides with WebSocket).

Reference forms (the final path component is the leaf; URL-encode if needed):

- `witself://memory/<path-or-id>` — current agent's memory.
- `witself://fact/<name>` — current agent's fact.
- `witself://agent/<agent>/fact/<name>` — a specific agent's fact (cross-agent,
  policy-gated).
- `witself://agent/<agent>/memory/<id>` — a specific agent's memory
  (cross-agent, policy-gated).
- `witself://group/<group>/memory/<id>` and `witself://group/<group>/fact/<name>`
  — group-scoped memory or fact.
- `witself://<realm-handle>/agent/<name>` — a realm-qualified cross-realm agent
  address used by [Message](#message) `to.realm`/`from.realm` and the
  [Realm Card](#realm-card). Cross-realm resolution is post-v0
  (capability-gated; see [agent-collaboration.md](agent-collaboration.md)).

Parse result (`reference parse`, no authorization or I/O):

```json
{
  "reference": "witself://agent/archivist/fact/email",
  "scheme": "witself",
  "kind": "fact",
  "owner_kind": "agent",
  "owner": "archivist",
  "leaf": "email",
  "valid": true
}
```

Resolve result (`reference resolve`, authorized lookup):

```json
{
  "reference": "witself://agent/archivist/fact/email",
  "resolved": true,
  "kind": "fact",
  "decision": "allow",
  "policy_id": "pol_123",
  "fact": {
    "id": "fact_999",
    "name": "email",
    "owner": {
      "kind": "agent",
      "agent_id": "agent_456",
      "agent_name": "archivist"
    },
    "value": "archivist@example.com",
    "value_encoding": "plain",
    "primary": true,
    "sensitive": false,
    "redacted": false
  }
}
```

Rules:

- `reference parse` validates structure only; it performs no authorization and
  no lookup. `valid: false` returns a `usage_error`-style detail rather than
  resolving.
- `reference resolve` enforces the same authorization as a direct read. A
  cross-agent or cross-group reference resolves only when policy permits; a
  denied resolve returns `access_denied` with the deciding context.
- A resolved reference embeds the underlying [Memory Detail](#memory-detail) or
  [Fact](#fact) shape under `memory` or `fact`, honoring the same
  `sensitive`/redaction posture (single authorized read returns the value in
  clear).
- References used in memory `links[]` are validated at write time and re-checked
  at resolve time. Dangling references resolve with `resolved: false` and a
  `not_found`-style reason; they are reported, not silently dropped.

## Sealed-Plane Shapes

The shapes below cover the **sealed plane** (secrets and TOTP), the
confidentiality counterpart to the open plane's memories and facts. Unlike the
open plane, sealed material is KMS-backed envelope-encrypted, reveal-gated, and
**never embedded, never returned by semantic recall, never in the self-digest,
and never plaintext-exported or ingested** from CLAUDE.md/AGENTS.md. The only
value-returning paths are the audited reveal / TOTP-code ceremonies. The data
model and lifecycle live in [secret-model.md](secret-model.md) and
[totp-2fa.md](totp-2fa.md); the crypto envelope and custody modes live in
[encryption-model.md](encryption-model.md) and
[key-hierarchy.md](key-hierarchy.md).

### Secret Summary

Used by `secret list` and `secret scan`. Sensitive field values are never
included here; this is redacted, inventory-only metadata.

```json
{
  "id": "sec_123",
  "name": "github/builder",
  "description": "GitHub login for browser-agent",
  "template": "login",
  "owner": {
    "kind": "agent",
    "agent_id": "agent_123",
    "agent_name": "browser-agent"
  },
  "realm_id": "realm_123",
  "field_count": 3,
  "sensitive_field_count": 1,
  "tags": ["github"],
  "archived": false,
  "created_at": "2026-06-26T18:00:00Z",
  "updated_at": "2026-06-26T18:00:00Z"
}
```

Rules:

- `owner.kind` is `agent` or `group`, matching the unified owner model; a
  group-owned secret uses the [group owner](#common-types) shape. There is no
  separate `shared` scope.
- `template` is one of `login`, `api-key`, `ssh-key`, `certificate`, `env`, or
  `generic`.
- Summaries carry only inventory metadata. Field values, TOTP seeds, and
  ciphertext never appear in list/scan output.

### Secret Detail

Used by `secret show`. Sensitive fields are **redacted by default**: a show is
not a reveal. Returning a sensitive value requires the explicit, audited reveal
ceremony ([Secret Reveal Result](#secret-reveal-result)).

```json
{
  "id": "sec_123",
  "name": "github/builder",
  "description": "GitHub login for browser-agent",
  "template": "login",
  "owner": {
    "kind": "agent",
    "agent_id": "agent_123",
    "agent_name": "browser-agent"
  },
  "realm_id": "realm_123",
  "fields": [
    {
      "name": "username",
      "sensitive": false,
      "value": "agent-amy",
      "value_encoding": "plain",
      "redacted": false
    },
    {
      "name": "password",
      "sensitive": true,
      "value": null,
      "value_encoding": null,
      "redacted": true,
      "value_ref": "witself://secret/github/builder/password"
    }
  ],
  "tags": ["github"],
  "archived": false,
  "created_at": "2026-06-26T18:00:00Z",
  "updated_at": "2026-06-26T18:00:00Z"
}
```

Rules:

- Sensitive fields are redacted by default (`value: null`, `redacted: true`).
  This redaction is an encryption boundary, not the open plane's PII/display
  posture: the value is ciphertext at rest and is only returned through reveal.
- Redacted sensitive fields should carry a `value_ref` (a
  `witself://secret/...` reference) when a stable reference is available, so
  callers can reveal or inject without copying plaintext.
- Non-sensitive fields may include `value`. Binary-safe values use
  `value_encoding: "base64"`.
- A field may have its own stable id (`fld_` prefix); callers should not parse ID
  internals.

### Secret Reveal Result

Used only by explicit reveal operations (`secret reveal`,
`POST /v1/secrets/{secret_id}:reveal`). This is the sealed plane's audited
value-returning ceremony; the open plane has no equivalent. The shape is
selected by backend/realm capability ([Capability Result](#capability-result),
[key-hierarchy.md](key-hierarchy.md)): `server_side_decrypt` returns the
decrypted value; `client_side_decrypt` returns ciphertext plus the envelope and
key-unwrap material so the client decrypts locally — no plaintext crosses the
wire.

Server-mediated shape (`server_side_decrypt`, e.g. managed token-only pods — the
v0 over-the-wire path):

```json
{
  "secret": {
    "id": "sec_123",
    "name": "github/builder"
  },
  "field": {
    "name": "password",
    "sensitive": true,
    "value": "generated-password",
    "value_encoding": "plain"
  },
  "decrypt_mode": "server_side",
  "audit_event_id": "aud_123",
  "expires_at": null
}
```

Client-held shape (`client_side_decrypt`, BYOK over the wire): **post-v0** —
remote v0 backends advertise `client_side_decrypt: false` and do not emit this
shape (see [key-hierarchy.md](key-hierarchy.md) V0 crypto subset). No plaintext
`value`; the client unwraps the DEK and AEAD-opens the ciphertext per the
[key-hierarchy.md](key-hierarchy.md) client-held step list.

```json
{
  "secret": {
    "id": "sec_123",
    "name": "github/builder"
  },
  "field": {
    "name": "password",
    "sensitive": true,
    "value": null,
    "value_encoding": null
  },
  "decrypt_mode": "client_side",
  "envelope": {
    "ciphertext": "<base64>",
    "nonce": "<base64>",
    "aead_algorithm": "XCHACHA20_POLY1305",
    "dek_id": "dek_123",
    "dek_version": 1,
    "kms_provider": "aws-kms",
    "aad_context": {
      "realm_id": "realm_123",
      "secret_id": "sec_123",
      "field": "password",
      "owner_kind": "agent",
      "domain": "secret-field"
    }
  },
  "key_material": {
    "kek_id": "kek_123",
    "wrapped_dek": "<base64>",
    "wrapped_kek": "<base64>",
    "kms_provider": "aws-kms",
    "kms_key_ref": "arn:aws:kms:...",
    "encryption_context": {
      "realm_id": "realm_123",
      "purpose": "realm-kek",
      "kek_id": "kek_123",
      "key_version": 1
    }
  },
  "audit_event_id": "aud_123",
  "expires_at": null
}
```

Rules:

- Reveal responses are the only secret responses that contain a sensitive value,
  and only in the `server_side` shape; the `client_side` shape carries
  ciphertext and wrapped key material, never plaintext.
- `decrypt_mode` (`server_side` | `client_side`) tells the client which shape it
  received and must match the advertised capability.
- `aead_algorithm` is `XCHACHA20_POLY1305` or `AES_256_GCM`. The canonical
  `wrapped_dek` and its current wrapping-KEK pointer live on the `secret_deks`
  row; the envelope references the DEK by `dek_id` and records the **frozen**
  `dek_version` (see [key-hierarchy.md](key-hierarchy.md)). `key_material` MAY be
  returned inline (as above) or fetched once via a key-material endpoint keyed by
  `kek_id` and cached.
- `aad_context` is reconstructed strictly from stored envelope fields and binds
  ciphertext to its logical slot (`realm_id`, `secret_id`, field, `owner_kind`,
  `domain`); the `encryption_context` binds the KMS KEK unwrap to
  `realm_id` + purpose + `kek_id`/`key_version`.
- Reveals include `audit_event_id` when audit is available and `expires_at` when
  the reveal carries a TTL or lease. The server-mediated path emits
  `secret.reveal` with the `server_side_decrypt` flag (see
  [audit-retention.md](audit-retention.md)).

### TOTP Code Result

Used by `totp code` and `POST /v1/totp/{secret_id}:code`. An explicit,
audited sealed-plane value-returning op. The TOTP seed (`totp-seed`) is
high-value sealed material and is **never** returned by `totp code`.

```json
{
  "secret": {
    "id": "sec_123",
    "name": "github/builder"
  },
  "totp_id": "totp_123",
  "code": "123456",
  "digits": 6,
  "period_seconds": 30,
  "remaining_seconds": 18,
  "expires_at": "2026-06-26T18:00:30Z",
  "decrypt_mode": "server_side",
  "audit_event_id": "aud_124"
}
```

Rules:

- `totp code` returns the current generated code only; the underlying seed is
  never returned here and is never embedded, recalled, in the self-digest, or
  plaintext-exported. The seed is revealed only through the more privileged
  `totp:enroll` path (see [totp-2fa.md](totp-2fa.md)).
- `decrypt_mode` mirrors the [Secret Reveal Result](#secret-reveal-result)
  custody modes; the server-mediated path emits `totp.code` with the
  `server_side_decrypt` flag.

### Password Generate Result

Used by `password generate` and `POST /v1/password:generate`. Generation does not
touch the sealed store unless the caller also writes the value into a secret.

```json
{
  "values": [
    {
      "kind": "password",
      "value": "generated-password",
      "length": 32
    }
  ]
}
```

Rules:

- When multiple values are requested, return all generated values in `values`.
- Generated values are sensitive output: they must not appear in errors, logs,
  or audit records. Persisting a generated value into a secret follows the normal
  sealed-plane write path.

### Secret Grant

Used by `secret grant`/`secret revoke` and grant list/show. A grant is the
explicit, audited, optionally field-scoped and optionally expiring authorization
that lets a named agent or group access a secret it does not own. Grants are
authorization checks, not separate crypto boundaries.

```json
{
  "id": "grt_123",
  "realm_id": "realm_123",
  "secret": {
    "id": "sec_123",
    "name": "github/builder"
  },
  "owner": {
    "kind": "agent",
    "agent_id": "agent_123",
    "agent_name": "browser-agent"
  },
  "grantee": {
    "kind": "agent",
    "agent_id": "agent_456",
    "agent_name": "archivist"
  },
  "permissions": ["secret:show", "secret:reveal", "totp:code"],
  "fields": ["password"],
  "created_by": {
    "kind": "operator",
    "id": "opr_123",
    "name": "scott"
  },
  "reason": "CI runner needs the deploy token",
  "expires_at": "2026-07-26T18:00:00Z",
  "created_at": "2026-06-26T18:00:00Z",
  "updated_at": "2026-06-26T18:00:00Z"
}
```

Rules:

- `grantee.kind` is `agent` or `group`. Cross-owner access is **never** a
  default; it exists only through a grant or a realm role (see
  [authorization-and-roles.md](authorization-and-roles.md)). Secrets are not
  subject to the open-plane cross-agent read/curate/forget verbs in
  [Policy](#policy).
- `permissions` is a subset of the sealed-plane scopes (`secret:show`,
  `secret:reveal`, `secret:update`, `totp:code`).
- `fields` optionally narrows the grant to specific fields; absent or `null`
  means the whole secret.
- `expires_at: null` means the grant does not expire. Grant and revoke emit
  `secret.grant` / `secret.revoke` audit events and carry the `reason` on the
  audit record.
