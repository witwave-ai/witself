#!/usr/bin/env bash
# Stage Stripe LIVE credentials as control-plane Worker secrets — dark.
#
# Reads the two secret values from interactive prompts (never argv, never
# echoed), fetches the default Customer Portal configuration id (bpc_...)
# with the supplied secret key, then stores everything via `wrangler secret
# put` for the control-plane Worker. Staging is dark: the Go container only
# receives env at plan-lifecycle activation, and billing stays disabled
# until CP_BILLING_PROVIDER/CP_STRIPE_MODE/CP_PLAN_LIFECYCLE_ENABLED are
# set during the real cutover.
#
# Run from the repo root:  bash scripts/stage-stripe-live-secrets.sh
set -euo pipefail
cd "$(dirname "$0")/../infra/cloudflare/control-plane"

read -r -s -p "Paste sk_live_... secret key (input hidden): " SK; echo
case "$SK" in sk_live_*) ;; *) echo "not an sk_live_ key; aborting" >&2; exit 1;; esac

read -r -s -p "Paste whsec_... webhook signing secret (input hidden): " WH; echo
case "$WH" in whsec_*) ;; *) echo "not a whsec_ secret; aborting" >&2; exit 1;; esac

echo "Fetching default Customer Portal configuration id..."
BPC=$(curl -sf -u "$SK:" "https://api.stripe.com/v1/billing_portal/configurations?is_default=true&limit=1" \
  | python3 -c 'import json,sys; d=json.load(sys.stdin)["data"]; print(d[0]["id"] if d else "")')
case "$BPC" in bpc_*) echo "portal configuration: $BPC";; *) echo "no default portal configuration found — save the portal settings in the dashboard first" >&2; exit 1;; esac

printf '%s' "$SK" | npx --no-install wrangler secret put CP_STRIPE_SECRET_KEY --name witself-control-plane
printf '%s' "$WH" | npx --no-install wrangler secret put CP_STRIPE_WEBHOOK_SECRET --name witself-control-plane
printf '%s' "$BPC" | npx --no-install wrangler secret put CP_STRIPE_PORTAL_CONFIGURATION_ID --name witself-control-plane
printf '%s' "https://self.witwave.ai/billing/success" | npx --no-install wrangler secret put CP_STRIPE_SUCCESS_URL --name witself-control-plane
printf '%s' "https://self.witwave.ai/billing/cancelled" | npx --no-install wrangler secret put CP_STRIPE_CANCEL_URL --name witself-control-plane
printf '%s' "https://self.witwave.ai/billing/portal-return" | npx --no-install wrangler secret put CP_STRIPE_PORTAL_RETURN_URL --name witself-control-plane

echo "Staged. Billing remains dark: CP_BILLING_PROVIDER / CP_STRIPE_MODE /"
echo "CP_PLAN_LIFECYCLE_ENABLED / CP_BILLING_ACCOUNT_ALLOWLIST are untouched;"
echo "the container picks everything up only at the cutover activation."
