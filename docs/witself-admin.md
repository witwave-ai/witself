# witself-admin CLI reference

Status: implemented command reference. The inventory below follows the actual
[root dispatch](../cmd/witself-admin/main.go) and its nested command handlers.
`witself-admin` is the operator CLI for the control plane. Fleet queries may
fan out from the control plane to cells; the CLI selects a control-plane
endpoint. See the [customer CLI command surface](cli-command-surface.md) for
`witself` commands.

## Connection, credentials, and output

Put flags after the complete command name, for example
`witself-admin ticket list --json`. Network commands accept `--endpoint URL`;
the fallback is `WITSELF_CONTROL_PLANE`, then `https://self.witwave.ai`.
Managed token files live under `~/.witself/tokens`; `WITSELF_HOME` replaces
`~/.witself`.

| Credential | Resolution, highest priority first | Commands |
|---|---|---|
| Admin token | `--token`, `--token-file`, `WITSELF_ADMIN_TOKEN`, managed `admin.token` | `whoami`, tickets, account policies, email aliases/domains, `cells list`, events |
| Fleet token | `--fleet-token`, `WITSELF_FLEET_TOKEN`, managed `fleet.token` | `admin`, `invite`, `placement rescue`, `cells show` and repairs; see cell authentication below |
| Domain recovery token, in addition to the admin token | `--recovery-token-file`, `WITSELF_AGENT_EMAIL_DOMAIN_RECOVERY_TOKEN_FILE`, managed `agent-email-domain-recovery.token` | `email-domain journal` and `email-domain recovery` |

Recovery token files must be regular files, must not be symlinks, and must have
owner-only permissions on POSIX systems. Explicit token files override the
managed fallback.

```sh
witself-admin whoami --endpoint http://127.0.0.1:8080 \
  --token-file /run/secrets/witself-admin-token --json
witself-admin help
witself-admin version
```

`help`, `--help`, and `-h` at the root print usage; `version`, `--version`,
and `-v` print build information. Leaf commands expose their flags with
`--help`. Existing leaf handlers generally return usage status `2` after
printing flag help.

Most commands support `--json`. `ticket watch --json` and `events watch --json`
emit one JSON object per line. `admin delete`, the interactive dashboard,
root help, and version have no JSON flag. Diagnostics go to stderr; table
headers also go to stderr when stdout is piped. Exit status is `0` for
success, `1` for operation/authentication/transport failure or failed evidence
verification, and `2` for invalid arguments or missing local credentials.
These are the admin binary's exit codes, separate from the customer CLI's
larger exit-code table.

## Dashboard and TUI

[Handler](../cmd/witself-admin/tui_cmd.go).

```sh
witself-admin dashboard --interval 15s
witself-admin tui --interval 15s
```

`tui` is an alias of `dashboard`. The fullscreen dashboard exposes cells,
support tickets, and audit events. It runs this binary's CLI commands for
operations and live watches. Configure the endpoint and credentials through
the environment or managed token files: the dashboard itself accepts
`--interval`, not `--endpoint` or token flags. Release builds periodically
check for upgrades and can resume the active view after upgrading. `--resume`
is internal state used by that restart flow.

## Cells

[List handler](../cmd/witself-admin/fleet_cmd.go),
[repair handlers](../cmd/witself-admin/cells_cmd.go), and
[control-plane routes](../infra/cloudflare/control-plane/src/index.js).

```sh
witself-admin cells list --json
witself-admin cells show civo-example --token-file /run/secrets/fleet-token --json
witself-admin cells register civo-example \
  --cell-endpoint https://cell.example.com --cloud civo --region NYC1 \
  --region-code use1 --channel stable --weight 1 --accepting=false \
  --provision-token-file /run/secrets/cell-provision-token \
  --backup-token-file /run/secrets/cell-backup-token --json
witself-admin cells drain civo-example --json
witself-admin cells undrain civo-example --json
witself-admin cells deregister civo-example --yes --yes-cell civo-example --json
```

`list` uses the admin token and reports registration, live/archived account
counts, and cell version. The other five verbs use the fleet token:
`--fleet-token` or its `--token` alias, then `--token-file`, then
`WITSELF_FLEET_TOKEN`, then managed `fleet.token`. Do not combine
`--fleet-token` with `--token`. They do not fall back to an admin token.
Each verb accepts `--endpoint` and `--json`. Cell names contain 1–64 lowercase
letters, digits, or hyphens and may appear before or after the flags.
The five repair verbs return success for `--help`.
For these repair verbs, the control-plane endpoint must be an HTTP or HTTPS
origin, optionally ending in `/`, without a base path, credentials, query,
or fragment. This applies equally to `--endpoint` and `WITSELF_CONTROL_PLANE`.

