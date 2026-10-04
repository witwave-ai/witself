# witself-infra

`witself-infra` provisions and manages **Witself cells**. A cell is one complete,
isolated Witself stack in a single cloud account/region. The same cell program
provisions a self-hoster's single cell and each cell in the Witself Cloud fleet —
only the stack config and who runs it (a human vs CI) differ.

## Why this is a separate module

This directory is its own Go module
(`github.com/witwave-ai/witself/infra/pulumi`), independent of the repo root. It
is built on the [Pulumi Automation API](https://www.pulumi.com/docs/iac/using-pulumi/automation-api/)
in inline-program mode: the cell definition is a Go closure compiled into the
`witself-infra` binary, so there is no project directory and you never invoke
`pulumi` yourself. The Automation API does drive the `pulumi` engine binary under
the hood (see Prerequisites).

Pulumi's provider SDKs are a large dependency tree. Keeping them in this nested
module means they never touch the lean `ws` and `witself-server` binaries, which
build from the repo-root module.

## Layout

```text
infra/pulumi/
  cmd/witself-infra/    # the CLI: up | preview | destroy | refresh | outputs |
                        #   config | whoami | dashboard | version | fleet verbs
  internal/backend/      # state backend bootstrap/lookup (AWS S3, GCP GCS, Azure Blob) and the R2 check
  internal/cell/        # the inline Pulumi program — the cell definition
  internal/fleet/       # control-plane fleet-registry client
```

## Prerequisites

The Automation API drives the `pulumi` engine binary, so it must be on `PATH`
(`brew install pulumi`). A planned follow-up has `witself-infra` install and pin
its own engine on first run (via `auto.NewPulumiCommand`), so the end user
installs only `witself-infra` — the engine is fetched like a provider plugin.

## The cell inventory (`~/.witself/infra.yaml`)

Cells can live in a local config file instead of being retyped as
flags. Record a cell once with the flags you already know, then every
verb takes `-cell`:

```sh
witself-infra config init
witself-infra config add-cell -cloud aws -account-alias sandbox \
  -region us-west-2 -role dev -aws-profile witwave-sandbox -argocd
witself-infra up -cell aws-sandbox-usw2-dev
witself-infra config show -cell aws-sandbox-usw2-dev   # effective merged config
```

Precedence: explicit flag > cell entry > `defaults:` block > built-in.

The optional per-cell `registry_name` identifies the fleet registry entry while
the inventory key remains the Pulumi stack name. `up` registers and probes the
cell under its registry name.

The inventory-only `deletion_protection` boolean uses cell entry >
`defaults.deletion_protection` > **true** when absent, for both `minimal` and
`prod` profiles. There is no flag override. For example:

```yaml
version: 1
defaults:
  deletion_protection: true
cells:
  aws-sandbox-usw2-dev:
    cloud: aws
    account_alias: sandbox
    region: us-west-2
    role: dev
    deletion_protection: true
```

Destroy refuses while this is true. Break-glass requires recording
`deletion_protection: false` on the cell and applying a **separate `up` before
`destroy`**. `up` reviews and consumes a saved plan, refusing protected
database/secret-store replacement, including protection adoption on old cells.
Azure purge protection remains irreversible. See
[Deletion protection and break-glass](../../docs/runbooks.md#deletion-protection-and-break-glass)
for provider primitives, the resource inventory, and the operator procedure.

The file holds references only — profile names, subscription/project IDs, token
file *paths*. Both the load and write paths reject anything shaped like a
credential, including a prefixed Cloudflare token (`cfut_`, `cfat_` or `cfk_`
followed by at least 20 letters, digits, `-` or `_`).

`config add-cell -dry-run` runs every check of `config add-cell`, prints the
entry it would add and writes nothing: no file and no directory. `-dry-run`
is valid only with `rebalance`, `config add-cell` and `config remove-cell`;
every other command refuses it, except `version` and `help`, which ignore
every flag.

`config remove-cell -cell NAME` removes one entry: after a completed
`destroy`, for a cell that no `up` has applied, or to record such a cell
again with corrected flags (`config add-cell` refuses a second record). It
refuses while the cell's control plane, taken from the entry or `defaults`
as `health` does, lists the cell under its `registry_name` or its inventory
name, and while the cell's stack holds any resource or pending operation.
The listing can lag: a cell that `destroy` removed moments ago can stay
listed briefly, so wait a minute and run the command again. It reads only
the backend that the entry names, and only a `local` or `r2` stack; it
creates no stack, state directory or passphrase and changes no stack or
kube context. An absent stack, or one that `destroy` emptied, passes.
Neither check sees an `up`, `preview` or `destroy` that has not written
state yet: do not run the command while one may be running for the cell.
An `r2` cell needs the three variables of
[Variables to export](#variables-to-export); without them the command
refuses. For an existing `local` stack, the check reads its passphrase from
`PULUMI_CONFIG_PASSPHRASE`, or, when that is unset or empty, from
`<state_dir>/passphrase` (`state_dir` defaults to `~/.witself-infra/state`);
Pulumi also honours `PULUMI_CONFIG_PASSPHRASE_FILE` from your environment.
Without a passphrase Pulumi can find, the stack export fails and the command
refuses to remove the entry, while an absent stack (no `<state_dir>/.pulumi`,
or a select-stack 404) needs no passphrase. Before it rewrites the file it
copies it byte for byte to `<file>.bak-<UTC time>` with mode 0600 and prints
the copy's path. If the
file changed while the checks ran, it writes nothing: run it again. The
rewrite drops YAML comments and any YAML document after the first; the
copy keeps them. `-dry-run` runs every check and writes neither the
inventory nor a backup. `-force` skips the registry and stack checks and
prints a warning: use it only after confirming by other means that the
cell has no cloud resources and no fleet registry entry. Once the entry is
gone, `health` no longer probes the cell and `destroy` refuses it. An
inventory that no longer loads must be repaired by hand from a copy.

Each cell can pin a **security context** — the identity its operations
must run as:

```yaml
security_context:
  aws:
    profile: witwave-sandbox
    expected_account_id: "537139788978"   # STS-verified before any op
  # azure: {subscription: ..., tenant: ...}
  # gcp:   {project: ..., credentials_file: ...}
```

`witself-infra whoami -cell X` resolves the context, calls the cloud
identity API, and refuses on a mismatch. The same check runs
automatically before `up`/`preview`/`destroy`/`refresh`/`bootstrap`
whenever `-cell` is used — a wrong profile fails in milliseconds, not
after twenty minutes of EKS provisioning.

## The dashboard

`witself-infra dashboard` is a fullscreen TUI. The left pane merges the cell
inventory with the default control plane's fleet registry. Active cells are
grouped under control-plane header rows; absent cells appear below a separator.
Status is live, draining, restore test, absent or error. Cells registered but
not configured also appear in the list, although the header's orphan count can
undercount them. The right pane is the context pane. A cell row has Overview,
Kubernetes, Database, Health and Logs tabs. A control-plane header has Overview
and Settings tabs, and a self-hosted header is untabbed.

While an operation runs, a one-line strip above the footer shows its verb,
cell, elapsed time and log path. Its output is in that cell's Logs tab. Each
operation tees its output to
`$WITSELF_HOME/logs/infra/<cell>-<verb>-<UTC time>.log`, with UTC time formatted
as `YYYYMMDDTHHMMSSZ`. `WITSELF_HOME` defaults to `~/.witself`; the log
directory is created with mode 0700 and the file with mode 0600. The Logs tab
lists the four newest logs for the cell and shows the selected one's tail.
A running operation, or the most recently completed operation, streams from its
memory buffer of the latest 2,000 lines; the log file retains the full output
when the tee succeeds.

The control plane's Settings tab edits runtime settings in three sections:
placement runner (enabled, restore archives, restore batch, restore
any-region, rebalance, rebalance batch), reaper (enabled, ttl (minutes)) and
placement (strategy `weighted` or `pinned`, pinned cell). Edits stay in a local
draft until applied. Apply re-reads the control plane, shows a diff and writes
only the changed sections after `y`. Quitting with `q` asks first when Settings
edits are unapplied; idle `ctrl+c` quits without asking.

These keys apply during normal navigation; confirmation dialogs and number
edits capture keys. Settings edits require focus in the context pane.

| Key | Where | Action |
| --- | --- | --- |
| `j`/`k`, `↓`/`↑` | anywhere | Move between rows (on a focused Settings tab: between fields) |
| `tab`, `shift+tab` | anywhere | Switch focus between the cell list and the context pane |
| `esc` | context pane | Return focus to the cell list |
| `←`/`→`, `h`/`l` | context pane | Previous or next tab |
| `p` | cell | Preview |
| `u` | cell | Up, only after a successful preview of that cell against the same config within the last 60 minutes; confirm with `y` or `enter` |
| `D` | cell | Destroy: runs inventory and account-placement checks first, then requires typing the cell name exactly and `enter` |
| `a` | cell | Run the cloud's login in the foreground (`aws sso login`, `gcloud auth application-default login`, `az login`); Civo has no interactive login |
| `g` | anywhere | Refresh |
| `[`, `]` | Logs tab | Older or newer log |
| `PgUp`, `PgDn`, `Home`, `End` | Logs tab | Scroll 10 lines, jump to the oldest line, return to the tail |
| `enter`, `space` | Settings tab | Toggle, cycle, or start a number edit (digits, `backspace`, `enter` to keep, `esc` to cancel) |
| `a` / `x` / `r` | Settings tab | Apply (diff, then `y`) / discard edits (`y`) / one runner pass now (`y`) |
| `q` | anywhere | Quit; refused while an operation runs; asks first with unapplied Settings edits |
| `ctrl+c` | anywhere | Quit without asking when idle; during an operation, `k` keeps it running and `c` cancels it with SIGKILL to its process group (detach is not supported) |

Operations run as subprocesses of the same binary for installed builds, or
rerun the same program from source for `go run` builds, so the dashboard drives
exactly what scripts drive.

Civo is a first-class dashboard provider. Its overview shows the effective
node size, Kubernetes version policy, API firewall CIDR, native Civo DNS, and
Traefik NodePort ingress instead of displaying the managed-database, VPC CIDR,
and delegated-domain settings used by the hyperscalers. Its Health tab probes
the K3s API, node readiness, Argo applications, public endpoint, control-plane
registration, and Civo token authentication.

For a cell recorded with `civo_ingress: loadbalancer` the overview shows the
load balancer, who manages the DNS record and the parent domain instead of the
native Civo DNS row.

`-progress-json` on `up`/`preview`/`destroy` additionally emits NDJSON
phase events on stderr (`{"ts","phase","state","cell","note"}`) for
machine consumers.

## Health probes and alerting

`witself-infra health --json` probes configured cells and their control planes
concurrently. Each target has its own `-timeout` deadline (default `5s`), so a
hung endpoint is emitted as `timeout` without blocking sibling probes. The
command writes one NDJSON object per target with exactly `name`, `state` (`ok`,
`degraded`, `timeout`, or `down`), `latency_ms`, and `checked_at`, plus
`registry_name` for a cell whose inventory entry sets one. It exits zero only
when every target is `ok`, and exits nonzero after emitting all records
otherwise. It probes cells only through the control plane and reads no Pulumi
state and no kubeconfig. A cell with no control plane in its entry or in
`defaults`, or that the control plane does not know (no `up` has registered it
yet, or `destroy` removed it), is reported `down`, so such an entry makes the
command exit nonzero. Piping this command into cron or a monitoring agent and
alerting on its exit status or records is the intended hook; `witself-infra`
does not integrate directly with PagerDuty or any other external alerting
service.

## Run it

```sh
# build
go build -o bin/witself-infra ./cmd/witself-infra

# the cell name is composed from components: <cloud>-<account-alias>-<region-code>-<role>
# e.g. these flags -> cell aws-sandbox-usw2-dev, resources witself-aws-sandbox-usw2-dev-*
# creds come from -aws-profile (or the ambient AWS chain / OIDC).
F="-cloud aws -account-alias sandbox -region us-west-2 -role dev -aws-profile witwave-sandbox"

# state lives in S3 by default — create the per-account+region backend once:
./bin/witself-infra bootstrap -cloud aws -region us-west-2 -aws-profile witwave-sandbox

# then the cell loop (S3 is the default; add -backend local for a no-AWS dev run)
./bin/witself-infra preview $F
./bin/witself-infra up      $F
./bin/witself-infra outputs $F
# Before destroy: save the cell in infra.yaml, record deletion_protection: false,
# and apply a separate up -cell CELL (see the break-glass runbook).
./bin/witself-infra destroy $F
```

### Civo production cells

Civo uses an inexpensive K3s worker pool, native Civo DNS, Traefik NodePort
ingress, cert-manager, and an in-cluster PostgreSQL volume. The control plane is
free and no managed load balancer is created. A token is read from a per-cell
mode-0600 file (recommended for multi-account operation) or from the
`CIVO_TOKEN` environment fallback. Its value is never written to `infra.yaml`
or Pulumi config. `witself-infra` replaces the value of `CIVO_TOKEN`, and a
token that it read from a token file, with `[redacted CIVO_TOKEN]` in its
final error line, in progress events and in the stack-output error of the
`cell-health` report. `cell-health` never reads the token file.

That is the default shape. A cell can opt in to one Civo load balancer, a
custom host name and a DNS record; see
[Civo load balancer and custom host name](#civo-load-balancer-and-custom-host-name-opt-in).

The managed production fleet currently uses two Civo cells in NYC1:
`civo-sandbox-use1-serving` is the serving cell and runs the fixed two-node
`prod` profile; `civo-sandbox-use1-backup` is the non-accepting,
rollback-only backup-restore drill target. Both are production-operated cells;
the latter is deliberately excluded from customer placement. See the
[recovery note](../../docs/deployment-cells.md) for the retained registry name.

```sh
install -m 600 /dev/null "$HOME/.witself/tokens/civo-sandbox.token"
# Write the Civo API token into that file using your password manager/editor.

./bin/witself-infra config add-cell \
  -cloud civo -account-alias sandbox -region nyc1 -role serving -profile prod \
  -backend local \
  -state-dir "$HOME/.witself/infra-state/civo-sandbox-use1-serving" \
  -civo-token-file "$HOME/.witself/tokens/civo-sandbox.token" \
  -civo-expected-account-id 00000000-0000-0000-0000-000000000000 \
  -civo-node-size g4s.kube.medium \
  -civo-admin-cidr 203.0.113.7/32 \
  -argocd \
  -control-plane https://self.witwave.ai

./bin/witself-infra whoami -cell civo-sandbox-use1-serving
./bin/witself-infra preview -cell civo-sandbox-use1-serving
./bin/witself-infra up -cell civo-sandbox-use1-serving
./bin/witself-infra cell-health -cell civo-sandbox-use1-serving
./bin/witself-infra dashboard
```

Persist the Civo API version in each cell's `k8s_version` field in
`~/.witself/infra.yaml`, alongside `civo_node_size`, to keep the pin across
`preview`, `up`, and health reads. For a cluster whose kubelets report
`v1.35.0+k3s1`, the Civo version name is `1.35.0-k3s1`:

```yaml
    k8s_version: "1.35.0-k3s1"
```

The optional field participates in the inventory merge: explicit
`-k8s-version` > cell record > `defaults:`. With no configured version, Civo's
`KubernetesVersion` input remains absent and the API chooses its latest stable
K3s release. A flag override applies only to that invocation; `up` and
`preview` do not save it to the inventory. `config add-cell -k8s-version ...`
persists it when creating a new record; edit an existing record to pin it.

Pinning the currently running version must produce **no changes** in
`witself-infra preview -cell CELL`; stop if it proposes an update or replacement.
A differing pin is a cluster upgrade and must follow
[K3s minor upgrade (Civo)](../../docs/runbooks.md#k3s-minor-upgrade-civo),
which includes version-format sources and the backup-first sequence.
The Civo stack exports the provider-reported `kubernetesVersion` for
`cell-health` and the dashboard. The automation `health` command reports probe
state only; use `kubectl get nodes` to verify actual running kubelet versions.

Civo currently uses an explicit local Pulumi backend for these cells; `bootstrap -cell
civo-sandbox-use1-serving` initializes that directory locally and performs no
cloud-side backend work.

A Civo cell can keep its state in Cloudflare R2 instead; see
[Cloudflare R2 state backend](#cloudflare-r2-state-backend-civo-cells).

The Civo `minimal` profile (and the default when `-profile` is omitted) uses one
K3s node. The `prod` profile uses a fixed two-node K3s pool; no cluster
autoscaler is wired. This fixed two-node production default reflects the Civo
provider's inline pool model, which exposes a literal `NodeCount` rather than
the min/max range available on AKS. Changing an existing cell from `minimal` to
`prod` is intended to update only that inline pool's `NodeCount` in place.
Before applying the change, run `pulumi preview` and verify that it reports a
pool update with no cluster, pool, network, firewall, or PVC replacement.

Inputs split two ways: **functional** (`-cloud`, `-region`, `-profile`) drive
behavior; **labels** (`-account-alias`, `-role`) are free text used only in the
name. Credentials are a name, not a secret — `-aws-profile` (or the ambient
`AWS_PROFILE`/OIDC when omitted); `-account-alias` does **not** select creds.
State is stored in **S3 by default**; `up` errors with "run bootstrap first" if
the backend is missing. Pass `-backend local` for a zero-setup local file backend
(dev/experiments), which uses a tool-managed passphrase under `~/.witself-infra/state`.

### Civo load balancer and custom host name (opt-in)

By default a Civo cell is reached under Civo's own name,
`api.<cluster-id>.k8s.civo.com`, which points at a node. Three per-cell
settings change that. A cell that sets none of them keeps the default shape.

| Inventory key | Flag | Values | Effect |
|---|---|---|---|
| `civo_ingress` | `-civo-ingress` | `nodeport` (default), `loadbalancer` | `loadbalancer` adds one Civo load balancer in front of Traefik and names the cell `api.<cell>.<domain>` |
| `civo_dns` | `-civo-dns` | `none` (default), `cloudflare` | `cloudflare` adds one Cloudflare `A` record for that name; needs `civo_ingress: loadbalancer` |
| `register_draining` | `-register-draining` | `false` (default), `true` | `up` registers the cell with `accepting=false`; needs a control plane |

`<domain>` is the inventory key `domain` (flag `-domain`, built-in default
`cells.witself.witwave.ai`). For a Civo cell `-domain` is accepted only
together with `-civo-ingress loadbalancer`. The three keys are per-cell only:
the `defaults:` block must not set them. `-civo-node-size` accepts
`g4s.kube.medium` and `g4s.kube.large`. The node count still comes from the
profile: one node for `minimal`, two for `prod`.

A `witself-infra` older than the release that introduced these keys rejects an
inventory that contains them, for every cell. Install the new binary before
you record the first cell that uses them.

#### What `loadbalancer` creates

- One Kubernetes Service, `kube-system/witself-lb`, of type `LoadBalancer`,
  with the ports 80 and 443. It selects the Traefik pods that the Civo
  application `traefik2-nodeport` installs, by the labels
  `app.kubernetes.io/name=traefik` and
  `app.kubernetes.io/instance=traefik-kube-system`. Civo's cloud controller
  turns that Service into one load balancer.
- The Service carries the annotation `kubernetes.civo.com/firewall-id` with
  the cell's own firewall from its first manifest on. Without it Civo creates
  a second firewall named `default-<network>` with every port open, and that
  firewall outlives the load balancer. No firewall rule is added: the cell
  firewall already allows 80 and 443.
- The cluster's applications, its node pool and its firewall are the same as
  in the default shape.
- The stack exports `loadBalancerIP`, `civoIngress` and `civoDNS`. `apiHost`
  is `api.<cell>.<domain>`; `civoDNSEntry` still shows Civo's own name.

The address is read from the status of the Service. The name
`<id>.lb.civo.com` in the same status is never used. No reserved address is
used, so the address changes when the Service is recreated. If that happens
outside of `up`, run `refresh` and then `up`, so that the record follows the
new address. Do not set the proxy protocol annotation on the Service: Civo
then publishes no address.

With `civo_dns: none` nobody creates the record for you. Create an `A` record
for `apiHost` that points at `loadBalancerIP` yourself. Until it resolves,
certificate issuance and the HTTPS check of `up` cannot succeed.

`preview` and `up` refuse to change the public host name of a cell that already
has one. Before they write any stack setting, they read the stack's recorded
`apiHost` and compare it with the host this run would serve:
`api.<cell>.<domain>` with `civo_ingress: loadbalancer`, otherwise
`api.<civoDNSEntry>` with `argocd`, otherwise none. A changed `civo_ingress`, a
changed `domain` on a cell with a load balancer, or `argocd` turned off on a
cell without one therefore stops the run with an error that names both hosts.
The check reads only the stack and applies whether or not the run names a
control plane: the apply itself moves the host, in Argo CD's root application
and, with `civo_dns: cloudflare`, in the DNS record, and the control plane
refuses a registration under a new host while the cell holds accounts. To give
a cell a new host, provision a new cell, move the accounts to it
(`witself-admin cells evacuate`, then `cells restore`) and destroy the old
cell once it is empty. A cell that was never registered can be destroyed and
provisioned again. A new stack has no recorded `apiHost` and is never refused.
Changing `civo_dns` adds or removes the record only.

`destroy` deletes the Service before the cluster, the firewall and the
network. Civo removes the load balancer about 2.5 minutes after the Service is
gone. If `destroy` stops because the cell's firewall or network is still in
use, wait until the Civo account lists no load balancer for the cell, then run
the same `destroy` again.

#### What `cloudflare` creates

One `A` record in the Cloudflare zone that holds `<domain>`: name `apiHost`,
content `loadBalancerIP`, TTL 300, not proxied. Argo CD's root application is
created only after the Service and the record exist, so certificate issuance
starts with a name that resolves. The record is explicit configuration. A
token in the environment never creates a record on its own, and a Civo cell
with `civo_dns: none` ignores the token.

The token comes only from the environment variable `CLOUDFLARE_API_TOKEN` of
the shell that runs `witself-infra`. It is never read from `infra.yaml`, a
flag or a file. Create a custom token with these settings. The zone is looked
up by its name, and that lookup is what needs the read permission:

| Setting | Value |
|---|---|
| Permissions | Zone, Zone, Read; and Zone, DNS, Edit |
| Zone Resources | Include, Specific zone, the zone that holds `<domain>` |
| TTL | an end date on the day you stop needing the token |

`preview`, `up`, `refresh` and `destroy` of such a cell refuse without the
token and name the variable. They also refuse while `CLOUDFLARE_API_KEY`,
`CLOUDFLARE_EMAIL`, `CLOUDFLARE_API_USER_SERVICE_KEY` or `CLOUDFLARE_BASE_URL`
is set, because the Cloudflare provider would read those as well. `outputs`
and `cell-health` need no token. Every later `preview`, `up`, `refresh` and
`destroy` of the cell needs a valid token again.

`witself-infra` replaces the token's value, and any text shaped like a
Cloudflare token, in its final error line, in progress events and in the
`cell-health` report (see [Variables to export](#variables-to-export)). The
Pulumi CLI's own progress output is not filtered, and it can show in the clear
the provider text that the final error line shows redacted. When you share the
output of a failed run, share only the final `witself-infra:` line.

Export the token in a terminal that runs only this cell's commands, and close
that terminal afterwards. Other tools read the same variable name:

- For an AWS, GCP or Azure cell, the presence of the token switches on
  Cloudflare DNS delegation. Do not run `preview` or `up` of such a cell
  while the variable is exported.
- The dashboard starts `preview` and `up` with its own environment. Do not
  start the dashboard from that terminal.
- `wrangler` uses the variable as its own credential.

#### Provision a cell with a load balancer

Record the cell once. For a cell with a load balancer use the command below
instead of the one in the R2 section: `config add-cell` refuses a second
record of the same name, and the ingress shape of a cell that holds accounts
cannot be changed later.

The example keeps the state in Cloudflare R2; see
[Cloudflare R2 state backend](#cloudflare-r2-state-backend-civo-cells) for the
bucket, its token and the passphrase.

```sh
# 1. Record the cell. The Civo token file is read to validate it; no secret
#    is printed.
witself-infra config add-cell \
  -cloud civo -account-alias prod -region nyc1 -role serving -profile prod \
  -backend r2 \
  -r2-bucket <bucket> \
  -r2-endpoint https://<account-id>.r2.cloudflarestorage.com \
  -civo-token-file "$HOME/.witself/tokens/civo-sandbox.token" \
  -civo-expected-account-id 00000000-0000-0000-0000-000000000000 \
  -civo-node-size g4s.kube.large \
  -civo-admin-cidr 203.0.113.7/32 \
  -k8s-version 1.35.0-k3s1 \
  -civo-ingress loadbalancer \
  -civo-dns cloudflare \
  -domain cells.witself.witwave.ai \
  -register-draining \
  -argocd \
  -control-plane https://self.witwave.ai

# 2. Export the secrets in this terminal, which runs only this cell's
#    commands. Nothing is echoed.
printf 'R2 access key id: ';     read -rs WITSELF_INFRA_R2_ACCESS_KEY_ID;     echo
printf 'R2 secret access key: '; read -rs WITSELF_INFRA_R2_SECRET_ACCESS_KEY; echo
printf 'State passphrase: ';     read -rs WITSELF_INFRA_STATE_PASSPHRASE;     echo
printf 'Cloudflare API token: '; read -rs CLOUDFLARE_API_TOKEN;               echo
export WITSELF_INFRA_R2_ACCESS_KEY_ID WITSELF_INFRA_R2_SECRET_ACCESS_KEY \
  WITSELF_INFRA_STATE_PASSPHRASE CLOUDFLARE_API_TOKEN
unset CLOUDFLARE_API_KEY CLOUDFLARE_EMAIL CLOUDFLARE_API_USER_SERVICE_KEY CLOUDFLARE_BASE_URL

# 3. Check, preview, apply, read back.
witself-infra whoami      -cell civo-prod-use1-serving
witself-infra state-check -cell civo-prod-use1-serving
witself-infra preview     -cell civo-prod-use1-serving
witself-infra up          -cell civo-prod-use1-serving
witself-infra outputs     -cell civo-prod-use1-serving
witself-infra preview     -cell civo-prod-use1-serving   # must show no change

# 4. Drop the Cloudflare token from the shell. Revoke it in Cloudflare once
#    no further run of this cell needs it.
unset CLOUDFLARE_API_TOKEN
```

`-civo-admin-cidr` must contain the public address of the machine that runs
`up`, because the Kubernetes API is open to that range only.

`up` waits up to 20 minutes for HTTPS on `apiHost`. Do not look the name up
before the record exists (`dig`, `curl`, a browser): a resolver may keep the
negative answer for up to 30 minutes. If the wait ends for that reason, run
the same `up` again.

After the first `up`, list the firewalls of the Civo account: no firewall
named `default-<network>` may exist, and the account shows exactly one load
balancer for the cell.

#### Registration in a drained state

With `register_draining: true`, `up` registers the cell with
`accepting=false`, so no signup is placed on it. Registration runs on every
`up` that has a control plane, and the control plane stores the value each
time. While the key is set, every `up` closes the cell again; without the
key, every `up` opens it. Open the cell with
`witself-admin cells undrain <cell>` and remove `register_draining` from the
cell record in the same step, before the next `up`.

## State backend

State is stored in a cloud object-storage backend for real cells — shared,
durable, cloud-KMS-encrypted secrets (no passphrase), **one object-store backend
and one cloud key per account/project/subscription + region**. (`-backend local`
is the dev opt-out.)

A Civo cell can use Cloudflare R2 with a passphrase that the operator supplies;
see [Cloudflare R2 state backend](#cloudflare-r2-state-backend-civo-cells).

AWS uses S3 + AWS KMS and remains the default backend:

```sh
# once per account+region: create the bucket + KMS key (idempotent — reuses if present)
witself-infra bootstrap -cloud aws -region us-west-2 -aws-profile witwave-sandbox

# then point cells at it
witself-infra up --backend s3 -cloud aws -account-alias sandbox -region us-west-2 -role dev -aws-profile witwave-sandbox
```

`bootstrap` creates `witself-state-<account-id>-<region-code>` (versioned,
SSE-KMS, public-access-blocked, TLS-only) and `alias/witself-state-<region-code>`,
and prints the `s3://…` backend + `awskms://…` secrets provider. `up --backend s3`
**uses** that backend (KMS-encrypted secrets, no passphrase) and errors if it is
missing; pass `-bootstrap` to create it on first use.

GCP uses GCS + Cloud KMS. The GCP project is a shared substrate boundary, not the
cell boundary: one project can host multiple cell stacks. The current GCP cell
program provisions a dedicated custom VPC, regional subnet, GKE pod/service
secondary ranges, an internal firewall rule, private services access for
private-IP Cloud SQL, a regional GKE Autopilot cluster, a minimal private-IP
Cloud SQL Postgres instance, Secret Manager JSON secrets for DB/bootstrap/
provision material, a regional Cloud Router + Public Cloud NAT with a reserved
outbound IPv4 address, a public Cloud DNS zone, and a reserved global IPv4
address for the GKE Ingress. With `-argocd`, it also installs Argo CD and
bootstraps the GCP cell values file. It grants Workload Identity paths for
External Secrets Operator to read the cell secrets and for ExternalDNS to manage
only the cell Cloud DNS zone. The GCP cell values enable `witself-server`,
ExternalDNS, GKE Ingress, BackendConfig health checks, FrontendConfig
HTTP-to-HTTPS redirects, and a Google-managed certificate.

GCP profile sizing follows the same operator intent as AWS. `-profile minimal`
is the low-cost, one-shot test profile: zonal Cloud SQL, small disk, and no
retained database backups. `-profile prod` raises Cloud SQL to regional
availability, enables PITR plus retained/final backups, and increases disk
headroom; it is meant for persistent cells rather than nightly save-money
teardown loops.

For the autoscaling hyperscaler paths, the intended Kubernetes node envelope is
`1..20` for `minimal` and `2..20` for `prod`. AKS exposes that envelope directly
on the system node pool. AWS EKS Auto Mode and GKE Autopilot are currently left
in their managed scaling modes; literal node-count min/max controls will require
custom EKS Auto Mode NodePools and/or a future GKE Standard node-pool path. Civo
uses the fixed node counts described above instead of this autoscaling envelope.

```sh
# Pulumi's GCS backend and gcpkms secrets provider use Application Default
# Credentials, not only `gcloud auth login`.
gcloud auth application-default login --project witself-sandbox
gcloud config set container/use_application_default_credentials true

# prepare state only
witself-infra bootstrap \
  -backend gcs \
  -cloud gcp \
  -gcp-project witself-sandbox \
  -region us-west2

# or one-shot: prepare state if missing, then create/update the GCP substrate
witself-infra up \
  -account-alias sandbox \
  -argocd \
  -backend gcs \
  -bootstrap \
  -cloud gcp \
  -cidr 10.20.0.0/16 \
  -control-plane https://self.witwave.ai \
  -gcp-project witself-sandbox \
  -gitops-path .gitops/charts/bootstrap \
  -gitops-repo https://github.com/witwave-ai/witself \
  -gitops-revision main \
  -gitops-values-path .gitops/cells/gcp-sandbox-use1-dev/values.yaml \
  -profile minimal \
  -region us-east1 \
  -restore-archives \
  -restore-any-region \
  -role dev
```

Azure uses Blob Storage + Key Vault. The Azure subscription is a shared
substrate boundary, not the cell boundary: one subscription+region backend can
hold many cell stacks. The current Azure cell program provisions a dedicated
resource group, VNet, workload subnet, PostgreSQL-delegated DB subnet, NAT
Gateway, static public IPv4 address, private DNS zone/link, private Azure
Database for PostgreSQL Flexible Server, the logical `witself` database, and a
per-cell Key Vault containing DB/bootstrap/provision JSON secrets. It also
creates an AKS cluster in the workload subnet with Azure CNI overlay, controlled
egress through the cell NAT Gateway, native cluster autoscaler enabled on the
system node pool, and OIDC/workload identity enabled. It also creates an ESO
managed identity, federates it to the
`external-secrets/external-secrets` Kubernetes service account, grants that
identity read access to the cell Key Vault, and can install Argo CD when
`-argocd` is set. It creates an Azure DNS zone for the cell, delegates that zone
from Cloudflare when `CLOUDFLARE_API_TOKEN` is available, and creates an
ExternalDNS managed identity plus federated credential. It also creates a
dedicated subnet delegated to `Microsoft.ServiceNetworking/trafficControllers`
for Azure Application Gateway for Containers, enables the AKS-managed ALB
Controller add-on, grants the add-on identity permission to join that subnet,
then passes the subnet ID into GitOps. The app layer renders the Gateway API
manifests for the Witself API plus cert-manager's Azure DNS-01
issuer/certificate resources for Let's Encrypt HTTPS. HTTP-to-HTTPS redirect
policy remains a follow-up Azure ingress polish slice.

```sh
# Pulumi's azblob backend and azurekeyvault secrets provider can use Azure CLI
# auth. The backend bootstrap stores a storage account key in the local process
# environment for Pulumi, but it never prints that key.
az login --tenant a18639f4-1eb4-4810-ab3b-5717aa935e27
az account set --subscription witwave-sandbox

# prepare state only
witself-infra bootstrap \
  -azure-subscription witwave-sandbox \
  -backend azblob \
  -cloud azure \
  -region eastus2

# create/update the Azure network and controlled-egress substrate
witself-infra up \
  -account-alias sandbox \
  -azure-subscription witwave-sandbox \
  -backend azblob \
  -cloud azure \
  -db-version 18 \
  -k8s-version 1.36 \
  -profile minimal \
  -region eastus2 \
  -role dev
```

`bootstrap` registers the required Azure resource providers if needed, creates
`witself-state-<region-code>`, a private versioned Blob container named
`pulumi-state`, and a Key Vault key named `pulumi-secrets`. It prints the
`azblob://...` backend and `azurekeyvault://...` secrets provider. In RBAC-mode
vaults, bootstrap grants the signed-in user `Key Vault Crypto Officer` on the
state vault so Pulumi can create and use the state encryption key.

`up` registers the required workload resource providers if needed before running
Pulumi, so a fresh subscription can create the VNet, NAT resources, database,
Key Vault, and AKS cluster in the same command. With `-argocd`, it waits for the
Argo CD app-of-apps tree to report `Synced/Healthy` after Pulumi finishes. The
AKS system node pool uses the native cluster autoscaler (`1..20` nodes for
`minimal`, `2..20` for `prod`). The workload subnet has default outbound access
disabled and egresses through the NAT Gateway; the DB subnet has default
outbound access disabled and is delegated to
`Microsoft.DBforPostgreSQL/flexibleServers`; the ALB subnet has default outbound
access disabled and is delegated to
`Microsoft.ServiceNetworking/trafficControllers`. The PostgreSQL server uses
password auth, public network access disabled, the delegated DB subnet, and a
private DNS zone named `privatelink.postgres.database.azure.com`.

The cell Key Vault is separate from the state-backend Key Vault. It stores the
same app material as AWS Secrets Manager and GCP Secret Manager: `db`,
`bootstrap-operator-token`, and `provision-token`. The current operator gets an
access policy so Pulumi can write the secrets during `up`; the ESO managed
identity gets a separate `get/list` access-policy resource for External
Secrets.

For GCP cells with `-argocd`, `up` does not stop at Pulumi success. After the
cell is registered and reachable through the control-plane probe, and after any
archive restore completes, the CLI reads the GKE API directly with ADC and waits
for every Argo CD Application to report `Synced/Healthy`. That makes Google
ManagedCertificate status lag visible as a normal waiter instead of a surprise
operator caveat after the command exits.

## Cloudflare R2 state backend (Civo cells)

`-backend r2` keeps a Civo cell's Pulumi state in a Cloudflare R2 bucket, so
the state does not live on one machine. It is accepted only with `-cloud civo`.
AWS, GCP and Azure cells keep their own backends. A Civo cell recorded with
`backend: local` keeps its local state until it is migrated, which is a
separate later step (see the end of this section).

### What the owner creates

`witself-infra` creates no bucket, no token and no bucket setting. It writes
objects into the bucket that the owner created: Pulumi's state files and, for
the check, one small probe object that it deletes again.

1. **The bucket.** Create it in the Cloudflare dashboard or with
   `wrangler r2 bucket create <name>`. A name has 3-63 characters: lowercase
   letters, digits and hyphens. One bucket can hold the stacks of several
   cells.
2. **A bucket-scoped token.** On the R2 API tokens page, create a token with
   the permission **Object Read & Write** and apply it to that bucket only.
   Cloudflare shows the Access Key ID and the Secret Access Key once. The token
   cannot create a bucket or change bucket settings. A new token can take up
   to a minute to work.
3. **The state passphrase.** Generate at least 20 characters in the password
   manager and keep it there. It encrypts the secrets inside the state, and
   every machine that operates the cell must use the same value. For an R2
   cell `witself-infra` never generates a passphrase, never reads one from a
   file and never writes one to disk. A lost passphrase cannot be recovered.

### Record the cell

```sh
witself-infra config add-cell \
  -cloud civo -account-alias prod -region nyc1 -role serving -profile prod \
  -backend r2 \
  -r2-bucket <bucket> \
  -r2-endpoint https://<account-id>.r2.cloudflarestorage.com \
  -civo-token-file "$HOME/.witself/tokens/civo-sandbox.token" \
  -civo-expected-account-id 00000000-0000-0000-0000-000000000000 \
  -civo-admin-cidr 203.0.113.7/32 \
  -argocd \
  -control-plane https://self.witwave.ai
```

For `civo-prod-use1-serving` and every other cell with a Civo load balancer,
use the complete command in
[Provision a cell with a load balancer](#provision-a-cell-with-a-load-balancer)
instead: `config add-cell` refuses a second record of the same name, and the
ingress shape of a cell that holds accounts cannot be changed later.

The inventory stores the bucket name and the endpoint as `r2_bucket` and
`r2_endpoint`. Both are per-cell only and neither is a secret. A bucket created
with a jurisdiction uses the `eu`, `us` or `fedramp` form of the endpoint host.
`-state-dir` does not apply.

A `witself-infra` older than the release that introduced these two keys rejects
an inventory that contains them, for every cell. Install the new binary before
you add the first R2 cell.

### Variables to export

The three secret values come only from the environment of the process:

| Variable | Value |
|---|---|
| `WITSELF_INFRA_R2_ACCESS_KEY_ID` | the token's Access Key ID |
| `WITSELF_INFRA_R2_SECRET_ACCESS_KEY` | the token's Secret Access Key |
| `WITSELF_INFRA_STATE_PASSPHRASE` | the state passphrase |

```sh
printf 'R2 access key id: ';     read -rs WITSELF_INFRA_R2_ACCESS_KEY_ID;     echo
printf 'R2 secret access key: '; read -rs WITSELF_INFRA_R2_SECRET_ACCESS_KEY; echo
printf 'State passphrase: ';     read -rs WITSELF_INFRA_STATE_PASSPHRASE;     echo
export WITSELF_INFRA_R2_ACCESS_KEY_ID WITSELF_INFRA_R2_SECRET_ACCESS_KEY WITSELF_INFRA_STATE_PASSPHRASE
```

If a variable is absent, the command refuses and names it. The values are
never written to `infra.yaml`. `witself-infra` replaces the three values in its
final error line, in progress events and in the stack-output error of the
`cell-health` report. In the same three places it also replaces the values of
`CLOUDFLARE_API_TOKEN`, `CLOUDFLARE_API_KEY` and
`CLOUDFLARE_API_USER_SERVICE_KEY`, and any text shaped like a prefixed
Cloudflare credential (`cfut_`, `cfat_` or `cfk_` followed by at least eight
letters, digits, `-` or `_`). Other output is not filtered: the Pulumi CLI
writes its own progress to the terminal and into the dashboard's log files.

Do not export `AWS_ACCESS_KEY_ID`, `AWS_SECRET_ACCESS_KEY` or
`PULUMI_CONFIG_PASSPHRASE` for this purpose. Those names also reach the AWS
cells and the local-state cells that you operate from the same shell, and they
change which credentials and which passphrase those cells use. `witself-infra`
maps the three dedicated variables to the names Pulumi reads, inside the R2
cell's own Pulumi process only.

All R2 cells operated from one shell share the three values. The dashboard
runs its operations and its background health probes as child processes, so
export the variables in the shell that starts the dashboard.

### Check the backend

```sh
witself-infra state-check -cell <cell>
```

`state-check` lists the bucket, writes one small probe object under
`.pulumi/locks/witself-infra-state-check/`, reads it back, confirms that a
listing shows it, deletes it and confirms that a listing no longer shows it.
These are the operations Pulumi's lock depends on. It then prints whether the
cell's stack file exists and how many lock, history and backup objects the
cell has. `up`, `preview`, `refresh`, `destroy` and `bootstrap` run the same
probe first. `outputs` and `cell-health` never run the probe. `whoami` checks
the Civo identity only. For an R2 cell `bootstrap` runs the probe and creates
no bucket and no local file, and `-bootstrap` is accepted and ignored.

The first Pulumi command on a bucket writes `.pulumi/meta.yaml`, whichever
verb it is.

On an R2 cell, `outputs`, `cell-health`, `refresh` and `destroy` never create
a stack. If the bucket holds no stack for the cell they refuse. Only `preview`
and `up` create the stack of a new cell.

### What R2 does not offer, and what stands in for it

R2 has no bucket versioning, no S3 object lock calls, no bucket policy and no
KMS-managed key. It encrypts every object at rest itself. R2 also accepts
encryption keys supplied per request (SSE-C); the backend URL below does not
use them.

Pulumi keeps its own copies inside the bucket:

- `.pulumi/history/witself-infra/<cell>/` holds one entry and one copy of the
  state per operation;
- `.pulumi/backups/witself-infra/<cell>/` holds a timestamped copy of the
  state after every `up`, `refresh` and `destroy`, whether or not the
  operation changed anything;
- `<cell>.json.bak` beside the active state file holds the previous version.

Optionally, add R2 bucket lock rules for the two prefixes `.pulumi/history/`
and `.pulumi/backups/`, so that these copies cannot be deleted or overwritten
during a retention period. Never add a rule without a prefix. Never add one on
`.pulumi/`, on `.pulumi/stacks/`, on `.pulumi/locks/` or on any prefix below
`.pulumi/locks/`: Pulumi overwrites the active state file and deletes its lock
object in every operation. `state-check` finds a rule that covers
`.pulumi/locks/` as a whole; it does not find a rule on one cell's lock folder.
Bucket lock rules are set by the owner; the state token cannot change them.
Try the rules on a scratch bucket first.

### The Pulumi URL

`witself-infra` builds this backend URL and needs Pulumi CLI 3.263.0 or newer:

```text
s3://<bucket>?endpoint=https%3A%2F%2F<account-id>.r2.cloudflarestorage.com&region=auto&request_checksum_calculation=when_required&use_path_style=true
```

- `endpoint` is the R2 S3 endpoint, with its scheme.
- `region=auto` is R2's region; the AWS SDK requires one.
- `use_path_style=true` addresses the bucket in the path.
- `request_checksum_calculation=when_required` stops the AWS SDK from adding
  checksums that a store other than AWS may reject.

### A stale lock

Pulumi's lock is an object under
`.pulumi/locks/organization/witself-infra/<cell>/`. It has no expiry. A run
that was killed leaves it behind, and every later `up`, `refresh` and
`destroy` refuses. `state-check` prints the number of lock objects.
`witself-infra` has no verb that removes a lock. Confirm that no operation is
running on any machine or in any dashboard, then delete the objects of that
folder in the Cloudflare dashboard.

### Migrating an existing local stack

Moving a cell from `backend: local` to `backend: r2` is a separate later step
and is not supported by this tool yet. Do not change `backend` on the record
of an existing cell: the cell would then address a new, empty state while its
real resources stay in the old one.

## GitOps (Argo CD)

Pass `-argocd` to install the Argo CD control plane into the cell's cluster from
its upstream Helm chart (`argo-cd` 10.0.1). This is **universal** — the chart,
not the AWS-only managed EKS capability — so the same install works on EKS, GKE,
or a self-hosted cluster. It is **opt-in** and off by default.

```sh
witself-infra up -argocd $F

# reach the UI (ClusterIP):
kubectl -n argocd port-forward svc/argocd-server 8080:443   # https://localhost:8080, user: admin
kubectl -n argocd get secret argocd-initial-admin-secret -o jsonpath='{.data.password}' | base64 -d
```

The Kubernetes provider authenticates with an exec kubeconfig (`aws eks
get-token` on AWS, `gcloud auth application-default print-access-token` on GCP),
so the first install can outlast a static token while managed Kubernetes
provisions capacity.

`-argocd` also creates a root Argo `Application` (`bootstrap`) that renders the
shared `.gitops/charts/bootstrap` chart with this cell's
`.gitops/cells/<cell>/values.yaml` file. That Git-owned values file pins the
platform/app chart versions and cell-specific settings. The repo is public, so
Argo needs **no credentials** (private-repo creds: issue #7). Point Argo at a
self-hosted fork with the `-gitops-*` flags:

```sh
witself-infra up -argocd \
  -gitops-repo https://github.com/you/your-config \
  -gitops-path .gitops/charts/bootstrap \
  -gitops-values-path .gitops/cells/aws-sandbox-usw2-dev/values.yaml \
  -gitops-revision main $F
```

SSO, ingress polish, and production hardening are later slices.

## Fleet (control plane)

Pass `-control-plane` to make cell lifecycle changes known to the Witself Cloud
control plane (`https://self.witwave.ai`). Omit it and no registration happens —
that is the self-hosted path, same command.

```sh
# up: provision, then REGISTER the cell with the fleet (post-step).
# The registered endpoint is the cell's apiHost output (api.<cell>.<domain>).
witself-infra up -control-plane https://self.witwave.ai $F

# Before destroy: record deletion_protection: false on this cell and apply
# a separate up -cell CELL, as described in the break-glass runbook.
# destroy: DRAIN the cell (placement stops), EVACUATE every account to a
# Cloudflare R2 archive (per-account file, integrity-checked), then REMOVE the
# empty registry entry and tear down:
witself-infra destroy -control-plane https://self.witwave.ai $F
#   evacuated acc_… from <cell>
#   ...
#   cell <cell>: N accounts evacuated to Cloudflare R2
#   cell <cell> removed from fleet

# sandbox/dev teardown where the cell's data genuinely dies — SKIP evacuation
# and force-purge account entries from the control plane:
witself-infra destroy -control-plane https://self.witwave.ai -destroy-accounts $F
```

Authorization is the **fleet token**, read from `-fleet-token-file` if given,
else `WITSELF_FLEET_TOKEN`, else `~/.witself/tokens/fleet.token` (minted when the control plane was deployed; its
counterpart lives as the `FLEET_TOKEN` Worker secret). One token per fleet — all
cells registering to the same control plane use the same token.

Registration is deliberately **outside the Pulumi resource graph**: fleet
membership is bookkeeping on the control plane, not a cloud resource. `up`
registers after a successful provision; `destroy` drains/removes before teardown.
The control plane never touches infrastructure — Pulumi destroys things, the
control plane forgets them.

| Flag | Applies to | Effect |
|---|---|---|
| `-control-plane URL` | `up` | register the cell (upsert) after provisioning |
| `-control-plane URL` | `destroy` | drain, evacuate every account to R2, then remove the cell from the fleet before teardown |
| `-fleet-token-file PATH` | both | read the fleet token from this file (default: `WITSELF_FLEET_TOKEN` env, then `~/.witself/tokens/fleet.token`) |
| `-destroy-accounts` | `destroy` | with `-control-plane`: SKIP evacuation and force-purge accounts — sandbox/dev override, the data dies with the cell |
| `--allow-unknown-cell` | `destroy` | bypass only the phantom-stack check; an unrecorded cell still fails deletion protection |
| `--force-with-accounts` | `destroy` | proceed after the control plane reports live or archived accounts still placed on the target; does not imply purge |
| `--skip-account-check` | `destroy` | bypass an unavailable placement-status read; required for an intentional self-host destroy with no control plane |
| `--yes-cell=NAME` | non-interactive `destroy` | confirm only when `NAME` exactly matches the target cell; interactive runs prompt for the exact name instead |
| `-restore-archives` | `up` | after registration, restore archived accounts whose stored region matches this cell's region |
| `-restore-any-region` | `up` | with `-restore-archives`: bypass the provider-region guard for legacy archives; policy-aware hard pins remain authoritative |

Placement-aware fleet operations use the same fleet token:

```sh
# Fleet health: cells, archived accounts blocked by pins, and live move candidates.
witself-infra placement-status -control-plane https://self.witwave.ai

# Preview once, then run bounded live moves until no better eligible target remains.
witself-infra rebalance -control-plane https://self.witwave.ai -dry-run
witself-infra rebalance -control-plane https://self.witwave.ai -batch 1

# The Worker cron ticks every five minutes but remains a no-op until enabled.
witself-infra placement-runner -control-plane https://self.witwave.ai -enable -run

# Emergency escape hatch for an archived account with impossible hard pins.
witself-admin placement rescue --account-id acc_... --axes cloud,region,channel
```

The rescue operation only clears the selected `allowed_*` hard-pin lists. It
preserves ranked preferences and does not restore or move the account itself;
the next manual or scheduled placement pass does that.

## Roadmap (one slice at a time)

1. **[done]** module + CLI + Automation API loop.
2. **[done]** AWS substrate: cell VPC (NAT egress) + EKS Auto Mode + RDS Postgres.
3. **[done]** S3 + KMS state backend (`bootstrap`).
4. **[done]** Argo CD (GitOps control plane) via Helm — opt-in `-argocd`.
5. **[done]** Wire Argo at the bootstrap app-of-apps chart + per-cell values.
6. **[done]** Metrics Server in the GitOps platform tier (resource metrics API
   for `kubectl top` and HPA CPU/memory signals).
7. **[done]** Fleet registration: `-control-plane` on `up`/`destroy` registers /
   drains+removes the cell against the control plane (`-destroy-accounts` to
   purge); fleet token from `~/.witself/tokens/fleet.token`.
8. **[done]** GCP GCS + Cloud KMS state backend and empty stack lifecycle.
9. **[done]** GCP network substrate: custom VPC, regional subnet, secondary
   ranges for future GKE pods/services, internal firewall, and private services
   access for future Cloud SQL.
10. **[done]** GCP GKE Autopilot substrate with VPC-native pod/service ranges
    and Workload Identity.
11. **[done]** GCP Cloud SQL Postgres over private services access plus Secret
    Manager DB connection JSON.
12. **[done]** GCP Argo CD control plane and GCP cell GitOps values scaffold.
13. **[done]** GCP ESO → Secret Manager via GKE Workload Identity for DB,
    bootstrap, and provision secrets.
14. **[done]** GCP `witself-server` GitOps app as an internal ClusterIP
    workload, backed by the ESO-synced Cloud SQL DSN.
15. **[done]** GCP Cloud DNS + Cloudflare delegation + ExternalDNS Workload
    Identity + GKE Ingress/BackendConfig/FrontendConfig + Google-managed
    certificate + HTTP-to-HTTPS redirect.
16. **[done]** GCP controlled egress with regional Cloud Router, reserved
    outbound IPv4 address, and Public Cloud NAT over the cell subnet ranges.
17. **[done]** Azure Blob Storage + Key Vault state backend and empty stack
    lifecycle in `eastus2`.
18. **[done]** Azure network substrate: resource group, VNet, workload subnet,
    PostgreSQL-delegated DB subnet, Application Gateway for Containers subnet,
    NAT Gateway, and static outbound IP.
19. **[done]** Azure private PostgreSQL Flexible Server plus logical `witself`
    database on the delegated DB subnet.
20. **[done]** Azure Key Vault app secrets for DB, bootstrap, and provision
    material.
21. **[done]** Azure AKS with Azure CNI overlay, native cluster autoscaler,
    controlled egress through the cell NAT Gateway, and OIDC/workload identity
    enabled.
22. **[done]** Azure ESO Workload Identity and Key Vault read access.
23. **[done]** Azure GitOps/Argo CD installation parity.
24. **[done]** Azure DNS delegation and ExternalDNS Workload Identity parity.
25. **[done]** Azure Application Gateway for Containers subnet, AKS-managed ALB
    Controller add-on, delegated subnet permission, and Gateway API HTTP
    manifest path.
26. **[done]** Azure HTTPS parity with cert-manager Azure DNS-01
    issuer/certificate automation for the Azure Gateway path.
27. Azure HTTP-to-HTTPS redirect policy.
28. **[done]** Database/secret-store deletion protection and inventory break-glass.
29. SSO; sealed-plane KMS (prod), and remaining production hardening.
