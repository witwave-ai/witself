# Sealed-Plane Acceptance

Status: executable acceptance specification. The schema-56 vault-key lifecycle
has an implemented code/database gate below. The larger 21-case live
runtime/cloud certification remains a release target and cannot be claimed
until its harness, deferred runtime injection, and every named live case pass.

This is the release gate for the client-custodied agent vault defined by
[ADR 0003](decisions/0003-client-custodied-agent-vault.md) and the
[Client-Custodied Agent Vault](client-custodied-agent-vault.md) plan. Where an
older sealed-plane document still describes a cloud-KMS vault root or
server-side decryption, ADR 0003 and this gate take precedence.

## Implemented schema-56 lifecycle gate

Run these repository checks from the root of the checkout. The PostgreSQL
command targets a disposable local database and specifically proves both the
schema-55-to-56 constraint transition and the atomic staged rotation flip:

```sh
go test ./...
go vet ./internal/store ./internal/client ./internal/server \
  ./cmd/witself-server ./internal/secretclient
WITSELF_TEST_DATABASE_URL='postgres://witself:witself@localhost:5432/witself?sslmode=disable' \
  go test ./internal/store \
  -run '^(TestMigration56ReplacesHistoricalVaultKeyVersionConstraintPostgres|TestVaultKeyRotationStagesAndFlipsAtomically|TestSensitiveCreateReplayPrecedesVaultKeyMismatchAcrossRotation)$' \
  -count=1 -v
```

Release review must also cover the implemented lifecycle contract:

1. On a clean target installation, run `witself vault key enroll begin`; on an
   installation that already has the matching AVK, run `witself vault key
   enroll approve ENROLLMENT_ID`; then finish on the target with `witself vault
   key enroll complete ENROLLMENT_ID`. Verify both installations can reveal the
   same synthetic field and that the transfer capsule is cleared after
   consumption.
2. Verify `enroll list`, `enroll status`, expiry, and `enroll cancel` expose
   only value-free lifecycle state. Pairing material must use the controlling
   TTY; attempts through argv, environment variables, stdin/pipes, JSON, or MCP
   must have no supported input path.
3. Run `witself vault key recovery export --out FILE`, inspect it offline with
   `witself vault key recovery inspect --file FILE`, and restore it in a clean
   matching installation with `witself vault key recovery import --file FILE`.
   Verify the output path is never overwritten, wrong passphrases/scopes fail
   closed, and passphrases use hidden controlling-TTY input only.
4. Run `witself vault key rotate --recovery-out FILE`, interrupt and re-run it
   during a disposable fixture to prove open-run discovery/resume, then verify
   `witself vault key rotation status [ROTATION_ID]` reports the exact
   `source + 1` epoch, full item count, `recovery_artifact` disposition, and
   artifact SHA-256. Verify the artifact was durably written, read back,
   inspected, and decrypted against the exact target/scope before commit; an
   exact existing artifact resumes idempotently, while wrong passphrase, scope,
   target, malformed bytes, or unavailable storage prevents commit. Also verify
   zero/both recovery choices fail before remote work and that
   `--accept-unrecoverable-key-loss` records only `risk_accepted`. Sensitive
   create remains blocked while open, commit is atomic, the old local epoch
   remains, and another installation must enroll again.
5. Exercise `witself vault key rotation cancel [ROTATION_ID]`, then retry the
   rotation. The retired candidate must remain as history while a fresh key id
   may reuse the same logical target version. Any recovery artifact already
   published for the retired candidate remains inert and never overwritten or
   deleted automatically; it must fail recovery against the later binding, and
   the retry must select a fresh output path.
6. Verify account export, irreversible account close, and deletion of the
   affected agent are rejected while an enrollment is pending/approved or a
   rotation is open. Suspend the account, cancel the work through the safety
   path, and verify on disposable clones that those operations are unblocked.
   A terminal schema-56
   archive must contain public AVK bindings, ciphertext/wrapped DEKs, terminal
   lifecycle history, and receipts,
   but no AVK, local key file, live transfer/staging capsule, pairing secret,
   passphrase, or recovery artifact. After cell import, supply the AVK
   separately by protected key transfer, recovery import, or enrollment.
