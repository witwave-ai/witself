# Local cell implementation plan

[WitSelf running locally](README.md)

Status: **draft for joint implementation planning with Claude**. The local-cell
direction is selected and revisable; the milestones below are proposed work,
not completed features. This plan authorizes no installation, production change,
or change to existing accounts. The [requirements](local-first-mode.md) govern
product scope; the [assessment](local-mode-code-assessment.md) supplies the
architecture rationale. The repository [feature scorecard](../../feature-status.md)
remains the progress authority once implementation begins.

Build a native, privately accessible WitSelf cell with managed PostgreSQL first,
while designing synchronization in parallel. Prove installation and recovery
early on all three operating systems. Then add account enrollment, an authorized
read replica, and progressively writable synchronization. Preserve the existing
remote client throughout. Continuous sync is required for the connected offering;
a standalone canary is an intermediate result, not completion of this plan.

## Requirements to carry into every slice

- The free offering runs supported core operations locally. Selecting local
  installation deploys the cell in the same operation; users do not administer
  PostgreSQL. Avoid mandatory Docker or Kubernetes. PostgreSQL is a separate
  managed process, not an embedded database.
- macOS, Windows, and Linux belong to the first supported local release. A
  macOS canary is useful evidence but does not satisfy that release gate.
- Keep a lightweight remote-only installation for SaaS and self-managed
  services, without a local database, cell download, or service registration.
- Connected cells continuously synchronize the whole account by default. Core
  local reads, searches, transcript capture, and declared mutations continue
  during a service outage. The AI client still performs memory inference.
- Hosted email, cross-machine messaging, shared work claims, and control-plane
  actions retain remote authority. A synchronized record never authorizes
  replaying an external action or reusing a remote claim fence locally.
- Preserve data and pending changes on capacity refusal, disconnect, failed
  upgrade, or ordinary uninstall. Distinguish local durability, remote acceptance,
  convergence, and delivery to an external service.
- Require disclosed, bounded, nonblocking usage reporting and quiet contextual
  hosted-feature discovery. Reporting failure cannot interrupt local work.
- Ship inactive foundations and deliberately enrolled canaries. Changing the
  installation default, migrating existing free SaaS customers, and changing
  billing or account policy are separate product decisions.

Secrets are a desired capability whose first-release operation coverage needs
an explicit decision. The plan includes vault work and its exit criteria rather
than silently promising it or dropping it. Future decentralized cells, exposing
a local cell as a public bare-metal service, and alternative backup destinations
remain follow-on opportunities. Basic local restore is required now.

## Code boundaries to use

These entry points were checked at `6c55133f1e7c`. The original research records
its earlier `adc6f186` baseline. Recheck affected code before implementing a slice;
the paths below identify reuse seams, not finished local-mode implementations.

