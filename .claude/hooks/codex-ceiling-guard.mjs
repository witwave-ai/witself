#!/usr/bin/env node
import process from "node:process";
import { pathToFileURL } from "node:url";

import {
  BROKER_SERVER_NAME,
  CEILING_ENV,
  GRANT_FIELD,
  GRANT_KEY_FILE_ENV,
  ceilingAllowsTool,
  createGrant,
  isModeAllowedForTool,
  normalizeHookMode,
  readGrantKey,
  requireProfile,
  toolKind,
} from "../codex-profiles.mjs";

const TOOL_PREFIX = `mcp__${BROKER_SERVER_NAME}__`;

function denial(reason) {
  return {
    action: "deny",
    reason,
    output: {
      hookSpecificOutput: {
        hookEventName: "PreToolUse",
        permissionDecision: "deny",
        permissionDecisionReason: reason,
      },
    },
  };
}

export function evaluateHookInput(input, environment = process.env, options = {}) {
  if (!input || typeof input !== "object" || Array.isArray(input)) {
    return denial("Codex delegation was denied because the PreToolUse input was malformed.");
  }
  if (typeof input.tool_name !== "string" || !input.tool_name.startsWith(TOOL_PREFIX)) {
    return { action: "ignore" };
  }
  if (input.hook_event_name !== "PreToolUse") {
    return denial("Codex delegation was denied because the hook event was not PreToolUse.");
  }

  const shortToolName = input.tool_name.slice(TOOL_PREFIX.length);
  const kind = toolKind(shortToolName);
  if (!kind) return denial("Codex delegation was denied because the broker tool is not on the exact profile whitelist.");

  const ceiling = environment[CEILING_ENV] ?? "repository";
  try {
    requireProfile(ceiling);
  } catch {
    return denial("Codex delegation was denied because the startup ceiling is missing or invalid.");
  }
  if (!ceilingAllowsTool(ceiling, shortToolName)) {
    return denial(`Codex delegation was denied because ${shortToolName} exceeds the immutable ${ceiling} startup ceiling.`);
  }

  let mode;
  try {
    mode = normalizeHookMode(input.permission_mode);
  } catch {
    return denial("Codex delegation was denied because Claude's current permission mode is missing or invalid.");
  }
  if (!isModeAllowedForTool(shortToolName, mode)) {
    return denial(`Codex delegation was denied because ${shortToolName} is not allowed in Claude permission mode ${mode}.`);
  }

  if (kind === "review" || options.issueGrant !== true) return { action: "ignore" };
  if (!input.tool_input || typeof input.tool_input !== "object" || Array.isArray(input.tool_input)) {
    return denial("Codex delegation was denied because the elevated tool input is malformed.");
  }
  if (Object.hasOwn(input.tool_input, GRANT_FIELD)) {
    return denial(`Codex delegation was denied because ${GRANT_FIELD} is reserved for the trusted launcher hook.`);
  }

  try {
    const key = options.key ?? readGrantKey(environment[GRANT_KEY_FILE_ENV]);
    const grant = createGrant({
      key,
      ceiling,
      toolName: shortToolName,
      mode,
      toolUseId: input.tool_use_id,
      sessionId: input.session_id ?? null,
      originalInput: input.tool_input,
      now: options.now?.() ?? Date.now(),
      nonce: options.nonce?.(),
    });
    return {
      action: "grant",
      grant,
      output: {
        hookSpecificOutput: {
          hookEventName: "PreToolUse",
          permissionDecision: "allow",
          permissionDecisionReason: "Authorized by the immutable Codex startup profile for this exact one-use call.",
          updatedInput: { ...input.tool_input, [GRANT_FIELD]: grant },
        },
      },
    };
  } catch {
    return denial("Codex delegation was denied because the one-use broker grant could not be created.");
  }
}

async function readStdin() {
  let body = "";
  for await (const chunk of process.stdin) {
    body += chunk;
    if (Buffer.byteLength(body, "utf8") > 1024 * 1024) throw new Error("Hook input is too large.");
  }
  return body;
}

async function main() {
  let result;
  try {
    const body = await readStdin();
    const mode = process.argv[2];
    if (mode !== "--deny-only" && mode !== "--issue-grant") throw new Error("Missing hook operating mode.");
    result = evaluateHookInput(JSON.parse(body), process.env, { issueGrant: mode === "--issue-grant" });
  } catch {
    result = denial("Codex delegation was denied because the PreToolUse hook could not validate its input.");
  }

  if (result.output) process.stdout.write(`${JSON.stringify(result.output)}\n`);
  if (result.action === "deny") process.exitCode = 2;
}

if (process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href) await main();
