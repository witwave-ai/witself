# Witself CLI Command Surface

Status: draft target contract with implemented slices labeled below.

Narrative-memory amendment (accepted 2026-07-14): direct capture, lifecycle,
supersede, evidence, delete, lexical/hybrid recall, migration-0032 vector, and
`memory curate` commands below are implemented. The `memory curate auto service`
surface is retained as explicit legacy/manual compatibility tooling; runtime
hooks do not invoke it.
Direct commands handle exact user/client-authored changes;
`memory curate` lets a client claim frozen inputs and submit one exact,
reversible plan. Any older native-only or backend-consolidation language is
superseded. See
[narrative-memory-and-curation.md](narrative-memory-and-curation.md). Exact
command spelling is frozen with the first public memory schema.

The CLI command is `ws`. The backend binary stays `witself-server`; the
`witself://` reference scheme, `WITSELF_` environment variables, and the
`witself.*` MCP tool names are unchanged.

## Design Goals

- The CLI must be pleasant for humans and deterministic for agents.
- Managed-service account administration should be CLI-native, not a thin
  wrapper around a browser dashboard.
- Humans and AI assistants should be able to use the same command surface,
  subject to the same authentication, authorization, audit, and redaction rules.
- Every command that returns data should support `--json`.
- Identity recall has a deterministic PostgreSQL lexical/structured baseline.
  Optional implemented client-supplied vectors may extend ranking, while `memory
  show` and `fact get` stay exact.
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
| `WITSELF_AGENT` | Default local agent name. |
| `WITSELF_STORE_FILE` | Default local development store file path. |

Token file conventions:

- Production: set `WITSELF_TOKEN_FILE` to an explicit secret mount, such as
  `/run/secrets/witself-agent-token`.
- Managed local-agent fallback:
  `~/.witself/tokens/accounts/<account>/realms/<realm>/agents/<agent>.token`
  (`WITSELF_HOME` replaces `~/.witself` when set).
- Token files created by Witself should be owner-readable and owner-writable
  only on platforms that support POSIX-style permissions, such as mode `0600`.
- Directories created by Witself for token files should be owner-only on
  platforms that support POSIX-style permissions, such as mode `0700`.

Agent tokens are bound server-side to a realm and a named agent. Agent identity
comes from the token, never from a caller-supplied agent name. For managed local
commands, `--account`, `--realm`, and `--agent` select the credential file; the
server derives the identity from that credential and the CLI rejects any
mismatch. This is load-bearing for cross-agent access and inter-agent messaging:
the actor and message sender are derived server-side from the token.

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
- Mutating commands summarize what changed according to their implemented JSON
  contract. Direct memory writes return the resource plus value-free
  idempotency, concurrency, supersession, lifecycle, or deletion receipts where
  applicable. There is no universal `echo` field and no backend-authored
  semantic duplicate/merge result.
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
  operator list|create|delete
  realm create|list|show|use|rename|delete|members|init|status|export|import
  billing show|usage|limits|plans|subscribe|subscription|payment-methods|sessions|crypto|invoices
  support create|list|show|comment|close
  remember
  self show|card
  usage
  session start|end                       # target; not implemented
  memory capture|show|list|recall|history|adjust|supersede|forget|restore|reactivate|delete|evidence
  digest emit                             # target; not implemented
  ingest
  bootstrap-instructions
  fact set|get|list|delete
  password generate
  secret create|show|list|scan|reveal|update|rename|copy|archive|restore|delete|grant|revoke
  run
  totp enroll|code|show|delete
  policy create|list|show|delete|test
  group create|list|show|add-member|remove-member|delete
  install RUNTIME[,RUNTIME...]
  uninstall RUNTIME[,RUNTIME...]
  transcript create|append|list|show|tail|flush
  message send|reply|list|listen|read|ack|claim|renew|release|complete
  federation peers|card
  reference parse|resolve
  agent create|list|peers|show|rename|copy|disable|enable|delete
  token create|list|revoke|rotate
  audit list|show
  export
  import
  mcp serve|tools
  config get|set|list|unset
  completion
```

## Current Lifecycle Slice

The current self-hosted implementation includes the first operator lifecycle
commands:

```sh
ws operator list --endpoint URL --token-file OPERATOR_TOKEN
ws operator create --endpoint URL --token-file OPERATOR_TOKEN --name "Deploy bot" --token-name "Deploy token" --out ./deploy.token
ws operator delete --endpoint URL --token-file OPERATOR_TOKEN --yes OPERATOR_ID
```

`witself operator create` creates a new operator principal and returns that
operator's first token once. `witself token create --operator` is different: it mints
another token for the already authenticated operator record.

An operator can also mint a short-lived, server-enforced curator credential for
an existing agent. Both an audit name and an expiry are required; 24 hours is
the maximum. Curator tokens default to stdout unless `--out` is explicit, so an
ephemeral credential never overwrites the agent's managed full-token file.

```sh
witself token create --agent agent_... --profile curator-preview \
  --name "nightly memory preview" --ttl 30m --out ./curator.token

# Adds exact-plan apply, but still grants no ordinary memory/fact/message,
# request/cancel/rollback, sensitive-input, or permanent-delete authority.
witself token create --agent agent_... --profile curator-apply \
  --name "approved memory curator" --ttl 30m --out ./curator-apply.token
```

Omitting `--profile` preserves the existing durable `full` agent-token
behavior. `curator-preview` may inspect, claim, page, renew, plan, abandon, and
read status; `curator-apply` adds apply only. These restrictions are enforced
by the HTTP authorization boundary and token row, not only by the CLI or MCP
tool list.

Destructive commands are explicit and guarded:

```sh
ws realm delete --endpoint URL --token-file OPERATOR_TOKEN --yes REALM_ID
ws agent delete --endpoint URL --token-file OPERATOR_TOKEN --realm REALM_ID --yes AGENT_ID
ws token revoke --endpoint URL --token-file OPERATOR_TOKEN --token TOKEN_ID --yes
```

The create/delete/revoke policy for currently implemented resources is tracked
in [resource-lifecycle.md](resource-lifecycle.md).

## `witself version`

Print the CLI version and build metadata.

```sh
ws version
ws version --json
```

Flags:

| Flag | Description |
|---|---|
| `--short` | Print only the version string in human mode. |

## `witself capabilities`

Show the active backend kind, version, supported features, unavailable features,
limits, and endpoint context.

This command is the CLI view of the backend capability contract. It lets a
human, script, or AI assistant determine whether the current endpoint supports
managed billing, payment flows, crypto payments, support tickets, audit, direct
memory/recall/delete support, curation automation, or local-development-only
behavior before running a command.

```sh
ws capabilities
ws capabilities --endpoint https://witself.internal.example.com
ws capabilities --feature messaging --include-reasons
ws capabilities --feature memory_supersede --include-reasons
ws capabilities --feature scheduled_curation --include-reasons
```

Flags:

| Flag | Description |
|---|---|
| `--feature FEATURE` | Show one feature flag. Repeatable. |
| `--include-reasons` | Include unsupported/degraded feature reasons. |
| `--include-limits` | Include plan, rate, or backend limits when visible. |
| `--check` | Exit non-zero if any requested feature is unsupported. |

Self-hosted or local backends may report billing, hosted payment flows, crypto
payment flows, Witself support tickets, or managed-only features as unsupported.
The current server reports `memory_recall`, `memory_supersede`, and
`memory_permanent_delete` as supported. It also reports
`opportunistic_curation` as supported, while `automatic_capture` and
`scheduled_curation` remain explicitly `not_implemented`. `semantic_recall`
and `client_vector_recall` are supported when the migration-0032 vector surface
is wired; lexical recall does not need a provider. Commands should
use the same capability data when returning `unsupported_operation`. These are
backend capabilities: PostgreSQL can store due curation work, but the server,
MCP, and runtime hooks cannot launch or schedule inference. Foreground checkpoint
handling and the explicit legacy/manual `memory curate auto` client process do
not change those server capability flags.

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
ws auth login
ws auth login --device-code
ws auth login --no-browser
ws auth login --token-file /run/secrets/witself-token
ws auth login --token-file ~/.config/witself/tokens/browser-agent.token
ws auth login --endpoint https://api.witself.com
ws auth login --endpoint https://witself.internal.example.com --bootstrap-token-file ./bootstrap.token
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
ws setup \
  --account "Acme Agents" \
  --email ops@example.com \
  --realm prod \
  --agent browser-agent \
  --agent release-agent \
  --token-dir ./witself-tokens
```

Local mock/development setup is available for early implementation, demos, and
tests, but it is not the production deployment path. Its memory path uses the
same model-free lexical contract as deployed cells:

```sh
ws setup --local \
  --realm dev \
  --store-file ./witself.store.json \
  --agent browser-agent \
  --token-out browser-agent=./tokens/browser-agent.token
```

Managed-service setup can also emit runtime delivery artifacts such as
Kubernetes Secret manifests:

```sh
ws setup \
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
context. The local backend uses the same model-free lexical recall contract.

```sh
ws realm init
ws realm init --store-file ~/.witself/store.json
ws realm init --operator ops
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
`vector_write`, `vector_storage_byte`, `crossagent_access`,
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
ws billing sessions show hps_123
ws billing sessions show hps_123 --watch --timeout 5m
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

## `witself remember` (deferred)

The current CLI does not expose this unified convenience command. Natural-
language routing is an integration responsibility: an atomic durable assertion
calls `witself.fact.set`, while narrative context calls the implemented
`witself.memory.capture` operation by default. Runtime-native memory is used only
when the user explicitly names it or asks for both providers.

If `witself remember` is implemented later, it must compose those existing
Witself operations and must not move classification or inference into the
backend. See [Agent Memory Routing](agent-memory-routing.md).

## `witself self show`

Show the always-loaded self-digest: a bounded view of who the agent is plus an
authenticated, value-free `memory_checkpoint` and content-free
`message_checkpoint`. The digest lists `primary` facts first, then the top-N
salient memories (blended salience + recency), both pending/idle checkpoint
lines, and a one-line index of kinds, tags, and counts. It is cheap and
model-free. This is the MCP/CLI analogue of an auto-loaded CLAUDE.md head. The
digest shape, cap, and `elided` behavior are tracked in
[context-hydration.md](context-hydration.md).

The checkpoint contains request/run/fence lifecycle metadata only. A pending
checkpoint tells the current foreground agent to process at most one fenced
request; it never includes source content or authorizes deletion or a canonical
fact write. Human output prints `memory-checkpoint: pending request=...` or
`memory-checkpoint: idle`; an additive projection failure prints
`memory-checkpoint: unavailable` and does not hide identity, facts, or salient
memories. JSON includes the structured field even when facts or salient memories
are omitted.

