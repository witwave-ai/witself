#!/usr/bin/env bash
# Run store tests in a disposable base and remove its package databases on exit.
set -euo pipefail
export LC_ALL=C

store_test_timeout=${1:?store test timeout required}
integration_db="witself_test_$(date -u +%Y%m%d%H%M%S)_$$"
cleanup_integration_db() {
  local status=$? databases database
  trap - EXIT
  trap '' HUP INT TERM
  # compose exec attaches stdin: without the redirect the first drop inside
  # the loop would swallow the rest of the catalog listing.
  if databases=$(docker compose exec -T postgres psql -At -U witself -d postgres \
    -c 'SELECT datname FROM pg_database' </dev/null 2>/dev/null); then
    while IFS= read -r database; do
      if [[ "$database" =~ ^${integration_db}_[0-9a-f]{12}$ ]]; then
        docker compose exec -T postgres dropdb --if-exists -U witself "$database" </dev/null >/dev/null 2>&1 || :
      fi
    done <<<"$databases"
  else
    printf 'NOT RUN store_integration_derived_cleanup\n' >&2
  fi
  docker compose exec -T postgres dropdb --if-exists -U witself "$integration_db" </dev/null >/dev/null 2>&1 || :
  return "$status"
}
trap cleanup_integration_db EXIT
trap 'exit 129' HUP
trap 'exit 130' INT
trap 'exit 143' TERM

docker compose exec -T postgres createdb -U witself "$integration_db"
WITSELF_TEST_DATABASE_URL="postgres://witself:witself@localhost:5432/$integration_db?sslmode=disable" \
  go test ./internal/store -count=1 -timeout="$store_test_timeout"
