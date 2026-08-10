export const MANAGED_DELIVERY_ACCOUNT_ALLOWLIST_BINDING =
  "AGENT_EMAIL_MANAGED_DELIVERY_ACCOUNT_ALLOWLIST";

const ACCOUNT_ID = /^[A-Za-z0-9_-]{1,128}$/;
const MAX_ACCOUNTS = 100;
const MAX_BYTES = MAX_ACCOUNTS * 129;

// This parser intentionally matches the control-plane contract byte for byte.
// Empty is the safe default; there is no wildcard, trimming, case folding,
// duplicate collapse, or implicit ordering.
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

export function managedDeliveryAccountIsAdmitted(env, accountID) {
  if (!ACCOUNT_ID.test(String(accountID ?? ""))) {
    throw new TypeError("managed email delivery account is invalid");
  }
  return parseManagedDeliveryAccountAllowlist(String(
    env?.[MANAGED_DELIVERY_ACCOUNT_ALLOWLIST_BINDING] ?? "",
  )).includes(accountID);
}