The message checkpoint identifies pending canonical mailbox, offer,
coordinator-selection, and selected-assignment lanes. Human output prints
`message-checkpoint: pending lanes=...`, `message-checkpoint: idle`, or
`message-checkpoint: unavailable`. It contains no message body, never claims or
acknowledges work, and is not an availability or wake signal.

The digest has a hard byte/line cap (default ~8 KiB / ~200 lines, configurable
via `--max-bytes`). When capped, output sets `elided=true` and points to
`memory recall`; it is never silently truncated.

```sh
ws self show --account default --agent scott
ws self show --salient-limit 5 --json
ws self show --no-salient --max-bytes 4096

# Run the current source tree without waiting for a CLI release.
go run ./cmd/witself self show --account default --agent scott
```

Flags:

| Flag | Description |
|---|---|
| `--account NAME` | Local account binding. Default: `WITSELF_ACCOUNT` or `default`. |
| `--realm NAME` | Local realm selector. Default: `WITSELF_REALM` or `default`. |
| `--agent NAME` | Local agent selector. Default: `WITSELF_AGENT`; required for managed credential lookup. |
| `--endpoint URL` | Explicit cell endpoint; use with `--token-file` for an unmanaged credential. |
| `--token-file PATH` | Explicit agent token file; requires `--endpoint`. |
| `--no-facts` | Omit `primary` facts from the digest. |
| `--no-salient` | Omit salient memories from the digest. |
| `--salient-limit N` | Maximum salient memories to include. Default: `10`. |
| `--max-bytes N` | Hard cap on digest size; sets `elided=true` when the cap is hit. |
| `--json` | Emit `{ identity, primary_facts[], salient_memories[], memory_checkpoint, message_checkpoint, index, elided }`. |

## `witself self card`

Show a presentation-only identity card for the token-bound agent. This command
does not widen either backend contract: it composes an identity-only
`GET /v1/self` read with the existing `GET /v1/self/avatar` read and renders
only the authenticated identity plus the active avatar's bounded display
metadata. A new agent's deterministic placeholder is the active presentation
until an immutable avatar version is activated. A pending proposal may appear
only as `pending_update=true`; proposed artwork is never rendered as identity.
The card, JSON document, and SVG hash are unsigned presentation data; callers
must not accept them as authentication, authorization, or a legal credential.

The default output adapts to the destination:

- On a sufficiently wide UTF-8 color terminal, the CLI revalidates the SVG,
  verifies its SHA-256, renders it into a fixed 20x20 in-memory raster, and
  shows the portrait as ANSI half-block cells beside the identity fields.
- Pipes, redirected output, `NO_COLOR`, `TERM=dumb`, non-UTF-8 terminals, and
  unsupported renderings receive a deterministic text card. Narrow terminals
  receive a key/value form rather than a clipped box.
- `--plain` always disables the color portrait. `--json` emits the bounded card
  document for scripts. The two flags are mutually exclusive.

All displayed text is single-line, control- and bidi-sanitized, and bounded.
The command rejects mismatched self/profile/avatar identities, inconsistent
active pointers, unsafe or non-canonical SVG, and hash mismatches. It never
emits SVG, `visual_spec`, facts, memories, checkpoints, or pending proposal
content. Use `witself avatar show` or `witself avatar version` when the explicit
creative payload is actually needed.

```sh
witself self card --account default --agent scott
witself self card --plain --account default --agent scott
witself self card --json --account default --agent scott
```

Flags:

| Flag | Description |
|---|---|
| `--account NAME` | Local account binding. Default: `WITSELF_ACCOUNT` or `default`. |
| `--realm NAME` | Local realm selector. Default: `WITSELF_REALM` or `default`. |
| `--agent NAME` | Local agent selector. Default: `WITSELF_AGENT`; required for managed credential lookup. |
| `--endpoint URL` | Explicit cell endpoint; use with `--token-file` for an unmanaged credential. |
| `--token-file PATH` | Explicit agent token file; requires `--endpoint`. |
| `--plain` | Print the deterministic text card even when rich terminal rendering is available. |
| `--json` | Emit `witself.self-card.v1` identity and bounded active-avatar display metadata. |

## `witself usage`

Show fast hourly or daily product-usage totals for the authenticated agent. V0
is deliberately self-scoped: an agent token cannot select another agent, and
an operator token cannot use this command for account-wide aggregation.

```sh
witself usage --account default --agent scott
witself usage --account default --agent scott --since 24h --group-by hour
witself usage --account default --agent scott \
  --dimension transcript_entry_write --dimension transcript_entry_read --json
```

| Flag | Description |
|---|---|
| `--account NAME` | Local account binding. Default: `WITSELF_ACCOUNT` or `default`. |
| `--realm NAME` | Local realm selector. Default: `WITSELF_REALM` or `default`. |
| `--agent NAME` | Local agent selector. Default: `WITSELF_AGENT`; required for managed credential lookup. |
| `--endpoint URL` | Explicit cell endpoint; use with `--token-file` for an unmanaged credential. |
| `--token-file PATH` | Explicit agent token file; requires `--endpoint`. |
| `--since TIMESTAMP_OR_DURATION` | Window start; RFC3339 or positive duration such as `30d`/`24h`. Default: `30d`. |
| `--until TIMESTAMP` | RFC3339 window end. Default: now. |
| `--dimension DIMENSION` | Filter a usage dimension. Repeatable; comma-separated values also work. |
| `--group-by hour\|day` | UTC rollup size. Default: `day`. |
| `--json` | Emit identity scope, window, points, and whole-window totals. |

Initial transcript dimensions are `transcript_created`,
`transcript_entry_write`, `transcript_entry_read`, and
`transcript_storage_byte`. This is product usage, not a Stripe or billing view;
future realm/account billing commands aggregate from the same portable event
ledger subject to operator permissions.

## `witself session` (target; not implemented)

Bootstrap and flush long-running, multi-session work. `session start` hydrates
identity, open goals, and last progress in one round-trip; `session end` persists
a progress memory and updates the open goals. This pairs with the
assume-interruption / flush-before-long-operations habit so resuming a task is a
single call rather than a list-and-recall crawl. The session model is tracked in
[context-hydration.md](context-hydration.md).

### `witself session start`

Hydrate identity, open goals, and last progress for a new working session in one
call. Reads only; it never invokes a model provider.

```sh
ws session start
ws session start --json
```

Flags:

| Flag | Description |
|---|---|
| `--json` | Emit `{ identity, open_goals[], last_progress }`. |

### `witself session end`

Persist a session progress memory (kind `session`) and update open goals.

```sh
ws session end --summary "Shipped v0.3; rollback documented." \
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
recalled lexically/structurally, and filtered by metadata; they are not name-unique. The
memory model and recall ranking are tracked in
[memory-model.md](memory-model.md).

The implemented agent-owned direct slice uses `memory capture`, `show`, `list`,
`recall`, `history`, `adjust`, `supersede`, `forget`, `restore`, `reactivate`,
`memory evidence resolve`, and `memory delete`. Mutations require an explicit
idempotency key; adjust and lifecycle changes also require the exact current
version. Permanent-delete preview is the deliberate exception: it accepts only
the memory id and creates no state. Client curation/automation and vector
profiles/hybrid recall are also implemented below. Cross-agent/group mutation
and recall remain target work.

`witself memory evidence resolve EVIDENCE_ID` terminates one pending evidence
locator with exactly one of an exact transcript range (`--transcript`,
`--from-sequence`, `--until-sequence`), source memory/version, realm message,
import-artifact locator, or `--unresolvable-reason`. It appends a terminal
evidence row and requires `--idempotency-key`; the original pending row remains
immutable.

A memory carries `content` plus `content_encoding` (`plain` by default or
canonical `base64`), a `kind` convention (such as `episodic`, `semantic`,
`profile`, or `note`), `tags[]`, `source`, `salience`, `links[]` of `witself://`
references, an optional `sensitive` marker, timestamps, and versioned edit
history. Show, list, recall, and history output identify the effective encoding.
Human `memory show` output also prints an immutable supersession receipt and the
currently active relation set when present. Human `memory history` includes
receipt-set/revision/replacement columns and active-set/revision columns so an
operator can distinguish historical provenance from current relation state.

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
ws memory list --all-agents
ws memory recall "incident postmortem" --owner-agent archivist
ws memory adjust mem_01H... --owner-agent archivist --add-tag reviewed --reason "operator cleanup"
ws memory forget mem_01H... --owner-agent archivist --reason "duplicate" --dry-run
```

### `witself memory capture`

Capture one client-authored, agent-owned narrative with at least one exact,
pending, or explicitly unavailable evidence item. The server derives actor,
account, realm, stable owner, and origin from the agent token; it does not
summarize or classify the content.

```sh
witself memory capture \
  --content "Chose PostgreSQL as the sole authoritative memory store." \
  --kind decision --capture-reason explicit \
  --evidence-transcript trn_123 --evidence-from-sequence 40 \
  --evidence-until-sequence 44 --idempotency-key capture-db-decision

echo "Session checkpoint" | witself memory capture --stdin --kind session \
  --capture-reason session --evidence-unavailable-reason runtime_no_hook \
  --idempotency-key capture-session-checkpoint
```

Flags:

| Flag | Description |
|---|---|
| `--content TEXT` | Client-authored narrative content. |
| `--file PATH` | Read content from a file; `-` means stdin. |
| `--stdin` | Read content from stdin. Exactly one content source is required. |
| `--content-encoding ENCODING` | `plain` (default) or canonical `base64`. |
| `--kind KIND` | Kind such as `decision`, `session`, `milestone`, or `lesson`; default `note`. |
| `--tag TAG` | Add a tag; repeatable or comma-separated. |
| `--salience FLOAT` | Optional rank hint from 0 to 1. |
| `--link REF` | Add an identity link; repeatable or comma-separated. |
| `--sensitive` | Redact content in broad list/recall output by default. |
| `--occurred-from RFC3339` | Optional event-range start. |
| `--occurred-until RFC3339` | Optional event-range end. |
| `--capture-reason CODE` | Trigger such as `explicit`, `automatic`, `session`, or `manual`; default `manual`. |
| `--evidence-file PATH` | JSON array of typed evidence objects. |
| `--evidence-transcript ID` | Exact transcript id; pair with ordered positive sequence flags. |
| `--evidence-from-sequence N` | First exact transcript entry. |
| `--evidence-until-sequence N` | Last exact transcript entry. |
| `--evidence-locator LOCATOR` | Stable pending locator for evidence not flushed yet. |
| `--evidence-unavailable-reason CODE` | Explicit bounded reason exact/pending evidence is unavailable. |
| `--evidence-role ROLE` | `supports`, `contradicts`, or `context`; default `supports`. |
| `--evidence-source-digest SHA256` | Optional client-supplied source digest. |
| `--client-runtime`, `--client-model`, `--client-recipe`, `--client-recipe-version` | Optional self-reported client provenance. |
| `--idempotency-key KEY` | Required fresh retry key; reuse only for the exact capture retry. |

The older proposed `memory add` name is not the implemented command.

### `witself memory adjust ID`

Adjust an existing memory. Adjust appends a new version to the versioned edit
history; prior versions are retained.

Flags:

| Flag | Description |
|---|---|
| `--expected-version N` | Required exact current version. |
| `--idempotency-key KEY` | Required retry key for this logical adjustment. |
| `--content TEXT` | Replace memory content from a flag. |
| `--file PATH` | Replace content from a file; `-` means stdin. |
| `--stdin` | Replace content from stdin. |
| `--content-encoding ENCODING` | Replace the encoding with `plain` or canonical `base64`; may be used with or without replacing content. |
| `--kind KIND` | Change the memory kind. |
| `--salience FLOAT` | Change the salience hint. |
| `--add-tag TAG` | Add a tag. Repeatable. |
| `--remove-tag TAG` | Remove a tag. Repeatable. |
| `--add-link REF` | Add a `witself://` link. Repeatable. |
| `--remove-link REF` | Remove a `witself://` link. Repeatable. |
| `--sensitive` | Mark as `sensitive`. |
| `--not-sensitive` | Clear the `sensitive` marker. |
| `--occurred-from RFC3339` / `--occurred-until RFC3339` | Replace event bounds. |
| `--clear-occurred-from` / `--clear-occurred-until` | Clear event bounds. |
| `--reason TEXT` | Optional bounded adjustment reason. |

