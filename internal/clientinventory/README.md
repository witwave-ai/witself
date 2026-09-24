# Local client inventory

`Scan(ctx context.Context, Options) (Report, error)` is an explicit, read-only
device check. The caller authenticates the current operator and supplies the
canonical account ID. This package does not authenticate that ID or confer
account authority. Do not call it from polling handlers.

`Options{Home, WitselfHome, DSHHome, AccountID string}` contains private inputs.
Home is required; all supplied roots must be clean absolute paths. Empty
WitselfHome and DSHHome default to `Home/.witself` and `Home/.dsh`. No environment,
home lookup, PATH search, hostname, process inspection, or provider invocation is
used. DSHHome is reserved for future static DSH checks; it is not scanned today.

Discovery reads exactly `WitselfHome/integrations/<runtime>/config.json`, as
persisted by `internal/transcriptcapture/config.go:ConfigPath`. The source audit
also covered `cmd/witself/integrations.go` and the generic, OpenClaw, Antigravity,
Copilot and DSH installation topologies. The existing collector and validators
are not imported or called: even ordinary collection can probe executables.

Only `witself.capture.v1` records whose runtime matches the fixed directory and
whose nonempty account_id exactly matches AccountID qualify. No alias, selector,
legacy record without account_id, or unknown runtime is authority. Account IDs
must contain 1–256 ASCII letters, digits, underscores or hyphens. Filtering
precedes decoding path references or examining any referenced file.

## Public JSON contract

Report has `schema_version: "witself.console.clients.v1"`, `device_label:
"This device"`, UTC RFC3339 `checked_at`, non-null `entries: []`, and
`scan_status`. Rows are ordered: `codex`, `claude-code`, `grok-build`, `cursor`,
`openclaw`, `antigravity`, `copilot`, `dsh`; at most one row per runtime.

| Field | Closed values / meaning |
| --- | --- |
| recorded_version | Recorded install version or empty. At most 48 ASCII bytes: optional `v`, three decimal components (no leading zeroes, at most nine digits each), optional `-alpha`, `-beta`, `-rc` and optional numeric suffix. Cursor alone also accepts `YYYY.MM.DD-<build>`: a valid calendar date with four-digit nonzero-leading year and zero-padded month/day, followed by 7–16 lowercase hexadecimal build characters (for example `2026.07.16-899851b`). No build metadata or arbitrary prerelease/diagnostic text. Never current command output. |
| executable_status | `present`: recorded path is an allowed regular file; `missing`: allowed path absent; `unchecked`: no safe check. Neither present nor missing asserts executable permissions, successful execution, or activity. |
| configuration_status | `match`: exact static MCP registration matches record; `incomplete`: missing pinned fields/file/registration; `changed`: registration differs or is disabled/ambiguous; `unavailable`: inaccessible, oversized, unsafe, or malformed file; `unsupported`: topology or root outside the narrow checker. |
| configuration_scope | `mcp_registration` or `none`. Always display this qualification with configuration status; match does not verify hooks, plugins, instructions, credentials, or effective runtime settings. |
| effective_verification | Always `not_run`. No liveness, online, or healthy assertion. |
| installed_at | Optional normalized UTC timestamp recorded by installer, from year 2000 through checked_at. Invalid/future values are omitted; not independently attested install history. |
| scan_status (report) | `complete`: all fixed record locations examined (foreign and absent records are omitted); `partial`: at least one record could not safely be read/decoded/identified; `unavailable`: root could not safely be opened. This does not mean all configurations matched. |

The projection contains no paths, identifiers, selectors, hostnames, credential
values, provider text, command arguments, or raw filesystem/decoder errors.
Errors are fixed `ErrAccountRequired`, `ErrInvalidOptions`,
`ErrUnsupportedPlatform`, or context cancellation/deadline errors. Cancellation
discards partial results. Other read failures use the closed statuses above.

## Deliberate check limits

Executable checks only stat the recorded exact runtime basename (`codex`,
`claude`, `grok`, `cursor-agent`, `openclaw`, `agy`, `copilot`, `dsh`) under the
caller's `Home/.local/bin`, `Home/.bun/bin`, or `Home/bin`. Other locations,
symlinks and multiply linked files are unchecked; executable bytes are never
read. There is no generic existence oracle for record-selected paths.

Static MCP matching covers Codex and Grok `config.toml`, Cursor `mcp.json`, and
Claude `.claude.json`. Persisted roots must exactly match Home's `.codex`,
`.grok`, `.cursor`, or `.claude`; Claude also supports the persisted default
`Home/.claude.json` location. Custom persisted roots return unsupported without
reading them; ambient selectors never override a persisted selection. The
comparison includes command, arguments, sole WITSELF_HOME environment binding,
enabled/type constraints, and rejects unknown registration fields/case aliases.
OpenClaw, Antigravity, Copilot and DSH records are inventoried but their static
configuration checks are unsupported. No plugin, YAML, workspace, hook, token,
credential or transaction files are read. TokenFile is only a deny reference.
All eligible records are decoded first, and their combined TokenFile exclusions
apply to every runtime's configuration reads and executable probes, regardless
of record order or whether the credential-owning runtime's topology is supported.
Both TokenFile and the allowed path are cleaned and canonically normalized to
Unicode NFC before case folding. Case-equivalent and NFC/NFD-equivalent paths
exclude both configuration reads and executable probes before I/O, conservatively
even on filesystems that distinguish those spellings. This is pure string
comparison: TokenFile is never statted, opened, or resolved.

## Filesystem and resource boundaries

Darwin and Linux use descriptor-relative no-follow traversal. Other platforms
fail closed. The caller owns custody of the absolute roots and their ancestors;
the root itself and every descendant component reject symlinks. Descendant
directory descriptors remain pinned through renames. File reads reject
nonregular/multiply linked/unreadable files, check metadata before opening,
open nonblocking/no-follow, compare opened identity, bound reads, and recheck
identity/size/mode/timestamps after reading. This detects substitutions and
ordinary concurrent edits, not an adversarial OS owner rewriting trusted
regular files in place. Records are local evidence, never authentication.

No directory enumeration or recursive walking occurs. Maximum reads: eight
64 KiB records and four 256 KiB provider files (1.5 MiB total content). JSON
rejects duplicate/folded keys, more than 8192 values, or nesting beyond 24 before
typed decode. TOML has a conservative 8192 structural-character bound before
decode and the pinned parser's nesting limit. Context is checked between paths
and read chunks; the OS can still delay a filesystem syscall. No writes, locks,
repairs, network, credential loading, or external commands occur. Ordinary
filesystem access-time updates can occur for the allowlisted files read.

Tests use synthetic roots exclusively. Run with the repository's Go 1.27.1
toolchain: `GOTOOLCHAIN=local GOPROXY=off /opt/homebrew/opt/go/bin/go test
./internal/clientinventory` and the same command with `-race`.
