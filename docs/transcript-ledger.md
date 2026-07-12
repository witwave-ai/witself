# Witself Transcript Ledger

Status: first implementation slice. Decision: Witself stores an append-only
ledger of the visible conversation an agent runtime chooses to record. The
ledger is distinct from addressed inter-agent messaging.

## Goal

Give agent runtimes and enterprise operators a portable record of what crossed
the model boundary without pretending that Witself can or should capture a
model's private reasoning.

A normal exchange is two immutable entries in one transcript:

1. A `user` entry records the prompt delivered to the model.
2. An `assistant` entry records the finalized response and points to the prompt
   through `reply_to_entry_id`.

Additional turns share the transcript id and receive a monotonically increasing
sequence number. Explicit `system` and `tool` entries may be recorded when an
integration needs an execution trace. Streaming token chunks are not entries;
the integration records the finalized visible result.

Codex and Claude Code integrations normalize their different hook envelopes
into this same model. Runtime-specific fields are retained as metadata, but do
not leak into the durable identity or ordering contract.

## Boundary From Messaging And Memory

- A **transcript** is an append-only record of visible interaction. The
  authenticated agent token identifies the recorder, while `role` describes
  the recorded participant and is treated as asserted data.
- A **message** is an addressed delivery between authenticated agents. Its
  sender is always the token-bound agent and can never be supplied as data. See
  [inter-agent-messaging.md](inter-agent-messaging.md).
- A **memory** is curated durable identity state. Transcript entries do not
  automatically become memories; an explicit extraction or curation step is
  required. This keeps conversational noise and prompt injection out of the
  agent's durable self.
- An **audit event** proves that an operation occurred without copying prompt or
  response bodies into the audit ledger.

Keeping these resources separate preserves the anti-spoofing boundary for A2A
messages and lets each resource have independent retention and access policy.

## First-Slice Model

`transcript_conversations` owns the ordered thread:

- `trn_` id, account, realm, and owning/recording agent.
- Optional external conversation id, title, and small JSON metadata object.
- The recorder's authenticated agent id and name, runtime (`codex`,
  `claude-code`, `grok-build`, or `cursor`), stable installation/location id and label, native session id,
  and workspace label are captured as metadata. The agent name is display
  metadata; the token-bound agent id remains authoritative.
- `next_sequence`, created time, and last-updated time.

`transcript_entries` is append-only:

- `ent_` id, transcript id, sequence, and token-derived recorder agent.
- Optional external message id, unique within the transcript, for idempotent
  capture when an integration retries a write.
- `role`: `user`, `assistant`, `system`, or `tool`.
- Visible text body, optional small JSON payload, optional model label, and an
  optional same-transcript `reply_to_entry_id`.
- A reserved `artifacts` array. The first slice accepts only an empty array; the
  field exists so file references can be added without changing the entry
  envelope.

The first-slice limits are 256 characters for a title, 512 characters for an
external id, 64 KiB for body text, and 16 KiB serialized for metadata or
payload. At least one of body or payload must be present on an entry.

Agent tokens may create, append, list, and read their own transcripts. Account
operator tokens may list and read every transcript in their account for audit
and compliance, but they cannot manufacture entries on an agent's behalf.

Creating a transcript with the same external conversation id is retry-safe and
returns the existing transcript. Entry batches are also retry-safe: repeating
an external entry id with identical content returns the existing entry, while
reusing it for different content is a conflict. An entry may point to an
earlier runtime event through `reply_to_external_id`; the server resolves that
reference to the immutable same-transcript entry id.

## Runtime Installation And Identity

The supported local installation commands are:

```sh
witself install codex
witself install claude
witself install grok
witself install cursor
witself install claude,codex,grok,cursor --agent scott --location home
```

Each command verifies the agent token against `/v1/self`, installs a stdio MCP
server named `witself`, and merges Witself command hooks into the runtime's
existing hook configuration. It does not put a token in a hook or MCP command.
The local binding stores the account, realm, agent selector, endpoint, optional
token-file path, capture mode, and server-confirmed agent identity under
`~/.witself/integrations/<runtime>/config.json`.

