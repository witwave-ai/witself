#!/bin/bash
# Offline synthetic issue lifecycle and public report contracts.
set -euo pipefail
SOURCE_ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)
SCRIPT=$SOURCE_ROOT/scripts/security-scan-issue.sh
FIXTURES=$SOURCE_ROOT/scripts/testdata/security-scan/issue
REAL_RUBY=$(command -v ruby)
TEST_BASH=$(command -v bash)
if [ "$(/bin/bash -c 'printf "%s" "${BASH_VERSINFO[0]}"')" = 3 ]; then TEST_BASH=/bin/bash; fi
SUITE=$(mktemp -d "${TMPDIR:-/tmp}/witself-security-scan-issue-test.XXXXXX")
SUITE=$(cd "$SUITE" && pwd -P)
trap 'rm -rf -- "$SUITE"' EXIT
TOOLS=$SUITE/tools
mkdir -p "$TOOLS"
# Do not inherit host directories: even /usr/bin contains gh on hosted runners.
for tool in dirname mktemp rm cat mkdir find jq ruby; do
  ln -s "$(command -v "$tool")" "$TOOLS/$tool"
done
CASE=
REPORT=
CODE=0
CASES=0
ENV_LINES=0
TEST=setup
RUN_URL=https://github.com/witwave-ai/witself/actions/runs/123
fail() { printf 'security scan issue: FAIL (%s: %s)\n' "$TEST" "$1" >&2; exit 1; }
selected() {
  case ",${SECURITY_SCAN_ISSUE_TEST_CASES:-all}," in
    *,all,*|*,"$1",*) return 0 ;;
    *) return 1 ;;
  esac
}
new_case() {
  CASE=$SUITE/case-$CASES
  CASES=$((CASES + 1))
  REPORT=$CASE/report.md
  mkdir -p "$CASE/bin" "$CASE/tmp"
  cp "$FIXTURES/gh" "$CASE/bin/gh"
  chmod +x "$CASE/bin/gh"
  cp "$FIXTURES/findings.md" "$REPORT"
  printf '[]\n' > "$CASE/issues.json"
  printf '[]\n' > "$CASE/labels.json"
  : > "$CASE/argv.jsonl"
}
issues() {
  "$REAL_RUBY" -E UTF-8:UTF-8 -rjson - "$CASE/issues.json" "$@" <<'RUBY'
path=ARGV.shift
rows=ARGV.map do |value|
  number, kind=value.split(':',2)
  row={'number'=>number.to_i,'title'=>'Scheduled security scan findings',
       'body'=>"<!-- witself-security-scan v1 status=findings -->\nSynthetic body",
       'author'=>{'login'=>'app/github-actions'}}
  case kind
  when 'title' then row['title']='Unrelated security issue'
  when 'body' then row['body']='Human report with no scanner header'
  when 'human' then row['author']['login']='octocat'
  when 'bot' then row['author']['login']='github-actions[bot]'
  when 'empty-human'
    row['body']=''; row['author']['login']='octocat'
  when 'empty-app' then row['body']=''
  when 'empty-bot'
    row['body']=''; row['author']['login']='github-actions[bot]'
  end
  row
end
File.write(path,JSON.generate(rows),encoding:'UTF-8')
RUBY
}
invoke() {
  env PATH="$CASE/bin:$TOOLS" \
    http_proxy=http://127.0.0.1:9 https_proxy=http://127.0.0.1:9 \
    HTTP_PROXY=http://127.0.0.1:9 HTTPS_PROXY=http://127.0.0.1:9 \
    GOPROXY=off npm_config_offline=true RUBYOPT=--disable-gems GH_TOKEN=witself-test-token-value \
    TMPDIR="$CASE/tmp" SECURITY_SCAN_ISSUE_CASE="$CASE" \
    "$TEST_BASH" "$SCRIPT" "$@"
}
run_issue() {
  CODE=0
  invoke "$@" > "$CASE/stdout" 2> "$CASE/stderr" || CODE=$?
  if [ "$1" = sync ] && [ "$CODE" -ne 2 ]; then
    [ ! -s "$CASE/stderr" ] || fail 'I16 sync wrote stderr'
    "$REAL_RUBY" -E UTF-8:UTF-8 - "$CASE/stdout" <<'RUBY' || fail 'I16 non-usage sync console format'
text=File.read(ARGV[0],encoding:'UTF-8')
exit(text.match?(/\Asecurity-scan-issue: action=(?:created|edited|closed|none) issue=(?:[0-9]+|-) status=(?:clean|findings|incomplete)\n\z/) ? 0 : 1)
RUBY
  fi
  [ -z "$(find "$CASE/tmp" -mindepth 1 -print -quit)" ] || fail 'private temporary directory leaked'
  if [ -f "$CASE/env.log" ]; then
    "$REAL_RUBY" -E UTF-8:UTF-8 - "$CASE" <<'RUBY' || fail 'stub provenance or offline environment'
root=ARGV.fetch(0)
env=File.readlines(root+'/env.log',chomp:true,encoding:'UTF-8')
argv=File.readlines(root+'/argv.log',chomp:true,encoding:'UTF-8')
exit(env.any? && env.all? { |s| s=='off|http://127.0.0.1:9|true' } &&
     argv.all? { |s| s.start_with?(root+'/bin/gh|') } ? 0 : 1)
RUBY
    ENV_LINES=$((ENV_LINES + $(wc -l < "$CASE/env.log")))
  fi
}
sync_issue() { run_issue sync --report "$REPORT" --scan-result "${1:-success}" --run-url "$RUN_URL" --repo witwave-ai/witself; }
render_issue() { run_issue render --detail "$1" --report "$REPORT" --out "$CASE/public.md"; }
expect_code() { [ "$CODE" -eq "$1" ] || fail "exit $CODE, expected $1"; }
equal_file() {
  "$REAL_RUBY" -E UTF-8:UTF-8 -e 'exit(File.read(ARGV[0],encoding:"UTF-8")==File.read(ARGV[1],encoding:"UTF-8") ? 0 : 1)' "$1" "$2" || fail "$3"
}
console() {
  [ ! -s "$CASE/stderr" ] || fail 'sync wrote stderr'
  "$REAL_RUBY" -E UTF-8:UTF-8 - "$CASE/stdout" "$1" "$2" "$3" <<'RUBY' || fail 'I16 console contract'
path,action,issue,status=ARGV
exit(File.read(path,encoding:'UTF-8')=="security-scan-issue: action=#{action} issue=#{issue} status=#{status}\n" ? 0 : 1)
RUBY
}
assert_calls() {
  "$REAL_RUBY" -E UTF-8:UTF-8 -rjson - "$CASE" "$1" "${2:-findings}" "$RUN_URL" "${3:-}" <<'RUBY' || fail 'GitHub command sequence or exact arguments'
root,expected,status,url,others=ARGV
calls=File.readlines(root+'/argv.jsonl',chomp:true,encoding:'UTF-8').map { |line| JSON.parse(line) }
raise 'sequence' unless calls.map { |a| a.take(2).join(' ') }==expected.split(',')
calls.each do |args|
  raise 'repo' unless args.count('--repo')==1 && args[args.index('--repo')+1]=='witwave-ai/witself'
  case args.take(2)
  when ['issue','list']
    raise 'issue list' unless args==%w[issue list --repo witwave-ai/witself --state open --label security --limit 100 --json number,title,body,author]
  when ['label','list']
    raise 'label list' unless args==%w[label list --repo witwave-ai/witself --json name --limit 200]
  when ['label','create']
    raise 'label create' unless args==['label','create','security','--repo','witwave-ai/witself','--description','Security scan findings','--color','B60205']
  when ['issue','create']
    raise 'create' unless args.take(9)==['issue','create','--repo','witwave-ai/witself','--title','Scheduled security scan findings','--label','security','--body-file'] && args.length==10
  when ['issue','edit']
    raise 'edit' unless args.take(6)==%w[issue edit 7 --repo witwave-ai/witself --body-file] && args.length==7
  when ['issue','comment']
    raise 'comment' unless args.take(6)==%w[issue comment 7 --repo witwave-ai/witself --body] && args.length==7
    expected_body=status=='clean' ? "Clean scan: #{url}" : "Scheduled scan #{status}: #{url}"
    expected_body+=" Other matching issues: #{others}." unless others.empty?
    raise 'comment body' unless args[6]==expected_body
  when ['issue','close']
    raise 'close' unless args==%w[issue close 7 --repo witwave-ai/witself --reason completed]
  else raise 'unknown command'
  end
end
RUBY
}
hygiene() {
  "$REAL_RUBY" -E UTF-8:UTF-8 - "$CASE" <<'RUBY' || fail 'I12 token or digest leaked'
Dir.glob(ARGV.fetch(0)+'/{argv.log,argv.jsonl,body.md,public.md,stdout,stderr}').each do |path|
  text=File.read(path,encoding:'UTF-8')
  exit 1 if text.include?('witself-test-token-value') || text.match?(/[a-fA-F0-9]{64}/)
end
RUBY
}

