#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)"
runner="$repo_root/scripts/run-agent-email-cell-smoke.sh"
test_root="$(mktemp -d)"
chmod 700 "$test_root"
test_root="$(cd "$test_root" && pwd -P)"
trap 'find "$test_root" -depth -mindepth 1 -delete 2>/dev/null || true; rmdir "$test_root" 2>/dev/null || true' EXIT

fail() {
  printf 'test-agent-email-cell-smoke: %s\n' "$1" >&2
  exit 1
}

assert_private_output() {
  local file="$1"
  for forbidden in \
    'acc_abcdefghijklmnop' \
    'realm_bcdefghijklmnopq' \
    'agent_cdefghijklmnopqr' \
    'alpha.bcdefghijklmnopq@witmail.net' \
    'witself_agt_' \
    'relay-private-marker'; do
    if grep -F "$forbidden" "$file" >/dev/null 2>&1; then
      fail "operator output exposed private target material"
    fi
  done
}

private="$test_root/private"
fake_bin="$test_root/bin"
mkdir "$private" "$fake_bin"
chmod 700 "$private" "$fake_bin"

key_file="$private/relay-key"
wrong_key_file="$private/wrong-relay-key"
public_value_file="$private/public-value"
node - "$key_file" "$public_value_file" <<'NODE'
const { generateKeyPairSync } = require("node:crypto");
const { writeFileSync } = require("node:fs");
const { privateKey, publicKey } = generateKeyPairSync("ed25519");
const privateDER = privateKey.export({ format: "der", type: "pkcs8" });
const publicDER = publicKey.export({ format: "der", type: "spki" });
writeFileSync(process.argv[2], `${privateDER.toString("base64")}\n`, { mode: 0o600 });
writeFileSync(process.argv[3], `${publicDER.subarray(publicDER.length - 32).toString("base64")}\n`, { mode: 0o600 });
NODE
node - "$wrong_key_file" <<'NODE'
const { generateKeyPairSync } = require("node:crypto");
const { writeFileSync } = require("node:fs");
const { privateKey } = generateKeyPairSync("ed25519");
writeFileSync(process.argv[2], `${privateKey.export({ format: "der", type: "pkcs8" }).toString("base64")}\n`, { mode: 0o600 });
NODE
chmod 600 "$key_file" "$wrong_key_file" "$public_value_file"
public_value="$(tr -d '\n' <"$public_value_file")"

kubeconfig="$private/kubeconfig"
target="$private/target.json"
state="$private/state.json"
agent_token_file="$private/agent-token"
other_agent_token_file="$private/other-agent-token"
printf '%s\n' 'apiVersion: v1' >"$kubeconfig"
chmod 600 "$kubeconfig"
printf 'witself_agt_%s\n' "$(printf 'A%.0s' {1..43})" >"$agent_token_file"
printf 'witself_agt_%s\n' "$(printf 'B%.0s' {1..43})" >"$other_agent_token_file"
chmod 600 "$agent_token_file" "$other_agent_token_file"
jq -n '{schema_version:1,account_id:"acc_abcdefghijklmnop",
  realm_id:"realm_bcdefghijklmnopq",agent_id:"agent_cdefghijklmnopqr",
  recipient:"alpha.bcdefghijklmnopq@witmail.net",
  disabled_plan:"free",entitled_plan:"standard"}' >"$target"
chmod 600 "$target"

checksum="$(printf 'c%.0s' {1..64})"
hash_free="$(printf 'a%.0s' {1..64})"
hash_standard="$(printf 'b%.0s' {1..64})"
deployment_json="$private/deployment.json"
config_json="$private/config.json"
service_json="$private/service.json"
secret_json="$private/secret.json"
pods_json="$private/pods.json"

jq -n --arg checksum "$checksum" '{
  metadata:{name:"witself-server",uid:"deployment-uid",resourceVersion:"41",generation:7},
  spec:{replicas:2,selector:{matchLabels:{"app.kubernetes.io/name":"witself-server"}},
    template:{metadata:{labels:{"app.kubernetes.io/name":"witself-server"},
      annotations:{"witself.io/server-config-checksum":$checksum}},
    spec:{containers:[{name:"witself-server",image:"ghcr.io/witwave-ai/images/witself-server:0.0.245",
      envFrom:[{configMapRef:{name:"witself-server-config"}}],
      env:[{name:"WITSELF_AGENT_EMAIL_RECEIVE_ACCOUNT_IDS",valueFrom:{secretKeyRef:{name:"receive-cohort-v1",key:"account_ids",optional:false}}}]}]} }},
  status:{observedGeneration:7,replicas:2,readyReplicas:2,updatedReplicas:2,availableReplicas:2,unavailableReplicas:0}
}' >"$deployment_json"
jq -n --arg checksum "$checksum" --arg public "$public_value" '{
  metadata:{name:"witself-server-config",uid:"config-uid",resourceVersion:"52",
    annotations:{"witself.io/server-config-checksum":$checksum}},
  data:{WITSELF_BACKEND_KIND:"managed",WITSELF_CELL_NAME:"civo-sandbox-usw2-dev",
    WITSELF_AGENT_EMAIL_RECEIVE_PRODUCTION_ENABLED:"true",
    WITSELF_AGENT_EMAIL_RECEIVE_PILOT_ENABLED:"false",
    WITSELF_AGENT_EMAIL_RECEIVE_DOMAIN:"witmail.net",
    WITSELF_AGENT_EMAIL_RECEIVE_AUDIENCE:"civo-sandbox-usw2-dev",
    WITSELF_AGENT_EMAIL_RELAY_PUBLIC_KEYS_JSON:({"relay-1":$public}|tojson)}
}' >"$config_json"
jq -n '{metadata:{name:"witself-server",uid:"service-uid",resourceVersion:"73"},
  spec:{clusterIP:"10.0.0.10",selector:{"app.kubernetes.io/name":"witself-server"},
  ports:[{name:"api",port:80,targetPort:"api",protocol:"TCP"}]}}' >"$service_json"
