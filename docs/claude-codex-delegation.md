# Claude-to-Codex delegation

Witself includes a project-scoped Claude Code skill and a local MCP broker that
lets Claude delegate bounded work to Codex. Claude remains the task owner: it
chooses a lane, supplies the bounded task, checks the returned evidence, and
decides what—if anything—to incorporate.

Every broker-started Codex thread is fixed to `gpt-5.6-sol` with `ultra`
reasoning, multi-agent v2, provider fallback disabled, an ephemeral thread, and
approval policy `never`. Codex cannot override those choices through task text
or MCP arguments.

## Access profiles

The launch-time ceiling and Claude's current permission mode jointly determine
which tools are available:

| Ceiling | Codex work | Required Claude mode | Effect |
| --- | --- | --- | --- |
| `repository` | Independent review | Any supported read/plan mode | Reads the exact repository and private scratch; cannot edit the worktree or use network. |
| `isolated-write` | Review or implementation | `acceptEdits`, `auto`, or `bypassPermissions` to start implementation | Writes only a broker-owned disposable full-history clone; returns bounded patch and evidence artifacts; never applies them. |
| `system` | Review, isolated implementation, or system work | `bypassPermissions` to start system work | Gives Codex the launcher's current OS-user filesystem, process, inherited-environment, toolchain, and host-network access. Direct repository and authorized external effects are possible. |

The ceiling is immutable for one Claude process. Claude can lower its current
permission mode and thereby lose the ability to start elevated work, but no
prompt, repository file, hook result, or tool argument can raise the ceiling.
Status, artifact-read, and cancel calls remain available after a mode downgrade
so Claude can safely finish managing work already started.

Access is not authorization. In particular, the `system` ceiling does not turn
a read-only audit request into permission to deploy, delete, publish, change an
account, or contact a third party. Claude must still keep every delegated action
inside the user's current task.

## Starting Claude

### Repository review in an ordinary session

The checked-in `.mcp.json` exposes only the `repository` ceiling. Start a fresh
Claude Code session at the repository root, open `/mcp`, and approve
`codex-local` after checking that it points at this checkout. Then invoke:

```text
/codex-delegation Review this change for concrete production correctness,
security, recovery, and test gaps.
```

Project MCP approval is intentionally required once per trusted checkout.

### Elevated launcher

Use the checked-in launcher when Claude should be able to delegate edits or
same-user system work:

```bash
# Review plus disposable-clone implementation. This is the default edit mode.
scripts/claude-codex.mjs --ceiling isolated-write -- --continue

# Same-user host access for both Claude and an explicitly delegated Codex task.
scripts/claude-codex.mjs --ceiling system -- "continue the production-readiness work"

# Inspect the exact launch configuration without starting Claude.
scripts/claude-codex.mjs --ceiling system --inspect
```

`repository` defaults to Claude `plan` mode. `isolated-write` defaults to
`acceptEdits` and also accepts `auto` or `bypassPermissions` through the
launcher's `--permission-mode` option. `system` is fixed to
`bypassPermissions`. Managed Claude flags cannot be smuggled through after
`--`.

The launcher resolves Claude Code once to its canonical executable, verifies
its version and required flags, and rechecks that exact file identity before
spawning it. Set `CLAUDE_CODE_EXECUTABLE` only to a canonical absolute path
when a non-default Claude installation is required; a relative override is
rejected.

The elevated launcher supplies a strict private MCP configuration containing
only `codex-local`. It also passes an explicit empty ordinary setting-source
list, so mutable Claude user, project, and local settings or hooks are excluded
while the launcher's generated private `--settings` hook remains active.
Admin-managed Claude policy still applies as a host trust boundary. Claude's
browser state, connectors, plugins, and other MCP sessions are not transferred
to Codex. A system Codex instead gets the host capabilities available to the
same current user; macOS TCC, keychain prompts, `sudo`, provider policy, and
network controls still apply.

Do not launch another Claude or Codex CLI recursively from a delegated task.
Changing ceilings requires ending the current launcher and starting a new one.

