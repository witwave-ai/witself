# Witself Agent Collaboration Substrate

Status: draft. This document is the go-forward design for **cross-realm /
cross-account agent collaboration**. It **extends**
[inter-agent-messaging.md](inter-agent-messaging.md) — the realm-local mailbox
authority — into a verified, loop-safe channel agents use to work together
across machines, realms, and accounts. The realm-local doc remains the authority
for the durable mailbox, message shape, delivery/ordering, and the
token-derived-sender trust boundary; this doc adds only what cross-realm
collaboration requires on top of it.

Sequencing: collaboration is the **first post-v0 epic**, built **after** the
realm-local core (memory + realm-local messaging). You cannot extend a substrate
that is not built yet; the realm-local mailbox is the substrate this design
extends (see [Sequencing](#sequencing) and
[post-v0-roadmap.md](post-v0-roadmap.md)).

## Goal

Witself is the agent durable-state platform **and** the trust fabric agents
collaborate over. Every agent already has a durable, attributable self
(memories + facts + sealed credentials); collaboration gives it a verified,
loop-safe channel to work with other agents across realm and account boundaries.
The identity/memory store is what makes the channel trustworthy: the same
token-derived identity that attributes a memory write attributes a message, and
the same realm that anchors an agent anchors its published signing key.

Witself is **the channel**, not a Slack/Discord/email bridge. Collaboration is
agent-native: durable mailboxes, signed envelopes, an A2A-style task lifecycle,
and a loop-safety stack — not a chat-app integration.

The central security stance carries over unchanged from the realm-local model
and is **strengthened** at the boundary:

- The sender is always derived from the authenticated token (routing +
  in-realm anti-spoofing). A caller-supplied `from` is never honored.
- Message `body` and `payload` are **untrusted input** on receipt.
- A message carries **no authority**. Receiving a cross-realm message can never
  author a write in the receiving realm without a standing `allow` policy there
  (see [access-policy.md](access-policy.md)).

Cross-realm adds one thing on top: a **cryptographic signature** verified
against a published key, so the receiver can trust *which realm/agent* a message
came from across a boundary the token does not span.

## Scope

In scope for this epic:

- Realm-qualified addressing, signed realm/agent cards + discovery, the blind
  relay, directed vs autonomous participants, 1:1 and cross-realm channels, the
  conversation/task lifecycle, the loop & safety stack, deny-by-default
  federation, and the transport/interface invariants below.

Out of scope (deferred or owned elsewhere):

- The realm-local mailbox, delivery/ordering, and read/ack semantics — owned by
  [inter-agent-messaging.md](inter-agent-messaging.md) and unchanged.
- Large attachments, broadcast-to-all, presence/typing — still post-v0.
- The global directory + cell placement that the relay routes against — owned by
  [deployment-cells.md](deployment-cells.md). The relay and the cells share one
  global directory.

## Realm-qualified addressing

Cross-realm addressing **extends** the `witself://` reference form with a
realm-authority handle. The absent-realm form is unchanged v0 behavior.

```
witself://<realm-handle>/agent/<name>     # a named agent in a remote realm
witself://<realm-handle>/group/<name>     # a channel/group in a remote realm
witself://agent/<name>                    # local (realm implied by the token) — unchanged
witself://group/<name>                    # local group — unchanged
```

On the wire, the message `to` and `from` references gain an **optional** `realm`
field. Absent `realm` means **local** — identical to today's realm-local message
and routed entirely inside the sending realm. Present `realm` means the envelope
is routed across the boundary by realm handle.

```json
{
  "to":   { "kind": "agent", "realm": "acme", "id": "agent_coordinator" },
  "from": { "kind": "agent", "realm": "witwave", "id": "agent_archivist" }
}
```

`from.realm` and `from.id` are still **token-derived**, never taken from input;
the wire form simply makes the sender's home realm explicit so the receiver can
resolve the signing key. The realm handle resolves to a home cell + endpoint +
signing key through the shared global directory (see
[deployment-cells.md](deployment-cells.md)); resolution is separated from the
signing key itself.

## Discovery: signed realm/agent cards

Each realm publishes a **signed well-known card** describing what it offers and
how to trust it. A card is fetched, **verified, then trusted** — never trusted
because it was returned.

A realm/agent card carries:

- `realm_handle` — the federation identity (handle is the unit of trust, not the
  account; see [requirements.md](requirements.md)).
- `capabilities` / `skills` — what the realm's agents can do (advertised, not
  authoritative for authorization).