| Boundary | Existing entry points | Planned adaptation |
|---|---|---|
| Connection and installed identity | [CLI resolution](../../../cmd/witself/main.go), [local bindings](../../../internal/local/local.go), [MCP](../../../cmd/witself/mcp.go) | Explicit local, connected-local, and remote-only profiles; offline local resolution without changing existing directory-based routing |
| Application and database | [server startup](../../../cmd/witself-server/main.go), [store](../../../internal/store/store.go), [migrations](../../../internal/store/migrate.go) | Private runtime configuration, durable bootstrap identity, serialized lifecycle and authenticated database-backed health |
| Temporary archives | [startup spool sweep](../../../internal/server/spool_sweep.go), [self-export](../../../internal/server/export_self.go) | Private temporary directories owned by one active server lifecycle; never sweep host-global temporary files or another cell's archives |
| Background jobs | [worker startup](../../../cmd/witself-worker/main.go), [runtime](../../../internal/worker/runtime.go) | Explicit job eligibility for standalone data versus synchronized projections; prevent copied remote work from executing |
| Native delivery | [release targets](../../../.goreleaser.yaml), [Unix installer](../../../install.sh), [Windows installer](../../../install.ps1), [Homebrew generator](../../../tools/homebrew-formula/main.go) | Deliver compatible cell components separately from the lean client and invoke one shared lifecycle |
| Infrastructure management | [infra module](../../../infra/pulumi/README.md), [protected preview](../../../infra/pulumi/cmd/witself-infra/deletion_plan.go) | Reuse inventory, preview, identity, and recovery concepts without introducing Pulumi/cloud dependencies into local or remote-only installs |
| Synchronization | [transcripts](../../../internal/store/transcript.go), [memory versions](../../../internal/store/memory.go), [export](../../../internal/store/export.go), [import](../../../internal/store/import.go) | Stable operation/evidence identities, transactional change capture, consistent snapshot plus catch-up, resumable application |
| Remote account movement and recovery | [evacuation finalization](../../../internal/store/finalize_evacuation.go), [import leases](../../../internal/store/account_import_lease.go), [account purge](../../../internal/store/account_purge.go) | Carry or explicitly invalidate sync state during move, restore and purge; bind cursors and receipts to a proven authority incarnation |
| Usage and audit provenance | [usage rollups](../../../internal/store/usage.go), [activity context](../../../internal/activity/context.go), [fact reads](../../../internal/store/fact.go) | Separate logical actions, replica application, local utilization and derived counters before sync activation |
| Deletion and sealed data | [fact deletion](../../../internal/store/fact_delete.go), [memory deletion](../../../internal/store/memory_delete.go), [vault associated data](../../../internal/sealed/aad.go) | Non-resurrection rules, retained deletion knowledge, stable account/realm/agent identities, separately held client keys |

Current native server/worker release targets cover macOS and Linux; the CLI also
covers Windows AMD64. Windows cell packaging and execution need proof early.
The existing infra CLI's local backend stores Pulumi state locally; it is not
a desktop process driver. Existing installers protect binary replacement;
whole-cell data upgrades need an additional lifecycle contract.

## Proposed milestones and dependencies

Use small reviewable PRs within each milestone. Assign one integration owner and
an independent reviewer; separate platform adapters and test harnesses can run
in parallel once their interfaces agree. All rows start **not started**.

| ID | Deliverable | Depends on | Completion evidence |
|---|---|---|---|
| M0 | Operation, identity, lifecycle, and compatibility contracts | Existing research | Reviewed decisions and explicit unresolved gates; no assumed sync semantics |
| M1 | Inactive local runtime and lifecycle foundation | M0 runtime contract | Private authenticated cell, isolated state, repeatable lifecycle, unchanged hosted path |
| M2 | Standalone local operations and explicit routing | M1; M0 identity contract | Durable offline CLI/MCP use, retrieval comparison, restart and concurrency proof |
| M3 | Native installers, updates, and restore | M1; M2 acceptance fixtures | Clean-machine and interruption matrix on macOS, Windows, Linux; lean remote package |
| M4 | Synchronization journal and failure simulator | M0 capture, authority-continuity and accounting contracts | Atomic capture/apply, mixed-version writer coverage, duplicate-safe move/restore and replay accounting |
| M5 | Enrollment and authorized snapshot plus catch-up | M4 capture barrier and recovery proof; M1 isolation; M0 destination policy | Authorized admission, restartable snapshot, proven authority continuity and read-only convergence without side effects |
| M6 | Writable domain synchronization | M5; M2; deletion/conflict rules in M0 | Two local writers plus remote-only writer converge under the failure matrix |
| M7 | Connected authority, vault coverage, and account transitions | M5/M6 as appropriate; M0 destination and accounting contracts | No copied delivery authority; tested revocation, capacity, key and lifecycle behavior |
| M8 | Usage reporting and product state visibility | M0 reporting decisions; M2/M5 state contracts | Disclosed payload, retrievable aggregates, retention and bounded failure behavior; accurate non-promotional UI |
| M9 | Opt-in canaries and supported offering | Applicable evidence from M0–M8 | Platform, security, recovery, sync, support and compatibility exit gates met |

M3 and M4 can advance in parallel after M1/M0 respectively; do not postpone
Windows discovery until synchronization is complete. M8 UI design can start
early, but collection stays disabled until its disclosure/control contract is
settled. M9 is progressive: enroll a standalone canary after M1/M2 and its
platform's recovery checks, then expand only as each additional capability passes.

### M0 Define the contracts before durable user data

