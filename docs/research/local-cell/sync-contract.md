# Local cell synchronization contract

Status: draft contract record for M0c; this revision closes M4-blocking design items O-2, O-5, O-7, O-9, O-11 and O-12 with adopted resolutions; remains a draft pending LC-S5 fixtures and M4 proof.

**CURRENT CODE** — Source revision for the adopted-resolution anchor review: `c507399c5487` (prior record pin: `7fd2f8321b05`).
The added and corrected source-line links were reopened in this checkout; source inspection is not runtime proof.

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

**DECIDED — SC-1.15.** Object identity, causal identity and numeric presentation coordinates are distinct. Preserve existing transcript conversation and entry IDs. New entries carry conversation generation, origin write-lineage identity, origin sequence and immutable entry ID; operation IDs remain the separate 128-bit identities required by SC-4.1. Cloning, restore or replica retirement cannot authorize two writers to reuse one origin write lineage. Same identity with different semantic input conflicts.

**DECIDED — SC-1.16.** Every published transcript sequence belongs to an immutable identified projection. Activation backfills existing mappings without changing values or meanings. Portable references carry projection identity or immutable entry IDs. Retain original aliases while live references require them; ambiguous unqualified coordinates block resolution. The accepted projection is append-only and dependency-respecting; its durable presentation order does not decide conflict winners. Provisional origin projections remain distinct and visibly pending.

**DECIDED — SC-1.17.** Transcript evidence denotes immutable ordered entry membership, not the interval between endpoints after merging. Before synchronization, freeze every existing inclusive range's complete membership and preserve its original projection/endpoints as aliases. Apply the same rule to curation inputs, cursor windows, coverage and other transcript locators. Redaction/deletion changes availability, never membership; retention preserves permitted value-free identity/deletion knowledge rather than deleted content. Reuse of an external conversation name starts a distinct generation.

**DECIDED — SC-1.18.** Expected versions name exact immutable revision identities within account incarnation and object generation. Bare branch-local integers are not causal tokens. Facts compare observed assertion and relevant fact/candidate/subject-namespace revisions; memories compare exact versions and evidence dependencies, with the exact shared-curation fence where applicable. Transcripts use explicit conversation/predecessor/entry dependencies. Deliveries compare authority epoch, revision and claim fence; complete requests compare authority epoch and graph revision for every graph mutation. Secrets compare exact secret/vault/field generations without rewriting sealed associated data. Avatars compare profile revision, lineage, parent and style identities. Preferences and consent compare separate revisions. Failed comparisons remain explicit conflicts; no family derives causality or victory from wall clock, numeric allocation or content equality.

**DECIDED attachment (SC-1.18) — Per-family causal basis.** The common causal union carries account incarnation, object generation, exact expected revision tokens and typed dependencies. Expected absence is explicit for creation; recreation names the tombstone/deletion generation and reviewed intent. Dependencies are bounded, sorted typed IDs, not an unbounded account-wide vector clock. Causal ordering never comes from wall-clock time. A provisional child names its exact parent's operation/version; if that parent conflicts, the child waits for explicit resolution rather than rebasing automatically.

