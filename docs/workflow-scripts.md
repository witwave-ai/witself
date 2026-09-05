# Witself Workflow Scripts

Status: current CLI walkthroughs, reviewed 2026-09-04 against the
[command dispatcher](../cmd/witself/main.go), its handlers, and `witself --help`.
Sections marked **Roadmap** describe unshipped workflows and contain no runnable
Witself commands. The broader target contract lives in
[cli-command-surface.md](cli-command-surface.md).

Replace example account names, addresses, IDs, file paths, and idempotency keys
with your own. Reuse a key only when retrying the same logical operation. Put
flags before positional arguments; several commands use Go's flag parser without
support for trailing flags.

Witself's open plane stores facts, narrative memories, transcripts, and messages.
The sealed plane stores client-encrypted agent secrets. Narrative capture and
curation are client-authored; PostgreSQL lexical recall is the baseline, optional
vectors are client-supplied, and the backend performs no model inference. See
[narrative-memory-and-curation.md](narrative-memory-and-curation.md).

Sealed-value operations require the agent's client-held vault key as well as its token.
The backend holds no AVK key material and offers no server-side decrypt path.
Ordinary secret inventory is redacted; explicit secret reveal and TOTP commands
return values through the active client. Archives contain encrypted sealed
values, never AVKs or plaintext secret/TOTP values. See
[ADR 0003](decisions/0003-client-custodied-agent-vault.md) and the
[client-custodied vault contract](client-custodied-agent-vault.md).

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

Windows x64, from Windows PowerShell 5.1 or newer:

```powershell
[Net.ServicePointManager]::SecurityProtocol = [Net.SecurityProtocolType]::Tls12
irm https://raw.githubusercontent.com/witwave-ai/witself/main/install.ps1 | iex
witself version
witself integrations
```

This installs `witself.exe` and the real `ws.exe` alias under the current
user's `%LOCALAPPDATA%\Witself\bin`. The installer verifies SHA-256 before
extraction and does not require administrator rights. Windows x64 CI exercises
the binary installer and the isolated Codex integration contract; that is not
yet an end-to-end certification of a signed-in Codex model or of every Witself
provider on Windows.

Inspect the installed command list:

```sh
witself --help
```

`witself version` and `witself --help` work without authentication. Shell
completion and a standalone capability-discovery command remain roadmap
surface; they are not dispatched by this CLI.

## 2. First Managed Account, Realm, And Agents

Read the published terms and privacy policy, then create a managed account:

```sh
witself legal terms
witself legal privacy
witself account create \
  --name acme \
  --email ops@example.com \
  --display-name "Acme Agents" \
  --accept-terms
```

When signup requests a Turnstile challenge, open the URL printed by the CLI,
complete the check, and rerun the same command with `--challenge TOKEN`.
Complete email verification and inspect the account status before provisioning
agents:

```sh
witself account status --account acme --json
```

Account creation saves the account binding and operator credential locally.
The managed path uses that binding; `auth login` is the bootstrap-token exchange
shown in section 17, not a browser or device-code login.

Create the realm and agents, substituting the realm ID returned by creation:

```sh
witself realm create --account acme prod
witself realm list --account acme --json
witself agent create --account acme --realm REALM_ID archivist
witself agent create --account acme --realm REALM_ID coordinator
witself agent list --account acme --realm REALM_ID --json
```

Mint a token for each returned agent ID. With a managed account and no `--out`,
the CLI writes each full agent token into its canonical local account/realm/agent
path:

```sh
witself token create --account acme --agent ARCHIVIST_AGENT_ID
witself token create --account acme --agent COORDINATOR_AGENT_ID
```

Use the saved account for the billing and agent examples that follow:

```sh
export WITSELF_ACCOUNT=acme
export WITSELF_REALM=prod
export WITSELF_AGENT=archivist
```

The combined setup command, setup-time plan/promo flags, and hosted-session
watch command remain roadmap surface. Use the separate billing commands below.

## 3. Add Billing Info Later

Managed Stripe billing is generally available through the provider-neutral
commands below.
See [billing-and-limits.md](billing-and-limits.md) for plans, metered
dimensions, soft/hard limits, and the roadmap-only crypto-rail contract.