if selected I1; then
  TEST=I1
  for label in absent present; do
    new_case
    if [ "$label" = present ]; then printf '[{"name":"security"},{"name":"other"}]\n' > "$CASE/labels.json"; fi
    sync_issue; expect_code 0
    if [ "$label" = absent ]; then assert_calls 'issue list,label list,label create,issue create'; else assert_calls 'issue list,label list,issue create'; fi
    equal_file "$REPORT" "$CASE/body.md" 'created body changed'
    console created 17 findings; hygiene
  done
fi
if selected I2; then
  TEST=I2; new_case; issues 7
  sync_issue; expect_code 0; assert_calls 'issue list,issue edit,issue comment'
  equal_file "$REPORT" "$CASE/body.md" 'edited body changed'; console edited 7 findings; hygiene
fi
if selected I3; then
  TEST=I3; new_case; issues 7; cp "$FIXTURES/clean.md" "$REPORT"
  sync_issue; expect_code 0; assert_calls 'issue list,issue comment,issue close' clean; console closed 7 clean
fi
if selected I4; then
  TEST=I4; new_case; cp "$FIXTURES/clean.md" "$REPORT"
  sync_issue; expect_code 0; assert_calls 'issue list' clean; console none - clean
fi
if selected I5; then
  TEST=I5; new_case; issues 9 7
  sync_issue; expect_code 0; assert_calls 'issue list,issue edit,issue comment' findings '#9'; console edited 7 findings
  for status in findings incomplete clean; do
    TEST=I5-three-$status; new_case; issues 11 7 9
    cp "$FIXTURES/$status.md" "$REPORT"
    sync_issue; expect_code 0
    if [ "$status" = clean ]; then
      assert_calls 'issue list,issue comment,issue close' clean; console closed 7 clean
    else
      assert_calls 'issue list,issue edit,issue comment' "$status" '#9, #11'; console edited 7 "$status"
      equal_file "$REPORT" "$CASE/body.md" 'duplicate selection changed body'
    fi
  done
