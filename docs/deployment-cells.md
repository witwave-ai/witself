# Witself Deployment Cells & Multi-Cloud

Status: draft. This document captures the go-forward deployment topology for both
managed Witself Cloud and self-hosted Witself: a fleet of independent cells under a
thin global control plane. Decided 2026-06-28.

Narrative-memory amendment (accepted 2026-07-14): cells have no backend memory
inference provider. Account movement uses source freeze or placement-epoch
fencing, clears imported leases, and rebuilds derived indexes under
[narrative-memory-and-curation.md](narrative-memory-and-curation.md).

> **Sealed-plane custody amendment (accepted 2026-07-18):**
> [ADR 0003](decisions/0003-client-custodied-agent-vault.md) and the
> [client-custodied vault plan](client-custodied-agent-vault.md) supersede the
> cell-rooted KMS design below. Cells store ciphertext, wrapped per-field DEKs,
> and public AVK metadata in PostgreSQL; they hold no agent AVK and expose no
> server-decrypt path. Moving an account copies the encrypted vault unchanged
> and requires no source/destination KMS re-wrap. An authorized client must
> separately possess the matching AVK. References below to a sealed-plane KMS,
> cloud-rooted secret key material, or cross-cloud KMS re-wrap are retained only
> as superseded design history.

## Decision

Witself deploys as a fleet of independent cells. A cell is one complete, isolated
Witself stack. A single thin, globally-replicated control plane holds only routing
metadata and decides which cell a tenant lands on and where to route. The control
plane holds no tenant data.

This topology is the same for managed Witself Cloud and for self-host: the same
[backend-architecture.md](backend-architecture.md) backend code runs in each cell;
the difference is how many cells exist and who operates them.

## Cell

A cell is one complete, independent Witself stack in a single cloud account/region:

- `witself-server` (see [backend-architecture.md](backend-architecture.md))
- PostgreSQL for the open plane (memories, facts, messaging, and optional
  migration-0032 JSONB vectors) — see
  [storage.md](storage.md)
- KMS for the sealed plane (secrets), rooted in that cell's cloud — see
  [storage.md](storage.md) and [key-hierarchy.md](key-hierarchy.md)
- Blob/object storage for attachments

Examples of distinct cells:

- AWS account #1, `us-east-1`
- AWS account #2, `us-east-1` — an independent second AWS account is simply another cell
- A GCP project, `europe-west1`
- An Azure subscription, `westus2`
- A self-host operator's single in-cluster stack

Cells are isolated. A cell holds the full data and key material for its own tenants
and depends on nothing in another cell to serve them. A cell outage affects only the
tenants homed on that cell — blast-radius containment. There is no shared data store
spanning cells.

Each cloud's cell is provisioned by the executable Pulumi program under
[`infra/pulumi`](../infra/pulumi), as tracked in
[cloud-targets.md](cloud-targets.md); a cell is one instantiation of a stack.

## Control plane

The control plane is the one new always-on global component. It is thin, HA, and
globally replicated. It holds only routing metadata:

```json
{
  "realm": "acme-prod",
  "account": "acct_8f3a",
  "home_cell": "aws-use1-01",
  "endpoint": "https://aws-use1-01.cells.witself.cloud",
  "signing_key": "<realm signing public key / JWKS ref>"
}
```

It does two things:

- **Placement** — picks the home cell for a new tenant.
- **Resolution** — answers "where is this realm/account, and how do I reach it?"

It holds no memories, facts, secrets, or messages. Keeping it thin keeps its blast
radius tiny: if the control plane degrades, existing clients that have already
resolved their home cell keep working against that cell directly; only fresh
placement and first-time resolution are affected.

The control plane extends the existing `--endpoint` / token model. Today a client
points at an endpoint; the go-forward client resolves its home cell from the control
plane (and may cache it), then talks directly to that cell. Tokens remain
cell-scoped and are validated by the home cell, not the control plane.

```text
witself --endpoint https://api.witself.cloud login   # control plane resolves home cell
# subsequent calls go directly to the resolved home-cell endpoint
```

## Placement and landing

At account/realm creation, the control plane picks a cell by:

- region / data-residency requirement
- capacity and load across the fleet
- provider preference (AWS / GCP / Azure, or a specific account)
- rollout wave (see versioning below)

