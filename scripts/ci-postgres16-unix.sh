#!/usr/bin/env bash
set -euo pipefail

fail() {
  printf 'PostgreSQL proof: %s\n' "$1" >&2
  exit 1
}

[[ $# == 1 ]] || fail 'expected platform or --cleanup'
[[ -n ${RUNNER_TEMP:-} && $RUNNER_TEMP == /* && -d $RUNNER_TEMP ]] \
  || fail 'requires an existing absolute runner temp directory'
runner_temp=$(cd -- "$RUNNER_TEMP" && pwd -P)
[[ $runner_temp != / && $runner_temp != *$'\n'* && $runner_temp != *$'\r'* ]] \
  || fail 'runner temp directory is invalid'
proof_root="$runner_temp/lc-pg16"
if [[ $1 == --cleanup ]]; then
  rm -rf -- "$proof_root"
  [[ ! -e $proof_root && ! -L $proof_root ]] || fail 'cleanup failed'
  exit 0
fi

case "$1:$(uname -s):$(uname -m)" in
  linux-x64:Linux:x86_64|linux-arm64:Linux:aarch64|macos-arm64:Darwin:arm64|macos-intel:Darwin:x86_64) ;;
  *) fail 'platform does not match the native runner' ;;
esac
[[ -n ${GITHUB_ENV:-} && -f $GITHUB_ENV && -w $GITHUB_ENV ]] \
  || fail 'requires a writable GitHub environment file'
[[ ! -e $proof_root && ! -L $proof_root ]] || fail 'temp tree already exists'
script_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
pin_values=$(python3 - "$script_dir/ci-postgres16-unix.json" "$1" <<'PY'
import json
import re
import sys

try:
    pin = json.load(open(sys.argv[1], encoding="utf-8"))[sys.argv[2]]
    version = pin["version"]
    url = pin["url"]
    digest = pin["sha256"]
    if (pin["major"] != 16 or not re.fullmatch(r"16\.[0-9]+", version)
            or not re.fullmatch(r"[a-f0-9]{64}", digest)
            or url != ("https://apt-archive.postgresql.org/pub/repos/apt/pool/main/"
                       f"p/postgresql-16/postgresql-16_{version}.orig.tar.bz2")):
        raise ValueError("invalid pin")
except (OSError, ValueError, KeyError, TypeError):
    sys.exit("PostgreSQL proof: invalid pin")
print("\t".join((version, url, digest)))
PY
)
IFS=$'\t' read -r pg_version archive_url expected_digest <<< "$pin_values"
mkdir -m 700 -- "$proof_root"
mkdir -m 700 -- "$proof_root/testtmp"
archive_path="$proof_root/postgresql.tar.bz2"
curl --fail --silent --show-error --proto '=https' --tlsv1.2 \
  --connect-timeout 20 --max-time 180 --retry 2 \
  "$archive_url" --output "$archive_path"
actual_digest=$(shasum -a 256 "$archive_path")
if [[ ${actual_digest%% *} != "$expected_digest" ]]; then
  fail 'archive checksum mismatch'
fi

# PGDG's source package contains the full server and client toolchain. The
# Zonky binary JARs omit psql/createdb. Build one consistent runtime instead of
# mixing their bundled libraries with clients from an unrelated installation.
# No package manager, system prefix, service, or database initialization is used.
tar -xjf "$archive_path" -C "$proof_root"
source_dir="$proof_root/postgresql-$pg_version"
[[ -d $source_dir ]] || fail 'source tree is incomplete'
runtime_dir="$proof_root/pgsql"
(
  cd -- "$source_dir"
  # Keep optional host packages out of the proof's runtime dependencies.
  # Capture compiler/configure output so arbitrary build values are not logged.
  ./configure --prefix="$runtime_dir" --without-icu --without-readline --without-zlib \
    > "$proof_root/build.log" 2>&1 || fail 'configure failed (runner build.log)'
  make -j2 >> "$proof_root/build.log" 2>&1 || fail 'build failed (runner build.log)'
  make install >> "$proof_root/build.log" 2>&1 || fail 'staging failed (runner build.log)'
)
for directory in bin lib share; do
  [[ -d $runtime_dir/$directory ]] || fail 'runtime tree is incomplete'
done
version_pattern='\(PostgreSQL\) 16\.[0-9]+($|[[:space:]])'
for name in postgres initdb pg_ctl psql createdb; do
  executable="$runtime_dir/bin/$name"
  [[ -f $executable && -x $executable ]] || fail 'runtime executable is missing'
  version=$("$executable" --version) || fail 'runtime executable failed'
  [[ $version =~ $version_pattern ]] || fail 'runtime executable did not report major sixteen'
done
# Publish only after the entire toolchain passes preflight. The next workflow
# step owns lifecycle activation; provisioning itself never starts PostgreSQL.
printf 'WITSELF_LOCALCELL_PGBIN=%s/bin\n' "$runtime_dir" >> "$GITHUB_ENV"
printf 'Pinned PostgreSQL sixteen runtime verified\n'
