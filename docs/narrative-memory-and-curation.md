# Narrative Memory And Client-Side Curation

Status: accepted architecture with implemented foreground automatic checkpoint
handling (2026-07-15). Direct storage, atomic supersede, lexical recall, the
fenced client-curation protocol, restricted curator credentials, authenticated
preflight, a provider-neutral runner/CLI driver, PostgreSQL due state, and
model-visible pending-checkpoint delivery for Codex/Claude are implemented in
the current checkout. Cursor/Grok use an explicitly guided `self.show` fallback.
The older `memory curate auto` and launchd/systemd surfaces remain explicit
legacy/manual compatibility tooling and are never invoked by runtime hooks.
Bounded automatic hydration, optional client-authored vectors, and the executable
three-cloud account-move gate are also implemented. Live managed-provider
certification remains operational work.
Native-provider launch is capability-probed and fails closed when an installed
CLI cannot prove the required isolation controls.
This document is the canonical design for portable narrative memory and
supersedes conflicting draft language that assigns narrative memory only to an
AI provider, asks the backend to make consolidation decisions, or has the
backend call an embedding model.

The factual memory system remains intact. Facts hold canonical atomic
assertions; narrative memories preserve episodes, decisions, rationale,
progress, and lessons whose meaning depends on context and time. Together they
form Witself's portable memory layer across machines, supported AI products,
and deployment cells.

## Current Repository Baseline

As of 2026-07-14, migration `0029` implements agent-owned memory heads, immutable
full versions, owner change clocks, exact/pending/unavailable evidence,
append-only evidence resolution, version-specific relations, and value-free
permanent-deletion tombstones/retry shields. Store, HTTP server, client, CLI,
MCP, salient self hydration, and logical account export/import are wired for the
direct lifecycle. PostgreSQL lexical recall is also wired end to end.

Migration `0030` implements owner curation lanes, filtered source cursors,
coalesced requests, fenced leased runs, immutable materialized inputs, canonical
plans and action records, value-free mutation receipts, apply, guarded rollback,
and read-only replay requests. The five reversible plan primitives are
`create`, `replace`, `supersede`, `relate`, and `propose_fact`. Source commits
atomically mark comprehensive automatic work due, bounded snapshots generate a
follow-up when capped work remains, and active leases cannot cross archive
import. The curation graph is included in logical export/import and validated
before destination writes.

Migration `0031` adds immutable credential profiles to the token model.
Existing credentials retain `full`; expiring agent-only `curator-preview` and
`curator-apply` tokens can be minted for at most 24 hours. The server enforces
the profile on every route, excludes sensitive requests from restricted queue
listing, and exposes an authenticated, no-store curation preflight describing
the presented credential's effective identity, profile, expiry, protocol,
permissions, and limits. `curator-preview` can list/read/claim/page/renew/plan/
abandon/status; `curator-apply` adds apply. Neither profile can create or cancel
requests, roll back work, include sensitive inputs, write memories or canonical
facts directly, send messages, or permanently delete.

Migration `0032` adds immutable, owner-scoped client vector profiles and exact
version-bound vector rows. The profile fixes provider/model/recipe identity,
dimensions, distance metric, normalization, and a canonical contract hash.
Clients generate and submit every memory and query vector; the backend validates
finite bounded components, dimensions, normalization, content hashes, and
immutability, then performs deterministic hybrid scoring. Portable JSONB is the
correctness baseline, so no pgvector extension or backend embedding credential
is required. Missing coverage is reported. Zero compatible coverage uses
lexical scoring and ordering; if the hybrid candidate budget was exceeded, that
fallback remains confined to the explicitly reported pinned candidate universe.
An ordinary recall with no vector profile remains the full lexical contract.

Hybrid candidate generation is deliberately bounded. The portable baseline
preselects at most 257 deterministic snapshot rows, ranks the first 256, and
sets `candidate_truncated=true`, `candidate_limit=256`, and degraded reason
`candidate_budget_exceeded` when more exist. A recall cursor then traverses
only that pinned 256-row universe: every later page recomputes the same subset,
keeps the truncation metadata visible, and never silently widens into the
omitted tail. Clients that need a different universe must issue a new filtered
recall request.

Transcript entries do not yet carry a trustworthy sensitivity label. Until
that contract exists, every request whose scope includes `transcript` is
treated as sensitive-by-default: it is invisible and inaccessible to
`curator-preview` and `curator-apply`, even when `include_sensitive=false`.
Full agent credentials retain the transcript-curation path; restricted workers
may curate explicitly memory/evidence-only scopes.

The provider-neutral client runner now pages one frozen run, renews its lease
while a planner reasons, validates and submits one bounded plan, recovers
value-free mutation state, and either applies with explicit approval or
abandons a completed preview for later work. `CommandPlanner` keeps the
Witself bearer token in the parent process, invokes an argv vector without a
shell, sends only the planner envelope on stdin, bounds output, removes every
inherited `WITSELF_*` variable, and sets `WITSELF_CURATOR_SESSION=1`. The CLI
driver performs authenticated preflight before launching that runner.

The legacy/manual local automation engine stores only binding, policy, provider
path, timing, wake, lock, and value-free health state under the agent-scoped
Witself home. Runtime hooks never record its wakes or launch it. An explicit
`memory curate auto wake --runtime RUNTIME` records one value-free manual-poll
marker and services a bounded pass; `run --force` records a scheduled-poll marker.
Debounce, minimum-interval, single-flight, bounded retry/drain, exact-marker
acknowledgement, and failure-backoff state survive restart. Preview is the
default; standing apply requires
`--policy apply --yes`. Because raw transcripts are sensitive-by-default, setup
also requires `--allow-transcript-content` and the installed full agent
credential in the trusted parent. The inference child receives neither that
credential nor its token-file path in argv, environment, or model input.

The explicit legacy persistent launcher is packaged as a private per-user LaunchAgent
on macOS or a systemd user service plus timer on Linux. Both run the bounded
`memory curate auto run --runtime RUNTIME --force --supervise` target, recover
after user-manager startup and failed attempts, and are managed with
`auto service install|status|start|uninstall`. A normal launch begins at user
login; pre-login Linux execution requires the host administrator to enable
standard user lingering. Definitions carry only the executable, runtime, and
value-free Witself state root; credentials, agent identity, provider/model
policy, and source content remain out of service argv and environment.
Installation is idempotent and will not replace or remove an unowned file at
the managed path.

The implemented direct lifecycle is capture, show/read, list, bounded history,
lexical recall with optional client-vector hybrid ranking, adjust, atomic
one-to-many supersede, forget, restore, reactivate, evidence resolution, and
guarded permanent deletion. It enforces
agent ownership, idempotency, optimistic versions, sensitive redaction,
evidence integrity, archive round trips, and no backend model call.

The following remain target work rather than complete end-to-end behavior:
model-visible per-prompt hook injection in runtimes that do not expose it,
live managed cross-cloud certification, and performance/evaluation tuning.
Automatic hydration is implemented against the real current contracts: Codex
and Claude Code receive session and history-dependent task context plus any
pending checkpoint already durable at hook read time. Cursor and Grok Build use
an explicitly labeled managed-instruction/MCP `self.show` fallback for session,
task recall, and checkpoint control.
Native Claude Code and Grok Build planners are
conditionally usable only when their installed CLIs advertise every required
safety control. Codex remains unsupported even when its read-only sandbox flags
are present because its headless CLI does not expose a no-tools/no-shell
contract; read-only tools could still disclose host data through an injected
transcript. Cursor does not advertise a safe prompt plus customization/session
isolation contract, so both native paths intentionally report unsupported.

## Decisions At A Glance

- PostgreSQL is the sole authoritative data store for memories, versions,
  evidence, lineage, curation state, and retrieval metadata. Local outboxes and
  object storage are delivery or archive mechanisms, never another source of
  truth.
- All semantic judgment happens in a client: the current agent, a native
  subagent, or a separate local/headless agent process. The backend never calls
  an LLM or embedding model.
- Real-time capture and deep curation are separate. Hooks continuously preserve
  visible transcript evidence. An explicit narrative `remember` request writes
  a client-authored memory capsule in the same turn. A curator may improve it
  later.
- MCP is request/response transport. It cannot wake an AI or initiate inference.
  Source commits mark work due in PostgreSQL. The next active foreground agent
  normally claims at most one pending request; runtime hooks only preserve and
  flush evidence. Explicit legacy/manual `memory curate auto` or user-owned
  scheduling is a separate compatibility choice.
- Curation submits an explicit, version-checked plan. The backend only
  authorizes, validates, stores, searches, applies, audits, exports, imports,
  and rolls back that plan deterministically.
- Curator authorization is a server-enforced credential property, not an MCP
  display filter. Every launcher first reads the authenticated effective
  preflight and refuses incompatible protocol, permission, or limit claims.
- Automatic curation may perform only reversible memory operations. It may
  propose a fact candidate, but it may not promote an inferred fact to
  canonical truth or permanently delete a fact or memory.
- Automatic retrieval is part of each runtime integration. Users should not
  normally need to ask the agent to search memory.
- Native memory in the currently supported Codex, Claude, Grok, and Cursor
  runtimes may coexist as a provider-local convenience. Witself is the portable
  system of record. The same information is targeted to both only when the user
  explicitly requests both; native persistence keeps that provider's
  guarantees. Gemini and GitHub Copilot adapters remain deferred and disabled.