Preview, then start the hosted payment-method setup flow:

```sh
witself billing setup \
  --reason "add billing method" \
  --email billing@example.com \
  --dry-run \
  --json

witself billing setup \
  --reason "add billing method" \
  --email billing@example.com \
  --idempotency-key billing-setup-001 \
  --yes \
  --open
```

Preview, then request a plan upgrade:

```sh
witself plan upgrade \
  --reason "upgrade to Team" \
  --dry-run \
  --json \
  team

witself plan upgrade \
  --reason "upgrade to Team" \
  --idempotency-key plan-upgrade-team-001 \
  --yes \
  --json \
  team
```

Inspect the resulting billing state and effective plan:

```sh
witself billing show --json
witself plan status --full --json
```

The CLI uses hosted provider flows and does not accept raw payment credentials.
A plan-upgrade response can include a checkout URL; open the returned URL to
complete that flow. There is no hosted-session watch subcommand.

**Roadmap:** crypto quote/checkout commands and crypto-provider integration are
not implemented. The contract is described in
[billing-and-limits.md](billing-and-limits.md).

## 4. Agent Runtime Starts From Saved Credentials Or A Token File

Use the managed selectors established in section 2:

```sh
witself self show --json
witself memory list --json
witself secret list --json
```

For a mounted token, pass both the endpoint and token file explicitly:

```sh
witself self show \
  --endpoint https://witself.internal.example.com \
  --token-file /run/secrets/witself-agent-token \
  --json
```

The token determines the authenticated agent. `--agent` selects a local
credential and, on the self-digest path, checks the token-bound identity; it does
not grant authority. `WITSELF_TOKEN_FILE` is not read by the CLI's connection
resolver. Sealed-value operations also require a matching local vault key;
section 11 covers that custody requirement.

## 5. Agent Captures And Recalls Memories

Capture client-authored narrative with an explicit evidence status and retry key:

```sh
witself memory capture \
  --content "Completed the Q2 migration runbook on 2026-06-24; the validation step caught a missing index before cutover." \
  --kind lesson \
  --tag migration \
  --salience 0.8 \
  --occurred-from 2026-06-24T00:00:00Z \
  --evidence-unavailable-reason manual-entry \
  --idempotency-key migration-lesson-001 \
  --json
```

Use unavailable evidence only when no exact source can be supplied. For a
recorded interaction, use `--evidence-transcript`, `--evidence-from-sequence`,
and `--evidence-until-sequence` with the actual transcript ID and sequence range.

Recall ranked narrative memory with kind, tag, and event-time filters:

```sh
witself memory recall \
  --kind lesson \
  --tag migration \
  --occurred-from 2026-06-01T00:00:00Z \
  --limit 5 \
  --json \
  "migration validation"
```

Read the returned memory ID and use its current version for an adjustment:

```sh
witself memory show --json mem_123
witself memory adjust \
  --expected-version 1 \
  --salience 0.9 \
  --reason "migration lesson remains useful" \
  --idempotency-key migration-lesson-adjust-001 \
  --json \
  mem_123
```

Expected behavior:

- Capture requires evidence and an idempotency key; adjustment requires the
  current version and its own idempotency key.
- Recall uses a deterministic PostgreSQL lexical baseline. Optional compatible
  client-supplied vectors can contribute to hybrid ranking; the backend never
  generates vectors.
- Show/list retrieve records without a model. Adjustment creates a new version;
  history remains available through `memory history`.

## 6. Agent Sets And Reads Facts

Set durable assertions by subject and predicate. The default subject is `self`:

```sh
witself fact set --json identity/name "Archivist"
witself fact set --type string --json preferences/home-region us-east-1
witself fact set --sensitive --json contact/email archivist@example.com
```

Read an exact assertion or list redacted inventory:

```sh
witself fact get --json contact/email
witself fact get preferences/home-region
witself fact list --json
```

An intentional exact read can return a sensitive value; broad lists redact it
unless `--include-sensitive` is explicit. Other subjects use the shipped
`fact subject` family and the `--subject` flag. See
[fact-service.md](fact-service.md).

**Roadmap:** the older name/format examples and `fact set --primary` workflow do
not describe the shipped fact CLI. Its current flags are `--subject`, `--type`,
and `--cardinality`; it has no primary-promotion flag.

