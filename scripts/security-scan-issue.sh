#!/bin/bash
# Publish a selected report view and maintain only the workflow-owned issue.
set -uo pipefail
umask 077

TITLE='Scheduled security scan findings'
LABEL=security
REPORT=
OUT=
DETAIL=
SCAN_RESULT=
RUN_URL=
REPO=
ISSUE_TMP=
ACTION=none
ISSUE=-
STATUS=incomplete

usage_error() {
  printf 'security-scan-issue: usage: %s\n' "$1" >&2
  exit 2
}

[[ $# -gt 0 ]] || usage_error 'render or sync is required'
COMMAND=$1
shift
case "$COMMAND" in render|sync) ;; *) usage_error 'unknown subcommand' ;; esac
while [[ $# -gt 0 ]]; do
  case "$COMMAND:$1" in
    render:--detail|render:--report|render:--out|sync:--report|sync:--scan-result|sync:--run-url|sync:--repo)
      [[ $# -ge 2 && -n $2 && $2 != --* ]] || usage_error 'option requires a value'
      case "$1" in
        --detail) DETAIL=$2 ;;
        --report) REPORT=$2 ;;
        --out) OUT=$2 ;;
        --scan-result) SCAN_RESULT=$2 ;;
        --run-url) RUN_URL=$2 ;;
        --repo) REPO=$2 ;;
      esac
      shift 2 ;;
    *) usage_error 'unknown option' ;;
  esac
done
[[ -n $REPORT ]] || usage_error '--report is required'
if [[ $COMMAND == render ]]; then
  [[ -n $OUT && -n $DETAIL ]] || usage_error '--detail and --out are required'
  case "$DETAIL" in counts|no-cells|full) ;; *) usage_error 'invalid detail' ;; esac
else
  [[ -n $SCAN_RESULT && -n $RUN_URL && -n $REPO ]] || usage_error '--scan-result, --run-url and --repo are required'
  case "$SCAN_RESULT" in success|failure|cancelled|skipped) ;; *) usage_error 'invalid scan result' ;; esac
  [[ $REPO =~ ^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$ ]] || usage_error 'invalid repository'
fi