- The protocol does not depend on native subagents. A subagent is an
  optimization; every adapter must also work in the main agent or a separate
  client process.
- Account export and cell migration include the complete narrative-memory
  graph and rebuild derived indexes after import.

## Durable Roles

| Role | Purpose | Authority |
| --- | --- | --- |
| Fact | Atomic durable assertion | Canonical |
| Transcript | Visible append-only interaction | Evidence, not memory |
| Narrative memory | Captured or curated account | Advisory context |
| Native memory | Runtime-owned memory outside Witself | Provider-local context |

The distinction is intentional. A transcript answers “what was recorded?” A
narrative memory answers “what should be useful later?” A fact answers “what
does this account currently assert as canonical?”

A capture capsule is the first version/stage of a narrative memory, not a fifth
resource or a second storage path. Curation may append a refined version or
derive a replacement while preserving that capture and its evidence.

No part of this design stores or exposes hidden chain-of-thought. Transcript
evidence is limited to runtime-visible content already admitted by the
transcript-ledger contract.

## System Boundary

```text
runtime hooks ──append──> transcript ledger (Postgres)
      │                         │
      └──mark due──────────────>│ curation request (Postgres)
                                │
explicit “remember” ─client summary─> capture capsule (Postgres)
                                │
local/main/subagent curator <──MCP/API──> bounded evidence + memory snapshot
           │
           └── explicit plan ──> deterministic validate/apply/rollback
                                             │
                                             └── versioned narrative memories
```

The backend may run ordinary deterministic workers for retention, queue lease
expiry, index maintenance, archive creation, and metrics rollups. It must not
run a synthesis, summarization, classification, deduplication, conflict-
resolution, or embedding-inference worker.

PostgreSQL queues and leases keep the deployed architecture identical on AWS,
Azure, and Google Cloud. No cloud-specific queue or model service is required.
The same container and migrations run in every cell.

## Capture: Immediate, Ambient, And Explicit

### Ambient capture

Installed and invoked runtime hooks append visible interactions to the
transcript ledger through the existing retryable local outbox. This is the
real-time safety net: if the client stops before curation, the source
interaction is still durable.

Hooks remain fast. They may append transcript events and deterministically mark
a curation request due, but they never wait for an LLM. If an invoked hook
cannot reach Witself, its outbox is flushed later. If a runtime lacks a hook or
never invokes it, no event is invented: the adapter reports transcript capture
as partial and relies on current-agent capture plus later available context.

Curator child processes are marked with `WITSELF_CURATOR_SESSION=1`; the
transcript hook returns before capture when that marker is present. This keeps
the planner's orchestration conversation out of its own future inputs. A
successful ordinary transcript source commit makes durable curation work due in
PostgreSQL. Runtime hooks do nothing else for curation: they do not record an
automation wake, detach a supervisor, or launch inference.

The integration policy is:

- the current agent captures a bounded narrative after a durable decision,
  milestone, correction, or reusable lesson;
- stop, session-end, and pre-compaction hooks flush evidence and mark curation
  due when those hook events exist;
- near a non-trivial foreground turn's end, the active agent processes at most
  one pending fenced request;
- an empty actions plan is applied when no input merits durable memory so the
  reviewed cursors advance;
- explicit legacy/manual `memory curate auto` operation is separate from runtime
  hooks.

Durable source marking and automatic hydration conformance are implemented.
Codex and Claude Code synchronously inject bounded session self-digests, focused
task recall, and any pending checkpoint already durable when the hook reads
`/v1/self`. Cursor can accept and log session-start context without reliably
delivering it to the model, and Grok ignores passive-hook stdout. Those runtimes
use managed instructions and `self.show` and are reported as guided fallbacks,
not automatic hook injection. The optional legacy launchd/systemd user service
can supply a separately selected client process when no interactive runtime is
active; it is never launched by hooks and does not change the boundary that an
MCP server cannot create inference by itself.
Runtime capabilities report
`transcript_capture`, `automatic_capture`, `opportunistic_curation`,
`scheduled_curation`, and `native_memory_write` independently so degraded
adapters are visible.

In the current server capabilities response, `memories`, `memory_recall`,
`memory_supersede`, `memory_permanent_delete`, and
`opportunistic_curation` report implemented server surfaces.
`automatic_capture` and `scheduled_curation` remain unsupported because the
backend does not synthesize memories, launch inference, or supervise a client
process. Foreground checkpoint handling does not change those server flags. The
optional legacy/manual `memory curate auto` worker is a client-owned process
with an explicit provider and policy. Installed runtime guidance tells an active
compatible client how to claim due work, but the backend never implies that such
a client is currently running.

### How an ordinary session creates memory eventually

Ordinary memory refinement is low-touch but still uses the foreground model for
semantic judgment:

1. Runtime hooks append visible transcript evidence. The source commit creates
   or coalesces durable curation work in PostgreSQL.
2. At prompt submission, Codex and Claude read the bounded self digest and
   automatically inject a value-free `memory_checkpoint` when work is already
   pending. Cursor and Grok rely on an always-on managed rule that tells the
   active agent to call `self.show`; this is guided behavior.
3. Near turn end, the active agent processes at most one fenced request. It
   submits only reversible narrative operations or fact proposals. If nothing
   merits memory, it submits and applies an empty actions plan so the exact
   reviewed cursor intervals advance.
4. The backend deterministically validates and applies the plan. It never
   performs selection, synthesis, or model inference.

A transcript still answers what happened; it is not itself a narrative memory.
A memory exists only after an explicit `memory.capture` or a client-authored
curation plan is applied. Delivery is also not execution: a model may ignore its
managed rule, MCP may be unavailable, or the session may end before another
foreground turn. In those cases PostgreSQL keeps the request pending.

Checkpoint timing is intentionally eventual. The prompt hook enqueues capture,
starts an asynchronous flush, and then reads `/v1/self`. The current prompt is
therefore not guaranteed to be inside the returned request, and the current
assistant response cannot be because it does not yet exist. Both may be reviewed
on a later interaction. A checkpoint delivered at prompt start represents the
work durable at that instant, not guaranteed same-turn synthesis.

MCP exposes the capture and curation operations, but it cannot call its own tools
or wake a model. Runtime hooks likewise never wait for an LLM or launch another
curator. The explicit legacy/manual `memory curate auto` path can still be
configured when a deployment deliberately wants a separate client process; it
is not required for the foreground design and is never invoked by hooks.

This preserves the important semantic boundary:

1. Session-start hydration and task recall are reads of existing facts and
   memories.
2. Runtime hooks preserve evidence and source commits mark due work.
3. The foreground client selects and synthesizes.
4. The backend stores and applies only the exact validated plan.

The resulting normal chain is:

```text
ordinary interaction
  -> transcript source committed
  -> PostgreSQL curation request pending
  -> next compatible foreground turn sees checkpoint
  -> active agent applies one reversible plan, including empty when appropriate
  -> request cursors advance; any newer work remains pending
```

If no compatible foreground turn occurs, no synthesized narrative is written;
the due request remains durable rather than being lost. A user who says
"remember what happened here" still gets the immediate same-turn
`memory.capture` path instead of waiting for later curation.

### Explicit narrative remember

When the user says “remember what happened here,” “remember the decisions we
made,” or otherwise explicitly asks Witself to retain narrative context, the
client must:

1. Produce a bounded capsule containing only the visible events, decisions,
   rationale, outcomes, open questions, and useful temporal context.
2. Attach the exact available transcript entry or sequence-range references.
3. Generate one idempotency key for this user intent and call
   `witself.memory.capture` in that turn. Reuse that key only when retrying the
   exact request.
4. Report success only after Witself returns a durable memory id and version.

This path does not wait for session end or deep curation. `memory.capture` is a
convenience surface over the ordinary memory-create resource, not a second
storage type. The target, not-yet-implemented `session.end` helper would compose
the same path with `kind=session` and a client-authored progress capsule; the
backend would never write the summary itself.

Evidence must not delay the capture. If hooks have not flushed the current turn,
the client includes the stable external conversation/session/turn locator it
has. A deterministic resolver attaches the exact transcript ids and sequences
after those entries arrive. Unresolved locators remain visibly pending and
portable; they are never guessed or attached across owners. Already-flushed
entries use exact references immediately.

If the request says “in both,” the client performs two independent operations:
the durable Witself capture and the supported native-provider action. The
Witself receipt and native outcome are reported independently because a
cross-provider dual write cannot be atomic. If the provider has no
transactional write API, its action is explicitly best-effort and the client
must not claim durable native storage. Witself success is not rolled back if
native memory fails.

An atomic assertion inside the same request follows the fact-routing contract.
The client may split the request into a canonical fact write and a narrative
capture. It does not silently store identical content in both Witself and a
native provider.

### Automatic checkpoints

Runtime integrations may create client-written capsules at safe boundaries:

- explicit checkpoint or pre-compaction;
- session stop/end;
- a durable decision or milestone;
- an evidence or token threshold;
- an explicitly configured legacy scheduled idle window.

Thresholds and budgets are client settings. Managed instructions ask the current
agent to capture recognized durable decisions, milestones, corrections, and
lessons, while transcript source commits queue deeper work in PostgreSQL. Hooks
only preserve and flush evidence. On a later compatible foreground turn, a
pending checkpoint asks the same active agent to review one bounded frozen
interval. Only an actual client `memory.capture` call or an applied
client-curation plan creates the memory; the server stores the result and its
capture reason but never decides what the capsule should say.

