#!/usr/bin/env bash
# roll-cell — bump a cell's witself-server chart+image to a released Witself
# version. Scoped: only apps.witselfServer.chartVersion, imageTag and
# imageDigest are changed through the cell-values generator. --backup-image
# also pins the purpose-built PostgreSQL backup image at the same release;
# without it, any existing backup pin is preserved. Upstream chart
# versions (cert-manager, external-dns, external-secrets, keda,
# metrics-server) are OFF-LIMITS to this script by design.
#
# Usage: scripts/roll-cell.sh <cell-name> <version> [gate options]
# Example shape: scripts/roll-cell.sh CELL_NAME RELEASED_VERSION --no-schema-change
set -euo pipefail

usage() {
  echo "usage: $0 <cell-name> <version> [--backup-image] (--backup-evidence DIR [--backup-evidence DIR] | --no-schema-change)" >&2
}

die() {
  echo "error: $*" >&2
  exit 2
}

BACKUP_EVIDENCE=()
NO_SCHEMA_CHANGE=false
PIN_BACKUP_IMAGE=false
POSITIONAL=()
while [ "$#" -gt 0 ]; do
  case "$1" in
    --backup-image)
      PIN_BACKUP_IMAGE=true
      shift
      ;;
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
  die "rollout gate required; see docs/runbooks.md and provide --backup-evidence artifact directories for civo-sandbox-use1-backup and civo-sandbox-use1-serving, or attest --no-schema-change"
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

# This gate must complete before registry access or any values pin is edited. The verifier is
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

# Older release charts silently ignore image.digest. Check the exact local
# release tag before resolving or writing a pin that its chart cannot enforce.
for tool in git jq; do
  command -v "$tool" >/dev/null 2>&1 || die "$tool is required to verify release chart digest support"
done
RELEASE_REF="refs/tags/v${VERSION}"
if ! git -C "$REPO_ROOT" rev-parse --verify --quiet "${RELEASE_REF}^{commit}" >/dev/null; then
  die "release tag v${VERSION} is not available locally; fetch the release tag before rolling"
fi
if ! RELEASE_SCHEMA=$(git -C "$REPO_ROOT" show "${RELEASE_REF}:charts/witself-server/values.schema.json" 2>/dev/null); then
  die "release v${VERSION} chart has no readable values.schema.json; cannot verify image.digest support"
fi
if ! printf '%s' "$RELEASE_SCHEMA" | jq -e '.properties.image.properties.digest' >/dev/null 2>&1; then
  die "release v${VERSION} chart does not declare image.digest; refusing an unenforceable digest pin"
fi

BACKUP_REPOSITORY=ghcr.io/witwave-ai/images/witself-postgres-backup
if [ "$PIN_BACKUP_IMAGE" = true ]; then
  if ! BACKUP_SCHEMA=$(git -C "$REPO_ROOT" show "${RELEASE_REF}:.gitops/charts/apps/values.schema.json" 2>/dev/null); then
    die "release v${VERSION} apps chart has no readable values.schema.json; cannot verify backup image pin support"
  fi
  if ! printf '%s' "$BACKUP_SCHEMA" | jq -e '
    [.properties.apps.properties.civoPostgres.properties.backup.properties.image |
      .. | objects | select(.type? == "object" and .properties.repository? and .properties.tag? and .properties.digest?)] |
    length > 0
  ' >/dev/null 2>&1; then
    die "release v${VERSION} apps chart does not declare backup image repository, tag, and digest; refusing an unenforceable backup pin"
  fi
fi

# Resolve and verify the complete registry manifest before the generator can
# change any pin. A failed lookup never leaves a tag-only partial roll.
if ! DIGEST=$(bash "$REPO_ROOT/scripts/resolve-server-image-digest.sh" "$VERSION"); then
  die "image digest resolution failed; rollout aborted before any values file edit"
fi
ROLL_ARGS=(--root "$REPO_ROOT" --roll-cell "$CELL" --version "$VERSION" --image-digest "$DIGEST")
if [ "$PIN_BACKUP_IMAGE" = true ]; then
  if ! BACKUP_DIGEST=$(bash "$REPO_ROOT/scripts/resolve-server-image-digest.sh" "$VERSION" --repository "$BACKUP_REPOSITORY"); then
    die "backup image digest resolution failed; rollout aborted before any values file edit"
  fi
  ROLL_ARGS+=(--backup-image-repository "$BACKUP_REPOSITORY" --backup-image-tag "$VERSION" --backup-image-digest "$BACKUP_DIGEST")
fi

BEFORE=$(mktemp "${TMPDIR:-/tmp}/witself-roll-cell-before.XXXXXX")
trap 'rm -f -- "$BEFORE"' EXIT
cp "$VALUES" "$BEFORE"
# The generator refuses existing drift and atomically changes only this cell.
bash "$REPO_ROOT/scripts/gitops-cell-values.sh" "${ROLL_ARGS[@]}"

ROLL_SUMMARY="apps.witselfServer.chartVersion + imageTag + imageDigest"
if [ "$PIN_BACKUP_IMAGE" = true ]; then
  ROLL_SUMMARY+="; backup image pinned to $BACKUP_REPOSITORY:$VERSION by digest"
fi
echo "rolled $CELL to $VERSION ($ROLL_SUMMARY)"
echo "diff:"
status=0
diff -u --label "$CELL/values.yaml (before)" --label "$CELL/values.yaml (after)" \
  "$BEFORE" "$VALUES" || status=$?
[ "$status" -le 1 ] || die "could not display values diff"
echo
echo "next: review this cell in the intended rollout wave; commit + push to main"
echo "      triggers reconciliation only for provisioned cells watching this repo."