cohort_base64="$(printf '%s' 'acc_abcdefghijklmnop' | base64 | tr -d '\n')"
jq -n --arg cohort "$cohort_base64" '{metadata:{name:"receive-cohort-v1",uid:"cohort-uid",resourceVersion:"63"},
  immutable:true,data:{account_ids:$cohort}}' >"$secret_json"
jq -n '{items:[{metadata:{name:"witself-postgresql-0"},spec:{containers:[{name:"postgresql"}]},
  status:{phase:"Running",conditions:[{type:"Ready",status:"True"}]}}]}' >"$pods_json"
chmod 600 "$deployment_json" "$config_json" "$service_json" "$secret_json" "$pods_json"

fake_server="$private/fake-server.mjs"
cat >"$fake_server" <<'NODE'
import { appendFileSync, writeFileSync } from "node:fs";
import { createServer } from "node:http";

const server = createServer((request, response) => {
  if (request.url === "/v1/version") {
    response.writeHead(200, { "Content-Type": "application/json" });
    response.end('{"schema_version":"witself.v0","version":"0.0.245","commit":"test","date":"test","account_evacuation_protocol":1,"account_provision_protocol":1}\n');
    return;
  }
  if (request.url === "/v1/email/address" && request.method === "GET") {
    appendFileSync(process.env.FAKE_OWNER_REQUEST_COUNT, "1\n");
    if (request.headers.authorization !== process.env.FAKE_AGENT_AUTHORIZATION) {
      response.writeHead(401, { "Content-Type": "application/json", "Cache-Control": "private, no-store" });
      response.end('{"schema_version":"witself.v0","error":"unauthorized"}\n');
      return;
    }
    if (process.env.FAKE_OWNER_GATE === "feature_disabled") {
      response.writeHead(403, { "Content-Type": "application/json", "Cache-Control": "private, no-store" });
      response.end('{"schema_version":"witself.v0","code":"feature_not_enabled","feature":"agent_email_receive","error":"Sorry, this feature is not enabled on this account.","retryable":false}\n');
      return;
    }
    response.writeHead(200, { "Content-Type": "application/json", "Cache-Control": "private, no-store" });
    response.end(`${JSON.stringify({schema_version:"witself.v0",address:{
      account_id:"acc_abcdefghijklmnop",realm_id:"realm_bcdefghijklmnopq",
      owner_agent_id:"agent_cdefghijklmnopqr",address:"alpha.bcdefghijklmnopq@witmail.net",
      domain:"witmail.net",local_part:"alpha.bcdefghijklmnopq",realm_label:"bcdefghijklmnopq",
      receive_state:"enabled",agent_receive_state:"enabled",realm_receive_state:"enabled",
      addresses:[{address:"alpha.bcdefghijklmnopq@witmail.net",domain:"witmail.net",role:"primary"}],aliases:[]
    }})}\n`);
    return;
  }
  if (request.url !== "/v1/internal/agent-email:ingest" || request.method !== "POST") {
    response.writeHead(404);
    response.end();
    return;
  }
  const chunks = [];
  request.on("data", (chunk) => chunks.push(chunk));
  request.on("end", () => {
    appendFileSync(process.env.FAKE_REQUEST_COUNT, "1\n");
    writeFileSync(process.env.FAKE_ACCEPT_FLAG, "accepted\n", { mode: 0o600 });
    response.writeHead(200, { "Content-Type": "application/json", "Cache-Control": "no-store" });
    response.end(`${JSON.stringify({ verdict: process.env.FAKE_RELAY_VERDICT })}\n`);
  });
});
server.listen(0, "127.0.0.1", () => {
  process.stdout.write(`Forwarding from 127.0.0.1:${server.address().port} -> 8080\n`);
});
NODE
chmod 600 "$fake_server"

fake_kube_proxy="$private/fake-kube-proxy.mjs"
cat >"$fake_kube_proxy" <<'NODE'
import { writeFileSync } from "node:fs";
import { createServer } from "node:http";

