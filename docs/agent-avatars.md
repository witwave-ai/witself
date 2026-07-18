# Agent avatars

Witself gives every named agent a portable, versioned visual identity. The
backend stores and validates avatar state but performs no model inference. An
active AI client creates or evolves an SVG through MCP, while CLI-only agent
creation always has a deterministic fallback.

## Product contract

- Every agent has an avatar profile. A newly created agent starts in
  `generation_due` with a deterministic placeholder derived from its immutable
  agent id and current name.
- Agents in the same realm inherit the same versioned style pack. The built-in
  `Witself Flat Portrait v1` pack supplies human, animal, and insect references
  plus one common canvas, crop, palette, line language, background motif, and
  SVG layer vocabulary.
- Built-in placeholders retain deterministic seed-derived variation. For an
  operator-authored style pack, the model-free fallback uses the pack's neutral
  human reference artwork because Witself has no creative renderer. That SVG
  may therefore be visually identical across several new agents, while each
  placeholder resource id remains deterministically agent-scoped. AI generation
  supplies the agent-specific artwork later.
- Subject form and visual style are independent. A human, fox, beetle, robot,
  or symbolic agent can still look like part of one team.
- The agent name is the strongest creative seed for initial AI generation. An
  evolution also receives the active SVG, description, visual specification,
  style pack's locked-by-default layer guidance, style-pack version, and parent
  version so identity is not regenerated from scratch. The client reviews
  visual continuity. For self-authored evolution under the exact same style
  version, the server also preserves the subject form and normalized source of
  every `locked_by_default` layer. It also enforces semantic visual continuity
  with deterministic `perceptual-v1` canonical renders: gross whole-portrait
  replacement and unlocked artwork that visually covers too much locked
  identity are rejected. This is bounded perceptual comparison, not a model,
  embedding, or backend inference claim about the meaning of an image.
- Initial fitting belongs to the active agent. It creates and inspects its
  first draft from its own perspective and may make one to three substantial
  local revisions when it wants changes, including a different subject form,
  facial hair, eyewear, eye color, palette, accessories, or expression. This is
  not a human approval dialog. Unchosen drafts are ephemeral and non-durable:
  they never enter repository or project files, temporary artifacts are cleaned
  up, and they are not proposals, history, or server state. The agent submits
  only its one chosen final SVG, description, and visual specification. An
  accidentally accepted proposal cannot be withdrawn from immutable history.
- An active agent may propose a voluntary evolution when its operator asks or
  when client-visible work reaches a meaningful identity or experience
  milestone; it does not need a server checkpoint for that proposal. This is
  deliberately event-driven rather than age-, time-, or token-driven. Routine
  work should not be interrupted for cosmetic churn, and each attempt still
  uses the exact active parent, selected style, autonomy policy, and one bounded
  lifecycle transition. The unlocked `experience`, `expression`, and `attire`
  layers are the normal places for gradual change.
- Each proposal creates a durable version record whose identity and lifecycle
  metadata are immutable. Its public, value-free `renderer_profile` records
  the exact rendering contract as either `perceptual-v1` or `legacy`.
  Activation changes the profile's active pointer;
  rollback points it to a prior full-payload version.
  Quota compaction may later remove only an eligible inactive version's SVG,
  description, and visual specification; it never rewrites that version's
  identity, hashes, generation provenance, proposer, lineage, style, or
  lifecycle ledgers. All mutations use
  idempotency and optimistic concurrency. An exact idempotent replay returns
  the original value-free receipt together with the resource's current
  projection; it never rewinds mutable profile state to an older response.
  While a proposal is pending, both replacement proposals and rollback are
  rejected; the pending version must be activated or rejected first.