7. Race one journaled sensitive create against an open rotation. Verify the
   open conflict does not change the journal or create a receipt. After commit,
   verify the client submits and authenticates the exact old request first,
   accepts only HTTP `409` plus `secret_vault_key_mismatch` as rebase authority,
   DEK-rewraps without changing ciphertext or generated ids/value, durably CAS
   publishes before send, and can decrypt the result under the target AVK.
   Also prove accepted-before-rotation response loss replays exactly, response
   loss after CAS replays the replacement after a later rotation, successive
   uncommitted epochs advance each wrap revision, concurrent CAS contenders
   send one winner, and generic/idempotency/transport failures never mutate the
   journal. Missing retired keys, overflow, unsafe storage, and failed CAS must
   fail closed. An old client must leave the journal intact and fail safely.

All of these checks are provider-neutral. AWS, Google Cloud, and Azure run the
same binary, schema, wire algorithms, and archive format; cloud KMS is not an
agent-vault dependency. The Witself backend performs no AI/model inference and
never receives an AVK, enrollment private key, pairing secret, recovery
passphrase, or plaintext sealed value.

The rest of this document defines the larger certification target. Passing the
schema-56 lifecycle gate alone is not a claim that the 21 live cases below have
run.

## Certification claim

A passing release proves all of the following for one fresh synthetic named
agent:

- Codex, Claude Code, Cursor, and Grok Build can use the same logical vault on
  AWS, Google Cloud, and Azure without changing the envelope format;
- an account archive moves losslessly in every directed source/destination
  cloud pair, and the imported vault remains usable while the source is
  unavailable;
- the bearer token and Agent Vault Key (AVK) remain independent;
- PostgreSQL, the Witself API, logs, audit, and account archives never contain
  an AVK or plaintext sensitive value;
- inventory is redacted, while deliberate reveal, TOTP calculation, and
  runtime injection happen only in the active client; and
- Witself-owned memory, facts, messaging, avatar, hydration, and transcript
  paths do not acquire sealed material as a side effect of agent use.

The gate does not claim that plaintext is absent from the active client's
process memory, an explicitly selected reveal sink, a child process receiving
an injected value, the destination service, or the AI provider after a user
deliberately enables and invokes a value-returning provider tool. Those are the
client-custody boundary described by ADR 0003. Certifying live-runtime stages
keep value-returning MCP tools disabled and use side-effect-oriented injection
so that no plaintext needs to enter model context.

## Required matrices

Certification consists of exactly 21 required cases: 12 live runtime/cell
cases and nine directed archive/import cases. A skipped, expected-failure,
rehearsal, or partial case does not count.

### Live runtime by cloud: 12 cases

| Runtime | AWS | GCP | Azure |
| --- | --- | --- | --- |
| Codex | required | required | required |
| Claude Code | required | required | required |
| Cursor | required | required | required |
| Grok Build | required | required | required |

The fixed case id is `runtime_<runtime>_<cloud>`. Runtime keys are `codex`,
`claude_code`, `cursor`, and `grok_build`; cloud keys are `aws`, `gcp`, and
`azure`. Their Cartesian product produces the 12 case ids without aliases.

Each case uses the real authenticated runtime, the released CLI/MCP adapter,
the deployed server API, and the managed PostgreSQL cell named by the column.
It is not satisfied by a mocked model, a local PostgreSQL alias, or a direct
store test.

### Directed archive/import: nine cases

| Source | AWS destination | GCP destination | Azure destination |
| --- | --- | --- | --- |
| AWS | `move_aws_aws` | `move_aws_gcp` | `move_aws_azure` |
| GCP | `move_gcp_aws` | `move_gcp_gcp` | `move_gcp_azure` |
| Azure | `move_azure_aws` | `move_azure_gcp` | `move_azure_azure` |