const expectedPath = "/api/v1/namespaces/witself/configmaps/witself-agent-email-operation-lock";
const server = createServer((request, response) => {
  const chunks = [];
  request.on("data", (chunk) => chunks.push(chunk));
  request.on("end", () => {
    let body;
    try { body = JSON.parse(Buffer.concat(chunks).toString("utf8")); } catch { body = null; }
    const requestedUID = body?.preconditions?.uid;
    const currentUID = process.env.FAKE_LOCK_REPLACED === "1" ? "replacement-lock-uid" : "smoke-lock-uid";
    if (request.method !== "DELETE" || request.url !== expectedPath ||
        body?.apiVersion !== "v1" || body?.kind !== "DeleteOptions" ||
        typeof requestedUID !== "string") {
      response.writeHead(400, { "Content-Type": "application/json" });
      response.end('{"kind":"Status","status":"Failure","reason":"BadRequest"}\n');
      return;
    }
    if (requestedUID !== currentUID) {
      writeFileSync(process.env.FAKE_REPLACEMENT_LOCK_SURVIVED, "replacement-survived\n", { mode: 0o600 });
      response.writeHead(409, { "Content-Type": "application/json" });
      response.end('{"kind":"Status","status":"Failure","reason":"Conflict"}\n');
      return;
    }
    writeFileSync(process.env.FAKE_LOCK_DELETED, "deleted\n", { mode: 0o600 });
    response.writeHead(200, { "Content-Type": "application/json" });
    response.end(`${JSON.stringify({kind:"Status",apiVersion:"v1",status:"Success",
      details:{name:"witself-agent-email-operation-lock",kind:"configmaps",uid:currentUID}})}\n`);
  });
});
server.listen(0, "127.0.0.1", () => {
  process.stdout.write(`Starting to serve on 127.0.0.1:${server.address().port}\n`);
});
NODE
chmod 600 "$fake_kube_proxy"

fake_kubectl="$fake_bin/kubectl"
cat >"$fake_kubectl" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
printf '%s\n' "$*" >>"$FAKE_COMMAND_LOG"
joined=" $* "
if [[ "$joined" == *" get applications.argoproj.io witself-postgresql "* ]]; then
  printf '%s' 'civo-sandbox-usw2-dev'
elif [[ "$joined" == *" create -f - -o json "* ]]; then
  input="$(cat)"
  grep -F 'witself-agent-email-operation-lock' <<<"$input" >/dev/null
  printf '%s\n' '{"metadata":{"uid":"smoke-lock-uid"}}'
elif [[ "$joined" == *" get configmap witself-agent-email-operation-lock "* ]]; then
  printf '%s' 'smoke-lock-uid'
elif [[ "$joined" == *" delete configmap witself-agent-email-operation-lock "* ]]; then
  printf '%s\n' 'legacy name-only operation-lock deletion is forbidden' >&2
  exit 1
elif [[ "$joined" == *" proxy --address=127.0.0.1 --port=0 "* ]]; then
  exec node "$FAKE_KUBE_PROXY"
elif [[ "$joined" == *" get deployment witself-server -o json "* ]]; then
  cat "$FAKE_DEPLOYMENT_JSON"
elif [[ "$joined" == *" get configmap witself-server-config -o json "* ]]; then
  cat "$FAKE_CONFIG_JSON"
elif [[ "$joined" == *" get service witself-server -o json "* ]]; then
  service_get_count=1
  if [ -f "$FAKE_SERVICE_GET_COUNT" ]; then
    service_get_count=$(( $(<"$FAKE_SERVICE_GET_COUNT") + 1 ))
  fi
  printf '%s\n' "$service_get_count" >"$FAKE_SERVICE_GET_COUNT"
  if [ "${FAKE_SERVICE_DRIFT_AFTER:-0}" -gt 0 ] &&
     [ "$service_get_count" -ge "$FAKE_SERVICE_DRIFT_AFTER" ]; then
    cat "$FAKE_SERVICE_DRIFT_JSON"
  else
    cat "$FAKE_SERVICE_JSON"
  fi
elif [[ "$joined" == *" get secret receive-cohort-v1 -o json "* ]]; then
  cat "$FAKE_SECRET_JSON"
elif [[ "$joined" == *" get pods "* ]]; then
  cat "$FAKE_PODS_JSON"
elif [[ "$joined" == *" port-forward "* ]]; then
  exec node "$FAKE_SERVER"