- An explicit reset is a non-destructive fresh start, not a purge. It retires
  the current lineage, clears its active and proposed pointers, returns the
  profile to the deterministic human placeholder in `generation_due`, and
  starts the next positive `lineage_generation`. Global avatar version numbers
  and the retired lineage remain immutable and portable, but retired versions
  are no longer rollback targets. The first proposal in the new lineage has no
  parent even though its global version continues from the historical head.
  A self reset is available only under `agent_self_managed`; the other policies
  require an account operator to execute it. Reset requires an active or
  proposed avatar, so it cannot churn an empty lineage or bypass generation
  retry backoff. Reset itself does not purge SVG data, although the retired
  lineage becomes the first class eligible for later quota compaction.
- `operator_only`, `agent_proposes`, and `agent_self_managed` are explicit
  autonomy policies. Payload, style, parent-version, and SVG validation apply
  in every mode. An `operator_only` agent sees `awaiting_operator` as
  informational state rather than an impossible foreground action, so its
  client is never trapped retrying a mutation it cannot authorize.
- Avatar state is open-plane identity data. It is portable with the account,
  available to the authenticated agent and account operators through the v1
  routes below, and never enters the sealed credential plane.
- `witself self card` is the bounded presentation surface for that identity. It
  combines the identity-only self read with the active avatar or deterministic
  placeholder, verifies the canonical SVG and content hash, and may render a
  fixed in-memory terminal portrait. Its plain and JSON forms deliberately omit
  SVG, visual specifications, facts, memories, checkpoints, and pending
  proposal content. The card, JSON document, and SVG hash are unsigned
  presentation data, not authentication, authorization, or a legal credential.
  The existing `self show`, API, and MCP contracts remain unchanged.

## Runtime flow

1. Agent creation establishes the placeholder and `generation_due` state.
2. `GET /v1/self` exposes an authenticated, value-free `avatar_checkpoint`.
3. At an explicit avatar or pending-self-maintenance request, or near the end of
   an eligible non-trivial foreground turn, the active client keeps the user's
   requested work first and may handle at most one bounded lifecycle attempt
   before its final response. Avatar housekeeping never interrupts or replaces
   that work, and the final response remains self-contained about the user's
   task. A tiny read-only, lookup, or status turn may defer the opportunity: the
   checkpoint stays pending, its attempt count remains unchanged, and the
   client does not record a generation failure merely because it deferred.
4. The client reads the profile and realm style pack through MCP. From the
   agent's own perspective, it creates and inspects an ephemeral SVG draft,
   description, and structured visual specification. When it wants changes, it
   may make one to three substantial local revisions before settling on a
   design. It does not ask the user or operator to make the creative choice.
5. The client cleans up unchosen variants without putting them in repository or
   project files and submits exactly one chosen final proposal. Witself validates
   and sanitizes that candidate, applies the autonomy policy, and stores one
   immutable version.
6. With `agent_self_managed`, the client activates that exact returned version
   and revision in the same bounded attempt. Activation records the agent's
   acceptance and settles its chosen avatar. Under an operator-governed policy,
   the agent's creative selection is complete, but identity remains unsettled;
   it leaves that one proposal pending until operator activation.
7. An actual failed bounded attempt keeps the placeholder, records a value-free
   failure state, and permits a later retry; it never traps every future turn.
   The server-stamped `retry_after` time is enforced on agent proposals and
   repeated failure reports, not merely advertised through the checkpoint. An
   operator may still recover the agent during that interval.

Deferral is not a lifecycle attempt or failure. It neither advances retry state
nor consumes an attempt; the same authenticated checkpoint remains available
for a later eligible foreground turn.

If a later checkpoint has reason `activation_due`, the client activates the
existing proposal and must not generate a replacement. Generation failures are
recorded only when no proposal is pending; activation failures leave the
immutable proposal in place for a later fenced retry.

`proposal_rejected` also re-enters broad fitting because no active version
exists. `retry_due` branches on the fresh profile read: without an
`active_version` it re-enters broad fitting; with one, it is a bounded
single-candidate evolution retry. `style_changed` likewise follows the bounded
evolution path from the active parent rather than restarting identity.

