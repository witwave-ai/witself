#!/bin/bash
# Image contracts use stub scanners, dead proxies, and sample-derived JSON only.
set -euo pipefail
SOURCE_ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)
SCRIPT=$SOURCE_ROOT/scripts/security-scan.sh
FIXTURES=$SOURCE_ROOT/scripts/testdata/security-scan
REAL_RUBY=$(command -v ruby)
REAL_JQ=$(command -v jq)
REAL_TAR=$(command -v tar)
SCAN_BASH=$(command -v bash)
if [ "$(/bin/bash -c 'printf "%s" "${BASH_VERSINFO[0]}"')" = 3 ]; then SCAN_BASH=/bin/bash; fi
SUITE=$(mktemp -d "${TMPDIR:-/tmp}/witself-security-scan-images-test.XXXXXX")
SUITE=$(cd "$SUITE" && pwd -P)
trap 'rm -rf -- "$SUITE"' EXIT
TOOLS=$SUITE/tools
mkdir -p "$TOOLS"
for tool in dirname mktemp rm cat mkdir sed awk tr sort cut date find wc env jq git ruby bash; do ln -s "$(command -v "$tool")" "$TOOLS/$tool"; done
if command -v sha256sum >/dev/null 2>&1; then
  REAL_SHA=$(command -v sha256sum)
  ln -s "$REAL_SHA" "$TOOLS/sha256sum"
else
  REAL_SHA=$(command -v shasum)
  ln -s "$REAL_SHA" "$TOOLS/shasum"