elif [[ "$joined" == *" exec -i witself-postgresql-0 "* ]]; then
  sql="$(cat)"
  if [ "$FAKE_DB_PHASE" != cleanup ] &&
     grep -E '(^|[^A-Za-z])(INSERT|UPDATE|DELETE|CREATE|ALTER|DROP|TRUNCATE)([^A-Za-z]|$)' <<<"$sql" >/dev/null; then
    printf '%s\n' 'signed phases must be database-read-only' >&2
    exit 1
  fi
  if grep -F 'WITH target AS' <<<"$sql" >/dev/null; then
    epoch="$(date +%s)"
    case "$FAKE_DB_PHASE" in
      disabled)
        jq -cn --arg hash "$FAKE_HASH_FREE" --argjson epoch "$epoch" '{target_count:1,account_status:"active",plan:"free",
          entitlement_version:1,feature_enabled:false,receive_state:"enabled",plan_applied:true,
          plan_applied_epoch:100,plan_revision:1,plan_hash:$hash,database_epoch:$epoch}'
        ;;
      entitled|cleanup)
        revision="${FAKE_ENTITLED_REVISION:-2}"
        jq -cn --arg hash "$FAKE_HASH_STANDARD" --argjson epoch "$epoch" --argjson revision "$revision" \
          '{target_count:1,account_status:"active",plan:"standard",entitlement_version:1,feature_enabled:true,
            receive_state:"enabled",plan_applied:true,plan_applied_epoch:200,plan_revision:$revision,
            plan_hash:$hash,database_epoch:$epoch}'
        ;;
    esac
  elif grep -F 'FROM tokens t' <<<"$sql" >/dev/null; then
    printf '%s\n' '{"token_count":1}'
  elif grep -F 'CREATE TEMP TABLE smoke_expected' <<<"$sql" >/dev/null; then
    grep -F 'PERFORM 1 FROM accounts' <<<"$sql" >/dev/null
    ! grep -F 'SELECT 1 FROM accounts WHERE' <<<"$sql" >/dev/null
    grep -F 'CREATE TEMP TABLE smoke_suspects(probe_tag text,id text,PRIMARY KEY(probe_tag,id))' <<<"$sql" >/dev/null
    [ "$(grep -Fc 'm.subaddress_tag=x.tag OR m.envelope_recipient=x.recipient OR m.mime_message_id=x.mime_id OR m.raw_sha256=x.raw_sha' <<<"$sql")" = 2 ]
    grep -F 'IF suspect_count<>matched_count THEN' <<<"$sql" >/dev/null
    grep -F 'CREATE TEMP TABLE smoke_candidates(probe_tag text UNIQUE,id text PRIMARY KEY)' <<<"$sql" >/dev/null
    grep -F "encode(sha256(m.raw_mime),'hex')=x.raw_sha" <<<"$sql" >/dev/null
    grep -F 'm.duplicate_group_sha256=encode(sha256(' <<<"$sql" >/dev/null
    grep -F "x.expected_message_id<>'' AND m.id=x.expected_message_id" <<<"$sql" >/dev/null
    grep -F 'emsg_abcdefghijklmnop' <<<"$sql" >/dev/null
    if [ "${FAKE_CLEANUP_UNSAFE:-0}" = 1 ]; then exit 1; fi
    if [ "${FAKE_CLEANUP_CORRUPT_OUTPUT:-0}" = 1 ]; then printf '%s\n' '1'; fi
    printf '%s\n' '{"matched":1,"deleted":1,"remaining":0,"events_retained":1}'
  else
    if [ "$FAKE_DB_PHASE" = entitled ] && [ -f "$FAKE_ACCEPT_FLAG" ]; then
      printf '%s\n' '{"messages":1,"deliveries":1,"events":1,"owner_events":10,"message_id":"emsg_abcdefghijklmnop"}'
    else
      printf '%s\n' '{"messages":0,"deliveries":0,"events":0,"owner_events":9,"message_id":""}'
    fi
  fi
else
  printf '%s\n' 'unexpected fake kubectl invocation' >&2
  exit 1
fi
SH
chmod 700 "$fake_kubectl"

export PATH="$fake_bin:$PATH"
export FAKE_DEPLOYMENT_JSON="$deployment_json"
export FAKE_CONFIG_JSON="$config_json"
export FAKE_SERVICE_JSON="$service_json"
export FAKE_SECRET_JSON="$secret_json"
export FAKE_PODS_JSON="$pods_json"
export FAKE_SERVER="$fake_server"
export FAKE_KUBE_PROXY="$fake_kube_proxy"
export FAKE_HASH_FREE="$hash_free"
export FAKE_HASH_STANDARD="$hash_standard"
export FAKE_ACCEPT_FLAG="$private/accepted.flag"
export FAKE_REQUEST_COUNT="$private/request.count"
export FAKE_OWNER_REQUEST_COUNT="$private/owner-request.count"
export FAKE_COMMAND_LOG="$private/commands.log"
export FAKE_SERVICE_GET_COUNT="$private/service-get.count"
export FAKE_REPLACEMENT_LOCK_SURVIVED="$private/replacement-lock-survived"
export FAKE_LOCK_DELETED="$private/lock-deleted"
FAKE_AGENT_AUTHORIZATION="Bearer $(tr -d '\n' <"$agent_token_file")"
export FAKE_AGENT_AUTHORIZATION
: >"$FAKE_COMMAND_LOG"
chmod 600 "$FAKE_COMMAND_LOG"

common=(--cell civo-sandbox-usw2-dev --kubeconfig "$kubeconfig" --context fake-civo \
  --target-file "$target" --namespace witself --deployment witself-server --service witself-server)
signed=(--agent-token-file "$agent_token_file" --relay-key-id relay-1 --relay-private-key-file "$key_file")

disabled_out="$private/disabled.out"
disabled_err="$private/disabled.err"
export FAKE_DB_PHASE=disabled FAKE_RELAY_VERDICT=feature_disabled FAKE_OWNER_GATE=feature_disabled
rm -f "$FAKE_ACCEPT_FLAG" "$FAKE_REQUEST_COUNT" "$FAKE_OWNER_REQUEST_COUNT"
"$runner" "${common[@]}" --phase disabled --state-file "$state" \
  "${signed[@]}" >"$disabled_out" 2>"$disabled_err" ||
  { sed -n '1,20p' "$disabled_err" >&2; fail "disabled phase failed"; }
