# GitOps cell values generation

Status: implemented. `scripts/gitops-cell-values.sh` generates the nine
`.gitops/cells/<cell>/values.yaml` overlays from checked-in cell config and
fails CI when a committed file drifts. Those nine files remain live Argo CD
inputs; this generator must reproduce them byte-for-byte or report the diff
without touching them.

## Design: source of truth

Each overlay is assembled from three owners. The generator never dumps the
whole document through `yq` (that re-indents comments; see
`scripts/roll-cell.sh`).

### Generated from cell config

Source: [`.gitops/cells/catalog.yaml`](../.gitops/cells/catalog.yaml), using
the same composed name as `witself-infra`
(`<cloud>-<account-alias>-<region-code>-<role>`).

| Field | Rule |
| --- | --- |
| `cell.name` | Catalog key / cell directory name |
| `cell.cloud` | Catalog `cloud` |
| `cell.accountAlias`, `cell.role` | Catalog / defaults. Omitted on Civo: the checked-in Civo files never had those keys, and Pulumi injects runtime DNS instead of treating this overlay as deployable as-is |
| `cell.region` | Catalog `region` (provider-native) |
| `cell.domain`, `cell.apiHost` | Default `<cell>.cells.witself.witwave.ai` and `api.<cell>.cells.witself.witwave.ai`. Civo sets a host override `domain: cells.witself.witwave.ai` (documentation only; Pulumi replaces both names) |
| `gitops.repoURL`, `gitops.targetRevision` | Catalog `gitops` block |
| `gitops.valuesPath` | Civo only: `.gitops/cells/<cell>/values.yaml` |
| Cloud-family ingress, secrets, ExternalDNS/ESO, and platform add-on enablement | AWS / Azure / GCP family templates. Chart versions for cert-manager, external-dns, external-secrets, KEDA, and metrics-server come from [`.gitops/charts/platform/values.yaml`](../.gitops/charts/platform/values.yaml) and are **not** per-cell: every overlay emits those fleet defaults. A future per-cell platform chart pin needs a catalog field; it is not expressible in `values.yaml` by hand. Civo Postgres chart version comes from [`.gitops/charts/apps/values.yaml`](../.gitops/charts/apps/values.yaml) |
| Switches | Catalog `switches`: AWS `aws_zone_type`; GCP managed-HA / worker jobs / fact-deletion / avatar-compaction / dark agent-email receive; Civo `domain_documentation_only` (the documentation-only domain comment); Civo `monitoring` / `collector_alerts` / `sealed_plane_alerts` (the `platform.monitoring` block in the `civo-sandbox-usw2-dev` overlay, with `collectorAlerts.enabled` only when `collector_alerts` is true and `sealedPlaneAlerts.enabled` only when `sealed_plane_alerts` is true) |

### Pinned scalars that `scripts/roll-cell.sh` owns

The generator **reads** these from the existing values file and writes them
back unchanged:

- `apps.witselfServer.chartVersion`
- `apps.witselfServer.imageTag`

`roll-cell.sh` is the only tool allowed to change those two paths (it uses
`yq -i` on just those scalars, then `git diff` to refuse any other edit).
Run `--write` after a roll and the generator keeps the new pins. Do not put
the live pin in the catalog or in an overlay placeholder's default.

When `--write` bootstraps a catalog cell that has no `values.yaml` yet, it
creates the file using `apps.witselfServer.chartVersion` / `imageTag` from
[`.gitops/charts/apps/values.yaml`](../.gitops/charts/apps/values.yaml). The
first `scripts/roll-cell.sh` for that cell then owns the pins. `--check`
reports that cell as missing until the file exists.

### Per-cell hand-maintained blocks (carried verbatim)

Civo cells diverge immediately after the identity/gitops header (Postgres
image digests, replica/PDB/topology comments, production agent-email, the
founder monitoring stack). Those bodies live in
[`internal/gitopsvalues/overlays/`](../internal/gitopsvalues/overlays/) and
are spliced in as rendered text, including comments and key order.

Azure gateway / ExternalDNS comments are part of the Azure family template,
not a per-cell overlay: both Azure cells share them.

The sealed-plane alert group is a catalog switch (`sealed_plane_alerts`)
rendered in the serving-cell overlay next to `collector_alerts`, not a
`roll-cell.sh` pin. Both switches take effect only through the
`civo-sandbox-usw2-dev` overlay's `platform.monitoring` block, so setting them
on a cell without that block (or without `monitoring: true`) renders nothing.

## Usage

From the repository root:

```sh
scripts/gitops-cell-values.sh --check
scripts/gitops-cell-values.sh --write
```

`--check` prints a unified diff and exits 1 when any generated file differs
from the committed file. A catalog cell with no `values.yaml` is reported as
missing. It does not write `.gitops/cells/*/values.yaml`.

`--write` rewrites only files that differ and creates `values.yaml` for
catalog cells whose directory is missing or empty. Matching files are left
untouched so `git diff --stat -- .gitops/cells` stays empty when the tree is
already in sync.

`make check-infra` and the CI helm job run `--check` plus
`scripts/test-gitops-cell-values.sh`.

Regenerate after catalog, family-template, overlay, or platform-chart-pin
changes:

```sh
scripts/gitops-cell-values.sh --write
git diff -- .gitops/cells
```

Then review the overlay diff the same way as any other GitOps values change.
Never combine a generated identity/enablement rewrite with a
`roll-cell.sh` pin bump in the same unreviewed blob; pins still belong in
their own rollout commit.

## How `roll-cell.sh` pins interact

1. `scripts/roll-cell.sh <cell> <version> …` edits only the two Witself
   pins with `yq` and refuses stray formatting.
2. The generator treats those pins as inputs. `--check` after a roll still
   passes because it substitutes the new scalars into the same template.
3. `--write` after a roll is a no-op unless some other generated field also
   drifted.

If `yq` ever rewrites comments or key order, `roll-cell.sh` already fails
closed on that stray diff. This generator does not call `yq` at all: it
emits Go `text/template` output so comment-bearing blocks round-trip.
