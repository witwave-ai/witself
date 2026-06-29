# Witself Workflow Scripts

Status: draft. These are script-style product walkthroughs for the most common
human and agent tasks. They are meant to expose CLI gaps before implementation.

The commands are examples of intended behavior. They should become smoke tests
or docs tests after the CLI exists.

Witself is one product with two data planes. These walkthroughs cover both. The
**open plane** is the Witself identity payload: adding and recalling memories,
setting and reading facts, granting cross-agent access through policy, organizing
agents into security groups, exchanging messages, and exporting/importing an
agent's self. The **sealed plane** is agent credential material: creating and
revealing secrets, enrolling TOTP and generating codes, generating passwords, and
injecting secret references into a subprocess at runtime. The platform spine
(install, auth, setup, token files, MCP, self-host, local dev) is shared by both
planes.

The sealed-plane carve-outs hold in every script that touches a secret: secret
values and TOTP seeds are never embedded, never recalled, never in the
self-digest, never ingested, and never plaintext-exported. Plaintext leaves the
sealed plane only through the explicit, audited value-returning operations
(`secret reveal`, `totp code`, value-returning reference resolution, and
`ws run`). See [secret-model.md](secret-model.md).

## 1. Install The CLI

Homebrew:

```sh
brew install witwave-ai/tap/witself
ws version
```

Universal installer:

```sh
curl -fsSL https://raw.githubusercontent.com/witwave-ai/witself/main/install.sh | sh
ws version
```

Optional shell completion:

```sh
ws completion zsh --install
```

Inspect backend capabilities before doing anything expensive:

```sh
ws capabilities --json
```

Expected behavior:

- Install should not require a web dashboard.
- `ws version` should work without auth.
- `ws capabilities` should show the default managed Witself Cloud endpoint
  when no endpoint/profile is configured.
- `ws capabilities` should report the active embedding provider, model, and
  vector dimensionality, and whether semantic recall is degraded.

## 2. First Managed Account, Realm, Agents, Promo Code, And Checkout

Human operator login:

```sh
ws auth login
```

Headless operator login:

```sh
ws auth login --device-code --no-browser
```

Create or select the managed account, create the realm, create agents, write
token files, apply a promo code, and start checkout:

```sh
mkdir -p ./witself-tokens

ws setup \
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
ws billing sessions show hps_123 --watch --timeout 10m
```

Verify setup:

```sh
ws whoami --show-permissions
ws billing show --show-plan --show-payment-method
ws billing usage --show-limits
ws agent list
```

Expected behavior:

- `ws setup` defaults to managed Witself Cloud.
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
ws billing payment-methods add \
  --setup \
  --type card \
  --set-default \
  --open \
  --json
```

Subscribe or change plans with a promo code:

```sh
ws billing subscribe team \
  --realm prod \
  --promo-code FOUNDERS25 \
  --checkout \
  --open \
  --json
```

Crypto payment rail example:

```sh
ws billing crypto quote \
  --subscription sub_123 \
  --asset USDC \
  --network base \
  --currency usd \
  --json

ws billing crypto checkout \
  --quote cpq_123 \
  --open \
  --json
```

Expected behavior:

- The CLI never accepts raw card numbers, bank credentials, wallet private keys,
  wallet seed phrases, or raw wallet credentials.
- Hosted flows are resumable through `ws billing sessions show`.
- Crypto payment is a payment rail, not a Witself utility-token requirement.

## 4. Agent Runtime Starts From A Token File

An agent process should be able to start with only a mounted token file:

```sh
export WITSELF_TOKEN_FILE=/run/secrets/witself-agent-token
export WITSELF_REALM=prod

ws whoami --json
ws memory list --json
ws secret list --json
```

Local file example for development:

```sh
export WITSELF_TOKEN_FILE="$PWD/witself-tokens/archivist.token"
ws whoami
```

Expected behavior:

- The token determines the named agent identity.
- Passing `--agent` is not authentication; the actor is always derived from the
  token.
- Ephemeral pods can restart and reuse the same mounted token file.

## 5. Agent Adds And Recalls Memories

Add a memory as the token-bound agent:

```sh
ws memory add \
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
ws memory add \
  --content "Completed the Q2 migration runbook end to end on 2026-06-24." \
  --kind episodic \
  --tag migration \
  --link witself://fact/home-region \
  --json