### `witself memory supersede ID`

Atomically and reversibly replace one exact active memory version with 1-32
client-authored memory capsules. The backend validates and stores the complete
set in one transaction; it does not decide what to merge, split, or rewrite.

```sh
witself memory supersede mem_01H... --expected-version 2 \
  --replacements-file replacements.json \
  --idempotency-key supersede-mem-01H...
```

`replacements.json` is a JSON array. Every element uses the capture capsule
shape and must include `content`, nonempty `evidence`, and an
`idempotency_key` distinct from the operation key and every other replacement
key. `kind` and `capture_reason` should identify the client-authored result.
Omitted `content_encoding` defaults to `plain`; use `base64` only when
`content` is canonical base64.

```json
[
  {
    "content": "PostgreSQL is the authoritative memory store.",
    "content_encoding": "plain",
    "kind": "decision",
    "capture_reason": "curation",
    "idempotency_key": "replacement-database-decision",
    "evidence": [
      {
        "state": "resolved",
        "type": "memory",
        "role": "supports",
        "source_memory_id": "mem_01H...",
        "source_memory_version": 2
      }
    ]
  }
]
```

Flags:

| Flag | Description |
|---|---|
| `--expected-version N` | Required exact current active source version. |
| `--replacements-file PATH` | Required JSON array of 1-32 replacement capsules; `-` means stdin. |
| `--idempotency-key KEY` | Required operation retry key, distinct from all replacement keys. |
| `--reason TEXT` | Optional bounded reversible-supersession reason. |
| `--client-runtime`, `--client-model`, `--client-recipe`, `--client-recipe-version` | Optional self-reported client provenance. |
| `--json` | Emit the source, full replacements, and value-free receipt. |

Success preserves the source as a `superseded` version, creates active
replacements, and returns the supersession set id/revision, exact source and
replacement version references, replacement count, and lowercase SHA-256
membership digest. The command rejects an inconsistent response and is
agent-self only.

### `witself memory read ID`

Read one memory deterministically by `id`. This is the exact-id counterpart to
ranked `recall`; neither operation makes a backend model call or mutates the
memory.

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

The implemented command performs bounded deterministic recall over the current
agent's active memory heads. It uses PostgreSQL literal full-text matching plus
salience and recency; the backend calls no model or embedding provider. An
optional profile plus caller-authored query vector enables deterministic hybrid
ranking. A text query is optional when a structured filter or query vector is
present. Natural phrases such as relative dates must be translated into RFC3339
filters by the client.

Results show total, similarity/vector-use, lexical, salience, and recency score
components. JSON also reports retrieval mode, profile, vector coverage and
candidate/match counts, candidate budget/truncation, degradation reason, and an
opaque next cursor when another page exists. Sensitive content is redacted
unless the exact intentional request uses `--include-sensitive`.

```sh
witself memory recall "what do I know about the prod outage"
witself memory recall "deploy preferences" --kind profile --limit 5 --json
witself memory recall --tag architecture \
  --occurred-from 2026-01-01T00:00:00Z
witself memory recall "deployment" --vector-profile mvp_... \
  --query-vector-file query-vector.json
```

Flags:

| Flag | Description |
|---|---|
| `--query TEXT` | Literal full-text query; mutually exclusive with positional `QUERY`. |
| `--kind KIND` | Exact memory-kind filter. |
| `--tag TAG` | Require a tag; repeatable or comma-separated. |
| `--link REF` | Require an identity link; repeatable or comma-separated. |
| `--origin CODE` | Filter by immutable origin. |
| `--capture-reason CODE` | Filter by capture trigger. |
| `--occurred-from RFC3339` | Event-range lower bound. |
| `--occurred-until RFC3339` | Event-range upper bound. |
| `--captured-from RFC3339` | Capture-time lower bound. |
| `--captured-until RFC3339` | Capture-time upper bound. |
| `--include-sensitive` | Intentionally include authorized sensitive content; default is redacted. |
| `--limit N` | Maximum ranked results, 1-100; default 20. |
| `--cursor TOKEN` | Continue the exact same query/filter set with the opaque server cursor. |
| `--vector-profile ID` | Immutable client-vector profile; requires `--query-vector-file`. |
| `--query-vector-file FILE` | JSON array for the compatible query vector; `-` reads stdin and requires `--vector-profile`. |
| `--json` | Emit the complete recall page and score metadata. |

Cross-agent/group recall remains future work. Client-vector ranking is explicit,
never a hidden fallback: omit both vector flags for the full lexical contract.

### `witself memory vector profile create|list` / `vector set`

Register/list immutable client-vector contracts and store exact memory-version
vectors. Provider/model/recipe values are identifiers only; Witself never calls
that model or accepts its credential. Each agent may register at most 100
profiles. Dimensions are 1-4096; metric is `cosine`, `dot`, or `euclidean`;
normalization is `none` or `l2`, with L2 required for dot.

```sh
witself memory vector profile create --provider local --model example \
  --recipe whole-memory --recipe-version 1 --dimensions 3
witself memory vector profile list
witself memory vector set mem_... --profile mvp_... --memory-version 2 \
  --content-hash SHA256 --vector-file vector.json
```

`vector set` binds the finite JSON array to the exact profile, memory version,
and content hash. Exact retries return the existing value-free receipt; changed
components conflict. Migration `0032` stores vectors as portable JSONB and
includes both profile/vector tables in schema-32 account export/import. No
pgvector extension is required.

### `witself memory list`

List memories visible to the current principal with metadata and filters.
`sensitive` content is redacted by default.

```sh
witself memory list
witself memory list --kind decision --tag architecture
witself memory list --state forgotten --limit 20
```

Flags:

| Flag | Description |
|---|---|
| `--owner-agent ID` | Optional explicit current-agent id; another owner is rejected. |
| `--state STATE` | Exact lifecycle state, including `active`, `superseded`, `forgotten`, or `reverted`. |
| `--kind KIND` | Exact memory-kind filter. |
| `--tag TAG` | Require a tag; repeatable or comma-separated. |
| `--origin CODE` | Filter by immutable origin. |
| `--capture-reason CODE` | Filter by capture reason. |
| `--occurred-from RFC3339` / `--occurred-until RFC3339` | Filter the event range. |
| `--captured-from RFC3339` / `--captured-until RFC3339` | Filter capture time. |
| `--include-sensitive` | Include authorized sensitive content; default is redacted. |
| `--limit N` | Maximum rows, 1-100; default 100. |
| `--cursor TOKEN` | Continue with the opaque cursor. |

### `witself memory forget ID`

Forget a memory by appending a reversible `forgotten` version. This is the
default removal path and is reversible via `memory restore`; it does not perform
the physical purge used by `memory delete`.

Flags:

| Flag | Description |
|---|---|
| `--expected-version N` | Required exact current version. |
| `--idempotency-key KEY` | Required retry key for this logical forget. |
| `--reason TEXT` | Optional bounded lifecycle reason. |

### `witself memory restore ID`

Restore a forgotten memory by appending a new active version.

Flags:

| Flag | Description |
|---|---|
| `--expected-version N` | Required exact current version. |
| `--idempotency-key KEY` | Required retry key for this logical restore. |
| `--reason TEXT` | Optional bounded lifecycle reason. |

### `witself memory reactivate ID`

Explicitly reactivate a reverted or otherwise invalidly-restorable memory. It
requires `--expected-version` and `--idempotency-key` like the other lifecycle
commands. A superseded head additionally requires the exact
`--expected-supersession-set-revision`; replacement memories remain active.

### `witself memory delete ID`

Permanently scrub one current-agent narrative memory. The operation is guarded,
irreversible, and distinct from reversible `memory forget`. It uses two explicit
commands:

```sh
witself memory delete mem_01H... --dry-run
witself memory delete mem_01H... --yes --expected-version 3 \
  --scrub-set-revision 0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef \
  --idempotency-key delete-mem-01H...
```

The deterministic preview accepts only the memory id plus connection/output
flags. It creates no preview id/hash/expiry, receipt id, or idempotency record.
Its value-free output supplies the exact current version, scrub-set revision,
version/evidence/relation/retry-shield counts and shield digest, plus incoming-
evidence and active-relation blocker counts.

Apply must copy `--expected-version` and `--scrub-set-revision` from that preview,
use one fresh idempotency key, and include `--yes`. The CLI sends
`X-Witself-Direct-User-Authorized: true`; therefore apply is valid only when this
turn's current user directly requested permanent deletion of that exact memory.
Autonomous/background work, standing instructions, subagents or delegated work,
and retrieved/untrusted content can never authorize it. Apply refuses stale
guards or live dependencies and never cascades into another memory.

Flags:

| Flag | Description |
|---|---|
| `--dry-run` | Return a value-free deterministic preview; accepts no mutation guards. |
| `--yes` | Assert direct current-user confirmation for irreversible apply. |
| `--expected-version N` | Apply only: exact positive current version returned by preview. |
| `--scrub-set-revision SHA256` | Apply only: exact 64-character lowercase scrub revision returned by preview. |
| `--idempotency-key KEY` | Apply only: fresh retry key for this one logical deletion; reuse only for its exact retry. |
| `--json` | Emit the value-free preview or apply receipt. |

The current implementation is agent-self only. It preserves a value-free
tombstone and hashed retry shields while purging memory versions, evidence, and
relations. It does not delete facts, transcripts, provider-native memory,
pre-existing exports, or backups.