It records the realm/account -> cell mapping. Clients then resolve their home cell on
login or first call and route directly. Placement emits `tenant.placed` — registered
in the audit-event registry alongside `tenant.migration_started` /
`tenant.migration_completed` / `tenant.migration_failed` (see
[audit-retention.md](audit-retention.md)).

## Multi-cloud

The fleet spans AWS, GCP, Azure, and Civo, across multiple accounts per cloud. Each cell is
one cloud account/region. An independent second AWS account is not a special case — it
is simply another cell. The fleet reuses the AWS, GCP, Azure, and Civo paths in the
Pulumi cell program described by [cloud-targets.md](cloud-targets.md); adding a
cloud account or project means standing up another stack and registering its
cell with the control plane.

## Cells at different versions

Cells may run different software versions at the same time. This is a strength of the
cell model, not a problem:

- **Canary / wave rollout** — a new release lands on one cell (or a wave of cells)
  first; placement can steer new tenants toward or away from a wave.
- **Capability discovery** — clients discover a cell's capabilities/version and adapt,
  rather than assuming a single global version. The same discovery mechanism the
  collaboration substrate uses for realm/agent cards (see
  [agent-collaboration.md](agent-collaboration.md)) covers cell capability advertisement.

Because cells are isolated, a bad release is contained to the cells it reached.

## Retired literal-route compatibility receive mode

The original bounded receive mode is retained only for compatibility with the
legacy literal-route deployment. It is not the production service and is
disabled unless every one of these server settings is present and valid:

- `WITSELF_AGENT_EMAIL_RECEIVE_PILOT_ENABLED=true`
- `WITSELF_AGENT_EMAIL_PILOT_DOMAIN` — the primary lowercase managed domain
- `WITSELF_AGENT_EMAIL_ACCEPTED_LEGACY_DOMAINS` — optional compatibility domain
  (the runtime accepts comma-separated syntax, but managed deployments cap the
  list at one); it accepts only canonical local parts issued before cutover and
  can never mint a new address or alias
- `WITSELF_AGENT_EMAIL_PILOT_AUDIENCE` — the exact destination-cell audience
- `WITSELF_AGENT_EMAIL_PILOT_REALM_ID` — the one enrolled realm
- `WITSELF_AGENT_EMAIL_PILOT_AGENT_IDS` — a comma-separated set of 5–10 enrolled
  agent IDs
- `WITSELF_AGENT_EMAIL_RELAY_PUBLIC_KEYS_JSON` — a JSON object mapping relay key
  IDs to standard-base64 raw Ed25519 public keys
- `WITSELF_AGENT_EMAIL_RELAY_REPLAY_WINDOW` — optional; defaults to `5m` and may
  not exceed `15m`

When `WITSELF_AGENT_EMAIL_RECEIVE_PILOT_ENABLED` is absent or parses as false,
the process leaves the compatibility mode disabled and ignores the other legacy
variables. When it is true, the primary domain and every other required
compatibility value must be present.
`WITSELF_AGENT_EMAIL_ACCEPTED_LEGACY_DOMAINS` may be absent/blank or
contain exactly one canonical lowercase ASCII domain. Although its wire syntax
is comma-separated, a second entry is rejected. Surrounding whitespace is
trimmed, but the resulting primary and legacy values must already be canonical
lowercase ASCII without a DNS root dot, and the legacy domain cannot equal the
primary. Any invalid enabled configuration fails startup before serving.

The private Ed25519 relay key is a secret of the isolated Cloudflare Email
Worker and must never be placed in cell configuration. On startup, an enabled
cell reconciles the one realm and exact agent allowlist into durable mailboxes
and their configured primary route. For an existing mailbox, it also preserves
a different original route only when that legacy address was already issued.
Configuring the legacy domain alone never issues it to a mailbox created after
cutover.
Managed canonical and realm-alias ingress must match both a currently
configured primary/legacy domain and a permanent
`agent_email_address_domains` reservation. A historical reservation remains
non-reusable but is not accepted after its domain leaves the runtime
configuration. The schema-88 custom-domain path never adds a
customer domain to that configuration: the signed envelope recipient must
instead resolve through an exact local `agent_email_custom_domain_routes` row
bound to an existing realm-alias claim. With all custom routing and delivery
gates absent, no such live ingress is reachable. Startup fails before serving
when the realm or an agent is
missing or inactive, an agent belongs to another realm, a route collides, or an
existing mailbox/address has inconsistent ownership. Realm-email aliases stay
on their explicit primary domain and are never fanned out to the legacy domain.

