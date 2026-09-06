#!/usr/bin/env bash
# Live operator harness. Offline verification MUST put witself and curl shims on PATH.
# This creates a synthetic agent and one billed backup object; it never commits a restore.
# shellcheck disable=SC2016 # jq expressions intentionally contain jq variables.
set -Eeuo pipefail
umask 077

usage() {
  cat <<'USAGE'
usage: run-avatar-acceptance.sh --account BINDING --realm-id ID --agent NAME
  --control-plane URL --fleet-token-file FILE --drill-cell civo-sandbox-use1-backup
  --work DIR --out RECORD --redact-check [--rehearsal]
       run-avatar-acceptance.sh --redact-check --out EXISTING_RECORD

The live mode creates a fresh synthetic agent, irreversibly compacts its old avatar
payloads, and requests one additional billing-bearing backup object. The restore
drill only validates and rolls back on the isolated backup cell. Use a fresh empty
0700 work directory outside every Git worktree. The record is written 0600; all
private temporary inputs, command output, tokens and the whole-account export are
deleted on exit. An interrupted run must use a new synthetic agent name.
--rehearsal still executes every live leg; it never certifies the release pair.
USAGE
}
bad_usage() { printf '%s\n' 'avatar acceptance: invalid arguments (use --help)' >&2; exit 2; }
account='' realm_id='' agent_name='' control_plane='' fleet_token_file='' drill_cell='' work='' out=''
redact_check=false rehearsal=false
while (($#)); do
  case "$1" in
    --help|-h) usage; exit 0 ;;
    --redact-check) [[ $redact_check == false ]] || bad_usage; redact_check=true; shift ;;
    --rehearsal) [[ $rehearsal == false ]] || bad_usage; rehearsal=true; shift ;;
    --account|--realm-id|--agent|--control-plane|--fleet-token-file|--drill-cell|--work|--out)
      (($# >= 2)) && [[ -n $2 && $2 != --* ]] || bad_usage
      case "$1" in
        --account) [[ -z $account ]] || bad_usage; account=$2 ;;
        --realm-id) [[ -z $realm_id ]] || bad_usage; realm_id=$2 ;;
        --agent) [[ -z $agent_name ]] || bad_usage; agent_name=$2 ;;
        --control-plane) [[ -z $control_plane ]] || bad_usage; control_plane=${2%/} ;;
        --fleet-token-file) [[ -z $fleet_token_file ]] || bad_usage; fleet_token_file=$2 ;;
        --drill-cell) [[ -z $drill_cell ]] || bad_usage; drill_cell=$2 ;;
        --work) [[ -z $work ]] || bad_usage; work=$2 ;;
        --out) [[ -z $out ]] || bad_usage; out=$2 ;;
      esac
      shift 2 ;;
    *) bad_usage ;;
  esac
done
command -v jq >/dev/null || { printf '%s\n' 'avatar acceptance: jq is required' >&2; exit 1; }

# A strict retained schema is the content boundary. No raw response object, receipt,
# path, URL, hash, free-form detail or creative payload can pass this check.
check_record() {
  jq -e '
    def keys_only($allowed): type == "object" and ((keys - $allowed)|length == 0);
    def timestamp: type == "string" and test("^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}(\\.[0-9]+)?(Z|[+-]([01][0-9]|2[0-3]):[0-5][0-9])$");
    def build: keys_only(["version","commit","date"]) and
      ([.version,.commit,.date] | all(type == "string")) and
      (.version|test("^([0-9]+\\.[0-9]+\\.[0-9]+)?$")) and
      (.commit|test("^([0-9a-f]{7,40})?$")) and
      (.date == "" or (.date|timestamp));
    def count: type == "number" and . >= 0 and floor == .;
    keys_only(["schema_version","run_id","status","certification_eligible","witself","cli","cell","account_id","realm_id","agent_id","legs","archive","backup"]) and
    .schema_version == "witself.avatar-acceptance.v1" and
    (.status == "passed" or .status == "failed" or .status == "rehearsal") and
    (.certification_eligible|type == "boolean") and
    (.witself|build) and (.cli|build) and
    (.run_id|type == "string" and test("^avatar-[A-Za-z0-9-]+$")) and
    (.cell|type == "string" and test("^([a-z0-9][a-z0-9-]+)?$")) and
    (.account_id|type == "string" and test("^(acc_[a-z0-9]+)?$")) and
    (.realm_id|type == "string" and test("^realm_[a-z0-9]+$")) and
    (.agent_id|type == "string" and test("^(agent_[a-z0-9]+)?$")) and
    (.legs|type == "array") and
    (.legs|all(keys_only(["name","passed","detail","versions","profile_revision","lineage_generation","at"]) and
      (.name|test("^L([1-9]|10|11)_[a-z_]+$")) and
      (.passed|type == "boolean") and
      (.detail|IN("initial_state","reference_fixture","activated","evolved","rolled_back","rejected","reset_and_activated","quota_compacted","cursor_walk","archive_verified","rollback_only_validated","deviation")) and
      (.versions|type == "array") and (.versions|all(count)) and
      (.profile_revision|count) and (.lineage_generation|count) and (.at|timestamp))) and
    (.archive|keys_only(["schema_version","row_counts","matches"])) and
    (.archive.schema_version|count) and
    (.archive.row_counts|keys_only(["agent_avatar_profiles","agent_avatar_versions","agent_avatar_activations","agent_avatar_rejections","agent_avatar_resets"]) and all(.[]; count)) and
    (.archive.matches|keys_only(["tables","checksums","profile","versions","lifecycle","svg","continuity_fingerprint"]) and all(.[]; type == "boolean")) and
    (.backup|keys_only(["backup_id","validated_at","drill_cell"]) and all(.[]; type == "string")) and
    ([..|strings]|all(length <= 240 and test("^[A-Za-z0-9 .:_+\\-]*$") and
      (test("witself_|bearer|[0-9a-f]{64}";"i")|not)))
  ' "$1" >/dev/null 2>&1
}
if [[ $redact_check == true && -n $out && -z $account && -z $realm_id && -z $agent_name && -z $control_plane && -z $fleet_token_file && -z $drill_cell && -z $work && $rehearsal == false ]]; then
  [[ -f $out ]] || bad_usage
  if check_record "$out"; then printf '%s\n' 'avatar acceptance: redaction check passed'; else printf '%s\n' 'avatar acceptance: record failed redaction or schema check' >&2; exit 1; fi
  exit 0