## 7. Cross-Agent Policy Workflows — Roadmap

**Roadmap:** policy creation, policy testing, and policy-authorized cross-agent
recall are target contracts. The CLI dispatches no `policy` family, and
`memory recall` has no owner-agent targeting flag. These are not runnable
customer workflows. See [access-policy.md](access-policy.md).

## 8. Security Groups And Group-Owned Records — Roadmap

**Roadmap:** group creation, membership management, group policy subjects, and
group-owned writes are not exposed by the shipped CLI. There is no `group`
dispatch family or `--group` ownership flag on current memory capture, fact set,
or secret create. See [security-groups.md](security-groups.md).

## 9. Agents Exchange Messages

Send a message to another agent. The sender is always derived from the token,
never from input:

```sh
export WITSELF_AGENT=coordinator

witself message send \
  --to archivist \
  --subject "handoff" \
  --body "Please record the migration outcome in your episodic memory." \
  --json
```

Omitted `--kind` normalizes to actionable `request` across CLI, MCP, and the
backend.

Send to a bounded explicit list or to every other live agent in the realm. Both
forms create one immutable send-time snapshot with per-recipient delivery and
ack state:

```sh
witself message send \
  --to-agents analyst-1,analyst-2 \
  --subject "sync" \
  --kind note \
  --body "Standup notes attached." \
  --payload-file ./notes.json \
  --json

witself message send \
  --to-realm \
  --subject "maintenance" \
  --kind note \
  --body "The maintenance window starts now." \
  --json
```

Open work that should be claimed by the best available agent uses the separate
client-ranked request state machine. The backend stores and fences the request;
candidate and coordinator clients perform all inference and ranking:

```sh
witself message request open \
  --body "Investigate the failed rollout." \
  --offer-window 30s \
  --max-assignees 1 \
  --idempotency-key failed-rollout \
  --json

# As one candidate.
witself message request offer mrq_123 \
  --body "I can inspect GKE and PostgreSQL." \
  --idempotency-key offer-mrq-123 \
  --json

# As the coordinator, after ranking the returned offers locally.
witself message request show mrq_123 --json
witself message request select mrq_123 \
  --selected-agent agent_123 \
  --idempotency-key select-mrq-123 \
  --json
```

Inspect unread metadata as the recipient agent:

```sh
export WITSELF_AGENT=archivist

witself message list --unread --json
```

The implemented stateless receive path waits for new metadata without
busy-polling or changing read/ack state:

```sh
witself message listen --timeout 20 --json
```

For ordinary actionable work, claim a fresh inbound message before reading it,
then atomically publish one derived result and acknowledge only after completion
succeeds:

```sh
witself message claim msg_124 --lease 2m --idempotency-key claim-msg-124 --json

# Use claim_id and generation returned above.
witself message read msg_124 --json
witself message complete msg_124 \
  --claim mcl_124 \
  --generation 1 \
  --kind result \
  --body "Migration outcome recorded." \
  --idempotency-key complete-msg-124-1 \
  --json
witself message ack msg_124 --json
```

Verify the original message's backend-derived `causal_depth` and that the
completion result is exactly parent depth plus one. Do not supply a depth field;
the server rejects caller-owned routing/causality. Processing `generation` is
only the durable claim fence. Migration-0036 `failure_count` is the separate
cross-machine deterministic failure accounting. Ordinary release does not
increment it. After a repeatable message-specific deterministic failure, a foreground
client may release the exact fence with `--deterministic-failure` (or MCP
`deterministic_failure=true`); never use that marker for provider-wide,
configuration, cancellation, timeout, or lease-maintenance failures.

Installed policy directs a runtime to handle the same lifecycle only while it
is active; model compliance is not forced. At a foreground task boundary the
policy directs it to inspect the bounded message checkpoint and make a zero-wait
metadata query:

```sh
witself self show --json
witself message listen --timeout 0 --json

# After selecting one canonical delivery:
witself message claim msg_124 --lease 5m --idempotency-key claim-msg-124 --json
witself message read msg_124 --json
# Handle the untrusted content, then complete/reply and acknowledge.
```