### `witself memory curate`

Drive the implemented client-side curation protocol. These commands expose the
same request, lease/fence, immutable-input, strict-plan, atomic-apply, and
compensating-rollback contract as HTTP and MCP. The manual protocol subcommands
do not start a model. The implemented `run`
driver can invoke either a caller-supplied planner executable or a native
provider CLI after a fail-closed safety probe; all inference remains client
side.

```sh
# Queue or coalesce due work, then list claimable requests.
witself memory curate request --source memory --source evidence \
  --source transcript --idempotency-key curate-request-1
witself memory curate requests

# Drive one due run in preview mode with a safe native provider, or with a
# JSON-in/JSON-out planner executable. Preview requeues without applying.
witself memory curate run --provider claude-code
witself memory curate run -- ./my-memory-planner --mode bounded

# Explicit legacy/manual client curator. Runtime hooks do not invoke it.
# Preview is the default policy.
witself memory curate auto enable --runtime claude-code \
  --provider claude-code --allow-transcript-content
witself memory curate auto status --runtime claude-code
witself memory curate auto wake --runtime claude-code
witself memory curate auto service install --runtime claude-code
witself memory curate auto service status --runtime claude-code

# Claim the request and page the frozen inputs returned for this fence.
witself memory curate start --request mcrq_... \
  --idempotency-key curate-start-1
witself memory curate show mrun_... --fence 7

# Submit, retrieve/review, then apply, the exact normalized accepted plan.
witself memory curate plan mrun_... --fence 7 \
  --file plan.json --idempotency-key curate-plan-1
witself memory curate plan-get mrun_... --fence 7
witself memory curate apply mrun_... --fence 7 --plan-revision 1 \
  --plan-hash 0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef \
  --idempotency-key curate-apply-1 --yes
```

Subcommands:

| Subcommand | Contract |
|---|---|
| `request` | Create or coalesce deterministic work. `--idempotency-key` is required. Scope flags are `--source`, `--memory-state`, `--include-sensitive`, `--max-memories`, `--max-evidence`, and `--max-transcript-entries`; queue metadata includes `--coalescing-key`, `--trigger-reason`, `--trigger-generation`, `--priority`, RFC3339 `--due-at`, and `--max-attempts`. |
| `requests` (`list`) | List stable cursor-paged requests. Empty `--state` lists claimable due work; an explicit lifecycle state lists that state. `--limit` is 1-200 and `--cursor` continues the exact filter. |
| `run` | Trusted provider-neutral driver for one due request. Preview is the default; `--apply --yes` enables exact reversible apply. Choose either `--provider codex|claude-code|grok-build|cursor` with optional `--provider-path`, or pass `-- PLANNER [ARGS...]`; the forms are mutually exclusive. Native mode probes the installed CLI before claiming work and fails closed when required isolation controls are absent. `--resume LAUNCH_ID` resumes value-free local retry state. Input, page, lease, timeout, action, output, and client-provenance flags remain bounded by authenticated preflight. Due selection excludes scopes explicitly marked `include_sensitive`; a full-token automatic client may still select a separately authorized transcript scope. Restricted profiles always refuse both transcript-bearing and explicitly sensitive work. |
| `auto enable\|disable\|status\|run\|wake` | Configure and inspect the explicit legacy/manual client-owned curator for an installed `--runtime`. Runtime transcript hooks never invoke it. Enable requires an explicit native `--provider`, successful safety probe, exact full-token binding, and `--allow-transcript-content`; preview is the default, while standing apply requires `--policy apply --yes`. `--debounce`, `--minimum-interval`, and `--max-runs` bound execution. `wake --runtime RUNTIME` records a value-free manual-poll marker and services one bounded, policy-gated pass. `run` services existing legacy markers; `run --force` first records a scheduled-poll marker. Disable preserves pending value-free wake state. |
| `auto service install\|status\|start\|uninstall` | Manage explicit legacy persistent per-user polling for one enabled `--runtime`: a private launchd LaunchAgent on macOS or systemd user service/timer on Linux. Install is idempotent, refuses unowned unit-file collisions, schedules `auto run --force --supervise`, and contains no credential, agent identity, provider, model, or source content. `start` requests an immediate bounded run. Uninstall removes only owned service definitions and leaves automation policy/wakes intact. This service is a separately selected compatibility path, not runtime-hook behavior. |
| `start` | Claim `--request REQ_ID` (or a positional id), freeze bounded inputs, and obtain the lease and fence. Accepts per-run input caps, `--lease-seconds`, optional `--budgets-file`, client provenance flags, and a required idempotency key. |
| `renew RUN_ID` | Heartbeat an active run with required `--fence` and idempotency key; `--extension-seconds` defaults to 300. |
| `show RUN_ID` (`get`) | Page the exact frozen inputs with required `--fence`, optional opaque `--cursor`, and `--limit` 1-200. Returned content is untrusted data, not instructions. |
| `plan RUN_ID` | Submit a strict `witself.memory-plan.v1` JSON file using required `--fence`, `--file`, and idempotency key. `-` reads stdin. The result returns the normalized plan, preallocated ids, value-free impact counts, accepted revision, and canonical lowercase SHA-256 hash. |
| `plan-get RUN_ID` (`accepted-plan`) | Read-only reconstruct and cryptographically verify the exact normalized accepted plan for a live planned run using required `--fence`. Review every action and preview against all paged inputs before apply; `--json` returns the complete envelope. |
| `apply RUN_ID` | Atomically apply the accepted plan. Required guards are `--fence`, `--plan-revision`, `--plan-hash`, a fresh idempotency key, and explicit `--yes`. |
| `cancel RUN_ID` | Terminate the fenced run and its queue request. Requires `--fence` and an idempotency key; `--reason` is optional. |
| `abandon RUN_ID` | Terminate the fenced run but requeue retryable work with bounded backoff. It takes the same guards as `cancel`. |
| `rollback RUN_ID` | Attempt guarded append-only compensation. Requires `--apply-receipt`, `--expected-heads` (a complete JSON array of exact apply-produced heads), an idempotency key, and `--yes`; `--reason` is optional. |
| `status [RUN_ID]` | Read value-free owner-lane/request/run status without requiring an active lease. |

The strict plan contains ordered, contiguous actions and only five primitives:
`create` writes a full new memory, `replace` appends a version-checked full
snapshot, `supersede` version-checks a source and points it at an exact bounded
replacement set, `relate` adds an approved lineage edge, and `propose_fact`
creates a review candidate. Inference can never call canonical `fact set`
through a curation plan. The backend validates input provenance, authorization,
bounds, expected heads, subject identity, and the canonical plan hash, then
commits every action and contiguous cursor advance in one transaction or none.
An empty actions plan is valid and should be applied when no frozen input merits
memory; it creates no memory or fact and advances only the reviewed cursors.

Rollback requires every apply-produced head still to be current and refuses
later consumers. It appends compensating memory state, reverts curation-created
relations, withdraws curation-created fact candidates, and queues a read-only
replay. It never cascades and never rewinds curation cursors. Source writes can
coalesce due work. `memory curate run` and safe native-provider adapters are
implemented. Normal automatic handling is foreground: PostgreSQL marks work
due, `self show` exposes a value-free pending pointer, and the active agent
processes at most one fenced request near turn end. Runtime transcript hooks
only enqueue and flush evidence; they never record a launcher wake, detach a
curator, or start inference.

`memory curate auto` is retained as explicit legacy/manual compatibility
tooling. A user may invoke `wake` or `run --force`, or separately install the
per-user launchd/systemd `auto service`, to service value-free manual/scheduled
markers under configured debounce, retry, provider, transcript-consent, and
preview/apply policy. Its internal supervisor may detach continuations, but it
is never invoked by runtime hooks. The child receives no Witself credential in
argv, environment, or model input. This client-owned launcher does not make
`scheduled_curation` a backend capability. MCP still cannot wake an AI.
PostgreSQL remains the sole canonical memory and curation store.

### `witself memory consolidate`

This command is not implemented. The autonomous server-classification design is
superseded by the implemented `witself memory curate ...` commands that submit
an exact client-authored plan; see
[narrative-memory-and-curation.md](narrative-memory-and-curation.md). The backend
validates and applies the plan but does not decide what to merge, split, or
supersede.

## `witself digest emit` (target; not implemented)

Render the self-digest as a CLAUDE.md / AGENTS.md fragment for file-load agent
harnesses. This is the outbound half of the two-way file bridge: it makes
Witself-backed identity available for free to runtimes that auto-load AGENTS.md
or CLAUDE.md, with provenance HTML comments (witself-generated marker plus
timestamp) so the emitted block is recognizable and round-trippable. The emit
formats and provenance markers are tracked in
[context-hydration.md](context-hydration.md).

`digest emit` would read only and invoke no model provider. It may optionally
emit a `self.digest.emitted` audit event.

```sh
ws digest emit --format agents-md -o ./AGENTS.md
ws digest emit --format claude-md --max-bytes 4096
ws digest emit --format markdown
```

Flags:

| Flag | Description |
|---|---|
| `--format claude-md\|agents-md\|markdown` | Output fragment format. |
| `--max-bytes N` | Hard cap on the emitted fragment size. |
| `-o, --out PATH` | Write the fragment to a file instead of stdout. |

## `witself ingest` (target; not implemented)

Ingest existing agent context files into Witself: the inbound half of the file
bridge. `ingest` parses CLAUDE.md / AGENTS.md / GEMINI.md, routing kv-shaped
lines to typed fact assertions and prose paragraphs to client-authored memories,
tagging everything with stable import provenance. Artifact identity plus exact
idempotency keys make unchanged re-imports no-ops; the backend does not infer
semantic duplicates. This makes Witself a good citizen of the
AGENTS.md ecosystem rather than a competitor; it composes the existing fact and
memory create paths and adds no new resource. The parser rules are tracked in
[context-hydration.md](context-hydration.md).

```sh
ws ingest ./AGENTS.md ./CLAUDE.md
ws ingest ./docs/GEMINI.md --source-label legacy --dry-run
```

Flags:

| Flag | Description |
|---|---|
| `--source-label L` | Override the `source=import:<file>` label applied to ingested records. |
| `--dry-run` | Show planned created/updated/deduplicated facts and memories without writing. |
| `--json` | Emit the per-file exact write plan and idempotency results. |

`ingest` emits `fact.imported` and `memory.imported` audit events. It is excluded
in `--read-only` MCP mode.

## `witself bootstrap-instructions` (target; not implemented)

Print the paste-able teaching stanza that installs the Witself usage habit
(recall before relevant work, capture after durable learning, and curate through
an explicit client-authored plan) into an agent's file-loaded context. Witself is
a service an agent must be taught to call, so the stanza is the file-ecosystem
half of the teaching layer, mirroring the MCP server `instructions` text. The
canonical stanza is pinned in
[context-hydration.md](context-hydration.md).

