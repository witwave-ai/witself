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

# Read only the two bounded public additions (B1 and B2). Run this before fixture
# setup so a documentation mutation fails without unrelated release scenarios.
check_runbook_contract() {
  local runbook section prose paragraph block recovery line previous command script flag
  local heading_count next_heading brew_line plans_line chain_line block_end
  local timing_line hold_line close_line
  runbook=$SOURCE_ROOT/docs/runbooks.md
  section=$OUT/runbook-section
  prose=$OUT/runbook-prose
  paragraph=$OUT/plans-paragraph
  block=$OUT/plans-block
  recovery=$OUT/recovery-block
  heading_count=$(grep -c '^## Ship a fix and close signups without an AI agent$' "$runbook" || true)
  [ "$heading_count" = 1 ] || fail 'T18 runbook heading must occur exactly once'
  next_heading=$(awk '
    /^## Ship a fix and close signups without an AI agent$/ { active=1; next }
    active && /^## / { print; exit }
  ' "$runbook")
  [ "$next_heading" = '## K3s minor upgrade (Civo)' ] || fail 'T18 K3s must immediately follow B1'
  awk '
    /^## Ship a fix and close signups without an AI agent$/ { active=1; print; next }
    active && /^## / { exit }
    active { print }
  ' "$runbook" > "$section"
  [ "$(awk 'NR > 1 && NF { print; exit }' "$section")" = 'Status: DRAFT; not rehearsed.' ] \
    || fail 'T18 draft status line'
  awk '{ for (i=1; i<=NF; i++) { if (seen++) printf " "; printf "%s", $i } } END { print "" }' "$section" > "$prose"
  awk 'BEGIN { RS="" } { gsub(/[[:space:]]+/, " "); print }' "$section" > "$OUT/runbook-paragraphs"

  cat > "$OUT/plans-expected" <<'PLANS'
PLANS_WT=$(mktemp -d)/plans
git fetch origin --tags && git worktree add --detach "$PLANS_WT" "v$VERSION"
git -C "$PLANS_WT" describe --tags --exact-match   # must print v$VERSION
test -z "$(git -C "$PLANS_WT" status --porcelain)"
(cd "$PLANS_WT/infra/cloudflare/control-plane" && umask 022 && set -a && . "$DEPLOY_ENV_FILE" && set +a \
  && if [ -n "${CLOUDFLARE_TOKEN_FILE:-}" ]; then CLOUDFLARE_API_TOKEN=$(cat "$CLOUDFLARE_TOKEN_FILE"); fi \
  && export CLOUDFLARE_API_TOKEN && npm ci && npm run deploy:plans)
git worktree remove --force "$PLANS_WT"
PLANS
  # These independent commands are not release-script options. Keep exact
  # spellings so a broad command-name exception cannot conceal an added flag.
  cat > "$OUT/non-release-commands" <<'COMMANDS'
witself secret reveal "$EDGE_TOKEN_SECRET" token --json | jq -j .value > "$SECRETS_DIR/edge.token"
SERVING_URL=$(witself-admin cells show civo-sandbox-use1-serving --token-file "$FLEET_TOKEN_FILE" --json | jq -r .cell.endpoint)
git switch main && git pull --ff-only
scripts/roll-train.sh "$VERSION" --no-schema-change --serving-url "$SERVING_URL"
witself-admin cells list --token-file "$ADMIN_TOKEN_FILE" --json | jq -r '.cells[] | select(.accepting == true) | .name'
witself-admin cells drain "$CELL" --token-file "$FLEET_TOKEN_FILE" --json | jq -e '.cell.accepting == false'
sleep 60; witself-admin cells list --token-file "$ADMIN_TOKEN_FILE" --json | jq -e '[.cells[] | select(.accepting == true)] | length == 0'
COMMANDS
  # Check physical lines so a missing continuation cannot hide an orphan flag.
  # Known non-release commands have their own flags; only release scripts are
  # invoked here, with --help as their sole input. Reset ownership at every fence.
  awk '
    FILENAME != ARGV[3] { known[$0]=1; next }
    /^[[:space:]]*```/ { fenced=!fenced; script=""; next }
    !fenced || /^[[:space:]]*#/ { next }
    {
      if (match($0, /scripts\/release-[a-z-]+\.sh([[:space:]]|$)/)) {
        script=substr($0, RSTART, RLENGTH); sub(/[[:space:]]$/, "", script)
      }
      line=$0; sub(/^[[:space:]]+/, "", line); sub(/[[:space:]]+$/, "", line)
      if (line in known) next
      if ($0 ~ /^[[:space:]]*--/ || $0 ~ /--[A-Za-z0-9][A-Za-z0-9-]*/) {
        if (script == "") exit 1
        print script "\t" $0
      }
    }
  ' "$OUT/plans-expected" "$OUT/non-release-commands" "$section" > "$OUT/runbook-commands" || fail 'T18 flag has no preceding release script in its block'
  [ -s "$OUT/runbook-commands" ] || fail 'T18 no release commands found'
  while IFS=$'\t' read -r script command; do
    [ -f "$SOURCE_ROOT/$script" ] || fail 'T18 documented release script is missing'
    "$BASH" "$SOURCE_ROOT/$script" --help > "$OUT/runbook-help" \
      || fail "T18 $script --help failed"
    # Only the first option token is advertised. Descriptions can mention flags
    # accepted by a child (for example, the drill-account-id description).
    awk '/^[[:space:]]+--[A-Za-z0-9][A-Za-z0-9-]*/ { print $1 }' "$OUT/runbook-help" > "$OUT/runbook-help-flags"
    [ -s "$OUT/runbook-help-flags" ] || fail "T18 $script advertises no flags"
    printf '%s\n' "$command" | grep -oE -- '--[A-Za-z0-9][A-Za-z0-9-]*' > "$OUT/runbook-command-flags" || true
    while IFS= read -r flag; do
      grep -Fxq -- "$flag" "$OUT/runbook-help-flags" || fail "T18 $script does not advertise $flag"
    done < "$OUT/runbook-command-flags"
  done < "$OUT/runbook-commands"
  awk '
    /^[[:space:]]*```/ { fenced=!fenced; next }
    fenced && !/^[[:space:]]*#/ { print }
  ' "$section" > "$OUT/runbook-fenced-commands"
  if grep -Eq -- '(^|[^A-Za-z0-9_-])--backup-evidence([^A-Za-z0-9_-]|$)' "$OUT/runbook-fenced-commands"; then
    fail 'T18 schema-release recovery path is forbidden'
  fi
  previous=''
  while IFS= read -r line; do
    case "$line" in
      *--no-schema-change*)
        [[ "$line" == *gate=--no-schema-change* || "$previous" == *gate=--no-schema-change* ]] \
          || fail 'T18 no-schema-change command lacks its immediately preceding gate' ;;
    esac
    previous=$line
  done < "$section"
  for line in 'cells drain' 'cells undrain' '-register-draining' 'register_draining' 'civo-prod-use1-serving' 'gate=--no-schema-change'; do
    grep -Fq -- "$line" "$prose" || fail "T18 missing runbook contract: $line"
  done
  if grep -Fq 'roll the control plane back' "$prose"; then fail 'T18 forbidden control-plane rollback'; fi
  awk '
    /^Since the production-receive rollout installed the two$/ { active=1 }
    active && !NF { exit }
    active { print }
  ' "$runbook" > "$OUT/runbook-attestations"
  for line in 'CP_DEPLOY_CANONICAL_EMAIL_ACTIVE=true' 'CP_DEPLOY_SIGNUP_OPEN_ACTIVE=true'; do
    grep -Fq "$line" "$OUT/runbook-attestations" || fail "T18 missing B2 attestation: $line"
  done
  if grep -Fq 'signup-Turnstile activation' "$runbook"; then fail 'T18 stale signup attestation description'; fi

  brew_line=$(grep -n '^brew upgrade witself witself-admin witself-infra$' "$section" || true)
  plans_line=$(grep -n '^The chain does not deploy the public plans page' "$section" || true)
  chain_line=$(grep -n '^The chain, in order:$' "$section" || true)
  brew_line=${brew_line%%:*}; plans_line=${plans_line%%:*}; chain_line=${chain_line%%:*}
  [ -n "$brew_line" ] && [ -n "$plans_line" ] && [ -n "$chain_line" ] \
    && [ "$brew_line" -lt "$plans_line" ] && [ "$plans_line" -lt "$chain_line" ] \
    || fail 'T18 plans paragraph must follow brew and precede chain stages'
  awk '
    /^The chain does not deploy the public plans page/ { active=1 }
    active && !NF { exit }
    active { for (i=1; i<=NF; i++) { if (seen++) printf " "; printf "%s", $i } }
    END { print "" }
  ' "$section" > "$paragraph"
  for line in 'release-chain: STAGE cp PASS' 'or, when you recover by hand,' \
    '`scripts/release-deploy-control-plane.sh` prints its `release-deploy-control-plane: PASS control plane at` line' \
    'from a clean worktree at the same tag' 'If it fails, the cells still roll'; do
    grep -Fq "$line" "$paragraph" || fail "T18 missing plans deployment trigger or recovery: $line"
  done
  awk '
    /^The chain does not deploy the public plans page/ { active=1 }
    active && /^```sh$/ { fenced=1; next }
    fenced && /^```$/ { exit }
    fenced { print }
  ' "$section" > "$block"
  block_end=$(awk -v start="$plans_line" 'NR > start && /^```/ { if (++fences == 2) { print NR; exit } }' "$section")
  [ -n "$block_end" ] && [ "$block_end" -lt "$chain_line" ] || fail 'T18 plans fenced block must precede chain stages'
  "$BASH" -n "$block" > "$OUT/plans-syntax" 2>&1 || fail 'T18 plans block is not valid shell syntax'
  cmp -s "$block" "$OUT/plans-expected" || fail 'T18 plans block must contain the exact commands in order'

  awk '
    /^If a stage fails, fix the cause\./ { active=1 }
    active && /^```sh$/ { fenced=1; next }
    fenced && /^```$/ { exit }
    fenced { print }
  ' "$section" > "$recovery"
  # Collapse only the recovery command continuations to find the physical line
  # immediately after the complete control-plane command, including its flags.
  awk '
    {
      line=$0
      if (sub(/\\[[:space:]]*$/, "", line)) { pending=pending line " "; next }
      line=pending line; pending=""
      if (previous ~ /^scripts\/release-deploy-control-plane\.sh[[:space:]]/) {
        if (line != "# Then deploy the plans page from the same tag, as described above.") exit 1
        found++
      }
      previous=line
    }
    END { if (found != 1) exit 1 }
  ' "$recovery" || fail 'T18 plans reminder must immediately follow the recovery deploy command'
  awk '
    /^[[:space:]]*scripts\/roll-train\.sh[[:space:]]/ {
      count++
      if (previous != "# Only when release-schema-check printed gate=--no-schema-change:") exit 1
      if ($0 !~ /[[:space:]]--no-schema-change([[:space:]]|$)/) exit 1
    }
    { previous=$0 }
    END { if (count != 1) exit 1 }
  ' "$recovery" || fail 'T18 recovery must contain exactly one gated no-schema-change roll-train command'

  for line in 'This section is for releases without a schema change.' \
    '#back-up-both-reviewed-civo-databases-before-a-migration' '#two-wave-roll-automated' \
    'window and database settings that the founder approves' 'Do not run it from this section;' \
    'stops before tagging if the schema advances;' 'do not pass `--backup-evidence`' \
    'If release-schema-check printed gate=--backup-evidence, stop: this is a schema release.'; do
    grep -Fq -- "$line" "$prose" || fail "T18 missing schema-release boundary: $line"
  done
  for line in '## Back up both reviewed Civo databases before a migration' '## Two-wave roll (automated)'; do
    grep -Fxq "$line" "$runbook" || fail "T18 schema-release link target is missing: $line"
  done
  for line in 'run the chain again with `--backup-evidence' 'without backup evidence'; do
    if grep -Fq -- "$line" "$prose"; then fail 'T18 schema-release recovery path is forbidden'; fi
  done
  grep -Fq 'cell as "Two-wave roll (automated)" describes for the production pair. This section is for releases without a schema change.' "$OUT/runbook-paragraphs" \
    || fail 'T18 A5 must directly follow the production-pair sentence'
  timing_line=$(grep -n 'a stage that is running continues\.$' "$OUT/runbook-paragraphs" || true)
  hold_line=$(grep -n '^Once weekly scheduled restore drills are switched on' "$OUT/runbook-paragraphs" || true)
  close_line=$(grep -n '^### Close signups$' "$OUT/runbook-paragraphs" || true)
  timing_line=${timing_line%%:*}; hold_line=${hold_line%%:*}; close_line=${close_line%%:*}
  [ -n "$timing_line" ] && [ -n "$hold_line" ] && [ -n "$close_line" ] \
    && [ "$timing_line" -lt "$hold_line" ] && [ "$hold_line" -lt "$close_line" ] \
    || fail 'T18 weekly drill hold must follow timing and precede Close signups'
  grep '^Once weekly scheduled restore drills are switched on' "$OUT/runbook-paragraphs" > "$OUT/weekly-hold"
  for line in 'Once weekly scheduled restore drills are switched on' \
    'default Wednesday, 16:00-20:00Z' 'from 13:30Z' 'from 15:30Z' 'until 21:32Z' \
    'witself_control_plane_account_backup_restore_drill_running' 'on the control plane' \
    '/metrics/probes' 'is 0. An absent gauge is not zero.' 'shift these times with it.'; do
    grep -Fq "$line" "$OUT/weekly-hold" || fail "T18 missing weekly drill hold: $line"
  done
  pass 'T18 the runbook section matches the scripts'
  # B1 and B2 are the complete documentation footprint of this slice. Scan both
  # after merge too, without depending on an uncommitted Git diff or its context.
  cat "$section" "$OUT/runbook-attestations" > "$OUT/runbook-public-additions"
  if grep -nE '/Users/|\$HOME|~/\.witself|acc_[a-z0-9]|sec_[a-z0-9]|k8s\.civo\.com|[0-9a-f]{32}|\.gitops/cells|/private/tmp|~/\.dsh|\$\{HOME\}|(agent|realm|cell|adm)_[a-z0-9]{8,}|[A-F0-9]{32,}|workers\.dev' "$OUT/runbook-public-additions" > "$OUT/static"; then
    fail 'T19 runbook operator values'
  fi
  pass 'T19 runbook has no operator values'
}
check_runbook_contract

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
# A pattern file keeps the audit values off argv and makes whole-tree scans fast.
printf '%s\n' "$EDGE" "$CF_FILE" "$CF_ENV" "$FLEET" "$INHERITED_EDGE" "$INHERITED_FLEET" "$CF_OLD" "$HEX" > "$SECRET_DIR/audit-fixtures"
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
        [ "${RO_TEST_CHAIN:-false}" != true ] || version=1.2.4
        [ "$scenario" != chain-serving-stale ] || version=1.2.3
        [ "$scenario" != chain-serving-fail ] || exit 22
        printf '{"version":"%s","commit":"12345678abcdef","store_schema_version":%s}\n' "$version" "${RO_TEST_SCHEMA:-2}" ;;
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
  tee)
    if [ "$scenario" = logger-fail ]; then "$RO_TEST_ROOT/real/tee" "$@"; exit 17; fi
    exec "$RO_TEST_ROOT/real/tee" "$@" ;;
  sed)
    if [ "$scenario" = redaction-fail ]; then
      count redactions
      [ "$n" -ne 2 ] || exit 19
    fi
    exec "$RO_TEST_ROOT/real/$name" "$@" ;;
  gh)
    case "$1 $2" in
      'pr view')
        case " $* " in
          *' mergeable '*)
            count mergeable
            value=MERGEABLE
            [ "$n" -ne 1 ] || value=UNKNOWN
            [ "$scenario" != conflicting ] || value=CONFLICTING
            [ "$scenario" != unknown-mergeable ] || value=UNKNOWN
            printf '{"mergeable":"%s"}\n' "$value" ;;
          *' headRefOid '*)
            if [ "$scenario" = wrong-head ]; then printf '%s\n' "$RO_TEST_MERGE"; else printf '%s\n' "$RO_TEST_HEAD"; fi ;;
          *' mergeCommit '*)
            count merge_oid
            if [ "$n" = 1 ] || [ "$scenario" = missing-merge ]; then printf 'null\n'; else printf '%s\n' "$RO_TEST_MERGE"; fi ;;
          *) exit 97 ;;
        esac ;;
      'pr checks')
        count checks
        case "$scenario" in
          checks-read-error) printf 'transport refused\n' >&2; exit 4 ;;
          checks-empty) printf "no checks reported on the 'fixture' branch\n" >&2; exit 1 ;;
          checks-empty-json) printf '[]\n'; exit 0 ;;
          checks-fail) printf '[{"name":"fixture-check","bucket":"fail","state":"FAILURE","link":""}]\n'; exit 1 ;;
          checks-cancel) printf '[{"name":"fixture-check","bucket":"cancel","state":"CANCELLED","link":""}]\n'; exit 1 ;;
          checks-fail-json) printf '[{"name":"fixture-check","bucket":"fail","state":"FAILURE","link":""}]\n'; exit 0 ;;
          checks-cancel-json) printf '[{"name":"fixture-check","bucket":"cancel","state":"CANCELLED","link":""}]\n'; exit 0 ;;
          checks-malformed) printf '{}\n'; exit 0 ;;
        esac
        if [ "$n" = 1 ] || [ "$scenario" = checks-pending ]; then
          printf '[{"name":"fixture-check","bucket":"pending","state":"PENDING","link":""}]\n'; exit 8
        fi
        printf '[{"name":"fixture-check","bucket":"pass","state":"SUCCESS","link":""},{"name":"optional-check","bucket":"skipping","state":"SKIPPED","link":""}]\n' ;;
      'pr merge') printf 'merged\n' ;;
      'run list')
        if [[ " $* " == *' release.yml '* ]]; then
          count release_lists
          if [ "$scenario" = no-release-run ]; then printf '[]\n'; exit 0; fi
          printf '[{"databaseId":70,"headBranch":"v9.9.9","event":"push","status":"completed","conclusion":"success"},{"databaseId":71,"headBranch":"v1.2.4","event":"workflow_dispatch","status":"completed","conclusion":"success"},{"databaseId":72,"headBranch":"v1.2.4","event":"push","status":"completed","conclusion":"success"}]\n'
        else
          count ci_lists
          status=completed; conclusion=success
          case "$scenario" in
            ci-failure|post-failure) conclusion=failure ;;
            ci-cancelled|post-cancelled) conclusion=cancelled ;;
            ci-pending) status=in_progress; conclusion='' ;;
          esac
          if [ "${RO_TEST_MERGING:-false}" = true ]; then
            if [ "$scenario" = post-failure ] || { [ "$scenario" = post-cancelled ] && [ "$n" = 1 ]; }; then
              printf '[{"databaseId":63,"headSha":"%s","status":"completed","conclusion":"%s"}]\n' "$RO_TEST_MERGE" "$conclusion"
            elif [ "$scenario" = post-unrelated ]; then
              printf '[{"databaseId":64,"headSha":"%s","status":"completed","conclusion":"success"}]\n' "$RO_TEST_OLDER"
            else
              printf '[{"databaseId":64,"headSha":"%s","status":"completed","conclusion":"success"}]\n' "$RO_TEST_HEAD"
            fi
          else
            printf '[{"databaseId":60,"headSha":"%s","status":"completed","conclusion":"success"},{"databaseId":61,"headSha":"%s","status":"%s","conclusion":"%s"}]\n' "$RO_TEST_HEAD" "$RO_TEST_MERGE" "$status" "$conclusion"
          fi
        fi ;;
      'run view')
        if [[ " $* " == *' jobs '* ]]; then
          printf '{"jobs":[{"name":"release-build-fixture","conclusion":"failure"},{"name":"success-job","conclusion":"success"}]}\n'
        else
          count release_polls
          status=completed; conclusion=success
          [ "$scenario" != release-fail ] || conclusion=failure
          if [ "$scenario" = release-pending ]; then status=in_progress; conclusion=''; fi
          printf '{"status":"%s","conclusion":"%s"}\n' "$status" "$conclusion"
        fi ;;
      'release view')
        draft=false; prerelease=false
        [ "$scenario" != draft ] || draft=true
        [ "$scenario" != prerelease ] || prerelease=true
        printf '{"isDraft":%s,"isPrerelease":%s,"assets":[{"name":"fixture-a"},{"name":"fixture-b"}]}\n' "$draft" "$prerelease" ;;
      *) exit 98 ;;
    esac ;;
  witself-admin)
    printf 'preflight\n' >> "$RO_TEST_OUT/order"
    case "$scenario" in
      production-registered) printf '{"cell":{"name":"civo-prod-use1-serving"}}\n' ;;
      registry-fail) printf 'registry unavailable\n' >&2; exit 1 ;;
      registry-extra) printf 'witself-admin cells: cell is not registered with the control plane\nextra\n' >&2; exit 1 ;;
      *) printf 'witself-admin cells: cell is not registered with the control plane\n' >&2; exit 1 ;;
    esac ;;
  node|witself|yq|shasum) exit 98 ;;
  *) exec "$RO_TEST_ROOT/real/$name" "$@" ;;
