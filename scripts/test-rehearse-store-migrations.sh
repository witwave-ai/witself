#!/bin/bash
# Offline contract checks: all Docker and helper calls use private stubs.
set -euo pipefail
export PATH=/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin
export LC_ALL=C LANG=C LANGUAGE=C

repo_root=$(CDPATH='' cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
exec python3 "$repo_root/scripts/testdata/rehearse-migrations-test.py" "$repo_root"
