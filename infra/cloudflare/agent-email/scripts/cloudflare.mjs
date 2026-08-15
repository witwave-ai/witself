const API_ROOT = "https://api.cloudflare.com/client/v4";
const ROUTING_RULE_ID = /^[0-9a-f]{1,32}(?![\s\S])/;
const ROUTING_INVENTORY_SNAPSHOT_ATTEMPTS = 4;
// This provider resource name predates the production Worker split. Its ID is
// shared by both paths, so keep the title stable during the Worker migration.
export const EMAIL_DIRECTORY_TITLE = "witself-agent-email-pilot-directory";

class RoutingInventoryDriftError extends Error {}

function canonicalJSON(value) {
  const canonicalize = (item) => {
    if (Array.isArray(item)) return item.map(canonicalize);
    if (item && typeof item === "object") {
      return Object.fromEntries(
        Object.keys(item).sort().map((key) => [key, canonicalize(item[key])]),
      );
    }
    return item;
  };
  return JSON.stringify(canonicalize(value));
}

function canonicalRuleInventory(rules) {
  return canonicalJSON([...rules].sort((left, right) =>
    String(left.id).localeCompare(String(right.id))));
}

function required(value, name, pattern = /^[A-Za-z0-9_-]+(?![\s\S])/) {
  if (!value || !pattern.test(value)) throw new Error(`${name} is missing or invalid`);
  return value;
}

export function cloudflareEnvironment(env = process.env) {
  return {
    accountID: required(env.CLOUDFLARE_ACCOUNT_ID, "CLOUDFLARE_ACCOUNT_ID", /^[0-9a-f]{32}(?![\s\S])/),
    zoneID: env.CLOUDFLARE_ZONE_ID
      ? required(env.CLOUDFLARE_ZONE_ID, "CLOUDFLARE_ZONE_ID", /^[0-9a-f]{32}(?![\s\S])/)
      : "",
    namespaceID: env.EMAIL_DIRECTORY_KV_ID
      ? required(env.EMAIL_DIRECTORY_KV_ID, "EMAIL_DIRECTORY_KV_ID", /^[0-9a-f]{32}(?![\s\S])/)
      : "",
    apiToken: required(env.CLOUDFLARE_API_TOKEN, "CLOUDFLARE_API_TOKEN", /^\S+(?![\s\S])/),
  };
}

export class CloudflareAPI {
  constructor({ accountID, zoneID = "", namespaceID = "", apiToken, fetchAPI = fetch }) {
    this.accountID = accountID;
    this.zoneID = zoneID;
    this.namespaceID = namespaceID;
    this.apiToken = apiToken;
    this.fetchAPI = fetchAPI;
  }

  async request(path, { method = "GET", body, raw = false, envelope = false } = {}) {
    const headers = { Authorization: `Bearer ${this.apiToken}` };
    if (body !== undefined) headers["Content-Type"] = "application/json";
    let response;
    try {
      response = await this.fetchAPI(`${API_ROOT}${path}`, {
        method,
        headers,
        body: body === undefined ? undefined : typeof body === "string" ? body : JSON.stringify(body),
        redirect: "error",
        signal: AbortSignal.timeout(15_000),
      });
    } catch {
      throw new Error("Cloudflare API request failed");
    }
    if (raw) {
      if (!response.ok) throw new Error(`Cloudflare API request failed with status ${response.status}`);
      return response;
    }
    let result;
    try {
      result = await response.json();
    } catch {
      throw new Error(`Cloudflare API returned a malformed response (${response.status})`);
    }
    if (!response.ok || result?.success !== true) {
      const code = Array.isArray(result?.errors) && result.errors[0]?.code
        ? ` (${result.errors[0].code})`
        : "";
      throw new Error(`Cloudflare API request failed${code}`);
    }
    return envelope ? result : result.result;
  }

