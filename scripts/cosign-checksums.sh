#!/usr/bin/env bash
set -euo pipefail

if [[ $# -ne 3 ]]; then
  echo "usage: cosign-checksums.sh ARTIFACT BUNDLE CERTIFICATE" >&2
  exit 2
fi

artifact="$1"
bundle="$2"
certificate="$3"
legacy_signature="${artifact}.sig"
issuer='https://token.actions.githubusercontent.com'

if [[ "${GITHUB_REF_TYPE:-}" != "tag" || ! "${GITHUB_REF_NAME:-}" =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
  echo "checksum signing requires an exact stable GitHub tag ref" >&2
  exit 1
fi
if [[ "${GITHUB_REF:-}" != "refs/tags/${GITHUB_REF_NAME}" ]]; then
  echo "GitHub tag ref metadata is inconsistent" >&2
  exit 1
fi
identity="https://github.com/witwave-ai/witself/.github/workflows/release.yml@${GITHUB_REF}"

if [[ ! -s "$artifact" ]]; then
  echo "checksum artifact is missing or empty: $artifact" >&2
  exit 1
fi
if [[ "$bundle" != "${artifact}.sigstore.json" ]]; then
  echo "unexpected bundle path: $bundle" >&2
  exit 1
fi
if [[ "$certificate" != "${artifact}.pem" ]]; then
  echo "unexpected certificate path: $certificate" >&2
  exit 1
fi

for dependency in cosign jq openssl; do
  if ! command -v "$dependency" >/dev/null 2>&1; then
    echo "required checksum signing dependency is unavailable: $dependency" >&2
    exit 1
  fi
done

certificate_der="$(mktemp "${TMPDIR:-/tmp}/witself-checksums-cert.XXXXXX")"
trap 'rm -f "$certificate_der"' EXIT

# Cosign v3 creates the sole canonical signature. The legacy companions below
# are derived from this exact bundle; no second signing operation occurs.
cosign sign-blob "--bundle=$bundle" "$artifact" --yes
if [[ ! -s "$bundle" ]]; then
  echo "cosign did not produce a signature bundle: $bundle" >&2
  exit 1
fi

jq -er \
  '.messageSignature.signature | select(type == "string" and length > 0)' \
  "$bundle" >"$legacy_signature"
jq -er \
  '(
    .verificationMaterial.certificate.rawBytes //
    .verificationMaterial.x509CertificateChain.certificates[0].rawBytes //
    empty
  ) | select(type == "string" and length > 0)' \
  "$bundle" | openssl base64 -d -A >"$certificate_der"
openssl x509 -inform DER -in "$certificate_der" -out "$certificate"

if [[ ! -s "$legacy_signature" || ! -s "$certificate" ]]; then
  echo "failed to derive legacy checksum signature companions" >&2
  exit 1
fi

# Fail the signer, and therefore the release before publication begins, unless
# both the canonical and compatibility representations verify successfully.
cosign verify-blob \
  --bundle "$bundle" \
  --certificate-identity "$identity" \
  --certificate-oidc-issuer "$issuer" \
  "$artifact"
cosign verify-blob \
  --certificate "$certificate" \
  --signature "$legacy_signature" \
  --certificate-identity "$identity" \
  --certificate-oidc-issuer "$issuer" \
  "$artifact"
