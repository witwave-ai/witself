#!/usr/bin/env bash
# Offline Helm render assertions; no cluster, provider, database, or credentials.
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)"
ruby "$repo_root/scripts/testdata/egress-networkpolicy.rb" "$repo_root"
