# Runbooks

Hand-testing recipes for what is actually built and running. Grown one entry at
a time. Commands here use the `default` account — what every command picks when
`--account` is omitted and `WITSELF_ACCOUNT` is unset. To juggle several
accounts, add `--name NAME` at create and `--account NAME` everywhere after.

## Create an account on Witself Cloud

Requires an invite code.

```sh
witself account create --email scott@witwave.ai --invite friends-2026
```

The account is remembered locally as `default` (binding in
`~/.witself/config.json`, token under `~/.witself/tokens/accounts/`).

## Check account status

New accounts start **pending**: nothing works until the emailed verification
link is clicked (`witself account resend-verification` sends a fresh one). Watch
for it to flip to `active`:

```sh
witself account status
```

## Create a realm and an agent

What an active account is for: realms partition the account, agents live in a
realm, and an agent token is the credential your agent actually runs with.
The ids come from each command's output.

```sh
witself realm create prod
witself agent create --realm realm_01xyz my-agent
witself token create --agent agt_01xyz
```

The agent token is written to
`~/.witself/tokens/accounts/default/agents/my-agent.token` — hand it to the
workload; `witself token revoke --token tok_ID --yes` kills it without touching
anything else.

## Inspect and install local AI integrations

Inventory is read-only. The normal view separates provider detection, platform
support, and persisted Witself state; `--verify` additionally checks each
installed provider against its exact recorded binding:

```sh
witself integrations
witself integrations --verify
witself integrations --verify --json
```

Preview a bulk install before changing provider configuration, then use the
literal `all` selector:

```sh
witself install all --agent my-agent --location home --dry-run
witself install all --agent my-agent --location home
witself install all --agent my-agent --location home --json
```

Do not substitute an unquoted `*`; the shell can expand it to filenames before
Witself receives the command. Bulk install skips runtimes that are undetected or
unsupported on the current platform, continues after an individual failure, and
does not roll back earlier successful runtimes. On native Windows, Cursor is
WSL-only; install both Cursor and Witself inside the same WSL distribution.
Native Windows Claude Code and Grok Build install core MCP/routing without
transcript hooks, while Windows Codex receives user-scoped hooks.

After restarting each installed runtime, use `witself integrations --verify` to
check the local topology. A healthy result proves exact local configuration,
not that an authenticated provider model invoked a tool. End-to-end acceptance
uses a dedicated provider QA account and disposable provider profile, never a
personal profile. To remove every recorded integration while retaining Witself
tokens and queued transcript events:

```sh
witself uninstall all --dry-run
witself uninstall all
witself uninstall all --json
```

## List operators

Every account is born with one root operator, `owner` — the identity your
local token authenticates as. Operators you add later appear alongside it:

```sh
witself operator list
```

## Create a backup operator token

A second credential for the same operator, so losing `owner.token` doesn't
lock you out:

```sh
witself token create --operator --name backup
```

The token is written to
`~/.witself/tokens/accounts/default/operators/backup.token`. Copy it into a
password manager (1Password or similar) and delete the file — a backup that
lives beside `owner.token` disappears with it, and re-minting under the same
name refuses while the file exists. Add `--out -` to print to the screen
instead, or `--out FILE` for a path of your choosing.

## Revoke a token

Each token dies independently — revoking one never touches the others:

```sh
witself token revoke --operator --name backup --yes
```

Revoking by name also removes the managed token file (revoking by id leaves
files alone). Any token (an agent's, another operator's) can be revoked by id
instead: `witself token revoke --token tok_ID --yes` — ids are in the last column
of `witself operator list`.

## Recover a lost owner token

Recovery proves inbox control: a code goes to the account's email, and
redeeming it rotates the owner's credentials — the old tokens die, agents and
other operators are untouched. Requesting a code changes nothing by itself.

```sh
witself account recover
# check the account's email for the code (valid ~15 minutes), then:
witself account recover --code 123-456-789
```

`witself account list` shows this machine's local names and account ids — handy
when the token is gone but you need the id. From a machine with no binding,
use `--id acc_...` (add `--name NAME` to save the recovered credential).

Recovery revokes **every** token the owner holds — including a backup stored
in a password manager — so re-mint the backup afterward. Tokens on other
operators and agents survive; automation that must outlive a recovery belongs
on its own operator.

---

The entries below are rarely needed.

## Adopt an existing account

For a token that arrived without a local name: a teammate minted you an
operator token, the token predates local names (a pre-v0.0.63 `--out` file),
or this is a second machine for an account created elsewhere.

```sh
witself account adopt --id acc_01xyz --token-file teammate.token --name shared-account
```

The token is verified against the account's cell first — it must authenticate
and belong to `acc_01xyz`. On success the binding is saved like `witself account
create` would: follow-up commands are just `--account shared-account`.
`--name` is required; adopting never falls back to `default`.

## Change the account email

Owner-only; nothing else changes — tokens, operators, and agents all keep
working. Three emails tell the story:

1. **New address**: a confirmation code (proves the inbox can receive).
2. **Old address, immediately**: a warning that a change was requested — if it
   wasn't you, `witself account recover` rotates the owner credentials before the
   change can commit.
3. **Old address, after the commit**: a revert link valid for **48 hours**.
   Clicking it points the account back at the old address and kills any
   outstanding recovery code — the safety net if a stolen token moved the
   email out from under you. It refuses politely if the email has since
   changed again legitimately.

```sh
witself account change-email --new-email new@example.com
# check the new address for the code, then:
witself account change-email --new-email new@example.com --code 123-456-789
```

## Add a second operator

For a teammate (or automation that must survive an owner recovery). Your side:
create the operator with a short-lived transfer token —

```sh
witself operator create --name "Alice" --token-name alice-bootstrap --ttl 24h --out alice.token
```

— then send Alice two things over a channel you trust: the `alice.token` file
and this command (fill in your account id from `witself account list`):

```sh
witself account adopt --id acc_01xyz --token-file alice.token --name work
```

Her side: the adopt binds the account on her machine, then
`witself token create --operator --name laptop` mints her own durable token into
her managed path — a credential only she has ever seen. The transfer token
expires within 24 hours on its own.

`witself operator delete --yes opr_ID` retires an operator and revokes everything
it holds.

## Forget a stranded local name

When an account is closed out from under the CLI — the verification window
expired and the reaper took it — the local name lives on with a dead token,
and `witself account close` can no longer authenticate to clean it up. Drop the
local binding only (this never contacts the server):

```sh
witself account forget --account default --yes
```

## Suspend and resume an account

Suspend freezes ordinary account work while keeping credentials and explicit
safety paths alive — a reversible pause for time off, an audit, or a planned
migration. Agent-email receive controls are the narrow exception: lifecycle
state remains readable and an operator may disable an agent or realm layer
while suspended, but may not re-enable either layer until the account resumes.

```sh
witself account suspend --yes                       # optionally --reason "on vacation"
# ordinary domain commands now refuse:
#   witself: account is suspended — this action requires an active account
# harm-reducing receive shutdown remains available:
witself email operator receive disable --realm-id realm_aaaaaaaaaaaaaaaa
# status still works, and shows why:
witself account status
witself account resume
```

Only the owner can suspend or resume their own suspension. Future non-owner
suspensions (planned: migration, fleet-admin, billing) will refuse `witself account
resume` — the authority that suspended is the one that resumes.

## Close an account

Closing is permanent: every credential is revoked and the account is retired
(its record remains as a tombstone). On success the local name is removed too.

```sh
witself account close --yes
```

Add `--reason TEXT` to record why.

## Close a realm without resurrecting its email route

Delete every agent and permanently retire every memorable alias in the realm,
then use the ordinary named-account command:

```sh
witself realm delete --account default --yes realm_aaaaaaaaaaaaaaaa
```

For a managed account this calls the control plane, which persists one exact
close operation, verifies there are no live/pending aliases, prepares the cell
route generation, publishes the canonical tombstone, and commits the cell
soft-delete. A response such as `close accepted (publish_retired)` is success:
the durable alarm continues the same operation until complete. Repeating the
same CLI command uses the same deterministic idempotency key and is safe.

Do not bypass this path by calling the managed cell's realm-delete endpoint;
the cell refuses that operation before its canonical retirement fence exists.
Explicit `--endpoint` plus `--token-file` remains the self-hosted path and
directly deletes an empty realm while writing the same portable retired shape.

## Decommission a cell and preserve its accounts

`witself-infra destroy` is the fleet operator's counterpart to signup: it drains
the cell (stops placement), evacuates every account into a per-account archive
in Cloudflare R2, then removes the cell from the fleet and tears down the AWS
resources. The accounts wait in R2 as `archived — awaiting placement` until
they are restored onto another cell.

```sh
witself-infra destroy \
  -account-alias sandbox -aws-profile witwave-sandbox \
  -backend s3 -cloud aws -region us-west-2 -role dev \
  -control-plane https://self.witwave.ai \
  -fleet-token-file ~/.witself/tokens/fleet.token \
  -domain cells.witself.witwave.ai
```

You'll see one line per account: `evacuated acc_… from <cell>` for real users,
`reaped pending acc_… on <cell> (no archive)` for signups that hadn't yet
verified their email (those die with the cell — incomplete signups are not
preserved). The loop ends with `<cell>: N accounts evacuated to Cloudflare R2`,
after which Pulumi tears down the stack.

Evacuation does not make an R2 upload authoritative merely because its HTTP
body reached EOF. Each attempt uses an isolated object key, then the Worker
streams the committed object back through gzip, tar, manifest, and trailing
checksum validation before it writes `archived:` or removes `acct:`. A failed
validation deletes only that attempt's object and leaves routing intact. The
control plane then removes the exact source write fence only after the cell
acknowledges that same evacuation id as aborted. If that acknowledgement is
ambiguous, the account intentionally remains fenced and routed rather than
guessing that it is safe; do not tear the cell down. Diagnose the source-cell
export log and retry evacuation.
Every close, evacuation, and restore for an account is serialized through that
account's Durable Object, and an existing `archived:` pointer is revalidated
before it can retire a still-live source route.
The SHA-256 checksums and in-memory manifest, scope, and graph checks detect
truncation and accidental corruption; they are integrity checks, not archive
authentication. A compromised writer that can forge both an archive and its
checksums is outside this mechanism's guarantee, so preserve the provision-token
and R2 access boundaries.

R2 bucket invariant: the archive bucket must have a lifecycle rule that aborts
stale incomplete multipart uploads for every prefix. The production
`witself-archives` bucket uses `Default Multipart Abort Rule` with a seven-day
limit. Do not remove or narrow that rule: a Worker terminated before it can
record the multipart upload id cannot abort that upload itself. If multipart
completion is ambiguous before archive authority is committed, the Worker
first obtains an exact source abort receipt and then best-effort deletes the
attempt-unique completed object key.

Sandbox override: add `-destroy-accounts` to skip the archive step entirely
and force-purge the directory entries. This is an explicit acknowledgment
that every account on the cell dies with it — no restore is possible.

While the archives sit in R2 you can verify state at any time:

```sh
witself account status --account <name>          # says "archived — awaiting placement"
curl https://self.witwave.ai/v1/directory/<account-id>
```

## Back up both serving Civo databases before a migration

Before running `scripts/roll-cell.sh` for a release that can advance the
database schema, run the encrypted logical backup and disposable restore drill
for **both** serving Civo cells. This is not the account-archive or periodic R2
backup path.