fi
# GNU tar decompresses -z through a gzip child process (bsdtar on macOS does it
# in-process), so the curated PATH must carry gzip for the download extraction.
if command -v gzip >/dev/null 2>&1; then ln -s "$(command -v gzip)" "$TOOLS/gzip"; fi
CASE=
ROOT=
REPORT=
CODE=0
CASES=0
ENV_LINES=0
TEST=setup
fail() { printf 'security scan images: FAIL (%s: %s)\n' "$TEST" "$1" >&2; exit 1; }
selected() {
  case ",${SECURITY_SCAN_IMAGE_TEST_CASES:-all}," in
    *,all,*|*,"$1",*) return 0 ;;
    *) return 1 ;;
  esac
}
new_case() {
  local tool
  CASE=$SUITE/case-$CASES
  CASES=$((CASES + 1))
  ROOT=$CASE/root
  REPORT=$CASE/report.md
  SCRIPT=$SOURCE_ROOT/scripts/security-scan.sh
  mkdir -p "$ROOT" "$CASE/bin" "$CASE/tmp"
  cp "$FIXTURES"/bin/* "$CASE/bin/"
  for tool in gh curl uname grype; do cp "$FIXTURES/bin/image-$tool" "$CASE/bin/$tool"; done
  cat > "$CASE/bin/tar" <<'STUB'
#!/bin/bash
set -eu
printf '%s|%s\n' "$PWD" "$*" >> "$SECURITY_SCAN_CASE/tar.argv"
[ "$#" -eq 5 ] && [ "$1" = -xzf ] && [ "$3" = -C ] && [ "$5" = grype ] || exit 97
case "$2" in "$SECURITY_SCAN_CASE"/tmp/witself-security-scan.*/grype_*.tar.gz) ;; *) exit 97 ;; esac
[ "${2%/*}" = "$4" ] || exit 97
exec "${SECURITY_SCAN_REAL_TAR:?}" "$@"
STUB
  chmod +x "$CASE/bin/"*
}
values() {
  # All digests are invented test data, constructed without console output.
  "$REAL_RUBY" -E UTF-8:UTF-8 -ryaml - "$ROOT" "${1:-same}" <<'RUBY'
root, mode=ARGV
base='ghcr.io/witwave-ai/images/'
server={'enabled'=>true,'imageTag'=>'0.0.333','imageDigest'=>'sha256:'+'a'*64}
backup={'enabled'=>true,'image'=>{'repository'=>base+'witself-postgres-backup','tag'=>'0.0.292','digest'=>'sha256:'+'b'*64}}
serving={'apps'=>{'witselfServer'=>server,'civoPostgres'=>{'backup'=>backup}}}
other=Marshal.load(Marshal.dump(serving))
other['apps']['civoPostgres']['backup']['enabled']=false
# A distinct ref prevents dedupe from hiding accidental inclusion of a disabled backup.
other['apps']['civoPostgres']['backup']['image'].merge!('tag'=>'0.0.291','digest'=>'sha256:'+'c'*64)
case mode
when 'different' then other['apps']['witselfServer']['imageDigest']='sha256:'+'c'*64
when 'tag-only' then other['apps']['witselfServer'].delete('imageDigest')
when 'disabled' then other['apps']['witselfServer']['enabled']=false
when 'backup-missing' then serving['apps']['civoPostgres']['backup'].delete('image')
when 'backup-string' then serving['apps']['civoPostgres']['backup']['image']=base+'witself-postgres-backup:0.0.292'
when 'backup-invalid' then serving['apps']['civoPostgres']['backup']['image']='malformed-image'
when 'release-only'
  [serving,other].each { |v| v['apps']['witselfServer']['enabled']=false; v['apps']['civoPostgres']['backup']['enabled']=false }
end
{'civo-sandbox-use1-serving'=>serving,'civo-sandbox-use1-backup'=>other}.each do |cell,value|
  dir=File.join(root,'.gitops/cells',cell)
  require 'fileutils'; FileUtils.mkdir_p(dir)
  File.write(File.join(dir,'values.yaml'),YAML.dump(value))
end
RUBY
}
# Disable RubyGems startup so macOS Ruby does not need uname before the scanner
# can classify an absent uname; YAML/JSON use only the standard library.
invoke_scan() {
  env PATH="$CASE/bin:$TOOLS" \
    http_proxy=http://127.0.0.1:9 https_proxy=http://127.0.0.1:9 \
    HTTP_PROXY=http://127.0.0.1:9 HTTPS_PROXY=http://127.0.0.1:9 \
    GOPROXY=off npm_config_offline=true RUBYOPT=--disable-gems GH_TOKEN=witself-test-token-value \
    SECURITY_SCAN_REAL_TAR="$REAL_TAR" TMPDIR="$CASE/tmp" SECURITY_SCAN_CASE="$CASE" SECURITY_SCAN_FIXTURES="$FIXTURES" \
    "$SCAN_BASH" "$SCRIPT" "$@"
}
check_cleanup() {
  if [ -f "$CASE/env.log" ]; then
    "$REAL_RUBY" -E UTF-8:UTF-8 -e 'exit(File.readlines(ARGV[0], chomp:true, encoding:"UTF-8").all? { |s| s=="off|http://127.0.0.1:9|true" } ? 0 : 1)' "$CASE/env.log" || fail 'offline environment changed'
    ENV_LINES=$((ENV_LINES + $(wc -l < "$CASE/env.log")))
  fi
  [ ! -s "$CASE/unexpected.argv" ] || fail 'unexpected tool invoked'
  [ -z "$(find "$CASE/tmp" -mindepth 1 -print -quit)" ] || fail 'private temporary directory was not removed'
}
run_scan() {
  CODE=0
  invoke_scan "$@" > "$CASE/stdout" 2> "$CASE/stderr" || CODE=$?
  check_cleanup
}
scan_images() { run_scan images --root "$ROOT" --release-version 0.0.333 --grype "$CASE/bin/grype" --report "$REPORT"; }
expect_code() { [ "$CODE" -eq "$1" ] || fail "exit $CODE, expected $1"; }
contains() { "$REAL_RUBY" -E UTF-8:UTF-8 -e 'exit(File.read(ARGV[0],encoding:"UTF-8").include?(ARGV[1]) ? 0 : 1)' "$1" "$2" || fail "$3"; }
absent() { "$REAL_RUBY" -E UTF-8:UTF-8 -e 'exit(File.read(ARGV[0],encoding:"UTF-8").include?(ARGV[1]) ? 1 : 0)' "$1" "$2" || fail "$3"; }
count_lines() { [ "$(wc -l < "$1")" -eq "$2" ] || fail "$3"; }
hygiene() {
  "$REAL_RUBY" -E UTF-8:UTF-8 - "$CASE" "$@" <<'RUBY' || fail 'digest, path, or token leaked'
root=ARGV.shift
ARGV.each do |path|
  text=File.read(path,encoding:"UTF-8")
  abort 'hygiene' if text.match?(/[a-fA-F0-9]{64}/) || text.include?(root) || text.include?('witself-test-token-value')
end
RUBY
}
console() {
  [ ! -s "$CASE/stdout" ] || fail 'report run wrote stdout'
  "$REAL_RUBY" -E UTF-8:UTF-8 -e 's=File.read(ARGV[0],encoding:"UTF-8"); exit(/\Asecurity-scan: status=#{ARGV[1]} go=\d+ npm_prod=\d+ npm_toolchain=\d+ images=\d+ incomplete=\d+\n\z/.match?(s) && !s.match?(/GO-|GHSA-|CVE-/) ? 0 : 1)' "$CASE/stderr" "$1" || fail 'summary leaked detail or extra output'
  hygiene "$CASE/stdout" "$CASE/stderr" "$REPORT"
}
map_fixture() {
  printf '%s\t%s\t%s\n' "$1" "$FIXTURES/images/$2.json" "${3:-0}" >> "$CASE/grype-map.tsv"
}
map_pinned_fixture() {
  "$REAL_RUBY" -E UTF-8:UTF-8 - "$CASE/grype-map.tsv" "$FIXTURES/images/$1.json" <<'RUBY'
File.write(ARGV[0],"registry:ghcr.io/witwave-ai/images/witself-postgres-backup@sha256:"+'b'*64+"\t#{ARGV[1]}\t0\n")
RUBY
}
reject_path_grype() { cp "$FIXTURES/bin/grype" "$CASE/bin/grype"; }
exact_grype_targets() {
  "$REAL_RUBY" -E UTF-8:UTF-8 - "$CASE/grype.argv" <<'RUBY' || fail 'exact digest-pinned Grype target arguments'
base='registry:ghcr.io/witwave-ai/images/'
refs=[base+'witself-server@sha256:'+'a'*64,base+'witself-postgres-backup@sha256:'+'b'*64,base+'witself-postgres-backup:0.0.333']
expected=refs.map { |ref| ref+' --platform linux/amd64 --output json --quiet' }.sort
actual=File.readlines(ARGV[0],chomp:true,encoding:'UTF-8').map { |line| line.split('|',2).last }.sort
abort 'target arguments' unless actual==expected
RUBY
}
downloaded_grype_contract() {
  "$REAL_RUBY" -E UTF-8:UTF-8 - "$CASE" <<'RUBY' || fail 'downloaded executable or extraction path'
root=ARGV[0]
paths=File.readlines(File.join(root,'grype-executable.log'),chomp:true,encoding:'UTF-8')
abort 'executable count' unless paths.length==3 && paths.uniq.length==1
abort 'executable path' unless paths.all? { |path| path.match?(Regexp.new('\\A'+Regexp.escape(root+'/tmp/witself-security-scan.')+'[^/]+/grype\\z')) }
args=File.readlines(File.join(root,'tar.argv'),chomp:true,encoding:'UTF-8').map { |line| line.split('|',2).last }
directory=File.dirname(paths.first)
abort 'extraction count' unless args.length==1
abort 'extraction arguments' unless args.first.match?(Regexp.new('\\A-xzf '+Regexp.escape(directory)+'/grype_[0-9.]+_(linux_amd64|darwin_arm64)\\.tar\\.gz -C '+Regexp.escape(directory)+' grype\\z'))
RUBY
}
coverage() {
  env PATH="$CASE/bin:$TOOLS" GOPROXY=off npm_config_offline=true RUBYOPT=--disable-gems \
    http_proxy=http://127.0.0.1:9 https_proxy=http://127.0.0.1:9 HTTP_PROXY=http://127.0.0.1:9 HTTPS_PROXY=http://127.0.0.1:9 \
    SECURITY_SCAN_CASE="$CASE" SECURITY_SCAN_FIXTURES="$FIXTURES" TMPDIR="$CASE/tmp" \
    "$REAL_RUBY" -E UTF-8:UTF-8 "$FIXTURES/targets-coverage.rb" "$1" 0.0.333
}
grype_contract() {
  "$REAL_RUBY" -E UTF-8:UTF-8 - "$CASE/grype.argv" "$CASE/grype-env.log" "$CASE/tmp" "${1:-}" <<'RUBY' || fail 'Grype arguments or environment'
argv,environment,tmp,cache=ARGV
lines=File.readlines(argv,chomp:true,encoding:"UTF-8")
abort 'no calls' if lines.empty?
abort 'arguments' unless lines.all? { |line| line.split('|',2).last.match?(/\Aregistry:[^ ]+ --platform linux\/amd64 --output json --quiet\z/) }
File.readlines(environment,chomp:true,encoding:"UTF-8").each do |line|
  update,dir=line.split('|',2)
  abort 'update' unless update=='false'
  abort 'cache' unless cache.empty? ? dir.start_with?(tmp+'/witself-security-scan.') : dir==cache
end
RUBY
}
archive() {
  mkdir -p "$CASE/archive"
  cp "$FIXTURES/bin/image-grype" "$CASE/archive/grype"
  chmod +x "$CASE/archive/grype"
  "$REAL_TAR" -czf "$CASE/archive.tar.gz" -C "$CASE/archive" grype
}
copy_scanner() {
  mkdir -p "$CASE/copied/scripts"
  cp "$SOURCE_ROOT/scripts/security-scan.sh" "$SOURCE_ROOT/scripts/security-scan-grype.env" "$CASE/copied/scripts/"
  SCRIPT=$CASE/copied/scripts/security-scan.sh
}
matching_pin() {
  # Rewrite only a disposable copy; no production checksum override exists.
  "$REAL_RUBY" -E UTF-8:UTF-8 -rdigest - "$SCRIPT" "$CASE/archive.tar.gz" "$1" <<'RUBY'
pin=File.join(File.dirname(ARGV[0]),'security-scan-grype.env')
text=File.read(pin,encoding:"UTF-8")
sum=Digest::SHA256.file(ARGV[1]).hexdigest
key='GRYPE_'+ARGV[2].upcase+'_SHA256='
other=sum=='d'*64 ? 'e'*64 : 'd'*64
text=text.gsub(/^(GRYPE_(?:LINUX_AMD64|DARWIN_ARM64)_SHA256=)[0-9a-f]{64}$/) { Regexp.last_match(1)+(Regexp.last_match(1)==key ? sum : other) }
File.write(pin,text)
RUBY
}
expected_targets() {
  # Hard-coded expected lines, independent of the script's discovery algorithm.
  local mode=${1:-same}
  local version=${2:-0.0.333}
  local base=ghcr.io/witwave-ai/images
  {
    printf 'cell:civo-sandbox-use1-serving\t%s/witself-server\t0.0.333\tsha256:aaaaaaaaaaaa\n' "$base"
    printf 'cell:civo-sandbox-use1-serving\t%s/witself-postgres-backup\t0.0.292\tsha256:bbbbbbbbbbbb\n' "$base"
    case "$mode" in
      different) printf 'cell:civo-sandbox-use1-backup\t%s/witself-server\t0.0.333\tsha256:cccccccccccc\n' "$base" ;;
      tag-only) printf 'cell:civo-sandbox-use1-backup\t%s/witself-server\t0.0.333\t-\n' "$base" ;;
    esac
    if [ "$version" != 0.0.333 ]; then printf 'release\t%s/witself-server\t%s\t-\n' "$base" "$version"; fi
    printf 'release\t%s/witself-postgres-backup\t%s\t-\n' "$base" "$version"
  } > "$CASE/expected"
}

