#!/usr/bin/env bash
# On-demand network scanners. Only the offline contract suite belongs in a gate.
set -uo pipefail

GOVULNCHECK_VERSION=v1.7.0
NPM_PROD_AUDIT_LEVEL=moderate
NPM_TOOLCHAIN_AUDIT_LEVEL=high
LIVE_CELLS="civo-sandbox-use1-serving civo-sandbox-use1-backup"
IMAGE_FAIL_SEVERITIES="Critical High"
IMAGE_GO_MODULE_INFORMATIONAL_REPOSITORIES="ghcr.io/witwave-ai/images/witself-server"

export LC_ALL=C
SCRIPT_DIR=$(CDPATH='' cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)
ROOT=$(CDPATH='' cd -- "$SCRIPT_DIR/.." && pwd -P)
REPORT=''
SCAN_TMP=''
RUN_GO=0
RUN_NPM=0
RUN_IMAGES=0
TARGETS_ONLY=0
RELEASE_VERSION=''
REPO=witwave-ai/witself
GRYPE_PATH=''
IMAGE_COUNT=0
IMAGE_INFO=0
IMAGE_SCANNED=0
IMAGE_PLANNED=0
GO_COUNT=0
GO_INFO=0
GO_SCANNED=0
GO_PLANNED=0
NPM_PROD_COUNT=0
NPM_PROD_INFO=0
NPM_PROD_SCANNED=0
NPM_TOOLCHAIN_COUNT=0
NPM_TOOLCHAIN_INFO=0
NPM_TOOLCHAIN_SCANNED=0
NPM_PLANNED=0
INCOMPLETE=0
TOOLS=''

usage_error() {
  printf 'security-scan: usage: %s\n' "$1" >&2
  exit 2
}

help() {
  printf '%s\n' 'Usage: bash scripts/security-scan.sh <all|go,npm,images|targets> [--root DIR] [--report FILE]' \
    'Scopes select go, npm or images; duplicates are ignored. targets must be used alone.' \
    'Images/targets: --release-version X.Y.Z --repo OWNER/NAME; images: --grype PATH.' \
    'Exit: 0 clean, 1 findings, 2 usage error, 3 incomplete. Scanning uses the network.'
}