## Broker protocol

The broker exposes four repository tools in every profile:

- `codex_runtime_probe`
- `codex_review_start`
- `codex_review_status`
- `codex_review_cancel`

The `isolated-write` and `system` ceilings additionally expose:

- `codex_implementation_start`
- `codex_implementation_status`
- `codex_implementation_artifact_read`
- `codex_implementation_cancel`

Only the `system` ceiling exposes:

- `codex_system_start`
- `codex_system_status`
- `codex_system_cancel`

Tool schemas accept only the task, job ID, or bounded artifact range required
by that operation. They never accept a model, effort, working directory,
environment, executable, permission profile, approval setting, configuration
override, network switch, or arbitrary timeout.

Each status tool accepts an optional integer `wait_seconds` from 0 through 30.
Claude should use `30` while a job is active: the call returns early when the
job becomes terminal, otherwise it returns the current bounded status at the
deadline. This prevents rapid polling from exhausting Claude turns while a Sol
Ultra job is still reasoning; it does not extend the broker-owned job timeout.

Every elevated call requires a fresh one-use HMAC grant created by the private
launcher hook from Claude's actual `PreToolUse` event. The grant binds the
static ceiling, exact tool, current Claude permission mode, tool-use ID,
session, complete original input, timestamp, and nonce. The broker consumes it
before starting asynchronous work and rejects missing, expired, altered, or
replayed grants. This makes hook failure fail closed; it is not a separate OS
principal boundary against a deliberately malicious process already running as
the same user.

## Current Codex, Sol, and Ultra

Before every new probe or job, the broker queries the official npm registry for
the current `@openai/codex@latest` package and its matching native platform
package. For a new broker process it:

1. resolves exact versions and registry-provided SHA-512 integrity values;
2. creates a private exact-version package lock;
3. installs with lifecycle scripts disabled;
4. validates the generic and native packages, sources, integrities, layout,
   executable, and reported CLI version; and
5. resolves `latest` again after installation so a publication during install
   cannot make the first task stale.

The runtime stays frozen while existing work uses it. Before each later work
item the broker re-resolves `latest`. An exact match may proceed. A new version,
changed integrity, registry failure, or incompatible protocol latches the
broker closed to new work and tells Claude to restart; active work may finish.
There is no cached, system, older-version, cheaper-model, lower-effort, or
provider fallback.

Before a model turn, Codex app-server must advertise exactly one
`gpt-5.6-sol` entry with `ultra` and multi-agent v2. The created thread must
then attest the same model and effort, the frozen CLI version, no provider
fallback, exact working root, exact permission profile, an ephemeral idle
thread, and approval policy `never`. The implementation and system lanes also
run lane-specific behavioral capability probes before their own model turn.

The broker binds both `thread/start` and `turn/start` to one explicit app-server
environment selection: environment ID `local`, the lane's exact working root,
and that same root as its runtime workspace. It never sends an empty environment
list, because the versioned app-server schema defines an empty list as disabling
environment access and therefore removes the model's shell and patch tools. The
broker also checks that `local` is ready, reports the expected canonical working
directory, and has a valid shell before the turn.

The isolated `config.toml` and thread override both pin the stable
`shell_tool`, `unified_exec`, `code_mode_host`, and allowed internal
`multi_agent` features on. Review and implementation additionally pin all
known Codex 0.149 provider, browser, computer-control, plugin, image-generation,
dependency-install/search, MCP-app/elicitation, permission-request,
workspace-dependency, goals, guardian-approval, and web-search feature surfaces
off. This includes the otherwise default-on `image_generation` extension,
which is independent of the thread's filesystem sandbox and network setting.
The constrained preflight requires every ceiling-critical catalog entry to
occur exactly once with its pinned effective value; missing entries or any
externally effectful model feature that comes back enabled fail the job before
a model turn. All other enabled feature names must exactly match the reviewed
0.149 internal/local allowlist, so a new default-on feature in a future
`latest` release also fails closed pending classification.

