#!/usr/bin/env bash
# Offline protocol fixtures follow internal/client/message{,_request}.go and
# cmd/witself/message{,_request}.go JSON wrappers. Never invokes a real CLI.
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)"
canary="$repo_root/scripts/run-collaboration-canary.sh"
work_dir="$(mktemp -d "${TMPDIR:-/tmp}/witself-collaboration-canary-test.XXXXXX")"
trap 'rm -rf "$work_dir"' EXIT
chmod 700 "$work_dir"
mkdir "$work_dir/bin" "$work_dir/cases"

cat >"$work_dir/bin/witself" <<'SHIM'
#!/usr/bin/env bash
set -euo pipefail
state="$FAKE_COLLABORATION_STATE"
scenario="$FAKE_COLLABORATION_SCENARIO"
die() { printf 'offline witself fixture: %s\n' "$1" >&2; exit 90; }
read_body() {
  # The sentinel prevents command substitution from stripping trailing newlines.
  body="$(cat "$@" && printf '.')" || die 'body source read failed'
  body="${body%.}"
}
if [ "${1:-}" = --version ]; then
  [ "$#" = 1 ] || die 'version arguments'
  printf '%s\n' 'witself 0.0.342 (commit abc1234, built 2026-09-05T00:00:00Z)'
  exit 0
fi

[ "$#" -ge 2 ] || die 'missing command'
family="$1 $2"
shift 2
if [ "$family" = 'message request' ]; then
  verb="${1:-}"
  shift
  operation="request $verb"
else
  operation="$family"
fi
case "$operation" in
  'self show') allowed=' no-facts no-salient ' ;;
  'request open') allowed=' body body-file body-stdin subject selection-policy max-assignees offer-window expires-in idempotency-key ' ;;
  'request list') allowed=' state phase role limit cursor ' ;;
  'request show'|'request cancel'|'message read'|'message ack') allowed=' ' ;;
  'request offer') allowed=' body body-file body-stdin subject idempotency-key ' ;;
  'request select') allowed=' selected-agent reservation idempotency-key ' ;;
  'request claim'|'message claim') allowed=' lease idempotency-key ' ;;
  'request renew') allowed=' claim generation lease ' ;;
  'request complete') allowed=' claim generation body body-file body-stdin subject idempotency-key ' ;;
  'message reply') allowed=' body body-file body-stdin subject kind idempotency-key ' ;;
  'message complete') allowed=' claim generation body body-file body-stdin subject kind idempotency-key ' ;;
  'message release') allowed=' claim generation deterministic-failure ' ;;
  *) die 'unexpected command' ;;
esac
allowed="$allowed endpoint token-file agent realm json "
endpoint= token_file= json=false id= body= body_sources=0 key= claim= generation= lease= kind=
binding_agent= binding_realm=
selection_policy= max_assignees= offer_window= expires_in= selected_agent= reservation=
filter_state= role= cursor= no_facts=false no_salient=false
while [ "$#" -gt 0 ]; do
  case "$1" in
    --*|-json)
      flag="${1#--}"
      [ "$1" != -json ] || flag=json
      case "$allowed" in *" $flag "*) ;; *) die 'unsupported flag for command' ;; esac
      case "$flag" in
        json) json=true; shift; continue ;;
        no-facts) no_facts=true; shift; continue ;;
        no-salient) no_salient=true; shift; continue ;;
        body-stdin) read_body; body_sources=$((body_sources + 1)); shift; continue ;;
        deterministic-failure) shift; continue ;;
      esac
      [ "$#" -ge 2 ] || die 'flag missing value'
      case "$flag" in
        endpoint) endpoint="$2" ;;
        token-file) token_file="$2" ;;
        agent) binding_agent="$2" ;;
        realm) binding_realm="$2" ;;
        body) body="$2"; body_sources=$((body_sources + 1)) ;;
        body-file) read_body "$2"; body_sources=$((body_sources + 1)) ;;
        idempotency-key) key="$2" ;;
        claim) claim="$2" ;;
        generation) generation="$2" ;;
        lease) lease="$2" ;;
        kind) kind="$2" ;;
        selection-policy) selection_policy="$2" ;;
        max-assignees) max_assignees="$2" ;;
        offer-window) offer_window="$2" ;;
        expires-in) expires_in="$2" ;;
        selected-agent) selected_agent="$2" ;;
        reservation) reservation="$2" ;;
        state) filter_state="$2" ;;
        role) role="$2" ;;
        cursor) cursor="$2" ;;
        subject|phase|limit) ;;
        *) die 'unhandled flag' ;;
      esac
      shift 2 ;;
    *) [ -z "$id" ] || die 'extra positional argument'; id="$1"; shift ;;
  esac
done
[ "$json" = true ] || die 'JSON output required'
case "$endpoint:$token_file" in
  "https://coordinator.invalid:$FAKE_COORDINATOR_TOKEN_FILE") actor=agent_coordinator ;;
  "https://worker.invalid:$FAKE_WORKER_TOKEN_FILE") actor=agent_worker ;;
  *) die 'explicit binding mismatch' ;;
esac
[ "$binding_agent" = "${actor#agent_}" ] && [ "$binding_realm" = Founder ] || die 'explicit agent name or realm mismatch'
# Reading only the supplied fixture binding also verifies it exists. Never log it.
[ -s "$token_file" ] || die 'fixture token missing'
printf '%s|%s|%s\n' "$actor" "$operation" "$id" >>"$state/calls"
db="$state/state.json"
fixture_now="$(jq -nr 'now')"
if [ -f "$state/clock" ]; then
  fixture_now="$(jq -nr --argjson previous "$(cat "$state/clock")" '$previous + 0.001')"
  printf '%s\n' "$fixture_now" >"$state/clock"