- Complete an operation matrix for every API family: standalone support,
  connected offline support, write authority, permissions, conflicts, deletion,
  retention, and required online coordination. Explicitly classify unsupported
  operations rather than relying on missing configuration.
- Define local installation identity separately from account, realm, agent,
  device, and replica identity. Specify adoption of a populated local account
  into a new remote account and joining an already populated remote account.
  Existing single-home cell moves/imports do not implement either sync flow.
- Choose process/service ownership, private paths and transport, component
  manifest format, supported OS/architecture versions, PostgreSQL distribution
  and extension requirements, and application/schema/protocol compatibility.
  Evaluate native processes first; use measured counterevidence to revisit them.
- Write the sync envelope and state machine: origin, operation ID, expected
  version/causal basis, schema/protocol version, authorization context, durable
  receipt, and explicit rejected/conflicting outcomes. Decide per-domain rules;
  do not choose blanket last-write-wins or wall-clock ordering.
- Define a capture activation barrier for every server and worker version that
  can write the account. Either capture below incompatible application writers
  or prove they are drained/blocked before enrollment and snapshot creation.
  Include transactions begun before the barrier that commit after it; observing
  no old processes is insufficient. Define capability checks and rollback
  behavior; an ordinary remote client using a new server does not prove that
  old writers are covered.
- Define the authoritative account's incarnation and cutover contract for
  evacuation and backup restore. Specify which journals, receipts, deletion
  knowledge and device enrollment records move with it, which are invalidated,
  and how cursors authenticate continuity. Set recovery limits for an older
  remote backup; do not assume that accepted operations absent from it can be
  recovered without retained evidence or surviving replicas.
- Set accounting and audit semantics before replication: stable logical-action
  identity/origin independent of transport batches, local utilization, hosted
  work, sync overhead, and derived rollups/index state. Distinguish new user
  reads from replay and preserve event
  provenance without counting an action twice. Nonbilling activity and billable
  usage are different concepts; pricing decisions need not be settled here.
- Define authoritative enrollment, authorization and feature-entitlement policy
  for both SaaS and self-managed destinations before M5. Do not assume a
  self-managed service requires a SaaS subscription, or trust a local plan row.
  Record intentional feature differences as testable policy, not exceptions
  inferred during enrollment.
- Define account/device enrollment, offline authorization lifetime, deletion
  knowledge retention and retired-device rebootstrap, plus authority for local
  curation versus shared curation. Stable transcript/evidence identity is a
  prerequisite, not a later repair by renumbering records.
- Set measurable release budgets for startup, idle CPU/RAM, disk growth, search
  latency/relevance, and sync recovery. Establish small and large representative
  synthetic account fixtures and expected answers before measuring alternatives.

**Exit:** reviewed contract records linked from this plan; each unresolved item
has an owner and a milestone it blocks. Runtime experiments may use disposable
synthetic identities meanwhile. Do not create durable customer data with IDs or
schemas the eventual enrollment design cannot preserve or migrate.

### M1 Build an isolated native cell foundation

- Introduce an explicit runtime profile and a dependency-light lifecycle module
  or companion executable. Keep its location and command names open until the
  module review; native installers and a possible infra CLI frontend use the
  same implementation. Do not embed the cloud provisioning engine in the client.
- Implement inspect/plan/install/start/stop/status/repair/remove semantics with
  one lifecycle owner, an exclusive operation lock, an installation manifest,
  and recovery after interruption. Preserve data on ordinary removal.
- Own only this installation's PostgreSQL process and directories. Check
  identity before stop/repair, never attach to or kill a user's unrelated
  database, and keep data outside package-managed binary directories.
- Construct local process configuration from an explicit allowlist. An existing
  shell's database, provision, dispatch, or cloud credentials must not redirect
  local startup to hosted resources or activate external jobs.
- Give every active server lifecycle a private archive/spool directory, including
  overlapping upgrades and multiple cells. Configure the applicable
  `TMPDIR`/`TEMP`/`TMP` before launching the process; protect permissions/ACLs and
  verify ownership before cleanup. Current startup sweeps matching archive names
  in the process-default temporary directory assuming exclusive ownership. Never
  point that sweep at host-global temp or share it between active servers.