TEST=T1i
if selected "$TEST"; then
  new_case; run_scan images --release-version 0.0; expect_code 2
  new_case; run_scan images --repo bad; expect_code 2
  new_case; run_scan images --grype; expect_code 2
  new_case; run_scan images --grype /nonexistent; expect_code 2
  new_case; run_scan targets,go; expect_code 2
  new_case; run_scan go,targets; expect_code 2
  new_case; mkdir "$CASE/executable-dir"; run_scan images --grype "$CASE/executable-dir"; expect_code 2
fi
TEST=T9
if selected "$TEST"; then
  for mode in same different tag-only disabled next-release; do
    new_case; values "$mode"
    version=0.0.333
    [ "$mode" != next-release ] || version=0.0.334
    run_scan targets --root "$ROOT" --release-version "$version"; expect_code 0
    expected_targets "$mode" "$version"
    cmp -s "$CASE/expected" "$CASE/stdout" || fail "exact targets ($mode)"
    [ ! -s "$CASE/stderr" ] || fail 'targets success wrote stderr'
    [ ! -f "$CASE/grype.argv" ] && [ ! -f "$CASE/gh.argv" ] && [ ! -f "$CASE/curl.argv" ] || fail 'targets scanned or fetched tools'
    hygiene "$CASE/stdout" "$CASE/stderr"
  done