After a successful reset, the checkpoint reason is `avatar_reset`. The client
uses the same bounded, agent-owned initial fitting flow, but reads the new exact
`lineage_generation`, keeps `parent_version` empty, and continues the global
version sequence. Reset reopens broad fitting freedom: the agent may change its
subject form, palette, and defining details substantially, keeps unchosen drafts
ephemeral, cleans up temporary artifacts, and submits only its chosen final
candidate. Natural-language routing requires explicit fresh-start intent such
as "start my avatar over from scratch"; ordinary dissatisfaction or a request
for gradual improvement remains an evolution, not a reset.

Codex and Claude can receive the checkpoint through model-visible hook
additional context. Cursor and Grok use the managed-instruction/MCP fallback
until their passive hooks can reliably inject model-visible context. Neither a
hook, MCP server, webhook, nor Witself itself wakes an idle model.

## SVG safety

The server accepts a small, self-contained SVG subset. It rejects scripts,
event attributes, `foreignObject`, animation, external resources, network or
file URLs, unsafe data URLs, CSS imports, entity tricks, and documents above
the configured size budget. The sanitized canonical SVG is hashed and stored;
the original untrusted payload is not retained.

Realm style packs are aggregate-bounded as well as field-bounded. A style that
cannot fit the durable JSONB contract is rejected as client input before a
transaction starts rather than failing later as a database error.

Every new proposal, style-pack reference, and generated placeholder must also
satisfy the versioned `perceptual-v1` renderer profile. That profile leaves the
released generic SVG sanitizer unchanged for old data, but narrows new renderer
inputs to a square finite canvas, bounded coordinates, strokes, path groups,
arc geometry, and deterministic raster-work budget. It rejects transforms,
definitions, clipping, gradients, paint URLs, percentage geometry, alpha-hex
paint, root or group opacity, alternate aspect-ratio behavior, and other
renderer-dependent features. In the canonical 96 by 96 projection, a new
baseline's locked-identity mask must cover no more than 6,144 pixels and must
cover at least 1,152 pixels inside a fixed centered portrait-focus ellipse.
The general 48-pixel mask floor remains a low-level sanity bound, but the
centered-focus requirement is the effective minimum for new baselines.
Renderer parse, work, or identity-mask uncertainty fails closed.

Versions created before this contract, plus rows written during a mixed-version
rollout by a server that omits the new field, are explicitly quarantined as
`legacy`; compatible-looking bytes are never promoted by inspection. Legacy
versions remain readable and exportable, but never create or consume a
perceptual continuity fingerprint; schema and archive upgrades discard any
pre-profile fingerprint because it cannot prove the renderer contract. A
legacy parent cannot be promoted to `perceptual-v1` by same-style self
evolution. Mixed-writer `perceptual-v1` to
`legacy` edges and `legacy` to `legacy` edges remain valid quarantined history
only when their verified locked-layer digests match; when both SVGs remain,
their normalized locked layers must also match exactly. An operator replacement,
an explicit reset followed by a parentless proposal, or a proposal under a newly
selected style creates a fresh `perceptual-v1` baseline while preserving the
legacy row as immutable history. During rollout only, a new client interprets a
missing wire field from an older server as `legacy`; it does not persist that
interpretation. Rich self-card rendering uses the strict profile and falls back
to the plain card when legacy SVG is outside it.

For evolution, the backend requires the active parent and the selected style
version and enforces the pack's canvas, palette, and layer structure. A
self-authored evolution under the exact same immutable style version must keep
the same subject form and normalized source for every locked-by-default layer.
The normalized projection includes root and wrapper presentation state, so an
ancestor transform, opacity, inherited paint, paint opacity, or framing change
cannot bypass the comparison. Locked layers may not depend on definitions,
paint-server URLs, or clipping outside their projection. Operators can
deliberately override locked-layer and subject continuity, and a self-authored
migration after an operator selects a new style version is exempt so the agent
can adopt the new grammar. The client can additionally review higher-level
recognizability. Witself's deterministic raster comparison is the enforced
semantic visual-continuity boundary, but it is deliberately bounded: it does
not infer image meaning or protect against every possible visual occlusion.