```

Recall semantically (the core Witself differentiator):

```sh
ws memory recall "what does the operator want from status updates" \
  --kind profile \
  --limit 5 \
  --json
```

Recall blended with filters and time:

```sh
ws memory recall "migration work" \
  --tag migration \
  --since 2026-06-01 \
  --json
```

Read one memory deterministically by id, then adjust it:

```sh
ws memory read mem_123 --json

ws memory adjust mem_123 \
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
ws fact set display-name "Archivist" --json

ws fact set home-region us-east-1 \
  --format string \
  --source self \
  --json
```

Set a sensitive fact (redacted by default in list/scan, but an ordinary
authorized read returns the value — there is no reveal ceremony):

```sh
ws fact set email archivist@example.com \
  --format email \
  --sensitive \
  --json
```

Promote a fact to primary (atomic; demotes any prior primary of the same logical
kind):

```sh
ws fact set email archivist@example.com --primary --json
```

Read facts deterministically by name:

```sh
ws fact get email --json
ws fact get home-region
```

List facts (primary facts surface first; sensitive values redacted by default):

```sh
ws fact list --json
ws fact list --include-sensitive --json
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
ws agent list --json
```

Test whether access would be allowed *before* creating a policy (the canonical
dry-run for access decisions):

```sh
ws policy test \
  --subject coordinator \
  --permission read \
  --target archivist \
  --scope memory \
  --json
```

Create a default-deny-overriding allow policy: let `coordinator` read
`archivist`'s memories, filtered to one kind:

```sh
ws policy create \
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
ws policy test \
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
ws policy create \
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

ws memory recall "operator preferences" \
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
ws group create analysts \
  --description "Agents that share analytical context" \
  --json
```

Add members:

```sh
ws group add-member analysts --agent archivist --json
ws group add-member analysts --agent coordinator --json
```

Show the group and its bound policies:

```sh
ws group show analysts --json
```

Bind a policy with the group as subject (every member inherits the permission)
and another with the group as target:

```sh
ws policy create \
  --subject analysts \
  --permission read \
  --target shared-context \
  --scope memory \
  --json

ws policy create \
  --subject coordinator \
  --permission contribute \
  --target analysts \
  --scope memory \
  --json
```

Write a group-scoped shared memory (collective memory owned by the group, not a
single agent):

```sh
ws memory add \
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

ws message send \
  --to archivist \
  --subject "handoff" \
  --kind request \
  --body "Please record the migration outcome in your episodic memory." \
  --json
```

Send a message to a group (fanned out to current members with per-member
delivery and ack state):

```sh
ws message send \
  --to analysts \
  --subject "sync" \
  --body "Standup notes attached." \
  --payload-file ./notes.json \
  --json
```

Read the inbox as the recipient agent, then read one message and acknowledge it:

```sh
export WITSELF_TOKEN_FILE="$PWD/witself-tokens/archivist.token"

ws message list --unread --json
ws message read msg_123 --json
ws message ack msg_123 --json
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
ws export \
  --agent archivist \
  --include-history \
  --out ./archivist-self.json \
  --json
```

Exporting `sensitive` records is warned-on and requires a reason; operators may
scope exports to non-sensitive records:

```sh
ws export \
  --agent archivist \
  --include-sensitive \
  --reason "operator-requested identity backup" \
  --out ./archivist-self.full.json \
  --json
```

Operator export with realm-level context (policies and group membership):

```sh
ws export \
  --realm prod \
  --include-policies \
  --include-groups \
  --out ./prod-realm-self.json \
  --json
```

Preview an import before persisting, then import into the same or a different
agent (remap mode when the target differs):

```sh
ws import \
  --in ./archivist-self.json \
  --dry-run \
  --json

ws import \
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

## 11. Agent Creates And Reveals A Sealed Secret

The sealed plane is where credential material lives. Where a fact is plainly
readable and a memory is recalled semantically, a secret is enveloped at rest and
crosses into plaintext only through the audited reveal ceremony. See
[secret-model.md](secret-model.md).

Create a login secret, generating the password into a sensitive field so the
value is enveloped immediately and never printed:

```sh
export WITSELF_TOKEN_FILE="$PWD/witself-tokens/archivist.token"
export WITSELF_REALM=prod

ws secret create github/builder \
  --description "GitHub login created by archivist" \
  --template login \
  --field username=archivist@example.com \
  --field url=https://github.com/login \
  --generate-sensitive password \
  --generate-length 40 \
  --generate-no-ambiguous \
  --tag signup \
  --tag github \
  --json
```

