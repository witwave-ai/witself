const API_ROOT = "https://api.cloudflare.com/client/v4";
export const EMAIL_DIRECTORY_TITLE = "witself-agent-email-pilot-directory";

function required(value, name, pattern = /^[A-Za-z0-9_-]+$/) {
  if (!value || !pattern.test(value)) throw new Error(`${name} is missing or invalid`);
  return value;
}

export function cloudflareEnvironment(env = process.env) {
  return {
    accountID: required(env.CLOUDFLARE_ACCOUNT_ID, "CLOUDFLARE_ACCOUNT_ID", /^[0-9a-f]{32}$/),
    zoneID: env.CLOUDFLARE_ZONE_ID
      ? required(env.CLOUDFLARE_ZONE_ID, "CLOUDFLARE_ZONE_ID", /^[0-9a-f]{32}$/)
      : "",
    namespaceID: env.EMAIL_DIRECTORY_KV_ID
      ? required(env.EMAIL_DIRECTORY_KV_ID, "EMAIL_DIRECTORY_KV_ID", /^[0-9a-f]{32}$/)
      : "",
    apiToken: required(env.CLOUDFLARE_API_TOKEN, "CLOUDFLARE_API_TOKEN", /^\S+$/),
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

  async request(path, { method = "GET", body, raw = false } = {}) {
    const headers = { Authorization: `Bearer ${this.apiToken}` };
    if (body !== undefined) headers["Content-Type"] = "application/json";
    let response;
    try {
      response = await this.fetchAPI(`${API_ROOT}${path}`, {
        method,
        headers,
        body: body === undefined ? undefined : typeof body === "string" ? body : JSON.stringify(body),
        redirect: "error",
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
    return result.result;
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

  async deleteKV(key) {
    if (!this.namespaceID) throw new Error("EMAIL_DIRECTORY_KV_ID is required");
    return this.request(
      `/accounts/${this.accountID}/storage/kv/namespaces/${this.namespaceID}/values/${encodeURIComponent(key)}`,
      { method: "DELETE" },
    );
  }

  async listRules() {
    if (!this.zoneID) throw new Error("CLOUDFLARE_ZONE_ID is required");
    return this.request(`/zones/${this.zoneID}/email/routing/rules?per_page=200`);
  }

  async getCatchAll() {
    if (!this.zoneID) throw new Error("CLOUDFLARE_ZONE_ID is required");
    return this.request(`/zones/${this.zoneID}/email/routing/rules/catch_all`);
  }

  async createRule(rule) {
    return this.request(`/zones/${this.zoneID}/email/routing/rules`, { method: "POST", body: rule });
  }

  async updateRule(ruleID, rule) {
    if (!/^[0-9a-f]{1,32}$/.test(ruleID)) throw new Error("Cloudflare routing rule id is invalid");
    return this.request(`/zones/${this.zoneID}/email/routing/rules/${ruleID}`, {
      method: "PUT",
      body: rule,
    });
  }

  async deleteRule(ruleID) {
    if (!/^[0-9a-f]{1,32}$/.test(ruleID)) throw new Error("Cloudflare routing rule id is invalid");
    return this.request(`/zones/${this.zoneID}/email/routing/rules/${ruleID}`, { method: "DELETE" });
  }
}

export async function assertIsolatedEmailDirectory(api) {
  const namespace = await api.getNamespace();
  if (namespace?.id !== api.namespaceID || namespace?.title !== EMAIL_DIRECTORY_TITLE) {
    throw new Error(`refusing non-isolated KV namespace; expected ${EMAIL_DIRECTORY_TITLE}`);
  }
  return namespace;
}
