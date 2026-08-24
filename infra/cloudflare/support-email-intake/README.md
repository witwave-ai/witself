# Witself support email intake bridge

This Cloudflare Email Worker is implemented **dark**. Both its
`SUPPORT_EMAIL_INTAKE_ENABLED` gate and the control plane's independent intake
gate must be exactly `"true"` before one message can reach support intake.
The committed default is `"false"`.

## Trust model

The Worker accepts only the exact `support@witwave.ai` recipient and one plain
RFC 5322 `From` address. That visible `From` address—not the envelope sender—is
sent to the control plane for account matching. The copied agent-email
Authentication-Results parser selects only the first header from the configured
trusted authserv-id. Intake requires that selected DMARC result to be exactly
`pass`; absent, malformed, untrusted, or non-pass results are dropped.

Loop-shaped mail is dropped before egress: automatic submissions, bulk/junk/
auto-reply precedence, list mail, an empty envelope sender, and visible mail
from `support@witwave.ai` or `no-reply@witwave.ai`. Raw messages over 256 KiB
are rejected. The first plain-text MIME part is decoded and bounded to 64 KiB;
HTML-only messages are dropped. A non-empty Message-ID is mandatory so control-
plane replay deduplication is always possible.

## No-send invariant

This package has no `send_email`, KV, service/cell, queue, R2, D1, Durable
Object, Container, HTTP route, or other application binding. It cannot reply,
backscatter, or contact a cell. Its sole egress is an authenticated HTTPS POST
to the configured public control-plane origin. Responses and errors are
value-free; the Worker never logs or returns sender, subject, or body.

Two exact Rate Limiter bindings bound the front door and are pinned by tests:

- `SUPPORT_EMAIL_SENDER_LIMITER` (`2401`): 10 messages per sender per 60 seconds.
  Denial drops the message without a retry.
- `SUPPORT_EMAIL_GLOBAL_LIMITER` (`2402`): 100 messages per 60 seconds. Denial
  or unavailability throws so Cloudflare retries the message.

## Value-free decisions

The decision vocabulary is `drop_wrong_recipient`, `drop_invalid_size`,
`reject_size`, `drop_auto_submitted`, `drop_precedence`, `drop_list_id`,
`drop_empty_envelope_sender`, `drop_invalid_from`, `drop_loop_sender`,
`drop_gate`, `drop_authserv_id`, `drop_dmarc`, `drop_sender_rate`,
`drop_html_only`, `drop_invalid_content`, `drop_message_id`, and `forward`. No
reason contains message or identity data.

## Local verification

```sh
npm ci
npm test
npm run bundle:check
```

`npm run config` writes the ignored `wrangler.generated.jsonc`. It requires a
credential-free public HTTPS `CONTROL_PLANE_URL`; the two dark variables default
to `"false"` and `""` when omitted. `npm run deploy` additionally requires a
clean checkout at one semantic release tag and the reviewed production
Cloudflare identity. Deployment freezes the exact tagged Worker modules into a
private read-only snapshot, verifies them before and after Wrangler runs, and
never bundles live worktree source.

## Enablement

1. Deploy the dark control-plane intake route first.
2. Mint one `SUPPORT_EMAIL_INTAKE_TOKEN`; store it as the control plane's
   `SUPPORT_EMAIL_INTAKE_TOKEN` and this Worker's
   `CONTROL_PLANE_SUPPORT_INTAKE_TOKEN`. Never place it in vars or files.
3. Send a probe through witwave.ai Email Routing and capture Cloudflare's exact
   trusted Authentication-Results authserv-id. Set
   `SUPPORT_EMAIL_AUTH_RESULTS_AUTHSERV_ID` to that value.
4. Deploy this Worker with the canonical `CONTROL_PLANE_URL`, while keeping
   `SUPPORT_EMAIL_INTAKE_ENABLED="false"`.
5. Add the exact `support@witwave.ai` Email Routing rule without replacing the
   preserved catch-all route.
6. Enable the control-plane gate, verify it, and only then flip this Worker's
   gate to exact `"true"`. Re-run `npm test` and `npm run bundle:check` before
   each release.

Disable the Worker gate first during rollback. It never sends mail, so disabling
it cannot generate a reply or backscatter.
