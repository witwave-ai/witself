#!/bin/bash
# Offline contracts only: each invocation sees fixture scanners and dead proxies.
set -euo pipefail
SOURCE_ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)
SCRIPT=$SOURCE_ROOT/scripts/security-scan.sh
FIXTURES=$SOURCE_ROOT/scripts/testdata/security-scan
REAL_JQ=$(command -v jq)
REAL_RUBY=$(command -v ruby)
REAL_GIT=$(command -v git)
SCAN_BASH=$(command -v bash)
if [ "$(/bin/bash -c 'printf "%s" "${BASH_VERSINFO[0]}"')" = 3 ]; then
  SCAN_BASH=/bin/bash
fi
SUITE=$(mktemp -d "${TMPDIR:-/tmp}/witself-security-scan-test.XXXXXX")
SUITE=$(cd "$SUITE" && pwd -P)
SIGNAL_SCAN_PID=
SIGNAL_CASE=
# Invoked by EXIT, including when a signal-case assertion fails.
# shellcheck disable=SC2329
cleanup() {
  if [ -n "$SIGNAL_SCAN_PID" ]; then
    : > "$SIGNAL_CASE/go.release"
    kill -TERM "$SIGNAL_SCAN_PID" 2>/dev/null || :
    wait "$SIGNAL_SCAN_PID" 2>/dev/null || :
  fi
  rm -rf -- "$SUITE"
}
trap cleanup EXIT
TOOLS=$SUITE/tools
mkdir -p "$TOOLS"
for tool in dirname mktemp rm cat mkdir sed awk tr sort cut date find wc env jq git ruby tar; do
  ln -s "$(command -v "$tool")" "$TOOLS/$tool"
done
if command -v sha256sum >/dev/null 2>&1; then
  ln -s "$(command -v sha256sum)" "$TOOLS/sha256sum"
else
  ln -s "$(command -v shasum)" "$TOOLS/shasum"
