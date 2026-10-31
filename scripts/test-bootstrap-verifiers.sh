#!/usr/bin/env bash
# Frozen B1 corpus replay through the exact sourceable installer functions.
# This adjudicator is sourceable without running the corpus so its contract can
# be tested independently of successful verifier executions.
ws_bootstrap_adjudicate() {
  local name=$1 positive=$2 expected=$3 result=$4 status=$5
  case "$result" in
    accepted|no_trust_root|document_too_large|envelope_too_large|invalid_json|duplicate_key|profile_violation|unknown_critical|unknown_field|invalid_document|wrong_kind|wrong_algorithm|invalid_key|invalid_encoding|signature_length|signature_count|signature_invalid|unknown_key|revoked_key|wrong_signer|state_invalid|state_io|sequence_invalid|replay|rollback_denied|wrong_tag|wrong_platform|schema_invalid|postgresql_invalid|extensions_unsupported|roles_invalid|path_invalid|inventory_invalid|mode_invalid|not_installed|mixed_set|expired|not_yet_valid|unknown_clock) ;;
    *) printf 'shell parity: %s returned an unknown reason\n' "$name" >&2; return 1 ;;
  esac
  if [[ $result != "$expected" || ( $positive == true && ( $result != accepted || $status != 0 ) ) || ( $positive != true && ( $result == accepted || $status == 0 ) ) ]]; then
    printf 'shell parity: %s got %s status=%s want %s\n' "$name" "$result" "$status" "$expected" >&2
    return 1
  fi
}

# Sourcing defines only the adjudicator; execution still replays every case.
if [[ ${BASH_SOURCE[0]} != "$0" ]]; then return 0; fi
set -euo pipefail
script_dir=$(CDPATH='' cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
repo_root=$(CDPATH='' cd -- "$script_dir/.." && pwd)
command -v openssl >/dev/null 2>&1 || { printf 'NOT_RUN openssl_unavailable\n'; exit 1; }
WS_BOOTSTRAP_LIBRARY=1 source "$repo_root/install.sh"
work=$(mktemp -d "${TMPDIR:-/tmp}/witself-bootstrap-shell.XXXXXX")
trap 'rm -rf "$work"' EXIT INT TERM
vectors="$repo_root/internal/localcell/trust/testdata/vectors"
keys="$repo_root/internal/localcell/trust/testdata/keys"
ws_cs_json "$vectors/index.json" index "$work" '' >"$work/cases"
# No reason exceptions for this corpus. Bootstrap persistence and full Unicode
# normalization remain separate B3 boundaries; every frozen case is exercised.
mkdir "$work/cache"
count=0; positive_count=0; negative_count=0; signature_count=0
while IFS='|' read -r name kind document envelope expected positive untrusted rollback tag os arch now roots setup negative_signature; do
  state="$work/state"
  rm -rf "$state"; mkdir "$state"
  WS_CS_NOW=${now:-2026-10-27T12:00:00Z}
  WS_CS_TAG=$tag; WS_CS_OS=$os; WS_CS_ARCH=$arch
  WS_UNTRUSTED_CLOCK=0; WS_CS_ALLOW_ROLLBACK=0
  [[ $rollback != true ]] || WS_CS_ALLOW_ROLLBACK=1
  [[ $roots != *,* ]] || { printf 'shell parity: multiple bootstrap roots unsupported\n' >&2; exit 1; }
  cache_id=$(printf '%s\n' "$roots|$setup|$WS_CS_NOW|$WS_CS_TAG|$WS_CS_OS|$WS_CS_ARCH|$WS_CS_ALLOW_ROLLBACK" | openssl dgst -sha256 | sed 's/^.*= //')
  if [[ -d $work/cache/$cache_id ]]; then
    cp -R "$work/cache/$cache_id/." "$state/"
  else
  result=$(ws_cs_init "$state" "$keys/TEST-ONLY-$roots.public.pem") || { printf 'shell parity: root setup refused %s %s\n' "$name" "$result" >&2; exit 1; }
  if [[ -n $setup ]]; then
    IFS=';' read -r -a refs <<<"$setup"
    for ref in "${refs[@]}"; do
      IFS=',' read -r sk sd se <<<"$ref"
      result=$(ws_cs_apply "$state" "$sk" "$vectors/$sd" "$vectors/$se") || { printf 'shell parity: setup refused %s %s\n' "$name" "$result" >&2; exit 1; }
      [[ $result == accepted ]] || exit 1
    done
  fi
  mkdir "$work/cache/$cache_id"
  cp -R "$state/." "$work/cache/$cache_id/"
  fi
  [[ $untrusted != true ]] || WS_UNTRUSTED_CLOCK=1
  status=0
  result=$(ws_cs_apply "$state" "$kind" "$vectors/$document" "$vectors/$envelope") || status=$?
  ws_bootstrap_adjudicate "$name" "$positive" "$expected" "$result" "$status"
  count=$((count+1))
  if [[ $positive == true ]]; then positive_count=$((positive_count+1)); else negative_count=$((negative_count+1)); fi
  if [[ $negative_signature == true ]]; then signature_count=$((signature_count+1)); fi
done <"$work/cases"
printf 'shell bootstrap parity: %s cases (%s positive, %s negative; %s negative-signature), accepted\n' "$count" "$positive_count" "$negative_count" "$signature_count"