## Curation: Low-Touch But Client-Run

Curation should need little routine user interaction after installation. Four
modes share one protocol:

| Mode | Trigger | Normal behavior |
| --- | --- | --- |
| Immediate | Explicit `remember` | Capture now; optionally queue refinement |
| Foreground | Active agent sees a pending checkpoint | Curate at most one fenced request; apply an empty plan when appropriate |
| Legacy scheduled | Explicit `memory curate auto` service | Curate within configured budget |
| Manual | User asks to refine memories | Run now, normally preview first |

The normal curator is the current foreground agent. A native subagent or the
explicit legacy separate client is an optimization or compatibility path, never
a requirement. All supported runtimes must work in the main agent without a
background runner.

The foreground path uses the active agent's authenticated MCP authority and
applies only reversible plans. The explicit legacy separate-process/unattended
posture is stricter:

- bounded batches and token/action budgets;
- sensitive memories excluded;
- a short-lived `curator-preview` credential for inspection and planning;
- a separately selected `curator-apply` credential plus explicit local apply
  policy before mutation;
- reversible memory operations only;
- fact candidates proposed, never silently confirmed;
- conflicts surfaced without choosing a winner;
- no secrets, permanent deletion, messages, policy, or unrelated account
  mutation permissions.

Curator-generated provider sessions are excluded at transcript capture through
the curator-session marker. Apply-produced memory changes are attributed to the
run and consumed by that run, so they do not enqueue themselves into a feedback
loop.

## Curation Protocol

The AI must never hold a database transaction or lock while it reasons.
Curation uses one active lane per owner, materialized run inputs, a lease with a
fencing generation, contiguous per-stream cursors, optimistic memory versions,
and an exact plan hash:

1. A transcript/evidence/memory source mutation increments the owner's
   `request_generation` in the same commit and creates or updates a due request.
   Coalescing may combine reasons, but it never lowers the generation.
2. `start` claims the request only when no other run is active for that owner
   lane. In one short database transaction it records the current request
   generation, materializes every exact run input, and stores:
   - current memory id/version references;
   - exact immutable evidence row ids;
   - one lower and upper sequence for each included transcript conversation;
   - each source cursor's expected prior value.
3. `start` returns `run_id`, `fencing_generation`, `lease_expires_at`, and the
   first page cursor. There is no owner-wide transcript sequence: transcript
   sequences remain local to their conversation.
4. `renew` extends the lease only for the current fencing generation. `get`
   requires that generation and pages only the materialized inputs, so later
   evidence resolution or commits cannot change the run while a model reasons.
5. The client reasons locally and submits a canonical plan. `plan` requires the
   fence and validates shape, authorization, provenance, limits, sensitivity,
   expected versions, and references. It returns the normalized plan revision,
   exact plan hash, preallocated create ids, and deterministic impact preview.
6. `apply` requires an unexpired matching fence, plan revision/hash, and
   idempotency key. In one bounded transaction it locks targets in stable id
   order, rechecks memory versions, applies all actions, and compare-and-swaps
   every source cursor from the run's recorded lower bound to its upper bound.
   Any stale head, non-contiguous cursor, or expired fence returns a conflict
   with no partial change. When no frozen input merits durable memory, a
   zero-action plan is the correct result and is still applied to advance the
   exact reviewed contiguous intervals without creating memory or facts.
7. Apply marks the request fulfilled only through the captured generation. If
   a new source commit raised the generation after `start`, a follow-up request
   remains queued. A trigger can therefore never disappear into an already
   claimed run.
8. `rollback` appends compensating versions and reverts created relations. It
   succeeds only if every affected head still matches the apply receipt and no
   later unreverted relation, evidence row, or curation action consumes an
   apply-produced version. A dependent consumer is a visible blocker, never a
   silent cascade.
9. Successful rollback creates a re-evaluation request with `replay_run_id`.
   Start clones the original materialized input ids in `read_only_replay` mode,
   adds current heads for context, and does not select or advance the old cursor
   intervals again. Later cursors are never rewound.

Preview is a successful non-mutating terminal path, not a failed attempt. After
the server accepts a preview plan, the runner abandons the planned run with the
value-free reason `preview_complete`. The request moves to `retry_wait` with a
24-hour cooldown without incrementing `attempt_count`; a newly committed source
generation makes it due sooner. Spoofing that reason on an unplanned run does
not receive the exemption and consumes the ordinary failure budget.

The owner-lane uniqueness constraint deliberately serializes deep curation for
v1. Parallel workers may curate different owners, while two machines for the
same owner cannot apply out of order. A later version may add explicitly
disjoint lanes only with disjoint cursor keys and inputs.

Lease expiry marks the old run `interrupted` and requeues the request for a new
run/fencing generation; it does not let a new worker mutate the old run.
`renew` is the heartbeat path for a long model call. Read-only `status` remains
available after expiry, but `get`, `plan`, and `apply` reject the stale fence.

Every mutation is idempotent by actor, operation, and key. The server stores a
canonical request hash. Replaying the same input returns the original receipt;
reusing a key with different input returns a conflict.

### Deterministic plan primitives

The backend supports a deliberately small operation language:

- `create`: create a full new memory and its exact evidence/lineage relations.
- `replace`: append a full desired snapshot to one memory at an
  `expected_version`.
- `supersede`: atomically mark an exact active version as replaced and insert
  one nonempty replacement set. A set may contain multiple versions for split.
- `relate`: add a version-specific `derived_from`, `summarizes`,
  `merged_from`, `split_from`, or `conflicts_with` relation. Generic `relate`
  cannot create a `supersedes` edge.
- `propose_fact`: create a review candidate through the existing fact service,
  with exact evidence; never call `fact.set` on an inferred assertion.

Retagging, relinking, salience changes, and ordinary revision compile to
`replace`. Merge creates one output and supersedes each input with it. Split
creates multiple outputs and supersedes the input with that replacement set.
This keeps semantic intent in the client while giving the backend a small,
testable, provider-neutral contract.

`replace` may change content, kind, tags, salience, links, sensitivity, and
occurrence range. It cannot change owner, origin, capture reason, or original
authorship. Evidence already attached to the memory remains in its immutable
history; the replacement may add exact evidence but cannot erase it. A curator
that needs different authorship or evidence semantics uses `create + relate`
instead.

`propose_fact` joins the same PostgreSQL transaction through a transaction-aware
fact-candidate store helper. Rollback may withdraw a candidate only while it is
still pending/conflict and still attributable solely to that run. A candidate
already confirmed by a user is a rollback blocker; rollback never changes a
canonical fact.

Curator-created candidates store nullable-but-required-for-this-source
`curation_run_id` and `curation_action_id` plus exact evidence references.
Rollback moves an eligible `pending | conflict` candidate to the distinct
terminal `withdrawn` state with reason `curation_rollback` and its own
idempotency key. It never uses the human `rejected` decision. Candidate
listing, export/import, and state checks include `withdrawn`.

Hard delete is not a plan primitive. A content hash is non-unique integrity and
diagnostic metadata: identical text can describe different events. Idempotency
and exact source-capture keys suppress retries. Content hash alone may report a
possible duplicate but may never discard a capture.

### Plan Canonicalization

The accepted plan schema is versioned independently as
`witself.memory-plan.v1`. The server:

1. validates action ordinals and preserves that order;
2. preallocates create ids and resolves client-local references to those ids;
3. rejects client timestamps and other nondeterministic fields;
4. normalizes the accepted JSON with RFC 8785 canonicalization;
5. hashes the canonical bytes with SHA-256; and
6. returns the normalized plan, `plan_revision`, and `plan_hash`.

Validation errors leave the run `open` so the client may submit a corrected
draft revision. The first accepted plan transitions the run to `planned` and is
immutable. Changing an accepted plan requires a new run. `apply` always names
both the server-returned revision and hash. Before applying a resumed planned
run, the client retrieves the normalized accepted plan through the fenced,
read-only plan-review surface, verifies its canonical hash again, and
independently reviews every action and preview against all frozen inputs and
current policy. Content-free run metadata is never sufficient apply authority.

## PostgreSQL Model

The memory resource becomes a stable head pointer into an append-only version
graph. The first implementation should use the following logical tables; exact
column spellings are frozen with the API schema in Phase 0.

### `memories`

Stable identity and head pointer:

- id, account, realm, and stable owner;
- immutable origin, capture reason, and original authorship;
- nullable `current_version`;
- created/updated timestamps;
- nullable value-free permanent-deletion tombstone metadata.

The mutable payload does not live in `memories`. The composite
`(id, current_version)` pointer has a deferrable foreign key to
`memory_versions(memory_id, version)`. Version insertion and head movement occur
in the same transaction. This normalized design makes an inconsistent
materialized head impossible; an optional read view may expose the joined
current payload.

A database check requires `current_version IS NOT NULL` while the resource is
live and `current_version IS NULL` only when permanent-deletion tombstone
metadata is present. Purging all versions can therefore leave a valid tombstone.

For the first vertical slice, ownership is one stable Witself agent. Runtime and
machine are provenance, not owners. The schema keeps the existing owner
abstraction so explicit group-owned memory can follow without changing the
version or evidence model.

