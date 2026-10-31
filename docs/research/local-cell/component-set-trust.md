# Component-set trust v1

Status: frozen B1 wire format and offline harness contract, 2026-10-27. This
package is not linked into shipped commands. The owner application gate remains
pending. [Implementation plan](implementation-plan.md) records the remaining gates.

## Bootstrap and delivery boundary

A reviewed installer, trusted package or trusted tap is the initial trust anchor.
A compromised script fetched and executed directly can replace its own key; no
downstream signature authenticates an already-compromised script. Existing
Sigstore release evidence remains independent evidence, not replaced by this
format. Root custody and production signing require S7-F2 approval.

The following are **not yet true**: there is no production root, no published
component manifest, no installer verification (B2), and no channel ownership or
private payload handoff (B3). Windows production packaging remains gated by S7-F1.
`ProductionRoots()` is empty. Tests and offline tools explicitly supply public
fixture roots; there is no ambient root discovery, unsigned fallback or network
operation. A verifier without explicit roots returns `no_trust_root`.

## Exact bytes and cryptography

Each document is a separate file. The signing input is every byte of that file,
including whitespace and any final newline. No canonicalization or re-encoding
occurs. SHA-256 of those exact bytes is signed using RSASSA-PKCS1-v1_5, with an RSA
modulus of exactly 3072 bits and public exponent 65537. The sole algorithm name is
`rsa3072-pkcs1v15-sha256`. A signature is exactly 384 raw bytes. Public keys are
SubjectPublicKeyInfo DER, serialized as standard padded base64. The key identifier
is lowercase hex SHA-256 of that DER, 64 characters. DER must round-trip through
SPKI encoding identically. Base64 must round-trip identically, including padding;
whitespace, URL alphabet and nonzero unused bits are refused. No algorithm agility
or negotiation exists.

The detached file `<document>.sig.json` is at most 8192 bytes and has exactly these
fields (all required): `format_version` (integer 1), `kind`, `algorithm`, and
`signatures` (array). Each signature object contains exactly `key_id` and
`signature`. Duplicate key identifiers refuse. `component_set` requires exactly
one delegated release-key signature; direct root signatures refuse. `delegation`
requires exactly one current-root signature. `root_rotation` requires exactly two
signatures, one current-root and one new-root, in either order. Any additional or
missing signature refuses.

Verification order is size bounds, envelope validation, key lookup in accepted
trust state, verification of the raw bytes, then document parsing. No unverified
metadata controls paths, extraction or trust selection. Rotation necessarily has
a two-stage authentication: select and verify the current root from trusted state
first; only then parse the authenticated new SPKI, verify its proof-of-possession
signature over the same bytes, and commit the rotation. Selecting an entirely new
key from the old state before reading a rotation would be impossible. Neither
partial verification nor a failed second signature advances state. With multiple
explicit bootstrap roots and no persisted selection, either trusted signature may
authenticate the initial parse, but the distinct old root must also authenticate
the bytes before selecting it as the chain anchor. Root-list and signature order
do not change acceptance.

## Strict JSON profile

Documents are UTF-8, one top-level object, without trailing JSON values. Invalid
UTF-8 and unpaired UTF-16 escapes refuse. Duplicate object names refuse at every
depth, including escaped equivalent spellings. Maximum container depth is eight,
counting the outer object as one. Every decoded string and object key is at most
4096 UTF-8 bytes. Integers use unsigned decimal digits, with no sign, fraction or
exponent, and must fit uint64. JSON syntax disallows NaN and Infinity. Schema
booleans must be JSON booleans. Schema arrays must be arrays, including an explicit
empty array where appropriate; null is never a substitute. All schema fields are
required except explicitly optional fields below. All known nested objects refuse
unknown fields; unknown top-level fields are ignored unless named in `critical`. Field names are
case-sensitive; case-folded aliases never overwrite understood fields.
Unknown fields still obey the universal JSON size, duplicate, string and depth
profile. `critical` is an array of distinct names of understood top-level fields;
an unknown name refuses, even if its corresponding field is absent.