fi
jq() { command jq --argjson fixture_now "$fixture_now" "$@"; }
advance_clock() {
  fixture_now="$(jq -nr --argjson seconds "$1" '$fixture_now + $seconds')"
  printf '%s\n' "$fixture_now" >"$state/clock"
}
update() {
  jq "$@" "$db" >"$state/next.json"
  mv "$state/next.json" "$db"
}
check() { jq -e "$@" "$db" >/dev/null || die 'invalid actor, state, or fence'; }
require_actor() { [ "$actor" = "$1" ] || die 'wrong actor'; }
require_body() { [ "$body_sources" = 1 ] && [ -n "$body" ] && [ -n "$key" ] || die 'body or retry key missing'; }
require_lease() {
  local duration supplied="${1:-$lease}"
  case "$supplied" in
    *s) duration="${supplied%s}" ;;
    *m) duration="${supplied%m}"; [[ "$duration" =~ ^[0-9]+$ ]] || die 'invalid lease'; duration=$((duration * 60)) ;;
    *) die 'unbounded or invalid lease' ;;
  esac
  [[ "$duration" =~ ^[0-9]+$ ]] && [ "$duration" -ge 30 ] && [ "$duration" -le 900 ] || die 'unbounded or invalid lease'
  fixture_lease_seconds="$duration"
}
require_live_request_claim() {
  check --arg id "$id" '
    def instant:
      capture("^(?<whole>[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2})(?<fraction>\\.[0-9]+)?Z$") |
      ((.whole+"Z"|fromdateiso8601)+((.fraction//"0")|tonumber));
    .requests[$id] | .request.state == "open" and
      $fixture_now < (.request.expires_at|instant) and
      $fixture_now < (.claims[0].lease_expires_at|instant)'
}

case "$scenario" in slow_retry_calls|late_retry_calls)
  if [ "$scenario" = late_retry_calls ] && [ "$id" = mrq_retry ] &&
    { [ "$operation" = 'request select' ] || [ "$operation" = 'request claim' ]; }; then
    advance_clock 15
  fi
  if jq -e '.requests.mrq_retry.claims[0].state == "claimed"' "$db" >/dev/null; then
    case "$operation" in 'message reply'|'message claim'|'message read'|'message complete'|'message ack'|'request renew'|'request complete')
      advance_clock 6
      update '.timed_retry_calls=(.timed_retry_calls//0)+1' ;;
    esac
  fi ;;
esac

case "$operation" in
  'self show')
    [ -z "$id" ] && [ "$no_facts" = true ] && [ "$no_salient" = true ] || die 'identity preflight must suppress memory values'
    realm=realm_founder
    [ "$scenario:$actor" != wrong_realm:agent_worker ] || realm=realm_other
    jq -n --arg actor "$actor" --arg realm "$realm" '{schema_version:"witself.v0",identity:{account_id:"acc_founder",agent_id:$actor,agent_name:($actor|sub("agent_";"")),realm_id:$realm,realm_name:"Founder"},primary_facts:[],salient_memories:[],index:{kinds:[],tags:[],counts:{}},elided:false}' ;;
  'request open')
    require_actor agent_coordinator
    require_body
    [ -z "$id" ] && [ "$selection_policy" = client_ranked ] && [ "$max_assignees" = 1 ] || die 'open policy'
    [[ "$offer_window" =~ ^[1-9][0-9]*s$ ]] && [[ "$expires_in" =~ ^[1-9][0-9]*s$ ]] || die 'open must use bounded integral seconds'
    offer_seconds="${offer_window%s}"
    expiry_seconds="${expires_in%s}"
    [ "$offer_seconds" -le 900 ] && [ "$expiry_seconds" -gt "$offer_seconds" ] && [ "$expiry_seconds" -le 604800 ] || die 'open timing bounds'
    if [[ "$scenario" == unowned_open_* ]]; then
      if [ -f "$state/open-key" ]; then
        [ "$(cat "$state/open-key")" = "$key" ] || die 'recovery changed the opening key'
      else
        printf '%s' "$key" >"$state/open-key"
      fi
      # Return unrelated coordinator-owned work on both the initial call and
      # exact-key recovery. Other faults forge the expected body so each
      # identity/graph check is exercised independently of the body check.
      jq --arg body "$body" --arg fault "${scenario#unowned_open_}" '
        .requests.mrq_existing | {request,opening_message} |
        if $fault == "body" then . else .opening_message.body=$body end |
        if $fault == "other_run" then .opening_message.body |= sub("run=[^; ]+"; "run=another-canary")
        elif $fault == "other_attempt" then .opening_message.body |= sub("attempt=[0-9]+"; "attempt=9")
        elif $fault == "request_account" then .request.account_id="acc_other"
        elif $fault == "request_realm" then .request.realm_id="realm_other"
        elif $fault == "coordinator" then .request.coordinator.agent_id="agent_other"
        elif $fault == "coordinator_kind" then .request.coordinator.kind="realm"
        elif $fault == "opening_account" then .opening_message.account_id="acc_other"
        elif $fault == "opening_realm" then .opening_message.realm_id="realm_other"
        elif $fault == "sender" then .opening_message.from.agent_id="agent_other"
        elif $fault == "sender_kind" then .opening_message.from.kind="realm"
        elif $fault == "recipient" then .opening_message.to.kind="agent"
        elif $fault == "kind" then .opening_message.kind="question"
        elif $fault == "opening_id" then .opening_message.id="msg_other"
        elif $fault == "thread" then .opening_message.thread_id=null
        elif $fault == "parent" then .opening_message.reply_to_message_id="msg_other"
        elif $fault == "depth" then .opening_message.causal_depth=2
        else . end' "$db"
      exit 0
    fi
    existing="$(jq -r --arg key "$key" '.open_keys[$key] // empty' "$db")"
    if [ -n "$existing" ]; then
      jq --arg id "$existing" '.requests[$id] | {request,opening_message}' "$db"
      exit 0
    fi
    check '.opens < 2'
    number="$(jq -r '.opens' "$db")"
    suffix=first
    if [ "$number" = 1 ]; then
      suffix=retry
      check '.requests.mrq_first.request.state == "completed" and .messages.msg_result_first.read_state.state == "acked"'
    fi
    id="mrq_$suffix"
    update --arg id "$id" --arg suffix "$suffix" --arg body "$body" --arg key "$key" \
      --argjson offer_seconds "$offer_seconds" --argjson expiry_seconds "$expiry_seconds" --argjson fixture_now "$fixture_now" '
      def agent($id): {kind:"agent",agent_id:$id,agent_name:($id|sub("agent_";""))};
      ($fixture_now|strftime("%Y-%m-%dT%H:%M:%SZ")) as $now |
      ($fixture_now+$offer_seconds|strftime("%Y-%m-%dT%H:%M:%SZ")) as $offer_deadline |
      ($fixture_now+$expiry_seconds|strftime("%Y-%m-%dT%H:%M:%SZ")) as $expiry |
      {id:("msg_open_"+$suffix),account_id:"acc_founder",realm_id:"realm_founder",from:agent("agent_coordinator"),to:{kind:"realm",count:1},kind:"open_request",body:$body,thread_id:("thr_"+$suffix),causal_depth:1,created_at:$now,delivery:{state:"delivered",delivered_at:$now},read_state:{state:"unread"},processing:{state:"available",generation:0,failure_count:0}} as $opening |
      .opens += 1 | .open_keys[$key]=$id | .requests[$id]={
        request:{id:$id,account_id:"acc_founder",realm_id:"realm_founder",opening_message_id:$opening.id,coordinator:agent("agent_coordinator"),selection_policy:"client_ranked",state:"open",phase:"collecting_offers",max_assignees:1,candidate_count:1,offer_count:0,decline_count:0,selected_agent_ids:[],selection_generation:0,offer_deadline:$offer_deadline,expires_at:$expiry,created_at:$now,updated_at:$now},
        opening_message:$opening,candidates:[{agent:agent("agent_worker"),response_state:"pending",created_at:$now}],offers:[],selections:[],claims:[]}
      | .messages[$opening.id]=$opening'
    if [ "$scenario" = delayed_open ]; then advance_clock 8; fi
    if [ "$scenario" = lost_open_deadline ] && [ "$id" = mrq_first ]; then
      advance_clock 15
      printf '%s\n' 'offline open response lost after HTTP timeout' >&2
      exit 1
    elif [ "$scenario" = lost_open_once ] && [ "$id" = mrq_first ]; then
      printf '%s\n' 'offline open response lost' >&2
      exit 1
    elif [ "$scenario" = malformed_open ]; then
      jq --arg id "$id" '.requests[$id] | {request,opening_message} | .request.max_assignees=2' "$db"
    elif [ "$scenario" = token_metadata ]; then
      jq --arg id "$id" --rawfile token "$FAKE_COORDINATOR_TOKEN_FILE" '.requests[$id] | {request,opening_message} | .opening_message.thread_id=("thr_"+($token|rtrimstr("\n")))' "$db"
    elif [ "$scenario" = body_metadata ]; then
      jq --arg id "$id" '.requests[$id] | {request,opening_message} | .opening_message.thread_id="thr_WITSELF_COLLABORATION_CANARY_V1"' "$db"
    else
      jq --arg id "$id" '.requests[$id] | {request,opening_message}' "$db"
    fi ;;
  'request list')
    require_actor agent_worker
    [ -z "$id" ] && [ "$filter_state" = open ] && [ "$role" = candidate ] || die 'discovery filters'
    case "$scenario" in delayed_open|lost_open_deadline) advance_clock 8 ;; esac
    if [ "$scenario" = no_discovery ]; then printf '%s\n' '{"requests":[]}';
    elif [ "$scenario" = paginated ] && [ -z "$cursor" ]; then
      jq '{requests:[(.requests[] | .request | select(.state == "open") | .id="mrq_unrelated")],next_cursor:"offline_page"}' "$db"
    elif [ "$scenario" = paginated ]; then
      [ "$cursor" = offline_page ] || die 'invalid pagination cursor'
      printf '%s\n' 'observed' >>"$state/second-page"
      jq '{requests:[.requests[] | .request | select(.state == "open")]}' "$db"
    else
      jq '{requests:[.requests[] | .request | select(.state == "open")]}' "$db"
    fi ;;
  'request show')
    check --arg id "$id" '.requests[$id] != null'
    case "$scenario:$actor" in delayed_open:agent_worker|lost_open_deadline:agent_worker) advance_clock 9 ;; esac
    if [ "$actor" = agent_coordinator ] &&
      { [ "$scenario" = pending_candidates ] || { [ "$scenario" = late_retry_calls ] && [ "$id" = mrq_retry ]; }; }; then
      if [ ! -f "$state/selection-wait-$id" ]; then
        # A legal individual HTTP response can outlast the ordinary 2s poll
        # budget while another realm candidate is still pending.
        if [ "$scenario" = pending_candidates ]; then sleep 3; fi
        : >"$state/selection-wait-$id"
      else
        fixture_now="$(jq -r --arg id "$id" '[$fixture_now, (.requests[$id].request.offer_deadline | fromdateiso8601 | .+1)] | max' "$db")"
        printf '%s\n' "$fixture_now" >"$state/clock"
      fi
      update --arg id "$id" --argjson clock "$fixture_now" '
        if .requests[$id].request.state == "open" and (.requests[$id].claims|length) == 0 then
          .requests[$id].request.phase=(if $clock < (.requests[$id].request.offer_deadline | fromdateiso8601)
            then "collecting_offers" else "awaiting_selection" end)
        else . end'
    fi
    if [ "$scenario:$actor" = wrong_run_discovery:agent_worker ]; then
      jq --arg id "$id" '.requests[$id] | .opening_message.body |= sub("run=[^; ]+"; "run=another-canary")' "$db"
    elif [ "$scenario:$actor" = wrong_attempt_discovery:agent_worker ]; then
      jq --arg id "$id" '.requests[$id] | .opening_message.body |= sub("attempt=[0-9]+"; "attempt=9")' "$db"
    elif [ "$scenario:$actor" = cleanup_wrong_request:agent_coordinator ]; then
      jq --arg id "$id" '.requests[$id] | .request.id="mrq_unrelated" | .request.state="completed"' "$db"
    else
      jq --arg id "$id" '.requests[$id]' "$db"
    fi ;;
  'request offer')
    require_actor agent_worker
    require_body
    check --arg id "$id" '.requests[$id].request.state == "open" and .requests[$id].request.offer_count == 0'
    check --arg id "$id" --argjson clock "$fixture_now" '($clock < (.requests[$id].request.offer_deadline | fromdateiso8601))'
    if [ "$scenario" = no_offer ] || [ "$scenario" = cleanup_wrong_request ]; then
      printf '%s\n' 'offline worker offer unavailable' >&2
      exit 1
    fi
    update --arg id "$id" --arg body "$body" '
      .requests[$id].opening_message as $opening |
      ($opening | .id=("msg_offer_"+($id|sub("mrq_";""))) | .from={kind:"agent",agent_id:"agent_worker",agent_name:"worker"} | .to={kind:"agent",agent_id:"agent_coordinator",agent_name:"coordinator"} | .kind="offer" | .body=$body | .reply_to_message_id=$opening.id | .causal_depth=2) as $message |
      .requests[$id].offers=[{agent:$message.from,message:$message,offered_at:$message.created_at}] |
      .requests[$id].request.offer_count=1 | .requests[$id].request.phase="awaiting_selection" |
      .requests[$id].candidates[0].response_state="offered" | .requests[$id].candidates[0].offer_message_id=$message.id |
      .messages[$message.id]=$message'
    if [ "$scenario" = pending_candidates ] || { [ "$scenario" = late_retry_calls ] && [ "$id" = mrq_retry ]; }; then
      update --arg id "$id" '.requests[$id].request.candidate_count=2 | .requests[$id].request.phase="collecting_offers"'
    fi
    jq --arg id "$id" '.requests[$id] | {request,offer:.offers[0]}' "$db" ;;
  'request select')
    require_actor agent_coordinator
    [ "$selected_agent" = agent_worker ] && [ -n "$reservation" ] && [ -n "$key" ] || die 'selection input'
    require_lease "$reservation"
    check --arg id "$id" '.requests[$id].request.offer_count == 1 and .requests[$id].request.phase == "awaiting_selection" and .requests[$id].request.state == "open" and (.requests[$id].claims|length)==0'
    update --arg id "$id" --argjson lease "$fixture_lease_seconds" '
      def stamp: (floor | strftime("%Y-%m-%dT%H:%M:%S")) + "." +
        (("000000" + (((. - floor) * 1000000 | floor) | tostring))[-6:]) + "Z";
      $fixture_now as $epoch | ($epoch | stamp) as $now |
      ([($epoch + $lease), (.requests[$id].request.expires_at | fromdateiso8601)] | min | stamp) as $expiry |
      .requests[$id].request.selected_agent_ids=["agent_worker"] |
      .requests[$id].request.selection_generation=1 | .requests[$id].request.phase="assigned" |
      .requests[$id].selections=[{id:("msel_"+($id|sub("mrq_";""))),generation:1,coordinator:.requests[$id].request.coordinator,selected_agent_ids:["agent_worker"],created_at:$now}] |
      .requests[$id].claims=[{claim_id:("mrc_"+($id|sub("mrq_";""))),request_id:$id,selection_id:.requests[$id].selections[0].id,agent:{kind:"agent",agent_id:"agent_worker",agent_name:"worker"},state:"reserved",generation:0,failure_count:0,lease_expires_at:$expiry,selected_at:$now,updated_at:$now}]'
    jq --arg id "$id" '.requests[$id] | {request,selection:.selections[0],claims}' "$db" ;;
  'request claim')
    require_actor agent_worker
    require_lease
    [ -n "$key" ] || die 'claim idempotency required'
    check --arg id "$id" '.requests[$id].claims[0].state == "reserved"'
    require_live_request_claim
    if [ "$scenario:$id" = no_retry:mrq_retry ]; then printf '%s\n' 'offline retry assignment unavailable' >&2; exit 1; fi
    update --arg id "$id" --argjson lease "$fixture_lease_seconds" '
      def stamp: (floor | strftime("%Y-%m-%dT%H:%M:%S")) + "." +
        (("000000" + (((. - floor) * 1000000 | floor) | tostring))[-6:]) + "Z";
      $fixture_now as $epoch | ($epoch | stamp) as $now |
      ([($epoch + $lease), (.requests[$id].request.expires_at | fromdateiso8601)] | min | stamp) as $expiry |
      .requests[$id].claims[0] |= (.state="claimed" | .generation=1 | .claimed_at=$now | .updated_at=$now | .lease_expires_at=$expiry)'
    jq --arg id "$id" '{claim:.requests[$id].claims[0]}' "$db" ;;
  'request renew')
    require_actor agent_worker
    require_lease
    check --arg id "$id" --arg claim "$claim" --argjson generation "$generation" '.requests[$id].claims[0] | .state == "claimed" and .claim_id == $claim and .generation == $generation'
    require_live_request_claim
    update --arg id "$id" --argjson lease "$fixture_lease_seconds" --arg scenario "$scenario" '
      def stamp: (floor | strftime("%Y-%m-%dT%H:%M:%S")) + "." +
        (("000000" + (((. - floor) * 1000000 | floor) | tostring))[-6:]) + "Z";
      $fixture_now as $epoch | ($epoch | stamp) as $now |
      ([($epoch + $lease), (.requests[$id].request.expires_at | fromdateiso8601)] | min | stamp) as $expiry |
      .renewals += 1 | .request_renewals[$id]=((.request_renewals[$id]//0)+1) |
      if $scenario == "renew_unchanged_lease" then .
      else .requests[$id].claims[0] |= (.lease_expires_at=$expiry | .updated_at=$now) end'
    case "$scenario" in
      renew_missing_lease) jq --arg id "$id" '{claim:.requests[$id].claims[0]} | del(.claim.lease_expires_at)' "$db" ;;
      renew_wrong_identity) jq --arg id "$id" '{claim:.requests[$id].claims[0]} | .claim.agent.agent_id="agent_unrelated"' "$db" ;;
      *) jq --arg id "$id" '{claim:.requests[$id].claims[0]}' "$db" ;;
    esac ;;
  'request complete')
    require_actor agent_worker
    require_body
    check --arg id "$id" --arg claim "$claim" --argjson generation "$generation" '.requests[$id].claims[0] | .state == "claimed" and .claim_id == $claim and .generation == $generation'
    require_live_request_claim
    if [ "$id" = mrq_first ]; then check '.request_renewals.mrq_first == 1'; else
      check '.request_renewals.mrq_retry == 1 and .messages.msg_answer.read_state.state == "acked" and .messages.msg_question.read_state.state == "acked"'
    fi
    update --arg id "$id" --arg body "$body" '
      .requests[$id].opening_message as $opening |
      ($opening | .id=("msg_result_"+($id|sub("mrq_";""))) | .from={kind:"agent",agent_id:"agent_worker",agent_name:"worker"} | .to={kind:"agent",agent_id:"agent_coordinator",agent_name:"coordinator"} | .kind="result" | .body=$body | .reply_to_message_id=$opening.id | .causal_depth=2) as $message |
      .messages[$message.id]=$message |
      .requests[$id].request.state="completed" | del(.requests[$id].request.phase) |
      .requests[$id].request.completed_at=($fixture_now|strftime("%Y-%m-%dT%H:%M:%SZ")) |
      .requests[$id].claims[0] |= (.state="completed" | .result_message_id=$message.id | .completed_at=($fixture_now|strftime("%Y-%m-%dT%H:%M:%SZ")) | del(.lease_expires_at))'
    jq --arg id "$id" '{request:.requests[$id].request,claim:.requests[$id].claims[0],message:.messages[.requests[$id].claims[0].result_message_id]}' "$db" ;;
  'request cancel')
    require_actor agent_coordinator
    check --arg id "$id" '.requests[$id].request.state == "open"'
    update --arg id "$id" '.requests[$id].request.state="cancelled" | .requests[$id].request.cancelled_at=($fixture_now|strftime("%Y-%m-%dT%H:%M:%SZ")) | del(.requests[$id].request.phase) | .requests[$id].claims |= map(.state="cancelled" | del(.lease_expires_at))'
    jq --arg id "$id" '{request:.requests[$id].request}' "$db" ;;
  'message reply')
    require_actor agent_worker
    require_body
    [ "$id" = msg_open_retry ] && [ "$kind" = question ] || die 'escalation must reply to retry opening'
    check '.requests.mrq_retry.claims[0].state == "claimed" and .messages.msg_question == null'
    update --arg body "$body" '
      .messages.msg_question=(.messages.msg_open_retry | .id="msg_question" | .from={kind:"agent",agent_id:"agent_worker",agent_name:"worker"} | .to={kind:"agent",agent_id:"agent_coordinator",agent_name:"coordinator"} | .kind="question" | .body=$body | .reply_to_message_id="msg_open_retry" | .causal_depth=2 | .processing={state:"available",generation:0,failure_count:0})'
    jq '{message:.messages.msg_question}' "$db" ;;
  'message claim')
    require_actor agent_coordinator
    require_lease
    [ "$id" = msg_question ] && [ -n "$key" ] || die 'only ordinary escalation is claimable'
    check '.messages.msg_question.processing.state == "available"'
    update --argjson lease "$fixture_lease_seconds" '.messages.msg_question.processing={state:"claimed",claim_id:"mcl_question",generation:1,failure_count:0,lease_expires_at:($fixture_now+$lease|strftime("%Y-%m-%dT%H:%M:%SZ"))}'
    jq '{processing:.messages.msg_question.processing}' "$db" ;;
  'message complete')
    require_actor agent_coordinator
    require_body
    [ "$id" = msg_question ] && [ "$kind" = answer ] || die 'answer must complete escalation'
    check --arg claim "$claim" --argjson generation "$generation" '.messages.msg_question.processing | .state == "claimed" and .claim_id == $claim and .generation == $generation'
    check '.messages.msg_question.read_state.state == "read"'
    if [ "$scenario" = no_answer ] || [ "$scenario" = cleanup_wrong_question ]; then
      update '.answer_attempted=true'
      printf '%s\n' 'offline escalation answer unavailable' >&2
      exit 1
    fi
    update --arg body "$body" '
      .messages.msg_answer=(.messages.msg_question | .id="msg_answer" | .from={kind:"agent",agent_id:"agent_coordinator",agent_name:"coordinator"} | .to={kind:"agent",agent_id:"agent_worker",agent_name:"worker"} | .kind="answer" | .body=$body | .reply_to_message_id="msg_question" | .causal_depth=3 | .processing={state:"available",generation:0,failure_count:0} | .read_state={state:"unread"}) |
      .messages.msg_question.processing |= (.state="completed" | .result_message_id="msg_answer" | .completed_at=($fixture_now|strftime("%Y-%m-%dT%H:%M:%SZ")) | del(.lease_expires_at))'
    jq '{processing:.messages.msg_question.processing,message:.messages.msg_answer}' "$db" ;;
  'message read'|'message ack')
    check --arg id "$id" --arg actor "$actor" '.messages[$id].to.agent_id == $actor'
    if [ "$id" = msg_question ]; then
      check '.messages.msg_question.processing.state == "claimed" or .messages.msg_question.processing.state == "completed"'
    fi
    if [ "$operation" = 'message ack' ]; then
      check --arg id "$id" '.messages[$id].read_state.state == "read" or .messages[$id].read_state.state == "acked"'
      [ "$id" != msg_question ] || check '.messages.msg_question.processing.state == "completed"'
      update --arg id "$id" '.messages[$id].read_state |= (.state="acked" | .acked_at=($fixture_now|strftime("%Y-%m-%dT%H:%M:%SZ")))'
    else
      update --arg id "$id" 'if .messages[$id].read_state.state != "acked" then .messages[$id].read_state |= (.state="read" | .read_at=($fixture_now|strftime("%Y-%m-%dT%H:%M:%SZ"))) else . end'
    fi
    if [ "$scenario:$operation:$id" = 'cleanup_wrong_question:message read:msg_question' ] &&
      jq -e '.answer_attempted == true' "$db" >/dev/null; then
      jq --arg id "$id" '{message:(.messages[$id] | del(.processing.claim_id,.processing.lease_expires_at))} | .message.id="msg_unrelated" | .message.processing.state="completed"' "$db"
    else
      jq --arg id "$id" '{message:(.messages[$id] | del(.processing.claim_id,.processing.lease_expires_at))}' "$db"
    fi ;;
  'message release')
    require_actor agent_coordinator
    [ "$id" = msg_question ] || die 'release target'
    check --arg claim "$claim" --argjson generation "$generation" '.messages.msg_question.processing | .state == "claimed" and .claim_id == $claim and .generation == $generation'
    update '.messages.msg_question.processing |= (.state="available" | del(.claim_id,.lease_expires_at))'
    jq '{processing:.messages.msg_question.processing}' "$db" ;;
