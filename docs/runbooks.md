# Runbooks

Hand-testing recipes for what is actually built and running. Grown one entry at
a time.

## Create an account on Witself Cloud

Requires an invite code.

```sh
ws account create --email scott@witwave.ai --invite friends-2026
```

Prints the account, the cell it landed on, and your operator token (add
`--out FILE` to save it instead), then a ready-to-run next command.