The model-free raster guard renders five fixed 96 by 96 RGBA projections: both
whole portraits, the parent's locked identity mask, and each version with only
its locked layers visible. A locked-identity mask outside the baseline bounds
fails closed; it never falls back to a broad background or generic locked-layer
mask. Change and occlusion metrics use every pixel in the fixed centered
ellipse, independently of the parent-supplied mask, so a client cannot hide a
drastic head-and-shoulders repaint by moving or enlarging its locked shape.
Inputs are already limited to
64 KiB, 512 elements, depth 32, and 1,024 bytes per attribute, so work and
image memory remain bounded. Ordinary wrapper groups are preserved by projection;
declared `data-layer` groups cannot nest because style validation requires
visible geometry to belong to exactly one declared layer. A pixel counts as
changed above normalized delta `0.12`. The proposal is rejected when
whole-portrait changed pixels exceed `0.42` or mean delta exceeds `0.20`, when
locked-identity changed pixels exceed `0.46` or mean delta exceeds `0.24`, or
when newly added visible unlocked-layer influence covers more than `0.30` of
the locked identity mask. Within the centered focus, the independent limits
are `0.26` changed pixels, `0.13` mean delta, and `0.30` newly added unlocked
influence. Visible influence is the per-pixel delta between the whole portrait
and its locked-only render, so opaque unlocked artwork hidden behind locked
layers does not become a false occlusion. Calibration leaves
ordinary expression, attire, and experience edits well below those limits
while rejecting full-canvas replacement and a face-covering front overlay. The
comparison is deterministic pure Go, and renderer failure is fail-closed. It
applies only where structural continuity already applies: self-authored
evolution under the same style version. Operator override and an
operator-selected style migration retain their existing exemptions.

The pure avatar domain also exposes a versioned continuity fingerprint for a
parent whose historical SVG will later be compacted. Version 1 is exactly
38,092 bytes: a small fixed header, an exact style-pack digest, the parent's
96 by 96 composite RGB bytes, a binary locked-identity mask, visible
unlocked-layer influence bytes, and a SHA-256 corruption checksum. The focus
guard is derived from the existing whole-RGB, identity-mask, and unlocked-
influence projections, so these stricter decoder and comparison rules do not
change the version 1 wire length or layout. A policy golden pins the exact
thresholds and centered-focus bitmap to the profile/fingerprint version.
Any future change to the focus geometry, thresholds, or projection semantics
after release requires a new perceptual profile or fingerprint version. The normal
full-parent guard builds and consumes this same format, so comparison across a
retained-SVG and compacted-parent boundary is behaviorally identical rather
than an approximation. Decoding requires the exact magic, version, render
size, reserved flags, payload length, total length, checksum, and style digest.
Quota compaction builds this fingerprint only from `perceptual-v1` bytes before
clearing a parent SVG when a
retained full direct child in the same lineage and style still depends on that
boundary and was proposed by the owning agent. The exact 38,092-byte value is
stored only on that compacted parent; full versions forbid it, and later
compaction prunes it as soon as no retained child needs it. Although smaller
and readily TOAST-compressible, the projection still contains raster-derived
avatar content. It is internal boundary evidence, is not returned by exact or
history reads, and follows the SVG's access, identity-export, and deletion
controls rather than value-free metadata controls. A checked-in compressed WAPF
v1 fixture pins the exact binary hash, renderer output, decoder, and comparator;
an intentional projection change requires a new format version rather than a
silent rewrite of historical boundaries.

Raster previews are caches, not identity authority. For a `full` version, the
SVG, structured visual specification, and immutable version metadata are
canonical. Small retained assets remain inline in PostgreSQL so self-hosted
installations do not acquire an object-store dependency merely for avatars.
Every version also retains `svg_sha256` and `locked_layers_sha256`; those hashes
remain available after creative-payload compaction.

