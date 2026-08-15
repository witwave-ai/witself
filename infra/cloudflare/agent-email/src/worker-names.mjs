// The production receive service and the retired literal-route compatibility
// lane are intentionally different Cloudflare Workers. Keeping both identities
// explicit prevents production route, deploy, secret, and rollback operations
// from mutating the legacy Worker by accident.
export const PRODUCTION_RECEIVE_WORKER = "witself-agent-email-receive";
export const LEGACY_PILOT_WORKER = "witself-agent-email-pilot";