- Bind API, PostgreSQL, health and metrics privately, avoid port collisions,
  protect files/Windows ACLs, and authenticate local clients. Loopback alone is
  not an authorization boundary against other users or untrusted browser requests.
- Explicitly select allowed worker jobs. Review retention, purge, style rollout,
  and outbound dispatch for each runtime/data role. A local projection must not
  execute imported remote claims, purge account data, or dispatch mail.
- Require database/schema and authenticated operation checks for cell readiness.
  A bare `/readyz` response is insufficient because current server startup can
  run without a database. Prove native startup on every intended OS early;
  cross-compilation alone does not establish Windows runtime support.

**Exit:** repeated install/start/stop and crash restart preserve synthetic data;
parallel lifecycle calls serialize; unrelated processes remain untouched;
starting or restarting a second cell leaves the first cell's active and retained
archives and foreign temporary files intact. Listeners are private; disabled
local support changes no existing install, account endpoint, background service,
or hosted job behavior.

### M2 Make standalone use complete enough for a canary

- Add explicit profiles without changing named hosted-account resolution by
  default. Local resolution must survive a blocked directory/control-plane
  connection; a local failure must never silently reroute writes remotely.
- Stage and atomically activate profile/credential bindings, with recovery from
  interruption. Define how running MCP clients and hooks adopt a changed profile;
  a partial switch must not combine one cell's identity with another's endpoint.
- Bootstrap a standalone account and its operator/agent permissions without
  granting hosted entitlements. Retain identity verification through CLI, MCP,
  hooks, console and TUI. Avoid automatic identity remapping on later enrollment.
- Exercise transcripts, narrative memories, facts, their references and recall,
  including client-driven curation for the declared local scope. Define local
  lease ownership; sync must not transplant completed/shared authority later.
- Provide local operation and storage status with explicit unsupported hosted
  features. Existing remote-only behavior remains available unchanged.
- Compare retrieval using identical representative records, queries, indexes,
  configuration and compatible application versions. Verify relevant memories
  and provenance as well as latency; same PostgreSQL engine alone is not parity.

**Exit:** multiple local AI clients can capture/read/search through real API/MCP
paths, restart offline, and recover committed data. A blocked reporting or hosted
endpoint does not delay core operations. No copied vault keys or provider
credentials are needed for a canary using synthetic transcripts and memories.
Interrupted profile activation and concurrently running clients preserve correct
identity/routing and recover to an explicit, usable state.

### M3 Deliver and update the cell through native installation

- Define one compatible component manifest for app, server, selected workers,
  PostgreSQL and required extensions. Verify downloaded artifacts; support
  repair without overwriting a newer incompatible data directory.
- Adapt Homebrew and shell delivery on macOS, the chosen native Windows
  installer/distribution path, and Linux shell/package delivery to that lifecycle.
  Resolve packaging/signing/licensing and supported platform requirements in
  M0/M3 before selecting dependencies. A package manager supplies software;
  the lifecycle owns data transitions.
- Preserve installation-channel ownership: do not let a self-updater overwrite
  Homebrew/MSI/package-owned files. Define ownership of private managed payloads
  separately, and recover interrupted updates through the selected channel.
- Define per-user versus system service ownership, login/boot/on-demand startup,
  least privilege, multi-user isolation, sleep/resume, reboot and PATH behavior.
  Keep the remote-only artifact free of cell payload and startup hooks.
  Specify foreground/non-systemd operation on supported Linux environments.
- Implement update preflight, safe writer coordination, recovery point,
  serialized schema migration, component activation, and authenticated health
  verification. Persist each step so interrupted updates can resume or recover.
- Separate compatible binary rollback, application schema recovery, and
  PostgreSQL major upgrade. Test restoration including pending sync operations;
  never describe disabling a flag as reversing a migration.

**Exit:** the same install/update/reinstall/uninstall contract passes on clean
machines for every declared platform/channel. Test locked files, long/non-ASCII
paths, occupied ports, missing permissions, full disk, partial downloads,
interrupted migration, unsupported version skew, reboot and offline restart.
Restore is demonstrated, not inferred from successful backup creation.

