const DNS_JSON_ENDPOINT = "https://cloudflare-dns.com/dns-query";
const DNS_NAME_PATTERN =
  /^_witself-verification\.[a-z0-9](?:[a-z0-9.-]{1,229}[a-z0-9])?$/;
const MAX_DNS_JSON_BYTES = 64 * 1024;
const DNS_TIMEOUT_MS = 5_000;
const TXT_RECORD_TYPE = 16;
const SHA256_PATTERN = /^[0-9a-f]{64}$/;

const TEMPORARY_OBSERVATION_CODES = new Set([
  "dns_lookup_inconclusive",
  "dns_response_too_large",
  "dns_resolver_unavailable",
]);

export class AgentEmailDomainVerificationError extends Error {
  constructor(message, code, temporary = false) {
    super(message);
    this.name = "AgentEmailDomainVerificationError";
    this.code = code;
    this.temporary = temporary;
  }
}

function verificationFail(message, code, temporary = false) {
  throw new AgentEmailDomainVerificationError(message, code, temporary);
}

async function sha256Hex(value) {
  const bytes = new TextEncoder().encode(value);
  const digest = await crypto.subtle.digest("SHA-256", bytes);
  return [...new Uint8Array(digest)]
    .map((byte) => byte.toString(16).padStart(2, "0"))
    .join("");
}

/**
 * Decode the presentation form returned by Cloudflare's DNS JSON endpoint.
 * TXT RDATA may contain several quoted character-strings. Joining them is the
 * DNS wire-format meaning and avoids rejecting a correctly chunked challenge.
 */
export function parseAgentEmailDomainTXT(value) {
  if (typeof value !== "string" || value.length < 2 || value.length > 8_192) {
    verificationFail("DNS TXT answer is invalid", "dns_answer_invalid");
  }
  let position = 0;
  let result = "";
  let strings = 0;
  while (position < value.length) {
    while (/\s/.test(value[position] ?? "")) position += 1;
    if (position >= value.length) break;
    if (value[position] !== '"') {
      verificationFail("DNS TXT answer is invalid", "dns_answer_invalid");
    }
    position += 1;
    strings += 1;
    let closed = false;
    while (position < value.length) {
      const character = value[position++];
      if (character === '"') {
        closed = true;
        break;
      }
      if (character !== "\\") {
        result += character;
        continue;
      }
      if (position >= value.length) {
        verificationFail("DNS TXT answer is invalid", "dns_answer_invalid");
      }
      const decimal = value.slice(position, position + 3);
      if (/^[0-9]{3}$/.test(decimal)) {
        const byte = Number(decimal);
        if (byte > 255) {
          verificationFail("DNS TXT answer is invalid", "dns_answer_invalid");
        }
        result += String.fromCharCode(byte);
        position += 3;
      } else {
        result += value[position++];
      }
    }
    if (!closed) {
      verificationFail("DNS TXT answer is invalid", "dns_answer_invalid");
    }
  }
  if (strings === 0 || result.length > 4_096) {
    verificationFail("DNS TXT answer is invalid", "dns_answer_invalid");
  }
  return result;
}

function canonicalAnswerName(value) {
  if (typeof value !== "string") return null;
  return value.toLowerCase().replace(/\.$/, "");
}

function timeoutSignal() {
  return typeof AbortSignal?.timeout === "function"
    ? AbortSignal.timeout(DNS_TIMEOUT_MS)
    : undefined;
}

/**
 * Resolve one fixed TXT owner through a fixed HTTPS endpoint. The customer
 * controls only the validated DNS name; it cannot choose a URL or redirect
 * target. Resolver outages are distinguished from authoritative absence so a
 * third-party failure cannot revoke otherwise-current ownership.
 */