Codex and Claude Code may receive the value-free checkpoint automatically
through supported hooks. Cursor, Grok Build, OpenClaw, Antigravity, and Copilot
use installed guidance and MCP fallback to call `self.show`. The installed policy instructs every active
runtime to call `message.listen(wait_seconds=0)` to retrieve unread metadata;
model compliance is not forced by a hook or the backend. No hook exposes a
message body, marks a delivery read, acknowledges it, starts inference, or wakes
an idle runtime.

There is no background message service, provider-credential capture, or
host-local notification ledger. If the runtime closes, pending and terminal
messages remain canonical and unacknowledged in PostgreSQL until the next
foreground turn. Backend-derived `causal_depth` and `failure_count` remain
portable causal and failure-accounting inputs; neither imposes a workflow
threshold. Processing generation remains only the stale-writer fence.

The post-v0 cross-realm story (realm-qualified addressing, the signed realm card,
blind relay, and federation) builds on this same mailbox. See
[agent-collaboration.md](agent-collaboration.md).

Expected behavior:

- `from` is always derived from the authenticated token; sender forgery is
  structurally impossible through the API.
- Message bodies and payloads are untrusted input to the receiving agent,
  especially when a message would drive a memory or fact write.
- A message grants no authority to write another agent's records. The shipped
  realm-local mailbox does not implement the target policy workflows in section 7.
- Current send, deliver, read/ack, and processing claim/renew/release/complete
  transitions are audited without content. An active foreground client owns
  the startup listen, open-request offer/ranking/execution, and inference; the
  backend remains model-free. Shared plan-backed send/delivery rate and meter
  enforcement is implemented; group/cross-realm routing,
  responsibility-aware eligibility, and granular policy-scope enforcement
  remain later slices.

## 10. Export A Whole Account

Export all portable state for the selected managed account to a verified logical
archive:

```sh
witself export \
  --account acme \
  --out ./acme-account.tar.gz
```

Without `--out`, the CLI chooses a dated
`witself-export-<account>-<UTC-YYYYMMDD>.tar.gz` filename. It refuses to replace
an existing destination unless `--force` is explicit:

```sh
witself export \
  --account acme \
  --out ./acme-account.tar.gz \
  --force
```

Expected behavior:

- The command uses the account-scoped operator credential and `GET /v1/export`;
  it is account-wide rather than agent- or realm-scoped.
- The gzip/tar archive carries a `self` manifest, ordered JSONL table chunks,
  and trailing checksums. The CLI verifies the manifest and every chunk before
  installing the requested file.
- Open-plane account data is portable in the archive. Secret/TOTP values remain
  client-encrypted; AVKs, recovery artifacts, and raw tokens are excluded.
- A partial or invalid download is retained with an `.unverified` suffix for
  inspection rather than installed as the requested archive.
- There is no customer `witself import` command. Account archive import is a
  separate provision-token-authorized server operation used by operators for
  paired evacuation archives during account moves; it does not accept this
  `purpose=self` customer artifact.

## 11. Agent Creates And Reveals A Sealed Secret

Select the token-bound agent and inspect its client-held key state:

```sh
export WITSELF_AGENT=archivist
witself vault key status --json
```

For an agent with no existing vault binding, initialize the first local key:

```sh
witself vault key init --json
```

If the backend already has a binding and this installation lacks the matching
key, enroll the installation with the existing key or recover it using the
[vault custody workflow](client-custodied-agent-vault.md). A token alone cannot
decrypt secrets, and initializing a replacement key is not a recovery path.

Create a structured login secret from strict JSON. Public fields are explicitly
non-sensitive; the password is generated locally, encrypted, and omitted from
creation output:

```sh
witself secret create \
  --stdin \
  --idempotency-key github-builder-create-001 \
  --json <<'JSON'
{
  "name": "github/builder",
  "description": "GitHub login created by archivist",
  "template": "login",
  "tags": ["github"],
  "fields": [
    {"name": "username", "kind": "username", "sensitive": false, "value": "archivist@example.com"},
    {"name": "url", "kind": "url", "sensitive": false, "value": "https://github.com/login"},
    {"name": "password", "kind": "password", "sensitive": true, "generate_password": true,
     "password_policy": {"length": 40, "exclude_ambiguous": true}}
  ]
}
JSON
```