```sh
ws bootstrap-instructions
ws bootstrap-instructions --format agents-md
ws bootstrap-instructions --format text
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
ws fact set display-name "Browser Agent"
ws fact set email builder@example.com --primary --format email
ws fact set aws/account-id 123456789012 --format string
ws fact set notes --value-file ./notes.txt
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
| `--recreate-deleted` | Explicitly create a new fact id at an address whose prior fact was permanently deleted. Never inferred. |
| `--owner-agent NAME_OR_ID` | Set a fact for a specific agent. `contribute`/`curate` policy or operator. |
| `--owner-group NAME_OR_ID` | Set a group-owned fact. |
| `--dry-run` | Show the planned upsert and any primary demotion without writing. |
| `--reason TEXT` | Audit reason. Required for cross-agent or group-owned writes and for primary promotion across agents. |

### `witself fact get NAME`

Get one fact deterministically by name. An authorized read returns the value,
including for `sensitive` facts; there is no reveal ceremony.

```sh
ws fact get email
ws fact get email --owner-agent archivist --reason "operator lookup"
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
ws fact list
ws fact list --primary-only
ws fact list --all-agents
ws fact list --owner-group shared-context
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

### `witself fact delete PREDICATE`

Permanently delete the token-bound agent's fact at
`(--subject, PREDICATE)`. Deletion removes the current value, every assertion
in its history, evidence references, and all candidates at the same canonical
address. It preserves the subject/aliases, immutable usage events, billing
rollups, and a value-free tombstone/receipt. It never rolls resolution back to
an older assertion and cannot be undone.

```sh
ws fact delete --subject person_spouse --dry-run identity/name
ws fact delete --subject person_spouse --yes identity/name
ws fact delete --yes --fact-id fact_01... --expected-assertion-id fas_01... \
  --expected-candidate-revision 0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef \
  --idempotency-key fact_delete_01...
```

Flags:

| Flag | Description |
|---|---|
| `--subject KEY_OR_ALIAS` | Stable subject or alias. Defaults to `self`. |
| `--dry-run` | Show a value-free impact preview without deleting anything. |
| `--yes` | Confirm permanent, non-restorable deletion. Required for apply. |
| `--fact-id ID` | Exact-mode fact id returned by preview. Cannot be combined with a subject or positional predicate. |
| `--expected-assertion-id ID` | Exact-mode resolved assertion guard returned by preview. |
| `--expected-candidate-revision REVISION` | Exact-mode 64-character lowercase hexadecimal candidate-set guard returned by preview. |
| `--idempotency-key KEY` | Stable retry key for this logical deletion. Generated when omitted. |

Apply is concurrency-safe: the CLI previews first and supplies the previewed
resolved assertion id and candidate-set revision. A fact or matching candidate
changed between preview and apply returns a conflict and remains intact. If the
address-mode apply returns an error or an inconsistent success receipt, the CLI
prints a copy-pasteable exact-mode replay command containing the fact id, both
guards, and the same generated or supplied idempotency key; the command never
contains the fact value, source, evidence, subject, or predicate. Reusing old
direct-write/proposal retry keys cannot resurrect deleted content. `fact set
--recreate-deleted` is the explicit path to create a new fact id at that
address; it requires a fresh mutation key and does not inherit the old usage
rank.

## `witself password generate`

Generate a password or passphrase. This is a sealed-plane utility used most often
to populate a sensitive secret field (see `secret create --generate-sensitive`),
but it also works standalone. Generated values are returned to the caller and are
not stored unless written into a secret.

```sh
ws password generate
ws password generate --length 40 --no-ambiguous
ws password generate --words 5
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
memory recall, never placed in the self-digest, and never included in the
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
ws secret scan --all-agents
ws secret show github/builder --owner-agent browser-agent
ws secret update github/builder --owner-agent browser-agent --field url=https://github.com/login
ws secret grant github/builder --owner-agent browser-agent --agent release-agent --read --reveal password
ws totp code github/builder --owner-agent browser-agent --reason "operator recovery"
```

### `witself secret create NAME`

Create a secret. `NAME` must be unique for the owning agent (or group) inside the
current realm. Different agents may use the same `NAME`.

```sh
ws secret create github/builder \
  --description "GitHub login for browser-agent" \
  --template login \
  --field username=builder@example.com \
  --field url=https://github.com/login \
  --generate-sensitive password

ws secret create stripe/test \
  --description "Stripe test API key" \
  --template api-key \
  --sensitive-stdin api-key

ws secret create deploy/tls \
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
ws secret list
ws secret list --all-agents
ws secret list --owner-agent builder-agent
ws secret list --group shared-context
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
ws secret scan
ws secret scan --all-agents
ws secret scan --owner-agent builder-agent
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
ws secret reveal github/builder password
ws secret reveal github/builder password --json
ws secret reveal github/builder password --owner-agent builder-agent --reason "operator recovery"
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
ws run --env GITHUB_TOKEN=witself://secret/github/builder/token -- gh repo view
ws run --env-file .env.witself -- npm test
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
ws totp enroll github/builder --otpauth 'otpauth://totp/...'
ws totp enroll github/builder --secret JBSWY3DPEHPK3PXP --issuer GitHub --account builder
ws totp enroll github/builder --secret-file ./github-seed.txt
ws totp enroll github/builder --qr ./github-2fa.png
```

Flags:

| Flag | Description |
|---|---|
| `--owner-agent NAME_OR_ID` | Operator/admin only: enroll TOTP on a secret owned by a specific agent. |
| `--group NAME_OR_ID` | Enroll TOTP on a group-owned secret. |
| `--otpauth URL` | Enroll from an `otpauth://` URL. |
| `--secret VALUE` | Base32 TOTP setup secret. Least safe. |
| `--secret-file PATH` | Read the Base32 seed from a file; preferred over `--secret` to keep the seed off argv. |
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
ws totp code github/builder
ws totp code github/builder --json
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
ws policy create \
  --subject-agent coordinator \
  --permission read \
  --target-agent archivist \
  --scope memory \
  --description "Coordinator may read archivist memories"

ws policy create \
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
ws policy test \
  --subject-agent coordinator \
  --permission read \
  --target-agent archivist \
  --scope memory

ws policy test \
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
ws group create analysts --description "Read-only analysis agents"
ws group create shared-context --admin coordinator
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

## `witself transcript`

Record the visible interaction between a user and an AI system. A transcript is
an append-only enterprise ledger, not an addressed A2A mailbox. The agent token
is the token-derived recorder; `role` is recorded data. Account operator tokens
may list/show for audit but cannot create or append.

```sh
ws transcript create \
  --endpoint https://cell.example.com \
  --token-file ./agent.token \
  --title "Deployment review" \
  --external-id vendor-thread-42

ws transcript append trn_123 --endpoint https://cell.example.com \
  --token-file ./agent.token --role user --body "Is the rollout healthy?"

ws transcript append trn_123 --endpoint https://cell.example.com \
  --token-file ./agent.token --role assistant --body-file ./answer.txt \
  --reply-to ent_123 --model model-version

ws transcript list --account default
ws transcript show trn_123 --account default --json
ws transcript tail trn_123 --account default --agent scott --limit 20
ws transcript flush --runtime codex
```

`create` accepts `--title`, `--external-id`, and `--metadata-file` (a bounded
JSON object). `append` requires `--role user|assistant|system|tool` and at least
one of `--body`, `--body-file`, `--stdin`, or `--payload-file`; it also accepts
`--external-id` (retry-safe runtime message id), `--model`, and `--reply-to`.
The interactive commands accept `--endpoint`,
`--token-file`, and `--json`; `WITSELF_ENDPOINT`, `WITSELF_TOKEN_FILE`, and
`WITSELF_TOKEN` are the unattended equivalents.

`tail` performs a bounded newest-entry read. `flush` retries the durable local
hook outbox for an installed `codex`, `claude-code`, `grok-build`, or `cursor`
integration. The foreground command drains all currently uploadable events or
returns a concrete delivery error; it does not stop merely because a large
valid backlog takes longer than the detached hook flusher's bounded work
window.

For Grok Build, `flush` also finalizes an unresolved Stop event from the trusted
native session file. Grok writes the final assistant response only after its
synchronous Stop hook returns, so Witself requires the exact matching native
Stop execution and later `turn_completed` fence, then atomically persists the
assistant form before upload. An incomplete or unsafe turn remains local and
causes a nonzero flush result; later events for other transcripts are not
blocked. A later Grok hook retries automatically, while an explicit foreground
flush after the client exits closes the final session without launching a
client, inference, wrapper, or persistent runner.

Only finalized visible output should be appended. Raw hidden chain-of-thought
and streaming chunks are out of contract. Small structured objects belong in
`payload`; non-empty file artifacts are refused until portable object storage
lands. See [transcript-ledger.md](transcript-ledger.md).

Cursor's exact native `timestamp` plus `user_query` prompt envelope is removed
from the canonical user body. In `raw` hook capture, a bounded original
provider payload is retained; an oversized envelope retains only `raw_omitted`,
`raw_bytes`, and `raw_sha256`, and native-transcript fallback does not invent
raw prompt payloads for recovered messages. Witself preserves the body
unchanged when that envelope is malformed, nested, repeated, or has any extra
bytes; no other runtime uses this normalization.

## `witself install`

Install both MCP access and transcript hooks for a supported local agent
runtime:

```sh
witself install codex
witself install claude
witself install grok
witself install cursor
witself install claude,codex,grok,cursor --agent scott --location home
witself install claude,codex --routing-only
```

The installer reuses an existing binding or the only local agent credential. If
selection is ambiguous, pass `--agent NAME`; the resolved account, realm, and
agent are pinned in the installed hook and MCP commands. The token-bound
identity is verified before runtime configuration changes. `--capture` accepts
`messages`, `trace`, or `raw`; `--location` is an optional human label paired
with a stable generated local id. A supplied label is pinned in both commands;
when omitted, no `--location` argument is written. `--endpoint` and
`--token-file` are optional and otherwise use the normal managed endpoint and
token-file conventions. No token is copied into MCP or hook configuration.

`--routing-only` atomically refreshes only the runtime's managed static
instruction block. It does not resolve credentials, contact Witself, invoke a
provider CLI, change the integration binding, register MCP, or install/remove
hooks. It intentionally conflicts with every binding and hook flag. Use it
when the installed binary contains newer routing policy but administrator-
managed hooks and the existing MCP registration are already correct; restart
the runtime or begin a new task afterward so it reloads the file.

