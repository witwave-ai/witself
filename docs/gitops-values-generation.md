# GitOps cell values generation

Status: implemented. `scripts/gitops-cell-values.sh` generates the ten
`.gitops/cells/<cell>/values.yaml` overlays from checked-in cell config and
fails CI when a committed file drifts. Those ten files remain live Argo CD
inputs; this generator must reproduce them byte-for-byte or report the diff
without touching them.

## Design: source of truth

Each overlay is assembled from three owners. The generator emits templates
and preserves their comments and key order without dumping the whole document
through a YAML serializer.

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
| Switches | Catalog `switches`: AWS `aws_zone_type`; GCP managed-HA / worker jobs / fact-deletion / avatar-compaction / dark agent-email receive; Civo `domain_documentation_only` (the documentation-only domain comment); Civo `monitoring` / `collector_alerts` / `sealed_plane_alerts` (the `platform.monitoring` block in the `civo-sandbox-usw2-dev` and `civo-sandbox-use1-serving` overlays; the recovery overlay renders all three switches explicitly, initially false) |

### Pinned scalars that `scripts/roll-cell.sh` owns

The generator **reads** these from the existing values file and writes them
back unchanged:

- `apps.witselfServer.chartVersion`
- `apps.witselfServer.imageTag`
- `apps.witselfServer.imageDigest` (optional; omitted when empty)

`roll-cell.sh` resolves the release server image from the GHCR manifest API,
validates its digest, and passes all three pins to the generator's
`--roll-cell CELL --version VERSION --image-digest DIGEST` mode. That mode
rejects existing generation drift and invalid pins before atomically replacing
only the selected cell's values. Run `--write` after a roll and the generator
keeps the new pins. Do not put the live pin in the catalog or in an overlay
placeholder's default.

The digest must be `sha256:` followed by 64 lowercase hexadecimal characters.
The apps chart forwards a non-empty `imageDigest` as the server chart's
`image.digest`. Empty or absent digest pins retain the existing tag rendering.

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
`roll-cell.sh` pin. The `civo-sandbox-usw2-dev` overlay emits enabled alert blocks only when
`monitoring: true`. The `civo-sandbox-use1-serving` recovery overlay renders
all three switches explicitly as false until a later monitoring change.
Setting switches on an overlay without a `platform.monitoring` block has
no effect.

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

1. `scripts/roll-cell.sh <cell> <version> …` resolves the manifest digest
   and asks the generator to update only the three Witself release pins.
   Registry failures or missing/malformed digests abort before any values edit.
2. The generator treats those pins as inputs. `--check` after a roll still
   passes because it substitutes the new scalars into the same template.
3. `--write` after a roll is a no-op unless some other generated field also
   drifted.

The generator emits Go `text/template` output and adds a non-empty digest
beside the generated server image tag, using the parsed YAML location to
preserve comment-bearing blocks. Unpinned cells retain their existing bytes.

`scripts/roll-train.sh` supports tag-only and digest-pinned rolls. Its digest
convergence and downgrade checks use the release pins in the cell values;
see [the release train contract](release-and-build.md#current-automated-release-and-rollout-boundary).
