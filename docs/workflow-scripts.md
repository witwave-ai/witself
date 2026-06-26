# Witself Workflow Scripts

Status: draft. These are script-style product walkthroughs for the most common
human and agent tasks. They are meant to expose CLI gaps before implementation.

The commands are examples of intended behavior. They should become smoke tests
or docs tests after the CLI exists.

Where Witpass walkthroughs centered on creating, revealing, and injecting
secrets, these center on the Witself payload: adding and recalling memories,
setting and reading facts, granting cross-agent access through policy, organizing
agents into security groups, exchanging messages, and exporting/importing an
agent's self. The platform spine (install, auth, setup, token files, MCP,
self-host, local dev) is reused unchanged.

## 1. Install The CLI

Homebrew:

```sh
brew install witwave-ai/tap/witself
witself version
```

Universal installer:

```sh
curl -fsSL https://raw.githubusercontent.com/witwave-ai/witself/main/install.sh | sh
witself version
```

Optional shell completion:

```sh
witself completion zsh --install
```

Inspect backend capabilities before doing anything expensive:

```sh
witself capabilities --json
```

Expected behavior:

- Install should not require a web dashboard.
- `witself version` should work without auth.
- `witself capabilities` should show the default managed Witself Cloud endpoint
  when no endpoint/profile is configured.
- `witself capabilities` should report the active embedding provider, model, and
  vector dimensionality, and whether semantic recall is degraded.

## 2. First Managed Account, Realm, Agents, Promo Code, And Checkout

Human operator login:

```sh
witself auth login
```

Headless operator login:

```sh
witself auth login --device-code --no-browser
```

Create or select the managed account, create the realm, create agents, write
token files, apply a promo code, and start checkout:

```sh
mkdir -p ./witself-tokens

witself setup \
  --account "Acme Agents" \
  --email ops@example.com \
  --billing-email billing@example.com \
  --realm prod \
  --agent archivist \
  --agent coordinator \
  --token-dir ./witself-tokens \
  --plan team \
  --promo-code FOUNDERS25 \
  --checkout \
  --open \
  --json > ./witself-setup.json
```

If the checkout remains pending, inspect or watch the hosted provider session:

```sh
witself billing sessions show hps_123 --watch --timeout 10m
```

Verify setup:

```sh
witself whoami --show-permissions
witself billing show --show-plan --show-payment-method
witself billing usage --show-limits
witself agent list
```

Expected behavior:

- `witself setup` defaults to managed Witself Cloud.
- Account, realm, and agent creation are idempotent by name.
- Token files are owner-only and are not overwritten unless reuse or rotation
  was explicitly requested.
- Promo code failure should be a clear billing/setup error, not a partial
  identity setup failure.
- Hosted checkout output should include a session ID, URL, expiration, and
  next command.

## 3. Add Billing Info Later

Billing reuses the Witpass managed apparatus verbatim; this is a brief stub.
See [billing-and-limits.md](billing-and-limits.md) for plans, metered
dimensions, soft/hard limits, and crypto rails.

Add a payment method after setup:

```sh
witself billing payment-methods add \
  --setup \
  --type card \
  --set-default \
  --open \
  --json
```

Subscribe or change plans with a promo code:

```sh
witself billing subscribe team \
  --realm prod \
  --promo-code FOUNDERS25 \
  --checkout \
  --open \
  --json
```

Crypto payment rail example:

```sh
witself billing crypto quote \
  --subscription sub_123 \
  --asset USDC \
  --network base \
  --currency usd \
  --json

witself billing crypto checkout \
  --quote cpq_123 \
  --open \
  --json
```

Expected behavior:

- The CLI never accepts raw card numbers, bank credentials, wallet private keys,
  wallet seed phrases, or raw wallet credentials.
- Hosted flows are resumable through `witself billing sessions show`.
- Crypto payment is a payment rail, not a Witself utility-token requirement.

## 4. Agent Runtime Starts From A Token File

An agent process should be able to start with only a mounted token file:

