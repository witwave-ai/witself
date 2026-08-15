import {
  DISPATCH_SCHEMA,
  RECEIPT_PROOF_SCHEMA,
  RESPONSE_SCHEMA,
} from "./signature.mjs";

const ID = /^(?:esnd|acc|realm|agent)_[A-Za-z0-9_-]{1,128}$/;
const MESSAGE_ID = /^<[^<>\r\n]{1,996}>$/;
const CLOSED_CODE = /^[a-z][a-z0-9_.-]{0,63}$/;
const KEY_ID = /^[a-z][a-z0-9_.-]{0,63}$/;
const TERMINAL = new Set([
  "accepted",
  "delivered",
  "queued",
  "permanent_bounce",
  "rejected",
  "ambiguous",
]);
const ROUTE_REPAIR_INITIAL_DELAY_MS = 30_000;
const ROUTE_REPAIR_MAX_DELAY_MS = 60 * 60 * 1000;
const MAX_RECEIPT_COUNTER = 1_000_000;
const RECEIPT_PROOF_ERROR_CODES = new Set([
  "receipt_missing",
  "receipt_conflict",
  "receipt_unresolved",
]);
const RECEIPT_PROOF_ERROR_STATES = new Set([
  "missing",
  "conflict",
  "unresolved",
]);
const ACCEPTED_RESPONSE_FIELDS = new Set([
  "schema_version",
  "send_id",
  "state",
  "provider",
  "provider_message_id",
]);
const RECEIPT_FINALIZATION_STATES = new Set([
  "provider_started",
  "accepted",
]);
export const RECEIPT_IDEMPOTENCY_TTL_MS = 7 * 24 * 60 * 60 * 1000;
export const PROVIDER_ROUTE_TTL_MS = 400 * 24 * 60 * 60 * 1000;

