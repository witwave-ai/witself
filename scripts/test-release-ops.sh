#!/usr/bin/env bash
# Literal shell probes and regexes intentionally contain unexpanded dollars.
# shellcheck disable=SC2016
set -euo pipefail

# All operations, including worktree mutations, are confined to this disposable
# fixture. Operational tools cannot reach the network or the operator's homes.
unset CONTROL_PLANE_EDGE_TOKEN WITSELF_FLEET_TOKEN CLOUDFLARE_API_TOKEN CLOUDFLARE_API_TOKEN_OLD RO_REDACT_EDGE RO_REDACT_CF
umask 077
SOURCE_ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)
command -v jq >/dev/null 2>&1 || { printf 'jq is required\n' >&2; exit 1; }
TEST_ROOT=$(mktemp -d "${TMPDIR:-/tmp}/witself-release-test.XXXXXX")
TEST_ROOT=$(cd "$TEST_ROOT" && pwd -P)
trap 'rm -rf "$TEST_ROOT"' EXIT
trap 'exit 1' INT TERM
fail() { printf 'release ops test: FAIL %s\n' "$*" >&2; exit 1; }
pass() { printf 'release ops test: PASS %s\n' "$*"; }
REPO=$TEST_ROOT/repo
OUT=$TEST_ROOT/out
BIN=$TEST_ROOT/bin
SECRET_DIR=$TEST_ROOT/private
mkdir -p "$REPO/scripts/lib" "$REPO/infra/cloudflare/control-plane" "$REPO/internal/store/migrations" "$OUT" "$BIN" "$SECRET_DIR" "$TEST_ROOT/home"
mkdir "$TEST_ROOT/tmp"
export TMPDIR=$TEST_ROOT/tmp
export WITSELF_HOME=$TEST_ROOT/home DSH_HOME=$TEST_ROOT/home
export GIT_AUTHOR_NAME='Release Fixture' GIT_AUTHOR_EMAIL='release@example.invalid'
export GIT_COMMITTER_NAME=$GIT_AUTHOR_NAME GIT_COMMITTER_EMAIL=$GIT_AUTHOR_EMAIL
export GIT_CONFIG_NOSYSTEM=1 GIT_CONFIG_GLOBAL=/dev/null
ORIGINAL_PATH=$PATH
REAL_GIT=$(command -v git)
REAL_DATE=$(command -v date)
export REAL_GIT REAL_DATE
cp "$SOURCE_ROOT/scripts/lib/release-ops.sh" "$REPO/scripts/lib/"
for script in "$SOURCE_ROOT"/scripts/release-*.sh; do cp "$script" "$REPO/scripts/"; done
printf 'fixture\n' > "$REPO/infra/cloudflare/control-plane/package.json"
printf 'select 1;\n' > "$REPO/internal/store/migrations/0001_a.sql"
"$REAL_GIT" -C "$REPO" init -q
"$REAL_GIT" -C "$REPO" add .
"$REAL_GIT" -C "$REPO" commit -qm fixture
"$REAL_GIT" -C "$REPO" tag v1.2.2
printf 'select 2;\n' > "$REPO/internal/store/migrations/0002_b.sql"
"$REAL_GIT" -C "$REPO" add .
"$REAL_GIT" -C "$REPO" commit -qm migration
"$REAL_GIT" -C "$REPO" tag v1.2.3
"$REAL_GIT" -C "$REPO" commit --allow-empty -qm release
"$REAL_GIT" -C "$REPO" tag v1.2.4
TAG_COMMIT=$("$REAL_GIT" -C "$REPO" rev-parse 'refs/tags/v1.2.4^{commit}')
printf 'operator checkout after release\n' > "$REPO/infra/cloudflare/control-plane/package.json"
"$REAL_GIT" -C "$REPO" add .
"$REAL_GIT" -C "$REPO" commit -qm post-release
"$REAL_GIT" -C "$REPO" update-ref refs/remotes/origin/main HEAD
[ "$("$REAL_GIT" -C "$REPO" rev-parse HEAD)" != "$TAG_COMMIT" ] || fail 'fixture HEAD must differ from release tag'
# Construct fixed fixtures without persisting their values in helper sources.
EDGE='edge-fake-'token-0123456789-abcdef
CF_FILE='cf-fake-'token-ABCDEFGHIJ
CF_ENV='cf-fake-env-'token-KLMNOPQRST
FLEET='fleet-fake-'token-0123456789
INHERITED_EDGE='edge-inherited-'fake-0123456789
INHERITED_FLEET='fleet-inherited-'fake-0123456789
CF_OLD='cf-old-inherited-'fake-0123456789
HEX=$(printf '%064d' 0); HEX=${HEX//0/e}
printf '%s\n' "$EDGE" > "$SECRET_DIR/edge"
printf '%s\n' "$CF_FILE" > "$SECRET_DIR/cf"
printf '%s\n' "$CF_ENV" > "$SECRET_DIR/cf-env"
printf '%s\n' "$FLEET" > "$SECRET_DIR/fleet"
cat > "$SECRET_DIR/deploy-env" <<'ENV'
# Four fake, non-secret identifiers.
CLOUDFLARE_ACCOUNT_ID=fixture-account
EMAIL_DIRECTORY_KV_ID=fixture-kv
AGENT_EMAIL_ROUTE_SIGNING_KEY_ID=fixture-key
CP_AGENT_EMAIL_MANAGED_DELIVERY_ACCOUNT_ALLOWLIST=fixture-allowlist
ENV
export RO_TEST_ROOT=$TEST_ROOT RO_TEST_OUT=$OUT RO_TEST_SECRETS=$SECRET_DIR
cat > "$BIN/dispatch" <<'STUB'
#!/usr/bin/env bash
set -euo pipefail
name=${0##*/}
# One append per call: redaction pipelines launch concurrent wrappers.
log=$name
for arg in "$@"; do log="$log <$arg>"; done
exported=$(export -p)
for key in CONTROL_PLANE_EDGE_TOKEN CLOUDFLARE_API_TOKEN CLOUDFLARE_API_TOKEN_OLD WITSELF_FLEET_TOKEN RO_REDACT_EDGE RO_REDACT_CF; do
  if [[ "$exported" == *" $key="* ]]; then log="$log $key=set"
  else log="$log $key=unset"; fi
done
unset exported
if [ "${RO_TEST_RECORD:-false}" = true ]; then printf '%s\n' "$log" >> "$RO_TEST_OUT/calls"; fi
scenario=${RO_TEST_SCENARIO:-normal}
count() {
  n=0
  if [ -f "$RO_TEST_OUT/$1" ]; then IFS= read -r n < "$RO_TEST_OUT/$1"; fi
  n=$((n + 1)); printf '%s\n' "$n" > "$RO_TEST_OUT/$1"
}
case "$name" in
  git)
    args=("$@"); i=0
    if [ "${args[0]:-}" = -C ]; then i=2; fi
    case "${args[$i]:-}" in fetch|push|ls-remote) exit 0 ;; esac
    if [ "${args[$i]:-} ${args[$((i+1))]:-}" = 'worktree add' ]; then
      [ "$scenario" != worktree-add-fail ] || exit 1
      if [ "$scenario" = dirty-worktree ]; then
        "$REAL_GIT" "$@"
        target=${args[$((i+4))]}
        printf 'dirty tracked file\n' >> "$target/infra/cloudflare/control-plane/package.json"
        exit 0
      fi
    fi
    if [ "$scenario" = zero-tags ] && [ "${args[$i]:-} ${args[$((i+1))]:-}" = 'tag --points-at' ]; then exit 0; fi
    exec "$REAL_GIT" "$@" ;;
  date)
    if [ "$*" = '-u +%H%M' ] || [ "$*" = '-u +%H:%M:%SZ' ]; then
      IFS= read -r seconds < "$RO_TEST_OUT/clock"
      minutes=$((seconds / 60 % 1440))
      if [ "$*" = '-u +%H%M' ]; then printf '%02d%02d\n' "$((minutes / 60))" "$((minutes % 60))"
      else printf '%02d:%02d:%02dZ\n' "$((minutes / 60))" "$((minutes % 60))" "$((seconds % 60))"; fi
    else exec "$REAL_DATE" "$@"; fi ;;
  sleep)
    IFS= read -r seconds < "$RO_TEST_OUT/clock"
    [ "$scenario" = frozen-clock ] || printf '%s\n' "$((seconds + $1))" > "$RO_TEST_OUT/clock" ;;
  npm)
    if [ "$*" = 'ci --silent' ]; then exit 0; fi
    [ "$*" = 'run deploy' ] || exit 93
    # Provenance probes are test instrumentation; strip credentials before Git.
    (
      unset CONTROL_PLANE_EDGE_TOKEN WITSELF_FLEET_TOKEN CLOUDFLARE_API_TOKEN CLOUDFLARE_API_TOKEN_OLD
      pwd -P > "$RO_TEST_OUT/npm-pwd"
      "$REAL_GIT" rev-parse --show-toplevel > "$RO_TEST_OUT/npm-toplevel"
      "$REAL_GIT" rev-parse HEAD > "$RO_TEST_OUT/npm-head"
    )
    {
      printf 'deploy cwd=%s umask=%s' "$PWD" "$(umask)"
      for key in CLOUDFLARE_ACCOUNT_ID EMAIL_DIRECTORY_KV_ID AGENT_EMAIL_ROUTE_SIGNING_KEY_ID CP_AGENT_EMAIL_MANAGED_DELIVERY_ACCOUNT_ALLOWLIST; do
        [ -n "${!key:-}" ] || exit 94
        printf ' %s=set' "$key"
      done
      printf ' canonical=%s signup=%s' "${CP_DEPLOY_CANONICAL_EMAIL_ACTIVE-unset}" "${CP_DEPLOY_SIGNUP_OPEN_ACTIVE-unset}"
      IFS= read -r expected < "$RO_TEST_SECRETS/cf"
      IFS= read -r env_expected < "$RO_TEST_SECRETS/cf-env"
      if [ "${CLOUDFLARE_API_TOKEN:-}" = "$expected" ]; then printf ' cf=file'
      elif [ "${CLOUDFLARE_API_TOKEN:-}" = "$env_expected" ]; then printf ' cf=env'
      else printf ' cf=other'; fi
      IFS= read -r expected < "$RO_TEST_SECRETS/edge"
      if [ "${CONTROL_PLANE_EDGE_TOKEN:-}" = "$expected" ]; then printf ' edge=file'; else printf ' edge=other'; fi
      printf '\n'
    } >> "$RO_TEST_OUT/npm"
    hex=$(printf '%064d' 0); hex=${hex//0/e}
    printf 'npm-stdout %s %s %s\n' "${CONTROL_PLANE_EDGE_TOKEN:-missing}" "${CLOUDFLARE_API_TOKEN:-missing}" "$hex"
    printf 'npm-stderr %s %s %s\n' "${CONTROL_PLANE_EDGE_TOKEN:-missing}" "${CLOUDFLARE_API_TOKEN:-missing}" "$hex" >&2
    [ "$scenario" != npm-fail ] || exit 1 ;;
  curl)
    for arg in "$@"; do
      case "$arg" in @*)
        auth_file=${arg#@}
        mode=$(stat -f '%Lp' "$auth_file" 2>/dev/null) || mode=$(stat -c '%a' "$auth_file")
        parent_mode=$(stat -f '%Lp' "${auth_file%/*}" 2>/dev/null) || parent_mode=$(stat -c '%a' "${auth_file%/*}")
        [ "$mode:$parent_mode" = 600:700 ] || exit 99
        IFS= read -r header < "$auth_file"
        IFS= read -r expected < "$RO_TEST_SECRETS/fleet"
        [ "$header" = "Authorization: Bearer $expected" ] || exit 99
        unset header expected
        printf 'private header verified\n' >> "$RO_TEST_OUT/auth-verified" ;;
      esac
    done
    url=${!#}
    case "$url" in
      */v1/version)
        count versions
        version=1.2.4
        if [ "$n" = 1 ] || [ "$scenario" = stale-version ]; then version=1.2.3; fi
        [ "$scenario" != downgrade ] || version=1.2.5
        [ "$scenario" != equal-version ] || version=1.2.4
        [ "$scenario" != bad-version ] || version=unknown
        printf '{"version":"%s","commit":"12345678abcdef"}\n' "$version" ;;
      *'/v1/backups/status?'*)
        count records
        state=idle; id=drill-fixture; finished='"2026-10-10T08:02:00Z"'
        if [ "$n" -gt 1 ]; then state=validated; fi
        if [ "$scenario" = normal ] && [ "$n" = 2 ]; then state=running; fi
        if [ "$scenario" = already-running ] || { [ "$scenario" = timeout ] && [ "$n" -gt 1 ]; }; then state=running; fi
        if [ "$scenario" = mismatched ] && [ "$n" -gt 1 ]; then id=drill-other; fi
        if [ "$scenario" = deadline ] && [ "$n" = 2 ]; then state=failed; finished=null; fi
        if [ "$scenario" = precedence ] && [ "$n" -gt 1 ]; then state=failed; fi
        [ "$scenario" != no-record ] || { printf '{"account":{"backups":{"catalog":[]}}}\n'; exit 0; }
        [ "$scenario" != status-fail ] || exit 22
        printf '{"account":{"restore_drill":{"drill_id":"%s","driver":"alarm","state":"%s","started_at":"2026-10-10T08:00:00Z","finished_at":%s},"backups":{"catalog":[' "$id" "$state" "$finished"
        if [ "$scenario" != no-backup ]; then
          printf '{"backup_id":"backup_20261010T070000Z","status":"committed","size":12,"chunks":1,"entries":1,"archive_schema_version":2,"committed_at":"2026-10-10T07:00:00Z","validations":['
          if [ "$scenario" = precedence ]; then printf '{"target_cell":"civo-sandbox-use1-backup","validated_at":"2026-10-10T08:00:00Z"}'; fi
          printf ']},{"backup_id":"backup_20261010T080000Z","status":"failed"}'
        fi
        printf ']}}}\n' ;;
      */v1/backups:restore-drill)
        count posts
        hdr=; out=; args=("$@")
        for ((i=0; i < ${#args[@]}; i++)); do
          case "${args[$i]}" in -D) hdr=${args[$((i+1))]} ;; -o) out=${args[$((i+1))]} ;; esac
        done
        [ -n "$hdr" ] && [ -n "$out" ] || exit 95
        printf 'HTTP/2 202\r\n' > "$hdr"
        if [ "$scenario" != no-header ] && [ "$scenario" != body-id ] && [ "$scenario" != connected ]; then
          printf 'X-Witself-Restore-Drill-ID: drill-fixture\r\nX-Witself-Restore-Drill-Driver: alarm\r\n' >> "$hdr"
        fi
        code=202
        printf '{"restore_drill":{"drill_id":"drill-fixture"}}\n' > "$out"
        case "$scenario" in
          protocol) code=409; printf '{"error":"requires backup validation protocol 2"}\n' > "$out" ;;
          gateway|no-header) code=502 ;;
          connected) code=200; printf '{"validated":true}\n' > "$out" ;;
          bad-http) code=500 ;;
        esac
        printf '%s' "$code" ;;
      */metrics/probes) printf 'witself_control_plane_account_backup_fixture 1\n' ;;
      *) exit 96 ;;
    esac ;;
  kubectl)
    if [[ " $* " == *' get pod '* ]]; then
      count servers
      if [ "$scenario" = restart ] && [ "$n" -gt 1 ]; then printf 'server:1:OOMKilled '
      else printf 'server:0: '; fi
    elif [[ " $* " == *' exec '* ]]; then
      printf '%s\0' "$@" > "$RO_TEST_OUT/kube-argv"
      [ "$scenario" != kube-fail ] || exit 1
      if [ "$scenario" = rows ]; then printf '1\n'; else printf '0\n'; fi
    else exit 97; fi ;;
  sed)
    if [ "$scenario" = redaction-fail ]; then
      count redactions
      [ "$n" -ne 2 ] || exit 19
    fi
    exec "$RO_TEST_ROOT/real/$name" "$@" ;;
  gh|node|witself|witself-admin) exit 98 ;;
  *) exec "$RO_TEST_ROOT/real/$name" "$@" ;;