esac
SHIM
chmod 700 "$work_dir/bin/witself"
export PATH="$work_dir/bin:$PATH"
export FAKE_COORDINATOR_TOKEN_FILE="$work_dir/coordinator.token"
export FAKE_WORKER_TOKEN_FILE="$work_dir/worker.token"
printf '%s\n' 'offline_coordinator_token_324ba871_secret_fixture' >"$FAKE_COORDINATOR_TOKEN_FILE"
printf '%s\n' 'offline_worker_token_989c51d7_secret_fixture' >"$FAKE_WORKER_TOKEN_FILE"
chmod 600 "$FAKE_COORDINATOR_TOKEN_FILE" "$FAKE_WORKER_TOKEN_FILE"

fail() {
  printf 'FAIL: %s\n' "$1" >&2
  if [ -n "${FAKE_COLLABORATION_STATE:-}" ] && [ -f "$FAKE_COLLABORATION_STATE/state.json" ]; then
    jq '{opens,requests:[.requests[] | {id:.request.id,state:.request.state,claims:[.claims[]|{state,generation,failure_count}]}]}' "$FAKE_COLLABORATION_STATE/state.json" >&2
    [ ! -f "$FAKE_COLLABORATION_STATE/stderr" ] || cat "$FAKE_COLLABORATION_STATE/stderr" >&2
  fi
  exit 1
}
pass() { printf 'PASS: %s\n' "$1"; }
init_case() {
  local scenario="$1"
  export FAKE_COLLABORATION_SCENARIO="$scenario"
  export FAKE_COLLABORATION_STATE="$work_dir/cases/$scenario"
  mkdir "$FAKE_COLLABORATION_STATE"
  printf '%s\n' '{"opens":0,"renewals":0,"requests":{},"messages":{}}' >"$FAKE_COLLABORATION_STATE/state.json"
  case "$scenario" in delayed_open|lost_open_deadline|pending_candidates|slow_retry_calls|late_retry_calls) date -u +%s >"$FAKE_COLLABORATION_STATE/clock" ;; esac
  if [[ "$scenario" == unowned_open_* ]]; then
    jq '
      {id:"msg_existing",account_id:"acc_founder",realm_id:"realm_founder",
        from:{kind:"agent",agent_id:"agent_coordinator"},to:{kind:"realm",count:1},
        kind:"open_request",body:"Existing unrelated production task.",
        thread_id:"thr_existing",causal_depth:1} as $opening |
      .requests.mrq_existing={
        request:{id:"mrq_existing",account_id:"acc_founder",realm_id:"realm_founder",
          coordinator:$opening.from,opening_message_id:$opening.id,state:"open",
          selection_policy:"client_ranked",max_assignees:1},
        opening_message:$opening,
        selections:[{id:"msel_existing",generation:1,selected_agent_ids:["agent_worker"]}],
        claims:[{claim_id:"mrc_existing",request_id:"mrq_existing",selection_id:"msel_existing",
          agent:{kind:"agent",agent_id:"agent_worker"},state:"reserved",generation:0,
          failure_count:0,lease_expires_at:"2099-01-01T00:00:00Z"}]} |
      .messages[$opening.id]=$opening' "$FAKE_COLLABORATION_STATE/state.json" >"$FAKE_COLLABORATION_STATE/before.json"
    cp "$FAKE_COLLABORATION_STATE/before.json" "$FAKE_COLLABORATION_STATE/state.json"
  fi
  : >"$FAKE_COLLABORATION_STATE/calls"
}
run_case() {
  local poll_budget=2
  # Successful pagination/recovery must allow time for separate shell and jq
  # processes on this host. Intentional timeout scenarios keep a short budget.
  case "$1" in happy|paginated|lost_open_once) poll_budget=10 ;; delayed_open|lost_open_deadline) poll_budget=30 ;; slow_retry_calls|late_retry_calls) poll_budget=1 ;; esac
  init_case "$1"
  result="$FAKE_COLLABORATION_STATE/record.json"
  diagnostic="$FAKE_COLLABORATION_STATE/stderr"
  status=0
  bash "$canary" \
    --coordinator-endpoint https://coordinator.invalid --coordinator-token-file "$FAKE_COORDINATOR_TOKEN_FILE" --coordinator-agent coordinator \
    --worker-endpoint https://worker.invalid --worker-token-file "$FAKE_WORKER_TOKEN_FILE" --worker-agent worker \
    --realm Founder --timeout-seconds "$poll_budget" --out "$result" --redact-check \
    >"$FAKE_COLLABORATION_STATE/stdout" 2>"$diagnostic" || status=$?
}
assert_json() { jq -e "$1" "$2" >/dev/null || fail "$3"; }

