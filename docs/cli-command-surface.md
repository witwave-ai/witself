# Witself CLI Command Surface

Status: draft. This document describes the proposed human and agent CLI
contract before implementation.

## Design Goals

- The CLI must be pleasant for humans and deterministic for agents.
- Managed-service account administration should be CLI-native, not a thin
  wrapper around a browser dashboard.
- Humans and AI assistants should be able to use the same command surface,
  subject to the same authentication, authorization, audit, and redaction rules.
- Every command that returns data should support `--json`.
- Identity recall should be semantic by default: `memory recall` is an
  embedding-backed similarity search blended with keyword, tag, kind, and time
  filters, while `memory read` and `fact get` stay deterministic.
- Witself spans two planes: the open plane (memories + facts) is plainly
  readable identity data; the sealed plane (secrets + TOTP) is reveal-gated,
  envelope-encrypted credential material. Secret-revealing actions
  (`secret reveal`, `totp code`) are explicit, audited, and never satisfied by
  ordinary list/show output. Sealed-plane values are never embedded, recalled,
  placed in the self-digest, or included in plaintext export; the sealed plane
  is described in [secret-model.md](secret-model.md), [totp-2fa.md](totp-2fa.md),
  [encryption-model.md](encryption-model.md), and
  [key-hierarchy.md](key-hierarchy.md).
- Operators should be able to scan and manage all agent-owned and group-owned
  secrets in a realm from the same CLI, without revealing sensitive values.
- Secret values should be accepted from files or stdin whenever possible, not
  only from flags. The canonical secret primitive is named fields, each marked
  sensitive or non-sensitive; only secret `name` and `description` are required.
- Secret references and runtime injection (`witself run`) should provide an
  alternative to printing sealed-plane values directly to stdout.
- Cross-agent access is default-deny and policy-driven; integrity-impacting
  actions (`memory forget`/`restore`/`delete`, cross-agent `curate`/`forget`,
  `fact delete`, `--primary` promotion) are explicit and auditable.
- Operators should be able to scan and manage all agent-owned and group-owned
  memories and facts in a realm from the same CLI, without switching to a
  separate admin console.
- Commands should target the managed service by default, support self-hosted
  backend endpoints through the same API contract, and allow a local
  mock/development backend for tests, demos, and early implementation.
- Memory content, fact values, and message bodies should be accepted from files
  or stdin whenever possible, not only from flags.
- First-class structured/plaintext identity export and round-trippable import
  are headline features, not forbidden ones (the deliberate inverse of Witpass's
  encrypted-only export stance).
- The same core service should eventually power the CLI and MCP server.

## Global Flags

These flags are available on every command unless a command explicitly says
otherwise.

| Flag | Description |
|---|---|
| `--json` | Emit machine-readable JSON. |
| `--quiet` | Suppress non-essential human output. |
| `--config PATH` | Use a specific config file. |
| `--profile NAME` | Use a named local or managed-service profile. |
| `--endpoint URL` | Specific remote managed, staging, private, localhost, or self-hosted API endpoint for this command. |
| `--realm NAME_OR_ID` | Select the realm scope. |
| `--store-file PATH` | Use a specific local development store file in local mock/development mode. |
| `--token-file PATH` | Read the managed-service or agent token from a file for this command. Highest auth precedence. |
| `--agent NAME_OR_ID` | Operator/admin only: act as or create for a specific agent when the caller has permission. |
| `--no-input` | Fail instead of prompting. Required for fully unattended runs. |

## Environment Variables

| Variable | Description |
|---|---|
| `WITSELF_TOKEN` | Managed-service or agent token. Convenient, but least safe. |
| `WITSELF_TOKEN_FILE` | File containing a managed-service or agent token. Preferred for unattended runs. |
| `WITSELF_ENDPOINT` | Default specific remote API endpoint for managed, staging, private, localhost, or self-hosted use. |
| `WITSELF_PROFILE` | Default profile name. |
| `WITSELF_CONFIG` | Default config path. |
| `WITSELF_REALM` | Default realm name or ID. |
| `WITSELF_STORE_FILE` | Default local development store file path. |
| `WITSELF_EMBEDDINGS_PROVIDER` | Embedding provider for semantic recall: `voyage` (default), `openai`, or `local-dev`. |
| `WITSELF_EMBEDDINGS_MODEL` | Embedding model within the selected provider. |

Token file conventions:

- Production: set `WITSELF_TOKEN_FILE` to an explicit secret mount, such as
  `/run/secrets/witself-agent-token`.
- Local development fallback:
  `${XDG_CONFIG_HOME:-~/.config}/witself/tokens/<profile-or-agent>.token`.
- Token files created by Witself should be owner-readable and owner-writable
  only on platforms that support POSIX-style permissions, such as mode `0600`.
- Directories created by Witself for token files should be owner-only on
  platforms that support POSIX-style permissions, such as mode `0700`.

Agent tokens are bound to a realm and a named agent, either by embedded token
claims or server-side token lookup. Agent identity comes from the token, never
from a caller-supplied agent name. `--agent` can select an acting agent only
when the authenticated token/operator credential is allowed to do so; it is not
proof of identity by itself. This is load-bearing for cross-agent access and
inter-agent messaging: the actor and message sender are derived server-side from
the token.

V0 token files contain plain token text. Token metadata is available through
token commands, not embedded in the token file.

Ephemeral agents, such as Kubernetes pods, should normally start with only
`WITSELF_TOKEN_FILE` mapped to a mounted secret in managed-service mode. The
token identifies the durable named agent; restarting the pod does not create a
new Witself agent.

Endpoint selection precedence is global `--endpoint`, `WITSELF_ENDPOINT`, stored
profile endpoint, then the default managed Witself Cloud endpoint.

Authentication source precedence is global `--token-file`,
`WITSELF_TOKEN_FILE`, `WITSELF_TOKEN`, then stored local profile auth. Raw token
flags should stay command-specific or testing-only rather than becoming part of
the broad global surface.

The stable JSON response contract for `--json` output is tracked in
[json-contracts.md](json-contracts.md).

## Human Output Rules

By default, human output should be readable and cautious:

- `memory list` and `memory recall` summarize matches with metadata and a
  content preview; `sensitive` memory content is redacted in list/recall output
  by default.
- `memory read` returns one memory's content for an authorized read; there is no
  reveal ceremony. The "no reveal ceremony / plainly readable" posture applies to
  the open plane (memories and facts) only; sealed-plane secrets are reveal-gated.
- `fact list` returns fact names and values; `sensitive` fact values are
  redacted in list/scan output by default.
- `fact get NAME` returns the one deterministic value for an authorized read.
- `secret list`, `secret scan`, and `secret show` never reveal sensitive field
  values; `secret show` may display non-sensitive fields such as `url` or
  `username`. Sealed-plane values are only returned by the explicit, audited
  `secret reveal NAME FIELD` and `totp code` commands (the reveal ceremony).
  Unlike a `sensitive` fact, which is merely redacted in summaries and readable
  on a direct authorized `fact get`, a secret field is never readable without a
  reveal.
- `whoami` and profile views surface `primary` facts first as identity anchors.
- Mutating commands summarize what changed. Every mutation returns a
  deterministic, human-readable `echo` line (such as
  `Remembered fact display-name=Atlas`, `Added mem_123 (kind=profile, salience=0.6)`,
  or `Merged into mem_120 (duplicate)`) so a human or agent can self-verify and
  chain the result. The `echo` field is part of the Mutation Result contract in
  [json-contracts.md](json-contracts.md).
- Destructive or integrity-impacting commands require confirmation unless
  `--yes` is provided, and cross-agent or operator mutations require `--reason`.

## Service Administration Rules

Account, realm, billing, payment, usage, and support commands are first-class
CLI workflows. A browser may appear only for external provider requirements such
as payment authorization, hosted checkout, identity verification, or other
regulated approval flows.

Rules:

- Every service-administration command should support `--json`.
- Commands whose availability may differ by backend should check
  `witself capabilities` or the backend capabilities API and fail with
  `unsupported_operation` when unsupported.
- Commands that create external sessions should return a stable session ID,
  status, URL when applicable, and the next command to check or resume the flow.
- Risky service mutations should support `--dry-run` where practical. Dry runs
  should validate inputs, permissions, conflicts, quotas, and provider
  prerequisites, then return planned changes without writing state, generating
  tokens, creating provider sessions, charging payment methods, sending
  messages, or sending support/customer notifications.
- Destructive, billing-impacting, or support-sensitive actions should require
  `--yes` for unattended execution.
- Sensitive operator actions and cross-agent mutations should accept `--reason`
  and produce an audit event when audit is available.
- AI-assisted account management must use the same commands and credentials as
  human operators; it should not require a separate AI-only backend.

## Exit Codes

| Code | Meaning |
|---|---|
| `0` | Success. |
| `1` | Internal error. |
| `2` | Usage error. |
| `3` | Access denied by policy or permissions. |
| `4` | Authentication or unlock failure. |
| `5` | Memory, fact, secret, TOTP enrollment, policy, group, message, agent, profile, or realm not found. |
| `6` | Conflict, such as already exists or stale version. |
| `7` | Backend unavailable, network failure, rate limit, or hard usage cap. Distinguish retryable cases (`backend_unavailable`, `rate_limited`) from the non-retryable hard cap (`limit_exceeded`) via `error.code`/`retryable` in `--json`. |
| `8` | Store integrity or corruption failure. |
| `9` | Unsupported operation for the current backend. |

## Command Tree

```text
witself
  version
  capabilities
  whoami
  auth login|logout|status|whoami
  setup
  account create|show|update|members|invite|remove|set-role|export|close
  realm create|list|show|use|rename|delete|members|init|status|export|import
  billing show|usage|limits|plans|subscribe|subscription|payment-methods|sessions|crypto|invoices
  support create|list|show|comment|close
  remember
  self show
  session start|end
  memory add|adjust|read|recall|list|forget|restore|delete|consolidate
  digest emit
  ingest
  bootstrap-instructions
  fact set|get|list|delete
  password generate
  secret create|show|list|scan|reveal|update|rename|copy|archive|restore|delete|grant|revoke
  run
  totp enroll|code|show|delete
  policy create|list|show|delete|test
  group create|list|show|add-member|remove-member|delete
  message send|list|read|ack
  reference parse|resolve
  agent create|list|show|rename|copy|disable|enable|delete
  token create|list|revoke|rotate
  audit list|show
  export
  import
  mcp serve|tools
  config get|set|list|unset
  completion
```

## `witself version`

Print the CLI version and build metadata.

```sh
witself version
witself version --json
```

Flags:

| Flag | Description |
|---|---|
| `--short` | Print only the version string in human mode. |

## `witself capabilities`

Show the active backend kind, version, supported features, unavailable features,
limits, embedding provider, and endpoint context.

This command is the CLI view of the backend capability contract. It lets a
human, script, or AI assistant determine whether the current endpoint supports
managed billing, payment flows, crypto payments, support tickets, audit, the
active embedding provider/model, whether semantic recall is degraded, or
local-development-only behavior before running a command.

```sh
witself capabilities
witself capabilities --endpoint https://witself.internal.example.com
witself capabilities --feature messaging --include-reasons
witself capabilities --feature semantic_recall --include-reasons
```

Flags:

| Flag | Description |
|---|---|
| `--feature FEATURE` | Show one feature flag. Repeatable. |
| `--include-reasons` | Include unsupported or degraded feature reasons, including degraded semantic recall. |
| `--include-limits` | Include plan, rate, or backend limits when visible. |
| `--include-embeddings` | Include the active embedding provider, model, and vector dimensionality. |
| `--check` | Exit non-zero if any requested feature is unsupported. |

Self-hosted or local backends may report billing, hosted payment flows, crypto
payment flows, Witself support tickets, or managed-only features as unsupported.
If the embedding provider is unavailable, capabilities should report semantic
recall as degraded so callers know `memory recall` will fall back to
keyword/tag/kind/time ranking. Commands should use the same capability data when
returning `unsupported_operation`.

## `witself whoami`

Show the current realm, profile, and principal, with `primary` facts surfaced
first as identity anchors. This is the top-level convenience alias for
`auth whoami`.

Flags:

| Flag | Description |
|---|---|
| `--show-permissions` | Include effective scopes/permissions. |
| `--show-facts` | Include the agent's `primary` facts. Default: true for agent tokens. |

## `witself auth`

Manage authentication to the managed service or active local profile.

### `witself auth login`

Authenticate a human or configure an unattended token.

```sh
witself auth login
witself auth login --device-code
witself auth login --no-browser
witself auth login --token-file /run/secrets/witself-token
witself auth login --token-file ~/.config/witself/tokens/browser-agent.token
witself auth login --endpoint https://api.witself.com
witself auth login --endpoint https://witself.internal.example.com --bootstrap-token-file ./bootstrap.token
```

Flags:

| Flag | Description |
|---|---|
| `--endpoint URL` | Specific remote managed, staging, private, localhost, or self-hosted API endpoint. |
| `--browser` | Use the browser login flow. Default when available for managed endpoints. |
| `--device-code` | Use a device-code flow for headless environments. |
| `--no-browser` | Do not open a browser; print the verification URL or next command. |
| `--token VALUE` | Token value. Least safe; useful for tests. |
| `--token-file PATH` | Read token from a file. |
| `--bootstrap-token-file PATH` | Use a one-time self-hosted first-operator bootstrap token. |
| `--local` | Configure a local-only profile. |
| `--profile NAME` | Save credentials under a named profile. |

Managed human login should be CLI-initiated but provider-hosted. The CLI should
not collect raw account passwords. When no credential store is available, stored
operator auth should use an owner-only local auth file where supported. The
operator auth model is tracked in [operator-auth.md](operator-auth.md).

### `witself auth logout`

Remove local authentication material for a profile.

Flags:

| Flag | Description |
|---|---|
| `--all` | Remove authentication material for all profiles. |
| `--yes` | Skip confirmation. |

### `witself auth status`

Show whether the current profile is authenticated or locally usable.

Flags:

| Flag | Description |
|---|---|
| `--show-source` | Show whether auth came from env, token file, config, or prompt. |

### `witself auth whoami`

Show the current realm, profile, and principal.

Flags:

