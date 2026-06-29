# Witself Operator Authentication

Status: draft. Decision: managed v0 operator authentication is CLI-initiated,
browser/device-code based, and token-file friendly. The CLI must not collect
raw account passwords.

## Decision

Human operators should authenticate to managed Witself Cloud through a
CLI-initiated hosted login flow:

- Default: browser-based OAuth/OIDC-style flow with PKCE and a local callback
  when available.
- Headless fallback: device-code flow that prints a short code and verification
  URL.
- No raw account password collection in CLI flags, environment variables,
  config files, logs, shell history, or support bundles.
- No card, wallet, or payment credentials collected by the auth flow.

`witself setup` should start or resume operator authentication when a managed or
remote endpoint requires an operator principal before account, realm, agent, or
token creation.

Operator authentication establishes a human principal that manages a realm and
its agents. It is distinct from agent authentication: agents authenticate with
durable, token-file bearer credentials and never use a browser flow. The agent
token lifecycle is tracked in [token-lifecycle.md](token-lifecycle.md).

### Account context

Witself uses one model — account → realm → agent — everywhere, but the account
root means different things in managed and self-hosted deployments:

- Managed: the account is the customer and billing root. An operator signs up,
  the account is the unit that holds billing, support, and managed-admin
  capability, and its realms may be placed on different cells. The control plane
  resolves the realm/account to a home cell (endpoint + signing key) at login,
  and per-cell calls then go directly to that home cell; placement, resolution,
  and account-level billing aggregation across cells are defined in
  [deployment-cells.md](deployment-cells.md) and
  [billing-and-limits.md](billing-and-limits.md).
- Self-hosted: there is no signup. A single implicit deployment/org account root
  is created at bootstrap and serves as the realm parent for that deployment. The
  billing, support, and managed-admin capabilities are capability-gated off, so a
  self-hosted operator never sees account billing or signup surfaces and is not
  asked to choose or resolve a cell.

In both cases the operator does its day-to-day work in realms and agents, not in
the account root; the account context above only changes what the root carries
(billing/support/managed-admin vs. an implicit deployment root) and, for managed,
which home cell a realm resolves to.

## Stored Operator Auth

The CLI should store human operator auth material carefully:

- Prefer the operating system credential store when available.
- Fall back to an owner-only local auth file when no credential store is
  available.
- Use owner-only permissions for any auth file where the operating system
  supports them, such as POSIX mode `0600`.
- Never store raw account passwords.
- Clearly report the auth source in `witself auth status --show-source`.

Human operator sessions should be revocable. Expiration can be provider-driven,
but long-lived unattended automation should not depend on a human browser
session.

## Operator Authorization

Authentication proves who the human operator is. Authorization decides what that
operator may do inside a realm, and it is enforced below every frontend so the
CLI, MCP, and HTTP API reach the same decision. Operator authorization combines
two layers:

- Operator scopes carried by the authenticated operator principal, expressed as
  the same scope strings used elsewhere in Witself (for example `realm:admin`,
  `agent:manage`, `token:manage`, `policy:manage`, `group:manage`,
  `audit:read`, `account:manage`, `billing:manage`). The role/scope model,
  realm roles, scope bundles, and resolution algorithm span both the open plane
  (memories and facts) and the sealed plane (secrets and TOTP) and are defined
  in [authorization-and-roles.md](authorization-and-roles.md), summarized in
  [requirements.md](requirements.md).
- The declarative cross-agent access policy engine, which governs how any
  principal — agent or operator — reads, contributes to, curates, or forgets
  identity data that another agent or security group owns. This engine is the
  open-plane cross-agent authority, defined in
  [access-policy.md](access-policy.md); sealed-plane access uses secret grants
  and realm roles in
  [authorization-and-roles.md](authorization-and-roles.md) instead.

### Operator override

An authenticated operator can manage and access identity state across every
agent in the realm (operator override). Override does not bypass attribution:

- Operator override is audited exactly like an agent action, with the operator
  principal recorded as the actor.
- Destructive and cross-agent operator actions (`curate`, `forget`, hard
  delete, broad export) require an audit `--reason` and confirmation unless
  `--yes` is supplied by an authorized operator.
- Broad destructive actions require explicit targeting. An operator may list or
  scan `--all-agents`, but curate/forget/delete must name a specific
  `--owner-agent` or `--owner-group`, or an explicitly group-shared target.

### Policy and security groups

Operator authorization reuses the same declarative policy engine that governs
cross-agent access, rather than a separate admin grant model:

- Policies bind a subject (agent or security group) × permission × target
  (agent or security group), scoped to memories and/or facts, default deny. The
  policy model, permission verbs (`read`, `contribute`, `curate`, `forget`),
  and guardrails are defined in [access-policy.md](access-policy.md).
- Operators are the principals who create, list, show, delete, and test
  policies under `policy:manage`/`policy:read`. `witself policy test` is the
  canonical dry-run for whether a subject/permission/target would be allowed.
- Security groups are operator-managed sets of agents that act as both policy
  subjects and policy targets, and may own group-scoped shared memories and
  facts. Operators (and agents holding `group:manage`) manage membership. The
  group model is defined in [security-groups.md](security-groups.md).
- Operator identity, like agent identity, is derived from the authenticated
  principal, never from a caller-supplied name. Passing an agent or group name
  does not let an operator act as another principal beyond what their scopes and
  the realm's policies permit.

The threat framing for operator actions is integrity and authenticity of
identity data — memory-poisoning, unauthorized curation or forgetting, and
cross-agent write abuse — not secret confidentiality. The full framing is in
[threat-model.md](threat-model.md).

## Unattended Operator And Agent Auth

Unattended auth should use explicitly issued tokens:

- Agent runtimes use agent token files through `WITSELF_TOKEN_FILE`.
- Operator automation uses scoped operator/service tokens created by an
  authorized operator.
- Raw token values are returned only once and should be written directly to
  owner-only token files.
- Tokens are bearer credentials in v0, so deployment-native secret mounts are
  preferred over environment variables.

## Self-Hosted Bootstrap

Self-hosted deployments need a first-operator bootstrap path without default
passwords.

Recommended v0 posture:

- `witself-server bootstrap token` or an equivalent deployment command creates a
  one-time bootstrap token.
- In Kubernetes, the bootstrap token can be delivered as an existing Kubernetes
  Secret or printed once by an explicitly run admin job.
- `witself setup --endpoint URL --bootstrap-token-file PATH` uses that one-time
  token to create the first operator context, the single implicit deployment/org
  account root for that deployment, realm, agents, and durable token files. The
  account-level billing, support, and managed-admin capabilities are
  capability-gated off for self-hosted deployments (see Account context above).
- Bootstrap tokens should expire quickly, be single-use, and be audited.
- After the first operator exists, ordinary account, realm, agent, policy,
  group, and token management happens through the public `witself` CLI.

There should be no default admin username/password for self-hosted deployments.

## Local Development

Local development mode can use:

- Interactive passphrase prompt.
- `WITSELF_PASSPHRASE_FILE`.
- Local operator profile stored in the local development identity store.

The first local operator is created by `witself realm init` when the local store
is empty, and can then create named agents and write token files. Local auth is
explicitly development/test behavior and should not define the managed or
self-hosted production security model.

## Related Docs

- [requirements.md](requirements.md)
- [cli-command-surface.md](cli-command-surface.md)
- [api-contract.md](api-contract.md)
- [authorization-and-roles.md](authorization-and-roles.md)
- [access-policy.md](access-policy.md)
- [security-groups.md](security-groups.md)
- [token-lifecycle.md](token-lifecycle.md)
- [self-hosting.md](self-hosting.md)
- [threat-model.md](threat-model.md)