## Payload retention and compaction

Each agent has two operator-configurable retained-content limits:

- `retained_payload_count_limit`: default `20`, allowed range `4`–`1000`.
- `retained_payload_byte_limit`: default `2097152` bytes (2 MiB), allowed range
  `524288`–`67108864` bytes (512 KiB–64 MiB).

`payload_bytes` records the original retained creative-payload size: canonical
sanitized SVG bytes plus the normalized description and canonical visual
specification. `retained_payload_count` counts `full` rows.
`retained_payload_bytes` is the storage-bearing total governed by the byte
limit: every `full` row's `payload_bytes` plus every retained 38,092-byte
continuity fingerprint on a compacted row. The profile reports both limits and
both current totals plus the fixed `rollback_payload_floor` of `2`. Raising a
limit never reconstructs a payload that was already compacted.

Compaction runs transactionally before a new proposal is inserted and whenever
an operator lowers either limit, but only after the process-lifetime activation
gate `WITSELF_AVATAR_PAYLOAD_COMPACTION_ENABLED=true`. With the gate false,
quota accounting and limits remain visible and enforced: a proposal or quota
change succeeds when the resulting retained totals already fit, while one that
would require cleanup fails without mutation with HTTP `409` and stable error
`avatar_payload_compaction_not_active`. That conflict is retryable after the
activation rollout. A quota update and every enabled compaction it requires
commit together; if the requested limits cannot be met, neither the new limits
nor any payload changes persist. Exact idempotent replays return the original
receipt and current projection without running compaction again.

The planner never compacts the active version, a pending proposed version, or
the two most recently activated distinct inactive versions in the current
lineage. Those two retained versions are the documented rollback floor; when
fewer than two exist, every available member of the floor stays full. Eligible
payloads are considered in this lifecycle order:

1. Retired-lineage versions, oldest version first.
2. Rejected current-lineage versions, oldest first.
3. Other never-activated, non-rollbackable current-lineage versions, oldest
   first.
4. Activated current-lineage versions older than the rollback floor, oldest
   first.

If all eligible payloads are exhausted and protected payloads plus an incoming
proposal still do not fit, the whole mutation fails closed with HTTP `409` and
the stable error `avatar_payload_quota_exceeded`. No partial compaction, new
proposal, profile revision, receipt, or lifecycle event is committed.

The planner indexes same-lineage, same-style, same-subject, owning-agent
`perceptual-v1` parent and child relationships and existing fingerprints before
selection. For each candidate it projects the full-payload bytes removed, a new
fingerprint required by any retained qualifying v1 child, and a fingerprint
pruned when its final qualifying child is also compacted. A candidate is
selected only when the projected final retained content does not exceed the
pre-cleanup footprint, including an incoming proposal. Consequently a small
parent SVG stays full when replacing it with a 38,092-byte boundary would grow
retained content; it may become eligible after another deterministic cleanup
supplies enough offsetting reclamation. Planning stops only when the full-row
count and inclusive retained-byte total both fit.

A same-lineage, same-style, owning-agent direct child that changes
`subject_form` is corrupt historical self-evolution, not a different boundary
class. If a compaction plan would clear either that parent or that child, the
transaction fails closed and leaves both payloads unchanged.

An obsolete fingerprint is reclaimed independently before any SVG is selected.
That cleanup can satisfy a byte limit with zero payload compactions, including
when every full payload is protected. The transaction still performs the
continuity checks, executes the fingerprint prune, and verifies the resulting
database count and byte total before it updates a quota or admits a proposal.

Every enabled quota pass also pages the complete same-style, owning-agent edge
graph that is not v1 on both sides. It rejects a subject-form change and
`legacy` to `perceptual-v1`, verifies each available SVG against its stored
locked-layer digest, requires equal parent and child digests, and performs exact
normalized locked-layer continuity when both payloads are full. These
quarantined edges never require WAPF, including a compacted v1 parent with a
retained legacy child; the equal verified digest is sufficient when the parent
SVG is gone. A failure rolls back the entire quota mutation even when no payload
needed compaction.

