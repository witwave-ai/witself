// The legal authority is a service binding, never a caller-selected URL. All
// persisted protocol records are bounded and contain no challenge or IP value.
export const LEGAL_PHASES = new Set([
  "legal_pending", "legal_rejected", "legal_reserved",
]);
const LABEL = /^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$/;
const ID = /^[A-Za-z0-9_-]{1,128}$/;
const HASH = /^[0-9a-f]{64}$/;
const MAX_BYTES = 64 << 10;
const object = (v) => v !== null && typeof v === "object" && !Array.isArray(v);
const exact = (v, keys) => object(v) && Object.keys(v).length === keys.length &&
  keys.every((key) => Object.hasOwn(v, key));
export const sameLegalValue = (a, b) => {
  if (a === b) return true;
  if (!object(a) || !object(b)) return false;
  const keys = Object.keys(a);
  return keys.length === Object.keys(b).length &&
    keys.every((key) => Object.hasOwn(b, key) && sameLegalValue(a[key], b[key]));
};

// JSON.parse alone accepts duplicate (including escaped-alias) keys. Scan the
// already syntax-checked text, with a depth limit, before accepting any object.
export function parseLegalJSON(text) {
  const parsed = JSON.parse(text);
  let pos = 0;
  const space = () => { while (/\s/.test(text[pos] ?? "") && pos < text.length) pos++; };
  const string = () => {
    const start = pos++;
    while (pos < text.length) {
      if (text[pos] === "\\") { pos += 2; continue; }
      if (text[pos++] === '"') {
        const value = JSON.parse(text.slice(start, pos));
        if ([...value].some((c) => c.codePointAt(0) >= 0xd800 && c.codePointAt(0) <= 0xdfff)) throw new Error("invalid JSON");
        return value;
      }
    }
    throw new Error("invalid JSON");
  };
  const visit = (depth) => {
    if (depth > 16) throw new Error("invalid JSON");
    space();
    if (text[pos] === '{') {
      pos++; space();
      const keys = new Set();
      if (text[pos] === '}') { pos++; return; }
      for (;;) {
        space(); const key = string();
        if (keys.has(key)) throw new Error("invalid JSON");
        keys.add(key); space(); pos++; visit(depth + 1); space();
        if (text[pos++] === '}') return;
      }
    }
    if (text[pos] === '[') {
      pos++; space();
      if (text[pos] === ']') { pos++; return; }
      for (;;) { visit(depth + 1); space(); if (text[pos++] === ']') return; }
    }
    if (text[pos] === '"') { string(); return; }
    while (pos < text.length && !/[\s,}\]]/.test(text[pos])) pos++;
  };
  visit(0);
  return parsed;
}

export function awaitLegalAbort(promise, signal) {
  if (!signal) return promise;
  return new Promise((resolve, reject) => {
    const abort = () => reject(new Error("legal response unavailable"));
    if (signal.aborted) { promise.catch(() => {}); abort(); return; }
    signal.addEventListener("abort", abort, { once: true });
    promise.then(resolve, reject).finally(() => signal.removeEventListener("abort", abort));
  });
}

export async function readLegalJSON(response, signal) {
  const controller = new AbortController();
  const abort = () => controller.abort();
  signal?.addEventListener("abort", abort, { once: true });
  if (signal?.aborted) abort();
  const timeout = setTimeout(abort, 15000);
  try { return await readLegalBody(response, controller.signal); }
  finally { clearTimeout(timeout); signal?.removeEventListener("abort", abort); }
}