esac
STUB
chmod 755 "$BIN/dispatch"
mkdir "$TEST_ROOT/real"
for name in sed awk env cat jq grep tr printf head tail cut xargs tee dirname stat wc mktemp chmod rm; do
  real=$(PATH=$ORIGINAL_PATH type -P "$name")
  ln -s "$real" "$TEST_ROOT/real/$name"
  ln -s dispatch "$BIN/$name"
done
for name in git date sleep gh curl npm node kubectl witself witself-admin yq shasum; do ln -s dispatch "$BIN/$name"; done
export PATH=$BIN:$ORIGINAL_PATH
DEPLOY=$REPO/scripts/release-deploy-control-plane.sh
DRILL=$REPO/scripts/release-drill-proof.sh
SCHEMA=$REPO/scripts/release-schema-check.sh
TAG=$REPO/scripts/release-tag.sh
MERGE=$REPO/scripts/release-merge-pr.sh
CHAIN=$REPO/scripts/release-chain.sh
RO_TEST_HEAD=$("$REAL_GIT" -C "$REPO" rev-parse HEAD)
export RO_TEST_HEAD
export RO_TEST_MERGE=$TAG_COMMIT
RO_TEST_OLDER=$("$REAL_GIT" -C "$REPO" rev-parse v1.2.2)
export RO_TEST_OLDER
chain_args=(1.2.4 --serving-url https://serving.example.invalid --edge-token-file "$SECRET_DIR/edge" --cloudflare-token-file "$SECRET_DIR/cf" --deploy-env-file "$SECRET_DIR/deploy-env" --fleet-token-file "$SECRET_DIR/fleet" --drill-account-id fixture_account)
chain_env_args=(1.2.4 --serving-url https://serving.example.invalid --edge-token-file "$SECRET_DIR/edge" --deploy-env-file "$SECRET_DIR/deploy-env" --fleet-token-file "$SECRET_DIR/fleet" --drill-account-id fixture_account)
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
  export RO_TEST_CHAIN=false RO_TEST_MERGING=false RO_TEST_SCHEMA=2 RO_TEST_FAIL_STAGE='' RO_TEST_NO_PASS='' RO_TEST_STAGE_DELAY='' RO_TEST_GATE=--no-schema-change
}
run() {
  set +e
  (
    if [ -n "$RO_TEST_CWD" ]; then cd "$RO_TEST_CWD"; fi
    if [ "$RO_TEST_INHERITED" = true ]; then
      export CONTROL_PLANE_EDGE_TOKEN=$INHERITED_EDGE WITSELF_FLEET_TOKEN=$INHERITED_FLEET
      # These exported sentinels intentionally belong only to the test child.
      # shellcheck disable=SC2030
      export RO_REDACT_EDGE=$INHERITED_EDGE RO_REDACT_CF=$CF_OLD
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
  local file line matches
  # All matches must be synthetic fixture inputs; grep reports only filenames.
  matches=$(grep -rlF -f "$SECRET_DIR/audit-fixtures" "$TEST_ROOT") || fail "$CASE cannot scan fixture artifacts"
  while IFS= read -r file; do
    case "$file" in "$SECRET_DIR"/*) continue ;; esac
    fail "$CASE raw fixture escaped into a file"
  done <<< "$matches"
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
    release-schema-check) run "$script" 1.2.4 --serving-url https://serving.example.invalid ;;
    release-tag) run "$script" 1.2.4 ;;
    release-merge-pr) run "$script" 12 --expect-head "$RO_TEST_HEAD" ;;
    release-chain) run "$script" "${chain_args[@]}" --order cp-first ;;
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
      contains "$OUT/stdout" 'follow docs/runbook-control-plane-rollback.md'
      absent "$OUT/stdout" 'readiness row'
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

# 125A2: real local commit ancestry with entirely offline GitHub responses.
CASE=T3
reset_case
RO_TEST_MERGING=true
RO_TEST_INHERITED=true
run "$MERGE" 12 --expect-head "$RO_TEST_HEAD" --execute
expect_rc 0
contains "$OUT/calls" "gh <pr> <merge> <12> <--repo> <witwave-ai/witself> <--squash> <--match-head-commit> <$RO_TEST_HEAD> <--delete-branch>"
contains "$OUT/stdout" 'post-merge CI run 64 success'
merge_call=$(grep '^gh <pr> <merge>' "$OUT/calls")
[ "${merge_call%% CONTROL_PLANE_EDGE_TOKEN=*}" = "gh <pr> <merge> <12> <--repo> <witwave-ai/witself> <--squash> <--match-head-commit> <$RO_TEST_HEAD> <--delete-branch>" ] || fail 'T3 merge argv is not exact'
contains "$OUT/calls" 'sleep <20>'; contains "$OUT/calls" 'sleep <10>'; contains "$OUT/calls" 'sleep <60>'
[ "$(cat "$OUT/checks")" = 2 ] || fail 'T3 did not wait for settled checks'
contains "$OUT/calls" "<merge-base> <--is-ancestor> <$RO_TEST_MERGE> <$RO_TEST_HEAD>"
secret_audit
pass T3

for scenario in conflicting checks-fail checks-cancel checks-fail-json checks-cancel-json wrong-head checks-empty checks-empty-json checks-read-error checks-malformed checks-pending unknown-mergeable; do
  CASE="T4 $scenario"
  reset_case "$scenario"
  RO_TEST_MERGING=true
  limit=3
  case "$scenario" in checks-empty*) limit=20 ;; esac
  run "$MERGE" 12 --expect-head "$RO_TEST_HEAD" --ci-timeout-min "$limit" --execute
  expect_rc 1
  contains "$OUT/stdout" 'release-merge-pr: FAIL'
  absent "$OUT/calls" 'gh <pr> <merge>'
  case "$scenario" in
    checks-empty*) [ "$(cat "$OUT/checks")" = 15 ] || fail 'T4 empty checks bound' ;;
    checks-read-error) [ "$(cat "$OUT/checks")" = 3 ] || fail 'T4 consecutive read error bound' ;;
    checks-fail|checks-fail-json) contains "$OUT/stdout" $'fixture-check\tfail' ;;
    checks-cancel|checks-cancel-json) contains "$OUT/stdout" $'fixture-check\tcancel' ;;
  esac
done
for scenario in post-failure post-cancelled post-unrelated missing-merge; do
  CASE="T4 $scenario"
  reset_case "$scenario"
  RO_TEST_MERGING=true
  run "$MERGE" 12 --expect-head "$RO_TEST_HEAD" --ci-timeout-min 3 --execute
  contains "$OUT/calls" 'gh <pr> <merge>'
  if [ "$scenario" = post-cancelled ]; then
    expect_rc 0; [ "$(cat "$OUT/ci_lists")" = 2 ] || fail 'T4 cancelled run not ignored'
  else expect_rc 1; contains "$OUT/stdout" 'release-merge-pr: FAIL'; fi
  if [ "$scenario" = post-failure ]; then
    contains "$OUT/stdout" 'release-merge-pr: FAIL post-merge CI failed on the merge commit'
    [ "$(cat "$OUT/ci_lists")" = 1 ] || fail 'T4 post-failure did not stop at the first CI list'
  fi
done
pass T4

# Keep deploy's earlier v1.2.4 fixture intact until all 125A1 cases have run.
"$REAL_GIT" -C "$REPO" tag -d v1.2.4 >/dev/null
CASE=T5
reset_case
RO_TEST_INHERITED=true
run "$TAG" 1.2.4 --commit "$TAG_COMMIT" --execute
expect_rc 0
contains "$OUT/calls" "<tag> <-a> <v1.2.4> <-m> <v1.2.4> <$TAG_COMMIT>"
contains "$OUT/calls" '<push> <origin> <refs/tags/v1.2.4>'
contains "$OUT/calls" 'gh <run> <view> <72>'
absent "$OUT/calls" 'gh <run> <view> <71>'
contains "$OUT/stdout" 'release run 72 success; assets=2'
secret_audit
pass T5
"$REAL_GIT" -C "$REPO" tag -d v1.2.4 >/dev/null
for scenario in ci-failure ci-cancelled ci-pending; do
  CASE="T6 $scenario"
  reset_case "$scenario"
  run "$TAG" 1.2.4 --commit "$TAG_COMMIT" --ci-timeout-min 3 --execute
  expect_rc 1
  absent "$OUT/calls" '<tag> <-a>'
  absent "$OUT/calls" '<push>'
  [ "$scenario" != ci-cancelled ] || contains "$OUT/stdout" 'gh run rerun 61 --repo witwave-ai/witself'
  if [ "$scenario" = ci-pending ]; then [ "$(cat "$OUT/ci_lists")" -le 4 ] || fail 'T6 unbounded CI'; fi
done
CASE='T6 highest tag'
reset_case
"$REAL_GIT" -C "$REPO" tag v2.0.0 "$TAG_COMMIT"
run "$TAG" 1.2.4 --commit "$TAG_COMMIT" --execute
expect_rc 1; absent "$OUT/calls" '<tag> <-a>'; absent "$OUT/calls" '<push>'
"$REAL_GIT" -C "$REPO" tag -d v2.0.0 >/dev/null
CASE='T6 existing different commit'
reset_case
"$REAL_GIT" -C "$REPO" tag v1.2.4 "$TAG_COMMIT"
run "$TAG" 1.2.4 --commit "$RO_TEST_HEAD" --execute
expect_rc 1
contains "$OUT/stdout" 'existing release tag differs from --commit'
absent "$OUT/calls" 'gh '; absent "$OUT/calls" '<push>'
"$REAL_GIT" -C "$REPO" tag -d v1.2.4 >/dev/null
for scenario in release-fail release-pending draft prerelease no-release-run; do
  CASE="T6 $scenario"
  reset_case "$scenario"
  run "$TAG" 1.2.4 --commit "$TAG_COMMIT" --release-timeout-min 3 --execute
  expect_rc 1
  contains "$OUT/stdout" 'release-tag: FAIL'
  case "$scenario" in
    release-fail|release-pending) contains "$OUT/stdout" $'release-build-fixture\tfailure'; absent "$OUT/stdout" 'success-job' ;;
    no-release-run) [ "$(cat "$OUT/release_lists")" = 12 ] || fail 'T6 release discovery bound' ;;
  esac
  "$REAL_GIT" -C "$REPO" tag -d v1.2.4 >/dev/null
done
"$REAL_GIT" -C "$REPO" tag v1.2.4 "$TAG_COMMIT"
CASE='T6 existing tag default'
reset_case
run "$TAG" 1.2.4 --execute
expect_rc 0
absent "$OUT/calls" '<tag> <-a>'; absent "$OUT/calls" '<push>'
contains "$OUT/stdout" "v1.2.4 at ${TAG_COMMIT:0:8}"
contains "$OUT/stdout" 'not re-tagging'
absent "$OUT/calls" '<--workflow> <ci.yml>'
pass T6

# Both existing and requested commits must belong to the fetched main history.
CASE='T6 commit ancestry'
"$REAL_GIT" -C "$REPO" update-ref refs/remotes/origin/main "$RO_TEST_OLDER"
for version in 1.2.4 1.2.5; do
  reset_case
  run "$TAG" "$version" --commit "$TAG_COMMIT" --execute
  expect_rc 1; absent "$OUT/calls" 'gh '; absent "$OUT/calls" '<tag> <-a>'; absent "$OUT/calls" '<push>'
  CASE='T14 commit ancestry'
  reset_case
  run "$SCHEMA" "$version" --commit "$TAG_COMMIT" --serving-url https://serving.example.invalid --execute
  expect_rc 1; absent "$OUT/calls" 'curl '
  CASE='T6 commit ancestry'
done
"$REAL_GIT" -C "$REPO" update-ref refs/remotes/origin/main HEAD
pass 'T6 T14 commit ancestry'

CASE=T14
for live in 2 1 3; do
  reset_case
  RO_TEST_SCHEMA=$live
  RO_TEST_INHERITED=true
  run "$SCHEMA" 1.2.4 --serving-url https://serving.example.invalid --execute
  if [ "$live" = 3 ]; then
    expect_rc 1; contains "$OUT/stdout" "live schema 3 is ahead of v1.2.4's 2"
  else
    expect_rc 0
    contains "$OUT/stdout" 'previous=v1.2.3 changed=false'
    if [ "$live" = 2 ]; then contains "$OUT/stdout" 'gate=--no-schema-change'; else contains "$OUT/stdout" 'gate=--backup-evidence'; fi
  fi
  secret_audit
done
reset_case
run "$SCHEMA" 1.2.4 --commit "$RO_TEST_HEAD" --serving-url https://serving.example.invalid --execute
expect_rc 1
# Advance only the disposable repository; source checkout and refs stay untouched.
printf 'select 3;\n' > "$REPO/internal/store/migrations/0003_c.sql"
"$REAL_GIT" -C "$REPO" add internal/store/migrations/0003_c.sql
"$REAL_GIT" -C "$REPO" commit -qm third-migration
"$REAL_GIT" -C "$REPO" update-ref refs/remotes/origin/main HEAD
reset_case
RO_TEST_SCHEMA=3
run "$SCHEMA" 1.2.5 --serving-url https://serving.example.invalid --execute
expect_rc 0; contains "$OUT/stdout" 'gate=--backup-evidence'; contains "$OUT/stdout" 'changed=true live=3 tag=3'
printf 'select 10;\n' > "$REPO/internal/store/migrations/0010_d.sql"
"$REAL_GIT" -C "$REPO" add internal/store/migrations/0010_d.sql
"$REAL_GIT" -C "$REPO" commit -qm decimal-migration
"$REAL_GIT" -C "$REPO" update-ref refs/remotes/origin/main HEAD
reset_case
RO_TEST_SCHEMA=10
run "$SCHEMA" 1.2.5 --serving-url https://serving.example.invalid --execute
expect_rc 0; contains "$OUT/stdout" 'tag=10'
pass 'T14 schema'

# Refuse xtrace before even dirname for all four new entry points.
CASE='T8 A2 xtrace'
for script in "$MERGE" "$TAG" "$SCHEMA" "$CHAIN"; do
  for trace in flag shellopts sourced; do
    reset_case
    RO_TEST_INHERITED=true
    case "$trace" in
      flag) run -x "$script" --help ;;
      shellopts) run -c 'exec env SHELLOPTS=xtrace "$BASH" "$@"' trace "$script" --help ;;
      sourced) run -c 'set -x; source "$@"' trace "$script" --help ;;
    esac
    expect_rc 2
    name=${script##*/}; name=${name%.sh}
    last_line "$name: FAIL xtrace must be disabled"
    if grep -v '^env ' "$OUT/calls" | grep -q .; then fail "$CASE started a child before refusing tracing"; fi
    # The explicit test launcher precedes the script; it is not its child.
    if [ "$trace" = shellopts ]; then
      sed '/^env /d' "$OUT/calls" > "$OUT/calls.audit"
      mv "$OUT/calls.audit" "$OUT/calls"
    fi
    secret_audit
  done
done
pass "$CASE"

# Child stand-ins isolate orchestration from the already-tested stage programs.
mkdir "$TEST_ROOT/original-children"
for child in release-schema-check release-tag release-deploy-control-plane release-drill-proof; do
  cp "$REPO/scripts/$child.sh" "$TEST_ROOT/original-children/"
done
cat > "$TEST_ROOT/chain-child" <<'CHILD'
#!/usr/bin/env bash
set -euo pipefail
name=${0##*/}; name=${name%.sh}
case "$name" in
  release-schema-check) stage=schema ;;
  release-tag) stage=tag ;;
  release-deploy-control-plane) stage=cp ;;
  roll-train) stage=train ;;
  release-drill-proof) stage=drill ;;
  *) exit 99 ;;
