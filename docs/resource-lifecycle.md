# Witself Resource Lifecycle

Status: draft. This document tracks the current operator-managed create and
delete/revoke behavior.

## Current Matrix

| Resource | Create | Delete / revoke | Safety behavior |
|---|---|---|---|
| Realm | `POST /v1/realms`, `witself realm create` | `DELETE /v1/realms/{realm}`, `witself realm delete --yes` | Soft-deletes only empty realms. Live agents must be deleted first. |
| Agent | `POST /v1/realms/{realm}/agents`, `witself agent create` | `DELETE /v1/realms/{realm}/agents/{agent}`, `witself agent delete --yes` | Soft-deletes the agent and immediately revokes live agent tokens. |
| Agent token | `POST /v1/agents/{agent}/tokens`, `witself token create --agent` | `POST /v1/tokens/{token_id}:revoke`, `witself token revoke --token TOKEN_ID --yes` | Revokes by server-side token ID. Raw token values are never needed for revoke. |
| Operator | `POST /v1/operators`, `witself operator create --name` | `DELETE /v1/operators/{operator}`, `witself operator delete --yes` | Soft-deletes non-root operators and revokes all live tokens bound to them. Self, root, and last-operator deletes are rejected. |
| Operator token | `POST /v1/operators/self/tokens`, `witself token create --operator` | `POST /v1/tokens/{token_id}:revoke`, `witself token revoke --token TOKEN_ID --yes` | Revokes by server-side token ID. Token IDs are visible in operator listings. |

Soft-deleted resources are omitted from ordinary list results but retained as
tombstones for future audit/history surfaces. Token revocation sets
`consumed_at`, which immediately excludes the credential from authentication.

## Audit Notes

The audit subsystem is not implemented yet. These lifecycle paths are all
security-relevant and should emit stable audit events when audit lands:

- `realm.deleted`
- `agent.deleted`
- `token.revoked`
- `operator.created`
- `operator.deleted`

Raw token values, token hashes, memory content, fact values, message bodies, and
sealed-plane material must never appear in audit payloads.

## Open Follow-Ups

- Add individual token list/show commands outside the operator listing.
- Add hard-delete or restore flows once retention windows are formalized.
- Add `--reason` and `--dry-run` to destructive CLI operations when audit is
  available.