async function readLegalBody(response, signal) {
  if (!response.body) throw new Error("legal response unavailable");
  const reader = response.body.getReader();
  const chunks = [];
  let size = 0;
  const aborted = () => { reader.cancel().catch(() => {}); };
  signal?.addEventListener("abort", aborted, { once: true });
  try {
    if (signal?.aborted) throw new Error("legal response unavailable");
    for (;;) {
      const { value, done } = await awaitLegalAbort(reader.read(), signal);
      if (signal?.aborted) throw new Error("legal response unavailable");
      if (done) break;
      size += value.byteLength;
      if (size > MAX_BYTES) throw new Error("legal response unavailable");
      chunks.push(value);
    }
    const bytes = new Uint8Array(size);
    let offset = 0;
    for (const chunk of chunks) { bytes.set(chunk, offset); offset += chunk.byteLength; }
    const text = new TextDecoder("utf-8", { fatal: true }).decode(bytes);
    return { value: parseLegalJSON(text), bytes };
  } finally {
    signal?.removeEventListener("abort", aborted);
    reader.cancel().catch(() => {});
    reader.releaseLock();
  }
}

export async function readCanonicalLegal(env, signal) {
  const controller = new AbortController();
  const abort = () => controller.abort();
  signal?.addEventListener("abort", abort, { once: true });
  const timeout = setTimeout(abort, 15000);
  try {
    if (signal?.aborted || typeof env.LEGAL_DOCUMENTS?.fetch !== "function") throw new Error();
    const response = await awaitLegalAbort(env.LEGAL_DOCUMENTS.fetch(new Request(
      "https://legal.internal/legal/versions.json",
      { redirect: "error", signal: controller.signal },
    )), controller.signal);
    if (response.status !== 200 || response.redirected) throw new Error();
    const { value, bytes } = await readLegalJSON(response, controller.signal);
    for (const slug of ["terms", "privacy"]) {
      if (!object(value?.[slug]) || typeof value[slug].version !== "string" || !LABEL.test(value[slug].version) ||
          value[slug].path !== `/legal/${slug}`) throw new Error();
    }
    const digest = await crypto.subtle.digest("SHA-256", bytes);
    if (controller.signal.aborted) throw new Error();
    return {
      terms: value.terms.version, privacy: value.privacy.version,
      manifest_sha256: Array.from(new Uint8Array(digest), (b) => b.toString(16).padStart(2, "0")).join(""),
    };
  } catch {
    throw new Error("signup legal authority is unavailable");
  } finally {
    clearTimeout(timeout);
    signal?.removeEventListener("abort", abort);
  }
}

const REFUSAL_KEYS = ["schema_version", "code", "error", "provision_id", "request_fingerprint",
  "consent_terms_version", "consent_privacy_version", "refusal_id", "refusal_revision",
  "required_terms_version", "required_privacy_version"];