function canonicalMailbox(value) {
  if (
    typeof value !== "string" ||
    value.length < 3 ||
    value.length > 320 ||
    value !== value.trim() ||
    /[\r\n\s]/.test(value)
  ) {
    return null;
  }
  const at = value.lastIndexOf("@");
  if (at < 1 || at === value.length - 1) return null;
  const local = value.slice(0, at);
  const domain = value.slice(at + 1);
  if (
    domain !== domain.toLowerCase() ||
    !/^[a-z0-9](?:[a-z0-9.-]{0,251}[a-z0-9])?$/.test(domain) ||
    domain.includes("..") ||
    !/^[A-Za-z0-9.!#$%&'*+/=?^_`{|}~-]+$/.test(local)
  ) {
    return null;
  }
  return { address: value, local, domain };
}

export function validateDispatch(value, env) {
  if (
    !value ||
    typeof value !== "object" ||
    Array.isArray(value) ||
    value.schema_version !== DISPATCH_SCHEMA ||
    !ID.test(value.send_id) ||
    !value.send_id.startsWith("esnd_") ||
    !ID.test(value.account_id) ||
    !value.account_id.startsWith("acc_") ||
    !ID.test(value.realm_id) ||
    !value.realm_id.startsWith("realm_") ||
    !ID.test(value.agent_id) ||
    !value.agent_id.startsWith("agent_")
  ) {
    throw new Error("dispatch identity is invalid");
  }
  const allowed = new Set([
    "schema_version", "send_id", "account_id", "realm_id", "agent_id",
    "from", "reply_to", "to", "subject", "text", "in_reply_to", "references",
  ]);
  if (Object.keys(value).some((key) => !allowed.has(key))) {
    throw new Error("dispatch contains an unknown field");
  }
  const from = canonicalMailbox(value.from);
  const replyTo = canonicalMailbox(value.reply_to);
  const to = canonicalMailbox(value.to);
  const sendDomain = String(env.SEND_DOMAIN ?? "send.witmail.net");
  const replyDomain = String(env.REPLY_DOMAIN ?? "witmail.net");
  if (
    !from ||
    !replyTo ||
    !to ||
    from.domain !== sendDomain ||
    replyTo.domain !== replyDomain ||
    from.local !== replyTo.local
  ) {
    throw new Error("dispatch address is invalid");
  }
  if (
    typeof value.subject !== "string" ||
    new TextEncoder().encode(value.subject).length > 4096 ||
    /[\r\n\u0000-\u0008\u000b\u000c\u000e-\u001f\u007f]/.test(value.subject) ||
    typeof value.text !== "string" ||
    value.text.length < 1 ||
    new TextEncoder().encode(value.text).length > 256 * 1024 ||
    value.text.includes("\u0000")
  ) {
    throw new Error("dispatch content bounds are invalid");
  }
  if (value.in_reply_to && !MESSAGE_ID.test(value.in_reply_to)) {
    throw new Error("dispatch thread parent is invalid");
  }
  if (
    value.references !== undefined &&
    (!Array.isArray(value.references) ||
      value.references.length > 16 ||
      !value.references.every((item) => MESSAGE_ID.test(item)))
  ) {
    throw new Error("dispatch references are invalid");
  }
  return value;
}

export function response(sendId, state, options = {}) {
  const body = {
    schema_version: RESPONSE_SCHEMA,
    send_id: sendId,
    state,
    provider: "cloudflare_email_sending",
  };
  if (options.providerMessageId) {
    body.provider_message_id = String(options.providerMessageId).slice(0, 512);
  }
  if (options.errorCode) {
    if (!CLOSED_CODE.test(options.errorCode)) throw new Error("bad closed code");
    body.error_code = options.errorCode;
  }
  if (options.retryAfterSeconds) {
    body.retry_after_seconds = Math.min(86400, Math.max(1, options.retryAfterSeconds));
  }
  return body;
}

function jsonResponse(body, status = 200) {
  return new Response(JSON.stringify(body), {
    status,
    headers: {
      "Content-Type": "application/json",
      "Cache-Control": "private, no-store",
    },
  });
}

export function receiptProofFailure(
  status,
  errorCode,
  sendId = "esnd_invalid",
  receiptState = "unresolved",
) {
  if (
    !RECEIPT_PROOF_ERROR_CODES.has(errorCode) ||
    !RECEIPT_PROOF_ERROR_STATES.has(receiptState)
  ) {
    throw new Error("receipt proof failure is invalid");
  }
  return jsonResponse({
    schema_version: RECEIPT_PROOF_SCHEMA,
    send_id: ID.test(sendId) && sendId.startsWith("esnd_")
      ? sendId
      : "esnd_invalid",
    receipt_state: receiptState,
    error_code: errorCode,
  }, status);
}

function boundedCounter(value, minimum = 0) {
  return Number.isSafeInteger(value) &&
    value >= minimum && value <= MAX_RECEIPT_COUNTER;
}

function receiptSignerKeyId(receipt) {
  if (KEY_ID.test(receipt?.signer_key_id ?? "")) {
    return receipt.signer_key_id;
  }
  if (KEY_ID.test(receipt?.route?.signer_key_id ?? "")) {
    return receipt.route.signer_key_id;
  }
  return null;
}

function exactObject(left, right) {
  return Boolean(left) && Boolean(right) &&
    JSON.stringify(left) === JSON.stringify(right);
}

function acceptedResponseMatches(value, sendId, providerMessageId) {
  return value?.schema_version === RESPONSE_SCHEMA &&
    value.send_id === sendId &&
    value.state === "accepted" &&
    value.provider === "cloudflare_email_sending" &&
    value.provider_message_id === providerMessageId &&
    Object.keys(value).every((key) => ACCEPTED_RESPONSE_FIELDS.has(key)) &&
    Object.keys(value).length === ACCEPTED_RESPONSE_FIELDS.size;
}

function receiptLineageMatches(current, expected, acceptedResult, now) {
  const providerCountIsCompatible = (
    current?.provider_call_started_count === undefined &&
    expected?.provider_call_started_count === undefined
  ) || (
    boundedCounter(current?.provider_call_started_count, 1) &&
    current.provider_call_started_count ===
      expected?.provider_call_started_count
  );
  if (
    !current || !expected ||
    !RECEIPT_FINALIZATION_STATES.has(current.state) ||
    !RECEIPT_FINALIZATION_STATES.has(expected.state) ||
    current.digest !== expected.digest ||
    current.provider_message_id !== expected.provider_message_id ||
    typeof current.provider_message_id !== "string" ||
    current.provider_message_id.length < 1 ||
    current.provider_message_id.length > 512 ||
    current.expires_at !== expected.expires_at ||
    !Number.isFinite(Date.parse(current.expires_at)) ||
    Date.parse(current.expires_at) <= now ||
    !providerCountIsCompatible ||
    current.signer_key_id !== expected.signer_key_id ||
    receiptSignerKeyId(current) !== receiptSignerKeyId(expected) ||
    !receiptSignerKeyId(current) ||
    !validProviderRoute(current.route) ||
    !exactObject(current.route, expected.route) ||
    !acceptedResponseMatches(
      acceptedResult,
      current.route.send_id,
      current.provider_message_id,
    )
  ) {
    return false;
  }
  for (const receipt of [current, expected]) {
    if (receipt.state === "accepted" && (
      receipt.status !== 200 ||
      typeof receipt.route_pending !== "boolean" ||
      !exactObject(receipt.response, acceptedResult)
    )) {
      return false;
    }
  }
  return true;
}

function providerError(error) {
  const code = String(error?.code ?? "");
  switch (code) {
    case "E_RATE_LIMIT_EXCEEDED":
      return { state: "retryable", status: 429, errorCode: "provider_rate_limited", retryAfterSeconds: 60 };
    case "E_DAILY_LIMIT_EXCEEDED":
      return { state: "retryable", status: 429, errorCode: "provider_daily_limit", retryAfterSeconds: 3600 };
    case "E_RECIPIENT_SUPPRESSED":
      return { state: "rejected", status: 422, errorCode: "recipient_suppressed" };
    case "E_VALIDATION_ERROR":
    case "E_FIELD_MISSING":
    case "E_TOO_MANY_RECIPIENTS":
    case "E_TOO_MANY_ATTACHMENTS":
    case "E_SENDER_NOT_VERIFIED":
    case "E_RECIPIENT_NOT_ALLOWED":
    case "E_SENDER_DOMAIN_NOT_AVAILABLE":
    case "E_CONTENT_TOO_LARGE":
    case "E_HEADER_NOT_ALLOWED":
    case "E_HEADER_USE_API_FIELD":
    case "E_HEADER_VALUE_INVALID":
    case "E_HEADER_VALUE_TOO_LONG":
    case "E_HEADER_NAME_INVALID":
    case "E_HEADERS_TOO_LARGE":
    case "E_HEADERS_TOO_MANY":
      return { state: "rejected", status: 422, errorCode: "provider_rejected" };
    case "E_DELIVERY_FAILED":
      // This code can represent either a hard bounce or exhausted temporary
      // retries. Only a lifecycle event with bounce.type=hard is strong enough
      // to create our durable recipient suppression.
      return { state: "rejected", status: 422, errorCode: "provider_delivery_failed" };
    default:
      return { state: "ambiguous", status: 503, errorCode: "provider_outcome_ambiguous" };
  }
}

export class OutboundReceipt {
  constructor(state, env) {
    this.state = state;
    this.env = env;
  }

  async fetch(request) {
    if (request.method !== "POST") {
      return new Response("method not allowed", { status: 405 });
    }
    const path = new URL(request.url).pathname;
    if (path === "/receipt-replay") {
      return this.receiptReplay(request);
    }
    if (path !== "/dispatch") {
      return new Response("not found", { status: 404 });
    }
    return this.dispatch(request);
  }

  async dispatch(request) {
    const digest = request.headers.get("X-Witself-Verified-Digest") ?? "";
    const signerKeyId = request.headers.get("X-Witself-Verified-Key-Id") ?? "";
    const dispatch = validateDispatch(await request.json(), this.env);
    if (!KEY_ID.test(signerKeyId)) {
      return jsonResponse(response(dispatch.send_id, "rejected", { errorCode: "signer_invalid" }), 400);
    }
    const existing = await this.state.storage.get("receipt");
    if (existing && existing.digest !== digest) {
      return jsonResponse(response(dispatch.send_id, "rejected", { errorCode: "idempotency_conflict" }), 409);
    }
    if (existing?.state === "provider_started" && !existing.provider_message_id) {
      return jsonResponse(response(dispatch.send_id, "ambiguous", { errorCode: "provider_outcome_ambiguous" }), 503);
    }
    if (existing?.state === "provider_started" && existing.provider_message_id) {
      const result = response(dispatch.send_id, "accepted", {
        providerMessageId: existing.provider_message_id,
      });
      let routePending = false;
      try {
        await registerProviderRoute(this.env, existing.provider_message_id, existing.route);
      } catch {
        routePending = true;
      }
      await this.persistAccepted(existing, result, routePending);
      return jsonResponse(result);
    }
    if (existing && TERMINAL.has(existing.state)) {
      if (existing.route_pending) {
        try {
          await this.scheduleRouteRepair(
            existing.route_attempts ?? 1,
            existing.expires_at,
          );
        } catch {
          // The accepted provider result remains authoritative. A later exact
          // replay or the existing alarm can retry this content-free repair.
        }
      }
      return jsonResponse(existing.response, existing.status ?? 200);
    }

    if (existing && existing.state !== "retryable") {
      return jsonResponse(response(dispatch.send_id, "ambiguous", { errorCode: "provider_outcome_ambiguous" }), 503);
    }

    const previousProviderCalls = existing
      ? existing.provider_call_started_count === undefined &&
          existing.state === "retryable"
        // Receipts shipped before provider-call accounting could only enter
        // retryable after crossing EMAIL.send. Count that unseen boundary as
        // one so an upgraded retry can never yield a one-call proof.
        ? 1
        : existing.provider_call_started_count
      : 0;
    if (
      !boundedCounter(previousProviderCalls, existing ? 1 : 0) ||
      previousProviderCalls >= MAX_RECEIPT_COUNTER
    ) {
      return jsonResponse(response(dispatch.send_id, "ambiguous", { errorCode: "provider_outcome_ambiguous" }), 503);
    }
    const previousVerifiedReplays = existing?.verified_replay_count ?? 0;
    if (!boundedCounter(previousVerifiedReplays)) {
      return jsonResponse(response(dispatch.send_id, "ambiguous", { errorCode: "provider_outcome_ambiguous" }), 503);
    }
    const emailBinding = this.env.EMAIL;
    if (!emailBinding || typeof emailBinding.send !== "function") {
      return jsonResponse(response(dispatch.send_id, "retryable", {
        errorCode: "provider_unavailable",
        retryAfterSeconds: 60,
      }), 503);
    }

    const expiresAt = new Date(Date.now() + RECEIPT_IDEMPOTENCY_TTL_MS).toISOString();
    const providerStarted = {
      digest,
      signer_key_id: signerKeyId,
      state: "provider_started",
      provider_call_started_count: previousProviderCalls + 1,
      verified_replay_count: previousVerifiedReplays,
      started_at: new Date().toISOString(),
      expires_at: expiresAt,
    };
    await this.scheduleExpiry(expiresAt);
    const headers = {};
    if (dispatch.in_reply_to) headers["In-Reply-To"] = dispatch.in_reply_to;
    if (dispatch.references?.length) headers.References = dispatch.references.join(" ");
    // This durable write is the final awaited operation before the real
    // provider boundary. A crash after it is conservatively unresolved, while
    // a failed write prevents EMAIL.send from being called at all.
    await this.state.storage.put("receipt", providerStarted);
    let sent;
    try {
      sent = await emailBinding.send({
        from: dispatch.from,
        replyTo: dispatch.reply_to,
        to: dispatch.to,
        subject: dispatch.subject,
        text: dispatch.text,
        headers,
      });
      if (!sent || typeof sent.messageId !== "string" || sent.messageId.length < 1) {
        throw Object.assign(new Error("provider returned no message id"), {
          code: "E_INTERNAL_SERVER_ERROR",
        });
      }
    } catch (error) {
      const mapped = providerError(error);
      const result = response(dispatch.send_id, mapped.state, mapped);
      const persist = mapped.state === "retryable" ? {
        ...providerStarted,
        state: "retryable",
        status: mapped.status,
        response: result,
        completed_at: new Date().toISOString(),
      } : {
        ...providerStarted,
        state: mapped.state,
        status: mapped.status,
        response: result,
        completed_at: new Date().toISOString(),
      };
      await this.state.storage.put("receipt", persist);
      await this.scheduleExpiry(expiresAt);
      return jsonResponse(result, mapped.status);
    }

    const route = {
      schema_version: "witself.agent-email-provider-route.v1",
      send_id: dispatch.send_id,
      account_id: dispatch.account_id,
      realm_id: dispatch.realm_id,
      signer_key_id: signerKeyId,
    };
    const providerAccepted = {
      ...providerStarted,
      state: "provider_started",
      provider_message_id: sent.messageId,
      route,
      started_at: new Date().toISOString(),
    };
    await this.state.storage.put("receipt", providerAccepted);
    let routePending = false;
    try {
      await registerProviderRoute(this.env, sent.messageId, route);
    } catch {
      routePending = true;
    }
    const result = response(dispatch.send_id, "accepted", {
      providerMessageId: sent.messageId,
    });
    await this.persistAccepted(providerAccepted, result, routePending);
    return jsonResponse(result);
  }

  async receiptReplay(request) {
    const digest = request.headers.get("X-Witself-Verified-Digest") ?? "";
    const signerKeyId = request.headers.get("X-Witself-Verified-Key-Id") ?? "";
    let dispatch;
    try {
      dispatch = validateDispatch(await request.json(), this.env);
    } catch {
      return receiptProofFailure(409, "receipt_conflict", "esnd_invalid", "conflict");
    }
    if (!KEY_ID.test(signerKeyId)) {
      return receiptProofFailure(409, "receipt_conflict", dispatch.send_id, "conflict");
    }
    const outcome = await this.state.storage.transaction(async (transaction) => {
      const existing = await transaction.get("receipt");
      if (!existing) return { state: "missing" };
      if (existing.digest !== digest) return { state: "conflict" };
      const storedSignerKeyId = receiptSignerKeyId(existing);
      if (storedSignerKeyId && storedSignerKeyId !== signerKeyId) {
        return { state: "conflict" };
      }
      if (!storedSignerKeyId || existing.state !== "accepted") {
        return { state: "unresolved" };
      }
      const route = existing.route;
      if (
        existing.status !== 200 ||
        typeof existing.provider_message_id !== "string" ||
        existing.provider_message_id.length < 1 ||
        existing.provider_message_id.length > 512 ||
        !validProviderRoute(route) ||
        route.send_id !== dispatch.send_id ||
        route.account_id !== dispatch.account_id ||
        route.realm_id !== dispatch.realm_id ||
        route.signer_key_id !== signerKeyId ||
        !acceptedResponseMatches(
          existing.response,
          dispatch.send_id,
          existing.provider_message_id,
        )
      ) {
        return { state: "conflict" };
      }
      if (
        typeof existing.route_pending !== "boolean" ||
        !boundedCounter(existing.provider_call_started_count, 1) ||
        !boundedCounter(existing.verified_replay_count) ||
        existing.verified_replay_count >= MAX_RECEIPT_COUNTER ||
        !Number.isFinite(Date.parse(existing.expires_at)) ||
        Date.parse(existing.expires_at) <= Date.now()
      ) {
        return { state: "unresolved" };
      }
      const verifiedReplayCount = existing.verified_replay_count + 1;
      await transaction.put("receipt", {
        ...existing,
        verified_replay_count: verifiedReplayCount,
      });
      return {
        state: "accepted",
        providerCallStartedCount: existing.provider_call_started_count,
        verifiedReplayCount,
        routePending: existing.route_pending,
      };
    });
    if (outcome.state === "missing") {
      return receiptProofFailure(404, "receipt_missing", dispatch.send_id, "missing");
    }
    if (outcome.state === "conflict") {
      return receiptProofFailure(409, "receipt_conflict", dispatch.send_id, "conflict");
    }
    if (outcome.state !== "accepted") {
      return receiptProofFailure(409, "receipt_unresolved", dispatch.send_id);
    }
    return jsonResponse({
      schema_version: RECEIPT_PROOF_SCHEMA,
      send_id: dispatch.send_id,
      receipt_state: "accepted",
      digest_matched: true,
      signer_matched: true,
      provider_call_started_count: outcome.providerCallStartedCount,
      verified_replay_count: outcome.verifiedReplayCount,
      route_pending: outcome.routePending,
    });
  }

  async persistAccepted(existing, result, routePending) {
    if (typeof routePending !== "boolean") {
      throw new Error("accepted receipt route state is invalid");
    }
    const now = Date.now();
    const completedAt = new Date(now).toISOString();
    const receipt = await this.state.storage.transaction(async (transaction) => {
      const current = await transaction.get("receipt");
      if (!receiptLineageMatches(current, existing, result, now)) return null;
      const pending = current.state === "accepted" &&
          current.route_pending === false
        ? false
        : routePending;
      const currentAttempts = boundedCounter(current.route_attempts)
        ? current.route_attempts
        : 0;
      const merged = {
        ...current,
        state: "accepted",
        status: 200,
        response: current.state === "accepted" ? current.response : result,
        route_pending: pending,
        route_attempts: pending ? Math.max(1, currentAttempts) : 0,
        completed_at: current.completed_at ?? completedAt,
      };
      await transaction.put("receipt", merged);
      return merged;
    });
    if (!receipt) return null;
    if (receipt.route_pending) {
      try {
        await this.scheduleRouteRepair(
          receipt.route_attempts,
          receipt.expires_at,
        );
      } catch {
        // Never turn a known provider acceptance into an ambiguous retry.
      }
    } else {
      await this.scheduleExpiry(receipt.expires_at);
    }
    return receipt;
  }

  async persistRouteRepairFailure(existing) {
    const now = Date.now();
    return this.state.storage.transaction(async (transaction) => {
      const current = await transaction.get("receipt");
      if (
        !receiptLineageMatches(current, existing, existing.response, now) ||
        current.state !== "accepted" ||
        current.route_pending !== true ||
        !boundedCounter(current.route_attempts, 1) ||
        current.route_attempts >= MAX_RECEIPT_COUNTER
      ) {
        return null;
      }
      const merged = {
        ...current,
        route_attempts: current.route_attempts + 1,
      };
      await transaction.put("receipt", merged);
      return merged;
    });
  }

  async scheduleRouteRepair(attempts, expiresAt) {
    if (typeof this.state.storage.setAlarm !== "function") return;
    const exponent = Math.min(Math.max(0, attempts - 1), 7);
    const delay = Math.min(
      ROUTE_REPAIR_MAX_DELAY_MS,
      ROUTE_REPAIR_INITIAL_DELAY_MS * (2 ** exponent),
    );
    const expiry = Date.parse(expiresAt);
    await this.state.storage.setAlarm(
      Number.isFinite(expiry) ? Math.min(Date.now() + delay, expiry) : Date.now() + delay,
    );
  }

  async scheduleExpiry(expiresAt) {
    if (typeof this.state.storage.setAlarm !== "function") return;
    const expiry = Date.parse(expiresAt);
    if (Number.isFinite(expiry)) await this.state.storage.setAlarm(expiry);
  }

  async alarm() {
    const receipt = await this.state.storage.get("receipt");
    if (!receipt) return;
    const expiry = Date.parse(receipt.expires_at);
    if (Number.isFinite(expiry) && expiry <= Date.now()) {
      if (typeof this.state.storage.deleteAll === "function") {
        await this.state.storage.deleteAll();
      }
      return;
    }
    if (!receipt.route_pending || !receipt.provider_message_id || !receipt.route) {
      await this.scheduleExpiry(receipt.expires_at);
      return;
    }
    try {
      await registerProviderRoute(
        this.env,
        receipt.provider_message_id,
        receipt.route,
      );
    } catch {
      const retry = await this.persistRouteRepairFailure(receipt);
      if (retry) {
        await this.scheduleRouteRepair(
          retry.route_attempts,
          retry.expires_at,
        );
      }
      return;
    }
    await this.persistAccepted(receipt, receipt.response, false);
  }
}

export class ProviderRoute {
  constructor(state) {
    this.state = state;
  }

  async fetch(request) {
    const url = new URL(request.url);
    if (request.method === "GET" && url.pathname === "/route") {
      const route = await this.state.storage.get("route");
      return route ? jsonResponse(route) : new Response("not found", { status: 404 });
    }
    if (request.method !== "POST" || url.pathname !== "/route") {
      return new Response("method not allowed", { status: 405 });
    }
    const route = await request.json();
    if (!validProviderRoute(route)) {
      return new Response("invalid route", { status: 400 });
    }
    const existing = await this.state.storage.get("route");
    if (existing && JSON.stringify(existing) !== JSON.stringify(route)) {
      return new Response("route conflict", { status: 409 });
    }
    if (!existing) {
      await this.state.storage.put("route", route);
      if (typeof this.state.storage.setAlarm === "function") {
        await this.state.storage.setAlarm(Date.now() + PROVIDER_ROUTE_TTL_MS);
      }
    }
    return new Response(null, { status: 204 });
  }

  async alarm() {
    if (typeof this.state.storage.deleteAll === "function") {
      await this.state.storage.deleteAll();
    }
  }
}

function validProviderRoute(route) {
  return route?.schema_version === "witself.agent-email-provider-route.v1" &&
    ID.test(route.send_id) && route.send_id.startsWith("esnd_") &&
    ID.test(route.account_id) && route.account_id.startsWith("acc_") &&
    ID.test(route.realm_id) && route.realm_id.startsWith("realm_") &&
    KEY_ID.test(route.signer_key_id) &&
    Object.keys(route).length === 5;
}

export async function registerProviderRoute(env, providerMessageId, route) {
  if (!env.PROVIDER_ROUTES || typeof providerMessageId !== "string" ||
      providerMessageId.length < 1 || providerMessageId.length > 512 ||
      !validProviderRoute(route)) {
    throw new Error("provider route is invalid");
  }
  const id = env.PROVIDER_ROUTES.idFromName(providerMessageId);
  const result = await env.PROVIDER_ROUTES.get(id).fetch("https://route.internal/route", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(route),
  });
  if (result.status !== 204) throw new Error("provider route was not stored");
}

export function durableObjectRequest(env, dispatch, digest, signerKeyId) {
  const id = env.RECEIPTS.idFromName(dispatch.send_id);
  const stub = env.RECEIPTS.get(id);
  return stub.fetch("https://receipt.internal/dispatch", {
    method: "POST",
    headers: {
      "Content-Type": "application/json",
      "X-Witself-Verified-Digest": digest,
      "X-Witself-Verified-Key-Id": signerKeyId,
    },
    body: JSON.stringify(dispatch),
  });
}

export function durableObjectReceiptReplay(
  env,
  dispatch,
  digest,
  signerKeyId,
) {
  const id = env.RECEIPTS.idFromName(dispatch.send_id);
  const stub = env.RECEIPTS.get(id);
  return stub.fetch("https://receipt.internal/receipt-replay", {
    method: "POST",
    headers: {
      "Content-Type": "application/json",
      "X-Witself-Verified-Digest": digest,
      "X-Witself-Verified-Key-Id": signerKeyId,
    },
    body: JSON.stringify(dispatch),
  });
}