Store a token read from stdin so the plaintext never lands in shell history or a
flag value:

```sh
printf '%s' "$GITHUB_PAT" | ws secret create github/pat \
  --description "GitHub personal access token for archivist" \
  --template api-key \
  --field url=https://github.com/settings/tokens \
  --sensitive-stdin api-key \
  --json
```

Show the secret — sensitive fields are redacted, returning a resolvable
`value_ref` instead of plaintext:

```sh
ws secret show github/builder --show-tags --show-access --json
```

Reveal exactly one sensitive field only when the value is actually needed (the
reveal ceremony; audited as `secret.reveal`, metered as `secret_read`):

```sh
ws secret reveal github/builder password --reason "fill signup form"
ws secret reveal github/builder password --json
```

Expected behavior:

- `secret show`/`secret list`/`secret scan` never return sensitive values; they
  return metadata plus `value_ref` placeholders.
- `secret reveal` returns exactly one named field, requires `secret:reveal` (own
  secret) or a matching grant/realm role, and is always audited; the plaintext is
  never written to the audit row, logs, metrics, or errors.
- Client-side decrypt is the default; managed token-only pods use the
  capability-gated `server_side_decrypt` path, flagged in the reveal audit record.
  See [key-hierarchy.md](key-hierarchy.md).
- Sealed-plane carve-out: secret values are never embedded, never recalled, never
  in the self-digest, never ingested, and never plaintext-exported. The agent does
  not need to keep credentials in prompt memory or project files.

## 12. Agent Enrolls TOTP And Generates A Code

Witself can act as the authenticator app. The TOTP seed is high-value sealed
material colocated with a secret; it is never returned by the ordinary agent
surface. `totp:enroll` (the privileged seed path) and `totp:code` (ordinary login
use) are distinct scopes. See [totp-2fa.md](totp-2fa.md).

Enroll TOTP when the service shows an authenticator setup URL or QR code:

```sh
ws totp enroll github/builder \
  --otpauth "$GITHUB_OTPAUTH_URL" \
  --issuer GitHub \
  --account archivist@example.com \
  --json

ws totp enroll github/builder --qr ./github-2fa.png --json
```

Generate a current login code (audited `totp.code`, metered `totp_code`):

```sh
ws totp code github/builder --remaining --json
```

Read the non-sensitive TOTP metadata without touching the seed:

```sh
ws totp show github/builder --json
```

Expected behavior:

- `totp code` returns the current generated code only; it never returns the seed
  and the code is never persisted or audited.
- The seed is returned only through the guarded, audited
  `totp show --reveal-seed` break-glass path under `totp:enroll`.
- Sealed-plane carve-out: the TOTP seed is never embedded, recalled, placed in the
  self-digest, ingested, or plaintext-exported.

## 13. Generate A Password Without Storing It

`ws password generate` produces credentials with consumer-grade controls and
does not persist them. Generated values appear only in the command's output, never
in logs, audit rows, or errors.

```sh
ws password generate --length 40 --no-ambiguous --json
ws password generate --words 5 --json
```

A common flow is generate then create/update a sensitive field in one authorized
step so the value is enveloped immediately rather than round-tripping through the
shell (the `--generate-sensitive` form in section 11 does exactly this).

Expected behavior:

- The generator runs without writing any sealed-plane record.
- Where policy allows, the same generator is exposed via MCP as
  `witself.password.generate`.
- Generated values follow the same redaction rules as revealed secrets.

## 14. Inject Secret References Into A Subprocess With `ws run`

`ws run` resolves `witself://secret/...` references and injects plaintext
into a child process's environment without printing it to stdout, so an agent uses
a credential without ever surfacing it in context, memory, or logs. Each injected
reference is authorized exactly like a reveal and is audited (`secret.reveal`) and
metered (`secret_read` plus `runtime_injection`).

Inject a single reference for one command (output masking is on by default):

```sh
ws run \
  --env GITHUB_TOKEN=witself://secret/github/pat/api-key \
  --mask-output \
  -- gh auth status
```

Use an env file of Witself references — the file is safe to commit because it
holds references, not plaintext:

```sh
cat > .env.witself <<'EOF'
GITHUB_TOKEN=witself://secret/github/pat/api-key
EOF

ws run --env-file .env.witself -- npm test
```

Expected behavior:

