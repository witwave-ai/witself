# Support Email Intake

Status: dark. The support@ intake bridge is installed but inert unless both
the email Worker and control-plane intake gates are set to the exact string
`true`. Shipping or deploying either Worker does not activate the channel.

## Flow

Cloudflare Email Routing sends only the `support@witwave.ai` address rule to
the isolated support-email Worker. The Worker rejects messages over 256 KiB,
drops loops and automated mail, accepts a single plain sender only after a
trusted `Authentication-Results` header reports `dmarc=pass`, extracts bounded
plain text, and posts one authenticated request to the control plane.

The control plane takes a short processing reservation and retains terminal
email message-ID deduplication for seven days. It asks every cell for active
accounts whose contact email matches the sender and proceeds only when exactly
one active account matches across the healthy fleet. It drops archived,
unmatched, ambiguous, or incompletely checked requests. A subject ticket tag
attempts an owner-attributed reply first; a missing, closed, or otherwise
ineligible tagged ticket falls back to opening a new ticket. Without a tag, the
control plane opens a new ticket directly.

The cell revalidates the sender against the account contact email inside the
same transaction that opens or appends the ticket. Opening also enforces the
active-account, support-policy, and current plan-entitlement gates. Messages
are attributed to the account owner and carry only the email channel and
optional source message ID as metadata. The same non-empty message ID is
idempotent under the account transaction, so a provider replay cannot create a
second ticket or append the same email twice.

## Trust and isolation

DMARC is trusted only from the configured Cloudflare authserv-id. A sender's
own `Authentication-Results` header is not authority, and anything other than
an exact trusted DMARC pass fails closed. Matching the authenticated sender to
exactly one active account prevents email from selecting an arbitrary tenant;
the in-transaction match prevents a stale control-plane lookup from bypassing
that boundary.

The bridge Worker has no send binding, KV binding, or cell binding. It cannot
reply, backscatter, or reach a cell. Its only egress is the configured control
plane endpoint. HTTP responses and logs contain disposition or reason codes,
never sender addresses, subjects, or bodies. Failed control-plane delivery is
thrown for provider retry; the short reservation, terminal dedup marker, and
cell-side message-ID idempotency make that replay converge safely.

## Gates and drop reasons

The bridge gate is `SUPPORT_EMAIL_INTAKE_ENABLED`; the independent
control-plane gate is `CP_SUPPORT_EMAIL_INTAKE_ENABLED`. Both default to
`false`. The bridge additionally requires
`SUPPORT_EMAIL_AUTH_RESULTS_AUTHSERV_ID`, `CONTROL_PLANE_URL`, and the shared
`CONTROL_PLANE_SUPPORT_INTAKE_TOKEN`. The control plane holds the matching
`SUPPORT_EMAIL_INTAKE_TOKEN` secret.

Bridge decisions use these value-free reasons:

- `drop_wrong_recipient`: the envelope recipient is not
  `support@witwave.ai`.
- `drop_invalid_size`: the provider did not supply a valid positive raw size;
  `reject_size` means the message exceeds 256 KiB.
- `drop_auto_submitted`, `drop_precedence`, `drop_list_id`,
  `drop_empty_envelope_sender`, and `drop_loop_sender`: the message matches a
  specific automated-mail or loop guard.
- `drop_invalid_from`: the visible sender is not one single plain address.
- `drop_gate`: bridge intake is disabled; `drop_authserv_id` means no trusted
  authserv-id is configured; `drop_dmarc` means its trusted DMARC verdict is
  not exactly `pass`.
- `drop_sender_rate`: the per-sender rate limit refused the message.
- `drop_html_only`: no usable plain-text MIME body was found;
  `drop_invalid_content` means the decoded subject or body is blank;
  `drop_message_id` means replay-safe intake was impossible because the
  Message-ID was absent or invalid.
- `forward`: the bounded intake request was sent to the control plane.

Control-plane dispositions are `duplicate`, `drop_unmatched`,
`drop_ambiguous`, `drop_fanout_error`, `drop_archived`, `opened`, and
`replied`. They are deliberately value-free.

## Enablement checklist

These are **needs-Scott** activation steps. Enable only after the edge-DMARC
authserv-id has been captured and verified:

1. In Cloudflare Email Routing for `witwave.ai`, preserve the existing
   catch-all unchanged and add one exact `support@witwave.ai` address rule for
   the isolated support-email Worker.
2. Send a probe through that route, capture Cloudflare's real authserv-id from
   its `Authentication-Results` header, and configure
   `SUPPORT_EMAIL_AUTH_RESULTS_AUTHSERV_ID` with the reviewed value.
3. Mint one random `SUPPORT_EMAIL_INTAKE_TOKEN`. Install the same value as the
   control-plane `SUPPORT_EMAIL_INTAKE_TOKEN` and bridge
   `CONTROL_PLANE_SUPPORT_INTAKE_TOKEN` secrets without committing it.
4. Deploy both Workers with their gates still `false`; verify configuration,
   bindings, value-free logging, fleet health, and the preserved catch-all.
5. Set `CP_SUPPORT_EMAIL_INTAKE_ENABLED=true`, then set
   `SUPPORT_EMAIL_INTAKE_ENABLED=true`, and send one authenticated canary from
   the contact email of a support-entitled test account.
6. Verify exactly one owner-attributed ticket, its email metadata, and a
   duplicate replay disposition. Keep both gates off if any cell cannot be
   checked.

Darkening is immediate: set either gate back to `false` and redeploy. Existing
tickets remain available through the in-product support channel.