All documents require `format_version: 1`, `critical`, `kind`, `issued_at` and
`expires_at`. Times use exactly `YYYY-MM-DDTHH:MM:SSZ`, valid RFC3339 UTC seconds,
with expiry strictly after issuance. Component sets are at most 262144 bytes;
delegations and rotations are at most 16384 bytes. The envelope follows the same
strict JSON profile but ignores no fields.

## Document schemas

All sequence numbers below are positive uint64 values. Release sequence is scoped
to channel; only `stable` exists in v1. An initial explicit root has sequence 1.

### Delegation

Besides common fields, `kind` is `delegation`; fields are `root_sequence`,
`delegation_sequence`, `keys` (nonempty array of `{key_id, spki}` public keys), and
`revoked_key_ids` (array of distinct key identifiers). Key identifiers in `keys`
are distinct, validated against their SPKI. A revocation may name a key no longer
in the accepted key set. Root keys cannot become release authority merely by
appearing in the delegation. The newest accepted delegation replaces the prior
release-key set and revocation list.

### Root rotation

Besides common fields, `kind` is `root_rotation`; fields are `root_sequence`,
`new_sequence`, and `new_root` (`{key_id, spki}`). `new_sequence` must equal
`root_sequence + 1`, without overflow. The new root must differ from the old root.
Persisted root history contains every root in sequence order; retired roots cannot
be delegated as release keys or selected again by rotation.
After complete authentication, persist the new root and invalidate the old
root's delegation authority. A new-root delegation is required for acquisition.

### Component set

Besides common fields, `kind` is `component_set`. Required fields:

| Field | Type and bound |
|---|---|
| `root_sequence`, `delegation_sequence`, `release_sequence` | Positive uint64 |
| `release_tag` | Exactly `vX.Y.Z`, decimal nonnegative components without leading zeros |
| `channel` | Literal `stable` |
| `source` | `{repository, commit}`; repository nonempty string; commit lowercase 40-hex |
| `provenance` | `{checksums_sha256, sigstore_bundle}`; lowercase 64-hex checksum; safe relative bundle filename/path |
| `license`, `sbom` | Safe relative filenames/paths |
| `rollback` | `{allowed_targets}` array of distinct positive release sequences strictly below this release |
| `platforms` | One to six distinct OS/architecture entries |

Each platform has `os` (`darwin`, `linux`, `windows`), `arch` (`amd64`, `arm64`),
nonempty `min_os`, `components`, and `glibc_floor`. `glibc_floor` is required on
Linux, exactly `2.NN` with two decimal digits; it is omitted or empty elsewhere.
There is exactly one `app`, `service_host`, `server` and `postgresql`, at least one
`worker`, and at most one optional `extension`. Unknown roles refuse. Multiple
workers are distinct components with unique archive names and paths. All archive
names and inventory paths are unique across a platform. These metadata assertions
do not certify an OS minimum, redistribution license or native loader compatibility.

Every component has the following required fields:

| Field | Type and bound |
|---|---|
| `role`, `version` | Known role above; nonempty version string |
| `archive` | `{name, size, sha256, format}`; safe single basename; positive uint64 size at most 2147483648 bytes; lowercase 64-hex digest; `tar.gz` or `zip` |
| `inventory` | Nonempty array, at most 4096 entries; schema below |
| `depends_on` | Array of distinct existing roles; no self-dependency or dependency cycle |
| `required_extensions` | Array of `{name, version, abi}`; **empty in v1** |
| `schema` | `{target, accepted_min, accepted_max}` uint64 values with accepted_min ≤ target ≤ accepted_max |
| `ownership` | `{package_owned_paths, private_payload_paths}` arrays; distinct safe inventory paths partitioning the component's complete inventory |
| `postgresql` | Required only for role `postgresql`; forbidden on other roles |