The installer also reports the runtime's automatic-hydration delivery modes.
The transcript hook durably queues capture first, then uses the exact installed
account/realm/agent binding for a bounded, fail-open open-plane read. Codex and
Claude Code inject `self.show` at session start, focused lexical
`memory.recall` context for deterministically history-dependent prompts, and an
authenticated value-free `memory_checkpoint` plus content-free
`message_checkpoint` on any prompt when attention is already pending at read
time. The active client then uses non-blocking `message.listen` to retrieve
unread metadata. Focused automatic recall is a bounded `OR` query over
distinct meaningful keywords, not the raw or literal whole prompt. The managed
rule tells the foreground model to process at most one fenced request near turn
end and to apply a valid empty plan when nothing merits memory. An additive
checkpoint projection failure leaves identity and recall available and marks
the affected checkpoint `unavailable:true`; emitted degraded recall is
explicitly marked with `recall_status:"degraded"` and `recall_reason`.

Cursor may accept or log session-start context without reliably delivering it
to the model, its prompt hook cannot add model context, and Grok passive-hook
output is ignored. Both runtimes therefore use `guided_mcp_fallback`: their
managed routing rules tell the active agent to call `self.show`, inspect the
memory and message checkpoints, run a non-blocking `message.listen`, and recall
over MCP. Witself does not mislabel that guidance as
synchronous automatic injection. Authorized `sensitive` open-plane facts and
memories are included for the authenticated owner and retain their sensitivity
markers; server-redacted and non-plain values are omitted. Sealed secret and
TOTP values are never included in hook output or MCP memory recall.

The injected checkpoint is a point-in-time snapshot, not same-turn synthesis.
The current prompt may still be flushing and the current assistant response does
not yet exist, so that evidence can be reviewed on a later interaction. Runtime
hooks never launch inference or another curator, and automatic delivery does not
guarantee that the model follows the managed rule.

Install also writes the managed fact-versus-native-memory routing policy for
the selected runtime. Cursor uses
`$CURSOR_CONFIG_DIR/rules/witself-memory-routing.mdc` (normally
`~/.cursor/rules/witself-memory-routing.mdc`) with `alwaysApply: true`
frontmatter and dotted Witself MCP tool names. The default rule is discovered
for workspaces beneath the user's home through Cursor's ancestor rule search;
a custom `CURSOR_CONFIG_DIR` works for routing only when that Cursor
installation also discovers its `rules` directory. Cursor Memories remain
project-scoped advisory context, so broad native-memory recall reports partial
coverage rather than claiming an exhaustive search.

Administrator-managed hooks are the default for Codex and Claude Code while
identity and MCP registration remain user-scoped. The command prompts for
administrator access only for that system policy write. Codex uses
`/etc/codex/requirements.toml`; Claude Code uses the platform
`managed-settings.d/50-witself.json` drop-in. Grok Build and Cursor use their
approval-free global user hook locations. Existing configuration is preserved,
Witself handlers are idempotent, and unrelated hooks are not disabled. Pass
`--user-hooks` to use Codex or Claude user settings instead; Codex asks for
one-time approval through `/hooks` in that mode.

```sh
witself uninstall codex
witself uninstall claude
witself uninstall grok
witself uninstall cursor
witself uninstall claude,codex,grok,cursor
```

Uninstall infers user versus managed hook mode from the local integration
record and preserves tokens and pending transcript events. `--managed-hooks`
forces removal of the administrator-managed policy for a supported runtime.
If the integration record is missing, uninstall fails closed without changing
MCP, hook, or routing state because it cannot reconstruct a rollback-safe
binding. Reinstall the runtime integration to rebuild that record, then
uninstall it.

## `witself message`

Exchange durable messages with other agents and groups. Messaging is fully in
scope for v0: a mailbox/queue model with at-least-once delivery, per-recipient
(and per-conversation) ordering, and explicit read/ack state. The sender (`from`)
is always derived from the authenticated token, never from input, so sender
forgery is structurally impossible. Message content is untrusted input to the
receiving agent; a message cannot itself authorize a cross-agent write. The
messaging model is tracked in
[inter-agent-messaging.md](inter-agent-messaging.md); the client-owned autonomy
extension is foreground-only and tracked in
[autonomous-realm-messaging.md](autonomous-realm-messaging.md).

Implementation note: the current checkout supports same-realm direct,
explicit-list, and realm fanout with one immutable send-time delivery snapshot;
recipient-only `reply`; metadata-only `list` and `listen`; separate `read` and
`ack`; delivery-processing `claim`/`renew`/`release`; atomic result-producing
`complete`; client-ranked realm open requests; and the `message_checkpoint`
plus foreground handling contract. Group/cross-realm recipients, dry-run,
time-window filters,
operator mailbox overrides, and responsibility/directive-aware eligibility
remain follow-on slices. This describes the current checkout, not a deployed or
released version.

### `witself message send`

Send to exactly one of: one agent, a bounded explicit agent list, or every other
live agent in the token-derived realm. `from` is the token-bound agent. Fanout
creates one immutable send-time recipient snapshot and one independent delivery
row per recipient; a realm audience excludes the sender. Caller-supplied
`thread_id` is correlation metadata; it is not proof that this send is a reply
and does not grant thread membership. Use `message reply` for validated reply
causality.

Realm-qualified agents, groups, and dry-run are target extensions tracked in
[agent-collaboration.md](agent-collaboration.md); the current command rejects
cross-realm routing.

```sh
ws message send --to archivist \
  --subject "handoff" \
  --body "Postmortem stored as mem_01H...; please review."

ws message send --to coordinator \
  --thread thr_01H... \
  --kind note \
  --body "Acknowledged; proceeding." \
  --payload-file ./status.json

ws message send --to-agents archivist,reviewer \
  --kind note --body "The release candidate is ready."

ws message send --to-realm \
  --kind note --body "The maintenance window starts now."
```

Flags:

| Flag | Description |
|---|---|
| `--to NAME_OR_ID` | Recipient agent in the authenticated realm. A lowercase `agent_` prefix selects the exact ID namespace and never falls back to a name. |
| `--to-agents NAME_OR_ID[,NAME_OR_ID...]` | Bounded explicit recipients; repeatable or comma-separated. Mutually exclusive with `--to` and `--to-realm`. |
| `--to-realm` | Every other live agent in the authenticated realm, resolved atomically at send time. Mutually exclusive with `--to` and `--to-agents`. |
| `--subject TEXT` | Short subject. |
| `--kind KIND` | Short classification; defaults to actionable `request`. Use explicit `note` for FYI-only delivery with no implied reply or provider-inference requirement. |
| `--body TEXT` | Message body from a flag. |
| `--body-file PATH` | Read the message body from a file. |
| `--body-stdin` | Read the message body from stdin. |
| `--payload-file PATH` | Attach an optional structured payload from a JSON file. |
| `--thread ID` | Continue an existing conversation/thread for ordered delivery. |
| `--idempotency-key KEY` | Retry key for one logical send; reuse returns the original message. |

The default is deliberately actionable across CLI, MCP, and API/store
normalization. An omitted kind becomes `request`. An explicit `note` (or other
non-actionable label) is FYI-only; an active foreground client acknowledges it
after handling it without implying a reply or a provider-inference call.

Recipient resolution is exact and case-sensitive. A selector beginning with
lowercase `agent_` is ID-only: if that live same-realm ID does not exist, the
send fails even if a live agent has that exact text as its name. Other selectors
use ordinary exact ID-or-name resolution, with an ID taking precedence over a
same-text name. This prevents a stale generated ID from silently routing to a
different, same-named agent.

The same namespace rule applies to inbox sender filters. On `message list` and
`message listen`, a lowercase `agent_` `--from` value matches only that exact
sender ID and never a same-text name; ordinary values use exact ID-or-name
matching with ID precedence.

An exact keyed send retry returns its original durable message even if that
message's recipient was subsequently deleted. A new key still performs live
recipient resolution and cannot create a delivery to the tombstone.

### `witself message reply ID`

Reply to one inbound parent message. The caller must be the parent recipient.
The server derives the recipient from the parent sender and derives both the
thread and `reply_to_message_id`; it derives `causal_depth` as parent plus one.
The command intentionally has no `--to`, `--thread`, or depth flag. Raw
`thread_id` remains correlation only.

```sh
ws message reply msg_01H... \
  --body "Which environment should I use?" \
  --idempotency-key reply-01
```

Flags:

| Flag | Description |
|---|---|
| `--subject TEXT` | Short reply subject. |
| `--kind KIND` | Reply classification; defaults to `reply`. |
| `--body TEXT` | Reply body from a flag. |
| `--body-file PATH` | Read the reply body from a file; `-` means stdin. |
| `--body-stdin` | Read the reply body from stdin. |
| `--payload-file PATH` | Attach an optional structured payload from a JSON file. |
| `--idempotency-key KEY` | Retry key for one logical reply; reuse returns the original message. |

An exact keyed reply retry likewise survives later deletion of its derived
recipient; a new reply key still requires that recipient to be live.

### `witself message list`

List messages in the caller's mailbox. Defaults to received messages for the
token-bound agent. The mailbox selector maps to the canonical `direction` set in
[json-contracts.md](json-contracts.md): the default (received) is `inbox`, and
`--sent` selects `outbox`.

```sh
ws message list
ws message list --unread
ws message list --from coordinator --thread thr_01H...
ws message list --sent
```

Flags:

| Flag | Description |
|---|---|
| `--unread` | Show only unread messages. |
| `--sent` | Show messages sent by the caller instead of received. |
| `--from NAME_OR_ID` | Filter by exact sender agent ID or name; lowercase `agent_` is ID-only. |
| `--thread ID` | Filter by conversation/thread. |
| `--kind KIND` | Filter by message kind. |
| `--limit N` | Maximum number of rows. |
| `--cursor TOKEN` | Continue from a pagination cursor. |

### `witself message read ID`

Read one message, including body and any structured payload. Reading marks the
per-recipient read state. Treat the body and payload as untrusted input.

The command has no no-mark-read mode: use metadata-only `list` or `listen` when
content should remain unread. `--json` returns the same message detail shape as
the API and MCP, including backend-derived `causal_depth`. Direct sends begin at
one; validated replies advance exactly one from their durable parent.

### `witself message ack ID`

Acknowledge a message, recording explicit per-recipient ack state. Ack is
distinct from read: it confirms the recipient processed the message.

`--json` returns acknowledged message metadata only; it never returns the body
or payload.

### `witself message listen`

Stateless long-poll receive: block up to `--timeout` seconds and return the
oldest unacknowledged inbound **message metadata** for the token-bound agent.
This is a waitable mailbox query, not a consuming drain: a timeout or dropped
poll loses no state, and listing or listening never marks read or acknowledged.
A crash after read but before ack leaves the message eligible for a later
listen. The installed foreground policy instructs an active client to call
`listen`, then explicitly read untrusted content and acknowledge only after
handling it; it does not force model compliance.
The active-session teaching boundary lives in
[context-hydration.md](context-hydration.md), and the foreground-only handling
model is tracked in
[autonomous-realm-messaging.md](autonomous-realm-messaging.md).