Before a selected v1 parent SVG is cleared, compaction builds the exact
fingerprint from that still-full source and validates every qualifying retained
full v1 child against both the stored/derived locked-layer digest and the
fingerprint-based perceptual guard. A directly corrupted child that violates
either boundary aborts the transaction; the parent remains full and no partial
quota cleanup is committed.
Before compaction clears the final qualifying full child of an already
compacted parent, it requires a fingerprint and validates its stored format,
checksum, and immutable style binding, then rechecks the child's stored and
derived locked-layer digest and fingerprint-based perceptual continuity. A
missing or corrupt historical boundary therefore fails before the child can be
cleared or the fingerprint pruned, leaving both rows unchanged.

Compaction is irreversible. It changes `payload_state` from `full` to
`compacted`, clears SVG, description, and visual specification, and records
`payload_compacted_at` with `payload_compaction_reason=quota`. The immutable
version id, version/parent/lineage, subject and style, original `payload_bytes`,
`svg_sha256`, `locked_layers_sha256`, generation provenance, proposer, proposal
timestamp, and activation/rejection/reset history remain. A compacted parent
also retains its exact perceptual continuity fingerprint only while a full,
same-lineage, same-style, same-subject, owning-agent direct child outside the
compaction plan needs it; all obsolete fingerprints are cleared in the same
transaction. A compacted version is never active, proposed, or
rollback-eligible.

Exact-version reads continue to return HTTP `200` for a compacted version. They
return its retained metadata and provenance with `payload_state=compacted`, but
omit `svg`, `description`, and `visual_spec`. History is always payload-free and
includes `payload_state`, `payload_bytes`, optional `payload_compacted_at`, and
optional `payload_compaction_reason`, so clients do not mistake a compacted
version for a missing resource.

Identity archives represent this state explicitly. Full rows export their
creative fields; compacted rows export those fields as `null` while retaining
the hashes, provenance, payload accounting, compaction metadata, and a
continuity fingerprint only when a retained child requires one. Import accepts
only the exact fingerprint length, checksum, version, and archived style digest;
full rows and compacted parents without a qualifying child must not carry one,
while a compacted parent with a qualifying child must. Import rejects an archive
that compacts an active, proposed, or protected rollback version.
Current-schema import also rejects a retained full-payload count or inclusive
retained-content byte total that exceeds the archived profile limits, so restore
cannot bypass the live quota invariant. The sole exception is an archived
profile with `payload_quota_reconciliation_required=true` that still contains
unreconciled legacy or mixed-writer history. A marked overage must retain at
least one full `legacy` version, its retained `perceptual-v1` subset must
independently fit both archived limits, and total retained content must remain
within the hard transition ceiling of 1,000 full payloads and 64 MiB. The marker
remains portable and is cleared only when an enabled proposal or quota mutation
successfully reconciles the retained history.

For a same-style agent-authored v1 evolution, import validates normalized
locked-layer source and the perceptual guard when both payloads are full. Across
a compacted-v1-parent/full-v1-child boundary it requires the retained
`locked_layers_sha256` to match and runs the same perceptual comparison from the
stored fingerprint without attempting to render a missing parent SVG. For
`perceptual-v1` to `legacy` and `legacy` to `legacy`, import requires equal
verified locked digests, performs exact locked continuity when both SVGs are
full, and requires no fingerprint. It rejects `legacy` to `perceptual-v1`. A
schema downgrade that would need to reconstruct a compacted payload is refused.
The schema-51 startup finalizer derives legacy locked-layer digests in bounded
transactions with a batch-scoped style cache. The digest remains nullable so a
schema-50 writer can keep inserting throughout a rolling deployment; detail and
history reads derive a missing full-row digest, export repairs frozen-account
rows before streaming, and import derives a missing full-row digest from the
validated SVG and style. A compacted row without its digest cannot be repaired
and fails closed.

