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
  RESPONSE="$(jq -nc \
    --arg reason "$BOOTSTRAP_REASON" --arg key "$BOOTSTRAP_KEY" \
    '{reason:$reason,idempotency_key:$key}' |
    curl --fail-with-body --silent --show-error \
      -H "$ADMIN_AUTH" \
      -H "$RECOVERY_AUTH" \
      -H 'Content-Type: application/json' \
      --data-binary @- \
      "$JOURNAL_API:bootstrap")" || break
  jq . <<<"$RESPONSE"
  jq -e '.complete == true' <<<"$RESPONSE" >/dev/null && break
done
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
  RESPONSE="$(jq -nc \
    --arg reason "$CHECKPOINT_REASON" --arg key "$CHECKPOINT_KEY" \
    '{reason:$reason,idempotency_key:$key}' |
    curl --fail-with-body --silent --show-error \
      -H "$ADMIN_AUTH" \
      -H "$RECOVERY_AUTH" \
      -H 'Content-Type: application/json' \
      --data-binary @- \
      "$JOURNAL_API:checkpoint")" || break
  jq . <<<"$RESPONSE"
  jq -e '.complete == true' <<<"$RESPONSE" >/dev/null && break
done
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