The diagonal cases are distinct source and destination account/schema
instances on the same managed-cloud endpoint. They prove backup/restore and
same-provider relocation. The six off-diagonal cases prove cross-cloud
portability. These nine cases exercise the provider-neutral CLI/API/archive
layer; the separate 12-case matrix proves the four runtime adapters. The
release gate is 12 plus nine, not an implied 36-case Cartesian product.

## Synthetic fixture and private markers

Use a fresh account, realm, named agent, and peer agent created only for this
gate. Never run certification against a person's normal agent. The same
logical subject agent and AVK are used throughout the 12 runtime/cell cases;
each runtime/cell binding receives an independently issued bearer token.

Preparation creates one GitHub-shaped synthetic secret with:

- a public secret name and template;
- public `username` and `login_url` fields that must remain searchable;
- sensitive `password`, `api_key`, and `recovery_code` fields;
- one sensitive TOTP payload using a verifier-controlled clock; and
- distinct high-entropy canaries for every sensitive value.

The private state records the sensitive fixture canaries, deterministic TOTP
output for the fixed test time, and old/new token identifiers needed to drive
the cases. It does not duplicate AVK or DEK bytes. When scanning, the trusted
verifier reads the AVK from the scoped key file into process memory and adds its
representations to the forbidden set. The state file is mode `0600`, is never
committed or uploaded, and is deleted only after evidence verification or
retained in a protected diagnostic location for a failed run.

The forbidden-marker set contains, at minimum:

- raw UTF-8 and raw binary bytes;
- JSON-escaped and URL-encoded forms;
- padded and unpadded standard and URL-safe Base64 forms; and
- lowercase and uppercase hexadecimal forms.

The scanner also treats every old, current, replacement, and revoked raw
bearer token as forbidden. Authorization headers are never persisted in API
traces; the collector retains only allowlisted headers and replaces the header
with a fixed presence boolean before scanning.

Public fixture values are not forbidden markers because the product
intentionally indexes and returns them. The sanitized evidence nevertheless
records only counts and case identifiers, not the public fixture values.

The certifying preparation flow uses the implemented `vault key enroll
begin|approve|complete` ceremony to provision each durable installation. A
trusted controller may copy an already-created opaque `.key` into throwaway
isolated homes only as a non-certifying fixture shortcut for tests whose subject
is not enrollment. Such copying does not satisfy the lifecycle gate. In either
case, AVK bytes must never pass through a prompt, MCP argument/result, message,
transcript, shell argument, or account archive.

## Required environment

A certifying run requires:

- one release-identifiable `witself-server` deployment in each cloud;
- the same server version, commit, schema version, and archive format in all
  three cells;
- the matching released `witself` CLI installed for every runtime;
- protected API credentials and a read-only database inspection credential for
  each cell;
- a protected way to collect complete Witself server logs for the run window;
- one immutable cloud-resource attestation for each cell; and
- a disposable trusted runner that can reach all three APIs and databases.

The concrete secret-bearing environment variables are:

```text
WITSELF_SEALED_AWS_SERVER_URL
WITSELF_SEALED_AWS_DATABASE_URL
WITSELF_SEALED_AWS_RESOURCE_ID
WITSELF_SEALED_GCP_SERVER_URL
WITSELF_SEALED_GCP_DATABASE_URL
WITSELF_SEALED_GCP_RESOURCE_ID
WITSELF_SEALED_AZURE_SERVER_URL
WITSELF_SEALED_AZURE_DATABASE_URL
WITSELF_SEALED_AZURE_RESOURCE_ID
WITSELF_SEALED_CLOUD_CERTIFY=1
```

Bearer tokens, operator credentials, and log-reader credentials are supplied
through protected token files or the runner's secret store, never as command
arguments. The harness retains only per-run salted endpoint fingerprints and
database versions. Resource attestations are used at runtime to reject aliased
cells and are not copied into sanitized evidence.

