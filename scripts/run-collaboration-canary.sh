#!/usr/bin/env bash
# Client-driven protocol evidence. Never run this script during offline validation.
set -euo pipefail
umask 077

usage() {
  cat <<'USAGE'
usage: run-collaboration-canary.sh
  --coordinator-endpoint URL --coordinator-token-file FILE --coordinator-agent NAME
  --worker-endpoint URL --worker-token-file FILE --worker-agent NAME --out FILE
  [--realm NAME] [--timeout-seconds 1..120] [--redact-check]

Drive nine collaboration legs using two explicit agent bindings. FILE must not
exist. Agent names are verified against the token identities; realm defaults to
default. Each ordinary poll has a bounded deadline (default 30 seconds) and
proportional backoff. Selection additionally allows the offer window to close.
All evidence is scanned before publication, including failure records.

Offline scan of an existing record, without invoking witself:
  --redact-check --out FILE --coordinator-token-file FILE --worker-token-file FILE
USAGE
}
usage_error() { usage >&2; exit 2; }
coordinator_endpoint='' worker_endpoint='' coordinator_token_file='' worker_token_file=''
coordinator_agent='' worker_agent='' out='' realm=default timeout_seconds=30 redact_check=false
while (($#)); do
  case "$1" in
    --help|-h) usage; exit 0 ;;
    --redact-check) redact_check=true; shift ;;
    --coordinator-endpoint|--worker-endpoint|--coordinator-token-file|--worker-token-file|--coordinator-agent|--worker-agent|--out|--realm|--timeout-seconds)
      [[ $# -ge 2 && -n "$2" && "$2" != --* ]] || usage_error
      case "$1" in
        --coordinator-endpoint) coordinator_endpoint=$2 ;;
        --worker-endpoint) worker_endpoint=$2 ;;
        --coordinator-token-file) coordinator_token_file=$2 ;;
        --worker-token-file) worker_token_file=$2 ;;
        --coordinator-agent) coordinator_agent=$2 ;;
        --worker-agent) worker_agent=$2 ;;
        --out) out=$2 ;;
        --realm) realm=$2 ;;
        --timeout-seconds) timeout_seconds=$2 ;;
      esac
      shift 2 ;;
    *) usage_error ;;
  esac
done
[[ -n "$out" && -f "$coordinator_token_file" && -r "$coordinator_token_file" &&
   -s "$coordinator_token_file" && -f "$worker_token_file" && -r "$worker_token_file" &&
   -s "$worker_token_file" ]] || usage_error
command -v jq >/dev/null || { echo 'collaboration canary: jq is required' >&2; exit 2; }

# Every body starts with this fixed marker. It is never a record field.
marker=WITSELF_COLLABORATION_CANARY_V1
redaction_scan() {
  # Later JSON documents must not override a failed scan's exit status.
  jq -se --arg marker "$marker" --arg nonce "${run_key:-}" --rawfile coordinator "$coordinator_token_file" \
    --rawfile worker "$worker_token_file" '
    def trim: gsub("^\\s+|\\s+$"; "");
    length == 1 and (.[0] |
      [$coordinator | trim, $worker | trim] as $secrets
      | ([.. | strings] + [.. | objects | keys[]]) as $strings
      | ($secrets | all(length > 0)) and
        ($strings | all(. as $s |
          (contains($marker) | not) and
          (($nonce | length) == 0 or ($s | contains($nonce) | not)) and
          (test("https?://|/Users/|/home/|~/"; "i") | not) and
          ($secrets | all(. as $secret | $s | contains($secret) | not)))))
  ' "$1" >/dev/null 2>&1
}
if [[ "$redact_check" == true && -z "$coordinator_endpoint$worker_endpoint$coordinator_agent$worker_agent" ]]; then
  [[ -f "$out" ]] || usage_error
  redaction_scan "$out" || { echo 'collaboration canary: redaction self-check failed' >&2; exit 1; }
  echo 'collaboration canary: redaction self-check passed'
  exit 0