esac
log="child-$stage"
for arg in "$@"; do log="$log <$arg>"; done
exported=$(export -p)
for key in CONTROL_PLANE_EDGE_TOKEN CLOUDFLARE_API_TOKEN CLOUDFLARE_API_TOKEN_OLD WITSELF_FLEET_TOKEN RO_REDACT_EDGE RO_REDACT_CF; do
  if [[ "$exported" == *" $key="* ]]; then log="$log $key=set"; else log="$log $key=unset"; fi
done
printf '%s\n' "$log" >> "$RO_TEST_OUT/calls"
unset exported CONTROL_PLANE_EDGE_TOKEN CLOUDFLARE_API_TOKEN CLOUDFLARE_API_TOKEN_OLD WITSELF_FLEET_TOKEN RO_REDACT_EDGE RO_REDACT_CF
# The dry-run preflight may validate a child. It must never record a stage.
if [ "${RO_TEST_CHAIN:-false}" != true ]; then exit 96; fi
printf '%s\n' "$stage" >> "$RO_TEST_OUT/order"
IFS= read -r clock < "$RO_TEST_OUT/clock"
printf '%s %s\n' "$stage" "$clock" >> "$RO_TEST_OUT/starts"
if [ "${RO_TEST_STAGE_DELAY%%:*}" = "$stage" ]; then sleep "${RO_TEST_STAGE_DELAY#*:}"; fi
if [ "$stage" = tag ]; then
  git -C "${0%/scripts/*}" tag --list v1.2.4 >/dev/null
  git -C "${0%/scripts/*}" push origin refs/tags/v1.2.4
