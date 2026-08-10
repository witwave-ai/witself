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

All canonical and managed-alias traffic has a second, account-scoped fence:
`AGENT_EMAIL_MANAGED_DELIVERY_ACCOUNT_ALLOWLIST`. It is an exact sorted CSV of
at most 100 generated `acc_[a-z2-7]{16}` IDs (2,099 bytes maximum); the empty
committed value admits nobody, and whitespace, duplicates, unsorted input, and
wildcard-like values are rejected.
The control plane must use the byte-identical
`CP_AGENT_EMAIL_MANAGED_DELIVERY_ACCOUNT_ALLOWLIST`. Its signed managed route
projection carries `account_id`; the edge checks that authority before an
inactive-route bounce, content read, or cell request. A held-back known route
therefore produces only `tempfail_account_cohort`. The metric is a fixed enum
and contains no account, realm, address, or message value. Custom-domain routes
remain independent of this managed-domain cohort.

Managed canonical and alias payloads are schema v2 and signed envelopes are
schema v3. Custom-domain payloads remain v1/signed-v2. The edge dual-reads a
v240 managed v1/signed-v2 row only to preserve prior-route evidence, always
refreshes it from the control plane, and never delivers from it. An old v240
edge can still deliver from a fresh legacy signed-v2 KV row without consulting
the upgraded control plane when its old managed-delivery gate is true. The v241
release uses control-plane-first code deployment. Before that upgrade, prove
both v240 canonical and alias delivery gates are false and keep the new
control-plane cohort empty. The v241 edge deploy refuses to run until the active
control plane is already v241 or newer. Keep both route-kind gates false until
both Workers are upgraded and the separately reviewed cohort activation begins.

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

- Keep the existing Email Routing catch-all unchanged while operating either
  literal-canary lifecycle. Only the separately fenced `routes:catch-all`
  workflow below owns a catch-all mutation method.
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

## Production `witmail.net` routing controls

The original `npm run routes` command is the retired compatibility-pilot
manager. It writes unsigned `pilot:config:v1` and `pilot:recipient:v1` rows
that the primary-domain runtime deliberately ignores. Never use it to stage
`witmail.net`.

`npm run routes:primary` owns the production primary-domain canary. Its private
manifest has this exact shape, with 5–10 entries; every address must be the
canonical `<agent-segment>.<realm-id-body>@witmail.net` for its `realm_id`:

```json
{
  "schema_version": 2,
  "domain": "witmail.net",
  "worker_name": "witself-agent-email-pilot",
  "account_ids": ["acc_abcdefghijkl2345"],
  "agents": [
    {
      "agent_id": "agent_aaaaaaaaaaaaaaa2",
      "realm_id": "realm_abcdefghijkl2345",
      "address": "alpha.abcdefghijkl2345@witmail.net"
    },
    {
      "agent_id": "agent_aaaaaaaaaaaaaaa3",
      "realm_id": "realm_abcdefghijkl2345",
      "address": "bravo.abcdefghijkl2345@witmail.net"
    },
    {
      "agent_id": "agent_aaaaaaaaaaaaaaa4",
      "realm_id": "realm_abcdefghijkl2345",
      "address": "charlie.abcdefghijkl2345@witmail.net"
    },
    {
      "agent_id": "agent_aaaaaaaaaaaaaaa5",
      "realm_id": "realm_abcdefghijkl2345",
      "address": "delta.abcdefghijkl2345@witmail.net"
    },
    {
      "agent_id": "agent_aaaaaaaaaaaaaaa6",
      "realm_id": "realm_abcdefghijkl2345",
      "address": "echo.abcdefghijkl2345@witmail.net"
    }
  ]
}
```

Do not hand-author the real addresses. After the released cell's explicit
mailbox backfill reports zero missing rows, generate them from actual enabled
primary mailbox state with the cell's production receive environment:

```sh
/usr/local/bin/witself-server agent-email canary-manifest \
  --output /absolute/private/primary-canary.json
```