| Verb | Control-plane requests | Result |
|---|---|---|
| `list` | `GET /v1/admin/cells` | Fleet operator view, JSON `{"cells":[...]}` |
| `show NAME` | `GET /v1/cells`, exact-name lookup | Registry entry |
| `register NAME` | `POST /v1/cells` | Upserted registry entry |
| `drain NAME` | `PATCH /v1/cells/{name}` with `{"accepting":false}` | Stop accepting placements |
| `undrain NAME` | `PATCH /v1/cells/{name}` with `{"accepting":true}` | Resume accepting placements |
| `deregister NAME` | Safe `DELETE /v1/cells/{name}` | Registry removal receipt |

`register` requires `--cell-endpoint`, an HTTPS URL without embedded
credentials, query, or fragment. `--endpoint` still selects the control plane.
Optional fields are `--cloud`, `--region`, `--region-code`, `--channel`
(`stable`, `edge`, `experimental`), positive finite `--weight` (default `1`),
and `--accepting` (default `false`). Registration upserts the supplied settings:
omitted cloud/region values are empty, the backup validation flag defaults to
`false`, and weight/accepting use the defaults above. Omitted region code and
channel retain the control plane's existing values or defaults (a new cell
defaults to the `experimental` channel). Include the
desired settings when repairing an existing entry. Credential files supply
provisioning/backup tokens; omitting those files preserves the stored
credentials. An accepting registration must meet the control plane's
provisioning-token requirement and, when backups are enabled, its distinct
backup-token requirement. Registration stays drained by default; explicitly
use `--accepting=true` or `undrain` when ready for new placements.
`--backup-validation-target` requires `--accepting=false`; such a
target cannot be undrained.

`drain` stops new placements while existing accounts remain. `undrain`
resumes placements. Both change only the coordinator's authoritative
`accepting` field, preserving its current registration metadata, backup
isolation marker, and stored credentials. The CLI does not replay a registry
snapshot. These verbs require the accepting-only PATCH route; an older
control plane's refusal is returned without falling back to registration.
`show`, `register`, `drain`, and `undrain` emit
`{"schema_version":"witself.v0","cell":{...}}` with `--json`. Cell credentials
are excluded from output.

`deregister` requires `--yes` and exact cell-name confirmation. At a terminal
it prompts for the name when `--yes-cell` is absent. Scripts must supply
`--yes --yes-cell NAME`; `--yes` alone is insufficient, and mismatched names
fail. After confirmation the CLI sends only the safe DELETE request. The
control plane independently requires a drained cell with no remaining
account directory entries; a serving or account-retaining cell is refused,
and the CLI surfaces the server's refusal text unchanged. Drain and move
accounts through the existing operator workflow before retrying. There is
no `--force` override or purge path in these repair commands. Successful JSON
is
`{"schema_version":"witself.v0","name":"civo-example","deleted":true}`.

## Events

[Handlers](../cmd/witself-admin/fleet_cmd.go). Both verbs use the admin token
and support `--verb` for an exact event verb and `--limit` for the per-cell
page size (1–500).

```sh
witself-admin events list --verb recovery.requested \
  --since 2026-09-01T00:00:00Z --limit 100 --json
witself-admin events watch --verb recovery.requested --interval 15s --json
```

`list` defaults to 50 rows per cell and accepts `--since` as RFC3339.
`watch` defaults to 100 rows per cell and a 30-second interval, with a
five-second minimum. The first poll establishes its starting position;
subsequent polls emit new events. A degraded poll holds the event position
for retry. Fleet results may be capped at 500 events; inspect the JSON cell
statuses and cap field, or the text-mode diagnostics, for partial coverage.

## Placement

[Handler](../cmd/witself-admin/placement_cmd.go). `rescue` uses the fleet token
to clear selected hard placement pins on an archived account.

```sh
witself-admin placement rescue --account-id ACCOUNT_ID \
  --axes cloud,region,channel --json
```

`--axes` defaults to all three axes; it accepts a comma-separated subset of
`cloud`, `region`, and `channel`. JSON wraps the receipt in `placement_rescue`
and includes whether anything changed.

