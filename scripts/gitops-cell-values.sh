#!/usr/bin/env bash
# gitops-cell-values — generate .gitops/cells/<cell>/values.yaml from the
# checked-in cell catalog. --check reports drift without writing live GitOps
# inputs; --write rewrites files that differ and creates values.yaml for a
# catalog cell whose directory is missing or empty. Pins in
# apps.witselfServer.chartVersion / imageTag / imageDigest are read from the existing file
# (or the apps chart defaults on first write) so scripts/roll-cell.sh remains
# the release pin writer through --roll-cell after bootstrap. Empty imageDigest
# remains omitted; --roll-cell requires a complete validated digest and refuses drift.
# apps.civoPostgres.backup.image pins are preserved on ordinary generation and
# server-only rolls. --backup-image-repository, --backup-image-tag, and
# --backup-image-digest opt a roll into changing the backup image atomically.
#
# Usage: scripts/gitops-cell-values.sh --check|--write [--root PATH]
#        scripts/gitops-cell-values.sh --roll-cell CELL --version VERSION --image-digest DIGEST
#          [--backup-image-repository REPOSITORY --backup-image-tag TAG --backup-image-digest DIGEST] [--root PATH]
set -euo pipefail

repo_root=$(CDPATH='' cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
cd -- "$repo_root"
exec go run ./internal/cmd/gitops-cell-values "$@"
