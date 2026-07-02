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

New accounts start **pending**: nothing works until activation (email
verification, eventually). Watch for it to flip to `active`:

```sh
ws account status --account test-account-1
```

## Close an account

Closing is permanent: every credential is revoked and the account is retired
(its record remains as a tombstone). On success the local name is removed too.

```sh
ws account close --account test-account-1 --yes
```

Add `--reason TEXT` to record why.

## Forget a stranded local account name

When an account is closed out from under the CLI — the pending-account reaper
retired it, or its cell was torn down — the local name lives on in
`~/.witself/config.json` with a dead token, and `ws account close` can no
longer authenticate to clean it up. Drop the local binding and token file only:

```sh
ws account forget --account test-account-1 --yes
```

This never contacts the server: an account that still exists stays open.
`--account` is required — forgetting never falls back to `WITSELF_ACCOUNT` or
`default`. Closing a live account is `ws account close`, which removes the
local name itself.