Certification rejects duplicate resource attestations, duplicate configured
endpoint identities, a development version, mismatched CLI/server commits, an
older schema, an existing non-synthetic subject, or missing raw-log/DB
inspection access. A run without cloud certification mode is a rehearsal and
can produce only `pass_rehearsal`.

## Harness interface

The release implementation must expose this operator flow or an exactly
equivalent versioned interface documented by the release:

```text
witself sealed acceptance prepare \
  --runtimes codex,claude-code,cursor,grok-build \
  --clouds aws,gcp,azure \
  --agent sealed-acceptance-bot

witself sealed acceptance prompts \
  --state ~/.witself/acceptance/spa_<run>.json \
  --case runtime_codex_aws

witself transcript flush --runtime grok-build  # after the final Grok case

witself sealed acceptance verify \
  --state ~/.witself/acceptance/spa_<run>.json \
  --server-logs-private-dir <protected-directory> \
  --out evidence/sealed-plane-acceptance.json
```

`prepare` prints only the private state path and value-free next steps. It
creates the state before its first mutation and updates it idempotently after
each mutation so a crash can resume the same run. `prompts` emits exact
stage-specific prompts for one real foreground runtime. Each prompt is used in
a new provider session unless the stage explicitly verifies a single-session
tool boundary. Witself never launches or wakes a provider.

The harness runs all nine move cases during `verify`, or exposes a resumable
equivalent:

```text
witself sealed acceptance move \
  --state ~/.witself/acceptance/spa_<run>.json \
  --source aws \
  --destination gcp
```

Raw API traces, database dumps, expanded archives, logs, reveal captures, and
transcripts are private verification inputs. They are not the evidence
artifact. Every private file is created mode `0600` under the protected run
directory.

## Per-runtime/cloud execution

Every one of the 12 live cases runs these stages in order. A fresh isolated
`WITSELF_HOME` and account clone prevent a failure injection in one case from
affecting another.

1. `identity_and_capabilities`

   Operation: observe the real runtime/version, token-derived
   account/realm/agent, and tool list.

   Pass: runtime and version match the installed record; identity equals the
   synthetic subject; no owner override is accepted; `server_side_decrypt` is
   absent; value tools are absent when `--no-value-tools` is active.

2. `key_bootstrap`

   Operation: from one designated clean installation, perform the first
   sensitive create. Other cases consume the opaque copied test key.

   Pass: a single version-1 AVK is generated locally; directories are `0700`;
   the key is a regular `0600` file; creation neither follows nor overwrites a
   symlink; the token file is unchanged; and the API receives only key
   id/fingerprint/version metadata. Concurrent attempts converge on the same
   file.

3. `create_and_search`

   Operation: generate a password locally, create the structured fixture, and
   search by public username and URL.

   Pass: the password satisfies the documented generator policy; each public
   field finds the expected secret; every sensitive-canary search finds zero;
   API and database inspection show envelopes and no public value in sensitive
   branches.

4. `redacted_inventory`

   Operation: run list, search, and show through the runtime adapter.

   Pass: sensitive fields have only redacted presence/type metadata. No value,
   envelope, nonce, wrapped DEK, unexpected AVK metadata, or TOTP code enters
   model-visible output. Public username and URL are returned as designed.

5. `direct_reveal`

   Operation: outside model context, have the verifier invoke explicit CLI
   reveal for exactly the password field and privately capture stdout/stderr.

   Pass: stdout is byte-for-byte the requested value plus only the documented
   line terminator; no sibling value is returned; stderr and structured errors
   are value-free; the access response has `Cache-Control: no-store` and
   exactly one authorized envelope, never plaintext.

6. `runtime_injection`

   Operation: the real runtime uses `witself run` or an equivalent
   non-value-returning reference path to launch the supplied child verifier.

   Pass: the child receives the exact value and emits only the fixed non-secret
   success marker. The secret is absent from child argv, the parent
   environment, model-visible output, and transcript. No temporary plaintext
   file remains, and the child exits zero.