### M4 Prove the synchronization mechanics in isolation

- Build a deterministic harness with two local cells, a remote authority and a
  second remote move/restore destination, plus an ordinary remote-only client.
  Use real stores and synthetic accounts; inject partitions, duplicate/reordered
  delivery, crashes, delayed acknowledgements, expired history and incompatible
  versions without touching production.
- Capture accepted local writes and their outbound operations in one database
  transaction. Commit inbound mutations and their deduplication/receipt state
  atomically. Acknowledge only after durable commit; bound queues and batches.
- Include writes made by remote-only clients and relevant background jobs in
  the remote change stream. A journal inside only the new desktop client misses
  existing write paths. Applied inbound records must not create replication loops.
- Prove the M0 capture barrier using mixed old/new server and worker binaries.
  Exercise writes before and after snapshot creation, activation, and rollback.
  Include in-flight transactions crossing the barrier and maintenance writers.
  Either all accepted writes enter the stream or incompatible writers are
  fenced/drained. Losing coverage suspends sync readiness explicitly, preserves
  pending work, and requires a safe catch-up/rebootstrap before resuming.
- Make snapshot watermarks, cursor scope, retry semantics, retention and
  resnapshot/rebase behavior explicit. Never lose pending local operations when
  rebuilding a replica; old backups and cloned devices need new session rules.
- Integrate new sync tables with export/import, evacuation fencing/finalization,
  backup/restore and purge registries. Exercise remote cutover with a committed
  operation whose ACK was lost, a stale endpoint, pending local work and deletion
  knowledge. Repeating a snapshot alone does not repair lost acceptance receipts.
  Test older remote restores and rollback against the declared recovery boundary:
  detect missing history, reconcile surviving acceptance evidence, and surface
  unresolved loss/conflicts rather than silently duplicating or dropping work.
  An ambiguous accepted operation must not be silently retried as a new action.
- Apply the M0 accounting contract to snapshot and incremental replay. Prove
  expected usage/audit outcomes for capture, new user reads, duplicates, lost
  ACKs, retry and rebuilding a replica before M5/M6 activation. Imported audit
  provenance, local sync diagnostics and derived rollups must not masquerade as
  repeated user actions; sync diagnostics can still record actual sync work.
  Split/regroup batches during retry and verify both stored events and rollups:
  suppressing every event is not a substitute for recording each required action.
- Define ordering/identity independently from timestamps and database-assigned
  sequence numbers. Carry enough evidence to distinguish retries, competing
  changes, explicit recreation, and stale resurrection after deletion.

**Exit:** within the declared durability/recovery contract, injected failures
cannot lose an acknowledged operation, apply a retry twice, cross an account
boundary, loop indefinitely, or skip changes at a snapshot boundary. Demonstrate
both committing-before-ACK and crashing-before-commit. For an older authoritative
backup, prove detection and bounded recovery with available evidence; an
unrecoverable gap blocks a claim of convergence.
Mixed-version capture and move/restore tests must pass, with expected accounting
counts. Preserve a reproducible failure seed and minimized fixture for defects.

### M5 Enroll a device and synchronize an authorized read replica

- Use explicit owner-authorized enrollment with device credentials and least
  privilege. Agent-scoped API access must not become full-account download
  permission. Separate replica-host trust from the permissions of each local AI
  agent; the OS owner can access downloaded open-plane data.
- Negotiate protocol/schema/features and use authoritative destination policy.
  A modified local row cannot set a paid plan, provider route, ownership, or
  credential authority. Use M0's explicit SaaS and self-managed policy fixtures;
  enrollment cannot pass while either destination's admission contract is open.
- Block enrollment/snapshot activation until M4's complete-writer capture
  barrier is satisfied. Bind continuation to the authenticated account authority
  incarnation. After relocation or restore, prove continuity or enter explicit
  rebootstrap/reconciliation that preserves pending local work and acceptance
  evidence; a copied cursor or matching account ID alone is insufficient.
- Preflight capacity in both directions and enforce admission during transfer.
  Resume or refuse coherently; never trim data or label a partial copy synchronized.
