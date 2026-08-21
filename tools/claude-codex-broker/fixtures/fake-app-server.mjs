import fs from "node:fs";
import net from "node:net";
import path from "node:path";
import readline from "node:readline";
import { pathToFileURL } from "node:url";

const scenario = process.env.FAKE_CODEX_SCENARIO || "success";
const recordPath = process.env.FAKE_CODEX_RECORD;
const version = process.env.FAKE_CODEX_VERSION || "9.8.7";
let threadId = "01900000-0000-7000-8000-000000000001";
let turnId = "01900000-0000-7000-8000-000000000002";
let threadFeatures = {};

const FEATURE_CATALOG = Object.freeze([
  ["shell_tool", "stable", true],
  ["unified_exec", "stable", true],
  ["code_mode_host", "stable", true],
  ["multi_agent", "stable", true],
  ["apps", "stable", true],
  ["enable_mcp_apps", "underDevelopment", false],
  ["plugins", "stable", true],
  ["recommended_plugins", "stable", false],
  ["tool_suggest", "stable", true],
  ["in_app_browser", "stable", true],
  ["in_app_chat", "stable", true],
  ["in_app_dictation", "stable", true],
  ["in_app_updates", "stable", true],
  ["browser_use", "stable", true],
  ["browser_use_full_cdp_access", "stable", true],
  ["browser_use_external", "stable", true],
  ["computer_use", "stable", true],
  ["remote_plugin", "stable", true],
  ["plugin_sharing", "stable", true],
  ["image_generation", "stable", true],
  ["skill_mcp_dependency_install", "stable", true],
  ["skill_search", "stable", true],
  ["tool_call_mcp_elicitation", "stable", true],
  ["auth_elicitation", "stable", true],
  ["request_permissions_tool", "underDevelopment", false],
  ["hooks", "stable", true],
  ["goals", "stable", true],
  ["guardian_approval", "stable", true],
  ["web_search_request", "deprecated", false],
  ["web_search_cached", "deprecated", false],
  ["standalone_web_search", "underDevelopment", false],
  ["workspace_dependencies", "stable", true],
  ["collaboration_modes", "removed", true],
  ["enable_request_compression", "stable", true],
  ["fast_mode", "stable", true],
  ["item_ids", "removed", true],
  ["mentions_v2", "stable", true],
  ["personality", "stable", true],
  ["remote_compaction_v2", "stable", true],
  ["resize_all_images", "removed", true],
  ["shell_snapshot", "stable", true],
  ["sqlite", "removed", true],
  ["steer", "removed", true],
  ["terminal_resize_reflow", "removed", true],
  ["tool_search_always_defer_mcp_tools", "removed", true],
  ["tui_app_server", "removed", true],
  ["unbounded_connection_retries", "stable", true],
  ["view_image", "stable", true],
]);

function write(message) {
  process.stdout.write(`${JSON.stringify(message)}\n`);
}

function record(message) {
  if (recordPath) fs.appendFileSync(recordPath, `${JSON.stringify(message)}\n`, { encoding: "utf8", mode: 0o600 });
}

function model() {
  if (scenario === "bad-model") return { id: "gpt-5.6-sol", model: "gpt-5.6-sol", multiAgentVersion: "v1", supportedReasoningEfforts: [{ reasoningEffort: "max" }] };
  return {
    id: "gpt-5.6-sol",
    model: "gpt-5.6-sol",
    multiAgentVersion: "v2",
    supportedReasoningEfforts: [{ reasoningEffort: "ultra", description: "delegation" }],
  };
}