### Compaction rollout

Schema 51 installs a `BEFORE INSERT` compatibility trigger that derives
`payload_bytes` when a schema-50 writer omits it and marks that profile for
quota reconciliation when the writer omits the locked-layer digest. Deploy the
compatible binary with `WITSELF_AVATAR_PAYLOAD_COMPACTION_ENABLED=false` and
wait until every old writer has drained. During this mixed window, no creative
payload is cleared. Operationally freeze all avatar mutations for the short
convergence window: proposals, activations, resets, rollbacks, style publishes,
quota edits, and avatar-bearing import or export. Compatibility keeps a late
schema-50 write data-safe, and schema 54 records it as `legacy`, but avoiding new
legacy active rows eliminates a later operator replacement. Then set the flag
to `true` in a separate configuration change. The Helm ConfigMap checksum
restarts all replicas; startup reruns the bounded nullable digest backfill
before readiness and before any request can compact data. Do not roll back to a
schema-50 binary after the database has advanced to schema 54; use a forward fix
instead.

## Large-realm style rollout

Selecting a new realm style is immediate, while projecting that selection onto
agent profiles is a durable bounded rollout. The publish transaction creates a
`pending` job and fences any older `pending` or `running` job. Server replicas
then reconcile at most `WITSELF_AVATAR_STYLE_ROLLOUT_BATCH_SIZE` mismatched live
profiles per transaction, no more often than
`WITSELF_AVATAR_STYLE_ROLLOUT_INTERVAL`. Every replica may run the worker: the
locked job row is the cross-replica fence. Profiles carry a selected-style
revision, and a concurrent expression index lets each batch read only older
revisions. Rows leave that range transactionally, so restart, deletion, and
concurrent profile changes cannot leave a cursor gap or force repeated scans of
an already-updated realm.

`GET /v1/realms/{realm}/avatar-style` and the corresponding operator CLI
read expose a value-free `rollout` block with status, target and processed
profile counts, batch count, last batch size, failure class/backoff, and
lifecycle timestamps. Publish never scans a large realm: target count is absent
while work remains and is finalized to the number of live profiles actually
projected when the job completes or is superseded. Self-agent HTTP and MCP
style reads omit this operator-only team-size and scheduler telemetry.

The worker never rewrites an immutable avatar version or active pointer. For a
profile it updates the next-generation style, clears one stale proposal
pointer, preserves the active subject form, resets generation failure state,
and moves the profile to `generation_due` or `evolution_due`. A profile is
updated only while mismatched, so retries do not repeatedly advance its
revision. New agents serialize with style publishing and inherit the selected
style revision directly. Deleted agents receive only the internal revision
fence so they leave bounded scans; their avatar projection is not rewritten. A
job for a deleted realm is durably superseded.

Suspended accounts are not discovered by workers. A suspended archive carries
the exact job and counters, and the destination resumes processing only after
the account is explicitly resumed. Closing an account terminally supersedes
every open rollout in the same account-locked transaction; archives for a
closed account are rejected if they try to restore an open job. During a
mixed-version deployment, the worker also reconciles a closed account left
with an open job by an older writer. The worker settings are strictly bounded:
batch size 1-1000, interval 100ms-1h, and worker-attempt/batch timeout
100ms-5m; defaults are 100, 2s, and 30s. The timeout is enforced by both the
whole-tick client deadline, each candidate cancellation scope, and
transaction-local PostgreSQL lock/statement timeouts. Caller cancellation is
not persisted as a job failure. A failed job receives a value-free error class
and bounded durable backoff, then the same tick may advance another realm. Set
`WITSELF_AVATAR_STYLE_ROLLOUT_ENABLED=false` only for an intentional operator
pause, because style publication remains available and queues durable work.

## Lifecycle events

