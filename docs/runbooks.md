# Runbooks

Hand-testing recipes for what is actually built and running. Grown one entry at
a time. Commands here use the `default` account — what every command picks when
`--account` is omitted and `WITSELF_ACCOUNT` is unset. To juggle several
accounts, add `--name NAME` at create and `--account NAME` everywhere after.

## Create an account on Witself Cloud

Requires an invite code.

```sh
ws account create --email scott@witwave.ai --invite friends-2026
```

The account is remembered locally as `default` (binding in
`~/.witself/config.json`, token under `~/.witself/tokens/accounts/`).

## Check account status

New accounts start **pending**: nothing works until the emailed verification
link is clicked (`ws account resend-verification` sends a fresh one). Watch
for it to flip to `active`:

```sh
ws account status
```

## Create a realm and an agent

What an active account is for: realms partition the account, agents live in a
realm, and an agent token is the credential your agent actually runs with.
The ids come from each command's output.

```sh
ws realm create prod
ws agent create --realm realm_01xyz my-agent
ws token create --agent agt_01xyz
```

The agent token is written to
`~/.witself/tokens/accounts/default/agents/my-agent.token` — hand it to the
workload; `ws token revoke --token tok_ID --yes` kills it without touching
anything else.

## List operators

Every account is born with one root operator, `owner` — the identity your
local token authenticates as. Operators you add later appear alongside it:

```sh
ws operator list
```

## Create a backup operator token

A second credential for the same operator, so losing `owner.token` doesn't
lock you out:

```sh
ws token create --operator --name backup
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
ws token revoke --operator --name backup --yes
```

Revoking by name also removes the managed token file (revoking by id leaves
files alone). Any token (an agent's, another operator's) can be revoked by id
instead: `ws token revoke --token tok_ID --yes` — ids are in the last column
of `ws operator list`.

## Recover a lost owner token

Recovery proves inbox control: a code goes to the account's email, and
redeeming it rotates the owner's credentials — the old tokens die, agents and
other operators are untouched. Requesting a code changes nothing by itself.

```sh
ws account recover
# check the account's email for the code (valid ~15 minutes), then:
ws account recover --code 123-456-789
```

`ws account list` shows this machine's local names and account ids — handy
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
ws account adopt --id acc_01xyz --token-file teammate.token --name shared-account
```

The token is verified against the account's cell first — it must authenticate
and belong to `acc_01xyz`. On success the binding is saved like `ws account
create` would: follow-up commands are just `--account shared-account`.
`--name` is required; adopting never falls back to `default`.

## Change the account email

Owner-only; nothing else changes — tokens, operators, and agents all keep
working. Three emails tell the story:

1. **New address**: a confirmation code (proves the inbox can receive).
2. **Old address, immediately**: a warning that a change was requested — if it
   wasn't you, `ws account recover` rotates the owner credentials before the
   change can commit.
3. **Old address, after the commit**: a revert link valid for **48 hours**.
   Clicking it points the account back at the old address and kills any
   outstanding recovery code — the safety net if a stolen token moved the
   email out from under you. It refuses politely if the email has since
   changed again legitimately.

```sh
ws account change-email --new-email new@example.com
# check the new address for the code, then:
ws account change-email --new-email new@example.com --code 123-456-789
```

## Add a second operator

For a teammate (or automation that must survive an owner recovery). Your side:
create the operator with a short-lived transfer token —

```sh
ws operator create --name "Alice" --token-name alice-bootstrap --ttl 24h --out alice.token
```

— then send Alice two things over a channel you trust: the `alice.token` file
and this command (fill in your account id from `ws account list`):

```sh
ws account adopt --id acc_01xyz --token-file alice.token --name work
```

Her side: the adopt binds the account on her machine, then
`ws token create --operator --name laptop` mints her own durable token into
her managed path — a credential only she has ever seen. The transfer token
expires within 24 hours on its own.

`ws operator delete --yes opr_ID` retires an operator and revokes everything
it holds.

## Forget a stranded local name

When an account is closed out from under the CLI — the verification window
expired and the reaper took it — the local name lives on with a dead token,
and `ws account close` can no longer authenticate to clean it up. Drop the
local binding only (this never contacts the server):

```sh
ws account forget --account default --yes
```

## Suspend and resume an account

Suspend freezes every write on the account while keeping reads and credentials
alive — a reversible pause for time off, an audit, or a planned migration.

```sh
ws account suspend --yes                       # optionally --reason "on vacation"
# every domain command now refuses:
#   ws: account is suspended — this action requires an active account
# status still works, and shows why:
ws account status
ws account resume
```

Only the owner can suspend or resume their own suspension. Future non-owner
suspensions (planned: migration, fleet-admin, billing) will refuse `ws account
resume` — the authority that suspended is the one that resumes.

## Close an account

Closing is permanent: every credential is revoked and the account is retired
(its record remains as a tombstone). On success the local name is removed too.

```sh
ws account close --yes
```

Add `--reason TEXT` to record why.

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

Sandbox override: add `-destroy-accounts` to skip the archive step entirely
and force-purge the directory entries. This is an explicit acknowledgment
that every account on the cell dies with it — no restore is possible.

While the archives sit in R2 you can verify state at any time:

```sh
ws account status --account <name>          # says "archived — awaiting placement"
curl https://self.witwave.ai/v1/directory/<account-id>
```

## Bring archived accounts back onto a new cell

`witself-infra up -restore-archives` closes the loop: after the new cell
registers, it pulls every archived account whose recorded region matches this
cell's region back from R2 into the cell's local database and reactivates the
system-suspension. The user experience is invisible — old credentials still
work, memories/facts/agents come back byte-identical to the export.

```sh
witself-infra up -restore-archives \
  -account-alias sandbox -aws-profile witwave-sandbox \
  -backend s3 -cloud aws -region us-west-2 -role dev \
  -control-plane https://self.witwave.ai \
  -fleet-token-file ~/.witself/tokens/fleet.token \
  ...  # the full up flag set
