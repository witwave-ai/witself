# Cloudflare receive-only agent-email edge

This directory contains the isolated Cloudflare Email Worker and route manager
for Witself inbound agent email. It is not the Witself control-plane Worker. It
has no HTTP route or control-plane Container binding, and it has no access to
the control-plane `DIRECTORY` KV namespace. The Worker and control plane instead
share only the dedicated email-route KV namespace.

`witmail.net` is reserved exclusively for agent email; it is not a general
mailbox or website domain. `AGENT_EMAIL_DOMAIN` names that primary domain and
`AGENT_EMAIL_LEGACY_DOMAINS` may name at most one compatibility domain. The
runtime can resolve a canonical realm label or managed realm alias on the
primary domain through `email:realm-route:v1:<domain>:<realm-label>`. Both
labels select the same realm and cell; the cell remains authoritative for the
agent segment, alias state, and account policy. A malformed, suspended,
retired, stale, or conflicting projection fails closed. Stale records are
refreshed through a bounded, authenticated control-plane lookup and are never
used when that lookup fails or returns an older controller revision. KV is a
route cache, never alias-claim authority.

Customer-owned domains use the same route key and unsigned schema-version-1
route shape only as a strict `route_kind: "custom_domain"` union variant. The
control plane wraps every canonical, managed-alias, and custom-domain route in
signed schema version 2 before returning it or writing KV. The edge verifies
that Ed25519 signature and the configured key id before it trusts the supplied
cell ingestion URL, and always does so before reading raw MIME. Unsigned,
unknown-key, malformed, or modified projections fail closed; corrupt KV is only
uncertain evidence and cannot redirect content. That variant
also requires the exact `domain_request_id`, `domain_allocation_revision`,
`realm_alias_claim_id`, and `realm_alias_revision` fences; canonical and
managed-alias projections continue to reject those fields. A valid address on
a non-managed domain is eligible for lookup only when
`AGENT_EMAIL_CUSTOM_DOMAIN_DELIVERY_ENABLED` is exactly `true`, and the
resolved projection is rechecked as `custom_domain`. Any other or missing gate
value tempfails before KV, the control plane, lookup limiters, or raw MIME are
read and emits only the fixed `tempfail_custom_domain_gate` edge outcome. The
gate is intentionally absent from the committed Wrangler template and
renderer. `npm run deploy` also refuses a persistent Worker secret with that
name both before and after deployment, so this contract remains dark and cannot
change live Email Routing or delivery configuration in this phase.

The original one-realm, 5–10-recipient literal-pilot code path is retained only
for bounded compatibility on the one configured legacy domain. Because its KV
rows predate signed route projections, the row may only narrow a destination
already fixed by two immutable Worker bindings:
`LEGACY_PILOT_TRUSTED_INGEST_URL` and
`LEGACY_PILOT_TRUSTED_CELL_AUDIENCE`. The committed production template omits
both bindings, so this release treats the legacy lane as retired and tempfails
before reading KV or raw MIME. Re-enabling it requires a separately reviewed
version that binds both anchors and exact literal Email Routing rules. Even
then, a legacy envelope must match the enabled pilot manifest and recipient
row, the KV destination must equal both anchors, and traffic never falls
through to a dynamic realm-route lookup. Merely listing
`agent-mail.witwave.ai` as the compatibility domain cannot make an address
reachable.

Managed alias delivery also requires
`REALM_EMAIL_ALIAS_DELIVERY_ENABLED=true`. The value is exact and defaults to
`false` at runtime; deployment rendering requires the operator to supply the
reviewed literal `true` or `false` so an omitted variable cannot silently
change live behavior. Any other runtime value tempfails `realm_alias` traffic
at the edge before a message body is read or a cell is contacted. The alias
gate does not control canonical Realm ID traffic or the independently retired
legacy literal-pilot lane.

Dynamic route lookup is protected independently of account policy. A positive
`EMAIL_DIRECTORY` projection is always checked first and
bypasses negative state. On a valid cold KV miss, the Worker hashes
`domain + NUL + realm-label`, coalesces identical in-flight lookups, and keeps
only a 10-second, 1,024-entry in-isolate SHA-256 miss-marker cache. The marker
contains no address, domain, or realm label. Only the one admitted live
control-plane lookup may turn an authoritative 404 with no prior route evidence
into a permanent unknown-recipient result; coalesced followers and later
marker hits tempfail so an activation race cannot create additional bounces.
A shared lookup that finds a valid fresh projection remains usable by all of
its followers.

