# Narrative Memory Load And Quality Harnesses

Status: two executable PostgreSQL slices. This runbook defines the original
opt-in lexical-memory baseline and the bounded curation load/lifecycle slice for
production-readiness issue
[#46](https://github.com/witwave-ai/witself/issues/46). They provide useful,
reproducible evidence, but neither separately nor together do they close that
issue; the remaining gates are listed below.

## What The Lexical Harness Proves

`TestNarrativeMemoryLoadQualityPostgres` runs directly against one PostgreSQL
endpoint in a fresh, disposable schema. It applies every migration, creates two
synthetic accounts and three synthetic agents, and then verifies:

- deterministic lexical relevance for the checked-in labeled corpus;
- default broad-recall redaction of one synthetic sensitive memory;
- exact owner retrieval of that same synthetic sensitive memory when explicitly
  enabled;
- same-realm cross-agent isolation and cross-account isolation;
- bounded capture latency and throughput; and
- bounded concurrent lexical-recall latency and throughput.

The workload is reproducible from a signed 64-bit seed. The checked-in corpus is
`internal/loadquality/testdata/corpus.v1.json`; its SHA-256 digest is included in
every result. Deterministic low-salience distractors are derived from
`SHA-256(seed:index)`, not a Go runtime random-number sequence.

The backend performs no inference in this test. The harness calls no AI, model,
embedding service, runtime client, MCP server, secret store, or sealed-plane
operation. All values are synthetic. It creates no agent token. The complete
test schema is dropped during cleanup.

## Safety Boundary

Supply a dedicated test database or a principal allowed to create and drop its
own schemas. The harness creates schemas named
`witself_migration_<pid>_<sequence>`. An abruptly killed process can leave one
for an operator to inspect and remove.

Keep `WITSELF_TEST_DATABASE_URL` in the trusted parent environment. Do not pass
the DSN as a Make variable or command-line argument. The retained JSON result is
sanitized, but a native PostgreSQL error printed by `go test` can contain
topology or principal metadata; run the harness only in a trusted terminal or
runner.

The result never contains:

- a DSN, hostname, port, database name, or database user;
- an account, realm, agent, or memory id;
- a query, memory value, tag set, content hash, or sensitive marker;
- a token, credential, secret, or resource id; or
- a transcript or client prompt.

Results are written atomically with mode `0600`.

## Run The Lexical Slice

Start local PostgreSQL and export its test DSN:

```sh
make db-up
export WITSELF_TEST_DATABASE_URL='postgres://witself:witself@localhost:5432/witself?sslmode=disable'
make test-memory-load-quality
```

The default result is `/tmp/witself-memory-load-quality.json`. The default
workload is:

| Setting | Default | Maximum |
|---|---:|---:|
| Seed | `20260717` | signed 64-bit integer |
| Noise memories | `250` | `10000` |
| Query iterations per labeled relevance case | `25` | `10000` |
| Concurrent recall workers | `4` | `64` |

Override only bounded workload and safe evidence metadata:

```sh
make test-memory-load-quality \
  MEMORY_LOAD_QUALITY_RESULTS=/trusted-artifacts/memory-load-quality.json \
  MEMORY_LOAD_QUALITY_SEED=20260717 \
  MEMORY_LOAD_QUALITY_NOISE=1000 \
  MEMORY_LOAD_QUALITY_ITERATIONS=100 \
  MEMORY_LOAD_QUALITY_CONCURRENCY=8 \
  MEMORY_LOAD_QUALITY_RELEASE=v0.0.172 \
  MEMORY_LOAD_QUALITY_COMMIT=67ec81d3f5485f1865f87e265ae9f33fa15c6988 \
  MEMORY_LOAD_QUALITY_PROVIDER=gcp \
  MEMORY_LOAD_QUALITY_HARDWARE=cloud-sql-postgres-18-tier-name
```

For a managed database, inject `WITSELF_TEST_DATABASE_URL` from its protected
secret environment and use the same Make target. Record the actual provider and
hardware tier. Do not describe a local Docker pass as a managed-cloud baseline.

The direct Go command is available for a trusted runner:

```sh
WITSELF_MEMORY_LOAD_QUALITY=1 \
WITSELF_MEMORY_LOAD_QUALITY_RESULTS=/trusted-artifacts/memory-load-quality.json \
go test ./internal/store \
  -run '^TestNarrativeMemoryLoadQualityPostgres$' \
  -count=1 -v -timeout 30m
```

All other controls use the `WITSELF_MEMORY_LOAD_QUALITY_*` names pinned in
`internal/loadquality/loadquality.go`. The Make target is preferred because it
records the current Git description and commit by default.

## Lexical Result Contract

The retained document has schema
`witself.memory-load-quality-result.v1` and harness version `1`. It records:

- UTC start and completion times and a pass outcome;
- safe release, commit, provider, hardware, Go, OS, architecture, and CPU
  metadata;
- PostgreSQL software version, never endpoint identity;
- seed, corpus digest, bounded fixture counts, iterations, and concurrency;
- capture and recall count, wall duration, throughput, minimum, p50, p95, p99,
  and maximum latency; and
- labeled relevance ranks plus boolean sensitive-redaction and isolation
  outcomes.

All quality checks must pass before a `pass` document can be serialized. The
measurement count must also agree exactly with the declared workload. This
prevents a partial run from being retained as successful evidence.
The checked-in Draft 2020-12 JSON Schema is
`internal/loadquality/testdata/result-schema.v1.json`.

Latency uses monotonic process time. Percentiles use nearest-rank selection.
Recall throughput uses total wall time rather than the sum of concurrent call
durations. This is a baseline, not an SLO: retain raw result documents and set
thresholds only after representative managed-cloud runs.

## Curation Load And Lifecycle Slice

`TestNarrativeMemoryCurationLoadPostgres` is the second executable slice. It
runs directly against one PostgreSQL endpoint in a fresh, disposable schema,
applies the complete migration set, creates only synthetic accounts, agents,
transcripts, memories, evidence, requests, and plans, and drops the complete
schema during cleanup. A signed 64-bit seed determines every harness-generated
fixture value and workload choice; store-assigned ids and wall-clock time are
never used as random seeds.

Like the lexical slice, this harness performs no inference. It calls no AI or
model, embedding service, runtime client, MCP server, secret store, or
sealed-plane operation, and it creates no agent token. It drives the production
store curation methods without weakening or replacing their queue, lease,
fencing, plan, apply, or retry semantics.

### What The Curation Harness Proves

The bounded workload exercises and measures:

1. **Request coalescing.** Repeated requests for one owner and coalescing key
   produce one live queue item. The result records request calls, newly queued
   requests, coalesced calls, observed queue depth, and coalescing ratio.
2. **Claim contention.** Concurrent workers race to start a pool of due
   requests. The result records start attempts, wins, clean losses, and the
   winner/loser rates; exactly one worker wins each request.
3. **Frozen-input paging.** Synthetic memory and transcript backlogs are frozen
   at several configured cardinalities and paged to an empty next cursor. The
   result records aggregate runs, pages, inputs, exhausted runs, and duplicate
   input detection; the harness asserts each configured run independently.
4. **Plan lifecycle and follow-up draining.** Runs mix the exact empty plan with
   small create plans whose resolved transcript evidence lies inside the frozen
   window and carries the matching resolved kind. Every staged plan is read back
   before exact revision/hash apply. The result records empty and non-empty
   plans/applies, create actions, follow-up requests, observed chain depth, and
   complete backlog drainage; each create apply must return exactly one created
   memory receipt before the passing result can be written.
5. **Lease churn and fencing.** Expired renewals durably interrupt and reconcile
   the run under retry policy. Stale fencing generations and a second apply are
   refused. The result records expiry cycles, reconciliations, stale-fence
   refusals, apply-race attempts, and double-apply refusals.
6. **Stale-plan and conflict handling.** An apply with the wrong plan hash and a
   second plan submission against a planned run return their typed refusals. The
   result counts both conflict classes.
7. **Abandon and requeue.** `preview_complete` requeues without consuming the
   attempt budget. Separate non-preview failures drive one request through its
   bounded attempt budget to terminal dead-letter, after which it cannot be
   started again. The result records preview abandons, unchanged-attempt checks,
   retrying abandons, terminalizations, and post-terminal start refusals.

Each operation family retains `OperationStats`: count, wall duration,
throughput, minimum, p50, p95, p99, and maximum latency. Measurements cover
`request_coalescing`, `claim_start`, `input_page`, `plan`, `plan_get`, `apply`,
`lease_renew`, `lease_apply_race`, `typed_refusal`, and `abandon`. Durations use
monotonic process time, nearest-rank percentiles, and total wall time for
concurrent throughput.

Lease-expiry cases are forced only inside the disposable fixture schema by
moving the synthetic run's stored lease into the past. This keeps the run
bounded without changing the process clock, sleeping through production lease
durations, or changing store curation code. The subsequent renew/apply calls
still use the normal store paths and PostgreSQL clock as the authority.

### Run The Curation Slice

Keep the DSN in the trusted parent environment and invoke the dedicated target:

```sh
make db-up
export WITSELF_TEST_DATABASE_URL='postgres://witself:witself@localhost:5432/witself?sslmode=disable'
make test-memory-curation-load
```

The default result is `/tmp/witself-memory-curation-load-<pid>.json` (pid-
scoped so concurrent runs never overwrite each other's evidence). Defaults are
designed to finish in less than ten minutes on a local development database;
every workload control is bounded:

| Setting | Default | Allowed range |
|---|---:|---:|
| Seed | `20260831` | signed 64-bit integer |
| Same-owner coalescing request calls | `24` | `2..10000` |
| Due requests in the claim pool | `6` | `1..64` |
| Concurrent claim workers | `4` | `2..64` |
| Paging cardinalities | `4,16,64` | 2-5 strictly increasing values, each `1..500` |
| Input page size | `8` | `1..200` |
| Plan-lifecycle source backlog | `24` | `2..32000` |
| Per-run source cap | `6` | `1..500`, less than backlog |
| Forced lease-expiry cycles | `3` | `1..20` |
| Dead-letter maximum attempts | `3` | `2..20` |

The derived follow-up chain depth, `ceil(backlog / cap)`, must be between 2 and
64. Override only bounded workload and safe evidence metadata:

```sh
make test-memory-curation-load \
  MEMORY_CURATION_LOAD_RESULTS=/trusted-artifacts/memory-curation-load.json \
  MEMORY_CURATION_LOAD_SEED=20260831 \
  MEMORY_CURATION_LOAD_COALESCING_REQUESTS=12 \
  MEMORY_CURATION_LOAD_CLAIM_REQUESTS=4 \
  MEMORY_CURATION_LOAD_CLAIM_WORKERS=4 \
  MEMORY_CURATION_LOAD_PAGING_CARDINALITIES=4,12,32 \
  MEMORY_CURATION_LOAD_PAGE_SIZE=4 \
  MEMORY_CURATION_LOAD_CHAIN_BACKLOG=12 \
  MEMORY_CURATION_LOAD_CHAIN_CAP=3 \
  MEMORY_CURATION_LOAD_LEASE_CYCLES=2 \
  MEMORY_CURATION_LOAD_MAX_ATTEMPTS=3 \
  MEMORY_CURATION_LOAD_RELEASE=v0.0.172 \
  MEMORY_CURATION_LOAD_COMMIT=67ec81d3f5485f1865f87e265ae9f33fa15c6988 \
  MEMORY_CURATION_LOAD_PROVIDER=gcp \
  MEMORY_CURATION_LOAD_HARDWARE=cloud-sql-postgres-18-tier-name
```

The direct Go command is available for a trusted runner:

```sh
WITSELF_MEMORY_CURATION_LOAD=1 \
WITSELF_MEMORY_CURATION_LOAD_RESULTS=/trusted-artifacts/memory-curation-load.json \
go test ./internal/store \
  -run '^TestNarrativeMemoryCurationLoadPostgres$' \
  -count=1 -v -timeout 12m
```

All other controls use the `WITSELF_MEMORY_CURATION_LOAD_*` names defined in
`internal/loadquality`. The Make target is preferred because it records the
current Git description and commit by default.

### Curation Result Contract

The retained document has schema
`witself.memory-curation-load-result.v1` and harness version `1`. The strict,
additional-properties-closed Draft 2020-12 JSON Schema is checked in at
`internal/loadquality/testdata/curation-result-schema.v1.json`; it is separate
from, and does not modify, the lexical `result-schema.v1.json` contract.

The curation document records UTC bounds and a pass outcome, the same safe
environment and PostgreSQL software metadata as the lexical result, the seed
and bounded workload shape, operation measurements, aggregate lifecycle
counters, and value-free outcomes grouped as `request_coalescing`,
`claim_contention`, `input_paging`, `plan_lifecycle`, `lease_churn`,
`stale_plan_conflict`, and `abandon_requeue`. Every declared operation count and
counter relationship must agree with the workload, and every required
correctness assertion must pass, before a `pass` document can be serialized and
atomically written with mode `0600`.

The outcome objects retain these exact aggregate fields:

| Outcome | Fields |
|---|---|
| `request_coalescing` | `calls`, `created`, `coalesced`, `queue_depth`, `coalescing_ratio`, `all_coalesced` |
| `claim_contention` | `requests`, `attempts`, `wins`, `losses`, `win_rate`, `loss_rate`, `single_winner_per_request` |
| `input_paging` | `runs`, `pages`, `inputs`, `exhausted_runs`, `duplicate_inputs`, `paged_to_exhaustion` |
| `plan_lifecycle` | `plans`, `plan_gets`, `applies`, `empty_applies`, `create_applies`, `create_actions`, `empty_cursor_advances`, `follow_up_requests`, `max_chain_depth`, `drained_chains`, `empty_plan_advanced_cursors`, `backlog_drained` |
| `lease_churn` | `cycles`, `live_renewals`, `renew_after_expiry`, `reconciliations`, `requeues`, `apply_race_attempts`, `apply_race_wins`, `apply_race_refusals`, `stale_fence_refusals`, `double_apply_successes`, `expired_renew_reconciled`, `no_double_apply` |
| `stale_plan_conflict` | `wrong_plan_hash_refusals`, `duplicate_plan_refusals`, `stale_fence_refusals`, `typed_refusals`, `all_refusals_typed` |
| `abandon_requeue` | `preview_abandons`, `preview_requeues`, `preview_attempt_count_before`, `preview_attempt_count_after`, `failure_abandons`, `expiry_interruptions`, `retry_requeues`, `dead_letters`, `terminal_attempt_count`, `post_terminal_start_refusals`, `preview_budget_preserved`, `dead_letter_terminal` |

A passing result requires zero duplicate inputs and double-apply successes,
exact workload-derived operation counts, all expected typed refusals, preserved
preview attempt budget, complete input/chain drainage, and terminal dead-letter
behavior at the configured maximum attempt count.

The curation result never retains a DSN or endpoint identity; an account, realm,
agent, request, run, memory, transcript, evidence, receipt, mutation, token, or
other resource id; an idempotency/coalescing key, input cursor, source range,
plan hash, query, tag, sensitive marker, or content hash; or any transcript,
memory, plan, evidence, prompt, token, credential, or secret content. Queue
depth, fencing, attempt, plan, apply, and evidence behavior appear only as
aggregate counts, rates, latencies, and passing booleans.

This is bounded local store-level evidence, not an SLO or a production capacity
claim. It does not exercise transport/network contention, a deployed worker,
client semantic quality, rollback under load, production backlog age,
managed-cloud behavior, or model token/cost envelopes. This second slice
advances issue #46's queue/curation load requirement; it does not close issue
#46.

### Curation Slice Honesty Notes

- `provider` and `hardware_tier` are operator-typed labels retained verbatim.
  The harness rejects dots in them so a pasted database hostname cannot pass
  as a label; they remain the operator's statement, not a measured value.
  `release` and `commit` accept dots (semantic versions require them).
- The per-workload summary booleans (`single_winner_per_request`,
  `empty_plan_advanced_cursors`, `expired_renew_reconciled`,
  `all_refusals_typed`, and siblings) certify that the workload's inline
  store-observing assertions all held during the run - they are roll-ups of
  test-enforced invariants, not independent re-measurements recorded from the
  database afterward. The inline assertions (typed errors.Is checks, duplicate
  winner maps, cursor-advance verification against re-read run state) are the
  primary proof; the booleans exist so evidence consumers do not have to parse
  the test source to know which invariants a pass implies.

## Evidence Checklist

Retain the JSON result with:

- the immutable release tag and full commit SHA being tested;
- PostgreSQL provider/version and non-sensitive hardware tier;
- runner hardware and network placement notes outside the JSON when needed;
- the command/workflow URL and timestamp; and
- any operator-approved exception.

For lexical comparisons, use the same seed and corpus digest. For curation
comparisons, use the same seed and complete workload shape. Change one workload
dimension at a time. A dirty checkout should be labeled exploratory, not a
release baseline.

## What Still Remains For Issue #46

These two slices intentionally do **not** claim production readiness. Issue #46
still requires:

1. Broader production instrumentation. Bounded HTTP, memory operation/recall,
   vector coverage/fallback, and curation domain-call metrics are implemented.
   Durable run-transition and lease-event metrics, queue-age distributions,
   archive/rebuild timing,
   remaining operation coverage, dashboards, alerts, and measured defaults
   still remain.
2. Queue and curation load beyond this bounded local slice: rollback under
   contention, production queue/backlog-age distributions, larger and more
   concurrent shapes, managed-cloud repetitions, and reviewed safe limits/SLOs.
3. Optional client-vector and hybrid-recall scale, coverage degradation, and
   explicit lexical-only fallback under partial/missing vectors.
4. Whole-account export/import, retrieval-projection rebuild, rollback, and
   large-archive duration.
5. Larger high-cardinality shapes for versions, evidence, relations,
   transcripts, and concurrent agents. The present maximum of 10,000 noise
   memories is a bounded first fixture, not a capacity claim.
6. A richer adjudicated relevance corpus with false-positive and ranking
   metrics beyond the two exact lexical cases in v1.
7. Client-side curation quality, duplicate growth, supersession quality,
   summarization drift, and model token/cost envelopes. Those require explicit
   client inference and remain outside this model-free store harness.
8. Managed-cloud baselines on representative hardware, documented production
   SLOs/alerts/safe limits, degraded-mode drills, and measured default tuning.
9. A protected repeatable workflow that uploads this sanitized result and
   identifies the release, PostgreSQL tier, and runner without exposing
   credentials.

No production default should be changed from either local result. Production
defaults and thresholds require repeated GCP/AWS/Azure measurements and an
explicit review of the retained evidence.