jq -e '.phase=="disabled" and .verdict=="feature_disabled" and .owner_gate=="feature_disabled" and
  .same_client_credential_fenced==true and .messages_after==0 and
  .owner_receive_event_delta==0 and
  .provider_mutation_performed==false' "$disabled_out" >/dev/null || fail "disabled result is invalid"
jq -e '.disabled.outcome=="verified" and .disabled.owner_gate=="feature_disabled" and
  .disabled.plan.plan=="free" and (.client_fence.token_sha256|test("^[0-9a-f]{64}$")) and
  .entitled==null' "$state" >/dev/null ||
  fail "disabled state fence is invalid"
[ "$(wc -l <"$FAKE_REQUEST_COUNT" | tr -d '[:space:]')" = 1 ] || fail "disabled phase did not issue exactly one POST"
[ "$(wc -l <"$FAKE_OWNER_REQUEST_COUNT" | tr -d '[:space:]')" = 1 ] ||
  fail "disabled phase did not prove the installed owner gate exactly once"
assert_private_output "$disabled_out"
assert_private_output "$disabled_err"
disabled_state="$private/disabled-state.json"
cp "$state" "$disabled_state"
chmod 600 "$disabled_state"

service_selector_state="$private/service-selector-state.json"
service_selector_json="$private/service-selector.json"
cp "$disabled_state" "$service_selector_state"
jq '.spec.selector={"app.kubernetes.io/name":"replacement"}' "$service_json" >"$service_selector_json"
chmod 600 "$service_selector_state" "$service_selector_json"
service_selector_out="$private/service-selector.out"
service_selector_err="$private/service-selector.err"
export FAKE_DB_PHASE=entitled FAKE_RELAY_VERDICT=accepted FAKE_ENTITLED_REVISION=2
export FAKE_OWNER_GATE=address_available FAKE_SERVICE_JSON="$service_selector_json"
export FAKE_LOCK_REPLACED=1
rm -f "$FAKE_ACCEPT_FLAG" "$FAKE_REQUEST_COUNT" "$FAKE_OWNER_REQUEST_COUNT" \
  "$FAKE_REPLACEMENT_LOCK_SURVIVED" "$FAKE_LOCK_DELETED"
if "$runner" "${common[@]}" --phase entitled --state-file "$service_selector_state" \
    "${signed[@]}" >"$service_selector_out" 2>"$service_selector_err"; then
  fail "Service selector not tied to the Deployment was accepted"
fi
[ ! -e "$FAKE_REQUEST_COUNT" ] || fail "invalid Service selector reached the ingest endpoint"
jq -e '.entitled==null' "$service_selector_state" >/dev/null || fail "Service selector failure changed state"
assert_private_output "$service_selector_out"
assert_private_output "$service_selector_err"
[ -f "$FAKE_REPLACEMENT_LOCK_SURVIVED" ] || fail "replacement operation lock did not survive UID-preconditioned cleanup"
[ ! -e "$FAKE_LOCK_DELETED" ] || fail "replacement operation lock was deleted"
export FAKE_LOCK_REPLACED=0

service_ports_state="$private/service-ports-state.json"
service_ports_json="$private/service-ports.json"
cp "$disabled_state" "$service_ports_state"
jq '.spec.ports += [{name:"metrics",port:9090,targetPort:"metrics",protocol:"TCP"}]' \
  "$service_json" >"$service_ports_json"
chmod 600 "$service_ports_state" "$service_ports_json"
service_ports_out="$private/service-ports.out"
service_ports_err="$private/service-ports.err"
export FAKE_SERVICE_JSON="$service_ports_json"
if "$runner" "${common[@]}" --phase entitled --state-file "$service_ports_state" \
    "${signed[@]}" >"$service_ports_out" 2>"$service_ports_err"; then
  fail "Service with an additional port was accepted"
fi
[ ! -e "$FAKE_REQUEST_COUNT" ] || fail "invalid Service ports reached the ingest endpoint"
jq -e '.entitled==null' "$service_ports_state" >/dev/null || fail "Service ports failure changed state"
assert_private_output "$service_ports_out"
assert_private_output "$service_ports_err"

service_drift_state="$private/service-drift-state.json"
service_drift_json="$private/service-drift.json"
cp "$disabled_state" "$service_drift_state"
jq '.metadata.resourceVersion="74"' "$service_json" >"$service_drift_json"
chmod 600 "$service_drift_state" "$service_drift_json"
service_drift_out="$private/service-drift.out"
service_drift_err="$private/service-drift.err"
export FAKE_SERVICE_JSON="$service_drift_json"
if "$runner" "${common[@]}" --phase entitled --state-file "$service_drift_state" \
    "${signed[@]}" >"$service_drift_out" 2>"$service_drift_err"; then
  fail "changed Service identity was accepted between plan phases"
fi
[ ! -e "$FAKE_REQUEST_COUNT" ] || fail "changed Service identity reached the ingest endpoint"
jq -e '.entitled==null' "$service_drift_state" >/dev/null || fail "Service drift changed state"
assert_private_output "$service_drift_out"
assert_private_output "$service_drift_err"
export FAKE_SERVICE_JSON="$service_json"

