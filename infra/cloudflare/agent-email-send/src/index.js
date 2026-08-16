import {
  DISPATCH_SCHEMA,
  RECEIPT_PROOF_SCHEMA,
  RECEIPT_REPLAY_AUDIENCE,
  RESPONSE_SCHEMA,
  verifyDispatchRequest,
} from "./signature.mjs";
import {
  OutboundReceipt,
  ProviderRoute,
  durableObjectReceiptReplay,
  durableObjectRequest,
  receiptProofFailure,
  validateDispatch,
} from "./dispatch.mjs";
import { consumeProviderEvents } from "./events.mjs";

// A valid text field may contain 256 KiB of single-byte control characters.
// JSON encodes each of those as a six-byte \u00xx escape, so the signed
// envelope needs substantially more room than the decoded text contract.
// Validation below still enforces the independent 256 KiB text limit.
export const MAX_BODY_BYTES = 2 * 1024 * 1024;

export function failure(
  status,
  code,
  sendId = "esnd_invalid",
  state = "rejected",
  retryAfterSeconds = 0,
) {
  const body = {
    schema_version: RESPONSE_SCHEMA,
    send_id: sendId,
    state,
    provider: "cloudflare_email_sending",
    error_code: code,
  };
  if (state === "retryable" && retryAfterSeconds > 0) {
    body.retry_after_seconds = Math.min(86400, retryAfterSeconds);
  }
  return new Response(
    JSON.stringify(body),
    {
      status,
      headers: {
        "Content-Type": "application/json",
        "Cache-Control": "private, no-store",
      },
    },
  );
}

export { OutboundReceipt, ProviderRoute };

async function readBoundedBody(request) {
  if (!request.body) return new Uint8Array();
  const reader = request.body.getReader();
  const chunks = [];
  let total = 0;
  try {
    while (true) {
      const { done, value } = await reader.read();
      if (done) break;
      const chunk = value instanceof Uint8Array ? value : new Uint8Array(value);
      total += chunk.byteLength;
      if (total > MAX_BODY_BYTES) {
        try {
          await reader.cancel();
        } catch {
          // The size refusal is authoritative even if peer cancellation fails.
        }
        return null;
      }
      chunks.push(chunk);
    }
  } finally {
    try {
      reader.releaseLock();
    } catch {
      // A canceled stream may already have released its reader.
    }
  }
  const body = new Uint8Array(total);
  let offset = 0;
  for (const chunk of chunks) {
    body.set(chunk, offset);
    offset += chunk.byteLength;
  }
  return body;
}

export default {
  async fetch(request, env) {
    const url = new URL(request.url);
    const isDispatch = request.method === "POST" &&
      url.pathname === "/v1/dispatch";
    const isReceiptReplay = request.method === "POST" &&
      url.pathname === "/v1/dispatch:receipt-replay";
    if (!isDispatch && !isReceiptReplay) {
      return new Response("not found", { status: 404 });
    }
    const endpointFailure = (
      status,
      code,
      sendId = "esnd_invalid",
      state = "rejected",
      retryAfterSeconds = 0,
    ) => isReceiptReplay
      ? receiptProofFailure(status, "receipt_unresolved", sendId)
      : failure(status, code, sendId, state, retryAfterSeconds);
    const declaredHeader = request.headers.get("Content-Length");
    let declaredLength = null;
    if (declaredHeader !== null) {
      if (!/^(?:0|[1-9][0-9]*)$/.test(declaredHeader)) {
        return endpointFailure(400, "request_invalid");
      }
      declaredLength = Number(declaredHeader);
      if (!Number.isSafeInteger(declaredLength)) {
        return endpointFailure(400, "request_invalid");
      }
      if (declaredLength > MAX_BODY_BYTES) {
        return endpointFailure(413, "request_too_large");
      }
    }
    const body = await readBoundedBody(request);
    if (body === null || body.length < 2) {
      return endpointFailure(413, "request_too_large");
    }
    if (declaredLength !== null && declaredLength !== body.length) {
      return endpointFailure(400, "request_invalid");
    }
    const dispatchAudience = String(env.DISPATCH_AUDIENCE ?? "");
    const receiptReplayAudience = String(env.RECEIPT_REPLAY_AUDIENCE ?? "");
    if (
      dispatchAudience.length < 1 ||
      receiptReplayAudience !== RECEIPT_REPLAY_AUDIENCE ||
      dispatchAudience === receiptReplayAudience
    ) {
      return endpointFailure(401, "signature_invalid");
    }
    let verified;
    try {
      verified = await verifyDispatchRequest(request, body, env, {
        expectedAudience: isReceiptReplay
          ? receiptReplayAudience
          : dispatchAudience,
      });
    } catch {
      return endpointFailure(401, "signature_invalid");
    }
    let dispatch;
    try {
      dispatch = validateDispatch(JSON.parse(new TextDecoder().decode(body)), env);
    } catch {
      return endpointFailure(400, "request_invalid");
    }
    if (!verified.signer.accountIds.has(dispatch.account_id)) {
      return endpointFailure(403, "account_not_allowed", dispatch.send_id);
    }
    if (isReceiptReplay) {
      if (String(env.RECEIPT_REPLAY_ENABLED ?? "false") !== "true") {
        return endpointFailure(503, "receipt_replay_unavailable", dispatch.send_id);
      }
      return durableObjectReceiptReplay(
        env,
        dispatch,
        verified.digest,
        verified.keyId,
      );
    }
    if (String(env.DISPATCH_ENABLED ?? "false") !== "true") {
      return failure(
        503,
        "provider_unavailable",
        dispatch.send_id,
        "retryable",
        60,
      );
    }
    return durableObjectRequest(env, dispatch, verified.digest, verified.keyId);
  },
  async queue(batch, env) {
    await consumeProviderEvents(batch, env);
  },
};

export { DISPATCH_SCHEMA, RECEIPT_PROOF_SCHEMA, RECEIPT_REPLAY_AUDIENCE };