The path must be new. The exporter writes the exact sorted manifest with the
complete 1-100 account receive cohort plus 5-10 actual canary mailboxes. The
cohort is independent of which accounts happen to supply those few literal
addresses, so a multi-account cohort is not accidentally constrained by the
canary size. The file is created with exclusive mode `0600`; the command emits
no identities or addresses to stdout and performs no database write. Keep that
manifest and every plan outside the repository.
After deploying the exact managed-account cohort and allowing signed inventory
to converge, `status` must report `ready_for_prepare: true`. The `prepare`,
`activate`, `disable`, and `remove` commands only create a new mode-`0600`,
15-minute review plan. Only `apply` mutates rules, and it must receive the exact
SHA-256 printed by the planning command:

```sh
npm run routes:primary -- status /absolute/private/primary-canary.json
npm run routes:primary -- prepare /absolute/private/primary-canary.json \
  --output /absolute/private/prepare-plan.json
npm run routes:primary -- apply \
  --plan /absolute/private/prepare-plan.json \
  --plan-sha256 REVIEWED_SHA256 \
  --receipt-output /absolute/private/prepare-receipt.json
```

The primary manager creates, enables, disables, or removes only exact literal
rules named `witself-agent-email-primary-canary:<address>`. It never writes KV
and its API class has no catch-all mutation. Before prepare or activation it
proves all of the following from live state:

- Email Routing is ready, subaddressing is enabled, the catch-all is disabled,
  and enabled forwarding rules exist for both `postmaster@witmail.net` and
  `abuse@witmail.net`;
- the active control-plane and edge releases, shared route-directory binding,
  signer/keyring, canonical gates, and exact managed-delivery cohort agree;
- the authenticated control-plane readiness digest matches both deployed
  cohort bindings and reports canonical inventory plus delivery enabled;
- each distinct canary realm has byte-identical control-plane and KV signed
  projection bytes, a valid trusted signature, fresh applied canonical state,
  and an `account_id` admitted by that exact cohort.

Every mutation re-reads its entire reviewed precondition. It fingerprints the
catch-all, role routes, and managed canary rules before and after. Stale plans,
unmanaged conflicts, an incomplete canary, or concurrent guard drift fail
closed. A failed activation disables every owned canary rule; partial removal
leaves survivors disabled. Emergency `disable` and cleanup `remove` remain
available through their own reviewed plans even when projections or delivery
gates are unavailable. Primary apply reserves and fsyncs its mandatory
mode-`0600` receipt path before any Cloudflare mutation, then atomically replaces
the pending marker with exact before/after rule evidence after verification.

`npm run routes:catch-all` is a separate, higher-risk workflow. It defaults to
read-only `status`; `enable`, `disable`, and `rollback` also create plans only.
An enable plan additionally requires a change identifier and the SHA-256 of a
separately reviewed provider-contract record, plus the fully active primary
canary and all readiness proofs above:

```sh
npm run routes:catch-all -- enable /absolute/private/primary-canary.json \
  --change-id REVIEW_ID \
  --provider-review-sha256 REVIEW_RECORD_SHA256 \
  --output /absolute/private/catch-all-enable-plan.json
npm run routes:catch-all -- apply \
  --plan /absolute/private/catch-all-enable-plan.json \
  --plan-sha256 REVIEWED_SHA256 \
  --receipt-output /absolute/private/catch-all-enable-receipt.json \
  --confirm-enable-witmail-net
```

The apply command exclusively creates and fsyncs the protected receipt path
with a pending marker before it calls Cloudflare, so an existing or unwritable
path is discovered before mutation. A crash may leave that marker; it is not a
rollback receipt. Inspect live status and use the independently planned disable
path before removing it. A completed receipt contains the exact predecessor
needed to plan rollback.
A rollback receipt is accepted only from a verified enable and may restore
only a disabled predecessor; a disable receipt can never be used to re-enable
mail. Enable verification failure restores the exact disabled predecessor.
Disable and rollback failures never auto-enable anything. The external review
digest is a fence around operator acceptance, not automated proof that the
document is sufficient: do not create an enable plan until the provider
contract blockers in `docs/agent-email.md` are explicitly closed.

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
renderer also requires a valid
`AGENT_EMAIL_MANAGED_DELIVERY_ACCOUNT_ALLOWLIST`; omission renders the safe
empty cohort. The control-plane renderer uses its `CP_`-prefixed counterpart.
The runtime remains default-off if either binding is ever absent. The renderer
refuses the KV ID bound to
the adjacent control-plane Worker. The generated file is local operator state
and must not be committed. The generated Worker must expose
`REALM_ROUTE_COLD_MISS_LIMITER` at 10 calls per 10 seconds and
`REALM_ROUTE_KNOWN_MISS_LIMITER` at 100 calls per 10 seconds; the committed
template owns their distinct namespace IDs. `CONTROL_PLANE_EDGE_TOKEN` is
deliberately absent from both the template and generated file. It must instead
be present only in the local operator environment for deployment and routing
apply commands, which use it to acquire the control plane's global operations
lease. Never print or persist its value.

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