### `memory_versions`

Immutable full snapshots keyed by memory id and version:

- previous version and monotonic owner-scoped `change_seq`;
- content, kind, tags, salience, links, sensitive flag, occurrence range,
  lifecycle state/reason, and non-unique content hash;
- token-derived actor and operation;
- reason and idempotency reference;
- client runtime, model, recipe, and recipe version when self-reported;
- for the exact version created by atomic supersede, immutable value-free
  `supersession_set_id`, `supersession_set_revision`, replacement count, and
  replacement membership digest;
- nullable curation run/action ids;
- created timestamp.

Full snapshots make stable reads, audit, export, conflict reporting, and
compensating rollback straightforward. Audit and error surfaces remain
value-free even though authorized history contains the payload.

The generated full-text document/index lives with each version. Normal recall
joins through `memories.current_version`; history search is a separate explicit
surface and never leaks superseded payload into ordinary recall.

`change_seq` is not a PostgreSQL sequence used as a completeness cursor.
Allocation occurs under a locked per-owner change-clock row held through
the version transaction, so committed versions cannot appear below an already
committed clock value. Curation correctness still comes from materialized input
ids and per-stream cursors, not from assuming `BIGSERIAL` commit order.

Value-bearing version states are `active | superseded | forgotten | reverted`:

- `active -> superseded` occurs only through the atomic `supersede` primitive.
- `active | superseded -> forgotten` is an explicit lifecycle operation that
  records the prior state.
- `forgotten -> prior_state` is `restore` with `expected_version`. If the prior
  supersession is no longer valid, restore conflicts and requires an explicit
  `reactivate` decision.
- A plan-created output becomes `reverted` on successful curation rollback.
  `reverted -> active` requires explicit `reactivate` with `expected_version`.
- Reactivating a superseded memory also requires the expected supersession-set
  revision and atomically marks that set reverted. Replacement memories remain
  independent active memories unless separately changed.
- Every adjust, replace, supersede, forget, restore, or reactivate uses
  `expected_version` and appends a full snapshot.

`deleted` is a value-free resource tombstone, not a payload-bearing version
state. Last-accessed counters are derived usage metadata outside immutable
versions; they never cause content versions or curation generations.

### `memory_change_clocks`

One row per `(account, realm, owner_kind, owner_id)` serializes `change_seq`
allocation. Mutation locks the row, advances the clock, writes its
version/evidence rows, and holds the lock until commit. Gaps from aborted
transactions are harmless; committed rows never arrive below an already
committed clock value.

### `memory_evidence`

Append-only version-aware evidence, not a loose tag:

- id, target memory id, and the version that introduced it;
- evidence type and role (`supports | contradicts | context`);
- `pending`: exactly one external conversation/session/turn locator;
- `resolved`: exactly one internal transcript entry/range,
  source-memory/version, message, or import-artifact locator, plus the pending
  id when it resolves one;
- `unresolvable`: a pending id and bounded value-free failure reason, with no
  source locator;
- `unavailable`: a bounded reason explaining absent capture, with no locator or
  pending id;
- source digest/checksum where it helps validate portable imports;
- commit-ordered evidence change sequence, timestamps, and token-derived actor.

Evidence keeps original captures reachable. Curators should prefer the original
transcript/capsule over repeatedly summarizing a summary.

Resolution never edits a pending row. It appends a resolved or unresolvable row
that points to the pending id, bumps the owner request generation, and queues
the new observation for a later run. A run materializes exact evidence row ids,
so resolution cannot alter an in-progress snapshot. Capture must provide exact
evidence, a pending external locator, or an explicit `unavailable` reason when
the runtime did not record evidence.

Database checks enforce those state-specific XOR shapes, tenant/owner
consistency, version foreign keys, and at most one terminal resolution for one
pending locator.

Referenced transcript entries cannot age into dangling evidence. Retention pins
them while a live memory depends on them. Before an authorized transcript purge,
Witself must either delete/replace the memory evidence under its own authority
or materialize an immutable, sensitive evidence artifact containing the bounded
visible excerpt and original checksum. The artifact is stored in PostgreSQL,
travels in account archives, and participates in permanent-memory deletion.

### `memory_relations`

Version-specific lineage:

- from and to memory id/version;
- relation type;
- optional server-assigned supersession-set id;
- curation run/action and created time;
- optional compensating-run marker when a relation is reverted.

Relations never erase predecessors. Recall normally hides superseded heads but
history, explanation, export, and re-curation can traverse them.

All endpoints have exact version foreign keys and tenant/owner checks.
`supersedes` is server-created only inside the atomic `supersede` primitive and
belongs to exactly one unreverted supersession set per superseded head. That set
contains one or more replacement versions. The source version and value-free
receipt bind the sorted exact replacement id/version set with
`replacement_count` and a lowercase SHA-256 `replacement_digest`. Replay fails
closed if live relation membership no longer matches that commitment.

Memory reads also project `active_supersession_set_id` and
`active_supersession_set_revision` from the tenant- and owner-scoped unreverted
relations for the stable memory identity. These fields describe current
relation state; they are not copied into immutable version rows. Consequently a
restored superseded head reports the active set, reactivation clears the active
projection for current and historical reads, and the original superseded
version still carries its immutable receipt. Both receipt and active-projection
fields are value-free and remain visible when broad sensitive-memory responses
redact payload fields.

### `memory_deleted_references`

Permanent deletion cannot retain a foreign key to a purged version. In the
implemented slice this separate value-free table stores only the deleted memory
id, an `idempotency.*` retry-shield kind, the SHA-256 hash of the former retry
key, the fixed `permanent_delete` reason code, and a timestamp. Curation ids are
required to be null. It has no content, excerpt, vector, content/plan hash, raw
retry key, or foreign key to `memory_versions`.

The delete transaction inserts one unique shield for every purged version-
mutation key and evidence-resolution key, verifies the set's count and digest,
then purges ordinary evidence, relations, and versions. Exact version foreign
keys remain mandatory for every live row; only this value-free tombstone table
may refer to the deleted memory id without a version. When curation and vectors
land, their value-bearing rows must be added to this scrub transaction without
expanding this table into a payload side channel.

### `memory_curation_lanes`

One v1 lane per owner stores `request_generation`, `fencing_generation`, current
active run, and timestamps. Source mutation, request coalescing, run claim, and
fence advancement compare-and-swap this row. A unique active-run constraint is
the serialization boundary for that owner.

### `memory_curation_runs` and `memory_curation_actions`

Runs hold owner/scope, token-derived actor, self-reported runtime/model/recipe,
watermarks, lease/fencing data, state, plan hash, apply receipt, budgets, and
timestamps. Actions hold the ordered primitive, exact inputs/expected versions,
proposed payload, validation result, applied ids/versions, and rollback link.

Run transitions are:

- `open -> planned -> applied -> rolled_back`;
- `open | planned -> abandoned | interrupted`; and
- `planned -> conflict` on a stale head, cursor, or fence.

`conflict`, `abandoned`, `interrupted`, and `rolled_back` are terminal. Apply is
one database transaction; no durable `applying` state can expose a partial plan.
Rebasing creates a new run so the original snapshot and plan/hash stay
immutable. Action states are `draft | validated | applied | reverted`; only
validated actions belong to an accepted immutable plan.

Proposed payloads receive the same authorization, redaction, retention, and
logging protections as memory content.

### `memory_curation_run_inputs`

Each run materializes stable, paginatable input membership:

- exact memory id/version;
- exact evidence row id;
- transcript id with inclusive lower/upper sequence;
- the expected prior cursor for each source stream; and
- deterministic page/order key.

Input rows, rather than a broad watermark query, are what `get` reads.

### `memory_curation_requests`

The PostgreSQL-backed due queue holds owner/scope, deterministic trigger reason,
coalescing key, `request_generation`, priority, due time, attempt/backoff count,
and timestamps. It contains no generated summary or semantic decision.

Explicit captures, user/operator edits, transcript appends, and evidence
resolution bump the generation. Versions produced by applying the current
curation run are marked as consumed by that run and do not enqueue themselves;
otherwise the curator would create a permanent feedback loop.

Transitions are `queued -> claimed -> fulfilled`, `claimed -> retry_wait ->
queued`, `queued | retry_wait | claimed -> cancelled`, and
`queued | retry_wait -> dead_letter` after the configured retry ceiling. Lease
expiry or retryable apply conflict makes the run `interrupted` or `conflict`,
increments attempt/backoff and fencing generation with compare-and-swap, and
moves the request through `retry_wait`. A nonretryable validation/authority
failure dead-letters the request; direct cancellation cancels it. Coalescing may
update a queued or claimed request's generation; fulfillment covers only the
run's captured generation.

An optional `replay_run_id` and `read_only_replay` mode identify rollback
re-evaluation. Such a request clones the original run inputs, may read current
heads, and never advances the already-consumed cursor intervals.

### `memory_curation_cursors`

A cursor is one row per owner and explicit source-stream id. A transcript stream
uses its transcript id and per-conversation sequence; memory/evidence streams
use their stable stream keys. Apply advances a cursor only by compare-and-swap
from the run's exact prior value to a contiguous upper value. There is no single
owner-wide “last transcript sequence,” and a later run cannot jump over an
unapplied interval.

### `memory_vector_profiles` and `memory_vectors` (migration `0032`)