Show redacted field inventory, then explicitly reveal one field when needed:

```sh
witself secret show --json github/builder
witself secret reveal github/builder password
```

The current create interface takes `--file FILE` or `--stdin` containing the
whole JSON document. Individual field flags, secret update, inventory scan, and
cross-agent grants are not shipped CLI operations. Sensitive field access is
audited without recording plaintext; encryption and decryption happen in the
client. See [secret-model.md](secret-model.md).

## 12. Agent Enrolls TOTP And Generates A Code

TOTP enrollment is a `kind: "totp"` field in a secret-create JSON document,
containing `otpauth_uri` from the service's actual setup flow. For example, have
an authorized client prepare that document and pipe it into
`witself secret create --stdin --idempotency-key KEY`. This field is sensitive
and is encrypted by the client before storage. See [totp-2fa.md](totp-2fa.md).

After creating a secret named `github/authenticator` with a TOTP field named
`two_factor`, inspect seed-free metadata or generate the current code:

```sh
witself totp show --json github/authenticator two_factor
witself totp code --json github/authenticator two_factor
```

Both commands require the matching local vault key to read the encrypted
payload; neither prints the seed. Code output includes its remaining lifetime.
The current CLI has no TOTP enroll, QR-image input, or reveal-seed command.

## 13. Generate A Password Without Storing It

Generate a password locally without creating a sealed-plane record:

```sh
witself password generate --length 40 --exclude-ambiguous --json
```

The result is intentionally printed on stdout. To generate and encrypt a
password without printing it, use `generate_password` in the secret-create JSON
from section 11. Word-based passphrase generation is not a current CLI option.

## 14. Inject Secret References Into A Subprocess — Roadmap

**Roadmap:** the proposed `witself run` command and secret-reference environment
injection are not dispatched by this CLI. The shipped value-returning interface
is explicit `secret reveal` or `totp code` in the active client; there is no
implemented subprocess-injection or output-masking walkthrough here.

## 15. Operator Secret Scans And Cross-Agent Grants — Roadmap

**Roadmap:** realm-wide secret scans, grants/revocation, group-owned secrets, and
cross-agent reveal are not implemented CLI operations. Current secret inventory
and value access are agent-owned and token-bound. See
[authorization-and-roles.md](authorization-and-roles.md) for the broader target
contract and [client-custodied-agent-vault.md](client-custodied-agent-vault.md)
for shipped custody.

## 16. MCP Stdio For An Agent Runtime

Install the current MCP integration for a local runtime. On macOS and Linux,
the first four runtimes below also receive transcript hooks; on native Windows,
only Codex does. The phase-one OpenClaw, Antigravity, and GitHub Copilot
adapters use managed instructions plus guided MCP fallback:

```sh
witself install codex
witself install claude
witself install grok
witself install cursor
witself install openclaw
witself install antigravity
witself install copilot
witself integrations --verify
witself install all --agent archivist --location home --dry-run
witself install all --agent archivist --location home
witself install all --agent archivist --location home --json
```

The installer reuses an existing binding or the only local agent credential.
Pass `--agent archivist` when multiple agents exist, and add `--location home` only
when a human location label is useful. The resolved account, realm, and agent
are pinned explicitly in the hook and MCP commands. A supplied location is
pinned in both; an omitted location is left out of both commands.

On macOS and Linux, administrator-managed hooks are the Codex and Claude Code
default and do not move the user's identity, token lookup, or MCP registration
into the administrator account. Native Windows Codex uses user-scoped hooks;
native Windows Claude Code and Grok Build install MCP/routing without
transcript hooks, and native Windows Cursor is WSL-only. Grok Build and Cursor
use their global user hook locations on macOS and Linux. Run the command
normally; Witself performs narrow privilege
elevation only where supported and needed. On macOS or Linux, use
`--user-hooks` for Codex or Claude where the system policy layer is unavailable.