- Tie the initial consistent snapshot to a durable change-stream watermark,
  catch up while the remote account is changing, and publish readiness only
  when the declared data coverage is available. Exclude executable leases,
  authentication tokens, private vault keys and remote dispatch credentials
  from materialized views. Approved sealed records follow M7's separate rules.
- Define revoke, disconnect, re-enroll and expired-cursor behavior, including
  which cached data remains available and how stale authorization is presented.

**Exit:** remote writes during bootstrap are present exactly once; interrupted
transfer resumes safely; an unauthorized/shared device cannot enroll; capacity
refusal preserves both sides; mirrored email/messages remain read-only and
produce zero delivery, claim, acknowledgement or billing effects. Old-writer
rollback cannot falsely report caught-up status, and authority move/restore
cannot silently reuse invalid cursors or erase pending work. Snapshot/replay and
new reads have the distinct accounting outcomes established in M4.

### M6 Add writable domains with deletion and conflicts together

Advance one family at a time after its semantics and failure cases pass. A
partial writable canary must advertise its coverage; it is not whole-account
writable synchronization.

| Family | First bounded slice | Gate before enabling it |
|---|---|---|
| Transcripts and evidence | Append and read with stable entry/origin IDs | Concurrent appends and remote-only writes preserve ordering semantics, evidence ranges and provenance; retention/replay cannot resurrect removed content |
| Memories and facts | Independent capture, then updates/replacements/relationships | Expected versions, competing assertions, provenance, tombstones and explicit recreation survive retry/reconnect; no silent overwrite |
| Curation and shared state | Local inference with scope-aware plans | A plan based on stale versions is rejected or rebased explicitly; remote leases never acquire local authority; no AI inference moves into the backend |
| Other account content | Avatars, profiles and remaining declared domains | Per-domain revisions, policy and deletion rules; enumerate residual exclusions rather than omitting them |

Deletion protection ships with the first writable slice. Define how deletion
knowledge outlives replayable history, how a long-offline or restored device is
forced to rebootstrap, and which explicit authorized operation may recreate a
deleted item. Replication applies an already-authorized deletion event; it must
not fabricate a new direct-user authorization or call a foreground deletion
ceremony as though a user had just requested it. Pending queues, snapshots and
conflict records need deletion and retention treatment too; they must not become
extra indefinite content stores.

**Exit:** two offline local writers plus an online remote-only writer pass
conflicting edits, deletion versus stale upload, lost ACK, retry, reordering,
crash, restore and schema skew tests. Conflicts preserve user work and have a
resolution path. No unexplained loss or duplicate effects is acceptable; target
latency and convergence bounds use the budgets agreed in M0.

### M7 Complete connected authority and account transitions

- Keep email delivery, cross-machine mailbox/request state and shared leases at
  the remote authority. Offline outgoing work is explicitly pending intent;
  validate authorization, quotas and idempotency remotely before claiming it
  was sent, delivered, assigned or acknowledged. Define cancellation races.
- Decide and implement the initial secrets/TOTP coverage. Preserve sealed
  associated-data identities; transfer/enroll client keys separately from
  account replication. Test absent keys, rotation/retired epochs, revoked grants,
  offline access policy and recovery without sending plaintext keys to a server.
- Complete standalone-to-connected adoption, joining an existing account,
  remote-only coexistence, disconnect, downgrade, expired subscription and
  account closure semantics. A mode change does not cancel billing or close an
  account. Remote closure must not be undone by a stale local upload.
- Reconcile access/role changes and revocation with cached local reads and
  pending writes. The product must state the permitted offline access horizon;
  it cannot promise immediate erasure of an unreachable device's copy.
- Exercise oversized local/remote accounts and growth while offline. Preserve
  unsynchronized changes and provide a visible recovery path; never silently
  convert whole-account sync into selective sync to fit a plan.
- Verify connected product policy against the M0/M4 accounting mechanics already
  required by M5/M6. Replica application and retries must not double-count
  billable usage or logical audit events; distinguish actual remote work from
  local utilization and sync overhead. Resolve any remaining pricing policy
  before enabling the affected feature, rather than redefining replay semantics.