Vectors are optional derived indexes, never the source of truth:

- a profile identifies provider/model/recipe, dimensions, distance metric, and
  normalization contract;
- a vector row is keyed by profile, memory id, memory version/content hash, and
  the exact agent owner scope;
- both memory and query vectors are supplied by an authorized client;
- the backend validates finite bounded values, profile/dimensions,
  normalization, version/content hash, and immutable retries, stores canonical
  JSONB arrays, and performs bounded deterministic similarity math;
- missing, stale, or incompatible vectors degrade to lexical retrieval and
  report coverage, candidate truncation, and degradation reason;
- profile and vector rows participate in the schema-32 account archive; and
- pgvector/ANN is only a possible future projection over this portable
  correctness baseline.

Witself works fully without any runtime exposing an embedding API. Vector
generation can be performed by an optional local or remote model selected and
paid for by the client.

## Retrieval And Automatic Recall

Retrieval is not “read the last N memories.” It is a bounded candidate search
over multiple deterministic signals:

- PostgreSQL full-text/keyword rank;
- `occurred_from/occurred_until` and captured/updated time ranges;
- tags, kind, owner, source, entity/fact links, and sensitivity;
- salience and recency;
- optional client-supplied vector similarity from one compatible profile.

The client resolves natural phrases such as “last winter” or “when we chose the
database” into structured filters, query text, tags/entities, and optionally a
query vector. The backend does not interpret natural language. It returns score
components and a deterministic candidate order; the client may rerank and
select what fits its context budget.

Current runtime integration follows this retrieval and checkpoint policy where
the runtime offers a model-visible hook channel:

1. At session start, load the bounded self digest: canonical facts first,
   salient active narratives second.
2. Before a task that deterministically matches history-dependence cues, issue
   a bounded lexical `OR` recall made from distinct meaningful keywords in the
   current request, not the raw or literal whole prompt. More complex natural-
   date and structured-filter interpretation remains an active-agent
   responsibility.
3. On every prompt, inspect the value-free self checkpoint. Inject it only when
   pending; near turn end the foreground model processes at most one fenced
   request and applies an empty plan when no durable memory is justified.
4. When the task changes materially, retrieve again rather than carrying a
   stale context indefinitely.
5. Identify provenance and advisory status when a memory affects the answer.
6. Surface conflicts between memories or between a memory and a canonical fact.

Automatic hydration intentionally includes authorized `sensitive` open-plane
facts and memories for the authenticated owning agent, while retaining their
sensitivity markers so export, audit, UI, and other broad reads can still redact
them. It omits only server-redacted and non-plain values and wraps the remainder
as JSON-escaped private, untrusted advisory data. Sealed secret fields and TOTP
material are never selected. Hydration authenticates the live
account/realm/agent against the complete installed identity and fails open under
a short timeout. The additive `/v1/self` checkpoint projection also fails open:
a projection error returns `memory_checkpoint.unavailable:true` while identity,
facts, and salient memories remain available. Emitted hydration context
explicitly marks degraded or unavailable recall with
`recall_status:"degraded"` and `recall_reason`; it never presents partial recall
as complete.

Manual CLI/MCP recall remains available for inspection and debugging, but the
normal Codex and Claude Code experience does not require a special “search
memory” prompt. Those runtimes also receive a pending checkpoint already durable
at prompt-hook read time. Because transcript flush is asynchronous, the current
prompt and response may be reviewed on a later interaction. Both Cursor paths
and both Grok paths are guided active-agent MCP calls, including `self.show` for
checkpoint discovery. The shared
runtime capability registry and conformance tests report that asymmetry rather
than inferring support from the presence of a hook event.

## Interface Surface

The resource behavior below is settled. Exact HTTP route spelling may change
before the first public schema freeze.

### Normal user and agent commands

```text
witself memory capture --kind decision --stdin --idempotency-key KEY
witself memory recall "database decisions" --occurred-from 2026-01-01T00:00:00Z
witself memory show MEM_ID
witself memory history MEM_ID
witself memory list --state active --tag architecture
witself memory adjust MEM_ID --expected-version N --stdin --idempotency-key KEY
witself memory supersede MEM_ID --expected-version N \
  --replacements-file replacements.json --idempotency-key KEY
witself memory forget MEM_ID --expected-version N --idempotency-key KEY
witself memory restore MEM_ID --expected-version N --idempotency-key KEY
witself memory reactivate MEM_ID --expected-version N --idempotency-key KEY
witself memory evidence resolve EVIDENCE_ID --transcript TRANSCRIPT_ID \
  --from-sequence N --until-sequence N --idempotency-key KEY
witself memory delete MEM_ID --dry-run
witself memory delete MEM_ID --expected-version N \
  --scrub-set-revision SHA256 --idempotency-key KEY --yes
```

Every mutation requires an idempotency key in machine interfaces. An interactive
TTY may generate, persist in its local retry receipt, and print one. A
noninteractive or `--json` invocation must supply it. The same key is reused
only for an exact retry; a new intent receives a new key.

### Curation operations

```text
witself token create --agent AGENT_ID --profile curator-preview \
  --name "memory curator" --ttl 30m --out ./curator.token
witself memory curate request --idempotency-key KEY
witself memory curate requests
witself memory curate start --request REQ_ID --idempotency-key KEY
witself memory curate renew RUN_ID --fence FENCE --idempotency-key KEY
witself memory curate show RUN_ID --fence FENCE --cursor CURSOR
witself memory curate plan RUN_ID --fence FENCE --file plan.json \
  --idempotency-key KEY
witself memory curate apply RUN_ID --fence FENCE --plan-revision N \
  --plan-hash HASH --idempotency-key KEY --yes
witself memory curate cancel RUN_ID --fence FENCE --idempotency-key KEY
witself memory curate abandon RUN_ID --fence FENCE --idempotency-key KEY
witself memory curate rollback RUN_ID --apply-receipt RECEIPT \
  --expected-heads heads.json --idempotency-key KEY --yes
witself memory curate status [RUN_ID]
witself memory curate run [flags] -- PLANNER [ARGS...]
witself memory curate run [flags] --provider codex|claude-code|grok-build|cursor \
  [--provider-path PATH]
```

`memory curate run` is the implemented trusted client driver. It obtains the
authenticated effective preflight, claims the highest-priority, oldest-due eligible request
unless `--request` is supplied, pages all frozen inputs, and previews by
default. It either runs an explicit JSON-in/JSON-out planner argv or probes and
uses the selected native provider CLI. The native path fails closed before
claiming work when required isolation controls are absent. `--apply --yes` selects
apply policy and requires effective apply permission. `--resume LAUNCH_ID`
recovers value-free protocol coordinates from a private local receipt; it does
not persist a second copy of inputs or plan JSON. A provider-specific automatic
launcher remains client orchestration, not a server worker.

### Minimum Machine Contract

Exact HTTP resource spelling may change before public schema freeze, but these
request/response fields are required:

- Capture accepts content, kind, tags, salience, sensitivity, occurrence range,
  exact/pending/unavailable evidence, capture reason, client provenance, and
  idempotency key. It returns memory id, version, content hash, evidence
  resolution states, and mutation receipt.
- Adjust/forget/restore/reactivate accept memory id, `expected_version`, the
  operation payload/reason, and idempotency key. A stale version is a conflict.
- Supersede accepts one active memory id/version, an operation idempotency key,
  and 1-32 client-authored replacement capsules. Every replacement has a unique
  key and exact, pending, or explicitly unavailable evidence. The implemented
  route is `POST /v1/memories/{memory_id}/supersede`; success returns the full
  authorized source/replacements and a value-free supersession-set receipt with
  exact references plus the replacement count and membership digest.
- Current-memory and history records expose the immutable supersede receipt as
  `supersession_set_id`, `supersession_set_revision`,
  `supersession_replacement_count`, and
  `supersession_replacement_digest`. They separately expose the relation-derived
  current view as `active_supersession_set_id` and
  `active_supersession_set_revision`.
- Pending evidence resolution accepts one pending evidence id, one exact source
  or value-free unresolvable reason, and an idempotency key. It appends a
  terminal evidence row at
  `POST /v1/memory-evidence/{evidence_id}/resolution`; the pending row is never
  edited.
- Permanent-delete preview is deterministic and accepts the memory id only. It
  creates no preview resource and has no preview id, hash, expiry, or
  idempotency key. It returns a value-free impact receipt containing the current
  version, deterministic scrub-set revision, purge/retry-shield counts and
  digest, and incoming-evidence/active-relation blocker counts. Apply binds the
  memory id, `expected_version`, `scrub_set_revision`, one fresh idempotency key,
  explicit confirmation, and same-turn direct-current-user authority. Preview
  or apply never belongs to a curator credential.
- List/history/recall use an opaque page cursor and bounded limit. Implemented
  recall accepts literal full-text query, occurrence/capture ranges,
  kind/tags/links, sensitivity, origin, and capture-reason filters. It returns
  lexical, similarity/vector-use, salience, recency, and total score components,
  `retrieval_mode`, profile/coverage/candidate fields, truncation, and degraded
  reason. Supplying both an immutable profile id and compatible query vector
  enables the implemented bounded hybrid ranker; omitting both uses lexical.
- Curation request accepts owner lane, bounded scope, coalescing key, trigger
  reason/generation, and idempotency key.