Avatar changes emit value-free, transactionally coupled lifecycle events:

- `avatar.generation.requested`
- `avatar.proposed`
- `avatar.activated`
- `avatar.evolved`
- `avatar.rejected`
- `avatar.generation.failed`
- `avatar.rolled_back`
- `avatar.reset`
- `avatar.policy.changed`
- `avatar.style.changed`
- `avatar.quota.changed`
- `avatar.payload.compacted`
- `avatar.style.rollout.completed`
- `avatar.style.rollout.superseded`

Event metadata may contain stable ids, version numbers, status, subject form,
and value-free quota counts and byte totals. `avatar.quota.changed` is attributed
to the operator; `avatar.payload.compacted` is a system event coupled to the
proposal or quota update that caused compaction. Events must never contain SVG,
prompts, descriptions, visual specifications, hashes, provenance, or raw
idempotency keys. Account audit records are the durable source for a future
generic outbound-webhook dispatcher; runtime hooks remain the mechanism that
gates the active AI client.

History reads are payload-free metadata summaries. A full version's SVG, visual
specification, description, and generation provenance require an exact version
read; a compacted exact read retains provenance but no longer has the three
creative fields. History
uses `limit` (default 20, maximum 100) and exclusive `before_version`; a nonzero
`next_before_version` continues the newest-first scan without overlap. Current
profile pointers produce `is_active` and `is_proposed`; activation history
produces `was_activated` and optional `last_activated_at`; server-evaluated
every profile and version exposes `lineage_generation`; rollback availability
produces `rollback_eligible` only for a full inactive version inside the current
lineage; rejection history produces `rejected` and optional `rejected_at`.
Payload retention fields identify full versus compacted history. Clients must
use these fields instead of guessing lifecycle or payload state from version
order.

Each optional generation-provenance label (`runtime`, `model`, `recipe`, and
`recipe_version`) is untrusted audit metadata bounded to 256 bytes. Labels
start with an ASCII letter or digit and otherwise accept the portable
identifier punctuation plus internal ASCII spaces, preserving provider display
names such as `GPT-5.6 Sol`. Outer whitespace is normalized; controls,
non-ASCII characters, quotes, and markup remain invalid inside a label.

## Initial surfaces

Agent-token surfaces:

- `GET /v1/self/avatar`
- `GET /v1/self/avatar/history`
- `GET /v1/self/avatar/versions/{version}`
- `GET /v1/self/avatar/style`
- `POST /v1/self/avatar/proposals`
- `POST /v1/self/avatar:activate`
- `POST /v1/self/avatar:rollback`
- `POST /v1/self/avatar:reset`
- `POST /v1/self/avatar:generation-failed`
- MCP `witself.avatar.show`, `witself.avatar.history`,
  `witself.avatar.version.show`,
  `witself.avatar.style.show`, `witself.avatar.propose`,
  `witself.avatar.activate`, `witself.avatar.rollback`, `witself.avatar.reset`, and
  `witself.avatar.generation.fail`

Operator surfaces manage one account-scoped target without weakening the
self-only agent routes:

- `GET /v1/agents/{agent}/avatar`
- `GET /v1/agents/{agent}/avatar/history`
- `GET /v1/agents/{agent}/avatar/versions/{version}`
- `POST /v1/agents/{agent}/avatar/proposals`
- `POST /v1/agents/{agent}/avatar:activate`
- `POST /v1/agents/{agent}/avatar:reject`
- `POST /v1/agents/{agent}/avatar:rollback`
- `POST /v1/agents/{agent}/avatar:reset`
- `PATCH /v1/agents/{agent}/avatar-policy`
- `PATCH /v1/agents/{agent}/avatar-quota`
- `GET /v1/realms/{realm}/avatar-style`
- `POST /v1/realms/{realm}/avatar-style/versions`

The CLI mirrors those operations with `witself avatar ...`; all generated SVG
payloads are file inputs or MCP values, never shell arguments.