fi
if selected I6; then
  TEST=I6; new_case; issues 2:title 3:body 4:human
  sync_issue; expect_code 0; assert_calls 'issue list,label list,label create,issue create'; console created 17 findings
  new_case; issues 7:bot
  sync_issue; expect_code 0; assert_calls 'issue list,issue edit,issue comment'; console edited 7 findings
fi
for kind in empty-human empty-app empty-bot; do
  for lifecycle in edit create close; do
    TEST=I6-$kind-$lifecycle
    if selected I6 || selected "$TEST"; then
      new_case
      if [ "$lifecycle" = create ]; then issues "3:$kind"; else issues "3:$kind" 7; fi
      if [ "$lifecycle" = close ]; then cp "$FIXTURES/clean.md" "$REPORT"; fi
      sync_issue; expect_code 0
      case "$lifecycle" in
        edit) assert_calls 'issue list,issue edit,issue comment'; console edited 7 findings ;;
        create) assert_calls 'issue list,label list,label create,issue create'; console created 17 findings ;;
        close) assert_calls 'issue list,issue comment,issue close' clean; console closed 7 clean ;;
      esac
      if [ "$lifecycle" != close ]; then equal_file "$REPORT" "$CASE/body.md" 'empty stranger changed body'; fi
      hygiene
    fi
  done
done
if selected I7; then
  TEST=I7
  for invalid in missing bogus headerless directory; do
    for detail in counts no-cells full; do
      new_case
      case "$invalid" in
        missing) rm "$REPORT" ;;
        bogus) sed '1s/status=findings/status=bogus/' "$FIXTURES/findings.md" > "$REPORT" ;;
        headerless) printf '# No header\n' > "$REPORT" ;;
        directory) rm "$REPORT"; mkdir "$REPORT" ;;
      esac
      render_issue "$detail"; expect_code 0
      equal_file "$FIXTURES/incomplete.md" "$CASE/public.md" 'invalid report must render exact stub'
      [ ! -s "$CASE/argv.jsonl" ] || fail 'render called gh'
    done
  done
fi
if selected I8; then
  TEST=I8
  for result in failure cancelled skipped; do
    new_case; issues 7; cp "$FIXTURES/clean.md" "$REPORT"
    sync_issue "$result"; expect_code 0; assert_calls 'issue list,issue edit,issue comment' incomplete; console edited 7 incomplete
    "$REAL_RUBY" -E UTF-8:UTF-8 - "$CASE/body.md" "$result" <<'RUBY' || fail 'non-success clean report was trusted'
