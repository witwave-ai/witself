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
The finished upgrade connects the existing local cell to paid SaaS automatically;
clients keep using that cell without starting over or manually moving data.

## Requirements to carry into every slice

- The default new WitSelf installation automatically provides a usable free local
  cell, replacing the hosted free plan. Installation provisions and initializes
  the cell in the same operation, without a separate deployment or local-mode
  selection step; users do not administer PostgreSQL. Avoid mandatory Docker or
  Kubernetes. PostgreSQL is a separate managed process, not an embedded database.
- macOS, Windows, and Linux belong to the first supported local release. A
  macOS canary is useful evidence but does not satisfy that release gate.
- Keep a lightweight remote-only installation for SaaS and self-managed
  services, without a local database, cell download, or service registration.
- Connected cells continuously synchronize the whole account by default. Core
  local reads, searches, transcript capture, and declared mutations continue
  during a service outage. The AI client still performs memory inference.
- Paid signup/enrollment and return to free local use preserve the same local
  cell, logical identities, data and client integrations. Automate bootstrap and
  catch-up; do not require manual export/import, recreated agents, a new empty
  account or a client endpoint switch. Local work continues through transfer.
- Agent messaging is required locally and through the paid transition. Standalone
  coordination uses local authority; connected message/request routing, claims,
  acknowledgements and recovery need explicit protocol rules. Copied records or
  leases never authorize duplicate execution. Cross-device delivery needs a
  working transport; local messaging must not depend on a SaaS subscription.
- Email receipt/delivery is the sole hosted-only product feature exception,
  because of its infrastructure overhead. Local history can be read/searched;
  provider dispatch, routes, billing and hosted entitlements retain service authority.
- Preserve data and pending changes on capacity refusal, disconnect, failed
  upgrade, or ordinary uninstall. Distinguish local durability, remote acceptance,
  convergence, and delivery to an external service.
- Require disclosed, bounded, nonblocking usage reporting and quiet contextual
  hosted-feature discovery. Reporting failure cannot interrupt local work.
- Ship inactive foundations and deliberately enrolled canaries. The free local
  default and replacement of the hosted free plan are selected product direction
  (2026-10-23). Activation timing, migration of existing free SaaS customers, and
  live catalog, billing or account changes require a reviewed transition plan;
  ordinary upgrades must not silently change existing bindings or account homes.

Non-email features must not be excluded merely because a cell is local. The
operation matrix still needs precise safe coverage, including vault/key behavior
and cross-device coordination; this is design work, not permission to make another
core feature SaaS-only. Future decentralized cells, exposing
a local cell as a public bare-metal service, and alternative backup destinations
remain follow-on opportunities. Basic local restore is required now.

## Code boundaries to use

These entry points were checked at `6c55133f1e7c`. The original research records
its earlier `adc6f186` baseline. Recheck affected code before implementing a slice;
the paths below identify reuse seams, not finished local-mode implementations.
The request-coordination and account-console seams added by the 2026-10-23
review were rechecked at `5380baec`; their current behavior motivates the gates
below rather than proving local or synchronized support.
On 2026-10-23 every source reference in these five documents was re-verified at
`88fce5a3`: no behavioral statement was contradicted; 18 line anchors were
corrected.

