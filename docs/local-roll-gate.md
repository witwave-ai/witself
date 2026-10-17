# Local checks for a verified release roll

Run `make check-local` before pushing. It refreshes `origin/main`, examines the
complete change, and selects the required local gate. This saves repeated local
application testing when a cell only moves to an already tested release. Full
GitHub PR and post-merge CI remain required.

The command retains the existing brochure-only path. All other changes use the
full application gate unless the deployment-specific classifier and verifier
both pass. Prepare PostgreSQL and the locked Worker dependencies for the full
path as described in [CLAUDE.md](../CLAUDE.md). The selector requires database and
Node tests on that path; an unset database cannot silently skip integration
coverage.

## What qualifies

The first version deliberately accepts only one existing catalog cell at a
time. Its generated `apps.witselfServer.chartVersion`, `imageTag`, and
`imageDigest` must advance together. Every other byte stays unchanged. The
classifier checks branch commits, the index, the working tree, and untracked
files, so reversing or hiding an unrelated edit does not earn the shorter path.
A shared integrity check also checks raw tracked-file bytes before either
shorter path, including the existing brochure path.
An unavailable or redirected repository context refuses before fetching.

First provisioning, multiple-cell changes, rollbacks, PostgreSQL
or backup image changes, secrets, configuration changes, source edits, and
changes to the checking tools or policy stay on the normal full path. This is
not an alternative to generating values with `scripts/roll-cell.sh`.

## What the focused gate proves

The verifier binds the target's local and remote release identities to a
published stable release and its completed successful release workflow. It
checks the pinned image and released chart against that identity, verifies
signing/provenance, checks generated-values consistency, and renders the
selected cell using the verified released chart.

A version-pin-only diff can still select a binary that migrates the database.
The focused gate therefore also compares migration sources and orchestration
between the previously pinned and target releases. It cannot replace the
existing schema attestation or backup-evidence requirement in `roll-cell.sh`.
Confirmed different migration sources select the full local application gate
automatically. Unknown migration sources stop verification. The normal
schema-change and backup procedure still applies to those releases.

Missing tools, unavailable release evidence, identity mismatches, failed
verification, or a changed checkout stop the gate. A full application test run
does not turn a mismatched artifact into a verified release. Resolve the failed
proof or follow the normal full validation and release procedure; do not bypass
the error with a locally authored success receipt.

To preserve the local verdict:

```sh
scripts/run-gate.sh --log /tmp/local-check.log -- make check-local
```

The exact implementation is in [the selector](../scripts/check-local.mjs),
[scope classifier](../scripts/local-roll-scope.mjs), and
[focused verifier](../scripts/check-local-roll.mjs). Their regression tests run
inside the existing roll-cell contract suite in full local, CI, and release
checks. The selector is a local command, not a mandatory Git hook. Repository
policy directs humans and agents to use it; GitHub continues enforcing its full
checks independently.

## What stays unchanged

Review the generated diff, settle every applicable PR check at its exact head,
squash-merge with the reviewed head fence, and verify exact post-merge CI.
Deploy backup before serving and verify live convergence. Existing release
publication, schema/backup evidence, and credential-handling requirements still
apply. This command performs checks only; it does not push, merge, publish, or
deploy anything.