`postgresql` has `major` (integer 16), `version` (equal to component version,
starting with `16.`), nonempty `distribution` and `build`, `runtime_libraries`
(array of safe relative paths), and positive uint64 `data_layout_version`.
The extension schema is reserved; an empty required-extension list expresses the
current vector path's lack of an extension dependency.

Each inventory entry has exactly `path`, `size` (uint64), `sha256` (lowercase
64-hex), `mode` (`0644` or `0755`), and `executable` (boolean, true exactly for
`0755`). Paths are relative forward-slash paths, at most 1024 bytes, NFC according
to frozen Unicode 16.0.0 canonical normalization. Empty, `.` and `..` segments,
leading slash, backslash, NUL and colon (including drive prefixes and alternate
streams) refuse. No file may also be an ancestor directory of another inventory
file. Paths are unique. The inventory sum must not exceed declared archive size;
this explicit v1 constraint also rejects highly compressible otherwise-valid
archives whose uncompressed contents exceed their compressed archive length.

## State, freshness, revocation and installation binding

`TrustState` has memory and file implementations. State records the accepted root
sequence, root identity and public key, newest delegation sequence and public
keys/revocations, highest release sequence and tag per channel, authenticated
rollback policy, and committed installed document identities. Same-sequence
metadata must match the already accepted exact document; equivocation refuses.
Root sequence advances only through dual-signed rotation. Delegation sequences
never decrease. Release acquisition rejects lower sequence replay by default.
Explicit `AllowRollback` additionally requires the target in the **current
high-water release's** allowed-target list; a candidate cannot authorize its own
downgrade. Allowed rollback does not lower security high-water marks or restore
old delegation authority. Equal release sequence cannot substitute different
bytes, tag or policy.

The caller supplies a clock `{Now, Trusted}`. A trusted clock reports `fresh` from
issuance inclusive until expiry exclusive, `not_yet_valid` before issuance, and
`expired` at or after expiry. An untrusted clock reports `unknown_clock`.
A component validity interval must be contained in its delegation interval.
Acquisition requires fresh component and accepted delegation authority, and current
root/delegation sequences and revocations. Repair has the same acquisition rule.
Trust-document acceptance also requires freshness. A current revoked key refuses
acquisition even if its old signature remains cryptographically valid.

`VerifyComponentSet` authenticates and checks without recording installation.
`AcceptForAcquisition` persists release high-water state. `AcceptSet` also binds a
complete selected platform's components to this exact verified manifest; passing
a component from another manifest refuses, even if that manifest is independently
valid. Component values must come from `ParseComponentSet` on those exact document
bytes; an internal provenance stamp prevents substituting even identical metadata
parsed from a different manifest. Callers may set expected tag and OS/architecture to require an exact target.
Only `AcceptSet` commits installation eligibility. `AcceptForOfflineStart` accepts
only the exact committed document and envelope identity already in state, without
requiring current freshness or reauthorizing a revoked signer. It reports current
freshness and does not advance acquisition state. Expected tag and platform must
also match the committed selected-platform receipt. Previously committed bytes are
not retroactively invalidated by expiry or later revocation in B1. This is a
boundary, not an implemented installer or revocation-distribution feature.

Every file-state read and transaction revalidates the private directory and
refuses a directory symlink or permission change.
The file adapter fails closed on Windows, where the standard library mode APIs
cannot prove private 0700/0600 permissions; Windows ACL-backed persistence is a
remaining integration boundary. In-memory verification remains portable. The
caller supplies a private directory for `trust-state.json` (0600). Memory
state is serialized. File state uses a fenced local transaction and atomic temp
write, file fsync and rename, followed by directory sync. A pre-rename failure
leaves old state intact. If directory sync fails after rename, a complete new state
may already be visible; the caller must reload before retrying. Concurrent writers refuse rather than lose a newer
sequence. A crash can leave a lock or temp file requiring caller recovery; there
is no guessed stale-lock takeover. State is bounded at 8 MiB and must follow the strict JSON profile. State is local
authority, not a signed export;
no same-user attacker resistance or restoration from untrusted state is promised.