fi
TEST=T9c
if selected "$TEST"; then
  new_case
  coverage "$SOURCE_ROOT" > "$CASE/checker.stdout" 2> "$CASE/checker.stderr" || fail 'committed targets coverage'
  contains "$CASE/checker.stdout" 'security scan targets coverage: PASS (' 'checker success summary'
  mkdir -p "$ROOT/scripts" "$ROOT/.gitops/cells"
  for cell in civo-sandbox-use1-serving civo-sandbox-use1-backup; do
    mkdir -p "$ROOT/.gitops/cells/$cell"
    cp "$SOURCE_ROOT/.gitops/cells/$cell/values.yaml" "$ROOT/.gitops/cells/$cell/values.yaml"
  done
  run_scan targets --root "$SOURCE_ROOT" --release-version 0.0.333; expect_code 0
  "$REAL_RUBY" -E UTF-8:UTF-8 - "$SCRIPT" "$CASE/stdout" "$ROOT/scripts/security-scan.sh" <<'RUBY'
line=File.readlines(ARGV[0],encoding:"UTF-8").find { |x| x.start_with?('LIVE_CELLS=') }
abort 'live cells constant missing' unless line
out=File.readlines(ARGV[1],encoding:"UTF-8")[0...-1].join
File.write(ARGV[2],"#!/bin/bash\n"+line+"cat <<'TARGETS'\n"+out+"TARGETS\n")
RUBY
  CODE=0
  coverage "$ROOT" > "$CASE/reject.stdout" 2> "$CASE/reject.stderr" || CODE=$?
  expect_code 1
  hygiene "$CASE/reject.stdout" "$CASE/reject.stderr"
fi
TEST=T9d
if selected "$TEST"; then
  for mode in backup-missing backup-string backup-invalid; do
    new_case; values "$mode"
    mkdir -p "$ROOT/scripts"
    cp "$SCRIPT" "$ROOT/scripts/security-scan.sh"
    scan_images
    if [ "$mode" = backup-string ]; then
      expect_code 0
      count_lines "$CASE/grype.argv" 3 'string backup discarded another target'
      contains "$CASE/grype.argv" 'registry:ghcr.io/witwave-ai/images/witself-postgres-backup:0.0.292 --platform linux/amd64 --output json --quiet' 'string backup target not scanned'
      contains "$REPORT" '| cell:civo-sandbox-use1-serving | ghcr.io/witwave-ai/images/witself-postgres-backup | 0.0.292 | - |' 'string backup target row'
      console clean
    else
      expect_code 3
      reason='unpinned backup image'
      [ "$mode" != backup-invalid ] || reason='invalid backup image'
      contains "$REPORT" "| cell:civo-sandbox-use1-serving backup | $reason |" 'backup image incomplete reason'
      contains "$REPORT" 'Scanned: 2 of 2.' 'bad backup discarded other targets'
      contains "$REPORT" '| cell:civo-sandbox-use1-serving | ghcr.io/witwave-ai/images/witself-server |' 'bad backup discarded same-cell server'
      count_lines "$CASE/grype.argv" 2 'bad backup stopped other target scans'
      console incomplete
      run_scan targets --root "$ROOT" --release-version 0.0.333; expect_code 3
      [ ! -s "$CASE/stdout" ] || fail 'incomplete targets returned partial stdout'
      contains "$CASE/stderr" "security-scan: targets incomplete: $reason" 'backup targets diagnostic'
      count_lines "$CASE/stderr" 1 'backup targets extra diagnostic'
    fi
    coverage "$ROOT" > "$CASE/checker.stdout" 2> "$CASE/checker.stderr" || fail 'backup shape targets coverage'
    contains "$CASE/checker.stdout" 'security scan targets coverage: PASS (' 'backup shape checker summary'
    hygiene "$CASE/checker.stdout" "$CASE/checker.stderr"
  done