for fault in other_run other_attempt body request_account request_realm coordinator coordinator_kind opening_account opening_realm sender sender_kind recipient kind opening_id thread parent depth; do
  run_case "unowned_open_$fault"
  [ "$status" != 0 ] || fail "$fault opening ownership mismatch should fail"
  cmp -s "$FAKE_COLLABORATION_STATE/before.json" "$FAKE_COLLABORATION_STATE/state.json" || fail "$fault opening mismatch changed unrelated work or its reservation"
  assert_json '.pass == false and .legs[0].pass == false and .cleanup == "failed" and
    .request_id == "" and .retry_request_id == "" and all(.transitions[]; .event == "failed")' \
    "$result" "$fault unverified request must not be retained as owned"
  jq -Rse '(split("\n") | map(select(length > 0))) as $calls |
    ($calls | map(select(contains("|request open|"))) | length) == 2 and
    all($calls[]; contains("|self show|") or contains("|request open|"))' \
    "$FAKE_COLLABORATION_STATE/calls" >/dev/null || fail "$fault must retry the exact opening key without acting on the unverified request"
  case "$(cat "$diagnostic")" in *'open outcome indeterminate; exact-key recovery failed'*) ;; *) fail "$fault missing indeterminate cleanup diagnostic" ;; esac
  pass "$fault opening mismatch: initial response and exact-key recovery leave unrelated work untouched"