7. `totp_local`

   Operation: with the clock fixed, locally decrypt the TOTP payload and invoke
   the supplied verifier without displaying the seed or code to the runtime.

   Pass: the code equals the private expected RFC-vector result for the
   algorithm, digits, and period. The backend receives neither seed nor code,
   and neither appears in transcript, logs, audit, or evidence. URI/Base32
   enrollment is required; optional QR decoding is not yet a blocker.

8. `open_plane_use`

   Operation: after injection, direct the runtime to record a non-secret
   success memory, send a non-secret completion message to the peer, and update
   only the supplied non-secret avatar marker.

   Pass: the non-secret markers exist in the intended surfaces, and every
   forbidden marker has zero matches in memory/fact/curation, messaging,
   avatar, hydration, and transcript inspection.

9. `missing_key`

   Operation: move the key file to a protected harness-only location and retry
   inventory, create, reveal, TOTP, and injection.

   Pass: redacted inventory still works; every sensitive create/use fails with
   the stable value-free missing-key class; no new AVK is generated for the
   registered vault; no token-only or server-decrypt fallback exists.

10. `wrong_key`

    Operation: install a syntactically valid nonmatching AVK in the isolated
    test home and retry sensitive use.

    Pass: the client rejects the key mismatch or AEAD authentication with one
    value-free integrity class; no plaintext is returned; registered key
    metadata and stored envelopes remain unchanged; no fallback occurs.

11. `tamper`

    Operation: in a disposable clone, flip a ciphertext bit, flip a wrapped-DEK
    bit, swap one immutable AAD binding, and submit an unsupported
    algorithm/version.

    Pass: every bit flip or binding swap fails AEAD authentication; unsupported
    metadata fails before decrypt; errors are value-free and content-agnostic;
    no partial update or plaintext result occurs.

12. `token_rotation`

    Operation: privately hash the key and envelopes, issue a replacement token,
    revoke the old token, and repeat redacted and sensitive operations.

    Pass: the new token plus unchanged AVK works; the revoked token receives an
    authorization failure and no envelope; key bytes, public AVK
    fingerprint/version, ciphertext, and wrapped DEKs remain byte-for-byte
    unchanged; token material never becomes a key input.

13. `audit_and_leak_scan`

    Operation: flush transcripts, collect API traces/logs, inspect PostgreSQL,
    and inspect all Witself surfaces created since preparation.

    Pass: value-free events and usage increments have exact expected counts;
    prohibited fields are absent; all forbidden-marker scans return zero
    outside explicitly authorized private sinks.

The missing-key, wrong-key, and tamper stages restore the original private key
and pristine encrypted fixture only through harness-held opaque bytes. The
restoration operation is not a product recovery path and is never presented to
the runtime or backend.

## Directed cloud-move execution

Each of the nine move cases uses a fresh clone of the synthetic account and
runs this exact sequence:

1. On the source, verify redacted inventory, direct CLI reveal, local TOTP, and
   runtime injection with the matching AVK.
2. Confirm no enrollment is pending/approved and no rotation is open, suspend
   the source account, and export one logical snapshot. A separate recovery
   artifact may be prepared before suspension but is not placed in the archive.
3. Expand and validate the archive in the protected runner. Record private
   per-stream row counts and byte digests for key metadata, secrets, fields,
   wrapped DEKs, mutation receipts, audit, and usage.
4. Import into a freshly migrated destination account/schema using the same
   logical ids. Rebuild derived search indexes.
5. Fence the source: remove the client's source endpoint binding and block or
   observe zero source API connections for the remainder of the case. Revoke
   source test tokens when the source and destination use independent
   authorization stores.
6. Point an installation already holding the matching `.key` at the destination,
   or separately restore the exact matching AVK from the recovery artifact.
7. Repeat redacted search by public username/URL, one-field direct reveal,
   local TOTP verification, and runtime injection.