The command reuses the existing runtime binding or the only local agent
credential. If multiple local agents exist, pass `--agent NAME`. The selected
account, realm, and agent are written explicitly into every hook and MCP command
and checked again when either entry point runs. `--location home` is optional;
when supplied it is also written into both commands, and when omitted no
location argument is written. `~/.witself/location.json` always contains a
stable generated `loc_` id and includes a human label only when one is supplied.
The runtime name, native session id, generated run id, turn id, and event id
distinguish overlapping sessions across all four runtimes at that location.
`WITSELF_HOME` replaces `~/.witself` when set.

Installing again replaces only Witself's MCP registration and hook handlers for
that runtime; unrelated runtime configuration is preserved. One local binding
per runtime is supported in this slice. Codex and Claude Code default to
administrator-managed hooks; the CLI keeps identity and MCP configuration
user-scoped, then uses a narrow elevation only for system policy. Do not run
the whole command with `sudo`. Grok Build and Cursor use their native
approval-free global user hook locations and require neither elevation nor
project trust.

On macOS, Codex policy is merged into `/etc/codex/requirements.toml`; an
existing managed hook directory is reused when one is already defined. Claude
Code receives an isolated drop-in at
`/Library/Application Support/ClaudeCode/managed-settings.d/50-witself.json`.
Linux uses `/etc/codex/requirements.toml` and
`/etc/claude-code/managed-settings.d/50-witself.json`. The hook policy invokes
an administrator-owned runner, and the runner records the absolute Witself
executable selected at installation. Reinstalling updates that path and the
capture-mode event set without duplicating handlers.

Grok Build receives `~/.grok/hooks/witself.json` and a native MCP entry in
`~/.grok/config.toml`. Cursor merges handlers into `~/.cursor/hooks.json`,
preserves unrelated entries in `~/.cursor/mcp.json`, and asks the Cursor CLI to
enable the `witself` MCP registration.

The installer does not set Codex `allow_managed_hooks_only` or Claude Code
`allowManagedHooksOnly`, so unrelated user, project, and plugin hooks remain
available. If Claude Code is already governed by a higher-precedence server or
MDM managed-settings source, deploy the same hook object through that active
source instead; Claude Code does not merge separate managed tiers.

`witself uninstall codex|claude|grok|cursor` removes the MCP registration, integration
binding, and the recorded user or managed hooks. Agent tokens and pending local
transcript events are deliberately preserved. `--managed-hooks` can be passed
to uninstall as a recovery override when the local integration record is
missing.

## Capture And Delivery

Hooks write an owner-only event file to
`~/.witself/capture/outbox/<runtime>/` before starting a detached flush. The hook
returns success even when the network is unavailable, so transcript storage
does not block or break the agent runtime. A failed flush leaves the event in
the outbox; the next hook retries it, or it can be retried explicitly:

```sh
witself transcript flush --runtime codex
witself transcript flush --runtime claude-code
witself transcript flush --runtime grok-build
witself transcript flush --runtime cursor
```

Capture modes are cumulative:

- `messages` records session, prompt, response, subagent, and compaction
  lifecycle events that the runtime exposes.
- `trace` also records runtime-exposed tool calls/results, failures,
  permissions, notifications, and Cursor thought summaries.
- `raw` also retains the hook envelope, local transcript path, and other
  runtime-exposed fields. It still cannot expose hidden chain-of-thought.

Visible bodies larger than one entry are split into ordered 60 KiB chunks and
are never silently truncated. A bounded raw hook envelope is stored in the
entry payload; oversized raw envelopes keep a digest and byte count in Postgres
while the complete pending event remains in the local outbox. There are no
per-token or streaming writes. Flushes use batches of at most 100 entries, and
reads use bounded forward pages or a bounded tail.