| Boundary | Existing entry points | Planned adaptation |
|---|---|---|
| Connection and installed identity | [CLI resolution](../../../cmd/witself/main.go), [local bindings](../../../internal/local/local.go), [MCP](../../../cmd/witself/mcp.go) | Explicit local, connected-local, and remote-only connection routes; offline local resolution without changing existing directory-based routing |
| Account-console authority | [manager resolution](../../../cmd/witself/account_console.go#L35), [account-view authorization](../../../internal/dashboard/account.go#L97) | Authenticated local connection route/installation trust independent of the hosted directory; preserve operator/agent credential separation and origin/identity checks |
| Application and database | [server startup](../../../cmd/witself-server/main.go), [store](../../../internal/store/store.go), [migrations](../../../internal/store/migrate.go) | Private local runtime configuration, durable bootstrap identity, serialized lifecycle and authenticated database-backed health |
| Temporary archives | [startup spool sweep](../../../internal/server/spool_sweep.go), [self-export](../../../internal/server/export_self.go) | Private temporary directories owned by one active server lifecycle; never sweep host-global temporary files or another cell's archives |
| Background jobs | [worker startup](../../../cmd/witself-worker/main.go), [runtime](../../../internal/worker/runtime.go) | Explicit job eligibility for standalone data versus synchronized projections; prevent copied remote work from executing |
| Native delivery | [release targets](../../../.goreleaser.yaml), [Unix installer](../../../install.sh), [Windows installer](../../../install.ps1), [Homebrew generator](../../../tools/homebrew-formula/main.go) | Deliver compatible cell components separately from the lean client and invoke one shared lifecycle |
| Infrastructure management | [infra module](../../../infra/pulumi/README.md), [protected preview](../../../infra/pulumi/cmd/witself-infra/deletion_plan.go) | Reuse inventory, preview, identity, and recovery concepts without introducing Pulumi/cloud dependencies into local or remote-only installs |
| Synchronization | [transcripts](../../../internal/store/transcript.go), [memory versions](../../../internal/store/memory.go), [export](../../../internal/store/export.go), [import](../../../internal/store/import.go) | Stable operation/evidence identities, transactional change capture, consistent snapshot plus catch-up, resumable application |
| Messages and request coordination | [protocol ownership](../../../internal/store/message.go#L1873), [request operations](../../../internal/store/message_request.go#L548), [inspection reconciliation](../../../internal/store/message_request.go#L669) | Atomic visibility of each compound request operation; request-wide authority and ordering; projection-only inspection of imported requests |
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
| M1 | Inactive local runtime and lifecycle foundation | M0a | Private authenticated cell, isolated state, repeatable lifecycle, unchanged hosted path |
| M2 | Standalone local operations and explicit routing | M1; M0b | Durable offline CLI/MCP and authorized account-console use, retrieval comparison, restart and concurrency proof |
| M3 | Native installers, updates, and restore | M1; M0a packaging and platform decisions; M2 smoke fixtures (not complete M2) | Clean-machine and interruption matrix on macOS, Windows, Linux; lean remote package |
| M4 | Synchronization journal and failure simulator | M0c capture, authority-continuity and accounting contracts | Atomic capture/apply, mixed-version writer coverage, duplicate-safe move/restore and replay accounting |
| M5 | Enrollment and authorized snapshot plus catch-up | M4 capture barrier and recovery proof; M1 isolation; M0c destination policy and identity transitions | Authorized admission, restartable snapshot, proven authority continuity and read-only convergence without side effects |
| M6 | Writable domain synchronization | M5; M2; deletion/conflict rules in M0c | Two local writers plus remote-only writer converge under the failure matrix |
| M7 | Connected authority, vault coverage, and account transitions | M5/M6 as appropriate; M0c destination and accounting contracts | No copied delivery authority; tested revocation, capacity, key and lifecycle behavior |
| M8 | Usage reporting and product state visibility | disclosure, control and receiver decisions (founder); M2 for local status states; M5 only for connected-state labels | Disclosed payload, retrievable aggregates, retention and bounded failure behavior; accurate non-promotional UI |
| M9 | Opt-in canaries and supported offering | Applicable evidence from M0–M8 | Platform, security, recovery, sync, support and compatibility exit gates met |

M1 and M4 start in parallel from different M0 subsets; M3 starts when M1 has a
lifecycle interface and M2 has smoke fixtures. Do not postpone Windows discovery
until synchronization is complete. M8 UI design can start early, but collection
stays disabled until its disclosure/control contract is settled. M9 is progressive.

### M0 Define the contracts before durable user data

#### Terms

- **Cell:** one application plus its database, not a deployment topology.
- **Installation identity:** the identity of one managed local installation,
  separate from the logical account and its devices.
- **Device or replica:** an enrolled participant or its synchronized account copy.
- **Authority incarnation:** the authoritative account's restore/cutover
  generation, not its ID or software version.
- **Enrollment**, **adoption**, and **namespace join:** respectively authorizing
  a device, hosting an existing standalone logical account at a destination,
  and merging two populated accounts; these are three distinct transitions.
- **Connection route:** the client-side choice of local, connected-local, or
  remote-only cell for CLI/MCP/hooks.
- **Access profile:** MCP's `full`, `read-only`, `curator-preview`, or
  `curator-apply` permission surface
  ([current profiles](../../../cmd/witself/mcp.go#L1191)).
- **Local runtime configuration:** server/worker private listeners, owned paths,
  child environment, job eligibility and readiness.

#### M0a runtime and lifecycle contract

- Choose process/service ownership, private paths and transport, component
  manifest format, supported OS/architecture versions, PostgreSQL distribution
  and version floor, extension requirements, and application/schema/protocol compatibility.
  Evaluate native processes first; use measured counterevidence to revisit them.
- Set measurable release budgets for startup, idle CPU/RAM, disk growth, search
  latency/relevance, and sync recovery. Establish small and large representative
  synthetic account fixtures and expected answers before measuring alternatives.

#### M0b standalone identity and policy contract

- Define local installation identity separately from account, realm, agent,
  device, and replica identity; specify local, connected-local and remote-only
  connection routes.
- Define offline account-console trust independently of hosted directory access,
  preserving origin/account verification and separate operator/agent authority.
- Specify standalone messaging authority: local routing, ownership/delegation,
  fencing epochs, deduplication, receipts and cancellation. Independent local
  messaging must continue without SaaS enrollment.
- Define the standalone columns of the operation matrix for every API family:
  support, write authority, permissions, conflicts, deletion and retention;
  explicitly classify unsupported operations.
- Define the standalone policy snapshot the free local cell runs under. Feature
  gates read the applied plan/policy snapshot from the database
  ([mutation gate](../../../internal/store/message.go#L266),
  [snapshot interpretation](../../../internal/store/message.go#L308)), and the
  current [hosted Personal plan](../../../web/plans/plans.json#L13) disables
  messaging and has zero secret capacity. The local cell therefore needs its own
  explicit standalone policy (messaging enabled, declared vault capacity, no
  hosted entitlements) rather than any hosted plan row or the unmanaged defaults
  (seeding applies no catalog, limits and features start empty, legacy messaging
  is allowed and a missing secret limit means unlimited:
  [seeding](../../../internal/store/seed.go#L33),
  [messaging default](../../../internal/store/message.go#L286),
  [secret limit](../../../internal/store/secret_limit.go#L75)), and connected
  enrollment must never accept that local snapshot as entitlement.

#### M0c synchronization contract

Nothing in M0c blocks M1–M3.

- Complete the connected columns of the operation matrix for every API family:
  connected offline support, write authority, permissions, conflicts, deletion,
  retention, and required online coordination. Explicitly classify unsupported
  operations rather than relying on missing configuration.
- Specify adoption of a populated local account at a new remote destination,
  namespace join with an already populated remote account, and device enrollment
  as distinct transitions. Existing single-home cell moves/imports do not
  implement either synchronization flow.
- Define seamless upgrade/return acceptance: preserve local endpoints and logical
  identities; automate authorized data bootstrap while journaling ongoing writes;
  resume interruptions; preserve local use on capacity or identity conflicts.
  Include message history, pending sends, completed requests and live claims.
  Use the [populated-account transition acceptance](#populated-account-transition-acceptance)
  fixture: independently populated local and hosted accounts must not be reduced
  to an empty-destination test. Resolve identity/ownership collisions and sealed
  identity continuity before enabling that enrollment path.
- Specify connected messaging authority alongside M0b standalone authority: routing,
  ownership/delegation, fencing epochs, deduplication, receipts and cancellation.
  Define which cross-device work waits during a partition; independent local
  messaging must continue. SaaS enrollment cannot create two valid claim owners
  or replay completed work, and return to local use cannot silently reset claims.
  Set the authority unit to the complete request graph, or prove equivalent
  coordination: offers/declines, selection generations, all sibling reservations,
  claim/renew/release, completion, cancellation and expiry share its ordering.
  One owner per individual claim alone cannot enforce request-wide capacity or
  safe settlement. Define authority transfer and stale-epoch rejection for this
  whole unit, including which operations wait during a partition.
  Preserve the visibility boundary of each compound request operation and its
  linked opening/offer/result messages during snapshots and incremental replay.
  Specify projection-only inspection of imported requests; ordinary list/show
  currently reconcile lifecycle state and are not inherently read-only.
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
  Local messaging is not gated by a paid SaaS entitlement; enabling it must not
  grant hosted email, provider credentials or forged paid-plan authority.
- Define account/device enrollment, offline authorization lifetime, deletion
  knowledge retention and retired-device rebootstrap, plus authority for local
  curation versus shared curation. Stable transcript/evidence identity is a
  prerequisite, not a later repair by renumbering records.

**Exit:** reviewed contract records linked from this plan; each unresolved item
has an owner and a milestone it blocks. Runtime experiments may use disposable
synthetic identities meanwhile. Do not create durable customer data with IDs or
schemas the eventual enrollment design cannot preserve or migrate.

### M1 Build an isolated native cell foundation

LC-S1 implements the opt-in runtime configuration, server/worker private listeners, owned archive spool, explicit worker job selection, and database/schema/bootstrap-account readiness bullets; lifecycle, child-environment construction, PostgreSQL ownership and singleton isolation: LC-S2 (merged #824); migration cancellation and restart policy remain deferred.

LC-S2 implements the disposable lifecycle module, exclusive ownership and interruption recovery, PostgreSQL/process identity, explicit child environments, private spool/temp layout, loopback ports, and bounded readiness checks through tests and a harness only; lifecycle planning, restart policy, migration cancellation, platform service adapters, downloads, installer manifest trust, Windows DACL privacy, and reclaiming a crashed cell's stale SysV shared-memory segment when it is purged (macOS allows 32 machine-wide; a normally stopped cell leaves none) remain deferred, with nothing installed or activated.

- Introduce an explicit local runtime configuration and a dependency-light lifecycle module
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
  verify ownership before cleanup. Today, in the hosted configuration, the sweep
  removes regular files matching
  `witself-(backup-validate|account-import|self-export)-<n>.tar.gz` in the process
  temp directory with no ownership check
  ([sweep](../../../internal/server/spool_sweep.go#L11)); self-export, backup
  validation and import all spool there
  ([self-export](../../../internal/server/export_self.go#L73),
  [validation](../../../internal/server/server.go#L6104),
  [import download](../../../internal/server/server.go#L7590)). Never point that
  sweep at host-global temp or share it between active servers.
- Bind API, PostgreSQL, health and metrics privately, avoid port collisions,
  protect files/Windows ACLs, and authenticate local clients. Loopback alone is
  not an authorization boundary against other users or untrusted browser requests.
- Explicitly select allowed worker jobs. Review retention, purge, style rollout,
  and outbound dispatch for each runtime/data role. A local projection must not
  execute imported remote claims, purge account data, or dispatch mail. Today,
  in the hosted configuration,
  [ten configurable job types](../../../cmd/witself-worker/main.go#L177) exist,
  three enabled by default, and the default `message_rate_bucket_cleanup` also
  performs [activity retirement](../../../internal/store/message_rate_bucket_cleanup_worker.go#L91)
  of nonbilling events older than 35 days. Request reconciliation and curation expiry have foreground
  paths ([requests](../../../internal/store/message_request.go#L669),
  [curation](../../../internal/store/memory_curation.go#L1532)), and
  [usage rollups](../../../internal/store/usage.go#L334) update transactionally
  wherever an event is recorded, including
  [worker settlement](../../../internal/store/agent_email_outbound_worker.go#L321),
  so disabling the worker does not make a copy inert.
- Every configured server startup that reaches
  [email-capacity configuration](../../../cmd/witself-server/main.go#L193)
  rewrites the shared singleton; a second cell or an upgrade must not overwrite
  another cell's limits.
- [Migration waiting](../../../internal/store/migrate.go#L93) uses a background
  context with no deadline and the
  [worker runtime](../../../internal/worker/runtime.go#L118) exits when a job
  returns, without restarting it; the lifecycle owner needs bounded, cancellable
  waits and an explicit restart policy.
- Require database/schema and authenticated operation checks for cell readiness.
  Today, in the hosted configuration, `/readyz` succeeds with no database
  configured (unless email receive
  requires one) and otherwise checks only a store ping; `/v1/version` reports the
  compiled schema version, not the attached database's
  ([startup](../../../cmd/witself-server/main.go#L2043),
  [readiness](../../../internal/server/server.go#L7504),
  [version](../../../internal/server/server.go#L2459)). Startup migrations still
  verify schema compatibility; these endpoints do not establish cell identity.
  Prove native startup on every intended OS early; cross-compilation alone does
  not establish Windows runtime support.

**Exit:** repeated install/start/stop and crash restart preserve synthetic data;
parallel lifecycle calls serialize; unrelated processes remain untouched;
starting or restarting a second cell leaves the first cell's active and retained
archives and foreign temporary files intact. Listeners are private; disabled
local support changes no existing install, account endpoint, background service,
or hosted job behavior.

### M2 Make standalone use complete enough for a canary

LC-S3 implements connection routes, atomic route activation, per-event hook adoption and the hosted-only plan/billing refusal; live MCP re-binding is deferred (restart required).
LC-S4 implements the console route trust path with independent operator verification and stale-route refusal; standalone operator bootstrap and offline inventory proof remain.

- Add explicit connection routes without changing named hosted-account resolution by
  default. Local resolution must survive a blocked directory/control-plane
  connection; a local failure must never silently reroute writes remotely.
- Stage and atomically activate connection route/credential bindings, with recovery from
  interruption. Define how running MCP clients and hooks adopt a changed connection route;
  a partial switch must not combine one cell's identity with another's endpoint.
- Bootstrap a standalone account and its operator/agent permissions without
  granting hosted entitlements. Retain identity verification through CLI, MCP,
  hooks, console and TUI. Avoid automatic identity remapping on later enrollment.
- Give account-console management its own authenticated local installation/connection route
  trust path. Adapting ordinary client endpoint resolution is insufficient: the
  console independently verifies the local route or hosted directory before admitting
  operator authority. Preserve origin/account identity verification and distinct
  operator/agent credentials; do not bypass these checks or promote an agent
  token to account-wide access to make offline views work. The directory
  lookup remains in the CLI manager resolver for accounts without a route; it
  also declines account-manager credentials for explicit endpoint/token-file invocations
  ([resolver](../../../cmd/witself/account_console.go#L35),
  [route and directory selection](../../../cmd/witself/account_console.go#L44)); the lower-level
  verifier already accepts a trusted loopback origin via `/v1/whoami`
  ([origin validation](../../../internal/client/account_console.go#L61),
  [manager verification](../../../internal/client/account_console.go#L90)) and is the reuse
  seam. Named CLI/agent connections without a route or explicit endpoint require the
  directory ([CLI](../../../cmd/witself/main.go#L338),
  [agent](../../../cmd/witself/main.go#L390)).
- Exercise transcripts, narrative memories, facts, their references and recall,
  including client-driven curation for the declared local scope. Define local
  lease ownership; sync must not transplant completed/shared authority later.
- Exercise real local messaging: send/listen/read, claim/acknowledge/release and
  request/offer/assignment/result workflows among authorized agents sharing the
  cell. Prove this without SaaS registration, payment or connectivity, including
  contention, retry, expiry and restart. Keep inference and agent execution in
  the client; a durable mailbox does not launch an offline agent.
- Provide local operation and storage status with explicit unsupported hosted
  features. Existing remote-only behavior remains available unchanged.
- Compare retrieval using identical representative records, queries, indexes,
  configuration and compatible application versions. Verify relevant memories
  and provenance as well as latency; same PostgreSQL engine alone is not parity.

**Exit:** multiple local AI clients can capture/read/search through real API/MCP
paths, restart offline, and recover committed data. A blocked reporting or hosted
endpoint does not delay core operations. No copied vault keys or provider
credentials are needed for a canary using synthetic transcripts and memories.
Interrupted connection route activation and concurrently running clients preserve correct
identity/routing and recover to an explicit, usable state.
Local messaging completes with no hosted dependency and no duplicate claim owner.
With the hosted directory blocked, authorized account views and local client
inventory work offline. Agent-only credentials and mismatched origin/account
bindings remain denied; enrollment and return later preserve these boundaries.

### M3 Deliver and update the cell through native installation

Inspected current behavior: embedded migrations require
[no PostgreSQL extension](../../self-hosting.md#L233), and startup checks no
PostgreSQL version floor; [Compose uses 16](../../../compose.yaml#L17) and the
[fleet configuration uses 18](../../../infra/pulumi/cmd/witself-infra/main.go#L348).
M0a sets the floor. The installers verify checksums but not the published
signatures ([Unix](../../../install.sh#L389), [Windows](../../../install.ps1#L520));
the component manifest needs an explicit trust contract.
[`install.sh` rejects the worker](../../../install.sh#L63), and
[Homebrew ships no server or worker](../../../tools/homebrew-formula/main.go#L40).
None of the three existing backup formats is a native desktop restore
([pre-migration dump](../../../scripts/civo-pre-migration-backup.sh#L311),
[scheduled backup](../../../.gitops/charts/apps/files/postgres-backup.sh#L103),
[archive validation](../../../internal/store/import.go#L7073)), so basic local
restore is new work.

A successful self-export of an active account is not importable even into an
empty destination:
[self-export permits an active account](../../../internal/store/export.go#L279),
while [ordinary import](../../../internal/store/import.go#L6624) requires a
suspended or closed manifest and the
[backup purpose](../../../internal/store/import.go#L6618) is accepted only by the
[rollback-only validation path](../../../internal/store/import.go#L7073).
Export [preserves `is_default`](../../../internal/store/export.go#L403) and
import [rejects `is_default=true`](../../../internal/store/import.go#L1637), so using today's
bootstrap account as the local cell's account is also a snapshot/restore
compatibility choice. Vault files require exact 0700/0600 modes, hard-link
publication and directory sync
([vault directory](../../../internal/local/vault.go#L346),
[key publication](../../../internal/local/vault.go#L382)), and the
[Windows journal adapter](../../../internal/local/account_provision_journal_fs_windows.go#L56)
already treats mode bits as insufficient; Windows vault custody needs native
proof. The bullets below are proposed work, not runtime acceptance.

- Prepare the ordinary new-install path to provision and initialize the free
  local cell automatically and verify usable CLI/MCP access without a second
  setup step. Keep activation behind the M9 rollout gate; test explicit
  remote-only installation separately, with no local cell payload or startup.
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

Inspected current behavior: no account-wide change stream, replication journal
or CDC was found. The
[outbound-email outbox](../../../internal/store/agent_email_outbound.go#L388) and
[audit stream](../../../internal/store/events.go#L47) are domain-specific and
cannot be assumed to capture every account change. `account_events` has no row
images; [dashboard preferences](../../../internal/store/dashboard_preferences.go#L129)
write without any event. Memory `change_seq` is an owner-lane counter that
[permanent deletion](../../../internal/store/memory_delete.go#L162) does not advance.
[Transcript retention](../../../internal/store/transcript_retention.go#L611) leaves
no tombstone and frees the conversation
[`external_id`](../../../internal/store/transcript.go#L124) for reuse;
[message retention](../../../internal/store/message_retention.go#L1205) removes
idempotency evidence a receipt horizon would need, and
[account purge](../../../internal/store/account_purge.go#L581) removes historical
usage and audit. [Resource IDs](../../../internal/id/id.go#L15) are random, not
serial; [transcript sequences](../../../internal/store/transcript.go#L239) and
[memory versions](../../../internal/store/memory.go#L863) are integer coordinates
disconnected writers can collide on.

Capture must observe database changes, not API outcomes: a failed request
mutation can still commit lifecycle changes through due-expiry reconciliation
([reconcile](../../../internal/store/message_request.go#L1308),
[commit on failure path](../../../internal/store/message_request.go#L1315)), and
[operator agent deletion](../../../internal/store/agent.go#L242) writes the
[request graph](../../../internal/store/message_request.go#L1383).
Self-export refuses while an unexpired vault enrollment is pending or approved,
a rotation is open or pending keys are orphaned
([enrollment](../../../internal/store/export.go#L302),
[rotation and keys](../../../internal/store/export.go#L326)). The control plane
already keeps a numeric account lifecycle epoch on
[lifecycle claims](../../../infra/cloudflare/control-plane/src/account-lifecycle-state.mjs#L858)
and [projections](../../../infra/cloudflare/control-plane/src/account-lifecycle-state.mjs#L221);
evaluate it as a seam for authority incarnation rather than presuming it
sufficient. The harness below must supply the runtime evidence.

- Build a deterministic harness with two local cells, a remote authority and a
  second remote move/restore destination, plus an ordinary remote-only client.
  Use real stores and synthetic accounts; inject partitions, duplicate/reordered
  delivery, crashes, delayed acknowledgements, expired history and incompatible
  versions without touching production.
- Capture accepted local writes and their outbound operations in one database
  transaction. Commit inbound mutations and their deduplication/receipt state
  atomically. Acknowledge only after durable commit; bound queues and batches.
- Preserve atomic visibility of each compound request operation, including its
  request/candidate/claim links and opening/offer/result messages. Apply together
  or stage invisibly until the operation is complete; transport batch boundaries
  must not expose a partial graph to list/listen/show, checkpoints or claims.
  Current ordinary-message claiming distinguishes protocol notifications through
  these links, so message deduplication and foreign keys alone are insufficient.
  Deliver notifications before their links, reorder fragments, split batches,
  crash between writes, and race an ordinary claim against publication in both
  snapshot and incremental replay. No protocol notification may become ordinary
  actionable work, and retry must publish the compound operation only once.
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
The compound-request fixtures must also prove no partially visible or ordinarily
claimable protocol notification at any interruption point.

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
  from materialized views. Sealed records are copied as ciphertext only in M5;
  key enrollment and recovery are asserted in M7.
- Prove automatic bootstrap in both directions: a populated local cell adopts a
  new hosted destination, and an authorized local replica joins an existing
  hosted account. Include the independently populated local-and-hosted fixture
  below; use the M0 collision policy and retain ongoing local writes.
  This read-only synchronization stage is intermediate evidence; do not expose
  it as the finished paid upgrade before writable continuity in M6/M7 passes.
- Introduce authority-aware, projection-only inspection for imported request
  views. Current request list/show perform foreground reconciliation that can
  persist expiry, claim cancellation and completion; disabling worker jobs does
  not stop those writes. Imported views must not run that reconciliation, while
  standalone local requests retain their ordinary lifecycle behavior.
- Define revoke, disconnect, re-enroll and expired-cursor behavior, including
  which cached data remains available and how stale authorization is presented.

**Exit:** remote writes during bootstrap are present exactly once; interrupted
transfer resumes safely; an unauthorized/shared device cannot enroll; capacity
refusal preserves both sides; mirrored email/messages remain read-only and
produce zero delivery, claim, acknowledgement or billing effects. Old-writer
rollback cannot falsely report caught-up status, and authority move/restore
cannot silently reuse invalid cursors or erase pending work. Snapshot/replay and
new reads have the distinct accounting outcomes established in M4.
These read-only constraints apply to imported views in this stage, not to the
standalone local messaging already accepted in M2.
Exercise ordinary foreground list/show against mixed local/imported requests,
with clock skew, expired-looking leases and delayed sibling updates. Imported
inspection produces no lifecycle mutations or reconciliation audit events;
legitimate local reconciliation and M4's ordinary-read accounting still work.

### M6 Add writable domains with deletion and conflicts together

Advance one family at a time after its semantics and failure cases pass. A
partial writable canary must advertise its coverage; it is not whole-account
writable synchronization.

| Family | First bounded slice | Gate before enabling it |
|---|---|---|
| Transcripts and evidence | Append and read with stable entry/origin IDs | Concurrent appends and remote-only writes preserve ordering semantics, evidence ranges and provenance; retention/replay cannot resurrect removed content |
| Memories and facts | Independent capture, then updates/replacements/relationships | Expected versions, competing assertions, provenance, tombstones and explicit recreation survive retry/reconnect; no silent overwrite |
| Messages and request workflows | Continue the local API while synchronizing authorized messages and outcomes across cells | Stable identities, atomic compound-operation visibility and duplicate suppression; request-wide authority, capacity and selection ordering; fenced handoff, acknowledgements and cancellation across offline/reconnect and paid/local transitions |
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
For request workflows, race local and remote-only selections for the same request
and completion against a sibling renewal. Assert maximum assignees, ordered
selection generations and correct settlement across partitions and authority
handoff, including rejection of late operations under a retired authority epoch.

### M7 Complete connected authority and account transitions

- Keep email receipt/delivery hosted. Implement connected messaging through the
  same local interface used standalone, following M0's authority/ownership
  contract. Validate each claim, acknowledgement and cancellation at its current
  authority; never execute a copied fence or conflate accepted local intent with
  delivery to another machine. Preserve independent local messaging during outages.
  Apply the request-wide authority contract to offers, selection, reservations,
  renewals, expiry and settlement as well as individual claims/acknowledgements.
  Transfers must not leave two cells independently coordinating the same graph.
- Implement the declared local secrets/TOTP coverage and connected key lifecycle.
  Preserve sealed
  associated-data identities; transfer/enroll client keys separately from
  account replication. Test absent keys, rotation/retired epochs, revoked grants,
  offline access policy and recovery without sending plaintext keys to a server.
- Complete seamless standalone-to-paid adoption and return to local use. After
  owner-approved signup/enrollment, automatically bootstrap and catch up the
  hosted account while clients keep using their existing local cell. Preserve
  identities, data, integrations and messaging progress; no manual export/import,
  empty replacement account or per-agent reconfiguration. Test interruptions
  during ongoing reads, writes, sends and claims, including an existing populated
  hosted destination and rollback to local-only operation. Complete every branch
  of the [populated-account transition acceptance](#populated-account-transition-acceptance)
  fixture; an empty hosted account alone cannot satisfy this gate.
- For planned disconnect/downgrade, bring authorized remote changes local before
  declaring transfer complete. Keep local work usable if connectivity or a
  capacity check prevents catch-up, and show pending work rather than discarding
  it. Define remote-only coexistence, expired subscription and closure separately:
  stopping sync does not cancel billing or close an account, hosted email does
  not become local, and stale uploads cannot undo remote closure.
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
Upgrade and return use the same working local environment with automatic data
continuity; local and connected messaging pass claim/retry/partition tests. Email
is the only hosted-only feature, not a label applied to unfinished local domains.
Offline account-console authority and client inventory remain usable before,
during and after the transition, with agent/operator boundaries intact.

#### Populated-account transition acceptance

This is one required end-to-end specification whose steps are asserted in stages:
M5 asserts steps 1–4 for read-only continuity (identity preservation, admission,
interrupted bootstrap, projection-only inspection); M6 adds the writable-domain
branches of steps 3–4; M7 asserts steps 5–6 plus key custody, authority handoff and
return; the supported M9 release requires every step. This is not an implemented
test or permission to use customer accounts.
Use real stores and synthetic identities, clients and service integrations.
Cover native installation/connection route behavior on macOS, Windows and Linux through
the M3 matrix; keep protocol fault cases reproducible in the M4 harness.

1. **Start with two independently populated accounts.** The local cell and an
   existing hosted account each contain realms/agents, transcripts, facts,
   memories with evidence, sealed secrets with their legitimate client-held keys,
   and message history. Include pending sends, completed requests and live request
   reservations/claims within their executable lifetime. Request authority ages
   on database time: requests default to one hour, with a seven-day maximum;
   offer collection lasts at most 15 minutes, and each reservation/claim lease
   grant or renewal lasts at most 15 minutes, bounded by request expiry. Renewal
   retains the same generation
   ([request bounds](../../../internal/store/message_request.go#L44),
   [input bounds](../../../internal/store/message_request.go#L284),
   [renewal](../../../internal/store/message_request.go#L2337),
   [lease bounds](../../../internal/store/message.go#L58)). Keep installed CLI/MCP
   clients, hooks and an authorized console using the local cell, plus a
   remote-only client writing to the hosted
   account. Record expected logical identities, references, permissions and
   message outcomes without recording content or keys in ordinary proof.
2. **Authorize adoption and prove admission.** Exercise the M0 ownership and
   collision contract for joining both populated accounts; do not silently choose
   a side, overwrite a record, remap sealed identities, or replace either account
   with an empty one. Include overlapping names, conflicting records, unauthorized
   enrollment and capacity mismatch. Refusals preserve local operation and both
   data sets, explain the needed resolution and can resume after it; user-reviewed
   conflict resolution is distinct from a manual data-transfer procedure.
3. **Enroll while both sides continue working.** One owner-approved paid enrollment
   initiates automatic snapshot/catch-up. Continue local reads, writes, searches,
   transcript capture, secret access and messaging, with concurrent remote-only
   writes. Preserve installed bindings, local endpoints and authorized access;
   no manual export/import, recreated agents or per-client reconfiguration.
   Required key enrollment/recovery uses client custody, never server-held
   plaintext or an assumption that a data snapshot transfers private keys.
4. **Interrupt and resume the transition.** Partition the network, restart the
   local cell, lose an acknowledgement, replay/reorder fragments and interrupt
   authority handoff. Resume automatically where safe; otherwise expose the
   specific conflict or pending state without discarding work. Verify evidence
   references and authorized secret decryption, once-only logical effects,
   request-wide capacity/ordering, stale-fence rejection and no replay of completed
   work. Continuity preserves request state, history and authority epoch, not an
   executable reservation/claim lease beyond its database-time bound across a
   sleep or partition. Imported request inspection must remain non-mutating where
   M5 applies.
5. **Change paid-service status and return to local use.** Test a lower hosted
   quota, effective subscription cancellation/expiry and deliberate sync disconnect
   as separate cases. Catch up authorized remote changes before marking a planned
   return complete; if unavailable or over capacity, keep local work usable and
   identify outstanding remote data and delivery. Hosted policy changes must not
   disable independent local messaging or destroy local data. Preserve pending
   work and valid request authority without resetting or duplicating claims;
   cross-device work may wait under the declared contract. Sync disconnect alone
   must not cancel billing or close the hosted account. Email remains a hosted
   operation and follows its authoritative availability; it never dispatches
   locally. Exercise closure separately so stale replay cannot reopen it.
6. **Assert continuity and honest completion.** The same local cell and installed
   clients remain usable, including offline account views/client inventory with
   the directory blocked; agent credentials never gain operator access. No
   acknowledged mutation is lost, no completed work is replayed, no conflicting
   record is silently discarded and no partial transfer is labeled caught up.
   Local durability, remote acceptance, convergence and remote delivery have
   separate assertions. Retain value-free before/after identity and reference
   checks, expected event/usage counts, fault seeds and unresolved-state evidence.

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
clients. Require the populated-account transition fixture above, including its
interruption, policy-change, offline-console and return-to-local branches. Record
implementation and rollout separately in the feature catalog
and regenerate its scorecard. After this gate, activate the selected free-local
default for new installations and replace the hosted free plan through a reviewed
transition plan. Define existing-user notice, migration, retention, grace periods
and hosted-feature continuity before changing live policy. These are rollout
decisions, not an open choice about whether local replaces hosted free. Preserve
explicit remote-only installation and existing users' data and bindings throughout.

## First implementation batch

Each slice records `ID / scope / acceptance / dependencies (satisfied?) /
evidence class / activation boundary / recovery limit / owner`. These are proposed
slices, not implementation authorization; do not turn them into one long branch.

- **LC-S0 contracts** / Scope: M0a and M0b text (this review supplies it);
  residual synthetic fixture inventory and expected outcomes are folded into
  LC-S1's first commit / Acceptance: text complete in this review; fixtures
  pending authorization / Dependencies: none / Evidence: static / Activation:
  none / Recovery: documentation only; no runtime state / Owner: planning lead
  for text; vacant for fixtures.
- **LC-S1 inactive local runtime configuration** / Scope: typed configuration
  for private listeners, owned data/temp paths, allowlisted child environment,
  per-job eligibility, authenticated database/schema/account readiness;
  existing defaults byte-identical when not selected / Acceptance: refuses a
  missing database, wrong installation identity, public listeners and disallowed
  jobs; `/readyz` and `/v1/version` under the configuration reflect the attached
  database / Dependencies: LC-S0 text (satisfied by this review) / Evidence: unit
  plus real PostgreSQL; one native macOS run / Activation: nothing changes unless
  explicitly selected / Recovery: remove the configuration; behavior reverts /
  Owner: Codex GPT-6 Astra (implementer); Fable 5.1 (integration owner).
  **Authorized by Scott on 2026-10-23 (decision D1); LC-S1 through LC-S4 merged (#822, #824, #825, #826).** Implemented subset:
  see the M1 note.
- **LC-S2 disposable lifecycle core** / Scope: one owner for process, data and
  temp with an exclusive lock, manifest and interruption journal / Acceptance:
  inspect/install/start/stop/status/repair/remove on synthetic data /
  Dependencies: LC-S1 (merged #822) / Evidence: fixture, real PostgreSQL, native /
  Activation: explicit harness only / Recovery: synthetic data only / Owner:
  vacant.
- **LC-S2-P platform adapters** / Scope: one slice per OS; Windows first because
  server/worker have no Windows release target and no native lifecycle proof yet
  (cross-compilation already passes)
  ([server targets](../../../.goreleaser.yaml#L32),
  [worker targets](../../../.goreleaser.yaml#L71)) / Acceptance: native lifecycle
  proof for the selected OS / Dependencies: LC-S2 and the M0a platform decisions
  (D8 answered 2026-10-24) / Evidence: native / Activation: explicit synthetic harness only /
  Recovery: synthetic data only / Owner: vacant.
  LC-S2-P PW1 implements protected Windows DACLs, private creation, and local NTFS/reparse path boundaries for lifecycle state and the archive spool. Retained handles fence renames; in-place reparse changes are detected after pathname creation with handle-bound rollback, not prevented (handle-relative creation remains deferred). PW2 still owes the launch/environment adapter, and PW3 the native executable fixtures and PostgreSQL lifecycle proof.
- **LC-S3 explicit local connection route** / Scope: CLI/MCP/hooks select a local
  cell with the directory blocked / Acceptance: atomic binding activation;
  fail-closed partial switch / Dependencies: LC-S1, M0b route decision (pending) /
  Evidence: fixture plus real API / Activation: explicitly selected synthetic
  local route only / Recovery: restore the prior complete binding; no customer
  account transition / Owner: vacant.
- **LC-S4 offline account-console trust** / Scope: independent local manager
  binding on the existing verifier seam / Acceptance: agent-only, stale and
  wrong-origin cases denied / Dependencies: LC-S3 (pending) / Evidence: fixture
  plus real API / Activation: explicitly selected synthetic local manager
  binding only / Recovery: remove that binding; no privilege promotion / Owner:
  vacant.
- **LC-S5 sync model and failure-harness skeleton** / Scope: parallel track from
  M0c; operation IDs, capture, incarnation, receipts, accounting, deletion
  knowledge, compound publication; two local stores plus a remote and a move
  target / Acceptance: failure-harness scaffolding is not synchronization /
  Dependencies: M0c decisions (pending) / Evidence: fixture plus real PostgreSQL /
  Activation: synthetic harness only; no account synchronization / Recovery:
  disposable stores only; no user-data recovery claim / Owner: vacant.

Recorded dissent: the LC-4 review lane recommended authorizing LC-S0's fixtures
as a separate first slice before any runtime code. The planning lead folds those
fixtures into LC-S1 because the contract text now exists and a fixture-only slice
would delay the first native evidence; the dissent stands until LC-S1's review.

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

The planning lead should first challenge the runtime boundary, identity/adoption design,
all-writer change capture, deletion retention, worker eligibility and upgrade
recovery. Add source-backed findings and measured alternatives. A simpler design
may replace this proposal if it meets the same requirements; record why and how
existing local data would survive. The local-cell decision remains revisable.

Resolve these before their dependent milestones:

| Decision | Needed before | Reviewer contribution | Owner |
|---|---|---|---|
| Exact standalone/offline operation set and secrets coverage | M0, M2, M7 | Fill the operation matrix; distinguish local curation from shared work | planning lead |
| Supported OS/architecture versions and PostgreSQL packaging/extensions | M1/M3 | Native proof, footprint and distribution/maintenance assessment | founder |
| Startup/service ownership and lifecycle command/module placement | M1/M3 | Compare user/system scope and infra frontend without dependency inflation | planning lead |
| Identity adoption, device trust, at-rest protection and offline access horizon | Durable enrollment in M5 | Threat scenarios and existing account/vault constraints | founder |
| Request-wide authority, handoff and lifecycle ordering | M0 contract; M4/M6/M7 messaging proof | Concurrent selection, sibling renewal/settlement and stale-epoch fixtures; individual claim uniqueness is insufficient | planning lead |
| Compound request visibility and projection-only imported inspection | M4 publication; M5 read replicas | Reordered notification/link delivery, interrupted apply, mixed local/imported list/show and no imported reconciliation writes | planning lead |
| Offline account-console authority | M2; transition proof in M7 | Directory-blocked account views/inventory, identity/origin checks and distinct agent/operator permissions | planning lead |
| Populated-local plus populated-hosted continuity | M5 bootstrap; complete M7/M9 acceptance | Identity/key/collision admission, ongoing writes, interruption, subscription changes and return using the same cell and clients | planning lead |
| Capture activation and rollback barrier across server/worker versions | M4 proof; any M5 snapshot/enrollment | Mixed-version writes, verified capture or drain, explicit recovery after coverage loss | planning lead |
| Remote authority incarnation, cutover and restore continuity | M4 proof; M5 enrollment | Lost-ACK relocation, portable receipt/deletion state, stale endpoints and older-backup recovery limits | planning lead |
| Logical usage/audit identity, replay and derived-state accounting | M4; before M5/M6 activation | Expected counts for user actions, snapshot, retry and replica application; separate pricing policy | planning lead |
| Self-managed and SaaS admission, authorization and feature entitlements | M5 enrollment; M7 feature activation | Authoritative policy fixtures for both destinations; no assumed SaaS billing rule for self-managed | founder |
| Conflict rules, stable transcript evidence and deletion/history horizon | M4/M6 | Executable multi-writer and stale-device counterexamples | planning lead |
| Reporting fields, linkage, disclosure, controls and receiver ownership | Enabling M8 | Payload/resource contract, operator aggregate and retention proof, product/privacy review | founder |
| Resource/retrieval budgets, canary duration and expansion thresholds | Enrolling the relevant M9 cohort | Named fixtures, baselines and pass/fail values | founder |
| Activation timing and existing free-user transition | Product transition after M9 | Implement the selected free-local default and hosted-free replacement; define notice, migration, retention and continuity without silently switching existing bindings | founder |
| PostgreSQL version floor and distribution per OS | M0a before M3 | Candidate distributions per OS, footprint and update cadence; no extension is required today | planning lead |
| Standalone local policy snapshot | M0b before M2 | Explicit messaging, vault and capacity policy for the free local cell; never uploaded as entitlement | planning lead |

For each milestone, append a record using `ID / owner / state / PR or decision /
evidence / unresolved gate / next action`. Use proposed, in progress, blocked, or
complete deliberately; a merged disabled slice is not an enabled capability.
Update the feature scorecard through its generator when implementation progresses,
and keep this plan focused on execution order and decisions rather than a second
competing readiness ledger.

### Living progress record

Record shape: `ID / owner / state / PR or decision / evidence / unresolved gate /
next action`. States: proposed, in progress, blocked, complete. A merged but
disabled slice is not an enabled capability. Edit this section only from the
branch named in the active review record below, or after that review hands off.

**PLAN-REVIEW-2026-10-23** / Fable 5.1 (planning lead); Codex GPT-6 Astra at
`ultra` for five read-only lanes / complete for this pass / Decisions: adopt the
M0a/M0b/M0c split and the dependency changes above; adopt connection route and local
runtime configuration terminology; stage the populated-account acceptance across
M5/M6/M7; LC-S1 through LC-S4 are merged; with D2–D8 answered on 2026-10-24, LC-S2-P (Windows adapters first) and LC-S5 are the next slices; the local-cell architecture stays the
provisional baseline (lane verdict "revise-with-conditions": native PostgreSQL
first, embedded-service challenger retained, remote-cache-only fails the fixed
requirements, container cell adds a runtime) with measurable budgets required
before ratification / Evidence: source revision
`88fce5a365c2e1a21d088173c82018f3146edd1d`; 130 references re-verified (0 contradicted,
18 anchors corrected); five lane reports and an adversarial re-check of 18 key
findings (12 confirmed, 4 qualified, 2 refuted and dropped) retained outside the
repository under the handoff reports directory; Codex model and effort verified
from session metadata / Unresolved gates: founder decisions listed in the decision
table; no runtime evidence yet (static review only) / Next action: Scott decides
whether to authorize LC-S1; the implementing agent claims ownership in this record
before starting.

| ID | Owner | State | PR or decision | Evidence | Unresolved gate | Next action |
|---|---|---|---|---|---|---|
| M0 | vacant | in progress | text split into M0a–M0c in this review | static review only | founder rows of the decision table | fixtures with LC-S1 |
| M1 | vacant | proposed | none | none | LC-S1 authorization | LC-S1 |
| M2 | vacant | proposed | none | none | identity and offline-console contract | after M1 foundation |
| M3 | vacant | proposed | none | none | platform, packaging and PostgreSQL distribution decisions | Windows runtime proof first; native lifecycle proof on all three OSes |
| M4 | vacant | proposed | none | none | capture, authority-continuity and accounting contracts | harness specification after M0 sync subset |
| M5 | vacant | proposed | staged assertions (steps 1–4) | none | M4 barrier proof; destination admission policy | blocked on M4 |
| M6 | vacant | proposed | none | none | deletion and conflict rules | blocked on M5 |
| M7 | vacant | proposed | steps 5–6, keys, handoff, return | none | request-wide authority and key-custody contracts | blocked on M5/M6 |
| M8 | vacant | proposed | none | none | reporting disclosure and controls (founder) | UI states may start early; collection stays off |
| M9 | vacant | proposed | none | none | all exit gates; transition plan for existing free users (founder) | last |
| LC-S0 | planning lead; fixtures vacant | in progress | text complete | static | fixtures | LC-S1 |
| LC-S1 | Codex GPT-6 Astra (implementer); Fable 5.1 (integration owner) | merged (disabled) | PR #822 | unit, real PostgreSQL, nine killed mutations, native macOS readiness/version | no live activation | LC-S2 |
| LC-S2 | Codex GPT-6 Astra (implementer); Fable 5.1 (integration owner) | merged (disabled) | PR #824 | unit, mutation, synthetic lifecycle, native real PostgreSQL 4/4 with no shared-memory leak, three executable reviews (19 killed mutations) | no live activation | LC-S3 |
| LC-S2-P | Codex GPT-6 Astra (implementer); Fable 5.1 (integration owner) | in progress | PW1 Windows private filesystem | none | LC-S2; M0a | Windows proof first |
| LC-S3 | Codex GPT-6 Astra (implementer); Fable 5.1 (integration owner) | merged (disabled) | PR #825 | route/CLI/MCP/hook tests; executable review (9 killed mutations) plus a default-account fix pass | no live activation | LC-S4 |
| LC-S4 | Codex GPT-6 Astra (implementer); Fable 5.1 (integration owner) | merged (disabled) | PR #826 | route/console and real API tests; executable review (10 killed mutations) | no live activation | LC-S2-P (D8), LC-S5 (M0c) |
| LC-S5 | vacant | proposed | sync harness | none | M0c | settle contracts |

## Founder decisions (answered)

Scott answered D1 on 2026-10-23 and D2–D8 on 2026-10-24, each taking the
planning lead's recommendation. These are the standing answers; a later change
is recorded here with its date.

- **D1 — Authorize LC-S1:** YES (2026-10-23). LC-S1 through LC-S4 are merged.
- **D2 — Identity adoption, namespace join and collisions:** adoption preserves
  the account namespace; joins require user-reviewed, per-item resolution; never
  silent remapping (2026-10-24).
- **D3 — Device trust, shared machines and offline access horizon:**
  owner-authorized personal replicas first; stale state is explicit; no promise
  of remote erasure (2026-10-24).
- **D4 — At-rest protection baseline:** an explicit threat model precedes
  whole-account enrollment; vault keys remain client-held (2026-10-24).
- **D5 — Usage reporting fields, linkage, retention and controls:** coarse
  bounded aggregates and a narrow build signal, bounded retention, and reporting
  failure never gates local work (2026-10-24).
- **D6 — Existing free-hosted user transition:** deferred until after M9
  (2026-10-24).
- **D7 — Destination commercial policy:** no SaaS subscription prerequisite for
  local messaging or self-managed enrollment (2026-10-24).
- **D8 — Supported OS versions/architectures and resource budgets:** macOS 14+
  on arm64 and x86_64, Windows 11 x86_64, and Linux glibc x86_64 and arm64, with
  the Windows proof first; resource budgets are set per platform by LC-S2-P
  (2026-10-24).
