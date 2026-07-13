# Agent Memory Routing

Status: implemented for the Codex integration's fact-versus-native-memory
routing policy. Provider aggregation remains an agent behavior contract, not a
new Witself API.

## Authority and delivery

The canonical Codex policy is the `codexMemoryRoutingInstructions` constant in
`cmd/witself/codex_instructions.go`.

`witself install codex` installs that policy as an idempotent managed block in
the active global Codex instruction file. The block contains routing policy
only. Personal facts and memory content never belong in it. Reinstall replaces
an older managed block without changing unrelated instructions; uninstall
removes only the managed block. Installation refuses to write a shadowed file
when the same Codex home contains a non-empty `AGENTS.override.md`; the user must
remove the override or merge its instructions before retrying.

`witself mcp serve --runtime codex` also returns the exact same policy at the
front of the MCP initialization `instructions`. Fact tool descriptions repeat
the critical when-to-call triggers so clients that defer or truncate server
instructions still see the distinction.

The CLI owns installation and lifecycle, MCP owns runtime tool guidance, and
the Witself API remains the fact system of record. The same policy is not copied
into an independent server-side classifier.

## Capture contract

Natural-language `remember`, `save`, and `store` requests are routed by content,
not by a product-specific incantation.

| Content | Default destination |
|---|---|
| Explicit request to store one atomic, durable assertion or preference | Witself fact |
| Narrative context, rationale, project history, lesson, or multi-step incident | Runtime-native memory |
| Clearly mixed request | Split the fact and narrative portions |
| Genuinely ambiguous boundary | Ask before storing |
| Explicit provider or request for both | Honor the explicit destination |

Examples of fact-shaped data include a name or relationship, birthday, address,
location, URL, identifier, stable status, and compact durable preference. A
fact about another person, place, project, or entity first resolves one stable
Witself subject. Private personal values are sensitive fact values, never
subject keys, display names, or aliases.

The same information is not silently duplicated across providers. An explicit
request to remember a fact authorizes the synchronous `witself.fact.set` call in
that turn. It does not authorize a second Codex Markdown memory.

Codex native memory is different: it is host-owned generated state, updated in
the background, and has no supported transactional write API. The agent can
leave narrative material eligible for native memory, but must not claim an
immediate durable write or fabricate a native memory record id. A Codex setting
may also exclude tasks that used external context such as MCP from background
memory generation. Witself does not alter that personal setting.

## Retrieval contract

The provider router follows the user's retrieval intent.

| Intent | Default behavior |
|---|---|
| Exact fact question | Resolve the subject and query `witself.fact.get` |
| Broad question such as "what do you remember about this?" | Query relevant Witself facts and consult available Codex memory context |
| Narrative history question | Consult available native memory context; use Witself memory recall only after that tool actually exists |
| Explicit Witself | Use only available Witself sources |
| Explicit Codex memory | Use only available Codex memory context |
| Explicit both/all sources | Consult every available requested provider and report partial coverage |

Witself transcripts are interaction records, not memories. They are searched
only when the user explicitly requests transcript or conversation history.

Codex currently has no supported transactional search API for its native local
memory store. "Consult Codex memory" therefore means use memory context made
available by the runtime. It is best-effort, not proof that the full native
store was searched. If a requested provider is unavailable or cannot be
queried, the answer names that provider and marks the result partial instead of
claiming comprehensive recall.

Results retain their provider and authority:

- A resolved Witself fact is a canonical assertion in Witself.
- Native or future Witself narrative memories are advisory context.
- Provider-local scores are never combined into a cross-provider ranking.
- Duplicate statements are collapsed for presentation without dropping their
  separate provenance.
- A conflicting memory is surfaced as stale or conflicting context; it never
  silently replaces a resolved fact.
- Non-overlapping time-qualified values, such as current and previous
  addresses, are history rather than a conflict.
- Open fact candidates are warnings, not canonical answers.

Broad retrieval uses redacted fact inventory results and never enables
`include_sensitive` automatically. An exact, intentional, authorized fact
lookup may return a sensitive value. Selecting a provider never implies
permission to reveal private data.

## Future provider aggregation

A future federating interface should use `provider`, not `source`, because
Witself records already use `source_kind` and `source_ref` for provenance. A
request may eventually accept `provider=auto|witself|codex|all`, with an envelope
that reports requested providers, provider statuses, results, conflicts, and a
top-level `partial` flag.

That wire contract should be added only when a supported Codex retrieval API or
a deliberate local federation boundary exists. Witself must not treat generated
files under `~/.codex/memories` as a stable integration API.

## Runtime expectations

Codex reads global `AGENTS.md` guidance when a session starts, and reads MCP
server instructions during MCP initialization. After installing or upgrading
the policy, start a new Codex task so both instruction surfaces are refreshed.

See the official Codex documentation for
[AGENTS.md guidance](https://learn.chatgpt.com/docs/agent-configuration/agents-md.md),
[native memories](https://learn.chatgpt.com/docs/customization/memories.md), and
[MCP server instructions](https://learn.chatgpt.com/docs/extend/mcp.md).