```sh
export WITSELF_TOKEN_FILE=/run/secrets/witself-agent-token
export WITSELF_REALM=prod

witself whoami --json
witself memory list --json
```

Local file example for development:

```sh
export WITSELF_TOKEN_FILE="$PWD/witself-tokens/archivist.token"
witself whoami
```

Expected behavior:

- The token determines the named agent identity.
- Passing `--agent` is not authentication; the actor is always derived from the
  token.
- Ephemeral pods can restart and reuse the same mounted token file.

## 5. Agent Adds And Recalls Memories

Add a memory as the token-bound agent:

```sh
witself memory add \
  --content "Operator prefers terse status updates, no preamble." \
  --kind profile \
  --tag preferences \
  --tag operator \
  --salience 0.8 \
  --source self \
  --json
```

Add an episodic memory linked to a fact:

```sh
witself memory add \
  --content "Completed the Q2 migration runbook end to end on 2026-06-24." \
  --kind episodic \
  --tag migration \
  --link witself://fact/home-region \
  --json
```

Recall semantically (the core Witself differentiator):

```sh
witself memory recall "what does the operator want from status updates" \
  --kind profile \
  --limit 5 \
  --json
```

Recall blended with filters and time:

```sh
witself memory recall "migration work" \
  --tag migration \
  --since 2026-06-01 \
  --json
```

Read one memory deterministically by id, then adjust it:

```sh
witself memory read mem_123 --json

witself memory adjust mem_123 \
  --content "Operator prefers terse status updates and a one-line TL;DR." \
  --salience 0.9 \
  --json
```

Expected behavior:

- `memory recall` is semantic by default; vector similarity is blended with
  keyword, tag, kind, recency, and salience ranking.
- `memory read`/`memory list` work by id/metadata and do not require the
  embedding provider.
- If the embedding provider is unavailable, `recall` degrades deterministically
  to keyword/tag/kind/time ranking and the result surfaces the degraded state;
  it never silently returns unranked or empty results.
- `memory adjust` appends a new version to edit history; prior versions are
  retained for audit and export.

## 6. Agent Sets Facts And Promotes A Primary

Set facts (upsert by name within the owning agent):

```sh
witself fact set display-name "Archivist" --json

witself fact set home-region us-east-1 \
  --format string \
  --source self \
  --json
```

Set a sensitive fact (redacted by default in list/scan, but an ordinary
authorized read returns the value — there is no reveal ceremony):

```sh
witself fact set email archivist@example.com \
  --format email \
  --sensitive \
  --json
```

Promote a fact to primary (atomic; demotes any prior primary of the same logical
kind):

```sh
witself fact set email archivist@example.com --primary --json
```

Read facts deterministically by name:

```sh
witself fact get email --json
witself fact get home-region
```

List facts (primary facts surface first; sensitive values redacted by default):

```sh
witself fact list --json
witself fact list --include-sensitive --json
```

Expected behavior:

- Lookup is deterministic by name: `fact get email` returns the one true value.
- Setting `--primary` is an atomic promotion that demotes the prior primary of
  the same logical kind; at most one primary per logical kind per owner.
- Primary facts are identity anchors and are surfaced first in `whoami`,
  profile, and export output.
- Only `sensitive` facts are redacted by default; reading one is an ordinary
  authorized read.

## 7. Operator Grants Cross-Agent Access With Policy

List agents:

```sh
witself agent list --json
```

Test whether access would be allowed *before* creating a policy (the canonical
dry-run for access decisions):

```sh
witself policy test \
  --subject coordinator \
  --permission read \
  --target archivist \
  --scope memory \
  --json
```

Create a default-deny-overriding allow policy: let `coordinator` read
`archivist`'s memories, filtered to one kind:

```sh
witself policy create \
  --subject coordinator \
  --permission read \
  --target archivist \
  --scope memory \
  --filter-kind profile \
  --description "coordinator can read archivist profile memories" \
  --json
```

