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
8. Run `witself uninstall <runtime>` and verify that only Witself-owned MCP,
   hook, rule, instruction, and integration records are removed.

The initial native matrix is:

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

The first Codex phase supplies the fake CLI explicitly so it can test exact
transaction behavior without depending on whatever happens to be installed on
the runner. It currently implements the Codex version/capability/add/remove
surface, runs a real built `witself` executable, initializes its registered MCP
stdio command, lists tools, invokes read-only `witself.self.show` against a
local authenticated backend, and exercises one-shot registration failure and
rollback. PATH discovery, executable-drift cases, and a real Codex process are
separate acceptance gates rather than implied by this contract.

Native Windows coverage in this phase is deliberately scoped to the Codex
provider transaction and the platform file/lock primitives that transaction
uses. It does not yet certify all Witself commands or other providers. In
particular, vault/secret custody and curator automation still require
Windows-DACL acceptance, while Antigravity and GitHub Copilot require native
operation-lock implementations before their Windows cells can move out of
unsupported.

## Real-client acceptance

Real-client tests run manually, on a schedule, or as a release gate. They do
not run on every pull request and never use a developer's personal runtime
profile.

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

## Provider order

The certification harness is proven with Codex first because its CLI, desktop
app, and IDE share the same user MCP configuration. The remaining integrations
then adopt the same contract one at a time:

1. Codex
2. Claude Code
3. Grok Build
4. GitHub Copilot CLI
5. OpenClaw
6. Antigravity
7. Cursor

The order is not a support claim. Each provider/platform cell is published only
after its own evidence passes. Cursor on Windows remains WSL-only unless the
vendor publishes a supported native Agent CLI contract.

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
