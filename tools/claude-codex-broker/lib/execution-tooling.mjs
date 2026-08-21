import fs from "node:fs/promises";
import path from "node:path";
import { fileURLToPath } from "node:url";

import { BrokerError } from "./util.mjs";

export const SYSTEM_FEATURES = Object.freeze({
  // Sol selects Code Mode through model metadata. Its under-development
  // code_mode flags stay unset; the process-level host and nested tools do not.
  shell_tool: true,
  unified_exec: true,
  code_mode_host: true,
  multi_agent: true,
  goals: false,
  guardian_approval: false,
  hooks: true,
  apps: false,
  plugins: false,
  recommended_plugins: false,
  tool_suggest: false,
});

export const CONSTRAINED_FEATURES = Object.freeze({
  // Keep only the local execution substrate and explicitly allowed internal
  // collaboration. The remaining entries are provider, browser, computer,
  // plugin, dependency-install, elicitation, or external-search surfaces.
  shell_tool: true,
  unified_exec: true,
  code_mode_host: true,
  multi_agent: true,
  apps: false,
  enable_mcp_apps: false,
  plugins: false,
  recommended_plugins: false,
  tool_suggest: false,
  in_app_browser: false,
  in_app_chat: false,
  in_app_dictation: false,
  in_app_updates: false,
  browser_use: false,
  browser_use_full_cdp_access: false,
  browser_use_external: false,
  computer_use: false,
  remote_plugin: false,
  plugin_sharing: false,
  image_generation: false,
  skill_mcp_dependency_install: false,
  skill_search: false,
  tool_call_mcp_elicitation: false,
  auth_elicitation: false,
  request_permissions_tool: false,
  hooks: true,
  goals: false,
  guardian_approval: false,
  web_search_request: false,
  web_search_cached: false,
  standalone_web_search: false,
  workspace_dependencies: false,
});

// These 0.149 features remain enabled but do not add an externally effectful
// model tool. Exact equality makes a newly enabled latest feature fail closed
// until it is classified and pinned or allowlisted here.
export const CONSTRAINED_ALLOWED_UNPINNED_ENABLED_FEATURES = Object.freeze([
  "collaboration_modes",
  "enable_request_compression",
  "fast_mode",
  "item_ids",
  "mentions_v2",
  "personality",
  "remote_compaction_v2",
  "resize_all_images",
  "shell_snapshot",
  "sqlite",
  "steer",
  "terminal_resize_reflow",
  "tool_search_always_defer_mcp_tools",
  "tui_app_server",
  "unbounded_connection_retries",
  "view_image",
]);

export function featureTomlLines(features) {
  return Object.entries(features).map(([name, enabled]) => `${name} = ${enabled}`);
}

export function localEnvironmentSelection(cwd) {
  // In app-server v2, [] explicitly disables environment access and therefore
  // removes filesystem/process tools. Keep the reserved local target explicit.
  return [{ environmentId: "local", cwd, runtimeWorkspaceRoots: [cwd] }];
}

function safeString(value) {
  return typeof value === "string" && value.length > 0 && value.length <= 4096 && !/[\0\r\n]/u.test(value);
}

export async function attestExecutionTooling(
  request, threadId, cwd, expectedFeatures, errorCode, allowedUnpinnedEnabledFeatures = null,
) {
  const environmentParams = { environmentId: "local" };
  const [status, info] = await Promise.all([
    request("environment/status", environmentParams),
    request("environment/info", environmentParams),
  ]);
  let actualCwd;
  let expectedCwd;
  try {
    actualCwd = await fs.realpath(fileURLToPath(info?.cwd));
    expectedCwd = await fs.realpath(cwd);
  } catch {
    throw new BrokerError(errorCode, "Codex did not attest the fixed local execution environment and working directory.");
  }
  if (status?.status !== "ready" || status?.error != null || actualCwd !== expectedCwd ||
      !safeString(info?.shell?.name) || !safeString(info?.shell?.path)) {
    throw new BrokerError(errorCode, "Codex did not attest the fixed local execution environment and working directory.");
  }

  const features = [];
  let cursor = null;
  for (let page = 0; page < 10; page += 1) {
    const response = await request("experimentalFeature/list", { threadId, limit: 100, cursor });
    if (!Array.isArray(response?.data) || response.data.length > 100) {
      throw new BrokerError(errorCode, "Codex returned an invalid effective execution-feature catalog.");
    }
    features.push(...response.data);
    cursor = response.nextCursor;
    if (cursor == null) break;
    if (typeof cursor !== "string" || cursor.length > 4096 || page === 9) {
      throw new BrokerError(errorCode, "Codex execution-feature pagination exceeded its fixed bound.");
    }
  }
  for (const [name, expectedEnabled] of Object.entries(expectedFeatures)) {
    const matches = features.filter((feature) => feature?.name === name);
    if (matches.length !== 1 || matches[0].enabled !== expectedEnabled ||
        (expectedEnabled && matches[0].stage !== "stable")) {
      throw new BrokerError(errorCode, "Codex did not attest the fixed local-tool and external-capability feature ceiling.");
    }
  }
  if (allowedUnpinnedEnabledFeatures !== null) {
    const allowed = new Set(allowedUnpinnedEnabledFeatures);
    if (allowed.size !== allowedUnpinnedEnabledFeatures.length || [...allowed].some((name) => Object.hasOwn(expectedFeatures, name))) {
      throw new BrokerError(errorCode, "Codex advertised an unexpected enabled feature under the constrained ceiling.");
    }
    for (const name of allowed) {
      const matches = features.filter((feature) => feature?.name === name);
      if (matches.length !== 1 || matches[0].enabled !== true) {
        throw new BrokerError(errorCode, "Codex advertised an unexpected enabled feature under the constrained ceiling.");
      }
    }
    if (features.some((feature) => feature?.enabled === true &&
        !Object.hasOwn(expectedFeatures, feature?.name) && !allowed.has(feature?.name))) {
      throw new BrokerError(errorCode, "Codex advertised an unexpected enabled feature under the constrained ceiling.");
    }
  }

  return Object.freeze({
    environmentId: "local",
    environmentStatus: "ready",
    cwd: expectedCwd,
    features: Object.freeze({ ...expectedFeatures }),
  });
}