Every control-plane fallback also requires one Cloudflare Rate Limiting
binding. Before that binding, fixed in-isolate windows strictly admit at most
10 cold and 100 known-or-uncertain leader lookups per 10 seconds; the two fixed
counters contain no label-derived state. Singleflight followers consume no
additional local or Cloudflare token. Cold misses use
`REALM_ROUTE_COLD_MISS_LIMITER`, configured for 10 calls per 10 seconds with the
fixed runtime key `cold-miss-v1`. Stale known routes, corrupt projections, and
KV read failures use
`REALM_ROUTE_KNOWN_MISS_LIMITER`, configured for 100 calls per 10 seconds with
the fixed key `known-miss-v1`. Missing bindings, binding errors, malformed
binding results, and denied admission all produce the same sanitized temporary
SMTP failure before the raw message is read. Labels never become limiter keys,
so rotating labels cannot create independent budgets. Cloudflare documents
these per-location counters as permissive and eventually consistent; they add
a shared protective layer around the strict per-isolate window, not exact
accounting, a billable quota, or a cross-location hard limit.

The Worker rejects messages larger than the 25 MiB transport ceiling, signs
the SMTP envelope plus raw-message digest with Ed25519, and relays the raw
message to the selected cell. The cell may return an exact plan-aware
`over_size` verdict for a lower account limit; the Worker maps it to a sanitized
permanent SMTP 552 rejection. An exact HTTP 429 `rate_limited` verdict instead
becomes a sanitized temporary provider result and a value-free
`tempfail_rate_limited` metric. Only a 2xx response containing exactly
`{"verdict":"accepted"}` or the deliberate accept-and-drop
`{"verdict":"feature_disabled"}` counts as SMTP success. This preserves the
Personal-plan discard behavior at the signed cell policy boundary. An exact
permanent cell verdict is rejected once without retry.

## Safety boundary

- Keep the existing Email Routing catch-all unchanged.
- Use only the dedicated `witself-agent-email-pilot-directory` KV namespace.
- Treat `CONTROL_PLANE_URL` as public configuration and
  `CONTROL_PLANE_EDGE_TOKEN` as a shared Worker secret. The matching
  control-plane route must validate that bearer token before consulting its
  durable route authority. Never put the token in Wrangler variables, KV,
  manifests, Git, logs, or generated configuration.
- Keep `global_fetch_strictly_public` enabled so the Worker reaches the
  DNS-only cell ingress through its public hostname even though both hostnames
  are in the `witwave.ai` zone. Signed headers are never followed across a
  redirect.
- Do not put relay private-key material in the manifest, generated Wrangler
  configuration, Git, logs, or cell configuration.
- Treat `pilot.example.json` as a shape example, not deployable values.
- Enable Email Routing subaddressing and run `status` to review the exact
  literal routes before activation. The route manager reports the live setting
  and refuses preparation or activation if it cannot be read or is disabled.
- Do not activate until the destination cell is enabled and healthy.
- The owning cell's PostgreSQL limiter is the sole authoritative account and
  delivery-throughput decision. The edge route-lookup limiters protect one
  shared dependency and never implement plan or billable usage. Every enrolled
  delivery reaches the cell feature check first, preserving accept-and-drop
  behavior for plan-disabled accounts without edge changes.
- A failed operation attempts to disable the pilot gate and its managed rules;
  inspect Cloudflare state before retrying any reported incomplete rollback.

The route-manager scripts in this directory still create and manage only the
reviewed literal pilot rules. They do not replace, disable, or redirect the
existing catch-all, and this change does not claim that full managed-domain
Email Routing has been promoted. Dynamic canonical and alias addresses receive
traffic only after a separate reviewed routing change directs that address
surface to this Worker.

The route manager reads and fingerprints the catch-all before and after every
operation. Its API client contains no catch-all update operation. It also
refuses to replace an unmanaged rule for an enrolled literal address.

## Local verification

From this directory:

```sh
npm ci
npm test
npm run bundle:check
```

