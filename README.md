# Witself

Status: active development. The `witself` CLI ships incrementally (with `ws`
kept as a short alias).

## Install

```sh
# Homebrew (installs the `witself` binary plus a `ws` alias)
brew install witwave-ai/tap/witself

# Optional infrastructure provisioner
brew install witwave-ai/tap/witself-infra

# Fleet-admin CLI (talks to the control plane; not needed for tenants)
brew install witwave-ai/tap/witself-admin
# (the fullscreen operator dashboard ships inside it: `witself-admin dashboard`)

# or curl | sh (verifies the SHA-256 checksum; installs witself by default)
curl -fsSL https://raw.githubusercontent.com/witwave-ai/witself/main/install.sh | sh

# Optional infrastructure provisioner
curl -fsSL https://raw.githubusercontent.com/witwave-ai/witself/main/install.sh | sh -s witself-infra

# Fleet-admin CLI
curl -fsSL https://raw.githubusercontent.com/witwave-ai/witself/main/install.sh | sh -s witself-admin

witself version
```

## Agent Runtime Integration

Once an agent token exists under the normal `~/.witself` account layout, one
command installs the Witself stdio MCP server. Codex, Claude Code, Grok Build,
and Cursor also receive durable transcript hooks. The OpenClaw and Antigravity
phase-1 integrations instead install stdio MCP plus managed static routing;
they have no Witself transcript hooks or automatic prompt-context injection. All six runtimes
receive managed safety and routing guidance for the full configured MCP catalog,
including identity, facts, narrative memory, curation, messaging and email,
avatars, and secrets. Witself is the default narrative destination;
native-provider memory is an optional second destination used only when
explicitly requested. See the
[narrative-memory design](docs/narrative-memory-and-curation.md).

- Codex: `$CODEX_HOME/AGENTS.md` (normally `~/.codex/AGENTS.md`)
- Claude Code: `$CLAUDE_CONFIG_DIR/rules/witself-memory-routing.md` (normally
  `~/.claude/rules/witself-memory-routing.md`)
- Grok Build: `$GROK_HOME/AGENTS.md` (normally `~/.grok/AGENTS.md`)
- Cursor: `$CURSOR_CONFIG_DIR/rules/witself-memory-routing.mdc` (normally
  `~/.cursor/rules/witself-memory-routing.mdc`)
- OpenClaw preview: `AGENTS.md` in the sole default agent's configured
  workspace
- Antigravity preview: an exact-owned, automatically discovered rules plugin at
  `~/.gemini/config/plugins/witself-managed-<binding-id>/` plus one exact
  collision-resistant server entry in `~/.gemini/config/mcp_config.json`

Cursor installation also merges `Mcp(witself:*)` into
`$CURSOR_CONFIG_DIR/cli-config.json` so the approved server's tools can run in
normal allowlist mode. Witself records whether it added that permission:
reinstall is idempotent, uninstall removes only a Witself-owned entry, and a
pre-existing user-owned entry is preserved.

```sh
witself integrations
witself install codex
witself install claude
witself install grok
witself install cursor
witself install openclaw
witself install antigravity

# Preview, then install every supported runtime detected on this machine.
witself install all --agent scott --location home --dry-run
witself install all --agent scott --location home
```

`witself integrations` distinguishes runtimes detected on the current machine
from runtimes that already have a Witself integration. Use the literal selector
`all` for bulk operations; do not use `*`, which a shell can expand before
Witself sees it. Bulk install applies common identity, location, and connection
flags to every detected target, processes targets sequentially, and prints a
per-runtime summary. It continues after an individual failure, preserves earlier
successful installs, and exits nonzero when any target fails. An explicit
`--agent` also applies to already installed targets, so preview first when those
integrations may be bound to different agents.

Bulk refreshes preserve each installed runtime's existing hook ownership. New
Codex and Claude Code integrations use their normal administrator-managed hook
default, which may request access; add `--user-hooks` to keep every hook-capable
runtime user-scoped instead.

OpenClaw phase 1 requires an installed `openclaw` CLI on `PATH` (or selected
with `OPENCLAW_CLI_PATH`) and exactly one configured agent. That sole agent must
be the default and have a clean absolute workspace path. Multi-agent OpenClaw
selection and native-plugin hooks are not part of this preview. The resulting
workspace `AGENTS.md` must fit within Witself's conservative 20,000-byte
OpenClaw bootstrap guard; installation fails without changing the file rather
than risk OpenClaw silently truncating the policy.

The OpenClaw MCP registration pins a 60-second connection timeout and only the
non-secret environment needed to preserve the selected local namespace:
the effective absolute `WITSELF_HOME`, plus non-empty
`OPENCLAW_CONFIG_PATH`, `OPENCLAW_STATE_DIR`, and `OPENCLAW_PROFILE` values.
When only `OPENCLAW_PROFILE` is set, Witself derives and persists OpenClaw's
normal `~/.openclaw-PROFILE/openclaw.json` namespace before invoking the CLI.
Witself never copies arbitrary environment variables, credentials, `HOME`, or
`PATH` into the registration. Reinstall refuses selector drift, and phase 1
fails closed on other OpenClaw home, workspace, agent-directory, or include-root
overrides instead of guessing how to reproduce them in the spawned MCP server.