fi
CASE=
ROOT=
REPORT=
CODE=0
CASES=0
ENV_LINES=0
TEST=setup
fail() { printf 'security scan: FAIL (%s: %s)\n' "$TEST" "$1" >&2; exit 1; }
selected() {
  case ",${SECURITY_SCAN_TEST_CASES:-all}," in
    *,all,*|*,"$1",*) return 0 ;;
    *) return 1 ;;
  esac
}
new_case() {
  CASE=$SUITE/case-$CASES
  CASES=$((CASES + 1))
  ROOT=$CASE/root
  REPORT=$CASE/report.md
  mkdir -p "$ROOT" "$CASE/bin" "$CASE/tmp"
  cp "$FIXTURES"/bin/* "$CASE/bin/"
  chmod +x "$CASE/bin/"*
}
module() { mkdir -p "$ROOT/$1"; printf 'module example.test/fixture\n' > "$ROOT/$1/go.mod"; }
worker() {
  mkdir -p "$ROOT/infra/cloudflare/$1"
  printf '{}\n' > "$ROOT/infra/cloudflare/$1/package.json"
  printf '{}\n' > "$ROOT/infra/cloudflare/$1/package-lock.json"
}
called() { cp "$FIXTURES/go/called.json" "$ROOT/$1/.scan-go.json"; }
audit_fixture() { cp "$FIXTURES/npm/$3.json" "$ROOT/infra/cloudflare/$1/.scan-npm-$2.json"; }
invoke_scan() {
  env PATH="$CASE/bin:$TOOLS" \
    http_proxy=http://127.0.0.1:9 https_proxy=http://127.0.0.1:9 \
    HTTP_PROXY=http://127.0.0.1:9 HTTPS_PROXY=http://127.0.0.1:9 \
    GOPROXY=off npm_config_offline=true \
    GH_TOKEN=witself-test-token-value TMPDIR="$CASE/tmp" \
    SECURITY_SCAN_CASE="$CASE" SECURITY_SCAN_FIXTURES="$FIXTURES" \
    "$SCAN_BASH" "$SCRIPT" "$@"
}
check_scan_cleanup() {
  if [ -f "$CASE/env.log" ]; then
    "$REAL_RUBY" -E UTF-8:UTF-8 -e 'exit(File.readlines(ARGV[0], chomp: true, encoding: "UTF-8").all? { |x| x == "off|http://127.0.0.1:9|true" } ? 0 : 1)' "$CASE/env.log" || fail 'offline environment changed'
    ENV_LINES=$((ENV_LINES + $(wc -l < "$CASE/env.log")))
  fi
  [ ! -s "$CASE/unexpected.argv" ] || fail 'unexpected tool was invoked'
  [ -z "$(find "$CASE/tmp" -mindepth 1 -print -quit)" ] || fail 'private temporary directory was not removed'
}
run_scan() {
  CODE=0
  invoke_scan "$@" > "$CASE/stdout" 2> "$CASE/stderr" || CODE=$?
  check_scan_cleanup
}
scan_report() { run_scan "$1" --root "$ROOT" --report "$REPORT"; check_header; check_markdown; }
expect_code() { [ "$CODE" -eq "$1" ] || fail "exit $CODE, expected $1"; }
contains() { "$REAL_RUBY" -E UTF-8:UTF-8 -e 'exit(File.read(ARGV[0], encoding: "UTF-8").include?(ARGV[1]) ? 0 : 1)' "$1" "$2" || fail "$3"; }
absent() { "$REAL_RUBY" -E UTF-8:UTF-8 -e 'exit(File.read(ARGV[0], encoding: "UTF-8").include?(ARGV[1]) ? 1 : 0)' "$1" "$2" || fail "$3"; }
check_header() {
  "$REAL_RUBY" -E UTF-8:UTF-8 - "$REPORT" <<'RUBY' || fail 'invalid report header'
text=File.read(ARGV[0], encoding: 'UTF-8')
line=text.lines.first.chomp
abort 'sentinel' unless /\A<!-- witself-security-scan v1 status=(clean|findings|incomplete) go=[0-9]+ npm_prod=[0-9]+ npm_toolchain=[0-9]+ images=[0-9]+ incomplete=[0-9]+ -->\z/.match?(line)
abort 'time' unless text.lines.grep(/^Time: /).length==1 && text.match?(/^Time: \d{4}-\d\d-\d\dT\d\d:\d\d:\d\dZ$/)
abort 'commit' unless text.lines.grep(/^Commit: /).length==1 && text.match?(/^Commit: ([0-9a-f]{40}|unknown)$/)
RUBY
}
check_markdown() {
  "$REAL_RUBY" -E UTF-8:UTF-8 - "$REPORT" <<'RUBY' || fail 'report table spacing or count lines'
text=File.read(ARGV[0], encoding: 'UTF-8')
text.split(/^## /).drop(1).each do |section|
  lines=section.lines
  expected=section.start_with?('Incomplete') ? 'Incomplete targets:' : 'Findings:'
  abort 'counts' unless lines.count { |line| line.start_with?(expected) }==1
  lines.each_with_index do |line,index|
    next unless line.start_with?('Findings:', 'Incomplete targets:', "\u2026 and ")
    abort 'paragraph spacing' unless index>0 && lines[index-1]=="\n"
  end
end
RUBY
}
console() {
  [ ! -s "$CASE/stdout" ] || fail 'report run wrote stdout'
  "$REAL_RUBY" -E UTF-8:UTF-8 -e 's=File.read(ARGV[0], encoding: "UTF-8"); exit(/\Asecurity-scan: status=#{ARGV[1]} go=\d+ npm_prod=\d+ npm_toolchain=\d+ images=0 incomplete=\d+\n\z/.match?(s) && !s.match?(/GO-|GHSA-|CVE-/) ? 0 : 1)' "$CASE/stderr" "$1" || fail 'console summary leaked detail or had extra lines'
}
threshold_case() {
  new_case
  worker a; worker b
  audit_fixture a prod threshold-a-prod
  audit_fixture a all threshold-a-all
  audit_fixture b all threshold-b-all
}

TEST=T1
if selected "$TEST"; then
  new_case; run_scan --help; expect_code 0
  for scope in bogus go,bogus "," "go," ,go go,,npm all,go go,all all,npm; do
    new_case; run_scan "$scope"; expect_code 2
    "$REAL_RUBY" -E UTF-8:UTF-8 -e 'exit(/\Asecurity-scan: usage: [^\n]+\n\z/.match?(File.read(ARGV[0], encoding: "UTF-8")) ? 0 : 1)' "$CASE/stderr" || fail 'usage diagnostic shape'
  done
  new_case; run_scan go --bogus; expect_code 2
  new_case; run_scan go --report; expect_code 2
  new_case; run_scan go --root /nonexistent; expect_code 2
fi
TEST=T2
if selected "$TEST"; then
  new_case; module .; module infra/pulumi; scan_report go; expect_code 0
  check_header
  contains "$REPORT" 'status=clean go=0' 'clean status'
  contains "$REPORT" 'Scanned: 2 of 2.' 'module scan count'
  contains "$REPORT" 'v1.7.0' 'tool pin missing'
  contains "$REPORT" 'go1.27.1' 'toolchain missing'
  "$REAL_RUBY" -E UTF-8:UTF-8 -e 'cmd="run golang.org/x/vuln/cmd/govulncheck@v1.7.0 -format json ./..."; expected=["#{ARGV[1]}|#{cmd}", "#{ARGV[1]}/infra/pulumi|#{cmd}"]; exit(File.readlines(ARGV[0], chomp: true, encoding: "UTF-8")==expected ? 0 : 1)' "$CASE/go.argv" "$ROOT" || fail 'Go argv or physical working directories'
  new_case; module .; module infra/pulumi; worker a
  run_scan npm,go,go,npm --root "$ROOT"; expect_code 0
  REPORT=$CASE/stdout; check_header
  "$REAL_RUBY" -E UTF-8:UTF-8 -e 'exit(File.readlines(ARGV[0], chomp: true, encoding: "UTF-8")==%w[go go npm npm] ? 0 : 1)' "$CASE/tool-order.log" || fail 'scanner order or scope dedupe'
fi
TEST=T3
if selected "$TEST"; then
  new_case; module .; called .; scan_report go; expect_code 1
  "$REAL_JQ" -sr '
    [.[] | .finding? // empty] | group_by(.osv)[] |
    [.[] | select((.trace[0].function // "") | length > 0)] |
    select(length > 0) | sort_by(.trace[0].module, .trace[0].package, .trace[0].function) | .[0] |
    [".", .osv, .trace[0].module, .trace[0].package, .trace[0].version, (.fixed_version // "none")] |
    "| " + join(" | ") + " |"' "$FIXTURES/go/called.json" > "$CASE/expected-rows"
  "$REAL_JQ" -s '[.[] | .finding? // empty] | group_by(.osv) | map(select(all(.[]; ((.trace[0].function // "") | length) == 0))) | length' "$FIXTURES/go/called.json" > "$CASE/informational"
  "$REAL_RUBY" -E UTF-8:UTF-8 -e 'report=File.read(ARGV[0], encoding: "UTF-8"); rows=File.readlines(ARGV[1], chomp: true, encoding: "UTF-8"); info=File.read(ARGV[2], encoding: "UTF-8").strip; exit(rows.all? { |r| report.lines.map(&:chomp).include?(r) } && report.lines.first.include?("go=#{rows.length} ") && report.include?("Findings: #{rows.length}. Informational: #{info}. Scanned: 1 of 1.") ? 0 : 1)' "$REPORT" "$CASE/expected-rows" "$CASE/informational" || fail 'called rows, deterministic selection or informational count'
  contains "$REPORT" '| . | GO-0000-0002 | golang.org/x/net | golang.org/x/net/http2 | v0.59.0 | v0.60.0 |' 'row must use byte-order minimum, not stream order'
  absent "$REPORT" 'GO-0000-0001' 'package/module finding became called'
  for id in GO-2026-6617 GO-2026-6613 GO-2026-6612; do
    contains "$REPORT" "$id" 'expected toolchain advisory missing'
  done
  new_case; module .; module infra/pulumi; called infra/pulumi
  scan_report go; expect_code 1
  contains "$REPORT" '| infra/pulumi | GO-0000-0002 | golang.org/x/net | golang.org/x/net/http2 | v0.59.0 | v0.60.0 |' 'called finding lost its nested module'
  contains "$REPORT" 'Scanned: 2 of 2.' 'clean root or nested module omitted'
  absent "$REPORT" '| . | GO-' 'nested finding attributed to root'
fi
TEST=T4
if selected "$TEST"; then
  for mode in failed no-config non-json package module empty-version scanner-name; do
    new_case; module .; module infra/pulumi
    case "$mode" in
      failed) printf '1\n' > "$ROOT/infra/pulumi/.scan-go-exit"; reason='scanner failed' ;;
      no-config) printf '{"progress":{"message":"nothing"}}\n' > "$ROOT/infra/pulumi/.scan-go.json"; reason='invalid output' ;;
      non-json) printf 'not JSON\n' > "$ROOT/infra/pulumi/.scan-go.json"; reason='invalid output' ;;
      package|module)
        # jq evaluates this variable, not the shell.
        # shellcheck disable=SC2016
        "$REAL_JQ" --arg level "$mode" '.config.scan_level=$level' "$FIXTURES/go/clean.json" > "$ROOT/infra/pulumi/.scan-go.json"
        reason='invalid output' ;;
      empty-version)
        "$REAL_JQ" '.config.go_version=""' "$FIXTURES/go/clean.json" > "$ROOT/infra/pulumi/.scan-go.json"
        reason='invalid output' ;;
      scanner-name)
        "$REAL_JQ" '.config.scanner_name="other-scanner"' "$FIXTURES/go/clean.json" > "$ROOT/infra/pulumi/.scan-go.json"
        reason='invalid output' ;;
    esac
    scan_report go; expect_code 3
    contains "$REPORT" "| infra/pulumi | $reason |" 'incomplete module or reason'
    contains "$REPORT" 'Scanned: 1 of 2.' 'partial module scan count'
  done
  new_case; module .; worker a; rm "$CASE/bin/go"
  if PATH="$CASE/bin:$TOOLS" "$SCAN_BASH" -c 'command -v go' > "$CASE/go.lookup" 2>&1; then
    fail 'missing-Go PATH contains go'
  fi
  scan_report go,npm; expect_code 3
  contains "$REPORT" 'missing tool: go' 'missing Go tool'
  contains "$REPORT" 'npm production dependencies' 'other scanner omitted'
  contains "$REPORT" 'Scanned: 1 of 1.' 'npm did not continue'
  new_case; module .; worker a; rm "$CASE/bin/npm"; scan_report go,npm; expect_code 3
  contains "$REPORT" 'missing tool: npm' 'missing npm tool'
  contains "$REPORT" 'Scanned: 1 of 1.' 'Go did not continue'
fi
TEST=T5
if selected "$TEST"; then
  new_case; module .; module cmd/x/testdata/m; module tools; module .claude/worktrees/w; module x/node_modules/m; module x/.git/m
  scan_report go; expect_code 0
  [ "$(wc -l < "$CASE/go.argv")" -eq 2 ] || fail 'non-git discovery exclusions'
  contains "$CASE/go.argv" "$ROOT/tools|run " 'untracked tool module absent'
  new_case; module .; module tools; module nested; module .claude/worktrees/w
  "$REAL_GIT" -C "$ROOT" init -q
  "$REAL_GIT" -C "$ROOT/nested" init -q
  scan_report go; expect_code 0
  [ "$(wc -l < "$CASE/go.argv")" -eq 2 ] || fail 'git discovery entered nested repository'
  contains "$CASE/go.argv" "$ROOT/tools|run " 'git omitted untracked module'
  # A conflicted index returns one pathname for each unmerged stage. Simulate
  # ls-files directly: do not stage or modify this worktree's real index.
  new_case; module .; module infra/pulumi; module tools
  cat > "$CASE/bin/git" <<'STUB'
#!/bin/bash
case "$*" in
  *'rev-parse --show-toplevel') printf '%s\n' "$SECURITY_SCAN_CASE/root" ;;
  *'ls-files -z --cached --others --exclude-standard')
    printf '%s\0' tools/go.mod infra/pulumi/go.mod infra/pulumi/go.mod go.mod go.mod go.mod ;;
  *) exit 1 ;;
esac
STUB
  chmod +x "$CASE/bin/git"
  scan_report go; expect_code 0
  contains "$REPORT" 'Scanned: 3 of 3.' 'unmerged index entries scanned repeatedly'
  "$REAL_RUBY" -E UTF-8:UTF-8 -e 'paths=File.readlines(ARGV[0], encoding: "UTF-8").map { |line| line.split("|",2).first }; exit(paths==[ARGV[1], "#{ARGV[1]}/infra/pulumi", "#{ARGV[1]}/tools"] ? 0 : 1)' "$CASE/go.argv" "$ROOT" || fail 'duplicate discovery changed sorted modules'
  new_case; scan_report go; expect_code 3
  contains "$REPORT" 'no targets' 'empty modules accepted'
fi
TEST=T6
if selected "$TEST"; then
  threshold_case; scan_report npm; expect_code 1
  contains "$REPORT" 'npm_prod=1 npm_toolchain=2' 'npm threshold counts'
  contains "$REPORT" '| infra/cloudflare/a | prod-moderate | moderate | GHSA-aaaa-bbbb-cccc,GHSA-zzzz-aaaa-bbbb | yes |' 'production row or sorted advisories'
  contains "$REPORT" '| infra/cloudflare/a | dev-high-a | high | transitive | yes |' 'transitive row or object fix'
  contains "$REPORT" '| infra/cloudflare/b | dev-high-b | high | GHSA-high-bbbb-cccc | no |' 'toolchain row or absent fix'
  absent "$REPORT" '| dev-moderate-b |' 'toolchain moderate became finding'
  "$REAL_RUBY" -E UTF-8:UTF-8 -e 'r=File.read(ARGV[0], encoding: "UTF-8"); logs=File.readlines(ARGV[1], chomp: true, encoding: "UTF-8"); audits=logs.reject { |x| x.end_with?("|--version") }; expected=%w[a b].flat_map { |w| ["#{ARGV[2]}/infra/cloudflare/#{w}|audit --package-lock-only --json --omit=dev", "#{ARGV[2]}/infra/cloudflare/#{w}|audit --package-lock-only --json"] }; valid=audits.length==4 && audits.count { |x| x.include?("--omit=dev") }==2 && audits.all? { |x| wd,args=x.split("|",2); %w[a b].any? { |w| wd=="#{ARGV[2]}/infra/cloudflare/#{w}" } && args.split.sort == (args.include?("--omit=dev") ? %w[audit --package-lock-only --json --omit=dev] : %w[audit --package-lock-only --json]).sort }; exit(valid && r.scan(/\| prod-moderate \|/).length==1 && r.include?("Findings: 2. Informational: 2. Scanned: 2 of 2.") ? 0 : 1)' "$REPORT" "$CASE/npm.argv" "$ROOT" || fail 'npm exact audit flags, working directory, dedupe or informational count'
  new_case; worker sample; audit_fixture sample all npm-all; scan_report npm; expect_code 1
  contains "$REPORT" 'npm_prod=0 npm_toolchain=3' 'real npm audit sample rejected'
  new_case; worker sample; audit_fixture sample prod npm-vulnerable; audit_fixture sample all npm-vulnerable; scan_report npm; expect_code 1
  contains "$REPORT" 'npm_prod=1 npm_toolchain=0' 'real vulnerable sample or production dedupe rejected'
fi
TEST=T7
if selected "$TEST"; then
  new_case; worker a; worker b; rm "$ROOT/infra/cloudflare/a/package-lock.json"
  scan_report npm; expect_code 3
  contains "$REPORT" '| infra/cloudflare/a | no lockfile |' 'missing lockfile silently skipped'
  contains "$CASE/npm.argv" "$ROOT/infra/cloudflare/b|audit " 'remaining worker not scanned'
  new_case; scan_report npm; expect_code 3
  contains "$REPORT" 'no targets' 'empty workers accepted'
fi
TEST=T8
if selected "$TEST"; then
  for mode in error malformed version metadata zero-total multiple-documents; do
    new_case; worker a
    case "$mode" in
      error) printf '{"error":{"code":"ENOTFOUND","summary":"x"}}\n' > "$ROOT/infra/cloudflare/a/.scan-npm-all.json" ;;
      malformed) printf 'not JSON\n' > "$ROOT/infra/cloudflare/a/.scan-npm-all.json" ;;
      version) "$REAL_JQ" '.auditReportVersion=1' "$FIXTURES/npm/npm-prod.json" > "$ROOT/infra/cloudflare/a/.scan-npm-all.json" ;;
      metadata) "$REAL_JQ" 'del(.metadata.vulnerabilities)' "$FIXTURES/npm/npm-prod.json" > "$ROOT/infra/cloudflare/a/.scan-npm-all.json" ;;
      multiple-documents) cat "$FIXTURES/npm/npm-prod.json" "$FIXTURES/npm/npm-prod.json" > "$ROOT/infra/cloudflare/a/.scan-npm-all.json" ;;
      zero-total) "$REAL_JQ" '.metadata.dependencies.total=0' "$FIXTURES/npm/npm-prod.json" > "$ROOT/infra/cloudflare/a/.scan-npm-all.json" ;;
    esac
    scan_report npm; expect_code 3
    contains "$REPORT" 'invalid output' 'invalid npm output accepted'
  done
fi
TEST=T13
if selected "$TEST"; then
  "$REAL_RUBY" -E UTF-8:UTF-8 - "$SOURCE_ROOT" <<'RUBY' || fail 'govulncheck constant agreement'
root=ARGV[0]
script=File.read("#{root}/scripts/security-scan.sh", encoding: "UTF-8")[/^GOVULNCHECK_VERSION=["']?(v[\d.]+)/,1]
make=File.read("#{root}/Makefile", encoding: "UTF-8")[/^GOVULNCHECK_VERSION\s*[:?+]?=\s*(v[\d.]+)/,1]
abort 'GOVULNCHECK_VERSION' unless script && make==script
%w[ci release].each do |name|
  versions=File.read("#{root}/.github/workflows/#{name}.yml", encoding: "UTF-8").scan(/govulncheck@(v[\d.]+)/).flatten
  abort "#{name} GOVULNCHECK_VERSION" unless versions==[script]
end
puts 'ok'
RUBY
fi
TEST=T14
if selected "$TEST"; then
  new_case; module .; worker a
  # jq variables must remain literal for jq to evaluate.
  # shellcheck disable=SC2016
  "$REAL_JQ" -n --slurpfile clean "$FIXTURES/go/clean.json" --arg root "$ROOT" --arg tmp "$CASE/tmp" '
    $clean[], (range(200) | {finding:{osv:("GO-TEST-" + tostring),fixed_version:"v2.0.0",trace:[{module:"example.test/m",package:(("wide" * 500) + $root + $tmp + ("a" * 64)),function:"Called",version:"v1.0.0"}]}})' > "$ROOT/.scan-go.json"
  "$REAL_JQ" -n '{auditReportVersion:2,vulnerabilities:([range(200) | {key:("prod-"+tostring),value:{name:("prod-"+tostring),severity:"moderate",via:[{url:("https://github.com/advisories/GHSA-"+("wide"*500))}],fixAvailable:true}}] | from_entries),metadata:{vulnerabilities:{total:200},dependencies:{total:200}}}' > "$ROOT/infra/cloudflare/a/.scan-npm-prod.json"
  "$REAL_JQ" -n '{auditReportVersion:2,vulnerabilities:([range(200) | {key:("dev-"+tostring),value:{name:("dev-"+tostring),severity:"high",via:["parent"],fixAvailable:true}}] | from_entries),metadata:{vulnerabilities:{total:200},dependencies:{total:200}}}' > "$ROOT/infra/cloudflare/a/.scan-npm-all.json"
  scan_report go,npm; expect_code 1; check_header
  contains "$REPORT" 'go=200 npm_prod=200 npm_toolchain=200 images=0 incomplete=0' 'uncapped counts'
  "$REAL_RUBY" -E UTF-8:UTF-8 - "$REPORT" "$ROOT" "$CASE/tmp" <<'RUBY' || fail 'report row/byte bounds or path/digest hygiene'
s=File.read(ARGV[0], encoding: "UTF-8"); abort 'size' if s.bytesize>60_000
abort 'path' if s.include?(ARGV[1]) || s.include?(ARGV[2])
abort 'digest' if s.match?(/[a-fA-F0-9]{64}/)
sections=s.split(/^## /).drop(1)
abort 'sections' unless sections.length==3
sections.each do |section|
  rows=section.lines.grep(/^\| /)
  # Every table has a header plus 50 findings; a separator may contain spaces.
  data=rows.reject { |r| r.match?(/\A\|[\s:|\-]+\|\s*\z/) }.drop(1)
  abort 'row cap' unless data.length==50
  abort 'remainder' unless section.include?("\u2026 and 150 more")
  abort 'counts' unless section.include?('Findings: 200.')
end
RUBY
fi
TEST=T15
if selected "$TEST"; then
  new_case; module .; called .; worker a
  printf '{"error":{"code":"ENOTFOUND"}}\n' > "$ROOT/infra/cloudflare/a/.scan-npm-all.json"
  scan_report go,npm; expect_code 3
  contains "$REPORT" 'status=incomplete' 'findings overrode incomplete'
  contains "$REPORT" '| . | GO-0000-0002 |' 'incomplete discarded Go findings'
fi
TEST=T16
if selected "$TEST"; then
  "$REAL_RUBY" -E UTF-8:UTF-8 -ryaml - "$SOURCE_ROOT" <<'RUBY' || fail 'offline-only Makefile/workflow wiring'
root=ARGV[0]; make=File.read("#{root}/Makefile", encoding: "UTF-8")
abort 'target count' unless make.scan(/^security-scan:/).length==1
recipe=make[/^security-scan:[^\n]*\n((?:\t[^\n]*\n)+)/,1]
abort 'target recipe' unless recipe=="\tbash scripts/security-scan.sh all\n"
phony=make.lines.grep(/^\.PHONY:/).join.split
abort 'phony' unless phony.include?('security-scan')
abort 'recursive scan' if make.lines.grep(/^\t/).any? { |l| l.match?(/\$\(MAKE\)\s+security-scan(?:\s|$)/) }
%w[check check-infra].each do |target|
  body=make[/^#{target}:[^\n]*\n((?:\t[^\n]*\n)+)/,1]
  abort target unless body && !body.include?('scripts/security-scan.sh')
  if target=='check-infra'
    abort 'test count' unless body.scan('bash scripts/test-security-scan.sh').length==1
  end
end
ci=YAML.safe_load(File.read("#{root}/.github/workflows/ci.yml", encoding: "UTF-8"), aliases:false)
# The offline contract checks live in the full application lane (helm-full); 'helm' is its frozen required-name wrapper (#737).
steps=ci.fetch('jobs').fetch('helm-full').fetch('steps').select { |s| s['run']=='bash scripts/test-security-scan.sh' }
abort 'helm test step' unless steps.length==1 && steps[0]['name']=='security scan offline contract checks' && steps[0]['timeout-minutes']==5
Dir.glob("#{root}/.github/workflows/*.{yml,yaml}").each do |path|
  text=File.read(path, encoding: "UTF-8"); workflow=YAML.safe_load(text, aliases:false)
  triggers=workflow['on'] || workflow[true]; names=triggers.is_a?(Hash) ? triggers.keys : Array(triggers)
  protected=%w[ci.yml release.yml].include?(File.basename(path)) || !(names & %w[pull_request pull_request_target push]).empty?
  abort 'PR or release scan caller' if protected && text.include?('scripts/security-scan.sh')
end
RUBY
fi
TEST=T17
if selected "$TEST"; then
  "$REAL_RUBY" -E UTF-8:UTF-8 - "$SCRIPT" "$FIXTURES/bin" <<'RUBY' || fail 'Bash 3.2 incompatible construct or xtrace'
# Concatenation keeps these assertions from matching themselves if reused.
patterns=[/\b#{'map'+'file'}\b/,/\b#{'read'+'array'}\b/,/#{'declare'+'\s+-A'}\b/,/#{'local'+'\s+-n'}\b/,
          /\$\{[^}]*#{','+','}/,/\$\{[^}]*#{'^'+'^'}/,Regexp.new(Regexp.escape('|'+'&')),
          Regexp.new(Regexp.escape('&'+'>>')),/#{'wait'+'\s+-n'}\b/,/#{'set'+'\s+-[a-zA-Z]*x'}/]
([ARGV[0]]+Dir.glob("#{ARGV[1]}/*")).each do |file|
  text=File.read(file, encoding: "UTF-8")
  abort 'forbidden shell construct' if patterns.any? { |p| p.match?(text) }
end
RUBY
fi
TEST=T18
if selected "$TEST"; then
  new_case; module .; called .; worker a
  audit_fixture a all npm-all
  scan_report go,npm; expect_code 1
  for file in "$CASE/go.argv" "$CASE/npm.argv" "$CASE/stdout" "$CASE/stderr" "$REPORT"; do
    absent "$file" 'witself-test-token-value' 'token escaped into public data'
  done
  new_case; module .
  # Runtime construction exercises redaction before field truncation.
  # shellcheck disable=SC2016
  "$REAL_JQ" -n --slurpfile clean "$FIXTURES/go/clean.json" --arg root "$ROOT" --arg tmp "$CASE/tmp" '
    $clean[], {finding:{osv:"GO-HYGIENE",fixed_version:"witself-test-token-value",trace:[{module:$root,package:$tmp,function:"Called",version:("a"*64)}]}}' > "$ROOT/.scan-go.json"
  scan_report go; expect_code 1
  for value in "$ROOT" "$CASE/tmp" witself-test-token-value; do
    absent "$REPORT" "$value" 'sensitive short scanner field leaked'
  done
  "$REAL_RUBY" -E UTF-8:UTF-8 -e 's=File.read(ARGV[0], encoding: "UTF-8"); exit(s.match?(/[a-fA-F0-9]{64}/) || s.include?(ARGV[1][0,32]) ? 1 : 0)' "$REPORT" "$ROOT" || fail 'short digest or truncated absolute path leaked'
fi
TEST=T19
if selected "$TEST"; then
  new_case; module .; worker a; scan_report go,npm; expect_code 0
  [ "$ENV_LINES" -gt 0 ] || fail 'offline invocation evidence empty'
  [ -s "$CASE/env.log" ] || fail 'sentinel test invoked no scanner'
fi
TEST=T20
if selected "$TEST"; then
  threshold_case; module .; called .; scan_report go,npm; expect_code 1; console findings
  new_case; module .; called .; worker a
  printf '{"error":{"code":"ENOTFOUND"}}\n' > "$ROOT/infra/cloudflare/a/.scan-npm-all.json"
  scan_report go,npm; expect_code 3; console incomplete
fi
TEST=T21
if selected "$TEST"; then
  new_case; module .; REPORT=$CASE/new/dir/report.md
  scan_report go; expect_code 0
  [ -s "$REPORT" ] || fail 'report parent not created'
fi
TEST=T22
if selected "$TEST"; then
  new_case; module .
  RUN_TOKEN="security_scan_term_$$_$RANDOM"
  cat > "$CASE/bin/go" <<'STUB'
#!/bin/bash
exec "$SECURITY_SCAN_REAL_RUBY" -E UTF-8:UTF-8 --disable-gems -e '
  root=ARGV[1]
  deadline=Process.clock_gettime(Process::CLOCK_MONOTONIC)+10
  File.write("#{root}/go.ready", Process.pid.to_s)
  until File.exist?("#{root}/go.release")
    if Process.clock_gettime(Process::CLOCK_MONOTONIC)>=deadline
      File.write("#{root}/go.timeout", "timed out")
      exit 1
    end
    sleep 0.02
  end
  print File.read("#{ENV.fetch("SECURITY_SCAN_FIXTURES")}/go/clean.json", encoding: "UTF-8")
' "$SECURITY_SCAN_RUN_TOKEN" "$SECURITY_SCAN_CASE"
STUB
  chmod +x "$CASE/bin/go"
  # exec makes $! the scanner itself, with no wrapper shell to intercept TERM.
  (
    export SECURITY_SCAN_REAL_RUBY="$REAL_RUBY" SECURITY_SCAN_RUN_TOKEN="$RUN_TOKEN"
    exec env PATH="$CASE/bin:$TOOLS" \
      http_proxy=http://127.0.0.1:9 https_proxy=http://127.0.0.1:9 \
      HTTP_PROXY=http://127.0.0.1:9 HTTPS_PROXY=http://127.0.0.1:9 \
      GOPROXY=off npm_config_offline=true GH_TOKEN=witself-test-token-value TMPDIR="$CASE/tmp" \
      SECURITY_SCAN_CASE="$CASE" SECURITY_SCAN_FIXTURES="$FIXTURES" \
      "$SCAN_BASH" "$SCRIPT" go --root "$ROOT" --report "$REPORT"
  ) > "$CASE/stdout" 2> "$CASE/stderr" &
  SIGNAL_SCAN_PID=$!
  SIGNAL_CASE=$CASE
  "$REAL_RUBY" -E UTF-8:UTF-8 -e 'deadline=Process.clock_gettime(Process::CLOCK_MONOTONIC)+5; until File.size?(ARGV[0]); exit 1 if Process.clock_gettime(Process::CLOCK_MONOTONIC)>=deadline; sleep 0.02; end' "$CASE/go.ready" || fail 'sleeping Go stub did not start'
  if [ "${SECURITY_SCAN_TEST_NO_PROCESS_LIST:-0}" = 1 ]; then
    printf 'security scan: T22 process-group/pgrep check NOT RUN (sandbox); TERM assertions still run\n'
  else
    # Same portable pattern as test-roll-train-poll.sh: match a per-run argv
    # token inside this process group, and drop the suite and its ancestors.
    REAL_PS=$(command -v ps)
    REAL_PGREP=$(command -v pgrep)
    RUN_PGID=$("$REAL_PS" -o pgid= -p "$$" | tr -d ' ')
    [[ "$RUN_PGID" =~ ^[0-9]+$ ]] || fail 'cannot read test process group'
    ancestor=$$
    : > "$CASE/ancestors"
    while [[ "$ancestor" =~ ^[0-9]+$ ]] && [ "$ancestor" -gt 1 ]; do
      printf '%s\n' "$ancestor" >> "$CASE/ancestors"
      ancestor=$("$REAL_PS" -o ppid= -p "$ancestor" | tr -d ' ')
    done
    probe_status=0
    "$REAL_PGREP" -g "$RUN_PGID" -f "$RUN_TOKEN" > "$CASE/processes" || probe_status=$?
    [ "$probe_status" -le 1 ] || fail 'cannot inspect tokenized Go stub'
    "$REAL_RUBY" -E UTF-8:UTF-8 - "$CASE" <<'RUBY' || fail 'tokenized Go stub not uniquely visible'
root=ARGV[0]
read_pids=lambda { |name| File.readlines("#{root}/#{name}", encoding: 'UTF-8').map(&:strip) }
visible=read_pids.call('processes')-read_pids.call('ancestors')
abort 'process match' unless visible==read_pids.call('go.ready')
RUBY
  fi
  kill -TERM "$SIGNAL_SCAN_PID" || fail 'could not signal scanner'
  # Bash defers its trap while waiting for the foreground scanner child.
  : > "$CASE/go.release"
  CODE=0
  wait "$SIGNAL_SCAN_PID" || CODE=$?
  SIGNAL_SCAN_PID=
  expect_code 3
  [ ! -f "$CASE/go.timeout" ] || fail 'sleeping Go stub timed out'
  check_scan_cleanup
  console incomplete
  contains "$CASE/stderr" 'security-scan: status=incomplete go=0 npm_prod=0 npm_toolchain=0 images=0 incomplete=1' 'signal did not count an incomplete target'
  [ -s "$REPORT" ] || fail 'signal report missing'
  check_header
  "$REAL_RUBY" -E UTF-8:UTF-8 -e 's=File.read(ARGV[0], encoding: "UTF-8"); exit(s.lines.length==6 && s.lines.first.include?("status=incomplete go=0 npm_prod=0 npm_toolchain=0 images=0 incomplete=1") && !s.match?(/^## |^\|/) ? 0 : 1)' "$REPORT" || fail 'signal report was not header-only'
fi
[ "$CASES" -gt 0 ] || { case ",${SECURITY_SCAN_TEST_CASES:-all}," in *,T13,*|*,T16,*|*,T17,*) ;; *) fail 'no cases selected' ;; esac; }
# A bounded nested run catches the four UTF-8 readers under an unset locale.
# The second run simulates Ubuntu's ambient /usr/bin/go without changing /usr/bin.
if [ "${SECURITY_SCAN_TEST_SELF_CHECK:-0}" != 1 ]; then
  env -u LANG -u LC_ALL -u LC_CTYPE SECURITY_SCAN_TEST_SELF_CHECK=1 \
    SECURITY_SCAN_TEST_CASES=T13,T14,T16,T17 "$SCAN_BASH" "$SOURCE_ROOT/scripts/test-security-scan.sh"
  mkdir -p "$SUITE/host-bin"
  cat > "$SUITE/host-bin/go" <<'STUB'
#!/bin/bash
printf 'ambient go invoked\n' >> "${SECURITY_SCAN_HOST_GO_LOG:?}"
exit 99
STUB
  chmod +x "$SUITE/host-bin/go"
  env PATH="$SUITE/host-bin:$PATH" SECURITY_SCAN_TEST_SELF_CHECK=1 \
    SECURITY_SCAN_HOST_GO_LOG="$SUITE/host-go.log" SECURITY_SCAN_TEST_CASES=T4 \
    "$SCAN_BASH" "$SOURCE_ROOT/scripts/test-security-scan.sh"
  [ ! -e "$SUITE/host-go.log" ] || fail 'ambient host go escaped curated PATH'
fi
printf 'security scan: PASS (%s cases)\n' "$CASES"