## Bounded extraction and activation recheck

`ExtractVerified(ctx, archivePath, component, stagingDir)` validates the declaration
and creates a fresh private 0700 staging directory. It refuses a pre-existing
staging path. Only a regular, non-symlink archive source is accepted. First pass streams at
most declared archive size plus one byte and
requires exact length and digest. The second pass uses the authenticated archive
handle, interprets only regular files, and creates directories implicitly with
0700. Entries must match the declared safe name, size, digest and mode. Duplicate,
undeclared, missing and extra entries refuse; an undeclared executable cannot be
created. Symlinks, hard links, devices, FIFOs, explicit directory entries, tar
PAX/GNU metadata/long-link records, and nonregular ZIP entries refuse. Extraction
checks cancellation and bounds every body at declared length plus one; aggregate
writes never exceed the inventory sum. Gzip decompression and archive metadata are
also bounded, so an oversized compressed stream cannot require unbounded work.
Tar accepts regular USTAR-compatible headers and at most 10 KiB of terminal zero
padding; its decompression cap is the inventory sum plus 4096 × 1024 bytes of
header/padding overhead plus 10240 bytes. ZIP preflight bounds allocation before
using the standard reader: at most 4096 records, an 8 MiB central directory,
1024-byte names, 4096-byte extra fields, entry comments and archive comments.
Multidisk and ZIP64 encodings refuse, as do inconsistent directory boundaries.
Success applies exact supported modes. Refusal removes only the staging directory
created by this call; an already-existing caller directory is never deleted.

`RecheckInventory(dir, component)` checks the full tree for missing/extra entries,
size, digest and mode after extraction. On Unix, file reads use `O_NOFOLLOW`,
reject symlinks and compare the opened file identity with before/after rooted
Lstat observations. Nonblocking open prevents a substituted FIFO from hanging.
Archive sources also use nonblocking regular-file opens on Unix. The second pass
uses a section reader bounded by authenticated archive length, even if that file
is subsequently extended. On Windows,
`os.Lstat` mode bits and the Windows reparse attribute reject reparse entries,
including non-symlink reparse tags that Go reports as regular files. Mode bits
alone cannot prove absence of reparse points. Windows does not expose Unix
executable bits through `os.Chmod`, so executable permission equivalence is not a
native Windows proof. Post-extraction content, mode, extra-file and symlink swaps
are tested. B3 **must call RecheckInventory immediately before activation** and
must protect the private payload against replacement between recheck and execution;
B1 alone does not establish a race-free execution handoff.

## Offline tooling and fixtures

Run `go run ./tools/component-manifest`. No command is a shipped release product.
`keygen --out DIR --label TEST-ONLY-<name>` writes fixture-only RSA-3072 keys;
labels without that prefix refuse. `--production` always refuses with
`production key generation is not authorized before S7-F2`. The five public
fixture keypairs are `root-a`, `root-b`, `release-1`, `release-2`, `revoked-1`, under
`internal/localcell/trust/testdata/keys/`, with TEST-ONLY names and PEM labels.
Their possession grants no production authority.

`build --dist DIR --tag vX.Y.Z --sequence N --channel stable --platform
os/arch=FILE --out component-set.json` reads unsigned metadata from
`DIR/component-metadata.json` and each platform template from FILE. Templates
supply source, authority sequences, dates, compatibility and ownership fields
which the specified flags cannot infer. Archive names resolve inside DIR;
digests and inventories are recomputed using the same bounded archive scanner as
extraction. The resulting document must pass the strict component schema.
The [deterministic TEST-ONLY dist recipe](../../../internal/localcell/trust/testdata/build-vector/README.md)
supplies five tiny USTAR/gzip archives and templates that reproduce the committed
`positive-built-dist` document byte-for-byte through this command. Its exact
document and envelope participate in corpus regeneration and the OpenSSL oracle;
the older synthetic schema vectors retain their frozen bytes.

