# Fable handoff: WitSelf Local Cell

[WitSelf running locally](README.md)

This is the reusable planning assignment for **Fable 5**. Give Fable the link to
this document on `main`; it contains the read order, scope and shared tracking
instructions. The requirements and implementation plan remain authoritative for
product scope and execution order. This handoff does not claim that the planned
features or acceptance tests have been implemented.

## Assignment and authority

You are Fable 5, the planning lead for **WitSelf Local Cell**. Thoroughly evaluate
the existing design and relevant code, challenge assumptions with evidence, and
refine the existing repository plan and documentation. Keep priorities,
dependencies, accepted decisions and acceptance criteria coherent.

Start from freshly fetched `main` in your own isolated worktree, after reading
the repository instructions, including [CLAUDE.md](../../../CLAUDE.md). Verify
the actual checkout and record the source revision you reviewed. Do not reuse
another task's validation checkout or disturb concurrent work on this machine.

This assignment authorizes research, review, planning and documentation edits.
It does not authorize feature implementation, pushing or merging, release,
publication, deployment, device enrollment, or live account, billing or policy
changes. Follow separately granted authorization and repository gates for any
later action. A proposed design or plan entry is not that authorization.

Future implementation may alternate between **Opus 5.5** and **GPT-6 Astra**
after Scott separately authorizes the implementation work. Fable remains the
planning lead; model names identify intended collaborators, not exclusive tool
capabilities or permission to launch them. Every agent uses the same repository
record so progress does not depend on this chat or a previous model's hidden
context.

## Read and verify

Read the [Local Cell documentation folder](.) and these documents:

1. [Feature index](README.md): selected direction and review boundaries.
2. [Implementation plan](implementation-plan.md): milestones, dependencies,
   contracts, acceptance gates and the living progress record.
3. [Product requirements and decisions](local-first-mode.md): required behavior
   and open product questions.
4. [Code assessment and rollout plan](local-mode-code-assessment.md): source
   findings, infrastructure reuse, native installation and staged delivery.
5. [Storage and synchronization research](local-storage-and-sync-patterns.md):
   alternatives, retrieval quality, competitors and design tradeoffs.
6. [Feature scorecard](../../feature-status.md) and its source
   [catalog](../../../featurestatus/catalog.json): repository feature status.

Refresh affected source evidence before relying on historical research. Trace
storage and retrieval, accounts and identity, credentials and vault keys,
messaging and request lifecycles, synchronization, consoles, installation,
updates, background workers, the infrastructure CLI and rollout controls.
Separate inspected behavior from proposals and runtime evidence. Investigate
reuse of infrastructure lifecycle tooling without assuming cloud provisioning
already supports local installation. Alternative backup destinations are a
secondary research opportunity; basic local recovery remains required.

## Preserve the selected requirements

Challenge the architecture, including the local-cell choice, when evidence
warrants it. Record counterarguments and alternatives. Do not silently change
these selected product requirements; ask Scott to resolve a proposed change.

- The ordinary default installation automatically provides a usable free local
  cell, replacing the hosted free plan. Existing hosted free users need an
  explicit, safe transition plan; documentation does not change their accounts.
- Keep lightweight remote-only installation for SaaS, self-managed services,
  containers, clusters and autonomous agents, without downloading or registering
  a local cell or database.
- Pursue native WitSelf with managed PostgreSQL and minimal dependencies, avoiding
  mandatory Docker or Kubernetes. PostgreSQL is a separate managed process.
  macOS, Windows and Linux are required for the first supported local release;
  packaging, service ownership and supported versions still need proof.
- Paid enrollment adds automatic continuous whole-account synchronization by
  default. The same local cell, logical identities, data, installed clients,
  endpoints and legitimate key access continue working through enrollment and
  return to local use. No reset, manual export/import or recreated agents.
- Everyday reads, writes, search, transcript capture and declared memory
  operations remain local. The AI client performs memory inference. Local work
  remains usable during outages under the explicit operation contract.
- Messaging works locally without paid SaaS and continues through the connected
  transition. Cross-device delivery requires a working transport and correct
  routing, authority and recovery; copying records or leases grants no authority.
- Email receipt and delivery are the sole hosted-only product feature exception.
  Local copies may be read and searched; remote provider dispatch, routes,
  billing and hosted entitlements retain the proper service authority.
- Preserve local data and pending work through capacity refusal, failed upgrade,
  disconnect, subscription changes and ordinary uninstall. Distinguish sync
  disconnect, billing cancellation and account closure. Prevent stale replay
  from resurrecting deleted data or reopening closed accounts.
- Preserve client custody of secret keys, encryption identity and authorized
  decryption. A copied database or ciphertext snapshot does not transfer keys;
  enrollment and recovery require an explicit client-side contract.