esac
STUB
chmod 755 "$BIN/dispatch"
mkdir "$TEST_ROOT/real"
for name in sed awk env cat jq grep tr printf head tail cut xargs dirname stat wc mktemp chmod rm; do
  real=$(PATH=$ORIGINAL_PATH type -P "$name")
  ln -s "$real" "$TEST_ROOT/real/$name"
  ln -s dispatch "$BIN/$name"
done
for name in git date sleep gh curl npm node kubectl witself witself-admin; do ln -s dispatch "$BIN/$name"; done
export PATH=$BIN:$ORIGINAL_PATH
DEPLOY=$REPO/scripts/release-deploy-control-plane.sh
DRILL=$REPO/scripts/release-drill-proof.sh
deploy_args=(1.2.4 --edge-token-file "$SECRET_DIR/edge" --cloudflare-token-file "$SECRET_DIR/cf" --deploy-env-file "$SECRET_DIR/deploy-env")
drill_args=(--account-id fixture_account --fleet-token-file "$SECRET_DIR/fleet" --max-wait-min 3)
reset_case() {
  rm -rf "$OUT"
  mkdir "$OUT"
  printf '28800\n' > "$OUT/clock"
  : > "$OUT/calls"
  export RO_TEST_SCENARIO=${1:-normal}
  RO_TEST_WITH_CF=false
  RO_TEST_INHERITED=false
  RO_TEST_CF_VALUE=$CF_ENV
  RO_TEST_CWD=''
}
run() {
  set +e
  (
    if [ -n "$RO_TEST_CWD" ]; then cd "$RO_TEST_CWD"; fi
    if [ "$RO_TEST_INHERITED" = true ]; then
      export CONTROL_PLANE_EDGE_TOKEN=$INHERITED_EDGE WITSELF_FLEET_TOKEN=$INHERITED_FLEET
    fi
    if [ "$RO_TEST_WITH_CF" = true ] || [ "$RO_TEST_INHERITED" = true ]; then
      export CP_DEPLOY_CANONICAL_EMAIL_ACTIVE=true CP_DEPLOY_SIGNUP_OPEN_ACTIVE=true
      export CLOUDFLARE_API_TOKEN=$RO_TEST_CF_VALUE CLOUDFLARE_API_TOKEN_OLD=$CF_OLD
    fi
    RO_TEST_RECORD=true "$BASH" "$@"
  ) > "$OUT/stdout" 2> "$OUT/stderr"
  RC=$?
  set -e
}
expect_rc() { [ "$RC" = "$1" ] || fail "$CASE exit $RC, expected $1"; }
contains() { grep -Fq -- "$2" "$1" || fail "$CASE missing expected text: $2"; }
absent() { if grep -Fq -- "$2" "$1"; then fail "$CASE unexpected text: $2"; fi; }
last_line() { [ "$(tail -n 1 "$OUT/stdout")" = "$1" ] || fail "$CASE final verdict mismatch"; }
no_action() {
  if grep -E '^(gh|curl|npm|node|kubectl|witself|witself-admin) |^git .*<(fetch|push|tag|worktree)>' "$OUT/calls" >/dev/null; then fail "$CASE acted during validation"; fi
}
secret_audit() {
  local file line key
  # Fixture inputs are the only persistent files allowed to retain fake secrets.
  while IFS= read -r file; do
    case "$file" in "$SECRET_DIR"/*) continue ;; esac
    # Read each file once; use builtin matching so the audit puts no secret on argv.
    while IFS= read -r line || [ -n "$line" ]; do
      for key in "$EDGE" "$CF_FILE" "$CF_ENV" "$FLEET" "$INHERITED_EDGE" "$INHERITED_FLEET" "$CF_OLD" "$HEX"; do
        [[ "$line" != *"$key"* ]] || fail "$CASE raw fixture escaped into a file"
      done
    done < "$file"
  done < <(find "$TEST_ROOT" -type f)
  while IFS= read -r line; do
    [[ "$line" != *'RO_REDACT_EDGE=set'* && "$line" != *'RO_REDACT_CF=set'* && "$line" != *'CLOUDFLARE_API_TOKEN_OLD=set'* && "$line" != *'WITSELF_FLEET_TOKEN=set'* ]] || fail "$CASE exported forbidden secret"
    case "$line" in 'npm <run> <deploy>'*) ;;
      *) [[ "$line" != *'CONTROL_PLANE_EDGE_TOKEN=set'* && "$line" != *'CLOUDFLARE_API_TOKEN=set'* ]] || fail "$CASE token inherited by non-deploy child (${line%% *}; ${line##*>})" ;;
    esac
  done < "$OUT/calls"
}
deploy_target() {
  local top cwd
  IFS= read -r top < "$OUT/npm-toplevel"
  IFS= read -r cwd < "$OUT/npm-pwd"
  case "$top" in "$1"/witself-release.*) ;; *) fail "$CASE deploy outside temporary tag worktree" ;; esac
  [ "$top" != "$REPO" ] && [ "$cwd" = "$top/infra/cloudflare/control-plane" ] || fail "$CASE wrong deploy directory"
  [ "$(cat "$OUT/npm-head")" = "$TAG_COMMIT" ] || fail "$CASE deploy HEAD differs from refs/tags/v1.2.4"
}

CASE=T2
reset_case
# Check the actual source wiring, not a fixture copy.
grep -Fxq $'\tbash scripts/test-release-ops.sh' "$SOURCE_ROOT/Makefile" || fail 'T2 Makefile wiring'
grep -Fxq '        run: bash scripts/test-release-ops.sh' "$SOURCE_ROOT/.github/workflows/ci.yml" || fail 'T2 CI wiring'
for script in "$REPO"/scripts/release-*.sh; do
  run "$script" --help; expect_rc 0; contains "$OUT/stdout" 'usage: scripts/'
  (cd "$REPO/scripts"; run "${script##*/}" --help; expect_rc 0)