`npm run config` requires `EMAIL_DIRECTORY_KV_ID`, `RELAY_KEY_ID`, and the
credential-free HTTPS origin `CONTROL_PLANE_URL`. It also requires
`AGENT_EMAIL_ROUTE_ED25519_PUBLIC_KEYS`, a canonical JSON object containing one
to four public Ed25519 keys indexed by key id, plus explicit literal values for
both managed delivery gates. Set
`REALM_EMAIL_ALIAS_DELIVERY_ENABLED=true` only for a reviewed alias activation;
the renderer rejects any value other than literal `true` or `false`. Set
`REALM_EMAIL_CANONICAL_DELIVERY_ENABLED=true` only for a separately reviewed
canonical-route activation; it has the same exact-boolean contract. The
runtime remains default-off if either binding is ever absent. The renderer
refuses the KV ID bound to
the adjacent control-plane Worker. The generated file is local operator state
and must not be committed. The generated Worker must expose
`REALM_ROUTE_COLD_MISS_LIMITER` at 10 calls per 10 seconds and
`REALM_ROUTE_KNOWN_MISS_LIMITER` at 100 calls per 10 seconds; the committed
template owns their distinct namespace IDs. `CONTROL_PLANE_EDGE_TOKEN` is
deliberately absent from both the template and generated file.

Rate-limit `namespace_id` values are account-wide, not repository-local. A
read-only account preflight on 2026-08-02 found only the control-plane recovery
limiter at namespace `1001`, so the committed email namespaces `2201` and
`2202` were unique at that check. Recheck all deployed Workers in the target
Cloudflare account immediately before every first deploy or namespace change;
sharing a namespace also shares counters for the same key. If either ID is in
use, stop and make one reviewed template-and-test change rather than deploying
a collision.

## Signed projection and release attestation

The control plane owns one active `AGENT_EMAIL_ROUTE_SIGNING_KEY_ID` and keeps
the matching PKCS#8 private key only in the
`AGENT_EMAIL_ROUTE_ED25519_PRIVATE_KEY` Worker secret. The email edge receives
only the raw 32-byte public key through
`AGENT_EMAIL_ROUTE_ED25519_PUBLIC_KEYS`. During rotation, publish an edge
keyring containing both old and new keys before changing the control-plane
signer. Keep the old public key until all routes have been republished and the
formal 600-second stale window has passed. Never reuse the relay signing key;
route authority and cell-relay identity are separate trust boundaries.

`npm run deploy` is release-only. It refuses a dirty checkout or a commit that
does not have exactly one `vMAJOR.MINOR.PATCH` tag, uses Wrangler strict mode,
adds deterministic version tag/message annotations, and stamps the full Git
commit and commit date into the Worker bindings. It then follows the current
Cloudflare deployment to its exact version and verifies one 100-percent
email-only version, the runtime and complete binding contract, both explicit
managed flags, the route-verification keyring, rate limiters, and the absence of
the custom-domain activation secret. `npm run verify:deployment` repeats the
same value-free inspection. `npm run bundle:check` performs a hermetic Wrangler
dry run and emits only source metadata and bundle/metafile SHA-256 digests.

After both tagged Workers are deployed, run
`npm run verify:route-signing-readiness`. This read-only check follows each
production deployment to its one exact 100-percent version and refuses unless
the control plane and email edge have the same immutable release identity,
both canonical controls and every custom-domain delivery control remain dark,
both edge managed-delivery flags are false, the control-plane signer id
is present in the edge's canonical public-key keyring, and all route-signing,
authenticated-fallback, and relay secret bindings exist. Its JSON attestation
contains key ids and binding-presence booleans, never public-key bytes or secret
values. It requires the release tag/message annotations from `npm run deploy`;
a later secret-only successor must be followed by a tagged redeploy before this
coordinated readiness check can pass again. Binding presence alone cannot prove
that the two fallback secrets contain the same value. The value-free receipt
from the coordinated provisioning command below is the evidence that one exact
validated token was uploaded to both Workers; preserve that receipt with the
release record.

## Staged managed rollout

Use a narrowly scoped Cloudflare token and set these environment variables in
the operator shell without printing their values:

- `CLOUDFLARE_API_TOKEN`
- `CLOUDFLARE_ACCOUNT_ID`
- `CLOUDFLARE_ZONE_ID`
- `EMAIL_DIRECTORY_KV_ID`
- `RELAY_KEY_ID`
- `CONTROL_PLANE_URL`
- `AGENT_EMAIL_ROUTE_ED25519_PUBLIC_KEYS`
- `REALM_EMAIL_ALIAS_DELIVERY_ENABLED`
- `REALM_EMAIL_CANONICAL_DELIVERY_ENABLED`

The token needs Workers deployment/secret access, Account Analytics Read,
Zone Settings Read, Email Routing Rules Write, and KV read/write for the
isolated namespace. The route-manager scripts validate the account, zone,
subaddressing setting, namespace, manifest, and existing routes before
mutation.

