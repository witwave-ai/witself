export const SUPPORT_RECIPIENT = "support@witwave.ai";
export const SUPPORT_EMAIL_MAX_RAW_BYTES = 256 * 1024;
export const SUPPORT_EMAIL_MESSAGE_ID_MAX_CHARS = 998;

const TICKET_TAG = /tkt_[a-z2-7]{16}/;
const LOOP_SENDERS = new Set([
  "support@witwave.ai",
  "no-reply@witwave.ai",
]);
const LOOP_PRECEDENCE = new Set(["bulk", "junk", "auto_reply", "list"]);

function objectHeaderEntries(headers) {
  if (!headers || typeof headers !== "object") return [];
  if (typeof headers.entries === "function") {
    try {
      return [...headers.entries()];
    } catch {
      return [];
    }
  }
  return Object.entries(headers);
}

export function headerValue(headers, name) {
  const expected = String(name).toLowerCase();
  if (headers && typeof headers.get === "function") {
    try {
      const value = headers.get(name);
      return value === null || value === undefined ? null : String(value);
    } catch {
      return null;
    }
  }
  const values = [];
  for (const [key, raw] of objectHeaderEntries(headers)) {
    if (String(key).toLowerCase() !== expected) continue;
    if (Array.isArray(raw)) values.push(...raw.map(String));
    else if (raw !== null && raw !== undefined) values.push(String(raw));
  }
  return values.length === 0 ? null : values.join(", ");
}

function hasHeader(headers, name) {
  if (headers && typeof headers.has === "function") {
    try {
      return headers.has(name);
    } catch {
      return false;
    }
  }
  const expected = String(name).toLowerCase();
  return objectHeaderEntries(headers)
    .some(([key]) => String(key).toLowerCase() === expected);
}

function validLocalPart(value) {
  return value.length >= 1 && value.length <= 64 &&
    !value.startsWith(".") && !value.endsWith(".") &&
    !value.includes("..") &&
    /^[A-Za-z0-9!#$%&'*+/=?^_`{|}~.-]+$/.test(value);
}

function validDomain(value) {
  if (value.length < 1 || value.length > 253 || value.endsWith(".")) return false;
  return value.split(".").every((label) =>
    label.length >= 1 && label.length <= 63 &&
    !label.startsWith("-") && !label.endsWith("-") &&
    /^[A-Za-z0-9-]+$/.test(label));
}

// visibleSender accepts one conservative RFC 5322 addr-spec only. Display
// names, groups, comments, angle brackets, and multiple addresses fail closed.
export function visibleSender(headers) {
  const value = headerValue(headers, "From");
  if (value === null || value !== value.trim() || /[\s<>,:;()\[\]\\"]/.test(value)) {
    return null;
  }
  const at = value.lastIndexOf("@");
  if (at < 1 || at !== value.indexOf("@") ||
      !validLocalPart(value.slice(0, at)) || !validDomain(value.slice(at + 1))) {
    return null;
  }
  return value.toLowerCase();
}

export function messageIDFromHeaders(headers) {
  const value = headerValue(headers, "Message-ID");
  if (value === null) return null;
  const trimmed = value.trim();
  if (!trimmed || trimmed.length > SUPPORT_EMAIL_MESSAGE_ID_MAX_CHARS ||
      /[\r\n\0]/.test(trimmed)) return null;
  return trimmed;
}

export function extractTicketTag(subject) {
  if (typeof subject !== "string") return null;
  return TICKET_TAG.exec(subject)?.[0] ?? null;
}

function result(action, reason) {
  return Object.freeze({ action, reason });
}

// decideIntake is deliberately value-free: it decides only the intake action
// and reason. The caller separately reads the validated visible sender.
export function decideIntake({ headers, from, to, size, verdicts, config }) {
  // The gate comes before every other check, including the size reject: a
  // dark deployment must be indistinguishable from no worker at all, so no
  // reject, no distinction, and no side effect may escape while it is off.
  if (config?.SUPPORT_EMAIL_INTAKE_ENABLED !== "true") {
    return result("drop", "drop_gate");
  }
  if (typeof to !== "string" || to.toLowerCase() !== SUPPORT_RECIPIENT) {
    return result("drop", "drop_wrong_recipient");
  }
  if (!Number.isSafeInteger(size) || size < 1) {
    return result("drop", "drop_invalid_size");
  }
  if (size > SUPPORT_EMAIL_MAX_RAW_BYTES) {
    return result("reject_size", "reject_size");
  }

  const autoSubmitted = headerValue(headers, "Auto-Submitted");
  if (autoSubmitted !== null && autoSubmitted.trim().toLowerCase() !== "no") {
    return result("drop", "drop_auto_submitted");
  }
  const precedence = headerValue(headers, "Precedence");
  if (precedence !== null && precedence.toLowerCase().split(/[\s,]+/)
    .some((value) => LOOP_PRECEDENCE.has(value))) {
    return result("drop", "drop_precedence");
  }
  if (hasHeader(headers, "List-Id")) {
    return result("drop", "drop_list_id");
  }
  if (typeof from !== "string" || !from.trim() || from.trim() === "<>") {
    return result("drop", "drop_empty_envelope_sender");
  }

  const sender = visibleSender(headers);
  if (sender === null) return result("drop", "drop_invalid_from");
  if (LOOP_SENDERS.has(sender)) return result("drop", "drop_loop_sender");
  if (typeof config?.SUPPORT_EMAIL_AUTH_RESULTS_AUTHSERV_ID !== "string" ||
      config.SUPPORT_EMAIL_AUTH_RESULTS_AUTHSERV_ID === "") {
    return result("drop", "drop_authserv_id");
  }
  if (verdicts?.dmarc !== "pass") return result("drop", "drop_dmarc");
  return result("forward", "forward");
}

