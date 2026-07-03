#!/usr/bin/env bash
# roll-cell — bump a cell's witself-server chart+image to a released Witself
# version. Scoped: only the two Witself-owned fields (apps.witselfServer.
# chartVersion and apps.witselfServer.imageTag) are touched. Upstream chart
# versions (cert-manager, external-dns, external-secrets, keda,
# metrics-server) are OFF-LIMITS to this script by design.
#
# Usage: scripts/roll-cell.sh <cell-name> <version>
# Example: scripts/roll-cell.sh aws-sandbox-usw2-dev 0.0.84
set -euo pipefail

if [ "$#" -ne 2 ]; then
  echo "usage: $0 <cell-name> <version>" >&2
  exit 2
fi
CELL="$1"
VERSION="$2"

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
  echo "error: yq is required (brew install yq)" >&2
  exit 2
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
echo "next: git commit + push to trigger Argo reconciliation."