`witmail.net` is a dedicated agent-email service domain. Do not place a website,
`A`, `AAAA`, or application `CNAME` on it; do not use it for marketing,
employee mailboxes, or platform-notification sending. The required
`postmaster@witmail.net` and `abuse@witmail.net` operator routes are the narrow
operational exception. Production outbound agent email uses separately
isolated sending
infrastructure on `send.witmail.net`; it remains agent-email-only and does not
turn either domain into a general company-mail surface.

The retired edge implementation uses the historical
`witself-agent-email-pilot` Worker and an isolated
`witself-agent-email-pilot-directory` KV namespace. It must never bind the
control-plane `DIRECTORY` namespace. Provider-side route management is limited
to literal rules for the 5–10 enrolled addresses; the Worker can read isolated
KV recipient projections after a provider route has delivered mail to it, but
that directory cannot create provider coverage. Infrastructure reads and
fingerprints the existing catch-all but has no operation that can update it.

Historically, an operator activated this compatibility edge only after the cell
release and configuration were healthy, the disabled exact-route set had been
reviewed, and KV propagation had settled. A synthetic exact-address canary then
proved Worker-to-cell commit before any expected compatibility mail was sent.
Rollback disabled only the compatibility mode's directory gate and literal
routes. See [Agent Email](agent-email.md) and
the [edge README](../infra/cloudflare/agent-email/README.md) for the staged
procedure. A configured cell, deployed Worker, or enabled routing rule alone is
not proof of end-to-end operation.

Outbound provider dispatch lives separately in
`infra/cloudflare/agent-email-send/`. Its Email Sending binding, Durable Object
receipts/routes, lifecycle Queue, exact signer/account cohort, and cell event
targets are not part of the inbound edge deployment. Committed templates and a
fresh cell keep it dark by default and must follow the staged procedure in the
[sending-adapter README](../infra/cloudflare/agent-email-send/README.md) only
after the schema-compatible cell release has been published. An outbound plan
entitlement, cell worker, adapter dispatch gate, event-delivery gate, or Queue
subscription never implicitly enables any other layer.

Current production state is narrower than the catalog and intentionally differs
from those defaults. The multi-account `civo-sandbox-usw2-dev` cell runs two
worker replicas; receive, adapter dispatch, lifecycle delivery, and the
`email.sending` subscription are enabled only for the exact Founder email
cohort, while receipt replay remains off. Agent-email retention is cell-wide and
active on application `0.0.252` at schema 90: enforce mode, batch 100, a
one-minute interval, and a two-minute timeout. Activation followed verification
of the release artifact and both pre-migration backups; Founder's effective
retention remains indefinite.
Every other cell/account remains subject to its own explicit activation. Treat
the README procedure as the repeatable contract for a new cell or cohort, not
as evidence that the current Founder deployment is still dark.

## Production account-cohort agent-email receive

Release `0.0.241` adds the production receive mode without widening the retired
compatibility mode implicitly. It is a second default-off gate, mutually
exclusive with the legacy realm/agent compatibility configuration:

The production Cloudflare service is `witself-agent-email-receive`. It reuses
the existing dedicated email-route KV namespace by ID but has its own Worker
deployment, secrets, and version history. The retired Worker is never a
production routing target.

- `WITSELF_AGENT_EMAIL_RECEIVE_PRODUCTION_ENABLED=true`
- `WITSELF_AGENT_EMAIL_RECEIVE_DOMAIN=witmail.net`
- `WITSELF_AGENT_EMAIL_RECEIVE_AUDIENCE` set to the exact destination cell
- `WITSELF_AGENT_EMAIL_RECEIVE_ACCOUNT_IDS` sourced in managed cells from the
  referenced Kubernetes Secret as a canonical, byte-sorted CSV of 1-100 unique
  generated `acc_*` IDs (portable/private chart users may use the literal list)
- the existing relay public-key, replay-window, and optional legacy-domain
  settings; managed chart/image `v0.0.245` or newer may source the optional
  retry canary from a distinct immutable, versioned Secret

