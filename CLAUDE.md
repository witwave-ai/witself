# Working in this repository

## Gates

Run `make check` before every push. It is CI's exact gate set, including
`check-infra` for the nested Pulumi module and the Cloudflare Workers. Never
tag on red.

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

## Tests that pass by not running

Two traps in this repository produce a green result that proves nothing:

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
new commit.