service_midrun_state="$private/service-midrun-state.json"
cp "$disabled_state" "$service_midrun_state"
chmod 600 "$service_midrun_state"
service_midrun_out="$private/service-midrun.out"
service_midrun_err="$private/service-midrun.err"
export FAKE_SERVICE_DRIFT_AFTER=2 FAKE_SERVICE_DRIFT_JSON="$service_drift_json"
rm -f "$FAKE_SERVICE_GET_COUNT" "$FAKE_ACCEPT_FLAG" "$FAKE_REQUEST_COUNT" "$FAKE_OWNER_REQUEST_COUNT"
if "$runner" "${common[@]}" --phase entitled --state-file "$service_midrun_state" \
    "${signed[@]}" >"$service_midrun_out" 2>"$service_midrun_err"; then
  fail "mid-run Service resourceVersion drift was accepted"
fi
[ ! -e "$FAKE_REQUEST_COUNT" ] || fail "mid-run Service drift reached the ingest endpoint"
jq -e '.entitled.outcome=="prepared"' "$service_midrun_state" >/dev/null ||
  fail "mid-run Service drift lost its prepared recovery state"
assert_private_output "$service_midrun_out"
assert_private_output "$service_midrun_err"

service_postsend_state="$private/service-postsend-state.json"
cp "$disabled_state" "$service_postsend_state"
chmod 600 "$service_postsend_state"
service_postsend_out="$private/service-postsend.out"
service_postsend_err="$private/service-postsend.err"
export FAKE_SERVICE_DRIFT_AFTER=3
rm -f "$FAKE_SERVICE_GET_COUNT" "$FAKE_ACCEPT_FLAG" "$FAKE_REQUEST_COUNT" "$FAKE_OWNER_REQUEST_COUNT"
if "$runner" "${common[@]}" --phase entitled --state-file "$service_postsend_state" \
    "${signed[@]}" >"$service_postsend_out" 2>"$service_postsend_err"; then
  fail "post-send Service resourceVersion drift was accepted"
fi
[ "$(wc -l <"$FAKE_REQUEST_COUNT" | tr -d '[:space:]')" = 1 ] ||
  fail "post-send Service drift did not preserve exactly one ingest request"
[ "$(wc -l <"$FAKE_OWNER_REQUEST_COUNT" | tr -d '[:space:]')" = 1 ] ||
  fail "post-send Service drift did not preserve exactly one owner request"
jq -e '.entitled.outcome=="prepared"' "$service_postsend_state" >/dev/null ||
  fail "post-send Service drift lost its prepared recovery state"
assert_private_output "$service_postsend_out"
assert_private_output "$service_postsend_err"
export FAKE_SERVICE_DRIFT_AFTER=0
unset FAKE_SERVICE_DRIFT_JSON
rm -f "$FAKE_SERVICE_GET_COUNT"

wrong_verdict_state="$private/wrong-verdict-state.json"
wrong_verdict_out="$private/wrong-verdict.out"
wrong_verdict_err="$private/wrong-verdict.err"
export FAKE_DB_PHASE=disabled FAKE_RELAY_VERDICT=accepted FAKE_OWNER_GATE=feature_disabled
rm -f "$FAKE_ACCEPT_FLAG" "$FAKE_REQUEST_COUNT" "$FAKE_OWNER_REQUEST_COUNT"
if "$runner" "${common[@]}" --phase disabled --state-file "$wrong_verdict_state" \
    "${signed[@]}" \
    >"$wrong_verdict_out" 2>"$wrong_verdict_err"; then
  fail "accepted verdict was allowed for the Personal proof"
fi
[ "$(wc -l <"$FAKE_REQUEST_COUNT" | tr -d '[:space:]')" = 1 ] ||
  fail "unexpected verdict caused zero or repeated signed POSTs"
jq -e '.disabled.outcome=="prepared" and .entitled==null' "$wrong_verdict_state" >/dev/null ||
  fail "unexpected verdict lost its crash-recovery fence"
assert_private_output "$wrong_verdict_out"
assert_private_output "$wrong_verdict_err"

stale_state="$private/stale-state.json"
cp "$disabled_state" "$stale_state"
chmod 600 "$stale_state"
stale_out="$private/stale.out"
stale_err="$private/stale.err"
export FAKE_DB_PHASE=entitled FAKE_RELAY_VERDICT=accepted FAKE_ENTITLED_REVISION=1 FAKE_OWNER_GATE=address_available
rm -f "$FAKE_ACCEPT_FLAG" "$FAKE_REQUEST_COUNT" "$FAKE_OWNER_REQUEST_COUNT"
if "$runner" "${common[@]}" --phase entitled --state-file "$stale_state" \
    "${signed[@]}" >"$stale_out" 2>"$stale_err"; then
  fail "stale plan revision was accepted"
fi
[ ! -e "$FAKE_REQUEST_COUNT" ] || fail "stale plan revision reached the ingest endpoint"
assert_private_output "$stale_out"
assert_private_output "$stale_err"