text=File.read(ARGV[0],encoding:'UTF-8')
exit(text.lines.first.include?('status=incomplete') && !text.lines.first.include?('status=clean') &&
     text.lines.last=="Scan job result: #{ARGV[1]}; this run is treated as incomplete.\n" ? 0 : 1)
RUBY
  done
fi
if selected I9; then
  TEST=I9
  for shape in lines longline unicode; do
    new_case; issues 7
    "$REAL_RUBY" -E UTF-8:UTF-8 - "$REPORT" "$shape" <<'RUBY'
path,shape=ARGV
header=File.readlines(path,encoding:'UTF-8').first
payload=case shape
when 'lines' then ("#{'x'*99}\n"*700)
when 'longline' then "#{'x'*70000}\n"
else "#{"\u00e9"*49}\n"*710
end
File.write(path,header+payload,encoding:'UTF-8')
RUBY
    sync_issue; expect_code 0; assert_calls 'issue list,issue edit,issue comment'; console edited 7 findings
    "$REAL_RUBY" -E UTF-8:UTF-8 - "$REPORT" "$CASE/body.md" <<'RUBY' || fail 'body byte limit or line boundary'
source=File.read(ARGV[0],encoding:'UTF-8'); text=File.read(ARGV[1],encoding:'UTF-8')
tail="Report truncated; see the run.\n"
prefix=text.delete_suffix(tail)
exit(text.bytesize<=65000 && text.end_with?(tail) && text.valid_encoding? &&
     prefix.end_with?("\n") && source.start_with?(prefix) ? 0 : 1)
RUBY
  done
fi
if selected I10; then
  TEST=I10; new_case; render_issue counts; expect_code 0
  "$REAL_RUBY" -E UTF-8:UTF-8 - "$REPORT" "$CASE/public.md" <<'RUBY' || fail 'counts allowlist or detail leak'
lines=File.readlines(ARGV[0],encoding:'UTF-8'); actual=File.read(ARGV[1],encoding:'UTF-8')
expected=lines.first+lines.drop(1).select { |s| s.match?(/\A(?:# |Time: |Commit: |## |Findings: |Incomplete targets: )/) }.join+
  "Detail is not published. Run make security-scan locally.\n"
exit(actual==expected && !actual.match?(/GO-|GHSA-|CVE-|cell:|\|/) ? 0 : 1)
RUBY
  new_case; render_issue full; expect_code 0; equal_file "$REPORT" "$CASE/public.md" 'full rendering changed report'
  new_case; cp "$FIXTURES/incomplete-summary.md" "$REPORT"
  render_issue counts; expect_code 0
  "$REAL_RUBY" -E UTF-8:UTF-8 - "$CASE/public.md" <<'RUBY' || fail 'incomplete counts allowlist or detail leak'
expected=<<'EXPECTED'
<!-- witself-security-scan v1 status=incomplete go=0 npm_prod=0 npm_toolchain=0 images=0 incomplete=2 -->
# Witself security scan
Time: 2026-10-12T13:17:00Z
Commit: unknown
## Incomplete
Incomplete targets: 2.
Detail is not published. Run make security-scan locally.
EXPECTED
exit(File.read(ARGV[0],encoding:'UTF-8')==expected ? 0 : 1)
RUBY
  [ ! -s "$CASE/argv.jsonl" ] || fail 'render called gh'
fi
if selected I10b; then
  TEST=I10b; new_case; render_issue no-cells; expect_code 0
  "$REAL_RUBY" -E UTF-8:UTF-8 - "$REPORT" "$CASE/public.md" <<'RUBY' || fail 'cell redaction or lost advisory details'
source=File.read(ARGV[0],encoding:'UTF-8'); text=File.read(ARGV[1],encoding:'UTF-8')
exit(text==source.gsub(/cell:[a-z0-9-]+/,'pinned') && !text.match?(/cell:|civo-/) && text.match?(/GO-/) && text.match?(/GHSA-/) && text.match?(/CVE-/) ? 0 : 1)
RUBY
fi
if selected I11; then
  TEST=I11
  for command in 'issue list' 'label list' 'label create' 'issue create' 'issue edit' 'issue comment' 'issue close'; do
    new_case
    case "$command" in 'issue edit'|'issue comment') issues 7 ;; 'issue close') issues 7; cp "$FIXTURES/clean.md" "$REPORT" ;; esac
    printf '%s\n' "$command" > "$CASE/fail-command"
    sync_issue; expect_code 1
    "$REAL_RUBY" -E UTF-8:UTF-8 -rjson - "$CASE/argv.jsonl" "$command" <<'RUBY' || fail 'continued writing after gh failure'
calls=File.readlines(ARGV[0],encoding:'UTF-8').map { |s| JSON.parse(s).take(2).join(' ') }
exit(calls.last==ARGV[1] ? 0 : 1)
RUBY
    hygiene
  done
fi
if selected I12; then
  TEST=I12; new_case; sync_issue; expect_code 0; hygiene; console created 17 findings
  new_case; issues 7; sync_issue; expect_code 0; hygiene; console edited 7 findings
fi
if selected I13; then
  TEST=I13
  new_case; run_issue bogus; expect_code 2
  new_case; run_issue render --detail partial --report "$REPORT" --out "$CASE/public.md"; expect_code 2
  new_case; run_issue sync --report "$REPORT" --scan-result success --repo witwave-ai/witself; expect_code 2
  new_case; sync_issue maybe; expect_code 2
  new_case; run_issue render --detail counts --report "$REPORT"; expect_code 2
  new_case; run_issue sync --report "$REPORT" --scan-result success --run-url "$RUN_URL"; expect_code 2
  new_case; run_issue render --detail counts --report "$REPORT" --out; expect_code 2
  new_case; run_issue sync --report "$REPORT" --scan-result success --run-url "$RUN_URL" --repo witwave-ai/witself --bogus; expect_code 2
fi
if selected I14; then
  TEST=I14
  for state in existing absent; do
    for invalid in missing headerless directory; do
      new_case
      if [ "$state" = existing ]; then issues 7; fi
      case "$invalid" in missing) rm "$REPORT" ;; headerless) printf '# Invalid\n' > "$REPORT" ;; directory) rm "$REPORT"; mkdir "$REPORT" ;; esac
      sync_issue cancelled; expect_code 0
      if [ "$state" = existing ]; then assert_calls 'issue list,issue edit,issue comment' incomplete; console edited 7 incomplete
      else assert_calls 'issue list,label list,label create,issue create' incomplete; console created 17 incomplete; fi
      equal_file "$FIXTURES/incomplete.md" "$CASE/body.md" 'missing or invalid sync report must use stub'
    done
  done
