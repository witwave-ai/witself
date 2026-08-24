const REQUIRED_FIELDS = Object.freeze([
  "body",
  "message_id",
  "sender",
  "subject",
]);
const OPTIONAL_FIELDS = Object.freeze(["ticket_tag"]);
const TICKET_TAG = /^tkt_[a-z2-7]{16}$/;
const MAX_SUBJECT_CHARACTERS = 200;
const MAX_BODY_BYTES = 65_536;
const NON_WHITESPACE = /\P{White_Space}/u;

function invalidPayload() {
  throw new TypeError("support email intake payload is invalid");
}

// validateIntakePayload owns the exact untrusted bridge-to-control-plane wire
// shape. Returning a fresh object prevents an accepted payload from retaining
// an unexpected prototype or acquiring extra fields downstream.
export function validateIntakePayload(value) {
  if (value === null || typeof value !== "object" || Array.isArray(value)) {
    invalidPayload();
  }
  const keys = Object.keys(value);
  if (keys.length < REQUIRED_FIELDS.length ||
      keys.length > REQUIRED_FIELDS.length + OPTIONAL_FIELDS.length ||
      REQUIRED_FIELDS.some((key) => !Object.hasOwn(value, key)) ||
      keys.some((key) =>
        !REQUIRED_FIELDS.includes(key) && !OPTIONAL_FIELDS.includes(key))) {
    invalidPayload();
  }
  if (typeof value.sender !== "string" || value.sender.length === 0 ||
      typeof value.subject !== "string" || !NON_WHITESPACE.test(value.subject) ||
      typeof value.body !== "string" || !NON_WHITESPACE.test(value.body) ||
      typeof value.message_id !== "string" || value.message_id.length === 0 ||
      [...value.subject].length > MAX_SUBJECT_CHARACTERS ||
      new TextEncoder().encode(value.body).byteLength > MAX_BODY_BYTES ||
      (Object.hasOwn(value, "ticket_tag") &&
       (typeof value.ticket_tag !== "string" ||
        !TICKET_TAG.test(value.ticket_tag)))) {
    invalidPayload();
  }
  return {
    sender: value.sender,
    subject: value.subject,
    body: value.body,
    message_id: value.message_id,
    ...(Object.hasOwn(value, "ticket_tag")
      ? { ticket_tag: value.ticket_tag }
      : {}),
  };
}

// dedupKey hashes the untrusted Message-ID before it reaches KV. Besides
// bounding the key, this keeps an email identifier out of the value-free
// control-plane operational surface.
export async function dedupKey(messageID) {
  if (typeof messageID !== "string" || messageID.length === 0) {
    throw new TypeError("support email message id is invalid");
  }
  const digest = await crypto.subtle.digest(
    "SHA-256",
    new TextEncoder().encode(messageID),
  );
  const hex = [...new Uint8Array(digest)]
    .map((byte) => byte.toString(16).padStart(2, "0"))
    .join("");
  return `intake_dedup:${hex}`;
}

// decideDisposition considers active matches only. Multiple reports of the
// same account remain ambiguous: duplicate ownership projections are a state
// that must be repaired, not silently collapsed at the intake boundary.
export function decideDisposition(matches) {
  if (!Array.isArray(matches)) {
    throw new TypeError("support contact matches are invalid");
  }
  const activeCount = matches.filter((match) =>
    match !== null && typeof match === "object" &&
    match.status === "active").length;
  if (activeCount === 1) return "proceed";
  if (activeCount === 0) return "drop_unmatched";
  return "drop_ambiguous";
}