8. Export the destination and privately compare every canonical encrypted
   envelope and wrapped-DEK byte sequence with the source archive.
9. Run the complete database/API/log/archive/open-plane leak scan on both
   sides and retain only sanitized booleans and counts.

A move case passes only when:

- the source and destination archive manifests are complete for schema 56 and
  contain every sealed-plane and lifecycle stream, including empty streams;
- row counts and immutable ids match, and all ciphertext, wrapped DEKs,
  algorithms, versions, AAD-binding columns, and public key metadata are
  byte-for-byte unchanged;
- the archive contains no AVK, local key file, enrollment private key, live
  transfer/staging capsule, recovery artifact/passphrase, token, plaintext
  sensitive value, TOTP seed, or generated TOTP code;
- destination search/redaction results match the source;
- destination reveal, TOTP, and injection succeed with the unchanged local
  AVK;
- the post-fence trace contains zero calls to the source API and Witself makes
  no agent-vault KMS call or cloud-specific re-wrap operation;
- importing with no key still succeeds as an encrypted archive but sensitive
  use fails with the missing-key class;
- importing with the wrong key never yields plaintext; and
- every source and destination leakage count is zero.

For a diagonal move, source fencing is an account/schema and credential fence
rather than shutting down the shared managed endpoint. The destination must
still be a distinct import target and must not read source rows.

## Storage and transport inspection

The verifier performs structural assertions and forbidden-marker scans. A
plain `grep` of pretty-printed JSON is insufficient.

### Local key custody

Inspect with `lstat`, permission and symlink checks, the durable-write and
concurrency test, and private byte comparison. Outside verifier process memory,
only scoped `.key` files may contain AVK bytes. Ordinary output exposes public
status only, and token files remain unchanged.

### PostgreSQL

Take a transactionally consistent raw export of every synthetic-account column
and inspect the catalog for sealed-table constraints. Public branches contain
only public values; sensitive branches contain nonempty ciphertext and wrapped
DEKs only. No AVK, plaintext DEK, sensitive canary, seed, or code may appear in
any column, search document, indexable expression, receipt, event, or usage row.

### API

Capture serialized HTTP bodies, error bodies, and allowlisted headers at the
trusted client transport boundary. Reduce authorization to a presence boolean
before persistence. Create/update transports envelopes, and access returns one
envelope with `no-store`. No retained request/response contains a raw token,
AVK, or plaintext. No decrypt route or `server_side_decrypt` capability exists.

### Server, client, MCP, and hook logs

Collect complete raw logs for the run window, including failure paths. No AVK,
token, raw retry key, public field value when avoidable, envelope, nonce,
wrapped DEK, plaintext, seed, or code may appear. Errors expose only closed
value-free classes.

### Account archive

Scan compressed bytes and every expanded manifest/stream, then structurally
validate dependency order and columns. The schema-56 archive is complete and
ciphertext-only, with public AVK bindings and terminal enrollment/rotation
history but no local key material, recovery artifact, live lifecycle capsule,
plaintext, unrecognized stream, or omitted sealed/lifecycle stream.

### Audit and usage

Read canonical events, usage events, ledger, and rollups. Require one successful
encrypted-material-delivery event per successful access response; rejected
requests do not claim delivery. Local outcomes, if emitted, are separately
named and value-free. Exact usage counts match successful reveal, TOTP, and
injection operations without counting redacted inventory as reveal.

### Support and metrics evidence

Inspect exported diagnostics and metric labels. Only bounded ids,
operation/result classes, counts, sizes, algorithm/key versions, and
provider-neutral status may appear. Labels and bundles contain no forbidden
marker or envelope material.

Audit schemas are closed allowlists. They may include stable synthetic resource
ids, operation, result class, sensitivity boolean, algorithm/key version,
bounded size, and timestamps. They must reject secret names when avoidable,
field values, public-value search queries, ciphertext, nonces, wrapped DEKs,
raw idempotency keys, AVK fingerprints when not required by the event, tokens,
TOTP seeds, and TOTP codes. An envelope-delivery event proves authorization and
delivery of encrypted material; it must not claim that client decryption
succeeded.