fi
[[ -n $account && -n $realm_id && -n $agent_name && -n $control_plane && -n $fleet_token_file && -n $drill_cell && -n $work && -n $out && $redact_check == true ]] || bad_usage
[[ $drill_cell == civo-sandbox-use1-backup ]] || bad_usage
[[ $realm_id =~ ^realm_[a-z0-9]+$ && $agent_name =~ ^[a-zA-Z0-9][a-zA-Z0-9_-]{0,63}$ ]] || bad_usage
safe_endpoint() { [[ $1 =~ ^https://[a-zA-Z0-9.-]+(:[0-9]+)?$ ]]; }
safe_endpoint "$control_plane" || bad_usage
[[ -f $fleet_token_file && ! -L $fleet_token_file && -r $fleet_token_file ]] || bad_usage
for dependency in witself curl tar gzip shasum git mktemp date chmod; do
  command -v "$dependency" >/dev/null || { printf '%s\n' 'avatar acceptance: a required binary is missing' >&2; exit 1; }
done
[[ ! -L $work && ! -e $out && ! -L $out ]] || bad_usage
mkdir -p -- "$work"
work=$(cd "$work" && pwd -P)
out_parent=$(cd "$(dirname "$out")" && pwd -P) || bad_usage
out=$out_parent/$(basename "$out")
# Resolve parents before the checks so symlink aliases cannot put private archives
# inside this or another registered worktree. A nested independent checkout is
# covered by its own rev-parse check.
outside_worktrees() {
  local directory=$1 tree
  if [[ $(git -C "$directory" rev-parse --is-inside-work-tree 2>/dev/null || true) == true ]]; then return 1; fi
  while IFS= read -r tree; do
    case "$tree" in
      'worktree '*) tree=${tree#worktree }; [[ $directory != "$tree" && $directory != "$tree/"* ]] || return 1 ;;
    esac
  done < <(git worktree list --porcelain 2>/dev/null)
}
outside_worktrees "$work" || bad_usage
outside_worktrees "$out_parent" || bad_usage
[[ -z $(ls -A "$work") ]] || bad_usage
chmod 700 "$work"
scratch=$(mktemp -d "$work/avatar-acceptance.XXXXXXXX")
run_id=avatar-$(date -u +%Y%m%dT%H%M%SZ)-${scratch##*.}
record=$scratch/record.json
leg='' revision=0 lineage=0 key_counter=0
account_id='' agent_id='' cell_endpoint='' cell_name=''
finished=false
date_now() { date -u +%Y-%m-%dT%H:%M:%SZ; }
jq -n --arg run "$run_id" --arg realm "$realm_id" --arg drill "$drill_cell" '{schema_version:"witself.avatar-acceptance.v1",run_id:$run,status:"failed",certification_eligible:false,witself:{version:"",commit:"",date:""},cli:{version:"",commit:"",date:""},cell:"",account_id:"",realm_id:$realm,agent_id:"",legs:[],archive:{schema_version:0,row_counts:{},matches:{}},backup:{backup_id:"",validated_at:"",drill_cell:$drill}}' >"$record"
record_update() { jq "$@" "$record" >"$scratch/record.next"; mv "$scratch/record.next" "$record"; }
write_record() {
  check_record "$record" || { printf '%s\n' 'avatar acceptance: retained record rejected by redaction check' >&2; return 1; }
  # Never overwrite earlier evidence, including during the error path.
  (set -o noclobber; cat "$record" >"$out")
  chmod 600 "$out"
}
cleanup() { rm -rf -- "$scratch"; }
failure() {
  trap - ERR INT TERM
  if [[ $finished == false ]]; then
    if [[ -n $leg ]]; then
      record_update --arg name "$leg" --arg at "$(date_now)" --argjson rev "$revision" --argjson lineage "$lineage" '.status="failed" | .certification_eligible=false | .legs += [{name:$name,passed:false,detail:"deviation",versions:[],profile_revision:$rev,lineage_generation:$lineage,at:$at}]' || true
    fi
    write_record || true
  fi
  printf '%s\n' 'avatar acceptance: failed; private temporary material removed' >&3
  exit 1
}
trap cleanup EXIT
trap failure ERR INT TERM
# Capture all child diagnostics. Errors may contain account content or credentials.
exec 3>&2
exec 2>"$scratch/private-errors"
must() { jq -e "$@" >/dev/null; }
pass_leg() {
  record_update --arg name "$leg" --arg detail "$1" --argjson versions "$2" --argjson rev "$revision" --argjson lineage "$lineage" --arg at "$(date_now)" '.legs += [{name:$name,passed:true,detail:$detail,versions:$versions,profile_revision:$rev,lineage_generation:$lineage,at:$at}]'
  printf 'avatar acceptance: %s passed\n' "$leg" >&3
  leg=
}
fresh_key() { key_counter=$((key_counter + 1)); mutation_key=$run_id-$key_counter; }
http_get() {
  if [[ ${3:-public} == fleet ]]; then
    curl --silent --show-error --fail --connect-timeout 10 --max-time "${4:-60}" --header "@$scratch/fleet-header" "$1" >"$2"
  else
    curl --silent --show-error --fail --connect-timeout 10 --max-time 60 "$1" >"$2"
  fi
}
http_post() { curl --silent --show-error --fail --connect-timeout 10 --max-time 300 --header "@$scratch/fleet-header" --header 'Content-Type: application/json' --request POST --data-binary "@$2" "$1" >"$3"; }
refresh() {
  witself avatar show --endpoint "$cell_endpoint" --token-file "$scratch/agent.token" >"$scratch/view.json"
  must --arg agent "$agent_id" --arg realm "$realm_id" --arg account "$account_id" '.avatar.profile | .agent_id==$agent and .realm_id==$realm and .account_id==$account and .profile_revision>=1 and .lineage_generation>=1' "$scratch/view.json"
  revision=$(jq -r '.avatar.profile.profile_revision' "$scratch/view.json")
  lineage=$(jq -r '.avatar.profile.lineage_generation' "$scratch/view.json")
}
receipt() {
  must --arg agent "$agent_id" --argjson prior "$revision" '.avatar.profile.agent_id==$agent and .receipt.result_revision==.avatar.profile.profile_revision and .receipt.result_revision>$prior and (.receipt.replayed//false)==false' "$scratch/mutation.json"
  revision=$(jq -r '.receipt.result_revision' "$scratch/mutation.json")
  lineage=$(jq -r '.avatar.profile.lineage_generation' "$scratch/mutation.json")
  cp "$scratch/mutation.json" "$scratch/view.json"
}
mutate() {
  local operation=$1; shift
  fresh_key
  if [[ $operation == operator ]]; then
    local operator_action=$1; shift
    witself avatar operator "$operator_action" --account "$account" --agent-id "$agent_id" --endpoint "$cell_endpoint" --expected-profile-revision "$revision" --idempotency-key "$mutation_key" "$@" >"$scratch/mutation.json"
  else
    witself avatar "$operation" --endpoint "$cell_endpoint" --token-file "$scratch/agent.token" --expected-profile-revision "$revision" --idempotency-key "$mutation_key" "$@" >"$scratch/mutation.json"
  fi
  receipt
}
activate() {
  local version=$1
  fresh_key
  if ! witself avatar activate --endpoint "$cell_endpoint" --token-file "$scratch/agent.token" --version "$version" --expected-profile-revision "$revision" --idempotency-key "$mutation_key" >"$scratch/mutation.json" 2>"$scratch/activate-error"; then
    # Only a conflict gets one retry. A timeout could have committed and is not
    # safe evidence for this bounded run. Re-read the exact pending proposal and
    # use a fresh key/revision, never replay an old receipt as a new transition.
    LC_ALL=C grep -Eiq 'conflict|409' "$scratch/activate-error"
    refresh
    must --argjson version "$version" '.avatar.profile.proposed_avatar_version==$version and .avatar.profile.autonomy_policy=="agent_self_managed"' "$scratch/view.json"
    fresh_key
    witself avatar activate --endpoint "$cell_endpoint" --token-file "$scratch/agent.token" --version "$version" --expected-profile-revision "$revision" --idempotency-key "$mutation_key" >"$scratch/mutation.json"
  fi
  receipt
  must --argjson version "$version" '.avatar.profile.active_avatar_version==$version and (.avatar.profile.proposed_avatar_version//0)==0 and .avatar.active.version==$version' "$scratch/view.json"
}
propose() {
  local version=$1 parent=$2 expression=$3
  # No pending version may be silently replaced or made into an idempotent replay.
  refresh
  must --argjson parent "$parent" '.avatar.profile.autonomy_policy=="agent_self_managed" and (.avatar.profile.proposed_avatar_version//0)==0 and (.avatar.profile.active_avatar_version//0)==$parent' "$scratch/view.json"
  jq -n --arg expression "$expression" '{identity:{expression:$expression}}' >"$scratch/spec.json"
  mutate propose --parent-version "$parent" --style-pack-id "$style_id" --style-pack-version "$style_version" --subject-form human --description 'Synthetic acceptance reference fixture.' --spec-file "$scratch/spec.json" --svg-file "$scratch/reference.svg"
  must --argjson version "$version" '.receipt.result_version==$version and .avatar.profile.proposed_avatar_version==$version and .avatar.proposed.version==$version' "$scratch/view.json"
}

# Preflight is read-only until the matched build metadata and isolated drill cell
# have been validated. Fleet credentials are passed through a private header file.
fleet_token=$(cat "$fleet_token_file")
[[ -n $fleet_token && $fleet_token != *$'\n'* && $fleet_token != *$'\r'* ]]
printf 'Authorization: Bearer %s\n' "$fleet_token" >"$scratch/fleet-header"
unset fleet_token
witself account list --json >"$scratch/accounts.json"
account_id=$(jq -er --arg name "$account" '[.accounts[]|select(.name==$name)] | select(length==1) | .[0].id' "$scratch/accounts.json")
[[ $account_id =~ ^acc_[a-z0-9]+$ ]]
http_get "$control_plane/v1/directory/$account_id" "$scratch/directory.json"
cell_endpoint=$(jq -er '.cell.endpoint' "$scratch/directory.json")
cell_name=$(jq -er '.cell.cell' "$scratch/directory.json")
safe_endpoint "$cell_endpoint"
[[ $cell_name =~ ^[a-z0-9][a-z0-9-]+$ && $cell_name != "$drill_cell" ]]
http_get "$cell_endpoint/v1/version" "$scratch/cell-build.json"
http_get "$control_plane/v1/version" "$scratch/cp-build.json"
witself version >"$scratch/cli-version"
jq -Rn 'input | capture("^witself (?<version>[^ ]+) \\(commit (?<commit>[^,]+), built (?<date>[^)]+)\\)$")' <"$scratch/cli-version" >"$scratch/cli-build.json"
record_update --slurpfile build "$scratch/cell-build.json" --slurpfile cli "$scratch/cli-build.json" --arg account "$account_id" --arg cell "$cell_name" '.account_id=$account | .cell=$cell | .witself=($build[0]|{version,commit,date}) | .cli=($cli[0]|{version,commit,date})'
pair_matches=false
if jq -e --slurpfile cli "$scratch/cli-build.json" --slurpfile cp "$scratch/cp-build.json" '
  def release: (.version|test("^[0-9]+\\.[0-9]+\\.[0-9]+$")) and (.commit|test("^[0-9a-f]{7,40}$")) and (.date|test("^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}(\\.[0-9]+)?(Z|[+-]([01][0-9]|2[0-3]):[0-5][0-9])$"));
  # CLI and cell ship ShortCommit; the control plane ships FullCommit.
  release and ($cli[0]|release) and ($cp[0]|release) and .version==$cli[0].version and .commit==$cli[0].commit and .version==$cp[0].version and (.commit as $short | $cp[0].commit | startswith($short))
' "$scratch/cell-build.json" >/dev/null; then pair_matches=true; fi
if [[ $pair_matches == true && $rehearsal == false ]]; then record_update '.certification_eligible=true'; fi
if [[ $pair_matches == false ]]; then printf '%s\n' 'avatar acceptance: release mismatch; result can only be rehearsal evidence' >&3; fi

leg=L1_initial
witself agent create --account "$account" --endpoint "$cell_endpoint" --realm "$realm_id" "$agent_name" >"$scratch/agent-created"
IFS=$'\t' read -r agent_id created_name <"$scratch/agent-created"
[[ $agent_id =~ ^agent_[a-z0-9]+$ && $created_name == "$agent_name" ]]
record_update --arg agent "$agent_id" '.agent_id=$agent'
witself token create --account "$account" --endpoint "$cell_endpoint" --agent "$agent_id" --out "$scratch/agent.token" >"$scratch/token-output"
chmod 600 "$scratch/agent.token"
refresh
must '.avatar.profile | (.status=="generation_due" or .status=="placeholder") and .autonomy_policy=="agent_self_managed" and .lineage_generation==1 and (.latest_avatar_version//0)==0 and (.active_avatar_version//0)==0 and (.proposed_avatar_version//0)==0' "$scratch/view.json"
pass_leg initial_state '[]'

leg=L2_style
witself avatar style show --endpoint "$cell_endpoint" --token-file "$scratch/agent.token" >"$scratch/style.json"
style_id=$(jq -er '.style.style_pack.id' "$scratch/style.json")
style_version=$(jq -er '.style.style_pack.version' "$scratch/style.json")
# A 38,092-byte continuity fingerprint must reclaim space, not grow storage.
# The tiny reference alone compacts a retired leaf instead of its oldest parent.
# Eighty bounded non-rendering descriptions in the unlocked experience layer make
# this synthetic fixture larger than the fingerprint while preserving every
# visible element and locked identity byte. The store regression uses this exact
# augmentation through the real sanitizer and continuity validator.
jq -er '.style.style_pack.references | map(select(.subject_form=="human")) | select(length==1) | .[0].svg |
  sub("<g id=\"experience\" data-layer=\"experience\"></g>";
      "<g id=\"experience\" data-layer=\"experience\">" + (("<desc>" + ("x" * 512) + "</desc>") * 80) + "</g>") |
  select(length>38092 and length<65536)' "$scratch/style.json" >"$scratch/reference.svg"
pass_leg reference_fixture '[]'

leg=L3_initial_activation
propose 1 0 calm
activate 1
pass_leg activated '[1]'
leg=L4_evolution
propose 2 1 curious
activate 2
pass_leg evolved '[1,2]'
leg=L5_rollback
witself avatar version --endpoint "$cell_endpoint" --token-file "$scratch/agent.token" --version 1 >"$scratch/rollback-version.json"
must '.version.version==1 and .version.rollback_eligible==true' "$scratch/rollback-version.json"
mutate rollback --version 1
must '.avatar.profile.active_avatar_version==1 and .avatar.active.version==1' "$scratch/view.json"
pass_leg rolled_back '[2,1]'
leg=L6_rejection
propose 3 1 focused
mutate operator reject --version 3 --reason-code acceptance_rejection
must '.avatar.profile.status=="active" and (.avatar.profile.proposed_avatar_version//0)==0 and .avatar.profile.active_avatar_version==1 and .avatar.active.version==1' "$scratch/view.json"
witself avatar version --endpoint "$cell_endpoint" --token-file "$scratch/agent.token" --version 3 >"$scratch/rejected-version.json"
must '.version.version==3 and .version.rejected==true and .version.is_active==false and .version.is_proposed==false' "$scratch/rejected-version.json"
pass_leg rejected '[3]'
leg=L7_reset
propose 4 1 calm
activate 4
mutate reset --reason-code acceptance_reset
must '.avatar.profile.lineage_generation==2 and (.avatar.profile.active_avatar_version//0)==0 and (.avatar.profile.proposed_avatar_version//0)==0' "$scratch/view.json"
propose 5 0 calm
activate 5
must '.avatar.profile.lineage_generation==2 and .avatar.active.lineage_generation==2 and (.avatar.active.parent_version//0)==0' "$scratch/view.json"
pass_leg reset_and_activated '[4,5]'
leg=L8_compaction
byte_limit=$(jq -er '.avatar.profile.retained_payload_byte_limit' "$scratch/view.json")
mutate operator quota --retained-payload-count-limit 4 --retained-payload-byte-limit "$byte_limit"
for attempt in 1 2 3 4 5; do
  witself avatar history --endpoint "$cell_endpoint" --token-file "$scratch/agent.token" --limit 100 >"$scratch/history.json"
  if jq -e 'any(.versions[]; .version==1 and .lineage_generation==1 and .payload_state=="compacted" and .payload_compaction_reason=="quota")' "$scratch/history.json" >/dev/null; then break; fi
  [[ $attempt != 5 ]]
  sleep "$attempt"
done
refresh
must '.avatar.profile.retained_payload_count==4 and .avatar.profile.retained_payload_count_limit==4 and .avatar.profile.active_avatar_version==5' "$scratch/view.json"
pass_leg quota_compacted '[1]'
leg=L9_history
printf '[]\n' >"$scratch/walk.json"
cursor=0
for page in 1 2 3 4; do
  args=(avatar history --endpoint "$cell_endpoint" --token-file "$scratch/agent.token" --limit 2)
  if ((cursor>0)); then args+=(--before-version "$cursor"); fi
  witself "${args[@]}" >"$scratch/page.json"
  must --argjson cursor "$cursor" '.versions|length<=2 and all(.[]; .version>0 and ($cursor==0 or .version<$cursor))' "$scratch/page.json"
  jq --slurpfile page "$scratch/page.json" '. + $page[0].versions' "$scratch/walk.json" >"$scratch/walk.next"
  mv "$scratch/walk.next" "$scratch/walk.json"
  next=$(jq -r '.next_before_version//0' "$scratch/page.json")
  [[ $next =~ ^[0-9]+$ ]]
  if ((next==0)); then break; fi
  ((cursor==0 || next<cursor))
  cursor=$next
  [[ $page != 4 ]]
done
must 'map(.version)==[5,4,3,2,1]' "$scratch/walk.json"
must --slurpfile history "$scratch/history.json" '(sort_by(.version))==($history[0].versions|sort_by(.version))' "$scratch/walk.json"
pass_leg cursor_walk '[5,4,3,2,1]'

leg=L10_archive
refresh
witself export --account "$account" --endpoint "$cell_endpoint" --out "$scratch/self.tar.gz" >"$scratch/export-output"
chmod 600 "$scratch/self.tar.gz"
gzip -t "$scratch/self.tar.gz"
tar -tzf "$scratch/self.tar.gz" >"$scratch/archive-members"
# Read named streams with -O; never extract a potentially hostile archive path.
tar -xzOf "$scratch/self.tar.gz" manifest.json >"$scratch/manifest.json"
tar -xzOf "$scratch/self.tar.gz" checksums.json >"$scratch/checksums.json"
must --arg account "$account_id" --slurpfile build "$scratch/cell-build.json" '.format_version==1 and .purpose=="self" and .account_id==$account and .server_version==$build[0].version and .schema_version>=50 and (["avatar_style_packs","avatar_style_pack_versions","realm_avatar_styles","avatar_style_rollout_jobs","agent_avatar_profiles","agent_avatar_versions","agent_avatar_activations","agent_avatar_rejections","agent_avatar_resets","avatar_mutation_receipts"] - .tables | length==0)' "$scratch/manifest.json"
must '.chunks|type=="array" and (map(.name)|length== (unique|length))' "$scratch/checksums.json"
tables=(agent_avatar_profiles agent_avatar_versions agent_avatar_activations agent_avatar_rejections agent_avatar_resets)
for table in "${tables[@]}"; do
  : >"$scratch/$table.ndjson"
  jq -er --arg table "$table" '.chunks[]|select(.name|startswith($table+"/"))|.name' "$scratch/checksums.json" >"$scratch/chunk-names"
  [[ -s $scratch/chunk-names ]]
  total_rows=0
  while IFS= read -r chunk; do
    [[ $chunk =~ ^$table/[0-9]{6}\.ndjson$ ]]
    [[ $(LC_ALL=C grep -Fxc "$chunk" "$scratch/archive-members") == 1 ]]
    tar -xzOf "$scratch/self.tar.gz" "$chunk" >"$scratch/chunk.ndjson"
    digest=$(shasum -a 256 "$scratch/chunk.ndjson"); digest=${digest%% *}
    bytes=$(wc -c <"$scratch/chunk.ndjson")
    rows=$(jq -s 'length' "$scratch/chunk.ndjson")
    must --arg name "$chunk" --arg digest "$digest" --argjson bytes "$bytes" --argjson rows "$rows" 'any(.chunks[]; .name==$name and .sha256==$digest and .bytes==$bytes and .rows==$rows)' "$scratch/checksums.json"
    total_rows=$((total_rows + rows))
    jq -c --arg agent "$agent_id" 'select(.agent_id==$agent)' "$scratch/chunk.ndjson" >>"$scratch/$table.ndjson"
  done <"$scratch/chunk-names"
  must --arg table "$table" --argjson rows "$total_rows" '.table_rows[$table]==$rows' "$scratch/checksums.json"
  # Reject unchecked extra chunks in an avatar stream, even if trailer rows match.
  actual_chunks=$(LC_ALL=C grep -Ec "^$table/" "$scratch/archive-members")
  expected_chunks=$(wc -l <"$scratch/chunk-names")
  ((actual_chunks==expected_chunks))
  jq -s '.' "$scratch/$table.ndjson" >"$scratch/$table.json"
done
must --slurpfile live "$scratch/view.json" 'length==1 and (.[0] as $p | $live[0].avatar.profile as $l | $p.account_id==$l.account_id and $p.realm_id==$l.realm_id and $p.agent_id==$l.agent_id and $p.revision==$l.profile_revision and $p.active_avatar_version==$l.active_avatar_version and $p.latest_avatar_version==$l.latest_avatar_version and ($p.proposed_avatar_version//0)==($l.proposed_avatar_version//0) and $p.lineage_generation==$l.lineage_generation and $p.status==$l.status)' "$scratch/agent_avatar_profiles.json"
must --slurpfile live "$scratch/walk.json" '
  def projection: {version,parent_version:(.parent_version//0),lineage_generation,payload_state,payload_compaction_reason:(.payload_compaction_reason//""),svg_sha256,locked_layers_sha256,renderer_profile};
  length==5 and (map(projection)|sort_by(.version))==($live[0]|map(projection)|sort_by(.version)) and
  (map(.lineage_generation)==[1,1,1,1,2]) and
  (.[0].payload_state=="compacted" and .[0].continuity_fingerprint!=null and (.[0].continuity_fingerprint|type=="string") and (.[0].continuity_fingerprint|test("^\\\\x[0-9a-f]{76184}$"))) and
  (.[0].svg==null or .[0].svg=="")
' "$scratch/agent_avatar_versions.json"
# Compare exact explicit live version reads too; only equality booleans survive.
for version in 1 2 3 4 5; do
  witself avatar version --endpoint "$cell_endpoint" --token-file "$scratch/agent.token" --version "$version" >"$scratch/live-version.json"
  must --argjson version "$version" --slurpfile live "$scratch/live-version.json" 'any(.[]; .version==$version and .svg_sha256==$live[0].version.svg_sha256 and (.svg//"")==($live[0].version.svg//"") and .payload_state==$live[0].version.payload_state)' "$scratch/agent_avatar_versions.json"
done
must 'length==5 and (sort_by(.sequence)|map(.avatar_version))==[1,2,1,4,5] and (sort_by(.sequence)|map(.action))==["activated","activated","rolled_back","activated","activated"] and (sort_by(.sequence)|map(.lineage_generation))==[1,1,1,1,2]' "$scratch/agent_avatar_activations.json"
must 'length==1 and .[0].avatar_version==3' "$scratch/agent_avatar_rejections.json"
must 'length==1 and .[0].retired_lineage_generation==1 and .[0].new_lineage_generation==2 and .[0].retired_active_version==4' "$scratch/agent_avatar_resets.json"
record_update --slurpfile manifest "$scratch/manifest.json" '.archive={schema_version:$manifest[0].schema_version,row_counts:{agent_avatar_profiles:1,agent_avatar_versions:5,agent_avatar_activations:5,agent_avatar_rejections:1,agent_avatar_resets:1},matches:{tables:true,checksums:true,profile:true,versions:true,lifecycle:true,svg:true,continuity_fingerprint:true}}'
rm -f "$scratch/self.tar.gz"
pass_leg archive_verified '[1,2,3,4,5]'

leg=L11_restore_drill
# Read the account's durable backup authority only after the lifecycle/archive
# checks. A preexisting job persists before acquiring its database snapshot, but
# its manifest exported_at can be assigned much later. That timestamp cannot
# prove that the backup contains this lifecycle. Fixed-width UTC minute IDs give
# a causal fence without comparing the client, database and control-plane clocks.
http_get "$control_plane/v1/backups/status?account_id=$account_id" "$scratch/backup-before.json" fleet
must --arg account "$account_id" '
  def identity:
    type=="object" and .account_id==$account and
    (.backup_id|type=="string" and test("^backup_[0-9]{8}T[0-9]{4}00Z$")) and
    (.scheduled_at|sub("\\.000Z$";"Z")|fromdateiso8601|strftime("backup_%Y%m%dT%H%M%SZ"))==.backup_id;
  .schema_version=="witself.v0" and .account.schema_version=="witself.v0" and
  .account.account_id==$account and
  (.account.backups |
    .schema_version=="witself.account-backup.v1" and .account_id==$account and
    has("current_job") and (.current_job==null or (.current_job|identity)) and
    (.catalog|type=="array" and all(.[]; identity)))
' "$scratch/backup-before.json"
printf '%s\n' 'avatar acceptance: requesting one additional backup object; this is a billing-bearing write' >&3
jq -n --arg account "$account_id" '{account_id:$account}' >"$scratch/backup-request.json"
http_post "$control_plane/v1/backups:run" "$scratch/backup-request.json" "$scratch/backup-run.json"
must --arg account "$account_id" '.account_id==$account and (.status=="committed" or .status=="retrying") and ((has("recovered_existing_object")|not) or .recovered_existing_object==false)' "$scratch/backup-run.json"
backup_id=$(jq -er '.backup_id' "$scratch/backup-run.json")
[[ $backup_id =~ ^backup_[0-9]{8}T[0-9]{6}Z$ ]]
must --arg backup "$backup_id" '
  .account.backups | [.catalog[].backup_id, .current_job.backup_id // empty] |
  all(.[]; . < $backup)
' "$scratch/backup-before.json"
# Allow the first scheduled retry (60s) and a full export attempt (300s), with
# one minute of scheduling slack. Neither retry_at nor a slow status response
# may extend this deadline; later retries can require a fresh acceptance run.
backup_deadline=$(( $(date -u +%s) + 420 ))
while :; do
  backup_remaining=$((backup_deadline - $(date -u +%s)))
  if [[ $backup_remaining -le 0 ]]; then failure; fi
  backup_poll_timeout=$backup_remaining
  if [[ $backup_poll_timeout -gt 60 ]]; then backup_poll_timeout=60; fi
  http_get "$control_plane/v1/backups/status?account_id=$account_id" "$scratch/backup-status.json" fleet "$backup_poll_timeout"
  must --arg backup "$backup_id" --arg account "$account_id" '
    .account.account_id==$account and (.account.backups.catalog|type=="array") and
    (.account.backups.current_job | .account_id==$account and .backup_id==$backup and
      (.status|IN("pending","running","retrying","failed","committed")))
  ' "$scratch/backup-status.json"
  if jq -e --arg backup "$backup_id" --arg account "$account_id" --slurpfile manifest "$scratch/manifest.json" '
    .account.backups.current_job.status=="committed" and
    any(.account.backups.catalog[]; .backup_id==$backup and .account_id==$account and .archive_schema_version==$manifest[0].schema_version)
  ' "$scratch/backup-status.json" >/dev/null; then break; fi
  must --arg backup "$backup_id" '(.account.backups.current_job|.backup_id!=$backup or .status!="failed")' "$scratch/backup-status.json"
  backup_now=$(date -u +%s)
  backup_remaining=$((backup_deadline - backup_now))
  if [[ $backup_remaining -le 0 ]]; then failure; fi
  backup_sleep=$(jq -er --arg backup "$backup_id" --argjson now "$backup_now" \
    --argjson remaining "$backup_remaining" --slurpfile run "$scratch/backup-run.json" '
    def instant: capture("^(?<seconds>[0-9-]+T[0-9:]+)(?<fraction>\\.[0-9]+)?Z$") |
      ((.seconds+"Z"|fromdateiso8601) + (("0"+(.fraction//""))|tonumber));
    (.account.backups.current_job // {}) as $job |
    (if $job.backup_id==$backup then
      (if $job.status=="retrying" then $job.retry_at else null end)
    else $run[0].retry_at end) |
    (if . == null then 10 else ((instant | ceil) - $now) end) |
    (if . <= 0 then 10 else . end) | [.,60,$remaining] | min
  ' "$scratch/backup-status.json")
  sleep "$backup_sleep"
done
jq -n --arg account "$account_id" --arg backup "$backup_id" --arg target "$drill_cell" '{account_id:$account,backup_id:$backup,target_cell:$target}' >"$scratch/drill-request.json"
http_post "$control_plane/v1/backups:restore-drill" "$scratch/drill-request.json" "$scratch/drill.json"
must --arg account "$account_id" --arg backup "$backup_id" --arg target "$drill_cell" --slurpfile manifest "$scratch/manifest.json" '.schema_version=="witself.v0" and .validated==true and .account_id==$account and .backup_id==$backup and .target_cell==$target and .archive_schema_version==$manifest[0].schema_version and (.validated_at|type=="string" and test("^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}(\\.[0-9]+)?Z$"))' "$scratch/drill.json"
http_get "$control_plane/v1/directory/$account_id" "$scratch/directory-after.json"
must --slurpfile before "$scratch/directory.json" '{account_id,cell,epoch,status}==($before[0]|{account_id,cell,epoch,status})' "$scratch/directory-after.json"
record_update --slurpfile drill "$scratch/drill.json" '.backup.backup_id=$drill[0].backup_id | .backup.validated_at=$drill[0].validated_at'
pass_leg rollback_only_validated '[]'
if [[ $pair_matches == true && $rehearsal == false ]]; then record_update '.status="passed"'; else record_update '.status="rehearsal" | .certification_eligible=false'; fi
write_record
finished=true
printf '%s\n' 'avatar acceptance: value-free record retained; private temporary material removed' >&3
if [[ $pair_matches == false && $rehearsal == false ]]; then exit 1; fi