| Flag | Description |
|---|---|
| `--show-permissions` | Include effective permissions. |
| `--show-facts` | Include the agent's `primary` facts. |

## `witself setup`

Create or connect everything needed for agents to start using Witself managed
service or a self-hosted Witself backend. This is the end-to-end bootstrap path
for a fresh operator.

Setup target defaults:

- `witself setup` with no target flag uses managed Witself Cloud.
- `witself setup --managed` is an explicit form of the managed-cloud default.
- `witself setup --endpoint URL` targets a specific remote backend, usually a
  self-hosted deployment, staging endpoint, or private managed endpoint.
- `witself setup --local` targets local mock/development mode only.

The command should discover remote backend kind and capabilities through
`/v1/capabilities` before creating account, realm, agent, token, billing, or
support resources.

`witself setup` should be safe for humans and deterministic for automation. In
interactive mode it can prompt for missing values. With `--no-input`, all
required choices must be supplied through flags.

Setup should be idempotent by default for account, realm, and agent resources.
When names match visible resources, setup should select and report those
resources instead of creating duplicates.

Token handling is deliberately stricter. If no usable token exists for a
requested agent/token destination, setup may create and write the first token.
If an existing token file or active token is detected, setup must not silently
reuse or rotate it. The caller must choose `--reuse-existing-token` or
`--rotate-existing-tokens`; otherwise setup should fail with a deterministic
conflict that explains the required choice.

Setup-created token files should be written atomically with owner-only
permissions where the operating system supports them. A normal token create
must refuse to overwrite an existing token file path. In reuse mode, setup
should verify the existing token file and leave it unchanged. In rotation mode,
setup may replace the existing token file only after the caller explicitly
requested rotation and the replacement token has been issued successfully.

Expected outcomes:

- Account is created or selected.
- Realm is created or selected.
- Named agents are created or selected.
- Missing agent token files are issued and written.
- Existing agent token files or active tokens are reused or rotated only when
  explicitly requested.
- Setup-created token files are written to operator-selected paths with
  owner-only permissions where supported.
- Output includes the `WITSELF_TOKEN_FILE` mapping each managed-service agent
  should use.
- Optional Kubernetes Secret manifests or commands are emitted.
- The command verifies that each token authenticates as the expected agent.

```sh
witself setup \
  --account "Acme Agents" \
  --email ops@example.com \
  --realm prod \
  --agent browser-agent \
  --agent release-agent \
  --token-dir ./witself-tokens
```

Local mock/development setup is available for early implementation, demos, and
tests, but it is not the production deployment path. It uses the `local-dev`
embedding provider so semantic recall can be exercised offline:

```sh
witself setup --local \
  --realm dev \
  --store-file ./witself.store.json \
  --agent browser-agent \
  --token-out browser-agent=./tokens/browser-agent.token
```

Managed-service setup can also emit runtime delivery artifacts such as
Kubernetes Secret manifests:

```sh
witself setup \
  --realm prod \
  --agent browser-agent \
  --token-out browser-agent=./tokens/browser-agent.token \
  --kubernetes-secret \
  --namespace agents \
  --out ./witself-agent-secret.yaml
```

Flags:

