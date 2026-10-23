# Local cell assessment and engineering direction

[WitSelf running locally research index](README.md)

Status: active research, 2026-10-17. Scott has selected the **local cell as the
product direction**, explicitly subject to adversarial review and revision.
Native processes versus containers, component packaging and the synchronization
protocol remain open. This decision does not authorize implementation or change
current production behavior. Source baseline: `adc6f186`.

The 2026-10-23 follow-up review checked messaging/request state machines and
account-console trust at `5380baec`. The additional contracts below remain
implementation requirements; they do not establish implemented local support.

## Recommendation and decision

Start the local-cell design with the existing WitSelf application, PostgreSQL and
appropriate worker jobs, installed and updated as one product. This preserves
the current domain implementation and search semantics while concentrating new
work on synchronization and the local installation lifecycle. Evaluate native
processes as the first packaging baseline. Scott explicitly wants minimal host
dependencies and to avoid Kubernetes or containers if possible. A managed
container or Kubernetes profile would need evidence that its benefits justify
those dependencies. Users should not administer the runtime themselves.

Scott selected the local-cell direction after discussing the alternatives. This
supersedes the earlier request to keep every architectural direction undecided;
it does not select a particular desktop orchestration stack. SQLite and other
embedded storage remain counterproposals if evidence overturns the reuse case.

The largest new workstream is reliable synchronization with explicit authority
rules. Cross-platform installation and safe upgrades are another substantial
workstream. Using the same database reduces behavioral divergence; it does not
make independently writable copies safe or establish a delivery estimate.

See the [product discussion](local-first-mode.md) and
[storage and competitor comparison](local-storage-and-sync-patterns.md).

## Product contract

| Requirement | Meaning for the design |
|---|---|
| Free local offering | The default new installation provides a usable free local cell that replaces the hosted free plan. This product direction is selected; activation timing and existing-user migration remain rollout work. |
| Minimal host dependencies | A native cell is the first baseline; avoid requiring Kubernetes or containers if possible. PostgreSQL remains a separate process, not an in-process library. |
| Integrated installation | Ordinary new installation automatically provisions and initializes a usable local cell with the software, without a separate local-mode selection or deployment step. |
| Three operating systems | macOS, Windows and Linux are in scope for the first supported local release. Supported versions and architectures still need definition. |
| Lightweight remote client | An explicit installation connects directly to SaaS or a self-managed service without local database, cell payload or service registration. |
| Required continuous sync | Whole-account scope by default, with explicit authority exceptions; preserve local work during outages and reconcile on return. Standalone use need not enroll with a remote account. |
| Seamless paid/local transition | Keep the same local cell, identities, data and client workflows; automate enrollment and sync without a reset, manual export/import or per-agent reconfiguration. Returning to local use preserves continuity. |
| Agent messaging | Required local messaging and coordination, preserved through paid enrollment; connected routing and claims need explicit coordination rather than copied leases. |
| Hosted-only feature exception | Email receipt/delivery remains hosted because of infrastructure overhead. Local history may be read/searched; billing and provider routing still require service authority. |
| Capacity preservation | Explain destination limits and retain local data and pending work; never silently trim an account to fit. |
| Usage reporting | Disclosed, non-intrusive reporting of local utilization and usage, including a carefully defined modification signal; reporting failure never blocks local operation. |
| Clear product visibility | Console and TUI explain relevant hosted capabilities and why they are unavailable locally, without banners, repeated upgrade prompts or interruption. |
| Future decentralization | Distributed cells on personal machines are a future possibility. Preserve useful seams without making peer-to-peer storage a first-release promise. |
| Incremental delivery | Ship inactive components and opt-in canaries while preserving current operations; evolve through explicit readiness gates to a complete supported product. |

“Local” describes where ordinary WitSelf operations run. It is not a claim of
zero outbound traffic: usage reporting is a separate requirement. An external
AI client may also contact its own model provider. Registration, reporting
identity and reporting controls still need explicit product decisions; core
local work must continue when the WitSelf service cannot be reached.

