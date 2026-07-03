# Runbooks

Hand-testing recipes for what is actually built and running. Grown one entry at
a time.

## Create an account on Witself Cloud

Requires an invite code.

```sh
ws account create --email scott@witwave.ai --invite friends-2026 --name test-account-1
```

The account is remembered locally as `test-account-1` (binding in
`~/.witself/config.json`, token under `~/.witself/tokens/accounts/`), so
follow-up commands are just `--account test-account-1`.

Leave off `--name` and the account is saved as `default` — the name every
command uses when `--account` is omitted.

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

## Check account status

New accounts start **pending**: nothing works until the emailed verification
link is clicked (`ws account resend-verification` sends a fresh one). Watch
for it to flip to `active`:

```sh
ws account status --account test-account-1
```

## List operators

Every account is born with one root operator, `owner` — the identity your
local token authenticates as. Operators you add later appear alongside it:

```sh
ws operator list --account test-account-1
```

One line per operator: id, display name, role, whether it is the root,
timestamps, and its live tokens.

## Create a backup operator token

A second credential for the same operator, so losing `owner.token` doesn't
lock you out:

```sh
ws token create --operator --name backup --account test-account-1
```

The token prints once — store it somewhere safe **off this machine**, like
1Password or another password manager. A backup that lives beside
`owner.token` disappears with it. Each token is independently revocable
(`ws token revoke --account test-account-1 --token tok_ID --yes`), so a
compromised one dies without touching the others.

## Close an account

Closing is permanent: every credential is revoked and the account is retired
(its record remains as a tombstone). On success the local name is removed too.

```sh
ws account close --account test-account-1 --yes
```

Add `--reason TEXT` to record why.
