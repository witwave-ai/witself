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

## Close an account

Closing is permanent: every credential is revoked and the account is retired
(its record remains as a tombstone). On success the local name is removed too.

```sh
ws account close --account test-account-1 --yes
```

Add `--reason TEXT` to record why.
