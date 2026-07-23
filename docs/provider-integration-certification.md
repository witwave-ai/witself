# Provider integration certification

This document defines what Witself means when it claims that an AI runtime
integration is supported on an operating system. Building or installing the
`witself` executable is necessary, but it is not sufficient. A supported cell
in the provider/platform matrix must prove that the runtime itself can discover,
start, and use the Witself MCP server without damaging unrelated runtime
configuration.

## Certification levels

Each provider/platform cell has one of these states:

- **contract-tested**: native tests prove install, reinstall, rollback,
  uninstall, and preservation using a portable fake runtime CLI and an
  isolated user home. Provider discovery is reported separately until PATH and
  vendor-default locations are exercised on that cell.
- **client-tested**: the vendor runtime is installed on the target platform and
  successfully initializes the configured Witself MCP server.
- **model-tested**: an authenticated vendor runtime starts a fresh task and its
  model successfully invokes `witself.self.show` through the installed MCP
  binding.
- **not applicable**: the vendor does not provide a supported runtime for that
  platform. WSL is recorded as Linux and never relabeled as native Windows.
- **unsupported**: the cell has not met the applicable acceptance gates.

Only **model-tested** cells are advertised as end-to-end certified. A release
may describe a cell as contract-tested or client-tested when the lower level is
useful, but it must not shorten either state to "certified."

## Platform eligibility is not certification

`witself integrations` reports the implementation boundary for the current
operating system before it probes a provider. This determines whether an
individual install may run and whether `install all` selects or skips the
runtime; it is not evidence that a provider/platform cell reached one of the
tested levels above.

The current boundary is:

| Runtime | macOS | Linux | Native Windows | Witself transcript hooks |
| --- | --- | --- | --- | --- |
| Codex | Native | Native | Native | macOS/Linux and user-scoped Windows |
| Claude Code | Native | Native | Native core MCP/routing | macOS/Linux only |
| Grok Build | Native | Native | Native core MCP/routing | macOS/Linux only |
| Cursor | Native | Native | WSL-only | macOS/Linux, including WSL as Linux |
| OpenClaw | Native | Native | Native | None |
| Antigravity | Native | Native | Native | None |
| GitHub Copilot CLI | Native | Native | Native | None |

Cursor's published Windows CLI path is treated as WSL, not as native Windows.
Witself and Cursor must be installed inside the same WSL distribution. A Linux
Witself process refuses a selected Windows PE provider executable so Windows
interop cannot silently split the Witself and provider configuration
namespaces. Native Windows Claude Code and Grok Build still receive the exact
MCP binding and managed routing; Witself omits their hooks until their hook
command fields have a validated native Windows execution contract.

## Credential-free contract

The complete per-provider harness uses the following sequence on native runners
with disposable home, configuration, and Witself directories:

1. Install a portable fake vendor CLI that records exact argument boundaries
   and implements only the provider commands exercised by that integration.
2. Seed unrelated MCP, hook, rule, and instruction entries in the runtime's
   normal user configuration.
3. Run `witself install <runtime>` for an exact account, realm, agent, and
   location against a local fake Witself identity endpoint.
4. Verify that the runtime owns exactly one enabled Witself MCP binding, that
   its stdio command is the persistent Witself executable, and that no agent
   token or secret was copied into runtime configuration.
5. Verify provider-specific hooks and routing instructions, including native
   Windows commands when the runtime supports them.
6. Repeat the installation and verify idempotence. Provider-specific expansion
   tests change selected executables or binding inputs where the runtime stores
   them and verify the documented safe refresh or refusal behavior.
7. Inject a provider-registration failure after local mutations and verify
   that both the prior Witself binding and unrelated runtime configuration are
   restored. Lower-level transaction tests cover the individual file-mutation
   and concurrent-edit boundaries.
8. Run `witself integrations --verify` and require a healthy exact topology.
9. Run `witself uninstall <runtime>`, verify that only Witself-owned MCP, hook,
   rule, instruction, and integration records are removed, then require
   `not_installed` from a second inventory verification.

The native installer and platform-primitives matrix is:

| Target | GitHub runner |
| --- | --- |
| Linux x64 | `ubuntu-latest` |
| Linux ARM64 | `ubuntu-24.04-arm` |
| macOS Intel | `macos-15-intel` |
| macOS Apple Silicon | `macos-15` |
| Windows x64 | `windows-latest` |

Windows ARM64 remains a separate expansion after Windows x64 is green. WSL
acceptance uses a Linux installation inside WSL and records whether the vendor
runtime and its configuration also live inside that same WSL environment.

The exact release-artifact chain supplies compiled fake provider CLIs explicitly
so transaction behavior never depends on whatever happens to be installed or
authenticated on the runner. On each matrix runner it derives a CLI-only build
from the release configuration. macOS and Linux feed the exact
GoReleaser-produced native tarball and its unmodified checksum set through the
shell installer; Windows feeds the exact native ZIP and checksum set through
the PowerShell installer. The matrix then runs the applicable provider install,
reinstall, verification, uninstall, and sibling-preservation contracts through
that installed command.

