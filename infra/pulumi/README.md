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
  cmd/witself-infra/    # the CLI: up | preview | destroy | refresh | outputs
  internal/cell/        # the inline Pulumi program — the cell definition
```

## Prerequisites

The Automation API drives the `pulumi` engine binary, so it must be on `PATH`
(`brew install pulumi`). A planned follow-up has `witself-infra` install and pin
its own engine on first run (via `auto.NewPulumiCommand`), so the end user
installs only `witself-infra` — the engine is fetched like a provider plugin.

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
./bin/witself-infra destroy $F
```

Inputs split two ways: **functional** (`-cloud`, `-region`, `-profile`) drive
behavior; **labels** (`-account-alias`, `-role`) are free text used only in the
name. Credentials are a name, not a secret — `-aws-profile` (or the ambient
`AWS_PROFILE`/OIDC when omitted); `-account-alias` does **not** select creds.
State is stored in **S3 by default**; `up` errors with "run bootstrap first" if
the backend is missing. Pass `-backend local` for a zero-setup local file backend
(dev/experiments), which uses a tool-managed passphrase under `~/.witself-infra/state`.

## State backend

State is stored in **S3 by default** — shared, durable, KMS-encrypted (no
passphrase), **one bucket + one KMS key per account + region**. (`-backend local`
is the dev opt-out.)

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

## GitOps (Argo CD)

Pass `-argocd` to install the Argo CD control plane into the cell's cluster from
its upstream Helm chart (`argo-cd` 10.0.1). This is **universal** — the chart,
not the AWS-only managed EKS capability — so the same install works on EKS, GKE,
or a self-hosted cluster. It is **opt-in** and off by default.

```sh
witself-infra up -argocd $F

# reach the UI (ClusterIP — no public LB yet):
kubectl -n argocd port-forward svc/argocd-server 8080:443   # https://localhost:8080, user: admin
kubectl -n argocd get secret argocd-initial-admin-secret -o jsonpath='{.data.password}' | base64 -d
```

The Kubernetes provider authenticates with an exec kubeconfig (`aws eks
get-token`, auto-refreshing — Auto Mode provisions nodes on demand, so the first
install can outlast a static token while Argo's pods wait for compute).

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

Wiring ESO to AWS Secrets Manager, SSO, and ingress are later slices.

## Fleet (control plane)

Pass `-control-plane` to make cell lifecycle changes known to the Witself Cloud
control plane (`https://self.witwave.ai`). Omit it and no registration happens —
that is the self-hosted path, same command.

```sh
# up: provision, then REGISTER the cell with the fleet (post-step).
# The registered endpoint is the cell's apiHost output (api.<cell>.<domain>).
witself-infra up -control-plane https://self.witwave.ai $F

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
8. ESO → AWS Secrets Manager (Pod Identity/IRSA + `SecretStore` + DB creds); then
   SSO + ingress; the witself-server chart; sealed-plane KMS (prod), GCP provider.