fi
TEST=T10
if selected "$TEST"; then
  new_case; values release-only
  map_fixture registry:ghcr.io/witwave-ai/images/witself-postgres-backup:0.0.333 policy
  scan_images; expect_code 1
  contains "$REPORT" 'images=2 incomplete=0' 'image findings count'
  contains "$REPORT" 'Findings: 2. Informational: 2. Scanned: 2 of 2.' 'policy counts'
  contains "$REPORT" '| CVE-0000-0001 | Critical | fixture-package-1 | 1.0 | apk | 1.1,1.2 |' 'Critical fixed row'
  contains "$REPORT" '| CVE-0000-0002 | High | fixture-package-2 | 1.0 | apk | 1.1,1.2 |' 'High fixed row'
  absent "$REPORT" 'CVE-0000-0003' 'unfixed vulnerability counted'
  absent "$REPORT" 'CVE-0000-0004' 'Medium vulnerability counted'
  grype_contract
  # Caller cache is honored; app-update checks remain disabled.
  new_case; values release-only
  export GRYPE_DB_CACHE_DIR=$CASE/caller-cache
  scan_images; expect_code 0
  grype_contract "$GRYPE_DB_CACHE_DIR"
  unset GRYPE_DB_CACHE_DIR
fi
TEST=T10b
if selected "$TEST"; then
  new_case; values release-only
  map_fixture registry:ghcr.io/witwave-ai/images/witself-server:0.0.333 server-rule
  map_fixture registry:ghcr.io/witwave-ai/images/witself-postgres-backup:0.0.333 go-module
  scan_images; expect_code 1
  contains "$REPORT" 'images=2 incomplete=0' 'repository-specific rule count'
  contains "$REPORT" 'Findings: 2. Informational: 1. Scanned: 2 of 2.' 'Go match informational count'
  contains "$REPORT" '| release | ghcr.io/witwave-ai/images/witself-server:0.0.333 | CVE-0000-0011 | Critical | fixture-deb | 1.0 | deb |' 'server OS package ignored'
  contains "$REPORT" '| release | ghcr.io/witwave-ai/images/witself-postgres-backup:0.0.333 | CVE-0000-0010 | Critical | fixture-go-module | v1.0.0 | go-module |' 'backup Go module ignored'
  absent "$REPORT" '| ghcr.io/witwave-ai/images/witself-server:0.0.333 | CVE-0000-0010 |' 'server Go module became finding'
fi
TEST=T10c
if selected "$TEST"; then
  new_case; values; map_pinned_fixture policy
  scan_images; expect_code 1
  exact_grype_targets
  contains "$REPORT" '| cell:civo-sandbox-use1-serving | ghcr.io/witwave-ai/images/witself-postgres-backup:0.0.292@sha256:bbbbbbbbbbbb | CVE-0000-0001 | Critical | fixture-package-1 | 1.0 | apk | 1.1,1.2 |' 'digest-pinned finding row missing'
  contains "$REPORT" 'images=2 incomplete=0' 'digest-pinned findings count'
  console findings
fi
TEST=T11
if selected "$TEST"; then
  new_case; values
  run_scan images --root "$ROOT" --grype "$CASE/bin/grype" --report "$REPORT"; expect_code 0
  contains "$CASE/gh.argv" '|release view --repo witwave-ai/witself --json tagName --jq .tagName' 'release lookup argv'
  count_lines "$CASE/gh.argv" 1 'release looked up more than once'
  count_lines "$CASE/grype.argv" 3 'latest release target count'
  exact_grype_targets
  for mode in invalid failed; do
    new_case; values
    if [ "$mode" = invalid ]; then printf 'latest\n' > "$CASE/gh.tag"; else printf '1\n' > "$CASE/gh.exit"; fi
    run_scan images --root "$ROOT" --grype "$CASE/bin/grype" --report "$REPORT"; expect_code 3
    contains "$REPORT" 'release lookup failed' 'release lookup failure reason'
    contains "$REPORT" 'Scanned: 2 of 2.' 'pinned targets stopped after release failure'
    absent "$REPORT" '| release | ghcr.io/' 'invalid release target planned'
    count_lines "$CASE/grype.argv" 2 'pins were not scanned'
    console incomplete
  done
  new_case; values
  run_scan targets --root "$ROOT" --repo example.test/project; expect_code 0
  contains "$CASE/gh.argv" '--repo example.test/project' 'custom repo ignored'
  printf 'latest\n' > "$CASE/gh.tag"
  run_scan targets --root "$ROOT"; expect_code 3
  contains "$CASE/stderr" 'security-scan: targets incomplete: release lookup failed' 'targets failure diagnostic'
  count_lines "$CASE/stderr" 1 'targets failure extra diagnostics'
fi
TEST=T12
if selected "$TEST"; then
  new_case; values; archive; reject_path_grype
  run_scan images --root "$ROOT" --release-version 0.0.333 --report "$REPORT"; expect_code 3
  contains "$REPORT" 'checksum mismatch' 'archive accepted without checksum verification'
  [ ! -f "$CASE/grype.argv" ] || fail 'unverified archive executed'
  pin_version=$(sed -n 's/^GRYPE_VERSION=//p' "$SOURCE_ROOT/scripts/security-scan-grype.env")
  contains "$CASE/curl.argv" "https://github.com/anchore/grype/releases/download/v$pin_version/grype_${pin_version}_linux_amd64.tar.gz" 'pinned Linux URL'
  contains "$CASE/curl.argv" '|--fail --silent --show-error --location --output ' 'download flags'
  console incomplete