1. Create or locate the isolated KV namespace, then verify its exact title:

   ```sh
   npm run directory -- ensure
   npm run directory -- show
   ```

2. At one clean exact release tag, render both Workers with the same release
   identity. The control plane must contain the active
   `AGENT_EMAIL_ROUTE_SIGNING_KEY_ID`; the edge keyring must contain the
   corresponding public Ed25519 key; and both edge managed-delivery values must
   be exactly `false`. Keep all canonical and custom-domain controls absent.
   The existing alias-claim authority workflow may remain enabled because it is
   not a delivery gate.

   ```sh
   (
     cd ../control-plane
     npm run config
   )
   npm run config
   ```

   Use the coordinated ceremony once. Select the Witself secret and fields by
   their public ids or unambiguous names, and write the value-free receipt to a
   private, previously nonexistent path:

   ```sh
   npm run provision:route-signing-secrets -- \
     --agent AGENT \
     --route-secret ROUTE_SECRET \
     --route-private-field PRIVATE_FIELD \
     --route-public-field PUBLIC_FIELD \
     --fallback-secret FALLBACK_SECRET \
     --fallback-field TOKEN_FIELD \
     --receipt /absolute/private/path/agent-email-secret-receipt.json
   ```

   The command preflights both exact existing Workers and every dark boundary,
   validates both complete Witself reveal envelopes, proves that the PKCS#8
   Ed25519 private key derives the configured public key, validates the shared
   token, and feeds values only through Wrangler stdin with logs and metrics
   disabled. It uploads the route private key to the control plane and one
   exact fallback token to both Workers, then re-inspects both active versions
   and secret-name inventories before creating a mode-`0600` receipt. It never
   prints a secret value. A failure during the sequential token updates is safe
   while all delivery gates remain dark; correct the cause and rerun the same
   ceremony.

   Secret updates create successor Worker versions without the reviewed release
   annotations. Therefore deploy both Workers from the same unchanged tag only
   after the ceremony, edge first and then control plane, and run coordinated
   readiness last:

   ```sh
   npm run deploy
   (
     cd ../control-plane
     npm run deploy
   )
   npm run verify:route-signing-readiness
   ```

   Do not use the older independent `secret:put` commands for this coordinated
   setup. They cannot prove that the two fallback values match. The ceremony
   also requires an existing email-edge Worker with its relay secret already
   bound. A first-ever Worker bootstrap must instead use a separately reviewed
   one-shot `--secrets-file` deployment; never create a partial Worker and
   never place that file in the repository.

3. Enable the matching cell configuration with only the public key, deploy the
   cell, and confirm its startup reconciliation and health checks.

4. Copy `pilot.example.json` outside the repository, replace every example
   value with the reviewed one-realm/5–10-agent enrollment, and prepare disabled
   literal routes:

   ```sh
   npm run routes -- prepare /absolute/path/to/pilot.json
   npm run routes -- status /absolute/path/to/pilot.json
   ```

5. Wait for directory propagation, review Cloudflare Email Routing and the
   unchanged catch-all, then activate and immediately recheck status:

   ```sh
   npm run routes -- activate /absolute/path/to/pilot.json
   npm run routes -- status /absolute/path/to/pilot.json
   ```

   Route activation is also eventually consistent. After the first active
   status, wait at least 60 seconds, run `status` again, and confirm the exact
   canary rule is still Active in Cloudflare before sending. An immediate
   message can otherwise miss the new literal rule and follow the unchanged
   catch-all.

6. Send one synthetic message to the exact canary address. Confirm a committed
   mailbox row through the owner-only API before allowing expected low-risk
   verification-code workflows.