swapped_token_state="$private/swapped-token-state.json"
cp "$disabled_state" "$swapped_token_state"
chmod 600 "$swapped_token_state"
swapped_token_out="$private/swapped-token.out"
swapped_token_err="$private/swapped-token.err"
export FAKE_DB_PHASE=entitled FAKE_RELAY_VERDICT=accepted FAKE_ENTITLED_REVISION=2 FAKE_OWNER_GATE=address_available
rm -f "$FAKE_ACCEPT_FLAG" "$FAKE_REQUEST_COUNT" "$FAKE_OWNER_REQUEST_COUNT"
if "$runner" "${common[@]}" --phase entitled --state-file "$swapped_token_state" \
    --agent-token-file "$other_agent_token_file" --relay-key-id relay-1 \
    --relay-private-key-file "$key_file" >"$swapped_token_out" 2>"$swapped_token_err"; then
  fail "a swapped client credential was accepted as a no-reinstall proof"
fi
[ ! -e "$FAKE_REQUEST_COUNT" ] || fail "swapped client credential reached the ingest endpoint"
[ ! -e "$FAKE_OWNER_REQUEST_COUNT" ] || fail "swapped client credential reached the owner endpoint"
jq -e '.entitled==null' "$swapped_token_state" >/dev/null ||
  fail "swapped client credential changed the durable state"
assert_private_output "$swapped_token_out"
assert_private_output "$swapped_token_err"

mismatch_state="$private/mismatch-state.json"
cp "$disabled_state" "$mismatch_state"
chmod 600 "$mismatch_state"
mismatch_out="$private/mismatch.out"
mismatch_err="$private/mismatch.err"
export FAKE_DB_PHASE=entitled FAKE_RELAY_VERDICT=accepted FAKE_ENTITLED_REVISION=2
export FAKE_OWNER_GATE=address_available
rm -f "$FAKE_ACCEPT_FLAG" "$FAKE_REQUEST_COUNT" "$FAKE_OWNER_REQUEST_COUNT"
if "$runner" "${common[@]}" --phase entitled --state-file "$mismatch_state" \
    --agent-token-file "$agent_token_file" --relay-key-id relay-1 \
    --relay-private-key-file "$wrong_key_file" >"$mismatch_out" 2>"$mismatch_err"; then
  fail "mismatched relay private key was accepted"
fi
[ ! -e "$FAKE_REQUEST_COUNT" ] || fail "key mismatch reached the ingest endpoint"
assert_private_output "$mismatch_out"
assert_private_output "$mismatch_err"

entitled_out="$private/entitled.out"
entitled_err="$private/entitled.err"
rm -f "$FAKE_ACCEPT_FLAG" "$FAKE_REQUEST_COUNT" "$FAKE_OWNER_REQUEST_COUNT"
"$runner" "${common[@]}" --phase entitled --state-file "$state" \
  "${signed[@]}" >"$entitled_out" 2>"$entitled_err" ||
  fail "entitled phase failed"
jq -e '.phase=="entitled" and .verdict=="accepted" and .owner_gate=="address_available" and
  .same_client_credential==true and .messages_after==1 and
  .owner_receive_event_delta==1 and
  .plan_flip_verified_without_reinstall==true and .cleanup_required==true and
  .provider_mutation_performed==false' "$entitled_out" >/dev/null || fail "entitled result is invalid"
jq -e '.disabled.outcome=="verified" and .entitled.outcome=="verified" and
  .entitled.owner_gate=="address_available" and
  .entitled.plan.plan=="standard" and .entitled.evidence.message_id=="emsg_abcdefghijklmnop"' \
  "$state" >/dev/null || fail "entitled state fence is invalid"
[ "$(wc -l <"$FAKE_REQUEST_COUNT" | tr -d '[:space:]')" = 1 ] || fail "entitled phase did not issue exactly one POST"
[ "$(wc -l <"$FAKE_OWNER_REQUEST_COUNT" | tr -d '[:space:]')" = 1 ] ||
  fail "entitled phase did not prove the installed owner gate exactly once"
assert_private_output "$entitled_out"
assert_private_output "$entitled_err"

raw_mismatch_state="$private/raw-mismatch-state.json"
cp "$state" "$raw_mismatch_state"
chmod 600 "$raw_mismatch_state"
jq '.entitled.probe.raw_sha256=("0"*64)' "$raw_mismatch_state" >"$private/raw-mismatch-updated.json"
mv "$private/raw-mismatch-updated.json" "$raw_mismatch_state"
chmod 600 "$raw_mismatch_state"
raw_mismatch_out="$private/raw-mismatch.out"
raw_mismatch_err="$private/raw-mismatch.err"
export FAKE_DB_PHASE=cleanup FAKE_CLEANUP_UNSAFE=0 FAKE_CLEANUP_CORRUPT_OUTPUT=0
requests_before_cleanup="$(wc -l <"$FAKE_REQUEST_COUNT" | tr -d '[:space:]')"
if "$runner" "${common[@]}" --phase cleanup --state-file "$raw_mismatch_state" \
    >"$raw_mismatch_out" 2>"$raw_mismatch_err"; then
  fail "cleanup trusted a state digest that did not match reconstructed raw MIME"
fi
jq -e '.cleanup==null' "$raw_mismatch_state" >/dev/null || fail "raw MIME mismatch changed state"
assert_private_output "$raw_mismatch_out"
assert_private_output "$raw_mismatch_err"
[ "$(wc -l <"$FAKE_REQUEST_COUNT" | tr -d '[:space:]')" = "$requests_before_cleanup" ] ||
  fail "raw MIME mismatch reached the ingest endpoint"

