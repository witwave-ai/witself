#!/usr/bin/env bash
# gitops-cell-values — generate .gitops/cells/<cell>/values.yaml from the
# checked-in cell catalog. --check reports drift without writing live GitOps
# inputs; --write rewrites files that differ and creates values.yaml for a
# catalog cell whose directory is missing or empty. Pins in
# apps.witselfServer.chartVersion / imageTag are read from the existing file
# (or the apps chart defaults on first write) so scripts/roll-cell.sh remains
# the writer of those two scalars after bootstrap.
#
# Usage: scripts/gitops-cell-values.sh --check|--write [--root PATH]
set -euo pipefail

repo_root=$(CDPATH='' cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
cd -- "$repo_root"
exec go run ./internal/cmd/gitops-cell-values "$@"
