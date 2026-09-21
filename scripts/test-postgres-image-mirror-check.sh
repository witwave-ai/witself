#!/usr/bin/env bash
# Exercise local/CI gate behavior without registry access or credentials.
set -euo pipefail
repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)"
python3 - "$repo_root" <<'PY'
import json
import os
from pathlib import Path
import shutil
import subprocess
import sys
import tempfile

source = Path(sys.argv[1])
bash = shutil.which('bash')
with tempfile.TemporaryDirectory(prefix='witself-postgres-mirror-check-test.') as work:
    root = Path(work)
    repo = root / 'repo'
    scripts = repo / 'scripts'
    scripts.mkdir(parents=True)
    config = repo / 'images/postgresql/mirror.json'
    config.parent.mkdir(parents=True)
    shutil.copyfile(source / 'scripts/check-postgres-image-mirror.sh',
                    scripts / 'check-postgres-image-mirror.sh')
    # This stub proves wrapper dispatch, local destinations and cleanup. The
    # separate mirror suite exercises Skopeo and raw-manifest verification.
    (scripts / 'mirror-postgresql-image.sh').write_text('''#!/usr/bin/env bash
set -euo pipefail
[[ $# == 4 && $1 == 0.0.0 && $3 == "$CHECK_CONFIG" ]]
[[ $4 == "dir:$TMPDIR/"*"/$2" ]]
printf '%s\\n' "$2" >>"$CHECK_LOG"
mkdir -p "${4#dir:}"
[[ $2 != "$CHECK_FAIL_CELL" ]]
''')
    available = root / 'available'
    absent = root / 'absent'
    available.mkdir()
    absent.mkdir()
    for tool in ('bash', 'dirname', 'jq', 'mktemp', 'rm', 'mkdir'):
        executable = shutil.which(tool)
        assert executable, f'missing test dependency: {tool}'
        (available / tool).symlink_to(executable)
    (available / 'skopeo').symlink_to(shutil.which('true'))
    cases = [
        ('local-missing', False, '', '', '', True, True),
        ('ci-missing', False, 'true', '', '', False, False),
        ('ci-one-missing', False, '1', '', '', False, False),
        ('actions-missing', False, '', 'true', '', False, False),
        ('actions-overrides-ci-false', False, 'false', 'true', '', False, False),
        ('local-all-cells', True, '', '', '', True, False),
        ('ci-all-cells', True, 'true', 'true', '', True, False),
        ('first-cell-fails', True, '', '', 'alpha', False, False),
        ('later-cell-fails', True, 'true', '', 'bravo', False, False),
        ('invalid-descriptor', True, '', '', '', False, False),
        ('empty-inventory', True, '', '', '', False, False),
    ]
    for name, installed, ci, actions, fail_cell, success, skipped in cases:
        case = root / name
        case.mkdir()
        temp = case / 'tmp'
        temp.mkdir()
        log = case / 'calls'
        # Deliberately use three descriptor cells unrelated to the real fleet.
        config.write_text(json.dumps({'cells': {'charlie': {}, 'alpha': {}, 'bravo': {}}}))
        if name == 'invalid-descriptor':
            config.write_text('invalid json')
        elif name == 'empty-inventory':
            config.write_text('{"cells": {}}')
        env = dict(os.environ, PATH=str(available if installed else absent),
                   CI=ci, GITHUB_ACTIONS=actions, TMPDIR=str(temp),
                   CHECK_CONFIG=str(config), CHECK_LOG=str(log), CHECK_FAIL_CELL=fail_cell)
        result = subprocess.run([bash, str(scripts / 'check-postgres-image-mirror.sh')],
                                env=env, capture_output=True, text=True)
        assert (result.returncode == 0) == success, f'{name}: unexpected exit {result.returncode}'
        output = result.stdout + result.stderr
        assert ('skipped: no skopeo' in output) == skipped, f'{name}: incorrect skip behavior'
        if not installed and not skipped:
            assert 'skopeo is required in CI' in output, f'{name}: missing CI diagnostic'
        expected = []
        if installed and name not in ('invalid-descriptor', 'empty-inventory'):
            expected = ['alpha', 'bravo', 'charlie']
            if fail_cell:
                expected = expected[:expected.index(fail_cell) + 1]
        calls = log.read_text().splitlines() if log.exists() else []
        assert calls == expected, f'{name}: wrong cell dispatch or failure boundary'
        assert not list(temp.iterdir()), f'{name}: temporary image copies leaked'
        assert ('check passed' in output) == (installed and success), f'{name}: incorrect success claim'
    print(f'postgres image mirror check tests passed ({len(cases)} synthetic cases)')
PY