Grok's default Claude/Cursor compatibility can discover those runtimes' Witself
hooks and MCP servers. The Grok installer inspects the effective configuration,
fences imported hooks from writing through a non-Grok binding, rejects unsafe
foreign MCP aliases, and verifies the exact native Grok MCP command after
registration. It does not change Grok compatibility settings. If an operator
chooses to disable all imported hooks or MCPs from a vendor, set
`hooks = false` and/or `mcps = false` under `[compat.claude]` or
`[compat.cursor]` in `$GROK_HOME/config.toml` before rerunning the install.

```sh
witself uninstall codex
witself uninstall claude
witself uninstall grok
witself uninstall cursor
witself uninstall openclaw
witself uninstall antigravity
witself uninstall copilot
witself uninstall all --dry-run
witself uninstall all
witself uninstall all --json
```

The command validates the token-bound agent, stores its binding under
`~/.witself`, registers the `witself` stdio server with the runtime, and merges
transcript hooks where the runtime has a validated platform contract. For
Codex, Claude Code, Grok Build, and Cursor, the record pins the exact provider
CLI, configuration root and MCP path, Witself command/arguments, and
`WITSELF_HOME`; foreign entries and later selector or binding drift fail closed.
Every provider install/uninstall holds a provider-root whole-operation lock. It
never embeds the token in the MCP registration. The installed server command is
equivalent to `witself mcp serve --runtime` with `codex`, `claude-code`,
`grok-build`, `cursor`, `openclaw`, `antigravity`, or `copilot`.

Each runtime also receives managed fact-versus-portable-narrative-memory
routing guidance. Atomic assertions go to Witself facts. Narrative remember
requests go to Witself narrative memory by default; a provider-native memory is
an independent destination used only when the user explicitly selects native
memory or asks for both.
Cursor's rule is `~/.cursor/rules/witself-memory-routing.mdc`, with
`alwaysApply: true` frontmatter. The managed rule is discovered from the
workspace's ancestor chain. Current `cursor-agent` builds ignore
`CURSOR_CONFIG_DIR` for MCP discovery, so Witself rejects that selector rather
than recording an ineffective provider namespace. Cursor MCP
keeps the standard dotted tool names. Installation idempotently merges the
required `Mcp(witself:*)` allowlist permission into
`~/.cursor/cli-config.json`; uninstall removes it only when the
Witself integration record says the installer created it. Cursor Memories remain
project-scoped; when a user explicitly includes native Cursor memory in broad
recall, its coverage is reported as partial.

GitHub Copilot phase one requires CLI 1.0.73 or newer and detects the runtime
through its semantic version plus current MCP capability probe. It uses one collision-resistant
`witself-managed-<24-hex>` server in `$COPILOT_HOME/mcp-config.json` and the
exact-owned global instruction file
`$COPILOT_HOME/instructions/witself-memory-routing.instructions.md`. The
canonical runtime is `copilot`; `github-copilot` is an alias. Sibling servers
and other instruction files are preserved, and the instruction frontmatter
applies globally with `applyTo: "**"`. MCP binding drift and an unmarked or
extra-content dedicated instruction file fail closed.
`witself install copilot --routing-only` refreshes only the
instruction file. Phase one installs no Copilot transcript hook.

Expected behavior:

- MCP stdio is the v0 transport.
- MCP uses the token-bound identity and the same authorization as the CLI.
- Reinstall replaces only Witself's marker-delimited routing policy without
  duplicating it; uninstall removes that policy and preserves unrelated runtime
  configuration.
- The MCP server exposes self/transcript reads as well as implemented fact,
  memory, messaging, email, avatar, and sealed-plane tools. `--read-only` and
  `--no-value-tools` restrict the served surface.
- Hooks, rather than model-invoked MCP writes, append visible prompts, finalized
  responses, and optionally runtime-exposed tool activity.
- Failed delivery remains in the owner-only local outbox and can be retried with
  `witself transcript flush --runtime codex|claude-code|grok-build|cursor`.
- Grok's passive Stop hook precedes its durable final response. The existing
  one-shot outbox flusher holds that Stop locally until the exact native turn is
  complete, while a foreground `witself transcript flush --runtime grok-build`
  provides a deterministic fence after the final Grok process exits. No
  persistent runner or provider wrapper is involved.
- [mcp-tools.md](mcp-tools.md) also contains target contracts; use the server
  tool list to inspect the surface offered by the installed binary.