done
run "$DRILL" --help
grep -Fxq '  --execute               Start the drill; without it, print the plan only' "$OUT/stdout" || fail 'T2 drill help spacing'
pass T2
CASE=T1
for script in "$REPO"/scripts/release-*.sh; do
  reset_case
  name=${script##*/}; name=${name%.sh}
  find "$TEST_ROOT" -not -path "$OUT" -not -path "$OUT/*" | LC_ALL=C sort > "$OUT/before"
  case "$name" in
    release-deploy-control-plane) run "$script" "${deploy_args[@]}" ;;
    release-drill-proof) run "$script" "${drill_args[@]}" ;;
    *) fail "T1 needs a valid-argument row for $name" ;;
  esac
  expect_rc 0
  last_line "$name: DRY-RUN (no action taken; add --execute)"
  no_action
  find "$TEST_ROOT" -not -path "$OUT" -not -path "$OUT/*" | LC_ALL=C sort > "$OUT/after"
  cmp -s "$OUT/before" "$OUT/after" || fail 'T1 dry run created files'
done
pass T1

CASE=T7
reset_case
run "$DEPLOY" "${deploy_args[@]}" --attest-canonical-email-active --attest-signup-open-active --execute
expect_rc 0
contains "$OUT/stdout" 'release-deploy-control-plane: PASS control plane at 1.2.4 commit=12345678'
contains "$OUT/npm" '/infra/cloudflare/control-plane umask=0022'
contains "$OUT/npm" 'canonical=true signup=true cf=file'
deploy_target "$TEST_ROOT/tmp"
contains "$OUT/calls" '<ci> <--silent>'
while IFS= read -r line; do
  case "$line" in 'npm <ci> <--silent>'*)
    [[ "$line" != *'=set'* ]] || fail 'T7 npm ci inherited a token' ;;
  esac