```

One line per account: `restored acc_… onto <cell>`. The loop ends with
`<cell>: N accounts restored from Cloudflare R2`. If no archives match the
cell's region you see `no archived accounts awaiting placement in region
<region>` and the up completes normally — a fresh cell in a region with no
archives is not an error.

Placement rule: an archive is only restored into a cell in the SAME region it
was exported from. A us-west-2 account never silently lands in eu-central-1;
if you bring up an eu-central-1 cell with us-west-2 archives waiting, they
stay in R2 until a us-west-2 cell exists.

If restore fails mid-flight (one account errors, or the cell endpoint isn't
ready yet), the up command exits with the failure detail. Re-run `up
-restore-archives` after fixing — restoreAccount is idempotent per account,
so already-restored accounts short-circuit and only the remaining ones
finish. The Cloudflare Worker's `restore:<cell>` KV entry tracks
cross-invocation progress if you need to debug.

## Clean up a ghost restore

The restore path has a layered defense against two accepting cells in the
same region racing to import the same archived account: a `restoring:` KV
claim with a short TTL, and a re-check of `acct:` immediately before the
Worker writes it. When the re-check catches a race it throws:

```
restore race: acc_… routes to <winning-cell> — imported rows on <losing-cell>
are a ghost; see docs/runbooks.md#clean-up-a-ghost-restore
```

At this point the winning cell has the account and is serving it correctly.
The losing cell has a full copy of the account's data but no directory
pointer at it — a ghost. The ghost is unreachable through the fleet API
(the directory answers with the winner's endpoint), but its rows are still
sitting in the losing cell's database, so left in place they will show up
in the losing cell's next `:evacuate` and re-archive on top of a fresh
export from the winner. Left long enough, they diverge as mutable state
(memories, agents, secrets) rotates on the winner but not on the loser.

The manual recovery — until a per-account fleet-level evacuate verb exists
(see [#20](https://github.com/witwave-ai/witself/issues/20)) — is direct
Postgres cleanup on the losing cell. The account id, the losing cell name,
and the winning cell name all appear in the error message.

Before deleting anything, verify the account really is on both cells and
`acct:` really names the winner:

```sh
curl https://self.witwave.ai/v1/directory/<account-id>
# should return {cell: {cell: "<winning-cell>", ...}}
```

Then connect to the losing cell's database — the DSN is in the cell's
Kubernetes secret (`witself-server` chart) or the AWS Secrets Manager
entry `<cell>/db` published by `witself-infra`. In a single transaction,
delete the ghost rows in FK-reverse order:

```sql
BEGIN;
DELETE FROM tokens    WHERE account_id = '<account-id>';
DELETE FROM agents    WHERE realm_id IN (SELECT id FROM realms WHERE account_id = '<account-id>');
DELETE FROM realms    WHERE account_id = '<account-id>';
DELETE FROM operators WHERE account_id = '<account-id>';
DELETE FROM accounts  WHERE id = '<account-id>';
COMMIT;
```

Confirm the account still routes correctly at the winning cell:

```sh
ws account status --account <local-name>          # should show "active" via the winner
```

The ghost is now gone; the winning cell continues serving the account
unchanged.

If the losing cell is small and about to be destroyed anyway, an easier
route is `witself-infra destroy` on that whole cell — the ghost dies with
it, and its evacuation loop just skips the account (its `acct:` pointer
correctly names the winner, so evacuation finds nothing routing to the
losing cell).
