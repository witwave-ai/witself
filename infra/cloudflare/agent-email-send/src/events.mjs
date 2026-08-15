const ACCOUNT_ID = /^acc_[a-z2-7]{16}$/;
const KEY_ID = /^[a-z][a-z0-9_.-]{0,63}$/;
const EVENT_ID = /^[A-Za-z0-9_.:-]{1,256}$/;
const MAX_EVENT_CELLS = 8;
const MAX_EVENT_ACCOUNTS = 100;
const MAX_HISTORICAL_SIGNERS_PER_CELL = 32;
const MAX_EVENT_TARGET_CONFIG_BYTES = 5 * 1024;

const EVENT_CLASSES = new Map([
  ["cf.email.sending.message.delivered", "delivered"],
  ["cf.email.sending.message.deferred", "deferred"],
  ["cf.email.sending.message.bounced", "bounced"],
  ["cf.email.sending.message.failed", "failed"],
  ["cf.email.sending.message.rejected", "rejected"],
  ["cf.email.sending.message.complained", "complained"],
]);

class PermanentEventError extends Error {}

function canonicalTargetURL(raw) {
  let parsed;
  try {
    parsed = new URL(raw);
  } catch {
    return null;
  }
  if (raw !== raw.trim() || parsed.protocol !== "https:" ||
      parsed.username || parsed.password || parsed.search || parsed.hash ||
      !parsed.hostname || parsed.hostname === "localhost" ||
      parsed.pathname !== "/v1/internal/agent-email-send:provider-event") {
    return null;
  }
  return parsed.toString();
}

export function parseEventTargets(raw) {
  const encoded = new TextEncoder().encode(String(raw ?? ""));
  if (encoded.length < 2 || encoded.length > MAX_EVENT_TARGET_CONFIG_BYTES) {
    throw new Error("provider event targets are invalid");
  }
  let value;
  try {
    value = JSON.parse(new TextDecoder().decode(encoded));
  } catch {
    throw new Error("provider event targets are invalid");
  }
  if (!value || Array.isArray(value) || typeof value !== "object") {
    throw new Error("provider event targets are invalid");
  }
  if (Object.keys(value).length !== 2 || !("cells" in value) ||
      !("account_targets" in value)) {
    throw new Error("provider event targets are invalid");
  }
  if (!value.cells || Array.isArray(value.cells) ||
      typeof value.cells !== "object" || !value.account_targets ||
      Array.isArray(value.account_targets) ||
      typeof value.account_targets !== "object") {
    throw new Error("provider event targets are invalid");
  }
  const cellEntries = Object.entries(value.cells);
  if (cellEntries.length < 1 || cellEntries.length > MAX_EVENT_CELLS) {
    throw new Error("provider event targets must contain 1-8 cells");
  }
  const cells = new Map();
  for (const [cellId, target] of cellEntries) {
    const url = canonicalTargetURL(target?.url);
    const targetKeys = target && typeof target === "object" &&
      !Array.isArray(target) ? Object.keys(target) : [];
    if (!KEY_ID.test(cellId) || targetKeys.length !== 3 ||
        !targetKeys.includes("url") || !targetKeys.includes("token") ||
        !targetKeys.includes("accepted_signer_key_ids") || !url ||
        typeof target.token !== "string" ||
        target.token.length < 32 || target.token.length > 4096 ||
        target.token !== target.token.trim() || /[\r\n]/.test(target.token) ||
        !Array.isArray(target.accepted_signer_key_ids) ||
        target.accepted_signer_key_ids.length < 1 ||
        target.accepted_signer_key_ids.length > MAX_HISTORICAL_SIGNERS_PER_CELL ||
        !target.accepted_signer_key_ids.every((keyId) => KEY_ID.test(keyId)) ||
        new Set(target.accepted_signer_key_ids).size !==
          target.accepted_signer_key_ids.length) {
      throw new Error("provider event target is invalid");
    }
    cells.set(cellId, {
      url,
      token: target.token,
      acceptedSignerKeyIds: new Set(target.accepted_signer_key_ids),
    });
  }
  const accountEntries = Object.entries(value.account_targets);
  if (accountEntries.length < 1 || accountEntries.length > MAX_EVENT_ACCOUNTS) {
    throw new Error("provider event targets must contain 1-100 accounts");
  }
  const accountTargets = new Map();
  for (const [accountId, cellId] of accountEntries) {
    if (!ACCOUNT_ID.test(accountId) || !KEY_ID.test(cellId) || !cells.has(cellId)) {
      throw new Error("provider event account target is invalid");
    }
    accountTargets.set(accountId, cellId);
  }
  return { cells, accountTargets };
}