`sign --key FILE --kind KIND --in DOC --out DOC.sig.json` signs exact input bytes;
rotation accepts two key flags. `verify --root PEM [--root PEM ...] [--state DIR]
[--delegation DOC] [--rotation DOC ...] --manifest DOC [--now RFC3339 |
--untrusted-clock]` verifies the chain using adjacent `.sig.json` envelopes and
prints only an outcome: reason, freshness, key identifier and sequences, never key
material or document bytes. The key identifier is public but operator logs may
redact it. No production flags, ambient private keys, network trust or release
pipeline changes are authorized.

The vector index `testdata/vectors/index.json` names each case, kind, exact document
and envelope files, setup chain, expected reason, positive classification and
negative-signature classification. Replay verifies every case. Positive envelopes
are regenerated deterministically from committed fixture private keys and compared
byte-for-byte. The independent OpenSSL oracle executes `openssl dgst -sha256
-verify <spki.pem> -signature <raw.sig> <doc>` for every positive signature and
every negative-signature case. Missing OpenSSL fails when
`WITSELF_TEST_REQUIRE_OPENSSL=1`; otherwise it loudly reports
`NOT_RUN openssl_unavailable`. The independent test profile checker is written
against this section's rules rather than calling package parsing helpers. Normal
tests never generate an RSA key; fixture generation is explicit offline maintenance.

## Closed outcome reasons

All outcomes and errors contain fixed codes, not source bytes, paths or key
material. `accepted` is the sole success reason. Parsing, crypto and trust reasons:

`no_trust_root`, `document_too_large`, `envelope_too_large`, `invalid_json`,
`duplicate_key`, `profile_violation`, `unknown_critical`, `unknown_field`,
`invalid_document`, `wrong_kind`, `wrong_algorithm`, `invalid_key`,
`invalid_encoding`, `signature_length`, `signature_count`, `signature_invalid`,
`unknown_key`, `revoked_key`, `wrong_signer`, `state_invalid`, `state_io`,
`sequence_invalid`, `replay`, `rollback_denied`, `wrong_tag`, `wrong_platform`,
`schema_invalid`, `postgresql_invalid`, `extensions_unsupported`, `roles_invalid`,
`path_invalid`, `inventory_invalid`, `mode_invalid`, `not_installed`, `mixed_set`,
`expired`, `not_yet_valid`, `unknown_clock`.

Extraction reasons:

`invalid_inventory`, `unsafe_path`, `unsupported_archive`,
`archive_size_mismatch`, `archive_digest_mismatch`, `archive_invalid`,
`archive_entry_type`, `archive_duplicate_entry`, `archive_undeclared_entry`,
`archive_missing_entry`, `entry_size_mismatch`, `entry_digest_mismatch`,
`entry_mode_mismatch`, `inventory_limit`, `staging_not_fresh`, `filesystem_error`,
`cancelled`.


## Bootstrap verifiers

LC-S7B2 adds an off-by-default bootstrap path to `install.sh` and `install.ps1`
and a dormant helper for an explicitly opted-in Homebrew formula. This updates
B1's historical statement above that installer verification was still pending;
the frozen format, Go acceptance and vector bytes remain unchanged. No production
root or component manifest is published, and `ProductionRoots()` is still empty.

Only `WS_COMPONENT_SET_DIR` selects installer verification. It names a local
bundle containing `component-set.json`, `delegation.json`, their adjacent
`.sig.json` envelopes, and at most 32 optional consecutive
`root-rotation-N.json` pairs. Numbering starts at 1 for the first transition;
the signed root sequences are checked separately.
`WS_TRUST_ROOT_PEM_FILE` explicitly supplies a public test root; without it the
selected path refuses with `no_trust_root`. There is no ambient root discovery or
unsigned fallback. `WS_UNTRUSTED_CLOCK=1` refuses acquisition with `unknown_clock`.
Normal acquisition uses the system UTC clock; corpus clocks are injected only
through the sourceable test interfaces.