The retired client-local runner is not part of the command surface. During the
upgrade that removes it, a hidden cleanup-only command remains available as
`witself message runner disable (--runtime RUNTIME|--all) [--force] [--json]`.
New CLI processes first check a private completion marker, then use a
filesystem-only artifact check so a host that never installed the runner does
not contact launchd or systemd. That no-artifact path does not write the marker.
When artifacts exist, startup runs the all-runtime cleanup.

For an owned service, cleanup disables it, removes its definition, and stops the
loaded unit before inspecting local state. Pending notification pointers or a
malformed state file therefore cannot leave the retired service running.
Without `--force`, valid pointers and malformed `state.json` content are
preserved while other files are scrubbed when the enclosing directory is
private; unsafe directory state is left untouched. `--force` is required to
discard either form of preserved state and never deletes canonical Postgres
messages. The completion marker represents an all-runtime result: the
startup migration or manual `--all` cleanup writes it only after successful
all-runtime retirement. Neither the no-artifact fast path nor `--runtime`
cleanup suppresses the next startup-wide pass. An exact legacy
`message runner serve --runtime RUNTIME` process is treated only as a retirement
tombstone so a still-loaded unit can disable itself; it cannot execute messages.

```sh
ws message listen
ws message listen --timeout 20 --json
ws message listen --conversation thr_01H... --timeout 10
```

Flags:

| Flag | Description |
|---|---|
| `--timeout SECONDS` | Maximum wait from 0 through 20 seconds; defaults to 20. |
| `--from NAME_OR_ID` | Filter by exact sender agent ID or name; lowercase `agent_` is ID-only. |
| `--thread ID` | Filter by conversation/thread. |
| `--conversation ID` | Alias for `--thread`; specifying both requires equal values. |
| `--kind KIND` | Filter by message kind. |
| `--limit N` | Maximum messages to return, from 1 through 100; defaults to 50. |

Listen has no cursor: each poll queries current unacknowledged state oldest
first. JSON output contains `messages` and `timed_out`. The server bounds
concurrent listen admission per process; saturation is retryable and surfaces
HTTP 429 with `Retry-After`.

### `witself message claim ID`

Acquire a bounded processing lease before autonomous work on one inbound,
unacknowledged direct message. Claiming does not read or acknowledge the
message. An exact idempotent retry returns the same claim; an active different
claim conflicts. A new or expired claim increments the monotonic generation
fence. Generation is only the stale-writer fence. The separate backend-owned
`failure_count` tracks exact-fence releases that a foreground client marks as
deterministic message failures.

```sh
ws message claim msg_01H... \
  --lease 2m \
  --idempotency-key foreground-claim-msg-01H
```

Flags:

| Flag | Description |
|---|---|
| `--lease DURATION` | Processing lease from 30 seconds through 15 minutes in whole seconds; defaults to 5 minutes. |
| `--idempotency-key KEY` | Required retry key for this one logical claim. |

Human output includes message id, claim id, generation, state, lease expiry, and
the appended `failure_count`. JSON output returns the value-free `processing`
object, including `failure_count`.

### `witself message renew ID`

Renew one exact, still-live claim using the claim id and generation returned by
`claim` (or the prior renewal). Renewal is a database-time heartbeat and does
not read, complete, or acknowledge the message.

```sh
ws message renew msg_01H... \
  --claim mcl_01H... \
  --generation 1 \
  --lease 2m
```

Flags:

| Flag | Description |
|---|---|
| `--claim ID` | Required active `mcl_` processing claim id. |
| `--generation N` | Required positive processing fence generation. |
| `--lease DURATION` | Replacement lease from 30 seconds through 15 minutes in whole seconds; defaults to 5 minutes. |

### `witself message release ID`

Release one exact claim so another client may retry. Release makes processing
available and invalidates the old fence immediately; it does not ack. By
default it leaves `failure_count` unchanged. Use `--deterministic-failure` only
for a repeatable failure attributable to this message; never use it for a
provider-wide or configuration failure, cancellation, timeout, or claim-lease
maintenance failure. The flag atomically increments the backend-owned
`failure_count`.

```sh
ws message release msg_01H... --claim mcl_01H... --generation 1
```

Flags:

| Flag | Description |
|---|---|
| `--claim ID` | Required active `mcl_` processing claim id. |
| `--generation N` | Required positive processing fence generation. |
| `--deterministic-failure` | Record one repeatable message-specific failure; false by default. |

### `witself message complete ID`

Atomically validate one exact unexpired fence, create a server-routed reply to
the original sender, link that result to the delivery, and mark processing
completed. Recipient, thread, causal parent, sender, account, and realm are
derived by the server, and result `causal_depth` is parent plus one. Completion
never acknowledges the parent; call `message ack` only after observing the
durable completion.

```sh
ws message complete msg_01H... \
  --claim mcl_01H... \
  --generation 1 \
  --kind result \
  --body-file ./result.txt \
  --idempotency-key foreground-complete-msg-01H-1
```

Flags:

| Flag | Description |
|---|---|
| `--claim ID` | Required active `mcl_` processing claim id. |
| `--generation N` | Required positive processing fence generation. |
| `--subject TEXT` | Optional result subject. |
| `--kind KIND` | Result classification; defaults to `result`. |
| `--body TEXT` | Result body from a flag. |
| `--body-file PATH` | Read the result body from a file; `-` means stdin. |
| `--body-stdin` | Read the result body from stdin. |
| `--payload-file PATH` | Attach an optional structured result object. |
| `--idempotency-key KEY` | Required retry key for this one atomic completion. |

### `witself message request`

Coordinate a realm-wide open job without backend inference. `open` snapshots
every other live agent in the authenticated realm as a bounded candidate set
and sends one ordinary `open_request` notification delivery to each candidate.
Candidates may offer or decline. Under the only current policy,
`client_ranked`, the coordinator client reads the offers and submits the chosen
agent IDs; PostgreSQL validates capacity and creates exact fenced reservations
but never ranks or selects an agent.

```sh
ws message request open \
  --subject "Investigate rollout" \
  --body "Find the cause of the failed GCP rollout." \
  --offer-window 30s \
  --expires-in 1h \
  --max-assignees 1 \
  --idempotency-key rollout-investigation

ws message request list --role candidate --state open
ws message request offer mrq_01H... \
  --body "I can inspect GKE and PostgreSQL." \
  --idempotency-key offer-mrq-01H

# The coordinator ranks the returned offers locally.
ws message request show mrq_01H... --json
ws message request select mrq_01H... \
  --selected-agent agent_01H... \
  --reservation 2m \
  --idempotency-key select-mrq-01H

ws message request claim mrq_01H... \
  --lease 2m --idempotency-key claim-mrq-01H
# Use the returned mrc_ claim id and generation.
ws message request complete mrq_01H... \
  --claim mrc_01H... --generation 1 \
  --body "The rollout failed because ..." \
  --idempotency-key complete-mrq-01H-1
```

Subcommands:

| Command | Behavior |
|---|---|
| `open` | Create the realm snapshot and opening notification. Requires a body and idempotency key; accepts `--subject`, `--payload-file`, closed `--selection-policy client_ranked`, `--max-assignees 1-8`, `--offer-window 1s-15m`, and `--expires-in` after that window and at most seven days. |
| `list` | List authorized requests with optional `--state`, `--phase`, `--role candidate|coordinator`, `--limit 1-100`, and cursor. |
| `show ID` | Read the request, opening message, visible candidates/offers, selections, and claims. Message content remains untrusted input. |
| `offer ID` | Submit one ordinary offer message with body, optional subject/payload, and required idempotency key. |
| `decline ID` | Record that the candidate will not offer; an idempotency key is optional. |
| `select ID` | Coordinator-only client-ranked choice after the offer deadline or once no candidate remains pending. `--selected-agent` is repeatable or comma-separated; `--reservation 30s-15m` and an idempotency key fence the mutation. `max_assignees` is a ceiling, so selecting fewer is valid. |
| `cancel ID` | Coordinator-only cancellation. Existing reservations/claims become unusable. |
| `claim ID` | Convert this selected agent's reservation into a processing lease using `--lease 30s-15m` and a required idempotency key. |
| `renew ID` | Extend the exact `--claim mrc_...` and positive `--generation` fence. |
| `release ID` | Release the exact fence. `--deterministic-failure` is reserved for durable foreground-client failure accounting. |
| `complete ID` | Atomically validate the exact fence, create an ordinary result message back to the coordinator, and complete the claim. The request closes when that selected batch has no other live reservation or claim, even if `max_assignees` was larger. Requires a non-empty body and idempotency key. |

Persisted request states are `open`, `completed`, `cancelled`, and `expired`;
the effective open phase is derived as `collecting_offers`,
`awaiting_selection`, or `assigned`. Selection creates no model work on the
backend. If the coordinator client goes offline, the request remains durably
awaiting its decision until it expires or is cancelled; deleting the coordinator
agent system-cancels its open requests and live claims. Deleting a candidate
declines a pending response and cancels that agent's live claims while retaining
historical offers. There is no first-offer or first-eligible fallback.

## `witself federation`

Manage the realm's cross-realm federation: the deny-by-default allow-list of
peer realms the realm accepts messages from, and the signed realm card the realm
publishes so peers can discover and verify it. Federation is the trust substrate
under cross-realm `message send`; allow-listing a peer and the per-conversation
consent step gate which peers are accepted, while the cross-realm send itself
still uses ordinary `message:send`. These commands require the
`federation:manage` operator scope. The federation model, signed cards, and
deny-by-default trust are tracked in
[agent-collaboration.md](agent-collaboration.md).

### `witself federation peers`

Manage the realm's accepted-peer allow-list. Removing a peer from the allow-list
is a deny; federation does not happen by default.

```sh
ws federation peers list
ws federation peers allow acme-research --reason "joint research program"
ws federation peers remove acme-research --reason "engagement ended"
```

#### `witself federation peers list`

List the realm handles the realm currently accepts as federation peers.

Flags:

| Flag | Description |
|---|---|
| `--include-disabled` | Include previously removed or suspended peers when available. |

#### `witself federation peers allow REALM_HANDLE`

Add a peer realm handle to the allow-list. The handle (and its published signing
key) is the unit of trust.

Flags:

| Flag | Description |
|---|---|
| `--endpoint URL` | Peer realm endpoint when it cannot be resolved automatically. |
| `--dry-run` | Validate the peer and show the planned allow-list change without applying it. |
| `--yes` | Skip confirmation. |
| `--reason TEXT` | Audit reason. |

#### `witself federation peers remove REALM_HANDLE`

Remove a peer realm handle from the allow-list. This is a deny that takes effect
for subsequent cross-realm traffic.