fi
if selected I15; then
  TEST=I15; new_case
  run_issue render --detail full --report "$REPORT" --out "$CASE/new/dir/report.md"; expect_code 0
  equal_file "$REPORT" "$CASE/new/dir/report.md" 'render did not create output parent'
fi
if selected I16; then
  TEST=I16
  for status in findings incomplete clean; do
    new_case; issues 7
    cp "$FIXTURES/$status.md" "$REPORT"
    sync_issue; expect_code 0
    if [ "$status" = clean ]; then console closed 7 clean; else console edited 7 "$status"; fi
  done
fi
if [ "${SECURITY_SCAN_ISSUE_TEST_CASES:-all}" = all ]; then [ "$ENV_LINES" -gt 0 ] || fail 'missing offline environment evidence'; fi
if [ "${SECURITY_SCAN_ISSUE_TEST_SELF_CHECK:-0}" != 1 ]; then
  env -u LANG -u LC_ALL -u LC_CTYPE SECURITY_SCAN_ISSUE_TEST_SELF_CHECK=1 \
    "$TEST_BASH" "$SOURCE_ROOT/scripts/test-security-scan-issue.sh"
  # An ambient gh must not answer even if the caller puts it first on PATH.
  mkdir -p "$SUITE/host-bin"
  cat > "$SUITE/host-bin/gh" <<'STUB'
#!/bin/bash
printf 'ambient gh invoked\n' >> "${SECURITY_SCAN_HOST_GH_LOG:?}"
exit 99
STUB
  chmod +x "$SUITE/host-bin/gh"
  env PATH="$SUITE/host-bin:$PATH" SECURITY_SCAN_ISSUE_TEST_SELF_CHECK=1 \
    SECURITY_SCAN_HOST_GH_LOG="$SUITE/host-gh.log" SECURITY_SCAN_ISSUE_TEST_CASES=I2 \
    "$TEST_BASH" "$SOURCE_ROOT/scripts/test-security-scan-issue.sh"
  [ ! -e "$SUITE/host-gh.log" ] || fail 'ambient gh escaped curated PATH'
fi
printf 'security scan issue: PASS (%s cases)\n' "$CASES"
