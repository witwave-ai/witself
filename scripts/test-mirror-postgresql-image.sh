#!/usr/bin/env bash
set -euo pipefail

source_root=$(CDPATH='' cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
work_dir=$(mktemp -d "${TMPDIR:-/tmp}/witself-postgresql-mirror-test.XXXXXX")
trap 'rm -rf -- "$work_dir"' EXIT

# Registry operations are synthetic. The fake records complete argv in the
# temporary directory; neither these fixtures nor failures print digest values.
python3 - "$source_root" "$work_dir" <<'PY'
import copy
import hashlib
import json
import os
from pathlib import Path
import re
import subprocess
import sys

root, work = map(Path, sys.argv[1:])
descriptor = json.loads((root / 'images/postgresql/mirror.json').read_text())
cells = sorted(descriptor['cells'])
assert cells, 'mirror descriptor must enumerate cells'
for cell in cells:
    for source in [f'internal/gitopsvalues/overlays/{cell}.yaml.tmpl', f'.gitops/cells/{cell}/values.yaml']:
        current = (root / source).read_text()
        pin = re.search(r'(?m)^      digest: (sha256:[0-9a-f]{64})$', current)
        assert pin and pin[1] == descriptor['cells'][cell]['digest'], 'reviewed upstream pin drifted from existing cell'
    assert re.fullmatch(r'[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}', descriptor['cells'][cell]['upstream_tag']), 'missing upstream tag provenance'
    upstream_version = descriptor['cells'][cell]['upstream_version']
    assert upstream_version is None or (isinstance(upstream_version, str) and len(upstream_version) <= 128
        and re.fullmatch(r'[0-9]+[.][0-9]+(?:[.][0-9]+)?(?:-[A-Za-z0-9_.-]+)?', upstream_version)), 'invalid upstream version metadata'

bin_dir = work / 'bin'
bin_dir.mkdir()
fake = bin_dir / 'skopeo'
fake.write_text('''#!/usr/bin/env python3
import json, os, pathlib, sys
args = sys.argv[1:]
with open(os.environ['MIRROR_TEST_CALLS'], 'a') as calls:
    calls.write(json.dumps(args) + '\\n')
mode = os.environ.get('MIRROR_TEST_FAILURE', '')
source = pathlib.Path(os.environ['MIRROR_TEST_SOURCE'])
destination = pathlib.Path(os.environ['MIRROR_TEST_DESTINATION'])
if args == ['--version']:
    if mode == 'version-unavailable':
        sys.exit('synthetic version failure')
    print('skopeo version ' + os.environ.get('MIRROR_TEST_VERSION', '1.22.0'))
elif args[:2] == ['inspect', '--raw'] and len(args) == 3:
    upstream = args[2].startswith('docker://registry-1.docker.io/bitnami/postgresql@sha256:')
    existing = args[2].startswith('docker://ghcr.io/witwave-ai/images/postgresql@sha256:')
    if existing:
        mirror_errors = {'mirror-unavailable': 'too many requests',
                         'mirror-auth-failed': 'unauthorized',
                         'mirror-forbidden': '403 Forbidden',
                         'mirror-denied': 'denied: requested access to the resource is denied'}
        if mode in mirror_errors:
            sys.exit(mirror_errors[mode])
        if os.environ['MIRROR_TEST_EXISTS'] != 'true':
            sys.exit('manifest unknown: requested digest does not exist')
        sys.stdout.buffer.write(source.read_bytes() + (b'\\n' if mode == 'mirror-mismatch' else b''))
    else:
        if mode == ('source-unavailable' if upstream else 'destination-unavailable'):
            sys.exit('synthetic source failure' if upstream else 'synthetic destination failure')
        sys.stdout.buffer.write((source if upstream else destination).read_bytes())
    print('synthetic inspect diagnostic sha256:' + 'a' * 64, file=sys.stderr)
elif args[:5] == ['copy', '--all', '--preserve-digests', '--retry-times', '3'] and len(args) == 7:
    if mode == 'copy-failed':
        sys.exit('synthetic copy failure sha256:' + 'b' * 64)
    destination.write_bytes(source.read_bytes() + (b'\\n' if mode == 'destination-mismatch' else b''))
    print('synthetic copy diagnostic', file=sys.stderr)
else:
    sys.exit('unexpected fake registry command')
''')
fake.chmod(0o755)

def content_digest(raw):
    return 'sha256:' + hashlib.sha256(raw).hexdigest()

def manifest(media_type):
    return json.dumps({'schemaVersion': 2, 'mediaType': media_type,
                       'config': {'digest': 'sha256:' + 'a' * 64}, 'layers': []}).encode()

single = manifest('application/vnd.oci.image.manifest.v1+json')
docker_single = manifest('application/vnd.docker.distribution.manifest.v2+json')
index = json.dumps({'schemaVersion': 2, 'mediaType': 'application/vnd.oci.image.index.v1+json',
                    'manifests': [{'digest': content_digest(single), 'platform': {'os': 'linux', 'architecture': arch}}
                                  for arch in ['amd64', 'arm64']]}).encode()
default_config = copy.deepcopy(descriptor)
default_config['cells'][cells[0]]['digest'] = content_digest(index)
default_config['cells'][cells[1]]['digest'] = content_digest(single)
count = 0

def run_case(name, *, cell=cells[0], raw=index, config=None, failure='',
             expect_error='', version='1.2.3', copy_expected=False, kind='index',
             exists=False, destination='', skopeo_version='1.22.0', upstream_expected=None,
             expect_logs=()):
    global count
    case = work / name
    case.mkdir()
    config = copy.deepcopy(default_config if config is None else config)
    (case / 'config.json').write_text(json.dumps(config))
    (case / 'source.json').write_bytes(raw)
    env = dict(os.environ, PATH=str(bin_dir) + os.pathsep + os.environ['PATH'],
               GITHUB_OUTPUT=str(case / 'output'), MIRROR_TEST_CALLS=str(case / 'calls'),
               MIRROR_TEST_SOURCE=str(case / 'source.json'),
               MIRROR_TEST_DESTINATION=str(case / 'destination.json'), MIRROR_TEST_FAILURE=failure,
               MIRROR_TEST_EXISTS=str(exists).lower(), MIRROR_TEST_VERSION=skopeo_version)
    command = ['bash', str(root / 'scripts/mirror-postgresql-image.sh'), version, cell, str(case / 'config.json')]
    if destination:
        command.append(destination.replace('{case}', str(case)))
    result = subprocess.run(command,
                            env=env, text=True, capture_output=True)
    calls = [json.loads(line) for line in (case / 'calls').read_text().splitlines()] if (case / 'calls').exists() else []
    copied = [args for args in calls if args[0] == 'copy']
    assert bool(copied) == copy_expected, name + ': incorrect publication boundary'
    upstream = [args for args in calls if any(arg.startswith('docker://registry-1.docker.io/') for arg in args)]
    if upstream_expected is not None:
        assert bool(upstream) == upstream_expected, name + ': incorrect upstream access'
    if exists and not destination:
        assert not upstream, name + ': existing mirror contacted Docker Hub'
    if expect_error:
        assert result.returncode == 1 and expect_error in result.stderr, name + ': wrong failure'
        assert not (case / 'output').exists(), name + ': exposed successful publication output on failure'
        assert result.stderr.startswith('PostgreSQL mirror: ' + expect_error), name + ': diagnostic obscured original failure'
        for stage, message in expect_logs:
            assert f'PostgreSQL mirror: skopeo {stage} log:\n{message}' in result.stderr, name + ': missing ' + stage + ' diagnostic'
    else:
        assert result.returncode == 0, name + ': mirror failed'
        assert not result.stderr, name + ': successful mirror echoed captured diagnostics'
        outputs = dict(line.split('=', 1) for line in (case / 'output').read_text().splitlines())
        digest = config['cells'][cell]['digest']
        image = descriptor['destination_repository']
        mirrored_new = not exists or bool(destination)
        assert outputs == dict(image=image, digest=digest, tag=f'{version}-{cell}', kind=kind,
                               mirrored_new=str(mirrored_new).lower()), name + ': wrong outputs'
        source_ref = (f'docker://registry-1.docker.io/bitnami/postgresql@{digest}' if mirrored_new
                      else f'docker://{image}@{digest}')
        destination_ref = destination.replace('{case}', str(case)) if destination else f'docker://{image}:{version}-{cell}'
        assert copied == [['copy', '--all', '--preserve-digests', '--retry-times', '3',
                           source_ref, destination_ref]], name + ': changed copy semantics'
        assert (case / 'destination.json').read_bytes() == raw, name + ': modified manifest bytes'
    assert not re.search(r'[0-9a-f]{64}', result.stdout + result.stderr), name + ': printed digest'
    count += 1

run_case('oci-index-all-platforms', copy_expected=True)
run_case('second-cell-single-platform', cell=cells[1], raw=single, copy_expected=True, kind='manifest')
config = copy.deepcopy(default_config)
config['cells'][cells[0]]['digest'] = content_digest(docker_single)
run_case('docker-single-platform', raw=docker_single, config=config, copy_expected=True, kind='manifest')
docker_index = index.replace(b'application/vnd.oci.image.index.v1+json', b'application/vnd.docker.distribution.manifest.list.v2+json')
config['cells'][cells[0]]['digest'] = content_digest(docker_index)
run_case('docker-index-all-platforms', raw=docker_index, config=config, copy_expected=True)
run_case('existing-mirror-retag', exists=True, copy_expected=True)
run_case('existing-mirror-hub-unavailable', exists=True, failure='source-unavailable', copy_expected=True)
run_case('existing-mirror-mismatch', exists=True, failure='mirror-mismatch', expect_error='existing mirror manifest digest mismatch')
for failure, diagnostic in [('mirror-unavailable', 'too many requests'),
                            ('mirror-auth-failed', 'unauthorized'),
                            ('mirror-forbidden', '403 Forbidden'),
                            ('mirror-denied', 'denied: requested access to the resource is denied')]:
    run_case(failure, failure=failure, upstream_expected=False, expect_error='could not determine whether',
             expect_logs=[('source', diagnostic)])
run_case('dir-transport-upstream-tripwire', exists=True, destination='dir:{case}/mirror', copy_expected=True, upstream_expected=True)
run_case('oci-transport-upstream-tripwire', destination='oci:{case}/mirror:pin', copy_expected=True, upstream_expected=True)
run_case('local-destination-mismatch', destination='dir:{case}/mirror', failure='destination-mismatch', copy_expected=True, expect_error='published manifest digest mismatch')
run_case('local-upstream-unavailable', exists=True, destination='dir:{case}/mirror', failure='source-unavailable', expect_error='could not read the digest-pinned upstream')
run_case('reject-docker-destination', destination='docker://unreviewed.invalid/test', upstream_expected=False, expect_error='destination override must')
run_case('reject-relative-destination', destination='dir:relative', upstream_expected=False, expect_error='destination override must')
run_case('skopeo-1-5-minimum', skopeo_version='1.5.0', copy_expected=True)
run_case('skopeo-too-old', skopeo_version='1.4.1', upstream_expected=False, expect_error='skopeo 1.5 or newer')
run_case('skopeo-unparseable-version', skopeo_version='unknown', upstream_expected=False, expect_error='skopeo 1.5 or newer')
run_case('skopeo-version-unavailable', failure='version-unavailable', upstream_expected=False, expect_error='could not determine skopeo version',
         expect_logs=[('version', 'synthetic version failure')])
run_case('source-unavailable', failure='source-unavailable', expect_error='could not read the digest-pinned upstream',
         expect_logs=[('source', 'synthetic source failure')])
source_diagnostic = ('source', 'synthetic inspect diagnostic sha256:[redacted-digest]')
copy_diagnostic = ('copy', 'synthetic copy diagnostic')
run_case('source-mismatch', raw=index + b'\n', expect_error='upstream manifest digest mismatch', expect_logs=[source_diagnostic])
run_case('copy-failed', failure='copy-failed', copy_expected=True, expect_error='copy failed while preserving',
         expect_logs=[source_diagnostic, ('copy', 'synthetic copy failure sha256:[redacted-digest]')])
run_case('destination-unavailable', failure='destination-unavailable', copy_expected=True, expect_error='could not read the published',
         expect_logs=[source_diagnostic, copy_diagnostic, ('destination', 'synthetic destination failure')])
run_case('destination-mismatch', failure='destination-mismatch', copy_expected=True, expect_error='published manifest digest mismatch',
         expect_logs=[source_diagnostic, copy_diagnostic, ('destination', 'synthetic inspect diagnostic sha256:[redacted-digest]')])
run_case('unknown-cell', cell='unlisted-cell', expect_error='unsupported cell')
run_case('invalid-version', version='latest', expect_error='invalid release version')
for field, value in [('schema_version', 2), ('source_registry', 'unreviewed.invalid'),
                     ('source_repository', 'bitnami/postgresql:latest'),
                     ('destination_repository', 'ghcr.io/unreviewed/postgresql')]:
    config = copy.deepcopy(default_config)
    config[field] = value
    run_case('invalid-' + field, config=config, expect_error='invalid source descriptor')
config = copy.deepcopy(default_config)
config['cells'][cells[1]]['digest'] = 'latest'
run_case('malformed-other-cell-pin', config=config, expect_error='invalid source descriptor')
config = copy.deepcopy(default_config)
config['cells'] = {}
run_case('empty-cell-descriptor', config=config, expect_error='invalid source descriptor')
config = copy.deepcopy(default_config)
config['cells'] = {cells[0]: config['cells'][cells[0]]}
run_case('single-descriptor-cell', config=config, copy_expected=True)
config['cells']['future-cell'] = copy.deepcopy(config['cells'][cells[0]])
run_case('descriptor-driven-future-cell', cell='future-cell', config=config, copy_expected=True)
config['cells']['invalid/name'] = copy.deepcopy(config['cells'][cells[0]])
run_case('invalid-cell-name', config=config, expect_error='invalid source descriptor')
for tag in ['', None, '-invalid', '16.6.0/debian-12-r2', 'x' * 129]:
    config = copy.deepcopy(default_config)
    config['cells'][cells[1]]['upstream_tag'] = tag
    run_case('invalid-upstream-tag-' + str(count), config=config, expect_error='invalid source descriptor')
config = copy.deepcopy(default_config)
del config['cells'][cells[1]]['upstream_tag']
run_case('missing-upstream-tag', config=config, expect_error='invalid source descriptor')
config = copy.deepcopy(default_config)
del config['cells'][cells[1]]['upstream_version']
run_case('missing-upstream-version', config=config, expect_error='invalid source descriptor')
for version_value in [18, {}, [], False, '', 'latest', '18.8.0/debian-12-r0', '18.8.0-' + 'x' * 129]:
    config = copy.deepcopy(default_config)
    config['cells'][cells[1]]['upstream_version'] = version_value
    run_case('invalid-upstream-version-' + str(count), config=config, expect_error='invalid source descriptor')
for version_value in [None, '18.8', '18.8.0-debian-12-r0']:
    config = copy.deepcopy(default_config)
    config['cells'][cells[1]]['upstream_version'] = version_value
    run_case('valid-upstream-version-' + str(count), config=config, copy_expected=True)
for name, raw in [('unsupported-manifest', b'{"schemaVersion": 1}'),
                  ('empty-index', b'{"schemaVersion": 2, "mediaType": "application/vnd.oci.image.index.v1+json", "manifests": []}')]:
    config = copy.deepcopy(default_config)
    config['cells'][cells[0]]['digest'] = content_digest(raw)
    run_case(name, config=config, raw=raw, expect_error='unsupported upstream manifest format')
print(f'PostgreSQL mirror synthetic registry checks passed ({count} cases)')
PY

ruby - "$source_root" <<'RUBY'
require 'yaml'
root = ARGV.fetch(0)
workflow = YAML.load_file(File.join(root, '.github/workflows/release.yml'))
jobs = workflow.fetch('jobs')
mirror = jobs.fetch('mirror-postgresql')
gates = %w[go verify provider-integration-verify provider-contract-evidence dashboard-acceptance homebrew-verify]
raise 'mirror can publish before a release gate' unless mirror.fetch('needs').sort == (gates + ['postgres-mirror-cells']).sort
raise 'mirror failures can block GoReleaser' unless jobs.fetch('goreleaser').fetch('needs').sort == gates.sort
raise 'snapshot cannot reach GoReleaser' if mirror.key?('if')
raise 'mirror matrix ignores descriptor cells' unless mirror.fetch('strategy').fetch('matrix').fetch('cell') == '${{ fromJSON(needs.postgres-mirror-cells.outputs.cells) }}'
cell_job = jobs.fetch('postgres-mirror-cells')
cell_step = cell_job.fetch('steps').find { |step| step['id'] == 'cells' }
raise 'mirror cell discovery bypasses descriptor' unless cell_step.fetch('run').include?("jq -ce '.cells | keys | select(length > 0)' images/postgresql/mirror.json")
raise 'mirror cell discovery output missing' unless cell_job.fetch('outputs').fetch('cells') == '${{ steps.cells.outputs.cells }}'
steps = mirror.fetch('steps')
steps.drop(1).each do |step|
  raise 'snapshot can publish a mirror' unless step['if'] == "github.event_name == 'push'"
end
copy = steps.find { |step| step['id'] == 'mirror' }.fetch('run')
raise 'workflow bypasses tested mirror helper' unless copy.include?('bash scripts/mirror-postgresql-image.sh "${GITHUB_REF_NAME#v}" "$MIRROR_CELL"')
sign = steps.find { |step| step['name'] == 'Sign PostgreSQL mirror and attest SBOM' }.fetch('run')
raise 'signing does not address the verified digest' unless sign.include?('image="$MIRROR_IMAGE@$MIRROR_DIGEST"') && sign.include?('cosign sign --yes "$image"')
raise 'SBOM attestation missing' unless sign.include?('syft "$image"') && sign.include?('cosign attest --yes --type https://spdx.dev/Document')
attest = steps.find { |step| step['name'] == 'Attest PostgreSQL mirror provenance' }
raise 'mirror provenance action missing' unless attest.fetch('uses').start_with?('actions/attest-build-provenance@')
expected = {'subject-name' => '${{ steps.mirror.outputs.image }}', 'subject-digest' => '${{ steps.mirror.outputs.digest }}', 'push-to-registry' => true}
raise 'provenance does not bind the verified registry subject' unless attest.fetch('with') == expected
ci = YAML.load_file(File.join(root, '.github/workflows/ci.yml')).fetch('jobs')
tripwire = ci.fetch('postgres-image-mirror')
raise 'mirror tripwire must run on release runner OS' unless tripwire.fetch('runs-on') == 'ubuntu-latest'
raise 'PR mirror tripwire can be skipped' if tripwire.key?('if')
tripwire_steps = tripwire.fetch('steps')
install = tripwire_steps.find { |step| step['name'] == 'Install digest-preserving registry copier' }.fetch('run')
raise 'tripwire does not check runner package installation' unless install.include?('sudo apt-get update') && install.include?('sudo apt-get install --yes skopeo')
check = tripwire_steps.find { |step| step['name'] == 'Verify PostgreSQL upstream copies before tagging' }
raise 'tripwire diverges from local check target' unless check.fetch('run').strip == 'make check-postgres-image-mirror'
raise 'CI tripwire may skip when skopeo is missing' unless check.fetch('env').fetch('CI') == 'true'
raise 'CI tripwire step can be skipped' if check.key?('if')
raise 'tripwire cannot block required CI gate' unless ci.fetch('go').fetch('needs').include?('postgres-image-mirror')
aggregate = ci.fetch('go').fetch('steps').find { |step| step['name'] == 'Require all Go gates' }
raise 'required gate does not reject failed tripwire' unless aggregate.fetch('env').fetch('POSTGRES_MIRROR_RESULT') == '${{ needs.postgres-image-mirror.result }}' && aggregate.fetch('run').include?('"$POSTGRES_MIRROR_RESULT"')
puts 'PostgreSQL mirror release independence, PR tripwire, signing, and provenance checks passed'
RUBY