Flags:

| Flag | Description |
|---|---|
| `--dry-run` | Show the planned removal and its effect on accepted peers without applying it. |
| `--yes` | Skip confirmation. |
| `--reason TEXT` | Audit reason. |

### `witself federation card`

Manage the realm's signed federation card. The card advertises the realm handle,
its agents and their skills, the endpoint, accepted auth, delivery modes, and the
public signing key; it is signed by the realm and served for peer discovery and
verification. Card signing is mandatory.

```sh
ws federation card publish
ws federation card rotate --reason "scheduled key rotation"
```

#### `witself federation card publish`

Publish or re-publish the realm's signed card so peers can discover and verify
the realm.

Flags:

| Flag | Description |
|---|---|
| `--ttl DURATION` | Card validity/expiry window when supported. |
| `--dry-run` | Validate and show the planned card contents without publishing. |
| `--reason TEXT` | Audit reason. |

#### `witself federation card rotate`

Rotate the realm's signing key and re-publish the card under the new key.

Flags:

| Flag | Description |
|---|---|
| `--dry-run` | Show the planned rotation and re-publish without applying it. |
| `--yes` | Skip confirmation. |
| `--reason TEXT` | Audit reason. |

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
ws reference parse witself://agent/archivist/fact/home-region
ws reference parse witself://memory/mem_01H... --json
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
ws reference resolve witself://fact/email
ws reference resolve witself://agent/archivist/memory/mem_01H... --reason "operator lookup"
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
ws agent create browser-agent --token-out /run/secrets/witself-agent-token
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

### `witself agent peers`

List every other agent in the caller's token-derived realm together with each
peer's last observed activity. This is an agent-profile command, not an
operator inventory command: the authenticated agent is excluded by the server,
and callers cannot target another realm. Activity is observational and does not
claim that a peer is online, available, or accepting work.

```sh
witself agent peers --agent scott
witself agent peers --account default --realm default --agent scott --json
```

`--account` and `--realm` use the normal local defaults (`WITSELF_ACCOUNT` /
`WITSELF_REALM`, then `default`) when omitted. `--agent` uses `WITSELF_AGENT`
when present. The text view shows a compact age such as `2m ago`, `5h ago`, or
`never` beside the UTC activity timestamp; JSON returns the exact optional
`last_activity_at`, `last_runtime`, `last_location`, and `last_event` fields.

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

The current implemented self-hosted slice supports:

```sh
ws token create --endpoint URL --token-file OPERATOR_TOKEN_FILE --agent AGENT_ID
ws token create --endpoint URL --token-file OPERATOR_TOKEN_FILE --operator
ws token create --endpoint URL --token-file OPERATOR_TOKEN_FILE --operator --name "deploy bot" --ttl 24h --out ./operator.token
```

`--operator` mints another token for the authenticated operator record. It does
not create a named additional operator yet; `--name` labels the token for later
list/show surfaces. Operator listing, additional named operator records, and
safe operator deletion are tracked as the next lifecycle slice.

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
ws export --out ./self.json
ws export --agent archivist --out ./archivist-self/ --format dir
ws export --no-sensitive --out ./self-public.json
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
ws import --in ./self.json
ws import --in ./archivist-self/ --remap-agent archivist=archivist-copy --dry-run
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

The implemented server starts through an installed runtime binding:

```sh
witself mcp serve --runtime codex
witself mcp serve --runtime claude-code
```

The current full server exposes 67 tools across self and realm-safe peer
activity, deterministic facts and
fact review/deletion, transcripts, direct-agent messages, the implemented
realm request lifecycle, direct narrative-memory lifecycle/recall/delete
surface, and fifteen client-curation tools documented in
[MCP Tools](mcp-tools.md). The broader catalog and posture below remain the
target contract as additional domain capabilities are wired in.

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
standing protocol. At the start of non-trivial work it teaches the active agent
to call `self.show`, inspect its message checkpoint, and call non-blocking
`message.listen(wait_seconds=0)`, then use focused memory recall when prior
context matters. Listen discovers canonical unacknowledged mailbox work.
Neither startup check exposes content, changes state, or wakes an idle model.
This is the MCP half of the teaching layer;
the pinned text and the file-ecosystem counterpart
([`witself bootstrap-instructions`](#witself-bootstrap-instructions)) are tracked
in [context-hydration.md](context-hydration.md). In `--read-only` mode the server
omits implemented mutating memory tools, including `capture`, `adjust`,
`supersede`, lifecycle changes, evidence resolution, and permanent-delete
apply; fact/candidate/subject mutations plus message `send`, `reply`, `read`,
`ack`, `claim`, `renew`, `release`, and `complete` are also omitted. The nine mutating
`message.request.*` operations are omitted, while request `list` and `show`
remain alongside message `list` and non-mutating `listen`. Memory `read`,
`list`, `history`, and `recall` remain alongside the other non-mutating lookup
tools.

Flags:

| Flag | Description |
|---|---|
| `--transport stdio|http` | MCP transport. Initial target: `stdio`. |
| `--listen ADDRESS` | Listen address for HTTP transport. |
| `--auth-token-file PATH` | Token file for HTTP transport auth. |
| `--read-only` | Serve without create/update/forget/delete tools (inspection-only). |
| `--profile full|read-only|curator-preview|curator-apply` | Select the exact MCP surface. Curator profiles require a token with the same authenticated access profile; preview exposes 10 curation-only tools and apply exposes 11. |
| `--token-file PATH` | Override the installed token file, including when launching a short-lived restricted curator profile. |
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
ws completion bash
ws completion zsh
ws completion fish
ws completion powershell
```

Flags:

| Flag | Description |
|---|---|
| `--install` | Install completion for the current shell when supported. |

## `witself avatar`

The avatar CLI mirrors the self and account-operator lifecycle without putting
SVG or structured visual specifications in shell arguments:

```sh
witself avatar show
witself avatar history [--limit 20] [--before-version N]
witself avatar version --version N
witself avatar style show
witself avatar propose --expected-profile-revision N \
  --style-pack-id ID --style-pack-version N --subject-form FORM \
  --description TEXT --spec-file spec.json --svg-file avatar.svg \
  --idempotency-key KEY
witself avatar activate --version N --expected-profile-revision N --idempotency-key KEY
witself avatar rollback --version N --expected-profile-revision N --idempotency-key KEY
witself avatar reset --expected-profile-revision N \
  [--reason-code CODE] --idempotency-key KEY
witself avatar generation fail --expected-profile-revision N \
  --reason-code CODE --idempotency-key KEY

witself avatar operator show --agent-id AGENT
witself avatar operator history --agent-id AGENT [--limit 20] [--before-version N]
witself avatar operator version --agent-id AGENT --version N
witself avatar operator propose --agent-id AGENT ...
witself avatar operator activate --agent-id AGENT ...
witself avatar operator reject --agent-id AGENT ...
witself avatar operator rollback --agent-id AGENT ...
witself avatar operator reset --agent-id AGENT \
  --expected-profile-revision N [--reason-code CODE] --idempotency-key KEY
witself avatar operator policy --agent-id AGENT ...
witself avatar operator quota --agent-id AGENT \
  --retained-payload-count-limit 20 \
  --retained-payload-byte-limit 2097152 \
  --expected-profile-revision N --idempotency-key KEY
witself avatar operator style show --realm-id REALM
witself avatar operator style version --realm-id REALM \
  --style-file style.json --expected-style-revision N --idempotency-key KEY
```

`--spec-file -`/`--spec-stdin` and `--svg-file -`/`--svg-stdin` are supported,
but both payloads cannot consume the same stdin stream. See
[agent-avatars.md](agent-avatars.md).

The self and operator `history` commands print payload-free metadata pages and
preserve `next_before_version` plus the server lifecycle fields for each
immutable version: `is_active`, `is_proposed`, `was_activated`,
`rollback_eligible`, `rejected`, and the optional activation or rejection
timestamps. They also include `payload_state`, original `payload_bytes`,
`svg_sha256`, `locked_layers_sha256`, and optional compaction timestamp/reason.
Pass that cursor back as `--before-version`; use `avatar version` for an exact
read. A `full` exact version includes SVG, description, visual specification,
and provenance. A `compacted` exact version returns retained metadata and
provenance but omits those three creative fields.

`avatar show` and `avatar operator show` report the payload count/byte limits,
current full-payload usage, and the fixed rollback floor. Defaults are 20 full
payloads and 2 MiB per agent. `avatar operator quota` is the only quota mutation
surface; the count range is 4–1000 and the byte range is 524288–67108864. It is
operator-only, revision-fenced, and idempotent. Lowering a limit immediately
compacts eligible inactive payloads as needed in the same transaction. A raise
does not restore compacted data. If the requested state cannot preserve the
active, proposed, and two most recently activated distinct inactive current-lineage
payloads, the command fails closed with `avatar_payload_quota_exceeded` and
leaves both limits and payloads unchanged.

Proposal commands apply the same quota before inserting the new version.
Eligible compaction order is retired lineage, rejected, other never-activated,
then activated versions older than the two-version rollback floor; each class
is oldest first. Compaction is irreversible and exact retry replays never run it
again.

`avatar reset` requires explicit fresh-start intent. It retires the current
lineage without deleting history, returns the profile to its deterministic
placeholder, and makes the next proposal parentless in the new lineage. That
checkpoint reopens the active agent's broad initial fitting: local draft
variants may change form, palette, and defining details, but only the one
agent-chosen final candidate is submitted. It is available to a self token only
under `agent_self_managed`; otherwise an account operator uses `avatar operator
reset`. It is not a permanent purge command.

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
- `witself agent peers`
- `witself token create`
- `witself memory capture`
- `witself memory recall`
- `witself memory show`
- `witself memory list`
- `witself memory history`
- `witself memory adjust`
- `witself memory supersede`
- `witself memory forget`
- `witself memory restore`
- `witself memory reactivate`
- `witself memory evidence resolve`
- `witself memory delete`
- `witself remember` (target)
- `witself self show`
- `witself session start` (target)
- `witself session end` (target)
- `witself digest emit` (target)
- `witself ingest` (target)
- `witself bootstrap-instructions` (target)
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
before managed APIs are ready. It validates the same model-free lexical recall
contract offline and is not a production milestone. This slice is enough to
validate human use, deterministic recall, deterministic facts,
default-deny policy, security groups, inter-agent messaging, identity references,
and round-trippable export/import while the managed backend takes shape. The
open-plane (memory/fact/identity) core ships first; the sealed credential plane
(`password generate`, `secret`, `run`, `totp`) is a defined v0 slice that
validates generated passwords, reveal-gated secrets, runtime injection, and
authenticator-style 2FA, and may be staged after the open-plane core. Sealed-plane
commands also exercise the envelope/KMS dependency, which is required only when
the sealed plane is enabled.