- Authenticated curation preflight returns the presented token's agent identity,
  immutable access profile and expiry, exact effective permissions, plan schema
  and primitives, client-inference boundary, and server limits. It is
  `private, no-store`; the client must not infer authority from deployment-wide
  capabilities or an MCP tool list.
- Start accepts request id, scope caps, and idempotency key. It returns run id,
  request generation, fencing generation, lease expiry, materialized-input
  counts, and first opaque page cursor.
- Renew accepts run id, current fence, requested bounded extension, and
  idempotency key. Get accepts run id, current fence, opaque cursor, and limit.
- Plan accepts run id/fence, `witself.memory-plan.v1` draft revision, ordered
  actions, and idempotency key. It returns validation errors or the canonical
  plan revision/hash, normalized actions, preallocated ids, and impact preview.
  An empty actions list is valid when no frozen input merits memory.
- Plan get accepts a live planned run id/fence and read-only-reconstructs the
  exact normalized accepted plan, preallocated ids, and preview without its
  planner mutation receipt. The caller reviews that untrusted content against
  every paged frozen input before apply.
- Apply accepts run id/fence, canonical plan revision/hash, and idempotency key.
  It returns the apply receipt, exact before/after heads, fact-candidate ids,
  advanced cursor intervals, and follow-up generation if work remains. Applying
  an empty plan advances only the reviewed intervals.
- Cancel accepts run id/fence and idempotency key. Rollback accepts run id,
  apply receipt, every expected apply-produced head, reason, and idempotency key.
- Status requires ordinary authorization but no active lease. It never returns
  raw plan or memory content unless the caller also has the corresponding
  memory-read permission.

Mutation receipts are value-free and include operation, actor, idempotency key,
canonical request hash, resource ids/versions, and timestamps. Pagination,
limits, error codes, and `409` conflict details use the shared JSON envelope.

MCP and CLI expose schema-equivalent operations:

- `witself.memory.capture`
- `witself.memory.read`
- `witself.memory.list`
- `witself.memory.history`
- `witself.memory.recall`
- `witself.memory.adjust`
- `witself.memory.supersede`
- `witself.memory.forget`
- `witself.memory.restore`
- `witself.memory.reactivate`
- `witself.memory.evidence.resolve`
- `witself.memory.delete`
- `witself.memory.curation.preflight`
- `witself.memory.curation.requests`
- `witself.memory.curation.request.get`
- `witself.memory.curation.request`
- `witself.memory.curation.start`
- `witself.memory.curation.run.get`
- `witself.memory.curation.renew`
- `witself.memory.curation.get`
- `witself.memory.curation.plan`
- `witself.memory.curation.plan.get`
- `witself.memory.curation.apply`
- `witself.memory.curation.cancel`
- `witself.memory.curation.abandon`
- `witself.memory.curation.rollback`
- `witself.memory.curation.status`

### Audit Events

The stable registry is [audit-retention.md](audit-retention.md). The implemented
direct mutation events are:

- memory lifecycle: `memory.added`, `memory.adjusted`,
  `memory.superseded`, `memory.forgotten`, `memory.restored`,
  `memory.reactivated`, `memory.evidence.resolved`, and `memory.deleted`.

Deletion preview is deterministic and non-mutating, so it emits no event. The
implemented registry also includes:

- retrieval: `memory.read` and `memory.recalled`;
- curation: `memory.curation.requested`, `memory.curation.started`,
  `memory.curation.planned`, `memory.curation.applied`,
  `memory.curation.conflicted`, `memory.curation.interrupted`,
  `memory.curation.cancelled`, and `memory.curation.rolled_back`; and
- curation rollback withdraws an apply-created fact candidate with explicit
  curation attribution; a separate `fact.candidate.withdrawn` audit verb remains
  future work.

Implemented events store resource ids, versions/states, resolution ids/state,
and value-free curation generations/counts only. No event stores memory,
plan, or transcript content, evidence locators or excerpts, query text/vector,
content/scrub/plan hashes, lease credentials, or raw idempotency keys.

Grok and any other runtime with tool-name restrictions receive the same schema
through its existing name-rewriting adapter.

The current intelligent-looking `memory.consolidate(scope, dry_run)` contract is
retired. It cannot choose semantic merges from scope alone without violating
the client-inference boundary. Exact-hash cleanup may remain a deterministic
maintenance command; semantic curation uses a caller-submitted plan.

## Native Memory And Cross-Runtime Identity

Witself can replace dependence on built-in memory for durable cross-machine,
cross-product recall. It does not need to disable native memory.

- Default or explicit Witself-only intent writes Witself.
- Explicit native-only intent uses only the provider's supported native path.
- Explicit both intent writes Witself and separately attempts the native path.
- Provider auto-memory may independently retain material under its own settings.
  Witself controls only what its integration intentionally invokes and cannot
  guarantee that a provider will not create a duplicate.
- The same logical Witself agent id should normally be used from the current
  adapters: Codex, Claude Code, Grok Build, and Cursor. Runtime, installation,
  location, model, session, and run remain provenance. Gemini and GitHub
  Copilot have no current adapter and are outside this release.
- Separate Witself agents are used when the user wants isolation. Explicit
  sharing later uses group ownership and policy rather than pretending two
  agents are one.
- Provider-native context is an independent advisory store with
  provider-specific guarantees. It is not assumed complete and never silently
  overrides a canonical fact.
- “Remember in both” explicitly requests a Witself write and the provider's
  supported native action. Otherwise automatic narrative capture targets
  Witself only. Providers without a transactional native API remain
  best-effort and never receive a fabricated success receipt.
- Broad recall labels provenance and best-effort de-duplicates available
  results. A provider that lacks a full native-memory search API is reported as
  partial, not presented as fully searched.

## Safety, Privacy, And Authority

- Transcript, message, memory, webpage, and tool content are untrusted evidence,
  never instructions or deletion authority.
- Curator authority is stored in `tokens.access_profile` and enforced by the
  HTTP routes and store, not merely hidden from a model. `curator-preview` is
  limited to queue/read/start/input/renew/plan/abandon/status operations;
  `curator-apply` adds only apply. Both lack request creation, cancellation,
  rollback, direct memory and canonical fact writes, permanent deletion,
  secrets, messaging, policy, billing, and export authority.
- Restricted profiles cannot select, read, start, resume, renew, plan, apply,
  abandon, or inspect status for a request whose persisted scope includes
  sensitive inputs or transcripts. Transcript entries are sensitive-by-default
  until they carry a trustworthy sensitivity label. Restricted due-work listing
  omits both classes so inaccessible work cannot starve ordinary work.
- Curator tokens require a bounded display name and expiry, are valid only for
  agent principals, and cannot live longer than 24 hours.
- The trusted runner defaults to preview; apply requires both effective apply
  permission and explicit local `--apply --yes` selection.
- Sensitive memories are excluded from restricted curation. A future or manual
  full-authority sensitive flow requires a separate explicit scoped opt-in for
  the selected client/model; a curator profile cannot be widened in place.
- Human-, operator-, import-, and explicitly captured source material is
  preserved. A curator normally derives a new representation and lineage rather
  than silently rewriting source authorship.
- Plan pages, actions, bytes, model time, and apply transaction size are
  bounded. Oversized work is split at snapshot boundaries.
- Raw content, proposed content, vectors, transcript bodies, and fact values
  never enter logs, metrics labels, ordinary audit metadata, idempotency
  receipts, or error strings.
- Concurrent curators cannot silently win. Expected-version mismatch is a
  visible conflict.
- Rollback is compensating and append-only. If later edits make safe rollback
  impossible, it returns blockers rather than forcing history backward.

### Permanent Delete Carve-Out

Append-only history has one explicit exception: a same-turn, directly authorized
permanent memory deletion. It is never available to curation. The implemented
transaction locks the owner change-clock lane and target head, verifies the
exact scrub set, retains hashed retry shields, writes a value-free tombstone,
and purges the target's full versions, evidence, relations, dependent
migration-0032 vector rows, and value-bearing curation material. Vector rows
follow their exact-version foreign-key cascade; curation cleanup is explicit and
its value-free counts remain verifiable.

The tombstone retains only value-free deletion metadata: actor and timestamp,
server-owned reason code, receipt id, hash of the apply idempotency key, prior
version, scrub-set revision, purged row counts, and retry-shield count/digest.
`memory_deleted_references` currently contains only SHA-256 retry shields for
the purged version mutations and evidence-resolution mutations. It cannot carry
content, excerpts, client provenance, raw retry keys, content-derived hashes, or
curation ids.

Deletion uses a deterministic value-free preview/apply contract. Preview is
`DELETE /v1/memories/{memory_id}?dry_run=true`; it accepts the memory id only,
does not create or persist a preview object, and has no id or expiry. Apply uses
the same route without `dry_run`, binds `expected_version` and
`scrub_set_revision` as query parameters, requires `Idempotency-Key`, and
requires `X-Witself-Direct-User-Authorized: true`. The CLI expresses the last
guard as `--yes`; the MCP tool requires `direct_user_authorized: true`. The
authority must come from this turn's direct current-user request, never from
autonomous/background work, standing instructions, a subagent or delegated
task, or retrieved/untrusted content.