Antigravity phase 1 requires the `agy` CLI on `PATH`, at
`~/.local/bin/agy`, or selected with `ANTIGRAVITY_CLI_PATH`, and currently
supports macOS and Linux because installation depends on native file locking
and no-replace/exchange renames. Witself validates
an immutable source bundle, then atomically installs exactly `plugin.json` and
`rules/witself.md` beneath Antigravity's standard global plugin root. It also
merges one exact, collision-resistant server entry into the canonical
`~/.gemini/config/mcp_config.json`; unrelated root fields and sibling servers
are preserved. That entry pins the absolute Witself binary, agent identity,
optional location, and only the non-secret absolute `WITSELF_HOME`; it never
persists credentials, `HOME`, or `PATH`. Witself does not edit `plugins.json` or
the import manifest. Reinstall and uninstall refuse foreign, disabled,
symlinked, malformed, locally edited, or extra-file owned state instead of
overwriting it. A per-home lock, durable transaction journal, atomic directory
exchange, and atomic 0600 shared-config writes make interrupted or concurrent
Witself mutations recoverable on the next install or uninstall.
`--routing-only` is unavailable because the exact-owned MCP entry and always-on
safety rule are maintained as one transaction across two provider surfaces.

The installer reuses an existing integration or the only local agent credential.
When more than one agent is available, select one explicitly; a location label
is optional:

```sh
witself install all --agent scott --location home --dry-run
witself install all --agent scott --location home
```

The resolved account, realm, and agent are pinned explicitly in every installed
MCP command and, where supported, every hook command. A supplied location is
pinned in both places for hook-capable runtimes and in the OpenClaw or
Antigravity MCP command;
when omitted, no `--location` argument is written. The installer verifies that
token-bound identity, preserves unrelated runtime configuration, and never
copies a token into the MCP or hook command. Local
integration identity and the retryable transcript outbox live under
`~/.witself/` (`WITSELF_HOME` overrides it).

Managed guidance contains policy only, never personal facts. Reinstall updates
the marker-delimited policy without duplicating it. Codex and Grok preserve
unrelated content in their shared `AGENTS.md` files; Claude and Cursor use
dedicated rule files that uninstall removes when empty. OpenClaw preserves
unrelated content in the default workspace's shared `AGENTS.md`; if that
workspace changes, uninstall the existing integration before reinstalling.
Cursor's MDC rule has
`alwaysApply: true` frontmatter and is discovered as an ancestor rule for
workspaces below the normal user home. `CURSOR_CONFIG_DIR` relocates the files
Witself manages, but the selected Cursor runtime must also discover that custom
`rules` directory; Witself does not copy the rule into each project. Codex
installation refuses to proceed when a non-empty global `AGENTS.override.md`
would shadow its managed file. After installing, restart the runtime and start a
new task so the file guidance and MCP initialization are refreshed. See
[Agent Memory Routing](docs/agent-memory-routing.md).

The OpenClaw preview registers only the exact `witself` stdio MCP binding that
Witself records, including its allowlisted environment and connection timeout.
Reinstall is idempotent when that binding already matches and refuses to claim
or replace an unrecorded or changed registration. Uninstall
likewise removes only the exact recorded binding and managed `AGENTS.md` block;
on a mismatch it fails closed and restores the routing block. This is a CLI/MCP
integration, not an OpenClaw-native plugin, so transcript capture and automatic
session or prompt injection remain unavailable. The managed workspace block is
therefore the safety contract for OpenClaw's full Witself MCP catalog, not just
its memory tools.

The Antigravity preview uses its native plugin format for an always-on rule and
the canonical shared MCP file for the server Antigravity actually loads.
Antigravity automatically discovers the owned plugin directory and exposes
dotted declared tool names under its collision-resistant per-binding namespace
`mcp_ws-<server-id>_`, such as
`mcp_ws-<server-id>_witself.memory.recall`. The 16-hex server id is a shortened
form of the plugin's 24-hex binding id, preventing an
ordinary workspace or global plugin named `witself` from shadowing this binding.
Atomic install, upgrade, and uninstall are fenced by a 0600 transaction journal;
Witself synchronizes the selected plugin, exact shared MCP entry, integration
config, immutable recovery bundle, and their parent directories before clearing
it. Unrelated shared-config fields and servers remain outside Witself's
ownership. Before every MCP server startup, Witself revalidates the installed
plugin, immutable recovery source, and managed shared entry against the recorded
binding; any drift prevents credential-bound tools from being exposed. A
recorded v0.0.198 three-file plugin remains valid for exact uninstall, while
reinstall migrates it transactionally to the rules-only plugin and canonical
shared entry. Antigravity's synchronous hooks and transcript-path payload remain
a phase-2 conformance task because they do not yet provide a validated direct
message or prompt-context contract.