Confirm the decision now flips to allow, returning the deciding policy id:

```sh
witself policy test \
  --subject coordinator \
  --permission read \
  --target archivist \
  --scope memory \
  --filter-kind profile \
  --json
```

Grant a more dangerous verb with guardrails — `curate` requires a reason and
supports dry-run:

```sh
witself policy create \
  --subject coordinator \
  --permission curate \
  --target archivist \
  --scope memory \
  --reason "coordinator maintains archivist's shared profile" \
  --dry-run \
  --json
```

Now `coordinator` can recall over `archivist`'s memories (policy-gated, metered
as a cross-agent access):

```sh
export WITSELF_TOKEN_FILE="$PWD/witself-tokens/coordinator.token"

witself memory recall "operator preferences" \
  --owner-agent archivist \
  --json
```

Expected behavior:

- Cross-agent access is default-deny; absence of a matching allow policy is a
  deny, and `policy.access_denied` is audited.
- `policy test` returns the deciding policy id or a deny reason, via CLI and MCP.
- `curate` and `forget` across agents require an audit `--reason`, support
  `--dry-run`, and require confirmation unless `--yes`.
- Every cross-agent mutation is fully attributed in audit (for example "memory
  `mem_…` of agent A was curated by agent B under policy `pol_…`").

## 8. Operator Creates A Security Group And Adds Members

Create a group within the realm:

```sh
witself group create analysts \
  --description "Agents that share analytical context" \
  --json
```

Add members:

```sh
witself group add-member analysts --agent archivist --json
witself group add-member analysts --agent coordinator --json
```

Show the group and its bound policies:

```sh
witself group show analysts --json
```

Bind a policy with the group as subject (every member inherits the permission)
and another with the group as target:

```sh
witself policy create \
  --subject analysts \
  --permission read \
  --target shared-context \
  --scope memory \
  --json

witself policy create \
  --subject coordinator \
  --permission contribute \
  --target analysts \
  --scope memory \
  --json
```

Write a group-scoped shared memory (collective memory owned by the group, not a
single agent):

```sh
witself memory add \
  --group analysts \
  --content "Shared finding: Q2 latency regressed after the cache change." \
  --kind semantic \
  --tag finding \
  --json
```

Expected behavior:

- A security group is both a policy subject and a policy target.
- Membership is managed by operators and by agents holding `group:manage`.
- As subject, a group grants every member the policy's permission on the target.
- Group-owned records use the same `mem_`/`fact_` shapes with a group owner, and
  group-owned destructive actions follow the cross-agent guardrails (`--reason`,
  `--dry-run`, confirmation, soft-delete by default).

## 9. Agents Exchange Messages

Send a message to another agent. The sender is always derived from the token,
never from input:

```sh
export WITSELF_TOKEN_FILE="$PWD/witself-tokens/coordinator.token"

witself message send \
  --to archivist \
  --subject "handoff" \
  --kind request \
  --body "Please record the migration outcome in your episodic memory." \
  --json
```

Send a message to a group (fanned out to current members with per-member
delivery and ack state):

```sh
witself message send \
  --to analysts \
  --subject "sync" \
  --body "Standup notes attached." \
  --payload-file ./notes.json \
  --json
```

Read the inbox as the recipient agent, then read one message and acknowledge it:

```sh
export WITSELF_TOKEN_FILE="$PWD/witself-tokens/archivist.token"

witself message list --unread --json
witself message read msg_123 --json
witself message ack msg_123 --json
```

Expected behavior:

- `from` is always derived from the authenticated token; sender forgery is
  structurally impossible through the API.
- Message bodies and payloads are untrusted input to the receiving agent,
  especially when a message would drive a memory or fact write.
- A message grants no policy by itself; a message-driven cross-agent write still
  requires a policy.
- Send, deliver, and read are rate-limited, scope-gated (`message:send`,
  `message:read`), audited, and metered.

## 10. Export And Import An Agent's Self

Export an agent's self as structured, round-trippable, plaintext identity data
(the deliberate inverse of Witpass's encrypted-only export stance):

```sh
witself export \
  --agent archivist \
  --include-history \
  --out ./archivist-self.json \
  --json
```

Exporting `sensitive` records is warned-on and requires a reason; operators may
scope exports to non-sensitive records:

```sh
witself export \
  --agent archivist \
  --include-sensitive \
  --reason "operator-requested identity backup" \
  --out ./archivist-self.full.json \
  --json
```

Operator export with realm-level context (policies and group membership):

```sh
witself export \
  --realm prod \
  --include-policies \
  --include-groups \
  --out ./prod-realm-self.json \
  --json
```

Preview an import before persisting, then import into the same or a different
agent (remap mode when the target differs):

```sh
witself import \
  --in ./archivist-self.json \
  --dry-run \
  --json

witself import \
  --in ./archivist-self.json \
  --target-agent archivist-restored \
  --remap \
  --reason "restore from backup into a new agent" \
  --json
```

Expected behavior:

- Export defaults to JSON using the `witself.v0` schema; a diff-friendly
  directory/file layout is also supported.
- Export preserves memories (content, kind, tags, source, salience, links,
  timestamps, edit history), facts (values, `primary`/`sensitive` flags, format,
  history), and identity anchors.
- `witself://…` references are preserved on export and re-resolved on import;
  dangling references are reported, not silently dropped.
- Import is idempotent by stable id where ids are preserved, supports
  rename/remap, is audited, and supports `--dry-run`.

## 11. MCP Stdio For An Agent Runtime

Example MCP server configuration:

```json
{
  "mcpServers": {
    "witself": {
      "command": "witself",
      "args": ["mcp", "serve"],
      "env": {
        "WITSELF_TOKEN_FILE": "/run/secrets/witself-agent-token",
        "WITSELF_REALM": "prod"
      }
    }
  }
}
```

Inspection-only MCP:

```json
{
  "mcpServers": {
    "witself-readonly": {
      "command": "witself",
      "args": ["mcp", "serve", "--read-only"],
      "env": {
        "WITSELF_TOKEN_FILE": "/run/secrets/witself-agent-token"
      }
    }
  }
}
```

Expected behavior:

- MCP stdio is the v0 transport.
- MCP uses the token-bound identity and the same authorization as the CLI.
- `--read-only` restricts the session to inspection (recall/read/list, fact get,
  policy test, group show, message read) for safer agent contexts.
- The MCP catalog covers memory add/adjust/read/recall/list/forget, fact
  set/get/list/delete, policy test, group list/show, message send/list/read, and
  reference parse/resolve; high-risk admin actions are operator-only.

## 12. Self-Hosted Bootstrap

Install with Helm:

```sh
helm install witself oci://ghcr.io/witwave-ai/charts/witself \
  --version 0.1.0 \
  --namespace witself \
  --create-namespace \
  --values ./witself-values.yaml
```

Verify the Kubernetes rollout, health probes, and metrics endpoint:

```sh
kubectl -n witself rollout status deploy/witself

kubectl -n witself port-forward deploy/witself 8080:8080 8081:8081 9090:9090

curl -fsS http://127.0.0.1:8081/v1/health/live
curl -fsS http://127.0.0.1:8081/v1/health/ready
curl -fsS http://127.0.0.1:8081/v1/health/startup
curl -fsS http://127.0.0.1:9090/metrics | head
```

Run migrations when appropriate (Postgres with pgvector is the system of
record):

```sh
witself-server migrate status --config ./witself-server.toml
witself-server migrate up --config ./witself-server.toml
```

Create a one-time first-operator bootstrap token:

```sh
witself-server bootstrap token \
  --config ./witself-server.toml \
  --ttl 15m \
  --out ./bootstrap.token
```

Bootstrap the self-hosted operator context, realm, and agents:

```sh
witself setup \
  --endpoint https://witself.internal.example.com \
  --bootstrap-token-file ./bootstrap.token \
  --account "Acme Agents" \
  --realm prod \
  --agent archivist \
  --token-out archivist=./witself-tokens/archivist.token \
  --json
```

Expected behavior:

- Self-hosted setup is explicit through `--endpoint`.
- There is no default admin username/password.
- The bootstrap token is short-lived, single-use, and not an ordinary operator
  token.
- The chart owns Kubernetes probes and metrics wiring through values.
- The self-hosted backend needs Postgres with pgvector and a configured
  embedding provider (`voyage`, `openai`, or `local-dev`); `witself
  capabilities` reports the active provider and whether recall is degraded.

## 13. Local Development Mode

Initialize a local development realm and store. Local mode uses the `local-dev`
embedding provider so semantic recall can be exercised offline:

```sh
witself setup --local \
  --realm dev \
  --store-file ./witself.store.json \
  --agent archivist \
  --token-out archivist=./witself-tokens/archivist.token \
  --json
```

Use it:

```sh
export WITSELF_STORE_FILE="$PWD/witself.store.json"
export WITSELF_TOKEN_FILE="$PWD/witself-tokens/archivist.token"
export WITSELF_EMBEDDINGS_PROVIDER=local-dev

witself memory add \
  --content "Local development demo memory." \
  --kind note \
  --tag demo \
  --json

witself memory recall "demo" --json
```

Expected behavior:

- Local mode is labeled development-only and is not a production setup path.
- Local mode persists the serialized identity store at rest with ordinary
  data-at-rest protection and atomic writes.
- The `local-dev` embedding provider lets semantic recall run offline without a
  paid provider.
- Local behavior still uses the shared core, JSON, policy, audit, and storage
  interfaces.

## Gaps Found By The Scripts

These scripts exposed command-surface requirements that should stay in the v0
spec:

- `memory recall` needs first-class filter flags (`--kind`, `--tag`, `--since`,
  `--limit`, `--owner-agent`) and must surface degraded-recall state in both
  human and `--json` output.
- `policy test` must be runnable before policy creation as the canonical access
  dry-run, returning either the deciding policy id or a structured deny reason.
- Cross-agent recall/read/curate/forget need a consistent `--owner-agent` (and
  `--owner-group`) targeting flag distinct from authentication.
- `fact set --primary` must be an atomic promotion that demotes the prior
  primary of the same logical kind, reported as `fact.primary_changed`.
- Group-scoped writes need a `--group` owner flag shared by `memory add` and
  `fact set`, and group-owned destructive actions must reuse the cross-agent
  guardrails.
- `message send` must derive `from` from the token only, and reject any attempt
  to set the sender via input.
- `witself export`/`witself import` need `--dry-run`, `--include-history`,
  `--include-sensitive` (with `--reason`), `--remap`, and dangling-reference
  reporting to be round-trippable.
- Promo codes need to be first-class on `witself setup`, `witself account
  create`, and `witself billing subscribe`.
- Hosted provider flows need `--open`/`--no-open` and a generic `witself billing
  sessions show` command so the CLI owns browser handoff without a dashboard.
- Token-file conflicts must remain explicit; setup should not overwrite token
  files during reruns unless token rotation was chosen.

## Related Docs

- [cli-command-surface.md](cli-command-surface.md)
- [requirements.md](requirements.md)
- [v0-scope.md](v0-scope.md)
- [memory-model.md](memory-model.md)
- [facts-model.md](facts-model.md)
- [access-policy.md](access-policy.md)
- [security-groups.md](security-groups.md)
- [inter-agent-messaging.md](inter-agent-messaging.md)
- [backup-and-recovery.md](backup-and-recovery.md)
- [billing-and-limits.md](billing-and-limits.md)
- [operator-auth.md](operator-auth.md)
- [mcp-tools.md](mcp-tools.md)
- [self-hosting.md](self-hosting.md)
- [observability-and-operations.md](observability-and-operations.md)
- [json-contracts.md](json-contracts.md)