This header/boolean is an agent-routing assertion, not cryptographic proof of
human presence. A full agent credential that can reach the route can assert it,
so it remains inappropriate for unattended inference. The implemented
`curator-preview` and `curator-apply` profiles are rejected before that route
and are therefore technically incapable of permanent deletion. A separately
authenticated, short-lived, single-use grant bound to the target memory,
expected version, and scrub-set revision remains a possible post-v0 hardening
for interactive full-authority agents; MCP must never mint one from
model-visible context.

Preview reports incoming live memory evidence and active relation dependencies.
Apply refuses a blocked or stale scrub set and never silently cascades into a
distinct live memory. An exact replay of the same apply key and guards returns
the original value-free receipt; reusing the key for different guards conflicts.
Delayed retries of purged capture/adjust/evidence-resolution operations are
stopped by the stored shields rather than recreating payload.

A current account archive contains the tombstone and the complete retry-shield
set, not the purged payload. Import accepts a deleted memory only when its
version/evidence/relation counts are coherent, every deleted-reference row is a
value-free SHA-256 idempotency shield, and the imported shield count and digest
exactly match the tombstone. It rejects live heads carrying delete metadata,
deleted heads with payload history, and shield rows for a non-deleted target.

The operation does not delete the separate transcript ledger, native-provider
memory, prior exports, or backups still under retention; those have their own
lifecycles. Every future table that can hold memory payload must extend the
permanent-delete scrub map, archive validator, and tests in the same schema
change. This privacy purge is why “immutable” in this document means immutable
under ordinary capture, curation, correction, forget, and rollback.

## Export, Import, And Cell Migration

The implemented account archive and cell-migration path includes:

- memory heads and immutable versions;
- value-free permanent-deletion tombstones and complete hashed retry-shield
  sets, including the tombstone's expected shield count and digest;
- evidence references and their account transcript conversations/entries;
- lineage relations;
- curation lanes/cursors, requests/runs, materialized inputs, canonical actions,
  and value-free mutation receipts; and
- migration-0032 immutable vector profiles and exact
  version/content-hash-bound JSONB vector rows; and
- relevant policies/groups, audit, usage, and account identity records already
  required by the account archive.

Every schema-32 manifest includes both vector table streams, even when empty.
Import validates contracts, dimensions, normalization, exact owner/version/
content-hash binding, hashes, chronology, and profile-before-row dependencies.

Export order and import remapping preserve ids and checksums where possible.
Transcripts import before evidence-bearing memory versions. References are
revalidated after remap; unresolved references are reported, never dropped.
The current exporter accepts only a suspended or closed account, streams every
table from one PostgreSQL `REPEATABLE READ` snapshot, and holds a shared account
row lock so a concurrent resume cannot split the archive across states.

Before destination writes resume, import validates each owner change-clock
against the maximum imported memory/evidence `change_seq`. Curation-lane
request/fencing generations are restored or advanced, never lowered; imported
active work is interrupted and its old fence cannot be reused. If optional
vectors travel, their immutable profile definitions travel first and must match
before the rows are admitted.

Cell movement requires a write cutover, not just archive copy. Managed cells
freeze account mutation or advance a control-plane `placement_epoch` before
final export. Every mutating request and curation lease is bound to that epoch;
after cutover the source cell rejects the old epoch and its account credentials
are revoked/rotated. A self-hosted move without the control plane uses an
explicit write freeze and credential rotation.

An imported open or planned run with an active lease becomes `interrupted`, its
lease and fencing token are cleared, and any still-pending work returns to
`queued` only after deterministic validation. A stale worker in the old cell
cannot complete after the source freeze/epoch cutover. Clearing destination
leases alone is not a cross-cell fence.

Full-text and any future ANN indexes are derived and rebuilt in the destination
cell. Canonical profile/vector rows are carried and validated as schema-32
archive data; their semantic role is still an optional derived index over the
authoritative memory content. PostgreSQL remains authoritative before and after
migration. Object storage may transport the checksummed archive but does not
become live memory storage.

Every schema change for narrative memory must update export/import and its
round-trip tests in the same change. Portability is not a final cleanup phase.

## Multi-Cloud And Runtime Portability

The portable minimum is:

- standard containerized Go services;
- ordinary PostgreSQL with full-text search, portable JSONB vector rows, and no
  required extension;
- PostgreSQL-backed queues, leases, cursors, idempotency, and audit;
- outbound client connections to the selected Witself cell;
- checksummed account archives compatible across cell providers.

This runs without architectural changes on AWS, Azure, and Google Cloud. Cloud
KMS and object-store adapters used elsewhere in Witself do not participate in
narrative inference.

A file-backed adapter may remain for unit tests or disposable fixtures, but it
is not a deployed memory mode and cannot claim archive, curation, concurrency,
or source-of-truth compatibility. Local product development runs PostgreSQL
(for example in a container) so memory semantics do not fork from cloud cells.

Each runtime adapter implements the same logical operations:

1. transcript capture/flush where hooks exist;
2. capability-declared automatic or guided digest, task recall, and pending
   checkpoint discovery;
3. same-turn explicit capture;
4. a main-agent path that processes at most one fenced checkpoint per turn;
5. optional native-subagent or explicit legacy headless-worker optimization;
6. explicit native-memory coexistence and dual-write reporting.

The first four integration targets are Codex, Claude Code, Grok Build, and
Cursor, but each native headless path remains subject to the fail-closed probe
above. Gemini and GitHub Copilot are explicitly deferred and disabled. A future
runtime needs only an adapter when it is deliberately brought into scope; it
does not change the database or plan protocol.

## Delivery Plan

### Phase 0 — Freeze And Reconcile The Contract (complete)

- Mark the old server-side embedding, native-only narrative routing, and
  autonomous consolidation designs as superseded.
- Freeze memory JSON, evidence/lineage relations, plan primitives, idempotency,
  optimistic concurrency, lifecycle/delete semantics, cursor/fencing state
  machines, wire fields, audit events, scopes, and capability names.
- Add threat-model cases for transcript prompt injection, concurrent curators,
  recursive summaries, sensitive-model exposure, and native dual-write failure.

Exit: requirements, memory, routing, transcript, API, MCP, CLI, storage, and
export docs point to one client-inference architecture.

### Phase 1 — Narrative Storage And Direct Capture (direct slice implemented)

- Add migrations after the current migration head for stable memory heads, full
  versions, the commit-ordered change clock, evidence, lineage, and value-free
  deletion references.
- Implement agent-owned store operations with tenant isolation, idempotency,
  `expected_version`, and redaction.
- Implement pending/unavailable evidence plus append-only deterministic
  resolution.
- Wire store → server config/routes → server command → client → CLI/MCP.
- Implement `memory.capture`; make `session.end` compose it when the session
  integration is added.
- Populate the existing salient-memory digest shape deterministically.
- Extend export/import in the same pull request.

Exit: “remember the decisions we made” produces an immediate durable id/version
with exact evidence, a visible pending locator, or an explicit unavailable
reason; survives restart and archive round-trip; and has no model call in the
backend.

### Phase 2 — Deterministic Retrieval (lexical service implemented)

- Add PostgreSQL full-text and tag/filter indexes.
- Implement stable lexical/time/tag/kind/salience/recency ranking with id
  tie-breaking and score components.
- Add structured recall filters and bounded candidates.
- Integrate automatic session-start and task-preflight retrieval.

All four items are implemented to each runtime's real capability boundary.
Codex and Claude Code inject session and focused task context plus an
already-durable pending checkpoint automatically; Cursor and Grok Build use
guided MCP recall and `self.show` because their current passive-hook paths are
not reliably model-visible.

Exit: repeatable golden tests pass without an embedding provider, and a user
does not need to invoke search manually in normal runtime use.

### Phase 3 — Curation Runs And Plans (implemented)

- Add owner lanes, requests, materialized run inputs, actions, per-stream
  cursors, leases/fences, plan hashing, apply, status, and rollback.
- Add the transaction-aware fact-candidate helper and validate all five plan
  primitives.
- Add concurrent-machine, out-of-order commit/trigger, contiguous-cursor,
  lease-expiry, retry, stale-plan, all-or-nothing, redaction, and rollback tests.
- Extend export/import again.

Exit: two machines cannot overwrite each other silently; every semantic change
is attributable to an exact client plan.

### Phase 4 — Client Curator And Automation (implemented local slice)

- Package one provider-neutral runner, curation recipe, CLI driver, and bounded
  MCP workflow. (implemented)
- Add short-lived server-enforced curator credential profiles and authenticated
  effective preflight. (implemented)
- Make source commits mark due without blocking. (implemented)
- Surface value-free pending state through `self.show`; inject it automatically
  for Codex/Claude and teach Cursor/Grok the guided fallback. (implemented)
- Instruct the active foreground agent to process at most one fenced request,
  including applying an empty plan when no input merits memory. (implemented)
- Add bounded budgets, sensitive exclusion, feedback-loop exclusion, value-free
  resume, and explicit preview-to-apply policy. (implemented)
- Wire capability-probed native provider planners into the trusted CLI driver;
  headless operation works without requiring native subagents. (implemented)
- Retain the older value-free wake, debounce/single-flight, provider/consent,
  preview/apply, and scheduler implementation as explicit legacy/manual tooling;
  runtime hooks no longer invoke it. (implemented compatibility path)
- Package optional legacy persistent operating-system supervisors only for
  deployments that deliberately select separate polling. (implemented for
  per-user launchd and systemd; other service managers remain packaging
  extensions)