Grok Build enables Claude and Cursor compatibility by default, including their
hooks and MCP servers. During `witself install grok`, Witself inspects Grok's
effective configuration before making changes. Imported Witself hooks are
reported and may proceed only when they invoke the same Witself executable;
that executable rejects every Grok-originated hook whose pinned runtime is not
`grok-build`. A foreign Witself MCP under another name is rejected because it
could still expose another agent binding. After registration, the installer
verifies that Grok's effective `witself` MCP is the exact native, enabled,
user-scoped command requested by the install. Witself never changes Grok's
broad compatibility settings. To stop Grok from launching all imported hooks
or MCPs for a vendor, explicitly set `hooks = false` and/or `mcps = false`
under `[compat.claude]` or `[compat.cursor]` in `$GROK_HOME/config.toml`; those
switches also disable unrelated compatibility entries from that vendor.

Facts also support guarded permanent deletion. Preview with
`witself fact delete --subject SUBJECT --dry-run PREDICATE`, then apply with
`--yes`. Apply binds both the previewed assertion and candidate-set revision;
on an ambiguous response the CLI prints an exact value-free replay command
that preserves the generated retry key. Deletion removes values, assertion/evidence history, and matching
candidates; it retains only a non-restorable value-free tombstone plus immutable
usage history so retries, audit, billing, and exports remain consistent. The
MCP tool `witself.fact.delete` uses the same preview/apply contract across
Codex, Claude Code, Grok Build, Cursor, OpenClaw, and Antigravity. Plain “forget” remains
ambiguous with each runtime's native memory and is clarified before any
destructive call.

Administrator-managed hooks are the Codex and Claude Code default. Run the
command as your normal user; Witself requests administrator access only for the
system hook policy, while identity, tokens, and MCP registration stay in the
user's configuration. Grok Build and Cursor use approval-free global user
hooks. On a workstation where Codex or Claude system policy cannot be
installed, pass `--user-hooks`; Codex then asks you to review the command hook
through `/hooks` once.

Remove an integration without deleting tokens or queued transcript events:

```sh
witself uninstall all --dry-run
witself uninstall all
```

Bulk uninstall selects runtimes by installed Witself state, not by whether their
provider executable is still detected. It uses the same sequential,
continue-and-summarize behavior as bulk install, so one failure does not undo
successful removals from other runtimes.

`messages` captures visible conversation and lifecycle events, `trace` adds
exposed tool, failure, permission, and subagent activity, and `raw` also retains
the runtime-exposed hook envelope. Entries include a normalized structured JSON
payload alongside readable text. None of the modes can capture hidden
chain-of-thought. See
[Witself Transcript Ledger](docs/transcript-ledger.md) for the data and retry
contract.

## Narrative Memory

Portable narrative memory is implemented with PostgreSQL as the canonical
store and no backend AI. The calling client authors every capsule, refinement,
and optional vector; the service provides durable versions, evidence,
lifecycle, deterministic lexical recall, fenced curation plans, guarded
rollback, and portable account archives.

```sh
witself memory capture --content "We chose PostgreSQL for durable memory." \
  --kind decision --capture-reason explicit \
  --evidence-unavailable-reason runtime_did_not_record \
  --idempotency-key decision-postgres-1

witself memory recall "PostgreSQL decision" --kind decision
witself memory list --state active
witself memory history mem_...
witself memory curate status

# Optional client-authored hybrid recall; Witself never runs the vector model.
witself memory vector profile create --provider local --model example \
  --recipe whole-memory --recipe-version 1 --dimensions 3
witself memory vector set mem_... --profile mvp_... --memory-version 1 \
  --content-hash SHA256 --vector-file memory-vector.json
witself memory recall "PostgreSQL decision" --vector-profile mvp_... \
  --query-vector-file query-vector.json

# Explicit legacy/manual client curator (preview is the default).
witself memory curate auto enable --runtime claude-code \
  --provider claude-code --allow-transcript-content
witself memory curate auto status --runtime claude-code
witself memory curate auto wake --runtime claude-code
witself memory curate auto service install --runtime claude-code
witself memory curate auto service status --runtime claude-code
```

Atomic one-to-many refinement uses `witself memory supersede`; reversible
`forget`, `restore`, and `reactivate` operations retain immutable history.
Permanent deletion is a separate value-free preview/apply flow. The same
surface is available over HTTP, the Go client, CLI, and MCP memory tools. Run
`witself mcp serve ... --read-only` to remove every mutating MCP tool. Dedicated
`--profile curator-preview|curator-apply` servers expose only the bounded
curation workflow and require an exactly matching restricted credential.

Client-run curation requests, leased/fenced snapshots, strict plans, apply, and
rollback are implemented. Source commits durably mark work due, and an active
compatible foreground client can claim it opportunistically. Installed Codex
and Claude hooks inject an authenticated, value-free `memory_checkpoint` when
work is already pending at the hook's `/v1/self` read, including on an otherwise
ordinary prompt. The active model is instructed to process at most one fenced
request near the end of that foreground turn and to apply an empty actions plan
when nothing merits memory so the reviewed cursors still advance. Cursor and
Grok hooks cannot reliably deliver model-visible control, so their always-on
managed rules use `self.show` as an explicitly guided fallback. Hook injection
is automatic delivery, not a guarantee that a model follows the rule.