done < "$OUT/calls"
contains "$OUT/calls" '<worktree> <remove> <--force>'
while IFS= read -r line; do
  case "$line" in curl*) [[ "$line" == *'<https://self.witwave.ai/v1/version>'* ]] || fail 'T7 alternate deploy origin' ;; esac
done < "$OUT/calls"
CASE=T8
contains "$OUT/stdout" 'npm-stdout <edge-token> <cloudflare-token> <hex64>'
contains "$OUT/stdout" 'npm-stderr <edge-token> <cloudflare-token> <hex64>'
secret_audit
pass 'T8 deploy'

CASE=T7
for flag in --attest-canonical-email-active --attest-signup-open-active; do
  reset_case
  run "$DEPLOY" "${deploy_args[@]}" "$flag" --execute
  expect_rc 0
  case "$flag" in
    --attest-canonical-email-active) contains "$OUT/npm" 'canonical=true signup=unset' ;;
    --attest-signup-open-active) contains "$OUT/npm" 'canonical=unset signup=true' ;;
  esac
  deploy_target "$TEST_ROOT/tmp"
  secret_audit
done
reset_case
mkdir "$TEST_ROOT/workdir"
run "$DEPLOY" "${deploy_args[@]}" --workdir "$TEST_ROOT/workdir" --execute
expect_rc 0; deploy_target "$TEST_ROOT/workdir"; secret_audit
pass T7

