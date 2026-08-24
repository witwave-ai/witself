# Signup Abuse Hardening

Status: implemented dark for the future open-signup path. The committed daily
quotas are disabled at `0`, and the Turnstile runtime-only secrets are absent.
The two Cloudflare rate-limit bindings are safe-always-on in the production
template and guarded for tests and development environments where the bindings
are absent. This slice does not relax the existing invite requirement.

The controls run before invite reservation. They therefore protect the public
entry point before signup opens and do not spend or disclose an invite when a
request fails an abuse check.

## Controls

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
only an exact positive integer enables it. Both values are committed and
deployment-attested as `0` in the dark release.

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
`https://self.witwave.ai/signup/challenge`. The public challenge page is itself
dark: it returns `404` unless the exact gate and site key are both configured.
Its restrictive content-security policy permits Cloudflare's challenge script
and frame only.

Network failure, a Cloudflare `5xx`, or a malformed verification response
returns retryable `503 Service Unavailable`. This is intentionally fail closed:
an unmetered signup window is worse than a paused one, and the runtime gate is
operator-flippable within minutes. A successful verification is persisted in
the signup phase so a workflow retry never re-verifies or exposes the token.
Challenge tokens and the secret key are never logged or included in the durable
request fingerprint.

## Keyed enablement checklist

These steps are **needs-Scott** wherever they require a new external resource,
raw secret value, or product limit decision. Keep the invite gate in place
until the complete acceptance check passes.

1. Mint a Cloudflare Turnstile widget restricted to the exact production host
   `self.witwave.ai`.
2. Provide its site key and secret key for installation as the runtime-only
   `CP_SIGNUP_TURNSTILE_SITE_KEY` and
   `CP_SIGNUP_TURNSTILE_SECRET_KEY` break-glass secrets. Do not commit, print,
   or log either value.
3. Choose positive daily per-IP and global signup limits. Review the interaction
   with the fixed 5-per-minute signup burst limit and expected launch volume.
4. Land a separate template-and-verifier activation PR that replaces both `0`
   defaults with the chosen exact values and updates every pinned control-plane
   deployed-binding fixture. Deploy and attest that release while all three
   Turnstile activation secrets are still absent.
5. Install the site and secret keys through the reviewed break-glass path, then
   set `CP_SIGNUP_TURNSTILE_ENABLED=true` last with
   `npm run secret:put:break-glass -- CP_SIGNUP_TURNSTILE_ENABLED`.
6. Verify `/signup/challenge` renders without account data, complete one
   challenge, and create one canary account with
   `witself account create ... --challenge <token>`. Confirm an invalid token
   returns `403`, a forced verifier outage returns `503`, and burst and daily
   denials return `429` without creating signup state or reserving an invite.
7. Retain value-free deployment and canary evidence before relaxing the invite
   requirement. If verification becomes unhealthy, remove or make the enabled
   gate non-`true` immediately; a verifier outage pauses signup only while the
   exact gate remains active.

The dark deployment assertion refuses any persistent Turnstile activation
secret during this slice. Future ordinary deployments must update that
attestation deliberately as part of the reviewed activation posture; they must
never silently convert the runtime-only keys into committed configuration.