7. Confirm the value-free edge outcome and route-lookup streams. The Worker
   writes one best-effort Analytics Engine point per final SMTP-facing outcome
   under `witself.agent-email.edge.v1`. It also writes route observations under
   `witself.agent-email.route-lookup.v1`, using only `result`, `evidence`, and
   `route_kind` closed enums plus count, latency, and numeric response status.
   Route results are `kv_fresh`, `legacy`, `cp_found`, `cp_not_found`,
   `miss_suppressed`, `cold_limited`, `known_limited`, `kv_error`, or
   `cp_error`; evidence is `none`, `known`, or `uncertain`; and route kind is
   `canonical`, `alias`, `custom_domain`, `pilot`, or `unknown`. Metrics failure
   never changes message disposition. Each recipient lookup emits exactly one
   terminal route event; for a failed or corrupt KV read that continues to the
   control plane, `evidence=uncertain` preserves the context without emitting
   a second early `kv_error` event. Neither schema contains an address, domain, realm
   label, account, realm, agent, sender, subject, message id, digest, signature,
   limiter key, or content-derived value. A complete renderer-validated release
   version, commit, and commit-date triple is appended only as metadata blobs,
   never as the low-cardinality sampling index or a summary group dimension;
   malformed or incomplete attribution is omitted. Query both the final-outcome
   and route-lookup streams for the last hour with a token carrying
   `Account Analytics Read`:

   ```sh
   npm run metrics -- summary 60
   ```

   The additive v2 output keeps final outcomes in `result` and adds
   `route_lookup_result`, grouped only by the three fixed route enums. This
   exposes combinations such as `result=cp_error` and
   `route_kind=custom_domain` without exposing the domain. `accepted`,
   permanent-rejection, and tempfail outcomes must all be visible during
   acceptance and rollback drills. Built-in Worker invocation metrics remain
   the independent signal for runtime exceptions and resource failures.

## Continuous canary

`npm run canary` first arms one owner-generated opaque UUID through the
owner-only retry-canary endpoint, then sends the synthetic message through
Cloudflare Email Sending with `X-Witself-Canary-Retry`. The cell commits the
first matching attempt as a deliberate temporary result without storing a
message; a provider retry with the same normalized envelope, stable parsed
projection, and exact MIME body is accepted once even when Cloudflare adds or
changes transport/authentication headers. Parse-invalid attempts still require
an exact raw-body/envelope retry. The runner requires
the cumulative tempfail-then-accepted checkpoint before it passively scans the
owner mailbox newest-first through bounded cursor pages, verifies the exact
subject and parsed synthetic code, claims, marks the code used, completes, and
acknowledges the message. A separate correlation nonce identifies the subject;
the retry challenge appears only in its dedicated MIME header, never the
subject or body. Its output is value-free and includes only
`provider_retry_proven:true`: no code, message content, challenge/message
identifier, address, or token is returned. A post-claim failure releases the
exact fence when possible; otherwise the bounded lease expires normally. A
retained tempfailed proof remains retryable for 24 hours but does not block the
next run from arming a fresh challenge.

The `agent-email-canary` GitHub Actions workflow is manual-dispatch-only.
Provision these repository variables:

- `CLOUDFLARE_ACCOUNT_ID`
- `WITSELF_EMAIL_CANARY_ENDPOINT`

Create the protected-main-only GitHub Environment named `agent-email-canary`
and place these environment secrets there (not repository variables or
repository-wide secrets):

- `AGENT_EMAIL_CANARY_CLOUDFLARE_TOKEN` (`Email Sending: Edit` only)
- `WITSELF_EMAIL_CANARY_TOKEN` (the dedicated enrolled canary agent only)
- `AGENT_EMAIL_CANARY_FROM`
- `AGENT_EMAIL_CANARY_TO`

Run one manual workflow dispatch and review both the value-free canary result
and Analytics Engine outcomes. Add a recurring schedule only when continuous
monitoring and its retained-message growth are intentionally accepted. The
Cloudflare sender must already belong to an onboarded Email Sending domain.
The job has a 15-minute outer limit and a 600-second absolute canary deadline.

Do not arm or send during a mixed-version deployment. Deploy schema-61-capable
server code with `WITSELF_AGENT_EMAIL_RETRY_CANARY_AGENT_ID` empty, wait for
every pod to converge, then perform a config-only rollout selecting exactly one
enrolled agent and wait for every pod again. Only then run the manual canary.
For rollback, first disable any recurring schedule that has been added, then
settle the unused arm or let its 15-minute TTL expire before unsetting the
canary agent or deploying older code; otherwise an old replica can
ordinary-accept the first synthetic delivery.

Acknowledgement does not delete synthetic messages. A future 15-minute schedule
would add about 96 retained messages per day until the ordinary mailbox
retention/delete contract is implemented. Keep the workflow manual-only unless
that accumulation is explicitly accepted and monitored.

## Raw-MIME storage probe

The separate `agent-email-storage-probe` workflow is manual-dispatch-only and
uses the same protected `agent-email-canary` GitHub Environment. Pin its one
exact disposable `@agent-mail.witwave.ai` recipient in the separate
`AGENT_EMAIL_STORAGE_CANARY_TO` Environment secret; do not reuse or overwrite
`AGENT_EMAIL_CANARY_TO`. The workflow has no dispatch inputs. The runner creates
one bounded multipart message with a fixed synthetic attachment and submits it
through Cloudflare's raw-MIME API. The sender, recipient, and Email Sending
token remain Environment secrets; the result contains only the exact synthetic
subject, byte counts, and booleans proving that no token, address, MIME, or
provider disposition was returned.