- Plaintext exists only in the spawned child's environment; it is never written to
  Witself logs, audit metadata, or the parent's stdout.
- References that cannot be authorized fail the run deterministically before the
  child starts.
- A secret reference is itself safe to store in config and logs because it
  resolves to plaintext only through value-returning operations like `run`.

## 15. Operator Scans Secrets And Grants Access To Another Agent Or Group

Sealed-plane cross-agent access is grant-based and composes with realm roles; it
does **not** use the open-plane cross-agent policy engine from section 7. See
[authorization-and-roles.md](authorization-and-roles.md).

Scan the realm-wide redacted inventory as an operator (no sensitive values are
ever revealed):

```sh
ws secret scan \
  --all-agents \
  --include-group \
  --show-sensitive-counts \
  --show-access \
  --json
```

Preview a grant, then grant `coordinator` redacted read, reveal of one field, and
TOTP code generation on `archivist`'s secret (cross-agent grants require an audit
reason):

```sh
ws secret grant github/builder \
  --owner-agent archivist \
  --agent coordinator \
  --read \
  --reveal password \
  --totp \
  --expires-at 2026-09-30T00:00:00Z \
  --reason "coordinator needs GitHub login for release" \
  --dry-run \
  --json

ws secret grant github/builder \
  --owner-agent archivist \
  --agent coordinator \
  --read \
  --reveal password \
  --totp \
  --reason "coordinator needs GitHub login for release" \
  --json
```

Grant a group access to a group-owned secret so every member resolves it under
group authorization (the unified ownership model — the former `shared` scope is
now a group):

```sh
ws secret create github/org-readonly-token \
  --group analysts \
  --description "Org read-only token shared by the analysts group" \
  --template api-key \
  --sensitive-stdin token \
  --json

ws secret grant github/org-readonly-token \
  --group analysts \
  --agent coordinator \
  --read \
  --reveal token \
  --reason "coordinator consumes the shared org token" \
  --json
```

Now `coordinator` can reveal the granted field and resolve the group reference:

```sh
export WITSELF_TOKEN_FILE="$PWD/witself-tokens/coordinator.token"

ws secret reveal github/builder password \
  --owner-agent archivist \
  --reason "release run"

ws run \
  --env ORG_TOKEN=witself://group/analysts/secret/github/org-readonly-token/token \
  -- ./release.sh
```

Expected behavior:

- `secret scan --all-agents` requires operator/admin permission and never reveals
  sensitive values.
- Cross-agent reveal, grant, copy-with-sensitive, and destructive actions require
  an audit `--reason`, and grant/revoke support `--dry-run`.
- A grant can be field-scoped (`--reveal FIELD`), can include TOTP (`--totp`), and
  can expire (`--expires-at`); revoking is `secret revoke … --field`/`--all`.
- Group-owned secrets replace the old vault-shared concept: ownership is
  `agent | group` across memories, facts, and secrets alike.

## 16. MCP Stdio For An Agent Runtime

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

Inspection-only MCP with sealed-plane value tools disabled (no reveal, no TOTP
codes, no value-returning reference resolution):

```json
{
  "mcpServers": {
    "witself-readonly": {
      "command": "witself",
      "args": ["mcp", "serve", "--read-only", "--no-value-tools"],
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
  secret show/list/scan, totp show, policy test, group show, message read) and
  disables all mutations for safer agent contexts.
- `--no-value-tools` disables the sealed-plane value-returning tools
  (`witself.secret.reveal`, `witself.totp.code`, and value-returning
  `witself.reference.resolve`) while leaving metadata reads available.
- The MCP catalog covers the open plane (memory add/adjust/read/recall/list/forget,
  fact set/get/list/delete, policy test, group list/show, message send/list/read,
  reference parse/resolve) and the sealed plane (secret
  create/list/show/reveal/update, totp enroll/code/show, password generate);
  secrets are never placed in the self-digest and high-risk admin actions are
  operator-only.

## 17. Self-Hosted Bootstrap

Install with Helm:

```sh
helm install witself oci://ghcr.io/witwave-ai/charts/witself-server \
  --version 0.1.0 \
  --namespace witself \
  --create-namespace \
  --values ./witself-values.yaml
```

Verify the Kubernetes rollout, health probes, and metrics endpoint:

```sh
kubectl -n witself rollout status deploy/witself

kubectl -n witself port-forward deploy/witself 8080:8080 8081:8081 9090:9090

