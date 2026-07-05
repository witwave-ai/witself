# Competitive Analysis

Status: initial live-research pass from June 26, 2026. This document captures
product patterns relevant to Witself, not a complete market map. Claims drawn
from vendor docs and current commentary are best-effort and should be
re-verified before any external use.

## Summary

Witself now competes on two fronts at once, because it is one product spanning
two planes: an **open plane** for memories and facts, and a **sealed plane** for
secrets and TOTP. That means the relevant field is the union of two crowded
categories:

- **Agent-memory layers** (mem0, Letta, Zep, LangMem), vendor-native memory
  (OpenAI, Anthropic), and DIY vector-DB memory — the competitors for the open
  plane.
- **Secrets and password managers** (1Password, Bitwarden, Vault, Infisical,
  Doppler) — the competitors for the sealed plane.

Several products already cover pieces of the Witself idea on one side or the
other:

- Memory layers (mem0, Letta, Zep, LangMem) already extract facts, store
  memories, embed them, and recall semantically.
- Vendor-native memory (OpenAI, Anthropic) ships persistent cross-session
  memory directly inside the assistant or platform.
- Vector databases plus an embedding model are the default do-it-yourself
  memory pattern, and pgvector is a common production substrate.
- Multi-agent frameworks and the A2A protocol are standardizing how agents
  message and delegate to each other.
- Password and secrets managers already expose CLI and MCP surfaces, support
  machine identities, scoped tokens, runtime injection, TOTP generation from
  stored vault material, and dynamic secret leases.

Witself should not assume that "memory layer plus MCP" or "secrets manager plus
MCP" is unique by itself, and it should not pretend semantic recall or runtime
injection is novel — both are table stakes in their respective categories. The
sharper product position is **one governed agent durable-state platform** that
spans both planes under a single realm/agent/token model: an agent-native
*identity* store (facts and a `primary` anchor alongside memories, a declarative
cross-agent **policy** engine with default deny, **security groups** as policy
subjects/targets, durable **inter-agent messaging** with token-derived senders,
first-class plaintext **export/import**) joined to an agent-native *credential*
store (per-realm sealed secrets and TOTP under KMS-backed envelope encryption,
explicit audited reveal, runtime injection, and stable `witself://` references).
Both planes share one inspectable public backend that runs as managed Witself
Cloud or self-hosted, and one CLI-native administration surface where operators
manage realms, agents, policies, groups, secrets, billing, and support through
the CLI with or without AI assistance, instead of a browser dashboard as the
primary control plane.

The categorical distinction worth holding spans both planes. On the open plane,
most memory products optimize recall *quality* (better extraction, ranking,
temporal graphs); Witself optimizes the *integrity and authenticity* of identity
data across many agents — who may read, contribute to, curate, or forget whose
memory, attributed and audited. On the sealed plane, most secrets managers
optimize confidentiality for human-or-machine consumers; Witself binds secrets
to the same authenticated agent principals, with a sealed-plane carve-out that
keeps the two planes from contaminating each other: secrets and TOTP seeds are
**never embedded, recalled, placed in the self-digest, ingested, or
plaintext-exported, and are returned only through an explicit, audited,
reveal-gated path** (see [secret-model.md](secret-model.md),
[encryption-model.md](encryption-model.md), and
[totp-2fa.md](totp-2fa.md)). No reviewed competitor governs both planes under
one principal model. That joined governance surface is where the field is thin.
See [requirements.md](requirements.md) and
[access-policy.md](access-policy.md).

## Products Reviewed

### mem0

mem0 is the most direct framing competitor: a self-described universal memory
layer for AI agents, available both as an embeddable library and as a
self-hosted server. It extracts structured memories — facts, preferences, and
relationships — from conversations automatically, embeds them, and retrieves by
vector similarity. The open-source server packages a familiar stack: a REST
API, PostgreSQL with pgvector for embeddings, and a graph store for entity
relationships, with three hosting models (managed cloud, self-hosted, local
MCP).

Relevant patterns:

- Automatic memory extraction: feed conversations in, get structured facts out.
- pgvector-powered semantic search as the retrieval primitive.
- Self-host via containers; per-user API keys and a request audit log on the
  server.