## Backup evidence

[Handler](../cmd/witself-admin/backup_evidence_cmd.go). This command reads local
artifacts without network access or credentials.

```sh
witself-admin backup-evidence verify --release 0.0.267 \
  --cell civo-example --max-age 24h --json \
  --evidence-out ./backup-verification.json ./retained-backups
```

`verify` checks encrypted Civo pre-migration backup artifact triples against
the release, schema, restore-drill, checksum, and owner-only storage contract.
`--release` is required in `MAJOR.MINOR.PATCH` form without `v`; one or more
artifact directories follow the flags. Repeat `--cell` to select required
source cells; omitting it requires both reviewed Civo cells. `--max-age 0`
disables the age limit. The optional evidence output file is create-only,
mode `0600`. Both output modes contain counts and findings categories.

## Admin credentials

[Handlers](../cmd/witself-admin/main.go). These four verbs require the fleet
token. `whoami`, shown above, verifies an individual admin token.

```sh
witself-admin admin mint --handle operator --note "on-call operator" \
  --out ./operator.token --json
witself-admin admin list --json
witself-admin admin revoke --id ADMIN_ID --json
witself-admin admin delete --id ADMIN_ID --yes
```

`mint` requires `--handle`; its returned raw token is shown only at mint time.
By default it saves to the managed `admin.token` if that path does not already
exist. `--out` chooses an explicit destination. `revoke` disables the admin;
`delete` requires prior revocation and `--yes`, and prints text output.

## Signup invites

[Handlers](../cmd/witself-admin/invite_cmd.go). The command family is singular
`invite`; all six verbs use the fleet token and support JSON.

```sh
witself-admin invite list --json
witself-admin invite show example-invite --json
witself-admin invite create --code example-invite --max-uses 5 \
  --not-before 2026-09-04T00:00:00Z --expires 2026-09-11T00:00:00Z \
  --region NYC1 --note "operator-issued invite" --json
witself-admin invite disable example-invite --json
witself-admin invite enable example-invite --json
witself-admin invite delete example-invite --json
```

`create` generates a code when `--code` is omitted, or upserts the supplied
code while retaining its creation time and use count. Optional `--cell` pins
placement to a cell; `--region` constrains its region. `--max-uses` must be
positive when supplied. `show` includes current validity, exhaustion, expiry,
and enabled state. `disable`/`enable` retain usage. `delete` removes the invite
entry without deleting historical use records; it is idempotent and has no
`--yes` flag. Code-selecting verbs accept flags before or after the code.
Invite codes are included in output, so handle that output accordingly.

## Tickets

[Handlers](../cmd/witself-admin/main.go). All network ticket verbs use the
admin token. `states` reads the built-in state graph locally.

```sh
witself-admin ticket list --state open,awaiting_admin \
  --since 2026-09-01T00:00:00Z --limit 100 --json
witself-admin ticket watch --state awaiting_admin --interval 15s --json
witself-admin ticket show --account ACCOUNT_ID --ticket TICKET_ID --json
witself-admin ticket reply --account ACCOUNT_ID --ticket TICKET_ID \
  --body-file ./reply.txt --json
witself-admin ticket retriage --account ACCOUNT_ID --ticket TICKET_ID \
  --category technical --priority high --json
witself-admin ticket state --account ACCOUNT_ID --ticket TICKET_ID \
  --state awaiting_customer --json
witself-admin ticket resolve --account ACCOUNT_ID --ticket TICKET_ID --json
witself-admin ticket close --account ACCOUNT_ID --ticket TICKET_ID --json
witself-admin ticket states --json
```

`list` and `watch` default to all non-closed states and 100 rows per cell;
`--limit` is 1–500. Only `list` accepts `--since` (RFC3339). The watch interval
defaults to 30 seconds with a five-second minimum, and its first poll
establishes the starting position. Inspect cell statuses for partial results.

`show` reads the thread. `reply` requires exactly one body source:
`--body TEXT`, `--body-file PATH` (`-` means stdin), or `--stdin`.
`retriage` requires at least one of category (`technical`, `billing`,
`security`, `other`) or priority (`low`, `normal`, `high`, `urgent`).
`state` selects a target state; `resolve` and `close` select `resolved` and
`closed`. Legal transitions come from `states`; the server enforces them.

## Account policies

