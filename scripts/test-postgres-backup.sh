#!/usr/bin/env bash
# Offline Helm units and failure-path tests; no database, cloud, or credentials.
# Optional separate SQL/grant integration fixture (requires the local codex_a DB):
# WITSELF_TEST_REQUIRE_DATABASE=1 WITSELF_TEST_DATABASE_URL=postgres://postgres:test@127.0.0.1:5599/codex_a \
#   python3 scripts/testdata/postgres-backup-sql.py "$PWD"
# The SQL fixture refuses other databases and rolls back all changes.
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)"
ruby "$repo_root/scripts/testdata/postgres-backup-image.rb" "$repo_root"
ruby "$repo_root/scripts/testdata/postgres-backup-chart.rb" "$repo_root"
python3 "$repo_root/scripts/testdata/postgres-backup-runner.py" "$repo_root"