done

for body_source in stdin file; do
  for trailing_newlines in 0 1 2; do
    init_case "body_${body_source}_$trailing_newlines"
    expected_body="$FAKE_COLLABORATION_STATE/body.txt"
    printf '%s' 'offline fixture body.' >"$expected_body"
    for ((newline = 0; newline < trailing_newlines; newline++)); do
      printf '\n' >>"$expected_body"
    done
    body_args=(--body-stdin)
    if [ "$body_source" = file ]; then body_args=(--body-file "$expected_body"); fi
    witself message request open \
      --endpoint https://coordinator.invalid --token-file "$FAKE_COORDINATOR_TOKEN_FILE" \
      --agent coordinator --realm Founder --json --selection-policy client_ranked \
      --max-assignees 1 --offer-window 1s --expires-in 60s --idempotency-key fixture-body \
      "${body_args[@]}" <"$expected_body" >"$FAKE_COLLABORATION_STATE/response.json" || fail 'body fixture open'
    jq -e --rawfile expected "$expected_body" '.opening_message.body == $expected' \
      "$FAKE_COLLABORATION_STATE/response.json" >/dev/null || fail "$body_source fixture changed $trailing_newlines trailing newlines"
  done
  pass "body-$body_source fixture preserves zero, one, and multiple trailing newlines"