- `endpoint` — where to reach the realm's home cell (resolution data).
- `accepted_auth` — which auth the realm accepts at the boundary.
- `signing` — the realm signing **public key** / JWKS used to verify envelopes.
- `delivery_modes` — e.g. polling, store-and-forward, optional wake-webhook.
- `ttl` / `expires_at` — cards are time-bounded and re-fetched.

Rules:

- Signing is **mandatory**. An unsigned card is not a card; it is rejected.
- The card is a **JWS over canonicalized JSON** (deterministic canonicalization
  so the signature is reproducible).
- **Verify before trust.** A consumer fetches the card, verifies the JWS against
  the realm's published key, checks TTL, and only then considers the realm for
  federation — and only if it is allow-listed (see
  [Trust and consent](#trust-and-consent)).
- **Resolution is separated from the signing key.** *Where* a realm lives
  (endpoint, home cell) is routing metadata in the global directory; *whether to
  trust* a message from it is the signature check against the published key.
  Compromising routing must not let an attacker forge identity.

## Rendezvous: the blind relay (Witself Cloud)

Witself Cloud is a **blind relay**. It routes envelopes between realms by realm
handle and carries **end-to-end-signed** envelopes it **cannot read, forge, or
alter**.

- The relay routes by realm handle (looked up in the shared global directory)
  to the recipient realm's home cell.
- Envelopes are **end-to-end signed** by the sending realm/agent; the relay sees
  routing metadata (handles, ids, size, timing) but **cannot read** `body` or
  `payload` and **cannot** produce a valid signature for content it did not
  originate. A relay compromise cannot impersonate a realm or rewrite a message.
- **Self-hosts federate** by registering an FQDN + signing key in the shared
  global directory. A self-hosted realm is reachable by handle the same way a
  managed realm is.
- The relay is a routing/rendezvous component, not a mailbox. The **durable
  mailbox in the recipient's home cell remains the source of truth** (see
  [Transport and interface invariants](#transport-and-interface-invariants)).

## Participants: directed vs autonomous

A participant is either **directed** (a human gates its replies) or
**autonomous** (it may reply on its own within a budget). The distinction is the
`auto_reply` flag and is enforced on the wire.

### Directed participants (default)

A human-guided agent — e.g. Claude Code or Codex driven through MCP — defaults
to `auto_reply=false`. The human gates every reply.

- Inbound cross-realm messages **surface to the human** via `list` / `read` (and
  via `listen`; see below). They do not auto-trigger an outbound reply.
- The directed agent replies only when the human (or its loop) explicitly sends.
- This is the safe default across a trust boundary (see
  [Loop and safety stack](#loop-and-safety-stack)).

### Autonomous participants

A hosted cloud agent may set `auto_reply=true`, but only within a **finite
per-conversation reply budget** bounded by the loop caps below. Autonomy is
always budgeted; it is never an unbounded auto-responder.

- An autonomous participant exhausting its per-conversation reply budget stops
  replying and surfaces the conversation for human attention, the same as a
  budget-exhausted loop.
- `auto_reply=true` **across a trust boundary** is still gated by the
  do-not-auto-reply-by-default rule: it requires standing federation consent
  with the peer realm, not just the local flag.

## 1:1 and cross-realm channels

- **1:1 is the default.** A cross-realm conversation is between two named agents
  (each in its own realm).
- **1:many is a cross-realm channel.** A channel **generalizes the realm-local
  group fan-out** ([security-groups.md](security-groups.md),
  [inter-agent-messaging.md](inter-agent-messaging.md)) across realms, with:
  - **Mutual consent** — every participating realm must allow-list the others
    (deny-by-default federation applies per-edge, not once for the channel).
  - **Snapshot-at-send** fan-out — a channel message is delivered to the
    participants resolved **at send time**, exactly like group fan-out; later
    joiners do not retroactively receive earlier messages.
  - A **fan-out cap** — large channels are throttled, not silently truncated,
    reusing the realm-local fan-out cap.

A cross-realm channel is fan-out + consent over the same signed-envelope
substrate; it is not a new mailbox model.

## Conversation and task lifecycle

The realm-local `thread_id` is **promoted** to a first-class, cross-realm
`conversation_id`, carrying an **A2A-style task state machine** so both sides
agree on where a unit of work stands.

```
submitted ──▶ working ──▶ completed
                 │
                 ├──▶ input_required ──▶ working
                 ├──▶ auth_required  ──▶ working
                 ├──▶ failed
                 └──▶ canceled
```

- `submitted` — the conversation/task exists and is accepted.
- `working` — the responding agent is actively progressing it.
- `input_required` — progress is blocked pending more input (gated to the human
  for directed participants).
- `auth_required` — progress is blocked pending an authorization/consent step (a
  human gate; see [Loop and safety stack](#loop-and-safety-stack)).
- `completed` | `failed` | `canceled` — terminal.

Rules:

- **The durable mailbox is the source of truth.** Conversation/task state is
  reconstructed from the durable mailbox in the home cell; an agent can resume a
  conversation after a restart by reading its mailbox.
- **Any live stream is only a latency accelerator**, never authoritative. A
  dropped stream loses no state; the next `listen`/`list` drains the mailbox.
- State transitions emit audit events (`conversation.started`,
  `conversation.state_changed`, `conversation.completed`,
  `conversation.failed`, `conversation.canceled`); the canonical names land in
  [audit-retention.md](audit-retention.md) on the contract pass.

## Loop and safety stack

Multi-agent autonomy across realms is a loop hazard: two agents can ping-pong,
fan-out can amplify, and a budget-less auto-reply can burn cost without a human
in the loop. The safety stack is **enforced on the wire** (below every
frontend), with the pinned v0 defaults below.

| Control | Default | Behavior |
| --- | --- | --- |
| Auto-reply across a trust boundary | **off** | No autonomous reply across a boundary without standing consent. |
| `hop_count` / `max_hops` | `8` | Each relay hop increments `hop_count`; over `max_hops` the message is dropped + audited. |
| Per-conversation `turn_budget` | `24` | Turns are counted per `conversation_id`; exhaustion suspends the conversation. |
| TTL / `expires_at` | `1h` (max `24h`) | Expired envelopes are not delivered. |
| Idempotency + dedup | on | Dedup on `id` + `nonce`; redelivery is a no-op (extends at-least-once dedup by `msg_` id). |
| Repeat-hash loop detection | 3× | The same message hash seen 3× **suspends** the conversation + notifies. |
| Shared cost kill-switch | soft **$5** warn / hard **$25** fail | Evaluated **before each model call**, per conversation. |
| Adaptive rate limits + new-sender quarantine | on | First contact from a new sender is quarantined; rates adapt to behavior. |
| Circuit breaker + wait timeouts | on | A failing/slow peer trips the breaker; waits time out deterministically. |

In addition:

- Agents are told their **remaining budget**. `remaining_turns` and remaining
  spend/budget are exposed so an agent can self-pace before it is cut off.
- **Human gates fire at exactly three points**, and nowhere else by default:
  1. trust-boundary **auto-reply** (crossing a realm boundary autonomously),
  2. **over-threshold spend** (the cost kill-switch), and
  3. **`auth_required`** in the task lifecycle.

These caps compose with — and never replace — the realm-local send/delivery rate
limits in [inter-agent-messaging.md](inter-agent-messaging.md) and
[billing-and-limits.md](billing-and-limits.md). `loop.suspended` and
`budget.exhausted` audit events land on the contract pass.

## Trust and consent

Cross-realm trust is **two-layer** and **deny-by-default**.

### Two-layer identity

- **Layer 1 — token-derived sender** (routing + in-realm anti-spoofing).
  Unchanged from [inter-agent-messaging.md](inter-agent-messaging.md): the
  sender is derived from the authenticated token. This is sufficient *within* a
  realm, where one issuer is authoritative.
- **Layer 2 — cross-realm signature** (trust across the boundary). A cross-realm
  envelope is signed with the **realm key** (ideally an **agent keypair**) and
  verified against the sender realm's **published JWKS** (from its signed card).
  The token does not span realms; the signature is what lets the receiver trust
  *which realm/agent* sent the message.

### Deny-by-default federation

- A realm **allow-lists** which remote realm handles + keys it accepts. Absence
  from the allow-list is a **deny** — federation does not happen by default.
- **First contact is quarantined** and requires **consent** before the
  conversation proceeds. New-sender quarantine (above) is the wire mechanism; a
  consent step (`federation.consent_accepted`) is the durable decision.
- Trust is **anchored, not transitive.** Trusting realm A does not imply trusting
  whatever A trusts. Each federation edge is its own allow-list decision.
- Federation decisions are audited (`federation.peer_allowed`,
  `federation.peer_denied`, `federation.consent_accepted`).

### Untrusted content, no authority across realms

- Content stays **untrusted** and carries **no authority** across realms. A
  cross-realm message can **never** author a write in the receiving realm without
  a **standing `allow` policy** there (see [access-policy.md](access-policy.md)).
  A signed, allow-listed sender proves *who sent it*, not *what it may do*.
- This preserves the realm-local rule (a message is data, not a command, not a
  grant) and extends it: even a verified cross-realm peer is a **peer principal,
  not a trusted one** (see [threat-model.md](threat-model.md)).

### Revocation

- The design **needs real-time revocation**: a compromised or de-trusted realm/
  agent key must stop being honored promptly, not at the next card TTL. Card TTL
  bounds staleness; revocation is the fast path. (The exact revocation mechanism
  is an [open decision](#open-decisions) tied to the identity-root choice.)

## Transport and interface invariants

These are **decided** invariants for the collaboration substrate.

- **MCP-everywhere, full parity.** Everything works via MCP with the same
  capability surface as the CLI — not a CLI-only feature with an MCP subset.
- **CLI is primary/canonical.** The CLI is the canonical surface; MCP and API
  mirror it (one core, multiple adapters).
- **`listen` / `recv` verb.** A long-poll-style verb is added to **both** CLI and
  MCP, next to `send` / `list` / `read`: it **blocks up to N seconds** and
  returns inbound messages (draining the mailbox), then returns. This is how an
  agent "hears" without running a server.
- **No agent-run HTTP servers for normal I/O.** Agents are **outbound clients**.
  The only HTTP server in the system is the backend (`witself-server` / the
  relay). An agent never needs to bind a port or accept inbound connections to
  participate.
- **Optional wake-webhook**, only for already-hosted cloud **autonomous** agents,
  is a latency optimization and is **never required**. A directed agent on a
  laptop participates fully with polling alone.
- **Durable mailbox is the source of truth.** Live streams are accelerators;
  state lives in the mailbox in the home cell.
- **Offline recipients are the default.** A `send` **never requires the recipient
  to be online**: it persists into the recipient's durable mailbox (store-and-
  forward) and **drains on the recipient's next `listen`**. This is the
  realm-local mailbox model extended across the relay.
- **Polling-first transport for v0.** v0 collaboration is polling-first (`listen`
  long-poll); push/streaming is an optimization layered on top, not a
  requirement.
- **Agent-directive `listen` instruction.** Agents are told to listen in their
  **agent directive** — the context-hydration teaching stanza gains an
  instruction equivalent to: *"to hear, call the `witself listen` tool each
  loop."* See [context-hydration.md](context-hydration.md).

## Surfaces

One model, three thin adapters, authorization enforced below every frontend
(identical result across CLI, MCP, and API). Detailed request/response contracts
land in the follow-up contract pass; the surface deltas are:

### CLI — the `message` group gains `listen`

- `witself message listen [--timeout <sec>] [--conversation <id>] [--json]` —
  long-poll: block up to `--timeout` seconds, return inbound messages, drain the
  mailbox. Sits next to the existing `message send` / `list` / `read` / `ack`.
- `witself message send` extends `--to` to accept a realm-qualified
  `witself://<realm-handle>/agent/<name>` (and `/group/<name>`); absent realm is
  local, unchanged.
- Cross-realm sends and channel fan-out honor `--dry-run` (validate recipient,
  federation allow-list, budgets, quotas; persist/deliver nothing).

### MCP — `witself.message.listen`

- `witself.message.listen` — the long-poll verb, full parity with the CLI; same
  authorization; honors `--read-only` (in read-only, `listen` and `read`/`list`
  remain available, `send` does not).
- `witself.message.send` / `.list` / `.read` extend to realm-qualified
  addressing. The agent-token MCP session can still only send **as** the
  token-bound agent.

### API — cross-realm envelope + conversation/task resource

- The message envelope gains the optional `realm` on `to`/`from`, the
  `conversation_id`, and the loop/safety fields (`hop_count`, `turn_budget`,
  `nonce`, `expires_at`, `signature`).
- A **conversation/task resource** exposes the A2A-style state machine
  (`submitted` → `working` → `input_required`/`auth_required` →
  `completed`/`failed`/`canceled`) and remaining-budget fields.
- A **listen** route (long-poll) returns inbound messages for the caller.
- All routes use the shared envelope `{schema_version, ok, data, warnings}` and
  the standard error/HTTP/exit-code mapping; contracts land in
  [json-contracts.md](json-contracts.md), [api-contract.md](api-contract.md),
  [api-routes.md](api-routes.md), [cli-command-surface.md](cli-command-surface.md),
  and [mcp-tools.md](mcp-tools.md) on the follow-up pass.

Naming note: the CLI command is decided to become `ws` (the mechanical rename is
a separate follow-up); examples here keep the current `witself <cmd>` form for
repo consistency. See [requirements.md](requirements.md).

## Open decisions

Documented, not resolved in this pass.

- **Identity root.** Per-realm signing key for v1 (simpler, one key per realm)
  vs per-agent keypair now (finer-grained attribution and revocation, more key
  management). This choice also shapes the revocation mechanism.
- **Self-host federation topology.** Cloud-relay-first (all federation flows
  through the blind relay) vs peer-to-peer (self-hosts reach each other
  directly by FQDN). The relay model is described above as the baseline; P2P is
  the open fork.
- **Auto-reply default.** Off-by-default + budgeted opt-in (recommended) vs a
  more permissive default for hosted autonomous agents. The stack above assumes
  off-by-default; this records the fork rather than closing it.
- **A2A interop.** Native A2A at the boundary (speak A2A on the wire) vs
  Witself-native envelopes with an A2A gateway (translate at the edge). The
  lifecycle above is A2A-style either way.

## Sequencing

- Collaboration is the **first post-v0 epic**, built **after** the realm-local
  core (memory + realm-local messaging). It **extends** the realm-local mailbox;
  it does not replace it.
- It depends on the shared global directory + cell model
  ([deployment-cells.md](deployment-cells.md)) for realm-handle resolution and
  the blind relay's routing.
- Tracked as the headline post-v0 epic in
  [post-v0-roadmap.md](post-v0-roadmap.md).

## Cross-references

- [inter-agent-messaging.md](inter-agent-messaging.md) — the realm-local mailbox
  authority this doc extends (durable mailbox, delivery/ordering, token-derived
  sender).
- [security-groups.md](security-groups.md) — group fan-out that cross-realm
  channels generalize.
- [access-policy.md](access-policy.md) — why a cross-realm message carries no
  authority; writes still require a standing `allow` policy.
- [threat-model.md](threat-model.md) — peer-principal stance, untrusted content,
  spoofing/injection/poisoning across the boundary.
- [deployment-cells.md](deployment-cells.md) — the shared global directory, cell
  placement, and realm-handle resolution the relay routes against.
- [context-hydration.md](context-hydration.md) — the agent directive carrying the
  `listen` instruction.
- [requirements.md](requirements.md) — master spec; addressing, scopes, naming
  (incl. the `ws` CLI-command decision).
- [post-v0-roadmap.md](post-v0-roadmap.md) — collaboration as the first post-v0
  epic and its sequencing.
- [json-contracts.md](json-contracts.md), [api-contract.md](api-contract.md),
  [api-routes.md](api-routes.md), [cli-command-surface.md](cli-command-surface.md),
  [mcp-tools.md](mcp-tools.md) — the surfaces (contract details on follow-up).
- [audit-retention.md](audit-retention.md) — the new conversation/federation/
  loop audit events (land on the contract pass).
- [billing-and-limits.md](billing-and-limits.md) — rate limits and the shared
  cost kill-switch this stack composes with.
