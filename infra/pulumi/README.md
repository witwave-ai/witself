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
export AWS_PROFILE=witwave-sandbox          # creds are ambient, not derived from --account-alias
export PULUMI_CONFIG_PASSPHRASE=…           # encrypts secret outputs in local state

./bin/witself-infra preview -cloud aws -account-alias sandbox -region us-west-2 -role dev
./bin/witself-infra up      -cloud aws -account-alias sandbox -region us-west-2 -role dev
./bin/witself-infra outputs -cloud aws -account-alias sandbox -region us-west-2 -role dev
./bin/witself-infra destroy -cloud aws -account-alias sandbox -region us-west-2 -role dev
```

Inputs split two ways: **functional** (`-cloud`, `-region`, `-profile`) drive
behavior; **labels** (`-account-alias`, `-role`) are free text used only in the
name. The real region is passed to the provider; its short code (`us-west-2` →
`usw2`, from a lookup table) appears in the name. State defaults to a local file
backend under `~/.witself-infra/state`.

## Roadmap (one slice at a time)

1. **[done]** module + CLI + Automation API loop.
2. **[done]** AWS substrate: dedicated cell VPC (private subnets) + RDS Postgres.
3. Install the published OCI chart
   (`oci://ghcr.io/witwave-ai/charts/witself-server`).
4. Ingress modes: `cloudflare-tunnel | alb | none`.
5. Sealed-plane KMS (prod profile), IRSA, NAT/egress, GCP provider.