fi
TEST=T12b
if selected "$TEST"; then
  for platform in linux_amd64 darwin_arm64; do
    new_case; values; archive; reject_path_grype; copy_scanner; matching_pin "$platform"
    if [ "$platform" = linux_amd64 ]; then os=Linux; arch=x86_64; else os=Darwin; arch=arm64; fi
    printf '%s\n' "$os" > "$CASE/uname.os"; printf '%s\n' "$arch" > "$CASE/uname.arch"
    run_scan images --root "$ROOT" --release-version 0.0.333 --report "$REPORT"; expect_code 0
    contains "$CASE/curl.argv" "_${platform}.tar.gz" 'platform archive URL'
    contains "$REPORT" 'Scanned: 3 of 3.' 'downloaded scanner targets'
    count_lines "$CASE/grype.argv" 3 'downloaded scanner did not run'
    grype_contract
    exact_grype_targets
    downloaded_grype_contract
    console clean
    absent "$REPORT" '(caller-supplied)' 'download incorrectly attributed to caller'
    # Only the other platform matches; choosing its checksum must be rejected.
    new_case; values; archive; reject_path_grype; copy_scanner
    if [ "$platform" = linux_amd64 ]; then other_platform=darwin_arm64; else other_platform=linux_amd64; fi
    matching_pin "$other_platform"
    printf '%s\n' "$os" > "$CASE/uname.os"; printf '%s\n' "$arch" > "$CASE/uname.arch"
    run_scan images --root "$ROOT" --release-version 0.0.333 --report "$REPORT"; expect_code 3
    contains "$REPORT" 'checksum mismatch' 'wrong platform checksum accepted'
    [ ! -f "$CASE/grype.argv" ] && [ ! -f "$CASE/tar.argv" ] || fail 'wrong platform archive extracted or executed'
    console incomplete
  done
fi
TEST=T12c
if selected "$TEST"; then
  new_case; values
  printf 'aarch64\n' > "$CASE/uname.arch"
  run_scan images --root "$ROOT" --release-version 0.0.333 --report "$REPORT"; expect_code 3
  contains "$REPORT" 'missing tool: grype (unsupported platform; pass --grype PATH)' 'unsupported platform classification'
  [ ! -s "$CASE/curl.argv" ] || fail 'unsupported platform downloaded an archive'
  console incomplete
fi
TEST=T12d
if selected "$TEST"; then
  for tool in curl uname tar; do
    new_case; values; reject_path_grype
    rm "$CASE/bin/$tool"
    run_scan images --root "$ROOT" --release-version 0.0.333 --report "$REPORT"; expect_code 3
    contains "$REPORT" "missing tool: $tool" "missing download tool not classified ($tool)"
    [ ! -f "$CASE/curl.argv" ] && [ ! -f "$CASE/tar.argv" ] && [ ! -f "$CASE/grype.argv" ] || fail 'missing tool used ambient download or scanner'
    console incomplete
  done
fi
TEST=T13g
if selected "$TEST"; then
  new_case
  "$REAL_RUBY" -E UTF-8:UTF-8 - "$SOURCE_ROOT/scripts/security-scan-grype.env" <<'RUBY' || fail 'committed Grype pin invalid'
lines=File.readlines(ARGV[0],chomp:true,encoding:"UTF-8")
abort 'line count' unless lines.length==3
expected=%w[GRYPE_VERSION GRYPE_LINUX_AMD64_SHA256 GRYPE_DARWIN_ARM64_SHA256]
values={}
lines.each do |line|
  key,value=line.split('=',2)
  abort 'key' unless expected.include?(key) && !values.key?(key)
  pattern=key=='GRYPE_VERSION' ? /\A\d+\.\d+\.\d+\z/ : /\A[0-9a-f]{64}\z/
  abort key unless value && pattern.match?(value)
  values[key]=value
end
checksums=expected.drop(1).map { |key| values.fetch(key) }
abort 'GRYPE_LINUX_AMD64_SHA256' if checksums[0]=='0'*64
abort 'GRYPE_DARWIN_ARM64_SHA256' if checksums[1]=='0'*64 || checksums[0]==checksums[1]
puts 'ok'
RUBY
  for mode in missing directory malformed duplicate injection; do
    new_case; values; copy_scanner
    pin=$CASE/copied/scripts/security-scan-grype.env
    case "$mode" in
      missing) rm "$pin" ;;
      directory) rm "$pin"; mkdir "$pin" ;;
      malformed) printf 'GRYPE_VERSION=bad\n' > "$pin" ;;
      duplicate) first_line=$(sed -n '1p' "$pin"); printf '%s\n' "$first_line" >> "$pin" ;;
      injection) printf '\ntouch %s\n' "$CASE/pin-executed" >> "$pin" ;;
    esac
    run_scan images --root "$ROOT" --release-version 0.0.333 --report "$REPORT"; expect_code 3
    contains "$REPORT" 'missing tool: grype (invalid pin file)' 'strict pin parser'
    [ ! -f "$CASE/pin-executed" ] && [ ! -f "$CASE/curl.argv" ] || fail 'pin file was sourced or download proceeded'
  done
