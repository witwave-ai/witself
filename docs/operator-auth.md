# Witself Operator Authentication

Status: the current CLI implements account onboarding, bootstrap-token exchange,
and operator/token lifecycle management. Hosted browser PKCE, device-code,
refreshable human sessions, and OS credential-store integration remain targets.
The `operator-auth-contract-reconciliation` gate records this current/target
contract boundary; it does not establish that the hosted authentication target
has shipped. See the [Feature Status](feature-status.md) scorecard.

## Implemented onboarding

### Managed account creation and adoption

Create a managed account with the shipped CLI:

```sh
witself account create --email owner@example.com --name work --accept-terms
witself account status --account work
```

`account create` accepts an optional invite and challenge token; the server
decides whether an invite or challenge is required. It submits signup consent
when `--accept-terms` is supplied, receives the assigned cell and bootstrap
token, exchanges that token at the cell, and saves the account and operator
credential under the local name. A pending account can obtain its credential
and check status before activation; ordinary management requires an active
account. The CLI retains a provision journal to retry the same signup while
the account remains pending and its bootstrap remains unconsumed, and to
resume local saving after the operator credential is durably journaled.
This is implemented account onboarding, not the target browser/device-code
operator login.

If the bootstrap exchange commits but its successful response is lost,
rerunning `account create` cannot recover the credential: provision replay
refuses the consumed bootstrap. The same limit applies if the CLI exits before
journaling the returned credential. Use the account ID printed before login
or included in the verification email. For a pending account, complete
activation through the verification email before requesting recovery; managed
recovery sends a code only for an active account. Then request and redeem an
emailed recovery code, saving the new credential under the local name:

```sh
witself account recover --id acc_ID
witself account recover --id acc_ID --code CODE --name work
```

