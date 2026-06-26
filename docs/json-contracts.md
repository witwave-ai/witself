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
- Keep identity data readable by default while redacting `sensitive` facts and
  `sensitive`-flagged memory content in list/scan responses.
- Prevent memory content, fact values, message bodies/payloads, embedding
  vectors, and raw tokens from appearing in errors, logs, or audit records.
- Keep managed-service and self-hosted responses aligned while leaving room for
  a local mock/development backend.

Unlike Witpass, Witself protects the *integrity and authenticity* of identity
data rather than the *confidentiality* of secret material. There is no reveal
ceremony: an authorized read of a single record returns its value directly.
Only `sensitive` facts and `sensitive`-flagged memory content are redacted in
list/scan output, and that redaction is a PII/display posture, not an
encryption boundary.

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
  [Recall Result](#recall-result)).
- `retryable` indicates whether retrying the identical request may later
  succeed. Transient codes (`backend_unavailable`, `rate_limited`) are
  `retryable: true`; hard conditions (`limit_exceeded`, `access_denied`,
  `auth_failed`, `not_found`, `conflict`, `unsupported_operation`) are
  `retryable: false`.
- `rate_limited` responses should include `details.retry_after` in seconds when
  a wait is known; the HTTP API should also send a `Retry-After` header.
- Memory content, fact values, message bodies/payloads, embedding vectors, and
  raw tokens must never appear in `error.message` or `error.details`.

## Error Codes

JSON error codes should align with CLI exit-code categories.

| Code | CLI Exit | Meaning |
|---|---:|---|
| `internal_error` | 1 | Unexpected internal error. |
| `usage_error` | 2 | Invalid command, flag, input, or request shape. |
| `access_denied` | 3 | Authenticated principal lacks permission, or no policy allows the cross-agent access. |
| `auth_failed` | 4 | Authentication or local unlock failed. |
| `not_found` | 5 | Memory, fact, policy, group, message, agent, realm, token, or event not found. |
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
  `fact_`, `grp_`, `pol_`, `msg_`, `tok_`, `aud_`.
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

Used by `witself capabilities` and `/v1/capabilities`.

```json
{
  "backend": {
    "kind": "self-hosted",
    "version": "v0.1.0",
    "api_version": "v1",
    "endpoint": "https://witself.internal.example.com"
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
    }
  }
}
```

Rules:

- `backend.kind` values are `managed`, `self-hosted`, and `local`.
- `semantic_recall` reports the active embedding provider, model, and vector
  dimensionality. `degraded: true` means the provider is unavailable or disabled
  and recall has fallen back to keyword/tag/kind/time ranking (see
  [Recall Result](#recall-result)). The embedding-provider abstraction is
  tracked in [memory-model.md](memory-model.md).
- `field_level_encryption` reflects optional encryption of `sensitive` fact
  values; it is a capability, not the default (see [storage.md](storage.md)).
- `limits` keys use the canonical metered-dimension names from
  [billing-and-limits.md](billing-and-limits.md) (e.g. `active_agent`,
  `stored_memory`, `memory_recall`), so they join directly to
  `/v1/billing/usage` items and the `limit_dimension` metric label.
- `features` values must include at least `supported`.
- Unsupported features should include a stable `reason` when known.
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
  "conversation_id": "cnv_123",
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
  ordering.
- `direction` selects a mailbox view in `message list` and the messaging API.
  Its value set is `inbox` or `outbox`; there is no `all` value in v0. The MCP
  `direction` enum references this set, and the CLI selector maps to it.

Inter-agent messaging is tracked in
[inter-agent-messaging.md](inter-agent-messaging.md).

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
  "next_command": "witself billing usage --realm prod --show-limits"
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
  "next_command": "witself billing crypto status hps_123 --watch",
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
  "next_command": "witself billing crypto status hps_123 --watch"
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
  "action": "crossagent.forgotten",
  "actor": {
    "kind": "agent",
    "id": "agent_123",
    "name": "browser-agent"
  },
  "target": {
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
  "timestamp": "2026-06-26T18:00:00Z",
  "metadata": {
    "kind": "episodic",
    "tags": ["staging"]
  }
}
```

Rules:

- Audit `action` values are stable dotted event names from
  [requirements.md](requirements.md), for example `memory.added`,
  `memory.adjusted`, `memory.recalled`, `memory.forgotten`, `memory.restored`,
  `memory.deleted`, `fact.set`, `fact.primary_changed`, `fact.deleted`,
  `policy.created`, `policy.deleted`, `policy.access_denied`, `crossagent.read`,
  `crossagent.contributed`, `crossagent.curated`, `crossagent.forgotten`,
  `group.created`, `group.member_added`, `group.member_removed`,
  `message.sent`, `message.delivered`, `message.read`, `message.acked`,
  `identity.exported`, and `identity.imported`.
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

Used by `witself reference parse`/`witself reference resolve` and the
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