const rl = readline.createInterface({ input: process.stdin, crlfDelay: Infinity });
rl.on("line", (line) => {
  let message;
  try { message = JSON.parse(line); } catch { process.exit(71); }
  record(message);
  if (message.method === "initialized") return;
  if (message.method === "initialize") {
    write({ id: message.id, result: { userAgent: `witself-claude-codex-broker/${version} (fake)`, codexHome: process.env.CODEX_HOME, platformFamily: "unix", platformOs: "fake" } });
    return;
  }
  if (message.method === "account/login/start") {
    write({ id: message.id, result: { type: "chatgptAuthTokens" } });
    return;
  }
  if (message.method === "account/read") {
    write({ id: message.id, result: { requiresOpenaiAuth: true, account: { type: "chatgpt", email: "hidden@example.invalid", planType: scenario === "bad-account" ? "plus" : "pro" } } });
    return;
  }
  if (message.method === "model/list") {
    write({ id: message.id, result: { data: [model()], nextCursor: null } });
    return;
  }
  if (message.method === "experimentalFeature/list") {
    const data = FEATURE_CATALOG.filter(([name]) => scenario !== `missing-${name}`).map(([name, stage, defaultEnabled]) => ({
      name,
      stage,
      displayName: null,
      description: null,
      announcement: null,
      enabled: scenario === `disabled-${name}` ? false
        : scenario === `enabled-${name}` ? true
          : threadFeatures[name] ?? defaultEnabled,
      defaultEnabled,
    }));
    if (scenario === "unexpected-enabled-feature") data.push({
      name: "future_external_control", stage: "stable", displayName: null,
      description: null, announcement: null, enabled: true, defaultEnabled: true,
    });
    if (scenario === "mixed-duplicate-view_image") data.push({
      name: "view_image", stage: "stable", displayName: null,
      description: null, announcement: null, enabled: false, defaultEnabled: true,
    });
    write({ id: message.id, result: { data, nextCursor: null } });
    return;
  }
  if (message.method === "environment/status") {
    const ready = message.params?.environmentId === "local" && scenario !== "local-environment-unavailable";
    write({ id: message.id, result: ready
      ? { status: "ready" }
      : { status: "unknown", error: "environment unavailable" } });
    return;
  }
  if (message.method === "environment/info") {
    const cwd = scenario === "local-environment-wrong-cwd" ? path.dirname(process.cwd()) : process.cwd();
    write({ id: message.id, result: { shell: { name: "zsh", path: "/bin/zsh" }, cwd: pathToFileURL(cwd).href } });
    return;
  }
  if (message.method === "permissionProfile/list") {
    write({ id: message.id, result: {
      data: [{ id: ":danger-full-access", description: "full", allowed: scenario !== "system-profile-denied" }],
      nextCursor: null,
    } });
    return;
  }
  if (message.method === "configRequirements/read") {
    const requirements = scenario === "system-requirements-denied"
      ? { allowedSandboxModes: ["read-only"], allowedApprovalPolicies: ["never"] }
      : { allowedSandboxModes: ["danger-full-access"], allowedApprovalPolicies: ["never"], allowedApprovalsReviewers: ["user"], allowedWebSearchModes: ["live"], allowedPermissionProfiles: { ":danger-full-access": true }, cliAuthCredentialsStore: "file" };
    write({ id: message.id, result: { requirements } });
    return;
  }
  if (message.method === "skills/list") {
    const names = ["imagegen", "openai-docs", "plugin-creator", "review-agent", "skill-creator", "skill-installer"];
    const skills = names.map((name) => ({
      name,
      path: path.join(process.env.CODEX_HOME, "skills", ".system", name, "SKILL.md"),
      scope: "system",
      enabled: true,
    }));
    if (scenario === "unexpected-skill") skills.push({ name: "untrusted", path: path.join(process.env.CODEX_HOME, "skills", "untrusted", "SKILL.md"), scope: "user", enabled: true });
    write({ id: message.id, result: { data: [{ cwd: message.params.cwds[0], skills, errors: [] }] } });
    return;
  }
  if (message.method === "hooks/list") {
    let hooks = [];
    if (scenario === "unexpected-hook") hooks = [{
      eventName: "preToolUse", source: "project", sourcePath: ".codex/hooks", enabled: true,
      isManaged: false, trustStatus: "trusted", handlerType: "command", key: "project:bad", timeoutSec: 10,
      pluginId: null, command: "bad", currentHash: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
    }];
    if (scenario === "managed-hooks") {
      const events = ["permissionRequest", "postCompact", "postToolUse", "preCompact", "preToolUse", "sessionStart", "stop", "subagentStart", "subagentStop", "userPromptSubmit"];
      hooks = events.map((eventName) => ({
        eventName, source: "system", sourcePath: "/etc/codex/witself-hooks", enabled: true,
        isManaged: true, trustStatus: "managed", handlerType: "command", key: `/etc/codex/witself-hooks:${eventName}`,
        timeoutSec: 10, pluginId: null, currentHash: `sha256:${"a".repeat(64)}`,
        command: "'/etc/codex/witself-hooks/witself-transcript-hook-aaaaaaaaaaaaaaaaaaaaaaaa' --runtime codex --account 'default' --realm 'default' --agent 'scott' --location 'home' --witself-home '/Users/scott/.witself'",
      }));
    }
    write({ id: message.id, result: { data: [{ cwd: message.params.cwds[0], hooks, warnings: [], errors: [] }] } });
    return;
  }
  if (message.method === "thread/start") {
    threadFeatures = { ...(message.params?.config?.features ?? {}) };
    if (scenario === "unexpected-capability") {
      write({ method: "mcpServer/startupStatus/updated", params: { threadId, name: "codex_apps", status: "starting", error: null, failureReason: null } });
    }
    const system = message.params?.permissions === ":danger-full-access";
    const implementation = message.params?.permissions === "claude-implementation";
    const profile = message.params?.config?.permissions?.[message.params?.permissions];
    const writableRoots = Object.entries(profile?.filesystem ?? {})
      .filter(([root, access]) => access === "write" && (!implementation || root !== message.params.cwd)).map(([root]) => root);
    const bad = scenario === "bad-thread" || scenario === "bad-system-thread";
    write({
      id: message.id,
      result: {
        thread: { id: threadId, ephemeral: true, cwd: message.params.cwd, cliVersion: version },
        model: bad ? "gpt-5.6-terra" : message.params.model,
        modelProvider: "openai",
        cwd: message.params.cwd,
        runtimeWorkspaceRoots: [],
        instructionSources: [],
        approvalPolicy: message.params.approvalPolicy,
        approvalsReviewer: message.params.approvalsReviewer,
        sandbox: system
          ? { type: scenario === "bad-system-sandbox" ? "workspaceWrite" : "dangerFullAccess" }
          : {
              type: scenario === "bad-implementation-sandbox" && implementation ? "dangerFullAccess" : "workspaceWrite",
              writableRoots,
              networkAccess: false,
              excludeTmpdirEnvVar: implementation,
              excludeSlashTmp: true,
            },
        activePermissionProfile: { id: message.params.permissions, extends: null },
        reasoningEffort: "ultra",
        multiAgentMode: "explicitRequestOnly",
      },
    });
    return;
  }
  if (message.method === "command/exec") {
    const command = message.params?.command;
    if (!Array.isArray(command) || command.length < 1) {
      write({ id: message.id, error: { code: -32602, message: "bad command" } });
      return;
    }
    if (command[0] === "/usr/bin/git") {
      if (message.params?.permissionProfile === "claude-implementation" && command[1] === "-C" && command[2] !== message.params.cwd &&
          scenario !== "unsafe-implementation-source") {
        write({ id: message.id, result: { exitCode: 1, stdout: "", stderr: "Permission denied\n" } });
        return;
      }
      write({ id: message.id, result: { exitCode: 0, stdout: `${"a".repeat(40)}\n`, stderr: "" } });
      return;
    }
    if (message.params?.permissionProfile === ":danger-full-access" && command[0] === "/usr/bin/id") {
      write({ id: message.id, result: { exitCode: 0, stdout: `${process.getuid?.() ?? 0}\n`, stderr: "" } });
      return;
    }
    if (message.params?.permissionProfile === ":danger-full-access" && command[0] === process.execPath) {
      if (String(command[2]).includes("loopback-ok")) {
        const socket = net.connect({ host: "127.0.0.1", port: Number(command.at(-1)) });
        let body = "";
        socket.setEncoding("utf8");
        socket.on("data", (chunk) => { body += chunk; });
        socket.on("end", () => write({ id: message.id, result: { exitCode: body === "loopback-ok" ? 0 : 8, stdout: "", stderr: "" } }));
        socket.on("error", () => write({ id: message.id, result: { exitCode: 7, stdout: "", stderr: "connect failed" } }));
        return;
      }
      write({ id: message.id, result: { exitCode: 0, stdout: "spawn-ok", stderr: "" } });
      return;
    }
    if (["claude-review", "claude-implementation"].includes(message.params?.permissionProfile) && command[0] === "/usr/bin/nc") {
      const unsafeNetwork = (message.params.permissionProfile === "claude-review" && scenario === "unsafe-review-network") ||
        (message.params.permissionProfile === "claude-implementation" && scenario === "unsafe-implementation-network");
      if (unsafeNetwork) {
        const socket = net.connect({ host: "127.0.0.1", port: Number(command.at(-1)) });
        socket.on("connect", () => { socket.end(); write({ id: message.id, result: { exitCode: 0, stdout: "", stderr: "" } }); });
        socket.on("error", () => write({ id: message.id, result: { exitCode: 7, stdout: "", stderr: "connect failed" } }));
        return;
      }
      write({ id: message.id, result: { exitCode: 7, stdout: "", stderr: "network denied" } });
      return;
    }
    if (command[0] === "/usr/bin/touch") {
      if (message.params?.permissionProfile === "claude-implementation" && command[1].includes(`${path.sep}.git${path.sep}`) &&
          scenario !== "unsafe-implementation-git-write") {
        write({ id: message.id, result: { exitCode: 1, stdout: "", stderr: "Permission denied\n" } });
        return;
      }
      fs.writeFileSync(command[1], "", { encoding: "utf8", mode: 0o600 });
      write({ id: message.id, result: { exitCode: 0, stdout: "", stderr: "" } });
      return;
    }
    if (command[0] === "/bin/rm") {
      fs.rmSync(command.at(-1), { force: true });
      write({ id: message.id, result: { exitCode: 0, stdout: "", stderr: "" } });
      return;
    }
    if (command[0] === "/bin/cat") {
      if (command[1] === process.env.FAKE_DENIED_SENTINEL) {
        const allowed = scenario === "unsafe-sentinel" || message.params?.permissionProfile === ":danger-full-access";
        write({ id: message.id, result: { exitCode: allowed ? 0 : 1, stdout: allowed ? fs.readFileSync(command[1], "utf8") : "", stderr: allowed ? "" : "Permission denied\n" } });
        return;
      }
      try {
        write({ id: message.id, result: { exitCode: 0, stdout: fs.readFileSync(command[1], "utf8"), stderr: "" } });
      } catch {
        write({ id: message.id, result: { exitCode: 1, stdout: "", stderr: "read failed\n" } });
      }
      return;
    }
    write({ id: message.id, result: { exitCode: 127, stdout: "", stderr: "unsupported\n" } });
    return;
  }
  if (message.method === "turn/start") {
    if (scenario === "turn-reject") {
      write({ id: message.id, error: { code: -32602, message: "rejected" } });
      return;
    }
    if (scenario === "invalid-turn-start") {
      write({ id: message.id, result: { turn: { id: turnId, status: "completed", items: [] } } });
      return;
    }
    write({ id: message.id, result: { turn: { id: turnId, status: "inProgress", items: [] } } });
    if (scenario === "hang") return;
    if (scenario === "reroute") {
      setTimeout(() => write({ method: "model/rerouted", params: { threadId, turnId } }), 10);
      return;
    }
    if (scenario === "refresh-request") {
      setTimeout(() => write({ id: 991, method: "account/chatgptAuthTokens/refresh", params: {} }), 10);
      return;
    }
    const system = message.params?.permissions === ":danger-full-access";
    const implementation = message.params?.permissions === "claude-implementation";
    const report = system
      ? { summary: scenario === "system-secret-result" ? `done ${process.env.FAKE_LAUNCHER_SECRET}` : "system done", actions: ["acted"], checks: ["verified"], changes: [{ scope: "repository", description: "updated requested state", reversible: true }], blockers: [], warnings: [] }
      : implementation
        ? scenario === "bad-implementation-result"
          ? { summary: "implemented", actions: [], checks: [], blockers: [], warnings: [], leaked: true }
          : { summary: "implemented", actions: ["updated isolated workspace"], checks: ["local checks"], blockers: [], warnings: [] }
        : scenario === "bad-result"
        ? { summary: "bad", findings: [{ severity: "low", title: "escape", detail: "x", path: "../secret", line: 1 }], checks: [], blockers: [] }
        : { summary: "reviewed", findings: [], checks: ["read-only inspection"], blockers: [] };
    setTimeout(() => write({
      method: "turn/completed",
      params: { threadId, turn: { id: turnId, status: "completed", items: [{ id: "msg", type: "agentMessage", text: JSON.stringify(report) }] } },
    }), 10);
    return;
  }
  if (message.method === "turn/interrupt") {
    write({ id: message.id, result: {} });
    return;
  }
  if (Object.hasOwn(message, "id")) write({ id: message.id, error: { code: -32601, message: "unknown" } });
});
