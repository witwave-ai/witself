export const MANAGED_DELIVERY_ACCOUNT_ALLOWLIST_BINDING =
  "CP_AGENT_EMAIL_MANAGED_DELIVERY_ACCOUNT_ALLOWLIST";
export const MANAGED_DELIVERY_COHORT_SCHEMA =
  "witself.agent-email-managed-delivery-cohort.v1";

const ACCOUNT_ID = /^acc_[a-z2-7]{16}$/;
const MAX_ACCOUNTS = 100;
const ACCOUNT_ID_BYTES = 20;
const MAX_BYTES = MAX_ACCOUNTS * ACCOUNT_ID_BYTES + (MAX_ACCOUNTS - 1);

// The rollout cohort is deliberately a canonical, exact CSV rather than a
// pattern language. An empty value means no account is admitted. Refusing
// whitespace, duplicates, reordering, and wildcard-like values makes the same
// bytes safe to compare across the control plane and email edge during an
// activation review.
export function parseManagedDeliveryAccountAllowlist(value = "") {
  if (typeof value !== "string" || value.length > MAX_BYTES) {
    throw new TypeError("managed email delivery account allowlist is invalid");
  }
  if (value === "") return Object.freeze([]);
  if (value !== value.trim() || /\s/.test(value)) {
    throw new TypeError("managed email delivery account allowlist is invalid");
  }
  const accounts = value.split(",");
  if (accounts.length > MAX_ACCOUNTS ||
      accounts.some((accountID) => !ACCOUNT_ID.test(accountID)) ||
      new Set(accounts).size !== accounts.length) {
    throw new TypeError("managed email delivery account allowlist is invalid");
  }
  const canonical = [...accounts].sort();
  if (canonical.join(",") !== value) {
    throw new TypeError("managed email delivery account allowlist is invalid");
  }
  return Object.freeze(canonical);
}

export function managedDeliveryAccountAllowlist(env = {}) {
  return parseManagedDeliveryAccountAllowlist(String(
    env[MANAGED_DELIVERY_ACCOUNT_ALLOWLIST_BINDING] ?? "",
  ));
}

export function managedDeliveryAccountIsAdmitted(env, accountID) {
  if (!ACCOUNT_ID.test(String(accountID ?? ""))) {
    throw new TypeError("managed email delivery account is invalid");
  }
  return managedDeliveryAccountAllowlist(env).includes(accountID);
}

function hex(bytes) {
  return [...new Uint8Array(bytes)]
    .map((byte) => byte.toString(16).padStart(2, "0"))
    .join("");
}

export async function managedDeliveryCohortSummary(env = {}, cryptoAPI = crypto) {
  const accounts = managedDeliveryAccountAllowlist(env);
  const canonical = accounts.join(",");
  const digest = await cryptoAPI.subtle.digest(
    "SHA-256",
    new TextEncoder().encode(canonical),
  );
  return Object.freeze({
    schema: MANAGED_DELIVERY_COHORT_SCHEMA,
    account_count: accounts.length,
    allowlist_sha256: hex(digest),
    empty: accounts.length === 0,
  });
}
