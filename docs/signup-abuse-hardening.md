# Signup Abuse Hardening

Status: live in production since 2026-08-29 (#295). The committed
`CP_SIGNUP_OPEN` gate is `true`, the daily quotas are 10 signups per source IP
and 500 globally, and the production Turnstile widget and runtime-only values
are active. The two Cloudflare rate-limit bindings are safe-always-on in the
production template and guarded for tests and development environments where
the bindings are absent.

The controls run before invite reservation. They therefore protect the public
entry point and do not spend or disclose an invite when a request fails an
abuse check.

## Controls

### Open-signup gate

`CP_SIGNUP_OPEN` is a committed plain-text gate. Production uses the exact value
`true`, which makes an invite optional; every other value preserves the
established invite-required behavior. An invite-carrying request always follows
that established path.

An invite-less request fails closed unless Turnstile is enabled with its secret
key configured and both committed daily limits are positive integers. It must
then pass Turnstile verification, the per-IP and global daily counters, and
provide both valid consent-version fields. A missing runtime prerequisite
returns one value-free `503` configuration refusal without identifying the
missing control. Setting `CP_SIGNUP_OPEN=false` is the production rollback to
invite-only signup.

### Cloudflare edge rate limits

The committed control-plane deployment has two new unsafe rate-limit bindings:

| Binding | Scope | Limit | Placement |
|---|---|---:|---|
| `PUBLIC_IP_LIMITER` | Source IP | 300 requests per 60 seconds | At the top of the public Worker fetch path |
| `SIGNUP_IP_LIMITER` | Source IP | 5 signup requests per 60 seconds | Before the AccountSignup Durable Object call |

The general limiter deliberately exempts internal machine-lifecycle bridge
paths and `POST /v1/intake/support-email`. Those paths already have their own
credentials and controls, and a shared `429` would make their bridges classify
the response incorrectly. A missing binding is inert so plain Node tests and
local development preserve their previous behavior.

Limiter denials return `429 Too Many Requests`. Logs contain only value-free
denial information; they never contain a source IP, signup body, invite, or
challenge token.

### Durable daily signup quotas

`CP_SIGNUP_DAILY_LIMIT_PER_IP` and `CP_SIGNUP_DAILY_LIMIT_GLOBAL` control two
UTC-day quotas. An absent value or the exact value `0` disables that quota;
only an exact positive integer enables it. Production commits and attests the
ratified values `10` per IP and `500` globally.

The per-IP scope is the SHA-256 hex digest of `CF-Connecting-IP`; the global
scope contains no customer value. Both scopes use the existing
`ACCOUNT_SIGNUP` Durable Object class under these instance names:

- `signup-counter:ip:<sha256>`
- `signup-counter:global`

Each allowed consumption records the caller's durable `provision_id` with its
verdict. A retry of the same signup returns that committed allowance without
consuming a second unit; an at-limit denial writes no per-provision marker.
Before the first counter call, the signup authority checkpoints the chosen
hashed IP scope, so an ambiguous retry from a different network still reaches
any committed marker. Prior-day keys are pruned lazily. A denied request
returns `429` and leaves no signup state or reserved invite. Once the ordinary
signup phase is initialized, replays skip the abuse controls and resume the
existing durable workflow.

### Turnstile

Turnstile is enabled only when `CP_SIGNUP_TURNSTILE_ENABLED` is exactly `true`
and the runtime secret key is present. A usable browser flow additionally
requires the site key. All three values are runtime-only break-glass secrets,
not entries in `secrets.required` or committed plain-text variables:

- `CP_SIGNUP_TURNSTILE_ENABLED`
- `CP_SIGNUP_TURNSTILE_SECRET_KEY`
- `CP_SIGNUP_TURNSTILE_SITE_KEY`

When enabled, account creation verifies the supplied `turnstile_token` against
Cloudflare before daily quota consumption or invite reservation. A bad,
missing, or duplicate token returns `403 Forbidden` with `challenge_url` set to
`https://self.witwave.ai/signup/challenge`. The production challenge page is
live; it still returns `404` unless the exact gate and site key are both
configured. Its restrictive content-security policy permits Cloudflare's
challenge script and frame only.

Network failure, a Cloudflare `5xx`, or a malformed verification response
returns retryable `503 Service Unavailable`. This is intentionally fail closed:
an unmetered signup window is worse than a paused one, and the runtime gate is
operator-flippable within minutes. A successful verification is persisted in
the signup phase so a workflow retry never re-verifies or exposes the token.
Challenge tokens and the secret key are never logged or included in the durable
request fingerprint.

## Production activation and rollback

The keyed checklist completed on 2026-08-29. The production widget is restricted
to `self.witwave.ai`; its enablement, site key, and secret key remain runtime-only
break-glass values and must never be committed, printed, or logged. The reviewed
activation changed the two daily limits from `0` to `10` and `500` in the same
change that set `CP_SIGNUP_OPEN=true`, after the challenge page and an invited
canary had been verified; the deployment contract now pins the active values.

If Turnstile verification becomes unhealthy, restore `CP_SIGNUP_OPEN=false`
immediately to close invite-less signup. The runtime Turnstile gate may then be
disabled separately through the break-glass path if invited signup must continue
during verifier trouble. Any future quota or gate change must update the pinned
control-plane deployed-binding fixtures and retain the same fail-closed
attestation; runtime-only keys must never become committed configuration.