The control-plane deploy, this email-edge deploy, the guarded email-edge
rollback, the coordinated route-signing secret ceremony, and both primary and
catch-all routing apply workflows share one global, expiring operations lease in
the control plane's existing `REALM_EMAIL_ALIASES` Durable Object. Their exact
operation identifiers are `control_plane_deploy`, `email_edge_deploy`,
`email_edge_rollback`, `route_signing_secret_provision`,
`primary_routing_apply`, and `catch_all_routing_apply`. Each workflow renews the
lease while its subprocess is running, performs a final renewal before success,
and releases it afterward. A live conflicting operation fails closed; a crashed
holder can be replaced only after the bounded expiry. Never bypass a lease
conflict or run these operations concurrently. Each supported Worker deploy
renders into its own unpredictable mode-`0700` temporary directory, freezes the
config mode `0400`, verifies its SHA-256 while holding the lease, and removes it
afterward. A concurrent `npm run config` or second deploy therefore cannot swap
the cohort or delivery gates underneath Wrangler. The control-plane deploy and
secret ceremony derive the lease origin from a stable inspection of the exact
active email-edge Worker binding. The edge deploy requires the canonical
`https://self.witwave.ai/` origin directly. All three pin that exact authority;
process environment cannot redirect the lease.

There is one bounded legacy bootstrap exception. A literal acquire 404 may
proceed only for the v0.0.240-to-v0.0.241 transition after stable provider reads
prove the exact Git-tagged v0.0.240 control plane, the missing legacy managed
cohort binding, absent canonical inventory and delivery gates, an empty target
cohort, and a dark v0.0.240 email edge. The independent alias-administration
gate may remain active; it does not enable delivery. The sole unleased write
uses `wrangler deploy --containers-rollout none` to install the exact outer
v0.0.241 Worker without building or updating Containers. The deploy then proves
the target release and every Durable Object namespace are unchanged, acquires
the newly installed lease, and performs the full Container deploy and endpoint
verification under that lease. A current or newer control plane returning 404
never qualifies. The edge deploy and provider-routing workflows have no
bootstrap bypass.

Cloudflare exposes no compare-and-swap fence around the first outer-Worker
upload. Simultaneous supported bootstraps therefore converge by uploading the
same clean tagged source, generated config, annotations, and outer-only
arguments; the first successful endpoint then serializes the full deployment.
This makes the supported first write byte-identical and idempotent, but it
cannot serialize an unrelated dashboard or API write.

This lease covers the supported Witself tools, not unrelated writes made in the
Cloudflare dashboard or through another API client. Freeze those external
changes during every plan/apply window. If an external write may have occurred,
treat the receipt as suspect, inspect live state, and take the separately
planned fail-closed disable path before proceeding.

After both tagged Workers are deployed, run
`npm run verify:route-signing-readiness`. This read-only check follows each
production deployment to its one exact 100-percent version and refuses unless
the control plane and email edge have the same immutable release identity,
both canonical controls and every custom-domain delivery control remain dark,
both edge managed-delivery flags are false, the control-plane signer id
is present in the edge's canonical public-key keyring, and all route-signing,
authenticated-fallback, and relay secret bindings exist. Its JSON attestation
contains key ids and binding-presence booleans, never public-key bytes or secret
values. It requires the release tag/message annotations from `npm run deploy`.
A default readiness check also requires both canonical cohort strings to match
and be empty, and emits only their zero count and SHA-256 digest. To attest a
reviewed nonempty cohort before provider activation, keep both delivery gates
false and supply the exact expected bytes explicitly:

```sh
npm run verify:route-signing-readiness -- \
  --expected-managed-delivery-cohort acc_aaaaaaaaaaaaaaaa
```

This mode requires CP, edge, and the argument to match byte-for-byte and emits
only count and digest; it does not relax any dark gate check. A later
secret-only successor must be followed by a tagged redeploy before this
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
- `CP_AGENT_EMAIL_MANAGED_DELIVERY_ACCOUNT_ALLOWLIST` (when rendering the
  control plane)
- `AGENT_EMAIL_MANAGED_DELIVERY_ACCOUNT_ALLOWLIST`
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
   disabled. The revealed fallback token authenticates the exact
   `route_signing_secret_provision` lease directly; it is never inherited by a
   Wrangler child. After acquiring the lease, the command reacquires every live
   Worker, dark-gate, empty-cohort, and canonical-origin fence. It renews
   immediately before and after each bounded secret write, uploads the route
   private key to the control plane and one exact fallback token to both Workers,
   then re-inspects both active versions and secret-name inventories and renews
   again before creating the value-free v2 mode-`0600` receipt. It never prints
   a secret value. A failure during the sequential token updates is safe while
   all delivery gates remain dark; correct the cause and rerun the same ceremony.

   Secret updates create successor Worker versions without the reviewed release
   annotations. Therefore deploy both Workers from the same unchanged tag only
   after the ceremony, control plane first and then edge, and run coordinated
   readiness last:

   ```sh
   (
     cd ../control-plane
     npm run deploy
   )
   npm run deploy
   npm run verify:route-signing-readiness
   ```

   Independent edge secret-put npm commands are intentionally not exposed; npm
   treats those old names as unknown commands because they cannot prove that the
   two fallback values match or participate in the global lease. The ceremony
   also requires an existing email-edge Worker with its relay secret already
   bound and requires the desired fallback token to already authenticate the
   live control-plane lease. A first-ever Worker bootstrap must instead use a
   separately reviewed one-shot `--secrets-file` deployment; never create a
   partial Worker and never place that file in the repository.

   This ceremony cannot rotate `CONTROL_PLANE_EDGE_TOKEN`: replacing the
   credential that authenticates acquire, renew, and release would invalidate
   its own live fence. Token rotation uses only the control-plane package's
   explicit `secret:put:break-glass` path under a documented global
   provider-mutation freeze, followed by exact convergence and verification.

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

The planner normally requires the candidate to have the same email-only
handlers, runtime, complete binding inventory, shared KV, metrics dataset, rate
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
auto-accepts that warning. After checking the submitted plan hash, apply acquires
the shared operations lease as `email_edge_rollback`, reconstructs and rechecks
the complete plan fence while holding that lease, and renews immediately before
the provider write. Lease loss terminates the active Wrangler subprocess. The
tool renews again around readback, verifies the chosen version at 100 percent,
proves one final renewal, and releases the exact lease afterward. A lease
conflict fails before the final provider reads or mutation; rollback has no
bootstrap bypass. Because this rollback tool targets the production
`witmail.net` Worker, it pins lease acquisition to
`https://self.witwave.ai`; inherited `WITSELF_CONTROL_PLANE` and
`CONTROL_PLANE_URL` values cannot redirect its fencing authority. The lease
client receives only `CONTROL_PLANE_EDGE_TOKEN` from the operator environment
and refuses HTTP redirects.

The pre-signing release remains incompatible. v0.0.240 is a narrower exception:
it already has the signed-route/keyring contract but predates the account cohort
binding. The planner may normalize that one absent binding to empty only when
the current cohort is empty and both the current and v0.0.240 alias/canonical
delivery gates are false. This is a dark emergency rollback, not an active
managed-delivery rollback. v0.0.240 cannot read the new signed-v3 managed rows;
leave managed delivery dark until compatible Workers are restored. Its
custom-domain v1/signed-v2 route behavior is unchanged.

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