A successful workflow proves only that the Cloudflare Email Sending API
accepted the submission request. It does not prove eventual delivery,
retention, capacity omission, or permanent rejection. `send_raw` can return
before the Email Routing Worker later calls `setReject`, so the probe
deliberately neither interprets nor returns the API's immediate `delivered`,
`queued`, `permanent_bounces`, or message-id fields.

Converge the intended account policy before each dispatch. Use sufficient
attachment capacity for the retained case. For capacity omission, keep the
raw-message maximum above the probe size but leave less attachment capacity
than the complete attachment-bearing MIME requires. For raw-message rejection,
set the effective raw-message maximum below the prior probe's reported
`raw_bytes`. Do not change policy while a submission is still in flight.

Run only one storage probe at a time. Capture its exact synthetic subject from
the value-free `witself.agent-email.storage-probe.v2` result with
`outcome='submitted'`, then combine two independent observations:

- Query the live cell by that exact subject. Retained storage requires exactly
  one parsed row with `payload_retention_state='retained'`, non-null
  `raw_mime`, and retained bytes equal to the full received raw-message size.
  Capacity omission requires exactly one parsed row with
  `payload_retention_state='omitted_capacity'`, null `raw_mime`, and zero
  retained bytes. A raw-message-limit rejection requires no row for that exact
  subject.
- Query a narrow Analytics Engine window with
  `npm run metrics -- summary [minutes]`. Retained and capacity-omitted
  submissions require the Worker's final `accepted` outcome. A converged
  raw-message limit below the probe's reported `raw_bytes` requires
  `rejected_over_size` in the `response` phase. Edge metrics intentionally
  contain no subject or other correlation identifier, so serialize probes,
  compare the narrow-window counts with a baseline, and use the exact-subject
  database check to distinguish the storage result.

Do not infer the result from the workflow's `submitted` receipt. Analytics
Engine writes are best-effort; if the expected point is absent, inspect the
Worker's built-in invocation metrics and repeat only after the first submission
has been conclusively accounted for. The workflow never prepares routes,
changes cell policy, or deletes mail.

## Rollback

The email-edge Worker has a separate, guarded code-version rollback. First
create a new plan for one exact older Cloudflare Worker version in a private,
previously nonexistent file:

```sh
npm run rollback -- \
  --candidate-version 01234567-89ab-4cde-8f01-23456789abcd \
  --output /absolute/private/path/agent-email-rollback.json
```

The planner requires the candidate to have the same email-only handlers,
runtime, complete binding inventory, shared KV, metrics dataset, rate
limiters, managed-delivery flags, route-verification keyring, and secret
binding names as the active version. It creates the file with mode `0600` and
refuses to overwrite an existing path. Review the whole plan, its exact active
and candidate version ids, and its invariant-contract digest. Then copy the
plan's `apply_fence.sha256` into the explicit apply command:

```sh
npm run rollback -- \
  --apply \
  --plan /absolute/private/path/agent-email-rollback.json \
  --plan-sha256 0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef
```

Apply requires an interactive terminal. Cloudflare Worker versions include
secret state, and Wrangler may warn that the selected version restores older
secret values. Stop and investigate unexpected differences; the tool never
auto-accepts that warning. It rechecks the complete plan fence immediately
before mutation and verifies the chosen version at 100 percent afterward.

The first signed-route release has no compatible rollback candidate: the
current pre-signing version lacks the signature/keyring contract and is
deliberately rejected by the planner. Keep every delivery gate dark and use a
forward fix until a later older signed release with the exact same operational
contract exists.

Literal-pilot route rollback is independent of Worker code rollback. Disable
the reviewed rules first; this preserves the exact rules and directory rows
for inspection:

```sh
npm run routes -- disable /absolute/path/to/pilot.json
npm run routes -- status /absolute/path/to/pilot.json
```

After the incident record and state review, `remove` deletes only the pilot's
managed literal rules and isolated directory rows. It does not modify the
catch-all:

```sh
npm run routes -- remove /absolute/path/to/pilot.json
```

Do not use `remove` as an automatic failure response; retaining disabled state
usually gives better forensic evidence.