fi
[[ "$timeout_seconds" =~ ^[1-9][0-9]*$ && ${#timeout_seconds} -le 3 ]] || usage_error
((timeout_seconds <= 120)) || usage_error
[[ "$coordinator_agent" =~ ^[A-Za-z0-9._-]+$ && "$worker_agent" =~ ^[A-Za-z0-9._-]+$ &&
   "$realm" =~ ^[A-Za-z0-9._-]+$ && "$coordinator_agent" != "$worker_agent" ]] || usage_error
# Refuse credential-bearing URL syntax; never copy either endpoint to evidence.
for endpoint in "$coordinator_endpoint" "$worker_endpoint"; do
  [[ "$endpoint" =~ ^https?://[^/?#@[:space:]]+(/[^?#@[:space:]]*)?$ ]] || usage_error
done
[[ ! -e "$out" && ! -L "$out" ]] || usage_error
out_parent=$(dirname "$out")
[[ -d "$out_parent" && -w "$out_parent" ]] || usage_error
command -v witself >/dev/null || { echo 'collaboration canary: witself is required' >&2; exit 2; }

scratch=$(mktemp -d "${TMPDIR:-/tmp}/witself-collaboration.XXXXXX")
publish_tmp=
transitions=$scratch/transitions.jsonl
: >"$transitions"
started_at=$(date -u +%Y-%m-%dT%H:%M:%SZ)
run_key="collaboration-$(date -u +%Y%m%dT%H%M%SZ)-$$-${RANDOM}"
release_version='' realm_id='' account_id='' coordinator_id='' worker_id=''
request_id='' retry_request_id='' active_request='' opening_id='' opening_thread='' opening_depth=0
claim_id='' claim_generation=0 claim_lease='' selection_id='' reserved_claim_id='' reserved_generation=0
question_id='' question_claim='' question_generation=0
attempt=0 leg=a passed_legs='' cleanup_status=not_needed success=false
send_body='' response='' poll_count=0 opening_pending=false question_claim_pending=false
# Opening (including exact-key retries), paginated discovery, marker show, and
# offering each have a polling budget. Discovery can enter its last inner poll
# near its outer deadline, so reserve five budgets plus four final HTTP calls.
# 1..120s input yields 70..665s, below the CLI/server's 900s offer-window ceiling.
http_timeout_seconds=15
offer_seconds=$((timeout_seconds * 5 + http_timeout_seconds * 4 + 5))
selection_wait_seconds=$((offer_seconds + timeout_seconds + http_timeout_seconds))
# A poll can finish on one final HTTP call after its deadline. Each retry half
# makes four polls; reserve five such operations plus headroom, and renew once
# between the halves. Leases stay 110..705s, below the 900s server limit.
operation_seconds=$((timeout_seconds + http_timeout_seconds))
lease_seconds=$((operation_seconds * 5 + 30))
# Request expiry caps every reservation and lease. Reserve the whole offer
# window and its separate selection wait, then claim, conversation, and terminal
# observations. 456..3550s stays below the server's seven-day maximum, including
# a selection poll that spends its full budget recovering transient failures.
expiry_seconds=$((offer_seconds + selection_wait_seconds + operation_seconds * 15 + 60))
opening_body() {
  printf '%s: run=%s; attempt=%s; exercise only synthetic collaboration; no external actions or expanded authority.' \
    "$marker" "$run_key" "$attempt"
}

die() { echo "collaboration canary: leg $leg: $1" >&2; exit 1; }

# Explicit endpoint+token-file takes the CLI's direct connection branch, without
# looking up any other binding. Raw CLI stdout/stderr never reaches the terminal.
# The CLI HTTP client bounds each call at 15s (internal/client/client.go).
api() {
  local actor=$1 endpoint token_file agent
  shift
  if [[ "$actor" == coordinator ]]; then
    endpoint=$coordinator_endpoint token_file=$coordinator_token_file agent=$coordinator_agent
  else
    endpoint=$worker_endpoint token_file=$worker_token_file agent=$worker_agent
  fi
  printf '%s' "$send_body" | witself "$@" --endpoint "$endpoint" \
    --token-file "$token_file" --agent "$agent" --realm "$realm" --json \
    >"$scratch/response.json" 2>/dev/null
}

# Retry only observations or idempotent mutations. Backoff scales with the poll
# budget, doubles to a tenth of that budget, and never exceeds remaining time.
poll() {
  poll_with_budget "$timeout_seconds" "$@"
}
poll_with_budget() {
  local budget=$1 label=$2 actor=$3 predicate=$4 deadline delay
  deadline=$((SECONDS + budget))
  shift 4
  poll_count=0
  while true; do
    poll_count=$((poll_count + 1))
    if api "$actor" "$@" && jq -e "$predicate" "$scratch/response.json" >/dev/null 2>&1; then
      response=$(cat "$scratch/response.json")
      return 0
    fi
    ((SECONDS < deadline)) || die "timeout waiting for $label"
    delay=$(jq -nr --argjson budget "$budget" --argjson remaining "$((deadline - SECONDS))" \
      --argjson n "$poll_count" '[($budget / 100 * pow(2; ([$n - 1, 4] | min))), ($budget / 10), $remaining] | min')
    sleep "$delay"
  done
}
check() { jq -e "$1" <<<"$response" >/dev/null 2>&1 || die "$2"; }
field() { jq -er "$1" <<<"$response" 2>/dev/null || die 'missing protocol identity'; }

# An opening ID is tentative until its body, bound identities, and root message
# graph agree. Share this gate with exact-key recovery; policy deviations can
# still be cleaned up after ownership is established.
owned_open_request_id() {
  jq -ser --arg account "$account_id" --arg realm "$realm_id" \
    --arg coordinator "$coordinator_id" --arg body "$send_body" '
    def id($prefix): type == "string" and test("^" + $prefix + "_[A-Za-z0-9_-]{1,128}$");
    select(length == 1) | .[0] |
    select((.request.id | id("mrq")) and
      .request.account_id == $account and .request.realm_id == $realm and
      .request.coordinator.kind == "agent" and .request.coordinator.agent_id == $coordinator and
      .opening_message.account_id == $account and .opening_message.realm_id == $realm and
      .opening_message.from.kind == "agent" and .opening_message.from.agent_id == $coordinator and
      .opening_message.to.kind == "realm" and .opening_message.kind == "open_request" and
      .opening_message.body == $body and
      (.opening_message.id | id("msg")) and .opening_message.id == .request.opening_message_id and
      (.opening_message.thread_id | id("thr")) and
      .opening_message.reply_to_message_id == null and .opening_message.causal_depth == 1) |
    .request.id
  ' 2>/dev/null
}

# Whitelist metadata rather than redact a copied response. String/enum validation
# prevents a server field from smuggling content into an ID or state column.
record() {
  local event=$1
  jq -c --arg at "$(date -u +%Y-%m-%dT%H:%M:%SZ)" --arg leg "$leg" \
    --arg event "$event" --argjson attempt "$attempt" --argjson polls "$poll_count" '
    def id: if type == "string" and test("^(agent|realm|mrq|msel|mrc|mcl|msg|thr)_[A-Za-z0-9_-]{1,128}$") then . else error("id") end;
    def count: if type == "number" and . >= 0 and floor == . then . else error("count") end;
    def state: if . == null then null elif IN("open","completed","cancelled","expired","collecting_offers","awaiting_selection","assigned","reserved","claimed","released","available","unread","read","acked") then . else error("state") end;
    def stamp: if . == null then null elif type == "string" and test("^[0-9T:.+Z-]+$") then . else error("timestamp") end;
    def claim: {claim_id:(.claim_id|id),request_id:(.request_id|id),selection_id:(.selection_id|id),
      agent_id:(.agent.agent_id|id),state:(.state|state),generation:(.generation|count),
      failure_count:(.failure_count|count),lease_expires_at:(.lease_expires_at|stamp),
      updated_at:(.updated_at|stamp),result_message_id:(if .result_message_id then .result_message_id|id else null end)};
    def message: {message_id:(.id|id),thread_id:(.thread_id|id),
      reply_to_message_id:(if .reply_to_message_id then .reply_to_message_id|id else null end),
      causal_depth:(.causal_depth|count),read_state:(.read_state.state|state),
      processing_state:(.processing.state|state),generation:(.processing.generation|count),
      failure_count:(.processing.failure_count|count),created_at:(.created_at|stamp)};
    . as $data | {at:$at,leg:$leg,attempt:$attempt,event:$event,polls:$polls} +
    ($data | if .request then {request:{request_id:(.request.id|id),state:(.request.state|state),
      phase:(.request.phase|state),max_assignees:(.request.max_assignees|count),
      candidate_count:(.request.candidate_count|count),offer_count:(.request.offer_count|count),
      selection_generation:(.request.selection_generation|count),
      expires_at:(.request.expires_at|stamp),updated_at:(.request.updated_at|stamp)}} else {} end) +
    ($data | if .claim then {claims:[.claim|claim]} elif .claims then {claims:[.claims[]|claim]} else {} end) +
    ($data | if .message then {message:(.message|message)} elif .offer then {message:(.offer.message|message)}
      elif .opening_message then {message:(.opening_message|message)} else {} end) +
    ($data | if .processing then {processing:{state:(.processing.state|state),
      claim_id:(if .processing.claim_id then .processing.claim_id|id else null end),
      generation:(.processing.generation|count),failure_count:(.processing.failure_count|count),
      lease_expires_at:(.processing.lease_expires_at|stamp),
      result_message_id:(if .processing.result_message_id then .processing.result_message_id|id else null end)}} else {} end)
  ' <<<"$response" >>"$transitions" 2>/dev/null || die 'invalid value-free transition metadata'
}
pass_leg() { passed_legs="$passed_legs$leg"; }

finish() {
  local status=$? current_state recovered_request
  trap - EXIT INT TERM
  set +e
  if [[ "$opening_pending" == true && -z "$active_request" ]]; then
    # A lost open response is not proof that no request exists. Recover only
    # this run's exact idempotency key; never search/cancel by the shared marker.
    send_body=$(opening_body)
    if api coordinator message request open --body-stdin --selection-policy client_ranked \
      --max-assignees 1 --offer-window "${offer_seconds}s" --expires-in "${expiry_seconds}s" \
      --idempotency-key "$run_key-open-$attempt" &&
      recovered_request=$(owned_open_request_id <"$scratch/response.json"); then
      active_request=$recovered_request
      opening_pending=false
      if [[ "$attempt" == 1 ]]; then request_id=$active_request; else retry_request_id=$active_request; fi
    fi
    if [[ -z "$active_request" ]]; then
      cleanup_status=failed
      echo 'collaboration canary: open outcome indeterminate; exact-key recovery failed' >&2
    fi
  fi
  if [[ "$question_claim_pending" == true ]]; then
    send_body=
    if api coordinator message claim "$question_id" --lease "${lease_seconds}s" --idempotency-key "$run_key-question-claim"; then
      current_state=$(jq -r '.processing.state // empty' "$scratch/response.json" 2>/dev/null)
      if [[ "$current_state" == claimed ]]; then
        question_claim=$(jq -er '.processing.claim_id | select(test("^mcl_[A-Za-z0-9_-]+$"))' "$scratch/response.json" 2>/dev/null)
        question_generation=$(jq -er '.processing.generation | select(type == "number" and . > 0 and floor == .)' "$scratch/response.json" 2>/dev/null)
      elif [[ "$current_state" == completed ]]; then
        question_claim_pending=false
      fi
    fi
    if [[ "$question_claim_pending" == true && -z "$question_claim" ]]; then
      cleanup_status=failed
      echo 'collaboration canary: question claim outcome indeterminate; exact-key recovery failed' >&2
    fi
  fi
  if [[ -n "$question_claim" ]]; then
    send_body=
    # Completion may have committed before its response was lost. It is already
    # terminal in that case; do not misreport a rejected release as a live lease.
    if api coordinator message read "$question_id" &&
      jq -e --arg id "$question_id" '.message.id == $id and
        (.message.processing.state == "completed" or .message.processing.state == "available")' "$scratch/response.json" >/dev/null 2>&1; then
      question_claim=
    elif api coordinator message release "$question_id" --claim "$question_claim" --generation "$question_generation" &&
      jq -e '.processing.state == "available" and (.processing.lease_expires_at == null)' "$scratch/response.json" >/dev/null 2>&1; then
      question_claim=
    else
      cleanup_status=failed
      echo 'collaboration canary: could not release question claim' >&2
    fi
  fi
  if [[ "$success" != true && -n "$active_request" ]]; then
    send_body=
    current_state=
    if api coordinator message request show "$active_request"; then
      current_state=$(jq -r --arg id "$active_request" 'select(.request.id == $id) | .request.state // empty' "$scratch/response.json" 2>/dev/null)
    fi
    if [[ "$current_state" == completed || "$current_state" == cancelled || "$current_state" == expired ]]; then
      [[ "$cleanup_status" != failed ]] && cleanup_status=already_terminal
    elif api coordinator message request cancel "$active_request" &&
      jq -e --arg id "$active_request" '.request.id == $id and .request.state == "cancelled"' "$scratch/response.json" >/dev/null 2>&1; then
      [[ "$cleanup_status" != failed ]] && cleanup_status=cancelled
      jq -cn --arg at "$(date -u +%Y-%m-%dT%H:%M:%SZ)" --arg leg "$leg" \
        --arg id "$active_request" --argjson attempt "$attempt" \
        '{at:$at,leg:$leg,attempt:$attempt,event:"cancelled",request:{request_id:$id,state:"cancelled"}}' >>"$transitions"
    else
      cleanup_status=failed
      echo 'collaboration canary: open request cancellation could not be verified' >&2
    fi
  fi
  [[ "$cleanup_status" != failed ]] || status=1
  [[ "$success" == true ]] || status=1
  if [[ "$status" != 0 ]]; then
    jq -cn --arg at "$(date -u +%Y-%m-%dT%H:%M:%SZ)" --arg leg "$leg" \
      --argjson attempt "$attempt" '{at:$at,leg:$leg,attempt:$attempt,event:"failed"}' >>"$transitions"
  fi
  publish_tmp=$(mktemp "$out_parent/.witself-collaboration-record.XXXXXX")
  if jq -n --arg release "$release_version" --arg realm "$realm_id" \
    --arg coordinator "$coordinator_id" --arg worker "$worker_id" \
    --arg request "$request_id" --arg retry "$retry_request_id" \
    --arg started "$started_at" --arg finished "$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
    --arg passed "$passed_legs" --arg cleanup "$cleanup_status" --argjson status "$status" \
    --slurpfile transitions "$transitions" '
    {schema:"witself.collaboration-canary.v1",release_version:$release,realm_id:$realm,
      coordinator_agent_id:$coordinator,worker_agent_id:$worker,request_id:$request,
      retry_request_id:$retry,started_at:$started,finished_at:$finished,pass:($status == 0),
      legs:(["a","b","c","d","e","f","g","h","i"] | map(. as $leg | {leg:$leg,pass:($passed|contains($leg))})),
      transitions:$transitions,cleanup:$cleanup,redaction_checked:true}
  ' >"$publish_tmp" && redaction_scan "$publish_tmp"; then
    if ! ln "$publish_tmp" "$out" 2>/dev/null; then
      echo 'collaboration canary: evidence publication failed; refusing overwrite' >&2
      status=1
    fi
  else
    echo 'collaboration canary: redaction self-check or record encoding failed; evidence withheld' >&2
    status=1
  fi
  rm -f "$publish_tmp"
  rm -rf "$scratch"
  if [[ "$status" == 0 ]]; then echo 'collaboration canary: all nine legs passed; value-free record retained'; fi
  exit "$status"
}
trap finish EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

version_output=$(witself --version 2>/dev/null) || die 'could not read CLI release version'
if [[ "$version_output" =~ ^witself\ (v?[0-9]+\.[0-9]+\.[0-9]+([-+][A-Za-z0-9.-]+)?|dev)\  ]]; then
  release_version=${BASH_REMATCH[1]}
else
  die 'invalid CLI release version'
fi
for actor in coordinator worker; do
  poll 'bound agent identity' "$actor" '.identity.agent_id != null' self show --no-facts --no-salient
  check '.identity | (.agent_id|test("^agent_[A-Za-z0-9_-]+$")) and (.realm_id|test("^realm_[A-Za-z0-9_-]+$"))' 'invalid bound identity'
  expected_name=$coordinator_agent
  [[ "$actor" != worker ]] || expected_name=$worker_agent
  jq -e --arg name "$expected_name" --arg realm "$realm" \
    '.identity.agent_name == $name and .identity.realm_name == $realm' <<<"$response" >/dev/null || die 'agent or realm binding mismatch'
  if [[ "$actor" == coordinator ]]; then
    coordinator_id=$(field '.identity.agent_id')
    realm_id=$(field '.identity.realm_id')
    account_id=$(field '.identity.account_id')
  else
    worker_id=$(field '.identity.agent_id')
    [[ "$worker_id" != "$coordinator_id" && "$(field '.identity.realm_id')" == "$realm_id" &&
       "$(field '.identity.account_id')" == "$account_id" ]] || die 'bindings must be distinct agents in the same account and realm'
  fi
done

open_request() {
  local tentative_request
  opening_pending=true
  send_body=$(opening_body)
  poll 'open request' coordinator '.request.id != null' message request open --body-stdin \
    --selection-policy client_ranked --max-assignees 1 --offer-window "${offer_seconds}s" \
    --expires-in "${expiry_seconds}s" --idempotency-key "$run_key-open-$attempt"
  tentative_request=$(owned_open_request_id <<<"$response") || die 'open request ownership deviation'
  active_request=$tentative_request
  opening_pending=false
  if [[ "$attempt" == 1 ]]; then request_id=$active_request; else retry_request_id=$active_request; fi
  check '.request.state == "open" and .request.selection_policy == "client_ranked" and
    .request.max_assignees == 1' 'open request identity or policy deviation'
  opening_id=$(field '.opening_message.id')
  opening_thread=$(field '.opening_message.thread_id')
  opening_depth=$(field '.opening_message.causal_depth')
  record opened
  send_body=
}

discover_and_offer() {
  # List is metadata-only. Match the owned ID, then verify this run/attempt body through
  # show. Paginate rather than assuming this run is on the first mailbox page.
  local deadline=$((SECONDS + timeout_seconds)) cursor='' next page_count=0 found=false
  while [[ "$found" == false ]]; do
    poll 'worker discovery/offer' worker '.requests | type == "array"' message request list \
      --role candidate --state open --limit 100 --cursor "$cursor"
    if jq -e --arg id "$active_request" 'any(.requests[]; .id == $id)' <<<"$response" >/dev/null; then
      found=true
      break
    fi
    next=$(jq -r '.next_cursor // ""' <<<"$response")
    [[ -z "$next" || "$next" != "$cursor" ]] || die 'repeated request page cursor'
    cursor=$next
    page_count=$((page_count + 1))
    ((SECONDS < deadline && page_count < 1000)) || die 'timeout waiting for worker discovery/offer'
    sleep "$(jq -nr --argjson budget "$timeout_seconds" --argjson remaining "$((deadline - SECONDS))" '[$budget / 20, $remaining] | min')"
  done
  poll 'worker marker observation' worker '.opening_message.body != null' message request show "$active_request"
  jq -e --arg id "$active_request" --arg body "$(opening_body)" --arg coordinator "$coordinator_id" \
    '.request.id == $id and .request.coordinator.agent_id == $coordinator and
     .opening_message.body == $body' <<<"$response" >/dev/null || die 'discovered request marker or run identity mismatch'
  record discovered
  send_body="$marker: worker offers one bounded synthetic attempt."
  poll 'worker offer' worker '.offer.message.id != null' message request offer "$active_request" \
    --body-stdin --idempotency-key "$run_key-offer-$attempt"
  check ".offer.agent.agent_id == \"$worker_id\" and .request.id == \"$active_request\"" 'offer identity deviation'
  check ".offer.message.from.agent_id == \"$worker_id\" and .offer.message.to.agent_id == \"$coordinator_id\" and
    .offer.message.thread_id == \"$opening_thread\" and .offer.message.reply_to_message_id == \"$opening_id\" and
    .offer.message.causal_depth == $((opening_depth + 1)) and .offer.message.kind == \"offer\"" 'offer routing deviation'
  record offered
  send_body=
}

select_worker() {
  poll_with_budget "$selection_wait_seconds" 'offer and selection phase' coordinator \
    ".request.phase == \"awaiting_selection\" and any(.offers[]; .agent.agent_id == \"$worker_id\")" \
    message request show "$active_request"
  record awaiting_selection
  poll 'worker selection' coordinator '.selection.id != null' message request select "$active_request" \
    --selected-agent "$worker_id" --reservation "${lease_seconds}s" --idempotency-key "$run_key-select-$attempt"
  check ".request.id == \"$active_request\" and .request.state == \"open\" and
    .request.phase == \"assigned\" and .request.selected_agent_ids == [\"$worker_id\"] and
    .selection.selected_agent_ids == [\"$worker_id\"] and .selection.generation == .request.selection_generation and
    (.claims|length) == 1 and .claims[0].selection_id == .selection.id and .claims[0].request_id == \"$active_request\" and
    .claims[0].agent.agent_id == \"$worker_id\" and .claims[0].state == \"reserved\"" 'selection deviation'
  selection_id=$(field '.selection.id')
  reserved_claim_id=$(field '.claims[0].claim_id')
  reserved_generation=$(field '.claims[0].generation')
  record selected
}

claim_work() {
  poll 'worker attempt claim' worker '.claim.state == "claimed"' message request claim "$active_request" \
    --lease "${lease_seconds}s" --idempotency-key "$run_key-claim-$attempt"
  check ".claim.request_id == \"$active_request\" and .claim.agent.agent_id == \"$worker_id\" and
    .claim.claim_id == \"$reserved_claim_id\" and .claim.selection_id == \"$selection_id\" and
    .claim.failure_count == 0 and .claim.generation > $reserved_generation and
    (.claim.lease_expires_at | type == \"string\")" 'claim identity or failure accounting deviation'
  claim_id=$(field '.claim.claim_id')
  claim_generation=$(field '.claim.generation')
  claim_lease=$(field '.claim.lease_expires_at')
  record claimed
}

renew_work() {
  local label=$1
  send_body=
  api worker message request renew "$active_request" --claim "$claim_id" --generation "$claim_generation" --lease "${lease_seconds}s" || die "$label"
  response=$(cat "$scratch/response.json")
  poll_count=1
  jq -e --arg request "$active_request" --arg selection "$selection_id" --arg worker "$worker_id" \
    --arg claim "$claim_id" --argjson generation "$claim_generation" --arg previous "$claim_lease" '
    # Compare instants with fractions so a fast no-op renewal cannot pass.
    def instant:
      capture("^(?<whole>[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2})(?<fraction>\\.[0-9]+)?Z$")
      | ((.whole + "Z" | fromdateiso8601) + ((.fraction // "0") | tonumber));
    .claim.claim_id == $claim and .claim.generation == $generation and
    .claim.request_id == $request and .claim.selection_id == $selection and
    .claim.agent.agent_id == $worker and .claim.state == "claimed" and .claim.failure_count == 0 and
    ((.claim.lease_expires_at | instant) > ($previous | instant))
  ' <<<"$response" >/dev/null 2>&1 || die 'renewal fence or lease deviation'
  claim_lease=$(field '.claim.lease_expires_at')
  record renewed
}

complete_work() {
  local outcome=$1
  send_body="$marker: synthetic result $outcome."
  poll 'durable request result' worker '.claim.state == "completed" and .request.state == "completed"' \
    message request complete "$active_request" --claim "$claim_id" --generation "$claim_generation" \
    --body-stdin --idempotency-key "$run_key-complete-$attempt"
  jq -e --arg request "$active_request" --arg claim "$claim_id" --arg worker "$worker_id" \
    --arg coordinator "$coordinator_id" --arg opening "$opening_id" --arg thread "$opening_thread" --arg body "$send_body" \
    --argjson generation "$claim_generation" --argjson depth "$opening_depth" '
    .request.id == $request and .claim.claim_id == $claim and .claim.generation == $generation and
    .claim.failure_count == 0 and .claim.result_message_id == .message.id and
    .message.from.agent_id == $worker and .message.to.agent_id == $coordinator and
    .message.reply_to_message_id == $opening and .message.thread_id == $thread and .message.causal_depth == ($depth + 1) and
    .message.body == $body and .message.kind == "result"' <<<"$response" >/dev/null || die 'result identity, causality, or failure accounting deviation'
  result_id=$(field '.message.id')
  record "result_$outcome"
  send_body=
}

read_and_ack_result() {
  local outcome=$1
  poll 'coordinator result read' coordinator '.message.read_state.state == "read"' message read "$result_id"
  jq -e --arg id "$result_id" --arg body "$marker: synthetic result $outcome." \
    '.message.id == $id and .message.body == $body' <<<"$response" >/dev/null || die 'coordinator observed a different result'
  record "observed_$outcome"
  poll 'coordinator result acknowledgement' coordinator '.message.read_state.state == "acked"' message ack "$result_id"
  check ".message.id == \"$result_id\"" 'acknowledged a different result'
  record acknowledged
  poll 'closed request and failure accounting' coordinator \
    ".request.id == \"$active_request\" and .request.state == \"completed\" and
     any(.claims[]; .claim_id == \"$claim_id\" and .state == \"completed\" and .failure_count == 0 and .result_message_id == \"$result_id\")" \
    message request show "$active_request"
  record closed
  active_request=
}

attempt=1
open_request; pass_leg
leg=b; discover_and_offer; pass_leg
leg=c; select_worker; pass_leg
leg=d; claim_work
renew_work 'single claim renewal failed'
complete_work failure; pass_leg

# A failure body is client interpretation, not backend failure accounting.
# complete is terminal. Re-open, never re-select the completed first request.
leg=e
read_and_ack_result failure
attempt=2
open_request
[[ "$retry_request_id" != "$request_id" ]] || die 'retry did not create a second request'
discover_and_offer; select_worker; claim_work; pass_leg

leg=f
send_body="$marker: expanding this task to an external action would require new authority. Is that authorized?"
question_body=$send_body
poll 'escalation question' worker '.message.id != null' message reply "$opening_id" --kind question \
  --body-stdin --idempotency-key "$run_key-question"
check ".message.from.agent_id == \"$worker_id\" and .message.to.agent_id == \"$coordinator_id\" and
  .message.reply_to_message_id == \"$opening_id\" and .message.thread_id == \"$opening_thread\" and
  .message.causal_depth == $((opening_depth + 1)) and .message.kind == \"question\"" 'escalation routing deviation'
question_id=$(field '.message.id')
question_depth=$(field '.message.causal_depth')
record escalated; pass_leg
send_body=

leg=g
question_claim_pending=true
poll 'coordinator question claim' coordinator '.processing.state == "claimed"' message claim "$question_id" \
  --lease "${lease_seconds}s" --idempotency-key "$run_key-question-claim"
question_claim=$(field '.processing.claim_id')
question_generation=$(field '.processing.generation')
question_claim_pending=false
record question_claimed
poll 'coordinator question read' coordinator '.message.read_state.state == "read"' message read "$question_id"
jq -e --arg id "$question_id" --arg body "$question_body" '.message.id == $id and .message.kind == "question" and .message.body == $body' <<<"$response" >/dev/null || die 'question read identity or body deviation'
record question_read
send_body="$marker: original context permits only synthetic collaboration. No external action is authorized; finish the original canary."
poll 'escalation answer' coordinator '.processing.state == "completed" and .message.id != null' \
  message complete "$question_id" --claim "$question_claim" --generation "$question_generation" --kind answer \
  --body-stdin --idempotency-key "$run_key-answer"
check ".processing.claim_id == \"$question_claim\" and .processing.generation == $question_generation and
  .processing.result_message_id == .message.id and .message.from.agent_id == \"$coordinator_id\" and
  .message.to.agent_id == \"$worker_id\" and .message.reply_to_message_id == \"$question_id\" and
  .message.thread_id == \"$opening_thread\" and .message.causal_depth == $((question_depth + 1)) and .message.kind == \"answer\"" 'answer fence or routing deviation'
question_claim=
answer_id=$(field '.message.id')
answer_body=$send_body
record answered
send_body=
# Keep the retry claim alive for acknowledgements and final completion after
# the four-poll question/answer exchange, without changing its claim fence.
renew_work 'retry claim maintenance failed'
poll 'question acknowledgement' coordinator '.message.read_state.state == "acked"' message ack "$question_id"
check ".message.id == \"$question_id\"" 'question acknowledgement identity deviation'
record question_acknowledged
pass_leg
leg=h
poll 'worker answer read' worker '.message.read_state.state == "read"' message read "$answer_id"
jq -e --arg id "$answer_id" --arg body "$answer_body" '.message.id == $id and .message.body == $body' <<<"$response" >/dev/null || die 'worker did not receive the original-context answer'
record answer_read
poll 'worker answer acknowledgement' worker '.message.read_state.state == "acked"' message ack "$answer_id"
check ".message.id == \"$answer_id\"" 'answer acknowledgement identity deviation'
record answer_acknowledged

complete_work success; pass_leg
leg=i; read_and_ack_result success; pass_leg
success=true