| Flag | Description |
|---|---|
| `--managed` | Use the default managed Witself Cloud endpoint. This is the product path and the default when no target flag is supplied. Mutually exclusive with `--endpoint` and `--local`. |
| `--local` | Use local mock/development mode. Not a production mode. Mutually exclusive with `--managed` and `--endpoint`. |
| `--endpoint URL` | Specific remote managed, staging, private, or self-hosted API endpoint. Mutually exclusive with `--managed` and `--local`. |
| `--account NAME` | Customer account display name to create or select. |
| `--email EMAIL` | Primary operator/customer email for account bootstrap. |
| `--billing-email EMAIL` | Billing contact email. |
| `--realm NAME` | Realm to create or select. Required for unattended setup. |
| `--agent NAME` | Agent to create or ensure exists. Repeatable. |
| `--token-dir PATH` | Directory where agent token files should be written. |
| `--token-out AGENT=PATH` | Write a specific agent token to a specific path. Repeatable. |
| `--no-reuse-existing-resources` | Fail if the account, realm, or agent already exists instead of selecting the matching resource. |
| `--reuse-existing-token` | Reuse an existing token file or active token when it verifies as the requested agent. Does not overwrite token files. Mutually exclusive with `--rotate-existing-tokens`. |
| `--rotate-existing-tokens` | Rotate existing agent tokens during setup and atomically write replacement token files. This is the explicit overwrite path for existing token files. Mutually exclusive with `--reuse-existing-token`. |
| `--store-file PATH` | Local development store file for `--local` mode. |
| `--plan PLAN` | Managed-service plan to select when creating a subscription. |
| `--promo-code CODE` | Apply a promotional code during plan or checkout setup when supported. |
| `--checkout` | Create a hosted checkout flow when billing setup is required. |
| `--open` | Open the hosted checkout URL when possible. |
| `--no-open` | Print the hosted checkout URL without opening a browser. |
| `--kubernetes-secret` | Emit a Kubernetes Secret manifest for issued token files. |
| `--namespace NAME` | Kubernetes namespace for emitted manifests. |
| `--secret-name NAME` | Kubernetes Secret name. Default should derive from the realm. |
| `--out PATH` | Write setup output, manifest, or instructions to a file. |
| `--write-agents-md` | Install the Witself teaching stanza (see [`witself bootstrap-instructions`](#witself-bootstrap-instructions)) into the project AGENTS.md so file-loaded agents learn to call Witself. |
| `--dry-run` | Show planned resources and token destinations without creating them. |
| `--verify` | Verify issued tokens. Default: true. |
| `--no-verify` | Skip token verification. |

When `--checkout` is used with `--json`, output should include setup progress
plus the hosted provider session result from
[json-contracts.md](json-contracts.md).

## `witself account`

Manage the Witself managed-service customer account from the CLI. The CLI is the
primary control plane for customer account details, human operators/admins,
billing ownership, support identity, and account exports.

### `witself account create`

Create a managed-service customer account from the CLI.

Flags:

| Flag | Description |
|---|---|
| `--display-name TEXT` | Customer account display name. |
| `--legal-name TEXT` | Legal customer name for billing records. |
| `--email EMAIL` | Primary account/operator email. |
| `--billing-email EMAIL` | Billing contact email. |
| `--support-email EMAIL` | Support contact email. |
| `--address-field KEY=VALUE` | Set a billing/customer address field. Repeatable. |
| `--tax-id TEXT` | Tax identifier when required. |
| `--plan PLAN` | Initial plan. |
| `--promo-code CODE` | Apply a promotional code during initial plan or checkout setup when supported. |
| `--checkout` | Create a hosted checkout flow if payment setup is required. |
| `--open` | Open the hosted checkout URL when possible. |
| `--no-open` | Print the hosted checkout URL without opening a browser. |
| `--profile NAME` | Save the created account under a named profile. |
| `--dry-run` | Validate inputs and show planned account, profile, and billing setup without creating anything. |

When `--checkout` is used with `--json`, output should use the hosted provider
session result from [json-contracts.md](json-contracts.md) for the payment setup
portion of account creation.

### `witself account show`

Show the current managed-service customer account.

Flags:

| Flag | Description |
|---|---|
| `--show-usage` | Include usage summary. |
| `--show-billing` | Include billing summary when allowed. |

### `witself account update`

Update customer account details.

Flags:

| Flag | Description |
|---|---|
| `--display-name TEXT` | Customer account display name. |
| `--legal-name TEXT` | Legal customer name for billing records. |
| `--email EMAIL` | Primary account email. |
| `--billing-email EMAIL` | Billing contact email. |
| `--support-email EMAIL` | Support contact email. |
| `--address-field KEY=VALUE` | Set a billing/customer address field. Repeatable. |
| `--tax-id TEXT` | Tax identifier when required. |
| `--dry-run` | Show planned account changes without applying them. |
| `--reason TEXT` | Audit reason. |

### `witself account members`

List human operators and admins for the customer account.

Flags:

| Flag | Description |
|---|---|
| `--role ROLE` | Filter by role. |
| `--include-disabled` | Include disabled members. |

### `witself account invite EMAIL`

Invite a human operator/admin to the customer account.

Flags:

| Flag | Description |
|---|---|
| `--role ROLE` | Account role to grant. |
| `--realm NAME_OR_ID` | Optionally grant access to a specific realm. Repeatable. |
| `--expires-at TIMESTAMP` | Invite expiration. |
| `--dry-run` | Validate and preview the invite without sending it. |
| `--reason TEXT` | Audit reason. |

### `witself account remove PRINCIPAL`

Remove a human operator/admin from the customer account.

Flags:

| Flag | Description |
|---|---|
| `--transfer-to PRINCIPAL` | Transfer ownership of resources when required. |
| `--dry-run` | Show planned removal and transfer effects without applying them. |
| `--yes` | Skip confirmation. |
| `--reason TEXT` | Audit reason. |

### `witself account set-role PRINCIPAL ROLE`

Change a human operator/admin's account-level role.

Flags:

| Flag | Description |
|---|---|
| `--dry-run` | Show planned role change without applying it. |
| `--reason TEXT` | Audit reason. |

### `witself account export`

Export managed-service account metadata when policy allows.

Flags:

| Flag | Description |
|---|---|
| `--out PATH` | Write export to a file. |
| `--include-billing` | Include billing metadata when allowed. |
| `--include-support` | Include support-ticket metadata when allowed. |
| `--format json` | Export format. Initial format is JSON. |

### `witself account close`

Close the managed-service customer account when policy allows.

Flags:

| Flag | Description |
|---|---|
| `--effective-at TIMESTAMP` | Schedule closure for a future time when supported. |
| `--dry-run` | Show closure impact, blockers, and scheduled effects without closing the account. |
| `--reason TEXT` | Audit reason. |
| `--yes` | Skip confirmation. |

## `witself realm`

Manage realms. A realm is the operator-owned container for a group of named
agents. It holds agents, agent-owned and group-owned memories and facts,
security groups, policies, messages, grants, audit records, and usage limits.
The realm is the rename of the Witpass "vault". In local mock/development mode,
the realm is backed by a local development store file.

### `witself realm create NAME`

Create a realm.

Flags:

| Flag | Description |
|---|---|
| `--display-name TEXT` | Human-readable display name. |
| `--description TEXT` | Realm description. |
| `--local` | Create a local mock/development realm. |
| `--store-file PATH` | Local development store file path for `--local` mode. |
| `--dry-run` | Validate and show planned realm creation without creating it. |

### `witself realm list`

List realms visible to the current principal.

Flags:

| Flag | Description |
|---|---|
| `--include-disabled` | Include disabled or archived realms when allowed. |

### `witself realm show NAME_OR_ID`

Show realm metadata, usage summary, and counts.

Flags:

| Flag | Description |
|---|---|
| `--show-usage` | Include billing or usage counters when available. |
| `--show-agents` | Include agent summary counts. |
| `--show-groups` | Include security-group summary counts. |

### `witself realm use NAME_OR_ID`

Set the default realm for the current profile.

Flags:

| Flag | Description |
|---|---|
| `--profile NAME` | Set the default realm on a named profile. |

### `witself realm rename NAME_OR_ID NEW_NAME`

Rename a realm. Operator/admin only.

Flags:

| Flag | Description |
|---|---|
| `--dry-run` | Show planned rename without applying it. |
| `--reason TEXT` | Audit reason. |

### `witself realm delete NAME_OR_ID`

Archive or delete a realm. Operator/admin only.

Flags:

| Flag | Description |
|---|---|
| `--archive` | Archive instead of permanently deleting. Default for managed service. |
| `--permanent` | Permanently delete when allowed. |
| `--dry-run` | Show deletion impact, blockers, and affected agents/memories/facts/groups without deleting anything. |
| `--yes` | Skip confirmation. |
| `--reason TEXT` | Audit reason. |

### `witself realm members NAME_OR_ID`

List human operators, admins, and agent principals in a realm.

Flags:

| Flag | Description |
|---|---|
| `--agents` | Include agent principals. Default: true. |
| `--humans` | Include human operators/admins. Default: true. |
| `--role ROLE` | Filter by role. |

### `witself realm members add REALM_NAME_OR_ID PRINCIPAL`

Add a human operator/admin principal or agent principal to a realm.

Flags:

| Flag | Description |
|---|---|
| `--role ROLE` | Role to grant. |
| `--dry-run` | Validate and preview the member addition without changing access. |
| `--reason TEXT` | Audit reason. |

### `witself realm members remove REALM_NAME_OR_ID PRINCIPAL`

Remove a human operator/admin principal or agent principal from a realm.

Flags:

| Flag | Description |
|---|---|
| `--dry-run` | Show planned member removal without changing access. |
| `--yes` | Skip confirmation. |
| `--reason TEXT` | Audit reason. |

### `witself realm members set-role REALM_NAME_OR_ID PRINCIPAL ROLE`

Change a realm member's role.

Flags:

| Flag | Description |
|---|---|
| `--dry-run` | Show planned role change without applying it. |
| `--reason TEXT` | Audit reason. |

### `witself realm init`

Create the local development store for local mock/development mode. This is the
local backend bootstrap command; `realm create --local` may call it internally.
When the store is empty, this also creates the first local operator/admin
context. The local backend uses the `local-dev` embedding provider so semantic
recall works offline.

```sh
witself realm init
witself realm init --store-file ~/.witself/store.json
witself realm init --operator ops
```

Flags:

| Flag | Description |
|---|---|
| `--store-file PATH` | Initialize the local store at a specific path. |
| `--operator NAME` | Name for the first local operator/admin context. |
| `--force` | Overwrite an empty or test store. Should fail on non-empty stores. |

### `witself realm status`

Show local mock/development store file path, backend type, and item counts.

Flags:

| Flag | Description |
|---|---|
| `--check-integrity` | Verify the store can be opened and decoded. |

### `witself realm export`

Export a realm for backup or migration. Unlike Witpass vault export, Witself
realm export is structured/plaintext and round-trippable by default; it can
include all agents' memories and facts, edit history, policies, security-group
membership, and group-owned identity data when the operator is authorized. See
also the top-level [`witself export`](#witself-export) for single-agent self
export. Backup, export, and recovery are tracked in
[backup-and-recovery.md](backup-and-recovery.md).

Flags:

| Flag | Description |
|---|---|
| `--out PATH` | Write export to a file or directory. |
| `--format json` | Export format. Initial format is JSON using the `witself.v0` schema. |
| `--include-history` | Include memory/fact edit history. Default: true. |
| `--include-policies` | Include realm policies. Operator only. Default: true. |
| `--include-groups` | Include security groups and group-owned identity data. Operator only. Default: true. |
| `--include-audit` | Include realm audit records when available. |
| `--no-sensitive` | Exclude `sensitive` memories and facts from the export. |
| `--reason TEXT` | Required audit reason when exporting `sensitive` records. |

### `witself realm import`

Import a realm backup or export.

Imports should validate schema version, integrity metadata, migration
compatibility, identity reference resolution, and target realm conflicts before
writing state. Dangling `witself://` references are reported, not silently
dropped.

Flags:

| Flag | Description |
|---|---|
| `--in PATH` | Read import from a file or directory. |
| `--merge` | Merge into the current realm. |
| `--replace` | Replace the current realm. |
| `--remap-agent FROM=TO` | Remap an owning agent on import. Repeatable. |
| `--dry-run` | Validate the import and show planned created/updated/conflicting records without writing changes. |
| `--yes` | Skip confirmation for replacement. |
| `--reason TEXT` | Audit reason. |

## `witself billing`

Manage managed-service billing, usage, plans, payment methods, crypto payment
flows, and invoices from the CLI. Billing attaches at the account level, and
usage rolls up by realm.

V0 billing is plan-based and usage-aware. The managed service should meter
important dimensions internally, but initial pricing should be plan-tier based
rather than raw per-call billing.

Payment workflows are CLI-driven, but Witself should not accept raw card numbers
or high-risk payment details directly in flags, environment variables, config
files, or logs. Payment setup should use provider tokens or CLI-initiated hosted
checkout/setup flows when required.

When a command starts a hosted provider flow, `--json` output should include a
stable `session_id`, current `status`, provider URL when applicable,
`expires_at` when known, and a `next_command` that can resume, poll, or inspect
the flow. This applies to checkout, payment-method setup, crypto payment, and
other provider approval sessions.

Crypto payments should be an optional payment rail alongside cards, bank
payments, invoices, credits, and other traditional methods. The CLI may create
quotes, initiate hosted wallet checkout, and poll payment status, but it must
not ask for seed phrases, private keys, raw wallet credentials, or direct wallet
custody. Supported assets and networks should come from the configured payment
provider rather than being hard-coded into product assumptions. There is no
Witself utility token.

### `witself billing show`

Show billing status for the current account.

Flags:

| Flag | Description |
|---|---|
| `--show-payment-method` | Include redacted default payment method summary. |
| `--show-plan` | Include current plan. |
| `--show-balance` | Include current balance when available. |

### `witself billing usage`

Show usage and metering data.

V0 usage dimensions should include `active_agent`, `stored_memory`,
`stored_fact`, `memory_recall`, `memory_read`, `memory_write`,
`embedding_operation`, `vector_storage_byte`, `crossagent_access`,
`security_group`, `message_sent`, `message_delivered`, `audit_event`, and
`api_request`.

Flags:

| Flag | Description |
|---|---|
| `--since TIMESTAMP_OR_DURATION` | Usage window start. |
| `--until TIMESTAMP` | Usage window end. |
| `--realm NAME_OR_ID` | Filter by realm. Repeatable. |
| `--agent NAME_OR_ID` | Filter by agent. Repeatable. |
| `--dimension DIMENSION` | Filter by usage dimension. Repeatable. |
| `--group-by FIELD` | Group usage by `realm`, `agent`, `dimension`, or `day`. |
| `--show-limits` | Include limit and overage context for each dimension. |

### `witself billing limits`

Show current plan limits and rate limits.

Limit output should show plan, dimensions, used quantity, included quantity,
soft limit, hard limit, overage behavior (`warn`, `throttle`, or `block`), and
reset window when applicable.

Flags:

| Flag | Description |
|---|---|
| `--realm NAME_OR_ID` | Show realm-specific limits when applicable. |
| `--dimension DIMENSION` | Show one limit dimension. Repeatable. |
| `--include-usage` | Include current usage next to each limit. |

### `witself billing plans`

List available plans.

Flags:

| Flag | Description |
|---|---|
| `--include-enterprise` | Include enterprise/contact-sales plans when available. |

### `witself billing subscribe PLAN`

Start or change a subscription.

Flags:

| Flag | Description |
|---|---|
| `--realm NAME_OR_ID` | Attach the plan to a realm when plans are realm-scoped. |
| `--checkout` | Create a hosted checkout flow and print/open the URL. |
| `--promo-code CODE` | Apply a promotional code to the subscription or checkout session when supported. |
| `--payment-method ID` | Use an existing payment method. |
| `--open` | Open the hosted checkout URL when possible. |
| `--no-open` | Print the hosted checkout URL without opening a browser. |
| `--dry-run` | Show planned subscription change, billing impact, and provider-session need without changing billing. |
| `--yes` | Confirm subscription changes when allowed. |
| `--reason TEXT` | Audit reason for the subscription change. |

When `--checkout` is used with `--json`, output should use the hosted provider
session result from [json-contracts.md](json-contracts.md).

### `witself billing subscription`

Show or manage subscription state.

Flags:

| Flag | Description |
|---|---|
| `--cancel` | Cancel subscription at period end when allowed. |
| `--resume` | Resume a cancellable subscription. |
| `--plan PLAN` | Change to a different plan. |
| `--dry-run` | Show planned subscription state changes without applying them. |
| `--yes` | Confirm the change. |
| `--reason TEXT` | Audit reason for subscription changes. |

### `witself billing payment-methods`

List payment methods.

Flags:

| Flag | Description |
|---|---|
| `--include-expired` | Include expired payment methods. |

### `witself billing payment-methods add`

Add a payment method through a provider-safe flow.

Flags:

| Flag | Description |
|---|---|
| `--setup` | Create a hosted payment-method setup flow and print/open the URL. |
| `--provider-token TOKEN` | Attach a payment-provider token. Do not pass raw card data. |
| `--type card|bank|crypto` | Preferred payment-method type when the provider supports multiple setup flows. |
| `--set-default` | Make the new payment method the default. |
| `--open` | Open the hosted setup URL when possible. |
| `--no-open` | Print the hosted setup URL without opening a browser. |
| `--dry-run` | Validate setup inputs and show whether a provider session would be created without attaching a payment method. |
| `--reason TEXT` | Audit reason for adding the payment method. |

When `--setup` is used with `--json`, output should use the hosted provider
session result from [json-contracts.md](json-contracts.md).

### `witself billing sessions show SESSION_ID`

Show the status of a hosted provider session created by checkout,
payment-method setup, identity verification, crypto payment, or another provider
approval flow.

```sh
witself billing sessions show hps_123
witself billing sessions show hps_123 --watch --timeout 5m
```

Flags:

| Flag | Description |
|---|---|
| `--watch` | Poll until the session reaches a terminal state. |
| `--timeout DURATION` | Maximum time to watch. |
| `--show-provider` | Include redacted provider-specific metadata. |

With `--json`, output should use the hosted provider session result from
[json-contracts.md](json-contracts.md).

### `witself billing payment-methods remove PAYMENT_METHOD_ID`

Remove a payment method.

Flags:

| Flag | Description |
|---|---|
| `--dry-run` | Show planned removal without detaching the payment method. |
| `--yes` | Skip confirmation. |
| `--reason TEXT` | Audit reason for removing the payment method. |

### `witself billing payment-methods set-default PAYMENT_METHOD_ID`

Set the default payment method.

Flags:

| Flag | Description |
|---|---|
| `--dry-run` | Show planned default-payment-method change without applying it. |
| `--reason TEXT` | Audit reason for changing the default payment method. |

### `witself billing crypto`

Manage provider-mediated crypto payment flows. These commands are for payment
rails such as stablecoins or ETH alongside traditional payments; they are not a
Witself utility-token surface.

Crypto payments should normally use hosted provider flows. The CLI can print or
open a checkout URL, return a payment quote, and poll status. It should never
accept wallet seed phrases, private keys, or raw wallet credentials.

Potential assets include stablecoins such as USDC or USDT, plus ETH when the
configured provider supports it. The provider determines the exact supported
assets, networks, limits, confirmation behavior, settlement currency, refunds,
and recurring-payment capabilities.

### `witself billing crypto quote`

Create or preview a crypto payment quote for an invoice, subscription setup, or
account balance.

Flags:

| Flag | Description |
|---|---|
| `--invoice INVOICE_ID` | Quote payment for a specific invoice. |
| `--subscription SUBSCRIPTION_ID` | Quote payment for a subscription flow when supported. |
| `--amount DECIMAL` | Explicit payment amount when not tied to an invoice. |
| `--currency CURRENCY` | Fiat pricing currency, such as `usd`. |
| `--asset ASSET` | Preferred source asset, such as `USDC`, `USDT`, or `ETH`. |
| `--network NETWORK` | Preferred network, such as `ethereum`, `base`, `polygon`, or `solana`. |
| `--provider NAME` | Payment provider to use when more than one is configured. |
| `--expires-in DURATION` | Requested quote lifetime when the provider allows it. |

### `witself billing crypto checkout`

Start a provider-hosted crypto checkout or payment setup flow.

Flags:

| Flag | Description |
|---|---|
| `--invoice INVOICE_ID` | Pay a specific invoice. |
| `--subscription SUBSCRIPTION_ID` | Start or update a subscription payment flow. |
| `--quote QUOTE_ID` | Use an existing quote. |
| `--asset ASSET` | Preferred source asset. |
| `--network NETWORK` | Preferred network. |
| `--open` | Open the provider-hosted checkout URL in a browser when possible. |
| `--no-open` | Print the checkout URL without opening a browser. |
| `--return-url URL` | Return URL for hosted provider flows when supported. |
| `--dry-run` | Validate checkout inputs and show the planned provider flow without creating a checkout session. |
| `--reason TEXT` | Audit reason for starting the crypto payment flow. |

With `--json`, output should use the hosted provider session result from
[json-contracts.md](json-contracts.md), with crypto payment metadata included
when available.

### `witself billing crypto status SESSION_ID`

Show the status of a crypto payment flow by its hosted provider session id
(`hps_...`), matching the `next_command` poll examples. This reads
`GET /v1/billing/sessions/{session_id}`; the related quote (`cpq_`) and charge
(`cps_`) ids are surfaced inside the session result rather than fetched through
their own routes. Returns the hosted provider session / crypto charge shape from
[json-contracts.md](json-contracts.md).

Flags:

| Flag | Description |
|---|---|
| `--watch` | Poll until the payment reaches a terminal state. |
| `--timeout DURATION` | Maximum time to watch. |
| `--show-provider` | Include provider-specific redacted metadata. |

### `witself billing invoices`

List invoices.

Flags:

| Flag | Description |
|---|---|
| `--since TIMESTAMP_OR_DURATION` | Invoice window start. |
| `--until TIMESTAMP` | Invoice window end. |
| `--status STATUS` | Filter by invoice status. |

### `witself billing invoices show INVOICE_ID`

Show one invoice.

Flags:

| Flag | Description |
|---|---|
| `--format json|text` | Output format. |

### `witself billing invoices download INVOICE_ID`

Download an invoice PDF or JSON representation.

Flags:

| Flag | Description |
|---|---|
| `--out PATH` | Write invoice to a file. |
| `--format pdf|json` | Download format. |

## `witself support`

Create and manage support tickets from the CLI.

Support commands must not attach memory content, fact values, message bodies or
payloads, raw tokens, or private keys. Diagnostic bundles should redact
sensitive material by default.

### `witself support create`

Create a support ticket.

Flags:

| Flag | Description |
|---|---|
| `--subject TEXT` | Ticket subject. |
| `--body TEXT` | Ticket body. |
| `--body-file PATH` | Read ticket body from a file. |
| `--priority low|normal|high|urgent` | Ticket priority. |
| `--category CATEGORY` | Ticket category. |
| `--realm NAME_OR_ID` | Related realm. |
| `--agent NAME_OR_ID` | Related agent. |
| `--attach PATH` | Attach a file. Repeatable. Sensitive material must be redacted. |
| `--include-diagnostics` | Attach a redacted diagnostic bundle. |
| `--dry-run` | Validate ticket content, attachments, and redaction checks without creating the ticket. |

### `witself support list`

List support tickets.

Flags:

| Flag | Description |
|---|---|
| `--status STATUS` | Filter by ticket status. |
| `--priority PRIORITY` | Filter by priority. |
| `--limit N` | Maximum number of rows. |

### `witself support show TICKET_ID`

Show one support ticket.

Flags:

| Flag | Description |
|---|---|
| `--include-comments` | Include comments. |
| `--include-attachments` | Include attachment metadata, not attachment contents. |

### `witself support comment TICKET_ID`

Add a comment to a support ticket.

Flags:

| Flag | Description |
|---|---|
| `--body TEXT` | Comment body. |
| `--body-file PATH` | Read comment body from a file. |
| `--attach PATH` | Attach a redacted file. Repeatable. |
| `--dry-run` | Validate the comment and attachments without posting it. |

### `witself support close TICKET_ID`

Close a support ticket.

Flags:

| Flag | Description |
|---|---|
| `--reason TEXT` | Close reason. |
| `--dry-run` | Show planned close action without closing the ticket. |
| `--yes` | Skip confirmation. |

## `witself remember`

Quick-add self-knowledge. `remember` is the tested primary capture path for
agents and humans: it auto-routes a single piece of text to either a fact or a
memory so callers do not have to choose. A clear name→value assertion (such as
`package manager is pnpm`, `email = a@b.com`, or `display name is Atlas`) upserts
a fact (idempotent by name); anything else is added as a verbatim memory with
dedup/supersede. It never bypasses validation, limits, or the dedup contract.
`fact set` and `memory add` remain for explicit control; `remember` is a
first-class, tested command, not a thin alias. The auto-routing rules are tracked
in [context-hydration.md](context-hydration.md).

```sh
witself remember "package manager is pnpm"
witself remember "Operator prefers terse summaries." --scope self
witself remember "shared on-call rotation starts Mondays" --scope group --reason "team context"
```

Flags:

| Flag | Description |
|---|---|
| `--scope self\|project\|group` | Routing scope for the captured fact/memory. Default: `self`. |
| `--sensitive` | Mark the created fact or memory as `sensitive` (PII); content/value is redacted in list/recall by default. |
| `--reason TEXT` | Audit reason, required for cross-context (e.g. group) capture. |
| `--json` | Emit the created/updated resource, its `kind` (`fact` or `memory`), the `echo` line, and `duplicate_of` when merged. |

When `remember` adds a memory that near-duplicates an existing one, it returns
the existing `mem_` id and a `memory_duplicate`/`memory_merged` warning plus
`duplicate_of`, rather than silently creating a near-dup. `remember` does not emit
its own audit event; it routes to the existing `memory.added` / `fact.created` /
`fact.updated` events.

## `witself self show`

Show the always-loaded self-digest: a bounded, session-start view of who the
agent is. The digest lists `primary` facts first, then the top-N salient memories
(blended salience + recency), then a one-line index of kinds, tags, and counts.
It is cheap and never requires the embedding provider, so it works even when
semantic recall is degraded. This is the MCP/CLI analogue of an auto-loaded
CLAUDE.md head. The digest shape, cap, and `elided` behavior are tracked in
[context-hydration.md](context-hydration.md).

The digest has a hard byte/line cap (default ~8 KiB / ~200 lines, configurable
via `--max-bytes`). When capped, output sets `elided=true` and points to
`memory recall`; it is never silently truncated.

```sh
witself self show
witself self show --salient-limit 5 --json
witself self show --no-salient --max-bytes 4096
```

Flags:

| Flag | Description |
|---|---|
| `--no-facts` | Omit `primary` facts from the digest. |
| `--no-salient` | Omit salient memories from the digest. |
| `--salient-limit N` | Maximum salient memories to include. Default: `10`. |
| `--max-bytes N` | Hard cap on digest size; sets `elided=true` when the cap is hit. |
| `--json` | Emit `{ identity, primary_facts[], salient_memories[], index, elided }`. |

## `witself session`

Bootstrap and flush long-running, multi-session work. `session start` hydrates
identity, open goals, and last progress in one round-trip; `session end` persists
a progress memory and updates the open goals. This pairs with the
assume-interruption / flush-before-long-operations habit so resuming a task is a
single call rather than a list-and-recall crawl. The session model is tracked in
[context-hydration.md](context-hydration.md).

### `witself session start`

Hydrate identity, open goals, and last progress for a new working session in one
call. Reads only; never requires the embedding provider.

```sh
witself session start
witself session start --json
```

Flags:

| Flag | Description |
|---|---|
| `--json` | Emit `{ identity, open_goals[], last_progress }`. |

### `witself session end`

Persist a session progress memory (kind `session`) and update open goals.

```sh
witself session end --summary "Shipped v0.3; rollback documented." \
  --open-goals "write release notes,monitor error rate"
```

Flags:

| Flag | Description |
|---|---|
| `--summary TEXT` | Required progress summary stored as a `session`-kind memory. |
| `--open-goals "a,b"` | Comma-separated open goals to carry into the next session. |
| `--reason TEXT` | Audit reason. |
| `--json` | Emit `{ saved, progress_memory_id }` plus the `echo` line. |

`session start` emits a `session.started` audit event and `session end` emits
`session.ended`.

## `witself memory`

Manage memories. A memory is free-form self-content owned by an agent (or, in
the group case, by a security group). It is one of the two first-class identity
payload types alongside facts. Memories are addressed by `id` (`mem_...`),
recalled semantically, and filtered by metadata; they are not name-unique. The
memory model and recall ranking are tracked in
[memory-model.md](memory-model.md).

A memory carries `content`, a `kind` convention (such as `episodic`, `semantic`,
`profile`, or `note`), `tags[]`, `source`, `salience`, `links[]` of `witself://`
references, an optional `sensitive` marker, timestamps, and versioned edit
history.

Memory targeting rules:

- Default target: the current token-bound agent.
- `--owner-agent NAME_OR_ID`: operator/admin, or an agent acting under a policy
  that permits the verb, targets a specific owning agent.
- `--owner-group NAME_OR_ID`: targets a group-owned (collective) memory.
- `--owner-agent` and `--owner-group` are mutually exclusive.
- Cross-agent and group-owned reads, writes, and destructive actions are
  default-deny and require an authorizing policy or operator override.
  Cross-agent `curate`/`forget`/`delete` require `--reason`, support
  `--dry-run`, and require confirmation unless `--yes`. Cross-agent access is
  tracked in
  [access-policy.md](access-policy.md).

Operator/cross-agent examples:

```sh
witself memory list --all-agents
witself memory recall "incident postmortem" --owner-agent archivist
witself memory adjust mem_01H... --owner-agent archivist --add-tag reviewed --reason "operator cleanup"
witself memory forget mem_01H... --owner-agent archivist --reason "duplicate" --dry-run
```

### `witself memory add`

Add a memory. The creating agent is the owner unless an authorized caller
targets another agent with `--owner-agent` (under a `contribute` policy or operator
override) or a group with `--owner-group`.

```sh
witself memory add --content "Deployed v0.3 to prod; rollback plan documented." \
  --kind episodic --tag deploy --tag prod

witself memory add --content-file ./postmortem.md \
  --kind semantic --salience 0.8 --link witself://fact/home-region

echo "Operator prefers terse summaries." | witself memory add --content-stdin --kind profile
```

Flags:

| Flag | Description |
|---|---|
| `--content TEXT` | Memory content from a flag. |
| `--content-file PATH` | Read memory content from a file. |
| `--content-stdin` | Read memory content from stdin. |
| `--kind KIND` | Memory kind convention, such as `episodic`, `semantic`, `profile`, or `note`. Unknown kinds are allowed. |
| `--tag TAG` | Add a tag. Repeatable. |
| `--source TEXT` | Provenance, such as `self`, an agent name, a message id, or an import batch id. |
| `--salience FLOAT` | Importance/weight hint used in recall ranking. |
| `--link REF` | Add a `witself://` link to another memory or fact. Repeatable. |
| `--sensitive` | Mark the memory as `sensitive` (PII); content is redacted in list/recall by default. |
| `--owner-agent NAME_OR_ID` | Contribute the memory to a specific agent's store. Policy-gated or operator only. |
| `--owner-group NAME_OR_ID` | Create the memory as group-owned (collective). |
| `--reason TEXT` | Audit reason when contributing to another agent or group. |

### `witself memory adjust ID`

Adjust an existing memory. Adjust appends a new version to the versioned edit
history; prior versions are retained.

Flags:

| Flag | Description |
|---|---|
| `--owner-agent NAME_OR_ID` | Operator/admin or `curate`-policy caller: adjust a memory owned by a specific agent. |
| `--owner-group NAME_OR_ID` | Adjust a group-owned memory. |
| `--content TEXT` | Replace memory content from a flag. |
| `--content-file PATH` | Replace memory content from a file. |
| `--content-stdin` | Replace memory content from stdin. |
| `--kind KIND` | Change the memory kind. |
| `--salience FLOAT` | Change the salience hint. |
| `--source TEXT` | Change provenance. |
| `--add-tag TAG` | Add a tag. Repeatable. |
| `--remove-tag TAG` | Remove a tag. Repeatable. |
| `--add-link REF` | Add a `witself://` link. Repeatable. |
| `--remove-link REF` | Remove a `witself://` link. Repeatable. |
| `--sensitive` | Mark as `sensitive`. |
| `--not-sensitive` | Clear the `sensitive` marker. |
| `--dry-run` | Show planned changes and the resulting version without writing. |
| `--reason TEXT` | Audit reason. Required for cross-agent or group-owned curation. |

### `witself memory read ID`

Read one memory deterministically by `id`. Reading updates `last_accessed_at`.
This is the deterministic counterpart to semantic `recall`; it does not require
the embedding provider.

Flags:

| Flag | Description |
|---|---|
| `--owner-agent NAME_OR_ID` | Operator/admin or `read`-policy caller: read a memory owned by a specific agent. |
| `--owner-group NAME_OR_ID` | Read a group-owned memory. |
| `--version VERSION` | Read a historical version from edit history when supported. |
| `--include-history` | Include the full versioned edit history alongside the current version. |
| `--show-links` | Resolve and show `witself://` links when authorized. |
| `--reason TEXT` | Audit reason for cross-agent reads. |

### `witself memory recall QUERY`

Recall memories by semantic similarity (the core Witself differentiator). Recall
performs an embedding-backed vector similarity search over the caller's
accessible memories, then blends in keyword, tag, kind, and time filters with
hybrid ranking (similarity, lexical match, tag/kind match, recency, and
salience). If the embedding provider is unavailable, recall degrades
deterministically to keyword/tag/kind/time ranking and the result reports the
degraded state.

```sh
witself memory recall "what do I know about the prod outage"
witself memory recall "deploy preferences" --kind profile --limit 5 --json
witself memory recall "shared runbooks" --owner-group shared-context
```

Flags:

| Flag | Description |
|---|---|
| `--kind KIND` | Restrict recall to a memory kind. Repeatable. |
| `--tag TAG` | Restrict recall to a tag. Repeatable. |
| `--since TIMESTAMP_OR_DURATION` | Only recall memories created/accessed since this point. |
| `--until TIMESTAMP` | Recall window end. |
| `--limit N` | Maximum number of ranked results. |
| `--min-score FLOAT` | Minimum blended relevance score. |
| `--owner-agent NAME_OR_ID` | Recall over a specific agent's memories. `read`-policy or operator. Metered as a cross-agent access. |
| `--owner-group NAME_OR_ID` | Recall over a group's shared memories. |
| `--all-agents` | Operator/admin only: recall across every agent in the realm. |
| `--show-scores` | Include per-result similarity/recency/salience contributions. |
| `--keyword-only` | Force keyword/tag/kind/time ranking without the embedding provider. |
| `--reason TEXT` | Audit reason for cross-agent recall. |

### `witself memory list`

List memories visible to the current principal with metadata and filters.
`sensitive` content is redacted by default.

```sh
witself memory list
witself memory list --kind episodic --tag deploy
witself memory list --all-agents
witself memory list --owner-group shared-context
```

Flags:

| Flag | Description |
|---|---|
| `--kind KIND` | Filter by memory kind. Repeatable. |
| `--tag TAG` | Filter by tag. Repeatable. |
| `--source TEXT` | Filter by provenance. |
| `--since TIMESTAMP_OR_DURATION` | Filter by created/updated window start. |
| `--until TIMESTAMP` | Filter window end. |
| `--owner-agent NAME_OR_ID` | Filter by the agent that owns the memory. |
| `--owner-group NAME_OR_ID` | Filter by group-owned memories. |
| `--all-agents` | Operator/admin only: include memories owned by every agent in the realm. |
| `--include-sensitive` | Include `sensitive` memories in results. Content remains redacted unless individually read. |
| `--include-forgotten` | Include soft-deleted (tombstoned) memories within the retention window. |
| `--limit N` | Maximum number of rows. |
| `--cursor TOKEN` | Continue from a pagination cursor. |

### `witself memory forget ID`

Forget a memory. Forget is a soft delete (tombstone), reversible within the
retention window. This is the default destructive path; it is audited and
reversible via `memory restore`.

Flags:

| Flag | Description |
|---|---|
| `--owner-agent NAME_OR_ID` | Operator/admin or `forget`-policy caller: forget a memory owned by a specific agent. |
| `--owner-group NAME_OR_ID` | Forget a group-owned memory. |
| `--dry-run` | Show the forget plan and retention window without tombstoning the memory. |
| `--yes` | Skip confirmation. |
| `--reason TEXT` | Audit reason. Required for cross-agent or group-owned forgets. |

### `witself memory restore ID`

Restore a forgotten (tombstoned) memory within the retention window.

Flags:

| Flag | Description |
|---|---|
| `--owner-agent NAME_OR_ID` | Operator/admin or `forget`-policy caller: restore a memory owned by a specific agent. |
| `--owner-group NAME_OR_ID` | Restore a group-owned memory. |
| `--reason TEXT` | Audit reason for cross-agent or group-owned restores. |

### `witself memory delete ID`

Hard-delete a memory. Explicit, guarded, audited, and irreversible. Prefer
`memory forget` for normal removal. Cross-agent hard delete is a
further-guarded step beyond the soft-delete default.

Flags:

| Flag | Description |
|---|---|
| `--owner-agent NAME_OR_ID` | Operator/admin or `manage-others` caller: hard-delete a memory owned by a specific agent. |
| `--owner-group NAME_OR_ID` | Hard-delete a group-owned memory. |
| `--permanent` | Confirm permanent, irreversible deletion. |
| `--dry-run` | Show deletion impact, links, and blockers without deleting the memory. |
| `--yes` | Skip confirmation. |
| `--reason TEXT` | Required audit reason for deletion. |

### `witself memory consolidate`

Consolidate (garbage-collect) the memory store: merge near-duplicate memories,
supersede stale ones, surface (never auto-resolve) conflicting facts, and trim
the digest/index. Consolidation respects `source` provenance and never silently
overwrites human-, operator-, or import-authored records; such conflicts are
surfaced for a human/agent to resolve. This addresses the primary failure mode of
an append-only store. The selection and merge rules are tracked in
[memory-model.md](memory-model.md).

`--dry-run` defaults to true, so consolidation previews planned merges,
supersessions, and conflicts without writing until the caller opts in. The
command is excluded in `--read-only` MCP mode and emits a `memory.consolidated`
audit event when it writes.

```sh
witself memory consolidate
witself memory consolidate --dry-run=false --reason "post-session cleanup"
witself memory consolidate --scope self --json
```

Flags:

| Flag | Description |
|---|---|
| `--dry-run` | Preview merges/supersessions/conflicts without writing. Default: true. |
| `--scope SCOPE` | Restrict consolidation to a scope. |
| `--reason TEXT` | Audit reason; required when writing. |
| `--json` | Emit `{ merged[], superseded[], conflicts[], trimmed_index }`. |

## `witself digest emit`

Render the self-digest as a CLAUDE.md / AGENTS.md fragment for file-load agent
harnesses. This is the outbound half of the two-way file bridge: it makes
Witself-backed identity available for free to runtimes that auto-load AGENTS.md
or CLAUDE.md, with provenance HTML comments (witself-generated marker plus
timestamp) so the emitted block is recognizable and round-trippable. The emit
formats and provenance markers are tracked in
[context-hydration.md](context-hydration.md).

`digest emit` reads only and never requires the embedding provider. It optionally
emits a `self.digest.emitted` audit event.

```sh
witself digest emit --format agents-md -o ./AGENTS.md
witself digest emit --format claude-md --max-bytes 4096
witself digest emit --format markdown
```

Flags:

| Flag | Description |
|---|---|
| `--format claude-md\|agents-md\|markdown` | Output fragment format. |
| `--max-bytes N` | Hard cap on the emitted fragment size. |
| `-o, --out PATH` | Write the fragment to a file instead of stdout. |

## `witself ingest`

Ingest existing agent context files into Witself: the inbound half of the file
bridge. `ingest` parses CLAUDE.md / AGENTS.md / GEMINI.md, routing kv-shaped
lines to facts (upsert) and prose paragraphs to memories, tagging everything
`source=import:<file>`. Dedup/upsert prevents re-import duplication, so the same
file can be re-ingested safely. This makes Witself a good citizen of the
AGENTS.md ecosystem rather than a competitor; it composes the existing fact and
memory create paths and adds no new resource. The parser rules are tracked in
[context-hydration.md](context-hydration.md).

```sh
witself ingest ./AGENTS.md ./CLAUDE.md
witself ingest ./docs/GEMINI.md --source-label legacy --dry-run
```

Flags:

| Flag | Description |
|---|---|
| `--source-label L` | Override the `source=import:<file>` label applied to ingested records. |
| `--dry-run` | Show planned created/updated/deduplicated facts and memories without writing. |
| `--json` | Emit the per-file ingest plan and results, including `echo` lines and `duplicate_of` on merges. |

`ingest` emits `fact.imported` and `memory.imported` audit events. It is excluded
in `--read-only` MCP mode.

## `witself bootstrap-instructions`

Print the paste-able teaching stanza that installs the Witself usage habit
(recall-before-act, write-after-learn, consolidate-when-noisy) into an agent's
file-loaded context. Witself is a service an agent must be taught to call, so the
stanza is the file-ecosystem half of the teaching layer, mirroring the MCP server
`instructions` text. The canonical stanza is pinned in
[context-hydration.md](context-hydration.md).

```sh
witself bootstrap-instructions
witself bootstrap-instructions --format agents-md
witself bootstrap-instructions --format text
```

Flags:

| Flag | Description |
|---|---|
| `--format agents-md\|claude-md\|text` | Output format for the teaching stanza. |

To install the stanza directly into a project's AGENTS.md as part of bootstrap,
`witself setup --write-agents-md` writes it during setup (see
[`witself setup`](#witself-setup)).

## `witself fact`

Manage facts. A fact is a name→value pair: the canonical, queryable identity
card for an agent (or, in the group case, a security group). Facts are deterministic
and addressable by name, in contrast to memories. The facts model is tracked in
[facts-model.md](facts-model.md).

Fact names are unique within the owning agent (or group); different agents may
reuse the same name because ownership disambiguates them. Facts are ordinary
readable identity data; only `sensitive` facts are redacted by default in
list/scan output. There is no secret-style reveal ceremony.

Fact targeting rules:

- Default target: the current token-bound agent.
- `--owner-agent NAME_OR_ID`: operator/admin, or an agent acting under a policy
  that permits the verb, targets a specific owning agent.
- `--owner-group NAME_OR_ID`: targets a group-owned fact.
- `--owner-agent` and `--owner-group` are mutually exclusive.

### `witself fact set NAME VALUE`

Create or update a fact (upsert by name within the owner). `--primary` marks the
canonical value of the fact's logical kind and atomically demotes any prior
primary of the same kind for the same owner. Value may also be supplied from a
file or stdin.

```sh
witself fact set display-name "Browser Agent"
witself fact set email builder@example.com --primary --format email
witself fact set aws/account-id 123456789012 --format string
witself fact set notes --value-file ./notes.txt
```

Flags:

| Flag | Description |
|---|---|
| `--value TEXT` | Fact value from a flag. |
| `--value-file PATH` | Read the fact value from a file. |
| `--value-stdin` | Read the fact value from stdin. |
| `--primary` | Promote this fact to primary for its logical kind. Demotes the prior primary. |
| `--not-primary` | Clear the primary flag for this fact. |
| `--sensitive` | Mark the fact `sensitive` (PII); value is redacted in list/scan by default. |
| `--not-sensitive` | Clear the `sensitive` marker. |
| `--format HINT` | Type hint such as `string`, `email`, `url`, `date`, or `number`. |
| `--source TEXT` | Provenance. |
| `--owner-agent NAME_OR_ID` | Set a fact for a specific agent. `contribute`/`curate` policy or operator. |
| `--owner-group NAME_OR_ID` | Set a group-owned fact. |
| `--dry-run` | Show the planned upsert and any primary demotion without writing. |
| `--reason TEXT` | Audit reason. Required for cross-agent or group-owned writes and for primary promotion across agents. |

### `witself fact get NAME`

Get one fact deterministically by name. An authorized read returns the value,
including for `sensitive` facts; there is no reveal ceremony.

```sh
witself fact get email
witself fact get email --owner-agent archivist --reason "operator lookup"
```

Flags:

| Flag | Description |
|---|---|
| `--owner-agent NAME_OR_ID` | Operator/admin or `read`-policy caller: get a fact owned by a specific agent. |
| `--owner-group NAME_OR_ID` | Get a group-owned fact. |
| `--version VERSION` | Read a historical version from edit history when supported. |
| `--reason TEXT` | Audit reason for cross-agent reads. |

### `witself fact list`

List facts visible to the current principal. `primary` facts are surfaced first
as identity anchors. `sensitive` values are redacted by default.

```sh
witself fact list
witself fact list --primary-only
witself fact list --all-agents
witself fact list --owner-group shared-context
```

Flags:

| Flag | Description |
|---|---|
| `--prefix TEXT` | Filter by name prefix or namespace, such as `aws/`. |
| `--primary-only` | Show only `primary` facts. |
| `--format HINT` | Filter by format hint. |
| `--owner-agent NAME_OR_ID` | Filter by the agent that owns the fact. |
| `--owner-group NAME_OR_ID` | Filter by group-owned facts. |
| `--all-agents` | Operator/admin only: include facts owned by every agent in the realm. |
| `--include-sensitive` | Include `sensitive` facts; values remain redacted unless individually read. |
| `--limit N` | Maximum number of rows. |
| `--cursor TOKEN` | Continue from a pagination cursor. |

### `witself fact delete NAME`

Delete a fact (guarded, audited).

Flags:

| Flag | Description |
|---|---|
| `--owner-agent NAME_OR_ID` | Operator/admin, or a `curate`/`forget`-policy caller holding `memory:manage-others` + `fact:delete`: delete a fact owned by a specific agent. |
| `--owner-group NAME_OR_ID` | Delete a group-owned fact. |
| `--dry-run` | Show deletion impact, including loss of a `primary` anchor, without deleting the fact. |
| `--yes` | Skip confirmation. |
| `--reason TEXT` | Audit reason. Required for cross-agent or group-owned deletes. |

## `witself password generate`

Generate a password or passphrase. This is a sealed-plane utility used most often
to populate a sensitive secret field (see `secret create --generate-sensitive`),
but it also works standalone. Generated values are returned to the caller and are
not stored unless written into a secret.

```sh
witself password generate
witself password generate --length 40 --no-ambiguous
witself password generate --words 5
```

Flags:

| Flag | Description |
|---|---|
| `--length N` | Password length. Default: `32`. |
| `--lower` | Include lowercase letters. Default: true. |
| `--upper` | Include uppercase letters. Default: true. |
| `--digits` | Include digits. Default: true. |
| `--symbols` | Include symbols. Default: true. |
| `--no-lower` | Disable lowercase letters. |
| `--no-upper` | Disable uppercase letters. |
| `--no-digits` | Disable digits. |
| `--no-symbols` | Disable symbols. |
| `--no-ambiguous` | Avoid ambiguous characters such as `O`, `0`, `I`, `l`, and `1`. |
| `--words N` | Generate a human-readable passphrase with N words instead of a random-character password. |
| `--separator TEXT` | Separator for passphrase mode. Default: `-`. |
| `--count N` | Generate N values. Default: `1`. |

## `witself secret`

Manage stored secrets: the sealed plane of Witself. A secret can be a login, API
key, token, private key, certificate bundle, connection string, or arbitrary
structured secret. Secrets are envelope-encrypted (CMK → per-realm KEK →
per-secret/field DEK); the encryption and key model are tracked in
[encryption-model.md](encryption-model.md) and
[key-hierarchy.md](key-hierarchy.md), and the secret data model and lifecycle in
[secret-model.md](secret-model.md).

Secrets are modeled as flat named fields. Each field is marked sensitive or
non-sensitive. Secret templates (`login`, `api-key`, `ssh-key`, `certificate`,
`env`, `generic`) define useful conventions, but they do not limit the fields a
secret may contain. Field size limits and attachments are tracked in
[secret-size-and-attachments.md](secret-size-and-attachments.md).

Sealed-plane carve-outs: secret values are never embedded, never returned by
semantic recall, never placed in the self-digest, and never included in the
plaintext export; secret backup is encrypted-only. Sensitive field values are
only returned by the explicit, audited `secret reveal` ceremony.

Secret targeting rules:

- Default target: the current token-bound agent.
- `--owner-agent NAME_OR_ID`: operator/admin only, targets a specific owning
  agent.
- `--group NAME_OR_ID`: targets a group-owned secret (the unified replacement for
  the former vault-shared scope). Operator/admin or `group:manage`.
- `--owner-agent` and `--group` are mutually exclusive.
- Cross-agent secret access is governed by grants and realm roles in
  [authorization-and-roles.md](authorization-and-roles.md), not by the open-plane
  cross-agent identity policy engine; secrets are not subject to the open
  cross-agent read/curate/forget verbs.

Operator/admin multi-agent examples:

```sh
witself secret scan --all-agents
witself secret show github/builder --owner-agent browser-agent
witself secret update github/builder --owner-agent browser-agent --field url=https://github.com/login
witself secret grant github/builder --owner-agent browser-agent --agent release-agent --read --reveal password
witself totp code github/builder --owner-agent browser-agent --reason "operator recovery"
```

### `witself secret create NAME`

Create a secret. `NAME` must be unique for the owning agent (or group) inside the
current realm. Different agents may use the same `NAME`.

```sh
witself secret create github/builder \
  --description "GitHub login for browser-agent" \
  --template login \
  --field username=builder@example.com \
  --field url=https://github.com/login \
  --generate-sensitive password

witself secret create stripe/test \
  --description "Stripe test API key" \
  --template api-key \
  --sensitive-stdin api-key

witself secret create deploy/tls \
  --description "TLS certificate bundle for deploy agent" \
  --template certificate \
  --field-file cert=./tls.crt \
  --sensitive-file private-key=./tls.key
```

Flags:

| Flag | Description |
|---|---|
| `--description TEXT` | Required human-readable description. Prompts in interactive mode if omitted. |
| `--template TEMPLATE` | Optional field convention, such as `login`, `api-key`, `ssh-key`, `certificate`, `env`, or `generic`. Default: `generic`. |
| `--field KEY=VALUE` | Add a non-sensitive named field from a flag. Repeatable. |
| `--field-file KEY=PATH` | Add a non-sensitive field from a file. Repeatable. |
| `--field-stdin KEY` | Read one non-sensitive field from stdin. |
| `--sensitive KEY=VALUE` | Add a sensitive named field from a flag. Least safe. Repeatable. |
| `--sensitive-file KEY=PATH` | Add a sensitive field from a file. Repeatable. |
| `--sensitive-stdin KEY` | Read one sensitive field from stdin. |
| `--generate-sensitive KEY` | Generate a password and store it in a named sensitive field. Common value for login templates: `password`. |
| `--generate-length N` | Length used with `--generate-sensitive`. Default: `32`. |
| `--generate-words N` | Generate a passphrase into the sensitive field instead of a random-character password. |
| `--generate-no-ambiguous` | Avoid ambiguous characters in generated values. |
| `--tag TAG` | Add a tag. Repeatable. |
| `--owner-agent NAME_OR_ID` | Operator/admin only: create the secret for a specific owning agent. |
| `--group NAME_OR_ID` | Create the secret as group-owned (collective). Operator/admin or `group:manage`. |

### `witself secret show NAME`

Show non-sensitive fields and redacted sensitive fields for a secret. This never
reveals sensitive values; use `secret reveal` for that.

Flags:

| Flag | Description |
|---|---|
| `--owner-agent NAME_OR_ID` | Operator/admin only: show a secret owned by a specific agent. |
| `--group NAME_OR_ID` | Show a group-owned secret. |
| `--show-tags` | Include tags. |
| `--show-access` | Include access grants. |
| `--version VERSION` | Show a historical version when supported. |

### `witself secret list`

List secrets visible to the current principal. Sensitive values are never
included.

```sh
witself secret list
witself secret list --all-agents
witself secret list --owner-agent builder-agent
witself secret list --group shared-context
```

Flags:

| Flag | Description |
|---|---|
| `--template TEMPLATE` | Filter by template. |
| `--tag TAG` | Filter by tag. Repeatable. |
| `--prefix TEXT` | Filter by name prefix. |
| `--owner-agent NAME_OR_ID` | Filter by the agent that owns the secret. |
| `--group NAME_OR_ID` | Filter by group-owned secrets. |
| `--all-agents` | Operator/admin only: include secrets owned by every agent in the realm. |
| `--include-group` | Operator/admin only: include group-owned secrets alongside agent-owned results. |
| `--limit N` | Maximum number of rows. |
| `--include-archived` | Include archived secrets when allowed. Maps to MCP `include_archived` and the JSON `archived` field. |
| `--include-disabled` | Include secrets whose owning agent is disabled, when allowed. |

### `witself secret scan`

Scan visible secrets and produce a redacted inventory. Operator/admin callers can
scan across all agents in the realm without revealing sensitive values.

```sh
witself secret scan
witself secret scan --all-agents
witself secret scan --owner-agent builder-agent
```

Flags:

| Flag | Description |
|---|---|
| `--all-agents` | Operator/admin only: scan secrets owned by every agent in the realm. |
| `--owner-agent NAME_OR_ID` | Scan secrets owned by one agent. |
| `--group NAME_OR_ID` | Operator/admin only: scan group-owned secrets. |
| `--include-group` | Operator/admin only: include group-owned secrets alongside agent-owned results. |
| `--template TEMPLATE` | Filter by template. |
| `--tag TAG` | Filter by tag. Repeatable. |
| `--prefix TEXT` | Filter by name prefix. |
| `--field FIELD` | Include only secrets with this field name. Repeatable. |
| `--show-sensitive-counts` | Include counts of sensitive fields, but not values. |
| `--show-access` | Include access-grant summary. |
| `--limit N` | Maximum number of rows. |

### `witself secret reveal NAME FIELD`

Reveal one sensitive field. This is the explicit, audited reveal ceremony of the
sealed plane; it is the only path (alongside `totp code` and `run`) that returns a
sensitive value. Every reveal emits a `secret.reveal` audit event. Hybrid
client-side / server-side decrypt is governed by the realm's
`client_side_decrypt` / `server_side_decrypt` capability per
[key-hierarchy.md](key-hierarchy.md); a server-mediated decrypt is flagged in the
audit record.

```sh
witself secret reveal github/builder password
witself secret reveal github/builder password --json
witself secret reveal github/builder password --owner-agent builder-agent --reason "operator recovery"
```

Flags:

| Flag | Description |
|---|---|
| `--owner-agent NAME_OR_ID` | Operator/admin only: reveal a secret field owned by a specific agent. |
| `--group NAME_OR_ID` | Operator/admin or `group:manage`: reveal a field from a group-owned secret. |
| `--version VERSION` | Reveal a historical version when supported. |
| `--reason TEXT` | Audit reason for the reveal. |
| `--ttl DURATION` | Request a short-lived reveal lease when supported. |

### `witself secret update NAME`

Update fields on an existing secret.

Flags:

| Flag | Description |
|---|---|
| `--owner-agent NAME_OR_ID` | Operator/admin only: update a secret owned by a specific agent. |
| `--group NAME_OR_ID` | Update a group-owned secret. |
| `--template TEMPLATE` | Change the secret template. |
| `--description TEXT` | Change the secret description. |
| `--field KEY=VALUE` | Set a non-sensitive field from a flag. Repeatable. |
| `--field-file KEY=PATH` | Set a non-sensitive field from a file. Repeatable. |
| `--field-stdin KEY` | Read one non-sensitive field from stdin. |
| `--sensitive KEY=VALUE` | Set a sensitive field from a flag. Least safe. Repeatable. |
| `--sensitive-file KEY=PATH` | Set a sensitive field from a file. Repeatable. |
| `--sensitive-stdin KEY` | Read one sensitive field from stdin. |
| `--generate-sensitive KEY` | Generate a password and store it in a named sensitive field. |
| `--generate-length N` | Length used with `--generate-sensitive`. |
| `--generate-words N` | Generate a passphrase into the sensitive field instead of a random-character password. |
| `--generate-no-ambiguous` | Avoid ambiguous characters in generated values. |
| `--remove-field KEY` | Remove a secret field. Repeatable. |
| `--tag TAG` | Add tag. Repeatable. |
| `--remove-tag TAG` | Remove tag. Repeatable. |
| `--reason TEXT` | Audit reason for the update. |

### `witself secret rename NAME NEW_NAME`

Rename a secret within the same owner. `NEW_NAME` must be unique for that owner
inside the current realm.

Flags:

| Flag | Description |
|---|---|
| `--owner-agent NAME_OR_ID` | Operator/admin only: rename a secret owned by a specific agent. |
| `--group NAME_OR_ID` | Rename a group-owned secret. |
| `--reason TEXT` | Audit reason. |

### `witself secret copy NAME NEW_NAME`

Copy a secret. By default this copies non-sensitive fields and structure only;
copying sensitive values must be explicit.

Flags:

| Flag | Description |
|---|---|
| `--owner-agent NAME_OR_ID` | Operator/admin only: copy a secret owned by a specific agent. |
| `--group NAME_OR_ID` | Copy a group-owned source secret. |
| `--to-agent NAME_OR_ID` | Copy into another agent's ownership. Operator/admin only. |
| `--to-group NAME_OR_ID` | Copy into a group's ownership. Operator/admin or `group:manage`. |
| `--include-sensitive` | Include sensitive field values. Requires confirmation and audit reason. |
| `--dry-run` | Show copy plan, destination ownership, and sensitive-field inclusion without copying values. |
| `--yes` | Skip confirmation when allowed. |
| `--reason TEXT` | Audit reason. |

### `witself secret archive NAME`

Archive a secret without permanently deleting it.

Flags:

| Flag | Description |
|---|---|
| `--owner-agent NAME_OR_ID` | Operator/admin only: archive a secret owned by a specific agent. |
| `--group NAME_OR_ID` | Archive a group-owned secret. |
| `--dry-run` | Show archive impact without archiving the secret. |
| `--yes` | Skip confirmation. |
| `--reason TEXT` | Audit reason. |

### `witself secret restore NAME`

Restore an archived secret when supported.

Flags:

| Flag | Description |
|---|---|
| `--owner-agent NAME_OR_ID` | Operator/admin only: restore a secret owned by a specific agent. |
| `--group NAME_OR_ID` | Restore a group-owned secret. |
| `--reason TEXT` | Audit reason. |

### `witself secret delete NAME`

Permanently delete a secret when allowed. Prefer `secret archive` for normal
removal.

Flags:

| Flag | Description |
|---|---|
| `--owner-agent NAME_OR_ID` | Operator/admin only: delete a secret owned by a specific agent. |
| `--group NAME_OR_ID` | Delete a group-owned secret. |
| `--permanent` | Permanently delete when allowed. |
| `--dry-run` | Show deletion impact, blockers, and affected grants without deleting the secret. |
| `--yes` | Skip confirmation. |
| `--reason TEXT` | Audit reason for deletion. |

### `witself secret grant NAME`

Grant an agent access to a secret. Secret access is grant-based and composes with
realm roles per [authorization-and-roles.md](authorization-and-roles.md); it does
not use the open-plane cross-agent policy engine.

Flags:

| Flag | Description |
|---|---|
| `--owner-agent NAME_OR_ID` | Operator/admin only: grant access to a secret owned by a specific agent. |
| `--group NAME_OR_ID` | Operator/admin or `group:manage`: grant access to a group-owned secret. |
| `--agent NAME_OR_ID` | Agent receiving access. Required. |
| `--read` | Allow redacted show/list access. |
| `--reveal FIELD` | Allow reveal of a specific field. Repeatable. |
| `--totp` | Allow TOTP code generation for this secret. |
| `--write` | Allow updates. |
| `--expires-at TIMESTAMP` | Expiration time for the grant. |
| `--dry-run` | Show planned access grant without changing permissions. |
| `--reason TEXT` | Audit reason. |

### `witself secret revoke NAME`

Revoke an agent's access to a secret.

Flags:

| Flag | Description |
|---|---|
| `--owner-agent NAME_OR_ID` | Operator/admin only: revoke access from a secret owned by a specific agent. |
| `--group NAME_OR_ID` | Operator/admin or `group:manage`: revoke access from a group-owned secret. |
| `--agent NAME_OR_ID` | Agent losing access. Required. |
| `--field FIELD` | Revoke reveal access for one field. Repeatable. |
| `--all` | Revoke all access for the agent. |
| `--dry-run` | Show planned access revocation without changing permissions. |
| `--reason TEXT` | Audit reason. |

## Secret References

Secret references identify a secret field without embedding the plaintext value.
They are intended for config files, scripts, MCP tool inputs, and runtime
injection, and are distinct from the open-plane `witself://` identity references
(memories/facts) described under [`witself reference`](#witself-reference).

Reference forms (the unified ownership model is agent or group; the former
`shared` scope is now a group):

```text
witself://secret/<secret-path>/<field>
witself://agent/<agent>/secret/<secret-path>/<field>
witself://group/<group>/secret/<secret-path>/<field>
```

Examples:

```text
witself://secret/github/builder/password
witself://agent/browser-agent/secret/github/builder/password
witself://group/shared-context/secret/github/org-readonly-token/token
```

Secret references should resolve only through explicit, value-returning commands
such as `secret reveal`, `totp code`, `run`, or the value-returning MCP
`reference.resolve` tool (disabled by `mcp serve --no-value-tools`). Merely
printing or listing a secret should not resolve references, and the sealed-plane
carve-outs apply: a secret reference is never embedded, recalled, placed in the
self-digest, or plaintext-exported.

## `witself run`

Run a subprocess with selected secret references resolved only for that process
lifetime. This is the safer path when an agent or human needs credentials for a
CLI tool, test suite, deploy script, or MCP server without printing the values.
It is a sealed-plane, value-returning command and is audited.

```sh
witself run --env GITHUB_TOKEN=witself://secret/github/builder/token -- gh repo view
witself run --env-file .env.witself -- npm test
```

Flags:

| Flag | Description |
|---|---|
| `--env KEY=REF` | Set an environment variable from a Witself secret reference. Repeatable. |
| `--env-file PATH` | Read environment assignments containing Witself secret references. |
| `--mask-output` | Mask injected secret values in stdout/stderr when possible. Default: true. |
| `--no-mask-output` | Disable output masking. |
| `--owner-agent NAME_OR_ID` | Operator/admin only: resolve unqualified references as a specific owning agent. |
| `--reason TEXT` | Audit reason for resolving references. |

## `witself totp`

Make Witself the authenticator application for accounts that use TOTP 2FA. A TOTP
enrollment stores its seed as high-value sealed material in the same plane as
secrets: the seed is never embedded, recalled, placed in the self-digest, or
plaintext-exported, and is revealed only through the guarded `totp show
--reveal-seed` path. The TOTP model is tracked in [totp-2fa.md](totp-2fa.md);
`totp:enroll` and `totp:code` are distinct scopes.

### `witself totp enroll NAME`

Enroll TOTP setup material into an existing or new secret.

```sh
witself totp enroll github/builder --otpauth 'otpauth://totp/...'
witself totp enroll github/builder --secret JBSWY3DPEHPK3PXP --issuer GitHub --account builder
witself totp enroll github/builder --qr ./github-2fa.png
```

Flags:

| Flag | Description |
|---|---|
| `--owner-agent NAME_OR_ID` | Operator/admin only: enroll TOTP on a secret owned by a specific agent. |
| `--group NAME_OR_ID` | Enroll TOTP on a group-owned secret. |
| `--otpauth URL` | Enroll from an `otpauth://` URL. |
| `--secret VALUE` | Base32 TOTP setup secret. Least safe. |
| `--secret-file PATH` | Read Base32 TOTP setup secret from a file. |
| `--qr PATH` | Parse TOTP setup material from a QR-code image. |
| `--description TEXT` | Required when `--create-secret` creates a new secret. |
| `--issuer TEXT` | Issuer name. |
| `--account TEXT` | Account label. |
| `--digits N` | Number of TOTP digits. Default: `6`. |
| `--period SECONDS` | TOTP period. Default: `30`. |
| `--algorithm SHA1|SHA256|SHA512` | TOTP HMAC algorithm. Default: `SHA1`. |
| `--create-secret` | Create the secret if it does not exist. |

### `witself totp code NAME`

Generate the current TOTP code for a secret. This is a value-returning,
reveal-gated sealed-plane command: it requires `totp:code`, emits a `totp.code`
audit event, and is disabled by `mcp serve --no-value-tools`.

```sh
witself totp code github/builder
witself totp code github/builder --json
```

Flags:

| Flag | Description |
|---|---|
| `--owner-agent NAME_OR_ID` | Operator/admin only: generate a code for a secret owned by a specific agent. |
| `--group NAME_OR_ID` | Generate a code for a group-owned secret. |
| `--at TIMESTAMP` | Generate code for a specific time. Testing and recovery only. |
| `--remaining` | Include seconds remaining in the current period. |
| `--reason TEXT` | Audit reason for code generation. |

### `witself totp show NAME`

Show TOTP metadata without revealing the seed.

Flags:

| Flag | Description |
|---|---|
| `--owner-agent NAME_OR_ID` | Operator/admin only: show TOTP metadata for a secret owned by a specific agent. |
| `--group NAME_OR_ID` | Show TOTP metadata for a group-owned secret. |
| `--reveal-seed` | Reveal the underlying TOTP seed. Admin/operator path only; emits a `totp.seed_revealed` audit event. |
| `--reason TEXT` | Audit reason when revealing seed. |

### `witself totp delete NAME`

Remove TOTP setup material from a secret.

Flags:

| Flag | Description |
|---|---|
| `--owner-agent NAME_OR_ID` | Operator/admin only: remove TOTP from a secret owned by a specific agent. |
| `--group NAME_OR_ID` | Remove TOTP from a group-owned secret. |
| `--dry-run` | Show removal impact without deleting TOTP setup material. |
| `--yes` | Skip confirmation. |
| `--reason TEXT` | Audit reason. |

## `witself policy`

Manage cross-agent access policies. Authorization for cross-agent identity
access is default-deny: with no matching `allow` policy, cross-agent access is
denied. A
policy binds a subject (agent or group) × permission verb × target (agent or
group), scoped to memories and/or facts, optionally filtered by kind, tag, name,
or `sensitive` flag. The policy engine and guardrails are tracked in
[access-policy.md](access-policy.md).

The `--scope memory|fact|both` flag is a CLI surface convenience that maps to the
`scope` array of singular tokens in [json-contracts.md](json-contracts.md):
`memory` → `["memory"]`, `fact` → `["fact"]`, and `both` → `["memory","fact"]`.

Permission verbs escalate in danger: `read`, `contribute`, `curate`, `forget`.
Operators can always manage and access identity data within their realm
(operator override), audited like any agent action.

### `witself policy create`

Create an `allow` policy.

```sh
witself policy create \
  --subject-agent coordinator \
  --permission read \
  --target-agent archivist \
  --scope memory \
  --description "Coordinator may read archivist memories"

witself policy create \
  --subject-group analysts \
  --permission contribute \
  --target-group shared-context \
  --scope fact \
  --filter-name "report/*"
```

Flags:

| Flag | Description |
|---|---|
| `--subject-agent NAME_OR_ID` | Subject is a specific agent. Mutually exclusive with `--subject-group`. |
| `--subject-group NAME_OR_ID` | Subject is a security group. Mutually exclusive with `--subject-agent`. |
| `--permission VERB` | One of `read`, `contribute`, `curate`, or `forget`. |
| `--target-agent NAME_OR_ID` | Target is a specific agent. Mutually exclusive with `--target-group`. |
| `--target-group NAME_OR_ID` | Target is a security group. Mutually exclusive with `--target-agent`. |
| `--scope memory|fact|both` | Identity data the policy applies to. Default: `both`. |
| `--filter-kind KIND` | Restrict to memories of a kind. Repeatable. |
| `--filter-tag TAG` | Restrict to memories/facts with a tag. Repeatable. |
| `--filter-name PATTERN` | Restrict to fact names matching a pattern or namespace. |
| `--filter-sensitive` | Restrict to (or, with `--exclude-sensitive`, away from) `sensitive` records. |
| `--exclude-sensitive` | Exclude `sensitive` records from the policy. |
| `--description TEXT` | Human-readable policy description. |
| `--dry-run` | Validate and show the planned policy without creating it. |
| `--reason TEXT` | Audit reason. |

### `witself policy list`

List policies in the realm.

Flags:

| Flag | Description |
|---|---|
| `--subject-agent NAME_OR_ID` | Filter by subject agent. |
| `--subject-group NAME_OR_ID` | Filter by subject group. |
| `--target-agent NAME_OR_ID` | Filter by target agent. |
| `--target-group NAME_OR_ID` | Filter by target group. |
| `--permission VERB` | Filter by permission verb. |
| `--scope memory|fact|both` | Filter by scope. |
| `--limit N` | Maximum number of rows. |
| `--cursor TOKEN` | Continue from a pagination cursor. |

### `witself policy show ID`

Show one policy, including subject, permission, target, scope, filters, effect,
and metadata.

Flags:

| Flag | Description |
|---|---|
| `--show-matches` | Summarize which agents/groups the policy currently resolves to (for group subjects/targets). |

### `witself policy delete ID`

Delete a policy. Removing a policy can revoke cross-agent access, so it is
guarded and audited.

Flags:

| Flag | Description |
|---|---|
| `--dry-run` | Show which cross-agent accesses would be revoked without deleting the policy. |
| `--yes` | Skip confirmation. |
| `--reason TEXT` | Audit reason. |

### `witself policy test`

Evaluate whether a given subject/permission/target/scope would be allowed under
current policy. This is the canonical dry-run for access decisions and returns
the deciding policy id or a deny reason. Available via CLI and MCP
(`witself.policy.test`).

```sh
witself policy test \
  --subject-agent coordinator \
  --permission read \
  --target-agent archivist \
  --scope memory

witself policy test \
  --subject-agent coordinator \
  --permission forget \
  --target-agent archivist \
  --scope memory --filter-kind episodic --json
```

Flags:

| Flag | Description |
|---|---|
| `--subject-agent NAME_OR_ID` | Subject agent to evaluate. Mutually exclusive with `--subject-group`. |
| `--subject-group NAME_OR_ID` | Subject group to evaluate. |
| `--permission VERB` | Permission verb to evaluate. |
| `--target-agent NAME_OR_ID` | Target agent to evaluate. Mutually exclusive with `--target-group`. |
| `--target-group NAME_OR_ID` | Target group to evaluate. |
| `--scope memory|fact|both` | Scope to evaluate. |
| `--filter-kind KIND` | Evaluate against a memory kind. Repeatable. |
| `--filter-tag TAG` | Evaluate against a tag. Repeatable. |
| `--filter-name PATTERN` | Evaluate against a fact name. |
| `--explain` | Include the full evaluation trace, not just the decision. |

## `witself group`

Manage security groups. A security group is a named set of agents within a
realm. It is both a policy subject and a policy target, and it can own
group-scoped shared memories and facts (collective memory). Membership is managed by operators
and by agents holding `group:manage`. Security groups are tracked in
[security-groups.md](security-groups.md).

### `witself group create NAME`

Create a security group. `NAME` is unique within the realm.

```sh
witself group create analysts --description "Read-only analysis agents"
witself group create shared-context --admin coordinator
```

Flags:

| Flag | Description |
|---|---|
| `--description TEXT` | Group description. |
| `--member NAME_OR_ID` | Add an initial member agent. Repeatable. |
| `--admin NAME_OR_ID` | Add an initial group admin agent (may manage membership under `group:manage`). Repeatable. |
| `--dry-run` | Validate and show planned group creation without creating it. |
| `--reason TEXT` | Audit reason. |

### `witself group list`

List security groups in the realm.

Flags:

| Flag | Description |
|---|---|
| `--member NAME_OR_ID` | Filter to groups containing a member agent. |
| `--limit N` | Maximum number of rows. |
| `--cursor TOKEN` | Continue from a pagination cursor. |

### `witself group show NAME_OR_ID`

Show one security group, including members, admins, and bound policies.

Flags:

| Flag | Description |
|---|---|
| `--show-members` | Include the member agent list. Default: true. |
| `--show-policies` | Include policies bound to the group as subject and/or target. |
| `--show-owned` | Include counts of group-owned memories and facts. |

### `witself group add-member NAME_OR_ID AGENT`

Add an agent to a security group. Operator/admin or `group:manage`.

Flags:

| Flag | Description |
|---|---|
| `--admin` | Add the agent as a group admin (may manage membership). |
| `--dry-run` | Show the planned membership change and resulting access without applying it. |
| `--reason TEXT` | Audit reason. |

### `witself group remove-member NAME_OR_ID AGENT`

Remove an agent from a security group. Operator/admin or `group:manage`.
Removing a member can revoke group-derived access, so it is guarded.

Flags:

| Flag | Description |
|---|---|
| `--dry-run` | Show which group-derived accesses would be revoked without removing the member. |
| `--yes` | Skip confirmation. |
| `--reason TEXT` | Audit reason. |

### `witself group delete NAME_OR_ID`

Delete a security group. Deleting a group removes its policy bindings and affects
access to group-owned memories and facts, so it is guarded and audited.

Flags:

| Flag | Description |
|---|---|
| `--reassign-owned-to AGENT_OR_GROUP` | Re-home group-owned memories and facts before deletion. |
| `--forget-owned` | Soft-delete group-owned memories and facts instead of re-homing them. |
| `--dry-run` | Show deletion impact, bound policies, members, and owned-record handling without deleting anything. |
| `--yes` | Skip confirmation. |
| `--reason TEXT` | Audit reason. |

## `witself message`

Exchange durable messages with other agents and groups. Messaging is fully in
scope for v0: a mailbox/queue model with at-least-once delivery, per-recipient
(and per-conversation) ordering, and explicit read/ack state. The sender (`from`)
is always derived from the authenticated token, never from input, so sender
forgery is structurally impossible. Message content is untrusted input to the
receiving agent; a message cannot itself authorize a cross-agent write. The
messaging model is tracked in
[inter-agent-messaging.md](inter-agent-messaging.md).

### `witself message send`

Send a message to an agent or group. `from` is the token-bound agent.

```sh
witself message send --to archivist \
  --subject "handoff" \
  --body "Postmortem stored as mem_01H...; please review."

witself message send --to-group shared-context \
  --kind notice \
  --body-file ./broadcast.txt

witself message send --to coordinator \
  --thread thr_01H... \
  --body "Acknowledged; proceeding." \
  --payload-file ./status.json
```

Flags:

| Flag | Description |
|---|---|
| `--to NAME_OR_ID` | Recipient agent. Mutually exclusive with `--to-group`. |
| `--to-group NAME_OR_ID` | Recipient security group; fanned out to current members. Mutually exclusive with `--to`. |
| `--subject TEXT` | Short subject. |
| `--kind KIND` | Short classification, such as `notice`, `handoff`, or `request`. |
| `--body TEXT` | Message body from a flag. |
| `--body-file PATH` | Read the message body from a file. |
| `--body-stdin` | Read the message body from stdin. |
| `--payload-file PATH` | Attach an optional structured payload from a JSON file. |
| `--thread ID` | Continue an existing conversation/thread for ordered delivery. |
| `--dry-run` | Validate recipients, authorization, and rate limits without sending. |
| `--reason TEXT` | Audit reason when required by operator/cross-context policy. |

### `witself message list`

List messages in the caller's mailbox. Defaults to received messages for the
token-bound agent. The mailbox selector maps to the canonical `direction` set in
[json-contracts.md](json-contracts.md): the default (received) is `inbox`, and
`--sent` selects `outbox`.

```sh
witself message list
witself message list --unread
witself message list --from coordinator --thread thr_01H...
witself message list --sent
```

Flags:

| Flag | Description |
|---|---|
| `--unread` | Show only unread messages. |
| `--sent` | Show messages sent by the caller instead of received. |
| `--from NAME_OR_ID` | Filter by sender agent. |
| `--to-group NAME_OR_ID` | Filter to messages delivered via a group. |
| `--thread ID` | Filter by conversation/thread. |
| `--kind KIND` | Filter by message kind. |
| `--since TIMESTAMP_OR_DURATION` | Filter by created window start. |
| `--until TIMESTAMP` | Filter window end. |
| `--owner-agent NAME_OR_ID` | Operator/admin only: list a specific agent's mailbox. |
| `--limit N` | Maximum number of rows. |
| `--cursor TOKEN` | Continue from a pagination cursor. |

### `witself message read ID`

Read one message, including body and any structured payload. Reading marks the
per-recipient read state. Treat the body and payload as untrusted input.

Flags:

| Flag | Description |
|---|---|
| `--owner-agent NAME_OR_ID` | Operator/admin only: read a message from a specific agent's mailbox. |
| `--no-mark-read` | Do not update read state when reading. |
| `--show-payload` | Include the structured payload. Default: true. |
| `--reason TEXT` | Audit reason for operator reads. |

### `witself message ack ID`

Acknowledge a message, recording explicit per-recipient ack state. Ack is
distinct from read: it confirms the recipient processed the message.

Flags:

| Flag | Description |
|---|---|
| `--owner-agent NAME_OR_ID` | Operator/admin only: ack on behalf of a specific agent when permitted. |
| `--reason TEXT` | Audit reason for operator acks. |

## `witself reference`

Parse and resolve `witself://` identity references. References let memories,
facts, messages, scripts, config files, and MCP tools point at identity data
without copying it. Reference resolution enforces the same authorization as a
direct read: a cross-agent or cross-group reference resolves only when policy
permits. Reference forms and rules are tracked in
[json-contracts.md](json-contracts.md) and the
[requirements](requirements.md#identity-references).

Reference forms (the final path component is the leaf; URL-encode if needed):

```text
witself://memory/<path-or-id>
witself://fact/<name>
witself://agent/<agent>/memory/<id>
witself://agent/<agent>/fact/<name>
witself://group/<group>/memory/<id>
witself://group/<group>/fact/<name>
```

Examples:

```text
witself://memory/mem_01H...
witself://fact/email
witself://agent/archivist/fact/home-region
witself://group/shared-context/memory/mem_01H...
```

### `witself reference parse REF`

Parse a `witself://` reference into its components (scheme, owner kind, owner,
leaf kind, leaf) without resolving it. Does not require authorization to the
target, because nothing is read.

```sh
witself reference parse witself://agent/archivist/fact/home-region
witself reference parse witself://memory/mem_01H... --json
```

Flags:

| Flag | Description |
|---|---|
| `--strict` | Fail on unknown forms instead of returning a best-effort parse. |

### `witself reference resolve REF`

Resolve a `witself://` reference to the underlying memory or fact through an
authorized read. Cross-agent and group references are policy-gated; resolution
enforces the same authorization as `memory read`/`fact get`.

```sh
witself reference resolve witself://fact/email
witself reference resolve witself://agent/archivist/memory/mem_01H... --reason "operator lookup"
```

Flags:

| Flag | Description |
|---|---|
| `--owner-agent NAME_OR_ID` | Operator/admin only: resolve unqualified references as a specific owning agent. |
| `--show-metadata` | Include record metadata alongside the resolved value/content. |
| `--reason TEXT` | Audit reason for cross-agent or operator resolution. |

## `witself agent`

Manage agent principals. Billing rolls up to the realm, but identity and
permissions are per agent. Operators can manage the full lifecycle of named
agents inside a realm. Ordinary agents cannot run lifecycle operations unless
explicitly granted that permission.

### `witself agent create NAME`

Create an agent principal. Operator/admin by default.

For ephemeral runtimes, the normal bootstrap path is to create the named agent
and write its durable token directly to a token file:

```sh
witself agent create browser-agent --token-out /run/secrets/witself-agent-token
```

Flags:

| Flag | Description |
|---|---|
| `--display-name TEXT` | Human-readable display name. |
| `--description TEXT` | Description of the agent's purpose. |
| `--tag TAG` | Add a tag. Repeatable. |
| `--no-token` | Create the agent without issuing a token. |
| `--token-out PATH` | Issue the initial durable agent token and write it to a file. |
| `--dry-run` | Validate and show planned agent creation without creating the agent or issuing a token. |

### `witself agent list`

List agent principals.

Flags:

| Flag | Description |
|---|---|
| `--tag TAG` | Filter by tag. Repeatable. |
| `--group NAME_OR_ID` | Filter to members of a security group. |
| `--disabled` | Include disabled agents. |
| `--limit N` | Maximum number of rows. |

### `witself agent show NAME_OR_ID`

Show one agent principal, including its `primary` facts as identity anchors.

Flags:

| Flag | Description |
|---|---|
| `--show-access` | Include policies where the agent is subject or target. |
| `--show-groups` | Include the agent's group memberships. |
| `--show-facts` | Include the agent's `primary` facts. Default: true. |
| `--show-usage` | Include usage summary when available. |

### `witself agent rename NAME_OR_ID NEW_NAME`

Rename an agent. Operator/admin by default. The agent keeps its identity,
memories, facts, policies, group membership, audit history, and tokens unless a
policy requires token rotation.

Flags:

| Flag | Description |
|---|---|
| `--rotate-tokens` | Rotate tokens after rename. |
| `--dry-run` | Show planned rename and token-rotation effects without applying them. |
| `--reason TEXT` | Audit reason. |

### `witself agent copy SOURCE_AGENT NEW_AGENT`

Copy an agent into a new named agent. Operator/admin by default. By default this
copies agent metadata, policy bindings, and group membership, but not memories
or facts. Copying identity material duplicates self-data and must be explicit and
audited.

Flags:

| Flag | Description |
|---|---|
| `--copy-tags` | Copy agent tags. Default: true. |
| `--copy-policies` | Copy policy bindings where allowed. Default: true. |
| `--copy-groups` | Copy group membership where allowed. Default: true. |
| `--copy-memories` | Copy memories owned by the source agent. Requires confirmation and audit reason. |
| `--copy-facts` | Copy facts owned by the source agent, including `primary` flags. Requires confirmation and audit reason. |
| `--include-sensitive` | Include `sensitive` memories/facts when copying identity material. Requires confirmation and audit reason. |
| `--issue-token` | Issue a token for the copied agent. |
| `--dry-run` | Show copy plan, included resources, and token issuance plan without creating anything. |
| `--yes` | Skip confirmation when allowed. |
| `--reason TEXT` | Audit reason. |

### `witself agent disable NAME_OR_ID`

Disable an agent principal. Operator/admin by default. Disabled agents cannot
authenticate with existing tokens.

Flags:

| Flag | Description |
|---|---|
| `--dry-run` | Show planned disable action without disabling the agent. |
| `--reason TEXT` | Audit reason. |
| `--yes` | Skip confirmation. |

### `witself agent enable NAME_OR_ID`

Re-enable an agent principal. Operator/admin by default.

Flags:

| Flag | Description |
|---|---|
| `--dry-run` | Show planned enable action without enabling the agent. |
| `--reason TEXT` | Audit reason. |

### `witself agent delete NAME_OR_ID`

Delete an agent principal when allowed. Operator/admin by default. Agent deletion
invalidates that agent's tokens.

Flags:

| Flag | Description |
|---|---|
| `--archive` | Archive instead of permanently deleting. |
| `--permanent` | Permanently delete when allowed. |
| `--forget-identity` | Soft-delete memories and facts owned by the agent. Default for safe deletion. |
| `--transfer-identity-to AGENT_OR_GROUP` | Transfer owned memories and facts to another agent or group. |
| `--delete-identity` | Permanently delete owned memories and facts when allowed. Requires confirmation and audit reason. |
| `--dry-run` | Show deletion impact, blockers, and identity-handling plan without deleting anything. |
| `--yes` | Skip confirmation. |
| `--reason TEXT` | Audit reason. |

## `witself token`

Manage agent or operator tokens.

V0 agent tokens are durable by default. They do not expire unless `--ttl` or
`--expires-at` is provided. Raw token values are returned only once during token
create or rotate; list/show output must contain metadata only. The token
lifecycle is tracked in [token-lifecycle.md](token-lifecycle.md).

### `witself token create`

Create a token for an agent or operator workflow.

Flags:

| Flag | Description |
|---|---|
| `--agent NAME_OR_ID` | Create an agent token. |
| `--operator` | Create an operator token. Operator/admin only. |
| `--name TEXT` | Token display name. |
| `--expires-at TIMESTAMP` | Token expiration. |
| `--scope SCOPE` | Token scope, such as `memory:read` or `message:send`. Repeatable. |
| `--out PATH` | Write the new token to a file instead of stdout. |
| `--ttl DURATION` | Token lifetime. Optional for durable agent tokens. |
| `--dry-run` | Validate and show planned token creation without issuing a token. |

If neither `--ttl` nor `--expires-at` is supplied for an agent token, the token
has no default expiration in v0. The full scope list is defined in
[requirements.md](requirements.md#authorization-and-scopes).

### `witself token list`

List tokens visible to the current principal.

Flags:

| Flag | Description |
|---|---|
| `--agent NAME_OR_ID` | Filter by agent. |
| `--operator` | Filter operator tokens. |
| `--include-expired` | Include expired tokens. |
| `--include-revoked` | Include revoked tokens. |

### `witself token revoke TOKEN_ID`

Revoke a token. Revocation is immediate.

Flags:

| Flag | Description |
|---|---|
| `--dry-run` | Show planned revocation without revoking the token. |
| `--reason TEXT` | Audit reason. |
| `--yes` | Skip confirmation. |

### `witself token rotate TOKEN_ID`

Rotate a token and return the replacement once.

Flags:

| Flag | Description |
|---|---|
| `--expires-at TIMESTAMP` | Expiration for the replacement token. |
| `--out PATH` | Write the replacement token to a file instead of stdout. |
| `--ttl DURATION` | Lifetime for the replacement token. Optional for durable agent tokens. |
| `--grace-period DURATION` | Keep the old token valid for this duration after issuing the replacement. |
| `--revoke-immediately` | Revoke the old token immediately after replacement is written. Default when no grace period is supplied. |
| `--dry-run` | Validate and show planned rotation without issuing or revoking tokens. |
| `--reason TEXT` | Audit reason. |

## `witself audit`

Inspect audit records.

### `witself audit list`

List audit events.

Flags:

| Flag | Description |
|---|---|
| `--since TIMESTAMP_OR_DURATION` | Start time. Examples: `2026-06-01`, `24h`. |
| `--until TIMESTAMP` | End time. |
| `--agent NAME_OR_ID` | Filter by agent. |
| `--actor NAME_OR_ID` | Filter by actor that performed the action. |
| `--all-agents` | Operator/admin only: include events for every agent in the realm. |
| `--memory ID` | Filter by memory id. |
| `--fact NAME` | Filter by fact name when recorded. |
| `--policy ID` | Filter by policy id. |
| `--group NAME_OR_ID` | Filter by security group. |
| `--message ID` | Filter by message id. |
| `--action ACTION` | Filter by action such as `memory.recalled`, `crossagent.forgotten`, or `message.sent`. |
| `--limit N` | Maximum number of rows. |

### `witself audit show EVENT_ID`

Show one audit event.

Flags:

| Flag | Description |
|---|---|
| `--include-request` | Include request metadata when available. |

## `witself export`

Export an agent's self (single-agent identity export). This is the headline,
plaintext, round-trippable export of a self: all memories (content, kind, tags,
source, salience, links, timestamps, and edit history) and all facts (values,
`primary` flags, `sensitive` flags, format hints, and edit history), with
identity anchors surfaced. For realm-wide operator export including policies and
groups, use [`witself realm export`](#witself-realm-export). Export/import is
tracked in [backup-and-recovery.md](backup-and-recovery.md).

`sensitive` records are exported in clear by default; the CLI requires an audit
`--reason` and warns when exporting `sensitive` records. Identity references are
preserved on export and re-resolved on import; dangling references are reported,
not silently dropped.

```sh
witself export --out ./self.json
witself export --agent archivist --out ./archivist-self/ --format dir
witself export --no-sensitive --out ./self-public.json
```

Flags:

| Flag | Description |
|---|---|
| `--agent NAME_OR_ID` | Operator/admin or self-permitted caller: export a specific agent's self. Default: the token-bound agent. |
| `--out PATH` | Write export to a file or directory. |
| `--format json|dir` | Export layout. `json` is a single file; `dir` is a diff/VCS-friendly directory layout. Default: `json` (`witself.v0` schema). |
| `--include-history` | Include memory/fact edit history. Default: true. |
| `--memory-kind KIND` | Restrict exported memories to a kind. Repeatable. |
| `--no-sensitive` | Exclude `sensitive` memories and facts. |
| `--reason TEXT` | Required audit reason when the export includes `sensitive` records. |

## `witself import`

Import an exported self into the same or a different agent/realm, preserving
`primary` flags, `sensitive` markers, links, and (where chosen) edit history.
Import is idempotent by stable id where ids are preserved and supports a
rename/remap mode when importing into a different agent. Import is audited and
supports `--dry-run`.

```sh
witself import --in ./self.json
witself import --in ./archivist-self/ --remap-agent archivist=archivist-copy --dry-run
```

Flags:

| Flag | Description |
|---|---|
| `--in PATH` | Read import from a file or directory. |
| `--agent NAME_OR_ID` | Target agent to import into. Default: the agent recorded in the export or the token-bound agent. |
| `--remap-agent FROM=TO` | Remap an owning agent on import. Repeatable. |
| `--merge` | Merge into the target agent's existing self. |
| `--replace` | Replace the target agent's existing self. |
| `--include-history` | Import edit history where present. Default: true. |
| `--dry-run` | Validate and show planned created/updated/conflicting records and dangling references without writing. |
| `--yes` | Skip confirmation for replacement. |
| `--reason TEXT` | Audit reason. Required when importing `sensitive` records. |

## `witself mcp`

Expose Witself to MCP-compatible agent runtimes.

### `witself mcp serve`

Start the MCP server.

The default MCP posture should be local-first. Stdio is the first target.
Network transports are a later, explicit deployment mode and must be
authenticated, scoped, and documented as higher risk. MCP tools respect the same
authorization checks as CLI commands, and agent-token MCP sessions act only as
the token-bound agent. Open-plane tools (memories/facts) have no reveal ceremony;
sealed-plane tools (`secret.reveal`, `totp.code`, and value-returning
`reference.resolve`) do, and any tool that returns a secret value or one-time code
may place that value in model-visible context. `--no-value-tools` disables those
value-returning sealed-plane tools while leaving open-plane and metadata tools
available; `--read-only` separately disables mutating tools.

On connect, the server returns its `instructions` field carrying the canonical
standing protocol that teaches the agent to call Witself: load `self.show` and
`recall` at the start of non-trivial work, `remember` after learning something
durable, `adjust`/`forget` instead of contradicting, and flush state with
`session.end` before long operations. This is the MCP half of the teaching layer;
the pinned text and the file-ecosystem counterpart
([`witself bootstrap-instructions`](#witself-bootstrap-instructions)) are tracked
in [context-hydration.md](context-hydration.md). In `--read-only` mode the server
omits the mutating tools (`remember`, `session.end`, `memory.consolidate`,
`ingest`); `self.show`, `session.start`, `recall`, and `digest.emit` remain.

Flags:

| Flag | Description |
|---|---|
| `--transport stdio|http` | MCP transport. Initial target: `stdio`. |
| `--listen ADDRESS` | Listen address for HTTP transport. |
| `--auth-token-file PATH` | Token file for HTTP transport auth. |
| `--read-only` | Serve without create/update/forget/delete tools (inspection-only). |
| `--no-value-tools` | Disable sealed-plane MCP tools that return a sensitive value or one-time code (`secret.reveal`, `totp.code`, value-returning `reference.resolve`). |
| `--agent NAME_OR_ID` | Operator/admin only: bind the server to one agent principal. Agent tokens are already bound by identity. |

### `witself mcp tools`

Print the MCP tool list and schemas.

The proposed MCP tool names, inputs, outputs, and exposure rules are tracked in
[mcp-tools.md](mcp-tools.md).

Flags:

| Flag | Description |
|---|---|
| `--name NAME` | Show one tool schema. |

## `witself config`

Manage local CLI configuration.

### `witself config get KEY`

Print one config value.

Flags:

| Flag | Description |
|---|---|
| `--show-source` | Include the source file or env var. |

### `witself config set KEY VALUE`

Set one config value.

Flags:

| Flag | Description |
|---|---|
| `--profile NAME` | Set the value for a named profile. |

### `witself config list`

List config values.

Flags:

| Flag | Description |
|---|---|
| `--show-source` | Include the source of each value. |

### `witself config unset KEY`

Remove one config value.

Flags:

| Flag | Description |
|---|---|
| `--profile NAME` | Remove the value from a named profile. |

## `witself completion`

Generate shell completion scripts.

```sh
witself completion bash
witself completion zsh
witself completion fish
witself completion powershell
```

Flags:

| Flag | Description |
|---|---|
| `--install` | Install completion for the current shell when supported. |

## First Implementation Slice

The first CLI slice should validate the managed-service command shape while
using the local mock/development backend only as scaffolding where needed. The
broader repo implementation sequence, including `witself-server`, Helm,
Terraform, images, and release automation, is tracked in
[implementation-plan.md](implementation-plan.md). The full v0 release boundary
is tracked in [v0-scope.md](v0-scope.md).

- `witself version`
- `witself capabilities`
- `witself setup`
- `witself realm init` for local mock/development scaffolding only
- `witself realm create`
- `witself realm status`
- `witself agent create`
- `witself token create`
- `witself memory add`
- `witself memory recall`
- `witself memory read`
- `witself memory list`
- `witself memory consolidate`
- `witself remember`
- `witself self show`
- `witself session start`
- `witself session end`
- `witself digest emit`
- `witself ingest`
- `witself bootstrap-instructions`
- `witself fact set`
- `witself fact get`
- `witself password generate`
- `witself secret create`
- `witself secret list`
- `witself secret show`
- `witself secret reveal`
- `witself run`
- `witself totp enroll`
- `witself totp code`
- `witself policy create`
- `witself policy test`
- `witself group create`
- `witself message send`
- `witself message read`
- `witself reference resolve`
- `witself export`
- `witself import`
- `witself mcp tools` as a schema preview command, even before `mcp serve`
  exists

`realm init` is included so early CLI work has a local store to exercise commands
before managed APIs are ready, using the `local-dev` embedding provider so
semantic recall can be validated offline. It is not a production milestone. This
slice is enough to validate human use, semantic recall, deterministic facts,
default-deny policy, security groups, inter-agent messaging, identity references,
and round-trippable export/import while the managed backend takes shape. The
open-plane (memory/fact/identity) core ships first; the sealed credential plane
(`password generate`, `secret`, `run`, `totp`) is a defined v0 slice that
validates generated passwords, reveal-gated secrets, runtime injection, and
authenticator-style 2FA, and may be staged after the open-plane core. Sealed-plane
commands also exercise the envelope/KMS dependency, which is required only when
the sealed plane is enabled.