done

run_case happy
if [ "$status" != 0 ]; then cat "$diagnostic" >&2; fail 'happy path exited nonzero'; fi
assert_json '.schema == "witself.collaboration-canary.v1" and .pass == true and (.legs|length)==9 and ([.legs[].leg] == ["a","b","c","d","e","f","g","h","i"]) and all(.legs[]; .pass == true) and .realm_id == "realm_founder" and .request_id == "mrq_first" and .retry_request_id == "mrq_retry"' "$result" 'passing nine-leg record'
assert_json '.release_version == "0.0.342" and .coordinator_agent_id == "agent_coordinator" and .worker_agent_id == "agent_worker" and (.transitions|length)>9 and all(.transitions[]; (.at|test("^[0-9]{4}-[0-9]{2}-[0-9]{2}T")) and .polls >= 1) and any(.transitions[]; .event == "escalated" and .message.causal_depth == 2) and any(.transitions[]; .event == "answered" and .message.causal_depth == 3)' "$result" 'record version, identities, ordered observations, and causal depths'
# These variables belong to jq, not the shell.
# shellcheck disable=SC2016
assert_json '(.transitions | map(select(.event == "claimed" and .attempt == 1)) | .[0].claims[0]) as $claimed |
  (.transitions | map(select(.event == "renewed")) | .[0].claims[0]) as $renewed |
  ($claimed.lease_expires_at | test("\\.[0-9]{6}Z$")) and
  ($renewed.lease_expires_at | test("\\.[0-9]{6}Z$")) and
  $renewed.lease_expires_at > $claimed.lease_expires_at and $renewed.updated_at > $claimed.updated_at' \
  "$result" 'renewal must advance the lease and timestamp with realistic microsecond precision'
assert_json '.opens == 2 and .renewals == 2 and .request_renewals == {"mrq_first":1,"mrq_retry":1} and all(.requests[]; .request.state == "completed" and .claims[0].failure_count == 0) and .messages.msg_result_first.read_state.state == "acked" and .messages.msg_result_retry.read_state.state == "acked" and .messages.msg_question.read_state.state == "acked" and .messages.msg_answer.read_state.state == "acked"' "$FAKE_COLLABORATION_STATE/state.json" 'terminal protocol state'
# shellcheck disable=SC2016
assert_json '(.requests.mrq_first.opening_message.body | capture("run=(?<run>[^;]+); attempt=(?<attempt>[0-9]+)")) as $first |
  (.requests.mrq_retry.opening_message.body | capture("run=(?<run>[^;]+); attempt=(?<attempt>[0-9]+)")) as $retry |
  $first.run == $retry.run and $first.attempt == "1" and $retry.attempt == "2"' \
  "$FAKE_COLLABORATION_STATE/state.json" 'opening bodies must identify this run and the individual attempt'
