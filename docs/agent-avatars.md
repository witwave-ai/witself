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
  every `locked_by_default` layer. This remains structural continuity; v1 does
  not claim semantic image-similarity enforcement.
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
- Each proposal is immutable. Activation changes the profile's active pointer;
  rollback points it to a prior immutable version. All mutations use
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
  retry backoff. Permanent SVG purging is deliberately outside this operation.
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
can adopt the new grammar. The client still reviews overall recognizability;
Witself does not treat structural source equality as semantic image comparison
or protection against every possible visual occlusion.

Raster previews are caches, not identity authority. The SVG, structured visual
specification, and immutable version metadata are canonical. Small v1 assets
remain inline in PostgreSQL so self-hosted installations do not acquire an
object-store dependency merely for avatars.

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

Event metadata may contain stable ids, version numbers, status, and subject
form. It must never contain SVG, prompts, descriptions, visual specifications,
or raw idempotency keys. Account audit records are the durable source for a
future generic outbound-webhook dispatcher; runtime hooks remain the mechanism
that gates the active AI client.

History reads are payload-free metadata summaries; SVG, visual specification,
description, and generation provenance require an exact version read. History
uses `limit` (default 20, maximum 100) and exclusive `before_version`; a nonzero
`next_before_version` continues the newest-first scan without overlap. Current
profile pointers produce `is_active` and `is_proposed`; activation history
produces `was_activated` and optional `last_activated_at`; server-evaluated
every profile and version exposes `lineage_generation`; rollback availability
produces `rollback_eligible` only inside the current lineage; rejection history produces
`rejected` and optional `rejected_at`. Clients must use these fields instead of
guessing lifecycle state from version order.

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
- `GET /v1/realms/{realm}/avatar-style`
- `POST /v1/realms/{realm}/avatar-style/versions`

The CLI mirrors those operations with `witself avatar ...`; all generated SVG
payloads are file inputs or MCP values, never shell arguments.
