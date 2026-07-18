#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
wrapper="$repo_root/scripts/cosign-checksums.sh"
work="$(mktemp -d "${TMPDIR:-/tmp}/witself-cosign-test.XXXXXX")"
trap 'rm -rf "$work"' EXIT

fail() {
  echo "checksum signer test failed: $*" >&2
  exit 1
}

for dependency in cosign goreleaser git go jq openssl; do
  command -v "$dependency" >/dev/null 2>&1 || fail "missing dependency: $dependency"
done

mkdir -p "$work/fake-bin" "$work/project"
openssl req -x509 -newkey rsa:2048 -nodes \
  -keyout "$work/fixture.key" \
  -out "$work/fixture.pem" \
  -days 1 \
  -subj /CN=witself-cosign-test >/dev/null 2>&1
openssl x509 -in "$work/fixture.pem" -outform DER -out "$work/fixture.der"
certificate_raw="$(openssl base64 -A -in "$work/fixture.der")"
jq -n --arg certificate_raw "$certificate_raw" '
  {
    mediaType: "application/vnd.dev.sigstore.bundle.v0.3+json",
    messageSignature: {signature: "ZmFrZS1zaWduYXR1cmU="},
    verificationMaterial: {
      x509CertificateChain: {certificates: [{rawBytes: $certificate_raw}]}
    }
  }
' >"$work/fixture.sigstore.json"

cat >"$work/fake-bin/cosign" <<'FAKE_COSIGN'
#!/usr/bin/env bash
set -euo pipefail

printf '%s\n' "$*" >>"$FAKE_COSIGN_LOG"
case "${1:-}" in
  sign-blob)
    bundle=""
    for argument in "$@"; do
      if [[ "$argument" == --bundle=* ]]; then
        bundle="${argument#--bundle=}"
      fi
    done
    [[ -n "$bundle" ]]
    cp "$FAKE_COSIGN_BUNDLE" "$bundle"
    ;;
  verify-blob)
    [[ "${FAKE_COSIGN_FAIL_VERIFY:-false}" != true ]]
    ;;
  *)
    echo "unexpected fake cosign command: ${1:-}" >&2
    exit 1
    ;;
esac
FAKE_COSIGN
chmod +x "$work/fake-bin/cosign"

cat >"$work/project/go.mod" <<'GO_MOD'
module example.com/witself-cosign-test

go 1.25
GO_MOD
cat >"$work/project/main.go" <<'GO_SOURCE'
package main

func main() {}
GO_SOURCE
cat >"$work/project/.goreleaser.yaml" <<GO_RELEASER
version: 2
project_name: witself-cosign-test
builds:
  - id: fixture
    main: .
    binary: fixture
    env:
      - CGO_ENABLED=0
    goos:
      - linux
    goarch:
      - amd64
archives:
  - ids:
      - fixture
checksum:
  name_template: checksums.txt
signs:
  - id: checksums
    cmd: bash
    signature: "\${artifact}.sigstore.json"
    certificate: "\${artifact}.pem"
    args:
      - "$wrapper"
      - "\${artifact}"
      - "\${signature}"
      - "\${certificate}"
    artifacts: checksum
release:
  extra_files:
    - glob: dist/checksums.txt.sig
changelog:
  disable: true
GO_RELEASER

(
  cd "$work/project"
  git init -q
  git config user.name witself-test
  git config user.email witself-test@example.invalid
  git add .
  git commit -qm fixture
)

export FAKE_COSIGN_BUNDLE="$work/fixture.sigstore.json"
export FAKE_COSIGN_LOG="$work/cosign.log"
export PATH="$work/fake-bin:$PATH"
export GITHUB_REF_TYPE=tag
export GITHUB_REF_NAME=v9.9.9
export GITHUB_REF=refs/tags/v9.9.9

if ! (
  cd "$work/project"
  goreleaser release --snapshot --clean --parallelism 1 --timeout 2m
) >"$work/signed-run.log" 2>&1; then
  cat "$work/signed-run.log" >&2
  fail "signed snapshot failed"
fi

dist="$work/project/dist"
[[ -s "$dist/checksums.txt.sigstore.json" ]] || fail "bundle was not tracked"
[[ -s "$dist/checksums.txt.pem" ]] || fail "certificate was not tracked"
[[ -s "$dist/checksums.txt.sig" ]] || fail "legacy signature was not created"
openssl x509 -in "$dist/checksums.txt.pem" -noout >/dev/null 2>&1 || fail "invalid PEM certificate"
[[ "$(<"$dist/checksums.txt.sig")" == "ZmFrZS1zaWduYXR1cmU=" ]] || fail "wrong legacy signature"
[[ "$(grep -c '^sign-blob ' "$FAKE_COSIGN_LOG")" -eq 1 ]] || fail "expected one signing operation"
[[ "$(grep -c '^verify-blob ' "$FAKE_COSIGN_LOG")" -eq 2 ]] || fail "expected two verification operations"
expected_identity="https://github.com/witwave-ai/witself/.github/workflows/release.yml@refs/tags/v9.9.9"
[[ "$(grep -Fc -- "--certificate-identity $expected_identity" "$FAKE_COSIGN_LOG")" -eq 2 ]] || \
  fail "expected both verifications to pin the exact release workflow identity"
if grep -Fq -- '--certificate-identity-regexp' "$FAKE_COSIGN_LOG"; then
  fail "signer used a broad certificate identity regexp"
fi
jq -e '[.[] | select(.type == "Signature" and .name == "checksums.txt.sigstore.json")] | length == 1' \
  "$dist/artifacts.json" >/dev/null || fail "bundle missing from GoReleaser artifacts"
jq -e '[.[] | select(.type == "Certificate" and .name == "checksums.txt.pem")] | length == 1' \
  "$dist/artifacts.json" >/dev/null || fail "certificate missing from GoReleaser artifacts"

# A failed verification must make the wrapper fail before GoReleaser can enter
# its publishing pipe.
failure_artifact="$work/failure-checksums.txt"
cp "$dist/checksums.txt" "$failure_artifact"
if FAKE_COSIGN_FAIL_VERIFY=true bash "$wrapper" \
  "$failure_artifact" \
  "${failure_artifact}.sigstore.json" \
  "${failure_artifact}.pem" >"$work/failed-verify.log" 2>&1; then
  fail "wrapper accepted a failed verification"
fi

if GITHUB_REF=refs/heads/main bash "$wrapper" \
  "$failure_artifact" \
  "${failure_artifact}.sigstore.json" \
  "${failure_artifact}.pem" >"$work/wrong-ref.log" 2>&1; then
  fail "wrapper accepted non-tag GitHub ref metadata"
fi

# Manual snapshots use --skip=sign. GoReleaser also skips publishing in
# snapshot mode, so the release.extra_files glob must tolerate the absent .sig.
if ! (
  cd "$work/project"
  goreleaser release --snapshot --clean --skip=sign --parallelism 1 --timeout 2m
) >"$work/unsigned-run.log" 2>&1; then
  cat "$work/unsigned-run.log" >&2
  fail "unsigned snapshot rejected the absent legacy extra file"
fi
[[ ! -e "$dist/checksums.txt.sig" ]] || fail "unsigned snapshot unexpectedly produced a legacy signature"

echo "checksum signer integration passed"
