# Claude-to-Codex delegation

Witself's production-readiness work uses Claude Code as the orchestrator and
Codex (GPT-5.6 Sol) as a delegated implementer and reviewer. The integration
is the official OpenAI **Codex plugin for Claude Code**
(`openai/codex-plugin-cc`), which runs Codex through the local Codex app
server and works from the Claude Code desktop app and the CLI without any
project-local broker, launcher, or hook.

## One-time setup

```bash
# Current Codex CLI with the app server (the Homebrew formula is stale).
npm install -g @openai/codex
codex login status

# Plugin.
claude plugin marketplace add openai/codex-plugin-cc
claude plugin install codex@openai-codex
```

Then, in a fresh Claude Code session, `/codex:setup` should report `ready`.
Model and reasoning defaults come from `~/.codex/config.toml` (`model`,
`model_reasoning_effort`); per-call overrides use `--model` and `--effort`.
Leave the plugin's optional stop-time review gate disabled for this
repository; Claude runs its own review loop.

## What the plugin provides

| Command | Lane | Notes |
| --- | --- | --- |
| `/codex:rescue <task> [--background\|--wait] [--model M] [--effort E]` | implementation or investigation | Hands the task to the `codex:codex-rescue` subagent, which runs the companion `task` with `--write` (Codex `workspace-write` sandbox, approvals `never`). |
| `/codex:review [--base REF]` | read-only review | Structured findings on the working tree or branch. |
| `/codex:adversarial-review [focus]` | read-only challenge review | Questions the chosen design and implementation. |
| `/codex:status`, `/codex:result`, `/codex:cancel` | job control | Background jobs are detached workers; state lives under the plugin data directory, never inside the repository. |
| `/codex:transfer` | hand-off | Creates a persistent Codex thread from the current Claude session. |

Claude may also call the companion directly from Bash
(`node "$CLAUDE_PLUGIN_ROOT/scripts/codex-companion.mjs" task ...`) when it
needs to control the working directory, for example to run Codex inside a
dedicated worktree.

## How Witself uses it

The project skill [`codex-delegation`](../.claude/skills/codex-delegation/SKILL.md)
is the operating contract: Claude creates the branch and worktree, writes a
bounded task, starts Codex inside that worktree, reads the complete diff and
`touchedFiles`, runs the repository gates (`make check` before any push),
runs an adversarial review, and only then commits, opens the PR, waits for
CI, and squash-merges at the reviewed head. Codex output is a lead, never a
conclusion, and never authorization for deployment, secrets use, account
changes, destructive recovery, or external communication.

## Trust boundary

Codex runs as the current OS user under its own sandbox; the sandbox bounds
file writes to the workspace and temp and does not expand the user's
authorization. The plugin's hooks are limited to session lifecycle bookkeeping
and the optional stop-time review gate. Everything Codex returns is untrusted
data to be verified against the actual worktree.

## History

An earlier in-repository broker (`tools/claude-codex-broker`), terminal
launcher (`scripts/claude-codex.mjs`), grant hook, and profile module
provided the same capability with one-use HMAC grants and a terminal-only
elevated ceiling. It was removed when the official plugin made the
integration app-native; see the Git history before the removal commit for
that design.