fi
[ "$RO_TEST_NO_PASS" != "$stage" ] || { printf 'no verdict\n'; exit 0; }
case "$stage" in
  schema) printf '%s: PASS gate=%s previous=v1.2.3 changed=false live=2 tag=3 commit=12345678\n' "$name" "$RO_TEST_GATE" ;;
  train)
    if [ "$RO_TEST_SCENARIO" = wrong-train-verdict ]; then printf 'roll-train: Both waves verified at 1.2.3.\n'
    else printf 'roll-train: Both waves verified at 1.2.4.\n'; fi ;;
  *) printf '%s: PASS fixture\n' "$name" ;;
esac
[ "$RO_TEST_FAIL_STAGE" != "$stage" ] || exit 1
CHILD
for child in release-schema-check release-tag release-deploy-control-plane roll-train release-drill-proof; do
  cp "$TEST_ROOT/chain-child" "$REPO/scripts/$child.sh"
done
chain_reset() { reset_case "${1:-normal}"; RO_TEST_CHAIN=true; }
check_order() {
  local actual
  actual=$(tr '\n' ' ' < "$OUT/order")
  [ "$actual" = "$1 " ] || fail "$CASE wrong stage order: $actual"
}
for scenario in production-registered registry-fail registry-extra normal; do
  CASE="T21 $scenario"
  chain_reset "$scenario"
  run "$CHAIN" "${chain_args[@]}" --order cp-first --execute
  contains "$OUT/calls" "witself-admin <cells> <show> <civo-prod-use1-serving> <--token-file> <$SECRET_DIR/fleet> <--json>"
  if [ "$scenario" = normal ]; then expect_rc 0; else
    expect_rc 1; last_line 'release-chain: FAIL preflight v1.2.4'
    absent "$OUT/calls" 'child-'; absent "$OUT/calls" '<tag>'; absent "$OUT/calls" 'npm '
    if [ "$scenario" = production-registered ]; then contains "$OUT/stderr" 'this chain covers only the sandbox pair'; else contains "$OUT/stderr" 'registry read failed'; fi
  fi