Machine-managed Witself hooks are an explicit trusted host-policy exception,
not model-granted authority. Both feature maps pin and attest the stable hooks
subsystem on, while inventory permits only zero hooks or the exact root/admin-
managed Witself event, source, handler, and hash shape. These hooks execute host
child processes outside the permission profile. Consequently, constrained
`networkAccess: false` describes model-visible tools, not mandatory managed
hooks, and their presence makes the session non-sterile.
The constrained inventory exposes that condition explicitly as `hooks.sterile`.

Codex 0.149 selects Sol's Code Mode entrypoint from the model's
`code_mode_only` metadata; that selection takes precedence over the separate
under-development `code_mode` feature. Consequently, the broker deliberately
does not enable `code_mode` or `code_mode_only`; it pins and attests the local
Code Mode host plus its nested filesystem/process substrate instead. The
system lane has a different full-access feature ceiling: it pins the local
execution and managed-hook features on and apps, plugins, recommended plugins,
tool suggestion, goals, and guardian approval off. Disabling goals prevents
idle continuation beyond the one granted bounded turn. System otherwise
intentionally does not apply the constrained provider/UI feature shutdown.
Provider extensions such as image generation may therefore remain available when the installed
app-server and model support them, consistent with that lane's explicit
full-access authority.

“Latest” deliberately trusts the current official npm dist-tag and publisher.
Registry integrity protects the exact artifact selected during that lookup; it
does not make the mutable publisher decision reproducible. This is an explicit
developer-tool tradeoff.

## Authentication

The broker reads the current user-owned, mode-private Codex `auth.json` only to
obtain a sufficiently long-lived ChatGPT access token and account ID. It passes
the access token to app-server through the external in-memory token-login RPC,
never copies the refresh token, and requires `account/read` to attest the
expected ChatGPT Pro account. The source authentication file must remain
byte-identical through the operation, and no per-operation `auth.json` may be
persisted. Refresh requests fail the job closed.

## Repository review

A review runs in a root-deny custom permission profile with the exact Git
worktree and Git metadata readable, a private scratch directory writable, and
network disabled. The broker fingerprints Git HEAD, status, every tracked and
nonignored-untracked path, mode, size, and content before and after the turn.
If any source state changes, the report is discarded and nothing is undone.

The runtime behaviorally tests repository/Git reads, private scratch writes,
network denial, and denial of a broker-owned outside sentinel before starting a
turn. These checks substantiate the current runtime's configured profile, but
they are not an operating-system container boundary; see Limitations.

## Isolated implementation

An implementation job creates a private, bounded, full-history clone with a
synthetic baseline that exactly represents the source's tracked and
nonignored-untracked state, including ordinary uncommitted changes and
deletions. It rejects incomplete histories, sparse checkout, submodules or
gitlinks, unsafe paths or modes, escaping or changed symlinks, special files,
unbounded object histories, and state drift during capture.

Codex receives write access only to that clone and private build/cache
directories. The source worktree and unrelated home data are denied, `.git` is
read-only, network is disabled, remotes/hooks/includes/filters are inert, and
model-free probes verify the principal boundaries before the turn.

After the turn—or a cancellation or safe failure—the app-server process tree is
stopped before the broker captures a result. The broker emits an atomic,
bounded binary Git patch and sanitized evidence artifact, records source
divergence, removes the large clone/capture material, and retains only bounded
immutable artifacts for the broker's lifetime. Claude reads artifacts in
base64 chunks with exact size and SHA-256 descriptors.

A no-follow monitor runs from clone creation until the app-server process tree
has stopped. It bounds logical bytes, allocated bytes, entry count, and special
filesystem types and interrupts the job on a violation. The broker also has
fixed lifetime limits for job records and retained artifact bytes; reaching a
limit refuses new work until Claude and the broker restart. The monitor is not
an operating-system filesystem quota, so an instant pathological writer can
outpace one sampling interval. Use an outer quota or disposable container when
that availability boundary must be strict.

Nothing is applied to the source. Claude must decode and hash-check the complete
artifact, inspect every change, reconcile it against the current tree, and run
the relevant checks itself. A patch is advisory evidence, not authority to
change the repository.