CASE='T8 inherited credentials'
for script in "$DEPLOY" "$DRILL"; do
  reset_case
  RO_TEST_INHERITED=true
  if [ "$script" = "$DEPLOY" ]; then
    run "$script" "${deploy_args[@]}" --execute
    expect_rc 0
    contains "$OUT/npm" 'cf=file edge=file'
    contains "$OUT/npm" 'canonical=unset signup=unset'
  else
    run "$script" "${drill_args[@]}" --execute
    expect_rc 0
  fi
  secret_audit
done
pass "$CASE"

CASE='T8 xtrace'
for script in "$DEPLOY" "$DRILL"; do
  if [ "$script" = "$DEPLOY" ]; then args=("${deploy_args[@]}"); else args=("${drill_args[@]}"); fi
  for trace in flag shellopts sourced; do
    reset_case
    case "$trace" in
      flag) run -x "$script" "${args[@]}" --execute ;;
      shellopts) run -c 'exec env SHELLOPTS=xtrace "$BASH" "$@"' trace "$script" "${args[@]}" --execute ;;
      sourced) run -c 'set -x; source "$@"' trace "$script" "${args[@]}" --execute ;;
    esac
    expect_rc 2
    name=${script##*/}; name=${name%.sh}
    last_line "$name: FAIL xtrace must be disabled"
    contains "$OUT/stderr" "$name: FAIL xtrace must be disabled"
    no_action
    # Only the explicit env launcher is allowed; no token reader or root helper.
    if grep -v '^env ' "$OUT/calls" | grep -q .; then fail "$CASE started a child before refusing tracing"; fi
    secret_audit
  done
done
(
  source "$REPO/scripts/lib/release-ops.sh"
  [[ $- != *x* ]] || fail 'T8 library enabled xtrace'
)
pass "$CASE"

CASE=T9
for token_flag in --edge-token-file --cloudflare-token-file --fleet-token-file; do
  case "$token_flag" in
    --edge-token-file) token=$EDGE; minimum=16; maximum=8192; script=$DEPLOY; args=("${deploy_args[@]}") ;;
    --cloudflare-token-file) token=$CF_FILE; minimum=1; maximum=4096; script=$DEPLOY; args=("${deploy_args[@]}") ;;
    --fleet-token-file) token=$FLEET; minimum=16; maximum=8192; script=$DRILL; args=("${drill_args[@]}") ;;
  esac
  for kind in mode symlink relative in-repo empty undersized oversized cr multiline nul parent canonical linked-worktree; do
  reset_case
  CASE="T9 $token_flag $kind"
  bad=$SECRET_DIR/bad
  rm -f "$bad"
  printf '%s\n' "$token" > "$bad"; chmod 600 "$bad"
  case "$kind" in
    mode) chmod 644 "$bad" ;;
    symlink)
      printf '%s\n' "$token" > "$SECRET_DIR/link-target"; rm "$bad"; ln -s "$SECRET_DIR/link-target" "$bad"
      # On systems with link modes, isolate the symlink guard from the 0600 guard.
      chmod -h 600 "$bad" 2>/dev/null || : ;;
    relative) RO_TEST_CWD=$SECRET_DIR; bad=bad ;;
    in-repo) bad=$REPO/private-token; printf '%s\n' "$token" > "$bad" ;;
    empty) : > "$bad" ;;
    undersized) if [ "$minimum" -gt 1 ]; then printf '%*s' "$((minimum - 1))" '' | tr ' ' x > "$bad"; else : > "$bad"; fi ;;
    oversized) printf '%*s' "$((maximum + 1))" '' | tr ' ' x > "$bad" ;;
    cr) printf '%s\r\n' "$token" > "$bad" ;;
    multiline) printf '%s\nx\n' "$token" > "$bad" ;;
    nul) printf '%s\0\n' "$token" > "$bad" ;;
    parent) mkdir -p "$SECRET_DIR/public-parent"; chmod 755 "$SECRET_DIR/public-parent"; bad=$SECRET_DIR/public-parent/bad; printf '%s\n' "$token" > "$bad" ;;
    canonical) bad=$SECRET_DIR/../private/edge ;;
    linked-worktree)
      "$REAL_GIT" -C "$REPO" worktree add -q --detach "$TEST_ROOT/linked" HEAD
      bad=$TEST_ROOT/linked/private-token; printf '%s\n' "$token" > "$bad" ;;
  esac
  run "$script" "${args[@]}" "$token_flag" "$bad" --execute
  [ "$RC" = 1 ] || [ "$RC" = 2 ] || fail "$CASE accepted invalid token file"
  no_action
  chmod 700 "$SECRET_DIR"
  rm -f "$REPO/private-token"
  if [ "$kind" = linked-worktree ]; then "$REAL_GIT" -C "$REPO" worktree remove --force "$TEST_ROOT/linked"; fi
  secret_audit
  done
  # Check the exact allowed bounds, with and without the optional final LF.
  for size in "$minimum" "$maximum"; do
    for newline in false true; do
      reset_case
      CASE="T9 $token_flag boundary $size newline=$newline"
      rm -f "$SECRET_DIR/bad"
      printf '%*s' "$size" '' | tr ' ' x > "$SECRET_DIR/bad"
      if [ "$newline" = true ]; then printf '\n' >> "$SECRET_DIR/bad"; fi
      run "$script" "${args[@]}" "$token_flag" "$SECRET_DIR/bad"
      expect_rc 0; no_action; secret_audit
    done
  done