# Read the input once. Missing, unreadable, or invalid reports must never look
# clean. Ruby's explicit encoding also works in the dispatcher's unset locale.
prepare_body() {
  ruby -E UTF-8:UTF-8 - "$COMMAND" "$DETAIL" "$REPORT" "$OUT" "$SCAN_RESULT" 2>/dev/null <<'RUBY'
command, detail, input, output, result = ARGV
header = /\A<!-- witself-security-scan v1 status=(clean|findings|incomplete) go=[0-9]+ npm_prod=[0-9]+ npm_toolchain=[0-9]+ images=[0-9]+ incomplete=[0-9]+ -->\z/
stub = "<!-- witself-security-scan v1 status=incomplete go=0 npm_prod=0 npm_toolchain=0 images=0 incomplete=1 -->\n# Witself security scan\nThe scan produced no valid report.\n"
begin
  text = File.read(input, encoding: 'UTF-8')
rescue SystemCallError, IOError
  text = nil
end
first = text && text.lines.first
match = first && text.valid_encoding? && header.match(first.delete_suffix("\n"))
if !match
  text = stub
elsif command == 'render'
  case detail
  when 'no-cells'
    text = text.gsub(/cell:[a-z0-9-]+/, 'pinned')
  when 'counts'
    lines = text.lines.drop(1).select { |line| line.match?(/\A(?:# |Time: |Commit: |## |Findings: |Incomplete targets: )/) }
    text = ([first.chomp] + lines.map(&:chomp) + ['Detail is not published. Run make security-scan locally.']).join("\n") + "\n"
  end
elsif match[1] == 'clean' && result != 'success'
  text = text.sub('status=clean', 'status=incomplete')
  text += "\n" unless text.end_with?("\n")
  text += "Scan job result: #{result}; this run is treated as incomplete.\n"
end
if command == 'sync' && text.bytesize > 65_000
  suffix = "Report truncated; see the run.\n"
  bounded = +''
  text.each_line do |line|
    break if bounded.bytesize + line.bytesize > 65_000 - suffix.bytesize
    bounded << line
  end
  text = bounded + suffix
end
File.write(output, text, encoding: 'UTF-8')
RUBY
}

if [[ $COMMAND == render ]]; then
  mkdir -p -- "$(dirname -- "$OUT")" 2>/dev/null || exit 1
  prepare_body || exit 1
  exit 0
fi

# Both gh streams stay private, including errors and issue-create's URL. Emit
# only this value-free action summary, even when an API call fails.
finish() {
  if [[ -n $ISSUE_TMP ]]; then rm -rf -- "$ISSUE_TMP" 2>/dev/null; fi
  printf 'security-scan-issue: action=%s issue=%s status=%s\n' "$ACTION" "$ISSUE" "$STATUS"
}
trap finish EXIT
trap 'exit 1' HUP INT TERM
ISSUE_TMP=$(mktemp -d "${TMPDIR:-/tmp}/witself-security-scan-issue.XXXXXX" 2>/dev/null) || exit 1
OUT=$ISSUE_TMP/body/report.md
mkdir -p -- "$(dirname -- "$OUT")" 2>/dev/null || exit 1
prepare_body || exit 1
IFS= read -r first_line < "$OUT" || exit 1
status_pattern='^<!-- witself-security-scan v1 status=(clean|findings|incomplete) '
[[ $first_line =~ $status_pattern ]] || exit 1
STATUS=${BASH_REMATCH[1]}

run_gh() { gh "$@" > "$ISSUE_TMP/gh.out" 2> "$ISSUE_TMP/gh.err"; }
run_gh issue list --repo "$REPO" --state open --label "$LABEL" --limit 100 --json number,title,body,author || exit 1
jq -r --arg title "$TITLE" '
  if type != "array" then error("invalid issue list") else . end
  | map(select(.title == $title
      and (.body | type == "string")
      and (.body | startswith("<!-- witself-security-scan "))
      and (.author.login == "app/github-actions" or .author.login == "github-actions[bot]")
      and (.number | type == "number") and .number > 0 and (.number | floor == .)))
  | unique_by(.number) | sort_by(.number) | .[].number
' "$ISSUE_TMP/gh.out" > "$ISSUE_TMP/matches" 2> "$ISSUE_TMP/parse.err" || exit 1
OTHERS=
while IFS= read -r number; do
  if [[ $ISSUE == - ]]; then ISSUE=$number
  elif [[ -z $OTHERS ]]; then OTHERS="#$number"
  else OTHERS="$OTHERS, #$number"
  fi
done < "$ISSUE_TMP/matches"

if [[ $STATUS == clean ]]; then
  if [[ $ISSUE != - ]]; then
    run_gh issue comment "$ISSUE" --repo "$REPO" --body "Clean scan: $RUN_URL" || exit 1
    run_gh issue close "$ISSUE" --repo "$REPO" --reason completed || exit 1
    ACTION=closed
  fi
elif [[ $ISSUE != - ]]; then
  run_gh issue edit "$ISSUE" --repo "$REPO" --body-file "$OUT" || exit 1
  comment="Scheduled scan $STATUS: $RUN_URL"
  if [[ -n $OTHERS ]]; then comment="$comment Other matching issues: $OTHERS."; fi
  run_gh issue comment "$ISSUE" --repo "$REPO" --body "$comment" || exit 1
  ACTION=edited
else
  run_gh label list --repo "$REPO" --json name --limit 200 || exit 1
  label_present=$(jq -r --arg label "$LABEL" 'if type == "array" then any(.[]; .name == $label) else error("invalid label list") end' "$ISSUE_TMP/gh.out" 2> "$ISSUE_TMP/parse.err") || exit 1
  if [[ $label_present == false ]]; then
    run_gh label create "$LABEL" --repo "$REPO" --description 'Security scan findings' --color B60205 || exit 1
  fi
  run_gh issue create --repo "$REPO" --title "$TITLE" --label "$LABEL" --body-file "$OUT" || exit 1
  # gh prints the created issue URL; require a numeric suffix before reporting it.
  IFS= read -r created_url < "$ISSUE_TMP/gh.out" || [[ -n $created_url ]] || exit 1
  ISSUE=${created_url##*/}
  [[ $ISSUE =~ ^[1-9][0-9]*$ ]] || { ISSUE=-; exit 1; }
  ACTION=created
fi
