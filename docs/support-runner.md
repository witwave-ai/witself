# AI Support Runner

Status: parked on 2026-08-31. The `witself-support-runner` binary is staged but
unloaded; no runner process serves the production queue. It remains inert by
default and refuses to serve unless `WITSELF_SUPPORT_RUNNER_ENABLED=true` is
set explicitly. Live support is human-driven through `witself-admin ticket`
commands, with Claude Code sessions assisting.

The runner is a single-purpose, read-mostly first responder for the managed
support queue. It lists and reads tickets through the fleet-admin control-plane
API, applies deterministic escalation rules before any model request, and may
post only a clearly attributed assistant reply or retriage a ticket. It cannot
perform refunds, billing changes, deletions, account changes, or arbitrary
operator actions.

## Operator-service boundary

Witself's no-wake doctrine applies to tenant agents: a message, email, hook,
MCP server, or backend checkpoint never launches an idle customer AI. The
support runner is different in kind. It is operator-run staff tooling, started
and supervised deliberately by Witself operations to serve Witself's own
support queue. It does not wake or impersonate a tenant agent.

Assistant replies use the reserved `assistant` author identity and render as
AI-authored in ticket threads and notifications. Human fleet-admin replies
retain their existing attribution.

## Processing contract

On each tick, the runner lists `open` and `awaiting_admin` tickets within the
configured lookback. It asks each cell for at most 500 candidates, while the
control plane bounds the merged fleet response to the newest 500. The runner
requires an explicit successful status from every reported cell and
`aggregate_capped` to be false; any cell error, timeout, missing result array,
or aggregate cap aborts the entire tick before a thread read or mutation.
Within an accepted response, the configured per-tick cap still limits thread
reads and model calls globally while unchanged recent tickets do not hide older
returned work. It
ignores unchanged threads, threads already touched by a fleet admin, threads
whose latest message is not customer-authored, and threads that already contain
the configured maximum number of assistant replies.
Immediately before posting, it reads the thread again and requires both an
eligible state and the same last-message ID. A concurrent update therefore
causes a silent freshness drop rather than a stale reply.

The process keeps its unchanged-ticket and permanent fleet-admin skip state in
memory only. Run exactly one replica. There is no distributed claim or lease in
this dark slice, so multiple replicas could evaluate or reply to the same
ticket concurrently despite the final freshness check. Restarting the process
also clears its in-memory suppression map.

## Mechanical escalation

The deterministic gate runs before any LLM call. A gated ticket remains in the
human queue: the runner posts no message, makes no ticket-state transition, and
does not set `first_response_at`. The only allowed mutation is an idempotent
retriage suggestion:

- category `security`: escalate and set priority to `urgent`;
- category `billing`: escalate without changing classification;
- refund, chargeback, or dispute language: escalate;
- legal, lawyer, attorney, subpoena, or court language: escalate;
- data-deletion, erasure, right-to-be-forgotten, or GDPR language: escalate;
- vulnerability, breach, hacked, or compromised language: escalate and set
  category `security` plus priority `urgent`;
- a request for a human or real person: escalate.

The gate scans the ticket subject and customer-authored message bodies using
lowercased word-boundary matching. Model refusals, API failures, malformed tool
output, unknown enums, and empty or oversized reply bodies are also fail-safe:
they cause zero mutations and only a value-free error log containing the ticket
ID.

## Configuration

The runner accepts the following environment variables:

| Variable | Default | Purpose |
|---|---:|---|
| `WITSELF_SUPPORT_RUNNER_ENABLED` | `false` | Mandatory explicit dark gate. Only `true` serves. |
| `WITSELF_SUPPORT_RUNNER_CONTROL_PLANE` | `https://self.witwave.ai` | Fleet control-plane endpoint. |
| `WITSELF_SUPPORT_RUNNER_ADMIN_TOKEN_FILE` | none | File containing the fleet-admin bearer token. Preferred for secret mounts. |
| `WITSELF_SUPPORT_RUNNER_ADMIN_TOKEN` | none | Inline fleet-admin bearer token fallback. |
| `WITSELF_SUPPORT_RUNNER_ANTHROPIC_API_KEY_FILE` | none | File containing the Anthropic API key. Preferred for secret mounts. |
| `WITSELF_SUPPORT_RUNNER_ANTHROPIC_API_KEY` | none | Inline Anthropic API key fallback. |
| `WITSELF_SUPPORT_RUNNER_MODEL` | `claude-opus-5` | Anthropic model name. |
| `WITSELF_SUPPORT_RUNNER_INTERVAL` | `60s` | Delay between queue scans. |
| `WITSELF_SUPPORT_RUNNER_MAX_TICKETS_PER_TICK` | `5` | Maximum candidates processed per tick. |
| `WITSELF_SUPPORT_RUNNER_LLM_TIMEOUT` | `120s` | Per-ticket LLM deadline. |
| `WITSELF_SUPPORT_RUNNER_MAX_ASSISTANT_REPLIES` | `3` | Assistant replies allowed before silent human escalation. |
| `WITSELF_SUPPORT_RUNNER_LOOKBACK` | `720h` | Age window used for ticket listing. |
| `WITSELF_HEALTH_ADDR` | `:8081` | Private health listener (`/livez`, `/readyz`, `/startupz`). |
| `WITSELF_METRICS_ADDR` | `:9090` | Private Prometheus listener. |

Durations use Go duration syntax. Counts and durations must be positive.
Credential files must contain one non-empty value; surrounding whitespace is
discarded. Logs and metrics must never include ticket content, credentials,
model output, account IDs, or customer-controlled labels.

## Version skew

Mixed cell versions are expected in the fleet. Older cells do not understand
`as_assistant`; their JSON decoder silently drops it and records a reply as a
human fleet-admin message. Therefore enablement requires every reachable cell
to be at this release or newer. Keep the runner dark through any mixed-version
interval.

## Enablement checklist

Activation remains parked. Before loading the binary and setting the gate to
`true`:

1. Provision a dedicated single-replica host with private health and metrics
   listeners and normal process supervision.
2. Verify every reachable cell is at this release or newer.
3. Mint the runner's credential with `scope: support_ai` (the CP refuses it
   everywhere except the ticket surface: list, get, reply, retriage, whoami —
   no state changes, no support-policy writes, no fleet administration),
   mount it, and configure its file
   path.
4. Mount the Anthropic API-key secret and configure its file path.
5. Verify the control-plane endpoint, queue visibility, escalation fixtures,
   value-free logs, and shutdown behavior while the gate remains off.
6. Set `WITSELF_SUPPORT_RUNNER_ENABLED=true` in one reviewed rollout and verify
   assistant attribution plus human-queue preservation before leaving it on.

Darkening is immediate: remove or set the flag to `false` and restart the
process. Tickets remain durable and available to human fleet admins throughout.