The checkpoint is a point-in-time view: the current prompt may still be flushing,
and the current assistant response cannot yet be part of it, so current-turn
evidence can be curated on a later interaction. Runtime transcript hooks only
enqueue and flush evidence; they never launch inference or a curator. The
explicit `witself memory curate run` remains a planner/native-provider driver.
The older `memory curate auto` and per-user `auto service` surfaces remain explicit
legacy/manual compatibility tooling for deployments that deliberately configure
a separate client process; they are not invoked by runtime hooks. MCP and the
backend cannot wake or schedule an AI. See the
[canonical design and delivery plan](docs/narrative-memory-and-curation.md).

## Agent Messaging

Agents in the same account realm can exchange durable direct, explicit-list,
and realm-wide messages using their existing agent tokens:

```sh
witself message send --account default --agent scott \
  --to coordinator --body "Please pick up the indexing task."

witself message send --account default --agent scott \
  --to-agents bob,alice --kind note --body "The migration is complete."

witself message send --account default --agent scott \
  --to-realm --kind note --body "The maintenance window starts now."

witself message list --account default --agent coordinator --unread
witself message listen --account default --agent coordinator --timeout 20
witself message read msg_... --account default --agent coordinator
witself message reply msg_... --account default --agent coordinator \
  --body "I have one question before I start."
witself message ack msg_... --account default --agent coordinator
```

For work that should be offered to the realm instead of assigned immediately,
open a client-ranked request. Eligible agents offer or decline, and the
coordinator client selects one or more offers when all candidates have
responded or the bounded offer window ends:

```sh
witself message request open --account default --agent scott \
  --body "Investigate the failing GCP rollout." \
  --offer-window 30s --max-assignees 1 --idempotency-key rollout-investigation

witself message request list --account default --agent bob --role candidate
witself message request offer mrq_... --account default --agent bob \
  --body "I can inspect the GKE and database state." --idempotency-key bob-offer
witself message request select mrq_... --account default --agent scott \
  --selected-agent agent_... --idempotency-key select-bob
```

The sender and realm always come from the authenticated agent token. An
ordinary send defaults to actionable `kind=request`; use explicit `--kind note`
for an FYI that the recipient may read and acknowledge without treating it as
work. Fanout uses one immutable send-time recipient snapshot with separate
delivery/read/ack state per recipient; the authenticated sender is excluded
from `--to-realm`.

Inbox lists and `listen` contain metadata only. `listen` returns the oldest
unacknowledged inbound work without changing state, `read` is the content
boundary, and message content must be treated as untrusted input. Replies are
recipient-only: the server validates that the caller received the parent and
derives the reply recipient, thread, and parent link. Read, acknowledgement,
claim, and completion remain separate in the CLI, API, and MCP.