done
pass T21

for mode in file env; do
  CASE="T8 chain $mode"
  chain_reset
  RO_TEST_INHERITED=true
  if [ "$mode" = file ]; then args=("${chain_args[@]}"); else args=("${chain_env_args[@]}"); fi
  run "$CHAIN" "${args[@]}" --order cp-first --execute
  expect_rc 0
  while IFS= read -r line; do
    case "$line" in
      child-cp*)
        if [ "$mode" = env ]; then
          [[ "$line" == *'CLOUDFLARE_API_TOKEN=set'* ]] || fail 'T8 env token missing from deploy child'
          continue
        fi ;;
    esac
    [[ "$line" != *'CLOUDFLARE_API_TOKEN=set'* ]] || fail 'T8 Cloudflare token inherited outside deploy child'
  done < "$OUT/calls"
  # Allow the intentional chain-to-deploy handoff, then reuse the strict audit.
  sed 's/^child-cp /npm <run> <deploy> /' "$OUT/calls" > "$OUT/calls.audit"
  mv "$OUT/calls.audit" "$OUT/calls"
  secret_audit
done
pass 'T8 chain'

CASE='T14 chain missing evidence'
chain_reset
RO_TEST_GATE=--backup-evidence
run "$CHAIN" "${chain_args[@]}" --order cp-first --execute
expect_rc 1; last_line 'release-chain: FAIL schema v1.2.4'
contains "$OUT/stderr" 'schema advances from 2 to 3'
check_order 'preflight schema'
absent "$OUT/calls" '<tag>'; absent "$OUT/calls" '<push>'; absent "$OUT/calls" 'gh <run> <list>'
mkdir "$TEST_ROOT/evidence-backup" "$TEST_ROOT/evidence-serving"
for gate in --backup-evidence --no-schema-change; do
  CASE='T14 chain evidence forwarding'
  chain_reset
  RO_TEST_GATE=$gate
  run "$CHAIN" "${chain_args[@]}" --order cp-first --backup-evidence "$TEST_ROOT/evidence-backup" --backup-evidence "$TEST_ROOT/evidence-serving" --execute
  expect_rc 0
  line=$(grep '^child-train ' "$OUT/calls")
  [[ "$line" == *"<--backup-evidence> <$TEST_ROOT/evidence-backup>"* && "$line" == *"<--backup-evidence> <$TEST_ROOT/evidence-serving>"* && "$line" != *'<--no-schema-change>'* ]] || fail 'T14 train evidence argv'
