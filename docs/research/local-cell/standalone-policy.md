# Standalone identity and policy contract

This B1-POLICY design is a **PROPOSED CONTRACT**, dated 2026-11-07 and reviewed
against dispatch HEAD `dbacf2a1`. It awaits planning-lead review. It selects no
shipping capacity, retention duration, credential lifetime or offline horizon.
The [owner questions](#open-owner-questions) are deliverables, not answers or
permission to apply policy. This document supplies neither bootstrap
implementation nor M0, M2, M3 or M9 acceptance.

## Status and decision provenance

The labels in this document have distinct meanings:

- **CURRENT CODE** describes inspected implementation, not standalone readiness.
- **RECORDED DECISION** preserves an existing dated decision or plan requirement.
- **PROPOSED CONTRACT** describes behavior for later review and implementation.
- **OPEN OWNER QUESTION** identifies a choice requiring the named owner's evidence
  and decision before its dependent implementation or acceptance.

**RECORDED DECISION:** [M0b](implementation-plan.md#m0b-standalone-identity-and-policy-contract)
requires separate installation/account identities, independent console trust,
explicit standalone policy and offline messaging. [M2](implementation-plan.md#m2-make-standalone-use-complete-enough-for-a-canary)
requires real local reads, client-driven curation, message workflows and recovery
without silent remote fallback. The [ownership table](implementation-plan.md#L940-L959)
assigns standalone policy to the planning lead and destination admission and
resource/canary choices to the founder.

**RECORDED DECISION:** [D1–D8](implementation-plan.md#founder-decisions-answered)
remain answered: D1 authorizes the initial runtime work; D2 preserves adoption
namespaces and requires reviewed joins; D3 starts with owner-authorized personal
replicas, explicit stale state and no remote-erasure promise; D4 keeps vault keys
client-held and requires a threat model before whole-account enrollment; D5
requires bounded reporting that cannot gate local work; D6 defers the existing
hosted-free transition until after M9; D7 removes any SaaS subscription
prerequisite for local messaging or self-managed enrollment; D8 fixes the platform
selection while leaving platform budgets to their owners. D1 was answered on
2026-10-23 and D2–D8 on 2026-10-24. The [F2/F4 YES records](implementation-plan.md#L1185-L1191)
of 2026-11-02 concern an immutable CI predecessor and an opt-in proof-worker
profile; they do not select local policy values. [D9](implementation-plan.md#L1213)
remains pending. None of these decisions is reopened here.

**RECORDED DECISION, dispatch provenance:** the 2026-11-07 owner notes commission
this one question-bearing document, retain ledger/catalog/scorecard ownership
with DOCSROW-B1, and supersede the older digest's part-C2 scheduling hold. That
digest is not a new authority gate or a new founder poll. B1-MESSAGE and B1-READ
may use explicitly synthetic policy fixtures; such fixtures do not choose the
shipping policy.

## Identity and bootstrap boundaries

**CURRENT CODE and RECORDED DECISION:** the [plan's terms](implementation-plan.md#terms)
separate adoption, namespace join and enrollment. The identities below must not
be collapsed merely because one installation initially hosts one account.

| Identity | Boundary and evidence | PROPOSED CONTRACT |
| --- | --- | --- |
| Installation | Managed runtime instance; the manifest already has distinct installation/account fields ([manifest](../../../internal/localcell/types.go#L54-L71), [allocation](../../../internal/localcell/lifecycle.go#L95-L115)). | Preserve the installation identity across restart; a copied account or endpoint is not proof of this installation. |
| Account | Durable ownership namespace; default-account seed returns persisted identity ([seed](../../../internal/store/seed.go#L13-L43)). | Bind policy and principals to the exact account. Adoption preserves it; a join requires reviewed collision resolution. |
| Realm | Account-owned namespace ([realm](../../../internal/store/realm.go#L99-L127)). | Scope agent data and local routing to the admitted realm; a shared display name grants nothing. |
| Operator | Separate account-management principal ([operator](../../../internal/store/operator.go#L37-L50), [root seed](../../../internal/store/seed.go#L46-L77)). | Verify operator identity/role independently; an agent credential never becomes an operator credential. |
| Agent | Realm-scoped principal whose realm must belong to its account ([creation](../../../internal/store/agent.go#L25-L43)). | Bind data ownership and mailbox actions to authenticated account/realm/agent, not caller-supplied identity. |
| Device | Enrolled participant, distinct from installation and account ([terms](implementation-plan.md#L125-L142)). | Admission and revocation belong to later enrollment; local startup alone grants no connected device authority. |
| Replica | Synchronized copy of an account namespace, distinct from the participant and from live authority ([terms](implementation-plan.md#L125-L142)). | No implicit replica enrollment or imported live lease authority; preserve identities under the [connected transition contract](sync-contract.md#2-identity-transitions-and-seamless-continuity). |

**CURRENT CODE:** [server startup](../../../cmd/witself-server/main.go#L339-L393)
seeds/resolves the default account, configures local readiness, seeds/resolves the
root operator, optionally adopts bootstrap material and installs distinct
operator/principal authentication paths. These steps do not form an atomic
standalone identity-and-policy bootstrap. Seeding applies no standalone policy.
The [bootstrap resolver](../../../cmd/witself-server/main.go#L2358-L2377) accepts
explicit file or environment configuration before local mode suppresses the
ambient deployment-file fallback. Local mode therefore does not reject all
explicit bootstrap material. [Re-adoption and exchange](../../../internal/store/auth.go#L82-L170)
already preserve token expiry on identical re-adoption and atomically exchange a
valid bootstrap token for an operator token; these mechanics do not settle Q02.

**CURRENT CODE:** [readiness](../../../internal/runtimeconfig/readiness.go#L33-L58)
checks database, schema and active default-account binding, not standalone policy
or completion of end-user operator bootstrap. The lifecycle [reopen binding](../../../internal/localcell/lifecycle.go#L206-L220)
refuses a changed account. The [service-health operator credential](../../../internal/localcell/service_health.go#L149-L159)
is a service mechanism, not a completed end-user admission contract.

**CURRENT CODE:** [route setup](../../../cmd/witself/account_route.go#L73-L107)
probes version compatibility without credentials. The [console resolver](../../../cmd/witself/account_console.go#L23-L78)
separately verifies account, origin, operator credential and unchanged route
metadata; explicit endpoint/token-file invocations remain agent-only. The
[client verifier](../../../internal/client/account_console.go#L61-L128) accepts
loopback HTTP as an origin shape but requires an operator of the exact account
and revalidates that binding. A loopback origin is not authority. The
[inventory path](../../../cmd/witself/account_console.go#L136-L164) uses verified
account and captured roots; offline inventory proof remains an M2 obligation.

### Proposed bootstrap state sequence

**PROPOSED CONTRACT:** absent → partially bootstrapped → ready; interruption from
an incomplete transition enters interrupted; reopening persisted state validates
it before returning to ready. These are proposed product states, not current
readiness enums. The durable journal/receipt and admission mechanism below are
new proposals whose schema and authorization are Q01/Q02.

| State | Actor and durable evidence | Allowed operations | Refusal and recovery |
| --- | --- | --- | --- |
| Absent | Prospective local owner through the reviewed admission mechanism; no validated installation/account/bootstrap receipt yet. | Bounded status and explicit initialization under Q02. | No domain or operator authority from path ownership/loopback alone. Reject foreign or ambiguous persisted state rather than adopting it silently. |
| Partially bootstrapped | Admitted bootstrap actor bound to one installation and account; durable progress records identify completed identity, operator and policy steps. | Inspect value-free progress; resume only missing, authorized steps. | Domain operations remain unavailable until identities, operator admission and explicit policy are committed together as ready. Never reseed into a second namespace to hide partial state. |
| Ready | Separately authenticated operator or agent; exact installation/account binding, completed admission receipt and supported applied policy. | Only the actor's operations in the [operation matrix](#standalone-operation-matrix). | Wrong scope/origin/credential class or disabled feature is refused before operator/domain work. Account/policy mismatch leaves explicit recovery state. |
| Interrupted | Same authorized recovery actor under Q02; retained progress, prior policy and last committed receipt, with incomplete work identified. | Read recovery status; retry the same transition or an explicitly reviewed repair. | Preserve committed data and identities. No new root identity, silent default policy, credential-expiry extension or automatic remote fallback. Ambiguous authority cannot resume writes. |
| Reopened | Runtime validates durable evidence before admitting any authenticated client; operator recovery is separately authorized. | Read status; resume normal operations only after identity, schema, admission and policy checks succeed. | Reuse the same committed identities and policy. Missing/unsupported policy or changed binding refuses governed operations; preserve evidence and require an authorized repair. |

The implementation gap is a durable bootstrap completion boundary spanning
identity, admitted authority and explicit policy, plus interruption/reopen
classification. Existing seed, lifecycle and console checks are reuse seams,
not that completion boundary. Later M5/M7 enrollment must preserve local
identities, authenticate the destination and obtain destination authorization;
it must not repair collisions with automatic remapping or transfer a local
operator's authority merely because an endpoint or display name matches.

## Snapshot identity and authority

**CURRENT CODE:** [AccountPlanSnapshot and application](../../../internal/store/plan.go#L56-L160)
contain account, revision, hash, plan label, limits, policies, features and
application time. The cell accepts resolved state rather than consulting a
catalog. Positive revisions fence stale writers and accept equal revision/hash
replays; replay currently refreshes application time. The legacy revision-zero
shape is accepted only before a fenced snapshot. Applying the account row is
atomic; it is not an identity/bootstrap transaction.

**CURRENT CODE:** the [digest](../../../internal/plans/plans.go#L675-L706) covers
plan label, limits, policies and sorted features, not account or installation
identity. It proves payload integrity, not entitlement. The schema has no
standalone policy identity/version/authority-source field. The [hosted Personal
row](../../../web/plans/plans.json#L13-L48) is hosted product configuration, not a
local-policy template; its values are not copied here.

**CURRENT CODE:** [messaging interpretation](../../../internal/store/message.go#L257-L321)
allows active unmanaged accounts with no applied snapshot and applied legacy
snapshots lacking the messaging marker. With the marker present, its supported
version and the messaging feature are required. [Collaboration interpretation](../../../internal/store/message.go#L335-L371)
inherits messaging when its marker is absent; when present it additionally
requires an applied snapshot, supported version and collaboration feature.
Collaboration still requires basic messaging. These are deliberate legacy
allowances, not the new standalone fail-closed rule. Nil maps and omitted caps
currently mean no caps, and a [missing secret limit](../../../internal/store/secret_limit.go#L66-L87)
is unlimited.

### Proposed snapshot envelope

**PROPOSED CONTRACT:** require an explicit, complete standalone envelope before
governed local operations. The symbolic fields below are design names, **not new
validated API keys**, runnable configuration or a migration recipe. No row
selects a shipping value; Q01 decides the authority/distribution mechanism.

| Proposed field group | Required meaning and unresolved symbol | Existing seam or gap |
| --- | --- | --- |
| Standalone identity | `standalone_policy_id = POLICY_ID`, `policy_version = POLICY_VERSION`, `envelope_schema = ENVELOPE_SCHEMA`; distinct semantic version and wire compatibility. | New fields; neither a hosted plan ID nor an account/installation ID. |
| Scope binding | Exact `installation_id`, `account_id`, standalone mode and permitted scope. | Existing identities; binding them into the accepted policy receipt is proposed. A digest alone supplies no binding authority. |
| Provenance and admission | `authority_source = AUTHORITY_SOURCE`, authorized issuer/applying operator, distribution provenance and approval reference. | New authenticated provenance contract under Q01/Q02; a self-declared issuer string is insufficient. No signing scheme is selected here. |
| Revision and integrity | Application revision, expected prior revision and content digest, with an immutable accepted-payload receipt. | Reuse revision/conflict concepts; extend integrity coverage to the reviewed standalone envelope. Receipt schema and replay timestamps need review. |
| Feature decisions | Explicit basic messaging and independent collaboration decision, supported entitlement markers, explicit unsupported hosted families. | Existing feature/marker vocabulary; M0b requires standalone messaging. Request enablement remains Q05. |
| Capacity decisions | Every required dimension explicitly reviewed as finite `CAPACITY(dimension)` or expressly approved unlimited, with `COUNT_SCOPE(dimension)` and over-limit behavior. | Existing omission means unlimited; the new completeness validator must distinguish reviewed unlimited from missing input before any translation to existing maps. |
| Retention and recovery | `RETENTION(family)`, reference/history treatment, recovery compatibility, policy-change rules and `STALE_POLICY_RULE`. | Existing retention keys cover only part of this contract; new semantics require Q06/Q08 and must not be inserted into closed maps as invented keys. |
| Operator-visible receipt | Applied policy identity/version/revision, scope, provenance, compatibility, completeness, effective gates and reason for refusal. | Existing agent [entitlement projection](../../../cmd/witself-server/plan_entitlements.go#L12-L62) is not a standalone operator status API. |

**PROPOSED CONTRACT:** validate issuer authority, exact scope, supported versions,
completeness and vocabulary before applying. Persist envelope, effective gates
and ready-state evidence as one atomic transition, ordered against governed
operations. A failed application preserves the last committed policy and data.
Replay of an identical accepted request returns the same effective policy and
durable identity; a reused revision with different content or an older revision
is a conflict. Receipt timestamp semantics remain Q08 rather than a claim of
byte-identical replay in current code.

On restart, load the persisted policy; do not download a hosted default, infer a
policy from a plan label or regenerate it from today's catalog. Missing,
incomplete, invalid or unsupported policy refuses governed domain operations
while retaining a bounded, authenticated diagnostic/recovery path. Such refusal
must not delete data, advertise ready status, grant legacy unlimited access or
silently route to hosted services. Proposed operator states are missing,
incomplete, unsupported, invalid, applied and transition-refused; their exact
public vocabulary and visibility are Q01/Q09. A separate status must distinguish
runtime reachability from policy readiness.

Upgrades, binary downgrades, policy rollback, capacity reductions, feature
disablement with in-flight work, stale-policy horizons and offline recovery are
Q08. Preservation of committed data is the proposed invariant; grace periods,
which correction operations remain allowed, and compatible recovery paths are
not selected. No expiry rule may trigger silent hosted fallback.

**RECORDED DECISION:** [destination admission](sync-contract.md#8-destination-enrollment-and-entitlement-policy)
re-authorizes account ownership, device/replica, permissions, protocol, capacity
and features at the destination. Local policy is never SaaS entitlement, paid
plan authority or provider authority. D7 remains settled. This document
authorizes no policy upload, billing change, remote activation or sync work.

## Standalone operation matrix

**PROPOSED CONTRACT:** support below means the intended standalone boundary,
conditional on the explicit snapshot and later proof. It is not an implementation
receipt. Each family inherits exact installation/account binding, authenticated
scope and the bootstrap refusal rules. Reads do not bypass ownership; a read
that reconciles expiry is also a write. No backend ranks offers, infers curation
actions, launches an idle client or wakes an offline agent.

| Family and disposition | Authenticated actor; read/write authority | Feature/capacity gate | Conflict, fencing and idempotency | Deletion/retention | Offline dependency and CURRENT CODE evidence |
| --- | --- | --- | --- | --- | --- |
| Transcripts and references — proposed supported | Owning agent in account/realm; capture, append and scoped reads/reference resolution. | Explicit policy; Q06 retention and Q10 storage/retrieval budgets; no invented transcript-count key. | Locked transcript, live scope, exact external-entry retry; conflicting retry fails. | Whole-conversation retention, evidence/active-curation holds and terminal pointer pruning exist; Q06 must declare survival guarantees. | Local store/client only; no hosted lookup required by proposal. [Append](../../../internal/store/transcript.go#L204-L300), [retention](../../../internal/store/transcript_retention.go#L113-L129), [pruning](../../../internal/store/transcript_retention.go#L570-L616). |
| Fact assertions/history — proposed supported | Agent owns its subject namespace; scoped assertion, resolution and history. | Explicit policy; `stored_fact`, Q04 count scope/history. Catalog feature labels alone do not prove a store admission gate. | Namespace serialization and exact replay; correction must not become a second fact by remapping. | History is distinct from current-fact capacity; no history horizon selected. | Local store/client; [set/replay/capacity](../../../internal/store/fact.go#L156-L227), [counter semantics](../../../internal/plans/plans.go#L90-L94). |
| Fact permanent deletion — proposed supported under existing explicit-user boundary | Owning agent transports direct current-user deletion authority; a data record or curation action is not authority. | Valid policy and authorized delete; at/over-cap behavior must be expressly retained in Q04/Q08. | Preview then exact fact/assertion/candidate revision and idempotency fence. | Purge values/assertions/candidates; retain value-free tombstone, subjects/aliases and usage/audit. No export/backup purge promise. | Local transaction; [delete contract](../../../internal/store/fact_delete.go#L20-L58), [apply guards](../../../internal/store/fact_delete.go#L91-L110), [MCP user-authority check](../../../cmd/witself/mcp.go#L1377-L1410). |
| Memory capture/recall/evidence — proposed supported | Owning agent; scope-filtered recall and evidence operations, sensitive-read rules retained. | Explicit policy; `stored_memory`, Q04 active-head/history scope; Q10 retrieval budget. | Serialized capture/replay, version checks for mutation; recall cursor bound to filters and consistent snapshot. | Forgotten/superseded/history state is not permanent deletion; evidence lifecycle remains explicit. | Client authors meaning; local store retrieves. [Capture](../../../internal/store/memory.go#L348-L461), [recall](../../../internal/store/memory_recall.go#L123-L182), [counts](../../../internal/plans/plans.go#L85-L89). |
| Memory permanent deletion — proposed supported under existing explicit-user boundary | Owning agent with direct current-user authority established at transport; not autonomous curation. | Valid policy; Q04/Q08 exact recovery/delete availability. | Preview/apply, expected version, scrub-set revision and durable retry; live dependencies can block. | Physical scrub is scoped; no silent cascading dependency deletion or remote-erasure/export/backup promise. | Local transaction; [authority and guards](../../../internal/store/memory_delete.go#L79-L140). |
| Client-driven curation — proposed supported | Authorized owner-agent lane and exact request/run scope; client reads all pages, synthesizes, reviews and submits actions. | Explicit policy, resulting active-memory capacity and bounded plan language; fact proposals are uncounted review candidates, with later confirmation separately gated. Candidate/history storage remains Q04; expiry is Q06. | Exact run generation/lease and accepted plan revision/hash; apply and cursor advance together; stale/expired authority refused or reconciled. | Reversible plan actions only; permanent deletion is a separate operation. Retain reviewed provenance; no unseen-input advancement. | Active client required for inference and invocation; [allowed actions](../../../internal/store/memory_curation_plan.go#L392-L446), [fact-proposal boundary](../../../internal/store/memory_curation_plan.go#L192-L194), [input paging](../../../internal/store/memory_curation.go#L2065-L2143), [apply/fence](../../../internal/store/memory_curation_apply.go#L180-L249). |
| Ordinary messages/claims — proposed supported | Authenticated sender and recipient in admitted realm; recipient-only claim and recipient-scoped acknowledgement; processing completion/results require the exact claim. | Explicit `messaging` plus supported marker; three rate scopes, Q05 storage/burst and Q06 retention. | Immutable delivery scope, idempotent live claim, generation/lease renew/complete; stale claims cannot take later work. Acknowledgement refuses an actively claimed delivery. Request protocol messages are excluded from ordinary claims. | Receipt/cancellation and retained history follow reviewed Q06; expiry is not permission to erase live work. | Local mailbox and active recipient client; no SaaS or wakeup dependency. [Delivery](../../../internal/store/message.go#L375-L378), [claim/renew](../../../internal/store/message.go#L504-L658), [complete](../../../internal/store/message.go#L744-L875), [acknowledgement](../../../internal/store/message.go#L1279-L1318), [protocol exclusion](../../../internal/store/message.go#L1873-L1898). |
| Request/offer/selection/assignment/result — proposed support, enablement OPEN Q05 | Coordinator/candidates/selected agent have distinct visibility and mutation rights; client ranks offers and chooses selection. | Explicit collaboration marker/feature in addition to messaging; basic messaging grants no implicit request authority. | Request graph and reservation/processing fences; client selection persisted/validated, not chosen by backend. Keep protocol lifecycle separate from ordinary claims. | Foreground reconciliation can expire/cancel fences even without a worker; graph/reference retention is Q06. | Local store plus active coordinator/agents; [visibility](../../../internal/store/message_request.go#L648-L650), [selection](../../../internal/store/message_request.go#L1778-L1822), [processing lease](../../../internal/store/message_request.go#L2190-L2225), [expiry](../../../internal/store/message_request.go#L1023-L1038). |
| Vault/sealed data — canary promise OPEN Q07 | Complete owner-agent principal with full access profile; client encrypts/decrypts, server stores ciphertext and metadata. Operator inventory is not permission to decrypt. | Explicit reviewed vault decision and `stored_secret`; no missing-limit inference. | Owner/key binding, rotation checks, revision/idempotency guards; preserve failure on absent/mismatched client key. | Active and archived top-level secrets count; archive does not release capacity; scoped delete scrubs values with minimal tombstone. Key recovery and remote erasure not promised. | Local client-held key is necessary; no backend crypto inference. [Boundary](../../../internal/store/secret.go#L14-L22), [principal](../../../internal/store/secret.go#L337-L343), [create/key binding](../../../internal/store/secret_mutation.go#L129-L209), [lifecycle](../../../internal/store/secret_mutation.go#L287-L307). |
| Account console/access/inventory — proposed supported, bootstrap/proof OPEN | Verified account operator and role; separately scoped agent views. Read local inventory from captured roots only after trust checks. | Q02 admission and operator enrollment; `operator_seats`, Q03; Q09 unsupported surfaces. | Revalidate origin/account/operator and reject stale route metadata; route changes confer no new authority. | Inventory is not account deletion or credential recovery; lifecycle/history policies OPEN Q02/Q06. | Trusted local route without directory dependency; [resolver](../../../cmd/witself/account_console.go#L23-L78), [inventory](../../../cmd/witself/account_console.go#L136-L164). |
| Local status — proposed supported | Bounded public liveness; authenticated operator for policy/installation detail, agent for own effective gates. | Diagnostic access survives policy refusal without enabling domain writes; disclosure vocabulary OPEN Q09. | Read exact persisted receipt/revision; distinguish unsupported policy from ready runtime. | No implicit repair, purge or retention mutation in status. | Local disk/store only; [current readiness](../../../internal/runtimeconfig/readiness.go#L33-L58), [agent projection](../../../cmd/witself-server/plan_entitlements.go#L12-L62); operator policy projection is a gap. |
| Hosted-only billing, email, support and reporting — unsupported standalone service operations | Local credentials authorize no hosted subscription/payment, email dispatch/admission or managed-support entitlement. Future reporting is separately disclosed/authorized. | Explicit unsupported disposition; never infer these features from messaging or vault access. Reporting activation remains M8/D5. | Refuse before provider work; no hidden request, fallback or policy upload. Destination-specific admission remains independent. | Existing locally retained history keeps its scoped read/reference contract; no new hosted retention/removal or support-service promise. | Core local operations cannot depend on these endpoints. [Runtime exclusions](../../../internal/runtimeconfig/config.go#L33-L56), [destination differences](sync-contract.md#L808-L822), [reporting gate](implementation-plan.md#L773-L800). |

### Current vocabulary and retention gaps

**CURRENT CODE:** [capacity definitions](../../../internal/plans/plans.go#L66-L94)
count live operators including root and live realms account-wide; root seeding is
outside the seat gate. `agents` is the legacy account-wide live-agent cap;
`agents_per_realm` counts each live realm independently. `stored_fact` counts
current resolved addresses per owner agent, excluding assertions/candidates and
history; `stored_memory` counts active heads per owner agent, excluding historical,
superseded, forgotten and deleted memories; `stored_secret` counts active and
archived top-level secrets per owner agent, excluding deleted secrets. An
account-wide replacement interpretation would be a new contract, not a relabeling.
[Facts](../../../internal/store/fact_limit.go#L110-L129) and
[memories](../../../internal/store/memory_limit.go#L125-L144) already admit
non-growing mutations at/over cap. This does not settle every standalone
transition or the treatment of uncounted retained history.

**CURRENT CODE:** the three [message rate keys](../../../internal/plans/plans.go#L128-L141)
are `message_sent_per_agent_minute`, `message_delivered_per_realm_minute` and
`message_delivered_per_recipient_minute`. Fan-out charges each recipient.
[Rate admission](../../../internal/store/message_rate_limit.go#L124-L171) debits
only new messages transactionally; [omission](../../../internal/store/message_rate_limit.go#L192-L196)
still leaves a defensive platform maximum. Separate burst or message-storage
policy keys are absent from the [closed vocabulary](../../../internal/plans/plans.go#L302-L346).
[Limit validators](../../../internal/plans/plans.go#L550-L625),
[message validity/lease bounds](../../../internal/store/message.go#L1909-L1926)
and [request validity bounds](../../../internal/store/message_request.go#L270-L311)
are representation/protocol limits, not chosen standalone capacities or TTLs.

**CURRENT CODE:** `transcript_retention_days` and `message_retention_days` are
[existing policy keys](../../../internal/plans/plans.go#L242-L267). Omission
currently means indefinite retention; [validators reject zero](../../../internal/plans/plans.go#L628-L655).
No zero sentinel is proposed. Message retention remains meaningful when messaging
is disabled. Transcript retention already holds evidence and active frozen
curation inputs and prunes terminal pointers. [Message retention](../../../internal/store/message_retention.go#L134-L143)
uses whole threads with [live-claim/graph holds](../../../internal/store/message_retention.go#L717-L766)
and [evidence protection](../../../internal/store/message_retention.go#L836-L862).
But the [local job inventory](../../../internal/runtimeconfig/config.go#L46-L56)
currently makes transcript/message retention workers ineligible. Foreground
request expiry and [curation expiry](../../../internal/store/memory_curation.go#L804-L882)
still mutate state. A retention duration alone cannot promise offline cleanup,
reference survival or a wakeup schedule. Q06 must resolve this gap.

## Open owner questions

Every row is an **OPEN OWNER QUESTION**. Its stable ID is local to this document.
The planning lead owns technical synthesis and the founder owns product/threat
choices noted below. Symbols remain unresolved until a dated answer and its
evidence are reviewed; omission is not an answer of unlimited.

| ID and accountable role | Question and affected fields | What it blocks; evidence needed |
| --- | --- | --- |
| Q01 — planning lead; founder approves distribution authority | What explicit `POLICY_ID`, `POLICY_VERSION`, `ENVELOPE_SCHEMA` and `AUTHORITY_SOURCE` ship; who may apply/replace them, and how are unknown versions presented? Which authenticated provenance binds installation/account/revision, beyond content digest integrity? | Blocks the snapshot validator, issuer/apply boundary and operator status. Need reviewed wire/provenance design, issuer threat scenarios, compatibility/error examples and replay/unsupported-version cases. |
| Q02 — planning lead; founder approves admission/recovery | What local owner/bootstrap admission, additional operator/agent enrollment, credential expiry/revocation and interrupted-bootstrap recovery are supported? Which durable admission/ready receipt and authorized repair actors apply? | Blocks usable bootstrap and console acceptance. Need interruption/lost-response/reopen cases, wrong-account/origin/agent denials, key/credential custody boundaries and a recovery threat review; established origin checks do not answer end-user bootstrap. |
| Q03 — planning lead; founder approves capacities | Which finite `CAPACITY(dimension)` or explicitly reviewed unlimited values apply to `operator_seats`, `realms`, `agents`, `agents_per_realm`; what scopes and over-limit behavior are intended, including the root-seed exception? | Blocks membership provisioning and reduction acceptance. Need representative local cohorts, per-dimension count examples, concurrent creation proof and a reviewed relationship between legacy account-wide and per-realm agent caps. |
| Q04 — planning lead; founder approves capacities/history budget | What `stored_fact`, `stored_memory`, `stored_secret` values and per-agent versus account scopes are chosen? Which correction/update/delete operations work at/over cap; how is retained history bounded? Current secret counting is per owner agent and includes archived entries. | Blocks data/vault capacity enforcement acceptance. Need create/correct/delete/recreate and archive/restore cases, counted versus uncounted history evidence, quota-reduction cases and disk-cost measurements. Changing count scope requires an explicit implementation proposal. |
| Q05 — planning lead; founder approves messaging bounds | What values apply to the three supported message rate keys, local burst and retained-storage bounds, and independent request-workflow enablement? How are new burst/storage semantics represented, if needed, without inventing already-supported keys? | Blocks messaging/collaboration policy and saturation acceptance. Need fan-out/retry accounting, concurrent claim/request fixtures, storage-growth measurements and reviewed handling of in-flight work on disablement. Fixed API validity/lease bounds do not select product policy. |
| Q06 — planning lead; founder approves retention contract | What `RETENTION(family)`, transcript/message policy, tombstone/history retention and reference-survival rules apply offline? Which workers become eligible, what remains foreground expiry, and what happens while no client/runtime runs? | Blocks retention/reference acceptance. Need evidence/active-plan/live-claim holds, terminal pruning, delete/retry/restore fixtures, worker eligibility review and explicit disclosure of unavailable references. No unsupported zero sentinel or inferred indefinite choice. |
| Q07 — planning lead; founder owns D4/M5 threat boundary | Which vault operations and threat boundary are promised for the standalone canary; which key custody, loss, rotation, recovery and enrollment questions wait for D4/M5? | Blocks a vault canary claim, not the already-decided client-held-key boundary. Need synthetic sealed-data/key-mismatch cases, threat review and a scoped recovery statement. No expansion into whole-account enrollment or remote erasure. |
| Q08 — planning lead; founder approves recovery/horizon | What update, upgrade/downgrade, rollback, capacity-reduction, feature-disable and stale-policy rules are acceptable offline? What is `STALE_POLICY_RULE`; what recovery/correction remains available; what are replay receipt/timestamp semantics? | Blocks transition/reopen acceptance. Need compatibility matrix, stale writer/lost response/interrupted apply and over-cap fixtures, operator-visible refusal cases and data-preservation proof. Choose no horizon or grace period here; no hosted fallback on expiry. |
| Q09 — planning lead; founder approves product wording | Which hosted-only email, billing, managed support and reporting surfaces are unavailable, and what exact local status/error vocabulary and disclosure scope explain that and destination-specific admission? | Blocks truthful console/status/help and unsupported-feature acceptance. Need API/UI refusal examples, blocked-network traces and destination authorization fixtures. D7's no-subscription prerequisite is settled, not a new question. |
| Q10 — founder; planning lead coordinates D8/M9 owners | Which per-platform storage/resource/retrieval budgets, representative fixtures, canary durations and expansion thresholds remain outside this policy contract? | Blocks relevant [M9 cohorts](implementation-plan.md#m9-expand-canaries-and-meet-the-support-gate), not writing this document. Need named platform baselines, measured retrieval correctness/latency and approved pass/fail criteria under D8/M9. No invented benchmark values and no closure of D9. |

## Future hermetic acceptance

All tests below are **PROPOSED, NOT IMPLEMENTED/NOT RUN**. Names are reserved
design expectations, not links to nonexistent tests. B1-POLICY adds no test or
fixture. The planning lead must allocate later implementation owners; B1-MESSAGE
and B1-READ can supply synthetic evidence without selecting policy values.

| Proposed test | Exact expected assertion | Later owner and prerequisite |
| --- | --- | --- |
| `TestStandalonePolicyBootstrapReplay` | Interrupt at each durable boundary and repeat bootstrap: the same installation/account/realm/operator/agent identities and accepted policy survive; incomplete authority never becomes ready and replay never extends credential authority implicitly. | M0b/M2 bootstrap implementer under planning lead; Q01/Q02. |
| `TestStandalonePolicyExplicitSnapshot` | Missing, incomplete, invalid or unknown standalone policy refuses governed operations and cannot inherit unmanaged unlimited/legacy messaging or collaboration allowance; bounded recovery status remains available. | M0b policy/store implementer; Q01/Q03–Q06. |
| `TestStandalonePolicyAuthorityIsolation` | Agent-only, wrong-account and wrong-origin requests fail before operator work; loopback/version success and stale route metadata cannot grant operator authority. | M2 console/bootstrap implementer; Q02/Q09. |
| `TestStandalonePolicyOfflineMessaging` | With hosted endpoints blocked and calls instrumented, explicitly enabled messaging/collaboration follows the local snapshot with no hosted call; disabling collaboration leaves no implicit request grant, and ordinary claims cannot acquire protocol messages. | B1-MESSAGE evidence owner, then M2 policy integration owner; Q05/Q06. |
| `TestStandalonePolicyCapacityTransitions` | Create is blocked at the reviewed cap, reviewed correction/delete behavior is exact at/over cap, concurrent admission cannot exceed it, and reduction preserves all committed data with explicit over-limit status. | M0b/M2 capacity implementer; Q03/Q04/Q08. |
| `TestStandalonePolicyReopen` | Persist, stop and reopen offline: durable identities and effective policy remain unchanged; unsupported versions refuse governed work without erasure, reseeding, implicit upgrade or hosted fallback. | M2 bootstrap/policy implementer, M3 compatibility owner; Q01/Q08. |
| `TestStandalonePolicyNotHostedEntitlement` | A destination refuses standalone policy as entitlement even when its content is internally valid; separate destination admission/authorization is required and refusal preserves independent local work. | M5/M7 destination-admission owner; Q01/Q09 and destination contract. |
| `TestStandalonePolicyRetentionReferences` | Reviewed expiry/permanent-deletion behavior preserves declared evidence/reference holds or exposes the declared unavailable/pruned state; live authority cannot be silently removed, and API retry/logical lifecycle restore cannot resurrect deleted values while the current-state deletion receipts are retained. Older-backup recovery remains Q08/D9. | B1-READ evidence owner, then M2 retention owner; Q06/Q08. |

Every future home-touching test must bind `WITSELF_HOME`, `DSH_HOME` and applicable
process home roots to `t.TempDir()`. Credentials/data are synthetic and isolated;
receipts cite synthetic fixture locations by `file:line` without displaying
literals. No real home, hosted credentials, native services or production
database are proof inputs. Future source contracts that read multiline source
must normalize CRLF and replay against CRLF bytes. This design introduces none.

## Validation and limits

The executable oracle is [make check-docs](../../../Makefile#L400-L402), its
[link/anchor/line checks](../../../scripts/check-docs.mjs#L410-L437) and the
[runner](../../../scripts/run-docs-check.mjs#L84-L105). It checks document
structure, local target existence, heading anchors, source-line bounds and
whitespace. It cannot establish source semantics, policy correctness, issuer
authority or runtime readiness. Linked ranges were re-resolved at dispatch HEAD
and read for meaning; future changes can leave an in-range citation semantically
stale. See the [docs gate guide](../../docs-validation.md#shared-checks).

B1-POLICY validation includes an unmutated pass, separate missing-target,
missing-heading and out-of-range-source-link mutations of this document, exact
byte restoration after each, and a final unmutated pass. The worktree report
records actual diagnostics and command statuses; this paragraph makes no claim
that future runtime tests ran. Owner CI uses the [docs job](../../../.github/workflows/ci.yml#L46-L65),
step `Validate documentation`. The owner relocates the root report before
classifying the committed candidate as docs-only.

No Go package, test, schema, generated contract, ledger, catalog, scorecard,
workflow or committed-cell reader changes here. The Go build/vet/tidy/test/lint
gates, shell suites, PostgreSQL and feature-status generation are not run in
this slice. No roll-state proof is triggered. M0/M2/M3/M9 remain open, and this
document does not promote sync obligations, close D9 or approve policy numbers.
Windows behavior is proven only on CI; this document supplies no Windows or
macOS runtime receipt. It includes no systemd/launchd query or native macOS
proof; launchd hermetic fakes and native CI evidence belong to their own lanes.