export function normalizeProviderEvent(raw, sendDomain = "send.witmail.net") {
  const eventClass = EVENT_CLASSES.get(raw?.type);
  const eventId = raw?.payload?.eventId;
  const messageId = raw?.payload?.messageId;
  const occurredAt = raw?.metadata?.eventTimestamp;
  const occurredMillis = Date.parse(occurredAt);
  if (!eventClass || raw?.source?.type !== "email.sending" ||
      raw.source.domain !== sendDomain || !EVENT_ID.test(eventId) ||
      typeof messageId !== "string" || messageId.length < 1 ||
      messageId.length > 512 || /[\r\n]/.test(messageId) ||
      raw?.metadata?.eventSchemaVersion !== 1 ||
      !Number.isFinite(occurredMillis)) {
    throw new PermanentEventError("provider event is invalid");
  }
  const event = {
    schema_version: "witself.agent-email-provider-event.v1",
    event_id: eventId,
    provider_message_id: messageId,
    event_class: eventClass,
    occurred_at: new Date(occurredMillis).toISOString(),
  };
  if (eventClass === "bounced") {
    switch (raw?.payload?.bounce?.type) {
      case "hard": event.bounce_type = "permanent"; break;
      case "soft": event.event_class = "failed"; break;
      default: event.event_class = "failed";
    }
  }
  return event;
}

export async function getProviderRoute(env, providerMessageId) {
  if (!env.PROVIDER_ROUTES) throw new Error("provider route binding is missing");
  const id = env.PROVIDER_ROUTES.idFromName(providerMessageId);
  const response = await env.PROVIDER_ROUTES.get(id).fetch("https://route.internal/route");
  if (response.status === 404) return null;
  if (response.status !== 200) throw new Error("provider route lookup failed");
  const route = await response.json();
  if (route?.schema_version !== "witself.agent-email-provider-route.v1" ||
      !ACCOUNT_ID.test(route.account_id) || !KEY_ID.test(route.signer_key_id)) {
    throw new Error("provider route is invalid");
  }
  return route;
}

export async function forwardProviderEvent(event, route, env, fetchImpl = fetch) {
  const routing = parseEventTargets(env.EVENT_TARGETS_JSON);
  const cellId = routing.accountTargets.get(route.account_id);
  const target = routing.cells.get(cellId);
  if (!target || !target.acceptedSignerKeyIds.has(route.signer_key_id)) {
    throw new Error("provider event target does not authorize route");
  }
  const controller = new AbortController();
  const timeout = setTimeout(() => controller.abort(), 10_000);
  let response;
  try {
    response = await fetchImpl(target.url, {
      method: "POST",
      redirect: "error",
      signal: controller.signal,
      headers: {
        "Authorization": `Bearer ${target.token}`,
        "Content-Type": "application/json",
      },
      body: JSON.stringify(event),
    });
  } finally {
    clearTimeout(timeout);
  }
  if (response.status === 204) return "ack";
  // Only the cell's durable success is acknowledgement authority. A 404 can
  // race the worker's accepted-state commit, while validation or idempotency
  // conflicts can expose clock skew, schema drift, or changed provider
  // evidence. Retry every non-success through the bounded Queue policy so a
  // persistent failure becomes inspectable DLQ evidence instead of silently
  // losing a bounce or complaint.
  return "retry";
}

export async function consumeProviderEvents(batch, env) {
  for (const message of batch.messages) {
    try {
      if (String(env.EVENT_DELIVERY_ENABLED ?? "false") !== "true") {
        message.retry({ delaySeconds: 60 });
        continue;
      }
      const event = normalizeProviderEvent(message.body, String(env.SEND_DOMAIN ?? "send.witmail.net"));
      const route = await getProviderRoute(env, event.provider_message_id);
      if (!route) {
        // Late route registration or unexpected route loss must remain visible.
        // Retrying through max_retries lets the Queue move it to the DLQ rather
        // than silently acknowledging an event that could not be routed.
        message.retry({ delaySeconds: 60 });
        continue;
      }
      const result = await forwardProviderEvent(event, route, env);
      if (result === "ack") message.ack();
      else message.retry({ delaySeconds: 60 });
    } catch {
      // Event-subscription schema drift and malformed provider evidence must
      // remain inspectable. Bounded Queue retries move every unprocessable
      // event to the DLQ instead of silently deleting bounce/complaint facts.
      message.retry({ delaySeconds: 60 });
    }
  }
}