**Messaging status:** the agreed same-realm foreground feature is operationally
complete in release `v0.0.172` (`67ec81d3f5485f1865f87e265ae9f33fa15c6988`).
The GCP sandbox rollout is recorded by `f984008`; `2e99290` synchronizes the
other, dormant cell definitions to the same desired version without claiming
that those cells are deployed. Codex, Claude Code, Grok Build, and Cursor
integrations were refreshed. Live direct-message handling passed for Claude,
Cursor, and Grok, and a reverse Claude-to-Codex flow verified Codex receive and
completion; all four provider inboxes were clean at closeout. See the sanitized
[activation evidence](docs/autonomous-realm-messaging.md#completion-boundary).
This messaging closeout does not certify the broader narrative-memory system;
its production gates remain tracked by
[#44](https://github.com/witwave-ai/witself/issues/44),
[#45](https://github.com/witwave-ai/witself/issues/45), and
[#46](https://github.com/witwave-ai/witself/issues/46).

There is no background Witself messaging process. At the beginning of a
non-trivial foreground turn, the installed policy instructs a client to inspect
the bounded `self.show.message_checkpoint` and call
`message.listen(wait_seconds=0)`. Codex and Claude Code can receive the
content-free checkpoint automatically through supported hooks; Cursor and Grok
Build use the installed guidance and MCP fallback. The policy cannot force model
compliance. In every runtime, listen is the operation that retrieves unread
message metadata. An active client may then claim, read, handle, complete or
reply, and acknowledge the canonical message. If the client is closed, the
message remains durable and unacknowledged until its next foreground turn. MCP,
hooks, webhooks, and the backend never start or wake an AI.

The installed MCP server exposes matching `witself.message.send`, `reply`,
`list`, `listen`, `read`, `ack`, `claim`, `renew`, `release`, and
`complete` tools plus the complete `witself.message.request.*` lifecycle.
Migration 0035 supplies server-derived `causal_depth`; migration 0036 supplies
durable deterministic `failure_count`; processing generation remains only the
stale-writer fence. PostgreSQL is the sole message and handoff source, and all
canonical message/request state participates in account export/import.
See [Witself Inter-Agent Messaging](docs/inter-agent-messaging.md) and
[Autonomous Realm Messaging](docs/autonomous-realm-messaging.md) for the
implemented same-realm direct, fanout, and client-ranked open-request boundary.
Group audiences, cross-realm routing, and responsibility-aware eligibility
remain separate follow-on work.

## Local Agent Dashboard

One command serves a loopback-only, read-only web dashboard for a single
agent: six live surfaces (overview, transcripts, facts, memories,
conversations, and sealed-secret metadata) over the agent's own token, using
observational and passive reads so viewing never perturbs retrieval usage or
read-state. The listener binds `127.0.0.1` only and requires the per-process
tokened URL printed at startup; `status` and `stop` manage running dashboards
from the same local registry. See
[ADR 0004](docs/decisions/0004-local-agent-dashboard.md).

```sh
witself dashboard serve --agent scout --open
witself dashboard status
witself dashboard stop --agent scout
```

## Infrastructure Example

`witself-infra` provisions one complete isolated cell per cloud account/region.
For the current AWS sandbox cell in `us-west-2`:

```sh
witself-infra up \
  -account-alias sandbox \
  -argocd \
  -aws-profile witwave-sandbox \
  -backend s3 \
  -bootstrap-token-file ~/.witself/tokens/bootstrap.token \
  -cidr 10.20.0.0/16 \
  -cloud aws \
  -control-plane https://self.witwave.ai \
  -db-version 18 \
  -domain cells.witself.witwave.ai \
  -fleet-token-file ~/.witself/tokens/fleet.token \
  -gitops-path .gitops/charts/bootstrap \
  -gitops-repo https://github.com/witwave-ai/witself \
  -gitops-revision main \
  -gitops-values-path .gitops/cells/aws-sandbox-usw2-dev/values.yaml \
  -k8s-version 1.36 \
  -profile minimal \
  -region us-west-2 \
  -role dev
```

For the current GCP sandbox cell in `us-east1`:

```sh
# Pulumi GCS/GCP KMS and GKE verification use Application Default Credentials.
gcloud auth application-default login --project witself-sandbox

witself-infra up \
  -account-alias sandbox \
  -argocd \
  -backend gcs \
  -bootstrap \
  -bootstrap-token-file ~/.witself/tokens/bootstrap.token \
  -cidr 10.20.0.0/16 \
  -cloud gcp \
  -control-plane https://self.witwave.ai \
  -db-version 18 \
  -domain cells.witself.witwave.ai \
  -fleet-token-file ~/.witself/tokens/fleet.token \
  -gcp-project witself-sandbox \
  -gitops-path .gitops/charts/bootstrap \
  -gitops-repo https://github.com/witwave-ai/witself \
  -gitops-revision main \
  -gitops-values-path .gitops/cells/gcp-sandbox-use1-dev/values.yaml \
  -profile minimal \
  -region us-east1 \
  -restore-archives \
  -restore-any-region \
  -role dev
```

For the Azure sandbox subscription in `eastus2`, Azure support currently covers
the shared Pulumi state backend plus the first workload substrate slices:
resource group, VNet, workload subnet, PostgreSQL-delegated DB subnet,
controlled outbound egress through Azure NAT Gateway, private Azure Database
for PostgreSQL Flexible Server, per-cell Azure Key Vault app secrets, AKS with
OIDC/workload identity enabled, native AKS cluster autoscaler on the system
node pool, Azure DNS delegation, and GitOps identities for platform add-ons such
as External Secrets Operator and ExternalDNS. Pulumi also enables the
AKS-managed Application Gateway for Containers ALB Controller add-on so the
GitOps app tier can render the Azure Gateway API path for the Witself API,
including cert-manager-managed Let's Encrypt TLS with Azure DNS-01 validation
for HTTPS. HTTP-to-HTTPS redirect policy remains a follow-up ingress polish
slice.

```sh
az login --tenant a18639f4-1eb4-4810-ab3b-5717aa935e27
az account set --subscription witwave-sandbox

witself-infra bootstrap \
  -azure-subscription witwave-sandbox \
  -backend azblob \
  -cloud azure \
  -region eastus2

witself-infra up \
  -account-alias sandbox \
  -argocd \
  -azure-subscription witwave-sandbox \
  -backend azblob \
  -cloud azure \
  -db-version 18 \
  -domain cells.witself.witwave.ai \
  -gitops-path .gitops/charts/bootstrap \
  -gitops-repo https://github.com/witwave-ai/witself \
  -gitops-revision main \
  -gitops-values-path .gitops/cells/azure-sandbox-use2-dev/values.yaml \
  -k8s-version 1.36 \
  -profile minimal \
  -region eastus2 \
  -role dev
```

The bootstrap token file contains the first operator token that `witself-infra`
publishes to the cell's cloud secret store. One shared token can bootstrap every
cell (it is single-use *per cell* — each cell consumes its own copy on first
claim). If `-bootstrap-token-file` is omitted, the CLI prefers a per-cell
`~/.witself/tokens/<cell>/bootstrap.token` and falls back to the shared
`~/.witself/tokens/bootstrap.token`.

When `CLOUDFLARE_API_TOKEN` is present, `witself-infra` also delegates the
per-cell DNS zone from the Cloudflare zone for the configured domain. Keep
that token available during teardown so the delegated DNS records can be removed.

The GCP substrate includes a regional Cloud Router and Public Cloud NAT with a
reserved outbound IP, so sandbox and production cells get predictable controlled
egress in the same one-shot `up` path.

The Azure substrate includes a NAT Gateway and static public IP on the workload
subnet, with default outbound access disabled, so AKS workloads have a
predictable controlled egress path. AKS uses Azure CNI overlay, a one-node
minimal system pool that can autoscale to 20 nodes (`prod` starts at two nodes
and also caps at 20), and OIDC/workload identity so GitOps platform components
can read cloud secrets without long-lived
credentials. Its
PostgreSQL Flexible Server is private only, uses the delegated DB subnet, and
resolves through the cell's private DNS zone link. Azure stores the cell DB JSON,
first-operator bootstrap token, and account-provision token as JSON secrets in
the cell Key Vault. Pulumi also creates the ESO managed identity, federates it
to the `external-secrets/external-secrets` Kubernetes service account, and grants
it read access to the cell Key Vault. Pulumi creates the Azure DNS zone,
delegates it from Cloudflare when `CLOUDFLARE_API_TOKEN` is present, and creates
the ExternalDNS managed identity plus federated credential for the
`external-dns/external-dns` service account. Pulumi also reserves a delegated
subnet for Azure Application Gateway for Containers, enables the AKS-managed ALB
Controller add-on, grants that add-on identity permission to join the delegated
subnet, and passes the subnet ID into GitOps so Argo renders the Gateway API
manifests plus cert-manager's Azure DNS-01 issuer/certificate resources for
HTTPS.
Add `-argocd` to the Azure `up` command to install the same Argo CD app-of-apps
layer used by AWS and GCP.

With `-control-plane`, `up` registers the cell with the Witself Cloud fleet
after provisioning (endpoint = the cell's `apiHost` output), authorized by the
fleet token. `-fleet-token-file` points at the token file; when omitted the
token is read from `WITSELF_FLEET_TOKEN`, then `~/.witself/tokens/fleet.token`.
Omit `-control-plane` entirely and no registration happens — the self-hosted
path is the same command without the flag.

With `-argocd` on GCP and Azure, `up` also waits for the Argo CD Applications to
report `Synced/Healthy` through the cluster API after Pulumi finishes. This
catches late DNS, ingress, certificate, and app-of-apps convergence before the
command exits.

The examples use `-profile minimal`: cheap, disposable sandbox shapes with small
managed Postgres instances and clean teardown behavior. GCP `prod` raises Cloud
SQL to regional availability, PITR backups, retained/final backups, and larger
disk headroom; similar Azure prod hardening is a later slice. Use prod for
persistent cells, not save-money teardown tests.

The teardown command keeps only the stack identity, backend, configured domain,
and credentials. It destroys the cell resources; the shared Pulumi state backend
remains for the next run.

```sh
witself-infra destroy \
  -account-alias sandbox \
  -aws-profile witwave-sandbox \
  -backend s3 \
  -cloud aws \
  -control-plane https://self.witwave.ai \
  -domain cells.witself.witwave.ai \
  -fleet-token-file ~/.witself/tokens/fleet.token \
  -region us-west-2 \
  -role dev
```

For the GCP sandbox, use the same teardown shape with the GCS backend and GCP
project:

```sh
witself-infra destroy \
  -account-alias sandbox \
  -argocd \
  -backend gcs \
  -cloud gcp \
  -control-plane https://self.witwave.ai \
  -domain cells.witself.witwave.ai \
  -fleet-token-file ~/.witself/tokens/fleet.token \
  -gcp-project witself-sandbox \
  -region us-east1 \
  -role dev
```

For the Azure sandbox, use the Azure Blob backend and subscription selector:

```sh
witself-infra destroy \
  -account-alias sandbox \
  -azure-subscription witwave-sandbox \
  -backend azblob \
  -cloud azure \
  -profile minimal \
  -region eastus2 \
  -role dev
```

With `-control-plane`, `destroy` first drains the cell (placement stops),
**evacuates every account into a per-account archive in Cloudflare R2**
(suspend → export → verify → retire routing, looping until none remain), and
only then removes the cell from the fleet and tears the infrastructure down.
The accounts wait in R2 — "archived, awaiting placement" — until they are
restored onto another cell. Add `-destroy-accounts` to skip preservation and
purge the directory entries instead: the sandbox/dev override, an explicit
acknowledgment that the accounts die with the cell.

Pass `-restore-archives` to `up` to close the loop. After the new cell
registers, the fleet placement runner assigns each archived account to its
best eligible accepting cell (import → resume → route → cleanup). Ranked
cloud, canonical-region, and release-channel preferences decide first; hard
`only-*` pins filter eligibility; current account count and then cell name
break ties.

For an explicit evacuation test where provider region names do not line up
(for example, restoring AWS `us-west-2` archives onto a temporary GCP
`us-east1` cell), add `-restore-any-region` with `-restore-archives`. That
operator override bypasses the provider-region guard on legacy archives.
Policy-aware archives still honor their explicit hard pins.

Account owners manage the policy stored with their account:

```sh
witself account placement show
witself account placement set \
  --prefer-clouds gcp,aws,azure \
  --prefer-regions usw2,use1 \
  --prefer-channels stable,edge,experimental \
  --rebalance-on cloud,channel
```

Fleet operators can inspect placement, preview or perform live movement, and
enable the five-minute scheduled restore/rebalance pass:

```sh
witself-infra placement-status -control-plane https://self.witwave.ai
witself-infra rebalance -control-plane https://self.witwave.ai -dry-run
witself-infra placement-runner -control-plane https://self.witwave.ai -enable -run
```

If an archived account has impossible hard pins, an operator can clear only
the blocked axes while preserving its ranked preferences. The rescued policy
is applied to the imported account before it resumes on its destination cell:

```sh
witself-admin placement rescue --account-id acc_... --axes region,channel
```

See [infra/pulumi/README.md](infra/pulumi/README.md) for the CLI internals and
[.gitops/README.md](.gitops/README.md) for how Argo CD reconciles the GitOps
tree after `-argocd` is enabled.

## Overview

Witself is the agent durable-state platform **and** the trust fabric agents
collaborate over. Every agent gets a durable, attributable self — memories,
facts, and sealed credentials — plus a verified, loop-safe channel to work with
other agents across machines, realms, and accounts. The identity and memory
store is what makes that channel trustworthy: collaboration rides on the same
attributable self, so a counterpart can be verified before it is trusted.

It is being designed to let AI agents and human operators manage their own state
across two planes: an **open plane** of memories and facts, and a **sealed
plane** of secrets and TOTP authenticators. Agents manage their own state,
access other agents' state under declarative policy and grants, organize into
security groups, and exchange durable messages — within a realm and, as the
flagship post-v0 epic, across realms and accounts — all through a safe,
auditable CLI, MCP, and API surface. Witself is deployed as a fleet of
independent multi-cloud cells, each authoritative for its own tenants.

The two planes share one platform spine — one core domain service behind thin
CLI/MCP/API adapters, the same account/realm/agent tenancy and token model, the
same deployment shape, and the same authorization, audit, observability, release,
and billing apparatus — but take deliberately opposite postures on the payload:

- The **open plane** (memories, facts) protects the *integrity and authenticity*
  of identity data: plaintext at rest, semantically indexed and recallable,
  cross-agent readable under policy, in the self-digest, and plaintext-exportable.
- The **sealed plane** (secrets, TOTP) protects the *confidentiality* of
  credential material: the client-held agent vault key wraps per-field DEKs,
  and each field value is encrypted before transport. The backend authorizes
  access to one encrypted package at a time but has no decrypt key. Sealed-plane
  values are never embedded, returned by semantic recall, placed in the
  self-digest, or plaintext-exported. They surface only through explicit,
  audited client-side use.

Witself consolidates the former Witpass credential vault and authenticator into
the sealed plane: one product, one `witself` CLI, one backend, one account and
agent model.

The product goal is a CLI-first agent durable-state service spanning both planes:

- Named agents inside an operator-managed realm.
- Memories: free-form self-content with versioned edit history and a soft
  forget/restore lifecycle.
- Portable narrative memory: immediate client-authored capture, versioned
  evidence and lineage, and low-touch client-side curation.
- Recall by default: PostgreSQL keyword, tag, kind, time, salience, and recency
  ranking, optionally blended with client-supplied vectors.
- Facts: deterministic name→value identity cards, with a `primary` flag for
  identity anchors.
- A sealed credential plane: structured secrets and TOTP authenticators encrypted
  by the active client under a separate, client-custodied agent vault key. The
  backend stores and exports ciphertext, wrapped field keys, and public metadata
  but cannot decrypt sensitive values. The agent-owned vertical provides
  explicit one-field access, short-lived multi-installation key enrollment,
  passphrase-encrypted offline recovery, and crash-resumable client-side AVK
  rotation. Secret update, runtime injection, and cross-agent grants remain
  follow-on slices. Sealed-plane values are never embedded, returned by semantic
  recall, placed in the self-digest, or plaintext-exported.
- Cross-agent access governed by evaluable, default-deny policy.
- Security groups that act as both policy subjects and policy targets and can own
  group-scoped shared memories and facts.
- Durable inter-agent messaging with delivery, ordering, and acknowledgement.
- Cross-realm agent collaboration: a verified, loop-safe channel for agents to
  work together across machines, realms, and accounts — signed realm discovery,
  a blind relay, and enforced loop and spend caps — built on the realm-local
  mailbox as the first post-v0 epic.
- First-class structured/plaintext identity export and round-trippable import.
- Identity references such as `witself://fact/email` and
  `witself://agent/archivist/memory/mem_...`.
- Agent self-managed memory and hydration: an always-loaded self-digest,
  provider-aware natural capture, and recall across available sources — plus
  being a good CLAUDE.md/AGENTS.md citizen via two-way `digest emit` / `ingest`.
- MCP compatibility for agent runtimes.
- Managed Witself Cloud by default.
- A multi-cloud cell platform: each cell is one complete, independent Witself
  stack in its own cloud account or region, with a thin global control plane for
  tenant placement and routing — a fleet of cells, each authoritative for its
  own tenants, so a cell outage stays contained.
- Public backend code and first-class self-hosting for operators who want to run
  Witself in their own cloud.

The implemented AVK lifecycle is available through `witself vault key enroll
begin|approve|complete|list|status|cancel`, `vault key recovery
export|inspect|import`, `vault key rotate
(--recovery-out FILE|--accept-unrecoverable-key-loss)`, and `vault key rotation
status|cancel`. Artifact-backed rotation durably writes and decrypt-verifies the
exact target recovery copy before commit; production output must be external or
synchronously replicated rather than only another file on the same disk.
Pairing secrets and recovery passphrases use the controlling TTY only; there is
no argv, environment, stdin/pipe, JSON, or MCP credential path.
Account archives contain ciphertext, wrapped DEKs, public AVK bindings, and
terminal lifecycle history, but never the AVK or recovery artifact. Active
enrollment/rotation work must be cancelled before account export, agent
deletion, or irreversible account close. The matching AVK moves separately
after a cell import. See
[Client-Custodied Agent Vault](docs/client-custodied-agent-vault.md).

Managed Witself Cloud is the default supported product. Self-hosting is available
from the public repo, while production self-host support is a paid or contracted
support path once hardening docs and operational guidance are real.

## Repository Status

This repository is pre-v0 and still docs-led, but implementation is now landing
incrementally. The `witself` CLI, `witself-server`, Helm chart, GitOps tree, release
workflows, and Pulumi-based `witself-infra` module are built in this repo.

## Docs

- [Runbooks](docs/runbooks.md) — hand-testing recipes for everything that is built and running
- [Requirements](docs/requirements.md)
- [Data Model](docs/data-model.md)
- [Context Hydration](docs/context-hydration.md)
- [V0 Scope](docs/v0-scope.md)
- [Memory Model](docs/memory-model.md)
- [Narrative Memory](docs/narrative-memory-and-curation.md)
- [Facts Model](docs/facts-model.md)
- [Secret Model](docs/secret-model.md)
- [Client-Custodied Agent Vault](docs/client-custodied-agent-vault.md)
- [TOTP 2FA](docs/totp-2fa.md)
- [Secret Size And Attachments](docs/secret-size-and-attachments.md)
- [Encryption Model](docs/encryption-model.md)
- [Key Hierarchy](docs/key-hierarchy.md)
- [Authorization And Roles](docs/authorization-and-roles.md)
- [Access Policy](docs/access-policy.md)
- [Security Groups](docs/security-groups.md)
- [Inter-Agent Messaging](docs/inter-agent-messaging.md)
- [Autonomous Realm Messaging](docs/autonomous-realm-messaging.md)
- [Agent Collaboration](docs/agent-collaboration.md)
- [Agent Avatars](docs/agent-avatars.md)
- [Operator Authentication](docs/operator-auth.md)
- [Token Lifecycle](docs/token-lifecycle.md)
- [Workflow Scripts](docs/workflow-scripts.md)
- [CLI Command Surface](docs/cli-command-surface.md)
- [Server Command Surface](docs/server-command-surface.md)
- [API Contract](docs/api-contract.md)
- [API Routes](docs/api-routes.md)
- [MCP Tools](docs/mcp-tools.md)
- [JSON Contracts](docs/json-contracts.md)
- [Backend Architecture](docs/backend-architecture.md)
- [Deployment Cells](docs/deployment-cells.md)
- [Storage](docs/storage.md)
- [Observability And Operations](docs/observability-and-operations.md)
- [Self-Hosting](docs/self-hosting.md)
- [Self-Hosted Support](docs/self-host-support.md)
- [Helm Chart](docs/helm-chart.md)
- [Terraform Infrastructure](docs/terraform-infrastructure.md)
- [Cloud Targets](docs/cloud-targets.md)
- [Memory Cloud Conformance](docs/memory-cloud-conformance.md)
- [Backup And Recovery](docs/backup-and-recovery.md)
- [Billing And Limits](docs/billing-and-limits.md)
- [Release And Build](docs/release-and-build.md)
- [Implementation Plan](docs/implementation-plan.md)
- [Scaffold Readiness](docs/scaffold-readiness.md)
- [Audit Retention](docs/audit-retention.md)
- [Post-v0 Roadmap](docs/post-v0-roadmap.md)
- [Threat Model](docs/threat-model.md)
- [Security Policy](docs/security-policy.md)
- [Governance And Support](docs/governance-and-support.md)
- [Competitive Analysis](docs/competitive-analysis.md)

## Planned Public Artifacts

- Source repo: `github.com/witwave-ai/witself`
- Homebrew tap: `github.com/witwave-ai/homebrew-tap`
- CLI/MCP image: `ghcr.io/witwave-ai/images/witself`
- Backend image: `ghcr.io/witwave-ai/images/witself-server`
- Helm chart: `ghcr.io/witwave-ai/charts/witself-server`

## Security

Do not file public issues for suspected vulnerabilities. See
[SECURITY.md](SECURITY.md) and [docs/security-policy.md](docs/security-policy.md).

## License

Witself is **source-available** under the [Functional Source License,
FSL-1.1-ALv2](LICENSE). You may use, modify, and self-host it for any purpose
**except** offering it as a commercial product or service that competes with
Witself. Two years after each release is published, that release converts to the
Apache License 2.0.

The license covers the software. It does not grant rights to use the Witself
name, marks, logos, or branding except as allowed by the license for describing
the origin of the work.