## Redaction and authorized plaintext sinks

The suite distinguishes a deliberate plaintext sink from a leak.

The only permitted persisted plaintext test captures are:

- the mode-`0600` private preparation state used by the verifier; and
- the mode-`0600` direct-CLI reveal capture used for byte equality, deleted or
  protected after verification.

The active client's memory and the child verifier's environment necessarily
contain plaintext transiently. The child prints only a fixed success marker and
does not write the value. No other file, stream, table, log, prompt, tool
result, evidence document, or archive is an authorized sink.

The live runtime is started with `--no-value-tools`. The verifier separately
tests the reveal and TOTP CLI/MCP contracts outside model context. It also
asserts that:

- redacted list/search/show tools remain present;
- `secret.reveal`, `totp.code`, and value-returning reference resolution are
  absent;
- disabling value tools does not silently expose an alternate raw-envelope or
  server-decrypt tool; and
- `witself run` can perform local child-only injection without returning the
  resolved value to the model.

## Open-plane and transcript non-leakage

For every live case and move case, inspect every row or model-visible payload
created for the subject or peer since `prepared_at` in these lanes:

- narrative memories, versions, evidence, curation inputs/plans, fact values
  and candidates, recall/search documents, and embedding inputs;
- direct messages, request/offer/result payloads, delivery attempts, and
  value-free message checkpoints;
- avatar profiles, source payloads, renderer metadata, render outputs, and
  avatar checkpoints;
- transcript events, normalized transcript content, hook input/output,
  hydration payloads, self digest, and runtime checkpoint context; and
- support bundles, diagnostics, metrics, and retained acceptance logs.

The verifier scans raw bytes and parsed scalar values using the full private
forbidden-marker set. It also calls the normal broad and exact read surfaces to
guard against a value hidden from the raw-table selection by a new projection.
Every lane must report zero matches. A missing lane, unavailable transcript
flush, truncated log window, uninspectable table, or unknown newly introduced
payload column is a failure, not a zero.

This claim covers Witself-owned capture and persistence. Portable transcript
events are held locally until a terminal turn fence; a sealed tool call causes
the queued turn and every later hook in it to be replaced by value-free
markers before upload. It does not attest undocumented telemetry retained
inside an AI provider, operating system, shell, child process, or destination
service. In particular, an explicit value-returning MCP call can expose its
result to the active model/provider even though Witself's portable transcript
must remain clean.

## Sanitized evidence

The retained artifact schema is
`witself.sealed-plane-acceptance.evidence.v1`. A minimal document has this
shape:

```json
{
  "schema_version": "witself.sealed-plane-acceptance.evidence.v1",
  "suite_version": "1",
  "run_id": "spa_...",
  "status": "pass",
  "certification_eligible": true,
  "prepared_at": "2026-07-18T00:00:00Z",
  "verified_at": "2026-07-18T00:20:00Z",
  "witself": {
    "version": "0.0.0",
    "commit": "abcdef1",
    "schema_version": 56,
    "archive_schema_version": 56
  },
  "runtimes": [
    {"name": "codex", "client_version": "...", "case_count": 3}
  ],
  "clouds": [
    {
      "name": "aws",
      "endpoint_fingerprint": "run-salted-opaque-value",
      "database_version": "..."
    }
  ],
  "runtime_cloud_cases": [],
  "cloud_move_cases": [],
  "leakage": {
    "inspected_surface_count": 0,
    "forbidden_match_count": 0,
    "unknown_surface_count": 0
  }
}
```

The final schema may add value-free fields but must retain closed validation
and these required semantics. Every case entry contains only its fixed case id,
start/end timestamps, release/runtime versions, status, value-free operation
counts, inspected-surface counts, and booleans for the pass conditions above.
Move entries additionally contain source/destination provider names and
value-free row-count/equality results. Private byte digests are used during
verification but are not retained in the sanitized artifact.