export async function resolveAgentEmailDomainTXT(
  recordName,
  fetchImpl = (...args) => globalThis.fetch(...args),
) {
  const normalizedName = String(recordName ?? "").trim().toLowerCase();
  if (!DNS_NAME_PATTERN.test(normalizedName) || normalizedName.includes("..")) {
    verificationFail("DNS TXT owner is invalid", "dns_request_invalid");
  }
  const url = new URL(DNS_JSON_ENDPOINT);
  url.searchParams.set("name", normalizedName);
  url.searchParams.set("type", "TXT");
  let response;
  try {
    response = await fetchImpl(url.toString(), {
      method: "GET",
      headers: { Accept: "application/dns-json" },
      redirect: "error",
      signal: timeoutSignal(),
    });
  } catch {
    verificationFail(
      "DNS ownership resolver is temporarily unavailable",
      "dns_resolver_unavailable",
      true,
    );
  }
  if (!response?.ok) {
    verificationFail(
      "DNS ownership resolver is temporarily unavailable",
      "dns_resolver_unavailable",
      true,
    );
  }
  let text;
  try {
    text = await response.text();
  } catch {
    verificationFail(
      "DNS ownership resolver returned an unreadable response",
      "dns_resolver_unavailable",
      true,
    );
  }
  if (new TextEncoder().encode(text).byteLength > MAX_DNS_JSON_BYTES) {
    verificationFail(
      "DNS ownership resolver response is too large",
      "dns_response_too_large",
      true,
    );
  }
  let payload;
  try {
    payload = JSON.parse(text);
  } catch {
    verificationFail(
      "DNS ownership resolver returned invalid JSON",
      "dns_resolver_unavailable",
      true,
    );
  }
  const status = payload?.Status;
  const question = Array.isArray(payload?.Question) &&
      payload.Question.length === 1
    ? payload.Question[0]
    : null;
  if (!Number.isSafeInteger(status) || ![0, 3].includes(status) ||
      payload?.TC === true || question?.type !== TXT_RECORD_TYPE ||
      canonicalAnswerName(question?.name) !== normalizedName ||
      (payload?.Answer !== undefined && !Array.isArray(payload.Answer))) {
    verificationFail(
      "DNS ownership lookup did not complete authoritatively",
      "dns_lookup_inconclusive",
      true,
    );
  }
  const answers = [];
  let minimumTTL = null;
  for (const answer of Array.isArray(payload?.Answer) ? payload.Answer : []) {
    if (answer?.type !== TXT_RECORD_TYPE ||
        canonicalAnswerName(answer?.name) !== normalizedName ||
        typeof answer?.data !== "string") {
      continue;
    }
    let parsed;
    try {
      parsed = parseAgentEmailDomainTXT(answer.data);
    } catch (error) {
      if (error instanceof AgentEmailDomainVerificationError) continue;
      throw error;
    }
    answers.push(parsed);
    if (Number.isSafeInteger(answer.TTL) && answer.TTL >= 0) {
      minimumTTL = minimumTTL === null
        ? answer.TTL
        : Math.min(minimumTTL, answer.TTL);
    }
  }
  // An NXDOMAIN response has no effective RRset even if a contradictory or
  // compromised JSON resolver payload includes Answer records.
  const canonicalAnswers = status === 0
    ? [...new Set(answers)].sort()
    : [];
  return {
    resolver: "cloudflare-dns-json",
    response_code: status,
    authoritative_absence: status === 3 || canonicalAnswers.length === 0,
    dnssec_authenticated: payload?.AD === true,
    answers: canonicalAnswers,
    minimum_ttl_seconds: status === 0 ? minimumTTL : null,
    rrset_sha256: await sha256Hex(JSON.stringify(canonicalAnswers)),
  };
}

export function agentEmailDomainTXTMatches(result, expectedValue) {
  return result?.authoritative_absence !== true &&
    Array.isArray(result?.answers) &&
    typeof expectedValue === "string" &&
    result.answers.includes(expectedValue);
}

/**
 * Reduce an untrusted DNS response to the bounded evidence the authority needs.
 * Raw TXT values never cross the durable observation boundary.
 */
export function agentEmailDomainResolvedObservation(result, expectedValue) {
  if (!result || !Array.isArray(result.answers) ||
      typeof result.authoritative_absence !== "boolean" ||
      typeof result.dnssec_authenticated !== "boolean" ||
      !SHA256_PATTERN.test(result.rrset_sha256 ?? "") ||
      !(result.minimum_ttl_seconds === null ||
        (Number.isSafeInteger(result.minimum_ttl_seconds) &&
          result.minimum_ttl_seconds >= 0))) {
    verificationFail(
      "DNS ownership observation is invalid",
      "dns_observation_invalid",
    );
  }
  return {
    kind: "resolved",
    matched: agentEmailDomainTXTMatches(result, expectedValue),
    authoritative_absence: result.authoritative_absence,
    dnssec_authenticated: result.dnssec_authenticated,
    minimum_ttl_seconds: result.minimum_ttl_seconds,
    rrset_sha256: result.rrset_sha256,
  };
}

export function agentEmailDomainTemporaryObservation(error) {
  if (!(error instanceof AgentEmailDomainVerificationError) ||
      error.temporary !== true ||
      !TEMPORARY_OBSERVATION_CODES.has(error.code)) {
    throw error;
  }
  return { kind: "temporary_error", code: error.code };
}