[Handlers](../cmd/witself-admin/main.go). All verbs use the admin token and
support JSON. `support-policy` reads by default and accepts `--set` to change
the value:

```sh
witself-admin account support-policy --account ACCOUNT_ID --json
witself-admin account support-policy --account ACCOUNT_ID --set disabled --json
```

The remaining eight families implement `get`, `set`, and `clear`. Every action
requires `--account`; every `set`/`clear` requires `--reason`. `clear` removes
the override so inherited policy applies. Read output includes the effective
account policy and propagation state; a pending cell application is reported
as an operation failure even when the control plane retained the override.

```sh
witself-admin account transcript-retention get --account ACCOUNT_ID --json
witself-admin account transcript-retention set --account ACCOUNT_ID \
  --days 365 --reason "approved retention exception" --json
witself-admin account transcript-retention clear --account ACCOUNT_ID \
  --reason "return to inherited retention" --json

witself-admin account messaging get --account ACCOUNT_ID --json
witself-admin account messaging set --account ACCOUNT_ID \
  --enabled --reason "approved messaging exception" --json
witself-admin account messaging clear --account ACCOUNT_ID \
  --reason "return to inherited messaging policy" --json

witself-admin account message-retention get --account ACCOUNT_ID --json
witself-admin account message-retention set --account ACCOUNT_ID \
  --days 365 --reason "approved retention exception" --json
witself-admin account message-retention clear --account ACCOUNT_ID \
  --reason "return to inherited retention" --json

witself-admin account email-receive get --account ACCOUNT_ID --json
witself-admin account email-receive set --account ACCOUNT_ID \
  --enabled --reason "approved inbound-email exception" --json
witself-admin account email-receive clear --account ACCOUNT_ID \
  --reason "return to inherited inbound-email policy" --json

witself-admin account email-send get --account ACCOUNT_ID --json
witself-admin account email-send set --account ACCOUNT_ID \
  --disabled --reason "outbound-email hold" --json
witself-admin account email-send clear --account ACCOUNT_ID \
  --reason "return to inherited outbound-email policy" --json

witself-admin account email-retention get --account ACCOUNT_ID --json
witself-admin account email-retention set --account ACCOUNT_ID \
  --indefinite --reason "approved retention exception" --json
witself-admin account email-retention clear --account ACCOUNT_ID \
  --reason "return to inherited retention" --json

witself-admin account plan-override get --account ACCOUNT_ID --json
witself-admin account plan-override set --account ACCOUNT_ID \
  --plan PLAN_ID --reason "approved classification exception" --json
witself-admin account plan-override clear --account ACCOUNT_ID \
  --reason "return to inherited plan" --json

witself-admin account limit-override get --account ACCOUNT_ID \
  --dimension message_sent_per_agent_minute --json
witself-admin account limit-override set --account ACCOUNT_ID \
  --dimension message_sent_per_agent_minute --max 1000 \
  --reason "approved throughput exception" --json
witself-admin account limit-override clear --account ACCOUNT_ID \
  --dimension message_sent_per_agent_minute --reason "return to inherited limit" --json
```

