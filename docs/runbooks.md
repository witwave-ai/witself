# Runbooks

Hand-testing recipes for what is actually built and running. Grown one entry at
a time. Commands here use the `default` account — the one every command picks
when `--account` is omitted. To juggle several accounts, add `--name NAME` at
create and `--account NAME` everywhere after.

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

The token prints once — store it somewhere safe **off this machine**, like
1Password or another password manager. A backup that lives beside
`owner.token` disappears with it. Each token is independently revocable
(`ws token revoke --token tok_ID --yes`), so a compromised one dies without
touching the others.

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

A confirmation code goes to the **new** address (proving it can receive), and
a notice goes to the current one. Owner-only; nothing else changes — tokens,
operators, and agents all keep working.

```sh
ws account change-email --new-email new@example.com
# check the new address for the code, then:
ws account change-email --new-email new@example.com --code 123-456-789
```

## Close an account

Closing is permanent: every credential is revoked and the account is retired
(its record remains as a tombstone). On success the local name is removed too.

```sh
ws account close --yes
```

Add `--reason TEXT` to record why.