  async queryAnalytics(query) {
    if (typeof query !== "string" || query.trim() === "" || query.length > 16_384) {
      throw new Error("Analytics Engine query is missing or invalid");
    }
    let response;
    try {
      response = await this.fetchAPI(
        `${API_ROOT}/accounts/${this.accountID}/analytics_engine/sql`,
        {
          method: "POST",
          headers: { Authorization: `Bearer ${this.apiToken}` },
          body: query,
          redirect: "error",
        },
      );
    } catch {
      throw new Error("Cloudflare Analytics Engine query failed");
    }
    let result;
    try {
      result = await response.json();
    } catch {
      throw new Error(`Cloudflare Analytics Engine returned a malformed response (${response.status})`);
    }
    if (!response.ok || !result || typeof result !== "object" || Array.isArray(result)) {
      throw new Error(`Cloudflare Analytics Engine query failed with status ${response.status}`);
    }
    return result;
  }

  async getNamespace() {
    if (!this.namespaceID) throw new Error("EMAIL_DIRECTORY_KV_ID is required");
    return this.request(`/accounts/${this.accountID}/storage/kv/namespaces/${this.namespaceID}`);
  }

  async listNamespaces() {
    return this.request(`/accounts/${this.accountID}/storage/kv/namespaces?per_page=100`);
  }

  async createNamespace(title = EMAIL_DIRECTORY_TITLE) {
    return this.request(`/accounts/${this.accountID}/storage/kv/namespaces`, {
      method: "POST",
      body: { title },
    });
  }

  async putKV(key, value) {
    if (!this.namespaceID) throw new Error("EMAIL_DIRECTORY_KV_ID is required");
    return this.request(
      `/accounts/${this.accountID}/storage/kv/namespaces/${this.namespaceID}/values/${encodeURIComponent(key)}`,
      { method: "PUT", body: JSON.stringify(value) },
    );
  }

  async getKVJSON(key) {
    if (!this.namespaceID) throw new Error("EMAIL_DIRECTORY_KV_ID is required");
    const response = await this.request(
      `/accounts/${this.accountID}/storage/kv/namespaces/${this.namespaceID}/values/${encodeURIComponent(key)}`,
      { raw: true },
    );
    const declared = Number(response.headers.get("Content-Length"));
    if (Number.isFinite(declared) && declared > 16_384) {
      throw new Error("Cloudflare KV value exceeded the route projection limit");
    }
    const raw = await response.text();
    if (raw.length < 2 || raw.length > 16_384) {
      throw new Error("Cloudflare KV value exceeded the route projection limit");
    }
    try {
      return JSON.parse(raw);
    } catch {
      throw new Error("Cloudflare KV route projection was malformed");
    }
  }

  async deleteKV(key) {
    if (!this.namespaceID) throw new Error("EMAIL_DIRECTORY_KV_ID is required");
    return this.request(
      `/accounts/${this.accountID}/storage/kv/namespaces/${this.namespaceID}/values/${encodeURIComponent(key)}`,
      { method: "DELETE" },
    );
  }

  async readRuleInventorySnapshot() {
    const perPage = 50;
    const maximumPages = 200;
    const rules = [];
    const ruleIDs = new Set();
    let expectedTotalPages;
    let expectedTotalCount;
    let expectedHasTotalPages;
    for (let page = 1; page <= maximumPages; page += 1) {
      const response = await this.request(
        `/zones/${this.zoneID}/email/routing/rules?page=${page}&per_page=${perPage}`,
        { envelope: true },
      );
      const info = response?.result_info;
      const pageRules = response?.result;
      const hasTotalPages = info != null &&
        Object.hasOwn(info, "total_pages");
      if (!Array.isArray(pageRules) || !info || typeof info !== "object" ||
          !Number.isSafeInteger(info.page) || info.page !== page ||
          !Number.isSafeInteger(info.per_page) || info.per_page !== perPage ||
          !Number.isSafeInteger(info.count) || info.count !== pageRules.length ||
          !Number.isSafeInteger(info.total_count) || info.total_count < 0 ||
          (hasTotalPages &&
           (!Number.isSafeInteger(info.total_pages) ||
            info.total_pages < page))) {
        throw new Error("Cloudflare Email Routing pagination was malformed");
      }
      const totalPages = hasTotalPages
        ? info.total_pages
        : Math.max(1, Math.ceil(info.total_count / perPage));
      if (totalPages > maximumPages ||
          info.total_count > maximumPages * perPage) {
        throw new Error("Cloudflare Email Routing rule inventory exceeded the safe pagination limit");
      }
      if (expectedTotalPages === undefined) {
        expectedTotalPages = totalPages;
        expectedTotalCount = info.total_count;
        expectedHasTotalPages = hasTotalPages;
      } else if (totalPages !== expectedTotalPages ||
          info.total_count !== expectedTotalCount ||
          hasTotalPages !== expectedHasTotalPages) {
        throw new RoutingInventoryDriftError(
          "Cloudflare Email Routing rule inventory changed during pagination",
        );
      }
      for (const rule of pageRules) {
        const ruleID = String(rule?.id ?? "");
        if (!ROUTING_RULE_ID.test(ruleID)) {
          throw new Error("Cloudflare Email Routing rule inventory contained an invalid id");
        }
        if (ruleIDs.has(ruleID)) {
          throw new RoutingInventoryDriftError(
            "Cloudflare Email Routing rule inventory shifted during pagination",
          );
        }
        ruleIDs.add(ruleID);
      }
      rules.push(...pageRules);
      if (page === expectedTotalPages) {
        if (rules.length !== expectedTotalCount) {
          throw new RoutingInventoryDriftError(
            "Cloudflare Email Routing pagination was incomplete",
          );
        }
        return rules;
      }
    }
    throw new Error("Cloudflare Email Routing rule inventory exceeded the safe pagination limit");
  }