Ignored dependency trees and user-global build caches are not copied into the
isolated clone. Local checks that require them may be unavailable. If the
authorized task genuinely requires host credentials, existing caches, network,
or direct effects, use the `system` lane from a system-ceiling launcher instead
of weakening isolation.

## System work

A system job is exclusive: no review or implementation may overlap it in the
same broker. Codex receives the built-in `:danger-full-access` permission
profile, current launcher environment except the broker's grant controls,
live-web mode, the exact repository root, and the current effective user. A
zero-model preflight proves outside-repository reads, private scratch writes,
child-process execution, loopback networking, and effective-user identity.

The system result uses a closed schema that reports actions, checks, repository
or external changes, reversibility, blockers, and warnings. Known access-token
and inherited-environment values are redacted before returning to Claude, but
redaction is best effort. A nonempty blocker list makes system and isolated
implementation action jobs fail instead of presenting an incomplete action as
successful. An action report with neither a completed action nor a verification
check also fails as unverified; warnings alone are not completion evidence.
Those semantic failure statuses retain the bounded redacted report for
diagnosis, while cleanup or finalization safety failures still suppress it.
Review blockers remain part of a successful diagnostic report.
A same-user full-access model can read and transform
host secrets, kill local processes, or make consequential network calls. Use
this lane only when those capabilities are necessary and the user's bounded
task authorizes their use.

## Trust boundary and limitations

- The repository's `.mcp.json`, broker, launcher, skill, and hook execute only
  after the checkout is trusted. Protecting against a malicious initial
  checkout requires an externally installed and independently signed launcher.
- At startup the launcher copies the complete broker runtime plus grant signer
  and hook into a private snapshot, verifies every file, makes it read-only,
  and continuously rechecks it. Ordinary edits to the checkout cannot change
  the running ceiling. A deliberately malicious same-user process can still
  attack same-user state; the HMAC is not a separate-user security boundary.
- On Unix, Claude and its MCP descendants run in a dedicated process group.
  Interrupts and integrity failures drain that group and force-kill it when
  necessary before private-session cleanup. Windows can guarantee only
  bounded direct-child termination with the current launcher, and the current
  restricted/system capability probes fail closed on Windows pending an
  equivalent verified implementation.
- Codex's macOS custom permission profile has not reliably denied reads from
  every unrelated `/private/tmp` subtree. The broker uses private home/temp
  paths, scrubs constrained environments, denies a broker-owned outside
  sentinel, and reports this limitation rather than claiming complete
  host-secret confinement. Strict confidentiality requires an outer OS sandbox
  or container that supplies the required repository and toolchain roots.
- An isolated Codex home still materializes bundled system skills, which the
  broker inventory-attests exactly. Exact machine-managed Witself hooks may
  also remain as a root/admin-policy exception and potential external observer;
  all other hook inventories fail closed. Eliminating the managed hooks
  requires an administrator policy change or an outer OS isolation boundary.
  Every lane reports its environment as non-sterile when they are present.
- The launcher requires Claude Code 2.1.214 or newer and Node.js 22 or newer.

## Local verification

The mandatory suite is hermetic: it uses fake npm, Git repositories, Claude,
and app-server transcripts. It makes no model, provider, cloud, or deployment
calls.

```bash
npm --prefix tools/claude-codex-broker ci
npm --prefix tools/claude-codex-broker test
```

The suite covers strict MCP schemas; frozen ceilings; one-use grants; launcher
process-tree cleanup and snapshot integrity; exact latest package resolution;
Sol/Ultra/no-fallback attestation; external-token handling; review, isolated,
and system capability probes; hostile Git histories and filesystems; bounded
artifacts; cancellation; cleanup; protocol drift; and output limits.

Each live broker performs its own model-free compatibility probes against the
currently resolved Codex CLI before starting work. CI and release gates stay
hermetic and exercise the same protocol with hostile fake transcripts. A
deliberate, disposable real-model smoke may be run before first use, but it is
not part of hermetic CI.
