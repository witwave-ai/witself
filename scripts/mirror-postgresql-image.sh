#!/usr/bin/env bash
set -euo pipefail

# Retag the reviewed image bytes, including every child of an upstream index.
# Never rebuild, select a platform, or manufacture an index for a single image:
# either operation would change the cell's existing content-addressed identity.
source_root=$(CDPATH='' cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
fail() {
  printf 'PostgreSQL mirror: %s\n' "$1" >&2
  # Keep registry/auth failures diagnosable, including an initial GHCR denial.
  # Early validation failures happen before any log directory exists.
  local stage log_file
  if [[ -n ${work_dir:-} ]]; then
    for stage in version source copy destination; do
      log_file="$work_dir/$stage.log"
      if [[ -s $log_file ]]; then
        printf 'PostgreSQL mirror: skopeo %s log:\n' "$stage" >&2
        # Registry errors and copy progress can contain content digests.
        sed -E 's/[[:xdigit:]]{64}/[redacted-digest]/g' "$log_file" >&2 || true
      fi
    done
  fi
  exit 1
}
if (( $# < 2 || $# > 4 )); then
  fail 'usage: mirror-postgresql-image.sh VERSION CELL [CONFIG [dir:PATH|oci:PATH]]'
fi
version=$1
cell=$2
config=${3:-$source_root/images/postgresql/mirror.json}
[[ $version =~ ^[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$ ]] || fail 'invalid release version'
tag="$version-$cell"
(( ${#tag} <= 128 )) || fail 'release image tag is too long'
for dependency in jq skopeo python3; do
  command -v "$dependency" >/dev/null 2>&1 || fail "missing dependency: $dependency"
done

# The descriptor is also consumed from the target release by roll-cell.sh.
# Its repository identities are fixed; a malformed descriptor cannot redirect
# this publisher or turn a mutable upstream tag into the source of a release.
jq -e '
  .schema_version == 1 and
  .source_registry == "registry-1.docker.io" and
  .source_repository == "bitnami/postgresql" and
  .destination_repository == "ghcr.io/witwave-ai/images/postgresql" and
  (.cells | type == "object" and length > 0) and
  all(.cells | keys[]; test("^[a-z0-9]+(-[a-z0-9]+)*$")) and
  all(.cells[];
    (.digest | type == "string" and test("^sha256:[0-9a-f]{64}$")) and
    (.upstream_tag | type == "string" and length <= 128 and
      test("^[A-Za-z0-9_][A-Za-z0-9_.-]*$")) and
    has("upstream_version") and
    (.upstream_version == null or
      (.upstream_version | type == "string" and length <= 128 and
        test("^[0-9]+[.][0-9]+([.][0-9]+)?(-[A-Za-z0-9_.-]+)?$"))))
' "$config" >/dev/null 2>&1 || fail 'invalid source descriptor'
jq -e --arg cell "$cell" '.cells | has($cell)' "$config" >/dev/null || fail 'unsupported cell'
digest=$(jq -er --arg cell "$cell" '.cells[$cell].digest' "$config")
repository=$(jq -er '.destination_repository' "$config")
source_ref="docker://registry-1.docker.io/bitnami/postgresql@$digest"
destination_ref="docker://$repository:$tag"
local_destination=false
if (( $# == 4 )); then
  # Local transports provide a pre-tag copy/digest tripwire without publishing.
  # Always read upstream in this mode so an existing mirror cannot mask outages.
  case "$4" in
    dir:/*|oci:/*) destination_ref=$4; local_destination=true ;;
    *) fail 'destination override must be an absolute dir: or oci: path' ;;
  esac
fi

work_dir=$(mktemp -d "${TMPDIR:-/tmp}/witself-postgresql-mirror.XXXXXX")
trap 'rm -rf -- "$work_dir"' EXIT
manifest_digest() {
  python3 - "$1" <<'PY'
import hashlib
import sys
with open(sys.argv[1], 'rb') as manifest:
    print('sha256:' + hashlib.sha256(manifest.read()).hexdigest())
PY
}
# Digest preservation was introduced in skopeo 1.5. Check explicitly before
# invoking a copy, including on the ubuntu-latest PR runner.
if ! skopeo --version >"$work_dir/version" 2>"$work_dir/version.log"; then
  fail 'could not determine skopeo version'
fi
python3 - "$work_dir/version" <<'PYVERSION' || fail 'skopeo 1.5 or newer is required'
import re
import sys
from pathlib import Path
match = re.search(r'\b(\d+)\.(\d+)(?:\.\d+)?\b', Path(sys.argv[1]).read_text())
sys.exit(0 if match and tuple(map(int, match.groups())) >= (1, 5) else 1)
PYVERSION

mirrored_new=true
if [[ $local_destination == false ]]; then
  # Resolve immutable GHCR bytes first. A transient registry/auth failure must
  # not be misclassified as a missing digest and trigger an upstream copy.
  if skopeo inspect --raw "docker://$repository@$digest" >"$work_dir/source.json" 2>"$work_dir/source.log"; then
    source_ref="docker://$repository@$digest"
    mirrored_new=false
    [[ $(manifest_digest "$work_dir/source.json") == "$digest" ]] || fail 'existing mirror manifest digest mismatch'
  elif ! python3 - "$work_dir/source.log" <<'PYMISSING'
import re
import sys
from pathlib import Path
message = Path(sys.argv[1]).read_text().lower()
sys.exit(0 if re.search(r'\b(?:manifest unknown|name unknown|manifest_unknown|name_unknown)\b', message) else 1)
PYMISSING
  then
    fail 'could not determine whether the digest is already mirrored'
  fi
fi
if [[ $mirrored_new == true ]]; then
  if ! skopeo inspect --raw "$source_ref" >"$work_dir/source.json" 2>"$work_dir/source.log"; then
    fail 'could not read the digest-pinned upstream manifest'
  fi
  [[ $(manifest_digest "$work_dir/source.json") == "$digest" ]] || fail 'upstream manifest digest mismatch'
fi
kind=$(jq -er '
  if .schemaVersion != 2 then error("unsupported schema")
  elif (.mediaType == "application/vnd.docker.distribution.manifest.list.v2+json" or
        .mediaType == "application/vnd.oci.image.index.v1+json") and
       (.manifests | type == "array" and length > 0) then "index"
  elif .mediaType == "application/vnd.docker.distribution.manifest.v2+json" or
       .mediaType == "application/vnd.oci.image.manifest.v1+json" then "manifest"
  else error("unsupported manifest") end
' "$work_dir/source.json" 2>/dev/null) || fail 'unsupported upstream manifest format'

# --all copies every platform when the source is an index. --preserve-digests
# fails instead of silently converting media types or recompressing layers.
if ! skopeo copy --all --preserve-digests --retry-times 3 \
  "$source_ref" "$destination_ref" >"$work_dir/copy.log" 2>&1; then
  fail 'copy failed while preserving upstream image digests'
fi
if ! skopeo inspect --raw "$destination_ref" >"$work_dir/destination.json" 2>"$work_dir/destination.log"; then
  fail 'could not read the published manifest'
fi
[[ $(manifest_digest "$work_dir/destination.json") == "$digest" ]] || fail 'published manifest digest mismatch'

# Consumers sign and attest the verified immutable reference, never the tag.
if [[ -n ${GITHUB_OUTPUT:-} ]]; then
  {
    printf 'image=%s\n' "$repository"
    printf 'digest=%s\n' "$digest"
    printf 'tag=%s\n' "$tag"
    printf 'kind=%s\n' "$kind"
    printf 'mirrored_new=%s\n' "$mirrored_new"
  } >>"$GITHUB_OUTPUT"
fi
printf 'Verified PostgreSQL %s mirror for %s at release %s\n' "$kind" "$cell" "$version"