done
pass 'T14 chain'

for order in cp-first cells-first; do
  CASE="T13 $order"
  chain_reset
  run "$CHAIN" "${chain_args[@]}" --order "$order" --execute
  expect_rc 0
  if [ "$order" = cp-first ]; then check_order 'preflight schema tag cp train drill'; else check_order 'preflight schema tag train cp drill'; fi
  last_line "release-chain: PASS v1.2.4 (order $order; train gate --no-schema-change; drill validated)"
  contains "$OUT/stdout" 'next: brew upgrade witself witself-admin witself-infra'
  contains "$OUT/calls" "child-tag <1.2.4> <--commit> <$TAG_COMMIT>"
  for stage in preflight schema tag cp train drill; do contains "$OUT/stdout" "release-chain: STAGE $stage PASS"; done
  secret_audit
done
CASE='T13 fresh release commit'
chain_reset
"$REAL_GIT" -C "$REPO" tag -d v1.2.4 >/dev/null
fresh_commit=$("$REAL_GIT" -C "$REPO" rev-parse origin/main)
run "$CHAIN" "${chain_args[@]}" --order cp-first --execute
expect_rc 0
check_order 'preflight schema tag cp train drill'
for child in schema tag; do
  contains "$OUT/calls" "child-$child <1.2.4> <--commit> <$fresh_commit>"