done
for kind in unknown duplicate missing injection mode symlink parent in-repo canonical; do
  reset_case
  CASE="T9 deploy-env $kind"
  rm -f "$SECRET_DIR/bad-env"
  cp "$SECRET_DIR/deploy-env" "$SECRET_DIR/bad-env"
  bad=$SECRET_DIR/bad-env
  case "$kind" in
    unknown) printf 'OTHER=anything\n' >> "$SECRET_DIR/bad-env" ;;
    duplicate) printf 'EMAIL_DIRECTORY_KV_ID=duplicate\n' >> "$SECRET_DIR/bad-env" ;;
    missing) printf 'CLOUDFLARE_ACCOUNT_ID=one\n' > "$SECRET_DIR/bad-env" ;;
    injection) printf 'CLOUDFLARE_ACCOUNT_ID=$(touch %s/probe)\n' "$TEST_ROOT" > "$SECRET_DIR/bad-env" ;;
    mode) chmod 644 "$bad" ;;
    symlink)
      rm "$bad"; ln -s "$SECRET_DIR/deploy-env" "$bad"
      chmod -h 600 "$bad" 2>/dev/null || : ;;
    parent) mkdir -p "$SECRET_DIR/public-parent"; chmod 755 "$SECRET_DIR/public-parent"; bad=$SECRET_DIR/public-parent/bad-env; cp "$SECRET_DIR/deploy-env" "$bad" ;;
    in-repo) bad=$REPO/private-env; cp "$SECRET_DIR/deploy-env" "$bad" ;;
    canonical) bad=$SECRET_DIR/../private/deploy-env ;;
  esac
  run "$DEPLOY" "${deploy_args[@]}" --deploy-env-file "$bad" --execute
  [ "$RC" = 1 ] || [ "$RC" = 2 ] || fail "$CASE accepted invalid deploy env file"
  no_action
  chmod 700 "$SECRET_DIR"
  rm -f "$REPO/private-env"
  [ ! -e "$TEST_ROOT/probe" ] || fail 'T9 env file executed'
  secret_audit
