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
follow-up commands are just:

```sh
ws realm list --account test-account-1
```

Leave off `--name` and the account is saved as `default` — the name every
command uses when `--account` is omitted.

## Close an account

Closing is permanent: every credential is revoked and the account is retired
(its record remains as a tombstone). On success the local name is removed too.

```sh
ws account close --account test-account-1 --yes
```

Add `--reason TEXT` to record why.