done
last_line 'release-chain: PASS v1.2.4 (order cp-first; train gate --no-schema-change; drill validated)'
secret_audit
"$REAL_GIT" -C "$REPO" tag v1.2.4 "$TAG_COMMIT"
for failed in schema tag cp train drill; do
  for reason in exit verdict; do
    CASE="T13 $failed $reason"
    chain_reset
    if [ "$reason" = exit ]; then RO_TEST_FAIL_STAGE=$failed; else RO_TEST_NO_PASS=$failed; fi
    run "$CHAIN" "${chain_args[@]}" --order cp-first --execute
    expect_rc 1
    last_line "release-chain: FAIL $failed v1.2.4"
    contains "$OUT/stdout" "release-chain: STAGE $failed FAIL"
    case "$failed" in
      schema) check_order 'preflight schema' ;;
      tag) check_order 'preflight schema tag' ;;
      cp) check_order 'preflight schema tag cp' ;;
      train) check_order 'preflight schema tag cp train' ;;
      drill) check_order 'preflight schema tag cp train drill' ;;
    esac
  done
done
CASE='T13 checkout head'
chain_reset
"$REAL_GIT" -C "$REPO" update-ref refs/remotes/origin/main "$TAG_COMMIT"
run "$CHAIN" "${chain_args[@]}" --order cp-first --execute
expect_rc 1; last_line 'release-chain: FAIL preflight v1.2.4'; absent "$OUT/calls" 'child-'
"$REAL_GIT" -C "$REPO" update-ref refs/remotes/origin/main HEAD
pass T13

# Refuse malformed preflight inputs before any action; preserve stage logs safely.
CASE='T1 cells-first plan'
chain_reset
run "$CHAIN" "${chain_args[@]}" --order cells-first
expect_rc 0; no_action
plan_order=''
while IFS= read -r line; do
  case "$line" in
    *'/scripts/roll-train.sh '*) plan_order="${plan_order}train " ;;
    *'/scripts/release-deploy-control-plane.sh '*) plan_order="${plan_order}cp " ;;
  esac