Exit: after one-time setup, ordinary sessions are captured and a later active
foreground turn reviews durable pending work without routine user prompts, using
only client inference. Work remains pending when no compatible turn occurs.

### Phase 5 — Runtime Expansion And Optional Vectors (current-runtime slice implemented)

- Verify and package the conditionally supported Claude Code and Grok Build
  native planners. Keep the Codex and Cursor paths failed closed until their
  installed CLIs advertise every required prompt, no-tools/no-shell,
  customization, session, and filesystem-isolation control. Gemini and GitHub
  Copilot are explicitly outside the current release and remain disabled.
- Add client-supplied memory/query vector profiles and portable deterministic
  hybrid ranking without requiring pgvector. (implemented)
- Keep full lexical functionality and report vector coverage/degradation.
  (implemented)

Exit: all runtimes pass the same capture/recall/curation conformance suite on
all three cloud cell targets.

### Phase 6 — Migration And Hardening (portable gate implemented)

- Exercise whole-account export/import between AWS, Azure, and Google Cloud
  cells with source freeze/placement-epoch fencing. (the provider-neutral 3-by-3
  gate is implemented; real managed-endpoint certification remains to run)
- Load-test queue claims, bounded plans, FTS/vector indexes, and archive rebuild.
- Evaluate duplicate growth, stale-plan rate, retrieval usefulness, recursive
  summarization drift, sensitive-data exposure, latency, token cost, and
  rollback success.
- Roll out by capabilities: direct capture → lexical recall → curation protocol
  → foreground checkpoint apply → optional legacy scheduling → optional vectors.

Exit: a moved account recalls the same canonical facts and narrative graph after
index rebuild, with complete provenance and no active lease crossing cells.

## Implemented Slice And Honest Gaps

The current checkout implements:

1. migration `0029` for `memories`, full `memory_versions`,
   `memory_change_clocks`, `memory_evidence`, `memory_relations`, and
   `memory_deleted_references`;
2. store and PostgreSQL integration coverage for capture/read/list/history,
   adjust, atomic one-to-many supersede, forget/restore/reactivate, evidence
   resolution, lifecycle and evidence constraints, idempotency, head-pointer
   integrity, deterministic permanent-delete preview/apply/replay, and retry
   shields;
3. server/client/CLI/MCP direct lifecycle wiring, including atomic supersede,
   lexical recall, and permanent deletion;
4. deterministic salient-memory hydration;
5. migration `0030` and store coverage for coalesced requests, one active fenced
   owner lane, bounded immutable inputs, strict canonical plans, all five
   reversible primitives, atomic apply, guarded rollback, read-only replay,
   cap-backlog follow-up, lease expiry, and stale-head/cursor conflicts;
6. curation HTTP server, Go client, CLI, and bounded MCP workflow with explicit
   apply and rollback guards;
7. single-snapshot logical archive export/import with narrative, curation,
   deletion-tombstone, and retry-shield graph validation;
8. migration `0031`, short-lived agent-only `curator-preview` and
   `curator-apply` credentials, route/store enforcement, restricted sensitive
   request filtering, authenticated effective preflight, and logical
   export/import preservation of credential profiles; and
9. the provider-neutral `Runner`, shell-free `CommandPlanner`, value-free
   private resume state, explicit `memory curate run` CLI driver with either
   planner argv or `--provider`, self-capture exclusion, successful-preview
   requeue semantics, and fail-closed native provider capability probes; and
10. the explicit legacy/manual `memory curate auto` configuration/status/run
    surface, read-compatible value-free wake markers, restart-safe
    debounce/backoff, single-flight execution, exact wake acknowledgement,
    explicit provider and transcript consent, preview/apply standing policy, and
    credential-free inference-child boundary, with no runtime-hook launcher; and
11. idempotent per-user launchd and systemd service/timer lifecycle, private
    managed definitions, scheduled polling, user-manager restart recovery, and
    unsuccessful-run restart without credential or provider/model exposure;
    and
12. the provider-neutral automatic-hydration executor, exact installed identity
    verification, deterministic history-dependent query gate, authenticated
    value-free checkpoint envelope, foreground one-request policy, open-plane
    advisory boundary, hard timeout/byte/candidate ceilings, fail-open Codex/
    Claude hook delivery, guided Cursor/Grok fallback, and current-runtime
    conformance matrix; and
13. migration `0032`, immutable owner-scoped vector profiles, exact
    memory-version vector writes, deterministic hybrid recall, exact lexical
    zero-coverage fallback, coverage/degradation reporting, HTTP/client/CLI/MCP
    surfaces, and logical archive validation; and
14. an opt-in AWS/GCP/Azure 3-by-3 managed-PostgreSQL conformance gate that
    applies every migration, freezes and exports an account, imports it to a
    separate destination schema, rebuilds retrieval projections, and verifies
    facts, narrative history, curation fencing, deletion shields, vectors,
    idempotency, and tenant isolation.

Model-visible Cursor session/task and Grok session/task injection, live managed
three-cloud move certification, and production performance/evaluation tuning
are not complete.
Cursor and Grok keep the managed-instruction/MCP fallback until their runtime
contracts provide a dependable injection channel. Claude Code and Grok Build
native curator planners remain conditional on their installed safety controls;
the Codex and Cursor native curator paths are unsupported rather than weakened.
Those gaps are deliberately visible; the backend does not substitute autonomous
classification, consolidation, or embedding inference for them.

## Production Readiness Checklist

The four-runtime narrative-memory contract is feature-complete, but it is not
production-certified until the three evidence-producing gates below are
complete. [Issue #47](https://github.com/witwave-ai/witself/issues/47) is the
umbrella tracker. These are pre-production gates for the implemented system,
not optional post-v0 product features.

### Gate 1 — Managed-Cloud Portability

- [ ] Complete [issue #44](https://github.com/witwave-ai/witself/issues/44):
  run the protected, release-specific AWS/GCP/Azure 3-by-3 managed-PostgreSQL
  certification described in
  [memory-cloud-conformance.md](memory-cloud-conformance.md).
- [ ] Retain the workflow URL, release tag and commit SHA, provider
  attestations, salted endpoint fingerprints, database versions, and all nine
  successful directed cases without exposing endpoint credentials.

### Gate 2 — Live Four-Runtime Acceptance

- [ ] Complete [issue #45](https://github.com/witwave-ai/witself/issues/45):
  exercise Codex, Claude Code, Cursor, and Grok Build with their real
  authenticated clients and isolated synthetic test agents.
- [ ] Verify explicit capture, history-dependent recall without a user search
  instruction, one pending foreground curation checkpoint, sensitive recall and
  broad-result redaction, same-agent continuity, and cross-agent isolation.
- [ ] Retain sanitized evidence identifying each client version, Witself release
  and commit, delivery mode, test identity, timestamp, and outcome.

The accepted delivery contract remains capability-accurate: Codex and Claude
Code receive automatic model-visible hook hydration, while Cursor and Grok Build
use the managed always-on instruction plus MCP `self.show` and `memory.recall`
fallback. The guided fallback is not a backend blocker and must not be reported
as automatic injection. A future runtime contract can upgrade that row only
after a live, version-gated conformance test passes.

### Gate 3 — Production Load, Quality, And Defaults

- [ ] Complete [issue #46](https://github.com/witwave-ai/witself/issues/46):
  load-test queue claims and fencing, bounded curation plans, lexical/vector
  indexes, archive rebuild, high-cardinality accounts, and concurrent agents.
- [ ] Establish documented baselines and operating thresholds for latency,
  throughput, queue age, stale-plan/conflict rates, duplicate growth, recall
  usefulness, summarization drift, sensitive exposure, client-side inference
  cost, rebuild duration, degraded lexical-only behavior, and rollback success.
- [ ] Measure and set the remaining production defaults:

  - checkpoint/evidence thresholds and curation budgets;
  - transcript pin duration and the identity-only export policy for
    materialized evidence excerpts; evidence is never allowed to dangle;
  - the first recommended client embedding profile; the profile contract is
    already implemented and provider-neutral;
  - debounce, minimum-interval, backoff, and service-poll defaults; provider
    selection remains explicit; and
  - ranking weights, result budgets, and which memory kinds are pinned in the
    self digest.

### Exit And Scope Boundary

Production readiness is complete only when issues
[#44](https://github.com/witwave-ai/witself/issues/44),
[#45](https://github.com/witwave-ai/witself/issues/45), and
[#46](https://github.com/witwave-ai/witself/issues/46) are closed with retained
evidence, the certified release and commit are identified, and this checklist
links the results. Optional conveniences and intelligence work remain in
[post-v0-roadmap.md](post-v0-roadmap.md); Gemini and GitHub Copilot remain
intentionally deferred and are not part of this closeout.

## Related Documents

- [memory-model.md](memory-model.md)
- [facts-model.md](facts-model.md)
- [agent-memory-routing.md](agent-memory-routing.md)
- [transcript-ledger.md](transcript-ledger.md)
- [context-hydration.md](context-hydration.md)
- [mcp-tools.md](mcp-tools.md)
- [data-model.md](data-model.md)
- [storage.md](storage.md)
- [backup-and-recovery.md](backup-and-recovery.md)
- [memory-cloud-conformance.md](memory-cloud-conformance.md)
- [decisions/0002-client-side-narrative-memory.md](decisions/0002-client-side-narrative-memory.md)
