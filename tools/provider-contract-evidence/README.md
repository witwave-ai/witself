# Provider contract evidence tool

This CI utility runs the repository's existing offline provider fixtures and
retains a bounded, allowlisted report. It is not a Witself command or a vendor
runtime certification runner. Build it outside the checkout and keep its output
outside the checkout or in an ignored evidence directory.

## Commands

`run` accepts `--output`, `--target`, `--binary`, `--dist`, and these trusted
workflow identity flags:

- `--expected-commit`: full actual checkout commit (`GITHUB_SHA`, including a PR
  synthetic merge commit when that is the checkout).
- `--repository`, `--workflow` (`ci` or `release`), `--run-id`, `--run-attempt`.
- `--source-ref` and `--release-tag`. The publishing tag is empty for CI and
  manual dispatch, including a manual dispatch that selects a tag ref.

The native targets are `linux-x64`, `linux-arm64`, `macos-intel`, `macos-arm64`,
and `windows-x64`. The supplied target must match the running tool's native Go
OS and architecture. A clean checkout matching the expected commit is required
before and after execution.

The runner executes the unchanged test selections once each, serially:

1. `source-codex`: two Codex source tests.
2. `source-portable`: six other provider source tests.
3. `installed-snapshot`: Codex MCP stdio plus six provider lifecycle fixtures
   using the installer-produced executable.

Every phase uses `go test -json`, the exact existing anchored selection, and
`-count=1`. Go source overrides are disabled with `GOENV=off`, `GOWORK=off`, a
controlled `GOFLAGS=-p=2 -mod=readonly`, and no inherited `GOEXPERIMENT`. The
installed-binary environment variable is removed for both source phases and set
only for the installed phase. Each phase has a twelve-minute process deadline
(the existing Go test default timeout remains in force).

Archive metadata, checksum entries, actual archive bytes, the sole regular root
executable member, and the installed executable must agree. The version probe
uses a disposable home, configuration, Witself home and temporary directories;
it precreates the private legacy-migration completion marker. The binary's
stamped short commit must match the actual checkout, and any embedded VCS
revision must match too. A modified VCS build is refused. Hashes are checked
again after the fixture phases.

A failed phase does not suppress later phases. A missing or invalid artifact
leaves the installed phase incomplete while source phases can still run. The
first real nonzero child exit is preserved; evidence-only failures return 1.
The runner writes a sanitized failed/incomplete cell when valid identity and
output arguments permit it. No report or a failed upload is not passing evidence.

`aggregate` accepts `--input-dir`, `--output`, optional `--markdown`, and the same
identity flags. It reads five distinct `provider-contract-<target>.json` files,
either directly or one directory below the input root (the download-artifact
layout). It rejects symlinks, unexpected files, missing or duplicate targets,
mixed identities and attempts, malformed reports, and unsuccessful outcomes.

`validate-release` accepts `--input`, `--version` (unprefixed publishing version),
`--commit`, `--repository`, `--run-id`, and `--run-attempt`. It validates the entire
aggregate and requires workflow `release`, `release_tag=vVERSION`, and
`source_ref=refs/tags/vVERSION`. Manual snapshots use aggregate validation;
they cannot be validated as published releases.

## Report and claim boundaries

The versioned schemas are `witself.provider-contract.cell.v1` and
`witself.provider-contract.matrix.v1`. Reports carry actual checkout/run identity,
native target, runner timestamps, snapshot archive/binary/checksums SHA-256
digests, and named phase/test/process outcomes. Aggregation derives all counts
and matrix rows from validated cells: 75 exact top-level outcomes, 73 passes and
two native Windows Cursor not-applicable outcomes; 35 provider/target rows.

The Windows exception requires the exact current Cursor skip reason in both
applicable phases. Every other selected test must run and pass, its package must
pass, and its process must exit successfully. Missing, duplicated, conflicting,
truncated, malformed or unexpected events fail closed. Subtest failures also
fail the phase. Raw Go output and stderr are never report fields. Input streams
are bounded; rejected streams cancel and reap their child rather than blocking
on an unread pipe. JSON validation rejects duplicate, case-aliased, unknown and
missing report keys. Errors use fixed categories rather than raw command output.

Runtime kind is always `fixture`; vendor version is `unobserved`; client and
model results are `not_run`. `published_bytes_tested` is always false. Both CI
and release matrix jobs test GoReleaser **snapshots**, whose version and hashes
are distinct from the separately built final publishing archives. The optional
publishing tag does not change that claim.

The static test data is fabricated parser/verifier input, clearly marked in
`testdata/README.md`. It must never be copied into a build workflow as execution
evidence. Tests use fixture bytes and the already-built test executable; one
small disposable Go module reproduces a real external overlay substitution and
proves the controlled runner executes the unchanged original source instead.