  async listRules() {
    if (!this.zoneID) throw new Error("CLOUDFLARE_ZONE_ID is required");
    let previousCanonical;
    for (let attempt = 0; attempt < ROUTING_INVENTORY_SNAPSHOT_ATTEMPTS; attempt += 1) {
      let rules;
      try {
        rules = await this.readRuleInventorySnapshot();
      } catch (error) {
        if (!(error instanceof RoutingInventoryDriftError)) throw error;
        previousCanonical = undefined;
        continue;
      }
      const canonical = canonicalRuleInventory(rules);
      if (previousCanonical === canonical) return rules;
      previousCanonical = canonical;
    }
    throw new Error(
      "Cloudflare Email Routing rule inventory did not stabilize after bounded retries",
    );
  }

  async getEmailRoutingSettings() {
    if (!this.zoneID) throw new Error("CLOUDFLARE_ZONE_ID is required");
    return this.request(`/zones/${this.zoneID}/email/routing`);
  }

  async getZone() {
    if (!this.zoneID) throw new Error("CLOUDFLARE_ZONE_ID is required");
    return this.request(`/zones/${this.zoneID}`);
  }

  async getCatchAll() {
    if (!this.zoneID) throw new Error("CLOUDFLARE_ZONE_ID is required");
    return this.request(`/zones/${this.zoneID}/email/routing/rules/catch_all`);
  }

  async createRule(rule) {
    return this.request(`/zones/${this.zoneID}/email/routing/rules`, { method: "POST", body: rule });
  }

  async updateRule(ruleID, rule) {
    if (!ROUTING_RULE_ID.test(ruleID)) throw new Error("Cloudflare routing rule id is invalid");
    return this.request(`/zones/${this.zoneID}/email/routing/rules/${ruleID}`, {
      method: "PUT",
      body: rule,
    });
  }

  async deleteRule(ruleID) {
    if (!ROUTING_RULE_ID.test(ruleID)) throw new Error("Cloudflare routing rule id is invalid");
    return this.request(`/zones/${this.zoneID}/email/routing/rules/${ruleID}`, { method: "DELETE" });
  }

  async sendEmail(message) {
    if (!message || typeof message !== "object" || Array.isArray(message)) {
      throw new Error("Cloudflare email submission is invalid");
    }
    return this.request(`/accounts/${this.accountID}/email/sending/send`, {
      method: "POST",
      body: message,
    });
  }

  async sendRawEmail(message) {
    if (!message || typeof message !== "object" || Array.isArray(message)) {
      throw new Error("Cloudflare raw email submission is invalid");
    }
    return this.request(`/accounts/${this.accountID}/email/sending/send_raw`, {
      method: "POST",
      body: message,
    });
  }
}

export async function assertIsolatedEmailDirectory(api) {
  const namespace = await api.getNamespace();
  if (namespace?.id !== api.namespaceID || namespace?.title !== EMAIL_DIRECTORY_TITLE) {
    throw new Error(`refusing non-isolated KV namespace; expected ${EMAIL_DIRECTORY_TITLE}`);
  }
  return namespace;
}