| Family | Decided encoding and atomic acceptance | CURRENT CODE seam and missing work |
|---|---|---|
| Facts, assertions and candidates | fact_id/address + expected assertion_id + fact metadata revision + relevant candidate-set revision + subject namespace revision; create requires expected absence; candidate decision names candidate_id and observed assertion; recreation names deletion generation. | Candidate acceptance compares the observed assertion ([comparison](../../../internal/store/fact_candidate.go#L524)); direct set installs a new assertion/head ([set](../../../internal/store/fact.go#L230)). Add direct-set CAS and stable metadata/set revisions. |
| Subjects and aliases | Expected subject and owner-namespace revision; resolve canonical key and alias collisions under one namespace lock. Do not commit a mutable alias as if it were immutable subject identity. | Shared/exclusive owner-agent locking exists ([locking](../../../internal/store/fact_subject.go#L256)); upsert updates an existing display name ([upsert](../../../internal/store/fact_subject.go#L86)). Add revision tokens, preserving stable subject IDs. |
| Memories, evidence, curation and vectors | Exact memory version_id and evidence entry/version dependencies; multi-memory supersede compares all affected heads atomically. Shared curation adds request/run/fencing generation and exact plan revision/commitment; owner-local curation carries no invented shared lease. Vectors bind exact immutable version and content commitment. | Integer head CAS exists ([CAS](../../../internal/store/memory.go#L847)); evidence has scoped version FKs ([FKs](../../../internal/store/migrations/0029_add_narrative_memories.sql#L224)); vector equality checks content ([equality](../../../internal/store/memory_vector.go#L325)). Add stable version identities and projection-qualified curation windows. |
| Transcripts | Conversation ID/generation, explicit predecessor entry ID(s), reply target when present, entry ID and creation dependency. Concurrent siblings survive. Redaction names exact entry/generation and dominates stale payload. | Opaque IDs/reply links exist ([schema](../../../internal/store/migrations/0019_add_transcript_ledger.sql#L35)); append's numeric allocation remains local ([allocation](../../../internal/store/transcript.go#L239)). Add generation, explicit causal predecessors and projection tables. |
| Ordinary messages/deliveries | Immutable message ID/send operation; mutable delivery basis is delivery_id + authority_epoch + revision token + exact claim_id/processing_generation where required. Reply/send dependencies identify parents. | Claim/generation/lease checks exist ([checks](../../../internal/store/message.go#L633)); add delivery authority epoch/revision. Replica history conveys no executable claim. |
| Complete request graphs | request_id + authority_epoch + expected_graph_revision, plus exact selection/claim generation for the operation. Every state-affecting graph mutation, including expiry, sibling reservation changes and notification publication, advances the graph revision in one transaction. | Request locking and selection generations exist ([locking](../../../internal/store/message_request.go#L1692), [generation](../../../internal/store/message_request.go#L1915)); ordinary claims consult graph links ([links](../../../internal/store/message.go#L1873)). Add the complete graph revision; a selection generation is not its substitute. O-5 retains transfer-fence proof. |
| Secrets and vault | Expected stable secret revision and vault key ID/epoch; field value identity/version, DEK generation and wrap revision; dependency on exact sealed coordinates. Shared grant/rotation settlement stays authoritative. | Expected row version exists ([version](../../../internal/store/secret_mutation.go#L386)); sealed AAD binds immutable coordinates and encryption generations ([AAD](../../../internal/sealed/aad.go#L34)). Layer stable causal identity around these values; never rename/renumber AEAD-bound fields or decrypt on replay. |
| Avatars | Expected profile revision token, lineage generation, immutable parent version ID, style ID/version, and exact proposal dependency for activation. | Profile/style/parent checks and immutable version creation exist ([checks](../../../internal/store/avatar_mutation.go#L165), [creation](../../../internal/store/avatar_mutation.go#L222)). Qualify branch-local revision integers and preserve immutable parent IDs. |
| Preferences and consent | Separate expected preference and consent revision tokens plus policy basis. Local display changes can overlay accepted state provisionally; withdrawal tightens local use immediately; shared expansion requires destination validation. | Preferences explicitly use LWW without revision or idempotency ([preferences](../../../internal/store/dashboard_preferences.go#L129)); consent stores accepted terms/privacy versions and time ([consent](../../../internal/store/migrations/0094_add_account_consent.sql#L10)). Add mutation revisions; terms version and timestamp are not concurrent-write fences. |

Each mutation installs its accepted revision identity, aliases and causal result atomically with journal/receipt state. Expected-version comparison occurs under the corresponding existing domain lock or a new equivalent. The operation matrix still controls permissions and unsupported effects; explicit dependencies do not grant authority.

**DECIDED — SC-1.19.** Creation requires explicit expected absence or a creation dependency; recreation names the prior deletion generation and reviewed intent. Missing dependencies stage invisibly within bounded limits; incompatible generations refuse. A child of a rejected/conflicting provisional parent remains pending for explicit resolution and cannot be silently rebased. Install numeric aliases and version identities atomically with mutations and capture records.

### Current code

**CURRENT CODE** — Memory updates compare an expected version before advancing
it ([version check](../../../internal/store/memory.go#L863)). Transcript append
allocates sequence numbers under a database row lock
([allocation](../../../internal/store/transcript.go#L239)); that is not a
disconnected multi-writer identity scheme.

**CURRENT CODE** — Transcript entries already have opaque primary keys and ID-based reply links ([entry schema](../../../internal/store/migrations/0019_add_transcript_ledger.sql#L35), [reply links](../../../internal/store/migrations/0019_add_transcript_ledger.sql#L47)); append creates the entry ID after locking the conversation ([creation](../../../internal/store/transcript.go#L307)). Memory evidence stores inclusive sequence bounds and integer source-memory versions ([evidence](../../../internal/store/migrations/0029_add_narrative_memories.sql#L194)); endpoint foreign keys and a deferred contiguous-range check protect the full interval ([endpoints](../../../internal/store/migrations/0029_add_narrative_memories.sql#L238), [contiguity](../../../internal/store/migrations/0029_add_narrative_memories.sql#L392)). SC-1.15–SC-1.19 preserve those references; endpoint translation alone does not preserve membership.

**CURRENT CODE** — Fact deletion stores a tombstone and conflict basis
([tombstone](../../../internal/store/fact_delete.go#L190)). These domain controls
are reuse seams, not evidence that the connected matrix is implemented.

### Open items

| ID | item | owner | blocks |
|---|---|---|---|
| OPEN O-1 | Enumerate every API operation, including maintenance writes, against the matrix and unsupported outcomes. | LC-S5 | LC-S5 |
| CLOSED O-2 | Stable opaque identities, projection-preserving evidence and typed causal bases fixed by SC-1.15–SC-1.19. LC-S5 specifies migration/projection fixtures; M4 proves preservation; ambiguous preactivation joins remain O-3. | planning lead; LC-S5/M4 for proof | M4 proof; per-family M6 enablement |

### Harness obligations

- **DECIDED (SC-1.1–SC-1.14):** LC-S5 supplies one permission, offline,
  conflict, deletion, retention and unsupported-operation fixture per row.
- **DECIDED (SC-1.4–SC-1.6):** M4 races two local writers and a remote-only
  writer, then replays deleted generations and checks stable evidence references.
- **DECIDED (SC-1.15–SC-1.19):** LC-S5 specifies migration/projection fixtures; M4 records every legacy coordinate-to-ID mapping, exact evidence membership and memory-version reference, interrupts/resumes backfill, and proves unchanged meaning with no partly migrated account ready.
- **DECIDED (SC-1.15, SC-1.16):** M4 starts two disconnected local replicas and a remote-only client from one baseline, forces equal local sequence numbers on different entries, shuffles batches and loses ACKs; preserve every ID, origin alias and one durable accepted projection.
- **DECIDED (SC-1.17):** M4 maps source range 10–12 naming A/B/C into accepted coordinates 20–24 interleaved with X/Y; assert exact ordered A/B/C membership by ID, not equal endpoints or counts.
- **DECIDED (SC-1.18, SC-1.19):** M4 creates different memory/profile revisions with equal branch-local integers; neither passes CAS against the other. Preserve the losing branch and dependent curation result for explicit resolution.
- **DECIDED (SC-1.15):** M4 clones an origin counter/installation, restores an older copy and retires/re-enrolls it; publication needs a valid new lineage or explicit continuity, and copied identity cannot accept different operations at one origin coordinate.
- **DECIDED (SC-1.16, SC-1.17):** M4 redacts/deletes then replays stale entries, retires receipt horizons and reuses an external conversation name; old evidence retains its original generation or explicit unavailable state, without resurrection or alias retargeting.
- **DECIDED (SC-1.17–SC-1.19):** M4 pins curation windows to one projection while delaying another's dependencies; no cursor advances over unseen input and a plan against a conflicted provisional version cannot silently apply to the accepted head.
- **DECIDED (SC-1.18, SC-1.19):** M4 refuses wrong account/realm/owner dependencies even when the ID exists, and exercises fact/alias, request-sibling, vault-rotation, avatar-parent and preference/consent races using each family's basis.

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

**DECIDED — SC-3.6.** Request authority is identified by account incarnation, request ID, authority installation ID and a positive monotonic graph epoch. Graph epoch and graph revision are bounded integers encoded as decimal strings; neither is lifecycle epoch, capture epoch, selection generation, claim generation or a timestamp. The current authority stores a durable active, fenced, staged or retired state. Transfer preserves the final graph revision, logical identities, completed outcomes and original lease bounds.

**DECIDED — SC-3.7.** A transfer has one immutable random 128-bit transfer ID and exactly one named successor. The predecessor transaction locks and drains the whole graph, records its final revision, journal boundary and complete manifest, installs a permanent old-epoch write fence and commits a transfer outbox. Only committed fence state may produce an authenticated predecessor-fence proof binding both incarnations, graph, installations, epochs, revision, manifest and protocol version. An advisory lock, route projection, successful RPC or elapsed timeout is not such proof.

**DECIDED — SC-3.8.** The successor verifies the predecessor-fence chain and stages the complete graph invisibly. It durably installs the next epoch before publication. Publication is a separate transaction recording successor authority with its receipt/outbox. Fence, installation, publication and abort proofs become usable only after their issuing checkpoints are independently pinned. Successor execution requires its verified publication pin; local publication commit already excludes abort while pinning is pending. Missing proof, incomplete installation or pending pin remains authority_transfer_pending and confers no executable lease or reconciliation authority. Routing projects verified publication; readers and mutations reject superseded authority.

**DECIDED — SC-3.9.** Transfer recovery replays the exact immutable transfer and committed proofs. A lost acknowledgement creates neither a new transfer nor a new action. A fenced predecessor never resumes its old epoch. Returning authority before successor publication requires the successor's durable terminal abort naming the recovery owner at a strictly newer epoch; abort and publication are mutually exclusive. After publication, return uses an ordinary fenced transfer. Missing or contradictory proof, including an unavailable successor, blocks activation without discarding work.

**DECIDED — SC-3.10.** Every graph mutation, including foreground reconciliation, maintenance, deletion and linked protocol-message mutation, validates current incarnation, installation, active authority state, verified publication pin and exact graph epoch inside the committing transaction. Database guards and the capture barrier cover incompatible writers. A stale epoch returns authority_epoch_stale without graph mutation. Historical receipt inspection may return the original immutable outcome but never recreates execution authority. Imported inspection remains projection-only.

**DECIDED — SC-3.11.** Fence, abort, installation and publication evidence moves and survives retirement under the incarnation and recovery contract. Restoring or cloning an old authority row cannot activate it without continuity proof. Transfer never treats lease expiry as proof that an external action stopped; unresolved transferred execution blocks reassignment until its outcome is safely reconciled using its original logical action identity.

### Current code

**CURRENT CODE** — Ordinary claiming checks protocol ownership through request
links ([ownership check](../../../internal/store/message.go#L1873)). Request
creation writes graph and notification state in its store operation
([creation](../../../internal/store/message_request.go#L552)).

**CURRENT CODE** — Request list reconciles lifecycle state
([inspection](../../../internal/store/message_request.go#L669)). A mutation
failure path can commit due-expiry reconciliation
([reconcile](../../../internal/store/message_request.go#L1308),
[failure commit](../../../internal/store/message_request.go#L1315)); operator
agent deletion also changes requests
([deletion call](../../../internal/store/agent.go#L242),
[graph reconciliation](../../../internal/store/message_request.go#L1383)).

**CURRENT CODE** — Request selection/claim generations exist ([selection](../../../internal/store/migrations/0038_add_open_message_requests.sql#L97), [claims](../../../internal/store/migrations/0038_add_open_message_requests.sql#L122)); they are not graph authority coordinates. Creation shares one transaction ([begin](../../../internal/store/message_request.go#L512), [opening message](../../../internal/store/message_request.go#L552), [request](../../../internal/store/message_request.go#L583), [commit](../../../internal/store/message_request.go#L613)). Completion retries compare exact generation/input and return the stored result ([replay](../../../internal/store/message_request.go#L2516)); this does not prove external execution stopped.

**CURRENT CODE** — Account evacuation already uses shared mutation locks and a conflicting transition lock ([guard](../../../internal/store/migrations/0070_add_account_evacuation_fence.sql#L37), [drain](../../../internal/store/migrations/0070_add_account_evacuation_fence.sql#L60), [transition](../../../internal/store/evacuation.go#L74)). These are reusable database-fence patterns, not an existing graph transfer proof.

### Open items

| ID | item | owner | blocks |
|---|---|---|---|
| CLOSED O-5 | Durable predecessor fence, successor-bound installation/publication proofs and monotone failed-transfer recovery specified by SC-3.6–SC-3.11. O-6 retains inert/compound projection inventory; O-11/O-12 retain restore/authentication evidence boundaries. | planning lead | LC-S5/M4 implementation and runtime proof remain required |
| OPEN O-6 | Define inert request projections and adversarial compound-publication fixtures for every request lifecycle operation. | LC-S5 | M4 |

### Harness obligations

- **DECIDED (SC-3.1–SC-3.3):** M4 races sibling claims and transfer, partitions
  the predecessor, and rejects every stale-epoch mutation without dual owners.
- **DECIDED (SC-3.4, SC-3.5):** M4 delivers notifications before links, splits
  batches, crashes between writes and races ordinary claims against publication.
  Snapshot and incremental retries publish once; inspection produces no writes.
- **DECIDED (SC-3.7, SC-3.10):** LC-S5 models and M4 races freeze against claim, renewal, release, offer/decline, selection, completion, cancellation, expiry, agent deletion, maintenance and failure-path reconciliation; pre-freeze transactions cannot commit unrecorded later effects.
- **DECIDED (SC-3.7):** Race two transfer IDs/targets for one predecessor epoch; exactly one successor binding survives and altered-input ID reuse conflicts.
- **DECIDED (SC-3.7–SC-3.9):** Crash before/after fence, install, publish and outbox commits and between every control commit and witness pin; lose ACKs and reorder duplicates. Preserve one transition/message set and refuse reliance on unpinned control proofs.
- **DECIDED (SC-3.8, SC-3.10):** Reject missing, forged, wrong-account/incarnation/target/revision proofs and manifest fragments; exercise notification-before-link, ordinary-claim races and imported list/show/checkpoint inactivity under O-6.
- **DECIDED (SC-3.10):** Send every stale-epoch mutation to both endpoints; historical receipt inspection restores no lease. Direct SQL, old writers and retention cannot bypass guards.
- **DECIDED (SC-3.3, SC-3.9):** Leave the target unavailable while advancing simulated time arbitrarily; transfer stays pending while unrelated local graphs continue.
- **DECIDED (SC-3.9, SC-3.11):** Race abort with publication and restore pre-abort/pre-fence snapshots; cancelled or retired epochs never reactivate.
- **DECIDED (SC-3.6, SC-3.11):** Preserve completed outcomes, unexecuted reservations and ambiguous already-started actions distinctly; none becomes fresh work. LC-S5 supplies the deterministic model; M4 requires real disposable-store proof, not model-only synchronization claims.

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

**DECIDED — SC-4.8.** Protocol v1 uses a strict versioned canonical JSON profile for semantic envelopes and value-free receipts. Reject ambiguous/duplicate fields, invalid Unicode, unknown schema fields and noncanonical typed identifiers. Encode counters as exact decimal strings, preserve ordered arrays, canonicalize declared sets, and preserve arbitrary JSON numeric values through the versioned exact-decimal typed payload adapter. Freeze domain normalization before capture. Exclude transport retries, batching, endpoints and credentials; preserve every semantic field, original incarnation and capture epoch on retry.

**DECIDED — SC-4.9.** Compare semantic input with an account-scoped authority-held HMAC-SHA-256 commitment under the key ID fixed at first admission of (account_id, operation_id). Sensitive input participates in comparison but is absent from durable value-free receipts, refusal bases and diagnostics. Vault plaintext and credentials never enter the envelope. Preserve the original comparison key through every dependent receipt horizon, unresolved operation and authorized authority transfer; inability to compare requires recovery, not acceptance of a client commitment.

**DECIDED — SC-4.10.** Serialize operation admission by account and operation ID. Authorize operation/origin-scoped receipt and equality access before disclosure, separately from new-mutation permission. Store one immutable canonical terminal receipt and separately store each operation_id_reuse refusal keyed by that ID, its original comparison-key ID and the conflicting commitment. Identical retries return their stored outcome without changing origin, incarnation, causal result or signature. Mutation, deduplication and accepted receipt commit together; refusal commits without the requested mutation. Bounded invalid envelopes receive a durable invalid_envelope receipt only with an authenticated unambiguous identity header including original origin/incarnation; unidentifiable transport input cannot fabricate one.

**DECIDED — SC-4.11.** Authenticate canonical receipts with a dedicated destination Ed25519 signature binding original operation scope, commitment, issuing authority/incarnation/epoch, acceptance coordinate and closed status/reason. A client accepts only after signature/trust/scope validation and authenticated binding to its immutable submitted operation. Persist and return the original signed bytes after durable commit. A signature is historical evidence, not proof of present authority: successor and restored authorities additionally require SC-6 continuity/fence evidence. Missing comparison keys, trust bindings or recovery evidence leave work pending and never justify fresh-ID replay.

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

**CURRENT CODE** — The memory-plan canonicalizer is a restricted encoder with UTF-8/duplicate/trailing-data validation ([encoder](../../../internal/store/memory_curation_plan.go#L990)), UTF-16 key ordering ([ordering](../../../internal/store/memory_curation_plan.go#L1083)) and binary64 number parsing ([numbers](../../../internal/store/memory_curation_plan.go#L1148)). Another JSON seam preserves numeric source text ([numeric preservation](../../../internal/nulsafe/nulsafe.go#L39), [sanitization](../../../internal/nulsafe/nulsafe.go#L118)); neither implements SC-4.8's generic exact-decimal typed adapter.

**CURRENT CODE** — Curation receipt lookup compares a request hash within its domain scope ([lookup](../../../internal/store/memory_curation.go#L569), [receipt](../../../internal/store/memory_curation.go#L599)). Outbound email signs an explicit audience/version/key/body binding ([binding](../../../internal/agentemailoutbound/dispatch.go#L220), [signing](../../../internal/agentemailoutbound/dispatch.go#L348)); relay verification uses an independently selected public key and a transport replay window ([verification](../../../internal/agentemail/relay.go#L186)). These are reusable primitives, not cross-family sync deduplication, dedicated sync trust roots or historical-authority proof.

### Open items

| ID | item | owner | blocks |
|---|---|---|---|
| CLOSED O-7 | Canonical JSON/typed payload profile, protected semantic commitments, immutable signed receipt and commitment-keyed refusal format fixed by SC-4.8–SC-4.11. LC-S5 owns codec and fault fixtures; O-12 retains enrollment trust roots, key transition and authenticated historical receipt-frontier proof. | planning lead; LC-S5 for proof; O-12 for continuity | M4 until proof and O-12 are accepted |
| OPEN O-8 | Set bounded queue, fragment and dependency-wait budgets with explicit refusal/recovery fixtures. | LC-S5 | M4 |

### Harness obligations

- **DECIDED (SC-4.1–SC-4.3):** LC-S5 rejects missing fields, unknown reasons,
  forged authorization, cross-account replay and changed-input ID reuse.
- **DECIDED (SC-4.4–SC-4.7):** M4 crashes at every state boundary, loses ACKs,
  duplicates/reorders batches and advances retention beyond payload retirement.
  Check durable refusal, once-only apply, pinned unresolved work and no loops.
- **DECIDED (SC-4.8):** LC-S5 supplies golden vectors from two independent encoders; M4 verifies reordered keys, escaping, supplementary Unicode, duplicates, invalid UTF-8/surrogates, omitted/null/default distinctions, set ordering, binary encodings, exact large integers/decimals and unknown fields. Equivalent valid inputs have identical bytes/commitments; distinct values cannot collapse through rounding.
- **DECIDED (SC-4.9, SC-4.10):** M4 races identical input and changed payload/origin/causal basis under one account/ID, then restarts and retries; assert one canonical receipt, no duplicate mutation and one stable refusal per conflicting commitment. Another account may reuse that random ID.
- **DECIDED (SC-4.10, SC-4.11):** M4 crashes before/after mutation/receipt commit, signature storage and source terminal-state persistence, drops ACKs, splits/regroups batches and reconnects; stored outcomes/signatures remain byte-identical and inbound replay emits no new logical operation.
- **DECIDED (SC-4.7, SC-4.9):** M4 deletes sensitive payload beyond domain retention while preserving receipt floors/pins, retries original/conflicting input under the original HMAC key, and asserts absence of content, dedup-only ciphertext copies and unkeyed sensitive hashes in value-free rows/diagnostics.
- **DECIDED (SC-4.9, SC-4.11, SC-6.10–SC-6.14):** M4 rotates signing/comparison keys, moves authority with a lost ACK and verifies historical signatures without transport freshness. Missing protected comparison key, public trust binding or complete handoff proof stays pending; claimed commitments cannot substitute.
- **DECIDED (SC-4.11, SC-6.10–SC-6.14):** M4 forges signature/key/account/issuer/incarnation/result/reason, presents signed old-authority receipts beyond the authenticated cutover frontier and restores backups missing receipts; assert no signature-only acceptance, false convergence or receipt-only secret reconstruction.
- **DECIDED (SC-4.8–SC-4.10):** M4 mixes identifiable malformed input, unidentifiable garbage and unsupported versions, requiring durable refusal only for a trustworthy complete minimal identity header; deny another origin/member's existence/equality probes before disclosure and exercise O-8 bounds without evicting pinned work or inventing domain refusals for transport failure.

## 5. Capture activation barrier

### Contract

**DECIDED — SC-5.1.** Capture activation is per account, recorded durably in a
schema-versioned activation record carrying capture epoch, minimum capable
writer schema, coverage state and snapshot watermark. Snapshot activation
requires verified coverage of every server, worker and maintenance write path,
including remote-only clients through their server and writes on failed requests.

**DECIDED — SC-5.2.** Capture uses a semantic transactional journal enforced by database writer guards and deferred coverage checks. Startup checks do not fence already-running writers. Every covered path explicitly admits its transaction before domain access, acquires the account gate, lock-validates capability/activation state and maps actual mutations into a sealed bundle before commit. Guards refuse missing admission rather than acquiring gates late. Capable writers use distinct registered principals inaccessible to legacy roles; shared-login promotion, claimed schema fields and session settings are not capability proof. Capability revocation drains its locking readers. Runtime roles cannot bypass guards or modify capture-control state.

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

**DECIDED — SC-5.5.** Activation record version 1 binds account identity, lifecycle epoch, restore marker, monotonically advancing capture epoch, capture protocol version 1, minimum writer schema/capability, reviewed coverage-manifest version/commitment, coverage state and snapshot identity/watermark. Capture epoch is distinct from lifecycle and request-authority epochs. The actual capture migration ordinal is recorded during implementation; a schema number alone is not coverage proof.

**DECIDED — SC-5.6.** First guard installation drains pre-existing unguarded DML using conflicting relation locks over the complete covered inventory. Subsequent activation obtains the exclusive account capture gate; covered writers retain the shared transaction gate through commit/rollback, including before activation. Activation advances its persistent control row and pins a consistent snapshot/watermark; the same physical gate-owning session must acknowledge the pinned snapshot before releasing its lock. Loss/uncertainty of that session before acknowledgment invalidates the candidate boundary. A transaction with an older snapshot must serialization-fail or be explicitly refused when locking changed control/admission rows; cached/nonlocking state cannot authorize a write. Lock timeout, prepared transaction or incomplete coverage leaves activation pending.

**DECIDED — SC-5.7.** Journal bundles preserve transaction and compound-operation boundaries. Deferred checks reject any uncovered semantic mutation. Finalization serializes the account watermark using a separate transactionally locked counter retained until commit; later domain mutation in a sealed transaction is refused. Database sequence allocation, maximum observed sequence, timestamps and process inventories are not commit-order or snapshot proofs. Inbound application preserves the original logical operation and commits its receipt atomically without emitting a fresh outbound user action.

**DECIDED — SC-5.8.** Admission, table/trigger coverage and rollback protection remain enforced after activation. Unsupported downgrade cannot remove guards while connected state or unresolved work depends on them. Coverage loss suspends readiness and snapshot publication, preserving journal, receipts and pending work. Recovery requires verified complete catch-up or a strictly newer capture epoch and rebootstrap. LC-S5 inventories writers/tables/maintenance and M4 proves mixed binaries, stale snapshots, forgotten journal writes, commit order and rollback before any activation is accepted.

### Current code

**CURRENT CODE** — Startup performs migration preflight before applying schema
changes ([serialized preflight](../../../internal/store/migrate.go#L128), [migration](../../../internal/store/migrate.go#L150)), verifies the
exact compiled target ([target](../../../internal/store/migrate.go#L198)), and
rejects a database newer than the binary
([schema-ahead check](../../../internal/store/migrate.go#L299)). The existing
compatibility-release rule is specific to fact deletion
([compatibility rule](../../../internal/store/migrate.go#L323)); it is not an
account capture record or an in-flight transaction barrier.

**CURRENT CODE** — Unselected and capture-disabled dashboard preference writes
retain their direct SQL and absence of an audit event or revision fence
([preferences](../../../internal/store/dashboard_preferences.go)). The selected,
capture-active path uses one admitted `preferences.put` operation with the prior
revision dependency ([admission and sealing](../../../internal/store/sync_capture_tx.go)).
The audit insertion stores verb and metadata
([event insertion](../../../internal/store/events.go#L1075)), while outbound
email uses its own outbox
([outbox](../../../internal/store/agent_email_outbound.go#L682)). These paths
cannot substitute for verified account-wide change capture.

**CURRENT CODE** — Storage accounting already uses DML triggers, TRUNCATE rejection and durable-state downgrade guards ([DML](../../../internal/store/migrations/0091_bound_agent_email_cell_storage.sql#L381), [TRUNCATE](../../../internal/store/migrations/0091_bound_agent_email_cell_storage.sql#L401), [downgrade](../../../internal/store/migrations/0091_bound_agent_email_cell_storage.sql#L436)). Migration 0101 combines an activity probe with a real table lock ([probe](../../../internal/store/migrations/0101_add_transcript_storage_usage.sql#L39), [lock](../../../internal/store/migrations/0101_add_transcript_storage_usage.sql#L60)); an integration fixture observes an actually blocked advisory lock ([fixture](../../../internal/store/activity_integration_test.go#L497), [observation](../../../internal/store/activity_integration_test.go#L521)). These enforcement/scheduling seams do not implement the proposed capture barrier.

**CURRENT CODE** — Import leases hold a database/account/epoch-scoped session advisory lock with connection cleanup ([scope](../../../internal/store/account_import_lease.go#L15), [acquisition](../../../internal/store/account_import_lease.go#L88), [cleanup](../../../internal/store/account_import_lease.go#L132)). Backup/self export uses read-only REPEATABLE READ snapshots ([snapshot](../../../internal/store/export.go#L130), [self export](../../../internal/store/export.go#L214)); purge uses SERIALIZABLE ([purge](../../../internal/store/account_purge.go#L248), [closed-account purge](../../../internal/store/account_purge.go#L490)). The SC-5.6 stale-snapshot and physical-session experiments therefore remain necessary.

**CURRENT CODE** — Explicitly selected local sync migration 0002 installs guards
and deferred coverage checks over the fixed-order reviewed inventory
([migration](../../../internal/store/sync_migrations/0002_sync_capture_guards.sql),
[inventory](../../../internal/store/sync_writer_inventory.go)). The production
helper admits before domain access, records semantic touches, seals one bundle,
and holds the finalized counter through commit
([transaction helper](../../../internal/store/sync_capture_tx.go)). Activation
holds an exclusive session gate through acknowledgment by that same physical
connection, and reports only disabled, bootstrapping or suspended capture status
([activation](../../../internal/store/sync_capture_activation.go),
[separate status](../../../internal/runtimeconfig/sync_status.go)). C1's
[single-executable capability records](../../../internal/syncharness/capture_runtime_results.go)
do not promote any H obligation; snapshot materialization, mixed binaries,
principal enforcement and native recovery remain separate acceptance work.

### Open items

| ID | item | owner | blocks |
|---|---|---|---|
| CLOSED O-9 | Semantic transactional journal with database coverage enforcement; activation record v1; relation-lock installation drain and account-gate snapshot barrier selected by SC-5.2 and SC-5.5–SC-5.8. O-10 retains complete writer/role/table inventory; runtime barrier acceptance remains unproven. | planning lead; O-10/LC-S5 inventory; M4 proof owner | M4 proof before M5 |
| OPEN O-10 | Inventory and instrument mixed server, worker, foreground reconciliation and maintenance writers with rollback coverage cases. | LC-S5 | M4 |

### Harness obligations

- **DECIDED (SC-5.1–SC-5.3):** M4 mixes old/new server and worker binaries,
  starts transactions before activation, and commits them after activation and
  snapshot creation. Assert complete capture or explicit refusal/drain; the
  current startup check alone is not a passing result.
- **DECIDED (SC-5.3, SC-5.4):** M4 includes failed request mutations, preference
  writes, maintenance and rollback; coverage loss must suspend readiness and
  preserve pending work before any further convergence claim.
- **DECIDED (SC-5.2, SC-5.5–SC-5.8):** LC-S5 inventories writers/tables/maintenance and supplies the following M4 barrier schedule. These are future disposable harness steps, not runtime proof. Use separate explicitly named test connections, barriers and observed database locks. Set timeouts and fail deterministically; retain the interleaving seed. The lock-key function below denotes a stable database/account-scoped namespace allocated by the implementation, with collision-safe scope checks. An advisory-key collision may over-serialize, never authorize another account.

- **DECIDED (SC-5.6) — Install guards under relation-lock drain.** Old connection U begins and mutates a covered table, then pauses before commit. Migration connection I takes the existing migration lock and requests ACCESS EXCLUSIVE on the full reviewed covered-table inventory in a fixed order:

  ~~~sql
  BEGIN;
  LOCK TABLE <complete reviewed covered-table inventory>
    IN ACCESS EXCLUSIVE MODE;
  -- Seed persistent activation rows, install guards/deferred checks,
  -- register coverage manifest and required role restrictions.
  COMMIT;
  ~~~

  Assert via pg_locks that I waits on the relation actually held by U, and that no account is activation-ready. Commit U; installation may now complete, and U's effect must appear in the later baseline snapshot. Repeat with U rollback. Existing prepared transactions can hold conflicting relation locks without an ordinary active backend; require timeout/pending activation rather than ignoring them. A lock timeout or partial inventory cannot publish a barrier.

- **DECIDED (SC-5.2, SC-5.6) — Probe the pre-BEGIN/late-write hole.** Start V before installation/activation but do not touch a covered relation. Once activation finishes, attempt its first covered DML. An old binary without admitted context is refused by the mandatory trigger. A capable READ COMMITTED transaction must read the current epoch and capture. Repeat with prepared statements on a connection that remained alive through the compatibility roll.

- **DECIDED (SC-5.2, SC-5.6) — Prove stale MVCC snapshots cannot read an obsolete “disabled” flag.** Begin V at REPEATABLE READ, establish its snapshot, then let A advance the existing activation row under its exclusive gate. V must take the shared gate and perform the locking read, never a cached or ordinary SELECT:

  ~~~sql
  SELECT pg_advisory_xact_lock_shared(capture_lock_key($1));
  SELECT capture_epoch, state, min_writer_schema,
         min_writer_capability, coverage_manifest_version
    FROM account_sync_capture
   WHERE account_id = $1
   FOR SHARE;
  ~~~

  Assert serialization failure or explicit refusal for the stale snapshot, no domain commit and no false readiness. Repeat under SERIALIZABLE and with a snapshot predating the control row itself; missing row must refuse. The changed-row locking behavior is a mandatory real-PostgreSQL experiment, not assumed proven by this record. Retry starts a fresh transaction and revalidates admission. Existing serializable purge makes this schedule material.

- **DECIDED (SC-5.6) — Steady-state account drain.** Every guarded writer holds its shared transaction gate through commit/rollback, even before activation. Pause capable writer T after DML. Connection A requests the matching exclusive **session** advisory lock, polling with a bound:

  ~~~sql
  SELECT pg_try_advisory_lock(capture_lock_key($1));
  ~~~

  Assert false while T is open; commit/abort T and assert the exclusive acquisition succeeds. This acquisition, combined with mandatory guard coverage, proves pre-barrier guarded transactions have finished. A process list or pg_stat_activity's current xmin alone proves neither complete writer coverage nor future writes from an old snapshot. No host process listing is needed.

- **DECIDED (SC-5.5, SC-5.6) — Advance epoch and pin the snapshot without an unlock gap.** While A keeps its exclusive session gate, begin an activation transaction, revalidate coverage/capabilities, advance capture_epoch, set state=bootstrapping and snapshot_id, and read W from the finalized counter. Write snapshot_watermark=W and COMMIT. Keep the session gate across that commit. On connection S:

  ~~~sql
  BEGIN ISOLATION LEVEL REPEATABLE READ READ ONLY;
  SELECT capture_epoch, snapshot_id, snapshot_watermark
    FROM account_sync_capture
   WHERE account_id = $1;
  ~~~

  This first read pins S's snapshot and must match A's epoch/ID/W. S reports that pinned state to A; the **same physical A connection**, still owning the exclusive session gate, must acknowledge it before the harness/coordinator accepts the candidate snapshot. Do not substitute a reconnected pool session or merely reread the durable control row. Then A releases its gate. New writers see the new epoch and finalize bundles greater than W. If A dies or its ownership becomes uncertain before this acknowledgment, discard the candidate S snapshot and take a fresh gated boundary—even if S read matching durable epoch/W, because writes might have slipped in after A's session lock disappeared. No stored watermark alone proves the boundary.

- **DECIDED (SC-5.6–SC-5.8) — Materialize, then publish.** Keep S's fixed snapshot for every baseline chunk; verify completeness/integrity before publishing its manifest. Catch up bundles above W with complete operation boundaries and receipt verification. If S dies before a durable complete snapshot exists, stage/discard the incomplete materialization and take a new boundary. A dead transaction's exported snapshot identifier cannot be treated as a durable resumable snapshot. Already captured/pending work survives this restart. Set active/readiness only after the declared snapshot/catch-up contract is satisfied.

- **DECIDED (SC-5.2, SC-5.7) — Forget the journal deliberately.** In a synthetic capable path update preferences, omit finalization and COMMIT. The deferred check must abort the whole mutation. Repeat with a fake empty bundle, one omitted touch among several writes, repeated updates of the same row, agent-deletion cascades, request-expiry changes committed on a failed request, and maintenance deletion. Assert no partial domain state/receipt. Then run legitimate paths and verify their full semantic effects, since a row-touch count alone is weak evidence.

- **DECIDED (SC-5.7) — Prove commit-order watermark.** T1 mutates and assigns its final ordinal, then pauses before commit. T2 mutates a different same-account row and tries to finalize. Assert T2 cannot publish a higher ordinal before T1 commits/aborts; a third consumer must not advance past missing committed work. Repeat T1 crash/rollback and lost ACK. Also demonstrate a counterexample using an ordinary sequence in the model, so replacing the serialized counter with MAX(sequence) kills the test.

- **DECIDED (SC-5.2, SC-5.8) — Mixed writers, rollback and enforcement.** Keep old server/worker connections alive across activation; exercise remote-only requests, foreground reconciliation, jobs, preference/autocommit conversions, direct maintenance, serializable purge, COPY and cascades. Each committed effect is captured, or the incompatible write is refused. Attempt legacy SET ROLE/EXECUTE, shared-login promotion, forged GUC/capability, missing explicit admission, trigger removal, TRUNCATE, an unregistered relation and a downgrade removing capture tables. Revoke capability while an old RR snapshot exists and prove its locking admission read fails. Ordinary roles must fail; a permitted schema change that invalidates coverage must atomically suspend readiness before publication. A refused old-writer attempt with guards intact is not itself coverage loss.

- **DECIDED (SC-5.5–SC-5.8) — Crash/recovery and accounting.** Crash after activation commit, before/after snapshot pin and A's acknowledgment, chunk write, bundle seal and receipt commit. In the A-death gap, let another writer commit before S reads a matching epoch/W and prove that candidate snapshot is rejected without the original-session acknowledgment. Preserve unresolved work/receipts; resume only through verified catch-up or a newer epoch/rebootstrap. Corrupt a coverage manifest or remove an enforcement mechanism in the authorized synthetic fixture and assert suspended readiness. Retry inbound application and snapshot rebuild with distinct replay provenance; assert no loop, duplicate user action, executable copied claim or duplicate usage rollup.

Initial relation locking covers writers that already executed unguarded DML; mandatory post-install guards cover transactions that began earlier but write later. The shared/exclusive account gate covers transactions crossing activation; the locking control-row read covers old MVCC snapshots; the finalization counter and pinned snapshot cover cursor continuity. None of those proofs can substitute for the others.

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
The detection requirement is subject to SC-6.14's independent-evidence and failure-domain limit; an unproven frontier never permits convergence.

**DECIDED — SC-6.6.** Lifecycle epoch is an exact nonnegative integer within the current control-plane safe-integer range, with canonical decimal authenticated encoding and overflow refusal. Allocation and published authority are distinct; aborted allocations remain consumed. Restore marker is 16 cryptographically random bytes with canonical encoding and equality-only comparison. A proven move advances lifecycle epoch and preserves the marker. Every backup restore, including the newest backup, creates a new marker before synchronization readiness. Exact retries of one durable restore operation reuse its marker; separate restores do not. Original operation IDs and immutable acceptance incarnations remain unchanged.

**DECIDED — SC-6.7.** Restore and writer activation pass an incarnation-installation barrier before authority publication. A supported restore wrapper durably records intent and marker outside restored account data, installs it under the account fence and resumes its exact transition after interruption. Continuation requires current authenticated single-writer evidence outside the restore/copy domain; a cloned local lock or witness cannot prove exclusivity. Missing proof causes a fresh marker and reconciliation, not active copied authority. Keep account lifecycle allocation/active incarnation separate from issuer-scoped committed-prefix heads; graph transfer does not move account ownership. Self-managed/local deployments provide equivalent mechanisms without SaaS. Whole-installation rollback without independent evidence cannot establish completeness; arbitrary live database rewrites are outside the supported restore interface.

**DECIDED — SC-6.8.** The versioned movement registry explicitly enumerates incarnation lineage, journal/atomic groups, immutable receipts and reuse refusals, deduplication commitments, deletion knowledge, capture coverage/watermarks, device enrollment/revocation, public verifier history, recovery cases and request authority/transfer proofs. Preserve original identity/provenance; install destination state without importing readiness as authority. Invalidate endpoint sessions, old cursors, transport capabilities and live writer grants, and prove destination capture coverage anew. Preserve replica-local pending work across materialized snapshot replacement. Missing required streams, including empty streams, or invalid cross-account references refuse publication.

**DECIDED — SC-6.9.** Source finalization follows proven successor publication and retains value-free retired-authority denial outside cascading account deletion. Account purge and source finalization have distinct enumerated retention policies. Purge removes payloads/sessions while preserving terminal account denial and the receipt/deletion floors; it cannot remove replay prevention while old authority remains admissible. Ordinary archive lease cancellation/reconciliation is not synchronization handoff: inert projection or proven handoff must independently satisfy SC-3 before copied graph state becomes executable.

**DECIDED — SC-6.10.** Authenticate cursors and portable operation receipts with distinct versioned, domain-separated authority signatures and retained public verification history. Bind cursors to issuer, audience, account incarnation, replica/enrollment generation, authority-stream/capture scope, complete applied prefix and commitment, deletion/revocation coverage, policy/grant and expiry. Whole-account continuation binds a signed stream catalog and complete frontier vector. A signature is neither current authorization nor proof of latest authority. Independently verify account and replica admission; never continue from an unsigned watermark or copied cursor.

**DECIDED — SC-6.11.** Commit immutable outcomes and input/result commitments atomically with mutation, deduplication and authority journal. Publish durable outcomes, usable control proofs and executable grants only after an idempotent outbox pins their issuer-scoped committed prefix in an authenticated witness outside the restore/copy domain. Witness CAS validates current authority and refuses sealed-stream extension. Cursors, snapshots, inspection and convergence cannot advertise unpinned history as settled; local visibility is explicitly pending. A committed operation awaiting pin is durability_publication_pending; retry resumes publication without reapplying or changing identity/outcome. Fencing seals all committed outcomes, including pending ACKs. Historical proof establishes content membership, not merely a numeric cutoff or possession of an old key.

**DECIDED — SC-6.12.** Reconnect presents authenticated replica identity, original pending operations, retained receipts and observed checkpoints. Old-incarnation receipts are historical evidence, not current permission. Recover effects only from authenticated terminal outcomes or complete committed authority journal, matching materialization/dependencies and deletion/authorization coverage through the independently verified frontier. Replica-captured work alone is not acceptance. A receipt proves its outcome, not recoverable payload or permission to restore later-deleted content. Recovery never repeats an external effect or creates a fresh operation to replace ambiguous prior work.

**DECIDED — SC-6.13.** Evaluate missing intervals by the receipt/journal/replica evidence table and expose separately versioned value-free recovery states. Preserve SC-4.3 terminal receipt status/reason. Ambiguous acceptance, unreconstructable accepted effects, missing deletion/authorization coverage, contradictory evidence and an unproven latest frontier block convergence and automatic re-execution. Rebootstrap preserves unresolved operations and does not supply missing acceptance proof. Evidence must prove complete intervals and atomic groups; a largest observed watermark is insufficient.

#### SC-6.13 normative attachment: partial-evidence decisions

Evaluate per missing operation or complete gap interval, not once per account. Presence must include authenticity and the coverage required below:

- **R**: terminal receipt with trusted issuer/incarnation and proved membership in its committed/fenced prefix. It can describe acceptance, rejection or conflict.
- **J**: complete authenticated **committed authority journal**, including immutable outcome/effect evidence and interval coverage. A replica's captured/published outbox, unsigned database row or raw WAL fragment is not J.
- **P**: surviving authorized replica materialization. Use its bytes only if matched to R/J result commitments or a certified snapshot/range proof, with current deletion/authorization checks. If a replica also holds receipts/journal, count those under R/J rather than giving arbitrary replica bytes equivalent authority.

Every recoverable row additionally requires a proven recovery frontier, complete causal/atomic dependencies and deletion/revocation coverage. Partial J or partial P recovers only individually proven complete effects; it cannot make the missing rest converge.

| R | J | P | Decision for the missing interval/effect |
|---|---|---|---|
| Yes | Yes | Yes | Recover proven canonical outcomes/effects from J; P may fill verified payload gaps. Disagreement triggers evidence conflict. Preserve original IDs, outcomes and accounting. |
| Yes | Yes | No | Recover if J contains complete required effect materialization/dependencies. Otherwise preserve known outcome and surface accepted-effect loss. Replica presence is not itself necessary. |
| Yes | No | Yes | R proves terminal outcome. Recover accepted materialization only where P matches result commitment and complete dependencies. A similar live row or input-only commitment is insufficient. |
| Yes | No | No | Preserve known terminal outcome/deduplication. Refusal needs no mutation reconstruction. Missing accepted materialization is `accepted_effect_unrecoverable` unless the restored base already proves the effect or verified later deletion/retirement requires its absence. Never re-execute. |
| No | Yes | Yes | Recover original canonical outcome from committed J. Retain recovered evidence and add a successor recovery attestation if original signature is lost; do not invent that signature. P supplies only verified missing materialization. |
| No | Yes | No | Same if J contains complete effect materialization. Otherwise preserve reconstructed outcome evidence and report accepted-effect loss. |
| No | No | Yes | Replica materialization alone cannot distinguish accepted, provisional or superseded work. Quarantine/preserve it and pending work as `acceptance_ambiguous`; do not upload it as a fresh mutation. Reclassify if authenticated R/J is discovered. |
| No | No | No | If an independent head proves a missing range: `history_gap_unrecoverable`. If the latest frontier itself is unknown: `continuity_unproven`. Neither permits convergence or automatic re-execution. |

Mandatory overrides:

1. **Deletion coverage:** a genuine accepted create/update is not proof the payload should still exist. If deletion knowledge through the required frontier is unproven, return `deletion_coverage_unproven` and do not resurrect it. A verified newer deletion wins; retention expiry does not recreate permission.
2. **Authorization coverage:** old enrollment/token/grant rows cannot prove current permission. Return `authorization_coverage_unproven`; obtain fresh authoritative enrollment/revocation evidence before protected continuation.
3. **Frontier completeness:** agreement among all available stale copies cannot prove no newer acceptance existed. Keep `continuity_unproven` where no independent current frontier is established.
4. **Horizon:** SC-4.7's minimum is 37 days through settlement/retirement, with unresolved work pinned afterward; SC-9.2 also requires deletion catch-up. Horizon expiry may require rebootstrap, never infer nonacceptance or change operation ID.
5. **Nonacceptance:** automatic retry as an unapplied operation needs positive proof that a complete fenced history excludes it and that no earlier executable grant/effect remains unresolved. An absent receipt in a snapshot is not that proof. Even when safely retried, retain the same operation ID.
6. **External effects:** restore state/receipt evidence only. Proven or ambiguous prior execution never becomes fresh provider dispatch, claim or other external action. Preserve logical-action accounting rather than adding a replayed user action.
7. **Conflicting authenticated histories:** do not pick the largest timestamp, counter or most numerous copies. Block with `evidence_conflict` until an authorized proof-based resolution exists.

An independent head that survives database rollback can prove the backup is older. It cannot reconstruct a missing range by itself. Conversely, if all observation-bearing state is rolled back with no external signal, automatic detection is impossible. In a declared restore or uncertain activation without a separately established frontier, the supported workflow reports `continuity_unproven`. This is not a claim that a hidden, perfectly indistinguishable whole-system rollback can always be detected; that failure domain lies outside the detection guarantee.

#### SC-6.13 normative attachment: exact value-free outcomes

These are a separate, versioned handshake/recovery protocol, **not additions to or replacements for SC-4.3's closed terminal receipt reasons**. The original accepted/rejected/conflict receipt remains immutable.

Response fields: `protocol_version, recovery_state, reason_code, convergence, recovery_id, required_action, blocked_operation_count, missing_range_count, receipt_evidence_present, committed_journal_evidence_present, replica_evidence_present, deletion_coverage, authorization_coverage`. `recovery_state` is `ready|pending|reconciling|blocked`; `required_action` is one of `none|retry_same_operation|rebootstrap_preserve_pending|obtain_evidence|reenroll|operator_review`. Evidence flags and counts describe the relevant gap, not a claim of full-account completeness. Ordinary user/diagnostic output excludes payload, tokens, keys, commitment bytes and sensitive resource identifiers.

| Exact reason code | User-visible meaning | State; convergence; required action |
|---|---|---|
| `continuation_verified` | Current continuation and declared catch-up are verified. | ready; true; none |
| `authority_transfer_pending` | Transfer lacks required fence, install or publication proof. | pending; false; obtain_evidence |
| `rebootstrap_required` | Authority changed; preserve pending work while replacing its projection. | reconciling; false; rebootstrap_preserve_pending |
| `durability_publication_pending` | Change committed; acceptance publication remains pending. | pending; false; retry_same_operation |
| `recovery_in_progress` | Recovering effects supported by verified evidence. | reconciling; false; obtain_evidence |
| `recovery_complete` | Recovered through the independently verified frontier, including deletion/authorization. | ready; true; none |
| `cursor_authentication_failed` | Continuation proof cannot be verified. | blocked; false; rebootstrap_preserve_pending, after fresh admission |
| `receipt_authentication_failed` | Historical outcome proof cannot be verified. | blocked; false; obtain_evidence |
| `replica_reenrollment_required` | Device continuation is retired or no longer authorized. | blocked; false; reenroll |
| `acceptance_ambiguous` | Evidence cannot determine whether prior work was accepted. | blocked; false; obtain_evidence |
| `accepted_effect_unrecoverable` | Acceptance is proven, but a required retained effect cannot be reconstructed. | blocked; false; operator_review |
| `history_gap_unrecoverable` | A proven history range is missing. | blocked; false; operator_review |
| `continuity_unproven` | Available evidence cannot establish the latest accepted frontier. | blocked; false; obtain_evidence |
| `deletion_coverage_unproven` | Deletion history is insufficient for safe recovery. | blocked; false; obtain_evidence |
| `authorization_coverage_unproven` | Current permission/revocation history is insufficient. | blocked; false; obtain_evidence |
| `evidence_conflict` | Authenticated evidence is inconsistent. | blocked; false; operator_review |

`convergence` is scoped to the explicitly declared account/stream coverage, and cannot be true while any required gap or pinned unresolved operation in that coverage remains. An operator's acknowledgement of loss does not silently turn an unrecovered history into `recovery_complete`; any separately authorized abandonment/fork has its own explicit disposition.

**DECIDED — SC-6.14.** Rollback detection requires an authenticated continuity checkpoint, committed receipt/journal chain or authorized replica observation outside the rolled-back failure domain. If every distinguishing observation can have rolled back together, no surviving state proves whether later acceptance was lost. Declared restore or uncertain activation without an independent frontier reports continuity_unproven; no unconditional detection guarantee covers an indistinguishable whole-system rollback. A fresh restore marker identifies recovery, not completeness or reconstruction of lost history.

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

**CURRENT CODE** — Exact lifecycle retries preserve an operation ([replay](../../../infra/cloudflare/control-plane/src/account-lifecycle-state.mjs#L827)); live-location commit requires target-resumed phase and precedes route publication ([commit order](../../../infra/cloudflare/control-plane/src/account-lifecycle-state.mjs#L1016), [phase](../../../infra/cloudflare/control-plane/src/account-lifecycle-state.mjs#L1054)). These are allocation/publication seams, not the proposed incarnation witness. Local atomic publication and fsynced operation journals provide persistence patterns ([publish](../../../internal/localcell/files.go#L176), [journal](../../../internal/localcell/files.go#L205)), not account continuity proof.

**CURRENT CODE** — The archive registry is schema-versioned and requires even empty streams ([registry](../../../internal/store/archive_tables.go#L9)); the manifest lacks sync incarnation/frontier proof ([manifest](../../../internal/export/export.go#L60)). Source finalization retains a cell-local receipt after portable deletion ([receipt](../../../internal/store/finalize_evacuation.go#L146)); reverse registry order drives deletion ([order](../../../internal/store/finalize_evacuation.go#L182)). Ordinary import cancels executable request claims and reconciles expiry ([claims](../../../internal/store/import.go#L1526), [expiry](../../../internal/store/import.go#L7064)); provider-started email becomes ambiguous rather than resent ([email](../../../internal/store/import.go#L1428)). Backup validation deliberately never commits ([validation](../../../internal/store/import.go#L6464)). These paths do not establish sync handoff or committed restore.

**CURRENT CODE** — Curation and realm-route cursors encode base64 data ([curation](../../../internal/store/memory_curation.go#L2682), [realm](../../../internal/store/realm.go#L597)); they are not authenticated sync continuations. Email-route canonical signing and Ed25519 are reusable primitives with separate trust scope ([input](../../../infra/cloudflare/control-plane/src/agent-email-route-signature.mjs#L94), [signature](../../../infra/cloudflare/control-plane/src/agent-email-route-signature.mjs#L148)). Token admission and revocation depend on current database rows ([admission](../../../internal/store/auth.go#L186), [revocation](../../../internal/store/auth.go#L542)); an old restored row cannot prove current authorization coverage.

### Open items

| ID | item | owner | blocks |
|---|---|---|---|
| CLOSED O-11 | Restore-marker minting, activation barrier and purpose-specific movement/purge specified by SC-6.6–SC-6.9; authentication and partial-evidence decisions specified with O-12. | planning lead | LC-S5/M4 registry, barrier and recovery proof remain required |
| CLOSED O-12 | Signed continuation/evidence, independent prefix publication and partial-evidence recovery fixed by SC-6.10–SC-6.14 and the evidence/outcome tables. O-7 retains canonical encoding, commitment privacy and proof-format ownership. | planning lead | LC-S5 fixtures and M4 signing, witness, rollback and no-reexecution proof remain required |

### Harness obligations

- **DECIDED (SC-6.1–SC-6.4):** M4 moves authority with a lost ACK, stale
  endpoint, pending local writes and deletion knowledge; reject forged cursors
  and check exact movement or invalidation of every synchronization record.
- **DECIDED (SC-6.2, SC-6.5):** M4 restores older backups with complete,
  partial and absent surviving evidence. Recover only supported effects; the
  absent-evidence case must remain explicitly nonconverged.
- **DECIDED (SC-6.8, SC-6.9):** LC-S5 inventories and M4 tests every portable stream with rows and with zero rows, every invalidation/local exclusion/retention decision, and cross-account validation; required empty streams cannot disappear.
- **DECIDED (SC-6.6, SC-6.8):** Move a committed lost-ACK operation and return its original receipt at the successor.
- **DECIDED (SC-6.7, SC-6.12):** Restore with newer deletion/revocation evidence on a surviving replica; stale payload and revoked grants cannot win.
- **DECIDED (SC-6.6, SC-6.7):** Crash at restore-intent persistence, marker mint, import commit, witness CAS, installation receipt and publication; retries preserve one marker/transition.
- **DECIDED (SC-6.7, SC-6.14):** Exercise database-only and whole-installation rollback, cloned activation grants, live old hosts, absent witnesses and old endpoints after finalization; retain ordinary independent local work during remote outage.
- **DECIDED (SC-6.9, SC-3.8):** Preserve ordinary archive normalization while proving imported sync inspection inert and graph handoff explicit.
- **DECIDED (SC-6.9):** Purge with stale devices/endpoints present; terminal denial outlives replayability without preserving deleted content.
- **DECIDED (SC-6.10):** Mutate each cursor/receipt field, audience, account, replica, enrollment generation, scope, epoch, watermark/root and signature; unknown keys/versions, expired grants and retired devices never become acceptance.
- **DECIDED (SC-6.10, SC-6.11):** Verify old receipts historically while refusing old cursor continuation; retired signers cannot create content outside their sealed prefix even at an apparently allowed numeric position.
- **DECIDED (SC-6.11):** Crash/reorder apply, outbox, witness and ACK boundaries; assert one mutation, unchanged outcome/ID, exact logical accounting and no executable grant before publication. Snapshot/list/show/checkpoint/cursor bypasses leave unpinned commits pending; test behind, equal-conflicting and ahead database/witness frontiers separately.
- **DECIDED (SC-6.12, SC-6.13):** Run all eight R/J/P cases, partial intervals and missing middle atomic groups; replica pending rows are not committed J. Test accepted receipts with input but no result materialization.
- **DECIDED (SC-6.12, SC-6.13):** Recover a create whose later deletion is absent from the backup but proven elsewhere without resurrection; stale enrollment missing newer revocation cannot continue.
- **DECIDED (SC-6.14):** Compare observationally identical surviving datasets, one complete and one missing later history and every witness; declared restore cannot certify completeness from agreement. Record the external-signal limit.
- **DECIDED (SC-6.11, SC-6.13):** Present an independent head ahead of the database, a journal hole despite a larger maximum, and contradictory valid proofs; assert distinct exact gap/continuity/conflict outcomes.
- **DECIDED (SC-6.13, SC-4.7, SC-9.2):** Cross the 37-day floor with unresolved work, an expired grant and missing deletion history; pin unresolved identity/evidence and never close unknown acceptance by rebuilding projections.
- **DECIDED (SC-6.12):** Recover historical request/email completion with zero new claim, dispatch or external effect; preserve original audit/accounting provenance, not just final rows.
- **DECIDED (SC-6.7, SC-6.14):** Restore PGDATA without its local witness, then the whole disk including that witness; prove detection in the first case and the declared non-guarantee/recovery boundary in the second. LC-S5 owns fixtures; M4 supplies runtime proof.

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
| CLOSED O-2 | Stable opaque identities, projection-preserving evidence and typed causal bases fixed by SC-1.15–SC-1.19. LC-S5 specifies migration/projection fixtures; M4 proves preservation; ambiguous preactivation joins remain O-3. | planning lead; LC-S5/M4 for proof | M4 proof; per-family M6 enablement |
| CLOSED O-5 | Durable predecessor fence, successor-bound installation/publication proofs and monotone failed-transfer recovery specified by SC-3.6–SC-3.11. O-6 retains inert/compound projection inventory; O-11/O-12 retain restore/authentication evidence boundaries. | planning lead | LC-S5/M4 implementation and runtime proof remain required |
| OPEN O-6 | Define inert request projections and adversarial compound-publication fixtures for every request lifecycle operation. | LC-S5 | M4 |
| CLOSED O-7 | Canonical JSON/typed payload profile, protected semantic commitments, immutable signed receipt and commitment-keyed refusal format fixed by SC-4.8–SC-4.11. LC-S5 owns codec and fault fixtures; O-12 retains enrollment trust roots, key transition and authenticated historical receipt-frontier proof. | planning lead; LC-S5 for proof; O-12 for continuity | M4 until proof and O-12 are accepted |
| OPEN O-8 | Set bounded queue, fragment and dependency-wait budgets with explicit refusal/recovery fixtures. | LC-S5 | M4 |
| CLOSED O-9 | Semantic transactional journal with database coverage enforcement; activation record v1; relation-lock installation drain and account-gate snapshot barrier selected by SC-5.2 and SC-5.5–SC-5.8. O-10 retains complete writer/role/table inventory; runtime barrier acceptance remains unproven. | planning lead; O-10/LC-S5 inventory; M4 proof owner | M4 proof before M5 |
| OPEN O-10 | Inventory and instrument mixed server, worker, foreground reconciliation and maintenance writers with rollback coverage cases. | LC-S5 | M4 |
| CLOSED O-11 | Restore-marker minting, activation barrier and purpose-specific movement/purge specified by SC-6.6–SC-6.9; authentication and partial-evidence decisions specified with O-12. | planning lead | LC-S5/M4 registry, barrier and recovery proof remain required |
| CLOSED O-12 | Signed continuation/evidence, independent prefix publication and partial-evidence recovery fixed by SC-6.10–SC-6.14 and the evidence/outcome tables. O-7 retains canonical encoding, commitment privacy and proof-format ownership. | planning lead | LC-S5 fixtures and M4 signing, witness, rollback and no-reexecution proof remain required |
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
  records. This revision adopts and closes the six M4-blocking design items
  O-2, O-5, O-7, O-9, O-11 and O-12. Sections 1–9 remain a draft pending
  LC-S5 fixtures and M4 implementation/runtime proof; the residual owners in
  CLOSED rows retain those obligations. Adoption does not mark M0 complete or authorize LC-S5.
- **DECIDED (SC-0.2, SC-6.13, SC-6.14):** D9 records the pending self-managed disaster-recovery durability question; the default never certifies continuity-unproven recovery. Ask when it blocks M5.
- **DECIDED (SC-0.2):** Every unresolved item is listed with owner and blocked
  milestone or slice in [Open items by milestone](#open-items-by-milestone).
  A milestone cannot pass while one of its contract gates remains open.
- **DECIDED (SC-0.1, SC-2.1, SC-5.1, SC-6.1, SC-9.5):** Disposable synthetic
  identities may exercise the sections' harness obligations. Durable customer
  data remains prohibited until identity/schema preservation or migration and
  the applicable enrollment/security contracts are demonstrated.