done
CASE=T9
reset_case
run "$DEPLOY" 1.2.4 --edge-token-file "$SECRET_DIR/edge" --deploy-env-file "$SECRET_DIR/deploy-env" --execute
expect_rc 2; no_action
reset_case
RO_TEST_WITH_CF=true
run "$DEPLOY" "${deploy_args[@]}" --execute
expect_rc 0
contains "$OUT/stdout" 'CLOUDFLARE_API_TOKEN from the environment is ignored; using --cloudflare-token-file'
contains "$OUT/npm" 'cf=file'
contains "$OUT/npm" 'canonical=unset signup=unset'
secret_audit
reset_case
RO_TEST_WITH_CF=true
run "$DEPLOY" 1.2.4 --edge-token-file "$SECRET_DIR/edge" --deploy-env-file "$SECRET_DIR/deploy-env" --execute
expect_rc 0; contains "$OUT/npm" 'cf=env'; secret_audit
for kind in space tab control oversized; do
  reset_case
  CASE="T9 Cloudflare environment $kind"
  RO_TEST_WITH_CF=true
  case "$kind" in
    space) RO_TEST_CF_VALUE="$CF_ENV space" ;;
    tab) RO_TEST_CF_VALUE=$CF_ENV$'\t' ;;
    control) RO_TEST_CF_VALUE=$CF_ENV$'\001' ;;
    oversized) RO_TEST_CF_VALUE=$(printf '%4097s' '' | tr ' ' x) ;;
  esac
  run "$DEPLOY" 1.2.4 --edge-token-file "$SECRET_DIR/edge" --deploy-env-file "$SECRET_DIR/deploy-env" --execute
  expect_rc 2; no_action; secret_audit
done
for size in 1 4096; do
  reset_case
  CASE="T9 Cloudflare environment boundary $size"
  RO_TEST_WITH_CF=true
  RO_TEST_CF_VALUE=$(printf '%*s' "$size" '' | tr ' ' x)
  run "$DEPLOY" 1.2.4 --edge-token-file "$SECRET_DIR/edge" --deploy-env-file "$SECRET_DIR/deploy-env"
  expect_rc 0; no_action; secret_audit
done
pass T9

CASE=T10
for scenario in npm-fail stale-version downgrade equal-version bad-version redaction-fail worktree-add-fail dirty-worktree zero-tags two-tags; do
  reset_case "$scenario"
  CASE="T10 $scenario"
  if [ "$scenario" = two-tags ]; then "$REAL_GIT" -C "$REPO" tag v1.2.4-duplicate refs/tags/v1.2.4; fi
  run "$DEPLOY" "${deploy_args[@]}" --execute
  if [ "$scenario" = two-tags ]; then "$REAL_GIT" -C "$REPO" tag -d v1.2.4-duplicate >/dev/null; fi
  secret_audit
  if [ "$scenario" = equal-version ]; then expect_rc 0; contains "$OUT/calls" 'npm <run> <deploy>'; continue; fi
  expect_rc 1
  case "$scenario" in
    npm-fail)
      last_line 'release-deploy-control-plane: FAIL npm run deploy exited 1'
      contains "$OUT/calls" '<worktree> <remove> <--force>'
      cat "$OUT/stdout" "$OUT/stderr" > "$OUT/deploy.log"
      contains "$OUT/deploy.log" 'npm-stdout <edge-token> <cloudflare-token> <hex64>'
      contains "$OUT/deploy.log" 'npm-stderr <edge-token> <cloudflare-token> <hex64>'
      secret_audit ;;
    stale-version)
      contains "$OUT/stdout" 'release-deploy-control-plane: FAIL control plane reports 1.2.3'
      [ "$(cat "$OUT/versions")" = 11 ] || fail 'T10 not exactly ten post-deploy polls' ;;
    downgrade)
      contains "$OUT/stdout" 'refusing to deploy 1.2.4 below the live 1.2.5'
      absent "$OUT/calls" '<worktree> <add>'; absent "$OUT/calls" 'npm ' ;;
    redaction-fail)
      contains "$OUT/stdout" 'redaction pipeline exited'
      contains "$OUT/calls" '<worktree> <remove> <--force>' ;;
    bad-version)
      contains "$OUT/stdout" 'cannot read the live control-plane version'
      absent "$OUT/calls" '<worktree> <add>' ;;
    worktree-add-fail)
      last_line 'release-deploy-control-plane: FAIL cannot create tag worktree'
      absent "$OUT/calls" '<worktree> <remove>'
      absent "$OUT/calls" 'npm ' ;;
    dirty-worktree)
      last_line 'release-deploy-control-plane: FAIL tag worktree is not clean'
      absent "$OUT/calls" 'npm ' ;;
    zero-tags|two-tags)
      last_line 'release-deploy-control-plane: FAIL tag worktree must have exactly one release tag'
      absent "$OUT/calls" 'npm ' ;;
  esac
done
pass T10