The artifact must not contain:

- bearer tokens, token fingerprints, AVKs, DEKs, key-file paths, key bytes, or
  key fingerprints;
- secret names, field values, usernames, URLs, passwords, API keys, recovery
  codes, TOTP seeds/codes, or any forbidden-marker representation;
- ciphertext, nonces, wrapped DEKs, envelope digests, raw idempotency keys, or
  archive bodies;
- API/DB endpoints, hosts, ports, DSNs, users, database names, cloud resource
  ids, private log locations, or local configuration paths;
- prompts, tool arguments/results, transcript bodies, messages, memories,
  avatars, model identifiers, or child-process output; or
- raw errors from providers, databases, APIs, runtimes, or cleanup.

Before writing the artifact, the verifier serializes the final bytes and
rejects every private forbidden marker and every prohibited field name. It
writes the file mode `0600`; artifact transport does not preserve that mode as
a security boundary. A failed or incomplete run may write a sanitized failure
artifact, but it can never set `status: "pass"` or
`certification_eligible: true`.

The protected release record retains the release tag, commit, workflow URL,
human approval, and a protected mapping to the three raw resource attestations.
The downloadable artifact deliberately lacks enough endpoint information to
act as durable proof of resource identity by itself.

## Exact aggregate pass criteria

The verifier may emit `status: "pass"` only when all of these are true:

1. The evidence names exactly the four runtimes fixed by this acceptance version and three attested cloud
   providers, all on the same released Witself commit and compatible schema.
2. All 12 fixed runtime/cloud case ids are present exactly once with `pass`.
3. All nine fixed move case ids are present exactly once with `pass`.
4. There are no skipped, waived, retried-as-success, expected-failure, or
   unknown cases in the final run.
5. Key bootstrap, missing-key, wrong-key, tamper, and token-rotation assertions
   have passed at their required scope with no fallback.
6. Every database/API/log/archive assertion passed, every required surface was
   inspected, `unknown_surface_count` is zero, and `forbidden_match_count` is
   zero.
7. Redaction, one-field reveal, child-only runtime injection, local TOTP, and
   value-free audit/usage counts passed in every live case.
8. Every move preserved encrypted bytes and public metadata, used the unchanged
   local AVK, and made zero post-fence source-cloud calls or Witself
   agent-vault KMS calls.
9. Every Witself-owned runtime transcript is flushed and version-attributed,
   and no sealed value or key material appears in memory, messaging, avatar,
   hydration, or portable transcript surfaces.
10. The serialized evidence artifact passes its own forbidden-marker and
    closed-schema validation.

Any assertion failure makes the overall result `fail`. Infrastructure
unavailability, incomplete logs, missing database inspection, an unflushed
transcript, or inability to fence a source makes the result `incomplete`; it is
not silently retried into a pass. Rehearsals use `pass_rehearsal` or
`fail_rehearsal` and are never certification-eligible.

## Deferred and out of scope

The schema-56 gate above covers multi-installation enrollment, client-side
passphrase recovery artifacts, and recovery-gated crash-resumable AVK rotation.
Production certification uses an external or synchronously replicated recovery
destination; a same-disk artifact is not accepted as an independent device-loss
copy. The larger
21-case certification still depends on the live harness and deferred runtime
injection. Neither gate certifies secret update/replacement, grants or
group/cross-agent sharing, dedicated TOTP enrollment/removal commands,
irreversible secret purge or crypto-shred (guarded tombstone deletion is
implemented separately through schema 67), OS keychains or secure enclaves,
encrypted attachments, browser-native filling, or additional installation
proof-of-possession. Those features require separate threat models and
acceptance suites.

Cloud KMS may protect database volumes, backups, deployment credentials, and
infrastructure state. The gate forbids only using cloud KMS as the agent-vault
decrypt root or as a requirement for moving encrypted agent vault state.