## What the code lets us reuse

The store directly contains a PostgreSQL pool, and domain methods depend on
PostgreSQL behavior rather than an engine-neutral interface. Recall uses text
search, JSONB filters and consistent snapshots; facts and secrets use transaction
locks; memory changes carry owner sequences and version checks. Preserving
PostgreSQL avoids a parallel storage-conformance project. Matching retrieval
still requires compatible data, versions and configuration, plus relevance and
latency measurements. Sources: [store](../../../internal/store/store.go#L16),
[recall](../../../internal/store/memory_recall.go#L149),
[fact mutations](../../../internal/store/fact.go#L156),
[secret mutation locks](../../../internal/store/secret_mutation.go#L656).

Server startup already opens PostgreSQL, applies migrations and bootstraps
account state. Local authentication can use its database records. The explicit
endpoint/token client path avoids the hosted directory, whereas named managed
accounts normally resolve that directory afresh. These provide real reuse seams,
but a durable offline connection profile is still needed. Sources:
[startup](../../../cmd/witself-server/main.go#L181),
[authentication](../../../internal/store/auth.go#L222),
[connection selection](../../../cmd/witself/main.go#L314).

Account-console access needs its own offline trust path. Today's manager
resolver refuses explicit endpoint/token profiles and verifies a named account's
origin against the hosted directory before reading its separate operator bearer.
Merely pointing an agent client at loopback therefore does not enable the local
account console. Define a local installation/profile binding that authenticates
the selected cell and verifies the exact account, operator and current role
without directory access. Keep agent credentials separate from operator authority,
reject mismatched or stale bindings, and preserve the existing remote trust path.
Prove local console bootstrap and restart with the directory blocked; hosted
billing actions still require their service authority. Sources:
[manager resolution](../../../cmd/witself/account_console.go#L35),
[operator verification](../../../internal/client/account_console.go#L87),
[bound-principal revalidation](../../../internal/client/account_console.go#L115).

Secrets use client-held keys and identity-bound ciphertext. A local API/database
can serve the backend reads involved in reveal, while encryption and decryption
remain in the client. Copying the database alone does not install keys. Preserve
immutable account/realm/agent identities across enrollment and synchronization.
Sources: [reveal](../../../internal/secretclient/secretclient.go#L916),
[associated data](../../../internal/sealed/aad.go#L34).

The existing default bootstrap account has lifecycle restrictions, including a
closure guard. Turning a standalone account into a managed account therefore
needs an explicit identity/adoption design, not an assumption that today's cell
move applies unchanged. [Default-account guard](../../../internal/store/close.go#L15).

## Infrastructure CLI as a lifecycle foundation

The infra CLI is a serious reuse candidate. It already provisions self-hosted
cloud cells as well as fleet cells, and contains safety patterns worth retaining.
It does not currently install an isolated desktop cell. In particular,
`-backend local` selects local **Pulumi state storage**; its resource providers
remain AWS, GCP, Azure and Civo. Sources:
[infra purpose and module boundary](../../../infra/pulumi/README.md#L1),
[providers](../../../infra/pulumi/cmd/witself-infra/main.go#L49),
[local state](../../../infra/pulumi/cmd/witself-infra/main.go#L740).

| Existing tooling | Reusable idea | Adaptation for local operation |
|---|---|---|
| Infra CLI and inventory | Named managed installations, explicit lifecycle and status | Local process/service driver, no cloud credentials or fleet registration prerequisite |
| Protected deployment preview | Detect destructive replacement before applying changes | Protect local data directories and identify reversible versus irreversible update steps |
| Generated release pins and rollout checks | Verified, compatible component identities | Local release manifest and component compatibility; no GitHub PR or Argo dependency for each consumer update |
| Health and convergence checks | Check the actual resulting system | Verify private endpoints, database/schema and authenticated operations on this computer |
| Backup and restore rehearsal | Prove recoverability before risky migration | Local transport and storage; alternative backup destinations are secondary research |

Relevant implementations: [protected preview](../../../infra/pulumi/cmd/witself-infra/deletion_plan.go#L28),
[cloud readiness](../../../infra/pulumi/cmd/witself-infra/health.go#L103),
[application roll](../../../scripts/roll-cell.sh#L1),
[backup rehearsal](../../../scripts/civo-pre-migration-backup.sh#L1).

My recommendation is one small, platform-neutral lifecycle implementation, with
native installer adapters and potentially an infra CLI frontend. A versioned
companion executable is one packaging option; a shared module is another.
Command names and module placement should follow the dependency boundary. The
current infra module deliberately isolates the large Pulumi/provider dependency
tree, and its Homebrew formula requires Pulumi. Do not pull that tree into every
remote-only client. An `infra local` surface is worth evaluating if it can use
the same local lifecycle without requiring cloud provisioning tools. Sources:
[dependency separation](../../../infra/pulumi/README.md#L9),
[formula products](../../../tools/homebrew-formula/main.go#L40),
[release boundary](../../../.goreleaser.yaml#L105).

## Native installation and updates

Package managers should deliver the verified components; WitSelf should own one
consistent cell lifecycle. This is the proposed ownership model, not existing
installer behavior. It avoids implementing different migration logic in a shell
script, a Homebrew formula and a Windows installer.

| Platform or path | Candidate delivery | Cell lifecycle responsibility |
|---|---|---|
| macOS | Homebrew; retain the verified shell installer for users without it | Private paths, local PostgreSQL initialization and launchd integration; define login versus boot behavior |
| Windows | Signed MSI or EXE installer, optionally distributed through WinGet; preserve a small remote-only package | Native cell artifacts, PostgreSQL distribution, service/user startup ownership, ACLs, upgrade and uninstall behavior |
| Linux | Verified shell installer initially; evaluate Debian and RPM packages for native package management | Integrate with systemd where available; explicitly define other supported environments and foreground operation |

Homebrew supports launchd/systemd service registration, and distinguishes
login/boot registration from running a service once. It does not supply WitSelf
data migrations. [Homebrew services](https://docs.brew.sh/Manpage#services-subcommand),
[formula service definitions](https://docs.brew.sh/Formula-Cookbook#service-files).
WinGet distributes several installer formats, including MSI and EXE; the installer
still needs to implement the product's setup and lifecycle.
[Windows Package Manager](https://learn.microsoft.com/en-us/windows/package-manager/winget/).
Debian maintainer scripts support installation/upgrade actions and require
idempotency; interrupted or retried configuration must be safe.
[Debian package lifecycle](https://www.debian.org/doc/debian-policy/ch-maintainerscripts.html).

Today the CLI has Windows x64 release coverage, while server, worker and infra
releases cover macOS/Linux. This is a distribution/acceptance gap, not proof they
cannot run on Windows. The Homebrew templates install binaries and aliases;
the shell installer protects binary replacement, not a whole-cell data upgrade.
Sources: [release targets](../../../.goreleaser.yaml#L10),
[formula](../../../packaging/homebrew/formula.rb.tmpl#L14),
[installer transaction](../../../install.sh#L611).

The lifecycle needs installation identity, one service owner, private endpoints,
compatible component versions, data outside disposable package directories,
writer coordination, migration/recovery sequencing and restart verification.
Existing listeners default to all interfaces, and server/worker health and
metrics ports collide when launched unchanged on one host. A success response
from the present readiness endpoint alone is insufficient: no-database startup
is possible. Sources: [server listeners](../../../internal/server/server.go#L2322),
[worker listeners](../../../cmd/witself-worker/main.go#L472),
[no-database startup](../../../cmd/witself-server/main.go#L2042).

Goose migrations, schema-ahead guards and migration locking are useful. They do
not implement a PostgreSQL major upgrade, nor make binary rollback a data
rollback. Account data and unsynchronized work must survive update interruption
and ordinary software removal; destructive data removal should be a separate,
explicit operation. [Migration gate](../../../internal/store/migrate.go#L37).

## Synchronization and authority are the largest new work

The hard part is deciding what concurrent operations mean, not simply moving
database rows. The same protocol must include remote-only writers and both SaaS
and self-managed destinations.

| Domain | Local role | Coordination requirement |
|---|---|---|
| Memories, facts and transcripts | Ordinary local reads/writes and client-driven curation | Durable operation identity, provenance, conflicts, deletion propagation and consistent replay |
| Agent messages and requests | Full standalone local messaging; same local interface with connected sync/transport | Preserve message/request identity and completed/pending work; fence each delivery and the complete request graph, preserve compound-operation visibility, and distinguish pending cross-device delivery |
| Hosted email | Read/search authorized synchronized history; retain outgoing intent as pending | Receipt/delivery and provider side effects remain hosted; synchronized records cannot trigger dispatch |
| Plans, billing and provider routing | Display known status | A modified local database cannot grant hosted entitlement or external authority |
| Secret ciphertext | Local storage with separately held client keys | Enrollment, recovery, key epochs and rotation need explicit shared rules |
| Full-account replicas | Owner-authorized device copy | Agent permission alone cannot authorize full-account download; offline revocation is not instantaneous |

Concrete source constraints show why generic replication is insufficient:

1. Transcript entry sequences are allocated under a database lock, and memory
   evidence refers to those sequences. Disconnected allocation can collide;
   renumbering must not invalidate provenance. [Transcript allocation](../../../internal/store/transcript.go#L235),
   [evidence references](../../../internal/store/migrations/0029_add_narrative_memories.sql#L234).
2. Permanent deletion scrubs values and retains retry protection. A stale device
   must not recreate deleted content through ordinary replay. Device retirement,
   rebootstrap and intentional recreation need distinct rules.
   [Fact deletion](../../../internal/store/fact_delete.go#L91),
   [memory deletion](../../../internal/store/memory_delete.go#L79).
3. Mailbox and curation leases derive authority from one database. Copying a
   claim does not permit two devices to complete it. Requests additionally need
   authority over the whole graph: open, offer, decline, select/reserve, claim,
   renew, release, complete, cancel, expiry and settlement. Selection counts
   completed and live sibling reservations against `MaxAssignees` and advances
   one request-wide generation; completion examines siblings before closing the
   request. Per-claim ownership alone cannot preserve those invariants. Define a
   single fenced request authority or prove an equivalent coordination protocol,
   including across ownership transfer. Local email dispatch must not replay a
   remote side effect. Sources: [message claims](../../../internal/store/message.go#L504),
   [selection capacity](../../../internal/store/message_request.go#L1885),
   [selection generation](../../../internal/store/message_request.go#L1953),
   [request settlement](../../../internal/store/message_request.go#L1094),
   [curation apply](../../../internal/store/memory_curation_apply.go#L95),
   [email dispatch fencing](../../../internal/store/agent_email_outbound_worker.go#L253).
4. Opening a request publishes its message, request and candidate rows in one
   transaction. Ordinary message processing identifies request notifications
   through their linked graph rows, not their message kind. Sync must atomically
   publish each compound operation or stage visibility until all required links
   are present. Otherwise an opening, offer or result arriving ahead of its graph
   can become ordinary claimable work. Preserve this boundary when splitting
   batches, resuming snapshots and replaying after crashes. Sources:
   [opening transaction](../../../internal/store/message_request.go#L548),
   [protocol-message guard](../../../internal/store/message.go#L1873),
   [atomic result publication](../../../internal/store/message_request.go#L2466).
5. Request inspection is not currently a pure read. List/show reconcile lifecycle
   state, including persisted expiry, claim cancellation and terminal completion,
   using database time and visible siblings. An imported projection needs a read
   path that cannot perform those mutations; disabling workers or collaboration
   mutations is insufficient. Route reconciliation only to the current request
   authority while independent standalone requests remain operational. Sources:
   [list reconciliation](../../../internal/store/message_request.go#L669),
   [show reconciliation](../../../internal/store/message_request.go#L767),
   [expiry](../../../internal/store/message_request.go#L1027),
   [settlement](../../../internal/store/message_request.go#L1094).

Acceptance must exercise local and remote-only coordinators selecting concurrently,
completion racing a sibling renewal, notifications arriving before their graph,
and list/show over mixed standalone and imported requests with clock skew and
delayed updates. Imported inspection must produce no lifecycle or lifecycle-audit
writes. These are protocol gates, not additional hosted-only feature exclusions.

Export/import offers snapshot and validation building blocks. Existing import
does not merge into a live existing account, and active claims are invalidated.
Continuous sync needs a coherent initial snapshot tied to incremental history,
durable pending operations, resumable cursors and idempotent receipts.
[Export](../../../internal/store/export.go#L159),
[import](../../../internal/store/import.go#L6678),
[claim normalization](../../backup-and-recovery.md#L521).

These are internal implementation seams, not steps the customer should perform.
Paid enrollment must automatically attach the populated local cell and catch up
in the background while existing clients keep using it. Preserve agent identities,
messaging history, pending operations and work claims across enrollment and return
to local use; test interrupted transfer and collisions with an existing hosted
account. Do not present a partially synchronized account as fully ready.
Use the implementation plan's canonical
[populated-account transition acceptance](implementation-plan.md#populated-account-transition-acceptance)
scenario: both sides already contain data, secrets, bindings and messaging work;
clients continue writing through enrollment, interruption, paid-plan changes,
cancellation and return to local use. The shared scenario owns the detailed
assertions so milestone tests do not substitute an empty-account demonstration.

## Existing synchronization technology worth evaluating

The transport and replication mechanics need not all be invented in-house.
These are candidates, not verified fits for WitSelf or selected dependencies.
Platform, licensing, self-hosting and operational constraints need review.

| Candidate | Useful mechanism | Remaining WitSelf work |
|---|---|---|
| PostgreSQL logical replication | PostgreSQL change publication/subscription | Multi-writer domain rules, account/device trust, offline history retention and upgrades; schema DDL and sequence state are not automatically replicated |
| PowerSync | Local SQLite materialization and queued mutations uploaded through an application API | Not a direct PostgreSQL-to-PostgreSQL local-cell implementation; our API still supplies authorization, idempotency and conflict policy |
| Electric | PostgreSQL read-path synchronization into local applications | Current Electric does not implement the reverse write path; offline mutations and reconciliation need another component |
| Automerge | Mergeable document data and repository network/storage adapters | Useful selectively; not a replacement for account permissions, leases, vault epochs or remote delivery authority |

Primary references: [PostgreSQL restrictions](https://www.postgresql.org/docs/18/logical-replication-restrictions.html),
[PowerSync conflicts and upload queue](https://docs.powersync.com/handling-writes/handling-update-conflicts),
[Electric write-path boundary](https://electric.ax/docs/sync/guides/writes),
[Automerge repositories](https://automerge.org/docs/reference/repositories/).
The [competitor comparison](local-storage-and-sync-patterns.md#competitor-and-adjacent-product-observations)
provides additional product patterns, not proof that a whole-account sync engine
can be adopted unchanged.

## Usage reporting and quiet feature discovery

Scott requires a disclosed phone-home mechanism for local utilization, usage
and modifications. It must be best-effort: failure, blocked networking, timeout
or unavailable service cannot gate startup, reads/writes, search or continued
free use. Reporting is separate from account synchronization and remote commands.

Proposed boundaries are aggregate feature counters, coarse storage/utilization
bands, version/platform/install channel and an explicit official/custom-build
signal. “Modifications” needs a precise definition before selecting fields;
it does not imply uploading source diffs, user configuration or changed records.
Do not send transcript/memory contents, secret values, prompts, filenames or
other content-bearing labels as usage telemetry. Identity should be scoped and
disclosed; installation counts are not verified counts of people, and an offline
or modified client makes complete reporting impossible.

Use bounded local buffering, short asynchronous timeouts, capped retries with
backoff, and a disk/CPU/network budget. Dropping expired telemetry is preferable
to exhausting resources or blocking the product. Privacy disclosure should cover
fields, purpose, identifier/account linkage, retention and controls; consent and
opt-out policy remain decisions to resolve before implementation. This research
does not edit the privacy policy or enable collection.

The console/TUI should display useful local statistics and relevant unavailable
capabilities in context. Proposed examples: “Hosted email — requires a hosted
service” or “Cloud account usage — available with a paid SaaS account.” Distinguish
plan restriction from offline, not configured and unsupported states. Exact
SaaS versus self-managed entitlement is still a product decision. Use an optional
details/enable action; avoid promotional banners, repeated prompts, artificial
urgency or replacing useful local data with upgrade messages.

## Self-managed hosting and future distributed cells

The infra CLI already covers self-hosted cloud provisioning. A native local
cell could also provide a foundation for non-cloud, bare-metal installations. A
private local cell and a remotely reachable self-managed cell can share application behavior
and lifecycle code, but exposing one involves network reachability, TLS, identity,
access controls, availability and recovery ownership. It is not merely changing
a loopback address. Keep native local, self-managed remote and SaaS responsibilities
explicit, with the lightweight client able to connect to either remote service.

Future decentralized storage across personal cells is a useful architectural
aspiration. Stable identities, origin-aware operations, transport-independent
sync and per-domain authority would preserve options. Peer discovery, topology,
trust, availability, partition handling and deletion across peers are additional
research. Do not require them for the first local-cell delivery or weaken the
hosted-email authority or messaging claim safety in anticipation of a possible
future design. Local messaging is required now; peer-to-peer federation remains
a separate future topology choice.

## Effort and evidence needed next

| Workstream | Relative assessment | Evidence that would reduce uncertainty |
|---|---|---|
| Synchronization, identity and authority | Largest new correctness/design effort | Two writable replicas plus remote-only writer; crash/retry/reordering, conflicts, evidence identity, deletion, key rotation, quota refusal and long disconnection |
| Native lifecycle and upgrades | Substantial, especially Windows and database upgrades | Clean-machine install, restart/sleep, interrupted updates, recovery, disk-full and uninstall/data preservation on all three OSes |
| Routing, profiles and product transition | Significant integration work | Offline local resolution, no cell in remote-only mode, explicit adoption of populated accounts and no silent destination changes |
| Retrieval and resource behavior | Cross-cutting acceptance work | Same realistic questions/data; relevant memories and provenance, latency, idle CPU/RAM, battery and disk footprint |
| Reporting and feature discovery | Bounded compared with sync, but requires a clear contract | Payload inspection, blocked-network tests, bounded queue/resource tests and useful non-promotional UI states |

Existing migration serialization, worker shutdown and domain tests are useful
starting contracts. CLI artifact smoke tests do not establish whole-cell desktop
acceptance. Source review supports these relative work boundaries, not a credible
delivery date or measured infrastructure saving. No services, installs or runtime
benchmarks were performed for this assessment. Sources:
[migration tests](../../../internal/store/migrate_integration_test.go#L1733),
[worker tests](../../../internal/worker/runtime_test.go#L271),
[CLI smoke scope](../../../.github/workflows/ci.yml#L513).

## Incremental development and deployment

Scott expects fast iteration, dark deployment and canary testing while the
existing product continues to work. This should become a complete supported
local offering, not remain research indefinitely. The sequence below is a
delivery proposal for that expectation; this documentation turn does not install
software on anyone's machine or start implementation/deployment.

Separate **shipping code**, **installing a cell**, **activating local routing**,
**enrolling synchronization** and **changing the default experience**. Each has
different consequences and should have its own versioned capability/activation
boundary. Disabled code must preserve the current path. In particular, ordinary
upgrades must not change account identity, endpoint, plan, storage location,
background service behavior or send local content to a service. The remote-only
package remains independent of the cell payload.

| Stage | Deliverable and activation | Evidence to advance |
|---|---|---|
| 1. Inactive foundations | Add lifecycle interfaces, capability discovery and protocol/version definitions behind disabled boundaries | Existing CLI/MCP, hosted routing, account operations and lightweight installation continue through unchanged behavior; no hidden service or reporting starts |
| 2. Isolated local canary | Explicitly opt in on Scott's machine or other enrolled test machines; separate cell identity, data paths, credentials and ports | Clean install, authenticated local operations, offline restart, stop/start, upgrade interruption and safe removal with synthetic or intentionally selected test data |
| 3. Read-only sync projection | Enroll a test account and populate an isolated replica; use retained synthetic fixtures for retrieval comparisons | Complete authorized snapshot/catch-up, resumable transfer, capacity reporting and no claim, dispatch or other side effect from mirrored records |
| 4. Bounded writable sync | Enable named operation families for an opt-in test cohort, then broaden coverage | Multiple writers, retries, dropped acknowledgements, deletion, evidence references, key lifecycle and remote authority remain correct; pending local work stays durable |
| 5. Supported local offering | Broaden installer/platform coverage and complete the promised operation matrix | All three OSes meet agreed install/recovery/resource requirements; disclosure and UI states are ready; support and upgrade contracts are usable |
| 6. Default and existing-user transition | Activate the selected free-local default for new installations and replace the hosted free plan through a reviewed transition | Tested migration/adoption, notice and continuity for existing free users, compatible old/new clients and explicit remote-only support; no silent destination change or data loss |

These stages overlap where independent work is safe; they need not become one
large branch or a final integration event. Keep slices small and releasable under
the repository's standing validation/release policy. Installer work, sync
invariants and UI capability states can advance in parallel. Define a concrete
exit criterion and next owner for each slice, and keep a completion checklist for
all required features so a narrow successful canary is not mistaken for launch.

Canary installation must be a deliberate enrollment. Merely shipping disabled
code does not authorize downloading a full account, provisioning a local cell,
starting processes or turning on telemetry on existing installations. Reporting
may activate only with its defined disclosure/control contract. A kill switch
must be locally usable even when the control plane is unavailable, and must not
disable basic local access simply because a reporting endpoint fails.

Shadow evaluation may compare authorized read results in an isolated test copy.
Do not replay production writes, copied leases, provider email delivery, message
acknowledgements, account closure or billing actions as shadow traffic. The
email service stays authoritative for email effects; messaging uses the declared
standalone or connected coordination authority without replaying copied claims.
Production customer data is not the default test fixture.

Use additive/version-compatible schema and protocol changes where possible,
with old/new client coexistence tests. Feature flags cannot undo data migrations.
Before enabling writable sync, define how to pause uploads while preserving
local work, drain or retain pending operations and recover after interruption.
Do not switch back to a remote account by discarding unsynchronized writes.
Binary downgrade, disabling a feature and restoring data are different recovery
actions with different limits.

Readiness should be based on agreed functional and resource criteria: existing
behavior, domain correctness, installation reliability, recovery and relevant
search results. Use value-free observations of failures, queue age and resource
use to expand or halt a cohort. Set schedules after the first lifecycle and sync
proofs establish actual effort; urgency alone does not justify a confident ETA.

## Adversarial review and grounds to revise the decision

Claude and future reviewers should try to disprove the local-cell recommendation,
not treat the decision as a constraint against better evidence. Reopen it if:

- A supported OS cannot provide reliable installation, upgrades, private access
  and recovery within an acceptable maintenance burden.
- Measured idle resource use, disk footprint or battery impact defeats the
  intended free local experience, and a smaller design can preserve the contracts.
- Reusing PostgreSQL still requires enough divergent domain behavior that the
  expected maintenance benefit disappears.
- A demonstrated alternative meets retrieval, offline durability, identity,
  deletion and synchronization requirements with substantially less complexity.
- The chosen topology cannot preserve remote service authority or the required
  lightweight client without unacceptable coupling.

A challenge should identify the contradicted assumption, cite code or measured
evidence, compare alternatives against the same requirements, and propose an
explicit revision with migration implications. Record accepted revisions rather
than silently changing the design. The local-cell decision is **revisable**, not
irreversible; later reviewers are explicitly invited to recommend a change.
