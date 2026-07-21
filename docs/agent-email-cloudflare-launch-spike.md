# Agent Email Cloudflare Launch Spike

Status: **STRICT PRODUCTION GATE FAILED — LIMITED PILOT AUTHORIZED**

- Run date: 2026-07-21 UTC
- Canonical design commit: `f350825b71614db9470f059cb6cdf445e515b247`
- Cloudflare account: `8f0bf04a4e7aab3a8cc60f02cc8c8fdb`
- Cloudflare zone: `witwave.ai` (`48924798d030e81963833193e1dde868`)
- Receive domain: `agent-mail.witwave.ai`

This report records the launch-gating spike required by Open Question 9 in
[agent-email.md](agent-email.md). It is evidence, not a replacement design.
No schema, ingestion, Worker, CLI, MCP, API, metering, retention, or archive
code was started during the spike. A subsequent operator decision authorized a
capability-limited internal pilot while preserving the failed requirements as
production promotion gates.

## Decision

Cloudflare Email Routing can cover the launch subdomain, applies its inbound
authentication gate before Worker dispatch, and invokes a Worker once per
envelope recipient. The current Worker contract cannot, however, implement two
settled requirements:

1. `ForwardableEmailMessage.setReject(reason)` is documented as a permanent
   SMTP error. There is no supported API for selecting a temporary `451`
   response. Throwing from the handler produced provider-managed retries in the
   spike, but that is precisely the exception fallthrough that the settled
   contract forbids.
2. The Worker event does not expose trusted structured SPF, DKIM, DMARC, spam,
   or provider-message-id fields. It exposes envelope addresses, MIME headers,
   the raw stream, raw size, and forwarding capability. The only authentication
   data visible inside the spike Worker was in MIME headers, which the settled
   contract explicitly forbids treating as authoritative.

The strict production launch gate therefore fails. This is not a total
Cloudflare receive no-go: the proven baseline is sufficient for an explicitly
limited internal pilot. Production promotion remains blocked pending an
inbound edge that exposes the required synchronous controls and metadata, or a
separate operator decision that revises the production contract.

## Pilot Authorization And Boundary