Every entry also has a provider-neutral JSON payload with `kind`, canonical and
native event names, runtime/session/run/turn ids, location, model, cwd, and
typed `data`. Tool entries keep structured name, use id, input/output or error;
turn entries keep status, reason, duration, and token usage when supplied.
Large structured values retain a digest and byte count instead of overrunning
the 16 KiB payload limit.

Every entry uses the same nullable `provenance` object across Codex, Claude
Code, Grok Build, and Cursor. `runtime` is always the installed integration;
`runtime_version` is captured from the native CLI at installation and may be
overridden by a version explicitly supplied by the hook. `model_provider` and
`model` are recorded only when a runtime supplies them. When a response hook
omits the model but the runtime's bounded native session transcript contains
it, Witself records that exact value with source `native_transcript`. A parallel
`sources` object identifies `integration`, `cli`, `hook`, or
`native_transcript` provenance for each value; unavailable values and sources
are JSON `null` rather than inferred. Runtime version remains associated with
each entry's `run_id`, so resumed transcripts can accurately span runtime
upgrades without adding a separate run table.

The common lifecycle is session start, user prompt, turn stop, tool call/result,
subagent start/stop, and pre-compaction. Witself also records session end,
turn/tool failures, post-compaction, permission events, notifications, and
Cursor thought summaries where the runtime exposes them. Missing provider
events are never fabricated, and a thought summary is not treated as hidden
chain-of-thought.

## Reasoning And Execution Traces

Witself does not request, expose, or store raw hidden chain-of-thought. When an
enterprise needs to understand a complex action, an integration may record:

- The visible prompt and final response.
- Explicit tool calls and sanitized tool results as `tool` entries.
- A concise decision or rationale summary in a visible assistant/system entry.
- Model, timing, citation, and cost metadata in the small JSON payload.

This provides an inspectable execution record without treating private model
reasoning or every internal token as durable customer data.

## Structured Objects And File Artifacts

Postgres `jsonb` is the right first-slice home for small structured objects:
tool arguments, result summaries, citations, decisions, and other bounded JSON.
It remains queryable and travels with the account archive.

Postgres can technically store arbitrary bytes in `bytea`, but Witself will not
use ordinary rows as a general file store. Generated documents, images,
archives, and other binary artifacts require a cell-local object-store adapter.
When that slice lands:

- Postgres stores `art_` metadata, ownership, content type, size, checksum,
  source entry, version, retention, and object key.
- Object storage holds the bytes.
- Account export/import carries both the metadata and blob manifest/content so
  moving an account never leaves its artifacts behind.
- Revisions create new immutable versions rather than overwriting prior output.

Until then, an integration may store a bounded structured result in `payload`,
but non-empty file attachments are refused instead of creating a non-portable
reference.

## First-Slice Surfaces

API:

```text
GET  /v1/transcripts
POST /v1/transcripts
GET  /v1/transcripts/{transcript_id}?after_sequence=0&limit=100
GET  /v1/transcripts/{transcript_id}?tail=true&limit=20
POST /v1/transcripts/{transcript_id}/entries
POST /v1/transcripts/{transcript_id}/entries:batch
```

CLI:

```text
witself transcript create
witself transcript append TRANSCRIPT_ID
witself transcript list
witself transcript show TRANSCRIPT_ID
witself transcript tail TRANSCRIPT_ID --limit 20
witself transcript flush --runtime codex|claude-code|grok-build|cursor
```

The installed stdio MCP server exposes the read-only
`witself.self.show`, `witself.transcript.list`, `witself.transcript.get`, and
`witself.transcript.tail` tools. Hook capture is the write path; model-initiated
MCP transcript mutation is intentionally not part of this slice.

Transcript rows are included in the account's logical export/import stream in
foreign-key order. Bodies and payloads must never appear in logs, metrics,
errors, or account-event metadata.

The first slice has no edit or delete endpoint: entries remain immutable and
are retained with the account. Configurable retention, legal holds, redaction,
and an explicitly audited deletion workflow are later enterprise-policy work,
not implicit side effects of reading or moving an account.
