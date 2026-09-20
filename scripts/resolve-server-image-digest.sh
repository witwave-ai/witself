#!/usr/bin/env bash
# Resolve the release tag to the digest of the complete GHCR manifest/index.
# Anonymous registry API access only: never consult Docker or gh credentials.
set -euo pipefail

die() { echo "error: image digest resolution failed: $*" >&2; exit 2; }
repository=ghcr.io/witwave-ai/images/witself-server
if [ "$#" -eq 3 ] && [ "$2" = --repository ]; then
  repository=$3
elif [ "$#" -ne 1 ]; then
  die "expected a release version [--repository ghcr.io/OWNER/IMAGE]"
fi
version=$1
[[ "$version" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]] || die "invalid release version"
[[ "$repository" =~ ^ghcr\.io/[a-z0-9]+([._-][a-z0-9]+)*(/[a-z0-9]+([._-][a-z0-9]+)*)+$ ]] || die "repository must be a canonical ghcr.io image repository without a tag or digest"
repository=${repository#ghcr.io/}
for tool in curl jq shasum; do
  command -v "$tool" >/dev/null 2>&1 || die "$tool is required"
done

work_dir=$(mktemp -d "${TMPDIR:-/tmp}/witself-image-digest.XXXXXX")
trap 'rm -rf -- "$work_dir"' EXIT
accept='application/vnd.oci.image.index.v1+json, application/vnd.docker.distribution.manifest.list.v2+json, application/vnd.oci.image.manifest.v1+json, application/vnd.docker.distribution.manifest.v2+json'

# -q must be first: a user's .curlrc must not add credentials, output, or redirects.
if ! auth=$(curl -q --fail --silent --show-error --connect-timeout 10 --max-time 60 \
  --get --data-urlencode service=ghcr.io \
  --data-urlencode "scope=repository:$repository:pull" https://ghcr.io/token); then
  die "registry authentication endpoint unreachable or refused the request"
fi
if ! bearer=$(printf '%s' "$auth" | jq -er '.token // .access_token | select(type == "string" and length > 0)' 2>/dev/null); then
  die "registry returned no anonymous pull token"
fi
unset auth
# A token is data, not curl configuration. Only RFC 6750 bearer characters pass.
[[ "$bearer" =~ ^[A-Za-z0-9._~+/-]+=*$ ]] || die "registry returned an invalid pull token"
if ! curl -q --fail --silent --show-error --connect-timeout 10 --max-time 60 \
  --config - --header "Accept: $accept" \
  --dump-header "$work_dir/headers" --output "$work_dir/manifest" \
  "https://ghcr.io/v2/$repository/manifests/$version" <<EOF_AUTH
header = "Authorization: Bearer $bearer"
EOF_AUTH
then
  die "registry manifest unreachable or refused the release tag"
fi
unset bearer

# Reset on HTTP status lines so proxy/100 responses cannot supply the digest.
digest=$(awk '
  /^HTTP\// { digest=""; count=0; status=$2 }
  tolower($1) == "docker-content-digest:" { sub(/\r$/, ""); digest=$2; count++; if (NF != 2) count+=2 }
  END { if (status == 200 && count == 1) print digest }
' "$work_dir/headers")
[[ "$digest" =~ ^sha256:[a-f0-9]{64}$ ]] || die "missing or malformed manifest digest header"
actual="sha256:$(shasum -a 256 "$work_dir/manifest" | awk '{print $1}')"
[ "$digest" = "$actual" ] || die "manifest bytes do not match the registry digest"

# Keep the index/list digest for multi-platform releases; selecting a child
# manifest would silently pin the cell to one architecture.
if ! jq -e '
  .schemaVersion == 2 and
  (if .mediaType == "application/vnd.oci.image.index.v1+json" or
      .mediaType == "application/vnd.docker.distribution.manifest.list.v2+json"
   then (.manifests | type == "array" and length > 0)
   elif .mediaType == "application/vnd.oci.image.manifest.v1+json" or
        .mediaType == "application/vnd.docker.distribution.manifest.v2+json"
   then (.config | type == "object") and (.layers | type == "array")
   else false end)
' "$work_dir/manifest" >/dev/null 2>&1; then
  die "unsupported or malformed image manifest"
fi
printf '%s\n' "$digest"