Development may proceed under the Cloudflare limited receive-only pilot in
[agent-email.md](agent-email.md#capability-tiers-and-authorized-pilot). The
authorization is deliberately narrower than the production contract:

- one internal realm and 5–10 explicitly enrolled agents;
- one exact Email Routing rule per pilot address, with the pre-existing global
  catch-all left unchanged;
- a default-off feature flag and realm/agent allowlist;
- a 5 MiB raw-MIME cap and no raw-MIME or attachment-content retrieval;
- durable cell commit before successful Worker completion;
- deliberate sanitized Worker exceptions for transient failures, relying on
  the provider-managed retry observed here without claiming an explicit `451`
  response, retry count, or retry schedule;
- all messages marked sender-unverified, authentication/spam `unknown`, and
  provider id unavailable; MIME authentication headers remain untrusted;
- no billable receive usage or inbound quota enforcement during the pilot;
- raw-digest, envelope-recipient, and envelope-sender grouping for suspected
  duplicates, with no automatic drop, overwrite, or deletion;
- only already-expected, low-risk verification-code workflows, with no
  financial, identity-proofing, account/password recovery, credential/domain
  transfer, automated link following, or similarly consequential automation;
  and
- a synthetic exact-route canary for durable accept and exception/retry, plus
  rollback by disabling the feature and removing only pilot exact rules.

This boundary makes unavailable capabilities explicit instead of representing
them as passed, empty, or inferred fields. Catch-all Worker cutover, external
realm enrollment, messages above 5 MiB, billable receive, sender-auth-dependent
automation, and production delivery claims remain out of scope.

## Gate Matrix

| Requirement | Result | Evidence |
| --- | --- | --- |
| Configure `agent-mail.witwave.ai` for Email Routing | Pass | Cloudflare created three MX records and an SPF TXT record; the subdomain reports `ready`. |
| Full coverage on the configured subdomain | Pass, with integration caveat | A random address with no literal rule matched the existing zone-global catch-all and was forwarded. The catch-all is not subdomain-scoped; routing it to the Email Worker would also place apex catch-all traffic through that Worker, which would need an explicit apex-preserving forwarding branch. The production catch-all was not changed during this spike. |
| Current routing/domain caps | Pass | Current limits are 200 routing rules per domain and 30 Email Routing plus Email Sending domains per zone, including the apex. |
| Inbound provider size cap | Pass by documentation | Cloudflare rejects inbound messages over 25 MiB. |
| Worker resource feasibility at 25 MiB | Partial | Workers Paid limits are 128 MB per isolate, 30 seconds default CPU (configurable to 5 minutes), and 10,000 subrequests per invocation. A 4.9 KiB stream was exercised; a 25 MiB end-to-end relay was not. The implementation must stream rather than multiply-buffer the message. |
| SMTP transaction latency | Partial | A deliberately delayed 15-second handler completed as handled. No Email Worker-specific SMTP transaction deadline was found in current documentation, and raw inbound SMTP could not be reached from the test environment to find the upstream timeout. |
| One Worker invocation per recipient | Pass | After rule propagation, one two-recipient submission produced two Worker invocations at the same timestamp, with the same Message-ID and distinct `message.to` values. |
| SPF-or-DKIM gate before Worker dispatch | Pass | Cloudflare's inbound lifecycle runs authentication before rule matching and Worker dispatch. Current postmaster policy requires SPF or DKIM. Activity details for Worker probes showed SPF, DKIM, and DMARC `pass`. |
| Permanent SMTP rejection | Pass | `setReject()` produced one permanent rejection event and no retry in the controlled sender path. Cloudflare documents this action as a permanent SMTP error. |
| Explicit temporary `451` | **Fail** | No status-code or temporary-reject API exists. Throwing generated a Cloudflare `temporary error` and retries, but the client cannot select or guarantee an explicit `451` without forbidden provider-default exception behavior. |
| Trusted auth/spam/provider identity in the Worker event | **Fail** | The event interface has no structured authentication results, spam verdict, or provider-generated message identifier. Cloudflare exposes structured results in post-event activity/analytics, too late for the signed synchronous relay. |

Current limit and interface sources:

- [Email Service limits](https://developers.cloudflare.com/email-service/platform/limits/)
- [Workers limits](https://developers.cloudflare.com/workers/platform/limits/)
- [Email Worker handler and `ForwardableEmailMessage`](https://developers.cloudflare.com/email-service/api/route-emails/email-handler/)
- [Inbound Email Routing lifecycle](https://developers.cloudflare.com/email-service/concepts/email-lifecycle/)
- [Email Routing mail-authentication requirement](https://developers.cloudflare.com/email-service/reference/postmaster/#mail-authentication-requirement)

## Live Probe Evidence

The temporary Worker was `witself-agent-email-spike`. The final deployed
version was `d263f1b2-b6ec-4924-9c45-ee9b8c01279c`; its `index.js` SHA-256 was
`ae56105a25d2c4f0690dd8f82cd660d8567dc0e8df4f7efc9fcde9c484479279`.
Two exact-address rules routed synthetic recipients to that Worker:

- `b9b52f71090f4039b3a2c9ab3bbb094e`
- `a10783aa63a54dba80e52576c5a5c116`

All payloads were synthetic and contained no user mail.

| Probe | UTC observation | Message-ID | Result |
| --- | --- | --- | --- |
| Accept and stream read | 2026-07-21 05:07:34 | `<NS5oSStBs7Es9Gu1HAv2SXfDohZoq05tziAL@witwave.ai>` | One handled invocation; raw and observed size 4,771 bytes; SHA-256 completed; 2 ms Worker elapsed time. |
| Permanent reject | 2026-07-21 05:10:27 | `<PJlkHCpt4WSnpJ6IQJ6bBBbcDuB5keHCkc3n@witwave.ai>` | `setReject()` produced one delivery-failed event. Activity reason: `Worker rejected email` with the synthetic reason. No retry was observed. |
| Exception / provider tempfail | 2026-07-21 05:10:29–05:15:53 | `<PTK47t0AddeRnKoIuqqLuIUI8AQocoPYCA9v@witwave.ai>` | Eight failed invocations with the same Message-ID. Activity reason: `temporary error: worker script threw an exception`. This demonstrates provider retry behavior, not a supported explicit-451 contract. |
| Delayed completion | 2026-07-21 05:10:45 | `<5XlQMpRg1lu3ccJFUE1bowkrfgaxTtZ1LX3c@witwave.ai>` | One handled invocation after 15,001 ms; raw size 4,769 bytes. |
| Random subdomain recipient | 2026-07-21 05:14:59 | `<5byC6d35l7Q0dI3T2iWvZjQIkTDzwKpAAAsL@witwave.ai>` | No literal rule existed. The zone-global catch-all matched and the Activity Log recorded `Forwarded`, action `Forward`. |
| Two envelope recipients | 2026-07-21 05:19:20 | `<iAdaoSgJBAZXc7wTvBRnhNkv64IrK6cyhepv@witwave.ai>` | Two handled invocations, one per `message.to`, at 05:19:20.236Z and 05:19:20.237Z. Both retained the same Message-ID; raw sizes were 4,858 and 4,860 bytes. |

The controlled sender path and Cloudflare Activity Log established retry and
permanent-rejection classifications, but did not expose the literal SMTP wire
responses. Direct TCP access to Cloudflare's inbound MX on port 25 timed out
from the test environment. The API token also lacked Zone Analytics Read, so
the GraphQL datasets could not be queried; the authenticated dashboard Activity
Log supplied the post-event classifications above.

## Configuration And Cleanup

The spike onboarded `agent-mail.witwave.ai` and left that valid launch
configuration in place:

- `route1.mx.cloudflare.net`, priority 13
- `route2.mx.cloudflare.net`, priority 38
- `route3.mx.cloudflare.net`, priority 32
- `v=spf1 include:_spf.mx.cloudflare.net ~all`

Temporary runtime state was removed after the evidence was sanitized:

- both exact-address spike rules deleted;
- temporary Worker deleted;
- all 14 `email-spike:event:` KV records deleted;
- local `.tmp/agent-email-spike` source and Wrangler state deleted;
- no temporary Worker or spike routing rule remains.

The pre-existing apex/zone catch-all remains enabled as rule
`368188ab5380434db8385b37781641ea`, priority `2147483647`, with matcher `all`
and its original single forwarding destination. Its action and destination were
not changed. The pre-existing exact `witwave.ai` routing rules were also left
unchanged.