- Per-project configuration of inclusion/exclusion prompts and memory depth.

Witself takeaway:

- The library-vs-server split and the pgvector substrate validate Witself's
  storage choices (see [storage.md](storage.md)), but mem0's unit of isolation
  is the *user/project*, not an authenticated *agent principal*. Witself's
  agent-as-principal model (token = identity, bound to one realm + one agent)
  is the structural difference, and it is what makes cross-agent policy and
  messaging possible at all.
- mem0 conflates "fact" with "extracted memory." Witself keeps **facts** as
  deterministic, name-unique, queryable identity cards with a `primary` anchor,
  separate from free-form **memories**. See [facts-model.md](facts-model.md)
  and [memory-model.md](memory-model.md).
- mem0 has no declarative cross-agent authorization engine. Witself's default-
  deny policy with `read`/`contribute`/`curate`/`forget` verbs is the gap.

Sources:

- [mem0 open-source overview](https://docs.mem0.ai/open-source/overview)
- [mem0 GitHub](https://github.com/mem0ai/mem0)
- [Self-hosting mem0 with Docker](https://mem0.ai/blog/self-host-mem0-docker)
- [State of AI agent memory 2026](https://mem0.ai/blog/state-of-ai-agent-memory-2026)

### Letta (MemGPT)

Letta (formerly MemGPT) is an open-source stateful-agent framework (Apache 2.0,
self-hostable) where the agent manages its own memory through tool calls. It
uses an OS-inspired hierarchy — main context, recall storage, archival storage
— and labeled memory blocks that the model edits in its normal loop. Core
memory (persona and key facts) lives in-context and is agent-editable; archival
memory is a vector store for long-term recall. Persistence is the default, not
an afterthought.

Relevant patterns:

- Agent-managed memory: the model itself reads and rewrites memory blocks.
- A clear hierarchy between always-in-context state and archival vector recall.
- Apache 2.0 and self-hostable; Witself is self-hostable too, but source-available
  under FSL-1.1-ALv2 rather than permissive.

Witself takeaway:

- Letta's "agent edits its own memory" is the inverse emphasis from Witself.
  Letta optimizes a *single* agent's self-management loop; Witself optimizes
  *multi-agent* governance — one agent curating or forgetting another's memory
  under explicit policy, fully attributed in audit. Witself's edit history and
  soft-delete/restore lifecycle (see [memory-model.md](memory-model.md)) give
  the safety rails Letta leaves to the agent loop.
- Letta has no cross-agent authorization, no security groups, and no durable
  inter-agent mailbox as a first-class product surface. Those are headline
  Witself features (see [security-groups.md](security-groups.md) and
  [inter-agent-messaging.md](inter-agent-messaging.md)).
- Letta's memory blocks map loosely onto Witself's `kind`-tagged memories and
  `primary` facts, but Witself keeps identity data inspectable and exportable
  in plaintext rather than embedded in an agent runtime's state.

Sources:

- [MemGPT is now part of Letta](https://www.letta.com/blog/memgpt-and-letta/)
- [Letta / MemGPT walkthrough (2026)](https://sureprompts.com/blog/letta-memgpt-walkthrough)
- [mem0 vs Letta comparison](https://vectorize.io/articles/mem0-vs-letta)

### Zep / Graphiti

Zep is memory infrastructure built around a *temporal knowledge graph*. Its
open-source core engine, Graphiti, synthesizes conversational and structured
data into a graph where every fact carries a validity window (`valid_at` /
`invalid_at`), entities carry relationships, and stale facts are superseded
rather than left to pollute retrieval. Zep reports large latency reductions and
accuracy gains over stuffing full history into context, and Graphiti has seen
broad open-source adoption.

Relevant patterns:

- Bitemporal facts: facts have a truth window and are explicitly superseded.
- A knowledge graph as the memory substrate, not just a flat vector index.
- Hybrid retrieval over the graph (semantic plus structured edges).

Witself takeaway:

- Zep's temporal fact model is the most sophisticated fact handling in the
  field and a sharper recall story than Witself's v0. Witself's answer is *not*
  to out-graph Zep in v0; it is versioned edit history per memory and per fact
  plus a `primary` anchor, which gives auditable "what was true and who changed
  it" without a full bitemporal graph. Temporal/graph recall is a candidate for
  the post-v0 roadmap, not a v0 claim.
- Zep, like mem0 and Letta, has no multi-agent authorization model: it answers
  "what does this user/agent know," not "which agent may touch which other
  agent's identity data." Witself's policy engine, groups, and attributed
  cross-agent mutations are the differentiator.
- Bitemporal supersession is worth tracking as an enhancement to Witself's
  edit-history and `primary`-promotion semantics in
  [facts-model.md](facts-model.md).

Sources:

- [Zep: a temporal knowledge graph architecture for agent memory (arXiv)](https://arxiv.org/abs/2501.13956)
- [Graphiti GitHub](https://github.com/getzep/graphiti)
- [Graphiti knowledge-graph memory (Neo4j)](https://neo4j.com/blog/developer/graphiti-knowledge-graph-memory/)

### LangGraph / LangMem

LangMem is LangChain's open-source SDK giving LangGraph agents long-term memory
— semantic, episodic, and procedural. Memories are stored in LangGraph's store
primitives as JSON documents organized by *namespace* and key, where namespaces
nest hierarchically (organization, user, application) and can include runtime
template variables such as `{user_id}`. It is the long-term-memory option
alongside LangGraph checkpointers, though the package remains pre-1.0 with a
slow release cadence.

Relevant patterns:

- Hierarchical namespaces for natural multi-tenant segmentation of memories.
- Memory categorized by type (semantic / episodic / procedural), close to
  Witself's `kind` convention.
- JSON-document storage with contextual keys.

Witself takeaway:

- LangMem's namespaces and Witself's realm → agent/group hierarchy solve the
  same segmentation problem, but LangMem's namespaces are an application
  convention, not an enforced authorization boundary. Witself enforces
  isolation below every frontend with token-bound identity and default-deny
  policy. Namespace strings do not stop one agent from reading another's
  memories; Witself's policy engine does.
- LangMem is an SDK inside one framework's runtime. Witself is framework- and
  runtime-agnostic, reachable from CLI, MCP, and HTTP API, with stable
  `witself://` references usable across any agent stack (see
  [json-contracts.md](json-contracts.md)).
- LangMem has no inter-agent messaging, no security groups, no audited cross-
  agent curation, and no plaintext export/import product surface.

Sources:

- [LangMem conceptual guide](https://langchain-ai.github.io/langmem/concepts/conceptual_guide/)
- [LangMem SDK launch](https://www.langchain.com/blog/langmem-sdk-launch)
- [LangChain long-term memory docs](https://docs.langchain.com/oss/python/langchain/long-term-memory)

### OpenAI and Anthropic native memory

The model vendors now ship memory directly. OpenAI's ChatGPT memory references
past conversations and synthesizes background memory across sessions, with user
controls to review, correct, or disable it; but the *API* has no built-in
cross-session memory — the Agents SDK has per-session history, not cross-session
semantic memory. Anthropic ships a memory *tool* that lets Claude create, read,
update, and delete files in a memory directory stored in the developer's own
infrastructure, persisting across sessions; the consumer Claude memory feature
is separate from API/Claude Code, and Claude Managed Agents now support
persistent memory in beta with export/edit controls.

Relevant patterns:

- Vendor-native, low-friction memory for the assistant product.
- Anthropic's memory tool stores memory as files in the developer's own
  infrastructure — an inspectable, exportable, developer-owned posture.
- Additive memory (extracted facts/preferences) rather than a full transcript.
- User/developer controls to review, correct, export, or disable memory.

Witself takeaway:

- Native memory is the strongest "do nothing, it's built in" competitor, and it
  is closing the easy-onboarding gap. Witself's answer is not convenience; it
  is *governance and portability*. Native memory is per-account/per-assistant
  and largely opaque; it does not model multiple authenticated agent principals
  sharing or guarding identity data under policy, and it ties an agent's self to
  one vendor.
- Anthropic's developer-infrastructure, file-based, exportable model is
  philosophically aligned with Witself's plaintext export and inspectable-
  backend stance — useful validation that "the developer should own and be able
  to read the memory" is a credible market position. Witself goes further with
  a typed identity model (memories + facts + primary), cross-agent policy, and
  audit. See [backup-and-recovery.md](backup-and-recovery.md).
- The API-side gap ("agents forget between runs") is exactly Witself's wedge: a
  portable, vendor-neutral identity store reachable over MCP and HTTP, with an
  embedding-provider abstraction (`voyage` default, `openai`, `local-dev`) so
  Witself is not bound to any single vendor's embeddings or memory product. See
  [memory-model.md](memory-model.md).

Sources:

- [Memory and new controls for ChatGPT (OpenAI)](https://openai.com/index/memory-and-new-controls-for-chatgpt/)
- [Anthropic memory tool docs](https://platform.claude.com/docs/en/agents-and-tools/tool-use/memory-tool)
- [Managing context on the Claude Developer Platform](https://anthropic.com/news/context-management)

### Vector databases as memory (pgvector, Pinecone, Qdrant, Weaviate, Chroma)

The default do-it-yourself memory pattern is "embed text, store vectors, recall
by similarity," built on a vector database. Current guidance favors pgvector as
the default until scale forces a dedicated engine; Chroma suits local
prototyping; Qdrant, Weaviate, Milvus, and Vespa take over at larger scale.
Hybrid search (semantic plus keyword) is increasingly treated as non-optional
for agent memory and RAG.

Relevant patterns:

- pgvector as the pragmatic production default for agent-memory workloads.
- Hybrid (vector + keyword) retrieval as the expected quality bar.
- A clear scale ladder before a dedicated vector engine is justified.

Witself takeaway:

- This validates Witself's storage and recall design: Postgres-as-system-of-
  record with pgvector, and hybrid ranking (vector similarity blended with
  keyword, tag, kind, recency, and salience). See
  [storage.md](storage.md) and [memory-model.md](memory-model.md).
- A bare vector DB is *infrastructure*, not a product: no agent principals, no
  authorization, no audit, no facts, no messaging, no export contract, no
  billing. Witself is the governed product layer over that substrate, not a
  competitor to the database.
- The "most workloads are smaller than they feel; default to pgvector" guidance
  supports shipping v0 on pgvector and deferring a dedicated vector engine to
  the post-v0 roadmap.

Sources:

- [Vector databases for AI agents: 2026 comparison](https://www.jahanzaib.ai/blog/vector-database-ai-agents-pinecone-weaviate-chroma-qdrant)
- [Pinecone vs pgvector vs Chroma vs Weaviate (2026)](https://www.groovyweb.co/blog/vector-database-comparison-2026)
- [Best vector databases 2026 (DataCamp)](https://www.datacamp.com/blog/the-top-5-vector-databases)

### Multi-agent frameworks and A2A

Multi-agent frameworks (LangGraph, CrewAI, AutoGen) and the Agent-to-Agent
(A2A) protocol standardize how agents message, coordinate, and delegate. A2A,
announced by Google in 2025, contributed to the Linux Foundation, and reaching
v1.0 in early 2026 with broad enterprise adoption, defines a JSON-based message
exchange for agents built on different frameworks to interoperate. Research
protocols (for example identity-aware multi-agent protocols) are pushing on the
identity dimension of agent communication.

Relevant patterns:

- Standardized, structured agent-to-agent message exchange.
- Cross-framework interoperability via a common protocol.
- Emerging attention to *identity* in agent communication, not just transport.

Witself takeaway:

- A2A and friends standardize the *transport* of agent messages; they are
  largely silent on durable mailboxes, persisted read/ack state, sender
  authenticity, and authorization. Witself's messaging is a durable,
  realm-scoped store where the sender (`from`) is always derived from the
  authenticated token — sender forgery is structurally impossible — with
  per-recipient delivery, ordering, and ack state, rate limits, and audit. See
  [inter-agent-messaging.md](inter-agent-messaging.md).
- These frameworks treat the *agent* as an ephemeral runtime object. Witself
  treats the named agent as a durable authenticated principal inside a realm,
  which is what makes identity-aware messaging and cross-agent policy
  enforceable rather than advisory.
- A2A is a complementary transport, not a competitor: Witself could expose or
  bridge to A2A in a later phase while remaining the system of record for who
  said what to whom, governed and audited. Track as a post-v0 interoperability
  item.

Sources:

- [A2A protocol explained (2026)](https://is4.ai/blog/our-blog-1/a2a-protocol-ai-agents-communication-guide-2026-416)
- [What is the Agent2Agent (A2A) protocol? (IBM)](https://www.ibm.com/think/topics/agent2agent-protocol)
- [LDP: an identity-aware protocol for multi-agent LLM systems (arXiv)](https://arxiv.org/pdf/2603.08852)

### Secrets and password managers (1Password, Bitwarden, Vault, Infisical, Doppler)

These are the competitors for Witself's **sealed plane**. The category already
has strong agent-facing patterns; Witself's job is not to re-invent them but to
fold them into the same realm/agent/token model as the open plane, behind the
sealed-plane carve-out.

- **1Password** ships CLI service accounts (`OP_SERVICE_ACCOUNT_TOKEN`), secret
  references such as `op://vault/item/field`, runtime injection via `op run` and
  `op inject`, and TOTP retrieval. Its MCP server is deliberately conservative:
  the docs state it does not read or return secrets to the AI tool, instead
  injecting values into the authorized application process.
- **Bitwarden** is the closest direct validation of the agent-credential
  premise: a local-first MCP server over the existing manager that lets
  assistants unlock a vault, retrieve passwords and TOTP codes, create/edit login
  items, and generate passwords — with strong README warnings that it is
  local-only and can expose credentials through AI responses. Secrets Manager
  machine accounts get scoped read or read-write access to projects.
- **Infisical** is a secrets platform with machine identities
  (`INFISICAL_TOKEN`), `infisical run` environment injection, dynamic secrets
  (list/lease/renew/revoke), and MCP gateways that route to private servers
  without public exposure.
- **Doppler**'s experimental MCP server reinforces the guardrails: narrowly
  scoped tokens, read-only mode, runtime token injection over plaintext MCP
  config, and treating hosted/network MCP as higher risk than local stdio.
- **Vault, Akeyless, CyberArk Conjur** are mature on non-human identity, RBAC,
  policy, audit, and dynamic/short-lived credentials (Vault AppRole, lease-based
  rotation), though less focused on agent browser-login flows.

Witself takeaway:

- These products validate the sealed-plane surface Witself ships: stable
  `witself://secret/<path>/<field>` references, runtime injection via
  `witself run`, TOTP from sealed material, scoped/revocable per-agent access,
  and a local-first MCP posture with `--no-value-tools` and `--read-only`
  modes. See [secret-model.md](secret-model.md) and
  [totp-2fa.md](totp-2fa.md).
- The decisive difference is that none of them treat the *agent* as the durable
  authenticated principal that also owns memories and facts. Witself's secrets
  are owned by an **agent** or a **group** inside one realm, governed by the
  same token = identity model, the same audit, and the same CLI/MCP/API spine as
  the open plane. A secrets manager bolts agents on as machine accounts; Witself
  starts from the agent.
- Witself adopts the conservative reveal posture (1Password's "don't hand
  secrets to the model," Bitwarden/Doppler's warnings) as a structural rule, not
  a recommendation: the sealed plane is **never embedded, recalled, in the
  self-digest, ingested, or plaintext-exported**, and values are returned only
  through an explicit, audited, reveal-gated ceremony (`witself secret reveal`,
  `witself totp code`) under KMS-backed envelope encryption (CMK → per-realm KEK
  → per-secret/field DEK). See [encryption-model.md](encryption-model.md),
  [key-hierarchy.md](key-hierarchy.md), and
  [authorization-and-roles.md](authorization-and-roles.md).
- Dynamic/short-lived leases (Vault, Akeyless, Infisical) are a credible
  enhancement to track for the sealed plane post-v0, not a v0 claim.

Sources:

- [Use service accounts with 1Password CLI](https://www.1password.dev/service-accounts/use-with-1password-cli)
- [1Password CLI run command](https://developer.1password.com/docs/cli/reference/commands/run/)
- [1Password MCP server](https://www.1password.dev/environments/mcp-server)
- [Bitwarden MCP server announcement](https://bitwarden.com/blog/bitwarden-mcp-server/)
- [Bitwarden MCP server README](https://github.com/bitwarden/mcp-server)
- [Bitwarden Secrets Manager quick start](https://bitwarden.com/help/secrets-manager-quick-start/)
- [infisical run](https://infisical.com/docs/cli/commands/run)
- [Infisical dynamic secrets CLI](https://infisical.com/docs/cli/commands/dynamic-secrets)
- [Doppler MCP server](https://docs.doppler.com/docs/mcp)
- [Vault AppRole authentication](https://developer.hashicorp.com/vault/docs/auth/approle)
- [CyberArk Secrets Manager CLI](https://docs.cyberark.com/secrets-manager-sh/13.8/en/content/developer/cli/cli-setup.htm)

## Where Witself Is Differentiated

The memory field is strong on recall and weak on governance; the secrets field
is strong on confidentiality and silent on durable agent identity. No reviewed
product on either side governs both planes under one principal model. Witself's
defensible position is the set of features that almost none of the reviewed
products treat as first-class:

### Facts plus a primary identity anchor

Every memory layer extracts "facts," but as unstructured, ranked recall hits.
Witself keeps **facts** as deterministic, name-unique identity cards with a
`primary` flag that marks the canonical value of a logical kind and is surfaced
first in `whoami`, profile, and export. `fact get email` returns the one true
value; it is not a similarity search. See [facts-model.md](facts-model.md).

### Declarative cross-agent policy (default deny)

No reviewed memory product ships an evaluable authorization engine for
*cross-agent* identity access. Witself binds subject × permission × target ×
scope with escalating verbs (`read` → `contribute` → `curate` → `forget`),
default deny, `policy test` for dry-run decisions, and full audit attribution
on every cross-agent mutation. This is the open-plane *integrity and
authenticity* engine; the sealed plane governs credential access through grants
and realm roles instead (see
[authorization-and-roles.md](authorization-and-roles.md)), and secrets are not
subject to the open cross-agent read/curate/forget verbs. See
[access-policy.md](access-policy.md).

### Security groups

A named set of agents that acts as both a policy subject and a policy target,
and can own group-scoped shared (collective) memories and facts. Membership is
operator-managed or delegated via `group:manage`. None of the reviewed memory
products model collective, governed group memory this way. See
[security-groups.md](security-groups.md).

### Durable, authentic inter-agent messaging

A realm-scoped mailbox with at-least-once delivery, per-recipient and
per-conversation ordering, read/ack state, and a token-derived sender that
cannot be spoofed. Message bodies are treated as untrusted input, and a message
alone never authorizes a cross-agent write. A2A standardizes transport; Witself
is the governed system of record. See
[inter-agent-messaging.md](inter-agent-messaging.md).

### First-class plaintext export and import

Structured, human-readable, round-trippable export of an agent's self —
memories (with edit history), facts (with `primary` and `sensitive` flags), and
identity anchors — plus operator-level realm context. It directly answers the
"vendor lock-in / opaque memory" weakness of native memory features. The carve-
out is deliberate and load-bearing: the plaintext export covers the **open
plane only**. The sealed plane is **never in the plaintext export** — secret
backup is encrypted-only (envelope blobs plus KMS key identity, never
plaintext), behind a separate, explicit, audited flag. The two-tier export is
itself a differentiator: portability for identity, confidentiality for
credentials, in one product. See
[backup-and-recovery.md](backup-and-recovery.md).

### A governed sealed plane for secrets and TOTP, on the same principal model

No memory product holds credentials; no secrets manager holds durable agent
identity. Witself ships both as one platform. Secrets and TOTP live in a
**sealed plane** under KMS-backed envelope encryption (CMK → per-realm KEK →
per-secret/field DEK, XChaCha20-Poly1305 / AES-256-GCM), owned by the same
**agent** or **group** principals as memories and facts, governed by grants and
realm roles, and returned only through an explicit, audited reveal ceremony
(`witself secret reveal`, `witself totp code`) — with hybrid client-side or
server-side decrypt behind a capability switch. The carve-out is the product
guarantee: sealed material is **never embedded, recalled, in the self-digest,
ingested from CLAUDE.md/AGENTS.md, or plaintext-exported**. A secrets manager
would have to grow an agent-memory model with cross-agent governance to match
this; a memory layer would have to grow KMS-backed encryption and a reveal
discipline. See [secret-model.md](secret-model.md),
[encryption-model.md](encryption-model.md),
[key-hierarchy.md](key-hierarchy.md), and [totp-2fa.md](totp-2fa.md).

### Inspectable public backend and self-hosting

The CLI, MCP adapter, `witself-server` API, storage and embedding adapters,
crypto and KMS provider adapters, authorization and policy logic, and audit
model live in one public repository. Operators choose managed Witself Cloud or
self-hosted control without changing the agent-facing CLI, MCP tools,
`witself://` references, or JSON contracts. Security reviewers can read the code
that stores, authorizes, audits, embeds, recalls, and serves identity material
on the open plane, and the code that envelope-encrypts, key-manages, reveals,
and audits credential material on the sealed plane. See
[self-hosting.md](self-hosting.md) and
[backend-architecture.md](backend-architecture.md).

## Recommended Positioning For Witself

### Lead with one governed platform across both planes

Semantic recall is table stakes (mem0, Zep, LangMem, native memory, and any
vector DB do it), and so are secret references, runtime injection, and TOTP
(1Password, Bitwarden, Infisical do them). Neither category alone is the pitch.
Lead with the joined story: one governed agent durable-state platform where the
same agent principal owns memories, facts, and secrets under one realm, one
token, one audit trail, and one CLI — the open plane optimized for *integrity
and authenticity* (default-deny cross-agent policy, attributed
curation/forgetting, security groups, authentic messaging) and the sealed plane
optimized for *confidentiality* (KMS-backed envelope encryption, reveal-gated
access, never embedded/recalled/in-digest/plaintext-exported). The wedge is the
seam no competitor crosses, not parity on either side.

### Own the agent-as-principal model

The reviewed field isolates by user, project, namespace, or framework runtime.
Witself's enforced agent-as-authenticated-principal model (token = identity,
bound to one realm + one agent) is the foundation that makes cross-agent policy
and messaging real rather than advisory. Keep this front and center; it is the
hardest thing for a memory layer to retrofit.

### Be vendor-neutral on embeddings

Native memory locks an agent's self to one vendor. Keep the embedding-provider
abstraction (`voyage` default, `openai`, `local-dev`) capability-gated, with
deterministic degradation to keyword/tag/time recall when no provider is
available. Portability is a selling point against OpenAI/Anthropic native
memory. See [memory-model.md](memory-model.md).

### Treat A2A and frameworks as bridges, not rivals

Position Witself as the governed identity-and-memory system of record that
multi-agent frameworks and A2A transports can sit on top of. Track A2A
bridging, temporal/graph recall (Zep-style supersession), and a dedicated
vector engine at scale as post-v0 enhancements rather than v0 claims.

### Match secrets-manager guardrails, then out-govern them

On the sealed plane, adopt the conservative defaults the category has converged
on — local-first MCP, `--no-value-tools` and `--read-only` modes, runtime
injection over plaintext config, scoped/revocable per-agent access — and then
win on the agent-as-principal model and the cross-plane carve-out that no
secrets manager offers. Keep the reveal ceremony explicit and audited, never
hand sealed material to the model implicitly, and treat dynamic/short-lived
leases (Vault/Akeyless/Infisical) as a post-v0 enhancement, not a v0 claim. See
[secret-model.md](secret-model.md) and
[authorization-and-roles.md](authorization-and-roles.md).

### Keep CLI-native administration as a wedge

Across both planes, CLI-first administration with `--json`, `--dry-run`,
`--reason`, and an AI-assistable surface (same auth, permissions, audit, and
confirmations as a human) is a differentiator against products that assume a
hosted dashboard. The same CLI manages realms, agents, policies, and groups on
the open plane and secrets, grants, and TOTP on the sealed plane. See
[cli-command-surface.md](cli-command-surface.md) and
[billing-and-limits.md](billing-and-limits.md).