## 17. Self-Hosted Bootstrap

Generate the bootstrap token before Helm install and mount it through
`bootstrap.existingSecret`:

```sh
witself gen-bootstrap-token --out ./bootstrap.token
kubectl create namespace witself --dry-run=client -o yaml | kubectl apply -f -
kubectl -n witself create secret generic witself-bootstrap \
  --from-file=token=./bootstrap.token \
  --from-literal=ttl=15m
# Set bootstrap.existingSecret.name: witself-bootstrap in witself-values.yaml.

helm install witself oci://ghcr.io/witwave-ai/charts/witself-server \
  --version 0.0.272 \
  --namespace witself \
  --create-namespace \
  --values ./witself-values.yaml
```

Verify the Kubernetes rollout, health probes, and metrics endpoint:

```sh
kubectl -n witself rollout status deploy/witself-witself-server
```

Keep the port-forward running in a second terminal:

```sh
kubectl -n witself port-forward \
  deploy/witself-witself-server 8080:8080 8081:8081 9090:9090
```

Then verify the probes and metrics from the first terminal:

```sh
curl -fsS http://127.0.0.1:8081/livez
curl -fsS http://127.0.0.1:8081/readyz
curl -fsS http://127.0.0.1:8081/startupz
curl -fsS http://127.0.0.1:9090/metrics | head
```

Exchange the adopted one-time bootstrap token for an operator token:

```sh
witself auth login \
  --endpoint https://witself.internal.example.com \
  --bootstrap-token-file ./bootstrap.token \
  --out ./operator.token
```

Create the realm and agent, then issue its token:

```sh
witself realm create \
  --endpoint https://witself.internal.example.com \
  --token-file ./operator.token \
  prod

witself agent create \
  --endpoint https://witself.internal.example.com \
  --token-file ./operator.token \
  --realm REALM_ID \
  archivist

witself token create \
  --endpoint https://witself.internal.example.com \
  --token-file ./operator.token \
  --agent AGENT_ID \
  --out ./witself-tokens/archivist.token
```

Expected behavior:

- Self-hosted administration is explicit through `--endpoint` and
  `--token-file`.
- There is no default admin username/password.
- The bootstrap token is short-lived, single-use, and not an ordinary operator
  token.
- Database migrations run automatically under the shared migration lock before
  each database-backed process becomes Ready; there is no migration command or
  chart migration Job.
- The chart owns Kubernetes probes and metrics wiring through values.
- The self-hosted backend needs PostgreSQL and serves deterministic lexical
  recall without any model credential or model egress. Optional implemented
  client-supplied vectors use portable JSONB; the customer CLI has no standalone
  capabilities command.
- Sealed values remain client-encrypted under the client-custodied AVK; the
  current chart has no backend KMS or sealed-plane-switch settings. See
  [self-hosting.md](self-hosting.md) and
  [client-custodied-agent-vault.md](client-custodied-agent-vault.md).

## 18. Local Development Mode — Roadmap

**Roadmap:** the earlier local setup/store-file workflow is not implemented by
this CLI. There is no `setup --local` command or offline JSON-store CLI backend.
For development against the shipped server, follow the PostgreSQL-backed path
in [self-hosting.md](self-hosting.md) and use explicit endpoint/token flags.
Client-held vault custody also applies in development; a server-side local KMS
is not the agent-secret implementation.

## Remaining Roadmap Workflows

The explicitly fenced sections above cover policy testing and grants, security
groups, crypto billing, subprocess secret injection, and offline local setup.
Realm import is also roadmap customer surface: the shipped export is
account-wide, and server account-move import accepts paired evacuation archives,
not customer self-export artifacts. These targets should not be used as scripts
until their commands and handlers ship.

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
- [agent-collaboration.md](agent-collaboration.md)
- [backup-and-recovery.md](backup-and-recovery.md)
- [billing-and-limits.md](billing-and-limits.md)
- [operator-auth.md](operator-auth.md)
- [mcp-tools.md](mcp-tools.md)
- [self-hosting.md](self-hosting.md)
- [observability-and-operations.md](observability-and-operations.md)
- [data-model.md](data-model.md)
- [json-contracts.md](json-contracts.md)