Retention `set` takes exactly one of `--days` (1–36500) or `--indefinite`.
Messaging and email direction `set` take exactly one of `--enabled` or
`--disabled`. `plan-override set` requires `--plan` and changes the effective
classification without creating or changing a provider subscription.
`limit-override` requires `--dimension` for all actions; `set` takes exactly
one of `--max N` (including zero) or `--unlimited`. See
[the limit-override contract](cli-command-surface.md#operator-only-throughput-overrides)
for dimensions and inherited breaker behavior.

## Email aliases

[Handlers](../cmd/witself-admin/email_alias_cmd.go). These verbs use the admin
token and support JSON. Every mutation requires `--reason` and accepts
`--idempotency-key`; the CLI generates a key when omitted.

```sh
witself-admin email-alias requests list --status pending_review --json
witself-admin email-alias requests approve --request REQUEST_ID \
  --reason "namespace review passed" --json
witself-admin email-alias requests reject --request REQUEST_ID \
  --reason "namespace review rejected" --json

witself-admin email-alias assignments list --account ACCOUNT_ID --json
witself-admin email-alias assignments suspend --alias example \
  --reason "operator hold" --json
witself-admin email-alias assignments reactivate --alias example \
  --reason "hold cleared" --json
witself-admin email-alias assignments retire --alias example \
  --reason "customer retired alias" --json
witself-admin email-alias assignments abort-provisioning --alias example \
  --reason "abandon stuck provisioning" --json
witself-admin email-alias assignments assign-internal \
  --account ACCOUNT_ID --realm REALM_ID --alias status \
  --reason "platform-owned route" --json

witself-admin email-alias reserved list --category operational_role --json
witself-admin email-alias reserved get --name status --json
witself-admin email-alias reserved add --name status \
  --category operational_role --internal-assignable true \
  --reason "protect service role" --json
witself-admin email-alias reserved update --name status \
  --internal-assignable false --reason "disable internal assignment" --json
witself-admin email-alias reserved retire --name status \
  --reason "reservation retired" --json
witself-admin email-alias audit --action alias.approved --limit 100 --json
```

Request and assignment lists accept `--status`, `--account`, `--realm`, and
`--cursor`. Reserved lists accept `--category`, `--enabled true|false`, and
`--cursor`; audit accepts `--action`, `--limit` (1–500), and `--cursor`.
Lists read one bounded page. Reuse `next_cursor` with the same filters until
it is empty; a filtered page may be empty while still having a next cursor.
Text output prints the next cursor to stderr.

`reserved get` reads one exact name and requires `--name`; category, enabled,
and cursor filters apply only to `reserved list`.

Reserved names block customer use. `--internal-assignable true` permits the
privileged internal assignment path. `reserved update` also accepts
`--category` and `--enabled true|false`. `abort-provisioning` terminally
abandons a hidden stuck provisioning intent; it does not retire an active
assignment. See the [alias administration contract](cli-command-surface.md#realm-email-alias-administration).

## Email domains

[Handlers](../cmd/witself-admin/email_domain_cmd.go). All verbs support JSON.
Request review and audit use the admin token.

```sh
witself-admin email-domain requests list --account ACCOUNT_ID \
  --domain mail.example.com --json
witself-admin email-domain requests show --request REQUEST_ID --json
witself-admin email-domain requests verify --request REQUEST_ID \
  --idempotency-key VERIFY_KEY --json
witself-admin email-domain requests reject --request REQUEST_ID \
  --reason "domain review rejected" --json
witself-admin email-domain requests retire --request REQUEST_ID \
  --reason "customer retired domain" --json
witself-admin email-domain audit --account ACCOUNT_ID --limit 100 --json
```

`requests list` accepts `--state`, `--account`, `--domain`, and `--cursor`.
`show` and `verify` select one request; verification requires an explicit
retry key. `reject` and `retire` require `--reason` and accept an optional
`--idempotency-key`, generated when omitted. Audit accepts `--action`,
`--account`, `--domain`, `--limit` (1–100), and `--cursor`. List/audit pagination
uses the same bounded-page and opaque `next_cursor` convention as aliases.

Journal and recovery commands additionally require the distinct recovery
token file described above. Mutating journal commands and `recovery start`
require both `--reason` and an explicit `--idempotency-key`.

```sh
witself-admin email-domain journal status --json
witself-admin email-domain journal bootstrap \
  --reason "initialize retained domain journal" --idempotency-key BOOTSTRAP_KEY --json
witself-admin email-domain journal checkpoint \
  --reason "retain verified domain checkpoint" --idempotency-key CHECKPOINT_KEY --json

witself-admin email-domain recovery start --recovery aedrec_EXACT_ID \
  --source-stream aedj_EXACT_STREAM --expected-sequence 42 \
  --expected-hash EXACT_HEAD_SHA256 --reason "recover domain registry" \
  --idempotency-key START_KEY --json
witself-admin email-domain recovery status --recovery aedrec_EXACT_ID --json
witself-admin email-domain recovery advance --recovery aedrec_EXACT_ID \
  --expected-action-fence EXACT_CURRENT_FENCE --idempotency-key ADVANCE_KEY --json
witself-admin email-domain recovery verify --recovery aedrec_EXACT_ID \
  --expected-action-fence EXACT_CURRENT_FENCE --idempotency-key VERIFY_KEY --json
```

Replace the illustrative stream, recovery, hash, and fence values with the
exact values for the retained journal and current recovery. `start` requires
a positive exact journal-head sequence and hash. `advance` and `verify`
require the current action fence and an explicit retry key; they do not
accept start-only flags or `--reason`. Read the returned status before the
next action. `recovery status` accepts the recovery selector without mutation
or start flags.
