// Admin-handle rules, extracted so they are testable under plain node
// (index.js itself imports workerd-only packages and cannot be imported by
// tests). index.js consumes these; keep the two in lockstep.

// Mirrors the store's adminHandleRE and server.validateAdminHandle — drift
// across the tiers is a cross-tier integration bug.
export const ADMIN_HANDLE = /^[a-z][a-z0-9_-]{1,31}$/;

// Handles that would collide with an existing actor_kind or role name
// (owner / operator / control_plane / system). Reserved so a rogue mint
// can't forge audit rows that read like a system-emitted event.
export const RESERVED_HANDLES = new Set([
  "system",
  "control_plane",
  "root",
  "admin",
  "fleet",
  "owner",
  "operator",
  // The AI support assistant's fixed posting identity (author_kind
  // 'assistant', author_id 'assistant' on support_ticket_messages). Reserved
  // so no human admin credential can mint a handle that posts replies
  // rendering as the assistant — the support policy's labeling promise
  // depends on this pair being unforgeable in both directions.
  "assistant",
]);

// validateMintHandle applies the shape gate then the reservation. Returns
// null when the handle may be minted, or the exact refusal message.
export function validateMintHandle(handle) {
  if (!ADMIN_HANDLE.test(handle)) {
    return "invalid handle (2-32 lowercase chars; must start with a letter; letters/digits/underscore/hyphen only)";
  }
  if (RESERVED_HANDLES.has(handle)) {
    return `handle "${handle}" is reserved`;
  }
  return null;
}
