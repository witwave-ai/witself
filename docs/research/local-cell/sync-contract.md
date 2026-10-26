# Local cell synchronization contract

Status: draft contract record for M0c (planning lead defaults; DECIDED rules are testable; OPEN items carry an owner and the milestone they block)

**CURRENT CODE** — Source revision: `7fd2f8321b05410a04cdf990a527ce86dc2426f9`.
The source-line links below describe this checkout, not runtime proof.

**DECIDED — SC-0.1.** This record supplies reviewable synchronization contracts
for [M0c](implementation-plan.md#m0c-synchronization-contract), LC-S5 fixtures and
M4 experiments. It authorizes no implementation or durable customer data.
LC-S5 remains proposed; review of this draft does not itself satisfy its gate.

**DECIDED — SC-0.2.** Honor
[D1, D2, D3, D4, D5, D6, D7 and D8](implementation-plan.md#founder-decisions-answered).
Each OPEN item requires its named owner's resolution and planning lead review;
runtime acceptance still requires the named milestone's evidence.

## Terms

**DECIDED — SC-0.3.** Use the plan's [terms](implementation-plan.md#terms).
The following additional definitions apply to the rules and fixtures here:

- **Receipt horizon:** the minimum interval during which a durable outcome and
  its deduplication evidence remain available after settlement or retirement.
- **Capture epoch:** a monotonically advancing, account-scoped activation
  generation binding writer coverage, schema capability and snapshot watermark.
- **Request authority epoch:** a fencing generation for the entire request graph;
  it is distinct from capture epoch and account lifecycle epoch.
- **Provisional mutation:** locally durable work awaiting destination acceptance;
  it is neither convergence nor permission to execute an external side effect.
- **Deletion knowledge:** value-free evidence that prevents a deleted identity
  or generation from being revived by an older operation or snapshot.

## 1. Connected operation matrix

### Contract

**DECIDED — SC-1.1.** The following rows are the connected contract. Each row is
a numbered rule; every operation in a family inherits its row's authorization,
conflict, retention and accounting constraints. Cached reads require the current
offline grant. Provisional writes enter the journal and remain visible as pending
until receipted. A named unsupported effect is refused, not silently queued.
Retention includes the receipt floor in SC-4.7 and deletion floor in SC-9.2;
neither floor preserves deleted payloads. Expected versions and dependencies
are scoped to the account incarnation. No row orders writes by wall clock.

| DECIDED rule / API family | Connected offline support | Write authority | Permissions | Conflicts and causal basis | Deletion | Retention | Required online coordination | Action accounting |
|---|---|---|---|---|---|---|---|---|
| SC-1.2 Accounts, operators and agents | Cached identity; provisional profile edits; privilege grants and account closure unsupported offline | Destination owns account status, operators and grants; authorized owners edit profiles | Operator for account administration; agent for own permitted profile | Expected account, operator or agent version; competing edits conflict; no privilege union | Local access can tighten immediately; shared deletion and closure require authoritative ordering | Identity history plus receipt and deletion floors; credential material excluded from replication | Enrollment, grants, ownership, closure and shared revocation | Nonbilling activity |
| SC-1.3 Realms | Cached reads; provisional create and edit | Authorized account operator at destination acceptance | Account-scoped realm management; agent membership checked separately | Expected realm version; explicit creation dependency on account; duplicate names conflict | Tombstone realm and dependent references atomically; no stale recreation | Realm metadata policy plus receipt and deletion floors | Admission and collision resolution; permission changes require destination | Nonbilling activity |
| SC-1.4 Facts, subjects, candidates and aliases | Authorized reads and provisional mutations | Owning agent or explicit delegate; destination validates each operation | Account, realm, owner and sensitive-value permission | Expected fact/assertion, candidate-set and subject/alias versions; competing resolution conflicts; alias collisions need review | Explicit authorized delete; recreation names the deletion generation and reviewed intent | Payload retention separate from tombstones and receipts; candidate evidence retains causal references | Shared acceptance, collision resolution and changed permissions | Nonbilling activity |
| SC-1.5 Memories, capture, curation, vectors and deletion | Capture/read/search; provisional owner-lane curation; shared curation lease acquisition unsupported offline | Owner for local lane; current fenced authority for shared curation | Agent ownership and access profile; curator role does not grant account download | Expected memory version and curation fence; creation dependencies name evidence; vectors depend on exact content version | Delete suppresses stale versions and derived vectors; no replay resurrection | Version/evidence policy plus receipt and deletion floors; vectors rebuild from accepted content | Shared curation claims and acceptance; expired fences require reacquisition | Nonbilling activity; derived index work is sync diagnostics |
| SC-1.6 Transcripts | Capture, read and search; provisional append/redaction | Owning agent; destination validates immutable entry identity | Account/realm/owner; evidence access checked independently | Explicit dependency set of conversation generation, predecessor and immutable entry IDs; divergent appends retain IDs and require stable projection order | Redaction/deletion knowledge beats stale payload; external name reuse creates a new generation | Content retention never erases the receipt or deletion floor | Acceptance and evidence mapping; incompatible generations require resolution | Nonbilling activity |
| SC-1.7 Messages and requests, complete request graph | History and pending sends; independent local graphs execute; remote-owned graph mutations unsupported offline | One graph authority and epoch; ordinary messages have one delivery authority | Sender/recipient plus request role and exact claim fence | Expected graph revision plus authority epoch; all sibling reservations and linked notifications share ordering | Cancellation/deletion ordered by graph authority; completion cannot replay | History policy plus receipt and deletion floors; leases retain their execution bounds | Cross-device claims, renewals, offers, selection, settlement and authority transfer at graph authority | Nonbilling activity |
| SC-1.8 Agent email, inbound and outbound | Authorized local history/search and drafts; provider receipt, routing and dispatch unsupported offline | Hosted email service for delivery and provider outcomes | Authorized mailbox view; hosted send/receive entitlement at service | Expected message/outbox version and provider-attempt dependency; competing dispatch conflicts | History deletion does not retract provider delivery; delivery receipts remain value-free | Mail content policy separate from receipts and service delivery evidence | Every provider receipt/dispatch, route change and capacity admission | Delivery/send work is billable usage; history reads and drafts are nonbilling activity |
| SC-1.9 Secrets and vault | Authorized ciphertext access and client decryption with held keys; provisional sealed edits; new shared key grants unsupported offline | Client holds keys; destination validates ciphertext/grant versions | Agent, realm, secret-field and key-epoch access; replica enrollment alone grants no decryption | Expected secret version and vault epoch; dependencies bind sealed identity; concurrent rotation conflicts | Revoke grants and tombstone ciphertext; unreachable copies cannot be erased remotely | Ciphertext policy plus receipt and deletion floors; plaintext keys never enter sync storage | Key enrollment, rotation settlement and shared grant changes | Nonbilling activity |
| SC-1.10 Avatars | Read active version; provisional proposals or permitted local evolution | Agent or operator according to authoritative autonomy policy | Exact profile revision, style version and permitted layer changes | Expected profile revision and parent version; competing proposals conflict | Reset retires lineage; deletion requires separate authorization and deletion knowledge | Immutable version policy plus receipt and deletion floors | Destination activation if required by policy; style migration and shared settlement | Nonbilling activity |
| SC-1.11 Dashboard preferences and consent | Local display edits and consent withdrawal; provisional shared preferences | Agent for own preferences; operator for account consent; destination for shared policy | Authenticated principal and own scope; agent token cannot promote to operator | Expected preference or consent revision; conflicts shown; restrictive local consent takes immediate effect | Delete preference payload; retain minimal consent/deletion evidence | Preference policy plus receipt and deletion floors; reporting retention separately bounded | Shared preference settlement and any consent expansion | Nonbilling activity |
| SC-1.12 Usage and billing | Local utilization counters and cached status; paid-plan changes and charges unsupported offline | Destination owns billing ledger and plan; client owns bounded local diagnostics | Billing administrator/service for paid changes; scoped aggregate read | Logical operation ID plus expected ledger/policy revision; duplicates return original outcome | Account deletion follows ledger policy; replay cannot recreate erased payload | Usage policy plus receipt/deletion floors; derived rollups never extend payload retention | Paid-plan authority, charging and authoritative usage settlement | Billable usage for service consumption; local reads/reporting are nonbilling activity |
| SC-1.13 Backups, exports and imports | Authorized local backup/export; local restore suspends sync; connected import/cutover unsupported offline | Owner for local recovery; destination for account import and incarnation cutover | Whole-account export permission distinct from agent access | Expected incarnation and snapshot watermark plus manifest dependencies; nonempty destination collision conflicts | Purge/restore must preserve or invalidate deletion knowledge explicitly | Archive policy plus retained receipt/deletion evidence; old archives cannot assert current authority | Connected restore, import, evacuation, bootstrap and cursor continuity | Nonbilling activity; any future charge requires separate pricing decision |

**DECIDED — SC-1.14.** Unlisted API operations default to `unsupported` until an
explicit row extension is reviewed. Unsupported connected effects do not make
the corresponding standalone non-email feature SaaS-only. LC-S5 enumerates API
operations against these rows; unknown operations cannot inherit permission
from an empty feature setting.

### Current code

**CURRENT CODE** — Memory updates compare an expected version before advancing
it ([version check](../../../internal/store/memory.go#L863)). Transcript append
allocates sequence numbers under a database row lock
([allocation](../../../internal/store/transcript.go#L239)); that is not a
disconnected multi-writer identity scheme.

**CURRENT CODE** — Fact deletion stores a tombstone and conflict basis
([tombstone](../../../internal/store/fact_delete.go#L190)). These domain controls
are reuse seams, not evidence that the connected matrix is implemented.

### Open items

| ID | item | owner | blocks |
|---|---|---|---|
| OPEN O-1 | Enumerate every API operation, including maintenance writes, against the matrix and unsupported outcomes. | LC-S5 | LC-S5 |
| OPEN O-2 | Fix stable transcript entry/evidence projection and per-family causal encodings without renumbering existing references. | planning lead | M4 |

### Harness obligations

- **DECIDED (SC-1.1–SC-1.14):** LC-S5 supplies one permission, offline,
  conflict, deletion, retention and unsupported-operation fixture per row.
- **DECIDED (SC-1.4–SC-1.6):** M4 races two local writers and a remote-only
  writer, then replays deleted generations and checks stable evidence references.

## 2. Identity transitions and seamless continuity

### Contract

**DECIDED — SC-2.1.** Adoption, namespace join and device enrollment are three
distinct transitions. Adoption preserves the existing account namespace.
Namespace join requires user-reviewed resolution for every colliding identity,
ownership edge and sealed reference; unresolved items block that join. Device
enrollment grants a new replica access to an existing namespace without merging
another namespace. None permits silent remapping.

**DECIDED — SC-2.2.** Apply the
[populated-account transition acceptance](implementation-plan.md#populated-account-transition-acceptance)
fixture to all three transitions. Keep independently populated local and remote
accounts in the fixture even where a particular transition does not merge them.
For adoption, an unrelated populated namespace is preserved and collision is
refused. For enrollment, populated replica state is verified or staged for
explicit resolution, never overwritten. A namespace join exercises both sides.

**DECIDED — SC-2.3.** Preserve logical identities, local endpoints, installed
bindings, hooks and authorized console access through upgrade and return.
One owner authorization starts automatic bootstrap and catch-up; ordinary
clients require no manual export/import or per-agent reconfiguration.
Journal ongoing reads that require audit and every accepted local mutation
during transfer; distinguish a pending local mutation from remote acceptance.

**DECIDED — SC-2.4.** Capacity or identity refusal preserves both data sets and
pending operations. Interrupted bootstrap resumes from a verified watermark or
enters explicit rebootstrap. It never replaces a populated account with an empty
one, trims data to fit, or reports a partial copy as synchronized.

**DECIDED — SC-2.5.** Transition state includes message history, pending sends,
completed requests, current reservations, claims and authority epochs.
Restoring history cannot revive a completed action or extend a lease. Planned
return catches up authorized remote changes before declaring completion;
unavailable changes remain visible as pending while independent local work
continues. Disconnect, subscription expiry and account closure are separate
events; disconnect neither cancels billing nor reopens a closed account.

### Current code

**CURRENT CODE** — Ordinary import requires a suspended or closed manifest
([status check](../../../internal/store/import.go#L6624)); backup archives use a
different validation path ([purpose check](../../../internal/store/import.go#L6618)).
These checks are not live namespace-join or enrollment semantics.

**CURRENT CODE** — Self-export refuses unexpired pending/approved vault enrollment and
open rotation or orphaned pending keys
([enrollment](../../../internal/store/export.go#L302),
[rotation](../../../internal/store/export.go#L307),
[refusal](../../../internal/store/export.go#L323)). A snapshot cannot be treated
as automatic transfer of key authority.

### Open items

| ID | item | owner | blocks |
|---|---|---|---|
| OPEN O-3 | Define resumable per-item join review and sealed-identity conversion or refusal with client-held keys. | planning lead | M5 |
| OPEN O-4 | Specify transition checkpoints and expected identity, capacity and message outcomes for all three fixture branches. | LC-S5 | M5 |

### Harness obligations

- **DECIDED (SC-2.1–SC-2.4):** LC-S5 models all three transitions; M4 injects
  interruption and lost acknowledgement during ongoing writes. M5 proves
  identity/admission and read-only continuity; M6 adds writable branches.
- **DECIDED (SC-2.3, SC-2.5):** M7 proves upgrade, return, key custody and
  messaging continuity; M9 requires every staged acceptance branch to pass.

## 3. Connected request-graph authority

### Contract

**DECIDED — SC-3.1.** The authority unit is the complete request graph:
request, candidates, offers/declines, selection generations, sibling reservations,
claim/renew/release, completion, cancellation, expiry and opening/offer/result
messages. One authority orders all mutations at one graph revision and fencing
epoch. Per-claim ownership alone cannot authorize request settlement.

**DECIDED — SC-3.2.** Transfer first fences the old graph authority, settles or
records its last accepted revision, and durably installs a strictly newer epoch
with that complete graph at the successor. The successor publishes authority
only after the predecessor fence is proven. An unproven predecessor fence leaves
transfer pending; elapsed disconnection is not evidence of ownership transfer.
Old-epoch claims, renewals, releases, offers, selections and results are rejected.

**DECIDED — SC-3.3.** During a partition, operations on a graph whose authority
cannot be reached wait: offers/declines, reservations, selection, claims,
renewals, release, completion, cancellation and authoritative expiry settlement.
Local intent may be captured but confers no executable lease. Independent
local-authority messaging and graphs continue without SaaS or cross-device
transport. A copied remote graph never becomes an independent local graph.

**DECIDED — SC-3.4.** Each compound request operation publishes its graph links
and protocol messages atomically, in snapshot and incremental application.
Incomplete fragments remain staged and invisible to list/listen/show,
checkpoints and ordinary claims. Protocol notifications never become ordinary
actionable messages, including before their graph links arrive.

**DECIDED — SC-3.5.** Imported request inspection is projection-only. It cannot
reconcile expiry, select offers, emit messages, claim work or settle usage.
Expiry uses the authority's lease rules; logical conflict ordering uses graph
revision and epoch, never wall-clock timestamps. Return to local use preserves
the fence and completed outcomes rather than resetting claims.

### Current code

**CURRENT CODE** — Ordinary claiming checks protocol ownership through request
links ([ownership check](../../../internal/store/message.go#L1873)). Request
creation writes graph and notification state in its store operation
([creation](../../../internal/store/message_request.go#L548)).

**CURRENT CODE** — Request list reconciles lifecycle state
([inspection](../../../internal/store/message_request.go#L669)). A mutation
failure path can commit due-expiry reconciliation
([reconcile](../../../internal/store/message_request.go#L1308),
[failure commit](../../../internal/store/message_request.go#L1315)); operator
agent deletion also changes requests
([deletion call](../../../internal/store/agent.go#L242),
[graph reconciliation](../../../internal/store/message_request.go#L1383)).

### Open items

| ID | item | owner | blocks |
|---|---|---|---|
| OPEN O-5 | Choose the durable predecessor-fence and successor-publication proof, including failed transfer recovery. | planning lead | M4 |
| OPEN O-6 | Define inert request projections and adversarial compound-publication fixtures for every request lifecycle operation. | LC-S5 | M4 |

### Harness obligations

- **DECIDED (SC-3.1–SC-3.3):** M4 races sibling claims and transfer, partitions
  the predecessor, and rejects every stale-epoch mutation without dual owners.
- **DECIDED (SC-3.4, SC-3.5):** M4 delivers notifications before links, splits
  batches, crashes between writes and races ordinary claims against publication.
  Snapshot and incremental retries publish once; inspection produces no writes.

## 4. Sync envelope, states and receipts

### Contract

**DECIDED — SC-4.1.** Every operation carries origin installation identity plus
device or replica identity; a client-assigned random 128-bit operation ID;
account incarnation; capture epoch; operation kind; per-domain causal basis;
schema and protocol versions; and authorization context containing agent,
access profile and realm. Operator/service operations also carry their
authenticated principal kind; an absent agent is explicit, never impersonated.
Authorization context is evidence to validate, not permission by assertion.

**DECIDED — SC-4.2.** Operation IDs are idempotent per account across batches,
reconnects and incarnation reconciliation. A duplicate with identical semantic
input returns the original outcome. Reuse with different input returns conflict
and cannot replace the original receipt. Retain a canonical payload commitment
without retaining sensitive payload solely for deduplication. IDs and expected
versions/dependency sets from the matrix determine identity and causality;
database sequence allocation and timestamps cannot decide concurrent winners.
For `operation_id_reuse`, retain a separate value-free refusal keyed by the
conflicting payload commitment alongside the unchanged canonical receipt.
Repeating that conflicting attempt returns the same refusal.

**DECIDED — SC-4.3.** A durable receipt contains operation ID, original origin,
incarnation, accepted causal result or refusal basis, status and exactly one
reason code from the following closed sets. A new code requires a protocol
revision; unknown codes are incompatible, never interpreted as acceptance.

| DECIDED SC-4.3 status | Closed reason-code list |
|---|---|
| `accepted` | `applied` |
| `rejected` | `invalid_envelope`, `unauthorized`, `authorization_expired`, `unsupported_operation`, `schema_incompatible`, `protocol_incompatible`, `capacity_refused`, `account_closed` |
| `conflict` | `expected_version_mismatch`, `dependency_unavailable`, `identity_collision`, `deletion_conflict`, `authority_epoch_stale`, `incarnation_mismatch`, `operation_id_reuse` |

**DECIDED — SC-4.4.** Outbound state is `captured` → `published` → `accepted`,
`rejected` or `conflict`. Capture commits the local mutation and journal row
together. Published means durably recorded for transport, not delivered or
accepted. Retry of captured/published work retains operation ID and semantic
input; a lost acknowledgement cannot cause a new logical action. Terminal
outcomes are immutable. User resolution creates a new operation with an explicit
dependency on the conflicted or rejected one.

**DECIDED — SC-4.5.** Inbound state is `received` → `staged` → `applied` or
`refused`. Received data is inert. Staging validates authorization, dependencies
and complete compound boundaries. Apply commits mutation, deduplication and
receipt state in one database transaction. Refusal commits its deduplication
and terminal outcome without the requested mutation. Acknowledge only after
durable commit. `applied` maps to accepted; `refused` maps to rejected or conflict.
Temporary missing fragments remain staged; a dependency proven unavailable
becomes conflict. Transport failure alone is not a domain refusal.

**DECIDED — SC-4.6.** A crash before capture commit leaves neither accepted local
mutation nor operation. A crash after capture resumes captured/published work.
A crash before inbound commit replays from staged; a crash after commit but
before ACK returns the stored outcome without reapplying. Applied inbound work
cannot emit a fresh logical operation and loop back as a new user mutation.
Queue limits refuse new admission explicitly while preserving committed work.

**DECIDED — SC-4.7.** The receipt horizon is 37 days, revisable only through a
reviewed contract change. Keep value-free receipts and deduplication evidence
through data retirement plus that horizon, and at least 37 days after terminal
settlement, whichever is later. For long-lived retained data, extend receipt
retention with data retirement. Unacknowledged or unreceipted journal operations
cannot be collected before the horizon and remain pinned afterward until a
durable terminal outcome or explicit recovery disposition is known. Elapsed
time alone never discards unresolved work. Horizon expiry forces reconciliation
or rebootstrap when proof is missing, not a fresh-ID replay.

### Current code

**CURRENT CODE** — Message retention removes send-idempotency evidence
([retention](../../../internal/store/message_retention.go#L1205),
[key storage](../../../internal/store/message.go#L1006)). Transcript
retention deletes conversation data without writing a tombstone
([retention](../../../internal/store/transcript_retention.go#L611)); creation
can reuse its external name after deletion
([name uniqueness](../../../internal/store/transcript.go#L144)). Existing resource IDs
are random ([ID generator](../../../internal/id/id.go#L15)), not the proposed
128-bit operation envelope.

### Open items

| ID | item | owner | blocks |
|---|---|---|---|
| OPEN O-7 | Fix canonical envelope/receipt encoding, payload commitment, durable refusal and authenticated receipt verification. | planning lead | M4 |
| OPEN O-8 | Set bounded queue, fragment and dependency-wait budgets with explicit refusal/recovery fixtures. | LC-S5 | M4 |

### Harness obligations

- **DECIDED (SC-4.1–SC-4.3):** LC-S5 rejects missing fields, unknown reasons,
  forged authorization, cross-account replay and changed-input ID reuse.
- **DECIDED (SC-4.4–SC-4.7):** M4 crashes at every state boundary, loses ACKs,
  duplicates/reorders batches and advances retention beyond payload retirement.
  Check durable refusal, once-only apply, pinned unresolved work and no loops.

## 5. Capture activation barrier

### Contract

**DECIDED — SC-5.1.** Capture activation is per account, recorded durably in a
schema-versioned activation record carrying capture epoch, minimum capable
writer schema, coverage state and snapshot watermark. Snapshot activation
requires verified coverage of every server, worker and maintenance write path,
including remote-only clients through their server and writes on failed requests.

**DECIDED (contested: see report) — SC-5.2.** Writers below the capture-capable
schema version are fenced by the existing startup migration compatibility
preflight and target check. This is a required integration rule, not a claim
that the present startup check fences already-running writers. Activation must
also prove drain or capture of every pre-barrier transaction before publishing
the first snapshot. A process inventory alone does not satisfy that proof.

**DECIDED — SC-5.3.** A transaction begun before activation must either finish
before the snapshot boundary with its effects included, or enter capture with
its committed changes. Capture records database changes, not only successful
API responses. Mutation and journal commit share a transaction. A maintenance
writer without established coverage blocks activation.

**DECIDED — SC-5.4.** Coverage loss, unsupported rollback or incompatible writer
admission suspends sync readiness explicitly. Preserve pending local work and
receipts, fence unsafe synchronization, and require verified catch-up or a new
capture epoch and rebootstrap before readiness resumes. Ordinary database ping
or compiled schema reporting cannot certify this barrier.

### Current code

**CURRENT CODE** — Startup performs migration preflight before applying schema
changes ([preflight](../../../internal/store/migrate.go#L113)), verifies the
exact compiled target ([target](../../../internal/store/migrate.go#L127)), and
rejects a database newer than the binary
([schema-ahead check](../../../internal/store/migrate.go#L207)). The existing
compatibility-release rule is specific to fact deletion
([compatibility rule](../../../internal/store/migrate.go#L231)); it is not an
account capture record or an in-flight transaction barrier.

**CURRENT CODE** — Dashboard preference writes intentionally have no audit event
or revision fence ([preferences](../../../internal/store/dashboard_preferences.go#L129)).
The audit insertion stores verb and metadata
([event insertion](../../../internal/store/events.go#L1075)), while outbound
email uses its own outbox
([outbox](../../../internal/store/agent_email_outbound.go#L682)). These paths
cannot substitute for verified account-wide change capture.

### Open items

| ID | item | owner | blocks |
|---|---|---|---|
| OPEN O-9 | Select capture mechanism, activation schema/version and the database proof that drains or captures transactions spanning the barrier. | planning lead | M4 |
| OPEN O-10 | Inventory and instrument mixed server, worker, foreground reconciliation and maintenance writers with rollback coverage cases. | LC-S5 | M4 |

### Harness obligations

- **DECIDED (SC-5.1–SC-5.3):** M4 mixes old/new server and worker binaries,
  starts transactions before activation, and commits them after activation and
  snapshot creation. Assert complete capture or explicit refusal/drain; the
  current startup check alone is not a passing result.
- **DECIDED (SC-5.3, SC-5.4):** M4 includes failed request mutations, preference
  writes, maintenance and rollback; coverage loss must suspend readiness and
  preserve pending work before any further convergence claim.

## 6. Authority incarnation, cutover and older restore

### Contract

**DECIDED — SC-6.1.** Authority incarnation is the tuple of account identity,
lifecycle epoch and restore marker. Every cursor and receipt carries it.
Authentication binds cursor scope, issuing authority, replica, watermark and
incarnation. Matching account identity alone cannot authenticate continuation.
An incarnation mismatch enters explicit rebootstrap/reconciliation, never a
silent continuation or destructive replacement of pending local work.

**DECIDED — SC-6.2.** Evaluate the control-plane lifecycle epoch as the cutover
seam. It cannot alone stand for restore continuity. Each restore creates a new
restore marker before synchronization can resume, including rollback to an
older backup and restoration outside that control plane. Self-managed
destinations must provide equivalent authenticated incarnation semantics.

**DECIDED — SC-6.3.** Cutover carries the journal, immutable acceptance receipts,
deduplication commitments, deletion knowledge, capture coverage/watermarks and
device enrollment/revocation records. It carries request graph fences and
authority-transfer evidence. Endpoint sessions and old cursors are invalidated;
they are replaced only after authenticated continuity checks. Export/import,
evacuation, backup/restore and purge must enumerate each item explicitly.

**DECIDED — SC-6.4.** A committed operation with a lost ACK keeps its original
identity through move or restore. The new authority reconciles surviving
receipts and journal evidence before accepting retry. An old endpoint cannot
mint valid acceptance after fencing. A repeated snapshot is not evidence that
a missing acceptance receipt never existed.

**DECIDED — SC-6.5.** An older remote backup must be detected before convergence
is declared. Recovery is bounded by surviving authenticated receipts, journal,
deletion knowledge and authorized replicas within their declared horizons.
Recover only effects supported by that evidence; surface ambiguous acceptance
and unrecoverable loss explicitly. An unrecoverable gap blocks a convergence
claim and automatic re-execution, including when every surviving copy is stale.

### Current code

**CURRENT CODE** — The control plane increments an account lifecycle epoch on
claim ([claim epoch](../../../infra/cloudflare/control-plane/src/account-lifecycle-state.mjs#L858))
and includes epoch in route projections
([projection](../../../infra/cloudflare/control-plane/src/account-lifecycle-state.mjs#L221)).
Those fields are a seam, not a restore marker or authenticated sync cursor.

**CURRENT CODE** — Import has an evacuation receipt lookup
([receipt](../../../internal/store/account_import_lease.go#L41)). Evacuation
finalization deletes portable account rows through a registry
([portable purge](../../../internal/store/finalize_evacuation.go#L189)); account
purge invokes that path ([purge](../../../internal/store/account_purge.go#L581)).
These database-local mechanisms need the proposed synchronization state added
to their movement or invalidation contract.

### Open items

| ID | item | owner | blocks |
|---|---|---|---|
| OPEN O-11 | Add restore marker semantics and journal, receipt, deletion-knowledge and device-record movement to the lifecycle-epoch seam. | planning lead | M4 |
| OPEN O-12 | Define cursor/receipt authentication and older-backup recovery limits when only partial surviving evidence remains. | planning lead | M4 |

### Harness obligations

- **DECIDED (SC-6.1–SC-6.4):** M4 moves authority with a lost ACK, stale
  endpoint, pending local writes and deletion knowledge; reject forged cursors
  and check exact movement or invalidation of every synchronization record.
- **DECIDED (SC-6.2, SC-6.5):** M4 restores older backups with complete,
  partial and absent surviving evidence. Recover only supported effects; the
  absent-evidence case must remain explicitly nonconverged.

## 7. Accounting, audit and reporting

### Contract

**DECIDED — SC-7.1.** Logical action identity is the operation ID, independent
of transport batch, retry count and destination. Record each required user
action once at its authoritative acceptance boundary. A new user read is a new
action; fetching a snapshot or projection for replay is not that read again.
Preserve the original actor and action provenance when importing history.

**DECIDED — SC-7.2.** Replayed operations are recorded with `origin = replay`
and never counted as new user actions. This accounting origin is separate from
the immutable installation/replica origin in the envelope. Replica application
may record actual sync work but cannot impersonate a second user action.
Duplicates with lost ACKs retain the original accounting result.

**DECIDED — SC-7.3.** The matrix classifies every family's actions as nonbilling
activity or billable usage. Billable usage is an accounting classification,
not a price or authorization to charge. Separate local utilization, hosted
service work and sync diagnostics. Derived rollups, vectors and rebuilt indexes
are never re-counted as source actions. Suppressing every event is not a valid
replacement for preserving required logical audit and usage records.

**DECIDED — SC-7.4.** Replay and snapshot fixtures assert both stored events and
hourly/daily aggregates. Batch split/regroup, replica rebuild and retry must
produce the same logical counts; actual sync diagnostics may differ with work
performed. Rebuilding a rollup from accepted history replaces derived state
without adding a second usage event.

**DECIDED — SC-7.5.** Reporting contains only disclosed coarse, bounded
aggregates and a narrow build signal. It excludes account content, secrets and
per-operation payloads. Reporting failure, a full reporting queue or unavailable
receiver never gates local work. Cap queued aggregates and expire them under
the reviewed reporting policy; reporting backlog is not the durable sync journal.
Collection remains disabled until field, control and retention review closes.

### Current code

**CURRENT CODE** — Usage event insertion deduplicates by account/idempotency key
and updates hourly/daily rollups in the caller transaction
([insertion](../../../internal/store/usage.go#L323),
[rollups](../../../internal/store/usage.go#L334)). Outbound email settlement
records usage before its commit
([worker settlement](../../../internal/store/agent_email_outbound_worker.go#L321)).

**CURRENT CODE** — Observation context suppresses nonbilling activity only
([observation](../../../internal/activity/context.go#L25)). It does not establish
replication-wide replay accounting or suppress legacy usage.

### Open items

| ID | item | owner | blocks |
|---|---|---|---|
| OPEN O-13 | Define expected per-action events/rollups and replay provenance for every matrix row, including user reads and reconciliation. | LC-S5 | M4 |
| OPEN O-14 | Specify reporting fields, build signal, disclosure/control, queue/retention bounds and receiver ownership. | founder | M8 |
| OPEN O-15 | Resolve pricing for affected hosted usage before feature activation without changing replay semantics. | founder | M7 |

### Harness obligations

- **DECIDED (SC-7.1–SC-7.4):** M4 compares original action, snapshot, replay,
  duplicate, lost ACK and rebuild counts; split/regroup transport batches and
  distinguish passive synchronization reads from fresh user reads.
- **DECIDED (SC-7.5):** M8 blocks the reporting receiver and fills its queue;
  local work remains usable and payload/storage stay within reviewed bounds.

## 8. Destination enrollment and entitlement policy

### Contract

**DECIDED — SC-8.1.** Local messaging and self-managed enrollment require no
SaaS subscription. A connected destination validates its own authoritative
policy, admission and permissions. The M0b standalone snapshot is never
accepted as connected entitlement, paid-plan authority or provider authority.
The paid-enrollment branch of the transition fixture describes its SaaS case;
the same continuity assertions apply without SaaS payment to self-managed use.

**DECIDED — SC-8.2.** The following intentional differences are testable policy.
An unsupported hosted feature is explicit; missing configuration cannot grant
it. Messaging availability does not imply any email or credential entitlement.

| DECIDED SC-8.2 mode | Messaging | Hosted email | Provider credentials | Vault capacity source | Paid-plan authority |
|---|---|---|---|---|---|
| Local standalone | Enabled under explicit M0b local policy | unsupported; local history remains readable | Provider dispatch credentials unsupported in replica state | Explicit M0b standalone policy, never missing-limit inference | unsupported |
| Self-managed destination | Destination-authorized; no SaaS subscription prerequisite | Only an explicitly operated hosted email service; otherwise unsupported | Destination operator/service custody; copying through sync unsupported | Authoritative self-managed destination policy | Destination policy only; asserting a SaaS paid plan unsupported |
| SaaS destination | Destination policy for connected service; independent local messaging continues | Hosted entitlement, routing and capacity required | Hosted service custody; copying through sync unsupported | Authoritative SaaS destination policy | Authoritative SaaS billing/control plane only |

**DECIDED — SC-8.3.** Admission checks account ownership, enrolled replica,
protocol/schema capability, capacity and permitted features before snapshot
activation. An agent credential or modified local plan row cannot authorize a
whole-account download. Refusal preserves local use and pending work; a partial
feature negotiation must not silently reduce whole-account replication scope.

### Current code

**CURRENT CODE** — Messaging reads the applied account policy snapshot
([gate](../../../internal/store/message.go#L266),
[interpretation](../../../internal/store/message.go#L308)); unmanaged accounts
have a permissive legacy branch
([default](../../../internal/store/message.go#L286)). Missing secret capacity
means unlimited ([limit](../../../internal/store/secret_limit.go#L75)). Neither
default supplies the explicit standalone or connected policy contract.

### Open items

| ID | item | owner | blocks |
|---|---|---|---|
| OPEN O-16 | Bind M0b's explicit local policy and both destination admission snapshots to authenticated policy versions and revocation. | planning lead | M5 |
| OPEN O-17 | Confirm self-managed hosted-email eligibility and capacity policy without imposing SaaS payment on enrollment. | founder | M5 |

### Harness obligations

- **DECIDED (SC-8.1–SC-8.3):** LC-S5 supplies all three policy rows. M5 rejects
  forged local paid plans, unauthorized account downloads and over-capacity
  admission, while local messaging and self-managed enrollment need no SaaS plan.
- **DECIDED (SC-8.2, SC-8.3):** M7 tests entitlement loss and independent local
  continuity; copied email routes or provider state never authorize dispatch.

## 9. Device trust, horizons, curation and at-rest protection

### Contract

**DECIDED — SC-9.1.** Enroll owner-authorized personal replicas first. Replica
host trust is distinct from every local agent's access profile. Stale data and
pending writes are explicit. No contract promises remote erasure of an
unreachable copy; an offline grant does not prove immediate knowledge of
remote revocation. Shared-machine enrollment waits for its reviewed threat model.

**DECIDED — SC-9.2.** The default offline authorization lifetime is 30 days,
renewable while connected after authoritative permission validation. Retain
deletion knowledge for the longest offline authorization lifetime plus 7 days:
37 days with this default, measured from deletion. Review these defaults at M5;
extending the grant lifetime requires extending deletion retention before the
longer grant is issued. Horizon timing is expiry policy, never conflict ordering.
Renewal and cursor continuation also prove catch-up through retained deletion
knowledge. Permission renewal alone cannot extend a stalled replica's cursor.
If that proof is unavailable, fence the old cursor and rebootstrap before
accepting pending operations; preserve those operations for reconciliation.

**DECIDED — SC-9.3.** After grant expiry, synchronized protected reads and shared
mutations require renewal or explicit recovery; locally owned standalone work
remains separate. Clock rollback or uncertain expiry cannot extend a grant.
Reconnection revalidates queued writes against current permissions and deletion
knowledge. A retired device re-enrolls with a new replica identity and never
resumes an old cursor, even if its local database survives unchanged.

**DECIDED — SC-9.4.** Local owner-lane curation may produce provisional memory
operations against stable transcript/evidence identities. Shared curation needs
the destination's lease and revision fence. Replay of a curation plan cannot
obtain a new lease, advance unseen inputs or rerun inference. The client selects
and synthesizes; replicated results are data, not instructions to another client.

**DECIDED — SC-9.5.** An explicit at-rest threat model must be reviewed before
whole-account enrollment. It covers OS owner access to open-plane data, other
local users, device theft, backup copies, key custody and revoked/offline devices.
Vault keys stay client-held; synchronization carries ciphertext and authorized
key lifecycle metadata, never plaintext vault keys. Account replication alone
does not provision keys or grant decryption. Missing or mismatched keys fail
closed without replacement of an existing binding.

### Current code

**CURRENT CODE** — CLI secret creation invokes the client service
([CLI](../../../cmd/witself/secret.go#L265)); that service seals sensitive fields
before sending them ([sealing](../../../internal/secretclient/secretclient.go#L411)).
The store receives already-encrypted input
([store boundary](../../../internal/store/secret.go#L229)). Associated data binds
account, realm, agent, secret and field identity
([binding](../../../internal/sealed/aad.go#L78)). This proves a client-encryption
boundary by source inspection, not whole-account at-rest protection.

**CURRENT CODE** — Memory permanent deletion writes deletion state without
advancing the memory change sequence
([deletion](../../../internal/store/memory_delete.go#L162)). Stable evidence and
retained deletion knowledge therefore cannot rely on that counter alone.

### Open items

| ID | item | owner | blocks |
|---|---|---|---|
| OPEN O-18 | Review at-rest threat model, personal-replica admission, grant expiry clock handling and the 30/37-day defaults. | planning lead | M5 |
| OPEN O-19 | Specify shared curation handoff, evidence identity and lease-fenced reconciliation of local provisional results. | planning lead | M6 |

### Harness obligations

- **DECIDED (SC-9.1–SC-9.3):** M4 models stale replicas, deletion near horizon,
  renewal with stalled catch-up, clock rollback, expiry and retirement;
  M5 proves new identity on re-enrollment
  and explicit stale/readiness state without claiming erasure.
- **DECIDED (SC-9.4, SC-9.5):** M6 races local/shared curation; M7 exercises
  missing/rotated/revoked keys and verifies that no plaintext key crosses the
  service boundary. M5 cannot enroll whole accounts before threat-model review.

## Open items by milestone

| ID | item | owner | blocks |
|---|---|---|---|
| OPEN O-1 | Enumerate every API operation, including maintenance writes, against the matrix and unsupported outcomes. | LC-S5 | LC-S5 |
| OPEN O-2 | Fix stable transcript entry/evidence projection and per-family causal encodings without renumbering existing references. | planning lead | M4 |
| OPEN O-5 | Choose the durable predecessor-fence and successor-publication proof, including failed transfer recovery. | planning lead | M4 |
| OPEN O-6 | Define inert request projections and adversarial compound-publication fixtures for every request lifecycle operation. | LC-S5 | M4 |
| OPEN O-7 | Fix canonical envelope/receipt encoding, payload commitment, durable refusal and authenticated receipt verification. | planning lead | M4 |
| OPEN O-8 | Set bounded queue, fragment and dependency-wait budgets with explicit refusal/recovery fixtures. | LC-S5 | M4 |
| OPEN O-9 | Select capture mechanism, activation schema/version and the database proof that drains or captures transactions spanning the barrier. | planning lead | M4 |
| OPEN O-10 | Inventory and instrument mixed server, worker, foreground reconciliation and maintenance writers with rollback coverage cases. | LC-S5 | M4 |
| OPEN O-11 | Add restore marker semantics and journal, receipt, deletion-knowledge and device-record movement to the lifecycle-epoch seam. | planning lead | M4 |
| OPEN O-12 | Define cursor/receipt authentication and older-backup recovery limits when only partial surviving evidence remains. | planning lead | M4 |
| OPEN O-13 | Define expected per-action events/rollups and replay provenance for every matrix row, including user reads and reconciliation. | LC-S5 | M4 |
| OPEN O-3 | Define resumable per-item join review and sealed-identity conversion or refusal with client-held keys. | planning lead | M5 |
| OPEN O-4 | Specify transition checkpoints and expected identity, capacity and message outcomes for all three fixture branches. | LC-S5 | M5 |
| OPEN O-16 | Bind M0b's explicit local policy and both destination admission snapshots to authenticated policy versions and revocation. | planning lead | M5 |
| OPEN O-17 | Confirm self-managed hosted-email eligibility and capacity policy without imposing SaaS payment on enrollment. | founder | M5 |
| OPEN O-18 | Review at-rest threat model, personal-replica admission, grant expiry clock handling and the 30/37-day defaults. | planning lead | M5 |
| OPEN O-19 | Specify shared curation handoff, evidence identity and lease-fenced reconciliation of local provisional results. | planning lead | M6 |
| OPEN O-15 | Resolve pricing for affected hosted usage before feature activation without changing replay semantics. | founder | M7 |
| OPEN O-14 | Specify reporting fields, build signal, disclosure/control, queue/retention bounds and receiver ownership. | founder | M8 |

## Exit check

- **DECIDED (SC-0.1, SC-0.2):** The plan's
  [M0c exit](implementation-plan.md#m0c-synchronization-contract) requires reviewed
  records. Sections 1–9 supply the draft; planning lead acceptance remains
  pending. Links alone do not mark M0 complete or authorize LC-S5.
- **DECIDED (SC-0.2):** Every unresolved item is listed with owner and blocked
  milestone or slice in [Open items by milestone](#open-items-by-milestone).
  A milestone cannot pass while one of its contract gates remains open.
- **DECIDED (SC-0.1, SC-2.1, SC-5.1, SC-6.1, SC-9.5):** Disposable synthetic
  identities may exercise the sections' harness obligations. Durable customer
  data remains prohibited until identity/schema preservation or migration and
  the applicable enrollment/security contracts are demonstrated.
