#!/usr/bin/env bash
set -euo pipefail

action="${1:-provision}"
queue_name="${WITSELF_EMAIL_EVENT_QUEUE:-witself-agent-email-send-events}"
dead_letter_queue="${WITSELF_EMAIL_EVENT_DLQ:-witself-agent-email-send-events-dlq}"
subscription_name="${WITSELF_EMAIL_EVENT_SUBSCRIPTION:-witself-agent-email-send-lifecycle}"
zone_id="${CLOUDFLARE_ZONE_ID:-}"
sending_domain="${WITSELF_EMAIL_SENDING_DOMAIN:-send.witmail.net}"
script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
config_path="${script_dir}/../wrangler.template.jsonc"

wrangler() {
  if [[ -n "${CLOUDFLARE_PROFILE:-}" ]]; then
    npx wrangler --config "${config_path}" --profile "${CLOUDFLARE_PROFILE}" "$@"
  else
    npx wrangler --config "${config_path}" "$@"
  fi
}

ensure_queue() {
  local name="$1"
  if ! wrangler queues update "${name}" --message-retention-period-secs 1209600 >/dev/null 2>&1; then
    wrangler queues create "${name}" --message-retention-period-secs 1209600
  fi
  wrangler queues update "${name}" --message-retention-period-secs 1209600 >/dev/null
}

subscription_id() {
  local subscriptions
  subscriptions="$(wrangler queues subscription list "${queue_name}" --per-page 100 --json)"
  SUBSCRIPTIONS_JSON="${subscriptions}" SUBSCRIPTION_NAME="${subscription_name}" node -e '
    const parsed = JSON.parse(process.env.SUBSCRIPTIONS_JSON);
    const rows = Array.isArray(parsed) ? parsed : parsed.result ?? parsed.subscriptions ?? [];
    const match = rows.find((row) => row && row.name === process.env.SUBSCRIPTION_NAME);
    if (match?.id) process.stdout.write(String(match.id));
  '
}

case "${action}" in
  provision)
    if [[ ! "${zone_id}" =~ ^[0-9a-f]{32}$ ]]; then
      echo "CLOUDFLARE_ZONE_ID must be the 32-character lowercase hex zone id" >&2
      exit 2
    fi
    if [[ -z "${sending_domain}" || "${sending_domain}" == *[[:space:]]* ]]; then
      echo "WITSELF_EMAIL_SENDING_DOMAIN must be a verified sending domain" >&2
      exit 2
    fi
    ensure_queue "${dead_letter_queue}"
    ensure_queue "${queue_name}"
    id="$(subscription_id)"
    if [[ -z "${id}" ]]; then
      wrangler queues subscription create "${queue_name}" \
        --name "${subscription_name}" \
        --source email.sending \
        --events message.delivered,message.deferred,message.bounced,message.failed,message.rejected,message.complained \
        --zone-id "${zone_id}" \
        --domain "${sending_domain}" \
        --enabled=false
    fi
    ;;
  enable|disable)
    id="$(subscription_id)"
    if [[ -z "${id}" ]]; then
      echo "subscription ${subscription_name} does not exist; run provision first" >&2
      exit 1
    fi
    if [[ "${action}" == "enable" ]]; then enabled=true; else enabled=false; fi
    wrangler queues subscription update "${queue_name}" --id "${id}" --enabled="${enabled}"
    ;;
  status)
    wrangler queues subscription list "${queue_name}" --per-page 100 --json
    ;;
  *)
    echo "usage: $0 [provision|enable|disable|status]" >&2
    exit 2
    ;;
esac
