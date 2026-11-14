#!/usr/bin/env bash
set -euo pipefail

source_root=$(CDPATH='' cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
work_dir=$(mktemp -d "${TMPDIR:-/tmp}/witself-store-integration-test.XXXXXX")
trap 'rm -rf -- "$work_dir"' EXIT
trap 'exit 129' HUP
trap 'exit 130' INT
trap 'exit 143' TERM
fail() { printf 'store integration regression: FAIL: %s\n' "$1" >&2; exit 1; }

awk '
  /^[^ \t#]/ { recipe = 0 }
  /^test-integration:([ \t]|$)/ {
    recipe = 1; targets++
    if ($0 != "test-integration: db-up ## Run the PostgreSQL-backed store tests in a disposable database") bad = 1
    next
  }
  recipe && /^\t/ {
    lines++
    if ($0 != "\tbash scripts/run-store-integration.sh \"$(STORE_TEST_TIMEOUT)\"") bad = 1
  }
  END { exit !(targets == 1 && lines == 1 && !bad) }
' "$source_root/Makefile" || fail 'Makefile target, prerequisite, help or recipe changed'

mkdir -p "$work_dir/bin"
cat >"$work_dir/bin/docker" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
printf '%s\n' "$@" >>"$STORE_CASE_DIR/arguments"
[[ "$1 $2 $3 $4" == 'compose exec -T postgres' ]] || exit 91
# Like the real compose exec, consume whatever stdin the caller left attached.
cat >/dev/null
shift 4
printf '%s\n' "$*" >>"$STORE_CASE_DIR/calls"
case "$1" in
  createdb) exit "$STORE_CREATE_STATUS" ;;
  psql)
    [[ "$*" == 'psql -At -U witself -d postgres -c SELECT datname FROM pg_database' ]] || exit 92
    [[ "$STORE_QUERY_STATUS" == 0 ]] || { printf 'synthetic catalog failure\n' >&2; exit "$STORE_QUERY_STATUS"; }
    base=$(awk '$1 == "createdb" { print $4 }' "$STORE_CASE_DIR/calls")
    sed "s/@BASE@/$base/g" "$STORE_CASE_DIR/fixture"
    ;;
  dropdb)
    printf '%s\n' "${@:5}" >>"$STORE_CASE_DIR/drop-targets"
    printf 'synthetic drop diagnostic\n' >&2
    exit "$STORE_DROP_STATUS"
    ;;
  *) exit 93 ;;
