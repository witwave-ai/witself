# Working in this repository

## Gates

Before every push, use the deterministic local check selector:

```sh
make check-local
```

It fetches `origin`, compares against `origin/main`, and includes the complete
branch plus staged, unstaged, and untracked files. Do not substitute a narrower
base or decide eligibility from a short diff. It selects one of four paths:

- **Brochure only:** the existing positive `website_only` classification installs
  locked website tooling and runs `make check-product-site`. Its exact allowlist
  remains in `scripts/ci-product-scope.mjs`.
- **Audited research documentation only:** regular, non-executable additions or
  edits directly under `docs/research/local-cell/*.md`, plus `docs/README.md`,
  run `make check-docs`. The command validates local links, anchors, source-line
  bounds, and content; an affected main docs index also runs its existing Go
  index contract without PostgreSQL. Deletions, renames, mode/type changes,
  unknown paths, and mixed changes stay on the full path. The same docs gate
  runs in PR and main CI. See [the docs gate guide](docs/docs-validation.md).
- **Verified release roll only:** `scripts/local-roll-scope.mjs` must accept an
  existing cell's generated application chart version, image tag, and image
  digest as the only changes. `scripts/check-local-roll.mjs` then verifies the
  release, artifact identities, unchanged migration sources, generated values,
  and selected-cell rendering. Only its complete PASS replaces the full local
  application gate. Partial updates, downgrades, extra configuration, database
  or backup images, secrets, and source changes do not qualify. A confirmed
  migration-source change selects the full application path automatically.
- **Everything else:** run `make check`, including `check-infra`, with required
  PostgreSQL and Node tests. Prepare the isolated database and locked Worker
  dependencies described below before running this path.

These are standing, machine-checked local rules; no per-push exception is needed
when the selected gate passes. If release evidence or artifact verification is
missing or fails, the selector stops. Passing unrelated application tests does
not repair bad release evidence; resolve it or use the normal full validation
and release procedure. See [the rollout gate guide](docs/local-roll-gate.md).

Shared dependencies, hosting, screenshot tools, workflows, check code, and this
policy always require the full path. Changes to this policy or any gate
implementation require the full path. Required PR check names, exact post-merge
checks,
backup-first rollout order, schema/backup obligations, and live convergence are
unchanged. The command does not push, merge, tag, deploy, or authorize a rollout.
It is not a Git hook and cannot prevent someone from bypassing local checks.
Never tag on red.

**Read a gate's verdict from its own output, never from a wrapper's exit
code.** A gate wrapped inline as

```
make check > gate.log 2>&1; echo "EXIT=$?" >> gate.log; tail -3 gate.log
```

exits with the status of the last command in the list — the `tail`, which
always succeeds. Anything reading that exit code, including an agent harness
reporting a completed background task, is told the gate passed when `make`
actually failed. Use `scripts/run-gate.sh`, which exits with the gate's real
status and ends its log with `run-gate: PASS <cmd>` or
`run-gate: FAIL <code> <cmd>`:

```
scripts/run-gate.sh --log /tmp/check.log check
```

To retain the selector's complete verdict, use
`scripts/run-gate.sh --log /tmp/local-check.log -- make check-local`.

## Tests that pass by not running

Three traps in this repository produce a green result that proves nothing:

- **PostgreSQL integration tests skip silently** when
  `WITSELF_TEST_DATABASE_URL` is unset. A local `make check` without a
  database has not exercised the store. Set `WITSELF_TEST_REQUIRE_DATABASE=1`
  to make an unset DSN fatal; use `WITSELF_TEST_REQUIRE_NODE=1` analogously
  for a missing Node executable. Before trusting one, start a
  database and export the DSN:

  ```
  docker run -d --name witself-test-pg -p 5599:5432 -e POSTGRES_PASSWORD=test postgres:16
  export WITSELF_TEST_DATABASE_URL="postgres://postgres:test@127.0.0.1:5599/postgres?sslmode=disable"
  ```

- **A fresh git worktree has no `node_modules`** under `infra/cloudflare/*/`,
  so `check-infra` fails on a missing wrangler until `npm ci` has run in each
  worker directory. That failure is environmental, not a code defect — but do
  not paper over it, install and re-run.

- **The roll-state matrix needs a verified PostgreSQL child chart.** The
  runner fetches and verifies it. `WITSELF_TEST_SKIP_POSTGRESQL_CHART=1`
  skips the child-chart row loudly with a `NOT RUN test-civo-postgres-chart`
  line on stderr; a run with that line in its output is not a full gate, and
  the knob does not make `check-infra` work offline (the fetch contract
  checks still need the verified chart).

Prefer timing assertions that are proportional to the fixture's own timings
over absolute wall-clock bounds, which flake on a loaded machine.

## Generated and mirrored files

- `docs/feature-status.md` is generated from `featurestatus/catalog.json`.
  Regenerate with `make feature-status`; never hand-edit it. The catalog
  validator enforces closed vocabularies, byte caps (feature and gate
  summaries 320, `evidence_scope` 240, open-gate summaries 300), sorted
  evidence lists, and the existence of every referenced path.
- `internal/supportrunner/context/support-policy.md` is a byte-for-byte copy
  of `docs/support-policy.md` and a test enforces it. Edit both together.

## Change discipline

Commit locally per logical change; push, run CI, and tag once per feature
batch. Merge only at a settle-verified green: confirm the local head matches
the PR head, wait until every check has reported and none is failing, then
squash-merge with `--match-head-commit` and verify post-merge CI against the
new commit. The six required check names remain real, successful checks in every
lane: for a positively classified brochure-only change, they require passed
website validation and confirm application jobs were intentionally skipped.
For positively classified research documentation, they require successful docs
validation and confirm application and website jobs were intentionally skipped.
For application or mixed changes, every applicable application gate must pass.
Brochure-only merges publish automatically after the separate website checks;
mixed changes retain manual website publication after full CI.