CASE=T11
reset_case
run "$DRILL" "${drill_args[@]}" --execute
expect_rc 0
contains "$OUT/stdout" 'release-drill-proof: PASS drill drill-fixture validated'
contains "$OUT/stdout" 'drill DB rows for the account (must be 0): 0'
contains "$OUT/calls" '<-H> <@'
contains "$OUT/auth-verified" 'private header verified'
absent "$OUT/calls" 'Bearer'
# Keep this independently specified literal byte-for-byte with A6 step 9.
PQ='pf="${POSTGRES_PASSWORD_FILE:-${POSTGRESQL_PASSWORD_FILE:-/opt/bitnami/postgresql/secrets/password}}"; PGPASSWORD="$(cat "$pf")" psql --no-password --host=127.0.0.1 --username="${POSTGRES_USER:-${POSTGRESQL_USERNAME:-witself}}" --dbname="${POSTGRES_DB:-${POSTGRESQL_DATABASE:-witself}}" -tAc "select count(*) from accounts where id = '\''$1'\''"'
args=()
while IFS= read -r -d '' arg; do args+=("$arg"); done < "$OUT/kube-argv"
n=${#args[@]}
[ "${args[$((n-6))]}" = -- ] && [ "${args[$((n-5))]}" = sh ] && [ "${args[$((n-4))]}" = -c ] && [ "${args[$((n-3))]}" = "$PQ" ] && [ "${args[$((n-2))]}" = sh ] && [ "${args[$((n-1))]}" = fixture_account ] || fail 'T11 pod shell argv differs from the literal contract'
secret_audit
reset_case
run "$DRILL" "${drill_args[@]}" --account-id "x'y" --execute
expect_rc 2; no_action
pass T11
pass 'T8 drill'
CASE=T12
for scenario in already-running protocol gateway no-header mismatched deadline precedence rows kube-fail restart connected body-id no-record status-fail no-backup timeout bad-http; do
  reset_case "$scenario"
  CASE="T12 $scenario"
  run "$DRILL" "${drill_args[@]}" --execute
  secret_audit
  case "$scenario" in
    gateway|deadline|precedence|restart|connected|body-id)
      expect_rc 0; contains "$OUT/stdout" 'release-drill-proof: PASS'
      case "$scenario" in
        gateway) contains "$OUT/calls" 'sleep <60>' ;;
        deadline) contains "$OUT/calls" 'sleep <120>'; [ "$(cat "$OUT/records")" = 3 ] || fail 'T12 deadline extra read count' ;;
        restart) contains "$OUT/stdout" 'WARN server restarted' ;;
      esac ;;
    *)
      expect_rc 1; contains "$OUT/stdout" 'release-drill-proof: FAIL'
      case "$scenario" in
        already-running) [ ! -e "$OUT/posts" ] || fail 'T12 reposted running drill' ;;
        protocol) [ "$(cat "$OUT/posts")" = 2 ] || fail 'T12 protocol retry count'; contains "$OUT/stdout" 'drill cell refused protocol 2 twice' ;;
        no-header) contains "$OUT/stdout" 'drill not recorded' ;;
        timeout) [ "$(cat "$OUT/posts")" = 1 ] || fail 'T12 timeout post count'; [ "$(grep -c '^sleep <60>' "$OUT/calls")" = 3 ] || fail 'T12 polling bound'; [ "$(cat "$OUT/records")" = 6 ] || fail 'T12 bounded record reads' ;;
      esac ;;
  esac
done
pass T12

CASE=T15
reset_case
# shellcheck source=scripts/lib/release-ops.sh
source "$REPO/scripts/lib/release-ops.sh"
for row in '0800 1' '0959 1' '2329 1' '2330 0' '0000 0' '0044 0' '0045 1'; do
  hhmm=${row% *}; expected=${row#* }
  result=0; ro_in_window "$hhmm" 2330 0045 2> "$OUT/window-errors" || result=$?
  [ ! -s "$OUT/window-errors" ] || fail "T15 window $hhmm emitted an error"
  [ "$result" = "$expected" ] || fail "T15 window $hhmm"
done
[ "$(ro_hhmm_minutes 0830)" = 510 ] || fail 'T15 decimal clock'
if ro_hhmm_minutes 2400 >/dev/null 2>&1; then fail 'T15 invalid hour'; fi
pass T15
CASE=T17
(
# shellcheck source=scripts/lib/release-ops.sh
source "$REPO/scripts/lib/release-ops.sh"
# Source the reference only: its existing main guard prevents execution.
# shellcheck disable=SC1091
# shellcheck source=scripts/roll-train.sh
source "$SOURCE_ROOT/scripts/roll-train.sh"
for row in '0.0.9 0.0.10' '1.0.0 0.9.99' '1.2.3 1.2.3' '0.0.0 0.0.1' '2.0.0 10.0.0' '10.0.0 2.0.0' '1.9.0 1.10.0' '1.2.99 1.3.0' '99999999999999999999.0.0 99999999999999999998.0.0' '01.2.3 1.2.3' '1.2 1.2.3' 'v1.2.3 1.2.3'; do
  left=${row% *}; right=${row#* }
  a=0; b=0; version_lower "$left" "$right" || a=$?; ro_version_lower "$left" "$right" || b=$?
  [ "$a" = "$b" ] || fail "T17 version ordering $row"
  for value in "$left" "$right"; do
    a=0; b=0; valid_version "$value" || a=$?; ro_valid_version "$value" || b=$?
    [ "$a" = "$b" ] || fail "T17 valid version $value"
  done
done
pass T17
)
for script in "$SOURCE_ROOT/scripts/lib/release-ops.sh" "$SOURCE_ROOT"/scripts/release-*.sh; do
  if grep -nE 'declare -A|mapfile|readarray|,,\}|\^\^\}|wait -n|&>>|coproc' "$script" > "$OUT/static"; then fail 'T16 Bash compatibility'; fi
  if grep -nE '/Users/|\$HOME|~/\.witself|acc_[a-z0-9]|sec_[a-z0-9]|k8s\.civo\.com|[0-9a-f]{32}|\.gitops/cells' "$script" > "$OUT/static"; then fail 'T19 operator values'; fi
  if tail -n +2 "$script" | grep -nE '(^|[^A-Za-z0-9_])/(usr/)?(local/)?s?bin/' > "$OUT/static"; then fail 'T20 absolute binaries'; fi
done
# The future runbook section is optional until 125B, and is read in isolation.
sed -n '/^## Ship a fix and close signups without an AI agent$/,/^## K3s minor upgrade (Civo)$/p' "$SOURCE_ROOT/docs/runbooks.md" > "$OUT/runbook-section"
if grep -nE '/Users/|\$HOME|~/\.witself|acc_[a-z0-9]|sec_[a-z0-9]|k8s\.civo\.com|[0-9a-f]{32}|\.gitops/cells' "$OUT/runbook-section" >/dev/null; then fail 'T19 runbook operator values'; fi
pass 'T16 T19 T20'
printf 'release ops test: PASS all 125A1 contracts\n'