done < "$OUT/stdout"
[ "$plan_order" = 'train cp ' ] || fail 'T1 cells-first plan prints the wrong child order'
last_line 'release-chain: DRY-RUN (no action taken; add --execute)'
CASE='T13 chain preflight validation'
for invalid in 'https://[::1]' 'https://serving.example.invalid/path'; do
  chain_reset
  run "$CHAIN" "${chain_args[@]}" --order cp-first --serving-url "$invalid" --execute
  expect_rc 2; no_action
done
chain_reset
printf 'UNKNOWN=fixture\n' > "$SECRET_DIR/invalid-env"
run "$CHAIN" "${chain_args[@]}" --order cp-first --deploy-env-file "$SECRET_DIR/invalid-env" --execute
expect_rc 2; no_action
chain_reset
chmod 644 "$SECRET_DIR/fleet"
run "$CHAIN" "${chain_args[@]}" --order cp-first --execute
chmod 600 "$SECRET_DIR/fleet"
[ "$RC" = 1 ] || [ "$RC" = 2 ] || fail 'T13 chain accepted an unsafe fleet input'
no_action

for scenario in chain-serving-stale chain-serving-fail wrong-train-verdict logger-fail; do
  CASE="T13 $scenario"
  chain_reset "$scenario"
  run "$CHAIN" "${chain_args[@]}" --order cp-first --execute
  expect_rc 1
  if [ "$scenario" = logger-fail ]; then
    last_line 'release-chain: FAIL schema v1.2.4'; check_order 'preflight schema'
  else
    last_line 'release-chain: FAIL train v1.2.4'; check_order 'preflight schema tag cp train'
  fi
done
CASE='T13 forwarded options and private logs'
chain_reset
mkdir "$TEST_ROOT/chain-logs"
run "$CHAIN" "${chain_args[@]}" --order cells-first --workdir "$TEST_ROOT/chain-logs" --repo fixture/project --drill-cell fixture-drill --attest-canonical-email-active --attest-signup-open-active --execute
expect_rc 0
contains "$OUT/calls" 'child-tag <1.2.4>'
contains "$OUT/calls" '<--repo> <fixture/project>'
for stage in cp train; do
  line=$(grep "^child-$stage " "$OUT/calls")
  [[ "$line" == *"<--workdir> <$TEST_ROOT/chain-logs>"* ]] || fail 'T13 missing forwarded workdir'
done
line=$(grep '^child-cp ' "$OUT/calls")
for pair in "<--edge-token-file> <$SECRET_DIR/edge>" "<--cloudflare-token-file> <$SECRET_DIR/cf>" "<--deploy-env-file> <$SECRET_DIR/deploy-env>"; do
  [[ "$line" == *"$pair"* ]] || fail 'T13 missing deploy file forwarding'
done
[[ "$line" == *'<--attest-canonical-email-active>'* && "$line" == *'<--attest-signup-open-active>'* ]] || fail 'T13 missing deploy attestations'
line=$(grep '^child-drill ' "$OUT/calls")
for pair in '<--account-id> <fixture_account>' "<--fleet-token-file> <$SECRET_DIR/fleet>" '<--drill-cell> <fixture-drill>'; do
  [[ "$line" == *"$pair"* ]] || fail 'T13 missing drill forwarding'
done
line=$(grep '^child-schema ' "$OUT/calls")
[[ "$line" == *"<--commit> <$TAG_COMMIT>"* && "$line" == *'<--serving-url> <https://serving.example.invalid>'* ]] || fail 'T13 missing schema input forwarding'
line=$(grep '^child-train ' "$OUT/calls")
[[ "$line" == *'<--serving-url> <https://serving.example.invalid>'* && "$line" == *'<--no-schema-change>'* ]] || fail 'T13 missing train input forwarding'
for stage in schema tag cp train drill; do [ -s "$TEST_ROOT/chain-logs/$stage.log" ] || fail 'T13 missing stage log'; done
secret_audit
CASE='T13 symbolic stage log'
chain_reset
rm "$TEST_ROOT/chain-logs/schema.log"
printf 'preserve\n' > "$OUT/log-target"
ln -s "$OUT/log-target" "$TEST_ROOT/chain-logs/schema.log"
run "$CHAIN" "${chain_args[@]}" --order cp-first --workdir "$TEST_ROOT/chain-logs" --execute
expect_rc 1; last_line 'release-chain: FAIL schema v1.2.4'; check_order 'preflight'
[ "$(cat "$OUT/log-target")" = preserve ] || fail 'T13 overwritten symbolic log target'
pass 'T13 preflight and stage safeguards'

for row in 'cp-first 83100 none cp 89100' 'cp-first 72000 cp:4200 train 89100' 'cells-first 79200 none train 89100' 'cells-first 28800 none train 28800' 'cp-first 28800 train:54600 drill 89100'; do
  CASE="T15 stage $row"
  chain_reset
  read -r order start delay stage expected <<< "$row"
  printf '%s\n' "$start" > "$OUT/clock"
  RO_TEST_STAGE_DELAY=$delay
  run "$CHAIN" "${chain_args[@]}" --order "$order" --execute
  expect_rc 0
  contains "$OUT/starts" "$stage $expected"
  if [ "$expected" = 28800 ]; then absent "$OUT/calls" 'sleep <300>'; fi
done
# Exercise each stage's own clearance edge. The train delay reaches the drill
# boundary after the earlier guards have passed; later waits cannot hide a
# premature cp start because we check the selected stage's exact timestamp.
for row in \
  'cp 23:00Z cp-first 82800 none 89100' \
  'cp 22:59Z cp-first 82740 none 82740' \
  'drill 23:00Z cp-first 28800 train:54000 89100' \
  'drill 22:59Z cp-first 28800 train:53940 82740' \
  'train 21:00Z cells-first 75600 none 89100' \
  'train 20:59Z cells-first 75540 none 75540'; do
  read -r stage boundary order start delay expected <<< "$row"
  CASE="T15 $stage clearance $boundary"
  chain_reset
  printf '%s\n' "$start" > "$OUT/clock"
  RO_TEST_STAGE_DELAY=$delay
  run "$CHAIN" "${chain_args[@]}" --order "$order" --execute
  expect_rc 0
  grep -Fxq "$stage $expected" "$OUT/starts" || fail "$CASE wrong stage start; expected $expected"
done
CASE='T15 frozen clock'
chain_reset frozen-clock
printf '83100\n' > "$OUT/clock"
run "$CHAIN" "${chain_args[@]}" --order cp-first --execute
expect_rc 1; last_line 'release-chain: FAIL cp v1.2.4'
[ "$(grep -c '^sleep <300>' "$OUT/calls")" = 48 ] || fail 'T15 maximum wait not 240 minutes'
check_order 'preflight schema tag'
pass 'T15 stages'

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
pass 'T16 T19 T20'
printf 'release ops test: PASS all 125A1, 125A2 and 125B contracts\n'