corrupt_cleanup_state="$private/corrupt-cleanup-state.json"
cp "$state" "$corrupt_cleanup_state"
chmod 600 "$corrupt_cleanup_state"
corrupt_cleanup_out="$private/corrupt-cleanup.out"
corrupt_cleanup_err="$private/corrupt-cleanup.err"
export FAKE_CLEANUP_CORRUPT_OUTPUT=1
if "$runner" "${common[@]}" --phase cleanup --state-file "$corrupt_cleanup_state" \
    >"$corrupt_cleanup_out" 2>"$corrupt_cleanup_err"; then
  fail "cleanup accepted more than one database result object"
fi
jq -e '.cleanup==null' "$corrupt_cleanup_state" >/dev/null || fail "corrupt cleanup output changed state"
assert_private_output "$corrupt_cleanup_out"
assert_private_output "$corrupt_cleanup_err"
export FAKE_CLEANUP_CORRUPT_OUTPUT=0

unsafe_state="$private/unsafe-state.json"
cp "$state" "$unsafe_state"
chmod 600 "$unsafe_state"
unsafe_out="$private/unsafe.out"
unsafe_err="$private/unsafe.err"
export FAKE_DB_PHASE=cleanup FAKE_CLEANUP_UNSAFE=1
requests_before_cleanup="$(wc -l <"$FAKE_REQUEST_COUNT" | tr -d '[:space:]')"
if "$runner" "${common[@]}" --phase cleanup --state-file "$unsafe_state" >"$unsafe_out" 2>"$unsafe_err"; then
  fail "unsafe cleanup was accepted"
fi
jq -e '.cleanup==null' "$unsafe_state" >/dev/null || fail "unsafe cleanup changed durable state"
assert_private_output "$unsafe_out"
assert_private_output "$unsafe_err"
[ "$(wc -l <"$FAKE_REQUEST_COUNT" | tr -d '[:space:]')" = "$requests_before_cleanup" ] ||
  fail "unsafe cleanup reached the ingest endpoint"

cleanup_out="$private/cleanup.out"
cleanup_err="$private/cleanup.err"
export FAKE_DB_PHASE=cleanup FAKE_CLEANUP_UNSAFE=0
"$runner" "${common[@]}" --phase cleanup --state-file "$state" >"$cleanup_out" 2>"$cleanup_err" ||
  fail "safe cleanup failed"
jq -e '.phase=="cleanup" and .messages_matched==1 and .messages_deleted==1 and
  .audit_events_retained==1 and .shared_rate_state_retained==true and
  .provider_mutation_performed==false' "$cleanup_out" >/dev/null || fail "cleanup result is invalid"
jq -e '.cleanup.outcome=="complete" and .cleanup.deleted==1 and .cleanup.events_retained==1' "$state" >/dev/null ||
  fail "cleanup state result is invalid"
assert_private_output "$cleanup_out"
assert_private_output "$cleanup_err"
[ "$(wc -l <"$FAKE_REQUEST_COUNT" | tr -d '[:space:]')" = "$requests_before_cleanup" ] ||
  fail "safe cleanup reached the ingest endpoint"

repeat_cleanup_out="$private/repeat-cleanup.out"
repeat_cleanup_err="$private/repeat-cleanup.err"
if "$runner" "${common[@]}" --phase cleanup --state-file "$state" \
    >"$repeat_cleanup_out" 2>"$repeat_cleanup_err"; then
  fail "completed cleanup was allowed to run twice"
fi
assert_private_output "$repeat_cleanup_out"
assert_private_output "$repeat_cleanup_err"

loose_target="$private/loose-target.json"
cp "$target" "$loose_target"
chmod 644 "$loose_target"
permission_out="$private/permission.out"
permission_err="$private/permission.err"
permission_state="$private/permission-state.json"
export FAKE_DB_PHASE=disabled FAKE_RELAY_VERDICT=feature_disabled
if "$runner" --cell civo-sandbox-usw2-dev --kubeconfig "$kubeconfig" --context fake-civo \
    --phase disabled --target-file "$loose_target" --state-file "$permission_state" \
    "${signed[@]}" >"$permission_out" 2>"$permission_err"; then
  fail "loose private target permissions were accepted"
fi
[ ! -e "$permission_state" ] || fail "permission failure created state"
assert_private_output "$permission_out"
assert_private_output "$permission_err"

if grep -E 'wrangler|cloudflare\.com|api\.cloudflare| account (create|set)| plan (set|clear)' "$FAKE_COMMAND_LOG" >/dev/null; then
  fail "smoke harness invoked an out-of-scope provider/account/plan operation"
fi
grep -F -- 'proxy --address=127.0.0.1 --port=0' "$FAKE_COMMAND_LOG" >/dev/null ||
  fail "operation-lock cleanup did not use the preconditioned Kubernetes API path"
if grep -F -- 'delete configmap witself-agent-email-operation-lock' "$FAKE_COMMAND_LOG" >/dev/null; then
  fail "operation-lock cleanup used a name-only kubectl delete"
fi
[ -f "$FAKE_LOCK_DELETED" ] || fail "owned operation lock was never deleted"

printf '%s\n' 'test-agent-email-cell-smoke: ok'