jq -e --slurpfile state "$FAKE_COLLABORATION_STATE/state.json" '
  ($state[0].requests.mrq_first.opening_message.body | capture("run=(?<run>[^;]+)").run) as $run |
  all(.. | strings; contains($run) | not)' "$result" >/dev/null || fail 'record must not retain the run nonce'
if [[ "$(cat "$FAKE_COLLABORATION_STATE/calls")" == *'request cancel'* ]]; then fail 'happy path must not cancel closed work'; fi
happy_record="$result"
pass 'happy path: all nine legs, two terminal requests, one renewal per attempt, exact claims, acknowledgements, no cancel'

for scenario in paginated lost_open_once delayed_open lost_open_deadline pending_candidates; do
  run_case "$scenario"
  if [ "$status" != 0 ]; then cat "$diagnostic" >&2; fail "$scenario should pass"; fi
  assert_json '.pass == true and all(.legs[]; .pass == true)' "$result" "$scenario passing record"
  assert_json '.opens == 2 and all(.requests[]; .request.state == "completed")' "$FAKE_COLLABORATION_STATE/state.json" "$scenario must open exactly two requests"
  if [ "$scenario" = paginated ]; then
    [ "$(wc -l <"$FAKE_COLLABORATION_STATE/second-page" | tr -d ' ')" = 2 ] || fail 'must discover both attempts through a next cursor'
  elif [ "$scenario" = lost_open_once ] || [ "$scenario" = lost_open_deadline ]; then
    jq -Rse '(split("\n") | map(select(contains("|request open|"))) | length) == 3' "$FAKE_COLLABORATION_STATE/calls" >/dev/null || fail 'lost open must replay its exact idempotency key once'
  fi
  if [ "$scenario" = delayed_open ] || [ "$scenario" = lost_open_deadline ]; then
    jq -e 'all(.requests[]; .request.offer_count == 1 and .request.state == "completed")' \
      "$FAKE_COLLABORATION_STATE/state.json" >/dev/null || fail 'delayed discovery must offer before the backend deadline'
  fi
  if [ "$scenario" = pending_candidates ]; then
    assert_json 'all(.requests[]; .request.candidate_count == 2 and .request.offer_count == 1)' \
      "$FAKE_COLLABORATION_STATE/state.json" 'selection must wait through the offer deadline while another candidate stays pending'
  fi
  pass "$scenario: nine legs pass with exactly two durable requests"
done

for scenario in slow_retry_calls late_retry_calls; do
  run_case "$scenario"
  if [ "$status" != 0 ]; then cat "$diagnostic" >&2; fail "$scenario should pass"; fi
  assert_json '.pass == true and all(.legs[]; .pass == true) and
    ([.transitions[]|select(.event=="renewed")|.attempt] == [1,2])' "$result" "$scenario must maintain both exact claim fences"
  assert_json '.timed_retry_calls == 9 and .request_renewals == {"mrq_first":1,"mrq_retry":1} and
    all(.requests[]; .request.state == "completed")' "$FAKE_COLLABORATION_STATE/state.json" "$scenario must complete after slow calls without expired claims"
  if [ "$scenario" = late_retry_calls ]; then
    assert_json '(.requests.mrq_retry.request.expires_at|fromdateiso8601)-
      (.requests.mrq_retry.request.created_at|fromdateiso8601) > 140' "$FAKE_COLLABORATION_STATE/state.json" \
      'request lifetime must not clip a correctly sized lease at the previous 140s expiry'
  fi
  pass "$scenario: six-second calls preserve worker lease and request lifetime through completion"
done

for scenario in wrong_run_discovery wrong_attempt_discovery; do
  run_case "$scenario"
  [ "$status" != 0 ] || fail "$scenario should fail"
  assert_json '.pass == false and .cleanup == "cancelled" and .legs[1].pass == false and
    all(.transitions[]; .event != "offered")' "$result" "$scenario must fail before offering"
  assert_json '.requests.mrq_first.request.state == "cancelled" and .requests.mrq_first.request.offer_count == 0' \
    "$FAKE_COLLABORATION_STATE/state.json" "$scenario must cancel only the already verified owned request"
  case "$(cat "$diagnostic")" in *'discovered request marker or run identity mismatch'*) ;; *) fail "$scenario missing precise diagnostic" ;; esac
  pass "$scenario: discovery refuses a body from a different run or attempt"
done

for scenario in no_discovery no_offer no_retry no_answer cleanup_wrong_request cleanup_wrong_question; do
  run_case "$scenario"
  [ "$status" != 0 ] || fail "$scenario should fail"
  case "$scenario" in
    no_discovery|no_offer|cleanup_wrong_request) failed_leg=b; cancelled=mrq_first ;;
    no_retry) failed_leg=e; cancelled=mrq_retry ;;
    no_answer|cleanup_wrong_question) failed_leg=g; cancelled=mrq_retry ;;
  esac
  assert_json ".pass == false and .cleanup == \"cancelled\" and any(.legs[]; .leg == \"$failed_leg\" and .pass == false)" "$result" "$scenario failed-leg record"
  assert_json ".requests.$cancelled.request.state == \"cancelled\" and all(.requests[].claims[]; .state != \"claimed\" and .state != \"reserved\" and .lease_expires_at == null)" "$FAKE_COLLABORATION_STATE/state.json" "$scenario must cancel remaining open request without a live lease"
  [ -s "$diagnostic" ] || fail "$scenario missing diagnostic"
  case "$(cat "$diagnostic")" in *[Tt]imeout*|*'timed out'*) ;; *) fail "$scenario missing timeout diagnostic" ;; esac
  case "$scenario" in
    no_discovery)
      assert_json 'all(.transitions[]; .event != "discovered")' "$result" 'undiscovered request must not reach offer'
      case "$(cat "$FAKE_COLLABORATION_STATE/calls")" in *'request offer'*) fail 'undiscovered request must not offer' ;; esac ;;
    no_offer|cleanup_wrong_request)
      assert_json 'any(.transitions[]; .event == "discovered") and all(.transitions[]; .event != "offered")' "$result" 'offer failure must follow successful request discovery'
      case "$(cat "$FAKE_COLLABORATION_STATE/calls")" in *'request offer'*) ;; *) fail 'offer failure must attempt the offer mutation' ;; esac
      case "$(cat "$diagnostic")" in *'timeout waiting for worker offer'*) ;; *) fail 'offer failure needs the specific worker-offer diagnostic' ;; esac ;;
    no_answer|cleanup_wrong_question)
      assert_json '.messages.msg_question.processing | .state == "available" and .lease_expires_at == null and .failure_count == 0' \
        "$FAKE_COLLABORATION_STATE/state.json" "$scenario must release the exact question lease without counting provider failure"
      case "$(cat "$FAKE_COLLABORATION_STATE/calls")" in *'message release|msg_question'*) ;; *) fail "$scenario must release the owned question" ;; esac
      case "$(cat "$diagnostic")" in *'timeout waiting for escalation answer'*) ;; *) fail 'answer failure needs the specific escalation diagnostic' ;; esac ;;
  esac
  pass "$scenario: bounded timeout at leg $failed_leg and cancelled $cancelled"