esac
SH
cat >"$work_dir/bin/go" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
printf '%s\n' "$*" >>"$STORE_CASE_DIR/go"
base=$(awk '$1 == "createdb" { print $4 }' "$STORE_CASE_DIR/calls")
[[ ${WITSELF_TEST_DATABASE_URL##*/} == "$base?sslmode=disable" ]] || exit 94
[[ ${WITSELF_TEST_REQUIRE_DATABASE-unset} == unset &&
   ${WITSELF_TEST_REQUIRE_PACKAGE_DATABASE-unset} == unset ]] || exit 95
if [[ -n "$STORE_SIGNAL" ]]; then kill -s "$STORE_SIGNAL" "$PPID"; fi
exit "$STORE_GO_STATUS"
SH
chmod +x "$work_dir/bin/docker" "$work_dir/bin/go"

run_case() {
  local name=$1 expected=$2 count=$3 result=0 base decoy index
  export STORE_CASE_DIR="$work_dir/$name"
  export STORE_GO_STATUS=${4:-0} STORE_QUERY_STATUS=${5:-0} STORE_CREATE_STATUS=${6:-0}
  export STORE_SIGNAL=${7:-} STORE_DROP_STATUS=${8:-0}
  mkdir -p "$STORE_CASE_DIR"
  cat >"$STORE_CASE_DIR/decoys" <<EOF
@BASE@_0123456789a
@BASE@_0123456789abc0
@BASE@_0123456789ag
@BASE@_ABCDEFABCDEF
different_0123456789ab
@BASE@x_0123456789ab
postgres
witself

-x
a;b
\$(touch $work_dir/pwned)
EOF
  printf '@BASE@_0123456789ab \n' >>"$STORE_CASE_DIR/decoys"
  cp "$STORE_CASE_DIR/decoys" "$STORE_CASE_DIR/fixture"
  for ((index=0; index<count; index++)); do
    printf '@BASE@_0123456789a%s\n' "$index" >>"$STORE_CASE_DIR/fixture"
  done
  (
    unset WITSELF_TEST_DATABASE_URL WITSELF_TEST_REQUIRE_DATABASE WITSELF_TEST_REQUIRE_PACKAGE_DATABASE
    PATH="$work_dir/bin:$PATH" bash "$source_root/scripts/run-store-integration.sh" 17m </dev/null
  ) >"$STORE_CASE_DIR/stdout" 2>"$STORE_CASE_DIR/stderr" || result=$?
  [[ "$result" == "$expected" ]] || fail "$name: exit $result, expected $expected"
  base=$(awk '$1 == "createdb" { print $4 }' "$STORE_CASE_DIR/calls")
  [[ "$base" =~ ^witself_test_[0-9]{14}_[0-9]+$ && ${#base} -lt 50 ]] || fail "$name: base shape"
  {
    printf 'createdb -U witself %s\n' "$base"
    printf 'psql -At -U witself -d postgres -c SELECT datname FROM pg_database\n'
    if [[ "$STORE_QUERY_STATUS" == 0 ]]; then
      for ((index=0; index<count; index++)); do
        printf 'dropdb --if-exists -U witself %s_0123456789a%s\n' "$base" "$index"
      done
    fi
    printf 'dropdb --if-exists -U witself %s\n' "$base"
  } >"$STORE_CASE_DIR/expected"
  cmp -s "$STORE_CASE_DIR/expected" "$STORE_CASE_DIR/calls" || fail "$name: exact call/drop order"
  if [[ "$STORE_CREATE_STATUS" == 0 ]]; then
    [[ $(cat "$STORE_CASE_DIR/go") == 'test ./internal/store -count=1 -timeout=17m' ]] || fail "$name: go arguments"
  else
    [[ ! -e "$STORE_CASE_DIR/go" ]] || fail "$name: go ran after createdb failure"
  fi
  sed "s/@BASE@/$base/g" "$STORE_CASE_DIR/decoys" >"$STORE_CASE_DIR/resolved-decoys"
  while IFS= read -r decoy; do
    # postgres and witself are required container/user arguments, never drop targets.
    if grep -Fxq -- "$decoy" "$STORE_CASE_DIR/drop-targets"; then fail "$name: decoy drop target"; fi
    if [[ "$decoy" != postgres && "$decoy" != witself ]] &&
      grep -Fxq -- "$decoy" "$STORE_CASE_DIR/arguments"; then fail "$name: decoy passed as argument"; fi
  done <"$STORE_CASE_DIR/resolved-decoys"
  [[ ! -e "$work_dir/pwned" && ! -s "$STORE_CASE_DIR/stdout" ]] || fail "$name: unexpected side effect/output"
  : >"$STORE_CASE_DIR/expected-stderr"
  if [[ "$STORE_QUERY_STATUS" != 0 ]]; then
    printf 'NOT RUN store_integration_derived_cleanup\n' >"$STORE_CASE_DIR/expected-stderr"
  fi
  cmp -s "$STORE_CASE_DIR/expected-stderr" "$STORE_CASE_DIR/stderr" || fail "$name: exact stderr"
  printf 'store integration regression: %s passed\n' "$name"
}

run_case pass 0 1
run_case go-failure 7 1 7
run_case term 143 1 0 0 0 TERM
run_case catalog-failure 7 1 7 8
run_case createdb-failure 9 0 0 0 9
run_case two-derived 0 2
run_case hup 129 1 0 0 0 HUP
run_case int 130 1 0 0 0 INT
run_case empty 0 0
run_case drop-failure 7 2 7 0 0 '' 8