Whitespace, duplicates, wildcard-like values, unsorted input, invalid generated
IDs, or more than 100 accounts fail before the API listens. The app-of-apps
passes this shape only when the server chart and image are both `0.0.241` or
newer. Fleet and portable defaults remain false; no live cell is enabled by the
release itself.

The managed app-of-apps commits only
`accountIDsExistingSecret.name`/`.key`, never the IDs. The referenced Secret
must be immutable, versioned, and present in the server namespace before
activation. Its key is non-optional; missing data prevents pod startup, while
malformed CSV prevents API readiness. In-place mutation is unsupported: create
the next Secret and update the reference name so the Deployment rolls. Managed
cells always keep the literal `retryCanaryAgentID` empty. First converge
`v0.0.245` code and the cohort with
`retryCanaryAgentIDExistingSecret.name` empty. After backfill and a private
canary export, choose one eligible agent and store exactly its canonical
`agent_*` ID, with no whitespace or trailing newline, in a distinct immutable,
versioned Secret. Set the new Secret name and key in a separate config-only
rollout. Both fields participate in the pod checksums; pre-`0.0.245` strict
child schemas never receive the empty field.

Serving replicas do only a bounded read-only check that each configured account
exists in the cell and is active or suspended, plus one optional canary
membership check. They do not scan agents and never provision mailboxes during
startup. This makes 20 API replicas equivalent to one for receive setup. A
Personal account accidentally present in the cohort does not prevent
startup: the local plan entitlement remains authoritative, and an attempted
delivery is accepted and discarded without persisting message content. A plan
transition takes effect from the cell snapshot without reinstalling any client.
Use `scripts/run-agent-email-cell-smoke.sh` and the exact staged procedure in
[runbooks.md](runbooks.md#prove-the-personal-to-professional-receive-boundary-inside-one-cell)
to prove that boundary before provider activation. The harness uses the shared
cell-operation lock, signs only on the operator host, forwards only to
loopback, fences one byte-identical installed agent credential across both
phases, never retries ingest, and leaves plan mutation outside the harness.

Existing mailbox provisioning is an explicit one-shot operator action:

```sh
scripts/run-agent-email-cell-operation.sh \
  --cell CELL --kubeconfig KUBECONFIG --context CONTEXT \
  --operation backfill \
  --artifact-output /absolute/private/backfill-exception.json
```

The supported script copies no values: it snapshots the active non-secret
ConfigMap, reuses only the exact database, cohort, and optional retry-canary
Secret references, and runs the released image in a fixed-name, non-API
Kubernetes Job. The fixed name is the concurrency lock. Its memory-backed
private volume is exported through the same distroless binary directly to a new
mode-`0600` local file outside Git; neither identities nor Secret references are
printed. On a successful backfill the requested exception path remains absent.

If an ungraceful operator-client exit leaves the fixed lock, follow the exact
Job/pod inspection and fixed-resource recovery sequence in
[runbooks.md](runbooks.md#roll-out-production-cell-receive-and-the-v00245-retry-canary). Never
delete the lock while the exact Job is active or one of its pods is Pending or
Running—or in any other nonterminal phase, including `Unknown`—and never use a
broad application selector for cleanup.

The operation first validates the exact cohort,
then processes agents in fixed 100-row keyset pages and verifies zero missing
mailboxes. It is idempotent and safe to rerun after interruption, but it must not
be placed in an API-pod startup command or run concurrently from every replica.
Founder remains bounded in memory even with unlimited agents. Suspended accounts
are checked read-only. Once production mode is active, creating a new cohort
agent and its canonical mailbox is one database transaction, so a successful
agent create needs no restart or later repair.

If a preexisting unrestricted agent name is reserved, becomes empty after
normalization, exceeds the address budget, or collides, the backfill refuses
instead of inventing an address. Supply a reviewed private override manifest:

```json
{
  "schema_version": 1,
  "overrides": [
    {
      "agent_id": "agent_aaaaaaaaaaaaaaaa",
      "agent_segment": "support-agent"
    }
  ]
}
```

The file must be canonical absolute, regular, mode `0600`, at most 64 KiB,
contain 1-1000 strictly sorted unique live-cohort agents, and use canonical
lowercase segments. Rerun the supported script with `--operation backfill`, a
new `--artifact-output`, and `--overrides /absolute/private/overrides.json`.
The mandatory exception output must be a
new canonical absolute path. It is created mode `0600` only when a particular
agent needs intervention and contains the private agent/realm identity, a
bounded reason code, and the number already processed; process logs remain
value-free. Every override is preflighted against the cohort, the complete
override set, live addresses, and permanent route reservations before the first
write. Matching replays are idempotent and typos or a different existing
address fail closed. For a newly created agent, the account operator can pass
`witself agent create --email-agent-segment support-agent ...`; the agent and
explicitly marked operator-override mailbox are committed atomically. The flag
uses the strict v0.0.241-only
`POST /v1/realms/{realm}/agents:with-email-segment` route. A pre-v0.0.241
server returns 404 before mutation, while ordinary no-segment creates continue
to use `POST /v1/realms/{realm}/agents`. Supplied explicit segments are never
trimmed or lowercased: empty, whitespace-only, noncanonical, or reserved values
fail before agent creation. A reserved address returns the stable
`agent_email_address_conflict` 409 and directs the operator to choose a
different `--email-agent-segment` without exposing tenant values.
If ordinary derivation discovers an exceptional name partway through a page,
mailboxes already committed remain valid; add the explicit override and rerun
the idempotent command to converge the remainder.

Do not hand-author the edge canary. After a successful backfill, use one selected
cell process to generate it from actual currently receive-enabled mailbox rows:

```sh
scripts/run-agent-email-cell-operation.sh \
  --cell CELL --kubeconfig KUBECONFIG --context CONTEXT \
  --operation canary-manifest \
  --artifact-output /absolute/private/new/primary-canary.json
```

The output path must be canonical, absolute, and absent. The command performs no
database write, requires zero missing cohort mailboxes, includes the configured
retry canary when present, sorts 5-10 unique entries by canonical address, and
creates the exact edge manifest with mode `0600` and exclusive-create semantics.
It prints no IDs or addresses. Keep the file outside Git and ordinary logs, and
pass it unchanged to `npm run routes:primary -- status ...` before any routing
plan is prepared.

Production ingress still fails closed through every local check: trusted relay
signature and audience, exact account cohort, permanent canonical/alias/custom
route reservation, live account/realm/agent/mailbox state, plan entitlement,
and independent realm/agent receive controls. The process gate changes routing
eligibility only; it does not bypass account policy or enable custom-domain,
alias, canonical-delivery, MX, catch-all, or provider gates.

## Current GitOps Release Rollout

The directories under `.gitops/cells/` are configured desired-state targets;
their presence does not prove that the cell is provisioned, reachable, or
currently reconciled. Confirm the intended rollout set from live Argo and cloud
state before changing a values file.

Release publication and cell deployment are separate operations. First verify
that the tag-triggered release completed and that its version-matched chart
exists. `VERSION` omits the Git tag's `v` prefix:

```sh
VERSION="${RELEASE_VERSION:?set RELEASE_VERSION}"
gh release view "v${VERSION}"
helm show chart oci://ghcr.io/witwave-ai/charts/witself-server \
  --version "$VERSION"
```

Before any release that can advance the database schema, create and verify a
pre-migration backup for the canary cell and record its identifier in the
private rollout record. Managed GCP rollouts must complete the on-demand Cloud
SQL procedure in
[Backup And Recovery](backup-and-recovery.md#gcp-cloud-sql-pre-migration-backup)
before `roll-cell.sh`; a recent scheduled backup is not a substitute.

Then roll one provisioned canary by its exact cell-directory name:

```sh
CELL="${CANARY_CELL:?set CANARY_CELL}"
scripts/roll-cell.sh "$CELL" "$VERSION"
git diff -- ".gitops/cells/${CELL}/values.yaml"
```

The helper changes only `apps.witselfServer.chartVersion` and
`apps.witselfServer.imageTag`, keeping the chart and image on the same released
version. Review and commit the desired canary or wave to `main`; do not edit
unrelated platform chart versions as part of an application rollout. A
bootstrapped cell's Argo applications use automated pruning and self-healing,
so they reconcile the committed values without a separate deployment command.

For each provisioned cell in the wave, verify all of the following before
advancing:

1. The bootstrap, apps, and `witself-server` Argo applications are Healthy and
   Synced.
2. Replacement pods become Ready without sacrificing the required available
   replicas.
3. `GET https://<cell-api-host>/v1/version` reports `${VERSION}` and the tagged
   commit.
4. Server startup logs confirm migration completion. The current server runs
   embedded Goose migrations before serving when a database DSN is configured;
   a migration error exits the process rather than serving the new build.
5. The release-specific API, CLI/MCP, and multi-provider client smoke tests pass.

For the agent-email schema-60/61 rollout, treat old/new writer convergence as
a hard feature barrier. Freeze agent-email receive-control mutations and all
account export/import or cell-move work before Phase A. Deploy the new schema
and application with `retryCanaryAgentID` empty, then verify that every old pod
has drained. A realm disable is not authoritative while a pre-schema-60 pod is
still serving: that binary reads only each mailbox's agent layer. A pre-60
export can also omit the realm-control row and cause a newer importer to
synthesize `enabled`.

Only after full Phase-A convergence may operators change agent/realm receive
controls or resume archive movement. Enable the provider-retry canary in a
separate config-only Phase B, wait for every pod to converge again, and only
then arm/send a manual proof. For rollback, turn off any recurring canary
schedule that has been added and settle any armed proof first. Before removing
the canary setting or deploying pre-60/61 code, disable the process-level
receive mode and the exact edge routes; never rely on a realm-disabled row to
protect traffic from an older binary, and never run a pre-60 export after that
row has become authoritative.

For the schema-87 managed-domain cutover, create the required pre-migration
backup, keep every canonical/alias inventory and delivery gate dark, and freeze
mailbox provisioning, realm-alias projection, account export/import, and cell
movement during mixed-version convergence. Before changing the primary domain,
prove that the legacy domain has no realm-alias request, assignment, or cell
projection. If one exists in a future rollout, resolve and retire it while the
legacy domain is still primary; legacy aliases are intentionally unsupported
after cutover. Migration `0087` backfills each
existing address's original domain into `agent_email_address_domains`. The new
startup reconciler then adds `witmail.net` as primary for an existing
compatibility mailbox and preserves its issued `agent-mail.witwave.ai` route; a mailbox first
created after cutover receives only `witmail.net`. Until both the child chart
and image tag are at least `0.0.232`, the app-of-apps withholds the new
legacy-domain field and passes the issued legacy domain through the old
single-domain contract. Advance both pins before relying on dual-domain
behavior.

A suspended account is verified read-only during startup: reconciliation does
not add its missing primary route while the account is frozen. Resuming the
account changes lifecycle state only. After resume, explicitly restart or run
the normal startup reconciliation on the active account, then verify that the
new primary route was added without changing its address or mailbox IDs.

After every new pod is Ready, verify schema 87, the exact route set and roles,
and that no new legacy-domain alias or post-cutover canonical route was issued
before resuming archive movement. Once any additive route exists, schema 87
intentionally refuses downgrade. Roll back application behavior/configuration
while leaving schema 87 and all permanent route reservations intact; never
delete a route merely to make the down migration pass. Delivery activation is
a later, separately reviewed edge rollout.

The custom-domain routing foundation completed its dark schema-88 cell wave;
the multi-account `civo-sandbox-usw2-dev` cell hosting the exact Founder email
cohort has since advanced to schema 90. That does not activate customer-domain
provider delivery. Keep both control-plane routing gates and
`AGENT_EMAIL_CUSTOM_DOMAIN_DELIVERY_ENABLED` off, do not add a customer domain
to managed receive configuration, and do not change MX/Email Routing or send a
live custom-domain canary until separately approved.

The completed schema-88 wave created and verified the normal pre-migration
backup, then froze custom-domain projection, realm-alias projection, realm
close, account export/import, and cell movement during mixed-version
convergence. Migration `0088` adds the account-scoped,
evacuation-fenced `agent_email_custom_domain_routes` table and nullable
`agent_email_messages.recipient_custom_domain_request_id`. Deploy the canary
cell and then every intended destination before the control plane is allowed to
project. Verify the provision-token POST plus exact GET readback, same-revision
idempotency, stale/misbound rejection, account archive round trip, and signed
envelope/local-route provenance. No provider operation belongs in this wave.

The control plane does not discover these rows by scanning cells. Before its
first cell or KV write it journals one permanent sparse
`route-binding:<domain-request-id>:<realm-alias-claim-id>` and receives an
idempotent acknowledgement for the alias registry's permanent
`custom-domain-subscription:<realm-alias-claim-id>`. Domain changes then use a
journaled `route-source-intent`; subscribed alias changes use a journaled
`custom-domain-sync`. Their account, realm, reverse-binding, and due indexes are
derived and rebuilt after recovery, and every fan-out page is bounded. This is
why a cell move must preserve the control-plane registries and their journals:
the cell custom-route table is enforcement authority for that account, but it
is not the global membership catalog.

If a crash occurs after the domain binding is journaled but before the first
alias subscription is acknowledged, realm close fences that late subscription.
The retired, never-subscribed binding then completes without a cell or KV
write; the missing acknowledgement proves that no earlier leaf write was
permitted. An already acknowledged subscription remains permanent and must
pass the ordinary positive retirement barrier.

Ordinary direct domain or alias changes may finish their source commit while
the durable child outbox is still converging. Operators should expect the
normal 300-second cache window; because the edge accepts a timestamp up to 300
seconds ahead of its clock, acceptance tests must use 600 seconds as the formal
worst-case stale window when full clock skew is present. Do not apply that
eventual allowance to a parent transition. Plan completion and account
movement/close wait until every exact account outbox is complete. Realm close
waits for every subscribed custom-domain route to be retired in the cell and
edge directory before it prepares the cell realm. A queued or accepted child
task is not completion, and an empty elapsed TTL is not a substitute for the
positive barrier acknowledgement.

Removing a routing/activation gate must stop new applied projection while
still allowing already-bound suspended and retired work to drain. An account
with no permanent subscription remains a true dark no-op: alias mutation does
not call the custom-domain registry or arm a custom-domain alarm. During a
mixed-version rollback, do not deploy code that cannot classify the permanent
binding/subscription or journaled source outboxes once any of those authority
keys exists. Roll forward with the gates dark and let restrictive convergence
finish before resuming lifecycle work.

Empty-target recovery rebuilds sparse indexes from journaled bindings,
subscriptions, and pending source outboxes. It deliberately drops local leaf
projection intents and domain-side alias tasks. Only an outbox that was pending
at the replayed journal head regains a due entry; a completed permanent binding
does not create a recovery-due key, convergence obligation, or alarm. The
sealed drill target does not drain recovered work, perform cell/KV writes,
accept routing traffic, or become a cutover target. Recovery must fail if a
binding is orphaned from its domain allocation or if a subscription is orphaned
from its claim; never repair either condition by inventing the cross product or
copying cell inventory into global authority. Any future active restore would
require a separately reviewed explicit activation protocol that is not
implemented by this drill.

A schema-87 archive remains importable: it creates no custom-domain route and
uses the new provenance column's null default. Schema-88 archives preserve route
rows before their messages and validate the exact account, realm, domain, and
alias identities. Once any custom-domain route exists, including a retired
tombstone, or a custom-domain receipt exists, migration `0088` refuses downgrade
before mutation. Roll application behavior or configuration back while leaving
schema 88 intact and roll forward to repair; never delete authority or mail to
force schema 87.

Schema-89 archives add the durable outbound email streams: realm/agent send
controls, outbound messages, provider-event receipts, and recipient
suppressions. Schema 90 preserves those rows byte-for-byte and adds no portable
stream; inbound realm/account and outbound minute/daily/recipient limiter debt
is cell-local and excluded from export. Freeze account moves while a possible
destination remains below schema 90. After convergence, a restored account
starts with fresh defensive debt rather than importing source-cell buckets.

Schema 91 also adds no portable stream. Its retained-email capacity singleton
and triggers are cell-local platform safety state. The destination charges
imported mail against its own configured ledger and fails the account import
atomically if the rows do not fit; it never imports or overwrites the source
singleton. Freeze moves while any possible destination remains below schema 91,
and verify destination headroom before cutover. Do not raise the cell boundary
as an implicit accommodation for an account move.

For avatar creative-payload compaction, this release pin is Phase A: leave
`apps.witselfServer.avatarPayloadCompactionEnabled: false`, freeze avatar
mutation/import/export during writer convergence, and wait until every old
writer has drained. After Phase A is healthy, use a separate config-only commit
to set the gate to `true`; verify that the nested ConfigMap checksum restarts
every pod. Do not mix the Phase-B gate flip with another chart/image change.

Repeat the same narrow GitOps change and verification for later waves. A values
pin, a Git commit, or an Argo sync alone is not proof that a feature is
operational end to end. When a release changes installed hooks or managed
instructions, upgrade the client binary and rerun `witself install` for each
supported runtime before declaring the client behavior complete. See
[Release And Build Notes](release-and-build.md) and
[Autonomous Realm Messaging](autonomous-realm-messaging.md).

## Tenant migration

Moving a realm/account from cell A to cell B is bounded but not free:

1. **Export** the tenant from cell A.
2. **Import** into cell B.
3. **Repoint** the control-plane mapping from cell A to cell B.
4. **Cut over** with a brief read-only freeze, or dual-write + reconcile.

Per plane:

- **Open plane** (memories, facts, messaging) moves via the existing first-class
  export/import (see [storage.md](storage.md) and
  [backup-and-recovery.md](backup-and-recovery.md)). Immutable vector profiles and
  client-supplied JSONB vector rows move in that archive. Only derived full-text
  indexes and any future optional ANN projection are rebuilt in the destination.
- **Sealed plane** (secrets) is KMS-rooted per cell/cloud, so migration **re-wraps**
  keys under the destination KMS: an audited decrypt-at-source / re-encrypt-at-dest
  pass. The plaintext data keys are unwrapped under cell A's KMS and re-wrapped under
  cell B's KMS; the operation is audited end to end (see
  [key-hierarchy.md](key-hierarchy.md)).

Migration emits `tenant.migration_started`, `tenant.migration_completed`, and
`tenant.migration_failed`. After repoint, clients re-resolve and route to cell B.

## Fleet model

The fleet is many independent live cells, each authoritative for its own tenants.
There is no shared data store across cells and no shared-data multi-master across
clouds in v1 — that is a much harder problem and is deferred. A tenant has exactly one
home cell at a time; that cell is the single writer and source of truth for its data.

Migration (above) is how a tenant changes home cell; it is a deliberate, bounded
operation, not continuous replication. Per-cell backup/recovery follows
[backup-and-recovery.md](backup-and-recovery.md).

## Shared global directory

The collaboration relay needs to resolve a realm handle to where it lives plus its
signing key. That is the same registry the control plane already maintains for
placement and resolution: realm/account -> home cell + endpoint + signing key. Cells
and cross-realm collaboration share one global directory.

So a cross-realm message addressed to `witself://<realm-handle>/agent/<name>` resolves
through the same control-plane directory that routes a client to its home cell. The
relay routes by realm handle to the realm's home cell and verifies the published
signing key; see [agent-collaboration.md](agent-collaboration.md) for the blind-relay
model, signed realm/agent cards, and federation trust.

## Billing aggregation across cells

Billing is account-level (see [billing-and-limits.md](billing-and-limits.md)). An
account's realms may be placed on different cells. Usage is metered per realm in each
realm's home cell, then aggregated to the account level across cells for billing and
limit enforcement. The control-plane account -> realm -> cell mapping is what makes
cross-cell aggregation possible.

## Open decisions

These are open; this document records them without resolving them.

- **Placement unit (account vs realm).** Whether the cell-placement and migration unit
  is the account or the realm. Recommendation under discussion: the realm is the
  placement/migration unit, with an account-level default cell, and realms individually
  re-homeable.
- **Self-host single-cell vs multi-cell.** Whether a self-host deployment is always a
  single cell (single-tenant norm) or may itself be a multi-cell fleet with its own
  control plane.
- **Migration cutover approach.** Brief read-only freeze vs dual-write + reconcile as
  the default cutover mechanism.

## Cross-links

- [backend-architecture.md](backend-architecture.md) — backend code that runs in each cell
- [cloud-targets.md](cloud-targets.md) — provider order and per-cloud targets
- [`infra/pulumi`](../infra/pulumi) — executable per-cloud cell provisioner
- [storage.md](storage.md) — open/sealed planes, export/import
- [billing-and-limits.md](billing-and-limits.md) — account-level billing
- [backup-and-recovery.md](backup-and-recovery.md) — per-cell backup and migration data movement
- [agent-collaboration.md](agent-collaboration.md) — cross-realm collaboration over the shared global directory