curl -fsS http://127.0.0.1:8081/livez
curl -fsS http://127.0.0.1:8081/readyz
curl -fsS http://127.0.0.1:8081/startupz
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
ws setup \
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
- When the sealed plane is enabled, a configured KMS provider
  (`WITSELF_KMS_PROVIDER` / `WITSELF_KMS_KEY_ID`) is a required dependency and
  gates readiness; the open plane does not depend on KMS. See
  [self-hosting.md](self-hosting.md) and [key-hierarchy.md](key-hierarchy.md).

## 18. Local Development Mode

Initialize a local development realm and store. Local mode uses the `local-dev`
embedding provider so semantic recall can be exercised offline:

```sh
ws setup --local \
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

ws memory add \
  --content "Local development demo memory." \
  --kind note \
  --tag demo \
  --json

ws memory recall "demo" --json
```

Expected behavior:

- Local mode is labeled development-only and is not a production setup path.
- Local mode persists the serialized identity store at rest with ordinary
  data-at-rest protection and atomic writes.
- The `local-dev` embedding provider lets semantic recall run offline without a
  paid provider.
- Local behavior still uses the shared core, JSON, policy, audit, and storage
  interfaces.
- Sealed-plane work in local mode uses the `local-dev` KMS provider
  (`WITSELF_KMS_PROVIDER=local-dev`) so secret create/reveal and TOTP can be
  exercised offline; it is development-only and not a production key path. See
  [encryption-model.md](encryption-model.md).

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
- `ws export`/`ws import` need `--dry-run`, `--include-history`,
  `--include-sensitive` (with `--reason`), `--remap`, and dangling-reference
  reporting to be round-trippable.
- Promo codes need to be first-class on `ws setup`, `ws account
  create`, and `ws billing subscribe`.
- Hosted provider flows need `--open`/`--no-open` and a generic `ws billing
  sessions show` command so the CLI owns browser handoff without a dashboard.
- Token-file conflicts must remain explicit; setup should not overwrite token
  files during reruns unless token rotation was chosen.
- `secret reveal` and `totp code` must be the only value-returning sealed-plane
  ops besides `ws run` and the value-returning MCP `reference.resolve`, each
  requiring `secret:reveal`/`totp:code` (or a grant/realm role), an audit
  `--reason` for cross-agent use, and a `secret.reveal`/`totp.code` audit event.
- `secret create` needs flag/file/stdin field inputs (`--field`, `--field-file`,
  `--sensitive-stdin`, `--generate-sensitive`) so plaintext never has to pass
  through a shell flag, and `--group`/`--owner-agent` for ownership and operator
  targeting.
- `secret grant` needs field-scoped reveal (`--reveal FIELD`), `--totp`,
  `--expires-at`, `--dry-run`, and a required `--reason`, composing with realm
  roles rather than the open-plane cross-agent policy engine.
- `mcp serve` needs `--no-value-tools` (distinct from `--read-only`) to disable
  reveal/`totp code`/value-returning reference resolution while leaving
  sealed-plane metadata reads available.
- The sealed-plane carve-outs must hold across every command: secret values and
  TOTP seeds are never embedded, recalled, in the self-digest, ingested, or
  plaintext-exported, and KMS is a required dependency only when the sealed plane
  is enabled.

## Related Docs

- [cli-command-surface.md](cli-command-surface.md)
- [requirements.md](requirements.md)
- [v0-scope.md](v0-scope.md)
- [memory-model.md](memory-model.md)
- [facts-model.md](facts-model.md)
- [secret-model.md](secret-model.md)
- [totp-2fa.md](totp-2fa.md)
- [secret-size-and-attachments.md](secret-size-and-attachments.md)
- [encryption-model.md](encryption-model.md)
- [key-hierarchy.md](key-hierarchy.md)
- [authorization-and-roles.md](authorization-and-roles.md)
- [access-policy.md](access-policy.md)
- [security-groups.md](security-groups.md)
- [inter-agent-messaging.md](inter-agent-messaging.md)
- [backup-and-recovery.md](backup-and-recovery.md)
- [billing-and-limits.md](billing-and-limits.md)
- [operator-auth.md](operator-auth.md)
- [mcp-tools.md](mcp-tools.md)
- [self-hosting.md](self-hosting.md)
- [observability-and-operations.md](observability-and-operations.md)
- [data-model.md](data-model.md)
- [json-contracts.md](json-contracts.md)