Recovery revokes all prior owner tokens. See
[Recover a lost owner token](runbooks.md#recover-a-lost-owner-token).

To bind an existing account and operator credential on another machine:

```sh
witself account adopt --id acc_ID --token-file ./operator.token --name work
```

Adoption requires all three arguments. It looks up the account's cell through
the control plane and verifies that the token belongs to that account before
writing the local binding. Its optional `--endpoint` selects the control plane,
not a direct cell. For subsequent operator commands, `--account NAME` selects
a local account; absent that flag, the CLI uses `WITSELF_ACCOUNT` or `default`.
Without an explicit cell endpoint, the CLI looks up the account's current cell
on each connection. The direct self-hosted alternative is
`--endpoint URL --token-file FILE`.

Sources: `cmd/witself/main.go` (`accountCreateWithLegalVersions`, `accountRecover`,
`accountAdopt`, `connect`, and the source help table); `internal/store/auth.go`
(`ExchangeBootstrap`); `internal/store/provision.go`; `internal/store/recover.go`;
`infra/cloudflare/control-plane/src/index.js` (`handleVerify`, `handleRecover`);
`internal/server/server.go` (`requireOperator`).

### Self-hosted bootstrap

Generate the pre-shared bootstrap token with the client:

```sh
witself gen-bootstrap-token --out ./bootstrap.token
```

Generation alone does not create an account or authenticate: the server must
adopt the token and bind it to its seeded root operator. The store seeds a
default account and root operator, and stores a bootstrap-token hash with an
expiry. Re-adopting the same token does not extend its original expiry.

The Helm chart can mount a deployment-provided Kubernetes Secret using
`bootstrap.existingSecret.name` and `bootstrap.existingSecret.tokenKey`.
`bootstrap.tokenFile` defaults to `/.witself/tokens/bootstrap.token`; the chart
sets `WITSELF_BOOTSTRAP_TOKEN_FILE` to that path and optionally reads
`WITSELF_BOOTSTRAP_TOKEN_TTL` from the Secret's `ttlKey`.

After the deployment has adopted that token, exchange it:

```sh
witself auth login --endpoint https://witself.example.com \
  --bootstrap-token-file ./bootstrap.token --out ./operator.token
witself realm create --endpoint https://witself.example.com \
  --token-file ./operator.token default
```

The implemented exchange is `POST /v1/auth/bootstrap` with
`{"bootstrap_token":"<token>"}`. A successful response contains
`schema_version: "witself.v0"`, `operator_token`, and `operator_id`. The store
consumes the bootstrap once, atomically, and creates an operator token bound to
the same operator. The minted operator token has no expiry set by this
exchange. Unknown, expired, or already-consumed bootstrap credentials fail
the exchange. See the pinned [API authentication contract](api-contract.md#authentication).

`auth login` accepts only `--endpoint`, `--bootstrap-token-file`, and `--out`;
it writes the returned token to the requested file or prints it to stdout. It
does not create a named local account binding, realm, or agent. The CLI requests
mode `0600` when creating token output files. It does not provide an OS
credential-store login or `auth status --show-source` command.

Sources: `cmd/witself/main.go` (`genBootstrapToken`, `authLogin`, `realmCreate`);
`internal/store/seed.go`; `internal/store/auth.go` (`AdoptBootstrapToken`,
`ExchangeBootstrap`); `internal/server/server.go` (`bootstrapLoginHandler`);
`charts/witself-server/values.yaml` and
`charts/witself-server/templates/deployment.yaml`.

## Implemented principals and authorization

Operator bearer credentials resolve to an operator ID, account ID, and account
status. Agent bearer credentials resolve to one agent and its realm/account.
The server derives those identities from the token and stored relationships;
caller-supplied names do not change the actor.

The seeded root operator has role `account_owner`; `operator create` makes a
non-root `account_operator`. The role lookup treats the root bit as
authoritative, including for older imported rows. `GET /v1/whoami` is an
operator-only endpoint returning `schema_version` and a `principal` with
`kind`, `operator_id`, and `account_id`; `account_role` is included when the
role-lookup callback is configured. There is no `witself whoami` command.

The ordinary operator wrapper requires a live operator token and an active
account. Selected lifecycle and harm-reducing routes use an explicit
any-status wrapper and their own guards. These wrappers do not evaluate the
proposed `realm:admin`, `policy:manage`, or other role/scope bundles. Existing
owner checks are operation-specific; for example, the account-events route
requires owner authorization in addition to operator authentication.

Operator authentication does not grant the direct fact and memory routes to
every agent's identity data: the current fact and memory handlers reject
operator principals. It does grant whole-account export (`GET /v1/export`,
`requireOperatorAnyStatus`), whose archive includes every agent's fact values
and memory contents, sensitive records included, so an operator token must be
treated as able to read the account's complete content (see
[access-policy.md](access-policy.md)).
Policy evaluation, security-group ownership, cross-agent grants, and a blanket
operator override remain targets in [access-policy.md](access-policy.md) and
[security-groups.md](security-groups.md). Internal admin operations are also a
separate boundary: for example, the cell-wide events endpoint authenticates a
deployment provision token, not an ordinary account operator token.

Sources: `internal/store/auth.go` (`AuthenticateOperator`,
`AuthenticatePrincipal`, `GetOperatorAccountRole`); `internal/store/seed.go`;
`internal/store/operator.go`; `internal/server/server.go` (`requireOperator`,
`requireOperatorAnyStatus`, `whoamiHandler`, `listAccountEventsHandler`,
`eventsAdminCellHandler`); `internal/server/fact.go`; the
[API authentication contract](api-contract.md#authentication).

## Implemented operator and token lifecycle

The CLI provides:

| Command | Current behavior |
|---|---|
| `witself operator list` | Lists account operators, roles, root status, and live token metadata. |
| `witself operator create --name NAME` | Creates an additional operator and an initial operator token; accepts `--token-name`, `--ttl`, and `--out`. |
| `witself operator delete --yes OPERATOR` | Soft-deletes that account's operator and revokes its live tokens. The authenticated operator, root operator, and last live operator are protected. |
| `witself token create --operator` | Mints another token for the authenticated operator, with optional `--name`, `--ttl`, and `--out`; it does not create a new operator record. |
| `witself token create --agent AGENT` | Mints an agent token. Restricted `curator-preview` or `curator-apply` profiles require `--name` and `--ttl`; operator tokens only accept the `full` profile. |
| `witself token revoke --token TOKEN_ID --yes` | Revokes a token through authenticated account token management. |

Operator creation and token minting return raw token material at issuance;
operator lists contain metadata, not token values. Revocation and optional
operator-token expiry exist today. They must not be confused with the target
hosted-session refresh/logout/revocation flow.

Sources: `cmd/witself/main.go` (`operatorCmd`, `operatorList`, `operatorCreate`,
`operatorDelete`, `tokenCreate`, `tokenRevoke`); `internal/store/operator.go`;
`internal/store/auth.go` (`AuthenticateOperator`).

## Deferred hosted authentication and access policy

The hosted browser/PKCE and headless device-code flows, refreshable human
sessions, session status/logout/revocation, and OS credential-store integration
are **targets, not implemented CLI contracts**. The target browser/device-code
and no-password API posture is described in
[api-contract.md](api-contract.md#authentication). The current `auth` dispatch
accepts only `login` and implements the bootstrap-file exchange above.

The target managed authorization work depends on the access-policy rock:
realm role/scope bundles, policy/group administration, and cross-agent operator
authority require their own implemented contracts and tests. No command or
permission is granted by this document. The related gates are
`access-policy-contract-reconciliation` and `advanced-fact-policy`; hosted
authentication implementation remains tracked separately from
`operator-auth-contract-reconciliation` in the feature scorecard.

For local development, use an explicit endpoint with the bootstrap/API commands
above. The [API transport contract](api-contract.md#transport-requirements)
permits loopback HTTP for development. The CLI has no passphrase-based operator
login or `realm init` subcommand.

## Related docs

- [API contract](api-contract.md)
- [Access policy](access-policy.md)
- [Security groups](security-groups.md)
- [Token lifecycle](token-lifecycle.md)
- [Self-hosting](self-hosting.md)
- [Feature Status](feature-status.md)