fi
TEST=T14i
if selected "$TEST"; then
  new_case; values
  # jq evaluates this variable, not the shell.
  # shellcheck disable=SC2016
  "$REAL_JQ" '.matches = [range(200) as $i | .matches[0] | .vulnerability.id=("CVE-0000-"+($i|tostring))]' "$FIXTURES/images/policy.json" > "$CASE/many.json"
  printf 'registry:ghcr.io/witwave-ai/images/witself-postgres-backup:0.0.333\t%s\t0\n' "$CASE/many.json" > "$CASE/grype-map.tsv"
  scan_images; expect_code 1
  contains "$REPORT" 'images=200 incomplete=0' 'uncapped header finding count'
  contains "$REPORT" '… and 150 more' 'image row cap remainder'
  "$REAL_RUBY" -E UTF-8:UTF-8 - "$REPORT" <<'RUBY' || fail 'image report bounds'
text=File.read(ARGV[0],encoding:'UTF-8')
abort 'size' unless text.bytesize<=60_000
abort 'rows' unless text.lines.count { |line| line.start_with?('| release |') && line.include?('| CVE-') }==50
abort 'target markers' unless text.scan('<!-- targets:begin -->').length==1 && text.scan('<!-- targets:end -->').length==1
abort 'targets' unless text.split('<!-- targets:begin -->').last.split('<!-- targets:end -->').first.lines.count { |line| line.start_with?('| cell:', '| release |') }==3
targets=text.split('<!-- targets:begin -->').last.split('<!-- targets:end -->').first.lines.select { |line| line.start_with?('| cell:', '| release |') }
abort 'digest width' unless targets.all? { |line| line.split('|')[4].strip.match?(/\A(?:sha256:[0-9a-f]{12}|-)\z/) }
abort 'header' unless text.lines.first.match?(/\A<!-- witself-security-scan v1 status=findings go=0 npm_prod=0 npm_toolchain=0 images=200 incomplete=0 -->\n\z/)
RUBY
  hygiene "$REPORT" "$CASE/stdout" "$CASE/stderr"
fi
TEST=T16i
if selected "$TEST"; then
  new_case
  "$REAL_RUBY" -E UTF-8:UTF-8 -ryaml - "$SOURCE_ROOT" <<'RUBY' || fail 'image test wiring'
root=ARGV[0]
make=File.read(File.join(root,'Makefile'),encoding:'UTF-8')
recipe=make[/^check-infra:[^\n]*\n(?:\t[^\n]*\n|\n)*/]
abort 'check-infra' unless recipe && recipe.scan('bash scripts/test-security-scan-images.sh').length==1
workflow=YAML.safe_load(File.read(File.join(root,'.github/workflows/ci.yml'),encoding:'UTF-8'),aliases:false)
# The offline contract checks live in the full application lane (helm-full); 'helm' is its frozen required-name wrapper (#737).
steps=workflow.fetch('jobs').fetch('helm-full').fetch('steps')
image=steps.select { |s| s['run']=='bash scripts/test-security-scan-images.sh' }
abort 'helm' unless image.length==1 && image[0]['name']=='security scan image offline contract checks' && image[0]['timeout-minutes']==5
matrix=File.read(File.join(root,'scripts/test-cell-roll-states.sh'),encoding:'UTF-8')
line='run_check security-scan-targets ruby scripts/testdata/security-scan/targets-coverage.rb "$fixture" 0.0.333'
abort 'matrix count' unless matrix.scan(line).length==1
branch=matrix[/elif \[\[ "\$state" == committed \]\]; then.*?\n  fi/m]
abort 'matrix branch' unless branch && branch.split(/^  else\n/,2).last&.include?(line)
abort 'matrix generation order' unless branch.index(line)<branch.rindex('run_check generation-check')
RUBY
fi
TEST=T17i
if selected "$TEST"; then
  new_case
  "$REAL_RUBY" -E UTF-8:UTF-8 - "$SCRIPT" "$FIXTURES"/bin/image-* <<'RUBY' || fail 'Bash 3.2 compatibility'