- Require disclosed, bounded, nonblocking usage reporting and quiet contextual
  explanations of paid capabilities. Reporting failure must not block local use.
- Deliver inactive foundations, opt-in tests and canaries incrementally while
  preserving existing behavior. Standalone canary success is not completion of
  continuous sync or the supported product. Future decentralized cells and public
  bare-metal service exposure remain follow-on opportunities.

## Preserve the four review safeguards

These are required boundaries to refine into contracts and tests, not optional
optimizations:

1. **Request-wide authority and ordering.** Fence the complete request graph,
   including sibling reservations, capacity, selection generations, claims,
   cancellation, expiry, settlement and authority handoff. One owner per claim
   alone does not prevent conflicting request-wide decisions.
2. **Atomic compound visibility.** Publish a request operation's linked records
   and opening, offer or result notifications together. Partial synchronization
   must not make protocol-linked messages ordinary claimable mailbox work.
3. **Non-mutating imported inspection.** Imported request list/show views are
   projections and must not trigger lifecycle reconciliation or audit writes.
   Legitimate standalone local reconciliation must continue functioning.
4. **Authenticated offline account-console access.** Account views and client
   inventory work with the directory unavailable, using verified account and
   origin binding. Keep operator and agent credentials and permissions distinct;
   an agent credential must not gain account-wide operator access.

## Keep the populated-account acceptance scenario

Preserve the plan's canonical
[populated-account transition acceptance](implementation-plan.md#populated-account-transition-acceptance).
Refine it in place; do not replace it with an empty-destination bootstrap test.

Both local and hosted accounts begin independently populated with data, sealed
secrets and legitimate client-held keys, installed clients, message history and
active requests. Local clients continue operating while a remote-only client
writes to the hosted account. Cover identity and ownership collisions, admission
and capacity refusal, interrupted bootstrap, network partitions, restarts,
reordered or repeated application, lost acknowledgements, authority handoff,
subscription changes, deliberate disconnect and return to local use.

Require continuity of identities, keys, references, client bindings and authorized
access; no lost acknowledged mutations, duplicate logical effects, privilege
escalation, replay of completed work or deleted-data resurrection. Assert local
durability, remote acceptance, convergence and external delivery separately.
Use synthetic identities and real stores, reproducible fault cases and native
platform coverage. This is a required test specification, not an implemented
test or authorization to use customer accounts.

## Shared tracking and resumption

Update the existing [living progress record](implementation-plan.md#claude-review-and-living-progress-record)
and related requirements or assessment when needed. Do not create a competing
implementation plan or readiness ledger. The feature scorecard remains the
feature-status authority once implementation begins; update its catalog and
regenerate it when authorized, never hand-edit the generated scorecard. Planning
completion alone does not advance implementation or rollout status.

Keep the plan's record shape, `ID / owner / state / PR or decision / evidence /
unresolved gate / next action`, and make each slice resumable with:

- Bounded scope, acceptance criteria and dependencies, including whether each
  dependency is actually satisfied.
- Integration owner and current agent, an ownership claim or handoff, and state.
- Accepted decisions, open questions, alternatives and unresolved dissent.
- Relevant code paths, source revision, worktree/branch, commits and PR links.
- Validation performed, exact outcomes, evidence references and remaining gaps;
  distinguish static review, fixtures, real database, platform and live evidence.
- Blockers, authorization boundaries and the exact next safe step.

At the start of each turn, read this record and repository instructions, inspect
the actual checkout and active ownership, and reconcile discrepancies before
editing. Coordinate ownership to avoid concurrent edits; use isolated worktrees
and task-owned test resources. Never infer ownership from a model name alone.

At the end of each turn or handoff, update the record and identify the next
owner or explicitly leave it unassigned. Distinguish **planned**, **implemented**,
**tested**, **merged** and **deployed** work with supporting evidence. Record
partial or failed validation honestly; a merge of disabled code is not an
enabled capability. Keep credentials, private bodies and customer data out of
ordinary tracking records. Keep authorized changes reviewable through the
repository's normal checks and PR process; unmerged work must be labeled with
its branch/commit rather than presented as available on `main`.

## Deliverables for this planning pass

- Source-backed gaps and contradictions, ranked by impact, with evidence and
  recommendations. Preserve unresolved dissent and name decisions Scott must make.
- Refined existing documents with consistent requirements and a justified
  dependency order across M0–M9. Avoid treating a proposed mechanism as a decision
  or historical source references as current behavior.
- Small implementation slices with owners or explicit vacancies, acceptance
  criteria, validation requirements, activation boundaries and recovery limits.
- A clear next safe slice for separate implementation authorization, plus
  blockers and the evidence needed to proceed.
- A concise handoff describing changed documents, validation performed, source
  revision and remaining work. Follow current repository gates if a later
  instruction authorizes pushing or merging the documentation.