The shell verifier is embedded in the installer and uses POSIX shell, a bounded
awk JSON parser and an already-installed OpenSSL. It never fetches a verifier.
Missing OpenSSL produces a fixed dependency message and refuses. PowerShell uses
a bounded strict JSON parser compiled locally by trusted .NET `Add-Type`, with
`RSA.ImportSubjectPublicKeyInfo` and `VerifyData`; the selected path requires
PowerShell 7 with those .NET APIs. The default Windows PowerShell 5.1 installer
path retains its existing behavior. Ruby uses standard-library JSON parsing
primitives and OpenSSL RSA verification. The Homebrew `LocalCell` template flag
adds `depends_on "openssl@3"` and the private `verify_component_set!(dir,
root_pem)` helper only when true. Every current product leaves it false, and
render tests compare all three current formulae byte-for-byte with their pre-B2
template output.

All implementations bound document and envelope reads, check the fixed algorithm,
RSA modulus and exponent, canonical SPKI/base64 and signature counts, and verify
the exact file bytes before parsing document fields. Delegation, dual-signed root
rotation, freshness, revocation, schema, target selection and in-session
sequence checks share the frozen corpus. The installers select the `app` archive
from one authenticated platform and check its exact length and SHA-256 before
the existing single-binary archive preflight, extraction or candidate execution.
Refusals are fixed codes from the closed list; metadata and key material are not
printed by the selected path.

Bootstrap scope is deliberately bounded. Its trust state lasts for one invocation;
it does not provide durable rollback protection across separate invocations or an
installed-generation receipt. Shell path validation accepts printable ASCII and NFC-safe Latin-1
paths; Ruby and .NET refuse characters unknown to their runtime Unicode tables
before checking NFC. These checks do not replace Go's frozen Unicode 16 inventory
validation. Full component extraction, inventory recheck, durable state and the
protected activation handoff remain B3 work. Homebrew's trusted tap and pinned
archive hash remain its first-stage extraction boundary: the dormant formula
helper is not a claim of verification before Homebrew extracts its formula
archive. Existing Sigstore evidence remains independent of component signatures.

The bash and PowerShell parity scripts and the rendered Ruby helper load every
case from the unchanged B1 index: 75 cases, with 16 positives and 59 negatives,
including 3 negative-signature cases. No corpus case is excluded. Exact refusal
reasons are checked except explicitly enumerated bootstrap diagnostic differences
in the suites; success/refusal classification must always match. One frozen
compatibility detail takes precedence over the earlier general numeric prose:
`positive-uppercase-aliases-ignored` contains a fractional value within an ignored
unknown top-level alias. B1's Go verifier accepts it. Bootstrap parsers likewise
preserve JSON numbers in ignored fields and enforce unsigned uint64 values in
understood integer fields. This does not broaden any understood schema field.

`TestBootstrapVerifierParity` runs the bash/OpenSSL and PowerShell suites where
the interpreters exist, plus the portable Windows installer verifier assertions
through `-BootstrapOnly` under PowerShell 7. Missing interpreters are reported as
`NOT_RUN <interpreter>_unavailable`; `WITSELF_TEST_REQUIRE_BOOTSTRAP_PARITY=1`
makes missing prerequisites fail. CI sets this requirement on existing native
Linux, macOS and Windows rows, and on the non-store Go lane. Native execution and
owner gating remain separate from local hermetic evidence. The remote-only
installer mocks reject service-manager calls and cell/server/worker/PostgreSQL
URLs, while Go checks keep client archive declarations limited to their matching
client binaries and declared client assets. Neither release configuration nor
publication is changed by B2.
