# Live Runtime Memory Acceptance

Status: executable four-runtime acceptance harness for production-readiness
gate [#45](https://github.com/witwave-ai/witself/issues/45). The harness covers
Codex, Claude Code, Cursor, and Grok Build. GitHub Copilot's phase-one
guided-MCP adapter has no transcript hooks and remains outside this gate, as
does Gemini.

This is a live-client test, not a backend simulation. `witself` prepares
isolated synthetic fixtures and exact prompts, the operator gives each prompt
to the real authenticated AI client, and `witself` verifies the resulting
transcripts and backend state. Witself never launches or wakes a provider; the
operator may invoke the real client directly or through a foreground test
helper. No persistent runner or backend inference participates.

## Contract

The same six stages run in every provider:

1. verify the token-derived account, realm, and agent identity;
2. explicitly capture one narrative in Witself (and not provider-native
   memory);
3. start a new client session and answer a history-dependent question without
   a user instruction to search or recall;
4. produce a broad result without exposing a synthetic sensitive fact;
5. intentionally retrieve that exact synthetic sensitive fact; and
6. search only the current agent's default memory scope for a peer-only
   fixture and demonstrate isolation.

Preparation also creates one pending synthetic curation checkpoint. Backend
evidence proves that exact request reached a canonical applied zero-action plan
by verification. Because transcript delivery is asynchronous, the v1 evidence
does not attribute that apply to the identity stage—or to any specific session.
It therefore makes a lifecycle claim, not a stage-causality claim. Foreground
curation remains subordinate to each stage: the client must complete the stage's
requested work and deliver its user-facing answer even when it also processes
the pending checkpoint.

The delivery claim remains capability-accurate:

| Runtime | Session/checkpoint context | History-dependent recall |
| --- | --- | --- |
| Codex | automatic hook `additionalContext` | automatic hook `additionalContext` |
| Claude Code | automatic hook `additionalContext` | automatic hook `additionalContext` |
| Cursor | managed instruction plus guided `self.show` | guided MCP `memory.recall` |
| Grok Build | managed instruction plus guided `self.show` | guided MCP `memory.recall` |

Guided means the active foreground client follows the installed always-on
policy without the user asking it to search. It is not renamed automatic hook
injection. The evidence records this distinction.

## Prerequisites

For certifying evidence (rather than a rehearsal), use:

- one release-identifiable `witself-server` build (`/v1/version` must contain a
  semantic version, release commit, and build date);
- the matching released `witself` CLI (same version and commit as the server),
  installed for the provider with `witself install`;
- one fresh synthetic test agent with no narrative memories and no pending
  curation request;
- a distinct peer test agent in the same account and realm; and
- the real provider client restarted after installation so its managed
  instructions and hooks are active.

Preparation fails closed when the provider executable, installed integration
record, current client version, token-derived identity, hooks, managed routing
instructions, peer credential, or server build cannot be verified. A stale
recorded client version requires reinstalling the provider integration. MCP
availability and behavior are then exercised by the live stages themselves.

Use a fresh subject agent for each certifying run. `--rehearsal` permits an
existing-memory agent or development server, but the resulting evidence is
marked `pass_rehearsal` and cannot close #45.

Preparation never rewrites the selected runtime binding, hooks, instructions,
or tokens. It does write synthetic memories, a synthetic fact, and a curation
request to the bound subject, plus one peer-only memory to the selected peer.
Never point even a rehearsal at Scott or another everyday agent; use dedicated
test agents.

## Run One Provider

Prepare the synthetic fixtures and private run state:

```text
witself memory acceptance prepare \
  --runtime codex \
  --agent codex-test-bot \
  --peer-agent claude-test-bot
```

`--account` and `--realm` default to the installed binding. The test subject
must exactly match that binding. If the peer credential is not stored in the
normal local account/realm token layout, add `--peer-token-file FILE`; its path
is retained only in the private state.

Preparation prints a state path such as:

```text
~/.witself/acceptance/mra_example.json
```

The state is a mode-0600 JSON file. It contains synthetic canary values and
idempotency keys, so do not commit, paste, or archive it as certification
evidence. Preparation writes it before creating fixtures and updates it after
each idempotent mutation. The curation request's due time is derived from the
persisted `prepared_at`, so a crash after the backend commit can replay the
same request hash. A partial preparation can be resumed exactly:

```text
witself memory acceptance prepare \
  --runtime codex \
  --state ~/.witself/acceptance/mra_example.json
```

Now use the real provider client. Run the stages in the printed order. For each
stage, start a new provider session or task and paste exactly one prompt.
Reprint all prompts or one stage with:

```text
witself memory acceptance prompts \
  --state ~/.witself/acceptance/mra_example.json

witself memory acceptance prompts \
  --state ~/.witself/acceptance/mra_example.json \
  --stage history-recall
```

Starting a new session for every prompt is deliberate. The verifier requires
six distinct transcript IDs. This proves that explicit capture in one session
is used by another session instead of accidentally reusing the provider's
conversation buffer.

After all six stages complete, verify and retain sanitized evidence:

```text
witself transcript flush --runtime grok-build  # Grok Build only

witself memory acceptance verify \
  --state ~/.witself/acceptance/mra_example.json \
  --out evidence/runtime-memory-codex.json \
  --wait 30s
```

`--wait` only polls for normal asynchronous transcript flushing and foreground
work already performed by the active client. It never wakes a client or starts
a background process. Use `--wait 0` for one immediate check.

The explicit Grok flush is a deterministic post-client transcript fence. Grok
persists its final assistant chunk after its synchronous Stop hook returns, so
the command finalizes any last local Stop event from the already-closed native
session before verification. It launches neither Grok nor inference. Normal
interactive use also retries automatically through the Stop-triggered one-shot
flusher and every later Grok hook.

Repeat with fresh provider-bound subject agents for `claude-code`, `cursor`,
and `grok-build`. A #45 certification set consists of four `status: "pass"`
evidence documents that name the same Witself release and commit.

## What Verification Checks

The verifier combines two independent evidence sources:

- visible transcript entries prove the real runtime and observed client
  version, exact prompt boundaries, six distinct sessions, identity response,
  history answer, sensitive-output behavior, and absence of the peer marker.
  The exact user-prompt entry and every post-prompt assistant response entry
  used by a stage must each carry the pinned runtime and client version; an
  unrelated entry in the same transcript cannot supply provenance;
- direct authenticated backend reads prove the explicit memory exists, the
  exact curation request reached an applied plan whose canonical hash is the
  deterministic zero-action plan hash, and the canonical run carries a
  nonempty apply-receipt ID and applied timestamp for that exact fixture
  request. This proves lifecycle completion by verification but deliberately
  does not correlate the apply to a transcript or stage. Backend reads also
  prove the sensitive fact is clear on exact read and redacted in a broad list,
  the peer can read its own fixture, and the subject cannot read the peer
  fixture through its default owner scope.

The harness does not accept a self-reported provider response as the only
proof. It also does not infer a successful memory write merely because a
transcript says “remembered.”

## Sanitized Evidence Schema

The retained schema is
`witself.runtime-memory-acceptance.evidence.v1`. Its top-level fields are:

```json
{
  "schema_version": "witself.runtime-memory-acceptance.evidence.v1",
  "suite_version": "1",
  "run_id": "mra_...",
  "status": "pass",
  "certification_eligible": true,
  "prepared_at": "2026-07-16T00:00:00Z",
  "verified_at": "2026-07-16T00:05:00Z",
  "runtime": {
    "name": "codex",
    "delivery": {
      "session_automatic": true,
      "session_mode": "hook_additional_context",
      "recall_automatic": true,
      "recall_mode": "hook_additional_context"
    },
    "client_version": "1.2.3",
    "configured_version": "1.2.3",
    "observed_versions": ["1.2.3"]
  },
  "witself": {
    "version": "0.0.172",
    "commit": "abcdef1",
    "date": "2026-07-16T00:00:00Z"
  },
  "identity": {},
  "peer_identity": {},
  "cases": []
}
```

Every report contains exactly seven named cases:

- `identity_binding`
- `explicit_narrative_capture`
- `history_dependent_recall`
- `applied_empty_curation_checkpoint`
- `sensitive_exact_and_broad_redaction`
- `same_agent_cross_session_continuity`
- `cross_agent_isolation`

Case resources contain only transcript, memory, fact, curation-request, run,
and apply-receipt IDs plus value-free counts. Before writing a report, the
verifier serializes it and rejects any occurrence of the three private
synthetic markers.

The evidence intentionally excludes bearer tokens, token paths, endpoints,
local configuration paths, prompt text, transcript bodies, fact values, memory
content, provider model identifiers, and peer-only canaries. It is suitable for
a protected CI artifact or issue attachment after ordinary review. The CLI
writes an `--out` evidence file with mode `0600`; explicitly copy or change its
permissions only when publishing the reviewed sanitized artifact.

## Failure Interpretation

Common failures are intentionally specific:

- `identity_binding` — stale/missing integration, wrong token, hooks or managed
  instructions not active, a prompt or response entry without its own pinned
  runtime/version provenance, reused transcript IDs, or a response that did not
  use the authenticated identity;
- `explicit_narrative_capture` — the real provider never produced a durable
  memory containing the synthetic narrative marker;
- `history_dependent_recall` — the second session did not use the prior
  narrative under the runtime's declared delivery mode;
- `applied_empty_curation_checkpoint` — the pending fixture request was ignored,
  left active, applied a nonempty plan, or lacked a canonical applied run and
  apply-receipt ID; this case does not identify which session performed it;
- `sensitive_exact_and_broad_redaction` — exact retrieval failed, the assistant
  did not put the requested value in its final answer, or a broad answer exposed
  the synthetic private marker;
- `same_agent_cross_session_continuity` — capture and recall occurred in one
  provider session rather than two; and
- `cross_agent_isolation` — the peer could not retrieve its fixture or the
  subject's default owner scope could retrieve it.

Failed runs remain inspectable and retryable. The harness performs no permanent
deletion and does not broaden cross-agent policy to make a test pass.