**Exit:** authority and identity tests pass across local, connected-local and
remote-only clients for SaaS and a self-managed test service. The supported
operation matrix is complete, including explicitly decided exclusions and their
user-visible behavior. Product decisions still open cannot be treated as passed.

### M8 Add reporting and accurate product visibility

- Settle disclosed fields, purpose, identity/account linkage, retention and
  controls before enabling reporting. Define a modification indicator narrowly;
  no source diffs, transcripts, memories, secrets or content-bearing paths.
- Assign an owner for the receiving service and its operator interface. Implement
  bounded intake/validation, permitted storage and aggregation, retention/removal,
  and an authorized query/report path. Define report identity, retry deduplication
  and partial-report semantics so a lost response does not inflate totals.
  Report observed installations and coverage accurately; offline or modified
  clients make reporting incomplete, and installation counts are not people.
- Keep reporting independent from sync and license enforcement. Use bounded
  buffering, asynchronous short deadlines, backoff and expiry/drop behavior.
  Blackholed networks and a full telemetry queue must not stall local work.
- Show local-only, pending, caught-up, conflict, stale, capacity-blocked and
  service-unavailable states. Distinguish paid restriction, unconfigured,
  unsupported and offline features in CLI/MCP, TUI and console.
- Offer contextual details for hosted capabilities without banners or repeated
  prompts. Preserve useful local statistics even when hosted features are absent.

**Exit:** inspect exact payloads and disclosure; prove reporting failure within
the agreed CPU/disk/network budgets and unaffected core operation latency.
Send synthetic permitted counters end to end, lose a response and retry, then
retrieve the expected aggregate through the authorized operator path. Verify
retention/removal and bounded rejection of malformed, oversized or abusive input.
Collector unavailability still cannot interrupt local use; no collection is
enabled before the disclosure/control contract passes.
UI acceptance includes misleading-success cases, not just screenshots.

### M9 Expand canaries and meet the support gate

Keep separate controls for shipping components, installing a cell, routing to it,
enrolling sync, enabling each writable family, reporting, and changing defaults.
Each cohort has an owner, exact component/protocol versions, enabled operations,
evidence location, rollback procedure and next review point.
Before activating sync, record complete server/worker capture coverage, the
current authority incarnation, destination policy and replay-accounting proof.
Recheck coverage after version changes or rollback; pause sync on loss of the
barrier until safe recovery passes, without deleting pending local work.

Start with synthetic accounts and explicitly enrolled machines. For each stage,
verify the old remote path and the new path together. Read comparison is allowed
only on authorized isolated data; shadow traffic must never replay production
email, mailbox claims, billing, account closure or other side effects.

Pause expansion on data loss/resurrection, an authority violation, unintended
network exposure, incorrect delivery, broken existing behavior, or resource use
beyond agreed limits. A locally usable stop-sync control preserves pending work
and local access. Recovery must not require a working control plane or discard
local changes merely to switch the client back remotely.

The supported release requires all promised domains, all three platforms,
upgrade/restore evidence, security review, support diagnostics/runbooks,
reporting disclosure, usable conflict/capacity handling and compatible old/new
clients. Record implementation and rollout separately in the feature catalog
and regenerate its scorecard. Changing the default or retiring a hosted tier
requires its own reviewed transition plan after this gate.

## First implementation batch for Claude to refine

Do not turn this into one long branch. Suggested first independently reviewable
slices, after agreement on their contracts:

1. **Contract and acceptance fixtures:** complete M0's operation/authority table,
   identity choices and platform matrix; add synthetic fixtures and expected
   outcomes. Identify blocking decisions without implementing a second store.
2. **Inactive runtime profile:** private listener configuration, explicit worker
   eligibility and database-backed identity/health checks. Keep existing
   defaults unchanged; prove that copied remote work cannot run locally.
3. **Disposable native lifecycle proof:** isolated managed PostgreSQL and server
   with repeatable start/stop/restart/repair, process/data/temp ownership protection
   and native smoke execution on all three OSes. Test concurrent cells' archive
   isolation. No routing change on ordinary installs.
4. **Explicit local connection profile:** a deliberately selected test profile
   uses that cell through CLI/MCP while directory access is blocked; remote-only
   profiles continue through existing contracts.

