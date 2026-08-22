#!/usr/bin/env bash
# roll-cell — bump a cell's witself-server chart+image to a released Witself
# version. Scoped: only the two Witself-owned fields (apps.witselfServer.
# chartVersion and apps.witselfServer.imageTag) are touched. Upstream chart
# versions (cert-manager, external-dns, external-secrets, keda,
# metrics-server) are OFF-LIMITS to this script by design.
#
# Usage: scripts/roll-cell.sh <cell-name> <version> [gate options]
# Example shape: scripts/roll-cell.sh CELL_NAME RELEASED_VERSION --no-schema-change
set -euo pipefail

usage() {
  echo "usage: $0 <cell-name> <version> (--backup-evidence DIR [--backup-evidence DIR] | --no-schema-change)" >&2
}

die() {
  echo "error: $*" >&2
  exit 2
}

BACKUP_EVIDENCE=()
NO_SCHEMA_CHANGE=false
POSITIONAL=()
while [ "$#" -gt 0 ]; do
  case "$1" in
    --backup-evidence)
      if [ "$#" -lt 2 ]; then
        usage
        die "--backup-evidence requires an artifact directory"
      fi
      # Never forward an option-looking token to the verifier: it could be
      # parsed as a verifier flag (for example --cell=...) and narrow the gate.
      case "$2" in
        ''|-*)
          usage
          die "--backup-evidence requires an artifact directory path; '$2' looks like an option (prefix a relative path with ./)"
          ;;
      esac
      if [ "${#BACKUP_EVIDENCE[@]}" -ge 2 ]; then
        usage
        die "--backup-evidence may be specified at most twice"
      fi
      BACKUP_EVIDENCE+=("$2")
      shift 2
      ;;
    --no-schema-change)
      NO_SCHEMA_CHANGE=true
      shift
      ;;
    --)
      shift
      while [ "$#" -gt 0 ]; do
        POSITIONAL+=("$1")
        shift
      done
      ;;
    -*)
      usage
      die "unknown option '$1'"
      ;;
    *)
      POSITIONAL+=("$1")
      shift
      ;;
  esac
done

if [ "${#POSITIONAL[@]}" -ne 2 ]; then
  usage
  exit 2
fi
CELL="${POSITIONAL[0]}"
VERSION="${POSITIONAL[1]}"

if [ "$NO_SCHEMA_CHANGE" = true ] && [ "${#BACKUP_EVIDENCE[@]}" -gt 0 ]; then
  usage
  die "--no-schema-change and --backup-evidence are mutually exclusive"
fi
if [ "$NO_SCHEMA_CHANGE" = false ] && [ "${#BACKUP_EVIDENCE[@]}" -eq 0 ]; then
  die "rollout gate required; see docs/runbooks.md and provide --backup-evidence artifact directories for civo-sandbox-use1-backup and civo-sandbox-usw2-dev, or attest --no-schema-change"
fi

# Version must match Witself's release tag shape. Anything else and the
# user is probably trying to bump the wrong thing with the wrong tool.
if ! [[ "$VERSION" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
  echo "error: version '$VERSION' must look like MAJOR.MINOR.PATCH (no 'v' prefix, matches the git tag suffix)" >&2
  exit 2
fi

REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
VALUES="$REPO_ROOT/.gitops/cells/$CELL/values.yaml"

if [ ! -f "$VALUES" ]; then
  echo "error: no cell values file at $VALUES" >&2
  echo "known cells:" >&2
  find "$REPO_ROOT/.gitops/cells" -mindepth 1 -maxdepth 1 -type d -exec basename {} \; >&2
  exit 2
fi

if ! command -v yq >/dev/null 2>&1; then
  die "yq is required (brew install yq)"
fi

# This gate must complete before either values pin is edited. The verifier is
# deliberately resolved only from the operator's override or normal PATH.
if [ "$NO_SCHEMA_CHANGE" = true ]; then
  echo "warning: operator attests release $VERSION cannot advance the database schema; backup evidence verification skipped" >&2
else
  ADMIN="${WITSELF_ADMIN_BIN:-witself-admin}"
  ADMIN_PATH="$(command -v "$ADMIN" 2>/dev/null || true)"
  if [ -z "$ADMIN_PATH" ] || [ ! -f "$ADMIN_PATH" ] || [ ! -x "$ADMIN_PATH" ]; then
    die "backup evidence verifier is not executable; set WITSELF_ADMIN_BIN to an executable witself-admin binary"
  fi
  if ! "$ADMIN" backup-evidence verify --release "$VERSION" -- "${BACKUP_EVIDENCE[@]}"; then
    die "backup evidence verification failed; rollout aborted before any values file edit"
  fi
  echo "backup evidence verified for release $VERSION (${#BACKUP_EVIDENCE[@]} artifact directories)"
fi

# The two paths this script may touch. Any other chartVersion (upstream
# helm charts under platform.*) is silently left alone — that's the point.
yq -i ".apps.witselfServer.chartVersion = \"$VERSION\"" "$VALUES"
yq -i ".apps.witselfServer.imageTag = \"$VERSION\"" "$VALUES"

# Diff surface check: if any line outside our two paths changed, something
# is wrong with the script — bail before we commit noise.
if ! git -C "$REPO_ROOT" diff --unified=0 "$VALUES" | grep -E '^[+-][^+-]' | grep -vE '^[+-] *(chartVersion|imageTag): *"?[0-9]+\.[0-9]+\.[0-9]+"?' > /tmp/roll-cell.stray 2>&1 && [ -s /tmp/roll-cell.stray ]; then
  # No stray lines — good. (The grep -v filter emptied the output.)
  :
fi
STRAY="$(git -C "$REPO_ROOT" diff --unified=0 "$VALUES" 2>&1 | awk '/^[+-][^+-]/' | grep -Ev 'chartVersion:|imageTag:' || true)"
if [ -n "$STRAY" ]; then
  echo "error: unexpected changes outside apps.witselfServer:" >&2
  echo "$STRAY" >&2
  git -C "$REPO_ROOT" checkout -- "$VALUES"
  exit 2
fi

echo "rolled $CELL to $VERSION (apps.witselfServer.chartVersion + imageTag)"
echo "diff:"
git -C "$REPO_ROOT" --no-pager diff "$VALUES"
echo
echo "next: review this cell in the intended rollout wave; commit + push to main"
echo "      triggers reconciliation only for provisioned cells watching this repo."