if [[ $# -eq 1 && ( $1 == --help || $1 == -h ) ]]; then help; exit 0; fi
[[ $# -gt 0 ]] || usage_error 'a scope is required'
scope=$1
original_scope=$scope
shift
[[ -n $scope && $scope != ,* && $scope != *, && $scope != *,,* ]] || usage_error 'empty scope element'
while :; do
  element=${scope%%,*}
  case "$element" in
    all) [[ $original_scope == all ]] || usage_error 'all must be used alone'; RUN_GO=1; RUN_NPM=1; RUN_IMAGES=1 ;;
    go) RUN_GO=1 ;;
    npm) RUN_NPM=1 ;;
    images) RUN_IMAGES=1 ;;
    targets) [[ $original_scope == targets ]] || usage_error 'targets must be used alone'; TARGETS_ONLY=1 ;;
    *) usage_error 'unknown scope element' ;;
  esac
  [[ $scope == *,* ]] || break
  scope=${scope#*,}
done
while [[ $# -gt 0 ]]; do
  case "$1" in
    --root|--report|--release-version|--repo|--grype)
      [[ $# -ge 2 && -n $2 && $2 != --* ]] || usage_error 'option requires a value'
      case "$1" in
        --root) ROOT=$2 ;;
        --report) REPORT=$2 ;;
        --release-version) [[ $2 =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]] || usage_error 'invalid release version'; RELEASE_VERSION=$2 ;;
        --repo) [[ $2 =~ ^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$ ]] || usage_error 'invalid repository'; REPO=$2 ;;
        --grype) [[ -f $2 && -x $2 ]] || usage_error 'grype must be an executable file'; GRYPE_PATH=$2; [[ $GRYPE_PATH == /* ]] || GRYPE_PATH=$PWD/$GRYPE_PATH ;;
      esac
      shift 2 ;;
    -h|--help) help; exit 0 ;;
    *) usage_error 'unknown option' ;;
  esac
done
[[ -d $ROOT ]] || usage_error 'root is not a directory'
ROOT=$(CDPATH='' cd -- "$ROOT" 2>/dev/null && pwd -P) || usage_error 'root is not a directory'

render_header() {
  local commit
  commit=$(git -C "$ROOT" rev-parse HEAD 2>/dev/null) || commit=unknown
  # SHA-256 repositories must not put a full object id into the report.
  if [[ ! $commit =~ ^[0-9a-f]{40}$ ]]; then commit=unknown; fi
  printf '<!-- witself-security-scan v1 status=%s go=%s npm_prod=%s npm_toolchain=%s images=%s incomplete=%s -->\n' "$STATUS" "$GO_COUNT" "$NPM_PROD_COUNT" "$NPM_TOOLCHAIN_COUNT" "$IMAGE_COUNT" "$INCOMPLETE"
  printf '# Witself security scan\n\nTime: %s\nCommit: %s\nTools: %s\n' "$(date -u '+%Y-%m-%dT%H:%M:%SZ')" "$commit" "${1:-}"
}

print_summary() {
  printf 'security-scan: status=%s go=%s npm_prod=%s npm_toolchain=%s images=%s incomplete=%s\n' "$STATUS" "$GO_COUNT" "$NPM_PROD_COUNT" "$NPM_TOOLCHAIN_COUNT" "$IMAGE_COUNT" "$INCOMPLETE" >&2
}

# A signal can interrupt a child before its output can be validated. Report only
# metadata, even if earlier targets produced complete findings.
# shellcheck disable=SC2329
interrupted() {
  trap '' HUP INT TERM
  INCOMPLETE=$((INCOMPLETE + 1))
  STATUS=incomplete
  if [[ -n $REPORT ]]; then
    if ! { mkdir -p -- "$(dirname -- "$REPORT")" && render_header > "$REPORT"; } 2>/dev/null; then
      INCOMPLETE=$((INCOMPLETE + 1))
    fi
  fi
  if [[ $TARGETS_ONLY -eq 1 ]]; then
    printf 'security-scan: targets incomplete: scanner failed\n' >&2
  else
    print_summary
  fi
  exit 3
}
trap interrupted HUP INT TERM

# Invoked by the EXIT trap, including under Bash 3.2.
# shellcheck disable=SC2329
cleanup() {
  if [[ -n $SCAN_TMP && -d $SCAN_TMP ]]; then rm -rf -- "$SCAN_TMP"; fi
}
trap cleanup EXIT
umask 077
SCAN_TMP=$(mktemp -d "${TMPDIR:-/tmp}/witself-security-scan.XXXXXX" 2>/dev/null) || {
  if [[ $TARGETS_ONLY -eq 1 ]]; then
    printf 'security-scan: targets incomplete: scanner failed\n' >&2
  else
    printf 'security-scan: status=incomplete go=0 npm_prod=0 npm_toolchain=0 images=0 incomplete=1\n' >&2
  fi
  exit 3
}
for file in go.rows go.versions npm_prod.rows npm_toolchain.rows incomplete.rows images.rows images.targets images.versions; do
  : > "$SCAN_TMP/$file"
done

incomplete() {
  # Targets are our relative names; diagnostics never include child output.
  local target=$1
  target=${target//$'\n'/ }
  target=${target//$'\r'/ }
  target=${target//$'\t'/ }
  printf '%s\t%s\n' "$target" "$2" >> "$SCAN_TMP/incomplete.rows"
  INCOMPLETE=$((INCOMPLETE + 1))
}

missing_tools() {
  local tool
  MISSING=''
  for tool in "$@"; do
    if ! command -v "$tool" >/dev/null 2>&1; then MISSING=$tool; return; fi
  done
}

discover_go() {
  local top path candidate index shift_index
  MODULES=()
  top=$(git -C "$ROOT" rev-parse --show-toplevel 2>/dev/null) || top=''
  if [[ $top == "$ROOT" ]]; then
    git -C "$ROOT" ls-files -z --cached --others --exclude-standard > "$SCAN_TMP/files" 2>/dev/null || return 1
  else
    (cd "$ROOT" && find . -type d \( -name testdata -o -name node_modules -o -name .git -o -name .claude \) -prune -o -type f -name go.mod -print0) > "$SCAN_TMP/files" 2>/dev/null || return 1
  fi
  while IFS= read -r -d '' path; do
    path=${path#./}
    [[ $path == go.mod || $path == */go.mod ]] || continue
    case "/$path/" in */testdata/*|*/node_modules/*|*/.git/*|*/.claude/*) continue ;; esac
    [[ -f $ROOT/$path ]] || continue
    candidate=${path%go.mod}
    candidate=${candidate%/}
    [[ -n $candidate ]] || candidate=.
    # Insertion sort preserves NUL-delimited discovery even for unusual filenames.
    index=${#MODULES[@]}
    while [[ $index -gt 0 && $candidate < ${MODULES[$((index - 1))]} ]]; do
      index=$((index - 1))
    done
    # Unmerged index stages can list the same path more than once.
    if [[ $index -gt 0 && $candidate == "${MODULES[$((index - 1))]}" ]]; then continue; fi
    shift_index=${#MODULES[@]}
    while [[ $shift_index -gt $index ]]; do
      MODULES[shift_index]=${MODULES[$((shift_index - 1))]}
      shift_index=$((shift_index - 1))
    done
    MODULES[index]=$candidate
  done < "$SCAN_TMP/files"
}

scan_go() {
  local module count info version
  TOOLS="govulncheck $GOVULNCHECK_VERSION"
  if ! discover_go; then incomplete go 'invalid output'; return; fi
  GO_PLANNED=${#MODULES[@]}
  if [[ $GO_PLANNED -eq 0 ]]; then incomplete go 'no targets'; return; fi
  missing_tools go jq
  for module in ${MODULES[@]+"${MODULES[@]}"}; do
    if [[ -n $MISSING ]]; then incomplete "$module" "missing tool: $MISSING"; continue; fi
    if ! (cd "$ROOT/$module" && go run "golang.org/x/vuln/cmd/govulncheck@${GOVULNCHECK_VERSION}" -format json ./...) > "$SCAN_TMP/go.stdout" 2> "$SCAN_TMP/go.stderr"; then
      incomplete "$module" 'scanner failed'
      continue
    fi
    if ! jq -es --arg module "$module" '
      def called: (.trace[0].function? | type == "string" and length > 0);
      if (any(.[]; .config? | type == "object" and .scanner_name == "govulncheck"
            and (.go_version | type == "string" and length > 0) and .scan_level == "symbol"))
         and all(.[]; type == "object")
         and all(.[] | select(has("finding")) | .finding;
           type == "object" and (.osv | type == "string" and length > 0) and (.trace | type == "array"))
      then . else error("invalid output") end
      | [ .[] | .finding? // empty ] as $findings
      | {versions: [.[] | .config? | select(.scanner_name == "govulncheck") | .go_version] | unique,
         info: ([$findings | group_by(.osv)[] | select(any(.[]; called) | not)] | length),
         rows: [$findings | group_by(.osv)[] | map(select(called)) | select(length > 0)
           | sort_by(.trace[0].module, .trace[0].package, .trace[0].function) | .[0]
           | [$module, .osv, (.trace[0].module // "none"), (.trace[0].package // "none"),
              (.trace[0].version // "none"), (.fixed_version // "none")]]}
    ' "$SCAN_TMP/go.stdout" > "$SCAN_TMP/go.parsed" 2> "$SCAN_TMP/go.parse.stderr"; then
      incomplete "$module" 'invalid output'
      continue
    fi
    count=$(jq '.rows | length' "$SCAN_TMP/go.parsed")
    info=$(jq '.info' "$SCAN_TMP/go.parsed")
    jq -r '.rows[] | map(tostring) | @tsv' "$SCAN_TMP/go.parsed" >> "$SCAN_TMP/go.rows"
    jq -r '.versions[]' "$SCAN_TMP/go.parsed" >> "$SCAN_TMP/go.versions"
    GO_COUNT=$((GO_COUNT + count))
    GO_INFO=$((GO_INFO + info))
    GO_SCANNED=$((GO_SCANNED + 1))
  done
  version=$(sort -u "$SCAN_TMP/go.versions" | tr '\n' ',')
  TOOLS="$TOOLS (go_version ${version%,})"
}

scan_npm_audit() {
  local directory=$1 kind=$2 level=$3 count info
  local flags=()
  if [[ $kind == prod ]]; then flags=(--omit=dev); fi
  # npm uses nonzero for findings as well as failures; only the JSON is authoritative.
  (cd "$ROOT/$directory" && npm audit --package-lock-only --json ${flags[@]+"${flags[@]}"}) > "$SCAN_TMP/npm.stdout" 2> "$SCAN_TMP/npm.stderr" || :
  if ! jq -es --arg directory "$directory" --arg kind "$kind" --arg level "$level" --slurpfile prod "$SCAN_TMP/npm.prod" '
    def rank: {info:0, low:1, moderate:2, high:3, critical:4}[.];
    if length == 1 then .[0] else error("invalid output") end
    | if type == "object" and (has("error") | not) and .auditReportVersion == 2
       and (.metadata.vulnerabilities | type == "object")
       and (.vulnerabilities | type == "object")
       and ($kind == "prod" or (.metadata.dependencies.total | type == "number" and . >= 1))
       and all(.vulnerabilities[]; type == "object" and (.severity | rank) != null and (.via | type == "array"))
    then . else error("invalid output") end
    | .vulnerabilities | to_entries | sort_by(.key)
    | {info: ([.[] | select((.value.severity | rank) < ($level | rank))] | length),
       rows: [.[] | select((.value.severity | rank) >= ($level | rank))
         | select($kind == "prod" or (.key as $name | $prod[0] | index($name) | not))
         | [$directory, .key, .value.severity,
            ([.value.via[] | objects | .url? | select(type == "string" and length > 0) | split("/")[-1]]
             | unique | if length == 0 then "transitive" else join(",") end),
            (if .value.fixAvailable == true or (.value.fixAvailable | type == "object") then "yes" else "no" end)]]}
  ' "$SCAN_TMP/npm.stdout" > "$SCAN_TMP/npm.parsed" 2> "$SCAN_TMP/npm.parse.stderr"; then
    incomplete "$directory (npm $kind)" 'invalid output'
    return
  fi
  count=$(jq '.rows | length' "$SCAN_TMP/npm.parsed")
  info=$(jq '.info' "$SCAN_TMP/npm.parsed")
  if [[ $kind == prod ]]; then
    jq '.rows | map(.[1])' "$SCAN_TMP/npm.parsed" > "$SCAN_TMP/npm.prod"
    jq -r '.rows[] | map(tostring) | @tsv' "$SCAN_TMP/npm.parsed" >> "$SCAN_TMP/npm_prod.rows"
    NPM_PROD_COUNT=$((NPM_PROD_COUNT + count))
    NPM_PROD_INFO=$((NPM_PROD_INFO + info))
    NPM_PROD_SCANNED=$((NPM_PROD_SCANNED + 1))
  else
    jq -r '.rows[] | map(tostring) | @tsv' "$SCAN_TMP/npm.parsed" >> "$SCAN_TMP/npm_toolchain.rows"
    NPM_TOOLCHAIN_COUNT=$((NPM_TOOLCHAIN_COUNT + count))
    NPM_TOOLCHAIN_INFO=$((NPM_TOOLCHAIN_INFO + info))
    NPM_TOOLCHAIN_SCANNED=$((NPM_TOOLCHAIN_SCANNED + 1))
  fi
}

scan_npm() {
  local package directory version='unknown'
  local directories=()
  for package in "$ROOT"/infra/cloudflare/*/package.json; do
    [[ -f $package ]] || continue
    directory=${package%/package.json}
    directories[${#directories[@]}]=${directory#"$ROOT/"}
  done
  NPM_PLANNED=${#directories[@]}
  missing_tools npm jq
  if command -v npm >/dev/null 2>&1; then
    if npm --version > "$SCAN_TMP/npm.version" 2> "$SCAN_TMP/npm.version.stderr"; then
      IFS= read -r version < "$SCAN_TMP/npm.version" || :
    fi
  fi
  TOOLS="${TOOLS}${TOOLS:+; }npm $version"
  if [[ $NPM_PLANNED -eq 0 ]]; then incomplete npm 'no targets'; return; fi
  for directory in ${directories[@]+"${directories[@]}"}; do
    if [[ -n $MISSING ]]; then incomplete "$directory" "missing tool: $MISSING"; continue; fi
    if [[ ! -f $ROOT/$directory/package-lock.json ]]; then incomplete "$directory" 'no lockfile'; continue; fi
    printf '[]\n' > "$SCAN_TMP/npm.prod"
    scan_npm_audit "$directory" prod "$NPM_PROD_AUDIT_LEVEL"
    scan_npm_audit "$directory" toolchain "$NPM_TOOLCHAIN_AUDIT_LEVEL"
  done
}

discover_images() {
  local tag cell reason
  missing_tools jq ruby
  if [[ -n $MISSING ]]; then incomplete images "missing tool: $MISSING"; return; fi
  if [[ -z $RELEASE_VERSION ]]; then
    missing_tools gh
    if [[ -n $MISSING ]]; then
      incomplete release 'missing tool: gh'
    elif ! gh release view --repo "$REPO" --json tagName --jq .tagName > "$SCAN_TMP/release.stdout" 2> "$SCAN_TMP/release.stderr"; then
      incomplete release 'release lookup failed'
    else
      tag=$(cat "$SCAN_TMP/release.stdout")
      if [[ $tag =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
        RELEASE_VERSION=${tag#v}
      else
        incomplete release 'release lookup failed'
      fi
    fi
  fi
  # Full refs remain private. Ruby parses data, never evaluates cell content.
  if ! ruby -E UTF-8:UTF-8 -ryaml -rjson - "$ROOT" "$LIVE_CELLS" "$RELEASE_VERSION" > "$SCAN_TMP/images.plan" 2> "$SCAN_TMP/images.plan.stderr" <<'RUBY'
root, cells, version = ARGV
server_repo = 'ghcr.io/witwave-ai/images/witself-server'
backup_repo = 'ghcr.io/witwave-ai/images/witself-postgres-backup'
targets, errors, pinned_tags, refs = [], [], [], []
def entry(source, repository, tag, digest)
  raise 'invalid repository' unless repository.is_a?(String) && repository.match?(/\A[a-z0-9][a-z0-9._\/-]*\z/)
  raise 'invalid tag' unless tag.is_a?(String) && tag.match?(/\A[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}\z/)
  raise 'invalid digest' unless digest.nil? || digest == '' || (digest.is_a?(String) && digest.match?(/\Asha256:[0-9a-f]{64}\z/))
  digest = '-' if digest.nil? || digest == ''
  [source, repository, tag, digest]
end
cells.split.each do |cell|
  begin
    values = YAML.safe_load(File.read(File.join(root, '.gitops/cells', cell, 'values.yaml'), encoding: 'UTF-8'), aliases: false)
    apps = values.fetch('apps')
    candidates = []
    server = apps.fetch('witselfServer', {})
    if server['enabled'] == true
      candidates << entry("cell:#{cell}", server_repo, server['imageTag'], server['imageDigest'])
    end
    backup = apps.fetch('civoPostgres', {}).fetch('backup', {})
    if backup['enabled'] == true
      image = backup['image']
      if image.nil?
        errors << ["cell:#{cell} backup", 'unpinned backup image']
      else
        # A bad backup image must not discard the cell's server target.
        begin
          if image.is_a?(String)
            repository, separator, tag = image.rpartition(':')
            raise 'invalid backup image' if separator.empty?
            candidates << entry("cell:#{cell}", repository, tag, nil)
          elsif image.is_a?(Hash)
            candidates << entry("cell:#{cell}", image['repository'], image['tag'], image['digest'])
          else
            raise 'invalid backup image'
          end
        rescue StandardError
          errors << ["cell:#{cell} backup", 'invalid backup image']
        end
      end
    end
    candidates.each do |candidate|
      _, repository, tag, digest = candidate
      pinned_tags << "#{repository}:#{tag}"
      ref = digest == '-' ? "#{repository}:#{tag}" : "#{repository}@#{digest}"
      next if refs.include?(ref)
      refs << ref
      targets << candidate
    end
  rescue StandardError
    errors << ["cell:#{cell}", 'missing values']
  end
end
unless version.empty?
  [server_repo, backup_repo].each do |repository|
    next if pinned_tags.include?("#{repository}:#{version}")
    targets << entry('release', repository, version, nil)
  end
end
puts JSON.generate({targets: targets, errors: errors})
RUBY
  then incomplete images 'invalid output'; return; fi
  while IFS=$'\t' read -r cell reason; do
    incomplete "$cell" "$reason"
  done < <(jq -r '.errors[] | @tsv' "$SCAN_TMP/images.plan")
  jq -r '.targets[] | @tsv' "$SCAN_TMP/images.plan" > "$SCAN_TMP/images.refs"
  jq -r '.targets[] | .[3] |= (if . == "-" then . else .[0:19] end) | @tsv' "$SCAN_TMP/images.plan" > "$SCAN_TMP/images.targets"
  IMAGE_PLANNED=$(jq '.targets | length' "$SCAN_TMP/images.plan")
  if [[ $IMAGE_PLANNED -eq 0 && $INCOMPLETE -eq 0 ]]; then incomplete images 'no targets'; fi
}

read_grype_pin() {
  local line versions=0 linux=0 darwin=0
  GRYPE_VERSION=''
  GRYPE_LINUX_PIN=''
  GRYPE_DARWIN_PIN=''
  [[ -f $SCRIPT_DIR/security-scan-grype.env && -r $SCRIPT_DIR/security-scan-grype.env ]] || return 1
  while IFS= read -r line || [[ -n $line ]]; do
    if [[ $line =~ ^GRYPE_VERSION=[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
      GRYPE_VERSION=${line#*=}; versions=$((versions + 1))
    elif [[ $line =~ ^GRYPE_LINUX_AMD64_SHA256=[0-9a-f]{64}$ ]]; then
      GRYPE_LINUX_PIN=${line#*=}; linux=$((linux + 1))
    elif [[ $line =~ ^GRYPE_DARWIN_ARM64_SHA256=[0-9a-f]{64}$ ]]; then
      GRYPE_DARWIN_PIN=${line#*=}; darwin=$((darwin + 1))
    else
      return 1
    fi
  done < "$SCRIPT_DIR/security-scan-grype.env"
  [[ $versions -eq 1 && $linux -eq 1 && $darwin -eq 1 ]]
}

prepare_grype() {
  local system architecture asset expected archive actual checksum_tool
  GRYPE_ERROR=''
  if ! read_grype_pin; then GRYPE_ERROR='missing tool: grype (invalid pin file)'; return; fi
  if [[ -n $GRYPE_PATH ]]; then GRYPE_BIN=$GRYPE_PATH; return; fi
  missing_tools uname curl tar
  if [[ -n $MISSING ]]; then GRYPE_ERROR="missing tool: $MISSING"; return; fi
  if command -v sha256sum >/dev/null 2>&1; then
    checksum_tool=sha256sum
  elif command -v shasum >/dev/null 2>&1; then
    checksum_tool=shasum
  else
    GRYPE_ERROR='missing tool: sha256sum or shasum'; return
  fi
  system=$(uname -s 2> "$SCAN_TMP/uname.stderr") || system=''
  architecture=$(uname -m 2>> "$SCAN_TMP/uname.stderr") || architecture=''
  case "$system $architecture" in
    'Linux x86_64') asset=linux_amd64; expected=$GRYPE_LINUX_PIN ;;
    'Darwin arm64') asset=darwin_arm64; expected=$GRYPE_DARWIN_PIN ;;
    *) GRYPE_ERROR='missing tool: grype (unsupported platform; pass --grype PATH)'; return ;;
  esac
  archive="$SCAN_TMP/grype_${GRYPE_VERSION}_${asset}.tar.gz"
  if ! curl --fail --silent --show-error --location --output "$archive" "https://github.com/anchore/grype/releases/download/v${GRYPE_VERSION}/grype_${GRYPE_VERSION}_${asset}.tar.gz" > "$SCAN_TMP/curl.stdout" 2> "$SCAN_TMP/curl.stderr"; then
    GRYPE_ERROR='scanner failed'; return
  fi
  if [[ $checksum_tool == sha256sum ]]; then
    sha256sum "$archive" > "$SCAN_TMP/checksum" 2> "$SCAN_TMP/checksum.stderr" || { GRYPE_ERROR='checksum mismatch'; return; }
  else
    shasum -a 256 "$archive" > "$SCAN_TMP/checksum" 2> "$SCAN_TMP/checksum.stderr" || { GRYPE_ERROR='checksum mismatch'; return; }
  fi
  IFS=' ' read -r actual _ < "$SCAN_TMP/checksum"
  if [[ $actual != "$expected" ]]; then GRYPE_ERROR='checksum mismatch'; return; fi
  if ! tar -xzf "$archive" -C "$SCAN_TMP" grype > "$SCAN_TMP/tar.stdout" 2> "$SCAN_TMP/tar.stderr"; then
    GRYPE_ERROR='scanner failed'; return
  fi
  GRYPE_BIN=$SCAN_TMP/grype
  if [[ ! -f $GRYPE_BIN || ! -x $GRYPE_BIN || -L $GRYPE_BIN ]]; then GRYPE_ERROR='scanner failed'; fi
}

scan_images() {
  local source repository tag digest ref display count info version supplied=''
  discover_images
  [[ $IMAGE_PLANNED -gt 0 ]] || return
  prepare_grype
  while IFS=$'\t' read -r source repository tag digest; do
    if [[ -n $GRYPE_ERROR ]]; then incomplete "$source $repository:$tag" "$GRYPE_ERROR"; continue; fi
    ref=$repository:$tag
    display=$ref
    if [[ $digest != - ]]; then ref=$repository@$digest; display="$display@${digest:0:19}"; fi
    if ! GRYPE_CHECK_FOR_APP_UPDATE=false GRYPE_DB_CACHE_DIR="${GRYPE_DB_CACHE_DIR-$SCAN_TMP}" \
      "$GRYPE_BIN" "registry:$ref" --platform linux/amd64 --output json --quiet > "$SCAN_TMP/grype.stdout" 2> "$SCAN_TMP/grype.stderr"; then
      incomplete "$source $repository:$tag" 'scanner failed'; continue
    fi
    if ! jq -es --arg source "$source" --arg image "$display" --arg repository "$repository" \
      --arg severities "$IMAGE_FAIL_SEVERITIES" --arg informational "$IMAGE_GO_MODULE_INFORMATIONAL_REPOSITORIES" '
      if length == 1 then .[0] else error("invalid output") end
      | if .descriptor.name == "grype" and .source.type == "image" and (.matches | type == "array")
        then . else error("invalid output") end
      | def finding:
          (.vulnerability.severity as $severity | $severities | split(" ") | index($severity)) != null
          and .vulnerability.fix.state == "fixed"
          and ((.artifact.type == "go-module" and ($informational | split(" ") | index($repository)) != null) | not);
        {version: (.descriptor.version // "unknown"),
         info: ([.matches[] | select(finding | not)] | length),
         rows: [.matches[] | select(finding)
           | [$source, $image, .vulnerability.id, .vulnerability.severity,
              .artifact.name, .artifact.version, .artifact.type, ((.vulnerability.fix.versions // []) | join(","))]]}
    ' "$SCAN_TMP/grype.stdout" > "$SCAN_TMP/grype.parsed" 2> "$SCAN_TMP/grype.parse.stderr"; then
      incomplete "$source $repository:$tag" 'invalid output'; continue
    fi
    count=$(jq '.rows | length' "$SCAN_TMP/grype.parsed")
    info=$(jq '.info' "$SCAN_TMP/grype.parsed")
    jq -r '.rows[] | map(tostring) | @tsv' "$SCAN_TMP/grype.parsed" >> "$SCAN_TMP/images.rows"
    jq -r '.version | tostring' "$SCAN_TMP/grype.parsed" >> "$SCAN_TMP/images.versions"
    IMAGE_COUNT=$((IMAGE_COUNT + count))
    IMAGE_INFO=$((IMAGE_INFO + info))
    IMAGE_SCANNED=$((IMAGE_SCANNED + 1))
  done < "$SCAN_TMP/images.refs"
  version=$(sort -u "$SCAN_TMP/images.versions" | tr '\n' ',')
  if [[ -n $GRYPE_PATH ]]; then supplied=' (caller-supplied)'; fi
  TOOLS="${TOOLS}${TOOLS:+; }Grype ${version%,}$supplied"
}

# Only selected fields enter the report. Escape Markdown/control characters,
# remove private paths and token-shaped text, and bound even hostile field values.
safe_text() {
  local value=$1
  value=${value//"$ROOT"/[root]}
  value=${value//"$SCAN_TMP"/[temporary]}
  if [[ -n ${GH_TOKEN:-} ]]; then value=${value//"$GH_TOKEN"/[redacted]}; fi
  value=${value//|/'&#124;'}
  value=${value//</'&lt;'}
  value=${value//>/'&gt;'}
  printf '%s' "$value" | sed -E 's/[[:xdigit:]]{64}/[digest-redacted]/g; s/(gh[pousr]_|github_pat_)[A-Za-z0-9_]+/[token-redacted]/g; s@(^|[[:space:]])/[^[:space:]]+@\1[path-redacted]@g' | tr '\001-\010\013-\037\177' ' '
}

# Global byte budget leaves ample room for metadata, headings and count lines.
# Later scanners can use the same table renderer without exceeding the bound.
ROW_BYTES=0
render_table() {
  local file=$1 header=$2 separator=$3 count=$4 width=${5:-64} row rendered printed=0
  if [[ $count -eq 0 ]]; then printf 'None.\n'; return; fi
  printf '%s\n%s\n' "$header" "$separator"
  while IFS= read -r row; do
    if [[ $printed -ge 50 ]]; then break; fi
    # Sanitize before truncating so a long private value cannot leave a prefix.
    # TSV escapes embedded control characters; only our delimiters remain tabs.
    rendered=$(safe_text "$row" | awk -v width="$width" -F '\t' '{
      for (i = 1; i <= NF; i++) {
        if (i > 1) printf " | ";
        if (length($i) > width) printf "%s...", substr($i, 1, width - 3);
        else printf "%s", $i;
      }
    }')
    [[ $((ROW_BYTES + ${#rendered} + 5)) -le 54000 ]] || break
    printf '| %s |\n' "$rendered"
    ROW_BYTES=$((ROW_BYTES + ${#rendered} + 5))
    printed=$((printed + 1))
  done < "$file"
  if [[ $printed -lt $count ]]; then printf '\n… and %s more\n' "$((count - printed))"; fi
}

render_report() {
  render_header "$(safe_text "$TOOLS" | cut -c 1-1024)"
  if [[ $RUN_GO -eq 1 ]]; then
    printf '\n## Go (govulncheck, called vulnerabilities)\n\n'
    render_table "$SCAN_TMP/go.rows" '| Module | ID | Dependency | Package | Version | Fixed in |' '| --- | --- | --- | --- | --- | --- |' "$GO_COUNT"
    printf '\nFindings: %s. Informational: %s. Scanned: %s of %s.\n' "$GO_COUNT" "$GO_INFO" "$GO_SCANNED" "$GO_PLANNED"
  fi
  if [[ $RUN_NPM -eq 1 ]]; then
    printf '\n## npm production dependencies (moderate or above)\n\n'
    render_table "$SCAN_TMP/npm_prod.rows" '| Directory | Package | Severity | Advisories | Fix available |' '| --- | --- | --- | --- | --- |' "$NPM_PROD_COUNT"
    printf '\nFindings: %s. Informational: %s. Scanned: %s of %s.\n' "$NPM_PROD_COUNT" "$NPM_PROD_INFO" "$NPM_PROD_SCANNED" "$NPM_PLANNED"
    printf '\n## npm toolchain (high or above)\n\n'
    render_table "$SCAN_TMP/npm_toolchain.rows" '| Directory | Package | Severity | Advisories | Fix available |' '| --- | --- | --- | --- | --- |' "$NPM_TOOLCHAIN_COUNT"
    printf '\nFindings: %s. Informational: %s. Scanned: %s of %s.\n' "$NPM_TOOLCHAIN_COUNT" "$NPM_TOOLCHAIN_INFO" "$NPM_TOOLCHAIN_SCANNED" "$NPM_PLANNED"
  fi
  if [[ $RUN_IMAGES -eq 1 ]]; then
    printf '\n## Images (Critical or High with a fix)\n\n<!-- targets:begin -->\n'
    render_table "$SCAN_TMP/images.targets" '| Source | Image | Tag | Digest |' '| --- | --- | --- | --- |' "$IMAGE_PLANNED"
    printf '<!-- targets:end -->\n\n'
    render_table "$SCAN_TMP/images.rows" '| Source | Image | ID | Severity | Package | Version | Type | Fixed in |' '| --- | --- | --- | --- | --- | --- | --- | --- |' "$IMAGE_COUNT" 128
    printf '\nFindings: %s. Informational: %s. Scanned: %s of %s.\n' "$IMAGE_COUNT" "$IMAGE_INFO" "$IMAGE_SCANNED" "$IMAGE_PLANNED"
  fi
  if [[ $INCOMPLETE -gt 0 ]]; then
    printf '\n## Incomplete\n\n'
    render_table "$SCAN_TMP/incomplete.rows" '| Target | Reason |' '| --- | --- |' "$INCOMPLETE" 128
    printf '\nIncomplete targets: %s.\n' "$INCOMPLETE"
  fi
}

if [[ $TARGETS_ONLY -eq 1 ]]; then
  discover_images
  if [[ $INCOMPLETE -gt 0 ]]; then
    IFS=$'\t' read -r _ reason < "$SCAN_TMP/incomplete.rows"
    printf 'security-scan: targets incomplete: %s\n' "$reason" >&2
    exit 3
  fi
  while IFS= read -r target; do safe_text "$target"; printf '\n'; done < "$SCAN_TMP/images.targets"
  exit 0
fi
if [[ $RUN_GO -eq 1 ]]; then scan_go; fi
if [[ $RUN_NPM -eq 1 ]]; then scan_npm; fi
if [[ $RUN_IMAGES -eq 1 ]]; then scan_images; fi
STATUS=clean
EXIT_CODE=0
if [[ $((GO_COUNT + NPM_PROD_COUNT + NPM_TOOLCHAIN_COUNT + IMAGE_COUNT)) -gt 0 ]]; then STATUS=findings; EXIT_CODE=1; fi
if [[ $INCOMPLETE -gt 0 ]]; then STATUS=incomplete; EXIT_CODE=3; fi
render_report > "$SCAN_TMP/report.md"
if [[ -n $REPORT ]]; then
  if ! { mkdir -p -- "$(dirname -- "$REPORT")" && cat "$SCAN_TMP/report.md" > "$REPORT"; } 2> "$SCAN_TMP/report.stderr"; then
    STATUS=incomplete
    EXIT_CODE=3
    INCOMPLETE=$((INCOMPLETE + 1))
  fi
else
  cat "$SCAN_TMP/report.md"
fi
print_summary
exit "$EXIT_CODE"