The Codex contract additionally initializes the registered MCP stdio command,
lists tools, invokes read-only `witself.self.show` against a local authenticated
backend, and exercises one-shot registration failure and rollback. The shell
and PowerShell installer smokes also cover checksum refusal, staged and
installed self-tests, pair repair, contention, and failed-upgrade rollback.

Credential-free lower-level contracts also cover the other providers'
ownership boundaries. Codex, Claude Code, Grok Build, and Cursor pin the exact
provider CLI, configuration root and MCP path, Witself command/arguments, and
absolute non-secret `WITSELF_HOME`; they refuse a foreign `witself` entry and
selector, CLI, root, home, symlink, or binding drift. OpenClaw pins its resolved
default or selected state directory, configuration file, optional profile, and
`WITSELF_HOME`. Antigravity and Copilot retain their collision-resistant
exact-owned bindings. Every provider install/uninstall is fenced by one
provider-root whole-operation lock, including native Windows locking with a
protected user DACL and reparse-point refusal.

`witself integrations --verify` is intentionally read-only. If it finds a
pending provider transaction, it reports the integration as incomplete and
directs the operator to rerun install or uninstall; those mutating commands
perform recovery while holding the provider operation lock.

The current native Cursor contract selects `cursor-agent` (or an explicitly
configured executable) only when `mcp --help` succeeds and contains `Manage MCP
servers`; MCP operations use `cursor-agent mcp ...` directly. Cursor's effective
user MCP root is `~/.cursor`, and installation rejects `CURSOR_CONFIG_DIR`
because current Agent builds ignore it. Claude Code registration uses the
structured `claude mcp add-json --scope user` surface, avoiding the variadic
`-e`/`--env` parser and preserving one exact native binding payload.

The same native matrix executes installed-artifact lifecycle contracts for
Claude Code, Grok Build, Cursor, OpenClaw, Antigravity, and GitHub Copilot.
Cursor skips native Windows because its supported Windows contract is WSL-as-
Linux. These cells prove the Witself command, provider adapter, exact owned
configuration, recovery journal, and uninstall path as one credential-free
chain. They use deterministic fake provider CLIs or isolated config surfaces;
only the Codex cell currently crosses the additional MCP stdio initialization
and tool-invocation boundary. None of these gates replaces a real vendor
process, authenticated account, or model-level acceptance.

## Real-client acceptance

Real-client tests run manually, on a schedule, or as a release gate. They do
not run on every pull request, cannot be replaced by a fake CLI, and never use a
developer's personal runtime profile. No provider account is required for the
credential-free contract; a dedicated QA account is required only when the
client or model gate actually needs vendor authentication.

For each vendor-supported platform:

1. Start from a clean VM snapshot or dedicated test host with the exact vendor
   runtime version recorded.
2. Authenticate a low-privilege provider QA account when the vendor requires
   authentication. Keep credentials in the runner secret store or the active
   agent's sealed Witself inventory; never write them to the repository,
   runtime configuration, test artifacts, or logs.
3. Install the matching released Witself artifact and run
   `witself install <runtime>` for a dedicated Witself test agent.
4. Use the vendor runtime's own MCP status surface to prove that the Witself
   server initializes and exposes the expected tool inventory.
5. Start a new task and require one exact `witself.self.show` invocation. Assert
   the value-free schema, runtime, test-agent identity, and location rather than
   relying on prose from the model.
6. Reinstall, start another fresh task, and repeat the exact invocation.
7. Uninstall, verify that Witself is absent, and verify that seeded unrelated
   runtime configuration is unchanged.

GUI-backed runtimes use dedicated persistent VMs when a hosted runner cannot
provide the required desktop or signed-in session. A provider-side outage or
authentication failure is recorded separately from an integration defect.

## Real-client acceptance order

The credential-free release-artifact chain covers every provider listed below.
Authenticated real-client and model-level acceptance is still expanded from
Codex first because its CLI, desktop app, and IDE share the same user MCP
configuration:

1. Codex
2. Claude Code
3. Grok Build
4. GitHub Copilot CLI
5. OpenClaw
6. Antigravity
7. Cursor

The order is not a support claim. Each provider/platform cell is advertised as
model-tested only after its own real-client evidence passes. Cursor on Windows
remains WSL-only unless the vendor publishes a supported native Agent CLI
contract.

## Planned release evidence

Before any cell is advertised as model-tested, the release certification job
will emit one value-free JSON record per cell containing:

- Witself version and commit;
- runtime name and version;
- operating system and architecture;
- installation method;
- contract, client, and model test results;
- start and completion timestamps; and
- a bounded failure category when a gate does not pass.

The record contains no credentials, tokens, prompts, model responses, home
paths, or configuration contents. Generating these records and the public
support matrix is a release-gate task; the initial pull-request harness does
not claim to emit them yet.