done

for scenario in renew_missing_lease renew_unchanged_lease renew_wrong_identity; do
  run_case "$scenario"
  [ "$status" != 0 ] || fail "$scenario should fail"
  assert_json '.pass == false and .cleanup == "cancelled" and any(.legs[]; .leg == "d" and .pass == false) and all(.transitions[]; .event != "renewed")' \
    "$result" "$scenario must fail renewal at leg d before retaining a successful renewal"
  assert_json '.opens == 1 and .renewals == 1 and .requests.mrq_first.request.state == "cancelled" and all(.requests[].claims[]; .state != "claimed" and .state != "reserved" and .lease_expires_at == null and .failure_count == 0)' \
    "$FAKE_COLLABORATION_STATE/state.json" "$scenario must cancel the exact request and leave no live lease"
  case "$(cat "$diagnostic")" in *'leg d: renewal fence or lease deviation'*) ;; *) fail "$scenario missing renewal deviation diagnostic" ;; esac
  pass "$scenario: rejected at leg d and owned request cancelled without a live lease"
done

scan_record() {
  bash "$canary" --redact-check --out "$1" \
    --coordinator-token-file "$FAKE_COORDINATOR_TOKEN_FILE" --worker-token-file "$FAKE_WORKER_TOKEN_FILE"
}
scan_record "$happy_record" >"$work_dir/scan.stdout" 2>"$work_dir/scan.stderr" || fail 'clean record scan'
for contamination in body token body_key token_key; do
  dirty="$work_dir/dirty-$contamination.json"
  case "$contamination" in
    body) jq '.injected="WITSELF_COLLABORATION_CANARY_V1"' "$happy_record" >"$dirty" ;;
    token) jq --rawfile token "$FAKE_COORDINATOR_TOKEN_FILE" '.injected=($token|rtrimstr("\n"))' "$happy_record" >"$dirty" ;;
    body_key) jq '.["WITSELF_COLLABORATION_CANARY_V1"]=true' "$happy_record" >"$dirty" ;;
    token_key) jq --rawfile token "$FAKE_COORDINATOR_TOKEN_FILE" '.[($token|rtrimstr("\n"))]=true' "$happy_record" >"$dirty" ;;
  esac
  if scan_record "$dirty" >"$work_dir/scan.stdout" 2>"$work_dir/scan.stderr"; then fail "$contamination contamination passed redaction"; fi
  [ -s "$work_dir/scan.stderr" ] || fail 'redaction diagnostic absent'
  pass "redaction rejects $contamination contamination"
done

for document_case in body_first coordinator_token_first worker_token_first multiple_documents empty; do
  dirty="$work_dir/dirty-$document_case.json"
  case "$document_case" in
    body_first) printf '%s\n' '{"injected":"WITSELF_COLLABORATION_CANARY_V1"}' '{}' >"$dirty" ;;
    coordinator_token_first|worker_token_first)
      fixture_token="$FAKE_COORDINATOR_TOKEN_FILE"
      if [ "$document_case" = worker_token_first ]; then fixture_token="$FAKE_WORKER_TOKEN_FILE"; fi
      jq -n --rawfile token "$fixture_token" '{injected:($token|rtrimstr("\n"))}' >"$dirty"
      printf '%s\n' '{}' >>"$dirty" ;;
    multiple_documents) printf '%s\n' '{}' '{}' >"$dirty" ;;
    empty) : >"$dirty" ;;
  esac
  if scan_record "$dirty" >"$work_dir/scan.stdout" 2>"$work_dir/scan.stderr"; then fail "$document_case input passed redaction"; fi
  [ -s "$work_dir/scan.stderr" ] || fail "$document_case redaction diagnostic absent"
  pass "redaction rejects $document_case JSON input"
done

run_case malformed_open
[ "$status" != 0 ] || fail 'malformed open response should fail'
assert_json '.pass == false and .legs[0].pass == false' "$result" 'malformed opening must fail leg a'
assert_json '.opens == 1 and .requests.mrq_first.request.state == "cancelled"' "$FAKE_COLLABORATION_STATE/state.json" 'owned ID must be cancelled after remaining opening fields fail validation'
pass 'malformed opening policy: fails leg a and cancels the known owned request'

for scenario in body_metadata token_metadata; do
  run_case "$scenario"
  [ "$status" != 0 ] || fail "$scenario should fail"
  [ ! -f "$result" ] || fail "$scenario evidence must be withheld"
  assert_json 'all(.requests[]; .request.state != "open") and all(.requests[].claims[]; .state != "claimed" and .state != "reserved")' "$FAKE_COLLABORATION_STATE/state.json" "$scenario leaves no live request lease"
  jq -Rse --rawfile token "$FAKE_COORDINATOR_TOKEN_FILE" --rawfile worker "$FAKE_WORKER_TOKEN_FILE" '
    (contains("WITSELF_COLLABORATION_CANARY_V1")|not) and
    (contains($token|rtrimstr("\n"))|not) and (contains($worker|rtrimstr("\n"))|not)
  ' "$diagnostic" "$FAKE_COLLABORATION_STATE/stdout" >/dev/null || fail "$scenario leaked content to output"
  pass "$scenario: contaminated evidence withheld without leaked output or live request lease"
done

run_case wrong_realm
[ "$status" != 0 ] || fail 'realm mismatch should fail'
assert_json '.opens == 0' "$FAKE_COLLABORATION_STATE/state.json" 'realm mismatch must issue zero requests'
pass 'binding preflight rejects different realms before opening'

bash "$canary" --help >"$work_dir/help" 2>"$work_dir/help.stderr" || fail 'help'
case "$(cat "$work_dir/help")" in *--coordinator-endpoint*--worker-*|*--worker-*--coordinator-endpoint*) ;; *) fail 'help lacks explicit bindings' ;; esac
for argument in --unknown --coordinator-endpoint --timeout-seconds; do
  status=0
  bash "$canary" "$argument" >"$work_dir/usage.stdout" 2>"$work_dir/usage.stderr" || status=$?
  [ "$status" = 2 ] || fail "usage error should exit 2: $argument"
done
status=0
bash "$canary" >"$work_dir/usage.stdout" 2>"$work_dir/usage.stderr" || status=$?
[ "$status" = 2 ] || fail 'missing required arguments should exit 2'
pass 'help and usage errors'
printf '%s\n' 'All collaboration canary tests passed (offline PATH shim).'
