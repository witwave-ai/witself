export const CONFIG_KEY = "pilot:config:v1";
export const RECIPIENT_PREFIX = "pilot:recipient:v1:";
export const DIRECTORY_SCHEMA_VERSION = 1;

const DOMAIN_LABEL = /^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$/;
const REALM_ID = /^realm_([a-z2-7]{16})$/;
const AGENT_ID = /^agent_[a-z2-7]{16}$/;
const AGENT_SEGMENT = /^[a-z0-9](?:[a-z0-9-]{0,45}[a-z0-9])?$/;
const AUDIENCE = /^[a-z](?:[a-z0-9-]{0,126}[a-z0-9])?$/;
const WORKER_NAME = /^[A-Za-z0-9](?:[A-Za-z0-9-]{0,61}[A-Za-z0-9])?$/;
const RESERVED = new Set([
  "abuse", "admin", "administrator", "billing", "hostmaster", "info",
  "mailer-daemon", "no-reply", "noc", "noreply", "postmaster", "root",
  "sales", "security", "support", "webmaster",
]);

export function validateDomain(value) {
  if (typeof value !== "string" || value !== value.trim().toLowerCase().replace(/\.$/, "")) {
    throw new Error("domain must be canonical lowercase ASCII DNS");
  }
  if (value.length < 1 || value.length > 253 || value.split(".").some((part) => !DOMAIN_LABEL.test(part))) {
    throw new Error("domain must be canonical lowercase ASCII DNS");
  }
  return value;
}

export function parsePilotAddress(value, expectedDomain, expectedRealmLabel, allowTag = false) {
  if (typeof value !== "string" || value !== value.trim().toLowerCase() || value.length > 320) {
    throw new Error("pilot address must be canonical lowercase ASCII");
  }
  const parts = value.split("@");
  if (parts.length !== 2) throw new Error("pilot address is malformed");
  let [local, domain] = parts;
  domain = validateDomain(domain);
  if (domain !== expectedDomain) throw new Error("pilot address uses another domain");
  let tag = "";
  const plus = local.indexOf("+");
  if (plus >= 0) {
    tag = local.slice(plus + 1);
    local = local.slice(0, plus);
    if (!allowTag || tag.length < 1 || tag.length > 64 || !/^[\x21-\x2a\x2c-\x3f\x41-\x7e]+$/.test(tag)) {
      throw new Error("pilot subaddress tag is invalid");
    }
  }
  const components = local.split(".");
  if (components.length !== 2) throw new Error("pilot address local part is malformed");
  const [agentSegment, realmLabel] = components;
  if (!AGENT_SEGMENT.test(agentSegment) || agentSegment.length > 47 || RESERVED.has(agentSegment)) {
    throw new Error("pilot agent segment is invalid");
  }
  if (realmLabel !== expectedRealmLabel) throw new Error("pilot address uses another realm");
  const baseAddress = `${local}@${domain}`;
  if (baseAddress.length > 90) throw new Error("pilot address exceeds Cloudflare literal-rule limit");
  return { address: value, baseAddress, domain, agentSegment, realmLabel, tag };
}

export function normalizePilotManifest(input) {
  if (!input || typeof input !== "object" || Array.isArray(input) || input.schema_version !== 1) {
    throw new Error("pilot manifest schema_version must be 1");
  }
  const realmID = String(input.realm_id ?? "");
  const realmMatch = REALM_ID.exec(realmID);
  if (!realmMatch) throw new Error("pilot manifest realm_id is invalid");
  const realmLabel = String(input.realm_label ?? "");
  if (realmLabel !== realmMatch[1]) throw new Error("pilot realm_label must match the realm id body");
  const domain = validateDomain(String(input.domain ?? ""));
  const workerName = String(input.worker_name ?? "");
  if (!WORKER_NAME.test(workerName) || workerName !== "witself-agent-email-pilot") {
    throw new Error("pilot manifest must target the isolated witself-agent-email-pilot Worker");
  }
  const cellAudience = String(input.cell_audience ?? "");
  if (!AUDIENCE.test(cellAudience) || cellAudience.length > 128) {
    throw new Error("pilot cell audience is invalid");
  }
  let ingestURL;
  try {
    ingestURL = new URL(String(input.ingest_url ?? ""));
  } catch {
    throw new Error("pilot ingestion URL is invalid");
  }
  if (
    ingestURL.protocol !== "https:" ||
    ingestURL.username || ingestURL.password || ingestURL.hash ||
    !ingestURL.hostname || ingestURL.hostname === "localhost"
  ) {
    throw new Error("pilot ingestion URL must be a credential-free HTTPS URL");
  }
  if (!Array.isArray(input.agents) || input.agents.length < 5 || input.agents.length > 10) {
    throw new Error("pilot manifest must explicitly enroll 5-10 agents");
  }
  const addresses = new Set();
  const agentIDs = new Set();
  const agents = input.agents.map((agent) => {
    if (!agent || typeof agent !== "object" || Array.isArray(agent)) throw new Error("pilot agent entry is invalid");
    const agentID = String(agent.agent_id ?? "");
    // JavaScript `$` also matches immediately before one final line break;
    // the exact byte length closes that difference from Go's generated-id
    // validator while retaining the shared canonical regex.
    if (agentID.length !== 22 || !AGENT_ID.test(agentID) || agentIDs.has(agentID)) {
      throw new Error("pilot agent id is invalid or duplicated");
    }
    const parsed = parsePilotAddress(String(agent.address ?? ""), domain, realmLabel, false);
    if (addresses.has(parsed.baseAddress)) throw new Error("pilot address is duplicated");
    addresses.add(parsed.baseAddress);
    agentIDs.add(agentID);
    return { agent_id: agentID, address: parsed.baseAddress };
  });
  agents.sort((left, right) => left.address.localeCompare(right.address));
  return {
    schema_version: 1,
    realm_id: realmID,
    realm_label: realmLabel,
    domain,
    worker_name: workerName,
    cell_audience: cellAudience,
    ingest_url: ingestURL.toString(),
    agents,
  };
}

export function validateRuntimeConfig(value) {
  const manifest = normalizePilotManifest(value);
  if (typeof value.enabled !== "boolean") throw new Error("pilot directory enabled state is invalid");
  return { ...manifest, enabled: value.enabled };
}

export function validateRuntimeRecipient(value, config, baseAddress) {
  if (!value || typeof value !== "object" || Array.isArray(value) || value.schema_version !== 1) {
    throw new Error("pilot recipient directory entry is invalid");
  }
  if (value.enabled !== true || value.realm_id !== config.realm_id || value.address !== baseAddress) {
    throw new Error("pilot recipient directory entry is inconsistent");
  }
  const enrolled = config.agents.find((agent) => agent.address === baseAddress);
  if (!enrolled || value.agent_id !== enrolled.agent_id) {
    throw new Error("pilot recipient is not enrolled");
  }
  if (value.cell_audience !== config.cell_audience || value.ingest_url !== config.ingest_url) {
    throw new Error("pilot recipient cell route is inconsistent");
  }
  return value;
}

export function runtimeConfig(manifest, enabled) {
  return { ...manifest, enabled };
}

export function runtimeRecipient(manifest, agent) {
  return {
    schema_version: DIRECTORY_SCHEMA_VERSION,
    enabled: true,
    realm_id: manifest.realm_id,
    agent_id: agent.agent_id,
    address: agent.address,
    cell_audience: manifest.cell_audience,
    ingest_url: manifest.ingest_url,
  };
}

export function recipientKey(address) {
  return `${RECIPIENT_PREFIX}${address}`;
}