Use an explicit owner-only kubeconfig/context for each cell, an existing
mode-0700 output directory outside the checkout, a separate age recipient and
identity, and a preloaded pgvector image matching the live PostgreSQL major.
Then follow the two
exact invocations in
[Civo PostgreSQL pre-migration backup](backup-and-recovery.md#civo-postgresql-pre-migration-backup).

Do not change either GitOps values file until there are two current manifests,
one for `civo-sandbox-use1-backup` and one for `civo-sandbox-usw2-dev`, with all
of these properties:

```sh
jq -e --arg version "$RELEASE_VERSION" '
  .schema == "witself.civo-pre-migration-backup.v1" and
  .target_release == $version and
  .restore_verification.status == "verified" and
  .restore_verification.disposable_target_cleaned == true and
  .restore_verification.pgvector_extension_matches_source == true and
  .restore_verification.schema_version == .source.schema_version
' /protected/path/<backup-id>/<backup-id>.json
```

From inside each artifact directory, verify its ciphertext checksum with
`sha256sum -c <backup-id>.sha256` (or `shasum -a 256 -c` on a host without
`sha256sum`). Record the exact two backup IDs and SHA-256 values in the private
rollout record. The script never writes to the source database and never leaves
a plaintext dump; a `pending` manifest or any nonzero exit blocks the rollout.
Do not run a live restore as part of this procedure.

## Move and stage the `witmail.net` managed-email domain

This is a registrar and edge-foundation procedure, not an email activation.
`witmail.net` was registered with Cloudflare Registrar on 2026-08-03 in a
source Cloudflare account so the name could be secured. The source-account zone
is intentionally dormant: it contains restrictive SPF/DKIM/DMARC
anti-spoofing records but no MX records, Email Routing configuration, Worker
route, catch-all, or DNSSEC delegation. Do not add any of those delivery paths
while the registration remains in the source account.

Cloudflare's
[Registrar inter-account move](https://developers.cloudflare.com/registrar/account-options/inter-account-transfer/)
is different from a transfer to another registrar. At the time of this
decision, Cloudflare requires the registration to be **more than 10 days old**
before an inter-account move. The strict threshold passes partway through
2026-08-13 for this registration, so use **2026-08-14** as the earliest
practical move date and let the dashboard's live eligibility result be
authoritative. A successful move applies a 30-day
registration transfer lock. Do not change registrant contact information while
waiting: a contact change can create a separate 60-day lock that also blocks an
inter-account move.

Before requesting the move:

1. Confirm the exact production Cloudflare account ID and record it in the
   private operator change record, not in this repository.
2. Add `witmail.net` to that target account as a DNS zone (the Cloudflare UI may
   call this a website), select its plan, add no web records, and wait until
   Cloudflare reports the target zone ready for the move.
3. Verify the registrant email. Keep DNSSEC off and release any source-zone
   lock as Cloudflare requires.
4. Export the source DNS zone and separately record the reviewed restrictive
   sender-auth records. Cloudflare moves WHOIS registration data but **no zone
   configuration or settings**.
5. Submit the move from the source account. An administrator of the target
   account must accept it within Cloudflare's five-day window; otherwise the
   request is canceled.

After acceptance, verify the registration, renewal responsibility, nameservers,
and target-zone ownership before writing DNS. Recreate the restrictive
SPF/DKIM/DMARC posture from the reviewed record, compare it with the source
export, and only then enable DNSSEC in the target account. Do not copy an MX
record, Email Routing rule, catch-all, Worker association, KV projection, or
secret from the source account; none should exist there, and all production
edge state must be created from reviewed infrastructure in the target account.

Before changing the configured primary managed domain, prove that
`agent-mail.witwave.ai` has no realm-alias request, assignment, or cell
projection. If a future cutover finds one, resolve and retire it under the old
primary-domain configuration first. The compatibility path deliberately keeps
only previously issued canonical local parts; it cannot converge a legacy
realm alias after `.net` becomes primary.

Keep the rollout dark in this order:

1. Deploy code and configuration that name `witmail.net` as the primary managed
   domain while all five gates below are absent or exactly `false`. If an
   account was suspended during this cutover, resume it first and then
   explicitly restart or run normal startup reconciliation; resume alone does
   not add the missing primary route. Verify that reconciliation preserves the
   existing address and mailbox IDs.
2. Prove the authority journal and registry are healthy, inventory current
   canonical local parts, and verify that no new alias can target the retired
   `agent-mail.witwave.ai` domain.
3. Build a bounded compatibility manifest containing only canonical local
   parts that were actually issued on `agent-mail.witwave.ai`. Never add a new
   legacy canonical identity or alias, and never attach a broad catch-all to
   the legacy domain.
4. Recreate the `witmail.net` Email Routing foundation only after the Worker,
   target-cell validation, role-address destinations, and rollback path have
   all passed review. Use the separately fenced production path: bootstrap
   `witself-agent-email-receive` dark, converge the exact account cohort and
   mailbox backfill, establish the operator role routes, and require the
   ordinary, storage, and near-limit production probes below to pass before
   broad routing. Keep the public apex catch-all disabled until its own
   reviewed activation step.
5. Treat canonical inventory, canonical delivery, alias activation, and alias
   delivery as separate later reviews. Personal remains receive-disabled even
   after the managed domain is live, and a plan-table address promise never
   enables delivery by itself.

The required dark gates are:

- `CP_REALM_EMAIL_ALIAS_ACTIVATION_ENABLED`
- `REALM_EMAIL_ALIAS_DELIVERY_ENABLED`
- `CP_REALM_EMAIL_CANONICAL_INVENTORY_ENABLED`
- `CP_REALM_EMAIL_CANONICAL_DELIVERY_ENABLED`
- `REALM_EMAIL_CANONICAL_DELIVERY_ENABLED`

Before any routing review, choose and verify operator-controlled destinations
for `postmaster@witmail.net` and `abuse@witmail.net`. That is a human governance
decision; do not silently route either role address to a personal mailbox.

### Bootstrap the production receive Worker once

The production service is `witself-agent-email-receive`; it is not the retired
`witself-agent-email-pilot` Worker. A Worker rename does not transfer secrets,
deployments, or routes. Before deploying a control plane whose release guard
expects the production name, bootstrap the new Worker dark from that same clean
tag. Use the existing email-route KV namespace ID and legacy physical title;
never create a second namespace.

Prepare a temporary mode-`0600` JSON secrets file outside Git containing
exactly `CONTROL_PLANE_EDGE_TOKEN` and the base64 PKCS#8
`RELAY_ED25519_PRIVATE_KEY`. With the cohort empty, both receive delivery gates
false, the exact `witmail.net` account/zone/KV environment, and an API token
that can read the complete Email Routing inventory in both managed zones, set
`CLOUDFLARE_LEGACY_EMAIL_ZONE_ID` to the distinct `witwave.ai` zone ID, set
`RELAY_ED25519_PUBLIC_KEY` to the reviewed canonical base64 raw public key and
set the operator `CONTROL_PLANE_EDGE_TOKEN` to the byte-identical token in the
secrets file. Then run:

```sh
cd infra/cloudflare/agent-email
npm run bootstrap:production-receive -- \
  --secrets-file /absolute/private/receive-bootstrap-secrets.json \
  --receipt /absolute/private/receive-bootstrap-receipt.json
```

The command refuses an existing production Worker, checks that every retired
Worker delivery trust anchor is absent, requires the `witmail.net` catch-all
and all routes targeting either Witself email Worker to be disabled, and holds
the global operations lease. The existing `witwave.ai` company-mail catch-all
and unrelated forwarding rules may remain active, but their complete state is
fingerprinted and neither may target a Witself email Worker. The command
freezes the exact tagged Worker sources and writes a durable pending receipt
before the provider mutation. The secrets file and receipt must both be
outside the repository. It uploads both secrets with the initial tagged
version and finalizes success only after the exact resulting deployment,
unchanged predecessor and two-zone route inventories, lease release, and
private-input cleanup are verified. A pending receipt means stop and
reconcile; it is not permission to retry elsewhere. Securely remove the
operator secrets file afterward. Keep the retired Worker deployed and
unrouted through the production soak period.

### Roll out production cell receive and the v0.0.245 retry canary

This procedure supersedes the earlier blanket instruction not to deploy schema
88 to a serving cell. Production receive itself requires chart and image
`v0.0.241` or newer; this production procedure uses matching `v0.0.245` or
newer so the selected retry canary can remain outside Git and the non-secret
ConfigMap. It promotes the cell-side scalable receive foundation; it does not
approve custom-domain provider delivery or turn on any control-plane, edge, MX,
catch-all, canonical-delivery, or alias-delivery gate. Those remain dark until
their own reviewed stage.

1. Create and verify the normal pre-migration backup. Deploy matching chart and
   image `v0.0.245` or newer with both `receivePilot.enabled` and
   `receiveProduction.enabled` false. Wait for every old writer to drain and
   verify the release's migrations plus API health before changing receive
   configuration.
2. Build one canonical, byte-sorted CSV of 1-100 exact account IDs resident in
   this cell, with no whitespace or trailing newline. Store it outside Git and
   provision it as one immutable, versioned Kubernetes Secret data value in the
   server namespace. Set only `accountIDsExistingSecret.name` and `.key` in
   managed cell values; leave the literal `accountIDs` array and
   `retryCanaryAgentID` empty. Also leave
   `retryCanaryAgentIDExistingSecret.name` empty for this first rollout. Deploy
   the identical CSV to the
   separately guarded control-plane/edge cohort settings, but keep every
   delivery gate false. There is no wildcard. A missing Secret/key, duplicated,
   malformed, whitespace-padded, unsorted, or cross-cell account must stop the
   rollout. Never mutate the referenced Secret in place: create the next
   immutable Secret and update its versioned reference name, which rolls every
   API pod. Verify the new cohort before changing edge state.
3. Enable only `agentEmail.receiveProduction` in the cell. Its startup is
   read-only and O(cohort): each API pod verifies account presence/status and,
   after the second rollout below, the selected retry-canary membership. It
   never scans or mutates all agents.
   Missing existing mailboxes therefore do not block readiness. From this point,
   every successful new-agent create in the cohort includes its mailbox in the
   same transaction.
4. Run exactly one isolated backfill Job from the released cell image. Never
   exec the command in an API pod:

   ```sh
   scripts/run-agent-email-cell-operation.sh \
     --cell CELL --kubeconfig KUBECONFIG --context CONTEXT \
     --operation backfill \
     --artifact-output /absolute/private/backfill-exception.json
   ```

   The fixed Job name prevents concurrent runs. The script snapshots only the
   non-secret ConfigMap, inherits the exact database/cohort and optional
   retry-canary Secret references without reading their values, and exports
   from a memory-backed volume without putting identities or Secret references
   in its output. A retry-canary Secret is accepted only from a `v0.0.245` or
   newer source image. Record only its
   value-free JSON counts. The exception path must be new,
   canonical, absolute, and outside Git. It is created mode `0600` only if one
   agent needs a private operator override; review it locally and use a new
   path on the rerun. The command pages at 100 agents, is idempotent,
   and is safe to rerun after interruption. Never add it to API startup or run
   one copy per replica. A successful result must report
   `missing_mailbox_count_after: 0`. Suspended accounts are verified read-only;
   resume and rerun explicitly if repair is required.

   A killed operator process can leave the fixed concurrency lock behind. Do
   not delete it merely because a later invocation reports a collision. Inspect
   only the exact operation Job and its exact job-name pod set first:

   ```sh
   KUBE=(kubectl --request-timeout=30s --kubeconfig KUBECONFIG \
     --context CONTEXT -n witself)
   "${KUBE[@]}" get job witself-agent-email-operation \
     --ignore-not-found=true \
     -o 'jsonpath={.metadata.name}{"\t"}{.status.active}{"\t"}{.status.succeeded}{"\t"}{.status.failed}{"\n"}'
   "${KUBE[@]}" get pods \
     -l 'batch.kubernetes.io/job-name=witself-agent-email-operation' \
     -o 'jsonpath={range .items[*]}{.metadata.name}{"\t"}{.status.phase}{"\n"}{end}'
   ```

   A failed read is unknown and therefore active. Treat a Job active count as
   active, and treat every pod phase other than the terminal `Succeeded` and
   `Failed` phases as active, including `Unknown`. Stop and resolve operator
   ownership before touching the fixed resources if any result is active or
   ambiguous. Once no operator owns the run and every observed exact pod is
   terminal, foreground-delete the exact Job and then separately prove that its
   exact pod set is empty. Only after that proof may the override Secret and
   fixed lock be removed:

   ```sh
   if ! "${KUBE[@]}" delete job witself-agent-email-operation \
     --ignore-not-found=true --cascade=foreground --wait=true --timeout=30s; then
     echo 'could not foreground-delete the exact operation Job; leave the lock in place' >&2
     exit 1
   fi

   REMAINING_PODS="$("${KUBE[@]}" get pods \
     -l 'batch.kubernetes.io/job-name=witself-agent-email-operation' \
     -o name)" || {
       echo 'could not prove the exact operation pods absent; leave the lock in place' >&2
       exit 1
     }
   [ -z "$REMAINING_PODS" ] || {
     echo 'exact operation pods remain; leave the lock and override Secret in place' >&2
     exit 1
   }

   if ! "${KUBE[@]}" delete secret witself-agent-email-operation-overrides \
     --ignore-not-found=true --wait=true --timeout=30s; then
     echo 'could not delete the exact override Secret; leave the lock in place' >&2
     exit 1
   fi
   "${KUBE[@]}" delete configmap witself-agent-email-operation-lock \
     --ignore-not-found=true --wait=true --timeout=30s
   ```

   If foreground deletion times out or the pod absence read fails, stop and
   deliberately leave the lock and override Secret stale. Never delete by a
   broad application label. A lost private artifact is recovered by rerunning
   the idempotent operation with a new absent local output path.
5. Verify a receive-disabled Personal account in the cohort still reaches the
   deterministic accept-and-discard path with no message or delivery row, while
   an entitled test account persists one signed synthetic delivery. Keep this
   inside the cell/relay test boundary; provider routing is still dark. Verify a
   plan flip takes effect without reinstalling the client.
6. Generate a new mode-0600 canary manifest with the cell-native command in the
   next section. It must be derived after zero-missing verification. Choose one
   eligible agent from that private artifact. Store exactly its canonical
   `agent_*` ID, with no leading or trailing whitespace and no trailing newline,
   in a distinct immutable, versioned Kubernetes Secret. Do not reuse the
   cohort Secret. Keep the literal `retryCanaryAgentID` empty and set only
   `retryCanaryAgentIDExistingSecret.name` and `.key` in a separate config-only
   rollout. Changing either field changes both server rollout checksums. Wait
   for every replacement API pod to become Ready; startup must verify that the
   selected agent is live and belongs to the configured cohort. Re-run the
   exporter to a new absent path and verify privately that the selected agent
   is included. That second manifest must pass `routes:primary -- status`
   before a disabled-rule plan is created.

### Prove the Personal-to-Professional receive boundary inside one cell

Run this before enabling a provider rule. It proves the signed cell-ingest
boundary only; it does not send through Cloudflare, change DNS or routing,
create an account, install a client, or mutate a plan. Use an existing,
dedicated Personal account already in the exact cell cohort with one live
canonical `@witmail.net` mailbox. Do not use a customer mailbox or an account
with an existing plan override.

Prepare a mode-`0700` directory outside every Git worktree. In it, create:

- a mode-`0600` target JSON with exactly the schema printed by
  `scripts/run-agent-email-cell-smoke.sh --help`;
- an absent path for the state JSON;
- the existing installed client's full `witself_agt_*` token in a mode-`0600`
  file; use the same file and bytes in both signed phases;
- the reviewed base64 PKCS8 Ed25519 relay private key in a mode-`0600` file,
  obtained from approved escrow rather than a pod or provider log;
- the mode-`0600` kubeconfig for the destination cell.

The target is private because it contains account, realm, agent, and canonical
address identities. The state is private crash-recovery evidence and remains
the same path for every phase. The harness obtains the matching public key and
audience from the fenced cell ConfigMap, proves the private key has exactly one
matching key ID, and does not read that key until the cell, cohort, target,
effective policy, and zero-row preconditions pass.
It likewise delays reading the agent token until those preconditions pass,
proves its hash is one live full credential for the exact target, and stores
only that digest in the private state. The plaintext token is never placed in
an argument, environment variable, SQL statement, or operator output.

First prove authoritative Personal policy. The entitlement-version marker must
be `1`, `agent_email_receive` must be absent, and there must be no existing
message, delivery, or receive-event row for the fresh probe:

```sh
SMOKE_STATE=/absolute/private/receive-smoke-state.json
SMOKE_TARGET=/absolute/private/receive-smoke-target.json
RELAY_KEY_FILE=/absolute/private/relay-ed25519-private-key
RELAY_KEY_ID=reviewed-key-id
AGENT_TOKEN_FILE=/absolute/private/installed-agent-token

scripts/run-agent-email-cell-smoke.sh \
  --cell CELL --kubeconfig KUBECONFIG --context CONTEXT \
  --phase disabled --target-file "$SMOKE_TARGET" \
  --state-file "$SMOKE_STATE" \
  --agent-token-file "$AGENT_TOKEN_FILE" \
  --relay-key-id "$RELAY_KEY_ID" \
  --relay-private-key-file "$RELAY_KEY_FILE"
```

Success is one value-free JSON document with HTTP 200,
`verdict:"feature_disabled"`, and zero messages, deliveries, and audit events
before and after, including zero change in the target owner's total receive
event count. Before ingest, the same loopback service must return the exact
HTTP 403, non-retryable `feature_not_enabled` owner response to that agent token.
The script signs operator-side, reaches only a loopback
`kubectl port-forward` to the fenced API Service, and issues at most one POST.
It shares the fixed cell-operation lock with backfill/canary Jobs. A timeout or
lost response is `indeterminate`; never rerun the signed phase. Keep the state
and use `--phase cleanup` to reconcile zero or one row.

This is a temporary audited classification exception, not a billing change.
First prove that the billing and effective plans are Personal, application is
settled, and no plan override exists:

```sh
CONTROL_PLANE_URL="${CONTROL_PLANE_URL:-https://self.witwave.ai}"
PLATFORM_ADMIN_TOKEN_FILE=/absolute/private/platform-admin-token
ACCOUNT_ID=acc_reviewed

witself-admin account plan-override get \
  --endpoint "$CONTROL_PLANE_URL" \
  --token-file "$PLATFORM_ADMIN_TOKEN_FILE" \
  --account "$ACCOUNT_ID" --json |
  jq -e '.billing_plan=="free" and .plan=="free" and
    .plan_override==null and .apply_pending==false and
    .desired_revision==.applied_revision'
```

Stop if this read fails or reports a pre-existing override. Set Professional
with a reviewed reason, then poll `plan-override get --json` until
`plan=="standard"`, `email_receive.enabled==true`, `apply_pending==false`, and
the desired/applied revisions match. A `set` response may intentionally return
nonzero while cell application is pending; accept only a parseable policy
document and verify convergence independently:

```sh
witself-admin account plan-override set \
  --endpoint "$CONTROL_PLANE_URL" \
  --token-file "$PLATFORM_ADMIN_TOKEN_FILE" \
  --account "$ACCOUNT_ID" --plan standard \
  --reason "production receive plan-flip smoke" --json

witself-admin account plan-override get \
  --endpoint "$CONTROL_PLANE_URL" \
  --token-file "$PLATFORM_ADMIN_TOKEN_FILE" \
  --account "$ACCOUNT_ID" --json |
  jq -e '.billing_plan=="free" and .plan=="standard" and
    .plan_override.plan=="standard" and .email_receive.enabled==true and
    .apply_pending==false and .desired_revision==.applied_revision'
```

Run the entitled phase with the byte-identical target, state, agent token, key
ID, key, kubeconfig, context, and cell. The harness requires a strictly newer
applied snapshot while the Deployment UID, generation, image, ConfigMap
checksum, API Service UID/resourceVersion, exact selector and port set, and
target remain unchanged. The Service selector must exactly match the fenced
Deployment selector before, immediately before, and immediately after the
request. The harness rejects a different token digest and requires the same
token to return its exact enabled canonical mailbox before ingest. Together,
those are the no-reinstall/no-redeploy proof:

```sh
scripts/run-agent-email-cell-smoke.sh \
  --cell CELL --kubeconfig KUBECONFIG --context CONTEXT \
  --phase entitled --target-file "$SMOKE_TARGET" \
  --state-file "$SMOKE_STATE" \
  --agent-token-file "$AGENT_TOKEN_FILE" \
  --relay-key-id "$RELAY_KEY_ID" \
  --relay-private-key-file "$RELAY_KEY_FILE"
```

Success is HTTP 200 with `verdict:"accepted"`, an exact `0 -> 1` message,
delivery, and probe-linked event transition, and exactly `+1` in the target
owner's total receive-event count. It also reports `cleanup_required:true`.
Run cleanup once; it uses no relay key and sends no request:

```sh
scripts/run-agent-email-cell-smoke.sh \
  --cell CELL --kubeconfig KUBECONFIG --context CONTEXT \
  --phase cleanup --target-file "$SMOKE_TARGET" \
  --state-file "$SMOKE_STATE"
```

Cleanup locks the account and every row matching any unique probe marker. It
requires each suspect to satisfy the complete synthetic predicate, recomputes
the hash from stored raw MIME, and uses the exact verified Professional
message ID when available. It then refuses any row that was read,
acknowledged, claimed, completed, failure-counted, duplicate-linked,
retry-canary-linked, ambiguous, or missing its one receive audit event. On a
safe row it deletes only the synthetic message; its delivery cascades and the
attachment counter trigger reconciles. The append-only account event and
shared rate-limiter state remain. If cleanup cannot prove safety, retain the
state and investigate; never hand-delete by digest, address, or broad label.

Finally clear only the override created above and poll until Personal is
settled again. Do this on success and on every failure after `set`:

```sh
witself-admin account plan-override clear \
  --endpoint "$CONTROL_PLANE_URL" \
  --token-file "$PLATFORM_ADMIN_TOKEN_FILE" \
  --account "$ACCOUNT_ID" \
  --reason "restore Personal after production receive smoke" --json

witself-admin account plan-override get \
  --endpoint "$CONTROL_PLANE_URL" \
  --token-file "$PLATFORM_ADMIN_TOKEN_FILE" \
  --account "$ACCOUNT_ID" --json |
  jq -e '.billing_plan=="free" and .plan=="free" and
    .plan_override==null and .email_receive.enabled==false and
    .apply_pending==false and .desired_revision==.applied_revision'
```

Do not delete the private state until synthetic cleanup and Personal restore
are both verified. Record only the three value-free harness result documents
and the control-plane audit reason; never attach the target, state, key, or raw
SQL/HTTP artifacts to a ticket.

Before provider activation, rollback is config-only: disable any canary
schedule, settle an unused arm or let its 15-minute TTL expire, clear
`retryCanaryAgentIDExistingSecret.name`, and wait for every API pod to converge.
To deploy code older than `v0.0.245`, that Secret reference must be empty; the
app-of-apps omits the unknown field from older strict child schemas. To deploy
code older than `v0.0.241`, also set production receive false and drain all
newer pods. Leave the idempotently created mailboxes and permanent address
reservations intact; do not delete mailbox rows to undo a cohort selection.
Once any edge rule is prepared or activated, also follow the separately fenced
primary-route disable and removal sequence below.

### Stage the primary-domain canary and catch-all

Do not use `infra/cloudflare/agent-email`'s original `npm run routes` command
for `witmail.net`. That command owns only the retired legacy manifest and
unsigned `pilot:*` KV rows; the primary-domain Worker ignores those rows.
Do not hand-author address strings. After the released cell's explicit mailbox
backfill reports zero missing mailboxes, run the cell-native read-only exporter
once with that cell's exact production receive environment:

```sh
scripts/run-agent-email-cell-operation.sh \
  --cell CELL --kubeconfig KUBECONFIG --context CONTEXT \
  --operation canary-manifest \
  --artifact-output /absolute/private/primary-canary.json
```

The output path must be new and outside the repository. The command derives
5-10 sorted entries from actual `witmail.net` primary mailbox rows with active
entitlement and enabled account/realm/agent receive state, and creates the exact
edge manifest using exclusive mode `0600`. A configured retry canary is included
for both literal/private and managed Secret-backed modes. The operation Job
copies and fences the optional Secret reference without reading or printing its
value. On the first managed export the reference is empty; after choosing one
eligible agent and completing the `v0.0.245` config-only Secret rollout above,
create a second export and require that it includes the selection. The command
prints no identities or addresses. Move each private artifact to
operator-controlled storage without relaxing its mode.

Before creating any Worker-targeted rule, enable the zone-wide Email Routing
subaddressing foundation through its dedicated fenced helper. Run these
commands from `infra/cloudflare/agent-email` with the exact production
Cloudflare account and `witmail.net` zone environment:

```sh
npm run routing:foundation -- status
npm run routing:foundation -- enable \
  --output /absolute/private/routing-foundation-enable-plan.json
# Review the mode-0600 plan's target ids, current and desired settings,
# complete provider fingerprints, expiration, and printed SHA-256.
npm run routing:foundation -- apply \
  --plan /absolute/private/routing-foundation-enable-plan.json \
  --plan-sha256 REVIEWED_SHA256 \
  --receipt-output /absolute/private/routing-foundation-enable-receipt.json
npm run routing:foundation -- status
```

The enable planner requires ready Email Routing with subaddressing currently
false, a disabled catch-all, both operator role forwards, and zero rules
targeting either email Worker. Apply reconstructs the exact 15-minute plan
under the global `email_routing_settings_apply` lease, PATCHes only the
normalized settings contract, and refuses changed zone, setting, catch-all,
role, or rule-inventory state. It reserves and fsyncs a new mode-`0600` pending
receipt before mutation and replaces it only after exact readback. An enable
provider mutation or postcondition failure attempts to restore the exact
disabled predecessor while the original lease remains renewable. If a pending
marker remains, retain it and reconcile live state before retrying. A lease
settlement, release, or receipt-commit failure is ambiguous: subaddressing may
have changed, but the disabled catch-all and absence of Witself Worker rules keep
delivery dark.

Emergency rollback of this foundation is a separately planned `disable` and
is accepted only while no enabled rule targets either Worker:

```sh
npm run routing:foundation -- disable \
  --output /absolute/private/routing-foundation-disable-plan.json
npm run routing:foundation -- apply \
  --plan /absolute/private/routing-foundation-disable-plan.json \
  --plan-sha256 REVIEWED_DISABLE_SHA256 \
  --receipt-output /absolute/private/routing-foundation-disable-receipt.json
npm run routing:foundation -- status
```

A failed or ambiguous disable never auto-enables subaddressing. Do not use a
raw Cloudflare API call or dashboard edit for this setting.

Before preparing rules, deploy the byte-identical sorted canary-account CSV in
`CP_AGENT_EMAIL_MANAGED_DELIVERY_ACCOUNT_ALLOWLIST` and
`AGENT_EMAIL_MANAGED_DELIVERY_ACCOUNT_ALLOWLIST`. Keep edge canonical delivery
false. Stage the two control-plane canonical gates only through their atomic,
fenced helper:

```sh
npm run gates:canonical -- status
npm run gates:canonical -- enable \
  --output /absolute/private/canonical-gates-enable-plan.json
# Review deployment/release, all-binding and secret-name fingerprints, the
# exact Founder cohort digest, expiration, and printed plan SHA-256.
npm run gates:canonical -- apply \
  --plan /absolute/private/canonical-gates-enable-plan.json \
  --plan-sha256 REVIEWED_SHA256 \
  --receipt-output /absolute/private/canonical-gates-enable-receipt.json
npm run gates:canonical -- status
```

The enable plan requires both names absent and a one-account Founder cohort.
Apply holds `control_plane_canonical_gates_apply`, rechecks the exact plan just
before one Cloudflare bulk-secret PATCH, preserves every unrelated binding and
secret, and proves both gates present on the active successor. An ambiguous
enable attempts to delete both gates atomically; an ambiguous disable never
re-enables them. A durable pending receipt means reconcile live status before
retry. To roll back, create and apply a separate `disable` plan using the same
command and a new receipt path. Never edit one gate independently in the
dashboard or with Wrangler.

Deploy and verify the control-plane release containing
`control_plane_canonical_gates_apply` while both gates are still absent; only
then create the enable plan. The protected control-plane deploy path refuses
active email activation secrets. For a future control-plane release, first
make external delivery dark, use this helper to disable both gates, deploy and
verify, then create a fresh enable plan and reconverge inventory before
restoring external delivery.

After the enable receipt, let inventory converge. The authenticated
control-plane readiness endpoint
must report the same cohort count and SHA-256 as both active Worker bindings,
with canonical inventory and delivery true. Keep edge alias delivery false.

Only after that cohort and signed inventory have converged, validate the cell
artifact from `infra/cloudflare/agent-email`. The first command below must
report `ready_for_prepare: true` before a disabled-rule plan is created:

```sh
npm run routes:primary -- status /absolute/private/primary-canary.json
npm run routes:primary -- prepare /absolute/private/primary-canary.json \
  --output /absolute/private/primary-prepare-plan.json
# Review the mode-0600 plan, including target ids, route/role/catch-all hashes,
# cohort digest, gates, and every signed projection fence.
npm run routes:primary -- apply \
  --plan /absolute/private/primary-prepare-plan.json \
  --plan-sha256 REVIEWED_SHA256 \
  --receipt-output /absolute/private/primary-prepare-receipt.json
```

The status and planner compare each realm's authenticated control-plane route
with its isolated-KV row byte for byte, verify its trusted signature and fresh
applied canonical state, and prove its signed `account_id` is in the exact
deployed cohort. They also require ready Email Routing with subaddressing, a
disabled catch-all, and enabled forwarding rules for both operator role
addresses. Prepare writes no KV and cannot mutate the catch-all.

After the disabled rules are visible in Cloudflare, deploy the same reviewed
edge release with `REALM_EMAIL_CANONICAL_DELIVERY_ENABLED=true`, leaving alias
delivery false. Re-run status, then create and apply a new activation plan:

```sh
npm run routes:primary -- activate /absolute/private/primary-canary.json \
  --output /absolute/private/primary-activate-plan.json
npm run routes:primary -- apply \
  --plan /absolute/private/primary-activate-plan.json \
  --plan-sha256 REVIEWED_SHA256 \
  --receipt-output /absolute/private/primary-activate-receipt.json
npm run routes:primary -- status /absolute/private/primary-canary.json
```

Every plan expires after 15 minutes. Apply rejects a changed target, catch-all,
role route, managed rule, Worker release, gate, cohort, or projection. A partial
activation disables all owned canary rules. After the first active status, wait
at least 60 seconds, run status again, then send one synthetic message to one
exact canary address and verify the committed owner mailbox plus value-free
edge metrics. Disable and cleanup use separate reviewed plans and do not depend
on healthy projections or enabled delivery gates:

```sh
npm run routes:primary -- disable /absolute/private/primary-canary.json \
  --output /absolute/private/primary-disable-plan.json
npm run routes:primary -- apply \
  --plan /absolute/private/primary-disable-plan.json \
  --plan-sha256 REVIEWED_DISABLE_SHA256 \
  --receipt-output /absolute/private/primary-disable-receipt.json
npm run routes:primary -- status /absolute/private/primary-canary.json
# Only after status proves every owned rule disabled may removal be planned.
npm run routes:primary -- remove /absolute/private/primary-canary.json \
  --output /absolute/private/primary-remove-plan.json
npm run routes:primary -- apply \
  --plan /absolute/private/primary-remove-plan.json \
  --plan-sha256 REVIEWED_REMOVE_SHA256 \
  --receipt-output /absolute/private/primary-remove-receipt.json
npm run routes:primary -- status /absolute/private/primary-canary.json
```

Each primary apply receipt path must be canonical, absolute, private, and new.
The tool exclusively creates and fsyncs a complete pending marker before the
first Cloudflare mutation, then atomically replaces it with the verified
mode-`0600` receipt containing exact before/after rule evidence. If a crash or
failure leaves the pending marker, do not delete and retry blindly: inspect
live status, use the separately planned fail-closed disable path if needed, and
retain the marker with the incident record.

Do not plan a catch-all while the provider-contract blockers remain open. Once
an explicit review closes them, hash that external review record and create a
short-lived catch-all plan. Planning remains read-only:

```sh
npm run routes:catch-all -- status /absolute/private/primary-canary.json
npm run routes:catch-all -- enable /absolute/private/primary-canary.json \
  --change-id REVIEW_ID \
  --provider-review-sha256 REVIEW_RECORD_SHA256 \
  --output /absolute/private/catch-all-enable-plan.json
```

Apply requires the exact plan fence, a literal domain confirmation, and a new
mode-`0600` receipt path. It rechecks the active primary canary, cohort, gates,
projections, operator routes, and disabled catch-all before the only catch-all
mutation:

```sh
npm run routes:catch-all -- apply \
  --plan /absolute/private/catch-all-enable-plan.json \
  --plan-sha256 REVIEWED_SHA256 \
  --receipt-output /absolute/private/catch-all-enable-receipt.json \
  --confirm-enable-witmail-net
```

Apply exclusively creates and fsyncs a pending marker at the receipt path
before contacting Cloudflare. An existing or unwritable path therefore blocks
the mutation. If a crash leaves the pending marker, do not treat it as a
receipt: inspect live status and use the separately planned disable path before
operator cleanup.

If enable verification fails, the tool restores the exact disabled predecessor.
Plan rollback only from that protected enable receipt; rollback can target only
the disabled predecessor and never enables mail:

```sh
npm run routes:catch-all -- rollback /absolute/private/primary-canary.json \
  --receipt /absolute/private/catch-all-enable-receipt.json \
  --output /absolute/private/catch-all-rollback-plan.json
npm run routes:catch-all -- apply \
  --plan /absolute/private/catch-all-rollback-plan.json \
  --plan-sha256 REVIEWED_SHA256 \
  --receipt-output /absolute/private/catch-all-rollback-receipt.json
```

A separately reviewed disable is always the emergency path:

```sh
npm run routes:catch-all -- disable /absolute/private/primary-canary.json \
  --output /absolute/private/catch-all-disable-plan.json
npm run routes:catch-all -- apply \
  --plan /absolute/private/catch-all-disable-plan.json \
  --plan-sha256 REVIEWED_DISABLE_SHA256 \
  --receipt-output /absolute/private/catch-all-disable-receipt.json
```

Disable or rollback recovery never auto-enables a rule. Keep every manifest,
plan, receipt, external review, token, and account mapping outside Git and in
operator-controlled storage.

## Deploy, checkpoint, and drill the dark custom-domain authority

The deployed v0.0.238 procedure in this section is a control-plane-only rollout.
It covers both a fresh authority-journal bootstrap and later dark deployments
of the request, ownership-verification, plan, and account-lifecycle state
machine. It reuses the existing
`AgentEmailDomainRegistry` Durable Object class and fixed active object name
`global`; there is no new Durable Object migration, cell database migration,
cell selection, plan deployment, DNS operation, Email Routing change, or mail
delivery activation. The later schema-88 dark routing foundation is documented
separately below and is not part of this completed v0.0.238 procedure.

The release must contain the custom-domain journal/recovery runtime, external
administrator routes, Go client and `witself-admin` commands, and the private R2
binding. Verify those surfaces in:

- `infra/cloudflare/control-plane/src/agent-email-domain-journal.mjs`
- `infra/cloudflare/control-plane/src/agent-email-domain-journal-runtime.mjs`
- `infra/cloudflare/control-plane/src/agent-email-domain-runtime.mjs`
- `infra/cloudflare/control-plane/src/agent-email-domain-verification.mjs`
- `infra/cloudflare/control-plane/src/agent-email-domain-api.mjs`
- `infra/cloudflare/control-plane/src/agent-email-domain-recovery-api.mjs`
- `infra/cloudflare/control-plane/src/account-lifecycle-runtime.mjs`
- `infra/cloudflare/control-plane/src/bridge.mjs`
- `infra/cloudflare/control-plane/src/index.js`
- `infra/cloudflare/control-plane/wrangler.template.jsonc`
- `internal/client/agent_email_domain_recovery.go`
- `cmd/witself-admin/email_domain_cmd.go`

Keep the four v0.0.238 customer and DNS-observation controls absent during every
dark authority deployment:

- `CP_AGENT_EMAIL_CUSTOM_DOMAIN_REQUESTS_ENABLED`
- `CP_AGENT_EMAIL_CUSTOM_DOMAIN_REQUEST_ACCOUNT_ALLOWLIST`
- `CP_AGENT_EMAIL_CUSTOM_DOMAIN_AUTHORITY_READY`
- `CP_AGENT_EMAIL_CUSTOM_DOMAIN_VERIFICATION_ENABLED`

There is intentionally no wildcard account allowlist. Request creation must
fail unless the request gate, exact account membership, and authority-ready
gate all pass. It also requires the existing
`CP_PLAN_LIFECYCLE_ENABLED=true` durable scanner so an `awaiting_cell` intent
can recover after a lost bridge completion. Administrator and scheduled
verification must fail/no-op unless
the separate verification gate passes. Also keep every managed-domain
canonical/alias activation and delivery gate absent or exactly `false`.
Installing the distinct recovery secret enables none of these controls.

That managed-domain requirement records the original v0.0.238 journal-only
acceptance snapshot; it is not an instruction to remove an established alias
authority workflow during later signed-route releases. The current coordinated
readiness contract permits
`CP_REALM_EMAIL_ALIAS_ACTIVATION_ENABLED` to remain present while both edge
managed-delivery flags are exactly `false` and both canonical controls remain
absent.

For a fresh unjournaled registry, also keep
`CP_AGENT_EMAIL_DOMAIN_AUTHORITY_JOURNAL_ENABLED` absent until bootstrap
establishes a valid head. For an already bootstrapped production registry,
leave that journal-required secret exactly `true`; removing it would permit
journal-unaware authority writes. Do not run bootstrap again. Freeze
administrator request mutations for the short checkpoint/drill window so the
active head can be compared exactly before and after the drill.

The journal/recovery implementation supports at most 10,000 authority keys.
This is not a plan allowance. It is a hard recovery bound and therefore a
customer-request activation boundary. Each ordinary authority commit now uses
an exact count bound to the current journal head, applies only the normalized
after-image delta, and rejects an over-limit result before creating a pending
record or writing R2. If bootstrap or checkpoint reports
`agent_email_domain_journal_authority_limit_exceeded`, keep the request gate
dark and redesign or raise the reviewed recovery bound before accepting any
customer request.

Do not treat that static ceiling or successful admission as the whole
activation case. The admin status exposes only value-free head-bound counts and
a fixed category breakdown; an over-limit refusal emits one value-free
structured event. The durable claim/observe/commit flow resolves DNS outside
the global authority lane. A scheduled observation whose durable evidence
outcome is unchanged replaces one journal-local
`verification-refresh:<request_id>` record and its derived due entry. That
record carries the newest clocks, retry counter, recursive TTL, and schedule,
and list/show responses expose those effective values. It does not modify the
authority request or allocation, audit/meta keys, head-bound capacity, journal
head, or immutable R2 object count. The refresh generation is part of the
scheduled claim fence.

The first persisted refresh is also a forward-only activation boundary for
code versions that predate this key class, including v0.0.237. Those versions
cannot classify `verification-refresh:` and cannot preserve its effective due
entry. A dark deployment is rollback-safe while the verification gate remains
absent and no refresh exists. After verification is enabled, do not roll back
to a pre-refresh release. A supported downgrade would first need a bounded,
drilled drain that blocks new claims, removes every refresh and work record,
restores each request's authority-derived due entry, checkpoints the exact
head, and proves zero refresh keys plus exact derived parity. This release does
not expose that drain, so live activation must be treated as forward-only.

First observations, evidence/state changes, and every newly executed manual
check remain audited authority commits, so genuinely changing DNS can still
consume audit capacity until admission closes. Recovery intentionally drops
refresh and work records and reconstructs scheduling from journaled request
authority; an unchanged conservative repeat check recreates the single local
refresh. Keep both request and verification gates dark until these
refresh/claim fences have passed a controlled canary and exact `awaiting_cell`
plan recovery has been exercised through the durable plan workflow.
Custom-domain provider routing, cell projection, and mail delivery must be
accepted separately.

### Schema-88 dark routing acceptance: live controls remain off

This engineering slice, through `v0.0.240`, was not a production activation. It adds the cell
table/API, permanent sparse authority, durable source outboxes, a journal-local
leaf route intent, the schema-v1 unsigned inner route union carried by the
signed schema-v2 projection, and receipt provenance for
`agent.realm-alias@customer-domain`. The control-plane and edge code may be
deployed dark, but do not deploy schema 88 to a serving cell, enable a live
route, send a real canary, or change DNS, MX, Cloudflare Email Routing, a
catch-all, Worker association, or a customer zone during this acceptance pass.
The `v0.0.241` cell-receive procedure above supersedes only the schema-deployment
ban for its exact target cell. Every custom-domain and provider-activation ban
in this section remains in force.

In addition to the four v0.0.238 authority controls above, the new control-plane
release must prove that both routing controls are absent:

- `CP_AGENT_EMAIL_CUSTOM_DOMAIN_ROUTING_ENABLED`
- `CP_AGENT_EMAIL_CUSTOM_DOMAIN_ROUTING_ACCOUNT_ALLOWLIST`

The agent-email Worker must independently prove that
`AGENT_EMAIL_CUSTOM_DOMAIN_DELIVERY_ENABLED` is absent. The first two controls
must later be exact-true plus exact account membership, with no wildcard or
surrounding whitespace; the edge control must be the exact lowercase string
`true`. None is implied by request, verification, managed alias, canonical, or
receive-entitlement state.

The control-plane deployment guard now rejects any of its six custom-domain
secret names plus both canonical inventory/delivery controls. It intentionally
permits the existing alias-activation authority secret. Verify only secret
names, never values:

```sh
CP_DIR="${WITSELF_RELEASE_CHECKOUT:?set clean release checkout}/infra/cloudflare/control-plane"
EDGE_DIR="${WITSELF_RELEASE_CHECKOUT}/infra/cloudflare/agent-email"
(
  cd "$CP_DIR"
  npm run assert:custom-domain-dark
)

(
  cd "$EDGE_DIR"
  npm run assert:custom-domain-dark
)
```

Use repository fakes and a disposable test database to prove this exact order:

1. The control plane resolves one verified domain allocation and one existing
   realm-alias claim with the same account, exact realm, and exact label.
2. The domain registry journals the permanent sparse binding and its source
   outbox; the alias registry independently proves the claim and journals its
   permanent subscription marker.
3. Only after both acknowledgements does the domain registry durably stage the
   journal-local leaf intent keyed by domain request and realm label.
4. It POSTs the complete schema-88 projection to the account's cell and GETs the
   same request/claim identity back under the provision token.
5. It re-proves both source authorities, then publishes
   `email:realm-route:v1:<domain>:<realm-label>` with
   `route_kind=custom_domain` and the four request/allocation and
   claim/revision fences. Before either the control-plane response or KV write,
   it signs the complete scalar projection as schema version 2 with the active
   route-authority key.
6. The fake edge accepts only a fresh, trusted schema-version-2 signature and
   then the exact custom-domain variant when its test-only gate is exact true.
   Signature verification must finish before raw MIME is read. The fake cell
   derives both receipt ids from the existing signed envelope and local route
   rows; no new relay header is present.
7. Lost subscription/cell/KV acknowledgements replay durable work, stale authority
   prevents KV publication, and suspended/retired rows fail closed.

Run the complete suites rather than enabling any live control:

```sh
make check
(
  cd infra/cloudflare/control-plane
  npm test
)
(
  cd infra/cloudflare/agent-email
  npm test
)
```

Run the PostgreSQL custom-domain integration/archive/downgrade tests only with
`WITSELF_TEST_DATABASE_URL` pointing at a disposable database. Require schema
88, exact POST/readback replay, `custom_domain` message provenance with both
source ids, schema-88 archive round trip, schema-87 archive compatibility, and
fail-closed downgrade with any route or receipt. Delete only the disposable
test database when finished; never delete a retired route or message from a
serving cell to make a rollback pass.

Passing these tests makes the branch reviewable; it does not authorize a cell,
control-plane, or edge rollout. Provider onboarding, DNS/MX/Email Routing,
live route publication, live delivery, routing-account selection, and a real
canary remain separate human-assisted work.

For any dark deployment containing signed route projections, require the
control-plane plain variable `AGENT_EMAIL_ROUTE_SIGNING_KEY_ID`, its separately
stored `AGENT_EMAIL_ROUTE_ED25519_PRIVATE_KEY` secret, and an email-edge
`AGENT_EMAIL_ROUTE_ED25519_PUBLIC_KEYS` keyring containing that exact id. Inspect
secret names only; never print the private key. Keep canonical, alias, and
custom-domain delivery dark across the mixed-version interval. The control
plane must never publish an unsigned fallback: key configuration, import, or
signing failure is retryable `503 agent_email_route_signing_unavailable`, while
KV failure remains 502. The edge treats unsigned, unknown-key, malformed, and
modified projections as unusable and tempfails when no authenticated fallback
can replace them.

Provision the signer and shared fallback token only with the coordinated
ceremony. From one clean exact release tag, render both generated configs first
with edge alias/canonical delivery exactly `false`, canonical controls absent,
and all custom-domain controls absent. The existing
`CP_REALM_EMAIL_ALIAS_ACTIVATION_ENABLED` authority workflow may remain active;
it is not a mail-delivery gate. Then run from
`infra/cloudflare/agent-email`:

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

This command requires two existing Workers and the existing edge relay secret.
It preflights both live deployments and secret-name inventories, validates the
Witself reveal envelopes, proves the private/public Ed25519 keypair, and sends
values to Wrangler only over stdin with logs and metrics disabled. The validated
fallback token authenticates `route_signing_secret_provision` directly against
the canonical control-plane origin read from the live email edge; the token and
all endpoint-selector environment variables are removed from every Wrangler
child. Once the lease is held, the command reacquires the complete live/dark
fence, renews immediately before and after each bounded secret write, installs
the route private key on the control plane and one exact fallback token on both
Workers, re-inspects the resulting bindings, and renews before creating a
value-free v2 mode-`0600` receipt. If a sequential token put fails, keep every
delivery gate dark, correct the cause, and rerun. Independent edge secret-put npm
commands are not exposed and their old names are unknown to npm; binding
presence cannot prove that separately entered token values match. A first-ever
Worker bootstrap is a different, explicitly reviewed `--secrets-file`
procedure.

Rotate an existing cell-relay signing key with the dedicated edge-only ceremony,
never with an ad hoc Wrangler command. Create or select a Witself secret with
three distinct UTF-8 fields: a nonsensitive `text` key id, a nonsensitive
`text` canonical base64 raw 32-byte Ed25519 public key, and a
sensitive/redacted `private_key` containing the matching base64 PKCS#8 private
key. If the live control plane is still v0.0.240, only the exact v0.0.242
recovery deploy may use the bounded legacy-404 lease bootstrap; v0.0.241 never
reached a control-plane provider mutation, and v0.0.243 or later fails closed
on that legacy state. Deploy the exact v0.0.241-or-newer control plane dark
first, then deploy the same release of the edge dark while it still has the old
relay id/private key.
From the unchanged tag, re-render both target configs with empty managed
cohorts, both delivery flags false, and the desired new public `RELAY_KEY_ID`,
but do not deploy that edge config yet. Keep direct Cloudflare dashboard/API
routing and Worker mutation globally frozen, provide
`CONTROL_PLANE_EDGE_TOKEN` securely in the operator environment, and run from
`infra/cloudflare/agent-email`:

```sh
npm run provision:relay-signing-key -- \
  --agent AGENT \
  --relay-secret RELAY_SECRET \
  --relay-key-id-field KEY_ID_FIELD \
  --relay-public-field PUBLIC_KEY_FIELD \
  --relay-private-field PRIVATE_KEY_FIELD \
  --provider-zone-name witmail.net \
  --receipt /absolute/private/path/relay-signing-key-receipt.json
```

The command requires both exact live Workers and an already bound relay secret.
It proves the exact target version/commit/tag on the control plane and edge,
while requiring the live edge's old relay id to differ from the desired id.
`--provider-zone-name` defaults to `witmail.net`; Cloudflare must return that
exact active zone in the exact Worker account. An explicit `witwave.ai` target
is only legacy compatibility evidence and cannot satisfy primary-zone staging.
The `witmail.net` registrar move is complete and the zone is active in that
account; the Founder ceremony still requires every remaining dark-release and
operator-route prerequisite below.

The command freezes the two rendered target configs before validation in
separate unpredictable mode-`0700` directories with mode-`0400` files. All
Worker inspection and the secret write use only those private snapshots, which
are rechecked under the lease and removed on success or failure. It also strips
the deprecated `CF_ACCOUNT_ID` and `CF_API_TOKEN` aliases from every Wrangler
child; canonical `CLOUDFLARE_*` credentials remain the sole provider account
authority.

It refuses nonempty control-plane or edge cohorts, either enabled edge delivery
flag, a custom-domain delivery control, an enabled catch-all, any enabled owned
route, or any enabled Worker action targeting either
`witself-agent-email-receive` or the retired `witself-agent-email-pilot`.
Unrelated enabled Worker routes are permitted but included in the stable
provider-inventory fingerprint. It validates public field policy, reveals only
the private field, proves the private key derives the selected public key, then
acquires `relay_signing_key_provision` and reacquires every live/provider fence.
It reserves the receipt with a complete mode-`0600` value-free pending marker
before writing only `RELAY_ED25519_PRIVATE_KEY` over stdin. It changes no plain
variable. Success requires a new edge deployment and version, unchanged control
plane, unchanged non-secret edge resources, exact binding/inventory, unchanged
provider state, and a final lease fence. The atomically committed receipt binds
the prior/desired key ids, public-key digest, target release/config digests,
provider-zone/account digests, and successor ids, but contains no key value,
Witself secret id, field id, or source-secret reference.

The secret write creates an unannotated Worker successor. Immediately redeploy
the email edge from the same unchanged exact tag, then run coordinated
readiness and deploy the matching public key to the selected cell before any
cohort, delivery gate, or provider route is enabled. This command rotates an
existing binding only; use the separately reviewed complete `--secrets-file`
path for first-ever Worker bootstrap.

If a failure occurs after reservation, the pending marker remains and every
rerun refuses it. Keep the rollout dark and use the marker's predecessor ids and
non-secret digests to reconcile bindings, provider state, and any successor. If
no successor exists, preserve the marker in the private incident/change record
and rerun identical desired inputs at an empty receipt path. If a secret-only
successor exists, first redeploy the unchanged tag with the recorded prior
`RELAY_KEY_ID` to restore the tagged dark precondition, verify it, preserve the
marker, and then rerun the identical desired-key ceremony. Never delete or
overwrite a pending marker simply to unblock the command, and never treat it as
proof of the private value.

The desired fallback token must already match the live control-plane lease
credential. This ceremony therefore cannot rotate `CONTROL_PLANE_EDGE_TOKEN`:
changing the credential that authenticates acquire, renew, and release would
invalidate its own fence. Perform such a rotation only through the control-plane
package's explicit `secret:put:break-glass` path while globally freezing every
control-plane/email-edge deploy or rollback, route-signing ceremony,
routing-foundation/primary/catch-all apply, and direct Cloudflare dashboard/API
Worker mutation. Keep that freeze until both Workers and their exact tagged
verification have converged.

Every secret update creates a successor without the reviewed release
annotations. After the ceremony, deploy the unchanged exact tag to the control
plane first and then the email edge; the release commands enforce this order.
Each deploy renders and freezes a private per-invocation config, rechecks its
digest under the global operations lease, and removes it afterward; the shared
preview config cannot be raced into a provider upload. The edge deploy also
pins its lease to the exact `https://self.witwave.ai` authority.
Only then run readiness. Preserve the ceremony
receipt and both final deployment attestations together; the receipt proves
same-value upload while readiness proves final binding presence and release
identity.

After both exact tagged Workers are deployed, but before any delivery gate or
provider route is changed, run the coordinated value-free attestation from the
agent-email directory:

```sh
npm run verify:route-signing-readiness
```

It follows the control-plane and agent-email production deployments through
Wrangler JSON to one exact 100-percent version each. Require `outcome=verified`,
one shared immutable release identity, every `dark` field true, the active
signer id among `trusted_key_ids`, and every secret-binding presence field
true. The output intentionally omits key bytes and secret values. A secret-only
version does not retain the reviewed release annotations and therefore does not
pass this coordinated check; redeploy the same exact tagged release after any
such secret update. This attests matching configuration and binding presence,
not secret values. The coordinated ceremony receipt proves that the uploaded
private key derives the configured public key and that one exact fallback token
was sent to both Workers. Neither receipt proves live provider execution; prove
that only with the separately authorized signed-route canary while all live
delivery gates remain dark.

For an email-edge code rollback, create and review a two-phase fenced plan;
never deploy an arbitrary historical version directly:

```sh
cd infra/cloudflare/agent-email
npm run rollback -- \
  --candidate-version 01234567-89ab-4cde-8f01-23456789abcd \
  --output /absolute/private/path/agent-email-rollback.json
npm run rollback -- \
  --apply \
  --plan /absolute/private/path/agent-email-rollback.json \
  --plan-sha256 0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef
```

The first command creates a new mode-`0600` file and refuses overwrite. Review
the exact current/candidate versions, invariant checks, contract digest, and
`apply_fence.sha256`; the second command must repeat that exact fence. Apply is
interactive because Cloudflare versions carry secret state. Wrangler may ask
whether an older secret value should be restored; never accept an unexpected
secret difference. The tool rechecks the current deployment and candidate
contract before mutation, does not pass an automatic-confirmation flag, and
verifies the selected version at 100 percent afterward.

The first signed-route release cannot roll back to the preceding unsigned
email-edge version: its binding and signature contract is incompatible, so the
planner rejects it. Leave every delivery gate dark and forward-fix until an
older signed release with the exact same operational contract exists.

### Dark lifecycle and ownership-verification deployment

Use this path when the active registry is already bootstrapped and journal
enforcement is required. Before deployment, capture the exact control-plane
release identity, complete journal head, request/audit counts, and Worker
binding/secret names. For an exact v0.0.235 source, confirm journal status is
`enabled=true`, `required=true`, `pending=false`, and `forked=false`, with a
nonnull head whose sequence is positive. That source predates the `healthy`
and `remote_head_*` response fields; false or null values filled in by a newer
client from absent JSON are not evidence of degradation. Instead, independently
download and validate the complete create-only R2 chain through the captured
head, require its replayed head to match every captured head field exactly, and
require the replayed authority to match the zero-inventory preflight. Freeze
administrator mutations between that replay and deployment. This one-time
legacy-source check is not a waiver of the strict postdeploy health contract.

For every journal-aware source release, `pending=false` and `bootstrap=null`
are hard predeploy requirements, not only health observations. Pending records
written before the head-bound capacity release do not contain `capacity_after`,
and in-progress maintenance records do not contain the category breakdown;
new code refuses either incomplete old record rather than invent an atomic
capacity fence. Finish the old pending write or maintenance operation on the
old release before deploying. Never delete or rewrite either record.

Confirm all four dark controls above are absent from the persistent Worker
secret-name list, not only from rendered configuration. The pre-v0.0.236
authority has no migration
for the new account/domain and verification-due indexes: require exactly zero
existing request rows and exactly zero audit rows. Any nonzero request or audit
count is a hard deployment abort until an explicit migration is implemented
and reviewed. Do not attempt to infer a secret value by issuing a customer
request or verification mutation.

Capture and validate the secret names without printing their values:

```sh
DARK_CUSTOM_DOMAIN_NAMES='CP_AGENT_EMAIL_CUSTOM_DOMAIN_REQUESTS_ENABLED|CP_AGENT_EMAIL_CUSTOM_DOMAIN_REQUEST_ACCOUNT_ALLOWLIST|CP_AGENT_EMAIL_CUSTOM_DOMAIN_AUTHORITY_READY|CP_AGENT_EMAIL_CUSTOM_DOMAIN_VERIFICATION_ENABLED'
SECRET_INVENTORY="$(npm exec -- wrangler secret list --name witself-control-plane --format json)" || exit 1
jq -e 'type == "array" and all(.[]; type == "object" and (.name | type == "string"))' \
  <<<"$SECRET_INVENTORY" >/dev/null || exit 1
SECRET_NAMES="$(jq -r '.[].name' <<<"$SECRET_INVENTORY")" || exit 1
if grep -Eq "^(${DARK_CUSTOM_DOMAIN_NAMES})$" <<<"$SECRET_NAMES"; then
  echo "custom-domain activation secret is present; aborting dark deploy" >&2
  exit 1
fi
```

Deploy only a clean tagged control-plane release. Do not run
`npm run deploy:plans`, deploy the agent-email Worker, upgrade a cell, change
MX/TXT records, or alter Cloudflare Email Routing. The existing scheduled
handler may run, but with the verification gate absent it must return before a
DNS lookup. Plan and account-lifecycle reconciliation may be called by their
ordinary fenced workflows. While an account is not request-allowlisted, an
account with no active custom-domain request (including one with terminal
history only) must complete without creating new registry or journal state;
an already-pending intent still converges fail closed.

For this already-bootstrapped path, inspect rather than recreate the bucket and
recovery credential, perform deployment step 3, skip bootstrap and journal
enablement steps 4-5, then continue with checkpoint and recovery steps 6-8.

After deployment from a release that predates head-bound capacity, require all
of the following before appending the mandatory checkpoint:

- `/v1/version` is the exact new tagged release;
- the four dark controls remain absent and the journal-required secret remains
  present;
- raw journal-status JSON reports `remote_head_checked=true` and
  `remote_head_healthy=true`, while `.capacity.ready=false`,
  `.capacity.used=null`, `.capacity.remaining=null`, and
  `.capacity.max=10000`; `healthy=false` with
  `degradation_code=agent_email_domain_journal_capacity_unavailable` is the
  only accepted degradation, and the positive complete head is byte-for-byte
  unchanged;
- customer creation remains `custom_domain_requests_disabled`, administrator
  verification remains `custom_domain_verification_disabled`, exactly zero
  request and audit rows remain exactly zero, and the persistent Worker secret
  list again contains none of the four dark control names; and
- there was no DNS lookup, route publication, cell projection, Email Routing
  change, or mail delivery change.

Then append one complete checkpoint. Require it to materialize capacity bound
to the new exact head and return the journal to `healthy=true` before sealing
one newly named empty recovery target using the procedures below. Preserve the
exact `aedrec_` id, source head, action fences, sealed result, and post-drill
active head in the private rollout record. The active head must remain
unchanged during the recovery drill. Leave request, allowlist, authority-ready,
and verification controls absent after acceptance; this completes only the
dark deployment.

### 1. Provision and inspect the private R2 bucket

Use an authenticated Cloudflare operator in the production account. Install
the repository-pinned Wrangler version before issuing bucket commands:

```sh
REPO="${WITSELF_RELEASE_CHECKOUT:?set clean release checkout}"
CONTROL_PLANE_DIR="$REPO/infra/cloudflare/control-plane"
JOURNAL_BUCKET="witself-agent-email-domain-authority-journal"

cd "$CONTROL_PLANE_DIR"
npm ci

# Inspect first. Create only when Cloudflare confirms that it does not exist.
npm exec -- wrangler r2 bucket info "$JOURNAL_BUCKET"
npm exec -- wrangler r2 bucket create "$JOURNAL_BUCKET"

# The second info call is mandatory after creation.
npm exec -- wrangler r2 bucket info "$JOURNAL_BUCKET"
npm exec -- wrangler r2 bucket lifecycle list "$JOURNAL_BUCKET"
npm exec -- wrangler r2 bucket lock list "$JOURNAL_BUCKET"
npm exec -- wrangler r2 bucket dev-url get "$JOURNAL_BUCKET"
```

Do not run `bucket create` when the first `info` succeeds, and never delete or
recreate an existing bucket to make this step pass. The accepted initial
policy matches the established realm-alias journal: private access, r2.dev
disabled, no object-expiration rule, the default incomplete-multipart abort
rule, and no bucket-lock rule. If r2.dev is enabled, disable it and re-read the
status:

```sh
npm exec -- wrangler r2 bucket dev-url disable "$JOURNAL_BUCKET"
npm exec -- wrangler r2 bucket dev-url get "$JOURNAL_BUCKET"
```

Treat stronger object-lock/retention policy as a separate reviewed governance
change. Never add an expiration rule to the journal prefixes.

### 2. Install the independent recovery credential

The recovery credential must be distinct from the platform-admin, fleet,
edge, provisioning, and realm-alias recovery credentials. Keep the same bytes
in Cloudflare and in one operator-protected file. The CLI rejects symlinks,
non-regular files, and, on non-Windows systems, any group/world-readable mode.

```sh
RECOVERY_TOKEN_FILE="${AGENT_EMAIL_DOMAIN_RECOVERY_TOKEN_FILE:?set a new token-file path}"
umask 077
openssl rand -base64 48 | tr -d '\r\n' > "$RECOVERY_TOKEN_FILE"
chmod 600 "$RECOVERY_TOKEN_FILE"

cd "$CONTROL_PLANE_DIR"
npm exec -- wrangler secret put CP_AGENT_EMAIL_DOMAIN_RECOVERY_TOKEN \
  --name witself-control-plane < "$RECOVERY_TOKEN_FILE"
```

Do not print the file, place its value in an environment variable, or include
it in a ticket or rollout log. `wrangler secret list --name
witself-control-plane` may be used to confirm only the secret name.

### 3. Deploy exactly one tagged control-plane release

Use a clean checkout whose `HEAD` has exactly one semantic release tag. The
renderer enforces both conditions and embeds the immutable version, commit, and
date. Supply the existing dedicated agent-email KV namespace id; it must not be
the broad control-plane directory namespace.

```sh
cd "$REPO"
test -z "$(git status --porcelain)"
test "$(git tag --points-at HEAD --list 'v*' | wc -l | tr -d ' ')" = 1

cd "$CONTROL_PLANE_DIR"
EMAIL_DIRECTORY_KV_ID="${EMAIL_DIRECTORY_KV_ID:?set dedicated agent-email KV id}" \
  npm run deploy
```

`npm run deploy` renders `wrangler.generated.jsonc`, deploys the Worker and its
existing Durable Object class set, and runs deployment verification. Do not run
`npm run deploy:plans`; this slice changes no plan matrix. Do not deploy or
migrate any cell. Confirm the deployed configuration contains the
`AGENT_EMAIL_DOMAIN_AUTHORITY_JOURNAL` binding and does not contain any of the
four customer/verification controls above. Immediately repeat the persistent
`wrangler secret list` name-only check from the preflight and abort acceptance
if any dark-control name appears. For a fresh bootstrap, also confirm that the
journal-required gate is absent.

If deployment or verification fails before bootstrap starts, redeploy the
previous clean tagged control-plane release. Keep the new bucket and recovery
secret; neither changes authority by itself.

### 4. Bootstrap the existing fixed active registry

Use the released `witself-admin`. The ordinary admin token and distinct
recovery token are both required. The default admin-token file is allowed, but
the recovery file must remain owner-only.

```sh
CONTROL_PLANE_URL="${CONTROL_PLANE_URL:-https://self.witwave.ai}"
PLATFORM_ADMIN_TOKEN_FILE="${PLATFORM_ADMIN_TOKEN_FILE:?set admin token file}"
BOOTSTRAP_KEY="${BOOTSTRAP_KEY:?set one durable bootstrap idempotency key}"
BOOTSTRAP_REASON="${BOOTSTRAP_REASON:?set reviewed bootstrap reason}"

while :; do
  if ! RESPONSE="$(witself-admin email-domain journal bootstrap \
      --endpoint "$CONTROL_PLANE_URL" \
      --token-file "$PLATFORM_ADMIN_TOKEN_FILE" \
      --recovery-token-file "$RECOVERY_TOKEN_FILE" \
      --reason "$BOOTSTRAP_REASON" \
      --idempotency-key "$BOOTSTRAP_KEY" \
      --json)"; then
    echo "custom-domain journal bootstrap failed" >&2
    exit 1
  fi
  jq . <<<"$RESPONSE" || exit 1
  if jq -e '.complete == true' <<<"$RESPONSE" >/dev/null; then
    break
  fi
done
jq -e '.complete == true' <<<"$RESPONSE" >/dev/null || exit 1

witself-admin email-domain journal status \
  --endpoint "$CONTROL_PLANE_URL" \
  --token-file "$PLATFORM_ADMIN_TOKEN_FILE" \
  --recovery-token-file "$RECOVERY_TOKEN_FILE" \
  --json | jq .
```

Repeat the byte-equivalent bootstrap call with the same reason and idempotency
key until `complete=true`. An incomplete operation deliberately leaves
authority writes frozen. When complete, require `enabled=true`, `pending=false`,
`forked=false`, `healthy=true`, `.capacity.ready=true`,
status `.capacity.used` equal to the completed bootstrap response's
`authority_keys`, `.capacity.max == 10000`, and a fixed breakdown whose values
sum exactly to `.capacity.used`. Stop on an unknown
storage key, storage/authority limit, R2 failure, fork, fence mismatch, missing
capacity, or any other failed state; do not remove the bucket, reset the
Durable Object, or enable a gate to work around it.

Bootstrap creates the journal head even though the journal-required gate stays
absent. Once a valid head exists, this release writes every later authority
mutation R2-first. The absent gate merely avoids forcing an unjournaled
pre-existing registry before this deliberate bootstrap.

### 5. Enable journal enforcement only

After bootstrap reports a valid head with `pending=false` and `forked=false`,
install the exact-true enforcement secret:

```sh
cd "$CONTROL_PLANE_DIR"
printf '%s' 'true' | npm exec -- wrangler secret put \
  CP_AGENT_EMAIL_DOMAIN_AUTHORITY_JOURNAL_ENABLED \
  --name witself-control-plane

witself-admin email-domain journal status \
  --endpoint "$CONTROL_PLANE_URL" \
  --token-file "$PLATFORM_ADMIN_TOKEN_FILE" \
  --recovery-token-file "$RECOVERY_TOKEN_FILE" \
  --json | jq -e \
    '.enabled == true and .required == true and .healthy == true and
     .pending == false and .forked == false and .capacity.ready == true and
     .capacity.max == 10000'
```

This enables only fail-closed journal enforcement. It does not enable customer
requests, DNS verification, routing, projection, or delivery. Recheck the
control-plane release identity after the secret deployment. Stop if the
journal status is not exact or the release identity changes unexpectedly.

### 6. Append a complete checkpoint and capture an exact head

```sh
CHECKPOINT_KEY="${CHECKPOINT_KEY:?set one durable checkpoint idempotency key}"
CHECKPOINT_REASON="${CHECKPOINT_REASON:?set reviewed checkpoint reason}"

while :; do
  if ! RESPONSE="$(witself-admin email-domain journal checkpoint \
      --endpoint "$CONTROL_PLANE_URL" \
      --token-file "$PLATFORM_ADMIN_TOKEN_FILE" \
      --recovery-token-file "$RECOVERY_TOKEN_FILE" \
      --reason "$CHECKPOINT_REASON" \
      --idempotency-key "$CHECKPOINT_KEY" \
      --json)"; then
    echo "custom-domain journal checkpoint failed" >&2
    exit 1
  fi
  jq . <<<"$RESPONSE" || exit 1
  if jq -e '.complete == true' <<<"$RESPONSE" >/dev/null; then
    break
  fi
done
jq -e '.complete == true' <<<"$RESPONSE" >/dev/null || exit 1

ACTIVE_BEFORE="$(witself-admin email-domain journal status \
  --endpoint "$CONTROL_PLANE_URL" \
  --token-file "$PLATFORM_ADMIN_TOKEN_FILE" \
  --recovery-token-file "$RECOVERY_TOKEN_FILE" \
  --json)"
jq -e '
  .enabled == true and .required == true and .healthy == true and
  .pending == false and .forked == false and
  .capacity.ready == true and .capacity.max == 10000 and
  (.capacity.used | type == "number") and
  (.capacity.remaining == (10000 - .capacity.used)) and
  ([.capacity.breakdown[]] | add) == .capacity.used
' <<<"$ACTIVE_BEFORE" >/dev/null || exit 1
SOURCE_STREAM_ID="$(jq -er '.head.stream_id' <<<"$ACTIVE_BEFORE")"
EXPECTED_SEQUENCE="$(jq -er '.head.sequence' <<<"$ACTIVE_BEFORE")"
EXPECTED_HASH="$(jq -er '.head.hash' <<<"$ACTIVE_BEFORE")"
```

The recovery target is an exact journal head, not necessarily the checkpoint
entry itself. A later mutation head is valid if its replay chain contains the
complete checkpoint. This maintenance window freezes admin mutations, so the
captured status head should normally equal the checkpoint head. Never label a
guessed sequence or hash as the expected head.

### 7. Replay and seal one named empty recovery target

Generate one fresh id using the required `aedrec_` prefix and 16 lowercase
base32 characters, then start against the captured exact head:

```sh
RECOVERY_ID="aedrec_$(python3 -c \
  'import secrets; print("".join(secrets.choice("abcdefghijklmnopqrstuvwxyz234567") for _ in range(16)))')"
RECOVERY_REASON="${RECOVERY_REASON:?set reviewed drill reason}"
RECOVERY_START_KEY="${RECOVERY_START_KEY:?set one start idempotency key}"

witself-admin email-domain recovery start \
  --endpoint "$CONTROL_PLANE_URL" \
  --token-file "$PLATFORM_ADMIN_TOKEN_FILE" \
  --recovery-token-file "$RECOVERY_TOKEN_FILE" \
  --recovery "$RECOVERY_ID" \
  --source-stream "$SOURCE_STREAM_ID" \
  --expected-sequence "$EXPECTED_SEQUENCE" \
  --expected-hash "$EXPECTED_HASH" \
  --reason "$RECOVERY_REASON" \
  --idempotency-key "$RECOVERY_START_KEY" \
  --json | jq .
```

The only permitted target is `recovery:<recovery_id>` and it must be empty and
alarm-free. The start response supplies an opaque 64-lowercase-hex
`action_fence`. Use one operator and one serial action at a time.

For each replay page, read status, preserve the fence and idempotency key, then
advance once:

```sh
STEP=1
STATUS="$(witself-admin email-domain recovery status \
  --endpoint "$CONTROL_PLANE_URL" \
  --token-file "$PLATFORM_ADMIN_TOKEN_FILE" \
  --recovery-token-file "$RECOVERY_TOKEN_FILE" \
  --recovery "$RECOVERY_ID" --json)"
ACTION_FENCE="$(jq -er '.action_fence | select(test("^[0-9a-f]{64}$"))' \
  <<<"$STATUS")"
ADVANCE_KEY="$RECOVERY_ID-advance-$STEP"

witself-admin email-domain recovery advance \
  --endpoint "$CONTROL_PLANE_URL" \
  --token-file "$PLATFORM_ADMIN_TOKEN_FILE" \
  --recovery-token-file "$RECOVERY_TOKEN_FILE" \
  --recovery "$RECOVERY_ID" \
  --expected-action-fence "$ACTION_FENCE" \
  --idempotency-key "$ADVANCE_KEY" \
  --json | jq .
```

Repeat status plus one `advance` with a newly numbered key and the newly
returned fence until `phase` is `replayed`. If an acknowledgement is lost, do
not read a newer status and do not invent a new key: retry the exact command
with the saved key and fence. A persisted action rotates the fence; a stale
fence returns 409 without mutation.

Then use the same serial pattern with `verify` until `sealed=true`:

```sh
STEP=1
STATUS="$(witself-admin email-domain recovery status \
  --endpoint "$CONTROL_PLANE_URL" \
  --token-file "$PLATFORM_ADMIN_TOKEN_FILE" \
  --recovery-token-file "$RECOVERY_TOKEN_FILE" \
  --recovery "$RECOVERY_ID" --json)"
ACTION_FENCE="$(jq -er '.action_fence | select(test("^[0-9a-f]{64}$"))' \
  <<<"$STATUS")"
VERIFY_KEY="$RECOVERY_ID-verify-$STEP"

witself-admin email-domain recovery verify \
  --endpoint "$CONTROL_PLANE_URL" \
  --token-file "$PLATFORM_ADMIN_TOKEN_FILE" \
  --recovery-token-file "$RECOVERY_TOKEN_FILE" \
  --recovery "$RECOVERY_ID" \
  --expected-action-fence "$ACTION_FENCE" \
  --idempotency-key "$VERIFY_KEY" \
  --json | jq .
```

Stop immediately on `failed=true`, a missing complete checkpoint, sequence gap,
fork, digest mismatch, authority limit, nonempty target, invalid fence, or an
unexpected collision. Never delete or reuse that target. The sealed object is
restore-drill evidence only; there is no merge, promotion, active-object
selector, or cutover endpoint.

### 8. Prove non-activation and close the rollout

Read active status again and compare the complete `.head` with
`ACTIVE_BEFORE`. With administrator mutations frozen, they must be identical.
Confirm the sealed recovery status is still readable, r2.dev remains disabled,
all four request/allowlist/authority-ready/verification controls remain absent,
the journal-required status remains exactly true, and no DNS, Email Routing,
edge directory, cell, plan, or delivery configuration changed.
The customer request endpoint must continue returning
`custom_domain_requests_disabled` for an authenticated account operator, and
the administrator verification endpoint must continue returning
`custom_domain_verification_disabled`.

Leave `CP_AGENT_EMAIL_DOMAIN_AUTHORITY_JOURNAL_ENABLED` enabled after the drill;
once the journal head exists, journal-unaware writers are unsafe. Do not enable
the request gate, account allowlist, authority-ready gate, or verification gate
until head-bound capacity is ready and every remaining ownership, lifecycle,
projection, and receive-canary blocker in [agent-email.md](agent-email.md) is
closed through a separately reviewed canary.

### Stop and rollback rules

- The schema-88 custom-domain route foundation still has no live provider
  activation or destructive rollback procedure. The `v0.0.241` cell receive
  rollout above permits the schema on its exact serving-cell cohort but does
  not authorize customer-domain delivery. On a disposable test cell, an empty
  route table and zero custom-domain receipts permit the migration's normal down path. Once any
  route exists, including a retired tombstone, or any `custom_domain` receipt
  exists, schema 88 refuses downgrade before mutation. Leave schema 88 intact
  and roll application behavior forward; never delete authority or mail to
  force schema 87.
- A release that understands `verification-refresh:` may roll back to a
  pre-refresh release only while the verification gate has remained absent and
  an exact scan proves that no refresh record exists. After the first refresh
  write, block verification and roll forward; there is no supported in-place
  downgrade until the bounded drain described above exists and has been
  drilled.
- A dark lifecycle/verification deployment whose journal head and registry
  state remain unchanged may roll back to the immediately previous
  journal-aware release. Once a new request state, ownership observation,
  allocation, plan/lifecycle fence, due index, or related audit event is
  written, do not roll back to code that cannot classify and recover those
  keys. Block mutations and roll forward to compatible code.
- Before bootstrap begins, a code rollback to the previous clean release tag is
  safe. Keep the bucket and secret; they are inert without the new code.
- After bootstrap, the previous code is journal-unaware. It is an emergency
  rollback only while the customer request gate remains absent, all
  custom-domain administrator mutations are operationally blocked, and the
  active authority is proven unchanged. Restore service, then roll forward to
  journal-aware code before allowing any authority mutation.
- After any post-bootstrap authority mutation, or after this rollout enables
  the journal-required gate, do not deploy journal-unaware code. Roll
  forward. Otherwise a successful old-code write could exist without its R2
  after-image.
- Never delete or truncate the journal bucket, clear a pending/fork fence,
  mutate the fixed `global` object, delete a sealed recovery target, or treat a
  recovery object as active authority during rollback.
- Rotating a suspected recovery credential is independent of code rollback:
  replace the Worker secret and the owner-only operator file together, then
  verify dual authentication again.

## Bootstrap, checkpoint, and drill the realm-email-alias authority journal

This is a control-plane procedure. It does not restore a cell database and it
does not turn on email. Before starting, confirm the release configuration has
all of these gates absent or set to the exact string `false`:

- `CP_REALM_EMAIL_ALIAS_ACTIVATION_ENABLED`
- `REALM_EMAIL_ALIAS_DELIVERY_ENABLED`
- `CP_REALM_EMAIL_CANONICAL_INVENTORY_ENABLED`
- `CP_REALM_EMAIL_CANONICAL_DELIVERY_ENABLED`
- `REALM_EMAIL_CANONICAL_DELIVERY_ENABLED`
- `CP_REALM_EMAIL_ALIAS_AUTHORITY_JOURNAL_ENABLED` for the initial bootstrap

The control plane must already have the dedicated private
`witself-realm-email-alias-authority-journal` R2 bucket bound as
`REALM_EMAIL_ALIAS_AUTHORITY_JOURNAL`. Configure
`CP_REALM_EMAIL_ALIAS_RECOVERY_TOKEN` as a distinct Worker secret; do not reuse
the platform-admin token, fleet token, edge token, or a cell provision token.
The commands below intentionally read both credentials from operator-protected
files and never print them:

```sh
CONTROL_PLANE_URL="${CONTROL_PLANE_URL:?set control-plane URL}"
PLATFORM_ADMIN_TOKEN_FILE="${PLATFORM_ADMIN_TOKEN_FILE:?set admin token file}"
RECOVERY_TOKEN_FILE="${REALM_ALIAS_RECOVERY_TOKEN_FILE:?set token file}"
PLATFORM_ADMIN_TOKEN="$(tr -d '\r\n' < "$PLATFORM_ADMIN_TOKEN_FILE")"
REALM_ALIAS_RECOVERY_TOKEN="$(tr -d '\r\n' < "$RECOVERY_TOKEN_FILE")"
ADMIN_AUTH="Authorization: Bearer $PLATFORM_ADMIN_TOKEN"
RECOVERY_AUTH="X-Witself-Realm-Alias-Recovery: $REALM_ALIAS_RECOVERY_TOKEN"
JOURNAL_API="$CONTROL_PLANE_URL/v1/admin/realm-email-alias-journal"
RECOVERY_API="$CONTROL_PLANE_URL/v1/admin/realm-email-alias-recoveries"
```

Do not place either value in command history, a JSON body, a ticket, or a
rollout record. The normal admin bearer token and the distinct recovery header
are both required on every journal/recovery request.

### Bootstrap an existing active registry

Choose one durable idempotency key and reason. Repeat the byte-equivalent call
with the same key until `.complete` is `true`; every incomplete or failed step
leaves authority writes frozen. Never change the active registry object during
this loop. A `503` with code
`realm_email_alias_journal_operational_work_active` is the one pre-freeze
exception: an ordinary request or alarm was already in flight, no maintenance
state was installed, and the same call should be retried after that work ends.

```sh
BOOTSTRAP_KEY="${BOOTSTRAP_KEY:?set one durable bootstrap idempotency key}"
BOOTSTRAP_REASON="${BOOTSTRAP_REASON:?set reviewed bootstrap reason}"

while :; do
  REQUEST_BODY="$(jq -nc \
    --arg reason "$BOOTSTRAP_REASON" --arg key "$BOOTSTRAP_KEY" \
    '{reason:$reason,idempotency_key:$key}')" || exit 1
  if ! RESPONSE="$(curl --fail-with-body --silent --show-error \
      -H "$ADMIN_AUTH" \
      -H "$RECOVERY_AUTH" \
      -H 'Content-Type: application/json' \
      --data-binary "$REQUEST_BODY" \
      "$JOURNAL_API:bootstrap")"; then
    echo "realm-alias journal bootstrap failed" >&2
    exit 1
  fi
  jq . <<<"$RESPONSE" || exit 1
  if jq -e '.complete == true' <<<"$RESPONSE" >/dev/null; then
    break
  fi
done
jq -e '.complete == true' <<<"$RESPONSE" >/dev/null || exit 1
```

Then read status and record only the value-free stream id, sequence, hash,
authority epoch, registry revision, audit sequence, and scan counts:

```sh
curl --fail-with-body --silent --show-error \
  -H "$ADMIN_AUTH" \
  -H "$RECOVERY_AUTH" \
  "$JOURNAL_API" | jq .
```

Only after bootstrap reports `complete=true`, status reports `enabled=true`,
`pending=false`, and `forked=false`, and R2 retention/access controls have been
reviewed may a separate deployment review consider setting
`CP_REALM_EMAIL_ALIAS_AUTHORITY_JOURNAL_ENABLED=true`. That journal-only gate
does not authorize alias creation, canonical inventory, or any delivery gate.

### Append a complete checkpoint

Use one new idempotency key and repeat the same checkpoint request until
complete. Keep all email gates dark during a drill. The same pre-freeze
`realm_email_alias_journal_operational_work_active` response is safe to retry
with the identical key after the active work ends.

```sh
CHECKPOINT_KEY="${CHECKPOINT_KEY:?set one durable checkpoint idempotency key}"
CHECKPOINT_REASON="${CHECKPOINT_REASON:?set reviewed checkpoint reason}"

while :; do
  REQUEST_BODY="$(jq -nc \
    --arg reason "$CHECKPOINT_REASON" --arg key "$CHECKPOINT_KEY" \
    '{reason:$reason,idempotency_key:$key}')" || exit 1
  if ! RESPONSE="$(curl --fail-with-body --silent --show-error \
      -H "$ADMIN_AUTH" \
      -H "$RECOVERY_AUTH" \
      -H 'Content-Type: application/json' \
      --data-binary "$REQUEST_BODY" \
      "$JOURNAL_API:checkpoint")"; then
    echo "realm-alias journal checkpoint failed" >&2
    exit 1
  fi
  jq . <<<"$RESPONSE" || exit 1
  if jq -e '.complete == true' <<<"$RESPONSE" >/dev/null; then
    break
  fi
done
jq -e '.complete == true' <<<"$RESPONSE" >/dev/null || exit 1
```

Use the returned checkpoint head, not a guessed or older journal position, for
the recovery drill.

### Recover only into a named empty target

Choose a new valid `rear_` id, provide the checkpoint's exact `reaj_` stream,
sequence, and SHA-256 hash, and create the recovery. The service derives the
only permitted target name as `recovery:<recovery_id>` and refuses a nonempty
or differently bound Durable Object.

```sh
RECOVERY_ID="${RECOVERY_ID:?set a new rear_ id with 16 base32 characters}"
SOURCE_STREAM_ID="${SOURCE_STREAM_ID:?set exact checkpoint stream id}"
EXPECTED_SEQUENCE="${EXPECTED_SEQUENCE:?set exact checkpoint sequence}"
EXPECTED_HASH="${EXPECTED_HASH:?set exact checkpoint SHA-256}"
RECOVERY_REASON="${RECOVERY_REASON:?set reviewed drill reason}"
RECOVERY_START_KEY="${RECOVERY_START_KEY:?set one start idempotency key}"

jq -nc \
  --arg recovery_id "$RECOVERY_ID" \
  --arg stream "$SOURCE_STREAM_ID" \
  --argjson sequence "$EXPECTED_SEQUENCE" \
  --arg hash "$EXPECTED_HASH" \
  --arg reason "$RECOVERY_REASON" \
  --arg key "$RECOVERY_START_KEY" \
  '{recovery_id:$recovery_id,source_stream_id:$stream,
    expected_head:{sequence:$sequence,hash:$hash},reason:$reason,
    idempotency_key:$key}' |
  curl --fail-with-body --silent --show-error \
    -H "$ADMIN_AUTH" \
    -H "$RECOVERY_AUTH" \
    -H 'Content-Type: application/json' \
    --data-binary @- \
    "$RECOVERY_API" | jq .
```

The start response and every later status response expose the current opaque
`action_fence` as 64 lowercase hexadecimal characters. Drive the recovery from
one operator and issue only one action at a time. Each request must carry the
current fence; every successfully persisted action returns a different fence.
A 409 fence mismatch means the supplied fence is stale and nothing changed.

Advance one journal entry at a time. Preserve each generated request body
until its response is known. If the connection drops or the acknowledgement is
otherwise ambiguous, retry that byte-equivalent body before reading a newer
status or issuing another action. Only the immediately preceding identical
action can replay its durable result. The idempotency-key label is part of that
fenced request rather than a forever-reserved global key.

```sh
STEP=1
while :; do
  STATUS="$(curl --fail-with-body --silent --show-error \
    -H "$ADMIN_AUTH" \
    -H "$RECOVERY_AUTH" \
    "$RECOVERY_API/$RECOVERY_ID")" || break
  jq . <<<"$STATUS"
  jq -e '.failed == true' <<<"$STATUS" >/dev/null && break
  jq -e '.phase != "replay"' <<<"$STATUS" >/dev/null && break
  ACTION_FENCE="$(jq -er \
    '.action_fence | select(type == "string" and test("^[0-9a-f]{64}$"))' \
    <<<"$STATUS")" || break
  ADVANCE_KEY="${RECOVERY_ID}-advance-${STEP}"
  ADVANCE_BODY="$(jq -nc \
    --arg key "$ADVANCE_KEY" --arg fence "$ACTION_FENCE" \
    '{idempotency_key:$key,expected_action_fence:$fence}')"
  RESULT="$(curl --fail-with-body --silent --show-error \
      -H "$ADMIN_AUTH" \
      -H "$RECOVERY_AUTH" \
      -H 'Content-Type: application/json' \
      --data-binary "$ADVANCE_BODY" \
      "$RECOVERY_API/$RECOVERY_ID:advance")" || break
  jq . <<<"$RESULT"
  NEXT_ACTION_FENCE="$(jq -er \
    '.action_fence | select(type == "string" and test("^[0-9a-f]{64}$"))' \
    <<<"$RESULT")" || break
  test "$NEXT_ACTION_FENCE" != "$ACTION_FENCE" || break
  STEP=$((STEP + 1))
done
```

If `curl` exits without an authoritative HTTP response, do not regenerate
`ADVANCE_BODY`; retry exactly that saved body. A stored success returns its
already-rotated fence. A persisted deterministic failure likewise occupies the
immediate replay slot; its identical retry returns the same failure. An R2
unavailable error before local persistence leaves the fence unchanged.

Then repeat verification one page at a time until `sealed=true`. Verification
rebuilds derived state in bounded pages and checks the complete authority
digest and exact checkpoint fences. Apply the same lost-ack rule to
`VERIFY_BODY`.

```sh
STEP=1
while :; do
  STATUS="$(curl --fail-with-body --silent --show-error \
    -H "$ADMIN_AUTH" \
    -H "$RECOVERY_AUTH" \
    "$RECOVERY_API/$RECOVERY_ID")" || break
  jq . <<<"$STATUS"
  jq -e '.failed == true' <<<"$STATUS" >/dev/null && break
  jq -e '.sealed == true' <<<"$STATUS" >/dev/null && break
  ACTION_FENCE="$(jq -er \
    '.action_fence | select(type == "string" and test("^[0-9a-f]{64}$"))' \
    <<<"$STATUS")" || break
  VERIFY_KEY="${RECOVERY_ID}-verify-${STEP}"
  VERIFY_BODY="$(jq -nc \
    --arg key "$VERIFY_KEY" --arg fence "$ACTION_FENCE" \
    '{idempotency_key:$key,expected_action_fence:$fence}')"
  RESULT="$(curl --fail-with-body --silent --show-error \
      -H "$ADMIN_AUTH" \
      -H "$RECOVERY_AUTH" \
      -H 'Content-Type: application/json' \
      --data-binary "$VERIFY_BODY" \
      "$RECOVERY_API/$RECOVERY_ID:verify")" || break
  jq . <<<"$RESULT"
  jq -e '.failed == true' <<<"$RESULT" >/dev/null && break
  NEXT_ACTION_FENCE="$(jq -er \
    '.action_fence | select(type == "string" and test("^[0-9a-f]{64}$"))' \
    <<<"$RESULT")" || break
  test "$NEXT_ACTION_FENCE" != "$ACTION_FENCE" || break
  jq -e '.sealed == true' <<<"$RESULT" >/dev/null && break
  STEP=$((STEP + 1))
done
```

A legacy recovery whose status reports `action_fence: null` is readable for
diagnosis but cannot be advanced or verified; the action routes return 409.
Start a new recovery id rather than trying to upgrade that target in place.

Stop on any `failed`, `forked`, digest mismatch, gap, unexpected collision, or
nonempty-target response. Preserve the active object and all gates. A sealed
target is evidence for the drill only: there is no automatic cutover, no merge,
and no active-object selector. Production authority remains fixed to `global`.
Any future cutover requires a separately designed promotion protocol, incident
plan, and independent review.

## Operate periodic account backups

Periodic logical backups use the dedicated `witself-backups` R2 bucket and are
independent of cell-evacuation archives. Before activation:

1. Roll out the provider secret changes so every cell registration contains a
   distinct backup credential.
2. Enable `apps.witselfServer.backup.enabled` for source cells only after the
   referenced backup Secret contains `backup_token`. Provider-backed ESO cells
   use the extracted `witself-provision` Secret; Civo uses the additive
   `witself-backup` Secret so provisioning authority is never replaced.
3. Keep `apps.witselfServer.backup.validationEnabled=false` on serving cells.
   Enable it only on a registered, drained drill cell whose registry entry has
   `accepting=false` and no live account projections.
4. Create `witself-backups`, restrict its Worker/API access, and configure the
   multipart-abort and reviewed generation-retention rules. The initial
   development policy is 90-day expiration on `accounts/`, alongside the
   default seven-day incomplete-multipart abort rule. Do not lock the
   direct-write prefix: invalid completed objects must remain deletable for an
   exact-generation retry.
5. Leave the `CP_ACCOUNT_BACKUPS_ENABLED` Worker secret absent (or false) until
   one manual snapshot and one rollback-only drill have succeeded.

Fleet operations use the ordinary fleet bearer token:

```sh
CONTROL_PLANE="${WITSELF_CONTROL_PLANE_URL:?set control-plane URL}"
FLEET_TOKEN_FILE="${WITSELF_FLEET_TOKEN_FILE:?set fleet-token file}"
ACCOUNT_ID="${WITSELF_ACCOUNT_ID:?set account id}"
DRILL_CELL="${WITSELF_BACKUP_DRILL_CELL:?set accepting=false drill cell}"
FLEET_TOKEN="$(tr -d '\r\n' < "$FLEET_TOKEN_FILE")"

# The drill-cell infra profile must set backup_validation_target: true.
# Registration then atomically records that purpose and accepting=false.
curl --fail-with-body \
  -H "Authorization: Bearer ${FLEET_TOKEN}" \
  "${CONTROL_PLANE}/v1/cells" |
jq --arg name "$DRILL_CELL" '
  .cells[] |
  select(.name == $name) |
  {
    name,
    backup_validation_target,
    accepting,
    has_backup_token
  }'

# Inspect schedule/scan state, then inspect one account's immutable catalog.
curl --fail-with-body \
  -H "Authorization: Bearer ${FLEET_TOKEN}" \
  "${CONTROL_PLANE}/v1/backups/status"
curl --fail-with-body \
  -H "Authorization: Bearer ${FLEET_TOKEN}" \
  "${CONTROL_PLANE}/v1/backups/status?account_id=${ACCOUNT_ID}"

# Create one deterministic manual generation.
curl --fail-with-body -X POST \
  -H "Authorization: Bearer ${FLEET_TOKEN}" \
  -H "Content-Type: application/json" \
  --data "{\"account_id\":\"${ACCOUNT_ID}\"}" \
  "${CONTROL_PLANE}/v1/backups:run"
```

Copy the returned `backup_id`. If the run reports `retrying`, poll the
account-specific status endpoint until that exact id appears in the committed
catalog. Then run the isolated rollback-only restore drill:

```sh
BACKUP_ID="${WITSELF_BACKUP_ID:?set committed backup id}"

curl --fail-with-body -X POST \
  -H "Authorization: Bearer ${FLEET_TOKEN}" \
  -H "Content-Type: application/json" \
  --data "{\"account_id\":\"${ACCOUNT_ID}\",\"backup_id\":\"${BACKUP_ID}\",\"target_cell\":\"${DRILL_CELL}\"}" \
  "${CONTROL_PLANE}/v1/backups:restore-drill"
```

Success returns `validated: true` and records `validated_at` plus the drill
cell in that exact backup's catalog entry. Confirm the account still has no row
in the drill database and that its live directory route is unchanged. A generic
2xx from the cell is not accepted as proof.

After the manual path is healthy, activate the operator-controlled Worker
secret:

```sh
printf '%s' true |
  wrangler secret put CP_ACCOUNT_BACKUPS_ENABLED \
    --name witself-control-plane
```

Confirm `/v1/backups/status` reports `schedule.enabled=true`, then watch it
through at least one complete cursor scan. Set the secret to `false` (or delete
it, since absence is disabled) to stop future periodic scans. Do not
enable the schedule when the backup bucket, credential rollout, or drill-cell
is incomplete. Provider PostgreSQL PITR remains required.

## Bring archived accounts back onto a new cell

`witself-infra up -restore-archives` closes the loop: after the new cell
registers, it asks the control plane to restore each archive eligible for that
cell under the archive's placement policy (or the legacy same-region rule).
For a live account, the import preserves its credentials and portable data and
then removes the system suspension.

```sh
witself-infra up -restore-archives \
  -account-alias sandbox -aws-profile witwave-sandbox \
  -backend s3 -cloud aws -region us-west-2 -role dev \
  -control-plane https://self.witwave.ai \
  -fleet-token-file ~/.witself/tokens/fleet.token \
  ...  # the full up flag set
```

One line per account: `restored acc_… onto <cell>`. The loop ends with
`<cell>: N accounts restored from Cloudflare R2`. If no archive is eligible
for that cell, the up completes normally — a fresh cell with no eligible
archives is not an error.

A validated archive whose manifest status is `closed` remains the canonical
recovery artifact. A restore or replay may discover that status while importing
the archive's tombstone, but it retains both the R2 object and
`archived:<account>` discovery pointer and never publishes a live `acct:`
route. Once the pointer records `status: closed`, placement reports it as
retained rather than pending work. Cell teardown therefore cannot hide the only
portable copy of a closed account.

Placement-policy `allowed_regions` and `allowed_channels` lists are hard
eligibility filters (as is `allowed_clouds`); an empty list leaves that axis
unpinned. The corresponding `preferred_*` lists rank eligible cells in their
declared order but do not make an unlisted value ineligible. The control plane
compares cloud preference, then region preference, then channel preference,
then favors the less-loaded cell and uses cell name as a deterministic tie
breaker.

Archives created before placement policies use the legacy same-region guard:
`region_code` is compared when both archive and cell have one, otherwise their
provider region strings must match. An explicit `all_regions: true` restore
(the placement runner's `restore_any_region` option) bypasses only that legacy
guard. It does not override any policy's hard `allowed_*` filters.

If restore fails mid-flight (one account errors, or the cell endpoint is not
ready yet), the up command exits with the failure detail. Fix the reported
condition and re-run `up -restore-archives`. The account lifecycle Durable
Object resumes the same operation and evacuation id; already acknowledged
steps short-circuit. The Worker's `restore:<cell>` KV entry is a progress
projection, not lifecycle authority.

A deterministic gzip, tar, manifest, or checksum failure quarantines only that
exact archive identity. The control plane releases any exact target
reservation, retains the `archived:<account>` pointer and R2 object as recovery
evidence, and does not publish a live route. Repeating the same restore fails
fast without rereading or reserving it, so one corrupt archive cannot wedge the
placement queue. A transient R2/body-stream failure remains retryable and is
never quarantine evidence. To recover later, replace the archived projection
with a newly validated archive identity through a separately reviewed recovery
workflow; do not delete or rewrite the quarantined object in place.

## Diagnose an interrupted restore or source finalization

Successful restore no longer requires normal-path SQL cleanup on a losing
source cell. After the target import and resume are acknowledged and the target
route becomes authoritative, the account lifecycle coordinator calls
`:finalize-evacuation` on the original source with the exact account and
evacuation id. The source deletes the portable account rows transactionally
only when that row still has the matching `source` role and evacuation id, and
then records an idempotent finalization receipt. A retry with that same id
returns the receipt; a stale id, a different account, or a restored `target`
row cannot authorize deletion. A same-cell restore promotes the source row to
the target role without purging it.

If a restore or finalization reports an ambiguous outcome:

1. Stop cell teardown and retain both source databases and the R2 object.
2. Record the account id, evacuation id, source and target cell names, and
   their registration ids from the lifecycle/control-plane logs.
3. Check the public directory without changing it:

   ```sh
   curl https://self.witwave.ai/v1/directory/<account-id>
   ```

4. Confirm whether the target cell acknowledges the exact evacuation id and
   whether the source has an exact finalization receipt. Treat a stale
   `restoring:` or `restore:<cell>` KV record as a projection to reconcile, not
   proof that import or deletion committed.
5. After fixing reachability or version skew, retry the same restore/drain
   workflow so the Durable Object resumes its recorded phase.

Rows still visible on the old source after the directory points at the target
are not, by themselves, permission to delete anything. Do not manually import
the archive, issue ad hoc SQL deletes, remove `acct:` or `archived:` keys, or
delete the R2 object while lifecycle state is ambiguous. Escalate with the
captured ids and logs if exact acknowledgements disagree; direct database work
requires a separately reviewed recovery plan and backups.