In parallel, specify M4's journal and failure harness using the agreed identities;
do not enable writes between real accounts. After this batch, use measured
lifecycle effort, platform gaps and one fault-tested replication slice to size
the remaining work. Synchronization is the largest uncertainty, followed by
native upgrades/recovery. Avoid a calendar commitment based only on code reuse.

## Validation and evidence for each slice

Use focused contract/integration tests during development, then follow the
current [repository gates](../../../CLAUDE.md) before any push. `make check-local`
selects the required path; application/shared tooling and these research docs do
not qualify as a verified-release pin-only roll. Require database and Node tests
where prescribed, keep isolated resources private, and read the actual gate's
terminal verdict. Preserve failures as failures. Do not rerun unchanged passing
gates or substitute a mock-only result for platform/database/live evidence.

Keep full applicable GitHub checks, exact-head merge verification, exact
post-merge CI, and existing release/rollout requirements. The local-cell feature
does not bypass backup-first cloud rollout or schema/recovery obligations. A
documentation draft is not authorization to publish a release or enroll a device.

Every completed slice should record:

- Owner/reviewer, source commit and bounded scope, plus dependencies fulfilled.
- Tests and failures with exact outcomes; separate static, fixture, real
  PostgreSQL, native-platform, release and canary evidence.
- Compatibility range, activation controls, changed data/side effects, and
  recovery limits. Preserve pending writes when disabling a capability.
- Remaining gates and next slice. Store credentials, private bodies and customer
  data outside ordinary evidence; diagnostics must be value-free.

## Claude review and living progress record

Claude should first challenge the runtime boundary, identity/adoption design,
all-writer change capture, deletion retention, worker eligibility and upgrade
recovery. Add source-backed findings and measured alternatives. A simpler design
may replace this proposal if it meets the same requirements; record why and how
existing local data would survive. The local-cell decision remains revisable.

Resolve these before their dependent milestones:

| Decision | Needed before | Reviewer contribution |
|---|---|---|
| Exact standalone/offline operation set and secrets coverage | M0, M2, M7 | Fill the operation matrix; distinguish local curation from shared work |
| Supported OS/architecture versions and PostgreSQL packaging/extensions | M1/M3 | Native proof, footprint and distribution/maintenance assessment |
| Startup/service ownership and lifecycle command/module placement | M1/M3 | Compare user/system scope and infra frontend without dependency inflation |
| Identity adoption, device trust, at-rest protection and offline access horizon | Durable enrollment in M5 | Threat scenarios and existing account/vault constraints |
| Capture activation and rollback barrier across server/worker versions | M4 proof; any M5 snapshot/enrollment | Mixed-version writes, verified capture or drain, explicit recovery after coverage loss |
| Remote authority incarnation, cutover and restore continuity | M4 proof; M5 enrollment | Lost-ACK relocation, portable receipt/deletion state, stale endpoints and older-backup recovery limits |
| Logical usage/audit identity, replay and derived-state accounting | M4; before M5/M6 activation | Expected counts for user actions, snapshot, retry and replica application; separate pricing policy |
| Self-managed and SaaS admission, authorization and feature entitlements | M5 enrollment; M7 feature activation | Authoritative policy fixtures for both destinations; no assumed SaaS billing rule for self-managed |
| Conflict rules, stable transcript evidence and deletion/history horizon | M4/M6 | Executable multi-writer and stale-device counterexamples |
| Reporting fields, linkage, disclosure, controls and receiver ownership | Enabling M8 | Payload/resource contract, operator aggregate and retention proof, product/privacy review |
| Resource/retrieval budgets, canary duration and expansion thresholds | Enrolling the relevant M9 cohort | Named fixtures, baselines and pass/fail values |
| Existing free users and default installation | Product transition after M9 | Separate proposed product transitions from earlier destination admission contracts |

For each milestone, append a record using `ID / owner / state / PR or decision /
evidence / unresolved gate / next action`. Use proposed, in progress, blocked, or
complete deliberately; a merged disabled slice is not an enabled capability.
Update the feature scorecard through its generator when implementation progresses,
and keep this plan focused on execution order and decisions rather than a second
competing readiness ledger.