patterns=['map'+'file','read'+'array','declare '+'-A','local '+'-n','${'+'[^}]*,,','${'+'[^}]*\^\^','|'+'&','&'+'>>','wait '+'-n','set '+'-x']
# Fixed syntax tokens and patterns are kept separate to avoid self-matching.
patterns=patterns.map.with_index { |s,i| (i==4 || i==5) ? Regexp.new(Regexp.escape('${')+s[2..]) : Regexp.new(Regexp.escape(s)) }
ARGV.each { |path| text=File.read(path,encoding:"UTF-8"); abort File.basename(path) if patterns.any? { |p| text.match?(p) } }
RUBY
fi
TEST=T18i
if selected "$TEST"; then
  new_case; values
  run_scan images --root "$ROOT" --grype "$CASE/bin/grype" --report "$REPORT"; expect_code 0
  for log in "$CASE"/*.argv "$CASE/stdout" "$CASE/stderr" "$REPORT"; do
    absent "$log" 'witself-test-token-value' 'token leaked'
  done
  contains "$REPORT" '(caller-supplied)' 'caller Grype attribution missing'
  contains "$REPORT" '0.120.1' 'Grype descriptor version missing'
  new_case; values release-only
  mkdir "$CASE/caller"
  cp "$FIXTURES/bin/image-grype" "$CASE/caller/caller-grype"
  chmod +x "$CASE/caller/caller-grype"
  pushd "$CASE/caller" >/dev/null
  run_scan images --root "$ROOT" --release-version 0.0.333 --grype caller-grype --report "$REPORT"; expect_code 0
  popd >/dev/null
  count_lines "$CASE/grype.argv" 2 'relative caller executable not used'
fi
TEST=T19i
if selected "$TEST"; then
  new_case; values; map_pinned_fixture policy
  printf 'module example.test/fixture\n' > "$ROOT/go.mod"
  mkdir -p "$ROOT/infra/cloudflare/fixture"
  printf '{}\n' > "$ROOT/infra/cloudflare/fixture/package.json"
  printf '{}\n' > "$ROOT/infra/cloudflare/fixture/package-lock.json"
  run_scan all --root "$ROOT" --release-version 0.0.333 --grype "$CASE/bin/grype" --report "$REPORT"; expect_code 1
  contains "$REPORT" '## Go (govulncheck, called vulnerabilities)' 'all omitted Go section'
  contains "$REPORT" '## npm production dependencies (moderate or above)' 'all omitted npm production section'
  contains "$REPORT" '## npm toolchain (high or above)' 'all omitted npm toolchain section'
  contains "$REPORT" '## Images (Critical or High with a fix)' 'all omitted image section'
  count_lines "$CASE/go.argv" 1 'all did not scan Go target'
  count_lines "$CASE/npm.argv" 3 'all did not scan both npm audits'
  count_lines "$CASE/grype.argv" 3 'all image target count'
  exact_grype_targets
  "$REAL_RUBY" -E UTF-8:UTF-8 - "$REPORT" <<'RUBY' || fail 'all header image count'
header=File.readlines(ARGV[0],encoding:'UTF-8').first
abort 'image count' unless header.include?(' images=2 incomplete=0 -->')
RUBY
  console findings
fi
TEST=T20i
if selected "$TEST"; then
  new_case; values release-only
  map_fixture registry:ghcr.io/witwave-ai/images/witself-postgres-backup:0.0.333 policy
  scan_images; expect_code 1; console findings
fi
TEST=T22
if selected "$TEST"; then
  for mode in failed invalid missing-values broken-values; do
    new_case; values
    case "$mode" in
      failed)
        map_fixture registry:ghcr.io/witwave-ai/images/witself-postgres-backup:0.0.333 clean 1
        reason='scanner failed'; target='release ghcr.io/witwave-ai/images/witself-postgres-backup:0.0.333'; counts='Scanned: 2 of 3.' ;;
      invalid)
        printf '{}\n' > "$CASE/invalid.json"
        printf 'registry:ghcr.io/witwave-ai/images/witself-postgres-backup:0.0.333\t%s\t0\n' "$CASE/invalid.json" > "$CASE/grype-map.tsv"
        reason='invalid output'; target='release ghcr.io/witwave-ai/images/witself-postgres-backup:0.0.333'; counts='Scanned: 2 of 3.' ;;
      missing-values)
        rm "$ROOT/.gitops/cells/civo-sandbox-use1-backup/values.yaml"
        reason='missing values'; target='cell:civo-sandbox-use1-backup'; counts='Scanned: 3 of 3.' ;;
      broken-values)
        printf 'apps: [\n' > "$ROOT/.gitops/cells/civo-sandbox-use1-backup/values.yaml"
        reason='missing values'; target='cell:civo-sandbox-use1-backup'; counts='Scanned: 3 of 3.' ;;
    esac
    scan_images; expect_code 3
    contains "$REPORT" "| $target | $reason |" 'incomplete target identity or reason'
    contains "$REPORT" "$counts" 'other image targets did not finish'
    console incomplete
  done
fi
if [ "${SECURITY_SCAN_IMAGE_TEST_CASES:-all}" = all ]; then [ "$ENV_LINES" -gt 0 ] || fail 'no offline environment evidence'; fi
# Exercise the UTF-8 readers with no inherited locale and prove that ambient
# scanner executables never enter the curated per-case PATH.
if [ "${SECURITY_SCAN_IMAGE_TEST_SELF_CHECK:-0}" != 1 ]; then
  env -u LANG -u LC_ALL -u LC_CTYPE SECURITY_SCAN_IMAGE_TEST_SELF_CHECK=1 \
    SECURITY_SCAN_IMAGE_TEST_CASES=T9c,T16i,T17i "$SCAN_BASH" "$SOURCE_ROOT/scripts/test-security-scan-images.sh"
  mkdir -p "$SUITE/host-bin"
  for tool in go grype; do
    cat > "$SUITE/host-bin/$tool" <<'STUB'
#!/bin/bash
printf 'ambient scanner invoked\n' >> "${SECURITY_SCAN_HOST_SCANNER_LOG:?}"
exit 99
STUB
    chmod +x "$SUITE/host-bin/$tool"
  done
  env PATH="$SUITE/host-bin:$PATH" SECURITY_SCAN_IMAGE_TEST_SELF_CHECK=1 \
    SECURITY_SCAN_HOST_SCANNER_LOG="$SUITE/host-scanner.log" SECURITY_SCAN_IMAGE_TEST_CASES=T12,T12b,T12d,T19i \
    "$SCAN_BASH" "$SOURCE_ROOT/scripts/test-security-scan-images.sh"
  [ ! -e "$SUITE/host-scanner.log" ] || fail 'ambient host scanner escaped curated PATH'
fi
printf 'security scan images: PASS (%s cases)\n' "$CASES"