export function validRefusal(v) {
  return exact(v, REFUSAL_KEYS) && v.schema_version === "witself.signup-legal-refusal.v1" &&
    v.code === "signup_legal_stale" && v.error === "signup legal acceptance is out of date" &&
    typeof v.provision_id === "string" && ID.test(v.provision_id) &&
    typeof v.request_fingerprint === "string" && HASH.test(v.request_fingerprint) &&
    typeof v.refusal_id === "string" && ID.test(v.refusal_id) &&
    Number.isSafeInteger(v.refusal_revision) && v.refusal_revision > 0 &&
    [v.consent_terms_version, v.consent_privacy_version, v.required_terms_version,
      v.required_privacy_version].every((s) => typeof s === "string" && LABEL.test(s));
}
export function validCandidate(v) {
  return exact(v, ["provision_id", "consent_terms_version", "consent_privacy_version"]) &&
    typeof v.provision_id === "string" && ID.test(v.provision_id) &&
    [v.consent_terms_version, v.consent_privacy_version].every((s) => typeof s === "string" && LABEL.test(s));
}
export function validReconsent(v) {
  return exact(v, ["schema_version", "refusal", "transition_id", "candidate", "email", "invite", "display_name"]) &&
    v.schema_version === "witself.signup-reconsent.v1" && validRefusal(v.refusal) &&
    typeof v.transition_id === "string" && ID.test(v.transition_id) && validCandidate(v.candidate) &&
    v.candidate.provision_id !== v.refusal.provision_id &&
    typeof v.email === "string" && typeof v.invite === "string" && typeof v.display_name === "string";
}
// encoding/json in the CLI escapes HTML and JavaScript line separators.
// Budget only the invariant normalized core with those same wire bytes, so
// every allowed ID/version pair still fits a bounded successor request.
export function legalCoreByteLength(v) {
  const core = { email: v.email, display_name: v.display_name, invite: v.invite };
  if (Object.values(core).some((value) => typeof value !== "string" ||
      [...value].some((c) => c.codePointAt(0) >= 0xd800 && c.codePointAt(0) <= 0xdfff))) return Infinity;
  const escaped = JSON.stringify(core).replace(/[<>&\u2028\u2029]/g,
    (c) => "\\u" + c.charCodeAt(0).toString(16).padStart(4, "0"));
  return new TextEncoder().encode(escaped).byteLength;
}
export function validLegalCore(v) {
  return object(v) && legalCoreByteLength(v) <= 32 * 1024 && exact(v, ["provision_id", "email", "invite", "display_name", "consent_terms_version", "consent_privacy_version"]) &&
    validCandidate({ provision_id: v.provision_id, consent_terms_version: v.consent_terms_version,
      consent_privacy_version: v.consent_privacy_version }) &&
    typeof v.email === "string" && v.email.includes("@") && v.email === v.email.trim().toLowerCase() &&
    typeof v.display_name === "string" && v.display_name.length <= 200 && v.display_name === v.display_name.trim() &&
    typeof v.invite === "string" && (v.invite === "" || /^[a-z0-9][a-z0-9-]{2,63}$/.test(v.invite));
}
export function legalCore(request) {
  const { provision_id, email, invite, display_name, consent_terms_version, consent_privacy_version } = request;
  return { provision_id, email, invite, display_name, consent_terms_version, consent_privacy_version };
}
export function allowedCounterReceipt(verdict, limit) {
  const receipt = { allowed: verdict?.allowed, count: verdict?.count, limit: verdict?.limit, day: verdict?.day };
  return validAllowedCounter(receipt, limit) ? receipt : null;
}
function validAllowedCounter(v, limit) {
  if (limit === 0) return v === null;
  return exact(v, ["allowed", "count", "limit", "day"]) && v.allowed === true &&
    v.limit === limit && Number.isSafeInteger(v.count) && v.count > 0 && v.count <= limit &&
    typeof v.day === "string" && /^\d{4}-\d{2}-\d{2}$/.test(v.day) &&
    Number.isFinite(Date.parse(v.day + "T00:00:00Z"));
}
export function validAbuseReceipt(v) {
  return exact(v, ["root_provision_id", "signup_ip_scope", "turnstile", "per_ip_limit", "global_limit",
    "per_ip", "global"]) && typeof v.root_provision_id === "string" && ID.test(v.root_provision_id) &&
    typeof v.turnstile === "boolean" && [v.per_ip_limit, v.global_limit].every((n) => Number.isSafeInteger(n) && n >= 0) &&
    validAllowedCounter(v.per_ip, v.per_ip_limit) && validAllowedCounter(v.global, v.global_limit) &&
    (v.per_ip_limit > 0 ? typeof v.signup_ip_scope === "string" && /^signup-counter:ip:[0-9a-f]{64}$/.test(v.signup_ip_scope) : v.signup_ip_scope === null);
}
export function legalCoreCanonical(request) {
  return JSON.stringify(["signup-core/v1", request.email, request.display_name, request.invite]);
}
export function validLegalState(v) {
  const common = ["schema_version", "revision", "phase", "provision_id", "request_fingerprint", "cell", "account",
    "created_at", "email_attempted", "verification_email_sent", "legal_core_fingerprint", "legal_terms_version",
    "legal_privacy_version", "legal_open_signup", "legal_abuse"];
  if (object(v) && Object.hasOwn(v, "turnstile_verified")) common.push("turnstile_verified");
  if (object(v) && Object.hasOwn(v, "legal_reservation")) common.push("legal_reservation");
  if (v?.phase === "legal_rejected") {
    common.push("legal_refusal", "legal_manifest_sha256");
    if (Object.hasOwn(v, "legal_successor")) common.push("legal_successor");
  }
  if (!exact(v, common) || (Object.hasOwn(v, "turnstile_verified") && v.turnstile_verified !== true)) return false;
  if (v.schema_version !== "witself.signup.v1" || !LEGAL_PHASES.has(v.phase) ||
      !Number.isSafeInteger(v.revision) || v.revision < 0 ||
      typeof v.provision_id !== "string" || !ID.test(v.provision_id) ||
      typeof v.request_fingerprint !== "string" || !HASH.test(v.request_fingerprint) ||
      typeof v.legal_core_fingerprint !== "string" || !HASH.test(v.legal_core_fingerprint) ||
      ![v.legal_terms_version, v.legal_privacy_version].every((s) => typeof s === "string" && LABEL.test(s)) ||
      typeof v.legal_open_signup !== "boolean" || !validAbuseReceipt(v.legal_abuse) ||
      v.cell !== null || v.account !== null || v.email_attempted !== false || v.verification_email_sent !== false ||
      (v.turnstile_verified === true) !== v.legal_abuse.turnstile ||
      typeof v.created_at !== "string" || !Number.isFinite(Date.parse(v.created_at))) return false;
  if (v.phase === "legal_rejected") {
    if (!validRefusal(v.legal_refusal) || v.legal_refusal.provision_id !== v.provision_id ||
        v.legal_refusal.request_fingerprint !== v.request_fingerprint ||
        v.legal_refusal.consent_terms_version !== v.legal_terms_version ||
        v.legal_refusal.consent_privacy_version !== v.legal_privacy_version ||
        v.legal_refusal.refusal_revision > v.revision || typeof v.legal_manifest_sha256 !== "string" ||
        !HASH.test(v.legal_manifest_sha256)) return false;
    if (v.legal_successor !== undefined && (!validRegistration(v.legal_successor) ||
        !sameLegalValue(v.legal_successor.refusal, v.legal_refusal) ||
        !sameLegalValue(v.legal_successor.abuse, v.legal_abuse) ||
        v.legal_successor.core_fingerprint !== v.legal_core_fingerprint ||
        v.legal_successor.open_signup !== v.legal_open_signup)) return false;
  }
  if ((v.phase === "legal_reserved" || v.legal_reservation !== undefined) && (!validRegistration(v.legal_reservation) ||
      v.legal_reservation.request_fingerprint !== v.request_fingerprint ||
      v.legal_reservation.core_fingerprint !== v.legal_core_fingerprint ||
      v.legal_reservation.open_signup !== v.legal_open_signup ||
      !sameLegalValue(v.legal_reservation.candidate, { provision_id: v.provision_id,
        consent_terms_version: v.legal_terms_version, consent_privacy_version: v.legal_privacy_version }) ||
      !sameLegalValue(v.legal_reservation.abuse, v.legal_abuse))) return false;
  return true;
}
export function validRegistration(v) {
  return exact(v, ["refusal", "transition_id", "candidate", "request_fingerprint", "core_fingerprint",
    "open_signup", "abuse", "registered"]) && validRefusal(v.refusal) &&
    typeof v.transition_id === "string" && ID.test(v.transition_id) && validCandidate(v.candidate) &&
    v.candidate.provision_id !== v.refusal.provision_id && typeof v.open_signup === "boolean" &&
    typeof v.request_fingerprint === "string" && HASH.test(v.request_fingerprint) &&
    typeof v.core_fingerprint === "string" && HASH.test(v.core_fingerprint) &&
    validAbuseReceipt(v.abuse) && typeof v.registered === "boolean";
}
export function registrationAck(registration) {
  return { schema_version: "witself.signup-reconsent-ack.v1", status: "registered",
    refusal: registration.refusal, transition_id: registration.transition_id,
    candidate: registration.candidate, candidate_request_fingerprint: registration.request_fingerprint };
}
